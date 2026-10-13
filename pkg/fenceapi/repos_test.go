package fenceapi_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/gitstore"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
)

// newRepoEnv is newEnv with repos kept by gitstore under a temporary
// data directory's silos/, which it returns.
func newRepoEnv(t *testing.T) (*env, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on $PATH")
	}
	silos := filepath.Join(t.TempDir(), "silos")
	if err := os.Mkdir(silos, 0o755); err != nil {
		t.Fatal(err)
	}
	return newEnv(t, fenceapi.Options{Repos: &gitstore.Host{Storage: &storage.Dir{Root: silos}}}), silos
}

func repoNames(l obj) string {
	var out []string
	for _, r := range l["repos"].([]any) {
		out = append(out, r.(obj)["silo"].(string)+":"+r.(obj)["name"].(string))
	}
	return strings.Join(out, " ")
}

// Repos are silo objects (local-git.md, §2): created, listed, read,
// patched and deleted through the fence API, kept as bare repos in the
// silo's subtree, and counted in the repos quota class.
func TestRepos(t *testing.T) {
	e, silos := newRepoEnv(t)
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"quota": obj{"global": obj{"repos": 3}}})
	x := e.mk(e.root, "team-a", "x", obj{"quota": obj{"global": obj{"repos": 2}}})
	b := e.mk(e.root, "-"+e.root, "team-b", obj{"quota": obj{"global": obj{"repos": 1}}})

	r := e.ok(x, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
	for path, want := range map[string]string{
		"name":                   "foo",
		"silo":                   "/team-a/x",
		"default_branch":         "main",
		"clone_url":              "http://silo.internal/git/team-a/x/foo.git",
		"clone_url_by_id":        "http://silo.internal/git/-" + x + "/foo.git",
		"mirror.state":           "UNCONFIGURED",
		"mirror.external_prefix": "refs/heads/silo/team-a/x/foo.git/",
		"mirror.last_error":      "<nil>",
	} {
		if got := str(get(r, strings.Split(path, ".")...)); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	id := r["id"].(string)
	if !strings.HasPrefix(id, "r-") || len(id) != 8 {
		t.Errorf("id = %s", id)
	}
	dir := filepath.Join(silos, x, "repos", id+".git")
	if seq, err := os.ReadFile(filepath.Join(dir, ".silo-seq")); err != nil || strings.TrimSpace(string(seq)) == "" {
		t.Errorf(".silo-seq: %q %v", seq, err)
	}
	if head, err := os.ReadFile(filepath.Join(dir, "HEAD")); err != nil || string(head) != "ref: refs/heads/main\n" {
		t.Errorf("HEAD: %q %v", head, err)
	}
	if n := r["size_bytes"].(float64); n <= 0 {
		t.Errorf("size_bytes = %v", n)
	}

	// Names are per silo, and checked.
	e.fail(fenceapi.AlreadyExists, x, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
	e.ok(b, "POST", "/silo/v1/self/repos", obj{"name": "foo", "default_branch": "trunk"})
	for _, name := range []string{"", "Foo", "foo.git", "a..b", ".x", strings.Repeat("a", 64)} {
		e.fail(fenceapi.InvalidArgument, x, "POST", "/silo/v1/self/repos", obj{"name": name})
	}
	e.fail(fenceapi.InvalidArgument, x, "POST", "/silo/v1/self/repos", obj{"name": "bar", "default_branch": "a..b"})

	// validate_only makes nothing.
	v := e.ok(x, "POST", "/silo/v1/self/repos?validate_only=true", obj{"name": "bar"})
	if v["name"] != "bar" || v["default_branch"] != "main" {
		t.Errorf("validate_only: %v", v)
	}
	e.fail(fenceapi.NotFound, x, "GET", "/silo/v1/self/repos/bar", nil)

	// An ancestor creates, reads and lists them; a sibling sees nothing.
	e.ok(a, "POST", "/silo/v1/silos/team-a/x/repos", obj{"name": "bar"})
	e.ok(a, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
	if got := repoNames(e.ok(a, "GET", "/silo/v1/silos/team-a/x/repos", nil)); got != "/team-a/x:bar /team-a/x:foo" {
		t.Errorf("list = %s", got)
	}
	if got := repoNames(e.ok(e.root, "GET", "/silo/v1/silos/team-a/repos?recursive=true", nil)); got != "/team-a:foo /team-a/x:bar /team-a/x:foo" {
		t.Errorf("recursive list = %s", got)
	}
	if got := repoNames(e.ok(e.root, "GET", "/silo/v1/self/repos?recursive=true&page_size=2", nil)); got != "/team-a:foo /team-b:foo" {
		t.Errorf("recursive list's first page = %s", got)
	}
	e.ok(a, "GET", "/silo/v1/silos/team-a/x/repos/foo", nil)
	e.fail(fenceapi.NotFound, b, "GET", "/silo/v1/silos/team-a/x/repos/foo", nil)
	e.fail(fenceapi.NotFound, b, "GET", "/silo/v1/silos/team-a/x/repos", nil)
	e.fail(fenceapi.NotFound, b, "POST", "/silo/v1/silos/team-a/x/repos", obj{"name": "evil"})
	e.fail(fenceapi.NotFound, x, "GET", "/silo/v1/silos/team-a/repos/foo", nil)
	e.fail(fenceapi.NotFound, x, "GET", "/silo/v1/self/repos/nope", nil)

	// The repos count: x has 2, both used.
	e.fail(fenceapi.QuotaExceeded, x, "POST", "/silo/v1/self/repos", obj{"name": "third"})
	d := e.ok(e.root, "GET", "/silo/v1/silos/team-a/x", nil)
	if got := str(get(d, "quota", "global", "repos", "used")); got != "2" {
		t.Errorf("x's repos used = %s", got)
	}
	if got := str(get(e.ok(e.root, "GET", "/silo/v1/self", nil), "quota", "global", "repos", "granted")); got == "0" || got == "<nil>" {
		t.Errorf("the root's repos = %s, want unlimited", got)
	}
	none := e.mk(e.root, "team-a", "none", obj{"quota": obj{"global": obj{"repos": 0}}})
	e.fail(fenceapi.QuotaExceeded, none, "POST", "/silo/v1/self/repos", obj{"name": "foo"})

	// PATCH moves HEAD.
	p := e.ok(x, "PATCH", "/silo/v1/self/repos/foo", obj{"default_branch": "trunk"})
	if p["default_branch"] != "trunk" {
		t.Errorf("patch: %v", p)
	}
	if head, _ := os.ReadFile(filepath.Join(dir, "HEAD")); string(head) != "ref: refs/heads/trunk\n" {
		t.Errorf("HEAD after patch: %q", head)
	}
	e.fail(fenceapi.InvalidArgument, x, "PATCH", "/silo/v1/self/repos/foo", obj{"default_branch": "-x"})
	e.fail(fenceapi.NotFound, x, "PATCH", "/silo/v1/self/repos/nope", obj{"default_branch": "x"})

	// DELETE is an operation, done at once with no mirror; the count
	// comes back.
	op := e.ok(x, "DELETE", "/silo/v1/self/repos/foo", nil)
	if op["kind"] != "repo.delete" || op["done"] != true || op["state"] != fenceapi.OpDone || op["target"] != "/team-a/x/foo" {
		t.Errorf("delete: %v", op)
	}
	e.ok(a, "GET", "/silo/v1/"+op["name"].(string), nil)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the repo's directory after delete: %v", err)
	}
	e.fail(fenceapi.NotFound, x, "GET", "/silo/v1/self/repos/foo", nil)
	e.fail(fenceapi.NotFound, x, "DELETE", "/silo/v1/self/repos/foo", nil)
	e.ok(x, "POST", "/silo/v1/self/repos", obj{"name": "third"})

	if l := okRecords(e.records(e.root, "team-a/x", "repo.create")); len(l) != 3 {
		t.Errorf("%d repo.create records, want 3", len(l))
	}
	if l := okRecords(e.records(e.root, "team-a/x", "repo.delete")); len(l) != 1 || get(l[0], "fields", "repo") != "foo" {
		t.Errorf("repo.delete records: %v", l)
	}

	// A silo deleted with force takes its repos, files and all.
	bar := e.ok(x, "GET", "/silo/v1/self/repos/bar", nil)["id"].(string)
	e.fail(fenceapi.FailedPrecondition, e.root, "DELETE", "/silo/v1/silos/team-a/x", nil)
	op = e.ok(e.root, "DELETE", "/silo/v1/silos/team-a/x?force=true", nil)
	if op = e.waitOp(e.root, op["name"].(string)); op["state"] != fenceapi.OpDone {
		t.Fatalf("delete x: %v", op)
	}
	if _, err := os.Stat(filepath.Join(silos, x, "repos", bar+".git")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("x's repo after x's delete: %v", err)
	}
	if got := str(get(e.ok(e.root, "GET", "/silo/v1/silos/team-a", nil), "quota", "global", "repos", "delegated")); got != "0" {
		t.Errorf("team-a's repos delegated after x went = %s", got)
	}
}

func okRecords(l []obj) []obj {
	var out []obj
	for _, r := range l {
		if r["result"] == "OK" {
			out = append(out, r)
		}
	}
	return out
}

func gitIn(t *testing.T, dir string, args ...string) string {
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

// A create's seed is a repo the caller may read, copied server-side:
// code moves down the tree and never up or sideways (local-git.md,
// "Seeding").
func TestRepoSeeding(t *testing.T) {
	e, silos := newRepoEnv(t)
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"quota": obj{"global": obj{"repos": 10}}})
	x := e.mk(a, "self", "x", obj{"quota": obj{"global": obj{"repos": 6}}})
	helper := e.mk(x, "self", "helper", obj{"quota": obj{"global": obj{"repos": 3}}})
	b := e.mk(e.root, "-"+e.root, "team-b", obj{"quota": obj{"global": obj{"repos": 3}}})

	foo := e.ok(x, "POST", "/silo/v1/self/repos", obj{"name": "foo", "default_branch": "trunk"})
	src := filepath.Join(silos, x, "repos", foo["id"].(string)+".git")
	work := t.TempDir()
	gitIn(t, work, "init", "--quiet", "--initial-branch=trunk", ".")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", "f")
	gitIn(t, work, "commit", "--quiet", "-m", "one")
	tip := gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "push", "--quiet", src, "trunk")

	// The parent seeds its child's repo from its own, by absolute path
	// or relative to itself.
	for name, seed := range map[string]string{"work": "/team-a/x/foo", "work2": "foo"} {
		w := e.ok(x, "POST", "/silo/v1/silos/team-a/x/helper/repos", obj{"name": name, "seed": obj{"repo": seed}})
		if w["default_branch"] != "trunk" {
			t.Errorf("%s's default_branch = %v, want the seed's", name, w["default_branch"])
		}
		dst := filepath.Join(silos, helper, "repos", w["id"].(string)+".git")
		if got := gitIn(t, dst, "rev-parse", "refs/heads/trunk"); got != tip {
			t.Errorf("%s's trunk = %s, want %s", name, got, tip)
		}
	}
	// So does the grandparent, from a descendant's repo, with its own
	// branch.
	w := e.ok(a, "POST", "/silo/v1/silos/team-a/x/helper/repos", obj{"name": "work3", "default_branch": "dev", "seed": obj{"repo": "x/foo"}})
	if w["default_branch"] != "dev" {
		t.Errorf("work3's default_branch = %v", w["default_branch"])
	}

	// The child can't seed from its parent's repos, nor a sibling from
	// another's; neither learns whether the repo exists.
	e.fail(fenceapi.NotFound, helper, "POST", "/silo/v1/self/repos", obj{"name": "steal", "seed": obj{"repo": "/team-a/x/foo"}})
	e.fail(fenceapi.NotFound, b, "POST", "/silo/v1/self/repos", obj{"name": "steal", "seed": obj{"repo": "/team-a/x/foo"}})
	e.fail(fenceapi.NotFound, b, "POST", "/silo/v1/self/repos", obj{"name": "steal", "seed": obj{"repo": "/team-a/x/nope"}})
	e.fail(fenceapi.NotFound, x, "POST", "/silo/v1/self/repos", obj{"name": "steal", "seed": obj{"repo": "nope"}})
	e.fail(fenceapi.InvalidArgument, x, "POST", "/silo/v1/self/repos", obj{"name": "s", "seed": obj{"repo": "/team-a/x/Foo"}})
	if got := repoNames(e.ok(b, "GET", "/silo/v1/self/repos", nil)); got != "" {
		t.Errorf("team-b's repos = %s", got)
	}
}

