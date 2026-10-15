package session_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/gitproxy"
	"github.com/bwsalmon/silo/v2/internal/githubsim"
	"github.com/bwsalmon/silo/v2/sandbox/container"
	"github.com/bwsalmon/silo/v2/session"
)

// gitImage is a small image with git and a shell.
const gitImage = "alpine/git:2.49.1"

// upstreamToken is the credential the git proxy holds: the simulated
// GitHub's token for acme/widgets, read and write.
const upstreamToken = "ghp_upstreamSecretToken0123456789"

// readOnlyToken may fetch acme/widgets but not push to it.
const readOnlyToken = "ghp_readOnlyToken0123456789"

// gitUpstream is a simulated github.com (internal/githubsim) over
// HTTPS, serving github.com/acme/widgets from a bare repository with one
// commit on main, and the forwarder that points the git proxy at it.
type gitUpstream struct {
	bare     string
	pushes   atomic.Int64 // git-receive-pack requests that reached it
	srv      *httptest.Server
	forwards func(host string) (gitproxy.Forwarder, error)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("SILO_V2_REQUIRE_GIT") != "" {
			t.Fatal("git is not installed, and SILO_V2_REQUIRE_GIT is set")
		}
		t.Skip("git is not installed")
	}
}

func gitCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=seed", "-c", "user.email=seed@example.invalid"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitUpstream(t *testing.T) *gitUpstream {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	u := &gitUpstream{bare: filepath.Join(root, "acme", "widgets.git")}
	gitCmd(t, "init", "-q", "--bare", "-b", "main", u.bare)
	seed := t.TempDir()
	gitCmd(t, "init", "-q", "-b", "main", seed)
	gitCmd(t, "-C", seed, "commit", "-q", "--allow-empty", "-m", "Initial widgets")
	gitCmd(t, "-C", seed, "push", "-q", u.bare, "main")
	backend, err := githubsim.GitBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	sim := &githubsim.GitUpstream{
		Tokens: map[string]githubsim.GitAccess{
			upstreamToken: {Read: []string{"acme/widgets"}, Write: []string{"acme/widgets"}},
			readOnlyToken: {Read: []string{"acme/widgets"}},
		},
		Backend: backend,
	}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			u.pushes.Add(1)
		}
		sim.ServeHTTP(w, r)
	}))
	t.Cleanup(u.srv.Close)
	u.forwards = func(host string) (gitproxy.Forwarder, error) {
		if host != "github.com" {
			t.Errorf("the proxy forwarded to %s", host)
		}
		return &gitproxy.RealForwarder{Host: u.srv.Listener.Addr().String(), UseTLS: true, Client: u.srv.Client()}, nil
	}
	return u
}

