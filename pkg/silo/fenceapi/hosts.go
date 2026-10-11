package fenceapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// Harness sessions on the control VMs (agents.md, "silod restarts",
// SV-115, and "Placement on the control VMs", SV-116): what package
// harness's Runner reads and writes of an agent besides the control
// surface. An agent's host is the control VM its session runs on; the
// harness's own session state is saved here after every turn, so that
// any control VM can resume it.

// KindAgentSessionRestart is the audit kind of a session resumed after
// silod restarted, or moved off a control VM that is gone.
const KindAgentSessionRestart = "agent.session_restart"

// MaxStartFailures is how many times in a row a harness may fail to
// start before its agent is FAILED.
const MaxStartFailures = 3

// WaitingForSlot is the status_message of an agent whose session waits
// for a free slot on the control VMs.
const WaitingForSlot = "waiting for a session slot: the control VMs can't carry every session, and the oldest agents start first"

// SessionState is a harness's saved session.
type SessionState struct {
	Adapter   string
	SessionID string
	// State is a tar.gz of the CLI's session files.
	State []byte
}

// HostedAgent is an agent whose session a control VM should run.
type HostedAgent struct {
	agents.Session
	State         string
	StatusMessage string
	Created       time.Time
}

// HostLoad is a control VM, by its live replicas' heartbeats: how many
// sessions it can run and how many it runs.
type HostLoad struct {
	Host     string
	Slots    int
	Sessions int
}

// Free is how many more sessions it can run.
func (h HostLoad) Free() int {
	if n := h.Slots - h.Sessions; n > 0 {
		return n
	}
	return 0
}

// ToolCall is a tool call of a session's.
type ToolCall struct {
	ID   string
	Tool string
}

// AgentState is an agent's state, or "" if it is gone.
func (s *Server) AgentState(ctx context.Context, agent string) (string, error) {
	var st string
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		err := tx.QueryRow(ctx, `SELECT state FROM agents WHERE id = ?`, agent).Scan(&st)
		if errors.Is(err, sql.ErrNoRows) {
			st = ""
			return nil
		}
		return err
	})
	return st, err
}

// LoadSessionState is agent's saved session, if it has one.
func (s *Server) LoadSessionState(ctx context.Context, agent string) (SessionState, error) {
	var out SessionState
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var b64 string
		err := tx.QueryRow(ctx, `SELECT adapter, session_id, state FROM agent_session_state WHERE agent = ?`, agent).Scan(&out.Adapter, &out.SessionID, &b64)
		if errors.Is(err, sql.ErrNoRows) {
			out = SessionState{}
			return nil
		}
		if err != nil {
			return err
		}
		out.State, err = base64.StdEncoding.DecodeString(b64)
		return err
	})
	return out, err
}

// SaveSessionState saves agent's session after a turn.
func (s *Server) SaveSessionState(ctx context.Context, agent string, st SessionState) error {
	return s.st.Tx(ctx, func(tx store.Tx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO agent_session_state (agent, adapter, session_id, state, saved_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (agent) DO UPDATE SET adapter = excluded.adapter, session_id = excluded.session_id,
			state = excluded.state, saved_at = excluded.saved_at`,
			agent, st.Adapter, st.SessionID, base64.StdEncoding.EncodeToString(st.State), store.Millis(now))
		return err
	})
}

// PlaceAgent records host as agent's session's host.
func (s *Server) PlaceAgent(ctx context.Context, agent, host string) error {
	return s.st.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agents SET host = ? WHERE id = ?`, host, agent)
		return err
	})
}

