package gcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	facade "github.com/bwsalmon/silo/pkg/facade/gcp/gce"
	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	compute "google.golang.org/api/compute/v1"
)

// Inventory is the host project's objects, for the reconcile
// (gcp-backend.md, "Reconcile", SV-77).
type Inventory struct {
	GCE gce.Client
	// Project is the host project.
	Project string
	// Network is the VPC's name; "silo" by default.
	Network string
}

var _ backend.Inventory = (*Inventory)(nil)

func (v *Inventory) network() string {
	if v.Network == "" {
		return "silo"
	}
	return v.Network
}

// List lists instances and disks in the allowed regions' zones,
// subnetworks and regional templates in the allowed regions, and the
// global images, snapshots, firewall rules and templates.
func (v *Inventory) List(ctx context.Context, regions []string) (*backend.Listing, error) {
	p := v.Project
	network := v.network()
	allowed := map[string]bool{}
	for _, rg := range regions {
		allowed[rg] = true
	}
	zones, err := v.GCE.ListZones(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("listing zones: %w", err)
	}
	out := &backend.Listing{}
	add := func(collection, scope string) {
		if lo := facade.LedgerObject(collection, scope, ""); lo != "" {
			out.Covered = append(out.Covered, lo)
		}
	}
	obj := func(o backend.Object) { out.Objects = append(out.Objects, o) }
	var all gce.ListFilter
	for _, z := range zones {
		if !allowed[gce.ZoneRegion(z.Name)] {
			continue
		}
		insts, err := v.GCE.ListInstances(ctx, p, z.Name, all)
		if err != nil {
			return nil, fmt.Errorf("listing instances in %s: %w", z.Name, err)
		}
		for _, i := range insts {
			o := object("instances", z.Name, i.Name, i.Labels)
			for _, n := range i.NetworkInterfaces {
				o.InVPC = o.InVPC || gce.LastSegment(n.Network) == network
			}
			obj(o)
		}
		add("instances", z.Name)
		disks, err := v.GCE.ListDisks(ctx, p, z.Name, all)
		if err != nil {
			return nil, fmt.Errorf("listing disks in %s: %w", z.Name, err)
		}
		for _, d := range disks {
			obj(object("disks", z.Name, d.Name, d.Labels))
		}
		add("disks", z.Name)
	}
	for _, rg := range regions {
		if rg == ledger.Global {
			continue
		}
		subs, err := v.GCE.ListSubnetworks(ctx, p, rg, all)
		if err != nil {
			return nil, fmt.Errorf("listing subnetworks in %s: %w", rg, err)
		}
		for _, s := range subs {
			o := object("subnetworks", rg, s.Name, nil)
			o.InVPC = gce.LastSegment(s.Network) == network
			obj(o)
		}
		add("subnetworks", rg)
		ts, err := v.GCE.ListInstanceTemplates(ctx, p, rg, all)
		if err != nil {
			return nil, fmt.Errorf("listing instance templates in %s: %w", rg, err)
		}
		for _, t := range ts {
			obj(object("instanceTemplates", rg, t.Name, templateLabels(t)))
		}
		add("instanceTemplates", rg)
	}
	ts, err := v.GCE.ListInstanceTemplates(ctx, p, "", all)
	if err != nil {
		return nil, fmt.Errorf("listing global instance templates: %w", err)
	}
	for _, t := range ts {
		obj(object("instanceTemplates", "", t.Name, templateLabels(t)))
	}
	add("instanceTemplates", "global")
	imgs, err := v.GCE.ListImages(ctx, p, all)
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	for _, i := range imgs {
		obj(object("images", "", i.Name, i.Labels))
	}
	add("images", "")
	snaps, err := v.GCE.ListSnapshots(ctx, p, all)
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	for _, s := range snaps {
		obj(object("snapshots", "", s.Name, s.Labels))
	}
	add("snapshots", "")
	fws, err := v.GCE.ListFirewalls(ctx, p, all)
	if err != nil {
		return nil, fmt.Errorf("listing firewall rules: %w", err)
	}
	for _, f := range fws {
		o := object("firewalls", "", f.Name, nil)
		o.InVPC = gce.LastSegment(f.Network) == network
		o.Install = o.Install || strings.HasPrefix(f.Description, installDescription)
		obj(o)
	}
	add("firewalls", "")
	return out, nil
}

func templateLabels(t *compute.InstanceTemplate) map[string]string {
	if t.Properties == nil {
		return nil
	}
	return t.Properties.Labels
}

// What marks install's own objects (pkg/install, whose names these
// are; this package may not import it).
const (
	installLabel       = "silo-install"
	installDescription = "silo-install:"
	installPrefix      = "silo-"
	siloLabel          = "silo"
)

// object sorts one object by its label and name.
func object(collection, scope, name string, labels map[string]string) backend.Object {
	o := backend.Object{Kind: collection, Scope: scope, Name: name, Ledger: facade.LedgerObject(collection, scope, name)}
	if _, ok := labels[installLabel]; ok || strings.HasPrefix(name, installPrefix) {
		o.Install = true
		return o
	}
	if id := labels[siloLabel]; id != "" {
		o.Silo, o.Labelled = id, true
		return o
	}
	if id, _, ok := strings.Cut(name, "-"); ok && ids.Valid(id) {
		o.Silo = id
	}
	return o
}

// Delete deletes o. One GCE no longer has is not an error.
func (v *Inventory) Delete(ctx context.Context, o backend.Object) (string, error) {
	op, err := v.delete(ctx, o)
	if gce.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if op == nil {
		return "", nil
	}
	return op.Name, nil
}

func (v *Inventory) delete(ctx context.Context, o backend.Object) (*compute.Operation, error) {
	p := v.Project
	switch o.Kind {
	case "instances":
		return v.GCE.DeleteInstance(ctx, p, o.Scope, o.Name)
	case "disks":
		return v.GCE.DeleteDisk(ctx, p, o.Scope, o.Name)
	case "images":
		return v.GCE.DeleteImage(ctx, p, o.Name)
	case "snapshots":
		return v.GCE.DeleteSnapshot(ctx, p, o.Name)
	case "firewalls":
		return v.GCE.DeleteFirewall(ctx, p, o.Name)
	case "subnetworks":
		return v.GCE.DeleteSubnetwork(ctx, p, o.Scope, o.Name)
	case "instanceTemplates":
		return v.GCE.DeleteInstanceTemplate(ctx, p, o.Scope, o.Name)
	}
	return nil, fmt.Errorf("no delete for %s", o.Kind)
}

// Pending are the objects of the facade's operations not yet done.
func (v *Inventory) Pending(ctx context.Context, tx store.Tx) (map[string]bool, error) {
	return facade.PendingObjects(ctx, tx)
}
