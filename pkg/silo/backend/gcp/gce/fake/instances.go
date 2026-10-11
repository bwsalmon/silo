package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

var labelRE = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
var labelValueRE = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)

func checkLabels(field string, labels map[string]string) error {
	for k, v := range labels {
		if !labelRE.MatchString(k) || !labelValueRE.MatchString(v) {
			return invalid(field, "Label %q=%q is not valid", k, v)
		}
	}
	return nil
}

// filtered is objs that pass lf, by name and labels.
func filtered[T any](objs []*T, lf gce.ListFilter, key func(*T) (string, map[string]string)) []*T {
	out := []*T{}
	for _, o := range objs {
		if name, labels := key(o); lf.Match(name, labels) {
			out = append(out, clone(o))
		}
	}
	return out
}

func zoneScope(zone string) string     { return "zones/" + zone }
func regionScope(region string) string { return "regions/" + region }

// instance finds name in zone, under the lock.
func (f *Fake) instance(p *state, zone, name string) (*compute.Instance, error) {
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	inst := p.instances.get(zoneScope(zone), name)
	if inst == nil {
		return nil, notFound(p.link(zoneScope(zone), "instances", name))
	}
	return inst, nil
}

// InsertInstance implements gce.Client. Everything GCE checks before
// starting the operation is checked here; quota is checked when the
// operation finishes. The instance and its new disks exist, PROVISIONING
// and holding their names, until then.
func (f *Fake) InsertInstance(ctx context.Context, project, zone string, body *compute.Instance, sourceInstanceTemplate string) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	inst := clone(body)
	if sourceInstanceTemplate != "" {
		if inst, err = f.mergeTemplate(p, zone, inst, sourceInstanceTemplate); err != nil {
			return nil, err
		}
	}
	if err := checkName("resource.name", inst.Name); err != nil {
		return nil, err
	}
	scope := zoneScope(zone)
	link := p.link(scope, "instances", inst.Name)
	if p.instances.get(scope, inst.Name) != nil {
		return nil, alreadyExists(link)
	}
	if err := checkLabels("resource.labels", inst.Labels); err != nil {
		return nil, err
	}
	mt, err := f.resolveMachineType(zone, inst.MachineType, "resource.machineType")
	if err != nil {
		return nil, err
	}
	if s := inst.Scheduling; s != nil && s.ProvisioningModel != "" && s.ProvisioningModel != "SPOT" && s.ProvisioningModel != "STANDARD" {
		return nil, invalid("resource.scheduling.provisioningModel", "%s", s.ProvisioningModel)
	}
	var accels []accel
	for _, a := range inst.GuestAccelerators {
		accels = append(accels, accel{gce.LastSegment(a.AcceleratorType), a.AcceleratorCount})
	}
	var ohm string
	if inst.Scheduling != nil {
		ohm = inst.Scheduling.OnHostMaintenance
	}
	if err := f.checkGPUs(zone, mt, accels, ohm, isSpot(inst.Scheduling)); err != nil {
		return nil, err
	}
	for i, a := range inst.GuestAccelerators {
		a.AcceleratorType = p.link(scope, "acceleratorTypes", accels[i].typ)
	}
	if len(inst.NetworkInterfaces) == 0 {
		return nil, invalid("resource.networkInterfaces", "At least one network interface is required")
	}
	for i, nic := range inst.NetworkInterfaces {
		if err := f.attachNIC(p, zone, i, nic); err != nil {
			return nil, err
		}
	}
	newDisks, err := f.instanceDisks(p, zone, inst, mt, link)
	if err != nil {
		return nil, err
	}

	inst.Kind = "compute#instance"
	inst.Id = f.nextID()
	inst.SelfLink = link
	inst.Zone = gce.SelfLinkBase + "projects/" + p.name + "/" + scope
	inst.MachineType = p.link(scope, "machineTypes", mt.Name)
	inst.Status = "PROVISIONING"
	inst.CreationTimestamp = f.timestamp()
	inst.LabelFingerprint = f.fingerprint()
	inst.CpuPlatform = "Intel Cascade Lake"
	if inst.Metadata == nil {
		inst.Metadata = &compute.Metadata{}
	}
	inst.Metadata.Kind = "compute#metadata"
	inst.Metadata.Fingerprint = f.fingerprint()
	if inst.Tags == nil {
		inst.Tags = &compute.Tags{}
	}
	inst.Tags.Fingerprint = f.fingerprint()
	if inst.Scheduling == nil {
		inst.Scheduling = &compute.Scheduling{}
	}
	if inst.Scheduling.OnHostMaintenance == "" {
		inst.Scheduling.OnHostMaintenance = "MIGRATE"
		if isSpot(inst.Scheduling) {
			inst.Scheduling.OnHostMaintenance = "TERMINATE"
		}
	}
	if inst.Scheduling.ProvisioningModel == "" {
		inst.Scheduling.ProvisioningModel = "STANDARD"
	}

	p.instances.put(scope, inst.Name, inst)
	p.pending[link] = true
	for _, d := range newDisks {
		p.disks.put(scope, d.Name, d)
		p.pending[d.SelfLink] = true
	}
	setPending := func(on bool) {
		for _, l := range append([]string{link}, links(newDisks)...) {
			if on {
				p.pending[l] = true
			} else {
				delete(p.pending, l)
			}
		}
	}
	commit := func() *opErr {
		if p.instances.get(scope, inst.Name) != inst {
			return nil
		}
		return f.withQuota(func() func() {
			setPending(false)
			inst.Status = "RUNNING"
			for _, d := range newDisks {
				d.Status = "READY"
			}
			return func() { setPending(true) }
		})
	}
	abort := func() {
		setPending(false)
		if p.instances.get(scope, inst.Name) == inst {
			p.instances.del(scope, inst.Name)
		}
		for _, d := range newDisks {
			if p.disks.get(scope, d.Name) == d {
				p.disks.del(scope, d.Name)
			}
		}
		for _, ad := range inst.Disks {
			if d := f.diskAt(p, ad.Source); d != nil {
				d.Users = slices.DeleteFunc(d.Users, func(u string) bool { return u == link })
			}
		}
	}
	return f.start(p, "instances.insert", link, inst.Id, commit, abort), nil
}

