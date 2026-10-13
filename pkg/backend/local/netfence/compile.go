package netfence

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"

	"github.com/bwsalmon/silo/pkg/fence"
)

// The table and set names silod owns in silo-net.
const (
	TableName = "silo"

	SetNetSrc     = "net_src"     // ifname . ipv4_addr: each bridge and its network's subnet
	SetTenants    = "tenants"     // ipv4_addr: every silo's blocks
	SetAPIOK      = "api_ok"      // ipv4_addr: the own spaces of silos that aren't sealed
	SetExternal   = "external"    // ifname: bridges of networks that aren't Internal
	SetGateway    = "gateway"     // ipv4_addr: gw0's addresses
	SetGrantPorts = "grant_ports" // ipv4_addr . inet_proto . inet_service . ipv4_addr
	SetGrantAny   = "grant_any"   // ipv4_addr . inet_proto . ipv4_addr
	SetPins       = "pins"        // ifname . ether_addr . ipv4_addr, in bridge silo
)

// The gateway's ports (§9 "Rules").
const (
	PortDNS    = 53
	PortHTTP   = 80
	PortEgress = 3128
	PortAPI    = 8080 // on silo.internal
	PortDocker = 2375 // on docker.silo.internal
)

// Network is one Docker network: a bridge in silo-net, owned by one
// silo, whose subnet was carved from that silo's blocks.
type Network struct {
	ID       string
	Silo     string
	Bridge   string // sb-<network id>
	Subnet   netip.Prefix
	Internal bool
}

// Port is one container's veth on a network's bridge, pinned to the MAC
// and IPv4 address silod chose for it.
type Port struct {
	Name    string // the host end, on the bridge
	Network string // the Network's ID
	MAC     net.HardwareAddr
	Addr    netip.Addr
}

// Topology is silo-net as silod made it: gw0's addresses, the networks
// and the ports. The ledger and the Docker facade own it; the fence
// only reads it.
type Topology struct {
	// API is silo.internal and Docker is docker.silo.internal, both on
	// gw0. Either may be the zero Addr, for none.
	API, Docker netip.Addr
	Networks    []Network
	Ports       []Port
}

// Config tunes the compiler.
type Config struct {
	// ExportPorts is the gateway's range of TCP export ports. Zero is
	// DefaultExportPorts.
	ExportPorts [2]uint16
	// MaxSetElements caps every set's size (Budget). Zero is the number
	// of addresses in the root's address space.
	MaxSetElements int
}

// DefaultExportPorts is install's range of TCP export ports.
var DefaultExportPorts = [2]uint16{20000, 29999}

func (c Config) exportPorts() [2]uint16 {
	if c.ExportPorts == [2]uint16{} {
		return DefaultExportPorts
	}
	return c.ExportPorts
}

