package install_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/install"
	"github.com/bwsalmon/silo/pkg/silo/install/fakecloud"
	compute "google.golang.org/api/compute/v1"
)

// The uninstall-state harness: like TestStates, but for silo uninstall
// (install.md "silo uninstall", SV-96). Each
// testdata/uninstall/<name>.json is a finished install with a tree in
// it, uninstalled with the recorded flags and interrupted after "after"
// of uninstall's steps; <name>.plan is uninstall's plan from there.
// go test -run TestUninstallStates -update rewrites them.

type uninstallFile struct {
	Args       []string                   `json:"args"`
	Force      bool                       `json:"force,omitempty"`
	DeleteData bool                       `json:"deleteData,omitempty"`
	After      int                        `json:"after"`
	Observed   map[string]json.RawMessage `json:"observed"`
}

// uninstallSeries are the uninstalls interrupted after each step: of a
// tree with silos, an agent, objects, an orphan and a credential, with
// --force --delete-data; and of an empty tree, keeping the database.
var uninstallSeries = []struct {
	name              string
	args              []string
	force, deleteData bool
	tree              bool
}{
	{name: "force-delete-data", args: []string{"--agent-sessions", "2"}, force: true, deleteData: true, tree: true},
	{name: "keep"},
}

// uninstallArgs are the install flags an uninstall's config is built
// from: uninstall reads the same from the install's record.
func uninstallConfig(t *testing.T, args []string, force, deleteData bool) install.Config {
	cfg := config(t, append(append([]string{}, baseArgs...), args...)...)
	cfg.Force, cfg.DeleteData = force, deleteData
	return cfg
}

// removalOrder is UninstallSteps in the order uninstall runs them, the
// Removers only.
func removalOrder(st []install.Step) []install.Remover {
	var out []install.Remover
	for i := len(st) - 1; i >= 0; i-- {
		if r, ok := st[i].(install.Remover); ok {
			out = append(out, r)
		}
	}
	return out
}

// The tree populate makes.
const (
	childID  = "aaaaaa"
	orphanID = "cccccc"
)

// populate gives a finished install a tree: /team-a and /team-a/dev,
// each with a VM, an agent and a VM in the root, an orphaned disk and
// firewall rule, and an agent credential.
func populate(t *testing.T, f *fakecloud.Fake) {
	t.Helper()
	ctx := context.Background()
	f.Silos["/team-a"] = childID
	f.Silos["/team-a/dev"] = "bbbbbb"
	f.Agents["/"] = []string{"fixer"}
	f.CredentialAPI, f.Credentials = true, []string{"anthropic-api"}
	mustOp(t, f)(f.Compute.InsertSubnetwork(ctx, project, "us-central1", &compute.Subnetwork{Name: childID + "-net",
		Network: "global/networks/" + install.NetworkName, IpCidrRange: "10.1.0.0/24", Description: "silo:" + childID + " net"}))
	vm := func(name, silo string) {
		mustOp(t, f)(f.Compute.InsertInstance(ctx, project, "us-central1-a", tenantVM(name, silo), ""))
	}
	vm(childID+"-web", childID)
	vm("bbbbbb-db", "bbbbbb")
	vm(f.RootID+"-tools", f.RootID)
	mustOp(t, f)(f.Compute.InsertDisk(ctx, project, "us-central1-b", &compute.Disk{Name: orphanID + "-data", SizeGb: 10,
		Labels: map[string]string{install.SiloLabel: orphanID, install.SiloManagedLabel: "true"}}))
	mustOp(t, f)(f.Compute.InsertFirewall(ctx, project, &compute.Firewall{Name: orphanID + "-allow-ssh", Network: "global/networks/" + install.NetworkName,
		Description: "silo:" + orphanID + " allow-ssh", Direction: "INGRESS", SourceRanges: []string{"10.0.0.0/8"},
		Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}}))
	// Neither silod's nor a silo's: a name alone doesn't make it one.
	mustOp(t, f)(f.Compute.InsertDisk(ctx, project, "us-central1-b", &compute.Disk{Name: "dddddd-mine", SizeGb: 10}))
}

