// Package tree keeps the silo tree's rows: each silo's id, name,
// parent, depth, path, backing, state and labels (docs/silo/README.md,
// "Silo"). It is the minimum the allocators hang on; the fence API adds
// the rest of a silo around it.
//
// Every function runs inside a store.Tx the caller holds, so creating a
// silo can commit together with its allocations.
package tree

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/ids"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// MaxDepth is the depth cap, with the root at depth 0
// (docs/silo/gcp-backend.md, "The fence"; README call 9).
const MaxDepth = 16

// The states a silo is in.
const (
	StateActive       = "ACTIVE"
	StateDeleting     = "DELETING"
	StateDeleteFailed = "DELETE_FAILED"
)

// BackingSubnet is the only backing v1 implements.
const BackingSubnet = "subnet"

// Silo is one row of the tree.
type Silo struct {
	ID      string
	Name    string
	Parent  string // "" for the root
	Depth   int
	Path    string // "/" for the root, "/team-a/ci" below it
	Backing string
	State   string
	Labels  map[string]string
	Created time.Time
	Updated time.Time
}

// IsRoot reports whether s is the root.
func (s *Silo) IsRoot() bool { return s.Parent == "" }

var (
	// ErrNotFound is a silo id with no row.
	ErrNotFound = errors.New("tree: no such silo")
	// ErrExists is a sibling, or a root, already holding the name.
	ErrExists = errors.New("tree: already exists")
	// ErrTooDeep is a child below MaxDepth.
	ErrTooDeep = fmt.Errorf("tree: deeper than the depth cap of %d", MaxDepth)
	// ErrInvalid is a bad name or backing.
	ErrInvalid = errors.New("tree: invalid")
)

var dnsLabel = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName reports whether name is a DNS label, as a silo's name must be.
func ValidName(name string) bool { return dnsLabel.MatchString(name) }

// CreateRoot creates the root silo. There is one per deployment.
func CreateRoot(ctx context.Context, tx store.Tx, name string, labels map[string]string) (*Silo, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM silos WHERE parent = ''`).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, fmt.Errorf("%w: the root silo", ErrExists)
	}
	return insert(ctx, tx, nil, name, BackingSubnet, labels)
}

// CreateChild creates a child of parent.
func CreateChild(ctx context.Context, tx store.Tx, parent, name, backing string, labels map[string]string) (*Silo, error) {
	p, err := Get(ctx, tx, parent)
	if err != nil {
		return nil, err
	}
	if p.Depth+1 > MaxDepth {
		return nil, ErrTooDeep
	}
	return insert(ctx, tx, p, name, backing, labels)
}

func insert(ctx context.Context, tx store.Tx, parent *Silo, name, backing string, labels map[string]string) (*Silo, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: name %q is not a DNS label", ErrInvalid, name)
	}
	if backing != BackingSubnet {
		return nil, fmt.Errorf("%w: backing %q (v1 has only %q)", ErrInvalid, backing, BackingSubnet)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	lj, err := json.Marshal(labels)
	if err != nil {
		return nil, err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	s := &Silo{Name: name, Path: "/", Backing: backing, State: StateActive, Labels: labels, Created: now, Updated: now}
	if parent != nil {
		s.Parent, s.Depth = parent.ID, parent.Depth+1
		if parent.IsRoot() {
			s.Path = "/" + name
		} else {
			s.Path = parent.Path + "/" + name
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM silos WHERE parent = ? AND name = ?`, s.Parent, name).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, fmt.Errorf("%w: %s", ErrExists, s.Path)
		}
	}
	// Draw ids until one is free. Checking first, inside the
	// serializable transaction, keeps a collision from aborting it. A
	// silo id starts with a letter: it is the prefix of its objects'
	// real names, <id>-<name>, and a GCE name must start with one
	// (gcp-backend.md, "Naming").
	for {
		s.ID = ids.New()
		if s.ID[0] < 'a' || s.ID[0] > 'z' {
			continue
		}
		var n int
		// Agents draw from the same generator, and never share a
		// silo's id (agents.md, "The agent object").
		if err := tx.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM silos WHERE id = ?) + (SELECT COUNT(*) FROM agents WHERE id = ?)`, s.ID, s.ID).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO silos (id, name, parent, depth, path, backing, state, labels, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.Parent, s.Depth, s.Path, s.Backing, s.State, string(lj), store.Millis(now), store.Millis(now))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Append(ctx, "silo", s.ID, ""); err != nil {
		return nil, err
	}
	return s, nil
}

const columns = `id, name, parent, depth, path, backing, state, labels, created_at, updated_at`

func scan(row interface{ Scan(...any) error }) (*Silo, error) {
	var s Silo
	var labels string
	var created, updated int64
	if err := row.Scan(&s.ID, &s.Name, &s.Parent, &s.Depth, &s.Path, &s.Backing, &s.State, &labels, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(labels), &s.Labels); err != nil {
		return nil, fmt.Errorf("silo %s labels: %w", s.ID, err)
	}
	s.Created, s.Updated = store.Time(created), store.Time(updated)
	return &s, nil
}

// Get reads one silo.
func Get(ctx context.Context, tx store.Tx, id string) (*Silo, error) {
	s, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM silos WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s, err
}

// Root reads the root silo. Before install has made it, it is
// ErrNotFound.
func Root(ctx context.Context, tx store.Tx) (*Silo, error) {
	s, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM silos WHERE parent = ''`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: the root silo", ErrNotFound)
	}
	return s, err
}

