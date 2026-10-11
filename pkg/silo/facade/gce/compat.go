package gce

import (
	"slices"
	"strings"

	gceapi "github.com/bwsalmon/silo/pkg/silo/gce"
	compute "google.golang.org/api/compute/v1"
)

// GCE's documented machine-type compatibility, as data (gcp-backend.md,
// "GPUs", "Local SSD" and "Disks", SV-67 to SV-69): which machine series
// take which GPUs and in what counts, which take local SSD partitions,
// which each disk type attaches to, and the disk types' provisioned
// performance, GCE's defaults and minimums. The facade checks a request
// against it before charging, so that a mismatch comes back as GCE's 400
// rather than as a failed operation after the charge. A combination the
// table doesn't know is passed to GCE, whose refusal is passed through
// and the charge refunded. silod's config may replace it
// (Options.Compat).

// Compat is the compatibility table.
type Compat struct {
	// Accelerators are the GPU types that may be attached with
	// guestAccelerators, by accelerator type. A type listed with no
	// series comes only bundled in a machine type (A2, A3, G2). A type
	// not listed is passed to GCE to judge.
	Accelerators map[string]AcceleratorCompat
	// LocalSSDSeries are the machine series that take attached local
	// SSD partitions. A machine type that bundles its own takes none.
	LocalSSDSeries []string
	// LocalSSDPartitionGB is the size of one local SSD partition.
	LocalSSDPartitionGB int64
	// HyperdiskOnly are the machine series that take no pd-* disks.
	HyperdiskOnly []string
	// DiskTypes are the disk types the facade charges for, by type.
	DiskTypes map[string]DiskTypeCompat
}

// AcceleratorCompat is where a GPU type may be attached.
type AcceleratorCompat struct {
	Series []string
	Counts []int64
}

// DiskTypeCompat is a disk type's machine-type compatibility and its
// provisioned performance.
type DiskTypeCompat struct {
	// Attaches are the machine series the type attaches to. None means
	// every series but Compat.HyperdiskOnly.
	Attaches []SeriesRule
	// IOPS and Throughput are set for a type whose provisioned IOPS or
	// throughput may be set (and is charged).
	IOPS, Throughput *Performance
	// AccessMode is set for a type whose accessMode may be set.
	AccessMode bool
}

// SeriesRule is a machine series, from MinCPUs vCPUs up.
type SeriesRule struct {
	Series  string
	MinCPUs int64
}

// Performance is a provisioned quantity's range and GCE's default, each
// a function of the disk's size.
type Performance struct {
	Min, Max, Default Linear
}

// Linear is clamp(Base + PerGB*gb/Per, Lo, Hi): Per 0 is 1, and Hi 0 is
// no cap.
type Linear struct {
	Base, PerGB, Per, Lo, Hi int64
}

func (l Linear) at(gb int64) int64 {
	per := l.Per
	if per == 0 {
		per = 1
	}
	v := max(l.Base+l.PerGB*gb/per, l.Lo)
	if l.Hi > 0 {
		v = min(v, l.Hi)
	}
	return v
}

// Minimum is the least a gb disk may be provisioned.
func (p *Performance) Minimum(gb int64) int64 { return p.Min.at(gb) }

// DefaultAt is what GCE provisions a gb disk with when it is given
// nothing: its default, within the size's range.
func (p *Performance) DefaultAt(gb int64) int64 {
	lo, hi := p.Min.at(gb), p.Max.at(gb)
	return max(lo, min(p.Default.at(gb), max(lo, hi)))
}

