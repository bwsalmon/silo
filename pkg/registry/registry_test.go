package registry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

var ctx = context.Background()

type obj = map[string]any

// env is a store, the fence API and the registry over Memory, served
// as the gateway forwards it: the caller is the Silo-Caller header's
// silo, by origin.
type env struct {
	t       *testing.T
	db      store.Database
	dbPath  string
	api     *fenceapi.Server
	apiSrv  *httptest.Server
	content *registry.Memory
	reg     *registry.Service
	srv     *httptest.Server
	now     time.Time
	root    string
}

func newEnv(t *testing.T, capacity obj) *env {
	t.Helper()
	return newEnvWith(t, capacity, nil)
}

// newEnvWith is newEnv with extra fields in the bootstrap call.
func newEnvWith(t *testing.T, capacity, bootstrap obj) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "silo.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, dbPath: path, content: registry.NewMemory(), now: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	e.content.Now = func() time.Time { return e.now }
	caller := func(r *http.Request) fenceapi.Caller {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaOrigin}
	}
	e.api = fenceapi.New(db, fenceapi.Options{Caller: func(r *http.Request) (fenceapi.Caller, error) { return caller(r), nil }})
	e.apiSrv = httptest.NewServer(e.api)
	e.reg = registry.New(registry.Options{Store: db, API: e.api, Content: e.content, Now: func() time.Time { return e.now },
		MaxChunk: 1 << 20})
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.reg.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), caller(r))))
	}))
	t.Cleanup(func() { e.srv.Close(); e.apiSrv.Close(); e.api.Close() })
	if capacity == nil {
		capacity = obj{"global": obj{"disk_gb": 100, "image_repos": 100}}
	}
	body := obj{"regions": []string{"us-central1"}, "capacity": capacity,
		"grant_policy": obj{"exports": "allow"}, "admins": []string{"user:admin@example.com"}}
	for k, v := range bootstrap {
		body[k] = v
	}
	code, root := e.call("", "POST", "/silod/bootstrap", body)
	if code != 200 {
		t.Fatalf("bootstrap: %d %v", code, root)
	}
	e.root = root["id"].(string)
	return e
}

