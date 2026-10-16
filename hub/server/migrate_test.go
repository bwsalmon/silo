package server

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/tasks"
	"github.com/bwsalmon/silo/internal/pgtest"
)

// TestMigrateThenServeLeastPrivileged is the deployment's split
// (docs/deploy.md, SA1-L8) on a throwaway server: `silo hub migrate`
// runs as the role that owns the database, granting a second role the
// hub's rows, and the hub serves as that second role. It can read and
// write every table, including one a later migration adds, and can
// change none of them; a role that could is refused.
func TestMigrateThenServeLeastPrivileged(t *testing.T) {
	ctx := context.Background()
	pg := pgtest.Start(t)
	admin := sqlDB(t, pg.URL)
	for _, stmt := range []string{
		`CREATE ROLE "silo-hub" LOGIN`,
		`CREATE ROLE "silo-hub@p.iam" LOGIN`,
		`CREATE ROLE creator LOGIN CREATEDB`,
		`CREATE DATABASE silo OWNER "silo-hub"`,
		// PostgreSQL before 15 let everyone create in public; Cloud
		// SQL's are 16.
		`REVOKE CREATE ON SCHEMA public FROM PUBLIC`,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	url := func(user string) string {
		return fmt.Sprintf("postgres://%s@127.0.0.1:%d/silo?sslmode=disable", strings.ReplaceAll(user, "@", "%40"), pg.Port)
	}
	owner, runtime := url("silo-hub"), url("silo-hub@p.iam")
	cfg := Config{
		Database:      runtime,
		NoMigrate:     true,
		DevUserHeader: userHeader,
		LeaseRetry:    100 * time.Millisecond,
		ShutdownGrace: 10 * time.Second,
		Logf:          logf(t),
	}

	if _, err := New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "silo hub migrate") {
		t.Fatalf("serving before the migration: %v, want an error naming silo hub migrate", err)
	}
	if err := Migrate(ctx, owner, "silo-hub@p.iam"); err != nil {
		t.Fatal(err)
	}
	// Again, as each start of the pod does.
	if err := Migrate(ctx, owner, "silo-hub@p.iam"); err != nil {
		t.Fatalf("migrating again: %v", err)
	}

	// The hub serves as the runtime role: each store reads and writes,
	// and the dispatcher takes its lease.
	h, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.Serve(sctx, l) }()
	defer func() {
		stop()
		<-done
		h.Close()
	}()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(h.Directory.CreateTeam(ctx, "infra", "Infra"))
	must(h.Directory.SetMember(ctx, "infra", "alice@example.com", directory.Admin))
	must(h.Directory.AssignRepo(ctx, "infra", "bwsalmon/silo"))
	must(h.Directory.SetTeamSettings(ctx, "infra", directory.Settings{LostRequeueAfter: time.Hour}))
	_, _, err = h.Directory.MintNodeToken(ctx, "alice@example.com", "ws-1")
	must(err)
	task, err := h.Backlog.Create(ctx, tasks.Spec{Team: "infra", Repo: "bwsalmon/silo", Submitter: "alice@example.com", Prompt: "p"})
	must(err)
	e, class, err := live.Classify([]byte(`{"v":1,"seq":1,"type":"status","task":"` + task.ID + `","status":{"phase":"setup","step":"tools","state":"done"}}`))
	must(err)
	must(h.Events.Append(ctx, e, class))
	eventually(t, "the dispatcher to take its lease", func() bool {
		return strings.Contains(scrape(t, h), "\nsilo_hub_dispatcher_leader 1\n")
	})

	// No DDL, on the hub's tables or new ones.
	rt := sqlDB(t, runtime)
	for _, stmt := range []string{
		`CREATE TABLE other (a INTEGER)`,
		`CREATE INDEX tasks_prompt ON tasks (prompt)`,
		`ALTER TABLE tasks ADD COLUMN x INTEGER`,
		`DROP TABLE task_events`,
		`TRUNCATE node_tokens`,
		`CREATE SCHEMA other`,
	} {
		if _, err := rt.ExecContext(ctx, stmt); err == nil {
			t.Errorf("the runtime role ran %s", stmt)
		}
	}

	// A table a later migration adds is the runtime role's to use.
	ow := sqlDB(t, owner)
	if _, err := ow.ExecContext(ctx, `CREATE TABLE later (id BIGSERIAL PRIMARY KEY, a TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.ExecContext(ctx, `INSERT INTO later (a) VALUES ('x')`); err != nil {
		t.Errorf("the runtime role on a later table: %v", err)
	}

	// Roles that could change the schema are refused.
	for _, role := range []string{"silo-hub", "postgres", "creator"} {
		if err := Migrate(ctx, owner, role); err == nil {
			t.Errorf("runtime role %s was accepted", role)
		}
	}
}

// TestMigrateSQLite: SQLite has one URL and no roles. `silo hub
// migrate` makes the tables a hub with NoMigrate then serves from, and
// refuses a runtime role.
func TestMigrateSQLite(t *testing.T) {
	ctx := context.Background()
	db := "sqlite:" + filepath.Join(t.TempDir(), "hub.db")
	cfg := Config{Database: db, NoMigrate: true, DevUserHeader: userHeader, Logf: logf(t)}
	if _, err := New(ctx, cfg); err == nil {
		t.Fatal("served an empty database without migrating it")
	}
	if err := Migrate(ctx, db, "app"); err == nil {
		t.Error("a runtime role was accepted on SQLite")
	}
	if err := Migrate(ctx, db, ""); err != nil {
		t.Fatal(err)
	}
	h, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.Directory.CreateTeam(ctx, "infra", "Infra"); err != nil {
		t.Fatal(err)
	}
}

func sqlDB(t *testing.T, url string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
