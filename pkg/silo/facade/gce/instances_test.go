package gce

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

const (
	zoneA   = "us-central1-a"
	debian  = "projects/debian-cloud/global/images/family/debian-12"
	central = "us-central1"
)

// compute is newEnv with the classes instances and storage charge
// granted, a VPC, and a subnet web for team-a and one for team-b.
type computeEnv struct {
	*env
	webA, webB, pods netip.Prefix
}

func newComputeEnv(t *testing.T) *computeEnv {
	t.Helper()
	e := &computeEnv{env: newEnv(t)}
	e.vpc()
	amounts := func(regional, global map[string]int64) []ledger.Amount {
		var out []ledger.Amount
		for c, n := range regional {
			out = append(out, ledger.Amount{Key: ledger.Key{Kind: "gce", Class: c, Region: central}, Amount: ledger.Units(n)})
		}
		for c, n := range global {
			out = append(out, ledger.Amount{Key: ledger.Key{Kind: "gce", Class: c, Region: ledger.Global}, Amount: ledger.Units(n)})
		}
		return out
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, e.root, amounts(
			map[string]int64{"vcpu": 100, "memory_mb": 409600, "spot_vcpu": 50, "spot_memory_mb": 204800, "instances": 50, "standard_disk_gb": 2000, "ssd_disk_gb": 1000},
			map[string]int64{"subnetworks": 20, "images": 10, "snapshots": 10, "image_storage_gb": 1000, "snapshot_storage_gb": 1000})); err != nil {
			return err
		}
		if err := ledger.Grant(ctx, tx, e.a, amounts(
			map[string]int64{"vcpu": 16, "memory_mb": 65536, "spot_vcpu": 8, "spot_memory_mb": 32768, "instances": 5, "standard_disk_gb": 500, "ssd_disk_gb": 100},
			map[string]int64{"subnetworks": 2, "images": 3, "snapshots": 3, "image_storage_gb": 100, "snapshot_storage_gb": 100})); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, e.b, amounts(
			map[string]int64{"vcpu": 8, "memory_mb": 32768, "instances": 3, "standard_disk_gb": 200, "ssd_disk_gb": 100},
			map[string]int64{"subnetworks": 2, "images": 2, "snapshots": 2, "image_storage_gb": 50, "snapshot_storage_gb": 50}))
	}); err != nil {
		t.Fatal(err)
	}
	e.webA = freeIn(e.blocks(e.a)[0], 24, e.blocks(e.ci)[0], e.blocks(e.sealed)[0])
	e.pods = freeIn(e.blocks(e.a)[0], 24, e.blocks(e.ci)[0], e.blocks(e.sealed)[0], e.webA)
	e.webB = sub(e.blocks(e.b)[0], 24, 0)
	e.ok(e.a, "POST", cv(host, "regions/us-central1/subnetworks"), obj{"name": "web", "ipCidrRange": e.webA.String(),
		"secondaryIpRanges": []obj{{"rangeName": "pods", "ipCidrRange": e.pods.String()}}})
	e.ok(e.b, "POST", cv(host, "regions/us-central1/subnetworks"), obj{"name": "web", "ipCidrRange": e.webB.String()})
	e.run()
	return e
}

