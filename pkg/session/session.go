// Package session is the per-session socket (docs/silo/agents.md, §5,
// SV-107; call 44): how a harness on the control VM reaches silod.
//
// For each session silod creates /run/silo/sessions/<agent id>/silo.sock
// and mounts only that socket into the session's container. The socket
// is the session's identity: a connection's agent is the one whose
// socket it arrived on, and nothing in a request changes it. Every call
// on it is made as the agent's silo, with exactly the authority that
// silo has from inside the cloud, and audited with via harness and the
// agent (fenceapi.Caller, Via ViaHarness, Agent set).
//
// The socket serves
//
//   - /mcp, the one MCP server in the harness's configuration: the tool
//     VM's tools, proxied down the tool channel; the fence API as tools;
//     a generic compute tool that sends one facade request; and ask and
//     request_approval;
//   - /silo/v1/ and /compute/v1/, the fence API and the facade over
//     HTTP, for adapters that would rather call them directly;
//   - /model/<provider>/, the model proxy (package modelproxy), for
//     API-key credentials: it adds the key on the way out, and meters
//     every call against the agent's budget. It is served only here,
//     never through the gateway.
//
// The silo's control policy binds the fence tools and compute as it
// binds the same calls over HTTP; silod's own checks (the fence API's
// and the facade's) are the only layer, as the socket isn't behind the
// fence. The tools, the model proxy, ask and request_approval, and the
// control surface of the silo's own agents keep working under none and
// read (agents.md, "Agents in a sealed silo", SV-100).
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/fenceapi"
)

// DefaultDir is where the sessions' sockets are made.
const DefaultDir = "/run/silo/sessions"

// SocketName is a session socket's name in its agent's directory.
const SocketName = "silo.sock"

// ToolChannel sends one MCP message down an agent's tool channel and
// returns the tool server's answer (toolchan's Hub).
type ToolChannel interface {
	MCP(ctx context.Context, agent string, msg []byte) []byte
}

// Control is the part of the fence API the socket's own tools use
// (fenceapi's Server): the session's events, and ask and
// request_approval.
type Control interface {
	RecordEvent(ctx context.Context, agent, kind string, data any) error
	OpenRequest(ctx context.Context, c fenceapi.Caller, kind, text string) (*fenceapi.AgentControlRequest, error)
	WaitAnswer(ctx context.Context, agent, id string) (*fenceapi.AgentControlRequest, error)
}

// ModelProxy carries a session's model calls to its provider (package
// modelproxy's Proxy). r's path is what follows /model/<provider>.
type ModelProxy interface {
	ServeModel(w http.ResponseWriter, r *http.Request, agent, provider string)
}

// Sockets are this silod's session sockets, one an agent. Its fields
// are set before the first Open.
type Sockets struct {
	// Dir is where sockets are made; DefaultDir if empty.
	Dir string
	// API serves /silo/v1/: the fence API.
	API http.Handler
	// Control is the fence API's control surface, for ask,
	// request_approval and the session's events.
	Control Control
	// Facade serves /compute/v1/; with none, the facade is UNAVAILABLE.
	Facade http.Handler
	// Tools reaches the agents' tool VMs; with none, their tools are
	// listed but every call is retryable.
	Tools ToolChannel
	// Model is the model proxy, served at /model/<provider>/; with none,
	// it is UNAVAILABLE.
	Model ModelProxy
	// Peer, if set, checks every connection to a socket before it is
	// served, beside the socket's file mode: on the local backend,
	// where every session's harness runs as the same user, that its
	// peer is in the session's own scope (ScopePeer, local-backend.md
	// call L18).
	Peer PeerCheck

	mu   sync.Mutex
	open map[string]*socket
}

type socket struct {
	path string
	l    net.Listener
	srv  *http.Server
}

// Path is agent's socket path.
func (s *Sockets) Path(agent string) string {
	dir := s.Dir
	if dir == "" {
		dir = DefaultDir
	}
	return filepath.Join(dir, agent, SocketName)
}

