// Package bridge implements Silo's session bridge (design §5): the ACP client
// that owns agent sessions, multiplexes transcripts and live updates, holds
// pending permission requests under the default `permissions: "ask"` policy,
// routes secret values directly to `silo connect` without logging them,
// reverse-proxies `/p/<slug>/` page links, and serves the outward HTTP/WS and
// Unix-socket APIs.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/silo/v3/acp"
	"github.com/bwsalmon/silo/v3/gateway"
)

// Default paths and addresses (design Appendix B).
const (
	DefaultBridgeSocket = "/run/silo/bridge.sock"
	DefaultSessionsDir  = "/var/lib/silo/sessions"
	DefaultHTTPAddr     = "127.0.0.1:7070"
)

// DefaultSocketPath returns SILO_BRIDGE_SOCKET if set, else DefaultBridgeSocket.
func DefaultSocketPath() string {
	if s := os.Getenv("SILO_BRIDGE_SOCKET"); s != "" {
		return s
	}
	if u := os.Getenv("SILO_HUB_URL"); strings.HasPrefix(u, "unix://") {
		return strings.TrimPrefix(u, "unix://")
	}
	return DefaultBridgeSocket
}

// Config configures the session bridge.
type Config struct {
	GatewaySocket     string
	BridgeSocket      string
	HTTPAddr          string
	SessionsDir       string
	HeartbeatInterval time.Duration
	Auth              Authenticator
	Logf              func(format string, args ...any)
}

func (c *Config) setDefaults() {
	if c.GatewaySocket == "" {
		c.GatewaySocket = gateway.DefaultSocketPath
	}
	if c.BridgeSocket == "" {
		c.BridgeSocket = DefaultBridgeSocket
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = DefaultHTTPAddr
	}
	if c.SessionsDir == "" {
		c.SessionsDir = DefaultSessionsDir
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 5 * time.Second
	}
	if c.Logf == nil {
		c.Logf = log.Printf
	}
}

// Session states reported by the bridge.
const (
	SessionConnecting   = "connecting"
	SessionReady        = "ready"
	SessionBusy         = "busy"
	SessionDisconnected = "disconnected"
	SessionStopped      = "stopped"
)

// SessionInfo is the public metadata of a bridge-owned ACP session.
type SessionInfo struct {
	ID             string    `json:"id"`
	Agent          string    `json:"agent"`
	ACPSessionID   string    `json:"acp_session_id"`
	State          string    `json:"state"`
	TranscriptPath string    `json:"transcript_path"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
	ExitCode       *int      `json:"exit_code,omitempty"`
}

// TranscriptEvent is one JSONL line persisted in /var/lib/silo/sessions/<agent>/.
type TranscriptEvent struct {
	Seq       int64           `json:"seq"`
	Time      time.Time       `json:"time"`
	Agent     string          `json:"agent"`
	SessionID string          `json:"session_id"`
	Direction string          `json:"direction"` // "in" (from agent), "out" (to agent), "bridge"
	Method    string          `json:"method,omitempty"`
	Message   json.RawMessage `json:"message"`
}

// PermissionRequest is a pending or resolved human approval item in the bridge
// (ACP `session/request_permission` or child `register_agent` under
// `permissions: "ask"`).
type PermissionRequest struct {
	ID          string          `json:"id"`
	Agent       string          `json:"agent"`
	SessionID   string          `json:"session_id,omitempty"`
	Kind        string          `json:"kind"` // "acp" or "register_agent"
	TargetAgent string          `json:"target_agent,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	State       string          `json:"state"` // "pending", "approved", "denied", "cancelled"
	OptionID    string          `json:"option_id,omitempty"`
	Comment     string          `json:"comment,omitempty"`
	Created     time.Time       `json:"created"`
	Resolved    *time.Time      `json:"resolved,omitempty"`

	decideCh chan permDecision
}

type permDecision struct {
	approve  bool
	optionID string
	comment  string
}

// Permission states.
const (
	PermStatePending   = "pending"
	PermStateApproved  = "approved"
	PermStateDenied    = "denied"
	PermStateCancelled = "cancelled"
)

// SecretStoredResult is the value-free confirmation returned to an agent when
// its secret request has been written to disk by `silo connect`.
type SecretStoredResult struct {
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Note   string `json:"note,omitempty"`
}

// Session is an active or historical ACP session owned by the bridge.
type Session struct {
	mu   sync.Mutex
	info SessionInfo

	regToken     string
	sessionToken string
	permissions  string
	conn         FrameConn
	promptMu     sync.Mutex

	rpcSeq     atomic.Int64
	pendingRPC map[string]chan acp.Frame
	secretWait map[string]chan acp.Frame

	txSeq    int64
	txFile   *os.File
	txSecond *os.File // optional <acp_session_id>.jsonl when distinct from transcript.jsonl

	subSeq  int64
	subs    map[int64]chan json.RawMessage
	readyCh chan struct{}
	doneCh  chan struct{}
}

