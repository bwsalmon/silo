package docker

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/objseq"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/ledger/address"
	"github.com/bwsalmon/silo/pkg/store"
)

// DefaultNetwork is the network a container that names none is put on,
// made at the silo's first create that needs it (local-backend.md,
// "Networks").
const DefaultNetwork = "bridge"

// predefined are the names Docker reserves for its own networks.
var predefined = map[string]bool{"bridge": true, "host": true, "none": true}

// ownObject is a network's own range in the address allocator.
func ownObject(id string) string { return "docker-net-" + id[:12] }

// carve gives a new network its subnet: want, which must lie in the
// silo's free blocks, or when it is invalid the smallest free /24, or
// the largest free range if no /24 is free.
func carve(ctx context.Context, tx store.Tx, silo, id string, want netip.Prefix) (netip.Prefix, error) {
	if !want.IsValid() {
		m, err := address.Map(ctx, tx, silo)
		if err != nil {
			return netip.Prefix{}, err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return netip.Prefix{}, err
		}
		var fit, largest netip.Prefix
		for _, r := range m {
			if r.State != address.StateFree || r.Cooling.After(now) || r.Range.Bits() > address.SmallestSubnet {
				continue
			}
			if r.Range.Bits() <= 24 && (!fit.IsValid() || r.Range.Bits() > fit.Bits()) {
				fit = r.Range
			}
			if !largest.IsValid() || r.Range.Bits() < largest.Bits() {
				largest = r.Range
			}
		}
		switch {
		case fit.IsValid():
			want = netip.PrefixFrom(fit.Addr(), 24)
		case largest.IsValid():
			want = largest
		default:
			return netip.Prefix{}, conflict("silo: no free address space is left in this silo's blocks for a network")
		}
	}
	if err := address.SetOwn(ctx, tx, silo, []address.OwnRange{{Object: ownObject(id), Range: want, Region: ledger.Local}}); err != nil {
		return netip.Prefix{}, err
	}
	return want, nil
}

type networkCreateBody struct {
	Name     string
	Driver   string
	Internal bool
	Labels   map[string]string
	IPAM     struct {
		Driver string
		Config []struct {
			Subnet  string
			Gateway string
		}
	}
}

func (s *Server) networkCreate(q *request) error {
	b, err := decode[networkCreateBody](q)
	if err != nil {
		return s.refuse(q, err)
	}
	switch {
	case b.Name == "":
		return s.refuse(q, invalid("network name is required"))
	case !validName.MatchString(b.Name):
		return s.refuse(q, invalid("silo: invalid network name %q", b.Name))
	case predefined[b.Name]:
		return s.refuse(q, forbidden("%s is a pre-defined network and cannot be created", b.Name))
	case b.Driver != "" && b.Driver != "bridge":
		return s.refuse(q, invalid("silo: network driver %q is not supported; only bridge is", b.Driver))
	case b.IPAM.Driver != "" && b.IPAM.Driver != "default":
		return s.refuse(q, invalid("silo: IPAM driver %q is not supported", b.IPAM.Driver))
	case len(b.IPAM.Config) > 1:
		return s.refuse(q, invalid("silo: a network has one IPv4 subnet"))
	}
	var want netip.Prefix
	if len(b.IPAM.Config) == 1 {
		c := b.IPAM.Config[0]
		if c.Subnet != "" {
			if want, err = netip.ParsePrefix(c.Subnet); err != nil || !want.Addr().Is4() || want.Masked() != want {
				return s.refuse(q, invalid("silo: subnet %q is not an aligned IPv4 CIDR", c.Subnet))
			}
		}
		if c.Gateway != "" {
			gw, err := netip.ParseAddr(c.Gateway)
			if err != nil || !want.IsValid() || gw != want.Addr().Next() {
				return s.refuse(q, invalid("silo: a network's gateway is its subnet's first address, which silod gives it"))
			}
		}
	}
	n := &Network{ID: newID(), Name: b.Name, Internal: b.Internal, Labels: b.Labels, Attachable: true}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		ns, err := s.networks(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(ns, func(o *Network) bool { return o.Name == b.Name }) {
			return conflict("network with name %s already exists", b.Name)
		}
		charges, err := s.makeNetwork(q.ctx(), tx, q, n, want)
		if err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"name": n.Name, "id": n.ID, "subnet": n.Subnet.String()}, charges)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	return ok(q, map[string]any{"Id": n.ID, "Warning": ""})
}

// makeNetwork carves n's subnet, charges it and keeps it, inside tx.
func (s *Server) makeNetwork(ctx context.Context, tx store.Tx, q *request, n *Network, want netip.Prefix) ([]fenceapi.ChargeLine, error) {
	subnet, err := carve(ctx, tx, q.target.ID, n.ID, want)
	if err != nil {
		return nil, err
	}
	n.Subnet = subnet
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	n.Created = now
	if n.Seq, err = objseq.Next(ctx, tx); err != nil {
		return nil, err
	}
	charges, err := s.charge(ctx, tx, q, Object(q.target.ID, kindNetwork, n.ID), line{ClassNetworks, 1})
	if err != nil {
		return nil, err
	}
	return charges, put(ctx, tx, q.target.ID, kindNetwork, n.ID, n.Name, n)
}

