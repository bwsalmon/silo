package gce

import (
	"strings"
	"testing"

	gceapi "github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// Tests of the GPU, local SSD, pd-extreme and Hyperdisk pricing
// (gcp-backend.md, "GPUs", "Local SSD" and "Disks", SV-67 to SV-69;
// allocators.md, "GPU classes", SV-28; testing.md, "Quota pricing").

// newHardwareEnv is newComputeEnv with team-a granted GPUs, local SSD,
// and pd-extreme and Hyperdisk capacity, IOPS and throughput.
func newHardwareEnv(t *testing.T) *computeEnv {
	t.Helper()
	e := newComputeEnv(t)
	regional := map[string]int64{
		"vcpu": 128, "memory_mb": 819200, "spot_vcpu": 32, "spot_memory_mb": 204800, "instances": 10, "ssd_disk_gb": 2000,
		"nvidia_t4_gpus": 4, "spot_nvidia_t4_gpus": 2, "nvidia_l4_gpus": 2, "nvidia_a100_gpus": 2,
		"local_ssd_gb": 1500, "spot_local_ssd_gb": 750, "pd_extreme_iops": 10000,
		"hyperdisk_balanced_gb": 1000, "hyperdisk_balanced_iops": 10000, "hyperdisk_balanced_throughput": 1000,
		"hyperdisk_extreme_gb": 1000, "hyperdisk_extreme_iops": 100000,
		"hyperdisk_throughput_gb": 10000, "hyperdisk_throughput_throughput": 1000,
		"hyperdisk_ml_gb": 1000, "hyperdisk_ml_throughput": 10000,
	}
	var root, a []ledger.Amount
	for c, n := range regional {
		k := ledger.Key{Kind: "gce", Class: c, Region: central}
		root = append(root, ledger.Amount{Key: k, Amount: ledger.Units(2 * n)})
		a = append(a, ledger.Amount{Key: k, Amount: ledger.Units(n)})
	}
	global := ledger.Key{Kind: "gce", Class: "gpus_all_regions", Region: ledger.Global}
	root = append(root, ledger.Amount{Key: global, Amount: ledger.Units(20)})
	a = append(a, ledger.Amount{Key: global, Amount: ledger.Units(5)})
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, e.root, root); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, e.a, a)
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// gpuVM is an instance of machineType with a pd-balanced boot disk and
// scheduling sc.
func gpuVM(name, machineType string, sc obj, extra obj) obj {
	o := vm(name, machineType, extra)
	if sc != nil {
		o["scheduling"] = sc
	}
	return o
}

var terminate = obj{"onHostMaintenance": "TERMINATE"}

func t4(n int) []obj { return []obj{{"acceleratorType": "nvidia-tesla-t4", "acceleratorCount": n}} }

// wantGPUs checks team-a's GPU usage: the regional classes and the global
// count.
func (e *computeEnv) wantGPUs(what string, classes map[string]int64, all int64) {
	e.t.Helper()
	e.want(what, e.a, classes)
	if got := e.used(e.a, "gpus_all_regions", ledger.Global); got != all {
		e.t.Errorf("%s: gpus_all_regions used %d, want %d", what, got, all)
	}
}

