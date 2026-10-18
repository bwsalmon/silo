package faulttest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/silo/hub/grainfiles"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

// Grain is a simulated Grain node: a rendezvous client that runs the
// tasks the hub offers it as a real node would, for as long as its
// Work says, streaming their events, and that keeps a task running
// through the loss of its connection, reports its result when the hub
// asks it to resume, and replays each unfinished task's events on every
// new connection.
type Grain struct {
	ID         string
	Owner      string // defaults to Ops when empty
	Hub        string // the hub's URL, or a Proxy's in front of it
	Hubs       []string
	Token      string
	Repos      []string
	Recipients []string
	Folders    []string
	Grainfiles []string
	Slots      int
	// Refuse makes the Grain refuse every offer.
	Refuse bool
	// Work is how long a task runs, and Events how many status events
	// it emits over that time, each Pad bytes of message, and Tokens
	// how many transcript (token) events it emits after each status.
	Work   time.Duration
	Events int
	Pad    int
	Tokens int
	// Ledger, when set, records what the Grain ran.
	Ledger *Ledger
	Logf   func(format string, args ...any)

	mu      sync.Mutex
	conn    *rendezvous.Conn
	gen     int // counts connections
	dialIdx int
	tasks   map[string]*localTask
	busy    int

	gfMu               sync.Mutex
	grainfileEvents    []grainfiles.Event
	grainfileBySeq     map[int64]grainfiles.Event
	grainfileDups      int
	lastGrainfileSeq   int64
	grainfileLatencies []time.Duration

	connects atomic.Int64
}

// localTask is one task the Grain has accepted.
type localTask struct {
	id, local string
	mu        sync.Mutex
	cond      *sync.Cond
	lines     [][]byte // its events, seq i+1 at i
	result    tasks.State
	stream    *tasks.Stream // the task stream to report on, if any
	reported  bool
	delivered bool // an event stream carried every event, result included
	pumped    int  // the connection generation a pump is running for
}

func (g *Grain) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf("grain %s: "+format, append([]any{g.ID}, args...)...)
	}
}

// Connects is how many times the Grain has been welcomed.
func (g *Grain) Connects() int64 { return g.connects.Load() }

// Online is whether the Grain has a connection it believes in.
func (g *Grain) Online() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn == nil {
		return false
	}
	select {
	case <-g.conn.Done():
		return false
	default:
		return true
	}
}

