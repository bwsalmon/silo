package install

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/ids"
	compute "google.golang.org/api/compute/v1"
)

// The first three of uninstall's steps (install.md "silo uninstall",
// SV-96) have nothing to do with an install: they undo what the tree
// made in it. Install plans them ok; UninstallSteps puts them after
// install's own, so that uninstall, which runs in reverse, runs them
// first: tenants, then orphans, then credentials, then install's steps
// from admin back to apis.

// The marks silod puts on what it makes for a tenant (gcp-backend.md,
// "Naming"): the labels silo=<id> and silo-managed=true, or, where GCE
// has no labels, the prefix <id>- and a description beginning silo:<id>.
const (
	SiloLabel        = "silo"
	SiloManagedLabel = "silo-managed"
	siloDescription  = "silo:"
)

// UninstallSteps are Steps with uninstall's own three after them.
// PlanRemoveAll and Remove run them in reverse.
func UninstallSteps(env *Env) []Step {
	return append(Steps(env), &Credentials{Env: env}, &Orphans{Env: env}, &Tenants{Env: env})
}

// TenantObject is one GCE object silod made for a silo.
type TenantObject struct {
	// Collection is compute/v1's: instances, disks, instanceTemplates,
	// images, snapshots, firewalls or subnetworks.
	Collection string `json:"collection"`
	// Scope is the zone or region, or "" for a global object.
	Scope string `json:"scope,omitempty"`
	Name  string `json:"name"`
	// Silo is the id its label or prefix names.
	Silo string `json:"silo"`
	// Protected is an instance's deletion protection, which uninstall
	// never lifts.
	Protected bool `json:"protected,omitempty"`
}

func (o TenantObject) String() string {
	if o.Scope == "" {
		return o.Collection + "/" + o.Name
	}
	return o.Collection + "/" + o.Scope + "/" + o.Name
}

// deleteOrder is the order objects are deleted in: an instance holds its
// disks and its subnetwork, and a template or image may be in use.
var deleteOrder = map[string]int{"instances": 0, "disks": 1, "instanceTemplates": 2, "images": 3, "snapshots": 4, "firewalls": 5, "subnetworks": 6}

func sortObjects(objs []TenantObject) {
	sort.SliceStable(objs, func(i, j int) bool {
		a, b := objs[i], objs[j]
		if deleteOrder[a.Collection] != deleteOrder[b.Collection] {
			return deleteOrder[a.Collection] < deleteOrder[b.Collection]
		}
		return a.String() < b.String()
	})
}

// labelledSilo is the silo a labelled object belongs to: both of silod's
// labels, and never install's own.
func labelledSilo(labels map[string]string) string {
	if _, ok := labels[LabelKey]; ok || labels[SiloManagedLabel] != "true" || !ids.Valid(labels[SiloLabel]) {
		return ""
	}
	return labels[SiloLabel]
}

// prefixedSilo is the silo an object without labels belongs to: the
// prefix <id>- and a description silo:<id>, both. A name alone doesn't
// make an object a silo's: uninstall deletes what it finds.
func prefixedSilo(name, description string) string {
	id, _, ok := strings.Cut(name, "-")
	if !ok || !ids.Valid(id) || !strings.HasPrefix(description, siloDescription+id) || strings.HasPrefix(description, DescriptionPrefix) {
		return ""
	}
	return id
}

