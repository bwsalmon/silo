package fenceapi_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// addrIn is the n-th address of silo's first block.
func (e *env) addrIn(silo string, n int) netip.Addr {
	e.t.Helper()
	var bs []netip.Prefix
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		bs, err = address.Blocks(ctx, tx, silo)
		return err
	}); err != nil || len(bs) == 0 {
		e.t.Fatalf("blocks of %s: %v %v", silo, bs, err)
	}
	a := bs[0].Addr()
	for range n {
		a = a.Next()
	}
	return a
}

func (e *env) records(caller, silo, kind string) []obj {
	e.t.Helper()
	l, _ := e.ok(caller, "GET", "/silo/v1/silos/"+silo+"/audit?kind="+kind, nil)["records"].([]any)
	var out []obj
	for _, r := range l {
		out = append(out, r.(obj))
	}
	return out
}

// TestExports walks exports through the fence API alone, so it runs on
// both backends (TestPostgres): offers and their checks, accepts,
// suspension by policy, the three withdrawals, calls on a descendant's
// behalf and through a manage grant, and a deleted silo's grants.
func TestExports(t *testing.T) {
	allow := obj{"grant_policy": obj{"exports": "allow"}}
	e := newEnv(t, fenceapi.Options{Instances: func(_ context.Context, silo, zone, name string) (netip.Addr, error) {
		if name != "cache" {
			return netip.Addr{}, errors.New("no such instance")
		}
		return e0addr, nil
	}})
	team := e.mk(e.root, "-"+e.root, "team", allow)
	ci := e.mk(e.root, "team", "ci", allow)
	other := e.mk(e.root, "-"+e.root, "other", obj{"grant_policy": obj{"exports": "deny"}})
	deploy := e.mk(e.root, "-"+e.root, "deploy", allow)
	target := netip.AddrPortFrom(e.addrIn(other, 300), 8080).String()
	e0addr = e.addrIn(team, 400)

	// other's policy doesn't allow exports.
	e.fail(fenceapi.PermissionDenied, other, "POST", "/silo/v1/self/exports", obj{"name": "db", "target": target, "protocol": "tcp", "to": []string{"/team/ci"}})
	// Checks: name, protocol, to, target in own space only.
	for _, body := range []obj{
		{"name": "Bad_Name", "target": target, "protocol": "http", "to": []string{"/team"}},
		{"name": "x", "target": target, "protocol": "udp", "to": []string{"/team"}},
		{"name": "x", "target": target, "protocol": "http"},
		{"name": "x", "target": target, "protocol": "http", "to": []string{"/team"}, "proxy_protocol": true},
		{"name": "x", "target": "10.0.0.1", "protocol": "http", "to": []string{"/team"}},
		{"name": "x", "target": netip.AddrPortFrom(e.addrIn(team, 5), 80).String(), "protocol": "http", "to": []string{"/team"}}, // team's, not the root's own
		{"name": "x", "target": obj{"instance": "cache"}, "protocol": "http", "to": []string{"/team"}},
	} {
		e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/self/exports", body)
	}
	// A silo can't offer on its parent's behalf, nor on a sibling's.
	e.fail(fenceapi.NotFound, ci, "POST", "/silo/v1/silos/team/exports", obj{"name": "x", "target": target, "protocol": "http", "to": []string{"/other"}})
	e.fail(fenceapi.NotFound, deploy, "POST", "/silo/v1/silos/team/exports", obj{"name": "x", "target": target, "protocol": "http", "to": []string{"/other"}})

	// An instance target, resolved.
	ex := e.ok(team, "POST", "/silo/v1/self/exports", obj{"name": "cache", "target": obj{"instance": "cache", "zone": "us-central1-a", "port": 3128},
		"protocol": "tcp", "to": []string{"/team/ci", "/no/such/silo"}})
	if get(ex, "target", "instance") != "cache" || ex["address_ip"] != netip.AddrPortFrom(e0addr, 3128).String() || ex["owner"] != "tenant" {
		t.Errorf("instance export: %v", ex)
	}
	// Offered again, the same: more silos; to another target: refused.
	e.ok(team, "POST", "/silo/v1/self/exports", obj{"name": "cache", "target": obj{"instance": "cache", "zone": "us-central1-a", "port": 3128},
		"protocol": "tcp", "to": []string{"/other"}})
	e.fail(fenceapi.AlreadyExists, team, "POST", "/silo/v1/self/exports", obj{"name": "cache", "target": netip.AddrPortFrom(e0addr, 1).String(), "protocol": "tcp", "to": []string{"/other"}})
	l := e.ok(team, "GET", "/silo/v1/self/exports", nil)["exports"].([]any)
	if len(l) != 1 || len(l[0].(obj)["to"].([]any)) != 3 {
		t.Fatalf("exports: %v", l)
	}

	// other's policy doesn't allow it to accept; ci's does.
	e.fail(fenceapi.PermissionDenied, other, "POST", "/silo/v1/self/imports", obj{"export": "/team/cache"})
	e.fail(fenceapi.NotFound, deploy, "POST", "/silo/v1/self/imports", obj{"export": "/team/cache"})
	im := e.ok(ci, "POST", "/silo/v1/self/imports", obj{"export": "/team/cache"})
	if im["state"] != "ACTIVE" || im["port"] != float64(20000) {
		t.Fatalf("accept: %v", im)
	}
	if l := e.ok(ci, "GET", "/silo/v1/self/offers", nil)["offers"].([]any); len(l) != 0 {
		t.Errorf("an accepted offer is still listed: %v", l)
	}

	// The root tightens team: team's export, and so ci's import, is
	// suspended; then loosened, resumed. Both audited under both sides.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team", obj{"grant_policy": obj{"exports": "deny"}})
	if l := e.ok(ci, "GET", "/silo/v1/self/imports", nil)["imports"].([]any); len(l) != 1 || l[0].(obj)["state"] != "SUSPENDED" {
		t.Fatalf("after the policy change: %v", l)
	}
	e.ok(e.root, "PATCH", "/silo/v1/silos/team", allow)
	if l := e.ok(ci, "GET", "/silo/v1/self/imports", nil)["imports"].([]any); len(l) != 1 || l[0].(obj)["state"] != "ACTIVE" {
		t.Fatalf("after loosening: %v", l)
	}
	for _, s := range []string{"team", "team/ci"} {
		if n := len(e.records(e.root, s, "grant.suspend")); n != 1 {
			t.Errorf("%s: %d grant.suspend", s, n)
		}
		if n := len(e.records(e.root, s, "grant.resume")); n != 1 {
			t.Errorf("%s: %d grant.resume", s, n)
		}
	}

	// A manage holder with grants acts for the target; one without
	// can't.
	g := e.ok(e.root, "POST", "/silo/v1/silos/team/manageGrants", obj{"name": "g", "holder": "/deploy", "scope": obj{"fence": []string{"read", "grants"}}, "lease": obj{"ttl": "3600s"}})
	e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	r := e.ok(e.root, "POST", "/silo/v1/silos/other/manageGrants", obj{"name": "r", "holder": "/deploy", "lease": obj{"ttl": "3600s"}})
	e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+r["id"].(string)+":accept", nil)
	er := e.fail(fenceapi.PermissionDenied, deploy, "POST", "/silo/v1/silos/other/exports", obj{"name": "x", "target": target, "protocol": "http", "to": []string{"/team"}})
	if get(er, "details", "needs") != "grants" {
		t.Errorf("without grants: %v", er)
	}
	e.ok(deploy, "GET", "/silo/v1/silos/team/ci/imports", nil)
	e.ok(deploy, "POST", "/silo/v1/silos/team/ci/exports", obj{"name": "logs", "target": netip.AddrPortFrom(e.addrIn(ci, 7), 80).String(), "protocol": "http", "to": []string{"/team"}})
	recs := e.records(e.root, "team/ci", "export.offer")
	if len(recs) != 1 || get(recs[0], "actor", "silo") != deploy || recs[0]["via"] != "manage:"+deploy {
		t.Errorf("offer through a manage grant: %v", recs)
	}
	// The root accepts on team's behalf: as if team had.
	im2 := e.ok(e.root, "POST", "/silo/v1/silos/team/imports", obj{"export": "/team/ci/logs"})
	if im2["address"] != "http://silo.internal/x/"+ci+"/logs/" {
		t.Errorf("accept on team's behalf: %v", im2)
	}

	// Withdrawing for one silo, then the importer giving up, then the
	// whole export.
	e.fail(fenceapi.NotFound, team, "DELETE", "/silo/v1/self/exports/cache/to/nobody", nil)
	e.ok(team, "DELETE", "/silo/v1/self/exports/cache/to/no/such/silo", nil)
	if l := e.ok(team, "GET", "/silo/v1/self/exports", nil)["exports"].([]any); len(l[0].(obj)["to"].([]any)) != 2 {
		t.Errorf("after withdrawing for one: %v", l)
	}
	e.fail(fenceapi.NotFound, team, "DELETE", "/silo/v1/self/imports/"+im["id"].(string), nil) // not team's
	e.ok(ci, "DELETE", "/silo/v1/self/imports/"+im["id"].(string), nil)
	if l := e.ok(ci, "GET", "/silo/v1/self/offers", nil)["offers"].([]any); len(l) != 1 {
		t.Errorf("the offer after giving up the import: %v", l)
	}
	e.ok(team, "DELETE", "/silo/v1/self/exports/cache", nil)
	if l := e.ok(ci, "GET", "/silo/v1/self/offers", nil)["offers"].([]any); len(l) != 0 {
		t.Errorf("offers after the withdrawal: %v", l)
	}
	// For one silo, by the importer, for everyone; refusals are audited
	// too, as every refused mutation is.
	okWithdraw := 0
	for _, r := range e.records(e.root, "team", "export.withdraw") {
		if r["result"] == "OK" {
			okWithdraw++
		}
	}
	if okWithdraw != 3 {
		t.Errorf("team's export.withdraw records: %d OK, want 3", okWithdraw)
	}

	// Deleting the exporter ends its exports and their imports.
	op := e.ok(e.root, "DELETE", "/silo/v1/silos/-"+ci+"?force=true", nil)
	e.waitOp(e.root, op["name"].(string))
	if l := e.ok(team, "GET", "/silo/v1/self/imports", nil)["imports"].([]any); len(l) != 0 {
		t.Errorf("team's imports after the exporter went: %v", l)
	}
}

var e0addr netip.Addr
