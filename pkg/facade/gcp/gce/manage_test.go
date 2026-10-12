package gce

import (
	"fmt"
	"strings"
	"testing"
	"time"

	gceapi "github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	compute "google.golang.org/api/compute/v1"
)

// The manage-grant cases of docs/silo/testing.md ("Cases", "manage
// grants") that run against the fake: / offers /team-b a grant on
// /team-a, and /team-b acts on /team-a's facade and fence through it.

// injectDiskInsert serves disks.insert for the test, as the disks write
// path will: charge the target, insert, audit, and follow the
// operation.
func injectDiskInsert(t *testing.T) {
	realInsert, realFields := handlers["disks.insert"], requestFields["disks.insert"]
	handlers["disks.insert"] = func(s *Server, q *request) (any, error) {
		name, _ := q.body["name"].(string)
		if err := q.n.checkName(name); err != nil {
			return nil, err
		}
		size, _ := q.body["sizeGb"].(string)
		var gb int64
		fmt.Sscan(size, &gb)
		object := "disk:" + q.call.Zone + "/" + name
		amount := []ledger.Amount{{Key: ledger.Key{Kind: "gce", Class: "ssd_disk_gb", Region: gceapi.ZoneRegion(q.call.Zone)}, Amount: ledger.Units(gb)}}
		err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
			return ledger.Charge(q.ctx(), tx, ledger.ChargeRequest{Object: object, Silo: q.target.ID, Via: q.via(), Amounts: amount})
		})
		if err != nil {
			return nil, err
		}
		op, err := s.gce.InsertDisk(q.ctx(), s.opts.HostProject, q.call.Zone, &compute.Disk{Name: name, SizeGb: gb,
			Type: "zones/" + q.call.Zone + "/diskTypes/pd-ssd", Labels: map[string]string{"silo": q.target.ID, "silo-managed": "true"}})
		if err != nil {
			return nil, err
		}
		s.audit(q, "OK", nil, []fenceapi.ChargeLine{{Payer: q.target.ID, Home: q.target.ID, Via: q.via(), Kind: "gce", Class: "ssd_disk_gb", Region: "us-central1", Amount: gb}})
		return s.started(q, op, object, nil)
	}
	requestFields["disks.insert"] = []string{"name", "sizeGb", "labels"}
	t.Cleanup(func() { handlers["disks.insert"], requestFields["disks.insert"] = realInsert, realFields })
}

// fenceFail makes a fence API call that must fail with code, and
// returns the error.
func (e *env) fenceFail(code fenceapi.Code, caller, method, path string, body any) obj {
	e.t.Helper()
	status, out := e.do(caller, method, path, body)
	er, _ := out["error"].(obj)
	if er == nil || er["code"] != string(code) || status != code.Status() {
		e.t.Fatalf("%s %s as %s: %d %v, want %s", method, path, caller, status, out, code)
	}
	return er
}

func (e *env) offer(target, name, holder string, scope obj, lease obj) obj {
	e.t.Helper()
	return e.ok(e.root, "POST", "/silo/v1/silos/"+target+"/manageGrants", obj{"name": name, "holder": holder, "scope": scope, "lease": lease})
}

func (e *env) held(caller string) []obj {
	e.t.Helper()
	var out []obj
	for _, g := range e.ok(caller, "GET", "/silo/v1/self/manageGrants", nil)["manage_grants"].([]any) {
		out = append(out, g.(obj))
	}
	return out
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if err := e.db.Tx(ctx, func(tx store.Tx) error { _, err := tx.Exec(ctx, q, args...); return err }); err != nil {
		e.t.Fatal(err)
	}
}

var rolloutScope = obj{"facade": obj{"resources": []string{"instances", "disks"}, "access": "write"}, "fence": []string{"read"}}

