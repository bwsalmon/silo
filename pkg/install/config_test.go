package install_test

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/install"
)

func parse(args ...string) (install.Config, error) {
	cfg := install.Defaults()
	fs := flag.NewFlagSet("silo install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg.Flags(fs, "v1.2.3")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := parse("--project", "my-project", "--regions", "us-central1, europe-west4", "--admin", "user:a@example.com", "--admin", "serviceAccount:b@p.iam.gserviceaccount.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"address-space", cfg.AddressSpace.String(), "10.0.0.0/8"},
		{"gateway-range", cfg.GatewayRange.String(), "10.255.255.0/24"},
		{"control-range", cfg.ControlRange.String(), "10.255.254.0/24"},
		{"root-grant-policy", cfg.RootGrantPolicy, "exports=allow,links=deny"},
		{"version", cfg.Version, "v1.2.3"},
		{"control-machine-type", cfg.ControlMachineType, "e2-standard-2"},
		{"budget-period", cfg.BudgetPeriod, "month"},
		{"db-tier", cfg.DBTier, "db-g1-small"},
		{"db-version", cfg.DBVersion, "POSTGRES_17"},
		{"db-maintenance-window", cfg.DBMaintenanceWindow, "sun:05:00"},
		{"home region", cfg.HomeRegion(), "us-central1"},
		{"regions", strings.Join(cfg.Regions, " "), "us-central1 europe-west4"},
		{"admins", strings.Join(cfg.Admins, " "), "user:a@example.com serviceAccount:b@p.iam.gserviceaccount.com"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if cfg.ControlReplicas != 1 || cfg.DBBackupRetention != 14 || cfg.RootMemoryMB != 0 || cfg.AgentSessions != 0 || cfg.DBHA || cfg.Yes || cfg.DryRun {
		t.Errorf("numeric or boolean defaults: %+v", cfg)
	}
}

func TestConfigRootMemory(t *testing.T) {
	for in, want := range map[string]int64{"unlimited": 0, "4096": 4096, "512MB": 512, "64GB": 65536} {
		cfg, err := parse("--project", "my-project", "--regions", "us-central1", "--root-memory", in)
		if err != nil || cfg.RootMemoryMB != want {
			t.Errorf("--root-memory %s = %d, %v; want %d", in, cfg.RootMemoryMB, err, want)
		}
	}
	if _, err := parse("--project", "my-project", "--regions", "us-central1", "--root-memory", "lots"); err == nil {
		t.Error("--root-memory lots was accepted")
	}
}

func TestConfigRejects(t *testing.T) {
	// Every problem is reported at once.
	_, err := parse("--regions", "us-central1,us-central1", "--admin", "bob", "--gateway-range", "192.168.0.0/24",
		"--control-replicas", "0", "--budget-period", "year", "--db-version", "MYSQL_8_0", "--db-maintenance-window", "sun:05:30",
		"--image", "example/silod:latest", "--root-grant-policy", "exports=maybe", "--db-backup-retention", "0", "--output", "yaml")
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"--project is required", "us-central1 is given twice", `--admin "bob"`,
		"--gateway-range 192.168.0.0/24 is not inside --address-space", "--control-replicas", "--budget-period",
		"--db-version", "on the hour", "--image", "--root-grant-policy", "--db-backup-retention", "--output"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if _, err := parse("--project", "my-project", "--regions", "us-central1", "--address-space", "10.0.0.1/8"); err == nil || !strings.Contains(err.Error(), "did you mean 10.0.0.0/8") {
		t.Errorf("host bits: %v", err)
	}
	if _, err := parse("--project", "my-project", "--regions", "us-central1", "--control-range", "10.255.255.128/25"); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Errorf("overlapping ranges: %v", err)
	}
}

func TestMaintenanceWindow(t *testing.T) {
	day, hour, err := install.MaintenanceWindow("sun:05:00")
	if err != nil || day != 7 || hour != 5 {
		t.Errorf("sun:05:00 = %d %d %v", day, hour, err)
	}
	for _, bad := range []string{"sunday:05:00", "mon:24:00", "mon:5:00", "mon:05", "mon:05:15"} {
		if _, _, err := install.MaintenanceWindow(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestInstallIDIsStableAndALabelValue(t *testing.T) {
	a, b := install.InstallID("p-1"), install.InstallID("p-1")
	if a != b || a == install.InstallID("p-2") || len(a) > 63 || strings.ToLower(a) != a {
		t.Errorf("InstallID: %q %q", a, b)
	}
}

func TestSessionsPerVM(t *testing.T) {
	for _, c := range []struct {
		cpus, mem int64
		want      int
	}{{2, 8192, 5}, {4, 16384, 15}, {2, 4096, 4}, {1, 1024, 0}, {8, 32768, 35}} {
		if got := install.SessionsPerVM(c.cpus, c.mem); got != c.want {
			t.Errorf("SessionsPerVM(%d, %d) = %d, want %d", c.cpus, c.mem, got, c.want)
		}
	}
}

func TestRenderMatchesInstallMD(t *testing.T) {
	plans := []install.StepPlan{
		{Step: "prereqs", Changes: []install.Change{{Action: install.ActionOK, Detail: "billing on; CPUS us-central1 24/96 used; NETWORKS 1/15"}}},
		{Step: "apis", Changes: []install.Change{
			{Action: install.ActionEnable, Object: "compute.googleapis.com", Estimate: 15 * time.Second},
			{Action: install.ActionEnable, Object: "dns.googleapis.com", Estimate: 15 * time.Second},
			{Action: install.ActionEnable, Object: "iap.googleapis.com", Estimate: 15 * time.Second},
			{Action: install.ActionEnable, Object: "sqladmin.googleapis.com", Estimate: 15 * time.Second},
			{Action: install.ActionEnable, Object: "cloudquotas.googleapis.com", Estimate: 15 * time.Second},
		}},
		{Step: "db-instance", Changes: []install.Change{{Action: install.ActionCreate, Object: "silo-db-x4kq",
			Detail: "POSTGRES_17 db-g1-small zonal us-central1", Estimate: 8 * time.Minute, Waiting: "Cloud SQL"}}},
		{Step: "network", Changes: []install.Change{{Action: install.ActionOK, Object: "silo"}}},
	}
	got := render(plans)
	want := `silo install: project silo-test-project

  prereqs           ok      billing on; CPUS us-central1 24/96 used; NETWORKS 1/15
  apis              enable  compute.googleapis.com, dns.googleapis.com, iap.googleapis.com,
                            sqladmin.googleapis.com, cloudquotas.googleapis.com
  db-instance       create  silo-db-x4kq POSTGRES_17 db-g1-small zonal us-central1
  network           ok      silo

  4 steps: 2 to create or apply, 0 to update, 2 ok
  Estimated time: about 10 minutes, most of it waiting for Cloud SQL.
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	var b strings.Builder
	if err := install.RenderJSON(&b, plans); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != len(plans) || !strings.HasPrefix(lines[1], `{"step":"apis","action":"enable","changes":[{"action":"enable","object":"compute.googleapis.com"}`) {
		t.Errorf("json:\n%s", b.String())
	}
}
