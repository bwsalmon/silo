package fence

import (
	"fmt"
	"math/rand"
	"net/netip"
	"strings"
	"testing"

	fe "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/fake/fenceeval"
	compute "google.golang.org/api/compute/v1"
)

// The property tests of docs/silo/testing.md "Fake GCE backend": random
// trees, blocks, control policies, grants and tenant rules, compiled
// and decided by the fence evaluator.

const propertyRuns = 40

type probe struct {
	proto string
	port  int
}

var probes = []probe{
	{"tcp", 22}, {"tcp", 80}, {"tcp", 443}, {"tcp", 3128}, {"tcp", 5432}, {"tcp", 8080}, {"tcp", 8443},
	{"tcp", 19999}, {"tcp", 20000}, {"tcp", 25000}, {"tcp", 29999}, {"tcp", 30000},
	{"udp", 53}, {"udp", 8080}, {"icmp", 0}, {"47", 0}, {"esp", 0},
}

// gatewayPort says whether a silo may reach the gateway on p: TCP 80,
// 3128 and the export ports always, and 8080 unless sealed.
func gatewayPort(p probe, sealed bool) bool {
	if p.proto != "tcp" {
		return false
	}
	switch {
	case p.port == HTTPPort, p.port == ProxyPort, p.port >= 20000 && p.port <= 29999:
		return true
	case p.port == APIPort:
		return !sealed
	}
	return false
}

