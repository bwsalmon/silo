// Package sqlite is silod's SQLite store, --store=sqlite:<path>: for
// local development, the fake-GCE tests and single-binary demos
// (docs/silo/state.md, SV-45). It is not shared. One process owns the
// file, and a second is refused.
//
// How it provides what silod relies on (SV-46):
//
//   - Serializable transactions: one writer at a time. Every transaction
//     begins with BEGIN IMMEDIATE, in WAL mode, behind an in-process
//     writer lock, so transactions never interleave and serializable
//     holds by construction. A busy database is still mapped to
//     store.ErrConflict and retried, in case something outside silod has
//     the file open.
//   - The change log's followers are woken in-process when a transaction
//     that appended to it commits, and poll once a second as well.
//   - Leases are an in-process mutex per name.
//   - Database time is SQLite's clock, read in SQL.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"

	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/internal/sqltx"
	"github.com/bwsalmon/silo/pkg/store/migrations"
)

// SQLite result codes this backend tells apart, from sqlite3.h.
const (
	codeBusy             = 5
	codeLocked           = 6
	codeConstraintPK     = 1555
	codeConstraintUnique = 2067
)

func code(err error) int {
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return -1
}

func newDialect() *sqltx.Dialect {
	return &sqltx.Dialect{
		NowSQL: `SELECT CAST(unixepoch('subsec') * 1000 AS INTEGER)`,
		IsConflict: func(err error) bool {
			c := code(err) & 0xff
			return c == codeBusy || c == codeLocked
		},
		IsUnique: func(err error) bool {
			c := code(err)
			return c == codeConstraintUnique || c == codeConstraintPK
		},
		IsNoTable: func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "no such table")
		},
	}
}

// pollInterval is the followers' backstop poll.
const pollInterval = time.Second

// Store is the SQLite store.Database.
type Store struct {
	db      *sql.DB
	dialect *sqltx.Dialect
	ms      []migrations.Migration
	unlock  func() error

	writer chan struct{} // the one writer: hold it to run a transaction

	mu     sync.Mutex
	wake   chan struct{}            // closed at a commit that appended to the log
	leases map[string]chan struct{} // one-slot semaphores, by lease name

	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
	running   sync.WaitGroup // lease holders and followers
}

var _ store.Database = (*Store)(nil)

// Open opens, creating if need be, the SQLite database at path. It does
// not migrate. It fails with store.ErrInUse if another Store, in this
// process or any other, has the file open.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlite store: empty path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("sqlite store: %w", err)
		}
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("sqlite store %s: %w", path, err)
	}
	ms, err := migrations.Load("sqlite")
	if err != nil {
		unlock()
		return nil, err
	}
	// _txlock=immediate makes every BeginTx a BEGIN IMMEDIATE.
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("sqlite store %s: %w", path, err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		db.Close()
		unlock()
		return nil, fmt.Errorf("sqlite store %s: journal_mode is %q, want wal: %v", path, mode, err)
	}
	return &Store{
		db:      db,
		dialect: newDialect(),
		ms:      ms,
		unlock:  unlock,
		writer:  make(chan struct{}, 1),
		wake:    make(chan struct{}),
		leases:  map[string]chan struct{}{},
		closed:  make(chan struct{}),
	}, nil
}

// Tx implements store.Store.
func (s *Store) Tx(ctx context.Context, fn func(store.Tx) error) error {
	return sqltx.Retry(ctx, func() error {
		if err := s.acquireWriter(ctx); err != nil {
			return err
		}
		defer func() { <-s.writer }()
		appended, err := sqltx.Attempt(ctx, s.db, s.dialect, fn)
		if appended {
			s.mu.Lock()
			close(s.wake)
			s.wake = make(chan struct{})
			s.mu.Unlock()
		}
		return err
	})
}

func (s *Store) acquireWriter(ctx context.Context) error {
	select {
	case <-s.closed:
		return store.ErrClosed
	default:
	}
	select {
	case s.writer <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return store.ErrClosed
	}
}

// Changes implements store.Store.
func (s *Store) Changes(ctx context.Context, since uint64) (<-chan store.Change, error) {
	select {
	case <-s.closed:
		return nil, store.ErrClosed
	default:
	}
	return sqltx.Follow(ctx, sqltx.Follower{
		DB:      s.db,
		Dialect: s.dialect,
		Wake: func() <-chan struct{} {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.wake
		},
		Done: s.closed,
		Poll: pollInterval,
	}, since), nil
}

// Lead implements store.Store. The lease is held until ctx ends or the
// store closes; lost is closed then, before the lease is released.
func (s *Store) Lead(ctx context.Context, name string) (<-chan struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	sem, ok := s.leases[name]
	if !ok {
		sem = make(chan struct{}, 1)
		s.leases[name] = sem
	}
	s.mu.Unlock()

	select {
	case <-s.closed:
		return nil, store.ErrClosed
	default:
	}
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, store.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		<-sem // won the race with ctx ending; it ended, so give it back
		return nil, err
	}
	lost := make(chan struct{})
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		select {
		case <-ctx.Done():
		case <-s.closed:
		}
		close(lost)
		<-sem
	}()
	return lost, nil
}

// Schema implements store.Store.
func (s *Store) Schema(ctx context.Context) (int, error) {
	return sqltx.Schema(ctx, s.db, s.dialect)
}

// Shared implements store.Store: one process owns a SQLite file.
func (s *Store) Shared() bool { return false }

// Migrate implements store.Database.
func (s *Store) Migrate(ctx context.Context, to int) error {
	if err := s.acquireWriter(ctx); err != nil {
		return err
	}
	defer func() { <-s.writer }()
	return sqltx.Migrate(ctx, s.db, s.dialect, s.ms, to)
}

// Contracts implements store.Database.
func (s *Store) Contracts(ctx context.Context) (int, error) {
	return sqltx.Contracts(ctx, s.db, s.dialect)
}

// Latest implements store.Database.
func (s *Store) Latest() int { return len(s.ms) }

// Close implements store.Database. Every lease's lost is closed by the
// time it returns.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.running.Wait()
		s.closeErr = errors.Join(s.db.Close(), s.unlock())
	})
	return s.closeErr
}
