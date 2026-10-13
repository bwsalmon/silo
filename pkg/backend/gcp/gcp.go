// Package gcp is the GCP backend: package backend's interfaces over
// silod's GCE client (package gce), the fence's applier (package
// fence), the root capacity reader (package ledger/rootcap) and the
// compute/v1 facade (package facade/gcp/gce). It wraps them without
// changing what they do (docs/silo/local-backend.md, "The refactor").
//
// GCP has no Contents yet: deleting a silo's GCE objects on a delete
// with force is a later task's, so a GCP backend's Contents is nil.
package gcp

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/bwsalmon/silo/pkg/backend"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/gcefence"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/rootcap"
	facade "github.com/bwsalmon/silo/pkg/facade/gcp/gce"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
)

// Fence is the fence as a global network firewall policy, which a
// *gcefence.Applier applies.
type Fence struct {
	*gcefence.Applier
}

var _ backend.Fence = Fence{}

// NewFence returns a's Fence.
func NewFence(a *gcefence.Applier) Fence { return Fence{a} }

// Repair is the applier's Repair, with each step as its String.
func (f Fence) Repair(ctx context.Context) ([]string, error) {
	steps, err := f.Applier.Repair(ctx)
	var out []string
	for _, s := range steps {
		out = append(out, s.String())
	}
	return out, err
}

// Capacity sets the root's capacity from the host project's quotas.
type Capacity struct {
	Reader *rootcap.Reader
}

var _ backend.Capacity = Capacity{}

// NewCapacity returns the Capacity that rootcap.New(db, c, config)
// reads.
func NewCapacity(db store.Store, c gce.Client, config rootcap.Config) Capacity {
	return Capacity{Reader: rootcap.New(db, c, config)}
}

// Refresh is the reader's Refresh.
func (c Capacity) Refresh(ctx context.Context) error {
	_, err := c.Reader.Refresh(ctx)
	return err
}

// Kind is the class kind of GCE's quota classes.
const Kind = "gce"

// Classes are GCE's quota classes, and the facade's prices for them.
type Classes struct {
	fenceapi.Pricer
}

var _ backend.Classes = Classes{}

// NewClasses returns the Classes that fc prices.
func NewClasses(fc *facade.Server) Classes { return Classes{fc.Pricer()} }

// Kind is "gce".
func (Classes) Kind() string { return Kind }

// Classes lists the class table's gce classes.
func (Classes) Classes(ctx context.Context, tx store.Tx) ([]ledger.Class, error) {
	all, err := ledger.Classes(ctx, tx)
	if err != nil {
		return nil, err
	}
	var out []ledger.Class
	for _, c := range all {
		if c.Kind == Kind {
			out = append(out, c)
		}
	}
	return out, nil
}

// Targets resolves an export's instance target to the instance's
// primary internal IP, by its real name in the host project.
type Targets struct {
	GCE     gce.Client
	Project string
}

var _ backend.Targets = Targets{}

// Resolve is the primary internal IP of silo's instance name in zone.
func (t Targets) Resolve(ctx context.Context, silo, zone, name string) (netip.Addr, error) {
	inst, err := t.GCE.GetInstance(ctx, t.Project, zone, silo+"-"+name)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(inst.NetworkInterfaces) == 0 || inst.NetworkInterfaces[0].NetworkIP == "" {
		return netip.Addr{}, fmt.Errorf("instance %s has no internal IP yet", name)
	}
	return netip.ParseAddr(inst.NetworkInterfaces[0].NetworkIP)
}

// ToolVMs are the tool VMs fc makes (SV-104).
func ToolVMs(fc *facade.Server) backend.ToolVMs {
	return fc.ToolVMs().(backend.ToolVMs)
}

// Options are what New builds a backend from. Each part is built only
// where what it needs is given.
type Options struct {
	Store store.Store
	// Host is the host project's client and Project its name: the
	// Inventory, Targets and, with Capacity set, the Capacity.
	Host    gce.Client
	Project string
	// Fence applies the fence.
	Fence *gcefence.Applier
	// Facade is the compute/v1 facade: the Classes and the ToolVMs.
	Facade *facade.Server
	// Capacity configures the root capacity reader, or nil for none
	// (over the fake GCE, whose quotas a test sets).
	Capacity *rootcap.Config
}

// New returns the GCP backend o describes.
func New(o Options) backend.Backend {
	var b backend.Backend
	if o.Host != nil && o.Project != "" {
		b.Inventory = &Inventory{GCE: o.Host, Project: o.Project}
		b.Targets = Targets{GCE: o.Host, Project: o.Project}
		if o.Capacity != nil {
			b.Capacity = NewCapacity(o.Store, o.Host, *o.Capacity)
		}
	}
	if o.Fence != nil {
		b.Fence = NewFence(o.Fence)
	}
	if o.Facade != nil {
		b.Classes = NewClasses(o.Facade)
		b.ToolVMs = ToolVMs(o.Facade)
	}
	return b
}
