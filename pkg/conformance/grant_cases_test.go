//go:build silo_conformance

package conformance_test

// The cross-silo grant cases (docs/silo/testing.md, "Cases", SV-122):
// exports and links under "Crossing silos without consent", HTTP and
// TCP exports end to end (Silo-Caller headers, PROXY v2), what a sealed
// silo keeps (an accepted export's port and an L3 link), and the
// database with an active link and an accepted export in the tree.
//
// Every grant call is the root's, made on the silo's behalf through
// /silo/v1/silos/{silo}/... (SV-16): it acts exactly as the silo's own
// would, and the suite has no identity of its own inside /a or /b.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/client"
	. "github.com/bwsalmon/silo/pkg/conformance"
)

// hostPort splits a TCP import's "silo.internal:<port>".
func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		t.Fatalf("import address %q has no port", addr)
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		t.Fatalf("import address %q: %v", addr, err)
	}
	return addr[:i], p
}

// offer offers exporter's export name, at its probe's ip:port, to each
// of to, and withdraws it when the test ends.
func offer(t *testing.T, exporter siloInfo, name, protocol, target string, pp bool, to ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := run.API.OfferExport(ctx, exporter.path, client.OfferRequest{Name: name, Target: target,
		Protocol: protocol, To: to, ProxyProtocol: pp}, client.Mutation{}); err != nil {
		t.Fatalf("offer %s/%s: %v", exporter.path, name, err)
	}
	t.Cleanup(func() { run.API.WithdrawExport(context.Background(), exporter.path, name, "", client.Mutation{}) })
}

// accept accepts export ("<exporter path>/<name>") as importer.
func accept(t *testing.T, importer siloInfo, export string) *client.Import {
	t.Helper()
	im, err := run.API.AcceptExport(context.Background(), importer.path, export, client.Mutation{})
	if err != nil {
		t.Fatalf("%s accepting %s: %v", importer.path, export, err)
	}
	if im.State != "ACTIVE" {
		t.Errorf("%s's import of %s is %s, want ACTIVE", importer.path, export, im.State)
	}
	return im
}

// link proposes side's half of link name, and deletes it when the test
// ends (deleting either side ends both).
func link(t *testing.T, side siloInfo, name, peer, dir string, rules ...client.LinkRule) (*client.Link, error) {
	t.Helper()
	l, err := run.API.ProposeLink(context.Background(), side.path, client.LinkRequest{Name: name, Peer: peer, Direction: dir, Rules: rules}, client.Mutation{})
	if err == nil {
		t.Cleanup(func() { run.API.DeleteLink(context.Background(), side.path, name, client.Mutation{}) })
	}
	return l, err
}

func mustLink(t *testing.T, side siloInfo, name, peer, dir string, rules ...client.LinkRule) *client.Link {
	t.Helper()
	l, err := link(t, side, name, peer, dir, rules...)
	if err != nil {
		t.Fatalf("%s proposing link %s to %s: %v", side.path, name, peer, err)
	}
	return l
}

// linkState is the state of side's link name.
func linkState(t *testing.T, side siloInfo, name string) string {
	t.Helper()
	ls, err := run.API.Links(context.Background(), side.path)
	if err != nil {
		t.Fatalf("links of %s: %v", side.path, err)
	}
	for _, l := range ls {
		if l.Name == name {
			return l.State
		}
	}
	return ""
}

var tcp7000 = client.LinkRule{Protocol: "tcp", Ports: []string{"7000"}}

// exporterProbe starts a probe in s at its 30th address serving
// exporterServers, and waits until they're up.
func exporterProbe(t *testing.T, s siloInfo, name string) (*vm, string) {
	t.Helper()
	ip := addrIn(s, 30)
	v := startProbe(t, s, name, ip, []Check{listeners(3000), ExporterServers(3000), {ID: "hold", Kind: "sh", Script: "sleep 3000"}})
	r, ok := v.until("serve", 10*time.Minute)
	if !ok || !r.OK {
		t.Fatalf("the exporter probe in %s never served: %+v", s.path, r)
	}
	return v, ip
}

