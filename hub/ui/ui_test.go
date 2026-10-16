package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

var app = fstest.MapFS{
	"index.html":          {Data: []byte(`<!doctype html><div id="root"></div><script type="module" src="/assets/index-abc.js"></script>`)},
	"assets/index-abc.js": {Data: []byte(`console.log("app")`)},
	"favicon.svg":         {Data: []byte(`<svg/>`)},
}

func get(t *testing.T, h http.Handler, method, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Result()
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHandler(t *testing.T) {
	h := Handler(app)
	// The app's own routes are its page, never cached, under its CSP.
	for _, p := range []string{"/", "/index.html", "/tasks/t-1", "/teams/infra", "/nodes"} {
		r := get(t, h, "GET", p)
		if r.StatusCode != http.StatusOK || !strings.Contains(body(t, r), `id="root"`) {
			t.Errorf("%s: %d, want the app's page", p, r.StatusCode)
		}
		if got := r.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", p, got)
		}
		if got := r.Header.Get("Content-Security-Policy"); !strings.Contains(got, "worker-src 'none'") || !strings.Contains(got, "connect-src 'self'") {
			t.Errorf("%s: CSP %q", p, got)
		}
	}
	r := get(t, h, "GET", "/assets/index-abc.js")
	if r.StatusCode != http.StatusOK || body(t, r) != `console.log("app")` {
		t.Errorf("an asset: %d", r.StatusCode)
	}
	if got := r.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("an asset's Cache-Control %q", got)
	}
	if r := get(t, h, "GET", "/favicon.svg"); r.StatusCode != http.StatusOK || body(t, r) != `<svg/>` {
		t.Errorf("a top-level file: %d", r.StatusCode)
	}
	// A missing asset is missing, not the page: a stale page asking for
	// an old bundle's file must not get HTML as JavaScript.
	if r := get(t, h, "GET", "/assets/gone.js"); r.StatusCode != http.StatusNotFound {
		t.Errorf("a missing asset: %d, want 404", r.StatusCode)
	}
	// The hub's own prefixes are never the app's.
	for _, p := range []string{"/v1/nope", "/auth/x", "/internal/v1/x", "/.well-known/x"} {
		if r := get(t, h, "GET", p); r.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, r.StatusCode)
		}
	}
	if r := get(t, h, "POST", "/"); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d, want 405", r.StatusCode)
	}
	if r := get(t, h, "HEAD", "/"); r.StatusCode != http.StatusOK || body(t, r) != "" {
		t.Errorf("HEAD /: %d", r.StatusCode)
	}
}

// A binary built without the bundle serves the placeholder.
func TestPlaceholder(t *testing.T) {
	r := get(t, Handler(nil), "GET", "/tasks/t-1")
	if b := body(t, r); r.StatusCode != http.StatusOK || !strings.Contains(b, "built without its web UI") {
		t.Errorf("the placeholder: %d %q", r.StatusCode, b)
	}
}

// The bundle, when it was built, has a page that loads a module.
func TestBundle(t *testing.T) {
	b := Bundle()
	if b == nil {
		if os.Getenv("SILO_UI_REQUIRE_BUNDLE") != "" {
			t.Fatal("SILO_UI_REQUIRE_BUNDLE is set, and the bundle is not embedded")
		}
		t.Skip("built without the bundle (ui/: npm run build)")
	}
	r := get(t, Handler(b), "GET", "/")
	if s := body(t, r); r.StatusCode != http.StatusOK || !strings.Contains(s, `type="module"`) {
		t.Errorf("the bundle's page: %d %q", r.StatusCode, s)
	}
}
