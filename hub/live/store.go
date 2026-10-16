package live

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/hub/dbaccess"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite"
)

// Log is where the hub keeps the events it stores: lifecycle and
// conversation, never a token-stream or node-only one (Hub enforces
// that before it calls Append). Store is the database one.
type Log interface {
	// Append keeps e, the next event of its task.
	Append(ctx context.Context, e Event, c Class) error
	// Since returns task's kept events with after < seq <= upto, in
	// order.
	Since(ctx context.Context, task string, after, upto int64) ([]Event, error)
	// Last returns task's last kept event's seq and type: 0 and "" when
	// it has none.
	Last(ctx context.Context, task string) (int64, string, error)
}

// Store is the event store: SQLite for one hub replica, PostgreSQL for
// several, as the backlog is. It holds events as the hub classified
// them (Classify), one row per event.
type Store struct {
	db       *sql.DB
	postgres bool
}

// Open opens the event store at url: "postgres://..." (or
// "postgresql://...") for PostgreSQL, "sqlite:PATH" or a bare path for
// SQLite (":memory:" for a throwaway one). It creates its table if it is
// missing; it may share a database with the backlog and the team
// directory.
func Open(ctx context.Context, url string) (*Store, error) { return open(ctx, url, true) }

// OpenExisting opens the event store at url as Open does, but creates
// nothing: its table was made by Open or `silo hub migrate` as a role
// that may create it, and url's role need only read and write its rows
// (docs/deploy.md). It fails if the table is missing.
func OpenExisting(ctx context.Context, url string) (*Store, error) { return open(ctx, url, false) }

func open(ctx context.Context, url string, create bool) (*Store, error) {
	var (
		db       *sql.DB
		err      error
		postgres = strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://")
	)
	if postgres {
		db, err = sql.Open("pgx", url)
	} else {
		path := strings.TrimPrefix(url, "sqlite:")
		db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		// One connection: an in-memory database is per connection, and
		// SQLite serialises writers anyway.
		db.SetMaxOpenConns(1)
	}
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: postgres}
	if !create {
		if err := dbaccess.Check(ctx, db, "live", "task_events"); err != nil {
			db.Close()
			return nil, err
		}
		return s, nil
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS task_events (
	task TEXT NOT NULL,
	seq BIGINT NOT NULL,
	type TEXT NOT NULL,
	class TEXT NOT NULL,
	body TEXT NOT NULL,
	PRIMARY KEY (task, seq)
)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("live: creating tables: %w", err)
	}
	return s, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// q rewrites the ? placeholders of a query for PostgreSQL.
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

// Append keeps e. It refuses a class the store does not keep, so that
// nothing but lifecycle and conversation can reach it.
func (s *Store) Append(ctx context.Context, e Event, c Class) error {
	if !c.Stored() || e.Artifact != nil {
		return fmt.Errorf("live: the store keeps no %s event", c)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO task_events (task, seq, type, class, body) VALUES (?, ?, ?, ?, ?)`),
		e.Task, e.Seq, e.Type, c.String(), string(b))
	return err
}

// Since returns task's events with after < seq <= upto, in order.
func (s *Store) Since(ctx context.Context, task string, after, upto int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT body FROM task_events WHERE task = ? AND seq > ? AND seq <= ? ORDER BY seq`), task, after, upto)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var e Event
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, fmt.Errorf("live: task %s's event: %w", task, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Size is how many events the store holds and how many bytes their
// bodies take: the hub's store-size metric.
func (s *Store) Size(ctx context.Context) (events, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(body)), 0) FROM task_events`).Scan(&events, &bytes)
	return events, bytes, err
}

// Stats is the store's connection pool's statistics.
func (s *Store) Stats() sql.DBStats { return s.db.Stats() }

// Last returns task's last event's seq and type.
func (s *Store) Last(ctx context.Context, task string) (int64, string, error) {
	var (
		seq int64
		typ string
	)
	err := s.db.QueryRowContext(ctx, s.q(`SELECT seq, type FROM task_events WHERE task = ? ORDER BY seq DESC LIMIT 1`), task).Scan(&seq, &typ)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return seq, typ, err
}
