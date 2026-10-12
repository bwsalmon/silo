package fenceeval

import (
	"errors"
	"net/netip"
	"testing"

	compute "google.golang.org/api/compute/v1"
)

func ruleNamed(t *testing.T, pol *compute.FirewallPolicy, name string) *compute.FirewallPolicyRule {
	t.Helper()
	for _, r := range pol.Rules {
		if r.RuleName == name {
			return r
		}
	}
	t.Fatalf("no rule %s", name)
	return nil
}

func adds(t *testing.T, pol *compute.FirewallPolicy, names ...string) []Step {
	var out []Step
	for _, n := range names {
		out = append(out, Step{Op: AddRule, Rule: ruleNamed(t, pol, n)})
	}
	return out
}

func removes(t *testing.T, pol *compute.FirewallPolicy, names ...string) []Step {
	var out []Step
	for _, n := range names {
		out = append(out, Step{Op: RemoveRule, Priority: ruleNamed(t, pol, n).Priority})
	}
	return out
}

// tenantSpace is where every address in use is a VM: the root's block.
func tenantSpace() []netip.Prefix {
	var out []netip.Prefix
	for _, b := range rootBlocks {
		out = append(out, netip.MustParsePrefix(b))
	}
	return out
}

func wantLooser(t *testing.T, err error, than string) *LooserError {
	t.Helper()
	var le *LooserError
	if !errors.As(err, &le) {
		t.Fatalf("CheckSteps = %v, want a LooserError", err)
	}
	if le.Than != than {
		t.Errorf("looser than its %s, want %s: %v", le.Than, than, le)
	}
	return le
}

func TestCheckStepsNewSilo(t *testing.T) {
	start := baseTree()
	end := baseTree()
	d := silo{id: "d", depth: 1, blocks: []string{"10.0.32.0/24"}}
	end.silos = append(end.silos, d)
	pol := end.policy()
	steps := adds(t, pol, "d-deny-in", "d-deny-out", "d-gw-out", "d-api-out", "d-intra-in", "d-intra-out")

	// Between d's two deny rules, packets from d's space to the root's
	// are still the root's to decide, where the end denies them.
	le := wantLooser(t, CheckSteps(start.policy(), steps, CheckOptions{VMOnly: tenantSpace()}), "end")
	if le.Step != 0 || !netip.MustParsePrefix(d.blocks[0]).Contains(le.Packet.Src.Addr) {
		t.Errorf("found %v, want a packet from d after step 0", le)
	}
	// A new silo's space holds nothing yet (gcp-backend.md "Ordering").
	opts := CheckOptions{VMOnly: tenantSpace(), Empty: []netip.Prefix{netip.MustParsePrefix(d.blocks[0])}}
	if err := CheckSteps(start.policy(), steps, opts); err != nil {
		t.Errorf("with d empty: %v", err)
	}
	// Deleting it again, once it is empty.
	if err := CheckSteps(pol, removes(t, pol, "d-intra-in", "d-intra-out", "d-gw-out", "d-api-out", "d-deny-in", "d-deny-out"), opts); err != nil {
		t.Errorf("deleting d: %v", err)
	}
	// Emptiness covers d's space and nothing else: a step that opens the
	// root to the gateway's port 22 is still found.
	hole := prule("hole", 999, "EGRESS", Allow, siloRoot.blocks, []string{gwRange}, l4cfg("tcp", "22"))
	withHole := append(append([]Step{}, steps[:2]...), Step{Op: AddRule, Rule: hole}, Step{Op: RemoveRule, Priority: 999})
	withHole = append(withHole, steps[2:]...)
	le = wantLooser(t, CheckSteps(start.policy(), withHole, opts), "start")
	if le.Packet.Port != 22 || netip.MustParsePrefix(d.blocks[0]).Contains(le.Packet.Src.Addr) {
		t.Errorf("found %s, want port 22 from outside d", le.Packet)
	}
}

func TestCheckStepsGrant(t *testing.T) {
	start := baseTree()
	start.links = nil
	pol := baseTree().policy()
	steps := adds(t, pol, "link-ab-out", "link-ab-in")
	if err := CheckSteps(start.policy(), steps, CheckOptions{VMOnly: tenantSpace()}); err != nil {
		t.Errorf("adding a link: %v", err)
	}
	// Without VMOnly, an External receiver in b's space is reachable
	// from a once the egress rule is in.
	le := wantLooser(t, CheckSteps(start.policy(), steps, CheckOptions{}), "start")
	if !le.Packet.Dst.External || !netip.MustParsePrefix(siloB.blocks[0]).Contains(le.Packet.Dst.Addr) {
		t.Errorf("found %s, want a packet to an outside address in b", le.Packet)
	}
	// Removing it the other way round is as safe.
	if err := CheckSteps(pol, removes(t, pol, "link-ab-in", "link-ab-out"), CheckOptions{VMOnly: tenantSpace()}); err != nil {
		t.Errorf("removing a link: %v", err)
	}
}

