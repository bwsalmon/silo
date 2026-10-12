package install_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/install"
	"github.com/bwsalmon/silo/pkg/install/fakecloud"
)

// planOf plans one step, by name, against the fake.
func planOf(t *testing.T, st []install.Step, name string) install.StepPlan {
	t.Helper()
	for _, s := range st {
		if s.Name() == name {
			return install.PlanStep(context.Background(), s)
		}
	}
	t.Fatalf("no step %s", name)
	return install.StepPlan{}
}

// TestDBInstanceSettings: the instance install creates has every
// setting of state.md's table (SV-47), and no password anywhere.
func TestDBInstanceSettings(t *testing.T) {
	f := fakecloud.New(project)
	if err := install.Apply(context.Background(), steps(t, f, "--db-ha", "--db-tier", "db-custom-1-3840",
		"--db-backup-retention", "30", "--db-maintenance-window", "sat:03:00"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.SQLInstances) != 1 {
		t.Fatalf("%d instances", len(f.SQLInstances))
	}
	in := f.SQLInstances[0]
	want := install.SQLInstance{Name: "silo-db-t3st", Region: "us-central1", DatabaseVersion: "POSTGRES_17", Edition: "ENTERPRISE",
		Tier: "db-custom-1-3840", Availability: "REGIONAL", DiskType: "PD_SSD", DiskSizeGB: 10, AutoResize: true,
		Backups: true, BackupRetention: 30, PITR: true, LogRetentionDays: 7, DeletionProtection: true,
		PSC: true, PSCConsumers: []string{project}, MaintenanceDay: 6, MaintenanceHour: 3}
	got := in
	got.State, got.Labels, got.Flags, got.ServiceAttachment, got.DNSName, got.ConnectionName = "", nil, nil, "", "", ""
	if g, w := toJSON(t, got), toJSON(t, want); g != w {
		t.Errorf("instance\n got %s\nwant %s", g, w)
	}
	if in.Flags["cloudsql.iam_authentication"] != "on" || len(in.Flags) != 1 {
		t.Errorf("flags %v", in.Flags)
	}
	id := install.InstallID(project)
	if in.Labels[install.LabelKey] != id || in.Labels[install.RoleLabel] != "db" || in.Labels[install.SchemaLabel] != strconv.Itoa(install.SchemaVersion()) ||
		in.Labels[install.RestoredLabel] != "" {
		t.Errorf("labels %v", in.Labels)
	}
	// One database user besides the built-in postgres, which has no
	// password: silod's, an IAM service account with cloudsqlsuperuser.
	users := f.Users[in.Name]
	if len(users) != 2 || users[1].Name != "silod@"+project+".iam" || users[1].Type != "CLOUD_IAM_SERVICE_ACCOUNT" ||
		strings.Join(users[1].DatabaseRoles, ",") != "cloudsqlsuperuser" {
		t.Errorf("users %+v", users)
	}
	if !contains(f.Databases[in.Name], "silo") {
		t.Errorf("databases %v", f.Databases[in.Name])
	}
	// The endpoint is in the control subnet and points at the
	// instance; the zone resolves the instance's PSC name to it.
	rule := f.ForwardingRules["us-central1/silo-db-psc"]
	addr := f.Addresses["us-central1/silo-db-psc"]
	if rule == nil || addr == nil || rule.Target != in.ServiceAttachment || rule.IPAddress != addr.Address ||
		!strings.HasSuffix(addr.Subnetwork, "/subnetworks/silo-ctl") || !strings.HasPrefix(addr.Address, "10.255.254.") {
		t.Errorf("endpoint: rule %+v, address %+v", rule, addr)
	}
	if z := f.Zones["silo-db"]; z == nil || z.DNSName != in.DNSName {
		t.Errorf("zone %+v", z)
	}
	if r := f.Records["silo-db/"+in.DNSName+"/A"]; r == nil || strings.Join(r.Rrdatas, " ") != addr.Address {
		t.Errorf("record %+v", r)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// TestDBStepWaits: while the instance is being created, db plans to
// wait for it, and Apply does.
func TestDBStepWaits(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	st := steps(t, f)
	for _, s := range st[:3] {
		if err := s.Apply(ctx, install.PlanStep(ctx, s).Changes); err != nil {
			t.Fatal(err)
		}
	}
	if f.SQLInstances[0].State != "PENDING_CREATE" {
		t.Fatalf("db-instance waited for the instance: %s", f.SQLInstances[0].State)
	}
	p := planOf(t, st, "db")
	if p.Action() != install.ActionCreate || !strings.Contains(p.Changes[0].Detail, "waits for silo-db-t3st") {
		t.Errorf("db plans %v", p.Changes)
	}
	if p := planOf(t, st, "db-instance"); p.Action() != install.ActionOK || p.Changes[0].Detail != "being created" {
		t.Errorf("db-instance plans %v", p.Changes)
	}
}

// TestDBRefusedRole is open question 19's call: if the Admin API won't
// create the IAM user with cloudsqlsuperuser, the step fails, saying so,
// and nothing falls back to a password.
func TestDBRefusedRole(t *testing.T) {
	f := fakecloud.New(project)
	f.RefuseRoles = true
	err := install.Apply(context.Background(), steps(t, f), &bytes.Buffer{})
	var refused *install.RefusedRoleError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "open question 19") || !strings.Contains(err.Error(), "password") {
		t.Fatalf("install with roles refused: %v", err)
	}
	for _, u := range f.Users[f.SQLInstances[0].Name] {
		if u.Name != "postgres" {
			t.Errorf("a user was created anyway: %+v", u)
		}
	}
}

// TestDBInPlace: tier, HA, retention and the window change in place; the
// version only goes up.
func TestDBInPlace(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	if err := install.Apply(ctx, steps(t, f, "--db-version", "POSTGRES_16"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	st := steps(t, f, "--db-version", "POSTGRES_17", "--db-ha", "--db-tier", "db-custom-1-3840", "--db-backup-retention", "7",
		"--db-maintenance-window", "wed:22:00")
	p := planOf(t, st, "db-instance")
	if p.Action() != install.ActionUpdate || !strings.Contains(p.Changes[0].Detail, "restarts the instance") ||
		!strings.Contains(p.Changes[0].Detail, "POSTGRES_17, upgraded from POSTGRES_16") {
		t.Fatalf("db-instance plans %v", p.Changes)
	}
	if err := install.Apply(ctx, st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	in := f.SQLInstances[0]
	if in.DatabaseVersion != "POSTGRES_17" || in.Tier != "db-custom-1-3840" || in.Availability != "REGIONAL" ||
		in.BackupRetention != 7 || in.MaintenanceDay != 3 || in.MaintenanceHour != 22 || in.Name != "silo-db-t3st" {
		t.Errorf("after the update: %+v", in)
	}
	if p := planOf(t, steps(t, f, "--db-version", "POSTGRES_16"), "db-instance"); !p.Failed() ||
		!strings.Contains(p.Changes[0].Why, "only go up") {
		t.Errorf("a version downgrade plans %v", p.Changes)
	}
}

// TestDBAdopt: --db-instance adopts a restored instance: it is labelled,
// the endpoint and DNS move to it, silod migrate runs on it with
// reconcile set to report-only, and the old instance is kept unlabelled
// (state.md "Backups and recovery", SV-54).
func TestDBAdopt(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	if err := install.Apply(ctx, steps(t, f), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	restored(t, f)
	st := steps(t, f, "--db-instance", "silo-db-restored")
	if err := install.Apply(ctx, st, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var old, adopted install.SQLInstance
	for _, in := range f.SQLInstances {
		if in.Name == "silo-db-restored" {
			adopted = in
		} else {
			old = in
		}
	}
	if old.Labels[install.LabelKey] != "" || old.Labels[install.RoleLabel] != "" || !old.DeletionProtection {
		t.Errorf("the old instance: %+v", old)
	}
	if adopted.Labels[install.LabelKey] != install.InstallID(project) || adopted.Labels[install.SchemaLabel] != strconv.Itoa(install.SchemaVersion()) ||
		adopted.Labels[install.RestoredLabel] != "" || !adopted.DeletionProtection {
		t.Errorf("the adopted instance: %+v", adopted)
	}
	if r := f.ForwardingRules["us-central1/silo-db-psc"]; r == nil || r.Target != adopted.ServiceAttachment {
		t.Errorf("rule %+v, want it on %s", r, adopted.ServiceAttachment)
	}
	if z := f.Zones["silo-db"]; z == nil || z.DNSName != adopted.DNSName || f.Records["silo-db/"+adopted.DNSName+"/A"] == nil {
		t.Errorf("zone %+v", z)
	}
	for _, p := range install.PlanAll(ctx, st) {
		if p.Changing() || p.Failed() {
			t.Errorf("after adopting, %s plans %v (%v)", p.Step, p.Changes, p.Err)
		}
	}
	// Without --db-instance, the adopted instance is now the one found
	// by its labels.
	for _, p := range install.PlanAll(ctx, steps(t, f)) {
		if p.Changing() || p.Failed() {
			t.Errorf("after adopting, without --db-instance, %s plans %v (%v)", p.Step, p.Changes, p.Err)
		}
	}
}

// TestMigrateArgs: the VM's silod migrate dials the instance with IAM
// login, as silod's IAM user, and sets report-only reconcile only on
// an adopted instance.
func TestMigrateArgs(t *testing.T) {
	in := &install.SQLInstance{ConnectionName: project + ":us-central1:silo-db-t3st"}
	got := strings.Join(install.MigrateArgs(project, in, 9, false), " ")
	want := "migrate --to 9 --report-guest-attribute --db-instance " + project + ":us-central1:silo-db-t3st --db-name silo --db-user silod@" + project + ".iam"
	if got != want {
		t.Errorf("args %q, want %q", got, want)
	}
	if got := install.MigrateArgs(project, in, 9, true); got[len(got)-1] != "--reconcile-report-only" {
		t.Errorf("restored args %q", got)
	}
}

// TestMigrateFails: silod migrate's error is install's, and the VM is
// deleted either way.
func TestMigrateFails(t *testing.T) {
	ctx := context.Background()
	f := fakecloud.New(project)
	f.MigrateResult = "error store: a contract step would break a running replica: old-vm (v1.0.0, schema 8)"
	err := install.Apply(ctx, steps(t, f), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "old-vm") {
		t.Fatalf("install with migrate failing: %v", err)
	}
	if zone, _ := f.Cloud().ComputeExtras.InstanceZone(ctx, project, "us-central1", install.MigrateVM); zone != "" {
		t.Errorf("%s was left in %s", install.MigrateVM, zone)
	}
	if f.SQLInstances[0].Labels[install.SchemaLabel] != "0" {
		t.Errorf("schema label %q after a failed migration", f.SQLInstances[0].Labels[install.SchemaLabel])
	}
	f.MigrateResult = ""
	if err := install.Apply(ctx, steps(t, f), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
