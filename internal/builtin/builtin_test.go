package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/config"
	"github.com/bwsalmon/silo/internal/gitrepo"
	"github.com/bwsalmon/silo/internal/mcp"
	"github.com/bwsalmon/silo/internal/router"
	"github.com/bwsalmon/silo/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.MaybeServe()
	os.Exit(m.Run())
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	co      *gitrepo.Checkout
	bare    string
	rt      *router.Router
	host    *router.Host
	fake    *approval.Fake
	log     *audit.Memory
	scratch *Scratch
}

const branch = "silo/test"

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	bare := testutil.Remote(t, base, "ctx", files)
	cfg := &config.Config{Repos: config.Repos{Root: filepath.Join(base, "co"), GitBase: base + "/"}, Push: config.Push{Remote: "origin"}}
	co, err := gitrepo.Prepare(ctx, cfg, "ctx.git", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := co.StartBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, co: co, bare: bare, fake: approval.NewFake(), log: &audit.Memory{}}
	sessionDir := t.TempDir()
	f.rt = router.New(f.fake, f.log)
	f.host = &router.Host{Router: f.rt, Checkout: co.Dir, SessionDir: sessionDir, Disabled: map[string]bool{}, Log: f.log}
	w := &Writer{Repo: &co.Repo, Branch: branch, Remote: "origin", Identity: config.Identity{Name: "Ada", Email: "ada@example.com"}, Approver: f.fake, Host: f.host, Log: f.log}
	f.scratch = &Scratch{Dir: filepath.Join(sessionDir, "tools"), Approver: f.fake, Host: f.host, Writer: w, Log: f.log}
	rf := &Refresher{Checkout: co, Branch: branch, Host: f.host, Log: f.log}
	f.rt.Add(ctx, "repo-read", NewRepoRead(&co.Repo), router.Options{Origin: router.Builtin})
	f.rt.Add(ctx, "repo-write", NewRepoWrite(w), router.Options{Origin: router.Builtin})
	f.rt.Add(ctx, "scratch", NewScratch(f.scratch), router.Options{Origin: router.Builtin})
	f.rt.Add(ctx, "silo", NewSilo(rf, f.rt, true), router.Options{Origin: router.Builtin})
	t.Cleanup(f.rt.Close)
	return f
}

func (f *fixture) call(tool string, args any) *mcp.ToolResult {
	f.t.Helper()
	raw, _ := json.Marshal(args)
	return f.rt.Call(f.ctx, tool, raw)
}

func (f *fixture) ok(tool string, args any) string {
	f.t.Helper()
	res := f.call(tool, args)
	if res.IsError {
		f.t.Fatalf("%s: %s", tool, res.AllText())
	}
	return res.AllText()
}

func approveNext(f *fixture, ds ...approval.Decision) {
	f.fake.Reset(ds...)
}

func yes() approval.Decision          { return approval.Decision{Approved: true} }
func no(why string) approval.Decision { return approval.Decision{Reason: why} }

