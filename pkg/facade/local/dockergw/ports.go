package dockergw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/pkg/facade/local/docker"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Ports are the published ports' listeners (local-backend.md,
// "Published ports"), which the facade opens and closes as containers
// start and stop (docker.Listeners):
//
//   - on docker.silo.internal:<port> in silo-net, one listener a port,
//     which answers a connection only from a silo that publishes that
//     port and forwards it to that silo's container: two silos may both
//     publish 8080;
//   - where Host is set, on 127.0.0.1:<port> in the host's namespace,
//     a proxied export to the front door, audited as it opens and
//     closes, and refused with docker.ErrPortTaken (409) if something
//     else on the host has the port.
//
// Only tcp is served: a udp port is recorded and not listened on.
type Ports struct {
	// Origin is origin identity in silo-net.
	Origin Origin
	// ListenSiloNet listens on docker.silo.internal:port in silo-net.
	ListenSiloNet func(port int) (net.Listener, error)
	// ListenHost listens on 127.0.0.1:port in the host's namespace;
	// net.Listen if nil.
	ListenHost func(port int) (net.Listener, error)
	// Dial connects to a container's address from the gateway, in
	// silo-net.
	Dial func(ctx context.Context, target netip.AddrPort) (net.Conn, error)
	// Audit records a host export opening ("open") or closing
	// ("close"). Nil records nothing.
	Audit func(ctx context.Context, p docker.Published, event string)
	// Logf logs a listener that failed; log.Printf if nil.
	Logf func(string, ...any)

	mu sync.Mutex
	// inner are the listeners on docker.silo.internal, by port.
	inner map[int]*innerListener
	// host are the listeners on the host's loopback, by port.
	host map[int]*hostListener
	// open are each container's published ports, as Open had them.
	open map[string][]docker.Published
}

type innerListener struct {
	l net.Listener
	// silos are the targets of the silos that publish the port.
	silos map[string]docker.Published
}

type hostListener struct {
	l net.Listener
	p docker.Published
}

var _ docker.Listeners = (*Ports)(nil)

func (p *Ports) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Open listens on a container's published ports. On an error, nothing
// it opened is left open.
func (p *Ports) Open(ctx context.Context, ps []docker.Published) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inner == nil {
		p.inner, p.host, p.open = map[int]*innerListener{}, map[int]*hostListener{}, map[string][]docker.Published{}
	}
	if len(ps) > 0 {
		// A container opened again (a restart racing Sync) has only
		// its new ports.
		for _, pp := range p.open[ps[0].Container] {
			p.closeOne(ctx, pp)
		}
		delete(p.open, ps[0].Container)
	}
	var done []docker.Published
	for _, pp := range ps {
		if pp.Proto != "tcp" {
			continue
		}
		if err := p.openOne(ctx, pp); err != nil {
			for _, d := range done {
				p.closeOne(ctx, d)
			}
			return err
		}
		done = append(done, pp)
	}
	if len(ps) > 0 {
		p.open[ps[0].Container] = append(p.open[ps[0].Container], done...)
	}
	return nil
}

func (p *Ports) openOne(ctx context.Context, pp docker.Published) error {
	if pp.Host {
		if h := p.host[pp.Port]; h != nil {
			return fmt.Errorf("Bind for 127.0.0.1:%d failed: %w", pp.Port, docker.ErrPortTaken)
		}
		listen := p.ListenHost
		if listen == nil {
			listen = func(port int) (net.Listener, error) {
				return net.Listen("tcp", netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(port)).String())
			}
		}
		l, err := listen(pp.Port)
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("Bind for 127.0.0.1:%d failed: %w", pp.Port, docker.ErrPortTaken)
		} else if err != nil {
			return err
		}
		h := &hostListener{l: l, p: pp}
		p.host[pp.Port] = h
		go p.serveHost(h)
		if p.Audit != nil {
			p.Audit(ctx, pp, "open")
		}
	}
	in := p.inner[pp.Port]
	if in == nil {
		l, err := p.ListenSiloNet(pp.Port)
		if err != nil {
			if pp.Host {
				p.closeHost(ctx, pp)
			}
			return fmt.Errorf("listening on %s:%d: %w", docker.Domain, pp.Port, err)
		}
		in = &innerListener{l: l, silos: map[string]docker.Published{}}
		p.inner[pp.Port] = in
		go p.serveInner(pp.Port, in)
	}
	in.silos[pp.Silo] = pp
	return nil
}

