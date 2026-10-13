package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/tree"
)

var ctx = context.Background()

func newDB(t *testing.T) store.Database {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	return db
}

// do runs fn in a transaction and fails the test on an error.
func do(t *testing.T, db store.Store, fn func(tx store.Tx) error) {
	t.Helper()
	if err := db.Tx(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

// try runs fn in a transaction and returns its error.
func try(db store.Store, fn func(tx store.Tx) error) error { return db.Tx(ctx, fn) }

func want(t *testing.T, err error, code ledger.Code) {
	t.Helper()
	if ledger.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func rk(class string) ledger.Key { return ledger.Key{Kind: "gce", Class: class, Region: "us-central1"} }
func gk(class string) ledger.Key { return ledger.Key{Kind: "gce", Class: class, Region: ledger.Global} }

func amt(k ledger.Key, units int64) []ledger.Amount {
	return []ledger.Amount{{Key: k, Amount: ledger.Units(units)}}
}

func row(t *testing.T, db store.Store, silo string, k ledger.Key) ledger.Row {
	t.Helper()
	var r ledger.Row
	do(t, db, func(tx store.Tx) error {
		var err error
		r, err = ledger.Get(ctx, tx, silo, k)
		return err
	})
	return r
}

func checkBooks(t *testing.T, db store.Store) {
	t.Helper()
	do(t, db, func(tx store.Tx) error {
		bad, err := ledger.Check(ctx, tx)
		for _, b := range bad {
			t.Error(b)
		}
		return err
	})
}

// fixture is a root allowed us-central1 and europe-west4, with child
// /a under it and /a/b under that, both allowed us-central1.
type fixture struct {
	db         store.Database
	root, a, b *tree.Silo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{db: newDB(t)}
	do(t, f.db, func(tx store.Tx) error {
		var err error
		if f.root, err = tree.CreateRoot(ctx, tx, "root", tree.BackingSubnet, nil); err != nil {
			return err
		}
		if err := ledger.SetRegions(ctx, tx, f.root.ID, []string{"us-central1", "europe-west4"}); err != nil {
			return err
		}
		if f.a, err = tree.CreateChild(ctx, tx, f.root.ID, "a", tree.BackingSubnet, nil); err != nil {
			return err
		}
		if err := ledger.SetRegions(ctx, tx, f.a.ID, []string{"us-central1"}); err != nil {
			return err
		}
		if f.b, err = tree.CreateChild(ctx, tx, f.a.ID, "b", tree.BackingSubnet, map[string]string{"team": "b"}); err != nil {
			return err
		}
		return ledger.SetRegions(ctx, tx, f.b.ID, []string{"us-central1"})
	})
	return f
}

func TestClassTableSeeded(t *testing.T) {
	db := newDB(t)
	do(t, db, func(tx store.Tx) error {
		for _, c := range []struct{ name, unit, scope string }{
			{"vcpu", "vCPU", ledger.ScopeRegional},
			{"memory_mb", "MiB", ledger.ScopeRegional},
			{"spot_vcpu", "vCPU", ledger.ScopeRegional},
			{"pd_extreme_iops", "IOPS", ledger.ScopeRegional},
			{"hyperdisk_ml_throughput", "MiB/s", ledger.ScopeRegional},
			{"spot_local_ssd_gb", "GB", ledger.ScopeRegional},
			{"nvidia_t4_gpus", "GPU", ledger.ScopeRegional},
			{"spot_nvidia_a100_80gb_gpus", "GPU", ledger.ScopeRegional},
			{"nvidia_p4_vws_gpus", "GPU", ledger.ScopeRegional},
			{"subnetworks", "count", ledger.ScopeGlobal},
			{"gpus_all_regions", "GPU", ledger.ScopeGlobal},
			{"agent_sessions", "count", ledger.ScopeGlobal},
		} {
			got, err := ledger.LookupClass(ctx, tx, "gce", c.name)
			if err != nil {
				return err
			}
			if got.Unit != c.unit || got.Scope != c.scope || got.Sort != ledger.SortHeld || got.Period != "" {
				t.Errorf("gce/%s = %+v", c.name, got)
			}
		}
		m, err := ledger.LookupClass(ctx, tx, "model", "usd")
		if err != nil {
			return err
		}
		if m.Sort != ledger.SortMetered || m.Scope != ledger.ScopeGlobal || m.Period != ledger.PeriodMonth {
			t.Errorf("model/usd = %+v", m)
		}

		gpus, err := ledger.GPUClasses(ctx, tx)
		if err != nil {
			return err
		}
		if len(gpus) != 22 {
			t.Errorf("%d GPU mappings, want 22", len(gpus))
		}
		for _, g := range gpus {
			// The class is the metric in lower case, PREEMPTIBLE_ as spot_.
			if w := strings.ToLower(strings.Replace(g.Metric, "PREEMPTIBLE_", "SPOT_", 1)); g.Class != w {
				t.Errorf("%s: class %s, want %s", g.Metric, g.Class, w)
			}
			if g.Spot != strings.HasPrefix(g.Metric, "PREEMPTIBLE_") {
				t.Errorf("%+v: spot doesn't match the metric", g)
			}
			if _, err := ledger.LookupClass(ctx, tx, "gce", g.Class); err != nil {
				t.Errorf("GPU class %s is not in the class table: %v", g.Class, err)
			}
		}
		g, err := ledger.LookupGPU(ctx, tx, "nvidia-l4", true)
		if err != nil {
			return err
		}
		if g.Class != "spot_nvidia_l4_gpus" || g.Metric != "PREEMPTIBLE_NVIDIA_L4_GPUS" {
			t.Errorf("nvidia-l4 spot = %+v", g)
		}
		_, err = ledger.LookupGPU(ctx, tx, "nvidia-h100-80gb", false)
		want(t, err, ledger.InvalidArgument)

		// The root-capacity reader adds a family it discovers.
		h100 := ledger.GPUClass{Accelerator: "nvidia-h100-80gb", Metric: "NVIDIA_H100_GPUS", Class: "nvidia_h100_gpus"}
		if err := ledger.AddGPU(ctx, tx, h100); err != nil {
			return err
		}
		if g, err := ledger.LookupGPU(ctx, tx, "nvidia-h100-80gb", false); err != nil || *g != h100 {
			t.Errorf("after AddGPU: %+v, %v", g, err)
		}
		_, err = ledger.LookupClass(ctx, tx, "gce", "nvidia_h100_gpus")
		return err
	})
}

// TestMadeUpClass: a class that is only a row in the class table is
// granted, charged, previewed and refunded like any other, with no
// code knowing its name.
func TestMadeUpClass(t *testing.T) {
	f := newFixture(t)
	do(t, f.db, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO quota_classes (kind, name, unit, sort, scope, period)
			VALUES ('ci-pool', 'runner_minutes', 'minute', 'metered', 'global', 'week'),
			       ('gce', 'widgets', 'widget', 'held', 'regional', '')`)
		return err
	})
	minutes := ledger.Key{Kind: "ci-pool", Class: "runner_minutes", Region: ledger.Global}
	widgets := rk("widgets")
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, append(amt(minutes, 600), amt(widgets, 5)...)); err != nil {
			return err
		}
		if err := ledger.Grant(ctx, tx, f.a.ID, append(amt(minutes, 100), amt(widgets, 3)...)); err != nil {
			return err
		}
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "job-1", Silo: f.a.ID, Via: ledger.ViaFacade,
			Amounts: []ledger.Amount{{Key: minutes, Amount: 2_500_000}, {Key: widgets, Amount: ledger.Units(3)}}})
	})
	err := try(f.db, func(tx store.Tx) error {
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "job-2", Silo: f.a.ID, Via: ledger.ViaFacade,
			Amounts: amt(widgets, 1)})
	})
	want(t, err, ledger.QuotaExceeded)
	if r := row(t, f.db, f.a.ID, minutes); r.Used != 2_500_000 || r.Free() != ledger.Units(100)-2_500_000 {
		t.Errorf("runner_minutes row = %+v", r)
	}
	if r := row(t, f.db, f.root.ID, widgets); r.Delegated != ledger.Units(3) || r.Free() != ledger.Units(2) {
		t.Errorf("root widgets row = %+v", r)
	}
	do(t, f.db, func(tx store.Tx) error {
		_, err := ledger.Refund(ctx, tx, "job-1", nil)
		return err
	})
	if r := row(t, f.db, f.a.ID, widgets); r.Used != 0 {
		t.Errorf("after the refund, widgets used = %d", r.Used)
	}
	checkBooks(t, f.db)
}

func TestInvalidArgument(t *testing.T) {
	f := newFixture(t)
	for name, fn := range map[string]func(tx store.Tx) error{
		"unknown class grant": func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.a.ID, amt(rk("vcpus"), 1)) },
		"unknown kind charge": func(tx store.Tx) error {
			return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "o", Silo: f.a.ID, Via: ledger.ViaFacade,
				Amounts: amt(ledger.Key{Kind: "aws", Class: "vcpu", Region: "us-central1"}, 1)})
		},
		"unknown class capacity": func(tx store.Tx) error {
			return ledger.SetCapacity(ctx, tx, f.root.ID, amt(gk("nope"), 1))
		},
		"global class in a region": func(tx store.Tx) error {
			return ledger.SetCapacity(ctx, tx, f.root.ID, amt(ledger.Key{Kind: "gce", Class: "subnetworks", Region: "us-central1"}, 1))
		},
		"regional class global": func(tx store.Tx) error { return ledger.SetCapacity(ctx, tx, f.root.ID, amt(gk("vcpu"), 1)) },
		"negative grant":        func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.a.ID, amt(rk("vcpu"), -1)) },
		"grant to the root":     func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.root.ID, amt(rk("vcpu"), 1)) },
		"capacity of a child":   func(tx store.Tx) error { return ledger.SetCapacity(ctx, tx, f.a.ID, amt(rk("vcpu"), 1)) },
		"bad via": func(tx store.Tx) error {
			return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "o", Silo: f.a.ID, Via: "tenant", Amounts: amt(rk("vcpu"), 1)})
		},
		"no object": func(tx store.Tx) error {
			return ledger.Charge(ctx, tx, ledger.ChargeRequest{Silo: f.a.ID, Via: ledger.ViaFacade, Amounts: amt(rk("vcpu"), 1)})
		},
	} {
		t.Run(name, func(t *testing.T) { want(t, try(f.db, fn), ledger.InvalidArgument) })
	}
	// errors.Is matches the sentinel of the code.
	err := try(f.db, func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.a.ID, amt(rk("vcpus"), 1)) })
	if !errors.Is(err, ledger.ErrInvalidArgument) || errors.Is(err, ledger.ErrQuotaExceeded) {
		t.Errorf("errors.Is(%v) matches the wrong sentinel", err)
	}
}

func TestHardPartition(t *testing.T) {
	f := newFixture(t)
	vcpu := rk("vcpu")
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, amt(vcpu, 100)); err != nil {
			return err
		}
		if err := ledger.Grant(ctx, tx, f.a.ID, amt(vcpu, 60)); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, f.b.ID, amt(vcpu, 40))
	})

	// A raise needs the parent's free amount.
	err := try(f.db, func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.b.ID, amt(vcpu, 61)) })
	want(t, err, ledger.QuotaExceeded)
	var e *ledger.Error
	if !errors.As(err, &e) || e.Silo != f.a.ID || e.Asked != ledger.Units(21) || e.Free != ledger.Units(20) {
		t.Errorf("refusal = %+v", e)
	}
	// A charge needs the silo's own free amount; ancestors are not asked.
	charge := func(silo, obj string, n int64) error {
		return try(f.db, func(tx store.Tx) error {
			return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: obj, Silo: silo, Via: ledger.ViaFacade, Amounts: amt(vcpu, n)})
		})
	}
	if err := charge(f.b.ID, "vm-1", 30); err != nil {
		t.Fatal(err)
	}
	want(t, charge(f.b.ID, "vm-2", 11), ledger.QuotaExceeded)
	if err := charge(f.b.ID, "vm-2", 10); err != nil {
		t.Fatal(err)
	}
	want(t, charge(f.a.ID, "vm-3", 21), ledger.QuotaExceeded)
	if err := charge(f.a.ID, "vm-3", 20); err != nil {
		t.Fatal(err)
	}
	if err := charge(f.root.ID, "vm-4", 40); err != nil {
		t.Fatal(err)
	}
	want(t, charge(f.root.ID, "vm-5", 1), ledger.QuotaExceeded)

	// Lowering a grant below what the child holds strands nothing.
	err = try(f.db, func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.a.ID, amt(vcpu, 59)) })
	want(t, err, ledger.FailedPrecondition)
	err = try(f.db, func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.b.ID, amt(vcpu, 39)) })
	want(t, err, ledger.FailedPrecondition)

	// Free the space, then lower.
	do(t, f.db, func(tx store.Tx) error {
		if _, err := ledger.Refund(ctx, tx, "vm-2", nil); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, f.b.ID, amt(vcpu, 30))
	})
	if r := row(t, f.db, f.a.ID, vcpu); r.Granted != ledger.Units(60) || r.Delegated != ledger.Units(30) || r.Used != ledger.Units(20) {
		t.Errorf("/a = %+v", r)
	}
	do(t, f.db, func(tx store.Tx) error {
		es, err := ledger.Edges(ctx, tx, f.b.ID)
		if len(es) != 1 || es[0].From != f.a.ID || es[0].Amount != ledger.Units(30) {
			t.Errorf("edges into /a/b = %+v", es)
		}
		return err
	})
	checkBooks(t, f.db)
}

func TestRefundAndRecount(t *testing.T) {
	f := newFixture(t)
	iops := rk("pd_extreme_iops")
	ssd := rk("ssd_disk_gb")
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, append(amt(iops, 10000), amt(ssd, 1000)...)); err != nil {
			return err
		}
		if err := ledger.Grant(ctx, tx, f.a.ID, append(amt(iops, 5000), amt(ssd, 500)...)); err != nil {
			return err
		}
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "disk-1", Silo: f.a.ID, Via: "ancestor:" + f.root.ID,
			Amounts: append(amt(iops, 3000), amt(ssd, 100)...)})
	})
	do(t, f.db, func(tx store.Tx) error {
		cs, err := ledger.Charges(ctx, tx, "disk-1")
		for _, c := range cs {
			if c.Payer != f.a.ID || c.Home != f.a.ID || c.Via != "ancestor:"+f.root.ID {
				t.Errorf("charge row %+v", c)
			}
		}
		return err
	})
	// Growing charges the difference; lowering IOPS refunds it.
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "disk-1", Silo: f.a.ID, Via: ledger.ViaFacade,
			Amounts: amt(ssd, 50)}); err != nil {
			return err
		}
		got, err := ledger.Refund(ctx, tx, "disk-1", amt(iops, 1000))
		if len(got) != 1 || got[0].Amount != ledger.Units(1000) || got[0].Payer != f.a.ID {
			t.Errorf("refunded %+v", got)
		}
		return err
	})
	if r := row(t, f.db, f.a.ID, ssd); r.Used != ledger.Units(150) {
		t.Errorf("ssd used = %d", r.Used)
	}
	if r := row(t, f.db, f.a.ID, iops); r.Used != ledger.Units(2000) {
		t.Errorf("iops used = %d", r.Used)
	}
	err := try(f.db, func(tx store.Tx) error {
		_, err := ledger.Refund(ctx, tx, "disk-1", amt(iops, 2001))
		return err
	})
	want(t, err, ledger.InvalidArgument)
	checkBooks(t, f.db)

	// The recount puts used back to what the charges say, by payer.
	do(t, f.db, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE quota SET used = used + 7 WHERE silo = ? AND class = 'ssd_disk_gb'`, f.a.ID)
		return err
	})
	do(t, f.db, func(tx store.Tx) error {
		d, err := ledger.Recount(ctx, tx, f.a.ID)
		if len(d) != 1 || d[0].Class != "ssd_disk_gb" || d[0].Was != ledger.Units(150)+7 || d[0].Now != ledger.Units(150) {
			t.Errorf("drift = %+v", d)
		}
		return err
	})
	// The object is gone: refund all of it.
	do(t, f.db, func(tx store.Tx) error {
		got, err := ledger.Refund(ctx, tx, "disk-1", nil)
		if len(got) != 2 {
			t.Errorf("refunded %+v", got)
		}
		return err
	})
	if r := row(t, f.db, f.a.ID, ssd); r.Used != 0 {
		t.Errorf("after the full refund, used = %d", r.Used)
	}
	do(t, f.db, func(tx store.Tx) error {
		cs, err := ledger.Charges(ctx, tx, "disk-1")
		if len(cs) != 0 {
			t.Errorf("charges left: %+v", cs)
		}
		return err
	})
	checkBooks(t, f.db)
}

func TestOverCommit(t *testing.T) {
	f := newFixture(t)
	vcpu := rk("vcpu")
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, amt(vcpu, 100)); err != nil {
			return err
		}
		if err := ledger.Grant(ctx, tx, f.a.ID, amt(vcpu, 80)); err != nil {
			return err
		}
		if err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "vm-1", Silo: f.a.ID, Via: ledger.ViaFacade, Amounts: amt(vcpu, 50)}); err != nil {
			return err
		}
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "vm-root", Silo: f.root.ID, Via: ledger.ViaFacade, Amounts: amt(vcpu, 10)})
	})
	seq := lastSeq(t, f.db)

	// GCE's limit drops under what the tree holds (90).
	do(t, f.db, func(tx store.Tx) error { return ledger.SetCapacity(ctx, tx, f.root.ID, amt(vcpu, 60)) })
	r := row(t, f.db, f.root.ID, vcpu)
	if !r.OverCommitted || r.Free() != ledger.Units(-30) {
		t.Fatalf("root = %+v, want over-committed by 30", r)
	}
	if !hasChange(t, f.db, seq, "quota.over_committed") {
		t.Error("no quota.over_committed change logged")
	}
	do(t, f.db, func(tx store.Tx) error {
		rs, err := ledger.OverCommitted(ctx, tx)
		if len(rs) != 1 || rs[0].Silo != f.root.ID || rs[0].Key != vcpu {
			t.Errorf("OverCommitted = %+v", rs)
		}
		return err
	})
	// The child has 30 free of its own, but nothing new is charged or
	// granted in that class and region anywhere in the tree.
	err := try(f.db, func(tx store.Tx) error {
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "vm-2", Silo: f.a.ID, Via: ledger.ViaFacade, Amounts: amt(vcpu, 1)})
	})
	want(t, err, ledger.QuotaExceeded)
	err = try(f.db, func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.b.ID, amt(vcpu, 1)) })
	want(t, err, ledger.QuotaExceeded)
	// Other classes are unaffected.
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, amt(rk("memory_mb"), 1024)); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, f.a.ID, amt(rk("memory_mb"), 512))
	})
	// Nothing is reclaimed: the child keeps its grant and its charge.
	if r := row(t, f.db, f.a.ID, vcpu); r.Granted != ledger.Units(80) || r.Used != ledger.Units(50) {
		t.Errorf("/a = %+v", r)
	}
	checkBooks(t, f.db)

	// Lowering the child's grant and a refund bring it back under.
	do(t, f.db, func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.a.ID, amt(vcpu, 55)) })
	if r := row(t, f.db, f.root.ID, vcpu); !r.OverCommitted {
		t.Errorf("root back under too soon: %+v", r)
	}
	do(t, f.db, func(tx store.Tx) error {
		_, err := ledger.Refund(ctx, tx, "vm-root", nil)
		return err
	})
	if r := row(t, f.db, f.root.ID, vcpu); r.OverCommitted || r.Free() != ledger.Units(5) {
		t.Errorf("root = %+v, want back under with 5 free", r)
	}
	do(t, f.db, func(tx store.Tx) error {
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "vm-2", Silo: f.a.ID, Via: ledger.ViaFacade, Amounts: amt(vcpu, 5)})
	})
	checkBooks(t, f.db)
}

