package live

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/logsafe"
)

// Defaults for the Hub's knobs.
const (
	DefaultRing      = 1024
	DefaultRingBytes = 1 << 20
	DefaultLinger    = 10 * time.Minute
)

// subBuffer is how many events a subscriber may fall behind by before
// the hub drops it; it reconnects from its cursor.
const subBuffer = 512

// Tasks is the backlog as the hub's event path reads it: which node a
// task is on, and who may see it. *tasks.Store is one.
type Tasks interface {
	Get(ctx context.Context, id string) (tasks.Task, error)
}

// Hub takes in the event streams nodes open (NodeStream), keeps each
// event as its class allows, and serves each task's events to its
// subscribers and its artifacts to its viewers (Handler).
type Hub struct {
	// Log keeps the stored classes: the event Store.
	Log Log
	// Tasks says which node runs a task, and whose task it is.
	Tasks Tasks
	// Directory signs subscribers and viewers in and says what they may
	// see.
	Directory *directory.API
	// Open opens a stream to a connected node, for the view-through:
	// rendezvous.Server.Open.
	Open func(ctx context.Context, nodeID string) (net.Conn, error)
	// Ring and RingBytes bound each task's in-memory ring of token and
	// node-only events: DefaultRing events and DefaultRingBytes of them
	// when zero.
	Ring, RingBytes int
	// Linger is how long a task's ring outlives its last stream and
	// subscriber; DefaultLinger when zero.
	Linger time.Duration
	// Logf, when set, receives one line per stream event and per event
	// the hub rejects.
	Logf func(format string, args ...any)
	// Now, when set, is the clock.
	Now func() time.Time
	// Ingested, when set, is told of each event a node streams up, for
	// the hub's metrics: its class, and err if the hub rejected it or
	// could not keep it.
	Ingested func(c Class, err error)

	mu        sync.Mutex
	topics    map[string]*topic
	lastSweep time.Time
}

func (h *Hub) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

func (h *Hub) ingested(c Class, err error) {
	if h.Ingested != nil {
		h.Ingested(c, err)
	}
}

func (h *Hub) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// topic is one task's live state: the high-water mark of its events,
// its ring, its subscribers and its open upstream streams.
type topic struct {
	id string

	mu       sync.Mutex
	dead     bool // swept: look it up again
	loaded   bool
	last     int64  // the last event's seq, stored or not
	lastType string // and its type
	ring     []ringEntry
	ringSize int
	subs     map[*sub]struct{}
	upstream int
	over     bool // the task is known to be over
	idle     time.Time
}

type ringEntry struct {
	e    Event
	size int
}

// sub is one subscriber. The topic closes ch when the subscription is
// over, having set why.
type sub struct {
	ch  chan Event
	why string
}

// Why a subscription ends.
const (
	whyEnd    = "end"    // the task's events are over
	whyLagged = "lagged" // the subscriber fell behind: reconnect from its cursor
)

// acquire returns task's topic, locked and loaded.
func (h *Hub) acquire(ctx context.Context, task string) (*topic, error) {
	for {
		h.mu.Lock()
		if h.topics == nil {
			h.topics = map[string]*topic{}
		}
		h.sweep()
		t := h.topics[task]
		if t == nil {
			t = &topic{id: task, subs: map[*sub]struct{}{}}
			h.topics[task] = t
		}
		h.mu.Unlock()
		t.mu.Lock()
		if t.dead {
			t.mu.Unlock()
			continue
		}
		if !t.loaded {
			seq, typ, err := h.Log.Last(ctx, task)
			if err != nil {
				t.mu.Unlock()
				return nil, err
			}
			t.last, t.lastType, t.loaded = seq, typ, true
		}
		return t, nil
	}
}

// sweep forgets topics with no stream and no subscriber that have been
// idle for Linger: their rings go with them. h.mu is held.
func (h *Hub) sweep() {
	now := h.now()
	if now.Sub(h.lastSweep) < time.Minute {
		return
	}
	h.lastSweep = now
	linger := h.Linger
	if linger <= 0 {
		linger = DefaultLinger
	}
	for id, t := range h.topics {
		if !t.mu.TryLock() {
			continue
		}
		if len(t.subs) == 0 && t.upstream == 0 && now.Sub(t.idle) >= linger {
			t.dead = true
			delete(h.topics, id)
		}
		t.mu.Unlock()
	}
}

// publish takes in one classified event. t is locked. An event at or
// before the task's high-water mark is one the hub already has, which a
// node replays after reconnecting, and is dropped.
func (h *Hub) publish(ctx context.Context, t *topic, e Event, c Class) error {
	if e.Seq <= t.last {
		return nil
	}
	if c.Stored() {
		if err := h.Log.Append(ctx, e, c); err != nil {
			return err
		}
	} else {
		h.remember(t, e)
	}
	t.last, t.lastType = e.Seq, e.Type
	t.idle = h.now()
	for s := range t.subs {
		select {
		case s.ch <- e:
		default:
			t.end(s, whyLagged)
		}
	}
	if e.Type == events.TypeResult {
		t.endAll(whyEnd)
	}
	return nil
}

