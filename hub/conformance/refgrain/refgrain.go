// Package refgrain is the reference fake Grain: the smallest node that
// passes the conformance kit (package conformance, docs/conformance.md).
// It runs no model and touches no repository. It runs only the
// conformance workloads (docs/hub-interface.md, "Conformance
// workloads"), and fails every other task.
//
// It is also a worked example of the task interface
// (docs/hub-interface.md) for anyone writing a Grain of their own. Each
// of its parts is one section of that document:
//
//   - Run: the connection and control stream (register, heartbeat
//     with its free slots, reconnect after goodbye or a drop);
//   - handle, offer, resume and follow: the task stream (accept or
//     refuse, cancel, approve, message, result);
//   - pump: the event stream, with seq increasing across connections;
//   - view: the view stream, serving only refs it listed itself;
//   - file and depended: filing a task upward, and the dependency_done
//     that unblocks the local task waiting on it.
package refgrain

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/conformance/workload"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

// Features this Grain can advertise in its hello.
const (
	// FeatureInteractive is the interface's interactive sessions.
	FeatureInteractive = "interactive"
	// FeatureApprovals says the Grain sends approval_request events and
	// takes approve answers. The hub does not read it; the kit does.
	FeatureApprovals = "approvals"
	// FeatureFiling says the Grain files tasks upward and takes
	// dependency_done. The hub does not read it; the kit does.
	FeatureFiling = "filing"
)

// Views is how a Grain answers a view stream. Only ViewListed and
// ViewClose conform; the others are for the kit's own tests.
type Views string

const (
	// ViewListed serves a ref it listed for that task, and answers an
	// error to any other.
	ViewListed Views = ""
	// ViewClose serves a ref it listed, and refuses any other by
	// closing the stream unanswered.
	ViewClose Views = "close"
	// ViewNone closes every view stream unanswered, as a Grain that
	// serves no views does.
	ViewNone Views = "none"
	// ViewAny serves its diff whatever ref, task or node_task it is
	// asked for: what INV-11 forbids.
	ViewAny Views = "any"
)

// Grain is the reference Grain.
type Grain struct {
	// Hub is the hub's base URL; Token the node token minted for ID.
	Hub, Token, ID string
	// Repos are what it registers for; Allow is its local policy, the
	// repositories of those it runs a task for. An offer for any other
	// is refused.
	Repos, Allow []string
	// Slots is how many tasks it runs at once.
	Slots int
	// Features it lists in its hello: any of FeatureInteractive,
	// FeatureApprovals and FeatureFiling.
	Features []string
	// Tick is how often a held task says it is still running; a second
	// when zero.
	Tick time.Duration
	// Views is how it answers a view: ViewListed when empty.
	Views Views
	// UIOffered, UITeam, and UIAccess ("read-only" | "read-write") configure
	// the node's tunneled local web UI (G2-G4).
	UIOffered bool
	UITeam    string
	UIAccess  string
	// UIHandler, when set, is wrapped by the node's UI policy enforcement
	// handler; when nil, the default reference UI handler is used.
	UIHandler http.Handler
	Logf      func(format string, args ...any)

	mu    sync.Mutex
	conn  *rendezvous.Conn
	gen   int
	tasks map[string]*task // by hub task ID
	n     int
}

// TaskStatus is the node's local status/UI view of a task placed by or
// mirrored to the hub (U5).
type TaskStatus struct {
	ID        string      `json:"id"`
	NodeTask  string      `json:"node_task"`
	HubTaskID string      `json:"hub_task_id"`
	HubURL    string      `json:"hub_url"`
	Repo      string      `json:"repo,omitempty"`
	Kind      string      `json:"kind,omitempty"`
	Title     string      `json:"title,omitempty"`
	State     tasks.State `json:"state"`
	Reason    string      `json:"reason,omitempty"`
}

// HubTaskURL returns the canonical Hub task URL (<hub>/tasks/<id>) for a hub
// task ID (U5).
func HubTaskURL(hub, id string) string {
	base := strings.TrimSpace(hub)
	switch {
	case strings.HasPrefix(base, "ws://"):
		base = "http://" + strings.TrimPrefix(base, "ws://")
	case strings.HasPrefix(base, "wss://"):
		base = "https://" + strings.TrimPrefix(base, "wss://")
	}
	base = strings.TrimSuffix(base, "/")
	base = strings.TrimSuffix(base, "/v1/nodes/rendezvous")
	base = strings.TrimSuffix(base, "/")
	if base == "" || id == "" {
		return ""
	}
	return base + "/tasks/" + id
}