// Info returns a snapshot of the session's metadata.
func (s *Session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Bridge is the session bridge server.
type Bridge struct {
	cfg         Config
	defaultCSRF string

	mu          sync.Mutex
	sessions    map[string]*Session // keyed by agent id
	bySessionID map[string]*Session // also indexed by ACP session id
	readyWait   map[string][]chan *Session

	nextPerm int64
	perms    []*PermissionRequest
	permCond *sync.Cond

	secretWaiters map[string][]chan SecretStoredResult

	exposeMu        sync.Mutex
	exposeListeners map[int]net.Listener
}

// New creates a new Bridge with the given configuration.
func New(cfg Config) (*Bridge, error) {
	cfg.setDefaults()
	if err := os.MkdirAll(cfg.SessionsDir, 0o700); err != nil {
		return nil, err
	}
	b := &Bridge{
		cfg:             cfg,
		defaultCSRF:     randomPrefixed(csrfTokenPrefix, 32),
		sessions:        map[string]*Session{},
		bySessionID:     map[string]*Session{},
		readyWait:       map[string][]chan *Session{},
		secretWaiters:   map[string][]chan SecretStoredResult{},
		exposeListeners: map[int]net.Listener{},
	}
	b.permCond = sync.NewCond(&b.mu)
	return b, nil
}

// Close closes any active sessions and exposed port listeners.
func (b *Bridge) Close() error {
	b.mu.Lock()
	var conns []FrameConn
	for _, s := range b.sessions {
		s.mu.Lock()
		if s.conn != nil {
			conns = append(conns, s.conn)
		}
		s.mu.Unlock()
	}
	b.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	b.exposeMu.Lock()
	for port, l := range b.exposeListeners {
		_ = l.Close()
		delete(b.exposeListeners, port)
	}
	b.exposeMu.Unlock()
	return nil
}

// gwCall calls a method on the gateway socket.
func (b *Bridge) gwCall(ctx context.Context, method string, params, result any) error {
	if pm, ok := params.(map[string]any); ok {
		if tok, ok := pm["agent_token"].(string); ok && tok != "" {
			b.mu.Lock()
			for _, s := range b.sessions {
				if s.regToken == tok && s.sessionToken != "" {
					pm["agent_token"] = s.sessionToken
					break
				}
			}
			b.mu.Unlock()
		}
	}
	c, err := gateway.Dial(ctx, b.cfg.GatewaySocket)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Call(method, params, result)
}

// withAuth builds a gateway params map carrying either an agent session token
// or an `on_behalf_of` relay.
func withAuth(auth gateway.Auth, fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		out[k] = v
	}
	if auth.AgentToken != "" {
		out["agent_token"] = auth.AgentToken
	} else if auth.OnBehalfOf != nil {
		ob := map[string]any{"agent": auth.OnBehalfOf.Agent}
		if auth.OnBehalfOf.Approved {
			ob["approved"] = true
		}
		out["on_behalf_of"] = ob
	}
	return out
}

// ---- `silo connect` attachment & ACP session lifecycle ----------------------

// AttachConnect handles a `silo connect` connection after its initial
// `{"silo":"connect",...}` frame has been read. It blocks until the agent's
// wrapper disconnects.
func (b *Bridge) AttachConnect(ctx context.Context, fc FrameConn, hello acp.Frame) error {
	defer fc.Close()
	if hello.Token == "" {
		_ = sendFrame(fc, acp.Frame{Silo: acp.ControlExit, Err: "missing registration token"})
		return errors.New("silo connect: missing registration token")
	}
	var cr gateway.ConnectResult
	if err := b.gwCall(ctx, "agent.connect", map[string]any{
		"token": hello.Token,
		"host":  hello.Host,
		"pid":   hello.PID,
	}, &cr); err != nil {
		_ = sendFrame(fc, acp.Frame{Silo: acp.ControlExit, Err: err.Error()})
		return err
	}

	agentDir := filepath.Join(b.cfg.SessionsDir, cr.Agent)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return err
	}
	txPath := filepath.Join(agentDir, "transcript.jsonl")
	txFile, err := os.OpenFile(txPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	perm := cr.Permissions
	if perm == "" {
		perm = gateway.PermAsk
	}
	sess := &Session{
		info: SessionInfo{
			ID:             cr.Agent,
			Agent:          cr.Agent,
			ACPSessionID:   cr.Agent,
			State:          SessionConnecting,
			TranscriptPath: txPath,
			Created:        now,
			Updated:        now,
		},
		regToken:     hello.Token,
		sessionToken: cr.SessionToken,
		permissions:  perm,
		conn:         fc,
		pendingRPC:   map[string]chan acp.Frame{},
		secretWait:   map[string]chan acp.Frame{},
		txFile:       txFile,
		subs:         map[int64]chan json.RawMessage{},
		readyCh:      make(chan struct{}),
		doneCh:       make(chan struct{}),
	}

	b.mu.Lock()
	if prev := b.sessions[cr.Agent]; prev != nil {
		prev.mu.Lock()
		if prev.conn != nil {
			_ = prev.conn.Close()
		}
		prev.mu.Unlock()
	}
	b.sessions[cr.Agent] = sess
	b.bySessionID[cr.Agent] = sess
	b.mu.Unlock()

	if err := sendFrame(fc, acp.Frame{
		Silo:         acp.ControlConnected,
		Agent:        cr.Agent,
		SessionToken: cr.SessionToken,
		Runtime:      cr.Runtime,
		Brief:        cr.Brief,
		Permissions:  perm,
		HeartbeatSec: cr.HeartbeatSec,
	}); err != nil {
		sess.cleanup(b, "failed to send connected ack", nil)
		return err
	}

	readErrCh := make(chan error, 1)
	go func() {
		readErrCh <- b.readAgentLoop(ctx, sess)
	}()

	// Run the ACP handshake: initialize -> session/new.
	if err := b.handshakeACP(ctx, sess); err != nil {
		_ = fc.Close()
		<-readErrCh
		sess.cleanup(b, "acp handshake failed: "+err.Error(), nil)
		return err
	}

	sess.mu.Lock()
	sess.info.State = SessionReady
	sess.info.Updated = time.Now().UTC()
	close(sess.readyCh)
	sess.mu.Unlock()

	b.mu.Lock()
	for _, ch := range b.readyWait[cr.Agent] {
		ch <- sess
		close(ch)
	}
	delete(b.readyWait, cr.Agent)
	b.mu.Unlock()

	// If the agent was registered with an initial brief, send it as the first
	// ACP prompt turn.
	if cr.Brief != "" {
		go func() {
			_, _ = b.Prompt(ctx, cr.Agent, cr.Brief)
		}()
	}

	hbTicker := time.NewTicker(b.cfg.HeartbeatInterval)
	defer hbTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = sendFrame(fc, acp.Frame{Silo: acp.ControlStop, Mode: "graceful"})
			err := <-readErrCh
			sess.cleanup(b, "bridge shutting down", nil)
			return err
		case err := <-readErrCh:
			sess.mu.Lock()
			exitCode := sess.info.ExitCode
			sess.mu.Unlock()
			reason := "disconnected"
			if exitCode != nil {
				reason = fmt.Sprintf("cli exited %d", *exitCode)
			} else if err != nil && !errors.Is(err, io.EOF) {
				reason = err.Error()
			}
			sess.cleanup(b, reason, exitCode)
			return nil
		case <-hbTicker.C:
			var hb gateway.HeartbeatResult
			err := b.gwCall(ctx, "agent.heartbeat", map[string]any{"agent_token": cr.SessionToken}, &hb)
			if err == nil && hb.State == gateway.StateStopped {
				mode := hb.Stop
				if mode == "" {
					mode = "graceful"
				}
				_ = sendFrame(fc, acp.Frame{Silo: acp.ControlStop, Mode: mode})
			}
		}
	}
}

