// Package gcetest is behaviour tests of a gce.Client: the calls silod
// makes, and what it relies on in their answers. The same tests run
// against the in-memory fake, and against the real client (package
// gce/gcp) pointed at the fake's HTTP server, so a difference between
// the two, in the wire format, paging, errors or operations, fails one
// of them.
//
// Tests that need to set GCE's state, a quota limit or an operation
// that fails, use Env.Fake and are skipped when it is nil, as it is
// against real GCE.
package gcetest

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/gce/fake"
	compute "google.golang.org/api/compute/v1"
)

// Env is one client under test.
type Env struct {
	Client  gce.Client
	Project string
	Region  string
	Zone    string // in Region
	// Fake is the fake behind Client, or nil.
	Fake *fake.Fake
}

// NewFake is a fake whose operations finish on their own, quickly, as
// Run's tests expect, with an empty host project.
func NewFake(project string) *fake.Fake {
	return fake.New(fake.Options{Project: project, OperationLatency: 5 * time.Millisecond})
}

// Run runs every test, each against an Env of its own from newEnv.
func Run(t *testing.T, newEnv func(t *testing.T) Env) {
	for _, tc := range []struct {
		name string
		fn   func(t *testing.T, e Env)
	}{
		{"InstanceLifecycle", testInstanceLifecycle},
		{"Errors", testErrors},
		{"Lists", testLists},
		{"FailedOperation", testFailedOperation},
		{"Disks", testDisks},
		{"ImagesAndSnapshots", testImagesAndSnapshots},
		{"InstanceTemplates", testInstanceTemplates},
		{"Firewalls", testFirewalls},
		{"FirewallPolicy", testFirewallPolicy},
		{"Catalog", testCatalog},
		{"Quotas", testQuotas},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			setup(t, ctx, e)
			tc.fn(t, e.with(ctx))
		})
	}
}

// with carries the test's context in e.Client, so the tests need not
// take it.
func (e Env) with(ctx context.Context) Env {
	e.Client = ctxClient{e.Client, ctx}
	return e
}

// ctxClient is a gce.Client whose test context is known to the
// helpers; the methods are the embedded client's.
type ctxClient struct {
	gce.Client
	ctx context.Context
}

func ctxOf(e Env) context.Context { return e.Client.(ctxClient).ctx }

// setup makes the network silo and the subnetwork sub (10.0.0.0/24)
// in e.Region.
func setup(t *testing.T, ctx context.Context, e Env) {
	t.Helper()
	await(t, ctx, e, ok)(e.Client.InsertNetwork(ctx, e.Project, &compute.Network{Name: "silo",
		NetworkFirewallPolicyEnforcementOrder: "BEFORE_CLASSIC_FIREWALL", ForceSendFields: []string{"AutoCreateSubnetworks"}}))
	await(t, ctx, e, ok)(e.Client.InsertSubnetwork(ctx, e.Project, e.Region, &compute.Subnetwork{Name: "sub",
		Network: "global/networks/silo", IpCidrRange: "10.0.0.0/24"}))
}

const ok = ""

// await finishes the operation a call started with gce.Await and
// checks that it failed with code, or succeeded when code is ok.
func await(t *testing.T, ctx context.Context, e Env, code string) func(*compute.Operation, error) *compute.Operation {
	t.Helper()
	return func(op *compute.Operation, err error) *compute.Operation {
		t.Helper()
		if err != nil {
			t.Fatalf("call failed: %v", err)
		}
		got, err := gce.Await(ctx, e.Client, e.Project, op)
		if err != nil {
			t.Fatalf("await %s: %v", op.Name, err)
		}
		if got.Status != "DONE" || got.Name != op.Name {
			t.Fatalf("awaited %+v", got)
		}
		gotCode := ""
		if got.Error != nil && len(got.Error.Errors) > 0 {
			gotCode = got.Error.Errors[0].Code
		}
		if gotCode != code {
			t.Fatalf("operation %s (%s) ended with %q, want %q: %+v", op.Name, op.OperationType, gotCode, code, got.Error)
		}
		return got
	}
}