// remember keeps e in t's ring, evicting the oldest past its bounds.
func (h *Hub) remember(t *topic, e Event) {
	size := len(e.Type) + 64
	if e.Transcript != nil {
		size += len(e.Transcript.Text)
	}
	if e.Artifact != nil {
		size += len(e.Artifact.Ref) + len(e.Artifact.Kind)
	}
	n, bytes := h.Ring, h.RingBytes
	if n <= 0 {
		n = DefaultRing
	}
	if bytes <= 0 {
		bytes = DefaultRingBytes
	}
	t.ring = append(t.ring, ringEntry{e, size})
	t.ringSize += size
	drop := 0
	for drop < len(t.ring)-1 && (len(t.ring)-drop > n || t.ringSize > bytes) {
		t.ringSize -= t.ring[drop].size
		drop++
	}
	if drop > 0 {
		t.ring = append(t.ring[:0:0], t.ring[drop:]...)
	}
}

func (t *topic) end(s *sub, why string) {
	if _, ok := t.subs[s]; !ok {
		return
	}
	delete(t.subs, s)
	s.why = why
	close(s.ch)
}

func (t *topic) endAll(why string) {
	for s := range t.subs {
		t.end(s, why)
	}
}

// NodeStream takes in one event stream a node opened: hand it to
// rendezvous.Server.NodeStream. The node writes its task's events, one
// JSON line each (package events), each with the hub's task ID added
// in "task". The first line binds the stream to its task, which must be
// one the hub placed on this node; a line naming another task, or a
// line too long to read, ends the stream. An event the hub rejects is
// dropped and the stream goes on.
//
// The hub writes nothing back. It closes the stream once the node has
// closed its end and every event is in; a stream it ends for any other
// reason (an event it could not keep, the stream refused) it resets,
// which the node sees as an error rather than an end, so that it
// replays the task's events, now or after it reconnects. The hub drops
// the replayed events it already has.
func (h *Hub) NodeStream(nodeID string, c net.Conn) {
	clean := false
	defer func() {
		if r, ok := c.(interface{ Reset() error }); ok && !clean {
			r.Reset()
			return
		}
		c.Close()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := bufio.NewReaderSize(c, 4096)
	var t *topic
	defer func() {
		if t != nil {
			h.upstreamClosed(t)
		}
	}()
	for {
		line, err := readLine(r)
		if err != nil {
			clean = err == io.EOF
			if t == nil {
				h.logf("live: node %q's event stream ended before it named a task: %v", nodeID, err)
			}
			return
		}
		e, class, err := Classify(line)
		if err != nil {
			h.logf("live: node %q: %s", nodeID, logsafe.Quote(err.Error()))
			h.ingested(class, err)
			continue
		}
		if t == nil {
			task, err := h.Tasks.Get(ctx, e.Task)
			if err == nil && task.Node != nodeID {
				err = fmt.Errorf("task %s is not on node %q", logsafe.Quote(e.Task), nodeID)
			}
			if err != nil {
				h.logf("live: refused node %q's event stream: %s", nodeID, logsafe.Quote(err.Error()))
				return
			}
			if t, err = h.acquire(ctx, e.Task); err != nil {
				h.logf("live: task %s: %v", logsafe.Quote(e.Task), err)
				return
			}
			t.upstream++
			t.mu.Unlock()
		} else if e.Task != t.id {
			h.logf("live: node %q sent task %s's event on task %s's stream", nodeID, logsafe.Quote(e.Task), t.id)
			return
		}
		stamp(&e, h.now())
		t.mu.Lock()
		if t.dead {
			// Not while it had a stream; never happens.
			t.mu.Unlock()
			return
		}
		err = h.publish(ctx, t, e, class)
		t.mu.Unlock()
		h.ingested(class, err)
		if err != nil {
			// The node replays it after it reconnects.
			h.logf("live: task %s: keeping event %d: %v", t.id, e.Seq, err)
			return
		}
	}
}

// upstreamClosed notes a stream's end. When the task's last stream ends
// with the task over, its subscribers have had everything. If the task
// cannot be read, it is read again until it can be (recheck).
func (h *Hub) upstreamClosed(t *topic) {
	t.mu.Lock()
	t.upstream--
	t.idle = h.now()
	open, over := t.upstream, t.over
	t.mu.Unlock()
	if open > 0 {
		return
	}
	if over {
		h.ended(t)
		return
	}
	task, err := h.Tasks.Get(context.Background(), t.id)
	if err != nil {
		h.logf("live: task %s: reading it after its last stream closed: %v", t.id, err)
		go h.recheck(t)
		return
	}
	if task.State.Terminal() {
		h.ended(t)
	}
}

// recheck reads t's task again, backing off, until it can, and ends its
// subscribers if it is over. It gives up when a stream opens, which
// closes in turn, or the subscribers have gone.
func (h *Hub) recheck(t *topic) {
	wait := 100 * time.Millisecond
	for {
		time.Sleep(wait)
		t.mu.Lock()
		waiting := !t.dead && !t.over && t.upstream == 0 && len(t.subs) > 0
		t.mu.Unlock()
		if !waiting {
			return
		}
		task, err := h.Tasks.Get(context.Background(), t.id)
		if err == nil {
			if task.State.Terminal() {
				h.ended(t)
			}
			return
		}
		h.logf("live: task %s: reading it after its last stream closed: %v; retrying in %s", t.id, err, wait)
		wait = min(2*wait, 5*time.Second)
	}
}

// ended notes that t's task is over, and ends its subscribers unless a
// stream is still open: that stream's end does.
func (h *Hub) ended(t *topic) {
	t.mu.Lock()
	t.over = true
	if t.upstream == 0 {
		t.endAll(whyEnd)
	}
	t.mu.Unlock()
}

// Finished tells the hub that a task is over: done, failed or
// cancelled. Hand it tasks.Dispatcher.Finished. Its subscribers are
// ended once no stream of its events is open, even if its result event
// never reached the hub, and without the hub having to read the task.
func (h *Hub) Finished(task string) {
	h.mu.Lock()
	t := h.topics[task]
	h.mu.Unlock()
	if t != nil {
		h.ended(t)
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, errors.New("live: event line too long")
		}
		if !isPrefix {
			return line, nil
		}
	}
}

