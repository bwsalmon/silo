package gcp

import (
	"context"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	compute "google.golang.org/api/compute/v1"
)

// lister is the part of a generated List call the paging needs.
type lister[C any, L any] interface {
	Pages(ctx context.Context, f func(L) error) error
	MaxResults(int64) C
}

// list follows call's pages and collects what items returns of each.
func list[C lister[C, L], L any, T any](ctx context.Context, c *Client, call C, items func(L) []*T) ([]*T, error) {
	if c.pageSize > 0 {
		call = call.MaxResults(c.pageSize)
	}
	var out []*T
	err := call.Pages(ctx, func(l L) error {
		out = append(out, items(l)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// filtered sets a list call's filter when f restricts anything.
func filtered[C interface{ Filter(string) C }](call C, f gce.ListFilter) C {
	if f.IsZero() {
		return call
	}
	return call.Filter(f.String())
}

// ---- Instances ----

func (c *Client) InsertInstance(ctx context.Context, project, zone string, inst *compute.Instance, sourceInstanceTemplate string) (*compute.Operation, error) {
	call := c.svc.Instances.Insert(project, zone, inst)
	if sourceInstanceTemplate != "" {
		call = call.SourceInstanceTemplate(sourceInstanceTemplate)
	}
	return call.Context(ctx).Do()
}

func (c *Client) GetInstance(ctx context.Context, project, zone, name string) (*compute.Instance, error) {
	return c.svc.Instances.Get(project, zone, name).Context(ctx).Do()
}

func (c *Client) ListInstances(ctx context.Context, project, zone string, f gce.ListFilter) ([]*compute.Instance, error) {
	return list(ctx, c, filtered(c.svc.Instances.List(project, zone), f),
		func(l *compute.InstanceList) []*compute.Instance { return l.Items })
}

func (c *Client) DeleteInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return c.svc.Instances.Delete(project, zone, name).Context(ctx).Do()
}

func (c *Client) StartInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return c.svc.Instances.Start(project, zone, name).Context(ctx).Do()
}

func (c *Client) StopInstance(ctx context.Context, project, zone, name string, discardLocalSsd bool) (*compute.Operation, error) {
	call := c.svc.Instances.Stop(project, zone, name)
	if discardLocalSsd {
		call = call.DiscardLocalSsd(true)
	}
	return call.Context(ctx).Do()
}

func (c *Client) ResetInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return c.svc.Instances.Reset(project, zone, name).Context(ctx).Do()
}

func (c *Client) SuspendInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return c.svc.Instances.Suspend(project, zone, name).Context(ctx).Do()
}

func (c *Client) ResumeInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return c.svc.Instances.Resume(project, zone, name).Context(ctx).Do()
}

func (c *Client) AttachDisk(ctx context.Context, project, zone, name string, disk *compute.AttachedDisk) (*compute.Operation, error) {
	return c.svc.Instances.AttachDisk(project, zone, name, disk).Context(ctx).Do()
}

func (c *Client) DetachDisk(ctx context.Context, project, zone, name, deviceName string) (*compute.Operation, error) {
	return c.svc.Instances.DetachDisk(project, zone, name, deviceName).Context(ctx).Do()
}

func (c *Client) SetInstanceLabels(ctx context.Context, project, zone, name string, req *compute.InstancesSetLabelsRequest) (*compute.Operation, error) {
	return c.svc.Instances.SetLabels(project, zone, name, req).Context(ctx).Do()
}

func (c *Client) SetInstanceMetadata(ctx context.Context, project, zone, name string, md *compute.Metadata) (*compute.Operation, error) {
	return c.svc.Instances.SetMetadata(project, zone, name, md).Context(ctx).Do()
}

func (c *Client) SetInstanceTags(ctx context.Context, project, zone, name string, tags *compute.Tags) (*compute.Operation, error) {
	return c.svc.Instances.SetTags(project, zone, name, tags).Context(ctx).Do()
}

func (c *Client) SetMachineType(ctx context.Context, project, zone, name, machineType string) (*compute.Operation, error) {
	return c.svc.Instances.SetMachineType(project, zone, name,
		&compute.InstancesSetMachineTypeRequest{MachineType: machineType}).Context(ctx).Do()
}

