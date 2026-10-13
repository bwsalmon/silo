package install_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/gcefence"
	"github.com/bwsalmon/silo/pkg/install"
	"github.com/bwsalmon/silo/pkg/install/fakecloud"
	compute "google.golang.org/api/compute/v1"
)

// The install-state harness (install.md "Testing install", SV-97): every
// step's Plan, run against recorded Observe results, for every
// partly-installed state and for a few broken projects. It needs no
// credentials and makes no calls: Plan is pure.
//
// Each testdata/states/<name>.json holds an install command line, the
// number of steps applied before the observations were recorded
// ("after", or -1 for a state that isn't a point in an install), and
// each step's Observed; <name>.plan is the plan table those give.
//
// A new step needs its observation in every state: run
//
//	go test ./pkg/install -run TestStates -update
//
// which rebuilds every state against the in-memory cloud and rewrites
// the fixtures and plans, then read the diff.

var update = flag.Bool("update", false, "rebuild testdata/states from the in-memory cloud")

type stateFile struct {
	Args     []string                   `json:"args"`
	After    int                        `json:"after"`
	Observed map[string]json.RawMessage `json:"observed"`
}

// replicasPrefix names the states of an install with replicaArgs.
const replicasPrefix = "replicas-"

// series are the installs interrupted after each step: with the base
// flags, and with control and gateway replicas.
var series = []struct {
	prefix string
	args   []string
}{{"", nil}, {replicasPrefix, replicaArgs}}

// scenario is a broken project, built on the in-memory cloud.
type scenario struct {
	name  string
	args  []string
	setup func(t *testing.T, f *fakecloud.Fake)
}