// subscription is one subscriber's view of a task: what to replay, then
// what follows.
type subscription struct {
	replay []Event
	// s is nil when the replay is all there will be.
	s  *sub
	t  *topic
	hw int64
}

// subscribe follows task from the cursor from: the
// events after from that the hub holds (the stored ones, and the ring's
// still in memory), then the rest as they arrive. A task whose events
// end with its result, or that is over with no stream open, is replayed
// and not followed.
func (h *Hub) subscribe(ctx context.Context, task tasks.Task, from int64) (*subscription, error) {
	t, err := h.acquire(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	var ring []Event
	for _, r := range t.ring {
		if r.e.Seq > from {
			ring = append(ring, r.e)
		}
	}
	sn := &subscription{t: t, hw: t.last}
	if over := t.lastType == events.TypeResult || ((task.State.Terminal() || t.over) && t.upstream == 0); !over {
		sn.s = &sub{ch: make(chan Event, subBuffer)}
		t.subs[sn.s] = struct{}{}
	}
	t.mu.Unlock()

	var stored []Event
	if from < sn.hw {
		if stored, err = h.Log.Since(ctx, task.ID, from, sn.hw); err != nil {
			h.unsubscribe(sn)
			return nil, err
		}
	}
	// Both are in seq order and none is past hw: merge them.
	sn.replay = make([]Event, 0, len(stored)+len(ring))
	for len(stored) > 0 || len(ring) > 0 {
		if len(ring) == 0 || (len(stored) > 0 && stored[0].Seq < ring[0].Seq) {
			sn.replay, stored = append(sn.replay, stored[0]), stored[1:]
		} else {
			sn.replay, ring = append(sn.replay, ring[0]), ring[1:]
		}
	}
	return sn, nil
}

func (h *Hub) unsubscribe(sn *subscription) {
	if sn.s == nil {
		return
	}
	sn.t.mu.Lock()
	sn.t.end(sn.s, whyEnd)
	sn.t.idle = h.now()
	sn.t.mu.Unlock()
}

// follow delivers sn to send: the replay, then each event as it
// arrives, until the task's events end (whyEnd), the subscriber falls
// behind (whyLagged), ctx ends or send fails. keepalive, when set, is
// called when nothing has been sent for a while.
func (h *Hub) follow(ctx context.Context, sn *subscription, send func(Event) error, keepalive func() error) (string, error) {
	defer h.unsubscribe(sn)
	for _, e := range sn.replay {
		if err := send(e); err != nil {
			return "", err
		}
	}
	if sn.s == nil {
		return whyEnd, nil
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-sn.s.ch:
			if !ok {
				return sn.s.why, nil
			}
			if err := send(e); err != nil {
				return "", err
			}
		case <-tick.C:
			if keepalive != nil {
				if err := keepalive(); err != nil {
					return "", err
				}
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}
