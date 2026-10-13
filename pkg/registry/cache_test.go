package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/egress"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
)

// testUpstream is a registry at registry.test that wants an anonymous
// token from auth.test, and redirects every blob GET to a second host,
// cdn (cdn.test unless a test moves it), which serves the bytes.
type testUpstream struct {
	mu        sync.Mutex
	manifests map[string][]byte // by tag and by digest
	blobs     map[string][]byte
	cdn       string
	down      bool
	// requests counts requests by "host method path".
	requests map[string]int
	// hold, if set, holds blob GETs on the CDN until it is closed, and
	// held is told of each.
	hold chan struct{}
	held chan struct{}
}

func (u *testUpstream) count(k string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[k]
}

func (u *testUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	// The API host's requests count once they carry the token.
	if r.Host != "registry.test" || r.Header.Get("Authorization") != "" {
		u.requests[r.Host+" "+r.Method+" "+r.URL.Path]++
	}
	cdn, hold, held := u.cdn, u.hold, u.held
	u.mu.Unlock()
	switch {
	case r.Host == "auth.test" && r.URL.Path == "/token":
		if r.URL.Query().Get("service") != "registry.test" || !strings.HasPrefix(r.URL.Query().Get("scope"), "repository:library/alpine:pull") {
			http.Error(w, "bad token request "+r.URL.RawQuery, 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "anon-token"})
	case r.Host == "registry.test":
		if r.Header.Get("Authorization") != "Bearer anon-token" {
			w.Header().Set("Www-Authenticate", `Bearer realm="http://auth.test/token",service="registry.test",scope="repository:library/alpine:pull"`)
			w.WriteHeader(401)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/v2/library/alpine/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if ref, ok := strings.CutPrefix(rest, "manifests/"); ok {
			u.mu.Lock()
			b, ok := u.manifests[ref]
			u.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", registry.MediaOCIManifest)
			w.Header().Set("Docker-Content-Digest", registry.Digest("sha256", b))
			if r.Method == "HEAD" {
				return
			}
			w.Write(b)
			return
		}
		http.Redirect(w, r, "http://"+cdn+"/b/"+strings.TrimPrefix(rest, "blobs/")+"?sig=x", http.StatusTemporaryRedirect)
	case r.Host == cdn && strings.HasPrefix(r.URL.Path, "/b/"):
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "the token went to the CDN", 400)
			return
		}
		if hold != nil {
			held <- struct{}{}
			<-hold
		}
		u.mu.Lock()
		b, ok := u.blobs[strings.TrimPrefix(r.URL.Path, "/b/")]
		u.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	default:
		http.Error(w, "no such host "+r.Host, 502)
	}
}

// testEgress is the gateway's egress proxy as the cache sees it: every
// request, as the silo, is judged by the silo's effective policy, and
// then sent to the test upstream's listeners whatever its host.
type testEgress struct {
	db   store.Store
	up   *testUpstream
	addr string
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (p *testEgress) Transport(silo string) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		var levels []egress.Policy
		if err := p.db.Tx(r.Context(), func(tx store.Tx) error {
			var err error
			levels, _, err = fenceapi.EffectiveEgress(r.Context(), tx, silo)
			return err
		}); err != nil {
			return nil, err
		}
		host := r.URL.Hostname()
		if ok, _, _ := egress.Allowed(levels, host, 80, egress.DefaultHTTPPort); !ok {
			return nil, &registry.EgressRefusal{Host: host, Reason: "denied by the test proxy"}
		}
		p.up.mu.Lock()
		down := p.up.down
		p.up.mu.Unlock()
		if down && host == "registry.test" {
			return nil, errors.New("connection refused")
		}
		out := r.Clone(r.Context())
		out.Host = r.URL.Host
		out.URL.Host = p.addr
		return http.DefaultTransport.RoundTrip(out)
	})
}

// cacheEnv is env with the cache's upstream docker.io at the test
// upstream, which holds library/alpine:3, and silos:
//
//	a  allows registry.test, auth.test and cdn.test
//	b  allows nothing
//	c  allows registry.test and auth.test, not the CDN
//	d  allows what a does
type cacheEnv struct {
	*env
	up              *testUpstream
	a, b, c, d      string
	manifest, layer string
	config          string
	layerB, configB []byte
}