// Open creates agent's socket, in silo, and serves it, replacing one it
// has already; it returns the socket's path.
func (s *Sockets) Open(agent, silo string) (string, error) {
	if agent == "" || strings.ContainsAny(agent, "/.") || silo == "" {
		return "", fmt.Errorf("session: bad agent %q or silo %q", agent, silo)
	}
	s.Close(agent)
	path := s.Path(agent)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return "", err
	}
	// Only silod, and the session's container it is mounted into, may
	// connect: whoever can is the agent. On the local backend the
	// harness is silod's own user, and Peer tells the sessions apart.
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return "", err
	}
	srv := &http.Server{Handler: s.Handler(agent, silo), ReadHeaderTimeout: 10 * time.Second}
	served := l
	if s.Peer != nil {
		served = &peerListener{Listener: l, agent: agent, check: s.Peer}
	}
	s.mu.Lock()
	if s.open == nil {
		s.open = map[string]*socket{}
	}
	s.open[agent] = &socket{path: path, l: l, srv: srv}
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(served); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("session socket %s: %v", path, err)
		}
	}()
	return path, nil
}

// Close stops serving agent's socket and removes it.
func (s *Sockets) Close(agent string) {
	s.mu.Lock()
	so := s.open[agent]
	delete(s.open, agent)
	s.mu.Unlock()
	if so == nil {
		return
	}
	so.srv.Close()
	os.Remove(so.path)
	os.Remove(filepath.Dir(so.path))
}

// CloseAll closes every socket, as silod stops.
func (s *Sockets) CloseAll() {
	s.mu.Lock()
	var all []string
	for a := range s.open {
		all = append(all, a)
	}
	s.mu.Unlock()
	for _, a := range all {
		s.Close(a)
	}
}

// Caller is who every call on agent's socket is: its silo, by its
// harness.
func Caller(agent, silo string) fenceapi.Caller {
	return fenceapi.Caller{Silo: silo, Via: fenceapi.ViaHarness, Agent: agent}
}

// Handler serves agent's socket, in silo. Each request is made as
// Caller(agent, silo) whatever it carries: any Silo- header is dropped.
func (s *Sockets) Handler(agent, silo string) http.Handler {
	h := &handler{s: s, agent: agent, silo: silo, caller: Caller(agent, silo)}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "silo-") {
				r.Header.Del(k)
			}
		}
		r = r.WithContext(fenceapi.ContextWithCaller(r.Context(), h.caller))
		p := r.URL.Path
		switch {
		case p == "/mcp":
			h.mcp(w, r)
		case strings.HasPrefix(p, "/silo/v1/"):
			if s.API == nil {
				writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "silo: the fence API isn't served here")
				return
			}
			s.API.ServeHTTP(w, r)
		case strings.HasPrefix(p, "/compute/v1/"):
			if s.Facade == nil {
				writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "silo: the compute facade isn't served here")
				return
			}
			s.Facade.ServeHTTP(w, r)
		case strings.HasPrefix(p, "/model/"):
			if s.Model == nil {
				writeErr(w, http.StatusServiceUnavailable, "UNAVAILABLE", "silo: the model proxy isn't served here")
				return
			}
			provider, rest, _ := strings.Cut(strings.TrimPrefix(p, "/model/"), "/")
			r2 := r.Clone(r.Context())
			r2.URL.Path, r2.URL.RawPath = "/"+rest, ""
			s.Model.ServeModel(w, r2, agent, provider)
		default:
			writeErr(w, http.StatusNotFound, "NOT_FOUND", "silo: no such path "+p+"; the session socket serves /mcp, /silo/v1/, /compute/v1/ and /model/")
		}
	})
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

// Runner is a SessionRunner that opens each session's socket before it
// starts, and closes it once it stops; the Session it hands on names
// the socket to mount.
type Runner struct {
	Inner   agents.SessionRunner
	Sockets *Sockets
}

// HasSlot implements agents.SessionRunner.
func (r *Runner) HasSlot(ctx context.Context) (bool, error) { return r.Inner.HasSlot(ctx) }

// Start implements agents.SessionRunner.
func (r *Runner) Start(ctx context.Context, s agents.Session) error {
	path, err := r.Sockets.Open(s.Agent, s.Silo)
	if err != nil {
		return fmt.Errorf("opening the session socket: %w", err)
	}
	s.Socket = path
	if err := r.Inner.Start(ctx, s); err != nil {
		r.Sockets.Close(s.Agent)
		return err
	}
	return nil
}

// Stop implements agents.SessionRunner.
func (r *Runner) Stop(ctx context.Context, agent string) error {
	err := r.Inner.Stop(ctx, agent)
	if err == nil {
		r.Sockets.Close(agent)
	}
	return err
}

// StartTurn implements agents.TurnStarter, if the inner runner does.
func (r *Runner) StartTurn(ctx context.Context, agent string) error {
	if ts, ok := r.Inner.(agents.TurnStarter); ok {
		return ts.StartTurn(ctx, agent)
	}
	return nil
}
