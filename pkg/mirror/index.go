package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The submodule index (local-git.md, §7, call L51): a branch of the
// uber repo, main by default, whose tree has one gitlink per mirrored
// repo at <silo path>/<repo>.git, pointing at the tip of the repo's
// default branch the uber repo already has, with a .gitmodules and a
// generated README.md. Only silod writes it: each commit carries the
// IndexTrailer and is pushed with a lease on the last one silod pushed.

// IndexTrailer ends every index commit's message. silo mirror set only
// takes an index branch that is absent or whose tip carries it.
const IndexTrailer = "Silo-Index: 1"

// DefaultIndexDebounce is how long the index waits after a mirror pass,
// so that a burst of pushes makes one index commit.
const DefaultIndexDebounce = 10 * time.Second

// The index's own settings: the last commit silod pushed to the index
// branch, and why it stopped writing it, if it has.
const (
	settingIndexTip      = "mirror.index_tip"
	settingIndexDiverged = "mirror.index_diverged"
	settingIndexError    = "mirror.index_error"
)

// indexAuthor is every index commit's author and committer: fixed, so
// that a commit carries no host details.
var indexAuthor = []string{"GIT_AUTHOR_NAME=silo", "GIT_AUTHOR_EMAIL=silo@localhost",
	"GIT_COMMITTER_NAME=silo", "GIT_COMMITTER_EMAIL=silo@localhost"}

// indexRemote is where the external index tip is fetched to, in
// mirror/index.git, to be the next commit's parent after a loss.
const indexRemote = "refs/silo-index/external"

// indexRepo is mirror/index.git: the index's own trees and commits, and
// nothing else, since a gitlink doesn't pull the commit it points at
// into a pack.
func (m *Mirror) indexRepo() string { return filepath.Join(m.opts.Dir, "index.git") }

