package fake

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

const (
	proj   = "host-project"
	zone   = "us-central1-a"
	region = "us-central1"
)

var ctx = context.Background()

// newFake is a fake whose operations finish only on RunOperations, with
// the network silo and the subnetwork sub (10.0.0.0/24) in us-central1.
func newFake(t *testing.T) *Fake {
	t.Helper()
	f := New(Options{Project: proj, ManualOperations: true})
	mustDo(t, f)(f.InsertNetwork(ctx, proj, &compute.Network{Name: "silo", NetworkFirewallPolicyEnforcementOrder: "BEFORE_CLASSIC_FIREWALL"}))
	mustDo(t, f)(f.InsertSubnetwork(ctx, proj, region, &compute.Subnetwork{Name: "sub", Network: "global/networks/silo", IpCidrRange: "10.0.0.0/24"}))
	return f
}

// do finishes the operation a call started and returns it, DONE.
func do(t *testing.T, f *Fake) func(*compute.Operation, error) *compute.Operation {
	t.Helper()
	return func(op *compute.Operation, err error) *compute.Operation {
		t.Helper()
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		if op.Status == "DONE" {
			t.Fatalf("operation %s was DONE before it was run", op.Name)
		}
		f.RunOperations()
		got, err := f.GetOperation(ctx, proj, gce.ScopeOf(op), op.Name)
		if err != nil {
			t.Fatalf("GetOperation: %v", err)
		}
		if got.Status != "DONE" {
			t.Fatalf("operation %s is %s after RunOperations", op.Name, got.Status)
		}
		return got
	}
}

// mustDo is do, failing the test if the operation failed.
func mustDo(t *testing.T, f *Fake) func(*compute.Operation, error) *compute.Operation {
	t.Helper()
	return func(op *compute.Operation, err error) *compute.Operation {
		t.Helper()
		got := do(t, f)(op, err)
		if got.Error != nil {
			t.Fatalf("operation %s failed: %s", got.Name, got.Error.Errors[0].Message)
		}
		return got
	}
}

// opCode is a failed operation's error code, or "".
func opCode(op *compute.Operation) string {
	if op.Error == nil {
		return ""
	}
	return op.Error.Errors[0].Code
}

// wantErr checks that err is GCE's error with code and reason.
func wantErr(t *testing.T, err error, code int, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want %d %s", code, reason)
	}
	if gce.Code(err) != code || (reason != "" && gce.Reason(err) != reason) {
		t.Fatalf("got %v (code %d, reason %q), want %d %s", err, gce.Code(err), gce.Reason(err), code, reason)
	}
}

func wantField(t *testing.T, err error, field string) {
	t.Helper()
	wantErr(t, err, http.StatusBadRequest, "")
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("error %v doesn't name %s", err, field)
	}
}

func vm(name, machineType string) *compute.Instance {
	return &compute.Instance{
		Name:        name,
		MachineType: "zones/" + zone + "/machineTypes/" + machineType,
		Disks: []*compute.AttachedDisk{{Boot: true, InitializeParams: &compute.AttachedDiskInitializeParams{
			SourceImage: "projects/debian-cloud/global/images/family/debian-12",
		}}},
		NetworkInterfaces: []*compute.NetworkInterface{{Network: "global/networks/silo", Subnetwork: "regions/us-central1/subnetworks/sub"}},
	}
}

func withBootType(inst *compute.Instance, typ string) *compute.Instance {
	inst.Disks[0].InitializeParams.DiskType = "zones/" + zone + "/diskTypes/" + typ
	return inst
}

func gpuVM(name, machineType, accel string, n int64) *compute.Instance {
	inst := vm(name, machineType)
	if accel != "" {
		inst.GuestAccelerators = []*compute.AcceleratorConfig{{AcceleratorType: "zones/" + zone + "/acceleratorTypes/" + accel, AcceleratorCount: n}}
	}
	inst.Scheduling = &compute.Scheduling{OnHostMaintenance: "TERMINATE"}
	return inst
}

func spot(inst *compute.Instance) *compute.Instance {
	if inst.Scheduling == nil {
		inst.Scheduling = &compute.Scheduling{}
	}
	inst.Scheduling.ProvisioningModel = "SPOT"
	return inst
}

func TestNames(t *testing.T) {
	f := newFake(t)
	long := strings.Repeat("a", 63)
	mustDo(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: long, SizeGb: 10}))
	_, err := f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: long + "a", SizeGb: 10})
	wantErr(t, err, http.StatusBadRequest, gce.ReasonInvalid)
	for _, bad := range []string{"Upper", "1abc", "ends-", "has_underscore", ""} {
		if _, err := f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: bad, SizeGb: 10}); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	_, err = f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: long, SizeGb: 10})
	wantErr(t, err, http.StatusConflict, gce.ReasonAlreadyExists)
	// Names are unique per scope: the same name in another zone is fine.
	mustDo(t, f)(f.InsertDisk(ctx, proj, "us-central1-b", &compute.Disk{Name: long, SizeGb: 10}))

	// A name is held from the call, before the operation finishes.
	if _, err := f.InsertInstance(ctx, proj, zone, vm("vm", "e2-standard-2"), ""); err != nil {
		t.Fatal(err)
	}
	_, err = f.InsertInstance(ctx, proj, zone, vm("vm", "e2-standard-2"), "")
	wantErr(t, err, http.StatusConflict, gce.ReasonAlreadyExists)

	// Unknown project: GCE's 403.
	_, err = f.GetDisk(ctx, "someone-else", zone, long)
	wantErr(t, err, http.StatusForbidden, gce.ReasonForbidden)
	_, err = f.GetDisk(ctx, proj, zone, "nope")
	wantErr(t, err, http.StatusNotFound, gce.ReasonNotFound)
}

