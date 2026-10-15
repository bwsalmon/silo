package rendezvous

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/registry"
)

// tokens is a static node credential table, token to owner. A token
// ending "@node" is bound to that node.
func tokens(table map[string]string) func(*http.Request) (Identity, error) {
	return func(r *http.Request) (Identity, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if owner := table[tok]; ok && owner != "" {
			_, node, _ := strings.Cut(tok, "@")
			return Identity{Owner: owner, Node: node}, nil
		}
		return Identity{}, errors.New("unknown token")
	}
}

func hub(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{
		Registry:     registry.New(),
		Authenticate: tokens(map[string]string{"alice-tok": "alice@example.com", "bob-tok": "bob@example.com", "alice-tok@ws-9": "alice@example.com"}),
		Heartbeat:    time.Second,
		Logf:         t.Logf,
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func client(url, tok string, n registry.Node) *Client {
	return &Client{
		Hub:   url,
		Token: func(context.Context) (string, error) { return tok, nil },
		Node:  n,
	}
}

// eventually waits for cond, which reads the registry.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestRegisterAndHeartbeat(t *testing.T) {
	s, ts := hub(t)
	var free atomic.Int64
	free.Store(2)
	cl := client(ts.URL, "alice-tok", registry.Node{ID: "ws-1", Repos: []string{"bwsalmon/silo"}, Slots: 2, Version: "test"})
	cl.Free = func() int { return int(free.Load()) }
	c, err := cl.Connect(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.Owner() != "alice@example.com" {
		t.Errorf("owner %q, want the one the token proves", c.Owner())
	}
	e, ok := s.Registry.Get("ws-1")
	if !ok || e.Status != registry.Online || e.Owner != "alice@example.com" || e.Slots != 2 {
		t.Fatalf("entry %+v", e)
	}

	free.Store(1)
	eventually(t, "the heartbeat's free count", func() bool {
		e, _ := s.Registry.Get("ws-1")
		return e.Free == 1
	})

	// The directory serves it.
	resp, err := http.Get(ts.URL + DirectoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var dir struct{ Nodes []registry.Entry }
	if err := json.NewDecoder(resp.Body).Decode(&dir); err != nil {
		t.Fatal(err)
	}
	if len(dir.Nodes) != 1 || dir.Nodes[0].ID != "ws-1" || dir.Nodes[0].Status != registry.Online {
		t.Errorf("directory %+v", dir.Nodes)
	}

	c.Close()
	eventually(t, "the node to go offline", func() bool {
		e, _ := s.Registry.Get("ws-1")
		return e.Status == registry.Offline
	})
}

func TestUnauthenticated(t *testing.T) {
	s, ts := hub(t)
	_, err := client(ts.URL, "wrong", registry.Node{ID: "ws-1", Slots: 1}).Connect(ctx(t))
	if err == nil {
		t.Fatal("connected with a bad token")
	}
	if len(s.Registry.List()) != 0 {
		t.Errorf("directory %v, want empty", s.Registry.List())
	}
}

func TestRefusals(t *testing.T) {
	s, ts := hub(t)
	s.Admit = func(owner string, n registry.Node) error {
		for _, r := range n.Repos {
			if r == "secret/repo" {
				return fmt.Errorf("%s may not serve %s", owner, r)
			}
		}
		return nil
	}
	if _, err := client(ts.URL, "alice-tok", registry.Node{ID: "bad id"}).Connect(ctx(t)); !errors.Is(err, ErrRefused) {
		t.Errorf("invalid node: %v, want ErrRefused", err)
	}
	_, err := client(ts.URL, "alice-tok", registry.Node{ID: "ws-1", Repos: []string{"secret/repo"}}).Connect(ctx(t))
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "may not serve") {
		t.Errorf("unadmitted repo: %v, want ErrRefused saying why", err)
	}

	c, err := client(ts.URL, "alice-tok", registry.Node{ID: "ws-1", Slots: 1}).Connect(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Bob cannot take over Alice's node by naming its ID.
	_, err = client(ts.URL, "bob-tok", registry.Node{ID: "ws-1", Slots: 1}).Connect(ctx(t))
	if !errors.Is(err, ErrRefused) {
		t.Errorf("another owner's node: %v, want ErrRefused", err)
	}
	if e, _ := s.Registry.Get("ws-1"); e.Owner != "alice@example.com" || e.Status != registry.Online {
		t.Errorf("entry after the takeover attempt: %+v", e)
	}
}

func TestNodeBoundCredential(t *testing.T) {
	s, ts := hub(t)
	_, err := client(ts.URL, "alice-tok@ws-9", registry.Node{ID: "ws-1", Slots: 1}).Connect(ctx(t))
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "ws-9") {
		t.Errorf("another node's ID: %v, want ErrRefused naming the bound node", err)
	}
	c, err := client(ts.URL, "alice-tok@ws-9", registry.Node{ID: "ws-9", Slots: 1}).Connect(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if e, ok := s.Registry.Get("ws-9"); !ok || e.Owner != "alice@example.com" {
		t.Errorf("entry %+v", e)
	}
}

func TestVisibleDirectory(t *testing.T) {
	s, ts := hub(t)
	for _, tok := range []string{"alice-tok", "bob-tok"} {
		c, err := client(ts.URL, tok, registry.Node{ID: strings.TrimSuffix(tok, "-tok"), Slots: 1}).Connect(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	s.Visible = func(r *http.Request) (func(registry.Entry) bool, error) {
		who := r.Header.Get("X-Test-User")
		if who == "" {
			return nil, errors.New("no user")
		}
		return func(e registry.Entry) bool { return e.Owner == who }, nil
	}
	get := func(user string) (int, []string) {
		req, _ := http.NewRequest("GET", ts.URL+DirectoryPath, nil)
		if user != "" {
			req.Header.Set("X-Test-User", user)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d struct{ Nodes []registry.Entry }
		json.NewDecoder(resp.Body).Decode(&d)
		var ids []string
		for _, e := range d.Nodes {
			ids = append(ids, e.ID)
		}
		return resp.StatusCode, ids
	}
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Errorf("anonymous directory: %d, want 401", code)
	}
	if code, ids := get("bob@example.com"); code != 200 || len(ids) != 1 || ids[0] != "bob" {
		t.Errorf("bob's directory: %d %v, want only bob", code, ids)
	}
}

func TestReconnectSupersedes(t *testing.T) {
	s, ts := hub(t)
	cl := client(ts.URL, "alice-tok", registry.Node{ID: "ws-1", Slots: 1})
	old, err := cl.Connect(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	cur, err := cl.Connect(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	select {
	case <-old.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded connection is still up")
	}
	// The old connection's end must not take the node offline.
	time.Sleep(100 * time.Millisecond)
	if e, _ := s.Registry.Get("ws-1"); e.Status != registry.Online {
		t.Errorf("after reconnect: %+v", e)
	}
}

// TestReverseStreams checks both directions of the tunnel: the hub
// opening a stream to a node behind it (dispatch, view-through), and
// the node opening one to the hub (events).
func TestReverseStreams(t *testing.T) {
	s, ts := hub(t)
	got := make(chan string, 1)
	s.NodeStream = func(nodeID string, c net.Conn) {
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- nodeID + ": " + string(b)
	}
	c, err := client(ts.URL, "alice-tok", registry.Node{ID: "ws-1", Repos: []string{"o/r"}, Slots: 1}).Connect(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	go func() {
		st, err := c.Accept()
		if err != nil {
			return
		}
		defer st.Close()
		line, _ := bufio.NewReader(st).ReadString('\n')
		fmt.Fprintf(st, "ran %s", line)
	}()
	claim, err := s.Registry.Claim("o/r")
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Open(ctx(t), claim.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(st, "task-1\n")
	b, err := io.ReadAll(st)
	if err != nil || string(b) != "ran task-1\n" {
		t.Errorf("node answered %q, %v", b, err)
	}

	up, err := c.Open(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(up, "event")
	up.(interface{ CloseWrite() error }).CloseWrite()
	select {
	case g := <-got:
		if g != "ws-1: event" {
			t.Errorf("hub got %q", g)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node's stream never reached the hub")
	}

	if _, err := s.Open(ctx(t), "nobody"); !errors.Is(err, ErrOffline) {
		t.Errorf("Open of an unknown node = %v, want ErrOffline", err)
	}
}

func TestRunReconnects(t *testing.T) {
	s, ts := hub(t)
	cl := client(ts.URL, "alice-tok", registry.Node{ID: "ws-1", Slots: 1})
	c, cancel := context.WithCancel(ctx(t))
	done := make(chan error, 1)
	go func() { done <- cl.Run(c, func(st net.Conn) { st.Close() }) }()
	eventually(t, "the node online", func() bool {
		e, _ := s.Registry.Get("ws-1")
		return e.Status == registry.Online
	})
	// Drop the node's connection from the hub's side.
	s.mu.Lock()
	first := s.conns["ws-1"]
	s.mu.Unlock()
	first.m.Close()
	eventually(t, "the node reconnected", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		nc := s.conns["ws-1"]
		return nc != nil && nc != first
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	eventually(t, "the node offline", func() bool {
		e, _ := s.Registry.Get("ws-1")
		return e.Status == registry.Offline
	})
}