func (m *Mirror) ensureIndexRepo(ctx context.Context) error {
	d := m.indexRepo()
	if _, err := os.Stat(filepath.Join(d, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(m.opts.Dir, 0o700); err != nil {
		return err
	}
	_, err := m.git(ctx, "", nil, nil, "init", "--quiet", "--bare", d)
	return err
}

// entry is one gitlink of the index.
type entry struct {
	Path     string // in the index tree: team-a/x/foo.git
	Silo     string // the silo's id
	SiloPath string // /team-a/x
	Name     string
	Branch   string // the repo's default branch
	SHA      string // what the uber repo has on it
	Mirrored time.Time
}

// indexEntries is the index's gitlinks, from the store's record of
// what the uber repo already has, never from local refs: a repo whose
// default branch silod hasn't pushed yet has no entry.
func indexEntries(ctx context.Context, tx store.Tx, cfg *config) ([]entry, error) {
	silos, err := tree.All(ctx, tx)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	for _, s := range silos {
		paths[s.ID] = s.Path
	}
	rs, err := repos.All(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := readRepoRows(ctx, tx)
	if err != nil {
		return nil, err
	}
	mirrored := map[string]bool{}
	var out []entry
	for _, r := range rs {
		sp, ok := paths[r.Silo]
		if !ok {
			continue
		}
		if row := rows[r.ID]; row != nil && row.Deleting != "" {
			continue
		}
		on, ok := mirrored[r.Silo]
		if !ok {
			if on, err = fenceapi.ReposMirrored(ctx, tx, r.Silo); err != nil {
				return nil, err
			}
			mirrored[r.Silo] = on
		}
		if !on {
			continue
		}
		p := fenceapi.GitPath(sp, r.Name)
		ref := external(cfg.Prefix, p, "refs/heads/"+r.DefaultBranch)
		var sha string
		var at int64
		err := tx.QueryRow(ctx, `SELECT sha, updated_at FROM mirror_refs WHERE ref = ? AND repo = ?`, ref, r.ID).Scan(&sha, &at)
		if err != nil {
			continue // not in the uber repo yet
		}
		out = append(out, entry{Path: p, Silo: r.Silo, SiloPath: sp, Name: r.Name, Branch: r.DefaultBranch, SHA: sha, Mirrored: store.Time(at)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// gitmodules is the index's .gitmodules: one entry per gitlink, sorted,
// each pointing back at the uber repo itself (url = ./) and following
// the repo's namespaced default branch.
func gitmodules(cfg *config, es []entry) string {
	var b strings.Builder
	for _, e := range es {
		fmt.Fprintf(&b, "[submodule %q]\n\tpath = %s\n\turl = ./\n\tbranch = %s\n",
			e.Path, e.Path, cfg.Prefix+"/"+e.Path+"/"+e.Branch)
	}
	return b.String()
}

// readme is the index's generated README.md.
func readme(cfg *config, es []entry) string {
	p := cfg.Prefix
	var b strings.Builder
	b.WriteString("# Silo repositories\n\n")
	b.WriteString("This repository is written by Silo. Every Silo repository that is mirrored here keeps its own\n")
	b.WriteString("branches and tags, with their exact history, under its silo's path:\n\n")
	b.WriteString("```\n")
	fmt.Fprintf(&b, "refs/heads/%s/<silo path>/<repo>.git/<branch>\n", p)
	fmt.Fprintf(&b, "refs/tags/%s/<silo path>/<repo>.git/<tag>\n", p)
	fmt.Fprintf(&b, "refs/%s-archive/<silo path>/<repo>.git/<ts>-<seq>/heads/<branch>     (overwritten and deleted tips)\n", p)
	fmt.Fprintf(&b, "refs/%s-archive/<silo path>/<repo>.git/<ts>-<seq>/tags/<tag>\n", p)
	fmt.Fprintf(&b, "refs/heads/%s:<silo path>/<repo>.git                                (this index)\n", cfg.IndexBranch)
	b.WriteString("```\n\n")
	fmt.Fprintf(&b, "This branch, `%s`, is the index: one folder per repository, each a submodule pinned to the\n", cfg.IndexBranch)
	b.WriteString("tip of that repository's default branch here. Only Silo commits to it; a commit by anyone\n")
	fmt.Fprintf(&b, "else stops Silo updating it. Don't push to `%s/**` either: Silo won't overwrite what you\n", p)
	b.WriteString("push, and stops mirroring that branch until an operator decides.\n\n")
	fmt.Fprintf(&b, "Workflows triggered on `%s/**` would run on every mirror push, so this repository should\n", p)
	b.WriteString("have none.\n\n")
	b.WriteString("## Cloning\n\n")
	b.WriteString("A plain clone fetches every branch and tag of every repository. To clone one repository:\n\n")
	b.WriteString("```sh\n")
	fmt.Fprintf(&b, "git clone --single-branch -b %s/team-a/x/foo.git/main <this repository's URL> foo\n", p)
	b.WriteString("```\n\n")
	b.WriteString("(add `--filter=blob:none` for a partial clone). Submodule URLs inside Silo repositories say\n")
	b.WriteString("`http://silo.internal/git/...`, which means nothing outside Silo, but every commit they name is\n")
	b.WriteString("in this repository. Run this in the clone before `git submodule update`, and again in each\n")
	b.WriteString("nested submodule (`git submodule foreach --recursive`):\n\n")
	b.WriteString("```sh\n")
	b.WriteString("UBER=$(git remote get-url origin)\n")
	b.WriteString("git config -f .gitmodules --get-regexp '\\.url$' | while read -r key url; do\n")
	b.WriteString("  case $url in http://silo.internal/git/*) git config \"$key\" \"$UBER\" ;; esac\n")
	b.WriteString("done\n")
	b.WriteString("git submodule update --init --single-branch\n")
	b.WriteString("```\n\n")
	b.WriteString("Checking out this index with `--recurse-submodules` clones this whole repository once per\n")
	b.WriteString("folder unless `--single-branch` is passed too.\n\n")
	b.WriteString("## Repositories\n\n")
	if len(es) == 0 {
		b.WriteString("None yet.\n")
		return b.String()
	}
	b.WriteString("A silo's path can be reused after it is deleted; its id can't.\n\n")
	b.WriteString("| Path | Silo id | Default branch | Branch here | Last mirrored |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, e := range es {
		sp := strings.TrimSuffix(e.SiloPath, "/")
		fmt.Fprintf(&b, "| `%s/%s` | `%s` | `%s` | `%s/%s/%s` | %s |\n", sp, e.Name, e.Silo, e.Branch,
			p, e.Path, e.Branch, e.Mirrored.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// errIndexDiverged is an index branch someone else has written to.
var errIndexDiverged = errors.New("mirror: the index branch has a commit silod didn't write")

// Index runs the index pass now: it commits a new index if the mirrored
// repos' default branches have moved since the last one, and pushes it
// with a lease on the last index commit silod pushed. With no mirror,
// no index branch, or an index branch that has diverged, it does
// nothing.
func (m *Mirror) Index(ctx context.Context) error {
	m.indexMu.Lock()
	defer m.indexMu.Unlock()
	var cfg *config
	var es []entry
	var tip, div string
	err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		if cfg, err = loadConfig(ctx, tx); err != nil || cfg == nil || cfg.IndexBranch == "" {
			return err
		}
		if div, _, err = store.ReadSetting(ctx, tx, settingIndexDiverged); err != nil || div != "" {
			return err
		}
		if tip, _, err = store.ReadSetting(ctx, tx, settingIndexTip); err != nil {
			return err
		}
		es, err = indexEntries(ctx, tx, cfg)
		return err
	})
	if err != nil || cfg == nil || cfg.IndexBranch == "" || div != "" {
		return err
	}
	err = m.writeIndex(ctx, cfg, tip, es)
	return m.recordIndex(ctx, cfg, err)
}

// writeIndex makes and pushes the index commit for es on top of tip,
// the last one silod pushed ("" if the store has none).
func (m *Mirror) writeIndex(ctx context.Context, cfg *config, tip string, es []entry) error {
	if err := m.ensureIndexRepo(ctx); err != nil {
		return err
	}
	dir := m.indexRepo()
	branch := "refs/heads/" + cfg.IndexBranch
	parent, lease := tip, tip
	if tip == "" || !m.hasCommit(ctx, dir, tip) {
		// A first index, a lost mirror/index.git or a lost store: the
		// external tip is the parent, if silod wrote it. Only that one
		// ref is fetched, and it holds only silod's own commits.
		ext, err := m.externalIndex(ctx, cfg, m.opts.Keys, true)
		if err != nil {
			return err
		}
		switch {
		case tip == "" && ext.SHA != "" && !ext.Ours:
			return &indexDivergedError{expected: "", actual: ext.SHA}
		case tip != "" && ext.SHA != tip:
			return &indexDivergedError{expected: tip, actual: ext.SHA}
		}
		parent, lease = ext.SHA, ext.SHA
	}
	tree, err := m.indexTree(ctx, cfg, dir, es)
	if err != nil {
		return err
	}
	old := map[string]string{}
	if parent != "" {
		out, err := m.git(ctx, dir, nil, nil, "rev-parse", parent+"^{tree}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == tree {
			return nil // nothing moved
		}
		if old, err = m.gitlinks(ctx, dir, parent); err != nil {
			return err
		}
	}
	args := []string{"commit-tree", tree, "-m", indexMessage(old, es)}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	out, err := m.git(ctx, dir, indexAuthor, nil, args...)
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(out)
	// A ref of its own keeps the commit from being pruned before the
	// push lands.
	if _, err := m.git(ctx, dir, nil, nil, "update-ref", "refs/silo-index/next", commit); err != nil {
		return err
	}
	_, err = m.push(ctx, cfg, m.opts.Keys, dir, []update{{Dst: branch, Src: commit, Lease: lease}})
	if errors.Is(err, errStale) {
		actual, lerr := m.lsRemote(ctx, cfg, m.opts.Keys, branch)
		if lerr != nil {
			return lerr
		}
		return &indexDivergedError{expected: lease, actual: actual[branch]}
	}
	if err != nil {
		return err
	}
	if _, err := m.git(ctx, dir, nil, nil, "update-ref", branch, commit); err != nil {
		return err
	}
	return m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		return store.WriteSetting(ctx, tx, settingIndexTip, commit)
	})
}

// indexDivergedError is an index branch whose tip isn't what silod
// expected.
type indexDivergedError struct{ expected, actual string }

func (e *indexDivergedError) Error() string {
	exp, act := e.expected, e.actual
	if exp == "" {
		exp = "a branch silod wrote, or none"
	}
	if act == "" {
		act = "no branch"
	}
	return fmt.Sprintf("%v: expected %s, found %s", errIndexDiverged, exp, act)
}

func (e *indexDivergedError) Unwrap() error { return errIndexDiverged }

// recordIndex writes the index pass's outcome: a divergence stops the
// index until silo mirror set is run again, and is audited once.
func (m *Mirror) recordIndex(ctx context.Context, cfg *config, passErr error) error {
	var de *indexDivergedError
	err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		if errors.As(passErr, &de) {
			if err := store.WriteSetting(ctx, tx, settingIndexDiverged, de.Error()); err != nil {
				return err
			}
			root, err := tree.Root(ctx, tx)
			if err != nil {
				return err
			}
			if err := m.audit(ctx, tx, root.ID, fenceapi.KindMirrorIndexDiverged, map[string]any{
				"branch": cfg.IndexBranch, "expected": de.expected, "actual": de.actual}); err != nil {
				return err
			}
			return store.WriteSetting(ctx, tx, settingIndexError, "")
		}
		msg := ""
		if passErr != nil {
			msg = passErr.Error()
		}
		return store.WriteSetting(ctx, tx, settingIndexError, msg)
	})
	if de != nil {
		m.opts.Logf("mirror: index branch %s: %v; silod stops writing it", cfg.IndexBranch, de)
		return err
	}
	if passErr != nil {
		m.opts.Logf("mirror: index: %v", passErr)
		return passErr
	}
	return err
}

func (m *Mirror) hasCommit(ctx context.Context, dir, sha string) bool {
	_, err := m.git(ctx, dir, nil, nil, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// indexTree writes the index's tree for es into dir and returns it.
func (m *Mirror) indexTree(ctx context.Context, cfg *config, dir string, es []entry) (string, error) {
	blob := func(s string) (string, error) {
		out, err := m.git(ctx, dir, nil, []byte(s), "hash-object", "-w", "--stdin")
		return strings.TrimSpace(out), err
	}
	var info strings.Builder
	for _, e := range es {
		fmt.Fprintf(&info, "160000 %s\t%s\n", e.SHA, e.Path)
	}
	if len(es) > 0 {
		mods, err := blob(gitmodules(cfg, es))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&info, "100644 %s\t.gitmodules\n", mods)
	}
	rd, err := blob(readme(cfg, es))
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&info, "100644 %s\tREADME.md\n", rd)
	// A throwaway index file: update-index lays the nested trees out,
	// and write-tree writes them, gitlinks and all, without needing the
	// commits they point at.
	f, err := os.CreateTemp(dir, "index-*.tmp")
	if err != nil {
		return "", err
	}
	f.Close()
	os.Remove(f.Name())
	defer os.Remove(f.Name())
	env := []string{"GIT_INDEX_FILE=" + f.Name()}
	if _, err := m.git(ctx, dir, env, []byte(info.String()), "update-index", "--add", "--index-info"); err != nil {
		return "", err
	}
	out, err := m.git(ctx, dir, env, nil, "write-tree")
	return strings.TrimSpace(out), err
}

// gitlinks is commit's gitlinks, by path.
func (m *Mirror) gitlinks(ctx context.Context, dir, commit string) (map[string]string, error) {
	out, err := m.git(ctx, dir, nil, nil, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	links := map[string]string{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 && f[0] == "160000" {
			links[p] = f[2]
		}
	}
	return links, nil
}

// indexMessage lists the repos that changed since old, and ends in the
// trailer.
func indexMessage(old map[string]string, es []entry) string {
	var lines []string
	now := map[string]bool{}
	for _, e := range es {
		now[e.Path] = true
		switch was, ok := old[e.Path]; {
		case !ok:
			lines = append(lines, fmt.Sprintf("- %s: added at %.12s", e.Path, e.SHA))
		case was != e.SHA:
			lines = append(lines, fmt.Sprintf("- %s: %.12s to %.12s", e.Path, was, e.SHA))
		}
	}
	for p := range old {
		if !now[p] {
			lines = append(lines, fmt.Sprintf("- %s: removed", p))
		}
	}
	sort.Strings(lines)
	subject := "Update the Silo index"
	if len(lines) == 0 {
		subject = "Regenerate the Silo index"
	}
	msg := subject + "\n\n"
	if len(lines) > 0 {
		msg += strings.Join(lines, "\n") + "\n\n"
	}
	return msg + IndexTrailer + "\n"
}

// externalTip is the uber repo's index branch as silod finds it.
type externalTip struct {
	SHA  string // "" if the branch is absent
	Ours bool   // its tip carries the IndexTrailer
}

// externalIndex reads the uber repo's index branch with the credential
// in keys. If its tip is silod's and keep is set, the branch is fetched
// into mirror/index.git, to be the next commit's parent; otherwise
// only the tip is fetched, shallow, into quarantine.git, to read its
// message.
func (m *Mirror) externalIndex(ctx context.Context, cfg *config, keys string, keep bool) (externalTip, error) {
	branch := "refs/heads/" + cfg.IndexBranch
	refs, err := m.lsRemote(ctx, cfg, keys, branch)
	if err != nil {
		return externalTip{}, err
	}
	sha := refs[branch]
	if sha == "" {
		return externalTip{}, nil
	}
	env, opts := remoteEnv(cfg.Credential, keys)
	fetch := func(dir string, extra ...string) error {
		args := append(append(append([]string{}, opts...), "fetch", "--quiet", "--no-tags", "--no-write-fetch-head"), extra...)
		args = append(args, cfg.URL, "+"+branch+":"+indexRemote)
		_, err := m.git(ctx, dir, env, nil, args...)
		return err
	}
	if err := m.ensureIndexRepo(ctx); err != nil {
		return externalTip{}, err
	}
	dir := m.indexRepo()
	if !m.hasCommit(ctx, dir, sha) {
		// Only the tip, to read its message, into a throwaway repo: a
		// branch silod didn't write may be anything, and its history
		// stays out of index.git.
		tmp, err := os.MkdirTemp(m.opts.Dir, "index-check-*.git")
		if err != nil {
			return externalTip{}, err
		}
		defer os.RemoveAll(tmp)
		if _, err := m.git(ctx, "", nil, nil, "init", "--quiet", "--bare", tmp); err != nil {
			return externalTip{}, err
		}
		dir = tmp
		if err := fetch(dir, "--depth=1"); err != nil {
			return externalTip{}, fmt.Errorf("fetching the index branch's tip: %w", err)
		}
	}
	msg, err := m.git(ctx, dir, nil, nil, "log", "-1", "--format=%B", sha)
	if err != nil {
		return externalTip{}, err
	}
	tip := externalTip{SHA: sha, Ours: hasTrailer(msg)}
	if tip.Ours && keep && dir != m.indexRepo() {
		if err := fetch(m.indexRepo()); err != nil {
			return externalTip{}, fmt.Errorf("fetching the index branch: %w", err)
		}
	}
	return tip, nil
}

// hasTrailer reports whether a commit message's last paragraph has the
// IndexTrailer.
func hasTrailer(msg string) bool {
	paras := strings.Split(strings.TrimSpace(msg), "\n\n")
	for _, line := range strings.Split(paras[len(paras)-1], "\n") {
		if strings.TrimSpace(line) == IndexTrailer {
			return true
		}
	}
	return false
}
