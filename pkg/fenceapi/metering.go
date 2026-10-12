package fenceapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Metering (agents.md, "Metering", SV-110; call 47), the budget running
// out (SV-114) and the lease's warning (SV-113). Every model call is
// priced at the model's price and charged to the agent's spent_usd: the
// model proxy's calls on the actual usage the provider reports, and a
// subscription's turns on the usage its harness reports, at the API list
// price. At spent_usd >= budget the agent is BUDGET_EXHAUSTED: its
// harness stops, its session saved and its tool VM kept, until its
// budget is raised or a new period starts.

// The audit kinds of metering.
const (
	KindModelCall            = "model.call"
	KindAgentBudgetExhausted = "agent.budget_exhausted"
	KindAgentBudgetRestored  = "agent.budget_restored"
	exhaustedMessage         = "the budget for this period is spent"
	LeaseWarning             = 10 * time.Minute
)

// ModelUsage is one model call's tokens, by kind, as the provider or the
// harness reports them. Input excludes the cached tokens, which are
// CacheWrite and CacheRead.
type ModelUsage struct {
	Model      string
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
	// Proxied is set for a call the model proxy carried, and clear for
	// one a subscription's harness reported.
	Proxied bool
}

// tokenCost is n tokens at perMillion micro-dollars a million, rounded
// up to the micro-dollar.
func tokenCost(n, perMillion int64) int64 {
	if n <= 0 || perMillion <= 0 {
		return 0
	}
	return (n*perMillion + 999_999) / 1_000_000
}

// Cost is u's price in micro-dollars at p, cache reads and writes at
// their own prices.
func Cost(p agents.Price, u ModelUsage) int64 {
	return tokenCost(u.Input, p.Input) + tokenCost(u.Output, p.Output) +
		tokenCost(u.CacheWrite, p.CacheWrite) + tokenCost(u.CacheRead, p.CacheRead)
}

// WorstCase is the most a call can cost before it is made: its prompt's
// tokens at the input price, plus max_tokens at the output price.
func WorstCase(p agents.Price, promptTokens, maxTokens int64) int64 {
	return tokenCost(promptTokens, p.Input) + tokenCost(maxTokens, p.Output)
}

// ModelAllowance is what the model proxy needs to carry a call for an
// agent: its credential, and what its budget has left.
type ModelAllowance struct {
	Agent      string
	Credential agents.Credential
	// Model is the agent's own model.
	Model string
	// Left is budget.usd less spent_usd, in micro-dollars (0 or less:
	// none).
	Left int64
	// State is the agent's state.
	State string
}

// ModelAllowanceOf is an agent's ModelAllowance.
func (s *Server) ModelAllowanceOf(ctx context.Context, agent string) (ModelAllowance, error) {
	var out ModelAllowance
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		a, err := readAgent(ctx, tx, `id = ?`, agent)
		if err != nil {
			return err
		}
		if a == nil {
			return errorf(NotFound, "no agent -%s", agent)
		}
		cred, ok, err := s.credential(ctx, tx, a.Harness.Credential)
		if err != nil {
			return err
		}
		if !ok {
			return errorf(FailedPrecondition, "agent %s's credential %s is gone", a.Name, a.Harness.Credential)
		}
		out = ModelAllowance{Agent: a.ID, Credential: cred, Model: a.Model, Left: a.budget - a.spent, State: a.State}
		return nil
	})
	return out, err
}

// ModelPriceFor is the price of model under the agent's credential's
// provider; false if it has none, and so can't be used.
func (s *Server) ModelPriceFor(ctx context.Context, provider, model string) (agents.Price, bool, error) {
	var p agents.Price
	var ok bool
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		p, ok, err = modelPrice(ctx, tx, provider, model)
		return err
	})
	return p, ok, err
}

