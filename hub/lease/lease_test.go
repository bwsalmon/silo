package lease

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/pgtest"
)

// runner runs Run for one holder and records its terms.
type runner struct {
	holder  string
	holding atomic.Bool
	terms   atomic.Int64
	stop    context.CancelFunc
	done    chan struct{}
}

func start(t *testing.T, s *Store, holder string, both *atomic.Int64) *runner {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{holder: holder, stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		Run(ctx, Options{
			Store:  s,
			Name:   "dispatcher",
			Holder: holder,
			Retry:  50 * time.Millisecond,
			Run: func(ctx context.Context, tm Term) error {
				if both != nil {
					if n := both.Add(1); n > 1 {
						t.Errorf("%s: %d holders at once", holder, n)
					}
					defer both.Add(-1)
				}
				r.holding.Store(true)
				r.terms.Add(1)
				<-ctx.Done()
				r.holding.Store(false)
				return ctx.Err()
			},
		})
	}()
	t.Cleanup(func() { r.end() })
	return r
}

func (r *runner) end() {
	r.stop()
	<-r.done
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// handOver checks that of two runners one holds the lease, and that
// when it stops the other takes it, with a new epoch.
func handOver(t *testing.T, s1, s2 *Store) {
	var both atomic.Int64
	a := start(t, s1, "a", &both)
	eventually(t, "a to hold the lease", a.holding.Load)
	b := start(t, s2, "b", &both)
	time.Sleep(300 * time.Millisecond)
	if b.holding.Load() {
		t.Fatal("b holds the lease while a does")
	}
	epoch, holder, err := s1.Holder(context.Background(), "dispatcher")
	if err != nil || epoch != 1 || holder != "a" {
		t.Fatalf("Holder = %d, %q, %v; want 1, a", epoch, holder, err)
	}
	a.end()
	eventually(t, "b to take the lease", b.holding.Load)
	epoch, holder, err = s2.Holder(context.Background(), "dispatcher")
	if err != nil || epoch != 2 || holder != "b" {
		t.Fatalf("Holder = %d, %q, %v; want 2, b", epoch, holder, err)
	}
}

func TestSQLiteOneHolderAtATime(t *testing.T) {
	s, err := Open(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	handOver(t, s, s)
}

func TestPostgresOneHolderAtATime(t *testing.T) {
	pgtest.SkipOnSpanner(t, "the lease is a PostgreSQL advisory lock")
	pg := pgtest.Start(t)
	ctx := context.Background()
	s1, err := Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	handOver(t, s1, s2)
}

// A holder whose database session is killed gives the lease up at its
// next ping, and the other pod takes it.
func TestPostgresKilledSessionLosesTheLease(t *testing.T) {
	pgtest.SkipOnSpanner(t, "the lease is a PostgreSQL advisory lock")
	pg := pgtest.Start(t)
	ctx := context.Background()
	s1, err := Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// Not exclusive: a learns its session was killed at its next ping,
	// up to two seconds after b may have taken the lease (docs/hub.md
	// §2.2.1, "A stalled holder").
	a := start(t, s1, "a", nil)
	eventually(t, "a to hold the lease", a.holding.Load)
	b := start(t, s2, "b", nil)

	db, err := sql.Open("pgx", pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND classid::bigint = $1`, int64(uint32(lockNamespace))); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b to take the lease", b.holding.Load)
	eventually(t, "a to give it up", func() bool { return !a.holding.Load() })
	if a.terms.Load() != 1 {
		t.Fatalf("a took %d terms while b held the lease; want 1", a.terms.Load())
	}
}

// Racing Takes each get an epoch of their own: one that the database
// aborts for conflicting with another, as Spanner does, is run again.
func TestConcurrentTakes(t *testing.T) {
	pg := pgtest.Start(t)
	ctx := context.Background()
	s, err := Open(ctx, pg.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const n = 10
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		epochs = map[int64]bool{}
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			epoch, err := s.Take(ctx, "dispatcher", fmt.Sprint("pod-", i))
			if err != nil {
				t.Errorf("Take: %v", err)
				return
			}
			mu.Lock()
			epochs[epoch] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	epoch, _, err := s.Holder(ctx, "dispatcher")
	if err != nil {
		t.Fatal(err)
	}
	if len(epochs) != n || epoch != n {
		t.Fatalf("%d Takes got %d distinct epochs and left epoch %d; want %d and %d", n, len(epochs), epoch, n, n)
	}
}

// Closing the store ends the term.
func TestCloseEndsTheTerm(t *testing.T) {
	s, err := Open(context.Background(), "sqlite::memory:")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		events []bool
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Store: s, Name: "dispatcher", Retry: 10 * time.Millisecond,
			OnTerm: func(_ Term, started bool) {
				mu.Lock()
				events = append(events, started)
				mu.Unlock()
			},
			Run: func(ctx context.Context, _ Term) error { <-ctx.Done(); return nil },
		})
	}()
	eventually(t, "a term", func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 1 })
	s.Close()
	eventually(t, "the term to end", func() bool { mu.Lock(); defer mu.Unlock(); return len(events) >= 2 && !events[1] })
	cancel()
	<-done
}
