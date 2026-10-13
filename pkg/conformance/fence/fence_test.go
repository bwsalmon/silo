package fence_test

import (
	"net/netip"
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

// The backend-neutral fence cases of local-backend.md §13 (call L24):
// isolation, origin spoofing, never-looser, links, egress, and sealed
// and read silos, run against every target's Fence. The GCP harness's
// own forms of them are in pkg/conformance's fence_cases_test.go and
// grant_cases_test.go, behind the silo_conformance tag.

func eachFence(t *testing.T, fn func(t *testing.T, f conformance.F)) {
	conformance.ForEachFence(t, targets.All(), func(t *testing.T, fe conformance.Fence) {
		fn(t, conformance.F{Fence: fe, T: t})
	})
}

// tree is /a, /a/c and /b, each allowed every egress name, so that
// nothing but the fence stands between their probes.
type tree struct{ a, c, b string }

func mkTree(f conformance.F) tree {
	all := obj{"egress": obj{"allow": []string{"*"}}}
	var tr tree
	tr.a = f.Mk("", "a", 20, all)
	tr.c = f.Mk("/a", "c", 24, all)
	tr.b = f.Mk("", "b", 20, all)
	return tr
}

// The GCP and installed local targets report their Fence as not
// runnable here, never as passing; the fake runs.
func TestTargetsFence(t *testing.T) {
	for _, tg := range targets.All() {
		_, err := tg.Fence(t)
		switch tg.Name() {
		case "local/fake":
			if err != nil {
				t.Errorf("local/fake: %v", err)
			}
		default:
			if _, ok := err.(*conformance.NotRunnable); !ok {
				t.Errorf("%s: %v, want not runnable", tg.Name(), err)
			}
		}
	}
}

// Isolation: /a → /b, /a → /a/c and /a/c → /a are unreachable on TCP,
// UDP and ICMP; two probes of one silo reach each other, so the
// refusals are the fence's.
func TestIsolation(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F) {
		tr := mkTree(f)
		pa, pa2 := f.Probe(t, tr.a, "pa"), f.Probe(t, tr.a, "pa2")
		pc, pb := f.Probe(t, tr.c, "pc"), f.Probe(t, tr.b, "pb")
		var checks []conformance.Check
		checks = append(checks, conformance.Reach("own", pa2.Addr().String(), 7000)...)
		checks = append(checks, conformance.Reach("sibling", pb.Addr().String(), 7000)...)
		checks = append(checks, conformance.Reach("child", pc.Addr().String(), 7000)...)
		res := pa.Run(checks)
		conformance.ExpectReach(t, res, "own", true)
		conformance.ExpectReach(t, res, "sibling", false)
		conformance.ExpectReach(t, res, "child", false)
		res = pc.Run(conformance.Reach("parent", pa.Addr().String(), 7000))
		conformance.ExpectReach(t, res, "parent", false)
		res = pb.Run(conformance.Reach("sibling", pa.Addr().String(), 7000))
		conformance.ExpectReach(t, res, "sibling", false)
	})
}

// Origin spoofing: a packet sent from another silo's address, or from
// an address of the probe's own silo that isn't its own, never
// arrives, even at a probe it could otherwise reach.
func TestOriginSpoofing(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F) {
		tr := mkTree(f)
		pa, pa2 := f.Probe(t, tr.a, "pa"), f.Probe(t, tr.a, "pa2")
		pb := f.Probe(t, tr.b, "pb")
		to := conformance.Reach("own", pa2.Addr().String(), 7000)
		spoof := func(src netip.Addr) map[string]conformance.ProbeResult {
			t.Helper()
			res, err := pa.Spoof(src, to)
			if err == conformance.ErrNotOffered {
				t.Skip("not offered: this target's probes can't spoof")
			}
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		conformance.ExpectReach(t, spoof(pa.Addr()), "own", true)
		conformance.ExpectReach(t, spoof(pb.Addr()), "own", false)
		conformance.ExpectReach(t, spoof(pa2.Addr()), "own", false)
		conformance.ExpectReach(t, spoof(pa.Addr().Next()), "own", false)
		// pb's own address, from pa, toward pb's silo: the reply would
		// go to pb, so the fence must drop it before it arrives.
		res, _ := pa.Spoof(pb.Addr(), conformance.Reach("as-b", pb.Addr().String(), 7000))
		conformance.ExpectReach(t, res, "as-b", false)
	})
}

