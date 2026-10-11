package fenceapi_test

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
)

// TestLinks walks links through the fence API alone, so it runs on both
// backends (TestPostgres): the checks, a mirror image that isn't one,
// matching, the same proposal again, policy refusal and suspension, a
// manage holder's and an ancestor's calls on a silo's behalf, the audit
// kinds, and a deleted silo's links.
func TestLinks(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	allow := obj{"grant_policy": obj{"exports": "allow", "links": "allow"}}
	a := e.mk(e.root, "-"+e.root, "a", allow)
	b := e.mk(e.root, "-"+e.root, "b", allow)
	no := e.mk(e.root, "-"+e.root, "no", obj{"grant_policy": obj{"exports": "allow", "links": "deny"}})
	deploy := e.mk(e.root, "-"+e.root, "deploy", allow)
	pg := []obj{{"protocol": "tcp", "ports": []string{"5432"}}}
	propose := func(caller, path, name, peer, dir string, rules []obj) obj {
		return e.ok(caller, "POST", path, obj{"name": name, "peer": peer, "direction": dir, "rules": rules})
	}

	// Checks.
	for _, body := range []obj{
		{"name": "Bad_Name", "peer": "/b", "direction": "outbound", "rules": pg},
		{"name": "db", "peer": "/b", "direction": "sideways", "rules": pg},
		{"name": "db", "direction": "outbound", "rules": pg},
		{"name": "db", "peer": "/b", "direction": "outbound"},
		{"name": "db", "peer": "/b", "direction": "outbound", "rules": []obj{{"protocol": "gre"}}},
		{"name": "db", "peer": "/b", "direction": "outbound", "rules": []obj{{"protocol": "icmp", "ports": []string{"1"}}}},
		{"name": "db", "peer": "/b", "direction": "outbound", "rules": []obj{{"protocol": "tcp", "ports": []string{"0"}}}},
		{"name": "db", "peer": "/b", "direction": "outbound", "rules": []obj{{"protocol": "tcp", "ports": []string{"9-1"}}}},
		{"name": "db", "peer": "/b", "direction": "outbound", "rules": []obj{{"protocol": "all"}, {"protocol": "tcp"}}},
		{"name": "db", "peer": "/a", "direction": "outbound", "rules": pg}, // itself
	} {
		e.fail(fenceapi.InvalidArgument, a, "POST", "/silo/v1/self/links", body)
	}
	// The policy: no's effective grant policy doesn't allow links.
	e.fail(fenceapi.PermissionDenied, no, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/a", "direction": "outbound", "rules": pg})

	// A proposal to a path nobody holds is kept like any other.
	if l := propose(a, "/silo/v1/self/links", "ghost", "/no/such", "both", pg); l["state"] != "PENDING" {
		t.Errorf("to nobody: %v", l)
	}
	// b's proposal under the same name that isn't the mirror image
	// (another port; the same direction) matches nothing.
	propose(a, "/silo/v1/self/links", "db", "/b", "outbound", pg)
	if l := propose(b, "/silo/v1/self/links", "db", "/a", "outbound", pg); l["state"] != "PENDING" {
		t.Errorf("the same direction both sides: %v", l)
	}
	e.ok(b, "DELETE", "/silo/v1/self/links/db", nil)
	if l := propose(b, "/silo/v1/self/links", "db", "/a", "inbound", []obj{{"protocol": "tcp", "ports": []string{"5433"}}}); l["state"] != "PENDING" {
		t.Errorf("another port: %v", l)
	}
	e.ok(b, "DELETE", "/silo/v1/self/links/db", nil)
	// The mirror image, its ports spelled otherwise: ACTIVE, both sides,
	// with the same id.
	l := propose(b, "/silo/v1/self/links", "db", "a", "inbound", []obj{{"protocol": "tcp", "ports": []string{"5432-5432"}}})
	if l["state"] != "ACTIVE" || l["id"] == "" || len(l["fence_rules"].([]any)) != 2 {
		t.Fatalf("mirrored: %v", l)
	}
	la := e.ok(a, "GET", "/silo/v1/self/links", nil)["links"].([]any)
	var adb obj
	for _, x := range la {
		if x.(obj)["name"] == "db" {
			adb = x.(obj)
		}
	}
	if adb["state"] != "ACTIVE" || adb["id"] != l["id"] || adb["peer"] != "/b" || adb["direction"] != "outbound" {
		t.Errorf("a's side: %v", adb)
	}
	// The same again changes nothing; otherwise, it's refused.
	propose(a, "/silo/v1/self/links", "db", "/b", "outbound", pg)
	e.fail(fenceapi.AlreadyExists, a, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/b", "direction": "both", "rules": pg})
	for _, s := range []string{"a", "b"} {
		if n := len(e.records(e.root, s, "link.activate")); n != 1 {
			t.Errorf("%s: %d link.activate", s, n)
		}
	}

	// The root tightens b: suspended; loosened: active again, each
	// audited under both sides.
	e.ok(e.root, "PATCH", "/silo/v1/silos/b", obj{"grant_policy": obj{"links": "deny"}})
	if l := e.ok(a, "GET", "/silo/v1/self/links", nil)["links"].([]any); l[0].(obj)["state"] != "SUSPENDED" {
		t.Fatalf("after the policy change: %v", l)
	}
	e.ok(e.root, "PATCH", "/silo/v1/silos/b", allow)
	for _, s := range []string{"a", "b"} {
		if n := len(e.records(e.root, s, "grant.suspend")); n != 1 {
			t.Errorf("%s: %d grant.suspend", s, n)
		}
		if n := len(e.records(e.root, s, "grant.resume")); n != 1 {
			t.Errorf("%s: %d grant.resume", s, n)
		}
	}

	// A manage holder with grants proposes for b; one without can't. An
	// ancestor proposes for a child.
	g := e.ok(e.root, "POST", "/silo/v1/silos/b/manageGrants", obj{"name": "g", "holder": "/deploy", "scope": obj{"fence": []string{"read", "grants"}}, "lease": obj{"ttl": "3600s"}})
	e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	r := e.ok(e.root, "POST", "/silo/v1/silos/a/manageGrants", obj{"name": "r", "holder": "/deploy", "lease": obj{"ttl": "3600s"}})
	e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+r["id"].(string)+":accept", nil)
	er := e.fail(fenceapi.PermissionDenied, deploy, "POST", "/silo/v1/silos/a/links", obj{"name": "x", "peer": "/b", "direction": "both", "rules": pg})
	if get(er, "details", "needs") != "grants" {
		t.Errorf("without grants: %v", er)
	}
	propose(deploy, "/silo/v1/silos/b/links", "ssh", "/a", "both", []obj{{"protocol": "tcp", "ports": []string{"22"}}})
	recs := e.records(e.root, "b", "link.propose")
	if len(recs) == 0 || get(recs[len(recs)-1], "actor", "silo") != deploy || recs[len(recs)-1]["via"] != "manage:"+deploy {
		t.Errorf("propose through a manage grant: %v", recs)
	}
	if l := propose(e.root, "/silo/v1/silos/a/links", "ssh", "/b", "both", []obj{{"protocol": "tcp", "ports": []string{"22"}}}); l["state"] != "ACTIVE" {
		t.Errorf("on a's behalf: %v", l)
	}
	// A silo can't act for a sibling.
	e.fail(fenceapi.NotFound, a, "GET", "/silo/v1/silos/b/links", nil)

	// Deleting b ends its side: a's proposals are PENDING, and stay so.
	op := e.ok(e.root, "DELETE", "/silo/v1/silos/-"+b+"?force=true", nil)
	e.waitOp(e.root, op["name"].(string))
	for _, x := range e.ok(a, "GET", "/silo/v1/self/links", nil)["links"].([]any) {
		if x.(obj)["state"] != "PENDING" {
			t.Errorf("a's link after b went: %v", x)
		}
	}
	if n := len(e.records(e.root, "a", "link.delete")); n != 2 {
		t.Errorf("a: %d link.delete, want one for each matched link b's deletion ended", n)
	}
	// A new /b is another silo: it must propose for itself, and matching
	// a's old proposal takes a's proposing again.
	nb := e.mk(e.root, "-"+e.root, "b", allow)
	if l := propose(nb, "/silo/v1/self/links", "db", "/a", "inbound", pg); l["state"] != "PENDING" {
		t.Errorf("a new /b matched the old one's link: %v", l)
	}
}

// TestRootDeniesLinksByDefault: a root bootstrapped without a grant
// policy allows exports and denies links (README, call 14).
func TestRootDeniesLinksByDefault(t *testing.T) {
	e2 := newEnvBody(t, fenceapi.Options{}, func(b obj) { delete(b, "grant_policy") })
	d := e2.ok(e2.root, "GET", "/silo/v1/self", nil)
	if get(d, "grant_policy", "effective", "links") != "deny" || get(d, "grant_policy", "effective", "exports") != "allow" {
		t.Errorf("the root's default grant policy: %v", d["grant_policy"])
	}
	e2.fail(fenceapi.PermissionDenied, e2.root, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/x", "direction": "both", "rules": []obj{{"protocol": "all"}}})
}