func TestCheckStepsSealSubtree(t *testing.T) {
	start := baseTree().policy()
	steps := removes(t, start, "a-api-out", "c-api-out")
	// Between the two, c still reaches the API port while a doesn't,
	// and the end has neither.
	le := wantLooser(t, CheckSteps(start, steps, CheckOptions{VMOnly: tenantSpace()}), "end")
	if le.Packet.Protocol != "tcp" || le.Packet.Port != 8080 {
		t.Errorf("found %s, want TCP 8080", le.Packet)
	}
	// The gateway's own refusal covers the API port meanwhile.
	api := func(p Packet) bool { return p.Protocol == "tcp" && p.Port == 8080 }
	if err := CheckSteps(start, steps, CheckOptions{VMOnly: tenantSpace(), Exempt: api}); err != nil {
		t.Errorf("with the API port exempt: %v", err)
	}
	// One silo alone is one step.
	if err := CheckSteps(start, removes(t, start, "c-api-out"), CheckOptions{}); err != nil {
		t.Errorf("sealing c: %v", err)
	}
}

func TestCheckStepsFindsNarrowHoles(t *testing.T) {
	start := baseTree().policy()
	// For one step, a reaches one gateway address on one port.
	hole := prule("hole", 999, "EGRESS", Allow, siloA.blocks, []string{"10.0.255.7/32"}, l4cfg("tcp", "2222"))
	steps := []Step{{Op: AddRule, Rule: hole}, {Op: RemoveRule, Priority: 999}}
	le := wantLooser(t, CheckSteps(start, steps, CheckOptions{VMOnly: tenantSpace()}), "start")
	p := le.Packet
	if p.Dst.Addr != netip.MustParseAddr("10.0.255.7") || p.Protocol != "tcp" || p.Port != 2222 ||
		!netip.MustParsePrefix("10.0.1.0/24").Contains(p.Src.Addr) && !netip.MustParsePrefix("10.0.2.0/24").Contains(p.Src.Addr) {
		t.Errorf("found %s", p)
	}

	// Patching a-gw-out to add a port, and back.
	gw := ruleNamed(t, start, "a-gw-out")
	wider := *gw
	wider.Match = &compute.FirewallPolicyRuleMatcher{
		SrcIpRanges: gw.Match.SrcIpRanges, DestIpRanges: gw.Match.DestIpRanges,
		Layer4Configs: []*compute.FirewallPolicyRuleMatcherLayer4Config{l4cfg("tcp", "80", "3128", "22", exportPorts)},
	}
	steps = []Step{{Op: PatchRule, Priority: gw.Priority, Rule: &wider}, {Op: PatchRule, Priority: gw.Priority, Rule: gw}}
	le = wantLooser(t, CheckSteps(start, steps, CheckOptions{VMOnly: tenantSpace()}), "start")
	if le.Packet.Port != 22 {
		t.Errorf("found %s, want port 22", le.Packet)
	}
}

func TestApply(t *testing.T) {
	start := baseTree().policy()
	gw := ruleNamed(t, start, "a-gw-out")
	for name, s := range map[string]Step{
		"add onto a taken priority":   {Op: AddRule, Rule: &compute.FirewallPolicyRule{Priority: gw.Priority}},
		"remove a missing priority":   {Op: RemoveRule, Priority: 5},
		"patch a missing priority":    {Op: PatchRule, Priority: 5, Rule: gw},
		"patch onto a taken priority": {Op: PatchRule, Priority: gw.Priority, Rule: &compute.FirewallPolicyRule{Priority: 100}},
		"unknown op":                  {Op: "move"},
		"add nothing":                 {Op: AddRule},
	} {
		if _, err := Apply(start, s); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	moved := *gw
	moved.Priority = 5
	got, err := Apply(start, Step{Op: PatchRule, Priority: gw.Priority, Rule: &moved})
	if err != nil {
		t.Fatal(err)
	}
	if got.Rules[0].RuleName != "a-gw-out" || got.Rules[0].Priority != 5 || len(got.Rules) != len(start.Rules) {
		t.Errorf("patched rule not moved into place: %v", got.Rules[0])
	}
	if gw.Priority == 5 || start.Rules[0].RuleName != "ctl-deny-out" {
		t.Error("Apply modified its input")
	}
}
