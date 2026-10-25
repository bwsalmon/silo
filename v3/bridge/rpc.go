package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/bwsalmon/silo/v3/acp"
	"github.com/bwsalmon/silo/v3/gateway"
)

// ListenSocket creates a Unix socket listener for the bridge at path (mode 0666
// so agents in any local uid can dial it with their token or SO_PEERCRED).
func ListenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		_ = os.Remove(path)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	l.SetUnlinkOnClose(true)
	_ = os.Chmod(path, 0o666)
	return l, nil
}

// SocketServer serves `silo connect` streams and JSON-RPC 2.0 requests on the
// bridge Unix socket (/run/silo/bridge.sock).
type SocketServer struct {
	B    *Bridge
	Cred func(net.Conn) (gateway.PeerCred, error)
	wg   sync.WaitGroup
}

// Serve accepts connections until l is closed.
func (s *SocketServer) Serve(ctx context.Context, l net.Listener) error {
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

func (s *SocketServer) serveConn(ctx context.Context, conn net.Conn) {
	credOf := s.Cred
	if credOf == nil {
		credOf = gateway.PeerCredOf
	}
	cred, _ := credOf(conn)

	br := bufio.NewReaderSize(conn, 64<<10)
	fc := newLineConn(conn, br)

	firstLine, err := fc.ReadFrame()
	if err != nil {
		return
	}
	var peek acp.Frame
	if err := json.Unmarshal(firstLine, &peek); err != nil {
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   &gateway.Error{Code: gateway.CodeParseError, Message: err.Error()},
		}
		raw, _ := json.Marshal(resp)
		_ = fc.WriteFrame(raw)
		return
	}

	// If the first frame is `{"silo":"connect",...}`, hand the stream to the
	// `silo connect` session attachment handler.
	if peek.Silo == acp.ControlConnect {
		_ = s.B.AttachConnect(ctx, fc, peek)
		return
	}

	// Otherwise, handle standard JSON-RPC 2.0 calls over the socket.
	if !s.handleRPCLine(ctx, cred, fc, firstLine) {
		return
	}
	for {
		line, err := fc.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      nil,
					"error":   &gateway.Error{Code: gateway.CodeInvalidRequest, Message: err.Error()},
				}
				raw, _ := json.Marshal(resp)
				_ = fc.WriteFrame(raw)
			}
			return
		}
		if !s.handleRPCLine(ctx, cred, fc, line) {
			return
		}
	}
}

func (s *SocketServer) handleRPCLine(ctx context.Context, cred gateway.PeerCred, fc FrameConn, line []byte) bool {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		raw, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   &gateway.Error{Code: gateway.CodeParseError, Message: err.Error()},
		})
		return fc.WriteFrame(raw) == nil
	}
	reply := len(req.ID) > 0
	if req.JSONRPC != "2.0" || req.Method == "" {
		raw, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error":   &gateway.Error{Code: gateway.CodeInvalidRequest, Message: `want {"jsonrpc":"2.0","method":...}`},
		})
		return fc.WriteFrame(raw) == nil
	}

	result, err := s.B.DispatchRPC(ctx, cred, req.Method, req.Params)
	if !reply {
		return true
	}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
	}
	if err != nil {
		var ge *gateway.Error
		if !errors.As(err, &ge) {
			ge = &gateway.Error{Code: gateway.CodeInternal, Message: err.Error()}
		}
		resp["error"] = ge
	} else {
		resp["result"] = result
	}
	raw, _ := json.Marshal(resp)
	return fc.WriteFrame(raw) == nil
}

