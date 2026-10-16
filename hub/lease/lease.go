// Package lease runs a piece of the hub on one pod at a time, under a
// named lease in the hub's database (docs/hub.md §2.2.1, stage R1). The
// dispatcher is its one user: every pod runs Run for the lease
// "dispatcher", and runs the dispatcher only while it holds it, so
// that two pods never recover or place the same task.
//
// It is v1's pkg/leader and its PostgreSQL Lead (this repository's
// history before 689c3a2), cut down to one duty.
//
// On PostgreSQL the lease is a session-level advisory lock on the
// name's fixed key, held on a connection of its own: a waiter tries it
// every Retry, the holder pings its connection every two seconds and
// gives the lease up on the first ping that fails or takes longer than
// two seconds, and PostgreSQL frees the lock when the holder's session
// ends. On SQLite, which is one process, a lease is a semaphore in the
// Store.
//
// Each hold of the lease is a term. Taking it bumps the lease's epoch,
// a row in hub_leases that also names the holder, so terms are
// numbered and the holder is visible in the database.
package lease

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/hub/dbaccess"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite"
)

// The lease's timings on PostgreSQL.
const (
	// DefaultRetry is how often a pod that does not hold a lease asks
	// for it again.
	DefaultRetry = 5 * time.Second
	// pingEvery is how often the holder checks its connection, and
	// pingLimit how long a check may take before the lease is given up.
	pingEvery = 2 * time.Second
	pingLimit = 2 * time.Second
	// keepalive is the lease connection's TCP keepalive idle time, on
	// both ends: PostgreSQL frees a vanished holder's lock no sooner
	// than this, then three probes two seconds apart, by which time the
	// holder's own pings have told it the lease is gone.
	keepalive = 10 * time.Second
)

// lockNamespace is the first half of every advisory lock key, so the
// hub's locks stay apart from anything else using advisory locks on the
// same database. It is "silo" in ASCII.
const lockNamespace int32 = 0x73696c6f

// lockKey is the fixed advisory lock key of a lease name: the silo
// namespace, and the name's 32-bit FNV-1a hash.
func lockKey(name string) (int32, int32) {
	h := fnv.New32a()
	h.Write([]byte(name))
	return lockNamespace, int32(h.Sum32())
}

// ErrClosed is a lease asked of a closed Store.
var ErrClosed = errors.New("lease: the store is closed")

// Store holds leases in the hub's database.
type Store struct {
	db       *sql.DB
	postgres bool
	pgConfig *pgx.ConnConfig

	ctx     context.Context
	cancel  context.CancelFunc
	running sync.WaitGroup

	mu   sync.Mutex
	sems map[string]chan struct{}
}

// Open opens the lease store at url, as the hub's other stores take it:
// "postgres://..." (or "postgresql://...") for PostgreSQL, "sqlite:PATH"
// or a bare path for SQLite. It creates hub_leases if it is missing.
func Open(ctx context.Context, url string) (*Store, error) { return open(ctx, url, true) }

// OpenExisting opens the lease store at url as Open does, but creates
// nothing: hub_leases was made by Open or `silo hub migrate` as a role
// that may create it, and url's role need only read and write its rows
// (docs/deploy.md). It fails if the table is missing.
func OpenExisting(ctx context.Context, url string) (*Store, error) { return open(ctx, url, false) }