// DefaultCompat is GCE's table as documented at the time of writing.
func DefaultCompat() *Compat {
	series := func(names ...string) []SeriesRule {
		out := make([]SeriesRule, len(names))
		for i, s := range names {
			out[i] = SeriesRule{Series: s}
		}
		return out
	}
	bundledOnly := AcceleratorCompat{}
	return &Compat{
		Accelerators: map[string]AcceleratorCompat{
			"nvidia-tesla-t4":       {Series: []string{"n1"}, Counts: []int64{1, 2, 4}},
			"nvidia-tesla-t4-vws":   {Series: []string{"n1"}, Counts: []int64{1, 2, 4}},
			"nvidia-tesla-v100":     {Series: []string{"n1"}, Counts: []int64{1, 2, 4, 8}},
			"nvidia-tesla-p100":     {Series: []string{"n1"}, Counts: []int64{1, 2, 4}},
			"nvidia-tesla-p100-vws": {Series: []string{"n1"}, Counts: []int64{1, 2, 4}},
			"nvidia-tesla-p4":       {Series: []string{"n1"}, Counts: []int64{1, 2, 4}},
			"nvidia-tesla-p4-vws":   {Series: []string{"n1"}, Counts: []int64{1, 2, 4}},
			"nvidia-l4":             bundledOnly,
			"nvidia-l4-vws":         bundledOnly,
			"nvidia-tesla-a100":     bundledOnly,
			"nvidia-a100-80gb":      bundledOnly,
			"nvidia-h100-80gb":      bundledOnly,
			"nvidia-h100-mega-80gb": bundledOnly,
		},
		LocalSSDSeries:      []string{"n1", "n2", "n2d", "c2", "c2d"},
		LocalSSDPartitionGB: 375,
		HyperdiskOnly:       []string{"c4", "c4a", "c4d", "n4", "m4", "x4", "a4"},
		DiskTypes: map[string]DiskTypeCompat{
			"pd-standard": {},
			"pd-balanced": {},
			"pd-ssd":      {},
			"pd-extreme": {
				Attaches: []SeriesRule{{Series: "n2", MinCPUs: 64}, {Series: "m1"}, {Series: "m2"}},
				IOPS:     &Performance{Min: Linear{Base: 2500}, Max: Linear{Base: 120000}, Default: Linear{Base: 2500}},
			},
			"hyperdisk-balanced": {
				Attaches:   series("a3", "c3", "c3d", "c4", "c4a", "c4d", "h3", "m3", "n4", "z3"),
				IOPS:       &Performance{Min: Linear{Base: 3000}, Max: Linear{PerGB: 500, Lo: 3000, Hi: 160000}, Default: Linear{Base: 3000, PerGB: 6}},
				Throughput: &Performance{Min: Linear{Base: 140}, Max: Linear{Base: 2400}, Default: Linear{Base: 140, PerGB: 6, Per: 10}},
				AccessMode: true,
			},
			"hyperdisk-extreme": {
				Attaches: []SeriesRule{{Series: "c3"}, {Series: "c4"}, {Series: "m3"}, {Series: "n2", MinCPUs: 80}},
				IOPS:     &Performance{Min: Linear{Base: 2500}, Max: Linear{PerGB: 1000, Hi: 350000}, Default: Linear{PerGB: 100, Lo: 2500, Hi: 350000}},
			},
			"hyperdisk-throughput": {
				Attaches:   series("c3", "c3d", "g2", "h3", "m3", "n2", "n2d", "t2d"),
				Throughput: &Performance{Min: Linear{PerGB: 10, Per: 1024}, Max: Linear{PerGB: 90, Per: 1024, Hi: 600}, Default: Linear{PerGB: 10, Per: 1024}},
			},
			"hyperdisk-ml": {
				Attaches:   series("a2", "a3", "c3", "c3d", "g2"),
				Throughput: &Performance{Min: Linear{Base: 400}, Max: Linear{Base: 1200000}, Default: Linear{PerGB: 24, Lo: 400, Hi: 1200000}},
				AccessMode: true,
			},
		},
	}
}

// machineSeries is a machine type's series: n1 for n1-standard-4.
func machineSeries(name string) string {
	s, _, _ := strings.Cut(gceapi.LastSegment(name), "-")
	return s
}

// attaches reports whether disk type typ attaches to mt. A type the
// table doesn't know is left to GCE.
func (c *Compat) attaches(typ string, mt *compute.MachineType) bool {
	dt, ok := c.DiskTypes[typ]
	if !ok {
		return true
	}
	s := machineSeries(mt.Name)
	if len(dt.Attaches) == 0 {
		return !slices.Contains(c.HyperdiskOnly, s)
	}
	for _, r := range dt.Attaches {
		if r.Series == s && mt.GuestCpus >= r.MinCPUs {
			return true
		}
	}
	return false
}

