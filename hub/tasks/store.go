package tasks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/hub/dbaccess"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite"
)

// maxConns caps the backlog's PostgreSQL connections, all of which it
// keeps open once made: a connection made for each query, and closed
// after it, costs PostgreSQL a process, and a pool with no cap runs the
// server out of connections under load
// (docs/test-runs/2026-10-16-hub-load.md).
const maxConns = 16

// Store is the backlog: PostgreSQL, as the team directory's store is
// (SQLite only in local development and tests).
type Store struct {
	db       *sql.DB
	postgres bool
	// Now, when set, is the clock tasks are stamped with.
	Now func() time.Time
	// OnTerminal, when set, is called when a task transitions to a terminal
	// state (Done, Failed, or Cancelled).
	OnTerminal func(ctx context.Context, t Task)
}

// Open opens the backlog at url: "postgres://..." (or "postgresql://...")
// for PostgreSQL, "sqlite:PATH" or a bare path for SQLite (":memory:" for
// a throwaway one). It creates its table if it is missing; it may share
// a database with the team directory.
func Open(ctx context.Context, url string) (*Store, error) { return open(ctx, url, true) }

// OpenExisting opens the backlog at url as Open does, but creates
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
	if create {
		err = s.migrate(ctx)
	} else {
		err = dbaccess.Check(ctx, db, "tasks", "tasks", "task_approvals", "task_messages", "task_watchers")
		if err == nil {
			err = dbaccess.CheckColumns(ctx, db, "tasks", added...)
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// version counts a task's updates: an update applies only to the version
// it read, so the dispatcher and a human cancelling never both win.
const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	team TEXT NOT NULL,
	repo TEXT NOT NULL,
	title TEXT NOT NULL,
	prompt TEXT NOT NULL,
	submitter TEXT NOT NULL,
	approved_by TEXT NOT NULL DEFAULT '',
	approved_at BIGINT NOT NULL DEFAULT 0,
	constraints TEXT NOT NULL,
	state TEXT NOT NULL,
	node TEXT NOT NULL,
	node_owner TEXT NOT NULL DEFAULT '',
	node_task TEXT NOT NULL,
	reason TEXT NOT NULL,
	excluded TEXT NOT NULL,
	attempts INTEGER NOT NULL,
	cancel_pending INTEGER NOT NULL,
	created_at BIGINT NOT NULL,
	updated_at BIGINT NOT NULL,
	version BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_state ON tasks (state, created_at);
CREATE INDEX IF NOT EXISTS tasks_team ON tasks (team, created_at);
CREATE TABLE IF NOT EXISTS task_approvals (
	task TEXT NOT NULL,
	request TEXT NOT NULL,
	decision TEXT NOT NULL,
	answered_by TEXT NOT NULL,
	reason TEXT NOT NULL,
	answered_at BIGINT NOT NULL,
	delivered INTEGER NOT NULL,
	PRIMARY KEY (task, request)
);
CREATE TABLE IF NOT EXISTS task_messages (
	task TEXT NOT NULL,
	id TEXT NOT NULL,
	text TEXT NOT NULL,
	attachments TEXT NOT NULL,
	sender TEXT NOT NULL,
	sent_at BIGINT NOT NULL,
	delivered INTEGER NOT NULL,
	PRIMARY KEY (task, id)
);
CREATE TABLE IF NOT EXISTS task_watchers (
	task TEXT NOT NULL,
	node TEXT NOT NULL,
	node_owner TEXT NOT NULL,
	node_task TEXT NOT NULL,
	created_at BIGINT NOT NULL,
	delivered INTEGER NOT NULL,
	PRIMARY KEY (task, node, node_task)
);
CREATE INDEX IF NOT EXISTS task_watchers_pending ON task_watchers (delivered, task)
`

// added are the columns added to tasks since it was first made.
// node_owner (SA1-H1) is empty on a task offered before it: such a task,
// if lost, is never resumed, since its node's owner cannot be checked,
// and goes back to the queue after its team's grace.
// approved_by (INV-17) is empty on a task created before it, as it is
// on every task nobody approved at the hub, and approved_at (0) with it.
// kind is empty, an ordinary task, on a task created before it.
// depends_on, filed_by, filed_by_owner and origin_task (upward filing,
// docs/hub-interface.md) are a task with no dependencies that a human
// filed, on a task created before them.
var added = []dbaccess.Column{
	{Table: "tasks", Name: "node_owner", Add: `ALTER TABLE tasks ADD COLUMN node_owner TEXT NOT NULL DEFAULT ''`},
	{Table: "tasks", Name: "approved_by", Add: `ALTER TABLE tasks ADD COLUMN approved_by TEXT NOT NULL DEFAULT ''`},
	{Table: "tasks", Name: "approved_at", Add: `ALTER TABLE tasks ADD COLUMN approved_at BIGINT NOT NULL DEFAULT 0`},
	{Table: "tasks", Name: "kind", Add: `ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT ''`},
	{Table: "tasks", Name: "depends_on", Add: `ALTER TABLE tasks ADD COLUMN depends_on TEXT NOT NULL DEFAULT '[]'`},
	{Table: "tasks", Name: "filed_by", Add: `ALTER TABLE tasks ADD COLUMN filed_by TEXT NOT NULL DEFAULT ''`},
	{Table: "tasks", Name: "filed_by_owner", Add: `ALTER TABLE tasks ADD COLUMN filed_by_owner TEXT NOT NULL DEFAULT ''`},
	{Table: "tasks", Name: "origin_task", Add: `ALTER TABLE tasks ADD COLUMN origin_task TEXT NOT NULL DEFAULT ''`},
	{Table: "tasks", Name: "grainfile_url", Add: `ALTER TABLE tasks ADD COLUMN grainfile_url TEXT NOT NULL DEFAULT ''`},
	{Table: "tasks", Name: "commit_sha", Add: `ALTER TABLE tasks ADD COLUMN commit_sha TEXT NOT NULL DEFAULT ''`},
	{Table: "tasks", Name: "trigger_source", Add: `ALTER TABLE tasks ADD COLUMN trigger_source TEXT NOT NULL DEFAULT ''`},
}

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range strings.Split(schema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("tasks: creating tables: %w", err)
		}
	}
	return dbaccess.AddColumns(ctx, s.db, "tasks", added...)
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// q rewrites the ? placeholders of a query for PostgreSQL. No query
// here has a ? anywhere else.
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

const columns = `id, team, repo, kind, title, prompt, submitter, approved_by, approved_at, depends_on, filed_by, filed_by_owner, origin_task, grainfile_url, commit_sha, trigger_source, constraints, state, node, node_owner, node_task, reason, excluded, attempts, cancel_pending, created_at, updated_at, version`

type scanner interface{ Scan(...any) error }

func scanTask(r scanner) (Task, error) {
	var (
		t                Task
		cons, excl, deps string
		pending          int
		created, updated int64
		approved         int64
		state            string
	)
	if err := r.Scan(&t.ID, &t.Team, &t.Repo, &t.Kind, &t.Title, &t.Prompt, &t.Submitter, &t.ApprovedBy, &approved, &deps, &t.FiledBy, &t.FiledByOwner, &t.OriginTask, &t.GrainfileURL, &t.CommitSHA, &t.TriggerSource, &cons, &state, &t.Node, &t.NodeOwner, &t.NodeTask,
		&t.Reason, &excl, &t.Attempts, &pending, &created, &updated, &t.version); err != nil {
		return Task{}, err
	}
	t.State = State(state)
	t.CancelPending = pending != 0
	t.Created = time.UnixMilli(created).UTC()
	t.Updated = time.UnixMilli(updated).UTC()
	if approved != 0 {
		at := time.UnixMilli(approved).UTC()
		t.ApprovedAt = &at
	}
	if err := json.Unmarshal([]byte(cons), &t.Constraints); err != nil {
		return Task{}, fmt.Errorf("tasks: task %s's constraints: %w", t.ID, err)
	}
	if err := json.Unmarshal([]byte(excl), &t.Excluded); err != nil {
		return Task{}, fmt.Errorf("tasks: task %s's exclusions: %w", t.ID, err)
	}
	if err := json.Unmarshal([]byte(deps), &t.DependsOn); err != nil {
		return Task{}, fmt.Errorf("tasks: task %s's dependencies: %w", t.ID, err)
	}
	if len(t.DependsOn) == 0 {
		t.DependsOn = nil
	}
	return t, nil
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "t-" + hex.EncodeToString(b)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Create adds a task to the backlog, queued. It is given its ID and
// creation time; the caller has decided its team and submitter.
func (s *Store) Create(ctx context.Context, spec Spec) (Task, error) {
	return s.create(ctx, spec, Queued)
}

// Propose adds a task to the backlog, proposed: filed by someone outside
// its team, it waits for a member of the team to approve it (Approve)
// or reject it (Reject), and is not dispatched until it is approved.
func (s *Store) Propose(ctx context.Context, spec Spec) (Task, error) {
	spec.ApprovedBy, spec.ApprovedAt = "", nil
	return s.create(ctx, spec, Proposed)
}

func (s *Store) create(ctx context.Context, spec Spec, state State) (Task, error) {
	spec, err := normalize(spec)
	if err != nil {
		return Task{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	spec.ID, spec.Created = newID(), now
	var approved int64
	if spec.ApprovedBy == "" {
		spec.ApprovedAt = nil
	} else {
		if spec.ApprovedAt == nil {
			spec.ApprovedAt = &now
		}
		at := spec.ApprovedAt.UTC().Truncate(time.Millisecond)
		spec.ApprovedAt, approved = &at, at.UnixMilli()
	}
	t := Task{Spec: spec, State: state, Excluded: []string{}, Updated: now, version: 1}
	cons, _ := json.Marshal(t.Constraints)
	deps := []byte("[]")
	if len(t.DependsOn) > 0 {
		deps, _ = json.Marshal(t.DependsOn)
	}
	_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO tasks (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		t.ID, t.Team, t.Repo, t.Kind, t.Title, t.Prompt, t.Submitter, t.ApprovedBy, approved, string(deps), t.FiledBy, t.FiledByOwner, t.OriginTask, t.GrainfileURL, t.CommitSHA, t.TriggerSource, string(cons), string(t.State), "", "", "", "", "[]", 0, 0,
		now.UnixMilli(), now.UnixMilli(), t.version)
	if err != nil {
		return Task{}, err
	}
	return t, nil
}

// Get returns one task.
func (s *Store) Get(ctx context.Context, id string) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, s.q(`SELECT `+columns+` FROM tasks WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return t, err
}

// Filter narrows List.
type Filter struct {
	// States, when set, are the states to list.
	States []State
	// Teams, when set, are the teams whose tasks to list.
	Teams []string
	// Submitter, when set with Teams, also lists the tasks this
	// submitter filed in any team: their proposals to teams they are
	// not in.
	Submitter string
	// Limit caps the tasks returned; 0 is no cap.
	Limit int
}

// List returns the tasks f selects, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]Task, error) {
	return s.list(ctx, f, false)
}