func TestRepoRead(t *testing.T) {
	f := newFixture(t, map[string]string{
		"README.md":       "# Payments\nline two\nline three\n",
		"docs/runbook.md": "Restart the payments service.\n",
	})
	if got := f.ok("repo-read.list_files", nil); !strings.Contains(got, "README.md") || !strings.Contains(got, "docs/runbook.md") || strings.Contains(got, ".git/") {
		t.Errorf("list_files = %q", got)
	}
	if got := f.ok("repo-read.list_files", map[string]any{"recursive": false}); !strings.Contains(got, "docs/") || strings.Contains(got, "runbook") {
		t.Errorf("list_files non-recursive = %q", got)
	}
	if got := f.ok("repo-read.read_file", map[string]any{"path": "README.md", "offset": 2, "limit": 1}); got != "line two\n" {
		t.Errorf("read_file = %q", got)
	}
	if got := f.ok("repo-read.grep", map[string]any{"pattern": "RESTART", "ignore_case": true}); got != "docs/runbook.md:1:Restart the payments service." {
		t.Errorf("grep = %q", got)
	}
	if got := f.ok("repo-read.git_log", nil); !strings.Contains(got, "initial") {
		t.Errorf("git_log = %q", got)
	}
	if got := f.ok("repo-read.git_show", map[string]any{"ref": "HEAD", "path": "docs/runbook.md"}); !strings.Contains(got, "Restart") {
		t.Errorf("git_show path = %q", got)
	}
	if got := f.ok("repo-read.git_show", map[string]any{"ref": "HEAD"}); !strings.Contains(got, "+# Payments") {
		t.Errorf("git_show = %q", got)
	}
	if got := f.ok("repo-read.git_diff", map[string]any{"from": "HEAD"}); got != "(no output)" {
		t.Errorf("git_diff = %q", got)
	}

	os.Symlink("/etc", filepath.Join(f.co.Dir, "escape"))
	os.Symlink(".git", filepath.Join(f.co.Dir, "gitlink"))
	os.Symlink(".git/hooks", filepath.Join(f.co.Dir, "hooklink"))
	os.Symlink(".git/hooks/post-commit", filepath.Join(f.co.Dir, "dangling-hook"))
	planted := filepath.Join(t.TempDir(), "planted")
	os.Symlink(planted, filepath.Join(f.co.Dir, "dangling-out"))
	// On a case-sensitive file system .GIT is an ordinary directory, so
	// make one: the refusal must not depend on it being missing.
	os.MkdirAll(filepath.Join(f.co.Dir, ".GIT"), 0o755)
	os.WriteFile(filepath.Join(f.co.Dir, ".GIT", "config"), []byte("x\n"), 0o644)
	for _, args := range []map[string]any{
		{"path": "../outside"},
		{"path": "/etc/passwd"},
		{"path": ".git/config"},
		{"path": ".GIT/config"},
		{"path": "sub/.Git/config"},
		{"path": "escape/passwd"},
		{"path": "gitlink/config"},
		{"path": "hooklink"},
	} {
		if res := f.call("repo-read.read_file", args); !res.IsError {
			t.Errorf("read_file %v allowed: %s", args, res.AllText())
		}
	}
	// Nor may repo-write plant a hook, or a file outside the repository,
	// through a symlink, dangling or not.
	for _, p := range []string{"hooklink/pre-commit", "gitlink/hooks/pre-commit", "dangling-hook", "dangling-out"} {
		approveNext(f, yes())
		res := f.call("repo-write.propose_change", map[string]any{
			"message": "Add a hook",
			"edits":   []map[string]any{{"path": p, "content": "#!/bin/sh\n"}},
		})
		if !res.IsError || f.fake.Asked() != 0 {
			t.Errorf("propose_change to %s: %v %q (asked %d)", p, res.IsError, res.AllText(), f.fake.Asked())
		}
	}
	for _, p := range []string{".git/hooks/pre-commit", ".git/hooks/post-commit"} {
		if _, err := os.Stat(filepath.Join(f.co.Dir, p)); err == nil {
			t.Errorf("%s written through a symlink", p)
		}
	}
	if _, err := os.Lstat(planted); err == nil {
		t.Error("file planted outside the repository")
	}
	if res := f.call("repo-read.git_log", map[string]any{"ref": "--output=/tmp/x"}); !res.IsError {
		t.Error("option-like ref allowed")
	}
}

func TestProposeRejectedWritesNothing(t *testing.T) {
	f := newFixture(t, nil)
	approveNext(f, no("wrong file"))
	head, _ := f.co.Head(f.ctx)
	res := f.call("repo-write.propose_change", map[string]any{
		"message": "Add notes",
		"edits":   []map[string]any{{"path": "notes.md", "content": "hello\n"}},
	})
	if !res.IsError || !strings.Contains(res.AllText(), "wrong file") {
		t.Fatalf("result = %v %q", res.IsError, res.AllText())
	}
	if f.fake.Asked() != 1 || !strings.Contains(f.fake.Requests[0].Body, "+hello") {
		t.Errorf("approval request = %+v", f.fake.Requests)
	}
	if _, err := os.Stat(filepath.Join(f.co.Dir, "notes.md")); !os.IsNotExist(err) {
		t.Error("file written despite rejection")
	}
	if now, _ := f.co.Head(f.ctx); now != head {
		t.Error("commit made despite rejection")
	}
	if out := testutil.Git(t, f.bare, "branch", "--list", branch); strings.TrimSpace(out) != "" {
		t.Error("pushed despite rejection")
	}
	if f.log.Count("tool_call") != 1 {
		t.Errorf("audit: %v", f.log.Events())
	}
}

