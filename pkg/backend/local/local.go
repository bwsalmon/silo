// Package local is the local backend (docs/silo/local-backend.md): one
// machine, with containers in the vm or sandbox class on silod's
// private containerd, every tenant network in silo-net behind the
// nftables fence, and SQLite as the store. New builds the
// backend.Backend that silod's backend-neutral parts call.
//
// This is the skeleton (§15 step 5). It has the fence
// (local/netfence), the inventory, contents and targets over the
// runtime drivers (local/runtime), and nothing yet for Capacity,
// Classes or ToolVMs: the host's capacity reader, the local quota
// classes and tool containers come with later steps, and a nil part
// is one silod serves nothing through.
package local

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"github.com/bwsalmon/silo/pkg/backend"
	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Kind is the backend's name, as --backend takes it.
const Kind = "local"

// KindContainers is the inventory's one kind of object so far.
const KindContainers = "containers"

// Options are what New builds a backend from. Each part is built only
// where what it needs is given.
type Options struct {
	Store store.Store
	// Drivers are the classes this host runs.
	Drivers []runtime.Driver
	// NFT loads the fence into silo-net. Nil applies no fence.
	NFT netfence.NFT
	// FenceConfig tunes the fence's compiler.
	FenceConfig netfence.Config
	// Topology is silo-net's gateway addresses, networks and ports.
	// Nil is no networks, so that no tenant address is let through.
	Topology netfence.TopologySource
	// Grants are the fence's grants; nil is fence.AllGrants.
	Grants fence.GrantSource
	// Guard and OnDrift are netfence.Applier's.
	Guard   func(ctx context.Context, tx store.Tx) error
	OnDrift func(ctx context.Context, drift []string, repaired bool)
}

// New builds the local backend.
func New(o Options) backend.Backend {
	var b backend.Backend
	if o.NFT != nil && o.Store != nil {
		b.Fence = &netfence.Applier{Store: o.Store, NFT: o.NFT, Config: o.FenceConfig, Grants: o.Grants,
			Topology: o.Topology, Guard: o.Guard, OnDrift: o.OnDrift}
	}
	if len(o.Drivers) > 0 {
		c := Containers{Drivers: o.Drivers}
		b.Inventory, b.Contents, b.Targets = c, c, c
	}
	return b
}

var _ backend.Fence = (*netfence.Applier)(nil)

// ErrMappingChanged is a holder whose user namespace maps other ids
// than the store recorded.
var ErrMappingChanged = errors.New("local: the namespace holder's id mapping changed")

// BindMapping checks the holder's id mapping against the store's
// (local-rootless-storage.md call L33): the first start records it, and
// every later one, after a restart or a reboot, must find the same.
// Volumes and snapshots on disk belong to the container ranges the
// mapping gives, so a holder with another mapping, after an edit of
// /etc/subuid, would hand every silo files it can't use.
func BindMapping(ctx context.Context, st store.Store, mapping string) error {
	return st.Tx(ctx, func(tx store.Tx) error {
		got, ok, err := store.ReadSetting(ctx, tx, store.SettingHolderMapping)
		switch {
		case err != nil:
			return err
		case !ok:
			return store.WriteSetting(ctx, tx, store.SettingHolderMapping, mapping)
		case got != mapping:
			return fmt.Errorf("%w: the store recorded %q, silo-nsd maps %q; give silod's user its old ranges in /etc/subuid and /etc/subgid again", ErrMappingChanged, got, mapping)
		}
		return nil
	})
}

// Containers are the drivers' containers, as the reconcile, a silo's
// delete and an export's target see them.
type Containers struct {
	Drivers []runtime.Driver
}

var (
	_ backend.Inventory = Containers{}
	_ backend.Contents  = Containers{}
	_ backend.Targets   = Containers{}
)

// List implements backend.Inventory: every container in every class.
// The host is one region, so regions doesn't narrow it. Containers
// carry no ledger object yet (the Docker facade charges them), so the
// listing covers no ledger prefix.
func (c Containers) List(ctx context.Context, _ []string) (*backend.Listing, error) {
	l := &backend.Listing{}
	for _, d := range c.Drivers {
		cs, err := d.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("local: listing %s containers: %w", d.Class(), err)
		}
		for _, ct := range cs {
			silo := ct.Labels[runtime.LabelSilo]
			l.Objects = append(l.Objects, backend.Object{Kind: KindContainers, Scope: string(d.Class()), Name: ct.ID,
				Silo: silo, Labelled: silo != "", InVPC: true})
		}
	}
	return l, nil
}

// Delete implements backend.Inventory: it kills the container if it
// runs, and deletes it. One that is gone already is not an error.
func (c Containers) Delete(ctx context.Context, o backend.Object) (string, error) {
	if o.Kind != KindContainers {
		return "", fmt.Errorf("local: no objects of kind %q", o.Kind)
	}
	for _, d := range c.Drivers {
		if o.Scope != "" && string(d.Class()) != o.Scope {
			continue
		}
		if err := remove(ctx, d, o.Name); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return "", err
		}
	}
	return "", nil
}

func remove(ctx context.Context, d runtime.Driver, id string) error {
	// A container that isn't running (created, or stopped) refuses the
	// kill, and is deleted as it is.
	switch err := d.Kill(ctx, id, syscall.SIGKILL); {
	case err == nil:
		if _, err := d.Wait(ctx, id); err != nil {
			return err
		}
	case !errors.Is(err, runtime.ErrState):
		return err
	}
	return d.Delete(ctx, id)
}

// Pending implements backend.Inventory. The drivers' operations finish
// before they return, so none is ever pending.
func (Containers) Pending(context.Context, store.Tx) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// DeleteContents implements fenceapi.SiloDeleter: it stops and removes
// the silo's containers (§12 "Teardown", step 1). Networks, volumes,
// image records and cgroups come with the facade that makes them.
func (c Containers) DeleteContents(ctx context.Context, silo *tree.Silo) error {
	for _, d := range c.Drivers {
		cs, err := d.List(ctx)
		if err != nil {
			return err
		}
		for _, ct := range cs {
			if ct.Labels[runtime.LabelSilo] != silo.ID {
				continue
			}
			if err := remove(ctx, d, ct.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				return fmt.Errorf("local: deleting container %s of %s: %w", ct.ID, silo.ID, err)
			}
		}
	}
	return nil
}

// Resolve implements backend.Targets: the address of silo's container
// name, from the label silod set when it wired the container. The
// scope is ignored; the host is one.
func (c Containers) Resolve(ctx context.Context, silo, _ string, name string) (netip.Addr, error) {
	for _, d := range c.Drivers {
		cs, err := d.List(ctx)
		if err != nil {
			return netip.Addr{}, err
		}
		for _, ct := range cs {
			if ct.Labels[runtime.LabelSilo] != silo || ct.Labels[runtime.LabelName] != name {
				continue
			}
			a, err := netip.ParseAddr(ct.Labels[runtime.LabelAddr])
			if err != nil {
				return netip.Addr{}, fmt.Errorf("container %s has no address yet", name)
			}
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%w: container %s in silo %s", runtime.ErrNotFound, name, silo)
}