func TestGPUPricing(t *testing.T) {
	e := newHardwareEnv(t)
	// Attached, standard: the type's class and the global count.
	body := gpuVM("gpu", "n1-standard-4", terminate, obj{"guestAccelerators": t4(2)})
	got := e.preview(e.a, "POST", cv(host, instancesPath), body)
	if got["nvidia_t4_gpus"] != 2 || got["gpus_all_regions"] != 2 || got["vcpu"] != 4 {
		t.Errorf("preview, attached: %v", got)
	}
	e.ok(e.a, "POST", cv(host, instancesPath), body)
	e.wantGPUs("attached", map[string]int64{"nvidia_t4_gpus": 2, "vcpu": 4}, 2)
	e.run()
	real, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if len(real.GuestAccelerators) != 1 || !strings.HasSuffix(real.GuestAccelerators[0].AcceleratorType, "/projects/"+host+"/zones/"+zoneA+"/acceleratorTypes/nvidia-tesla-t4") {
		t.Errorf("real guestAccelerators: %+v", real.GuestAccelerators)
	}
	shown := e.ok(e.a, "GET", cv(host, instancesPath+"/gpu"), nil)
	if g, _ := shown["guestAccelerators"].([]any); len(g) != 1 || g[0].(obj)["acceleratorCount"] != float64(2) {
		t.Errorf("shown guestAccelerators: %v", shown["guestAccelerators"])
	}

	// Attached, Spot (and preemptible): the spot_ class, and the same
	// global count.
	for _, sc := range []obj{{"provisioningModel": "SPOT"}, {"preemptible": true}} {
		got := e.preview(e.a, "POST", cv(host, instancesPath), gpuVM("spot", "n1-standard-4", sc, obj{"guestAccelerators": t4(1)}))
		if got["spot_nvidia_t4_gpus"] != 1 || got["nvidia_t4_gpus"] != 0 || got["gpus_all_regions"] != 1 || got["spot_vcpu"] != 4 {
			t.Errorf("preview, Spot %v: %v", sc, got)
		}
	}
	e.ok(e.a, "POST", cv(host, instancesPath), gpuVM("spot", "n1-standard-4", obj{"provisioningModel": "SPOT"}, obj{"guestAccelerators": t4(2)}))
	e.wantGPUs("Spot", map[string]int64{"nvidia_t4_gpus": 2, "spot_nvidia_t4_gpus": 2}, 4)
	e.run()

	// Bundled: the machine type's GPUs charge the same classes.
	for mt, want := range map[string]map[string]int64{
		"g2-standard-4":  {"nvidia_l4_gpus": 1, "gpus_all_regions": 1},
		"g2-standard-24": {"nvidia_l4_gpus": 2, "gpus_all_regions": 2},
		"a2-highgpu-1g":  {"nvidia_a100_gpus": 1, "gpus_all_regions": 1},
		"a2-ultragpu-1g": {"nvidia_a100_80gb_gpus": 1, "gpus_all_regions": 1, "local_ssd_gb": 375},
	} {
		got := e.preview(e.a, "POST", cv(host, instancesPath), gpuVM("b", mt, terminate, nil))
		for c, n := range want {
			if got[c] != n {
				t.Errorf("preview, %s: %s %d, want %d (%v)", mt, c, got[c], n, got)
			}
		}
		spot := e.preview(e.a, "POST", cv(host, instancesPath), gpuVM("b", mt, obj{"provisioningModel": "SPOT"}, nil))
		for c, n := range want {
			if c != "gpus_all_regions" {
				c = "spot_" + c
			}
			if spot[c] != n {
				t.Errorf("preview, Spot %s: %s %d, want %d (%v)", mt, c, spot[c], n, spot)
			}
		}
	}
	// One more past the global count: team-a has 5, and holds 4.
	msg := e.fail(403, "quotaExceeded", e.a, "POST", cv(host, instancesPath), gpuVM("g2", "g2-standard-24", terminate, nil))
	if !strings.Contains(msg, "gpus_all_regions") {
		t.Errorf("quota message: %q", msg)
	}
	e.ok(e.a, "POST", cv(host, instancesPath), gpuVM("g2", "g2-standard-4", terminate, nil))
	e.wantGPUs("bundled", map[string]int64{"nvidia_l4_gpus": 1, "nvidia_t4_gpus": 2}, 5)
	e.run()
	// And past a class: two more T4s fit the class but not the count.
	e.fail(403, "quotaExceeded", e.a, "POST", cv(host, instancesPath), gpuVM("more", "n1-standard-4", terminate, obj{"guestAccelerators": t4(1)}))
	e.wantGPUs("after the refusals", map[string]int64{"nvidia_l4_gpus": 1, "nvidia_t4_gpus": 2}, 5)

	// A GPU type with no class in silod's config isn't allowed; once
	// root capacity finds its family, it is priced.
	msg = e.fail(403, "forbidden", e.a, "POST", cv(host, instancesPath), gpuVM("h", "a3-highgpu-8g", terminate, nil))
	if !strings.Contains(msg, "nvidia-h100-80gb") {
		t.Errorf("unpriced GPU: %q", msg)
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return ledger.AddGPU(ctx, tx, ledger.GPUClass{Accelerator: "nvidia-h100-80gb", Metric: "NVIDIA_H100_GPUS", Class: "nvidia_h100_gpus"})
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.preview(e.a, "POST", cv(host, instancesPath), gpuVM("h", "a3-highgpu-8g", terminate, nil)); got["nvidia_h100_gpus"] != 8 || got["gpus_all_regions"] != 8 || got["local_ssd_gb"] != 16*375 {
		t.Errorf("preview, a3 after AddGPU: %v", got)
	}

	// A stopped instance holds no GPUs; start takes them again; delete
	// gives back the rest.
	e.ok(e.a, "POST", cv(host, instancesPath+"/gpu/stop"), nil)
	e.run()
	e.wantGPUs("stopped", map[string]int64{"nvidia_t4_gpus": 0, "nvidia_l4_gpus": 1}, 3)
	if got := e.preview(e.a, "POST", cv(host, instancesPath+"/gpu/start"), nil); got["nvidia_t4_gpus"] != 2 || got["gpus_all_regions"] != 2 {
		t.Errorf("preview of start: %v", got)
	}
	e.ok(e.a, "POST", cv(host, instancesPath+"/gpu/start"), nil)
	e.run()
	e.wantGPUs("started", map[string]int64{"nvidia_t4_gpus": 2}, 5)
	for _, n := range []string{"gpu", "spot", "g2"} {
		e.ok(e.a, "DELETE", cv(host, instancesPath+"/"+n), nil)
	}
	e.run()
	e.wantGPUs("deleted", map[string]int64{"nvidia_t4_gpus": 0, "spot_nvidia_t4_gpus": 0, "nvidia_l4_gpus": 0, "vcpu": 0}, 0)
}

func TestGPUChecks(t *testing.T) {
	e := newHardwareEnv(t)
	zoneC := "us-central1-c"
	// Every refusal comes before anything is charged, as GCE's 400 (or
	// 403 for a region), naming the field.
	for _, c := range []struct {
		path   string
		body   obj
		code   int
		reason string
		names  string
	}{
		{instancesPath, gpuVM("x", "n1-standard-4", nil, obj{"guestAccelerators": t4(1)}), 400, "invalid", "scheduling.onHostMaintenance"},
		{instancesPath, gpuVM("x", "n1-standard-4", obj{"onHostMaintenance": "MIGRATE"}, obj{"guestAccelerators": t4(1)}), 400, "invalid", "scheduling.onHostMaintenance"},
		{instancesPath, gpuVM("x", "g2-standard-4", nil, nil), 400, "invalid", "scheduling.onHostMaintenance"},
		{instancesPath, gpuVM("x", "e2-standard-4", terminate, obj{"guestAccelerators": t4(1)}), 400, "invalid", "guestAccelerators[0].acceleratorType"},
		{instancesPath, gpuVM("x", "n1-standard-4", terminate, obj{"guestAccelerators": t4(3)}), 400, "invalid", "guestAccelerators[0].acceleratorCount"},
		{instancesPath, gpuVM("x", "n1-standard-4", terminate, obj{"guestAccelerators": t4(0)}), 400, "invalid", "guestAccelerators[0].acceleratorCount"},
		{instancesPath, gpuVM("x", "g2-standard-4", terminate, obj{"guestAccelerators": []obj{{"acceleratorType": "nvidia-l4", "acceleratorCount": 1}}}), 400, "invalid", "guestAccelerators"},
		{instancesPath, gpuVM("x", "n1-standard-4", terminate, obj{"guestAccelerators": []obj{{"acceleratorType": "nvidia-l4", "acceleratorCount": 1}}}), 400, "invalid", "guestAccelerators[0].acceleratorType"},
		{instancesPath, gpuVM("x", "n1-standard-4", terminate, obj{"guestAccelerators": []obj{{"acceleratorType": "nvidia-tesla-p4", "acceleratorCount": 1}}}), 400, "invalid", "not available in zone"},
		{instancesPath, gpuVM("x", "n1-standard-4", terminate, obj{"guestAccelerators": []obj{{"acceleratorType": "projects/" + host + "/zones/europe-west4-a/acceleratorTypes/nvidia-tesla-t4", "acceleratorCount": 1}}}), 403, "regionNotAllowed", "europe-west4-a"},
		{instancesPath, gpuVM("x", "n1-standard-4", terminate, obj{"guestAccelerators": []obj{{"acceleratorType": "zones/us-central1-b/acceleratorTypes/nvidia-tesla-t4", "acceleratorCount": 1}}}), 400, "invalid", "us-central1-a"},
		// V100 is offered in us-central1-a, not in -c.
		{"zones/" + zoneC + "/instances", obj{"name": "x", "machineType": "n1-standard-4", "scheduling": terminate,
			"guestAccelerators": []obj{{"acceleratorType": "nvidia-tesla-v100", "acceleratorCount": 1}},
			"networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/web"}},
			"disks":             []obj{{"boot": true, "initializeParams": obj{"sourceImage": debian}}}}, 400, "invalid", "not available in zone " + zoneC},
	} {
		msg := e.fail(c.code, c.reason, e.a, "POST", cv(host, c.path), c.body)
		if !strings.Contains(msg, c.names) {
			t.Errorf("%v: %q doesn't name %s", c.body["guestAccelerators"], msg, c.names)
		}
	}
	e.wantGPUs("nothing charged", map[string]int64{"vcpu": 0, "instances": 0, "nvidia_t4_gpus": 0, "nvidia_l4_gpus": 0, "ssd_disk_gb": 0}, 0)
	if l, _ := e.gce.ListInstances(ctx, host, zoneA, gceLabels(e.a)); len(l) != 0 {
		t.Errorf("GCE was called: %d instances", len(l))
	}

	// A combination the table misses is GCE's to refuse: its 400 is
	// passed through, and the charge refunded.
	e.facade.opts.Compat.Accelerators["nvidia-tesla-t4"] = AcceleratorCompat{Series: []string{"n1"}, Counts: []int64{1, 2, 3, 4}}
	msg := e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath), gpuVM("x", "n1-standard-4", terminate, obj{"guestAccelerators": t4(3)}))
	if !strings.Contains(msg, "acceleratorCount") {
		t.Errorf("GCE's refusal: %q", msg)
	}
	e.wantGPUs("GCE refused", map[string]int64{"vcpu": 0, "nvidia_t4_gpus": 0}, 0)

	// setMachineType checks the GPUs again: a T4 instance can't move to
	// E2, nor to a type that bundles its own.
	e.ok(e.a, "POST", cv(host, instancesPath), gpuVM("gpu", "n1-standard-4", terminate, obj{"guestAccelerators": t4(1)}))
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/gpu/stop"), nil)
	e.run()
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/gpu/setMachineType"), obj{"machineType": "e2-standard-4"})
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/gpu/setMachineType"), obj{"machineType": "g2-standard-4"})
	e.ok(e.a, "POST", cv(host, instancesPath+"/gpu/setMachineType"), obj{"machineType": "n1-standard-8"})
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/gpu/start"), nil)
	e.run()
	e.wantGPUs("after setMachineType", map[string]int64{"vcpu": 8, "nvidia_t4_gpus": 1}, 1)
}

