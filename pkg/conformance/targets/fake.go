package targets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/fake"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/egress"
	"github.com/bwsalmon/silo/pkg/facade/local/docker"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// Fake is the local backend over its fake drivers, in process.
type Fake struct{}

func (Fake) Name() string { return "local/fake" }

// fakeRegistry is silod as the local backend wires it, but in process:
// SQLite, the fence API, the registry and its cache over
// registry.Memory, and the Docker facade over the fake drivers, each
// caller named by a Silo-Caller header as the gateway names it by
// origin. The cache's upstream fetches go through egress, which judges
// each request by the silo's effective policy, as the gateway's egress
// proxy does, and then sends it to the test upstream whatever its host.
type fakeRegistry struct {
	t       testing.TB
	db      store.Database
	api     *fenceapi.Server
	reg     *registry.Service
	content *registry.Memory
	up      *conformance.Upstream
	upAddr  string
	apiSrv  *httptest.Server
	regSrv  *httptest.Server
	dockSrv *httptest.Server
	root    string

	// With a fence (newFake's withFence): the applier and the ruleset
	// it last loaded.
	applier *netfence.Applier
	nft     *netfence.Fake

	// With a fence: silod's repos, git service and mirror, and the
	// gateway in front (fakegit.go), which a restore replaces; apiH
	// serves the fence API of the silod there is now.
	silod atomic.Pointer[fakeSilod]
	apiH  handlerSwap

	mu sync.Mutex
	// skew is how far Advance has moved the clock on from time.Now.
	skew time.Duration
}

var (
	_ conformance.Registry   = (*fakeRegistry)(nil)
	_ conformance.Structural = (*fakeRegistry)(nil)
)

func (Fake) Registry(t testing.TB) (conformance.Registry, error) {
	f, err := newFake(t, false)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// newFake starts the in-process silod. withFence also gives it an
// address space and the local fence's applier, over netfence.Fake, so
// that probes' checks can be judged by the ruleset it loads.
func newFake(t testing.TB, withFence bool) (*fakeRegistry, error) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "silo.db"))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		return nil, err
	}
	f := &fakeRegistry{t: t, db: db, content: registry.NewMemory(), up: conformance.NewUpstream()}
	upSrv := httptest.NewServer(f.up)
	t.Cleanup(upSrv.Close)
	u, _ := url.Parse(upSrv.URL)
	f.upAddr = u.Host

	opts := fenceapi.Options{Caller: byHeader(fenceapi.ViaOrigin)}
	if withFence {
		f.nft = &netfence.Fake{}
		f.applier = &netfence.Applier{Store: db, NFT: f.nft, Topology: docker.Topology(gatewayAPI, gatewayDocker)}
		opts.Fence, opts.Budget = f.applier, f.applier
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); f.applier.Run(runCtx, 10*time.Millisecond) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	if withFence {
		s, err := f.startSilod(t, db, dir, opts)
		if err != nil {
			return nil, err
		}
		f.silod.Store(s)
		f.api = s.api
	} else {
		f.api = fenceapi.New(db, opts)
		t.Cleanup(func() { f.api.Close() })
	}
	f.apiH.set(f.api)
	f.content.Now = f.now
	f.reg = registry.New(registry.Options{Store: db, API: f.api, Content: f.content, Egress: f, Now: f.now})
	f.api.SetRegistry(f.reg)
	vols := t.TempDir()
	vm, sbx := fake.New(runtime.VM), fake.New(runtime.Sandbox)
	vm.Volumes, sbx.Volumes = vols, vols
	dopts := docker.Options{Drivers: []runtime.Driver{vm, sbx}, Storage: &storage.Dir{Root: vols},
		Fetcher: f.reg, Content: f.content, Registry: f.reg, Login: f.login, Caller: byHeader(fenceapi.ViaInternal), Version: "conformance"}
	if withFence {
		// Probes' images come from probeImages, not through the cache,
		// so that a probe's silo needs no egress to be made.
		dopts.Fetcher, dopts.Images = nil, probeImages{}
	}
	facade := docker.New(db, dopts)
	f.apiSrv = httptest.NewServer(&f.apiH)
	f.regSrv = httptest.NewServer(f.asCaller(f.reg))
	f.dockSrv = httptest.NewServer(facade)
	t.Cleanup(func() { f.dockSrv.Close(); f.regSrv.Close(); f.apiSrv.Close(); facade.Close() })

	boot := map[string]any{
		"backing": map[string]any{"kind": "local"},
		"capacity": map[string]any{"global": map[string]any{"vcpu_m": 32000, "memory_mb": 32768, "disk_gb": 1000,
			"containers": 50, "networks": 20, "volumes": 20, "image_repos": 100}},
		"grant_policy": map[string]any{"exports": "allow"},
		"egress":       map[string]any{"allow": []string{"*"}},
		"admins":       []string{"user:admin@example.com"},
	}
	if withFence {
		boot["grant_policy"] = map[string]any{"exports": "allow", "links": "allow"}
		boot["capacity"].(map[string]any)["global"].(map[string]any)["repos"] = 50
		boot["address_space"], boot["gateway_range"], boot["control_range"] = "10.0.0.0/16", "10.0.255.0/24", "10.0.254.0/24"
	}
	code, root := f.API("", "POST", "/silod/bootstrap", boot)
	if code != 200 {
		return nil, fmt.Errorf("bootstrap: %d %v", code, root)
	}
	f.root = root["id"].(string)
	if code, out := f.API(f.root, "PUT", "/silo/v1/registry/cache", map[string]any{
		"upstreams":     []any{map[string]any{"name": "docker.io", "endpoint": "http://registry.test", "aliases": []string{"index.docker.io"}}},
		"size_limit_gb": 10, "stale_tag_max": "24h", "public_ttl": "24h", "auth_ttl": "300s"}); code != 200 {
		return nil, fmt.Errorf("configuring the cache: %d %v", code, out)
	}
	return f, nil
}