// defaultNetwork is the silo's bridge network, made if it has none,
// inside tx. Its charges are added to *charges.
func (s *Server) defaultNetwork(ctx context.Context, tx store.Tx, q *request, charges *[]fenceapi.ChargeLine) (*Network, error) {
	ns, err := s.networks(ctx, tx, q.target.ID)
	if err != nil {
		return nil, err
	}
	for _, n := range ns {
		if n.Name == DefaultNetwork {
			return n, nil
		}
	}
	n := &Network{ID: newID(), Name: DefaultNetwork, Attachable: true}
	c, err := s.makeNetwork(ctx, tx, q, n, netip.Prefix{})
	if err != nil {
		return nil, err
	}
	*charges = append(*charges, c...)
	return n, nil
}

func (s *Server) networkList(q *request) error {
	f, err := filters(q, "name", "id", "label", "driver", "scope", "type", "dangling")
	if err != nil {
		return err
	}
	out := []map[string]any{}
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		ns, err := s.networks(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if !f.match("name", func(v string) bool { return strings.Contains(n.Name, v) }) ||
				!f.match("id", func(v string) bool { return len(v) <= len(n.ID) && n.ID[:len(v)] == v }) ||
				!f.match("driver", func(v string) bool { return v == "bridge" }) ||
				!f.match("scope", func(v string) bool { return v == "local" }) ||
				!f.match("type", func(v string) bool {
					return v == "custom" && n.Name != DefaultNetwork || v == "builtin" && n.Name == DefaultNetwork
				}) ||
				!f.labels(n.Labels) {
				continue
			}
			if d, ok := f.one("dangling"); ok && (d == "true" || d == "1") == attached(cs, n) {
				continue
			}
			out = append(out, networkJSON(n, cs))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return ok(q, out)
}

func attached(cs []*Container, n *Network) bool {
	return slices.ContainsFunc(cs, func(c *Container) bool { _, ok := c.Networks[n.Name]; return ok })
}

// networkJSON is n as Docker shows it, with the containers on it.
func networkJSON(n *Network, cs []*Container) map[string]any {
	ctrs := map[string]any{}
	for _, c := range cs {
		e, ok := c.Networks[n.Name]
		if !ok {
			continue
		}
		ctrs[c.ID] = map[string]any{"Name": c.Name, "EndpointID": e.EndpointID, "MacAddress": e.MAC,
			"IPv4Address": netip.PrefixFrom(e.Addr, n.Subnet.Bits()).String(), "IPv6Address": ""}
	}
	return map[string]any{
		"Name": n.Name, "Id": n.ID, "Created": n.Created.Format(time.RFC3339Nano), "Scope": "local",
		"Driver": "bridge", "EnableIPv6": false,
		"IPAM": map[string]any{"Driver": "default", "Options": map[string]string{},
			"Config": []map[string]string{{"Subnet": n.Subnet.String(), "Gateway": n.Gateway().String()}}},
		"Internal": n.Internal, "Attachable": n.Attachable, "Ingress": false,
		"ConfigFrom": map[string]string{"Network": ""}, "ConfigOnly": false,
		"Containers": ctrs, "Options": map[string]string{}, "Labels": nonNilMap(n.Labels),
	}
}

func (s *Server) networkInspect(q *request) error {
	var out map[string]any
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		n, err := s.network(q.ctx(), tx, q.target.ID, q.params["id"])
		if err != nil {
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		out = networkJSON(n, cs)
		return nil
	})
	if err != nil {
		return err
	}
	return ok(q, out)
}

func (s *Server) networkDelete(q *request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		n, err := s.network(q.ctx(), tx, q.target.ID, q.params["id"])
		if err != nil {
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		if attached(cs, n) {
			return forbidden("error while removing network: network %s id %s has active endpoints", n.Name, n.ID)
		}
		charges, err := s.deleteNetwork(q.ctx(), tx, q.target.ID, n)
		if err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"name": n.Name, "id": n.ID}, charges)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	return noContent(q)
}

func (s *Server) deleteNetwork(ctx context.Context, tx store.Tx, silo string, n *Network) ([]fenceapi.ChargeLine, error) {
	charges, err := refund(ctx, tx, Object(silo, kindNetwork, n.ID))
	if err != nil {
		return nil, err
	}
	if err := address.RemoveOwn(ctx, tx, silo, ownObject(n.ID)); err != nil {
		return nil, err
	}
	return charges, remove(ctx, tx, silo, kindNetwork, n.ID)
}

