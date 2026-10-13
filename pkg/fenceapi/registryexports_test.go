package fenceapi_test

import (
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
)

// mayRead asks what the registry will: may silo pull repoSilo's image
// repo name through an accepted import?
func (e *env) mayRead(silo, repoSilo, name string) bool {
	e.t.Helper()
	var ok bool
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		r, err := imagerepos.ByName(ctx, tx, repoSilo, name)
		if err != nil {
			return err
		}
		ok, err = fenceapi.MayReadImported(ctx, tx, silo, r)
		return err
	}); err != nil {
		e.t.Fatalf("may %s read %s's %s: %v", silo, repoSilo, name, err)
	}
	return ok
}

// imported is what silo's registry catalog lists of its imports, by
// silo id and name.
func (e *env) imported(silo string) string {
	e.t.Helper()
	var out []string
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		rs, err := fenceapi.ImportedImageRepos(ctx, tx, silo)
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return strings.Join(out, " ")
}

// TestRegistryExports is local-registry.md §12's export cases that need
// no registry service (call L64): offered but not accepted gives
// nothing; accepted gives read and never write; a prefix covers a repo
// created after the accept and never a descendant's; grant-policy
// suspension; withdrawal by either side; and an export on a
// descendant's behalf.
func TestRegistryExports(t *testing.T) {
	allow := obj{"grant_policy": obj{"exports": "allow"}}
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"grant_policy": obj{"exports": "allow"}, "quota": obj{"global": obj{"image_repos": 6}}})
	x := e.mk(e.root, "team-a", "x", obj{"grant_policy": obj{"exports": "allow"}, "quota": obj{"global": obj{"image_repos": 1}}})
	b := e.mk(e.root, "-"+e.root, "team-b", allow)
	c := e.mk(e.root, "-"+e.root, "team-c", allow)
	for _, p := range []struct{ silo, name string }{{a, "builder"}, {a, "base-alpine"}, {a, "app"}, {x, "base-x"}} {
		if err := e.push(p.silo, p.silo, p.name, "1", image('1', 'a')); err != nil {
			t.Fatal(err)
		}
	}

	// Checks: repos is required, and only for registry exports, which
	// have no target.
	for _, body := range []obj{
		{"name": "imgs", "protocol": "registry", "to": []string{"/team-b"}},
		{"name": "imgs", "protocol": "registry", "repos": []string{}, "to": []string{"/team-b"}},
		{"name": "imgs", "protocol": "registry", "repos": []string{"Builder"}, "to": []string{"/team-b"}},
		{"name": "imgs", "protocol": "registry", "repos": []string{"a*b*"}, "to": []string{"/team-b"}},
		{"name": "imgs", "protocol": "registry", "repos": []string{"id_*"}, "to": []string{"/team-b"}},
		{"name": "imgs", "protocol": "registry", "repos": []string{"builder"}, "target": "10.0.0.1:80", "to": []string{"/team-b"}},
		{"name": "imgs", "protocol": "registry", "repos": []string{"builder"}, "proxy_protocol": true, "to": []string{"/team-b"}},
		{"name": "imgs", "protocol": "http", "repos": []string{"builder"}, "target": "10.0.0.1:80", "to": []string{"/team-b"}},
	} {
		e.fail(fenceapi.InvalidArgument, a, "POST", "/silo/v1/self/exports", body)
	}

	ex := e.ok(a, "POST", "/silo/v1/self/exports", obj{"name": "base-images", "protocol": "registry",
		"repos": []string{"builder", "base-*", "builder"}, "to": []string{"/team-b", "/team-c"}})
	if str(ex["repos"]) != "[base-* builder]" || ex["target"] != nil {
		t.Errorf("export: %v", ex)
	}
	// The same export again is the same offer; other repos are another
	// export.
	e.ok(a, "POST", "/silo/v1/self/exports", obj{"name": "base-images", "protocol": "registry", "repos": []string{"base-*", "builder"}, "to": []string{"/team-b"}})
	e.fail(fenceapi.AlreadyExists, a, "POST", "/silo/v1/self/exports", obj{"name": "base-images", "protocol": "registry", "repos": []string{"app"}, "to": []string{"/team-b"}})

	// Offered but not accepted gives nothing.
	if offers := e.ok(b, "GET", "/silo/v1/self/offers", nil)["offers"].([]any); len(offers) != 1 || offers[0].(obj)["protocol"] != "registry" {
		t.Errorf("offers: %v", offers)
	}
	if e.mayRead(b, a, "builder") || e.imported(b) != "" {
		t.Error("an offer not accepted reads")
	}

	// Accepted: the exporter's own names, and read.
	im := e.ok(b, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/base-images"})
	if str(im["repos"]) != "[registry.silo.internal/team-a/base-* registry.silo.internal/team-a/builder]" || im["state"] != "ACTIVE" || im["address"] != nil {
		t.Errorf("import: %v", im)
	}
	for name, want := range map[string]bool{"builder": true, "base-alpine": true, "app": false} {
		if got := e.mayRead(b, a, name); got != want {
			t.Errorf("team-b reads team-a's %s: %v, want %v", name, got, want)
		}
	}
	// A prefix never covers a descendant's repos, and team-c hasn't
	// accepted.
	if e.mayRead(b, x, "base-x") || e.mayRead(c, a, "builder") {
		t.Error("read beyond the accepted import")
	}
	// Never write: an import gives nothing on the fence API's repo calls.
	e.fail(fenceapi.NotFound, b, "DELETE", "/silo/v1/silos/team-a/imageRepos/builder", nil)
	e.fail(fenceapi.NotFound, b, "DELETE", "/silo/v1/silos/team-a/imageRepos/builder/tags/1", nil)
	e.fail(fenceapi.NotFound, b, "GET", "/silo/v1/silos/team-a/imageRepos", nil)

	// A prefix covers a repo created after the accept.
	if err := e.push(a, a, "base-debian", "1", image('2', 'b')); err != nil {
		t.Fatal(err)
	}
	if !e.mayRead(b, a, "base-debian") {
		t.Error("base-debian, created after the accept, isn't covered")
	}
	if got := e.imported(b); got != "base-alpine base-debian builder" {
		t.Errorf("catalog of imports = %q", got)
	}

	// Grant policy: the exporter's effective policy stops allowing
	// exports, so the grant is suspended and reads nothing; allowing them
	// again resumes it.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"grant_policy": obj{"exports": "deny"}})
	if l := e.ok(b, "GET", "/silo/v1/self/imports", nil)["imports"].([]any); len(l) != 1 || l[0].(obj)["state"] != "SUSPENDED" {
		t.Errorf("imports after deny: %v", l)
	}
	if e.mayRead(b, a, "builder") || e.imported(b) != "" {
		t.Error("a suspended import reads")
	}
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"grant_policy": obj{"exports": "allow"}})
	if !e.mayRead(b, a, "builder") {
		t.Error("a resumed import doesn't read")
	}
	// The importer's side suspends it too.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"grant_policy": obj{"exports": "deny"}})
	if e.mayRead(b, a, "builder") {
		t.Error("an import its importer's policy suspends reads")
	}
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-b", obj{"grant_policy": obj{"exports": "allow"}})

	// Withdrawn by the importer, then accepted again and withdrawn by the
	// exporter.
	e.ok(b, "DELETE", "/silo/v1/self/imports/"+im["id"].(string), nil)
	if e.mayRead(b, a, "builder") {
		t.Error("an import the importer gave up reads")
	}
	e.ok(b, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/base-images"})
	e.ok(a, "DELETE", "/silo/v1/self/exports/base-images/to/team-b", nil)
	if e.mayRead(b, a, "builder") {
		t.Error("an import the exporter withdrew reads")
	}

	// On a descendant's behalf: the ancestor exports x's own repos.
	e.ok(a, "POST", "/silo/v1/silos/team-a/x/exports", obj{"name": "x-images", "protocol": "registry", "repos": []string{"*"}, "to": []string{"/team-c"}})
	im = e.ok(c, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/x/x-images"})
	if str(im["repos"]) != "[registry.silo.internal/team-a/x/*]" {
		t.Errorf("import of x's: %v", im)
	}
	if !e.mayRead(c, x, "base-x") || e.mayRead(c, a, "builder") {
		t.Error("x's export reads wrongly")
	}
}

// images.sharing is never looser than the parent's (local-registry.md,
// call L73): isolated anywhere up the tree isolates the silo, a child
// can't set shared under it, and effective_from is the sharing domain.
func TestImagesSharing(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	e.want(e.root, "-"+e.root, map[string]string{
		"images.own.sharing":       "shared",
		"images.effective.sharing": "shared",
		"images.effective_from":    "/",
	})
	e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/silos/-"+e.root+"/children",
		obj{"name": "bad", "address": obj{"prefix_length": 16}, "images": obj{"sharing": "private"}})
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"images": obj{"sharing": "isolated"}})
	e.want(e.root, "team-a", map[string]string{
		"images.own.sharing":       "isolated",
		"images.effective.sharing": "isolated",
		"images.effective_from":    "/team-a",
	})
	// A child is in its parent's domain, and can't set shared.
	x := e.mk(e.root, "team-a", "x", nil)
	e.want(e.root, "team-a/x", map[string]string{
		"images.own.sharing":       "shared",
		"images.effective.sharing": "isolated",
		"images.effective_from":    "/team-a",
	})
	er := e.fail(fenceapi.LooserThanParent, e.root, "POST", "/silo/v1/silos/team-a/children",
		obj{"name": "y", "address": obj{"prefix_length": 24}, "images": obj{"sharing": "shared"}})
	if get(er, "details", "policy") != "images" {
		t.Errorf("refusal: %v", er)
	}
	e.fail(fenceapi.LooserThanParent, a, "PATCH", "/silo/v1/silos/team-a/x", obj{"images": obj{"sharing": "shared"}})

	// Nested isolation: x isolates itself and is its own domain, and
	// can't take it back; its ancestors can.
	e.ok(x, "PATCH", "/silo/v1/self", obj{"images": obj{"sharing": "isolated"}})
	e.want(e.root, "team-a/x", map[string]string{"images.effective.sharing": "isolated", "images.effective_from": "/team-a/x"})
	e.fail(fenceapi.PermissionDenied, x, "PATCH", "/silo/v1/self", obj{"images": obj{"sharing": "shared"}})
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"images": obj{"sharing": "shared"}})
	e.want(e.root, "team-a/x", map[string]string{"images.effective.sharing": "isolated", "images.effective_from": "/team-a/x"})
	e.ok(a, "PATCH", "/silo/v1/silos/team-a/x", obj{"images": obj{"sharing": "shared"}})
	e.want(e.root, "team-a/x", map[string]string{"images.effective.sharing": "shared", "images.effective_from": "/"})

	// The root may isolate the whole host, and share again.
	e.ok(e.root, "PATCH", "/silo/v1/self", obj{"images": obj{"sharing": "isolated"}})
	e.want(e.root, "team-a/x", map[string]string{"images.effective.sharing": "isolated", "images.effective_from": "/"})
	e.ok(e.root, "PATCH", "/silo/v1/self", obj{"images": obj{"sharing": "shared"}})
	e.want(e.root, "team-a/x", map[string]string{"images.effective.sharing": "shared", "images.effective_from": "/"})
}
