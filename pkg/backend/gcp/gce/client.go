// Package gce is silod's client of GCE: the one interface through which
// it makes every compute/v1 call, and the one Cloud Quotas read it makes
// (docs/silo/gcp-backend.md, docs/silo/allocators.md "Root capacity").
//
// Requests and responses are compute/v1's own JSON types
// (google.golang.org/api/compute/v1), so that the facade can pass
// GCE-shaped bodies through its allowlists without translating them.
// Errors are *googleapi.Error, as the generated client returns them;
// Reason and the Is* helpers read them.
//
// Two implementations exist: the real one over the generated client,
// in package gce/gceclient, and the in-memory fake in package gce/fake, which
// CI runs silod against (docs/silo/testing.md "Fake GCE backend",
// SV-119). Package gce/gcetest holds behaviour tests both pass. The
// interface is kept to what silod calls, so that the fake can implement
// all of it honestly.
//
// # What is deliberately left out
//
//   - aggregatedList. The facade serves a tenant's aggregatedList by
//     listing each zone or region it allows, so the client lists one
//     scope at a time.
//   - Paging. Each List returns every match; the real client follows
//     nextPageToken itself.
//   - Arbitrary filters. silod only ever asks GCE for a silo's own
//     objects, by label or by name prefix (gcp-backend.md "Lists and
//     filters", call 26); the tenant's own filter is applied by the
//     facade, never passed through. ListFilter is that and nothing more.
//
// # Scopes
//
// Methods that take a region for an object GCE keeps both globally and
// per region (instance templates) treat an empty region as global.
package gce

import (
	"context"

	compute "google.golang.org/api/compute/v1"
)

// Client is every GCE call silod makes. Mutations return the operation
// GCE started; the caller watches it with WaitOperation or GetOperation.
// Validation errors that GCE reports before starting an operation are
// returned as errors; failures GCE reports in the operation (quota,
// stockout, internal) are in the operation's Error.
type Client interface {
	Instances
	Disks
	Images
	Snapshots
	InstanceTemplates
	Networks
	Subnetworks
	Firewalls
	FirewallPolicies
	Operations
	Catalog
	Quotas
}

// Instances is compute/v1 instances.
type Instances interface {
	// InsertInstance creates inst in zone. A non-empty
	// sourceInstanceTemplate (a URL or partial path to a global or
	// regional template) makes GCE merge inst over the template's
	// properties.
	InsertInstance(ctx context.Context, project, zone string, inst *compute.Instance, sourceInstanceTemplate string) (*compute.Operation, error)
	GetInstance(ctx context.Context, project, zone, name string) (*compute.Instance, error)
	ListInstances(ctx context.Context, project, zone string, f ListFilter) ([]*compute.Instance, error)
	DeleteInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error)
	StartInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error)
	StopInstance(ctx context.Context, project, zone, name string, discardLocalSsd bool) (*compute.Operation, error)
	ResetInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error)
	SuspendInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error)
	ResumeInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error)
	AttachDisk(ctx context.Context, project, zone, name string, disk *compute.AttachedDisk) (*compute.Operation, error)
	DetachDisk(ctx context.Context, project, zone, name, deviceName string) (*compute.Operation, error)
	SetInstanceLabels(ctx context.Context, project, zone, name string, req *compute.InstancesSetLabelsRequest) (*compute.Operation, error)
	SetInstanceMetadata(ctx context.Context, project, zone, name string, md *compute.Metadata) (*compute.Operation, error)
	SetInstanceTags(ctx context.Context, project, zone, name string, tags *compute.Tags) (*compute.Operation, error)
	// SetMachineType takes a machine type URL or partial path, as GCE's
	// InstancesSetMachineTypeRequest does.
	SetMachineType(ctx context.Context, project, zone, name, machineType string) (*compute.Operation, error)
	SetDeletionProtection(ctx context.Context, project, zone, name string, protect bool) (*compute.Operation, error)
	GetSerialPortOutput(ctx context.Context, project, zone, name string, port, start int64) (*compute.SerialPortOutput, error)
}

// Disks is compute/v1 disks.
type Disks interface {
	InsertDisk(ctx context.Context, project, zone string, disk *compute.Disk) (*compute.Operation, error)
	GetDisk(ctx context.Context, project, zone, name string) (*compute.Disk, error)
	ListDisks(ctx context.Context, project, zone string, f ListFilter) ([]*compute.Disk, error)
	DeleteDisk(ctx context.Context, project, zone, name string) (*compute.Operation, error)
	ResizeDisk(ctx context.Context, project, zone, name string, sizeGb int64) (*compute.Operation, error)
	// UpdateDisk is disks.update: paths names the fields of disk to
	// change (provisionedIops, provisionedThroughput, accessMode).
	UpdateDisk(ctx context.Context, project, zone, name string, disk *compute.Disk, paths []string) (*compute.Operation, error)
	SetDiskLabels(ctx context.Context, project, zone, name string, req *compute.ZoneSetLabelsRequest) (*compute.Operation, error)
	CreateSnapshot(ctx context.Context, project, zone, disk string, snap *compute.Snapshot) (*compute.Operation, error)
}