func do(t *testing.T, e Env) func(*compute.Operation, error) *compute.Operation {
	t.Helper()
	return await(t, ctxOf(e), e, ok)
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

func vm(e Env, name, machineType string) *compute.Instance {
	return &compute.Instance{
		Name:        name,
		MachineType: "zones/" + e.Zone + "/machineTypes/" + machineType,
		Disks: []*compute.AttachedDisk{{Boot: true, AutoDelete: true, InitializeParams: &compute.AttachedDiskInitializeParams{
			SourceImage: "projects/debian-cloud/global/images/family/debian-12",
		}}},
		NetworkInterfaces: []*compute.NetworkInterface{{Network: "global/networks/silo",
			Subnetwork: "regions/" + e.Region + "/subnetworks/sub"}},
	}
}

func testInstanceLifecycle(t *testing.T, e Env) {
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	inst := vm(e, "vm-1", "e2-standard-2")
	inst.Labels = map[string]string{"silo": "k7"}
	op := do(t, e)(c.InsertInstance(ctx, p, z, inst, ""))
	if op.OperationType != "insert" || !strings.HasSuffix(op.TargetLink, "/zones/"+z+"/instances/vm-1") {
		t.Fatalf("insert op %+v", op)
	}
	got, err := c.GetInstance(ctx, p, z, "vm-1")
	if err != nil || got.Status != "RUNNING" || got.Labels["silo"] != "k7" || gce.LastSegment(got.MachineType) != "e2-standard-2" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.NetworkInterfaces[0].NetworkIP == "" {
		t.Fatal("no address")
	}
	ops, err := c.ListOperations(ctx, p, gce.Zonal(z))
	if err != nil || len(ops) == 0 {
		t.Fatalf("operations: %d %v", len(ops), err)
	}
	if o, err := c.GetOperation(ctx, p, gce.ScopeOf(op), op.Name); err != nil || o.Status != "DONE" {
		t.Fatalf("get operation %+v %v", o, err)
	}

	do(t, e)(c.StopInstance(ctx, p, z, "vm-1", true))
	if got, _ := c.GetInstance(ctx, p, z, "vm-1"); got.Status != "TERMINATED" {
		t.Fatalf("after stop: %s", got.Status)
	}
	do(t, e)(c.SetMachineType(ctx, p, z, "vm-1", "zones/"+z+"/machineTypes/e2-standard-4"))
	do(t, e)(c.StartInstance(ctx, p, z, "vm-1"))
	got, _ = c.GetInstance(ctx, p, z, "vm-1")
	if got.Status != "RUNNING" || gce.LastSegment(got.MachineType) != "e2-standard-4" {
		t.Fatalf("after start: %s %s", got.Status, got.MachineType)
	}
	do(t, e)(c.SetInstanceLabels(ctx, p, z, "vm-1", &compute.InstancesSetLabelsRequest{
		Labels: map[string]string{"silo": "k7", "x": "y"}, LabelFingerprint: got.LabelFingerprint}))
	do(t, e)(c.SetInstanceTags(ctx, p, z, "vm-1", &compute.Tags{Items: []string{"web"}, Fingerprint: got.Tags.Fingerprint}))
	md := got.Metadata
	if md == nil {
		md = &compute.Metadata{}
	}
	md.Items = append(md.Items, &compute.MetadataItems{Key: "k", Value: ptr("v")})
	do(t, e)(c.SetInstanceMetadata(ctx, p, z, "vm-1", md))
	got, _ = c.GetInstance(ctx, p, z, "vm-1")
	if got.Labels["x"] != "y" || len(got.Tags.Items) != 1 || len(got.Metadata.Items) == 0 {
		t.Fatalf("after set: labels %v tags %v metadata %v", got.Labels, got.Tags, got.Metadata)
	}
	do(t, e)(c.ResetInstance(ctx, p, z, "vm-1"))
	do(t, e)(c.SuspendInstance(ctx, p, z, "vm-1"))
	do(t, e)(c.ResumeInstance(ctx, p, z, "vm-1"))
	if _, err := c.GetSerialPortOutput(ctx, p, z, "vm-1", 1, 0); err != nil {
		t.Fatalf("serial port: %v", err)
	}

	// Deletion protection, both ways: false has to reach GCE too.
	do(t, e)(c.SetDeletionProtection(ctx, p, z, "vm-1", true))
	if _, err := c.DeleteInstance(ctx, p, z, "vm-1"); err == nil {
		t.Fatal("deleted a protected instance")
	}
	do(t, e)(c.SetDeletionProtection(ctx, p, z, "vm-1", false))
	do(t, e)(c.DeleteInstance(ctx, p, z, "vm-1"))
	_, err = c.GetInstance(ctx, p, z, "vm-1")
	wantErr(t, err, http.StatusNotFound, gce.ReasonNotFound)
}

func ptr(s string) *string { return &s }

func testErrors(t *testing.T, e Env) {
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	do(t, e)(c.InsertInstance(ctx, p, z, vm(e, "dup", "e2-standard-2"), ""))
	_, err := c.InsertInstance(ctx, p, z, vm(e, "dup", "e2-standard-2"), "")
	wantErr(t, err, http.StatusConflict, gce.ReasonAlreadyExists)
	if !gce.IsAlreadyExists(err) || !strings.Contains(err.Error(), "dup") {
		t.Fatalf("duplicate: %v", err)
	}
	_, err = c.GetDisk(ctx, p, z, "nope")
	wantErr(t, err, http.StatusNotFound, gce.ReasonNotFound)
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("not found doesn't name the resource: %v", err)
	}
	_, err = c.InsertDisk(ctx, p, z, &compute.Disk{Name: "Bad_Name", SizeGb: 10})
	wantErr(t, err, http.StatusBadRequest, gce.ReasonInvalid)
	_, err = c.GetNetwork(ctx, p, "nope")
	wantErr(t, err, http.StatusNotFound, gce.ReasonNotFound)
	_, err = c.GetOperation(ctx, p, gce.Global, "operation-nope")
	wantErr(t, err, http.StatusNotFound, "")
}

