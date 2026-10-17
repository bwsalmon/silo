package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/events"
)

func TestProposals(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		bob := spec("infra", "bwsalmon/silo", "an idea")
		bob.Submitter = "bob@example.com"
		// A proposal cannot arrive approved.
		bob.ApprovedBy = "alice@example.com"
		p, err := s.Propose(ctx, bob)
		check(t, err)
		if p.State != Proposed || p.ApprovedBy != "" || p.ApprovedAt != nil {
			t.Fatalf("proposed %+v", p)
		}
		if ts, err := s.Dispatchable(ctx); err != nil || len(ts) != 0 {
			t.Fatalf("dispatchable: %v %v, want no proposal", ts, err)
		}
		if _, err := s.Approve(ctx, p.ID, "Bob@Example.com"); !errors.Is(err, ErrInvalid) {
			t.Errorf("approved by its submitter: %v, want ErrInvalid", err)
		}
		got, err := s.Approve(ctx, p.ID, "Alice@Example.com")
		check(t, err)
		if got.State != Queued || got.ApprovedBy != "alice@example.com" || got.ApprovedAt == nil {
			t.Fatalf("approved %+v", got)
		}
		read, err := s.Get(ctx, p.ID)
		check(t, err)
		if read.ApprovedBy != "alice@example.com" || read.ApprovedAt == nil || !read.ApprovedAt.Equal(*got.ApprovedAt) {
			t.Errorf("read back %+v", read)
		}
		if ts, err := s.Dispatchable(ctx); err != nil || len(ts) != 1 || ts[0].ID != p.ID {
			t.Errorf("dispatchable once approved: %v %v", ts, err)
		}
		// Approved once: not again, and not rejected after.
		if _, err := s.Approve(ctx, p.ID, "dave@example.com"); !errors.Is(err, ErrState) {
			t.Errorf("approving twice: %v, want ErrState", err)
		}
		if _, err := s.Reject(ctx, p.ID, "no"); !errors.Is(err, ErrState) {
			t.Errorf("rejecting an approved task: %v, want ErrState", err)
		}
		// Offered, the approver is still the task's claim.
		offered, err := s.Offer(ctx, p.ID, "ws-1", "alice@example.com")
		check(t, err)
		if offered.ApprovedBy != "alice@example.com" || offered.ApprovedAt == nil {
			t.Errorf("offered %+v", offered)
		}

		r, err := s.Propose(ctx, bob)
		check(t, err)
		rej, err := s.Reject(ctx, r.ID, "rejected by alice@example.com: not now")
		check(t, err)
		if rej.State != Rejected || !rej.State.Terminal() || rej.ApprovedBy != "" {
			t.Errorf("rejected %+v", rej)
		}
		if _, err := s.Approve(ctx, r.ID, "alice@example.com"); !errors.Is(err, ErrState) {
			t.Errorf("approving a rejected task: %v, want ErrState", err)
		}

		w, err := s.Propose(ctx, bob)
		check(t, err)
		if c, err := s.Cancel(ctx, w.ID, "withdrawn"); err != nil || c.State != Cancelled || c.CancelPending {
			t.Errorf("withdrawn: %+v %v", c, err)
		}

		// The proposer lists their proposals with their teams' tasks.
		_, err = s.Create(ctx, spec("web", "bwsalmon/web", "web's"))
		check(t, err)
		ts, err := s.List(ctx, Filter{Teams: []string{"web"}, Submitter: "bob@example.com"})
		check(t, err)
		if len(ts) != 4 {
			t.Errorf("web's and bob's: %d tasks, want 4", len(ts))
		}
		ts, err = s.List(ctx, Filter{Teams: []string{}, Submitter: "bob@example.com", States: []State{Proposed, Rejected}})
		check(t, err)
		if len(ts) != 1 || ts[0].ID != r.ID {
			t.Errorf("bob's proposed or rejected: %v", ts)
		}
		counts, err := s.Counts(ctx)
		check(t, err)
		if counts["infra"][Offered] != 1 || counts["infra"][Proposed] != 0 {
			t.Errorf("counts %v", counts)
		}
	})
}