// listTenantObjects lists every object in the project with silod's
// marks, in every zone and region that is up, sorted in delete order.
func listTenantObjects(ctx context.Context, env *Env) ([]TenantObject, error) {
	c, p := env.Cloud.Compute, env.Config.Project
	zones, err := c.ListZones(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("listing zones: %w", err)
	}
	var out []TenantObject
	managed := gce.ListFilter{Labels: map[string]string{SiloManagedLabel: "true"}}
	regions := map[string]bool{}
	for _, z := range zones {
		if z.Status != "" && z.Status != "UP" {
			continue
		}
		if r := gce.LastSegment(z.Region); r != "" {
			regions[r] = true
		} else {
			regions[gce.ZoneRegion(z.Name)] = true
		}
		insts, err := c.ListInstances(ctx, p, z.Name, managed)
		if err != nil {
			return nil, fmt.Errorf("listing instances in %s: %w", z.Name, err)
		}
		for _, i := range insts {
			if id := labelledSilo(i.Labels); id != "" {
				out = append(out, TenantObject{Collection: "instances", Scope: z.Name, Name: i.Name, Silo: id, Protected: i.DeletionProtection})
			}
		}
		disks, err := c.ListDisks(ctx, p, z.Name, managed)
		if err != nil {
			return nil, fmt.Errorf("listing disks in %s: %w", z.Name, err)
		}
		for _, d := range disks {
			if id := labelledSilo(d.Labels); id != "" {
				out = append(out, TenantObject{Collection: "disks", Scope: z.Name, Name: d.Name, Silo: id})
			}
		}
	}
	names := make([]string, 0, len(regions))
	for r := range regions {
		names = append(names, r)
	}
	sort.Strings(names)
	template := func(scope string, t *compute.InstanceTemplate) {
		id := prefixedSilo(t.Name, t.Description)
		if t.Properties != nil && id == "" {
			id = labelledSilo(t.Properties.Labels)
		}
		if id != "" {
			out = append(out, TenantObject{Collection: "instanceTemplates", Scope: scope, Name: t.Name, Silo: id})
		}
	}
	var all gce.ListFilter
	for _, r := range names {
		subs, err := c.ListSubnetworks(ctx, p, r, all)
		if err != nil {
			return nil, fmt.Errorf("listing subnetworks in %s: %w", r, err)
		}
		for _, s := range subs {
			if id := prefixedSilo(s.Name, s.Description); id != "" {
				out = append(out, TenantObject{Collection: "subnetworks", Scope: r, Name: s.Name, Silo: id})
			}
		}
		ts, err := c.ListInstanceTemplates(ctx, p, r, all)
		if err != nil {
			return nil, fmt.Errorf("listing instance templates in %s: %w", r, err)
		}
		for _, t := range ts {
			template(r, t)
		}
	}
	ts, err := c.ListInstanceTemplates(ctx, p, "", all)
	if err != nil {
		return nil, fmt.Errorf("listing instance templates: %w", err)
	}
	for _, t := range ts {
		template("", t)
	}
	imgs, err := c.ListImages(ctx, p, managed)
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	for _, i := range imgs {
		if id := labelledSilo(i.Labels); id != "" {
			out = append(out, TenantObject{Collection: "images", Name: i.Name, Silo: id})
		}
	}
	snaps, err := c.ListSnapshots(ctx, p, managed)
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	for _, s := range snaps {
		if id := labelledSilo(s.Labels); id != "" {
			out = append(out, TenantObject{Collection: "snapshots", Name: s.Name, Silo: id})
		}
	}
	fws, err := c.ListFirewalls(ctx, p, all)
	if err != nil {
		return nil, fmt.Errorf("listing firewall rules: %w", err)
	}
	for _, f := range fws {
		if id := prefixedSilo(f.Name, f.Description); id != "" {
			out = append(out, TenantObject{Collection: "firewalls", Name: f.Name, Silo: id})
		}
	}
	sortObjects(out)
	return out, nil
}

// deleteObject deletes o and waits for it. One already gone is not an
// error. It never lifts an instance's deletion protection: GCE refuses,
// and the plan has said so.
func deleteObject(ctx context.Context, env *Env, o TenantObject) error {
	c, p := env.Cloud.Compute, env.Config.Project
	var op *compute.Operation
	var err error
	switch o.Collection {
	case "instances":
		op, err = c.DeleteInstance(ctx, p, o.Scope, o.Name)
	case "disks":
		op, err = c.DeleteDisk(ctx, p, o.Scope, o.Name)
	case "instanceTemplates":
		op, err = c.DeleteInstanceTemplate(ctx, p, o.Scope, o.Name)
	case "images":
		op, err = c.DeleteImage(ctx, p, o.Name)
	case "snapshots":
		op, err = c.DeleteSnapshot(ctx, p, o.Name)
	case "firewalls":
		op, err = c.DeleteFirewall(ctx, p, o.Name)
	case "subnetworks":
		op, err = c.DeleteSubnetwork(ctx, p, o.Scope, o.Name)
	default:
		return fmt.Errorf("no delete for %s", o.Collection)
	}
	if err == nil {
		err = await(ctx, c, p, op)
	}
	if err != nil && !IsNotFound(err) && !gce.IsNotFound(err) {
		return fmt.Errorf("deleting %s: %w", o, err)
	}
	return nil
}

