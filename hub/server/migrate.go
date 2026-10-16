package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/bwsalmon/silo/hub/dbaccess"
	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/lease"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/tasks"
)

// Migrate creates the stores' tables at url that are missing, as url's
// role, which becomes their owner: `silo hub migrate`, run before the
// hub serves with NoMigrate as a role that cannot create them
// (docs/deploy.md, SA1-L8). When runtimeRole is set, url must be
// PostgreSQL's, and Migrate then grants runtimeRole what the hub needs
// to serve, on these tables and on any a later migration makes
// (dbaccess.Grant), and fails if runtimeRole could change the schema
// anyway.
func Migrate(ctx context.Context, url, runtimeRole string) error {
	postgres := strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://")
	if runtimeRole != "" && !postgres {
		return errors.New("server: a runtime role needs PostgreSQL; SQLite has no roles")
	}
	d, err := directory.Open(ctx, url)
	if err != nil {
		return fmt.Errorf("server: migrating the team directory: %w", err)
	}
	d.Close()
	b, err := tasks.Open(ctx, url)
	if err != nil {
		return fmt.Errorf("server: migrating the backlog: %w", err)
	}
	b.Close()
	e, err := live.Open(ctx, url)
	if err != nil {
		return fmt.Errorf("server: migrating the event store: %w", err)
	}
	e.Close()
	l, err := lease.Open(ctx, url)
	if err != nil {
		return fmt.Errorf("server: migrating the leases: %w", err)
	}
	l.Close()
	if runtimeRole == "" {
		return nil
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()
	return dbaccess.Grant(ctx, db, runtimeRole)
}
