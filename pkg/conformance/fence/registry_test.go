package fence_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

// The backend-neutral registry and cache cases of local-registry.md
// §12, run against every target.

type obj = map[string]any

func each(t *testing.T, fn func(t *testing.T, r conformance.R)) {
	conformance.ForEach(t, targets.All(), func(t *testing.T, reg conformance.Registry) {
		fn(t, conformance.R{Registry: reg, T: t})
	})
}

// The GCP target reports the registry cases as not offered, and the
// installed local targets as not runnable here: neither is a pass.
func TestTargetsOffer(t *testing.T) {
	for _, tg := range targets.All() {
		_, err := tg.Registry(t)
		switch tg.Name() {
		case "gcp":
			if err != conformance.ErrNotOffered {
				t.Errorf("gcp: %v, want not offered", err)
			}
		case "local/fake":
			if err != nil {
				t.Errorf("local/fake: %v", err)
			}
		default:
			if _, ok := err.(*conformance.NotRunnable); !ok {
				t.Errorf("%s: %v, want not runnable", tg.Name(), err)
			}
		}
	}
}

// Access: a sibling gets 404 on pull, push, HEAD and tag list,
// identical for a repo that exists and one that doesn't, by path name
// and by id name. One silo can't read another's repo without an export.
func TestRegistrySiblingNotFound(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b := r.Mk("", "team-a", nil), r.Mk("", "team-b", nil)
		im := conformance.NewImage(`{"os":"linux"}`, "team-a's secret layer")
		r.Push(a, "team-a/app", "1", im)
		cases := []struct {
			method, path, ctype string
			body                []byte
		}{
			{"GET", "/manifests/1", "", nil},
			{"HEAD", "/manifests/1", "", nil},
			{"GET", "/manifests/" + im.ManifestD, "", nil},
			{"GET", "/blobs/" + im.LayerD, "", nil},
			{"HEAD", "/blobs/" + im.LayerD, "", nil},
			{"GET", "/tags/list", "", nil},
			{"POST", "/blobs/uploads/", "", nil},
			{"PUT", "/manifests/2", conformance.MediaOCIManifest, im.Manifest},
			{"DELETE", "/manifests/1", "", nil},
		}
		var first []string
		for i, name := range []string{"team-a/app", "id_" + a + "/app", "team-a/nope", "id_" + a + "/nope", "team-zzz/app"} {
			for j, c := range cases {
				res := r.Req(b, c.method, "/v2/"+name+c.path, c.ctype, c.body)
				if res.Code != 404 || (c.method != "HEAD" && res.ErrCode() != "NAME_UNKNOWN") {
					t.Errorf("%s %s%s as a sibling: %d %s", c.method, name, c.path, res.Code, res.Body)
				}
				got := fmt.Sprintf("%d %s", res.Code, res.Body)
				if i == 0 {
					first = append(first, got)
				} else if got != first[j] {
					t.Errorf("%s %s%s differs from an existing repo's answer: %q vs %q", c.method, name, c.path, got, first[j])
				}
			}
		}
		// Credentials name nobody.
		if res := r.ReqAuth(b, "/v2/team-a/app/manifests/1", a, "anything"); res.Code != 404 {
			t.Errorf("with a's name as credentials: %d", res.Code)
		}
		if res := r.Req(b, "GET", "/v2/_catalog", "", nil); strings.Contains(string(res.Body), "team-a") {
			t.Errorf("b's catalog: %s", res.Body)
		}
		if d := r.Last("registry.denied"); d["_silo"] != b || d["reason"] != "access" {
			t.Errorf("registry.denied: %v", d)
		}
	})
}

