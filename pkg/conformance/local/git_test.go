package local_test

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

// The repo and mirror cases of local-git.md §11 item 24, with real git
// clients through the gateway's port 80 as a probe in each silo
// reaches it: who may clone and push (§4), and what the outbound
// mirror keeps in the uber repo (§8).

// TestMain runs the test binary as the git service's pre-receive hook
// when receive-pack runs it so, as silod's main does.
func TestMain(m *testing.M) {
	targets.GitHook()
	os.Exit(m.Run())
}

// gitCase is one case's install, with its Git.
type gitCase struct {
	conformance.F
	g conformance.Git
}

func eachGit(t *testing.T, fn func(t *testing.T, c gitCase)) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
	eachFence(t, func(t *testing.T, f conformance.F) {
		g, ok := f.Fence.(conformance.Git)
		if !ok {
			t.Skipf("not offered: %T has no git service", f.Fence)
		}
		fn(t, gitCase{F: f, g: g})
	})
}

// mk makes a silo as F.Mk does, with a repos quota: a /24 and 8 repos
// for a child of the root, a /26 and 2 below that.
func (c gitCase) mk(parent, name string, extra obj) string {
	c.T.Helper()
	quota := obj{"vcpu_m": 8000, "memory_mb": 8192, "disk_gb": 100, "containers": 8, "networks": 4, "volumes": 4, "repos": 8}
	if parent != "" {
		quota = obj{"vcpu_m": 2000, "memory_mb": 2048, "disk_gb": 30, "containers": 2, "networks": 1, "volumes": 1, "repos": 2}
	}
	body := obj{"quota": obj{"global": quota}}
	for k, v := range extra {
		body[k] = v
	}
	prefix := 24
	if parent != "" {
		prefix = 26
	}
	return c.Mk(parent, name, prefix, body)
}

// url is the git service's base URL from a probe made in silo.
func (c gitCase) url(silo, probe string) string {
	c.T.Helper()
	return c.g.GitURL(c.T, c.Probe(c.T, silo, probe))
}

func (c gitCase) tryGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitstore.Env(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c gitCase) git(dir string, args ...string) string {
	c.T.Helper()
	out, err := c.tryGit(dir, args...)
	if err != nil {
		c.T.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

// eventually runs git until it succeeds, for the first call after a
// change the gateway has to follow (a new silo, a grant, a restored
// silod), which it may answer 503 until it has.
func (c gitCase) eventually(dir string, args ...string) string {
	c.T.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err := c.tryGit(dir, args...)
		if err == nil {
			return strings.TrimSpace(out)
		}
		if time.Now().After(deadline) {
			c.T.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// refused runs git, which must fail saying want.
func (c gitCase) refused(what, want, dir string, args ...string) {
	c.T.Helper()
	out, err := c.tryGit(dir, args...)
	if err == nil || !strings.Contains(out, want) {
		c.T.Errorf("%s: %v, want a refusal with %q:\n%s", what, err, want, out)
	}
}

// work is a fresh work tree on main with url as origin.
func (c gitCase) work(url string) string {
	dir := c.T.TempDir()
	c.git(dir, "init", "-q", "-b", "main")
	c.git(dir, "remote", "add", "origin", url)
	return dir
}

// commit makes a commit in the work tree dir writing content to file.
func (c gitCase) commit(dir, file, content string) string {
	c.T.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		c.T.Fatal(err)
	}
	c.git(dir, "add", file)
	c.git(dir, "commit", "-q", "-m", "write "+file)
	return c.git(dir, "rev-parse", "HEAD")
}

// clone clones url into a new directory.
func (c gitCase) clone(url string) string {
	c.T.Helper()
	dir := filepath.Join(c.T.TempDir(), "c")
	c.eventually("", "clone", "-q", url, dir)
	return dir
}

// pushed makes repo name in silo (at path) and pushes a first commit
// to its main from a probe in silo, and returns that probe's base URL
// and the commit.
func (c gitCase) pushed(silo, path, name string) (string, string) {
	c.T.Helper()
	c.OK(silo, "POST", "/silo/v1/self/repos", obj{"name": name})
	u := c.url(silo, "p"+name)
	w := c.work(u + strings.TrimPrefix(path, "/") + "/" + name + ".git")
	sha := c.commit(w, "README", path+" "+name+"\n")
	c.eventually(w, "push", "-q", "origin", "main")
	return u, sha
}

func TestRepoSiblingIsNotFound(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		a, b := c.mk("", "a", nil), c.mk("", "b", nil)
		_, sha := c.pushed(a, "/a", "foo")
		bu := c.url(b, "pb")
		// b's own repo first, so that the gateway has every silo.
		c.OK(b, "POST", "/silo/v1/self/repos", obj{"name": "own"})
		c.clone(bu + "b/own.git")
		for _, u := range []string{bu + "a/foo.git", bu + "-" + a + "/foo.git"} {
			c.refused("a sibling's clone of "+u, "not found", "", "clone", "-q", u, filepath.Join(t.TempDir(), "c"))
		}
		w := c.work(bu + "a/foo.git")
		c.commit(w, "x", "from b\n")
		c.refused("a sibling's push", "not found", w, "push", "origin", "main")
		if out := c.git("", "ls-remote", c.url(a, "pa2")+"a/foo.git", "main"); !strings.Contains(out, sha) {
			t.Errorf("a's main moved: %s, want %s", out, sha)
		}
	})
}

func TestRepoAncestorClonesAndCantPush(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		a := c.mk("", "a", nil)
		kid := c.mk("/a", "kid", nil)
		_, sha := c.pushed(kid, "/a/kid", "bar")
		anc := c.clone(c.url(a, "pa") + "a/kid/bar.git")
		if got := c.git(anc, "rev-parse", "HEAD"); got != sha {
			t.Errorf("the ancestor's clone is at %s, want %s", got, sha)
		}
		c.commit(anc, "y", "from the parent\n")
		c.refused("an ancestor's push", "403", anc, "push", "origin", "main")
	})
}