// gceLabels lists a silo's own objects.
func gceLabels(silo string) gceapi.ListFilter {
	return gceapi.ListFilter{Labels: map[string]string{"silo": silo}}
}

func TestBundledGPUsSetMachineType(t *testing.T) {
	e := newHardwareEnv(t)
	// setMachineType re-prices the bundled GPUs: g2-standard-4 (one L4)
	// to g2-standard-24 (two), stopped, then started.
	e.ok(e.a, "POST", cv(host, instancesPath), gpuVM("g", "g2-standard-4", terminate, nil))
	e.run()
	e.wantGPUs("g2-standard-4", map[string]int64{"nvidia_l4_gpus": 1, "vcpu": 4}, 1)
	e.ok(e.a, "POST", cv(host, instancesPath+"/g/stop"), nil)
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/g/setMachineType"), obj{"machineType": "g2-standard-24"})
	e.run()
	e.wantGPUs("stopped", map[string]int64{"nvidia_l4_gpus": 0, "vcpu": 0}, 0)
	if got := e.preview(e.a, "POST", cv(host, instancesPath+"/g/start"), nil); got["nvidia_l4_gpus"] != 2 || got["gpus_all_regions"] != 2 || got["vcpu"] != 24 {
		t.Errorf("preview of start: %v", got)
	}
	e.ok(e.a, "POST", cv(host, instancesPath+"/g/start"), nil)
	e.run()
	e.wantGPUs("g2-standard-24", map[string]int64{"nvidia_l4_gpus": 2, "vcpu": 24}, 2)
	// A machine type with no GPUs: the instance gives them back.
	e.ok(e.a, "POST", cv(host, instancesPath+"/g/stop"), nil)
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/g/setMachineType"), obj{"machineType": "n1-standard-4"})
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/g/start"), nil)
	e.run()
	e.wantGPUs("n1-standard-4", map[string]int64{"nvidia_l4_gpus": 0, "vcpu": 4}, 0)
}

