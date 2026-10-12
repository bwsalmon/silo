package fenceapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The agent control surface (agents.md, §8, SV-112): an agent's session
// as events, its messages, and its ask and request_approval requests
// and their answers, under silos/{silo}/agents/{agent}. They are kept in
// the database (agent_events, agent_messages, agent_requests), so any
// control VM serves them, for AuditRetention.
//
// Who may use it follows the fence API's table: the agent's silo, its
// ancestors, and a manage holder with the agents group. In a silo whose
// control policy is none or read, the agent's harness keeps it for its
// own silo's agents (checkControl). An agent's own session may answer
// none of its own requests.

// The audit kinds of the control surface.
const (
	KindAgentMessage = "agent.message"
	KindAgentRequest = "agent.request"
	KindAgentAnswer  = "agent.answer"
)

// The kinds of a session's events.
const (
	EventTurn       = "turn"        // a turn started: {"turn"}
	EventTurnEnd    = "turn_end"    // it ended: {"turn"}
	EventMessage    = "message"     // a message in or out: {"id", "direction", "text"}
	EventToolCall   = "tool_call"   // {"id", "tool", "arguments"}
	EventToolResult = "tool_result" // {"id", "tool", "is_error", "content"}
	EventUsage      = "usage"       // model usage, as the model proxy meters it
	EventError      = "error"       // {"message"}
	EventRequest    = "request"     // an ask or request_approval: {"id", "kind", "text"}
	EventAnswer     = "answer"      // its answer: {"id", "text", "approve"}
)

// The directions of a message.
const (
	MessageIn  = "in"  // to the agent
	MessageOut = "out" // from it
)

// The kinds and states of a request.
const (
	RequestQuestion = "question" // the ask tool
	RequestApproval = "approval" // the request_approval tool
	RequestOpen     = "OPEN"
	RequestAnswered = "ANSWERED"
)

// SessionSchema is the first schema with the control surface's tables.
const SessionSchema = 16

// The Op each control-surface call resolves its silo with, for the
// ManageChecker: the agents group, like the rest of an agent.
const (
	opAgentsSession  Op = "agents.session"
	opAgentsMessages Op = "agents.messages"
	opAgentsMessage  Op = "agents.message"
	opAgentsRequests Op = "agents.requests"
	opAgentsAnswer   Op = "agents.answer"
)

// AgentEvent is one event of an agent's session.
type AgentEvent struct {
	Seq  int64           `json:"seq"`
	Time time.Time       `json:"time"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// AgentMessage is a message to or from an agent.
type AgentMessage struct {
	ID        string      `json:"id"`
	Direction string      `json:"direction"`
	Text      string      `json:"text"`
	Time      time.Time   `json:"time"`
	Sender    *AuditActor `json:"sender,omitempty"`
	// Delivered is set once the agent has taken a message in, at a turn.
	Delivered bool `json:"delivered"`
}

// AgentControlRequest is an agent's ask (a question) or
// request_approval (a proposed action), and its answer once it has one.
type AgentControlRequest struct {
	ID     string       `json:"id"`
	Agent  string       `json:"agent"`
	Kind   string       `json:"kind"`
	Text   string       `json:"text"`
	State  string       `json:"state"`
	Time   time.Time    `json:"time"`
	Answer *AgentAnswer `json:"answer,omitempty"`
}

// AgentAnswer is a request's answer.
type AgentAnswer struct {
	Text string `json:"text,omitempty"`
	// Approve is an approval's answer.
	Approve *bool      `json:"approve,omitempty"`
	Time    time.Time  `json:"time"`
	By      AuditActor `json:"by"`
}

// notifier wakes whoever waits on a session's events, messages or
// answers in this process. Another control VM's change is noticed by
// polling, every pollEvery.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

const pollEvery = time.Second

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

func (n *notifier) wake() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
}

// appendEvent adds an event to agent's session, numbered after its last.
func appendEvent(ctx context.Context, tx store.Tx, agent, kind string, data any) (int64, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	if data == nil {
		b = []byte("{}")
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return 0, err
	}
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_events WHERE agent = ?`, agent).Scan(&seq); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_events (agent, seq, at, kind, data) VALUES (?, ?, ?, ?, ?)`,
		agent, seq, store.Millis(now), kind, string(b))
	return seq, err
}

// nextSeq is the seq for agent's next row in table (agent_messages or
// agent_requests), numbered after its last so that they are listed in
// the order they were made, whatever the clock says.
func nextSeq(ctx context.Context, tx store.Tx, table, agent string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM `+table+` WHERE agent = ?`, agent).Scan(&seq)
	return seq, err
}

