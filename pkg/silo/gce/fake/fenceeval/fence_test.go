package fenceeval

import (
	"fmt"
	"net/netip"
	"testing"

	compute "google.golang.org/api/compute/v1"
)

// A fence written by hand from gcp-backend.md "The fence", "Ordering"
// and "Sealed silos" and state.md "What the fence does", for a small
// tree: the root, silos a and b at depth 1 (b sealed), and c, a's child,
// at depth 2. The fence compiler is tested against the evaluator later;
// these tests check the evaluator against the fence as the design
// writes it.

const (
	addressSpace = "10.0.0.0/16"
	gwRange      = "10.0.255.0/24"
	ctlRange     = "10.0.254.0/24"
	iapRange     = "35.235.240.0/20"
	apiPort      = "8080"
	frontDoor    = "443"
	exportPorts  = "30000-30099"
)

// rootBlocks is the root's block: the address space minus the gateway
// and control ranges.
var rootBlocks = minus(addressSpace, gwRange, ctlRange)

type silo struct {
	id     string
	depth  int
	blocks []string
	sealed bool
}

type link struct {
	name           string
	from, to       silo
	proto          string
	ports          []string
	fromOut, toOut bool
}

type export struct {
	name, ip, port string
}

var (
	siloRoot = silo{id: "root", depth: 0, blocks: rootBlocks}
	siloA    = silo{id: "a", depth: 1, blocks: []string{"10.0.1.0/24", "10.0.2.0/24"}}
	siloB    = silo{id: "b", depth: 1, blocks: []string{"10.0.16.0/20"}, sealed: true}
	siloC    = silo{id: "c", depth: 2, blocks: []string{"10.0.2.128/25"}}

	linkAB   = link{name: "ab", from: siloA, to: siloB, proto: "tcp", ports: []string{"5432"}}
	exportBX = export{name: "x", ip: "10.0.16.5", port: "30001"}
)

type tree struct {
	silos   []silo
	links   []link
	exports []export
	// extraGrants are rules written into the grants band by hand, to
	// show that no grant can open the control range.
	extraGrants []*compute.FirewallPolicyRule
}

func baseTree() tree {
	return tree{
		silos:   []silo{siloRoot, siloA, siloB, siloC},
		links:   []link{linkAB},
		exports: []export{exportBX},
	}
}

func l4cfg(name string, ports ...string) *compute.FirewallPolicyRuleMatcherLayer4Config {
	return &compute.FirewallPolicyRuleMatcherLayer4Config{IpProtocol: name, Ports: ports}
}

func prule(name string, priority int64, dir, action string, src, dst []string, l4s ...*compute.FirewallPolicyRuleMatcherLayer4Config) *compute.FirewallPolicyRule {
	return &compute.FirewallPolicyRule{
		RuleName: name, Priority: priority, Direction: dir, Action: action,
		Match: &compute.FirewallPolicyRuleMatcher{SrcIpRanges: src, DestIpRanges: dst, Layer4Configs: l4s},
	}
}

var anywhere = []string{"0.0.0.0/0"}

// siloBase is the first priority of a silo's six slots: its depth's band,
// then its place among the silos of that depth.
func siloBase(depth, index int) int64 {
	return 20000 + int64(16-depth)*10000 + int64(index)*6
}

// siloRules are a silo's rules, in its slots. Each rule keeps its slot
// whether or not n-api-out is present.
func siloRules(n silo, base int64) []*compute.FirewallPolicyRule {
	b := n.blocks
	gw := []string{gwRange}
	rs := []*compute.FirewallPolicyRule{
		prule(n.id+"-intra-in", base, "INGRESS", GotoNext, b, b, l4cfg("all")),
		prule(n.id+"-intra-out", base+1, "EGRESS", GotoNext, b, b, l4cfg("all")),
		prule(n.id+"-gw-out", base+2, "EGRESS", Allow, b, gw, l4cfg("tcp", "80", "3128", exportPorts)),
	}
	if !n.sealed {
		rs = append(rs, prule(n.id+"-api-out", base+3, "EGRESS", Allow, b, gw, l4cfg("tcp", apiPort)))
	}
	return append(rs,
		prule(n.id+"-deny-in", base+4, "INGRESS", Deny, anywhere, b, l4cfg("all")),
		prule(n.id+"-deny-out", base+5, "EGRESS", Deny, b, anywhere, l4cfg("all")),
	)
}

