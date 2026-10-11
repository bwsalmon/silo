package fenceeval

import (
	"fmt"
	"net/netip"
	"sort"

	compute "google.golang.org/api/compute/v1"
)

// Op is what a Step does to a policy, after the networkFirewallPolicies
// method that does it.
type Op string

const (
	// AddRule adds Step.Rule at its priority (addRule).
	AddRule Op = "add"
	// RemoveRule removes the rule at Step.Priority (removeRule).
	RemoveRule Op = "remove"
	// PatchRule replaces the rule at Step.Priority with Step.Rule, which
	// may move it to Step.Rule.Priority (patchRule).
	PatchRule Op = "patch"
)

// Step is one rule-by-rule change to a firewall policy.
type Step struct {
	Op       Op
	Priority int64
	Rule     *compute.FirewallPolicyRule
}

func (s Step) String() string {
	switch s.Op {
	case AddRule:
		return fmt.Sprintf("add %s at %d", ruleName(s.Rule), s.Rule.Priority)
	case PatchRule:
		return fmt.Sprintf("patch %d to %s at %d", s.Priority, ruleName(s.Rule), s.Rule.Priority)
	}
	return fmt.Sprintf("%s %d", s.Op, s.Priority)
}

// Apply returns pol with s applied, as GCE would apply it, refusing what
// GCE refuses: an add or a patch onto a priority already taken, and a
// remove or a patch of a priority with no rule. pol is not modified.
func Apply(pol *compute.FirewallPolicy, s Step) (*compute.FirewallPolicy, error) {
	out := *pol
	out.Rules = append([]*compute.FirewallPolicyRule(nil), pol.Rules...)
	at := func(priority int64) int {
		for i, r := range out.Rules {
			if r.Priority == priority {
				return i
			}
		}
		return -1
	}
	switch s.Op {
	case AddRule:
		if s.Rule == nil {
			return nil, fmt.Errorf("fenceeval: %s with no rule", s.Op)
		}
		if at(s.Rule.Priority) >= 0 {
			return nil, fmt.Errorf("fenceeval: %s: a rule with priority %d already exists", s, s.Rule.Priority)
		}
		r := *s.Rule
		out.Rules = append(out.Rules, &r)
	case RemoveRule:
		i := at(s.Priority)
		if i < 0 {
			return nil, fmt.Errorf("fenceeval: %s: no rule has priority %d", s, s.Priority)
		}
		out.Rules = append(out.Rules[:i], out.Rules[i+1:]...)
	case PatchRule:
		if s.Rule == nil {
			return nil, fmt.Errorf("fenceeval: %s with no rule", s.Op)
		}
		i := at(s.Priority)
		if i < 0 {
			return nil, fmt.Errorf("fenceeval: %s: no rule has priority %d", s, s.Priority)
		}
		if s.Rule.Priority != s.Priority && at(s.Rule.Priority) >= 0 {
			return nil, fmt.Errorf("fenceeval: %s: a rule with priority %d already exists", s, s.Rule.Priority)
		}
		r := *s.Rule
		out.Rules[i] = &r
	default:
		return nil, fmt.Errorf("fenceeval: unknown step op %q", s.Op)
	}
	sort.SliceStable(out.Rules, func(i, j int) bool { return out.Rules[i].Priority < out.Rules[j].Priority })
	return &out, nil
}

// LooserError is CheckSteps' finding: after Step, the policy lets
// Packet through for some tenant rules where Than ("start" or "end")
// doesn't.
type LooserError struct {
	// Step is the index in the steps of the step after which the policy
	// is looser, and StepText says what it was.
	Step     int
	StepText string
	Than     string
	Packet   Packet
	// Got and Want are the policy's outcome for Packet on each side,
	// after Step and at Than.
	Got, Want string
}

func (e *LooserError) Error() string {
	return fmt.Sprintf("fenceeval: after step %d (%s) the fence is looser than at its %s for %s: %s, where its %s has %s",
		e.Step, e.StepText, e.Than, e.Packet, e.Got, e.Than, e.Want)
}