// objectChanges are the deletes of objs, or a failure for each instance
// with deletion protection, which uninstall doesn't lift.
func objectChanges(objs []TenantObject, why string) []Change {
	var out []Change
	for _, o := range objs {
		if o.Protected {
			out = append(out, Change{Action: ActionFail, Object: o.String(), Detail: "has deletion protection",
				Why: "uninstall lifts deletion protection only on the database, and only with --delete-data",
				Fix: fmt.Sprintf("gcloud compute instances update %s --zone %s --no-deletion-protection, if it should go", o.Name, o.Scope)})
			continue
		}
		out = append(out, Change{Action: ActionDelete, Object: o.String(), Detail: why, Estimate: 20 * time.Second})
	}
	return out
}

func objectFromChange(ch Change) (TenantObject, bool) {
	parts := strings.Split(ch.Object, "/")
	if _, ok := deleteOrder[parts[0]]; !ok {
		return TenantObject{}, false
	}
	switch len(parts) {
	case 2:
		return TenantObject{Collection: parts[0], Name: parts[1]}, true
	case 3:
		return TenantObject{Collection: parts[0], Scope: parts[1], Name: parts[2]}, true
	}
	return TenantObject{}, false
}

// Tenants is uninstall's first step, tenants: if any silo but the root
// exists, or the root has facade objects or agents, uninstall stops and
// lists them, unless --force. With --force it asks silod to delete each
// of the root's children with its subtree (silo delete --force), waits
// for that, then deletes the root's agents and its own objects.
type Tenants struct{ Env *Env }

// TenantsObserved is the tree as silod knows it: the root's children,
// agents and objects. Unreachable is why silod couldn't be asked, with
// silo-control there to ask.
type TenantsObserved struct {
	Control     bool           `json:"control,omitempty"`
	Unreachable string         `json:"unreachable,omitempty"`
	Rooted      bool           `json:"rooted,omitempty"`
	RootID      string         `json:"rootID,omitempty"`
	Children    []string       `json:"children,omitempty"`
	Agents      []string       `json:"agents,omitempty"`
	Objects     []TenantObject `json:"objects,omitempty"`
}

func (s *Tenants) Name() string          { return "tenants" }
func (s *Tenants) NewObserved() Observed { return &TenantsObserved{} }

func (s *Tenants) Observe(ctx context.Context) (Observed, error) {
	obs := &TenantsObserved{}
	zone, h, why, err := observeSilod(ctx, s.Env)
	if err != nil || zone == "" {
		return obs, err
	}
	obs.Control = true
	if h == nil {
		obs.Unreachable = why
		return obs, nil
	}
	if !h.Root {
		return obs, nil
	}
	obs.Rooted = true
	sd := s.Env.Cloud.Silod
	tree, err := sd.Tree(ctx)
	if err != nil {
		obs.Unreachable = "reading the tree: " + err.Error()
		return obs, nil
	}
	for i, n := range tree {
		switch {
		case i == 0:
			obs.RootID = n.ID
		case strings.Count(n.Path, "/") == 1:
			obs.Children = append(obs.Children, n.Path)
		}
	}
	sort.Strings(obs.Children)
	if obs.Agents, err = sd.Agents(ctx, "/"); err != nil {
		obs.Unreachable = "listing the root's agents: " + err.Error()
		return obs, nil
	}
	sort.Strings(obs.Agents)
	objs, err := listTenantObjects(ctx, s.Env)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		if o.Silo == obs.RootID {
			obs.Objects = append(obs.Objects, o)
		}
	}
	return obs, nil
}

// Plan: install has nothing to do here.
func (s *Tenants) Plan(Observed) []Change { return []Change{{Action: ActionOK, Detail: "uninstall's"}} }