// Never-looser: a child's isolation, ingress, egress, links and
// control are never looser than its parent's, whoever asks.
func TestNeverLooser(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F) {
		f.Mk("", "strict", 20, obj{
			"egress":       obj{"allow": []string{"example.com"}},
			"grant_policy": obj{"links": "deny"},
			"isolation":    obj{"allowed": []string{"sandbox"}},
			"control":      "read",
		})
		c := f.Mk("/strict", "child", 24, nil)
		path := "/silo/v1/silos/strict/child"
		for _, body := range []obj{
			{"egress": obj{"allow": []string{"github.com"}}},
			{"grant_policy": obj{"links": "allow"}},
			{"isolation": obj{"allowed": []string{"vm", "sandbox"}}},
			{"ingress": "host"},
			{"control": "full"},
		} {
			f.Refused(403, "LOOSER_THAN_PARENT", f.Root(), "PATCH", path, body)
		}
		f.Refused(403, "LOOSER_THAN_PARENT", f.Root(), "POST", "/silo/v1/silos/strict/children",
			obj{"name": "x", "address": obj{"prefix_length": 26}, "isolation": obj{"allowed": []string{"vm"}}})
		// The child can't loosen itself either.
		if code, out := f.API(c, "PATCH", "/silo/v1/self", obj{"egress": obj{"allow": []string{"*"}}}); code == 200 {
			t.Errorf("the child loosened its own egress: %v", out)
		}
	})
}

// Links: a link proposed by one side only opens nothing; by both, it
// opens its own port and no other, in its direction only; deleted, it
// closes again.
func TestLinks(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F) {
		tr := mkTree(f)
		pc, pb := f.Probe(t, tr.c, "pc"), f.Probe(t, tr.b, "pb")
		rules := []obj{{"protocol": "tcp", "ports": []string{"7000"}}}
		checks := []conformance.Check{
			{ID: "7000", Kind: "tcp", Host: pb.Addr().String(), Port: 7000},
			{ID: "7002", Kind: "tcp", Host: pb.Addr().String(), Port: 7002},
			{ID: "icmp", Kind: "icmp", Host: pb.Addr().String()},
		}
		back := []conformance.Check{{ID: "back", Kind: "tcp", Host: pc.Addr().String(), Port: 7000}}
		res := pc.Run(checks)
		conformance.Expect(t, res, "7000", false)

		l := f.OK(tr.c, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/b", "direction": "outbound", "rules": rules})
		if l["state"] != "PENDING" {
			t.Errorf("one side's link is %v, want PENDING", l["state"])
		}
		conformance.Expect(t, pc.Run(checks), "7000", false)

		l = f.OK(tr.b, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/a/c", "direction": "inbound", "rules": rules})
		if l["state"] != "ACTIVE" {
			t.Errorf("both sides' link is %v, want ACTIVE", l["state"])
		}
		res = pc.Run(checks)
		conformance.Expect(t, res, "7000", true)
		conformance.Expect(t, res, "7002", false)
		conformance.Expect(t, res, "icmp", false)
		conformance.Expect(t, pb.Run(back), "back", false)

		f.OK(tr.b, "DELETE", "/silo/v1/self/links/db", nil)
		conformance.Expect(t, pc.Run(checks), "7000", false)
	})
}

