package dockergw

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/facade/local/docker"
)

// Holder is what wiring asks of silo-nsd (nsd.Client).
type Holder interface {
	Container(id string, n uint32) (*nsd.ContainerNS, error)
	Bridge(name string, addrs ...netip.Prefix) error
	Veth(v nsd.Veth) error
	DelLink(name string) error
	DeleteContainer(id, veth string) error
}

var _ Holder = (*nsd.Client)(nil)

// Wiring makes a container's namespaces and links in silo-net through
// the holder, as the facade's Options.Wire and Options.Unwire.
type Wiring struct {
	Holder Holder
	// Range is the container range (nsd.ContainerRange) a silo's
	// containers map; range 0, shared, if nil.
	Range func(silo string) uint32
}

// Wire pins c's user namespace and netns, and for each endpoint, in
// Endpoints' order, makes its network's bridge (idempotent), holding
// the gateway address, and a veth on it: eth0, eth1… in the container,
// pinned to the endpoint's MAC and address, the first with the default
// route. spec then names the namespaces and the range. On an error,
// nothing is left pinned.
func (w *Wiring) Wire(ctx context.Context, c *docker.Container, nets map[string]*docker.Network, spec *runtime.Spec) error {
	var n uint32
	if w.Range != nil {
		n = w.Range(spec.Silo)
	}
	ns, err := w.Holder.Container(c.ID, n)
	if err != nil {
		return fmt.Errorf("dockergw: %s's namespaces: %w", c.ID, err)
	}
	for i, e := range c.Endpoints() {
		net := nets[e.NetworkID]
		if net == nil {
			err = fmt.Errorf("dockergw: %s's network %s is gone", c.ID, e.NetworkID)
		} else if err = w.Holder.Bridge(net.Bridge(), netip.PrefixFrom(net.Gateway(), net.Subnet.Bits())); err == nil {
			v := nsd.Veth{Container: c.ID, Name: docker.VethName(c.ID, e.NetworkID), Bridge: net.Bridge(),
				Peer: fmt.Sprintf("eth%d", i), Addrs: []netip.Prefix{netip.PrefixFrom(e.Addr, net.Subnet.Bits())}, MAC: e.MAC}
			if i == 0 {
				v.Gateway = net.Gateway()
			}
			err = w.Holder.Veth(v)
		}
		if err != nil {
			w.Unwire(context.WithoutCancel(ctx), c)
			return fmt.Errorf("dockergw: wiring %s: %w", c.ID, err)
		}
	}
	spec.UserNS, spec.NetNS = ns.User, ns.Net
	spec.Root = runtime.IDRange{Outside: ns.Range.Outside, Size: ns.Range.Size}
	return nil
}

// Unwire deletes c's links and unpins its namespaces. Each step is
// idempotent, so a container half wired is undone too.
func (w *Wiring) Unwire(_ context.Context, c *docker.Container) error {
	var errs []error
	first := ""
	for i, e := range c.Endpoints() {
		name := docker.VethName(c.ID, e.NetworkID)
		if i == 0 {
			first = name
			continue
		}
		if err := w.Holder.DelLink(name); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, w.Holder.DeleteContainer(c.ID, first))
	return errors.Join(errs...)
}