// ifname is what an interface name may be, here: the kernel's 15
// bytes, from a set that needs no escaping in nft's syntax.
var ifname = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// Compile builds the ruleset for in and topo. It refuses a topology
// that the ledger should never have produced (a network outside its
// silo's blocks, two networks sharing addresses, a port outside its
// network), rather than compile a fence around it.
func Compile(in *fence.Input, topo *Topology, cfg Config) (*Ruleset, error) {
	if topo == nil {
		topo = &Topology{}
	}
	tenants := tenantSpace(in)
	blocks := map[string][]netip.Prefix{}
	for _, s := range in.Silos {
		blocks[s.ID] = s.Blocks
	}
	nets := map[string]Network{}
	var subnets []netip.Prefix
	for _, n := range topo.Networks {
		if !ifname.MatchString(n.Bridge) {
			return nil, fmt.Errorf("netfence: network %s: bridge name %q", n.ID, n.Bridge)
		}
		if _, dup := nets[n.ID]; dup {
			return nil, fmt.Errorf("netfence: network %s twice", n.ID)
		}
		if !n.Subnet.IsValid() || !n.Subnet.Addr().Is4() || !within(n.Subnet, blocks[n.Silo]) {
			return nil, fmt.Errorf("netfence: network %s: subnet %s is not in silo %q's blocks", n.ID, n.Subnet, n.Silo)
		}
		for _, o := range subnets {
			if o.Overlaps(n.Subnet) {
				return nil, fmt.Errorf("netfence: network %s: subnet %s overlaps %s", n.ID, n.Subnet, o)
			}
		}
		subnets = append(subnets, n.Subnet)
		nets[n.ID] = n
	}
	ports := map[string]bool{}
	pinned := map[netip.Addr]bool{}
	for _, p := range topo.Ports {
		n, ok := nets[p.Network]
		switch {
		case !ifname.MatchString(p.Name):
			return nil, fmt.Errorf("netfence: port name %q", p.Name)
		case ports[p.Name]:
			return nil, fmt.Errorf("netfence: port %s twice", p.Name)
		case !ok:
			return nil, fmt.Errorf("netfence: port %s: no network %q", p.Name, p.Network)
		case len(p.MAC) != 6:
			return nil, fmt.Errorf("netfence: port %s: MAC %q", p.Name, p.MAC)
		case !n.Subnet.Contains(p.Addr) || p.Addr == n.Subnet.Masked().Addr():
			return nil, fmt.Errorf("netfence: port %s: address %s is not a host of %s", p.Name, p.Addr, n.Subnet)
		case pinned[p.Addr]:
			return nil, fmt.Errorf("netfence: port %s: address %s is pinned twice", p.Name, p.Addr)
		}
		ports[p.Name], pinned[p.Addr] = true, true
	}

	inet := &Table{Family: "inet", Name: TableName}
	netSrc := &Set{Name: SetNetSrc, Key: []Field{IIF, SAddr}, Interval: true}
	external := &Set{Name: SetExternal, Key: []Field{IIF}}
	for _, n := range topo.Networks {
		netSrc.Elems = append(netSrc.Elems, Elem{n.Bridge, n.Subnet.Masked().String()})
		if !n.Internal {
			external.Elems = append(external.Elems, Elem{n.Bridge})
		}
	}
	tset := &Set{Name: SetTenants, Key: []Field{SAddr}, Interval: true}
	for _, p := range tenants {
		tset.Elems = append(tset.Elems, Elem{p.String()})
	}
	apiOK := &Set{Name: SetAPIOK, Key: []Field{SAddr}, Interval: true}
	for _, p := range apiSpace(in) {
		apiOK.Elems = append(apiOK.Elems, Elem{p.String()})
	}
	gw := &Set{Name: SetGateway, Key: []Field{SAddr}}
	for _, a := range []netip.Addr{topo.API, topo.Docker} {
		if a.IsValid() {
			gw.Elems = append(gw.Elems, Elem{a.String()})
		}
	}
	grantPorts := &Set{Name: SetGrantPorts, Key: []Field{SAddr, L4Proto, DPort, DAddr}, Interval: true}
	grantAny := &Set{Name: SetGrantAny, Key: []Field{SAddr, L4Proto, DAddr}, Interval: true}
	if err := grantElems(in, grantPorts, grantAny); err != nil {
		return nil, err
	}
	inet.Sets = []*Set{netSrc, tset, apiOK, external, gw, grantPorts, grantAny}
	for _, s := range inet.Sets {
		sortElems(s)
	}

	accept := func(c string, ms ...Match) Rule { return Rule{Matches: ms, Verdict: "accept", Comment: c} }
	set := func(name string, fs ...Field) Match { return Match{Fields: fs, Set: name} }
	is := func(f Field, vs ...string) Match { return Match{Fields: []Field{f}, Values: vs} }
	not := func(f Field, vs ...string) Match { return Match{Fields: []Field{f}, Values: vs, Neg: true} }
	tcp := is(L4Proto, "tcp")
	tcpUDP := is(L4Proto, "tcp", "udp")
	port := func(n int) string { return strconv.Itoa(n) }
	ep := cfg.exportPorts()

	prerouting := &Chain{Name: "prerouting", Hook: "prerouting", Priority: -300, Policy: "drop", Rules: []Rule{
		accept("loopback", is(IIF, "lo")),
		accept("source owned by the input bridge's network", set(SetNetSrc, IIF, SAddr)),
	}}
	from := []Match{set(SetExternal, IIF), set(SetTenants, SAddr)}
	input := &Chain{Name: "input", Hook: "input", Priority: 0, Policy: "drop", Rules: []Rule{
		accept("loopback", is(IIF, "lo")),
		accept("dns", append(from, set(SetGateway, DAddr), tcpUDP, is(DPort, port(PortDNS)))...),
		accept("exports and egress", append(from, set(SetGateway, DAddr), tcp,
			is(DPort, port(PortHTTP), port(PortEgress), fmt.Sprintf("%d-%d", ep[0], ep[1])))...),
	}}
	if topo.Docker.IsValid() {
		input.Rules = append(input.Rules,
			accept("published ports", append(from, is(DAddr, topo.Docker.String()), tcpUDP,
				not(DPort, port(PortDNS), port(PortDocker)))...))
	}
	api := []Match{set(SetExternal, IIF), set(SetAPIOK, SAddr)}
	if topo.API.IsValid() {
		input.Rules = append(input.Rules,
			accept("fence api", append(api, is(DAddr, topo.API.String()), tcp, is(DPort, port(PortAPI)))...))
	}
	if topo.Docker.IsValid() {
		input.Rules = append(input.Rules,
			accept("docker api", append(api, is(DAddr, topo.Docker.String()), tcp, is(DPort, port(PortDocker)))...))
	}
	// Grants first, as in the GCP fence; a connection a grant accepted
	// is followed by its replies and ICMP errors. Everything else,
	// between silos or between one silo's networks, drops: traffic in
	// one network is switched on its bridge and never forwarded.
	forward := &Chain{Name: "forward", Hook: "forward", Priority: 0, Policy: "drop", Rules: []Rule{
		accept("replies", is(CtState, "established", "related")),
		accept("link grants with ports", set(SetGrantPorts, SAddr, L4Proto, DPort, DAddr)),
		accept("link grants", set(SetGrantAny, SAddr, L4Proto, DAddr)),
	}}
	output := &Chain{Name: "output", Hook: "output", Priority: 0, Policy: "drop", Rules: []Rule{
		accept("loopback", is(OIF, "lo")),
		accept("gateway to silos", set(SetGateway, SAddr), set(SetTenants, DAddr)),
	}}
	inet.Chains = []*Chain{prerouting, input, forward, output}

	br := &Table{Family: "bridge", Name: TableName}
	pins := &Set{Name: SetPins, Key: []Field{IIF, EtherSrc, SAddr}}
	for _, p := range topo.Ports {
		pins.Elems = append(pins.Elems, Elem{p.Name, p.MAC.String(), p.Addr.String()})
	}
	sortElems(pins)
	br.Sets = []*Set{pins}
	// A frame from a port must carry the port's pinned MAC and address;
	// any other EtherType (IPv6, VLAN tags, LLDP) drops, and so does
	// everything from a port with no pin.
	br.Chains = []*Chain{{Name: "prerouting", Hook: "prerouting", Priority: -300, Policy: "drop", Rules: []Rule{
		accept("pinned ipv4", is(EtherType, "ip"), set(SetPins, IIF, EtherSrc, SAddr)),
		accept("pinned arp", is(EtherType, "arp"), set(SetPins, IIF, EtherSrc, ArpSPA), set(SetPins, IIF, ArpSHA, ArpSPA)),
	}}}
	return &Ruleset{Tables: []*Table{inet, br}}, nil
}