func TestSubnetRanges(t *testing.T) {
	f := newFake(t)
	sub := func(name, region, cidr string) (*compute.Operation, error) {
		return f.InsertSubnetwork(ctx, proj, region, &compute.Subnetwork{Name: name, Network: "global/networks/silo", IpCidrRange: cidr})
	}
	_, err := sub("overlap", region, "10.0.0.128/25")
	wantField(t, err, "ipCidrRange")
	// The network is global: overlap is refused across regions too.
	_, err = sub("overlap", "us-east1", "10.0.0.0/16")
	wantField(t, err, "ipCidrRange")
	_, err = sub("tiny", region, "10.1.0.0/30")
	wantField(t, err, "ipCidrRange")
	_, err = sub("hostbits", region, "10.1.0.1/29")
	wantField(t, err, "ipCidrRange")
	op := mustDo(t, f)(sub("smallest", region, "10.1.0.0/29"))
	s, err := f.GetSubnetwork(ctx, proj, region, "smallest")
	if err != nil || s.GatewayAddress != "10.1.0.1" {
		t.Fatalf("subnet %+v, %v (op %s)", s, err, op.Name)
	}
	// A pending subnet holds its range.
	if _, err := sub("pending", region, "10.2.0.0/24"); err != nil {
		t.Fatal(err)
	}
	_, err = sub("clash", "us-east1", "10.2.0.0/25")
	wantField(t, err, "ipCidrRange")
	f.RunOperations()

	// Another network may reuse the range.
	mustDo(t, f)(f.InsertNetwork(ctx, proj, &compute.Network{Name: "other"}))
	mustDo(t, f)(f.InsertSubnetwork(ctx, proj, region, &compute.Subnetwork{Name: "o", Network: "global/networks/other", IpCidrRange: "10.0.0.0/24"}))

	// Secondary ranges count too.
	_, err = f.InsertSubnetwork(ctx, proj, region, &compute.Subnetwork{Name: "sec", Network: "global/networks/silo", IpCidrRange: "10.3.0.0/24",
		SecondaryIpRanges: []*compute.SubnetworkSecondaryRange{{RangeName: "pods", IpCidrRange: "10.0.0.64/26"}}})
	wantField(t, err, "secondaryIpRanges[0]")

	// expandIpCidrRange: must contain the old range, and overlap nothing.
	_, err = f.ExpandIpCidrRange(ctx, proj, region, "smallest", "10.1.0.0/30")
	wantField(t, err, "ipCidrRange")
	_, err = f.ExpandIpCidrRange(ctx, proj, region, "smallest", "10.0.0.0/8")
	wantField(t, err, "ipCidrRange")
	mustDo(t, f)(f.ExpandIpCidrRange(ctx, proj, region, "smallest", "10.1.0.0/28"))

	// patch needs the fingerprint.
	s, _ = f.GetSubnetwork(ctx, proj, region, "sub")
	_, err = f.PatchSubnetwork(ctx, proj, region, "sub", &compute.Subnetwork{Fingerprint: "stale", Description: "x"})
	wantErr(t, err, http.StatusPreconditionFailed, gce.ReasonConditionNotMet)
	mustDo(t, f)(f.PatchSubnetwork(ctx, proj, region, "sub", &compute.Subnetwork{Fingerprint: s.Fingerprint,
		SecondaryIpRanges: []*compute.SubnetworkSecondaryRange{{RangeName: "pods", IpCidrRange: "10.9.0.0/24"}}}))
	s2, _ := f.GetSubnetwork(ctx, proj, region, "sub")
	if s2.Fingerprint == s.Fingerprint || len(s2.SecondaryIpRanges) != 1 {
		t.Fatalf("patch didn't apply: %+v", s2)
	}
}

func TestOperationsAreAsynchronous(t *testing.T) {
	f := newFake(t)
	op, err := f.InsertInstance(ctx, proj, zone, vm("vm", "e2-standard-2"), "")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != "RUNNING" || op.OperationType != "insert" || !strings.HasSuffix(op.TargetLink, "/zones/"+zone+"/instances/vm") {
		t.Fatalf("op %+v", op)
	}
	inst, err := f.GetInstance(ctx, proj, zone, "vm")
	if err != nil || inst.Status != "PROVISIONING" {
		t.Fatalf("before the op finishes: %+v, %v", inst, err)
	}
	if _, err := f.StopInstance(ctx, proj, zone, "vm", false); gce.Reason(err) != gce.ReasonNotReady {
		t.Fatalf("stop of a provisioning instance: %v", err)
	}
	ops, _ := f.ListOperations(ctx, proj, gce.Zonal(zone))
	if len(ops) != 1 {
		t.Fatalf("zone operations: %d", len(ops))
	}
	if n := f.RunOperations(); n != 1 {
		t.Fatalf("RunOperations finished %d", n)
	}
	got, _ := f.GetOperation(ctx, proj, gce.Zonal(zone), op.Name)
	if got.Status != "DONE" || got.Error != nil || got.Progress != 100 {
		t.Fatalf("op after run %+v", got)
	}
	inst, _ = f.GetInstance(ctx, proj, zone, "vm")
	if inst.Status != "RUNNING" || inst.NetworkInterfaces[0].NetworkIP != "10.0.0.2" {
		t.Fatalf("instance after run: %s %s", inst.Status, inst.NetworkInterfaces[0].NetworkIP)
	}
	if _, err := f.GetDisk(ctx, proj, zone, "vm"); err != nil {
		t.Fatalf("boot disk: %v", err)
	}

	// Operations on a fake that runs them itself finish on their own;
	// WaitOperation waits for them.
	g := New(Options{Project: proj, OperationLatency: 10 * time.Millisecond})
	op, err = g.InsertNetwork(ctx, proj, &compute.Network{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = g.WaitOperation(ctx, proj, gce.Global, op.Name)
	if err != nil || got.Status != "DONE" {
		t.Fatalf("wait: %+v, %v", got, err)
	}
	// WaitOperation returns at its cap with the operation still running.
	h := New(Options{Project: proj, ManualOperations: true, WaitTimeout: 10 * time.Millisecond})
	op, _ = h.InsertNetwork(ctx, proj, &compute.Network{Name: "n"})
	if got, err := h.WaitOperation(ctx, proj, gce.Global, op.Name); err != nil || got.Status != "RUNNING" {
		t.Fatalf("capped wait: %+v, %v", got, err)
	}
}

func TestFailOperation(t *testing.T) {
	for _, c := range []struct {
		kind FailureKind
		code string
	}{
		{FailQuota, gce.CodeQuotaExceeded},
		{FailStockout, gce.CodeStockout},
		{FailInternal, gce.CodeInternal},
	} {
		t.Run(c.code, func(t *testing.T) {
			f := newFake(t)
			f.FailOperation(Failure{Method: "instances.insert", Name: "doomed", Kind: c.kind})
			mustDo(t, f)(f.InsertInstance(ctx, proj, zone, vm("other", "e2-standard-2"), ""))
			op := do(t, f)(f.InsertInstance(ctx, proj, zone, vm("doomed", "e2-standard-2"), ""))
			if opCode(op) != c.code || op.HttpErrorStatusCode == 0 {
				t.Fatalf("op error %+v", op.Error)
			}
			// A failed insert leaves nothing: not the instance, not its disk.
			if _, err := f.GetInstance(ctx, proj, zone, "doomed"); !gce.IsNotFound(err) {
				t.Fatalf("instance after failed insert: %v", err)
			}
			if _, err := f.GetDisk(ctx, proj, zone, "doomed"); !gce.IsNotFound(err) {
				t.Fatalf("disk after failed insert: %v", err)
			}
			// The failure was used up.
			mustDo(t, f)(f.InsertInstance(ctx, proj, zone, vm("doomed", "e2-standard-2"), ""))
		})
	}
	f := newFake(t)
	mustDo(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "d", SizeGb: 10}))
	f.FailOperation(Failure{Method: "disks.resize", Kind: FailInternal})
	op := do(t, f)(f.ResizeDisk(ctx, proj, zone, "d", 20))
	if opCode(op) != gce.CodeInternal {
		t.Fatalf("resize op %+v", op.Error)
	}
	if d, _ := f.GetDisk(ctx, proj, zone, "d"); d.SizeGb != 10 {
		t.Fatalf("failed resize changed the size to %d", d.SizeGb)
	}
}