func TestAnswers(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		a, err := s.Answer(ctx, Answer{Task: "t-1", Request: "r-1", Decision: Allow, AnsweredBy: "alice@example.com"})
		check(t, err)
		if a.Delivered || a.AnsweredAt.IsZero() {
			t.Errorf("answer %+v", a)
		}
		if _, err := s.Answer(ctx, Answer{Task: "t-1", Request: "r-1", Decision: Deny, AnsweredBy: "dave@example.com"}); !errors.Is(err, ErrState) {
			t.Errorf("answering twice: %v, want ErrState", err)
		}
		for _, bad := range []Answer{
			{Task: "t-1", Request: "r-2", Decision: "maybe", AnsweredBy: "alice@example.com"},
			{Task: "t-1", Request: "", Decision: Allow, AnsweredBy: "alice@example.com"},
			{Task: "t-1", Request: "r-2", Decision: Allow},
		} {
			if _, err := s.Answer(ctx, bad); !errors.Is(err, ErrInvalid) {
				t.Errorf("answer %+v: %v, want ErrInvalid", bad, err)
			}
		}
		_, err = s.Answer(ctx, Answer{Task: "t-1", Request: "r-2", Decision: Deny, AnsweredBy: "dave@example.com", Reason: "too broad"})
		check(t, err)
		check(t, s.AnswerDelivered(ctx, "t-1", "r-1"))
		un, err := s.Answers(ctx, "t-1", true)
		check(t, err)
		if len(un) != 1 || un[0].Request != "r-2" || un[0].Decision != Deny || un[0].Reason != "too broad" {
			t.Errorf("undelivered %+v", un)
		}
		all, err := s.Answers(ctx, "t-1", false)
		check(t, err)
		if len(all) != 2 || !all[0].Delivered || all[0].AnsweredBy != "alice@example.com" {
			t.Errorf("all %+v", all)
		}
	})
}

func TestAPIProposals(t *testing.T) {
	h := newAPIHub(t)
	// bob, in web, proposes in infra's repository.
	p := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "an idea"})
	if p.State != Proposed {
		t.Fatalf("filed %+v", p)
	}
	// He sees it, and so does infra; nobody else does.
	if ts := h.list(t, "bob@example.com", ""); len(ts) != 1 || ts[0].ID != p.ID {
		t.Errorf("bob lists %v", ts)
	}
	if ts := h.list(t, "dave@example.com", "?state=proposed"); len(ts) != 1 || ts[0].ID != p.ID {
		t.Errorf("dave lists %v", ts)
	}
	if code, _ := h.do(t, "carol@example.com", "GET", TasksPath+"/"+p.ID, nil); code != http.StatusNotFound {
		t.Errorf("carol reading it: %d, want 404", code)
	}
	// A proposal to a team outside one's own is not filed.
	if code, _ := h.do(t, "bob@example.com", "POST", TasksPath, map[string]any{"team": "infra", "prompt": "x"}); code != http.StatusNotFound {
		t.Errorf("proposing to a team: %d, want 404", code)
	}

	approve := TasksPath + "/" + p.ID + "/approve"
	for _, c := range []struct {
		user string
		want int
	}{
		{"", http.StatusUnauthorized},
		{"carol@example.com", http.StatusNotFound},
		// Its submitter, and a hub admin outside the team.
		{"bob@example.com", http.StatusForbidden},
		{"root@example.com", http.StatusForbidden},
	} {
		if code, b := h.do(t, c.user, "POST", approve, nil); code != c.want {
			t.Errorf("%q approving: %d %s, want %d", c.user, code, b, c.want)
		}
	}
	code, b := h.do(t, "dave@example.com", "POST", approve, nil)
	var got Task
	if code != http.StatusOK || json.Unmarshal(b, &got) != nil || got.State != Queued || got.ApprovedBy != "dave@example.com" || got.ApprovedAt == nil {
		t.Fatalf("dave approving: %d %s", code, b)
	}
	// Once only.
	if code, _ := h.do(t, "alice@example.com", "POST", approve, nil); code != http.StatusConflict {
		t.Errorf("approving again: %d, want 409", code)
	}

	r := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "another"})
	reject := TasksPath + "/" + r.ID + "/reject"
	if code, _ := h.do(t, "bob@example.com", "POST", reject, map[string]string{"reason": "x"}); code != http.StatusForbidden {
		t.Errorf("bob rejecting his own: %d, want 403", code)
	}
	if code, _ := h.do(t, "alice@example.com", "POST", reject, map[string]string{"reason": strings.Repeat("x", maxReason+1)}); code != http.StatusBadRequest {
		t.Errorf("a long reason: %d, want 400", code)
	}
	code, b = h.do(t, "alice@example.com", "POST", reject, map[string]string{"reason": "not now"})
	if code != http.StatusOK || json.Unmarshal(b, &got) != nil || got.State != Rejected || got.Reason != "rejected by alice@example.com: not now" {
		t.Fatalf("alice rejecting: %d %s", code, b)
	}
	if code, _ := h.do(t, "dave@example.com", "POST", TasksPath+"/"+r.ID+"/approve", nil); code != http.StatusConflict {
		t.Errorf("approving a rejected task: %d, want 409", code)
	}
	// A proposer withdraws their own proposal.
	w := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "a third"})
	if code, b := h.do(t, "bob@example.com", "POST", TasksPath+"/"+w.ID+"/cancel", map[string]string{"reason": "never mind"}); code != http.StatusOK || !strings.Contains(string(b), `"state":"cancelled"`) {
		t.Errorf("bob withdrawing: %d %s", code, b)
	}
	// A member's own task is queued, approved by nobody.
	if q := h.submit(t, "dave@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "mine"}); q.State != Queued || q.ApprovedBy != "" {
		t.Errorf("dave's task %+v", q)
	}
	if code, _ := h.do(t, "alice@example.com", "POST", TasksPath+"/"+w.ID+"/approve", nil); code != http.StatusConflict {
		t.Errorf("approving a queued task: %d, want 409", code)
	}
}