func links(ds []*compute.Disk) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.SelfLink)
	}
	return out
}

// diskAt is the disk at a selfLink, or nil.
func (f *Fake) diskAt(p *state, link string) *compute.Disk {
	ref := gce.ParseRef(link)
	return p.disks.get(zoneScope(ref.Zone), ref.Name)
}

func (f *Fake) resolveMachineType(zone, ref, field string) (*MachineTypeSpec, error) {
	r := gce.ParseRef(ref)
	if r.Name == "" {
		return nil, gce.NewError(http.StatusBadRequest, "required", "Required field '%s' not specified", field)
	}
	if r.Zone != "" && r.Zone != zone {
		return nil, invalid(field, "Machine type %s is in zone %s, not %s", r.Name, r.Zone, zone)
	}
	mt := f.catalog.machineType(zone, r.Name)
	if mt == nil {
		return nil, invalid(field, "Machine type with name '%s' does not exist in zone '%s'", r.Name, zone)
	}
	return mt, nil
}

// attachNIC resolves nic's network and subnetwork and gives it an
// address in the subnetwork.
func (f *Fake) attachNIC(p *state, zone string, i int, nic *compute.NetworkInterface) error {
	field := fmt.Sprintf("resource.networkInterfaces[%d]", i)
	region := gce.ZoneRegion(zone)
	netName := "default"
	if nic.Network != "" {
		netName = gce.ParseRef(nic.Network).Name
	}
	network := p.networks.get("global", netName)
	if network == nil {
		return invalid(field+".network", "The resource 'projects/%s/global/networks/%s' was not found", p.name, netName)
	}
	var subnet *compute.Subnetwork
	if nic.Subnetwork != "" {
		r := gce.ParseRef(nic.Subnetwork)
		if r.Region != "" && r.Region != region {
			return invalid(field+".subnetwork", "Subnetwork %s is in region %s, not %s", r.Name, r.Region, region)
		}
		subnet = p.subnets.get(regionScope(region), r.Name)
		if subnet == nil {
			return invalid(field+".subnetwork", "The resource 'projects/%s/regions/%s/subnetworks/%s' was not found", p.name, region, r.Name)
		}
		if subnet.Network != network.SelfLink {
			return invalid(field+".subnetwork", "Subnetwork %s is not in network %s", r.Name, netName)
		}
	} else {
		for _, s := range p.subnets.list(regionScope(region)) {
			if s.Network == network.SelfLink {
				subnet = s
				break
			}
		}
		if subnet == nil {
			return invalid(field+".subnetwork", "Network %s has no subnetwork in region %s", netName, region)
		}
	}
	prefix := netip.MustParsePrefix(subnet.IpCidrRange)
	used := f.usedIPs(p, subnet.SelfLink)
	if nic.NetworkIP != "" {
		ip, err := netip.ParseAddr(nic.NetworkIP)
		if err != nil || !prefix.Contains(ip) || ip == prefix.Addr() || ip == prefix.Addr().Next() {
			return invalid(field+".networkIP", "IP address %s is not in subnetwork %s's usable range", nic.NetworkIP, subnet.Name)
		}
		if used[ip] {
			return invalid(field+".networkIP", "IP address '%s' is already being used by another resource", nic.NetworkIP)
		}
	} else {
		ip := prefix.Addr().Next().Next()
		for used[ip] {
			ip = ip.Next()
		}
		if !prefix.Contains(ip) {
			return gce.NewError(http.StatusBadRequest, "ipSpaceExhausted", "IP space of '%s' is exhausted", trimBase(subnet.SelfLink))
		}
		nic.NetworkIP = ip.String()
	}
	for j, alias := range nic.AliasIpRanges {
		ap, err := netip.ParsePrefix(alias.IpCidrRange)
		if err != nil {
			return invalid(fmt.Sprintf("%s.aliasIpRanges[%d].ipCidrRange", field, j), "%s", alias.IpCidrRange)
		}
		within := prefix
		if alias.SubnetworkRangeName != "" {
			within = netip.Prefix{}
			for _, r := range subnet.SecondaryIpRanges {
				if r.RangeName == alias.SubnetworkRangeName {
					within = netip.MustParsePrefix(r.IpCidrRange)
				}
			}
		}
		if !within.IsValid() || !contains(within, ap) {
			return invalid(fmt.Sprintf("%s.aliasIpRanges[%d].ipCidrRange", field, j), "%s is not within the subnetwork's range", alias.IpCidrRange)
		}
	}
	nic.Kind = "compute#networkInterface"
	nic.Name = fmt.Sprintf("nic%d", i)
	nic.Network = network.SelfLink
	nic.Subnetwork = subnet.SelfLink
	return nil
}