// run finishes every GCE operation and waits for silod to see them.
func (e *computeEnv) run() {
	e.t.Helper()
	e.gce.RunOperations()
	e.eventually("operations finished", func() bool {
		done := true
		e.db.Tx(ctx, func(tx store.Tx) error {
			for _, s := range []string{e.a, e.b} {
				l, err := Operations(ctx, tx, s)
				for _, r := range l {
					done = done && r.Done
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
		return done
	})
}

// used is silo's used amount of a class in region (global for a
// global count).
func (e *computeEnv) used(silo, class, region string) int64 {
	e.t.Helper()
	var r ledger.Row
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		r, err = ledger.Get(ctx, tx, silo, ledger.Key{Kind: "gce", Class: class, Region: region})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return r.Used / ledger.Micro
}

// want checks silo's used amounts.
func (e *computeEnv) want(what, silo string, used map[string]int64) {
	e.t.Helper()
	for class, n := range used {
		region := central
		if strings.Contains(class, "image") || strings.Contains(class, "snapshot") {
			region = ledger.Global
		}
		if got := e.used(silo, class, region); got != n {
			e.t.Errorf("%s: %s used %d, want %d", what, class, got, n)
		}
	}
}

func vm(name, machineType string, extra obj) obj {
	o := obj{"name": name, "machineType": "zones/" + zoneA + "/machineTypes/" + machineType,
		"networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/web"}},
		"disks":             []obj{{"boot": true, "autoDelete": true, "initializeParams": obj{"sourceImage": debian, "diskType": "zones/" + zoneA + "/diskTypes/pd-balanced"}}}}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

var instancesPath = "zones/" + zoneA + "/instances"

// preview prices a facade request through the fence API's quota:preview.
func (e *computeEnv) preview(caller, method, path string, body any) map[string]int64 {
	e.t.Helper()
	out := e.ok(caller, "POST", "/silo/v1/quota:preview", obj{"facade_request": obj{"method": method, "path": path, "body": body}})
	got := map[string]int64{}
	for _, c := range out["charges"].([]any) {
		m := c.(obj)
		var n int64
		switch a := m["amount"].(type) {
		case float64:
			n = int64(a)
		case string:
			v, _ := ledger.ParseAmount(a)
			n = v / ledger.Micro
		}
		got[m["class"].(string)] += n
	}
	return got
}

func TestInstancePricing(t *testing.T) {
	e := newComputeEnv(t)
	// Every machine type the catalog offers, standard and Spot, prices
	// at its guestCpus and memoryMb, plus one instance, the local SSD it
	// bundles, and the boot disk (10 GB from debian-12, pd-balanced, or
	// hyperdisk-balanced on a series that takes no pd-* disks). Those
	// that bundle GPUs are TestGPUPricing's.
	mts, err := e.gce.ListMachineTypes(ctx, host, zoneA)
	if err != nil {
		t.Fatal(err)
	}
	compat := DefaultCompat()
	for _, mt := range mts {
		if len(mt.Accelerators) > 0 {
			continue
		}
		for _, spot := range []bool{false, true} {
			extra := obj{}
			cpu, mem, lssd := "vcpu", "memory_mb", "local_ssd_gb"
			if spot {
				extra["scheduling"] = obj{"provisioningModel": "SPOT"}
				cpu, mem, lssd = "spot_vcpu", "spot_memory_mb", "spot_local_ssd_gb"
			}
			want := map[string]int64{cpu: mt.GuestCpus, mem: mt.MemoryMb, "instances": 1, "ssd_disk_gb": 10}
			if !compat.attaches("pd-balanced", mt) {
				extra["disks"] = []obj{{"boot": true, "initializeParams": obj{"sourceImage": debian, "diskType": "hyperdisk-balanced"}}}
				want = map[string]int64{cpu: mt.GuestCpus, mem: mt.MemoryMb, "instances": 1,
					"hyperdisk_balanced_gb": 10, "hyperdisk_balanced_iops": 3060, "hyperdisk_balanced_throughput": 146}
			}
			if mt.BundledLocalSsds != nil {
				want[lssd] = 375 * mt.BundledLocalSsds.PartitionCount
			}
			got := e.preview(e.a, "POST", cv(host, instancesPath), vm("x", mt.Name, extra))
			if len(got) != len(want) {
				t.Errorf("%s spot=%v: %v, want %v", mt.Name, spot, got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s spot=%v: %s %d, want %d", mt.Name, spot, k, got[k], v)
				}
			}
		}
	}
	// preemptible is Spot too.
	if got := e.preview(e.a, "POST", cv(host, instancesPath), vm("x", "e2-standard-4", obj{"scheduling": obj{"preemptible": true}})); got["spot_vcpu"] != 4 || got["vcpu"] != 0 {
		t.Errorf("preemptible: %v", got)
	}
	// Each disk type charges its class; one the price table doesn't
	// have isn't allowed.
	for typ, class := range map[string]string{"pd-standard": "standard_disk_gb", "pd-balanced": "ssd_disk_gb", "pd-ssd": "ssd_disk_gb"} {
		body := obj{"name": "d", "sizeGb": "25", "type": "zones/" + zoneA + "/diskTypes/" + typ}
		if got := e.preview(e.a, "POST", cv(host, "zones/"+zoneA+"/disks"), body); len(got) != 1 || got[class] != 25 {
			t.Errorf("%s: %v, want %s=25", typ, got, class)
		}
		vmBody := vm("x", "e2-medium", obj{"disks": []obj{{"boot": true, "initializeParams": obj{"sourceImage": debian, "diskSizeGb": "20", "diskType": typ}}}})
		if got := e.preview(e.a, "POST", cv(host, instancesPath), vmBody); got[class] != 20 {
			t.Errorf("boot %s: %v", typ, got)
		}
	}
	// A blank disk with no size gets the type's default.
	if got := e.preview(e.a, "POST", cv(host, "zones/"+zoneA+"/disks"), obj{"name": "d"}); got["standard_disk_gb"] == 0 {
		t.Errorf("default size: %v", got)
	}
	e.fail(403, "forbidden", e.a, "POST", cv(host, "zones/"+zoneA+"/disks"), obj{"name": "d", "sizeGb": "10", "type": "local-ssd"})

	// The insert charges what the preview said, in the same terms.
	op := e.ok(e.a, "POST", cv(host, instancesPath), vm("vm1", "n2-standard-2", obj{
		"tags": obj{"items": []string{"web"}}, "labels": obj{"env": "dev"},
		"metadata": obj{"items": []obj{{"key": "startup-script", "value": "echo"}}}}))
	if op["targetLink"] != DefaultEndpoint+"projects/silo-"+e.a+"/zones/"+zoneA+"/instances/vm1" {
		t.Errorf("operation: %v", op)
	}
	e.want("after insert", e.a, map[string]int64{"vcpu": 2, "memory_mb": 8192, "instances": 1, "ssd_disk_gb": 10})
	e.run()

	// Step 7: the real instance.
	real, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-vm1")
	if err != nil {
		t.Fatal(err)
	}
	if real.Labels["silo"] != e.a || real.Labels["silo-managed"] != "true" || real.Labels["env"] != "dev" {
		t.Errorf("labels: %v", real.Labels)
	}
	if strings.Join(real.Tags.Items, ",") != e.a+","+e.a+"-web" {
		t.Errorf("tags: %v", real.Tags.Items)
	}
	md := map[string]string{}
	for _, it := range real.Metadata.Items {
		md[it.Key] = *it.Value
	}
	if md["block-project-ssh-keys"] != "true" || md["startup-script"] != "echo" {
		t.Errorf("metadata: %v", md)
	}
	if len(real.ServiceAccounts) != 0 || real.ReservationAffinity == nil || real.ReservationAffinity.ConsumeReservationType != "NO_RESERVATION" {
		t.Errorf("service accounts %v, reservation %v", real.ServiceAccounts, real.ReservationAffinity)
	}
	if d := real.Disks[0]; !strings.HasSuffix(d.Source, "/disks/"+e.a+"-vm1") {
		t.Errorf("boot disk: %v", d.Source)
	}
	// The tenant sees none of it.
	got := e.ok(e.a, "GET", cv(host, instancesPath+"/vm1"), nil)
	if l := got["labels"].(obj); len(l) != 1 || l["env"] != "dev" {
		t.Errorf("labels shown: %v", l)
	}
	if tags := strs(got["tags"].(obj)["items"]); len(tags) != 1 || tags[0] != "web" {
		t.Errorf("tags shown: %v", tags)
	}
	if _, ok := got["serviceAccounts"]; ok {
		t.Errorf("serviceAccounts shown: %v", got)
	}
	nic := got["networkInterfaces"].([]any)[0].(obj)
	if nic["subnetwork"] != DefaultEndpoint+"projects/silo-"+e.a+"/regions/us-central1/subnetworks/web" || !strings.HasSuffix(nic["network"].(string), "/networks/default") {
		t.Errorf("nic: %v", nic)
	}
	if l := items(e.ok(e.a, "GET", cv(host, instancesPath), nil)); len(l) != 1 || l[0]["name"] != "vm1" {
		t.Errorf("list: %v", l)
	}
	if l := items(e.ok(e.b, "GET", cv(host, instancesPath), nil)); len(l) != 0 {
		t.Errorf("team-b lists %v", l)
	}
	e.fail(404, "notFound", e.b, "GET", cv(host, instancesPath+"/vm1"), nil)
	agg := e.ok(e.a, "GET", cv(host, "aggregated/instances"), nil)
	if g := agg["items"].(obj)["zones/"+zoneA].(obj)["instances"].([]any); len(g) != 1 {
		t.Errorf("aggregatedList: %v", agg)
	}

	// Quota: team-a has 16 vCPUs, 14 left.
	msg := e.fail(403, "quotaExceeded", e.a, "POST", cv(host, instancesPath), vm("big", "n2-standard-64", nil))
	if !strings.Contains(msg, "vcpu") || !strings.Contains(msg, "/team-a") {
		t.Errorf("quota message: %q", msg)
	}
	e.want("after a refusal", e.a, map[string]int64{"vcpu": 2, "instances": 1, "ssd_disk_gb": 10})

	// setTags, setLabels and setMetadata are rewritten like insert.
	fp := got["tags"].(obj)["fingerprint"]
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm1/setTags"), obj{"items": []string{"db"}, "fingerprint": fp})
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm1/setLabels"), obj{"labels": obj{"env": "prod"}, "labelFingerprint": got["labelFingerprint"]})
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm1/setMetadata"), obj{"items": []obj{{"key": "a", "value": "b"}}, "fingerprint": got["metadata"].(obj)["fingerprint"]})
	e.run()
	real, _ = e.gce.GetInstance(ctx, host, zoneA, e.a+"-vm1")
	md = map[string]string{}
	for _, it := range real.Metadata.Items {
		md[it.Key] = *it.Value
	}
	if strings.Join(real.Tags.Items, ",") != e.a+","+e.a+"-db" || real.Labels["silo"] != e.a || real.Labels["env"] != "prod" || md["block-project-ssh-keys"] != "true" || md["a"] != "b" {
		t.Errorf("after the setters: tags %v labels %v metadata %v", real.Tags.Items, real.Labels, md)
	}
	e.fail(403, "forbidden", e.a, "POST", cv(host, instancesPath+"/vm1/setTags"), obj{"items": []string{"silo-x"}})
	e.fail(403, "forbidden", e.a, "POST", cv(host, instancesPath+"/vm1/setLabels"), obj{"labels": obj{"silo": "x"}})
	e.fail(403, "forbidden", e.a, "POST", cv(host, instancesPath+"/vm1/setMetadata"), obj{"items": []obj{{"key": "enable-oslogin", "value": "TRUE"}}})

	// Delete gives back the instance and the boot disk it takes with it.
	e.ok(e.a, "DELETE", cv(host, instancesPath+"/vm1"), nil)
	e.want("before the delete finishes", e.a, map[string]int64{"vcpu": 2, "instances": 1, "ssd_disk_gb": 10})
	e.run()
	e.want("after delete", e.a, map[string]int64{"vcpu": 0, "memory_mb": 0, "instances": 0, "ssd_disk_gb": 0})
}

func TestInstanceClock(t *testing.T) {
	e := newComputeEnv(t)
	e.ok(e.a, "POST", cv(host, instancesPath), vm("vm", "e2-standard-4", nil))
	e.run()
	e.want("running", e.a, map[string]int64{"vcpu": 4, "memory_mb": 16384, "instances": 1, "ssd_disk_gb": 10})

	// A stopped instance holds no vCPU or memory.
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/stop"), nil)
	e.want("stopping", e.a, map[string]int64{"vcpu": 4})
	e.run()
	e.want("stopped", e.a, map[string]int64{"vcpu": 0, "memory_mb": 0, "instances": 1, "ssd_disk_gb": 10})
	// Stopping again gives back nothing more.
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/stop"), nil)
	e.run()
	e.want("stopped twice", e.a, map[string]int64{"vcpu": 0, "instances": 1})

	// setMachineType re-prices: stopped, it holds no vCPU at either size;
	// start then charges the new one, and is checked like insert.
	if got := e.preview(e.a, "POST", cv(host, instancesPath+"/vm/setMachineType"), obj{"machineType": "zones/" + zoneA + "/machineTypes/e2-standard-8"}); len(got) != 0 {
		t.Errorf("preview of setMachineType while stopped: %v", got)
	}
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/setMachineType"), obj{"machineType": "zones/" + zoneA + "/machineTypes/n1-standard-16"})
	e.run()
	if got := e.preview(e.a, "POST", cv(host, instancesPath+"/vm/start"), nil); got["vcpu"] != 16 || got["memory_mb"] != 61440 || got["instances"] != 0 {
		t.Errorf("preview of start: %v", got)
	}
	// Somebody else takes a vCPU: 16 no longer fit.
	e.ok(e.a, "POST", cv(host, instancesPath), vm("small", "n1-standard-1", obj{"disks": []obj{{"boot": true, "initializeParams": obj{"sourceImage": debian, "diskType": "pd-standard"}}}}))
	e.run()
	e.fail(403, "quotaExceeded", e.a, "POST", cv(host, instancesPath+"/vm/start"), nil)
	e.ok(e.a, "DELETE", cv(host, instancesPath+"/small"), nil)
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/setMachineType"), obj{"machineType": "e2-standard-8"})
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/start"), nil)
	e.want("starting", e.a, map[string]int64{"vcpu": 8, "memory_mb": 32768, "instances": 1})
	e.run()
	e.want("started", e.a, map[string]int64{"vcpu": 8, "memory_mb": 32768, "instances": 1, "standard_disk_gb": 0})

	// A suspended instance keeps its charges.
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/suspend"), nil)
	e.run()
	e.want("suspended", e.a, map[string]int64{"vcpu": 8, "memory_mb": 32768})
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/resume"), nil)
	e.run()
	e.want("resumed", e.a, map[string]int64{"vcpu": 8, "memory_mb": 32768})

	// A start whose operation fails (an N2_CPUS quota silod didn't
	// predict) is refunded.
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/stop"), nil)
	e.run()
	e.gce.FailOperation(fake.Failure{Method: "instances.start", Kind: fake.FailQuota})
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/start"), nil)
	e.want("starting again", e.a, map[string]int64{"vcpu": 8})
	e.run()
	e.want("start failed", e.a, map[string]int64{"vcpu": 0, "memory_mb": 0, "instances": 1})
	if n := len(e.audit(e.a, KindFacadeRefund)); n != 1 {
		t.Errorf("%d refund records", n)
	}
}

func TestDisks(t *testing.T) {
	e := newComputeEnv(t)
	disks := "zones/" + zoneA + "/disks"
	e.ok(e.a, "POST", cv(host, disks), obj{"name": "data", "sizeGb": "10", "type": "pd-standard", "labels": obj{"k": "v"}})
	e.want("insert", e.a, map[string]int64{"standard_disk_gb": 10})
	e.fail(409, "alreadyExists", e.a, "POST", cv(host, disks), obj{"name": "data", "sizeGb": "10"})
	e.want("a second insert of the name", e.a, map[string]int64{"standard_disk_gb": 10})
	e.run()
	d := e.ok(e.a, "GET", cv(host, disks+"/data"), nil)
	if d["name"] != "data" || len(d["labels"].(obj)) != 1 {
		t.Errorf("get: %v", d)
	}
	if l := items(e.ok(e.a, "GET", cv(host, disks), nil)); len(l) != 1 {
		t.Errorf("list: %v", l)
	}
	e.fail(404, "notFound", e.b, "GET", cv(host, disks+"/data"), nil)

	// Resize charges the difference; a failed resize is refunded.
	if got := e.preview(e.a, "POST", cv(host, disks+"/data/resize"), obj{"sizeGb": "30"}); got["standard_disk_gb"] != 20 {
		t.Errorf("preview of resize: %v", got)
	}
	e.ok(e.a, "POST", cv(host, disks+"/data/resize"), obj{"sizeGb": "30"})
	e.want("resizing", e.a, map[string]int64{"standard_disk_gb": 30})
	e.run()
	e.gce.FailOperation(fake.Failure{Method: "disks.resize", Kind: fake.FailInternal})
	e.ok(e.a, "POST", cv(host, disks+"/data/resize"), obj{"sizeGb": "50"})
	e.want("resizing again", e.a, map[string]int64{"standard_disk_gb": 50})
	e.run()
	e.want("resize failed", e.a, map[string]int64{"standard_disk_gb": 30})
	// GCE refuses a disk getting smaller; nothing is charged.
	e.fail(400, "invalid", e.a, "POST", cv(host, disks+"/data/resize"), obj{"sizeGb": "5"})
	e.want("shrink refused", e.a, map[string]int64{"standard_disk_gb": 30})

	// update changes provisioned performance, which pd-standard has
	// none of, and nothing else; setLabels keeps the facade's.
	e.fail(400, "invalid", e.a, "PATCH", cv(host, disks+"/data?paths=provisionedIops"), obj{"provisionedIops": "5000"})
	e.fail(403, "forbidden", e.a, "PATCH", cv(host, disks+"/data?paths=sizeGb"), obj{"name": "data"})
	e.ok(e.a, "POST", cv(host, disks+"/data/setLabels"), obj{"labels": obj{"k": "w"}, "labelFingerprint": d["labelFingerprint"]})
	e.run()
	if real, _ := e.gce.GetDisk(ctx, host, zoneA, e.a+"-data"); real.Labels["silo"] != e.a || real.Labels["k"] != "w" {
		t.Errorf("labels: %v", real.Labels)
	}

	// Attach it to an instance, and a new disk too.
	e.ok(e.a, "POST", cv(host, instancesPath), vm("vm", "e2-medium", nil))
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/attachDisk"), obj{"source": "zones/" + zoneA + "/disks/data"})
	// A new disk attached is priced like one made with the instance
	// (the fake doesn't make one on attachDisk, so only its preview).
	if got := e.preview(e.a, "POST", cv(host, instancesPath+"/vm/attachDisk"), obj{"initializeParams": obj{"diskSizeGb": "7", "diskType": "pd-ssd"}, "autoDelete": true}); len(got) != 1 || got["ssd_disk_gb"] != 7 {
		t.Errorf("preview of attachDisk: %v", got)
	}
	e.want("attached", e.a, map[string]int64{"ssd_disk_gb": 10, "standard_disk_gb": 30})
	e.run()
	e.ok(e.a, "POST", cv(host, instancesPath+"/vm/detachDisk?deviceName="+e.a+"-data"), nil)
	e.run()

	// Snapshots and images, charged their source's size until GCE says.
	e.ok(e.a, "POST", cv(host, disks+"/data/createSnapshot"), obj{"name": "snap"})
	e.want("snapshotting", e.a, map[string]int64{"snapshots": 1, "snapshot_storage_gb": 30})
	e.run()
	sn := e.ok(e.a, "GET", cv(host, "global/snapshots/snap"), nil)
	if sn["sourceDisk"] != DefaultEndpoint+"projects/silo-"+e.a+"/zones/"+zoneA+"/disks/data" {
		t.Errorf("snapshot: %v", sn)
	}
	e.want("snapshot", e.a, map[string]int64{"snapshots": 1})
	if got := e.used(e.a, "snapshot_storage_gb", ledger.Global); got > 30 || got == 0 {
		t.Errorf("snapshot storage %d", got)
	}
	e.ok(e.a, "POST", cv(host, "global/images"), obj{"name": "img", "sourceDisk": "zones/" + zoneA + "/disks/data", "family": "mine"})
	e.ok(e.a, "POST", cv(host, "global/images"), obj{"name": "deb", "sourceImage": debian, "storageLocations": []string{"us"}})
	e.want("imaging", e.a, map[string]int64{"images": 2})
	e.run()
	e.ok(e.a, "GET", cv(host, "global/images/family/mine"), nil)
	if l := names(items(e.ok(e.a, "GET", cv(host, "global/images"), nil))); strings.Join(l, ",") != "deb,img" {
		t.Errorf("images: %v", l)
	}
	e.ok(e.a, "POST", cv(host, "global/images/img/deprecate"), obj{"state": "DEPRECATED", "replacement": "global/images/deb"})
	deb := e.ok(e.a, "GET", cv(host, "global/images/deb"), nil)
	e.ok(e.a, "POST", cv(host, "global/images/deb/setLabels"), obj{"labels": obj{"x": "y"}, "labelFingerprint": deb["labelFingerprint"]})
	// A disk from the silo's own image and snapshot.
	e.ok(e.a, "POST", cv(host, disks), obj{"name": "from-img", "sourceImage": "global/images/deb"})
	e.ok(e.a, "POST", cv(host, disks+"?sourceImage=projects/silo-"+e.a+"/global/images/deb"), obj{"name": "from-img2"})
	e.ok(e.a, "POST", cv(host, disks), obj{"name": "from-snap", "sourceSnapshot": "global/snapshots/snap", "type": "pd-ssd"})
	e.run()

	// Storage locations: an allowed region, or the multi-region with
	// one in it.
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, "global/images"), obj{"name": "eu", "sourceImage": debian, "storageLocations": []string{"eu"}})
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, "global/images"), obj{"name": "e1", "sourceImage": debian, "storageLocations": []string{"us-east1"}})
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, disks+"/data/createSnapshot"), obj{"name": "s2", "storageLocations": []string{"asia"}})

	// Failures are refunded: an image, a snapshot, a disk.
	before := map[string]int64{"images": e.used(e.a, "images", ledger.Global), "snapshots": e.used(e.a, "snapshots", ledger.Global),
		"standard_disk_gb": e.used(e.a, "standard_disk_gb", central)}
	e.gce.FailOperation(fake.Failure{Method: "images.insert", Kind: fake.FailQuota})
	e.gce.FailOperation(fake.Failure{Method: "disks.createSnapshot", Kind: fake.FailInternal})
	e.gce.FailOperation(fake.Failure{Method: "disks.insert", Kind: fake.FailStockout})
	e.ok(e.a, "POST", cv(host, "global/images"), obj{"name": "img2", "sourceSnapshot": "global/snapshots/snap"})
	e.ok(e.a, "POST", cv(host, disks+"/data/createSnapshot"), obj{"name": "snap2", "storageLocations": []string{central}})
	e.ok(e.a, "POST", cv(host, disks), obj{"name": "d2", "sizeGb": "40", "type": "pd-standard"})
	e.run()
	e.want("after failures", e.a, before)

	// Deletes give back once they work.
	e.ok(e.a, "DELETE", cv(host, "global/snapshots/snap"), nil)
	e.ok(e.a, "DELETE", cv(host, "global/images/img"), nil)
	e.ok(e.a, "DELETE", cv(host, disks+"/from-snap"), nil)
	e.run()
	e.want("after deletes", e.a, map[string]int64{"snapshots": 0, "snapshot_storage_gb": 0, "images": 1})
}