func (c *Client) SetDeletionProtection(ctx context.Context, project, zone, name string, protect bool) (*compute.Operation, error) {
	// The parameter defaults to true, so false is always sent.
	return c.svc.Instances.SetDeletionProtection(project, zone, name).DeletionProtection(protect).Context(ctx).Do()
}

func (c *Client) GetSerialPortOutput(ctx context.Context, project, zone, name string, port, start int64) (*compute.SerialPortOutput, error) {
	call := c.svc.Instances.GetSerialPortOutput(project, zone, name)
	if port != 0 {
		call = call.Port(port)
	}
	if start != 0 {
		call = call.Start(start)
	}
	return call.Context(ctx).Do()
}

// ---- Disks ----

func (c *Client) InsertDisk(ctx context.Context, project, zone string, disk *compute.Disk) (*compute.Operation, error) {
	return c.svc.Disks.Insert(project, zone, disk).Context(ctx).Do()
}

func (c *Client) GetDisk(ctx context.Context, project, zone, name string) (*compute.Disk, error) {
	return c.svc.Disks.Get(project, zone, name).Context(ctx).Do()
}

func (c *Client) ListDisks(ctx context.Context, project, zone string, f gce.ListFilter) ([]*compute.Disk, error) {
	return list(ctx, c, filtered(c.svc.Disks.List(project, zone), f),
		func(l *compute.DiskList) []*compute.Disk { return l.Items })
}

func (c *Client) DeleteDisk(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return c.svc.Disks.Delete(project, zone, name).Context(ctx).Do()
}

func (c *Client) ResizeDisk(ctx context.Context, project, zone, name string, sizeGb int64) (*compute.Operation, error) {
	return c.svc.Disks.Resize(project, zone, name, &compute.DisksResizeRequest{SizeGb: sizeGb}).Context(ctx).Do()
}

func (c *Client) UpdateDisk(ctx context.Context, project, zone, name string, disk *compute.Disk, paths []string) (*compute.Operation, error) {
	call := c.svc.Disks.Update(project, zone, name, disk)
	if len(paths) > 0 {
		call = call.Paths(paths...)
	}
	return call.Context(ctx).Do()
}

func (c *Client) SetDiskLabels(ctx context.Context, project, zone, name string, req *compute.ZoneSetLabelsRequest) (*compute.Operation, error) {
	return c.svc.Disks.SetLabels(project, zone, name, req).Context(ctx).Do()
}

func (c *Client) CreateSnapshot(ctx context.Context, project, zone, disk string, snap *compute.Snapshot) (*compute.Operation, error) {
	return c.svc.Disks.CreateSnapshot(project, zone, disk, snap).Context(ctx).Do()
}

// ---- Images ----

func (c *Client) InsertImage(ctx context.Context, project string, img *compute.Image) (*compute.Operation, error) {
	return c.svc.Images.Insert(project, img).Context(ctx).Do()
}

func (c *Client) GetImage(ctx context.Context, project, name string) (*compute.Image, error) {
	return c.svc.Images.Get(project, name).Context(ctx).Do()
}

func (c *Client) GetImageFromFamily(ctx context.Context, project, family string) (*compute.Image, error) {
	return c.svc.Images.GetFromFamily(project, family).Context(ctx).Do()
}

func (c *Client) ListImages(ctx context.Context, project string, f gce.ListFilter) ([]*compute.Image, error) {
	return list(ctx, c, filtered(c.svc.Images.List(project), f),
		func(l *compute.ImageList) []*compute.Image { return l.Items })
}

func (c *Client) DeleteImage(ctx context.Context, project, name string) (*compute.Operation, error) {
	return c.svc.Images.Delete(project, name).Context(ctx).Do()
}

func (c *Client) SetImageLabels(ctx context.Context, project, name string, req *compute.GlobalSetLabelsRequest) (*compute.Operation, error) {
	return c.svc.Images.SetLabels(project, name, req).Context(ctx).Do()
}

func (c *Client) DeprecateImage(ctx context.Context, project, name string, status *compute.DeprecationStatus) (*compute.Operation, error) {
	return c.svc.Images.Deprecate(project, name, status).Context(ctx).Do()
}

// ---- Snapshots ----

func (c *Client) GetSnapshot(ctx context.Context, project, name string) (*compute.Snapshot, error) {
	return c.svc.Snapshots.Get(project, name).Context(ctx).Do()
}

