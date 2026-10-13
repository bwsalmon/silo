package fenceapi_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// fakeMirror records what the fence API asks of the mirror.
type fakeMirror struct {
	set      fenceapi.MirrorSettings
	retried  []string
	adopted  string
	keep     bool
	deleting []string // repo ids handed over
	take     bool
}

func (f *fakeMirror) Set(_ context.Context, _ fenceapi.Caller, s fenceapi.MirrorSettings) (*fenceapi.MirrorStatus, error) {
	f.set = s
	return &fenceapi.MirrorStatus{Configured: true, URL: s.URL}, nil
}

func (f *fakeMirror) Status(context.Context, bool) (*fenceapi.MirrorStatus, error) {
	return &fenceapi.MirrorStatus{Configured: f.set.URL != "", URL: f.set.URL}, nil
}

func (f *fakeMirror) Retry(_ context.Context, id string) (*fenceapi.MirrorStatus, error) {
	f.retried = append(f.retried, id)
	return &fenceapi.MirrorStatus{}, nil
}

func (f *fakeMirror) Adopt(_ context.Context, _ fenceapi.Caller, id string, _ []string, keep bool) (*fenceapi.MirrorStatus, error) {
	f.adopted, f.keep = id, keep
	return &fenceapi.MirrorStatus{}, nil
}

func (f *fakeMirror) Deleting(_ context.Context, _ store.Tx, _ *tree.Silo, r *repos.Repo, _ string) (bool, error) {
	f.deleting = append(f.deleting, r.ID)
	return f.take, nil
}