// TaskStatuses returns the node's local status records for all accepted tasks,
// including hub_task_id and hub_url (U5).
func (g *Grain) TaskStatuses() []TaskStatus {
	g.mu.Lock()
	list := make([]*task, 0, len(g.tasks))
	for _, t := range g.tasks {
		list = append(list, t)
	}
	g.mu.Unlock()
	out := make([]TaskStatus, 0, len(list))
	for _, t := range list {
		out = append(out, g.taskStatusOf(t))
	}
	slices.SortFunc(out, func(a, b TaskStatus) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// TaskStatus returns the node's local status record for a single task by hub
// task ID or local node_task ID (U5).
func (g *Grain) TaskStatus(id string) (TaskStatus, bool) {
	g.mu.Lock()
	t := g.tasks[id]
	if t == nil {
		for _, cand := range g.tasks {
			if cand.local == id {
				t = cand
				break
			}
		}
	}
	g.mu.Unlock()
	if t == nil {
		return TaskStatus{}, false
	}
	return g.taskStatusOf(t), true
}

func (g *Grain) taskStatusOf(t *task) TaskStatus {
	t.mu.Lock()
	state := t.result
	reason := t.reason
	over := t.over
	t.mu.Unlock()
	if state == "" {
		if over {
			state = tasks.Cancelled
		} else {
			state = tasks.Running
		}
	}
	return TaskStatus{
		ID:        t.spec.ID,
		NodeTask:  t.local,
		HubTaskID: t.spec.ID,
		HubURL:    HubTaskURL(g.Hub, t.spec.ID),
		Repo:      t.spec.Repo,
		Kind:      t.spec.Kind,
		Title:     t.spec.Title,
		State:     state,
		Reason:    reason,
	}
}

// task is one task the Grain accepted.
type task struct {
	spec  tasks.Spec
	local string
	work  string // the conformance workload

	mu     sync.Mutex
	cond   *sync.Cond
	lines  [][]byte // its events: seq i+1 at i
	result tasks.State
	reason string
	over   bool          // result set, or cancelled
	stream *tasks.Stream // the task stream it reports on, if one is open
	told   bool          // result written on stream
	pumped int           // the connection generation its pump runs for
	sent   int           // events the hub has had on that connection

	cancelled chan struct{}
	approve   chan tasks.Message
	messages  chan events.Message
	seen      map[string]bool    // message IDs taken in
	deps      chan tasks.Message // dependency_done for this local task
}

func (g *Grain) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf("refgrain %s: "+format, append([]any{g.ID}, args...)...)
	}
}

// busy is how many tasks hold a slot. Lock g.mu.
func (g *Grain) busy() int {
	n := 0
	for _, t := range g.tasks {
		t.mu.Lock()
		if !t.over {
			n++
		}
		t.mu.Unlock()
	}
	return n
}

// Run keeps the Grain connected to its hub until ctx ends. Every
// connection registers it afresh (hello), heartbeats its free slots,
// and replays the events of every task it holds, which the hub drops
// as duplicates up to the seq it has.
func (g *Grain) Run(ctx context.Context) error {
	g.mu.Lock()
	if g.tasks == nil {
		g.tasks = map[string]*task{}
	}
	g.mu.Unlock()
	node := registry.Node{ID: g.ID, Repos: g.Repos, Slots: g.Slots, Version: "refgrain", Features: g.Features}
	if g.uiEnabled() {
		node.UI = &registry.NodeUI{
			Offered: true,
			Team:    g.UITeam,
			Access:  g.uiAccess(),
		}
	}
	cl := &rendezvous.Client{
		Hub:   g.Hub,
		Token: func(context.Context) (string, error) { return g.Token, nil },
		Node:  node,
		Free: func() int {
			g.mu.Lock()
			defer g.mu.Unlock()
			return max(g.Slots-g.busy(), 0)
		},
	}
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		c, err := cl.Connect(cctx)
		cancel()
		if errors.Is(err, rendezvous.ErrRefused) {
			return err
		}
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 2*time.Second)
			continue
		}
		backoff = 100 * time.Millisecond
		g.logf("connected")
		g.mu.Lock()
		g.conn = c
		g.gen++
		gen := g.gen
		held := slices.Collect(func(yield func(*task) bool) {
			for _, t := range g.tasks {
				if !yield(t) {
					return
				}
			}
		})
		g.mu.Unlock()
		for _, t := range held {
			g.pump(c, gen, t)
		}
		stop := context.AfterFunc(ctx, func() { c.Close() })
		for {
			st, err := c.Accept()
			if err != nil {
				break
			}
			go g.handle(st)
		}
		stop()
		g.logf("connection ended (goodbye %q)", c.Goodbye())
	}
	return ctx.Err()
}