func TestRepoManageGrantReadsAndIsCutOff(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		a, ro := c.mk("", "a", nil), c.mk("", "ro", nil)
		c.pushed(ro, "/ro", "app")
		au := c.url(a, "pa")
		c.OK(a, "POST", "/silo/v1/self/repos", obj{"name": "own"})
		c.clone(au + "a/own.git")
		c.refused("a clone with no grant", "not found", "", "clone", "-q", au+"ro/app.git", filepath.Join(t.TempDir(), "c"))

		gr := c.OK(c.Root(), "POST", "/silo/v1/silos/ro/manageGrants", obj{"name": "pipeline", "holder": "/a",
			"scope": obj{"facade": obj{"resources": []string{"repos"}, "access": "read"}, "fence": []string{"read"}},
			"lease": obj{"ttl": "3600s"}})
		c.OK(a, "POST", "/silo/v1/self/manageGrants/"+gr["id"].(string)+":accept", obj{})
		h := c.clone(au + "ro/app.git")
		c.git(h, "fetch", "-q", "origin")
		c.commit(h, "deploy", "x\n")
		c.refused("a read grant's push", "403", h, "push", "origin", "main")

		// A fetch in flight, whose request body never ends.
		conn, err := net.Dial("tcp", strings.TrimPrefix(strings.TrimSuffix(au, "/git/"), "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		io.WriteString(conn, "POST /git/ro/app.git/git-upload-pack HTTP/1.1\r\nHost: silo.internal\r\n"+
			"Content-Type: application/x-git-upload-pack-request\r\nTransfer-Encoding: chunked\r\n\r\n"+
			"5\r\n0032w\r\n")
		done := make(chan struct{})
		go func() { io.Copy(io.Discard, conn); close(done) }()
		time.Sleep(300 * time.Millisecond)
		select {
		case <-done:
			t.Fatal("the holder's fetch ended before its grant did")
		default:
		}
		c.OK(c.Root(), "DELETE", "/silo/v1/silos/ro/manageGrants/pipeline", nil)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the holder's fetch is still open after its grant was revoked")
		}
		c.refused("a fetch after the revoke", "not found", h, "fetch", "origin")
	})
}

func TestRepoSealedPushesAndCantCreate(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		s := c.mk("", "s", nil)
		u, _ := c.pushed(s, "/s", "ci")
		c.OK(c.Root(), "PATCH", "/silo/v1/silos/s", obj{"control": "none"})
		c.Refused(403, "CONTROL_DENIED", s, "POST", "/silo/v1/self/repos", obj{"name": "more"})
		w := c.clone(u + "s/ci.git")
		sha := c.commit(w, "build", "ok\n")
		c.eventually(w, "push", "-q", "origin", "main")
		if out := c.git(w, "ls-remote", "origin", "main"); !strings.Contains(out, sha) {
			t.Errorf("the sealed silo's push didn't land: %s", out)
		}
	})
}

