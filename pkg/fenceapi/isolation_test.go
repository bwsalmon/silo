package fenceapi_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

func (e *env) want(caller, ref string, paths map[string]string) {
	e.t.Helper()
	d := e.ok(caller, "GET", "/silo/v1/silos/"+ref, nil)
	for path, want := range paths {
		if got := str(get(d, strings.Split(path, ".")...)); got != want {
			e.t.Errorf("%s %s = %s, want %s", ref, path, got, want)
		}
	}
}

// The isolation and ingress policies are never looser than the
// parent's (local-backend.md, "In a silo's policy" and "Published
// ports"; fence-api.md, "Isolation and ingress policies").
func TestIsolationAndIngress(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	e.want(e.root, "-"+e.root, map[string]string{
		"isolation.own.allowed":       "[vm sandbox]",
		"isolation.own.default":       "vm",
		"isolation.effective.allowed": "[vm sandbox]",
		"ingress.own":                 "host",
		"ingress.effective":           "host",
	})

	// A child takes its parent's effective isolation and no ingress,
	// unless it is given others no looser.
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"ingress": "host"})
	e.want(e.root, "team-a", map[string]string{
		"isolation.effective.allowed": "[vm sandbox]",
		"isolation.effective.default": "vm",
		"ingress.effective":           "host",
	})
	b := e.mk(e.root, "-"+e.root, "team-b", obj{"isolation": obj{"allowed": []string{"vm"}}})
	e.want(e.root, "team-b", map[string]string{
		"isolation.own.allowed": "[vm]",
		"isolation.own.default": "vm",
		"ingress.own":           "none",
		"ingress.effective":     "none",
	})
	e.mk(e.root, "team-b", "plain", nil)
	e.want(e.root, "team-b/plain", map[string]string{"isolation.own.allowed": "[vm]", "ingress.own": "none"})

	path := "/silo/v1/silos/team-b/children"
	for name, body := range map[string]obj{
		"sandbox": {"isolation": obj{"allowed": []string{"sandbox"}}},
		"both":    {"isolation": obj{"allowed": []string{"vm", "sandbox"}, "default": "vm"}},
		"ingress": {"ingress": "host"},
	} {
		body["name"], body["address"] = "x", obj{"prefix_length": 24}
		er := e.fail(fenceapi.LooserThanParent, e.root, "POST", path, body)
		t.Logf("%s: %v", name, er["message"])
	}
	for name, body := range map[string]obj{
		"empty":       {"isolation": obj{"allowed": []string{}}},
		"runc":        {"isolation": obj{"allowed": []string{"runc"}}},
		"default-out": {"isolation": obj{"allowed": []string{"vm"}, "default": "sandbox"}},
		"ingress":     {"ingress": "public"},
	} {
		body["name"], body["address"] = "x", obj{"prefix_length": 24}
		er := e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/silos/team-a/children", body)
		t.Logf("%s: %v", name, er["message"])
	}

	// A sandbox default under a parent that allows both.
	e.mk(e.root, "team-a", "ci", obj{"isolation": obj{"allowed": []string{"sandbox", "vm"}, "default": "sandbox"}, "ingress": "host"})
	e.want(e.root, "team-a/ci", map[string]string{
		"isolation.own.allowed":       "[vm sandbox]",
		"isolation.effective.default": "sandbox",
		"ingress.effective":           "host",
	})

	// PATCH by an ancestor is held to the parent's too.
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", "/silo/v1/silos/team-b/plain", obj{"isolation": obj{"allowed": []string{"vm", "sandbox"}}})
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", "/silo/v1/silos/team-b/plain", obj{"ingress": "host"})
	e.fail(fenceapi.InvalidArgument, e.root, "PATCH", "/silo/v1/silos/team-b/plain", obj{"isolation": obj{"default": "sandbox"}})

	// Tightening a parent tightens its subtree at once, keeps the
	// descendants' own settings, and doesn't stop them being patched.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"isolation": obj{"allowed": []string{"vm"}}, "ingress": "none"})
	e.want(e.root, "team-a", map[string]string{"isolation.own.allowed": "[vm]", "isolation.own.default": "vm"})
	e.want(e.root, "team-a/ci", map[string]string{
		"isolation.own.allowed":       "[vm sandbox]",
		"isolation.own.default":       "sandbox",
		"isolation.effective.allowed": "[vm]",
		"isolation.effective.default": "vm",
		"ingress.own":                 "host",
		"ingress.effective":           "none",
	})
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a/ci", obj{"labels": obj{"x": "y"}})
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", "/silo/v1/silos/team-a/ci", obj{"ingress": "host"})
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"isolation": obj{"allowed": []string{"vm", "sandbox"}}, "ingress": "host"})
	e.want(e.root, "team-a/ci", map[string]string{"isolation.effective.default": "sandbox", "ingress.effective": "host"})

	// A silo narrows its own, never widens.
	e.ok(a, "PATCH", "/silo/v1/self", obj{"isolation": obj{"allowed": []string{"vm"}}, "ingress": "none"})
	e.want(e.root, "team-a", map[string]string{"isolation.own.allowed": "[vm]", "ingress.own": "none"})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"isolation": obj{"allowed": []string{"vm", "sandbox"}}})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"ingress": "host"})
	e.fail(fenceapi.PermissionDenied, b, "PATCH", "/silo/v1/self", obj{"ingress": "host"})

	// The root's are set at install.
	e.fail(fenceapi.InvalidArgument, e.root, "PATCH", "/silo/v1/self", obj{"ingress": "none"})
	e.fail(fenceapi.InvalidArgument, e.root, "PATCH", "/silo/v1/self", obj{"isolation": obj{"allowed": []string{"vm"}}})
}

