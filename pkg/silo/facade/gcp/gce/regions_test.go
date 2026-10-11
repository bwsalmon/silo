package gce

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// TestAllowedRegions pins allocators.md, "Allowed regions" (SV-37), end
// to end through the fence API and the facade: the root's regions, the
// facade's 403 for anything outside a silo's, the lists, quota only in
// an allowed region, removing one only when nothing in the subtree
// uses it (quota, an object charged there, or a subnet), adding one granting nothing, images' and snapshots'
// storageLocations, and templates made in a region since removed.
func TestAllowedRegions(t *testing.T) {
	e := newTemplateEnv(t, 2)
	const east = "us-east1"
	silo := func(caller, ref string) obj { return e.ok(caller, "GET", "/silo/v1/silos/"+ref, nil) }
	regions := func(caller, ref string) string { return fmt.Sprint(silo(caller, ref)["regions"]) }
	// fenceFail is a fence API call that must fail with code.
	fenceFail := func(code, caller, method, path string, body any) {
		t.Helper()
		got, out := e.do(caller, method, path, body)
		er, _ := out["error"].(obj)
		if got < 400 || er == nil || er["code"] != code {
			t.Fatalf("%s %s as %s: %d %v, want %s", method, path, caller, got, out, code)
		}
	}

	// The root's regions are the ones it was installed with, and only
	// install sets them.
	if got := regions(e.root, "-"+e.root); got != "[us-central1 us-east1]" {
		t.Errorf("the root's regions: %s", got)
	}
	if got := names(items(e.ok(e.root, "GET", cv(host, "regions"), nil))); fmt.Sprint(got) != "[us-central1 us-east1]" {
		t.Errorf("the root's regions.list: %v", got)
	}
	fenceFail("PERMISSION_DENIED", e.root, "PATCH", "/silo/v1/silos/-"+e.root, obj{"regions": []string{central}})
	// A child's are a subset of its parent's.
	fenceFail("LOOSER_THAN_PARENT", e.root, "PATCH", "/silo/v1/silos/team-a/ci", obj{"regions": []string{central, east}})
	fenceFail("LOOSER_THAN_PARENT", e.root, "PATCH", "/silo/v1/silos/team-b", obj{"regions": []string{central, "europe-west4"}})

	// regions.list and zones.list show only the allowed set, for a silo
	// and for its child.
	for _, s := range []string{e.a, e.ci} {
		if got := names(items(e.ok(s, "GET", cv(host, "regions"), nil))); fmt.Sprint(got) != "[us-central1]" {
			t.Errorf("%s's regions.list: %v", s, got)
		}
		for _, z := range names(items(e.ok(s, "GET", cv(host, "zones"), nil))) {
			if !strings.HasPrefix(z, central+"-") {
				t.Errorf("%s's zones.list has %s", s, z)
			}
		}
	}
	zs := names(items(e.ok(e.b, "GET", cv(host, "zones"), nil)))
	if !strings.Contains(fmt.Sprint(zs), central+"-a") || !strings.Contains(fmt.Sprint(zs), east+"-b") {
		t.Errorf("team-b's zones.list: %v", zs)
	}

	// Every call naming a region, or a zone in one, outside the caller's
	// set is a 403, not a 404: reads and writes, a region the root has
	// and one it doesn't, the silo's own project and the host's.
	for _, s := range []string{e.a, e.ci} {
		for _, project := range []string{host, "silo-" + s} {
			for _, c := range []struct {
				method, rest string
				body         obj
			}{
				{"GET", "regions/us-east1", nil},
				{"GET", "regions/europe-west4", nil},
				{"GET", "regions/us-east1/subnetworks", nil},
				{"GET", "regions/us-east1/subnetworks/web", nil},
				{"POST", "regions/us-east1/subnetworks", obj{"name": "x", "ipCidrRange": "10.9.0.0/24"}},
				{"DELETE", "regions/us-east1/subnetworks/web", nil},
				{"GET", "regions/us-east1/operations", nil},
				{"GET", "regions/us-east1/instanceTemplates", nil},
				{"POST", "regions/us-east1/instanceTemplates", tpl("x", "e2-medium", obj{"networkInterfaces": []obj{{}}})},
				{"GET", "zones/us-east1-b", nil},
				{"GET", "zones/europe-west4-a", nil},
				{"GET", "zones/us-east1-b/machineTypes", nil},
				{"GET", "zones/us-east1-b/machineTypes/e2-medium", nil},
				{"GET", "zones/us-east1-b/diskTypes", nil},
				{"GET", "zones/us-east1-b/instances", nil},
				{"GET", "zones/us-east1-b/instances/vm", nil},
				{"POST", "zones/us-east1-b/instances", vm("x", "e2-medium", nil)},
				{"DELETE", "zones/us-east1-b/instances/vm", nil},
				{"GET", "zones/us-east1-b/disks", nil},
				{"POST", "zones/us-east1-b/disks", obj{"name": "d", "sizeGb": "10"}},
				{"GET", "zones/us-east1-b/operations/op", nil},
			} {
				e.fail(403, "regionNotAllowed", s, c.method, cv(project, c.rest), c.body)
			}
		}
	}

	// Quota can only be granted in an allowed region, to a child made
	// or changed.
	fenceFail("FAILED_PRECONDITION", e.root, "PATCH", "/silo/v1/silos/team-a", obj{"quota": obj{east: obj{"vcpu": 1}}})
	fenceFail("FAILED_PRECONDITION", e.root, "POST", "/silo/v1/silos/team-b/children",
		obj{"name": "x", "address": obj{"prefix_length": 24}, "regions": []string{central}, "quota": obj{east: obj{"vcpu": 1}}})
	fenceFail("FAILED_PRECONDITION", e.root, "POST", "/silo/v1/silos/team-a/children",
		obj{"name": "x", "address": obj{"prefix_length": 24}, "quota": obj{east: obj{"vcpu": 1}}})

	// Adding a region grants no quota there: team-a may be allowed
	// us-east1 and have none of it.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"regions": []string{central, east}})
	if got := regions(e.a, "self"); got != "[us-central1 us-east1]" {
		t.Errorf("team-a's regions after adding: %s", got)
	}
	if q, _ := silo(e.root, "team-a")["quota"].(obj); q[east] != nil {
		t.Errorf("team-a got quota in %s by being allowed it: %v", east, q[east])
	}
	r := e.ok(e.a, "GET", cv(host, "regions/"+east), nil)
	for _, m := range []string{"CPUS", "SILO_MEMORY_MB", "SSD_TOTAL_GB"} {
		if lim, use, ok := quotaOf(r, m); !ok || lim != 0 || use != 0 {
			t.Errorf("team-a's %s in %s: limit %v usage %v (%v), want 0", m, east, lim, use, ok)
		}
	}
	e.fail(403, "quotaExceeded", e.a, "POST", cv(host, "zones/us-east1-b/disks"), obj{"name": "d", "sizeGb": "10", "type": "zones/us-east1-b/diskTypes/pd-standard"})
	// Holding nothing there, it can be taken away again.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"regions": []string{central}})
	e.fail(403, "regionNotAllowed", e.a, "GET", cv(host, "regions/"+east), nil)

	// Removing a region that the silo, or a descendant, still holds
	// quota in fails, and touches nothing.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return ledger.SetCapacity(ctx, tx, e.root, []ledger.Amount{
			{Key: ledger.Key{Kind: "gce", Class: "standard_disk_gb", Region: east}, Amount: ledger.Units(100)},
			{Key: ledger.Key{Kind: "gce", Class: "vcpu", Region: east}, Amount: ledger.Units(10)}})
	}); err != nil {
		t.Fatal(err)
	}
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"quota": obj{east: obj{"vcpu": 2}}})
	kid := e.ok(e.root, "POST", "/silo/v1/silos/team-b/children", obj{"name": "east", "address": obj{"prefix_length": 24},
		"regions": []string{east}, "quota": obj{east: obj{"vcpu": 2}}})["id"].(string)
	fenceFail("FAILED_PRECONDITION", e.root, "PATCH", "/silo/v1/silos/team-b", obj{"regions": []string{central}})
	// team-b's own grant is all delegated: it's the child's that pins it.
	fenceFail("FAILED_PRECONDITION", e.root, "PATCH", "/silo/v1/silos/team-b/east", obj{"regions": []string{}})
	if got := regions(e.root, "team-b"); got != "[us-central1 us-east1]" {
		t.Errorf("team-b's regions after a refused removal: %s", got)
	}
	// Removing it and lowering the quota in one PATCH works: the quota
	// goes first.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b/east", obj{"regions": []string{}, "quota": obj{east: obj{"vcpu": 0}}})
	if got := regions(kid, "self"); got != "[]" {
		t.Errorf("east's regions: %s", got)
	}

	// Objects pin a region too: a disk of team-b's in us-east1.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"quota": obj{east: obj{"vcpu": 0, "standard_disk_gb": 10}}})
	e.ok(e.b, "POST", cv(host, "zones/us-east1-b/disks"), obj{"name": "d", "sizeGb": "10", "type": "zones/us-east1-b/diskTypes/pd-standard"})
	e.run()
	fenceFail("FAILED_PRECONDITION", e.root, "PATCH", "/silo/v1/silos/team-b", obj{"regions": []string{central}, "quota": obj{east: obj{"standard_disk_gb": 0}}})
	e.ok(e.b, "GET", cv(host, "zones/us-east1-b/disks/d"), nil)
	// So does a subnet, though it is charged to a global count: while
	// team-b has one in us-east1, us-east1 can't be taken away, or the
	// subnet could no longer be deleted and its range would be stranded.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"quota": obj{"global": obj{"firewall_rules": 2, "subnetworks": 2}}})
	taken := e.blocks(kid)
	for _, r := range e.own(e.b) {
		taken = append(taken, netip.MustParsePrefix(r))
	}
	e.ok(e.b, "POST", cv(host, "regions/us-east1/subnetworks"), obj{"name": "web", "ipCidrRange": freeIn(e.blocks(e.b)[0], 24, taken...).String()})
	e.run()

	// Storage locations: an allowed region, or the multi-region that
	// contains one (call 25); for team-b, us-east1 now too.
	e.ok(e.a, "POST", cv(host, "global/images"), obj{"name": "us", "sourceImage": debian, "storageLocations": []string{"us"}})
	e.ok(e.a, "POST", cv(host, "global/images"), obj{"name": "c1", "sourceImage": debian, "storageLocations": []string{central}})
	e.ok(e.b, "POST", cv(host, "global/images"), obj{"name": "e1", "sourceImage": debian, "storageLocations": []string{east}})
	for _, locs := range [][]string{{east}, {"eu"}, {"asia"}, {central, "eu"}, {"us", "europe-west4"}} {
		e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, "global/images"), obj{"name": "x", "sourceImage": debian, "storageLocations": locs})
	}
	e.ok(e.a, "POST", cv(host, "zones/"+zoneA+"/disks"), obj{"name": "src", "sizeGb": "10", "type": "zones/" + zoneA + "/diskTypes/pd-standard"})
	e.run()
	e.ok(e.a, "POST", cv(host, "zones/"+zoneA+"/disks/src/createSnapshot"), obj{"name": "s-us", "storageLocations": []string{"us"}})
	for _, locs := range [][]string{{east}, {"eu"}} {
		e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, "zones/"+zoneA+"/disks/src/createSnapshot"), obj{"name": "x", "storageLocations": locs})
	}
	e.run()

	// A template whose region is later removed is refused at instance
	// insert, through the fence API's PATCH: team-b's regional template
	// in us-east1, and its global one naming nothing.
	e.ok(e.b, "POST", cv(host, "regions/us-east1/instanceTemplates"), tpl("east", "e2-medium", obj{"networkInterfaces": []obj{{}}}))
	e.ok(e.b, "POST", cv(host, templatesPath), tpl("g", "e2-medium", obj{"networkInterfaces": []obj{{}}}))
	e.run()
	e.ok(e.b, "DELETE", cv(host, "zones/us-east1-b/disks/d"), nil)
	e.run()
	fenceFail("FAILED_PRECONDITION", e.root, "PATCH", "/silo/v1/silos/team-b", obj{"regions": []string{central}, "quota": obj{east: obj{"standard_disk_gb": 0}}})
	e.ok(e.b, "DELETE", cv(host, "regions/us-east1/subnetworks/web"), nil)
	e.run()
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"regions": []string{central}, "quota": obj{east: obj{"standard_disk_gb": 0}}})
	if got := regions(e.b, "self"); got != "[us-central1]" {
		t.Errorf("team-b's regions after removing: %s", got)
	}
	e.fail(403, "regionNotAllowed", e.b, "POST", cv(host, "zones/us-east1-b/instances?sourceInstanceTemplate=regions/us-east1/instanceTemplates/east"), obj{"name": "x"})
	e.fail(403, "regionNotAllowed", e.b, "POST", cv(host, "zones/us-east1-b/instances?sourceInstanceTemplate=global/instanceTemplates/g"), obj{"name": "x"})
	e.fail(403, "regionNotAllowed", e.b, "POST", cv(host, "global/images"), obj{"name": "e2", "sourceImage": debian, "storageLocations": []string{east}})
	if got := e.used(e.b, "instances", east); got != 0 {
		t.Errorf("team-b charged %d instances in %s", got, east)
	}
	var n int
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM charges WHERE region = ?`, east).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("charges left in %s: %d %v", east, n, err)
	}
}