func (s *Session) cleanup(b *Bridge, reason string, exitCode *int) {
	s.mu.Lock()
	select {
	case <-s.doneCh:
		s.mu.Unlock()
		return
	default:
		close(s.doneCh)
	}
	if s.info.State != SessionStopped {
		s.info.State = SessionDisconnected
	}
	s.info.Updated = time.Now().UTC()
	for id, ch := range s.pendingRPC {
		close(ch)
		delete(s.pendingRPC, id)
	}
	for id, ch := range s.secretWait {
		close(ch)
		delete(s.secretWait, id)
	}
	for id, sub := range s.subs {
		close(sub)
		delete(s.subs, id)
	}
	if s.txFile != nil {
		_ = s.txFile.Close()
		s.txFile = nil
	}
	if s.txSecond != nil {
		_ = s.txSecond.Close()
		s.txSecond = nil
	}
	tok := s.sessionToken
	agent := s.info.Agent
	s.mu.Unlock()

	// Cancel any pending permission requests for this agent.
	b.mu.Lock()
	for _, pr := range b.perms {
		if pr.Agent == agent && pr.State == PermStatePending {
			now := time.Now().UTC()
			pr.State = PermStateCancelled
			pr.Resolved = &now
			select {
			case pr.decideCh <- permDecision{approve: false, comment: "session disconnected"}:
			default:
			}
		}
	}
	b.permCond.Broadcast()
	b.mu.Unlock()

	if tok != "" {
		disc := map[string]any{"agent_token": tok, "reason": reason}
		if exitCode != nil {
			disc["exit_code"] = *exitCode
		}
		_ = b.gwCall(context.Background(), "agent.disconnect", disc, nil)
	}
}

func (b *Bridge) handshakeACP(ctx context.Context, s *Session) error {
	var initRes acp.InitializeResult
	if err := b.callAgentRPC(ctx, s, acp.MethodInitialize, acp.InitializeParams{
		ProtocolVersion:    acp.ProtocolVersion,
		ClientCapabilities: map[string]any{},
	}, &initRes); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd = "/"
	}
	selfExe, _ := os.Executable()
	var mcpServers []acp.MCPServer
	if selfExe != "" {
		mcpServers = []acp.MCPServer{{
			Name:    "silo",
			Command: selfExe,
			Args:    []string{"mcp", "--socket", b.cfg.BridgeSocket},
			Env: []acp.EnvVar{
				{Name: "SILO_AGENT_TOKEN", Value: s.sessionToken},
				{Name: "SILO_BRIDGE_SOCKET", Value: b.cfg.BridgeSocket},
			},
		}}
	}
	var newRes acp.SessionNewResult
	if err := b.callAgentRPC(ctx, s, acp.MethodSessionNew, acp.SessionNewParams{
		Cwd:        cwd,
		MCPServers: mcpServers,
	}, &newRes); err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	acpID := newRes.SessionID
	if acpID == "" {
		acpID = s.info.Agent
	}
	s.mu.Lock()
	s.info.ACPSessionID = acpID
	if acpID != "transcript" {
		secPath := filepath.Join(b.cfg.SessionsDir, s.info.Agent, acpID+".jsonl")
		if f, err := os.OpenFile(secPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			s.txSecond = f
			s.info.TranscriptPath = secPath
		}
	}
	s.mu.Unlock()

	b.mu.Lock()
	b.bySessionID[acpID] = s
	b.mu.Unlock()
	return nil
}