// Images is compute/v1 images. Reads work in public image projects too.
type Images interface {
	InsertImage(ctx context.Context, project string, img *compute.Image) (*compute.Operation, error)
	GetImage(ctx context.Context, project, name string) (*compute.Image, error)
	GetImageFromFamily(ctx context.Context, project, family string) (*compute.Image, error)
	ListImages(ctx context.Context, project string, f ListFilter) ([]*compute.Image, error)
	DeleteImage(ctx context.Context, project, name string) (*compute.Operation, error)
	SetImageLabels(ctx context.Context, project, name string, req *compute.GlobalSetLabelsRequest) (*compute.Operation, error)
	DeprecateImage(ctx context.Context, project, name string, status *compute.DeprecationStatus) (*compute.Operation, error)
}

// Snapshots is compute/v1 snapshots. They are made with CreateSnapshot.
type Snapshots interface {
	GetSnapshot(ctx context.Context, project, name string) (*compute.Snapshot, error)
	ListSnapshots(ctx context.Context, project string, f ListFilter) ([]*compute.Snapshot, error)
	DeleteSnapshot(ctx context.Context, project, name string) (*compute.Operation, error)
	SetSnapshotLabels(ctx context.Context, project, name string, req *compute.GlobalSetLabelsRequest) (*compute.Operation, error)
}

// InstanceTemplates is compute/v1 instanceTemplates (region "") and
// regionInstanceTemplates.
type InstanceTemplates interface {
	InsertInstanceTemplate(ctx context.Context, project, region string, t *compute.InstanceTemplate) (*compute.Operation, error)
	GetInstanceTemplate(ctx context.Context, project, region, name string) (*compute.InstanceTemplate, error)
	ListInstanceTemplates(ctx context.Context, project, region string, f ListFilter) ([]*compute.InstanceTemplate, error)
	DeleteInstanceTemplate(ctx context.Context, project, region, name string) (*compute.Operation, error)
}

// Networks is compute/v1 networks. Install makes the shared VPC; the
// facade only reads.
type Networks interface {
	InsertNetwork(ctx context.Context, project string, n *compute.Network) (*compute.Operation, error)
	GetNetwork(ctx context.Context, project, name string) (*compute.Network, error)
	ListNetworks(ctx context.Context, project string, f ListFilter) ([]*compute.Network, error)
	DeleteNetwork(ctx context.Context, project, name string) (*compute.Operation, error)
}

// Subnetworks is compute/v1 subnetworks.
type Subnetworks interface {
	InsertSubnetwork(ctx context.Context, project, region string, s *compute.Subnetwork) (*compute.Operation, error)
	GetSubnetwork(ctx context.Context, project, region, name string) (*compute.Subnetwork, error)
	ListSubnetworks(ctx context.Context, project, region string, f ListFilter) ([]*compute.Subnetwork, error)
	DeleteSubnetwork(ctx context.Context, project, region, name string) (*compute.Operation, error)
	// PatchSubnetwork needs s.Fingerprint, as GCE does.
	PatchSubnetwork(ctx context.Context, project, region, name string, s *compute.Subnetwork) (*compute.Operation, error)
	ExpandIpCidrRange(ctx context.Context, project, region, name, ipCidrRange string) (*compute.Operation, error)
}

// Firewalls is compute/v1 firewalls: VPC firewall rules.
type Firewalls interface {
	InsertFirewall(ctx context.Context, project string, fw *compute.Firewall) (*compute.Operation, error)
	GetFirewall(ctx context.Context, project, name string) (*compute.Firewall, error)
	ListFirewalls(ctx context.Context, project string, f ListFilter) ([]*compute.Firewall, error)
	DeleteFirewall(ctx context.Context, project, name string) (*compute.Operation, error)
	PatchFirewall(ctx context.Context, project, name string, fw *compute.Firewall) (*compute.Operation, error)
	UpdateFirewall(ctx context.Context, project, name string, fw *compute.Firewall) (*compute.Operation, error)
}

// FirewallPolicies is compute/v1 networkFirewallPolicies: global network
// firewall policies, which the fence is.
type FirewallPolicies interface {
	InsertFirewallPolicy(ctx context.Context, project string, p *compute.FirewallPolicy) (*compute.Operation, error)
	GetFirewallPolicy(ctx context.Context, project, name string) (*compute.FirewallPolicy, error)
	ListFirewallPolicies(ctx context.Context, project string) ([]*compute.FirewallPolicy, error)
	DeleteFirewallPolicy(ctx context.Context, project, name string) (*compute.Operation, error)
	// PatchFirewallPolicy needs p.Fingerprint to match the policy's
	// current one; a stale fingerprint fails with 412.
	PatchFirewallPolicy(ctx context.Context, project, name string, p *compute.FirewallPolicy) (*compute.Operation, error)
	AddFirewallPolicyRule(ctx context.Context, project, name string, r *compute.FirewallPolicyRule) (*compute.Operation, error)
	GetFirewallPolicyRule(ctx context.Context, project, name string, priority int64) (*compute.FirewallPolicyRule, error)
	PatchFirewallPolicyRule(ctx context.Context, project, name string, priority int64, r *compute.FirewallPolicyRule) (*compute.Operation, error)
	RemoveFirewallPolicyRule(ctx context.Context, project, name string, priority int64) (*compute.Operation, error)
	AddFirewallPolicyAssociation(ctx context.Context, project, name string, a *compute.FirewallPolicyAssociation, replaceExisting bool) (*compute.Operation, error)
	RemoveFirewallPolicyAssociation(ctx context.Context, project, name, association string) (*compute.Operation, error)
}

