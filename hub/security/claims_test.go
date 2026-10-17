package security_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/claims"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/tasks"
)

// attestation decodes the claims of the attestation the node was
// offered task with. The node verifies the signature (grain's policy);
// here only what the hub claims matters.
func attestation(t *testing.T, n *fakeNode, task string) claims.Claims {
	t.Helper()
	var st *taskStream
	eventually(t, "task "+task+" offered", func() bool { st = n.taskStream(task); return st != nil })
	st.mu.Lock()
	jwt := st.in[0].Attestation
	st.mu.Unlock()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("task %s was offered with attestation %q", task, jwt)
	}
	var c claims.Claims
	b, err := b64.DecodeString(parts[1])
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestApprovedBy: approved_by is set by a real approval only, by a
// member of the task's own team who did not file it, and the node is
// told exactly that in the attestation; no request field, no other
// task's approval and no store call can put it there.
func TestApprovedBy(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	tok, _ := hb.mint(alice, "ws-a")
	n := connectNode(t, hb, tok, "ws-a", repoA)

	// A member's own task: nobody approved it at the hub.
	own := hb.file(alice, map[string]string{"repo": repoA, "prompt": "mine"})
	if c := attestation(t, n, own.ID); c.ApprovedBy != "" || c.Requester != alice || c.Team != teamA || c.Audience != "ws-a" {
		t.Errorf("a member's task attested as %+v", c)
	}

	// An outsider's proposal: unapproved, and not offered, until a
	// member approves it.
	prop := hb.file(pat, map[string]string{"repo": repoA, "prompt": "please"})
	if prop.State != tasks.Proposed || prop.ApprovedBy != "" {
		t.Fatalf("pat's task filed as %+v", prop)
	}
	time.Sleep(2 * time.Second) // more than DispatchRetry
	if n.taskStream(prop.ID) != nil {
		t.Fatal("a proposal was offered before anyone approved it")
	}
	// Not by another team's admin, nor by the hub admin outside the
	// team.
	for _, who := range []string{carol, root} {
		if code, _, _ := hb.do(hb.user(who), "POST", tasks.TasksPath+"/"+prop.ID+"/approve", nil); code == http.StatusOK {
			t.Errorf("%s approved team A's proposal", who)
		}
	}
	var approved tasks.Task
	hb.must(hb.user(dave), "POST", tasks.TasksPath+"/"+prop.ID+"/approve", nil, http.StatusOK, &approved)
	if approved.ApprovedBy != dave || approved.ApprovedAt == nil {
		t.Errorf("approved as %+v", approved)
	}
	if c := attestation(t, n, prop.ID); c.ApprovedBy != dave || c.Requester != pat || c.Team != teamA {
		t.Errorf("an approved proposal attested as %+v", c)
	}

	// pat's next task, in team B's repository, carries no approval of
	// the first's: each task is approved on its own, by its own team.
	next := hb.file(pat, map[string]string{"repo": repoB, "prompt": "again"})
	if next.State != tasks.Proposed || next.ApprovedBy != "" || next.Team != teamB {
		t.Errorf("pat's task for team B filed as %+v", next)
	}
	// Nor can a request carry one, or name its submitter.
	for _, field := range []string{"approved_by", "approved_at", "submitter", "state", "id", "node"} {
		body := map[string]any{"repo": repoB, "prompt": "p", field: dave}
		if field == "approved_at" {
			body[field] = time.Now()
		}
		if code, b, _ := hb.do(hb.user(pat), "POST", tasks.TasksPath, body); code != http.StatusBadRequest {
			t.Errorf("filing with %q: %d %s, want 400", field, code, b)
		}
	}
	// The store refuses one on a proposal however it is asked (4224).
	tk, err := hb.h.Backlog.Propose(context.Background(), tasks.Spec{Team: teamB, Repo: repoB, Prompt: "p", Submitter: pat, ApprovedBy: alice})
	if err != nil || tk.ApprovedBy != "" || tk.ApprovedAt != nil {
		t.Errorf("Propose with an approval: %+v %v", tk, err)
	}

	// A submitter who later joins the team still cannot approve their
	// own proposal.
	late := hb.file(pat, map[string]string{"repo": quietA, "prompt": "mine to approve?"})
	hb.must(hb.user(root), "PUT", "/v1/teams/team-a/members/user:"+pat, map[string]string{"role": "admin"}, http.StatusNoContent, nil)
	if code, _, _ := hb.do(hb.user(pat), "POST", tasks.TasksPath+"/"+late.ID+"/approve", nil); code != http.StatusForbidden {
		t.Errorf("the submitter approving their own proposal: %d, want 403", code)
	}
	if got, _ := hb.task(hb.user(pat), late.ID); got.ApprovedBy != "" || got.State != tasks.Proposed {
		t.Errorf("after the submitter's attempt: %+v", got)
	}
}

