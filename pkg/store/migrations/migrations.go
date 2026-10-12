// Package migrations carries silod's schema: numbered SQL files, one
// directory per backend, embedded in the binary (docs/silo/state.md,
// SV-53).
//
// Each file is named NNNN_name.sql, NNNN its version. Versions start at
// 1 and have no gaps, and sqlite/ and postgres/ have the same versions,
// so a version means the same schema on either backend. A file holds
// the shared subset of SQL that package store describes, except where
// the two backends' DDL has to differ. Migrations only go forward: a
// released file is never edited, and a change to the schema is a new
// file in both directories.
//
// The applied version is recorded in schema_migrations, which the
// migration runner creates itself, so version 0 is an empty database.
//
// A later task that adds tables adds a new number here; it does not
// edit an earlier one.
//
// # Expand, then contract
//
// A migration shipped in release N+1 must leave the schema usable by
// release N (state.md "Schema migrations and upgrades", SV-53): it may
// add tables, nullable or defaulted columns and indexes, and nothing
// else. Those are expand steps. A step that drops, renames or tightens
// something an older release uses is a contract step, and says so on a
// line of its own:
//
//	-- silo:contract requires=<version>
//
// where <version> is the newest schema of the oldest release that runs
// on the schema after it: every release whose own newest migration
// (Latest) is below it still uses what the step takes away. silod
// migrate refuses the step while a replica of such a release is
// heartbeating, and records the requirement in schema_contracts, where
// silod serve reads it: a release runs against any schema from its
// Oldest up to the first contract step that requires a newer release
// than itself. Its newest schema is therefore its own Latest plus every
// expand step after it, which it can't name in advance but the
// database records.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed sqlite/*.sql postgres/*.sql
var files embed.FS

// Backends are the directories migrations are kept in.
var Backends = []string{"sqlite", "postgres"}

// Migration is one numbered schema step.
type Migration struct {
	Version int
	Name    string // the file name
	SQL     string
	// Requires is a contract step's requirement (see the package
	// comment), or 0 for an expand step.
	Requires int
}

// Oldest is the oldest schema this release's queries run against: the
// first version holding every table it reads and writes. Raising it is
// what drops support for an older schema; a contract step that removes
// something this release uses would have to raise it first.
const Oldest = 4

var contractRE = regexp.MustCompile(`(?m)^--\s*silo:contract\s+requires=(\d+)\s*$`)

// contractOnly are the statements an expand step may not hold.
var contractOnly = regexp.MustCompile(`(?i)\b(DROP|RENAME)\b|\bSET\s+NOT\s+NULL\b|\bALTER\s+COLUMN\b[^;]*\bTYPE\b`)

// ContractStatement returns the first statement in an expand step's SQL
// that only a contract step may make (a DROP, a RENAME, a SET NOT NULL
// or a column type change), or "".
func ContractStatement(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line + "\n")
	}
	return contractOnly.FindString(b.String())
}

// Load returns backend's migrations in version order, checking that
// they are numbered 1, 2, 3, ... with no gaps or repeats.
func Load(backend string) ([]Migration, error) {
	return load(files, backend)
}

func load(fsys fs.FS, backend string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, backend)
	if err != nil {
		return nil, fmt.Errorf("migrations for %q: %w", backend, err)
	}
	var out []Migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s/%s: name must be NNNN_name.sql", backend, name)
		}
		body, err := fs.ReadFile(fsys, path.Join(backend, name))
		if err != nil {
			return nil, err
		}
		m := Migration{Version: v, Name: name, SQL: string(body)}
		if c := contractRE.FindStringSubmatch(m.SQL); c != nil {
			m.Requires, _ = strconv.Atoi(c[1])
			if m.Requires <= 0 {
				return nil, fmt.Errorf("migration %s/%s: a contract step requires a schema above 0", backend, name)
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migrations for %q: %s is version %d, want %d (versions start at 1 with no gaps or repeats)",
				backend, m.Name, m.Version, i+1)
		}
	}
	return out, nil
}