func lastSeq(t *testing.T, db store.Store) uint64 {
	t.Helper()
	var n uint64
	do(t, db, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT seq FROM change_counter`).Scan(&n)
	})
	return n
}

func hasChange(t *testing.T, db store.Store, since uint64, kind string) bool {
	t.Helper()
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := db.Changes(c, since)
	if err != nil {
		t.Fatal(err)
	}
	end := lastSeq(t, db)
	for since < end {
		select {
		case e := <-ch:
			since = e.Seq
			if e.Kind == kind {
				return true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("change log stalled")
		}
	}
	return false
}

func TestRegions(t *testing.T) {
	f := newFixture(t)
	vcpu := rk("vcpu")
	eu := ledger.Key{Kind: "gce", Class: "vcpu", Region: "europe-west4"}
	do(t, f.db, func(tx store.Tx) error {
		return ledger.SetCapacity(ctx, tx, f.root.ID, append(amt(vcpu, 100), amt(eu, 100)...))
	})
	// A child's regions are a subset of its parent's.
	err := try(f.db, func(tx store.Tx) error {
		return ledger.SetRegions(ctx, tx, f.b.ID, []string{"us-central1", "europe-west4"})
	})
	want(t, err, ledger.LooserThanParent)
	err = try(f.db, func(tx store.Tx) error { return ledger.SetRegions(ctx, tx, f.a.ID, []string{"asia-east1"}) })
	want(t, err, ledger.LooserThanParent)
	// Quota only in an allowed region.
	err = try(f.db, func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.a.ID, amt(eu, 1)) })
	want(t, err, ledger.FailedPrecondition)
	err = try(f.db, func(tx store.Tx) error {
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "vm", Silo: f.a.ID, Via: ledger.ViaFacade, Amounts: amt(eu, 1)})
	})
	want(t, err, ledger.FailedPrecondition)
	// Adding a region grants nothing.
	do(t, f.db, func(tx store.Tx) error {
		return ledger.SetRegions(ctx, tx, f.a.ID, []string{"us-central1", "europe-west4"})
	})
	if r := row(t, f.db, f.a.ID, eu); r.Granted != 0 {
		t.Errorf("adding a region granted %d", r.Granted)
	}
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetRegions(ctx, tx, f.b.ID, []string{"us-central1", "europe-west4"}); err != nil {
			return err
		}
		if err := ledger.Grant(ctx, tx, f.a.ID, amt(eu, 10)); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, f.b.ID, amt(eu, 4))
	})
	// A descendant holds quota in europe-west4: /a can't lose it.
	err = try(f.db, func(tx store.Tx) error { return ledger.SetRegions(ctx, tx, f.a.ID, []string{"us-central1"}) })
	want(t, err, ledger.FailedPrecondition)
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.Grant(ctx, tx, f.b.ID, amt(eu, 0)); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, f.a.ID, amt(eu, 0))
	})
	// An object with no quota row left still pins the region (the
	// reconcile can find one before it recounts).
	do(t, f.db, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO charges (object, payer, home, via, kind, class, region, amount, created_at, updated_at)
			VALUES ('stray', ?, ?, 'facade', 'gce', 'instances', 'europe-west4', 0, 0, 0)`, f.b.ID, f.b.ID)
		return err
	})
	err = try(f.db, func(tx store.Tx) error { return ledger.SetRegions(ctx, tx, f.a.ID, []string{"us-central1"}) })
	want(t, err, ledger.FailedPrecondition)
	do(t, f.db, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM charges WHERE object = 'stray'`)
		return err
	})
	// So does a subnet, charged to a global count: its own range in
	// the address space names its region.
	do(t, f.db, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO address_own (silo, object, start, bits, region, updated_at)
			VALUES (?, 'subnetworks/europe-west4/web', 0, 24, 'europe-west4', 0)`, f.b.ID)
		return err
	})
	err = try(f.db, func(tx store.Tx) error { return ledger.SetRegions(ctx, tx, f.a.ID, []string{"us-central1"}) })
	want(t, err, ledger.FailedPrecondition)
	do(t, f.db, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM address_own WHERE object = 'subnetworks/europe-west4/web'`)
		return err
	})
	// Now it goes, from /a and from /a/b with it.
	do(t, f.db, func(tx store.Tx) error { return ledger.SetRegions(ctx, tx, f.a.ID, []string{"us-central1"}) })
	do(t, f.db, func(tx store.Tx) error {
		rs, err := ledger.Regions(ctx, tx, f.b.ID)
		if len(rs) != 1 || rs[0] != "us-central1" {
			t.Errorf("/a/b's regions = %v", rs)
		}
		return err
	})
	// Global classes have no region to allow.
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, amt(gk("subnetworks"), 10)); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, f.a.ID, amt(gk("subnetworks"), 2))
	})
	err = try(f.db, func(tx store.Tx) error { return ledger.SetRegions(ctx, tx, f.a.ID, []string{"global"}) })
	want(t, err, ledger.InvalidArgument)
	checkBooks(t, f.db)
}

func TestModelBudget(t *testing.T) {
	f := newFixture(t)
	usd := ledger.ModelUSD
	reserve := func(silo, agent string, micros int64) error {
		return try(f.db, func(tx store.Tx) error {
			return ledger.ReserveModelBudget(ctx, tx, silo, agent, ledger.ViaFacade, micros)
		})
	}
	// --root-model-budget defaults to 0: nobody can reserve anything.
	want(t, reserve(f.root.ID, "ag1", 1), ledger.QuotaExceeded)
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, amt(usd, 200)); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, f.a.ID, amt(usd, 40))
	})
	if err := reserve(f.a.ID, "ag1", 30_000_000); err != nil {
		t.Fatal(err)
	}
	want(t, reserve(f.a.ID, "ag2", 10_000_001), ledger.QuotaExceeded)
	if err := reserve(f.a.ID, "ag2", 250); err != nil { // $0.00025
		t.Fatal(err)
	}
	// Raising reserves the difference.
	want(t, reserve(f.a.ID, "ag1", 40_000_000), ledger.QuotaExceeded)
	if err := reserve(f.a.ID, "ag1", 39_999_750); err != nil {
		t.Fatal(err)
	}
	if r := row(t, f.db, f.a.ID, usd); r.Used != ledger.Units(40) || r.Free() != 0 {
		t.Errorf("/a model row = %+v", r)
	}
	// Lowering the grant below what the agents hold fails.
	err := try(f.db, func(tx store.Tx) error { return ledger.Grant(ctx, tx, f.a.ID, amt(usd, 39)) })
	want(t, err, ledger.FailedPrecondition)
	// An agent's budget is held by its own silo.
	want(t, reserve(f.b.ID, "ag1", 1), ledger.FailedPrecondition)
	// Lowering an agent's budget, and deleting an agent, give it back.
	if err := reserve(f.a.ID, "ag1", 5_000_000); err != nil {
		t.Fatal(err)
	}
	do(t, f.db, func(tx store.Tx) error {
		n, err := ledger.ModelBudget(ctx, tx, "ag1")
		if n != 5_000_000 {
			t.Errorf("ag1 holds %d", n)
		}
		if err != nil {
			return err
		}
		return ledger.ReturnModelBudget(ctx, tx, "ag2")
	})
	if r := row(t, f.db, f.a.ID, usd); r.Used != 5_000_000 {
		t.Errorf("/a model used = %d", r.Used)
	}
	if got := ledger.FormatAmount(row(t, f.db, f.root.ID, usd).Free()); got != "160" {
		t.Errorf("root model free = %s", got)
	}
	checkBooks(t, f.db)

	// The period is a deployment setting on the class.
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetBudgetPeriod(ctx, tx, ledger.PeriodWeek); err != nil {
			return err
		}
		p, err := ledger.BudgetPeriod(ctx, tx)
		if p != ledger.PeriodWeek {
			t.Errorf("period = %q", p)
		}
		return err
	})
	err = try(f.db, func(tx store.Tx) error { return ledger.SetBudgetPeriod(ctx, tx, "year") })
	want(t, err, ledger.InvalidArgument)

	at := time.Date(2026, 10, 8, 15, 4, 5, 0, time.UTC) // a Thursday
	for p, w := range map[string]time.Time{
		ledger.PeriodMonth: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ledger.PeriodWeek:  time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		ledger.PeriodDay:   time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
	} {
		if got, err := ledger.PeriodStart(p, at); err != nil || !got.Equal(w) {
			t.Errorf("PeriodStart(%s) = %v, %v; want %v", p, got, err, w)
		}
	}
	if got, _ := ledger.PeriodStart(ledger.PeriodWeek, time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a Sunday's week starts %v", got)
	}
}

func TestPreview(t *testing.T) {
	f := newFixture(t)
	vcpu := rk("vcpu")
	do(t, f.db, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, amt(vcpu, 100)); err != nil {
			return err
		}
		if err := ledger.Grant(ctx, tx, f.a.ID, amt(vcpu, 60)); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, f.b.ID, amt(vcpu, 20))
	})
	before := lastSeq(t, f.db)
	do(t, f.db, func(tx store.Tx) error {
		p, err := ledger.PreviewCharge(ctx, tx, f.b.ID, amt(vcpu, 8))
		if err != nil {
			return err
		}
		if p.Refusal != nil || len(p.Charges) != 1 || p.Charges[0] != (ledger.Payment{Payer: f.b.ID, Key: vcpu, Amount: ledger.Units(8)}) {
			t.Errorf("charge preview = %+v", p)
		}
		wantLevels(t, p.Levels, []string{f.b.ID, f.a.ID, f.root.ID}, []int64{20, 40, 40}, []int64{12, 40, 40})

		p, err = ledger.PreviewCharge(ctx, tx, f.b.ID, amt(vcpu, 21))
		if err != nil {
			return err
		}
		want(t, p.Refusal, ledger.QuotaExceeded)

		p, err = ledger.PreviewGrant(ctx, tx, f.b.ID, amt(vcpu, 30))
		if err != nil {
			return err
		}
		if p.Refusal != nil || len(p.Charges) != 1 || p.Charges[0] != (ledger.Payment{Payer: f.a.ID, Key: vcpu, Amount: ledger.Units(10)}) {
			t.Errorf("grant preview = %+v", p)
		}
		wantLevels(t, p.Levels, []string{f.b.ID, f.a.ID, f.root.ID}, []int64{20, 40, 40}, []int64{30, 30, 40})

		p, err = ledger.PreviewGrant(ctx, tx, f.b.ID, amt(vcpu, 61))
		if err != nil {
			return err
		}
		want(t, p.Refusal, ledger.QuotaExceeded)
		return nil
	})
	if r := row(t, f.db, f.b.ID, vcpu); r.Used != 0 || r.Granted != ledger.Units(20) {
		t.Errorf("a preview changed /a/b: %+v", r)
	}
	if lastSeq(t, f.db) != before {
		t.Error("a preview wrote to the change log")
	}
}

func wantLevels(t *testing.T, got []ledger.Level, silos []string, before, after []int64) {
	t.Helper()
	if len(got) != len(silos) {
		t.Fatalf("levels = %+v", got)
	}
	for i, l := range got {
		if l.Silo != silos[i] || l.FreeBefore != ledger.Units(before[i]) || l.FreeAfter != ledger.Units(after[i]) {
			t.Errorf("level %d = %+v, want %s %d -> %d", i, l, silos[i], before[i], after[i])
		}
	}
}

// TestConcurrent: many goroutines charging, refunding, granting and
// moving the root's capacity at once never break the books.
func TestConcurrent(t *testing.T) {
	f := newFixture(t)
	keys := []ledger.Key{rk("vcpu"), gk("subnetworks"), ledger.ModelUSD}
	var silos []*tree.Silo
	do(t, f.db, func(tx store.Tx) error {
		var caps []ledger.Amount
		for _, k := range keys {
			caps = append(caps, ledger.Amount{Key: k, Amount: ledger.Units(1000)})
		}
		if err := ledger.SetCapacity(ctx, tx, f.root.ID, caps); err != nil {
			return err
		}
		silos = []*tree.Silo{f.root, f.a, f.b}
		for i := range 3 {
			c, err := tree.CreateChild(ctx, tx, f.a.ID, fmt.Sprintf("c%d", i), tree.BackingSubnet, nil)
			if err != nil {
				return err
			}
			if err := ledger.SetRegions(ctx, tx, c.ID, []string{"us-central1"}); err != nil {
				return err
			}
			silos = append(silos, c)
		}
		return nil
	})

	const workers, ops = 16, 60
	var wg sync.WaitGroup
	var unexpected sync.Map
	var okN, refusedN atomic.Int64
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			for i := range ops {
				k := keys[rng.IntN(len(keys))]
				s := silos[rng.IntN(len(silos))]
				n := ledger.Units(int64(rng.IntN(120)))
				obj := fmt.Sprintf("obj-%d", rng.IntN(40))
				var err error
				switch op := rng.IntN(10); {
				case op < 5:
					err = try(f.db, func(tx store.Tx) error {
						return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: obj, Silo: s.ID, Via: ledger.ViaFacade,
							Amounts: []ledger.Amount{{Key: k, Amount: n}}})
					})
				case op < 7:
					err = try(f.db, func(tx store.Tx) error { _, err := ledger.Refund(ctx, tx, obj, nil); return err })
				case op < 9:
					if s.IsRoot() {
						continue
					}
					err = try(f.db, func(tx store.Tx) error {
						return ledger.Grant(ctx, tx, s.ID, []ledger.Amount{{Key: k, Amount: n * 3}})
					})
				default:
					err = try(f.db, func(tx store.Tx) error {
						return ledger.SetCapacity(ctx, tx, f.root.ID, []ledger.Amount{{Key: k, Amount: ledger.Units(600) + n*4}})
					})
				}
				switch c := ledger.CodeOf(err); {
				case err == nil:
					okN.Add(1)
				case c == ledger.QuotaExceeded || c == ledger.FailedPrecondition:
					refusedN.Add(1)
				default:
					unexpected.Store(fmt.Sprint(w, i), err)
				}
				// The books balance after every step, not only at the end.
				if i%10 == 0 {
					_ = try(f.db, func(tx store.Tx) error {
						bad, err := ledger.Check(ctx, tx)
						for _, b := range bad {
							unexpected.Store(fmt.Sprint(w, i, b), errors.New(b))
						}
						return err
					})
				}
			}
		}()
	}
	wg.Wait()
	unexpected.Range(func(k, v any) bool { t.Errorf("%v: %v", k, v); return true })
	if okN.Load() < ops || refusedN.Load() < ops {
		t.Errorf("%d operations succeeded and %d were refused: the mix doesn't exercise both", okN.Load(), refusedN.Load())
	}
	checkBooks(t, f.db)
	do(t, f.db, func(tx store.Tx) error {
		for _, s := range silos[1:] {
			rows, err := ledger.Quota(ctx, tx, s.ID)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if r.Delegated+r.Used > r.Granted {
					t.Errorf("%s %s: delegated %d + used %d > granted %d", s.Path, r.Key, r.Delegated, r.Used, r.Granted)
				}
			}
		}
		return nil
	})
}

func TestAmounts(t *testing.T) {
	for s, w := range map[string]int64{"0": 0, "16": 16_000_000, "0.00025": 250, "200.5": 200_500_000, "1.000001": 1_000_001} {
		got, err := ledger.ParseAmount(s)
		if err != nil || got != w {
			t.Errorf("ParseAmount(%q) = %d, %v; want %d", s, got, err, w)
		}
		if back := ledger.FormatAmount(got); back != s {
			t.Errorf("FormatAmount(%d) = %q, want %q", got, back, s)
		}
	}
	for _, s := range []string{"", "-1", "1.0000001", "abc", ".5", "1.-5", "1e3"} {
		if _, err := ledger.ParseAmount(s); err == nil {
			t.Errorf("ParseAmount(%q) succeeded", s)
		}
	}
}