func (b *Bridge) callAgentRPC(ctx context.Context, s *Session, method string, params, result any) error {
	idNum := s.rpcSeq.Add(1)
	idStr := strconv.FormatInt(idNum, 10)
	idRaw := json.RawMessage(idStr)

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	frame := acp.Frame{
		JSONRPC: "2.0",
		ID:      idRaw,
		Method:  method,
		Params:  rawParams,
	}
	ch := make(chan acp.Frame, 1)
	s.mu.Lock()
	select {
	case <-s.doneCh:
		s.mu.Unlock()
		return errors.New("agent session disconnected")
	default:
	}
	s.pendingRPC[idStr] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pendingRPC, idStr)
		s.mu.Unlock()
	}()

	rawFrame, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	s.recordTranscript("out", method, rawFrame)
	if err := s.conn.WriteFrame(rawFrame); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.doneCh:
		return errors.New("agent session disconnected")
	case resp, ok := <-ch:
		if !ok {
			return errors.New("agent session closed while waiting for " + method)
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

func normalizeID(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	return s
}

func (b *Bridge) readAgentLoop(ctx context.Context, s *Session) error {
	for {
		line, err := s.conn.ReadFrame()
		if err != nil {
			return err
		}
		var f acp.Frame
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		// Control frame from `silo connect`.
		if f.Silo != "" {
			switch f.Silo {
			case acp.ControlSecretAck:
				s.mu.Lock()
				ch := s.secretWait[f.ReqID]
				s.mu.Unlock()
				if ch != nil {
					select {
					case ch <- f:
					default:
					}
				}
			case acp.ControlExit:
				s.mu.Lock()
				s.info.ExitCode = f.Code
				s.mu.Unlock()
				return io.EOF
			}
			continue
		}

		// ACP JSON-RPC message from the CLI.
		s.recordTranscript("in", f.Method, line)

		switch {
		case f.Method == acp.MethodRequestPermission && len(f.ID) > 0:
			// Handle permission request in a goroutine so the reader loop
			// continues processing updates, secret acks, or exit events.
			go b.handlePermissionRequest(ctx, s, f)
		case f.Method != "" && len(f.ID) == 0:
			// Notification from the agent (e.g. session/update).
		case len(f.ID) > 0 && f.Method == "":
			// Response to a bridge request (initialize, session/new, session/prompt).
			idKey := normalizeID(f.ID)
			s.mu.Lock()
			ch := s.pendingRPC[idKey]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
		}
	}
}

func (s *Session) recordTranscript(dir, method string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txSeq++
	now := time.Now().UTC()
	s.info.Updated = now
	ev := TranscriptEvent{
		Seq:       s.txSeq,
		Time:      now,
		Agent:     s.info.Agent,
		SessionID: s.info.ACPSessionID,
		Direction: dir,
		Method:    method,
		Message:   append(json.RawMessage(nil), raw...),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line := append(b, '\n')
	if s.txFile != nil {
		_, _ = s.txFile.Write(line)
		_ = s.txFile.Sync()
	}
	if s.txSecond != nil {
		_, _ = s.txSecond.Write(line)
		_ = s.txSecond.Sync()
	}
	for _, sub := range s.subs {
		select {
		case sub <- append(json.RawMessage(nil), raw...):
		default:
		}
	}
}

// ---- Permissions (default permissions="ask") --------------------------------

func (b *Bridge) agentPermissions(ctx context.Context, s *Session) string {
	var info gateway.AgentInfo
	if err := b.gwCall(ctx, "agent.status", map[string]any{"agent": s.info.Agent}, &info); err == nil && info.Permissions != "" {
		s.mu.Lock()
		s.permissions = info.Permissions
		s.mu.Unlock()
		return info.Permissions
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.permissions == "" {
		return gateway.PermAsk
	}
	return s.permissions
}

func (b *Bridge) handlePermissionRequest(ctx context.Context, s *Session, req acp.Frame) {
	var rp acp.RequestPermissionParams
	_ = json.Unmarshal(req.Params, &rp)

	policy := b.agentPermissions(ctx, s)
	var approve bool
	var optID, comment string

	switch policy {
	case gateway.PermAuto:
		approve = true
		optID = rp.PickOption(true)
	case gateway.PermDenyShell:
		approve = !rp.IsShell()
		optID = rp.PickOption(approve)
		if !approve {
			comment = "shell execution denied by deny-shell policy"
		}
	default: // gateway.PermAsk is the default for all agents
		summary := summarizePermission(rp)
		b.mu.Lock()
		b.nextPerm++
		pr := &PermissionRequest{
			ID:        fmt.Sprintf("perm-%d", b.nextPerm),
			Agent:     s.info.Agent,
			SessionID: s.info.ID,
			Kind:      "acp",
			Summary:   summary,
			Params:    append(json.RawMessage(nil), req.Params...),
			State:     PermStatePending,
			Created:   time.Now().UTC(),
			decideCh:  make(chan permDecision, 1),
		}
		b.perms = append(b.perms, pr)
		b.permCond.Broadcast()
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-s.doneCh:
			return
		case dec := <-pr.decideCh:
			approve = dec.approve
			optID = dec.optionID
			if optID == "" {
				optID = rp.PickOption(approve)
			}
			comment = dec.comment
		}
	}

	decisionStr := "reject"
	if approve {
		decisionStr = "approve"
	}
	res := acp.RequestPermissionResult{
		Outcome: acp.PermissionOutcome{
			Outcome:  "selected",
			OptionID: optID,
		},
		Approved: approve,
		Decision: decisionStr,
		Comment:  comment,
	}
	resRaw, _ := json.Marshal(res)
	resp := acp.Frame{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  resRaw,
	}
	respBytes, _ := json.Marshal(resp)
	s.recordTranscript("out", acp.MethodRequestPermission, respBytes)
	_ = s.conn.WriteFrame(respBytes)
}

func summarizePermission(rp acp.RequestPermissionParams) string {
	if rp.Command != "" {
		return rp.Command
	}
	if len(rp.ToolCall) > 0 {
		var tc struct {
			Title string `json:"title"`
			Kind  string `json:"kind"`
			Name  string `json:"name"`
		}
		if json.Unmarshal(rp.ToolCall, &tc) == nil {
			if tc.Title != "" {
				return tc.Title
			}
			if tc.Name != "" {
				return tc.Name
			}
			if tc.Kind != "" {
				return tc.Kind
			}
		}
	}
	if rp.Tool != "" {
		return rp.Tool
	}
	return "tool call permission"
}

// ListPermissions returns pending (or all) permission requests.
func (b *Bridge) ListPermissions(agent string, pendingOnly bool) []PermissionRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []PermissionRequest{}
	for _, pr := range b.perms {
		if agent != "" && pr.Agent != agent {
			continue
		}
		if pendingOnly && pr.State != PermStatePending {
			continue
		}
		cp := *pr
		cp.decideCh = nil
		out = append(out, cp)
	}
	return out
}

// DecidePermission resolves a pending permission request (`acp` or `register_agent`).
func (b *Bridge) DecidePermission(ctx context.Context, id string, approve bool, optionID, comment string) (*PermissionRequest, error) {
	b.mu.Lock()
	var target *PermissionRequest
	for _, pr := range b.perms {
		if pr.ID == id {
			target = pr
			break
		}
	}
	if target == nil {
		b.mu.Unlock()
		return nil, &gateway.Error{Code: gateway.CodeNotFound, Message: fmt.Sprintf("no permission request %q", id)}
	}
	if target.State != PermStatePending {
		st := target.State
		b.mu.Unlock()
		return nil, &gateway.Error{Code: gateway.CodeConflict, Message: fmt.Sprintf("permission request %s is already %s", id, st)}
	}
	now := time.Now().UTC()
	if approve {
		target.State = PermStateApproved
	} else {
		target.State = PermStateDenied
	}
	target.OptionID = optionID
	target.Comment = comment
	target.Resolved = &now
	kind := target.Kind
	targetAgent := target.TargetAgent
	ch := target.decideCh
	cp := *target
	cp.decideCh = nil
	b.permCond.Broadcast()
	b.mu.Unlock()

	if kind == "register_agent" && targetAgent != "" {
		_ = b.gwCall(ctx, "agent.approve", map[string]any{
			"agent":   targetAgent,
			"approve": approve,
			"comment": comment,
		}, nil)
	}
	if ch != nil {
		select {
		case ch <- permDecision{approve: approve, optionID: optionID, comment: comment}:
		default:
		}
	}
	return &cp, nil
}

// SetAgentPermissions updates an agent's permission policy on the gateway and
// caches it on any active session.
func (b *Bridge) SetAgentPermissions(ctx context.Context, agent, permissions string) (*gateway.AgentInfo, error) {
	var info gateway.AgentInfo
	if err := b.gwCall(ctx, "agent.permissions", map[string]any{
		"agent":       agent,
		"permissions": permissions,
	}, &info); err != nil {
		return nil, err
	}
	b.mu.Lock()
	if s := b.sessions[agent]; s != nil {
		s.mu.Lock()
		s.permissions = info.Permissions
		s.mu.Unlock()
	}
	b.mu.Unlock()
	return &info, nil
}

// ApproveAgent approves or rejects a pending child registration on the gateway
// and wakes any waiting `register_agent` call in the bridge.
func (b *Bridge) ApproveAgent(ctx context.Context, agent string, approve bool, comment string) (*gateway.AgentInfo, error) {
	var info gateway.AgentInfo
	if err := b.gwCall(ctx, "agent.approve", map[string]any{
		"agent":   agent,
		"approve": approve,
		"comment": comment,
	}, &info); err != nil {
		return nil, err
	}
	b.mu.Lock()
	for _, pr := range b.perms {
		if pr.Kind == "register_agent" && pr.TargetAgent == agent && pr.State == PermStatePending {
			now := time.Now().UTC()
			if approve {
				pr.State = PermStateApproved
			} else {
				pr.State = PermStateDenied
			}
			pr.Comment = comment
			pr.Resolved = &now
			if pr.decideCh != nil {
				select {
				case pr.decideCh <- permDecision{approve: approve, comment: comment}:
				default:
				}
			}
		}
	}
	b.permCond.Broadcast()
	b.mu.Unlock()
	return &info, nil
}

// RegisterAgent registers a new agent. When called by an agent whose permission
// policy is `permissions: "ask"` (the default), it holds the registration in the
// bridge's pending-permissions queue and waits for human approval before
// returning the active registration token.
func (b *Bridge) RegisterAgent(ctx context.Context, auth gateway.Auth, params map[string]any) (*gateway.RegisterResult, error) {
	clean := make(map[string]any, len(params))
	for k, v := range params {
		clean[k] = v
	}
	waitApproval := true
	if w, ok := clean["wait_approval"].(bool); ok {
		waitApproval = w
	}
	delete(clean, "wait_approval")
	delete(clean, "uid")
	if _, hasLoc := clean["location"]; !hasLoc {
		kind, _ := clean["kind"].(string)
		switch kind {
		case "process", "subprocess":
			kind = "subprocess"
		case "service", "systemd":
			kind = "systemd"
		case "vm", "microvm":
			kind = "microvm"
		case "container":
			kind = "container"
		case "remote":
			kind = "remote"
		default:
			kind = "other"
		}
		detail, _ := clean["unit"].(string)
		if detail == "" {
			detail, _ = clean["workdir"].(string)
		}
		clean["location"] = map[string]any{"kind": kind, "detail": detail}
	}
	delete(clean, "kind")
	delete(clean, "unit")
	delete(clean, "workdir")

	reqParams := withAuth(auth, clean)
	var reg gateway.RegisterResult
	if err := b.gwCall(ctx, "agent.register", reqParams, &reg); err != nil {
		return nil, err
	}
	if reg.State != gateway.StatePending {
		return &reg, nil
	}

	// Determine the calling agent id for the permission queue entry.
	callerAgent := ""
	if auth.OnBehalfOf != nil {
		callerAgent = auth.OnBehalfOf.Agent
	} else if auth.AgentToken != "" {
		var list []gateway.AgentInfo
		if err := b.gwCall(ctx, "agent.list", withAuth(auth, nil), &list); err == nil {
			for _, a := range list {
				if a.ID == reg.Agent && a.Parent != "" {
					callerAgent = a.Parent
					break
				}
			}
		}
	}
	rawSpec, _ := json.Marshal(clean)
	b.mu.Lock()
	b.nextPerm++
	pr := &PermissionRequest{
		ID:          fmt.Sprintf("perm-%d", b.nextPerm),
		Agent:       callerAgent,
		Kind:        "register_agent",
		TargetAgent: reg.Agent,
		Summary:     fmt.Sprintf("register child agent %q", reg.Agent),
		Params:      rawSpec,
		State:       PermStatePending,
		Created:     time.Now().UTC(),
		decideCh:    make(chan permDecision, 1),
	}
	b.perms = append(b.perms, pr)
	b.permCond.Broadcast()
	b.mu.Unlock()

	if !waitApproval {
		return &reg, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case dec := <-pr.decideCh:
		if !dec.approve {
			msg := fmt.Sprintf("permission denied: registration of agent %q was rejected by human", reg.Agent)
			if dec.comment != "" {
				msg += ": " + dec.comment
			}
			return nil, &gateway.Error{Code: gateway.CodeForbidden, Message: msg}
		}
		reg.State = gateway.StateRegistered
		return &reg, nil
	}
}

// ---- Sessions, Prompt, Cancel, Transcript & Stream --------------------------

// WaitReady waits until the named agent has an ACP session in the ready state.
func (b *Bridge) WaitReady(ctx context.Context, agent string) (*Session, error) {
	b.mu.Lock()
	s := b.sessions[agent]
	if s != nil {
		s.mu.Lock()
		ready := s.readyCh
		s.mu.Unlock()
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ready:
			return s, nil
		}
	}
	ch := make(chan *Session, 1)
	b.readyWait[agent] = append(b.readyWait[agent], ch)
	b.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s := <-ch:
		if s == nil {
			return nil, errors.New("session closed")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.readyCh:
			return s, nil
		}
	}
}

