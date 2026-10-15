package local_test

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

// The local-only structural fence cases of local-backend.md §13: from
// a probe, silo-net has no way into the root namespace (no route to
// the metadata server, the host's own addresses, or a service the host
// listens for on 0.0.0.0), and a port with no pin entry passes
// nothing. Adoption across a silod restart needs an installed target,
// and comes with it.

func eachFence(t *testing.T, fn func(t *testing.T, f conformance.F)) {
	var local []conformance.Target
	for _, tg := range targets.All() {
		if strings.HasPrefix(tg.Name(), "local/") {
			local = append(local, tg)
		}
	}
	conformance.ForEachFence(t, local, func(t *testing.T, fe conformance.Fence) {
		fn(t, conformance.F{Fence: fe, T: t})
	})
}

// hostAddrs are this machine's IPv4 addresses, loopback excepted: a
// probe's own loopback is its own, in a container as on a VM, and
// answers it.
func hostAddrs(t *testing.T) []netip.Addr {
	t.Helper()
	as, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var out []netip.Addr
	for _, a := range as {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() && !p.Addr().IsLoopback() {
			out = append(out, p.Addr())
		}
	}
	return out
}

func TestNoWayIntoTheRootNamespace(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F) {
		// A service on the host's 0.0.0.0, as a careless one would
		// listen: no probe reaches it at any of the host's addresses.
		l, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		port := l.Addr().(*net.TCPAddr).Port
		s := f.Mk("", "s", 24, obj{"egress": obj{"allow": []string{"*"}}})
		p := f.Probe(t, s, "pp")
		checks := conformance.Reach("metadata", "169.254.169.254", 80)
		for i, a := range hostAddrs(t) {
			checks = append(checks, conformance.Reach(fmt.Sprintf("host%d", i), a.String(), port)...)
		}
		// The gateway's own addresses answer only their services.
		checks = append(checks,
			conformance.Check{ID: "gw-ssh", Kind: "tcp", Host: "10.255.255.1", Port: 22},
			conformance.Check{ID: "gw-host-port", Kind: "tcp", Host: "10.255.255.1", Port: port},
			conformance.Check{ID: "gw-dns", Kind: "udp", Host: "10.255.255.1", Port: 53})
		res := p.Run(checks)
		conformance.ExpectReach(t, res, "metadata", false)
		for i, a := range hostAddrs(t) {
			for _, k := range []string{"tcp", "udp", "icmp"} {
				id := fmt.Sprintf("host%d-%s", i, k)
				if res[id].OK {
					t.Errorf("%s: the probe reached the host at %s: %s", id, a, res[id].Detail)
				}
			}
		}
		conformance.Expect(t, res, "gw-ssh", false)
		conformance.Expect(t, res, "gw-host-port", false)
		conformance.Expect(t, res, "gw-dns", true)
	})
}

func TestUnpinnedPortPassesNothing(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F) {
		s := f.Mk("", "s", 24, obj{"egress": obj{"allow": []string{"*"}}})
		p, q := f.Probe(t, s, "pp"), f.Probe(t, s, "pq")
		u, ok := f.Fence.(conformance.Unpinned)
		if !ok {
			t.Skip("not offered: no way to send from an unpinned port")
		}
		checks := append(conformance.Reach("peer", q.Addr().String(), 7000),
			conformance.Check{ID: "dns", Kind: "udp", Host: "10.255.255.1", Port: 53},
			conformance.Check{ID: "api", Kind: "tcp", Host: "10.255.255.1", Port: 8080})
		pinned := p.Run(checks)
		conformance.ExpectReach(t, pinned, "peer", true)
		conformance.Expect(t, pinned, "dns", true)
		conformance.Expect(t, pinned, "api", true)
		res := u.FromUnpinnedPort(p, checks)
		conformance.ExpectReach(t, res, "peer", false)
		conformance.Expect(t, res, "dns", false)
		conformance.Expect(t, res, "api", false)
	})
}
