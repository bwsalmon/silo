package gateway

import (
	"net"
	"net/http"
	"strings"

	"github.com/bwsalmon/silo/pkg/imagerepos"
)

// The registry on port 80 (local-registry.md, "The address", call
// L56): a request whose Host is registry.silo.internal is forwarded to
// silod's registry service under RegistrySilodPath, with the caller's
// silo and source address as for git, and nothing else is. Registry
// traffic is a data path, as git's is: a sealed silo keeps pulling
// and pushing to its existing repos (call L65). Authorization never
// identifies anyone (call L66), and goes no further, except Basic
// credentials on an upstream path, which are the caller's own for the
// upstream's private content (call L72) and which silod uses for that
// request only.

// RegistrySilodPath is where the gateway forwards the registry on
// silod's socket: /v2/<rest> goes to RegistrySilodPath/v2/<rest>.
const RegistrySilodPath = "/silod/gateway/registry"

// IsRegistryHost reports whether a request's Host names the registry,
// with or without port 80.
func IsRegistryHost(host string) bool {
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port != "80" {
			return false
		}
		host = h
	}
	return strings.EqualFold(strings.TrimSuffix(host, "."), imagerepos.Host)
}

func (g *Gateway) serveRegistry(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v2" && !strings.HasPrefix(r.URL.Path, "/v2/") {
		http.Error(w, "silo: "+imagerepos.Host+" serves the OCI distribution API under /v2/ only", http.StatusNotFound)
		return
	}
	var repo *imagerepos.Name
	auth := ""
	if name, _, ok := imagerepos.SplitPath(r.URL.Path); ok {
		if n, ok := imagerepos.ParseName(name, r.URL.Query().Get("ns")); ok && n.Local() {
			repo = &n
		} else if ok {
			if a := r.Header.Get("Authorization"); len(a) > 6 && strings.EqualFold(a[:6], "basic ") {
				auth = a
			}
		}
	}
	g.forwardPort80(w, r, g.regProxy, repo, auth)
}
