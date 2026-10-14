package gitproxy

// The proxy end to end, with real git clients: a StaticPolicy, a session
// token, and a LocalForwarder upstream (git http-backend over bare
// repositories on disk) standing in for GitHub. Every refusal is checked
// twice: what git printed, and that the upstream never moved.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gitFixture is one session's proxy in front of a local upstream holding
// github.com/acme/widgets (main, one commit) and github.com/acme/other.
type gitFixture struct {
	t        *testing.T
	upstream string // the upstream's root: <root>/<owner>/<name>.git
	local    *LocalForwarder
	token    *SessionToken
	audit    *RecordingAuditLog
	requests atomic.Int64 // HTTP requests the proxy was sent
	url      string       // the proxy's base URL
	home     string       // HOME for every git command, so no user config applies
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

func newGitFixture(t *testing.T, force bool) *gitFixture {
	t.Helper()
	requireGit(t)
	f := &gitFixture{t: t, upstream: t.TempDir(), home: t.TempDir(), audit: &RecordingAuditLog{}}
	f.local = &LocalForwarder{Root: f.upstream}
	for _, name := range []string{"widgets", "other"} {
		bare := filepath.Join(f.upstream, "acme", name+".git")
		f.git("", "init", "-q", "--bare", "-b", "main", bare)
		seed := filepath.Join(t.TempDir(), name)
		f.git("", "init", "-q", "-b", "main", seed)
		f.git(seed, "commit", "-q", "--allow-empty", "-m", "Initial "+name)
		f.git(seed, "push", "-q", bare, "main")
	}

	policy, err := NewStaticPolicy(StaticConfig{
		Repos: []StaticRepo{{
			Repo: "github.com/acme/widgets", Access: AccessPush, Force: force,
			Push: RefRules{{Pattern: "refs/heads/silo/*"}, {Pattern: "refs/tags/release-*"}, {Pattern: "refs/heads/scratch", Delete: true}},
		}},
		Credentials: map[string]*CredentialSource{"github.com": staticToken("gh", "upstream-secret")},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.token, err = NewSessionToken(Caller{ID: "session-1", Audit: map[string]string{"session": "session-1"}})
	if err != nil {
		t.Fatal(err)
	}
	proxy := &GitProxy{
		Policy:       policy,
		Tokens:       f.token,
		Audit:        f.audit,
		ForwarderFor: func(string) (Forwarder, error) { return f.local, nil },
	}
	handler := NewHandler(proxy)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// remote is repo's URL through the proxy, carrying token.
func (f *gitFixture) remote(repo, token string) string {
	u, _ := url.Parse(f.url + "/" + repo + ".git")
	u.User = url.UserPassword("x", token)
	return u.String()
}

// run runs git in dir ("" for none) and returns its combined output.
func (f *gitFixture) run(dir string, args ...string) (string, error) {
	f.t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	args = append([]string{"-c", "user.name=agent", "-c", "user.email=agent@example.invalid", "-c", "protocol.version=0"}, args...)
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "HOME="+f.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// git is run, failing the test on an error.
func (f *gitFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.run(dir, args...)
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// upstreamRef is ref's commit in the upstream's acme/widgets, or "".
func (f *gitFixture) upstreamRef(ref string) string {
	f.t.Helper()
	out, err := f.run(filepath.Join(f.upstream, "acme", "widgets.git"), "rev-parse", "--verify", "-q", ref)
	if err != nil {
		return ""
	}
	return out
}

// clone clones acme/widgets through the proxy.
func (f *gitFixture) clone() string {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "widgets")
	f.git("", "clone", "-q", f.remote("github.com/acme/widgets", f.token.Token()), dir)
	return dir
}

// receivePacksForwarded is how many pushes reached the upstream.
func (f *gitFixture) receivePacksForwarded() int {
	n := 0
	for _, c := range f.local.Calls() {
		if c.Method == "POST" && strings.HasSuffix(c.Path, "/git-receive-pack") {
			n++
		}
	}
	return n
}

// checkAudit checks that every request the proxy was sent left exactly
// one audit entry.
func (f *gitFixture) checkAudit() {
	f.t.Helper()
	if got, want := len(f.audit.Entries()), int(f.requests.Load()); got != want {
		f.t.Errorf("%d audit entries for %d requests", got, want)
	}
}

func TestGitCloneOfAnAllowedRepo(t *testing.T) {
	f := newGitFixture(t, false)
	dir := f.clone()
	if got := f.git(dir, "log", "--format=%s", "-1"); got != "Initial widgets" {
		t.Errorf("cloned HEAD is %q", got)
	}
	// The upstream saw the policy's credential, never the session token.
	calls := f.local.Calls()
	if len(calls) == 0 {
		t.Fatal("nothing was forwarded")
	}
	for _, c := range calls {
		if c.Credential == nil || c.Credential.Name != "gh" || c.Credential.Token != "upstream-secret" {
			t.Errorf("%s %s went upstream with %+v", c.Method, c.Path, c.Credential)
		}
		if strings.Contains(c.Headers["Authorization"], f.token.Token()) {
			t.Errorf("the session's own Authorization header reached the forwarder")
		}
	}
	var forwarded int
	for _, e := range f.audit.Entries() {
		if strings.HasPrefix(e.Outcome, "forwarded: 200") {
			forwarded++
			if e.Repo != "github.com/acme/widgets" || e.Credential != "gh" || e.Caller["session"] != "session-1" {
				t.Errorf("entry = %+v", e)
			}
		}
	}
	if forwarded < 2 {
		t.Errorf("%d forwarded entries, want info/refs and git-upload-pack at least", forwarded)
	}
	f.checkAudit()
}

func TestGitCloneOfAnUnlistedRepoIsRefused(t *testing.T) {
	f := newGitFixture(t, false)
	out, err := f.run("", "clone", "-q", f.remote("github.com/acme/other", f.token.Token()), filepath.Join(t.TempDir(), "other"))
	if err == nil {
		t.Fatalf("an unlisted repo was cloned:\n%s", out)
	}
	if !strings.Contains(out, "github.com/acme/other is not one of this session's repositories") ||
		!strings.Contains(out, "Silofile") {
		t.Errorf("git printed:\n%s\nwant the refusal, naming the Silofile", out)
	}
	if calls := f.local.Calls(); len(calls) != 0 {
		t.Errorf("forwarded %+v", calls)
	}
	f.checkAudit()
}

func TestGitWrongTokenIsRefused(t *testing.T) {
	f := newGitFixture(t, false)
	out, err := f.run("", "clone", "-q", f.remote("github.com/acme/widgets", "not-the-token"), filepath.Join(t.TempDir(), "w"))
	if err == nil {
		t.Fatalf("a wrong token cloned:\n%s", out)
	}
	if !strings.Contains(out, "Authentication failed") && !strings.Contains(out, "401") {
		t.Errorf("git printed:\n%s", out)
	}
	if calls := f.local.Calls(); len(calls) != 0 {
		t.Errorf("forwarded %+v", calls)
	}
	for _, e := range f.audit.Entries() {
		if e.Status != 401 || e.Caller != nil {
			t.Errorf("entry = %+v, want a 401 naming no caller", e)
		}
	}
	f.checkAudit()
}

func TestGitPushToAnAllowedRef(t *testing.T) {
	f := newGitFixture(t, false)
	dir := f.clone()
	f.git(dir, "commit", "-q", "--allow-empty", "-m", "Agent work")
	head := f.git(dir, "rev-parse", "HEAD")
	f.git(dir, "push", "-q", "origin", "HEAD:refs/heads/silo/work")
	if got := f.upstreamRef("refs/heads/silo/work"); got != head {
		t.Fatalf("upstream silo/work = %q, want %s", got, head)
	}
	// A fast-forward of it, shown by the commits the push carries.
	f.git(dir, "commit", "-q", "--allow-empty", "-m", "More work")
	head = f.git(dir, "rev-parse", "HEAD")
	f.git(dir, "push", "-q", "origin", "HEAD:refs/heads/silo/work")
	if got := f.upstreamRef("refs/heads/silo/work"); got != head {
		t.Fatalf("upstream silo/work = %q after a fast-forward, want %s", got, head)
	}
	// A fast-forward the push carries no commits for: main's own commit,
	// which the upstream already has, onto a branch that is behind it.
	f.git(dir, "push", "-q", "origin", "HEAD~2:refs/heads/silo/behind")
	f.git(dir, "push", "-q", "origin", "HEAD:refs/heads/silo/behind")
	if got := f.upstreamRef("refs/heads/silo/behind"); got != head {
		t.Fatalf("upstream silo/behind = %q, want %s", got, head)
	}

	var pushes []AuditEntry
	for _, e := range f.audit.Entries() {
		if e.Action == "git-receive-pack" {
			pushes = append(pushes, e)
		}
	}
	if len(pushes) != 4 {
		t.Fatalf("%d receive-pack entries, want 4", len(pushes))
	}
	if r := pushes[0].Refs; len(r) != 1 || r[0].Ref != "refs/heads/silo/work" || r[0].Kind != "create" ||
		r[0].Caller["pattern"] != "refs/heads/silo/*" {
		t.Errorf("first push's refs = %+v", r)
	}
	if r := pushes[1].Refs; len(r) != 1 || r[0].Kind != "update" {
		t.Errorf("second push's refs = %+v", r)
	}
	if len(pushes[3].Checks) == 0 || !strings.Contains(pushes[3].Checks[0], "fast-forward check") {
		t.Errorf("a fast-forward shown by the upstream's commits recorded no check: %+v", pushes[3])
	}
	f.checkAudit()
}

// refusedPush pushes refspec from dir and checks it was refused before
// anything was forwarded, with git printing want.
func (f *gitFixture) refusedPush(dir, refspec, want string, extra ...string) {
	f.t.Helper()
	before := f.receivePacksForwarded()
	out, err := f.run(dir, append(append([]string{"push"}, extra...), "origin", refspec)...)
	if err == nil {
		f.t.Fatalf("push %s was accepted:\n%s", refspec, out)
	}
	if !strings.Contains(out, want) {
		f.t.Errorf("push %s: git printed\n%s\nwant %q", refspec, out, want)
	}
	if n := f.receivePacksForwarded(); n != before {
		f.t.Errorf("push %s reached the upstream", refspec)
	}
}

func TestGitPushToAnUnmatchedRefIsRefusedBeforeForwarding(t *testing.T) {
	f := newGitFixture(t, false)
	dir := f.clone()
	main := f.upstreamRef("refs/heads/main")
	f.git(dir, "commit", "-q", "--allow-empty", "-m", "Straight to main")
	f.refusedPush(dir, "HEAD:refs/heads/main", "refs/heads/main matches no push pattern")
	f.refusedPush(dir, "HEAD:refs/heads/feature", "refs/heads/feature matches no push pattern")
	// One refused update refuses the whole push.
	before := f.receivePacksForwarded()
	if out, err := f.run(dir, "push", "--atomic", "origin", "HEAD:refs/heads/silo/ok", "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("a push with one refused ref was accepted:\n%s", out)
	}
	if f.receivePacksForwarded() != before || f.upstreamRef("refs/heads/silo/ok") != "" {
		t.Error("part of a refused push reached the upstream")
	}
	if got := f.upstreamRef("refs/heads/main"); got != main {
		t.Errorf("upstream main moved from %s to %s", main, got)
	}
	f.checkAudit()
}

func TestGitTagAndDeleteAreRefusedWithoutAPattern(t *testing.T) {
	f := newGitFixture(t, false)
	dir := f.clone()
	f.git(dir, "tag", "v1")
	f.refusedPush(dir, "refs/tags/v1", "refs/tags/v1 matches no push pattern")
	if f.upstreamRef("refs/tags/v1") != "" {
		t.Error("the tag reached the upstream")
	}
	// A tag a pattern names may be created, but not moved without force.
	f.git(dir, "tag", "release-1")
	f.git(dir, "push", "-q", "origin", "refs/tags/release-1")
	f.git(dir, "commit", "-q", "--allow-empty", "-m", "Next")
	f.git(dir, "tag", "-f", "release-1")
	f.refusedPush(dir, "refs/tags/release-1", "moving a tag is a force-push", "--force")

	f.git(dir, "push", "-q", "origin", "HEAD:refs/heads/silo/gone")
	f.refusedPush(dir, ":refs/heads/silo/gone", "may be pushed but not deleted")
	f.refusedPush(dir, ":refs/heads/main", "refs/heads/main matches no push pattern")
	if f.upstreamRef("refs/heads/silo/gone") == "" || f.upstreamRef("refs/heads/main") == "" {
		t.Error("a refused delete reached the upstream")
	}
	// A pattern that allows a delete allows it.
	f.git(dir, "push", "-q", "origin", "HEAD:refs/heads/scratch")
	f.git(dir, "push", "-q", "origin", ":refs/heads/scratch")
	if f.upstreamRef("refs/heads/scratch") != "" {
		t.Error("an allowed delete did not happen")
	}
	f.checkAudit()
}

func TestGitNonFastForwardNeedsForce(t *testing.T) {
	for _, force := range []bool{false, true} {
		f := newGitFixture(t, force)
		dir := f.clone()
		f.git(dir, "commit", "-q", "--allow-empty", "-m", "First")
		f.git(dir, "push", "-q", "origin", "HEAD:refs/heads/silo/x")
		first := f.git(dir, "rev-parse", "HEAD")
		// A rewrite: a sibling of First, not a descendant.
		f.git(dir, "reset", "-q", "--hard", "HEAD~1")
		f.git(dir, "commit", "-q", "--allow-empty", "-m", "Rewritten")
		rewritten := f.git(dir, "rev-parse", "HEAD")
		if !force {
			f.refusedPush(dir, "HEAD:refs/heads/silo/x", "could not show", "--force")
			if got := f.upstreamRef("refs/heads/silo/x"); got != first {
				t.Errorf("force false: upstream silo/x = %s, want it left at %s", got, first)
			}
		} else {
			f.git(dir, "push", "-q", "--force", "origin", "HEAD:refs/heads/silo/x")
			if got := f.upstreamRef("refs/heads/silo/x"); got != rewritten {
				t.Errorf("force true: upstream silo/x = %s, want %s", got, rewritten)
			}
			e := f.audit.Entries()
			if r := e[len(e)-1].Refs; len(r) != 1 || r[0].Kind != "force" {
				t.Errorf("force true: refs = %+v", r)
			}
		}
		f.checkAudit()
	}
}

// A body whose command list is truncated or garbled is refused, never
// forwarded on the guess that it is harmless.
func TestGitTruncatedOrGarbledCommandListFailsClosed(t *testing.T) {
	f := newGitFixture(t, false)
	good := receivePackBody(zero, oidB, "refs/heads/silo/x", packOf())
	for name, body := range map[string][]byte{
		"truncated":        good[:20],
		"no flush":         good[:bytes.Index(good, []byte("0000"))],
		"garbled length":   append([]byte("zz"), good[2:]...),
		"garbled command":  receivePackBody("not-an-object-id", oidB, "refs/heads/silo/x", nil),
		"gzip that is not": good,
		"empty":            {},
	} {
		req, err := http.NewRequest("POST", f.url+"/github.com/acme/widgets.git/git-receive-pack", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range postHeaders("git-receive-pack", f.token.Token()) {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		if name == "gzip that is not" {
			req.Header.Set("Content-Encoding", "gzip")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("%s: status %d, want 403", name, resp.StatusCode)
		}
	}
	if n := f.receivePacksForwarded(); n != 0 {
		t.Errorf("%d unreadable pushes reached the upstream", n)
	}
	for _, e := range f.audit.Entries() {
		if !strings.HasPrefix(e.Outcome, "denied: unreadable push") {
			t.Errorf("entry = %+v", e)
		}
	}
	f.checkAudit()
}

// Serve is the proxy a session runs: on loopback, until Close, with the
// audit log written as JSON lines.
func TestServeRunsTheProxyUntilClose(t *testing.T) {
	requireGit(t)
	upstream := t.TempDir()
	f := &gitFixture{t: t, home: t.TempDir()}
	bare := filepath.Join(upstream, "acme", "widgets.git")
	f.git("", "init", "-q", "--bare", "-b", "main", bare)
	policy := testPolicy(t, StaticRepo{Repo: "github.com/acme/widgets", Access: AccessRead})
	token, err := NewSessionToken(Caller{ID: "s", Audit: map[string]string{"session": "s"}})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	local := &LocalForwarder{Root: upstream}
	srv, err := Serve(&GitProxy{Policy: policy, Tokens: token, Audit: &JSONAuditLog{W: &log},
		ForwarderFor: func(string) (Forwarder, error) { return local, nil }}, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.url = srv.URL
	if out, err := f.run("", "ls-remote", f.remote("github.com/acme/widgets", token.Token())); err != nil {
		t.Fatalf("ls-remote: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var lines int
	for _, line := range bytes.Split(bytes.TrimSpace(log.Bytes()), []byte("\n")) {
		var e map[string]any
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		lines++
	}
	if lines == 0 {
		t.Error("nothing was audited")
	}
	if _, err := http.Get(srv.URL); err == nil {
		t.Error("the server still answers after Close")
	}
}