// SessionGateway is the gateway address of the control VM that hosts
// agent's session, which a tool channel attach that reached self is
// redirected to (agents.md, "The tool channel", SV-106): "" if the
// session is self's, or unplaced, or its host has no live replica
// (a heartbeat newer than store.ReplicaTimeout by database time) that
// reports one. The leader's MoveSessions re-places a session whose host
// is gone, and the tool server's next attach is redirected to the new
// one.
func (s *Server) SessionGateway(ctx context.Context, agent, self string) (string, error) {
	var addr string
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		addr = ""
		var host string
		err := tx.QueryRow(ctx, `SELECT host FROM agents WHERE id = ?`, agent).Scan(&host)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (host == "" || host == self) {
			return nil
		}
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT gateway_address FROM replicas WHERE host = ? AND gateway_address <> '' AND heartbeat_at > ?
			ORDER BY heartbeat_at DESC, id LIMIT 1`, host, store.Millis(now.Add(-store.ReplicaTimeout))).Scan(&addr)
		if errors.Is(err, sql.ErrNoRows) {
			addr = ""
			return nil
		}
		return err
	})
	return addr, err
}

// sessionStates are the states whose sessions a host runs: the ones
// that hold a session slot.
var sessionStates = []any{agents.StateProvisioning, agents.StateRunning, agents.StateIdle, agents.StateWaiting}

// HostedAgents are the agents whose sessions host should run, oldest
// first: those placed on it in a state that holds a session.
func (s *Server) HostedAgents(ctx context.Context, host string) ([]HostedAgent, error) {
	var out []HostedAgent
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		out = out[:0]
		rows, err := tx.Query(ctx, `SELECT id, silo, adapter, credential, model, prompt, state, status_message, created_at FROM agents
			WHERE host = ? AND state IN (?, ?, ?, ?) ORDER BY created_at, id`, append([]any{host}, sessionStates...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h HostedAgent
			var created int64
			if err := rows.Scan(&h.Agent, &h.Silo, &h.Adapter, &h.Credential, &h.Model, &h.Prompt, &h.State, &h.StatusMessage, &created); err != nil {
				return err
			}
			h.Created = store.Time(created)
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// HostLoads are the control VMs with a live replica (a heartbeat newer
// than store.ReplicaTimeout by database time), by host: the slots their
// heartbeats report, and the sessions they run, which is the larger of
// what they report and the agents placed on them.
func (s *Server) HostLoads(ctx context.Context) ([]HostLoad, error) {
	var out []HostLoad
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		out, err = hostLoads(ctx, tx)
		return err
	})
	return out, err
}

func hostLoads(ctx context.Context, tx store.Tx) ([]HostLoad, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT host, agent_slots, agent_sessions FROM replicas WHERE host <> '' AND heartbeat_at > ?`,
		store.Millis(now.Add(-store.ReplicaTimeout)))
	if err != nil {
		return nil, err
	}
	by := map[string]*HostLoad{}
	for rows.Next() {
		var h HostLoad
		if err := rows.Scan(&h.Host, &h.Slots, &h.Sessions); err != nil {
			rows.Close()
			return nil, err
		}
		cur := by[h.Host]
		if cur == nil {
			cur = &HostLoad{Host: h.Host}
			by[h.Host] = cur
		}
		cur.Slots, cur.Sessions = max(cur.Slots, h.Slots), max(cur.Sessions, h.Sessions)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT host, COUNT(*) FROM agents WHERE host <> '' AND state IN (?, ?, ?, ?) GROUP BY host`, sessionStates...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var host string
		var n int
		if err := rows.Scan(&host, &n); err != nil {
			return nil, err
		}
		if h := by[host]; h != nil {
			h.Sessions = max(h.Sessions, n)
		}
	}
	var list []HostLoad
	for _, h := range by {
		list = append(list, *h)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Host < list[j].Host })
	return list, rows.Err()
}

// PickHost is the control VM with the most free session slots, self on
// a tie; "" if none has one.
func PickHost(loads []HostLoad, self string) string {
	best, free := "", 0
	for _, h := range loads {
		if f := h.Free(); f > free || f == free && f > 0 && h.Host == self {
			best, free = h.Host, f
		}
	}
	return best
}

// SessionWaiting records why agent's session hasn't started.
func (s *Server) SessionWaiting(ctx context.Context, agent, why string) error {
	return s.st.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agents SET status_message = ? WHERE id = ? AND status_message <> ?`, why, agent, why)
		return err
	})
}

// SessionStarted records that agent's harness started a turn: its
// failures in a row start again from none, and it no longer waits.
func (s *Server) SessionStarted(ctx context.Context, agent string) error {
	return s.st.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agents SET start_failures = 0,
			status_message = CASE WHEN status_message = ? THEN '' ELSE status_message END WHERE id = ?`, WaitingForSlot, agent)
		return err
	})
}

// SessionStartFailed records that agent's harness failed to start; the
// MaxStartFailures-th time in a row the agent is FAILED with why, and
// failed is true.
func (s *Server) SessionStartFailed(ctx context.Context, agent, why string) (failed bool, err error) {
	err = s.tx(ctx, func(tx store.Tx) error {
		failed = false
		a, err := readAgent(ctx, tx, `id = ?`, agent)
		if err != nil || a == nil || a.State == agents.StateDeleting || a.State == agents.StateFailed {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT start_failures FROM agents WHERE id = ?`, agent).Scan(&n); err != nil {
			return err
		}
		n++
		if _, err := tx.Exec(ctx, `UPDATE agents SET start_failures = ? WHERE id = ?`, n, agent); err != nil {
			return err
		}
		if n < MaxStartFailures {
			return nil
		}
		failed = true
		a.State = agents.StateFailed
		a.StatusMessage = "the harness failed to start " + strconv.Itoa(n) + " times in a row: " + why
		if err := releaseSession(ctx, tx, a.ID); err != nil {
			return err
		}
		if a.op != "" {
			if err := setOp(ctx, tx, a.op, agents.StateFailed, true, errorf(FailedPrecondition, "%s", a.StatusMessage)); err != nil {
				return err
			}
			a.op = ""
		}
		return a.save(ctx, tx)
	})
	if err == nil {
		s.sessions.wake()
	}
	return failed, err
}