func TestRegionalQuota(t *testing.T) {
	f := newFake(t)
	f.SetQuota(region, gce.MetricCPUS, 4)
	f.SetQuota(region, gce.MetricInstances, 10)
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, vm("a", "e2-standard-2"), ""))
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, vm("b", "e2-standard-2"), ""))
	op := do(t, f)(f.InsertInstance(ctx, proj, zone, vm("c", "e2-standard-2"), ""))
	if opCode(op) != gce.CodeQuotaExceeded || !strings.Contains(op.Error.Errors[0].Message, "'CPUS'") {
		t.Fatalf("third instance: %+v", op.Error)
	}
	r, _ := f.GetRegion(ctx, proj, region)
	if q := quota(r.Quotas, gce.MetricCPUS); q == nil || q.Limit != 4 || q.Usage != 4 {
		t.Fatalf("CPUS quota %+v", q)
	}
	// Another region's quota is its own.
	if r, _ := f.GetRegion(ctx, proj, "us-east1"); quota(r.Quotas, gce.MetricCPUS) != nil {
		t.Fatal("us-east1 reports CPUS with no limit set")
	}

	// A stopped instance holds no vCPUs; starting it is checked again.
	mustDo(t, f)(f.StopInstance(ctx, proj, zone, "a", false))
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, vm("c", "e2-standard-2"), ""))
	op = do(t, f)(f.StartInstance(ctx, proj, zone, "a"))
	if opCode(op) != gce.CodeQuotaExceeded {
		t.Fatalf("start over quota: %+v", op.Error)
	}
	if inst, _ := f.GetInstance(ctx, proj, zone, "a"); inst.Status != "TERMINATED" {
		t.Fatalf("refused start left status %s", inst.Status)
	}
	// INSTANCES counts stopped ones too.
	r, _ = f.GetRegion(ctx, proj, region)
	if q := quota(r.Quotas, gce.MetricInstances); q.Usage != 3 {
		t.Fatalf("INSTANCES usage %v", q.Usage)
	}
}

func quota(qs []*compute.Quota, metric string) *compute.Quota {
	for _, q := range qs {
		if q.Metric == metric {
			return q
		}
	}
	return nil
}

func TestSpotQuota(t *testing.T) {
	f := newFake(t)
	f.SetQuota(region, gce.MetricCPUS, 2)
	// With no PREEMPTIBLE_CPUS limit, Spot counts against CPUS.
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, spot(vm("s1", "e2-standard-2")), ""))
	if op := do(t, f)(f.InsertInstance(ctx, proj, zone, vm("std", "e2-standard-2"), "")); opCode(op) != gce.CodeQuotaExceeded {
		t.Fatalf("standard after spot used CPUS: %+v", op.Error)
	}
	// With one, Spot counts against it alone.
	f.SetQuota(region, gce.MetricPreemptibleCPUS, 4)
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, vm("std", "e2-standard-2"), ""))
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, spot(vm("s2", "e2-standard-2")), ""))
	if op := do(t, f)(f.InsertInstance(ctx, proj, zone, spot(vm("s3", "e2-standard-2")), "")); !strings.Contains(op.Error.Errors[0].Message, gce.MetricPreemptibleCPUS) {
		t.Fatalf("third spot: %+v", op.Error)
	}
}

func TestGPUQuota(t *testing.T) {
	f := newFake(t)
	f.SetQuota(region, "NVIDIA_T4_GPUS", 2)
	f.SetQuota(region, "PREEMPTIBLE_NVIDIA_T4_GPUS", 1)
	f.SetQuota(region, "NVIDIA_L4_GPUS", 1)
	f.SetQuota(region, "NVIDIA_H100_GPUS", 8)
	f.SetQuota("", gce.MetricGPUsAllRegions, 4)

	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, gpuVM("t4", "n1-standard-4", "nvidia-tesla-t4", 2), ""))
	if op := do(t, f)(f.InsertInstance(ctx, proj, zone, gpuVM("t4b", "n1-standard-4", "nvidia-tesla-t4", 1), "")); opCode(op) != gce.CodeQuotaExceeded {
		t.Fatalf("T4 over quota: %+v", op.Error)
	}
	// Spot GPUs have their own metric.
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, spot(gpuVM("t4s", "n1-standard-4", "nvidia-tesla-t4", 1)), ""))
	// Bundled GPUs count: g2-standard-4 brings an L4.
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, gpuVM("g2", "g2-standard-4", "", 0), ""))
	if op := do(t, f)(f.InsertInstance(ctx, proj, zone, gpuVM("g2b", "g2-standard-4", "", 0), "")); opCode(op) != gce.CodeQuotaExceeded {
		t.Fatalf("second L4: %+v", op.Error)
	}
	// GPUS_ALL_REGIONS: 2 + 1 + 1 used of 4; an A3 (8) doesn't fit.
	a3 := withBootType(gpuVM("a3", "a3-highgpu-8g", "", 0), "hyperdisk-balanced")
	op := do(t, f)(f.InsertInstance(ctx, proj, zone, a3, ""))
	if opCode(op) != gce.CodeQuotaExceeded || !strings.Contains(op.Error.Errors[0].Message, gce.MetricGPUsAllRegions) {
		t.Fatalf("A3 over GPUS_ALL_REGIONS: %+v", op.Error)
	}
	pr, _ := f.GetProject(ctx, proj)
	if q := quota(pr.Quotas, gce.MetricGPUsAllRegions); q == nil || q.Usage != 4 {
		t.Fatalf("GPUS_ALL_REGIONS %+v", q)
	}
	// A stopped GPU instance holds no GPUs.
	mustDo(t, f)(f.StopInstance(ctx, proj, zone, "t4", false))
	r, _ := f.GetRegion(ctx, proj, region)
	if q := quota(r.Quotas, "NVIDIA_T4_GPUS"); q.Usage != 0 {
		t.Fatalf("T4 usage after stop %v", q.Usage)
	}
}