func newCacheEnv(t *testing.T) *cacheEnv {
	t.Helper()
	e := newEnvWith(t, nil, obj{"egress": obj{"allow": []string{"*"}}})
	ce := &cacheEnv{env: e, layerB: []byte("the alpine layer"), configB: []byte(`{"architecture":"amd64","os":"linux"}`)}
	ce.layer = registry.Digest("sha256", ce.layerB)
	ce.config = registry.Digest("sha256", ce.configB)
	m := image(ce.config, len(ce.configB), map[string]int{ce.layer: len(ce.layerB)})
	ce.manifest = registry.Digest("sha256", m)
	ce.up = &testUpstream{cdn: "cdn.test", requests: map[string]int{},
		manifests: map[string][]byte{"3": m, ce.manifest: m},
		blobs:     map[string][]byte{ce.layer: ce.layerB, ce.config: ce.configB}}
	srv := httptest.NewServer(ce.up)
	t.Cleanup(srv.Close)
	su, _ := url.Parse(srv.URL)
	e.reg = registry.New(registry.Options{Store: e.db, API: e.api, Content: e.content, Now: func() time.Time { return e.now },
		Egress: &testEgress{db: e.db, up: ce.up, addr: su.Host}})
	e.ok(e.root, "PUT", "/silo/v1/registry/cache", obj{
		"upstreams":     []obj{{"name": "docker.io", "endpoint": "http://registry.test", "aliases": []string{"index.docker.io"}}},
		"size_limit_gb": 10, "stale_tag_max": "24h", "public_ttl": "24h", "auth_ttl": "300s"})
	all := []string{"registry.test", "auth.test", "cdn.test"}
	ce.a = e.mk(e.root, "-"+e.root, "team-a", obj{"egress": obj{"allow": all}})
	ce.b = e.mk(e.root, "-"+e.root, "team-b", obj{"egress": obj{"allow": []string{}}})
	ce.c = e.mk(e.root, "-"+e.root, "team-c", obj{"egress": obj{"allow": all[:2]}})
	ce.d = e.mk(e.root, "-"+e.root, "team-d", obj{"egress": obj{"allow": all}})
	return ce
}

func (ce *cacheEnv) last(kind string) obj {
	ce.t.Helper()
	recs := ce.audit(kind)
	if len(recs) == 0 {
		ce.t.Fatalf("no %s record", kind)
	}
	return recs[len(recs)-1]
}