func names[T any](items []*T, name func(*T) string) []string {
	var out []string
	for _, it := range items {
		out = append(out, name(it))
	}
	sort.Strings(out)
	return out
}

func testLists(t *testing.T, e Env) {
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	for _, n := range []string{"k7-a", "k7-b", "q2-a", "q2-b", "q2-c"} {
		inst := vm(e, n, "e2-medium")
		inst.Labels = map[string]string{"silo": n[:2]}
		do(t, e)(c.InsertInstance(ctx, p, z, inst, ""))
	}
	iname := func(i *compute.Instance) string { return i.Name }
	all, err := c.ListInstances(ctx, p, z, gce.ListFilter{})
	if err != nil || strings.Join(names(all, iname), ",") != "k7-a,k7-b,q2-a,q2-b,q2-c" {
		t.Fatalf("all: %v %v", names(all, iname), err)
	}
	byLabel, err := c.ListInstances(ctx, p, z, gce.ListFilter{Labels: map[string]string{"silo": "q2"}})
	if err != nil || strings.Join(names(byLabel, iname), ",") != "q2-a,q2-b,q2-c" {
		t.Fatalf("by label: %v %v", names(byLabel, iname), err)
	}
	byPrefix, err := c.ListInstances(ctx, p, z, gce.ListFilter{NamePrefix: "k7-"})
	if err != nil || strings.Join(names(byPrefix, iname), ",") != "k7-a,k7-b" {
		t.Fatalf("by prefix: %v %v", names(byPrefix, iname), err)
	}
	// A filter's metacharacters are matched literally.
	none, err := c.ListInstances(ctx, p, z, gce.ListFilter{NamePrefix: "k7."})
	if err != nil || len(none) != 0 {
		t.Fatalf("literal prefix: %v %v", names(none, iname), err)
	}
	disks, err := c.ListDisks(ctx, p, z, gce.ListFilter{NamePrefix: "q2-"})
	if err != nil || len(disks) != 3 {
		t.Fatalf("disks: %d %v", len(disks), err)
	}
	empty, err := c.ListInstances(ctx, p, "us-east1-b", gce.ListFilter{})
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty zone: %d %v", len(empty), err)
	}
	nets, err := c.ListNetworks(ctx, p, gce.ListFilter{})
	if err != nil || len(nets) != 1 {
		t.Fatalf("networks: %d %v", len(nets), err)
	}
	subs, err := c.ListSubnetworks(ctx, p, e.Region, gce.ListFilter{NamePrefix: "sub"})
	if err != nil || len(subs) != 1 {
		t.Fatalf("subnetworks: %d %v", len(subs), err)
	}
}