// requests is a task's approval requests, by task and ID.
type requests map[string]map[string]events.ApprovalRequest

func (r requests) ApprovalRequest(_ context.Context, task, id string) (events.ApprovalRequest, bool, error) {
	a, ok := r[task][id]
	return a, ok, nil
}

func TestAPIAnswer(t *testing.T) {
	h := newAPIHub(t)
	ctx := context.Background()
	// bob proposes, dave approves, and a node runs it.
	p := h.submit(t, "bob@example.com", map[string]any{"repo": "bwsalmon/silo", "prompt": "an idea"})
	_, err := h.store.Approve(ctx, p.ID, "dave@example.com")
	check(t, err)
	_, err = h.store.Offer(ctx, p.ID, "ws-1", "alice@example.com")
	check(t, err)
	_, err = h.store.Accept(ctx, p.ID, "local-1")
	check(t, err)
	past := time.Now().Add(-time.Minute)
	h.api.Requests = requests{p.ID: {
		"r-1":   {ID: "r-1", Tool: "deploy"},
		"r-2":   {ID: "r-2", Tool: "deploy"},
		"r-old": {ID: "r-old", Tool: "deploy", Deadline: &past},
	}}
	path := func(id string) string { return TasksPath + "/" + p.ID + "/approvals/" + id }
	allow := map[string]string{"decision": "allow"}
	for _, c := range []struct {
		user, id string
		body any
		want     int
	}{
		{"", "r-1", allow, http.StatusUnauthorized},
		{"carol@example.com", "r-1", allow, http.StatusNotFound},
		// Its submitter sees the task but is not in its team.
		{"bob@example.com", "r-1", allow, http.StatusForbidden},
		{"root@example.com", "r-1", allow, http.StatusForbidden},
		{"dave@example.com", "r-1", map[string]string{"decision": "maybe"}, http.StatusBadRequest},
		{"dave@example.com", "r-1", map[string]string{}, http.StatusBadRequest},
		// A request the node never sent, and one past its deadline.
		{"dave@example.com", "r-nope", allow, http.StatusNotFound},
		{"dave@example.com", "r-old", allow, http.StatusConflict},
	} {
		if code, b := h.do(t, c.user, "POST", path(c.id), c.body); code != c.want {
			t.Errorf("%q answering %s with %v: %d %s, want %d", c.user, c.id, c.body, code, b, c.want)
		}
	}
	code, b := h.do(t, "dave@example.com", "POST", path("r-1"), map[string]string{"decision": "deny", "reason": "too broad"})
	var a Answer
	if code != http.StatusOK || json.Unmarshal(b, &a) != nil || a.AnsweredBy != "dave@example.com" || a.Decision != Deny || a.Reason != "too broad" {
		t.Fatalf("dave answering: %d %s", code, b)
	}
	// Once only: the request is stale once answered.
	if code, _ := h.do(t, "alice@example.com", "POST", path("r-1"), allow); code != http.StatusConflict {
		t.Errorf("answering again: %d, want 409", code)
	}
	// Who answered is there for anyone who sees the task.
	code, b = h.do(t, "bob@example.com", "GET", TasksPath+"/"+p.ID+"/approvals", nil)
	var list struct{ Approvals []Answer }
	if code != http.StatusOK || json.Unmarshal(b, &list) != nil || len(list.Approvals) != 1 || list.Approvals[0].AnsweredBy != "dave@example.com" {
		t.Errorf("listing answers: %d %s", code, b)
	}
	// A task that is no longer running answers nothing.
	_, err = h.store.Finish(ctx, p.ID, Done, "")
	check(t, err)
	if code, _ := h.do(t, "alice@example.com", "POST", path("r-2"), allow); code != http.StatusConflict {
		t.Errorf("answering a finished task's request: %d, want 409", code)
	}
	// Without the event store, nothing is answered.
	h.api.Requests = nil
	if code, _ := h.do(t, "alice@example.com", "POST", path("r-2"), allow); code != http.StatusNotFound {
		t.Errorf("answering with no event store: %d, want 404", code)
	}
}

