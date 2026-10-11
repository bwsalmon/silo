package fenceapi_test

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// Messages: an IDLE agent starts a turn with one, a RUNNING one takes it
// at its next turn, and GET lists them both ways.
func TestAgentMessages(t *testing.T) {
	e := newAgentEnv(t)
	id := e.create(e.root, "team", "fixer", nil)
	e.runner.Connect(ctx, id)
	path := "/silo/v1/silos/team/agents/fixer"
	state := func() string { return e.ok(e.root, "GET", path, nil)["state"].(string) }
	turns := func() int {
		n := 0
		for _, l := range e.runner.Log() {
			if l == "turn "+id {
				n++
			}
		}
		return n
	}
	// RUNNING on its prompt: a message waits for the next turn.
	m := e.ok(e.team, "POST", path+"/messages", obj{"text": "Also fix the tests."})
	if m["direction"] != "in" || m["delivered"] != false || get(m, "sender", "silo") != e.team {
		t.Errorf("message: %v", m)
	}
	if state() != agents.StateRunning || turns() != 0 {
		t.Errorf("a message to a running agent: %s, %d turns", state(), turns())
	}
	// The turn ends with a message waiting: a new one starts.
	if more, err := e.api.EndTurn(ctx, id); err != nil || !more || state() != agents.StateRunning {
		t.Fatalf("end of turn 1: %v %v %s", more, err, state())
	}
	got, err := e.api.TakeMessages(ctx, id)
	if err != nil || len(got) != 1 || got[0].Text != "Also fix the tests." {
		t.Fatalf("take: %v %v", got, err)
	}
	if again, _ := e.api.TakeMessages(ctx, id); len(again) != 0 {
		t.Errorf("taken twice: %v", again)
	}
	if _, err := e.api.Reply(ctx, id, "Done; both fixed."); err != nil {
		t.Fatal(err)
	}
	if more, err := e.api.EndTurn(ctx, id); err != nil || more || state() != agents.StateIdle {
		t.Fatalf("end of turn 2: %v %v %s", more, err, state())
	}
	// IDLE: a message starts a turn.
	e.ok(e.root, "POST", path+"/messages", obj{"text": "Now the docs."})
	if state() != agents.StateRunning || turns() != 1 {
		t.Errorf("a message to an idle agent: %s, %d turns", state(), turns())
	}
	if a := e.ok(e.root, "GET", path, nil); get(a, "session", "turns") != float64(3) {
		t.Errorf("session: %v", a["session"])
	}
	var dirs []string
	for _, m := range e.ok(e.root, "GET", path+"/messages", nil)["messages"].([]any) {
		dirs = append(dirs, m.(obj)["direction"].(string)+":"+m.(obj)["text"].(string))
	}
	if got := strings.Join(dirs, "|"); got != "in:Also fix the tests.|out:Done; both fixed.|in:Now the docs." {
		t.Errorf("messages: %s", got)
	}
	e.fail(fenceapi.InvalidArgument, e.root, "POST", path+"/messages", obj{"text": " "})
	// validate_only sends nothing.
	e.ok(e.root, "POST", path+"/messages?validate_only=true", obj{"text": "dry"})
	if n := len(e.ok(e.root, "GET", path+"/messages", nil)["messages"].([]any)); n != 3 {
		t.Errorf("after validate_only: %d messages", n)
	}
	if recs := e.records(e.team, fenceapi.KindAgentMessage); len(recs) != 2 || get(recs[1], "fields", "from_state") != agents.StateIdle {
		t.Errorf("agent.message: %v", recs)
	}
	// The session: turns, messages, in order.
	var kinds []string
	for _, ev := range e.ok(e.root, "GET", path+"/session", nil)["events"].([]any) {
		kinds = append(kinds, ev.(obj)["kind"].(string))
	}
	if got := strings.Join(kinds, " "); got != "turn message turn_end turn message turn_end message turn" {
		t.Errorf("events: %s", got)
	}
	page := e.ok(e.root, "GET", path+"/session?after=2&page_size=2", nil)
	if evs := page["events"].([]any); len(evs) != 2 || evs[0].(obj)["seq"] != float64(3) || evs[0].(obj)["kind"] != "turn_end" || page["next_page_token"] == nil {
		t.Errorf("a page: %v", page)
	}
}