// resolveCallerAuth extracts `agent_token` or `on_behalf_of` from raw params,
// falling back to matching `cred.UID` against a connected agent when an
// unprivileged local process omits `agent_token`.
func (b *Bridge) resolveCallerAuth(ctx context.Context, cred gateway.PeerCred, raw json.RawMessage) (gateway.Auth, map[string]any, error) {
	m := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &m); err != nil {
			return gateway.Auth{}, nil, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()}
		}
	}
	var auth gateway.Auth
	if tok, ok := m["agent_token"].(string); ok && tok != "" {
		auth.AgentToken = tok
		delete(m, "agent_token")
	}
	if ob, ok := m["on_behalf_of"].(map[string]any); ok {
		if ag, ok := ob["agent"].(string); ok && ag != "" {
			app, _ := ob["approved"].(bool)
			auth.OnBehalfOf = &gateway.OnBehalf{Agent: ag, Approved: app}
		}
		delete(m, "on_behalf_of")
	}
	// SO_PEERCRED fallback: if no explicit token was given and the caller uid
	// matches a connected agent's peer uid (and is not root or the bridge process),
	// relay on behalf of that agent.
	if auth.AgentToken == "" && auth.OnBehalfOf == nil && cred.UID > 0 && cred.UID != os.Getuid() {
		var list []gateway.AgentInfo
		if err := b.gwCall(ctx, "agent.list", map[string]any{"state": []string{gateway.StateConnected}}, &list); err == nil {
			for _, a := range list {
				if a.Peer != nil && a.Peer.UID == cred.UID {
					auth.OnBehalfOf = &gateway.OnBehalf{Agent: a.ID}
					break
				}
			}
		}
	}
	return auth, m, nil
}