// call makes a fence API call as caller.
func (e *env) call(caller, method, path string, body any) (int, obj) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.apiSrv.URL+path, bytes.NewReader(b))
	req.Header.Set("Silo-Caller", caller)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := obj{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) ok(caller, method, path string, body any) obj {
	e.t.Helper()
	code, out := e.call(caller, method, path, body)
	if code != 200 {
		e.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out
}

// mk makes a child of parent as caller and returns its id.
func (e *env) mk(caller, parent, name string, extra obj) string {
	e.t.Helper()
	bits, disk, repos := 24, 5, 3
	if parent == "-"+e.root {
		bits, disk, repos = 16, 20, 10
	}
	body := obj{"name": name, "address": obj{"prefix_length": bits},
		"quota": obj{"global": obj{"disk_gb": disk, "image_repos": repos}}}
	for k, v := range extra {
		body[k] = v
	}
	return e.ok(caller, "POST", "/silo/v1/silos/"+parent+"/children", body)["id"].(string)
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (r resp) errCode() string {
	var out struct {
		Errors []struct{ Code string } `json:"errors"`
	}
	json.Unmarshal(r.body, &out)
	if len(out.Errors) == 0 {
		return ""
	}
	return out.Errors[0].Code
}

// reg makes a registry request as caller.
func (e *env) req(caller, method, path, ctype string, body []byte) resp {
	e.t.Helper()
	u := path
	if !strings.HasPrefix(u, "http") {
		u = e.srv.URL + path
	}
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	req.Header.Set("Silo-Caller", caller)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{code: res.StatusCode, header: res.Header, body: b}
}

// pushBlob uploads b to name monolithically as caller, and returns its
// digest.
func (e *env) pushBlob(caller, name string, b []byte) string {
	e.t.Helper()
	d := registry.Digest("sha256", b)
	if r := e.req(caller, "POST", "/v2/"+name+"/blobs/uploads/?digest="+d, "application/octet-stream", b); r.code != 201 {
		e.t.Fatalf("pushing a blob to %s: %d %s", name, r.code, r.body)
	}
	return d
}

// image is a manifest of config and layers, as pushed.
func image(config string, cs int, layers map[string]int) []byte {
	m := obj{"schemaVersion": 2, "mediaType": registry.MediaOCIManifest,
		"config": obj{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": config, "size": cs}}
	var ls []obj
	for d, n := range layers {
		ls = append(ls, obj{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": d, "size": n})
	}
	m["layers"] = ls
	b, _ := json.Marshal(m)
	return b
}

// pushImage pushes a one-layer image to name:tag as caller and returns
// the layer's and the manifest's digests.
func (e *env) pushImage(caller, name, tag, layer string) (string, string) {
	e.t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	c := e.pushBlob(caller, name, cfg)
	l := e.pushBlob(caller, name, []byte(layer))
	m := image(c, len(cfg), map[string]int{l: len(layer)})
	r := e.req(caller, "PUT", "/v2/"+name+"/manifests/"+tag, registry.MediaOCIManifest, m)
	if r.code != 201 {
		e.t.Fatalf("pushing %s:%s: %d %s", name, tag, r.code, r.body)
	}
	return l, registry.Digest("sha256", m)
}

func (e *env) audit(kind string) []obj {
	e.t.Helper()
	var out []obj
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT silo, actor, result, fields FROM audit WHERE kind = ? ORDER BY n`, kind)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var silo, actor, result, fields string
			if err := rows.Scan(&silo, &actor, &result, &fields); err != nil {
				return err
			}
			f := obj{}
			json.Unmarshal([]byte(fields), &f)
			f["_silo"], f["_actor"], f["_result"] = silo, actor, result
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// A push and a pull, by path and by id: a first push creates the repo,
// the blobs and the manifest come back, the tag list and the catalog
// name it, and the repo's silo-registry record carries dev.silo.seq.
func TestPushPull(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	x := e.mk(e.root, "team-a", "x", nil)

	if r := e.req(x, "GET", "/v2/", "", nil); r.code != 200 || r.header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatalf("/v2/: %d %v", r.code, r.header)
	}
	layer, md := e.pushImage(x, "team-a/x/app", "1", "layer one")
	for _, name := range []string{"team-a/x/app", "id_" + x + "/app"} {
		r := e.req(x, "GET", "/v2/"+name+"/manifests/1", "", nil)
		if r.code != 200 || r.header.Get("Docker-Content-Digest") != md || r.header.Get("Content-Type") != registry.MediaOCIManifest {
			t.Fatalf("GET %s:1: %d %v %s", name, r.code, r.header, r.body)
		}
		if r := e.req(x, "GET", "/v2/"+name+"/blobs/"+layer, "", nil); r.code != 200 || string(r.body) != "layer one" {
			t.Fatalf("GET %s's layer: %d %s", name, r.code, r.body)
		}
		if r := e.req(x, "GET", "/v2/"+name+"/tags/list", "", nil); r.code != 200 || !strings.Contains(string(r.body), `"tags":["1"]`) {
			t.Fatalf("tags of %s: %d %s", name, r.code, r.body)
		}
	}
	// Range reads.
	req, _ := http.NewRequest("GET", e.srv.URL+"/v2/team-a/x/app/blobs/"+layer, nil)
	req.Header.Set("Silo-Caller", x)
	req.Header.Set("Range", "bytes=0-4")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 || string(b) != "layer" {
		t.Fatalf("range: %d %q", res.StatusCode, b)
	}
	if r := e.req(a, "GET", "/v2/_catalog", "", nil); r.code != 200 || string(r.body) != `{"repositories":["team-a/x/app"]}` {
		t.Fatalf("a's catalog: %d %s", r.code, r.body)
	}
	if r := e.req(x, "GET", "/v2/_catalog", "", nil); string(r.body) != `{"repositories":["team-a/x/app"]}` {
		t.Fatalf("x's catalog: %s", r.body)
	}
	imgs := e.content.Images()
	if len(imgs) != 1 || !strings.HasSuffix(imgs[0].Name, "@"+md) || !strings.HasPrefix(imgs[0].Name, "ir-") || imgs[0].Labels[registry.SeqLabel] == "" {
		t.Fatalf("silo-registry records: %+v", imgs)
	}
	got := e.ok(x, "GET", "/silo/v1/self/imageRepos/app", nil)
	if got["tags"] != float64(1) || got["manifests"] != float64(1) {
		t.Fatalf("the repo: %v", got)
	}
	push, pull := e.audit(fenceapi.KindRegistryPush), e.audit(fenceapi.KindRegistryPull)
	if len(push) != 3 || push[2]["digest"] != md || push[2]["tag"] != "1" || push[2]["_silo"] != x {
		t.Fatalf("registry.push: %v", push)
	}
	if len(pull) != 5 || pull[0]["repo"] != "app" || pull[0]["reference"] != "1" {
		t.Fatalf("registry.pull: %v", pull)
	}
	// Retagging keeps the old manifest, untagged, pullable by digest.
	_, md2 := e.pushImage(x, "team-a/x/app", "1", "layer two")
	if r := e.req(x, "GET", "/v2/team-a/x/app/manifests/1", "", nil); r.header.Get("Docker-Content-Digest") != md2 {
		t.Fatalf("retag: %v", r.header)
	}
	if r := e.req(x, "HEAD", "/v2/team-a/x/app/manifests/"+md, "", nil); r.code != 200 {
		t.Fatalf("the old manifest: %d", r.code)
	}
	// An upstream path is the cache's, which is read-only.
	if r := e.req(x, "PUT", "/v2/docker.io/library/alpine/manifests/1", registry.MediaOCIManifest, []byte("{}")); r.code != 405 || r.errCode() != "UNSUPPORTED" {
		t.Fatalf("a push to the cache: %d %s", r.code, r.body)
	}
}

// A sibling gets the same 404 NAME_UNKNOWN on pull, push, HEAD and the
// tag list, for a repo that exists and one that doesn't, by path and
// by id (call L63).
func TestSiblingNotFound(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	b := e.mk(e.root, "-"+e.root, "team-b", nil)
	layer, md := e.pushImage(a, "team-a/app", "1", "secret layer")
	var answers []string
	for _, name := range []string{"team-a/app", "id_" + a + "/app", "team-a/nope", "id_" + a + "/nope", "team-zzz/app", "id_zzzzzz/app"} {
		for _, c := range []struct {
			method, path, ctype string
			body                []byte
		}{
			{"GET", "/manifests/1", "", nil},
			{"HEAD", "/manifests/1", "", nil},
			{"GET", "/manifests/" + md, "", nil},
			{"GET", "/blobs/" + layer, "", nil},
			{"HEAD", "/blobs/" + layer, "", nil},
			{"GET", "/tags/list", "", nil},
			{"GET", "/referrers/" + md, "", nil},
			{"POST", "/blobs/uploads/", "", nil},
			{"POST", "/blobs/uploads/?digest=" + registry.Digest("sha256", []byte("x")), "application/octet-stream", []byte("x")},
			{"PUT", "/manifests/2", registry.MediaOCIManifest, image(md, 1, nil)},
			{"DELETE", "/manifests/1", "", nil},
		} {
			r := e.req(b, c.method, "/v2/"+name+c.path, c.ctype, c.body)
			if r.code != 404 || (c.method != "HEAD" && r.errCode() != "NAME_UNKNOWN") {
				t.Errorf("%s %s%s as a sibling: %d %s", c.method, name, c.path, r.code, r.body)
			}
			answers = append(answers, fmt.Sprintf("%s %s %d %s", c.method, c.path, r.code, r.body))
		}
	}
	// Authorization identifies nobody: credentials change nothing.
	req, _ := http.NewRequest("GET", e.srv.URL+"/v2/team-a/app/manifests/1", nil)
	req.Header.Set("Silo-Caller", b)
	req.SetBasicAuth(a, "anything")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 404 {
		t.Fatalf("with Authorization: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	// Identical whatever exists.
	n := len(answers) / 6
	for i := 1; i < 6; i++ {
		for j := 0; j < n; j++ {
			if answers[i*n+j] != answers[j] {
				t.Errorf("answer %d differs: %q vs %q", j, answers[i*n+j], answers[j])
			}
		}
	}
	// Nothing was created in a's silo, and b has no repo.
	if r := e.req(a, "GET", "/v2/_catalog", "", nil); string(r.body) != `{"repositories":["team-a/app"]}` {
		t.Fatalf("a's catalog: %s", r.body)
	}
	if r := e.req(b, "GET", "/v2/_catalog", "", nil); string(r.body) != `{"repositories":[]}` {
		t.Fatalf("b's catalog: %s", r.body)
	}
	if d := e.audit(fenceapi.KindRegistryDenied); len(d) == 0 || d[0]["_silo"] != b || d[0]["reason"] != "access" {
		t.Fatalf("registry.denied: %v", d)
	}
}

// A silo that pushed blob B can't learn that another silo has B: not by
// HEAD, not by a mount without from, not by a mount from a repo it
// can't read (call L62).
func TestProofOfPossession(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	b := e.mk(e.root, "-"+e.root, "team-b", nil)
	blob := []byte("the shared blob")
	d := e.pushBlob(a, "team-a/app", blob)
	e.pushImage(a, "team-a/app", "1", "other")
	// The bytes are on disk; b's repo doesn't hold them.
	e.pushImage(b, "team-b/mine", "1", "mine")
	if r := e.req(b, "HEAD", "/v2/team-b/mine/blobs/"+d, "", nil); r.code != 404 {
		t.Fatalf("HEAD of a's blob in b's repo: %d", r.code)
	}
	if r := e.req(b, "GET", "/v2/team-b/mine/blobs/"+d, "", nil); r.code != 404 || r.errCode() != "BLOB_UNKNOWN" {
		t.Fatalf("GET of a's blob in b's repo: %d %s", r.code, r.body)
	}
	for _, q := range []string{"mount=" + d, "mount=" + d + "&from=team-a/app", "mount=" + d + "&from=id_" + a + "/app", "mount=" + d + "&from=docker.io/library/x"} {
		r := e.req(b, "POST", "/v2/team-b/mine/blobs/uploads/?"+q, "", nil)
		if r.code != 202 || r.header.Get("Location") == "" {
			t.Fatalf("mount %s: %d %v %s, want a session", q, r.code, r.header, r.body)
		}
	}
	// A manifest naming it is MANIFEST_BLOB_UNKNOWN.
	cfg := e.pushBlob(b, "team-b/mine", []byte("{}"))
	if r := e.req(b, "PUT", "/v2/team-b/mine/manifests/2", registry.MediaOCIManifest, image(cfg, 2, map[string]int{d: len(blob)})); r.code != 400 || r.errCode() != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatalf("a manifest naming a's blob: %d %s", r.code, r.body)
	}
	// Uploading it is proof: then it's b's.
	e.pushBlob(b, "team-b/mine", blob)
	if r := e.req(b, "HEAD", "/v2/team-b/mine/blobs/"+d, "", nil); r.code != 200 {
		t.Fatalf("HEAD after b uploaded it: %d", r.code)
	}
	// And a mount from a repo the caller reads works: x's parent a
	// reads x's repos.
	x := e.mk(e.root, "team-a", "x", nil)
	xl, _ := e.pushImage(x, "team-a/x/lib", "1", "x's layer")
	if r := e.req(a, "POST", "/v2/team-a/app/blobs/uploads/?mount="+xl+"&from=team-a/x/lib", "", nil); r.code != 201 {
		t.Fatalf("a's mount from x: %d %s", r.code, r.body)
	}
	if r := e.req(a, "HEAD", "/v2/team-a/app/blobs/"+xl, "", nil); r.code != 200 {
		t.Fatalf("HEAD after the mount: %d", r.code)
	}
}

// An ancestor pulls its descendant's repos and can't push to them.
func TestAncestor(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	x := e.mk(e.root, "team-a", "x", nil)
	layer, _ := e.pushImage(x, "team-a/x/app", "1", "x's")
	if r := e.req(a, "GET", "/v2/team-a/x/app/manifests/1", "", nil); r.code != 200 {
		t.Fatalf("ancestor pull: %d %s", r.code, r.body)
	}
	if r := e.req(a, "GET", "/v2/team-a/x/app/blobs/"+layer, "", nil); r.code != 200 {
		t.Fatalf("ancestor blob: %d", r.code)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/blobs/uploads/"},
		{"PUT", "/manifests/2"},
		{"DELETE", "/manifests/1"},
	} {
		if r := e.req(a, c.method, "/v2/team-a/x/app"+c.path, registry.MediaOCIManifest, []byte("{}")); r.code != 404 || r.errCode() != "NAME_UNKNOWN" {
			t.Errorf("ancestor %s %s: %d %s", c.method, c.path, r.code, r.body)
		}
	}
	// Nor create one in a descendant.
	if r := e.req(a, "POST", "/v2/team-a/x/new/blobs/uploads/", "", nil); r.code != 404 {
		t.Fatalf("ancestor first push: %d", r.code)
	}
	// The pull is recorded under x, by a.
	pulls := e.audit(fenceapi.KindRegistryPull)
	if len(pulls) == 0 || pulls[0]["_silo"] != x || pulls[0]["_actor"] != a {
		t.Fatalf("pulls: %v", pulls)
	}
}

// A sealed silo's first push is refused, audited as registry.denied,
// and it still pushes to its existing repos (call L65).
func TestSealed(t *testing.T) {
	e := newEnv(t, nil)
	s := e.mk(e.root, "-"+e.root, "cell", nil)
	e.pushImage(s, "cell/app", "1", "before")
	e.ok(e.root, "PATCH", "/silo/v1/silos/cell", obj{"control": "none"})
	r := e.req(s, "POST", "/v2/cell/new/blobs/uploads/", "", nil)
	if r.code != 403 || r.errCode() != "DENIED" || !strings.Contains(string(r.body), "control-plane call") {
		t.Fatalf("sealed first push: %d %s", r.code, r.body)
	}
	if d := e.audit(fenceapi.KindRegistryDenied); len(d) != 1 || d[0]["reason"] != "control" || d[0]["_silo"] != s {
		t.Fatalf("registry.denied: %v", d)
	}
	e.pushImage(s, "cell/app", "2", "after")
	if r := e.req(s, "GET", "/v2/cell/app/tags/list", "", nil); !strings.Contains(string(r.body), `["1","2"]`) {
		t.Fatalf("tags: %s", r.body)
	}
	if r := e.req(s, "GET", "/v2/_catalog", "", nil); string(r.body) != `{"repositories":["cell/app"]}` {
		t.Fatalf("catalog: %s", r.body)
	}
}

// A repo charges its silo's disk_gb as bytes arrive; a chunk that would
// take the silo over its grant is 413 DENIED and taken back.
func TestDiskQuota(t *testing.T) {
	e := newEnv(t, nil)
	small := e.mk(e.root, "-"+e.root, "small", obj{"quota": obj{"global": obj{"disk_gb": "0.000002", "image_repos": 1}}})
	// 0.000002 GiB is 2147 bytes, and charges round up to a millionth
	// of a GiB: 1500 bytes is 2 of them, 2500 bytes 3.
	r := e.req(small, "POST", "/v2/small/app/blobs/uploads/", "", nil)
	if r.code != 202 {
		t.Fatalf("start: %d %s", r.code, r.body)
	}
	loc := r.header.Get("Location")
	if r := e.req(small, "PATCH", loc, "application/octet-stream", bytes.Repeat([]byte("a"), 1500)); r.code != 202 || r.header.Get("Range") != "0-1499" {
		t.Fatalf("first chunk: %d %v %s", r.code, r.header, r.body)
	}
	r = e.req(small, "PATCH", loc, "application/octet-stream", bytes.Repeat([]byte("b"), 1000))
	if r.code != 413 || r.errCode() != "DENIED" || !strings.Contains(string(r.body), "/small is over its disk_gb grant") {
		t.Fatalf("over: %d %s", r.code, r.body)
	}
	if r := e.req(small, "GET", loc, "", nil); r.code != 204 || r.header.Get("Range") != "0-1499" {
		t.Fatalf("after the refusal: %d %v", r.code, r.header)
	}
	if d := e.audit(fenceapi.KindRegistryDenied); len(d) != 1 || d[0]["reason"] != "quota" {
		t.Fatalf("registry.denied: %v", d)
	}
	// Cancelling refunds it.
	if r := e.req(small, "DELETE", loc, "", nil); r.code != 204 {
		t.Fatalf("cancel: %d", r.code)
	}
	if q := used(t, e, small); q != 0 {
		t.Fatalf("used after cancel: %d", q)
	}
	d := e.pushBlob(small, "small/app", bytes.Repeat([]byte("c"), 2000))
	if q := used(t, e, small); q != 2 { // micro-GiB, rounded up
		t.Fatalf("used: %d", q)
	}
	// The grace hour: unreferenced, it is dropped and refunded.
	e.now = e.now.Add(time.Hour + time.Second)
	if r := e.req(small, "HEAD", "/v2/small/app/blobs/"+d, "", nil); r.code != 404 {
		t.Fatalf("after the grace hour: %d", r.code)
	}
	if q := used(t, e, small); q != 0 {
		t.Fatalf("used after the grace hour: %d", q)
	}
}

func used(t *testing.T, e *env, silo string) int64 {
	t.Helper()
	var n int64
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0) FROM charges WHERE payer = ? AND class = 'disk_gb'`, silo).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Chunked uploads, an out-of-order chunk, a wrong digest, and the
// session's lease.
func TestChunked(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	b := e.mk(e.root, "-"+e.root, "team-b", nil)
	r := e.req(a, "POST", "/v2/team-a/app/blobs/uploads/", "", nil)
	loc := r.header.Get("Location")
	if len(e.content.Leases()) != 1 {
		t.Fatalf("leases: %v", e.content.Leases())
	}
	// Another silo can't use the session, even on its own repo name.
	if r := e.req(b, "PATCH", strings.Replace(loc, "team-a", "team-b", 1), "", []byte("x")); r.code != 404 {
		t.Fatalf("b on a's session: %d %s", r.code, r.body)
	}
	e.req(a, "PATCH", loc, "", []byte("hello "))
	req, _ := http.NewRequest("PATCH", e.srv.URL+loc, strings.NewReader("zzz"))
	req.Header.Set("Silo-Caller", a)
	req.Header.Set("Content-Range", "0-2")
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 416 {
		t.Fatalf("out-of-order chunk: %d", res.StatusCode)
	}
	if r := e.req(a, "PUT", loc+"?digest="+registry.Digest("sha256", []byte("nope")), "", []byte("world")); r.code != 400 || r.errCode() != "DIGEST_INVALID" {
		t.Fatalf("wrong digest: %d %s", r.code, r.body)
	}
	d := registry.Digest("sha256", []byte("hello world"))
	r = e.req(a, "PUT", loc+"?digest="+d, "", nil)
	if r.code != 201 || r.header.Get("Docker-Content-Digest") != d {
		t.Fatalf("commit: %d %s", r.code, r.body)
	}
	if r := e.req(a, "GET", "/v2/team-a/app/blobs/"+d, "", nil); string(r.body) != "hello world" {
		t.Fatalf("blob: %q", r.body)
	}
}

// Referrers: a manifest pushed with subject is listed, filtered by
// artifactType; the subject may be absent when it is pushed.
func TestReferrers(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	subject := registry.Digest("sha256", []byte("not pushed yet"))
	cfg := e.pushBlob(a, "team-a/app", []byte("{}"))
	sig := obj{"schemaVersion": 2, "mediaType": registry.MediaOCIManifest, "artifactType": "application/vnd.example.sig",
		"config": obj{"mediaType": "application/vnd.oci.empty.v1+json", "digest": cfg, "size": 2},
		"layers": []obj{}, "subject": obj{"mediaType": registry.MediaOCIManifest, "digest": subject, "size": 10}}
	b, _ := json.Marshal(sig)
	r := e.req(a, "PUT", "/v2/team-a/app/manifests/"+registry.Digest("sha256", b), registry.MediaOCIManifest, b)
	if r.code != 201 || r.header.Get("OCI-Subject") != subject {
		t.Fatalf("push with subject: %d %v %s", r.code, r.header, r.body)
	}
	r = e.req(a, "GET", "/v2/team-a/app/referrers/"+subject, "", nil)
	if r.code != 200 || !strings.Contains(string(r.body), "application/vnd.example.sig") {
		t.Fatalf("referrers: %d %s", r.code, r.body)
	}
	r = e.req(a, "GET", "/v2/team-a/app/referrers/"+subject+"?artifactType=other", "", nil)
	if r.header.Get("OCI-Filters-Applied") != "artifactType" || strings.Contains(string(r.body), "example.sig") {
		t.Fatalf("filtered: %v %s", r.header, r.body)
	}
	// A manifest with an unknown field is invalid.
	if r := e.req(a, "PUT", "/v2/team-a/app/manifests/x", registry.MediaOCIManifest, []byte(`{"schemaVersion":2,"bogus":1}`)); r.code != 400 || r.errCode() != "MANIFEST_INVALID" {
		t.Fatalf("bogus: %d %s", r.code, r.body)
	}
}

// An import of a registry export lets the importer pull and lists the
// repo in its catalog; it never pushes.
func TestImport(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	b := e.mk(e.root, "-"+e.root, "team-b", nil)
	e.pushImage(a, "team-a/base", "1", "base")
	e.ok(a, "POST", "/silo/v1/self/exports", obj{"name": "imgs", "protocol": "registry", "repos": []string{"base"}, "to": []string{"/team-b"}})
	e.ok(b, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/imgs"})
	if r := e.req(b, "GET", "/v2/team-a/base/manifests/1", "", nil); r.code != 200 {
		t.Fatalf("import pull: %d %s", r.code, r.body)
	}
	if r := e.req(b, "POST", "/v2/team-a/base/blobs/uploads/", "", nil); r.code != 404 {
		t.Fatalf("import push: %d", r.code)
	}
	if r := e.req(b, "GET", "/v2/_catalog", "", nil); string(r.body) != `{"repositories":["team-a/base"]}` {
		t.Fatalf("b's catalog: %s", r.body)
	}
	if p := e.audit(fenceapi.KindRegistryPull); len(p) != 1 || p[0]["import"] == nil {
		t.Fatalf("pull: %v", p)
	}
}

// An export offered but not accepted gives nothing; accepted, it gives
// pulls and never pushes or deletes; a prefix covers a repo created
// after the accept; and once withdrawn the importer pulls nothing more
// (local-registry.md §12, "exports").
func TestExportGrants(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	b := e.mk(e.root, "-"+e.root, "team-b", nil)
	e.pushImage(a, "team-a/base-go", "1", "go")
	e.ok(a, "POST", "/silo/v1/self/exports", obj{"name": "imgs", "protocol": "registry", "repos": []string{"base-*"}, "to": []string{"/team-b"}})
	if r := e.req(b, "GET", "/v2/team-a/base-go/manifests/1", "", nil); r.code != 404 || r.errCode() != "NAME_UNKNOWN" {
		t.Fatalf("offered, not accepted: %d %s", r.code, r.body)
	}
	if r := e.req(b, "GET", "/v2/_catalog", "", nil); string(r.body) != `{"repositories":[]}` && string(r.body) != `{"repositories":null}` {
		t.Fatalf("catalog before the accept: %s", r.body)
	}
	e.ok(b, "POST", "/silo/v1/self/imports", obj{"export": "/team-a/imgs"})
	if r := e.req(b, "GET", "/v2/team-a/base-go/manifests/1", "", nil); r.code != 200 {
		t.Fatalf("accepted: %d %s", r.code, r.body)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/blobs/uploads/"},
		{"PUT", "/manifests/2"},
		{"DELETE", "/manifests/1"},
	} {
		if r := e.req(b, c.method, "/v2/team-a/base-go"+c.path, registry.MediaOCIManifest, []byte("{}")); r.code != 404 || r.errCode() != "NAME_UNKNOWN" {
			t.Errorf("importer %s %s: %d %s", c.method, c.path, r.code, r.body)
		}
	}
	// Nor create one in the exporter by pushing, under the prefix.
	if r := e.req(b, "POST", "/v2/team-a/base-new/blobs/uploads/", "", nil); r.code != 404 {
		t.Fatalf("importer first push: %d", r.code)
	}
	// A repo the prefix covers, created after the accept, is pulled;
	// one it doesn't cover isn't.
	layer, _ := e.pushImage(a, "team-a/base-py", "1", "py")
	e.pushImage(a, "team-a/secret", "1", "s")
	if r := e.req(b, "GET", "/v2/team-a/base-py/blobs/"+layer, "", nil); r.code != 200 || string(r.body) != "py" {
		t.Fatalf("a repo created after the accept: %d %s", r.code, r.body)
	}
	if r := e.req(b, "GET", "/v2/team-a/secret/manifests/1", "", nil); r.code != 404 {
		t.Fatalf("a repo the export doesn't cover: %d", r.code)
	}
	if r := e.req(b, "GET", "/v2/_catalog", "", nil); string(r.body) != `{"repositories":["team-a/base-go","team-a/base-py"]}` {
		t.Fatalf("b's catalog: %s", r.body)
	}
	// Withdrawn, the next pull is refused.
	e.ok(a, "DELETE", "/silo/v1/self/exports/imgs", nil)
	if r := e.req(b, "GET", "/v2/team-a/base-go/manifests/1", "", nil); r.code != 404 {
		t.Fatalf("after withdrawal: %d", r.code)
	}
}

// A manage grant with registry at read pulls the target's repos and its
// descendants' and can't push; at write it pushes, untags, deletes and
// creates by pushing, charged to the target and audited under it
// via=manage:<holder>; and once revoked, nothing (local-registry.md §12,
// "manage grants").
func TestManageGrant(t *testing.T) {
	e := newEnv(t, nil)
	prod := e.mk(e.root, "-"+e.root, "prod", nil)
	web := e.mk(e.root, "prod", "web", nil)
	deploy := e.mk(e.root, "-"+e.root, "deploy", nil)
	e.pushImage(prod, "prod/app", "1", "app")
	e.pushImage(web, "prod/web/site", "1", "site")
	grant := func(name, access string, resources ...string) {
		g := e.ok(e.root, "POST", "/silo/v1/silos/prod/manageGrants", obj{"name": name, "holder": "/deploy",
			"scope": obj{"facade": obj{"resources": resources, "access": access}}, "lease": obj{"ttl": "3600s"}})
		e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	}
	pushRefused := func(what string) {
		t.Helper()
		for _, c := range []struct{ method, path string }{
			{"POST", "/v2/prod/app/blobs/uploads/"},
			{"PUT", "/v2/prod/app/manifests/2"},
			{"DELETE", "/v2/prod/app/manifests/1"},
			{"POST", "/v2/prod/new/blobs/uploads/"},
		} {
			if r := e.req(deploy, c.method, c.path, registry.MediaOCIManifest, []byte("{}")); r.code != 404 || r.errCode() != "NAME_UNKNOWN" {
				t.Errorf("%s: %s %s: %d %s", what, c.method, c.path, r.code, r.body)
			}
		}
	}

	pushRefused("no grant")
	if r := e.req(deploy, "GET", "/v2/prod/app/manifests/1", "", nil); r.code != 404 {
		t.Fatalf("no grant pull: %d", r.code)
	}
	// A grant without registry in its scope gives nothing here.
	grant("code", "write", "repos")
	if r := e.req(deploy, "GET", "/v2/prod/app/manifests/1", "", nil); r.code != 404 {
		t.Fatalf("repos grant pull: %d", r.code)
	}
	e.ok(e.root, "DELETE", "/silo/v1/silos/prod/manageGrants/code", nil)

	grant("pull", "read", "registry")
	for _, p := range []string{"/v2/prod/app/manifests/1", "/v2/prod/web/site/manifests/1"} {
		if r := e.req(deploy, "GET", p, "", nil); r.code != 200 {
			t.Fatalf("read grant %s: %d %s", p, r.code, r.body)
		}
	}
	pushRefused("read grant")
	if p := e.audit(fenceapi.KindRegistryPull); len(p) == 0 || p[len(p)-1]["_silo"] != web || p[len(p)-1]["_actor"] != deploy {
		t.Fatalf("pull records: %v", p)
	}
	e.ok(e.root, "DELETE", "/silo/v1/silos/prod/manageGrants/pull", nil)
	if r := e.req(deploy, "GET", "/v2/prod/app/manifests/1", "", nil); r.code != 404 {
		t.Fatalf("after revocation: %d", r.code)
	}

	grant("rollout", "write", "registry")
	before, mine := used(t, e, prod), used(t, e, deploy)
	e.pushImage(deploy, "prod/app", "2", strings.Repeat("x", 4000))
	e.pushImage(deploy, "prod/fresh", "1", "created by the holder")
	if used(t, e, prod) <= before || used(t, e, deploy) != mine {
		t.Fatalf("charges: prod %d -> %d, deploy %d -> %d", before, used(t, e, prod), mine, used(t, e, deploy))
	}
	if r := e.req(deploy, "DELETE", "/v2/prod/app/manifests/1", "", nil); r.code != 202 {
		t.Fatalf("holder untag: %d %s", r.code, r.body)
	}
	if r := e.req(prod, "GET", "/v2/prod/fresh/manifests/1", "", nil); r.code != 200 {
		t.Fatalf("the repo the holder created: %d", r.code)
	}
	var via string
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT via FROM charges WHERE payer = ? AND class = 'disk_gb' AND via != 'facade' LIMIT 1`, prod).Scan(&via)
	})
	if err != nil || !strings.HasPrefix(via, "manage:") {
		t.Fatalf("the charge's via: %q %v", via, err)
	}
	for _, kind := range []string{fenceapi.KindRegistryPush, fenceapi.KindRegistryDelete} {
		l := e.audit(kind)
		if len(l) == 0 {
			t.Fatalf("no %s records", kind)
		}
		last := l[len(l)-1]
		if last["_silo"] != prod || last["_actor"] != deploy {
			t.Errorf("%s: %v", kind, last)
		}
	}
	var vias []string
	err = e.db.Tx(ctx, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT via FROM audit WHERE actor = ? AND kind IN (?, ?)`, deploy, fenceapi.KindRegistryPush, fenceapi.KindRegistryDelete)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			rows.Scan(&v)
			vias = append(vias, v)
		}
		return rows.Err()
	})
	if err != nil || len(vias) == 0 {
		t.Fatalf("vias: %v %v", vias, err)
	}
	for _, v := range vias {
		if v != "manage:"+deploy {
			t.Errorf("via %q, want manage:%s", v, deploy)
		}
	}
}
