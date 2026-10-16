package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

const userHeader = "X-Test-User"

// logf is t.Logf until the test's cleanups run, and a no-op after:
// goroutines of a hub or a node can log once their test has returned.
func logf(t *testing.T) func(string, ...any) {
	var (
		mu   sync.Mutex
		done bool
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		done = true
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}

// running is a hub serving on a listener of its own.
type running struct {
	url  string
	stop context.CancelFunc
	done chan error
}

// start serves cfg on addr ("127.0.0.1:0" for any port).
func start(t *testing.T, cfg Config, addr string) *running {
	t.Helper()
	var (
		l   net.Listener
		err error
	)
	// The port a stopped hub held can take a moment to be free again.
	for range 50 {
		if l, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{url: "http://" + l.Addr().String(), stop: cancel, done: make(chan error, 1)}
	go func() { r.done <- Serve(ctx, l, cfg) }()
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

// shutdown stops r as SIGTERM would, and waits for it.
func (r *running) shutdown(t *testing.T) {
	t.Helper()
	r.stop()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		r.done <- nil
	case <-time.After(30 * time.Second):
		t.Fatal("the hub did not shut down")
	}
}

// call makes an API request as user ("" for none) and decodes a JSON
// answer into out, if it is not nil.
func call(t *testing.T, method, url, user string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.Header.Set(userHeader, user)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, url, err, b)
		}
	}
	if resp.StatusCode >= 300 && out != nil {
		t.Fatalf("%s %s: %s: %s", method, url, resp.Status, b)
	}
	return resp.StatusCode
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// online is whether alice sees node id online in the hub's directory.
func online(t *testing.T, url, id string) bool {
	var dir struct {
		Nodes []registry.Entry `json:"nodes"`
	}
	call(t, "GET", url+rendezvous.DirectoryPath, "alice@example.com", nil, &dir)
	for _, e := range dir.Nodes {
		if e.ID == id && e.Status == registry.Online {
			return true
		}
	}
	return false
}

func taskState(t *testing.T, url, id string) tasks.State {
	var tk tasks.Task
	call(t, "GET", url+tasks.TasksPath+"/"+id, "alice@example.com", nil, &tk)
	return tk.State
}

// node is a grain node's end of the task interface, as far as this
// test needs: it accepts every offer and resume it is handed, and
// remembers them.
type node struct {
	mu   sync.Mutex
	seen []string // message types, in order
	logs []string // the rendezvous client's log lines
}

func (n *node) handle(c net.Conn) {
	defer c.Close()
	st := tasks.NewStream(c)
	m, err := st.Read()
	if err != nil {
		return
	}
	n.mu.Lock()
	n.seen = append(n.seen, m.Type)
	n.mu.Unlock()
	if m.Type == tasks.TypeOffer || m.Type == tasks.TypeResume {
		st.Write(tasks.Message{Type: tasks.TypeAccept, NodeTask: "local-1"})
	}
	for {
		if _, err := st.Read(); err != nil {
			return
		}
	}
}

func (n *node) saw(typ string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.seen {
		if s == typ {
			return true
		}
	}
	return false
}

