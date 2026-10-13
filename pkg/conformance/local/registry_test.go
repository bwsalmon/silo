// Package local holds the conformance suite's local-only structural
// cases (local-backend.md §13, call L24), against the local targets:
// the registry's, of local-registry.md §12, through a look at
// silo-containerd's content store (conformance.Structural), and the
// fence's (fence_test.go): no way from silo-net into the root
// namespace, and nothing through a port with no pin.
//
// §12's other local-only cases are still where their steps put them,
// as they need a real data directory, a restored store, real uids or a
// real gVisor: wiping cache/ in pkg/backend/local/datadir
// (TestWipingCacheKeepsContainerd), the restored store's orphan in
// pkg/registry (TestRestoreOrphan), the host front door's uids in
// pkg/hostdoor, and the builder recipes in pkg/registry's
// TestBuildersLive (testdata/builders/run.sh).
package local_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

type obj = map[string]any

// each runs fn against the local targets' registries.
func each(t *testing.T, fn func(t *testing.T, r conformance.R, s conformance.Structural)) {
	var local []conformance.Target
	for _, tg := range targets.All() {
		if strings.HasPrefix(tg.Name(), "local/") {
			local = append(local, tg)
		}
	}
	conformance.ForEach(t, local, func(t *testing.T, reg conformance.Registry) {
		s, ok := reg.(conformance.Structural)
		if !ok {
			t.Fatal("a local target's registry offers no look at its content store")
		}
		fn(t, conformance.R{Registry: reg, T: t}, s)
	})
}

// The cache stays within its budget under a pull loop, evicts least
// recently used, and never evicts a blob a repo references.
func TestCacheBudget(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R, s conformance.Structural) {
		const budget = 500
		r.OK(r.Root(), "PUT", "/silo/v1/registry/cache", obj{
			"upstreams":     []obj{{"name": "docker.io", "endpoint": "http://registry.test"}},
			"size_limit_gb": float64(budget) / (1 << 30), "stale_tag_max": "24h", "public_ttl": "24h", "auth_ttl": "300s"})
		a := r.Mk("", "team-a", obj{"egress": obj{"allow": conformance.Hosts}})
		up := r.Upstream()
		var digests []string
		for i := 0; i < 20; i++ {
			digests = append(digests, up.AddBlob([]byte(fmt.Sprintf("blob %02d of a pull loop, a hundred bytes long%s", i, strings.Repeat(".", 54)))))
		}
		pull := func(d string) {
			t.Helper()
			if res := r.Req(a, "GET", "/v2/docker.io/library/alpine/blobs/"+d, "", nil); res.Code != 200 {
				t.Fatalf("GET %s: %d %s", d, res.Code, res.Body)
			}
		}
		// Blob 0 is pushed to a's own repo too: a repo references it.
		pinned := digests[0]
		pull(pinned)
		r.Push(a, "team-a/keep", "1", conformance.NewImage(`{"os":"linux"}`, fmt.Sprintf("blob %02d of a pull loop, a hundred bytes long%s", 0, strings.Repeat(".", 54))))
		// Blob 1 is used on every round: the most recently used.
		hot := digests[1]
		for _, d := range digests[2:] {
			pull(d)
			pull(hot)
			if n := s.CacheBytes(); n > budget {
				t.Fatalf("the cache holds %d bytes, over its %d", n, budget)
			}
		}
		if !s.HasBlob(pinned) {
			t.Error("the cache evicted a blob a repo references")
		}
		if !s.HasBlob(hot) {
			t.Error("the cache evicted the most recently used blob")
		}
		for _, d := range digests[2:6] {
			if s.HasBlob(d) {
				t.Errorf("the least recently used blob %s is still held", d)
			}
		}
	})
}

// After deleting a repo, its blobs are gone from the content store once
// nothing else references them, and a blob another repo references
// stays.
func TestRepoDeleteCollects(t *testing.T) {
	each(t, func(t *testing.T, r conformance.R, s conformance.Structural) {
		a := r.Mk("", "team-a", nil)
		shared := conformance.NewImage(`{"os":"linux","shared":1}`, "the shared layer")
		own := conformance.NewImage(`{"os":"linux","shared":1}`, "only the doomed repo's layer")
		r.Push(a, "team-a/doomed", "1", own)
		r.Push(a, "team-a/doomed", "2", shared)
		r.Push(a, "team-a/kept", "1", shared)
		// Past the uploads' grace hour, their leases hold nothing.
		s.Advance(time.Hour + time.Minute)
		r.OK(a, "DELETE", "/silo/v1/self/imageRepos/doomed", nil)
		if s.HasBlob(own.LayerD) || s.HasBlob(own.ManifestD) {
			t.Error("the deleted repo's own blobs are still in the content store")
		}
		for _, d := range []string{shared.LayerD, shared.ConfigD, shared.ManifestD} {
			if !s.HasBlob(d) {
				t.Errorf("%s, which another repo references, was collected", d)
			}
		}
		if res := r.Req(a, "GET", "/v2/team-a/kept/blobs/"+shared.LayerD, "", nil); res.Code != 200 {
			t.Errorf("the kept repo's layer: %d", res.Code)
		}
	})
}
