package live

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
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/bwsalmon/silo/v2/hub/directory"
	"github.com/bwsalmon/silo/v2/hub/identity"
	"github.com/bwsalmon/silo/v2/hub/registry"
	"github.com/bwsalmon/silo/v2/hub/rendezvous"
	"github.com/bwsalmon/silo/v2/hub/tasks"
	"github.com/bwsalmon/silo/v2/internal/tunnel/mux"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// headerAuth is a test sign-on: X-User is the email.
type headerAuth struct{}

func (headerAuth) Principal(r *http.Request) (identity.Principal, error) {
	u := r.Header.Get("X-User")
	if u == "" {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return identity.Principal{Email: u}, nil
}

// fakeTasks is a backlog whose tasks the test places by hand.
type fakeTasks struct {
	mu sync.Mutex
	m  map[string]tasks.Task
}

func (f *fakeTasks) Get(_ context.Context, id string) (tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.m[id]
	if !ok {
		return tasks.Task{}, fmt.Errorf("%w: %s", tasks.ErrNotFound, id)
	}
	return t, nil
}

func (f *fakeTasks) put(t tasks.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[t.ID] = t
}

// recordingLog is the event store, remembering every Append.
type recordingLog struct {
	*Store
	mu       sync.Mutex
	appended []Event
	classes  []Class
}

func (l *recordingLog) Append(ctx context.Context, e Event, c Class) error {
	l.mu.Lock()
	l.appended = append(l.appended, e)
	l.classes = append(l.classes, c)
	l.mu.Unlock()
	return l.Store.Append(ctx, e, c)
}

// harness is a hub's event path: the directory, where alice is in
// infra, which owns bwsalmon/silo, and bob in web; a backlog with
// task t-1 running on node ws-1; the event store; and the rendezvous
// that ws-1 connects to.
type harness struct {
	hub   *Hub
	log   *recordingLog
	tasks *fakeTasks
	rvURL string
	url   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	dir, err := directory.Open(ctx, "sqlite::memory:")
	check(t, err)
	t.Cleanup(func() { dir.Close() })
	check(t, dir.CreateTeam(ctx, "infra", ""))
	check(t, dir.CreateTeam(ctx, "web", ""))
	check(t, dir.SetMember(ctx, "infra", "alice@example.com", directory.Member))
	check(t, dir.SetMember(ctx, "web", "bob@example.com", directory.Member))
	check(t, dir.AssignRepo(ctx, "infra", "bwsalmon/silo"))

	store, err := Open(ctx, "sqlite::memory:")
	check(t, err)
	t.Cleanup(func() { store.Close() })
	log := &recordingLog{Store: store}

	ft := &fakeTasks{m: map[string]tasks.Task{}}
	ft.put(tasks.Task{Spec: tasks.Spec{ID: "t-1", Team: "infra", Repo: "bwsalmon/silo"}, State: tasks.Running, Node: "ws-1", NodeTask: "local-1"})

	h := &Hub{
		Log:       log,
		Tasks:     ft,
		Directory: &directory.API{Store: dir, Auth: headerAuth{}},
		Logf:      t.Logf,
	}
	rv := &rendezvous.Server{
		Registry: registry.New(),
		Authenticate: func(r *http.Request) (rendezvous.Identity, error) {
			tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tok == "" {
				return rendezvous.Identity{}, errors.New("no token")
			}
			return rendezvous.Identity{Owner: tok}, nil
		},
		NodeStream: h.NodeStream,
		Heartbeat:  time.Second,
		Logf:       t.Logf,
	}
	h.Open = rv.Open
	rvs := httptest.NewServer(rv.Handler())
	t.Cleanup(rvs.Close)
	hs := httptest.NewServer(h.Handler())
	t.Cleanup(hs.Close)
	return &harness{hub: h, log: log, tasks: ft, rvURL: rvs.URL, url: hs.URL}
}

// connect connects node id to the hub, as alice.
func (h *harness) connect(t *testing.T, id string) *rendezvous.Conn {
	t.Helper()
	cl := &rendezvous.Client{
		Hub:   h.rvURL,
		Token: func(context.Context) (string, error) { return "alice@example.com", nil },
		Node:  registry.Node{ID: id, Repos: []string{"bwsalmon/silo"}, Slots: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cl.Connect(ctx)
	check(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}

// upstream opens an event stream on c.
func upstream(t *testing.T, c *rendezvous.Conn) net.Conn {
	t.Helper()
	st, err := c.Open(context.Background())
	check(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

func send(t *testing.T, w io.Writer, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if _, err := io.WriteString(w, l+"\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// waitLast waits for the hub to have task's events up to seq.
func (h *harness) waitLast(t *testing.T, task string, seq int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.hub.mu.Lock()
		tp := h.hub.topics[task]
		h.hub.mu.Unlock()
		if tp != nil {
			tp.mu.Lock()
			last := tp.last
			tp.mu.Unlock()
			if last >= seq {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s's events did not reach seq %d", task, seq)
}

// received is one SSE message.
type received struct {
	id, event, data string
}

// subscribe follows task's events over SSE as user, from from: the
// messages arrive on the channel, which closes when the response ends.
func (h *harness) subscribe(t *testing.T, user, task string, from string, header http.Header) <-chan received {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	url := h.url + "/v1/tasks/" + task + "/events"
	if from != "" {
		url += "?from=" + from
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("X-User", user)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	check(t, err)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("subscribing as %s: %d %s", user, resp.StatusCode, b)
	}
	ch := make(chan received, 100)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var m received
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if m.event != "" {
					ch <- m
				}
				m = received{}
			case strings.HasPrefix(line, "id: "):
				m.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				m.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				m.data = line[6:]
			}
		}
	}()
	return ch
}

// collect reads ch until it closes.
func collect(t *testing.T, ch <-chan received) []received {
	t.Helper()
	var out []received
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, m)
		case <-timeout:
			t.Fatalf("the subscription did not end; got %v", out)
		}
	}
}

// next reads n messages from ch.
func next(t *testing.T, ch <-chan received, n int) []received {
	t.Helper()
	var out []received
	timeout := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatalf("the subscription ended after %v", out)
			}
			out = append(out, m)
		case <-timeout:
			t.Fatalf("waiting for %d messages; got %v", n, out)
		}
	}
	return out
}

func ids(ms []received) string {
	var s []string
	for _, m := range ms {
		if m.event == "end" {
			s = append(s, "end")
			continue
		}
		s = append(s, m.id+":"+m.event)
	}
	return strings.Join(s, " ")
}

func status(seq int, msg string) string {
	return fmt.Sprintf(`{"v":1,"seq":%d,"task":"t-1","type":"status","status":{"phase":"running","state":"info","message":%q}}`, seq, msg)
}

func text(seq int, s string) string {
	return fmt.Sprintf(`{"v":1,"seq":%d,"task":"t-1","type":"transcript","transcript":{"kind":"text","text":%q}}`, seq, s)
}

func result(seq int) string {
	return fmt.Sprintf(`{"v":1,"seq":%d,"task":"t-1","type":"result","result":{"exit":{"code":0,"reason":"completed"},"final_text":"all done"}}`, seq)
}

// secret marks what only a node may hold.
const secret = "SECRET-NODE-ONLY-PAYLOAD"

// TestNodeOnlyNeverStored is docs/hub.md §7.2's promise: whatever a node
// sends, no node-only payload reaches the hub's store, and subscribers
// get only its reference.
func TestNodeOnlyNeverStored(t *testing.T) {
	h := newHarness(t)
	sub := h.subscribe(t, "alice@example.com", "t-1", "", nil)
	st := upstream(t, h.connect(t, "ws-1"))
	send(t, st,
		status(1, "starting"),
		// Gateway calls, transcript tool items, a diff, a file, a
		// version the hub does not read: node-only, whatever they carry.
		`{"v":1,"seq":2,"task":"t-1","type":"tool_call","tool_call":{"phase":"start","call":1,"tool":"write","args":{"content":"`+secret+`"}},"artifact":{"ref":"call-1"}}`,
		`{"v":1,"seq":3,"task":"t-1","type":"tool_call","tool_call":{"phase":"end","call":1,"tool":"write","head":"`+secret+`"}}`,
		`{"v":1,"seq":4,"task":"t-1","type":"transcript","transcript":{"kind":"tool_call","id":"x","tool":"Edit","args":{"new":"`+secret+`"}}}`,
		`{"v":1,"seq":5,"task":"t-1","type":"transcript","transcript":{"kind":"tool_result","id":"x","text":"`+secret+`"}}`,
		`{"v":1,"seq":6,"task":"t-1","type":"artifact","artifact":{"kind":"diff","ref":"diff-1","size":40,"content":"`+secret+`"}}`,
		`{"v":1,"seq":7,"task":"t-1","type":"file_contents","file_contents":{"path":"main.go","text":"`+secret+`"}}`,
		`{"v":2,"seq":8,"task":"t-1","type":"status","status":{"phase":"running","state":"info","message":"`+secret+`"}}`,
		// Stored types smuggling a payload: rejected outright.
		`{"v":1,"seq":9,"task":"t-1","type":"status","status":{"phase":"running","state":"info"},"diff":"`+secret+`"}`,
		`{"v":1,"seq":10,"task":"t-1","type":"status","status":{"phase":"running","state":"info","diff":"`+secret+`"}}`,
		`{"v":1,"seq":11,"task":"t-1","type":"result","result":{"exit":{"code":0,"reason":"completed","stdout":"`+secret+`"}}}`,
		`{"v":1,"seq":12,"task":"t-1","type":"approval_request","approval_request":{"id":"a","tool":"push"},"artifact":{"ref":"`+secret+`"}}`,
		text(13, "thinking aloud"),
		`{"v":1,"seq":14,"task":"t-1","type":"approval_request","approval_request":{"id":"a","tool":"push","message":"push?"}}`,
		result(15),
	)
	got := collect(t, sub)
	if want := "1:status 2:artifact 3:artifact 4:artifact 5:artifact 6:artifact 7:artifact 8:artifact 13:transcript 14:approval_request 15:result end"; ids(got) != want {
		t.Fatalf("subscriber got %s\nwant %s", ids(got), want)
	}
	for _, m := range got {
		if strings.Contains(m.data, secret) {
			t.Errorf("a subscriber got a node-only payload: %s", m.data)
		}
	}
	var refs []string
	for _, m := range got {
		var e Event
		check(t, json.Unmarshal([]byte(m.data), &e))
		if e.Artifact != nil {
			refs = append(refs, e.Artifact.Kind+"="+e.Artifact.Ref)
		}
	}
	if want := "tool_call=call-1 tool_call= tool_call= tool_result= diff=diff-1 file_contents= status="; strings.Join(refs, " ") != want {
		t.Errorf("references %q, want %q", strings.Join(refs, " "), want)
	}

	// What the hub asked its store to keep: lifecycle and conversation.
	h.log.mu.Lock()
	for i, e := range h.log.appended {
		if !h.log.classes[i].Stored() || e.Type == TypeArtifact || e.Artifact != nil || e.Transcript != nil || e.ToolCall != nil {
			t.Errorf("the store was handed %s event %d (%s)", h.log.classes[i], e.Seq, e.Type)
		}
	}
	if len(h.log.appended) != 3 {
		t.Errorf("the store was handed %d events, want 3 (status, approval, result)", len(h.log.appended))
	}
	h.log.mu.Unlock()

	// And what is in it, row by row.
	rows, err := h.log.db.Query(`SELECT seq, type, class, body FROM task_events`)
	check(t, err)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var (
			seq              int64
			typ, class, body string
		)
		check(t, rows.Scan(&seq, &typ, &class, &body))
		n++
		if strings.Contains(body, secret) || typ == TypeArtifact || (class != "lifecycle" && class != "conversation") {
			t.Errorf("stored event %d (%s, %s): %s", seq, typ, class, body)
		}
	}
	check(t, rows.Err())
	if n != 3 {
		t.Errorf("%d stored events, want 3", n)
	}

	// The store itself refuses what is not its to keep.
	if err := h.log.Store.Append(context.Background(), Event{Task: "t-1", Artifact: &Artifact{Kind: "diff"}}, NodeOnly); err == nil {
		t.Error("the store kept a node-only event")
	}
	if err := h.log.Store.Append(context.Background(), Event{Task: "t-1"}, Token); err == nil {
		t.Error("the store kept a token event")
	}
}