func testFailedOperation(t *testing.T, e Env) {
	if e.Fake == nil {
		t.Skip("needs the fake to make an operation fail")
	}
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	e.Fake.SetQuota(e.Region, gce.MetricCPUS, 2)
	do(t, e)(c.InsertInstance(ctx, p, z, vm(e, "fits", "e2-standard-2"), ""))
	op := await(t, ctx, e, gce.CodeQuotaExceeded)(c.InsertInstance(ctx, p, z, vm(e, "over", "e2-standard-2"), ""))
	if op.HttpErrorStatusCode != http.StatusForbidden || !strings.Contains(op.Error.Errors[0].Message, "CPUS") {
		t.Fatalf("over quota: %d %+v", op.HttpErrorStatusCode, op.Error.Errors[0])
	}
	_, err := c.GetInstance(ctx, p, z, "over")
	wantErr(t, err, http.StatusNotFound, gce.ReasonNotFound)

	e.Fake.FailOperation(fake.Failure{Method: "instances.insert", Name: "unlucky", Kind: fake.FailStockout})
	e.Fake.ClearQuota(e.Region, gce.MetricCPUS)
	await(t, ctx, e, gce.CodeStockout)(c.InsertInstance(ctx, p, z, vm(e, "unlucky", "e2-standard-2"), ""))
}

func testDisks(t *testing.T, e Env) {
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	do(t, e)(c.InsertDisk(ctx, p, z, &compute.Disk{Name: "d", SizeGb: 10, Labels: map[string]string{"silo": "k7"}}))
	do(t, e)(c.ResizeDisk(ctx, p, z, "d", 20))
	d, err := c.GetDisk(ctx, p, z, "d")
	if err != nil || d.SizeGb != 20 {
		t.Fatalf("resized %+v %v", d, err)
	}
	do(t, e)(c.SetDiskLabels(ctx, p, z, "d", &compute.ZoneSetLabelsRequest{Labels: map[string]string{"silo": "k7", "a": "b"}, LabelFingerprint: d.LabelFingerprint}))
	do(t, e)(c.InsertDisk(ctx, p, z, &compute.Disk{Name: "hdb", Type: "zones/" + z + "/diskTypes/hyperdisk-balanced", SizeGb: 10}))
	do(t, e)(c.UpdateDisk(ctx, p, z, "hdb", &compute.Disk{ProvisionedIops: 4000}, []string{"provisionedIops"}))
	if d, err := c.GetDisk(ctx, p, z, "hdb"); err != nil || d.ProvisionedIops != 4000 {
		t.Fatalf("updated %+v %v", d, err)
	}

	do(t, e)(c.InsertInstance(ctx, p, z, vm(e, "host", "e2-standard-2"), ""))
	do(t, e)(c.AttachDisk(ctx, p, z, "host", &compute.AttachedDisk{Source: "zones/" + z + "/disks/d", DeviceName: "data"}))
	inst, _ := c.GetInstance(ctx, p, z, "host")
	if len(inst.Disks) != 2 {
		t.Fatalf("attached: %d disks", len(inst.Disks))
	}
	do(t, e)(c.DetachDisk(ctx, p, z, "host", "data"))
	do(t, e)(c.DeleteDisk(ctx, p, z, "d"))
	_, err = c.GetDisk(ctx, p, z, "d")
	wantErr(t, err, http.StatusNotFound, gce.ReasonNotFound)
}

