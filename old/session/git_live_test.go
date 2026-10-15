package session_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/old/sandbox/container"
	"github.com/bwsalmon/silo/old/session"
)

// Where grain's github-sandbox capability places a private, single-use
// repository's owner/name and a token scoped to it.
const (
	liveRepoFile  = "/home/debian/.github-sandbox-repo"
	liveTokenFile = "/home/debian/.github-sandbox-token"
)

// TestGitProxyLiveGitHub is TestGitProxyEndToEnd against real GitHub
// over HTTPS, opt-in with SILO_V2_LIVE_GITHUB=1 and not part of the
// gate: the sandbox clones the repository through the proxy, pushes a
// silo/* branch, and is refused main by the proxy, the token never
// leaving silo. It needs a repository it may push to, and a token for it
// in a file only its owner reads -- grain's github-sandbox capability
// places both.
func TestGitProxyLiveGitHub(t *testing.T) {
	if os.Getenv("SILO_V2_LIVE_GITHUB") != "1" {
		t.Skip("set SILO_V2_LIVE_GITHUB=1 to test against real GitHub")
	}
	requireGit(t)
	repoName, err := os.ReadFile(liveRepoFile)
	if err != nil {
		t.Fatalf("the live test needs %s (owner/name): %v", liveRepoFile, err)
	}
	token, err := os.ReadFile(liveTokenFile)
	if err != nil {
		t.Fatalf("the live test needs %s: %v", liveTokenFile, err)
	}
	b, err := container.New()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(b.Runtime, "pull", "-q", gitImage).CombinedOutput(); err != nil {
		t.Fatalf("pulling %s: %v\n%s", gitImage, err, out)
	}
	repo := "github.com/" + strings.TrimSpace(string(repoName))
	branch := "silo/live-" + session.NewID()
	e := newEnv(t)
	script := `cd /work || exit 9
git commit -q --allow-empty -m 'silo live test'
git push -q origin HEAD:refs/heads/` + branch + ` 2>&1; echo "push-silo=$?"
git push origin HEAD:refs/heads/main 2>&1; echo "push-main=$?"
echo "--- config"; cat .git/config
echo "--- env"; env
`
	e.fake(fakeConfig{Call: "sandbox_exec", Args: map[string]any{"sandbox": "dev", "command": script}})
	f := e.silofile(headless + `  - name: github
    from: {file: ` + liveTokenFile + `}
    to: [{proxy: git}]
git:
  - name: live
    repo: ` + repo + `
    access: push
    push: [refs/heads/silo/*]
    credential: github
sandboxes:
  - name: dev
    kind: container
    image: ` + gitImage + `
    workdir: /work
    repos: [live]
    env: {GIT_AUTHOR_NAME: Silo, GIT_AUTHOR_EMAIL: silo@example.invalid, GIT_COMMITTER_NAME: Silo, GIT_COMMITTER_EMAIL: silo@example.invalid}
`)
	res := e.run(f, func(o *session.Options) { o.Backend = b })
	if res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	out := e.report().Result
	t.Logf("in the sandbox:\n%s", out)
	if !strings.Contains(out, "push-silo=0") {
		t.Errorf("the push to %s failed", branch)
	}
	if strings.Contains(out, "push-main=0") || !strings.Contains(out, "matches no push pattern") {
		t.Error("the push to main was not refused by the proxy")
	}
	if strings.Contains(out, strings.TrimSpace(string(token))) {
		t.Error("the token reached the sandbox")
	}
}