// TestCursor is silo attach's semantics: replay what the hub holds after
// the cursor, stored and ring alike, then follow; a node that replays
// its events after reconnecting duplicates none.
func TestCursor(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, "ws-1")
	st := upstream(t, c)
	send(t, st, status(1, "one"), text(2, "two"), status(3, "three"))
	h.waitLast(t, "t-1", 3)

	if got := ids(next(t, h.subscribe(t, "alice@example.com", "t-1", "", nil), 3)); got != "1:status 2:transcript 3:status" {
		t.Fatalf("from the start: %s", got)
	}
	sub := h.subscribe(t, "alice@example.com", "t-1", "1", nil)
	if got := ids(next(t, sub, 2)); got != "2:transcript 3:status" {
		t.Fatalf("from 1: %s", got)
	}
	// An EventSource reconnecting says where it was in Last-Event-ID.
	resumed := h.subscribe(t, "alice@example.com", "t-1", "", http.Header{"Last-Event-Id": {"2"}})
	if got := ids(next(t, resumed, 1)); got != "3:status" {
		t.Fatalf("from Last-Event-ID 2: %s", got)
	}

	// The node reconnects its stream and replays from the start.
	st.Close()
	st2 := upstream(t, c)
	send(t, st2, status(1, "one"), text(2, "two"), status(3, "three"), text(4, "four"), result(5))
	if got := ids(collect(t, sub)); got != "4:transcript 5:result end" {
		t.Fatalf("following: %s", got)
	}
	if got := ids(collect(t, resumed)); got != "4:transcript 5:result end" {
		t.Fatalf("following from Last-Event-ID: %s", got)
	}
	// A finished task replays to its result and ends.
	if got := ids(collect(t, h.subscribe(t, "alice@example.com", "t-1", "3", nil))); got != "4:transcript 5:result end" {
		t.Fatalf("after the result: %s", got)
	}
	if got := ids(collect(t, h.subscribe(t, "alice@example.com", "t-1", "5", nil))); got != "end" {
		t.Fatalf("from the result: %s", got)
	}
}