func TestRefusedFields(t *testing.T) {
	e := newComputeEnv(t)
	nic := []obj{{"subnetwork": "regions/us-central1/subnetworks/web"}}
	for _, c := range []struct {
		extra obj
		names string
	}{
		{obj{"canIpForward": true}, "canIpForward"},
		{obj{"serviceAccounts": []obj{{"email": "default", "scopes": []string{"cloud-platform"}}}}, "serviceAccounts"},
		{obj{"metadata": obj{"items": []obj{{"key": "enable-oslogin", "value": "TRUE"}}}}, "enable-oslogin"},
		{obj{"metadata": obj{"items": []obj{{"key": "enable-oslogin-2fa", "value": "TRUE"}}}}, "enable-oslogin-2fa"},
		{obj{"metadata": obj{"items": []obj{{"key": "enable-windows-ssh", "value": "TRUE"}}}}, "enable-windows-ssh"},
		{obj{"metadata": obj{"items": []obj{{"key": "block-project-ssh-keys", "value": "false"}}}}, "block-project-ssh-keys"},
		{obj{"reservationAffinity": obj{"consumeReservationType": "ANY_RESERVATION"}}, "reservationAffinity"},
		{obj{"reservationAffinity": obj{"consumeReservationType": "SPECIFIC_RESERVATION", "key": "compute.googleapis.com/reservation-name"}}, "reservationAffinity"},
		{obj{"scheduling": obj{"nodeAffinities": []obj{{"key": "compute.googleapis.com/node-group-name", "operator": "IN", "values": []string{"g"}}}}}, "scheduling.nodeAffinities"},
		{obj{"networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/web", "accessConfigs": []obj{{"type": "ONE_TO_ONE_NAT"}}}}}, "networkInterfaces[0].accessConfigs"},
		{obj{"networkInterfaces": []obj{{"subnetwork": "regions/us-central1/subnetworks/web", "ipv6AccessConfigs": []obj{{"type": "DIRECT_IPV6"}}}}}, "networkInterfaces[0].ipv6AccessConfigs"},
		{obj{"disks": []obj{{"boot": true, "initializeParams": obj{"sourceImage": debian, "storagePool": "p"}}}}, "disks[0].initializeParams.storagePool"},
		{obj{"guestAccelerators": []obj{{"acceleratorType": "nvidia-tesla-t4", "acceleratorCount": 1, "x": 1}}}, "guestAccelerators[0].x"},
		{obj{"confidentialInstanceConfig": obj{"enableConfidentialCompute": true}}, "confidentialInstanceConfig"},
		{obj{"labels": obj{"silo-x": "y"}}, "silo-x"},
		{obj{"tags": obj{"items": []string{"silo"}}}, "tag silo"},
	} {
		body := vm("x", "e2-medium", obj{"networkInterfaces": nic})
		for k, v := range c.extra {
			body[k] = v
		}
		msg := e.fail(403, "forbidden", e.a, "POST", cv(host, instancesPath), body)
		if !strings.Contains(msg, c.names) {
			t.Errorf("%v: %q doesn't name %s", c.extra, msg, c.names)
		}
	}
	// Defaults pass: canIpForward false, no service accounts, metadata
	// block-project-ssh-keys true, NO_RESERVATION.
	e.ok(e.a, "POST", cv(host, instancesPath), vm("ok", "e2-medium", obj{"canIpForward": false, "serviceAccounts": []obj{},
		"metadata":            obj{"items": []obj{{"key": "block-project-ssh-keys", "value": "TRUE"}}},
		"reservationAffinity": obj{"consumeReservationType": "NO_RESERVATION"}}))
	e.want("only the allowed insert charged", e.a, map[string]int64{"vcpu": 2, "instances": 1})

	disks := "zones/" + zoneA + "/disks"
	for _, c := range []struct {
		path  string
		body  obj
		names string
	}{
		{disks, obj{"name": "d", "storagePool": "projects/p/zones/z/storagePools/x"}, "storagePool"},
		{"global/images", obj{"name": "i", "rawDisk": obj{"source": "gs://b/disk.tar.gz"}}, "rawDisk"},
		{"global/images", obj{"name": "i", "sourceImage": debian, "licenses": []string{"projects/x/global/licenses/y"}}, "licenses"},
		{"global/snapshots", obj{"name": "s"}, "snapshots.insert"},
		{"zones/" + zoneA + "/instances/ok/setServiceAccount", obj{"email": "x"}, "instances.setServiceAccount"},
		{"zones/" + zoneA + "/instances/ok/setScheduling", obj{"nodeAffinities": []obj{}}, "instances.setScheduling"},
	} {
		msg := e.fail(403, "forbidden", e.a, "POST", cv(host, c.path), c.body)
		if !strings.Contains(msg, c.names) {
			t.Errorf("%s %v: %q doesn't name %s", c.path, c.body, msg, c.names)
		}
	}
	// A sourceInstanceTemplate must be one of the silo's templates.
	e.fail(404, "notFound", e.a, "POST", cv(host, instancesPath+"?sourceInstanceTemplate=global/instanceTemplates/t"), vm("t", "e2-medium", nil))
	// The network interface checks (step 3).
	webA := e.webA
	for _, n := range []obj{
		{"subnetwork": "regions/us-central1/subnetworks/web", "networkIP": sub(e.webB, 32, 5).Addr().String()},
		{"subnetwork": "regions/us-central1/subnetworks/web", "aliasIpRanges": []obj{{"ipCidrRange": e.webB.String()}}},
		{"subnetwork": "regions/us-central1/subnetworks/web", "aliasIpRanges": []obj{{"ipCidrRange": sub(webA, 28, 1).String(), "subnetworkRangeName": "nope"}}},
		{"subnetwork": "regions/us-central1/subnetworks/web", "aliasIpRanges": []obj{{"ipCidrRange": "/16"}}},
		{"subnetwork": "regions/us-east1/subnetworks/web"},
	} {
		e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath), vm("x", "e2-medium", obj{"networkInterfaces": []obj{n}}))
	}
	e.fail(404, "notFound", e.a, "POST", cv(host, instancesPath), vm("x", "e2-medium", obj{"networkInterfaces": []obj{{"network": "global/networks/other", "subnetwork": "regions/us-central1/subnetworks/web"}}}))
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, "zones/us-east1-b/instances"), vm("x", "e2-medium", nil))
	e.ok(e.a, "POST", cv(host, instancesPath), vm("alias", "e2-medium", obj{"networkInterfaces": []obj{{
		"subnetwork": "regions/us-central1/subnetworks/web", "networkIP": sub(webA, 32, 9).Addr().String(),
		"aliasIpRanges": []obj{{"ipCidrRange": sub(webA, 28, 2).String()}, {"ipCidrRange": sub(e.pods, 28, 1).String(), "subnetworkRangeName": "pods"}}}}}))
	e.run()
	if inst, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-alias"); err != nil || inst.NetworkInterfaces[0].NetworkIP != sub(webA, 32, 9).Addr().String() {
		t.Errorf("alias instance: %v %v", inst, err)
	}
}

