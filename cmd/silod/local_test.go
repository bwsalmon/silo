package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/hostcap"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// TestBackendLocalFlags: --backend=local runs only on the data
// directory's SQLite store, and without the GCP backend's flags.
func TestBackendLocalFlags(t *testing.T) {
	ctx := context.Background()
	data := filepath.Join(t.TempDir(), "data")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--backend=local", "--store=sqlite:" + filepath.Join(t.TempDir(), "x.db")}, "--data-dir"},
		{[]string{"--backend=local", "--data-dir=" + data, "--fence-project=p"}, "GCP backend's"},
		{[]string{"--backend=nope", "--store=sqlite:" + filepath.Join(t.TempDir(), "x.db")}, "want gcp or local"},
		{[]string{"--ambient-agents", "--store=sqlite:" + filepath.Join(t.TempDir(), "x.db")}, "local backend's"},
		{[]string{"--backend=local", "--data-dir=" + data, "--ambient-agents", "--fake-agents"}, "not with --sessiond-socket or --fake-agents"},
	} {
		err := serve(ctx, append(c.args, "--listen=127.0.0.1:0"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("serve %v: %v, want an error naming %q", c.args, err, c.want)
		}
	}
}

func TestLocalBackend(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background(), db.Latest()); err != nil {
		t.Fatal(err)
	}
	if _, err := parseLocalGateway("10.255.255.1"); err == nil {
		t.Errorf("one gateway address was taken")
	}
	if _, err := parseLocalGateway("10.255.255.1,fd00::2"); err == nil {
		t.Errorf("an IPv6 gateway address was taken")
	}
	names, err := parseLocalGateway("10.255.255.1, 10.255.255.2")
	if err != nil {
		t.Fatal(err)
	}
	be, err := localBackend(db, nil, "/nowhere/nsd.state", "nft", names, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if be.Fence == nil {
		t.Fatal("no fence")
	}
	// With no holder, an apply fails saying so rather than touching the
	// host's own tables.
	if _, err := be.Fence.Repair(context.Background()); err == nil || !strings.Contains(err.Error(), "no holder") {
		t.Errorf("Repair with no holder: %v", err)
	}
	if be.Capacity != nil {
		t.Errorf("a capacity refresh with no config")
	}

	// With a config, the leader refreshes the root's capacity from the
	// host hourly; before bootstrap there is no root, which isn't an
	// error. A replica that isn't leader-led runs no refresh.
	capacity := &hostcap.Config{DataDir: t.TempDir()}
	if be, err = localBackend(db, nil, "/nowhere/nsd.state", "nft", names, true, nil, capacity); err != nil {
		t.Fatal(err)
	}
	if be.Capacity == nil {
		t.Fatal("no capacity refresh")
	}
	var found bool
	for _, d := range leaderDuties(db, leaderConfig{backend: be}) {
		if d.Name == "root capacity" {
			found = true
			if err := d.Run(context.Background()); err != nil {
				t.Errorf("root capacity before bootstrap: %v", err)
			}
		}
	}
	if !found {
		t.Errorf("no root capacity duty")
	}
	if be, err = localBackend(db, nil, "/nowhere/nsd.state", "nft", names, false, nil, capacity); err != nil || be.Capacity != nil {
		t.Errorf("not leader-led: capacity %v, %v", be.Capacity, err)
	}
}