func TestManageGrant(t *testing.T) {
	injectDiskInsert(t)
	e := newEnv(t)
	used := func() float64 {
		r := e.ok(e.a, "GET", cv(host, "regions/us-central1"), nil)
		_, u, _ := quotaOf(r, "SSD_TOTAL_GB")
		return u
	}
	g := e.offer("team-a", "rollout", "/team-b", rolloutScope, obj{"ttl": "3600s"})
	if g["state"] != "PENDING" || g["id"] == "" || g["target_path"] != "/team-a" {
		t.Fatalf("offer: %v", g)
	}
	id := g["id"].(string)
	// Names are unique per target.
	e.fenceFail(fenceapi.AlreadyExists, e.root, "POST", "/silo/v1/silos/team-a/manageGrants",
		obj{"name": "rollout", "holder": "/team-b", "lease": obj{"ttl": "60s"}})

	// Before accepting, /team-b sees /team-a as NOT_FOUND, and its
	// facade as 403.
	e.fenceFail(fenceapi.NotFound, e.b, "GET", "/silo/v1/silos/team-a", nil)
	e.fail(403, "forbidden", e.b, "GET", cv("silo-"+e.a, "regions/us-central1"), nil)
	l := e.held(e.b)
	if len(l) != 1 || l[0]["id"] != id || l[0]["state"] != "PENDING" || l[0]["target_path"] != "/team-a" {
		t.Fatalf("held before accept: %v", l)
	}
	// Someone else's grant is NOT_FOUND to it.
	e.fenceFail(fenceapi.NotFound, e.ci, "POST", "/silo/v1/self/manageGrants/"+id+":accept", nil)

	acc := e.ok(e.b, "POST", "/silo/v1/self/manageGrants/"+id+":accept", nil)
	if acc["state"] != "ACTIVE" || acc["expire_time"] == nil {
		t.Fatalf("accept: %v", acc)
	}

	// Now /team-b sees /team-a and its descendants, and its facade.
	if d := e.ok(e.b, "GET", "/silo/v1/silos/team-a", nil); d["path"] != "/team-a" {
		t.Errorf("describe: %v", d)
	}
	e.ok(e.b, "GET", "/silo/v1/silos/team-a/ci", nil)
	e.ok(e.b, "GET", "/silo/v1/silos/team-a/children", nil)
	e.ok(e.b, "GET", cv("silo-"+e.a, "regions/us-central1"), nil)
	e.ok(e.b, "GET", cv("silo-"+e.a, "zones"), nil)

	// It creates a disk in silo-<team-a>: team-a's, charged to team-a,
	// audited under team-a with via=manage:<team-b>.
	op := e.ok(e.b, "POST", cv("silo-"+e.a, "zones/us-central1-a/disks"), obj{"name": e.a + "-d1", "sizeGb": "10"})
	// The objects are team-a's, so carry team-a's prefix: the holder's
	// own, or a bare name, is refused naming team-a's.
	for _, name := range []string{e.b + "-d1", "d1"} {
		msg := e.fail(400, "invalid", e.b, "POST", cv("silo-"+e.a, "zones/us-central1-a/disks"), obj{"name": name, "sizeGb": "10"})
		if !strings.Contains(msg, `must start with "`+e.a+`-"`) {
			t.Errorf("%s through the grant: %q", name, msg)
		}
	}
	if used() != 10 {
		t.Errorf("team-a used %v after the holder's insert", used())
	}
	if op["user"] != "silo:"+e.b {
		t.Errorf("operation user %v", op["user"])
	}
	recs := e.audit(e.a, KindFacadeCall)
	last := recs[len(recs)-1]
	if last["via"] != "manage:"+e.b || last["actor"].(obj)["silo"] != e.b || last["result"] != "OK" ||
		last["fields"].(obj)["charges"].([]any)[0].(obj)["via"] != "manage:"+e.b {
		t.Errorf("audit: %v", last)
	}
	// The holder's own audit shows the same record.
	mine := e.ok(e.b, "GET", "/silo/v1/self/audit?kind="+KindFacadeCall, nil)["records"].([]any)
	if len(mine) != 1 || mine[0].(obj)["id"] != last["id"] {
		t.Errorf("holder's audit: %v", mine)
	}
	e.gce.RunOperations()
	opPath := "zones/us-central1-a/operations/" + op["name"].(string)
	e.ok(e.b, "POST", cv("silo-"+e.a, opPath+"/wait"), nil)

	// Out of scope: a subnet or a firewall rule (403 naming the grant).
	for _, c := range []struct{ method, path string }{
		{"POST", "regions/us-central1/subnetworks"},
		{"POST", "global/firewalls"},
		{"GET", "global/firewalls"},
	} {
		msg := e.fail(403, "forbidden", e.b, c.method, cv("silo-"+e.a, c.path), obj{"name": e.a + "-x"})
		if !strings.Contains(msg, `"rollout"`) || !strings.Contains(msg, id) {
			t.Errorf("%s %s: %q doesn't name the grant", c.method, c.path, msg)
		}
	}
	// ... and on the fence: a PATCH of the target's quota, a child of
	// it, a member mapped to it, its control, and a manage offer.
	for _, c := range []struct {
		method, path string
		body         obj
	}{
		{"PATCH", "/silo/v1/silos/team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 9}}}},
		{"PATCH", "/silo/v1/silos/team-a", obj{"control": "none"}},
		{"PATCH", "/silo/v1/silos/team-a/ci", obj{"control": "read"}},
		{"POST", "/silo/v1/silos/team-a/children", obj{"name": "x", "address": obj{"prefix_length": 28}}},
		{"DELETE", "/silo/v1/silos/team-a", nil},
		{"PUT", "/silo/v1/silos/team-a/members/user:x@example.com", obj{}},
		{"GET", "/silo/v1/silos/team-a/members", nil},
		{"POST", "/silo/v1/silos/team-a/manageGrants", obj{"name": "again", "holder": "/ro", "lease": obj{"ttl": "60s"}}},
		{"POST", "/silo/v1/silos/team-a/ci/manageGrants", obj{"name": "again", "holder": "/ro", "lease": obj{"ttl": "60s"}}},
		{"DELETE", "/silo/v1/silos/team-a/manageGrants/rollout", nil},
		{"GET", "/silo/v1/silos/team-a/manageGrants", nil},
	} {
		er := e.fenceFail(fenceapi.PermissionDenied, e.b, c.method, c.path, c.body)
		if d, _ := er["details"].(obj); d["grant"] != "rollout" || d["grant_id"] != id {
			t.Errorf("%s %s: details %v", c.method, c.path, er)
		}
	}
	// A read holder is still a read holder: the target is unchanged.
	if d := e.ok(e.root, "GET", "/silo/v1/silos/team-a", nil); d["control"].(obj)["own"] != "full" {
		t.Errorf("team-a after the refusals: %v", d["control"])
	}

	// Revoking: an in-flight apply from /team-b gets 403 on its next
	// call, and nothing it hadn't started appears. The operation it had
	// started runs to completion, and silod finishes its bookkeeping.
	op2 := e.ok(e.b, "POST", cv("silo-"+e.a, "zones/us-central1-a/disks"), obj{"name": e.a + "-d2", "sizeGb": "10"})
	op2Path := "zones/us-central1-a/operations/" + op2["name"].(string)
	rv := e.ok(e.root, "DELETE", "/silo/v1/silos/team-a/manageGrants/rollout", nil)
	if rv["state"] != "REVOKED" {
		t.Errorf("revoke: %v", rv)
	}
	e.fail(403, "forbidden", e.b, "GET", cv("silo-"+e.a, op2Path), nil)
	e.fail(403, "forbidden", e.b, "POST", cv("silo-"+e.a, "zones/us-central1-a/disks"), obj{"name": e.a + "-d3", "sizeGb": "10"})
	e.fenceFail(fenceapi.NotFound, e.b, "GET", "/silo/v1/silos/team-a", nil)
	if _, err := e.gce.GetDisk(ctx, host, "us-central1-a", e.a+"-d3"); err == nil {
		t.Error("d3 appeared after the revoke")
	}
	e.gce.RunOperations()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var ops []Operation
		e.db.Tx(ctx, func(tx store.Tx) error { var err error; ops, err = Operations(ctx, tx, e.a); return err })
		done := 0
		for _, o := range ops {
			if o.Done {
				done++
			}
		}
		if done == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the started operation's bookkeeping didn't finish: %+v", ops)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := e.gce.GetDisk(ctx, host, "us-central1-a", e.a+"-d2"); err != nil {
		t.Errorf("d2, started before the revoke: %v", err)
	}
	if used() != 20 {
		t.Errorf("team-a used %v", used())
	}
	// The revoke is in the change log for the gateway, and audited.
	var data string
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT data FROM changes WHERE kind = ? AND key = ? ORDER BY seq DESC LIMIT 1`, fenceapi.ChangeKindManage, id).Scan(&data)
	}); err != nil || !strings.Contains(data, `"state":"REVOKED"`) || !strings.Contains(data, `"ended":true`) {
		t.Errorf("change log: %q %v", data, err)
	}
	for _, k := range []string{fenceapi.KindManageOffer, fenceapi.KindManageAccept, fenceapi.KindManageRevoke} {
		n := 0
		for _, r := range e.audit(e.a, k) {
			if r["result"] == "OK" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d OK %s records under team-a, want 1", n, k)
		}
	}
	if len(e.held(e.b)) != 0 {
		t.Errorf("held after revoke: %v", e.held(e.b))
	}
	// The name can be offered again once the grant has ended.
	e.offer("team-a", "rollout", "/team-b", nil, obj{"ttl": "60s"})
}

func TestManageOfferChecks(t *testing.T) {
	e := newEnv(t)
	path := "/silo/v1/silos/team-a/manageGrants"
	for _, b := range []obj{
		// A grant to the target, a descendant, or an ancestor.
		{"name": "g", "holder": "/team-a", "lease": obj{"ttl": "60s"}},
		{"name": "g", "holder": "/team-a/ci", "lease": obj{"ttl": "60s"}},
		{"name": "g", "holder": "/", "lease": obj{"ttl": "60s"}},
		// The lease: required, at most 30 days.
		{"name": "g", "holder": "/team-b"},
		{"name": "g", "holder": "/team-b", "lease": obj{"ttl": "2592001s"}},
		{"name": "g", "holder": "/team-b", "lease": obj{"ttl": "60s", "renew_until": "2001-01-01T00:00:00Z"}},
		// The scope.
		{"name": "g", "holder": "/team-b", "lease": obj{"ttl": "60s"}, "scope": obj{"facade": obj{"resources": []string{"routes"}, "access": "write"}}},
		{"name": "g", "holder": "/team-b", "lease": obj{"ttl": "60s"}, "scope": obj{"facade": obj{"resources": []string{"disks"}, "access": "admin"}}},
		{"name": "g", "holder": "/team-b", "lease": obj{"ttl": "60s"}, "scope": obj{"fence": []string{"control"}}},
		{"name": "G!", "holder": "/team-b", "lease": obj{"ttl": "60s"}},
	} {
		e.fenceFail(fenceapi.InvalidArgument, e.root, "POST", path, b)
	}
	// A child of /team-a offering a grant to its sibling is refused too:
	// a cell can't be given the means to change itself.
	e.fenceFail(fenceapi.InvalidArgument, e.a, "POST", "/silo/v1/silos/team-a/ci/manageGrants",
		obj{"name": "g", "holder": "/team-a/ci", "lease": obj{"ttl": "60s"}})
	// Only a proper ancestor offers: not the target itself, nor a silo
	// that can't see it.
	e.fenceFail(fenceapi.PermissionDenied, e.a, "POST", path, obj{"name": "g", "holder": "/team-b", "lease": obj{"ttl": "60s"}})
	e.fenceFail(fenceapi.NotFound, e.b, "POST", path, obj{"name": "g", "holder": "/ro", "lease": obj{"ttl": "60s"}})
	// Nothing is revealed about whether the holder exists.
	g := e.offer("team-a", "nobody", "/no/such/silo", nil, obj{"ttl": "60s"})
	if g["state"] != "PENDING" || g["holder"] != "/no/such/silo" {
		t.Errorf("offer to a missing holder: %v", g)
	}
	// An empty scope is fence read only; read is always in it.
	if s := g["scope"].(obj); fmt.Sprint(s["fence"]) != "[read]" || s["facade"] != nil {
		t.Errorf("empty scope: %v", s)
	}
	// Listing is an ancestor's: the target itself may not.
	if l := e.ok(e.root, "GET", path, nil)["manage_grants"].([]any); len(l) != 1 {
		t.Errorf("list: %v", l)
	}
	e.fenceFail(fenceapi.PermissionDenied, e.a, "GET", path, nil)
	e.fenceFail(fenceapi.NotFound, e.root, "DELETE", path+"/missing", nil)
	// An offerer under read can't offer: a mutation.
	e.fenceFail(fenceapi.ControlDenied, e.ro, "POST", "/silo/v1/self/manageGrants/x:accept", nil)
}

func TestManageLease(t *testing.T) {
	e := newEnv(t)
	renewUntil := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	g := e.offer("team-a", "short", "/team-b", rolloutScope, obj{"ttl": "3600s", "renew_until": renewUntil})
	id := g["id"].(string)
	// PENDING can't be renewed.
	e.fenceFail(fenceapi.FailedPrecondition, e.b, "POST", "/silo/v1/self/manageGrants/"+id+":renew", nil)
	acc := e.ok(e.b, "POST", "/silo/v1/self/manageGrants/"+id+":accept", nil)
	first, _ := time.Parse(time.RFC3339, acc["expire_time"].(string))
	if d := time.Until(first); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("expire_time %v is not an hour from accepting", first)
	}
	// Renewing restarts the ttl.
	e.exec(`UPDATE manage_grants SET expires_at = ? WHERE id = ?`, store.Millis(time.Now().Add(time.Minute)), id)
	rn := e.ok(e.b, "POST", "/silo/v1/self/manageGrants/"+id+":renew", nil)
	if exp, _ := time.Parse(time.RFC3339, rn["expire_time"].(string)); time.Until(exp) < 59*time.Minute {
		t.Errorf("renew: %v", rn)
	}
	// Past renew_until, renewal is refused.
	e.exec(`UPDATE manage_grants SET renew_until = ? WHERE id = ?`, store.Millis(time.Now().Add(-time.Second)), id)
	e.fenceFail(fenceapi.FailedPrecondition, e.b, "POST", "/silo/v1/self/manageGrants/"+id+":renew", nil)

	// Expiry, by database time, behaves as a revoke: the next call is
	// refused, the sweep marks it EXPIRED, audits and logs it.
	e.ok(e.b, "GET", cv("silo-"+e.a, "zones"), nil)
	e.exec(`UPDATE manage_grants SET expires_at = ? WHERE id = ?`, store.Millis(time.Now().Add(-time.Second)), id)
	e.fail(403, "forbidden", e.b, "GET", cv("silo-"+e.a, "zones"), nil)
	e.fenceFail(fenceapi.NotFound, e.b, "GET", "/silo/v1/silos/team-a", nil)
	n, err := fenceapi.ExpireManageGrants(ctx, e.db)
	if err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if n, _ := fenceapi.ExpireManageGrants(ctx, e.db); n != 0 {
		t.Errorf("expired twice: %d", n)
	}
	if recs := e.audit(e.a, fenceapi.KindManageExpire); len(recs) != 1 || recs[0]["fields"].(obj)["grant"] != id {
		t.Errorf("manage.expire: %v", recs)
	}
	l := e.ok(e.root, "GET", "/silo/v1/silos/team-a/manageGrants", nil)["manage_grants"].([]any)
	if len(l) != 1 || l[0].(obj)["state"] != "EXPIRED" {
		t.Errorf("list after expiry: %v", l)
	}
	e.fenceFail(fenceapi.FailedPrecondition, e.b, "POST", "/silo/v1/self/manageGrants/"+id+":renew", nil)
	e.fenceFail(fenceapi.FailedPrecondition, e.b, "POST", "/silo/v1/self/manageGrants/"+id+":accept", nil)

	// Without renew_until, there is no renewal.
	g = e.offer("team-a", "once", "/team-b", nil, obj{"ttl": "60s"})
	e.ok(e.b, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	e.fenceFail(fenceapi.FailedPrecondition, e.b, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":renew", nil)
	// Giving it up ends it.
	if out := e.ok(e.b, "DELETE", "/silo/v1/self/manageGrants/"+g["id"].(string), nil); out["state"] != "REVOKED" {
		t.Errorf("give up: %v", out)
	}
	e.fenceFail(fenceapi.NotFound, e.b, "GET", "/silo/v1/silos/team-a", nil)
}

func TestManageSealedHolder(t *testing.T) {
	e := newEnv(t)
	g := e.offer("team-a", "rollout", "/team-b", rolloutScope, obj{"ttl": "3600s"})
	e.ok(e.b, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	e.ok(e.b, "GET", cv("silo-"+e.a, "zones"), nil)
	// Sealing /team-b stops its calls on /team-a as well.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"control": "none"})
	e.fail(403, "forbidden", e.b, "GET", cv("silo-"+e.a, "zones"), nil)
	e.fenceFail(fenceapi.ControlDenied, e.b, "GET", "/silo/v1/silos/team-a", nil)
	// Under read, it reads and can't write.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"control": "read"})
	e.ok(e.b, "GET", cv("silo-"+e.a, "zones"), nil)
	e.fail(403, "forbidden", e.b, "POST", cv("silo-"+e.a, "zones/us-central1-a/disks"), obj{"name": e.a + "-d"})
	// A read grant can't write either, whatever the holder's policy.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"control": "full"})
	r := e.offer("team-a/ci", "look", "/team-b", obj{"facade": obj{"resources": []string{"*"}, "access": "read"}}, obj{"ttl": "60s"})
	e.ok(e.b, "POST", "/silo/v1/self/manageGrants/"+r["id"].(string)+":accept", nil)
	// The broader grant on team-a still covers ci; give it up, and the
	// read one is all that's left.
	e.ok(e.b, "DELETE", "/silo/v1/self/manageGrants/"+g["id"].(string), nil)
	e.ok(e.b, "GET", cv("silo-"+e.ci, "zones"), nil)
	e.ok(e.b, "GET", cv("silo-"+e.ci, "global/networks"), nil)
	msg := e.fail(403, "forbidden", e.b, "POST", cv("silo-"+e.ci, "zones/us-central1-a/disks"), obj{"name": e.ci + "-d"})
	if !strings.Contains(msg, `"look"`) {
		t.Errorf("read grant refusal: %q", msg)
	}
	e.fail(403, "forbidden", e.b, "GET", cv("silo-"+e.a, "zones"), nil)
}

func TestManageFenceGroups(t *testing.T) {
	e := newEnv(t)
	g := e.offer("team-a", "ops", "/team-b", obj{"fence": []string{"children", "blocks"}}, obj{"ttl": "3600s"})
	id := g["id"].(string)
	e.ok(e.b, "POST", "/silo/v1/self/manageGrants/"+id+":accept", nil)
	// children: create and delete the target's descendants, and change
	// their allocations, drawn from the target's own.
	c := e.ok(e.b, "POST", "/silo/v1/silos/team-a/children", obj{"name": "web", "address": obj{"prefix_length": 24},
		"quota": obj{"us-central1": obj{"vcpu": 1}}})
	if c["path"] != "/team-a/web" {
		t.Fatalf("create: %v", c)
	}
	recs := e.audit(c["id"].(string), fenceapi.KindSiloCreate)
	if len(recs) != 1 || recs[0]["via"] != "manage:"+e.b {
		t.Errorf("create audit: %v", recs)
	}
	e.ok(e.b, "PATCH", "/silo/v1/silos/team-a/web", obj{"quota": obj{"us-central1": obj{"vcpu": 2}}, "labels": obj{"v": "2"}})
	e.fenceFail(fenceapi.PermissionDenied, e.b, "PATCH", "/silo/v1/silos/team-a/web", obj{"control": "read"})
	e.fenceFail(fenceapi.PermissionDenied, e.b, "POST", "/silo/v1/silos/team-a/children",
		obj{"name": "x", "address": obj{"prefix_length": 28}, "control": "read"})
	// blocks: on descendants, and releaseBlock on the target too; but
	// never the target's own allocations.
	e.ok(e.b, "POST", "/silo/v1/silos/team-a/web:addBlock", obj{"prefix_length": 28})
	e.fenceFail(fenceapi.PermissionDenied, e.b, "POST", "/silo/v1/silos/team-a:addBlock", obj{"prefix_length": 28})
	e.fenceFail(fenceapi.PermissionDenied, e.b, "PATCH", "/silo/v1/silos/team-a", obj{"labels": obj{"x": "y"}})
	e.ok(e.b, "DELETE", "/silo/v1/silos/team-a/web?force=true", nil)
	// No facade scope: the facade is 403, naming the grant.
	msg := e.fail(403, "forbidden", e.b, "GET", cv("silo-"+e.a, "zones"), nil)
	if !strings.Contains(msg, `"ops"`) {
		t.Errorf("facade refusal: %q", msg)
	}

	// Deleting the holder ends the grant -- once the delete operation,
	// which the DELETE only starts, has run.
	e.ok(e.root, "DELETE", "/silo/v1/silos/team-b?force=true", nil)
	e.eventually("the grant to end with its holder", func() bool {
		return len(e.ok(e.root, "GET", "/silo/v1/silos/team-a/manageGrants", nil)["manage_grants"].([]any)) == 0
	})
	if recs := e.audit(e.a, fenceapi.KindManageRevoke); len(recs) != 1 || recs[0]["fields"].(obj)["reason"] != "holder deleted" {
		t.Errorf("revoke on holder deletion: %v", recs)
	}
	// Deleting the target ends its grants too.
	other := e.ok(e.root, "POST", "/silo/v1/silos/-"+e.root+"/children", obj{"name": "other", "address": obj{"prefix_length": 24}})["id"].(string)
	g = e.offer("team-a/ci", "x", "/other", nil, obj{"ttl": "60s"})
	e.ok(other, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	if len(e.held(other)) != 1 {
		t.Fatal("/other holds nothing")
	}
	e.ok(e.root, "DELETE", "/silo/v1/silos/team-a/ci?force=true", nil)
	e.eventually("the grant to end with its target", func() bool { return len(e.held(other)) == 0 })
}
