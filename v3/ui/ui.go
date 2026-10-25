// Package ui embeds and serves the Silo v3 web UI static assets (design §6).
// Assets are plain HTML, CSS, and JavaScript embedded into the single `silo`
// binary with `embed` so no Node.js toolchain is needed at build or install time.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// ContentSecurityPolicy is the strict CSP applied to the Silo admin UI
// (design §6.1): no inline scripts, no third-party origins, and no framing.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; img-src 'self' data:; frame-ancestors 'none'"

//go:embed static/*
var embeddedStatic embed.FS

// StaticFS returns the embedded static filesystem rooted at `static/`.
func StaticFS() fs.FS {
	sub, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// Handler returns an http.Handler that serves the embedded Silo web UI with
// strict security headers.
func Handler() http.Handler {
	sub := StaticFS()
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", ContentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")

		p := strings.TrimPrefix(r.URL.Path, "/static/")
		if p != r.URL.Path {
			r2 := new(http.Request)
			*r2 = *r
			u2 := *r.URL
			u2.Path = "/" + p
			r2.URL = &u2
			fileServer.ServeHTTP(w, r2)
			return
		}

		switch r.URL.Path {
		case "/", "/index.html", "/agents", "/pages", "/session", "/approvals", "/audit":
			data, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.Error(w, "missing UI index", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		default:
			fileServer.ServeHTTP(w, r)
		}
	})
}
