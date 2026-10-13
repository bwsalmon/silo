package gateway

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/pkg/imagerepos"
)

// The git service on port 80 (local-git.md, §3 "The git service",
// call L42): http://silo.internal/git/ is forwarded to silod, which
// runs git, under GitSilodPath on its socket, with the caller's silo
// and source address as for the API port. silod serves that path only
// to a request with Silo-Origin, so only through here. Git traffic is
// a data path: no control policy applies, so a sealed silo keeps
// fetching and pushing (call L44).
//
// When a manage grant ends, the holder's git requests in flight are
// cut off at once (local-git.md, "Manage grants: the repos resource
// type"): a push already in receive-pack completes or is rolled back
// by git's own ref transaction.

// GitSilodPath is where the gateway forwards /git/ on silod's socket:
// /git/<rest> goes to GitSilodPath/<rest>.
const GitSilodPath = "/silod/gateway/git"

// gitState is the git requests in flight, by the caller's silo.
type gitState struct {
	mu   sync.Mutex
	open map[string]map[*gitCall]struct{}
}

type gitCall struct {
	cancel context.CancelFunc
	// repo is a registry call's local repo, which an import of the
	// repo's silo's may be what lets the caller read it.
	repo *imagerepos.Name
}

func (g *Gateway) newGitProxy(tr http.RoundTripper) *httputil.ReverseProxy {
	return newForwarder(tr, "git", "/git", GitSilodPath)
}

// newForwarder forwards prefix's requests, streaming both ways, to
// silodPath on silod's socket.
func newForwarder(tr http.RoundTripper, what, prefix, silodPath string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: tr,
		// Packs and progress stream both ways.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "silod", "silod"
			pr.Out.URL.Path = silodPath + strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.RawPath = ""
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				panic(http.ErrAbortHandler) // cut off: close the connection
			}
			log.Printf("gateway: forwarding %s %s %s: %v", what, r.Method, r.URL.Path, err)
			http.Error(w, "silo: silod is unreachable; retry", http.StatusServiceUnavailable)
		},
	}
}

// serveGit forwards a /git/ request to silod, from a caller known by
// its source IP, with its silo and address. Git's Authorization header
// means nothing here, and goes no further.
func (g *Gateway) serveGit(w http.ResponseWriter, r *http.Request) {
	g.forwardPort80(w, r, g.gitProxy, nil)
}

// forwardPort80 sends a port 80 request to silod through p, from a caller
// known by its source IP, with its silo and address, cut off when the
// caller's manage grants end, or, for a registry call to repo, when an
// import of repo's silo's ends.
func (g *Gateway) forwardPort80(w http.ResponseWriter, r *http.Request, p *httputil.ReverseProxy, repo *imagerepos.Name) {
	t, silo, ip, err := g.portOrigin(r)
	if err != nil {
		writeToolsError(w, err)
		return
	}
	for k := range r.Header {
		if isSiloHeader(k) {
			r.Header.Del(k)
		}
	}
	r.Header.Del("Authorization")
	r.Header.Set(OriginHeader, FormatOrigin(silo, t.Version))
	r.Header.Set(OriginAddressHeader, ip.String())
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &gitCall{cancel: cancel, repo: repo}
	g.git.mu.Lock()
	if g.git.open == nil {
		g.git.open = map[string]map[*gitCall]struct{}{}
	}
	if g.git.open[silo] == nil {
		g.git.open[silo] = map[*gitCall]struct{}{}
	}
	g.git.open[silo][c] = struct{}{}
	g.git.mu.Unlock()
	defer func() {
		g.git.mu.Lock()
		delete(g.git.open[silo], c)
		if len(g.git.open[silo]) == 0 {
			delete(g.git.open, silo)
		}
		g.git.mu.Unlock()
	}()
	// The body goes up through a pipe that a cut-off breaks, since the
	// transport would otherwise wait on a client that is still sending.
	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, r.Body)
		pw.CloseWithError(err)
	}()
	go func() {
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	out := r.WithContext(ctx)
	out.Body = pr
	p.ServeHTTP(w, out)
}

// closeGit cuts off the port 80 requests in flight, git's and the
// registry's, of every silo that held a manage grant in old that cur no
// longer has: revoked, given up or expired. A registry upload cut off is
// abandoned, and its lease expires.
func (g *Gateway) closeGit(old, cur *Tables) {
	if old == nil {
		return
	}
	g.git.mu.Lock()
	defer g.git.mu.Unlock()
	n := 0
	for grant, holder := range old.Grants {
		if _, ok := cur.Grants[grant]; ok {
			continue
		}
		for c := range g.git.open[holder] {
			c.cancel()
			n++
		}
	}
	if n > 0 {
		log.Printf("gateway: cut off %d git and registry requests at version %d, whose callers' manage grants ended", n, cur.Version)
	}
}

// closeRegistry cuts off the registry calls in flight, to the exporter's
// repos, of every importer whose registry import in old cur no longer
// has: withdrawn by either side, suspended by a grant policy, or gone
// with a silo (local-registry.md, "Registry exports", call L64). A call
// to a repo the importer reads some other way is cut off too, and its
// client retries; one to any other silo's repo is left alone. An upload
// cut off is abandoned, and its lease expires.
func (g *Gateway) closeRegistry(old, cur *Tables) {
	if old == nil {
		return
	}
	g.git.mu.Lock()
	defer g.git.mu.Unlock()
	n := 0
	for id, im := range old.RegistryImports {
		if _, ok := cur.RegistryImports[id]; ok {
			continue
		}
		for c := range g.git.open[im.Importer] {
			if r := c.repo; r != nil && (r.SiloID == im.Exporter || r.SiloID == "" && r.SiloPath == im.ExporterPath) {
				c.cancel()
				n++
			}
		}
	}
	if n > 0 {
		log.Printf("gateway: cut off %d registry requests at version %d, whose callers' imports ended", n, cur.Version)
	}
}
