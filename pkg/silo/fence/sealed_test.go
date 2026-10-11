package fence_test

import (
	"net/netip"
	"testing"

	fe "github.com/bwsalmon/silo/pkg/silo/gce/fake/fenceeval"
)

// TestSealedSiloKeeps is the fence's half of gateway.md, "What a sealed
// silo keeps" (SV-82): a silo and its child make links and exports
// while they may, then the silo is sealed. With silod's applier
// running, the policy it leaves drops their packets to the API port
// and nothing else they had: egress on 3128, port 80 (proxied HTTP
// exports and agent tool channels), the TCP export ports, the gateway
// into an export's target, and both links. Their grants stay ACTIVE.
func TestSealedSiloKeeps(t *testing.T) {
	e := newEnvBoot(t, obj{"grant_policy": obj{"exports": "allow", "links": "allow"}})
	a := e.create(e.root, e.root, "a", 20, nil)
	sealed := e.create(e.root, e.root, "sealed", 20, nil)
	inner := e.create(e.root, sealed, "inner", 24, nil)
	ab, sb, ib := e.blocks(a)[0], e.blocks(sealed)[0], e.blocks(inner)[0]
	aVM, sVM, iVM := last(ab), last(sb), last(ib)
	if sb.Contains(netip.MustParseAddr(sVM)) && ib.Contains(netip.MustParseAddr(sVM)) {
		t.Fatalf("sealed's last address %s is inner's (%s)", sVM, ib)
	}
	gw := "10.0.255.2"

	// Links: a to sealed on 5432, inner to a on 6379.
	e.ok(a, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/sealed", "direction": "outbound", "rules": []obj{{"protocol": "tcp", "ports": []string{"5432"}}}})
	e.ok(sealed, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/a", "direction": "inbound", "rules": []obj{{"protocol": "tcp", "ports": []string{"5432"}}}})
	e.ok(inner, "POST", "/silo/v1/self/links", obj{"name": "cache", "peer": "/a", "direction": "outbound", "rules": []obj{{"protocol": "tcp", "ports": []string{"6379"}}}})
	e.ok(a, "POST", "/silo/v1/self/links", obj{"name": "cache", "peer": "/sealed/inner", "direction": "inbound", "rules": []obj{{"protocol": "tcp", "ports": []string{"6379"}}}})
	// Exports: sealed's HTTP one to a, and a's TCP one to inner.
	e.ok(sealed, "POST", "/silo/v1/self/exports", obj{"name": "web", "target": sVM + ":8080", "protocol": "http", "to": []string{"/a"}})
	e.ok(a, "POST", "/silo/v1/self/imports", obj{"export": "/sealed/web"})
	e.ok(a, "POST", "/silo/v1/self/exports", obj{"name": "db", "target": aVM + ":5432", "protocol": "tcp", "to": []string{"/sealed/inner"}})
	port := int(e.ok(inner, "POST", "/silo/v1/self/imports", obj{"export": "/a/db"})["port"].(float64))

	type pkt struct {
		p    fe.Packet
		want bool
	}
	keeps := []pkt{
		{pk("tcp", aVM, sVM, 5432), true},
		{pk("tcp", iVM, aVM, 6379), true},
		{pk("tcp", gw, sVM, 8080), true}, // into sealed's export
		{pk("tcp", gw, aVM, 5432), true}, // into a's, for inner
		{pk("tcp", aVM, sVM, 5433), false},
		{pk("tcp", gw, sVM, 8081), false},
	}
	for _, vm := range []string{sVM, iVM} {
		keeps = append(keeps,
			pkt{pk("tcp", vm, gw, 3128), true},
			pkt{pk("tcp", vm, gw, 80), true},
			pkt{pk("tcp", vm, gw, port), true})
	}
	check := func(when string, ps []pkt) {
		t.Helper()
		ev := e.open()
		for _, c := range ps {
			if got := ev.Allowed(c.p); got != c.want {
				v, _ := ev.Evaluate(c.p)
				t.Errorf("%s: %s: allowed %v, want %v: %s", when, c.p, got, c.want, v)
			}
		}
	}
	check("before sealing", append(keeps, pkt{pk("tcp", sVM, gw, 8080), true}, pkt{pk("tcp", iVM, gw, 8080), true}))

	// Sealed: the API port goes, for sealed and for inner, whose own
	// policy is still full; a keeps it; everything else stays.
	e.ok(e.root, "PATCH", "/silo/v1/silos/-"+sealed, obj{"control": "none"})
	rs, _ := e.rules()
	for _, id := range []string{sealed, inner} {
		if rs[id+"-api-out"] != nil {
			t.Errorf("%s has an api-out once sealed", id)
		}
	}
	check("sealed", append(keeps,
		pkt{pk("tcp", sVM, gw, 8080), false},
		pkt{pk("tcp", iVM, gw, 8080), false},
		pkt{pk("tcp", aVM, gw, 8080), true}))

	// Every grant is still ACTIVE.
	for _, c := range []struct{ ref, list string }{
		{"-" + sealed, "links"}, {"-" + inner, "links"}, {"-" + a, "links"},
		{"-" + a, "imports"}, {"-" + inner, "imports"},
	} {
		items, _ := e.ok(e.root, "GET", "/silo/v1/silos/"+c.ref+"/"+c.list, nil)[c.list].([]any)
		if len(items) == 0 {
			t.Errorf("%s has no %s", c.ref, c.list)
		}
		for _, it := range items {
			if it.(obj)["state"] != "ACTIVE" {
				t.Errorf("%s's %s: %v", c.ref, c.list, it)
			}
		}
	}
}
