package docker

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// What the gateway and the fence read of the facade's objects
// (local-backend.md, "Networks", "Published ports", "DNS"). Each runs
// in the transaction its caller holds, so the fence sees networks and
// ports as they were committed.

// Topology is silo-net's networks and ports as the facade made them,
// for netfence: a bridge per network and a port per container
// endpoint, pinned to its MAC and address. api and docker are gw0's
// addresses for silo.internal and docker.silo.internal.
func Topology(api, docker netip.Addr) netfence.TopologySource {
	return func(ctx context.Context, tx store.Tx) (*netfence.Topology, error) {
		t := &netfence.Topology{API: api, Docker: docker}
		nets, err := allRows[Network](ctx, tx, kindNetwork)
		if err != nil {
			return nil, err
		}
		for _, silo := range sortedKeys(nets) {
			for _, n := range nets[silo] {
				t.Networks = append(t.Networks, netfence.Network{ID: n.ID, Silo: silo, Bridge: n.Bridge(),
					Subnet: n.Subnet, Internal: n.Internal})
			}
		}
		ctrs, err := allRows[Container](ctx, tx, kindContainer)
		if err != nil {
			return nil, err
		}
		for _, silo := range sortedKeys(ctrs) {
			for _, c := range ctrs[silo] {
				for _, name := range sortedKeys(c.Networks) {
					e := c.Networks[name]
					mac, err := net.ParseMAC(e.MAC)
					if err != nil {
						return nil, err
					}
					t.Ports = append(t.Ports, netfence.Port{Name: VethName(c.ID, e.NetworkID), Network: e.NetworkID,
						MAC: mac, Addr: e.Addr})
				}
			}
		}
		return t, nil
	}
}

// TopologyChangeKind is the change-log entry the facade writes with a
// change to Topology's ports, a container's endpoints made, moved or
// removed: it wakes the fence's applier, which reloads the pins
// (local-backend.md, "Anti-spoofing"), as a grant's change does.
const TopologyChangeKind = "docker.topology"

// topologyChanged writes a TopologyChangeKind entry for silo's
// container id in tx.
func topologyChanged(ctx context.Context, tx store.Tx, silo, id string) error {
	_, err := tx.Append(ctx, TopologyChangeKind, silo, `{"container":"`+id+`"}`)
	return err
}

// VethName is the host end of a container's veth on a network's
// bridge: 15 bytes at most, as the kernel allows.
func VethName(container, network string) string { return "sv" + container[:7] + network[:6] }

// Published is one published port the gateway listens on.
type Published struct {
	Silo  string
	Proto string // tcp or udp
	// Port is the port on docker.silo.internal, answered only for
	// callers from Silo.
	Port int
	// Target is the container's address and port the gateway forwards
	// to.
	Target netip.AddrPort
	// Host is set when the silo's effective ingress is host: the gateway
	// also listens on 127.0.0.1:Port in the host's namespace, as a
	// proxied export to the front door.
	Host bool
	// Container is the container's id.
	Container string
}

// Published lists the published ports of running containers, in every
// silo, for the gateway's listeners. A container on no network
// publishes nothing it can be reached at.
func (s *Server) Published(ctx context.Context, tx store.Tx) ([]Published, error) {
	ctrs, err := allRows[Container](ctx, tx, kindContainer)
	if err != nil {
		return nil, err
	}
	var out []Published
	for _, silo := range sortedKeys(ctrs) {
		ingress := ""
		for _, c := range ctrs[silo] {
			if len(c.Ports) == 0 || c.firstEndpoint() == nil || s.state(ctx, c).State != runtime.Running {
				continue
			}
			if ingress == "" {
				if ingress, err = ingressOf(ctx, tx, silo); err != nil {
					return nil, err
				}
			}
			out = append(out, published(silo, c, ingress)...)
		}
	}
	return out, nil
}

// published are c's published ports, in a silo whose effective ingress
// is ingress.
func published(silo string, c *Container, ingress string) []Published {
	e := c.firstEndpoint()
	if e == nil {
		return nil
	}
	var out []Published
	for _, p := range c.Ports {
		out = append(out, Published{Silo: silo, Proto: p.Proto, Port: p.Public,
			Target: netip.AddrPortFrom(e.Addr, uint16(p.Private)), Host: ingress == fenceapi.IngressHost, Container: c.ID})
	}
	return out
}

// ingressOf is silo's effective ingress policy.
func ingressOf(ctx context.Context, tx store.Tx, silo string) (string, error) {
	chain, err := tree.Ancestry(ctx, tx, silo)
	if err != nil {
		return "", err
	}
	_, ingress, err := fenceapi.EffectiveIsolation(ctx, tx, chain)
	return ingress, err
}

