package harness

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
)

// Control is what a Runner reads and writes of its agents: the fence
// API's control surface and its hosts (*fenceapi.Server).
type Control interface {
	TakeMessages(ctx context.Context, agent string) ([]fenceapi.AgentMessage, error)
	EndTurn(ctx context.Context, agent string) (more bool, err error)
	RecordEvent(ctx context.Context, agent, kind string, data any) error
	// ReportUsage meters a turn's usage as the harness reports it: a
	// subscription session's charge (agents.md, "Metering", SV-110).
	ReportUsage(ctx context.Context, agent string, usage map[string]any) error
	Reply(ctx context.Context, agent, text string) (*fenceapi.AgentMessage, error)
	AgentState(ctx context.Context, agent string) (string, error)
	LoadSessionState(ctx context.Context, agent string) (fenceapi.SessionState, error)
	SaveSessionState(ctx context.Context, agent string, st fenceapi.SessionState) error
	PlaceAgent(ctx context.Context, agent, host string) error
	HostedAgents(ctx context.Context, host string) ([]fenceapi.HostedAgent, error)
	HostLoads(ctx context.Context) ([]fenceapi.HostLoad, error)
	SessionWaiting(ctx context.Context, agent, why string) error
	SessionStarted(ctx context.Context, agent string) error
	SessionStartFailed(ctx context.Context, agent, why string) (failed bool, err error)
	SessionRestarted(ctx context.Context, agent, host string) ([]fenceapi.ToolCall, error)
}

// Sockets opens and closes sessions' sockets (package session's
// Sockets).
type Sockets interface {
	Open(agent, silo string) (string, error)
	Close(agent string)
}

// Runner is the agents.SessionRunner of one control VM, its Host.
//
// Start places a session on the control VM with the most free slots,
// recorded as the agent's host; the host's Runner runs it. Run removes
// the session containers it finds, then every SyncEvery runs every
// session hosted here that isn't running, oldest first, as far as
// Slots go, and stops any that is no longer hosted here. A session
// that was RUNNING, IDLE or WAITING when it was last run is resumed,
// audited agent.session_restart, with its in-flight tool calls
// reported as interrupted.
type Runner struct {
	Host    string
	Slots   int
	Runtime Runtime
	Control Control
	Sockets Sockets
	Layout  Layout
	Limits  Limits
	// Adapters are the adapters it runs; Adapters if nil.
	Adapters map[string]Adapter
	// SyncEvery is how often it looks for sessions to start; 5s if 0.
	SyncEvery time.Duration
	// RetryAfter is how long after a failed start it tries again; 5s
	// if 0.
	RetryAfter time.Duration
	// Poll is how often a resumed WAITING session looks for its
	// answer; 2s if 0.
	Poll time.Duration

	mu   sync.Mutex
	base context.Context
	live map[string]*live
}

type live struct {
	s      agents.Session
	slot   int
	kick   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
	// restore is set until a turn has run with the saved state put
	// back.
	restore bool
	// notice is said at the start of the next turn: the restart.
	notice string
	// carry is a prompt a failed start didn't deliver.
	carry string
}

func (r *Runner) adapter(name string) (Adapter, bool) {
	m := r.Adapters
	if m == nil {
		m = Adapters
	}
	a, ok := m[name]
	return a, ok
}

func dur(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// Running is how many sessions it runs.
func (r *Runner) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}

