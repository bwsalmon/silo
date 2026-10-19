package dbaccess

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// maxAttempts bounds how often InTx or Retry runs an operation that keeps
// being aborted by serialization or lock contention.
const maxAttempts = 64

// Retryable reports whether err aborted a transaction or statement that
// would succeed if run again: a serialization failure (SQLSTATE 40001) or a
// deadlock (40P01). PostgreSQL raises them under REPEATABLE READ and
// SERIALIZABLE, and Cloud Spanner, whose read-write transactions are
// always serializable, raises 40001 whenever two of them conflict
// (docs/spanner-evaluation.md). On SQLite it is SQLITE_BUSY or
// SQLITE_LOCKED, which a transaction that read before writing gets at
// once, whatever the busy timeout, when another connection is committing a
// write.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "40001" || pe.Code == "40P01"
	}
	var se interface{ Code() int }
	if errors.As(err, &se) {
		c := se.Code() & 0xff
		if c == sqliteBusy || c == sqliteLocked {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "deadlock detected") ||
		strings.Contains(msg, "could not serialize access") ||
		strings.Contains(msg, "40001") ||
		strings.Contains(msg, "40p01")
}

// sqliteBusy and sqliteLocked are SQLITE_BUSY and SQLITE_LOCKED, the
// primary result codes of every SQLITE_BUSY_* and SQLITE_LOCKED_*
// extended code.
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// Retry runs f and repeats it, after a short random pause, while f returns
// a Retryable error and ctx has not ended.
func Retry(ctx context.Context, f func() error) error {
	var err error
	for attempt := range maxAttempts {
		if attempt > 0 {
			pause := time.Duration(1 + rand.Int64N(int64(2*time.Millisecond)<<min(attempt, 4)))
			select {
			case <-ctx.Done():
				return errors.Join(ctx.Err(), err)
			case <-time.After(pause):
			}
		}
		if err = f(); !Retryable(err) {
			return err
		}
	}
	return err
}

// InTx runs f in a transaction on db and commits it if f returns nil,
// running the whole of it again, after a short random pause, when the
// database aborts it as Retryable. f may therefore run more than once,
// and should change nothing outside the transaction but what it
// returns.
func InTx(ctx context.Context, db *sql.DB, f func(*sql.Tx) error) error {
	return Retry(ctx, func() error {
		return once(ctx, db, f)
	})
}

func once(ctx context.Context, db *sql.DB, f func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