func (f *Fake) usedIPs(p *state, subnetLink string) map[netip.Addr]bool {
	used := map[netip.Addr]bool{}
	for _, inst := range p.instances.all() {
		for _, nic := range inst.NetworkInterfaces {
			if nic.Subnetwork == subnetLink {
				if ip, err := netip.ParseAddr(nic.NetworkIP); err == nil {
					used[ip] = true
				}
			}
		}
	}
	return used
}

// instanceDisks checks inst's disks, makes the new ones (returned, not
// stored), and marks existing ones as used by link.
func (f *Fake) instanceDisks(p *state, zone string, inst *compute.Instance, mt *MachineTypeSpec, link string) ([]*compute.Disk, error) {
	scope := zoneScope(zone)
	var newDisks []*compute.Disk
	var existing []*compute.Disk
	boot := false
	scratch := 0
	for i, ad := range inst.Disks {
		field := fmt.Sprintf("resource.disks[%d]", i)
		ad.Kind = "compute#attachedDisk"
		ad.Index = int64(i)
		if ad.Type == "" {
			ad.Type = "PERSISTENT"
		}
		if ad.Mode == "" {
			ad.Mode = "READ_WRITE"
		}
		if ad.Type == "SCRATCH" {
			if ad.InitializeParams == nil || gce.LastSegment(ad.InitializeParams.DiskType) != localSSD {
				return nil, invalid(field+".initializeParams.diskType", "A SCRATCH disk must be of type local-ssd")
			}
			if ad.Interface == "" {
				ad.Interface = "SCSI"
			}
			if ad.Interface != "SCSI" && ad.Interface != "NVME" {
				return nil, invalid(field+".interface", "%s", ad.Interface)
			}
			if mt.LocalSSDPartitions > 0 || !slices.Contains(localSSDSeries, series(mt.Name)) {
				return nil, invalid(field, "Machine type %s does not support attaching local SSD", mt.Name)
			}
			ad.InitializeParams.DiskType = p.link(scope, "diskTypes", localSSD)
			ad.DiskSizeGb = LocalSSDPartitionGB
			ad.AutoDelete = true
			if ad.DeviceName == "" {
				ad.DeviceName = fmt.Sprintf("local-ssd-%d", scratch)
			}
			scratch++
			continue
		}
		if ad.Boot {
			if boot {
				return nil, invalid(field+".boot", "An instance can have only one boot disk")
			}
			boot = true
		}
		var d *compute.Disk
		switch {
		case ad.Source != "":
			r := gce.ParseRef(ad.Source)
			if r.Zone != "" && r.Zone != zone {
				return nil, invalid(field+".source", "Disk %s must be in zone %s", r.Name, zone)
			}
			d = p.disks.get(scope, r.Name)
			if d == nil || p.pending[d.SelfLink] {
				return nil, invalid(field+".source", "The resource 'projects/%s/zones/%s/disks/%s' was not found", p.name, zone, r.Name)
			}
			if len(d.Users) > 0 && ad.Mode != "READ_ONLY" && d.AccessMode != "READ_WRITE_MANY" {
				return nil, inUse("The disk resource '%s' is already being used by '%s'", trimBase(d.SelfLink), trimBase(d.Users[0]))
			}
			existing = append(existing, d)
		case ad.InitializeParams != nil:
			ip := ad.InitializeParams
			name := ip.DiskName
			if name == "" {
				name = inst.Name
				if !ad.Boot {
					name = fmt.Sprintf("%s-%d", inst.Name, i)
				}
			}
			var err error
			d, err = f.newDisk(p, zone, &compute.Disk{
				Name: name, SizeGb: ip.DiskSizeGb, Type: ip.DiskType, SourceImage: ip.SourceImage,
				SourceSnapshot: ip.SourceSnapshot, Labels: ip.Labels,
				ProvisionedIops: ip.ProvisionedIops, ProvisionedThroughput: ip.ProvisionedThroughput,
			}, field+".initializeParams")
			if err != nil {
				return nil, err
			}
			for _, nd := range newDisks {
				if nd.Name == d.Name {
					return nil, alreadyExists(d.SelfLink)
				}
			}
			ip.DiskType, ip.DiskSizeGb = d.Type, d.SizeGb
			ip.ProvisionedIops, ip.ProvisionedThroughput = d.ProvisionedIops, d.ProvisionedThroughput
			newDisks = append(newDisks, d)
			ad.InitializeParams = nil
			ad.AutoDelete = true
			ad.Source = d.SelfLink
		default:
			return nil, invalid(field, "A disk needs a source or initializeParams")
		}
		if k := diskKinds[gce.LastSegment(d.Type)]; k != nil && !k.attaches(mt) {
			return nil, invalid(field, "Disk type %s is not supported with machine type %s", k.name, mt.Name)
		}
		ad.Source = d.SelfLink
		ad.DiskSizeGb = d.SizeGb
		if ad.DeviceName == "" {
			ad.DeviceName = d.Name
		}
	}
	if !boot {
		return nil, invalid("resource.disks", "The instance must have a boot disk")
	}
	for _, d := range append(newDisks, existing...) {
		d.Users = append(d.Users, link)
	}
	return newDisks, nil
}