func (c *Client) ListSnapshots(ctx context.Context, project string, f gce.ListFilter) ([]*compute.Snapshot, error) {
	return list(ctx, c, filtered(c.svc.Snapshots.List(project), f),
		func(l *compute.SnapshotList) []*compute.Snapshot { return l.Items })
}

func (c *Client) DeleteSnapshot(ctx context.Context, project, name string) (*compute.Operation, error) {
	return c.svc.Snapshots.Delete(project, name).Context(ctx).Do()
}

func (c *Client) SetSnapshotLabels(ctx context.Context, project, name string, req *compute.GlobalSetLabelsRequest) (*compute.Operation, error) {
	return c.svc.Snapshots.SetLabels(project, name, req).Context(ctx).Do()
}

// ---- Instance templates ----

func (c *Client) InsertInstanceTemplate(ctx context.Context, project, region string, t *compute.InstanceTemplate) (*compute.Operation, error) {
	if region == "" {
		return c.svc.InstanceTemplates.Insert(project, t).Context(ctx).Do()
	}
	return c.svc.RegionInstanceTemplates.Insert(project, region, t).Context(ctx).Do()
}

func (c *Client) GetInstanceTemplate(ctx context.Context, project, region, name string) (*compute.InstanceTemplate, error) {
	if region == "" {
		return c.svc.InstanceTemplates.Get(project, name).Context(ctx).Do()
	}
	return c.svc.RegionInstanceTemplates.Get(project, region, name).Context(ctx).Do()
}

func (c *Client) ListInstanceTemplates(ctx context.Context, project, region string, f gce.ListFilter) ([]*compute.InstanceTemplate, error) {
	items := func(l *compute.InstanceTemplateList) []*compute.InstanceTemplate { return l.Items }
	if region == "" {
		return list(ctx, c, filtered(c.svc.InstanceTemplates.List(project), f), items)
	}
	return list(ctx, c, filtered(c.svc.RegionInstanceTemplates.List(project, region), f), items)
}

func (c *Client) DeleteInstanceTemplate(ctx context.Context, project, region, name string) (*compute.Operation, error) {
	if region == "" {
		return c.svc.InstanceTemplates.Delete(project, name).Context(ctx).Do()
	}
	return c.svc.RegionInstanceTemplates.Delete(project, region, name).Context(ctx).Do()
}

// ---- Networks ----

func (c *Client) InsertNetwork(ctx context.Context, project string, n *compute.Network) (*compute.Operation, error) {
	return c.svc.Networks.Insert(project, n).Context(ctx).Do()
}

func (c *Client) GetNetwork(ctx context.Context, project, name string) (*compute.Network, error) {
	return c.svc.Networks.Get(project, name).Context(ctx).Do()
}

func (c *Client) ListNetworks(ctx context.Context, project string, f gce.ListFilter) ([]*compute.Network, error) {
	return list(ctx, c, filtered(c.svc.Networks.List(project), f),
		func(l *compute.NetworkList) []*compute.Network { return l.Items })
}

func (c *Client) DeleteNetwork(ctx context.Context, project, name string) (*compute.Operation, error) {
	return c.svc.Networks.Delete(project, name).Context(ctx).Do()
}

// ---- Subnetworks ----

func (c *Client) InsertSubnetwork(ctx context.Context, project, region string, s *compute.Subnetwork) (*compute.Operation, error) {
	return c.svc.Subnetworks.Insert(project, region, s).Context(ctx).Do()
}

func (c *Client) GetSubnetwork(ctx context.Context, project, region, name string) (*compute.Subnetwork, error) {
	return c.svc.Subnetworks.Get(project, region, name).Context(ctx).Do()
}

func (c *Client) ListSubnetworks(ctx context.Context, project, region string, f gce.ListFilter) ([]*compute.Subnetwork, error) {
	return list(ctx, c, filtered(c.svc.Subnetworks.List(project, region), f),
		func(l *compute.SubnetworkList) []*compute.Subnetwork { return l.Items })
}

func (c *Client) DeleteSubnetwork(ctx context.Context, project, region, name string) (*compute.Operation, error) {
	return c.svc.Subnetworks.Delete(project, region, name).Context(ctx).Do()
}

func (c *Client) PatchSubnetwork(ctx context.Context, project, region, name string, s *compute.Subnetwork) (*compute.Operation, error) {
	return c.svc.Subnetworks.Patch(project, region, name, s).Context(ctx).Do()
}