func (b *Bridge) findSession(id string) *Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.sessions[id]; s != nil {
		return s
	}
	return b.bySessionID[id]
}

// ListSessions lists active and on-disk sessions, optionally filtered by agent.
func (b *Bridge) ListSessions(agent string) ([]SessionInfo, error) {
	b.mu.Lock()
	seen := map[string]bool{}
	out := []SessionInfo{}
	for _, s := range b.sessions {
		info := s.Info()
		if agent != "" && info.Agent != agent {
			continue
		}
		seen[info.Agent] = true
		out = append(out, info)
	}
	b.mu.Unlock()

	// Also include on-disk session directories from earlier runs.
	entries, err := os.ReadDir(b.cfg.SessionsDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			if agent != "" && e.Name() != agent {
				continue
			}
			txPath := filepath.Join(b.cfg.SessionsDir, e.Name(), "transcript.jsonl")
			if fi, err := os.Stat(txPath); err == nil {
				out = append(out, SessionInfo{
					ID:             e.Name(),
					Agent:          e.Name(),
					ACPSessionID:   e.Name(),
					State:          SessionDisconnected,
					TranscriptPath: txPath,
					Created:        fi.ModTime().UTC(),
					Updated:        fi.ModTime().UTC(),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Prompt sends a `session/prompt` to a connected agent's ACP session and waits
// for the turn to complete.
func (b *Bridge) Prompt(ctx context.Context, sessionOrAgentID, text string) (*acp.SessionPromptResult, error) {
	return b.PromptBlocks(ctx, sessionOrAgentID, []acp.ContentBlock{{Type: "text", Text: text}})
}

// PromptBlocks sends a `session/prompt` with content blocks to a connected
// agent's ACP session.
func (b *Bridge) PromptBlocks(ctx context.Context, sessionOrAgentID string, blocks []acp.ContentBlock) (*acp.SessionPromptResult, error) {
	s := b.findSession(sessionOrAgentID)
	if s == nil {
		return nil, &gateway.Error{Code: gateway.CodeNotFound, Message: fmt.Sprintf("no active session %q", sessionOrAgentID)}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.doneCh:
		return nil, &gateway.Error{Code: gateway.CodeConflict, Message: fmt.Sprintf("session %q is disconnected", sessionOrAgentID)}
	case <-s.readyCh:
	}

	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	s.mu.Lock()
	s.info.State = SessionBusy
	acpID := s.info.ACPSessionID
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.info.State == SessionBusy {
			s.info.State = SessionReady
		}
		s.mu.Unlock()
	}()

	var res acp.SessionPromptResult
	err := b.callAgentRPC(ctx, s, acp.MethodSessionPrompt, acp.SessionPromptParams{
		SessionID: acpID,
		Prompt:    blocks,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// Cancel sends a `session/cancel` notification to an active session.
func (b *Bridge) Cancel(ctx context.Context, sessionOrAgentID string) error {
	s := b.findSession(sessionOrAgentID)
	if s == nil {
		return &gateway.Error{Code: gateway.CodeNotFound, Message: fmt.Sprintf("no active session %q", sessionOrAgentID)}
	}
	s.mu.Lock()
	acpID := s.info.ACPSessionID
	s.mu.Unlock()

	rawParams, _ := json.Marshal(acp.SessionCancelParams{SessionID: acpID})
	notif := acp.Frame{
		JSONRPC: "2.0",
		Method:  acp.MethodSessionCancel,
		Params:  rawParams,
	}
	raw, _ := json.Marshal(notif)
	s.recordTranscript("out", acp.MethodSessionCancel, raw)
	return s.conn.WriteFrame(raw)
}

// StopAgent stops an agent (and its subtree) via the gateway and immediately
// signals any connected `silo connect` wrappers in that subtree.
func (b *Bridge) StopAgent(ctx context.Context, auth gateway.Auth, agent, mode string) (*gateway.StopResult, error) {
	if mode == "" {
		mode = "graceful"
	}
	var res gateway.StopResult
	if err := b.gwCall(ctx, "agent.stop", withAuth(auth, map[string]any{
		"agent": agent,
		"mode":  mode,
	}), &res); err != nil {
		return nil, err
	}
	for _, id := range res.Stopped {
		b.mu.Lock()
		s := b.sessions[id]
		b.mu.Unlock()
		if s != nil {
			s.mu.Lock()
			s.info.State = SessionStopped
			conn := s.conn
			s.mu.Unlock()
			if conn != nil {
				_ = sendFrame(conn, acp.Frame{Silo: acp.ControlStop, Mode: mode})
			}
		}
	}
	return &res, nil
}

// ReadTranscript reads transcript events for a session (since seq > after).
func (b *Bridge) ReadTranscript(sessionOrAgentID string, after int64) ([]TranscriptEvent, error) {
	txPath := b.transcriptFileFor(sessionOrAgentID)
	if txPath == "" {
		return nil, &gateway.Error{Code: gateway.CodeNotFound, Message: fmt.Sprintf("no transcript for %q", sessionOrAgentID)}
	}
	f, err := os.Open(txPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []TranscriptEvent{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []TranscriptEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev TranscriptEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	if out == nil {
		out = []TranscriptEvent{}
	}
	return out, sc.Err()
}

func (b *Bridge) transcriptFileFor(sessionOrAgentID string) string {
	if s := b.findSession(sessionOrAgentID); s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.info.TranscriptPath
	}
	p := filepath.Join(b.cfg.SessionsDir, sessionOrAgentID, "transcript.jsonl")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	matches, _ := filepath.Glob(filepath.Join(b.cfg.SessionsDir, sessionOrAgentID, "*.jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

// Subscribe attaches a live subscriber channel to a session, optionally
// returning historical raw ACP messages first.
func (b *Bridge) Subscribe(sessionOrAgentID string, includeHistory bool) ([]json.RawMessage, <-chan json.RawMessage, func(), error) {
	s := b.findSession(sessionOrAgentID)
	var history []json.RawMessage
	if includeHistory {
		evs, err := b.ReadTranscript(sessionOrAgentID, 0)
		if err != nil && s == nil {
			return nil, nil, nil, err
		}
		for _, ev := range evs {
			history = append(history, ev.Message)
		}
	}
	if s == nil {
		ch := make(chan json.RawMessage)
		close(ch)
		return history, ch, func() {}, nil
	}
	s.mu.Lock()
	s.subSeq++
	id := s.subSeq
	ch := make(chan json.RawMessage, 128)
	s.subs[id] = ch
	s.mu.Unlock()

	cancel := func() {
		s.mu.Lock()
		if sub, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(sub)
		}
		s.mu.Unlock()
	}
	return history, ch, cancel, nil
}

// ReadAgent implements `read_agent`: returns structured status for any child
// the caller may see, and includes the session transcript only when the caller
// is not the root agent (design §8.6).
func (b *Bridge) ReadAgent(ctx context.Context, auth gateway.Auth, agent string, since int64) (map[string]any, error) {
	var info gateway.AgentInfo
	if err := b.gwCall(ctx, "agent.status", withAuth(auth, map[string]any{"agent": agent}), &info); err != nil {
		return nil, err
	}
	out := map[string]any{
		"agent":       info.ID,
		"state":       info.State,
		"parent":      info.Parent,
		"runtime":     info.Runtime,
		"permissions": info.Permissions,
		"limits":      info.Limits,
		"budget_usd":  info.BudgetUSD,
		"children":    info.Children,
	}
	if info.ExitCode != nil {
		out["exit_code"] = *info.ExitCode
	}
	if info.Report != nil {
		out["report"] = info.Report
	}
	if info.Tombstone != nil {
		out["tombstone"] = info.Tombstone
	}

	// Check whether caller is the root agent. Root gets structured status only
	// (design §8.6) so child-written prose never enters root's context.
	callerIsRoot := false
	if auth.OnBehalfOf != nil && auth.OnBehalfOf.Agent == gateway.RootAgentID {
		callerIsRoot = true
	} else if auth.AgentToken != "" {
		var selfList []gateway.AgentInfo
		if err := b.gwCall(ctx, "agent.list", withAuth(auth, nil), &selfList); err == nil {
			for _, a := range selfList {
				if a.Root {
					callerIsRoot = true
					break
				}
			}
		}
	}
	if !callerIsRoot {
		evs, _ := b.ReadTranscript(agent, since)
		out["transcript"] = evs
	}
	return out, nil
}

// ---- Secret requests & zero-context delivery via `silo connect` -------------

// RequestSecret files a secret request on the gateway for the calling agent and,
// when wait is true, blocks until the human provides or rejects the secret.
// It returns only `{status, name, path}` — never the secret value.
func (b *Bridge) RequestSecret(ctx context.Context, auth gateway.Auth, name, reason, path string, wait bool) (*SecretStoredResult, error) {
	params := map[string]any{"name": name, "reason": reason}
	if path != "" {
		params["path"] = path
	}
	var sr gateway.SecretRequest
	if err := b.gwCall(ctx, "secret.request", withAuth(auth, params), &sr); err != nil {
		return nil, err
	}
	if !wait {
		return &SecretStoredResult{Status: sr.State, ID: sr.ID, Name: sr.Name, Path: sr.Path}, nil
	}

	ch := make(chan SecretStoredResult, 1)
	b.mu.Lock()
	b.secretWaiters[sr.ID] = append(b.secretWaiters[sr.ID], ch)
	b.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Status == gateway.SecretRejected {
			msg := fmt.Sprintf("secret %q was rejected", sr.Name)
			if res.Note != "" {
				msg += ": " + res.Note
			}
			return nil, &gateway.Error{Code: gateway.CodeForbidden, Message: msg}
		}
		return &SecretStoredResult{Status: res.Status, Name: res.Name, Path: res.Path}, nil
	}
}

// ProvideSecret sends the human-supplied secret value over the owning agent's
// `silo connect` control channel so `silo connect` writes it atomically (0600)
// to the requested file in the agent's environment, then records the outcome on
// the gateway without the value ever touching gateway state, transcripts, or
// audit logs.
func (b *Bridge) ProvideSecret(ctx context.Context, id, value string) (*SecretStoredResult, error) {
	if value == "" {
		return nil, &gateway.Error{Code: gateway.CodeInvalidParams, Message: "secret value must not be empty"}
	}
	var list []gateway.SecretRequest
	if err := b.gwCall(ctx, "secret.list", map[string]any{"pending": true}, &list); err != nil {
		return nil, err
	}
	var target *gateway.SecretRequest
	for i := range list {
		if list[i].ID == id {
			target = &list[i]
			break
		}
	}
	if target == nil {
		return nil, &gateway.Error{Code: gateway.CodeNotFound, Message: fmt.Sprintf("no pending secret request %q", id)}
	}

	b.mu.Lock()
	s := b.sessions[target.Agent]
	b.mu.Unlock()
	if s == nil {
		return nil, &gateway.Error{Code: gateway.CodeConflict, Message: fmt.Sprintf("agent %q has no active silo connect session", target.Agent)}
	}

	ackCh := make(chan acp.Frame, 1)
	s.mu.Lock()
	s.secretWait[id] = ackCh
	conn := s.conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.secretWait, id)
		s.mu.Unlock()
	}()

	if err := sendFrame(conn, acp.Frame{
		Silo:  acp.ControlSecretWrite,
		ReqID: id,
		Name:  target.Name,
		Path:  target.Path,
		Value: value,
	}); err != nil {
		return nil, err
	}

	var ack acp.Frame
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.doneCh:
		return nil, errors.New("agent disconnected while writing secret")
	case ack = <-ackCh:
	}
	if ack.Err != "" {
		return nil, fmt.Errorf("silo connect secret write failed: %s", ack.Err)
	}
	writtenPath := ack.Path
	if writtenPath == "" {
		writtenPath = target.Path
	}

	// Record on the gateway that the secret was stored (never passing `value`).
	var resolved gateway.SecretRequest
	if err := b.gwCall(ctx, "secret.provide", map[string]any{
		"id":   id,
		"path": writtenPath,
	}, &resolved); err != nil {
		return nil, err
	}

	res := SecretStoredResult{
		Status: gateway.SecretStored,
		Name:   target.Name,
		Path:   writtenPath,
	}
	b.mu.Lock()
	waiters := b.secretWaiters[id]
	delete(b.secretWaiters, id)
	b.mu.Unlock()
	for _, w := range waiters {
		w <- res
		close(w)
	}
	return &res, nil
}

// RejectSecret records the human's rejection of a pending secret request and
// notifies any waiting agent call.
func (b *Bridge) RejectSecret(ctx context.Context, id, note string) (*gateway.SecretRequest, error) {
	var resolved gateway.SecretRequest
	if err := b.gwCall(ctx, "secret.reject", map[string]any{
		"id":   id,
		"note": note,
	}, &resolved); err != nil {
		return nil, err
	}
	res := SecretStoredResult{
		Status: gateway.SecretRejected,
		ID:     resolved.ID,
		Name:   resolved.Name,
		Path:   resolved.Path,
		Note:   note,
	}
	b.mu.Lock()
	waiters := b.secretWaiters[id]
	delete(b.secretWaiters, id)
	b.mu.Unlock()
	for _, w := range waiters {
		w <- res
		close(w)
	}
	return &resolved, nil
}

func sendFrame(fc FrameConn, f acp.Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return fc.WriteFrame(b)
}

// ForwardRequestToRoot injects a forwarded child request into `root`'s session
// transcript (and active ACP session if connected), marked clearly as untrusted
// content (design §8.6).
func (b *Bridge) ForwardRequestToRoot(ctx context.Context, req gateway.AgentRequest, note string) {
	msg := fmt.Sprintf("Request from agent %s (untrusted content):\nSummary: %s\nDetail: %s", req.Agent, req.Summary, req.Detail)
	if note != "" {
		msg += fmt.Sprintf("\nOperator note: %s", note)
	}
	if s := b.findSession(gateway.RootAgentID); s != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = b.Prompt(context.Background(), gateway.RootAgentID, msg)
		}()
		select {
		case <-done:
		case <-time.After(50 * time.Millisecond):
		}
		return
	}
	// If root does not have a live connected session yet, append the untrusted
	// prompt event directly to root's transcript file so it is visible in the UI.
	dir := filepath.Join(b.cfg.SessionsDir, gateway.RootAgentID)
	_ = os.MkdirAll(dir, 0o700)
	txPath := filepath.Join(dir, "transcript.jsonl")
	existing, _ := b.ReadTranscript(gateway.RootAgentID, 0)
	seq := int64(len(existing)) + 1
	rawParams, _ := json.Marshal(acp.SessionPromptParams{
		SessionID: gateway.RootAgentID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: msg}},
	})
	rawFrame, _ := json.Marshal(acp.Frame{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  acp.MethodSessionPrompt,
		Params:  rawParams,
	})
	ev := TranscriptEvent{
		Seq:       seq,
		Time:      time.Now().UTC(),
		Agent:     gateway.RootAgentID,
		SessionID: gateway.RootAgentID,
		Direction: "out",
		Method:    acp.MethodSessionPrompt,
		Message:   rawFrame,
	}
	if line, err := json.Marshal(ev); err == nil {
		if f, err := os.OpenFile(txPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}
}
