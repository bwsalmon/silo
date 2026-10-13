package docker

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/fenceapi"
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
					t.Ports = append(t.Ports, netfence.Port{Name: vethName(c.ID, e.NetworkID), Network: e.NetworkID,
						MAC: mac, Addr: e.Addr})
				}
			}
		}
		return t, nil
	}
}

// vethName is the host end of a container's veth on a network's
// bridge: 15 bytes at most, as the kernel allows.
func vethName(container, network string) string { return "sv" + container[:7] + network[:6] }

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
			if len(c.Ports) == 0 || s.state(ctx, c).State != runtime.Running {
				continue
			}
			e := c.firstEndpoint()
			if e == nil {
				continue
			}
			if ingress == "" {
				chain, err := tree.Ancestry(ctx, tx, silo)
				if err != nil {
					return nil, err
				}
				if _, ingress, err = fenceapi.EffectiveIsolation(ctx, tx, chain); err != nil {
					return nil, err
				}
			}
			for _, p := range c.Ports {
				out = append(out, Published{Silo: silo, Proto: p.Proto, Port: p.Public,
					Target: netip.AddrPortFrom(e.Addr, uint16(p.Private)), Host: ingress == fenceapi.IngressHost, Container: c.ID})
			}
		}
	}
	return out, nil
}

// Answer is what the gateway's DNS answers for a name: the addresses,
// or none for NXDOMAIN.
type Answer struct {
	Addrs []netip.Addr
}

// Names are the gateway's own names and addresses, which Lookup
// answers for every caller.
type Names struct {
	API    netip.Addr // silo.internal
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
	case name == "silo.internal":
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