// repos.mirror is never looser than the parent's (local-git.md, call
// L54), and a repo's mirror state shows it.
func TestReposMirrorPolicy(t *testing.T) {
	e, _ := newRepoEnv(t)
	e.want(e.root, "-"+e.root, map[string]string{
		"repos.own.mirror":       "true",
		"repos.effective.mirror": "true",
		"repos.effective_from":   "/",
	})
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"repos": obj{"mirror": false}, "quota": obj{"global": obj{"repos": 5}}})
	e.want(e.root, "team-a", map[string]string{
		"repos.own.mirror":       "false",
		"repos.effective.mirror": "false",
		"repos.effective_from":   "/team-a",
	})
	// A child takes its parent's effective value, and can't turn it on.
	x := e.mk(e.root, "team-a", "x", obj{"quota": obj{"global": obj{"repos": 2}}})
	e.want(e.root, "team-a/x", map[string]string{"repos.own.mirror": "false", "repos.effective_from": "/team-a"})
	er := e.fail(fenceapi.LooserThanParent, e.root, "POST", "/silo/v1/silos/team-a/children",
		obj{"name": "y", "address": obj{"prefix_length": 24}, "repos": obj{"mirror": true}})
	if get(er, "details", "policy") != "repos" {
		t.Errorf("refusal: %v", er)
	}
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", "/silo/v1/silos/team-a/x", obj{"repos": obj{"mirror": true}})
	r := e.ok(x, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
	if get(r, "mirror", "state") != "OFF" || get(r, "mirror", "from") != "/team-a" || get(r, "mirror", "external_prefix") != nil {
		t.Errorf("mirror off: %v", r["mirror"])
	}

	// The ancestor turns it back on; x's own false stays, and x may turn
	// itself off but not on.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"repos": obj{"mirror": true}})
	e.want(e.root, "team-a/x", map[string]string{"repos.effective.mirror": "false", "repos.effective_from": "/team-a/x"})
	e.fail(fenceapi.PermissionDenied, x, "PATCH", "/silo/v1/self", obj{"repos": obj{"mirror": true}})
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a/x", obj{"repos": obj{"mirror": true}})
	if got := e.ok(x, "GET", "/silo/v1/self/repos/foo", nil)["mirror"]; get(got.(obj), "state") != "UNCONFIGURED" {
		t.Errorf("mirror on again: %v", got)
	}
	e.ok(a, "PATCH", "/silo/v1/silos/team-a/x", obj{"repos": obj{"mirror": false}})
	e.ok(x, "PATCH", "/silo/v1/self", obj{"repos": obj{"mirror": false}})

	// The root may turn its own off, and on again.
	e.ok(e.root, "PATCH", "/silo/v1/self", obj{"repos": obj{"mirror": false}})
	e.want(e.root, "team-a", map[string]string{"repos.effective.mirror": "false", "repos.effective_from": "/"})
	e.ok(e.root, "PATCH", "/silo/v1/self", obj{"repos": obj{"mirror": true}})
	e.want(e.root, "team-a", map[string]string{"repos.effective.mirror": "true"})
}

