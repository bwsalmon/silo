package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/internal/pgtest"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/migrations"
)

// TestContractStepWaitsForReplicas: silod migrate refuses a contract
// step while a replica of a release older than it allows is
// heartbeating, applies it once that replica is gone, and records it,
// after which the older release no longer passes the schema check
// (state.md "How a running fleet upgrades", SV-53).
func TestContractStepWaitsForReplicas(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, pgtest.Start(t))
	latest := s.Latest()
	// Release N carries the schema up to latest. N+1 adds a contract
	// step that only releases carrying latest+1 survive.
	s.ms = append(append([]migrations.Migration(nil), s.ms...), migrations.Migration{
		Version: latest + 1, Name: "contract.sql", Requires: latest + 1,
		SQL: "DROP TABLE replicas_unused_by_anyone",
	})
	if _, err := s.db.Exec(`CREATE TABLE replicas_unused_by_anyone (a TEXT)`); err != nil {
		t.Fatal(err)
	}
	old := store.Replica{ID: "old-vm", Release: "vN", Latest: latest, Started: time.Now()}
	if err := store.Heartbeat(ctx, s, old); err != nil {
		t.Fatal(err)
	}
	err := s.Migrate(ctx, latest+1)
	if !errors.Is(err, store.ErrContract) {
		t.Fatalf("Migrate past a contract step with release N running: %v", err)
	}
	if v, _ := s.Schema(ctx); v != latest {
		t.Fatalf("schema at %d after a refused contract step", v)
	}
	// A replica of N+1 doesn't hold it up; a silent N replica doesn't
	// either.
	if err := store.Heartbeat(ctx, s, store.Replica{ID: "new-vm", Release: "vN+1", Latest: latest + 1, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE replicas SET heartbeat_at = heartbeat_at - 31000 WHERE id = 'old-vm'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx, latest+1); err != nil {
		t.Fatalf("Migrate once release N has stopped: %v", err)
	}
	requires, err := s.Contracts(ctx)
	if err != nil || requires != latest+1 {
		t.Fatalf("Contracts = %d (%v), want %d", requires, err, latest+1)
	}
	if why := store.SchemaCheck(latest+1, requires, migrations.Oldest, latest); why == "" {
		t.Error("release N would still serve after the contract step")
	}
	if why := store.SchemaCheck(latest+1, requires, migrations.Oldest, latest+1); why != "" {
		t.Errorf("release N+1 won't serve: %s", why)
	}
}
