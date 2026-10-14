//go:build silo_conformance

package conformance_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance/gcpharness"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// The real-GCP harness is pkg/conformance/gcpharness, shared with the
// "gcp" target of pkg/conformance/targets. These are the names the cases
// here know it by.
var (
	run         = gcpharness.Run
	facade      = gcpharness.Facade
	waitOp      = gcpharness.WaitOp
	tail        = gcpharness.Tail
	lastSegment = gcpharness.LastSegment
)

func TestMain(m *testing.M) { gcpharness.Main(m) }

// installed skips a case that has no tree to test (gcpharness.Installed).
func installed(t *testing.T) {
	t.Helper()
	gcpharness.Installed(t)
}

// installChecked skips install's own checks in a binary that attached
// to a handed-on install: they were the installing binary's to make.
func installChecked(t *testing.T) {
	t.Helper()
	if run.Attached {
		t.Skip("attached to a handed-on install (-silo.attach): the installing run checked install")
	}
}

// TestInstall reports the install itself (SV-97): that it finished, used
// Cloud SQL with a PSC endpoint and IAM login as in production, and that
// the silo-migrate VM came and went.
func TestInstall(t *testing.T) {
	installChecked(t)
	t.Run("Finished", func(t *testing.T) {
		if run.InstallErr != nil {
			t.Fatalf("%v\n%s", run.InstallErr, tail(run.InstallOut, 6000))
		}
	})
	t.Run("CloudSQLWithPSCAndIAMLogin", func(t *testing.T) {
		installed(t)
		ctx := context.Background()
		l, err := run.SQL.Instances.List(run.Project).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		var db *sqladmin.DatabaseInstance
		for _, o := range l.Items {
			if o.Settings != nil && o.Settings.UserLabels["silo-install"] != "" {
				db = o
			}
		}
		if db == nil {
			t.Fatal("no Cloud SQL instance labelled silo-install")
		}
		ip := db.Settings.IpConfiguration
		if ip == nil || ip.PscConfig == nil || !ip.PscConfig.PscEnabled {
			t.Errorf("%s: PSC isn't on", db.Name)
		}
		if ip != nil && ip.Ipv4Enabled {
			t.Errorf("%s has a public IP", db.Name)
		}
		iam := false
		for _, f := range db.Settings.DatabaseFlags {
			if f.Name == "cloudsql.iam_authentication" && f.Value == "on" {
				iam = true
			}
		}
		if !iam {
			t.Errorf("%s: cloudsql.iam_authentication isn't on", db.Name)
		}
		if db.Settings.Tier != "db-f1-micro" {
			t.Errorf("%s: tier %s, want db-f1-micro", db.Name, db.Settings.Tier)
		}
		users, err := run.SQL.Users.List(run.Project, db.Name).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, u := range users.Items {
			if u.Type == "CLOUD_IAM_SERVICE_ACCOUNT" && strings.HasPrefix(u.Name, "silod@") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no IAM user for silod", db.Name)
		}
		if _, err := run.Compute.ForwardingRules.Get(run.Project, run.Region, "silo-db-psc").Context(ctx).Do(); err != nil {
			t.Errorf("the PSC endpoint silo-db-psc: %v", err)
		}
	})
	t.Run("MigrateVMCameAndWent", func(t *testing.T) {
		installed(t)
		if !run.MigrateSeen {
			t.Error("silo-migrate never appeared while install ran")
		}
		_, err := run.Compute.Instances.Get(run.Project, run.Zone, "silo-migrate").Do()
		if err == nil {
			t.Error("silo-migrate is still there after install")
		}
	})
}