// bundledGPUs are the GPUs machine type mt comes with, by type.
func bundledGPUs(mt *compute.MachineType) map[string]int64 {
	out := map[string]int64{}
	for _, a := range mt.Accelerators {
		if a != nil && a.GuestAcceleratorCount > 0 {
			out[gceapi.LastSegment(a.GuestAcceleratorType)] += a.GuestAcceleratorCount
		}
	}
	return out
}

// bundledLocalSSDs is the number of local SSD partitions mt comes with.
func bundledLocalSSDs(mt *compute.MachineType) int64 {
	if mt.BundledLocalSsds == nil {
		return 0
	}
	return mt.BundledLocalSsds.PartitionCount
}

// checkGPUs checks an instance's GPUs against its machine type, as GCE
// would (gcp-backend.md, "GPUs"): no guestAccelerators on a machine
// type that bundles its own; each attached type on a series that takes
// it, in a count GCE allows; and onHostMaintenance TERMINATE whenever
// there are any. accels are the instance's guestAccelerators, by type,
// with their field names; the zone checks are the caller's.
func (c *Compat) checkGPUs(mt *compute.MachineType, accels []attachedGPU, sc *compute.Scheduling) error {
	if len(bundledGPUs(mt)) > 0 && len(accels) > 0 {
		return invalid("Invalid value for field 'resource.guestAccelerators': machine type %s comes with its own GPUs, so takes no guestAccelerators", mt.Name)
	}
	for _, a := range accels {
		ac, ok := c.Accelerators[a.typ]
		if !ok {
			continue
		}
		if !slices.Contains(ac.Series, machineSeries(mt.Name)) {
			return invalid("Invalid value for field 'resource.%s.acceleratorType': '%s'. Accelerator type %s can't be attached to machine type %s", a.field, a.typ, a.typ, mt.Name)
		}
		if !slices.Contains(ac.Counts, a.count) {
			return invalid("Invalid value for field 'resource.%s.acceleratorCount': '%d'. %d is not a valid count of %s; allowed: %v", a.field, a.count, a.count, a.typ, ac.Counts)
		}
	}
	if len(accels) == 0 && len(bundledGPUs(mt)) == 0 {
		return nil
	}
	// GCE defaults onHostMaintenance to MIGRATE, and to TERMINATE for a
	// Spot or preemptible instance.
	ohm := ""
	if sc != nil {
		ohm = sc.OnHostMaintenance
	}
	if ohm == "" && instanceVariant(sc) == variantSpot {
		ohm = "TERMINATE"
	}
	if ohm != "TERMINATE" {
		return invalid("Invalid value for field 'resource.scheduling.onHostMaintenance': '%s'. Instances with GPUs do not support live migration; set it to TERMINATE", ohm)
	}
	return nil
}

// attachedGPU is one guestAccelerators entry.
type attachedGPU struct {
	field string // guestAccelerators[0]
	typ   string
	count int64
}

// checkLocalSSD checks an instance's attached local SSD partitions
// against its machine type.
func (c *Compat) checkLocalSSD(mt *compute.MachineType, fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	if bundledLocalSSDs(mt) > 0 || !slices.Contains(c.LocalSSDSeries, machineSeries(mt.Name)) {
		return invalid("Invalid value for field 'resource.%s': machine type %s does not support attaching local SSD", fields[0], mt.Name)
	}
	return nil
}

// typedDisk is a persistent disk on an instance, for the compatibility
// check: its field and its type.
type typedDisk struct {
	field string
	typ   string
}

// checkDisks checks each disk's type attaches to mt.
func (c *Compat) checkDisks(mt *compute.MachineType, disks []typedDisk) error {
	for _, d := range disks {
		if !c.attaches(d.typ, mt) {
			f := "resource"
			if d.field != "" {
				f += "." + d.field
			}
			return invalid("Invalid value for field '%s': disk type %s is not supported with machine type %s", f, d.typ, mt.Name)
		}
	}
	return nil
}
