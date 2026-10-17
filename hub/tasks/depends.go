package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A task's dependencies (Spec.DependsOn) are hub tasks it waits for: the
// dispatcher places it only once each of them is done, and cancels it
// if one of them ends any other way (Dispatcher.pass). A watcher is a
// node's own task waiting on a hub task: the node that filed the hub
// task asked to be told when it ends (docs/hub-interface.md, filing a
// task), and the dispatcher tells it, once, on a stream of its own
// (TypeDependencyDone).

// Ended is a hub task's end as a dependency sees it: its state, done,
// failed, cancelled or rejected, and why.
type Ended struct {
	ID     string `json:"id"`
	State  State  `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// States returns the state of each of ids that exists, by ID.
func (s *Store) States(ctx context.Context, ids []string) (map[string]Ended, error) {
	out := map[string]Ended{}
	for len(ids) > 0 {
		n := min(len(ids), maxIDsPerQuery)
		var args []any
		for _, id := range ids[:n] {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, state, reason FROM tasks WHERE id IN (?`+strings.Repeat(", ?", n-1)+`)`), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var e Ended
			var st string
			if err := rows.Scan(&e.ID, &st, &e.Reason); err != nil {
				rows.Close()
				return nil, err
			}
			e.State = State(st)
			out[e.ID] = e
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		ids = ids[n:]
	}
	return out, nil
}

// maxIDsPerQuery bounds the placeholders in one of States' queries.
const maxIDsPerQuery = 500

// Watcher is a node's local task waiting on a hub task.
type Watcher struct {
	// Task is the hub task waited on.
	Task string `json:"task"`
	// Node is the node to tell, and NodeOwner the owner it was
	// registered by when it asked: only a node registered by that owner
	// is told, as only it is told of a task's cancel.
	Node      string `json:"node"`
	NodeOwner string `json:"node_owner"`
	// NodeTask is the node's own ID for its waiting task.
	NodeTask string    `json:"node_task"`
	Created  time.Time `json:"created_at"`
	// Ended is how Task ended, in Store.Unnotified; empty otherwise.
	Ended Ended `json:"-"`
}

// maxNodeTask bounds a node's own ID for a task.
const maxNodeTask = 200

// Watch records that w.Node's local task w.NodeTask waits on hub task
// w.Task, to be told when it ends. Watching again is not an error.
func (s *Store) Watch(ctx context.Context, w Watcher) error {
	if w.NodeTask == "" || len(w.NodeTask) > maxNodeTask || strings.ContainsAny(w.NodeTask, "\r\n") {
		return fmt.Errorf("%w node task %q", ErrInvalid, w.NodeTask)
	}
	if !nodeID.MatchString(w.Node) || w.NodeOwner == "" {
		return fmt.Errorf("%w watching node %q", ErrInvalid, w.Node)
	}
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO task_watchers (task, node, node_owner, node_task, created_at, delivered)
		VALUES (?, ?, ?, ?, ?, 0) ON CONFLICT (task, node, node_task) DO NOTHING`),
		w.Task, w.Node, w.NodeOwner, w.NodeTask, s.now().UTC().UnixMilli())
	return err
}

// Watchers returns the watchers of task, delivered or not.
func (s *Store) Watchers(ctx context.Context, task string) ([]Watcher, error) {
	return s.watchers(ctx, `SELECT w.task, w.node, w.node_owner, w.node_task, w.created_at, t.state, t.reason
		FROM task_watchers w JOIN tasks t ON t.id = w.task WHERE w.task = ? ORDER BY w.created_at`, task)
}

// Unnotified returns the watchers not yet told of the end of the task
// they watch, which has ended, with how it ended.
func (s *Store) Unnotified(ctx context.Context) ([]Watcher, error) {
	return s.watchers(ctx, `SELECT w.task, w.node, w.node_owner, w.node_task, w.created_at, t.state, t.reason
		FROM task_watchers w JOIN tasks t ON t.id = w.task WHERE w.delivered = 0 AND t.state IN (?, ?, ?, ?) ORDER BY w.created_at`,
		string(Done), string(Failed), string(Cancelled), string(Rejected))
}

func (s *Store) watchers(ctx context.Context, q string, args ...any) ([]Watcher, error) {
	rows, err := s.db.QueryContext(ctx, s.q(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Watcher{}
	for rows.Next() {
		var (
			w       Watcher
			created int64
			st      string
		)
		if err := rows.Scan(&w.Task, &w.Node, &w.NodeOwner, &w.NodeTask, &created, &st, &w.Ended.Reason); err != nil {
			return nil, err
		}
		w.Created = time.UnixMilli(created).UTC()
		w.Ended.ID, w.Ended.State = w.Task, State(st)
		if !w.Ended.State.Terminal() {
			w.Ended = Ended{}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Notified records that w's node has been told its task ended.
func (s *Store) Notified(ctx context.Context, w Watcher) error {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE task_watchers SET delivered = 1 WHERE task = ? AND node = ? AND node_task = ?`), w.Task, w.Node, w.NodeTask)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: no watcher %s of %s on %s", ErrNotFound, w.NodeTask, w.Task, w.Node)
	}
	return nil
}

// blocked says, of a queued task with dependencies, whether it must
// wait for them (some are not over yet), or is to be cancelled (one of
// them ended other than done), with why; neither is a task whose
// dependencies are all done. deps holds the dependencies' states.
func blocked(t Task, deps map[string]Ended) (wait bool, cancel string) {
	for _, id := range t.DependsOn {
		e, ok := deps[id]
		switch {
		case !ok:
			return false, "dependency " + id + " does not exist"
		case e.State == Done:
		case e.State.Terminal():
			return false, "dependency " + id + " is " + string(e.State)
		default:
			wait = true
		}
	}
	return wait, ""
}

// dependencies reads the state of every dependency of ts.
func (s *Store) dependencies(ctx context.Context, ts []Task) (map[string]Ended, error) {
	var ids []string
	seen := map[string]bool{}
	for _, t := range ts {
		for _, id := range t.DependsOn {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return map[string]Ended{}, nil
	}
	return s.States(ctx, ids)
}