// The mirror's calls are the root's (local-git.md §8), and a repo
// deletion the mirror takes over stays DELETING, with the repo on disk,
// until the mirror has archived it.
func TestMirrorCalls(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
	silos := filepath.Join(t.TempDir(), "silos")
	if err := os.Mkdir(silos, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeMirror{take: true}
	e := newEnv(t, fenceapi.Options{Repos: &gitstore.Host{Storage: &storage.Dir{Root: silos}}, Mirror: f})
	x := e.mk(e.root, "-"+e.root, "x", obj{"quota": obj{"global": obj{"repos": 3}}})

	e.fail(fenceapi.PermissionDenied, x, "GET", "/silo/v1/mirror", nil)
	e.fail(fenceapi.PermissionDenied, x, "PUT", "/silo/v1/mirror", obj{"url": "https://example.com/a.git"})
	e.fail(fenceapi.NotFound, e.root, "POST", "/silo/v1/mirror", nil)
	if got := e.ok(e.root, "PUT", "/silo/v1/mirror", obj{"url": "https://example.com/a.git", "credential": "t", "prefix": "p"}); got["url"] != "https://example.com/a.git" {
		t.Errorf("set: %v", got)
	}
	if f.set.Credential != "t" || f.set.Prefix != "p" {
		t.Errorf("the mirror got %+v", f.set)
	}
	if got := e.ok(e.root, "GET", "/silo/v1/mirror", nil); got["configured"] != true {
		t.Errorf("status: %v", got)
	}

	r := e.ok(x, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
	id := r["id"].(string)
	e.ok(e.root, "POST", "/silo/v1/mirror:retry", obj{"repo": "/x/foo"})
	e.ok(e.root, "POST", "/silo/v1/mirror:retry", nil)
	if len(f.retried) != 2 || f.retried[0] != id || f.retried[1] != "" {
		t.Errorf("retried %v", f.retried)
	}
	e.fail(fenceapi.NotFound, e.root, "POST", "/silo/v1/mirror:retry", obj{"repo": "/x/nope"})
	e.ok(e.root, "POST", "/silo/v1/mirror:adopt", obj{"repo": "/x/foo", "keep_external": true})
	if f.adopted != id || !f.keep {
		t.Errorf("adopted %s keep=%v", f.adopted, f.keep)
	}

	op := e.ok(x, "DELETE", "/silo/v1/self/repos/foo", nil)
	if op["state"] != fenceapi.OpDeleting || op["done"] == true {
		t.Errorf("a deletion the mirror took over: %v", op)
	}
	if _, err := os.Stat(filepath.Join(silos, x, "repos", id+".git")); err != nil {
		t.Errorf("the repo went before the mirror archived it: %v", err)
	}
	// The name is free at once.
	e.ok(x, "POST", "/silo/v1/self/repos", obj{"name": "foo"})

	// One the mirror doesn't take goes at once.
	f.take = false
	r = e.ok(x, "POST", "/silo/v1/self/repos", obj{"name": "bar"})
	if op := e.ok(x, "DELETE", "/silo/v1/self/repos/bar", nil); op["done"] != true {
		t.Errorf("a deletion the mirror left: %v", op)
	}
	if _, err := os.Stat(filepath.Join(silos, x, "repos", r["id"].(string)+".git")); !os.IsNotExist(err) {
		t.Errorf("the repo is still on disk: %v", err)
	}
	if len(f.deleting) != 2 {
		t.Errorf("the mirror was told of %d deletions", len(f.deleting))
	}
}

// fakeExternals is a mirror that maps between the views.
type fakeExternals struct {
	fakeMirror
	which fenceapi.RefWhich
	local string
}

func (f *fakeExternals) External(_ context.Context, _, local string, out *fenceapi.RepoExternal) error {
	f.local = local
	out.State, out.URL = fenceapi.MirrorInSync, "https://example.com/uber.git"
	return nil
}

func (f *fakeExternals) Which(context.Context, string) (*fenceapi.RefWhich, error) {
	w := f.which
	return &w, nil
}

// silo repo external and which (local-git.md, §9) are for whoever may
// read the repo; a repo that is gone, for whoever may read the deepest
// silo still on its path.
func TestExternalCalls(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
	silos := filepath.Join(t.TempDir(), "silos")
	if err := os.Mkdir(silos, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeExternals{}
	e := newEnv(t, fenceapi.Options{Repos: &gitstore.Host{Storage: &storage.Dir{Root: silos}}, Mirror: f})
	x := e.mk(e.root, "-"+e.root, "x", obj{"quota": obj{"global": obj{"repos": 3}}})
	y := e.mk(e.root, "-"+e.root, "y", nil)
	e.ok(x, "POST", "/silo/v1/self/repos", obj{"name": "foo"})

	got := e.ok(e.root, "GET", "/silo/v1/silos/-"+x+"/repos/foo/external?branch=feature/a", nil)
	if got["state"] != fenceapi.MirrorInSync || got["repo"] != "/x/foo" || got["silo_id"] != x || f.local != "refs/heads/feature/a" {
		t.Errorf("external: %v (local %q)", got, f.local)
	}
	e.ok(x, "GET", "/silo/v1/self/repos/foo/external?tag=v1.0", nil)
	if f.local != "refs/tags/v1.0" {
		t.Errorf("external tag=v1.0 asked for %q", f.local)
	}
	e.fail(fenceapi.NotFound, y, "GET", "/silo/v1/silos/-"+x+"/repos/foo/external", nil)
	e.fail(fenceapi.InvalidArgument, x, "GET", "/silo/v1/self/repos/foo/external?branch=a&tag=b", nil)
	e.fail(fenceapi.NotFound, x, "GET", "/silo/v1/self/repos/nope/external", nil)

	e.fail(fenceapi.InvalidArgument, x, "GET", "/silo/v1/mirror:which", nil)
	e.fail(fenceapi.NotFound, x, "POST", "/silo/v1/mirror:which?ref=r", nil)
	f.which = fenceapi.RefWhich{Kind: fenceapi.RefBranch, Repo: "/x/foo", Silo: "/x", Exists: true}
	for _, c := range []string{x, e.root} {
		if got := e.ok(c, "GET", "/silo/v1/mirror:which?ref=r", nil); got["repo"] != "/x/foo" {
			t.Errorf("which: %v", got)
		}
	}
	e.fail(fenceapi.NotFound, y, "GET", "/silo/v1/mirror:which?ref=r", nil)
	f.which = fenceapi.RefWhich{Kind: fenceapi.RefBranch, Repo: "/x/gone/deeper/old", Silo: "/x/gone/deeper"}
	e.ok(x, "GET", "/silo/v1/mirror:which?ref=r", nil)
	e.fail(fenceapi.NotFound, y, "GET", "/silo/v1/mirror:which?ref=r", nil)
}