// policy is the fence for t, with GCE's default rules at the end.
func (t tree) policy() *compute.FirewallPolicy {
	var rs []*compute.FirewallPolicyRule
	// Control band.
	rs = append(rs, prule("ctl-deny-out", 100, "EGRESS", Deny, rootBlocks, []string{ctlRange}, l4cfg("all")))
	// Grants band.
	p := int64(1000)
	for _, x := range t.exports {
		rs = append(rs, prule("export-"+x.name+"-in", p, "INGRESS", Allow, []string{gwRange}, []string{x.ip + "/32"}, l4cfg("tcp", x.port)))
		p++
	}
	for _, l := range t.links {
		rs = append(rs,
			prule("link-"+l.name+"-out", p, "EGRESS", Allow, l.from.blocks, l.to.blocks, l4cfg(l.proto, l.ports...)),
			prule("link-"+l.name+"-in", p+1, "INGRESS", Allow, l.from.blocks, l.to.blocks, l4cfg(l.proto, l.ports...)))
		p += 2
	}
	for _, r := range t.extraGrants {
		r.Priority = p
		rs = append(rs, r)
		p++
	}
	// Gateway band.
	rs = append(rs,
		prule("gw-in", 10000, "INGRESS", Allow, rootBlocks, []string{gwRange}, l4cfg("tcp", "80", apiPort, "3128", exportPorts)),
		prule("gw-admin-in", 10001, "INGRESS", Allow, []string{iapRange}, []string{gwRange}, l4cfg("tcp", frontDoor)),
		prule("gw-out", 10002, "EGRESS", GotoNext, []string{gwRange}, anywhere, l4cfg("all")),
	)
	// Silo bands, deepest first.
	perDepth := map[int]int{}
	for _, n := range t.silos {
		rs = append(rs, siloRules(n, siloBase(n.depth, perDepth[n.depth]))...)
		perDepth[n.depth]++
	}
	// Catch-all band.
	rs = append(rs,
		prule("catch-in", 2147483000, "INGRESS", Deny, anywhere, nil, l4cfg("all")),
		prule("catch-out", 2147483001, "EGRESS", Deny, nil, anywhere, l4cfg("all")),
	)
	// GCE's own default rules.
	for i, d := range []struct{ dir, cidr string }{{"EGRESS", "::/0"}, {"INGRESS", "::/0"}, {"EGRESS", "0.0.0.0/0"}, {"INGRESS", "0.0.0.0/0"}} {
		r := prule("", 2147483644+int64(i), d.dir, GotoNext, nil, nil, l4cfg("all"))
		if d.dir == "INGRESS" {
			r.Match.SrcIpRanges = []string{d.cidr}
		} else {
			r.Match.DestIpRanges = []string{d.cidr}
		}
		rs = append(rs, r)
	}
	return &compute.FirewallPolicy{Name: "silo-fence", Rules: rs}
}