// A pull through the cache: a miss fetches through the egress proxy as
// the caller, with the token exchange and the blob's redirect, and
// records the hosts it used; a second pull by tag revalidates with a
// HEAD and fetches nothing; Docker Hub's names and ?ns= reach the same
// entries.
func TestCachePull(t *testing.T) {
	ce := newCacheEnv(t)
	r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/manifests/3", "", nil)
	if r.code != 200 || r.header.Get("Docker-Content-Digest") != ce.manifest || r.header.Get("Content-Type") != registry.MediaOCIManifest {
		t.Fatalf("GET alpine:3: %d %v %s", r.code, r.header, r.body)
	}
	rec := ce.last(fenceapi.KindRegistryCache)
	if rec["result"] != "miss" || rec["auth"] != "anonymous" || rec["egress"] != "allowed" || rec["_silo"] != ce.a ||
		rec["upstream"] != "docker.io" || rec["repo"] != "library/alpine" || rec["digest"] != ce.manifest {
		t.Fatalf("the miss's record: %v", rec)
	}
	if hosts := rec["hosts"].([]any); len(hosts) != 2 || hosts[0] != "registry.test:80" || hosts[1] != "auth.test:80" {
		t.Fatalf("a manifest's hosts: %v", hosts)
	}
	r = ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/blobs/"+ce.layer, "", nil)
	if r.code != 200 || string(r.body) != string(ce.layerB) {
		t.Fatalf("GET the layer: %d %s", r.code, r.body)
	}
	rec = ce.last(fenceapi.KindRegistryCache)
	if hosts := rec["hosts"].([]any); rec["result"] != "miss" || len(hosts) != 3 || hosts[2] != "cdn.test:80" {
		t.Fatalf("a blob's record: %v", rec)
	}

	// Again, by tag: one HEAD, no second GET.
	if r := ce.req(ce.d, "GET", "/v2/docker.io/library/alpine/manifests/3", "", nil); r.code != 200 {
		t.Fatalf("GET alpine:3 again: %d %s", r.code, r.body)
	}
	if rec := ce.last(fenceapi.KindRegistryCache); rec["result"] != "hit" || rec["_silo"] != ce.d {
		t.Fatalf("the hit's record: %v", rec)
	}
	if n := ce.up.count("registry.test GET /v2/library/alpine/manifests/" + ce.manifest); n != 1 {
		t.Fatalf("manifest GETs: %d, want 1", n)
	}
	if n := ce.up.count("registry.test HEAD /v2/library/alpine/manifests/3"); n < 2 {
		t.Fatalf("manifest HEADs: %d, want one per pull by tag", n)
	}
	// By digest: no upstream request at all.
	heads := ce.up.count("registry.test HEAD /v2/library/alpine/manifests/3")
	for _, p := range []string{
		"/v2/docker.io/library/alpine/manifests/" + ce.manifest,
		"/v2/index.docker.io/alpine/manifests/" + ce.manifest,
		"/v2/library/alpine/manifests/" + ce.manifest + "?ns=docker.io",
		"/v2/alpine/blobs/" + ce.layer + "?ns=index.docker.io",
	} {
		if r := ce.req(ce.d, "GET", p, "", nil); r.code != 200 {
			t.Fatalf("GET %s: %d %s", p, r.code, r.body)
		}
	}
	if n := ce.up.count("registry.test HEAD /v2/library/alpine/manifests/3"); n != heads {
		t.Fatalf("a pull by digest revalidated")
	}
	if n := ce.up.count("cdn.test GET /b/" + ce.layer); n != 1 {
		t.Fatalf("CDN GETs: %d, want 1", n)
	}

	// Not an upstream: NAME_UNKNOWN; and the cache is read-only.
	if r := ce.req(ce.a, "GET", "/v2/quay.example/x/manifests/1", "", nil); r.code != 404 || r.errCode() != "NAME_UNKNOWN" {
		t.Fatalf("an unconfigured upstream: %d %s", r.code, r.body)
	}
	if r := ce.req(ce.a, "PUT", "/v2/docker.io/library/alpine/manifests/4", registry.MediaOCIManifest, []byte("{}")); r.code != 405 {
		t.Fatalf("a push to the cache: %d %s", r.code, r.body)
	}
	if r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/manifests/9", "", nil); r.code != 404 || r.errCode() != "MANIFEST_UNKNOWN" {
		t.Fatalf("a tag the upstream doesn't have: %d %s", r.code, r.body)
	}
}

// Egress is checked on every request, not only on misses: a silo whose
// egress denies the upstream gets 403 for an image another silo pulled,
// though the cache needn't contact the upstream to serve it.
func TestCacheHitDeniedByEgress(t *testing.T) {
	ce := newCacheEnv(t)
	for _, p := range []string{"manifests/3", "blobs/" + ce.layer} {
		if r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/"+p, "", nil); r.code != 200 {
			t.Fatalf("a: GET %s: %d %s", p, r.code, r.body)
		}
	}
	for _, p := range []string{"manifests/" + ce.manifest, "manifests/3", "blobs/" + ce.layer} {
		r := ce.req(ce.b, "GET", "/v2/docker.io/library/alpine/"+p, "", nil)
		if r.code != 403 || r.errCode() != "DENIED" || !strings.Contains(string(r.body), "egress to registry.test is not allowed for /team-b") {
			t.Fatalf("b: GET %s: %d %s", p, r.code, r.body)
		}
	}
	rec := ce.last(fenceapi.KindRegistryCache)
	if rec["egress"] != "denied" || rec["denied_host"] != "registry.test" || rec["_silo"] != ce.b || rec["_result"] != "DENIED" {
		t.Fatalf("the refusal's record: %v", rec)
	}
	if d := ce.last(fenceapi.KindRegistryDenied); d["reason"] != "egress" || d["_silo"] != ce.b {
		t.Fatalf("registry.denied: %v", d)
	}
}

