package live

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/events"
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
	// AddRef keeps ref as one of task's artifacts' refs, which its node
	// listed in event seq: only the ref, never what it names. It keeps
	// the newest MaxRefs of a task's refs.
	AddRef(ctx context.Context, task, ref string, seq int64) error
	// HasRef is whether task's node listed ref (AddRef), among the
	// refs kept.
	HasRef(ctx context.Context, task, ref string) (bool, error)
}

// MaxRefs is how many of a task's artifact refs the hub keeps, the
// newest; a view of an older one is refused (O-5, INV-19).
const MaxRefs = 4096

// maxConns caps the event store's PostgreSQL connections, all of which
// it keeps open once made: every running task writes its events here, so
// a pool with no cap runs the server out of connections under load, and
// one that closes them makes PostgreSQL a process for each query
// (docs/test-runs/2026-10-16-hub-load.md). Writes beyond it wait for a
// connection.
const maxConns = 32

// Store is the event store: PostgreSQL, as the backlog is (SQLite only
// in local development and tests). It holds events as the hub classified
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
		if err == nil {
			db.SetMaxOpenConns(maxConns)
			db.SetMaxIdleConns(maxConns)
		}
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
		if err := dbaccess.Check(ctx, db, "live", "task_events", "task_refs"); err != nil {
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
	// The refs a task's node listed in its artifact events, which alone
	// the view-through asks a node for (SA1-M3, INV-11).
	for _, stmt := range []string{`CREATE TABLE IF NOT EXISTS task_refs (
	task TEXT NOT NULL,
	ref TEXT NOT NULL,
	seq BIGINT NOT NULL,
	PRIMARY KEY (task, ref)
)`, `CREATE INDEX IF NOT EXISTS task_refs_seq ON task_refs (task, seq)`} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("live: creating tables: %w", err)
		}
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
	return dbaccess.Retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO task_events (task, seq, type, class, body) VALUES (?, ?, ?, ?, ?)`),
			e.Task, e.Seq, e.Type, c.String(), string(b))
		return err
	})
}

// Since returns task's events with after < seq <= upto, in order.
func (s *Store) Since(ctx context.Context, task string, after, upto int64) ([]Event, error) {
	var out []Event
	err := dbaccess.Retry(ctx, func() error {
		out = nil
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT body FROM task_events WHERE task = ? AND seq > ? AND seq <= ? ORDER BY seq`), task, after, upto)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				return err
			}
			var e Event
			if err := json.Unmarshal([]byte(body), &e); err != nil {
				return fmt.Errorf("live: task %s's event: %w", task, err)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Size is how many events the store holds and how many bytes their
// bodies take: the hub's store-size metric.
func (s *Store) Size(ctx context.Context) (events, bytes int64, err error) {
	err = dbaccess.Retry(ctx, func() error {
		return s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(body)), 0) FROM task_events`).Scan(&events, &bytes)
	})
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
	err := dbaccess.Retry(ctx, func() error {
		seq, typ = 0, ""
		return s.db.QueryRowContext(ctx, s.q(`SELECT seq, type FROM task_events WHERE task = ? ORDER BY seq DESC LIMIT 1`), task).Scan(&seq, &typ)
	})
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return seq, typ, err
}

// AddRef keeps ref as one of task's listed refs, and drops the oldest
// past MaxRefs. A ref listed again keeps its first seq.
func (s *Store) AddRef(ctx context.Context, task, ref string, seq int64) error {
	if !validRef(ref) {
		return fmt.Errorf("live: not a ref: %q", ref)
	}
	var res sql.Result
	err := dbaccess.Retry(ctx, func() error {
		var err error
		res, err = s.db.ExecContext(ctx, s.q(`INSERT INTO task_refs (task, ref, seq) VALUES (?, ?, ?) ON CONFLICT (task, ref) DO NOTHING`), task, ref, seq)
		return err
	})
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil
	}
	return dbaccess.Retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM task_refs WHERE task = ? AND seq <= (
		SELECT seq FROM task_refs WHERE task = ? ORDER BY seq DESC LIMIT 1 OFFSET ?)`), task, task, MaxRefs)
		return err
	})
}

// HasRef is whether task's node listed ref.
func (s *Store) HasRef(ctx context.Context, task, ref string) (bool, error) {
	var n int
	err := dbaccess.Retry(ctx, func() error {
		n = 0
		return s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM task_refs WHERE task = ? AND ref = ?`), task, ref).Scan(&n)
	})
	return n > 0, err
}

// ApprovalRequest returns task's approval_request event with ID id, if
// its node sent one: tasks.API.Requests, which answers only a request
// the node made.
func (s *Store) ApprovalRequest(ctx context.Context, task, id string) (events.ApprovalRequest, bool, error) {
	var (
		found events.ApprovalRequest
		ok    bool
	)
	err := dbaccess.Retry(ctx, func() error {
		found = events.ApprovalRequest{}
		ok = false
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT body FROM task_events WHERE task = ? AND type = ? ORDER BY seq`), task, events.TypeApprovalRequest)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				return err
			}
			var e Event
			if err := json.Unmarshal([]byte(body), &e); err != nil {
				return fmt.Errorf("live: task %s's event: %w", task, err)
			}
			if a := e.ApprovalRequest; a != nil && a.ID == id {
				found = *a
				ok = true
				return nil
			}
		}
		return rows.Err()
	})
	return found, ok, err
}

// TaskInboxState inspects task's kept awaiting_reply, message, and
// approval_request events, reporting whether its latest awaiting_reply
// state is still waiting and returning all approval_request events in
// sequence order.
func (s *Store) TaskInboxState(ctx context.Context, task string) (awaiting bool, question string, awaitingAt time.Time, approvals []events.ApprovalRequest, approvalTimes []time.Time, err error) {
	err = dbaccess.Retry(ctx, func() error {
		awaiting = false
		question = ""
		awaitingAt = time.Time{}
		approvals = nil
		approvalTimes = nil
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT body FROM task_events WHERE task = ? AND type IN (?, ?, ?) ORDER BY seq`),
			task, events.TypeAwaitingReply, events.TypeMessage, events.TypeApprovalRequest)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				return err
			}
			var e Event
			if err := json.Unmarshal([]byte(body), &e); err != nil {
				return fmt.Errorf("live: task %s's event: %w", task, err)
			}
			switch e.Type {
			case events.TypeAwaitingReply:
				if ar := e.AwaitingReply; ar != nil {
					if ar.State == events.AwaitingWaiting {
						awaiting = true
						question = ar.Question
						awaitingAt = e.Time
					} else {
						awaiting = false
						question = ""
					}
				}
			case events.TypeMessage:
				awaiting = false
				question = ""
			case events.TypeApprovalRequest:
				if a := e.ApprovalRequest; a != nil {
					approvals = append(approvals, *a)
					approvalTimes = append(approvalTimes, e.Time)
				}
			}
		}
		return rows.Err()
	})
	return awaiting, question, awaitingAt, approvals, approvalTimes, err
}
