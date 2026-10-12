package fence

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/exports"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger/address"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
	compute "google.golang.org/api/compute/v1"
)

// GrantSource lists the grant rules the fence carries: AllGrants, the
// exports' and the links'.
type GrantSource func(ctx context.Context, tx store.Tx) ([]Grant, error)

// ExportGrants is the exports' grant rules (gateway.md, "Proxied
// exports", SV-83): for each export with an ACTIVE import,
// export-<id>-in, which lets the gateway range, and nothing else, reach
// the export's ip:port over TCP. An importer never gets a rule of its
// own: its traffic reaches the exporter only through the gateway. An
// export no import has accepted, or whose every import is suspended,
// or whose instance has no address, has no rule.
func ExportGrants(ctx context.Context, tx store.Tx) ([]Grant, error) {
	blocks, err := address.AllBlocks(ctx, tx)
	if err != nil {
		return nil, err
	}
	gw := blocks[address.SystemGateway]
	if len(gw) != 1 {
		return nil, nil
	}
	ims, err := exports.AllImports(ctx, tx)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, im := range ims {
		if im.State == exports.StateActive {
			live[im.Export] = true
		}
	}
	es, err := exports.All(ctx, tx)
	if err != nil {
		return nil, err
	}
	var out []Grant
	for _, e := range es {
		t := e.Target()
		if !live[e.ID] || !t.IsValid() {
			continue
		}
		out = append(out, ExportGrant(exports.RuleName(e), gw[0], t))
	}
	return out, nil
}

// ExportGrant is an export's rule: the gateway range, and nothing else,
// may reach target, on its TCP port alone.
func ExportGrant(name string, gateway netip.Prefix, target netip.AddrPort) Grant {
	return Grant{Name: name, Direction: "INGRESS",
		Src: []netip.Prefix{gateway}, Dst: []netip.Prefix{netip.PrefixFrom(target.Addr(), 32)},
		L4: []L4{{Protocol: "tcp", Ports: []string{strconv.Itoa(int(target.Port()))}}}}
}

// Load reads the fence's Input from the database, in tx: the gateway
// and control ranges, the root's block less both, every silo with its
// blocks and whether its effective control policy seals it, and the
// grants. grants nil means AllGrants.
func Load(ctx context.Context, tx store.Tx, grants GrantSource) (*Input, error) {
	blocks, err := address.AllBlocks(ctx, tx)
	if err != nil {
		return nil, err
	}
	in := &Input{}
	if gw := blocks[address.SystemGateway]; len(gw) == 1 {
		in.Gateway = gw[0]
	}
	if ct := blocks[address.SystemControl]; len(ct) == 1 {
		in.Control = ct[0]
	}
	silos, err := tree.All(ctx, tx)
	if err != nil {
		return nil, err
	}
	eff, err := fenceapi.EffectiveControls(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, s := range silos {
		n := Silo{ID: s.ID, Depth: s.Depth, Blocks: blocks[s.ID], Sealed: Sealed(eff[s.ID])}
		if s.IsRoot() {
			if len(n.Blocks) > 0 && (!in.Gateway.IsValid() || !in.Control.IsValid()) {
				return nil, errors.New("fence: the root has a block but no gateway or control range")
			}
			for _, b := range n.Blocks {
				in.Root = append(in.Root, Minus(b, in.Gateway, in.Control)...)
			}
		}
		in.Silos = append(in.Silos, n)
	}
	if grants == nil {
		grants = AllGrants
	}
	if in.Grants, err = grants(ctx, tx); err != nil {
		return nil, err
	}
	return in, nil
}

// Sealed reports whether a silo with effective control policy p has no
// API port. Only full and read keep it: anything else, including a
// policy this build doesn't know, is sealed.
func Sealed(p string) bool { return p != control.Full && p != control.Read }

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

// Applied reads the last change-log entry the policy in GCE carries.
func Applied(ctx context.Context, tx store.Tx) (uint64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT applied_seq FROM fence_policy WHERE id = 1`).Scan(&n)
	return uint64(n), err
}

// setApplied records that the policy carries the change log up to seq.
// It never goes back.
func setApplied(ctx context.Context, tx store.Tx, seq uint64) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE fence_policy SET applied_seq = ?, updated_at = ? WHERE id = 1 AND applied_seq < ?`,
		int64(seq), store.Millis(now), int64(seq))
	return err
}

// lastSeq is the change log's newest entry as tx sees it.
func lastSeq(ctx context.Context, tx store.Tx) (uint64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT seq FROM change_counter WHERE id = 1`).Scan(&n)
	return uint64(n), err
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
	Grants GrantSource
}

// CheckBudget implements fenceapi.FenceBudget. It returns a
// *BudgetError for a policy over budget.
func (b Budget) CheckBudget(ctx context.Context, tx store.Tx) error {
	q, err := GetQuota(ctx, tx)
	if err != nil || (q.Rules == 0 && q.Attributes == 0) {
		return err
	}
	in, err := Load(ctx, tx, b.Grants)
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
