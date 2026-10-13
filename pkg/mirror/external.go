package mirror

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path"
	"strings"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Mapping back (local-git.md, §9): Mirror is fenceapi.Externals.

// External fills in a repo's place in the uber repo, for silo repo
// external: the ref prefixes under the configured prefix, what silod
// last pushed, and where the repo stands, as silo mirror status would
// say. out's Repo, ID, Silo and SiloID are the caller's.
func (m *Mirror) External(ctx context.Context, id, local string, out *fenceapi.RepoExternal) error {
	st, err := m.Status(ctx, true)
	if err != nil {
		return err
	}
	if !st.Configured {
		out.State = fenceapi.MirrorUnconfigured
		return nil
	}
	for _, mr := range st.Repos {
		if mr.ID == id {
			out.State, out.LastError = mr.State, mr.LastError
		}
	}
	return m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		cfg, err := loadConfig(ctx, tx)
		if err != nil || cfg == nil {
			return err
		}
		r, err := repos.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		p := fenceapi.GitPath(out.Silo, r.Name)
		out.URL = shownURL(cfg.URL)
		out.Branches = "refs/heads/" + cfg.Prefix + "/" + p + "/"
		out.Tags = "refs/tags/" + cfg.Prefix + "/" + p + "/"
		out.Archives = archiveRoot(cfg.Prefix, p)
		out.DefaultBranch = cfg.Prefix + "/" + p + "/" + r.DefaultBranch
		recs, err := readRecords(ctx, tx, id)
		if err != nil {
			return err
		}
		for ref := range recs {
			if strings.HasPrefix(ref, out.Archives) {
				out.ArchiveRefs++
			}
		}
		if _, ok := recs[external(cfg.Prefix, p, "refs/heads/"+r.DefaultBranch)]; ok && cfg.IndexBranch != "" {
			out.Index = cfg.IndexBranch + ":" + p
		}
		var at sql.NullInt64
		if err := tx.QueryRow(ctx, `SELECT MAX(updated_at) FROM mirror_refs WHERE repo = ?`, id).Scan(&at); err != nil {
			return err
		}
		if at.Valid {
			t := store.Time(at.Int64)
			out.MirroredTime = &t
		}
		if local != "" {
			ext := external(cfg.Prefix, p, local)
			out.Ref = &fenceapi.ExternalRef{Local: local, External: ext, Commit: recs[ext].SHA}
		}
		return nil
	})
}

