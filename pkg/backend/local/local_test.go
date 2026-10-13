package local_test

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend"
	"github.com/bwsalmon/silo/pkg/backend/local"
	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/fake"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/tree"
)

const image = "sha256:00"

func add(t *testing.T, d *fake.Driver, id, silo, name, addr string, start bool) {
	t.Helper()
	ctx := context.Background()
	d.Prepare(ctx, image)
	labels := map[string]string{runtime.LabelSilo: silo, runtime.LabelName: name}
	if addr != "" {
		labels[runtime.LabelAddr] = addr
	}
	if err := d.Create(ctx, runtime.Spec{ID: id, Silo: silo, Image: image, NetNS: "/ns/" + id,
		Process: runtime.Process{Args: []string{"sh"}}, Labels: labels}); err != nil {
		t.Fatal(err)
	}
	if start {
		if err := d.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContainers(t *testing.T) {
	ctx := context.Background()
	sb, vm := fake.New(runtime.Sandbox), fake.New(runtime.VM)
	add(t, sb, "c1", "s1", "web", "10.200.1.2", true)
	add(t, sb, "c2", "s1", "db", "", false)
	add(t, vm, "c3", "s2", "web", "10.200.2.2", true)
	be := local.New(local.Options{Drivers: []runtime.Driver{sb, vm}})
	if be.Fence != nil {
		t.Errorf("a backend with no nft has a fence")
	}

	l, err := be.Inventory.List(ctx, []string{"local"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]backend.Object{}
	for _, o := range l.Objects {
		got[o.Key()] = o
	}
	if o := got["containers:vm/c3"]; o.Silo != "s2" || !o.Labelled || !o.InVPC || len(got) != 3 {
		t.Errorf("listing %+v", l.Objects)
	}

	if a, err := be.Targets.Resolve(ctx, "s2", "", "web"); err != nil || a != netip.MustParseAddr("10.200.2.2") {
		t.Errorf("resolve s2/web: %v %v", a, err)
	}
	if _, err := be.Targets.Resolve(ctx, "s1", "", "db"); err == nil {
		t.Errorf("resolved a container with no address")
	}
	if _, err := be.Targets.Resolve(ctx, "s3", "", "web"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("resolve in another silo: %v", err)
	}

	// Deleting s1's contents removes its running and created
	// containers, and nothing of s2's.
	if err := be.Contents.DeleteContents(ctx, &tree.Silo{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if cs, _ := sb.List(ctx); len(cs) != 0 {
		t.Errorf("s1's containers left: %+v", cs)
	}
	if cs, _ := vm.List(ctx); len(cs) != 1 {
		t.Errorf("s2's containers: %+v", cs)
	}

	// The reconcile's delete of an orphan, and of one gone already.
	o := got["containers:vm/c3"]
	if _, err := be.Inventory.Delete(ctx, o); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Inventory.Delete(ctx, o); err != nil {
		t.Errorf("deleting a container gone already: %v", err)
	}
	if cs, _ := vm.List(ctx); len(cs) != 0 {
		t.Errorf("after delete: %+v", cs)
	}
}

func TestNewWithFence(t *testing.T) {
	ctx := context.Background()
	if be := local.New(local.Options{NFT: &netfence.Fake{}}); be.Fence != nil {
		t.Errorf("a fence with no store")
	}
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	nft := &netfence.Fake{}
	be := local.New(local.Options{Store: db, NFT: nft})
	if be.Fence == nil || be.Inventory != nil {
		t.Fatalf("backend %+v: want a fence and, with no drivers, no inventory", be)
	}
	if _, err := be.Fence.Repair(ctx); err != nil {
		t.Fatal(err)
	}
	// An empty database is a fence that lets nothing through: no
	// tenants, no pins.
	rs := nft.Loaded()
	if rs == nil || len(rs.Table("inet").Set(netfence.SetTenants).Elems) != 0 || len(rs.Table("bridge").Set(netfence.SetPins).Elems) != 0 {
		t.Errorf("loaded %v", rs)
	}
}

// TestBindMapping: the first start records the holder's mapping, and a
// later one, after a reboot, must find the same.
func TestBindMapping(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	const m = "uid=0:1000:1,1:100000:65536 gid=0:1000:1,1:100000:65536"
	for range 2 {
		if err := local.BindMapping(ctx, db, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.BindMapping(ctx, db, "uid=0:1000:1,1:200000:65536 gid=0:1000:1,1:200000:65536"); !errors.Is(err, local.ErrMappingChanged) {
		t.Errorf("another mapping: %v, want ErrMappingChanged", err)
	}
}
