// Package postgres is silod's PostgreSQL store, --store=postgres, the
// default: every real install, on a Cloud SQL instance, shared by any
// number of silod replicas (docs/silo/state.md, SV-45).
//
// How it provides what silod relies on (SV-46):
//
//   - Serializable transactions: every Tx runs at SERIALIZABLE and is
//     retried on a serialization failure (40001) or a deadlock (40P01).
//     Tx.LockRows takes SELECT ... FOR UPDATE on rows in the caller's
//     order, for allocations to lock a silo and its ancestors root
//     first.
//   - The change log's counter row is bumped in the writing transaction,
//     which also runs NOTIFY silo_changes, '<seq>'. One connection per
//     store LISTENs, and a notification only wakes the followers, which
//     then read the log from where they are. They also poll once a
//     second, and read the log after every reconnect of the listener.
//   - A lease is a session-level advisory lock, pg_try_advisory_lock, on
//     a connection of its own, tried every five seconds and pinged every
//     two (see Lead).
//   - Database time is now(), the transaction's start.
//
// Connections are made through a pgxpool.Config, which the caller can
// adjust with Options before anything dials: WithDialFunc is the seam
// for dialing through the Cloud SQL Go connector (IAM login, PSC), and
// every connection the store makes -- the pool's, the listener's and
// each lease's -- dials through it.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/internal/sqltx"
	"github.com/bwsalmon/silo/pkg/store/migrations"
)

// SQLSTATEs this backend tells apart.
const (
	codeSerialization = "40001"
	codeDeadlock      = "40P01"
	codeUnique        = "23505"
	codeNoTable       = "42P01"
)

func sqlState(err error) string {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// channel is what the change log is announced on.
const channel = "silo_changes"

func newDialect() *sqltx.Dialect {
	return &sqltx.Dialect{
		Rebind: rebind,
		NowSQL: `SELECT CAST(floor(EXTRACT(EPOCH FROM now()) * 1000) AS BIGINT)`,
		IsConflict: func(err error) bool {
			c := sqlState(err)
			return c == codeSerialization || c == codeDeadlock
		},
		IsUnique:  func(err error) bool { return sqlState(err) == codeUnique },
		IsNoTable: func(err error) bool { return sqlState(err) == codeNoTable },
		TxOptions: &sql.TxOptions{Isolation: sql.LevelSerializable},
		// pg_notify is NOTIFY with its payload as a parameter. Postgres
		// delivers it when, and only if, the transaction commits.
		AfterAppend: `SELECT pg_notify('` + channel + `', ?)`,
		ForUpdate:   ` FOR UPDATE`,
	}
}

// Timings of the follower and the lease, from SV-46 and SV-52.
const (
	pollInterval   = time.Second     // the followers' backstop poll
	relisten       = time.Second     // pause before the listener reconnects
	leaseRetry     = 5 * time.Second // how often a waiter tries the lock
	leasePing      = 2 * time.Second // how often a holder pings its connection
	leasePingLimit = 2 * time.Second // how long a ping may take before it has failed
	// keepalive is the TCP keepalive idle time of every connection, and
	// on lease connections the server's too. It is longer than a ping
	// and its limit together, so a holder cut off from the database
	// closes lost before Postgres notices it has gone and frees the
	// lock for someone else.
	keepalive = 10 * time.Second
)

// Option adjusts the connection configuration before the store dials.
type Option func(*pgxpool.Config)

// WithDialFunc makes every connection the store opens dial through d:
// for example the Cloud SQL Go connector's dialer, which ignores the
// address and dials the instance it was built for. Its connections'
// TCP keepalive is then d's business; it should be about ten seconds
// (see keepalive).
func WithDialFunc(d pgconn.DialFunc) Option {
	return func(c *pgxpool.Config) { c.ConnConfig.DialFunc = d }
}

// WithConfig hands fn the whole configuration: pool sizes, runtime
// parameters, TLS, a BeforeConnect hook for IAM tokens.
func WithConfig(fn func(*pgxpool.Config)) Option { return Option(fn) }

// Store is the PostgreSQL store.Database.
type Store struct {
	pool    *pgxpool.Pool
	db      *sql.DB
	dialect *sqltx.Dialect
	ms      []migrations.Migration
	conn    *pgx.ConnConfig // what the listener and leases dial with

	// ctx ends when the store closes, taking everything it started
	// with it.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	wake   chan struct{}            // closed when the listener hears a change, or reconnects
	leases map[string]chan struct{} // one-slot semaphores, by lease name: one try per process at a time

	// onClose releases what was opened for the store, such as the Cloud
	// SQL connector's dialer, after the pool.
	onClose func() error

	listenOnce sync.Once
	closeOnce  sync.Once
	closeErr   error
	running    sync.WaitGroup // the listener and lease holders
}

var _ store.Database = (*Store)(nil)

// Open opens the store at connString, a libpq URL or key=value string,
// as pgxpool.ParseConfig reads it (pool_max_conns and the rest are
// accepted). It does not dial until it is used.
func Open(ctx context.Context, connString string, opts ...Option) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("postgres store: %w", err)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: keepalive}
	cfg.ConnConfig.DialFunc = dialer.DialContext
	for _, opt := range opts {
		opt(cfg)
	}
	ms, err := migrations.Load("postgres")
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres store: %w", err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	return &Store{
		pool:    pool,
		db:      stdlib.OpenDBFromPool(pool),
		dialect: newDialect(),
		ms:      ms,
		conn:    cfg.ConnConfig.Copy(),
		ctx:     sctx,
		cancel:  cancel,
		wake:    make(chan struct{}),
		leases:  map[string]chan struct{}{},
	}, nil
}