// Who may use the control surface: the silo and its ancestors; a
// sibling sees no agent.
func TestControlSurfaceAccess(t *testing.T) {
	e := newAgentEnv(t)
	e.create(e.root, "team", "fixer", nil)
	other := e.mk(e.root, "-"+e.root, "other", nil)
	for _, p := range []string{"/session", "/messages", "/requests"} {
		e.fail(fenceapi.NotFound, other, "GET", "/silo/v1/silos/team/agents/fixer"+p, nil)
		e.ok(e.team, "GET", "/silo/v1/self/agents/fixer"+p, nil)
	}
	e.fail(fenceapi.NotFound, other, "POST", "/silo/v1/silos/team/agents/fixer/messages", obj{"text": "hi"})
	e.fail(fenceapi.InvalidArgument, e.root, "GET", "/silo/v1/silos/team/agents/fixer/requests?state=maybe", nil)
	e.fail(fenceapi.NotFound, e.root, "POST", "/silo/v1/silos/team/agents/fixer/requests/r-none:answer", obj{"text": "x"})
}

// ask: WAITING until answered; the agent's own session may answer none
// of its own; listed by state.
func TestAgentRequests(t *testing.T) {
	e := newAgentEnv(t)
	id := e.create(e.root, "team", "fixer", nil)
	e.runner.Connect(ctx, id)
	harness := fenceapi.Caller{Silo: e.team, Via: fenceapi.ViaHarness, Agent: id}
	r, err := e.api.OpenRequest(ctx, harness, fenceapi.RequestQuestion, "Which branch?")
	if err != nil {
		t.Fatal(err)
	}
	path := "/silo/v1/silos/team/agents/fixer"
	if s := e.ok(e.root, "GET", path, nil)["state"]; s != agents.StateWaiting {
		t.Errorf("asking: %v", s)
	}
	open := e.ok(e.root, "GET", path+"/requests?state=open", nil)["requests"].([]any)
	if len(open) != 1 || open[0].(obj)["id"] != r.ID || open[0].(obj)["kind"] != "question" {
		t.Errorf("open: %v", open)
	}
	answer := path + "/requests/" + r.ID + ":answer"
	e.fail(fenceapi.PermissionDenied, e.team+"/agent:"+id, "POST", answer, obj{"text": "main"})
	e.fail(fenceapi.InvalidArgument, e.team, "POST", answer, obj{"approve": true})
	waited := make(chan *fenceapi.AgentControlRequest)
	go func() {
		got, _ := e.api.WaitAnswer(ctx, id, r.ID)
		waited <- got
	}()
	got := e.ok(e.team, "POST", answer, obj{"text": "main"})
	if got["state"] != "ANSWERED" || get(got, "answer", "text") != "main" || get(got, "answer", "by", "silo") != e.team {
		t.Errorf("answered: %v", got)
	}
	select {
	case w := <-waited:
		if w == nil || w.Answer == nil || w.Answer.Text != "main" {
			t.Errorf("waited: %+v", w)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WaitAnswer didn't return")
	}
	if s := e.ok(e.root, "GET", path, nil)["state"]; s != agents.StateRunning {
		t.Errorf("answered: %v", s)
	}
	e.fail(fenceapi.FailedPrecondition, e.team, "POST", answer, obj{"text": "again"})
	if n := len(e.ok(e.root, "GET", path+"/requests?state=open", nil)["requests"].([]any)); n != 0 {
		t.Errorf("%d open after the answer", n)
	}
	if n := len(e.ok(e.root, "GET", path+"/requests?state=answered", nil)["requests"].([]any)); n != 1 {
		t.Errorf("%d answered", n)
	}
	if recs := e.records(e.team, fenceapi.KindAgentRequest); len(recs) != 1 || recs[0]["via"] != "harness" {
		t.Errorf("agent.request: %v", recs)
	}
	if recs := e.records(e.team, fenceapi.KindAgentAnswer); len(recs) != 1 || recs[0]["via"] != nil {
		t.Errorf("agent.answer: %v", recs)
	}
	// Two open requests: WAITING until both are answered.
	a, _ := e.api.OpenRequest(ctx, harness, fenceapi.RequestApproval, "Force-push?")
	b, _ := e.api.OpenRequest(ctx, harness, fenceapi.RequestQuestion, "Why?")
	e.ok(e.root, "POST", path+"/requests/"+a.ID+":answer", obj{"approve": true})
	if s := e.ok(e.root, "GET", path, nil)["state"]; s != agents.StateWaiting {
		t.Errorf("one of two answered: %v", s)
	}
	e.ok(e.root, "POST", path+"/requests/"+b.ID+":answer", obj{"text": "Because."})
	if s := e.ok(e.root, "GET", path, nil)["state"]; s != agents.StateRunning {
		t.Errorf("both answered: %v", s)
	}
}

// ?follow=true streams the session as server-sent events: what is there,
// then each new event, resuming after Last-Event-ID.
func TestSessionFollow(t *testing.T) {
	e := newAgentEnv(t)
	id := e.create(e.root, "team", "fixer", nil)
	e.api.RecordEvent(ctx, id, fenceapi.EventToolCall, obj{"tool": "exec"})
	e.api.RecordEvent(ctx, id, fenceapi.EventToolResult, obj{"tool": "exec"})
	// Not connected, so no turn yet: these are 1 and 2.
	req, _ := http.NewRequest("GET", e.srv.URL+"/silo/v1/silos/team/agents/fixer/session?follow=true", nil)
	req.Header.Set("Silo-Caller", e.team)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	next := func() (id, kind string) {
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "id: "); ok {
				id = v
			}
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				kind = v
			}
			if line == "" && kind != "" {
				return id, kind
			}
		}
		t.Fatalf("the stream ended: %v", sc.Err())
		return
	}
	if id, kind := next(); id != "2" || kind != "tool_result" {
		t.Errorf("first: %s %s", id, kind)
	}
	go e.api.RecordEvent(ctx, id, fenceapi.EventUsage, obj{"input_tokens": 10})
	if id, kind := next(); id != "3" || kind != "usage" {
		t.Errorf("then: %s %s", id, kind)
	}
	// The agent's deletion ends the stream.
	e.ok(e.root, "DELETE", "/silo/v1/silos/team/agents/fixer", nil)
	for {
		if _, kind := next(); kind == "end" {
			break
		}
	}
}

