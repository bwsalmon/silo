package tasks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/registry"
)

// Directory is what the dispatcher asks the team directory
// (directory.Store): a team's repositories, for a task that targets the
// team, and whether a node's owner may still serve a repository.
type Directory interface {
	TeamRepos(ctx context.Context, team string) ([]string, error)
	AdmitContext(ctx context.Context, owner string, n registry.Node) error
}

// Defaults for the Dispatcher's durations.
const (
	DefaultOfferTimeout = time.Minute
	DefaultRetry        = 30 * time.Second
)

// Dispatcher places queued tasks on nodes (docs/hub.md §6.2). It wakes
// on a new task (Wake) and on every change to the registry, and takes
// the queue oldest first per team: each pass goes round the teams, one
// task from each in turn, so a team with a long queue does not hold
// back another team's first task.
//
// For each task it claims a slot on a node that serves the task's
// repository (or one of its team's) and meets its constraints, checks
// that the node's owner is still admitted to serve it, marks the task
// offered and offers it on a stream of its own. A node that refuses is
// excluded from the task, whose claim is released and which goes back
// to the queue; a node that accepts runs it. A node lost before it
// answered gets the task back in the queue; one lost while the task
// ran leaves it lost, and when that node is next online the dispatcher
// asks it to resume the task. A team may set a grace after which a
// lost task goes back to the queue for any node (LostRequeueAfter).
//
// The dispatcher assumes it is the only one: one hub replica
// (docs/hub.md §2.2).
type Dispatcher struct {
	Store    *Store
	Registry *registry.Registry
	// Open opens a stream to a node: rendezvous.Server.Open.
	Open func(ctx context.Context, nodeID string) (net.Conn, error)
	// Directory is the team directory. Without it, a team-targeted task
	// is never placed, and a node's admission is not checked again.
	Directory Directory
	// OfferTimeout is how long a node has to answer an offer (or a
	// resume) before it counts as refusing it; DefaultOfferTimeout when
	// zero. It is also how long the dispatcher waits for a cancelled
	// task's node to report the task's end.
	OfferTimeout time.Duration
	// Retry is how often the dispatcher looks at the queue with nothing
	// to wake it, and how long it passes over a node it could not reach
	// for a task; DefaultRetry when zero.
	Retry time.Duration
	// LostRequeueAfter, when set, is how long a task's team lets it stay
	// lost before the dispatcher puts it back in the queue for any node
	// (docs/hub.md §6.3): directory.Store.LostRequeueAfter, a per-team
	// setting. Zero, for a team, or nil leaves a lost task with its node
	// until the node resumes it or a human requeues it.
	LostRequeueAfter func(ctx context.Context, team string) (time.Duration, error)
	// Logf, when set, receives one line per dispatch event.
	Logf func(format string, args ...any)
	// Observe, when set, is told of each dispatch event, with the task
	// as the dispatcher last read it: the hub's metrics.
	Observe func(ev Event, t Task)

	once sync.Once
	wake chan struct{}
	wg   sync.WaitGroup

	mu   sync.Mutex
	live map[string]*live // tasks with a stream open, by ID
	hold map[[2]string]time.Time
}

// Event is something the dispatcher did or saw, for Observe.
type Event string

// The dispatcher's events.
const (
	// EventAccepted is a node accepting an offer.
	EventAccepted Event = "accepted"
	// EventResumed is a node resuming a lost task.
	EventResumed Event = "resumed"
	// EventRefused is a node refusing an offer or a resume.
	EventRefused Event = "refused"
	// EventTimeout is a node not answering an offer or a resume within
	// OfferTimeout.
	EventTimeout Event = "timeout"
	// EventUnreachable is a node that could not be reached for an offer
	// or a resume, or went away before it answered.
	EventUnreachable Event = "unreachable"
	// EventLost is a running task's node going away.
	EventLost Event = "lost"
	// EventLostRequeued is a lost task requeued after its team's grace.
	EventLostRequeued Event = "lost_requeued"
	// EventStoreError is the backlog failing an update or a read.
	EventStoreError Event = "store_error"
)

