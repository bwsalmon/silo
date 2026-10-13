package mirror

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

func keyScan(ctx context.Context, host, port string) ([]byte, error) {
	args := []string{"-T", "10"}
	if port != "" {
		args = append(args, "-p", port)
	}
	return exec.CommandContext(ctx, "ssh-keyscan", append(args, host)...).Output()
}

func errorf(code fenceapi.Code, format string, args ...any) *fenceapi.Error {
	return &fenceapi.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// stagingRef is where a chunked first push puts its steps.
func stagingRef(prefix, id string) string { return "refs/" + prefix + "-staging/" + id }

// stage splits a first push too big for one (local-git.md, "Size and
// limits", call L50): it pushes ancestors of the tips, in steps of at
// most ChunkBytes of new objects as rev-list --disk-usage estimates
// them, each to the repo's staging ref, and returns us with the staging
// ref's deletion added, for the last push.
func (m *Mirror) stage(ctx context.Context, cfg *config, t *target, dir string, us []update) ([]update, error) {
	var tips []string
	seen := map[string]bool{}
	for _, u := range us {
		if u.Src != "" && !seen[u.Src] {
			seen[u.Src] = true
			tips = append(tips, u.Src)
		}
	}
	if len(tips) == 0 {
		return us, nil
	}
	usage := func(from, not []string) (int64, error) {
		args := append([]string{"rev-list", "--objects", "--disk-usage"}, from...)
		if len(not) > 0 {
			args = append(append(args, "--not"), not...)
		}
		out, err := m.git(ctx, dir, nil, nil, args...)
		if err != nil {
			return 0, err
		}
		return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	}
	total, err := usage(tips, nil)
	if err != nil || total <= m.opts.ChunkBytes {
		return us, err
	}
	out, err := m.git(ctx, dir, nil, nil, append([]string{"rev-list", "--reverse", "--topo-order"}, tips...)...)
	if err != nil {
		return us, err
	}
	commits := strings.Fields(out)
	staging := stagingRef(cfg.Prefix, t.ID)
	var pushed []string
	for i := 0; i < len(commits); {
		if rest, err := usage(tips, pushed); err != nil {
			return us, err
		} else if rest <= m.opts.ChunkBytes {
			break
		}
		// The furthest commit whose new objects fit, or the next one
		// if none does.
		lo, hi := i, len(commits)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			n, err := usage([]string{commits[mid]}, pushed)
			if err != nil {
				return us, err
			}
			if n <= m.opts.ChunkBytes {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		step := []update{{Dst: staging, Src: commits[lo], NoLease: true}}
		if _, err := m.push(ctx, cfg, m.opts.Keys, dir, step); err != nil {
			return us, fmt.Errorf("pushing step %d of a chunked first push: %w", len(pushed)+1, err)
		}
		pushed = append(pushed, commits[lo])
		i = lo + 1
	}
	if len(pushed) > 0 {
		us = append(us, update{Dst: staging, NoLease: true})
	}
	return us, nil
}

// probe tests that the uber repo lets silod create, force-push and
// delete a branch under the prefix (local-git.md, "Size and limits":
// branch rules and rulesets), with the credential in keys.
func (m *Mirror) probe(ctx context.Context, cfg *config, keys string) error {
	if err := m.ensureQuarantine(ctx); err != nil {
		return err
	}
	q := m.quarantine()
	id := []string{"GIT_AUTHOR_NAME=silod", "GIT_AUTHOR_EMAIL=silod@silo.internal",
		"GIT_COMMITTER_NAME=silod", "GIT_COMMITTER_EMAIL=silod@silo.internal"}
	empty, err := m.git(ctx, q, nil, []byte{}, "mktree")
	if err != nil {
		return err
	}
	var commits [2]string
	for i := range commits {
		out, err := m.git(ctx, q, id, nil, "commit-tree", strings.TrimSpace(empty), "-m", fmt.Sprintf("silo mirror probe %d", i+1))
		if err != nil {
			return err
		}
		commits[i] = strings.TrimSpace(out)
	}
	ref := "refs/heads/" + cfg.Prefix + "/" + ProbeRef
	now, err := m.lsRemote(ctx, cfg, keys, ref)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", cfg.URL, err)
	}
	for _, s := range []struct {
		what string
		u    update
	}{
		{"creating", update{Dst: ref, Src: commits[0], Lease: now[ref]}},
		{"force-pushing", update{Dst: ref, Src: commits[1], Lease: commits[0]}},
		{"deleting", update{Dst: ref, Lease: commits[1]}},
	} {
		if _, err := m.push(ctx, cfg, keys, q, []update{s.u}); err != nil {
			return fmt.Errorf("the uber repo refused silod %s %s, so a branch rule or ruleset would stop the mirror: %v", s.what, ref, err)
		}
	}
	return nil
}

// Set sets the mirror (silo mirror set; local-git.md, call L52): it
// checks the URL with the credential and the probe, then keeps the
// credential in keys/mirror/ and the rest in the store. A new URL or
// prefix is a new target: everything is pushed to it again, and the
// old target's refs are left alone.
func (m *Mirror) Set(ctx context.Context, c fenceapi.Caller, s fenceapi.MirrorSettings) (*fenceapi.MirrorStatus, error) {
	cfg := &config{URL: strings.TrimSpace(s.URL), Prefix: s.Prefix, IndexBranch: s.IndexBranch}
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.IndexBranch == "" {
		cfg.IndexBranch = DefaultIndexBranch
	}
	kind, err := credentialKind(cfg.URL)
	if err != nil {
		return nil, errorf(fenceapi.InvalidArgument, "%v", err)
	}
	cfg.Credential = kind
	if !tree.ValidName(cfg.Prefix) {
		return nil, errorf(fenceapi.InvalidArgument, "prefix %q: want a DNS label, as a silo's name", cfg.Prefix)
	}
	if !repos.ValidBranch(cfg.IndexBranch) || strings.HasPrefix(cfg.IndexBranch, cfg.Prefix+"/") {
		return nil, errorf(fenceapi.InvalidArgument, "index branch %q: want a branch name outside the prefix", cfg.IndexBranch)
	}
	if s.Credential == "" {
		return nil, errorf(fenceapi.InvalidArgument, "a mirror needs a credential: an SSH deploy key with write access, or an HTTPS token")
	}
	staged, err := m.writeKeys(ctx, kind, cfg.URL, []byte(s.Credential))
	if err != nil {
		return nil, errorf(fenceapi.FailedPrecondition, "%v", err)
	}
	if err := m.probe(ctx, cfg, staged); err != nil {
		return nil, errorf(fenceapi.FailedPrecondition, "%v", err)
	}
	if err := install(staged, m.opts.Keys); err != nil {
		return nil, err
	}
	var seq uint64
	err = m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		old, err := loadConfig(ctx, tx)
		if err != nil {
			return err
		}
		if err := saveConfig(ctx, tx, cfg); err != nil {
			return err
		}
		if old == nil || old.URL != cfg.URL || old.Prefix != cfg.Prefix {
			// A new target: silod forgets what it put in the old one.
			for _, q := range []string{`DELETE FROM mirror_refs`, `DELETE FROM mirror_diverged`,
				`UPDATE mirror_repos SET mirrored_seq = 0, pending_since = 0, stalled = 0, last_error = ''`} {
				if _, err := tx.Exec(ctx, q); err != nil {
					return err
				}
			}
		}
		root, err := tree.Root(ctx, tx)
		if err != nil {
			return err
		}
		if seq, err = lastSeq(ctx, tx); err != nil {
			return err
		}
		return fenceapi.Audit(ctx, tx, root, c, fenceapi.KindMirrorSet, "", "OK", map[string]any{
			"url": cfg.URL, "prefix": cfg.Prefix, "index_branch": cfg.IndexBranch, "credential": cfg.Credential})
	})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	for _, st := range m.repos {
		st.mirrored, st.backoff, st.next = 0, 0, time.Time{}
	}
	m.mu.Unlock()
	if err := m.markAll(ctx, seq); err != nil {
		return nil, err
	}
	return m.Status(ctx, false)
}