// TestOtherSilosObjects tries to use team-b's disk, subnet, image and
// snapshot from team-a, by every name form: each is 403 (another
// project) or 404 (not team-a's).
func TestOtherSilosObjects(t *testing.T) {
	e := newComputeEnv(t)
	disks := "zones/" + zoneA + "/disks"
	e.ok(e.b, "POST", cv(host, disks), obj{"name": "bd", "sizeGb": "10"})
	e.run()
	e.ok(e.b, "POST", cv(host, disks+"/bd/createSnapshot"), obj{"name": "bs"})
	e.ok(e.b, "POST", cv(host, "global/images"), obj{"name": "bi", "sourceDisk": disks + "/bd"})
	e.run()
	b := e.b
	forms := func(scope, coll, name string) map[string]int {
		path := scope + "/" + coll + "/" + name
		return map[string]int{
			name:                              404, // bare: team-a's own name
			b + "-" + name:                    404, // the real name, as team-a's own
			path:                              404,
			"projects/silo-" + b + "/" + path: 403,
			"projects/" + host + "/" + scope + "/" + coll + "/" + b + "-" + name:                                       404,
			"http://silo.internal:8080/compute/v1/projects/silo-" + b + "/" + path:                                     403,
			"https://www.googleapis.com/compute/v1/projects/silo-" + b + "/" + path:                                    403,
			"https://compute.googleapis.com/compute/v1/projects/silo-" + b + "/" + path:                                403,
			"https://www.googleapis.com/compute/v1/projects/" + host + "/" + scope + "/" + coll + "/" + b + "-" + name: 404,
		}
	}
	reason := map[int]string{403: "forbidden", 404: "notFound"}
	for ref, code := range forms("zones/"+zoneA, "disks", "bd") {
		e.fail(code, reason[code], e.a, "POST", cv(host, instancesPath), vm("x", "e2-medium", obj{"disks": []obj{{"boot": true, "source": ref}}}))
		if strings.Contains(ref, "/") { // an image's sourceDisk names its zone
			e.fail(code, reason[code], e.a, "POST", cv(host, "global/images"), obj{"name": "x", "sourceDisk": ref})
		}
	}
	e.ok(e.a, "POST", cv(host, instancesPath), vm("vm", "e2-medium", nil))
	e.run()
	for ref, code := range forms("zones/"+zoneA, "disks", "bd") {
		e.fail(code, reason[code], e.a, "POST", cv(host, instancesPath+"/vm/attachDisk"), obj{"source": ref})
	}
	for ref, code := range forms("regions/us-central1", "subnetworks", "web") {
		if ref == "web" || ref == "regions/us-central1/subnetworks/web" {
			continue // team-a has a web of its own
		}
		e.fail(code, reason[code], e.a, "POST", cv(host, instancesPath), vm("x", "e2-medium", obj{"networkInterfaces": []obj{{"subnetwork": ref}}}))
	}
	for ref, code := range forms("global", "images", "bi") {
		e.fail(code, reason[code], e.a, "POST", cv(host, instancesPath), vm("x", "e2-medium", obj{"disks": []obj{{"boot": true, "initializeParams": obj{"sourceImage": ref}}}}))
		e.fail(code, reason[code], e.a, "POST", cv(host, disks), obj{"name": "x", "sourceImage": ref})
		e.fail(code, reason[code], e.a, "POST", cv(host, "global/images"), obj{"name": "x", "sourceImage": ref})
	}
	for ref, code := range forms("global", "snapshots", "bs") {
		e.fail(code, reason[code], e.a, "POST", cv(host, disks), obj{"name": "x", "sourceSnapshot": ref})
		e.fail(code, reason[code], e.a, "POST", cv(host, "global/images"), obj{"name": "x", "sourceSnapshot": ref})
	}
	// Nor can team-a touch them directly.
	e.fail(404, "notFound", e.a, "DELETE", cv(host, disks+"/bd"), nil)
	e.fail(404, "notFound", e.a, "DELETE", cv(host, "global/images/bi"), nil)
	e.fail(404, "notFound", e.a, "DELETE", cv(host, "global/snapshots/bs"), nil)
	e.fail(403, "forbidden", e.a, "DELETE", cv("silo-"+b, disks+"/bd"), nil)
	// A disk in another zone isn't attachable either.
	e.ok(e.a, "POST", cv(host, "zones/us-central1-b/disks"), obj{"name": "far", "sizeGb": "10"})
	e.run()
	e.fail(400, "invalid", e.a, "POST", cv(host, instancesPath+"/vm/attachDisk"), obj{"source": "zones/us-central1-b/disks/far"})
	e.want("nothing charged to team-a for them", e.a, map[string]int64{"images": 0, "snapshots": 0, "standard_disk_gb": 10, "instances": 1})
}