// TestRingBound: the token stream lives in a bounded ring; the stored
// classes outlast it.
func TestRingBound(t *testing.T) {
	h := newHarness(t)
	h.hub.Ring = 2
	st := upstream(t, h.connect(t, "ws-1"))
	send(t, st, status(1, "one"), text(2, "a"), text(3, "b"), text(4, "c"), status(5, "five"))
	h.waitLast(t, "t-1", 5)
	if got := ids(next(t, h.subscribe(t, "alice@example.com", "t-1", "", nil), 4)); got != "1:status 3:transcript 4:transcript 5:status" {
		t.Fatalf("got %s", got)
	}
}

// TestEndsWithTask: a task that ended without a result event ends its
// subscriptions when its node's stream closes, and later ones at once.
func TestEndsWithTask(t *testing.T) {
	h := newHarness(t)
	st := upstream(t, h.connect(t, "ws-1"))
	send(t, st, status(1, "one"))
	h.waitLast(t, "t-1", 1)
	sub := h.subscribe(t, "alice@example.com", "t-1", "", nil)
	next(t, sub, 1)
	task, _ := h.tasks.Get(context.Background(), "t-1")
	task.State = tasks.Failed
	h.tasks.put(task)
	st.Close()
	if got := ids(collect(t, sub)); got != "end" {
		t.Fatalf("got %s", got)
	}
	if got := ids(collect(t, h.subscribe(t, "alice@example.com", "t-1", "", nil))); got != "1:status end" {
		t.Fatalf("after: %s", got)
	}
}