func (d *Dispatcher) observe(ev Event, t Task) {
	if d.Observe != nil {
		d.Observe(ev, t)
	}
}

// live is a task the dispatcher has a stream open for.
type live struct {
	cancel chan struct{}
	once   sync.Once
}

func (l *live) signal() { l.once.Do(func() { close(l.cancel) }) }

func (d *Dispatcher) init() {
	d.once.Do(func() {
		d.wake = make(chan struct{}, 1)
		d.live = map[string]*live{}
		d.hold = map[[2]string]time.Time{}
	})
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

func (d *Dispatcher) offerTimeout() time.Duration {
	if d.OfferTimeout > 0 {
		return d.OfferTimeout
	}
	return DefaultOfferTimeout
}

func (d *Dispatcher) retry() time.Duration {
	if d.Retry > 0 {
		return d.Retry
	}
	return DefaultRetry
}

// Wake asks the dispatcher to look at the queue again: a task was
// submitted or requeued.
func (d *Dispatcher) Wake() {
	d.init()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run dispatches until ctx ends, and returns once every task stream it
// opened is closed. A task that was offered or running when the
// dispatcher last stopped has lost its stream: an offered one is
// requeued, and a running one is lost until its node resumes it.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.init()
	if err := d.Store.Recover(ctx, "the hub restarted"); err != nil {
		return fmt.Errorf("tasks: recovering the backlog: %w", err)
	}
	changes, stop := d.Registry.Changes()
	defer stop()
	tick := time.NewTicker(d.retry())
	defer tick.Stop()
	defer d.wg.Wait()
	for {
		if err := d.pass(ctx); err != nil && ctx.Err() == nil {
			d.logf("tasks: dispatching: %v", err)
			d.observe(EventStoreError, Task{})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changes:
		case <-d.wake:
		case <-tick.C:
		}
	}
}

// Cancel cancels a task, and tells its node if it has one: down the
// task's stream, or, for a task whose node is away, on a stream of its
// own when the node is back.
func (d *Dispatcher) Cancel(ctx context.Context, id, reason string) (Task, error) {
	d.init()
	t, err := d.Store.Cancel(ctx, id, reason)
	if err != nil {
		return t, err
	}
	d.mu.Lock()
	l := d.live[id]
	d.mu.Unlock()
	if l != nil {
		l.signal()
	} else if t.CancelPending {
		d.Wake()
	}
	return t, nil
}

// pass looks at every task the dispatcher has something to do for.
func (d *Dispatcher) pass(ctx context.Context) error {
	ts, err := d.Store.Dispatchable(ctx)
	if err != nil {
		return err
	}
	var queued []Task
	for _, t := range ts {
		if d.isLive(t.ID) {
			continue
		}
		switch {
		case t.State == Queued:
			queued = append(queued, t)
		case t.State == Lost && d.reachable(t.ID, t.Node):
			d.start(ctx, t, nil, TypeResume)
		case t.State == Lost:
			if q, ok := d.requeueLost(ctx, t); ok {
				queued = append(queued, q)
			}
		case t.State == Cancelled && d.reachable(t.ID, t.Node):
			d.start(ctx, t, nil, TypeCancel)
		}
	}
	teamRepos := map[string][]string{}
	for _, t := range fair(queued) {
		if err := d.place(ctx, t, teamRepos); err != nil {
			d.logf("tasks: placing %s: %v", t.ID, err)
		}
	}
	return nil
}

// requeueLost puts a lost task whose node is away back in the queue,
// once it has been lost for longer than its team's LostRequeueAfter.
// Its node is not excluded: if it comes back first, it may be offered
// the task afresh. The node is not told either, since it is away; a
// node that comes back still running the task is never asked to
// resume it, and its events for it are refused (docs/hub.md §6.3).
func (d *Dispatcher) requeueLost(ctx context.Context, t Task) (Task, bool) {
	if d.LostRequeueAfter == nil || t.CancelPending {
		return Task{}, false
	}
	grace, err := d.LostRequeueAfter(ctx, t.Team)
	if err != nil {
		d.logf("tasks: team %s's lost-task grace: %v", t.Team, err)
		d.observe(EventStoreError, t)
		return Task{}, false
	}
	lost := d.Store.now().Sub(t.Updated)
	if grace <= 0 || lost < grace {
		return Task{}, false
	}
	reason := fmt.Sprintf("requeued: node %s was lost for %s, past team %s's grace of %s", t.Node, lost.Truncate(time.Second), t.Team, grace)
	q, err := d.Store.RequeueLost(ctx, t.ID, reason, t.Updated)
	if err != nil {
		d.transition(ctx, t.ID)(q, err)
		return Task{}, false
	}
	d.logf("tasks: %s: %s", t.ID, reason)
	d.observe(EventLostRequeued, t)
	return q, true
}

// fair orders queued tasks, oldest first, for placing: round the teams,
// in the order of their oldest tasks, one task from each in turn.
func fair(ts []Task) []Task {
	var teams []string
	byTeam := map[string][]Task{}
	for _, t := range ts {
		if _, ok := byTeam[t.Team]; !ok {
			teams = append(teams, t.Team)
		}
		byTeam[t.Team] = append(byTeam[t.Team], t)
	}
	out := make([]Task, 0, len(ts))
	for len(out) < len(ts) {
		for _, team := range teams {
			if q := byTeam[team]; len(q) > 0 {
				out = append(out, q[0])
				byTeam[team] = q[1:]
			}
		}
	}
	return out
}

// match is whether a node meets the constraints.
func (c Constraints) match(e registry.Entry) bool {
	if c.Node != "" && e.ID != c.Node {
		return false
	}
	for k, v := range c.Labels {
		if got, ok := e.Labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// place offers a queued task to a node, if one can take it; otherwise
// the task waits for the next pass.
func (d *Dispatcher) place(ctx context.Context, t Task, teamRepos map[string][]string) error {
	repos := []string{t.Repo}
	if t.Repo == "" {
		var ok bool
		if repos, ok = teamRepos[t.Team]; !ok {
			if d.Directory != nil {
				var err error
				if repos, err = d.Directory.TeamRepos(ctx, t.Team); err != nil {
					d.logf("tasks: team %s's repositories: %v", t.Team, err)
				}
			}
			teamRepos[t.Team] = repos
		}
		if len(repos) == 0 {
			return nil
		}
	}
	for {
		c, err := d.Registry.ClaimFor(repos, func(e registry.Entry) bool {
			return !slices.Contains(t.Excluded, e.ID) && !d.held(t.ID, e.ID) && t.Constraints.match(e)
		})
		if errors.Is(err, registry.ErrNoNode) {
			return nil
		} else if err != nil {
			return err
		}
		if err := d.admit(ctx, c.NodeID, repos); err != nil {
			c.Release()
			d.logf("tasks: not offering %s to node %s: %v", t.ID, c.NodeID, err)
			d.holdNode(t.ID, c.NodeID)
			continue
		}
		ot, err := d.Store.Offer(ctx, t.ID, c.NodeID)
		if err != nil {
			c.Release()
			if errors.Is(err, ErrState) {
				return nil // cancelled meanwhile
			}
			return err
		}
		d.logf("tasks: offering %s to node %s (attempt %d)", t.ID, c.NodeID, ot.Attempts)
		d.start(ctx, ot, c, TypeOffer)
		return nil
	}
}

// admit checks again that a node's owner may serve the repositories the
// task may run in (docs/hub.md §5.3): its owner may have left the team
// since the node connected.
func (d *Dispatcher) admit(ctx context.Context, nodeID string, repos []string) error {
	if d.Directory == nil {
		return nil
	}
	e, ok := d.Registry.Get(nodeID)
	if !ok {
		return errors.New("not registered")
	}
	n := registry.Node{ID: e.ID}
	for _, r := range e.Repos {
		if slices.Contains(repos, r) {
			n.Repos = append(n.Repos, r)
		}
	}
	return d.Directory.AdmitContext(ctx, e.Owner, n)
}

// holdNode passes over node for task until Retry has gone by: the node
// could not be reached for it, or is not admitted to serve it, which
// trying again at once would not change.
func (d *Dispatcher) holdNode(task, node string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hold[[2]string{task, node}] = time.Now().Add(d.retry())
}

func (d *Dispatcher) held(task, node string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := [2]string{task, node}
	until, ok := d.hold[k]
	if ok && time.Now().After(until) {
		delete(d.hold, k)
		return false
	}
	return ok
}

// reachable is whether the dispatcher may open a stream to node for
// task now.
func (d *Dispatcher) reachable(task, node string) bool {
	if node == "" || d.held(task, node) {
		return false
	}
	e, ok := d.Registry.Get(node)
	return ok && e.Status == registry.Online
}

func (d *Dispatcher) isLive(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.live[id] != nil
}

// start opens a stream to t's node, in the background, and writes kind
// on it: an offer (holding claim c), a resume, or a cancel.
func (d *Dispatcher) start(ctx context.Context, t Task, c *registry.Claim, kind string) {
	d.mu.Lock()
	if d.live[t.ID] != nil {
		d.mu.Unlock()
		return
	}
	l := &live{cancel: make(chan struct{})}
	d.live[t.ID] = l
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.live, t.ID)
			d.mu.Unlock()
			d.Wake()
		}()
		if kind == TypeCancel {
			d.deliverCancel(ctx, t)
			return
		}
		d.follow(ctx, t, c, l, kind)
	}()
}