// Dispatchable returns, oldest first, the tasks the dispatcher has
// something to do for: the queued ones, to place; the lost ones, to
// resume when their node is back; the running ones with undelivered
// approvals or messages; and the cancelled ones whose node has not
// been told.
func (s *Store) Dispatchable(ctx context.Context) ([]Task, error) {
	return s.query(ctx, `SELECT `+columns+` FROM tasks WHERE state IN (?, ?) OR cancel_pending = 1 OR (state = ? AND (EXISTS (SELECT 1 FROM task_approvals a WHERE a.task = tasks.id AND a.decision != '' AND a.delivered = 0) OR EXISTS (SELECT 1 FROM task_messages m WHERE m.task = tasks.id AND m.delivered = 0))) ORDER BY created_at, id`,
		[]any{string(Queued), string(Lost), string(Running)})
}

func inList(where []string, args []any, col string, vals []string) ([]string, []any) {
	for _, v := range vals {
		args = append(args, v)
	}
	return append(where, col+` IN (?`+strings.Repeat(", ?", len(vals)-1)+`)`), args
}

func (s *Store) list(ctx context.Context, f Filter, oldest bool) ([]Task, error) {
	var (
		where []string
		args  []any
	)
	if len(f.States) > 0 {
		var states []string
		for _, st := range f.States {
			states = append(states, string(st))
		}
		where, args = inList(where, args, "state", states)
	}
	if f.Teams != nil {
		switch {
		case len(f.Teams) == 0 && f.Submitter == "":
			return []Task{}, nil
		case len(f.Teams) == 0:
			where, args = append(where, `submitter = ?`), append(args, f.Submitter)
		case f.Submitter == "":
			where, args = inList(where, args, "team", f.Teams)
		default:
			var in []string
			in, args = inList(nil, args, "team", f.Teams)
			where, args = append(where, `(`+in[0]+` OR submitter = ?)`), append(args, f.Submitter)
		}
	}
	q := `SELECT ` + columns + ` FROM tasks`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	if oldest {
		q += ` ORDER BY created_at, id`
	} else {
		q += ` ORDER BY created_at DESC, id DESC`
	}
	if f.Limit > 0 {
		q += ` LIMIT ` + strconv.Itoa(f.Limit)
	}
	return s.query(ctx, q, args)
}

