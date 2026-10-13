package registry_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/objseq"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// cacheBytes is the cache-only bytes Evict reports, with nothing over
// budget to drop.
func cacheBytes(t *testing.T, e *env) int64 {
	t.Helper()
	ev, err := e.reg.Evict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return ev.CacheBytes
}

// The cache stays within its budget under a pull loop, evicting least
// recently used entries down to 90% of it, and never evicts a blob that
// a silo's image record or a repo references (local-registry.md,
// "Eviction and the cache budget"; §12's first local-only case).
func TestCacheBudgetLRU(t *testing.T) {
	ce := newCacheEnv(t)
	ce.api.SetRegistry(ce.reg)
	const budget = 500
	ce.ok(ce.root, "PUT", "/silo/v1/registry/cache", obj{
		"upstreams":     []obj{{"name": "docker.io", "endpoint": "http://registry.test"}},
		"size_limit_gb": float64(budget) / (1 << 30), "stale_tag_max": "24h", "public_ttl": "24h", "auth_ttl": "300s"})
	var digests []string
	for i := 0; i < 20; i++ {
		b := []byte(fmt.Sprintf("blob %02d of a pull loop, a hundred bytes long %s", i, string(make([]byte, 52))))
		d := registry.Digest("sha256", b)
		ce.up.blobs[d] = b
		digests = append(digests, d)
	}
	// A silo's image record pins blob 0, as a facade pull does: it is
	// charged to that silo, and isn't the cache's to evict.
	pinned := digests[0]
	if err := ce.content.PutImage(ctx, "silo-"+ce.d, registry.Image{Name: "alpine:3", Digest: ce.manifest, Refs: []string{pinned}}); err != nil {
		t.Fatal(err)
	}
	pull := func(d string) {
		t.Helper()
		ce.now = ce.now.Add(time.Second)
		if r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/blobs/"+d, "", nil); r.code != 200 {
			t.Fatalf("GET %s: %d %s", d, r.code, r.body)
		}
	}
	for i, d := range digests {
		pull(d)
		// Blob 1 is used again after every pull: it is never the least
		// recently used.
		if i > 1 {
			pull(digests[1])
		}
		if n := cacheBytes(t, ce.env); n > budget {
			t.Fatalf("after pull %d: %d cache-only bytes, over the budget of %d", i, n, budget)
		}
	}
	if !ce.content.Has(pinned) {
		t.Fatal("eviction removed a blob a silo's image record references")
	}
	if !ce.content.Has(digests[1]) || !ce.content.Has(digests[19]) {
		t.Fatal("eviction removed a recently used blob")
	}
	for _, d := range digests[2:10] {
		if ce.content.Has(d) {
			t.Fatalf("the least recently used blob %s stayed", d)
		}
	}
	// What is cached is still served, and what was evicted is fetched
	// again.
	before := ce.up.count("cdn.test GET /b/" + digests[2])
	pull(digests[2])
	if ce.up.count("cdn.test GET /b/"+digests[2]) != before+1 {
		t.Fatal("an evicted blob was not fetched again")
	}

	// silo registry cache evict drops entries now; the pinned blob's
	// entry goes, but not its bytes.
	out := ce.ok(ce.root, "POST", "/silo/v1/registry/cache:evict", obj{"ref": "docker.io/library/alpine"})
	if out["entries"].(float64) == 0 || out["cache_bytes"] != float64(0) {
		t.Fatalf("evict: %v", out)
	}
	if ce.content.Has(digests[1]) || !ce.content.Has(pinned) {
		t.Fatal("evict: cache-only blobs stay, or a pinned blob went")
	}
	if len(ce.content.Records(registry.CacheNamespace)) != 0 {
		t.Fatalf("silo-cache records after evict: %v", ce.content.Records(registry.CacheNamespace))
	}
	if rec := ce.last(fenceapi.KindRegistryCacheEvict); rec["_silo"] != ce.root {
		t.Fatalf("registry.cache.evict: %v", rec)
	}
	if code, out := ce.call(ce.a, "POST", "/silo/v1/registry/cache:evict", obj{}); code != 403 {
		t.Fatalf("evict by a child: %d %v", code, out)
	}
	if code, out := ce.call(ce.root, "POST", "/silo/v1/registry/cache:evict", obj{"ref": "alpine@x"}); code != 400 {
		t.Fatalf("evict of a bad ref: %d %v", code, out)
	}
}

