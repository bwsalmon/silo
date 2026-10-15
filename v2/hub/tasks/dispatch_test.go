package tasks

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/hub/directory"
	"github.com/bwsalmon/silo/v2/hub/registry"
	"github.com/bwsalmon/silo/v2/hub/rendezvous"
)

// harness is a hub: the team directory, the rendezvous, the backlog and
// a running dispatcher. Its directory has two teams: "infra", owning
// bwsalmon/silo and bwsalmon/grain, with alice; and "web", owning
// bwsalmon/web, with bob.
type harness struct {
	dir   *directory.Store
	store *Store
	rv    *rendezvous.Server
	d     *Dispatcher
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
	check(t, dir.SetMember(ctx, "infra", "alice@example.com", directory.Admin))
	check(t, dir.SetMember(ctx, "web", "bob@example.com", directory.Member))
	check(t, dir.AssignRepo(ctx, "infra", "bwsalmon/silo"))
	check(t, dir.AssignRepo(ctx, "infra", "bwsalmon/grain"))
	check(t, dir.AssignRepo(ctx, "web", "bwsalmon/web"))

	store, err := Open(ctx, "sqlite::memory:")
	check(t, err)
	t.Cleanup(func() { store.Close() })
	store.Now = ticking()

	rv := &rendezvous.Server{
		Registry: registry.New(),
		// The bearer token is the owner's email.
		Authenticate: func(r *http.Request) (rendezvous.Identity, error) {
			tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tok == "" {
				return rendezvous.Identity{}, errors.New("no token")
			}
			return rendezvous.Identity{Owner: tok}, nil
		},
		Admit:     dir.Admit,
		Heartbeat: time.Second,
		Logf:      t.Logf,
	}
	ts := httptest.NewServer(rv.Handler())
	t.Cleanup(ts.Close)

	d := &Dispatcher{
		Store:        store,
		Registry:     rv.Registry,
		Open:         rv.Open,
		Directory:    dir,
		OfferTimeout: 3 * time.Second,
		Retry:        300 * time.Millisecond,
		Logf:         t.Logf,
	}
	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		d.Run(runCtx)
	}()
	// Before the test's servers close: the dispatcher's streams first.
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return &harness{dir: dir, store: store, rv: rv, d: d, url: ts.URL}
}

// submit queues a task as alice.
func (h *harness) submit(t *testing.T, s Spec) Task {
	t.Helper()
	if s.Submitter == "" {
		s.Submitter = "alice@example.com"
	}
	if s.Prompt == "" {
		s.Prompt = "do the thing"
	}
	task, err := h.store.Create(context.Background(), s)
	check(t, err)
	h.d.Wake()
	return task
}

// await waits for task id to satisfy cond, and returns it.
func (h *harness) await(t *testing.T, id, what string, cond func(Task) bool) Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		task, err := h.store.Get(context.Background(), id)
		check(t, err)
		if cond(task) {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; task is %+v", what, task)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func inState(s State) func(Task) bool { return func(t Task) bool { return t.State == s } }

// testNode is a Grain node connected to the harness, which hands the
// test each stream the hub opens, with its first message.
type testNode struct {
	cl      *rendezvous.Client
	conn    *rendezvous.Conn
	streams chan nodeStream
}

type nodeStream struct {
	*Stream
	first Message
}

func (h *harness) node(t *testing.T, owner string, n registry.Node) *testNode {
	t.Helper()
	tn := &testNode{
		cl: &rendezvous.Client{
			Hub:   h.url,
			Token: func(context.Context) (string, error) { return owner, nil },
			Node:  n,
		},
		streams: make(chan nodeStream, 16),
	}
	tn.connect(t)
	t.Cleanup(func() { tn.conn.Close() })
	return tn
}

func (tn *testNode) connect(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := tn.cl.Connect(ctx)
	check(t, err)
	tn.conn = c
	go func() {
		for {
			st, err := c.Accept()
			if err != nil {
				return
			}
			go func() {
				s := NewStream(st)
				m, err := s.Read()
				if err != nil {
					return
				}
				tn.streams <- nodeStream{s, m}
			}()
		}
	}()
}

// next waits for the hub to open a stream to the node.
func (tn *testNode) next(t *testing.T, want string) nodeStream {
	t.Helper()
	select {
	case s := <-tn.streams:
		if s.first.Type != want {
			t.Fatalf("node %s got %+v, want a %s", tn.cl.Node.ID, s.first, want)
		}
		t.Cleanup(func() { s.Close() })
		return s
	case <-time.After(10 * time.Second):
		t.Fatalf("node %s got no %s", tn.cl.Node.ID, want)
	}
	return nodeStream{}
}

// none checks that the hub opens no stream to the node for a while.
func (tn *testNode) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case s := <-tn.streams:
		t.Fatalf("node %s got %+v, want nothing", tn.cl.Node.ID, s.first)
	case <-time.After(d):
	}
}