// bufConn is a stream whose first line was read through r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// handle answers one stream the hub opened: a view, or a task stream.
func (g *Grain) handle(c net.Conn) {
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	json.Unmarshal(line, &head)
	if head.Type == live.TypeView {
		var m live.ViewMessage
		json.Unmarshal(line, &m)
		switch g.Views {
		case ViewNone:
			c.Close()
		case ViewAny:
			live.ServeView(c, m, func(live.ViewMessage) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(workload.Diff)), nil
			})
		case ViewClose:
			if _, err := g.view(m); err != nil {
				c.Close()
				return
			}
			live.ServeView(c, m, g.view)
		default:
			live.ServeView(c, m, g.view)
		}
		return
	}
	if head.Type == nodeui.TypeHTTP {
		var m nodeui.Message
		if json.Unmarshal(line, &m) != nil {
			c.Close()
			return
		}
		g.serveNodeUI(bufConn{c, r}, m)
		return
	}
	var m tasks.Message
	if json.Unmarshal(line, &m) != nil || m.Task == nil {
		c.Close()
		return
	}
	st := tasks.NewStream(bufConn{c, r})
	switch m.Type {
	case tasks.TypeOffer:
		g.offer(st, *m.Task)
	case tasks.TypeResume:
		g.resume(st, *m.Task)
	case tasks.TypeCancel:
		// A cancel for a task with no open stream comes on its own.
		g.cancel(m.Task.ID)
		st.Close()
	case tasks.TypeDependencyDone:
		// So does every dependency_done, the only message on its stream.
		g.depended(m)
		st.Close()
	default:
		st.Close()
	}
}

// refuse answers an offer with a refusal.
func refuse(st *tasks.Stream, why string) {
	st.Write(tasks.Message{Type: tasks.TypeRefuse, Reason: why})
	st.Close()
}

func (g *Grain) offer(st *tasks.Stream, spec tasks.Spec) {
	g.mu.Lock()
	t := g.tasks[spec.ID]
	if t != nil {
		// Offered again: the hub lost the accept. It is the same run.
		g.mu.Unlock()
		g.attach(st, t)
		return
	}
	// The local policy, which the hub's claims do not override.
	switch {
	case !slices.Contains(g.Allow, spec.Repo):
		g.mu.Unlock()
		refuse(st, fmt.Sprintf("repository %q is outside this grain's local policy", spec.Repo))
		return
	case g.busy() >= g.Slots:
		g.mu.Unlock()
		refuse(st, "no free slot")
		return
	}
	g.n++
	t = &task{spec: spec, local: fmt.Sprintf("%s-%d", g.ID, g.n), work: workload.Parse(spec.Prompt),
		cancelled: make(chan struct{}), approve: make(chan tasks.Message, 4),
		messages: make(chan events.Message, 16), seen: map[string]bool{},
		deps: make(chan tasks.Message, 4)}
	t.cond = sync.NewCond(&t.mu)
	g.tasks[spec.ID] = t
	c, gen := g.conn, g.gen
	g.mu.Unlock()
	g.logf("accepted %s (%s) as %s", spec.ID, t.work, t.local)
	g.attach(st, t)
	go g.work(t)
	if c != nil {
		g.pump(c, gen, t)
	}
}