func (s *Tenants) PlanRemove(o Observed) []Change {
	obs := o.(*TenantsObserved)
	force := s.Env.Config.Force
	switch {
	case !obs.Control:
		return []Change{{Action: ActionOK, Detail: "no " + ControlVM + ": no silod to hold a tree (orphans finds what one left)"}}
	case obs.Unreachable != "" && !force:
		return []Change{{Action: ActionFail, Object: "/", Detail: "can't list the tree's silos: " + obs.Unreachable,
			Why: "uninstall deletes no tenant's silos or objects without --force",
			Fix: "start " + ControlVM + " and run again, or run again with --force, which deletes every object silod marked"}}
	case obs.Unreachable != "":
		return []Change{{Action: ActionWarn, Object: "/", Detail: "can't ask silod (" + obs.Unreachable + "): the orphans step deletes every object silod marked"}}
	case !obs.Rooted:
		return []Change{{Action: ActionOK, Detail: "silod has no root"}}
	}
	if len(obs.Children)+len(obs.Agents)+len(obs.Objects) == 0 {
		return []Change{{Action: ActionOK, Object: "/", Detail: "no silo but the root, and no facade objects or agents in it"}}
	}
	if !force {
		var list []string
		for _, c := range obs.Children {
			list = append(list, "silo "+c)
		}
		for _, a := range obs.Agents {
			list = append(list, "agent /agents/"+a)
		}
		for _, o := range obs.Objects {
			list = append(list, o.String())
		}
		return []Change{{Action: ActionFail, Object: "/", Detail: "the tree isn't empty:\n" + strings.Join(list, "\n"),
			Why: "uninstall deletes tenants' silos, agents and VMs only when asked",
			Fix: "silo delete --force each of them, or run again with --force"}}
	}
	var out []Change
	for _, c := range obs.Children {
		out = append(out, Change{Action: ActionDelete, Object: "silo " + c, Detail: "silo delete --force: its subtree and every object in it",
			Estimate: 2 * time.Minute, Waiting: "silod"})
	}
	for _, a := range obs.Agents {
		out = append(out, Change{Action: ActionDelete, Object: "agent /agents/" + a, Estimate: 30 * time.Second, Waiting: "silod"})
	}
	return append(out, objectChanges(obs.Objects, "the root's")...)
}

// Apply deletes the subtrees first, waiting for each, then the root's
// agents, then the root's own objects: deleting a child may stop an
// agent's tool VM, and an agent's objects are its silo's.
func (s *Tenants) Apply(ctx context.Context, changes []Change) error {
	sd := s.Env.Cloud.Silod
	for _, ch := range changes {
		if ch.Action != ActionDelete {
			continue
		}
		switch {
		case strings.HasPrefix(ch.Object, "silo "):
			path := strings.TrimPrefix(ch.Object, "silo ")
			if err := sd.DeleteSilo(ctx, path); err != nil {
				return fmt.Errorf("deleting %s: %w", path, err)
			}
		case strings.HasPrefix(ch.Object, "agent /agents/"):
			name := strings.TrimPrefix(ch.Object, "agent /agents/")
			if err := sd.DeleteAgent(ctx, "/", name); err != nil {
				return fmt.Errorf("deleting agent %s: %w", name, err)
			}
		}
	}
	for _, ch := range changes {
		if ch.Action != ActionDelete {
			continue
		}
		if o, ok := objectFromChange(ch); ok {
			if err := deleteObject(ctx, s.Env, o); err != nil {
				return err
			}
		}
	}
	return nil
}

// Orphans is uninstall's second step, orphans: every object with
// silod's marks for a silo silod doesn't know (gcp-backend.md
// "Reconcile"), deleted with --force. Without --force uninstall stops
// and lists them: the subnetworks and firewall rules among them would
// keep the VPC from being deleted, and the VMs would outlive the fence.
// With no silod to ask (silo-control gone, or unreachable), every
// marked object is an orphan.
type Orphans struct{ Env *Env }

// OrphansObserved is every marked object, and the silos silod knows.
type OrphansObserved struct {
	ComputeEnabled bool           `json:"computeEnabled"`
	Known          []string       `json:"known,omitempty"`
	Objects        []TenantObject `json:"objects,omitempty"`
}

