// Package hostdoor is the host's front door for registry clients
// (docs/silo/local-registry.md §5, "Who is calling", call L66): a TCP
// listener on 127.0.0.1:5115 for crane, skopeo, oras and a host
// BuildKit, which can't speak the registry's API over a Unix socket.
//
// A TCP peer has no SO_PEERCRED. Instead, when a connection is
// accepted, the peer's socket is looked up with NETLINK_SOCK_DIAG
// (inet_diag, the interface ss -e uses), which reports the uid that
// owns it. On loopback a process can only own sockets as its own uid,
// so the uid is the kernel's word, as SO_PEERCRED's is. A connection
// whose peer socket can't be found (it closed during the lookup) is
// refused: closed before anything is read from it.
//
// Each request is then mapped as a front-door member is: the member
// table's uid:<n> row names the caller's silo, read again per request
// so that moving the uid takes effect on an open connection; a uid
// mapped to no silo gets 403 DENIED. The request goes on to the
// registry service as a front_door Caller of that silo.
package hostdoor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/frontdoor"
	"github.com/bwsalmon/silo/pkg/store"
)

// Port is the host port's default, as install sets it
// (--registry-host-port, where 0 turns the listener off).
const Port = 5115

// Addr is the address silod listens on for port: loopback only.
func Addr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// ErrNoPeer is a peer socket the kernel doesn't have: the connection is
// refused.
var ErrNoPeer = errors.New("the peer's socket can't be found")

// PeerUID returns the uid owning the socket at peer that is connected
// to local. InetDiagUID is the real one; tests give their own.
type PeerUID func(local, peer netip.AddrPort) (uint32, error)

// Conn is an accepted connection and the uid its peer's socket belongs
// to.
type Conn struct {
	net.Conn
	UID uint32
}

// Listen wraps l, a loopback TCP listener, so that it accepts only
// connections whose peer uid lookup finds: every other is closed, and
// refused is told why (log.Printf if nil). lookup is InetDiagUID if
// nil. Accept returns a *Conn.
func Listen(l net.Listener, lookup PeerUID, refused func(peer net.Addr, err error)) net.Listener {
	if lookup == nil {
		lookup = InetDiagUID
	}
	if refused == nil {
		refused = func(peer net.Addr, err error) {
			log.Printf("hostdoor: refused a connection from %s: %v", peer, err)
		}
	}
	return &listener{Listener: l, lookup: lookup, refused: refused}
}

type listener struct {
	net.Listener
	lookup  PeerUID
	refused func(net.Addr, error)
}

func (l *listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := l.peerUID(c)
		if err != nil {
			c.Close()
			l.refused(c.RemoteAddr(), err)
			continue
		}
		return &Conn{Conn: c, UID: uid}, nil
	}
}

func (l *listener) peerUID(c net.Conn) (uint32, error) {
	local, lok := c.LocalAddr().(*net.TCPAddr)
	peer, pok := c.RemoteAddr().(*net.TCPAddr)
	if !lok || !pok {
		return 0, fmt.Errorf("not a TCP connection: %w", ErrNoPeer)
	}
	lap, pap := local.AddrPort(), peer.AddrPort()
	// Only a loopback peer is on this host, where its socket's owner
	// is the kernel's word. The listener is bound to 127.0.0.1, so this
	// is a second check, not the first.
	if !pap.Addr().Unmap().IsLoopback() {
		return 0, fmt.Errorf("peer %s is not on loopback: %w", pap, ErrNoPeer)
	}
	return l.lookup(lap, pap)
}

type uidKey struct{}

// ConnContext is http.Server's ConnContext for a Listen listener: it
// puts the connection's peer uid in each request's context.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if hc, ok := c.(*Conn); ok {
		return context.WithValue(ctx, uidKey{}, hc.UID)
	}
	return ctx
}

// UIDFromContext is the peer uid ConnContext put in ctx.
func UIDFromContext(ctx context.Context) (uint32, bool) {
	uid, ok := ctx.Value(uidKey{}).(uint32)
	return uid, ok
}

// Handler serves the registry's /v2/ to the peer uid in each request's
// context, as its member silo: registry is silod's registry service,
// which reads the caller from the context; nil serves no repos.
func Handler(st store.Store, registry http.Handler) http.Handler {
	return &handler{st: st, registry: registry}
}

type handler struct {
	st       store.Store
	registry http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if r.URL.Path != "/v2" && !strings.HasPrefix(r.URL.Path, "/v2/") {
		denied(w, http.StatusNotFound, "NAME_UNKNOWN", "silo: the registry's host port serves the OCI distribution API under /v2/ only")
		return
	}
	// Who the caller is comes from the kernel and the member table:
	// headers meant for silod's other transports are dropped.
	for k := range r.Header {
		if siloHeader(k) {
			r.Header.Del(k)
		}
	}
	uid, ok := UIDFromContext(r.Context())
	if !ok {
		denied(w, http.StatusForbidden, "DENIED", "silo: the connection's peer uid is unknown")
		return
	}
	id := fenceapi.UIDIdentity(uid)
	silo, err := frontdoor.MemberSilo(r.Context(), h.st, id)
	if err != nil {
		denied(w, http.StatusServiceUnavailable, "UNAVAILABLE", "silo: reading the member table: "+err.Error())
		return
	}
	if silo == "" {
		denied(w, http.StatusForbidden, "DENIED", fmt.Sprintf("silo: %s is not a member of any silo", id))
		return
	}
	if h.registry == nil {
		denied(w, http.StatusNotFound, "NAME_UNKNOWN", "silo: this silod serves no registry")
		return
	}
	caller := fenceapi.Caller{Silo: silo, Via: fenceapi.ViaFrontDoor, Member: id}
	h.registry.ServeHTTP(w, r.WithContext(fenceapi.ContextWithCaller(r.Context(), caller)))
}

// siloHeader reports whether a header name is one of silod's
// transports' own: Silo-Origin and Silo-Caller*, in any case, with - or
// _.
func siloHeader(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(k, "_", "-"))
	return strings.HasPrefix(k, "silo-origin") || strings.HasPrefix(k, "silo-caller")
}

// denied answers in the distribution spec's error shape.
func denied(w http.ResponseWriter, status int, code, msg string) {
	b, _ := json.Marshal(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

// Server is the host port's HTTP server: Handler behind a Listen
// listener.
type Server struct {
	srv *http.Server
	l   net.Listener
}

// Start listens on addr (Addr(port)) and serves Handler(st, registry)
// on it until Close.
func Start(addr string, st store.Store, registry http.Handler) (*Server, error) {
	tl, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{l: Listen(tl, nil, nil), srv: &http.Server{
		Handler:           Handler(st, registry),
		ConnContext:       ConnContext,
		ReadHeaderTimeout: 30 * time.Second,
	}}
	go func() {
		if err := s.srv.Serve(s.l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("hostdoor: serving %s: %v", addr, err)
		}
	}()
	return s, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() net.Addr { return s.l.Addr() }

// Close stops the server, closing its connections.
func (s *Server) Close() error { return s.srv.Close() }