// ChargeModel charges an agent for one model call (or, for a
// subscription, one turn), audited model.call, and records it as a usage
// event. If that brings spent_usd to its budget, the agent is
// BUDGET_EXHAUSTED and its harness is stopped; what it spent past the
// budget is recorded as the overshoot. It returns what the call cost.
func (s *Server) ChargeModel(ctx context.Context, agent string, u ModelUsage) (int64, error) {
	var cost int64
	var a *agentRow
	stop := false
	err := s.tx(ctx, func(tx store.Tx) error {
		var err error
		stop = false
		if a, err = readAgent(ctx, tx, `id = ?`, agent); err != nil {
			return err
		}
		if a == nil {
			return errorf(NotFound, "no agent -%s", agent)
		}
		t, err := tree.Get(ctx, tx, a.Silo)
		if err != nil {
			return err
		}
		cred, _, err := s.credential(ctx, tx, a.Harness.Credential)
		if err != nil {
			return err
		}
		model := u.Model
		if model == "" {
			model = a.Model
		}
		p, ok, err := modelPrice(ctx, tx, cred.Provider, model)
		if err != nil {
			return err
		}
		if !ok {
			// A model the proxy let through (a price removed since) is
			// charged at the agent's own model's price.
			if p, _, err = modelPrice(ctx, tx, cred.Provider, a.Model); err != nil {
				return err
			}
		}
		cost = Cost(p, u)
		a.spent += cost
		via := "reported"
		if u.Proxied {
			via = "proxied"
		}
		tokens := map[string]any{"input": u.Input, "output": u.Output, "cache_write": u.CacheWrite, "cache_read": u.CacheRead}
		if _, err := appendEvent(ctx, tx, a.ID, EventUsage, map[string]any{
			"model": model, "tokens": tokens, "usd": usd(cost), "spent_usd": usd(a.spent), "source": via,
		}); err != nil {
			return err
		}
		c := Caller{Silo: a.Silo, Via: ViaHarness, Agent: a.ID}
		if err := Audit(ctx, tx, t, c, KindModelCall, "", "OK", agentFields(a, map[string]any{
			"model": model, "credential": a.Harness.Credential, "tokens": tokens, "usd": usd(cost),
			"spent_usd": usd(a.spent), "budget_usd": usd(a.budget), "metered": via,
		})); err != nil {
			return err
		}
		if a.spent >= a.budget {
			if stop, err = s.exhaust(ctx, tx, t, a); err != nil {
				return err
			}
		}
		return a.save(ctx, tx)
	})
	if err != nil {
		return 0, err
	}
	s.sessions.wake()
	if stop && s.opts.Runner != nil {
		_ = s.opts.Runner.Stop(ctx, a.ID)
	}
	return cost, nil
}

// exhaust makes a, whose spent_usd has reached its budget, BUDGET_EXHAUSTED,
// in tx, audited with the overshoot; it reports whether its session is to
// stop. One not holding a session (stopped, failed, deleting, exhausted
// already) is left as it is.
func (s *Server) exhaust(ctx context.Context, tx store.Tx, t *tree.Silo, a *agentRow) (bool, error) {
	if !agents.HoldsSession(a.State) {
		return false, nil
	}
	if err := releaseSession(ctx, tx, a.ID); err != nil {
		return false, err
	}
	before := a.State
	a.State, a.StatusMessage = agents.StateBudgetExhausted, exhaustedMessage
	c, err := internalCallerTx(ctx, tx)
	if err != nil {
		return false, err
	}
	if _, err := appendEvent(ctx, tx, a.ID, EventError, map[string]any{"message": exhaustedMessage}); err != nil {
		return false, err
	}
	return true, Audit(ctx, tx, t, c, KindAgentBudgetExhausted, "", "OK", agentFields(a, map[string]any{
		"state": a.State, "from_state": before, "budget_usd": usd(a.budget), "spent_usd": usd(a.spent),
		"overshoot_usd": usd(a.spent - a.budget),
	}))
}

func internalCallerTx(ctx context.Context, tx store.Tx) (Caller, error) {
	root, err := tree.Root(ctx, tx)
	if err != nil {
		return Caller{}, err
	}
	return Caller{Silo: root.ID, Via: ViaInternal}, nil
}

