package gwtls

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Registry is silod's read-only pull-through for gateway VMs, on its
// mTLS port: a gateway VM has no service account, and so no token to
// read Artifact Registry with, and pulls silod's image here instead,
// as a client of the gateway CA. Only the one repository is served,
// and only manifests and blobs are read; the image is named by digest,
// which docker checks, so what it gets is what install deployed.
type Registry struct {
	// Repository is the image's repository, <host>/<path>, as
	// us-central1-docker.pkg.dev/<project>/silo/silod.
	Repository string
	// Token is silod's access token, from its service account.
	Token func(ctx context.Context) (string, error)
	// Scheme is the upstream's, https if "" (a test's is http).
	Scheme string
	// Transport is the upstream's; http.DefaultTransport if nil.
	Transport http.RoundTripper
}

// Handler serves /v2/ for the repository.
func (r *Registry) Handler() http.Handler {
	host, path, _ := strings.Cut(r.Repository, "/")
	scheme := r.Scheme
	if scheme == "" {
		scheme = "https"
	}
	proxy := &httputil.ReverseProxy{
		Transport: r.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: scheme, Host: host, Path: pr.In.URL.Path, RawQuery: pr.In.URL.RawQuery}
			pr.Out.Host = host
		},
	}
	prefix := "/v2/" + path + "/"
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "silo: the gateway registry is read-only", http.StatusMethodNotAllowed)
			return
		}
		if req.URL.Path == "/v2/" {
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			return
		}
		rest, ok := strings.CutPrefix(req.URL.Path, prefix)
		kind, _, _ := strings.Cut(rest, "/")
		if !ok || (kind != "manifests" && kind != "blobs") || strings.Contains(rest, "..") {
			http.Error(w, "silo: the gateway registry serves "+r.Repository+" alone", http.StatusNotFound)
			return
		}
		tok, err := r.Token(req.Context())
		if err != nil {
			http.Error(w, "silo: silod has no token: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		for k := range req.Header {
			if k != "Accept" && k != "Range" {
				req.Header.Del(k)
			}
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		proxy.ServeHTTP(w, req)
	})
}
