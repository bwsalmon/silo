package fenceapi_test

import (
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// TestManageGrants walks a manage grant through its states on the
// fence API alone, so it runs on both backends (TestPostgres). The
// facade's side, and the rest of testing.md's cases, are in
// pkg/silo/facade/gcp/gce's manage tests.
func TestManageGrants(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	prod := e.mk(e.root, "-"+e.root, "prod", nil)
	web := e.mk(e.root, "prod", "web", nil)
	deploy := e.mk(e.root, "-"+e.root, "deploy", nil)
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	g := e.ok(e.root, "POST", "/silo/v1/silos/prod/manageGrants", obj{"name": "rollout", "holder": "/deploy",
		"scope": obj{"facade": obj{"resources": []string{"instances"}, "access": "write"}, "fence": []string{"read", "grants"}},
		"lease": obj{"ttl": "86400s", "renew_until": until}})
	id := g["id"].(string)
	if g["state"] != "PENDING" || get(g, "lease", "ttl") != "86400s" {
		t.Fatalf("offer: %v", g)
	}
	// PENDING gives nothing.
	e.fail(fenceapi.NotFound, deploy, "GET", "/silo/v1/silos/prod", nil)
	l := e.ok(deploy, "GET", "/silo/v1/self/manageGrants", nil)["manage_grants"].([]any)
	if len(l) != 1 || l[0].(obj)["target_path"] != "/prod" {
		t.Fatalf("held: %v", l)
	}
	e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+id+":accept", nil)
	e.ok(deploy, "GET", "/silo/v1/silos/prod", nil)
	e.ok(deploy, "GET", "/silo/v1/silos/prod/web", nil)
	e.ok(deploy, "GET", "/silo/v1/silos/prod/audit", nil)
	er := e.fail(fenceapi.PermissionDenied, deploy, "POST", "/silo/v1/silos/prod/children", obj{"name": "x", "address": obj{"prefix_length": 28}})
	if get(er, "details", "grant") != "rollout" || get(er, "details", "needs") != "children" {
		t.Errorf("refusal: %v", er)
	}
	e.fail(fenceapi.PermissionDenied, deploy, "PUT", "/silo/v1/silos/prod/web/members/user:x@example.com", obj{})
	e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+id+":renew", nil)
	// A descendant of the target can't be a holder; nor the target's
	// own parent.
	e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/silos/prod/manageGrants", obj{"name": "x", "holder": "/prod/web", "lease": obj{"ttl": "60s"}})
	e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/silos/prod/web/manageGrants", obj{"name": "x", "holder": "/prod", "lease": obj{"ttl": "60s"}})

	// Expiry by database time.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE manage_grants SET expires_at = ? WHERE id = ?`, store.Millis(time.Now().Add(-time.Second)), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.fail(fenceapi.NotFound, deploy, "GET", "/silo/v1/silos/prod", nil)
	if n, err := fenceapi.ExpireManageGrants(ctx, e.db); n != 1 || err != nil {
		t.Fatalf("expire: %d %v", n, err)
	}
	l = e.ok(e.root, "GET", "/silo/v1/silos/prod/manageGrants", nil)["manage_grants"].([]any)
	if len(l) != 1 || l[0].(obj)["state"] != "EXPIRED" {
		t.Errorf("after expiry: %v", l)
	}

	// A new grant, revoked by an ancestor; then the target deleted.
	g = e.ok(e.root, "POST", "/silo/v1/silos/prod/web/manageGrants", obj{"name": "w", "holder": "/deploy", "lease": obj{"ttl": "60s"}})
	e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	e.ok(deploy, "GET", "/silo/v1/silos/-"+web, nil)
	e.ok(e.root, "DELETE", "/silo/v1/silos/prod/web/manageGrants/w", nil)
	e.fail(fenceapi.NotFound, deploy, "GET", "/silo/v1/silos/-"+web, nil)
	recs := e.ok(deploy, "GET", "/silo/v1/self/audit?kind=manage.*", nil)["records"].([]any)
	if len(recs) < 3 {
		t.Errorf("the holder's audit shows %d manage records: %v", len(recs), recs)
	}
	e.ok(e.root, "DELETE", "/silo/v1/silos/-"+prod+"?force=true", nil)
	if l := e.ok(deploy, "GET", "/silo/v1/self/manageGrants", nil)["manage_grants"].([]any); len(l) != 0 {
		t.Errorf("held after the target went: %v", l)
	}
}