func TestQuotaReporting(t *testing.T) {
	f := newFake(t)
	regional := []string{
		gce.MetricCPUS, gce.MetricPreemptibleCPUS, gce.MetricInstances, gce.MetricSSDTotalGB, gce.MetricDisksTotalGB,
		gce.MetricLocalSSDTotalGB, gce.MetricPreemptibleLocalSSDGB, gce.MetricPDExtremeIOPS,
		gce.MetricHDBTotalGB, gce.MetricHDBTotalIOPS, gce.MetricHDBTotalThroughput,
		"NVIDIA_L4_GPUS", "PREEMPTIBLE_NVIDIA_L4_GPUS",
	}
	cloudOnly := []string{
		"NVIDIA_H100_GPUS", "PREEMPTIBLE_NVIDIA_H100_GPUS",
		gce.MetricHDXTotalGB, gce.MetricHDXTotalIOPS, gce.MetricHDTTotalGB, gce.MetricHDTTotalThroughput,
		gce.MetricHDMLTotalGB, gce.MetricHDMLTotalThroughput,
	}
	project := []string{gce.MetricGPUsAllRegions, gce.MetricInstanceTemplates, gce.MetricSubnetworks, gce.MetricFirewalls}
	for _, m := range append(append([]string{}, regional...), cloudOnly...) {
		f.SetQuota(region, m, 100)
	}
	for _, m := range project {
		f.SetQuota("", m, 50)
	}
	r, _ := f.GetRegion(ctx, proj, region)
	for _, m := range regional {
		if quota(r.Quotas, m) == nil {
			t.Errorf("regions.get doesn't report %s", m)
		}
	}
	for _, m := range cloudOnly {
		if quota(r.Quotas, m) != nil {
			t.Errorf("regions.get reports %s, which GCE reports only through Cloud Quotas", m)
		}
	}
	pr, _ := f.GetProject(ctx, proj)
	for _, m := range project {
		if quota(pr.Quotas, m) == nil {
			t.Errorf("projects.get doesn't report %s", m)
		}
	}
	if q := quota(pr.Quotas, gce.MetricSubnetworks); q.Usage != 1 {
		t.Errorf("SUBNETWORKS usage %v", q.Usage)
	}
	infos, err := f.ListComputeQuotaInfos(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	byMetric := map[string]*gce.QuotaInfo{}
	for _, qi := range infos {
		byMetric[qi.Metric] = qi
	}
	for _, m := range append(append(append([]string{}, regional...), cloudOnly...), project...) {
		qi := byMetric["compute.googleapis.com/"+strings.ToLower(m)]
		if qi == nil {
			t.Errorf("Cloud Quotas doesn't report %s", m)
			continue
		}
		if qi.DimensionsInfos[0].Details.Value == 0 {
			t.Errorf("Cloud Quotas %s has no limit", m)
		}
	}
	if di := byMetric["compute.googleapis.com/nvidia_h100_gpus"].DimensionsInfos[0]; di.Dimensions["region"] != region || di.Details.Value != 100 {
		t.Errorf("H100 dimensions %+v", di)
	}
}

func TestLocalSSDQuota(t *testing.T) {
	f := newFake(t)
	f.SetQuota(region, gce.MetricLocalSSDTotalGB, 750)
	f.SetQuota(region, gce.MetricPreemptibleLocalSSDGB, 375)
	// c3-standard-8-lssd bundles two partitions.
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, withBootType(vm("c3", "c3-standard-8-lssd"), "pd-balanced"), ""))
	r, _ := f.GetRegion(ctx, proj, region)
	if q := quota(r.Quotas, gce.MetricLocalSSDTotalGB); q.Usage != 750 {
		t.Fatalf("LOCAL_SSD_TOTAL_GB usage %v", q.Usage)
	}
	// Attached partitions on N1, Spot: PREEMPTIBLE_LOCAL_SSD_GB.
	n1 := spot(vm("n1", "n1-standard-4"))
	scratch := &compute.AttachedDisk{Type: "SCRATCH", Interface: "NVME", InitializeParams: &compute.AttachedDiskInitializeParams{DiskType: "local-ssd"}}
	n1.Disks = append(n1.Disks, scratch)
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, n1, ""))
	n1b := spot(vm("n1b", "n1-standard-4"))
	n1b.Disks = append(n1b.Disks, scratch)
	if op := do(t, f)(f.InsertInstance(ctx, proj, zone, n1b, "")); !strings.Contains(op.Error.Errors[0].Message, gce.MetricPreemptibleLocalSSDGB) {
		t.Fatalf("second spot local SSD: %+v", op.Error)
	}
	// A machine type that bundles local SSD takes no more, nor does E2.
	for _, mt := range []string{"c3-standard-4-lssd", "e2-standard-2"} {
		inst := withBootType(vm("x", mt), "pd-balanced")
		inst.Disks = append(inst.Disks, scratch)
		_, err := f.InsertInstance(ctx, proj, zone, inst, "")
		wantField(t, err, "resource.disks[1]")
	}
}

func TestDiskQuotaAndPerformance(t *testing.T) {
	f := newFake(t)
	f.SetQuota(region, gce.MetricSSDTotalGB, 1000)
	f.SetQuota(region, gce.MetricPDExtremeIOPS, 10000)
	f.SetQuota(region, gce.MetricHDBTotalIOPS, 10000)
	f.SetQuota(region, gce.MetricHDTTotalThroughput, 100)

	// pd-extreme counts in SSD_TOTAL_GB and its IOPS metric; GCE's
	// default IOPS when none is given.
	mustDo(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "x", Type: "pd-extreme", SizeGb: 500}))
	d, _ := f.GetDisk(ctx, proj, zone, "x")
	if d.ProvisionedIops != 2500 || d.Status != "READY" {
		t.Fatalf("pd-extreme default %+v", d)
	}
	_, err := f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "y", Type: "pd-extreme", SizeGb: 500, ProvisionedIops: 999999})
	wantField(t, err, "provisionedIops")
	_, err = f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "y", Type: "pd-ssd", SizeGb: 100, ProvisionedIops: 5000})
	wantField(t, err, "provisionedIops")
	if op := do(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "y", Type: "pd-extreme", SizeGb: 500, ProvisionedIops: 8000})); !strings.Contains(op.Error.Errors[0].Message, gce.MetricPDExtremeIOPS) {
		t.Fatalf("pd-extreme IOPS over quota: %+v", op.Error)
	}
	if op := do(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "y", Type: "pd-ssd", SizeGb: 600})); !strings.Contains(op.Error.Errors[0].Message, gce.MetricSSDTotalGB) {
		t.Fatalf("pd-ssd over SSD_TOTAL_GB: %+v", op.Error)
	}

	// Hyperdisk Balanced: defaults for both IOPS and throughput.
	mustDo(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "hdb", Type: "hyperdisk-balanced", SizeGb: 100}))
	d, _ = f.GetDisk(ctx, proj, zone, "hdb")
	if d.ProvisionedIops != 3600 || d.ProvisionedThroughput != 200 || d.AccessMode != "READ_WRITE_SINGLE" {
		t.Fatalf("hyperdisk-balanced defaults %d %d %s", d.ProvisionedIops, d.ProvisionedThroughput, d.AccessMode)
	}
	// disks.update: only the performance fields and accessMode, charged
	// against quota.
	mustDo(t, f)(f.UpdateDisk(ctx, proj, zone, "hdb", &compute.Disk{ProvisionedIops: 5000}, []string{"provisionedIops"}))
	if d, _ = f.GetDisk(ctx, proj, zone, "hdb"); d.ProvisionedIops != 5000 {
		t.Fatalf("after update %d", d.ProvisionedIops)
	}
	if op := do(t, f)(f.UpdateDisk(ctx, proj, zone, "hdb", &compute.Disk{ProvisionedIops: 12000}, []string{"provisionedIops"})); opCode(op) != gce.CodeQuotaExceeded {
		t.Fatalf("update over HDB_TOTAL_IOPS: %+v", op.Error)
	}
	_, err = f.UpdateDisk(ctx, proj, zone, "hdb", &compute.Disk{SizeGb: 500}, []string{"sizeGb"})
	wantField(t, err, "paths")
	_, err = f.UpdateDisk(ctx, proj, zone, "x", &compute.Disk{ProvisionedThroughput: 500}, []string{"provisionedThroughput"})
	wantField(t, err, "provisionedThroughput")
	_, err = f.UpdateDisk(ctx, proj, zone, "hdb", &compute.Disk{ProvisionedIops: 1}, []string{"provisionedIops"})
	wantField(t, err, "provisionedIops")

	// Resize: only up; Hyperdisk Throughput's throughput rises to the new
	// size's minimum, and that is charged.
	mustDo(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "hdt", Type: "hyperdisk-throughput", SizeGb: 2048}))
	d, _ = f.GetDisk(ctx, proj, zone, "hdt")
	if d.ProvisionedThroughput != 20 {
		t.Fatalf("hyperdisk-throughput default %d", d.ProvisionedThroughput)
	}
	_, err = f.ResizeDisk(ctx, proj, zone, "hdt", 1024)
	wantField(t, err, "sizeGb")
	mustDo(t, f)(f.ResizeDisk(ctx, proj, zone, "hdt", 4096))
	if d, _ = f.GetDisk(ctx, proj, zone, "hdt"); d.SizeGb != 4096 || d.ProvisionedThroughput != 40 {
		t.Fatalf("after resize %d GB %d MiB/s", d.SizeGb, d.ProvisionedThroughput)
	}
	if op := do(t, f)(f.ResizeDisk(ctx, proj, zone, "hdt", 32768)); !strings.Contains(op.Error.Errors[0].Message, gce.MetricHDTTotalThroughput) {
		t.Fatalf("resize over throughput quota: %+v", op.Error)
	}
	// Hyperdisk ML takes no IOPS; Hyperdisk Extreme no throughput.
	_, err = f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "ml", Type: "hyperdisk-ml", SizeGb: 100, ProvisionedIops: 3000})
	wantField(t, err, "provisionedIops")
	_, err = f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "hdx", Type: "hyperdisk-extreme", SizeGb: 100, ProvisionedThroughput: 100})
	wantField(t, err, "provisionedThroughput")
	_, err = f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "ssd", Type: "pd-ssd", SizeGb: 100, AccessMode: "READ_WRITE_MANY"})
	wantField(t, err, "accessMode")
}

