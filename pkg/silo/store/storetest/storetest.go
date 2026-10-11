// Package storetest is the store conformance suite (docs/silo/state.md,
// "Testing the store", SV-55; docs/silo/testing.md, SV-120). Every
// backend runs it, unchanged, from its own tests:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, storetest.Backend{NewDB: ...})
//	}
//
// It is written against package store's interfaces alone, and checks
// each thing silod relies on the store for (SV-46): rollback,
// concurrent allocations that never over-allocate, serialization
// retries that leak nothing, a gap-free change log in commit order and
// its watch, lease exclusivity and hand-off, request_id replays,
// database time for every delay, and migrations from an empty database
// to the newest version. It also runs the quota ledger's hard
// partition against the backend (testLedgerPartition), since that is
// the allocation silod most relies on the store to keep exact, and the
// address allocator's carving (testAddressCarving), which must never
// hand one block out twice.
//
// The suite's toy tables are created by the tests themselves, through
// Tx, and are named storetest_*.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/migrations"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// Backend is how the suite gets databases from the backend under test.
type Backend struct {
	// NewDB creates a fresh, empty database (schema version 0) and
	// returns a function that opens a store on it. Each call to open
	// is a new handle, as a new silod process would get. NewDB cleans
	// the database up through t.Cleanup; the suite closes the handles
	// it opens.
	NewDB func(t *testing.T) (open func() (store.Database, error))
}

// Run runs the whole suite against b.
func Run(t *testing.T, b Backend) {
	s := &suite{b: b}
	for _, c := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"Rollback", s.testRollback},
		{"NoOverAllocation", s.testNoOverAllocation},
		{"Retry", s.testRetry},
		{"ChangeLog", s.testChangeLog},
		{"ChangesResume", s.testChangesResume},
		{"ChangesWake", s.testChangesWake},
		{"Lease", s.testLease},
		{"LeaseHandOff", s.testLeaseHandOff},
		{"RequestReplay", s.testRequestReplay},
		{"DatabaseTime", s.testDatabaseTime},
		{"Migrations", s.testMigrations},
		{"Upgrades", s.testUpgrades},
		{"Heartbeat", s.testHeartbeat},
		{"LedgerPartition", s.testLedgerPartition},
		{"AddressCarving", s.testAddressCarving},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.fn(t)
		})
	}
}

type suite struct{ b Backend }

