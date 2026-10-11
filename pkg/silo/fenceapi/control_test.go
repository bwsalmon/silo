package fenceapi_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// controlChanges is the "control" entries of the change log, oldest
// first.
func (e *env) controlChanges() []fenceapi.ControlChange {
	e.t.Helper()
	var out []fenceapi.ControlChange
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, `SELECT data FROM changes WHERE kind = ? ORDER BY seq`, fenceapi.ChangeKindControl)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				return err
			}
			var c fenceapi.ControlChange
			if err := json.Unmarshal([]byte(d), &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func controlOf(d obj) string {
	return str(get(d, "control", "own")) + " " + str(get(d, "control", "effective")) + " " + str(get(d, "control", "effective_from"))
}

func TestControlPolicy(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	team := e.mk(e.root, "-"+e.root, "team", obj{"quota": obj{"us-central1": obj{"vcpu": 8}}})
	ro := e.mk(e.root, "team", "ro", obj{"control": "read"})
	prod := e.mk(e.root, "-"+e.root, "prod", obj{"control": "none"})

	// describe: own, effective and where it comes from.
	for id, want := range map[string]string{
		e.root: "full full /",
		team:   "full full /",
		ro:     "read read /team/ro",
		prod:   "none none /prod",
	} {
		if got := controlOf(e.ok(e.root, "GET", "/silo/v1/silos/-"+id, nil)); got != want {
			t.Errorf("%s control = %s, want %s", id, got, want)
		}
	}

	// An ancestor writes into a sealed child, and a child it creates
	// defaults to the parent's effective policy (call 54).
	e.ok(e.root, "PATCH", "/silo/v1/silos/prod", obj{"labels": obj{"tier": "prod"}})
	web := e.mk(e.root, "prod", "web", nil)
	if got := controlOf(e.ok(e.root, "GET", "/silo/v1/silos/prod/web", nil)); got != "none none /prod" {
		t.Errorf("prod/web control = %s", got)
	}

	// Sealed: nothing at all, describe of itself included, and the
	// refusal says which policy and where it comes from.
	for _, c := range []struct{ caller, method, path string }{
		{prod, "GET", "/silo/v1/self"},
		{prod, "GET", "/silo/v1/self/children"},
		{prod, "GET", "/silo/v1/self/audit"},
		{prod, "GET", "/silo/v1/silos/prod/web"},
		{prod, "PATCH", "/silo/v1/silos/prod/web"}, // a sealed ancestor can't change its descendants
		{prod, "POST", "/silo/v1/self/children"},
		{prod, "POST", "/silo/v1/quota:preview"},
		{prod, "GET", "/silo/v1/self/exports"},
		{web, "GET", "/silo/v1/self"},
	} {
		er := e.fail(fenceapi.ControlDenied, c.caller, c.method, c.path, obj{"labels": obj{"x": "y"}})
		if d := str(er["details"]); d != "map[effective_from:/prod policy:none]" {
			t.Errorf("%s %s: details %s", c.method, c.path, d)
		}
	}
	if got := str(get(e.ok(e.root, "GET", "/silo/v1/silos/prod/web", nil), "labels")); got != "map[]" {
		t.Errorf("prod changed web: labels %s", got)
	}

	// read: reads only, by method rather than verb, and an unknown
	// path is a mutation.
	e.ok(ro, "GET", "/silo/v1/self", nil)
	e.ok(ro, "GET", "/silo/v1/self/children", nil)
	e.ok(ro, "GET", "/silo/v1/self/audit", nil)
	e.ok(ro, "GET", "/silo/v1/self/members", nil)
	if _, out := e.do(ro, "POST", "/silo/v1/quota:preview", obj{"allocation": obj{"silo": "/team/ro", "quota": obj{}}}); str(get(out, "error", "code")) == string(fenceapi.ControlDenied) {
		t.Errorf("quota:preview is a read, but: %v", out)
	}
	for _, c := range []struct {
		method, path string
		body         obj
	}{
		{"PATCH", "/silo/v1/self", obj{"egress": obj{"allow": []string{}}}},
		{"POST", "/silo/v1/self/children", obj{"name": "x", "address": obj{"prefix_length": 28}}},
		{"PUT", "/silo/v1/self/members/user:a@example.com", nil},
		{"DELETE", "/silo/v1/self", nil},
		{"POST", "/silo/v1/self:grow", obj{"to_prefix_length": 20}},
		{"POST", "/silo/v1/self:frobnicate", nil},
		{"POST", "/silo/v1/self/exports", nil},
		{"GET", "/silo/v1/nonsense", nil},
	} {
		er := e.fail(fenceapi.ControlDenied, ro, c.method, c.path, c.body)
		if d := str(er["details"]); d != "map[effective_from:/team/ro policy:read]" {
			t.Errorf("%s %s: details %s", c.method, c.path, d)
		}
	}
	// A full caller still gets the path's own answer.
	e.fail(fenceapi.NotFound, team, "GET", "/silo/v1/self/links/db", nil)
	e.fail(fenceapi.NotFound, team, "GET", "/silo/v1/nonsense", nil)

	// Each refusal is audited on the caller's silo as control.denied.
	var denied []obj
	for _, r := range e.ok(e.root, "GET", "/silo/v1/silos/team/ro/audit", nil)["records"].([]any) {
		if r.(obj)["kind"] == "control.denied" {
			denied = append(denied, r.(obj))
		}
	}
	if len(denied) != 8 {
		t.Fatalf("team/ro has %d control.denied records, want 8", len(denied))
	}
	for path, want := range map[string]string{
		"result": "CONTROL_DENIED", "actor.silo": ro, "fields.op": "silos.patch", "fields.class": "mutation",
		"fields.policy": "read", "fields.effective_from": "/team/ro",
	} {
		if got := str(get(denied[0], strings.Split(path, ".")...)); got != want {
			t.Errorf("control.denied %s = %s, want %s", path, got, want)
		}
	}

	// It doesn't bind an ancestor acting on the silo: /team (full)
	// creates under /team/ro, no looser than it.
	x := e.mk(team, "team/ro", "x", nil)
	e.fail(fenceapi.LooserThanParent, team, "POST", "/silo/v1/silos/team/ro/children", obj{"name": "y", "address": obj{"prefix_length": 28}, "control": "full"})
	e.fail(fenceapi.LooserThanParent, team, "PATCH", "/silo/v1/silos/team/ro/x", obj{"control": "full"})
	if got := controlOf(e.ok(team, "GET", "/silo/v1/silos/-"+x, nil)); got != "read read /team/ro" {
		t.Errorf("team/ro/x control = %s", got)
	}

	// Only a proper ancestor sets it: never the silo itself.
	e.fail(fenceapi.PermissionDenied, team, "PATCH", "/silo/v1/self", obj{"control": "none"})

	// Tightening /team tightens its subtree at once and keeps the
	// descendants' own settings; loosening restores them.
	before := len(e.controlChanges())
	e.ok(e.root, "PATCH", "/silo/v1/silos/team", obj{"control": "none"})
	if got := controlOf(e.ok(e.root, "GET", "/silo/v1/silos/-"+ro, nil)); got != "read none /team" {
		t.Errorf("team/ro control after tightening = %s", got)
	}
	e.fail(fenceapi.ControlDenied, ro, "GET", "/silo/v1/self", nil)
	// A sealed /team can't change its own descendants either.
	e.fail(fenceapi.ControlDenied, team, "PATCH", "/silo/v1/silos/team/ro", obj{"control": "none"})
	// An ancestor can still change the descendants' other settings,
	// though their own control is looser than /team's now.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team/ro", obj{"labels": obj{"a": "b"}})

	ch := e.controlChanges()
	if len(ch) != before+1 {
		t.Fatalf("%d control changes, want %d", len(ch), before+1)
	}
	c := ch[len(ch)-1]
	var effects []string
	for _, s := range c.Silos {
		effects = append(effects, s.Path+":"+s.Old+">"+s.New)
	}
	if c.Silo != team || c.Own != (fenceapi.ControlDelta{Old: "full", New: "none"}) || !c.Tightened ||
		strings.Join(effects, " ") != "/team:full>none /team/ro:read>none /team/ro/x:read>none" {
		t.Errorf("control change %+v (%v)", c, effects)
	}

	e.ok(e.root, "PATCH", "/silo/v1/silos/team", obj{"control": "full"})
	if got := controlOf(e.ok(e.root, "GET", "/silo/v1/silos/-"+ro, nil)); got != "read read /team/ro" {
		t.Errorf("team/ro control after loosening = %s", got)
	}
	e.ok(ro, "GET", "/silo/v1/self", nil)
	if c := e.controlChanges(); c[len(c)-1].Tightened {
		t.Errorf("loosening is not tightening: %+v", c[len(c)-1])
	}

	// A /team that is read itself can't set its descendants' policy:
	// it may set only as an ancestor whose own effective policy is full.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team", obj{"control": "read"})
	e.fail(fenceapi.ControlDenied, team, "PATCH", "/silo/v1/silos/team/ro", obj{"control": "none"})
	e.ok(e.root, "PATCH", "/silo/v1/silos/team", obj{"control": "full"})

	// A null control goes back to the parent's effective policy.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team/ro", obj{"control": nil})
	if got := controlOf(e.ok(e.root, "GET", "/silo/v1/silos/-"+ro, nil)); got != "full full /" {
		t.Errorf("team/ro control after null = %s", got)
	}

	// Every change is audited as silo.set_control, a create that sets
	// it included.
	var sets []string
	for _, r := range e.ok(e.root, "GET", "/silo/v1/self/audit?subtree=true&page_size=1000", nil)["records"].([]any) {
		if r := r.(obj); r["kind"] == "silo.set_control" {
			sets = append(sets, str(get(r, "silo", "path"))+":"+str(get(r, "fields", "new")))
		}
	}
	if got := strings.Join(sets, " "); got != "/team/ro:read /prod:none /team:none /team:full /team:read /team:full /team/ro:full" {
		t.Errorf("silo.set_control records: %s", got)
	}
}
