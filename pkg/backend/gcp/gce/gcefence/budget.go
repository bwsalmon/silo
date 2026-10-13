package gcefence

import (
	"context"
	"fmt"

	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/store"
	compute "google.golang.org/api/compute/v1"
)

// Quota is the policy quota silo install recorded. A zero field is not
// recorded, and not enforced.
type Quota struct {
	Rules      int // rules in the policy
	Attributes int // rule attributes: ranges, protocols and ports
}

// The quota's names, as a refusal names them.
const (
	QuotaRules      = "firewall_policy_rules"
	QuotaAttributes = "firewall_policy_rule_attributes"
)

// BudgetPercent is how much of the quota the fence may use.
const BudgetPercent = 90

// SetQuota records the policy quota. silo install calls it.
func SetQuota(ctx context.Context, tx store.Tx, q Quota) error {
	if q.Rules < 0 || q.Attributes < 0 {
		return fmt.Errorf("fence: negative quota %+v", q)
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE fence_policy SET quota_rules = ?, quota_attributes = ?, updated_at = ? WHERE id = 1`,
		q.Rules, q.Attributes, store.Millis(now))
	return err
}

// GetQuota reads the recorded policy quota.
func GetQuota(ctx context.Context, tx store.Tx) (Quota, error) {
	var q Quota
	err := tx.QueryRow(ctx, `SELECT quota_rules, quota_attributes FROM fence_policy WHERE id = 1`).Scan(&q.Rules, &q.Attributes)
	return q, err
}

// Usage is how much of the quota a compiled policy uses.
type Usage struct {
	Rules, Attributes int
}

// Measure counts rules' use of the quota. A rule's attributes are its
// source and destination ranges, plus each layer4Config's protocol and
// each of its ports or port ranges; the catch-all rules count like any
// other, and GCE's default rules are not counted.
func Measure(rules []*compute.FirewallPolicyRule) Usage {
	var u Usage
	for _, r := range rules {
		if r.Priority >= gceDefaults {
			continue
		}
		u.Rules++
		if m := r.Match; m != nil {
			u.Attributes += len(m.SrcIpRanges) + len(m.DestIpRanges)
			for _, c := range m.Layer4Configs {
				u.Attributes += 1 + len(c.Ports)
			}
		}
	}
	return u
}

// BudgetError is a change refused because the compiled policy would
// pass BudgetPercent of a quota.
type BudgetError struct {
	Quota       string // QuotaRules or QuotaAttributes
	Limit, Want int    // the quota, and what the policy would use
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("the fence policy %s would hold %d %s, past %d%% of its quota of %d",
		PolicyName, e.Want, quotaUnit[e.Quota], BudgetPercent, e.Limit)
}

var quotaUnit = map[string]string{QuotaRules: "rules", QuotaAttributes: "rule attributes"}

// Budget is the fence API's FenceBudget: it compiles the policy a
// change would leave, in the change's own transaction, and refuses one
// past BudgetPercent of the recorded quota.
type Budget struct {
	Config Config
	Grants fence.GrantSource
}

// CheckBudget implements fenceapi.FenceBudget. It returns a
// *BudgetError for a policy over budget.
func (b Budget) CheckBudget(ctx context.Context, tx store.Tx) error {
	q, err := GetQuota(ctx, tx)
	if err != nil || (q.Rules == 0 && q.Attributes == 0) {
		return err
	}
	in, err := fence.Load(ctx, tx, b.Grants)
	if err != nil {
		return err
	}
	rules, err := Compile(in, b.Config, nil)
	if err != nil {
		return err
	}
	return checkUsage(Measure(rules), q)
}

func checkUsage(u Usage, q Quota) error {
	over := func(want, limit int) bool { return limit > 0 && want*100 > limit*BudgetPercent }
	if over(u.Rules, q.Rules) {
		return &BudgetError{Quota: QuotaRules, Limit: q.Rules, Want: u.Rules}
	}
	if over(u.Attributes, q.Attributes) {
		return &BudgetError{Quota: QuotaAttributes, Limit: q.Attributes, Want: u.Attributes}
	}
	return nil
}

// Budget reports the refusal for the fence API: which quota, its
// limit, and what the policy would use.
func (e *BudgetError) Budget() (quota string, limit, want int) { return e.Quota, e.Limit, e.Want }