// All reads every silo, shallowest first and then by id.
func All(ctx context.Context, tx store.Tx) ([]*Silo, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM silos ORDER BY depth, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Silo
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Ancestry returns id and its ancestors, the root first and id last.
func Ancestry(ctx context.Context, tx store.Tx, id string) ([]*Silo, error) {
	var chain []*Silo
	for cur := id; cur != ""; {
		s, err := Get(ctx, tx, cur)
		if err != nil {
			return nil, err
		}
		chain = append(chain, s)
		if len(chain) > MaxDepth+1 {
			return nil, fmt.Errorf("tree: silo %s: parent chain longer than the depth cap", id)
		}
		cur = s.Parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// Subtree returns the ids of id and all its descendants.
func Subtree(ctx context.Context, tx store.Tx, id string) ([]string, error) {
	rows, err := tx.Query(ctx, `WITH RECURSIVE sub (id) AS (
			SELECT id FROM silos WHERE id = ?
			UNION ALL
			SELECT s.id FROM silos s JOIN sub ON s.parent = sub.id
		) SELECT id FROM sub`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Children returns parent's children, by name.
func Children(ctx context.Context, tx store.Tx, parent string) ([]*Silo, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM silos WHERE parent = ? ORDER BY name`, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Silo
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ByPath reads the silo at path ("/" for the root, "/team-a/ci").
func ByPath(ctx context.Context, tx store.Tx, path string) (*Silo, error) {
	s, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM silos WHERE path = ?`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return s, err
}

// SetState sets a silo's state.
func SetState(ctx context.Context, tx store.Tx, id, state string) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	n, err := tx.Exec(ctx, `UPDATE silos SET state = ?, updated_at = ? WHERE id = ?`, state, store.Millis(now), id)
	if err == nil && n == 0 {
		err = fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return err
}

// SetLabels replaces a silo's labels.
func SetLabels(ctx context.Context, tx store.Tx, id string, labels map[string]string) error {
	if labels == nil {
		labels = map[string]string{}
	}
	lj, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE silos SET labels = ?, updated_at = ? WHERE id = ?`, string(lj), store.Millis(now), id)
	return err
}

// Delete removes a silo's row. It must have no children: deletion is
// bottom-up, and whatever else the silo held is the caller's to
// release first.
func Delete(ctx context.Context, tx store.Tx, id string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM silos WHERE parent = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("tree: silo %s still has %d children", id, n)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM silos WHERE id = ?`, id); err != nil {
		return err
	}
	_, err := tx.Append(ctx, "silo", id, `{"deleted":true}`)
	return err
}
