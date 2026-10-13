package netfence

import (
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fence"
)

// world is a random fence.Input and topology, built the way the ledger
// builds them: a root over the address space less the gateway and
// control ranges, children carved from their parents' blocks, networks
// carved from their silo's own space, and ports on hosts of those.
type world struct {
	in   *fence.Input
	topo *Topology
	cfg  Config
}

func genWorld(r *rand.Rand) world {
	in := &fence.Input{Gateway: pfx("10.200.255.0/24"), Control: pfx("10.200.254.0/24")}
	root := pfx("10.200.0.0/16")
	in.Root = fence.Minus(root, in.Gateway, in.Control)
	in.Silos = append(in.Silos, fence.Silo{ID: "root", Blocks: []netip.Prefix{root}, Sealed: r.Intn(4) == 0})
	// free is what each silo has not delegated, as prefixes.
	free := map[string][]netip.Prefix{"root": fence.Minus(root, in.Gateway, in.Control)}
	carve := func(holder string, bits int) (netip.Prefix, bool) {
		ps := free[holder]
		r.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
		for i, p := range ps {
			if p.Bits() > bits {
				continue
			}
			c := netip.PrefixFrom(p.Addr(), bits)
			free[holder] = append(append([]netip.Prefix(nil), ps[:i]...), ps[i+1:]...)
			free[holder] = append(free[holder], fence.Minus(p, c)...)
			return c, true
		}
		return netip.Prefix{}, false
	}
	n := 1 + r.Intn(6)
	for i := 0; i < n; i++ {
		parent := in.Silos[r.Intn(len(in.Silos))]
		b, ok := carve(parent.ID, parent.Depth*2+20+r.Intn(3))
		if !ok {
			continue
		}
		id := fmt.Sprintf("s%d", i)
		in.Silos = append(in.Silos, fence.Silo{ID: id, Depth: parent.Depth + 1, Blocks: []netip.Prefix{b}, Sealed: r.Intn(3) == 0})
		free[id] = []netip.Prefix{b}
	}
	topo := &Topology{API: addr("10.255.255.1"), Docker: addr("10.255.255.2")}
	if r.Intn(5) == 0 {
		topo.Docker = netip.Addr{}
	}
	for i := 0; i < 1+r.Intn(6); i++ {
		s := in.Silos[r.Intn(len(in.Silos))]
		sub, ok := carve(s.ID, 26+r.Intn(3))
		if !ok {
			continue
		}
		id := fmt.Sprintf("n%d", i)
		topo.Networks = append(topo.Networks, Network{ID: id, Silo: s.ID, Bridge: "sb-" + id, Subnet: sub, Internal: r.Intn(4) == 0})
		for j := 0; j < r.Intn(4); j++ {
			a := sub.Addr()
			for k := 0; k <= 1+j; k++ {
				a = a.Next()
			}
			b := a.As4()
			topo.Ports = append(topo.Ports, Port{Name: fmt.Sprintf("v%d-%d", i, j), Network: id,
				MAC: net.HardwareAddr{0x02, 0x00, b[0], b[1], b[2], b[3]}, Addr: a})
		}
	}
	protos := []string{"tcp", "udp", "icmp", "sctp", "esp", "all"}
	for i := 0; i < r.Intn(4) && len(in.Silos) > 1; i++ {
		a, b := in.Silos[r.Intn(len(in.Silos))], in.Silos[r.Intn(len(in.Silos))]
		if a.ID == b.ID {
			continue
		}
		var l4 []fence.L4
		for _, p := range protos {
			if r.Intn(3) != 0 {
				continue
			}
			var ports []string
			if hasPorts[p] {
				for k := 0; k < r.Intn(3); k++ {
					lo := 1 + r.Intn(9000)
					if r.Intn(2) == 0 {
						ports = append(ports, strconv.Itoa(lo))
					} else {
						ports = append(ports, fmt.Sprintf("%d-%d", lo, lo+r.Intn(200)))
					}
				}
			}
			l4 = append(l4, fence.L4{Protocol: p, Ports: ports})
		}
		in.Grants = append(in.Grants, fence.LinkGrant(fmt.Sprintf("l%d", i), a.Blocks, b.Blocks, r.Intn(3) != 0, r.Intn(2) == 0, l4)...)
	}
	if len(topo.Ports) > 0 && r.Intn(2) == 0 {
		p := topo.Ports[r.Intn(len(topo.Ports))]
		in.Grants = append(in.Grants, fence.ExportGrant("export-e-in", in.Gateway, netip.AddrPortFrom(p.Addr, 8000)))
	}
	return world{in: in, topo: topo}
}