// ErrPortTaken is a Listeners' answer for a host port that something
// else on the host already listens on, which the start refuses (409).
var ErrPortTaken = errors.New("port is already allocated")

// Listeners are the gateway's published-port listeners (Options.Listeners).
type Listeners interface {
	// Open listens on a container's published ports as it starts: on
	// docker.silo.internal, and on the host's loopback for those with
	// Host set. An error wrapping ErrPortTaken refuses the start with
	// 409. On any error, it has opened nothing.
	Open(ctx context.Context, ps []Published) error
	// Close closes a container's listeners, if it has any.
	Close(container string)
}

// listen opens the call's container's listeners as it starts.
func (s *Server) listen(q *request, c *Container) error {
	if s.opts.Listeners == nil || len(c.Ports) == 0 || c.firstEndpoint() == nil {
		return nil
	}
	var ingress string
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		ingress, err = ingressOf(q.ctx(), tx, q.target.ID)
		return err
	})
	if err != nil {
		return err
	}
	ps := published(q.target.ID, c, ingress)
	if err := s.opts.Listeners.Open(q.ctx(), ps); err != nil {
		if errors.Is(err, ErrPortTaken) {
			return conflict("silo: %v", err)
		}
		return err
	}
	return nil
}

// unlisten closes a container's listeners as it stops or goes.
func (s *Server) unlisten(container string) {
	if s.opts.Listeners != nil {
		s.opts.Listeners.Close(container)
	}
}

// Answer is what the gateway's DNS answers for a name: the addresses,
// or none for NXDOMAIN.
type Answer struct {
	Addrs []netip.Addr
}

// Names are the gateway's own names and addresses, which Lookup
// answers for every caller.
type Names struct {
	API    netip.Addr // silo.internal and registry.silo.internal
	Docker netip.Addr // docker.silo.internal, and <silo id>.docker.silo.internal
}

// Lookup answers an A query for name from the container at from, in
// silo (origin identity): the gateway's own names; and the caller's own
// containers, by name, id prefix and network alias, on the networks
// the asking container shares with them, as Docker's embedded DNS
// does. Everything else, other silos' names and every external name
// included, is NXDOMAIN: names are scoped by origin, and external
// names are the egress proxy's to resolve.
func (s *Server) Lookup(ctx context.Context, tx store.Tx, names Names, silo string, from netip.Addr, name string) (Answer, error) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	switch {
	case name == "silo.internal", name == imagerepos.Host:
		return Answer{Addrs: []netip.Addr{names.API}}, nil
	case name == Domain:
		return Answer{Addrs: []netip.Addr{names.Docker}}, nil
	case strings.HasSuffix(name, "."+Domain):
		if id := strings.TrimSuffix(name, "."+Domain); !strings.Contains(id, ".") {
			if _, err := tree.Get(ctx, tx, id); err == nil {
				return Answer{Addrs: []netip.Addr{names.Docker}}, nil
			}
		}
		return Answer{}, nil
	case strings.Contains(name, "."):
		return Answer{}, nil
	}
	cs, err := rows[Container](ctx, tx, silo, kindContainer)
	if err != nil {
		return Answer{}, err
	}
	// The networks the asking container is on.
	var mine []string
	for _, c := range cs {
		for _, e := range c.Networks {
			if e.Addr == from {
				for _, e2 := range c.Networks {
					mine = append(mine, e2.NetworkID)
				}
			}
		}
	}
	var out []netip.Addr
	for _, c := range cs {
		for _, e := range c.Networks {
			if !slices.Contains(mine, e.NetworkID) {
				continue
			}
			if strings.EqualFold(c.Name, name) || (len(name) >= 12 && strings.HasPrefix(c.ID, name)) ||
				slices.ContainsFunc(e.Aliases, func(a string) bool { return strings.EqualFold(a, name) }) {
				if !slices.Contains(out, e.Addr) {
					out = append(out, e.Addr)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return Answer{Addrs: out}, nil
}

// Endpoints are c's endpoints in the order its links are made: the
// first (firstEndpoint), which has the default route, then the rest by
// network name.
func (c *Container) Endpoints() []*Endpoint {
	first := c.firstEndpoint()
	if first == nil {
		return nil
	}
	out := []*Endpoint{first}
	for _, k := range sortedKeys(c.Networks) {
		if e := c.Networks[k]; e != first {
			out = append(out, e)
		}
	}
	return out
}
