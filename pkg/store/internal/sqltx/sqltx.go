// Package sqltx is the part of a store backend that is the same SQL on
// both: running a transaction with retries, the Tx handed to fn, the
// change log and its follower, and the migration runner. A backend
// supplies a Dialect for what differs, and the things that are not SQL
// at all -- its connection, its leases and how a follower is woken.
package sqltx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/migrations"
)

// Dialect is what differs between backends.
type Dialect struct {
	// Rebind turns a query's ? placeholders into the backend's own.
	// Nil leaves the query alone.
	Rebind func(query string) string
	// NowSQL is a query returning the database's time as Unix
	// milliseconds.
	NowSQL string
	// IsConflict reports a serialization failure worth a retry.
	IsConflict func(error) bool
	// IsUnique reports a unique-constraint violation.
	IsUnique func(error) bool
	// IsNoTable reports a query on a table that does not exist.
	IsNoTable func(error) bool
	// TxOptions is what each transaction is begun with.
	TxOptions *sql.TxOptions
	// AfterAppend, when set, runs in the same transaction after each
	// change-log entry is written, with the entry's sequence number as
	// its one argument, in decimal. Postgres uses it to NOTIFY.
	AfterAppend string
	// ForUpdate is what Tx.LockRows appends to its SELECT to lock the
	// row: " FOR UPDATE" where the backend has row locks, empty where
	// a single writer makes them unnecessary.
	ForUpdate string
}

func (d *Dialect) rebind(q string) string {
	if d.Rebind == nil {
		return q
	}
	return d.Rebind(q)
}

// Retry runs attempt until it succeeds, fails with something other than
// store.ErrConflict, ctx ends, or store.MaxAttempts attempts have
// conflicted.
func Retry(ctx context.Context, attempt func() error) error {
	for n := 1; ; n++ {
		err := attempt()
		if err == nil || !errors.Is(err, store.ErrConflict) || n >= store.MaxAttempts {
			return err
		}
		// A short, jittered, growing pause, so writers that just
		// collided do not collide again in lockstep.
		backoff := time.Duration(n) * time.Millisecond
		backoff += rand.N(backoff + time.Millisecond)
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(backoff):
		}
	}
}

// Attempt runs fn once in a transaction on db. It reports whether the
// transaction committed an entry to the change log, so the backend can
// wake its followers. A serialization failure, whether fn's statements
// or the commit hit it, comes back wrapping store.ErrConflict.
func Attempt(ctx context.Context, db *sql.DB, d *Dialect, fn func(store.Tx) error) (appended bool, err error) {
	sqlTx, err := db.BeginTx(ctx, d.TxOptions)
	if err != nil {
		return false, d.classify(err)
	}
	t := &tx{tx: sqlTx, d: d}
	committed := false
	defer func() {
		t.done = true
		if !committed {
			_ = sqlTx.Rollback()
		}
	}()
	if err := fn(t); err != nil {
		return false, d.classify(err)
	}
	if err := sqlTx.Commit(); err != nil {
		return false, d.classify(err)
	}
	committed = true
	return t.appended, nil
}

func (d *Dialect) classify(err error) error {
	if err != nil && !errors.Is(err, store.ErrConflict) && d.IsConflict(err) {
		return fmt.Errorf("%w: %w", store.ErrConflict, err)
	}
	return err
}

// tx is the store.Tx a backend hands fn.
type tx struct {
	tx       *sql.Tx
	d        *Dialect
	now      time.Time
	appended bool
	done     bool
}

var errTxDone = errors.New("store: Tx used after its fn returned")

func (t *tx) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if t.done {
		return 0, errTxDone
	}
	res, err := t.tx.ExecContext(ctx, t.d.rebind(query), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (t *tx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if t.done {
		return nil, errTxDone
	}
	return t.tx.QueryContext(ctx, t.d.rebind(query), args...)
}

func (t *tx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.d.rebind(query), args...)
}

func (t *tx) Now(ctx context.Context) (time.Time, error) {
	if t.now.IsZero() {
		var ms int64
		if err := t.QueryRow(ctx, t.d.NowSQL).Scan(&ms); err != nil {
			return time.Time{}, fmt.Errorf("reading the database's time: %w", err)
		}
		t.now = store.Time(ms)
	}
	return t.now, nil
}

