package postgres

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bwsalmon/silo/pkg/internal/pgtest"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, storetest.Backend{
		NewDB: func(t *testing.T) func() (store.Database, error) {
			srv := pgtest.Start(t)
			return func() (store.Database, error) { return Open(context.Background(), srv.URL) }
		},
	})
}

// named tags every connection a store makes with application_name, so
// a test can find its sessions in pg_stat_activity and kill them.
func named(name string) Option {
	return WithConfig(func(c *pgxpool.Config) { c.ConnConfig.RuntimeParams["application_name"] = name })
}

func openMigrated(t *testing.T, srv *pgtest.Server, opts ...Option) *Store {
	t.Helper()
	s, err := Open(context.Background(), srv.URL, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background(), s.Latest()); err != nil {
		t.Fatal(err)
	}
	return s
}

// kill terminates every session tagged name, once there are at least
// min of them, and reports how many it killed.
func kill(srv *pgtest.Server, name string, min int) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, srv.URL)
	if err != nil {
		return 0, err
	}
	defer admin.Close(ctx)
	for {
		var n int
		err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, name).Scan(&n)
		if err != nil {
			return 0, err
		}
		if n >= min {
			break
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("%d %s sessions, waiting for %d: %w", n, name, min, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	var n int
	err = admin.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE application_name = $1 AND pid <> pg_backend_pid()`, name).Scan(&n)
	return n, err
}

// TestLeaseMovesWhenHolderKilled: killing the holder's session frees
// the lock; the holder closes lost at its next failed ping, and a
// waiter in another process takes the lease at its next try (SV-55).
func TestLeaseMovesWhenHolderKilled(t *testing.T) {
	srv := pgtest.Start(t)
	holder := openMigrated(t, srv, named("holder"))
	waiter := openMigrated(t, srv, named("waiter"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	lost, err := holder.Lead(ctx, "leader")
	if err != nil {
		t.Fatal(err)
	}
	took := make(chan time.Time, 1)
	go func() {
		if _, err := waiter.Lead(ctx, "leader"); err != nil {
			t.Errorf("waiter's Lead: %v", err)
			return
		}
		took <- time.Now()
	}()
	select {
	case <-took:
		t.Fatal("the waiter took a lease that was held")
	case <-time.After(6 * time.Second): // past the waiter's first retry
	}

	killed := time.Now()
	if _, err := kill(srv, "holder", 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lost:
		if d := time.Since(killed); d > leasePing+leasePingLimit+time.Second {
			t.Errorf("lost closed %v after the holder's session was killed", d)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("lost not closed after the holder's session was killed")
	}
	select {
	case at := <-took:
		if d := at.Sub(killed); d > leaseRetry+2*time.Second {
			t.Errorf("the lease took %v to move", d)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the lease did not move after the holder's session was killed")
	}
}

// TestLeaseKeepalive pins the server-side keepalive on the lock
// connection: longer than a ping and its limit, so a holder that is
// cut off closes lost before Postgres frees the lock.
func TestLeaseKeepalive(t *testing.T) {
	srv := pgtest.Start(t)
	s := openMigrated(t, srv, named("holder"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Lead(ctx, "leader"); err != nil {
		t.Fatal(err)
	}
	conn, err := s.dial(ctx, leaseParams)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn(conn)
	var idleS string
	if err := conn.QueryRow(ctx, `SHOW tcp_keepalives_idle`).Scan(&idleS); err != nil {
		t.Fatal(err)
	}
	idle, _ := strconv.Atoi(idleS)
	if time.Duration(idle)*time.Second <= leasePing+leasePingLimit {
		t.Errorf("tcp_keepalives_idle = %ds, not longer than a ping and its limit", idle)
	}
	ns, key := lockKey("leader")
	var held int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
		AND classid = $1::int::oid AND objid = $2::int::oid AND granted`, ns, key).Scan(&held)
	if err != nil {
		t.Fatal(err)
	}
	if held != 1 {
		t.Errorf("%d granted advisory locks on the lease's key, want 1", held)
	}
}

