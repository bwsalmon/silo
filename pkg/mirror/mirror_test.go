package mirror_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/mirror"
	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/tree"
)

var ctx = context.Background()

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitstore.Env(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// proxy is the fault-injecting proxy in front of the uber repo's git
// http-backend.
type proxy struct {
	mode    atomic.Value // "", "refuse" or "hang"
	maxBody atomic.Int64 // refuse receive-pack bodies over this, if > 0
	pushes  atomic.Int64 // receive-pack POSTs that reached the backend
}

// env is one test's silod: a store with a tree, a repo host, an uber
// repo behind the proxy, and a Mirror.
type env struct {
	t     *testing.T
	db    store.Store
	host  *gitstore.Host
	uber  string // the uber repo's directory
	url   string
	proxy *proxy
	m     *mirror.Mirror
	silos map[string]*tree.Silo
	work  string // a scratch repo to make commits in
}

func newEnv(t *testing.T, o mirror.Options) *env {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, silos: map[string]*tree.Silo{}, proxy: &proxy{}}
	e.proxy.mode.Store("")
	err = db.Tx(ctx, func(tx store.Tx) error {
		root, err := tree.CreateRoot(ctx, tx, "root", tree.BackingLocal, nil)
		if err != nil {
			return err
		}
		a, err := tree.CreateChild(ctx, tx, root.ID, "team-a", tree.BackingLocal, nil)
		if err != nil {
			return err
		}
		x, err := tree.CreateChild(ctx, tx, a.ID, "x", tree.BackingLocal, nil)
		e.silos["/"], e.silos["/team-a"], e.silos["/team-a/x"] = root, a, x
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "silos"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.host = &gitstore.Host{Storage: &storage.Dir{Root: filepath.Join(dir, "silos")}}
	e.uber = filepath.Join(dir, "uber", "uber.git")
	git(t, "", "init", "--quiet", "--bare", e.uber)
	git(t, e.uber, "config", "http.receivepack", "true")
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(e.uber), "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch e.proxy.mode.Load() {
		case "refuse":
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		case "hang":
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			body, _ := io.ReadAll(r.Body)
			if max := e.proxy.maxBody.Load(); max > 0 && int64(len(body)) > max {
				// GitHub's 2 GB refusal, scaled down.
				http.Error(w, "push exceeds the size limit", http.StatusRequestEntityTooLarge)
				return
			}
			if len(body) > 4 { // not git's 0000 probe of a chunked post
				e.proxy.pushes.Add(1)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("Content-Length", fmt.Sprint(len(body)))
			r.Header.Del("Transfer-Encoding")
			r.TransferEncoding = nil
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e.url = srv.URL + "/uber.git"
	o.Store, o.Repos = db, e.host
	o.Keys, o.Dir = filepath.Join(dir, "keys", "mirror"), filepath.Join(dir, "mirror")
	if o.PushTimeout == 0 {
		o.PushTimeout = 20 * time.Second
	}
	o.Logf = t.Logf
	e.m = mirror.New(o)
	e.work = filepath.Join(dir, "work")
	git(t, "", "init", "--quiet", "-b", "main", e.work)
	return e
}

func (e *env) set() {
	e.t.Helper()
	if _, err := e.m.Set(ctx, fenceapi.Caller{Silo: e.silos["/"].ID}, fenceapi.MirrorSettings{URL: e.url, Credential: "token"}); err != nil {
		e.t.Fatalf("Set: %v", err)
	}
}

// repo makes repo name in the silo at path silo.
func (e *env) repo(silo, name string) *repos.Repo {
	e.t.Helper()
	r := &repos.Repo{ID: repos.NewID(), Silo: e.silos[silo].ID, Name: name, DefaultBranch: "main"}
	if _, _, err := e.host.Create(ctx, r.Silo, r.ID, "main", 1, nil); err != nil {
		e.t.Fatal(err)
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error { return repos.Create(ctx, tx, r) }); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) path(r *repos.Repo) string {
	p, err := e.host.Path(r.Silo, r.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

var commits atomic.Int64

// commit makes a commit in the scratch repo on top of parent ("" for a
// root commit), with size random bytes in a new file, and returns it.
func (e *env) commit(parent string, size int) string {
	e.t.Helper()
	n := commits.Add(1)
	if parent == "" {
		git(e.t, e.work, "checkout", "--quiet", "--orphan", fmt.Sprintf("o%d", n))
		git(e.t, e.work, "rm", "-rf", "--quiet", "--ignore-unmatch", ".")
	} else {
		git(e.t, e.work, "checkout", "--quiet", "--detach", parent)
	}
	b := make([]byte, size)
	rand.Read(b)
	if err := os.WriteFile(filepath.Join(e.work, fmt.Sprintf("f%d", n)), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
	git(e.t, e.work, "add", ".")
	git(e.t, e.work, "commit", "--quiet", "-m", fmt.Sprint("c", n))
	return git(e.t, e.work, "rev-parse", "HEAD")
}

// push puts sha at ref in r ("" deletes it), as a local push would:
// with the objects, with the pre-receive hook's archive ref if it
// orphans the old tip, and marking r dirty.
func (e *env) push(r *repos.Repo, ref, sha string) {
	e.t.Helper()
	p := e.path(r)
	old, _ := exec.Command("git", "-C", p, "rev-parse", "-q", "--verify", ref).Output()
	if sha != "" {
		git(e.t, e.work, "push", "--quiet", "--force", p, sha+":"+ref)
	} else {
		git(e.t, p, "update-ref", "-d", ref)
	}
	if o := strings.TrimSpace(string(old)); o != "" && sha != o {
		if sha == "" || exec.Command("git", "-C", p, "merge-base", "--is-ancestor", o, sha).Run() != nil {
			if exec.Command("git", "-C", p, "merge-base", "--is-ancestor", o, "--all").Run() != nil || sha != "" {
				name := fmt.Sprintf("refs/silo-archive/%s-%d/%s", time.Now().UTC().Format(repos.ArchiveTime), commits.Add(1), strings.TrimPrefix(ref, "refs/"))
				git(e.t, p, "update-ref", name, o)
			}
		}
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error { _, err := repos.MarkDirty(ctx, tx, r); return err }); err != nil {
		e.t.Fatal(err)
	}
}

// uberRefs is the uber repo's refs.
func (e *env) uberRefs() map[string]string {
	out := map[string]string{}
	s := git(e.t, e.uber, "for-each-ref", "--format=%(objectname) %(refname)")
	for _, line := range strings.Split(s, "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok {
			out[name] = sha
		}
	}
	return out
}

func (e *env) retry(r *repos.Repo) fenceapi.MirrorRepo {
	e.t.Helper()
	st, err := e.m.Retry(ctx, r.ID)
	if err != nil {
		e.t.Fatalf("Retry: %v", err)
	}
	for _, mr := range st.Repos {
		if mr.ID == r.ID {
			return mr
		}
	}
	return fenceapi.MirrorRepo{ID: r.ID, State: fenceapi.MirrorInSync}
}

func (e *env) audits(kind string) int {
	var n int
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit WHERE kind = ?`, kind).Scan(&n)
	}); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func under(refs map[string]string, root string) map[string]string {
	out := map[string]string{}
	for name, sha := range refs {
		if strings.HasPrefix(name, root) {
			out[name] = sha
		}
	}
	return out
}

func hasArchive(refs map[string]string, suffix, sha string) bool {
	for name, s := range refs {
		if strings.HasSuffix(name, suffix) && s == sha {
			return true
		}
	}
	return false
}

func TestMirrorsBranchesTagsAndArchives(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	e.set()
	r := e.repo("/team-a/x", "foo")
	a := e.commit("", 10)
	b := e.commit(a, 10)
	e.push(r, "refs/heads/main", b)
	e.push(r, "refs/heads/feature/a", a)
	e.push(r, "refs/tags/v1.0", a)
	if mr := e.retry(r); mr.State != fenceapi.MirrorInSync {
		t.Fatalf("after the first pass: %+v", mr)
	}
	got := e.uberRefs()
	for ref, want := range map[string]string{
		"refs/heads/silo/team-a/x/foo.git/main":      b,
		"refs/heads/silo/team-a/x/foo.git/feature/a": a,
		"refs/tags/silo/team-a/x/foo.git/v1.0":       a,
	} {
		if got[ref] != want {
			t.Errorf("%s = %q, want %q (uber: %v)", ref, got[ref], want, got)
		}
	}
	if _, ok := got["refs/heads/silo/"+mirror.ProbeRef]; ok {
		t.Errorf("the probe's branch is left behind")
	}

	// A force-push becomes the archive and the update in one push; a
	// deleted branch whose tip main's old tip reaches needs none of its
	// own.
	c := e.commit("", 10)
	e.push(r, "refs/heads/main", c)
	e.push(r, "refs/heads/feature/a", "")
	if mr := e.retry(r); mr.State != fenceapi.MirrorInSync {
		t.Fatalf("after the force-push: %+v", mr)
	}
	got = e.uberRefs()
	if got["refs/heads/silo/team-a/x/foo.git/main"] != c {
		t.Errorf("main = %s, want %s", got["refs/heads/silo/team-a/x/foo.git/main"], c)
	}
	if _, ok := got["refs/heads/silo/team-a/x/foo.git/feature/a"]; ok {
		t.Errorf("feature/a was not deleted")
	}
	if arch := under(got, "refs/silo-archive/team-a/x/foo.git/"); !hasArchive(arch, "/heads/main", b) {
		t.Errorf("no archive of main's old tip %s: %v", b, arch)
	}
	// A pass with nothing to do pushes nothing.
	before := e.proxy.pushes.Load()
	e.retry(r)
	if n := e.proxy.pushes.Load(); n != before {
		t.Errorf("an idle pass pushed %d times", n-before)
	}
}

func TestFaultsLeavePushesPendingThenStalled(t *testing.T) {
	e := newEnv(t, mirror.Options{StallAfter: time.Millisecond, PushTimeout: 2 * time.Second})
	e.set()
	r := e.repo("/team-a/x", "foo")
	a := e.commit("", 10)
	e.push(r, "refs/heads/main", a)

	e.proxy.mode.Store("refuse")
	if mr := e.retry(r); mr.State == fenceapi.MirrorInSync || mr.LastError == "" {
		t.Fatalf("with the uber repo refusing: %+v", mr)
	}
	time.Sleep(5 * time.Millisecond)
	e.proxy.mode.Store("hang")
	mr := e.retry(r)
	if mr.State != fenceapi.MirrorStalled || !strings.Contains(mr.LastError, "deadline") {
		t.Fatalf("past the stall time, with the uber repo hanging: %+v", mr)
	}
	if n := e.audits(fenceapi.KindMirrorStalled); n != 1 {
		t.Errorf("%d mirror_stalled audits, want 1", n)
	}
	e.proxy.mode.Store("refuse")
	e.retry(r)
	if n := e.audits(fenceapi.KindMirrorStalled); n != 1 {
		t.Errorf("%d mirror_stalled audits after another failure, want still 1", n)
	}
	// The local repo kept taking pushes all along.
	b := e.commit(a, 10)
	e.push(r, "refs/heads/main", b)

	e.proxy.mode.Store("")
	if mr := e.retry(r); mr.State != fenceapi.MirrorInSync {
		t.Fatalf("after the uber repo came back: %+v", mr)
	}
	if got := e.uberRefs()["refs/heads/silo/team-a/x/foo.git/main"]; got != b {
		t.Errorf("main = %s, want %s", got, b)
	}
}

func TestDivergedRefsAndAdopt(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep-external=%v", keep), func(t *testing.T) {
			e := newEnv(t, mirror.Options{})
			e.set()
			r := e.repo("/team-a/x", "foo")
			a := e.commit("", 10)
			e.push(r, "refs/heads/main", a)
			e.retry(r)
			// Someone pushes to the uber repo's branch for main.
			h := e.commit(a, 10)
			git(t, e.work, "push", "--quiet", e.uber, h+":refs/heads/silo/team-a/x/foo.git/main")
			l := e.commit(a, 10)
			e.push(r, "refs/heads/main", l)
			e.push(r, "refs/heads/other", a)
			mr := e.retry(r)
			if mr.State != fenceapi.MirrorDiverged || len(mr.Diverged) != 1 || mr.Diverged[0].Actual != h || mr.Diverged[0].Expected != a {
				t.Fatalf("after the lease failed: %+v", mr)
			}
			got := e.uberRefs()
			if got["refs/heads/silo/team-a/x/foo.git/main"] != h || got["refs/heads/silo/team-a/x/foo.git/other"] != a {
				t.Fatalf("the diverged ref was overwritten, or the rest didn't mirror: %v", got)
			}
			if n := e.audits(fenceapi.KindMirrorDiverged); n != 1 {
				t.Errorf("%d mirror_diverged audits", n)
			}
			// It stays diverged until adopted.
			if mr := e.retry(r); mr.State != fenceapi.MirrorDiverged {
				t.Errorf("a later pass: %+v", mr)
			}
			st, err := e.m.Adopt(ctx, fenceapi.Caller{Silo: e.silos["/"].ID}, r.ID, []string{"main"}, keep)
			if err != nil {
				t.Fatal(err)
			}
			got = e.uberRefs()
			if keep {
				// The external tip is kept, and the local main, which
				// doesn't fast-forward it, is held back again.
				if got["refs/heads/silo/team-a/x/foo.git/main"] != h {
					t.Errorf("keep-external moved main: %v", got)
				}
				if len(st.Repos) != 1 || st.Repos[0].State != fenceapi.MirrorDiverged || st.Repos[0].Diverged[0].Expected != h {
					t.Errorf("after adopt --keep-external: %+v", st.Repos)
				}
				// A local push that fast-forwards it goes out.
				f := e.commit(h, 10)
				e.push(r, "refs/heads/main", f)
				if _, err := e.m.Adopt(ctx, fenceapi.Caller{Silo: e.silos["/"].ID}, r.ID, nil, true); err != nil {
					t.Fatal(err)
				}
				if got := e.uberRefs()["refs/heads/silo/team-a/x/foo.git/main"]; got != f {
					t.Errorf("main = %s, want the fast-forward %s", got, f)
				}
				return
			}
			if len(st.Repos) != 0 {
				t.Errorf("after adopt: %+v", st.Repos)
			}
			if got["refs/heads/silo/team-a/x/foo.git/main"] != l {
				t.Errorf("main = %s, want the local %s", got["refs/heads/silo/team-a/x/foo.git/main"], l)
			}
			if !hasArchive(under(got, "refs/silo-archive/team-a/x/foo.git/"), "/external/main", h) {
				t.Errorf("no external archive of %s: %v", h, got)
			}
			// The silo's repo never fetched the external commit.
			if exec.Command("git", "-C", e.path(r), "cat-file", "-e", h).Run() == nil {
				t.Errorf("the silo's repo has the external commit %s", h)
			}
		})
	}
}

func TestChunkedFirstPushAndSizeRefusal(t *testing.T) {
	for _, chunk := range []int64{1 << 40, 300 << 10} {
		t.Run(fmt.Sprint("chunk=", chunk), func(t *testing.T) {
			e := newEnv(t, mirror.Options{ChunkBytes: chunk})
			e.set()
			e.proxy.maxBody.Store(500 << 10)
			r := e.repo("/team-a/x", "big")
			c := ""
			for range 6 {
				c = e.commit(c, 200<<10)
			}
			e.push(r, "refs/heads/main", c)
			before := e.proxy.pushes.Load()
			mr := e.retry(r)
			if chunk > 1<<30 {
				if mr.State == fenceapi.MirrorInSync || !strings.Contains(mr.LastError, "413") {
					t.Fatalf("an unchunked 1.2 MB push past a 500 KB limit: %+v", mr)
				}
				return
			}
			if mr.State != fenceapi.MirrorInSync {
				t.Fatalf("a chunked first push: %+v", mr)
			}
			got := e.uberRefs()
			if got["refs/heads/silo/team-a/x/big.git/main"] != c {
				t.Errorf("main = %q, want %s", got["refs/heads/silo/team-a/x/big.git/main"], c)
			}
			if n := e.proxy.pushes.Load() - before; n < 4 {
				t.Errorf("%d pushes, want a chunked push's several", n)
			}
			for name := range got {
				if strings.Contains(name, "-staging/") {
					t.Errorf("the staging ref %s is left", name)
				}
			}
		})
	}
}

func TestRefBatches(t *testing.T) {
	e := newEnv(t, mirror.Options{BatchRefs: 3})
	e.set()
	r := e.repo("/team-a/x", "many")
	a := e.commit("", 10)
	for i := range 7 {
		e.push(r, fmt.Sprintf("refs/heads/b%d", i), a)
	}
	before := e.proxy.pushes.Load()
	if mr := e.retry(r); mr.State != fenceapi.MirrorInSync {
		t.Fatal(mr)
	}
	if n := e.proxy.pushes.Load() - before; n != 3 {
		t.Errorf("%d pushes for 7 refs in batches of 3", n)
	}
	got := e.uberRefs()
	for i := range 7 {
		if got[fmt.Sprintf("refs/heads/silo/team-a/x/many.git/b%d", i)] != a {
			t.Errorf("b%d missing: %v", i, got)
		}
	}
}

func TestDeleteArchivesFirst(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	e.set()
	r := e.repo("/team-a/x", "foo")
	a := e.commit("", 10)
	b := e.commit("", 10)
	e.push(r, "refs/heads/main", a)
	e.push(r, "refs/tags/v1", a)
	e.retry(r)
	e.push(r, "refs/heads/never-mirrored", b)
	dir := e.path(r)

	err := e.db.Tx(ctx, func(tx store.Tx) error {
		if err := repos.Delete(ctx, tx, r.ID); err != nil {
			return err
		}
		handed, err := e.m.Deleting(ctx, tx, e.silos["/team-a/x"], r, "")
		if err == nil && !handed {
			err = errors.New("the mirror didn't take the deletion over")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.proxy.mode.Store("refuse")
	if mr := e.retry(r); mr.State != fenceapi.MirrorDeleting {
		t.Fatalf("while the uber repo refuses: %+v", mr)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the repo went before its archive: %v", err)
	}
	if st, _ := e.m.Status(ctx, false); st.PendingDeletes != 1 {
		t.Errorf("pending deletes = %d", st.PendingDeletes)
	}
	e.proxy.mode.Store("")
	e.retry(r)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the repo is still on disk: %v", err)
	}
	got := e.uberRefs()
	for name := range got {
		if strings.HasPrefix(name, "refs/heads/silo/team-a/x/foo.git/") || strings.HasPrefix(name, "refs/tags/silo/") {
			t.Errorf("live ref %s is left", name)
		}
	}
	arch := under(got, "refs/silo-archive/team-a/x/foo.git/")
	for suffix, sha := range map[string]string{"/heads/main": a, "/tags/v1": a, "/heads/never-mirrored": b} {
		if !hasArchive(arch, suffix, sha) {
			t.Errorf("no archive of %s at %s: %v", suffix, sha, arch)
		}
	}
	if st, _ := e.m.Status(ctx, true); st.PendingDeletes != 0 || len(st.Repos) != 0 {
		t.Errorf("after the deletion: %+v", st)
	}
}

func TestRenameMovesRefs(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	e.set()
	r := e.repo("/team-a/x", "foo")
	a := e.commit("", 10)
	b := e.commit("", 10)
	e.push(r, "refs/heads/main", a)
	e.push(r, "refs/heads/main", b) // leaves an archive of a
	e.retry(r)
	// /team-a/x becomes /team-a/y.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE silos SET name = 'y', path = '/team-a/y' WHERE id = ?`, e.silos["/team-a/x"].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := e.proxy.pushes.Load()
	if mr := e.retry(r); mr.State != fenceapi.MirrorInSync {
		t.Fatal(mr)
	}
	if n := e.proxy.pushes.Load() - before; n != 1 {
		t.Errorf("a rename took %d pushes, want one atomic one", n)
	}
	got := e.uberRefs()
	if got["refs/heads/silo/team-a/y/foo.git/main"] != b {
		t.Errorf("no main under the new path: %v", got)
	}
	for name := range got {
		if strings.Contains(name, "/team-a/x/") {
			t.Errorf("%s is left under the old path", name)
		}
	}
	if arch := under(got, "refs/silo-archive/team-a/y/foo.git/"); !hasArchive(arch, "/heads/main", a) {
		t.Errorf("the archive didn't move: %v", got)
	}
}

func TestPausedInReportOnly(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	e.set()
	r := e.repo("/team-a/x", "foo")
	if err := store.SetSetting(ctx, e.db, store.SettingReconcile, store.ReconcileReportOnly); err != nil {
		t.Fatal(err)
	}
	var fe *fenceapi.Error
	if _, err := e.m.Retry(ctx, r.ID); !errors.As(err, &fe) || fe.Code != fenceapi.FailedPrecondition {
		t.Errorf("a retry in report-only mode: %v", err)
	}
	if st, _ := e.m.Status(ctx, false); st.Paused == "" {
		t.Errorf("status doesn't say it's paused: %+v", st)
	}
}

func TestRunFollowsPushes(t *testing.T) {
	e := newEnv(t, mirror.Options{Poll: 50 * time.Millisecond})
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
	for e.uberRefs()["refs/heads/silo/team-a/x/foo.git/main"] != a {
		if time.Now().After(deadline) {
			t.Fatal("the loop never mirrored the push")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSetRefusesABranchRule(t *testing.T) {
	e := newEnv(t, mirror.Options{})
	hook := filepath.Join(e.uber, "hooks", "pre-receive")
	script := "#!/bin/sh\nwhile read old new ref; do case $new in 0000000000000000000000000000000000000000) echo \"rule: deletion of $ref is forbidden\" >&2; exit 1;; esac; done\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := e.m.Set(ctx, fenceapi.Caller{Silo: e.silos["/"].ID}, fenceapi.MirrorSettings{URL: e.url, Credential: "token"})
	if err == nil || !strings.Contains(err.Error(), "deleting") {
		t.Fatalf("Set past a rule forbidding deletions: %v", err)
	}
	if st, _ := e.m.Status(ctx, false); st.Configured {
		t.Errorf("the refused mirror was set")
	}
}

func TestSetChecksTheURLAndPinsSSHHostKeys(t *testing.T) {
	scanned := ""
	e := newEnv(t, mirror.Options{KeyScan: func(_ context.Context, host, port string) ([]byte, error) {
		scanned = host + ":" + port
		return []byte(host + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"), nil
	}})
	c := fenceapi.Caller{Silo: e.silos["/"].ID}
	// No ssh server answers on port 1: the probe fails, and nothing is
	// kept. The host was scanned to pin its keys first.
	if _, err := e.m.Set(ctx, c, fenceapi.MirrorSettings{URL: "ssh://git@127.0.0.1:1/acme/agents.git", Credential: "key"}); err == nil {
		t.Fatal("Set with no ssh server succeeded")
	}
	if scanned != "127.0.0.1:1" {
		t.Errorf("scanned %q for host keys", scanned)
	}
	for _, s := range []fenceapi.MirrorSettings{
		{URL: "ftp://x/y.git", Credential: "key"},
		{URL: e.url},
		{URL: e.url, Credential: "t", Prefix: "Not/A/Label"},
	} {
		if _, err := e.m.Set(ctx, c, s); err == nil {
			t.Errorf("Set(%+v) succeeded", s)
		}
	}
	if st, _ := e.m.Status(ctx, false); st.Configured {
		t.Errorf("a refused mirror was set")
	}
}