// Access: an ancestor pulls and can't push; a descendant can't read its
// parent's repos.
func TestRegistryAncestorAndDescendant(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a := r.Mk("", "team-a", nil)
		x := r.Mk("team-a", "x", nil)
		im := conformance.NewImage(`{"os":"linux"}`, "x's layer")
		r.Push(x, "team-a/x/app", "1", im)
		if res := r.Req(a, "GET", "/v2/team-a/x/app/blobs/"+im.LayerD, "", nil); res.Code != 200 {
			t.Fatalf("ancestor pull: %d %s", res.Code, res.Body)
		}
		for _, c := range []struct{ method, path string }{{"POST", "/v2/team-a/x/app/blobs/uploads/"},
			{"PUT", "/v2/team-a/x/app/manifests/2"}, {"DELETE", "/v2/team-a/x/app/manifests/1"}, {"POST", "/v2/team-a/x/new/blobs/uploads/"}} {
			if res := r.Req(a, c.method, c.path, conformance.MediaOCIManifest, im.Manifest); res.Code != 404 {
				t.Errorf("ancestor %s %s: %d %s", c.method, c.path, res.Code, res.Body)
			}
		}
		if p := r.Last("registry.pull"); p["_silo"] != x || p["_actor"] != a {
			t.Errorf("the ancestor's pull record: %v", p)
		}
		r.Push(a, "team-a/base", "1", conformance.NewImage(`{"os":"linux"}`, "a's layer"))
		for _, p := range []string{"/v2/team-a/base/manifests/1", "/v2/team-a/base/tags/list"} {
			if res := r.Req(x, "GET", p, "", nil); res.Code != 404 {
				t.Errorf("descendant GET %s: %d", p, res.Code)
			}
		}
	})
}

