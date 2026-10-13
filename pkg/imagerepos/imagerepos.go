// Package imagerepos is image repos' shared half (docs/silo/local-registry.md,
// §3 "Repos as silo objects", call L58, and §4, call L59): the
// image_repos, image_repo_manifests and image_tags tables, what a
// repo's name, tags and digests may be, and how a registry name splits
// into an upstream, a silo and a repo (call L57).
//
// Who may list, read and delete image repos is the fence API's; the
// registry service that creates them by a first push and serves their
// manifests and blobs is a later step's.
package imagerepos

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/store"
)

// ChangeKind is the change-log kind of an image repo's creation and
// deletion. Its key is the repo's id. A creation's sequence number is
// the repo's Seq.
const ChangeKind = "image_repo"

// The states of a repo: READY, or DELETING while a delete removes its
// manifests, in which every registry request on it is a 404
// (local-registry.md, §10 "Deleting tags, manifests and repos").
const (
	StateReady    = "READY"
	StateDeleting = "DELETING"
)

// Errors from the tables' operations, which the fence API and the
// registry turn into their codes.
var (
	ErrNotFound         = errors.New("imagerepos: no such repo")
	ErrExists           = errors.New("imagerepos: the silo already has an image repo of that name")
	ErrManifestNotFound = errors.New("imagerepos: no such manifest in the repo")
	ErrTagNotFound      = errors.New("imagerepos: no such tag in the repo")
)

// Repo is one row of the image_repos table.
type Repo struct {
	ID    string
	Silo  string // the silo's id
	Name  string
	State string
	// Seq is the change-log sequence number the repo was created at.
	Seq      uint64
	Created  time.Time
	LastPush time.Time
}

// Blob is a blob or child manifest a manifest references, with its
// stored size.
type Blob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Manifest is one row of image_repo_manifests: a manifest a repo holds,
// tagged or not.
type Manifest struct {
	Repo      string
	Digest    string
	MediaType string
	Size      int64
	// Refs are what it references: config, layers, an index's
	// manifests. Never its subject, which may be absent.
	Refs    []Blob
	Created time.Time
}

// Tag is one row of image_tags.
type Tag struct {
	Repo    string
	Tag     string
	Digest  string
	Updated time.Time
}

// NewID returns a new image repo id: ir- and six base-32 characters.
func NewID() string { return "ir-" + ids.New() }

const columns = `id, silo, name, state, seq, created_at, last_push_at`

func scan(row interface{ Scan(...any) error }) (*Repo, error) {
	var r Repo
	var seq, created, pushed int64
	if err := row.Scan(&r.ID, &r.Silo, &r.Name, &r.State, &seq, &created, &pushed); err != nil {
		return nil, err
	}
	r.Seq, r.Created, r.LastPush = uint64(seq), store.Time(created), store.Time(pushed)
	return &r, nil
}