func TestRepoNoEgressClonesAndPushes(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		s := c.mk("", "s", obj{"egress": obj{"allow": []string{}}})
		c.OK(s, "POST", "/silo/v1/self/repos", obj{"name": "app"})
		p := c.Probe(t, s, "pp")
		// The silo has no egress at all.
		res := p.Run([]conformance.Check{{ID: "out", Kind: "http", URL: "https://example.com/", Proxy: "10.255.255.1:3128"}})
		if r := res["out"]; !r.OK || !strings.HasPrefix(r.Detail, "403") {
			if !strings.HasPrefix(r.Detail, conformance.Unsupported) {
				t.Errorf("the silo with no egress reached example.com: %+v", r)
			}
		}
		u := c.g.GitURL(t, p) + "s/app.git"
		w := c.work(u)
		sha := c.commit(w, "README", "offline\n")
		c.eventually(w, "push", "-q", "origin", "main")
		cl := c.clone(u)
		if got := c.git(cl, "rev-parse", "HEAD"); got != sha {
			t.Errorf("the clone is at %s, want %s", got, sha)
		}
	})
}

// mirrorTo sets the install's mirror to a fresh uber repo.
func (c gitCase) mirrorTo() conformance.Uber {
	c.T.Helper()
	u := c.g.Uber(c.T)
	c.OK(c.Root(), "PUT", "/silo/v1/mirror", obj{"url": u.URL(), "credential": u.Credential()})
	return u
}

// waitRef waits until the uber repo has ref at sha.
func (c gitCase) waitRef(u conformance.Uber, ref, sha string) {
	c.T.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for u.Refs()[ref] != sha {
		if time.Now().After(deadline) {
			c.T.Fatalf("the uber repo's %s is %q, want %s; its refs: %v", ref, u.Refs()[ref], sha, u.Refs())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitState waits until the mirror's status gives repo (its path, as
// /a/foo) state, and returns that repo's status.
func (c gitCase) waitState(repo, state string) obj {
	c.T.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := c.OK(c.Root(), "GET", "/silo/v1/mirror?all=true", nil)
		repos, _ := st["repos"].([]any)
		for _, r := range repos {
			if r := r.(obj); r["path"] == repo && r["state"] == state {
				return r
			}
		}
		if time.Now().After(deadline) {
			c.T.Fatalf("%s never became %s: %v", repo, state, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func under(refs map[string]string, prefix string) map[string]string {
	out := map[string]string{}
	for name, sha := range refs {
		if strings.HasPrefix(name, prefix) {
			out[name] = sha
		}
	}
	return out
}

func TestMirrorOffKeepsASubtreeOut(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		u := c.mirrorTo()
		a, b := c.mk("", "a", nil), c.mk("", "b", nil)
		c.OK(c.Root(), "PATCH", "/silo/v1/silos/b", obj{"repos": obj{"mirror": false}})
		kid := c.mk("/b", "kid", nil)
		// The policy is never-looser: whatever kid's own setting
		// answers, its repos stay out.
		c.API(kid, "PATCH", "/silo/v1/self", obj{"repos": obj{"mirror": true}})
		_, bsha := c.pushed(b, "/b", "top")
		_, ksha := c.pushed(kid, "/b/kid", "bar")
		_, asha := c.pushed(a, "/a", "foo")
		c.waitRef(u, "refs/heads/silo/a/foo.git/main", asha)
		c.OK(c.Root(), "POST", "/silo/v1/mirror:retry", obj{})
		refs := u.Refs()
		for _, p := range []string{"refs/heads/silo/b/", "refs/tags/silo/b/", "refs/silo-archive/b/"} {
			if got := under(refs, p); len(got) > 0 {
				t.Errorf("the uber repo has refs of b's subtree: %v", got)
			}
		}
		for name, sha := range refs {
			if sha == bsha || sha == ksha {
				t.Errorf("the uber repo has b's subtree's commit at %s", name)
			}
		}
	})
}

func TestMirrorForcePushArchives(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		u := c.mirrorTo()
		a := c.mk("", "a", nil)
		au, _ := c.pushed(a, "/a", "foo")
		w := c.clone(au + "a/foo.git")
		old := c.commit(w, "more", "two\n")
		c.git(w, "push", "-q", "origin", "main")
		c.waitRef(u, "refs/heads/silo/a/foo.git/main", old)

		c.git(w, "checkout", "-q", "--orphan", "rewritten")
		c.git(w, "rm", "-rfq", ".")
		forced := c.commit(w, "new", "rewritten\n")
		c.git(w, "push", "-q", "--force", "origin", "rewritten:main")
		c.waitRef(u, "refs/heads/silo/a/foo.git/main", forced)

		l := c.OK(a, "GET", "/silo/v1/self/repos/foo/archives", nil)
		as, _ := l["archives"].([]any)
		if len(as) != 1 || as[0].(obj)["commit"] != old {
			t.Fatalf("the repo's archives: %v, want one of %s", l, old)
		}
		name := as[0].(obj)["name"].(string)
		// The uber repo's archive is the repo's own, under its path, at
		// the same commit.
		want := "refs/silo-archive/a/foo.git/" + name
		if got := u.Refs()[want]; got != old {
			t.Errorf("the uber repo's %s is %q, want %s; its archives: %v", want, got, old, under(u.Refs(), "refs/silo-archive/"))
		}
	})
}

func TestMirrorCatchesUpAfterTheUberRepoIsDown(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		u := c.mirrorTo()
		a := c.mk("", "a", nil)
		au, first := c.pushed(a, "/a", "foo")
		c.waitRef(u, "refs/heads/silo/a/foo.git/main", first)

		u.SetDown(true)
		w := c.clone(au + "a/foo.git")
		var last string
		for i := 0; i < 3; i++ {
			last = c.commit(w, "f", strings.Repeat("x", i+1))
			c.git(w, "push", "-q", "origin", "main")
		}
		c.waitState("/a/foo", "PENDING")
		if got := u.Refs()["refs/heads/silo/a/foo.git/main"]; got != first {
			t.Errorf("the uber repo moved while it was down: %s", got)
		}
		u.SetDown(false)
		c.waitRef(u, "refs/heads/silo/a/foo.git/main", last)
		c.waitState("/a/foo", "IN_SYNC")
	})
}

