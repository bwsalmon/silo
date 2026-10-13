package mirror_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/mirror"
	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
)

func (e *env) external(r *repos.Repo, silo, local string) *fenceapi.RepoExternal {
	e.t.Helper()
	out := &fenceapi.RepoExternal{ID: r.ID, Silo: silo, SiloID: e.silos[silo].ID}
	if err := e.m.External(ctx, r.ID, local, out); err != nil {
		e.t.Fatalf("External: %v", err)
	}
	return out
}

func (e *env) which(ref string) *fenceapi.RefWhich {
	e.t.Helper()
	w, err := e.m.Which(ctx, ref)
	if err != nil {
		e.t.Fatalf("Which(%s): %v", ref, err)
	}
	return w
}

// silo repo external and silo repo which (local-git.md, §9).
func TestExternalAndWhich(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	foo := e.repo("/team-a/x", "foo")
	if x := e.external(foo, "/team-a/x", ""); x.State != fenceapi.MirrorUnconfigured || x.URL != "" {
		t.Errorf("before silo mirror set: %+v", x)
	}
	if _, err := e.m.Which(ctx, "refs/heads/silo/team-a/x/foo.git/main"); err == nil {
		t.Errorf("Which with no mirror set: no error")
	}
	e.set()
	a := e.commit("", 10)
	b := e.commit(a, 10)
	e.push(foo, "refs/heads/main", b)
	e.push(foo, "refs/heads/feature/a", a)
	e.push(foo, "refs/tags/v1.0", a)
	e.retry(foo)
	e.push(foo, "refs/heads/main", e.commit("", 10)) // archives b
	e.retry(foo)
	e.index()

	x := e.external(foo, "/team-a/x", "")
	if x.URL != e.url || x.State != fenceapi.MirrorInSync || x.MirroredTime == nil {
		t.Errorf("external: %+v", x)
	}
	for got, want := range map[string]string{
		x.Branches:      "refs/heads/silo/team-a/x/foo.git/",
		x.Tags:          "refs/tags/silo/team-a/x/foo.git/",
		x.Archives:      "refs/silo-archive/team-a/x/foo.git/",
		x.DefaultBranch: "silo/team-a/x/foo.git/main",
		x.Index:         "main:team-a/x/foo.git",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if x.ArchiveRefs != 1 {
		t.Errorf("archive refs %d, want 1 (uber: %v)", x.ArchiveRefs, under(e.uberRefs(), x.Archives))
	}
	x = e.external(foo, "/team-a/x", "refs/heads/feature/a")
	if x.Ref == nil || x.Ref.External != "refs/heads/silo/team-a/x/foo.git/feature/a" || x.Ref.Commit != a {
		t.Errorf("external --branch feature/a: %+v", x.Ref)
	}
	if x := e.external(foo, "/team-a/x", "refs/heads/nope"); x.Ref.Commit != "" {
		t.Errorf("a branch that isn't mirrored has commit %s", x.Ref.Commit)
	}

	x0 := e.silos["/team-a/x"].ID
	for _, c := range []struct {
		in         string
		kind, ref  string
		local, sha string
	}{
		{"refs/heads/silo/team-a/x/foo.git/feature/a", fenceapi.RefBranch, "refs/heads/silo/team-a/x/foo.git/feature/a", "refs/heads/feature/a", a},
		{"silo/team-a/x/foo.git/feature/a", fenceapi.RefBranch, "refs/heads/silo/team-a/x/foo.git/feature/a", "refs/heads/feature/a", a},
		{"https://github.com/acme/agents/tree/silo/team-a/x/foo.git/feature/a", fenceapi.RefBranch, "refs/heads/silo/team-a/x/foo.git/feature/a", "refs/heads/feature/a", a},
		{"refs/tags/silo/team-a/x/foo.git/v1.0", fenceapi.RefTag, "refs/tags/silo/team-a/x/foo.git/v1.0", "refs/tags/v1.0", a},
		{"main:team-a/x/foo.git", fenceapi.RefIndex, "main:team-a/x/foo.git", "refs/heads/main", ""},
	} {
		w := e.which(c.in)
		if w.Kind != c.kind || w.Ref != c.ref || w.Local != c.local || w.Repo != "/team-a/x/foo" || w.Silo != "/team-a/x" ||
			!w.Exists || w.ID != foo.ID || w.SiloID != x0 || (c.sha != "" && w.Commit != c.sha) {
			t.Errorf("which %s: %+v", c.in, w)
		}
	}
	var archive string
	for ref, sha := range under(e.uberRefs(), "refs/silo-archive/team-a/x/foo.git/") {
		if sha == b {
			archive = ref
		}
	}
	if w := e.which(archive); w.Kind != fenceapi.RefArchive || !strings.HasPrefix(w.Local, "refs/silo-archive/") ||
		!strings.HasSuffix(w.Local, "/heads/main") || w.Commit != b {
		t.Errorf("which %s: %+v", archive, w)
	}
	for _, bad := range []string{"refs/heads/other/team-a/x/foo.git/main", "refs/heads/silo/team-a/x/main",
		"refs/heads/silo/team-a/x/foo.git", "refs/silo-archive/team-a/x/foo.git/20261012T185400Z-1/main", "main:team-a/x/foo.git/main"} {
		_, err := e.m.Which(ctx, bad)
		var fe *fenceapi.Error
		if !errors.As(err, &fe) || fe.Code != fenceapi.InvalidArgument {
			t.Errorf("Which(%s) = %v, want INVALID_ARGUMENT", bad, err)
		}
	}

	// A repo that is gone: its silo id from silod's records while it
	// keeps them, and then from the index's README.
	gone := e.repo("/team-a", "gone")
	e.push(gone, "refs/heads/main", a)
	e.retry(gone)
	e.index()
	if err := e.db.Tx(ctx, func(tx store.Tx) error { return repos.Delete(ctx, tx, gone.ID) }); err != nil {
		t.Fatal(err)
	}
	ta := e.silos["/team-a"].ID
	if w := e.which("silo/team-a/gone.git/main"); w.Exists || w.SiloID != ta || w.From != "mirror" || w.ID != gone.ID || w.Commit != a {
		t.Errorf("which, gone, with records: %+v", w)
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		for _, q := range []string{`DELETE FROM mirror_refs WHERE repo = ?`, `DELETE FROM mirror_repos WHERE repo = ?`} {
			if _, err := tx.Exec(ctx, q, gone.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.index() // a commit that drops gone's row
	if w := e.which("silo/team-a/gone.git/main"); w.Exists || w.SiloID != ta || w.From != "index" {
		t.Errorf("which, gone, from the index: %+v", w)
	}
	if w := e.which("silo/team-b/never.git/main"); w.Exists || w.SiloID != "" {
		t.Errorf("which, never there: %+v", w)
	}
}