func testImagesAndSnapshots(t *testing.T, e Env) {
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	pub, err := c.GetImageFromFamily(ctx, "debian-cloud", "debian-12")
	if err != nil || pub.Family != "debian-12" {
		t.Fatalf("public family: %+v %v", pub, err)
	}
	if _, err := c.GetImage(ctx, "debian-cloud", pub.Name); err != nil {
		t.Fatalf("public image: %v", err)
	}
	do(t, e)(c.InsertDisk(ctx, p, z, &compute.Disk{Name: "d", SizeGb: 20, SourceImage: pub.SelfLink}))
	do(t, e)(c.CreateSnapshot(ctx, p, z, "d", &compute.Snapshot{Name: "s"}))
	s, err := c.GetSnapshot(ctx, p, "s")
	if err != nil || s.DiskSizeGb != 20 {
		t.Fatalf("snapshot %+v %v", s, err)
	}
	do(t, e)(c.SetSnapshotLabels(ctx, p, "s", &compute.GlobalSetLabelsRequest{Labels: map[string]string{"silo": "k7"}, LabelFingerprint: s.LabelFingerprint}))
	if snaps, err := c.ListSnapshots(ctx, p, gce.ListFilter{Labels: map[string]string{"silo": "k7"}}); err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %d %v", len(snaps), err)
	}
	do(t, e)(c.InsertImage(ctx, p, &compute.Image{Name: "img-1", SourceSnapshot: "global/snapshots/s", Family: "mine"}))
	do(t, e)(c.InsertImage(ctx, p, &compute.Image{Name: "img-2", SourceDisk: "zones/" + z + "/disks/d", Family: "mine"}))
	if img, err := c.GetImageFromFamily(ctx, p, "mine"); err != nil || img.Name != "img-2" {
		t.Fatalf("family: %+v %v", img, err)
	}
	do(t, e)(c.DeprecateImage(ctx, p, "img-2", &compute.DeprecationStatus{State: "DEPRECATED"}))
	if img, err := c.GetImageFromFamily(ctx, p, "mine"); err != nil || img.Name != "img-1" {
		t.Fatalf("family after deprecate: %+v %v", img, err)
	}
	img, _ := c.GetImage(ctx, p, "img-1")
	do(t, e)(c.SetImageLabels(ctx, p, "img-1", &compute.GlobalSetLabelsRequest{Labels: map[string]string{"silo": "k7"}, LabelFingerprint: img.LabelFingerprint}))
	if imgs, err := c.ListImages(ctx, p, gce.ListFilter{Labels: map[string]string{"silo": "k7"}}); err != nil || len(imgs) != 1 {
		t.Fatalf("images: %d %v", len(imgs), err)
	}
	do(t, e)(c.DeleteImage(ctx, p, "img-1"))
	do(t, e)(c.DeleteSnapshot(ctx, p, "s"))
}

func testInstanceTemplates(t *testing.T, e Env) {
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	base := vm(e, "x", "e2-standard-2")
	props := func() *compute.InstanceProperties {
		return &compute.InstanceProperties{MachineType: "e2-standard-2", Disks: base.Disks, NetworkInterfaces: base.NetworkInterfaces}
	}
	do(t, e)(c.InsertInstanceTemplate(ctx, p, "", &compute.InstanceTemplate{Name: "tpl", Properties: props()}))
	do(t, e)(c.InsertInstanceTemplate(ctx, p, e.Region, &compute.InstanceTemplate{Name: "rtpl", Properties: props()}))
	if tp, err := c.GetInstanceTemplate(ctx, p, "", "tpl"); err != nil || tp.Name != "tpl" {
		t.Fatalf("global template %+v %v", tp, err)
	}
	if tp, err := c.GetInstanceTemplate(ctx, p, e.Region, "rtpl"); err != nil || tp.Name != "rtpl" {
		t.Fatalf("regional template %+v %v", tp, err)
	}
	if l, err := c.ListInstanceTemplates(ctx, p, "", gce.ListFilter{}); err != nil || len(l) != 1 {
		t.Fatalf("global templates %d %v", len(l), err)
	}
	if l, err := c.ListInstanceTemplates(ctx, p, e.Region, gce.ListFilter{}); err != nil || len(l) != 1 {
		t.Fatalf("regional templates %d %v", len(l), err)
	}
	do(t, e)(c.InsertInstance(ctx, p, z, &compute.Instance{Name: "from-tpl"}, "global/instanceTemplates/tpl"))
	if inst, err := c.GetInstance(ctx, p, z, "from-tpl"); err != nil || gce.LastSegment(inst.MachineType) != "e2-standard-2" {
		t.Fatalf("from template %+v %v", inst, err)
	}
	do(t, e)(c.DeleteInstanceTemplate(ctx, p, "", "tpl"))
	do(t, e)(c.DeleteInstanceTemplate(ctx, p, e.Region, "rtpl"))
}