// TestExportsWithoutConsent: an export offered but not accepted, and
// one accepted but not offered (or offered to someone else), are
// unreachable through the gateway.
func TestExportsWithoutConsent(t *testing.T) {
	tree(t)
	allowAll(t, b)
	_, ip := exporterProbe(t, b, "probe-x-nocons")
	offer(t, b, "offered", "http", ip+":7000", false, "/a")
	offer(t, b, "toc", "http", ip+":7000", false, "/a/c")
	offer(t, b, "tcpoffered", "tcp", ip+":7003", false, "/a")

	t.Run("AcceptedButNotOfferedIsRefused", func(t *testing.T) {
		for _, x := range []string{"/b/unoffered", "/b/toc"} {
			if im, err := run.API.AcceptExport(context.Background(), "/a", x, client.Mutation{}); err == nil {
				run.API.DeleteImport(context.Background(), "/a", im.ID, client.Mutation{})
				t.Errorf("/a accepted %s, which wasn't offered to it: %+v", x, im)
			}
		}
	})
	gw := "http://silo.internal/x/" + b.id + "/"
	res := probe(t, a, "probe-x-nocons", []Check{
		{ID: "offered", Kind: "http", URL: gw + "offered/"},
		{ID: "unoffered", Kind: "http", URL: gw + "unoffered/"},
		{ID: "toc", Kind: "http", URL: gw + "toc/"},
		{ID: "direct", Kind: "tcp", Host: ip, Port: 7000},
	})
	t.Run("OfferedNotAccepted", func(t *testing.T) { refusedFromProbe(t, res, "offered") })
	t.Run("AcceptedNotOffered", func(t *testing.T) {
		refusedFromProbe(t, res, "unoffered")
		refusedFromProbe(t, res, "toc")
	})
	t.Run("NoDirectPath", func(t *testing.T) { expect(t, res, "direct", false) })
	t.Run("TCPOfferedNotAcceptedHasNoPort", func(t *testing.T) {
		offers, err := run.API.Offers(context.Background(), "/a")
		if err != nil {
			t.Fatalf("offers of /a: %v", err)
		}
		for _, o := range offers {
			if o.Name == "tcpoffered" && (o.Port != 0 || o.State != "PENDING") {
				t.Errorf("an unaccepted TCP offer has a gateway port: %+v", o)
			}
		}
	})
}

