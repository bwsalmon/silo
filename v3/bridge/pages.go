package bridge

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/v3/gateway"
)

const pageCSP = "default-src 'self' 'unsafe-inline' 'unsafe-eval' data: blob:; frame-ancestors 'self'"

// RegisterPage records a page link on the gateway and, if ExposePort is set,
// starts a local reverse-proxy listener on that port.
func (b *Bridge) RegisterPage(ctx context.Context, auth gateway.Auth, p gateway.PageRegisterParams) (*gateway.Page, error) {
	if p.Title == "" {
		p.Title = p.Slug
	}
	req := map[string]any{
		"slug":  p.Slug,
		"title": p.Title,
	}
	if p.Port != 0 {
		req["port"] = p.Port
	}
	if p.Host != "" {
		req["host"] = p.Host
	}
	if p.ExposePort != 0 {
		req["expose_port"] = p.ExposePort
	}
	if p.URL != "" {
		req["url"] = p.URL
	}
	var pg gateway.Page
	if err := b.gwCall(ctx, "page.register", withAuth(auth, req), &pg); err != nil {
		return nil, err
	}
	if pg.ExposePort != 0 && pg.Port != 0 {
		_ = b.startExposePort(pg)
	}
	return &pg, nil
}

// UnregisterPage removes a page link on the gateway and closes any exposed
// port listener for it.
func (b *Bridge) UnregisterPage(ctx context.Context, auth gateway.Auth, slug string) (*gateway.Page, error) {
	var pg gateway.Page
	if err := b.gwCall(ctx, "page.unregister", withAuth(auth, map[string]any{"slug": slug}), &pg); err != nil {
		return nil, err
	}
	if pg.ExposePort != 0 {
		b.stopExposePort(pg.ExposePort)
	}
	return &pg, nil
}

// ListPages lists page links visible to the caller.
func (b *Bridge) ListPages(ctx context.Context, auth gateway.Auth, all bool) ([]gateway.Page, error) {
	req := map[string]any{}
	if all {
		req["all"] = true
	}
	var list []gateway.Page
	if err := b.gwCall(ctx, "page.list", withAuth(auth, req), &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (b *Bridge) findActivePage(ctx context.Context, slug string) (*gateway.Page, error) {
	pages, err := b.ListPages(ctx, gateway.Auth{}, false)
	if err != nil {
		return nil, err
	}
	for i := range pages {
		if pages[i].Slug == slug && pages[i].Active {
			return &pages[i], nil
		}
	}
	return nil, nil
}

func (b *Bridge) servePageProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/p/")
	slug, suffix, hasSlash := strings.Cut(rest, "/")
	if slug == "" {
		http.NotFound(w, r)
		return
	}
	pg, err := b.findActivePage(r.Context(), slug)
	if err != nil || pg == nil {
		http.NotFound(w, r)
		return
	}

	// External service URL link: validate and redirect.
	if pg.URL != "" {
		u, err := url.Parse(pg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			http.Error(w, "invalid external page URL", http.StatusBadGateway)
			return
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", pageCSP)
		if pg.Owner != "" {
			w.Header().Set("X-Silo-Page-Owner", pg.Owner)
		}
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}

	// Internal service port: redirect /p/<slug> -> /p/<slug>/ for normal HTTP
	// requests so relative assets resolve under /p/<slug>/.
	if !hasSlash && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		target := "/p/" + slug + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}

	host := pg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	targetURL := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, strconv.Itoa(pg.Port)),
	}
	proxy := newInternalProxy(targetURL, "/p/"+slug, "/"+suffix, pg.Owner)
	proxy.ServeHTTP(w, r)
}

func newInternalProxy(targetURL *url.URL, prefix, rewrittenPath, owner string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			if rewrittenPath != "" {
				req.URL.Path = rewrittenPath
				req.URL.RawPath = ""
			}
			req.Host = targetURL.Host
			// Strip Silo credentials before forwarding to agent-controlled services (§6.5).
			req.Header.Del("Cookie")
			req.Header.Del("Authorization")
			if prefix != "" {
				req.Header.Set("X-Forwarded-Prefix", prefix)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.Header.Get("Content-Security-Policy") == "" {
				resp.Header.Set("Content-Security-Policy", pageCSP)
			}
			if owner != "" {
				resp.Header.Set("X-Silo-Page-Owner", owner)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, fmt.Sprintf("page proxy error: %v", err), http.StatusBadGateway)
		},
	}
}

func (b *Bridge) startExposePort(pg gateway.Page) error {
	b.exposeMu.Lock()
	defer b.exposeMu.Unlock()
	if old, ok := b.exposeListeners[pg.ExposePort]; ok {
		_ = old.Close()
		delete(b.exposeListeners, pg.ExposePort)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(pg.ExposePort))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b.exposeListeners[pg.ExposePort] = l
	host := pg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	targetURL := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, strconv.Itoa(pg.Port)),
	}
	proxy := newInternalProxy(targetURL, "", "", pg.Owner)
	srv := &http.Server{Handler: proxy}
	go func() {
		_ = srv.Serve(l)
	}()
	return nil
}

func (b *Bridge) stopExposePort(port int) {
	b.exposeMu.Lock()
	defer b.exposeMu.Unlock()
	if l, ok := b.exposeListeners[port]; ok {
		_ = l.Close()
		delete(b.exposeListeners, port)
	}
}