// No global answers: a silo that pushed blob B can't learn that another
// silo has B, by HEAD, by mount without from, by mount with a from it
// can't read, or by pushing a manifest naming it.
func TestRegistryNoGlobalAnswers(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b := r.Mk("", "team-a", nil), r.Mk("", "team-b", nil)
		blob := []byte("the blob both silos have")
		d := r.PushBlob(a, "team-a/app", blob)
		r.Push(a, "team-a/app", "1", conformance.NewImage(`{"os":"linux"}`, "a"))
		r.Push(b, "team-b/mine", "1", conformance.NewImage(`{"os":"linux"}`, "b"))
		if res := r.Req(b, "HEAD", "/v2/team-b/mine/blobs/"+d, "", nil); res.Code != 404 {
			t.Errorf("HEAD of a's blob in b's repo: %d", res.Code)
		}
		for _, q := range []string{"mount=" + d, "mount=" + d + "&from=team-a/app", "mount=" + d + "&from=id_" + a + "/app"} {
			if res := r.Req(b, "POST", "/v2/team-b/mine/blobs/uploads/?"+q, "", nil); res.Code != 202 || res.Header.Get("Location") == "" {
				t.Errorf("mount %s: %d %s, want an upload session", q, res.Code, res.Body)
			}
		}
		cfg := r.PushBlob(b, "team-b/mine", []byte("{}"))
		m, _ := json.Marshal(obj{"schemaVersion": 2, "mediaType": conformance.MediaOCIManifest,
			"config": obj{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfg, "size": 2},
			"layers": []obj{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": d, "size": len(blob)}}})
		if res := r.Req(b, "PUT", "/v2/team-b/mine/manifests/2", conformance.MediaOCIManifest, m); res.Code != 400 || res.ErrCode() != "MANIFEST_BLOB_UNKNOWN" {
			t.Errorf("a manifest naming a's blob: %d %s", res.Code, res.Body)
		}
		// Pushing B uploads it in full, and then it is b's.
		r.PushBlob(b, "team-b/mine", blob)
		if res := r.Req(b, "HEAD", "/v2/team-b/mine/blobs/"+d, "", nil); res.Code != 200 {
			t.Errorf("HEAD after b uploaded it: %d", res.Code)
		}
	})
}

// Exports: offered but not accepted gives nothing; accepted gives pull
// and never push; a prefix covers a repo created after the accept;
// grant-policy suspension and withdrawal close it.
func TestRegistryExports(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a, b := r.Mk("", "team-a", nil), r.Mk("", "team-b", nil)
		r.Push(a, "team-a/base-go", "1", conformance.NewImage(`{"os":"linux"}`, "go"))
		r.Push(a, "team-a/secret", "1", conformance.NewImage(`{"os":"linux"}`, "s"))
		pull := func(name string) int {
			t.Helper()
			return r.Req(b, "GET", "/v2/"+name+"/manifests/1", "", nil).Code
		}
		r.OK(a, "POST", "/silo/v1/self/exports", obj{"name": "imgs", "protocol": "registry", "repos": []string{"base-*"}, "to": []string{"/team-b"}})
		if c := pull("team-a/base-go"); c != 404 {
			t.Fatalf("offered, not accepted: %d", c)
		}
		r.OK(b, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/imgs"})
		if c := pull("team-a/base-go"); c != 200 {
			t.Fatalf("accepted: %d", c)
		}
		if c := pull("team-a/secret"); c != 404 {
			t.Errorf("a repo the export doesn't cover: %d", c)
		}
		for _, c := range []struct{ method, path string }{{"POST", "/v2/team-a/base-go/blobs/uploads/"},
			{"PUT", "/v2/team-a/base-go/manifests/2"}, {"DELETE", "/v2/team-a/base-go/manifests/1"}, {"POST", "/v2/team-a/base-new/blobs/uploads/"}} {
			if res := r.Req(b, c.method, c.path, conformance.MediaOCIManifest, []byte("{}")); res.Code != 404 {
				t.Errorf("importer %s %s: %d %s", c.method, c.path, res.Code, res.Body)
			}
		}
		py := conformance.NewImage(`{"os":"linux"}`, "py")
		r.Push(a, "team-a/base-py", "1", py)
		if res := r.Req(b, "GET", "/v2/team-a/base-py/blobs/"+py.LayerD, "", nil); res.Code != 200 || string(res.Body) != "py" {
			t.Errorf("a repo created after the accept: %d %s", res.Code, res.Body)
		}
		r.OK(r.Root(), "PATCH", "/silo/v1/silos/team-a", obj{"grant_policy": obj{"exports": "deny"}})
		if c := pull("team-a/base-go"); c != 404 {
			t.Errorf("suspended by the exporter's grant policy: %d", c)
		}
		r.OK(r.Root(), "PATCH", "/silo/v1/silos/team-a", obj{"grant_policy": obj{"exports": "allow"}})
		if c := pull("team-a/base-go"); c != 200 {
			t.Errorf("resumed: %d", c)
		}
		r.OK(a, "DELETE", "/silo/v1/self/exports/imgs", nil)
		if c := pull("team-a/base-go"); c != 404 {
			t.Errorf("withdrawn: %d", c)
		}
	})
}

// Manage grants: registry read pulls and can't push; write pushes, is
// recorded under the target, and charges it; revocation ends both.
func TestRegistryManageGrants(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		prod, deploy := r.Mk("", "prod", nil), r.Mk("", "deploy", nil)
		r.Push(prod, "prod/app", "1", conformance.NewImage(`{"os":"linux"}`, "app"))
		grant := func(name, access string) {
			g := r.OK(r.Root(), "POST", "/silo/v1/silos/prod/manageGrants", obj{"name": name, "holder": "/deploy",
				"scope": obj{"facade": obj{"resources": []string{"registry"}, "access": access}}, "lease": obj{"ttl": "3600s"}})
			r.OK(deploy, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
		}
		pushes := func() int {
			return r.Req(deploy, "POST", "/v2/prod/app/blobs/uploads/", "", nil).Code
		}
		grant("pull", "read")
		if res := r.Req(deploy, "GET", "/v2/prod/app/manifests/1", "", nil); res.Code != 200 {
			t.Fatalf("read grant pull: %d %s", res.Code, res.Body)
		}
		if c := pushes(); c != 404 {
			t.Errorf("read grant push: %d", c)
		}
		r.OK(r.Root(), "DELETE", "/silo/v1/silos/prod/manageGrants/pull", nil)
		if res := r.Req(deploy, "GET", "/v2/prod/app/manifests/1", "", nil); res.Code != 404 {
			t.Errorf("after revocation: %d", res.Code)
		}
		grant("rollout", "write")
		before, mine := used(r, "prod"), used(r, "deploy")
		r.Push(deploy, "prod/app", "2", conformance.NewImage(`{"os":"linux"}`, strings.Repeat("x", 4000)))
		if used(r, "prod") <= before || used(r, "deploy") != mine {
			t.Errorf("charges: prod %v -> %v, deploy %v -> %v", before, used(r, "prod"), mine, used(r, "deploy"))
		}
		if p := r.Last("registry.push"); p["_silo"] != prod || p["_actor"] != deploy {
			t.Errorf("the holder's push record: %v", p)
		}
		r.OK(r.Root(), "DELETE", "/silo/v1/silos/prod/manageGrants/rollout", nil)
		if c := pushes(); c != 404 {
			t.Errorf("push after revocation: %d", c)
		}
	})
}