// Bootstrap takes the root's isolation and ingress.
func TestRootIsolationFromBootstrap(t *testing.T) {
	e := newEnvBody(t, fenceapi.Options{}, func(b obj) {
		b["isolation"] = obj{"allowed": []string{"sandbox"}}
		b["ingress"] = "none"
	})
	e.want(e.root, "-"+e.root, map[string]string{
		"isolation.effective.allowed": "[sandbox]",
		"isolation.effective.default": "sandbox",
		"ingress.effective":           "none",
	})
	e.fail(fenceapi.LooserThanParent, e.root, "POST", "/silo/v1/silos/-"+e.root+"/children",
		obj{"name": "x", "address": obj{"prefix_length": 16}, "isolation": obj{"allowed": []string{"vm"}}})
	e.fail(fenceapi.LooserThanParent, e.root, "POST", "/silo/v1/silos/-"+e.root+"/children",
		obj{"name": "x", "address": obj{"prefix_length": 16}, "ingress": "host"})
}

// A local tree (local-backend.md): backing local throughout, and one
// region, local.
func TestLocalBacking(t *testing.T) {
	e := newEnvBody(t, fenceapi.Options{}, func(b obj) {
		b["backing"] = obj{"kind": "local"}
		delete(b, "regions")
		b["capacity"] = obj{"global": obj{"subnetworks": 20}}
	})
	e.want(e.root, "-"+e.root, map[string]string{"backing.kind": "local", "regions": "[local]"})
	e.mk(e.root, "-"+e.root, "team-a", nil)
	e.want(e.root, "team-a", map[string]string{"backing.kind": "local", "regions": "[local]"})
	e.mk(e.root, "team-a", "ci", obj{"backing": obj{"kind": "local"}, "regions": []string{"local"}})
	e.want(e.root, "team-a/ci", map[string]string{"backing.kind": "local", "regions": "[local]"})

	path := "/silo/v1/silos/team-a/children"
	mk := func(extra obj) obj {
		body := obj{"name": "x", "address": obj{"prefix_length": 24}}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}
	e.fail(fenceapi.Unimplemented, e.root, "POST", path, mk(obj{"backing": obj{"kind": "subnet"}}))
	e.fail(fenceapi.Unimplemented, e.root, "POST", path, mk(obj{"backing": obj{"kind": "vpc"}}))
	e.fail(fenceapi.InvalidArgument, e.root, "POST", path, mk(obj{"backing": obj{"kind": "docker"}}))
	e.fail(fenceapi.InvalidArgument, e.root, "POST", path, mk(obj{"regions": []string{"us-central1"}}))
	e.fail(fenceapi.InvalidArgument, e.root, "PATCH", "/silo/v1/silos/team-a/ci", obj{"regions": []string{"us-central1"}})
}

// A subnet tree has GCE regions and subnet children only.
func TestSubnetBackingRefusesLocal(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	path := "/silo/v1/silos/-" + e.root + "/children"
	e.fail(fenceapi.Unimplemented, e.root, "POST", path, obj{"name": "x", "address": obj{"prefix_length": 16}, "backing": obj{"kind": "local"}})
	e.fail(fenceapi.InvalidArgument, e.root, "POST", path, obj{"name": "x", "address": obj{"prefix_length": 16}, "regions": []string{"local"}})
	e.mk(e.root, "-"+e.root, "x", obj{"backing": obj{"kind": "subnet"}})
	e.want(e.root, "x", map[string]string{"backing.kind": "subnet"})
}

// Migration 21 gives every silo already there every class, the root
// ingress host and every other silo none.
func TestIsolationMigration(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, 20); err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx store.Tx) error {
		for _, s := range []struct{ id, parent, path string }{{"root01", "", "/"}, {"kid001", "root01", "/kid"}} {
			if _, err := tx.Exec(ctx, `INSERT INTO silos (id, name, parent, depth, path, backing, state, labels, created_at, updated_at)
				VALUES (?, ?, ?, 0, ?, 'subnet', 'ACTIVE', '{}', 0, 0)`, s.id, s.id, s.parent, s.path); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO silo_policies (silo, egress, grant_policy, agent_policy, control, updated_at)
				VALUES (?, '{}', '{}', '{}', 'full', 0)`, s.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	err = db.Tx(ctx, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT silo, isolation, ingress FROM silo_policies`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, iso, ing string
			if err := rows.Scan(&id, &iso, &ing); err != nil {
				return err
			}
			got[id] = iso + " " + ing
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	all := `{"allowed":["vm","sandbox"],"default":"vm"}`
	if got["root01"] != all+" host" || got["kid001"] != all+" none" {
		t.Errorf("after migration: %v", got)
	}
}