// mergeTemplate is the instance GCE makes from body and the template at
// src: body merged over the template's properties. Objects merge
// field by field, the body winning; lists are replaced, except
// metadata.items, which merge by key.
func (f *Fake) mergeTemplate(p *state, zone string, body *compute.Instance, src string) (*compute.Instance, error) {
	r := gce.ParseRef(src)
	scope := "global"
	if r.Region != "" {
		if r.Region != gce.ZoneRegion(zone) {
			return nil, invalid("sourceInstanceTemplate", "Regional template %s is in region %s, not the instance's", r.Name, r.Region)
		}
		scope = regionScope(r.Region)
	}
	if r.Project != "" && r.Project != p.name {
		return nil, invalid("sourceInstanceTemplate", "Template %s is in another project", src)
	}
	t := p.templates.get(scope, r.Name)
	if t == nil || p.pending[t.SelfLink] {
		return nil, notFound(p.link(scope, "instanceTemplates", r.Name))
	}
	base, err := toMap(t.Properties)
	if err != nil {
		return nil, err
	}
	over, err := toMap(body)
	if err != nil {
		return nil, err
	}
	merged := mergeMaps(base, over)
	if bm, ok := base["metadata"].(map[string]any); ok {
		if om, ok := over["metadata"].(map[string]any); ok {
			merged["metadata"].(map[string]any)["items"] = mergeItems(bm["items"], om["items"])
		}
	}
	b, _ := json.Marshal(merged)
	out := &compute.Instance{}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, err
	}
	return out, nil
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	return m, json.Unmarshal(b, &m)
}

