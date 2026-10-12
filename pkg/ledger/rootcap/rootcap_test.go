package rootcap_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/gceclient"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/ledger/rootcap"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/tree"
	compute "google.golang.org/api/compute/v1"
)

const (
	proj = "host-project"
	usc  = "us-central1"
	euw  = "europe-west4"
)

var ctx = context.Background()

type fixture struct {
	db   store.Database
	fake *fake.Fake
	root *tree.Silo
}

// newFixture is a database with a root allowed us-central1 and
// europe-west4, and a fake GCE with no limits set and the control
// plane's network.
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
	fx := &fixture{db: db, fake: fake.New(fake.Options{Project: proj, OperationLatency: time.Millisecond})}
	do(t, db, func(tx store.Tx) error {
		if fx.root, err = tree.CreateRoot(ctx, tx, "root", nil); err != nil {
			return err
		}
		return ledger.SetRegions(ctx, tx, fx.root.ID, []string{usc, euw})
	})
	fx.wait(t)(fx.fake.InsertNetwork(ctx, proj, &compute.Network{Name: "silo"}))
	return fx
}

func do(t *testing.T, db store.Store, fn func(tx store.Tx) error) {
	t.Helper()
	if err := db.Tx(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

func (fx *fixture) wait(t *testing.T) func(*compute.Operation, error) {
	return func(op *compute.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		op, err = gce.Await(ctx, fx.fake, proj, op)
		if err != nil || op.Error != nil {
			t.Fatalf("operation %+v %v", op, err)
		}
	}
}

func (fx *fixture) refresh(t *testing.T, cfg rootcap.Config) *rootcap.Result {
	t.Helper()
	return fx.refreshWith(t, fx.fake, cfg)
}

func (fx *fixture) refreshWith(t *testing.T, c gce.Client, cfg rootcap.Config) *rootcap.Result {
	t.Helper()
	cfg.Project = proj
	res, err := rootcap.New(fx.db, c, cfg).Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func key(region, class string) ledger.Key {
	return ledger.Key{Kind: "gce", Class: class, Region: region}
}

// granted is the root's capacity of class in region, in whole units.
func (fx *fixture) granted(t *testing.T, region, class string) int64 {
	t.Helper()
	var r ledger.Row
	do(t, fx.db, func(tx store.Tx) error {
		var err error
		r, err = ledger.Get(ctx, tx, fx.root.ID, key(region, class))
		return err
	})
	if r.Granted == ledger.Unlimited {
		return -1
	}
	return r.Granted / ledger.Micro
}

// changes are the change log's entries of kind.
func (fx *fixture) changes(t *testing.T, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	do(t, fx.db, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT data FROM changes WHERE kind = ? ORDER BY seq`, kind)
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

func hasKey(keys []ledger.Key, k ledger.Key) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func TestRefresh(t *testing.T) {
	fx := newFixture(t)
	f := fx.fake
	f.SetQuota(usc, gce.MetricCPUS, 100)
	f.SetQuota(euw, gce.MetricCPUS, 50)
	f.SetQuota(usc, gce.MetricPreemptibleCPUS, 200)
	f.SetQuota(usc, gce.MetricInstances, 40)
	f.SetQuota(usc, gce.MetricDisksTotalGB, 4096)
	f.SetQuota(usc, gce.MetricSSDTotalGB, 2048)
	f.SetQuota(usc, gce.MetricHDBTotalIOPS, 160000)
	f.SetQuota(usc, gce.MetricHDXTotalGB, 1000) // only Cloud Quotas reports it
	f.SetQuota(usc, gce.MetricHDMLTotalThroughput, 2400)
	f.SetQuota(usc, "NVIDIA_T4_GPUS", 4)
	f.SetQuota(usc, "PREEMPTIBLE_NVIDIA_T4_GPUS", 8)
	f.SetQuota("", gce.MetricFirewalls, 100)
	f.SetQuota("", gce.MetricSubnetworks, 50)
	f.SetQuota("", gce.MetricImages, 30)
	f.SetQuota("", gce.MetricGPUsAllRegions, 12)

	// The control plane: its VM (and boot disk), the gateway subnet and
	// an install firewall rule. A tenant's VM is not the control plane.
	fx.wait(t)(f.InsertSubnetwork(ctx, proj, usc, &compute.Subnetwork{Name: "silo-gateway", Network: "global/networks/silo", IpCidrRange: "10.255.255.0/24"}))
	fx.wait(t)(f.InsertSubnetwork(ctx, proj, usc, &compute.Subnetwork{Name: "k7f3qa-sub", Network: "global/networks/silo", IpCidrRange: "10.0.0.0/24"}))
	for _, name := range []string{"silo-control", "k7f3qa-vm"} {
		sub := "silo-gateway"
		if name != "silo-control" {
			sub = "k7f3qa-sub"
		}
		fx.wait(t)(f.InsertInstance(ctx, proj, usc+"-a", &compute.Instance{
			Name: name, MachineType: "zones/us-central1-a/machineTypes/e2-standard-4",
			Disks: []*compute.AttachedDisk{{Boot: true, InitializeParams: &compute.AttachedDiskInitializeParams{
				SourceImage: "projects/debian-cloud/global/images/family/debian-12", DiskType: "zones/us-central1-a/diskTypes/pd-balanced", DiskSizeGb: 20}}},
			NetworkInterfaces: []*compute.NetworkInterface{{Network: "global/networks/silo", Subnetwork: "regions/us-central1/subnetworks/" + sub}},
		}, ""))
	}
	fx.wait(t)(f.InsertFirewall(ctx, proj, &compute.Firewall{Name: "silo-allow-iap", Network: "global/networks/silo",
		SourceRanges: []string{"35.235.240.0/20"}, Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}}))

	res := fx.refresh(t, rootcap.Config{})
	for _, c := range []struct {
		region, class string
		want          int64
	}{
		{usc, "vcpu", 96}, // 100, less the control VM's 4
		{euw, "vcpu", 50},
		{usc, "spot_vcpu", 200},
		{usc, "instances", 39},
		{usc, "standard_disk_gb", 4096},
		{usc, "ssd_disk_gb", 2028}, // less the control VM's boot disk
		{usc, "hyperdisk_balanced_iops", 160000},
		{usc, "hyperdisk_extreme_gb", 1000},
		{usc, "hyperdisk_ml_throughput", 2400},
		{usc, "nvidia_t4_gpus", 4},
		{usc, "spot_nvidia_t4_gpus", 8},
		{euw, "nvidia_t4_gpus", 0},
		{usc, "memory_mb", -1},
		{euw, "spot_memory_mb", -1},
		{ledger.Global, "firewall_rules", 99},
		{ledger.Global, "subnetworks", 49},
		{ledger.Global, "images", 30},
		{ledger.Global, "snapshots", 0},
		{ledger.Global, "gpus_all_regions", 12},
		{ledger.Global, "image_storage_gb", -1},
		{ledger.Global, "agent_sessions", 0}, // install's, not GCE's
	} {
		if got := fx.granted(t, c.region, c.class); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.region, c.class, got, c.want)
		}
	}
	for _, k := range []ledger.Key{key(euw, "instances"), key(usc, "hyperdisk_throughput_gb"), key(euw, "nvidia_t4_gpus"), key(ledger.Global, "snapshots")} {
		if !hasKey(res.NoCapacity, k) {
			t.Errorf("NoCapacity lacks %s", k)
		}
	}
	if hasKey(res.NoCapacity, key(usc, "vcpu")) || hasKey(res.NoCapacity, key(usc, "memory_mb")) {
		t.Errorf("NoCapacity %v", res.NoCapacity)
	}
	if len(res.OverCommitted) != 0 || len(res.Changed) == 0 {
		t.Fatalf("result %+v", res)
	}
	if n := len(fx.changes(t, "quota.root_resync")); n != 1 {
		t.Fatalf("%d quota.root_resync records", n)
	}

	// A second read that finds nothing new changes and logs nothing.
	res = fx.refresh(t, rootcap.Config{})
	if len(res.Changed) != 0 || len(res.AddedClasses) != 0 {
		t.Fatalf("second refresh changed %v, added %v", res.Changed, res.AddedClasses)
	}
	if n := len(fx.changes(t, "quota.root_resync")); n != 1 {
		t.Fatalf("%d quota.root_resync records after a quiet read", n)
	}

	// A limit that moves moves the capacity.
	f.SetQuota(euw, gce.MetricCPUS, 64)
	res = fx.refresh(t, rootcap.Config{})
	if got := fx.granted(t, euw, "vcpu"); got != 64 || len(res.Changed) != 1 {
		t.Fatalf("europe-west4 vcpu %d, changed %v", got, res.Changed)
	}
}

func TestRootMemory(t *testing.T) {
	fx := newFixture(t)
	fx.refresh(t, rootcap.Config{})
	if got := fx.granted(t, usc, "memory_mb"); got != -1 {
		t.Fatalf("memory without --root-memory: %d", got)
	}
	fx.refresh(t, rootcap.Config{RootMemoryMB: 65536, SnapshotStorageGB: 500})
	for _, c := range []struct{ region, class string }{{usc, "memory_mb"}, {usc, "spot_memory_mb"}, {euw, "memory_mb"}} {
		if got := fx.granted(t, c.region, c.class); got != 65536 {
			t.Fatalf("%s %s = %d", c.region, c.class, got)
		}
	}
	if got := fx.granted(t, ledger.Global, "snapshot_storage_gb"); got != 500 {
		t.Fatalf("snapshot storage %d", got)
	}
	// Taking it away makes memory unlimited again.
	fx.refresh(t, rootcap.Config{})
	if got := fx.granted(t, usc, "memory_mb"); got != -1 {
		t.Fatalf("memory after --root-memory is dropped: %d", got)
	}
}

func TestMissingOrZeroGivesNoCapacity(t *testing.T) {
	fx := newFixture(t)
	fx.fake.SetQuota(usc, gce.MetricCPUS, 10)
	fx.fake.SetQuota(usc, gce.MetricPreemptibleCPUS, 0)
	fx.fake.SetQuota(usc, "NVIDIA_L4_GPUS", 8)
	fx.fake.SetQuota(usc, "PREEMPTIBLE_NVIDIA_L4_GPUS", 0)
	res := fx.refresh(t, rootcap.Config{})
	for _, c := range []struct {
		class string
		want  int64
	}{{"vcpu", 10}, {"spot_vcpu", 0}, {"nvidia_l4_gpus", 8}, {"spot_nvidia_l4_gpus", 0}, {"hyperdisk_ml_gb", 0}} {
		if got := fx.granted(t, usc, c.class); got != c.want {
			t.Errorf("%s = %d, want %d", c.class, got, c.want)
		}
	}
	for _, class := range []string{"spot_vcpu", "spot_nvidia_l4_gpus", "hyperdisk_ml_gb"} {
		if !hasKey(res.NoCapacity, key(usc, class)) {
			t.Errorf("NoCapacity lacks %s", class)
		}
	}
	// A limit that disappears takes the capacity with it.
	fx.fake.ClearQuota(usc, "NVIDIA_L4_GPUS")
	fx.refresh(t, rootcap.Config{})
	if got := fx.granted(t, usc, "nvidia_l4_gpus"); got != 0 {
		t.Fatalf("L4 after its limit went: %d", got)
	}
}

func TestDiscoversGPUFamilies(t *testing.T) {
	fx := newFixture(t)
	// H100 isn't in silod's GPU config, and only Cloud Quotas reports
	// it.
	fx.fake.SetQuota(usc, "NVIDIA_H100_GPUS", 16)
	fx.fake.SetQuota(usc, "PREEMPTIBLE_NVIDIA_H100_GPUS", 32)
	res := fx.refresh(t, rootcap.Config{})
	if len(res.AddedClasses) != 2 || res.AddedClasses[0] != "nvidia_h100_gpus" || res.AddedClasses[1] != "spot_nvidia_h100_gpus" {
		t.Fatalf("added %v", res.AddedClasses)
	}
	if got := fx.granted(t, usc, "nvidia_h100_gpus"); got != 16 {
		t.Fatalf("H100 capacity %d", got)
	}
	if got := fx.granted(t, usc, "spot_nvidia_h100_gpus"); got != 32 {
		t.Fatalf("Spot H100 capacity %d", got)
	}
	do(t, fx.db, func(tx store.Tx) error {
		for _, spot := range []bool{false, true} {
			g, err := ledger.LookupGPU(ctx, tx, "nvidia-h100-80gb", spot)
			if err != nil {
				return err
			}
			if want := map[bool]string{false: "nvidia_h100_gpus", true: "spot_nvidia_h100_gpus"}[spot]; g.Class != want {
				t.Errorf("nvidia-h100-80gb spot=%v charges %s", spot, g.Class)
			}
		}
		return nil
	})
	recs := fx.changes(t, "quota.root_resync")
	if len(recs) != 1 || recs[0]["added_classes"] == nil {
		t.Fatalf("root_resync %v", recs)
	}
	// The next read finds it known.
	if res := fx.refresh(t, rootcap.Config{}); len(res.AddedClasses) != 0 {
		t.Fatalf("added again: %v", res.AddedClasses)
	}
}

func TestOverCommitted(t *testing.T) {
	fx := newFixture(t)
	fx.fake.SetQuota(usc, gce.MetricCPUS, 100)
	fx.refresh(t, rootcap.Config{})
	var child *tree.Silo
	do(t, fx.db, func(tx store.Tx) error {
		var err error
		if child, err = tree.CreateChild(ctx, tx, fx.root.ID, "a", tree.BackingSubnet, nil); err != nil {
			return err
		}
		if err := ledger.SetRegions(ctx, tx, child.ID, []string{usc}); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, child.ID, []ledger.Amount{{Key: key(usc, "vcpu"), Amount: ledger.Units(80)}})
	})

	// GCE's limit drops below what the tree holds.
	fx.fake.SetQuota(usc, gce.MetricCPUS, 50)
	res := fx.refresh(t, rootcap.Config{})
	if len(res.OverCommitted) != 1 || res.OverCommitted[0].Key != key(usc, "vcpu") {
		t.Fatalf("over-committed %+v", res.OverCommitted)
	}
	if got := fx.granted(t, usc, "vcpu"); got != 50 {
		t.Fatalf("capacity %d", got)
	}
	// Nothing is reclaimed, and nothing more is granted.
	err := fx.db.Tx(ctx, func(tx store.Tx) error {
		return ledger.Grant(ctx, tx, child.ID, []ledger.Amount{{Key: key(usc, "vcpu"), Amount: ledger.Units(81)}})
	})
	if !errors.Is(err, ledger.ErrQuotaExceeded) {
		t.Fatalf("grant while over-committed: %v", err)
	}
	if n := len(fx.changes(t, "quota.over_committed")); n != 1 {
		t.Fatalf("%d quota.over_committed records", n)
	}
	recs := fx.changes(t, "quota.root_resync")
	if last := recs[len(recs)-1]; last["over_committed"] == nil {
		t.Fatalf("root_resync %v", last)
	}

	// Back under when the limit is raised.
	fx.fake.SetQuota(usc, gce.MetricCPUS, 120)
	if res := fx.refresh(t, rootcap.Config{}); len(res.OverCommitted) != 0 {
		t.Fatalf("still over-committed: %+v", res.OverCommitted)
	}
	if n := len(fx.changes(t, "quota.over_committed")); n != 2 {
		t.Fatalf("%d quota.over_committed records", n)
	}
}

// failing is a client whose projects.get fails.
type failing struct{ gce.Client }

func (failing) GetProject(context.Context, string) (*compute.Project, error) {
	return nil, gce.NewError(503, gce.ReasonBackendError, "unavailable")
}

func TestFailedReadChangesNothing(t *testing.T) {
	fx := newFixture(t)
	fx.fake.SetQuota(usc, gce.MetricCPUS, 100)
	fx.refresh(t, rootcap.Config{})
	fx.fake.SetQuota(usc, gce.MetricCPUS, 10)
	_, err := rootcap.New(fx.db, failing{fx.fake}, rootcap.Config{Project: proj}).Refresh(ctx)
	if gce.Code(err) != 503 {
		t.Fatalf("refresh: %v", err)
	}
	if got := fx.granted(t, usc, "vcpu"); got != 100 {
		t.Fatalf("capacity after a failed read: %d", got)
	}
}

// quotaInfos is a client whose Cloud Quotas read returns infos.
type quotaInfos struct {
	gce.Client
	infos []*gce.QuotaInfo
}

func (q quotaInfos) ListComputeQuotaInfos(context.Context, string) ([]*gce.QuotaInfo, error) {
	return q.infos, nil
}

// TestCloudQuotasShapes reads the shapes Cloud Quotas reports limits
// in: a default for every region with overrides, per-family GPU
// quotas, and limits split by a dimension silod doesn't hand down.
func TestCloudQuotasShapes(t *testing.T) {
	fx := newFixture(t)
	info := func(metric string, dis ...*gce.QuotaDimensionsInfo) *gce.QuotaInfo {
		return &gce.QuotaInfo{Metric: gce.CloudQuotasService + "/" + metric, DimensionsInfos: dis}
	}
	di := func(v int64, dims map[string]string, locs ...string) *gce.QuotaDimensionsInfo {
		return &gce.QuotaDimensionsInfo{Dimensions: dims, Details: gce.QuotaDetails{Value: v}, ApplicableLocations: locs}
	}
	c := quotaInfos{Client: fx.fake, infos: []*gce.QuotaInfo{
		// A default with an override for one region.
		info("hdt_total_gb", di(500, map[string]string{"region": usc}), di(100, nil, usc, euw, "us-east1")),
		// Per-family GPUs.
		info("gpus_per_gpu_family",
			di(8, map[string]string{"region": usc, "gpu_family": "NVIDIA_B200"}),
			di(2, map[string]string{"region": euw, "gpu_family": "NVIDIA_B200"}),
			di(0, nil, usc, euw)),
		info("preemptible_gpus_per_gpu_family", di(4, map[string]string{"region": usc, "gpu_family": "NVIDIA_B200"})),
		// Split by VM family: not a limit silod hands down.
		info("hdx_total_gb", di(70, map[string]string{"region": usc, "vm_family": "C3"})),
		// Project-wide.
		info("snapshots", di(25, nil, "global")),
	}}
	res := fx.refreshWith(t, c, rootcap.Config{})
	for _, x := range []struct {
		region, class string
		want          int64
	}{
		{usc, "hyperdisk_throughput_gb", 500},
		{euw, "hyperdisk_throughput_gb", 100},
		{usc, "nvidia_b200_gpus", 8},
		{euw, "nvidia_b200_gpus", 2},
		{usc, "spot_nvidia_b200_gpus", 4},
		{usc, "hyperdisk_extreme_gb", 0},
		{ledger.Global, "snapshots", 25},
	} {
		if got := fx.granted(t, x.region, x.class); got != x.want {
			t.Errorf("%s %s = %d, want %d", x.region, x.class, got, x.want)
		}
	}
	if len(res.AddedClasses) != 2 {
		t.Fatalf("added %v", res.AddedClasses)
	}
}

// TestThroughRealClient refreshes through the real client and the
// fake's HTTP server.
func TestThroughRealClient(t *testing.T) {
	fx := newFixture(t)
	fx.fake.SetQuota(usc, gce.MetricCPUS, 72)
	fx.fake.SetQuota(euw, "NVIDIA_H100_GPUS", 8)
	srv := fake.NewServer(fx.fake)
	defer srv.Close()
	c, err := gceclient.New(ctx, gceclient.Options{ComputeEndpoint: srv.ComputeEndpoint(), CloudQuotasEndpoint: srv.CloudQuotasEndpoint(),
		HTTPClient: srv.Client(), PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	res := fx.refreshWith(t, c, rootcap.Config{})
	if got := fx.granted(t, usc, "vcpu"); got != 72 {
		t.Fatalf("vcpu %d", got)
	}
	if got := fx.granted(t, euw, "nvidia_h100_gpus"); got != 8 || len(res.AddedClasses) != 2 {
		t.Fatalf("H100 %d, added %v", got, res.AddedClasses)
	}
}