// intent is what §9 says the fence does, read from the Input and the
// topology rather than from the compiled sets.
func (w world) intent(family, hook string, p Packet) string {
	verdict := map[bool]string{true: "accept", false: "drop"}
	if family == "bridge" {
		for _, pt := range w.topo.Ports {
			if pt.Name != p.IIF {
				continue
			}
			m := pt.MAC.String()
			switch p.EtherType {
			case "ip":
				return verdict[p.EtherSrc == m && p.Src == pt.Addr]
			case "arp":
				return verdict[p.EtherSrc == m && p.ArpSHA == m && p.Src == pt.Addr]
			}
		}
		return "drop"
	}
	ip := p.EtherType == "" || p.EtherType == "ip"
	switch hook {
	case "prerouting":
		if p.IIF == "lo" {
			return "accept"
		}
		n := w.network(p.IIF)
		return verdict[ip && n != nil && n.Subnet.Contains(p.Src)]
	case "input":
		if p.IIF == "lo" {
			return "accept"
		}
		n := w.network(p.IIF)
		if !ip || n == nil || n.Internal || !w.tenant(p.Src) {
			return "drop"
		}
		tcp, udp := p.Proto == 6, p.Proto == 17
		gw := (p.Dst == w.topo.API || p.Dst == w.topo.Docker) && p.Dst.IsValid()
		ep := DefaultExportPorts
		switch {
		case gw && (tcp || udp) && p.DPort == PortDNS:
			return "accept"
		case gw && tcp && (p.DPort == PortHTTP || p.DPort == PortEgress || (ep[0] <= p.DPort && p.DPort <= ep[1])):
			return "accept"
		case w.topo.Docker.IsValid() && p.Dst == w.topo.Docker && (tcp || udp) && p.DPort != PortDNS && p.DPort != PortDocker:
			return "accept"
		}
		if o := w.owner(p.Src); o == nil || o.Sealed || !tcp {
			return "drop"
		}
		return verdict[(w.topo.API.IsValid() && p.Dst == w.topo.API && p.DPort == PortAPI) ||
			(w.topo.Docker.IsValid() && p.Dst == w.topo.Docker && p.DPort == PortDocker)]
	case "forward":
		if p.CtState == "established" || p.CtState == "related" {
			return "accept"
		}
		if !ip {
			return "drop"
		}
		for _, g := range w.in.Grants {
			if g.Direction == "INGRESS" && !strings.HasPrefix(g.Name, "export-") && in(g.Src, p.Src) && in(g.Dst, p.Dst) && l4Allows(g.L4, p) {
				return "accept"
			}
		}
		return "drop"
	case "output":
		if p.OIF == "lo" {
			return "accept"
		}
		gw := p.Src.IsValid() && (p.Src == w.topo.API || p.Src == w.topo.Docker)
		return verdict[ip && gw && w.tenant(p.Dst)]
	}
	return "accept"
}

func (w world) network(bridge string) *Network {
	for i, n := range w.topo.Networks {
		if n.Bridge == bridge {
			return &w.topo.Networks[i]
		}
	}
	return nil
}

func (w world) tenant(a netip.Addr) bool {
	return w.owner(a) != nil && !w.in.Gateway.Contains(a) && !w.in.Control.Contains(a)
}

// owner is the silo whose block holds a most narrowly.
func (w world) owner(a netip.Addr) *fence.Silo {
	var best *fence.Silo
	bits := -1
	for i, s := range w.in.Silos {
		for _, b := range s.Blocks {
			if b.Contains(a) && b.Bits() > bits {
				best, bits = &w.in.Silos[i], b.Bits()
			}
		}
	}
	return best
}

