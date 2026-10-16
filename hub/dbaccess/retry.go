package dbaccess

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// maxAttempts bounds how often InTx runs a transaction that keeps
// being aborted.
const maxAttempts = 10

// Retryable reports whether err aborted a transaction that would
// succeed if run again: a serialization failure (SQLSTATE 40001) or a
// deadlock (40P01). PostgreSQL raises them under REPEATABLE READ and
// SERIALIZABLE, and Cloud Spanner, whose read-write transactions are
// always serializable, raises 40001 whenever two of them conflict
// (docs/spanner-evaluation.md). On SQLite it is SQLITE_BUSY, which a
// transaction that read before writing gets at once, whatever the busy
// timeout, when another connection is committing a write.
func Retryable(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "40001" || pe.Code == "40P01"
	}
	var se interface{ Code() int }
	return errors.As(err, &se) && se.Code()&0xff == sqliteBusy
}

// sqliteBusy is SQLITE_BUSY, the primary result code of every
// SQLITE_BUSY_* extended one.
const sqliteBusy = 5

// InTx runs f in a transaction on db and commits it if f returns nil,
// running the whole of it again, after a short random pause, when the
// database aborts it as Retryable. f may therefore run more than once,
// and should change nothing outside the transaction but what it
// returns.
func InTx(ctx context.Context, db *sql.DB, f func(*sql.Tx) error) error {
	var err error
	for attempt := range maxAttempts {
		if attempt > 0 {
			pause := time.Duration(rand.Int64N(int64(10*time.Millisecond) << min(attempt, 5)))
			select {
			case <-ctx.Done():
				return errors.Join(ctx.Err(), err)
			case <-time.After(pause):
			}
		}
		if err = once(ctx, db, f); !Retryable(err) {
			return err
		}
	}
	return err
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