// minus is from with the given ranges taken out, as CIDRs.
func minus(from string, holes ...string) []string {
	ps := []netip.Prefix{netip.MustParsePrefix(from)}
	for _, h := range holes {
		hole := netip.MustParsePrefix(h)
		var next []netip.Prefix
		for _, p := range ps {
			next = append(next, cut(p, hole)...)
		}
		ps = next
	}
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

func cut(p, hole netip.Prefix) []netip.Prefix {
	switch {
	case !p.Overlaps(hole):
		return []netip.Prefix{p}
	case hole.Bits() <= p.Bits():
		return nil
	}
	lo := netip.PrefixFrom(p.Addr(), p.Bits()+1)
	hi := netip.PrefixFrom(lastAddr(lo).Next(), p.Bits()+1)
	return append(cut(lo, hole), cut(hi, hole)...)
}

// The VMs and outside addresses the tests send between.
var (
	vmRoot   = VM("10.0.128.10", "root")
	vmA1     = VM("10.0.1.10", "a")
	vmA2     = VM("10.0.2.10", "a")
	vmB      = VM(exportBX.ip, "b")
	vmC1     = VM("10.0.2.200", "c")
	vmC2     = VM("10.0.2.201", "c")
	vmGW     = VM("10.0.255.2") // silo-control, in the gateway range
	pscDB    = Outside("10.0.254.5")
	internet = Outside("8.8.8.8")
	iapHost  = Outside("35.235.240.7")

	siloVMs = []Endpoint{vmRoot, vmA1, vmB, vmC1}
)

// pinned is a tenant rule as the facade writes it for silo n: its tags
// prefixed, targetTags [n] when none were given, and an ingress rule's
// source (an egress rule's destination) cut to n's space.
func pinned(n silo, name string, fw compute.Firewall) *compute.Firewall {
	fw.Name = n.id + "-" + name
	for i, t := range fw.TargetTags {
		fw.TargetTags[i] = n.id + "-" + t
	}
	for i, t := range fw.SourceTags {
		fw.SourceTags[i] = n.id + "-" + t
	}
	if len(fw.TargetTags) == 0 {
		fw.TargetTags = []string{n.id}
	}
	if fw.Direction == "EGRESS" {
		fw.DestinationRanges = n.blocks
	} else if len(fw.SourceTags) == 0 {
		fw.SourceRanges = n.blocks
	}
	return &fw
}

func allowAll(n silo) []*compute.Firewall {
	all := []*compute.FirewallAllowed{{IPProtocol: "all"}}
	return []*compute.Firewall{
		pinned(n, "all-in", compute.Firewall{Allowed: all}),
		pinned(n, "all-out", compute.Firewall{Direction: "EGRESS", Allowed: all}),
	}
}

func newEval(t *testing.T, tr tree, fws ...*compute.Firewall) *Evaluator {
	t.Helper()
	e, err := New(BeforeClassic, tr.policy(), fws)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func eval(t *testing.T, e *Evaluator, p Packet) Verdict {
	t.Helper()
	v, err := e.Evaluate(p)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func wantAllowed(t *testing.T, e *Evaluator, p Packet, want bool) Verdict {
	t.Helper()
	v := eval(t, e, p)
	if v.Allowed != want {
		t.Errorf("%s: %s, want allowed=%v", p, v, want)
	}
	return v
}

func TestFenceIntraSiloIsTheTenants(t *testing.T) {
	e := newEval(t, baseTree())
	v := wantAllowed(t, e, TCP(vmA1, vmA2, 22), false)
	if v.Egress != (Decision{Allowed: true, Stage: StageImplied, Rule: ImpliedAllowEgress, Priority: 65535, Handoff: "a-intra-out"}) {
		t.Errorf("egress = %+v", v.Egress)
	}
	if v.Ingress != (Decision{Stage: StageImplied, Rule: ImpliedDenyIngress, Priority: 65535, Handoff: "a-intra-in"}) {
		t.Errorf("ingress = %+v", v.Ingress)
	}

	ssh := pinned(siloA, "ssh", compute.Firewall{Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}})
	e = newEval(t, baseTree(), ssh)
	v = wantAllowed(t, e, TCP(vmA1, vmA2, 22), true)
	if v.Ingress.Stage != StageVPC || v.Ingress.Rule != "a-ssh" || v.Ingress.Handoff != "a-intra-in" {
		t.Errorf("ingress = %+v, want a-ssh after a-intra-in", v.Ingress)
	}
	wantAllowed(t, e, TCP(vmA1, vmA2, 23), false)
	wantAllowed(t, e, Packet{Src: vmA1, Dst: vmA2, Protocol: "udp", Port: 22}, false)
	// The pinned rule names a's space, so it can't let anything else in,
	// and the fence wouldn't anyway.
	wantAllowed(t, e, TCP(vmRoot, vmA2, 22), false)
}

func TestFenceCrossSiloNeedsAGrant(t *testing.T) {
	// Every silo's tenant allows everything, and one rule with no target
	// allows everything everywhere, as an unpinned rule would.
	var fws []*compute.Firewall
	for _, n := range baseTree().silos {
		fws = append(fws, allowAll(n)...)
	}
	fws = append(fws, &compute.Firewall{Name: "wide-open", Allowed: []*compute.FirewallAllowed{{IPProtocol: "all"}}, SourceRanges: anywhere})
	e := newEval(t, baseTree(), fws...)

	in := map[netip.Addr]string{vmRoot.Addr: "root", vmA1.Addr: "a", vmB.Addr: "b", vmC1.Addr: "c"}
	granted := func(src, dst Endpoint, proto string, port int) bool {
		// The link ab covers a's blocks, and so c's, which lie inside them.
		return (in[src.Addr] == "a" || in[src.Addr] == "c") && in[dst.Addr] == "b" && proto == "tcp" && port == 5432
	}
	for _, src := range siloVMs {
		for _, dst := range siloVMs {
			if src.Addr == dst.Addr {
				continue
			}
			for _, pp := range []struct {
				proto string
				port  int
			}{{"tcp", 22}, {"tcp", 5432}, {"udp", 53}, {"icmp", 0}, {"47", 0}} {
				p := Packet{Src: src, Dst: dst, Protocol: pp.proto, Port: pp.port}
				wantAllowed(t, e, p, granted(src, dst, pp.proto, pp.port))
			}
		}
	}

	// The grant is the fence's: no tenant rule is needed for it.
	e = newEval(t, baseTree())
	v := wantAllowed(t, e, TCP(vmA1, vmB, 5432), true)
	if v.Egress.Rule != "link-ab-out" || v.Ingress.Rule != "link-ab-in" {
		t.Errorf("a->b 5432: %s", v)
	}
	wantAllowed(t, e, TCP(vmB, vmA1, 5432), false)
	wantAllowed(t, e, TCP(vmA1, vmB, 5433), false)
}

func TestFenceGateway(t *testing.T) {
	e := newEval(t, baseTree())
	for _, src := range []Endpoint{vmRoot, vmA1, vmB, vmC1} {
		for _, port := range []int{80, 3128, 30000, 30050, 30099} {
			wantAllowed(t, e, TCP(src, vmGW, port), true)
		}
		// b is sealed; the others keep the API port.
		wantAllowed(t, e, TCP(src, vmGW, 8080), src.Addr != vmB.Addr)
		for _, port := range []int{22, 443, 30100, 8081} {
			wantAllowed(t, e, TCP(src, vmGW, port), false)
		}
		wantAllowed(t, e, Packet{Src: src, Dst: vmGW, Protocol: "udp", Port: 80}, false)
		wantAllowed(t, e, Packet{Src: src, Dst: vmGW, Protocol: "icmp"}, false)
		wantAllowed(t, e, TCP(src, internet, 443), false)
	}
	v := wantAllowed(t, e, TCP(vmB, vmGW, 8080), false)
	if v.Egress.Rule != "b-deny-out" {
		t.Errorf("sealed b to the API port: %s, want b-deny-out", v)
	}
	// c is a's child: its own rules decide, before a's.
	if v := eval(t, e, TCP(vmC1, vmGW, 8080)); v.Egress.Rule != "c-api-out" {
		t.Errorf("c to the API port: %s, want c-api-out", v)
	}

	wantAllowed(t, e, TCP(iapHost, vmGW, 443), true)
	wantAllowed(t, e, TCP(iapHost, vmGW, 22), false)
	wantAllowed(t, e, TCP(iapHost, vmA1, 22), false)
	wantAllowed(t, e, TCP(internet, vmGW, 80), false)
	// Exports: the gateway reaches the export's address and port only.
	wantAllowed(t, e, TCP(vmGW, vmB, 30001), true)
	wantAllowed(t, e, TCP(vmGW, vmB, 30002), false)
	wantAllowed(t, e, TCP(vmGW, vmA1, 30001), false)
}

func TestFenceControlRange(t *testing.T) {
	// A grant written wrongly, opening the control range to a, is still
	// overridden by the control band.
	tr := baseTree()
	tr.extraGrants = []*compute.FirewallPolicyRule{
		prule("link-bad-out", 0, "EGRESS", Allow, siloA.blocks, []string{ctlRange}, l4cfg("all")),
	}
	var fws []*compute.Firewall
	for _, n := range tr.silos {
		fws = append(fws, allowAll(n)...)
	}
	fws = append(fws, &compute.Firewall{Name: "open-out", Direction: "EGRESS", Allowed: []*compute.FirewallAllowed{{IPProtocol: "all"}}})
	e := newEval(t, tr, fws...)
	for _, src := range siloVMs {
		for _, p := range []Packet{
			TCP(src, pscDB, 5432),
			TCP(src, pscDB, 443),
			{Src: src, Dst: pscDB, Protocol: "udp", Port: 53},
			{Src: src, Dst: pscDB, Protocol: "icmp"},
			{Src: src, Dst: pscDB, Protocol: "132", Port: 9},
			{Src: src, Dst: VM("10.0.254.9"), Protocol: "tcp", Port: 22},
		} {
			if v := wantAllowed(t, e, p, false); v.Egress.Rule != "ctl-deny-out" {
				t.Errorf("%s: %s, want ctl-deny-out", p, v)
			}
		}
	}
	// silod, in the gateway range, reaches the database endpoint.
	v := wantAllowed(t, e, TCP(vmGW, pscDB, 5432), true)
	if v.Egress.Handoff != "gw-out" {
		t.Errorf("silod to the database: %s, want after gw-out", v)
	}
}

func TestFenceDeepestFirst(t *testing.T) {
	e := newEval(t, baseTree(), allowAll(siloA)...)
	// a's intra rules match c's packets too; c's own rules come first.
	if v := wantAllowed(t, e, TCP(vmC1, vmA1, 22), false); v.Egress.Rule != "c-deny-out" {
		t.Errorf("c to a: %s, want c-deny-out", v)
	}
	if v := wantAllowed(t, e, TCP(vmA1, vmC1, 22), false); v.Ingress.Rule != "c-deny-in" {
		t.Errorf("a to c: %s, want c-deny-in", v)
	}
	e = newEval(t, baseTree(), allowAll(siloC)...)
	if v := wantAllowed(t, e, TCP(vmC1, vmC2, 22), true); v.Egress.Handoff != "c-intra-out" || v.Ingress.Handoff != "c-intra-in" {
		t.Errorf("within c: %s, want c's intra rules", v)
	}
	// The root's rules cover every silo's space, but are last.
	if v := wantAllowed(t, e, TCP(vmRoot, vmA1, 22), false); v.Egress.Handoff != "root-intra-out" || v.Ingress.Rule != "a-deny-in" {
		t.Errorf("root to a: %s", v)
	}
}

func TestEgressAtSenderIngressAtReceiver(t *testing.T) {
	ssh := pinned(siloA, "ssh", compute.Firewall{Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}})
	noOut := pinned(siloA, "no-out", compute.Firewall{Direction: "EGRESS", Priority: 100, Denied: []*compute.FirewallDenied{{IPProtocol: "tcp"}}, TargetTags: []string{"locked"}})
	e := newEval(t, baseTree(), ssh, noOut)
	locked := VM("10.0.1.11", "a", "a-locked")
	v := wantAllowed(t, e, TCP(locked, vmA2, 22), false)
	if v.Egress.Allowed || v.Egress.Rule != "a-no-out" || !v.Ingress.Allowed {
		t.Errorf("from a locked VM: %s, want denied at the sender only", v)
	}
	// The locked VM still receives: the egress rule targets the sender.
	wantAllowed(t, e, TCP(vmA2, locked, 22), true)
}

func TestTargetAndSourceTags(t *testing.T) {
	web := pinned(siloA, "web", compute.Firewall{TargetTags: []string{"web"}, Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"80", "8000-8999"}}}})
	db := pinned(siloA, "db", compute.Firewall{SourceTags: []string{"app"}, TargetTags: []string{"db"}, Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"5432"}}}})
	e := newEval(t, baseTree(), web, db)
	webVM := VM("10.0.1.20", "a", "a-web")
	appVM := VM("10.0.1.21", "a", "a-app")
	dbVM := VM("10.0.1.22", "a", "a-db")
	wantAllowed(t, e, TCP(vmA1, webVM, 80), true)
	wantAllowed(t, e, TCP(vmA1, webVM, 8500), true)
	wantAllowed(t, e, TCP(vmA1, webVM, 9000), false)
	wantAllowed(t, e, TCP(webVM, vmA1, 80), false)
	wantAllowed(t, e, TCP(appVM, dbVM, 5432), true)
	wantAllowed(t, e, TCP(webVM, dbVM, 5432), false)
	wantAllowed(t, e, TCP(appVM, webVM, 5432), false)
}