func testFirewalls(t *testing.T, e Env) {
	ctx, c, p := ctxOf(e), e.Client, e.Project
	fw := &compute.Firewall{Name: "k7-ssh", Network: "global/networks/silo", Direction: "INGRESS", Priority: 1000,
		SourceRanges: []string{"10.0.0.0/8"}, Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}}
	do(t, e)(c.InsertFirewall(ctx, p, fw))
	do(t, e)(c.PatchFirewall(ctx, p, "k7-ssh", &compute.Firewall{Priority: 900}))
	got, err := c.GetFirewall(ctx, p, "k7-ssh")
	if err != nil || got.Priority != 900 || len(got.Allowed) != 1 {
		t.Fatalf("patched %+v %v", got, err)
	}
	fw.Priority = 800
	do(t, e)(c.UpdateFirewall(ctx, p, "k7-ssh", fw))
	if l, err := c.ListFirewalls(ctx, p, gce.ListFilter{NamePrefix: "k7-"}); err != nil || len(l) != 1 || l[0].Priority != 800 {
		t.Fatalf("firewalls %v %v", l, err)
	}
	do(t, e)(c.DeleteFirewall(ctx, p, "k7-ssh"))
}

func testFirewallPolicy(t *testing.T, e Env) {
	ctx, c, p := ctxOf(e), e.Client, e.Project
	do(t, e)(c.InsertFirewallPolicy(ctx, p, &compute.FirewallPolicy{Name: "silo-fence"}))
	rule := &compute.FirewallPolicyRule{Priority: 1000, Action: "deny", Direction: "EGRESS",
		Match: &compute.FirewallPolicyRuleMatcher{DestIpRanges: []string{"10.255.254.0/24"},
			Layer4Configs: []*compute.FirewallPolicyRuleMatcherLayer4Config{{IpProtocol: "all"}}}}
	do(t, e)(c.AddFirewallPolicyRule(ctx, p, "silo-fence", rule))
	got, err := c.GetFirewallPolicyRule(ctx, p, "silo-fence", 1000)
	if err != nil || got.Action != "deny" {
		t.Fatalf("rule %+v %v", got, err)
	}
	rule.Action = "allow"
	do(t, e)(c.PatchFirewallPolicyRule(ctx, p, "silo-fence", 1000, rule))
	if got, _ := c.GetFirewallPolicyRule(ctx, p, "silo-fence", 1000); got.Action != "allow" {
		t.Fatalf("patched rule %+v", got)
	}
	_, err = c.GetFirewallPolicyRule(ctx, p, "silo-fence", 1234)
	if err == nil {
		t.Fatal("got a rule that doesn't exist")
	}
	do(t, e)(c.AddFirewallPolicyAssociation(ctx, p, "silo-fence", &compute.FirewallPolicyAssociation{Name: "silo", AttachmentTarget: "global/networks/silo"}, false))
	pol, err := c.GetFirewallPolicy(ctx, p, "silo-fence")
	if err != nil || len(pol.Associations) != 1 {
		t.Fatalf("policy %+v %v", pol, err)
	}
	do(t, e)(c.PatchFirewallPolicy(ctx, p, "silo-fence", &compute.FirewallPolicy{Description: "fence", Fingerprint: pol.Fingerprint}))
	_, err = c.PatchFirewallPolicy(ctx, p, "silo-fence", &compute.FirewallPolicy{Description: "stale", Fingerprint: pol.Fingerprint})
	wantErr(t, err, http.StatusPreconditionFailed, "")
	if l, err := c.ListFirewallPolicies(ctx, p); err != nil || len(l) != 1 {
		t.Fatalf("policies %d %v", len(l), err)
	}
	do(t, e)(c.RemoveFirewallPolicyAssociation(ctx, p, "silo-fence", "silo"))
	do(t, e)(c.RemoveFirewallPolicyRule(ctx, p, "silo-fence", 1000))
	do(t, e)(c.DeleteFirewallPolicy(ctx, p, "silo-fence"))
}