func (s *Store) closed() bool { return s.ctx.Err() != nil }

// dial opens a connection of its own, outside the pool, with the
// server-side parameters params added.
func (s *Store) dial(ctx context.Context, params map[string]string) (*pgx.Conn, error) {
	cfg := s.conn.Copy()
	for k, v := range params {
		cfg.RuntimeParams[k] = v
	}
	return pgx.ConnectConfig(ctx, cfg)
}

// Tx implements store.Store.
func (s *Store) Tx(ctx context.Context, fn func(store.Tx) error) error {
	return sqltx.Retry(ctx, func() error {
		if s.closed() {
			return store.ErrClosed
		}
		_, err := sqltx.Attempt(ctx, s.db, s.dialect, fn)
		return err
	})
}

// Changes implements store.Store.
func (s *Store) Changes(ctx context.Context, since uint64) (<-chan store.Change, error) {
	if s.closed() {
		return nil, store.ErrClosed
	}
	s.listenOnce.Do(func() {
		s.running.Add(1)
		go s.listen()
	})
	return sqltx.Follow(ctx, sqltx.Follower{
		DB:      s.db,
		Dialect: s.dialect,
		Wake: func() <-chan struct{} {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.wake
		},
		Done: s.ctx.Done(),
		Poll: pollInterval,
	}, since), nil
}

// wakeFollowers wakes every follower to read the log.
func (s *Store) wakeFollowers() {
	s.mu.Lock()
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

// listen keeps one connection in LISTEN silo_changes until the store
// closes, reconnecting when it drops. A notification carries the new
// sequence number, but it is only a wake-up: the followers read the log
// from where they are, so a notification lost or merged with another
// loses nothing. After each (re)connect the followers are woken as
// well, to read whatever committed while nobody was listening.
func (s *Store) listen() {
	defer s.running.Done()
	for {
		listened := false
		conn, err := s.dial(s.ctx, nil)
		if err == nil {
			if _, err = conn.Exec(s.ctx, `LISTEN `+channel); err == nil {
				listened = true
				s.wakeFollowers()
				for err == nil {
					_, err = conn.WaitForNotification(s.ctx)
					if err == nil {
						s.wakeFollowers()
					}
				}
			}
			closeConn(conn)
		}
		if s.closed() {
			return
		}
		// A connection that worked and dropped is replaced at once;
		// one that could not be made is retried after a pause.
		if listened {
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(relisten):
		}
	}
}

// closeConn closes a connection of the store's own, giving up on a
// polite goodbye after a second.
func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

// Schema implements store.Store.
func (s *Store) Schema(ctx context.Context) (int, error) {
	if s.closed() {
		return 0, store.ErrClosed
	}
	return sqltx.Schema(ctx, s.db, s.dialect)
}

// Shared implements store.Store: any number of silods share one
// Postgres database.
func (s *Store) Shared() bool { return true }

// Migrate implements store.Database. Replicas starting together may
// all try it, so it runs under a session advisory lock of its own and
// the second finds the work done.
func (s *Store) Migrate(ctx context.Context, to int) error {
	if s.closed() {
		return store.ErrClosed
	}
	conn, err := s.dial(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer closeConn(conn)
	ns, key := lockKey("migrate")
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, ns, key); err != nil {
		return fmt.Errorf("migrate: taking the migration lock: %w", err)
	}
	return sqltx.Migrate(ctx, s.db, s.dialect, s.ms, to)
	// Closing conn releases the lock.
}

// Contracts implements store.Database.
func (s *Store) Contracts(ctx context.Context) (int, error) {
	return sqltx.Contracts(ctx, s.db, s.dialect)
}

// Latest implements store.Database.
func (s *Store) Latest() int { return len(s.ms) }

// Close implements store.Database. Every lease's lost is closed, and
// the lease released, by the time it returns.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.running.Wait()
		s.closeErr = s.db.Close()
		s.pool.Close()
		if s.onClose != nil {
			if err := s.onClose(); s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}
