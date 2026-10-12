package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/egress"
)

// A subscription session's network (agents.md, "Isolation on the
// control VM", SV-118): its namespace is on EgressNetwork, an internal
// bridge with no route off the VM and no traffic between its
// containers, and the firewall in it allows one destination, the
// egress proxy that sessiond serves on the bridge's gateway address at
// EgressPort. The proxy knows each session by its address, and lets it
// CONNECT to its provider's hosts on 443 and nowhere else.
const (
	EgressNetwork = "silo-egress"
	EgressPort    = 3128
)

// EgressHosts are the hosts a subscription session of each provider
// may reach: the model API, and the hosts the CLI refreshes its login
// with. Nothing else, not even the CLI's telemetry or update checks
// (the adapters turn those off).
var EgressHosts = map[string][]string{
	// Claude Code on a Claude plan: inference on api.anthropic.com;
	// its OAuth token is refreshed on console.anthropic.com, or on
	// platform.claude.com in later releases.
	"anthropic": {"api.anthropic.com", "console.anthropic.com", "platform.claude.com"},
	// Codex on a ChatGPT plan: inference on chatgpt.com's Codex
	// backend (api.openai.com with an API-key login); the login is
	// refreshed on auth.openai.com.
	"openai": {"api.openai.com", "auth.openai.com", "chatgpt.com"},
}

// EgressProxy is the egress proxy for subscription sessions. Sessions
// are admitted by their address in EgressNetwork; a connection from an
// address it doesn't know is refused.
type EgressProxy struct {
	// Dial dials an allowed host's address; one that resolves to an
	// internal address (egress.Internal) is refused. A net.Dialer if
	// nil.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Resolve resolves an allowed host; net.DefaultResolver if nil.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	mu       sync.Mutex
	sessions map[netip.Addr]string
}

// Admit lets the session at addr reach provider's hosts.
func (p *EgressProxy) Admit(addr netip.Addr, provider string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = map[netip.Addr]string{}
	}
	p.sessions[addr.Unmap()] = provider
}

// Forget stops admitting the session at addr.
func (p *EgressProxy) Forget(addr netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, addr.Unmap())
}

// Allowed reports why the session at from may not CONNECT to hostport,
// or nil if it may.
func (p *EgressProxy) Allowed(from netip.Addr, hostport string) error {
	p.mu.Lock()
	provider, ok := p.sessions[from.Unmap()]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s is not a subscription session", from)
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return err
	}
	if port != "443" {
		return fmt.Errorf("port %s: only 443", port)
	}
	if !slices.Contains(EgressHosts[provider], egress.Normalize(host)) {
		return fmt.Errorf("%s is not one of %s's hosts (%s)", host, provider, strings.Join(EgressHosts[provider], ", "))
	}
	return nil
}

// ServeHTTP implements http.Handler: CONNECT only.
func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "silo egress proxy: CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	from, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "silo egress proxy: "+err.Error(), http.StatusForbidden)
		return
	}
	if err := p.Allowed(from.Addr(), r.Host); err != nil {
		log.Printf("egress proxy: refused %s to %s: %v", from.Addr(), r.Host, err)
		http.Error(w, "silo egress proxy: "+err.Error(), http.StatusForbidden)
		return
	}
	up, err := p.dial(r.Context(), r.Host)
	if err != nil {
		http.Error(w, "silo egress proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "silo egress proxy: can't hijack", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	c, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	done := make(chan struct{}, 2)
	go func() {
		// Whatever the client sent after its CONNECT is buffered.
		if n := rw.Reader.Buffered(); n > 0 {
			b, _ := rw.Reader.Peek(n)
			up.Write(b)
		}
		io.Copy(up, c)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

// dial connects to an allowed host, on an address it resolves to that
// isn't internal: no allowed name leads to the metadata server or
// anything else on the VPC.
func (p *EgressProxy) dial(ctx context.Context, hostport string) (net.Conn, error) {
	host, port, _ := net.SplitHostPort(hostport)
	resolve := p.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	addrs, err := resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	dial := p.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = d.DialContext
	}
	err = fmt.Errorf("%s has no address", host)
	for _, a := range addrs {
		if egress.Internal(a.Unmap(), nil) {
			err = fmt.Errorf("%s resolves to %s, an internal address", host, a)
			continue
		}
		var c net.Conn
		if c, err = dial(ctx, "tcp", net.JoinHostPort(a.Unmap().String(), port)); err == nil {
			return c, nil
		}
	}
	return nil, err
}

// Serve serves the proxy on l until it is closed.
func (p *EgressProxy) Serve(l net.Listener) error {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
