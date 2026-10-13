package netfence

import (
	"bytes"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fence"
)

var (
	pfx  = netip.MustParsePrefix
	addr = netip.MustParseAddr
)

func mac(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

// example is a root with two children, one sealed, and a link from a to
// b on tcp/5432 and icmp; a has two networks, one Internal.
func example() (*fence.Input, *Topology) {
	in := &fence.Input{
		Gateway: pfx("10.200.255.0/24"), Control: pfx("10.200.254.0/24"),
		Root: []netip.Prefix{pfx("10.200.0.0/17")},
		Silos: []fence.Silo{
			{ID: "root", Blocks: []netip.Prefix{pfx("10.200.0.0/16")}},
			{ID: "a", Depth: 1, Blocks: []netip.Prefix{pfx("10.200.1.0/24")}},
			{ID: "b", Depth: 1, Blocks: []netip.Prefix{pfx("10.200.2.0/24")}, Sealed: true},
		},
	}
	in.Grants = append(in.Grants, fence.LinkGrant("l1", []netip.Prefix{pfx("10.200.1.0/24")}, []netip.Prefix{pfx("10.200.2.0/24")},
		true, false, []fence.L4{{Protocol: "tcp", Ports: []string{"5432"}}, {Protocol: "icmp"}})...)
	in.Grants = append(in.Grants, fence.ExportGrant("export-x-in", in.Gateway, netip.MustParseAddrPort("10.200.2.10:8000")))
	topo := &Topology{
		API: addr("10.255.255.1"), Docker: addr("10.255.255.2"),
		Networks: []Network{
			{ID: "na", Silo: "a", Bridge: "sb-na", Subnet: pfx("10.200.1.0/26")},
			{ID: "ni", Silo: "a", Bridge: "sb-ni", Subnet: pfx("10.200.1.64/26"), Internal: true},
			{ID: "nb", Silo: "b", Bridge: "sb-nb", Subnet: pfx("10.200.2.0/26")},
		},
		Ports: []Port{
			{Name: "va1", Network: "na", MAC: mac("02:00:0a:c8:01:02"), Addr: addr("10.200.1.2")},
			{Name: "vi1", Network: "ni", MAC: mac("02:00:0a:c8:01:42"), Addr: addr("10.200.1.66")},
			{Name: "vb1", Network: "nb", MAC: mac("02:00:0a:c8:02:0a"), Addr: addr("10.200.2.10")},
		},
	}
	return in, topo
}

func TestCompileExample(t *testing.T) {
	in, topo := example()
	rs, err := Compile(in, topo, Config{})
	if err != nil {
		t.Fatal(err)
	}
	tcp := func(iif string, src, dst string, port uint16) Packet {
		return Packet{IIF: iif, Src: addr(src), Dst: addr(dst), Proto: 6, DPort: port}
	}
	for _, c := range []struct {
		name, family, hook string
		p                  Packet
		want               string
	}{
		{"a to dns", "inet", "input", Packet{IIF: "sb-na", Src: addr("10.200.1.2"), Dst: addr("10.255.255.1"), Proto: 17, DPort: 53}, "accept"},
		{"a to the api", "inet", "input", tcp("sb-na", "10.200.1.2", "10.255.255.1", 8080), "accept"},
		{"a to docker", "inet", "input", tcp("sb-na", "10.200.1.2", "10.255.255.2", 2375), "accept"},
		{"sealed b to the api", "inet", "input", tcp("sb-nb", "10.200.2.10", "10.255.255.1", 8080), "drop"},
		{"sealed b to docker", "inet", "input", tcp("sb-nb", "10.200.2.10", "10.255.255.2", 2375), "drop"},
		{"sealed b to egress", "inet", "input", tcp("sb-nb", "10.200.2.10", "10.255.255.1", 3128), "accept"},
		{"published port", "inet", "input", tcp("sb-nb", "10.200.2.10", "10.255.255.2", 8443), "accept"},
		{"published port off docker", "inet", "input", tcp("sb-nb", "10.200.2.10", "10.255.255.1", 8443), "drop"},
		{"internal network to egress", "inet", "input", tcp("sb-ni", "10.200.1.66", "10.255.255.1", 3128), "drop"},
		{"export port", "inet", "input", tcp("sb-na", "10.200.1.2", "10.255.255.1", 20001), "accept"},
		{"link a to b", "inet", "forward", tcp("sb-na", "10.200.1.2", "10.200.2.10", 5432), "accept"},
		{"link a to b, other port", "inet", "forward", tcp("sb-na", "10.200.1.2", "10.200.2.10", 22), "drop"},
		{"link a to b, icmp", "inet", "forward", Packet{IIF: "sb-na", Src: addr("10.200.1.2"), Dst: addr("10.200.2.10"), Proto: 1}, "accept"},
		{"link b to a", "inet", "forward", tcp("sb-nb", "10.200.2.10", "10.200.1.2", 5432), "drop"},
		{"reply b to a", "inet", "forward", Packet{IIF: "sb-nb", Src: addr("10.200.2.10"), Dst: addr("10.200.1.2"), Proto: 6, DPort: 40000, CtState: "established"}, "accept"},
		{"a's networks", "inet", "forward", tcp("sb-na", "10.200.1.2", "10.200.1.66", 80), "drop"},
		{"spoofed source", "inet", "prerouting", tcp("sb-na", "10.200.2.10", "10.255.255.1", 80), "drop"},
		{"owned source", "inet", "prerouting", tcp("sb-na", "10.200.1.2", "10.255.255.1", 80), "accept"},
		{"ipv6", "inet", "prerouting", Packet{IIF: "sb-na", EtherType: "ip6"}, "drop"},
		{"gateway to a silo", "inet", "output", Packet{OIF: "sb-nb", Src: addr("10.255.255.1"), Dst: addr("10.200.2.10"), Proto: 6, DPort: 8000}, "accept"},
		{"gateway to the host", "inet", "output", Packet{OIF: "sb-nb", Src: addr("10.255.255.1"), Dst: addr("192.168.1.1"), Proto: 6, DPort: 22}, "drop"},
		{"pinned frame", "bridge", "prerouting", Packet{IIF: "va1", EtherType: "ip", EtherSrc: "02:00:0a:c8:01:02", Src: addr("10.200.1.2")}, "accept"},
		{"wrong mac", "bridge", "prerouting", Packet{IIF: "va1", EtherType: "ip", EtherSrc: "02:00:0a:c8:01:03", Src: addr("10.200.1.2")}, "drop"},
		{"wrong ip", "bridge", "prerouting", Packet{IIF: "va1", EtherType: "ip", EtherSrc: "02:00:0a:c8:01:02", Src: addr("10.200.1.3")}, "drop"},
		{"another port's pin", "bridge", "prerouting", Packet{IIF: "va1", EtherType: "ip", EtherSrc: "02:00:0a:c8:02:0a", Src: addr("10.200.2.10")}, "drop"},
		{"pinned arp", "bridge", "prerouting", Packet{IIF: "va1", EtherType: "arp", EtherSrc: "02:00:0a:c8:01:02", ArpSHA: "02:00:0a:c8:01:02", Src: addr("10.200.1.2")}, "accept"},
		{"arp with another sender mac", "bridge", "prerouting", Packet{IIF: "va1", EtherType: "arp", EtherSrc: "02:00:0a:c8:01:02", ArpSHA: "02:00:0a:c8:01:09", Src: addr("10.200.1.2")}, "drop"},
		{"vlan", "bridge", "prerouting", Packet{IIF: "va1", EtherType: "vlan", EtherSrc: "02:00:0a:c8:01:02"}, "drop"},
		{"unpinned port", "bridge", "prerouting", Packet{IIF: "vx9", EtherType: "ip", EtherSrc: "02:00:0a:c8:01:02", Src: addr("10.200.1.2")}, "drop"},
	} {
		if got, _ := rs.Eval(c.family, c.hook, c.p); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCompileRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Topology)
	}{
		{"subnet outside the silo", func(tp *Topology) { tp.Networks[0].Subnet = pfx("10.200.2.64/26") }},
		{"overlapping subnets", func(tp *Topology) { tp.Networks[1].Subnet = pfx("10.200.1.0/27") }},
		{"bridge name", func(tp *Topology) { tp.Networks[0].Bridge = `sb"; flush ruleset` }},
		{"port outside its network", func(tp *Topology) { tp.Ports[0].Addr = addr("10.200.1.70") }},
		{"port on the network address", func(tp *Topology) { tp.Ports[0].Addr = addr("10.200.1.0") }},
		{"address pinned twice", func(tp *Topology) {
			tp.Ports = append(tp.Ports, Port{Name: "va2", Network: "na", MAC: tp.Ports[0].MAC, Addr: tp.Ports[0].Addr})
		}},
		{"no network", func(tp *Topology) { tp.Ports[0].Network = "zz" }},
	} {
		in, topo := example()
		c.edit(topo)
		if _, err := Compile(in, topo, Config{}); err == nil {
			t.Errorf("%s: compiled", c.name)
		}
	}
}