func testCatalog(t *testing.T, e Env) {
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	r, err := c.GetRegion(ctx, p, e.Region)
	if err != nil || r.Name != e.Region || len(r.Zones) == 0 {
		t.Fatalf("region %+v %v", r, err)
	}
	if rs, err := c.ListRegions(ctx, p); err != nil || len(rs) == 0 {
		t.Fatalf("regions %d %v", len(rs), err)
	}
	zz, err := c.GetZone(ctx, p, z)
	if err != nil || gce.LastSegment(zz.Region) != e.Region {
		t.Fatalf("zone %+v %v", zz, err)
	}
	zs, err := c.ListZones(ctx, p)
	if err != nil || len(zs) < len(r.Zones) {
		t.Fatalf("zones %d %v", len(zs), err)
	}
	mt, err := c.GetMachineType(ctx, p, z, "e2-standard-2")
	if err != nil || mt.GuestCpus != 2 || mt.MemoryMb != 8192 {
		t.Fatalf("machine type %+v %v", mt, err)
	}
	if l, err := c.ListMachineTypes(ctx, p, z); err != nil || len(l) == 0 {
		t.Fatalf("machine types %d %v", len(l), err)
	}
	if dt, err := c.GetDiskType(ctx, p, z, "pd-balanced"); err != nil || dt.Name != "pd-balanced" {
		t.Fatalf("disk type %+v %v", dt, err)
	}
	if l, err := c.ListDiskTypes(ctx, p, z); err != nil || len(l) == 0 {
		t.Fatalf("disk types %d %v", len(l), err)
	}
	accels, err := c.ListAcceleratorTypes(ctx, p, z)
	if err != nil {
		t.Fatalf("accelerator types: %v", err)
	}
	if len(accels) > 0 {
		if a, err := c.GetAcceleratorType(ctx, p, z, accels[0].Name); err != nil || a.Name != accels[0].Name {
			t.Fatalf("accelerator type %+v %v", a, err)
		}
	}
	_, err = c.GetMachineType(ctx, p, z, "no-such-type")
	wantErr(t, err, http.StatusNotFound, "")
}

func quota(qs []*compute.Quota, metric string) *compute.Quota {
	for _, q := range qs {
		if q.Metric == metric {
			return q
		}
	}
	return nil
}

func testQuotas(t *testing.T, e Env) {
	if e.Fake == nil {
		t.Skip("needs the fake to set quotas")
	}
	ctx, c, p, z := ctxOf(e), e.Client, e.Project, e.Zone
	e.Fake.SetQuota(e.Region, gce.MetricCPUS, 24)
	e.Fake.SetQuota(e.Region, "NVIDIA_H100_GPUS", 16)
	e.Fake.SetQuota("", gce.MetricFirewalls, 200)
	do(t, e)(c.InsertInstance(ctx, p, z, vm(e, "user", "e2-standard-4"), ""))

	r, err := c.GetRegion(ctx, p, e.Region)
	if err != nil {
		t.Fatal(err)
	}
	if q := quota(r.Quotas, gce.MetricCPUS); q == nil || q.Limit != 24 || q.Usage != 4 {
		t.Fatalf("regions.get CPUS %+v", q)
	}
	if q := quota(r.Quotas, "NVIDIA_H100_GPUS"); q != nil {
		t.Fatalf("regions.get reports H100 %+v", q)
	}
	pr, err := c.GetProject(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if q := quota(pr.Quotas, gce.MetricFirewalls); q == nil || q.Limit != 200 {
		t.Fatalf("projects.get FIREWALLS %+v", q)
	}
	infos, err := c.ListComputeQuotaInfos(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, qi := range infos {
		if qi.Metric == gce.CloudQuotasService+"/nvidia_h100_gpus" {
			for _, di := range qi.DimensionsInfos {
				if di.Dimensions["region"] == e.Region && di.Details.Value == 16 {
					found = true
				}
			}
		}
	}
	if !found || len(infos) != 3 {
		t.Fatalf("quotaInfos %d, H100 found %v", len(infos), found)
	}
}
