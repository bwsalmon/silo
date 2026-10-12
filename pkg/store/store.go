// Package store is silod's state: the one package through which all of
// it is read and written (docs/silo/state.md, SV-45).
//
// # Backends
//
// Two backends implement Store and pass the same conformance suite,
// package storetest (SV-55):
//
//   - postgres, selected by --store=postgres, the default, package
//     store/postgres. Every real install, on a Cloud SQL instance. Shared: any number of silod
//     replicas use one database.
//   - sqlite, selected by --store=sqlite:<path>, package store/sqlite.
//     Local development, the fake-GCE tests and single-binary demos. Not
//     shared: a second process on the same file is refused.
//
// # Who writes SQL
//
// All of silod's SQL runs through this package: inside Store.Tx,
// through the Tx it is handed. Code that owns a table keeps its queries
// in its own package (pkg/tree for silos, pkg/ledger for the
// quota ledger), and adds the table as a migration here; everything
// else calls those packages.
//
// # The shared subset of SQL
//
// The schema and every query are written once, in the subset of SQL both
// backends share:
//
//   - Types are TEXT, INTEGER (BIGINT where Postgres needs the width),
//     and BOOLEAN. Nothing else: no arrays, no JSONB, no ENUM, no SERIAL.
//   - Timestamps are UTC integers: Unix milliseconds in an INTEGER
//     column, written with Millis and read with Time. They are taken from
//     the database's clock, Tx.Now, never from the process's.
//   - JSON is TEXT, encoded and decoded in Go. Queries never look inside
//     it.
//   - Placeholders are written ?, and a backend rebinds them ($1, $2, ...
//     for Postgres).
//   - No backend-only features in queries: no Postgres arrays or JSONB
//     operators, no SQLite-only functions. RETURNING, ON CONFLICT and
//     COALESCE are in both and may be used.
//
// Where DDL has to differ (types, CREATE INDEX CONCURRENTLY, triggers),
// each backend has its own migration file with the same version number;
// see package store/migrations.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store is all of silod's state. This is the interface in
// docs/silo/state.md, "Two backends, one interface" (SV-45).
type Store interface {
	// Tx runs fn in one transaction at serializable isolation. A
	// serialization failure is retried, up to a limit, with fn run
	// again from the start. So fn must not call GCE or do anything
	// else that can't be repeated.
	Tx(ctx context.Context, fn func(Tx) error) error

	// Changes returns the change log from seq onwards, then follows
	// it: each committed change, in commit order, with no gaps.
	Changes(ctx context.Context, since uint64) (<-chan Change, error)

	// Lead blocks until this process holds the named lease, or ctx
	// ends. Lost is closed the moment the lease can no longer be
	// trusted.
	Lead(ctx context.Context, name string) (lost <-chan struct{}, err error)

	// Schema reports the applied migration version.
	Schema(ctx context.Context) (int, error)

	// Shared reports whether several processes may use this store at
	// once: true for postgres, false for sqlite.
	Shared() bool
}

// The details state.md leaves to the implementation, settled here so
// that both backends settle them the same way:
//
//   - Changes(ctx, since) delivers the entries with Seq > since, so a
//     reader resumes by passing the last Seq it handled, and 0 reads the
//     whole log. The channel is closed when ctx ends or the store is
//     closed.
//   - A lease taken by Lead is held until ctx ends (or the store is
//     closed, or, on a shared backend, its connection is lost). lost is
//     closed first, then the lease is released, so a holder learns it
//     has lost the lease before anyone else can take it.
//   - A Tx whose fn returns an error, or panics, rolls back. fn may
//     return an error wrapping ErrConflict to ask for a retry itself.

