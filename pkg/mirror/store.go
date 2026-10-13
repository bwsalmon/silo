package mirror

import (
	"context"
	"database/sql"
	"errors"

	"github.com/bwsalmon/silo/pkg/store"
)

// The settings the mirror's configuration is kept in. The credential
// is a file in keys/mirror/, never a setting.
const (
	settingURL         = "mirror.url"
	settingPrefix      = "mirror.prefix"
	settingIndexBranch = "mirror.index_branch"
	settingCredential  = "mirror.credential"
)

// config is the mirror's configuration.
type config struct {
	URL         string
	Prefix      string
	IndexBranch string
	Credential  string // its kind: ssh or https
}

// loadConfig reads the configuration, or nil if no mirror is set.
func loadConfig(ctx context.Context, tx store.Tx) (*config, error) {
	var c config
	for _, f := range []struct {
		name string
		v    *string
	}{{settingURL, &c.URL}, {settingPrefix, &c.Prefix}, {settingIndexBranch, &c.IndexBranch}, {settingCredential, &c.Credential}} {
		v, _, err := store.ReadSetting(ctx, tx, f.name)
		if err != nil {
			return nil, err
		}
		*f.v = v
	}
	if c.URL == "" {
		return nil, nil
	}
	return &c, nil
}

func saveConfig(ctx context.Context, tx store.Tx, c *config) error {
	for name, v := range map[string]string{settingURL: c.URL, settingPrefix: c.Prefix,
		settingIndexBranch: c.IndexBranch, settingCredential: c.Credential} {
		if err := store.WriteSetting(ctx, tx, name, v); err != nil {
			return err
		}
	}
	return nil
}

// record is one row of mirror_refs.
type record struct {
	Local string
	SHA   string
}

func readRecords(ctx context.Context, tx store.Tx, repo string) (map[string]record, error) {
	rows, err := tx.Query(ctx, `SELECT ref, local, sha FROM mirror_refs WHERE repo = ?`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]record{}
	for rows.Next() {
		var ref string
		var r record
		if err := rows.Scan(&ref, &r.Local, &r.SHA); err != nil {
			return nil, err
		}
		out[ref] = r
	}
	return out, rows.Err()
}

// setRecord records that the uber repo has sha at ref, or, with sha "",
// no ref.
func setRecord(ctx context.Context, tx store.Tx, repo, ref, local, sha string) error {
	if sha == "" {
		_, err := tx.Exec(ctx, `DELETE FROM mirror_refs WHERE ref = ?`, ref)
		return err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO mirror_refs (ref, repo, local, sha, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (ref) DO UPDATE SET repo = excluded.repo, local = excluded.local, sha = excluded.sha, updated_at = excluded.updated_at`,
		ref, repo, local, sha, store.Millis(now))
	return err
}

// diverged is one row of mirror_diverged.
type diverged struct {
	Ref, Local, Expected, Actual string
}

func readDiverged(ctx context.Context, tx store.Tx, repo string) (map[string]diverged, error) {
	rows, err := tx.Query(ctx, `SELECT ref, local, expected, actual FROM mirror_diverged WHERE repo = ?`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]diverged{}
	for rows.Next() {
		var d diverged
		if err := rows.Scan(&d.Ref, &d.Local, &d.Expected, &d.Actual); err != nil {
			return nil, err
		}
		out[d.Ref] = d
	}
	return out, rows.Err()
}

// repoRow is one row of mirror_repos.
type repoRow struct {
	Repo, Silo, Path string
	MirroredSeq      uint64
	PendingSince     int64 // ms; 0 while in sync
	Stalled          bool
	LastError        string
	Deleting         string
}

const repoColumns = `repo, silo, path, mirrored_seq, pending_since, stalled, last_error, deleting`

func scanRepoRow(row interface{ Scan(...any) error }) (*repoRow, error) {
	var r repoRow
	var seq, stalled int64
	if err := row.Scan(&r.Repo, &r.Silo, &r.Path, &seq, &r.PendingSince, &stalled, &r.LastError, &r.Deleting); err != nil {
		return nil, err
	}
	r.MirroredSeq, r.Stalled = uint64(seq), stalled != 0
	return &r, nil
}

func readRepoRow(ctx context.Context, tx store.Tx, repo string) (*repoRow, error) {
	r, err := scanRepoRow(tx.QueryRow(ctx, `SELECT `+repoColumns+` FROM mirror_repos WHERE repo = ?`, repo))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func readRepoRows(ctx context.Context, tx store.Tx) (map[string]*repoRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+repoColumns+` FROM mirror_repos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*repoRow{}
	for rows.Next() {
		r, err := scanRepoRow(rows)
		if err != nil {
			return nil, err
		}
		out[r.Repo] = r
	}
	return out, rows.Err()
}

func writeRepoRow(ctx context.Context, tx store.Tx, r *repoRow) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	stalled := 0
	if r.Stalled {
		stalled = 1
	}
	_, err = tx.Exec(ctx, `INSERT INTO mirror_repos (`+repoColumns+`, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo) DO UPDATE SET silo = excluded.silo, path = excluded.path, mirrored_seq = excluded.mirrored_seq,
			pending_since = excluded.pending_since, stalled = excluded.stalled, last_error = excluded.last_error,
			deleting = excluded.deleting, updated_at = excluded.updated_at`,
		r.Repo, r.Silo, r.Path, int64(r.MirroredSeq), r.PendingSince, stalled, r.LastError, r.Deleting, store.Millis(now))
	return err
}

// forget drops everything the mirror keeps of repo: a deleted repo's,
// once its refs are archived.
func forget(ctx context.Context, tx store.Tx, repo string) error {
	for _, q := range []string{`DELETE FROM mirror_refs WHERE repo = ?`, `DELETE FROM mirror_diverged WHERE repo = ?`, `DELETE FROM mirror_repos WHERE repo = ?`} {
		if _, err := tx.Exec(ctx, q, repo); err != nil {
			return err
		}
	}
	return nil
}

func lastSeq(ctx context.Context, tx store.Tx) (uint64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT seq FROM change_counter WHERE id = 1`).Scan(&n)
	return uint64(n), err
}
