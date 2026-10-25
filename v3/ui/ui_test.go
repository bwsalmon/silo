package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIHandlerServesEmbeddedAssetsAndCSP(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	// 1. Root index.html
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != ContentSecurityPolicy {
		t.Fatalf("CSP = %q, want %q", csp, ContentSecurityPolicy)
	}
	if xfo := resp.Header.Get("X-Frame-Options"); xfo != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", xfo)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	html := string(bodyBytes)
	for _, want := range []string{
		"view-agents",
		"view-pages",
		"view-session",
		"view-approvals",
		"view-audit",
		"no snapshot available — not undoable",
		"/static/styles.css",
		"/static/app.js",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing expected substring %q", want)
		}
	}

	// 2. /static/app.js
	jsResp, err := http.Get(srv.URL + "/static/app.js")
	if err != nil {
		t.Fatalf("GET /static/app.js: %v", err)
	}
	defer jsResp.Body.Close()
	if jsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/app.js status = %d", jsResp.StatusCode)
	}
	jsBytes, _ := io.ReadAll(jsResp.Body)
	js := string(jsBytes)
	for _, want := range []string{
		"X-Silo-CSRF",
		`target="_blank" rel="noopener noreferrer"`,
		`type="password"`,
		"no snapshot available — not undoable",
		"changes the root agent's own instructions",
		"/api/v1/approvals",
		"/api/v1/secrets/",
		"/api/v1/pages",
		"/api/v1/audit/verify",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing expected substring %q", want)
		}
	}

	// 3. /static/styles.css
	cssResp, err := http.Get(srv.URL + "/static/styles.css")
	if err != nil {
		t.Fatalf("GET /static/styles.css: %v", err)
	}
	defer cssResp.Body.Close()
	if cssResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/styles.css status = %d", cssResp.StatusCode)
	}
}