func (s *Store) query(ctx context.Context, q string, args []any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, s.q(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// update applies f to task id if it is in one of the states from, and
// writes it back. Its write applies only to the version it read; one
// that lost a race to another update reads the task again and retries.
func (s *Store) update(ctx context.Context, id string, from []State, f func(*Task)) (Task, error) {
	for {
		t, err := s.Get(ctx, id)
		if err != nil {
			return Task{}, err
		}
		if !slices.Contains(from, t.State) {
			return t, fmt.Errorf("%w: task %s is %s", ErrState, id, t.State)
		}
		if t, ok, err := s.write(ctx, id, t, f); ok {
			return t, err
		}
	}
}

// updateVersion applies f to task id only if it is still at version, as
// read by the caller; otherwise it fails with ErrState.
func (s *Store) updateVersion(ctx context.Context, id string, version int64, f func(*Task)) (Task, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if t.version != version {
		return t, fmt.Errorf("%w: task %s changed", ErrState, id)
	}
	if t, ok, err := s.write(ctx, id, t, f); ok {
		return t, err
	}
	return t, fmt.Errorf("%w: task %s changed", ErrState, id)
}

// write applies f to t, which was read at its version, and writes it
// back if the task is still at that version. ok is false, with no
// error, when another update got there first.
func (s *Store) write(ctx context.Context, id string, t Task, f func(*Task)) (_ Task, ok bool, _ error) {
	prevState := t.State
	f(&t)
	if !t.State.valid() {
		return Task{}, true, fmt.Errorf("%w state %q", ErrInvalid, t.State)
	}
	t.Updated = s.now().UTC().Truncate(time.Millisecond)
	if t.Excluded == nil {
		t.Excluded = []string{}
	}
	excl, _ := json.Marshal(t.Excluded)
	var approved int64
	if t.ApprovedAt != nil {
		approved = t.ApprovedAt.UnixMilli()
	}
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE tasks SET kind = ?, title = ?, prompt = ?, grainfile_url = ?, commit_sha = ?, trigger_source = ?, state = ?, approved_by = ?, approved_at = ?, node = ?, node_owner = ?, node_task = ?, reason = ?, excluded = ?,
		attempts = ?, cancel_pending = ?, updated_at = ?, version = ? WHERE id = ? AND version = ?`),
		t.Kind, t.Title, t.Prompt, t.GrainfileURL, t.CommitSHA, t.TriggerSource, string(t.State), t.ApprovedBy, approved, t.Node, t.NodeOwner, t.NodeTask, t.Reason, string(excl), t.Attempts, boolInt(t.CancelPending),
		t.Updated.UnixMilli(), t.version+1, id, t.version)
	if err != nil {
		return Task{}, true, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Task{}, false, nil
	}
	t.version++
	if !prevState.Terminal() && (t.State == Done || t.State == Failed || t.State == Cancelled) && s.OnTerminal != nil {
		s.OnTerminal(ctx, t)
	}
	return t, true, nil
}

// Approve queues a proposed task, approved by approver: a member of its
// team (the API's to check), who becomes its approved_by and may not be
// its submitter. A task is approved once: a second approval, or one of a
// task that was rejected or withdrawn, fails with ErrState.
func (s *Store) Approve(ctx context.Context, id, approver string) (Task, error) {
	approver = strings.ToLower(strings.TrimSpace(approver))
	if approver == "" {
		return Task{}, fmt.Errorf("%w: no approver", ErrInvalid)
	}
	t, err := s.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if strings.EqualFold(t.Submitter, approver) {
		return t, fmt.Errorf("%w: a task's submitter cannot approve it", ErrInvalid)
	}
	return s.update(ctx, id, []State{Proposed}, func(t *Task) {
		at := s.now().UTC().Truncate(time.Millisecond)
		t.State, t.ApprovedBy, t.ApprovedAt, t.Reason = Queued, approver, &at, ""
	})
}

// Reject turns down a proposed task, saying why.
func (s *Store) Reject(ctx context.Context, id, reason string) (Task, error) {
	return s.update(ctx, id, []State{Proposed}, func(t *Task) {
		t.State, t.Reason = Rejected, reason
	})
}

// Offer marks a queued task offered to node, which owner registered:
// the task is bound to that owner as well as to the node ID, which
// another owner may register later (SA1-H1).
func (s *Store) Offer(ctx context.Context, id, node, owner string) (Task, error) {
	return s.update(ctx, id, []State{Queued}, offerTo(node, owner))
}

// offerRead is Offer for a queued task as the dispatcher read it: one
// write, conditional on the task's version, with no read before it. A
// task that changed since it was read is read again, as Offer does.
func (s *Store) offerRead(ctx context.Context, t Task, node, owner string) (Task, error) {
	if t.State == Queued && t.version != 0 {
		if ot, ok, err := s.write(ctx, t.ID, t, offerTo(node, owner)); ok {
			return ot, err
		}
	}
	return s.Offer(ctx, t.ID, node, owner)
}

func offerTo(node, owner string) func(*Task) {
	return func(t *Task) {
		t.State, t.Node, t.NodeOwner, t.NodeTask, t.Reason = Offered, node, owner, "", ""
		t.Attempts++
	}
}

// Accept marks a task its node has accepted running: an offered task,
// or a lost one its node resumed.
func (s *Store) Accept(ctx context.Context, id, nodeTask string) (Task, error) {
	return s.update(ctx, id, []State{Offered, Lost}, func(t *Task) {
		t.State, t.Reason = Running, ""
		if nodeTask != "" {
			t.NodeTask = nodeTask
		}
	})
}

// Requeue returns a task to the queue, from the node it was offered to
// or lost on, saying why. A non-empty exclude is a node the task is not
// to be offered again: the one that refused it.
func (s *Store) Requeue(ctx context.Context, id, reason, exclude string) (Task, error) {
	return s.update(ctx, id, []State{Offered, Lost}, func(t *Task) {
		t.State, t.Node, t.NodeOwner, t.NodeTask, t.Reason = Queued, "", "", "", reason
		if exclude != "" && !slices.Contains(t.Excluded, exclude) {
			t.Excluded = append(t.Excluded, exclude)
		}
	})
}

// Retry is a human requeueing a task: a lost one, which goes back to
// the queue (its node, if it comes back, is not asked to resume it), or
// a queued one. Either way the task's exclusions are cleared, so a node
// that refused it is offered it again.
func (s *Store) Retry(ctx context.Context, id string) (Task, error) {
	return s.update(ctx, id, []State{Queued, Lost, Failed}, func(t *Task) {
		t.State, t.Node, t.NodeOwner, t.NodeTask, t.Reason = Queued, "", "", "", ""
		t.Excluded = []string{}
	})
}

// RequeueLost returns a lost task to the queue for any node, its own
// included: the dispatcher giving up on its node coming back
// (Dispatcher.LostRequeueAfter). It applies only to a task that is
// still lost, and has been since lostSince, so a task its node resumed
// in the meantime, even one lost again since, is left alone.
func (s *Store) RequeueLost(ctx context.Context, id, reason string, lostSince time.Time) (Task, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return t, err
	}
	if t.State != Lost || !t.Updated.Equal(lostSince) {
		return t, fmt.Errorf("%w: task %s is %s since %s", ErrState, id, t.State, t.Updated.Format(time.RFC3339))
	}
	return s.updateVersion(ctx, id, t.version, func(t *Task) {
		t.State, t.Node, t.NodeOwner, t.NodeTask, t.Reason = Queued, "", "", "", reason
	})
}

// Counts is how many tasks the backlog holds in each unfinished state,
// by team: the hub's backlog-depth metric. Finished tasks are not
// counted.
func (s *Store) Counts(ctx context.Context) (map[string]map[State]int, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT team, state, COUNT(*) FROM tasks WHERE state IN (?, ?, ?, ?, ?) GROUP BY team, state`),
		string(Proposed), string(Queued), string(Offered), string(Running), string(Lost))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[State]int{}
	for rows.Next() {
		var (
			team, state string
			n           int
		)
		if err := rows.Scan(&team, &state, &n); err != nil {
			return nil, err
		}
		if out[team] == nil {
			out[team] = map[State]int{}
		}
		out[team][State(state)] = n
	}
	return out, rows.Err()
}