func (s nodeStream) send(t *testing.T, m Message) {
	t.Helper()
	check(t, s.Write(m))
}

func silo(id string, slots int) registry.Node {
	return registry.Node{ID: id, Repos: []string{"bwsalmon/silo"}, Slots: slots}
}

func TestOfferAcceptDone(t *testing.T) {
	h := newHarness(t)
	n := h.node(t, "alice@example.com", silo("ws-1", 2))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Title: "Fix it", Prompt: "fix the parser"})

	s := n.next(t, TypeOffer)
	if s.first.Task == nil || s.first.Task.ID != task.ID || s.first.Task.Prompt != "fix the parser" || s.first.Task.Repo != "bwsalmon/silo" {
		t.Fatalf("offer %+v", s.first.Task)
	}
	got := h.await(t, task.ID, "offered", inState(Offered))
	if got.Node != "ws-1" || got.Attempts != 1 {
		t.Errorf("offered: %+v", got)
	}
	s.send(t, Message{Type: TypeAccept, NodeTask: "local-1"})
	got = h.await(t, task.ID, "running", inState(Running))
	if got.NodeTask != "local-1" {
		t.Errorf("running: %+v", got)
	}
	s.send(t, Message{Type: TypeResult, State: Done})
	h.await(t, task.ID, "done", inState(Done))
	// The hub closes the stream once the task has ended.
	if _, err := s.Read(); err == nil {
		t.Error("the stream is still open after the result")
	}
}

func TestRefuseRequeuesElsewhere(t *testing.T) {
	h := newHarness(t)
	big := h.node(t, "alice@example.com", silo("ws-big", 3))
	small := h.node(t, "alice@example.com", silo("ws-small", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})

	// The node with the most free slots is offered it first.
	s := big.next(t, TypeOffer)
	s.send(t, Message{Type: TypeRefuse, Reason: "disk full"})
	s2 := small.next(t, TypeOffer)
	got := h.await(t, task.ID, "offered again", inState(Offered))
	if got.Node != "ws-small" || strings.Join(got.Excluded, ",") != "ws-big" || got.Attempts != 2 {
		t.Errorf("re-offered: %+v", got)
	}
	s2.send(t, Message{Type: TypeRefuse, Reason: "no"})
	got = h.await(t, task.ID, "queued with both excluded", func(t Task) bool { return t.State == Queued && len(t.Excluded) == 2 })
	if !strings.Contains(got.Reason, "refused by node ws-small: no") {
		t.Errorf("reason %q", got.Reason)
	}
	// Every node has refused it: it waits.
	big.none(t, time.Second)
	if e, _ := h.rv.Registry.Get("ws-big"); e.Claimed != 0 {
		t.Errorf("ws-big has %d claimed after refusing, want the claim released", e.Claimed)
	}

	// A human requeues it, and it is offered again.
	_, err := h.store.Retry(context.Background(), task.ID)
	check(t, err)
	h.d.Wake()
	big.next(t, TypeOffer).send(t, Message{Type: TypeAccept})
	h.await(t, task.ID, "running", inState(Running))
}

func TestNoAnswerIsARefusal(t *testing.T) {
	h := newHarness(t)
	h.d.OfferTimeout = 500 * time.Millisecond
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	n.next(t, TypeOffer) // and say nothing
	got := h.await(t, task.ID, "requeued", func(t Task) bool { return t.State == Queued && len(t.Excluded) == 1 })
	if !strings.Contains(got.Reason, "no answer") {
		t.Errorf("reason %q", got.Reason)
	}
}

