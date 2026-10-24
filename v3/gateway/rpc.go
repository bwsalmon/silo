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

// relayed is implemented by every params type through the embedded Relay.
type relayed interface{ relay() *OnBehalf }

func (r Relay) relay() *OnBehalf { return r.OnBehalfOf }

// AuditReadParams pages through the audit log.
type AuditReadParams struct {
	Relay
	After int64 `json:"after,omitempty"`
	Limit int   `json:"limit,omitempty"`
}

func (s *Server) dispatch(ctx context.Context, cred PeerCred, method string, raw json.RawMessage) (any, error) {
	g := s.G
	var params relayed
	switch method {
	case "spawn":
		params = &SpawnParams{}
	case "stop":
		params = &StopParams{}
	case "list":
		params = &ListParams{}
	case "status":
		params = &StatusParams{}
	case "audit.read":
		params = &AuditReadParams{}
	case "audit.verify":
		params = &Relay{}
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("no method %q", method)}
	}
	if err := decodeParams(raw, params); err != nil {
		return nil, err
	}
	g.mu.Lock()
	c, err := g.identify(cred, params.relay())
	if err != nil {
		g.audit(fmt.Sprintf("uid=%d", cred.UID), EvAuthDeny, map[string]any{"method": method, "uid": cred.UID, "pid": cred.PID, "reason": err.Error()})
		g.mu.Unlock()
		return nil, &Error{Code: CodeForbidden, Message: "permission denied: " + err.Error()}
	}
	if (method == "audit.read" || method == "audit.verify") && !c.Human() {
		err := g.deny(c, method, "only the human may read the audit log")
		g.mu.Unlock()
		return nil, err
	}
	g.mu.Unlock()

	switch p := params.(type) {
	case *SpawnParams:
		return g.Spawn(ctx, c, *p)
	case *StopParams:
		return g.Stop(ctx, c, *p)
	case *ListParams:
		return g.List(ctx, c, *p)
	case *StatusParams:
		return g.Status(ctx, c, *p)
	case *AuditReadParams:
		limit := p.Limit
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
		return g.d.Audit.Read(p.After, limit)
	default: // audit.verify
		return g.d.Audit.Verify()
	}
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
