package hostcap_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/hostcap"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/tree"
)

var ctx = context.Background()

func TestCapacity(t *testing.T) {
	got := hostcap.Capacity(hostcap.Totals{CPUs: 8, MemoryMB: 16384, DiskGB: 100}, hostcap.Reserve{CacheGB: 9.5})
	want := map[string]int64{"vcpu_m": 14000, "memory_mb": 14336, "disk_gb": 80}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	// 10% of memory is more than 2 GiB; one CPU and no disk give none.
	got = hostcap.Capacity(hostcap.Totals{CPUs: 1, MemoryMB: 65536}, hostcap.Reserve{CPUOvercommit: 4, CacheGB: 10})
	if got["vcpu_m"] != 0 || got["memory_mb"] != 65536-6553 || got["disk_gb"] != 0 {
		t.Errorf("capacity = %v", got)
	}
}

func TestMemTotalMB(t *testing.T) {
	if got := hostcap.MemTotalMB("MemFree:  10 kB\nMemTotal:       16384000 kB\n"); got != 16000 {
		t.Errorf("MemTotalMB = %d, want 16000", got)
	}
	if got := hostcap.MemTotalMB("MemFree: 10 kB\n"); got != 0 {
		t.Errorf("MemTotalMB without MemTotal = %d", got)
	}
}

