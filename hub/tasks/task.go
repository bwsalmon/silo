// Package tasks is the Silo hub's backlog and dispatcher (docs/hub.md
// §6): the tasks humans and custom UIs submit (POST /v1/tasks), kept in
// a store until a node has taken them, and the dispatcher that places
// each one on a Grain node that can run it.
//
// A task targets a repository or a team, and goes through
//
//	queued → offered → running → done | failed | cancelled
//	            ↘ queued (refused, or the node was lost before it accepted)
//	                        ↘ lost (the node was lost while it ran)
//
// The dispatcher claims a slot on a node (registry.ClaimFor), opens a
// task stream to it (rendezvous.Server.Open) and offers the task there;
// the node accepts or refuses on the same stream. The stream stays open
// for the task's life: the node reports the task's result on it, and the
// hub writes a cancel down it. The protocol is in stream.go.
//
// The store keeps a task's metadata and lifecycle only: never a
// checkout, a diff or a log (docs/hub.md §1).
package tasks

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// State is where a task is in its life.
type State string

const (
	// Queued is a task waiting for a node.
	Queued State = "queued"
	// Offered is a task offered to a node that has not yet answered.
	Offered State = "offered"
	// Running is a task a node has accepted.
	Running State = "running"
	// Done is a task its node finished.
	Done State = "done"
	// Failed is a task its node gave up on.
	Failed State = "failed"
	// Cancelled is a task a human cancelled.
	Cancelled State = "cancelled"
	// Lost is a running task whose node went away. The node still owns
	// it: when the node comes back, the dispatcher asks it to resume the
	// task; a human may requeue it instead.
	Lost State = "lost"
)

// Terminal is whether s is a state a task does not leave.
func (s State) Terminal() bool { return s == Done || s == Failed || s == Cancelled }

func (s State) valid() bool {
	switch s {
	case Queued, Offered, Running, Done, Failed, Cancelled, Lost:
		return true
	}
	return false
}

// Constraints narrow the nodes a task may be placed on.
type Constraints struct {
	// Labels a node must carry, each with this value.
	Labels map[string]string `json:"labels,omitempty"`
	// Node is the one node the task may run on.
	Node string `json:"node,omitempty"`
}

// Spec is a task as it was submitted: what a node is offered.
type Spec struct {
	ID string `json:"id"`
	// Team is the team the task belongs to: the one it targets, or the
	// one that owned its repository when it was submitted. Tasks are
	// dispatched oldest first per team, and seen by its members.
	Team string `json:"team"`
	// Repo is the repository the task targets, "owner/name"; empty for a
	// task that targets a team, whose node's grain chooses the
	// repository.
	Repo        string      `json:"repo,omitempty"`
	Title       string      `json:"title"`
	Prompt      string      `json:"prompt"`
	Submitter   string      `json:"submitter"`
	Constraints Constraints `json:"constraints"`
	Created     time.Time   `json:"created_at"`
}

// Task is a task in the backlog.
type Task struct {
	Spec
	State State `json:"state"`
	// Node is the node the task is offered to or runs on; for a lost or
	// finished task, the node it last ran on.
	Node string `json:"node,omitempty"`
	// NodeTask is the node's own ID for the task, from its accept.
	NodeTask string `json:"node_task,omitempty"`
	// Reason says why the task is in its state: a refusal, a failure, a
	// lost node.
	Reason string `json:"reason,omitempty"`
	// Excluded are the nodes that refused the task; it is not offered to
	// them again unless a human requeues it.
	Excluded []string `json:"excluded,omitempty"`
	// Attempts counts the task's offers.
	Attempts int       `json:"attempts"`
	Updated  time.Time `json:"updated_at"`
	// CancelPending is a cancelled task whose node has not yet been told:
	// it was lost when it was cancelled, or its stream ended first.
	CancelPending bool `json:"cancel_pending,omitempty"`

	version int64
}

// Errors the store returns.
var (
	// ErrNotFound is a task that does not exist.
	ErrNotFound = errors.New("tasks: no such task")
	// ErrState is a transition the task's state does not allow, such as
	// cancelling a finished task.
	ErrState = errors.New("tasks: not allowed in the task's state")
	// ErrInvalid is a malformed task.
	ErrInvalid = errors.New("tasks: invalid")
)

// Limits on what a task may carry.
const (
	maxTitle  = 200
	maxPrompt = 256 << 10
	// maxReason is the longest reason a human's cancel may give, and
	// what a node's refusal or result reason is cut to before it is
	// stored and shown (threat model O-7).
	maxReason = 1 << 10
	// Label constraints, Kubernetes-sized.
	maxLabels     = 32
	maxLabelKey   = 63
	maxLabelValue = 255
)

var (
	teamID   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	repoName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9._-]+$`)
	nodeID   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
)

// normalize checks a submitted spec and fills in what it leaves out: a
// lower-cased repository, and a title from the prompt's first line.
func normalize(s Spec) (Spec, error) {
	s.Repo = strings.ToLower(strings.TrimSpace(s.Repo))
	if s.Repo != "" && !repoName.MatchString(s.Repo) {
		return s, fmt.Errorf("%w repository %q: want owner/name", ErrInvalid, s.Repo)
	}
	if !teamID.MatchString(s.Team) {
		return s, fmt.Errorf("%w team %q", ErrInvalid, s.Team)
	}
	if strings.TrimSpace(s.Prompt) == "" {
		return s, fmt.Errorf("%w: no prompt", ErrInvalid)
	}
	if len(s.Prompt) > maxPrompt {
		return s, fmt.Errorf("%w: the prompt is over %d bytes", ErrInvalid, maxPrompt)
	}
	if s.Submitter == "" {
		return s, fmt.Errorf("%w: no submitter", ErrInvalid)
	}
	s.Title = strings.TrimSpace(s.Title)
	if s.Title == "" {
		s.Title, _, _ = strings.Cut(strings.TrimSpace(s.Prompt), "\n")
		s.Title = truncate(strings.TrimSpace(s.Title), maxTitle)
	}
	if utf8.RuneCountInString(s.Title) > maxTitle {
		return s, fmt.Errorf("%w: the title is over %d characters", ErrInvalid, maxTitle)
	}
	if n := s.Constraints.Node; n != "" && !nodeID.MatchString(n) {
		return s, fmt.Errorf("%w node %q", ErrInvalid, n)
	}
	if len(s.Constraints.Labels) > maxLabels {
		return s, fmt.Errorf("%w: over %d labels", ErrInvalid, maxLabels)
	}
	for k, v := range s.Constraints.Labels {
		if k == "" {
			return s, fmt.Errorf("%w: an empty label", ErrInvalid)
		}
		if len(k) > maxLabelKey || len(v) > maxLabelValue {
			return s, fmt.Errorf("%w: a label over %d bytes or a value over %d", ErrInvalid, maxLabelKey, maxLabelValue)
		}
	}
	return s, nil
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