// ref is a ref of the upstream repository, or "" when it has none.
func (u *gitUpstream) ref(t *testing.T, name string) string {
	out, err := exec.Command("git", "--git-dir", u.bare, "rev-parse", "--verify", "-q", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitSilofile is a headless silo whose sandbox checks out widgets,
// which may be pushed to silo/* only, with a {file:} credential
// delivered {proxy: git}.
const gitSilofile = headless + `  - name: github
    from: {file: github-token}
    to: [{proxy: git}]
git:
  - name: widgets
    repo: github.com/acme/widgets
    access: push
    push: [refs/heads/silo/*]
    credential: github
`

func writeToken(t *testing.T, e *env, token string) {
	t.Helper()
	dir := filepath.Join(e.root, "silo")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "github-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sandboxScript is what the fake agent runs in the sandbox: look at the
// checkout, push a branch it may push, push main, which it may not, and
// show everything a credential could have leaked into.
const sandboxScript = `cd /work || exit 9
echo "head: $(git log -1 --format=%s)"
git checkout -q -b silo/feature
echo change > file.txt && git add file.txt && git commit -q -m 'Agent change'
git push -q origin silo/feature 2>&1; echo "push-silo=$?"
git push origin HEAD:refs/heads/main 2>&1; echo "push-main=$?"
echo "--- config"; cat .git/config
echo "--- env"; env
echo "--- ps"; ps -ef 2>/dev/null || ps
`

// TestGitProxyEndToEnd is stage 7b's deliverable: a container sandbox
// has its repo cloned through the session's git proxy, pushes a branch
// its Silofile allows, and is refused main before the push reaches the
// (simulated) GitHub; the credential is nowhere in the sandbox or the
// audit log, and the proxy and its network go with the session.
func TestGitProxyEndToEnd(t *testing.T) {
	b, err := container.New()
	if err == nil {
		err = exec.Command(b.Runtime, "info").Run()
	}
	if err != nil {
		if os.Getenv("SILO_V2_REQUIRE_DOCKER") != "" {
			t.Fatalf("SILO_V2_REQUIRE_DOCKER is set, but there is no container runtime: %v", err)
		}
		t.Skipf("no container runtime: %v", err)
	}
	if out, err := exec.Command(b.Runtime, "pull", "-q", gitImage).CombinedOutput(); err != nil {
		t.Fatalf("pulling %s: %v\n%s", gitImage, err, out)
	}
	up := newGitUpstream(t)
	e := newEnv(t)
	writeToken(t, e, upstreamToken)
	e.fake(fakeConfig{Call: "sandbox_exec", Args: map[string]any{"sandbox": "dev", "command": sandboxScript}})
	f := e.silofile(gitSilofile + `sandboxes:
  - name: dev
    kind: container
    image: ` + gitImage + `
    workdir: /work
    repos: [widgets]
    env:
      GIT_AUTHOR_NAME: Silo Agent
      GIT_AUTHOR_EMAIL: agent@example.invalid
      GIT_COMMITTER_NAME: Silo Agent
      GIT_COMMITTER_EMAIL: agent@example.invalid
`)
	mainBefore := up.ref(t, "refs/heads/main")
	res := e.run(f, func(o *session.Options) { o.Backend = b; o.GitForwarderFor = up.forwards })
	if res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	r := e.report()
	out := r.Result
	t.Logf("in the sandbox:\n%s", out)
	if r.Error != "" || !strings.Contains(out, "head: Initial widgets") {
		t.Fatalf("the checkout is not there: %q (error %q)\n%s", out, r.Error, res.stderr)
	}
	if !strings.Contains(out, "push-silo=0") {
		t.Errorf("the push to silo/feature failed:\n%s", out)
	}
	if strings.Contains(out, "push-main=0") || !strings.Contains(out, "matches no push pattern") {
		t.Errorf("the push to main was not refused by the proxy:\n%s", out)
	}
	if strings.Contains(out, upstreamToken) {
		t.Errorf("the credential reached the sandbox:\n%s", out)
	}
	if !strings.Contains(out, "url = http://silo:") {
		t.Errorf("origin is not the proxy URL with the sandbox's token:\n%s", out)
	}

	// Upstream: silo/feature arrived, by the author the sandbox's env
	// named; main did not move; the refused push never reached it.
	if up.ref(t, "refs/heads/silo/feature") == "" {
		t.Error("silo/feature did not reach the upstream")
	} else if a := gitCmd(t, "--git-dir", up.bare, "log", "-1", "--format=%an <%ae>", "silo/feature"); a != "Silo Agent <agent@example.invalid>" {
		t.Errorf("silo/feature's author is %q", a)
	}
	if got := up.ref(t, "refs/heads/main"); got != mainBefore {
		t.Errorf("main moved from %s to %s", mainBefore, got)
	}
	if n := up.pushes.Load(); n != 1 {
		t.Errorf("%d pushes reached the upstream; want only silo/feature's", n)
	}

	rec := e.sessionGone()
	var network string
	for _, res := range rec.Resources {
		if res.Kind == session.KindNetwork {
			network = res.Network
		}
	}
	if network == "" {
		t.Errorf("session.json records no network: %+v", rec.Resources)
	} else if err := exec.Command(b.Runtime, "network", "inspect", network).Run(); err == nil {
		t.Errorf("the session's network %s is left after teardown", network)
	}
	audit, _ := os.ReadFile(filepath.Join(rec.StateDir, "audit.jsonl"))
	if strings.Contains(string(audit), upstreamToken) {
		t.Error("the audit log holds the credential")
	}
	for _, want := range []string{`"event":"git"`, `"sandbox":"dev"`, `"credential":"github"`, `"git-receive-pack"`, "matches no push pattern"} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("the audit log has no %s:\n%s", want, audit)
		}
	}
}

// TestGitDryRun checks silo run --dry-run's git lines: the credential
// resolves, and read and push access are asked of the host without
// changing anything.
func TestGitDryRun(t *testing.T) {
	up := newGitUpstream(t)
	defer session.SetGitCheckForwarder(up.forwards)()
	e := newEnv(t)
	writeToken(t, e, upstreamToken)
	f := e.silofile(gitSilofile)
	check := func() session.CheckResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, r := range session.DryRun(ctx, f, e.environ) {
			if r.Name == "git widgets" {
				return r
			}
		}
		t.Fatal("no git widgets check")
		return session.CheckResult{}
	}

	if r := check(); !r.OK || !strings.Contains(r.Detail, "push access with credential github") {
		t.Errorf("with a token that may push: %+v", r)
	}
	writeToken(t, e, readOnlyToken)
	if r := check(); r.OK || !strings.Contains(r.Detail, "push access with credential github refused: 403") {
		t.Errorf("with a read-only token: %+v", r)
	}
	os.Remove(filepath.Join(e.root, "silo", "github-token"))
	if r := check(); r.OK || !strings.Contains(r.Detail, "github-token") {
		t.Errorf("with no token file: %+v", r)
	}
	if n := up.pushes.Load(); n != 0 {
		t.Errorf("the dry run pushed %d times", n)
	}
}
