package harness_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/harness"
)

// control is a fake fence API for a Runner.
type control struct {
	mu       sync.Mutex
	order    []string
	sessions map[string]agents.Session
	states   map[string]string
	hosts    map[string]string
	saved    map[string]fenceapi.SessionState
	loads    []fenceapi.HostLoad
	msgs     map[string][]string
	open     map[string][]fenceapi.ToolCall
	events   []string
	replies  []string
	waiting  map[string]string
	failures map[string]int
	restarts []string
	ended    chan string
}

func newControl() *control {
	return &control{sessions: map[string]agents.Session{}, states: map[string]string{}, hosts: map[string]string{},
		saved: map[string]fenceapi.SessionState{}, msgs: map[string][]string{}, open: map[string][]fenceapi.ToolCall{},
		waiting: map[string]string{}, failures: map[string]int{}, ended: make(chan string, 16)}
}

func (c *control) add(s agents.Session, state, host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order = append(c.order, s.Agent)
	c.sessions[s.Agent], c.states[s.Agent], c.hosts[s.Agent] = s, state, host
}

func (c *control) set(agent, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states[agent] = state
}

func (c *control) get(agent string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[agent]
}

func (c *control) TakeMessages(_ context.Context, agent string) ([]fenceapi.AgentMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []fenceapi.AgentMessage
	for _, m := range c.msgs[agent] {
		out = append(out, fenceapi.AgentMessage{Text: m})
	}
	delete(c.msgs, agent)
	return out, nil
}

func (c *control) EndTurn(_ context.Context, agent string) (bool, error) {
	c.mu.Lock()
	if c.states[agent] == agents.StateRunning {
		c.states[agent] = agents.StateIdle
	}
	c.mu.Unlock()
	c.ended <- agent
	return false, nil
}

func (c *control) RecordEvent(_ context.Context, agent, kind string, data any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := ""
	if m, ok := data.(map[string]any); ok {
		msg, _ = m["message"].(string)
	}
	c.events = append(c.events, agent+" "+kind+" "+msg)
	return nil
}

func (c *control) Reply(_ context.Context, agent, text string) (*fenceapi.AgentMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies = append(c.replies, text)
	return &fenceapi.AgentMessage{Text: text}, nil
}

func (c *control) AgentState(_ context.Context, agent string) (string, error) {
	return c.get(agent), nil
}

func (c *control) LoadSessionState(_ context.Context, agent string) (fenceapi.SessionState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saved[agent], nil
}

func (c *control) SaveSessionState(_ context.Context, agent string, st fenceapi.SessionState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saved[agent] = st
	return nil
}

func (c *control) PlaceAgent(_ context.Context, agent, host string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hosts[agent] = host
	return nil
}

func (c *control) HostedAgents(_ context.Context, host string) ([]fenceapi.HostedAgent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []fenceapi.HostedAgent
	for _, a := range c.order {
		if c.hosts[a] == host && agents.HoldsSession(c.states[a]) {
			out = append(out, fenceapi.HostedAgent{Session: c.sessions[a], State: c.states[a]})
		}
	}
	return out, nil
}

func (c *control) HostLoads(context.Context) ([]fenceapi.HostLoad, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.loads), nil
}

func (c *control) SessionWaiting(_ context.Context, agent, why string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waiting[agent] = why
	return nil
}

func (c *control) SessionStarted(_ context.Context, agent string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[agent] = 0
	delete(c.waiting, agent)
	return nil
}

func (c *control) SessionStartFailed(_ context.Context, agent, why string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[agent]++
	if c.failures[agent] >= fenceapi.MaxStartFailures {
		c.states[agent] = agents.StateFailed
		return true, nil
	}
	return false, nil
}

func (c *control) SessionRestarted(_ context.Context, agent, host string) ([]fenceapi.ToolCall, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restarts = append(c.restarts, agent+"@"+host)
	open := c.open[agent]
	delete(c.open, agent)
	return open, nil
}

