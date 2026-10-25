package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"

	"github.com/bwsalmon/silo/v3/gateway/approval"
)

// The socket speaks JSON-RPC 2.0 (design §4.1), one JSON object per line in
// each direction, the same framing ACP uses over stdio. See docs/api.md.

// JSON-RPC error codes. The -320xx ones are silo's.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeForbidden      = -32001
	CodeNotFound       = -32002
	CodeConflict       = -32003
)

// Error is a JSON-RPC error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// maxRequest bounds one request line.
const maxRequest = 1 << 20

// Server serves the gateway's API on a Unix socket listener.
type Server struct {
	G *Gateway
	// Cred returns the peer of a connection; PeerCredOf by default.
	Cred func(net.Conn) (PeerCred, error)
	// Approvals answers run_as_root.* and approval.* (design §4.3); nil
	// leaves those methods out.
	Approvals *approval.Service

	wg sync.WaitGroup
}

// Serve accepts connections until l is closed.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	defer s.wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.serveConn(ctx, conn)
		}()
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	credOf := s.Cred
	if credOf == nil {
		credOf = PeerCredOf
	}
	cred, err := credOf(conn)
	if err != nil {
		s.G.d.Logf("peer credentials: %v", err)
		return
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	enc := json.NewEncoder(conn)
	for {
		line, err := readLine(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &Error{Code: CodeInvalidRequest, Message: err.Error()}})
			}
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		resp, reply := s.handle(ctx, cred, line)
		if !reply {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxRequest {
			return nil, fmt.Errorf("request longer than %d bytes", maxRequest)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// handle runs one request. reply is false for notifications.
func (s *Server) handle(ctx context.Context, cred PeerCred, line []byte) (resp rpcResponse, reply bool) {
	resp = rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null")}
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = &Error{Code: CodeParseError, Message: err.Error()}
		return resp, true
	}
	if len(req.ID) > 0 {
		resp.ID = req.ID
	}
	reply = len(req.ID) > 0
	if req.JSONRPC != "2.0" || req.Method == "" {
		resp.Error = &Error{Code: CodeInvalidRequest, Message: `want {"jsonrpc":"2.0","method":...}`}
		return resp, true
	}
	result, err := s.dispatch(ctx, cred, req.Method, req.Params)
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Code: CodeInternal, Message: err.Error()}
		}
		resp.Error = e
		return resp, reply
	}
	resp.Result = result
	return resp, reply
}

// decodeParams strictly decodes params into v; absent params decode as {}.
func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &Error{Code: CodeInvalidParams, Message: "params: " + err.Error()}
	}
	return nil
}

// authed is implemented by every params type through the embedded Auth.
type authed interface{ auth() Auth }

func (a Auth) auth() Auth { return a }

// AuditReadParams pages through the audit log.
type AuditReadParams struct {
	Auth
	After int64 `json:"after,omitempty"`
	Limit int   `json:"limit,omitempty"`
}

