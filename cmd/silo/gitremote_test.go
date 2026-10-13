package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/client"
	"github.com/bwsalmon/silo/pkg/gitserve"
)

// testCaller is the environment variable the test binary, run by git
// as git-remote-silo, takes its caller from: asCaller's header, which
// the front door would know from the user instead.
const testCaller = "SILO_TEST_CALLER"

// TestMain runs the test binary as git-remote-silo, and as the git
// service's pre-receive hook, when git runs it so.
func TestMain(m *testing.M) {
	switch {
	case filepath.Base(os.Args[0]) == RemoteHelper:
		c := &cli{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, transport: asCaller{os.Getenv(testCaller)}}
		os.Exit(c.run(context.Background(), append([]string{RemoteHelper}, os.Args[1:]...)))
	case len(os.Args) > 1 && os.Args[1] == gitserve.HookArg:
		os.Exit(gitserve.HookMain(os.Args[2:], os.Stdin, os.Stderr))
	}
	os.Exit(m.Run())
}

// silo repo clone, and git through git-remote-silo after it; then
// silo repo archives and restore-ref (local-git.md, "Where it listens"
// and "Local archive refs").
func TestRepoCloneAndArchives(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
	d := startSilod(t)
	var x client.Silo
	d.json(d.root, &x, "create", "/team-a", "--address", "/16", "--quota", "repos=2")
	d.ok(x.ID, "repo", "create", "foo")
	t.Setenv(testCaller, x.ID)
	// Only the helper silo repo clone links: none on $PATH.
	t.Setenv("PATH", filepath.Dir(mustLook(t, "git")))

	dir := filepath.Join(t.TempDir(), "foo")
	d.ok(x.ID, "repo", "clone", "foo", dir)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		bin := t.TempDir()
		exe, _ := os.Executable()
		os.Symlink(exe, filepath.Join(bin, RemoteHelper))
		cmd.Env = append(gitstore.Env(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@example.com", "SILO_ENDPOINT="+d.url, testCaller+"="+x.ID,
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	if got := strings.TrimSpace(git("remote", "get-url", "origin")); got != "silo::/team-a/foo" {
		t.Errorf("origin is %q", got)
	}
	os.WriteFile(filepath.Join(dir, "README"), []byte("one\n"), 0o644)
	git("add", "README")
	git("commit", "-q", "-m", "one")
	first := strings.TrimSpace(git("rev-parse", "HEAD"))
	git("push", "-q", "origin", "HEAD:main")
	git("commit", "-q", "--amend", "-m", "one, again")
	git("push", "-q", "--force", "origin", "HEAD:main")
	git("fetch", "-q", "origin")
	git("ls-remote", "silo::-"+x.ID+"/foo")

	var l struct{ Archives []client.RepoArchive }
	d.json(x.ID, &l, "repo", "archives", "foo")
	if len(l.Archives) != 1 || l.Archives[0].Commit != first || l.Archives[0].Ref != "refs/heads/main" {
		t.Fatalf("repo archives: %+v", l)
	}
	contains(t, "repo archives", d.ok(x.ID, "repo", "archives", "foo"), "ARCHIVE", l.Archives[0].Name, first)
	contains(t, "restore-ref", d.ok(d.root, "repo", "restore-ref", "/team-a/foo", l.Archives[0].Name, "old-main"),
		"made branch old-main of /team-a/foo at "+first)
	contains(t, "ls-remote", git("ls-remote", "origin"), first+"\trefs/heads/old-main")
	res := d.silo(x.ID, "repo", "restore-ref", "foo", l.Archives[0].Name, "old-main")
	contains(t, "restore-ref onto a branch", res.stderr, "ALREADY_EXISTS")
}

func mustLook(t *testing.T, bin string) string {
	t.Helper()
	p, err := exec.LookPath(bin)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