// runtime is a fake Runtime: each turn's output is a transcript.
type runtime struct {
	mu      sync.Mutex
	specs   []harness.Spec
	removed []string
	left    []string
	lines   []string
	err     error
	state   []byte
}

func (r *runtime) Turn(ctx context.Context, s harness.Spec, line func([]byte)) (harness.Result, error) {
	r.mu.Lock()
	r.specs = append(r.specs, s)
	lines, err, st := r.lines, r.err, r.state
	r.mu.Unlock()
	if err != nil {
		return harness.Result{}, err
	}
	for _, l := range lines {
		line([]byte(l))
	}
	return harness.Result{State: st}, nil
}

func (r *runtime) Remove(_ context.Context, agent string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, agent)
	return nil
}

func (r *runtime) List(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.left), nil
}

func (r *runtime) turns() []harness.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.specs)
}

func transcript(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func newRunner(c *control, rt *runtime, host string, slots int) *harness.Runner {
	return &harness.Runner{Host: host, Slots: slots, Runtime: rt, Control: c, Layout: layout,
		SyncEvery: 5 * time.Millisecond, RetryAfter: time.Millisecond, Poll: 5 * time.Millisecond}
}

func waitEnd(t *testing.T, c *control, agent string) {
	t.Helper()
	select {
	case a := <-c.ended:
		if a != agent {
			t.Fatalf("%s's turn ended, not %s's", a, agent)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s's turn didn't end", agent)
	}
}

func session(id, adapter string) agents.Session {
	return agents.Session{Agent: id, Silo: "p2m8xd", Adapter: adapter, Credential: "anthropic-api", Model: "claude-opus-5-5", Prompt: "Fix the off-by-one."}
}

func TestPlacement(t *testing.T) {
	ctx := context.Background()
	c, rt := newControl(), &runtime{}
	r := newRunner(c, rt, "ctl-a", 2)
	c.loads = []fenceapi.HostLoad{{Host: "ctl-a", Slots: 2, Sessions: 1}, {Host: "ctl-b", Slots: 4, Sessions: 1}}
	c.add(session("x1", "claude-code"), agents.StateProvisioning, "")
	if err := r.Start(ctx, session("x1", "claude-code")); err != nil {
		t.Fatal(err)
	}
	if c.hosts["x1"] != "ctl-b" || r.Running() != 0 {
		t.Errorf("placed on %q, running %d here: want ctl-b, the most free slots", c.hosts["x1"], r.Running())
	}
	// On a tie, here.
	c.loads = []fenceapi.HostLoad{{Host: "ctl-a", Slots: 2}, {Host: "ctl-b", Slots: 2}}
	c.add(session("x2", "claude-code"), agents.StateProvisioning, "")
	if err := r.Start(ctx, session("x2", "claude-code")); err != nil {
		t.Fatal(err)
	}
	if c.hosts["x2"] != "ctl-a" || r.Running() != 1 {
		t.Errorf("placed on %q, running %d here", c.hosts["x2"], r.Running())
	}
	r.Stop(ctx, "x2")
	// A control VM whose heartbeat hasn't said so yet counts with its
	// own slots.
	c.loads = nil
	ok, err := r.HasSlot(ctx)
	if err != nil || !ok {
		t.Errorf("HasSlot with only this VM, free: %v %v", ok, err)
	}
	c.loads = []fenceapi.HostLoad{{Host: "ctl-a", Slots: 2, Sessions: 2}, {Host: "ctl-b", Slots: 3, Sessions: 5}}
	if ok, _ := r.HasSlot(ctx); ok {
		t.Error("HasSlot with every control VM full")
	}
	if err := r.Start(ctx, session("nope", "aider")); err == nil {
		t.Error("an adapter this release doesn't ship started")
	}
}

func TestTurns(t *testing.T) {
	ctx := context.Background()
	c, rt := newControl(), &runtime{lines: transcript(t, "claude-code.jsonl"), state: []byte("saved-1")}
	r := newRunner(c, rt, "ctl-a", 2)
	c.add(session("a1", "claude-code"), agents.StateProvisioning, "")
	if err := r.Start(ctx, session("a1", "claude-code")); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(ctx, "a1")
	// The tool server connects: its first turn, on the prompt.
	c.set("a1", agents.StateRunning)
	r.StartTurn(ctx, "a1")
	waitEnd(t, c, "a1")
	specs := rt.turns()
	if len(specs) != 1 {
		t.Fatalf("%d turns", len(specs))
	}
	first := specs[0]
	if err := first.Validate(layout); err != nil {
		t.Error(err)
	}
	if first.Argv[len(first.Argv)-1] != "Fix the off-by-one." || !slices.Contains(first.Argv, "--session-id") || first.Restore != nil {
		t.Errorf("first turn %q (restore %v)", first.Argv, first.Restore != nil)
	}
	c.mu.Lock()
	saved, replies, events := c.saved["a1"], slices.Clone(c.replies), slices.Clone(c.events)
	c.mu.Unlock()
	if saved.SessionID != "5d0a3c2e-6f1b-4a8e-9c1d-2b7f3e4a5c6d" || string(saved.State) != "saved-1" || saved.Adapter != "claude-code" {
		t.Errorf("saved %+v", saved)
	}
	if len(replies) != 2 || !strings.HasPrefix(replies[1], "The loop stops") {
		t.Errorf("replies %q", replies)
	}
	if !slices.Contains(events, "a1 usage ") {
		t.Errorf("events %q: no usage", events)
	}
	// A message: the next turn resumes the saved session.
	c.mu.Lock()
	c.msgs["a1"] = []string{"Now add a test."}
	c.mu.Unlock()
	c.set("a1", agents.StateRunning)
	r.StartTurn(ctx, "a1")
	waitEnd(t, c, "a1")
	second := rt.turns()[1]
	if i := slices.Index(second.Argv, "--resume"); i < 0 || second.Argv[i+1] != saved.SessionID || second.Argv[len(second.Argv)-1] != "Now add a test." {
		t.Errorf("second turn %q", second.Argv)
	}
	if second.UID != first.UID {
		t.Errorf("the session changed uid: %d, %d", first.UID, second.UID)
	}
}

func TestBuiltInToolsAreErrors(t *testing.T) {
	ctx := context.Background()
	c, rt := newControl(), &runtime{lines: transcript(t, "claude-code-error.jsonl")}
	r := newRunner(c, rt, "ctl-a", 1)
	c.add(session("a1", "claude-code"), agents.StateRunning, "")
	r.Start(ctx, session("a1", "claude-code"))
	defer r.Stop(ctx, "a1")
	waitEnd(t, c, "a1")
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []string
	for _, e := range c.events {
		if strings.Contains(e, " error ") {
			errs = append(errs, e)
		}
	}
	if len(errs) != 3 || !strings.Contains(errs[0], "Bash, Read") || !strings.Contains(errs[1], "built-in tool Bash") || !strings.Contains(errs[2], "529") {
		t.Errorf("errors %q", errs)
	}
}

func TestRestartResumes(t *testing.T) {
	c, rt := newControl(), &runtime{lines: transcript(t, "codex.jsonl"), left: []string{"old1", "stray"}}
	r := newRunner(c, rt, "ctl-a", 1)
	c.add(session("old1", "codex"), agents.StateRunning, "ctl-a")
	c.add(session("new2", "codex"), agents.StateIdle, "ctl-a")
	c.add(session("else", "codex"), agents.StateRunning, "ctl-b")
	c.saved["old1"] = fenceapi.SessionState{Adapter: "codex", SessionID: "0199-old", State: []byte("rollout")}
	c.open["old1"] = []fenceapi.ToolCall{{ID: "7", Tool: "exec"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.Run(ctx) }()
	waitEnd(t, c, "old1")
	specs := rt.turns()
	rt.mu.Lock()
	removed := slices.Clone(rt.removed)
	rt.mu.Unlock()
	if len(removed) < 2 || !slices.Equal(removed[:2], []string{"old1", "stray"}) {
		t.Errorf("removed %q: want the leftover containers first", removed)
	}
	if len(specs) != 1 || specs[0].Agent != "old1" {
		t.Fatalf("turns %v: want old1 alone, the oldest, in the one slot", specs)
	}
	s := specs[0]
	prompt := s.Argv[len(s.Argv)-1]
	if string(s.Restore) != "rollout" || !slices.Contains(s.Argv, "resume") || !slices.Contains(s.Argv, "0199-old") ||
		!strings.Contains(prompt, "interrupted") || !strings.Contains(prompt, "exec") || strings.Contains(prompt, "Fix the off-by-one") {
		t.Errorf("resumed turn: restore %q, argv %q", s.Restore, s.Argv)
	}
	c.mu.Lock()
	restarts, waiting := slices.Clone(c.restarts), c.waiting["new2"]
	c.mu.Unlock()
	if !slices.Equal(restarts, []string{"old1@ctl-a"}) {
		t.Errorf("restarts %q", restarts)
	}
	if waiting != fenceapi.WaitingForSlot {
		t.Errorf("new2 waits with %q", waiting)
	}
	// The slot frees: the next oldest starts.
	c.set("old1", agents.StateStopped)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		restarts := slices.Clone(c.restarts)
		c.mu.Unlock()
		if slices.Contains(restarts, "new2@ctl-a") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new2 didn't start: restarts %q", restarts)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !slices.Contains(rt.removed[2:], "new2") {
		t.Errorf("silod stopped, but new2's containers weren't removed: %q", rt.removed)
	}
	for _, s := range rt.specs {
		if s.Agent == "else" {
			t.Error("ran a session hosted on another control VM")
		}
	}
}

func TestWaitingResumesOnItsAnswer(t *testing.T) {
	c, rt := newControl(), &runtime{lines: transcript(t, "claude-code.jsonl")}
	r := newRunner(c, rt, "ctl-a", 1)
	c.add(session("w1", "claude-code"), agents.StateWaiting, "ctl-a")
	c.saved["w1"] = fenceapi.SessionState{Adapter: "claude-code", SessionID: "sid", State: []byte("s")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	time.Sleep(30 * time.Millisecond)
	if n := len(rt.turns()); n != 0 {
		t.Fatalf("%d turns while waiting for an answer", n)
	}
	c.mu.Lock()
	c.msgs["w1"] = []string{"[answer] yes"}
	c.mu.Unlock()
	c.set("w1", agents.StateRunning)
	waitEnd(t, c, "w1")
	s := rt.turns()[0]
	if p := s.Argv[len(s.Argv)-1]; !strings.Contains(p, "resumed") || !strings.Contains(p, "[answer] yes") {
		t.Errorf("prompt %q", p)
	}
}

func TestThreeFailedStartsFail(t *testing.T) {
	ctx := context.Background()
	c, rt := newControl(), &runtime{err: errors.New("sessiond: no such image")}
	r := newRunner(c, rt, "ctl-a", 1)
	c.add(session("f1", "claude-code"), agents.StateRunning, "")
	r.Start(ctx, session("f1", "claude-code"))
	deadline := time.Now().Add(5 * time.Second)
	for c.get("f1") != agents.StateFailed {
		if time.Now().After(deadline) {
			t.Fatalf("still %s after %d starts", c.get("f1"), len(rt.turns()))
		}
		time.Sleep(time.Millisecond)
	}
	specs := rt.turns()
	if len(specs) != fenceapi.MaxStartFailures {
		t.Errorf("%d starts, want %d", len(specs), fenceapi.MaxStartFailures)
	}
	for _, s := range specs {
		if s.Argv[len(s.Argv)-1] != "Fix the off-by-one." {
			t.Errorf("a retry lost the prompt: %q", s.Argv)
		}
	}
	for time.Now().Before(deadline) && r.Running() != 0 {
		time.Sleep(time.Millisecond)
	}
	if r.Running() != 0 {
		t.Error("a FAILED agent's session still runs")
	}
}
