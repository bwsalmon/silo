package mirror_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/mirror"
	"github.com/bwsalmon/silo/pkg/store"
)

func (e *env) index() {
	e.t.Helper()
	if err := e.m.Index(ctx); err != nil {
		e.t.Fatalf("Index: %v", err)
	}
}

func (e *env) status() *fenceapi.MirrorStatus {
	e.t.Helper()
	st, err := e.m.Status(ctx, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// gitlinks is the uber repo's index commit's gitlinks, by path.
func (e *env) gitlinks(commit string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(git(e.t, e.uber, "ls-tree", "-r", commit), "\n") {
		meta, p, _ := strings.Cut(line, "\t")
		if f := strings.Fields(meta); len(f) == 3 && f[0] == "160000" {
			out[p] = f[2]
		}
	}
	return out
}

func TestIndexCommitsGitlinks(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	e.set()
	foo := e.repo("/team-a/x", "foo")
	bar := e.repo("/", "bar")
	e.repo("/team-a", "empty") // no default branch yet: no entry
	a := e.commit("", 10)
	b := e.commit("", 10)
	e.push(foo, "refs/heads/main", a)
	e.push(foo, "refs/heads/feature", b)
	e.push(bar, "refs/heads/main", b)

	// Nothing mirrored yet: the index has no gitlinks.
	e.index()
	first := e.uberRefs()["refs/heads/main"]
	if first == "" {
		t.Fatalf("no index branch: %v", e.uberRefs())
	}
	if links := e.gitlinks(first); len(links) != 0 {
		t.Errorf("gitlinks before anything was mirrored: %v", links)
	}

	e.retry(foo)
	e.retry(bar)
	e.index()
	tip := e.uberRefs()["refs/heads/main"]
	if got := git(t, e.uber, "rev-parse", tip+"^"); got != first {
		t.Errorf("parent %s, want the previous index commit %s", got, first)
	}
	want := map[string]string{"team-a/x/foo.git": a, "bar.git": b}
	if links := e.gitlinks(tip); len(links) != 2 || links["team-a/x/foo.git"] != a || links["bar.git"] != b {
		t.Errorf("gitlinks %v, want %v", links, want)
	}
	mods := git(t, e.uber, "show", tip+":.gitmodules")
	wantMods := "[submodule \"bar.git\"]\n\tpath = bar.git\n\turl = ./\n\tbranch = silo/bar.git/main\n" +
		"[submodule \"team-a/x/foo.git\"]\n\tpath = team-a/x/foo.git\n\turl = ./\n\tbranch = silo/team-a/x/foo.git/main"
	if mods != wantMods {
		t.Errorf(".gitmodules:\n%s\nwant:\n%s", mods, wantMods)
	}
	readme := git(t, e.uber, "show", tip+":README.md")
	for _, s := range []string{"| `/team-a/x/foo` | `" + e.silos["/team-a/x"].ID + "` | `main` | `silo/team-a/x/foo.git/main` |",
		"| `/bar` |", "UBER=$(git remote get-url origin)", "silo/**"} {
		if !strings.Contains(readme, s) {
			t.Errorf("README lacks %q:\n%s", s, readme)
		}
	}
	msg := git(t, e.uber, "log", "-1", "--format=%an <%ae>%n%cn <%ce>%n%B", tip)
	for _, s := range []string{"silo <silo@localhost>\nsilo <silo@localhost>", "team-a/x/foo.git: added", "bar.git: added", "\n\n" + mirror.IndexTrailer} {
		if !strings.Contains(msg, s) {
			t.Errorf("commit lacks %q:\n%s", s, msg)
		}
	}
	if st := e.status(); st.IndexTip != tip || st.IndexDiverged != "" || st.IndexError != "" {
		t.Errorf("status: %+v", st)
	}

	// Nothing moved: no commit.
	e.index()
	if got := e.uberRefs()["refs/heads/main"]; got != tip {
		t.Errorf("an index commit with nothing moved: %s", got)
	}

	// A push moves one gitlink; deleting a repo drops its entry.
	c := e.commit(a, 10)
	e.push(foo, "refs/heads/main", c)
	e.retry(foo)
	e.index()
	next := e.uberRefs()["refs/heads/main"]
	if links := e.gitlinks(next); links["team-a/x/foo.git"] != c {
		t.Errorf("foo's gitlink %s, want %s", links["team-a/x/foo.git"], c)
	}
	if msg := git(t, e.uber, "log", "-1", "--format=%B", next); !strings.Contains(msg, "team-a/x/foo.git: "+a[:12]+" to "+c[:12]) || strings.Contains(msg, "bar.git") {
		t.Errorf("message:\n%s", msg)
	}

	// The index clones: url = ./ resolves to the uber repo, and the
	// gitlinked commit is fetched by id with --single-branch.
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, "", "clone", "--quiet", "-b", "main", e.url, clone)
	git(t, clone, "submodule", "update", "--init", "--single-branch", "team-a/x/foo.git")
	if got := git(t, filepath.Join(clone, "team-a/x/foo.git"), "rev-parse", "HEAD"); got != c {
		t.Errorf("the submodule checked out %s, want %s", got, c)
	}
}

func TestIndexRefusesABranchItDidNotWrite(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	h := e.commit("", 10)
	git(t, e.work, "push", "--quiet", e.uber, h+":refs/heads/main")
	root := fenceapi.Caller{Silo: e.silos["/"].ID}
	_, err := e.m.Set(ctx, root, fenceapi.MirrorSettings{URL: e.url, Credential: "token"})
	var fe *fenceapi.Error
	if !errors.As(err, &fe) || fe.Code != fenceapi.FailedPrecondition || !strings.Contains(fe.Message, "--index-branch") {
		t.Fatalf("Set over a people's main: %v", err)
	}
	if _, err := e.m.Set(ctx, root, fenceapi.MirrorSettings{URL: e.url, Credential: "token", IndexBranch: "silo-index"}); err != nil {
		t.Fatalf("Set with another index branch: %v", err)
	}
	r := e.repo("/team-a/x", "foo")
	a := e.commit("", 10)
	e.push(r, "refs/heads/main", a)
	e.retry(r)
	e.index()
	tip := e.uberRefs()["refs/heads/silo-index"]
	if tip == "" || e.uberRefs()["refs/heads/main"] != h {
		t.Fatalf("index on silo-index, main left alone: %v", e.uberRefs())
	}

	// Someone pushes to the index branch: silod stops writing it.
	human := e.commit("", 10)
	git(t, e.work, "push", "--quiet", "--force", e.uber, human+":refs/heads/silo-index")
	b := e.commit(a, 10)
	e.push(r, "refs/heads/main", b)
	e.retry(r)
	e.index()
	if got := e.uberRefs()["refs/heads/silo-index"]; got != human {
		t.Errorf("silod wrote over the human's index commit: %s", got)
	}
	st := e.status()
	if !strings.Contains(st.IndexDiverged, human) {
		t.Errorf("status doesn't say the index diverged: %+v", st)
	}
	if n := e.audits(fenceapi.KindMirrorIndexDiverged); n != 1 {
		t.Errorf("%d %s audits, want 1", n, fenceapi.KindMirrorIndexDiverged)
	}
	e.index()
	if n := e.audits(fenceapi.KindMirrorIndexDiverged); n != 1 {
		t.Errorf("a diverged index is audited again: %d", n)
	}

	// set again can't take the branch back, but can turn the index off.
	if _, err := e.m.Set(ctx, root, fenceapi.MirrorSettings{URL: e.url, Credential: "token", IndexBranch: "silo-index"}); err == nil {
		t.Errorf("Set took over a branch with a human's commit on top")
	}
	if _, err := e.m.Set(ctx, root, fenceapi.MirrorSettings{URL: e.url, Credential: "token", NoIndex: true}); err != nil {
		t.Fatal(err)
	}
	if st := e.status(); st.IndexBranch != "" || st.IndexDiverged != "" {
		t.Errorf("status with no index: %+v", st)
	}
	e.index()
	if got := e.uberRefs()["refs/heads/silo-index"]; got != human {
		t.Errorf("the index branch changed with no index: %s", got)
	}
}

func TestIndexRebuiltAfterLoss(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	e.set()
	r := e.repo("/team-a/x", "foo")
	a := e.commit("", 10)
	e.push(r, "refs/heads/main", a)
	e.retry(r)
	e.index()
	first := e.uberRefs()["refs/heads/main"]

	// mirror/index.git is lost: the next commit's parent is fetched.
	idx := filepath.Join(e.dir, "mirror", "index.git")
	if err := os.RemoveAll(idx); err != nil {
		t.Fatal(err)
	}
	b := e.commit(a, 10)
	e.push(r, "refs/heads/main", b)
	e.retry(r)
	e.index()
	second := e.uberRefs()["refs/heads/main"]
	if second == first || git(t, e.uber, "rev-parse", second+"^") != first {
		t.Errorf("after losing index.git: tip %s, want a child of %s", second, first)
	}

	// The store forgets the tip (a lost store, or a new silo mirror set):
	// silod's own external tip is taken as the parent.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE settings SET value = '' WHERE name = 'mirror.index_tip'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(idx); err != nil {
		t.Fatal(err)
	}
	c := e.commit(b, 10)
	e.push(r, "refs/heads/main", c)
	e.retry(r)
	e.index()
	third := e.uberRefs()["refs/heads/main"]
	if third == second || git(t, e.uber, "rev-parse", third+"^") != second {
		t.Errorf("after losing the tip: %s, want a child of %s", third, second)
	}
	if st := e.status(); st.IndexDiverged != "" || st.IndexTip != third {
		t.Errorf("status: %+v", st)
	}
	// index.git holds only index commits and trees, never a repo's.
	if exec.Command("git", "-C", idx, "cat-file", "-e", c).Run() == nil {
		t.Errorf("index.git has the repo's commit %s", c)
	}
}

func TestRunWritesTheIndex(t *testing.T) {
	e := newEnv(t, mirror.Options{Poll: 50 * time.Millisecond, IndexDebounce: 100 * time.Millisecond})
	e.set()
	r := e.repo("/team-a/x", "foo")
	c, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.m.Run(c) }()
	defer func() { cancel(); wg.Wait() }()
	a := e.commit("", 10)
	e.push(r, "refs/heads/main", a)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if tip := e.uberRefs()["refs/heads/main"]; tip != "" && e.gitlinks(tip)["team-a/x/foo.git"] == a {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop never wrote the index")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