func TestUninstallStates(t *testing.T) {
	dir := filepath.Join("testdata", "uninstall")
	if *update {
		writeUninstallStates(t, dir)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no states in %s (%v)", dir, err)
	}
	seen := map[string]map[int]bool{}
	var n int
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			var uf uninstallFile
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &uf); err != nil {
				t.Fatal(err)
			}
			rs := removalOrder(install.UninstallSteps(install.NewEnv(uninstallConfig(t, uf.Args, uf.Force, uf.DeleteData), install.Cloud{})))
			n = len(rs)
			var plans []install.StepPlan
			for _, r := range rs {
				raw, ok := uf.Observed[r.Name()]
				if !ok {
					t.Fatalf("no recorded observation of step %s: run go test ./pkg/silo/install -run TestUninstallStates -update", r.Name())
				}
				obs := r.NewObserved()
				if err := json.Unmarshal(raw, obs); err != nil {
					t.Fatalf("%s: %v", r.Name(), err)
				}
				plans = append(plans, install.StepPlan{Step: r.Name(), Changes: r.PlanRemove(obs)})
			}
			got := renderUninstall(plans)
			golden := strings.TrimSuffix(file, ".json") + ".plan"
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("plan differs from %s (go test -update rewrites it):\n--- got\n%s--- want\n%s", golden, got, want)
			}
			series, _, _ := strings.Cut(name, "-after-")
			if seen[series] == nil {
				seen[series] = map[int]bool{}
			}
			seen[series][uf.After] = true
			// Interrupted after step After, a re-run has nothing left to
			// do in the steps before it.
			for i, p := range plans {
				switch {
				case p.Failed():
					t.Errorf("after %d steps, %s fails: %v", uf.After, p.Step, p.Changes)
				case i < uf.After && p.Changing():
					t.Errorf("after %d steps, %s (step %d) plans changes again: %v", uf.After, p.Step, i+1, p.Changes)
				}
			}
			if !uf.DeleteData {
				for _, p := range plans {
					if p.Step == "db-instance" && p.Action() != install.ActionKeep && p.Action() != install.ActionOK {
						t.Errorf("without --delete-data the database plans %v", p.Changes)
					}
				}
			}
		})
	}
	for _, se := range uninstallSeries {
		for i := 0; i <= n; i++ {
			if !seen[se.name][i] {
				t.Errorf("no state %s-after-%d: run go test ./pkg/silo/install -run TestUninstallStates -update", se.name, i)
			}
		}
	}
}