func TestLocalSSD(t *testing.T) {
	e := newHardwareEnv(t)
	scratch := func(iface string) obj {
		return obj{"type": "SCRATCH", "autoDelete": true, "interface": iface, "initializeParams": obj{"diskType": "local-ssd"}}
	}
	boot := obj{"boot": true, "autoDelete": true, "initializeParams": obj{"sourceImage": debian, "diskType": "pd-balanced"}}
	withSSD := func(n int) obj {
		disks := []obj{boot}
		for i := 0; i < n; i++ {
			disks = append(disks, scratch("NVME"))
		}
		return obj{"disks": disks}
	}
	// Attached: 375 GB a partition, standard and Spot.
	if got := e.preview(e.a, "POST", cv(host, instancesPath), vm("x", "n2-standard-8", withSSD(2))); got["local_ssd_gb"] != 750 || got["ssd_disk_gb"] != 10 {
		t.Errorf("preview, attached: %v", got)
	}
	spot := vm("x", "n2-standard-8", withSSD(1))
	spot["scheduling"] = obj{"provisioningModel": "SPOT"}
	if got := e.preview(e.a, "POST", cv(host, instancesPath), spot); got["spot_local_ssd_gb"] != 375 || got["local_ssd_gb"] != 0 {
		t.Errorf("preview, Spot: %v", got)
	}
	// Bundled: what the machine type comes with.
	if got := e.preview(e.a, "POST", cv(host, instancesPath), vm("x", "c3-standard-8-lssd", nil)); got["local_ssd_gb"] != 750 {
		t.Errorf("preview, bundled: %v", got)
	}

	// The refusals, before any charge.
	for _, c := range []struct {
		mt    string
		disks []obj
		names string
	}{
		{"e2-standard-4", []obj{boot, scratch("NVME")}, "disks[1]"},
		{"c3-standard-8-lssd", []obj{boot, scratch("NVME")}, "disks[1]"},
		{"n2-standard-8", []obj{boot, scratch("VIRTIO")}, "disks[1].interface"},
		{"n2-standard-8", []obj{boot, {"type": "SCRATCH", "initializeParams": obj{"diskType": "pd-ssd"}}}, "disks[1].initializeParams.diskType"},
		{"n2-standard-8", []obj{boot, {"type": "SCRATCH", "initializeParams": obj{"diskType": "local-ssd", "diskSizeGb": "100"}}}, "disks[1].initializeParams.diskSizeGb"},
	} {
		msg := e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath), vm("x", c.mt, obj{"disks": c.disks}))
		if !strings.Contains(msg, c.names) {
			t.Errorf("%s %v: %q doesn't name %s", c.mt, c.disks, msg, c.names)
		}
	}
	e.want("nothing charged", e.a, map[string]int64{"local_ssd_gb": 0, "vcpu": 0, "instances": 0})

	// For as long as the instance exists, stopped too.
	e.ok(e.a, "POST", cv(host, instancesPath), vm("ssd", "n2-standard-8", withSSD(2)))
	e.ok(e.a, "POST", cv(host, instancesPath), vm("lssd", "c3-standard-4-lssd", nil))
	e.run()
	e.want("running", e.a, map[string]int64{"local_ssd_gb": 1125, "vcpu": 12})
	real, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-ssd")
	if err != nil {
		t.Fatal(err)
	}
	if d := real.Disks[1]; d.Type != "SCRATCH" || d.Interface != "NVME" || d.DiskSizeGb != 375 {
		t.Errorf("real local SSD: %+v", d)
	}
	// Local SSD can't be attached afterwards.
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/ssd/attachDisk"), scratch("NVME"))
	e.ok(e.a, "POST", cv(host, instancesPath+"/ssd/stop?discardLocalSsd=true"), nil)
	e.run()
	e.want("stopped", e.a, map[string]int64{"local_ssd_gb": 1125, "vcpu": 4})

	// setMachineType keeps the attached partitions, and can't move them
	// to a type that takes none.
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/ssd/setMachineType"), obj{"machineType": "c3-standard-8-lssd"})
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/ssd/setMachineType"), obj{"machineType": "e2-standard-8"})
	e.ok(e.a, "POST", cv(host, instancesPath+"/ssd/setMachineType"), obj{"machineType": "n2-standard-2"})
	e.run()
	e.want("after setMachineType", e.a, map[string]int64{"local_ssd_gb": 1125})
	// A bundling type re-prices with its own: -4-lssd (1) to -8-lssd (2).
	e.ok(e.a, "POST", cv(host, instancesPath+"/lssd/stop"), nil)
	e.run()
	if got := e.preview(e.a, "POST", cv(host, instancesPath+"/lssd/setMachineType"), obj{"machineType": "c3-standard-8-lssd"}); got["local_ssd_gb"] != 375 || len(got) != 1 {
		t.Errorf("preview of setMachineType to more local SSD: %v", got)
	}
	e.ok(e.a, "POST", cv(host, instancesPath+"/lssd/setMachineType"), obj{"machineType": "c3-standard-8-lssd"})
	e.run()
	e.want("bundled, after setMachineType", e.a, map[string]int64{"local_ssd_gb": 1500})
	e.ok(e.a, "DELETE", cv(host, instancesPath+"/ssd"), nil)
	e.ok(e.a, "DELETE", cv(host, instancesPath+"/lssd"), nil)
	e.run()
	e.want("deleted", e.a, map[string]int64{"local_ssd_gb": 0, "ssd_disk_gb": 0, "instances": 0})
}