func TestGPUCompatibility(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		name  string
		inst  *compute.Instance
		zone  string
		field string
	}{
		{"T4 on E2", gpuVM("x", "e2-standard-4", "nvidia-tesla-t4", 1), zone, "acceleratorType"},
		{"T4 count 3", gpuVM("x", "n1-standard-4", "nvidia-tesla-t4", 3), zone, "acceleratorCount"},
		{"L4 attached", gpuVM("x", "n1-standard-4", "nvidia-l4", 1), zone, "acceleratorType"},
		{"G2 with guestAccelerators", gpuVM("x", "g2-standard-4", "nvidia-l4", 1), zone, "guestAccelerators"},
		{"MIGRATE with GPUs", func() *compute.Instance {
			i := gpuVM("x", "n1-standard-4", "nvidia-tesla-t4", 1)
			i.Scheduling.OnHostMaintenance = "MIGRATE"
			return i
		}(), zone, "onHostMaintenance"},
		{"default MIGRATE with bundled GPUs", func() *compute.Instance {
			i := gpuVM("x", "g2-standard-4", "", 0)
			i.Scheduling = nil
			return i
		}(), zone, "onHostMaintenance"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.InsertInstance(ctx, proj, c.zone, c.inst, "")
			wantField(t, err, c.field)
		})
	}
	// A zone without the accelerator: us-east1-b offers no T4.
	mustDo(t, f)(f.InsertSubnetwork(ctx, proj, "us-east1", &compute.Subnetwork{Name: "sub", Network: "global/networks/silo", IpCidrRange: "10.5.0.0/24"}))
	east := gpuVM("x", "n1-standard-4", "", 0)
	east.MachineType = "n1-standard-4"
	east.GuestAccelerators = []*compute.AcceleratorConfig{{AcceleratorType: "nvidia-tesla-t4", AcceleratorCount: 1}}
	east.NetworkInterfaces[0].Subnetwork = "regions/us-east1/subnetworks/sub"
	_, err := f.InsertInstance(ctx, proj, "us-east1-b", east, "")
	wantField(t, err, "acceleratorType")
	// Spot defaults onHostMaintenance to TERMINATE, so GPUs are fine.
	sp := spot(gpuVM("sp", "n1-standard-4", "nvidia-tesla-t4", 1))
	sp.Scheduling.OnHostMaintenance = ""
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, sp, ""))
	if inst, _ := f.GetInstance(ctx, proj, zone, "sp"); inst.Scheduling.OnHostMaintenance != "TERMINATE" {
		t.Fatalf("spot onHostMaintenance %s", inst.Scheduling.OnHostMaintenance)
	}
	// setMachineType to a type that can't take the T4.
	mustDo(t, f)(f.StopInstance(ctx, proj, zone, "sp", false))
	_, err = f.SetMachineType(ctx, proj, zone, "sp", "zones/"+zone+"/machineTypes/e2-standard-4")
	wantField(t, err, "acceleratorType")
	mustDo(t, f)(f.SetMachineType(ctx, proj, zone, "sp", "zones/"+zone+"/machineTypes/n1-standard-8"))
}

func TestDiskCompatibility(t *testing.T) {
	f := newFake(t)
	for _, c := range []struct{ mt, typ string }{
		{"n1-standard-4", "hyperdisk-balanced"},
		{"c4-standard-4", "pd-balanced"},
		{"n4-standard-4", "pd-standard"},
		{"n2-standard-8", "pd-extreme"},
		{"e2-standard-4", "hyperdisk-ml"},
	} {
		inst := withBootType(vm("x", c.mt), c.typ)
		if c.typ == "pd-extreme" {
			inst.Disks[0].InitializeParams.DiskSizeGb = 500
		}
		_, err := f.InsertInstance(ctx, proj, zone, inst, "")
		if err == nil || !strings.Contains(err.Error(), "not supported with machine type") {
			t.Errorf("%s on %s: %v", c.typ, c.mt, err)
		}
	}
	ok := withBootType(vm("big", "n2-standard-64"), "pd-extreme")
	ok.Disks[0].InitializeParams.DiskSizeGb = 500
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, ok, ""))
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, withBootType(vm("c4", "c4-standard-4"), "hyperdisk-balanced"), ""))

	// attachDisk is checked too, and setMachineType against attached disks.
	mustDo(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "pd", Type: "pd-balanced", SizeGb: 10}))
	_, err := f.AttachDisk(ctx, proj, zone, "c4", &compute.AttachedDisk{Source: "zones/" + zone + "/disks/pd"})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("pd-balanced onto c4: %v", err)
	}
	mustDo(t, f)(f.AttachDisk(ctx, proj, zone, "big", &compute.AttachedDisk{Source: "zones/" + zone + "/disks/pd"}))
	if d, _ := f.GetDisk(ctx, proj, zone, "pd"); len(d.Users) != 1 {
		t.Fatalf("users %v", d.Users)
	}
	_, err = f.DeleteDisk(ctx, proj, zone, "pd")
	wantErr(t, err, http.StatusBadRequest, gce.ReasonResourceInUse)
	mustDo(t, f)(f.StopInstance(ctx, proj, zone, "big", false))
	_, err = f.SetMachineType(ctx, proj, zone, "big", "n2-standard-8")
	if err == nil || !strings.Contains(err.Error(), "pd-extreme") {
		t.Fatalf("setMachineType away from pd-extreme: %v", err)
	}
	mustDo(t, f)(f.DetachDisk(ctx, proj, zone, "big", "pd"))
	mustDo(t, f)(f.DeleteDisk(ctx, proj, zone, "pd"))
}

func TestCatalog(t *testing.T) {
	f := newFake(t)
	accels, _ := f.ListAcceleratorTypes(ctx, proj, "us-east1-b")
	if len(accels) != 1 || accels[0].Name != "nvidia-l4" {
		t.Fatalf("us-east1-b accelerators %v", accels)
	}
	if _, err := f.GetAcceleratorType(ctx, proj, "us-east1-b", "nvidia-tesla-t4"); !gce.IsNotFound(err) {
		t.Fatalf("T4 in us-east1-b: %v", err)
	}
	g2, err := f.GetMachineType(ctx, proj, zone, "g2-standard-24")
	if err != nil || len(g2.Accelerators) != 1 || g2.Accelerators[0].GuestAcceleratorType != "nvidia-l4" || g2.Accelerators[0].GuestAcceleratorCount != 2 {
		t.Fatalf("g2 %+v %v", g2, err)
	}
	a2, _ := f.GetMachineType(ctx, proj, zone, "a2-ultragpu-1g")
	if a2.Accelerators[0].GuestAcceleratorType != "nvidia-a100-80gb" || a2.BundledLocalSsds.PartitionCount != 1 {
		t.Fatalf("a2-ultragpu %+v", a2)
	}
	lssd, _ := f.GetMachineType(ctx, proj, zone, "c3-standard-8-lssd")
	if lssd.BundledLocalSsds == nil || lssd.BundledLocalSsds.PartitionCount != 2 {
		t.Fatalf("c3 lssd %+v", lssd)
	}
	// A3 is only where H100s are.
	if _, err := f.GetMachineType(ctx, proj, "us-central1-b", "a3-highgpu-8g"); !gce.IsNotFound(err) {
		t.Fatalf("a3 in us-central1-b: %v", err)
	}
	dts, _ := f.ListDiskTypes(ctx, proj, zone)
	if len(dts) != len(diskKinds) {
		t.Fatalf("disk types %d", len(dts))
	}
	zones, _ := f.ListZones(ctx, proj)
	regions, _ := f.ListRegions(ctx, proj)
	if len(zones) != 10 || len(regions) != 3 {
		t.Fatalf("%d zones, %d regions", len(zones), len(regions))
	}
	img, err := f.GetImageFromFamily(ctx, "debian-cloud", "debian-12")
	if err != nil || img.Name != "debian-12-bookworm-v20260901" {
		t.Fatalf("debian-12 family: %v %v", img, err)
	}
}