func (t *tx) Append(ctx context.Context, kind, key, data string) (uint64, error) {
	now, err := t.Now(ctx)
	if err != nil {
		return 0, err
	}
	// The counter row serializes every writer of the log, which is
	// what makes the numbers gap-free and in commit order.
	var seq uint64
	if err := t.QueryRow(ctx,
		`UPDATE change_counter SET seq = seq + 1 WHERE id = 1 RETURNING seq`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("taking a change sequence number: %w", err)
	}
	if _, err := t.Exec(ctx,
		`INSERT INTO changes (seq, kind, key, data, at) VALUES (?, ?, ?, ?, ?)`,
		seq, kind, key, data, store.Millis(now)); err != nil {
		return 0, fmt.Errorf("appending change %d: %w", seq, err)
	}
	if t.d.AfterAppend != "" {
		if _, err := t.Exec(ctx, t.d.AfterAppend, strconv.FormatUint(seq, 10)); err != nil {
			return 0, fmt.Errorf("announcing change %d: %w", seq, err)
		}
	}
	t.appended = true
	return seq, nil
}

func (t *tx) LockRows(ctx context.Context, table, column string, keys ...string) error {
	if !identifier.MatchString(table) || !identifier.MatchString(column) {
		return fmt.Errorf("LockRows: %q.%q is not a plain identifier", table, column)
	}
	// One statement per row, in the caller's order: a single
	// IN (...) FOR UPDATE would take the locks in whatever order the
	// plan visits the rows, and that order is what keeps two
	// transactions from deadlocking.
	q := `SELECT 1 FROM ` + table + ` WHERE ` + column + ` = ?` + t.d.ForUpdate
	for _, k := range keys {
		var one int
		err := t.QueryRow(ctx, q, k).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("LockRows: no row in %s with %s = %q", table, column, k)
		}
		if err != nil {
			return fmt.Errorf("LockRows: locking %s %q: %w", table, k, err)
		}
	}
	return nil
}

// identifier is what LockRows accepts as a table or column name, so
// that a name can never carry SQL of its own.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func (t *tx) Request(ctx context.Context, requestID string) (string, bool, error) {
	var result string
	err := t.QueryRow(ctx, `SELECT result FROM requests WHERE request_id = ?`, requestID).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return result, true, nil
}

func (t *tx) RecordRequest(ctx context.Context, requestID, result string) error {
	now, err := t.Now(ctx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `INSERT INTO requests (request_id, result, at) VALUES (?, ?, ?)`,
		requestID, result, store.Millis(now))
	if err != nil && t.d.IsUnique(err) {
		return fmt.Errorf("%w: %q: %w", store.ErrDuplicateRequest, requestID, err)
	}
	return err
}

