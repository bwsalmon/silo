package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/client"
)

// silo repo create|list|get|delete|url (local-git.md, §2 "Operations").
func TestRepoCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
	d := startSilod(t)
	var a client.Silo
	d.json(d.root, &a, "create", "/team-a", "--address", "/16", "--quota", "repos=5", "--repos-mirror", "false")
	var x client.Silo
	d.json(d.root, &x, "create", "/team-a/x", "--address", "/24", "--quota", "global:repos=2")
	if x.Repos.Own.Mirror || x.Repos.EffectiveFrom != "/team-a" {
		t.Errorf("x's repos policy: %+v", x.Repos)
	}
	contains(t, "describe", d.ok(d.root, "describe", "/team-a/x"), "Repos mirror:", "off, from /team-a (own: off)")

	out := d.ok(x.ID, "repo", "create", "foo", "--default-branch", "trunk")
	contains(t, "repo create", out, "created foo in /team-a/x", "trunk", "http://silo.internal/git/team-a/x/foo.git", "OFF (repos.mirror off from /team-a)")
	var r client.Repo
	d.json(a.ID, &r, "repo", "create", "x/bar", "--seed", "x/foo")
	if r.Silo != "/team-a/x" || r.DefaultBranch != "trunk" {
		t.Errorf("repo create x/bar: %+v", r)
	}
	d.ok(a.ID, "repo", "create", "own")
	contains(t, "repo list", d.ok(a.ID, "repo", "list", "x"), "/team-a/x", "bar", "foo")
	out = d.ok(d.root, "repo", "list", "--recursive", "/team-a")
	contains(t, "repo list --recursive", out, "/team-a ", "own", "/team-a/x", "bar")
	var l struct{ Repos []client.Repo }
	d.json(d.root, &l, "repo", "list", "/team-a/x")
	if len(l.Repos) != 2 {
		t.Errorf("repo list --output json: %+v", l)
	}
	contains(t, "repo get", d.ok(x.ID, "repo", "get", "/team-a/x/foo"), "Default branch:", "trunk", "Clone URL:")

	if got := strings.TrimSpace(d.ok(x.ID, "repo", "url", "foo")); got != "http://silo.internal/git/team-a/x/foo.git" {
		t.Errorf("repo url = %q", got)
	}
	if got := strings.TrimSpace(d.ok(x.ID, "repo", "url", "foo", "--by-id")); got != "http://silo.internal/git/-"+x.ID+"/foo.git" {
		t.Errorf("repo url --by-id = %q", got)
	}

	res := d.silo(x.ID, "repo", "create", "baz")
	contains(t, "over quota", res.stderr, "QUOTA_EXCEEDED")
	res = d.silo(x.ID, "repo", "get", "/team-a/own")
	contains(t, "a parent's repo", res.stderr, "NOT_FOUND")

	contains(t, "repo delete", d.ok(x.ID, "repo", "delete", "foo"), "deleted /team-a/x/foo")
	res = d.silo(x.ID, "repo", "get", "foo")
	contains(t, "deleted", res.stderr, "NOT_FOUND")
	d.ok(d.root, "patch", "/team-a", "--repos-mirror", "true")
	contains(t, "mirror back on", d.ok(a.ID, "repo", "get", "own"), "UNCONFIGURED (refs/heads/silo/team-a/own.git/)")
}