var scenarios = []scenario{
	{
		name: "prereqs-failing",
		args: []string{"--project", project, "--regions", "us-central1,xx-nowhere1", "--agent-sessions", "12", "--control-replicas", "2",
			"--db-tier", "db-perf-optimized-N-2", "--db-version", "POSTGRES_99"},
		setup: func(t *testing.T, f *fakecloud.Fake) {
			ctx := context.Background()
			f.EnableAll()
			f.Billing = false
			f.Denied["compute.networks.create"] = true
			f.Denied["billing.resourceAssociations.list"] = true
			f.Project.Parent = "organizations/1234"
			f.Policies["iam.disableServiceAccountCreation"] = &install.OrgPolicy{Constraint: "iam.disableServiceAccountCreation", Enforced: true}
			f.Policies["gcp.resourceLocations"] = &install.OrgPolicy{Constraint: "gcp.resourceLocations", Allowed: []string{"in:europe-locations"}}
			f.Policies["compute.disablePrivateServiceConnectCreationForConsumers"] = &install.OrgPolicy{Allowed: []string{"SERVICE_PRODUCERS"}}
			f.SQLInstances = []install.SQLInstance{{Name: "silo-db-kept", Region: "us-central1", Labels: map[string]string{install.LabelKey: install.InstallID(project)}}}
			f.Compute.SetQuota("us-central1", gce.MetricCPUS, 4)
			f.Compute.SetQuota("", gce.MetricSubnetworks, 2)
			mustOp(t, f)(f.Compute.InsertNetwork(ctx, project, &compute.Network{Name: "default", ForceSendFields: []string{"AutoCreateSubnetworks"}}))
			mustOp(t, f)(f.Compute.InsertSubnetwork(ctx, project, "us-central1", &compute.Subnetwork{
				Name: "default", Network: "global/networks/default", IpCidrRange: "10.128.0.0/20"}))
			mustOp(t, f)(f.Compute.InsertSubnetwork(ctx, project, "europe-west4", &compute.Subnetwork{
				Name: "other", Network: "global/networks/default", IpCidrRange: "192.168.0.0/24"}))
		},
	},
	{
		name: "drifted",
		args: baseArgs,
		setup: func(t *testing.T, f *fakecloud.Fake) {
			if err := install.Apply(context.Background(), steps(t, f), &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			drift(t, f)
		},
	},
	{
		name: "foreign-objects",
		args: baseArgs,
		setup: func(t *testing.T, f *fakecloud.Fake) {
			f.EnableAll()
			email := install.SilodEmail(project)
			f.ServiceAccounts[email] = &install.ServiceAccount{Email: email, DisplayName: "silod", Description: "made by hand"}
			f.Roles[install.RoleID] = &install.Role{Title: "silod", Description: "made by hand", Permissions: []string{"compute.instances.get"}}
			f.Zones[install.ZoneName] = &install.ManagedZone{Name: install.ZoneName, DNSName: install.ZoneDNSName}
		},
	},
	{
		// An earlier install, uninstalled: its database was kept.
		name: "db-kept",
		args: baseArgs,
		setup: func(t *testing.T, f *fakecloud.Fake) {
			st := steps(t, f)
			if err := install.Apply(context.Background(), st, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if err := install.Remove(context.Background(), st, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
		},
	},
	{
		// A finished install whose database was restored into a new
		// instance, adopted with --db-instance.
		name: "db-adopt",
		args: append(append([]string{}, baseArgs...), "--db-instance", "silo-db-restored"),
		setup: func(t *testing.T, f *fakecloud.Fake) {
			if err := install.Apply(context.Background(), steps(t, f), &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			restored(t, f)
		},
	},
	{
		name: "changed-flags",
		args: []string{"--project", project, "--regions", "europe-west4,us-central1", "--address-space", "172.16.0.0/12",
			"--gateway-range", "172.31.255.0/24", "--control-range", "172.31.254.0/24"},
		setup: func(t *testing.T, f *fakecloud.Fake) {
			st := steps(t, f)
			if err := install.Apply(context.Background(), st, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
		},
	},
}

// silodRule is a rule silod's fence applier owns (the root's
// n-intra-in, in depth 0's band): install leaves it alone.
var silodRule = &compute.FirewallPolicyRule{RuleName: "root-intra-in", Priority: gcefence.DepthBand(0), Direction: "INGRESS",
	Action: "goto_next", Match: &compute.FirewallPolicyRuleMatcher{SrcIpRanges: []string{"10.0.0.0/9"},
		Layer4Configs: []*compute.FirewallPolicyRuleMatcherLayer4Config{{IpProtocol: "all"}}}}

// drift changes, on a finished install, a field each step owns, and adds
// a rule of silod's to the fence.
func drift(t *testing.T, f *fakecloud.Fake) {
	ctx := context.Background()
	sa := f.ServiceAccounts[install.SilodEmail(project)]
	sa.DisplayName, sa.Disabled = "someone else's", true
	role := f.Roles[install.RoleID]
	role.Permissions = append(role.Permissions[1:], "iam.roles.create")
	for _, b := range f.ProjectPolicy.Bindings {
		if b.Role == "roles/logging.logWriter" {
			b.Members = nil
		}
	}
	mustOp(t, f)(f.Compute.SetPrivateIpGoogleAccess(ctx, project, "us-central1", install.GatewaySubnetName, false))
	mustOp(t, f)(f.Compute.SetPrivateIpGoogleAccess(ctx, project, "us-central1", install.ControlSubnetName, true))
	nat := f.Routers["us-central1/"+install.RouterName].Nats[0]
	nat.SourceSubnetworkIpRangesToNat, nat.Subnetworks = "ALL_SUBNETWORKS_ALL_IP_RANGES", nil
	pol, err := f.Compute.GetFirewallPolicy(ctx, project, gcefence.PolicyName)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pol.Rules {
		if r.RuleName == gcefence.RuleCatchOut {
			r.Action = "allow"
			mustOp(t, f)(f.Compute.PatchFirewallPolicyRule(ctx, project, gcefence.PolicyName, r.Priority, r))
		}
	}
	mustOp(t, f)(f.Compute.AddFirewallPolicyRule(ctx, project, gcefence.PolicyName, silodRule))
	mustOp(t, f)(f.Compute.RemoveFirewallPolicyAssociation(ctx, project, gcefence.PolicyName, install.NetworkName))
	f.Zones[install.ZoneName].Networks = nil
	for _, r := range f.Records {
		r.Rrdatas = []string{"10.9.9.9"}
	}
	for _, p := range f.InstancePolicies {
		p.Bindings = nil
	}
	db := &f.SQLInstances[0]
	db.Tier, db.MaintenanceDay, db.PublicIP = "db-f1-micro", 1, true
	db.Flags = map[string]string{"cloudsql.iam_authentication": "off", "log_connections": "on"}
	delete(f.Records, "silo-db/"+db.DNSName+"/A")
}

// restored adds silo-db-restored, a clone of the install's database
// with Cloud SQL's defaults for what a clone doesn't copy (no labels, no
// deletion protection, the default maintenance window).
func restored(t *testing.T, f *fakecloud.Fake) {
	t.Helper()
	sql := f.Cloud().SQLAdmin
	ctx := context.Background()
	in := f.SQLInstances[0]
	in.Name, in.Labels, in.DeletionProtection, in.MaintenanceDay, in.MaintenanceHour = "silo-db-restored", nil, false, 0, 0
	if err := sql.InsertInstance(ctx, project, &in); err != nil {
		t.Fatal(err)
	}
	if _, err := sql.WaitInstance(ctx, project, in.Name); err != nil {
		t.Fatal(err)
	}
}

// mustOp returns a func that waits for a compute call's operation.
func mustOp(t *testing.T, f *fakecloud.Fake) func(*compute.Operation, error) {
	return func(op *compute.Operation, err error) {
		t.Helper()
		if err == nil {
			_, err = gce.Await(context.Background(), f.Compute, project, op)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStates(t *testing.T) {
	dir := filepath.Join("testdata", "states")
	if *update {
		writeStates(t, dir)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no states in %s (%v)", dir, err)
	}
	// Each series of interrupted installs, by its files' prefix: the
	// plain one, and replicaArgs' (series).
	interrupted := map[string]map[int]bool{}
	for _, se := range series {
		interrupted[se.prefix] = map[int]bool{}
	}
	var n int
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			var sf stateFile
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &sf); err != nil {
				t.Fatal(err)
			}
			// Plan is pure: the steps get a Cloud with nothing in it.
			st := install.Steps(install.NewEnv(config(t, sf.Args...), install.Cloud{}))
			n = len(st)
			var plans []install.StepPlan
			for _, s := range st {
				raw, ok := sf.Observed[s.Name()]
				if !ok {
					t.Fatalf("no recorded observation of step %s: run go test ./pkg/install -run TestStates -update", s.Name())
				}
				obs := s.NewObserved()
				if err := json.Unmarshal(raw, obs); err != nil {
					t.Fatalf("%s: %v", s.Name(), err)
				}
				plans = append(plans, install.StepPlan{Step: s.Name(), Changes: s.Plan(obs)})
			}
			got := render(plans)
			golden := strings.TrimSuffix(file, ".json") + ".plan"
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("plan differs from %s (go test -update rewrites it):\n--- got\n%s--- want\n%s", golden, got, want)
			}
			if sf.After < 0 {
				return
			}
			prefix := ""
			if strings.HasPrefix(name, replicasPrefix) {
				prefix = replicasPrefix
			}
			interrupted[prefix][sf.After] = true
			// Interrupted after step After, a re-run has nothing left to
			// do in the steps before it, and picks up at the next.
			for i, p := range plans {
				switch {
				case p.Failed():
					t.Errorf("after %d steps, %s fails: %v", sf.After, p.Step, p.Changes)
				case i < sf.After && p.Changing():
					t.Errorf("after %d steps, %s (step %d) plans changes again: %v", sf.After, p.Step, i+1, p.Changes)
				// verify, like prereqs, is a check: it is ok as soon as
				// the steps before it have made something that passes.
				case i == sf.After && !p.Changing() && i > 0 && p.Step != "verify":
					t.Errorf("after %d steps, %s (the next step) plans nothing", sf.After, p.Step)
				}
			}
		})
	}
	for prefix, seen := range interrupted {
		for i := 0; i <= n; i++ {
			if !seen[i] {
				t.Errorf("no state %sinterrupted after step %d: run go test ./pkg/install -run TestStates -update", prefix, i)
			}
		}
	}
}

// writeStates rebuilds every state from the in-memory cloud: an install
// interrupted after each step in turn, and each scenario.
func writeStates(t *testing.T, dir string) {
	ctx := context.Background()
	old, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range old {
		os.Remove(f)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, se := range series {
		f := fakecloud.New(project)
		args := append(append([]string{}, baseArgs...), se.args...)
		st := steps(t, f, se.args...)
		for after := 0; after <= len(st); after++ {
			name := se.prefix + "fresh"
			if after > 0 {
				name = fmt.Sprintf("%safter-%d-%s", se.prefix, after, st[after-1].Name())
			}
			record(t, dir, name, args, after, st)
			if after < len(st) {
				p := install.PlanStep(ctx, st[after])
				if err := st[after].Apply(ctx, p.Changes); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, sc := range scenarios {
		f := fakecloud.New(project)
		sc.setup(t, f)
		st := install.Steps(newEnv(config(t, sc.args...), f))
		record(t, dir, sc.name, sc.args, -1, st)
	}
}

func record(t *testing.T, dir, name string, args []string, after int, st []install.Step) {
	ctx := context.Background()
	sf := stateFile{Args: args, After: after, Observed: map[string]json.RawMessage{}}
	var plans []install.StepPlan
	for _, s := range st {
		obs, err := s.Observe(ctx)
		if err != nil {
			t.Fatalf("%s: %s: %v", name, s.Name(), err)
		}
		b, err := json.Marshal(obs)
		if err != nil {
			t.Fatal(err)
		}
		sf.Observed[s.Name()] = b
		plans = append(plans, install.StepPlan{Step: s.Name(), Changes: s.Plan(obs)})
	}
	b, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, name)
	if err := os.WriteFile(base+".json", append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".plan", []byte(render(plans)), 0o644); err != nil {
		t.Fatal(err)
	}
}