// mergeMaps is over merged into base: objects recursively, anything
// else replaced.
func mergeMaps(base, over map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		bm, ok1 := out[k].(map[string]any)
		om, ok2 := v.(map[string]any)
		if ok1 && ok2 {
			out[k] = mergeMaps(bm, om)
		} else {
			out[k] = v
		}
	}
	return out
}

func mergeItems(base, over any) []any {
	var out []any
	idx := map[any]int{}
	for _, list := range []any{base, over} {
		items, _ := list.([]any)
		for _, it := range items {
			key := it.(map[string]any)["key"]
			if i, ok := idx[key]; ok {
				out[i] = it
				continue
			}
			idx[key] = len(out)
			out = append(out, it)
		}
	}
	return out
}

// GetInstance implements gce.Client.
func (f *Fake) GetInstance(ctx context.Context, project, zone, name string) (*compute.Instance, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	inst, err := f.instance(p, zone, name)
	if err != nil {
		return nil, err
	}
	return clone(inst), nil
}

// ListInstances implements gce.Client.
func (f *Fake) ListInstances(ctx context.Context, project, zone string, lf gce.ListFilter) ([]*compute.Instance, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	return filtered(p.instances.list(zoneScope(zone)), lf, func(i *compute.Instance) (string, map[string]string) { return i.Name, i.Labels }), nil
}

// instanceOp checks the instance, then runs check (under the lock) and
// starts method's operation, whose commit is apply. quota runs apply
// under the quota check.
func (f *Fake) instanceOp(ctx context.Context, project, zone, name, method string, quota bool, check func(p *state, inst *compute.Instance) error, apply func(p *state, inst *compute.Instance) func()) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	inst, err := f.instance(p, zone, name)
	if err != nil {
		return nil, err
	}
	if p.pending[inst.SelfLink] && method != "instances.delete" {
		return nil, gce.NewError(http.StatusBadRequest, gce.ReasonNotReady, "The resource '%s' is not ready", trimBase(inst.SelfLink))
	}
	if check != nil {
		if err := check(p, inst); err != nil {
			return nil, err
		}
	}
	commit := func() *opErr {
		if p.instances.get(zoneScope(zone), name) != inst {
			return &opErr{"RESOURCE_NOT_FOUND", http.StatusNotFound, "The resource '" + trimBase(inst.SelfLink) + "' was not found"}
		}
		if !quota {
			apply(p, inst)
			return nil
		}
		return f.withQuota(func() func() { return apply(p, inst) })
	}
	return f.start(p, method, inst.SelfLink, inst.Id, commit, nil), nil
}

func notReady(inst *compute.Instance, why string) error {
	return gce.NewError(http.StatusBadRequest, gce.ReasonNotReady, "The resource '%s' is not ready: %s", trimBase(inst.SelfLink), why)
}