// Transcripts are kept as long as audit.
func TestSessionRetention(t *testing.T) {
	e := newAgentEnv(t)
	id := e.create(e.root, "team", "fixer", nil)
	e.ok(e.root, "POST", "/silo/v1/silos/team/agents/fixer/messages", obj{"text": "hello"})
	old := store.Millis(time.Now().Add(-fenceapi.AuditRetention - time.Hour))
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		for _, q := range []string{`UPDATE agent_events SET at = ? WHERE seq = 1`, `UPDATE agent_messages SET at = ?`} {
			if _, err := tx.Exec(ctx, q, old); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.api.RecordEvent(ctx, id, fenceapi.EventError, obj{"message": "new"})
	if _, err := fenceapi.PruneAudit(ctx, e.db); err != nil {
		t.Fatal(err)
	}
	evs := e.ok(e.root, "GET", "/silo/v1/silos/team/agents/fixer/session", nil)["events"].([]any)
	if len(evs) != 1 || evs[0].(obj)["kind"] != "error" {
		t.Errorf("after the prune: %v", evs)
	}
	if n := len(e.ok(e.root, "GET", "/silo/v1/silos/team/agents/fixer/messages", nil)["messages"].([]any)); n != 0 {
		t.Errorf("%d messages after the prune", n)
	}
}