func (c *Client) ExpandIpCidrRange(ctx context.Context, project, region, name, ipCidrRange string) (*compute.Operation, error) {
	return c.svc.Subnetworks.ExpandIpCidrRange(project, region, name,
		&compute.SubnetworksExpandIpCidrRangeRequest{IpCidrRange: ipCidrRange}).Context(ctx).Do()
}

// ---- Firewalls ----

func (c *Client) InsertFirewall(ctx context.Context, project string, fw *compute.Firewall) (*compute.Operation, error) {
	return c.svc.Firewalls.Insert(project, fw).Context(ctx).Do()
}

func (c *Client) GetFirewall(ctx context.Context, project, name string) (*compute.Firewall, error) {
	return c.svc.Firewalls.Get(project, name).Context(ctx).Do()
}

func (c *Client) ListFirewalls(ctx context.Context, project string, f gce.ListFilter) ([]*compute.Firewall, error) {
	return list(ctx, c, filtered(c.svc.Firewalls.List(project), f),
		func(l *compute.FirewallList) []*compute.Firewall { return l.Items })
}

func (c *Client) DeleteFirewall(ctx context.Context, project, name string) (*compute.Operation, error) {
	return c.svc.Firewalls.Delete(project, name).Context(ctx).Do()
}

func (c *Client) PatchFirewall(ctx context.Context, project, name string, fw *compute.Firewall) (*compute.Operation, error) {
	return c.svc.Firewalls.Patch(project, name, fw).Context(ctx).Do()
}

func (c *Client) UpdateFirewall(ctx context.Context, project, name string, fw *compute.Firewall) (*compute.Operation, error) {
	return c.svc.Firewalls.Update(project, name, fw).Context(ctx).Do()
}

// ---- Network firewall policies ----

func (c *Client) InsertFirewallPolicy(ctx context.Context, project string, p *compute.FirewallPolicy) (*compute.Operation, error) {
	return c.svc.NetworkFirewallPolicies.Insert(project, p).Context(ctx).Do()
}

func (c *Client) GetFirewallPolicy(ctx context.Context, project, name string) (*compute.FirewallPolicy, error) {
	return c.svc.NetworkFirewallPolicies.Get(project, name).Context(ctx).Do()
}

func (c *Client) ListFirewallPolicies(ctx context.Context, project string) ([]*compute.FirewallPolicy, error) {
	return list(ctx, c, c.svc.NetworkFirewallPolicies.List(project),
		func(l *compute.FirewallPolicyList) []*compute.FirewallPolicy { return l.Items })
}

func (c *Client) DeleteFirewallPolicy(ctx context.Context, project, name string) (*compute.Operation, error) {
	return c.svc.NetworkFirewallPolicies.Delete(project, name).Context(ctx).Do()
}

func (c *Client) PatchFirewallPolicy(ctx context.Context, project, name string, p *compute.FirewallPolicy) (*compute.Operation, error) {
	return c.svc.NetworkFirewallPolicies.Patch(project, name, p).Context(ctx).Do()
}

func (c *Client) AddFirewallPolicyRule(ctx context.Context, project, name string, r *compute.FirewallPolicyRule) (*compute.Operation, error) {
	return c.svc.NetworkFirewallPolicies.AddRule(project, name, r).Context(ctx).Do()
}

func (c *Client) GetFirewallPolicyRule(ctx context.Context, project, name string, priority int64) (*compute.FirewallPolicyRule, error) {
	return c.svc.NetworkFirewallPolicies.GetRule(project, name).Priority(priority).Context(ctx).Do()
}

func (c *Client) PatchFirewallPolicyRule(ctx context.Context, project, name string, priority int64, r *compute.FirewallPolicyRule) (*compute.Operation, error) {
	return c.svc.NetworkFirewallPolicies.PatchRule(project, name, r).Priority(priority).Context(ctx).Do()
}

func (c *Client) RemoveFirewallPolicyRule(ctx context.Context, project, name string, priority int64) (*compute.Operation, error) {
	return c.svc.NetworkFirewallPolicies.RemoveRule(project, name).Priority(priority).Context(ctx).Do()
}

func (c *Client) AddFirewallPolicyAssociation(ctx context.Context, project, name string, a *compute.FirewallPolicyAssociation, replaceExisting bool) (*compute.Operation, error) {
	call := c.svc.NetworkFirewallPolicies.AddAssociation(project, name, a)
	if replaceExisting {
		call = call.ReplaceExistingAssociation(true)
	}
	return call.Context(ctx).Do()
}