// Operations is compute/v1 zoneOperations, regionOperations and
// globalOperations, chosen by Scope.
type Operations interface {
	GetOperation(ctx context.Context, project string, scope Scope, name string) (*compute.Operation, error)
	// WaitOperation returns when the operation is DONE, or after GCE's
	// own cap on waiting (about two minutes), whichever is first. A
	// result that isn't DONE is not an error.
	WaitOperation(ctx context.Context, project string, scope Scope, name string) (*compute.Operation, error)
	ListOperations(ctx context.Context, project string, scope Scope) ([]*compute.Operation, error)
}

// Catalog is GCE's read-only description of what a project may use, and
// projects.get for its project-wide quotas.
type Catalog interface {
	GetRegion(ctx context.Context, project, region string) (*compute.Region, error)
	ListRegions(ctx context.Context, project string) ([]*compute.Region, error)
	GetZone(ctx context.Context, project, zone string) (*compute.Zone, error)
	ListZones(ctx context.Context, project string) ([]*compute.Zone, error)
	GetMachineType(ctx context.Context, project, zone, name string) (*compute.MachineType, error)
	ListMachineTypes(ctx context.Context, project, zone string) ([]*compute.MachineType, error)
	GetDiskType(ctx context.Context, project, zone, name string) (*compute.DiskType, error)
	ListDiskTypes(ctx context.Context, project, zone string) ([]*compute.DiskType, error)
	GetAcceleratorType(ctx context.Context, project, zone, name string) (*compute.AcceleratorType, error)
	ListAcceleratorTypes(ctx context.Context, project, zone string) ([]*compute.AcceleratorType, error)
	GetProject(ctx context.Context, project string) (*compute.Project, error)
}

// Quotas is the Cloud Quotas API (cloudquotas.googleapis.com), read for
// the Compute limits regions.get doesn't report.
type Quotas interface {
	// ListComputeQuotaInfos lists project's quotaInfos for the service
	// compute.googleapis.com.
	ListComputeQuotaInfos(ctx context.Context, project string) ([]*QuotaInfo, error)
}

// Scope is where an operation lives: a zone, a region, or neither
// (global).
type Scope struct {
	Zone   string
	Region string
}

// Global is the scope of global operations.
var Global = Scope{}

// Zonal is the scope of zone's operations.
func Zonal(zone string) Scope { return Scope{Zone: zone} }

// Regional is the scope of region's operations.
func Regional(region string) Scope { return Scope{Region: region} }

// ScopeOf is the scope of op, from its zone or region URL.
func ScopeOf(op *compute.Operation) Scope {
	switch {
	case op.Zone != "":
		return Zonal(LastSegment(op.Zone))
	case op.Region != "":
		return Regional(LastSegment(op.Region))
	}
	return Global
}

// String is the scope as a partial path: "zones/z", "regions/r" or
// "global".
func (s Scope) String() string {
	switch {
	case s.Zone != "":
		return "zones/" + s.Zone
	case s.Region != "":
		return "regions/" + s.Region
	}
	return "global"
}

// QuotaInfo is the part of Cloud Quotas' QuotaInfo silod reads: a
// quota's limit per region. The generated client for Cloud Quotas isn't
// in the module's google.golang.org/api, so this is a thin struct in its
// JSON shape.
type QuotaInfo struct {
	// Name is the resource name,
	// projects/<p>/locations/global/services/compute.googleapis.com/quotaInfos/<quotaId>.
	Name    string `json:"name"`
	QuotaID string `json:"quotaId"`
	// Metric is the service's metric, such as
	// compute.googleapis.com/nvidia_h100_gpus.
	Metric          string                 `json:"metric"`
	Service         string                 `json:"service"`
	IsPrecise       bool                   `json:"isPrecise,omitempty"`
	DimensionsInfos []*QuotaDimensionsInfo `json:"dimensionsInfos,omitempty"`
}

// QuotaDimensionsInfo is one limit of a QuotaInfo: for silod, the limit
// in one region.
type QuotaDimensionsInfo struct {
	Dimensions          map[string]string `json:"dimensions,omitempty"`
	Details             QuotaDetails      `json:"details"`
	ApplicableLocations []string          `json:"applicableLocations,omitempty"`
}

// QuotaDetails is a limit. Cloud Quotas encodes it as an int64 string.
type QuotaDetails struct {
	Value int64 `json:"value,string"`
}
