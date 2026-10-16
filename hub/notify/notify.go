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
}

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
	db.SetMaxOpenConns(4)
	return &Bus{url: url, db: db}, nil
}

// Close closes the connections notices are sent on.
func (b *Bus) Close() error { return b.db.Close() }

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
