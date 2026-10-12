package install_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/install"
	"github.com/bwsalmon/silo/pkg/install/fakecloud"
)

const project = "silo-test-project"

// config parses an install command line onto the defaults.
func config(t *testing.T, args ...string) install.Config {
	t.Helper()
	cfg := install.Defaults()
	fs := flag.NewFlagSet("silo install", flag.ContinueOnError)
	cfg.Flags(fs, "v1.0.0")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

var baseArgs = []string{"--project", project, "--regions", "us-central1,europe-west4", "--admin", "user:admin@example.com"}

func steps(t *testing.T, f *fakecloud.Fake, args ...string) []install.Step {
	return install.Steps(newEnv(config(t, append(append([]string{}, baseArgs...), args...)...), f))
}

// newEnv is an Env on f whose database instance is silo-db-t3st.
func newEnv(cfg install.Config, f *fakecloud.Fake) *install.Env {
	env := install.NewEnv(cfg, f.Cloud())
	env.NewSuffix = func() string { return "t3st" }
	return env
}

func render(plans []install.StepPlan) string {
	var b bytes.Buffer
	install.RenderTable(&b, "silo install: project "+project, plans)
	return b.String()
}

func TestInstallFromScratchThenRerun(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	st := steps(t, f)
	plans := install.PlanAll(ctx, st)
	out := render(plans)
	for _, want := range []string{"defer", "compute.googleapis.com, dns.googleapis.com", "network           create  silo (custom"} {
		if !strings.Contains(out, want) {
			t.Errorf("fresh plan lacks %q:\n%s", want, out)
		}
	}
	var progress bytes.Buffer
	if err := install.Apply(ctx, st, &progress); err != nil {
		t.Fatalf("apply: %v\n%s", err, progress.String())
	}
	if !strings.Contains(progress.String(), "prereqs: every check has now run") {
		t.Errorf("prereqs wasn't rechecked after apis:\n%s", progress.String())
	}
	// A re-run with the same flags has nothing to do.
	st = steps(t, f)
	for _, p := range install.PlanAll(ctx, st) {
		if p.Changing() || p.Failed() || p.Deferred() {
			t.Errorf("re-run: %s still plans %v (%v)", p.Step, p.Changes, p.Err)
		}
	}
	progress.Reset()
	// It applies nothing; verify reports the root again.
	if err := install.Apply(ctx, st, &progress); err != nil || strings.Contains(progress.String(), "...") ||
		!strings.HasPrefix(progress.String(), "verify: root /\n  regions: us-central1, europe-west4\n  blocks:  10.0.0.0/8\n") {
		t.Errorf("re-run applied something: %v\n%s", err, progress.String())
	}
	n, err := f.Compute.GetNetwork(ctx, project, install.NetworkName)
	if err != nil {
		t.Fatal(err)
	}
	if n.RoutingConfig.RoutingMode != "GLOBAL" || n.NetworkFirewallPolicyEnforcementOrder != "BEFORE_CLASSIC_FIREWALL" {
		t.Errorf("network: %+v", n)
	}
	// No --admin is needed once installed.
	st = install.Steps(install.NewEnv(config(t, "--project", project, "--regions", "us-central1"), f.Cloud()))
	if p := install.PlanStep(ctx, st[0]); p.Failed() {
		t.Errorf("re-run without --admin: %v", p.Changes)
	}
}

func TestFlagThatCantChangeStopsAtPrereqs(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	if err := install.Apply(ctx, steps(t, f), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--regions", "us-central1", "--address-space", "172.16.0.0/12", "--gateway-range", "172.16.255.0/24", "--control-range", "172.16.254.0/24"},
		{"--regions", "us-central1", "--gateway-range", "10.255.253.0/24"},
		{"--regions", "europe-west4,us-central1"},
	} {
		st := install.Steps(install.NewEnv(config(t, append([]string{"--project", project}, args...)...), f.Cloud()))
		err := install.Apply(ctx, st, &bytes.Buffer{})
		var fe *install.FailedError
		if !errors.As(err, &fe) || fe.Plan.Step != "prereqs" || !strings.Contains(err.Error(), "silo uninstall first") {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

func TestDeferredCheckFailsAfterAPIs(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	f.Billing = false // unreadable until apis enables cloudbilling
	st := steps(t, f)
	err := install.Apply(ctx, st, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "billing is not enabled") {
		t.Fatalf("got %v", err)
	}
	if _, err := f.Compute.GetNetwork(ctx, project, install.NetworkName); err == nil {
		t.Error("install went on past a failed check")
	}
}

// A re-run puts back every field install owns, and leaves silod's
// rules in the fence as they are.
func TestRerunRepairsDrift(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	if err := install.Apply(ctx, steps(t, f), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	drift(t, f)
	var progress bytes.Buffer
	if err := install.Apply(ctx, steps(t, f), &progress); err != nil {
		t.Fatalf("%v\n%s", err, progress.String())
	}
	for _, p := range install.PlanAll(ctx, steps(t, f)) {
		if p.Changing() || p.Failed() {
			t.Errorf("after the repair, %s still plans %v (%v)", p.Step, p.Changes, p.Err)
		}
	}
	pol, err := f.Compute.GetFirewallPolicy(ctx, project, fence.PolicyName)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, r := range pol.Rules {
		kept = kept || (r.RuleName == silodRule.RuleName && r.Priority == silodRule.Priority)
	}
	if !kept {
		t.Errorf("the repair removed silod's rule %s", silodRule.RuleName)
	}
	sa := f.ServiceAccounts[install.SilodEmail(project)]
	if sa.Disabled || sa.DisplayName != "silod" || len(f.Roles[install.RoleID].Permissions) != len(install.SilodPermissions) {
		t.Errorf("account %+v, role %d permissions", sa, len(f.Roles[install.RoleID].Permissions))
	}
}

func TestUninstall(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	st := steps(t, f)
	if err := install.Apply(ctx, st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	plans := install.PlanRemoveAll(ctx, st)
	out := render(plans)
	for _, want := range []string{"network           delete  silo", "apis              keep    leaves on artifactregistry", "db-instance       keep    silo-db-t3st kept", "13 to delete, 2 to keep", "admin             delete  roles/iap.tunnelResourceAccessor on silo-control"} {
		if !strings.Contains(out, want) {
			t.Errorf("uninstall plan lacks %q:\n%s", want, out)
		}
	}
	if err := install.Remove(ctx, st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Compute.GetNetwork(ctx, project, install.NetworkName); err == nil {
		t.Error("network survived uninstall")
	}
	if !f.Enabled["compute.googleapis.com"] {
		t.Error("uninstall disabled an API")
	}
	if len(f.ServiceAccounts)+len(f.Zones)+len(f.Records)+len(f.Routers)+len(f.Addresses)+len(f.ForwardingRules) != 0 || !f.Roles[install.RoleID].Deleted {
		t.Errorf("uninstall left objects: accounts %v, zones %v, records %v, routers %v, addresses %v, rules %v, role %+v",
			f.ServiceAccounts, f.Zones, f.Records, f.Routers, f.Addresses, f.ForwardingRules, f.Roles[install.RoleID])
	}
	if len(f.SQLInstances) != 1 || !f.SQLInstances[0].DeletionProtection {
		t.Errorf("uninstall didn't keep the database: %+v", f.SQLInstances)
	}
	// Without --delete-data, deletion protection is never touched.
	if len(f.ProtectionChanges) != 0 || len(f.FinalBackups) != 0 {
		t.Errorf("uninstall touched the database: protection %v, final backups %v", f.ProtectionChanges, f.FinalBackups)
	}
	for _, b := range f.ProjectPolicy.Bindings {
		t.Errorf("uninstall left a binding: %+v", b)
	}
	// Uninstall is idempotent.
	for _, p := range install.PlanRemoveAll(ctx, st) {
		if p.Changing() {
			t.Errorf("second uninstall: %s plans %v", p.Step, p.Changes)
		}
	}
}

func TestUninstallLeavesAnotherInstallAlone(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	f.EnableAll()
	if err := (&install.Network{Env: install.NewEnv(config(t, baseArgs...), f.Cloud())}).Apply(ctx, []install.Change{{Action: install.ActionCreate}}); err != nil {
		t.Fatal(err)
	}
	// Another project's install id: the record is not this install's.
	other := install.Steps(install.NewEnv(config(t, "--project", "other-project", "--regions", "us-central1"), f.Cloud()))
	for _, p := range install.PlanRemoveAll(ctx, other) {
		if p.Changing() {
			t.Errorf("%s would remove another install's objects: %v", p.Step, p.Changes)
		}
	}
}

// The credentials check passes a real project's testIamPermissions: the
// first real install failed it because the set named a billing
// permission, which projects refuse outright (the fake now does too).
func TestCredentialsCheckIsValidOnAProject(t *testing.T) {
	for _, p := range install.Permissions() {
		if strings.HasPrefix(p, "billing.") {
			t.Errorf("Permissions() has %s, which a project's testIamPermissions refuses", p)
		}
	}
	f := fakecloud.New(project)
	g, err := f.Cloud().ResourceManager.TestIamPermissions(context.Background(), project, install.Permissions())
	if err != nil {
		t.Fatalf("testIamPermissions: %v", err)
	}
	if len(g) != len(install.Permissions()) {
		t.Errorf("granted %d of %d", len(g), len(install.Permissions()))
	}
}