// CheckOptions narrow what CheckSteps tries.
type CheckOptions struct {
	// VMOnly are ranges where every address in use is a VM's (a silo
	// tree's blocks): External endpoints are tried only outside them.
	// With none, they are tried everywhere, which flags, say, a grant
	// whose egress rule is added before its ingress rule, since an
	// External receiver in the grantee's space would be reachable in
	// between.
	VMOnly []netip.Prefix
	// Empty are ranges that hold no endpoint at all: the blocks of a
	// silo being created or deleted. No packet to or from them is tried.
	// Adding a silo's deny-in and deny-out rules takes two steps, and
	// between them one direction between its blocks and its parent's
	// space is still decided as at the start; deleting one is the same
	// in reverse. That is safe only because nothing lives there.
	Empty []netip.Prefix
	// Exempt, if not nil, skips the packets it returns true for. A
	// change that is looser by design for a moment exempts what it
	// covers that way: sealing a subtree takes one step per silo and
	// relies on the gateway's own refusal meanwhile (gcp-backend.md
	// "Sealed silos"), and a silo's space can only be handed back to its
	// parent once nothing is left in it.
	Exempt func(Packet) bool
}

// CheckSteps applies steps to start one at a time and checks that every
// intermediate state is no looser than both start and the end, for any
// tenant VPC rules: whatever the tenants' rules, a packet an
// intermediate state allows is allowed at the start and at the end.
// The policy is taken to be evaluated before the VPC rules
// (BEFORE_CLASSIC_FIREWALL).
//
// The check is exact, not sampled. A policy's outcome for a packet
// depends only on its addresses, protocol and port, and only changes
// at the edges of the ranges and port ranges its rules name; one packet
// from each piece those edges cut the space into, for every state, sees
// every outcome there is. Each is tried between two VMs, from a VM to
// an External address and from an External address to a VM, as
// opts allows.
//
// It returns a *LooserError for the first looser packet found, or the
// error of a step GCE would refuse.
func CheckSteps(start *compute.FirewallPolicy, steps []Step, opts CheckOptions) error {
	states := []*compute.FirewallPolicy{start}
	for i, s := range steps {
		next, err := Apply(states[len(states)-1], s)
		if err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		states = append(states, next)
	}
	parsed := make([][]*policyRule, len(states))
	for i, st := range states {
		rs, err := parsePolicy(st.Rules)
		if err != nil {
			return fmt.Errorf("after step %d: %w", i-1, err)
		}
		parsed[i] = rs
	}
	if len(states) < 3 {
		return nil
	}
	pr := newProbes(parsed, append(append([]netip.Prefix(nil), opts.VMOnly...), opts.Empty...))
	n := len(parsed)
	eg := make([]outcome, n)
	in := make([]outcome, n)
	for _, src := range pr.addrs {
		for _, dst := range pr.addrs {
			if src.Is4() != dst.Is4() || inRanges(opts.Empty, src) || inRanges(opts.Empty, dst) {
				continue
			}
			for _, pp := range pr.l4 {
				for i, rs := range parsed {
					eg[i] = policyOutcome(rs, false, src, dst, pp.proto, pp.port)
					in[i] = policyOutcome(rs, true, src, dst, pp.proto, pp.port)
				}
				for _, kind := range []struct{ srcOut, dstOut bool }{{false, false}, {false, true}, {true, false}} {
					if (kind.srcOut && inRanges(opts.VMOnly, src)) || (kind.dstOut && inRanges(opts.VMOnly, dst)) {
						continue
					}
					pk := Packet{
						Src:      Endpoint{Addr: src, External: kind.srcOut},
						Dst:      Endpoint{Addr: dst, External: kind.dstOut},
						Protocol: protoName(pp.proto),
						Port:     pp.port,
					}
					side := func(i int) (outcome, outcome) {
						e, g := eg[i], in[i]
						if kind.srcOut {
							e = outcome{action: Allow}
						}
						if kind.dstOut {
							g = outcome{action: Allow}
						}
						return e, g
					}
					masks := make([]uint8, n)
					for i := range masks {
						masks[i] = allowedMask(side(i))
					}
					for i := 1; i < n-1; i++ {
						for _, than := range []int{0, n - 1} {
							if masks[i]&^masks[than] == 0 {
								continue
							}
							if opts.Exempt != nil && opts.Exempt(pk) {
								continue
							}
							name := "start"
							if than == n-1 {
								name = "end"
							}
							return &LooserError{
								Step: i - 1, StepText: steps[i-1].String(), Than: name, Packet: pk,
								Got: describe(side(i)), Want: describe(side(than)),
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// outcome is the policy's say over one side of a packet.
type outcome struct {
	action string // GotoNext when no rule matched
	rule   string
}

func policyOutcome(rules []*policyRule, ingress bool, src, dst netip.Addr, proto, port int) outcome {
	if r := firstPolicyMatch(rules, ingress, src, dst, proto, port); r != nil {
		return outcome{action: r.action, rule: r.name}
	}
	return outcome{action: GotoNext}
}

// allowedMask is the set of tenant verdicts (egress, ingress) under
// which the packet gets through, as bits 1<<(2*egress+ingress).
func allowedMask(eg, in outcome) uint8 {
	ok := func(o outcome, tenant bool) bool {
		switch o.action {
		case Allow:
			return true
		case Deny:
			return false
		}
		return tenant
	}
	var m uint8
	for b := 0; b < 4; b++ {
		if ok(eg, b&2 != 0) && ok(in, b&1 != 0) {
			m |= 1 << b
		}
	}
	return m
}

func describe(eg, in outcome) string {
	one := func(o outcome) string {
		if o.rule == "" {
			return o.action
		}
		return o.action + " (" + o.rule + ")"
	}
	return "egress " + one(eg) + ", ingress " + one(in)
}

// probes are one representative of each piece of the packet space that
// the rules' edges cut it into.
type probes struct {
	addrs []netip.Addr
	l4    []struct{ proto, port int }
}

func newProbes(states [][]*policyRule, extra []netip.Prefix) *probes {
	addrs := map[netip.Addr]bool{netip.IPv4Unspecified(): true, netip.IPv6Unspecified(): true}
	cut := func(p netip.Prefix) {
		addrs[p.Masked().Addr()] = true
		if next := lastAddr(p).Next(); next.IsValid() {
			addrs[next] = true
		}
	}
	for _, p := range extra {
		cut(p)
	}
	ports := map[int]map[int]bool{}
	for _, rs := range states {
		for _, r := range rs {
			for _, p := range r.src {
				cut(p)
			}
			for _, p := range r.dst {
				cut(p)
			}
			for _, c := range r.l4 {
				if c.proto == anyProto {
					continue
				}
				if ports[c.proto] == nil {
					ports[c.proto] = map[int]bool{0: true}
				}
				for _, pr := range c.ports {
					ports[c.proto][pr.lo] = true
					if pr.hi < 65535 {
						ports[c.proto][pr.hi+1] = true
					}
				}
			}
		}
	}
	// One protocol no rule names stands for all of them.
	for p := 0; p <= 255; p++ {
		if ports[p] == nil {
			ports[p] = map[int]bool{0: true}
			break
		}
	}
	out := &probes{}
	for a := range addrs {
		out.addrs = append(out.addrs, a)
	}
	sort.Slice(out.addrs, func(i, j int) bool { return out.addrs[i].Less(out.addrs[j]) })
	for proto, ps := range ports {
		for port := range ps {
			out.l4 = append(out.l4, struct{ proto, port int }{proto, port})
		}
	}
	sort.Slice(out.l4, func(i, j int) bool {
		if out.l4[i].proto != out.l4[j].proto {
			return out.l4[i].proto < out.l4[j].proto
		}
		return out.l4[i].port < out.l4[j].port
	})
	return out
}

// lastAddr is the last address in p.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}