// Close closes a container's listeners.
func (p *Ports) Close(container string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pp := range p.open[container] {
		p.closeOne(context.Background(), pp)
	}
	delete(p.open, container)
}

func (p *Ports) closeOne(ctx context.Context, pp docker.Published) {
	if in := p.inner[pp.Port]; in != nil && in.silos[pp.Silo].Container == pp.Container {
		delete(in.silos, pp.Silo)
		if len(in.silos) == 0 {
			in.l.Close()
			delete(p.inner, pp.Port)
		}
	}
	if pp.Host {
		p.closeHost(ctx, pp)
	}
}

func (p *Ports) closeHost(ctx context.Context, pp docker.Published) {
	if h := p.host[pp.Port]; h != nil && h.p.Container == pp.Container {
		h.l.Close()
		delete(p.host, pp.Port)
		if p.Audit != nil {
			p.Audit(ctx, pp, "close")
		}
	}
}

// Sync makes the listeners those of ps, the published ports of the
// running containers (docker.Server.Published): after a restart, the
// listeners of containers still running are opened again, and those of
// containers that stopped while no one was watching are closed.
func (p *Ports) Sync(ctx context.Context, ps []docker.Published) {
	by := map[string][]docker.Published{}
	for _, pp := range ps {
		by[pp.Container] = append(by[pp.Container], pp)
	}
	p.mu.Lock()
	var stale []string
	for c := range p.open {
		if by[c] == nil {
			stale = append(stale, c)
		}
	}
	for c := range by {
		if _, ok := p.open[c]; ok {
			delete(by, c)
		}
	}
	p.mu.Unlock()
	for _, c := range stale {
		p.Close(c)
	}
	for c, cps := range by {
		if err := p.Open(ctx, cps); err != nil {
			p.logf("published ports of container %s: %v", c, err)
		}
	}
}

// Run follows the facade's published ports every interval until ctx
// ends, when it closes every listener.
func (p *Ports) Run(ctx context.Context, st store.Store, f *docker.Server, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		var ps []docker.Published
		err := st.Tx(ctx, func(tx store.Tx) error {
			var err error
			ps, err = f.Published(ctx, tx)
			return err
		})
		if err == nil {
			p.Sync(ctx, ps)
		} else if ctx.Err() == nil {
			p.logf("published ports: %v", err)
		}
		select {
		case <-ctx.Done():
			p.Sync(context.Background(), nil)
			return
		case <-t.C:
		}
	}
}

// serveInner forwards each connection to port on docker.silo.internal
// to the target of its origin's silo, and closes one from a silo that
// publishes nothing there.
func (p *Ports) serveInner(port int, in *innerListener) {
	for {
		c, err := in.l.Accept()
		if err != nil {
			return
		}
		from, ok := addrOf(c.RemoteAddr())
		silo := ""
		if ok {
			silo, ok = p.Origin(from)
		}
		p.mu.Lock()
		pp, found := in.silos[silo]
		p.mu.Unlock()
		if !ok || !found {
			c.Close()
			continue
		}
		go p.forward(c, pp.Target)
	}
}

// serveHost forwards each connection to the host's loopback port to
// its container.
func (p *Ports) serveHost(h *hostListener) {
	for {
		c, err := h.l.Accept()
		if err != nil {
			return
		}
		go p.forward(c, h.p.Target)
	}
}

func (p *Ports) forward(c net.Conn, target netip.AddrPort) {
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t, err := p.Dial(ctx, target)
	cancel()
	if err != nil {
		return
	}
	defer t.Close()
	splice(c, t)
}

// splice copies both ways until both are done, passing each side's end
// on as a half-close.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// KindHostExport is the audit kind of a published port's host export
// opening and closing.
const KindHostExport = "docker.host_export"

// AuditHostExport records, in p's silo's audit, its host export on
// 127.0.0.1:<port> opening or closing (event "open" or "close"). A
// silo gone since records nothing.
func AuditHostExport(ctx context.Context, tx store.Tx, p docker.Published, event string) error {
	t, err := tree.Get(ctx, tx, p.Silo)
	if err != nil {
		return nil
	}
	return fenceapi.Audit(ctx, tx, t, fenceapi.Caller{Silo: p.Silo, Via: fenceapi.ViaInternal}, KindHostExport, "", "OK",
		map[string]any{"event": event, "port": p.Port, "proto": p.Proto, "container": p.Container,
			"listen": fmt.Sprintf("127.0.0.1:%d", p.Port), "target": p.Target.String()})
}