func (c *Client) RemoveFirewallPolicyAssociation(ctx context.Context, project, name, association string) (*compute.Operation, error) {
	return c.svc.NetworkFirewallPolicies.RemoveAssociation(project, name).Name(association).Context(ctx).Do()
}

// ---- Operations ----

func (c *Client) GetOperation(ctx context.Context, project string, scope gce.Scope, name string) (*compute.Operation, error) {
	switch {
	case scope.Zone != "":
		return c.svc.ZoneOperations.Get(project, scope.Zone, name).Context(ctx).Do()
	case scope.Region != "":
		return c.svc.RegionOperations.Get(project, scope.Region, name).Context(ctx).Do()
	}
	return c.svc.GlobalOperations.Get(project, name).Context(ctx).Do()
}

func (c *Client) WaitOperation(ctx context.Context, project string, scope gce.Scope, name string) (*compute.Operation, error) {
	switch {
	case scope.Zone != "":
		return c.svc.ZoneOperations.Wait(project, scope.Zone, name).Context(ctx).Do()
	case scope.Region != "":
		return c.svc.RegionOperations.Wait(project, scope.Region, name).Context(ctx).Do()
	}
	return c.svc.GlobalOperations.Wait(project, name).Context(ctx).Do()
}

func (c *Client) ListOperations(ctx context.Context, project string, scope gce.Scope) ([]*compute.Operation, error) {
	items := func(l *compute.OperationList) []*compute.Operation { return l.Items }
	switch {
	case scope.Zone != "":
		return list(ctx, c, c.svc.ZoneOperations.List(project, scope.Zone), items)
	case scope.Region != "":
		return list(ctx, c, c.svc.RegionOperations.List(project, scope.Region), items)
	}
	return list(ctx, c, c.svc.GlobalOperations.List(project), items)
}

// ---- Catalog ----

func (c *Client) GetRegion(ctx context.Context, project, region string) (*compute.Region, error) {
	return c.svc.Regions.Get(project, region).Context(ctx).Do()
}

func (c *Client) ListRegions(ctx context.Context, project string) ([]*compute.Region, error) {
	return list(ctx, c, c.svc.Regions.List(project),
		func(l *compute.RegionList) []*compute.Region { return l.Items })
}

func (c *Client) GetZone(ctx context.Context, project, zone string) (*compute.Zone, error) {
	return c.svc.Zones.Get(project, zone).Context(ctx).Do()
}

func (c *Client) ListZones(ctx context.Context, project string) ([]*compute.Zone, error) {
	return list(ctx, c, c.svc.Zones.List(project),
		func(l *compute.ZoneList) []*compute.Zone { return l.Items })
}

func (c *Client) GetMachineType(ctx context.Context, project, zone, name string) (*compute.MachineType, error) {
	return c.svc.MachineTypes.Get(project, zone, name).Context(ctx).Do()
}

func (c *Client) ListMachineTypes(ctx context.Context, project, zone string) ([]*compute.MachineType, error) {
	return list(ctx, c, c.svc.MachineTypes.List(project, zone),
		func(l *compute.MachineTypeList) []*compute.MachineType { return l.Items })
}

func (c *Client) GetDiskType(ctx context.Context, project, zone, name string) (*compute.DiskType, error) {
	return c.svc.DiskTypes.Get(project, zone, name).Context(ctx).Do()
}

func (c *Client) ListDiskTypes(ctx context.Context, project, zone string) ([]*compute.DiskType, error) {
	return list(ctx, c, c.svc.DiskTypes.List(project, zone),
		func(l *compute.DiskTypeList) []*compute.DiskType { return l.Items })
}

func (c *Client) GetAcceleratorType(ctx context.Context, project, zone, name string) (*compute.AcceleratorType, error) {
	return c.svc.AcceleratorTypes.Get(project, zone, name).Context(ctx).Do()
}

func (c *Client) ListAcceleratorTypes(ctx context.Context, project, zone string) ([]*compute.AcceleratorType, error) {
	return list(ctx, c, c.svc.AcceleratorTypes.List(project, zone),
		func(l *compute.AcceleratorTypeList) []*compute.AcceleratorType { return l.Items })
}

func (c *Client) GetProject(ctx context.Context, project string) (*compute.Project, error) {
	return c.svc.Projects.Get(project).Context(ctx).Do()
}
