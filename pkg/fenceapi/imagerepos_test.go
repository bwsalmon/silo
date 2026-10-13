package fenceapi_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

func sha(c byte) string { return "sha256:" + strings.Repeat(string([]byte{c}), 64) }

// push does what the registry does for a push by caller into silo's
// repo name: the first push creates the repo, then the manifest and
// its tag are recorded. It returns CreateImageRepo's error, if any.
func (e *env) push(caller, silo, name, tag string, m *imagerepos.Manifest) error {
	e.t.Helper()
	return e.db.Tx(ctx, func(tx store.Tx) error {
		chain, err := tree.Ancestry(ctx, tx, silo)
		if err != nil {
			return err
		}
		r, err := imagerepos.ByName(ctx, tx, silo, name)
		if errors.Is(err, imagerepos.ErrNotFound) {
			r, err = fenceapi.CreateImageRepo(ctx, tx, chain, fenceapi.Caller{Silo: caller, Via: fenceapi.ViaOrigin}, name)
		}
		if err != nil {
			return err
		}
		mm := *m
		mm.Repo = r.ID
		if err := imagerepos.PutManifest(ctx, tx, &mm); err != nil {
			return err
		}
		return imagerepos.SetTag(ctx, tx, r.ID, tag, m.Digest)
	})
}

func image(d byte, layers ...byte) *imagerepos.Manifest {
	m := &imagerepos.Manifest{Digest: sha(d), MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 100}
	for _, l := range layers {
		m.Refs = append(m.Refs, imagerepos.Blob{Digest: sha(l), Size: 1000})
	}
	return m
}

func imageRepoNames(l obj) string {
	var out []string
	for _, r := range l["image_repos"].([]any) {
		out = append(out, r.(obj)["silo"].(string)+":"+r.(obj)["name"].(string))
	}
	return strings.Join(out, " ")
}

