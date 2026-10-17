package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
)

// filer gives the harness's rendezvous a Filer, with opts applied.
func (h *harness) filer(opts ...func(*Filer)) *Filer {
	f := &Filer{Store: h.store, Directory: h.dir, Dispatcher: h.d}
	for _, o := range opts {
		o(f)
	}
	h.rv.FileTask = f.File
	return f
}

// file files a task from tn on its control stream.
func (tn *testNode) file(t *testing.T, f Filing) (Task, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := tn.conn.FileTask(ctx, f)
	if err != nil {
		return Task{}, err
	}
	var task Task
	check(t, json.Unmarshal(raw, &task))
	return task, nil
}

// running has tn accept the next offer, for task, and waits for it to
// be running.
func (h *harness) running(t *testing.T, tn *testNode, task Task) nodeStream {
	t.Helper()
	s := tn.next(t, TypeOffer)
	if s.first.Task == nil || s.first.Task.ID != task.ID {
		t.Fatalf("offered %+v, want %s", s.first.Task, task.ID)
	}
	s.send(t, Message{Type: TypeAccept, NodeTask: "local-" + task.ID})
	h.await(t, task.ID, "running", inState(Running))
	return s
}

// An agent's filing is placed as a human's would be, for the human it
// works for: queued for a team that human and the node's owner are both
// in, proposed for any other, with the node recorded as its filer. An
// approval of the task it was filed from is not carried over.
func TestFileProposedOutsideTeam(t *testing.T) {
	h := newHarness(t)
	h.filer()
	ctx := context.Background()
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	origin := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	h.running(t, n, origin)

	// For another team's repository: proposed, on behalf of alice.
	web, err := n.file(t, Filing{Repo: "bwsalmon/web", Title: "Bump the client", Prompt: "bump it", OriginTask: origin.ID})
	check(t, err)
	if web.State != Proposed || web.Team != "web" || web.Submitter != "alice@example.com" ||
		web.FiledBy != "ws-1" || web.FiledByOwner != "alice@example.com" || web.OriginTask != origin.ID || web.ApprovedBy != "" {
		t.Errorf("filed for web: %+v", web)
	}
	// Its submitter may not approve it; a member of web may.
	if _, err := h.store.Approve(ctx, web.ID, "alice@example.com"); !errors.Is(err, ErrInvalid) {
		t.Errorf("alice approving her agent's filing: %v, want ErrInvalid", err)
	}
	if got, err := h.store.Approve(ctx, web.ID, "bob@example.com"); err != nil || got.State != Queued {
		t.Errorf("bob approving: %+v, %v", got, err)
	}

	// For alice's own team: queued, as her own submission would be.
	own, err := n.file(t, Filing{Repo: "bwsalmon/grain", Prompt: "and here", OriginTask: origin.ID})
	check(t, err)
	if own.State != Queued || own.Team != "infra" || own.Submitter != "alice@example.com" {
		t.Errorf("filed for infra: %+v", own)
	}
}

// A task filed from a hub task whose submitter is outside the team is
// proposed, though the Grain's owner is in it: the agent's work is for
// that submitter, and running on a team member's Grain does not make it
// the member's.
func TestFileNoApprovalLaundering(t *testing.T) {
	h := newHarness(t)
	h.filer()
	ctx := context.Background()
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	// Eve, in no team, proposed a task, which alice approved.
	p, err := h.store.Propose(ctx, Spec{Team: "infra", Repo: "bwsalmon/silo", Prompt: "eve's idea", Submitter: "eve@example.com"})
	check(t, err)
	origin, err := h.store.Approve(ctx, p.ID, "alice@example.com")
	check(t, err)
	h.d.Wake()
	h.running(t, n, origin)

	got, err := n.file(t, Filing{Repo: "bwsalmon/grain", Prompt: "more of eve's idea", OriginTask: origin.ID})
	check(t, err)
	if got.State != Proposed || got.Submitter != "eve@example.com" || got.ApprovedBy != "" {
		t.Errorf("filed from eve's task: %+v, want proposed, eve's, unapproved", got)
	}
	// A team-targeted filing for her is refused outright, as hers would be.
	if _, err := n.file(t, Filing{Team: "infra", Prompt: "x", OriginTask: origin.ID}); !errors.Is(err, rendezvous.ErrFileRefused) {
		t.Errorf("team filing for eve: %v, want refused", err)
	}

	// Filed from no hub task, the submitter is the node's owner.
	mine, err := n.file(t, Filing{Repo: "bwsalmon/grain", Prompt: "alice's own"})
	check(t, err)
	if mine.State != Queued || mine.Submitter != "alice@example.com" || mine.OriginTask != "" {
		t.Errorf("filed from no task: %+v", mine)
	}
}

// A filing names only a hub task its own node is running, and only its
// own node.
func TestFileRefusals(t *testing.T) {
	h := newHarness(t)
	h.filer()
	ctx := context.Background()
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	other, err := h.store.Create(ctx, Spec{Team: "web", Repo: "bwsalmon/web", Prompt: "bob's", Submitter: "bob@example.com"})
	check(t, err)
	for name, f := range map[string]Filing{
		"origin not running here": {Repo: "bwsalmon/web", Prompt: "x", OriginTask: other.ID},
		"origin missing":          {Repo: "bwsalmon/web", Prompt: "x", OriginTask: "t-0000000000000000"},
		"another node":            {Repo: "bwsalmon/web", Prompt: "x", Node: "ws-2"},
		"no target":               {Prompt: "x"},
		"unowned repository":      {Repo: "nobody/owns", Prompt: "x"},
		"no prompt":               {Repo: "bwsalmon/web"},
		"dependency unseen":       {Repo: "bwsalmon/web", Prompt: "x", DependsOn: []string{other.ID}},
		"dependency malformed":    {Repo: "bwsalmon/web", Prompt: "x", DependsOn: []string{"../x"}},
	} {
		if task, err := n.file(t, f); !errors.Is(err, rendezvous.ErrFileRefused) {
			t.Errorf("%s: filed %+v, %v; want refused", name, task, err)
		}
	}
	ts, err := h.store.List(ctx, Filter{})
	check(t, err)
	if len(ts) != 1 {
		t.Errorf("%d tasks after the refusals, want 1", len(ts))
	}
}

