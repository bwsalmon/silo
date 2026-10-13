package registry_test

import (
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/egress"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
)

// TestBuildersLive serves the registry and its cache on
// $SILO_BUILDERS_ADDR for local-registry.md §13, step 35: kaniko and
// rootless BuildKit, run by hand in containers, push to it and pull their
// base image through it (testdata/builders/run.sh). Every request is
// served as silo team-a/x, as the gateway would attribute a container of
// that silo, and the cache's docker.io is the real Docker Hub, reached
// directly after the silo's egress check (the gateway's proxy isn't in
// this picture). It serves until $SILO_BUILDERS_STOP exists, then checks
// what the builders left: the repos they pushed and the cache's records.
func TestBuildersLive(t *testing.T) {
	addr := os.Getenv("SILO_BUILDERS_ADDR")
	if addr == "" {
		t.Skip("SILO_BUILDERS_ADDR not set; this is run by testdata/builders/run.sh")
	}
	e := newEnvWith(t, nil, obj{"egress": obj{"allow": []string{"*"}}})
	e.reg = registry.New(registry.Options{Store: e.db, API: e.api, Content: e.content, Egress: liveEgress{db: e.db}})
	e.ok(e.root, "PUT", "/silo/v1/registry/cache", obj{
		"upstreams": []obj{{"name": "docker.io", "endpoint": "https://registry-1.docker.io",
			"aliases": []string{"index.docker.io", "registry-1.docker.io"}}},
		"size_limit_gb": 10, "stale_tag_max": "24h", "public_ttl": "24h", "auth_ttl": "300s"})
	hub := []string{"registry-1.docker.io", "auth.docker.io", "*.cloudflare.docker.com", "production.cloudflare.docker.com"}
	e.mk(e.root, "-"+e.root, "team-a", obj{"egress": obj{"allow": hub}})
	x := e.mk(e.root, "team-a", "x", obj{"egress": obj{"allow": hub}})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("%s %s %s", r.Method, r.Host, r.URL)
		e.reg.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), fenceapi.Caller{Silo: x, Via: fenceapi.ViaOrigin})))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	stop := os.Getenv("SILO_BUILDERS_STOP")
	for {
		if _, err := os.Stat(stop); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	for _, kind := range []string{"registry.push", "registry.cache", "registry.denied"} {
		for _, rec := range e.audit(kind) {
			t.Logf("%s: %v", kind, rec)
		}
	}
	t.Logf("repos: %v", e.ok(x, "GET", "/silo/v1/self/imageRepos", nil))
}

// liveEgress is the gateway's egress proxy as the cache sees it, without
// the proxy: the silo's effective policy, then a direct connection.
type liveEgress struct{ db store.Store }

func (p liveEgress) Transport(silo string) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		var levels []egress.Policy
		if err := p.db.Tx(r.Context(), func(tx store.Tx) error {
			var err error
			levels, _, err = fenceapi.EffectiveEgress(r.Context(), tx, silo)
			return err
		}); err != nil {
			return nil, err
		}
		if ok, _, _ := egress.Allowed(levels, r.URL.Hostname(), 443, egress.DefaultConnectPort); !ok {
			return nil, &registry.EgressRefusal{Host: r.URL.Hostname(), Reason: "denied by policy"}
		}
		return http.DefaultTransport.RoundTrip(r)
	})
}