// DispatchRPC executes one bridge JSON-RPC method.
func (b *Bridge) DispatchRPC(ctx context.Context, cred gateway.PeerCred, method string, raw json.RawMessage) (any, error) {
	auth, m, err := b.resolveCallerAuth(ctx, cred, raw)
	if err != nil {
		return nil, err
	}

	switch method {
	case "agent.register":
		return b.RegisterAgent(ctx, auth, m)
	case "agent.approve":
		agent, _ := m["agent"].(string)
		approve := true
		if v, ok := m["approve"].(bool); ok {
			approve = v
		}
		comment, _ := m["comment"].(string)
		return b.ApproveAgent(ctx, agent, approve, comment)
	case "agent.permissions":
		agent, _ := m["agent"].(string)
		perms, _ := m["permissions"].(string)
		return b.SetAgentPermissions(ctx, agent, perms)
	case "agent.list":
		var list []gateway.AgentInfo
		if err := b.gwCall(ctx, "agent.list", withAuth(auth, m), &list); err != nil {
			return nil, err
		}
		return list, nil
	case "agent.status":
		var info gateway.AgentInfo
		if err := b.gwCall(ctx, "agent.status", withAuth(auth, m), &info); err != nil {
			return nil, err
		}
		return &info, nil
	case "agent.read":
		agent, _ := m["agent"].(string)
		if agent == "" {
			agent, _ = m["id"].(string)
		}
		var since int64
		if v, ok := m["since"].(float64); ok {
			since = int64(v)
		}
		return b.ReadAgent(ctx, auth, agent, since)
	case "agent.stop":
		agent, _ := m["agent"].(string)
		if agent == "" {
			agent, _ = m["id"].(string)
		}
		mode, _ := m["mode"].(string)
		return b.StopAgent(ctx, auth, agent, mode)
	case "agent.report":
		status, _ := m["status"].(string)
		switch status {
		case "", "done", "ok":
			m["status"] = "ok"
		}
		delete(m, "exit_code")
		var info gateway.AgentInfo
		if err := b.gwCall(ctx, "agent.report", withAuth(auth, m), &info); err != nil {
			return nil, err
		}
		return &info, nil

	case "page.register":
		var p gateway.PageRegisterParams
		rawM, _ := json.Marshal(m)
		if err := json.Unmarshal(rawM, &p); err != nil {
			return nil, &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()}
		}
		return b.RegisterPage(ctx, auth, p)
	case "page.unregister":
		slug, _ := m["slug"].(string)
		return b.UnregisterPage(ctx, auth, slug)
	case "page.list":
		all, _ := m["all"].(bool)
		return b.ListPages(ctx, auth, all)

	case "secret.request":
		name, _ := m["name"].(string)
		reason, _ := m["reason"].(string)
		path, _ := m["path"].(string)
		wait := true
		if w, ok := m["wait"].(bool); ok {
			wait = w
		}
		return b.RequestSecret(ctx, auth, name, reason, path, wait)
	case "secret.provide":
		id, _ := m["id"].(string)
		val, _ := m["value"].(string)
		return b.ProvideSecret(ctx, id, val)
	case "secret.reject":
		id, _ := m["id"].(string)
		note, _ := m["note"].(string)
		return b.RejectSecret(ctx, id, note)
	case "secret.list":
		var list []gateway.SecretRequest
		if err := b.gwCall(ctx, "secret.list", withAuth(auth, m), &list); err != nil {
			return nil, err
		}
		return list, nil

	case "request.file":
		if _, hasDetail := m["detail"]; !hasDetail {
			if d, ok := m["details"].(string); ok && d != "" {
				m["detail"] = d
			}
		}
		delete(m, "details")
		delete(m, "commands")
		var req gateway.AgentRequest
		if err := b.gwCall(ctx, "request.file", withAuth(auth, m), &req); err != nil {
			return nil, err
		}
		return &req, nil
	case "request.list":
		var list []gateway.AgentRequest
		if err := b.gwCall(ctx, "request.list", withAuth(auth, m), &list); err != nil {
			return nil, err
		}
		return list, nil
	case "request.resolve":
		var req gateway.AgentRequest
		if err := b.gwCall(ctx, "request.resolve", withAuth(auth, m), &req); err != nil {
			return nil, err
		}
		return &req, nil

	case "session.list":
		agent, _ := m["agent"].(string)
		return b.ListSessions(agent)
	case "session.transcript":
		id, _ := m["session"].(string)
		if id == "" {
			id, _ = m["agent"].(string)
		}
		if id == "" {
			id, _ = m["id"].(string)
		}
		var after int64
		if v, ok := m["after"].(float64); ok {
			after = int64(v)
		}
		return b.ReadTranscript(id, after)
	case "session.prompt":
		id, _ := m["session"].(string)
		if id == "" {
			id, _ = m["agent"].(string)
		}
		if id == "" {
			id, _ = m["id"].(string)
		}
		rawM, _ := json.Marshal(m)
		text := acp.PromptText(rawM)
		return b.Prompt(ctx, id, text)
	case "session.cancel":
		id, _ := m["session"].(string)
		if id == "" {
			id, _ = m["agent"].(string)
		}
		if id == "" {
			id, _ = m["id"].(string)
		}
		if err := b.Cancel(ctx, id); err != nil {
			return nil, err
		}
		return map[string]any{"cancelled": id}, nil

	case "permission.list":
		agent, _ := m["agent"].(string)
		pending, _ := m["pending"].(bool)
		return b.ListPermissions(agent, pending), nil
	case "permission.decide":
		id, _ := m["id"].(string)
		decision, _ := m["decision"].(string)
		approve, hasApprove := m["approved"].(bool)
		if !hasApprove {
			switch decision {
			case "approve", "approved", "allow":
				approve = true
			case "reject", "rejected", "deny", "denied":
				approve = false
			default:
				return nil, &gateway.Error{Code: gateway.CodeInvalidParams, Message: "specify decision (approve|reject) or approved (bool)"}
			}
		}
		optID, _ := m["option_id"].(string)
		comment, _ := m["comment"].(string)
		return b.DecidePermission(ctx, id, approve, optID, comment)

	case "audit.read", "audit.verify":
		var out json.RawMessage
		if err := b.gwCall(ctx, method, withAuth(auth, m), &out); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, &gateway.Error{Code: gateway.CodeMethodNotFound, Message: fmt.Sprintf("no method %q", method)}
	}
}

// Client is a concurrency-safe JSON-RPC 2.0 client over the bridge Unix socket.
type Client struct {
	SocketPath string
}

// NewClient constructs a JSON-RPC 2.0 client for the bridge Unix socket at path.
func NewClient(path string) *Client {
	return &Client{SocketPath: path}
}

// Call dials the bridge socket, executes one JSON-RPC 2.0 call, and closes the connection.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	gc, err := gateway.Dial(ctx, c.SocketPath)
	if err != nil {
		return err
	}
	defer gc.Close()
	return gc.Call(method, params, result)
}