// Run keeps the Grain connected until ctx ends, reconnecting at once
// and then every 200ms while the hub cannot be reached: quicker than
// the rendezvous client's own backoff, so that a test's faults heal in
// seconds.
func (g *Grain) Run(ctx context.Context) {
	g.mu.Lock()
	if g.tasks == nil {
		g.tasks = map[string]*localTask{}
	}
	g.mu.Unlock()
	for ctx.Err() == nil {
		node := registry.Node{
			ID:         g.ID,
			Repos:      g.Repos,
			Slots:      g.Slots,
			Recipients: g.Recipients,
			Folders:    g.Folders,
			Grainfiles: g.Grainfiles,
		}
		var subs *grainfiles.Subscriptions
		if len(g.Recipients) > 0 || len(g.Folders) > 0 || len(g.Grainfiles) > 0 {
			node.Features = []string{registry.FeatureGrainfiles}
			subs = &grainfiles.Subscriptions{
				Recipients:  g.Recipients,
				Folders:     g.Folders,
				Grainfiles:  g.Grainfiles,
				Cursor:      g.LastGrainfileSeq(),
				ResetCursor: true,
			}
		}
		cl := &rendezvous.Client{
			Hub:              g.hub(),
			Token:            func(context.Context) (string, error) { return g.Token, nil },
			Node:             node,
			Subscriptions:    subs,
			OnGrainfileEvent: g.recordGrainfileEvent,
			Free: func() int {
				g.mu.Lock()
				defer g.mu.Unlock()
				return max(g.Slots-g.busy, 0)
			},
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		c, err := cl.Connect(cctx)
		cancel()
		if errors.Is(err, rendezvous.ErrRefused) {
			// Run would give up for good: a Grain the hub should take.
			g.Ledger.Violation("grain %s: refused by the hub: %v", g.ID, err)
		}
		if err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		g.connects.Add(1)
		g.mu.Lock()
		g.conn = c
		g.gen++
		gen := g.gen
		var open []*localTask
		for _, t := range g.tasks {
			open = append(open, t)
		}
		g.mu.Unlock()
		// Replay every task's events the hub may not have.
		for _, t := range open {
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
}

func (g *Grain) hub() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.Hubs) > 0 {
		u := g.Hubs[g.dialIdx%len(g.Hubs)]
		g.dialIdx++
		return u
	}
	return g.Hub
}

func (g *Grain) recordGrainfileEvent(ev grainfiles.Event) {
	now := time.Now()
	g.gfMu.Lock()
	defer g.gfMu.Unlock()
	if g.grainfileBySeq == nil {
		g.grainfileBySeq = map[int64]grainfiles.Event{}
	}
	if _, exists := g.grainfileBySeq[ev.Seq]; exists {
		g.grainfileDups++
		return
	}
	g.grainfileBySeq[ev.Seq] = ev
	g.grainfileEvents = append(g.grainfileEvents, ev)
	if ev.Seq > g.lastGrainfileSeq {
		g.lastGrainfileSeq = ev.Seq
	}
	if !ev.CreatedAt.IsZero() {
		if d := now.Sub(ev.CreatedAt); d >= 0 {
			g.grainfileLatencies = append(g.grainfileLatencies, d)
		}
	}
}

// LastGrainfileSeq returns the highest grainfile_event seq received by g.
func (g *Grain) LastGrainfileSeq() int64 {
	g.gfMu.Lock()
	defer g.gfMu.Unlock()
	return g.lastGrainfileSeq
}

// GrainfileEvents returns a snapshot of the unique grainfile_events received by g.
func (g *Grain) GrainfileEvents() []grainfiles.Event {
	g.gfMu.Lock()
	defer g.gfMu.Unlock()
	out := make([]grainfiles.Event, len(g.grainfileEvents))
	copy(out, g.grainfileEvents)
	return out
}

// DuplicateGrainfileEvents returns the number of duplicate grainfile_event seqs received by g.
func (g *Grain) DuplicateGrainfileEvents() int {
	g.gfMu.Lock()
	defer g.gfMu.Unlock()
	return g.grainfileDups
}

// GrainfileLatencies returns the recorded fan-out latencies for grainfile_events received by g.
func (g *Grain) GrainfileLatencies() []time.Duration {
	g.gfMu.Lock()
	defer g.gfMu.Unlock()
	out := make([]time.Duration, len(g.grainfileLatencies))
	copy(out, g.grainfileLatencies)
	return out
}

// CompleteGrainfile reports task/process completion over g's active rendezvous connection.
func (g *Grain) CompleteGrainfile(ctx context.Context, grainfileURL, commitSHA, state, reason string) ([]grainfiles.Event, error) {
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	if c == nil {
		return nil, errors.New("grain is offline")
	}
	return c.CompleteGrainfile(ctx, grainfileURL, commitSHA, state, reason)
}

// Reconnect drops the Grain's connection on its side only, without a
// word to the hub, and dials hub from now on: a node whose connection
// died without the hub noticing, coming back by another route.
func (g *Grain) Reconnect(hub string) {
	g.mu.Lock()
	g.Hub = hub
	c := g.conn
	g.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// bufConn is a stream whose first line has been read through r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// handle answers one stream the hub opened: a task stream or a view.
func (g *Grain) handle(c net.Conn) {
	r := bufio.NewReaderSize(c, 4096)
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
		live.ServeView(c, m, g.view)
		return
	}
	var m tasks.Message
	if err := json.Unmarshal(line, &m); err != nil || m.Task == nil {
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
		st.Close()
	default:
		st.Close()
	}
}

// view serves an artifact: 256 KiB of bytes, over about a second.
func (g *Grain) view(m live.ViewMessage) (io.ReadCloser, error) {
	if !strings.HasPrefix(m.Ref, "art-") {
		return nil, fmt.Errorf("no artifact %q", m.Ref)
	}
	return io.NopCloser(&slowReader{left: 256 << 10}), nil
}

type slowReader struct{ left int }

func (s *slowReader) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(30 * time.Millisecond)
	n := min(len(p), s.left, 8<<10)
	for i := range n {
		p[i] = 'x'
	}
	s.left -= n
	return n, nil
}

func (g *Grain) offer(st *tasks.Stream, spec tasks.Spec) {
	if g.Refuse {
		g.Ledger.refused(spec.ID, g.ID)
		st.Write(tasks.Message{Type: tasks.TypeRefuse, Reason: "this grain refuses everything"})
		st.Close()
		return
	}
	g.mu.Lock()
	t := g.tasks[spec.ID]
	fresh := t == nil
	if fresh {
		t = &localTask{id: spec.ID, local: fmt.Sprintf("%s-%d", g.ID, len(g.tasks)+1)}
		t.cond = sync.NewCond(&t.mu)
		g.tasks[spec.ID] = t
		g.busy++
	}
	c, gen := g.conn, g.gen
	g.mu.Unlock()
	if fresh {
		// A task the Grain already holds, offered again, is the hub
		// having lost its accept: it is the same run, not another.
		g.Ledger.started(spec.ID, g.ID)
		go g.work(t)
	}
	g.attach(st, t)
	if c != nil {
		g.pump(c, gen, t)
	}
}

func (g *Grain) resume(st *tasks.Stream, spec tasks.Spec) {
	g.mu.Lock()
	t := g.tasks[spec.ID]
	g.mu.Unlock()
	if t == nil {
		st.Write(tasks.Message{Type: tasks.TypeRefuse, Reason: "not mine"})
		st.Close()
		return
	}
	g.Ledger.resumed(spec.ID, g.ID)
	g.attach(st, t)
}