// setStatus is an apply that moves an instance to status.
func setStatus(status string) func(*state, *compute.Instance) func() {
	return func(_ *state, inst *compute.Instance) func() {
		old := inst.Status
		inst.Status = status
		return func() { inst.Status = old }
	}
}

// DeleteInstance implements gce.Client. Disks attached with autoDelete
// go with the instance.
func (f *Fake) DeleteInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return f.instanceOp(ctx, project, zone, name, "instances.delete", false,
		func(_ *state, inst *compute.Instance) error {
			if inst.DeletionProtection {
				return gce.NewError(http.StatusBadRequest, "resourceIsProtected",
					"Invalid resource usage: 'Resource cannot be deleted if it's protected against deletion.'")
			}
			return nil
		},
		func(p *state, inst *compute.Instance) func() {
			p.instances.del(zoneScope(gce.LastSegment(inst.Zone)), inst.Name)
			delete(p.pending, inst.SelfLink)
			for _, ad := range inst.Disks {
				d := f.diskAt(p, ad.Source)
				if d == nil {
					continue
				}
				d.Users = slices.DeleteFunc(d.Users, func(u string) bool { return u == inst.SelfLink })
				if ad.AutoDelete && len(d.Users) == 0 {
					p.disks.del(zoneScope(gce.LastSegment(d.Zone)), d.Name)
					delete(p.pending, d.SelfLink)
				}
			}
			return nil
		})
}

// StartInstance implements gce.Client. Starting takes vCPU and GPU
// quota again, and can fail QUOTA_EXCEEDED.
func (f *Fake) StartInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return f.instanceOp(ctx, project, zone, name, "instances.start", true,
		func(_ *state, inst *compute.Instance) error {
			if inst.Status == "SUSPENDED" {
				return notReady(inst, "a suspended instance is resumed, not started")
			}
			return nil
		}, setStatus("RUNNING"))
}

// StopInstance implements gce.Client.
func (f *Fake) StopInstance(ctx context.Context, project, zone, name string, discardLocalSsd bool) (*compute.Operation, error) {
	return f.instanceOp(ctx, project, zone, name, "instances.stop", false, nil, setStatus("TERMINATED"))
}

// ResetInstance implements gce.Client.
func (f *Fake) ResetInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return f.instanceOp(ctx, project, zone, name, "instances.reset", false,
		func(_ *state, inst *compute.Instance) error {
			if inst.Status != "RUNNING" {
				return notReady(inst, "only a running instance can be reset")
			}
			return nil
		}, setStatus("RUNNING"))
}

// SuspendInstance implements gce.Client. GCE can't suspend an instance
// with GPUs.
func (f *Fake) SuspendInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return f.instanceOp(ctx, project, zone, name, "instances.suspend", false,
		func(_ *state, inst *compute.Instance) error {
			if inst.Status != "RUNNING" {
				return notReady(inst, "only a running instance can be suspended")
			}
			mt := f.catalog.machineType(gce.LastSegment(inst.Zone), gce.LastSegment(inst.MachineType))
			if mt != nil && len(instanceGPUs(inst, mt)) > 0 {
				return badRequest("Instances with GPUs can't be suspended")
			}
			return nil
		}, setStatus("SUSPENDED"))
}

// ResumeInstance implements gce.Client.
func (f *Fake) ResumeInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return f.instanceOp(ctx, project, zone, name, "instances.resume", true,
		func(_ *state, inst *compute.Instance) error {
			if inst.Status != "SUSPENDED" {
				return notReady(inst, "only a suspended instance can be resumed")
			}
			return nil
		}, setStatus("RUNNING"))
}