func readEvents(ctx context.Context, tx store.Tx, agent string, after int64, limit int) ([]AgentEvent, error) {
	rows, err := tx.Query(ctx, `SELECT seq, at, kind, data FROM agent_events WHERE agent = ? AND seq > ? ORDER BY seq LIMIT ?`, agent, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentEvent{}
	for rows.Next() {
		var e AgentEvent
		var at int64
		var data string
		if err := rows.Scan(&e.Seq, &at, &e.Kind, &data); err != nil {
			return nil, err
		}
		e.Time, e.Data = store.Time(at), json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

const messageColumns = `id, at, direction, text, sender, member, delivered_at`

func scanMessage(row interface{ Scan(...any) error }) (AgentMessage, error) {
	var m AgentMessage
	var at, delivered int64
	var sender, member string
	if err := row.Scan(&m.ID, &at, &m.Direction, &m.Text, &sender, &member, &delivered); err != nil {
		return m, err
	}
	m.Time, m.Delivered = store.Time(at), delivered != 0 || m.Direction == MessageOut
	if sender != "" {
		m.Sender = &AuditActor{Silo: sender, Member: member}
	}
	return m, nil
}

func readMessages(ctx context.Context, tx store.Tx, where string, args ...any) ([]AgentMessage, error) {
	rows, err := tx.Query(ctx, `SELECT `+messageColumns+` FROM agent_messages WHERE `+where+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentMessage{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const requestColumns = `id, agent, at, kind, text, state, answer, approve, answered_at, answered_by`

func scanRequest(row interface{ Scan(...any) error }) (*AgentControlRequest, error) {
	r := &AgentControlRequest{}
	var at, approve, answeredAt int64
	var answer, by string
	if err := row.Scan(&r.ID, &r.Agent, &at, &r.Kind, &r.Text, &r.State, &answer, &approve, &answeredAt, &by); err != nil {
		return nil, err
	}
	r.Time = store.Time(at)
	if r.State == RequestAnswered {
		r.Answer = &AgentAnswer{Text: answer, Time: store.Time(answeredAt)}
		if r.Kind == RequestApproval {
			ok := approve != 0
			r.Answer.Approve = &ok
		}
		if err := json.Unmarshal([]byte(by), &r.Answer.By); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func readRequests(ctx context.Context, tx store.Tx, where string, args ...any) ([]*AgentControlRequest, error) {
	rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM agent_requests WHERE `+where+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AgentControlRequest{}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func readRequest(ctx context.Context, tx store.Tx, agent, id string) (*AgentControlRequest, error) {
	r, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM agent_requests WHERE agent = ? AND id = ?`, agent, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ownSiloCall reports whether a control-surface call names the caller's
// own silo, or an agent in it, which a harness keeps under a sealed or
// read control policy (agents.md, "Agents in a sealed silo", SV-100).
func ownSiloCall(ctx context.Context, tx store.Tx, c Caller, call control.FenceCall) (bool, error) {
	switch {
	case call.Op == control.OpAgentGetByID:
		a, err := readAgent(ctx, tx, `id = ?`, strings.TrimPrefix(call.ID, "-"))
		return a != nil && a.Silo == c.Silo, err
	case call.Ref == "self":
		return true, nil
	case strings.HasPrefix(call.Ref, "-"):
		return call.Ref[1:] == c.Silo, nil
	}
	t, err := tree.ByPath(ctx, tx, "/"+strings.Trim(call.Ref, "/"))
	if errors.Is(err, tree.ErrNotFound) {
		return false, nil
	}
	return err == nil && t.ID == c.Silo, err
}

// getSession is GET .../agents/{agent}/session: the session's events
// after ?after (a seq), paged; with ?follow=true, every event after it
// and then each new one as it comes, as server-sent events, until the
// caller goes or the agent is deleted. A follow resumes after
// Last-Event-ID.
func (s *Server) getSession(q *request, ref, name string) {
	ctx := q.r.Context()
	query := q.r.URL.Query()
	var after int64
	if v := query.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(q.w, errorf(InvalidArgument, "after %q: want an event's seq", v).with("field", "after"))
			return
		}
		after = n
	}
	follow := query.Get("follow") == "true"
	if v := q.r.Header.Get("Last-Event-ID"); follow && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > after {
			after = n
		}
	}
	var agent string
	var out map[string]any
	err := s.tx(ctx, func(tx store.Tx) error {
		_, _, a, err := s.resolveAgent(ctx, tx, q, ref, name, opAgentsSession)
		if err != nil {
			return err
		}
		agent = a.ID
		if follow {
			return nil
		}
		offset, size, err := page(q)
		if err != nil {
			return err
		}
		evs, err := readEvents(ctx, tx, a.ID, after, offset+size+1)
		if err != nil {
			return err
		}
		more := len(evs) > offset+size
		evs = evs[min(offset, len(evs)):min(offset+size, len(evs))]
		out = map[string]any{"agent": a.ID, "session": a.Session, "events": evs}
		if tok := nextToken(offset, size, more); tok != "" {
			out["next_page_token"] = tok
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	if !follow {
		writeJSON(q.w, http.StatusOK, out)
		return
	}
	s.followSession(q, agent, after)
}

func (s *Server) followSession(q *request, agent string, after int64) {
	ctx := q.r.Context()
	w := q.w
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	for {
		woken := s.sessions.wait()
		var evs []AgentEvent
		gone := false
		err := s.st.Tx(ctx, func(tx store.Tx) error {
			a, err := readAgent(ctx, tx, `id = ?`, agent)
			if err != nil {
				return err
			}
			gone = a == nil
			evs, err = readEvents(ctx, tx, agent, after, 500)
			return err
		})
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{"message": err.Error()}))
			return
		}
		for _, e := range evs {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, mustJSON(e))
			after = e.Seq
		}
		if flusher != nil {
			flusher.Flush()
		}
		if len(evs) == 500 {
			continue
		}
		if gone {
			fmt.Fprintf(w, "event: end\ndata: {\"reason\":\"the agent was deleted\"}\n\n")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-woken:
		case <-time.After(pollEvery):
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// listMessages is GET .../agents/{agent}/messages: both ways, oldest
// first.
func (s *Server) listMessages(q *request, ref, name string) {
	ctx := q.r.Context()
	var out map[string]any
	err := s.tx(ctx, func(tx store.Tx) error {
		_, _, a, err := s.resolveAgent(ctx, tx, q, ref, name, opAgentsMessages)
		if err != nil {
			return err
		}
		offset, size, err := page(q)
		if err != nil {
			return err
		}
		all, err := readMessages(ctx, tx, `agent = ?`, a.ID)
		if err != nil {
			return err
		}
		out = map[string]any{"messages": all[min(offset, len(all)):min(offset+size, len(all))]}
		if tok := nextToken(offset, size, offset+size < len(all)); tok != "" {
			out["next_page_token"] = tok
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// postMessage is POST .../agents/{agent}/messages with {"text"}. An
// IDLE agent starts a turn with it, RUNNING; any other gets it at its
// next turn. A deleting or failed agent takes none.
func (s *Server) postMessage(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(q, &body); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body.Text) == "" {
		return nil, errorf(InvalidArgument, "a message needs text").with("field", "text")
	}
	var out *AgentMessage
	var a *agentRow
	turn := false
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, _, row, err := s.resolveAgent(ctx, tx, q, ref, name, opAgentsMessage)
		if err != nil {
			return err
		}
		a, turn = row, false
		t := chain[len(chain)-1]
		q.note(t, KindAgentMessage)
		if a.State == agents.StateDeleting || a.State == agents.StateFailed {
			return errorf(FailedPrecondition, "agent %s is %s and takes no messages", a.Name, a.State)
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		out = &AgentMessage{ID: "m-" + ids.New() + ids.New(), Direction: MessageIn, Text: body.Text, Time: now,
			Sender: &AuditActor{Silo: q.caller.Silo, Member: q.caller.Member}}
		if q.validateOnly {
			return errValidateOnly
		}
		seq, err := nextSeq(ctx, tx, "agent_messages", a.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_messages (id, agent, seq, at, direction, text, sender, member) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			out.ID, a.ID, seq, store.Millis(now), MessageIn, out.Text, q.caller.Silo, q.caller.Member); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, a.ID, EventMessage, map[string]any{"id": out.ID, "direction": MessageIn, "text": out.Text, "sender": out.Sender}); err != nil {
			return err
		}
		before := a.State
		if a.State == agents.StateIdle {
			turn = true
			if err := s.beginTurn(ctx, tx, a, now); err != nil {
				return err
			}
		}
		if err := Audit(ctx, tx, t, q.caller, KindAgentMessage, q.requestID, "OK", agentFields(a, map[string]any{
			"message": out.ID, "state": a.State, "from_state": before,
		})); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	if errors.Is(err, errValidateOnly) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	s.sessions.wake()
	if turn {
		if ts, ok := s.opts.Runner.(agents.TurnStarter); ok {
			if err := ts.StartTurn(ctx, a.ID); err != nil {
				_ = s.RecordEvent(ctx, a.ID, EventError, map[string]any{"message": "starting the turn: " + err.Error()})
			}
		}
	}
	return out, nil
}

// beginTurn makes a RUNNING on its next turn.
func (s *Server) beginTurn(ctx context.Context, tx store.Tx, a *agentRow, now time.Time) error {
	a.State, a.StatusMessage = agents.StateRunning, ""
	if a.Session == nil {
		a.Session = &AgentSession{ID: "s-" + ids.New(), Started: now}
	}
	a.Session.Turns++
	if _, err := appendEvent(ctx, tx, a.ID, EventTurn, map[string]any{"turn": a.Session.Turns}); err != nil {
		return err
	}
	return a.save(ctx, tx)
}

// listRequests is GET .../agents/{agent}/requests, with ?state=open or
// answered for those only.
func (s *Server) listRequests(q *request, ref, name string) {
	ctx := q.r.Context()
	var out map[string]any
	err := s.tx(ctx, func(tx store.Tx) error {
		_, _, a, err := s.resolveAgent(ctx, tx, q, ref, name, opAgentsRequests)
		if err != nil {
			return err
		}
		where, args := `agent = ?`, []any{a.ID}
		switch st := q.r.URL.Query().Get("state"); strings.ToUpper(st) {
		case "":
		case RequestOpen, RequestAnswered:
			where, args = where+` AND state = ?`, append(args, strings.ToUpper(st))
		default:
			return errorf(InvalidArgument, "state %q: want open or answered", st).with("field", "state")
		}
		list, err := readRequests(ctx, tx, where, args...)
		if err != nil {
			return err
		}
		out = map[string]any{"requests": list}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// answerRequest is POST .../agents/{agent}/requests/{id}:answer, with
// {"text"} for a question and {"approve"} (and an optional text) for
// an approval. A WAITING agent with no other open request goes back to
// RUNNING. The agent's own session may not answer.
func (s *Server) answerRequest(q *request, ref, name, id string) (any, error) {
	ctx := q.r.Context()
	var body struct {
		Text    *string `json:"text"`
		Approve *bool   `json:"approve"`
	}
	if err := decode(q, &body); err != nil {
		return nil, err
	}
	var out *AgentControlRequest
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, _, a, err := s.resolveAgent(ctx, tx, q, ref, name, opAgentsAnswer)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindAgentAnswer)
		if ownSession(q.caller, a) {
			return errorf(PermissionDenied, "an agent's own session may not answer its own requests; its silo, an ancestor or a manage holder answers them")
		}
		r, err := readRequest(ctx, tx, a.ID, id)
		if err != nil {
			return err
		}
		if r == nil {
			return errorf(NotFound, "agent %s has no request %s", a.Name, id).with("request", id)
		}
		if r.State != RequestOpen {
			return errorf(FailedPrecondition, "request %s is answered already", id).with("request", id)
		}
		text := ""
		if body.Text != nil {
			text = *body.Text
		}
		approve := int64(0)
		switch r.Kind {
		case RequestQuestion:
			if strings.TrimSpace(text) == "" {
				return errorf(InvalidArgument, "a question's answer is {\"text\": ...}").with("field", "text")
			}
		case RequestApproval:
			if body.Approve == nil {
				return errorf(InvalidArgument, "an approval's answer is {\"approve\": true} or false").with("field", "approve")
			}
			if *body.Approve {
				approve = 1
			}
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		by := AuditActor{Silo: q.caller.Silo, Member: q.caller.Member}
		r.State = RequestAnswered
		r.Answer = &AgentAnswer{Text: text, Time: now, By: by}
		if r.Kind == RequestApproval {
			r.Answer.Approve = body.Approve
		}
		out = r
		if q.validateOnly {
			return errValidateOnly
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_requests SET state = ?, answer = ?, approve = ?, answered_at = ?, answered_by = ? WHERE id = ?`,
			RequestAnswered, text, approve, store.Millis(now), mustJSON(by), r.ID); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, a.ID, EventAnswer, map[string]any{"id": r.ID, "text": text, "approve": r.Answer.Approve, "by": by}); err != nil {
			return err
		}
		before := a.State
		if a.State == agents.StateWaiting {
			var open int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM agent_requests WHERE agent = ? AND state = ?`, a.ID, RequestOpen).Scan(&open); err != nil {
				return err
			}
			if open == 0 {
				a.State = agents.StateRunning
				if err := a.save(ctx, tx); err != nil {
					return err
				}
			}
		}
		fields := map[string]any{"request": r.ID, "request_kind": r.Kind, "state": a.State, "from_state": before}
		if r.Answer.Approve != nil {
			fields["approve"] = *r.Answer.Approve
		}
		if err := Audit(ctx, tx, t, q.caller, KindAgentAnswer, q.requestID, "OK", agentFields(a, fields)); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	if errors.Is(err, errValidateOnly) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	s.sessions.wake()
	return out, nil
}

// RecordEvent adds an event to agent's session: the session socket's
// tool calls and their results, and the runner's and the model proxy's
// turns, usage and errors.
func (s *Server) RecordEvent(ctx context.Context, agent, kind string, data any) error {
	err := s.tx(ctx, func(tx store.Tx) error {
		_, err := appendEvent(ctx, tx, agent, kind, data)
		return err
	})
	if err == nil {
		s.sessions.wake()
	}
	return err
}

// TakeMessages returns the messages to agent it hasn't had yet, oldest
// first, and marks them delivered: the harness's adapter calls it at
// the start of each turn.
func (s *Server) TakeMessages(ctx context.Context, agent string) ([]AgentMessage, error) {
	var out []AgentMessage
	err := s.tx(ctx, func(tx store.Tx) error {
		var err error
		if out, err = readMessages(ctx, tx, `agent = ? AND direction = ? AND delivered_at = 0`, agent, MessageIn); err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		for i := range out {
			out[i].Delivered = true
			if _, err := tx.Exec(ctx, `UPDATE agent_messages SET delivered_at = ? WHERE id = ?`, store.Millis(now), out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// EndTurn records that agent's turn is over. With messages it hasn't
// taken it stays RUNNING, on a new turn, and more is true; otherwise a
// RUNNING agent is IDLE until its next message.
func (s *Server) EndTurn(ctx context.Context, agent string) (more bool, err error) {
	err = s.tx(ctx, func(tx store.Tx) error {
		a, err := readAgent(ctx, tx, `id = ?`, agent)
		if err != nil {
			return err
		}
		if a == nil {
			return errorf(NotFound, "no agent -%s", agent)
		}
		turn := 0
		if a.Session != nil {
			turn = a.Session.Turns
		}
		if _, err := appendEvent(ctx, tx, a.ID, EventTurnEnd, map[string]any{"turn": turn}); err != nil {
			return err
		}
		if a.State != agents.StateRunning {
			more = false
			return nil
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM agent_messages WHERE agent = ? AND direction = ? AND delivered_at = 0`, a.ID, MessageIn).Scan(&pending); err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if more = pending > 0; more {
			return s.beginTurn(ctx, tx, a, now)
		}
		a.State = agents.StateIdle
		return a.save(ctx, tx)
	})
	if err == nil {
		s.sessions.wake()
	}
	return more, err
}

// Reply records a message from agent, as its harness says something to
// whoever is watching.
func (s *Server) Reply(ctx context.Context, agent, text string) (*AgentMessage, error) {
	var out *AgentMessage
	err := s.tx(ctx, func(tx store.Tx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		out = &AgentMessage{ID: "m-" + ids.New() + ids.New(), Direction: MessageOut, Text: text, Time: now, Delivered: true}
		seq, err := nextSeq(ctx, tx, "agent_messages", agent)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_messages (id, agent, seq, at, direction, text) VALUES (?, ?, ?, ?, ?, ?)`,
			out.ID, agent, seq, store.Millis(now), MessageOut, text); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, agent, EventMessage, map[string]any{"id": out.ID, "direction": MessageOut, "text": text})
		return err
	})
	if err == nil {
		s.sessions.wake()
	}
	return out, err
}

// OpenRequest records an agent's ask (kind RequestQuestion) or
// request_approval (RequestApproval), made by its session: c is the
// session's caller, whose Agent it is. The agent is WAITING until it is
// answered; it is audited agent.request.
func (s *Server) OpenRequest(ctx context.Context, c Caller, kind, text string) (*AgentControlRequest, error) {
	if kind != RequestQuestion && kind != RequestApproval {
		return nil, errorf(InvalidArgument, "request kind %q: want question or approval", kind)
	}
	if strings.TrimSpace(text) == "" {
		return nil, errorf(InvalidArgument, "a request needs text").with("field", "text")
	}
	var out *AgentControlRequest
	err := s.tx(ctx, func(tx store.Tx) error {
		a, err := readAgent(ctx, tx, `id = ?`, c.Agent)
		if err != nil {
			return err
		}
		if a == nil || a.Silo != c.Silo {
			return errorf(NotFound, "no agent -%s", c.Agent)
		}
		if a.State == agents.StateDeleting {
			return errorf(FailedPrecondition, "agent %s is being deleted", a.Name)
		}
		t, err := tree.Get(ctx, tx, a.Silo)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		out = &AgentControlRequest{ID: "r-" + ids.New() + ids.New(), Agent: a.ID, Kind: kind, Text: text, State: RequestOpen, Time: now}
		seq, err := nextSeq(ctx, tx, "agent_requests", a.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_requests (id, agent, seq, at, kind, text, state) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			out.ID, a.ID, seq, store.Millis(now), kind, text, RequestOpen); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, a.ID, EventRequest, map[string]any{"id": out.ID, "kind": kind, "text": text}); err != nil {
			return err
		}
		before := a.State
		if agents.HoldsSession(a.State) {
			a.State = agents.StateWaiting
			if err := a.save(ctx, tx); err != nil {
				return err
			}
		}
		return Audit(ctx, tx, t, c, KindAgentRequest, "", "OK", agentFields(a, map[string]any{
			"request": out.ID, "request_kind": kind, "state": a.State, "from_state": before,
		}))
	})
	if err != nil {
		return nil, err
	}
	s.sessions.wake()
	return out, nil
}

// WaitAnswer waits until agent's request id is answered, or ctx ends,
// and returns it.
func (s *Server) WaitAnswer(ctx context.Context, agent, id string) (*AgentControlRequest, error) {
	for {
		woken := s.sessions.wait()
		var r *AgentControlRequest
		if err := s.st.Tx(ctx, func(tx store.Tx) error {
			var err error
			r, err = readRequest(ctx, tx, agent, id)
			return err
		}); err != nil {
			return nil, err
		}
		if r == nil {
			return nil, errorf(NotFound, "agent -%s has no request %s", agent, id)
		}
		if r.State == RequestAnswered {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-woken:
		case <-time.After(pollEvery):
		}
	}
}

// pruneSessions drops the control surface's records older than
// AuditRetention of now, in tx.
func pruneSessions(ctx context.Context, tx store.Tx, now time.Time) error {
	cut := store.Millis(now.Add(-AuditRetention))
	for _, q := range []string{`DELETE FROM agent_events WHERE at < ?`, `DELETE FROM agent_messages WHERE at < ?`, `DELETE FROM agent_requests WHERE at < ?`} {
		if _, err := tx.Exec(ctx, q, cut); err != nil {
			return err
		}
	}
	return nil
}

// ToolVMAgentAt is the agent in silo whose tool VM silod recorded at
// ip, or "": what the gateway's API port notes on a call's audit as
// from_tool_vm (agents.md, §5). It reads only silod's own record; a
// tool channel's attach checks the instance too (ToolChannelAgent).
func (s *Server) ToolVMAgentAt(ctx context.Context, silo string, ip netip.Addr) (string, error) {
	var out string
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		list, err := readAgents(ctx, tx, `silo = ? AND state <> ?`, silo, agents.StateDeleting)
		if err != nil {
			return err
		}
		for _, a := range list {
			if at, err := netip.ParseAddr(a.ToolVM.IP); err == nil && at.Unmap() == ip.Unmap() {
				out = a.ID
			}
		}
		return nil
	})
	return out, err
}