// Denying only the blob redirect host refuses blobs, not manifests;
// and a refusal caused only by a recorded redirect host is re-checked
// against the hosts the upstream redirects through now.
func TestCacheRedirectHost(t *testing.T) {
	ce := newCacheEnv(t)
	if r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/blobs/"+ce.layer, "", nil); r.code != 200 {
		t.Fatalf("a: GET the layer: %d %s", r.code, r.body)
	}
	for _, ref := range []string{"3", ce.manifest} {
		if r := ce.req(ce.c, "GET", "/v2/docker.io/library/alpine/manifests/"+ref, "", nil); r.code != 200 {
			t.Fatalf("c: GET manifest %s: %d %s", ref, r.code, r.body)
		}
	}
	// A hit and a miss alike.
	for _, d := range []string{ce.layer, ce.config} {
		r := ce.req(ce.c, "GET", "/v2/docker.io/library/alpine/blobs/"+d, "", nil)
		if r.code != 403 || !strings.Contains(string(r.body), "egress to cdn.test is not allowed for /team-c") {
			t.Fatalf("c: GET blob %s: %d %s", d, r.code, r.body)
		}
	}

	// The CDN moves to cdn2.test, which c may reach and a may not.
	ce.up.mu.Lock()
	ce.up.cdn = "cdn2.test"
	ce.up.mu.Unlock()
	ce.ok(ce.root, "PATCH", "/silo/v1/silos/team-c", obj{"egress": obj{"allow": []string{"registry.test", "auth.test", "cdn2.test"}}})
	if r := ce.req(ce.c, "GET", "/v2/docker.io/library/alpine/blobs/"+ce.layer, "", nil); r.code != 200 || string(r.body) != string(ce.layerB) {
		t.Fatalf("c, after the move: %d %s", r.code, r.body)
	}
	if hosts := ce.last(fenceapi.KindRegistryCache)["hosts"].([]any); hosts[len(hosts)-1] != "cdn2.test:80" {
		t.Fatalf("the re-checked hosts: %v", hosts)
	}
	r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/blobs/"+ce.layer, "", nil)
	if r.code != 403 || !strings.Contains(string(r.body), "egress to cdn2.test is not allowed for /team-a") {
		t.Fatalf("a, after the move: %d %s", r.code, r.body)
	}
	if n := ce.up.count("cdn2.test GET /b/" + ce.layer); n != 1 {
		t.Fatalf("the re-check read the body, or didn't happen: %d", n)
	}
}

// A silo with an empty egress policy still pushes to and pulls from
// its own local repo: local repos are not egress.
func TestCacheLocalRepoNoEgress(t *testing.T) {
	ce := newCacheEnv(t)
	layer, md := ce.pushImage(ce.b, "team-b/app", "1", "a local layer")
	if r := ce.req(ce.b, "GET", "/v2/team-b/app/manifests/1", "", nil); r.code != 200 || r.header.Get("Docker-Content-Digest") != md {
		t.Fatalf("b: its own manifest: %d %s", r.code, r.body)
	}
	if r := ce.req(ce.b, "GET", "/v2/team-b/app/blobs/"+layer, "", nil); r.code != 200 || string(r.body) != "a local layer" {
		t.Fatalf("b: its own layer: %d %s", r.code, r.body)
	}
	if n := len(ce.audit(fenceapi.KindRegistryCache)); n != 0 {
		t.Fatalf("a local pull made %d registry.cache records", n)
	}
}

// Concurrent misses on one blob, from two silos allowed to have it,
// wait on one upstream fetch.
func TestCacheSingleFlight(t *testing.T) {
	ce := newCacheEnv(t)
	ce.up.mu.Lock()
	ce.up.hold, ce.up.held = make(chan struct{}), make(chan struct{}, 4)
	ce.up.mu.Unlock()
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, silo := range []string{ce.a, ce.d} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = ce.req(silo, "GET", "/v2/docker.io/library/alpine/blobs/"+ce.layer, "", nil).code
		}()
		if i == 0 {
			<-ce.up.held
		}
	}
	time.Sleep(200 * time.Millisecond)
	close(ce.up.hold)
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("codes: %v", codes)
	}
	if n := ce.up.count("cdn.test GET /b/" + ce.layer); n != 1 {
		t.Fatalf("CDN GETs: %d, want 1", n)
	}
}