// TestSubmitterCannotBeSpoofed: the submitter is the verified IAP
// identity, normalised, whatever else the request says about who sent
// it.
func TestSubmitterCannotBeSpoofed(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	mallory := "mallory@example.com"
	jwt := hb.key.sign(t, "Alice@Example.COM", nil)
	who := func(r *http.Request) {
		r.Header.Set(identity.IAPHeader, jwt)
		// What a proxy in front, or a client, might add.
		r.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:"+mallory)
		r.Header.Set("X-Forwarded-Email", mallory)
		r.Header.Set("X-User", mallory)
		r.Header.Set("X-Forwarded-User", mallory)
	}
	var tk tasks.Task
	hb.must(who, "POST", tasks.TasksPath, map[string]string{"repo": quietA, "prompt": "p"}, http.StatusCreated, &tk)
	if tk.Submitter != alice || tk.State != tasks.Queued {
		t.Errorf("filed as %q (%s), want %q queued", tk.Submitter, tk.State, alice)
	}
	// Without an assertion the headers sign no one in.
	unsigned := func(r *http.Request) { who(r); r.Header.Del(identity.IAPHeader) }
	if code, _, _ := hb.do(unsigned, "POST", tasks.TasksPath, map[string]string{"repo": quietA, "prompt": "p"}); code != http.StatusUnauthorized {
		t.Errorf("filing with only identity headers: %d, want 401", code)
	}
	// An assertion with no email, or an unverified one, is no one.
	for _, mod := range []func(map[string]any){
		func(c map[string]any) { delete(c, "email") },
		func(c map[string]any) { c["email"] = "" },
	} {
		tok := hb.key.sign(t, mallory, mod)
		if code, _, _ := hb.do(func(r *http.Request) { r.Header.Set(identity.IAPHeader, tok) }, "POST", tasks.TasksPath, map[string]string{"repo": quietA, "prompt": "p"}); code != http.StatusUnauthorized {
			t.Errorf("an assertion without an email filing: %d, want 401", code)
		}
	}
}

// fileFrom files a task from node n on its control stream, as an agent
// on its Grain proposing work does (docs/hub-interface.md).
func fileFrom(t *testing.T, n *fakeNode, f tasks.Filing) (tasks.Task, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := n.conn.FileTask(ctx, f)
	if err != nil {
		return tasks.Task{}, err
	}
	var tk tasks.Task
	if err := json.Unmarshal(raw, &tk); err != nil {
		t.Fatal(err)
	}
	return tk, nil
}

