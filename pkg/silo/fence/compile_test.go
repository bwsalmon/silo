package fence

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	fe "github.com/bwsalmon/silo/pkg/silo/gce/fake/fenceeval"
	compute "google.golang.org/api/compute/v1"
)

func pfx(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func smallInput() *Input {
	return &Input{
		Gateway: genGateway, Control: genControl, Root: Minus(genSpace, genGateway, genControl),
		Silos: []Silo{
			{ID: "root", Depth: 0, Blocks: pfx("10.0.0.0/16")},
			{ID: "a", Depth: 1, Blocks: pfx("10.0.1.0/24", "10.0.2.0/24")},
			{ID: "b", Depth: 1, Blocks: pfx("10.0.16.0/20"), Sealed: true},
			{ID: "c", Depth: 2, Blocks: pfx("10.0.2.128/25")},
		},
	}
}

func byName(rs []*compute.FirewallPolicyRule) map[string]*compute.FirewallPolicyRule {
	out := map[string]*compute.FirewallPolicyRule{}
	for _, r := range rs {
		out[r.RuleName] = r
	}
	return out
}

func TestCompileShapeAndBands(t *testing.T) {
	rules, err := Compile(smallInput(), DefaultConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	rs := byName(rules)
	want := map[string]int64{
		"ctl-deny-out": 100,
		"gw-in":        10000, "gw-admin-in": 10001, "gw-out": 10003,
		// Depth 2 is the band at 20000 + 14 * 10000, depth 1 the next.
		"c-intra-in": 160000, "c-deny-out": 160005,
		"a-intra-in": 170000, "a-api-out": 170003, "a-deny-out": 170005,
		"b-intra-in": 170006, "b-gw-out": 170008, "b-deny-in": 170010, "b-deny-out": 170011,
		"root-intra-in": 180000, "root-deny-out": 180005,
		"catch-in": 2147483000, "catch-out": 2147483001,
	}
	for n, p := range want {
		if r := rs[n]; r == nil || r.Priority != p {
			t.Errorf("%s = %+v, want priority %d", n, r, p)
		}
	}
	if rs["b-api-out"] != nil {
		t.Errorf("sealed b has an api-out")
	}
	if rs["gw-health-in"] != nil {
		t.Errorf("gw-health-in with no gateway replicas")
	}
	// The root's ranges leave out the gateway and control ranges.
	for _, s := range rs["root-deny-out"].Match.SrcIpRanges {
		p := netip.MustParsePrefix(s)
		if p.Overlaps(genGateway) || p.Overlaps(genControl) {
			t.Errorf("root-deny-out names %s", s)
		}
	}
	if got := strings.Join(rs["a-intra-out"].Match.SrcIpRanges, ","); got != "10.0.1.0/24,10.0.2.0/24" {
		t.Errorf("a-intra-out names %s, want a's blocks", got)
	}
	if got := rs["a-gw-out"].Match.Layer4Configs[0].Ports; strings.Join(got, ",") != "80,3128,20000-29999" {
		t.Errorf("a-gw-out ports = %v", got)
	}
	cfg := DefaultConfig
	cfg.GatewayReplicas = true
	rules2, _ := Compile(smallInput(), cfg, rules)
	if r := byName(rules2)["gw-health-in"]; r == nil || r.Priority != 10002 {
		t.Errorf("gw-health-in = %+v", r)
	}
	if _, err := Compile(&Input{Gateway: genGateway, Control: genControl, Root: pfx("10.0.0.0/17"),
		Silos: []Silo{{ID: "x", Depth: 17, Blocks: pfx("10.0.0.0/24")}}}, DefaultConfig, nil); err == nil {
		t.Errorf("depth 17 compiled")
	}
}

func TestCompileKeepsSlots(t *testing.T) {
	in := smallInput()
	first, err := Compile(in, DefaultConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	// a goes, and d comes at depth 1: d takes a slot nobody holds, and
	// b keeps its own.
	in.Silos = append(in.Silos[:1:1], in.Silos[2:]...)
	in.Silos = append(in.Silos, Silo{ID: "d", Depth: 1, Blocks: pfx("10.0.64.0/20")})
	in.Silos[1].Sealed = false
	next, err := Compile(in, DefaultConfig, first)
	if err != nil {
		t.Fatal(err)
	}
	rs := byName(next)
	if rs["b-intra-in"].Priority != 170006 || rs["b-api-out"].Priority != 170009 {
		t.Errorf("b moved: %d", rs["b-intra-in"].Priority)
	}
	if rs["d-intra-in"].Priority != 170012 {
		t.Errorf("d at %d, want the first free slot 170012", rs["d-intra-in"].Priority)
	}
	// Once a's rules are gone from the policy, its slot is free again.
	steps := Plan(first, next)
	applied := withoutDefaults(first)
	for _, s := range steps {
		applied = applyStep(applied, s)
	}
	in.Silos = append(in.Silos, Silo{ID: "e", Depth: 1, Blocks: pfx("10.0.80.0/20")})
	again, _ := Compile(in, DefaultConfig, applied)
	if byName(again)["e-intra-in"].Priority != 170000 {
		t.Errorf("e at %d, want a's old slot 170000", byName(again)["e-intra-in"].Priority)
	}
}

func TestPlanOrder(t *testing.T) {
	in := smallInput()
	before, _ := Compile(in, DefaultConfig, nil)
	in.Silos = append(in.Silos, Silo{ID: "n", Depth: 2, Blocks: pfx("10.0.17.0/24")})
	after, _ := Compile(in, DefaultConfig, before)
	var names []string
	for _, s := range Plan(before, after) {
		names = append(names, string(s.Op)+" "+s.Rule.RuleName)
	}
	got := strings.Join(names, ", ")
	if got != "add n-deny-in, add n-deny-out, add n-intra-in, add n-intra-out, add n-gw-out, add n-api-out" {
		t.Errorf("a new silo's steps: %s", got)
	}
	// Deleting it goes the other way: its deny rules go last.
	steps := Plan(after, before)
	if len(steps) != 6 {
		t.Fatalf("deleting a silo: %v", steps)
	}
	last := byName(after)
	if steps[4].Priority != last["n-deny-in"].Priority || steps[5].Priority != last["n-deny-out"].Priority {
		t.Errorf("deleting a silo: %v, want its deny rules removed last", steps)
	}
}

// TestCheckStepsCatchesUnsealing shows the intermediate-state check is
// live: unsealing a subtree takes one step per silo, and without the
// exemption for the API port the check finds the moment one silo has
// its port back and another doesn't.
func TestCheckStepsCatchesUnsealing(t *testing.T) {
	in := smallInput()
	in.Silos[1].Sealed, in.Silos[3].Sealed = true, true // a and c sealed
	before, _ := Compile(in, DefaultConfig, nil)
	in.Silos[1].Sealed, in.Silos[3].Sealed = false, false
	after, _ := Compile(in, DefaultConfig, before)
	steps := Plan(before, after)
	if len(steps) != 2 {
		t.Fatalf("unsealing a and c: %v", steps)
	}
	err := fe.CheckSteps(policyOf(before), evalSteps(steps), fe.CheckOptions{})
	var le *fe.LooserError
	if !errors.As(err, &le) || le.Packet.Port != APIPort {
		t.Fatalf("CheckSteps = %v, want the API port looser between the steps", err)
	}
	exempt := fe.CheckOptions{Exempt: func(p fe.Packet) bool { return p.Protocol == "tcp" && p.Port == APIPort }}
	if err := fe.CheckSteps(policyOf(before), evalSteps(steps), exempt); err != nil {
		t.Fatal(err)
	}
}