func TestHyperdiskPricing(t *testing.T) {
	e := newHardwareEnv(t)
	disks := "zones/" + zoneA + "/disks"
	dt := func(t string) string { return "zones/" + zoneA + "/diskTypes/" + t }
	// Each type charges its classes; where the tenant gives no IOPS or
	// throughput, GCE's default for the type and size.
	for _, c := range []struct {
		body obj
		want map[string]int64
	}{
		{obj{"sizeGb": "500", "type": dt("pd-extreme")}, map[string]int64{"ssd_disk_gb": 500, "pd_extreme_iops": 2500}},
		{obj{"sizeGb": "500", "type": dt("pd-extreme"), "provisionedIops": "5000"}, map[string]int64{"ssd_disk_gb": 500, "pd_extreme_iops": 5000}},
		{obj{"sizeGb": "100", "type": dt("hyperdisk-balanced")}, map[string]int64{"hyperdisk_balanced_gb": 100, "hyperdisk_balanced_iops": 3600, "hyperdisk_balanced_throughput": 200}},
		{obj{"sizeGb": "100", "type": dt("hyperdisk-balanced"), "provisionedIops": "4000", "provisionedThroughput": "300"}, map[string]int64{"hyperdisk_balanced_gb": 100, "hyperdisk_balanced_iops": 4000, "hyperdisk_balanced_throughput": 300}},
		{obj{"sizeGb": "100", "type": dt("hyperdisk-extreme")}, map[string]int64{"hyperdisk_extreme_gb": 100, "hyperdisk_extreme_iops": 10000}},
		{obj{"sizeGb": "2048", "type": dt("hyperdisk-throughput")}, map[string]int64{"hyperdisk_throughput_gb": 2048, "hyperdisk_throughput_throughput": 20}},
		{obj{"sizeGb": "100", "type": dt("hyperdisk-ml"), "accessMode": "READ_ONLY_MANY"}, map[string]int64{"hyperdisk_ml_gb": 100, "hyperdisk_ml_throughput": 2400}},
	} {
		c.body["name"] = "d"
		got := e.preview(e.a, "POST", cv(host, disks), c.body)
		if len(got) != len(c.want) {
			t.Errorf("%v: %v, want %v", c.body, got, c.want)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%v: %s %d, want %d", c.body, k, got[k], v)
			}
		}
	}
	// The insert charges the default, and shows what GCE set.
	e.ok(e.a, "POST", cv(host, disks), obj{"name": "hdb", "sizeGb": "100", "type": dt("hyperdisk-balanced")})
	e.ok(e.a, "POST", cv(host, disks), obj{"name": "hdt", "sizeGb": "2048", "type": dt("hyperdisk-throughput")})
	e.run()
	e.want("inserted", e.a, map[string]int64{"hyperdisk_balanced_gb": 100, "hyperdisk_balanced_iops": 3600, "hyperdisk_balanced_throughput": 200,
		"hyperdisk_throughput_gb": 2048, "hyperdisk_throughput_throughput": 20})
	d := e.ok(e.a, "GET", cv(host, disks+"/hdb"), nil)
	if d["provisionedIops"] != "3600" || d["provisionedThroughput"] != "200" {
		t.Errorf("shown: iops %v throughput %v", d["provisionedIops"], d["provisionedThroughput"])
	}

	// A field the type doesn't take is refused before any charge.
	e.fail(400, "invalid", e.a, "POST", cv(host, disks), obj{"name": "x", "sizeGb": "500", "type": dt("pd-extreme"), "provisionedThroughput": "100"})
	e.fail(400, "invalid", e.a, "POST", cv(host, disks), obj{"name": "x", "sizeGb": "100", "type": dt("pd-ssd"), "provisionedIops": "10000"})
	e.fail(400, "invalid", e.a, "POST", cv(host, disks), obj{"name": "x", "sizeGb": "500", "type": dt("pd-extreme"), "accessMode": "READ_WRITE_MANY"})
	e.fail(403, "forbidden", e.a, "POST", cv(host, disks), obj{"name": "x", "sizeGb": "100", "type": dt("hyperdisk-balanced"), "storagePool": "p"})
	// Past the grant: 10000 IOPS, 3600 held.
	e.fail(403, "quotaExceeded", e.a, "POST", cv(host, disks), obj{"name": "x", "sizeGb": "100", "type": dt("hyperdisk-balanced"), "provisionedIops": "7000"})
	e.want("after the refusals", e.a, map[string]int64{"ssd_disk_gb": 0, "pd_extreme_iops": 0, "hyperdisk_balanced_gb": 100, "hyperdisk_balanced_iops": 3600})

	// update charges an increase before GCE is called, and refunds a
	// decrease once it has worked.
	if got := e.preview(e.a, "PATCH", cv(host, disks+"/hdb?paths=provisionedIops"), obj{"provisionedIops": "5000"}); len(got) != 1 || got["hyperdisk_balanced_iops"] != 1400 {
		t.Errorf("preview of update: %v", got)
	}
	e.ok(e.a, "PATCH", cv(host, disks+"/hdb?paths=provisionedIops&paths=provisionedThroughput"), obj{"provisionedIops": "5000", "provisionedThroughput": "150"})
	e.want("raising IOPS, lowering throughput", e.a, map[string]int64{"hyperdisk_balanced_iops": 5000, "hyperdisk_balanced_throughput": 200})
	e.run()
	e.want("updated", e.a, map[string]int64{"hyperdisk_balanced_iops": 5000, "hyperdisk_balanced_throughput": 150})
	if real, _ := e.gce.GetDisk(ctx, host, zoneA, e.a+"-hdb"); real.ProvisionedIops != 5000 || real.ProvisionedThroughput != 150 {
		t.Errorf("real: iops %d throughput %d", real.ProvisionedIops, real.ProvisionedThroughput)
	}
	// Past the grant: refused, nothing charged.
	e.fail(403, "quotaExceeded", e.a, "PATCH", cv(host, disks+"/hdb?paths=provisionedIops"), obj{"provisionedIops": "20000"})
	e.want("update past the grant", e.a, map[string]int64{"hyperdisk_balanced_iops": 5000})
	// GCE's refusal (past its range: 180 MiB/s at 2048 GB) is passed
	// through and the charge refunded.
	e.fail(400, "invalid", e.a, "PATCH", cv(host, disks+"/hdt?paths=provisionedThroughput"), obj{"provisionedThroughput": "300"})
	e.want("update GCE refused", e.a, map[string]int64{"hyperdisk_throughput_throughput": 20})
	// A failed operation is refunded.
	e.gce.FailOperation(fake.Failure{Method: "disks.update", Kind: fake.FailInternal})
	e.ok(e.a, "PATCH", cv(host, disks+"/hdb?paths=provisionedIops"), obj{"provisionedIops": "6000"})
	e.want("updating", e.a, map[string]int64{"hyperdisk_balanced_iops": 6000})
	e.run()
	e.want("update failed", e.a, map[string]int64{"hyperdisk_balanced_iops": 5000})
	// Lowering gives back once it has worked.
	e.ok(e.a, "PATCH", cv(host, disks+"/hdb?paths=provisionedIops"), obj{"provisionedIops": "3000"})
	e.want("lowering", e.a, map[string]int64{"hyperdisk_balanced_iops": 5000})
	e.run()
	e.want("lowered", e.a, map[string]int64{"hyperdisk_balanced_iops": 3000})
	// accessMode, on a disk not in use; nothing else.
	e.ok(e.a, "PATCH", cv(host, disks+"/hdb?paths=accessMode"), obj{"accessMode": "READ_WRITE_MANY"})
	e.run()
	e.fail(403, "forbidden", e.a, "PATCH", cv(host, disks+"/hdb?paths=sizeGb"), obj{"sizeGb": "200"})
	e.fail(400, "invalid", e.a, "PATCH", cv(host, disks+"/hdt?paths=provisionedIops"), obj{"provisionedIops": "5000"})
	e.fail(400, "invalid", e.a, "PATCH", cv(host, disks+"/hdb"), obj{"provisionedIops": "5000"})

	// resize charges the GB, and the throughput GCE raises to the new
	// size's minimum: hyperdisk-throughput at 4096 GB is 40 MiB/s.
	if got := e.preview(e.a, "POST", cv(host, disks+"/hdt/resize"), obj{"sizeGb": "4096"}); got["hyperdisk_throughput_gb"] != 2048 || got["hyperdisk_throughput_throughput"] != 20 {
		t.Errorf("preview of resize: %v", got)
	}
	e.ok(e.a, "POST", cv(host, disks+"/hdt/resize"), obj{"sizeGb": "4096"})
	e.run()
	e.want("resized", e.a, map[string]int64{"hyperdisk_throughput_gb": 4096, "hyperdisk_throughput_throughput": 40})
	if real, _ := e.gce.GetDisk(ctx, host, zoneA, e.a+"-hdt"); real.ProvisionedThroughput != 40 {
		t.Errorf("GCE's throughput after resize: %d", real.ProvisionedThroughput)
	}
	// hyperdisk-balanced's minimum doesn't move: the GB only.
	if got := e.preview(e.a, "POST", cv(host, disks+"/hdb/resize"), obj{"sizeGb": "200"}); len(got) != 1 || got["hyperdisk_balanced_gb"] != 100 {
		t.Errorf("preview of hyperdisk-balanced resize: %v", got)
	}
	// Deletes give it all back.
	e.ok(e.a, "DELETE", cv(host, disks+"/hdb"), nil)
	e.ok(e.a, "DELETE", cv(host, disks+"/hdt"), nil)
	e.run()
	e.want("deleted", e.a, map[string]int64{"hyperdisk_balanced_gb": 0, "hyperdisk_balanced_iops": 0, "hyperdisk_balanced_throughput": 0,
		"hyperdisk_throughput_gb": 0, "hyperdisk_throughput_throughput": 0})
}