// AttachDisk implements gce.Client.
func (f *Fake) AttachDisk(ctx context.Context, project, zone, name string, ad *compute.AttachedDisk) (*compute.Operation, error) {
	ad = clone(ad)
	var d *compute.Disk
	return f.instanceOp(ctx, project, zone, name, "instances.attachDisk", false,
		func(p *state, inst *compute.Instance) error {
			r := gce.ParseRef(ad.Source)
			if r.Zone != "" && r.Zone != zone {
				return invalid("resource.source", "Disk %s must be in zone %s", r.Name, zone)
			}
			d = p.disks.get(zoneScope(zone), r.Name)
			if d == nil || p.pending[d.SelfLink] {
				return notFound(p.link(zoneScope(zone), "disks", r.Name))
			}
			if ad.Mode == "" {
				ad.Mode = "READ_WRITE"
			}
			if len(d.Users) > 0 && ad.Mode != "READ_ONLY" && d.AccessMode != "READ_WRITE_MANY" {
				return inUse("The disk resource '%s' is already being used by '%s'", trimBase(d.SelfLink), trimBase(d.Users[0]))
			}
			mt := f.catalog.machineType(zone, gce.LastSegment(inst.MachineType))
			if k := diskKinds[gce.LastSegment(d.Type)]; k != nil && mt != nil && !k.attaches(mt) {
				return invalid("resource", "Disk type %s is not supported with machine type %s", k.name, mt.Name)
			}
			if ad.DeviceName == "" {
				ad.DeviceName = d.Name
			}
			for _, x := range inst.Disks {
				if x.DeviceName == ad.DeviceName {
					return invalid("resource.deviceName", "Device name %s is already in use", ad.DeviceName)
				}
			}
			return nil
		},
		func(p *state, inst *compute.Instance) func() {
			ad.Kind, ad.Type, ad.Source, ad.DiskSizeGb = "compute#attachedDisk", "PERSISTENT", d.SelfLink, d.SizeGb
			ad.Index = int64(len(inst.Disks))
			inst.Disks = append(inst.Disks, ad)
			d.Users = append(d.Users, inst.SelfLink)
			return nil
		})
}

// DetachDisk implements gce.Client.
func (f *Fake) DetachDisk(ctx context.Context, project, zone, name, deviceName string) (*compute.Operation, error) {
	return f.instanceOp(ctx, project, zone, name, "instances.detachDisk", false,
		func(_ *state, inst *compute.Instance) error {
			for _, ad := range inst.Disks {
				if ad.DeviceName == deviceName {
					if ad.Boot && inst.Status != "TERMINATED" {
						return notReady(inst, "the boot disk can only be detached from a stopped instance")
					}
					return nil
				}
			}
			return invalid("deviceName", "No attached disk found with device name '%s'", deviceName)
		},
		func(p *state, inst *compute.Instance) func() {
			inst.Disks = slices.DeleteFunc(inst.Disks, func(ad *compute.AttachedDisk) bool {
				if ad.DeviceName != deviceName {
					return false
				}
				if d := f.diskAt(p, ad.Source); d != nil {
					d.Users = slices.DeleteFunc(d.Users, func(u string) bool { return u == inst.SelfLink })
				}
				return true
			})
			return nil
		})
}

// SetInstanceLabels implements gce.Client. The fingerprint must match.
func (f *Fake) SetInstanceLabels(ctx context.Context, project, zone, name string, req *compute.InstancesSetLabelsRequest) (*compute.Operation, error) {
	labels, fp := req.Labels, req.LabelFingerprint
	return f.instanceOp(ctx, project, zone, name, "instances.setLabels", false,
		func(_ *state, inst *compute.Instance) error {
			if fp != inst.LabelFingerprint {
				return conditionNotMet("Labels fingerprint either invalid or resource labels have changed")
			}
			return checkLabels("labels", labels)
		},
		func(_ *state, inst *compute.Instance) func() {
			inst.Labels = labels
			inst.LabelFingerprint = f.fingerprint()
			return nil
		})
}

// SetInstanceMetadata implements gce.Client. The fingerprint must match.
func (f *Fake) SetInstanceMetadata(ctx context.Context, project, zone, name string, md *compute.Metadata) (*compute.Operation, error) {
	md = clone(md)
	return f.instanceOp(ctx, project, zone, name, "instances.setMetadata", false,
		func(_ *state, inst *compute.Instance) error {
			if md.Fingerprint != inst.Metadata.Fingerprint {
				return conditionNotMet("Supplied fingerprint does not match current metadata fingerprint")
			}
			return nil
		},
		func(_ *state, inst *compute.Instance) func() {
			md.Kind = "compute#metadata"
			md.Fingerprint = f.fingerprint()
			inst.Metadata = md
			return nil
		})
}