func (n *node) logged(substr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// TestServeRestart is `silo hub serve` end to end on PostgreSQL: a
// team and a node token are set up through the API, a node connects
// with the token and is handed a task, and then the hub restarts, as
// a pod does when it is rolled. The node is told why its connection
// ended, reconnects to the new hub and registers again, and the new
// hub asks it to resume the task it was running.
func TestServeRestart(t *testing.T) {
	pg := pgtest.Start(t)
	cfg := Config{
		Database:      pg.URL,
		DevUserHeader: userHeader,
		HubAdmins:     []string{"admin@example.com"},
		Heartbeat:     time.Second,
		ShutdownGrace: 10 * time.Second,
		DispatchRetry: time.Second,
		Logf:          logf(t),
	}
	hub := start(t, cfg, "127.0.0.1:0")
	addr := strings.TrimPrefix(hub.url, "http://")

	for _, p := range []string{HealthzPath, ReadyzPath} {
		if code := call(t, "GET", hub.url+p, "", nil, nil); code != http.StatusOK {
			t.Fatalf("%s: %d", p, code)
		}
	}
	if code := call(t, "GET", hub.url+"/v1/me", "", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("/v1/me signed out: %d, want 401", code)
	}

	call(t, "POST", hub.url+"/v1/teams", "admin@example.com", map[string]string{"id": "infra", "name": "Infra"}, &struct{}{})
	call(t, "PUT", hub.url+"/v1/teams/infra/members/alice@example.com", "admin@example.com", map[string]string{"role": "member"}, &struct{}{})
	call(t, "PUT", hub.url+"/v1/teams/infra/repos/bwsalmon/silo", "admin@example.com", nil, &struct{}{})
	var minted struct {
		Token string `json:"token"`
	}
	call(t, "POST", hub.url+"/v1/node-tokens", "alice@example.com", map[string]string{"node": "ws-1"}, &minted)
	if minted.Token == "" {
		t.Fatal("no node token minted")
	}

	// A token for another node, and no token, are refused at the
	// handshake.
	bad := &rendezvous.Client{Hub: hub.url, Token: func(context.Context) (string, error) { return "silo-node_nope_nope", nil },
		Node: registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 1}}
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := bad.Connect(cctx); err == nil {
		t.Error("a node with a bogus token connected")
	}
	ccancel()

	n := &node{}
	cl := &rendezvous.Client{
		Hub:   hub.url,
		Token: func(context.Context) (string, error) { return minted.Token, nil },
		Node:  registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 1},
		Logf: func(format string, args ...any) {
			n.mu.Lock()
			n.logs = append(n.logs, fmt.Sprintf(format, args...))
			n.mu.Unlock()
		},
	}
	nctx, ncancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- cl.Run(nctx, n.handle) }()
	t.Cleanup(func() {
		ncancel()
		<-ran
	})
	eventually(t, "ws-1 online", func() bool { return online(t, hub.url, "ws-1") })

	var task tasks.Task
	call(t, "POST", hub.url+tasks.TasksPath, "alice@example.com", map[string]string{"repo": "bwsalmon/silo", "prompt": "do the thing"}, &task)
	eventually(t, "the task offered to ws-1", func() bool { return n.saw(tasks.TypeOffer) })
	eventually(t, "the task running", func() bool { return taskState(t, hub.url, task.ID) == tasks.Running })

	// The hub restarts. Its replacement serves the same address and the
	// same database, as a rolled pod behind the same Service does.
	hub.shutdown(t)
	if !n.logged("the hub is shutting down") {
		t.Errorf("the node was not told why its connection ended; it logged %q", n.logs)
	}
	select {
	case err := <-ran:
		t.Fatalf("the node stopped instead of reconnecting: %v", err)
	default:
	}

	hub = start(t, cfg, addr)
	eventually(t, "ws-1 online again", func() bool { return online(t, hub.url, "ws-1") })
	eventually(t, "the task resumed on ws-1", func() bool { return n.saw(tasks.TypeResume) })
	eventually(t, "the task running again", func() bool { return taskState(t, hub.url, task.ID) == tasks.Running })
}

// TestConfigSignOn checks that exactly one human sign-on is required.
func TestConfigSignOn(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []Config{
		{Database: "sqlite::memory:"},
		{Database: "sqlite::memory:", IAPAudience: "/projects/1/global/backendServices/2", DevUserHeader: "X-User"},
		{Database: "sqlite::memory:", IAPAudience: "my-backend"},
		{Database: "sqlite::memory:", OIDCIssuer: "https://accounts.example.com"},
		{IAPAudience: "/projects/1/global/backendServices/2"},
	} {
		if h, err := New(ctx, cfg); err == nil {
			h.Close()
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
	h, err := New(ctx, Config{Database: "sqlite::memory:", IAPAudience: "/projects/1/global/backendServices/2"})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
}

// TestReadyzWhileDraining checks that /readyz turns unready as soon as
// shutdown starts, and that a node connecting then is turned away.
func TestReadyzWhileDraining(t *testing.T) {
	h, err := New(context.Background(), Config{Database: "sqlite::memory:", DevUserHeader: userHeader, Logf: logf(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.draining.Store(true)
	req, _ := http.NewRequest("GET", ReadyzPath, nil)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz while draining: %d, want 503", rec.Code)
	}
	if err := h.Rendezvous.Shutdown(context.Background(), ""); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