// tenantSpace is every silo's blocks, less the gateway and control
// ranges.
func tenantSpace(in *fence.Input) []netip.Prefix {
	var all []netip.Prefix
	for _, s := range in.Silos {
		for _, b := range s.Blocks {
			all = append(all, fence.Minus(b, in.Gateway, in.Control)...)
		}
	}
	return v4(fence.Normalize(all))
}

// apiSpace is the own space of every silo that isn't sealed: a sealed
// child's blocks are absent even when its parent's block holds them.
func apiSpace(in *fence.Input) []netip.Prefix {
	blocks := map[string][]netip.Prefix{}
	for _, s := range in.Silos {
		blocks[s.ID] = s.Blocks
	}
	own := fence.OwnSpaces(blocks, in.Gateway, in.Control)
	var out []netip.Prefix
	for _, s := range in.Silos {
		if !s.Sealed {
			out = append(out, own[s.ID]...)
		}
	}
	return v4(fence.Normalize(out))
}

func v4(ps []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range ps {
		if p.Addr().Is4() {
			out = append(out, p)
		}
	}
	return out
}

func within(p netip.Prefix, blocks []netip.Prefix) bool {
	for _, b := range blocks {
		if b.Bits() <= p.Bits() && b.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// protoNumbers are the protocols a grant may name.
var protoNumbers = map[string]int{"icmp": 1, "tcp": 6, "udp": 17, "esp": 50, "ah": 51, "sctp": 132}

// hasPorts is whether a protocol's grants can name ports.
var hasPorts = map[string]bool{"tcp": true, "udp": true, "sctp": true}

// grantElems adds the links' grants to the forward sets. Only INGRESS
// rules are read: each direction of a link is an EGRESS and INGRESS
// pair with the same ends, and one forward rule covers both. An
// export's grant, from the gateway range, has no forward rule here: the
// gateway's own sockets reach the target, through output.
//
// The kernel refuses an element of a concatenated interval set whose
// ends both lie in another element, so two links over the same silos
// (one for all protocols, one for tcp) would not load as they are. The
// grants are cut into disjoint boxes first, which match the same
// packets.
func grantElems(in *fence.Input, withPorts, any *Set) error {
	var ports, all []box
	for _, g := range in.Grants {
		if g.Direction != "INGRESS" || fromGateway(g, in.Gateway) {
			continue
		}
		for _, l := range g.L4 {
			proto := [2]uint32{0, 255}
			if l.Protocol != "all" {
				n := protoNumbers[l.Protocol]
				if n == 0 {
					return fmt.Errorf("netfence: grant %s: protocol %q", g.Name, l.Protocol)
				}
				proto = [2]uint32{uint32(n), uint32(n)}
			}
			for _, src := range v4(g.Src) {
				for _, dst := range v4(g.Dst) {
					s, d := addrSpan(src), addrSpan(dst)
					if len(l.Ports) == 0 || !hasPorts[l.Protocol] {
						all = append(all, box{s, proto, d})
						continue
					}
					for _, p := range l.Ports {
						lo, hi, err := portRange(p)
						if err != nil {
							return fmt.Errorf("netfence: grant %s: %v", g.Name, err)
						}
						ports = append(ports, box{s, proto, {uint32(lo), uint32(hi)}, d})
					}
				}
			}
		}
	}
	for _, b := range disjoint(ports) {
		withPorts.Elems = append(withPorts.Elems, Elem{addrValue(b[0]), numValue(b[1]), numValue(b[2]), addrValue(b[3])})
	}
	for _, b := range disjoint(all) {
		any.Elems = append(any.Elems, Elem{addrValue(b[0]), numValue(b[1]), addrValue(b[2])})
	}
	return nil
}

// box is a set element as one inclusive range per key field.
type box [][2]uint32

func addrSpan(p netip.Prefix) [2]uint32 {
	return [2]uint32{u32(p.Masked().Addr()), u32(fence.LastAddr(p))}
}

func u32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func ipv4(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// addrValue is a span as nft writes it: a prefix where it is one, else
// a range.
func addrValue(r [2]uint32) string {
	lo, hi := ipv4(r[0]), ipv4(r[1])
	if r[0] == r[1] {
		return lo.String()
	}
	for bits := 0; bits <= 32; bits++ {
		p := netip.PrefixFrom(lo, bits)
		if p.Masked().Addr() == lo && fence.LastAddr(p) == hi {
			return p.String()
		}
	}
	return lo.String() + "-" + hi.String()
}

func numValue(r [2]uint32) string {
	if r[0] == r[1] {
		return strconv.FormatUint(uint64(r[0]), 10)
	}
	return fmt.Sprintf("%d-%d", r[0], r[1])
}

// disjoint is the union of bs as boxes no two of which overlap: each box
// less every box kept before it.
func disjoint(bs []box) []box {
	var out []box
	for _, b := range bs {
		pieces := []box{b}
		for _, o := range out {
			var next []box
			for _, p := range pieces {
				next = append(next, subtract(p, o)...)
			}
			pieces = next
		}
		out = append(out, pieces...)
	}
	return out
}

// subtract is a less o, as at most two boxes per field: in turn, the
// part of a below and above o in that field, and the rest narrowed to
// o's range there.
func subtract(a, o box) []box {
	for i := range a {
		if a[i][1] < o[i][0] || o[i][1] < a[i][0] {
			return []box{a}
		}
	}
	var out []box
	rest := append(box(nil), a...)
	for i := range rest {
		if rest[i][0] < o[i][0] {
			b := append(box(nil), rest...)
			b[i] = [2]uint32{rest[i][0], o[i][0] - 1}
			out = append(out, b)
			rest[i][0] = o[i][0]
		}
		if rest[i][1] > o[i][1] {
			b := append(box(nil), rest...)
			b[i] = [2]uint32{o[i][1] + 1, rest[i][1]}
			out = append(out, b)
			rest[i][1] = o[i][1]
		}
	}
	return out
}

func fromGateway(g fence.Grant, gw netip.Prefix) bool {
	if !gw.IsValid() || len(g.Src) == 0 {
		return false
	}
	for _, s := range g.Src {
		if !within(s, []netip.Prefix{gw}) {
			return false
		}
	}
	return true
}
