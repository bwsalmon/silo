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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/internal/testlog"
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
		Logf:         testlog.Logf(t),
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

func TestShutdownSaysGoodbye(t *testing.T) {
	s, ts := hub(t)
	c, err := client(ts.URL, "alice-tok", registry.Node{ID: "ws-1", Repos: []string{"o/r"}, Slots: 1}).Connect(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	eventually(t, "ws-1 online", func() bool { e, _ := s.Registry.Get("ws-1"); return e.Status == registry.Online })

	if err := s.Shutdown(ctx(t), "the hub is restarting"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the node's connection outlived the hub's shutdown")
	}
	if got := c.Goodbye(); got != "the hub is restarting" {
		t.Errorf("goodbye %q, want the hub's reason", got)
	}
	eventually(t, "ws-1 offline", func() bool { e, _ := s.Registry.Get("ws-1"); return e.Status == registry.Offline })

	_, err = client(ts.URL, "alice-tok", registry.Node{ID: "ws-1", Repos: []string{"o/r"}, Slots: 1}).Connect(ctx(t))
	if err == nil || errors.Is(err, ErrRefused) {
		t.Errorf("connecting to a hub that is shutting down: %v, want the handshake to fail (503)", err)
	}
}

func TestGrainfileSubscriptionsNotificationsAndCompletions(t *testing.T) {
	gfStore, err := grainfiles.Open(ctx(t), "sqlite:"+filepath.Join(t.TempDir(), "gf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gfStore.Close() })
	s, ts := hub(t)
	t.Cleanup(func() { _ = s.Shutdown(context.Background(), "test done") })
	s.Grainfiles = gfStore

	// 1. Backward compatibility: a node that omits subscriptions connects normally.
	plainConn, err := client(ts.URL, "alice-tok", registry.Node{ID: "ws-plain", Repos: []string{"acme/app"}, Slots: 1}).Connect(ctx(t))
	if err != nil {
		t.Fatalf("plain node connect: %v", err)
	}
	defer plainConn.Close()
	if plainConn.Subscriptions() != nil {
		t.Errorf("expected nil Subscriptions for plain node, got %+v", plainConn.Subscriptions())
	}

	// 2. Node B connects declaring its recipient deployment URL in hello.
	clB := client(ts.URL, "alice-tok", registry.Node{
		ID:         "ws-b",
		Repos:      []string{"acme/grain-b"},
		Slots:      2,
		Features:   []string{registry.FeatureGrainfiles},
		Recipients: []string{"https://github.com/acme/grain-b"},
	})
	connB, err := clB.Connect(ctx(t))
	if err != nil {
		t.Fatalf("ws-b connect: %v", err)
	}
	defer connB.Close()
	if sub := connB.Subscriptions(); sub == nil || len(sub.Recipients) != 1 || sub.Recipients[0] != "https://github.com/acme/grain-b" {
		t.Fatalf("welcome subscriptions = %+v", sub)
	}

	// 3. Index a .task.md targeting Node B and notify: Node B receives task_ready live.
	taskB, err := grainfiles.Parse("tasks/build.task.md", []byte("---\ntitle: Build artifact\nrecipient: https://github.com/acme/grain-b\n---\nBuild.\n"))
	if err != nil {
		t.Fatal(err)
	}
	taskB.Repo = "https://github.com/acme/repo-a"
	taskB.Ref = "main"
	taskB.Commit = "sha-1"
	recB, err := gfStore.IndexGrainfile(ctx(t), taskB)
	if err != nil {
		t.Fatal(err)
	}
	s.NotifyGrainfiles()

	select {
	case ev := <-connB.GrainfileEvents():
		if ev.Type != grainfiles.EventTaskReady || ev.GrainfileURL != recB.URL {
			t.Fatalf("unexpected live event: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for live task_ready event on ws-b")
	}

	// 4. Disconnect Node B, index a .process.md and trigger it while Node B is offline,
	// then reconnect Node B and verify cursor-based catch-up delivers only missed events.
	connB.Close()
	eventually(t, "ws-b offline", func() bool {
		e, _ := s.Registry.Get("ws-b")
		return e.Status == registry.Offline
	})

	procB, err := grainfiles.Parse("processes/audit.process.md", []byte("---\ntitle: Security audit\nrecipient: https://github.com/acme/grain-b\non:\n  push:\n    branches: [\"main\"]\n    paths: [\"src/**\"]\n---\nAudit.\n"))
	if err != nil {
		t.Fatal(err)
	}
	procB.Repo = "https://github.com/acme/repo-a"
	procB.Ref = "main"
	procB.Commit = "sha-2"
	if _, err := gfStore.IndexGrainfile(ctx(t), procB); err != nil {
		t.Fatal(err)
	}
	if _, err := gfStore.EvaluateTriggers(ctx(t), "https://github.com/acme/repo-a", "sha-2", grainfiles.ForgeEvent{
		Source:       grainfiles.TriggerPush,
		Branch:       "main",
		ChangedPaths: []string{"src/app.go"},
	}); err != nil {
		t.Fatal(err)
	}

	connB2, err := clB.Connect(ctx(t))
	if err != nil {
		t.Fatalf("ws-b reconnect: %v", err)
	}
	defer connB2.Close()

	var caughtUp []string
	for range 2 {
		select {
		case ev := <-connB2.GrainfileEvents():
			caughtUp = append(caughtUp, ev.Type)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for reconnect catchup events, got %v", caughtUp)
		}
	}
	if len(caughtUp) != 2 || caughtUp[0] != grainfiles.EventProcessUpdated || caughtUp[1] != grainfiles.EventProcessTriggered {
		t.Fatalf("expected [process_updated, process_triggered] on reconnect catchup, got %v", caughtUp)
	}

	// 5. Node C connects and subscribes via conn.Subscribe to recipient grain-c,
	// which has a task depending on taskB (depends_on). When Node B reports completion
	// of taskB via CompleteGrainfile, Node C receives dependency_completed live.
	taskC, err := grainfiles.Parse("tasks/deploy.task.md", []byte("---\ntitle: Deploy artifact\nrecipient: https://github.com/acme/grain-c\ndepends_on:\n  - https://github.com/acme/repo-a/blob/main/tasks/build.task.md\n---\nDeploy.\n"))
	if err != nil {
		t.Fatal(err)
	}
	taskC.Repo = "https://github.com/acme/repo-c"
	taskC.Ref = "main"
	taskC.Commit = "sha-c1"
	recC, err := gfStore.IndexGrainfile(ctx(t), taskC)
	if err != nil {
		t.Fatal(err)
	}

	connC, err := client(ts.URL, "bob-tok", registry.Node{ID: "ws-c", Slots: 1}).Connect(ctx(t))
	if err != nil {
		t.Fatalf("ws-c connect: %v", err)
	}
	defer connC.Close()

	updatedSub, err := connC.Subscribe(ctx(t), grainfiles.Subscriptions{
		Recipients: []string{"https://github.com/acme/grain-c"},
	})
	if err != nil {
		t.Fatalf("ws-c Subscribe: %v", err)
	}
	if updatedSub.Revision != 1 {
		t.Errorf("updatedSub.Revision = %d, want 1", updatedSub.Revision)
	}

	// Drain the initial task_ready for deploy.task.md delivered after Subscribe.
	select {
	case ev := <-connC.GrainfileEvents():
		if ev.Type != grainfiles.EventTaskReady || ev.GrainfileURL != recC.URL {
			t.Fatalf("expected initial task_ready for ws-c, got %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ws-c initial task_ready")
	}

	// Node B reports completion of build.task.md.
	depEvents, err := connB2.CompleteGrainfile(ctx(t), recB.URL, "sha-1", "done", "merged PR #10")
	if err != nil {
		t.Fatalf("CompleteGrainfile: %v", err)
	}
	if len(depEvents) != 1 || depEvents[0].Type != grainfiles.EventDependencyCompleted {
		t.Fatalf("CompleteGrainfile events = %+v", depEvents)
	}

	// Node C receives dependency_completed live over its rendezvous stream!
	select {
	case ev := <-connC.GrainfileEvents():
		if ev.Type != grainfiles.EventDependencyCompleted || ev.GrainfileURL != recC.URL || ev.DependencyState != "done" {
			t.Fatalf("unexpected dependency_completed event on ws-c: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for live dependency_completed event on ws-c")
	}
}