func TestInstanceInsertFailureRefund(t *testing.T) {
	e := newComputeEnv(t)
	// GCE refuses with a quota silod didn't predict: everything the
	// insert charged, the boot disk and a data disk among it, comes back.
	e.gce.FailOperation(fake.Failure{Method: "instances.insert", Kind: fake.FailQuota})
	body := vm("vm", "n2-standard-8", obj{"disks": []obj{
		{"boot": true, "initializeParams": obj{"sourceImage": debian, "diskType": "pd-ssd"}},
		{"initializeParams": obj{"diskName": "scratch", "diskSizeGb": "50", "diskType": "pd-standard"}, "autoDelete": true},
	}})
	e.ok(e.a, "POST", cv(host, instancesPath), body)
	e.want("charged", e.a, map[string]int64{"vcpu": 8, "memory_mb": 32768, "instances": 1, "ssd_disk_gb": 10, "standard_disk_gb": 50})
	e.run()
	e.want("refunded", e.a, map[string]int64{"vcpu": 0, "memory_mb": 0, "instances": 0, "ssd_disk_gb": 0, "standard_disk_gb": 0})
	if _, err := e.gce.GetDisk(ctx, host, zoneA, e.a+"-scratch"); err == nil {
		t.Error("the failed insert left a disk")
	}
	// The same body then works; the disk naming is GCE's.
	e.ok(e.a, "POST", cv(host, instancesPath), body)
	e.run()
	e.want("inserted", e.a, map[string]int64{"vcpu": 8, "instances": 1, "ssd_disk_gb": 10, "standard_disk_gb": 50})
	e.ok(e.a, "GET", cv(host, "zones/"+zoneA+"/disks/scratch"), nil)
	// GCE refusing the call itself is passed through, refunded at once.
	e.fail(409, "alreadyExists", e.a, "POST", cv(host, instancesPath), body)
	e.want("after the 409", e.a, map[string]int64{"vcpu": 8, "instances": 1, "ssd_disk_gb": 10, "standard_disk_gb": 50})
	// A delete that fails gives back nothing.
	e.gce.FailOperation(fake.Failure{Method: "instances.delete", Kind: fake.FailInternal})
	e.ok(e.a, "DELETE", cv(host, instancesPath+"/vm"), nil)
	e.run()
	e.want("delete failed", e.a, map[string]int64{"vcpu": 8, "instances": 1, "ssd_disk_gb": 10})
	var _ compute.Instance
}