// shownURL is the uber repo's URL without any password in it, which a
// URL given to silo mirror set shouldn't have, but might.
func shownURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, ok := u.User.Password(); !ok {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

// Which parses ref as one of the uber repo's refs for a silo's repo
// (local-git.md, §9), and looks the repo up. It takes a ref in full, a
// branch as GitHub shows it (silo/team-a/x/foo.git/main), a GitHub
// tree URL, or an index entry (main:team-a/x/foo.git). A repo that is
// gone is named by path, with the silo id silod still has for its refs,
// or else the one the index's README last gave it, if any.
func (m *Mirror) Which(ctx context.Context, ref string) (*fenceapi.RefWhich, error) {
	var cfg *config
	if err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		cfg, err = loadConfig(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errorf(fenceapi.FailedPrecondition, "no mirror is set, so no uber-repo ref is a silo's")
	}
	w, err := parseRef(cfg, ref)
	if err != nil {
		return nil, err
	}
	var tip string
	err = m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		if tip, _, err = store.ReadSetting(ctx, tx, settingIndexTip); err != nil {
			return err
		}
		var rec record
		var repo string
		err := tx.QueryRow(ctx, `SELECT repo, local, sha FROM mirror_refs WHERE ref = ?`, w.Ref).Scan(&repo, &rec.Local, &rec.SHA)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			w.Commit = rec.SHA
		}
		t, err := tree.ByPath(ctx, tx, w.Silo)
		if err == nil {
			w.SiloID = t.ID
			r, err := repos.ByName(ctx, tx, t.ID, path.Base(w.Repo))
			if err == nil {
				w.Exists, w.ID = true, r.ID
				if w.Kind == fenceapi.RefIndex {
					w.Local = "refs/heads/" + r.DefaultBranch
					w.Commit = ""
					if rec, err := readRecords(ctx, tx, r.ID); err == nil {
						w.Commit = rec[external(cfg.Prefix, strings.TrimPrefix(w.Ref, cfg.IndexBranch+":"), w.Local)].SHA
					}
				}
				return nil
			} else if !errors.Is(err, repos.ErrNotFound) {
				return err
			}
		} else if !errors.Is(err, tree.ErrNotFound) {
			return err
		}
		// Gone, or a silo of the same path that isn't the one whose
		// repo this was: silod's own record of the ref, if it has one.
		w.SiloID = ""
		if repo != "" {
			if row, err := readRepoRow(ctx, tx, repo); err != nil {
				return err
			} else if row != nil {
				w.SiloID, w.ID, w.From = row.Silo, repo, "mirror"
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !w.Exists && w.SiloID == "" && tip != "" {
		if id := m.indexSilo(ctx, tip, w.Repo); id != "" {
			w.SiloID, w.From = id, "index"
		}
	}
	return w, nil
}

// parseRef reads ref as an uber-repo ref of cfg's (local-git.md, §9).
// The first component of the path that ends in .git is the repo: silo
// names are DNS labels, which have no dot.
func parseRef(cfg *config, ref string) (*fenceapi.RefWhich, error) {
	ref = strings.TrimSpace(ref)
	bad := func(why string) error {
		e := errorf(fenceapi.InvalidArgument, "%s: %s", ref, why)
		e.Details = map[string]any{"field": "ref"}
		return e
	}
	s := ref
	if _, after, ok := strings.Cut(s, "/tree/"); ok && strings.Contains(s, "://") {
		s = after // a GitHub tree URL
	}
	p := cfg.Prefix
	w := &fenceapi.RefWhich{}
	var rest string
	switch {
	case strings.HasPrefix(s, "refs/heads/"+p+"/"):
		w.Kind, rest = fenceapi.RefBranch, strings.TrimPrefix(s, "refs/heads/"+p+"/")
	case strings.HasPrefix(s, "refs/tags/"+p+"/"):
		w.Kind, rest = fenceapi.RefTag, strings.TrimPrefix(s, "refs/tags/"+p+"/")
	case strings.HasPrefix(s, "refs/"+p+"-archive/"):
		w.Kind, rest = fenceapi.RefArchive, strings.TrimPrefix(s, "refs/"+p+"-archive/")
	case cfg.IndexBranch != "" && (strings.HasPrefix(s, cfg.IndexBranch+":") || strings.HasPrefix(s, "refs/heads/"+cfg.IndexBranch+":")):
		_, rest, _ = strings.Cut(s, ":")
		w.Kind = fenceapi.RefIndex
	case strings.HasPrefix(s, p+"/"):
		w.Kind, rest = fenceapi.RefBranch, strings.TrimPrefix(s, p+"/")
	case cfg.IndexBranch != "" && strings.HasPrefix(s, cfg.IndexBranch+"/"):
		w.Kind, rest = fenceapi.RefIndex, strings.TrimPrefix(s, cfg.IndexBranch+"/")
	default:
		return nil, bad("not a ref of a silo repo: want refs/heads/" + p + "/<silo path>/<repo>.git/<branch>, refs/tags/" + p +
			"/..., refs/" + p + "-archive/..., or " + orIndex(cfg))
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	i := 0
	for i < len(parts) && !strings.HasSuffix(parts[i], ".git") {
		i++
	}
	if i == len(parts) || parts[i] == ".git" {
		return nil, bad("no <repo>.git in it, so it names no repo")
	}
	name := strings.TrimSuffix(parts[i], ".git")
	gp := strings.Join(parts[:i+1], "/")
	tail := strings.Join(parts[i+1:], "/")
	w.Silo = "/" + strings.Join(parts[:i], "/")
	w.Repo = path.Join(w.Silo, name)
	switch w.Kind {
	case fenceapi.RefBranch:
		w.Ref, w.Local = "refs/heads/"+p+"/"+gp+"/"+tail, "refs/heads/"+tail
	case fenceapi.RefTag:
		w.Ref, w.Local = "refs/tags/"+p+"/"+gp+"/"+tail, "refs/tags/"+tail
	case fenceapi.RefArchive:
		// <ts>-<seq>/heads|tags|external/<name>
		f := strings.SplitN(tail, "/", 3)
		if len(f) < 3 || f[2] == "" || (f[1] != "heads" && f[1] != "tags" && f[1] != "external") {
			return nil, bad("an archive ref ends <ts>-<seq>/heads/<branch>, <ts>-<seq>/tags/<tag> or <ts>-<seq>/external/<branch>")
		}
		w.Ref, w.Local = archiveRoot(p, gp)+tail, "refs/silo-archive/"+tail
		if f[1] == "external" {
			w.Local = "" // the uber repo's own tip, adopted: never a local ref
		}
	case fenceapi.RefIndex:
		if tail != "" {
			return nil, bad("an index entry ends at <repo>.git")
		}
		w.Ref = cfg.IndexBranch + ":" + gp
	}
	if tail == "" && w.Kind != fenceapi.RefIndex {
		return nil, bad("no branch, tag or archive after " + parts[i])
	}
	return w, nil
}

func orIndex(cfg *config) string {
	if cfg.IndexBranch == "" {
		return cfg.Prefix + "/<silo path>/<repo>.git/<branch>"
	}
	return cfg.Prefix + "/<silo path>/<repo>.git/<branch> or " + cfg.IndexBranch + ":<silo path>/<repo>.git"
}

// indexSilo is the silo id the index's README last gave repo (a path,
// /team-a/x/foo), searching back from tip in mirror/index.git: the
// newest commit that added or removed its row, or that commit's parent
// if it removed it.
func (m *Mirror) indexSilo(ctx context.Context, tip, repo string) string {
	row := "| `" + repo + "` | `"
	out, err := m.git(ctx, m.indexRepo(), nil, nil, "log", "-1", "--format=%H", "-S"+row, tip, "--", "README.md")
	c := strings.TrimSpace(out)
	if err != nil || c == "" {
		return ""
	}
	for _, rev := range []string{c, c + "^"} {
		readme, err := m.git(ctx, m.indexRepo(), nil, nil, "show", rev+":README.md")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(readme, "\n") {
			if rest, ok := strings.CutPrefix(line, row); ok {
				if id, _, ok := strings.Cut(rest, "`"); ok {
					return id
				}
			}
		}
	}
	return ""
}

var _ fenceapi.Externals = (*Mirror)(nil)
