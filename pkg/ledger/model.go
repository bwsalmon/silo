package ledger

import (
	"context"
	"time"

	"github.com/bwsalmon/silo/pkg/store"
)

// The model budget is kind model, class usd: a metered, global class
// in this same ledger, kept in micro-dollars (allocators.md, "Model
// budget"; agents.md, "The allocator"). Its granted and delegated move
// with Grant and SetCapacity like any class's. Its used is the sum of
// the silo's agents' budgets, reserved with ReserveModelBudget and
// given back with ReturnModelBudget.
var ModelUSD = Key{Kind: "model", Class: "usd", Region: Global}

// The budget periods, all in UTC (silo install --budget-period).
const (
	PeriodMonth = "month"
	PeriodWeek  = "week"
	PeriodDay   = "day"
)

func periodOf(p string) (string, error) {
	for _, ok := range []string{PeriodMonth, PeriodWeek, PeriodDay} {
		if p == ok {
			return p, nil
		}
	}
	return "", invalid("budget period %q: want %s, %s or %s", p, PeriodMonth, PeriodWeek, PeriodDay)
}

// BudgetPeriod is the deployment's budget period, which is the period
// of the model/usd class.
func BudgetPeriod(ctx context.Context, tx store.Tx) (string, error) {
	c, err := LookupClass(ctx, tx, ModelUSD.Kind, ModelUSD.Class)
	if err != nil {
		return "", err
	}
	return c.Period, nil
}

// SetBudgetPeriod sets the deployment's budget period.
func SetBudgetPeriod(ctx context.Context, tx store.Tx, period string) error {
	if _, err := periodOf(period); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE quota_classes SET period = ? WHERE kind = ? AND name = ?`,
		period, ModelUSD.Kind, ModelUSD.Class)
	return err
}

// PeriodStart is the start of the period t falls in: the first of the
// month, the Monday of the ISO week, or midnight, in UTC. Agents'
// spent_usd goes back to 0 there.
func PeriodStart(period string, t time.Time) (time.Time, error) {
	if _, err := periodOf(period); err != nil {
		return time.Time{}, err
	}
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case PeriodMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	case PeriodWeek:
		back := (int(t.Weekday()) + 6) % 7 // days since Monday
		return day.AddDate(0, 0, -back), nil
	}
	return day, nil
}

func agentObject(agent string) string { return "agent:" + agent }

// ReserveModelBudget sets the budget an agent of silo holds to micros,
// reserving it out of silo's free model budget: creating an agent
// reserves its budget.usd, and raising it reserves the difference.
// Raising past the free budget is QUOTA_EXCEEDED; lowering always
// succeeds and gives the difference back. via is how the call reached
// the silo, as for Charge.
func ReserveModelBudget(ctx context.Context, tx store.Tx, silo, agent, via string, micros int64) error {
	if agent == "" {
		return invalid("an agent is needed to reserve model budget")
	}
	if micros < 0 {
		return &Error{Code: InvalidArgument, Key: ModelUSD, Message: "a negative model budget"}
	}
	held, err := Charges(ctx, tx, agentObject(agent))
	if err != nil {
		return err
	}
	var cur int64
	for _, c := range held {
		if c.Key == ModelUSD {
			if c.Home != silo {
				return precondition("agent %s's budget is held by silo %s, not %s", agent, c.Home, silo)
			}
			cur += c.Amount
		}
	}
	switch {
	case micros > cur:
		return Charge(ctx, tx, ChargeRequest{Object: agentObject(agent), Silo: silo, Via: via,
			Amounts: []Amount{{Key: ModelUSD, Amount: micros - cur}}})
	case micros < cur:
		_, err := Refund(ctx, tx, agentObject(agent), []Amount{{Key: ModelUSD, Amount: cur - micros}})
		return err
	}
	return nil
}

// ModelBudget returns what an agent holds of its silo's model budget.
func ModelBudget(ctx context.Context, tx store.Tx, agent string) (int64, error) {
	held, err := Charges(ctx, tx, agentObject(agent))
	if err != nil {
		return 0, err
	}
	var n int64
	for _, c := range held {
		if c.Key == ModelUSD {
			n += c.Amount
		}
	}
	return n, nil
}

// ReturnModelBudget gives an agent's whole budget back to its silo,
// when the agent is deleted.
func ReturnModelBudget(ctx context.Context, tx store.Tx, agent string) error {
	_, err := Refund(ctx, tx, agentObject(agent), nil)
	return err
}
