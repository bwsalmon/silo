package fake

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TestServer drives the fake through the generated compute/v1 client,
// as the real gce.Client will be.
func TestServer(t *testing.T) {
	f := New(Options{Project: proj})
	srv := NewServer(f)
	defer srv.Close()
	svc, err := compute.NewService(ctx, option.WithEndpoint(srv.ComputeEndpoint()), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	wait := func(op *compute.Operation, err error) *compute.Operation {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var got *compute.Operation
		switch sc := gce.ScopeOf(op); {
		case sc.Zone != "":
			got, err = svc.ZoneOperations.Wait(proj, sc.Zone, op.Name).Do()
		case sc.Region != "":
			got, err = svc.RegionOperations.Wait(proj, sc.Region, op.Name).Do()
		default:
			got, err = svc.GlobalOperations.Wait(proj, op.Name).Do()
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "DONE" || got.Error != nil {
			t.Fatalf("operation %+v", got)
		}
		return got
	}

	wait(svc.Networks.Insert(proj, &compute.Network{Name: "silo", ForceSendFields: []string{"AutoCreateSubnetworks"}}).Do())
	wait(svc.Subnetworks.Insert(proj, region, &compute.Subnetwork{Name: "sub", Network: "global/networks/silo", IpCidrRange: "10.0.0.0/24"}).Do())
	for _, name := range []string{"k7-a", "k7-b", "q2-a"} {
		inst := vm(name, "e2-standard-2")
		inst.Labels = map[string]string{"silo": name[:2]}
		wait(svc.Instances.Insert(proj, zone, inst).Do())
	}
	got, err := svc.Instances.Get(proj, zone, "k7-a").Do()
	if err != nil || got.Status != "RUNNING" || got.Labels["silo"] != "k7" {
		t.Fatalf("get: %+v %v", got, err)
	}

	// Filters go over the wire in ListFilter's syntax.
	list, err := svc.Instances.List(proj, zone).Filter(gce.ListFilter{Labels: map[string]string{"silo": "k7"}}.String()).Do()
	if err != nil || len(list.Items) != 2 {
		t.Fatalf("list by label: %v %v", list, err)
	}
	_, err = svc.Instances.List(proj, zone).Filter(`status = "RUNNING"`).Do()
	wantErr(t, err, http.StatusBadRequest, gce.ReasonInvalid)
	// Paging.
	var names []string
	err = svc.Instances.List(proj, zone).MaxResults(1).Pages(ctx, func(l *compute.InstanceList) error {
		for _, i := range l.Items {
			names = append(names, i.Name)
		}
		return nil
	})
	if err != nil || len(names) != 3 {
		t.Fatalf("paged: %v %v", names, err)
	}

	// Errors come back GCE-shaped, as *googleapi.Error with a reason.
	_, err = svc.Instances.Insert(proj, zone, vm("k7-a", "e2-standard-2")).Do()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusConflict || gce.Reason(err) != gce.ReasonAlreadyExists {
		t.Fatalf("duplicate insert: %v", err)
	}
	_, err = svc.Instances.Get(proj, zone, "nope").Do()
	wantErr(t, err, http.StatusNotFound, gce.ReasonNotFound)

	// A failed operation, through the wire.
	f.SetQuota(region, gce.MetricCPUS, 6)
	op, err := svc.Instances.Insert(proj, zone, vm("over", "e2-standard-2")).Do()
	if err != nil {
		t.Fatal(err)
	}
	op, err = svc.ZoneOperations.Wait(proj, zone, op.Name).Do()
	if err != nil || op.Error == nil || op.Error.Errors[0].Code != gce.CodeQuotaExceeded {
		t.Fatalf("over quota: %+v %v", op, err)
	}

	// Templates, query parameters and the catalog.
	wait(svc.InstanceTemplates.Insert(proj, &compute.InstanceTemplate{Name: "tpl", Properties: &compute.InstanceProperties{
		MachineType: "e2-standard-2", Disks: vm("x", "e2-standard-2").Disks, NetworkInterfaces: vm("x", "e2-standard-2").NetworkInterfaces,
	}}).Do())
	f.ClearQuota(region, gce.MetricCPUS)
	wait(svc.Instances.Insert(proj, zone, &compute.Instance{Name: "from-tpl"}).SourceInstanceTemplate("global/instanceTemplates/tpl").Do())
	wait(svc.Instances.Stop(proj, zone, "from-tpl").DiscardLocalSsd(true).Do())
	wait(svc.Instances.SetMachineType(proj, zone, "from-tpl", &compute.InstancesSetMachineTypeRequest{MachineType: "zones/" + zone + "/machineTypes/e2-standard-4"}).Do())
	wait(svc.Disks.Insert(proj, zone, &compute.Disk{Name: "hdb", Type: "zones/" + zone + "/diskTypes/hyperdisk-balanced", SizeGb: 10}).Do())
	wait(svc.Disks.Update(proj, zone, "hdb", &compute.Disk{ProvisionedIops: 4000}).Paths("provisionedIops").Do())
	if d, err := svc.Disks.Get(proj, zone, "hdb").Do(); err != nil || d.ProvisionedIops != 4000 {
		t.Fatalf("updated disk %+v %v", d, err)
	}
	mt, err := svc.MachineTypes.Get(proj, zone, "a2-highgpu-2g").Do()
	if err != nil || mt.Accelerators[0].GuestAcceleratorCount != 2 {
		t.Fatalf("machine type %+v %v", mt, err)
	}
	f.SetQuota(region, "NVIDIA_T4_GPUS", 8)
	r, err := svc.Regions.Get(proj, region).Do()
	if err != nil || quota(r.Quotas, "NVIDIA_T4_GPUS") == nil {
		t.Fatalf("region %+v %v", r, err)
	}
	img, err := svc.Images.GetFromFamily("debian-cloud", "debian-12").Do()
	if err != nil || img.Name != "debian-12-bookworm-v20260901" {
		t.Fatalf("image %+v %v", img, err)
	}

	// The fence's calls.
	wait(svc.NetworkFirewallPolicies.Insert(proj, &compute.FirewallPolicy{Name: "silo-fence"}).Do())
	wait(svc.NetworkFirewallPolicies.AddRule(proj, "silo-fence", &compute.FirewallPolicyRule{Priority: 1000, Action: "deny", Direction: "EGRESS",
		Match: &compute.FirewallPolicyRuleMatcher{DestIpRanges: []string{"10.255.254.0/24"}, Layer4Configs: []*compute.FirewallPolicyRuleMatcherLayer4Config{{IpProtocol: "all"}}}}).Do())
	rule, err := svc.NetworkFirewallPolicies.GetRule(proj, "silo-fence").Priority(1000).Do()
	if err != nil || rule.Action != "deny" {
		t.Fatalf("rule %+v %v", rule, err)
	}
	wait(svc.NetworkFirewallPolicies.AddAssociation(proj, "silo-fence", &compute.FirewallPolicyAssociation{Name: "silo", AttachmentTarget: "global/networks/silo"}).Do())
	wait(svc.NetworkFirewallPolicies.RemoveRule(proj, "silo-fence").Priority(1000).Do())

	// Cloud Quotas.
	f.SetQuota(region, "NVIDIA_H100_GPUS", 16)
	resp, err := srv.Client().Get(srv.CloudQuotasEndpoint() + "v1/projects/" + proj + "/locations/global/services/compute.googleapis.com/quotaInfos")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		QuotaInfos []*gce.QuotaInfo `json:"quotaInfos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, qi := range body.QuotaInfos {
		if qi.Metric == "compute.googleapis.com/nvidia_h100_gpus" && qi.DimensionsInfos[0].Details.Value == 16 {
			found = true
		}
	}
	if !found {
		t.Fatalf("quotaInfos %+v", body.QuotaInfos)
	}
}