func TestInstanceTemplates(t *testing.T) {
	f := newFake(t)
	f.SetQuota("", gce.MetricInstanceTemplates, 2)
	props := &compute.InstanceProperties{
		MachineType: "n1-standard-4",
		Labels:      map[string]string{"team": "a", "tier": "web"},
		Metadata:    &compute.Metadata{Items: []*compute.MetadataItems{{Key: "a", Value: ptr("1")}, {Key: "b", Value: ptr("2")}}},
		Disks: []*compute.AttachedDisk{{Boot: true, InitializeParams: &compute.AttachedDiskInitializeParams{
			SourceImage: "projects/debian-cloud/global/images/family/debian-12", DiskType: "pd-balanced"}}},
		NetworkInterfaces: []*compute.NetworkInterface{{Network: "global/networks/silo", Subnetwork: "regions/us-central1/subnetworks/sub"}},
		GuestAccelerators: []*compute.AcceleratorConfig{{AcceleratorType: "nvidia-tesla-t4", AcceleratorCount: 1}},
		Scheduling:        &compute.Scheduling{OnHostMaintenance: "TERMINATE"},
	}
	_, err := f.InsertInstanceTemplate(ctx, proj, "", &compute.InstanceTemplate{Name: "url", Properties: &compute.InstanceProperties{MachineType: "zones/us-central1-a/machineTypes/n1-standard-4"}})
	wantField(t, err, "properties.machineType")
	mustDo(t, f)(f.InsertInstanceTemplate(ctx, proj, "", &compute.InstanceTemplate{Name: "tpl", Properties: props}))
	mustDo(t, f)(f.InsertInstanceTemplate(ctx, proj, region, &compute.InstanceTemplate{Name: "tpl", Properties: props}))
	// Templates count against INSTANCE_TEMPLATES, global and regional.
	if op := do(t, f)(f.InsertInstanceTemplate(ctx, proj, "us-east1", &compute.InstanceTemplate{Name: "tpl3", Properties: props})); opCode(op) != gce.CodeQuotaExceeded {
		t.Fatalf("third template: %+v", op.Error)
	}

	// The body is merged over the template's properties.
	body := &compute.Instance{
		Name:     "from-tpl",
		Labels:   map[string]string{"tier": "db"},
		Metadata: &compute.Metadata{Items: []*compute.MetadataItems{{Key: "b", Value: ptr("override")}, {Key: "c", Value: ptr("3")}}},
	}
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, body, "global/instanceTemplates/tpl"))
	inst, _ := f.GetInstance(ctx, proj, zone, "from-tpl")
	if gce.LastSegment(inst.MachineType) != "n1-standard-4" || len(inst.GuestAccelerators) != 1 {
		t.Fatalf("not from template: %s %v", inst.MachineType, inst.GuestAccelerators)
	}
	if inst.Labels["team"] != "a" || inst.Labels["tier"] != "db" {
		t.Fatalf("labels %v", inst.Labels)
	}
	md := map[string]string{}
	for _, it := range inst.Metadata.Items {
		md[it.Key] = *it.Value
	}
	if md["a"] != "1" || md["b"] != "override" || md["c"] != "3" {
		t.Fatalf("metadata %v", md)
	}
	// The effective instance is checked as any other: a body that makes
	// it invalid is refused.
	_, err = f.InsertInstance(ctx, proj, zone, &compute.Instance{Name: "bad", MachineType: "e2-standard-4"}, "global/instanceTemplates/tpl")
	wantField(t, err, "acceleratorType")
	// A regional template is used only in its region.
	_, err = f.InsertInstance(ctx, proj, "us-east1-c", &compute.Instance{Name: "x"}, "projects/"+proj+"/regions/us-central1/instanceTemplates/tpl")
	wantField(t, err, "sourceInstanceTemplate")
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, &compute.Instance{Name: "from-rtpl"}, "projects/"+proj+"/regions/us-central1/instanceTemplates/tpl"))
	_, err = f.InsertInstance(ctx, proj, zone, &compute.Instance{Name: "y"}, "global/instanceTemplates/nope")
	wantErr(t, err, http.StatusNotFound, "")
}

func ptr(s string) *string { return &s }

func TestListFilters(t *testing.T) {
	f := newFake(t)
	for _, c := range []struct {
		name   string
		labels map[string]string
	}{
		{"k7-a", map[string]string{"silo": "k7"}},
		{"k7-b", map[string]string{"silo": "k7", "env": "prod"}},
		{"q2-a", map[string]string{"silo": "q2"}},
	} {
		mustDo(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: c.name, SizeGb: 10, Labels: c.labels}))
		mustDo(t, f)(f.InsertFirewall(ctx, proj, &compute.Firewall{Name: c.name, Network: "global/networks/silo",
			Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}}))
	}
	names := func(ds []*compute.Disk) string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Name)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		lf   gce.ListFilter
		want string
	}{
		{gce.ListFilter{}, "k7-a,k7-b,q2-a"},
		{gce.ListFilter{Labels: map[string]string{"silo": "k7"}}, "k7-a,k7-b"},
		{gce.ListFilter{Labels: map[string]string{"silo": "k7", "env": "prod"}}, "k7-b"},
		{gce.ListFilter{NamePrefix: "q2-"}, "q2-a"},
		{gce.ListFilter{NamePrefix: "k7-", Labels: map[string]string{"silo": "q2"}}, ""},
	} {
		got, err := f.ListDisks(ctx, proj, zone, c.lf)
		if err != nil || names(got) != c.want {
			t.Errorf("filter %q: %s, %v; want %s", c.lf.String(), names(got), err, c.want)
		}
		back, err := gce.ParseListFilter(c.lf.String())
		if err != nil || back.String() != c.lf.String() {
			t.Errorf("filter %q round trip: %q, %v", c.lf.String(), back.String(), err)
		}
	}
	// Firewall rules have no labels: only the name prefix selects them.
	fws, _ := f.ListFirewalls(ctx, proj, gce.ListFilter{NamePrefix: "k7-"})
	if len(fws) != 2 {
		t.Fatalf("firewalls by prefix: %d", len(fws))
	}
	fws, _ = f.ListFirewalls(ctx, proj, gce.ListFilter{Labels: map[string]string{"silo": "k7"}})
	if len(fws) != 0 {
		t.Fatalf("firewalls by label: %d", len(fws))
	}
	if _, err := gce.ParseListFilter(`name = "x"`); err == nil {
		t.Fatal("ParseListFilter accepted a filter ListFilter doesn't write")
	}
	if lf, err := gce.ParseListFilter(gce.ListFilter{NamePrefix: "a.b-"}.String()); err != nil || lf.NamePrefix != "a.b-" {
		t.Fatalf("escaped prefix: %+v %v", lf, err)
	}
}

