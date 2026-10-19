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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwsalmon/silo/events"
	"github.com/bwsalmon/silo/hub/dbaccess"
)

// An interactive task's humans talk to its session through the hub
// (API.post): the hub keeps each message here, with who sent it, and
// writes it to the node down the task's stream as a message message
// (stream.go), the way an approval's answer is delivered. A message the
// node has not yet been sent, because the task has not started, its
// node is away or its stream ended first, is kept and sent when the
// node next accepts the task. Delivery is at least once: a node may be
// sent a message again, and tells the copies apart by its ID.
//
// The session reports a message it has taken in with a message event
// naming its ID; the hub keeps and broadcasts that event with its own
// record of the message (MessageEvent), so the conversation shows each
// message where the session took it in, as the hub has it.

// TaskMessage is a human's message to an interactive task.
type TaskMessage struct {
	Task string `json:"task"`
	// ID names the message within its task: the sender's, so that a
	// retried post is not sent twice, or else the hub's.
	ID          string              `json:"id"`
	Text        string              `json:"text"`
	Attachments []events.Attachment `json:"attachments"`
	// Sender is who sent it, from their sign-on to the hub.
	Sender string    `json:"sender"`
	SentAt time.Time `json:"sent_at"`
	// Delivered is whether the task's node has been sent the message.
	Delivered bool `json:"delivered"`
}

// Limits on what a message may carry.
const (
	maxMessageID   = 200
	maxMessageText = 64 << 10
	maxAttachments = 16
	maxAttachName  = 255
	maxAttachRef   = 2048
	maxAttachMedia = 127
)

func newMessageID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "m-" + hex.EncodeToString(b)
}

