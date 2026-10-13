package main

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/install"
	"github.com/bwsalmon/silo/pkg/install/fakecloud"
)

// withFakeCloud points silo install at an in-memory project.
func withFakeCloud(t *testing.T) *fakecloud.Fake {
	f := fakecloud.New("silo-test-project")
	old := newCloud
	newCloud = func(ctx context.Context, credentials, version string) (install.Cloud, error) {
		return f.Cloud(), nil
	}
	t.Cleanup(func() { newCloud = old })
	return f
}

// runCLI runs silo with stdin, and returns the status and output.
func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	c := &cli{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errOut, getenv: func(string) string { return "" }}
	code := c.run(context.Background(), args)
	return code, out.String(), errOut.String()
}

var installArgs = []string{"install", "--project", "silo-test-project", "--regions", "us-central1,europe-west4", "--admin", "user:admin@example.com"}

func TestInstallDryRun(t *testing.T) {
	f := withFakeCloud(t)
	code, out, errOut := runCLI(t, "", append(installArgs, "--dry-run")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"silo install: project silo-test-project, home region us-central1", "apis              enable",
		"network           create  silo", "20 steps: 19 to create or apply", "control           create  silo-control", "root              create  / blocks 10.0.0.0/8 (minus 10.255.254.0/23)"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if len(f.Enabled) != 0 {
		t.Error("--dry-run enabled services")
	}

	code, out, _ = runCLI(t, "", append(installArgs, "--dry-run", "--output", "json")...)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 20 {
		t.Fatalf("json: exit %d\n%s", code, out)
	}
	var step struct {
		Step    string
		Action  string
		Changes []map[string]any
	}
	if err := json.Unmarshal([]byte(lines[1]), &step); err != nil || step.Step != "apis" || step.Action != "enable" || len(step.Changes) != len(install.Services) {
		t.Errorf("json step: %+v %v", step, err)
	}
}

func TestInstallAsksFirst(t *testing.T) {
	f := withFakeCloud(t)
	code, _, errOut := runCLI(t, "n\n", installArgs...)
	if code != 1 || !strings.Contains(errOut, "Apply this plan? [y/N]") || len(f.Enabled) != 0 {
		t.Fatalf("declined: exit %d, enabled %v\n%s", code, f.Enabled, errOut)
	}
	code, out, errOut := runCLI(t, "y\n", installArgs...)
	if code != 0 || !strings.Contains(out, "network: done") || !strings.Contains(out, "verify: root /") {
		t.Fatalf("confirmed: exit %d\n%s%s", code, out, errOut)
	}
	// Re-running finds nothing to do, and asks nothing.
	code, out, errOut = runCLI(t, "", installArgs...)
	if code != 0 || !strings.Contains(out, "Nothing to change.") || strings.Contains(errOut, "[y/N]") {
		t.Errorf("re-run: exit %d\n%s%s", code, out, errOut)
	}
	// Uninstall with --yes doesn't ask.
	code, out, errOut = runCLI(t, "", "uninstall", "--project", "silo-test-project", "--yes")
	if code != 0 || !strings.Contains(out, "network: done") || strings.Contains(errOut, "[y/N]") {
		t.Errorf("uninstall: exit %d\n%s%s", code, out, errOut)
	}
}

func TestInstallStopsOnFailedChecks(t *testing.T) {
	f := withFakeCloud(t)
	f.Denied["iam.roles.create"] = true
	code, out, errOut := runCLI(t, "", append(installArgs, "--yes")...)
	if code != 1 || !strings.Contains(out, "missing iam.roles.create") || !strings.Contains(out, "roles/iam.roleAdmin") || len(f.Enabled) != 0 {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestInstallUsage(t *testing.T) {
	withFakeCloud(t)
	for _, args := range [][]string{
		{"install", "--regions", "us-central1"},
		{"install", "--project", "silo-test-project", "--regions", "us-central1", "--output", "json"},
		{"install", "--project", "silo-test-project", "--regions", "us-central1", "extra"},
		{"uninstall"},
	} {
		if code, _, errOut := runCLI(t, "", args...); code != 2 {
			t.Errorf("%v: exit %d\n%s", args, code, errOut)
		}
	}
}

func TestUninstallDeleteDataNamesTheInstance(t *testing.T) {
	f := withFakeCloud(t)
	if code, _, errOut := runCLI(t, "", append(installArgs, "--yes")...); code != 0 {
		t.Fatalf("install: exit %d\n%s", code, errOut)
	}
	name := f.SQLInstances[0].Name
	code, out, errOut := runCLI(t, "n\n", "uninstall", "--project", "silo-test-project", "--delete-data")
	if code != 1 || !strings.Contains(errOut, "including the Cloud SQL instance "+name) || !strings.Contains(out, "db-instance       delete  "+name) {
		t.Fatalf("declined: exit %d\n%s%s", code, out, errOut)
	}
	if len(f.SQLInstances) != 1 || !f.SQLInstances[0].DeletionProtection || len(f.ProtectionChanges) != 0 {
		t.Fatalf("a declined uninstall touched the database: %+v %v", f.SQLInstances, f.ProtectionChanges)
	}
	// A tree with a silo in it stops uninstall without --force.
	f.Silos["/team-a"] = "aaaaaa"
	code, out, errOut = runCLI(t, "", "uninstall", "--project", "silo-test-project", "--delete-data", "--yes")
	if code != 1 || !strings.Contains(out, "silo /team-a") || f.SQLInstances[0].Name != name {
		t.Fatalf("without --force: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "", "uninstall", "--project", "silo-test-project", "--delete-data", "--force", "--yes")
	if code != 0 || len(f.SQLInstances) != 0 || f.FinalBackups[name] != 30 || len(f.Silos) != 0 {
		t.Fatalf("--force --delete-data: exit %d, instances %+v\n%s%s", code, f.SQLInstances, out, errOut)
	}
}

func TestBackendFlag(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		backend string
		rest    []string
	}{
		{[]string{"--project", "p"}, "", []string{"--project", "p"}},
		{[]string{"--backend", "local", "--system"}, "local", []string{"--system"}},
		{[]string{"--dry-run", "--backend=local"}, "local", []string{"--dry-run"}},
		{[]string{"-backend=gcp", "--project", "p"}, "gcp", []string{"--project", "p"}},
	} {
		b, rest := backendFlag(tc.args)
		if b != tc.backend || !slices.Equal(rest, tc.rest) {
			t.Errorf("backendFlag(%q) = %q, %q; want %q, %q", tc.args, b, rest, tc.backend, tc.rest)
		}
	}
}
