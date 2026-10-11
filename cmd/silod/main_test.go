package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/migrations"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
	compute "google.golang.org/api/compute/v1"
)

func TestSchemaCheck(t *testing.T) {
	o := migrations.Oldest
	for _, c := range []struct {
		v, requires, latest int
		want                string
	}{
		{o, 0, o, ""},
		{o - 1, 0, o, "schema_mismatch"},
		// A newer schema is served while every step past this release
		// is an expand step: the old release keeps running through an
		// upgrade, and a rollback runs on the newer schema.
		{o + 1, 0, o, ""},
		{o + 2, o, o, ""},
		// A contract step that needs a newer release than this one.
		{o + 2, o + 1, o, "schema_mismatch"},
		{0, 0, 9, "schema_mismatch"},
	} {
		if got := schemaCheck(c.v, c.requires, c.latest); got != c.want {
			t.Errorf("schemaCheck(%d, %d, %d) = %q, want %q", c.v, c.requires, c.latest, got, c.want)
		}
	}
}

func TestMigrateReportsGuestAttribute(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || r.URL.Path != "/computeMetadata/v1/instance/guest-attributes/"+MigrateAttribute ||
			r.Header.Get("Metadata-Flavor") != "Google" {
			t.Errorf("%s %s %v", r.Method, r.URL.Path, r.Header)
		}
		got <- string(b)
	}))
	defer srv.Close()
	old := metadataHost
	metadataHost = srv.URL
	defer func() { metadataHost = old }()
	path := filepath.Join(t.TempDir(), "silo.db")
	if err := migrate(context.Background(), []string{"--store=sqlite:" + path, "--to", "3", "--report-guest-attribute"}); err != nil {
		t.Fatal(err)
	}
	if v := <-got; v != "ok 3" {
		t.Errorf("guest attribute %q, want ok 3", v)
	}
	if err := migrate(context.Background(), []string{"--store=sqlite:" + path, "--to", "2", "--report-guest-attribute"}); err == nil {
		t.Fatal("migrating backwards succeeded")
	}
	if v := <-got; !strings.HasPrefix(v, "error ") || !strings.Contains(v, "forward") {
		t.Errorf("guest attribute %q, want the error", v)
	}
	if err := migrate(context.Background(), []string{"--store=sqlite:" + path, "--reconcile-report-only"}); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if mode, ok, err := store.GetSetting(context.Background(), db, store.SettingReconcile); err != nil || !ok || mode != store.ReconcileReportOnly {
		t.Errorf("reconcile = %q, %v, %v; want report-only", mode, ok, err)
	}
}

func TestPostgresNeedsADatabase(t *testing.T) {
	t.Setenv("SILOD_DATABASE_URL", "")
	t.Setenv("SILOD_DB_INSTANCE", "")
	if _, _, err := openStore(context.Background(), storeFlags{spec: "postgres"}); err == nil || !strings.Contains(err.Error(), "--db-instance") {
		t.Errorf("openStore with no database: %v", err)
	}
	if _, _, err := openStore(context.Background(), storeFlags{spec: "postgres", url: "postgres://x/y", instance: "p:r:i"}); err == nil {
		t.Error("openStore took both --database-url and --db-instance")
	}
	if _, _, err := openStore(context.Background(), storeFlags{spec: "postgres", instance: "p:r:i", name: "silo"}); err == nil || !strings.Contains(err.Error(), "user") {
		t.Errorf("openStore on Cloud SQL with no user: %v", err)
	}
}

func TestMigrateRefusedOnPostgres(t *testing.T) {
	err := serve(context.Background(), []string{"--store=postgres", "--database-url=postgres://nowhere.invalid/x", "--migrate", "--listen=127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "refused on postgres") {
		t.Fatalf("serve --migrate on postgres: %v", err)
	}
}

// get calls path on the Unix socket, returning the dial or request
// error rather than discarding it, so a caller trying to tell a slow
// start from a real failure has something to go on.
func get(t *testing.T, sock, path string) (int, map[string]any, error) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := c.Get("http://silod" + path)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, nil
}

