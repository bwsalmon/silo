package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// The callers tests use. The bridge is uid 999.
var (
	adminCred    = PeerCred{UID: 0, PID: 1}
	bridgeCred   = PeerCred{UID: 999, PID: 2}
	strangerCred = PeerCred{UID: 4321, PID: 3}
)

// env is a gateway in a temp dir, called the way the socket would call it.
type env struct {
	t     *testing.T
	dir   string
	cfg   *Config
	clock *fakeClock
	audit *AuditLog
	g     *Gateway
	srv   *Server
}

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		StateDir:   filepath.Join(dir, "state"),
		AuditLog:   filepath.Join(dir, "log", "audit.jsonl"),
		SocketPath: filepath.Join(dir, "gateway.sock"),
		AdminUIDs:  []int{1000},
	}
	for _, m := range mutate {
		m(cfg)
	}
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, cfg: cfg, clock: &fakeClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}}
	e.restart()
	return e
}

// restart builds a fresh Gateway on the same state and audit log, as a
// gateway restart would, and starts it.
func (e *env) restart() {
	e.t.Helper()
	if e.audit != nil {
		e.audit.Close()
	}
	a, err := OpenAuditLog(e.cfg.AuditLog, e.clock, false)
	if err != nil {
		e.t.Fatal(err)
	}
	e.audit = a
	e.t.Cleanup(func() { a.Close() })
	g, err := New(e.cfg, Deps{
		Clock: e.clock, Audit: a, Store: Store{Dir: e.cfg.StateDir}, Logf: e.t.Logf,
		LookupUID: func(name string) (int, error) {
			if name == e.cfg.BridgeUser {
				return bridgeCred.UID, nil
			}
			return -1, nil
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := g.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.g, e.srv = g, &Server{G: g}
}

// call makes one API call as cred, through JSON as the socket would.
func (e *env) call(cred PeerCred, method string, params, result any) error {
	e.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		e.t.Fatal(err)
	}
	if params == nil {
		raw = nil
	}
	res, err := e.srv.dispatch(context.Background(), cred, method, raw)
	if err != nil {
		return err
	}
	if result != nil {
		b, err := json.Marshal(res)
		if err != nil {
			e.t.Fatal(err)
		}
		if err := json.Unmarshal(b, result); err != nil {
			e.t.Fatal(err)
		}
	}
	return nil
}

func (e *env) mustCall(cred PeerCred, method string, params, result any) {
	e.t.Helper()
	if err := e.call(cred, method, params, result); err != nil {
		e.t.Fatalf("%s: %v", method, err)
	}
}

// with adds an agent token to params.
func with(tok string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["agent_token"] = tok
	return params
}

// register registers an agent as cred (with tok, if an agent registers it)
// and returns its registration token.
func (e *env) register(cred PeerCred, tok string, params map[string]any) string {
	e.t.Helper()
	if tok != "" {
		params = with(tok, params)
	}
	var r RegisterResult
	e.mustCall(cred, "agent.register", params, &r)
	if r.Token == "" || r.HubURL == "" {
		e.t.Fatalf("register result %+v", r)
	}
	return r.Token
}

// connect connects with a registration token and returns the session token.
func (e *env) connect(reg string) string {
	e.t.Helper()
	var r ConnectResult
	e.mustCall(strangerCred, "agent.connect", map[string]any{"token": reg, "host": "h", "pid": 42}, &r)
	return r.SessionToken
}

// spawn registers and connects an agent, returning its session token.
// If the registration requires human approval (default permissions="ask"),
// spawn approves it as the human before connecting.
func (e *env) spawn(cred PeerCred, tok string, params map[string]any) string {
	e.t.Helper()
	if tok != "" {
		params = with(tok, params)
	}
	var r RegisterResult
	e.mustCall(cred, "agent.register", params, &r)
	if r.State == StatePending {
		e.mustCall(adminCred, "agent.approve", map[string]any{"agent": r.Agent}, nil)
	}
	return e.connect(r.Token)
}

// standing connects a standing agent with the token from its token file.
func (e *env) standing(id string) string {
	e.t.Helper()
	return e.connect(e.tokenFile(id))
}

func (e *env) tokenFile(id string) string {
	e.t.Helper()
	b, err := readFile(filepath.Join(e.cfg.TokenDir(), id))
	if err != nil {
		e.t.Fatalf("token file for %s: %v", id, err)
	}
	return string(b[:len(b)-1])
}

func (e *env) auditEvents() []AuditEntry {
	e.t.Helper()
	entries, err := e.audit.Read(0, 10000)
	if err != nil {
		e.t.Fatal(err)
	}
	return entries
}

func (e *env) lastEvent(event string) map[string]any {
	e.t.Helper()
	entries := e.auditEvents()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Event == event {
			var m map[string]any
			if err := json.Unmarshal(entries[i].Payload, &m); err != nil {
				e.t.Fatal(err)
			}
			m["_actor"] = entries[i].Actor
			return m
		}
	}
	e.t.Fatalf("no %s event in the audit log", event)
	return nil
}

func codeOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