func TestLostWhileOffered(t *testing.T) {
	h := newHarness(t)
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	n.next(t, TypeOffer)
	n.conn.Close()
	got := h.await(t, task.ID, "requeued", inState(Queued))
	if got.Node != "" || len(got.Excluded) != 0 {
		t.Errorf("requeued: %+v; a lost node is not a refusal", got)
	}
	// The node comes back and is offered it again.
	n.connect(t)
	n.next(t, TypeOffer).send(t, Message{Type: TypeAccept})
	got = h.await(t, task.ID, "running", inState(Running))
	if got.Attempts != 2 {
		t.Errorf("attempts %d, want 2", got.Attempts)
	}
}

func TestLostWhileRunning(t *testing.T) {
	h := newHarness(t)
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	other := h.node(t, "alice@example.com", silo("ws-2", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Constraints: Constraints{Node: "ws-1"}})
	n.next(t, TypeOffer).send(t, Message{Type: TypeAccept, NodeTask: "local-9"})
	h.await(t, task.ID, "running", inState(Running))

	n.conn.Close()
	got := h.await(t, task.ID, "lost", inState(Lost))
	if got.Node != "ws-1" {
		t.Errorf("lost: %+v; it still belongs to its node", got)
	}
	// Not placed elsewhere: the node still owns it.
	other.none(t, 500*time.Millisecond)

	// The node comes back and resumes it.
	n.connect(t)
	s := n.next(t, TypeResume)
	if s.first.Task == nil || s.first.Task.ID != task.ID {
		t.Fatalf("resume %+v", s.first)
	}
	s.send(t, Message{Type: TypeAccept})
	got = h.await(t, task.ID, "running again", inState(Running))
	if got.NodeTask != "local-9" {
		t.Errorf("resumed: %+v", got)
	}
	s.send(t, Message{Type: TypeResult, State: Failed, Reason: "tests failed"})
	got = h.await(t, task.ID, "failed", inState(Failed))
	if got.Reason != "tests failed" {
		t.Errorf("failed: %+v", got)
	}
}

func TestResumeRefusedRequeues(t *testing.T) {
	h := newHarness(t)
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	n.next(t, TypeOffer).send(t, Message{Type: TypeAccept})
	h.await(t, task.ID, "running", inState(Running))
	n.conn.Close()
	h.await(t, task.ID, "lost", inState(Lost))

	// The node came back without it: nobody is running it, so it is
	// queued again, for any node, the same one included.
	n.connect(t)
	n.next(t, TypeResume).send(t, Message{Type: TypeRefuse, Reason: "unknown task"})
	n.next(t, TypeOffer).send(t, Message{Type: TypeAccept})
	got := h.await(t, task.ID, "running", inState(Running))
	if len(got.Excluded) != 0 || got.Attempts != 2 {
		t.Errorf("re-run: %+v", got)
	}
}

func TestCancelDownTheStream(t *testing.T) {
	h := newHarness(t)
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	s := n.next(t, TypeOffer)
	s.send(t, Message{Type: TypeAccept})
	h.await(t, task.ID, "running", inState(Running))

	got, err := h.d.Cancel(context.Background(), task.ID, "not wanted")
	check(t, err)
	if got.State != Cancelled {
		t.Errorf("cancelled: %+v", got)
	}
	m, err := s.Read()
	check(t, err)
	if m.Type != TypeCancel {
		t.Fatalf("node read %+v, want cancel", m)
	}
	h.await(t, task.ID, "the cancel delivered", func(t Task) bool { return !t.CancelPending })
	s.send(t, Message{Type: TypeResult, State: Failed, Reason: "killed"})
	if _, err := s.Read(); err == nil {
		t.Error("the stream is still open after the result")
	}
	if got := h.await(t, task.ID, "still cancelled", inState(Cancelled)); got.Reason != "not wanted" {
		t.Errorf("reason %q: the node's late result changed it", got.Reason)
	}
	if _, err := h.d.Cancel(context.Background(), task.ID, ""); !errors.Is(err, ErrState) {
		t.Errorf("cancelling twice: %v, want ErrState", err)
	}
}

