package fake

import (
	"context"
	"slices"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

// Catalog is what the fake GCE offers: regions and zones, machine types,
// and accelerator types per zone. Disk types are fixed (diskKinds).
type Catalog struct {
	Regions          []RegionSpec
	MachineTypes     []MachineTypeSpec
	AcceleratorTypes []AcceleratorTypeSpec
}

// RegionSpec is a region and its zones.
type RegionSpec struct {
	Name  string
	Zones []ZoneSpec
}

// ZoneSpec is a zone and the accelerator types it offers.
type ZoneSpec struct {
	Name         string
	Accelerators []string
}

// MachineTypeSpec is a machine type. One that bundles GPUs is offered
// only in zones that offer its GPU type; every other one in every zone.
type MachineTypeSpec struct {
	Name     string
	CPUs     int64
	MemoryMB int64
	// GPUType and GPUs are the accelerators the type bundles (A2, A3,
	// G2), listed in its accelerators[].
	GPUType string
	GPUs    int64
	// LocalSSDPartitions is the local SSD it bundles (-lssd, Z3, A3),
	// 375 GB each.
	LocalSSDPartitions int64
}

// AcceleratorTypeSpec is a GPU type.
type AcceleratorTypeSpec struct {
	Name string
	// Metric is its regional quota metric; PREEMPTIBLE_ + Metric is the
	// Spot one.
	Metric         string
	MaxPerInstance int64
	// AttachSeries are the machine series it may be attached to with
	// guestAccelerators, and AttachCounts the counts GCE allows. Empty
	// for a type that only comes bundled in a machine type.
	AttachSeries []string
	AttachCounts []int64
	// CloudQuotasOnly is set for a family regions.get doesn't report.
	CloudQuotasOnly bool
}

// LocalSSDPartitionGB is the size of one local SSD partition.
const LocalSSDPartitionGB = 375

// DefaultCatalog is a small slice of GCE: three regions, the machine
// series silod's rules distinguish, and the common GPUs.
func DefaultCatalog() *Catalog {
	t4 := "nvidia-tesla-t4"
	v100 := "nvidia-tesla-v100"
	l4 := "nvidia-l4"
	a100 := "nvidia-tesla-a100"
	a100x := "nvidia-a100-80gb"
	h100 := "nvidia-h100-80gb"
	return &Catalog{
		Regions: []RegionSpec{
			{Name: "us-central1", Zones: []ZoneSpec{
				{Name: "us-central1-a", Accelerators: []string{t4, v100, l4, a100, a100x, h100}},
				{Name: "us-central1-b", Accelerators: []string{t4, v100, l4, a100}},
				{Name: "us-central1-c", Accelerators: []string{t4, l4, a100x, h100}},
				{Name: "us-central1-f", Accelerators: []string{t4, a100}},
			}},
			{Name: "us-east1", Zones: []ZoneSpec{
				{Name: "us-east1-b", Accelerators: []string{l4}},
				{Name: "us-east1-c", Accelerators: []string{t4}},
				{Name: "us-east1-d", Accelerators: []string{t4, l4, h100}},
			}},
			{Name: "europe-west4", Zones: []ZoneSpec{
				{Name: "europe-west4-a", Accelerators: []string{t4, v100, l4, a100, a100x}},
				{Name: "europe-west4-b", Accelerators: []string{t4, v100, l4, a100, h100}},
				{Name: "europe-west4-c", Accelerators: []string{t4, l4, h100}},
			}},
		},
		MachineTypes: []MachineTypeSpec{
			{Name: "e2-medium", CPUs: 2, MemoryMB: 4096},
			{Name: "e2-standard-2", CPUs: 2, MemoryMB: 8192},
			{Name: "e2-standard-4", CPUs: 4, MemoryMB: 16384},
			{Name: "e2-standard-8", CPUs: 8, MemoryMB: 32768},
			{Name: "n1-standard-1", CPUs: 1, MemoryMB: 3840},
			{Name: "n1-standard-4", CPUs: 4, MemoryMB: 15360},
			{Name: "n1-standard-8", CPUs: 8, MemoryMB: 30720},
			{Name: "n1-standard-16", CPUs: 16, MemoryMB: 61440},
			{Name: "n2-standard-2", CPUs: 2, MemoryMB: 8192},
			{Name: "n2-standard-8", CPUs: 8, MemoryMB: 32768},
			{Name: "n2-standard-64", CPUs: 64, MemoryMB: 262144},
			{Name: "n2-standard-80", CPUs: 80, MemoryMB: 327680},
			{Name: "n2d-standard-2", CPUs: 2, MemoryMB: 8192},
			{Name: "c3-standard-4", CPUs: 4, MemoryMB: 16384},
			{Name: "c3-standard-8", CPUs: 8, MemoryMB: 32768},
			{Name: "c3-standard-4-lssd", CPUs: 4, MemoryMB: 16384, LocalSSDPartitions: 1},
			{Name: "c3-standard-8-lssd", CPUs: 8, MemoryMB: 32768, LocalSSDPartitions: 2},
			{Name: "c4-standard-4", CPUs: 4, MemoryMB: 15360},
			{Name: "n4-standard-4", CPUs: 4, MemoryMB: 16384},
			{Name: "z3-highmem-88", CPUs: 88, MemoryMB: 720896, LocalSSDPartitions: 12},
			{Name: "g2-standard-4", CPUs: 4, MemoryMB: 16384, GPUType: l4, GPUs: 1},
			{Name: "g2-standard-24", CPUs: 24, MemoryMB: 98304, GPUType: l4, GPUs: 2},
			{Name: "a2-highgpu-1g", CPUs: 12, MemoryMB: 87040, GPUType: a100, GPUs: 1},
			{Name: "a2-highgpu-2g", CPUs: 24, MemoryMB: 174080, GPUType: a100, GPUs: 2},
			{Name: "a2-ultragpu-1g", CPUs: 12, MemoryMB: 174080, GPUType: a100x, GPUs: 1, LocalSSDPartitions: 1},
			{Name: "a3-highgpu-8g", CPUs: 208, MemoryMB: 1872896, GPUType: h100, GPUs: 8, LocalSSDPartitions: 16},
		},
		AcceleratorTypes: []AcceleratorTypeSpec{
			{Name: t4, Metric: "NVIDIA_T4_GPUS", MaxPerInstance: 4, AttachSeries: []string{"n1"}, AttachCounts: []int64{1, 2, 4}},
			{Name: v100, Metric: "NVIDIA_V100_GPUS", MaxPerInstance: 8, AttachSeries: []string{"n1"}, AttachCounts: []int64{1, 2, 4, 8}},
			{Name: l4, Metric: "NVIDIA_L4_GPUS", MaxPerInstance: 8},
			{Name: a100, Metric: "NVIDIA_A100_GPUS", MaxPerInstance: 16},
			{Name: a100x, Metric: "NVIDIA_A100_80GB_GPUS", MaxPerInstance: 8},
			{Name: h100, Metric: "NVIDIA_H100_GPUS", MaxPerInstance: 8, CloudQuotasOnly: true},
		},
	}
}

func (c *Catalog) region(name string) *RegionSpec {
	for i := range c.Regions {
		if c.Regions[i].Name == name {
			return &c.Regions[i]
		}
	}
	return nil
}

func (c *Catalog) zone(name string) *ZoneSpec {
	for i := range c.Regions {
		for j := range c.Regions[i].Zones {
			if c.Regions[i].Zones[j].Name == name {
				return &c.Regions[i].Zones[j]
			}
		}
	}
	return nil
}

func (c *Catalog) accelerator(name string) *AcceleratorTypeSpec {
	for i := range c.AcceleratorTypes {
		if c.AcceleratorTypes[i].Name == name {
			return &c.AcceleratorTypes[i]
		}
	}
	return nil
}

func (c *Catalog) acceleratorInZone(zone, name string) *AcceleratorTypeSpec {
	z := c.zone(zone)
	if z == nil || !slices.Contains(z.Accelerators, name) {
		return nil
	}
	return c.accelerator(name)
}

// machineType finds name, offered in zone.
func (c *Catalog) machineType(zone, name string) *MachineTypeSpec {
	for i := range c.MachineTypes {
		mt := &c.MachineTypes[i]
		if mt.Name != name {
			continue
		}
		if mt.GPUType != "" && c.acceleratorInZone(zone, mt.GPUType) == nil {
			return nil
		}
		return mt
	}
	return nil
}

// machineTypeAnywhere finds name in any zone, for templates.
func (c *Catalog) machineTypeAnywhere(name string) *MachineTypeSpec {
	for i := range c.MachineTypes {
		if c.MachineTypes[i].Name == name {
			return &c.MachineTypes[i]
		}
	}
	return nil
}

// series is a machine type's series: n1 for n1-standard-4.
func series(machineType string) string {
	s, _, _ := strings.Cut(machineType, "-")
	return s
}

func (f *Fake) regionObj(p *state, r *RegionSpec) *compute.Region {
	out := &compute.Region{
		Kind:     "compute#region",
		Name:     r.Name,
		Status:   "UP",
		SelfLink: gce.SelfLinkBase + "projects/" + p.name + "/regions/" + r.Name,
		Quotas:   f.regionQuotas(p, r.Name),
	}
	for _, z := range r.Zones {
		out.Zones = append(out.Zones, gce.SelfLinkBase+"projects/"+p.name+"/zones/"+z.Name)
	}
	return out
}

// GetRegion implements gce.Client. Its quotas are the regional limits
// regions.get reports, with their usage.
func (f *Fake) GetRegion(ctx context.Context, project, region string) (*compute.Region, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	r := f.catalog.region(region)
	if r == nil {
		return nil, f.regionExists(region)
	}
	return f.regionObj(p, r), nil
}

// ListRegions implements gce.Client.
func (f *Fake) ListRegions(ctx context.Context, project string) ([]*compute.Region, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	var out []*compute.Region
	for i := range f.catalog.Regions {
		out = append(out, f.regionObj(p, &f.catalog.Regions[i]))
	}
	return out, nil
}

func zoneObj(p *state, z *ZoneSpec) *compute.Zone {
	return &compute.Zone{
		Kind:     "compute#zone",
		Name:     z.Name,
		Status:   "UP",
		Region:   gce.SelfLinkBase + "projects/" + p.name + "/regions/" + gce.ZoneRegion(z.Name),
		SelfLink: gce.SelfLinkBase + "projects/" + p.name + "/zones/" + z.Name,
	}
}

// GetZone implements gce.Client.
func (f *Fake) GetZone(ctx context.Context, project, zone string) (*compute.Zone, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	z := f.catalog.zone(zone)
	if z == nil {
		return nil, f.zoneExists(zone)
	}
	return zoneObj(p, z), nil
}

// ListZones implements gce.Client.
func (f *Fake) ListZones(ctx context.Context, project string) ([]*compute.Zone, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	var out []*compute.Zone
	for _, r := range f.catalog.Regions {
		for i := range r.Zones {
			out = append(out, zoneObj(p, &r.Zones[i]))
		}
	}
	return out, nil
}

func machineTypeObj(p *state, zone string, mt *MachineTypeSpec) *compute.MachineType {
	out := &compute.MachineType{
		Kind:                         "compute#machineType",
		Name:                         mt.Name,
		GuestCpus:                    mt.CPUs,
		MemoryMb:                     mt.MemoryMB,
		Zone:                         zone,
		SelfLink:                     p.link("zones/"+zone, "machineTypes", mt.Name),
		IsSharedCpu:                  mt.Name == "e2-medium",
		MaximumPersistentDisks:       128,
		MaximumPersistentDisksSizeGb: 263168,
	}
	if mt.GPUs > 0 {
		out.Accelerators = []*compute.MachineTypeAccelerators{{GuestAcceleratorType: mt.GPUType, GuestAcceleratorCount: mt.GPUs}}
	}
	if mt.LocalSSDPartitions > 0 {
		out.BundledLocalSsds = &compute.BundledLocalSsds{DefaultInterface: "NVME", PartitionCount: mt.LocalSSDPartitions}
	}
	return out
}

// GetMachineType implements gce.Client. A machine type bundling GPUs
// exists only in zones offering them.
func (f *Fake) GetMachineType(ctx context.Context, project, zone, name string) (*compute.MachineType, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	mt := f.catalog.machineType(zone, name)
	if mt == nil {
		return nil, notFound(p.link("zones/"+zone, "machineTypes", name))
	}
	return machineTypeObj(p, zone, mt), nil
}

// ListMachineTypes implements gce.Client.
func (f *Fake) ListMachineTypes(ctx context.Context, project, zone string) ([]*compute.MachineType, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	var out []*compute.MachineType
	for _, mt := range f.catalog.MachineTypes {
		if f.catalog.machineType(zone, mt.Name) != nil {
			out = append(out, machineTypeObj(p, zone, &mt))
		}
	}
	return out, nil
}

func acceleratorObj(p *state, zone string, a *AcceleratorTypeSpec) *compute.AcceleratorType {
	return &compute.AcceleratorType{
		Kind:                    "compute#acceleratorType",
		Name:                    a.Name,
		Zone:                    zone,
		MaximumCardsPerInstance: a.MaxPerInstance,
		SelfLink:                p.link("zones/"+zone, "acceleratorTypes", a.Name),
	}
}

// GetAcceleratorType implements gce.Client.
func (f *Fake) GetAcceleratorType(ctx context.Context, project, zone, name string) (*compute.AcceleratorType, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	a := f.catalog.acceleratorInZone(zone, name)
	if a == nil {
		return nil, notFound(p.link("zones/"+zone, "acceleratorTypes", name))
	}
	return acceleratorObj(p, zone, a), nil
}

// ListAcceleratorTypes implements gce.Client.
func (f *Fake) ListAcceleratorTypes(ctx context.Context, project, zone string) ([]*compute.AcceleratorType, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	var out []*compute.AcceleratorType
	for _, name := range f.catalog.zone(zone).Accelerators {
		if a := f.catalog.accelerator(name); a != nil {
			out = append(out, acceleratorObj(p, zone, a))
		}
	}
	return out, nil
}

func diskTypeObj(p *state, zone string, k *diskKind) *compute.DiskType {
	return &compute.DiskType{
		Kind:              "compute#diskType",
		Name:              k.name,
		Zone:              zone,
		DefaultDiskSizeGb: k.defaultGB,
		ValidDiskSize:     k.validSize(),
		SelfLink:          p.link("zones/"+zone, "diskTypes", k.name),
	}
}

// GetDiskType implements gce.Client.
func (f *Fake) GetDiskType(ctx context.Context, project, zone, name string) (*compute.DiskType, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	k := diskKinds[name]
	if k == nil {
		return nil, notFound(p.link("zones/"+zone, "diskTypes", name))
	}
	return diskTypeObj(p, zone, k), nil
}

// ListDiskTypes implements gce.Client.
func (f *Fake) ListDiskTypes(ctx context.Context, project, zone string) ([]*compute.DiskType, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	var out []*compute.DiskType
	for _, name := range diskKindNames() {
		out = append(out, diskTypeObj(p, zone, diskKinds[name]))
	}
	return out, nil
}

// PublicImages are the images the fake starts with, in public image
// projects.
func PublicImages() []*compute.Image {
	return []*compute.Image{
		{Name: "debian-12-bookworm-v20260801", Family: "debian-12", DiskSizeGb: 10, ArchiveSizeBytes: 1 << 30,
			SelfLink: gce.SelfLinkBase + "projects/debian-cloud/global/images/debian-12-bookworm-v20260801"},
		{Name: "debian-12-bookworm-v20260901", Family: "debian-12", DiskSizeGb: 10, ArchiveSizeBytes: 1 << 30,
			SelfLink: gce.SelfLinkBase + "projects/debian-cloud/global/images/debian-12-bookworm-v20260901"},
		{Name: "ubuntu-2404-noble-amd64-v20260901", Family: "ubuntu-2404-lts-amd64", DiskSizeGb: 10, ArchiveSizeBytes: 1 << 30,
			SelfLink: gce.SelfLinkBase + "projects/ubuntu-os-cloud/global/images/ubuntu-2404-noble-amd64-v20260901"},
	}
}