// DisconnectParams is agent.disconnect.
type DisconnectParams struct {
	Auth
	Reason   string `json:"reason,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// humanOnly methods may not be called by agents.
var humanOnly = map[string]bool{
	"agent.approve": true, "agent.permissions": true,
	"audit.read": true, "audit.verify": true,
	"secret.provide": true, "secret.reject": true,
	"request.resolve": true,
}

// agentOnly methods act on the calling agent's own session.
var agentOnly = map[string]bool{
	"agent.heartbeat": true, "agent.disconnect": true, "agent.report": true,
	"secret.request": true, "request.file": true,
}

func (s *Server) dispatch(ctx context.Context, cred PeerCred, method string, raw json.RawMessage) (any, error) {
	if s.Approvals != nil && approval.Methods[method] {
		return s.dispatchApproval(ctx, cred, method, raw)
	}
	g := s.G
	var params authed
	switch method {
	case "agent.register":
		params = &RegisterParams{}
	case "agent.approve":
		params = &ApproveParams{}
	case "agent.permissions":
		params = &PermissionsParams{}
	case "agent.connect":
		params = &ConnectParams{}
	case "agent.heartbeat":
		params = &Auth{}
	case "agent.disconnect":
		params = &DisconnectParams{}
	case "agent.stop":
		params = &StopParams{}
	case "agent.list":
		params = &ListParams{}
	case "agent.status":
		params = &StatusParams{}
	case "agent.report":
		params = &ReportParams{}
	case "page.register":
		params = &PageRegisterParams{}
	case "page.unregister":
		params = &PageUnregisterParams{}
	case "page.list":
		params = &PageListParams{}
	case "secret.request":
		params = &SecretRequestParams{}
	case "secret.provide", "secret.reject":
		params = &SecretResolveParams{}
	case "secret.list":
		params = &SecretListParams{}
	case "request.file":
		params = &RequestFileParams{}
	case "request.list":
		params = &RequestListParams{}
	case "request.resolve":
		params = &RequestResolveParams{}
	case "audit.read":
		params = &AuditReadParams{}
	case "audit.verify":
		params = &Auth{}
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("no method %q", method)}
	}
	if err := decodeParams(raw, params); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, err := g.identify(cred, params.auth())
	if err != nil {
		g.audit(fmt.Sprintf("uid=%d", cred.UID), EvAuthDeny, map[string]any{"method": method, "uid": cred.UID, "pid": cred.PID, "reason": err.Error()})
		return nil, &Error{Code: CodeForbidden, Message: "permission denied: " + err.Error()}
	}
	// agent.connect is authenticated by the registration token in its
	// params, whoever dials; everything else needs a known caller.
	if method == "agent.connect" {
		if c.Role == RoleAgent {
			return nil, g.deny(c, method, "an agent session cannot connect another agent")
		}
		return g.Connect(c, cred, *params.(*ConnectParams))
	}
	switch {
	case c.Role == RoleConnect:
		return nil, g.deny(c, method, fmt.Sprintf("uid %d is not an admin or the bridge, and gave no agent token", cred.UID))
	case humanOnly[method] && !c.Human():
		return nil, g.deny(c, method, "only the human may call "+method)
	case agentOnly[method] && c.Role != RoleAgent:
		return nil, invalidParams("%s is called by an agent's wrapper, with its agent_token", method)
	}
	// A stopped agent's session token still answers agent.heartbeat (that
	// is how its wrapper learns to stop) and agent.disconnect; nothing else.
	if c.Role == RoleAgent && method != "agent.heartbeat" && method != "agent.disconnect" {
		if a := g.st.Agents[c.Agent]; a.State != StateConnected {
			return nil, g.deny(c, method, fmt.Sprintf("agent %q is %s", c.Agent, a.State))
		}
	}

	switch p := params.(type) {
	case *RegisterParams:
		return g.Register(c, *p)
	case *ApproveParams:
		return g.Approve(c, *p)
	case *PermissionsParams:
		return g.SetPermissions(c, *p)
	case *DisconnectParams:
		return g.Disconnect(c, *p)
	case *StopParams:
		return g.Stop(c, *p)
	case *ListParams:
		return g.List(c, *p)
	case *StatusParams:
		return g.Status(c, *p)
	case *ReportParams:
		return g.Report(c, *p)
	case *PageRegisterParams:
		return g.PageRegister(c, *p)
	case *PageUnregisterParams:
		return g.PageUnregister(c, *p)
	case *PageListParams:
		return g.PageList(c, *p)
	case *SecretRequestParams:
		return g.SecretRequest(c, *p)
	case *SecretResolveParams:
		if method == "secret.provide" {
			return g.SecretProvide(c, *p)
		}
		return g.SecretReject(c, *p)
	case *SecretListParams:
		return g.SecretList(c, *p)
	case *RequestFileParams:
		return g.RequestFile(c, *p)
	case *RequestListParams:
		return g.RequestList(c, *p)
	case *RequestResolveParams:
		return g.RequestResolve(c, *p)
	case *AuditReadParams:
		limit := p.Limit
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
		return g.d.Audit.Read(p.After, limit)
	}
	if method == "agent.heartbeat" {
		return g.Heartbeat(c)
	}
	return g.d.Audit.Verify() // audit.verify
}

// PeerCredOf reads SO_PEERCRED from a Unix socket connection.
func PeerCredOf(conn net.Conn) (PeerCred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return PeerCred{}, fmt.Errorf("not a unix socket connection: %T", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return PeerCred{}, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return PeerCred{}, err
	}
	if serr != nil {
		return PeerCred{}, serr
	}
	return PeerCred{UID: int(cred.Uid), GID: int(cred.Gid), PID: int(cred.Pid)}, nil
}