// attach makes st t's task stream: it accepts, and reports t's result
// there, now if it has one.
func (g *Grain) attach(st *tasks.Stream, t *localTask) {
	if err := st.Write(tasks.Message{Type: tasks.TypeAccept, NodeTask: t.local}); err != nil {
		st.Close()
		return
	}
	t.mu.Lock()
	if t.stream != nil {
		t.stream.Close()
	}
	// The result may have gone down a stream that never reached the
	// hub: say it again on this one.
	t.stream, t.reported = st, false
	t.mu.Unlock()
	g.report(t)
	go func() {
		// The hub writes cancel and nothing else; a read ending is the
		// stream's end.
		for {
			if _, err := st.Read(); err != nil {
				break
			}
		}
		t.mu.Lock()
		if t.stream == st {
			t.stream = nil
		}
		t.mu.Unlock()
	}()
}

// report writes t's result on its task stream, if it has both.
func (g *Grain) report(t *localTask) {
	t.mu.Lock()
	st, res, done := t.stream, t.result, t.reported
	t.mu.Unlock()
	if st == nil || res == "" || done {
		return
	}
	if err := st.Write(tasks.Message{Type: tasks.TypeResult, State: res}); err != nil {
		return
	}
	t.mu.Lock()
	t.reported = true
	t.mu.Unlock()
	g.Ledger.finished(t.id, g.ID)
	// The hub closes the stream once it has the result.
}

// work runs t: its events, spaced over Work, then its result.
func (g *Grain) work(t *localTask) {
	n := max(g.Events, 1)
	step := g.Work / time.Duration(n)
	pad := strings.Repeat("p", g.Pad)
	seq := int64(0)
	add := func(line string) {
		t.mu.Lock()
		t.lines = append(t.lines, []byte(line))
		t.mu.Unlock()
		t.cond.Broadcast()
	}
	for i := range n {
		time.Sleep(step)
		seq++
		add(fmt.Sprintf(`{"v":1,"seq":%d,"task":%q,"time":%q,"type":"status","status":{"phase":"running","state":"info","message":"step %d %s"}}`,
			seq, t.id, time.Now().UTC().Format(time.RFC3339Nano), i, pad))
		for range g.Tokens {
			seq++
			add(fmt.Sprintf(`{"v":1,"seq":%d,"task":%q,"type":"transcript","transcript":{"kind":"text","text":"tok %s"}}`, seq, t.id, pad))
		}
	}
	seq++
	add(fmt.Sprintf(`{"v":1,"seq":%d,"task":%q,"type":"result","result":{"exit":{"code":0,"reason":"completed"},"final_text":"done"}}`, seq, t.id))
	t.mu.Lock()
	t.result = tasks.Done
	t.mu.Unlock()
	t.cond.Broadcast()
	g.mu.Lock()
	g.busy--
	g.mu.Unlock()
	g.report(t)
}

// pump streams t's events to the hub on c, from the first, following
// them until the result, and reopens the stream if the hub ends it
// early (it could not keep an event). The hub drops the ones it has.
func (g *Grain) pump(c *rendezvous.Conn, gen int, t *localTask) {
	t.mu.Lock()
	if t.pumped == gen || t.delivered {
		t.mu.Unlock()
		return
	}
	t.pumped = gen
	t.mu.Unlock()
	go func() {
		for {
			select {
			case <-c.Done():
				return
			default:
			}
			if g.pumpOnce(c, t) {
				return
			}
			select {
			case <-c.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
}

// pumpOnce is one event stream; it reports whether it delivered
// everything.
func (g *Grain) pumpOnce(c *rendezvous.Conn, t *localTask) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, err := c.Open(ctx)
	cancel()
	if err != nil {
		return false
	}
	defer st.Close()
	// The hub writes nothing back: a read ending early is it ending
	// the stream, having failed to keep an event.
	ended := make(chan struct{})
	var readErr error
	go func() {
		_, readErr = io.Copy(io.Discard, st)
		close(ended)
		t.cond.Broadcast()
	}()
	var buf bytes.Buffer
	sent := 0
	for {
		t.mu.Lock()
		for sent == len(t.lines) && t.result == "" {
			select {
			case <-ended:
				t.mu.Unlock()
				return false
			case <-c.Done():
				t.mu.Unlock()
				return false
			default:
			}
			waitTimeout(t.cond, time.Second)
		}
		batch := t.lines[sent:]
		over := t.result != ""
		t.mu.Unlock()
		buf.Reset()
		for _, l := range batch {
			buf.Write(l)
			buf.WriteByte('\n')
		}
		if buf.Len() > 0 {
			if _, err := st.Write(buf.Bytes()); err != nil {
				return false
			}
		}
		sent += len(batch)
		if over && sent == func() int { t.mu.Lock(); defer t.mu.Unlock(); return len(t.lines) }() {
			break
		}
	}
	// Ask the hub to finish reading, and wait for it to close: an
	// early close (an event it could not keep) is replayed.
	if cw, ok := st.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		return false
	}
	if readErr != nil {
		// Reset: the hub did not keep every event.
		return false
	}
	t.mu.Lock()
	t.delivered = true
	t.mu.Unlock()
	return true
}

// waitTimeout waits on cond, its lock held, for at most d.
func waitTimeout(cond *sync.Cond, d time.Duration) {
	t := time.AfterFunc(d, cond.Broadcast)
	cond.Wait()
	t.Stop()
}