// byHeader is a caller named by the Silo-Caller header, as the gateway
// names it by origin, via via.
func byHeader(via string) func(*http.Request) (fenceapi.Caller, error) {
	return func(r *http.Request) (fenceapi.Caller, error) {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: via}, nil
	}
}

// asCaller serves h with the Silo-Caller header's silo as the caller.
func (f *fakeRegistry) asCaller(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaOrigin}
		h.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), c)))
	})
}

func (f *fakeRegistry) Root() string { return f.root }

func (f *fakeRegistry) API(caller, method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, f.apiSrv.URL+path, bytes.NewReader(b))
	req.Header.Set("Silo-Caller", caller)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (f *fakeRegistry) send(srv *httptest.Server, caller string, req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(srv.URL)
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	req.Host = ""
	req.Header.Set("Silo-Caller", caller)
	return http.DefaultClient.Do(req)
}

func (f *fakeRegistry) Do(caller string, req *http.Request) (*http.Response, error) {
	return f.send(f.regSrv, caller, req)
}

func (f *fakeRegistry) Docker(caller string, req *http.Request) (*http.Response, error) {
	return f.send(f.dockSrv, caller, req)
}

func (f *fakeRegistry) Listen(silo string) string {
	f.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := fenceapi.Caller{Silo: silo, Via: fenceapi.ViaOrigin}
		f.reg.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), c)))
	})}
	go srv.Serve(l)
	f.t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

func (f *fakeRegistry) Upstream() *conformance.Upstream { return f.up }

func (f *fakeRegistry) Audit(kind string) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	ctx := context.Background()
	err := f.db.Tx(ctx, func(tx store.Tx) error {
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
			r := map[string]any{}
			json.Unmarshal([]byte(fields), &r)
			r["_silo"], r["_actor"], r["_result"] = silo, actor, result
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fakeRegistry) HasBlob(digest string) bool { return f.content.Has(digest) }

func (f *fakeRegistry) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Now().Add(f.skew)
}

func (f *fakeRegistry) Advance(d time.Duration) {
	f.mu.Lock()
	f.skew += d
	f.mu.Unlock()
}

func (f *fakeRegistry) CacheBytes() int64 {
	f.t.Helper()
	ev, err := f.reg.Evict(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return ev.CacheBytes
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Transport is the egress proxy as the cache sees it (registry.Egress).
func (f *fakeRegistry) Transport(silo string) http.RoundTripper {
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		if err := f.allowed(r.Context(), silo, r.URL.Hostname()); err != nil {
			return nil, err
		}
		out := r.Clone(r.Context())
		out.Host = r.URL.Host
		out.URL.Host = f.upAddr
		return http.DefaultTransport.RoundTrip(out)
	})
}

func (f *fakeRegistry) allowed(ctx context.Context, silo, host string) error {
	var levels []egress.Policy
	if err := f.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		levels, _, err = fenceapi.EffectiveEgress(ctx, tx, silo)
		return err
	}); err != nil {
		return err
	}
	if ok, _, _ := egress.Allowed(levels, host, 80, egress.DefaultHTTPPort); !ok {
		return &registry.EgressRefusal{Host: host, Reason: "denied by the egress proxy"}
	}
	return nil
}

// login is docker login's check (docker.Options.Login): a token
// request with the credentials, through the egress proxy as silo, that
// keeps nothing.
func (f *fakeRegistry) login(ctx context.Context, silo, server, user, pass string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://auth.test/token?service=registry.test&scope=repository:private/app:pull", nil)
	req.SetBasicAuth(user, pass)
	res, err := f.Transport(silo).RoundTrip(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("unauthorized: incorrect username or password")
	}
	return nil
}