func in(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func l4Allows(l4 []fence.L4, p Packet) bool {
	for _, l := range l4 {
		if l.Protocol == "all" {
			return true
		}
		if protoNumbers[l.Protocol] != int(p.Proto) {
			continue
		}
		if len(l.Ports) == 0 || !hasPorts[l.Protocol] {
			return true
		}
		for _, ps := range l.Ports {
			lo, hi, _ := portRange(ps)
			if lo <= int(p.DPort) && int(p.DPort) <= hi {
				return true
			}
		}
	}
	return false
}

// genPacket picks a packet that is likely to hit an edge: the world's
// own addresses, bridges, ports and grant ports, and some that are none
// of them.
func (w world) genPacket(r *rand.Rand) (family, hook string, p Packet) {
	var addrs []netip.Addr
	for _, s := range w.in.Silos {
		for _, b := range s.Blocks {
			addrs = append(addrs, b.Addr(), b.Addr().Next(), fence.LastAddr(b))
		}
	}
	var ifs []string
	for _, n := range w.topo.Networks {
		ifs = append(ifs, n.Bridge)
		addrs = append(addrs, n.Subnet.Addr().Next().Next(), fence.LastAddr(n.Subnet).Next())
	}
	for _, pt := range w.topo.Ports {
		ifs = append(ifs, pt.Name)
		addrs = append(addrs, pt.Addr)
	}
	ifs = append(ifs, "lo", "gw0", "eth0")
	addrs = append(addrs, addr("10.255.255.1"), addr("10.255.255.2"), addr("10.200.255.7"), addr("10.200.254.1"),
		addr("192.168.1.1"), addr("169.254.169.254"))
	ports := []uint16{0, 22, 53, 80, 443, 2375, 3128, 8000, 8080, 19999, 20000, 25000, 29999, 30000}
	for _, g := range w.in.Grants {
		for _, l := range g.L4 {
			for _, ps := range l.Ports {
				lo, hi, _ := portRange(ps)
				ports = append(ports, uint16(lo), uint16(hi), uint16(hi+1), uint16(lo-1))
			}
		}
	}
	pick := func() netip.Addr { return addrs[r.Intn(len(addrs))] }
	protos := []uint8{1, 6, 6, 17, 17, 47, 50, 132}
	p = Packet{IIF: ifs[r.Intn(len(ifs))], OIF: ifs[r.Intn(len(ifs))], Src: pick(), Dst: pick(),
		Proto: protos[r.Intn(len(protos))], DPort: ports[r.Intn(len(ports))]}
	if r.Intn(8) == 0 {
		p.CtState = []string{"established", "related"}[r.Intn(2)]
	}
	if r.Intn(4) == 0 {
		family, hook = "bridge", "prerouting"
		p.EtherType = []string{"ip", "ip", "arp", "arp", "ip6", "vlan"}[r.Intn(6)]
		var macs []string
		for _, pt := range w.topo.Ports {
			macs = append(macs, pt.MAC.String())
		}
		macs = append(macs, "02:00:00:00:00:01")
		p.EtherSrc, p.ArpSHA = macs[r.Intn(len(macs))], macs[r.Intn(len(macs))]
		if r.Intn(2) == 0 {
			p.ArpSHA = p.EtherSrc
		}
		return family, hook, p
	}
	if r.Intn(16) == 0 {
		p.EtherType = "ip6"
	}
	return "inet", []string{"prerouting", "input", "forward", "output"}[r.Intn(4)], p
}

// TestEvaluatorMatchesIntent is the property test (§15 step 5): over
// random trees, networks, pins and links, the evaluator's verdict on the
// compiled rules is the one §9 gives.
func TestEvaluatorMatchesIntent(t *testing.T) {
	seed := int64(1)
	if s := os.Getenv("NETFENCE_SEED"); s != "" {
		seed, _ = strconv.ParseInt(s, 10, 64)
	}
	r := rand.New(rand.NewSource(seed))
	worlds, packets := 300, 400
	if testing.Short() {
		worlds = 50
	}
	for i := 0; i < worlds; i++ {
		w := genWorld(r)
		rs, err := Compile(w.in, w.topo, w.cfg)
		if err != nil {
			t.Fatalf("world %d: %v", i, err)
		}
		for j := 0; j < packets; j++ {
			family, hook, p := w.genPacket(r)
			got, rule := rs.Eval(family, hook, p)
			if want := w.intent(family, hook, p); got != want {
				desc := "policy"
				if rule >= 0 {
					desc = rs.Table(family).Chain(hook).Rules[rule].String()
				}
				t.Fatalf("world %d, %s %s %+v: %s (by %s), want %s\n%s", i, family, hook, p, got, desc, want, rs.Render())
			}
		}
	}
}

// TestRandomRulesetsLoad loads a sample of the random worlds' rulesets
// with the real nft, so that the property test's rulesets are ones the
// kernel takes.
func TestRandomRulesetsLoad(t *testing.T) {
	nft := nftBinary(t)
	unshareNFT(t, nft)
	r := rand.New(rand.NewSource(2))
	var script strings.Builder
	dir := t.TempDir()
	for i := 0; i < 60; i++ {
		w := genWorld(r)
		rs, err := Compile(w.in, w.topo, w.cfg)
		if err != nil {
			t.Fatal(err)
		}
		f := fmt.Sprintf("%s/%d.nft", dir, i)
		if err := os.WriteFile(f, []byte(rs.Render()), 0o600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&script, "%s -f %s || { echo %s; exit 1; }\n", nft, f, f)
	}
	out, err := exec.Command("unshare", "-rn", "sh", "-c", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