// TestApproveDownTheStream answers a running task's approval request
// and checks the answer reaches its node over the rendezvous, on the
// task's stream, as cancel does; and that an answer kept while the node
// had not yet accepted the task reaches it once it does.
func TestApproveDownTheStream(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	n := h.node(t, "alice@example.com", silo("ws-1", 2))

	early := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	s1 := n.next(t, TypeOffer)
	h.await(t, early.ID, "offered", inState(Offered))
	_, err := h.store.Answer(ctx, Answer{Task: early.ID, Request: "r-0", Decision: Allow, AnsweredBy: "dave@example.com"})
	check(t, err)
	h.d.Answered(ctx, early.ID)
	s1.send(t, Message{Type: TypeAccept, NodeTask: "local-0"})
	m, err := s1.Read()
	check(t, err)
	if m.Type != TypeApprove || m.Request != "r-0" || m.Decision != Allow || m.AnsweredBy != "dave@example.com" || m.Task == nil || m.Task.ID != early.ID {
		t.Fatalf("node read %+v, want r-0's approve", m)
	}

	task := h.submit(t, Spec{Team: "infra", Repo: "bwsalmon/silo"})
	s := n.next(t, TypeOffer)
	s.send(t, Message{Type: TypeAccept, NodeTask: "local-1"})
	h.await(t, task.ID, "running", inState(Running))
	_, err = h.store.Answer(ctx, Answer{Task: task.ID, Request: "r-1", Decision: Deny, AnsweredBy: "alice@example.com", Reason: "no"})
	check(t, err)
	h.d.Answered(ctx, task.ID)
	m, err = s.Read()
	check(t, err)
	if m.Type != TypeApprove || m.Request != "r-1" || m.Decision != Deny || m.AnsweredBy != "alice@example.com" || m.Reason != "no" || m.Task.ID != task.ID {
		t.Fatalf("node read %+v, want r-1's approve", m)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		as, err := h.store.Answers(ctx, task.ID, true)
		check(t, err)
		if len(as) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer is still undelivered: %+v", as)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The task's stream still carries its result.
	s.send(t, Message{Type: TypeResult, State: Done})
	h.await(t, task.ID, "done", inState(Done))
}