// Image repos are silo objects created by their first push
// (local-registry.md, §3), listed, read and deleted through the fence
// API, scoped per silo, and counted in the image_repos class. Who may
// see them is git's table (call L63): the silo and its ancestors, and
// NOT_FOUND for everyone else, whether the repo exists or not.
func TestImageRepos(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"quota": obj{"global": obj{"image_repos": 3}}})
	x := e.mk(e.root, "team-a", "x", obj{"quota": obj{"global": obj{"image_repos": 2}}})
	b := e.mk(e.root, "-"+e.root, "team-b", obj{"quota": obj{"global": obj{"image_repos": 1}}})

	if err := e.push(x, x, "app", "1", image('1', 'a', 'b')); err != nil {
		t.Fatal(err)
	}
	if err := e.push(x, x, "app", "2", image('2', 'a', 'c')); err != nil {
		t.Fatal(err)
	}
	r := e.ok(x, "GET", "/silo/v1/self/imageRepos/app", nil)
	for path, want := range map[string]string{
		"name":          "app",
		"silo":          "/team-a/x",
		"state":         "READY",
		"ref":           "registry.silo.internal/team-a/x/app",
		"ref_by_id":     "registry.silo.internal/id_" + x + "/app",
		"tags":          "2",
		"manifests":     "2",
		"charged_bytes": "3200", // a once, b, c, and two manifests
	} {
		if got := str(get(r, path)); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	if id := r["id"].(string); !strings.HasPrefix(id, "ir-") || len(id) != 9 {
		t.Errorf("id = %s", id)
	}

	// Names are per silo; a second push to the same name is the same
	// repo, and an invalid name is refused.
	if err := e.push(b, b, "app", "1", image('1', 'a', 'b')); err != nil {
		t.Fatal(err)
	}
	if err := e.push(x, x, "a.b", "1", image('1')); err == nil {
		t.Error("a repo name with a dot was created")
	}

	// The silo and its ancestors list and read; a sibling and a
	// descendant get NOT_FOUND, the same for a repo that exists and one
	// that doesn't.
	if err := e.push(a, a, "base", "1", image('3', 'd')); err != nil {
		t.Fatal(err)
	}
	if got := imageRepoNames(e.ok(a, "GET", "/silo/v1/silos/team-a/x/imageRepos", nil)); got != "/team-a/x:app" {
		t.Errorf("list = %s", got)
	}
	if got := imageRepoNames(e.ok(e.root, "GET", "/silo/v1/silos/team-a/imageRepos?recursive=true", nil)); got != "/team-a:base /team-a/x:app" {
		t.Errorf("recursive list = %s", got)
	}
	e.ok(a, "GET", "/silo/v1/silos/team-a/x/imageRepos/app", nil)
	e.ok(e.root, "GET", "/silo/v1/silos/-"+x+"/imageRepos/app/tags", nil)
	for _, p := range []string{"/silo/v1/silos/team-a/x/imageRepos/app", "/silo/v1/silos/team-a/x/imageRepos/nope",
		"/silo/v1/silos/-" + x + "/imageRepos/app", "/silo/v1/silos/-" + x + "/imageRepos/nope",
		"/silo/v1/silos/team-a/x/imageRepos/app/tags", "/silo/v1/silos/team-a/x/imageRepos"} {
		e.fail(fenceapi.NotFound, b, "GET", p, nil)
	}
	e.fail(fenceapi.NotFound, b, "DELETE", "/silo/v1/silos/team-a/x/imageRepos/app", nil)
	e.fail(fenceapi.NotFound, b, "DELETE", "/silo/v1/silos/team-a/x/imageRepos/app/tags/1", nil)
	e.fail(fenceapi.NotFound, x, "GET", "/silo/v1/silos/team-a/imageRepos/base", nil)
	e.fail(fenceapi.NotFound, x, "GET", "/silo/v1/self/imageRepos/nope", nil)

	// Tags: what each points at, and every manifest with its tags.
	tags := e.ok(x, "GET", "/silo/v1/self/imageRepos/app/tags", nil)
	if l := tags["tags"].([]any); len(l) != 2 || get(l[0].(obj), "digest") != sha('1') {
		t.Errorf("tags = %v", tags["tags"])
	}
	if l := tags["manifests"].([]any); len(l) != 2 || str(get(l[0].(obj), "charged_bytes")) != "2100" {
		t.Errorf("manifests = %v", tags["manifests"])
	}

	// Untag keeps the manifest and its charge; deleting it refunds what
	// no other manifest references.
	u := e.ok(x, "DELETE", "/silo/v1/self/imageRepos/app/tags/1", nil)
	if str(u["tags"]) != "1" || str(u["manifests"]) != "2" || str(u["charged_bytes"]) != "3200" {
		t.Errorf("after untag: %v", u)
	}
	e.fail(fenceapi.NotFound, x, "DELETE", "/silo/v1/self/imageRepos/app/tags/1", nil)
	e.fail(fenceapi.InvalidArgument, x, "DELETE", "/silo/v1/self/imageRepos/app/manifests/sha256:nope", nil)
	d := e.ok(a, "DELETE", "/silo/v1/silos/team-a/x/imageRepos/app/manifests/"+sha('1'), nil)
	if str(d["manifests"]) != "1" || str(d["charged_bytes"]) != "2100" {
		t.Errorf("after the manifest delete: %v", d)
	}
	l := okRecords(e.records(e.root, "team-a/x", fenceapi.KindRegistryDelete))
	if len(l) != 2 || str(get(l[len(l)-1], "fields", "refunded_bytes")) != "1100" {
		t.Errorf("registry.delete records: %v", l)
	}

	// The image_repos count: x has 2.
	if err := e.push(x, x, "tools", "1", image('4')); err != nil {
		t.Fatal(err)
	}
	if err := e.push(x, x, "third", "1", image('5')); ledger.CodeOf(err) != ledger.QuotaExceeded || !strings.Contains(err.Error(), "image_repos") {
		t.Errorf("a third repo in x: %v", err)
	}
	if got := str(get(e.ok(e.root, "GET", "/silo/v1/silos/team-a/x", nil), "quota", "global", "image_repos", "used")); got != "2" {
		t.Errorf("x's image_repos used = %s", got)
	}
	if got := str(get(e.ok(e.root, "GET", "/silo/v1/self", nil), "quota", "global", "image_repos", "granted")); got == "0" || got == "<nil>" {
		t.Errorf("the root's image_repos = %s, want unlimited", got)
	}

	// Deleting a repo is an operation, done at once; the count comes
	// back, and the audit has the bytes it charged.
	op := e.ok(x, "DELETE", "/silo/v1/self/imageRepos/tools", nil)
	if op["kind"] != fenceapi.KindRegistryDelete || op["done"] != true || op["target"] != "/team-a/x/tools" {
		t.Errorf("delete: %v", op)
	}
	e.fail(fenceapi.NotFound, x, "GET", "/silo/v1/self/imageRepos/tools", nil)
	if err := e.push(x, x, "third", "1", image('5')); err != nil {
		t.Errorf("a repo after one was deleted: %v", err)
	}

	// A silo with image repos is deleted only with force, which takes
	// them and their charges.
	er := e.fail(fenceapi.FailedPrecondition, e.root, "DELETE", "/silo/v1/silos/team-a/x", nil)
	if !strings.Contains(str(er["message"]), "2 image repos") {
		t.Errorf("delete without force: %v", er)
	}
	op = e.ok(e.root, "DELETE", "/silo/v1/silos/team-a/x?force=true", nil)
	if op = e.waitOp(e.root, op["name"].(string)); op["state"] != fenceapi.OpDone {
		t.Fatalf("delete x: %v", op)
	}
	if got := imageRepoNames(e.ok(e.root, "GET", "/silo/v1/self/imageRepos?recursive=true", nil)); got != "/team-a:base /team-b:app" {
		t.Errorf("repos after x's delete: %s", got)
	}
	_ = e.db.Tx(ctx, func(tx store.Tx) error {
		if n, err := imagerepos.Count(ctx, tx, x); err != nil || n != 0 {
			t.Errorf("x's image repos after its delete: %d, %v", n, err)
		}
		return nil
	})
}

