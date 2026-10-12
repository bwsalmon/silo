package fence

import (
	"fmt"
	"reflect"
	"sort"

	compute "google.golang.org/api/compute/v1"
)

// Op is what a Step does, after the networkFirewallPolicies method that
// does it.
type Op string

const (
	AddRule    Op = "add"    // addRule: Step.Rule at its priority
	RemoveRule Op = "remove" // removeRule: the rule at Step.Priority
	PatchRule  Op = "patch"  // patchRule: the rule at Step.Priority becomes Step.Rule
)

// Step is one rule-by-rule change to the policy.
type Step struct {
	Op       Op
	Priority int64
	Rule     *compute.FirewallPolicyRule
}

func (s Step) String() string {
	if s.Rule != nil {
		return fmt.Sprintf("%s %s at %d", s.Op, s.Rule.RuleName, s.Priority)
	}
	return fmt.Sprintf("%s %d", s.Op, s.Priority)
}

// Plan is the steps that take the policy's rules from cur to want, in
// the order that keeps every intermediate state no looser than either
// end (gcp-backend.md "Ordering"). GCE's default rules in cur are left
// alone. The order is:
//
//  1. Rules whose ranges change are first patched to the tighter of the
//     two: a deny rule to the union of its old and new ranges, any
//     other rule to their intersection.
//  2. Adds, deny rules first: a new silo's n-deny-in and n-deny-out go
//     in before anything that lets a packet on.
//  3. Patches to the final rules: the allow and goto_next rules first,
//     then the deny rules.
//  4. Removes, the allow and goto_next rules first and the deny rules
//     last, so a silo's deny rules are the last of it to go.
//
// Since every rule keeps its priority (Compile), no step moves a rule.
func Plan(cur, want []*compute.FirewallPolicyRule) []Step {
	have := map[int64]*compute.FirewallPolicyRule{}
	for _, r := range cur {
		if r.Priority < gceDefaults {
			have[r.Priority] = r
		}
	}
	wanted := map[int64]*compute.FirewallPolicyRule{}
	for _, r := range want {
		wanted[r.Priority] = r
	}
	var tighten, addDeny, addOther, finalOther, finalDeny, removeOther, removeDeny []Step
	for _, w := range sortedRules(wanted) {
		h, ok := have[w.Priority]
		if !ok {
			s := Step{Op: AddRule, Priority: w.Priority, Rule: w}
			if w.Action == "deny" {
				addDeny = append(addDeny, s)
			} else {
				addOther = append(addOther, s)
			}
			continue
		}
		if Same(h, w) {
			continue
		}
		if mid := between(h, w); mid != nil && !Same(mid, h) && !Same(mid, w) {
			tighten = append(tighten, Step{Op: PatchRule, Priority: w.Priority, Rule: mid})
		}
		s := Step{Op: PatchRule, Priority: w.Priority, Rule: w}
		if w.Action == "deny" {
			finalDeny = append(finalDeny, s)
		} else {
			finalOther = append(finalOther, s)
		}
	}
	for _, h := range sortedRules(have) {
		if _, ok := wanted[h.Priority]; ok {
			continue
		}
		s := Step{Op: RemoveRule, Priority: h.Priority}
		if h.Action == "deny" {
			removeDeny = append(removeDeny, s)
		} else {
			removeOther = append(removeOther, s)
		}
	}
	var out []Step
	for _, ss := range [][]Step{tighten, addDeny, addOther, finalOther, finalDeny, removeOther, removeDeny} {
		out = append(out, ss...)
	}
	return out
}

func sortedRules(m map[int64]*compute.FirewallPolicyRule) []*compute.FirewallPolicyRule {
	out := make([]*compute.FirewallPolicyRule, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// between is the tighter of h and w when they differ only in their
// ranges, or nil: a deny rule matching the union of both, anything else
// the intersection. nil too when the intersection is empty, since GCE
// would read an empty range list as no limit at all.
func between(h, w *compute.FirewallPolicyRule) *compute.FirewallPolicyRule {
	if !sameExceptRanges(h, w) {
		return nil
	}
	hs, ok1 := parsePrefixes(h.Match.SrcIpRanges)
	hd, ok2 := parsePrefixes(h.Match.DestIpRanges)
	ws, ok3 := parsePrefixes(w.Match.SrcIpRanges)
	wd, ok4 := parsePrefixes(w.Match.DestIpRanges)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil
	}
	combine := intersect
	if w.Action == "deny" {
		combine = union
	}
	src, dst := hs, hd
	// An empty list puts no limit on that end: it must stay empty on
	// both sides for the combination to mean anything.
	if (len(hs) == 0) != (len(ws) == 0) || (len(hd) == 0) != (len(wd) == 0) {
		return nil
	}
	if len(hs) > 0 {
		if src = combine(hs, ws); len(src) == 0 {
			return nil
		}
	}
	if len(hd) > 0 {
		if dst = combine(hd, wd); len(dst) == 0 {
			return nil
		}
	}
	mid := *w
	m := *w.Match
	m.SrcIpRanges, m.DestIpRanges = strs(src), strs(dst)
	mid.Match = &m
	return &mid
}

// Same reports whether a and b are the same rule, as far as the fence
// is concerned: GCE's output-only fields are not compared.
func Same(a, b *compute.FirewallPolicyRule) bool {
	return sameExceptRanges(a, b) &&
		sameRanges(a.Match.SrcIpRanges, b.Match.SrcIpRanges) &&
		sameRanges(a.Match.DestIpRanges, b.Match.DestIpRanges)
}

func sameExceptRanges(a, b *compute.FirewallPolicyRule) bool {
	if a.Priority != b.Priority || a.RuleName != b.RuleName || a.Action != b.Action ||
		a.Direction != b.Direction || a.Disabled != b.Disabled || a.EnableLogging != b.EnableLogging ||
		a.Description != b.Description || (a.Match == nil) != (b.Match == nil) {
		return false
	}
	if a.Match == nil {
		return true
	}
	if len(a.TargetResources)+len(a.TargetSecureTags)+len(a.TargetServiceAccounts)+
		len(b.TargetResources)+len(b.TargetSecureTags)+len(b.TargetServiceAccounts) > 0 {
		return false
	}
	ma, mb := *a.Match, *b.Match
	ma.SrcIpRanges, ma.DestIpRanges, mb.SrcIpRanges, mb.DestIpRanges = nil, nil, nil, nil
	if len(ma.Layer4Configs) != len(mb.Layer4Configs) {
		return false
	}
	for i := range ma.Layer4Configs {
		x, y := ma.Layer4Configs[i], mb.Layer4Configs[i]
		if x.IpProtocol != y.IpProtocol || !reflect.DeepEqual(nonNil(x.Ports), nonNil(y.Ports)) {
			return false
		}
	}
	ma.Layer4Configs, mb.Layer4Configs = nil, nil
	ma.ForceSendFields, mb.ForceSendFields, ma.NullFields, mb.NullFields = nil, nil, nil, nil
	return reflect.DeepEqual(ma, mb)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func sameRanges(a, b []string) bool {
	pa, ok1 := parsePrefixes(a)
	pb, ok2 := parsePrefixes(b)
	if !ok1 || !ok2 {
		return reflect.DeepEqual(a, b)
	}
	return reflect.DeepEqual(strs(pa), strs(pb))
}
