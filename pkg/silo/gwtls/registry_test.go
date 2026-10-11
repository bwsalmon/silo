package gwtls_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/gwtls"
)

// TestRegistry: gateway VMs read silod's image, and nothing else,
// through silod, which adds its own token.
func TestRegistry(t *testing.T) {
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Cookie"))
		io.WriteString(w, "manifest")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	reg := &gwtls.Registry{Repository: u.Host + "/p/silo/silod", Scheme: "http",
		Token: func(context.Context) (string, error) { return "silod-token", nil }}
	srv := httptest.NewServer(reg.Handler())
	defer srv.Close()
	do := func(method, path string) int {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer someone-else")
		req.Header.Set("Cookie", "x=y")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do("GET", "/v2/"); c != 200 {
		t.Errorf("GET /v2/: %d", c)
	}
	if c := do("GET", "/v2/p/silo/silod/manifests/sha256:abc"); c != 200 {
		t.Errorf("the image's manifest: %d", c)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v2/p/silo/other/manifests/latest"},
		{"GET", "/v2/q/silo/silod/manifests/latest"},
		{"GET", "/v2/p/silo/silod/tags/list"},
		{"GET", "/v2/_catalog"},
		{"PUT", "/v2/p/silo/silod/manifests/latest"},
		{"POST", "/v2/p/silo/silod/blobs/uploads/"},
	} {
		if code := do(c.method, c.path); code < 400 {
			t.Errorf("%s %s: %d, want refused", c.method, c.path, code)
		}
	}
	if len(got) != 1 || got[0] != "GET /v2/p/silo/silod/manifests/sha256:abc Bearer silod-token " {
		t.Errorf("upstream saw %q", got)
	}
}