// When the upstream can't be reached, a tag is served from the cache
// if it was revalidated less than stale_tag_max ago, and is 503 after
// that; a digest is served without asking the upstream.
func TestCacheStaleTag(t *testing.T) {
	ce := newCacheEnv(t)
	if r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/manifests/3", "", nil); r.code != 200 {
		t.Fatalf("GET alpine:3: %d %s", r.code, r.body)
	}
	ce.up.mu.Lock()
	ce.up.down = true
	ce.up.mu.Unlock()
	ce.now = ce.now.Add(23 * time.Hour)
	if r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/manifests/3", "", nil); r.code != 200 || r.header.Get("Docker-Content-Digest") != ce.manifest {
		t.Fatalf("a stale tag: %d %s", r.code, r.body)
	}
	if rec := ce.last(fenceapi.KindRegistryCache); rec["result"] != "stale" {
		t.Fatalf("the stale pull's record: %v", rec)
	}
	// Stale is still held to egress.
	if r := ce.req(ce.b, "GET", "/v2/docker.io/library/alpine/manifests/3", "", nil); r.code != 403 {
		t.Fatalf("b, a stale tag: %d %s", r.code, r.body)
	}
	ce.now = ce.now.Add(2 * time.Hour)
	if r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/manifests/3", "", nil); r.code != 503 {
		t.Fatalf("a tag past stale_tag_max: %d %s", r.code, r.body)
	}
	if r := ce.req(ce.a, "GET", "/v2/docker.io/library/alpine/manifests/"+ce.manifest, "", nil); r.code != 200 {
		t.Fatalf("by digest, the upstream down: %d %s", r.code, r.body)
	}
}

// The facade's pull goes through the same fetcher and rules.
func TestCachePullThrough(t *testing.T) {
	ce := newCacheEnv(t)
	c := fenceapi.Caller{Silo: ce.a, Via: fenceapi.ViaOrigin}
	mt, b, d, err := ce.reg.PullThrough(context.Background(), c, "docker.io/library/alpine", "3", "pull-1")
	if err != nil || d != ce.manifest || mt != registry.MediaOCIManifest || registry.Digest("sha256", b) != d {
		t.Fatalf("PullThrough: %v %s %s", err, d, mt)
	}
	if err := ce.reg.FetchThrough(context.Background(), c, "docker.io/library/alpine", ce.layer, "pull-1"); err != nil {
		t.Fatal(err)
	}
	if rec := ce.last(fenceapi.KindRegistryCache); rec["pull_id"] != "pull-1" || rec["object"] != "blob" {
		t.Fatalf("the facade fetch's record: %v", rec)
	}
	cc := fenceapi.Caller{Silo: ce.c, Via: fenceapi.ViaOrigin}
	var re *registry.Error
	if err := ce.reg.FetchThrough(context.Background(), cc, "docker.io/library/alpine", ce.layer, "pull-2"); !errors.As(err, &re) || re.Status != 403 {
		t.Fatalf("c: FetchThrough: %v", err)
	}
}

// The index is per sharing domain (call L73): after a shared-domain
// pull, an isolated silo's pull of the same image makes an upstream
// request for every blob, though the bytes are on disk; a silo inside
// that isolated subtree then hits on its fetches, the rest of the host
// doesn't see them, a nested isolated silo misses again, and facade
// pulls follow the same rule.
func TestCacheSharingDomains(t *testing.T) {
	ce := newCacheEnv(t)
	all := obj{"allow": []string{"registry.test", "auth.test", "cdn.test"}}
	i := ce.mk(ce.root, "-"+ce.root, "team-i", obj{"egress": all, "images": obj{"sharing": "isolated"}})
	ix := ce.mk(ce.root, "team-i", "x", obj{"egress": all})
	iy := ce.mk(ce.root, "team-i", "y", obj{"egress": all, "images": obj{"sharing": "isolated"}})
	j := ce.mk(ce.root, "team-a", "j", obj{"egress": all, "images": obj{"sharing": "isolated"}})
	blobs := []string{ce.config, ce.layer}
	pull := func(silo, ref, want string) {
		t.Helper()
		if r := ce.req(silo, "GET", "/v2/docker.io/library/alpine/manifests/"+ref, "", nil); r.code != 200 {
			t.Fatalf("GET alpine:%s: %d %s", ref, r.code, r.body)
		}
		for _, b := range blobs {
			r := ce.req(silo, "GET", "/v2/docker.io/library/alpine/blobs/"+b, "", nil)
			if r.code != 200 {
				t.Fatalf("GET %s: %d %s", b, r.code, r.body)
			}
			if rec := ce.last(fenceapi.KindRegistryCache); rec["result"] != want || rec["_silo"] != silo {
				t.Fatalf("%s's record: %v, want %s", b, rec, want)
			}
		}
	}
	// fetches is how many times the upstream has sent the manifest and
	// each blob.
	fetches := func(want int) {
		t.Helper()
		if n := ce.up.count("registry.test GET /v2/library/alpine/manifests/" + ce.manifest); n != want {
			t.Errorf("manifest GETs: %d, want %d", n, want)
		}
		for _, b := range blobs {
			if n := ce.up.count("cdn.test GET /b/" + b); n != want {
				t.Errorf("%s: CDN GETs: %d, want %d", b, n, want)
			}
		}
	}

	pull(ce.a, "3", "miss")
	fetches(1)
	pull(i, "3", "miss")
	fetches(2)
	if rec := ce.last(fenceapi.KindRegistryCache); rec["domain"] != "/team-i" {
		t.Fatalf("an isolated silo's record: %v", rec)
	}
	// A silo that is isolated only by its parent is in the parent's
	// domain.
	pull(ix, ce.manifest, "hit")
	fetches(2)
	if rec := ce.last(fenceapi.KindRegistryCache); rec["domain"] != "/team-i" {
		t.Fatalf("the inherited domain's record: %v", rec)
	}
	// The isolated domain's fetches make no hits outside it.
	pull(ce.d, ce.manifest, "hit")
	fetches(2)
	if rec := ce.last(fenceapi.KindRegistryCache); rec["domain"] != "/" {
		t.Fatalf("the host's record: %v", rec)
	}
	// Nested isolation is a domain of its own.
	pull(iy, ce.manifest, "miss")
	fetches(3)

	// The facade's pull: the same rule.
	c := fenceapi.Caller{Silo: j, Via: fenceapi.ViaOrigin}
	if _, _, d, err := ce.reg.PullThrough(context.Background(), c, "docker.io/library/alpine", "3", "pull-j"); err != nil || d != ce.manifest {
		t.Fatalf("PullThrough: %v %s", err, d)
	}
	for _, b := range blobs {
		if err := ce.reg.FetchThrough(context.Background(), c, "docker.io/library/alpine", b, "pull-j"); err != nil {
			t.Fatal(err)
		}
		if rec := ce.last(fenceapi.KindRegistryCache); rec["result"] != "miss" || rec["domain"] != "/team-a/j" {
			t.Fatalf("the facade fetch's record: %v", rec)
		}
	}
	fetches(4)
}