func open(ctx context.Context, url string, create bool) (*Store, error) {
	s := &Store{sems: map[string]chan struct{}{}}
	s.postgres = strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://")
	var err error
	if s.postgres {
		if s.pgConfig, err = pgx.ParseConfig(url); err != nil {
			return nil, fmt.Errorf("lease: %w", err)
		}
		for k, v := range map[string]string{
			"tcp_keepalives_idle":     strconv.Itoa(int(keepalive / time.Second)),
			"tcp_keepalives_interval": "2",
			"tcp_keepalives_count":    "3",
		} {
			s.pgConfig.RuntimeParams[k] = v
		}
		d := &net.Dialer{KeepAlive: keepalive}
		s.pgConfig.DialFunc = d.DialContext
		s.db, err = sql.Open("pgx", url)
	} else {
		path := strings.TrimPrefix(url, "sqlite:")
		s.db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		s.db.SetMaxOpenConns(1)
	}
	if err != nil {
		return nil, err
	}
	if !create {
		if err := dbaccess.Check(ctx, s.db, "lease", "hub_leases"); err != nil {
			s.db.Close()
			return nil, err
		}
	} else if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS hub_leases (
	name TEXT PRIMARY KEY,
	epoch BIGINT NOT NULL,
	holder TEXT NOT NULL,
	taken_at BIGINT NOT NULL
)`); err != nil {
		s.db.Close()
		return nil, fmt.Errorf("lease: creating tables: %w", err)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// Close gives up every lease the store holds, waits for their
// connections to close, and closes the store.
func (s *Store) Close() error {
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.running.Wait()
	return s.db.Close()
}

func (s *Store) q(query string) string {
	if !s.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, c := range query {
		if c == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Lead waits until it holds the lease name, ctx ends or the store
// closes, trying every retry. It returns lost, which is closed when
// the lease is lost: ctx ended, the store closed or, on PostgreSQL,
// the lease's connection failed. The lease is released after lost is
// closed.
//
// Within one Store, callers for a name queue on a semaphore first, so
// only one of them at a time asks the database, and one handed the
// lease by its own process takes it at once.
func (s *Store) Lead(ctx context.Context, name string, retry time.Duration) (<-chan struct{}, error) {
	if retry <= 0 {
		retry = DefaultRetry
	}
	s.mu.Lock()
	sem, ok := s.sems[name]
	if !ok {
		sem = make(chan struct{}, 1)
		s.sems[name] = sem
	}
	s.mu.Unlock()
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, ErrClosed
	}
	var conn *pgx.Conn
	if s.postgres {
		var err error
		if conn, err = s.acquire(ctx, name, retry); err != nil {
			<-sem
			return nil, err
		}
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		if conn != nil {
			closeConn(conn)
		}
		<-sem
		return nil, ErrClosed
	}
	s.running.Add(1)
	s.mu.Unlock()
	lost := make(chan struct{})
	go func() {
		defer s.running.Done()
		defer func() { <-sem }()
		var tick <-chan time.Time
		if conn != nil {
			t := time.NewTicker(pingEvery)
			defer t.Stop()
			tick = t.C
		}
	hold:
		for {
			select {
			case <-ctx.Done():
				break hold
			case <-s.ctx.Done():
				break hold
			case <-tick:
				pctx, cancel := context.WithTimeout(s.ctx, pingLimit)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					break hold
				}
			}
		}
		close(lost)
		if conn == nil {
			return
		}
		// Closing the session releases the lock. Unlocking first lets the
		// next holder in without waiting on a close that might hang on a
		// dead connection.
		uctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ns, key := lockKey(name)
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1, $2)`, ns, key)
		cancel()
		closeConn(conn)
	}()
	return lost, nil
}