// used is silo's local/disk_gb used, as the fence API reports it.
func used(r conformance.R, silo string) float64 {
	r.T.Helper()
	s := r.OK(r.Root(), "GET", "/silo/v1/silos/"+silo, nil)
	q, _ := s["quota"].(map[string]any)
	g, _ := q["global"].(map[string]any)
	d, _ := g["local/disk_gb"].(map[string]any)
	var f float64
	fmt.Sscan(fmt.Sprint(d["used"]), &f)
	return f
}

// Sealed and read silos push and pull their existing repos, are refused
// a first push to a new repo, and can't delete a repo.
func TestRegistrySealedAndRead(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		for _, control := range []string{"none", "read"} {
			name := "cell-" + control
			s := r.Mk("", name, nil)
			r.Push(s, name+"/app", "1", conformance.NewImage(`{"os":"linux"}`, "before"))
			r.OK(r.Root(), "PATCH", "/silo/v1/silos/"+name, obj{"control": control})
			res := r.Req(s, "POST", "/v2/"+name+"/new/blobs/uploads/", "", nil)
			if res.Code != 403 || res.ErrCode() != "DENIED" {
				t.Errorf("%s: first push: %d %s", control, res.Code, res.Body)
			}
			r.Push(s, name+"/app", "2", conformance.NewImage(`{"os":"linux"}`, "after"))
			if res := r.Req(s, "GET", "/v2/"+name+"/app/manifests/1", "", nil); res.Code != 200 {
				t.Errorf("%s: pull: %d", control, res.Code)
			}
			if code, out := r.API(s, "DELETE", "/silo/v1/self/imageRepos/app", nil); code == 200 {
				t.Errorf("%s: repo delete: %d %v", control, code, out)
			}
		}
	})
}

// Never-looser: images.sharing can't be loosened by a child.
func TestRegistrySharingNeverLooser(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		a := r.Mk("", "team-a", obj{"images": obj{"sharing": "isolated"}})
		r.Mk("team-a", "x", nil)
		if code, _ := r.API(r.Root(), "POST", "/silo/v1/silos/team-a/children",
			obj{"name": "y", "address": obj{"prefix_length": 24}, "images": obj{"sharing": "shared"}}); code == 200 {
			t.Error("a child created shared under an isolated parent")
		}
		if code, _ := r.API(a, "PATCH", "/silo/v1/silos/team-a/x", obj{"images": obj{"sharing": "shared"}}); code == 200 {
			t.Error("a child patched shared under an isolated parent")
		}
		x := r.OK(r.Root(), "GET", "/silo/v1/silos/team-a/x", nil)
		if eff, _ := x["images"].(map[string]any)["effective"].(map[string]any); eff["sharing"] != "isolated" {
			t.Errorf("x's effective sharing: %v", x["images"])
		}
	})
}