func TestProposeApprovedCommitsAndPushes(t *testing.T) {
	f := newFixture(t, map[string]string{"README.md": "# Old title\n", "stale.md": "x\n"})
	approveNext(f, yes())
	got := f.ok("repo-write.propose_change", map[string]any{
		"message": "Retitle and add notes\n\nBecause.",
		"edits": []map[string]any{
			{"path": "README.md", "old_string": "Old", "new_string": "New"},
			{"path": "notes/a.md", "content": "hello\n"},
			{"path": "stale.md", "delete": true},
		},
	})
	if !strings.Contains(got, "Pushed to origin/"+branch) {
		t.Errorf("result = %q", got)
	}
	body := f.fake.Requests[0].Body
	for _, want := range []string{"-# Old title", "+# New title", "+hello", "--- a/stale.md", "Retitle and add notes"} {
		if !strings.Contains(body, want) {
			t.Errorf("approval body lacks %q:\n%s", want, body)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(f.co.Dir, "README.md")); string(data) != "# New title\n" {
		t.Errorf("README = %q", data)
	}
	author := testutil.Git(t, f.bare, "log", "-1", "--format=%an <%ae>|%cn|%s", branch)
	if strings.TrimSpace(author) != "Ada <ada@example.com>|Ada|Retitle and add notes" {
		t.Errorf("pushed commit = %q", author)
	}
	if st := testutil.Git(t, f.co.Dir, "status", "--porcelain"); st != "" {
		t.Errorf("checkout dirty after commit: %q", st)
	}
}

// An edit's path is a file name, never a git pathspec: ":(glob)**" must
// not make the commit sweep up files the person was not shown.
func TestProposeCommitsOnlyTheShownFiles(t *testing.T) {
	f := newFixture(t, map[string]string{"README.md": "# context\n"})
	if err := os.WriteFile(filepath.Join(f.co.Dir, "unshown.txt"), []byte("not in the diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	approveNext(f, yes())
	f.ok("repo-write.propose_change", map[string]any{
		"message": "Add a file",
		"edits":   []map[string]any{{"path": ":(glob)**", "content": "x\n"}},
	})
	files := testutil.Git(t, f.bare, "show", "--name-only", "--format=", branch)
	if strings.TrimSpace(files) != ":(glob)**" {
		t.Errorf("pushed commit touches %q, want only the proposed file", files)
	}
}

// Editing a symlink would write its target while the person is shown,
// and git commits, the link's path; such an edit is refused.
func TestProposeRefusesEditThroughSymlinkedFile(t *testing.T) {
	f := newFixture(t, map[string]string{"README.md": "# context\n"})
	os.Symlink("README.md", filepath.Join(f.co.Dir, "alias.md"))
	res := f.call("repo-write.propose_change", map[string]any{
		"message": "m",
		"edits":   []map[string]any{{"path": "alias.md", "content": "changed\n"}},
	})
	if !res.IsError {
		t.Errorf("edit through a symlink accepted: %s", res.AllText())
	}
	if f.fake.Asked() != 0 {
		t.Errorf("asked %d times for a refused proposal", f.fake.Asked())
	}
}

func TestProposeInvalidEditNeverAsks(t *testing.T) {
	f := newFixture(t, nil)
	for _, edits := range [][]map[string]any{
		{{"path": "README.md", "old_string": "absent", "new_string": "x"}},
		{{"path": "../x", "content": "x"}},
		{{"path": ".git/hooks/pre-commit", "content": "x"}},
		{{"path": "README.md", "content": "x", "delete": true}},
		{{"path": "README.md", "content": "# context\n"}},
	} {
		if res := f.call("repo-write.propose_change", map[string]any{"message": "m", "edits": edits}); !res.IsError {
			t.Errorf("%v accepted", edits)
		}
	}
	if f.fake.Asked() != 0 {
		t.Errorf("asked %d times for invalid proposals", f.fake.Asked())
	}
}

func addEcho(f *fixture, name, prefix string) *mcp.ToolResult {
	f.t.Helper()
	return f.call("repo-write.propose_change", map[string]any{
		"message": "Add tool set " + name,
		"edits":   []map[string]any{{"path": ".silo/tools/" + name + "/toolset.yaml", "content": testutil.EchoManifest(f.t, name, prefix)}},
	})
}

func TestHotReloadAfterApprovedCommit(t *testing.T) {
	f := newFixture(t, nil)
	approveNext(f, yes(), yes())
	res := addEcho(f, "echo", "v1")
	if res.IsError || !strings.Contains(res.AllText(), "loaded echo (tools: echo.echo, echo.env, echo.pwd)") {
		t.Fatalf("add: %q", res.AllText())
	}
	if got := f.ok("echo.echo", map[string]any{"text": "hi"}); got != "v1:hi" {
		t.Errorf("echo = %q", got)
	}
	// The child gets only what its manifest declares, and runs in the checkout.
	env := f.ok("echo.env", nil)
	names := " " + strings.Join(strings.Fields(env), " ") + " "
	if !strings.Contains(names, " ECHO_PREFIX ") || !strings.Contains(names, " PATH ") || strings.Contains(names, " HOME ") {
		t.Errorf("child env = %q", env)
	}
	if got := f.ok("echo.pwd", nil); got != f.co.Dir {
		t.Errorf("child cwd = %q, want %q", got, f.co.Dir)
	}
	// A changed set restarts in place.
	res = addEcho(f, "echo", "v2")
	if res.IsError || !strings.Contains(res.AllText(), "reloaded echo") {
		t.Fatalf("change: %q", res.AllText())
	}
	if got := f.ok("echo.echo", map[string]any{"text": "hi"}); got != "v2:hi" {
		t.Errorf("echo after reload = %q", got)
	}
	// Deleting it unloads it.
	approveNext(f, yes())
	f.ok("repo-write.propose_change", map[string]any{"message": "Drop echo", "edits": []map[string]any{{"path": ".silo/tools/echo/toolset.yaml", "delete": true}}})
	if f.rt.Has("echo") {
		t.Error("echo still loaded after deletion")
	}
}

func TestReloadFailureReturnsBuildOutput(t *testing.T) {
	f := newFixture(t, nil)
	approveNext(f, yes())
	res := f.call("repo-write.propose_change", map[string]any{
		"message": "Add broken",
		"edits": []map[string]any{{"path": ".silo/tools/broken/toolset.yaml", "content": "name: broken\n" +
			"build: [sh, -c, 'echo compiler says no >&2; exit 3']\nrun: [./server]\n"}},
	})
	text := res.AllText()
	if !res.IsError || !strings.Contains(text, "build failed") || !strings.Contains(text, "compiler says no") || !strings.Contains(text, "Approved and committed") {
		t.Errorf("result = %v %q", res.IsError, text)
	}
	if f.rt.Has("broken") {
		t.Error("broken set loaded")
	}
}

func TestDisabledToolsetIsNotLoaded(t *testing.T) {
	f := newFixture(t, nil)
	f.host.Disabled["echo"] = true
	approveNext(f, yes())
	if res := addEcho(f, "echo", "v1"); !strings.Contains(res.AllText(), "skipped echo: disabled by config") {
		t.Errorf("result = %q", res.AllText())
	}
	if f.rt.Has("echo") {
		t.Error("disabled set loaded")
	}
}

func defineArgs(t *testing.T, prefix string) map[string]any {
	return map[string]any{"name": "tmp", "manifest": testutil.EchoManifest(t, "tmp", prefix), "files": map[string]string{"NOTES.txt": "scratch notes " + prefix + "\n"}}
}

func TestScratchNeedsApprovalToCreateAndChange(t *testing.T) {
	f := newFixture(t, nil)
	approveNext(f, no("not now"))
	res := f.call("scratch.define", defineArgs(t, "s1"))
	if !res.IsError || !strings.Contains(res.AllText(), "not now") {
		t.Fatalf("rejected define = %q", res.AllText())
	}
	req := f.fake.Requests[0]
	if req.Kind != "scratch" || !strings.Contains(req.Body, "==== toolset.yaml ====") || !strings.Contains(req.Body, "ECHO_PREFIX: s1") || !strings.Contains(req.Body, "scratch notes s1") {
		t.Errorf("approval request = %+v", req)
	}
	if f.rt.Has("scratch.tmp") {
		t.Fatal("loaded despite rejection")
	}
	if _, err := os.Stat(filepath.Join(f.scratch.Dir, "tmp")); !os.IsNotExist(err) {
		t.Fatal("written despite rejection")
	}

	approveNext(f, yes())
	f.ok("scratch.define", defineArgs(t, "s1"))
	if got := f.ok("scratch.tmp.echo", map[string]any{"text": "x"}); got != "s1:x" {
		t.Errorf("scratch echo = %q", got)
	}

	approveNext(f, no("keep s1"))
	f.call("scratch.define", defineArgs(t, "s2"))
	if body := f.fake.Requests[0].Body; !strings.Contains(body, "Changes from the running version") || !strings.Contains(body, "+  ECHO_PREFIX: s2") {
		t.Errorf("change request body = %q", body)
	}
	if got := f.ok("scratch.tmp.echo", map[string]any{"text": "x"}); got != "s1:x" {
		t.Errorf("rejected change applied: %q", got)
	}
	approveNext(f, yes())
	f.ok("scratch.define", defineArgs(t, "s2"))
	if got := f.ok("scratch.tmp.echo", map[string]any{"text": "x"}); got != "s2:x" {
		t.Errorf("approved change not applied: %q", got)
	}
	if st := testutil.Git(t, f.co.Dir, "status", "--porcelain"); st != "" {
		t.Errorf("one-off tool touched the checkout: %q", st)
	}
}

func TestScratchCleanup(t *testing.T) {
	f := newFixture(t, nil)
	approveNext(f, yes())
	f.ok("scratch.define", defineArgs(t, "s1"))
	if _, err := os.Stat(filepath.Join(f.scratch.Dir, "tmp", "toolset.yaml")); err != nil {
		t.Fatal(err)
	}
	f.rt.Close()
	if err := f.scratch.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.scratch.Dir); !os.IsNotExist(err) {
		t.Error("one-off tools left behind")
	}
}

func TestScratchPromote(t *testing.T) {
	f := newFixture(t, nil)
	approveNext(f, yes(), yes())
	f.ok("scratch.define", defineArgs(t, "s1"))
	got := f.ok("scratch.promote", map[string]any{"name": "tmp", "message": "Promote tmp"})
	if !strings.Contains(got, "loaded tmp") || !strings.Contains(got, "Removed the one-off scratch.tmp") {
		t.Errorf("promote = %q", got)
	}
	if req := f.fake.Requests[1]; req.Kind != "commit" || !strings.Contains(req.Body, "+++ b/.silo/tools/tmp/toolset.yaml") {
		t.Errorf("promote approval = %+v", req)
	}
	if f.rt.Has("scratch.tmp") || !f.rt.Has("tmp") {
		t.Errorf("toolsets = %v", f.rt.Toolsets())
	}
	if got := f.ok("tmp.echo", map[string]any{"text": "x"}); got != "s1:x" {
		t.Errorf("promoted echo = %q", got)
	}
	if out := testutil.Git(t, f.bare, "show", branch+":.silo/tools/tmp/NOTES.txt"); out != "scratch notes s1\n" {
		t.Errorf("promoted file = %q", out)
	}
}

func TestRefreshPullsAndReloads(t *testing.T) {
	f := newFixture(t, nil)
	if got := f.ok("silo.refresh", nil); !strings.Contains(got, "Already up to date") {
		t.Errorf("refresh = %q", got)
	}
	testutil.PushFiles(t, f.bare, "main", map[string]string{".silo/tools/echo/toolset.yaml": testutil.EchoManifest(t, "echo", "pulled")})
	got := f.ok("silo.refresh", nil)
	if !strings.Contains(got, "Fast-forwarded to origin/main") || !strings.Contains(got, "loaded echo") || !strings.Contains(got, ".silo/tools/echo/toolset.yaml") {
		t.Errorf("refresh = %q", got)
	}
	if f.fake.Asked() != 0 {
		t.Error("refresh asked for approval")
	}
	if got := f.ok("echo.echo", map[string]any{"text": "x"}); got != "pulled:x" {
		t.Errorf("echo = %q", got)
	}
}

func TestFallbackCall(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "A\n"})
	if got := f.ok("silo.list_tools", nil); !strings.Contains(got, `"repo-read.read_file"`) {
		t.Errorf("list_tools = %q", got)
	}
	if got := f.ok("silo.call", map[string]any{"tool": "repo-read.read_file", "arguments": map[string]any{"path": "a.txt"}}); got != "A\n" {
		t.Errorf("call = %q", got)
	}
}