func (s *Orphans) Name() string          { return "orphans" }
func (s *Orphans) NewObserved() Observed { return &OrphansObserved{} }

func (s *Orphans) Observe(ctx context.Context) (Observed, error) {
	obs := &OrphansObserved{}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	obs.ComputeEnabled = true
	if _, h, _, err := observeSilod(ctx, s.Env); err != nil {
		return nil, err
	} else if h != nil && h.Root {
		if tree, err := s.Env.Cloud.Silod.Tree(ctx); err == nil {
			for _, n := range tree {
				obs.Known = append(obs.Known, n.ID)
			}
			sort.Strings(obs.Known)
		}
	}
	if obs.Objects, err = listTenantObjects(ctx, s.Env); err != nil {
		return nil, err
	}
	return obs, nil
}

func (s *Orphans) Plan(Observed) []Change { return []Change{{Action: ActionOK, Detail: "uninstall's"}} }

func (s *Orphans) orphans(obs *OrphansObserved) []TenantObject {
	var out []TenantObject
	for _, o := range obs.Objects {
		if !contains(obs.Known, o.Silo) {
			out = append(out, o)
		}
	}
	return out
}

func (s *Orphans) PlanRemove(o Observed) []Change {
	obs := o.(*OrphansObserved)
	orphans := s.orphans(obs)
	if len(orphans) == 0 {
		return []Change{{Action: ActionOK, Detail: "no object with a silo's label or prefix that silod doesn't know"}}
	}
	if !s.Env.Config.Force {
		var list []string
		for _, o := range orphans {
			list = append(list, o.String()+" (silo "+o.Silo+")")
		}
		return []Change{{Action: ActionFail, Detail: "objects with a silo's label or prefix that silod doesn't know:\n" + strings.Join(list, "\n"),
			Why: "uninstall deletes objects it didn't make only when asked, and those in the VPC would keep it from being deleted",
			Fix: "delete them, or run again with --force"}}
	}
	return objectChanges(orphans, "an orphan: silod doesn't know its silo")
}

func (s *Orphans) Apply(ctx context.Context, changes []Change) error {
	for _, ch := range changes {
		if ch.Action != ActionDelete {
			continue
		}
		if o, ok := objectFromChange(ch); ok {
			if err := deleteObject(ctx, s.Env, o); err != nil {
				return err
			}
		}
	}
	return nil
}

// Credentials is uninstall's third step, credentials: silod deletes
// every agent credential from the database, and silo-credential-key
// goes from the control VMs' metadata, before any of install's objects
// (agents.md "Credentials"). A kept database, or its final backup, then
// holds no credentials, and older backups hold them only encrypted with
// a key that is gone.
//
// A silod from before the credentials endpoint answers 404, and holds
// none: the step is then ok, and starts deleting them as soon as silod
// serves the endpoint.
type Credentials struct{ Env *Env }

// CredentialsObserved is the credentials silod holds, and the control
// VMs (zone/name) whose metadata holds the key.
type CredentialsObserved struct {
	Unreachable string   `json:"unreachable,omitempty"`
	NoAPI       bool     `json:"noAPI,omitempty"`
	Credentials []string `json:"credentials,omitempty"`
	KeyHolders  []string `json:"keyHolders,omitempty"`
}

func (s *Credentials) Name() string          { return "credentials" }
func (s *Credentials) NewObserved() Observed { return &CredentialsObserved{} }

func (s *Credentials) Observe(ctx context.Context) (Observed, error) {
	obs := &CredentialsObserved{}
	zone, h, why, err := observeSilod(ctx, s.Env)
	if err != nil {
		return nil, err
	}
	if zone == "" {
		on, err := computeOn(ctx, s.Env)
		if err != nil || !on {
			return obs, err
		}
	}
	switch {
	case zone == "":
	case h == nil:
		obs.Unreachable = why
	default:
		names, err := s.Env.Cloud.Silod.Credentials(ctx)
		switch {
		case errors.Is(err, ErrNoCredentialAPI):
			obs.NoAPI = true
		case err != nil:
			obs.Unreachable = "listing credentials: " + err.Error()
		default:
			sort.Strings(names)
			obs.Credentials = names
		}
	}
	if obs.KeyHolders, err = s.keyHolders(ctx); err != nil {
		return nil, err
	}
	return obs, nil
}