// open opens a handle with open and closes it at the end of the test.
func open(t *testing.T, open func() (store.Database, error)) store.Database {
	t.Helper()
	db, err := open()
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fresh returns a migrated store on a new database, and the opener for
// more handles on it.
func (s *suite) fresh(t *testing.T) (store.Database, func() (store.Database, error)) {
	t.Helper()
	opener := s.b.NewDB(t)
	db := open(t, opener)
	if err := db.Migrate(ctx(t), db.Latest()); err != nil {
		t.Fatalf("migrating to %d: %v", db.Latest(), err)
	}
	return db, opener
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return c
}

func exec(t *testing.T, db store.Store, stmts ...string) {
	t.Helper()
	err := db.Tx(ctx(t), func(tx store.Tx) error {
		for _, q := range stmts {
			if _, err := tx.Exec(ctx(t), q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	err := db.Tx(ctx(t), func(tx store.Tx) error {
		return tx.QueryRow(ctx(t), query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func lastSeq(t *testing.T, db store.Store) uint64 {
	t.Helper()
	return uint64(count(t, db, `SELECT COALESCE(MAX(seq), 0) FROM changes`))
}

// recv takes n changes from ch, failing the test if they take longer
// than d.
func recv(t *testing.T, ch <-chan store.Change, n int, d time.Duration) []store.Change {
	t.Helper()
	var out []store.Change
	deadline := time.After(d)
	for len(out) < n {
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatalf("Changes closed after %d of %d entries", len(out), n)
			}
			out = append(out, c)
		case <-deadline:
			t.Fatalf("Changes delivered %d of %d entries in %v", len(out), n, d)
		}
	}
	return out
}

// testRollback: a failed fn, or one that panics, rolls back everything
// it did -- rows, change-log entries and request records -- and leaves
// no hole in the change log.
func (s *suite) testRollback(t *testing.T) {
	db, _ := s.fresh(t)
	exec(t, db, `CREATE TABLE storetest_rows (id TEXT PRIMARY KEY, n INTEGER NOT NULL)`)
	before := lastSeq(t, db)
	write := func(tx store.Tx) error {
		if _, err := tx.Exec(ctx(t), `INSERT INTO storetest_rows (id, n) VALUES (?, ?)`, "a", 1); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx(t), `INSERT INTO storetest_rows (id, n) VALUES (?, ?)`, "b", 2); err != nil {
			return err
		}
		if _, err := tx.Append(ctx(t), "row", "a", `{"n":1}`); err != nil {
			return err
		}
		return tx.RecordRequest(ctx(t), "req-rollback", "done")
	}
	check := func(how string) {
		t.Helper()
		if n := count(t, db, `SELECT COUNT(*) FROM storetest_rows`); n != 0 {
			t.Errorf("%s: %d rows survived", how, n)
		}
		if seq := lastSeq(t, db); seq != before {
			t.Errorf("%s: the change log moved from %d to %d", how, before, seq)
		}
		_ = db.Tx(ctx(t), func(tx store.Tx) error {
			if _, ok, err := tx.Request(ctx(t), "req-rollback"); ok || err != nil {
				t.Errorf("%s: request record survived (ok=%v err=%v)", how, ok, err)
			}
			return nil
		})
	}

	boom := errors.New("boom")
	err := db.Tx(ctx(t), func(tx store.Tx) error {
		if err := write(tx); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx returned %v, want fn's error", err)
	}
	check("fn returned an error")

	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Errorf("recovered %v, want fn's panic to propagate", r)
			}
		}()
		_ = db.Tx(ctx(t), func(tx store.Tx) error {
			if err := write(tx); err != nil {
				return err
			}
			panic("kaboom")
		})
	}()
	check("fn panicked")

	// The store is still usable, and the next entry follows on with no
	// hole where the rolled-back ones were.
	var seq uint64
	err = db.Tx(ctx(t), func(tx store.Tx) error {
		var err error
		seq, err = tx.Append(ctx(t), "row", "c", "")
		return err
	})
	if err != nil || seq != before+1 {
		t.Errorf("the next Append got seq %d, err %v; want %d", seq, err, before+1)
	}
}

var errFull = errors.New("no room")

// testNoOverAllocation: many goroutines carving blocks and charging
// quota under one parent never over-allocate, and never hand out one
// block twice. The transactions are written naively, read then write,
// with no locking of their own: serializable isolation alone has to
// make that safe.
func (s *suite) testNoOverAllocation(t *testing.T) {
	db, _ := s.fresh(t)
	const (
		limit   = 15
		blocks  = 20
		workers = 24
		tries   = 3
	)
	stmts := []string{
		`CREATE TABLE storetest_quota (parent TEXT PRIMARY KEY, lim INTEGER NOT NULL, used INTEGER NOT NULL)`,
		`CREATE TABLE storetest_blocks (block INTEGER PRIMARY KEY, parent TEXT NOT NULL, owner TEXT NOT NULL)`,
		fmt.Sprintf(`INSERT INTO storetest_quota (parent, lim, used) VALUES ('p', %d, 0)`, limit),
	}
	for b := 1; b <= blocks; b++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO storetest_blocks (block, parent, owner) VALUES (%d, 'p', '')`, b))
	}
	exec(t, db, stmts...)

	carve := func(owner string) error {
		return db.Tx(ctx(t), func(tx store.Tx) error {
			var lim, used int
			if err := tx.QueryRow(ctx(t), `SELECT lim, used FROM storetest_quota WHERE parent = ?`, "p").Scan(&lim, &used); err != nil {
				return err
			}
			if used >= lim {
				return errFull
			}
			var block int
			err := tx.QueryRow(ctx(t),
				`SELECT block FROM storetest_blocks WHERE parent = ? AND owner = '' ORDER BY block LIMIT 1`, "p").Scan(&block)
			if err != nil {
				return err
			}
			n, err := tx.Exec(ctx(t), `UPDATE storetest_blocks SET owner = ? WHERE block = ? AND owner = ''`, owner, block)
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("block %d was free when read and taken when written (%d rows): not serializable", block, n)
			}
			if _, err := tx.Exec(ctx(t), `UPDATE storetest_quota SET used = used + 1 WHERE parent = ?`, "p"); err != nil {
				return err
			}
			_, err = tx.Append(ctx(t), "block", fmt.Sprint(block), owner)
			return err
		})
	}

	var granted atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range tries {
				err := carve(fmt.Sprintf("child-%d-%d", w, i))
				switch {
				case err == nil:
					granted.Add(1)
				case errors.Is(err, errFull):
				default:
					t.Errorf("carve: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if g := granted.Load(); g != limit {
		t.Errorf("%d carves succeeded against a quota of %d", g, limit)
	}
	if used := count(t, db, `SELECT used FROM storetest_quota WHERE parent = 'p'`); used != limit {
		t.Errorf("quota used = %d, want %d", used, limit)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM storetest_blocks WHERE owner <> ''`); n != limit {
		t.Errorf("%d blocks have an owner, want %d", n, limit)
	}
	if n := count(t, db, `SELECT COUNT(DISTINCT owner) FROM storetest_blocks WHERE owner <> ''`); n != limit {
		t.Errorf("%d distinct owners, want %d: a block went to two children", n, limit)
	}
}

// testRetry: a serialization failure is retried with fn run again from
// the start, and fn sees nothing of an attempt that failed; a conflict
// that never clears gives up after store.MaxAttempts; any other error
// is not retried.
func (s *suite) testRetry(t *testing.T) {
	db, _ := s.fresh(t)
	exec(t, db, `CREATE TABLE storetest_rows (id TEXT PRIMARY KEY, n INTEGER NOT NULL)`)
	before := lastSeq(t, db)

	attempts := 0
	err := db.Tx(ctx(t), func(tx store.Tx) error {
		attempts++
		// Nothing from an earlier attempt is visible.
		var n int
		if err := tx.QueryRow(ctx(t), `SELECT COUNT(*) FROM storetest_rows`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("attempt %d sees %d rows from a failed attempt", attempts, n)
		}
		if _, ok, err := tx.Request(ctx(t), "req-retry"); ok || err != nil {
			t.Errorf("attempt %d sees the failed attempt's request record (err %v)", attempts, err)
		}
		if _, err := tx.Exec(ctx(t), `INSERT INTO storetest_rows (id, n) VALUES (?, ?)`, "x", attempts); err != nil {
			return err
		}
		seq, err := tx.Append(ctx(t), "row", "x", "")
		if err != nil {
			return err
		}
		if seq != before+1 {
			t.Errorf("attempt %d got seq %d, want %d: a failed attempt consumed a number", attempts, seq, before+1)
		}
		if err := tx.RecordRequest(ctx(t), "req-retry", "ok"); err != nil {
			return err
		}
		if attempts < 3 {
			return fmt.Errorf("simulated serialization failure: %w", store.ErrConflict)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if attempts != 3 {
		t.Errorf("fn ran %d times, want 3", attempts)
	}
	if n := count(t, db, `SELECT n FROM storetest_rows WHERE id = 'x'`); n != 3 {
		t.Errorf("the committed row came from attempt %d, want 3", n)
	}
	if seq := lastSeq(t, db); seq != before+1 {
		t.Errorf("change log at %d, want %d", seq, before+1)
	}

	attempts = 0
	err = db.Tx(ctx(t), func(tx store.Tx) error {
		attempts++
		return store.ErrConflict
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("a conflict that never clears returned %v, want ErrConflict", err)
	}
	if attempts != store.MaxAttempts {
		t.Errorf("a conflict that never clears ran fn %d times, want %d", attempts, store.MaxAttempts)
	}

	attempts = 0
	boom := errors.New("boom")
	if err := db.Tx(ctx(t), func(tx store.Tx) error { attempts++; return boom }); !errors.Is(err, boom) || attempts != 1 {
		t.Errorf("an ordinary error: Tx returned %v after %d attempts, want it once", err, attempts)
	}
}

type logged struct{ kind, key, data string }

// testChangeLog: under concurrent writers, some of whose transactions
// roll back, the change log has no gaps, is in commit order, holds only
// committed entries, and Changes delivers each of them once, to a
// reader that was following while they were written.
func (s *suite) testChangeLog(t *testing.T) {
	db, _ := s.fresh(t)
	const writers, txs = 8, 20
	before := lastSeq(t, db)

	c, cancel := context.WithCancel(ctx(t))
	defer cancel()
	ch, err := db.Changes(c, before)
	if err != nil {
		t.Fatal(err)
	}

	var want sync.Map // data -> struct{}: what committed
	var total atomic.Int64
	errSkip := errors.New("skip")
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range txs {
				var appended []string
				err := db.Tx(ctx(t), func(tx store.Tx) error {
					appended = appended[:0]
					parts := 1 + i%3
					for p := range parts {
						d := fmt.Sprintf("%d/%d/%d", w, i, p)
						if _, err := tx.Append(ctx(t), "test", fmt.Sprint(w), d); err != nil {
							return err
						}
						appended = append(appended, d)
					}
					if i%5 == 4 {
						return errSkip
					}
					return nil
				})
				switch {
				case err == nil:
					for _, d := range appended {
						want.Store(d, struct{}{})
					}
					total.Add(int64(len(appended)))
				case errors.Is(err, errSkip):
				default:
					t.Errorf("writer %d: %v", w, err)
				}
			}
		}()
	}
	wg.Wait()
	n := int(total.Load())
	got := recv(t, ch, n, 30*time.Second)

	lastPer := map[string][2]int{}
	for i, c := range got {
		if c.Seq != before+uint64(i)+1 {
			t.Fatalf("entry %d has seq %d, want %d: the log has a gap or is out of order", i, c.Seq, before+uint64(i)+1)
		}
		if c.Kind != "test" || c.At.IsZero() {
			t.Errorf("entry %d = %+v", c.Seq, c)
		}
		if _, ok := want.LoadAndDelete(c.Data); !ok {
			t.Errorf("entry %d (%s) was delivered twice, or never committed", c.Seq, c.Data)
		}
		var w, tx, p int
		if _, err := fmt.Sscanf(c.Data, "%d/%d/%d", &w, &tx, &p); err != nil || c.Key != fmt.Sprint(w) {
			t.Fatalf("entry %d: data %q key %q", c.Seq, c.Data, c.Key)
		}
		// Commit order: one writer's transactions in the order it ran
		// them, and each transaction's entries together and in order.
		prev, seen := lastPer[c.Key]
		switch {
		case p > 0:
			if i == 0 || got[i-1].Data != fmt.Sprintf("%d/%d/%d", w, tx, p-1) {
				t.Errorf("entry %d (%s) is not right after its transaction's previous entry", c.Seq, c.Data)
			}
		case seen && tx <= prev[0]:
			t.Errorf("entry %d (%s) follows writer %d's transaction %d", c.Seq, c.Data, w, prev[0])
		}
		lastPer[c.Key] = [2]int{tx, p}
	}
	want.Range(func(k, _ any) bool {
		t.Errorf("committed entry %s was never delivered", k)
		return true
	})
	if seq := lastSeq(t, db); seq != before+uint64(n) {
		t.Errorf("the log ends at %d, want %d", seq, before+uint64(n))
	}

	// Nothing more arrives: no duplicates trail the last entry.
	select {
	case c := <-ch:
		t.Errorf("an extra entry arrived: %+v", c)
	case <-time.After(300 * time.Millisecond):
	}

	// A reader that starts afterwards reads the same log from its
	// backlog.
	again, err := db.Changes(c, before)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range recv(t, again, n, 30*time.Second) {
		if c != got[i] {
			t.Fatalf("a second reader's entry %d is %+v, the first's %+v", i, c, got[i])
		}
	}
}

func appendN(t *testing.T, db store.Store, n int, prefix string) {
	t.Helper()
	for i := range n {
		err := db.Tx(ctx(t), func(tx store.Tx) error {
			_, err := tx.Append(ctx(t), "test", prefix, fmt.Sprintf("%s-%d", prefix, i))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// testChangesResume: a reader that drops -- its context ends, as when
// its connection goes -- and resumes from the last sequence number it
// handled gets each entry once: none twice, none missed, including
// entries committed while it was away. A closed store ends every
// reader.
func (s *suite) testChangesResume(t *testing.T) {
	db, opener := s.fresh(t)
	appendN(t, db, 10, "a")

	c1, cancel1 := context.WithCancel(ctx(t))
	ch1, err := db.Changes(c1, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := recv(t, ch1, 4, 10*time.Second)
	cancel1()
	// The channel closes once its context has ended.
	for range ch1 {
	}

	appendN(t, db, 5, "b") // committed while nobody was reading

	resume := first[len(first)-1].Seq
	c2, cancel2 := context.WithCancel(ctx(t))
	defer cancel2()
	ch2, err := db.Changes(c2, resume)
	if err != nil {
		t.Fatal(err)
	}
	rest := recv(t, ch2, 11, 10*time.Second)
	all := append(first, rest...)
	for i, c := range all {
		if c.Seq != uint64(i)+1 {
			t.Fatalf("after resuming, entry %d has seq %d, want %d", i, c.Seq, i+1)
		}
	}
	if rest[len(rest)-1].Data != "b-4" {
		t.Errorf("last entry = %+v, want b-4", rest[len(rest)-1])
	}

	// A reader asking for entries past the end waits for them.
	ch3, err := db.Changes(c2, 20)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, db, 6, "c") // seqs 16..21
	if got := recv(t, ch3, 1, 10*time.Second); got[0].Seq != 21 {
		t.Errorf("Changes(since=20) delivered seq %d first, want 21", got[0].Seq)
	}

	// Closing the store ends its readers.
	extra := db // only one handle may be open on a store that is not shared
	if db.Shared() {
		extra = open(t, opener)
	}
	ch4, err := extra.Changes(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := extra.Close(); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(10 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-ch4:
		case <-timeout:
			t.Fatal("Changes kept its channel open after the store closed")
		}
	}
}

// testChangesWake: a reader following the tail sees a commit promptly,
// within the poll backstop's second plus slack at worst.
func (s *suite) testChangesWake(t *testing.T) {
	db, _ := s.fresh(t)
	appendN(t, db, 1, "warm")
	c, cancel := context.WithCancel(ctx(t))
	defer cancel()
	ch, err := db.Changes(c, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		time.Sleep(50 * time.Millisecond) // let the reader go idle
		start := time.Now()
		appendN(t, db, 1, fmt.Sprintf("wake%d", i))
		got := recv(t, ch, 1, 5*time.Second)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("commit %d took %v to reach a follower", i, d)
		}
		if got[0].Seq != uint64(i)+2 {
			t.Errorf("follower got seq %d, want %d", got[0].Seq, i+2)
		}
	}
}

// testLease: at most one holder of a lease at a time; a holder's lost
// is closed before anyone else can take the lease; leases of different
// names are independent; a Lead whose context ends while it waits gives
// up; closing the store closes lost.
func (s *suite) testLease(t *testing.T) {
	db, opener := s.fresh(t)

	const workers, rounds = 6, 4
	var (
		mu      sync.Mutex
		prev    <-chan struct{}
		holders atomic.Int32
		held    atomic.Int32
		wg      sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				c, cancel := context.WithCancel(ctx(t))
				lost, err := db.Lead(c, "leader")
				if err != nil {
					t.Errorf("Lead: %v", err)
					cancel()
					return
				}
				if h := holders.Add(1); h != 1 {
					t.Errorf("%d holders of one lease", h)
				}
				held.Add(1)
				mu.Lock()
				if prev != nil {
					select {
					case <-prev:
					default:
						t.Error("took the lease before the previous holder's lost was closed")
					}
				}
				prev = lost
				mu.Unlock()
				select {
				case <-lost:
					t.Error("lost closed while the lease was still held")
				case <-time.After(2 * time.Millisecond):
				}
				holders.Add(-1)
				cancel()
				select {
				case <-lost:
				case <-time.After(10 * time.Second):
					t.Error("lost not closed after the holder's context ended")
				}
			}
		}()
	}
	wg.Wait()
	if h := held.Load(); h != workers*rounds {
		t.Errorf("the lease was held %d times, want %d", h, workers*rounds)
	}

	// Different names do not exclude each other.
	ca, cancelA := context.WithTimeout(ctx(t), 30*time.Second)
	defer cancelA()
	if _, err := db.Lead(ca, "a"); err != nil {
		t.Fatalf("Lead(a): %v", err)
	}
	if _, err := db.Lead(ca, "b"); err != nil {
		t.Fatalf("Lead(b) while a is held: %v", err)
	}

	// A waiter gives up when its context ends.
	cw, cancelW := context.WithTimeout(ctx(t), 200*time.Millisecond)
	defer cancelW()
	if _, err := db.Lead(cw, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Lead on a held lease with a short deadline returned %v, want DeadlineExceeded", err)
	}

	// Closing the store closes every lost.
	other := db
	if db.Shared() {
		other = open(t, opener)
	}
	lost, err := other.Lead(ctx(t), "closing")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lost:
	case <-time.After(15 * time.Second):
		t.Error("lost not closed after the store closed")
	}
}

// testLeaseHandOff: on a shared store the lease moves to another
// process when its holder's process goes, and the old holder's lost is
// closed before the new holder has it. On a store that is not shared,
// the second process is refused outright, and admitted once the first
// has gone.
func (s *suite) testLeaseHandOff(t *testing.T) {
	db, opener := s.fresh(t)
	if !db.Shared() {
		if second, err := opener(); err == nil {
			second.Close()
			t.Fatal("a second handle opened on a store that is not shared")
		} else if !errors.Is(err, store.ErrInUse) {
			t.Errorf("a second handle was refused with %v, want ErrInUse", err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		again := open(t, opener)
		if v, err := again.Schema(ctx(t)); err != nil || v != again.Latest() {
			t.Errorf("reopened at schema %d (%v), want %d", v, err, again.Latest())
		}
		return
	}

	second := open(t, opener)
	lost1, err := db.Lead(ctx(t), "leader")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan bool, 1)
	go func() {
		_, err := second.Lead(ctx(t), "leader")
		if err != nil {
			t.Errorf("second Lead: %v", err)
		}
		// When the second process takes the lease, the first's lost
		// must already be closed.
		select {
		case <-lost1:
			got <- true
		default:
			got <- false
		}
	}()
	select {
	case <-got:
		t.Fatal("a second process took a lease that was held")
	case <-time.After(500 * time.Millisecond):
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-got:
		if !ok {
			t.Error("the lease moved before the old holder's lost was closed")
		}
	case <-time.After(30 * time.Second):
		t.Error("the lease did not move when its holder's process went")
	}
}

// testRequestReplay: work done under a request_id is done once; a retry
// of the same request, sequential or concurrent, gets the first result.
func (s *suite) testRequestReplay(t *testing.T) {
	db, _ := s.fresh(t)
	exec(t, db, `CREATE TABLE storetest_work (n INTEGER PRIMARY KEY, req TEXT NOT NULL)`)
	do := func(id string) (string, error) {
		var result string
		err := db.Tx(ctx(t), func(tx store.Tx) error {
			r, ok, err := tx.Request(ctx(t), id)
			if err != nil || ok {
				result = r
				return err
			}
			var n int
			if err := tx.QueryRow(ctx(t), `SELECT COUNT(*) FROM storetest_work`).Scan(&n); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx(t), `INSERT INTO storetest_work (n, req) VALUES (?, ?)`, n+1, id); err != nil {
				return err
			}
			result = fmt.Sprintf("%s-did-%d", id, n+1)
			return tx.RecordRequest(ctx(t), id, result)
		})
		return result, err
	}

	r1, err := do("a")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := do("a")
	if err != nil || r2 != r1 {
		t.Errorf("replay of a: %q, %v; want %q", r2, err, r1)
	}
	rb, err := do("b")
	if err != nil || rb == r1 {
		t.Errorf("b: %q, %v; want its own result", rb, err)
	}

	const n = 16
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if results[i], err = do("c"); err != nil {
				t.Errorf("concurrent c: %v", err)
			}
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r != results[0] {
			t.Errorf("concurrent replay %d got %q, replay 0 %q", i, r, results[0])
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		if k := count(t, db, `SELECT COUNT(*) FROM storetest_work WHERE req = ?`, id); k != 1 {
			t.Errorf("request %s did its work %d times", id, k)
		}
	}

	err = db.Tx(ctx(t), func(tx store.Tx) error { return tx.RecordRequest(ctx(t), "a", "again") })
	if !errors.Is(err, store.ErrDuplicateRequest) {
		t.Errorf("recording a second result for a: %v, want ErrDuplicateRequest", err)
	}
	if r, _ := do("a"); r != r1 {
		t.Errorf("after the refused record, a = %q, want %q", r, r1)
	}
}

// testDatabaseTime: Tx.Now is the database's time, one value for a
// transaction, never going backwards; the store stamps what it writes
// with it; and a delay measured against it elapses.
func (s *suite) testDatabaseTime(t *testing.T) {
	db, _ := s.fresh(t)
	exec(t, db, `CREATE TABLE storetest_delay (id TEXT PRIMARY KEY, ready_at BIGINT NOT NULL)`)

	var now1 time.Time
	var seq uint64
	err := db.Tx(ctx(t), func(tx store.Tx) error {
		var err error
		if now1, err = tx.Now(ctx(t)); err != nil {
			return err
		}
		if seq, err = tx.Append(ctx(t), "tick", "t", ""); err != nil {
			return err
		}
		if err := tx.RecordRequest(ctx(t), "req-time", "x"); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
		again, err := tx.Now(ctx(t))
		if err != nil {
			return err
		}
		if !again.Equal(now1) {
			t.Errorf("Now moved within one transaction: %v then %v", now1, again)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if now1.Location() != time.UTC {
		t.Errorf("Now is in %v, want UTC", now1.Location())
	}
	// The database and this test share a machine in CI, so this is a
	// sanity bound on the units, not a claim about whose clock it is.
	if d := time.Since(now1); d < -time.Hour || d > time.Hour {
		t.Errorf("Now = %v, %v from this process's clock: wrong units?", now1, d)
	}

	c, cancel := context.WithCancel(ctx(t))
	defer cancel()
	ch, err := db.Changes(c, seq-1)
	if err != nil {
		t.Fatal(err)
	}
	if got := recv(t, ch, 1, 10*time.Second)[0]; !got.At.Equal(now1) {
		t.Errorf("the change was stamped %v, its transaction's Now was %v", got.At, now1)
	}
	if at := count(t, db, `SELECT at FROM requests WHERE request_id = ?`, "req-time"); int64(at) != store.Millis(now1) {
		t.Errorf("the request record was stamped %d, its transaction's Now was %d", at, store.Millis(now1))
	}

	// Never backwards, transaction to transaction.
	prev := now1
	for range 5 {
		var n time.Time
		if err := db.Tx(ctx(t), func(tx store.Tx) error { var err error; n, err = tx.Now(ctx(t)); return err }); err != nil {
			t.Fatal(err)
		}
		if n.Before(prev) {
			t.Errorf("Now went backwards: %v after %v", n, prev)
		}
		prev = n
	}

	// A delay: ready 300ms after it was set, by the database's clock.
	const delay = 300 * time.Millisecond
	var set time.Time
	err = db.Tx(ctx(t), func(tx store.Tx) error {
		var err error
		if set, err = tx.Now(ctx(t)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx(t), `INSERT INTO storetest_delay (id, ready_at) VALUES (?, ?)`,
			"d", store.Millis(set.Add(delay)))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ready := func() (bool, time.Time) {
		var n int
		var at time.Time
		err := db.Tx(ctx(t), func(tx store.Tx) error {
			var err error
			if at, err = tx.Now(ctx(t)); err != nil {
				return err
			}
			return tx.QueryRow(ctx(t), `SELECT COUNT(*) FROM storetest_delay WHERE ready_at <= ?`, store.Millis(at)).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n == 1, at
	}
	if ok, at := ready(); ok && at.Sub(set) < delay {
		t.Fatalf("ready at once (%v after it was set)", at.Sub(set))
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, at := ready()
		if ok {
			if at.Sub(set) < delay {
				t.Errorf("ready %v after it was set, before its %v delay", at.Sub(set), delay)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a 300ms delay never elapsed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// testMigrations: an empty database is at version 0; migrations apply
// one version at a time or all at once to the newest, each recorded;
// they only go forward; and a migrated store is usable and keeps its
// version across a reopen.
func (s *suite) testMigrations(t *testing.T) {
	opener := s.b.NewDB(t)
	db := open(t, opener)
	if v, err := db.Schema(ctx(t)); err != nil || v != 0 {
		t.Fatalf("an empty database is at schema %d (%v), want 0", v, err)
	}
	latest := db.Latest()
	if latest < 1 {
		t.Fatalf("Latest = %d", latest)
	}
	if err := db.Migrate(ctx(t), 0); err != nil {
		t.Errorf("Migrate(0) on an empty database: %v", err)
	}
	for v := 1; v <= latest; v++ {
		if err := db.Migrate(ctx(t), v); err != nil {
			t.Fatalf("Migrate(%d): %v", v, err)
		}
		if got, err := db.Schema(ctx(t)); err != nil || got != v {
			t.Fatalf("after Migrate(%d), Schema = %d (%v)", v, got, err)
		}
	}
	if err := db.Migrate(ctx(t), latest); err != nil {
		t.Errorf("Migrate to the version already applied: %v", err)
	}
	if err := db.Migrate(ctx(t), latest-1); err == nil {
		t.Error("Migrate went backwards")
	}
	if err := db.Migrate(ctx(t), latest+1); err == nil {
		t.Error("Migrate went past the newest version this binary carries")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM schema_migrations`); n != latest {
		t.Errorf("schema_migrations has %d rows, want %d", n, latest)
	}
	appendN(t, db, 1, "migrated")

	// All at once, on another empty database, lands in the same place.
	direct := open(t, s.b.NewDB(t))
	if err := direct.Migrate(ctx(t), direct.Latest()); err != nil {
		t.Fatalf("Migrate(%d) from empty: %v", direct.Latest(), err)
	}
	if v, err := direct.Schema(ctx(t)); err != nil || v != latest {
		t.Errorf("migrated straight to %d (%v), want %d", v, err, latest)
	}
	appendN(t, direct, 1, "direct")

	// The version survives a reopen.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	re := open(t, opener)
	if v, err := re.Schema(ctx(t)); err != nil || v != latest {
		t.Errorf("reopened at schema %d (%v), want %d", v, err, latest)
	}
	if !strings.Contains(fmt.Sprint(re.Migrate(ctx(t), 0)), "forward") {
		t.Error("Migrate(0) on a migrated database did not say migrations only go forward")
	}
}

// createTable finds the tables a migration creates.
var createTable = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)

// tablesAt is every table the schema at version v has, from the
// migrations' own SQL (the backends' files create the same tables).
func tablesAt(t *testing.T, v int) []string {
	t.Helper()
	ms, err := migrations.Load("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms[:v] {
		for _, c := range createTable.FindAllStringSubmatch(m.SQL, -1) {
			out = append(out, strings.ToLower(c[1]))
		}
	}
	return out
}

// columns is each of tables' columns, as SELECT * reports them. when
// tags the query's text: Postgres refuses a cached SELECT * plan whose
// columns an ALTER TABLE has since changed (0A000), so the read after an
// upgrade must not reuse the statement the read before it prepared.
func columns(t *testing.T, db store.Store, tables []string, when string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, tbl := range tables {
		err := db.Tx(ctx(t), func(tx store.Tx) error {
			rows, err := tx.Query(ctx(t), `SELECT * FROM `+tbl+` LIMIT 0 -- `+when)
			if err != nil {
				return err
			}
			defer rows.Close()
			cols, err := rows.Columns()
			out[tbl] = cols
			return err
		})
		if err != nil {
			t.Fatalf("reading %s's columns: %v", tbl, err)
		}
	}
	return out
}

// testUpgrades migrates the schema from every earlier version to the
// newest, and checks expand-then-contract (state.md "Schema migrations
// and upgrades", SV-53): every table and column the previous release's
// queries could name at version v is still there at the newest, unless
// a contract step in between took it away, and the queries the store
// itself runs still work on the upgraded database.
func (s *suite) testUpgrades(t *testing.T) {
	latest := open(t, s.b.NewDB(t)).Latest()
	ms, err := migrations.Load("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for v := 0; v < latest; v++ {
		t.Run(fmt.Sprintf("from-%d", v), func(t *testing.T) {
			db := open(t, s.b.NewDB(t))
			if err := db.Migrate(ctx(t), v); err != nil {
				t.Fatalf("Migrate(%d): %v", v, err)
			}
			tables := tablesAt(t, v)
			before := columns(t, db, tables, "before")
			// The previous release writes as it did before the upgrade,
			// on the schema it knows.
			if v >= 1 {
				appendN(t, db, 2, "before")
			}
			if err := db.Migrate(ctx(t), latest); err != nil {
				t.Fatalf("Migrate(%d) from %d: %v", latest, v, err)
			}
			if got, err := db.Schema(ctx(t)); err != nil || got != latest {
				t.Fatalf("after upgrading from %d, Schema = %d (%v)", v, got, err)
			}
			contracted := false
			for _, m := range ms[v:latest] {
				contracted = contracted || m.Requires > 0
			}
			if !contracted {
				after := columns(t, db, tables, "after")
				for _, tbl := range tables {
					have := map[string]bool{}
					for _, c := range after[tbl] {
						have[c] = true
					}
					for _, c := range before[tbl] {
						if !have[c] {
							t.Errorf("upgrading from %d took away %s.%s with no contract step: release %d's queries would break", v, tbl, c, v)
						}
					}
					// The previous release's reads name the columns it
					// knew, and still run.
					if len(before[tbl]) > 0 {
						exec(t, db, `SELECT `+strings.Join(before[tbl], ", ")+` FROM `+tbl+` WHERE 1 = 0`)
					}
				}
			}
			// And the store's own queries run on the upgraded schema.
			appendN(t, db, 2, "after")
			if v >= 1 {
				if n := count(t, db, `SELECT COUNT(*) FROM changes`); n != 4 {
					t.Errorf("the change log has %d entries across the upgrade, want 4", n)
				}
			}
			requires, err := db.Contracts(ctx(t))
			if err != nil {
				t.Fatal(err)
			}
			if why := store.SchemaCheck(latest, requires, migrations.Oldest, latest); why != "" {
				t.Errorf("this release won't serve the schema it migrated to: %s", why)
			}
		})
	}
}

// testHeartbeat: a replica's heartbeat is written, rewritten and
// removed, and settings round-trip.
func (s *suite) testHeartbeat(t *testing.T) {
	db, _ := s.fresh(t)
	r := store.Replica{ID: "vm-1", Release: "v1.0.0", Latest: db.Latest(), Started: time.Now()}
	for i := 0; i < 2; i++ {
		if err := store.Heartbeat(ctx(t), db, r); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM replicas WHERE id = ? AND schema_latest = ?`, "vm-1", db.Latest()); n != 1 {
		t.Errorf("%d heartbeat rows, want 1", n)
	}
	if err := store.Depart(ctx(t), db, r.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM replicas`); n != 0 {
		t.Errorf("%d heartbeat rows after Depart", n)
	}
	if _, ok, err := store.GetSetting(ctx(t), db, store.SettingReconcile); err != nil || ok {
		t.Errorf("an unset setting: %v, %v", ok, err)
	}
	for _, v := range []string{"report-only", "apply"} {
		if err := store.SetSetting(ctx(t), db, store.SettingReconcile, v); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := store.GetSetting(ctx(t), db, store.SettingReconcile); err != nil || !ok || got != v {
			t.Errorf("setting = %q, %v, %v; want %q", got, ok, err, v)
		}
	}
}

// testLedgerPartition: many goroutines charging one silo's quota and
// raising its siblings' grants out of one parent, through package
// ledger, never get past delegated + used <= granted, and leave books
// that add up (docs/silo/allocators.md, "Hard partition", SV-31).
func (s *suite) testLedgerPartition(t *testing.T) {
	db, _ := s.fresh(t)
	vcpu := ledger.Key{Kind: "gce", Class: "vcpu", Region: "us-central1"}
	const (
		capacity = 50 // vCPUs at the root
		workers  = 16
		tries    = 8
	)
	var parent string
	var kids []string
	err := db.Tx(ctx(t), func(tx store.Tx) error {
		root, err := tree.CreateRoot(ctx(t), tx, "root", nil)
		if err != nil {
			return err
		}
		if err := ledger.SetRegions(ctx(t), tx, root.ID, []string{"us-central1"}); err != nil {
			return err
		}
		if err := ledger.SetCapacity(ctx(t), tx, root.ID, []ledger.Amount{{Key: vcpu, Amount: ledger.Units(capacity)}}); err != nil {
			return err
		}
		parent = root.ID
		for i := range 4 {
			k, err := tree.CreateChild(ctx(t), tx, root.ID, fmt.Sprintf("k%d", i), tree.BackingSubnet, nil)
			if err != nil {
				return err
			}
			if err := ledger.SetRegions(ctx(t), tx, k.ID, []string{"us-central1"}); err != nil {
				return err
			}
			kids = append(kids, k.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Half the workers charge the root a vCPU at a time; the other half
	// raise a child's grant by one. Together they ask for more than
	// there is, so exactly capacity of them succeed.
	var won atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range tries {
				err := db.Tx(ctx(t), func(tx store.Tx) error {
					if w%2 == 0 {
						return ledger.Charge(ctx(t), tx, ledger.ChargeRequest{Object: fmt.Sprintf("vm-%d-%d", w, i),
							Silo: parent, Via: ledger.ViaFacade, Amounts: []ledger.Amount{{Key: vcpu, Amount: ledger.Units(1)}}})
					}
					kid := kids[(w/2)%len(kids)]
					r, err := ledger.Get(ctx(t), tx, kid, vcpu)
					if err != nil {
						return err
					}
					return ledger.Grant(ctx(t), tx, kid, []ledger.Amount{{Key: vcpu, Amount: r.Granted + ledger.Units(1)}})
				})
				switch {
				case err == nil:
					won.Add(1)
				case errors.Is(err, ledger.ErrQuotaExceeded):
				default:
					t.Errorf("worker %d: %v", w, err)
				}
			}
		}()
	}
	wg.Wait()

	if n := won.Load(); n != capacity {
		t.Errorf("%d charges and grants succeeded against a capacity of %d", n, capacity)
	}
	err = db.Tx(ctx(t), func(tx store.Tx) error {
		r, err := ledger.Get(ctx(t), tx, parent, vcpu)
		if err != nil {
			return err
		}
		if r.Delegated+r.Used != ledger.Units(capacity) || r.Free() != 0 {
			t.Errorf("root: delegated %d + used %d, granted %d", r.Delegated, r.Used, r.Granted)
		}
		bad, err := ledger.Check(ctx(t), tx)
		for _, b := range bad {
			t.Error(b)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// testAddressCarving: many goroutines carving blocks for their own
// children out of one parent, through package address, never hand out
// one range twice or overlapping, and take exactly what there is
// (docs/silo/allocators.md, "Every silo's space map", SV-39).
func (s *suite) testAddressCarving(t *testing.T) {
	db, _ := s.fresh(t)
	// A /16 less the gateway and control /24s at its top leaves fifteen
	// free /20s.
	const (
		workers = 8
		free    = 15
	)
	var kids []string
	err := db.Tx(ctx(t), func(tx store.Tx) error {
		root, err := tree.CreateRoot(ctx(t), tx, "root", nil)
		if err != nil {
			return err
		}
		if err := address.InitRoot(ctx(t), tx, root.ID, netip.MustParsePrefix("10.0.0.0/16"),
			netip.MustParsePrefix("10.0.255.0/24"), netip.MustParsePrefix("10.0.254.0/24")); err != nil {
			return err
		}
		for i := range workers {
			k, err := tree.CreateChild(ctx(t), tx, root.ID, fmt.Sprintf("k%d", i), tree.BackingSubnet, nil)
			if err != nil {
				return err
			}
			kids = append(kids, k.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var won atomic.Int64
	var wg sync.WaitGroup
	for _, kid := range kids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range address.DefaultBlockCap {
				err := db.Tx(ctx(t), func(tx store.Tx) error {
					_, err := address.Delegate(ctx(t), tx, kid, address.Want{Bits: 20})
					return err
				})
				switch {
				case err == nil:
					won.Add(1)
				case errors.Is(err, ledger.ErrResourceExhausted):
				default:
					t.Errorf("carving for %s: %v", kid, err)
				}
			}
		}()
	}
	wg.Wait()

	if n := won.Load(); n != free {
		t.Errorf("%d /20s carved from a parent with %d free", n, free)
	}
	err = db.Tx(ctx(t), func(tx store.Tx) error {
		tab, err := address.Snapshot(ctx(t), tx)
		if err != nil {
			return err
		}
		var carved []netip.Prefix
		for _, e := range tab.Blocks {
			if e.Range.Bits() == 20 {
				carved = append(carved, e.Range)
			}
		}
		if len(carved) != free {
			t.Errorf("%d /20 blocks recorded, want %d: %v", len(carved), free, tab.Blocks)
		}
		for i, a := range carved {
			for _, b := range carved[:i] {
				if a.Overlaps(b) {
					t.Errorf("blocks %v and %v overlap", a, b)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