func TestCancelWhileLost(t *testing.T) {
	h := newHarness(t)
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	n.next(t, TypeOffer).send(t, Message{Type: TypeAccept})
	h.await(t, task.ID, "running", inState(Running))
	n.conn.Close()
	h.await(t, task.ID, "lost", inState(Lost))

	_, err := h.d.Cancel(context.Background(), task.ID, "")
	check(t, err)
	// The node is told when it comes back, not asked to resume.
	n.connect(t)
	s := n.next(t, TypeCancel)
	if s.first.Task == nil || s.first.Task.ID != task.ID {
		t.Errorf("cancel %+v", s.first)
	}
	h.await(t, task.ID, "the cancel delivered", func(t Task) bool { return t.State == Cancelled && !t.CancelPending })
	n.none(t, 500*time.Millisecond)
}

func TestTeamTargetAndConstraints(t *testing.T) {
	h := newHarness(t)
	web := h.node(t, "bob@example.com", registry.Node{ID: "ws-web", Repos: []string{"bwsalmon/web"}, Slots: 9})
	us := h.node(t, "alice@example.com", registry.Node{ID: "ws-us", Repos: []string{"bwsalmon/grain"}, Slots: 5, Labels: map[string]string{"zone": "us"}})
	eu := h.node(t, "alice@example.com", registry.Node{ID: "ws-eu", Repos: []string{"bwsalmon/grain"}, Slots: 1, Labels: map[string]string{"zone": "eu"}})
	task := h.submit(t, Spec{Team: "infra", Constraints: Constraints{Labels: map[string]string{"zone": "eu"}}})

	s := eu.next(t, TypeOffer)
	if s.first.Task.Team != "infra" || s.first.Task.Repo != "" {
		t.Errorf("offer %+v", s.first.Task)
	}
	s.send(t, Message{Type: TypeAccept})
	h.await(t, task.ID, "running", inState(Running))
	web.none(t, 300*time.Millisecond)
	us.none(t, 0)

	// A team with no node serving it waits.
	ctx := context.Background()
	check(t, h.dir.CreateTeam(ctx, "empty", ""))
	idle := h.submit(t, Spec{Team: "empty"})
	time.Sleep(300 * time.Millisecond)
	if got, _ := h.store.Get(ctx, idle.ID); got.State != Queued {
		t.Errorf("a task for a team nothing serves: %+v", got)
	}
}

func TestAdmissionCheckedAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	check(t, h.dir.SetMember(ctx, "infra", "carol@example.com", directory.Member))
	n := h.node(t, "carol@example.com", silo("ws-carol", 1))
	// Carol leaves the team after her node connected.
	check(t, h.dir.RemoveMember(ctx, "infra", "carol@example.com"))
	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	n.none(t, time.Second)
	if got, _ := h.store.Get(ctx, task.ID); got.State != Queued {
		t.Errorf("task %+v, want it left queued", got)
	}
	// She rejoins: the node is offered the task once its hold is over.
	check(t, h.dir.SetMember(ctx, "infra", "carol@example.com", directory.Member))
	n.next(t, TypeOffer)
}

func TestRecoverOnStart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// A store as a hub that stopped mid-dispatch left it.
	store, err := Open(ctx, "sqlite::memory:")
	check(t, err)
	defer store.Close()
	offered, err := store.Create(ctx, spec("infra", "bwsalmon/silo", "offered"))
	check(t, err)
	_, err = store.Offer(ctx, offered.ID, "ws-1")
	check(t, err)
	running, err := store.Create(ctx, spec("infra", "bwsalmon/silo", "running"))
	check(t, err)
	_, err = store.Offer(ctx, running.ID, "ws-1")
	check(t, err)
	_, err = store.Accept(ctx, running.ID, "")
	check(t, err)

	d := &Dispatcher{Store: store, Registry: registry.New(), Open: h.rv.Open, Logf: t.Logf}
	runCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- d.Run(runCtx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, _ := store.Get(ctx, offered.ID)
		b, _ := store.Get(ctx, running.ID)
		if a.State == Queued && b.State == Lost {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after starting: %s and %s, want queued and lost", a.State, b.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v", err)
	}
}