// deliverCancel tells the node of a task cancelled while it was away.
func (d *Dispatcher) deliverCancel(ctx context.Context, t Task) {
	conn, err := d.Open(ctx, t.Node)
	if err != nil {
		d.holdNode(t.ID, t.Node)
		return
	}
	st := NewStream(conn)
	defer st.Close()
	if err := st.Write(Message{Type: TypeCancel, Task: &Spec{ID: t.ID}, Reason: t.Reason}); err != nil {
		d.holdNode(t.ID, t.Node)
		return
	}
	if err := d.Store.CancelDelivered(ctx, t.ID); err != nil {
		d.logf("tasks: %s: %v", t.ID, err)
	}
	d.logf("tasks: told node %s that %s is cancelled", t.Node, t.ID)
}

type received struct {
	m   Message
	err error
}

// follow offers or resumes t on a stream to its node, and follows the
// task on that stream until it ends, writing a cancel down it if l is
// cancelled.
func (d *Dispatcher) follow(ctx context.Context, t Task, c *registry.Claim, l *live, kind string) {
	release := func() {
		if c != nil {
			c.Release()
		}
	}
	// unanswered is the node not answering: it could not be reached,
	// went away, or said nothing in time. An offer goes back to the
	// queue (excluding the node only if it is still there, saying
	// nothing); a resume leaves the task lost. Either way the node is
	// passed over for the task for a while.
	unanswered := func(reason string, exclude bool) {
		release()
		d.holdNode(t.ID, t.Node)
		d.logf("tasks: %s on node %s: %s", t.ID, t.Node, reason)
		if kind != TypeOffer {
			return
		}
		ex := ""
		if exclude {
			ex = t.Node
		}
		d.transition(ctx, t.ID)(d.Store.Requeue(ctx, t.ID, reason, ex))
	}
	conn, err := d.Open(ctx, t.Node)
	if err != nil {
		d.observe(EventUnreachable, t)
		unanswered("could not reach the node: "+err.Error(), false)
		return
	}
	st := NewStream(conn)
	defer st.Close()
	spec := t.Spec
	if err := st.Write(Message{Type: kind, Task: &spec}); err != nil {
		d.observe(EventUnreachable, t)
		unanswered("could not write the "+kind+": "+err.Error(), false)
		return
	}

	msgs := make(chan received)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			m, err := st.Read()
			select {
			case msgs <- received{m, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	answer := time.NewTimer(d.offerTimeout())
	defer answer.Stop()
	answered, unansweredC := false, answer.C
	var grace <-chan time.Time
	cancelled := l.cancel
	for {
		select {
		case <-ctx.Done():
			return
		case <-cancelled:
			cancelled = nil
			if !answered {
				release()
			}
			if err := st.Write(Message{Type: TypeCancel, Task: &Spec{ID: t.ID}}); err != nil {
				return // still pending: told when the node is back
			}
			d.transition(ctx, t.ID)(Task{}, d.Store.CancelDelivered(ctx, t.ID))
			d.logf("tasks: cancelled %s on node %s", t.ID, t.Node)
			grace = time.After(d.offerTimeout())
		case <-grace:
			return
		case <-unansweredC:
			d.observe(EventTimeout, t)
			unanswered(fmt.Sprintf("no answer to the %s within %s", kind, d.offerTimeout()), true)
			return
		case r := <-msgs:
			if r.err != nil {
				if !answered {
					d.observe(EventUnreachable, t)
					unanswered("lost the node before it answered: "+r.err.Error(), false)
					return
				}
				d.logf("tasks: %s: lost node %s: %v", t.ID, t.Node, r.err)
				d.observe(EventLost, t)
				d.transition(ctx, t.ID)(d.Store.Lose(ctx, t.ID, "lost the node while the task ran"))
				return
			}
			switch r.m.Type {
			case TypeAccept:
				if answered {
					continue
				}
				answered, unansweredC = true, nil
				d.logf("tasks: node %s accepted %s as %q", t.Node, t.ID, r.m.NodeTask)
				if kind == TypeOffer {
					d.observe(EventAccepted, t)
				} else {
					d.observe(EventResumed, t)
				}
				d.transition(ctx, t.ID)(d.Store.Accept(ctx, t.ID, r.m.NodeTask))
			case TypeRefuse:
				if answered {
					d.logf("tasks: node %s refused %s after accepting it; ignored", t.Node, t.ID)
					continue
				}
				release()
				d.logf("tasks: node %s refused %s: %s", t.Node, t.ID, r.m.Reason)
				d.observe(EventRefused, t)
				reason := "refused by node " + t.Node
				if r.m.Reason != "" {
					reason += ": " + r.m.Reason
				}
				exclude := t.Node
				if kind == TypeResume {
					// The node no longer has the task, so nobody is
					// running it: queue it afresh, the node included.
					exclude = ""
				}
				d.transition(ctx, t.ID)(d.Store.Requeue(ctx, t.ID, reason, exclude))
				return
			case TypeResult:
				if r.m.State != Done && r.m.State != Failed {
					d.logf("tasks: node %s reported %s %q; ignored", t.Node, t.ID, r.m.State)
					continue
				}
				d.logf("tasks: %s %s on node %s", t.ID, r.m.State, t.Node)
				d.transition(ctx, t.ID)(d.Store.Finish(ctx, t.ID, r.m.State, r.m.Reason))
				return
			default:
				d.logf("tasks: node %s sent an unknown %q message for %s", t.Node, r.m.Type, t.ID)
			}
		}
	}
}

// transition logs a store update's error. One the task's state no
// longer allows is a human having cancelled the task meanwhile, which
// is not an error.
func (d *Dispatcher) transition(ctx context.Context, id string) func(Task, error) {
	return func(_ Task, err error) {
		if err != nil && !errors.Is(err, ErrState) && ctx.Err() == nil {
			d.logf("tasks: updating %s: %v", id, err)
			d.observe(EventStoreError, Task{Spec: Spec{ID: id}})
		}
	}
}