func compileTree(t *testing.T, tr *gtree, grants []Grant, cur []*compute.FirewallPolicyRule) []*compute.FirewallPolicyRule {
	t.Helper()
	in := tr.input()
	in.Grants = grants
	rules, err := Compile(in, DefaultConfig, cur)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func evaluator(t *testing.T, rules []*compute.FirewallPolicyRule, fws []*compute.Firewall) *fe.Evaluator {
	t.Helper()
	e, err := fe.New(fe.BeforeClassic, policyOf(rules), fws)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func verdict(t *testing.T, e *fe.Evaluator, p fe.Packet) fe.Verdict {
	t.Helper()
	v, err := e.Evaluate(p)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func packet(src, dst fe.Endpoint, p probe) fe.Packet {
	return fe.Packet{Src: src, Dst: dst, Protocol: p.proto, Port: p.port}
}

// TestPropertyIsolation checks, for random trees with every tenant
// allowing everything, that no packet crosses silos; that every packet
// within a silo is handed to the tenant's rules by that silo's own
// intra rules; that every silo reaches the gateway on its ports and on
// nothing else, the API port only when not sealed (so a sealed silo and
// everything below it can't reach 8080); and that nothing else outside
// is reachable.
func TestPropertyIsolation(t *testing.T) {
	for run := 0; run < propertyRuns; run++ {
		r := rand.New(rand.NewSource(int64(run)))
		tr := randomTree(r, 1+r.Intn(8))
		rules := compileTree(t, tr, nil, nil)
		open := evaluator(t, rules, tr.tenantRules())
		closed := evaluator(t, rules, nil)
		vms := tr.vms(r)
		gw := fe.VM(genGWVM.String())
		outside := []fe.Endpoint{fe.Outside("8.8.8.8"), fe.Outside("192.168.0.1"), fe.Outside("10.1.0.1"), fe.Outside(genPSC.String()), fe.Outside("35.235.240.7")}
		for sid, addrs := range vms {
			for _, a := range addrs {
				src := fe.Endpoint{Addr: a, Tags: []string{sid}}
				if got := tr.owner(a); got != sid {
					t.Fatalf("run %d: %s is %s's, want %s's", run, a, got, sid)
				}
				for did, daddrs := range vms {
					for _, b := range daddrs {
						if a == b {
							continue
						}
						dst := fe.Endpoint{Addr: b, Tags: []string{did}}
						for _, p := range probes {
							pk := packet(src, dst, p)
							v := verdict(t, open, pk)
							if sid != did {
								if v.Allowed {
									t.Fatalf("run %d: %s crosses from %s to %s: %s", run, pk, sid, did, v)
								}
								continue
							}
							if !v.Allowed || v.Egress.Stage != fe.StageVPC || v.Ingress.Stage != fe.StageVPC ||
								v.Egress.Handoff != sid+"-intra-out" || v.Ingress.Handoff != sid+"-intra-in" {
								t.Fatalf("run %d: %s within %s isn't the tenant's: %s", run, pk, sid, v)
							}
							if v := verdict(t, closed, pk); v.Allowed || v.Ingress.Stage != fe.StageImplied || v.Ingress.Handoff != sid+"-intra-in" {
								t.Fatalf("run %d: %s within %s with no tenant rules: %s", run, pk, sid, v)
							}
						}
					}
				}
				for _, p := range probes {
					pk := packet(src, gw, p)
					want := gatewayPort(p, tr.sealed(sid))
					if v := verdict(t, open, pk); v.Allowed != want {
						t.Fatalf("run %d: %s from %s (sealed %v): %s, want allowed=%v", run, pk, sid, tr.sealed(sid), v, want)
					}
					for _, o := range outside {
						pk := packet(src, o, p)
						if v := verdict(t, open, pk); v.Allowed {
							t.Fatalf("run %d: %s from %s reaches outside: %s", run, pk, sid, v)
						}
						if v := verdict(t, open, packet(o, src, p)); v.Allowed {
							t.Fatalf("run %d: %s reaches %s from outside: %s", run, packet(o, src, p), sid, v)
						}
					}
				}
			}
		}
		// The gateway is reached from IAP on the front door only.
		iap := fe.Outside("35.235.240.7")
		for _, p := range probes {
			want := p.proto == "tcp" && p.port == DefaultConfig.FrontDoorPort
			if v := verdict(t, open, packet(iap, gw, p)); v.Allowed != want {
				t.Fatalf("run %d: %s: %s, want allowed=%v", run, packet(iap, gw, p), v, want)
			}
		}
		// silod, in the gateway range, reaches the database endpoint.
		if v := verdict(t, closed, fe.TCP(gw, fe.Outside(genPSC.String()), 5432)); !v.Allowed {
			t.Fatalf("run %d: the gateway can't reach the control range: %s", run, v)
		}
	}
}

// TestPropertyControlRange checks that no silo reaches the control
// range on any protocol or port, whatever grants and tenant rules exist:
// ctl-deny-out decides first.
func TestPropertyControlRange(t *testing.T) {
	for run := 0; run < propertyRuns; run++ {
		r := rand.New(rand.NewSource(int64(1000 + run)))
		tr := randomTree(r, 1+r.Intn(8))
		rules := compileTree(t, tr, tr.hostileGrants(r), nil)
		e := evaluator(t, rules, tr.tenantRules())
		psc := []fe.Endpoint{fe.Outside(genPSC.String()), fe.Outside(genControl.Addr().String()), fe.Outside(lastAddr(genControl).String()),
			fe.VM(genPSC.String())}
		for sid, addrs := range tr.vms(r) {
			for _, a := range addrs {
				src := fe.Endpoint{Addr: a, Tags: []string{sid}}
				for _, dst := range psc {
					for _, p := range append(probes, probe{"tcp", 5432}, probe{"tcp", 3307}, probe{"udp", 5432}, probe{"sctp", 1}) {
						pk := packet(src, dst, p)
						v := verdict(t, e, pk)
						if v.Allowed || v.Egress.Rule != RuleControlDeny {
							t.Fatalf("run %d: %s from %s: %s, want denied by %s", run, pk, sid, v, RuleControlDeny)
						}
					}
				}
			}
		}
	}
}

// mutation is one change to a tree, and what the property check must
// know about it: the ranges that change hands (which hold nothing while
// the change is applied: a new block is fresh, a released one empty),
// and the silos whose sealing changes.
type mutation struct {
	name     string
	after    *gtree
	handed   []netip.Prefix
	resealed map[string]bool
}

func mutate(r *rand.Rand, tr *gtree) mutation {
	for {
		m := mutation{after: tr.clone(), resealed: map[string]bool{}}
		ids := m.after.ids()
		switch r.Intn(6) {
		case 0: // create a silo
			m.name = "create"
			if id := m.after.addChild(r, ids[r.Intn(len(ids))]); id != "" {
				m.handed = m.after.silos[id].blocks
				return m
			}
		case 1: // delete a leaf
			m.name = "delete"
			var leaves []string
			for _, id := range ids {
				if id != "root" && len(m.after.children(id)) == 0 {
					leaves = append(leaves, id)
				}
			}
			if len(leaves) > 0 {
				id := leaves[r.Intn(len(leaves))]
				m.handed = m.after.silos[id].blocks
				delete(m.after.silos, id)
				return m
			}
		case 2: // add a block
			m.name = "add block"
			id := ids[r.Intn(len(ids))]
			if s := m.after.silos[id]; id != "root" {
				if b, ok := m.after.carve(r, s.parent, minInt(28, s.depthBits()+r.Intn(3))); ok {
					s.blocks = append(s.blocks, b)
					m.handed = []netip.Prefix{b}
					return m
				}
			}
		case 3: // release a leaf's block, when it has two
			m.name = "release block"
			id := ids[r.Intn(len(ids))]
			if s := m.after.silos[id]; id != "root" && len(s.blocks) > 1 && len(m.after.children(id)) == 0 {
				i := r.Intn(len(s.blocks))
				m.handed = []netip.Prefix{s.blocks[i]}
				s.blocks = append(s.blocks[:i:i], s.blocks[i+1:]...)
				return m
			}
		case 4: // widen a leaf's block in place
			m.name = "widen block"
			id := ids[r.Intn(len(ids))]
			if s := m.after.silos[id]; id != "root" && len(m.after.children(id)) == 0 {
				i := r.Intn(len(s.blocks))
				b := s.blocks[i]
				wide := netip.PrefixFrom(b.Addr(), b.Bits()-1).Masked()
				// The other half must be free in the parent.
				other := Minus(wide, b)
				free := m.after.free(s.parent)
				if len(intersect(other, free)) == 1 && intersect(other, free)[0] == other[0] {
					s.blocks[i] = wide
					m.handed = other
					return m
				}
			}
		case 5: // change a control policy
			m.name = "set control"
			id := ids[r.Intn(len(ids))]
			if id != "root" {
				before := map[string]bool{}
				for _, x := range ids {
					before[x] = tr.sealed(x)
				}
				m.after.silos[id].control = randControl(r)
				for _, x := range ids {
					if m.after.sealed(x) != before[x] {
						m.resealed[x] = true
					}
				}
				return m
			}
		}
	}
}

// TestPropertyIntermediateStates checks that every intermediate state of
// applying a change is no looser than both its start and its end, for
// any tenant rules (fenceeval.CheckSteps), and that the steps end at
// the compiled policy. A packet to or from space changing hands is not
// tried, since that space holds nothing while the change is applied;
// nor is the API port from a silo being sealed or unsealed, which the
// gateway's own refusal covers while the steps run.
func TestPropertyIntermediateStates(t *testing.T) {
	for run := 0; run < 3*propertyRuns; run++ {
		r := rand.New(rand.NewSource(int64(2000 + run)))
		tr := randomTree(r, 1+r.Intn(5))
		before := compileTree(t, tr, nil, nil)
		m := mutate(r, tr)
		// A few changes at once, as an apply catching up finds them.
		for i := r.Intn(3); i > 0; i-- {
			m2 := mutate(r, m.after)
			m.name += ", " + m2.name
			m.after = m2.after
			m.handed = append(m.handed, m2.handed...)
		}
		// Which silos are sealed or unsealed is worked out against the
		// start, across all the changes.
		m.resealed = map[string]bool{}
		for _, id := range m.after.ids() {
			if tr.silos[id] != nil && tr.sealed(id) != m.after.sealed(id) {
				m.resealed[id] = true
			}
		}
		after := compileTree(t, m.after, nil, before)
		steps := Plan(before, after)
		// Steps end at after.
		got := withoutDefaults(before)
		for _, s := range steps {
			got = applyStep(got, s)
		}
		if len(got) != len(after) {
			t.Fatalf("run %d (%s): steps end with %d rules, want %d", run, m.name, len(got), len(after))
		}
		for i := range got {
			if !Same(got[i], after[i]) {
				t.Fatalf("run %d (%s): steps end with %s at %d, want %s", run, m.name, got[i].RuleName, got[i].Priority, after[i].RuleName)
			}
		}
		// Nothing that stays moves.
		kept := map[string]int64{}
		for _, x := range before {
			kept[x.RuleName] = x.Priority
		}
		for _, x := range after {
			if p, ok := kept[x.RuleName]; ok && p != x.Priority {
				t.Fatalf("run %d (%s): %s moved from %d to %d", run, m.name, x.RuleName, p, x.Priority)
			}
		}
		var vmOnly []netip.Prefix
		for _, g := range []*gtree{tr, m.after} {
			for _, id := range g.ids() {
				vmOnly = append(vmOnly, g.ranges(id)...)
			}
		}
		resealed := m.resealed
		owners := []*gtree{tr, m.after}
		opts := fe.CheckOptions{
			VMOnly: normalize(vmOnly),
			Empty:  normalize(m.handed),
			Exempt: func(p fe.Packet) bool {
				if p.Protocol != "tcp" || p.Port != APIPort || !genGateway.Contains(p.Dst.Addr) {
					return false
				}
				for _, g := range owners {
					if resealed[g.owner(p.Src.Addr)] {
						return true
					}
				}
				return false
			},
		}
		if err := fe.CheckSteps(policyOf(before), evalSteps(steps), opts); err != nil {
			t.Fatalf("run %d (%s): %v\nsteps: %v", run, m.name, err, steps)
		}
	}
}

// TestPropertyDeepChain runs the isolation checks on a chain at the
// depth cap, where every band is used.
func TestPropertyDeepChain(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	tr := newTree()
	parent := "root"
	for d := 1; d <= MaxDepth; d++ {
		p := tr.silos[parent]
		b, ok := tr.carve(r, parent, minInt(28, p.depthBits()+1))
		if !ok {
			t.Fatalf("no room at depth %d", d)
		}
		tr.next++
		id := "d" + strings.Repeat("x", d)
		tr.silos[id] = &gsilo{id: id, parent: parent, depth: d, blocks: []netip.Prefix{b}, control: randControl(r)}
		tr.order = append(tr.order, id)
		parent = id
	}
	rules := compileTree(t, tr, nil, nil)
	e := evaluator(t, rules, tr.tenantRules())
	gw := fe.VM(genGWVM.String())
	vms := tr.vms(r)
	for sid, addrs := range vms {
		for _, a := range addrs {
			src := fe.Endpoint{Addr: a, Tags: []string{sid}}
			for did, daddrs := range vms {
				if did == sid {
					continue
				}
				if v := verdict(t, e, fe.TCP(src, fe.Endpoint{Addr: daddrs[0], Tags: []string{did}}, 22)); v.Allowed {
					t.Fatalf("%s reaches %s: %s", sid, did, v)
				}
			}
			for _, p := range probes {
				if v := verdict(t, e, packet(src, gw, p)); v.Allowed != gatewayPort(p, tr.sealed(sid)) {
					t.Fatalf("%s (sealed %v) to the gateway %s: %s", sid, tr.sealed(sid), packet(src, gw, p), v)
				}
			}
		}
	}
}

// TestPropertyExports adds random exports to random trees, each with an
// export-<x>-in rule as ExportGrants writes it, and checks that the
// gateway reaches each export's ip:port and nothing else of the
// exporter's; that no silo, importer or not, reaches any other silo
// directly, whatever its tenant rules say; and that every silo still
// reaches the gateway on its ports and nothing else.
func TestPropertyExports(t *testing.T) {
	for run := 0; run < propertyRuns; run++ {
		r := rand.New(rand.NewSource(int64(3000 + run)))
		tr := randomTree(r, 2+r.Intn(7))
		vms := tr.vms(r)
		type export struct {
			silo   string
			target netip.AddrPort
		}
		var exps []export
		var grants []Grant
		for i, sid := range tr.ids() {
			if len(vms[sid]) == 0 || r.Intn(3) == 0 {
				continue
			}
			p := probes[r.Intn(12)] // a TCP probe
			ex := export{sid, netip.AddrPortFrom(vms[sid][r.Intn(len(vms[sid]))], uint16(p.port))}
			exps = append(exps, ex)
			grants = append(grants, ExportGrant(fmt.Sprintf("export-x%d-in", i), genGateway, ex.target))
		}
		rules := compileTree(t, tr, grants, nil)
		open := evaluator(t, rules, tr.tenantRules())
		closed := evaluator(t, rules, nil)
		gw := fe.VM(genGWVM.String())
		for _, ex := range exps {
			dst := fe.Endpoint{Addr: ex.target.Addr(), Tags: []string{ex.silo}}
			for _, p := range probes {
				want := p.proto == "tcp" && p.port == int(ex.target.Port())
				for _, e := range []*fe.Evaluator{open, closed} {
					if v := verdict(t, e, packet(gw, dst, p)); v.Allowed != want {
						t.Fatalf("run %d: the gateway to %s's export %s: %s: %s, want allowed=%v", run, ex.silo, ex.target, packet(gw, dst, p), v, want)
					}
				}
			}
			// Nothing else of the exporter's is reachable from the gateway.
			for _, a := range vms[ex.silo] {
				if a == ex.target.Addr() {
					continue
				}
				pk := packet(gw, fe.Endpoint{Addr: a, Tags: []string{ex.silo}}, probe{"tcp", int(ex.target.Port())})
				if v := verdict(t, closed, pk); v.Allowed {
					t.Fatalf("run %d: %s reaches another VM of the exporter's: %s", run, pk, v)
				}
			}
		}
		for sid, addrs := range vms {
			for _, a := range addrs {
				src := fe.Endpoint{Addr: a, Tags: []string{sid}}
				for _, ex := range exps {
					if ex.silo == sid {
						continue
					}
					dst := fe.Endpoint{Addr: ex.target.Addr(), Tags: []string{ex.silo}}
					for _, p := range probes {
						if v := verdict(t, open, packet(src, dst, p)); v.Allowed {
							t.Fatalf("run %d: %s reaches %s's export directly: %s", run, packet(src, dst, p), ex.silo, v)
						}
					}
				}
				for _, p := range probes {
					want := gatewayPort(p, tr.sealed(sid))
					if v := verdict(t, open, packet(src, gw, p)); v.Allowed != want {
						t.Fatalf("run %d: %s from %s: %s, want allowed=%v", run, packet(src, gw, p), sid, v, want)
					}
				}
			}
		}
	}
}

// glink is a random link between two silos of a generated tree.
type glink struct {
	a, b             string
	forward, reverse bool // a may start connections to b, b to a
	l4               []L4
}

// opens reports whether l lets a connection on p start in src to dst.
func (l glink) opens(src, dst string, p probe) bool {
	switch {
	case src == l.a && dst == l.b && l.forward, src == l.b && dst == l.a && l.reverse:
	default:
		return false
	}
	for _, c := range l.l4 {
		if c.Protocol == "all" {
			return true
		}
		if c.Protocol != p.proto {
			continue
		}
		if len(c.Ports) == 0 {
			return true
		}
		for _, s := range c.Ports {
			lo, hi, _ := parsePortRange(s)
			if p.port >= lo && p.port <= hi {
				return true
			}
		}
	}
	return false
}

var linkRules = [][]L4{
	{{Protocol: "all"}},
	{{Protocol: "tcp", Ports: []string{"5432"}}},
	{{Protocol: "tcp", Ports: []string{"22", "8000-8443"}}, {Protocol: "udp", Ports: []string{"53"}}},
	{{Protocol: "icmp"}},
	{{Protocol: "tcp"}},
	{{Protocol: "esp"}, {Protocol: "udp"}},
}

// TestPropertyLinks adds random ACTIVE links to random trees, between
// any two silos (an ancestor and its descendant included), each with
// the rules LinkGrant writes between the two sides' own spaces as
// OwnSpaces works them out. With every tenant allowing everything, a
// packet crosses from one silo to another exactly when a link opens
// that direction for its protocol and port; a descendant carved out of
// a linked silo is never reached through its ancestor's link; nothing
// reaches the control range; and every silo still reaches the gateway
// on its ports and nothing else.
func TestPropertyLinks(t *testing.T) {
	opened := 0
	for run := 0; run < propertyRuns; run++ {
		r := rand.New(rand.NewSource(int64(4000 + run)))
		tr := randomTree(r, 2+r.Intn(7))
		blocks := map[string][]netip.Prefix{"system:gateway": {genGateway}, "system:control": {genControl}}
		for _, id := range tr.ids() {
			blocks[id] = tr.silos[id].blocks
		}
		own := OwnSpaces(blocks, genGateway, genControl)
		for _, id := range tr.ids() {
			if got, want := normalize(own[id]), tr.free(id); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("run %d: %s's own space %v, want %v", run, id, got, want)
			}
		}
		ids := tr.ids()
		var ls []glink
		var grants []Grant
		for i := r.Intn(2 * len(ids)); i >= 0; i-- {
			a, b := ids[r.Intn(len(ids))], ids[r.Intn(len(ids))]
			if a == b {
				continue
			}
			l := glink{a: a, b: b, l4: linkRules[r.Intn(len(linkRules))]}
			switch r.Intn(3) {
			case 0:
				l.forward = true
			case 1:
				l.reverse = true
			default:
				l.forward, l.reverse = true, true
			}
			ls = append(ls, l)
			grants = append(grants, LinkGrant(fmt.Sprintf("l%d", i), own[a], own[b], l.forward, l.reverse, l.l4)...)
		}
		rules := compileTree(t, tr, grants, nil)
		e := evaluator(t, rules, tr.tenantRules())
		vms := tr.vms(r)
		gw := fe.VM(genGWVM.String())
		ctl := []fe.Endpoint{fe.Outside(genPSC.String()), fe.VM(genPSC.String()), fe.Outside(genControl.Addr().String())}
		for sid, addrs := range vms {
			for _, a := range addrs {
				src := fe.Endpoint{Addr: a, Tags: []string{sid}}
				for did, daddrs := range vms {
					if did == sid {
						continue
					}
					for _, bAddr := range daddrs {
						dst := fe.Endpoint{Addr: bAddr, Tags: []string{did}}
						for _, p := range probes {
							want := false
							for _, l := range ls {
								want = want || l.opens(sid, did, p)
							}
							pk := packet(src, dst, p)
							if v := verdict(t, e, pk); v.Allowed != want {
								t.Fatalf("run %d: %s from %s to %s: %s, want allowed=%v (links %+v)", run, pk, sid, did, v, want, ls)
							}
							if want {
								opened++
							}
						}
					}
				}
				for _, p := range probes {
					if v := verdict(t, e, packet(src, gw, p)); v.Allowed != gatewayPort(p, tr.sealed(sid)) {
						t.Fatalf("run %d: %s from %s: %s", run, packet(src, gw, p), sid, v)
					}
					for _, d := range ctl {
						if v := verdict(t, e, packet(src, d, p)); v.Allowed {
							t.Fatalf("run %d: %s from %s reaches the control range: %s", run, packet(src, d, p), sid, v)
						}
					}
				}
			}
		}
	}
	if opened == 0 {
		t.Fatal("no run opened any path: the links checked nothing")
	}
}