// keyHolders are this install's VMs in the home region, silo-control and
// its replicas, whose metadata holds the key.
func (s *Credentials) keyHolders(ctx context.Context) ([]string, error) {
	c, p := s.Env.Cloud.Compute, s.Env.Config.Project
	zones, err := c.ListZones(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("listing zones: %w", err)
	}
	var out []string
	for _, z := range zones {
		if gce.LastSegment(z.Region) != s.Env.Config.HomeRegion() && gce.ZoneRegion(z.Name) != s.Env.Config.HomeRegion() {
			continue
		}
		insts, err := c.ListInstances(ctx, p, z.Name, gce.ListFilter{Labels: map[string]string{LabelKey: s.Env.ID}})
		if err != nil {
			return nil, fmt.Errorf("listing instances in %s: %w", z.Name, err)
		}
		for _, i := range insts {
			if i.Metadata == nil {
				continue
			}
			for _, it := range i.Metadata.Items {
				if it.Key == CredentialKeyKey {
					out = append(out, z.Name+"/"+i.Name)
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Credentials) Plan(Observed) []Change {
	return []Change{{Action: ActionOK, Detail: "uninstall's"}}
}

func (s *Credentials) PlanRemove(o Observed) []Change {
	obs := o.(*CredentialsObserved)
	var out []Change
	if obs.Unreachable != "" && len(obs.KeyHolders) > 0 {
		out = append(out, Change{Action: ActionWarn, Detail: "can't ask silod to delete the agent credentials (" + obs.Unreachable +
			"): the key goes, so what the database holds can't be decrypted"})
	}
	for _, c := range obs.Credentials {
		out = append(out, Change{Action: ActionDelete, Object: "credential " + c, Detail: "from the database", Estimate: 2 * time.Second})
	}
	for _, h := range obs.KeyHolders {
		out = append(out, Change{Action: ActionDelete, Object: h, Detail: CredentialKeyKey + " from its metadata", Estimate: 10 * time.Second})
	}
	if len(out) > 0 {
		return out
	}
	if obs.NoAPI {
		return []Change{{Action: ActionOK, Detail: "silod holds no agent credentials (it has no credentials endpoint)"}}
	}
	return []Change{{Action: ActionOK, Detail: "no agent credentials, and no control VM holds " + CredentialKeyKey}}
}

// Apply deletes the credentials, then the key: a credential silod still
// held once the key was gone could never be deleted by decrypting it,
// but it could still be read by whoever later found the key.
func (s *Credentials) Apply(ctx context.Context, changes []Change) error {
	for _, ch := range changes {
		if name, ok := strings.CutPrefix(ch.Object, "credential "); ok && ch.Action == ActionDelete {
			if err := s.Env.Cloud.Silod.DeleteCredential(ctx, name); err != nil {
				return fmt.Errorf("deleting credential %s: %w", name, err)
			}
		}
	}
	c, p := s.Env.Cloud.Compute, s.Env.Config.Project
	for _, ch := range changes {
		zone, name, ok := strings.Cut(ch.Object, "/")
		if !ok || ch.Action != ActionDelete || strings.HasPrefix(ch.Object, "credential ") {
			continue
		}
		vm, err := c.GetInstance(ctx, p, zone, name)
		if IsNotFound(err) || gce.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if vm.Metadata == nil {
			continue
		}
		md := &compute.Metadata{Fingerprint: vm.Metadata.Fingerprint}
		for _, it := range vm.Metadata.Items {
			if it.Key != CredentialKeyKey {
				md.Items = append(md.Items, it)
			}
		}
		if len(md.Items) == len(vm.Metadata.Items) {
			continue
		}
		op, err := c.SetInstanceMetadata(ctx, p, zone, name, md)
		if err == nil {
			err = await(ctx, c, p, op)
		}
		if err != nil {
			return fmt.Errorf("removing %s from %s: %w", CredentialKeyKey, name, err)
		}
	}
	return nil
}