// A manage grant's registry resource type gives its holder the
// target's image repos at its access, and a sealed or read silo can't
// create a repo by pushing, a control-plane call, or delete one through
// the fence API (local-registry.md, calls L63 and L65).
func TestImageReposThroughGrantsAndControl(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	prod := e.mk(e.root, "-"+e.root, "prod", obj{"quota": obj{"global": obj{"image_repos": 10}}})
	web := e.mk(e.root, "prod", "web", obj{"quota": obj{"global": obj{"image_repos": 2}}})
	deploy := e.mk(e.root, "-"+e.root, "deploy", obj{"quota": obj{"global": obj{"image_repos": 2}}})
	if err := e.push(prod, prod, "app", "1", image('1', 'a')); err != nil {
		t.Fatal(err)
	}
	if err := e.push(web, web, "site", "1", image('2', 'b')); err != nil {
		t.Fatal(err)
	}

	grant := func(name, access string, resources ...string) {
		g := e.ok(e.root, "POST", "/silo/v1/silos/prod/manageGrants", obj{"name": name, "holder": "/deploy",
			"scope": obj{"facade": obj{"resources": resources, "access": access}, "fence": []string{"read"}},
			"lease": obj{"ttl": "3600s"}})
		e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	}
	grant("pull", "read", "registry")
	e.ok(deploy, "GET", "/silo/v1/silos/prod/imageRepos/app/tags", nil)
	if got := imageRepoNames(e.ok(deploy, "GET", "/silo/v1/silos/prod/imageRepos?recursive=true", nil)); got != "/prod:app /prod/web:site" {
		t.Errorf("through the grant: %s", got)
	}
	er := e.fail(fenceapi.PermissionDenied, deploy, "DELETE", "/silo/v1/silos/prod/imageRepos/app/tags/1", nil)
	if get(er, "details", "grant") != "pull" {
		t.Errorf("refusal: %v", er)
	}
	e.fail(fenceapi.PermissionDenied, deploy, "DELETE", "/silo/v1/silos/prod/imageRepos/app", nil)
	e.ok(e.root, "DELETE", "/silo/v1/silos/prod/manageGrants/pull", nil)
	e.fail(fenceapi.NotFound, deploy, "GET", "/silo/v1/silos/prod/imageRepos/app", nil)

	// A grant on git repos, or on instances, doesn't reach image repos.
	grant("code", "write", "repos", "instances")
	e.fail(fenceapi.PermissionDenied, deploy, "GET", "/silo/v1/silos/prod/imageRepos", nil)
	e.ok(e.root, "DELETE", "/silo/v1/silos/prod/manageGrants/code", nil)

	// Write untags and deletes, audited under the target via the grant.
	grant("rollout", "write", "*")
	e.ok(deploy, "DELETE", "/silo/v1/silos/prod/web/imageRepos/site/tags/1", nil)
	e.ok(deploy, "DELETE", "/silo/v1/silos/prod/web/imageRepos/site", nil)
	l := okRecords(e.records(e.root, "prod/web", fenceapi.KindRegistryDelete))
	if len(l) != 2 || !strings.HasPrefix(str(l[len(l)-1]["via"]), "manage:") {
		t.Errorf("registry.delete records under web: %v", l)
	}

	// A sealed silo can't create a repo by pushing, keeps pushing to
	// one it has, and is refused every fence call; a read one lists and
	// gets, and can't untag or delete through the fence API.
	sealed := e.mk(e.root, "prod", "sealed", obj{"control": "none", "quota": obj{"global": obj{"image_repos": 2}}})
	if err := e.push(e.root, sealed, "code", "1", image('3')); err != nil {
		t.Fatal(err)
	}
	var pe *fenceapi.Error
	if err := e.push(sealed, sealed, "more", "1", image('4')); !errors.As(err, &pe) || pe.Code != fenceapi.PermissionDenied ||
		!strings.Contains(pe.Message, "control-plane call") {
		t.Errorf("a sealed silo's first push: %v", err)
	}
	if err := e.push(sealed, sealed, "code", "2", image('4')); err != nil {
		t.Errorf("a sealed silo's push to its own repo: %v", err)
	}
	e.fail(fenceapi.ControlDenied, sealed, "GET", "/silo/v1/self/imageRepos", nil)
	e.fail(fenceapi.ControlDenied, sealed, "DELETE", "/silo/v1/self/imageRepos/code", nil)
	reader := e.mk(e.root, "prod", "reader", obj{"control": "read", "quota": obj{"global": obj{"image_repos": 1}}})
	if err := e.push(reader, reader, "code", "1", image('5')); !errors.As(err, &pe) || pe.Code != fenceapi.PermissionDenied {
		t.Errorf("a read silo's first push: %v", err)
	}
	if err := e.push(e.root, reader, "code", "1", image('5')); err != nil {
		t.Fatal(err)
	}
	e.ok(reader, "GET", "/silo/v1/self/imageRepos", nil)
	e.ok(reader, "GET", "/silo/v1/self/imageRepos/code/tags", nil)
	e.fail(fenceapi.ControlDenied, reader, "DELETE", "/silo/v1/self/imageRepos/code/tags/1", nil)
	e.fail(fenceapi.ControlDenied, reader, "DELETE", "/silo/v1/self/imageRepos/code", nil)
}