// A manage grant's repos resource type gives its holder the target's
// repos at its access, and sealed and read silos keep only what their
// control policy allows (local-git.md, §4).
func TestReposThroughGrantsAndControl(t *testing.T) {
	e, _ := newRepoEnv(t)
	prod := e.mk(e.root, "-"+e.root, "prod", obj{"quota": obj{"global": obj{"repos": 5}}})
	e.mk(e.root, "prod", "web", obj{"quota": obj{"global": obj{"repos": 2}}})
	deploy := e.mk(e.root, "-"+e.root, "deploy", obj{"quota": obj{"global": obj{"repos": 2}}})
	e.ok(prod, "POST", "/silo/v1/self/repos", obj{"name": "app"})
	e.ok(e.root, "POST", "/silo/v1/silos/prod/web/repos", obj{"name": "site"})

	grant := func(name, access string, resources ...string) {
		g := e.ok(e.root, "POST", "/silo/v1/silos/prod/manageGrants", obj{"name": name, "holder": "/deploy",
			"scope": obj{"facade": obj{"resources": resources, "access": access}, "fence": []string{"read"}},
			"lease": obj{"ttl": "3600s"}})
		e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	}
	grant("pipeline", "read", "repos")
	e.ok(deploy, "GET", "/silo/v1/silos/prod/repos/app", nil)
	if got := repoNames(e.ok(deploy, "GET", "/silo/v1/silos/prod/repos?recursive=true", nil)); got != "/prod:app /prod/web:site" {
		t.Errorf("through the grant: %s", got)
	}
	er := e.fail(fenceapi.PermissionDenied, deploy, "POST", "/silo/v1/silos/prod/repos", obj{"name": "new"})
	if get(er, "details", "grant") != "pipeline" {
		t.Errorf("refusal: %v", er)
	}
	e.fail(fenceapi.PermissionDenied, deploy, "DELETE", "/silo/v1/silos/prod/repos/app", nil)
	// The holder seeds its own repo from what it can read.
	e.ok(deploy, "POST", "/silo/v1/self/repos", obj{"name": "app", "seed": obj{"repo": "/prod/app"}})
	e.ok(e.root, "DELETE", "/silo/v1/silos/prod/manageGrants/pipeline", nil)
	e.fail(fenceapi.NotFound, deploy, "GET", "/silo/v1/silos/prod/repos/app", nil)

	// A grant without repos doesn't reach them.
	grant("vms", "write", "instances")
	e.fail(fenceapi.PermissionDenied, deploy, "GET", "/silo/v1/silos/prod/repos", nil)
	e.ok(e.root, "DELETE", "/silo/v1/silos/prod/manageGrants/vms", nil)

	// Write creates and deletes, and seeds the target from the holder's
	// own repo; it is audited under the target, via the grant.
	grant("rollout", "write", "*")
	e.ok(deploy, "POST", "/silo/v1/silos/prod/web/repos", obj{"name": "deployed", "seed": obj{"repo": "/deploy/app"}})
	e.ok(deploy, "DELETE", "/silo/v1/silos/prod/web/repos/site", nil)
	l := okRecords(e.records(e.root, "prod/web", "repo.create"))
	if len(l) != 2 || !strings.HasPrefix(str(l[len(l)-1]["via"]), "manage:") {
		t.Errorf("repo.create records under web: %v", l)
	}

	// A sealed silo is refused every repo call; a read one may list and
	// get.
	sealed := e.mk(e.root, "prod", "sealed", obj{"control": "none", "quota": obj{"global": obj{"repos": 1}}})
	e.ok(e.root, "POST", "/silo/v1/silos/prod/sealed/repos", obj{"name": "code"})
	e.fail(fenceapi.ControlDenied, sealed, "POST", "/silo/v1/self/repos", obj{"name": "more"})
	e.fail(fenceapi.ControlDenied, sealed, "GET", "/silo/v1/self/repos", nil)
	reader := e.mk(e.root, "prod", "reader", obj{"control": "read", "quota": obj{"global": obj{"repos": 1}}})
	e.ok(e.root, "POST", "/silo/v1/silos/prod/reader/repos", obj{"name": "code"})
	e.ok(reader, "GET", "/silo/v1/self/repos", nil)
	e.ok(reader, "GET", "/silo/v1/self/repos/code", nil)
	e.fail(fenceapi.ControlDenied, reader, "DELETE", "/silo/v1/self/repos/code", nil)
	e.fail(fenceapi.ControlDenied, reader, "PATCH", "/silo/v1/self/repos/code", obj{"default_branch": "x"})
}

// Without a place to keep them, silod refuses to make repos.
func TestReposWithoutAHost(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	e.fail(fenceapi.FailedPrecondition, e.root, "POST", "/silo/v1/self/repos", obj{"name": "foo"})
	if got := repoNames(e.ok(e.root, "GET", "/silo/v1/self/repos", nil)); got != "" {
		t.Errorf("repos = %s", got)
	}
}
