package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/bwsalmon/silo/hub/dbaccess"
)

// A running task's Grain may ask for a human's approval before it goes
// on, with an approval_request event (package events, docs/hub.md
// §7.1). A member of the task's team answers it at the hub
// (API.answer), which keeps the answer here and writes it to the node
// down the task's stream as an approve message (stream.go), the way a
// cancel is delivered. An answer the node has not yet been sent, because
// its stream ended first, is sent when the node next accepts the task:
// when it resumes it.

// Decisions an answer gives.
const (
	Allow = "allow"
	Deny  = "deny"
)

// Answer is a human's answer to one of a task's approval requests.
type Answer struct {
	Task string `json:"task"`
	// Request is the approval_request event's ID.
	Request string `json:"request_id"`
	// Decision is Allow or Deny.
	Decision string `json:"decision"`
	// AnsweredBy is the member of the task's team who answered.
	AnsweredBy string    `json:"answered_by"`
	Reason     string    `json:"reason,omitempty"`
	AnsweredAt time.Time `json:"answered_at"`
	// Delivered is whether the task's node has been sent the answer.
	Delivered bool `json:"delivered"`
}

// maxRequestID bounds an approval request's ID.
const maxRequestID = 200

// Answer keeps a's answer to a's request, once: a request already
// answered fails with ErrState, whoever answered it.
func (s *Store) Answer(ctx context.Context, a Answer) (Answer, error) {
	switch {
	case a.Decision != Allow && a.Decision != Deny:
		return Answer{}, fmt.Errorf("%w decision %q: want allow or deny", ErrInvalid, a.Decision)
	case a.Request == "" || len(a.Request) > maxRequestID:
		return Answer{}, fmt.Errorf("%w approval request ID", ErrInvalid)
	case a.AnsweredBy == "":
		return Answer{}, fmt.Errorf("%w: no answerer", ErrInvalid)
	case len(a.Reason) > maxReason:
		return Answer{}, fmt.Errorf("%w: the reason is over %d bytes", ErrInvalid, maxReason)
	}
	a.AnsweredAt, a.Delivered = s.now().UTC().Truncate(time.Millisecond), false
	var n int64
	err := dbaccess.Retry(ctx, func() error {
		res, err := s.db.ExecContext(ctx, s.q(`INSERT INTO task_approvals (task, request, decision, answered_by, reason, answered_at, delivered)
			VALUES (?, ?, ?, ?, ?, ?, 0) ON CONFLICT (task, request) DO NOTHING`),
			a.Task, a.Request, a.Decision, a.AnsweredBy, a.Reason, a.AnsweredAt.UnixMilli())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return Answer{}, err
	}
	if n == 0 {
		return Answer{}, fmt.Errorf("%w: approval request %s of task %s is already answered", ErrState, a.Request, a.Task)
	}
	return a, nil
}

// Answers returns task's answers, oldest first; with undelivered, only
// those its node has not been sent.
func (s *Store) Answers(ctx context.Context, task string, undelivered bool) ([]Answer, error) {
	q := `SELECT task, request, decision, answered_by, reason, answered_at, delivered FROM task_approvals WHERE task = ?`
	if undelivered {
		q += ` AND delivered = 0`
	}
	var out []Answer
	err := dbaccess.Retry(ctx, func() error {
		out = []Answer{}
		rows, err := s.db.QueryContext(ctx, s.q(q+` ORDER BY answered_at, request`), task)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				a         Answer
				at        int64
				delivered int
			)
			if err := rows.Scan(&a.Task, &a.Request, &a.Decision, &a.AnsweredBy, &a.Reason, &at, &delivered); err != nil {
				return err
			}
			a.AnsweredAt, a.Delivered = time.UnixMilli(at).UTC(), delivered != 0
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// AnswerDelivered records that task's node has been sent the answer to
// request.
func (s *Store) AnswerDelivered(ctx context.Context, task, request string) error {
	return dbaccess.Retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, s.q(`UPDATE task_approvals SET delivered = 1 WHERE task = ? AND request = ?`), task, request)
		return err
	})
}

// approveMessage is a's approve message.
func approveMessage(a Answer) Message {
	return Message{Type: TypeApprove, Task: &Spec{ID: a.Task}, Request: a.Request, Decision: a.Decision, AnsweredBy: a.AnsweredBy, Reason: a.Reason}
}