// cacheSilos are, under the root: a, which may reach all of the
// upstream; b, which may reach none of it; and c, which may reach the
// registry and its token service but not the CDN.
func cacheSilos(r conformance.R) (a, b, c string) {
	all := conformance.Hosts
	return r.Mk("", "team-a", obj{"egress": obj{"allow": all}}),
		r.Mk("", "team-b", obj{"egress": obj{"allow": []string{}}}),
		r.Mk("", "team-c", obj{"egress": obj{"allow": all[:2]}})
}

const alpine = "/v2/docker.io/library/alpine/"

// Egress through the cache: a silo whose egress denies the upstream
// gets 403 on a hit for an image another silo pulled; denying only the
// blob redirect host refuses blobs and not manifests; a silo with empty
// egress still pulls its own local repo.
func TestRegistryCacheEgress(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		up := r.Upstream()
		a, b, c := cacheSilos(r)
		for _, p := range []string{"manifests/3", "blobs/" + up.Public.LayerD, "blobs/" + up.Public.ConfigD} {
			if res := r.Req(a, "GET", alpine+p, "", nil); res.Code != 200 {
				t.Fatalf("a: GET %s: %d %s", p, res.Code, res.Body)
			}
		}
		fetches := up.Count("cdn.test GET /b/" + up.Public.LayerD)
		for _, p := range []string{"manifests/3", "manifests/" + up.Public.ManifestD, "blobs/" + up.Public.LayerD} {
			res := r.Req(b, "GET", alpine+p, "", nil)
			if res.Code != 403 || res.ErrCode() != "DENIED" || !strings.Contains(string(res.Body), "egress to registry.test is not allowed") {
				t.Errorf("b: a hit on %s: %d %s", p, res.Code, res.Body)
			}
		}
		if rec := r.Last("registry.cache"); rec["_silo"] != b || rec["egress"] != "denied" {
			t.Errorf("b's refusal's record: %v", rec)
		}
		if res := r.Req(c, "GET", alpine+"manifests/3", "", nil); res.Code != 200 {
			t.Errorf("c: the manifest: %d %s", res.Code, res.Body)
		}
		if res := r.Req(c, "GET", alpine+"blobs/"+up.Public.LayerD, "", nil); res.Code != 403 || !strings.Contains(string(res.Body), "cdn.test") {
			t.Errorf("c: a blob behind the CDN: %d %s", res.Code, res.Body)
		}
		if n := up.Count("cdn.test GET /b/" + up.Public.LayerD); n != fetches {
			t.Errorf("refused hits fetched the layer: %d upstream GETs, want %d", n, fetches)
		}
		own := conformance.NewImage(`{"os":"linux"}`, "a local layer")
		r.Push(b, "team-b/app", "1", own)
		if res := r.Req(b, "GET", "/v2/team-b/app/blobs/"+own.LayerD, "", nil); res.Code != 200 || string(res.Body) != "a local layer" {
			t.Errorf("b: its own repo, with empty egress: %d %s", res.Code, res.Body)
		}
	})
}

// Private upstream images: a silo with credentials pulls; then a silo
// without credentials gets 401 on the manifest and 404 on each blob by
// digest, under the private repo's path and under a public repo's
// path, and wrong credentials do no better.
func TestRegistryCachePrivate(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		up := r.Upstream()
		a, _, _ := cacheSilos(r)
		d := r.Mk("", "team-d", obj{"egress": obj{"allow": conformance.Hosts}})
		const repo = "/v2/docker.io/private/app/"
		if res := r.ReqAuth(a, repo+"manifests/1", up.User, up.Pass); res.Code != 200 || res.Header.Get("Docker-Content-Digest") != up.Private.ManifestD {
			t.Fatalf("a, with credentials: %d %s", res.Code, res.Body)
		}
		for _, dg := range []string{up.Private.ConfigD, up.Private.LayerD} {
			if res := r.ReqAuth(a, repo+"blobs/"+dg, up.User, up.Pass); res.Code != 200 || conformance.Digest(res.Body) != dg {
				t.Fatalf("a, blob %s: %d", dg, res.Code)
			}
		}
		for _, ref := range []string{"1", up.Private.ManifestD} {
			if res := r.Req(d, "GET", repo+"manifests/"+ref, "", nil); res.Code != 401 {
				t.Errorf("d, no credentials, manifest %s: %d %s", ref, res.Code, res.Body)
			}
		}
		for _, p := range []string{repo, alpine} {
			for _, dg := range []string{up.Private.ConfigD, up.Private.LayerD} {
				if res := r.Req(d, "GET", p+"blobs/"+dg, "", nil); res.Code != 404 {
					t.Errorf("d, no credentials, %sblobs/%s: %d %s", p, dg, res.Code, res.Body)
				}
				if res := r.ReqAuth(d, p+"blobs/"+dg, up.User, "wrong"); res.Code != 404 {
					t.Errorf("d, wrong credentials, %sblobs/%s: %d %s", p, dg, res.Code, res.Body)
				}
			}
		}
		if res := r.ReqAuth(d, repo+"manifests/1", up.User, "wrong"); res.Code != 401 {
			t.Errorf("d, wrong credentials, the manifest: %d %s", res.Code, res.Body)
		}
	})
}