func TestRenderDeterministic(t *testing.T) {
	in, topo := example()
	a, _ := Compile(in, topo, Config{})
	in.Silos[0], in.Silos[2] = in.Silos[2], in.Silos[0]
	topo.Ports[0], topo.Ports[2] = topo.Ports[2], topo.Ports[0]
	b, _ := Compile(in, topo, Config{})
	if a.Render() != b.Render() {
		t.Errorf("the same fence rendered twice differs:\n%s\n%s", a.Render(), b.Render())
	}
}

// skip skips, or fails under SILO_REQUIRE_NSD (CI's nsd job, which
// has nft and unprivileged user namespaces).
func skip(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("SILO_REQUIRE_NSD") != "" {
		t.Fatalf("SILO_REQUIRE_NSD is set, but "+format, args...)
	}
	t.Skipf(format, args...)
}

// nftBinary finds nft, which Debian keeps in /usr/sbin.
func nftBinary(t *testing.T) string {
	for _, p := range []string{"nft", "/usr/sbin/nft", "/sbin/nft"} {
		if path, err := exec.LookPath(p); err == nil {
			return path
		}
	}
	skip(t, "nft is not installed")
	return ""
}

// unshare runs nft in a new user and network namespace, which needs no
// privilege where unprivileged user namespaces are allowed.
func unshareNFT(t *testing.T, nft string) *exec.Cmd {
	if _, err := exec.LookPath("unshare"); err != nil {
		skip(t, "unshare is not installed")
	}
	probe := exec.Command("unshare", "-rn", nft, "list", "ruleset")
	if out, err := probe.CombinedOutput(); err != nil {
		skip(t, "nft in a scratch user and network namespace: %v: %s", err, out)
	}
	return exec.Command("unshare", "-rn", "sh", "-c", `"$0" -f "$1" && "$0" -f "$1" && "$0" list ruleset`, nft)
}

// TestRenderLoads loads the rendered ruleset, twice (the second replaces
// the first), into a scratch network namespace with the real nft.
func TestRenderLoads(t *testing.T) {
	nft := nftBinary(t)
	cmd := unshareNFT(t, nft)
	in, topo := example()
	rs, err := Compile(in, topo, Config{})
	if err != nil {
		t.Fatal(err)
	}
	f := t.TempDir() + "/silo.nft"
	if err := os.WriteFile(f, []byte(rs.Render()), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args, f)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("nft -f: %v\n%s\n%s", err, out.String(), rs.Render())
	}
	for _, want := range []string{"table inet silo", "table bridge silo", "@api_ok", "10.200.1.0/24"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the listed ruleset has no %q:\n%s", want, out.String())
		}
	}
}