// TestChangesSurviveDroppedConnections kills every session of a
// follower's store -- its listener and its pool -- while another store
// writes, and checks the follower still delivers every entry once, in
// order (SV-55).
func TestChangesSurviveDroppedConnections(t *testing.T) {
	srv := pgtest.Start(t)
	reader := openMigrated(t, srv, named("reader"))
	writer := openMigrated(t, srv, named("writer"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	ch, err := reader.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 60
	done := make(chan error, 1)
	go func() {
		for i := range n {
			var err error
			// A write that fails for a reason of the writer's own is
			// tried again; the writer's sessions are not killed, so it
			// only has to ride out its own retries.
			for range 5 {
				if err = writer.Tx(ctx, func(tx store.Tx) error {
					_, err := tx.Append(ctx, "test", "w", fmt.Sprint(i))
					return err
				}); err == nil {
					break
				}
			}
			if err != nil {
				done <- err
				return
			}
			if i%15 == 7 {
				// Its listener and at least one pooled connection.
				if _, err := kill(srv, "reader", 2); err != nil {
					done <- err
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		done <- nil
	}()

	for i := 1; i <= n; i++ {
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatalf("Changes closed after %d entries", i-1)
			}
			if c.Seq != uint64(i) || c.Data != fmt.Sprint(i-1) {
				t.Fatalf("entry %d is %+v: lost, duplicated or out of order", i, c)
			}
		case err := <-done:
			if err != nil {
				t.Fatalf("the writer: %v", err)
			}
			done <- nil
		case <-time.After(20 * time.Second):
			t.Fatalf("entry %d never arrived", i)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-ch:
		t.Errorf("an extra entry arrived: %+v", c)
	case <-time.After(1500 * time.Millisecond):
	}
}

// TestNotifyWakesOtherReplica: a commit on one store reaches a follower
// on another through NOTIFY, well inside the one-second poll.
func TestNotifyWakesOtherReplica(t *testing.T) {
	srv := pgtest.Start(t)
	reader := openMigrated(t, srv)
	writer := openMigrated(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ch, err := reader.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the listener connect
	var slow int
	for i := range 5 {
		time.Sleep(100 * time.Millisecond)
		start := time.Now()
		err := writer.Tx(ctx, func(tx store.Tx) error {
			_, err := tx.Append(ctx, "test", "k", fmt.Sprint(i))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("change %d never arrived", i)
		}
		if time.Since(start) > 400*time.Millisecond {
			slow++
		}
	}
	// One slow delivery could be a busy machine; all of them is the
	// poll doing NOTIFY's job.
	if slow > 1 {
		t.Errorf("%d of 5 commits took over 400ms to reach the other store: NOTIFY is not waking it", slow)
	}
}

// TestLockRows: LockRows takes FOR UPDATE locks, so a second
// transaction locking the same chain waits for the first to finish;
// it refuses a missing row and a table name that is not an identifier.
func TestLockRows(t *testing.T) {
	srv := pgtest.Start(t)
	s := openMigrated(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := s.Tx(ctx, func(tx store.Tx) error {
		for _, q := range []string{
			`CREATE TABLE locktest_silos (id TEXT PRIMARY KEY, parent TEXT NOT NULL)`,
			`INSERT INTO locktest_silos (id, parent) VALUES ('root', ''), ('a', 'root'), ('b', 'a')`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- s.Tx(ctx, func(tx store.Tx) error {
			if err := tx.LockRows(ctx, "locktest_silos", "id", "root", "a", "b"); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	var secondDone atomic.Bool
	second := make(chan error, 1)
	go func() {
		second <- s.Tx(ctx, func(tx store.Tx) error {
			err := tx.LockRows(ctx, "locktest_silos", "id", "root", "a")
			secondDone.Store(true)
			return err
		})
	}()
	time.Sleep(300 * time.Millisecond)
	if secondDone.Load() {
		t.Fatal("a second transaction locked the root while the first held it")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatalf("the second transaction, after waiting: %v", err)
	}

	err = s.Tx(ctx, func(tx store.Tx) error { return tx.LockRows(ctx, "locktest_silos", "id", "root", "nope") })
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("locking a missing row: %v", err)
	}
	err = s.Tx(ctx, func(tx store.Tx) error {
		return tx.LockRows(ctx, "locktest_silos; DROP TABLE locktest_silos", "id", "root")
	})
	if err == nil {
		t.Error("LockRows accepted a table name that is not an identifier")
	}
}

// TestNowIsTheDatabases: Tx.Now is now() read in SQL, the
// transaction's start, and the same throughout it.
func TestNowIsTheDatabases(t *testing.T) {
	srv := pgtest.Start(t)
	s := openMigrated(t, srv)
	ctx := context.Background()
	err := s.Tx(ctx, func(tx store.Tx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		var start int64
		if err := tx.QueryRow(ctx, `SELECT CAST(floor(EXTRACT(EPOCH FROM now()) * 1000) AS BIGINT)`).Scan(&start); err != nil {
			return err
		}
		if store.Millis(now) != start {
			t.Errorf("Now = %d, the transaction's now() = %d", store.Millis(now), start)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDialFuncSeam: every connection the store makes -- the pool's,
// the listener's and each lease's -- dials through WithDialFunc, which
// is where the Cloud SQL connector plugs in.
func TestDialFuncSeam(t *testing.T) {
	srv := pgtest.Start(t)
	var mu sync.Mutex
	dialed := 0
	addr := fmt.Sprintf("127.0.0.1:%d", srv.Port)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		mu.Lock()
		dialed++
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr) // the address asked for, port 1, is ignored
	}
	s := openMigrated(t, srv, WithDialFunc(dial),
		WithConfig(func(c *pgxpool.Config) { c.ConnConfig.Port = 1 }))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Lead(ctx, "leader"); err != nil {
		t.Fatal(err)
	}
	ch, err := s.Changes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Append(ctx, "k", "k", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	<-ch
	mu.Lock()
	defer mu.Unlock()
	// The migration lock, the pool, the lease and the listener.
	if dialed < 4 {
		t.Errorf("the dial func was used %d times, want every connection (at least 4)", dialed)
	}
}

func TestRebind(t *testing.T) {
	for in, want := range map[string]string{
		`SELECT 1`:                            `SELECT 1`,
		`SELECT ? , ?`:                        `SELECT $1 , $2`,
		`WHERE a = '?' AND b = ?`:             `WHERE a = '?' AND b = $1`,
		`WHERE a = 'it''s' AND b = ?`:         `WHERE a = 'it''s' AND b = $1`,
		`SELECT "?" FROM t WHERE x = ?`:       `SELECT "?" FROM t WHERE x = $1`,
		"-- isn't it?\nSELECT ?":              "-- isn't it?\nSELECT $1",
		`/* ? */ SELECT ?`:                    `/* ? */ SELECT $1`,
		`DO $x$ ? $x$; SELECT ?`:              `DO $x$ ? $x$; SELECT $1`,
		`SELECT $$?$$, ?`:                     `SELECT $$?$$, $1`,
		`SELECT 'unterminated ?`:              `SELECT 'unterminated ?`,
		`SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`: `SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10`,
	} {
		if got := rebind(in); got != want {
			t.Errorf("rebind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLockKeyFixed(t *testing.T) {
	ns, k := lockKey("leader")
	ns2, k2 := lockKey("leader")
	_, other := lockKey("reconcile")
	if ns != lockNamespace || ns2 != ns || k2 != k || other == k {
		t.Errorf("lockKey: leader=(%d,%d) again=(%d,%d) reconcile=%d", ns, k, ns2, k2, other)
	}
}