// Status is silo mirror status: the configuration, and every repo that
// isn't in sync (or every repo, with all).
func (m *Mirror) Status(ctx context.Context, all bool) (*fenceapi.MirrorStatus, error) {
	paused, err := m.paused(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	view := map[string]state{}
	for id, s := range m.repos {
		view[id] = *s
	}
	m.mu.Unlock()
	var out *fenceapi.MirrorStatus
	err = m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		out = &fenceapi.MirrorStatus{Repos: []fenceapi.MirrorRepo{}, Paused: paused}
		cfg, err := loadConfig(ctx, tx)
		if err != nil {
			return err
		}
		if cfg != nil {
			out.Configured, out.URL, out.Prefix, out.IndexBranch, out.Credential = true, cfg.URL, cfg.Prefix, cfg.IndexBranch, cfg.Credential
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM mirror_refs`).Scan(&out.Refs); err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		rows, err := readRepoRows(ctx, tx)
		if err != nil {
			return err
		}
		silos, err := tree.All(ctx, tx)
		if err != nil {
			return err
		}
		paths := map[string]string{}
		for _, s := range silos {
			paths[s.ID] = s.Path
		}
		add := func(id, p string, on bool, size int64, row *repoRow) error {
			mr := fenceapi.MirrorRepo{ID: id, Path: p}
			div, err := readDiverged(ctx, tx, id)
			if err != nil {
				return err
			}
			v, seen := view[id]
			switch {
			case !out.Configured:
				mr.State = fenceapi.MirrorUnconfigured
			case !on:
				mr.State = fenceapi.MirrorOff
			case row != nil && row.Deleting != "":
				mr.State = fenceapi.MirrorDeleting
				out.PendingDeletes++
			case row != nil && row.Stalled:
				mr.State = fenceapi.MirrorStalled
			case row == nil, row.PendingSince > 0, seen && (v.force || v.dirty > v.mirrored) && v.dirty > row.MirroredSeq:
				mr.State = fenceapi.MirrorPending
			case len(div) > 0:
				mr.State = fenceapi.MirrorDiverged
			default:
				mr.State = fenceapi.MirrorInSync
				out.InSync++
			}
			if on && out.Configured {
				out.EstimatedBytes += size
			}
			if row != nil {
				mr.LastError = row.LastError
				if row.PendingSince > 0 {
					mr.LagSeconds = int64(now.Sub(store.Time(row.PendingSince)).Seconds())
				}
			}
			for _, d := range div {
				mr.Diverged = append(mr.Diverged, fenceapi.DivergedRef{Ref: d.Ref, Local: d.Local, Expected: d.Expected, Actual: d.Actual})
			}
			sort.Slice(mr.Diverged, func(i, j int) bool { return mr.Diverged[i].Ref < mr.Diverged[j].Ref })
			if all || (mr.State != fenceapi.MirrorInSync && mr.State != fenceapi.MirrorOff && mr.State != fenceapi.MirrorUnconfigured) {
				out.Repos = append(out.Repos, mr)
			}
			return nil
		}
		rs, err := repos.All(ctx, tx)
		if err != nil {
			return err
		}
		mirrored := map[string]bool{}
		for _, r := range rs {
			on, ok := mirrored[r.Silo]
			if !ok {
				if on, err = fenceapi.ReposMirrored(ctx, tx, r.Silo); err != nil {
					return err
				}
				mirrored[r.Silo] = on
			}
			if err := add(r.ID, path.Join(paths[r.Silo], r.Name), on, r.SizeBytes, rows[r.ID]); err != nil {
				return err
			}
		}
		for id, row := range rows {
			if row.Deleting != "" {
				if err := add(id, "/"+strings.TrimSuffix(row.Path, ".git"), true, 0, row); err != nil {
					return err
				}
			}
		}
		sort.Slice(out.Repos, func(i, j int) bool { return out.Repos[i].Path < out.Repos[j].Path })
		return nil
	})
	return out, err
}

// Retry is silo mirror retry: a pass over repo id now, or, with no id,
// every repo retried at once whatever its backoff.
func (m *Mirror) Retry(ctx context.Context, id string) (*fenceapi.MirrorStatus, error) {
	if why, err := m.paused(ctx); err != nil {
		return nil, err
	} else if why != "" {
		return nil, errorf(fenceapi.FailedPrecondition, "the mirror is paused: %s", why)
	}
	var seq uint64
	if err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		seq, err = lastSeq(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	if id == "" {
		m.mu.Lock()
		for _, s := range m.repos {
			s.next, s.backoff = time.Time{}, 0
		}
		m.mu.Unlock()
		if err := m.markAll(ctx, seq); err != nil {
			return nil, err
		}
		return m.Status(ctx, false)
	}
	m.markDirty(id, seq, false)
	err := m.pass(ctx, id, seq)
	m.done(id, seq, err)
	return m.Status(ctx, false)
}

// Adopt is silo mirror adopt (local-git.md, "When the uber repo
// disagrees"): for each of repo id's diverged refs (or of those refs
// names, by external or local name, or a branch's or tag's short
// name), either archive the uber repo's tip under
// .../<ts>-<seq>/external/ and let the next pass overwrite it with the
// local tip, or, with keepExternal, record the uber repo's tip as
// mirrored and leave it.
func (m *Mirror) Adopt(ctx context.Context, c fenceapi.Caller, id string, refs []string, keepExternal bool) (*fenceapi.MirrorStatus, error) {
	if err := m.adoptLocked(ctx, c, id, refs, keepExternal); err != nil {
		return nil, err
	}
	return m.Retry(ctx, id)
}

func (m *Mirror) adoptLocked(ctx context.Context, c fenceapi.Caller, id string, refs []string, keepExternal bool) error {
	unlock := m.lock(id)
	defer unlock()
	var cfg *config
	var t *target
	err := m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		cfg, t, err = m.load(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	if cfg == nil || t == nil {
		return errorf(fenceapi.FailedPrecondition, "no mirror is set, or repo %s is gone", id)
	}
	var pick []diverged
	for _, d := range t.Diverged {
		if len(refs) == 0 {
			pick = append(pick, d)
			continue
		}
		for _, r := range refs {
			if r == d.Ref || r == d.Local || "refs/heads/"+r == d.Local || "refs/tags/"+r == d.Local {
				pick = append(pick, d)
				break
			}
		}
	}
	if len(pick) == 0 {
		return errorf(fenceapi.FailedPrecondition, "repo %s has no diverged refs to adopt", t.Path)
	}
	sort.Slice(pick, func(i, j int) bool { return pick[i].Ref < pick[j].Ref })
	var names []string
	for _, d := range pick {
		names = append(names, d.Ref)
	}
	actual, err := m.lsRemote(ctx, cfg, m.opts.Keys, names...)
	if err != nil {
		return err
	}
	archives := map[string]string{}
	if !keepExternal {
		env, opts := remoteEnv(cfg.Credential, m.opts.Keys)
		q := m.quarantine()
		stamp := m.stamp()
		var us []update
		for i, d := range pick {
			sha := actual[d.Ref]
			if sha == "" {
				continue // gone from the uber repo: nothing to keep
			}
			// The one fetch of tenant history the mirror makes: into
			// quarantine.git, never a silo's repo, and only to push it
			// back out as an archive.
			tmp := fmt.Sprintf("refs/adopt/%d", i)
			args := append(append([]string{}, opts...), "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", cfg.URL, "+"+d.Ref+":"+tmp)
			if _, err := m.git(ctx, q, env, nil, args...); err != nil {
				return fmt.Errorf("fetching %s to archive it: %w", d.Ref, err)
			}
			defer m.git(context.WithoutCancel(ctx), q, nil, nil, "update-ref", "-d", tmp)
			rest := strings.TrimPrefix(d.Local, "refs/heads/")
			if strings.HasPrefix(d.Local, "refs/tags/") {
				rest = "tags/" + strings.TrimPrefix(d.Local, "refs/tags/")
			}
			ext := archiveRoot(cfg.Prefix, t.Path) + stamp + "/external/" + rest
			archives[d.Ref] = ext
			us = append(us, update{Dst: ext, Src: sha})
		}
		if len(us) > 0 {
			if _, err := m.push(ctx, cfg, m.opts.Keys, q, us); err != nil {
				return fmt.Errorf("archiving the uber repo's tips: %w", err)
			}
		}
	}
	return m.opts.Store.Tx(ctx, func(tx store.Tx) error {
		s, err := tree.Get(ctx, tx, t.Silo)
		if errors.Is(err, tree.ErrNotFound) {
			s, err = tree.Root(ctx, tx)
		}
		if err != nil {
			return err
		}
		for _, d := range pick {
			sha := actual[d.Ref]
			if ext, ok := archives[d.Ref]; ok {
				if err := setRecord(ctx, tx, t.ID, ext, "", sha); err != nil {
					return err
				}
			}
			if err := setRecord(ctx, tx, t.ID, d.Ref, d.Local, sha); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM mirror_diverged WHERE ref = ?`, d.Ref); err != nil {
				return err
			}
			if err := fenceapi.Audit(ctx, tx, s, c, fenceapi.KindMirrorAdopt, "", "OK", map[string]any{
				"repo": t.ID, "ref": d.Ref, "external": sha, "keep_external": keepExternal, "archive": archives[d.Ref]}); err != nil {
				return err
			}
		}
		return nil
	})
}