// resume is the hub asking whether a task it lost track of is still
// this Grain's: it is if the Grain holds it, ended or not.
func (g *Grain) resume(st *tasks.Stream, spec tasks.Spec) {
	g.mu.Lock()
	t := g.tasks[spec.ID]
	g.mu.Unlock()
	if t == nil {
		refuse(st, "not a task of this grain's")
		return
	}
	g.logf("resumed %s", spec.ID)
	g.attach(st, t)
}

// attach accepts t on st, makes st the stream t reports on, and
// follows what the hub writes there.
func (g *Grain) attach(st *tasks.Stream, t *task) {
	if err := st.Write(tasks.Message{Type: tasks.TypeAccept, NodeTask: t.local}); err != nil {
		st.Close()
		return
	}
	t.mu.Lock()
	if t.stream != nil {
		t.stream.Close()
	}
	// A result written on an earlier stream may never have reached the
	// hub: it is said again on this one.
	t.stream, t.told = st, false
	t.mu.Unlock()
	g.report(t)
	go g.follow(st, t)
}

// follow reads what the hub writes on t's task stream until it ends.
// A type the Grain does not know (attest, here) is ignored.
func (g *Grain) follow(st *tasks.Stream, t *task) {
	for {
		m, err := st.Read()
		if err != nil {
			break
		}
		switch m.Type {
		case tasks.TypeCancel:
			g.cancel(t.spec.ID)
		case tasks.TypeApprove:
			select {
			case t.approve <- m:
			default:
			}
		case tasks.TypeMessage:
			if m.Message == nil {
				continue
			}
			// At least once: one ID is taken once.
			t.mu.Lock()
			dup := t.seen[m.Message.ID]
			t.seen[m.Message.ID] = true
			t.mu.Unlock()
			if !dup {
				t.messages <- *m.Message
			}
		}
	}
	t.mu.Lock()
	if t.stream == st {
		t.stream = nil
	}
	t.mu.Unlock()
}

// cancel stops task id, freeing its slot.
func (g *Grain) cancel(id string) {
	g.mu.Lock()
	t := g.tasks[id]
	g.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.over {
		t.over = true
		close(t.cancelled)
		g.logf("cancelled %s", id)
	}
	st := t.stream
	t.mu.Unlock()
	t.cond.Broadcast()
	if st != nil {
		st.Close()
	}
}

// depended hands a dependency_done to the local task it names, if this
// Grain still has it; one for a task it no longer has is ignored.
func (g *Grain) depended(m tasks.Message) {
	g.mu.Lock()
	var t *task
	for _, c := range g.tasks {
		if c.local == m.NodeTask {
			t = c
		}
	}
	g.mu.Unlock()
	if t == nil {
		g.logf("dependency_done for %q, a task this grain does not have", m.NodeTask)
		return
	}
	select {
	case t.deps <- m:
	default:
	}
}