// TestExportsEndToEnd: an HTTP export carries Silo-Caller and
// Silo-Caller-Path (and drops the caller's own), a TCP export splices
// to the target with the gateway as the source, or with a PROXY v2
// header naming the importer's VM if the export asked for one, and a
// TCP import's port is refused from outside the importer's space.
// Revoking closes an open connection within a second.
func TestExportsEndToEnd(t *testing.T) {
	tree(t)
	allowAll(t, b)
	_, ip := exporterProbe(t, b, "probe-x-e2e")
	offer(t, b, "web", "http", ip+":7000", false, "/a")
	offer(t, b, "raw", "tcp", ip+":7003", false, "/a")
	offer(t, b, "pp2", "tcp", ip+":7003", true, "/a")
	web := accept(t, a, "/b/web")
	raw := accept(t, a, "/b/raw")
	pp2 := accept(t, a, "/b/pp2")
	rawHost, rawPort := hostPort(t, raw.Address)
	ppHost, ppPort := hostPort(t, pp2.Address)
	aIP := addrIn(a, 21)
	imp := startProbe(t, a, "probe-x-imp", aIP, []Check{
		{ID: "web", Kind: "http", URL: web.Address + "hello", Headers: []string{"Silo-Caller: forged", "Silo-Caller-Path: /forged"}},
		TCPHello("raw", rawHost, rawPort),
		TCPHello("pp2", ppHost, ppPort),
	})
	resC := probe(t, c, "probe-x-other", []Check{TCPHello("raw", rawHost, rawPort)})
	res := imp.wait()

	t.Run("HTTPCarriesSiloCaller", func(t *testing.T) {
		expectStatus(t, res, "web", 200, "path=/hello", "Silo-Caller: "+a.id, "Silo-Caller-Path: /a")
		if strings.Contains(res["web"].Detail, "forged") {
			t.Errorf("the caller's own Silo-Caller headers reached the exporter: %s", res["web"].Detail)
		}
	})
	t.Run("TCPSourceIsTheGateway", func(t *testing.T) {
		expect(t, res, "raw", true)
		d := res["raw"].Detail
		if !strings.HasPrefix(d, "noproxy src=") || strings.Contains(d, aIP) {
			t.Errorf("the exporter saw %q, want the gateway's address and no PROXY header", d)
		}
	})
	t.Run("TCPProxyV2NamesTheImporter", func(t *testing.T) {
		expect(t, res, "pp2", true)
		if d := res["pp2"].Detail; !strings.HasPrefix(d, "pp2 src="+aIP) {
			t.Errorf("the exporter saw %q, want a PROXY v2 header from %s", d, aIP)
		}
	})
	t.Run("TCPPortRefusedOutsideImporter", func(t *testing.T) {
		expect(t, resC, "raw", false)
	})
	t.Run("RevokeClosesOpenConnection", func(t *testing.T) {
		// The probe connects, reports "conn" once bytes flow, then reports
		// the time (its clock, which NTP keeps to the test's within a few
		// ms on GCE) the connection stopped.
		v := startProbe(t, a, "probe-x-revoke", "", []Check{
			{ID: "conn", Kind: "sh", Script: fmt.Sprintf(`cat >/tmp/hold.py <<'EOF'
import socket, time
s = socket.create_connection((%q, %d), 8)
open("/tmp/connected", "w").write("1")
s.settimeout(30)
try:
    while s.recv(64): pass
except Exception:
    pass
open("/tmp/closed", "w").write("%%.3f" %% time.time())
EOF
(timeout 900 python3 /tmp/hold.py >/dev/null 2>&1 &)
for i in $(seq 60); do [ -f /tmp/connected ] && exit 0; sleep 1; done; exit 1`, rawHost, rawPort)},
			{ID: "closed", Kind: "sh", Script: `for i in $(seq 600); do [ -f /tmp/closed ] && { cat /tmp/closed; exit 0; }; sleep 1; done; exit 1`},
		})
		r, ok := v.until("conn", 10*time.Minute)
		if !ok || !r.OK {
			t.Fatalf("the probe never connected to the import: %+v", r)
		}
		time.Sleep(5 * time.Second)
		if _, err := run.API.WithdrawExport(context.Background(), "/b", "raw", "", client.Mutation{}); err != nil {
			t.Fatalf("withdraw /b/raw: %v", err)
		}
		revoked := time.Now()
		got := v.wait()
		expect(t, got, "closed", true)
		sec, err := strconv.ParseFloat(strings.Fields(got["closed"].Detail + " x")[0], 64)
		if err != nil {
			t.Fatalf("the probe's close time %q: %v", got["closed"].Detail, err)
		}
		closed := time.UnixMilli(int64(sec * 1000))
		gap := closed.Sub(revoked)
		t.Logf("closed %v after the withdraw returned", gap)
		// A connection closed before the withdraw returned is fine; one
		// open more than a second after isn't. 250ms is for clock skew.
		if gap > 1250*time.Millisecond {
			t.Errorf("the open connection closed %v after the export was withdrawn, want within a second", gap)
		}
	})
}