// SetInstanceTags implements gce.Client. The fingerprint must match.
func (f *Fake) SetInstanceTags(ctx context.Context, project, zone, name string, tags *compute.Tags) (*compute.Operation, error) {
	tags = clone(tags)
	return f.instanceOp(ctx, project, zone, name, "instances.setTags", false,
		func(_ *state, inst *compute.Instance) error {
			if tags.Fingerprint != inst.Tags.Fingerprint {
				return conditionNotMet("Supplied fingerprint does not match current tags fingerprint")
			}
			return nil
		},
		func(_ *state, inst *compute.Instance) func() {
			tags.Fingerprint = f.fingerprint()
			inst.Tags = tags
			return nil
		})
}

// SetMachineType implements gce.Client. The instance must be stopped,
// and the new type must suit its GPUs, local SSD and disks.
func (f *Fake) SetMachineType(ctx context.Context, project, zone, name, machineType string) (*compute.Operation, error) {
	var mt *MachineTypeSpec
	return f.instanceOp(ctx, project, zone, name, "instances.setMachineType", true,
		func(p *state, inst *compute.Instance) error {
			if inst.Status != "TERMINATED" {
				return notReady(inst, "the instance must be stopped to change its machine type")
			}
			var err error
			if mt, err = f.resolveMachineType(zone, machineType, "machineType"); err != nil {
				return err
			}
			var accels []accel
			for _, a := range inst.GuestAccelerators {
				accels = append(accels, accel{gce.LastSegment(a.AcceleratorType), a.AcceleratorCount})
			}
			if err := f.checkGPUs(zone, mt, accels, inst.Scheduling.OnHostMaintenance, isSpot(inst.Scheduling)); err != nil {
				return err
			}
			for i, ad := range inst.Disks {
				if ad.Type == "SCRATCH" {
					if mt.LocalSSDPartitions > 0 || !slices.Contains(localSSDSeries, series(mt.Name)) {
						return invalid(fmt.Sprintf("resource.disks[%d]", i), "Machine type %s does not support attaching local SSD", mt.Name)
					}
					continue
				}
				if d := f.diskAt(p, ad.Source); d != nil {
					if k := diskKinds[gce.LastSegment(d.Type)]; k != nil && !k.attaches(mt) {
						return invalid("machineType", "Disk type %s is not supported with machine type %s", k.name, mt.Name)
					}
				}
			}
			return nil
		},
		func(p *state, inst *compute.Instance) func() {
			old := inst.MachineType
			inst.MachineType = p.link(zoneScope(zone), "machineTypes", mt.Name)
			return func() { inst.MachineType = old }
		})
}

// SetDeletionProtection implements gce.Client.
func (f *Fake) SetDeletionProtection(ctx context.Context, project, zone, name string, protect bool) (*compute.Operation, error) {
	return f.instanceOp(ctx, project, zone, name, "instances.setDeletionProtection", false, nil,
		func(_ *state, inst *compute.Instance) func() {
			inst.DeletionProtection = protect
			return nil
		})
}

// GetSerialPortOutput implements gce.Client. The fake's instances print
// nothing.
func (f *Fake) GetSerialPortOutput(ctx context.Context, project, zone, name string, port, start int64) (*compute.SerialPortOutput, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	inst, err := f.instance(p, zone, name)
	if err != nil {
		return nil, err
	}
	if port == 0 {
		port = 1
	}
	return &compute.SerialPortOutput{Kind: "compute#serialPortOutput", Start: start, Next: start, SelfLink: inst.SelfLink + "/serialPort", Contents: ""}, nil
}

// contains reports whether outer contains inner entirely.
func contains(outer, inner netip.Prefix) bool {
	return outer.Bits() <= inner.Bits() && outer.Contains(inner.Masked().Addr())
}