// file files a task upward from t, as the File workload does, and
// returns the hub's ID for it. The hub takes a filing from t only once
// it has t's accept, which may not have reached it yet: a refusal is
// tried again a few times.
func (g *Grain) file(t *task) (string, error) {
	f := tasks.Filing{
		Repo:       workload.Target(t.spec.Prompt),
		Title:      "silo conformance: " + workload.Run,
		Prompt:     workload.Prompt(workload.Run),
		OriginTask: t.spec.ID,
		NodeTask:   t.local,
	}
	var err error
	for try := 0; try < 5; try++ {
		if try > 0 {
			select {
			case <-t.cancelled:
				return "", errors.New("cancelled")
			case <-time.After(500 * time.Millisecond):
			}
		}
		g.mu.Lock()
		c := g.conn
		g.mu.Unlock()
		if c == nil {
			err = errors.New("not connected")
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var raw json.RawMessage
		raw, err = c.FileTask(ctx, f)
		cancel()
		if err != nil {
			continue
		}
		var filed tasks.Task
		if err = json.Unmarshal(raw, &filed); err != nil || filed.ID == "" {
			return "", fmt.Errorf("the hub's filed carried no task: %s", raw)
		}
		return filed.ID, nil
	}
	return "", err
}

// emit appends one event to t's stream, numbering it; payload sets the
// field its type names.
func (t *task) emit(typ string, payload func(e *live.Event)) {
	t.mu.Lock()
	e := live.Event{Event: events.Event{V: events.Version, Seq: int64(len(t.lines) + 1), Time: time.Now().UTC(), Type: typ}, Task: t.spec.ID}
	payload(&e)
	b, _ := json.Marshal(e)
	t.lines = append(t.lines, b)
	t.mu.Unlock()
	t.cond.Broadcast()
}

func (t *task) status(msg string) {
	t.emit(events.TypeStatus, func(e *live.Event) {
		e.Status = &events.Status{Phase: "running", State: "info", Message: msg}
	})
}

// finish ends t with its result: an event, then the task stream.
func (g *Grain) finish(t *task, state tasks.State, reason string) {
	t.mu.Lock()
	over := t.over
	t.mu.Unlock()
	if over {
		return
	}
	code := 0
	if state != tasks.Done {
		code = 1
	}
	t.emit(events.TypeResult, func(e *live.Event) {
		e.Result = &events.Result{State: string(state), Reason: reason, Exit: events.Exit{Code: code, Reason: reason}, FinalText: reason}
	})
	t.mu.Lock()
	if t.over {
		t.mu.Unlock()
		return
	}
	t.result, t.reason, t.over = state, reason, true
	t.mu.Unlock()
	t.cond.Broadcast()
	g.report(t)
}

// report writes t's result on its task stream, if it has both.
func (g *Grain) report(t *task) {
	t.mu.Lock()
	st, res, why, told := t.stream, t.result, t.reason, t.told
	t.mu.Unlock()
	if st == nil || res == "" || told {
		return
	}
	if st.Write(tasks.Message{Type: tasks.TypeResult, State: res, Reason: why}) == nil {
		t.mu.Lock()
		t.told = true
		t.mu.Unlock()
	}
}

// work runs t's conformance workload (docs/hub-interface.md).
func (g *Grain) work(t *task) {
	tick := g.Tick
	if tick <= 0 {
		tick = time.Second
	}
	switch t.work {
	case workload.Run:
		t.status(workload.Started)
		t.emit(live.TypeArtifact, func(e *live.Event) {
			e.Artifact = &live.Artifact{Kind: "diff", Ref: diffRef, Size: int64(len(workload.Diff))}
		})
		g.finish(t, tasks.Done, "applied the conformance change")

	case workload.Hold:
		t.status(workload.Started)
		for {
			select {
			case <-t.cancelled:
				return
			case <-time.After(tick):
				t.status("silo-conformance: holding")
			}
		}

	case workload.Approve:
		if !slices.Contains(g.Features, FeatureApprovals) {
			g.finish(t, tasks.Failed, "this grain does not ask for approvals")
			return
		}
		t.status(workload.Started)
		t.emit(events.TypeApprovalRequest, func(e *live.Event) {
			e.ApprovalRequest = &events.ApprovalRequest{ID: workload.ApprovalID, Tool: "conformance", Message: "may the conformance change be applied?"}
		})
		for {
			select {
			case <-t.cancelled:
				return
			case m := <-t.approve:
				if m.Request != workload.ApprovalID {
					continue // not a request this task is waiting on
				}
				if m.Decision == "allow" {
					g.finish(t, tasks.Done, "allowed by "+m.AnsweredBy)
				} else {
					g.finish(t, tasks.Failed, "denied by "+m.AnsweredBy)
				}
				return
			}
		}

	case workload.Interactive:
		if t.spec.Kind != "interactive" || !slices.Contains(g.Features, FeatureInteractive) {
			g.finish(t, tasks.Failed, "not an interactive session this grain runs")
			return
		}
		t.status(workload.Started)
		t.emit(events.TypeAwaitingReply, func(e *live.Event) {
			e.AwaitingReply = &events.AwaitingReply{State: events.AwaitingWaiting, Question: "what next?"}
		})
		select {
		case <-t.cancelled:
			return
		case m := <-t.messages:
			t.emit(events.TypeMessage, func(e *live.Event) { e.Message = &events.Message{ID: m.ID} })
			t.emit(events.TypeAwaitingReply, func(e *live.Event) {
				e.AwaitingReply = &events.AwaitingReply{State: events.AwaitingResumed, MessageID: m.ID}
			})
			g.finish(t, tasks.Done, "took message "+m.ID)
		}

	case workload.File:
		if !slices.Contains(g.Features, FeatureFiling) {
			g.finish(t, tasks.Failed, "this grain does not file tasks")
			return
		}
		t.status(workload.Started)
		id, err := g.file(t)
		if err != nil {
			g.finish(t, tasks.Failed, fmt.Sprintf("filing a task: %v", err))
			return
		}
		g.logf("filed %s from %s, for %s to wait on", id, t.spec.ID, t.local)
		t.status(workload.Filed + " " + id)
		for {
			select {
			case <-t.cancelled:
				return
			case m := <-t.deps:
				if m.Task == nil || m.Task.ID != id {
					continue // not the task this one waits on
				}
				if m.State != tasks.Done {
					g.finish(t, tasks.Failed, fmt.Sprintf("dependency %s is %s", id, m.State))
					return
				}
				t.status(workload.Unblocked + " " + id)
				g.finish(t, tasks.Done, "unblocked by "+id)
				return
			}
		}

	default:
		g.finish(t, tasks.Failed, "this grain runs only the conformance workloads")
	}
}

// diffRef is the Grain's own name for a run's diff: opaque to the hub.
const diffRef = "diff-1"

// pump streams t's events to the hub on c: on each connection one
// event stream, from the first event (the hub drops the ones it has),
// following until t is over and every event is written.
func (g *Grain) pump(c *rendezvous.Conn, gen int, t *task) {
	t.mu.Lock()
	if t.pumped == gen {
		t.mu.Unlock()
		return
	}
	t.pumped = gen
	t.mu.Unlock()
	go func() {
		for {
			done, err := g.pumpOnce(c, t)
			if done {
				return
			}
			g.logf("event stream for %s: %v", t.spec.ID, err)
			select {
			case <-c.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
}

// pumpOnce is one event stream. It reports whether it wrote every one
// of t's events, t being over.
func (g *Grain) pumpOnce(c *rendezvous.Conn, t *task) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, err := c.Open(ctx)
	cancel()
	if err != nil {
		return false, err
	}
	defer st.Close()
	// The hub writes nothing back: a read ending is the stream ending.
	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, st)
		close(ended)
		t.cond.Broadcast()
	}()
	sent := 0
	for {
		t.mu.Lock()
		for sent == len(t.lines) && !t.over {
			select {
			case <-ended:
				t.mu.Unlock()
				return false, errors.New("the hub ended it")
			default:
			}
			wait(t.cond, time.Second)
		}
		batch, over := t.lines[sent:], t.over
		t.mu.Unlock()
		for _, l := range batch {
			if _, err := st.Write(append(l, '\n')); err != nil {
				return false, err
			}
		}
		sent += len(batch)
		t.mu.Lock()
		all := sent == len(t.lines)
		t.mu.Unlock()
		if over && all {
			return true, nil
		}
	}
}

// wait waits on cond, its lock held, for at most d.
func wait(cond *sync.Cond, d time.Duration) {
	tm := time.AfterFunc(d, cond.Broadcast)
	cond.Wait()
	tm.Stop()
}

// view serves an artifact by its ref: only one this Grain listed, and
// never by resolving the ref as a path.
func (g *Grain) view(m live.ViewMessage) (io.ReadCloser, error) {
	g.mu.Lock()
	t := g.tasks[m.Task]
	g.mu.Unlock()
	if t == nil || t.local != m.NodeTask || m.Ref != diffRef || t.work != workload.Run {
		return nil, fmt.Errorf("no artifact %q", m.Ref)
	}
	return io.NopCloser(strings.NewReader(workload.Diff)), nil
}

// Header names synthesized on the node's local UI handler from the hub's
// nodeui stream metadata (G2-G4).
const (
	HeaderHubUIUser   = "X-Grain-Hub-UI-User"
	HeaderHubUIRole   = "X-Grain-Hub-UI-Role"
	HeaderHubUITeam   = "X-Grain-Hub-UI-Team"
	HeaderHubUIAccess = "X-Grain-Hub-UI-Access"
)

func (g *Grain) uiEnabled() bool {
	return g.UIOffered || g.UITeam != "" || g.UIAccess != "" || g.UIHandler != nil || slices.Contains(g.Features, registry.FeatureNodeUI)
}

func (g *Grain) uiAccess() string {
	switch strings.ToLower(strings.TrimSpace(g.UIAccess)) {
	case "read-write":
		return "read-write"
	default:
		return "read-only"
	}
}

func (g *Grain) serveNodeUI(c net.Conn, m nodeui.Message) {
	if !g.uiEnabled() {
		_ = nodeui.Refuse(c, "node UI is not enabled")
		return
	}
	_ = nodeui.Serve(c, m, g.LocalUIHandler(m))
}

// HasLeakedHubCredentials reports whether r carries any IAP identity/assertion
// header or Hub session/IAP cookie that hub/nodeui must strip before forwarding
// to a node (G4).
func HasLeakedHubCredentials(r *http.Request) (string, bool) {
	for k := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-goog-authenticated-user-") || lk == "x-goog-iap-jwt-assertion" || lk == "authorization" {
			return "header " + k, true
		}
	}
	for _, c := range r.Cookies() {
		name := strings.ToLower(c.Name)
		if name == strings.ToLower(identity.SessionCookie) ||
			name == "__host-"+strings.ToLower(identity.SessionCookie) ||
			name == strings.ToLower(identity.FlowCookie) ||
			name == "__host-"+strings.ToLower(identity.FlowCookie) ||
			strings.HasPrefix(name, "gcp_iap_") ||
			strings.HasPrefix(name, "__host-gcp_iap_") {
			return "cookie " + c.Name, true
		}
	}
	return "", false
}