func TestVPCPriorityAndDenyWins(t *testing.T) {
	tcp22 := []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}
	deny22 := []*compute.FirewallDenied{{IPProtocol: "tcp", Ports: []string{"22"}}}
	for _, c := range []struct {
		name     string
		fws      []*compute.Firewall
		want     bool
		wantRule string
	}{
		{"deny wins a tie", []*compute.Firewall{
			pinned(siloA, "allow", compute.Firewall{Priority: 500, Allowed: tcp22}),
			pinned(siloA, "deny", compute.Firewall{Priority: 500, Denied: deny22}),
		}, false, "a-deny"},
		{"lower number first", []*compute.Firewall{
			pinned(siloA, "allow", compute.Firewall{Priority: 499, Allowed: tcp22}),
			pinned(siloA, "deny", compute.Firewall{Priority: 500, Denied: deny22}),
		}, true, "a-allow"},
		{"priority 0 is 1000 unless sent", []*compute.Firewall{
			pinned(siloA, "allow", compute.Firewall{Allowed: tcp22}),
			pinned(siloA, "deny", compute.Firewall{Priority: 1001, Denied: deny22}),
			pinned(siloA, "deny0", compute.Firewall{Priority: 0, Denied: deny22, Disabled: true}),
		}, true, "a-allow"},
		{"priority 0 sent", []*compute.Firewall{
			pinned(siloA, "allow", compute.Firewall{Allowed: tcp22}),
			pinned(siloA, "deny0", compute.Firewall{Priority: 0, Denied: deny22, ForceSendFields: []string{"Priority"}}),
		}, false, "a-deny0"},
		{"disabled rules are skipped", []*compute.Firewall{
			pinned(siloA, "allow", compute.Firewall{Allowed: tcp22}),
			pinned(siloA, "deny", compute.Firewall{Priority: 1, Denied: deny22, Disabled: true}),
		}, true, "a-allow"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEval(t, baseTree(), c.fws...)
			v := wantAllowed(t, e, TCP(vmA1, vmA2, 22), c.want)
			if v.Ingress.Rule != c.wantRule {
				t.Errorf("decided by %s, want %s", v.Ingress.Rule, c.wantRule)
			}
		})
	}
}

