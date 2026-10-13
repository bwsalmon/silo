package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// externalSubmodules points silo submodules at the uber repo, all the
// way down, and fetches what they pin from it by SHA, through a
// single-branch clone that has none of the branches they are on
// (local-git.md, "External view: not rewritten").
func TestExternalSubmodules(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		// git refuses file:// submodules unless told otherwise.
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "protocol.file.allow", "GIT_CONFIG_VALUE_0": "always"} {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	uber := filepath.Join(dir, "uber.git")
	gitIn(t, "", "init", "--quiet", "--bare", uber)
	gitIn(t, uber, "config", "uploadpack.allowAnySHA1InWant", "true")
	url := "file://" + uber

	// commit makes a root commit in a fresh repo with file, and the
	// gitlinks given (path: sha, url), and pushes it to ref.
	n := 0
	commit := func(ref, file string, links map[string][2]string) string {
		n++
		w := filepath.Join(dir, "w", string(rune('a'+n)))
		gitIn(t, "", "init", "--quiet", "-b", "main", w)
		if err := os.WriteFile(filepath.Join(w, file), []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, w, "add", file)
		for p, l := range links {
			gitIn(t, w, "config", "-f", ".gitmodules", "submodule."+p+".path", p)
			gitIn(t, w, "config", "-f", ".gitmodules", "submodule."+p+".url", l[1])
			gitIn(t, w, "update-index", "--add", "--cacheinfo", "160000,"+l[0]+","+p)
			gitIn(t, w, "add", ".gitmodules")
		}
		gitIn(t, w, "commit", "--quiet", "-m", file)
		gitIn(t, w, "push", "--quiet", uber, "HEAD:"+ref)
		return gitIn(t, w, "rev-parse", "HEAD")
	}
	deep := commit("refs/heads/silo/deep.git/main", "deep.txt", nil)
	lib := commit("refs/heads/silo/team-a/x/lib.git/main", "lib.txt",
		map[string][2]string{"deep": {deep, "http://silo.internal/git/-k7f3qa/deep.git"}})
	commit("refs/heads/silo/team-a/y/app.git/main", "app.txt",
		map[string][2]string{"libs/lib": {lib, "http://silo.internal/git/team-a/x/lib.git"}})
	gitIn(t, uber, "symbolic-ref", "HEAD", "refs/heads/silo/deep.git/main")

	var errb bytes.Buffer
	c := &cli{stderr: &errb}
	ctx := context.Background()
	clone := filepath.Join(dir, "app")
	if err := c.git(ctx, "", "clone", "--quiet", "--single-branch", "-b", "silo/team-a/y/app.git/main", url, clone); err != nil {
		t.Fatal(err)
	}
	got, err := c.externalSubmodules(ctx, clone, url)
	if err != nil {
		t.Fatalf("externalSubmodules: %v\n%s", err, errb.String())
	}
	if got != 2 {
		t.Errorf("pointed %d submodules at the uber repo, want 2", got)
	}
	for _, f := range []string{"app.txt", "libs/lib/lib.txt", "libs/lib/deep/deep.txt"} {
		if _, err := os.Stat(filepath.Join(clone, f)); err != nil {
			t.Errorf("%s: %v\n%s", f, err, errb.String())
		}
	}
	if u := gitIn(t, clone, "config", "submodule.libs/lib.url"); u != url {
		t.Errorf("libs/lib's URL is %s, want %s", u, url)
	}
	if h := gitIn(t, filepath.Join(clone, "libs/lib"), "rev-parse", "HEAD"); h != lib {
		t.Errorf("libs/lib is at %s, want %s", h, lib)
	}

	// A repo with no submodules is left as it is.
	plain := filepath.Join(dir, "deep")
	if err := c.git(ctx, "", "clone", "--quiet", "--single-branch", "-b", "silo/deep.git/main", url, plain); err != nil {
		t.Fatal(err)
	}
	if got, err := c.externalSubmodules(ctx, plain, url); err != nil || got != 0 {
		t.Errorf("no submodules: %d, %v", got, err)
	}
}
