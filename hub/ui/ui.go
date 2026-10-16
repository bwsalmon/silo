// Package ui serves the hub's human web UI (docs/ui.md, stage U1): a
// single-page app, built from ui/ in this repository by Vite into
// static/app, and embedded in the hub binary. It is a client of the
// hub's public API only, from the same origin as /v1/..., so it needs
// no CORS and forwards no credential.
//
// static/app is not committed: grain CI builds it (.grain/workflows/ui.yml,
// and the Dockerfile for the image). A Go build without it, by anyone
// without Node, embeds static/placeholder.html instead, which says how
// to build the real one.
package ui

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"testing/fstest"
)

//go:embed static
var static embed.FS

// Bundle is the built app, or nil when the binary was built without
// it.
func Bundle() fs.FS {
	app, err := fs.Sub(static, "static/app")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(app, "index.html"); err != nil {
		return nil
	}
	return app
}

// placeholder is the page served in place of a bundle that was not
// built, as the only file of an app.
func placeholder() fs.FS {
	p, err := fs.ReadFile(static, "static/placeholder.html")
	if err != nil {
		panic("ui: the placeholder is not embedded: " + err.Error())
	}
	return fstest.MapFS{"index.html": {Data: p}}
}

// Reserved are the path prefixes that are the hub's own and never the
// app's: a request for one that reached Handler matched no route of the
// hub's, and answers 404 rather than the app's page. /metrics is served
// only on a listener of its own (hub/server), and is a 404 here too.
var Reserved = []string{"/v1/", "/internal/", "/auth/", "/.well-known/", "/metrics"}

// NoncePlaceholder, in the app's index.html, is replaced at each load
// by the nonce that page's styles are allowed under (CSP).
const NoncePlaceholder = "__SILO_CSP_NONCE__"

// CSP is the Content-Security-Policy the app is served under, with the
// page's style nonce (docs/threat-model.md T-TB4-1, INV-15). Script
// comes only from the hub's own files, never inline or evaluated;
// styles from its files, or a <style> carrying the nonce (MUI's,
// inserted at run time); images and everything fetched only from the
// hub's own origin. worker-src 'none' refuses the app a service worker,
// so that no artifact it views is cached behind the hub's no-store
// (docs/ui.md §2, INV-4).
func CSP(nonce string) string {
	style := "style-src 'self'"
	if nonce != "" {
		style += " 'nonce-" + nonce + "'"
	}
	return "default-src 'self'; script-src 'self'; " + style + "; img-src 'self'; font-src 'self'; " +
		"connect-src 'self'; worker-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

func newNonce() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// Handler serves the app from app, or the placeholder when app is nil:
// a file under assets/ as itself, cached for good (Vite names each
// after its content), and any other path, the app's own routes, as
// index.html, never cached, so that a new hub's app is picked up at
// the next load. Only GET and HEAD are served.
func Handler(app fs.FS) http.Handler {
	if app == nil {
		app = placeholder()
	}
	files := http.FileServerFS(app)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		for _, p := range Reserved {
			if strings.HasPrefix(r.URL.Path, p) {
				http.NotFound(w, r)
				return
			}
		}
		h := w.Header()
		h.Set("Content-Security-Policy", CSP(""))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if strings.HasPrefix(name, "assets/") {
			if st, err := fs.Stat(app, name); err == nil && !st.IsDir() {
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
				files.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(app, name); err == nil && !st.IsDir() {
				h.Set("Cache-Control", "no-cache")
				files.ServeHTTP(w, r)
				return
			}
		}
		index, err := fs.ReadFile(app, "index.html")
		if err != nil {
			http.Error(w, "ui: no index.html", http.StatusInternalServerError)
			return
		}
		nonce := newNonce()
		index = bytes.ReplaceAll(index, []byte(NoncePlaceholder), []byte(nonce))
		h.Set("Content-Security-Policy", CSP(nonce))
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			w.Write(index)
		}
	})
}