// ResetBudgetPeriods starts a new budget period for every agent whose
// period_start is before the current period's, by database time: its
// spent_usd goes back to 0, and a BUDGET_EXHAUSTED agent is IDLE again,
// its session resumed, if a session slot is free (otherwise it waits,
// with status_message saying so, for the next pass). It returns how
// many agents it reset. silod runs it with the expiry.
func (s *Server) ResetBudgetPeriods(ctx context.Context) (int, error) {
	var resumed []*agentRow
	n := 0
	err := s.tx(ctx, func(tx store.Tx) error {
		resumed, n = nil, 0
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		period, err := ledger.BudgetPeriod(ctx, tx)
		if err != nil {
			return err
		}
		start, err := ledger.PeriodStart(period, now)
		if err != nil {
			return err
		}
		list, err := readAgents(ctx, tx, `period_start < ? OR (state = ? AND spent_micros < budget_micros)`,
			store.Millis(start), agents.StateBudgetExhausted)
		if err != nil {
			return err
		}
		for _, a := range list {
			if a.Budget.PeriodStart.Before(start) {
				a.spent, a.Budget.PeriodStart = 0, start
				n++
			}
			if a.State == agents.StateBudgetExhausted && a.spent < a.budget {
				ok, err := s.restore(ctx, tx, a)
				if err != nil {
					return err
				}
				if ok {
					resumed = append(resumed, a)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE agents SET spent_micros = ?, period_start = ? WHERE id = ?`,
				a.spent, store.Millis(a.Budget.PeriodStart), a.ID); err != nil {
				return err
			}
			if err := a.save(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, tree.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	for _, a := range resumed {
		s.runnerAfter(ctx, a, true, false)
	}
	return n, nil
}

// restore puts an exhausted agent back to IDLE, holding a session slot
// again, in tx; false, with status_message saying why, if there is none.
func (s *Server) restore(ctx context.Context, tx store.Tx, a *agentRow) (bool, error) {
	chain, err := tree.Ancestry(ctx, tx, a.Silo)
	if err != nil {
		return false, err
	}
	c, err := internalCallerTx(ctx, tx)
	if err != nil {
		return false, err
	}
	if err := s.holdSession(ctx, tx, chain, c, a.ID, a.Harness.Credential); err != nil {
		var e *Error
		var le *ledger.Error
		if errors.As(err, &e) || errors.As(err, &le) {
			a.StatusMessage = "a new budget period started; waiting for a session slot: " + err.Error()
			return false, nil
		}
		return false, err
	}
	a.State, a.StatusMessage = agents.StateIdle, ""
	return true, Audit(ctx, tx, chain[len(chain)-1], c, KindAgentBudgetRestored, "", "OK", agentFields(a, map[string]any{
		"state": a.State, "from_state": agents.StateBudgetExhausted, "budget_usd": usd(a.budget), "spent_usd": usd(a.spent),
	}))
}

// WarnLeases adds a message to the session of every agent whose lease
// ends within LeaseWarning, once for each expire_time, saying when, so
// that it can wrap up and a person can renew it (agents.md, "The lease
// runs out", SV-113). An IDLE agent starts a turn with it. It returns
// how many it warned.
func (s *Server) WarnLeases(ctx context.Context) (int, error) {
	var turns []string
	n := 0
	err := s.tx(ctx, func(tx store.Tx) error {
		turns, n = nil, 0
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		list, err := readAgents(ctx, tx, `expire_at <= ? AND expire_at > ? AND lease_warned <> expire_at AND state NOT IN (?, ?)`,
			store.Millis(now.Add(LeaseWarning)), store.Millis(now), agents.StateDeleting, agents.StateFailed)
		if err != nil {
			return err
		}
		for _, a := range list {
			left := a.expire.Sub(now).Round(time.Minute)
			text := fmt.Sprintf("silo: this agent's lease ends at %s, in about %s. It is then deleted, with its tool VM. Wrap up now; a person may renew it.",
				a.expire.UTC().Format(time.RFC3339), formatDuration(left))
			id := "m-lease-" + a.ID + "-" + fmt.Sprint(store.Millis(a.expire))
			seq, err := nextSeq(ctx, tx, "agent_messages", a.ID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO agent_messages (id, agent, seq, at, direction, text) VALUES (?, ?, ?, ?, ?, ?)`,
				id, a.ID, seq, store.Millis(now), MessageIn, text); err != nil {
				return err
			}
			if _, err := appendEvent(ctx, tx, a.ID, EventMessage, map[string]any{"id": id, "direction": MessageIn, "text": text}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE agents SET lease_warned = ? WHERE id = ?`, store.Millis(a.expire), a.ID); err != nil {
				return err
			}
			if a.State == agents.StateIdle {
				if err := s.beginTurn(ctx, tx, a, now); err != nil {
					return err
				}
				turns = append(turns, a.ID)
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.sessions.wake()
	}
	if ts, ok := s.opts.Runner.(agents.TurnStarter); ok {
		for _, id := range turns {
			if err := ts.StartTurn(ctx, id); err != nil {
				_ = s.RecordEvent(ctx, id, EventError, map[string]any{"message": "starting the turn: " + err.Error()})
			}
		}
	}
	return n, nil
}

// ReportUsage is a harness's own report of a turn's model usage (the
// adapter reads it from the harness's event stream). For a subscription
// session, which the model proxy can't see, it is what the agent is
// charged, at the API list price of its model (call 47); usage is known
// only after the turn, so the agent can overshoot by up to one turn,
// which exhaust records. An API-key session's turns were charged by the
// proxy, call by call, so its report is only recorded.
func (s *Server) ReportUsage(ctx context.Context, agent string, report map[string]any) error {
	var kind string
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		a, err := readAgent(ctx, tx, `id = ?`, agent)
		if err != nil || a == nil {
			return err
		}
		cred, _, err := s.credential(ctx, tx, a.Harness.Credential)
		kind = cred.Kind
		return err
	})
	if err != nil {
		return err
	}
	if kind != agents.CredentialSubscription {
		return s.RecordEvent(ctx, agent, EventUsage, report)
	}
	_, err = s.ChargeModel(ctx, agent, HarnessUsage(report))
	return err
}

// HarnessUsage reads the tokens of an adapter's usage report
// ({"tokens": {...}}): Claude Code's input_tokens,
// cache_creation_input_tokens, cache_read_input_tokens and
// output_tokens, or Codex's input_tokens (cached_input_tokens among
// them) and output_tokens.
func HarnessUsage(report map[string]any) ModelUsage {
	t, _ := report["tokens"].(map[string]any)
	n := func(k string) int64 {
		switch v := t[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		}
		return 0
	}
	u := ModelUsage{Input: n("input_tokens"), Output: n("output_tokens"),
		CacheWrite: n("cache_creation_input_tokens"), CacheRead: n("cache_read_input_tokens")}
	if cached := n("cached_input_tokens"); cached > 0 {
		u.Input, u.CacheRead = max(u.Input-cached, 0), u.CacheRead+cached
	}
	return u
}
