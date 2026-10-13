// Package repos is silo repos' shared half (docs/silo/local-git.md,
// §2 "Repos as silo objects", call L41): the repos table, and what a
// repo's name and default branch may be. Package
// backend/local/gitstore keeps the repos themselves on disk.
//
// Who may create, read and delete repos is the fence API's; the git
// service that serves clones and pushes is a later step's.
package repos

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/store"
)

// DefaultBranch is a repo's default branch unless create names one.
const DefaultBranch = "main"

// ChangeKind is the change-log kind of a repo's creation and deletion.
// Its key is the repo's id. A creation's sequence number is the repo's
// Seq.
const ChangeKind = "repo"

// Errors from the table's operations, which the fence API turns into
// its codes.
var (
	ErrNotFound = errors.New("repos: no such repo")
	ErrExists   = errors.New("repos: the silo already has a repo of that name")
)

// Repo is one row of the repos table.
type Repo struct {
	ID            string
	Silo          string // the silo's id
	Name          string
	DefaultBranch string
	// Seq is the change-log sequence number the repo was created at,
	// which its directory's .silo-seq also holds.
	Seq       uint64
	SizeBytes int64
	Created   time.Time
}

// Source is a repo to seed a new one from.
type Source struct {
	Silo string // the silo's id
	ID   string // the repo's id
}

// NewID returns a new repo id: r- and six base-32 characters.
func NewID() string { return "r-" + ids.New() }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidName reports whether name may name a repo: 1 to 63 characters
// of [a-z0-9._-], starting with a letter or digit, with no "..", and
// not ending in .git or .lock. silod adds .git itself in URLs and refs.
// Names are lower case so that clones on case-insensitive filesystems
// can't collide.
func ValidName(name string) bool {
	return nameRE.MatchString(name) && !strings.Contains(name, "..") &&
		!strings.HasSuffix(name, ".git") && !strings.HasSuffix(name, ".lock")
}

// ValidBranch reports whether b may be a default branch: a name git
// takes as refs/heads/<b> (git check-ref-format --branch), of at most
// 255 bytes.
func ValidBranch(b string) bool {
	if b == "" || len(b) > 255 || b == "@" || strings.HasPrefix(b, "-") ||
		strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") {
		return false
	}
	for _, r := range b {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, c := range strings.Split(b, "/") {
		if strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return false
		}
	}
	return true
}

const columns = `id, silo, name, default_branch, seq, size_bytes, created_at`

func scan(row interface{ Scan(...any) error }) (*Repo, error) {
	var r Repo
	var seq, created int64
	if err := row.Scan(&r.ID, &r.Silo, &r.Name, &r.DefaultBranch, &seq, &r.SizeBytes, &created); err != nil {
		return nil, err
	}
	r.Seq, r.Created = uint64(seq), store.Time(created)
	return &r, nil
}

func one(ctx context.Context, tx store.Tx, where string, args ...any) (*Repo, error) {
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM repos WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// Create inserts r, filling in its Created time. Its id and Seq are
// the caller's.
func Create(ctx context.Context, tx store.Tx, r *Repo) error {
	if !ValidName(r.Name) {
		return fmt.Errorf("repos: invalid name %q", r.Name)
	}
	if _, err := ByName(ctx, tx, r.Silo, r.Name); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, r.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	r.Created = now
	_, err = tx.Exec(ctx, `INSERT INTO repos (`+columns+`, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Silo, r.Name, r.DefaultBranch, int64(r.Seq), r.SizeBytes, store.Millis(now), store.Millis(now))
	return err
}

// ByName reads silo's repo name.
func ByName(ctx context.Context, tx store.Tx, silo, name string) (*Repo, error) {
	return one(ctx, tx, `silo = ? AND name = ?`, silo, name)
}

// Get reads a repo by id.
func Get(ctx context.Context, tx store.Tx, id string) (*Repo, error) {
	return one(ctx, tx, `id = ?`, id)
}

// Of lists the repos of the silos named, by name within each silo, the
// silos in the order given.
func Of(ctx context.Context, tx store.Tx, silos ...string) ([]*Repo, error) {
	var out []*Repo
	for _, s := range silos {
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM repos WHERE silo = ? ORDER BY name`, s)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r, err := scan(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// All lists every repo, by id.
func All(ctx context.Context, tx store.Tx) ([]*Repo, error) {
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM repos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Repo
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func update(ctx context.Context, tx store.Tx, id, set string, v any) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	n, err := tx.Exec(ctx, `UPDATE repos SET `+set+` = ?, updated_at = ? WHERE id = ?`, v, store.Millis(now), id)
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// SetDefaultBranch records a repo's new default branch.
func SetDefaultBranch(ctx context.Context, tx store.Tx, id, branch string) error {
	if !ValidBranch(branch) {
		return fmt.Errorf("repos: invalid branch %q", branch)
	}
	return update(ctx, tx, id, "default_branch", branch)
}

// SetSize records a repo's measured size.
func SetSize(ctx context.Context, tx store.Tx, id string, bytes int64) error {
	return update(ctx, tx, id, "size_bytes", bytes)
}

// Delete removes a repo's row. One that is gone is ErrNotFound.
func Delete(ctx context.Context, tx store.Tx, id string) error {
	n, err := tx.Exec(ctx, `DELETE FROM repos WHERE id = ?`, id)
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}
