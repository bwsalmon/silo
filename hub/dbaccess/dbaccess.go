// Package dbaccess splits the hub's database between two roles
// (docs/deploy.md, SA1-L8): one that owns the schema and migrates it
// (`silo hub migrate`), and a least-privileged one the hub serves as,
// which may read and write the hub's rows and nothing more: no DDL, no
// other schema's tables. The stores' Open creates their tables, as the
// first; their OpenExisting creates nothing and checks, with Check,
// that the tables are there, as the second. Grant, run as the first,
// gives the second its privileges, on the tables there are and on the
// ones a later migration makes.
//
// SQLite, for development, has no roles: there one URL does both.
package dbaccess

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Check fails unless each of tables exists and db's role may read it.
// store names the store in the error.
func Check(ctx context.Context, db *sql.DB, store string, tables ...string) error {
	for _, t := range tables {
		if _, err := db.ExecContext(ctx, "SELECT 1 FROM "+t+" WHERE 1 = 0"); err != nil {
			return fmt.Errorf("%s: table %s is missing or unreadable; create it with `silo hub migrate` as the schema's owner: %w", store, t, err)
		}
	}
	return nil
}

// Column is a column added to a table after the table was first made,
// with the statement that adds it to a table made before it.
type Column struct {
	Table, Name string
	// Add is the ALTER TABLE ... ADD COLUMN that adds it, on both SQLite
	// and PostgreSQL.
	Add string
}

func (c Column) exists(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "SELECT "+c.Name+" FROM "+c.Table+" WHERE 1 = 0")
	return err
}

// AddColumns adds each of cols its table lacks, as the schema's owner.
// SQLite has no ADD COLUMN IF NOT EXISTS, so each is looked for first.
func AddColumns(ctx context.Context, db *sql.DB, store string, cols ...Column) error {
	for _, c := range cols {
		if c.exists(ctx, db) == nil {
			continue
		}
		if _, err := db.ExecContext(ctx, c.Add); err != nil {
			return fmt.Errorf("%s: adding column %s.%s: %w", store, c.Table, c.Name, err)
		}
	}
	return nil
}

// CheckColumns fails unless each of cols exists and db's role may read
// it: the tables were made by an older `silo hub migrate`.
func CheckColumns(ctx context.Context, db *sql.DB, store string, cols ...Column) error {
	for _, c := range cols {
		if err := c.exists(ctx, db); err != nil {
			return fmt.Errorf("%s: column %s.%s is missing or unreadable; add it with `silo hub migrate` as the schema's owner: %w", store, c.Table, c.Name, err)
		}
	}
	return nil
}

// Grant gives role, on PostgreSQL, exactly what the hub needs to
// serve: USAGE on the current schema, SELECT, INSERT, UPDATE and DELETE
// on every table in it and use of its sequences, and the same by
// default on every table and sequence db's role creates there later.
// db is connected as the role that ran the migration (and so owns the
// tables). Grant then refuses a role that could change the schema
// anyway: a superuser, a role that may create roles or databases, a
// member of Cloud SQL's cloudsqlsuperuser, a role that may create in
// the schema or the database, or one that owns, or is a member of the
// owner of, any of the schema's tables. Grant is idempotent.
func Grant(ctx context.Context, db *sql.DB, role string) error {
	if role == "" {
		return fmt.Errorf("dbaccess: no runtime role")
	}
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return fmt.Errorf("dbaccess: %w", err)
	}
	r, s := pgx.Identifier{role}.Sanitize(), pgx.Identifier{schema}.Sanitize()
	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA ` + s + ` TO ` + r,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + s + ` TO ` + r,
		`GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA ` + s + ` TO ` + r,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA ` + s + ` GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ` + r,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA ` + s + ` GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO ` + r,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("dbaccess: granting %s its privileges: %w", role, err)
		}
	}
	return LeastPrivileged(ctx, db, role)
}

// LeastPrivileged fails if role, on PostgreSQL, could change the
// current schema (see Grant).
func LeastPrivileged(ctx context.Context, db *sql.DB, role string) error {
	var (
		powerful, createSchema, createDB, owns bool
	)
	err := db.QueryRowContext(ctx, `SELECT
	EXISTS (SELECT 1 FROM pg_roles g WHERE pg_has_role($1::name, g.oid, 'MEMBER')
		AND (g.rolsuper OR g.rolcreaterole OR g.rolcreatedb OR g.rolname = 'cloudsqlsuperuser')),
	has_schema_privilege($1::name, current_schema(), 'CREATE'),
	has_database_privilege($1::name, current_database(), 'CREATE'),
	EXISTS (SELECT 1 FROM pg_tables t WHERE t.schemaname = current_schema()
		AND pg_has_role($1::name, t.tableowner, 'MEMBER'))`, role).Scan(&powerful, &createSchema, &createDB, &owns)
	if err != nil {
		return fmt.Errorf("dbaccess: checking %s: %w", role, err)
	}
	switch {
	case powerful:
		return fmt.Errorf("dbaccess: runtime role %s is, or is a member of, a superuser, cloudsqlsuperuser, or a role that may create roles or databases", role)
	case createSchema:
		return fmt.Errorf("dbaccess: runtime role %s may create objects in the schema", role)
	case createDB:
		return fmt.Errorf("dbaccess: runtime role %s may create schemas in the database", role)
	case owns:
		return fmt.Errorf("dbaccess: runtime role %s owns, or is a member of the owner of, the hub's tables", role)
	}
	return nil
}

// Spanner reports whether db is a Cloud Spanner PostgreSQL-dialect
// database, reached through PGAdapter (docs/spanner-evaluation.md),
// rather than PostgreSQL or SQLite. Only Spanner has the
// database_dialect option.
func Spanner(ctx context.Context, db *sql.DB) bool {
	var dialect string
	err := db.QueryRowContext(ctx, `SELECT option_value FROM information_schema.database_options WHERE option_name = 'database_dialect'`).Scan(&dialect)
	return err == nil && dialect == "POSTGRESQL"
}