// Isolation: after a shared-domain pull of an image, an isolated silo's
// pull of it makes upstream requests for every blob.
func TestRegistryCacheIsolation(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		up := r.Upstream()
		a, _, _ := cacheSilos(r)
		iso := r.Mk("", "iso", obj{"egress": obj{"allow": conformance.Hosts}, "images": obj{"sharing": "isolated"}})
		blobs := []string{up.Public.ConfigD, up.Public.LayerD}
		pull := func(silo string) {
			t.Helper()
			for _, p := range append([]string{"manifests/3"}, func() (ps []string) {
				for _, d := range blobs {
					ps = append(ps, "blobs/"+d)
				}
				return
			}()...) {
				if res := r.Req(silo, "GET", alpine+p, "", nil); res.Code != 200 {
					t.Fatalf("GET %s: %d %s", p, res.Code, res.Body)
				}
			}
		}
		pull(a)
		before := map[string]int{}
		for _, d := range blobs {
			before[d] = up.Count("cdn.test GET /b/" + d)
		}
		pull(a)
		for _, d := range blobs {
			if n := up.Count("cdn.test GET /b/" + d); n != before[d] {
				t.Errorf("a shared-domain hit fetched %s again", d)
			}
		}
		pull(iso)
		for _, d := range blobs {
			if n := up.Count("cdn.test GET /b/" + d); n != before[d]+1 {
				t.Errorf("the isolated silo's pull of %s: %d upstream GETs, want %d", d, n, before[d]+1)
			}
		}
	})
}

// Audit: every request of a pull, hit and miss, produces a record as
// the calling silo, and no record contains a canary credential the
// test presented or the token the upstream gave for it.
func TestRegistryCacheAudit(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		up := r.Upstream()
		a, _, _ := cacheSilos(r)
		d := r.Mk("", "team-d", obj{"egress": obj{"allow": conformance.Hosts}})
		paths := []string{"manifests/3", "blobs/" + up.Public.ConfigD, "blobs/" + up.Public.LayerD}
		for _, silo := range []string{a, d} { // a's misses, d's hits
			n := len(r.Audit("registry.cache"))
			for _, p := range paths {
				if res := r.Req(silo, "GET", alpine+p, "", nil); res.Code != 200 {
					t.Fatalf("GET %s: %d", p, res.Code)
				}
			}
			recs := r.Audit("registry.cache")[n:]
			if len(recs) != len(paths) {
				t.Fatalf("%d records for %d requests: %v", len(recs), len(paths), recs)
			}
			for _, rec := range recs {
				if rec["_silo"] != silo {
					t.Errorf("a record not as the caller %s: %v", silo, rec)
				}
			}
		}
		if res := r.ReqAuth(a, "/v2/docker.io/private/app/manifests/1", up.User, up.Pass); res.Code != 200 {
			t.Fatalf("the private pull: %d %s", res.Code, res.Body)
		}
		for _, kind := range []string{"registry.cache", "registry.pull", "registry.denied"} {
			b, _ := json.Marshal(r.Audit(kind))
			if strings.Contains(string(b), up.Pass) || strings.Contains(string(b), up.Token) {
				t.Errorf("a %s record holds a canary", kind)
			}
		}
	})
}