func TestDiskMachineCompat(t *testing.T) {
	e := newHardwareEnv(t)
	disks := "zones/" + zoneA + "/disks"
	bootOf := func(typ string) obj {
		return obj{"disks": []obj{{"boot": true, "autoDelete": true, "initializeParams": obj{"sourceImage": debian, "diskType": typ}}}}
	}
	// insert: Hyperdisk on a series that doesn't take it, pd-* on one
	// that takes only Hyperdisk, pd-extreme below 64 vCPUs.
	for _, c := range []struct{ mt, typ string }{
		{"e2-standard-4", "hyperdisk-balanced"},
		{"n2-standard-8", "hyperdisk-extreme"},
		{"c4-standard-4", "pd-balanced"},
		{"n4-standard-4", "pd-standard"},
		{"n2-standard-8", "pd-extreme"},
	} {
		msg := e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath), vm("x", c.mt, bootOf(c.typ)))
		if !strings.Contains(msg, "disks[0]") || !strings.Contains(msg, c.typ) {
			t.Errorf("%s on %s: %q", c.typ, c.mt, msg)
		}
	}
	e.want("nothing charged", e.a, map[string]int64{"vcpu": 0, "instances": 0, "ssd_disk_gb": 0, "hyperdisk_balanced_gb": 0})
	// And the ones that fit, including a new Hyperdisk with the
	// instance, which charges its default performance.
	got := e.preview(e.a, "POST", cv(host, instancesPath), vm("x", "c3-standard-4", bootOf("hyperdisk-balanced")))
	if got["hyperdisk_balanced_gb"] != 10 || got["hyperdisk_balanced_iops"] != 3060 || got["hyperdisk_balanced_throughput"] != 146 {
		t.Errorf("preview of a Hyperdisk boot disk: %v", got)
	}
	e.ok(e.a, "POST", cv(host, instancesPath), vm("c3", "c3-standard-4", bootOf("hyperdisk-balanced")))
	e.ok(e.a, "POST", cv(host, instancesPath), vm("n2", "n2-standard-64", obj{"disks": []obj{{"boot": true, "autoDelete": true,
		"initializeParams": obj{"sourceImage": debian, "diskType": "pd-extreme", "diskSizeGb": "500"}}}}))
	e.ok(e.a, "POST", cv(host, instancesPath), vm("e2", "e2-standard-4", nil))
	e.ok(e.a, "POST", cv(host, disks), obj{"name": "hd", "sizeGb": "50", "type": "hyperdisk-balanced"})
	e.run()
	e.want("inserted", e.a, map[string]int64{"hyperdisk_balanced_gb": 60, "ssd_disk_gb": 510, "pd_extreme_iops": 2500})

	// attachDisk: the same check.
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/e2/attachDisk"), obj{"source": disks + "/hd"})
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/e2/attachDisk"), obj{"initializeParams": obj{"diskType": "hyperdisk-balanced", "diskSizeGb": "10"}})
	e.want("attach refused", e.a, map[string]int64{"hyperdisk_balanced_gb": 60})
	e.ok(e.a, "POST", cv(host, instancesPath+"/c3/attachDisk"), obj{"source": disks + "/hd"})
	e.run()

	// setMachineType: the instance's disks against the new type.
	e.ok(e.a, "POST", cv(host, instancesPath+"/c3/stop"), nil)
	e.ok(e.a, "POST", cv(host, instancesPath+"/n2/stop"), nil)
	e.run()
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/c3/setMachineType"), obj{"machineType": "e2-standard-4"})
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/n2/setMachineType"), obj{"machineType": "n2-standard-8"})
	e.ok(e.a, "POST", cv(host, instancesPath+"/c3/setMachineType"), obj{"machineType": "c3-standard-8"})
	e.ok(e.a, "POST", cv(host, instancesPath+"/n2/setMachineType"), obj{"machineType": "n2-standard-80"})
	e.run()

	// A multi-writer disk attaches only to the silo's own instances:
	// team-b can't name it, by its project or by its name.
	e.ok(e.a, "POST", cv(host, disks), obj{"name": "shared", "sizeGb": "50", "type": "hyperdisk-balanced", "accessMode": "READ_WRITE_MANY"})
	e.run()
	e.ok(e.b, "POST", cv(host, instancesPath), vm("b", "e2-standard-2", nil))
	e.run()
	e.fail(403, "forbidden", e.b, "POST", cv(host, instancesPath+"/b/attachDisk"), obj{"source": "projects/silo-" + e.a + "/" + disks + "/shared"})
	e.fail(404, "notFound", e.b, "POST", cv(host, instancesPath+"/b/attachDisk"), obj{"source": disks + "/shared"})
	e.ok(e.a, "POST", cv(host, instancesPath+"/c3/attachDisk"), obj{"source": disks + "/shared"})
	e.run()
}