func TestReadHost(t *testing.T) {
	got, err := hostcap.ReadHost(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.CPUs < 1 || got.MemoryMB < 1 || got.DiskGB < 0 {
		t.Errorf("ReadHost = %+v", got)
	}
}

type fixture struct {
	db     store.Database
	root   *tree.Silo
	totals hostcap.Totals
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	fx := &fixture{db: db, totals: hostcap.Totals{CPUs: 8, MemoryMB: 16384, DiskGB: 200}}
	do(t, db, func(tx store.Tx) error {
		if fx.root, err = tree.CreateRoot(ctx, tx, "root", tree.BackingLocal, nil); err != nil {
			return err
		}
		if err := ledger.SetRegions(ctx, tx, fx.root.ID, []string{ledger.Local}); err != nil {
			return err
		}
		s := fenceapi.DefaultCacheSettings()
		s.SizeLimitGB = 20
		return fenceapi.WriteCacheSettings(ctx, tx, s)
	})
	return fx
}

func (fx *fixture) refresher() *hostcap.Refresher {
	return hostcap.New(fx.db, hostcap.Config{Read: func() (hostcap.Totals, error) { return fx.totals, nil }})
}

func do(t *testing.T, db store.Store, fn func(tx store.Tx) error) {
	t.Helper()
	if err := db.Tx(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

func (fx *fixture) granted(t *testing.T, class string) int64 {
	t.Helper()
	var row ledger.Row
	do(t, fx.db, func(tx store.Tx) (err error) {
		row, err = ledger.Get(ctx, tx, fx.root.ID, hostcap.Key(class))
		return err
	})
	return row.Granted / ledger.Micro
}

// resyncs are the root's quota.root_resync records.
func (fx *fixture) resyncs(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	do(t, fx.db, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT data FROM changes WHERE kind = ? ORDER BY seq`, "quota.root_resync")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				return err
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out
}

func TestRefresh(t *testing.T) {
	fx := newFixture(t)
	r := fx.refresher()
	res, err := r.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 200 GiB less 20 for the reserve and 20 for the cache's budget.
	for class, want := range map[string]int64{"vcpu_m": 14000, "memory_mb": 14336, "disk_gb": 160} {
		if got := fx.granted(t, class); got != want {
			t.Errorf("%s = %d, want %d", class, got, want)
		}
	}
	if len(res.Changed) != 3 || len(res.Refused) != 0 {
		t.Errorf("changed %v, refused %v", res.Changed, res.Refused)
	}
	if n := len(fx.resyncs(t)); n != 1 {
		t.Fatalf("%d quota.root_resync records, want 1", n)
	}

	// The same host again changes nothing and logs nothing.
	if res, err = r.Refresh(ctx); err != nil || len(res.Changed) != 0 {
		t.Fatalf("second Refresh: changed %v, %v", res, err)
	}
	if n := len(fx.resyncs(t)); n != 1 {
		t.Errorf("%d quota.root_resync records after an unchanged read, want 1", n)
	}

	// More CPUs and a smaller cache budget move the root with them.
	fx.totals.CPUs = 16
	do(t, fx.db, func(tx store.Tx) error {
		s := fenceapi.DefaultCacheSettings()
		s.SizeLimitGB = 10
		return fenceapi.WriteCacheSettings(ctx, tx, s)
	})
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.granted(t, "vcpu_m"); got != 30000 {
		t.Errorf("vcpu_m = %d, want 30000", got)
	}
	if got := fx.granted(t, "disk_gb"); got != 170 {
		t.Errorf("disk_gb = %d, want 170", got)
	}
}

// A host that shrank below what the tree holds keeps that class's
// capacity, and says so, while the others still follow the host.
func TestRefreshRefusesBelowHeld(t *testing.T) {
	fx := newFixture(t)
	r := fx.refresher()
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	do(t, fx.db, func(tx store.Tx) error {
		kid, err := tree.CreateChild(ctx, tx, fx.root.ID, "team", tree.BackingLocal, nil)
		if err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, kid.ID, []ledger.Amount{{Key: hostcap.Key("memory_mb"), Amount: ledger.Units(12000)}})
	})

	fx.totals.MemoryMB = 8192 // 6144 MiB of capacity, below the 12000 granted
	fx.totals.CPUs = 4
	res, err := r.Refresh(ctx)
	if !errors.Is(err, hostcap.ErrHeld) {
		t.Fatalf("Refresh = %v, want ErrHeld", err)
	}
	if got := fx.granted(t, "memory_mb"); got != 14336 {
		t.Errorf("memory_mb = %d, want 14336, as it was", got)
	}
	if got := fx.granted(t, "vcpu_m"); got != 6000 {
		t.Errorf("vcpu_m = %d, want 6000", got)
	}
	if len(res.Refused) != 1 || res.Refused[0].Class != "memory_mb" {
		t.Errorf("refused %v", res.Refused)
	}
	recs := fx.resyncs(t)
	if last := recs[len(recs)-1]; last["refused"] == nil {
		t.Errorf("quota.root_resync %v names nothing refused", last)
	}
	do(t, fx.db, func(tx store.Tx) error {
		over, err := ledger.OverCommitted(ctx, tx)
		if len(over) != 0 {
			t.Errorf("over-committed %v", over)
		}
		return err
	})

	// Lowering it to no less than what is held is fine.
	fx.totals.MemoryMB = 14336 // 12288 MiB
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fx.granted(t, "memory_mb"); got != 12288 {
		t.Errorf("memory_mb = %d, want 12288", got)
	}
}

func TestRefreshBeforeBootstrap(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	r := hostcap.New(db, hostcap.Config{Read: func() (hostcap.Totals, error) { return hostcap.Totals{CPUs: 2}, nil }})
	if _, err := r.Refresh(ctx); !errors.Is(err, tree.ErrNotFound) {
		t.Errorf("Refresh = %v, want tree.ErrNotFound", err)
	}
}

func TestRefreshReadFails(t *testing.T) {
	fx := newFixture(t)
	r := hostcap.New(fx.db, hostcap.Config{Read: func() (hostcap.Totals, error) { return hostcap.Totals{}, errors.New("no /proc") }})
	if _, err := r.Refresh(ctx); err == nil {
		t.Fatal("Refresh with a failed read succeeded")
	}
	if got := fx.granted(t, "vcpu_m"); got != 0 {
		t.Errorf("vcpu_m = %d after a failed read", got)
	}
}

func TestGuard(t *testing.T) {
	fx := newFixture(t)
	stop := errors.New("not the leader")
	r := hostcap.New(fx.db, hostcap.Config{Read: func() (hostcap.Totals, error) { return fx.totals, nil },
		Guard: func(context.Context, store.Tx) error { return stop }})
	if _, err := r.Refresh(ctx); !errors.Is(err, stop) {
		t.Errorf("Refresh = %v, want the guard's error", err)
	}
}