// Each node may file Burst tasks at once, and one more each Every.
func TestFileRateLimit(t *testing.T) {
	h := newHarness(t)
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	h.filer(func(f *Filer) { f.Burst, f.Every, f.Now = 2, time.Hour, c.Now })
	a := h.node(t, "alice@example.com", silo("ws-1", 0))
	b := h.node(t, "alice@example.com", silo("ws-2", 0))
	f := Filing{Repo: "bwsalmon/web", Prompt: "x"}
	for i := range 2 {
		if _, err := a.file(t, f); err != nil {
			t.Fatalf("filing %d: %v", i, err)
		}
	}
	_, err := a.file(t, f)
	if !errors.Is(err, rendezvous.ErrFileRefused) || !strings.Contains(err.Error(), "too fast") {
		t.Errorf("third filing: %v, want refused as too fast", err)
	}
	// Another node has its own limit.
	if _, err := b.file(t, f); err != nil {
		t.Errorf("another node's filing: %v", err)
	}
	c.advance(time.Hour)
	if _, err := a.file(t, f); err != nil {
		t.Errorf("filing an hour later: %v", err)
	}
	if _, err := a.file(t, f); err == nil {
		t.Error("a second filing an hour later was allowed")
	}
}

// A task is not placed until each of its dependencies is done, and is
// cancelled when one of them ends any other way.
func TestDependencyGating(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	n := h.node(t, "alice@example.com", silo("ws-1", 2))
	first := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Prompt: "first"})
	second := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Prompt: "second", DependsOn: []string{first.ID}})
	s := h.running(t, n, first)
	// A free slot, and the second still waits.
	n.none(t, time.Second)
	if got, _ := h.store.Get(ctx, second.ID); got.State != Queued {
		t.Fatalf("second while first runs: %+v", got)
	}
	s.send(t, Message{Type: TypeResult, State: Done})
	h.await(t, first.ID, "first done", inState(Done))
	h.running(t, n, second)

	// A dependency that fails cancels the task waiting on it.
	third := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Prompt: "third"})
	fourth := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo", Prompt: "fourth", DependsOn: []string{first.ID, third.ID}})
	s = h.running(t, n, third)
	s.send(t, Message{Type: TypeResult, State: Failed, Reason: "tests"})
	got := h.await(t, fourth.ID, "fourth cancelled", inState(Cancelled))
	if got.Reason != "dependency "+third.ID+" is failed" {
		t.Errorf("fourth's reason %q", got.Reason)
	}
	n.none(t, 500*time.Millisecond)
}

// A node that filed a task saying its own task waits on it is told,
// once, when the task ends: when it is next online if it is away then.
func TestDependencyDoneToNode(t *testing.T) {
	h := newHarness(t)
	h.filer()
	ctx := context.Background()
	n := h.node(t, "alice@example.com", silo("ws-1", 0))
	web, err := n.file(t, Filing{Repo: "bwsalmon/web", Prompt: "for web", NodeTask: "local-7"})
	check(t, err)
	if web.State != Proposed {
		t.Fatalf("filed %+v", web)
	}
	ws, err := h.store.Watchers(ctx, web.ID)
	check(t, err)
	if len(ws) != 1 || ws[0].Node != "ws-1" || ws[0].NodeOwner != "alice@example.com" || ws[0].NodeTask != "local-7" {
		t.Fatalf("watchers %+v", ws)
	}
	n.none(t, 500*time.Millisecond)

	// Bob rejects it, while the node is away.
	n.conn.Close()
	h.forget(t, "ws-1")
	_, err = h.store.Reject(ctx, web.ID, "not now")
	check(t, err)
	h.d.Wake()
	n.connect(t)
	s := n.next(t, TypeDependencyDone)
	if s.first.Task == nil || s.first.Task.ID != web.ID || s.first.State != Rejected || s.first.Reason != "not now" || s.first.NodeTask != "local-7" {
		t.Errorf("dependency_done %+v", s.first)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		un, err := h.store.Unnotified(ctx)
		check(t, err)
		if len(un) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still unnotified: %+v", un)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Told once.
	h.d.Wake()
	n.none(t, time.Second)
}

// Only a node registered by the owner that asked is told.
func TestDependencyDoneOnlyToOwner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	task, err := h.store.Create(ctx, Spec{Team: "infra", Repo: "bwsalmon/silo", Prompt: "x", Submitter: "alice@example.com"})
	check(t, err)
	check(t, h.store.Watch(ctx, Watcher{Task: task.ID, Node: "ws-1", NodeOwner: "alice@example.com", NodeTask: "local-1"}))
	_, err = h.d.Cancel(ctx, task.ID, "")
	check(t, err)
	other := h.node(t, "carol@example.com", registry.Node{ID: "ws-1"})
	other.none(t, time.Second)
	other.conn.Close()
	h.forget(t, "ws-1")
	n := h.node(t, "alice@example.com", silo("ws-1", 1))
	if s := n.next(t, TypeDependencyDone); s.first.State != Cancelled || s.first.NodeTask != "local-1" {
		t.Errorf("dependency_done %+v", s.first)
	}
}