func get(t *testing.T, url, user string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if user != "" {
		req.Header.Set("X-User", user)
	}
	resp, err := http.DefaultClient.Do(req)
	check(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAccess(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		user, path string
		code       int
	}{
		{"", "/v1/tasks/t-1/events", http.StatusUnauthorized},
		{"bob@example.com", "/v1/tasks/t-1/events", http.StatusNotFound},
		{"alice@example.com", "/v1/tasks/t-nope/events", http.StatusNotFound},
		{"alice@example.com", "/v1/tasks/t-1/events?from=-1", http.StatusBadRequest},
		{"", "/v1/tasks/t-1/artifacts?ref=diff-1", http.StatusUnauthorized},
		{"bob@example.com", "/v1/tasks/t-1/artifacts?ref=diff-1", http.StatusNotFound},
		{"alice@example.com", "/v1/tasks/t-1/artifacts", http.StatusBadRequest},
	} {
		if code, body := get(t, h.url+c.path, c.user); code != c.code {
			t.Errorf("%s GET %s: %d %s, want %d", c.user, c.path, code, body, c.code)
		}
	}
	// Bob joins the team that owns the repository: he sees it.
	check(t, h.hub.Directory.Store.SetMember(context.Background(), "infra", "bob@example.com", directory.Member))
	st := upstream(t, h.connect(t, "ws-1"))
	send(t, st, status(1, "one"))
	h.waitLast(t, "t-1", 1)
	next(t, h.subscribe(t, "bob@example.com", "t-1", "", nil), 1)
}

// TestStreamForAnotherNode: a node streams only the tasks placed on it.
func TestStreamForAnotherNode(t *testing.T) {
	h := newHarness(t)
	h.tasks.put(tasks.Task{Spec: tasks.Spec{ID: "t-2", Team: "infra"}, State: tasks.Running, Node: "ws-2"})
	st := upstream(t, h.connect(t, "ws-1"))
	send(t, st, strings.Replace(status(1, "mine"), "t-1", "t-2", 1))
	// The hub ends the stream.
	st.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := st.Read(make([]byte, 1)); err == nil || errors.Is(err, mux.ErrTimeout) {
		t.Fatalf("the stream stayed open: %v", err)
	}
	if seq, _, _ := h.log.Last(context.Background(), "t-2"); seq != 0 {
		t.Fatalf("kept another node's event: %d", seq)
	}

	// A stream bound to t-1 cannot carry t-2's events either.
	st2 := upstream(t, h.connect(t, "ws-1"))
	send(t, st2, status(1, "mine"), strings.Replace(status(2, "theirs"), "t-1", "t-2", 1), status(3, "late"))
	h.waitLast(t, "t-1", 1)
	st2.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := st2.Read(make([]byte, 1)); err == nil || errors.Is(err, mux.ErrTimeout) {
		t.Fatal("the stream stayed open")
	}
	if seq, _, _ := h.log.Last(context.Background(), "t-1"); seq != 1 {
		t.Fatalf("t-1's last event %d, want 1", seq)
	}
}

func TestWebSocket(t *testing.T) {
	h := newHarness(t)
	st := upstream(t, h.connect(t, "ws-1"))
	send(t, st, status(1, "one"), text(2, "two"))
	h.waitLast(t, "t-1", 2)

	wsURL := "ws" + strings.TrimPrefix(h.url, "http") + "/v1/tasks/t-1/events?from=1"
	dial := func(origin string) (*websocket.Conn, error) {
		cfg, err := websocket.NewConfig(wsURL, origin)
		check(t, err)
		cfg.Header.Set("X-User", "alice@example.com")
		return websocket.DialConfig(cfg)
	}
	if _, err := dial("http://evil.example"); err == nil {
		t.Fatal("a WebSocket from another origin was accepted")
	}
	c, err := dial(h.url)
	check(t, err)
	defer c.Close()
	send(t, st, result(3))
	var got []string
	for {
		var m map[string]any
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if err := websocket.JSON.Receive(c, &m); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		got = append(got, fmt.Sprint(m["seq"], ":", m["type"]))
	}
	if strings.Join(got, " ") != "2:transcript 3:result <nil>:end" {
		t.Fatalf("got %v", got)
	}
}

// TestViewThrough: an artifact is read from its node as it is viewed,
// and the hub keeps none of it.
func TestViewThrough(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, "ws-1")
	const diff = "--- a/main.go\n+++ b/main.go\n" + secret + "\n"
	views := make(chan ViewMessage, 10)
	go func() {
		for {
			s, err := c.Accept()
			if err != nil {
				return
			}
			go func() {
				line, err := readLine(bufio.NewReader(s))
				if err != nil {
					s.Close()
					return
				}
				var m ViewMessage
				json.Unmarshal(line, &m)
				views <- m
				ServeView(s, m, func(m ViewMessage) (io.ReadCloser, error) {
					if m.Ref != "diff-1" {
						return nil, errors.New("no such artifact: " + m.Ref)
					}
					return io.NopCloser(strings.NewReader(diff)), nil
				})
			}()
		}
	}()
	st := upstream(t, c)
	send(t, st, `{"v":1,"seq":1,"task":"t-1","type":"artifact","artifact":{"kind":"diff","ref":"diff-1"}}`)
	h.waitLast(t, "t-1", 1)

	code, body := get(t, h.url+"/v1/tasks/t-1/artifacts?ref=diff-1", "alice@example.com")
	if code != http.StatusOK || body != diff {
		t.Fatalf("viewing diff-1: %d %q", code, body)
	}
	if m := <-views; m.Type != TypeView || m.Task != "t-1" || m.NodeTask != "local-1" || m.Ref != "diff-1" {
		t.Fatalf("the node was asked %+v", m)
	}
	if code, body := get(t, h.url+"/v1/tasks/t-1/artifacts?ref=other", "alice@example.com"); code != http.StatusNotFound || !strings.Contains(body, "no such artifact") {
		t.Fatalf("viewing a ref the node refuses: %d %q", code, body)
	}
	var n int
	check(t, h.log.db.QueryRow(`SELECT COUNT(*) FROM task_events WHERE body LIKE ?`, "%"+secret+"%").Scan(&n))
	if n != 0 {
		t.Fatalf("the store holds the artifact's bytes")
	}

	// The node goes: its artifacts are unavailable.
	h.tasks.put(tasks.Task{Spec: tasks.Spec{ID: "t-3", Team: "infra"}, State: tasks.Lost, Node: "ws-gone"})
	if code, body := get(t, h.url+"/v1/tasks/t-3/artifacts?ref=diff-1", "alice@example.com"); code != http.StatusServiceUnavailable {
		t.Fatalf("viewing on an offline node: %d %q", code, body)
	}
}