// TestLinksWithoutConsent: a link proposed by one side only is
// unreachable; with both sides it is reachable on the named port only;
// deleted, unreachable again.
func TestLinksWithoutConsent(t *testing.T) {
	tree(t)
	allowAll(t, a)
	allowAll(t, b)
	_, ip := exporterProbe(t, b, "probe-l-peer")
	checks := func(phase string) []Check {
		return []Check{
			{ID: phase + "-7000", Kind: "tcp", Host: ip, Port: 7000},
			{ID: phase + "-7002", Kind: "tcp", Host: ip, Port: 7002},
			{ID: phase + "-udp", Kind: "udp", Host: ip, Port: 7001},
			{ID: phase + "-icmp", Kind: "icmp", Host: ip},
		}
	}
	mustLink(t, a, "web", "/b", "outbound", tcp7000)
	t.Run("OneSideOnly", func(t *testing.T) {
		if s := linkState(t, a, "web"); s != "PENDING" {
			t.Errorf("a one-sided link is %s, want PENDING", s)
		}
		res := probe(t, a, "probe-l-one", checks("one"))
		for _, p := range []string{"7000", "7002", "udp", "icmp"} {
			expect(t, res, "one-"+p, false)
		}
	})
	t.Run("BothSidesNamedPortOnly", func(t *testing.T) {
		mustLink(t, b, "web", "/a", "inbound", tcp7000)
		if s := linkState(t, a, "web"); s != "ACTIVE" {
			t.Fatalf("a link both sides proposed is %s, want ACTIVE", s)
		}
		res := probe(t, a, "probe-l-both", checks("both"))
		expect(t, res, "both-7000", true)
		for _, p := range []string{"7002", "udp", "icmp"} {
			expect(t, res, "both-"+p, false)
		}
	})
	t.Run("Deleted", func(t *testing.T) {
		if _, err := run.API.DeleteLink(context.Background(), "/a", "web", client.Mutation{}); err != nil {
			t.Fatalf("delete link: %v", err)
		}
		if s := linkState(t, b, "web"); s == "ACTIVE" {
			t.Errorf("/b's side of a deleted link is still ACTIVE")
		}
		res := probe(t, a, "probe-l-gone", append([]Check{{ID: "settle", Kind: "sh", Script: "sleep 20"}}, checks("gone")...))
		for _, p := range []string{"7000", "7002", "udp", "icmp"} {
			expect(t, res, "gone-"+p, false)
		}
	})
}

// TestLinksPolicy: /a sets links: deny on /a/c. A link /a/c and /b both
// want is suspended if it was active, and refused if proposed after.
//
// testing.md says "refused with LOOSER_THAN_PARENT or suspended". silod
// refuses a proposal its own effective policy forbids with
// PERMISSION_DENIED naming the policy; LOOSER_THAN_PARENT is, by
// fence-api.md's table, the code for a change giving a child more than
// its parent has, which a proposal isn't. The case takes either code.
func TestLinksPolicy(t *testing.T) {
	tree(t)
	ctx := context.Background()
	allowAll(t, c)
	allowAll(t, b)
	_, ip := exporterProbe(t, b, "probe-lp-peer")
	mustLink(t, c, "pol", "/b", "outbound", tcp7000)
	mustLink(t, b, "pol", "/a/c", "inbound", tcp7000)
	if s := linkState(t, c, "pol"); s != "ACTIVE" {
		t.Fatalf("the link before the policy is %s, want ACTIVE", s)
	}
	setLinks := func(v string) error {
		_, err := run.API.Patch(ctx, "/a/c", client.Patch{"grant_policy": map[string]string{"links": v}}, client.Mutation{})
		return err
	}
	if err := setLinks("deny"); err != nil {
		t.Fatalf("/a/c links: deny: %v", err)
	}
	t.Cleanup(func() { setLinks("allow") })
	t.Run("ActiveLinkSuspended", func(t *testing.T) {
		for _, s := range []siloInfo{c, b} {
			if st := linkState(t, s, "pol"); st != "SUSPENDED" {
				t.Errorf("%s's side is %s under links: deny, want SUSPENDED", s.path, st)
			}
		}
		res := probe(t, c, "probe-lp-susp", []Check{{ID: "tcp", Kind: "tcp", Host: ip, Port: 7000}})
		expect(t, res, "tcp", false)
	})
	t.Run("NewLinkRefused", func(t *testing.T) {
		_, err := link(t, c, "pol2", "/b", "outbound", tcp7000)
		if err == nil {
			t.Fatal("/a/c proposed a link under links: deny")
		}
		if code := apiCode(err); code != "LOOSER_THAN_PARENT" && code != "PERMISSION_DENIED" {
			t.Errorf("a link under links: deny: %v, want LOOSER_THAN_PARENT or PERMISSION_DENIED", err)
		}
	})
	t.Run("AllowedAgainResumes", func(t *testing.T) {
		if err := setLinks("allow"); err != nil {
			t.Fatalf("/a/c links: allow: %v", err)
		}
		if st := linkState(t, c, "pol"); st != "ACTIVE" {
			t.Errorf("the link is %s once links are allowed again, want ACTIVE", st)
		}
	})
}