// ReadChanges returns up to limit entries after seq, in order, stopping
// short of any gap.
func ReadChanges(ctx context.Context, db *sql.DB, d *Dialect, after uint64, limit int) ([]store.Change, error) {
	rows, err := db.QueryContext(ctx, d.rebind(
		`SELECT seq, kind, key, data, at FROM changes WHERE seq > ? ORDER BY seq LIMIT ?`), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Change
	for rows.Next() {
		var c store.Change
		var at int64
		if err := rows.Scan(&c.Seq, &c.Kind, &c.Key, &c.Data, &at); err != nil {
			return nil, err
		}
		// The counter makes a gap impossible in a committed log; if
		// one ever shows, stop at it rather than skip it.
		if c.Seq != after+uint64(len(out))+1 {
			break
		}
		c.At = store.Time(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Follower is what Follow needs from a backend.
type Follower struct {
	DB      *sql.DB
	Dialect *Dialect
	// Wake returns a channel closed at the next commit that appends to
	// the change log. It is called before each read, so a commit that
	// lands between a read and the wait is not missed.
	Wake func() <-chan struct{}
	// Done is closed when the store closes.
	Done <-chan struct{}
	// Poll is the backstop interval for a lost wake-up.
	Poll time.Duration
	// OnError hears a failed read; the follower carries on at the next
	// poll. Nil drops it.
	OnError func(error)
}

// Follow delivers the change log after since, then each entry as it
// commits, until ctx ends or the store closes; then it closes the
// channel. Each entry is delivered once, in order.
func Follow(ctx context.Context, f Follower, since uint64) <-chan store.Change {
	out := make(chan store.Change)
	go func() {
		defer close(out)
		last := since
		tick := time.NewTicker(f.Poll)
		defer tick.Stop()
		for {
			wake := f.Wake()
			batch, err := ReadChanges(ctx, f.DB, f.Dialect, last, 256)
			if err != nil && f.OnError != nil && ctx.Err() == nil {
				f.OnError(err)
			}
			for _, c := range batch {
				select {
				case out <- c:
					last = c.Seq
				case <-ctx.Done():
					return
				case <-f.Done:
					return
				}
			}
			if len(batch) == 256 {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-f.Done:
				return
			case <-wake:
			case <-tick.C:
			}
		}
	}()
	return out
}

// Schema reads the applied migration version; 0 before any migration.
func Schema(ctx context.Context, db *sql.DB, d *Dialect) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	if err != nil && d.IsNoTable(err) {
		return 0, nil
	}
	return v, err
}

// Contracts is the highest requirement of any contract step applied:
// the newest schema the oldest release that may run on this database
// must carry. 0 means none has been applied.
func Contracts(ctx context.Context, db *sql.DB, d *Dialect) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(requires), 0) FROM schema_contracts`).Scan(&v)
	if err != nil && d.IsNoTable(err) {
		return 0, nil
	}
	return v, err
}

// Live is how recent a replica's heartbeat must be for migrate to count
// it as running (state.md: the leader takes over a replica silent for
// 30 seconds).
const Live = 30 * time.Second

// staleReplicas lists the replicas heartbeating within Live whose
// binary's newest schema is below requires, as "id (release, schema
// n)".
func staleReplicas(ctx context.Context, db *sql.DB, d *Dialect, requires int) ([]string, error) {
	var nowMS int64
	if err := db.QueryRowContext(ctx, d.NowSQL).Scan(&nowMS); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, d.rebind(`SELECT id, release, schema_latest FROM replicas
		WHERE heartbeat_at >= ? AND schema_latest < ? ORDER BY id`), nowMS-Live.Milliseconds(), requires)
	if err != nil {
		if d.IsNoTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, release string
		var latest int
		if err := rows.Scan(&id, &release, &latest); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s (%s, schema %d)", id, release, latest))
	}
	return out, rows.Err()
}

// Migrate applies ms up to and including version to, each in its own
// transaction with its row in schema_migrations. It only goes forward.
// A contract step is refused, with store.ErrContract, while a replica
// whose binary predates its requirement is heartbeating; one it applies
// is recorded in schema_contracts.
func Migrate(ctx context.Context, db *sql.DB, d *Dialect, ms []migrations.Migration, to int) error {
	if to < 0 || to > len(ms) {
		return fmt.Errorf("migrate to version %d: this binary carries versions 1 to %d", to, len(ms))
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at BIGINT  NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_contracts (
		version  INTEGER PRIMARY KEY,
		requires INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating schema_contracts: %w", err)
	}
	cur, err := Schema(ctx, db, d)
	if err != nil {
		return err
	}
	if to < cur {
		return fmt.Errorf("migrate to version %d: the schema is at %d, and migrations only go forward", to, cur)
	}
	for _, m := range ms[cur:to] {
		if m.Requires > 0 {
			// Checked outside the step's transaction: a missing replicas
			// table would abort a Postgres transaction. A replica of an
			// older release that starts after this check refuses to
			// serve once the step has committed, since schema_contracts
			// then says it is too old.
			stale, err := staleReplicas(ctx, db, d, m.Requires)
			if err != nil {
				return fmt.Errorf("checking replicas before %s: %w", m.Name, err)
			}
			if len(stale) > 0 {
				return fmt.Errorf("%w: %s needs every replica to carry schema %d or newer, and these are running: %s",
					store.ErrContract, m.Name, m.Requires, strings.Join(stale, ", "))
			}
		}
		err := Retry(ctx, func() error {
			_, err := Attempt(ctx, db, d, func(t store.Tx) error {
				if _, err := t.Exec(ctx, m.SQL); err != nil {
					return fmt.Errorf("applying %s: %w", m.Name, err)
				}
				now, err := t.Now(ctx)
				if err != nil {
					return err
				}
				if m.Requires > 0 {
					if _, err := t.Exec(ctx, `INSERT INTO schema_contracts (version, requires) VALUES (?, ?)`,
						m.Version, m.Requires); err != nil {
						return err
					}
				}
				_, err = t.Exec(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
					m.Version, store.Millis(now))
				return err
			})
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