// Egress: the proxy passes allowed names and refuses denied ones and
// internal destinations; nothing reaches the metadata server, the
// host's loopback or the outside world except through it.
func TestEgress(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F) {
		s := f.Mk("", "e", 24, obj{"egress": obj{"allow": []string{"example.com"}}})
		p := f.Probe(t, s, "pe")
		proxy := "10.255.255.1:3128"
		res := p.Run([]conformance.Check{
			{ID: "allowed", Kind: "http", URL: "https://example.com/", Proxy: proxy},
			{ID: "denied", Kind: "http", URL: "https://github.com/", Proxy: proxy},
			{ID: "metadata-via-proxy", Kind: "http", URL: "http://169.254.169.254/", Proxy: proxy},
			{ID: "metadata", Kind: "tcp", Host: "169.254.169.254", Port: 80},
			{ID: "loopback", Kind: "tcp", Host: "127.0.0.1", Port: 22},
			{ID: "world", Kind: "tcp", Host: "93.184.216.34", Port: 443},
			{ID: "world-icmp", Kind: "icmp", Host: "8.8.8.8"},
		})
		status := func(id string, want int) {
			t.Helper()
			conformance.Expect(t, res, id, true)
			if got := conformance.HTTPStatus(res[id]); got != want {
				t.Errorf("%s: %d (%s), want %d", id, got, res[id].Detail, want)
			}
		}
		status("allowed", 200)
		status("denied", 403)
		status("metadata-via-proxy", 403)
		for _, id := range []string{"metadata", "loopback", "world", "world-icmp"} {
			conformance.Expect(t, res, id, false)
		}
	})
}

// Sealed and read silos, both layers: a sealed silo's probe can't
// reach the fence API's port, though DNS, HTTP and the egress proxy on
// the gateway still answer it, and its calls are refused with
// CONTROL_DENIED; a read silo reads but doesn't write.
func TestSealedAndReadSilos(t *testing.T) {
	eachFence(t, func(t *testing.T, f conformance.F) {
		sealed := f.Mk("", "s", 24, obj{"control": "none", "egress": obj{"allow": []string{"example.com"}}})
		ro := f.Mk("", "r", 24, obj{"control": "read"})
		open := f.Mk("", "o", 24, nil)
		gw := "10.255.255.1"
		checks := []conformance.Check{
			{ID: "api-port", Kind: "tcp", Host: gw, Port: 8080},
			{ID: "http-port", Kind: "tcp", Host: gw, Port: 80},
			{ID: "proxy-port", Kind: "tcp", Host: gw, Port: 3128},
			{ID: "dns-port", Kind: "udp", Host: gw, Port: 53},
		}
		res := f.Probe(t, sealed, "ps").Run(checks)
		conformance.Expect(t, res, "api-port", false)
		conformance.Expect(t, res, "http-port", true)
		conformance.Expect(t, res, "proxy-port", true)
		conformance.Expect(t, res, "dns-port", true)
		conformance.Expect(t, f.Probe(t, open, "po").Run(checks), "api-port", true)

		f.Refused(403, "CONTROL_DENIED", sealed, "GET", "/silo/v1/self", nil)
		f.Refused(403, "CONTROL_DENIED", sealed, "PATCH", "/silo/v1/self", obj{"labels": obj{"x": "y"}})
		f.OK(ro, "GET", "/silo/v1/self", nil)
		f.Refused(403, "CONTROL_DENIED", ro, "PATCH", "/silo/v1/self", obj{"labels": obj{"x": "y"}})
		f.Refused(403, "CONTROL_DENIED", ro, "POST", "/silo/v1/self/children", obj{"name": "x", "address": obj{"prefix_length": 28}})
		if r := f.DockerDo(ro, "POST", "/v1.47/networks/create", obj{"Name": "n"}); r.Code != 403 {
			t.Errorf("a read silo made a network: %d %s", r.Code, r.Body)
		}
		if r := f.DockerDo(sealed, "GET", "/v1.47/containers/json", nil); r.Code != 403 {
			t.Errorf("a sealed silo listed its containers: %d %s", r.Code, r.Body)
		}
	})
}