// TestSealedSiloKeepsGrants: from a sealed silo's probe, an accepted
// export's port and an L3 link still work (gateway.md, "What a sealed
// silo keeps", SV-82).
func TestSealedSiloKeepsGrants(t *testing.T) {
	tree(t)
	s := mkSilo(t, "/s2", 26, "none", nil)
	allowAll(t, s)
	allowAll(t, b)
	_, ip := exporterProbe(t, b, "probe-sg-peer")
	offer(t, b, "sealed-web", "http", ip+":7000", false, "/s2")
	offer(t, b, "sealed-tcp", "tcp", ip+":7003", false, "/s2")
	web := accept(t, s, "/b/sealed-web")
	tcpIm := accept(t, s, "/b/sealed-tcp")
	host, port := hostPort(t, tcpIm.Address)
	mustLink(t, s, "sl", "/b", "outbound", tcp7000)
	mustLink(t, b, "sl", "/s2", "inbound", tcp7000)
	res := probe(t, s, "probe-sg", []Check{
		{ID: "api-port", Kind: "tcp", Host: "silo.internal", Port: 8080},
		{ID: "export-http", Kind: "http", URL: web.Address},
		TCPHello("export-tcp", host, port),
		{ID: "link", Kind: "tcp", Host: ip, Port: 7000},
	})
	expect(t, res, "api-port", false)
	t.Run("ExportHTTP", func(t *testing.T) { expectStatus(t, res, "export-http", 200, "Silo-Caller: "+s.id) })
	t.Run("ExportTCPPort", func(t *testing.T) { expect(t, res, "export-tcp", true) })
	t.Run("L3Link", func(t *testing.T) { expect(t, res, "link", true) })
}

// TestDatabaseWithGrants: with an active link (every protocol) and an
// accepted export in the tree, and tenant rules that allow everything,
// still no probe in /a or /a/c reaches the database's PSC endpoint.
func TestDatabaseWithGrants(t *testing.T) {
	tree(t)
	addr, err := run.Compute.Addresses.Get(run.Project, run.Region, "silo-db-psc").Do()
	if err != nil {
		t.Fatalf("the PSC endpoint's address: %v", err)
	}
	ep := addr.Address
	for _, s := range []siloInfo{a, b, c} {
		allowAll(t, s)
	}
	_, ip := exporterProbe(t, b, "probe-dbg-peer")
	all := client.LinkRule{Protocol: "all"}
	mustLink(t, a, "dbg", "/b", "both", all)
	mustLink(t, b, "dbg", "/a", "both", all)
	if st := linkState(t, a, "dbg"); st != "ACTIVE" {
		t.Fatalf("the link is %s, want ACTIVE", st)
	}
	offer(t, b, "dbg", "tcp", ip+":7003", false, "/a", "/a/c")
	accept(t, a, "/b/dbg")
	accept(t, c, "/b/dbg")
	checks := []Check{{ID: "icmp", Kind: "icmp", Host: ep}, {ID: "link-works", Kind: "tcp", Host: ip, Port: 7002}}
	for _, port := range []int{5432, 3307, 22, 443} {
		checks = append(checks, Check{ID: fmt.Sprintf("tcp-%d", port), Kind: "tcp", Host: ep, Port: port})
	}
	for _, s := range []siloInfo{a, c} {
		res := probe(t, s, "probe-dbg", checks)
		t.Run("From"+strings.ReplaceAll(s.path, "/", "_"), func(t *testing.T) {
			if s == a {
				// The link is live, so the case isn't passing because
				// /a can't reach anything.
				expect(t, res, "link-works", true)
			}
			for _, ch := range checks {
				if ch.ID != "link-works" {
					expect(t, res, ch.ID, false)
				}
			}
		})
	}
}
