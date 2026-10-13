// Package dockergw serves the local backend's Docker facade
// (pkg/facade/local/docker) where callers reach it
// (docs/silo/local-backend.md, §4 "Address, and how a request names
// its silo", "Published ports" and "DNS"):
//
//   - a Unix socket per silo on the host, the caller its peer's uid
//     (SO_PEERCRED) as a front-door member (Sockets);
//   - tcp://docker.silo.internal:2375 in silo-net, the caller the silo
//     its source address belongs to (ByOrigin);
//   - DNS at silo.internal's address, port 53, answering through the
//     facade's Lookup (DNS);
//   - the published ports' listeners on docker.silo.internal, and on
//     the host's loopback where the silo's ingress is host (Ports).
//
// The package opens no socket in silo-net itself: silod is given them
// by silo-nsd (call L28), and passes in functions that do.
package dockergw

import (
	"net/http"
	"net/netip"
	"regexp"
	"strings"

	"github.com/bwsalmon/silo/pkg/fenceapi"
)

// Origin is origin identity in silo-net: the silo the source address
// belongs to, or false for an address that is no silo's.
type Origin func(netip.Addr) (silo string, ok bool)

// ping is the one path a caller with no silo is answered on: Docker's
// clients negotiate the API version from it before anything else.
var ping = regexp.MustCompile(`^(/v[0-9]+\.[0-9]+)?/_ping$`)

// IsPing reports whether r is a /_ping, at any version.
func IsPing(r *http.Request) bool { return ping.MatchString(r.URL.Path) }

// stripSiloHeaders drops the headers silod's other transports believe
// (Silo-Origin, Silo-Caller and the like): here the caller comes from
// the connection alone.
func stripSiloHeaders(r *http.Request) {
	for k := range r.Header {
		if l := strings.ToLower(k); strings.HasPrefix(l, "silo-") || strings.HasPrefix(l, "silo_") {
			r.Header.Del(k)
		}
	}
}

// refuse answers a call with no caller as the facade answers one: 403,
// in Docker's error shape.
func refuse(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	b, _ := jsonMessage(msg)
	w.Write(b)
}

// ByOrigin serves h on docker.silo.internal:2375 in silo-net: the
// caller is the silo the connection's source address belongs to
// (fenceapi.ViaOrigin), and an address that is no silo's gets 403 on
// everything but /_ping.
func ByOrigin(h http.Handler, origin Origin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stripSiloHeaders(r)
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		silo, ok := "", false
		if err == nil {
			silo, ok = origin(ap.Addr().Unmap())
		}
		if !ok {
			if IsPing(r) {
				h.ServeHTTP(w, r)
				return
			}
			refuse(w, "silo: the connection's source address is no silo's")
			return
		}
		c := fenceapi.Caller{Silo: silo, Via: fenceapi.ViaOrigin}
		h.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), c)))
	})
}
