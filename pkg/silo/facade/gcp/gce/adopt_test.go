package gce

import (
	"context"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// TestAdopt: the leader takes over the operations of a replica whose
// heartbeat stopped, or that never wrote one, and finishes them, so a
// failed call is still refunded; a live replica's are left to it
// (state.md "More than one silod, and the leader", SV-52).
func TestAdopt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	zone := "zones/us-central1-a/disks"
	ssd := ledger.Key{Kind: "gce", Class: "ssd_disk_gb", Region: "us-central1"}
	used := func() int64 {
		t.Helper()
		var r ledger.Row
		if err := e.db.Tx(ctx, func(tx store.Tx) error {
			var err error
			r, err = ledger.Get(ctx, tx, e.a, ssd)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return r.Used
	}
	replicaOf := func(name string) string {
		t.Helper()
		var r string
		if err := e.db.Tx(ctx, func(tx store.Tx) error {
			return tx.QueryRow(ctx, `SELECT replica FROM facade_operations WHERE name = ?`, name).Scan(&r)
		}); err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Three replicas start a disk insert each: "dead", whose heartbeat
	// stops, "alive", whose doesn't, and one from before the replica
	// column, which records none. None of them watches its operation.
	start := func(replica, name string) string {
		t.Helper()
		e.facade.opts.Replica = replica
		op := e.ok(e.a, "POST", cv(host, zone), obj{"name": name, "sizeGb": "10", "type": "zones/us-central1-a/diskTypes/pd-ssd"})
		return op["name"].(string)
	}
	dead := start("dead", "d-dead")
	alive := start("alive", "d-alive")
	legacy := start("", "d-legacy")
	e.facade.Close()
	if u := used(); u != ledger.Units(30) {
		t.Fatalf("used %d after three inserts, want 30 GB", u)
	}
	for _, r := range []string{"dead", "alive"} {
		if err := store.Heartbeat(ctx, e.db, store.Replica{ID: r, Release: "test", Started: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// dead's heartbeat is a minute old.
	e.exec(`UPDATE replicas SET heartbeat_at = heartbeat_at - ? WHERE id = ?`, time.Minute.Milliseconds(), "dead")

	// GCE fails every insert.
	for range 3 {
		e.gce.FailOperation(fake.Failure{Method: "disks.insert", Kind: fake.FailQuota})
	}
	e.gce.RunOperations()

	lead := New(e.db, Options{GCE: e.gce, HostProject: host, Replica: "leader"})
	t.Cleanup(lead.Close)
	n, err := lead.Adopt(ctx, nil)
	if err != nil || n != 2 {
		t.Fatalf("Adopt = %d, %v; want 2", n, err)
	}
	if r := replicaOf(dead); r != "leader" {
		t.Errorf("dead's operation is %q's, want the leader's", r)
	}
	if r := replicaOf(legacy); r != "leader" {
		t.Errorf("the legacy operation is %q's, want the leader's", r)
	}
	if r := replicaOf(alive); r != "alive" {
		t.Errorf("a live replica's operation was adopted by %q", r)
	}
	// Both adopted operations failed; their refunds happen.
	e.eventually("the adopted operations' refunds", func() bool { return used() == ledger.Units(10) })
	if n, err := lead.Adopt(ctx, nil); err != nil || n != 0 {
		t.Errorf("a second Adopt = %d, %v; want 0", n, err)
	}

	// A guard that refuses adopts nothing.
	e.exec(`UPDATE replicas SET heartbeat_at = 0 WHERE id = ?`, "alive")
	refuse := func(context.Context, store.Tx) error { return context.Canceled }
	if n, err := lead.Adopt(ctx, refuse); err == nil || n != 0 {
		t.Errorf("a refused Adopt = %d, %v", n, err)
	}
	if r := replicaOf(alive); r != "alive" {
		t.Errorf("a refused Adopt took the operation for %q", r)
	}
}
