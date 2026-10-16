// Package notify carries the hub's wake-ups between its pods
// (docs/hub.md §2.2.4): PostgreSQL NOTIFY on the channel silo_hub,
// with a short JSON payload, which every pod LISTENs for on a
// connection of its own. A pod that sends a notice gets it too.
//
// SQLite, one process, has no bus: Open returns nil, and the hub keeps
// its in-memory signals.
package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
)

// Channel is the channel the hub's notices go out on.
const Channel = "silo_hub"

// Message is one notice. Exactly one of Wake, Cancel, Node and Event is
// set on the wire:
//
//	{"wake":true}                 a task was submitted or requeued: the dispatcher looks at the queue
//	{"cancel":"<task>"}           a task was cancelled: the dispatcher signals its stream
//	{"node":"<id>"}               a node's row in the shared registry changed: refill the mirror
//	{"event":"<task>","seq":N}    the task's event N was stored
type Message struct {
	Wake   bool   `json:"wake,omitempty"`
	Cancel string `json:"cancel,omitempty"`
	Node   string `json:"node,omitempty"`
	Event  string `json:"event,omitempty"`
	Seq    int64  `json:"seq,omitempty"`

	// Reconnected is never sent: the bus hands it to its handlers each
	// time its listening connection is (re)established, since notices
	// sent while it was down are lost.
	Reconnected bool `json:"-"`
}

// Bus sends and receives the hub's notices. Its handlers are called,
// one at a time, on the goroutine running Run: they must not block.
type Bus struct {
	url string
	db  *sql.DB
	// Logf, when set, receives the listening connection's failures.
	Logf func(format string, args ...any)

	mu        sync.Mutex
	handlers  []func(Message)
	listening atomic.Bool

	// Notices Post has queued, by what they are about, which sender
	// sends.
	pmu      sync.Mutex
	pending  map[string]Message
	order    []string
	posted   chan struct{}
	stop     chan struct{}
	sent     chan struct{}
	startOne sync.Once
	stopOne  sync.Once
}

// maxConns caps the connections notices are sent on, all of which the
// bus keeps open once made: Post's sender, one at a time, and Notify's
// callers.
const maxConns = 4

// Post's sender gathers notices for postWindow before sending them,
// and sends at most postBatch in one statement.
const (
	postWindow = 2 * time.Millisecond
	postBatch  = 500
	postWait   = 5 * time.Second
)

// Open returns a bus over the PostgreSQL database at url, or nil for
// any other database: SQLite keeps its signals in memory.
func Open(url string) (*Bus, error) {
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		return nil, nil
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	// Every stored event is announced, on the node's stream as it is
	// read: thousands a second at 500 Grains. Too few connections, or
	// ones made afresh for each notice, hold up the streams behind them
	// once the database is a round trip away
	// (docs/test-runs/2026-10-16-hub-load.md).
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	return &Bus{url: url, db: db, pending: map[string]Message{}, posted: make(chan struct{}, 1),
		stop: make(chan struct{}), sent: make(chan struct{})}, nil
}

// Close sends what Post has queued, and closes the connections notices
// are sent on.
func (b *Bus) Close() error {
	b.stopOne.Do(func() {
		close(b.stop)
		b.startOne.Do(func() { close(b.sent) })
		<-b.sent
	})
	return b.db.Close()
}

func (b *Bus) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}

// Handle adds a handler for every notice the bus receives, this pod's
// own included. Add them before Run.
func (b *Bus) Handle(f func(Message)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers = append(b.handlers, f)
}

// Listening is whether the bus's listening connection is up.
func (b *Bus) Listening() bool { return b.listening.Load() }

// Notify sends m to every pod.
func (b *Bus) Notify(ctx context.Context, m Message) error {
	p, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = b.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, Channel, string(p))
	return err
}

// Post queues m to be sent to every pod, and returns without waiting
// for the database. Notices posted close together go out in one
// statement, and so in one transaction, from a goroutine of the bus's
// own; and the ones about the same thing are coalesced while they wait:
// one wake-up, one per node, one per task's events, with the highest
// seq posted. A receiver reads the state again at a notice, so a later
// one stands for the earlier.
//
// What Post sends is not retried: a notice that fails is logged, and
// lost, which every receiver tolerates (docs/hub.md §2.2.4). Notices
// posted after Close are dropped.
func (b *Bus) Post(m Message) {
	var key string
	switch {
	case m.Wake:
		key = "w"
	case m.Node != "":
		key = "n" + m.Node
	case m.Event != "":
		key = "e" + m.Event
	case m.Cancel != "":
		key = "c" + m.Cancel
	default:
		return
	}
	b.startOne.Do(func() { go b.send() })
	b.pmu.Lock()
	if old, ok := b.pending[key]; ok {
		if old.Seq > m.Seq {
			m.Seq = old.Seq
		}
	} else {
		b.order = append(b.order, key)
	}
	b.pending[key] = m
	b.pmu.Unlock()
	select {
	case b.posted <- struct{}{}:
	default:
	}
}

// send sends what Post queues until Close, and then what is left.
func (b *Bus) send() {
	defer close(b.sent)
	for {
		select {
		case <-b.posted:
		case <-b.stop:
			b.flush()
			return
		}
		// Gather the notices posted close behind this one; those posted
		// while flush waits on the database gather by themselves.
		select {
		case <-time.After(postWindow):
			b.flush()
		case <-b.stop:
			b.flush()
			return
		}
	}
}

// flush sends every notice queued, postBatch at a time.
func (b *Bus) flush() {
	b.pmu.Lock()
	order, pending := b.order, b.pending
	b.order, b.pending = nil, make(map[string]Message, len(pending))
	b.pmu.Unlock()
	for len(order) > 0 {
		n := min(len(order), postBatch)
		ps := make([]string, 0, n)
		for _, k := range order[:n] {
			p, err := json.Marshal(pending[k])
			if err != nil {
				continue
			}
			ps = append(ps, string(p))
		}
		order = order[n:]
		ctx, cancel := context.WithTimeout(context.Background(), postWait)
		_, err := b.db.ExecContext(ctx, `SELECT pg_notify($1, p) FROM unnest($2::text[]) AS p`, Channel, ps)
		cancel()
		if err != nil {
			b.logf("notify: sending %d notices on %s: %v; their receivers read again at their next look", len(ps), Channel, err)
		}
	}
}

func (b *Bus) deliver(m Message) {
	b.mu.Lock()
	hs := b.handlers
	b.mu.Unlock()
	for _, h := range hs {
		h(m)
	}
}

// Run listens until ctx ends, handing each notice to the handlers. It
// reconnects, backing off, when its connection fails, and hands the
// handlers a Reconnected message each time it is listening again (the
// first time included). It returns ctx's error.
func (b *Bus) Run(ctx context.Context) error {
	wait := 100 * time.Millisecond
	for {
		err := b.listen(ctx)
		if b.listening.Swap(false) {
			wait = 100 * time.Millisecond
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.logf("notify: listening on %s: %v; reconnecting in %s", Channel, err, wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, 5*time.Second)
	}
}

// listen runs one listening connection until it fails or ctx ends.
func (b *Bus) listen(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := pgx.Connect(cctx, b.url)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn.Close(cctx)
		cancel()
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	b.listening.Store(true)
	b.deliver(Message{Reconnected: true})
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var m Message
		if err := json.Unmarshal([]byte(n.Payload), &m); err != nil {
			b.logf("notify: an unreadable notice on %s: %v", Channel, err)
			continue
		}
		b.deliver(m)
	}
}