// Deleting a repo through the fence API removes its silo-registry
// records, so that GC collects its blobs, but not one another repo
// references; and it refunds what it charged. A manifest delete
// refunds at once, and removes its record (local-registry.md, §10).
func TestDeleteCollects(t *testing.T) {
	e := newEnv(t, nil)
	e.api.SetRegistry(e.reg)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	b := e.mk(e.root, "-"+e.root, "team-b", nil)
	shared, _ := e.pushImage(a, "team-a/one", "v1", "a layer both repos have")
	only, m2 := e.pushImage(a, "team-a/one", "v2", strings.Repeat("a layer only team-a/one has ", 400))
	e.pushImage(b, "team-b/two", "v1", "a layer both repos have")
	if len(e.content.Images()) != 3 {
		t.Fatalf("records: %v", e.content.Images())
	}
	charged := used(t, e, a)
	// Past the uploads' grace hour, their leases hold nothing.
	e.now = e.now.Add(registry.DefaultGrace + time.Second)

	// A manifest delete: the record goes, its own layer is collected,
	// and the refund is made now.
	e.ok(a, "DELETE", "/silo/v1/silos/team-a/imageRepos/one/manifests/"+m2, nil)
	if len(e.content.Images()) != 2 || e.content.Has(only) || e.content.Has(m2) {
		t.Fatalf("after the manifest delete: records %v, layer kept %v", e.content.Images(), e.content.Has(only))
	}
	if n := used(t, e, a); n >= charged {
		t.Fatalf("used after the manifest delete: %d, was %d", n, charged)
	}

	// A repo delete: the repo answers 404, its records go, the layer
	// team-b/two references stays, and every byte is refunded.
	op := e.ok(a, "DELETE", "/silo/v1/silos/team-a/imageRepos/one", nil)
	if op["done"] != true {
		t.Fatalf("the delete's operation: %v", op)
	}
	if r := e.req(a, "GET", "/v2/team-a/one/manifests/v1", "", nil); r.code != 404 {
		t.Fatalf("a deleted repo: %d", r.code)
	}
	if len(e.content.Images()) != 1 || !e.content.Has(shared) {
		t.Fatalf("after the repo delete: records %v, shared layer kept %v", e.content.Images(), e.content.Has(shared))
	}
	if n := used(t, e, a); n != 0 {
		t.Fatalf("used after the repo delete: %d", n)
	}
	if r := e.req(b, "GET", "/v2/team-b/two/blobs/"+shared, "", nil); r.code != 200 {
		t.Fatalf("team-b/two's layer: %d", r.code)
	}

	// A repo a delete left DELETING answers 404, and deleting it again
	// finishes it.
	_, m3 := e.pushImage(a, "team-a/three", "v1", "a layer of team-a/three")
	var id string
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		r, err := imagerepos.ByName(ctx, tx, a, "three")
		if err != nil {
			return err
		}
		id = r.ID
		return imagerepos.SetState(ctx, tx, r.ID, imagerepos.StateDeleting)
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := e.req(a, "GET", "/v2/team-a/three/manifests/v1", "", nil); r.code != 404 {
		t.Fatalf("a DELETING repo: %d", r.code)
	}
	e.ok(a, "DELETE", "/silo/v1/silos/team-a/imageRepos/three", nil)
	for _, img := range e.content.Images() {
		if img.Name == id+"@"+m3 {
			t.Fatal("the finished delete left its record")
		}
	}

	// Deleting a silo with force deletes its repos, records included.
	e.pushImage(b, "team-b/four", "v1", "a layer of team-b/four")
	e.ok(e.root, "DELETE", "/silo/v1/silos/team-b?force=true", nil)
	deadline := time.Now().Add(10 * time.Second)
	for len(e.content.Images()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(e.content.Images()) != 0 || e.content.Has(shared) {
		t.Fatalf("after deleting team-b: records %v, shared layer kept %v", e.content.Images(), e.content.Has(shared))
	}
}

// The reconcile re-creates a missing record when the content exists,
// reports image_manifest_missing when it doesn't, and removes a record
// a delete left behind (local-registry.md, §10 "Garbage collection").
func TestRegistryDrift(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	_, m1 := e.pushImage(a, "team-a/app", "v1", "layer one")
	_, m2 := e.pushImage(a, "team-a/app", "v2", "layer two")
	recs := e.content.Images()
	if len(recs) != 2 {
		t.Fatalf("records: %v", recs)
	}
	byDigest := map[string]registry.Image{}
	for _, r := range recs {
		byDigest[r.Digest] = r
	}
	// m2's record and content are both lost; then m1's record is.
	e.now = e.now.Add(registry.DefaultGrace + time.Second)
	e.content.DeleteImage(ctx, registry.Namespace, byDigest[m2].Name)
	e.content.Collect(ctx)
	e.content.DeleteImage(ctx, registry.Namespace, byDigest[m1].Name)
	// And a record whose row a delete removed is left over.
	stale := registry.Image{Name: "ir-gone@" + m1, Digest: m1, Labels: map[string]string{registry.SeqLabel: "1"}}
	e.content.PutImage(ctx, registry.Namespace, stale)

	rep, err := e.reg.Reconcile(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Recreated) != 1 || rep.Recreated[0] != byDigest[m1].Name || len(rep.Missing) != 1 || rep.Missing[0] != byDigest[m2].Name ||
		len(rep.Removed) != 1 || len(rep.Orphans) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if r := e.req(a, "GET", "/v2/team-a/app/manifests/v1", "", nil); r.code != 200 {
		t.Fatalf("v1 after re-creation: %d", r.code)
	}
	if r := e.req(a, "GET", "/v2/team-a/app/manifests/v2", "", nil); r.code != 404 || r.errCode() != "MANIFEST_UNKNOWN" {
		t.Fatalf("v2, whose content is gone: %d %s", r.code, r.body)
	}
	drift, missing := e.audit(registry.KindRegistryDrift), e.audit(registry.KindManifestMissing)
	if len(drift) != 2 || drift[0]["action"] != "recreated" || drift[1]["action"] != "removed" {
		t.Fatalf("registry_drift: %v", drift)
	}
	if len(missing) != 1 || missing[0]["digest"] != m2 || missing[0]["_silo"] != a {
		t.Fatalf("image_manifest_missing: %v", missing)
	}
	// Report-only changes nothing.
	e.content.DeleteImage(ctx, registry.Namespace, byDigest[m1].Name)
	if rep, err := e.reg.Reconcile(ctx, true); err != nil || len(rep.Recreated) != 0 {
		t.Fatalf("report-only: %+v %v", rep, err)
	}
	if len(e.content.Images()) != 0 {
		t.Fatalf("report-only re-created a record: %v", e.content.Images())
	}
}

// A store restored to before a push reports the pushed manifest's
// record as an orphan, and neither serves nor deletes it, however far
// the object sequence goes on (local-registry.md, §4 "Restore"; §12's
// fourth local-only case).
func TestRestoreOrphan(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mk(e.root, "-"+e.root, "team-a", nil)
	e.pushImage(a, "team-a/app", "v1", "pushed before the copy")

	// Copy the store, as a backup does, then push again.
	path := filepath.Join(t.TempDir(), "silo.db")
	copyStore(t, e, path)
	layer, m2 := e.pushImage(a, "team-a/app", "v2", "pushed after the copy")
	var orphan string
	for _, img := range e.content.Images() {
		if img.Digest == m2 {
			orphan = img.Name
		}
	}

	// Restore: the copy, over the same content store.
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db = db
	e.api = fenceapi.New(db, fenceapi.Options{})
	t.Cleanup(e.api.Close)
	e.reg = registry.New(registry.Options{Store: db, API: e.api, Content: e.content, Now: func() time.Time { return e.now }})
	e.api.SetRegistry(e.reg)
	rep, err := e.reg.Reconcile(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0] != orphan || len(rep.Removed) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if o := e.audit(registry.KindRegistryOrphan); len(o) != 1 || o[0]["record"] != orphan {
		t.Fatalf("registry.orphan: %v", o)
	}
	// Not served: the restored store has no row for it.
	if _, _, _, err := e.reg.Pull(ctx, fenceapi.Caller{Silo: a, Via: fenceapi.ViaOrigin}, "team-a/app", m2); err == nil {
		t.Fatal("the orphan was served")
	}
	// Not deleted, now or once the sequence has gone past its label,
	// and its bytes stay.
	if err := db.Tx(ctx, func(tx store.Tx) error { return objseq.Raise(ctx, tx, 1000) }); err != nil {
		t.Fatal(err)
	}
	for _, reportOnly := range []bool{false, false} {
		if rep, err := e.reg.Reconcile(ctx, reportOnly); err != nil || len(rep.Removed) != 0 || len(rep.Orphans) != 1 {
			t.Fatalf("a later reconcile: %+v %v", rep, err)
		}
	}
	if len(e.content.Images()) != 2 || !e.content.Has(layer) || !e.content.Has(m2) {
		t.Fatalf("the orphan's record or bytes went: %v", e.content.Images())
	}
	if o := e.audit(registry.KindRegistryOrphan); len(o) != 1 {
		t.Fatalf("registry.orphan reported again: %v", o)
	}
}

// copyStore copies e's store to path, as a backup does.
func copyStore(t *testing.T, e *env, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
}
