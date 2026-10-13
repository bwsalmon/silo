package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/gitserve"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
)

// TestMain runs the test binary as the git service's pre-receive hook
// when receive-pack runs it so, as silod's main does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == gitserve.HookArg {
		os.Exit(gitserve.HookMain(os.Args[2:], os.Stdin, os.Stderr))
	}
	os.Exit(m.Run())
}

// gitEnv is newEnv with repos and the git service: the holder's
// listener on port 80 is a plain one, which a real git client reaches
// through a forwarder per source address (gitURL).
type gitEnv struct {
	*env
	silos string // the data directory's silos/
	// disk is what the service's Disk answers for every silo: used and
	// limit, in bytes; a negative limit is none.
	used, limit atomic.Int64
}

func newGitEnv(t *testing.T) *gitEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
	g := &gitEnv{silos: t.TempDir()}
	g.limit.Store(-1)
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := gitserve.WriteHooks(hooks); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	host := &gitstore.Host{Storage: &storage.Dir{Root: g.silos}, HooksDir: hooks}
	svc := gitserve.New(gitserve.Options{Host: host, Hook: exe, MaxBlob: 64 << 10,
		Disk: func(context.Context, string) (int64, int64, error) { return g.used.Load(), g.limit.Load(), nil }})
	g.env = newEnv(t, envOptions{repos: host, git: svc})
	svc.SetAPI(g.fence)
	g.tx(func(tx store.Tx) error {
		for _, s := range []string{g.a, g.kid, g.ro, g.sealed} {
			if err := ledger.Grant(ctx, tx, s, []ledger.Amount{{Key: fenceapi.ReposClass, Amount: ledger.Units(4 - btoi(s == g.kid))}}); err != nil {
				return err
			}
		}
		return nil
	})
	return g
}

func btoi(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// gitURL is the base URL of port 80's /git/ as seen from ip: a local
// listener that forwards each connection to port 80 after the line
// that says where it comes from.
func (g *gitEnv) gitURL(ip netip.Addr) string {
	g.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := dialFrom(ctx, g.web80, ip)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { io.Copy(up, c); up.(*net.TCPConn).CloseWrite() }()
				io.Copy(c, up)
			}()
		}
	}()
	return "http://" + l.Addr().String() + "/git/"
}

