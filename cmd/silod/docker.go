package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/fake"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/facade/local/docker"
	"github.com/bwsalmon/silo/pkg/facade/local/dockergw"
	"github.com/bwsalmon/silo/pkg/gateway"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
)

// dockerSocketDir is where the host's Docker sockets go by default, as
// install's DockerSocket puts them: /run/silo/docker where /run/silo
// exists, and $XDG_RUNTIME_DIR/silo/docker otherwise.
func dockerSocketDir() string {
	if fi, err := os.Stat("/run/silo"); err == nil && fi.IsDir() {
		return "/run/silo/docker"
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "silo", "docker")
	}
	return ""
}

// siloNet is what silod needs of silo-net to serve the facade there:
// sockets the holder opens in it (call L28), and the wiring of
// containers' links. Nil where there is no holder to ask.
type siloNet interface {
	Listen(network, addr string) (net.Listener, error)
	ListenPacket(network, addr string) (net.PacketConn, error)
	// Dial connects from the gateway's address local to a container.
	Dial(ctx context.Context, local, target netip.AddrPort) (net.Conn, error)
	// Wire and Unwire are the facade's Options.Wire and Options.Unwire.
	Wire(ctx context.Context, c *docker.Container, nets map[string]*docker.Network, spec *runtime.Spec) error
	Unwire(ctx context.Context, c *docker.Container) error
	Close() error
}

// localDocker is the local backend's Docker facade (local-backend.md
// §4) and the listeners that serve it.
type localDocker struct {
	db    store.Store
	f     *docker.Server
	ports *dockergw.Ports
	names docker.Names
	net   siloNet
	// origin is origin identity in silo-net, from the gateway's tables.
	pub *gateway.Publisher
}

// newLocalDocker builds the facade over the runtime drivers: fake ones
// with fakeRuntime (development and tests only), and none otherwise
// until the real drivers are wired (local-backend.md §15 steps 7 and
// 10), so that networks, volumes and images are served and a container
// create is refused. net, if set, wires containers' links and serves
// published ports in silo-net. reg and content, if set, are the
// registry and silo-containerd's content store: push, pulls through
// registry.silo.internal and the cache, save, load, commit and docker
// login's check (local-registry.md §7).
func newLocalDocker(db store.Store, sd storage.Driver, names docker.Names, sn siloNet, fakeRuntime bool, volumes, version string,
	reg *registry.Service, content registry.Content) *localDocker {
	d := &localDocker{db: db, names: names, net: sn, pub: gateway.NewPublisher(db)}
	o := docker.Options{Storage: sd, Content: content, Version: version, Ready: func() string { return "" }}
	if reg != nil {
		o.Registry, o.Fetcher, o.Login = reg, reg, reg.Login
	}
	if fakeRuntime {
		vm, sbx := fake.New(runtime.VM), fake.New(runtime.Sandbox)
		vm.Volumes, sbx.Volumes = volumes, volumes
		o.Drivers = []runtime.Driver{vm, sbx}
	}
	if sn != nil {
		if !fakeRuntime {
			o.Wire, o.Unwire = sn.Wire, sn.Unwire
		}
		d.ports = &dockergw.Ports{Origin: d.origin,
			ListenSiloNet: func(port int) (net.Listener, error) {
				return sn.Listen("tcp4", netip.AddrPortFrom(names.Docker, uint16(port)).String())
			},
			Dial: func(ctx context.Context, target netip.AddrPort) (net.Conn, error) {
				return sn.Dial(ctx, netip.AddrPortFrom(names.Docker, 0), target)
			},
			Audit: d.auditHost}
		o.Listeners = d.ports
	}
	d.f = docker.New(db, o)
	return d
}

// origin is the silo a source address in silo-net belongs to, by the
// tables the gateway's origin identity reads.
func (d *localDocker) origin(a netip.Addr) (string, bool) {
	t, _ := d.pub.Current()
	if t == nil {
		return "", false
	}
	silo, _, ok := t.Resolve(a)
	return silo, ok
}

// auditHost records a published port's host export opening or closing
// in its silo's audit.
func (d *localDocker) auditHost(ctx context.Context, p docker.Published, event string) {
	ctx = context.WithoutCancel(ctx)
	err := d.db.Tx(ctx, func(tx store.Tx) error {
		return dockergw.AuditHostExport(ctx, tx, p, event)
	})
	if err != nil {
		log.Printf("auditing the host export of port %d of %s: %v", p.Port, p.Silo, err)
	}
}

// serve serves the facade until ctx ends: a socket per silo in
// socketDir, if set, and, with a holder, docker.silo.internal:2375,
// DNS and the published ports in silo-net.
func (d *localDocker) serve(ctx context.Context, socketDir string) error {
	go d.pub.Run(ctx)
	if socketDir != "" {
		s := &dockergw.Sockets{Dir: socketDir, Store: d.db, Handler: d.f}
		if err := s.Sync(ctx); err != nil {
			return err
		}
		log.Printf("serving the Docker facade on %s/<silo id>.sock", socketDir)
		go s.Run(ctx, 5*time.Second)
	}
	if d.net == nil {
		return nil
	}
	l, err := d.net.Listen("tcp4", netip.AddrPortFrom(d.names.Docker, docker.Port).String())
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: dockergw.ByOrigin(d.f, d.origin), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(l)
	dns := &dockergw.DNS{Store: d.db, Facade: d.f, Names: d.names, Origin: d.origin}
	dnsAddr := netip.AddrPortFrom(d.names.API, 53).String()
	pc, err := d.net.ListenPacket("udp4", dnsAddr)
	if err != nil {
		srv.Close()
		return err
	}
	dl, err := d.net.Listen("tcp4", dnsAddr)
	if err != nil {
		srv.Close()
		pc.Close()
		return err
	}
	go dns.ServeUDP(ctx, pc)
	go dns.ServeTCP(ctx, dl)
	go d.ports.Run(ctx, d.db, d.f, 10*time.Second)
	go func() {
		<-ctx.Done()
		srv.Close()
		pc.Close()
		dl.Close()
	}()
	log.Printf("serving the Docker facade on %s:%d and DNS on %s in silo-net", d.names.Docker, docker.Port, dnsAddr)
	return nil
}
