package gitstore_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/repos"
)

var ctx = context.Background()

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitstore.Env(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A repo is a bare repo in its silo's repos/, with silod's config and
// its creation sequence (local-git.md, "On disk" and "How a request is
// served"); a seeded one holds the seed's heads and tags, and nothing
// hidden (local-git.md, "Seeding").
func TestHost(t *testing.T) {
	needGit(t)
	root := filepath.Join(t.TempDir(), "silos")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &gitstore.Host{Storage: &storage.Dir{Root: root}, HooksDir: "/opt/silo/hooks"}

	head, size, err := h.Create(ctx, "k7f3qa", "r-aaaaaa", "", 41, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "k7f3qa", "repos", "r-aaaaaa.git")
	if head != "main" || size <= 0 {
		t.Errorf("Create = %q, %d", head, size)
	}
	if got := git(t, p, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("bare = %s", got)
	}
	for _, kv := range [][2]string{
		{"core.hooksPath", "/opt/silo/hooks"}, {"receive.fsckObjects", "true"}, {"uploadpack.allowFilter", "true"},
		{"uploadpack.hideRefs", "refs/silo-archive/"}, {"receive.hideRefs", "refs/silo-archive/"},
	} {
		if got := git(t, p, "config", kv[0]); got != kv[1] {
			t.Errorf("%s = %q, want %q", kv[0], got, kv[1])
		}
	}
	if seq, err := h.Seq("k7f3qa", "r-aaaaaa"); err != nil || seq != 41 {
		t.Errorf("Seq = %d %v", seq, err)
	}
	if err := h.SetHead(ctx, "k7f3qa", "r-aaaaaa", "trunk"); err != nil {
		t.Fatal(err)
	}
	if b, err := h.Head(ctx, "k7f3qa", "r-aaaaaa"); err != nil || b != "trunk" {
		t.Errorf("Head = %q %v", b, err)
	}

	// Give it history, a tag and an archive ref, through a clone.
	work := t.TempDir()
	git(t, work, "init", "--quiet", "--initial-branch=trunk", ".")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "README")
	git(t, work, "commit", "--quiet", "-m", "first")
	git(t, work, "tag", "v1")
	git(t, work, "branch", "side")
	tip := git(t, work, "rev-parse", "HEAD")
	git(t, work, "push", "--quiet", p, "trunk", "side", "v1")
	git(t, p, "update-ref", "refs/silo-archive/old", tip)
	git(t, p, "update-ref", "refs/notes/commits", tip)

	// A seeded repo takes the seed's HEAD unless it is given another.
	head, _, err = h.Create(ctx, "b2b2b2", "r-bbbbbb", "", 42, &repos.Source{Silo: "k7f3qa", ID: "r-aaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	if head != "trunk" {
		t.Errorf("seeded HEAD = %q, want the seed's trunk", head)
	}
	q := filepath.Join(root, "b2b2b2", "repos", "r-bbbbbb.git")
	refs := git(t, q, "for-each-ref", "--format=%(refname) %(objectname)")
	want := "refs/heads/side " + tip + "\nrefs/heads/trunk " + tip + "\nrefs/tags/v1 " + tip
	if refs != want {
		t.Errorf("seeded refs:\n%s\nwant\n%s", refs, want)
	}
	if out := git(t, q, "config", "--list"); strings.Contains(out, "remote.") {
		t.Errorf("seeded repo keeps a remote:\n%s", out)
	}
	if got := git(t, q, "config", "core.hooksPath"); got != "/opt/silo/hooks" {
		t.Errorf("seeded core.hooksPath = %q", got)
	}
	if seq, err := h.Seq("b2b2b2", "r-bbbbbb"); err != nil || seq != 42 {
		t.Errorf("seeded Seq = %d %v", seq, err)
	}
	head, _, err = h.Create(ctx, "b2b2b2", "r-cccccc", "side", 43, &repos.Source{Silo: "k7f3qa", ID: "r-aaaaaa"})
	if err != nil || head != "side" {
		t.Errorf("seeded with a branch: %q %v", head, err)
	}

	// A failed create leaves nothing; so does a delete.
	if _, _, err := h.Create(ctx, "b2b2b2", "r-dddddd", "", 44, &repos.Source{Silo: "k7f3qa", ID: "r-missin"}); err == nil {
		t.Errorf("seeding from a missing repo succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "b2b2b2", "repos", "r-dddddd.git")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed create left %v", err)
	}
	if _, _, err := h.Create(ctx, "b2b2b2", "r-eeeeee", "a..b", 45, nil); err == nil {
		t.Errorf("an invalid branch was taken")
	}
	for range 2 {
		if err := h.Delete(ctx, "k7f3qa", "r-aaaaaa"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after Delete: %v", err)
	}
}