func one(ctx context.Context, tx store.Tx, where string, args ...any) (*Repo, error) {
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM image_repos WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func many(ctx context.Context, tx store.Tx, q string, args ...any) ([]*Repo, error) {
	rows, err := tx.Query(ctx, q, args...)
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

// Create inserts r, READY, filling in its Created and LastPush times.
// Its id and Seq are the caller's.
func Create(ctx context.Context, tx store.Tx, r *Repo) error {
	if !ValidName(r.Name) {
		return fmt.Errorf("imagerepos: invalid name %q", r.Name)
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
	r.State, r.Created, r.LastPush = StateReady, now, now
	_, err = tx.Exec(ctx, `INSERT INTO image_repos (`+columns+`, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Silo, r.Name, r.State, int64(r.Seq), store.Millis(now), store.Millis(now), store.Millis(now))
	return err
}

// ByName reads silo's image repo name.
func ByName(ctx context.Context, tx store.Tx, silo, name string) (*Repo, error) {
	return one(ctx, tx, `silo = ? AND name = ?`, silo, name)
}

// Get reads an image repo by id.
func Get(ctx context.Context, tx store.Tx, id string) (*Repo, error) {
	return one(ctx, tx, `id = ?`, id)
}

// Of lists the image repos of the silos named, by name within each
// silo, the silos in the order given.
func Of(ctx context.Context, tx store.Tx, silos ...string) ([]*Repo, error) {
	var out []*Repo
	for _, s := range silos {
		rs, err := many(ctx, tx, `SELECT `+columns+` FROM image_repos WHERE silo = ? ORDER BY name`, s)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// Count is how many image repos silo has.
func Count(ctx context.Context, tx store.Tx, silo string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM image_repos WHERE silo = ?`, silo).Scan(&n)
	return n, err
}

// SetState records a repo's state.
func SetState(ctx context.Context, tx store.Tx, id, state string) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	n, err := tx.Exec(ctx, `UPDATE image_repos SET state = ?, updated_at = ? WHERE id = ?`, state, store.Millis(now), id)
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// Delete removes a repo's row, its manifests and its tags. One that is
// gone is ErrNotFound.
func Delete(ctx context.Context, tx store.Tx, id string) error {
	for _, t := range []string{"image_tags", "image_repo_manifests"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE repo = ?`, id); err != nil {
			return err
		}
	}
	n, err := tx.Exec(ctx, `DELETE FROM image_repos WHERE id = ?`, id)
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

// PutManifest records that repo holds m, replacing a row of the same
// digest, and moves the repo's last push time.
func PutManifest(ctx context.Context, tx store.Tx, m *Manifest) error {
	if !ValidDigest(m.Digest) {
		return fmt.Errorf("imagerepos: invalid digest %q", m.Digest)
	}
	refs := m.Refs
	if refs == nil {
		refs = []Blob{}
	}
	rj, _ := json.Marshal(refs)
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	m.Created = now
	if _, err := tx.Exec(ctx, `INSERT INTO image_repo_manifests (repo, digest, media_type, size, refs, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo, digest) DO UPDATE SET media_type = excluded.media_type, size = excluded.size, refs = excluded.refs`,
		m.Repo, m.Digest, m.MediaType, m.Size, string(rj), store.Millis(now)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE image_repos SET last_push_at = ?, updated_at = ? WHERE id = ?`, store.Millis(now), store.Millis(now), m.Repo)
	return err
}

// Manifests lists repo's manifests, by digest.
func Manifests(ctx context.Context, tx store.Tx, repo string) ([]*Manifest, error) {
	rows, err := tx.Query(ctx, `SELECT repo, digest, media_type, size, refs, created_at FROM image_repo_manifests
		WHERE repo = ? ORDER BY digest`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Manifest
	for rows.Next() {
		var m Manifest
		var refs string
		var created int64
		if err := rows.Scan(&m.Repo, &m.Digest, &m.MediaType, &m.Size, &refs, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(refs), &m.Refs); err != nil {
			return nil, fmt.Errorf("imagerepos: manifest %s of %s: %w", m.Digest, repo, err)
		}
		m.Created = store.Time(created)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// SetTag points repo's tag at digest, which the repo must hold. The
// manifest it pointed at before stays, untagged.
func SetTag(ctx context.Context, tx store.Tx, repo, tag, digest string) error {
	if !ValidTag(tag) {
		return fmt.Errorf("imagerepos: invalid tag %q", tag)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM image_repo_manifests WHERE repo = ? AND digest = ?`, repo, digest).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrManifestNotFound, digest)
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO image_tags (repo, tag, digest, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (repo, tag) DO UPDATE SET digest = excluded.digest, updated_at = excluded.updated_at`,
		repo, tag, digest, store.Millis(now))
	return err
}

// Tags lists repo's tags, by tag.
func Tags(ctx context.Context, tx store.Tx, repo string) ([]*Tag, error) {
	rows, err := tx.Query(ctx, `SELECT repo, tag, digest, updated_at FROM image_tags WHERE repo = ? ORDER BY tag`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Tag
	for rows.Next() {
		var t Tag
		var updated int64
		if err := rows.Scan(&t.Repo, &t.Tag, &t.Digest, &updated); err != nil {
			return nil, err
		}
		t.Updated = store.Time(updated)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// Untag removes repo's tag; its manifest stays, untagged.
func Untag(ctx context.Context, tx store.Tx, repo, tag string) error {
	n, err := tx.Exec(ctx, `DELETE FROM image_tags WHERE repo = ? AND tag = ?`, repo, tag)
	if err == nil && n == 0 {
		return ErrTagNotFound
	}
	return err
}

// DeleteManifest removes repo's manifest digest and every tag on it,
// and an index's child manifests with it where nothing else in the repo
// references them and no tag points at them (local-registry.md, §10).
// It returns the digests of the manifests removed, digest first.
func DeleteManifest(ctx context.Context, tx store.Tx, repo, digest string) ([]string, error) {
	ms, err := Manifests(ctx, tx, repo)
	if err != nil {
		return nil, err
	}
	byDigest := map[string]*Manifest{}
	for _, m := range ms {
		byDigest[m.Digest] = m
	}
	if byDigest[digest] == nil {
		return nil, fmt.Errorf("%w: %s", ErrManifestNotFound, digest)
	}
	tags, err := Tags(ctx, tx, repo)
	if err != nil {
		return nil, err
	}
	tagged := map[string]bool{}
	for _, t := range tags {
		tagged[t.Digest] = true
	}
	gone := map[string]bool{}
	var order []string
	var drop func(d string)
	drop = func(d string) {
		gone[d] = true
		order = append(order, d)
		for _, r := range byDigest[d].Refs {
			if c := byDigest[r.Digest]; c == nil || gone[r.Digest] || tagged[r.Digest] {
				continue
			}
			kept := false
			for _, m := range ms {
				if gone[m.Digest] {
					continue
				}
				for _, rr := range m.Refs {
					if rr.Digest == r.Digest {
						kept = true
					}
				}
			}
			if !kept {
				drop(r.Digest)
			}
		}
	}
	drop(digest)
	for _, d := range order {
		if _, err := tx.Exec(ctx, `DELETE FROM image_tags WHERE repo = ? AND digest = ?`, repo, d); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM image_repo_manifests WHERE repo = ? AND digest = ?`, repo, d); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// Untagged lists the digests of repo's manifests that no tag points at
// and no other manifest of the repo references, which is what
// `silo registry prune --untagged` deletes.
func Untagged(ctx context.Context, tx store.Tx, repo string) ([]string, error) {
	ms, err := Manifests(ctx, tx, repo)
	if err != nil {
		return nil, err
	}
	tags, err := Tags(ctx, tx, repo)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, t := range tags {
		held[t.Digest] = true
	}
	for _, m := range ms {
		for _, r := range m.Refs {
			held[r.Digest] = true
		}
	}
	var out []string
	for _, m := range ms {
		if !held[m.Digest] {
			out = append(out, m.Digest)
		}
	}
	return out, nil
}

// Charged is the bytes a repo's manifests charge its silo's disk_gb
// (call L61): the stored size of every distinct blob and manifest they
// reference, manifests included, each counted once however many
// manifests of the repo share it.
func Charged(ms []*Manifest) int64 {
	sizes := map[string]int64{}
	for _, m := range ms {
		sizes[m.Digest] = m.Size
		for _, r := range m.Refs {
			if _, ok := sizes[r.Digest]; !ok {
				sizes[r.Digest] = r.Size
			}
		}
	}
	var n int64
	for _, s := range sizes {
		n += s
	}
	return n
}

// SizeOf is one manifest's charged size: its own size and its
// references', each once.
func SizeOf(m *Manifest) int64 { return Charged([]*Manifest{m}) }
