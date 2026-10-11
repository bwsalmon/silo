// Package leader is silod's leadership (docs/silo/state.md, "More than
// one silod, and the leader", SV-52): which replica applies the fence,
// runs the reconcile, adopts the operations of replicas that died,
// refreshes the root's capacity and prunes the audit log.
//
// Every replica runs Run. It asks the store for the "leader" lease
// (Store.Lead, which on Postgres tries the advisory lock every five
// seconds and on SQLite, where there is one process, takes it at once).
// A replica that takes it bumps the leader epoch, a counter row, and
// starts its duties under a context that ends the moment the lease is
// lost. Every leader-only write calls Guard in its own transaction,
// which refuses it unless the epoch is still the one this term took:
// a leader that stalled past losing the lease, and wakes after another
// replica has taken it, writes nothing.
package leader

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/store"
)

// Lease is the name of the leader's lease.
const Lease = "leader"

// ErrNotLeader is a leader-only write by a term that is no longer the
// current one.
var ErrNotLeader = errors.New("leader: no longer the leader (the leader epoch moved on)")

// Term is one replica's hold on the lease, from taking it to losing it.
type Term struct {
	// Epoch is the leader epoch the term took.
	Epoch int64
	// Holder is the replica holding it.
	Holder string
	// Lost is closed when the lease is lost; the term's context ends
	// with it.
	Lost <-chan struct{}
}

type termKey struct{}

// WithTerm is ctx carrying t, whose writes Guard checks.
func WithTerm(ctx context.Context, t *Term) context.Context {
	return context.WithValue(ctx, termKey{}, t)
}

// TermOf is the term ctx carries, or nil.
func TermOf(ctx context.Context) *Term {
	t, _ := ctx.Value(termKey{}).(*Term)
	return t
}

// Guard refuses a leader-only write, in tx, unless the term ctx carries
// is still the current one: it fails with ErrNotLeader if the epoch has
// moved on, or if ctx carries no term at all. It also fails once ctx
// has ended, which it does the moment the lease is lost.
func Guard(ctx context.Context, tx store.Tx) error {
	t := TermOf(ctx)
	if t == nil {
		return fmt.Errorf("%w: the write carries no leader term", ErrNotLeader)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrNotLeader, err)
	}
	return Check(ctx, tx, t.Epoch)
}

// Check fails with ErrNotLeader unless the leader epoch is epoch.
func Check(ctx context.Context, tx store.Tx, epoch int64) error {
	cur, err := Current(ctx, tx)
	if err != nil {
		return err
	}
	if cur != epoch {
		return fmt.Errorf("%w: epoch %d, this term's is %d", ErrNotLeader, cur, epoch)
	}
	return nil
}

// Current is the leader epoch: 0 if no leader has ever taken the lease.
func Current(ctx context.Context, tx store.Tx) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT epoch FROM leader_epoch WHERE name = ?`, Lease).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Take bumps the leader epoch for holder, which has just taken the
// lease, and returns the new epoch.
func Take(ctx context.Context, st store.Store, holder string) (int64, error) {
	var epoch int64
	err := st.Tx(ctx, func(tx store.Tx) error {
		cur, err := Current(ctx, tx)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		epoch = cur + 1
		_, err = tx.Exec(ctx, `INSERT INTO leader_epoch (name, epoch, holder, taken_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET epoch = excluded.epoch, holder = excluded.holder, taken_at = excluded.taken_at`,
			Lease, epoch, holder, store.Millis(now))
		return err
	})
	return epoch, err
}

// Duty is one piece of leader work.
type Duty struct {
	Name string
	// Every is how often Run is called while the term lasts, the first
	// time at once. Zero calls it once, for the whole term: a duty that
	// follows something until its context ends, as the fence does.
	Every time.Duration
	// Run does the work. Its context carries the term and ends when the
	// term does.
	Run func(ctx context.Context) error
}

// Options configure Run.
type Options struct {
	Store store.Store
	// ID is this replica's id, recorded as the epoch's holder.
	ID     string
	Duties []Duty
	// Retry is how long to wait before asking again after a failure to
	// take the lease or bump the epoch. Zero means five seconds.
	Retry time.Duration
	// OnTerm, if set, is called as each term starts and after it ends.
	OnTerm func(t *Term, started bool)
	// Logf logs; nil is log.Printf.
	Logf func(format string, args ...any)
}

// Run takes the lease whenever it can and runs the duties while it
// holds it, until ctx ends. Each term's duties have all returned before
// the lease is asked for again.
func Run(ctx context.Context, o Options) error {
	logf := o.Logf
	if logf == nil {
		logf = log.Printf
	}
	retry := o.Retry
	if retry <= 0 {
		retry = 5 * time.Second
	}
	for ctx.Err() == nil {
		if err := term(ctx, o, logf); err != nil && ctx.Err() == nil {
			logf("leader: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(retry):
			}
		}
	}
	return ctx.Err()
}

// term holds the lease once, from taking it to losing it.
func term(ctx context.Context, o Options, logf func(string, ...any)) error {
	hold, release := context.WithCancel(ctx)
	defer release()
	lost, err := o.Store.Lead(hold, Lease)
	if err != nil {
		return err
	}
	epoch, err := Take(hold, o.Store, o.ID)
	if err != nil {
		return fmt.Errorf("taking the leader epoch: %w", err)
	}
	t := &Term{Epoch: epoch, Holder: o.ID, Lost: lost}
	tctx, stop := context.WithCancel(WithTerm(hold, t))
	defer stop()
	go func() {
		select {
		case <-lost:
			stop()
		case <-tctx.Done():
		}
	}()
	logf("leader: %s is the leader, epoch %d", o.ID, epoch)
	if o.OnTerm != nil {
		o.OnTerm(t, true)
	}
	var wg sync.WaitGroup
	for _, d := range o.Duties {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runDuty(tctx, d, logf)
		}()
	}
	<-tctx.Done()
	select {
	case <-lost:
		logf("leader: %s lost the lease (epoch %d): leader work stops", o.ID, epoch)
	default:
	}
	wg.Wait()
	if o.OnTerm != nil {
		o.OnTerm(t, false)
	}
	return nil
}

func runDuty(ctx context.Context, d Duty, logf func(string, ...any)) {
	if d.Every <= 0 {
		if err := d.Run(ctx); err != nil && ctx.Err() == nil {
			logf("leader: %s: %v", d.Name, err)
		}
		return
	}
	t := time.NewTicker(d.Every)
	defer t.Stop()
	for {
		if err := d.Run(ctx); err != nil && ctx.Err() == nil {
			logf("leader: %s: %v", d.Name, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