func TestEnforcementOrder(t *testing.T) {
	// A rule that would open b to everyone, were the VPC rules first.
	open := &compute.Firewall{Name: "open", Allowed: []*compute.FirewallAllowed{{IPProtocol: "all"}}}
	openOut := &compute.Firewall{Name: "open-out", Direction: "EGRESS", Allowed: []*compute.FirewallAllowed{{IPProtocol: "all"}}}
	pol := baseTree().policy()
	before, err := New("", pol, []*compute.Firewall{open, openOut})
	if err != nil {
		t.Fatal(err)
	}
	after, err := New(AfterClassic, pol, []*compute.Firewall{open, openOut})
	if err != nil {
		t.Fatal(err)
	}
	p := TCP(vmA1, vmB, 22)
	if before.Allowed(p) {
		t.Errorf("BEFORE_CLASSIC_FIREWALL: %s allowed", p)
	}
	if v := eval(t, after, p); !v.Allowed || v.Ingress.Stage != StageVPC {
		t.Errorf("AFTER_CLASSIC_FIREWALL: %s, want allowed by the VPC rules", v)
	}
	// After the VPC rules, a policy goto_next hands on to the implied
	// rules.
	after, err = New(AfterClassic, pol, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := eval(t, after, TCP(vmA1, vmA2, 22)); v.Allowed || v.Ingress.Rule != ImpliedDenyIngress || v.Ingress.Handoff != "a-intra-in" {
		t.Errorf("AFTER_CLASSIC_FIREWALL within a: %s", v)
	}
}

func TestImpliedRulesAndNoPolicy(t *testing.T) {
	e, err := New(BeforeClassic, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := eval(t, e, TCP(vmA1, internet, 443))
	if !v.Allowed || v.Egress.Rule != ImpliedAllowEgress || v.Ingress.Stage != "" {
		t.Errorf("to the internet: %s", v)
	}
	if v := eval(t, e, TCP(vmA1, vmA2, 22)); v.Allowed || v.Ingress.Rule != ImpliedDenyIngress || !v.Egress.Allowed {
		t.Errorf("VM to VM: %s", v)
	}
	if v := eval(t, e, TCP(internet, vmA1, 22)); v.Allowed || v.Egress.Stage != "" {
		t.Errorf("from the internet: %s", v)
	}
	// A VPC ingress rule with no source is 0.0.0.0/0: IPv4 only.
	noSource := &compute.Firewall{Name: "any", Allowed: []*compute.FirewallAllowed{{IPProtocol: "all"}}}
	e, err = New(BeforeClassic, nil, []*compute.Firewall{noSource})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Allowed(TCP(internet, vmA1, 22)) {
		t.Error("no-source rule doesn't admit IPv4")
	}
	if e.Allowed(TCP(Outside("2001:db8::1"), VM("fd20::1"), 22)) {
		t.Error("no-source rule admits IPv6")
	}
}

func TestDisabledPolicyRule(t *testing.T) {
	pol := baseTree().policy()
	for _, r := range pol.Rules {
		if r.RuleName == "ctl-deny-out" || r.RuleName == "a-deny-out" {
			r.Disabled = true
		}
	}
	e, err := New("", pol, nil)
	if err != nil {
		t.Fatal(err)
	}
	// With a-deny-out off, a's packets fall to root-intra-out and the
	// root's deny-out. The control range is still denied, by root-deny-out.
	if v := eval(t, e, TCP(vmA1, vmRoot, 22)); v.Egress.Handoff != "root-intra-out" {
		t.Errorf("a to root: %s", v)
	}
	if v := eval(t, e, TCP(vmA1, pscDB, 5432)); v.Allowed || v.Egress.Rule != "root-deny-out" {
		t.Errorf("a to the database: %s", v)
	}
}

func TestNewRefusesWhatItDoesntModel(t *testing.T) {
	ok := func() *compute.FirewallPolicyRule {
		return prule("r", 10, "INGRESS", Allow, anywhere, nil, l4cfg("tcp", "80"))
	}
	for name, mod := range map[string]func(r *compute.FirewallPolicyRule){
		"security profile action": func(r *compute.FirewallPolicyRule) { r.Action = "apply_security_profile_group" },
		"direction":               func(r *compute.FirewallPolicyRule) { r.Direction = "SIDEWAYS" },
		"target service accounts": func(r *compute.FirewallPolicyRule) { r.TargetServiceAccounts = []string{"x@y"} },
		"target secure tags": func(r *compute.FirewallPolicyRule) {
			r.TargetSecureTags = []*compute.FirewallPolicyRuleSecureTag{{Name: "tagValues/1"}}
		},
		"fqdns":        func(r *compute.FirewallPolicyRule) { r.Match.SrcFqdns = []string{"example.com"} },
		"region codes": func(r *compute.FirewallPolicyRule) { r.Match.SrcRegionCodes = []string{"US"} },
		"no layer4":    func(r *compute.FirewallPolicyRule) { r.Match.Layer4Configs = nil },
		"bad port": func(r *compute.FirewallPolicyRule) {
			r.Match.Layer4Configs = []*compute.FirewallPolicyRuleMatcherLayer4Config{l4cfg("tcp", "70000")}
		},
		"icmp port": func(r *compute.FirewallPolicyRule) {
			r.Match.Layer4Configs = []*compute.FirewallPolicyRuleMatcherLayer4Config{l4cfg("icmp", "1")}
		},
		"bad protocol": func(r *compute.FirewallPolicyRule) {
			r.Match.Layer4Configs = []*compute.FirewallPolicyRuleMatcherLayer4Config{l4cfg("gre")}
		},
		"bad range":     func(r *compute.FirewallPolicyRule) { r.Match.SrcIpRanges = []string{"10.0.0.0/33"} },
		"same priority": nil,
	} {
		t.Run(name, func(t *testing.T) {
			r := ok()
			rules := []*compute.FirewallPolicyRule{r}
			if mod != nil {
				mod(r)
			} else {
				rules = append(rules, ok())
			}
			if _, err := New("", &compute.FirewallPolicy{Rules: rules}, nil); err == nil {
				t.Error("New accepted it")
			}
		})
	}
	if _, err := New("", &compute.FirewallPolicy{Rules: []*compute.FirewallPolicyRule{ok()}}, nil); err != nil {
		t.Errorf("New refused a plain rule: %v", err)
	}
	if _, err := New("SOMETIMES", nil, nil); err == nil {
		t.Error("New accepted an unknown order")
	}
	all := []*compute.FirewallAllowed{{IPProtocol: "all"}}
	for name, fw := range map[string]*compute.Firewall{
		"service accounts":   {Name: "x", Allowed: all, SourceServiceAccounts: []string{"x@y"}},
		"allowed and denied": {Name: "x", Allowed: all, Denied: []*compute.FirewallDenied{{IPProtocol: "all"}}},
		"neither":            {Name: "x"},
		"egress source tags": {Name: "x", Direction: "EGRESS", Allowed: all, SourceTags: []string{"t"}},
		"priority":           {Name: "x", Allowed: all, Priority: 70000},
	} {
		if _, err := New("", nil, []*compute.Firewall{fw}); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}

func TestEvaluateRefusesMalformedPackets(t *testing.T) {
	e, err := New("", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Packet{
		{Src: vmA1, Dst: vmA2, Protocol: "all"},
		{Src: vmA1, Dst: vmA2, Protocol: "gre"},
		{Src: vmA1, Dst: VM("fd20::1"), Protocol: "tcp", Port: 1},
		{Src: vmA1, Dst: vmA2, Protocol: "tcp", Port: 70000},
		{Src: vmA1, Protocol: "tcp", Port: 1},
	} {
		if _, err := e.Evaluate(p); err == nil {
			t.Errorf("%s: no error", p)
		}
	}
}

func TestMinus(t *testing.T) {
	got := fmt.Sprint(minus("10.0.0.0/16", "10.0.255.0/24", "10.0.254.0/24"))
	want := "[10.0.0.0/17 10.0.128.0/18 10.0.192.0/19 10.0.224.0/20 10.0.240.0/21 10.0.248.0/22 10.0.252.0/23]"
	if got != want {
		t.Errorf("minus = %s, want %s", got, want)
	}
}