// SessionRestarted records that agent's session is resumed after silod
// restarted, or on another control VM: its restarts count goes up, a
// tool call that was in flight gets a result saying it was
// interrupted, and the resume is audited agent.session_restart. It
// returns the interrupted calls.
func (s *Server) SessionRestarted(ctx context.Context, agent, host string) ([]ToolCall, error) {
	c, err := s.internalCaller(ctx)
	if err != nil {
		return nil, err
	}
	var open []ToolCall
	err = s.tx(ctx, func(tx store.Tx) error {
		open = nil
		a, err := readAgent(ctx, tx, `id = ?`, agent)
		if err != nil || a == nil {
			return err
		}
		if open, err = openToolCalls(ctx, tx, agent); err != nil {
			return err
		}
		for _, tc := range open {
			if _, err := appendEvent(ctx, tx, agent, EventToolResult, map[string]any{"id": tc.ID, "tool": tc.Tool, "is_error": true,
				"content": "interrupted: silod restarted while this call was in flight. The command it started may still be running on the tool VM, which keeps its result for ten minutes."}); err != nil {
				return err
			}
		}
		if a.Session != nil {
			a.Session.Restarts++
			if err := a.save(ctx, tx); err != nil {
				return err
			}
		}
		t, err := tree.Get(ctx, tx, a.Silo)
		if err != nil {
			return err
		}
		var ids []string
		for _, tc := range open {
			ids = append(ids, tc.ID)
		}
		return Audit(ctx, tx, t, c, KindAgentSessionRestart, "", "OK", agentFields(a, map[string]any{
			"host": host, "state": a.State, "interrupted_tool_calls": ids}))
	})
	if err == nil {
		s.sessions.wake()
	}
	return open, err
}

// openToolCalls are agent's tool calls with no result, since its last
// turn started.
func openToolCalls(ctx context.Context, tx store.Tx, agent string) ([]ToolCall, error) {
	var from int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM agent_events WHERE agent = ? AND kind = ?`, agent, EventTurn).Scan(&from); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT kind, data FROM agent_events WHERE agent = ? AND seq >= ? AND kind IN (?, ?) ORDER BY seq`,
		agent, from, EventToolCall, EventToolResult)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var order []string
	calls := map[string]ToolCall{}
	for rows.Next() {
		var kind, data string
		if err := rows.Scan(&kind, &data); err != nil {
			return nil, err
		}
		var d struct {
			ID   any    `json:"id"`
			Tool string `json:"tool"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			continue
		}
		id := strings.Trim(string(mustJSON(d.ID)), `"`)
		if kind == EventToolCall {
			if _, ok := calls[id]; !ok {
				order = append(order, id)
			}
			calls[id] = ToolCall{ID: id, Tool: d.Tool}
		} else {
			delete(calls, id)
		}
	}
	var out []ToolCall
	for _, id := range order {
		if tc, ok := calls[id]; ok {
			out = append(out, tc)
		}
	}
	return out, rows.Err()
}

// MoveSessions is the leader's duty for control VMs that are gone
// (agents.md, "silod restarts"): every agent whose session holds a slot
// on a host with no live replica, or on none, is placed, oldest first,
// on the live control VM with the most free slots, whose silod resumes
// it. One that fits nowhere waits with WaitingForSlot. guard is the
// leader's (leader.Guard). It returns how many it moved.
func (s *Server) MoveSessions(ctx context.Context, guard func(context.Context, store.Tx) error) (int, error) {
	moved := 0
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		moved = 0
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		loads, err := hostLoads(ctx, tx)
		if err != nil || len(loads) == 0 {
			return err
		}
		live := map[string]bool{}
		for _, h := range loads {
			live[h.Host] = true
		}
		// An unplaced PROVISIONING agent is being started by the silod
		// that created it, which places it.
		rows, err := tx.Query(ctx, `SELECT id, host, status_message FROM agents WHERE (host <> '' OR state <> 'PROVISIONING') AND state IN (?, ?, ?, ?) ORDER BY created_at, id`, sessionStates...)
		if err != nil {
			return err
		}
		type stray struct{ id, host, msg string }
		var strays []stray
		for rows.Next() {
			var st stray
			if err := rows.Scan(&st.id, &st.host, &st.msg); err != nil {
				rows.Close()
				return err
			}
			if !live[st.host] {
				strays = append(strays, st)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, st := range strays {
			to := PickHost(loads, "")
			if to == "" {
				if st.msg != WaitingForSlot {
					if _, err := tx.Exec(ctx, `UPDATE agents SET status_message = ? WHERE id = ?`, WaitingForSlot, st.id); err != nil {
						return err
					}
				}
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE agents SET host = ? WHERE id = ?`, to, st.id); err != nil {
				return err
			}
			for i := range loads {
				if loads[i].Host == to {
					loads[i].Sessions++
				}
			}
			moved++
		}
		return nil
	})
	return moved, err
}