func TestFirewallPolicy(t *testing.T) {
	f := newFake(t)
	mustDo(t, f)(f.InsertFirewallPolicy(ctx, proj, &compute.FirewallPolicy{Name: "silo-fence"}))
	pol, _ := f.GetFirewallPolicy(ctx, proj, "silo-fence")
	if len(pol.Rules) != 4 || pol.Rules[0].Priority != 2147483644 || pol.Rules[3].Action != "goto_next" {
		t.Fatalf("default rules %+v", pol.Rules)
	}
	fp0 := pol.Fingerprint
	rule := func(prio int64, action string) *compute.FirewallPolicyRule {
		return &compute.FirewallPolicyRule{Priority: prio, Action: action, Direction: "INGRESS",
			Match: &compute.FirewallPolicyRuleMatcher{SrcIpRanges: []string{"10.0.0.0/8"},
				Layer4Configs: []*compute.FirewallPolicyRuleMatcherLayer4Config{{IpProtocol: "all"}}}}
	}
	mustDo(t, f)(f.AddFirewallPolicyRule(ctx, proj, "silo-fence", rule(20000, "goto_next")))
	mustDo(t, f)(f.AddFirewallPolicyRule(ctx, proj, "silo-fence", rule(1000, "allow")))
	_, err := f.AddFirewallPolicyRule(ctx, proj, "silo-fence", rule(1000, "deny"))
	wantField(t, err, "priority")
	_, err = f.AddFirewallPolicyRule(ctx, proj, "silo-fence", rule(5, "permit"))
	wantField(t, err, "action")
	pol, _ = f.GetFirewallPolicy(ctx, proj, "silo-fence")
	if pol.Fingerprint == fp0 || pol.Rules[0].Priority != 1000 || pol.Rules[1].Priority != 20000 {
		t.Fatalf("after addRule: fingerprint %s, rules %d %d", pol.Fingerprint, pol.Rules[0].Priority, pol.Rules[1].Priority)
	}
	// patch is fingerprinted.
	_, err = f.PatchFirewallPolicy(ctx, proj, "silo-fence", &compute.FirewallPolicy{Fingerprint: fp0, Description: "x"})
	wantErr(t, err, http.StatusPreconditionFailed, gce.ReasonConditionNotMet)
	mustDo(t, f)(f.PatchFirewallPolicy(ctx, proj, "silo-fence", &compute.FirewallPolicy{Fingerprint: pol.Fingerprint, Description: "x"}))

	mustDo(t, f)(f.PatchFirewallPolicyRule(ctx, proj, "silo-fence", 20000, &compute.FirewallPolicyRule{Action: "deny"}))
	r, err := f.GetFirewallPolicyRule(ctx, proj, "silo-fence", 20000)
	if err != nil || r.Action != "deny" || r.Match.SrcIpRanges[0] != "10.0.0.0/8" {
		t.Fatalf("patched rule %+v %v", r, err)
	}
	mustDo(t, f)(f.RemoveFirewallPolicyRule(ctx, proj, "silo-fence", 20000))
	_, err = f.GetFirewallPolicyRule(ctx, proj, "silo-fence", 20000)
	wantField(t, err, "priority")
	_, err = f.RemoveFirewallPolicyRule(ctx, proj, "silo-fence", 2147483647)
	wantField(t, err, "priority")

	// An addRule that loses a race for its priority fails in its operation.
	op1, _ := f.AddFirewallPolicyRule(ctx, proj, "silo-fence", rule(3000, "allow"))
	op2, _ := f.AddFirewallPolicyRule(ctx, proj, "silo-fence", rule(3000, "deny"))
	f.RunOperations()
	g1, _ := f.GetOperation(ctx, proj, gce.Global, op1.Name)
	g2, _ := f.GetOperation(ctx, proj, gce.Global, op2.Name)
	if g1.Error != nil || g2.Error == nil {
		t.Fatalf("racing addRule: %+v / %+v", g1.Error, g2.Error)
	}

	// Associations: one policy per network, unless replaced.
	mustDo(t, f)(f.AddFirewallPolicyAssociation(ctx, proj, "silo-fence", &compute.FirewallPolicyAssociation{Name: "silo", AttachmentTarget: "global/networks/silo"}, false))
	n, _ := f.GetNetwork(ctx, proj, "silo")
	if !strings.HasSuffix(n.FirewallPolicy, "/firewallPolicies/silo-fence") || n.NetworkFirewallPolicyEnforcementOrder != "BEFORE_CLASSIC_FIREWALL" {
		t.Fatalf("network %+v", n)
	}
	mustDo(t, f)(f.InsertFirewallPolicy(ctx, proj, &compute.FirewallPolicy{Name: "other"}))
	_, err = f.AddFirewallPolicyAssociation(ctx, proj, "other", &compute.FirewallPolicyAssociation{AttachmentTarget: "global/networks/silo"}, false)
	wantField(t, err, "attachmentTarget")
	_, err = f.DeleteFirewallPolicy(ctx, proj, "silo-fence")
	wantErr(t, err, http.StatusBadRequest, gce.ReasonResourceInUse)
	mustDo(t, f)(f.AddFirewallPolicyAssociation(ctx, proj, "other", &compute.FirewallPolicyAssociation{AttachmentTarget: "global/networks/silo"}, true))
	pol, _ = f.GetFirewallPolicy(ctx, proj, "silo-fence")
	if len(pol.Associations) != 0 {
		t.Fatalf("replaced association still on silo-fence: %+v", pol.Associations)
	}
	mustDo(t, f)(f.RemoveFirewallPolicyAssociation(ctx, proj, "other", "network-silo"))
	mustDo(t, f)(f.DeleteFirewallPolicy(ctx, proj, "silo-fence"))
}