// LocalUIHandler wraps the node's local UI handler with G2-G4 enforcement:
//   - verifies that X-Goog-Authenticated-User-* and Hub session cookies never
//     reach the node handler;
//   - stamps X-Grain-Hub-UI-User, X-Grain-Hub-UI-Role, X-Grain-Hub-UI-Team,
//     and X-Grain-Hub-UI-Access from the stream metadata m (when not already set);
//   - enforces read-only mode (allowing GET/HEAD and rejecting state-mutating
//     methods POST/PUT/DELETE/PATCH with 403 Forbidden) when
//     X-Grain-Hub-UI-Role == "team-admin" and ui.access == "read-only".
func (g *Grain) LocalUIHandler(m nodeui.Message) http.Handler {
	access := g.uiAccess()
	inner := g.UIHandler
	if inner == nil {
		inner = g.defaultUIHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if leaked, bad := HasLeakedHubCredentials(r); bad {
			http.Error(w, "forbidden: hub credential leaked to node handler: "+leaked, http.StatusBadRequest)
			return
		}
		if m.Viewer != "" && r.Header.Get(HeaderHubUIUser) == "" {
			r.Header.Set(HeaderHubUIUser, m.Viewer)
		}
		if m.Role != "" && r.Header.Get(HeaderHubUIRole) == "" {
			r.Header.Set(HeaderHubUIRole, m.Role)
		}
		if m.Team != "" && r.Header.Get(HeaderHubUITeam) == "" {
			r.Header.Set(HeaderHubUITeam, m.Team)
		}
		if r.Header.Get(HeaderHubUIAccess) == "" {
			r.Header.Set(HeaderHubUIAccess, access)
		}
		role := r.Header.Get(HeaderHubUIRole)
		if role == nodeui.RoleTeamAdmin && access == "read-only" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "forbidden: team-admin access is read-only", http.StatusForbidden)
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
}

func (g *Grain) defaultUIHandler() http.Handler {
	mux := http.NewServeMux()
	tasksHandler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node":  g.ID,
			"tasks": g.TaskStatuses(),
		})
	}
	taskByIDHandler := func(w http.ResponseWriter, r *http.Request) {
		st, ok := g.TaskStatus(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}
	mux.HandleFunc("GET /tasks", tasksHandler)
	mux.HandleFunc("GET /api/tasks", tasksHandler)
	mux.HandleFunc("GET /tasks/{id}", taskByIDHandler)
	mux.HandleFunc("GET /api/tasks/{id}", taskByIDHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node":   g.ID,
			"method": r.Method,
			"path":   r.URL.Path,
			"viewer": r.Header.Get(HeaderHubUIUser),
			"role":   r.Header.Get(HeaderHubUIRole),
			"team":   r.Header.Get(HeaderHubUITeam),
			"access": r.Header.Get(HeaderHubUIAccess),
			"tasks":  g.TaskStatuses(),
		})
	})
	return mux
}