func TestRestoreDoesNotRollTheUberRepoBack(t *testing.T) {
	eachGit(t, func(t *testing.T, c gitCase) {
		u := c.mirrorTo()
		a := c.mk("", "a", nil)
		p := c.Probe(t, a, "pa")
		c.OK(a, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
		url := c.g.GitURL(t, p) + "a/foo.git"
		w := c.work(url)
		before := c.commit(w, "README", "one\n")
		c.eventually(w, "push", "-q", "origin", "main")
		c.waitRef(u, "refs/heads/silo/a/foo.git/main", before)

		restore := c.g.Backup(t)
		after := c.commit(w, "more", "two\n")
		c.git(w, "push", "-q", "origin", "main")
		c.waitRef(u, "refs/heads/silo/a/foo.git/main", after)

		restore()
		st := c.OK(c.Root(), "GET", "/silo/v1/mirror", nil)
		if st["paused"] == nil || st["paused"] == "" {
			t.Errorf("the restored silod's mirror isn't paused: %v", st)
		}
		// The restored repo is at the backup's tip, and a push to it
		// that the uber repo never had doesn't overwrite what it has.
		cl := c.clone(url)
		if got := c.git(cl, "rev-parse", "HEAD"); got != before {
			t.Fatalf("the restored repo's main is %s, want %s", got, before)
		}
		local := c.commit(cl, "other", "after the restore\n")
		c.git(cl, "push", "-q", "origin", "main")
		c.OK(c.Root(), "POST", "/silo/v1/reconcile:apply", obj{})
		r := c.waitState("/a/foo", "DIVERGED")
		if got := u.Refs()["refs/heads/silo/a/foo.git/main"]; got != after {
			t.Errorf("the uber repo's main is %s after the restore, want %s (local %s, backup %s)", got, after, local, before)
		}
		div, _ := r["diverged"].([]any)
		if len(div) != 1 || div[0].(obj)["actual"] != after || div[0].(obj)["expected"] != before {
			t.Errorf("the diverged ref: %v, want expected %s and actual %s", r, before, after)
		}
	})
}
