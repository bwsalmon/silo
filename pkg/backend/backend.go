// Package backend is what silod's backend-neutral parts need from a
// backend (docs/silo/local-backend.md, "The backend interface under
// silod", call L14): the reconcile, the leader's duties and the fence
// API call these interfaces, never a backend's own client. Each has one
// implementation per backend; GCP's are in package backend/gcp, which
// wraps the GCE client, the fence applier, the root capacity reader and
// the compute/v1 facade.
//
// An Object names a backend's objects by the backend's own collection
// names (instances, containers). They are strings only, for audit and
// for the reconcile's records; nothing outside the backend reads them.
package backend

import (
	"context"
	"net/netip"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
)

// Object is one object in the backend, as the reconcile sorts it.
type Object struct {
	// Kind is the backend's collection: compute/v1's instances, disks,
	// images, snapshots, firewalls, subnetworks or instanceTemplates on
	// GCP.
	Kind string
	// Scope is the zone or region, or "" for a global object.
	Scope string
	// Name is the real name.
	Name string
	// Silo is the silo its silo label names, or its <id>- prefix; ""
	// for neither.
	Silo string
	// Labelled is whether the silo came from the label, which is a
	// stronger sign than a prefix.
	Labelled bool
	// InVPC is whether the object is in silo's network: on GCP, an
	// instance with an interface on the VPC, or a subnetwork or a
	// firewall rule of it. An object there that no silo owns is
	// foreign.
	InVPC bool
	// Install is whether it is install's own.
	Install bool
	// Unrecorded is whether the store has no row for it although its
	// silo exists: on the local backend, a container or a volume made
	// after the point a restored store was copied at. It is an orphan
	// (local-rootless-storage.md call L40).
	Unrecorded bool
	// Ledger is the ledger object it is charged as, or "" for one the
	// ledger doesn't know.
	Ledger string
}

// Key names the object: "<kind>:<scope>/<name>", or "<kind>:<name>" for
// a global one.
func (o Object) Key() string {
	if o.Scope == "" {
		return o.Kind + ":" + o.Name
	}
	return o.Kind + ":" + o.Scope + "/" + o.Name
}

// Listing is what Inventory.List read.
type Listing struct {
	Objects []Object
	// Covered are the ledger object prefixes it listed completely
	// ("instance:us-central1-a/"): a charged object under one of them
	// that isn't in Objects doesn't exist.
	Covered []string
}

// Inventory lists and deletes the backend's objects, for the reconcile.
type Inventory interface {
	// List lists every object the reconcile looks at, in regions (the
	// root's allowed regions) and globally.
	List(ctx context.Context, regions []string) (*Listing, error)
	// Delete deletes o, and returns the backend operation it started,
	// or "". One that is gone already is not an error.
	Delete(ctx context.Context, o Object) (operation string, err error)
	// Pending are the ledger objects that an operation not yet done is
	// for, which the reconcile leaves charged whether they exist or not.
	Pending(ctx context.Context, tx store.Tx) (map[string]bool, error)
}

// Fence applies the fence that silod's database describes
// (fence.Input) to the backend.
type Fence interface {
	fenceapi.FenceApplier
	fenceapi.FenceBudget
	// Run applies the fence whenever it changes, until ctx is done,
	// retrying a failed apply after retry.
	Run(ctx context.Context, retry time.Duration) error
	// Repair applies the fence once, and returns the steps that
	// repaired drift: a policy that differed from what was recorded as
	// applied.
	Repair(ctx context.Context) (drift []string, err error)
}

// Capacity sets the root silo's capacity from what the backend has:
// on GCP, its quotas, less the control plane's own use.
type Capacity interface {
	Refresh(ctx context.Context) error
}

// Classes are the quota classes the backend charges, and their prices
// for quota:preview.
type Classes interface {
	fenceapi.Pricer
	// Kind is the class kind the backend's classes are under ("gce").
	Kind() string
	// Classes lists them from the class table.
	Classes(ctx context.Context, tx store.Tx) ([]ledger.Class, error)
}

// Contents deletes what a silo holds in the backend, on a delete with
// force (fenceapi.SiloDeleter).
type Contents interface {
	fenceapi.SiloDeleter
}

// Targets resolves the named target of an export.
type Targets interface {
	// Resolve is the address of silo's object name in scope (on GCP, an
	// instance in a zone).
	Resolve(ctx context.Context, silo, scope, name string) (netip.Addr, error)
}

// ToolVMs makes, deletes and identifies agents' tool VMs.
type ToolVMs interface {
	fenceapi.ToolVMs
	fenceapi.ToolVMIdentifier
}

// Resolver is t as the fence API takes it, or nil for none.
func Resolver(t Targets) fenceapi.InstanceResolver {
	if t == nil {
		return nil
	}
	return t.Resolve
}

// Backend is one backend's implementations of the interfaces above.
// Any may be nil, for a silod that serves no part needing it: a nil
// Inventory lists nothing, a nil Fence applies none.
type Backend struct {
	Inventory Inventory
	Fence     Fence
	Capacity  Capacity
	Classes   Classes
	Contents  Contents
	Targets   Targets
	ToolVMs   ToolVMs
}