// checkMessage checks a message as it was posted. An attachment is a
// reference only, an https URL or one of the task's artifact refs: the
// hub keeps no file a human uploads.
func checkMessage(m TaskMessage) error {
	switch {
	case len(m.ID) > maxMessageID || !utf8.ValidString(m.ID) || strings.ContainsFunc(m.ID, isControl):
		return fmt.Errorf("%w message ID", ErrInvalid)
	case strings.TrimSpace(m.Text) == "" && len(m.Attachments) == 0:
		return fmt.Errorf("%w: an empty message", ErrInvalid)
	case len(m.Text) > maxMessageText:
		return fmt.Errorf("%w: the message is over %d bytes", ErrInvalid, maxMessageText)
	case len(m.Attachments) > maxAttachments:
		return fmt.Errorf("%w: over %d attachments", ErrInvalid, maxAttachments)
	case m.Sender == "":
		return fmt.Errorf("%w: no sender", ErrInvalid)
	}
	for _, a := range m.Attachments {
		switch {
		case a.Name == "" || len(a.Name) > maxAttachName || strings.ContainsFunc(a.Name, isControl):
			return fmt.Errorf("%w attachment name %q", ErrInvalid, a.Name)
		case a.Ref == "" || len(a.Ref) > maxAttachRef || strings.ContainsFunc(a.Ref, isControl):
			return fmt.Errorf("%w attachment %q: no ref, or one over %d bytes", ErrInvalid, a.Name, maxAttachRef)
		case len(a.MediaType) > maxAttachMedia || a.Size < 0:
			return fmt.Errorf("%w attachment %q", ErrInvalid, a.Name)
		}
		if strings.Contains(a.Ref, "://") {
			if u, err := url.Parse(a.Ref); err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("%w attachment %q: a URL must be https", ErrInvalid, a.Name)
			}
		}
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// Post keeps m, a message to its task, for its node to be sent. Its ID
// is the hub's when it brings none. A message posted again with an ID
// the task already has is the same message retried: Post returns the
// one kept, with dup set, and keeps nothing new. One whose sender or
// text differs from the one kept fails with ErrState.
func (s *Store) Post(ctx context.Context, m TaskMessage) (_ TaskMessage, dup bool, err error) {
	m.Sender = strings.ToLower(strings.TrimSpace(m.Sender))
	if m.ID == "" {
		m.ID = newMessageID()
	}
	if m.Attachments == nil {
		m.Attachments = []events.Attachment{}
	}
	if err := checkMessage(m); err != nil {
		return TaskMessage{}, false, err
	}
	m.SentAt, m.Delivered = s.now().UTC().Truncate(time.Millisecond), false
	att, _ := json.Marshal(m.Attachments)
	var n int64
	err = dbaccess.Retry(ctx, func() error {
		res, err := s.db.ExecContext(ctx, s.q(`INSERT INTO task_messages (task, id, text, attachments, sender, sent_at, delivered)
			VALUES (?, ?, ?, ?, ?, ?, 0) ON CONFLICT (task, id) DO NOTHING`),
			m.Task, m.ID, m.Text, string(att), m.Sender, m.SentAt.UnixMilli())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return TaskMessage{}, false, err
	}
	if n > 0 {
		return m, false, nil
	}
	kept, ok, err := s.message(ctx, m.Task, m.ID)
	switch {
	case err != nil:
		return TaskMessage{}, false, err
	case !ok:
		return TaskMessage{}, false, fmt.Errorf("tasks: message %s of task %s vanished", m.ID, m.Task)
	case kept.Sender != m.Sender || kept.Text != m.Text:
		return TaskMessage{}, false, fmt.Errorf("%w: task %s already has a different message %s", ErrState, m.Task, m.ID)
	}
	return kept, true, nil
}

const messageColumns = `task, id, text, attachments, sender, sent_at, delivered`

func scanMessage(r scanner) (TaskMessage, error) {
	var (
		m         TaskMessage
		att       string
		at        int64
		delivered int
	)
	if err := r.Scan(&m.Task, &m.ID, &m.Text, &att, &m.Sender, &at, &delivered); err != nil {
		return TaskMessage{}, err
	}
	m.SentAt, m.Delivered = time.UnixMilli(at).UTC(), delivered != 0
	if err := json.Unmarshal([]byte(att), &m.Attachments); err != nil {
		return TaskMessage{}, fmt.Errorf("tasks: message %s's attachments: %w", m.ID, err)
	}
	return m, nil
}

func (s *Store) message(ctx context.Context, task, id string) (TaskMessage, bool, error) {
	var m TaskMessage
	err := dbaccess.Retry(ctx, func() error {
		var err error
		m, err = scanMessage(s.db.QueryRowContext(ctx, s.q(`SELECT `+messageColumns+` FROM task_messages WHERE task = ? AND id = ?`), task, id))
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TaskMessage{}, false, nil
		}
		return TaskMessage{}, false, err
	}
	return m, true, nil
}

// Messages returns task's messages, oldest first; with undelivered,
// only those its node has not been sent.
func (s *Store) Messages(ctx context.Context, task string, undelivered bool) ([]TaskMessage, error) {
	q := `SELECT ` + messageColumns + ` FROM task_messages WHERE task = ?`
	if undelivered {
		q += ` AND delivered = 0`
	}
	var out []TaskMessage
	err := dbaccess.Retry(ctx, func() error {
		out = []TaskMessage{}
		rows, err := s.db.QueryContext(ctx, s.q(q+` ORDER BY sent_at, id`), task)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// MessageDelivered records that task's node has been sent message id.
func (s *Store) MessageDelivered(ctx context.Context, task, id string) error {
	return dbaccess.Retry(ctx, func() error {
		_, err := s.db.ExecContext(ctx, s.q(`UPDATE task_messages SET delivered = 1 WHERE task = ? AND id = ?`), task, id)
		return err
	})
}

// MessageEvent is the hub's record of task's message id as a message
// event carries it, if the hub has one: live.Hub.Messages.
func (s *Store) MessageEvent(ctx context.Context, task, id string) (events.Message, bool, error) {
	m, ok, err := s.message(ctx, task, id)
	if !ok || err != nil {
		return events.Message{}, false, err
	}
	return m.event(), true, nil
}

func (m TaskMessage) event() events.Message {
	e := events.Message{ID: m.ID, Text: m.Text, Sender: m.Sender, SentAt: m.SentAt}
	if len(m.Attachments) > 0 {
		e.Attachments = m.Attachments
	}
	return e
}

// messageMessage is m's message message, on the task stream.
func messageMessage(m TaskMessage) Message {
	e := m.event()
	return Message{Type: TypeMessage, Task: &Spec{ID: m.Task}, Message: &e}
}