func (s *Server) networkPrune(q *request) error {
	f, err := filters(q, "label", "until")
	if err != nil {
		return s.refuse(q, err)
	}
	deleted := []string{}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		ns, err := s.networks(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		cs, err := s.containers(q.ctx(), tx, q.target.ID)
		if err != nil {
			return err
		}
		var charges []fenceapi.ChargeLine
		for _, n := range ns {
			if n.Name == DefaultNetwork || attached(cs, n) || !f.labels(n.Labels) {
				continue
			}
			c, err := s.deleteNetwork(q.ctx(), tx, q.target.ID, n)
			if err != nil {
				return err
			}
			charges = append(charges, c...)
			deleted = append(deleted, n.Name)
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"deleted": deleted}, charges)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	return ok(q, map[string]any{"NetworksDeleted": deleted})
}

// endpointConfig is how a create or a connect asks for a place on a
// network.
type endpointConfig struct {
	IPAMConfig *struct{ IPv4Address string }
	Aliases    []string
}

// attach puts c on n, inside tx: an address from n's subnet (the one
// asked for, if it is free), a MAC derived from it, which anti-spoofing
// pins, and aliases. It doesn't write c.
func (s *Server) attach(ctx context.Context, tx store.Tx, silo string, c *Container, n *Network, ec endpointConfig) error {
	if _, ok := c.Networks[n.Name]; ok {
		return forbidden("endpoint with name %s already exists in network %s", c.Name, n.Name)
	}
	cs, err := s.containers(ctx, tx, silo)
	if err != nil {
		return err
	}
	used := map[netip.Addr]bool{}
	for _, o := range cs {
		if e, ok := o.Networks[n.Name]; ok && o.ID != c.ID {
			used[e.Addr] = true
		}
	}
	last := lastAddr(n.Subnet)
	var a netip.Addr
	if ec.IPAMConfig != nil && ec.IPAMConfig.IPv4Address != "" {
		a, err = netip.ParseAddr(ec.IPAMConfig.IPv4Address)
		switch {
		case err != nil || !n.Subnet.Contains(a):
			return invalid("silo: address %s is not in network %s's subnet %s", ec.IPAMConfig.IPv4Address, n.Name, n.Subnet)
		case a == n.Subnet.Addr() || a == n.Gateway() || a == last:
			return invalid("silo: address %s is reserved in network %s", a, n.Name)
		case used[a]:
			return conflict("Address already in use: %s", a)
		}
	} else {
		for x := n.Gateway().Next(); x.IsValid() && x != last && n.Subnet.Contains(x); x = x.Next() {
			if !used[x] {
				a = x
				break
			}
		}
		if !a.IsValid() {
			return conflict("silo: no free address is left in network %s (%s)", n.Name, n.Subnet)
		}
	}
	b := a.As4()
	if c.Networks == nil {
		c.Networks = map[string]*Endpoint{}
	}
	c.Networks[n.Name] = &Endpoint{NetworkID: n.ID, EndpointID: newID(), Addr: a,
		MAC: fmt.Sprintf("02:42:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3]), Aliases: ec.Aliases}
	return nil
}

// lastAddr is p's last address, its broadcast.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= 1<<(32-p.Bits()) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

type connectBody struct {
	Container      string
	EndpointConfig endpointConfig
	Force          bool
}

func (s *Server) networkConnect(q *request) error {
	b, err := decode[connectBody](q)
	if err != nil {
		return s.refuse(q, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		n, err := s.network(q.ctx(), tx, q.target.ID, q.params["id"])
		if err != nil {
			return err
		}
		c, err := s.container(q.ctx(), tx, q.target.ID, b.Container)
		if err != nil {
			return err
		}
		if c.HostConfig["NetworkMode"] == "none" {
			return forbidden("container %s is on network none, and cannot be connected to another", c.Name)
		}
		if err := s.attach(q.ctx(), tx, q.target.ID, c, n, b.EndpointConfig); err != nil {
			return err
		}
		if err := put(q.ctx(), tx, q.target.ID, kindContainer, c.ID, c.Name, c); err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"container": c.ID, "address": c.Networks[n.Name].Addr.String()}, nil)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	return noContent(q)
}

func (s *Server) networkDisconnect(q *request) error {
	b, err := decode[connectBody](q)
	if err != nil {
		return s.refuse(q, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		n, err := s.network(q.ctx(), tx, q.target.ID, q.params["id"])
		if err != nil {
			return err
		}
		c, err := s.container(q.ctx(), tx, q.target.ID, b.Container)
		if err != nil {
			return err
		}
		if _, ok := c.Networks[n.Name]; !ok {
			return forbidden("container %s is not connected to network %s", c.ID, n.Name)
		}
		delete(c.Networks, n.Name)
		if err := put(q.ctx(), tx, q.target.ID, kindContainer, c.ID, c.Name, c); err != nil {
			return err
		}
		return s.auditTx(q.ctx(), tx, q, "OK", map[string]any{"container": c.ID}, nil)
	})
	if err != nil {
		return s.refuse(q, err)
	}
	return noContent(q)
}