func TestFirewalls(t *testing.T) {
	f := newFake(t)
	f.SetQuota("", gce.MetricFirewalls, 1)
	_, err := f.InsertFirewall(ctx, proj, &compute.Firewall{Name: "both", Network: "global/networks/silo",
		Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp"}}, Denied: []*compute.FirewallDenied{{IPProtocol: "udp"}}})
	wantField(t, err, "resource")
	mustDo(t, f)(f.InsertFirewall(ctx, proj, &compute.Firewall{Name: "ssh", Network: "global/networks/silo",
		Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}, TargetTags: []string{"k7"}}))
	fw, _ := f.GetFirewall(ctx, proj, "ssh")
	if fw.Priority != 1000 || fw.Direction != "INGRESS" || fw.SourceRanges[0] != "0.0.0.0/0" {
		t.Fatalf("defaults %+v", fw)
	}
	if op := do(t, f)(f.InsertFirewall(ctx, proj, &compute.Firewall{Name: "two", Network: "global/networks/silo",
		Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp"}}})); opCode(op) != gce.CodeQuotaExceeded {
		t.Fatalf("FIREWALLS quota: %+v", op.Error)
	}
	mustDo(t, f)(f.PatchFirewall(ctx, proj, "ssh", &compute.Firewall{Priority: 500}))
	fw, _ = f.GetFirewall(ctx, proj, "ssh")
	if fw.Priority != 500 || fw.TargetTags[0] != "k7" {
		t.Fatalf("after patch %+v", fw)
	}
	mustDo(t, f)(f.UpdateFirewall(ctx, proj, "ssh", &compute.Firewall{Network: "global/networks/silo",
		Denied: []*compute.FirewallDenied{{IPProtocol: "all"}}, Direction: "EGRESS"}))
	fw, _ = f.GetFirewall(ctx, proj, "ssh")
	if len(fw.TargetTags) != 0 || fw.Direction != "EGRESS" || fw.Priority != 1000 {
		t.Fatalf("after update %+v", fw)
	}
	_, err = f.DeleteNetwork(ctx, proj, "silo")
	wantErr(t, err, http.StatusBadRequest, gce.ReasonResourceInUse)
}

func TestInstanceLifecycle(t *testing.T) {
	f := newFake(t)
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, vm("vm", "e2-standard-2"), ""))
	inst, _ := f.GetInstance(ctx, proj, zone, "vm")
	_, err := f.SetInstanceLabels(ctx, proj, zone, "vm", &compute.InstancesSetLabelsRequest{Labels: map[string]string{"a": "b"}, LabelFingerprint: "stale"})
	wantErr(t, err, http.StatusPreconditionFailed, gce.ReasonConditionNotMet)
	mustDo(t, f)(f.SetInstanceLabels(ctx, proj, zone, "vm", &compute.InstancesSetLabelsRequest{Labels: map[string]string{"a": "b"}, LabelFingerprint: inst.LabelFingerprint}))
	mustDo(t, f)(f.SetInstanceTags(ctx, proj, zone, "vm", &compute.Tags{Items: []string{"web"}, Fingerprint: inst.Tags.Fingerprint}))
	mustDo(t, f)(f.SetInstanceMetadata(ctx, proj, zone, "vm", &compute.Metadata{Items: []*compute.MetadataItems{{Key: "k", Value: ptr("v")}}, Fingerprint: inst.Metadata.Fingerprint}))
	inst, _ = f.GetInstance(ctx, proj, zone, "vm")
	if inst.Labels["a"] != "b" || inst.Tags.Items[0] != "web" || inst.Metadata.Items[0].Key != "k" {
		t.Fatalf("after set*: %+v", inst)
	}
	_, err = f.SetMachineType(ctx, proj, zone, "vm", "e2-standard-4")
	wantErr(t, err, http.StatusBadRequest, gce.ReasonNotReady)
	mustDo(t, f)(f.SuspendInstance(ctx, proj, zone, "vm"))
	mustDo(t, f)(f.ResumeInstance(ctx, proj, zone, "vm"))
	mustDo(t, f)(f.ResetInstance(ctx, proj, zone, "vm"))
	mustDo(t, f)(f.SetDeletionProtection(ctx, proj, zone, "vm", true))
	_, err = f.DeleteInstance(ctx, proj, zone, "vm")
	wantErr(t, err, http.StatusBadRequest, "")
	mustDo(t, f)(f.SetDeletionProtection(ctx, proj, zone, "vm", false))
	if _, err := f.GetSerialPortOutput(ctx, proj, zone, "vm", 1, 0); err != nil {
		t.Fatal(err)
	}
	mustDo(t, f)(f.DeleteInstance(ctx, proj, zone, "vm"))
	// The boot disk was autoDelete.
	if _, err := f.GetDisk(ctx, proj, zone, "vm"); !gce.IsNotFound(err) {
		t.Fatalf("boot disk after delete: %v", err)
	}
	// The address is free again; a given networkIP is checked.
	mustDo(t, f)(f.InsertInstance(ctx, proj, zone, vm("vm2", "e2-standard-2"), ""))
	dup := vm("vm3", "e2-standard-2")
	dup.NetworkInterfaces[0].NetworkIP = "10.0.0.2"
	_, err = f.InsertInstance(ctx, proj, zone, dup, "")
	wantField(t, err, "networkIP")
	dup.NetworkInterfaces[0].NetworkIP = "10.1.0.2"
	_, err = f.InsertInstance(ctx, proj, zone, dup, "")
	wantField(t, err, "networkIP")
	_, err = f.DeleteSubnetwork(ctx, proj, region, "sub")
	wantErr(t, err, http.StatusBadRequest, gce.ReasonResourceInUse)
}

func TestImagesAndSnapshots(t *testing.T) {
	f := newFake(t)
	f.SetQuota("", gce.MetricSnapshots, 1)
	mustDo(t, f)(f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "d", SizeGb: 20, SourceImage: "projects/debian-cloud/global/images/debian-12-bookworm-v20260901"}))
	mustDo(t, f)(f.CreateSnapshot(ctx, proj, zone, "d", &compute.Snapshot{Name: "s"}))
	if op := do(t, f)(f.CreateSnapshot(ctx, proj, zone, "d", &compute.Snapshot{Name: "s2"})); opCode(op) != gce.CodeQuotaExceeded {
		t.Fatalf("SNAPSHOTS quota: %+v", op.Error)
	}
	if _, err := f.GetSnapshot(ctx, proj, "s2"); !gce.IsNotFound(err) {
		t.Fatalf("refused snapshot exists: %v", err)
	}
	s, _ := f.GetSnapshot(ctx, proj, "s")
	if s.DiskSizeGb != 20 || s.Status != "READY" {
		t.Fatalf("snapshot %+v", s)
	}
	mustDo(t, f)(f.InsertImage(ctx, proj, &compute.Image{Name: "img-1", SourceSnapshot: "global/snapshots/s", Family: "mine"}))
	mustDo(t, f)(f.InsertImage(ctx, proj, &compute.Image{Name: "img-2", SourceDisk: "zones/" + zone + "/disks/d", Family: "mine"}))
	_, err := f.InsertImage(ctx, proj, &compute.Image{Name: "img-3"})
	wantField(t, err, "resource")
	img, _ := f.GetImageFromFamily(ctx, proj, "mine")
	if img.Name != "img-2" {
		t.Fatalf("family latest %s", img.Name)
	}
	mustDo(t, f)(f.DeprecateImage(ctx, proj, "img-2", &compute.DeprecationStatus{State: "DEPRECATED"}))
	img, _ = f.GetImageFromFamily(ctx, proj, "mine")
	if img.Name != "img-1" {
		t.Fatalf("family after deprecate %s", img.Name)
	}
	_, err = f.InsertDisk(ctx, proj, zone, &compute.Disk{Name: "small", SizeGb: 10, SourceImage: "global/images/img-2"})
	wantField(t, err, "sizeGb")
	mustDo(t, f)(f.SetImageLabels(ctx, proj, "img-1", &compute.GlobalSetLabelsRequest{Labels: map[string]string{"silo": "k7"}, LabelFingerprint: img.LabelFingerprint}))
	imgs, _ := f.ListImages(ctx, proj, gce.ListFilter{Labels: map[string]string{"silo": "k7"}})
	if len(imgs) != 1 {
		t.Fatalf("images by label: %d", len(imgs))
	}
	mustDo(t, f)(f.SetSnapshotLabels(ctx, proj, "s", &compute.GlobalSetLabelsRequest{Labels: map[string]string{"x": "y"}, LabelFingerprint: s.LabelFingerprint}))
	mustDo(t, f)(f.DeleteImage(ctx, proj, "img-1"))
	mustDo(t, f)(f.DeleteSnapshot(ctx, proj, "s"))
}