// TestAgentFilingLaunders: an agent filing a task through its Grain
// (task 4233, T-TB1-7) gets no approval it did not have. The task is
// filed for the human its run was for, with the Grain recorded beside
// them; an approval of the task it was filed from is never carried
// over; it is proposed unless that human and the Grain's owner are
// both in the team; and only a member of the team other than that
// human approves it.
func TestAgentFilingLaunders(t *testing.T) {
	hb := startHub(t, options{})
	hb.teams()
	tok, _ := hb.mint(alice, "ws-a")
	n := connectNode(t, hb, tok, "ws-a", repoA)

	// pat's proposal, approved by dave, runs on alice's Grain.
	prop := hb.file(pat, map[string]string{"repo": repoA, "prompt": "pat's idea"})
	hb.must(hb.user(dave), "POST", tasks.TasksPath+"/"+prop.ID+"/approve", nil, http.StatusOK, nil)
	eventually(t, "pat's task running", func() bool { got, _ := hb.task(hb.user(dave), prop.ID); return got.State == tasks.Running })

	// Its agent files into alice's own team's repository: proposed, as
	// pat's, however much alice's Grain is team A's.
	inA, err := fileFrom(t, n, tasks.Filing{Repo: repoA, Prompt: "more of pat's idea", OriginTask: prop.ID})
	if err != nil {
		t.Fatal(err)
	}
	if inA.State != tasks.Proposed || inA.Submitter != pat || inA.ApprovedBy != "" || inA.FiledBy != "ws-a" || inA.FiledByOwner != alice || inA.OriginTask != prop.ID {
		t.Errorf("pat's agent's filing into team A: %+v", inA)
	}
	// Into team B: proposed for team B.
	inB, err := fileFrom(t, n, tasks.Filing{Repo: repoB, Prompt: "and for B", OriginTask: prop.ID, NodeTask: "local-wait"})
	if err != nil {
		t.Fatal(err)
	}
	if inB.State != tasks.Proposed || inB.Team != teamB || inB.Submitter != pat || inB.ApprovedBy != "" {
		t.Errorf("pat's agent's filing into team B: %+v", inB)
	}
	// pat may not approve what their agent filed; nor may alice for team B.
	if code, _, _ := hb.do(hb.user(pat), "POST", tasks.TasksPath+"/"+inA.ID+"/approve", nil); code != http.StatusForbidden {
		t.Errorf("pat approving their agent's filing: %d, want 403", code)
	}
	if code, _, _ := hb.do(hb.user(alice), "POST", tasks.TasksPath+"/"+inB.ID+"/approve", nil); code == http.StatusOK {
		t.Error("alice approved team B's proposal")
	}
	var approved tasks.Task
	hb.must(hb.user(dave), "POST", tasks.TasksPath+"/"+inA.ID+"/approve", nil, http.StatusOK, &approved)
	if approved.ApprovedBy != dave {
		t.Errorf("approved as %+v", approved)
	}
	if c := attestation(t, n, inA.ID); c.ApprovedBy != dave || c.Requester != pat {
		t.Errorf("the approved filing attested as %+v", c)
	}

	// The Grain cannot say whose filing it is, nor file from a task it
	// does not run.
	if _, err := fileFrom(t, n, tasks.Filing{Repo: repoA, Prompt: "p", Node: "ws-b"}); err == nil {
		t.Error("a filing naming another node was filed")
	}
	other := hb.file(bob, map[string]string{"repo": repoB, "prompt": "bob's"})
	if _, err := fileFrom(t, n, tasks.Filing{Repo: repoA, Prompt: "p", OriginTask: other.ID}); err == nil {
		t.Error("a filing from another node's task was filed")
	}

	// Team B turns it down; the Grain waiting on it is told so.
	hb.must(hb.user(bob), "POST", tasks.TasksPath+"/"+inB.ID+"/reject", map[string]string{"reason": "no"}, http.StatusOK, nil)
	eventually(t, "dependency_done for "+inB.ID, func() bool {
		st := n.taskStream(inB.ID)
		return st != nil && st.saw(tasks.TypeDependencyDone)
	})
	st := n.taskStream(inB.ID)
	st.mu.Lock()
	m := st.in[0]
	st.mu.Unlock()
	if m.State != tasks.Rejected || m.NodeTask != "local-wait" {
		t.Errorf("dependency_done %+v", m)
	}
}