// git runs git in dir, failing the test if it fails, and returns its
// output.
func (g *gitEnv) git(dir string, args ...string) string {
	g.t.Helper()
	out, err := g.tryGit(dir, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (g *gitEnv) tryGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitstore.Env(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commit makes a commit in the work tree dir writing content to file.
func (g *gitEnv) commit(dir, file, content string) string {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
	g.git(dir, "add", file)
	g.git(dir, "commit", "-q", "-m", "write "+file)
	return strings.TrimSpace(g.git(dir, "rev-parse", "HEAD"))
}

// work is a fresh work tree on main with url as origin.
func (g *gitEnv) work(url string) string {
	dir := g.t.TempDir()
	g.git(dir, "init", "-q", "-b", "main")
	g.git(dir, "remote", "add", "origin", url)
	return dir
}

// pushRefs is the refs of each repo.push audited under silo, oldest
// first.
func (g *gitEnv) pushes(silo string) []obj {
	g.t.Helper()
	var out []obj
	g.tx(func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT fields FROM audit WHERE silo = ? AND kind = ? ORDER BY at, n`, silo, fenceapi.KindRepoPush)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f string
			if err := rows.Scan(&f); err != nil {
				return err
			}
			o := obj{}
			json.Unmarshal([]byte(f), &o)
			out = append(out, o)
		}
		return rows.Err()
	})
	return out
}

// TestGitService clones and pushes with real git through port 80
// (local-git.md §3 and §4): the silo itself reads and writes, by path
// and by id; an ancestor reads and can't push; a sibling, and anyone
// for a repo that doesn't exist, gets 404; a sealed silo keeps
// pushing. Each push is audited with its refs and marks the repo
// dirty.
func TestGitService(t *testing.T) {
	g := newGitEnv(t)
	g.ok(g.a, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
	g.ok(g.root, "POST", "/silo/v1/silos/a/kid/repos", obj{"name": "bar"})
	g.ok(g.root, "POST", "/silo/v1/silos/sealed/repos", obj{"name": "ci"})
	aURL, kidURL, roURL, sealedURL := g.gitURL(g.ip[g.a]), g.gitURL(g.ip[g.kid]), g.gitURL(g.ip[g.ro]), g.gitURL(g.ip[g.sealed])

	// a pushes to its own repo, and clones it back by id.
	w := g.work(aURL + "a/foo.git")
	c1 := g.commit(w, "README", "hello\n")
	g.git(w, "push", "-q", "origin", "main")
	clone := filepath.Join(t.TempDir(), "c")
	g.git("", "clone", "-q", aURL+"-"+g.a+"/foo.git", clone)
	if got := strings.TrimSpace(g.git(clone, "rev-parse", "HEAD")); got != c1 {
		t.Fatalf("clone by id is at %s, want %s", got, c1)
	}
	ps := g.pushes(g.a)
	if len(ps) != 1 {
		t.Fatalf("repo.push audits: %v", ps)
	}
	refs := ps[0]["refs"].([]any)
	if len(refs) != 1 || refs[0].(obj)["ref"] != "refs/heads/main" || refs[0].(obj)["new"] != c1 || refs[0].(obj)["old"] != nil {
		t.Errorf("repo.push refs: %v", refs)
	}
	repo := g.ok(g.a, "GET", "/silo/v1/self/repos/foo", nil)
	if repo["size_bytes"].(float64) <= 0 {
		t.Errorf("size after a push: %v", repo["size_bytes"])
	}
	var dirty int
	g.tx(func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM changes WHERE kind = 'repo.push' AND key = ?`, repo["id"]).Scan(&dirty)
	})
	if dirty != 1 {
		t.Errorf("the push marked the repo dirty %d times, want once", dirty)
	}

	// Protocol v2 and a partial clone.
	v2 := filepath.Join(t.TempDir(), "v2")
	g.git("", "-c", "protocol.version=2", "clone", "-q", "--filter=blob:none", aURL+"a/foo.git", v2)

	// The ancestor reads its child's repo and can't push to it.
	kw := g.work(kidURL + "a/kid/bar.git")
	g.commit(kw, "x", "kid\n")
	g.git(kw, "push", "-q", "origin", "main")
	anc := filepath.Join(t.TempDir(), "anc")
	g.git("", "clone", "-q", aURL+"a/kid/bar.git", anc)
	g.commit(anc, "y", "parent\n")
	if out, err := g.tryGit(anc, "push", "origin", "main"); err == nil || !strings.Contains(out, "403") {
		t.Errorf("an ancestor pushed to its child's repo: %v\n%s", err, out)
	}

	// A sibling, and a repo that doesn't exist, are 404 alike.
	for _, u := range []string{roURL + "a/foo.git", roURL + "-" + g.a + "/foo.git", aURL + "a/nope.git", aURL + "nosuch/foo.git"} {
		out, err := g.tryGit("", "ls-remote", u)
		if err == nil || !strings.Contains(out, "not found") {
			t.Errorf("ls-remote %s: %v\n%s", u, err, out)
		}
	}
	sw := g.work(roURL + "a/foo.git")
	g.commit(sw, "z", "sibling\n")
	if out, err := g.tryGit(sw, "push", "origin", "main"); err == nil || !strings.Contains(out, "not found") {
		t.Errorf("a sibling's push: %v\n%s", err, out)
	}

	// A sealed silo keeps pushing to its repos.
	sl := g.work(sealedURL + "sealed/ci.git")
	g.commit(sl, "build", "ok\n")
	g.git(sl, "push", "-q", "origin", "main")

	// A request with git's Authorization is the same request.
	req, _ := http.NewRequest("GET", aURL+"a/foo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Basic cm9vdDpyb290")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.HasPrefix(body, []byte("001e# service=git-upload-pack\n0000")) {
		t.Errorf("info/refs: %d %q", resp.StatusCode, body)
	}
}

// TestGitPreReceive: the hook keeps pushes to refs/heads/ and
// refs/tags/, refuses a blob over the limit and a push to a silo over
// its disk grant, and archives tips that a force-push or deletion
// would orphan, in refs that clones don't see and pushes can't touch;
// silo repo archives lists them and restore-ref brings one back.
func TestGitPreReceive(t *testing.T) {
	g := newGitEnv(t)
	g.ok(g.a, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
	url := g.gitURL(g.ip[g.a]) + "a/foo.git"
	w := g.work(url)
	base := g.commit(w, "README", "one\n")
	g.git(w, "push", "-q", "origin", "main")

	refuse := func(what, want string, args ...string) {
		t.Helper()
		out, err := g.tryGit(w, args...)
		if err == nil || !strings.Contains(out, want) {
			t.Errorf("%s: %v, want a refusal with %q:\n%s", what, err, want, out)
		}
	}
	refuse("a ref outside heads and tags", "only refs/heads/ and refs/tags/", "push", "origin", "HEAD:refs/notes/x")
	refuse("an archive ref", "", "push", "origin", "HEAD:refs/silo-archive/x/heads/main")

	big := filepath.Join(w, "big")
	os.WriteFile(big, bytes.Repeat([]byte("0123456789abcdef"), 8<<10), 0o644)
	g.git(w, "add", "big")
	g.git(w, "commit", "-q", "-m", "big")
	refuse("a big blob", "big (", "push", "origin", "main")
	g.git(w, "reset", "-q", "--hard", base)

	g.git(w, "push", "-q", "origin", "main:side")
	g.used.Store(13 << 30)
	g.limit.Store(12 << 30)
	g.commit(w, "more", "two\n")
	refuse("a push over the disk grant", "silo: /a is over its disk_gb grant (13 of 12 GiB)", "push", "origin", "main")
	// A push that only deletes is let through, and side's tip is main's,
	// so it needs no archive.
	g.git(w, "push", "-q", "origin", "--delete", "side")
	g.limit.Store(-1)
	g.used.Store(0)

	// A fast-forward archives nothing; a force-push archives the tip it
	// orphans; deleting a branch whose tip main reaches archives nothing.
	g.git(w, "push", "-q", "origin", "main")
	ff := strings.TrimSpace(g.git(w, "rev-parse", "HEAD"))
	g.git(w, "reset", "-q", "--hard", base)
	g.commit(w, "other", "rewritten\n")
	g.git(w, "push", "-q", "--force", "origin", "main")

	l := g.ok(g.a, "GET", "/silo/v1/self/repos/foo/archives", nil)
	as := l["archives"].([]any)
	if len(as) != 1 || as[0].(obj)["commit"] != ff || as[0].(obj)["ref"] != "refs/heads/main" {
		t.Fatalf("archives after one force-push: %v", l)
	}
	name := as[0].(obj)["name"].(string)
	if ls := g.git(w, "ls-remote", "origin"); strings.Contains(ls, "silo-archive") || strings.Contains(ls, ff) {
		t.Errorf("ls-remote shows the archive:\n%s", ls)
	}
	// A sibling can't list them; the parent can.
	if code, _ := g.as(g.ro, "GET", "/silo/v1/silos/a/repos/foo/archives", nil); code != 404 {
		t.Errorf("a sibling listing archives: %d", code)
	}
	g.ok(g.root, "GET", "/silo/v1/silos/a/repos/foo/archives", nil)

	r := g.ok(g.a, "POST", "/silo/v1/self/repos/foo:restoreRef", obj{"archive": name, "branch": "rescued"})
	if r["commit"] != ff {
		t.Errorf("restoreRef: %v", r)
	}
	if code, out := g.as(g.a, "POST", "/silo/v1/self/repos/foo:restoreRef", obj{"archive": name, "branch": "rescued"}); code != 409 {
		t.Errorf("restoring onto a branch that exists: %d %v", code, out)
	}
	if ls := g.git(w, "ls-remote", "origin", "rescued"); !strings.Contains(ls, ff) {
		t.Errorf("rescued is not at %s:\n%s", ff, ls)
	}
	// Deleting the rescued branch now orphans nothing it didn't before:
	// the archive still reaches its tip.
	g.git(w, "push", "-q", "origin", "--delete", "rescued")
	if l := g.ok(g.a, "GET", "/silo/v1/self/repos/foo/archives", nil); len(l["archives"].([]any)) != 1 {
		t.Errorf("archives after deleting the rescued branch: %v", l)
	}
}

// TestGitManageGrant: a manage grant with repos at read fetches and
// can't push, at write pushes, and when it ends the holder's git
// requests in flight are cut off.
func TestGitManageGrant(t *testing.T) {
	g := newGitEnv(t)
	g.ok(g.root, "POST", "/silo/v1/silos/ro/repos", obj{"name": "app"})
	roURL, aURL := g.gitURL(g.ip[g.ro]), g.gitURL(g.ip[g.a])
	w := g.work(roURL + "ro/app.git")
	g.commit(w, "main.go", "package main\n")
	g.git(w, "push", "-q", "origin", "main")

	grant := func(name, access string) {
		gr := g.ok(g.root, "POST", "/silo/v1/silos/ro/manageGrants", obj{"name": name, "holder": "/a",
			"scope": obj{"facade": obj{"resources": []string{"repos"}, "access": access}, "fence": []string{"read"}},
			"lease": obj{"ttl": "3600s"}})
		g.ok(g.a, "POST", "/silo/v1/self/manageGrants/"+gr["id"].(string)+":accept", obj{})
		g.sync()
	}
	if out, err := g.tryGit("", "ls-remote", aURL+"ro/app.git"); err == nil {
		t.Fatalf("a reads ro's repo with no grant:\n%s", out)
	}
	grant("pipeline", "read")
	h := filepath.Join(t.TempDir(), "h")
	g.git("", "clone", "-q", aURL+"ro/app.git", h)
	g.commit(h, "deploy", "x\n")
	if out, err := g.tryGit(h, "push", "origin", "main"); err == nil || !strings.Contains(out, "403") {
		t.Errorf("a read grant pushed: %v\n%s", err, out)
	}

	// A fetch in flight: its request body is never finished.
	conn, err := net.Dial("tcp", strings.TrimPrefix(strings.TrimSuffix(aURL, "/git/"), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /git/ro/app.git/git-upload-pack HTTP/1.1\r\nHost: silo.internal\r\n"+
		"Content-Type: application/x-git-upload-pack-request\r\nTransfer-Encoding: chunked\r\n\r\n"+
		"5\r\n0032w\r\n")
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(conn)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	g.ok(g.root, "DELETE", "/silo/v1/silos/ro/manageGrants/pipeline", nil)
	select {
	case <-done:
		if d := time.Since(start); d > time.Second {
			t.Errorf("the holder's fetch was cut off after %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the holder's fetch is still open after its grant ended")
	}
	if out, err := g.tryGit("", "ls-remote", aURL+"ro/app.git"); err == nil || !strings.Contains(out, "not found") {
		t.Errorf("after the grant ended: %v\n%s", err, out)
	}

	grant("deployer", "write")
	g.git(h, "push", "-q", "origin", "main")
	ps := g.pushes(g.ro)
	if len(ps) != 2 {
		t.Fatalf("repo.push audits under ro: %v", ps)
	}
}