// Tx is one transaction, handed to the fn of Store.Tx. It is only valid
// until fn returns.
type Tx interface {
	// Exec runs a statement and returns how many rows it changed.
	Exec(ctx context.Context, query string, args ...any) (int64, error)
	// Query runs a query that returns rows. The caller closes them.
	Query(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	// QueryRow runs a query expected to return at most one row.
	QueryRow(ctx context.Context, query string, args ...any) *sql.Row

	// Now is the database's time, the same for the whole transaction.
	// Every delay, grace period and retention silod applies is measured
	// against it, never against the process's clock, so replicas whose
	// clocks differ still agree (SV-46, "One clock").
	Now(ctx context.Context) (time.Time, error)

	// Append adds an entry to the change log and returns its sequence
	// number. The entry commits with the transaction, or not at all.
	Append(ctx context.Context, kind, key, data string) (uint64, error)

	// LockRows locks, for the rest of the transaction, the row of table
	// whose column equals each key, one at a time in the order given,
	// and fails if one is missing. A transaction that changes an
	// allocation locks the silo's row and each ancestor's, root first
	// and then by depth, so contention on a subtree queues on the root's
	// lock and two such transactions cannot deadlock (SV-46, item 2).
	// table and column are plain lower-case identifiers. On a backend
	// with a single writer it only checks that the rows exist.
	LockRows(ctx context.Context, table, column string, keys ...string) error

	// Request looks up the result recorded for requestID by an earlier,
	// committed transaction.
	Request(ctx context.Context, requestID string) (result string, ok bool, err error)
	// RecordRequest records the result of the work done for requestID.
	// It fails with ErrDuplicateRequest if requestID already has one.
	RecordRequest(ctx context.Context, requestID, result string) error
}

// Change is one entry of the change log.
type Change struct {
	Seq  uint64    // gap-free, in commit order, one sequence for the fleet
	Kind string    // what changed: "grant", "origin", ...
	Key  string    // which one changed
	Data string    // JSON, or empty
	At   time.Time // database time of the transaction that wrote it
}

// Database is what a backend opens: a Store, plus the operations only
// install, upgrade and tests perform.
type Database interface {
	Store
	// Migrate applies migrations up to and including version to. It
	// only goes forward: a to below the applied version is an error.
	Migrate(ctx context.Context, to int) error
	// Latest is the newest migration this binary carries.
	Latest() int
	// Contracts is the highest requirement of the contract steps
	// applied (package store/migrations): a binary whose Latest is
	// below it no longer runs on this database. 0 if there are none.
	Contracts(ctx context.Context) (int, error)
	// Close releases the database, and with it every lease held.
	Close() error
}

// MaxAttempts is how many times Tx runs fn before giving up on
// serialization failures. The change log's counter row (sqltx.Append)
// is a single row every writer in the database contends on, so under
// sustained concurrent writers this is the number of attempts that
// needs to absorb a thundering herd of retries, not just an occasional
// collision: 10 was observed to run out under 8 concurrent writers
// hammering it (storetest's TestConformance/ChangeLog, intermittently,
// against Postgres).
const MaxAttempts = 50

var (
	// ErrConflict is a serialization failure: the transaction lost to a
	// concurrent one and may succeed if run again. Tx retries it; a Tx
	// that still conflicts after MaxAttempts returns it.
	ErrConflict = errors.New("store: serialization conflict")
	// ErrDuplicateRequest is RecordRequest finding a result already
	// recorded for the request ID.
	ErrDuplicateRequest = errors.New("store: request_id already recorded")
	// ErrClosed is an operation on a closed store.
	ErrClosed = errors.New("store: closed")
	// ErrContract is silod migrate refusing a contract step while a
	// replica too old for it is running.
	ErrContract = errors.New("store: a contract step would break a running replica")
	// ErrInUse is a non-shared store refusing a second user.
	ErrInUse = errors.New("store: database in use by another process")
)

// Millis encodes t as the INTEGER the schema stores timestamps as: Unix
// milliseconds, UTC.
func Millis(t time.Time) int64 { return t.UnixMilli() }

// Time decodes a timestamp column written by Millis.
func Time(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// Spec is a parsed --store flag.
type Spec struct {
	Backend string // "postgres" or "sqlite"
	Path    string // the database file, for sqlite
}

// ParseSpec parses a --store value: "postgres", or "sqlite:<path>".
func ParseSpec(s string) (Spec, error) {
	backend, path, hasPath := strings.Cut(s, ":")
	switch backend {
	case "postgres":
		if hasPath {
			return Spec{}, fmt.Errorf("--store=%s: postgres takes no path", s)
		}
		return Spec{Backend: backend}, nil
	case "sqlite":
		if path == "" {
			return Spec{}, fmt.Errorf("--store=%s: sqlite needs a path, as sqlite:<path>", s)
		}
		return Spec{Backend: backend, Path: path}, nil
	}
	return Spec{}, fmt.Errorf("--store=%s: unknown backend %q (want postgres or sqlite:<path>)", s, backend)
}