// The configuration is the root's: shown with install's defaults until
// set, set only by root members, and checked.
func TestCacheSettings(t *testing.T) {
	e := newEnv(t, nil)
	got := e.ok(e.root, "GET", "/silo/v1/registry/cache", nil)
	if ups := got["upstreams"].([]any); len(ups) != 4 || got["stale_tag_max"] != "24h" {
		t.Fatalf("the defaults: %v", got)
	}
	x := e.mk(e.root, "-"+e.root, "team-a", nil)
	if code, out := e.call(x, "GET", "/silo/v1/registry/cache", nil); code != 403 {
		t.Fatalf("a child's GET: %d %v", code, out)
	}
	if code, out := e.call(x, "PUT", "/silo/v1/registry/cache", obj{"upstreams": []obj{}}); code != 403 {
		t.Fatalf("a child's PUT: %d %v", code, out)
	}
	for _, bad := range []obj{
		{"upstreams": []obj{{"name": "nodot", "endpoint": "https://x.io"}}, "stale_tag_max": "1h", "public_ttl": "1h", "auth_ttl": "1s"},
		{"upstreams": []obj{{"name": "x.io", "endpoint": "https://x.io/path"}}, "stale_tag_max": "1h", "public_ttl": "1h", "auth_ttl": "1s"},
		{"upstreams": []obj{{"name": "x.io", "endpoint": "https://x.io"}}, "stale_tag_max": "soon", "public_ttl": "1h", "auth_ttl": "1s"},
	} {
		if code, out := e.call(e.root, "PUT", "/silo/v1/registry/cache", bad); code != 400 {
			t.Fatalf("PUT %v: %d %v", bad, code, out)
		}
	}
	set := obj{"upstreams": []obj{{"name": "ghcr.io", "endpoint": "https://ghcr.io"}}, "size_limit_gb": 5,
		"stale_tag_max": "1h", "public_ttl": "1h", "auth_ttl": "60s"}
	e.ok(e.root, "PUT", "/silo/v1/registry/cache", set)
	if got := e.ok(e.root, "GET", "/silo/v1/registry/cache", nil); len(got["upstreams"].([]any)) != 1 || got["stale_tag_max"] != "1h" {
		t.Fatalf("after PUT: %v", got)
	}
	if recs := e.audit(fenceapi.KindRegistryCacheSet); len(recs) != 1 {
		t.Fatalf("registry.cache.set records: %v", recs)
	}
}
