package leader_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/leader"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
)

func open(t *testing.T) store.Database {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background(), db.Latest()); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestSQLiteAlwaysLeads: on SQLite, whose one process is always the
// leader, Run takes the lease at once, bumps the epoch, and runs each
// duty under the term: once, or every period, until ctx ends.
func TestSQLiteAlwaysLeads(t *testing.T) {
	db := open(t)
	if db.Shared() {
		t.Fatal("sqlite reports itself shared")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ticks atomic.Int32
	terms := make(chan *leader.Term, 1)
	guarded := make(chan error, 1)
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- leader.Run(ctx, leader.Options{Store: db, ID: "only", Duties: []leader.Duty{
			{Name: "once", Run: func(ctx context.Context) error {
				terms <- leader.TermOf(ctx)
				guarded <- db.Tx(ctx, func(tx store.Tx) error { return leader.Guard(ctx, tx) })
				<-ctx.Done()
				close(stopped)
				return nil
			}},
			{Name: "tick", Every: 10 * time.Millisecond, Run: func(ctx context.Context) error {
				ticks.Add(1)
				return nil
			}},
		}})
	}()
	var term *leader.Term
	select {
	case term = <-terms:
	case <-time.After(10 * time.Second):
		t.Fatal("never became leader")
	}
	if term == nil || term.Epoch != 1 || term.Holder != "only" {
		t.Fatalf("term %+v, want epoch 1 held by only", term)
	}
	if err := <-guarded; err != nil {
		t.Errorf("the leader's own write: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for ticks.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ticks.Load() < 3 {
		t.Errorf("the periodic duty ran %d times", ticks.Load())
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the duty's context didn't end with Run's")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v", err)
	}
	n := ticks.Load()
	time.Sleep(50 * time.Millisecond)
	if ticks.Load() != n {
		t.Error("a duty ran after Run returned")
	}
}

// TestGuard: a write under a term passes while its epoch is current,
// and is refused once another holder has taken the lease, once the
// term's context has ended, and with no term at all.
func TestGuard(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	guard := func(ctx context.Context) error {
		return db.Tx(ctx, func(tx store.Tx) error { return leader.Guard(ctx, tx) })
	}
	if err := guard(ctx); !errors.Is(err, leader.ErrNotLeader) {
		t.Errorf("no term: %v", err)
	}
	e1, err := leader.Take(ctx, db, "a")
	if err != nil {
		t.Fatal(err)
	}
	a := leader.WithTerm(ctx, &leader.Term{Epoch: e1, Holder: "a"})
	if err := guard(a); err != nil {
		t.Errorf("the current term: %v", err)
	}
	ended, cancel := context.WithCancel(a)
	cancel()
	// The store refuses a transaction on an ended context before the
	// guard is asked; either way nothing is written.
	if err := guard(ended); err == nil {
		t.Error("an ended term's write went through")
	}
	e2, err := leader.Take(ctx, db, "b")
	if err != nil {
		t.Fatal(err)
	}
	if e2 != e1+1 {
		t.Errorf("epochs %d then %d", e1, e2)
	}
	if err := guard(a); !errors.Is(err, leader.ErrNotLeader) {
		t.Errorf("a stale term: %v, want ErrNotLeader", err)
	}
}