// loads are the control VMs' loads, with this one's as it knows it if
// its heartbeat hasn't said so yet.
func (r *Runner) loads(ctx context.Context) ([]fenceapi.HostLoad, error) {
	loads, err := r.Control.HostLoads(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range loads {
		if h.Host == r.Host {
			return loads, nil
		}
	}
	return append(loads, fenceapi.HostLoad{Host: r.Host, Slots: r.Slots, Sessions: r.Running()}), nil
}

// HasSlot implements agents.SessionRunner.
func (r *Runner) HasSlot(ctx context.Context) (bool, error) {
	loads, err := r.loads(ctx)
	if err != nil {
		return false, err
	}
	return fenceapi.PickHost(loads, r.Host) != "", nil
}

// Start implements agents.SessionRunner: it places the session, and
// runs it if it is placed here.
func (r *Runner) Start(ctx context.Context, s agents.Session) error {
	if _, ok := r.adapter(s.Adapter); !ok {
		return fmt.Errorf("no adapter %q on this control VM", s.Adapter)
	}
	loads, err := r.loads(ctx)
	if err != nil {
		return err
	}
	host := fenceapi.PickHost(loads, r.Host)
	if host == "" {
		// Nowhere free: it waits here for a slot.
		host = r.Host
	}
	if err := r.Control.PlaceAgent(ctx, s.Agent, host); err != nil {
		return err
	}
	if host != r.Host {
		// Its host's Runner starts it on its next sync.
		return nil
	}
	r.launch(ctx, s, false)
	return nil
}

// launch runs s here if a slot is free, and otherwise records that it
// waits; it reports whether it runs.
func (r *Runner) launch(ctx context.Context, s agents.Session, restart bool) bool {
	r.mu.Lock()
	if r.live == nil {
		r.live = map[string]*live{}
	}
	if _, ok := r.live[s.Agent]; ok {
		r.mu.Unlock()
		return true
	}
	slot := r.freeSlot()
	if slot < 0 {
		r.mu.Unlock()
		if err := r.Control.SessionWaiting(ctx, s.Agent, fenceapi.WaitingForSlot); err != nil {
			log.Printf("harness: %s: %v", s.Agent, err)
		}
		return false
	}
	base := r.base
	if base == nil {
		base = context.WithoutCancel(ctx)
	}
	lctx, cancel := context.WithCancel(base)
	l := &live{s: s, slot: slot, kick: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}), restore: true}
	r.live[s.Agent] = l
	r.mu.Unlock()
	if r.Sockets != nil {
		if _, err := r.Sockets.Open(s.Agent, s.Silo); err != nil {
			log.Printf("harness: %s: opening its socket: %v", s.Agent, err)
		}
	}
	if restart {
		calls, err := r.Control.SessionRestarted(ctx, s.Agent, r.Host)
		if err != nil {
			log.Printf("harness: %s: recording its restart: %v", s.Agent, err)
		}
		l.notice = restartNotice(calls)
	}
	go r.loop(lctx, l)
	return true
}

// freeSlot is the lowest slot no live session has, or -1; r.mu is
// held.
func (r *Runner) freeSlot() int {
	n := r.Slots
	if n > MaxSessions {
		n = MaxSessions
	}
	used := map[int]bool{}
	for _, l := range r.live {
		used[l.slot] = true
	}
	for i := 0; i < n; i++ {
		if !used[i] {
			return i
		}
	}
	return -1
}

func restartNotice(calls []fenceapi.ToolCall) string {
	msg := "[silo] This session was resumed: silod restarted, or the session moved to another control VM."
	if len(calls) > 0 {
		var names []string
		for _, c := range calls {
			names = append(names, c.Tool)
		}
		msg += " Your tool call(s) in flight (" + strings.Join(names, ", ") + ") were interrupted. " +
			"A command one started may still be running on the tool VM, and silo-tools keeps its result for ten minutes, so you can ask for it."
	}
	return msg + " Continue where you left off."
}

func (l *live) poke() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// StartTurn implements agents.TurnStarter: agent has a message, or its
// tool server has connected for its first turn.
func (r *Runner) StartTurn(ctx context.Context, agent string) error {
	r.mu.Lock()
	l := r.live[agent]
	r.mu.Unlock()
	if l != nil {
		l.poke()
	}
	return nil
}

// Stop implements agents.SessionRunner: the session's harness is
// stopped and its containers and private directory removed. Its saved
// state, from its last turn, stays in the database.
func (r *Runner) Stop(ctx context.Context, agent string) error {
	r.mu.Lock()
	l := r.live[agent]
	delete(r.live, agent)
	r.mu.Unlock()
	if l != nil {
		l.cancel()
		select {
		case <-l.done:
		case <-time.After(30 * time.Second):
		}
	}
	err := r.Runtime.Remove(ctx, agent)
	if r.Sockets != nil {
		r.Sockets.Close(agent)
	}
	return err
}