// OldestQueued is, for each team with a queued task, when its oldest
// queued task was last queued (its last update): how long the team's
// queue has waited.
func (s *Store) OldestQueued(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT team, MIN(updated_at) FROM tasks WHERE state = ? GROUP BY team`), string(Queued))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var (
			team string
			ms   int64
		)
		if err := rows.Scan(&team, &ms); err != nil {
			return nil, err
		}
		out[team] = time.UnixMilli(ms).UTC()
	}
	return out, rows.Err()
}

// Stats is the store's connection pool's statistics.
func (s *Store) Stats() sql.DBStats { return s.db.Stats() }

// Lose marks a running task lost: its node went away.
func (s *Store) Lose(ctx context.Context, id, reason string) (Task, error) {
	return s.update(ctx, id, []State{Running}, func(t *Task) {
		t.State, t.Reason = Lost, reason
	})
}

// Finish records the result its node reported for a task: done or
// failed.
func (s *Store) Finish(ctx context.Context, id string, state State, reason string) (Task, error) {
	if state != Done && state != Failed {
		return Task{}, fmt.Errorf("%w result %q: want done or failed", ErrInvalid, state)
	}
	return s.update(ctx, id, []State{Offered, Running, Lost}, func(t *Task) {
		t.State, t.Reason = state, reason
	})
}

// Cancel cancels a task that has not finished: a proposal withdrawn, or
// a task stopped. One that a node holds, or may hold, is left
// CancelPending until the node is told (CancelDelivered).
func (s *Store) Cancel(ctx context.Context, id, reason string) (Task, error) {
	return s.update(ctx, id, []State{Proposed, Queued, Offered, Running, Lost}, func(t *Task) {
		t.CancelPending = t.State != Queued && t.State != Proposed
		t.State, t.Reason = Cancelled, reason
	})
}

// CancelDelivered records that a cancelled task's node has been told.
func (s *Store) CancelDelivered(ctx context.Context, id string) error {
	_, err := s.update(ctx, id, []State{Cancelled}, func(t *Task) { t.CancelPending = false })
	return err
}

// Recover is the dispatcher starting, with no task stream open: a task
// that was offered goes back to the queue, since no node can accept it
// on a stream that has gone, and one that was running via an imperative
// offer (Attempts > 0) is lost until its node comes back and resumes it.
// Node-originated runs registered directly in Running state (Attempts == 0)
// remain running.
func (s *Store) Recover(ctx context.Context, reason string) error {
	ts, err := s.list(ctx, Filter{States: []State{Offered, Running}}, true)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.State == Offered {
			_, err = s.Requeue(ctx, t.ID, reason, "")
		} else if t.Attempts > 0 {
			_, err = s.Lose(ctx, t.ID, reason)
		}
		if err != nil && !errors.Is(err, ErrState) {
			return err
		}
	}
	return nil
}

// CompleteRun records a terminal run result (Done, Failed, or Cancelled)
// reported by a node (e.g. via a NodeStream result event). If the task is
// already in a terminal state, CompleteRun returns it unchanged.
func (s *Store) CompleteRun(ctx context.Context, id string, state State, reason string) (Task, error) {
	if state != Done && state != Failed && state != Cancelled {
		return Task{}, fmt.Errorf("%w result %q: want done, failed, or cancelled", ErrInvalid, state)
	}
	reason = truncate(strings.TrimSpace(reason), maxReason)
	for {
		t, err := s.Get(ctx, id)
		if err != nil {
			return Task{}, err
		}
		if t.State.Terminal() {
			return t, nil
		}
		if updated, ok, err := s.write(ctx, id, t, func(t *Task) {
			t.State = state
			if reason != "" {
				t.Reason = reason
			}
			t.CancelPending = false
		}); ok {
			return updated, err
		}
	}
}

// RunRegistration describes a node-originated or Grainfile-triggered run
// announced by a connected node.
type RunRegistration struct {
	ID            string      `json:"id,omitempty"`
	Task          string      `json:"task,omitempty"`
	NodeTask      string      `json:"node_task,omitempty"`
	Team          string      `json:"team,omitempty"`
	Repo          string      `json:"repo,omitempty"`
	Kind          string      `json:"kind,omitempty"`
	Title         string      `json:"title,omitempty"`
	Prompt        string      `json:"prompt,omitempty"`
	Submitter     string      `json:"submitter,omitempty"`
	Node          string      `json:"node,omitempty"`
	NodeOwner     string      `json:"node_owner,omitempty"`
	GrainfileURL  string      `json:"grainfile_url,omitempty"`
	Grainfile     string      `json:"grainfile,omitempty"`
	CommitSHA     string      `json:"commit_sha,omitempty"`
	Commit        string      `json:"commit,omitempty"`
	TriggerSource string      `json:"trigger_source,omitempty"`
	Constraints   Constraints `json:"constraints,omitempty"`
}

// NormalizeRunRepo normalizes a repository identifier ("owner/name" or URL)
// and falls back to extracting "owner/name" from grainfileURL when repo is empty.
func NormalizeRunRepo(repo, grainfileURL string) string {
	r := strings.ToLower(strings.TrimSpace(repo))
	if strings.HasPrefix(r, "https://") || strings.HasPrefix(r, "http://") {
		if parsed := repoFromURL(r); parsed != "" {
			r = parsed
		}
	}
	r = strings.TrimSuffix(r, ".git")
	if r == "" && grainfileURL != "" {
		r = repoFromURL(grainfileURL)
	}
	return r
}

func repoFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	owner := strings.ToLower(parts[0])
	name := strings.ToLower(strings.TrimSuffix(parts[1], ".git"))
	candidate := owner + "/" + name
	if repoName.MatchString(candidate) {
		return candidate
	}
	return ""
}

func titleFromGrainfileURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Path == "" {
		return strings.TrimSpace(raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 {
		return strings.TrimSpace(raw)
	}
	return parts[len(parts)-1]
}

// RegisterRun registers a node-originated or Grainfile-triggered run directly
// in the Running state bound to (r.Node, r.NodeOwner), bypassing imperative
// Dispatcher offer/accept. If a task with r.ID already exists on the same
// node and owner, RegisterRun idempotently updates its metadata and returns it.
func (s *Store) RegisterRun(ctx context.Context, r RunRegistration) (Task, error) {
	id := strings.TrimSpace(r.ID)
	if id == "" {
		id = strings.TrimSpace(r.Task)
	}
	if id == "" {
		id = newID()
	}
	if len(id) > 128 || strings.ContainsAny(id, "/ \t\r\n\x00") {
		return Task{}, fmt.Errorf("%w run id %q", ErrInvalid, id)
	}
	node := strings.TrimSpace(r.Node)
	if !nodeID.MatchString(node) {
		return Task{}, fmt.Errorf("%w node %q", ErrInvalid, node)
	}
	owner := strings.TrimSpace(r.NodeOwner)
	if owner == "" {
		return Task{}, fmt.Errorf("%w: no node owner", ErrInvalid)
	}
	team := strings.ToLower(strings.TrimSpace(r.Team))
	if !teamID.MatchString(team) {
		return Task{}, fmt.Errorf("%w team %q", ErrInvalid, team)
	}
	grainfileURL := strings.TrimSpace(r.GrainfileURL)
	if grainfileURL == "" {
		grainfileURL = strings.TrimSpace(r.Grainfile)
	}
	commitSHA := strings.TrimSpace(r.CommitSHA)
	if commitSHA == "" {
		commitSHA = strings.TrimSpace(r.Commit)
	}
	triggerSource := strings.TrimSpace(r.TriggerSource)
	nodeTask := strings.TrimSpace(r.NodeTask)
	repo := NormalizeRunRepo(r.Repo, grainfileURL)
	if repo != "" && !repoName.MatchString(repo) {
		return Task{}, fmt.Errorf("%w repository %q: want owner/name", ErrInvalid, repo)
	}
	kind := strings.ToLower(strings.TrimSpace(r.Kind))
	if kind == "" {
		lowerGF := strings.ToLower(grainfileURL)
		if strings.HasSuffix(lowerGF, ".process.grain.md") || strings.HasSuffix(lowerGF, ".process.md") {
			kind = KindProcess
		} else {
			kind = "task"
		}
	}
	if kind != KindTask && kind != "task" && kind != KindProcess && kind != KindInteractive {
		return Task{}, fmt.Errorf("%w kind %q", ErrInvalid, kind)
	}
	title := strings.TrimSpace(r.Title)
	prompt := strings.TrimSpace(r.Prompt)
	if title == "" && prompt != "" {
		title, _, _ = strings.Cut(prompt, "\n")
		title = strings.TrimSpace(title)
	}
	if title == "" && grainfileURL != "" {
		title = titleFromGrainfileURL(grainfileURL)
	}
	if title == "" && nodeTask != "" {
		title = nodeTask
	}
	if title == "" {
		title = id
	}
	title = truncate(title, maxTitle)
	if prompt == "" {
		if grainfileURL != "" {
			prompt = grainfileURL
		} else {
			prompt = title
		}
	}
	if len(prompt) > maxPrompt {
		return Task{}, fmt.Errorf("%w: the prompt is over %d bytes", ErrInvalid, maxPrompt)
	}
	submitter := strings.ToLower(strings.TrimSpace(r.Submitter))
	if submitter == "" {
		submitter = strings.ToLower(strings.TrimPrefix(owner, "user:"))
	}
	if submitter == "" {
		submitter = team
	}

	for {
		existing, err := s.Get(ctx, id)
		if err == nil {
			if existing.Node != "" && existing.Node != node {
				return Task{}, fmt.Errorf("%w: task %s is already bound to node %s", ErrInvalid, id, existing.Node)
			}
			if existing.NodeOwner != "" && existing.NodeOwner != owner {
				return Task{}, fmt.Errorf("%w: task %s is already bound to another owner", ErrInvalid, id)
			}
			updated, ok, err := s.write(ctx, id, existing, func(t *Task) {
				if t.Node == "" {
					t.Node = node
				}
				if t.NodeOwner == "" {
					t.NodeOwner = owner
				}
				if nodeTask != "" {
					t.NodeTask = nodeTask
				}
				if grainfileURL != "" {
					t.GrainfileURL = grainfileURL
				}
				if commitSHA != "" {
					t.CommitSHA = commitSHA
				}
				if triggerSource != "" {
					t.TriggerSource = triggerSource
				}
				if r.Kind != "" || t.Kind == "" {
					t.Kind = kind
				}
				if r.Title != "" {
					t.Title = title
				}
				if r.Prompt != "" {
					t.Prompt = prompt
				}
			})
			if ok {
				return updated, err
			}
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return Task{}, err
		}

		now := s.now().UTC().Truncate(time.Millisecond)
		t := Task{
			Spec: Spec{
				ID:            id,
				Team:          team,
				Repo:          repo,
				Kind:          kind,
				Title:         title,
				Prompt:        prompt,
				Submitter:     submitter,
				GrainfileURL:  grainfileURL,
				CommitSHA:     commitSHA,
				TriggerSource: triggerSource,
				Constraints:   r.Constraints,
				Created:       now,
			},
			State:     Running,
			Node:      node,
			NodeOwner: owner,
			NodeTask:  nodeTask,
			Excluded:  []string{},
			Attempts:  0,
			Updated:   now,
			version:   1,
		}
		cons, _ := json.Marshal(t.Constraints)
		_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO tasks (`+columns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			t.ID, t.Team, t.Repo, t.Kind, t.Title, t.Prompt, t.Submitter, "", 0, "[]", t.FiledBy, t.FiledByOwner, t.OriginTask, t.GrainfileURL, t.CommitSHA, t.TriggerSource, string(cons), string(t.State), t.Node, t.NodeOwner, t.NodeTask, "", "[]", 0, 0,
			now.UnixMilli(), now.UnixMilli(), t.version)
		if err != nil {
			// If a concurrent call inserted the same ID first, retry and update.
			if _, gerr := s.Get(ctx, id); gerr == nil {
				continue
			}
			return Task{}, err
		}
		return t, nil
	}
}
