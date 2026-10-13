package local

import (
	"fmt"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// Wire makes a container's namespaces and network before its driver's
// Create (local-backend.md §6, "The substrate"): silo-nsd pins a user
// namespace for container range n and a netns inside it, and wires v,
// the container's veth on its bridge, with its address. spec then
// names them, so the container never runs without its link, its
// address and the anti-spoofing set entry that silod adds for v.Addrs.
// On an error, nothing is left pinned.
func Wire(c *nsd.Client, spec *runtime.Spec, n uint32, v nsd.Veth) error {
	ns, err := c.Container(spec.ID, n)
	if err != nil {
		return fmt.Errorf("local: %s's namespaces: %w", spec.ID, err)
	}
	v.Container = spec.ID
	if err := c.Veth(v); err != nil {
		c.DeleteContainer(spec.ID, v.Name)
		return fmt.Errorf("local: %s's veth: %w", spec.ID, err)
	}
	spec.UserNS, spec.NetNS = ns.User, ns.Net
	spec.Root = runtime.IDRange{Outside: ns.Range.Outside, Size: ns.Range.Size}
	return nil
}

// Unwire undoes Wire, after the container is deleted.
func Unwire(c *nsd.Client, id, veth string) error {
	return c.DeleteContainer(id, veth)
}