// Quota: a push over disk_gb is refused mid-upload; deleting the repo
// refunds exactly what the push charged; image_repos limits creation.
func TestRegistryQuota(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R) {
		// 0.000002 GiB is 2147 bytes.
		small := r.Mk("", "small", obj{"quota": obj{"global": obj{"disk_gb": "0.000002", "image_repos": 1}}})
		res := r.Req(small, "POST", "/v2/small/app/blobs/uploads/", "", nil)
		loc := res.Header.Get("Location")
		if res.Code != 202 || loc == "" {
			t.Fatalf("start: %d %s", res.Code, res.Body)
		}
		if res := r.Req(small, "PATCH", loc, "application/octet-stream", []byte(strings.Repeat("a", 1500))); res.Code != 202 {
			t.Fatalf("first chunk: %d %s", res.Code, res.Body)
		}
		if res := r.Req(small, "PATCH", loc, "application/octet-stream", []byte(strings.Repeat("b", 1000))); res.Code != 413 || res.ErrCode() != "DENIED" {
			t.Errorf("over disk_gb: %d %s", res.Code, res.Body)
		}
		r.Req(small, "DELETE", loc, "", nil)

		team := r.Mk("", "team", obj{"quota": obj{"global": obj{"disk_gb": 1, "image_repos": 1}}})
		before := used(r, "team")
		r.Push(team, "team/app", "1", conformance.NewImage(`{"os":"linux"}`, strings.Repeat("z", 5000)))
		if used(r, "team") <= before {
			t.Errorf("the push charged nothing: %v", used(r, "team"))
		}
		if res := r.Req(team, "POST", "/v2/team/other/blobs/uploads/", "", nil); res.Code != 403 && res.Code != 413 {
			t.Errorf("a second repo over image_repos: %d %s", res.Code, res.Body)
		}
		r.OK(team, "DELETE", "/silo/v1/self/imageRepos/app", nil)
		if after := used(r, "team"); after != before {
			t.Errorf("after the repo delete: %v used, want %v", after, before)
		}
	})
}

// OCI conformance: the distribution-spec conformance suite passes
// against a silo's own repo, for pull, push, content discovery and
// management. The suite is a test binary the registry CI job builds;
// without one the case is skipped, unless SILO_REQUIRE_REGISTRY_CLIENTS
// makes that a failure.
func TestRegistryDistributionSpec(t *testing.T) {
	bin := os.Getenv("SILO_OCI_CONFORMANCE")
	if bin == "" {
		if os.Getenv("SILO_REQUIRE_REGISTRY_CLIENTS") != "" {
			t.Fatal("SILO_OCI_CONFORMANCE names no conformance suite")
		}
		t.Skip("no distribution-spec conformance suite: set SILO_OCI_CONFORMANCE")
	}
	each(t, func(t *testing.T, r conformance.R) {
		s := r.Mk("", "conf", nil)
		cmd := exec.Command(bin)
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(),
			"OCI_ROOT_URL=http://"+r.Listen(s),
			"OCI_NAMESPACE=conf/repo-a",
			"OCI_CROSSMOUNT_NAMESPACE=conf/repo-b",
			"OCI_TEST_PULL=1", "OCI_TEST_PUSH=1", "OCI_TEST_CONTENT_DISCOVERY=1", "OCI_TEST_CONTENT_MANAGEMENT=1",
			"OCI_AUTOMATIC_CROSSMOUNT=1",
			"OCI_REPORT_DIR="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the conformance suite: %v\n%s", err, out)
		}
		if i := strings.Index(string(out), "OCI Conformance Result"); i >= 0 {
			t.Log(string(out[i:]))
		}
	})
}