// loop runs l's turns until it is stopped.
func (r *Runner) loop(ctx context.Context, l *live) {
	defer close(l.done)
	agent := l.s.Agent
	switch st, _ := r.Control.AgentState(ctx, agent); st {
	case agents.StateRunning:
		// In a turn when it last ran, or its first turn began before
		// it got here: on with it.
		l.poke()
	case agents.StateWaiting:
		// Its ask was in flight; its answer makes it RUNNING.
		go r.awaitAnswer(ctx, l)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.kick:
		}
		for ctx.Err() == nil {
			more, err := r.turn(ctx, l)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("harness: %s: %v", agent, err)
				}
				break
			}
			if !more {
				break
			}
		}
	}
}

func (r *Runner) awaitAnswer(ctx context.Context, l *live) {
	t := time.NewTicker(dur(r.Poll, 2*time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := r.Control.AgentState(ctx, l.s.Agent)
		if err != nil {
			continue
		}
		switch st {
		case agents.StateWaiting:
			continue
		case agents.StateRunning:
			l.poke()
		}
		return
	}
}

// errFailed is a session whose agent is FAILED.
var errFailed = errors.New("the harness failed to start too many times")

// turn runs one turn, and reports whether another follows at once.
func (r *Runner) turn(ctx context.Context, l *live) (bool, error) {
	agent := l.s.Agent
	a, ok := r.adapter(l.s.Adapter)
	if !ok {
		return false, fmt.Errorf("no adapter %q", l.s.Adapter)
	}
	saved, err := r.Control.LoadSessionState(ctx, agent)
	if err != nil {
		return false, err
	}
	if saved.Adapter != "" && saved.Adapter != a.Name() {
		saved = fenceapi.SessionState{}
	}
	msgs, err := r.Control.TakeMessages(ctx, agent)
	if err != nil {
		return false, err
	}
	var parts []string
	if l.carry != "" {
		parts = append(parts, l.carry)
	} else {
		if l.notice != "" {
			parts = append(parts, l.notice)
		}
		if saved.SessionID == "" {
			parts = append(parts, l.s.Prompt)
		}
	}
	for _, m := range msgs {
		parts = append(parts, m.Text)
	}
	prompt := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if prompt == "" {
		if st, _ := r.Control.AgentState(ctx, agent); st == agents.StateRunning {
			return r.Control.EndTurn(ctx, agent)
		}
		return false, nil
	}
	launch := Launch{Model: l.s.Model, Prompt: prompt, SessionID: saved.SessionID, Resume: saved.SessionID != ""}
	if !launch.Resume {
		launch.SessionID = a.NewSessionID()
	}
	spec := NewSpec(r.Layout, agent, l.slot, a, a.Command(launch), r.Limits)
	if l.restore && len(saved.State) > 0 {
		spec.Restore = saved.State
	}
	h := &handler{r: r, ctx: ctx, agent: agent, p: a.Parser()}
	res, err := r.Runtime.Turn(ctx, spec, h.line)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil || res.Exit != 0 && !h.started {
		why := fmt.Sprintf("exit %d", res.Exit)
		if err != nil {
			why = err.Error()
		}
		r.event(ctx, agent, fenceapi.EventError, map[string]any{"message": "the harness didn't start: " + why})
		l.carry = prompt
		failed, ferr := r.Control.SessionStartFailed(ctx, agent, why)
		if ferr != nil {
			return false, ferr
		}
		if failed {
			go r.Stop(context.WithoutCancel(ctx), agent)
			return false, errFailed
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(dur(r.RetryAfter, 5*time.Second)):
		}
		return true, nil
	}
	l.carry, l.notice, l.restore = "", "", false
	if err := r.Control.SessionStarted(ctx, agent); err != nil {
		log.Printf("harness: %s: %v", agent, err)
	}
	if res.Exit != 0 {
		r.event(ctx, agent, fenceapi.EventError, map[string]any{"message": fmt.Sprintf("the harness exited %d", res.Exit)})
	}
	id := h.sessionID
	if id == "" {
		id = launch.SessionID
	}
	if id != "" {
		if err := r.Control.SaveSessionState(ctx, agent, fenceapi.SessionState{Adapter: a.Name(), SessionID: id, State: res.State}); err != nil {
			r.event(ctx, agent, fenceapi.EventError, map[string]any{"message": "saving the session: " + err.Error()})
		}
	}
	return r.Control.EndTurn(ctx, agent)
}

func (r *Runner) event(ctx context.Context, agent, kind string, data map[string]any) {
	if err := r.Control.RecordEvent(context.WithoutCancel(ctx), agent, kind, data); err != nil {
		log.Printf("harness: %s: recording %s: %v", agent, kind, err)
	}
}

// handler turns one turn's output into the session's events.
type handler struct {
	r         *Runner
	ctx       context.Context
	agent     string
	p         Parser
	started   bool
	sessionID string
}

func (h *handler) line(b []byte) {
	for _, ev := range h.p.Line(b) {
		h.started = true
		switch ev.Kind {
		case EvSession:
			if ev.SessionID != "" {
				h.sessionID = ev.SessionID
			}
			var local []string
			for _, t := range ev.Tools {
				if !isSiloTool(t) {
					local = append(local, t)
				}
			}
			if len(local) > 0 {
				h.r.event(h.ctx, h.agent, fenceapi.EventError, map[string]any{
					"message": "the harness has built-in tools its adapter should have turned off: " + strings.Join(local, ", ")})
			}
		case EvReply:
			if _, err := h.r.Control.Reply(h.ctx, h.agent, ev.Text); err != nil {
				log.Printf("harness: %s: %v", h.agent, err)
			}
		case EvLocalTool:
			h.r.event(h.ctx, h.agent, fenceapi.EventError, map[string]any{
				"message": "the harness called its built-in tool " + ev.Tool + ", which its adapter turns off; the container allows it nothing"})
		case EvUsage:
			if err := h.r.Control.ReportUsage(context.WithoutCancel(h.ctx), h.agent, ev.Usage); err != nil {
				log.Printf("harness: %s: metering its usage: %v", h.agent, err)
			}
		case EvError:
			h.r.event(h.ctx, h.agent, fenceapi.EventError, map[string]any{"message": ev.Text})
		}
	}
}

// Sync starts the sessions hosted here that aren't running, oldest
// first, and stops the ones that aren't hosted here any more. A session
// it starts that was already past PROVISIONING is a restart.
func (r *Runner) Sync(ctx context.Context) error {
	hosted, err := r.Control.HostedAgents(ctx, r.Host)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, h := range hosted {
		want[h.Agent] = true
	}
	r.mu.Lock()
	var gone []string
	for a := range r.live {
		if !want[a] {
			gone = append(gone, a)
		}
	}
	r.mu.Unlock()
	for _, a := range gone {
		if err := r.Stop(ctx, a); err != nil {
			log.Printf("harness: stopping %s: %v", a, err)
		}
	}
	for _, h := range hosted {
		s := h.Session
		s.Resume = true
		r.launch(ctx, s, h.State != agents.StateProvisioning)
	}
	return nil
}

// Run is the Runner's life: it removes every session container on this
// control VM (none outlives the silod that ran it), then syncs until
// ctx ends, when it stops every session.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	r.base = ctx
	r.mu.Unlock()
	left, err := r.Runtime.List(ctx)
	if err != nil {
		return fmt.Errorf("listing session containers: %w", err)
	}
	for _, a := range left {
		if err := r.Runtime.Remove(ctx, a); err != nil {
			return fmt.Errorf("removing %s's session containers: %w", a, err)
		}
	}
	t := time.NewTicker(dur(r.SyncEvery, 5*time.Second))
	defer t.Stop()
	for {
		if err := r.Sync(ctx); err != nil && ctx.Err() == nil {
			log.Printf("harness: sync: %v", err)
		}
		select {
		case <-ctx.Done():
			r.stopAll()
			return nil
		case <-t.C:
		}
	}
}

func (r *Runner) stopAll() {
	r.mu.Lock()
	var all []string
	for a := range r.live {
		all = append(all, a)
	}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, a := range all {
		r.Stop(ctx, a)
	}
}