func writeUninstallStates(t *testing.T, dir string) {
	ctx := context.Background()
	old, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range old {
		os.Remove(f)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, se := range uninstallSeries {
		f := fakecloud.New(project)
		apply(t, f, se.args...)
		if se.tree {
			populate(t, f)
		}
		rs := removalOrder(install.UninstallSteps(newEnv(uninstallConfig(t, se.args, se.force, se.deleteData), f)))
		for after := 0; after <= len(rs); after++ {
			uf := uninstallFile{Args: se.args, Force: se.force, DeleteData: se.deleteData, After: after, Observed: map[string]json.RawMessage{}}
			var plans []install.StepPlan
			for _, r := range rs {
				obs, err := r.Observe(ctx)
				if err != nil {
					t.Fatalf("%s after %d: %s: %v", se.name, after, r.Name(), err)
				}
				b, err := json.Marshal(obs)
				if err != nil {
					t.Fatal(err)
				}
				uf.Observed[r.Name()] = b
				plans = append(plans, install.StepPlan{Step: r.Name(), Changes: r.PlanRemove(obs)})
			}
			b, err := json.MarshalIndent(uf, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(dir, fmt.Sprintf("%s-after-%02d", se.name, after))
			if after > 0 {
				base += "-" + rs[after-1].Name()
			}
			if err := os.WriteFile(base+".json", append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(base+".plan", []byte(renderUninstall(plans)), 0o644); err != nil {
				t.Fatal(err)
			}
			if after < len(rs) {
				p := plans[after]
				if p.Changing() {
					if err := rs[after].Apply(ctx, p.Changes); err != nil {
						t.Fatalf("%s: %s: %v", se.name, rs[after].Name(), err)
					}
				}
			}
		}
	}
}

func TestUninstallRefusesWithoutForce(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	apply(t, f)
	populate(t, f)
	st := install.UninstallSteps(newEnv(uninstallConfig(t, nil, false, true), f))
	plans := install.PlanRemoveAll(ctx, st)
	out := render(plans)
	for _, want := range []string{"tenants           fail", "silo /team-a", "agent /agents/fixer", "instances/us-central1-a/r2r2r2-tools",
		"orphans           fail", "disks/us-central1-b/cccccc-data (silo cccccc)", "firewalls/cccccc-allow-ssh", "run again with --force"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/team-a/dev") || strings.Contains(out, "dddddd-mine") {
		t.Errorf("plan lists a grandchild, or a disk that isn't a silo's:\n%s", out)
	}
	var fe *install.FailedError
	if err := install.Remove(ctx, st, &bytes.Buffer{}); !errors.As(err, &fe) || fe.Plan.Step != "tenants" {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := f.Compute.GetInstance(ctx, project, "us-central1-a", install.ControlVM); err != nil {
		t.Errorf("a refused uninstall deleted silo-control: %v", err)
	}
	if _, err := f.Compute.GetInstance(ctx, project, "us-central1-a", childID+"-web"); err != nil || len(f.Silos) != 2 {
		t.Errorf("a refused uninstall deleted a tenant's silo or VM: %v, %v", err, f.Silos)
	}
	if len(f.SQLInstances) != 1 || !f.SQLInstances[0].DeletionProtection || len(f.ProtectionChanges) != 0 {
		t.Errorf("a refused uninstall touched the database: %+v, %v", f.SQLInstances, f.ProtectionChanges)
	}
}

func TestUninstallForceDeleteData(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	apply(t, f, "--agent-sessions", "2")
	populate(t, f)
	// Someone else's database, with deletion protection: left alone.
	f.SQLInstances = append(f.SQLInstances, install.SQLInstance{Name: "billing-db", Region: "us-central1", State: "RUNNABLE", DeletionProtection: true})
	st := install.UninstallSteps(newEnv(uninstallConfig(t, []string{"--agent-sessions", "2"}, true, true), f))
	plans := install.PlanRemoveAll(ctx, st)
	out := render(plans)
	for _, want := range []string{"tenants           delete  silo /team-a", "agent /agents/fixer", "instances/us-central1-a/r2r2r2-tools",
		"orphans           delete  disks/us-central1-b/cccccc-data", "credentials       delete  credential anthropic-api",
		"us-central1-a/silo-control", "db-instance       delete  silo-db-t3st", "deletion protection lifted on this instance only", "kept 30 days"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "billing-db") {
		t.Errorf("plan names another database:\n%s", out)
	}
	if err := install.Remove(ctx, st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.Silos)+len(f.Agents["/"])+len(f.Credentials) != 0 {
		t.Errorf("left silos %v, agents %v, credentials %v", f.Silos, f.Agents, f.Credentials)
	}
	for _, z := range []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"} {
		insts, _ := f.Compute.ListInstances(ctx, project, z, noFilter)
		disks, _ := f.Compute.ListDisks(ctx, project, z, noFilter)
		for _, i := range insts {
			t.Errorf("left instance %s/%s", z, i.Name)
		}
		for _, d := range disks {
			if d.Name != "dddddd-mine" {
				t.Errorf("left disk %s/%s", z, d.Name)
			}
		}
	}
	if _, err := f.Compute.GetDisk(ctx, project, "us-central1-b", "dddddd-mine"); err != nil {
		t.Errorf("deleted a disk that isn't a silo's: %v", err)
	}
	if len(f.SQLInstances) != 1 || f.SQLInstances[0].Name != "billing-db" || !f.SQLInstances[0].DeletionProtection {
		t.Errorf("databases left: %+v", f.SQLInstances)
	}
	if fmt.Sprint(f.ProtectionChanges) != "[silo-db-t3st=false]" || f.FinalBackups["silo-db-t3st"] != install.FinalBackupDays {
		t.Errorf("deletion protection changes %v, final backups %v", f.ProtectionChanges, f.FinalBackups)
	}
	for _, p := range install.PlanRemoveAll(ctx, st) {
		if p.Changing() || p.Failed() {
			t.Errorf("second uninstall: %s plans %v", p.Step, p.Changes)
		}
	}
}

// TestUninstallRemovesTheKeyAfterTheCredentials: silod deletes the
// credentials while the key is still there, and every control VM loses
// the key before any of install's objects go.
func TestUninstallRemovesTheKeyAfterTheCredentials(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	apply(t, f, "--agent-sessions", "2")
	f.CredentialAPI, f.Credentials = true, []string{"anthropic-api", "claude-max-bob"}
	env := newEnv(uninstallConfig(t, []string{"--agent-sessions", "2"}, false, false), f)
	cred := &install.Credentials{Env: env}
	p := install.PlanStep(ctx, removerStep{cred})
	if err := cred.Apply(ctx, p.Changes); err != nil {
		t.Fatal(err)
	}
	if len(f.Credentials) != 0 {
		t.Errorf("credentials left: %v", f.Credentials)
	}
	vm, err := f.Compute.GetInstance(ctx, project, "us-central1-a", install.ControlVM)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range vm.Metadata.Items {
		if it.Key == install.CredentialKeyKey {
			t.Errorf("%s still holds %s", install.ControlVM, it.Key)
		}
	}
	if len(vm.Metadata.Items) == 0 {
		t.Error("the rest of the VM's metadata went with the key")
	}
}

// TestUninstallCredentialsBeforeTheEndpoint: a silod with no
// credentials endpoint answers 404, which is none, and the step is ok.
func TestUninstallCredentialsBeforeTheEndpoint(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	apply(t, f)
	env := newEnv(uninstallConfig(t, nil, false, false), f)
	p := install.PlanStep(ctx, removerStep{&install.Credentials{Env: env}})
	if p.Action() != install.ActionOK || !strings.Contains(p.Changes[0].Detail, "no credentials endpoint") {
		t.Errorf("plan: %+v", p)
	}
}

// TestUninstallLeavesProtectedVMs: uninstall lifts deletion protection
// only on the database, so a tenant VM with it stops even --force.
func TestUninstallLeavesProtectedVMs(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	apply(t, f)
	keep := tenantVM(f.RootID+"-keep", f.RootID)
	keep.DeletionProtection = true
	mustOp(t, f)(f.Compute.InsertInstance(ctx, project, "us-central1-a", keep, ""))
	st := install.UninstallSteps(newEnv(uninstallConfig(t, nil, true, true), f))
	plans := install.PlanRemoveAll(ctx, st)
	if !plans[0].Failed() || !strings.Contains(render(plans), "has deletion protection") {
		t.Errorf("plan:\n%s", render(plans))
	}
	if err := install.Remove(ctx, st, &bytes.Buffer{}); err == nil {
		t.Error("uninstall went ahead")
	}
	vm, err := f.Compute.GetInstance(ctx, project, "us-central1-a", f.RootID+"-keep")
	if err != nil || !vm.DeletionProtection || len(f.ProtectionChanges) != 0 {
		t.Errorf("vm %v, %v; protection changes %v", vm, err, f.ProtectionChanges)
	}
}

var noFilter gce.ListFilter

func renderUninstall(plans []install.StepPlan) string {
	var b bytes.Buffer
	install.RenderTable(&b, "silo uninstall: project "+project, plans)
	return b.String()
}

// tenantVM is a VM silod made for silo, in the gateway subnet.
func tenantVM(name, silo string) *compute.Instance {
	return &compute.Instance{Name: name, MachineType: "zones/us-central1-a/machineTypes/e2-standard-2",
		Labels: map[string]string{install.SiloLabel: silo, install.SiloManagedLabel: "true"},
		Disks: []*compute.AttachedDisk{{Boot: true, AutoDelete: true,
			InitializeParams: &compute.AttachedDiskInitializeParams{SourceImage: "projects/cos-cloud/global/images/family/cos-stable"}}},
		NetworkInterfaces: []*compute.NetworkInterface{{Network: "global/networks/" + install.NetworkName,
			Subnetwork: "regions/us-central1/subnetworks/" + install.GatewaySubnetName}}}
}

// removerStep plans a Remover's PlanRemove through PlanStep.
type removerStep struct{ install.Remover }

func (r removerStep) Plan(o install.Observed) []install.Change { return r.PlanRemove(o) }