func TestServe(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "silo.db")
	run := func(args ...string) (string, context.CancelFunc, chan error) {
		sock := filepath.Join(dir, "silod.sock")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- serve(ctx, append([]string{"--store=sqlite:" + db, "--socket=" + sock}, args...)) }()
		return sock, cancel, done
	}
	// wait polls for a response, but gives up the moment silod exits
	// on its own -- reporting that exit error, which is always more
	// informative than "never answered" -- and otherwise reports the
	// last dial error once the budget (generous, for a CI host
	// sharing a machine with other jobs) runs out.
	wait := func(sock string, done chan error) (int, map[string]any) {
		deadline := time.After(10 * time.Second)
		var lastErr error
		for {
			select {
			case err := <-done:
				t.Fatalf("silod exited before answering: %v", err)
			case <-deadline:
				t.Fatalf("silod never answered: %v", lastErr)
			default:
			}
			code, out, err := get(t, sock, "/silod/health")
			if err == nil {
				return code, out
			}
			lastErr = err
			time.Sleep(10 * time.Millisecond)
		}
	}
	stopped := func(cancel context.CancelFunc, done chan error) {
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	// An empty database is out of range: health says so.
	sock, cancel, done := run()
	if code, out := wait(sock, done); code != 503 || out["status"] != "schema_mismatch" {
		t.Errorf("unmigrated health: %d %v", code, out)
	}
	stopped(cancel, done)

	sock, cancel, done = run("--migrate", "--dev-caller-header")
	if code, out := wait(sock, done); code != 200 || out["status"] != "ok" {
		t.Errorf("migrated health: %d %v", code, out)
	}
	if code, out, err := get(t, sock, "/silo/v1/self"); err != nil || code != 403 {
		t.Errorf("no caller: %d %v %v", code, out, err)
	}
	stopped(cancel, done)
}

// TestOneReplicaOnSQLite: SQLite isn't shared, so silod refuses more
// than one replica on it (state.md "More than one silod, and the
// leader", SV-52); and --reconcile takes only report-only.
func TestOneReplicaOnSQLite(t *testing.T) {
	db := filepath.Join(t.TempDir(), "silo.db")
	err := serve(context.Background(), []string{"--store=sqlite:" + db, "--listen=127.0.0.1:0", "--control-replicas=2"})
	if err == nil || !strings.Contains(err.Error(), "not shared") {
		t.Errorf("--control-replicas=2 on sqlite: %v", err)
	}
	err = serve(context.Background(), []string{"--store=sqlite:" + db, "--listen=127.0.0.1:0", "--reconcile=delete-everything"})
	if err == nil || !strings.Contains(err.Error(), "report-only") {
		t.Errorf("--reconcile=delete-everything: %v", err)
	}
}

// TestDevFakeHasHostVPC is part 1's finding 2 (docs/test-runs/
// 2026-10-06-silo-v1-part1.md): --fake-gce's fake had no host VPC, so
// every subnetwork the facade made failed with network not found.
func TestDevFakeHasHostVPC(t *testing.T) {
	ctx := context.Background()
	f, err := devFake(ctx, "hp")
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.InsertSubnetwork(ctx, "hp", "us-central1", &compute.Subnetwork{
		Name: "s", Network: "projects/hp/global/networks/silo", IpCidrRange: "10.0.0.0/24",
	})
	if err != nil {
		t.Fatal(err)
	}
	if op, err = f.WaitOperation(ctx, "hp", gce.ScopeOf(op), op.Name); err != nil || op.Error != nil {
		t.Fatalf("subnetwork insert in the host VPC: %v, %+v", err, op.Error)
	}
}

// TestGCPOptionsKeepQuotasOnTheFake: with --compute-endpoint on a fake,
// the root capacity refresh read Cloud Quotas from Google.
func TestGCPOptionsKeepQuotasOnTheFake(t *testing.T) {
	if o := gcpOptions("http://127.0.0.1:18090/compute/v1/"); o.CloudQuotasEndpoint != "http://127.0.0.1:18090/" {
		t.Errorf("Cloud Quotas endpoint %q, want the fake's root", o.CloudQuotasEndpoint)
	}
	if o := gcpOptions(""); o.ComputeEndpoint != "" || o.CloudQuotasEndpoint != "" {
		t.Errorf("no --compute-endpoint: %+v, want GCE's and Google's defaults", o)
	}
}