// acquire tries the lease's lock on a connection of its own, every
// retry, until it holds it, ctx ends or the store closes. A failure to
// connect or to ask is retried the same way.
func (s *Store) acquire(ctx context.Context, name string, retry time.Duration) (*pgx.Conn, error) {
	ns, key := lockKey(name)
	var conn *pgx.Conn
	for {
		if conn == nil {
			conn, _ = pgx.ConnectConfig(ctx, s.pgConfig.Copy())
		}
		if conn != nil {
			var got bool
			err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, ns, key).Scan(&got)
			if err == nil && got {
				return conn, nil
			}
			if err != nil {
				closeConn(conn)
				conn = nil
			}
		}
		select {
		case <-ctx.Done():
		case <-s.ctx.Done():
		case <-time.After(retry):
			continue
		}
		if conn != nil {
			closeConn(conn)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
}

func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

// Take bumps the epoch of the lease name for holder, which has just
// taken it, and returns the new epoch.
func (s *Store) Take(ctx context.Context, name, holder string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	epoch, err := current(ctx, tx, s.q, name)
	if err != nil {
		return 0, err
	}
	epoch++
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO hub_leases (name, epoch, holder, taken_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET epoch = excluded.epoch, holder = excluded.holder, taken_at = excluded.taken_at`),
		name, epoch, holder, time.Now().UnixMilli()); err != nil {
		return 0, err
	}
	return epoch, tx.Commit()
}

// Holder is the lease name's current epoch and the holder that took
// it: 0 and "" if nobody ever has. The holder may since have lost it.
func (s *Store) Holder(ctx context.Context, name string) (int64, string, error) {
	var (
		epoch  int64
		holder string
	)
	err := s.db.QueryRowContext(ctx, s.q(`SELECT epoch, holder FROM hub_leases WHERE name = ?`), name).Scan(&epoch, &holder)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return epoch, holder, err
}

func current(ctx context.Context, tx *sql.Tx, q func(string) string, name string) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, q(`SELECT epoch FROM hub_leases WHERE name = ?`), name).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Term is one hold of a lease, from taking it to losing it.
type Term struct {
	Name   string
	Epoch  int64
	Holder string
}

// Options configure Run.
type Options struct {
	Store *Store
	// Name is the lease's name.
	Name string
	// Holder names this pod, recorded with each epoch it takes;
	// DefaultHolder when empty.
	Holder string
	// Run is the work done while the lease is held. Its context ends
	// when the lease is lost, and the lease is not asked for again
	// until it has returned.
	Run func(ctx context.Context, t Term) error
	// Retry is how often the lease is asked for, and how long to wait
	// after a term that failed; DefaultRetry when zero.
	Retry time.Duration
	// OnTerm, when set, is called as each term starts and after it ends.
	OnTerm func(t Term, started bool)
	// Logf, when set, receives a line per term.
	Logf func(format string, args ...any)
}

// DefaultHolder is the host's name (a pod's name, on Kubernetes) and a
// random suffix, so that two processes on one host are told apart.
func DefaultHolder() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "hub"
	}
	b := make([]byte, 3)
	rand.Read(b)
	return host + "-" + hex.EncodeToString(b)
}

// Run takes the lease whenever it can and runs o.Run while it holds
// it, until ctx ends. It returns ctx's error once the last term's Run
// has returned.
func Run(ctx context.Context, o Options) error {
	if o.Holder == "" {
		o.Holder = DefaultHolder()
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	retry := o.Retry
	if retry <= 0 {
		retry = DefaultRetry
	}
	for ctx.Err() == nil {
		if err := term(ctx, o, retry, logf); err != nil && ctx.Err() == nil {
			logf("lease %s: %v", o.Name, err)
			select {
			case <-ctx.Done():
			case <-time.After(retry):
			}
		}
	}
	return ctx.Err()
}

// term holds the lease once, from taking it to losing it.
func term(ctx context.Context, o Options, retry time.Duration, logf func(string, ...any)) error {
	hold, release := context.WithCancel(ctx)
	defer release()
	lost, err := o.Store.Lead(hold, o.Name, retry)
	if err != nil {
		return err
	}
	epoch, err := o.Store.Take(hold, o.Name, o.Holder)
	if err != nil {
		return fmt.Errorf("taking the epoch: %w", err)
	}
	t := Term{Name: o.Name, Epoch: epoch, Holder: o.Holder}
	tctx, stop := context.WithCancel(hold)
	defer stop()
	go func() {
		select {
		case <-lost:
			stop()
		case <-tctx.Done():
		}
	}()
	logf("lease %s: %s holds it, epoch %d", o.Name, o.Holder, epoch)
	if o.OnTerm != nil {
		o.OnTerm(t, true)
	}
	err = o.Run(tctx, t)
	select {
	case <-lost:
		if ctx.Err() == nil {
			logf("lease %s: %s lost it (epoch %d)", o.Name, o.Holder, epoch)
		}
	default:
	}
	// Give the lease up before saying the term is over.
	release()
	<-lost
	if o.OnTerm != nil {
		o.OnTerm(t, false)
	}
	if err != nil && tctx.Err() == nil {
		return fmt.Errorf("epoch %d: %w", epoch, err)
	}
	return nil
}
