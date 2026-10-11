package fake

import (
	"fmt"
	"slices"
	"sort"

	"github.com/bwsalmon/silo/pkg/silo/gce"
)

// diskKind is a disk type: its sizes, which performance values may be
// provisioned, their ranges and GCE's defaults, and the quota metrics
// it counts against.
//
// The ranges and defaults follow GCE's published ones at the time of
// writing, simplified to a formula per type. The conformance suite is
// what checks them against GCE.
type diskKind struct {
	name              string
	defaultGB         int64
	minGB, maxGB      int64
	iops, throughput  bool
	accessMode        bool
	iopsRange         func(gb int64) (lo, hi int64)
	iopsDefault       func(gb int64) int64
	throughputRange   func(gb int64) (lo, hi int64)
	throughputDefault func(gb int64) int64
	capacityMetric    string
	iopsMetric        string
	throughputMetric  string
	// series, if set, are the only machine series the type attaches to;
	// compatible decides when it is nil.
	series     []string
	compatible func(mt *MachineTypeSpec) bool
}

func (k *diskKind) validSize() string { return fmt.Sprintf("%dGB-%dGB", k.minGB, k.maxGB) }

func clamp(v, lo, hi int64) int64 { return max(lo, min(v, hi)) }

// hyperdiskOnly are the machine series that take no pd-* disks.
var hyperdiskOnly = []string{"c4", "n4"}

func anyButHyperdiskOnly(mt *MachineTypeSpec) bool {
	return !slices.Contains(hyperdiskOnly, series(mt.Name))
}

var diskKinds = map[string]*diskKind{
	"pd-standard": {name: "pd-standard", defaultGB: 500, minGB: 10, maxGB: 65536,
		capacityMetric: gce.MetricDisksTotalGB, compatible: anyButHyperdiskOnly},
	"pd-balanced": {name: "pd-balanced", defaultGB: 100, minGB: 10, maxGB: 65536,
		capacityMetric: gce.MetricSSDTotalGB, compatible: anyButHyperdiskOnly},
	"pd-ssd": {name: "pd-ssd", defaultGB: 100, minGB: 10, maxGB: 65536,
		capacityMetric: gce.MetricSSDTotalGB, compatible: anyButHyperdiskOnly},
	// pd-extreme attaches only to N2 with 64 or more vCPUs and to M1
	// and M2.
	"pd-extreme": {name: "pd-extreme", defaultGB: 1000, minGB: 500, maxGB: 65536, iops: true,
		iopsRange:      func(int64) (int64, int64) { return 2500, 120000 },
		iopsDefault:    func(int64) int64 { return 2500 },
		capacityMetric: gce.MetricSSDTotalGB, iopsMetric: gce.MetricPDExtremeIOPS,
		compatible: func(mt *MachineTypeSpec) bool {
			s := series(mt.Name)
			return (s == "n2" && mt.CPUs >= 64) || s == "m1" || s == "m2"
		}},
	"hyperdisk-balanced": {name: "hyperdisk-balanced", defaultGB: 100, minGB: 4, maxGB: 65536, iops: true, throughput: true, accessMode: true,
		iopsRange:         func(gb int64) (int64, int64) { return 3000, max(3000, min(500*gb, 160000)) },
		iopsDefault:       func(gb int64) int64 { return clamp(3000+6*gb, 3000, max(3000, min(500*gb, 160000))) },
		throughputRange:   func(gb int64) (int64, int64) { return 140, 2400 },
		throughputDefault: func(gb int64) int64 { return clamp(140+gb*6/10, 140, 2400) },
		capacityMetric:    gce.MetricHDBTotalGB, iopsMetric: gce.MetricHDBTotalIOPS, throughputMetric: gce.MetricHDBTotalThroughput,
		series: []string{"a3", "c3", "c3d", "c4", "h3", "m3", "n4", "z3"}},
	"hyperdisk-extreme": {name: "hyperdisk-extreme", defaultGB: 1000, minGB: 64, maxGB: 65536, iops: true,
		iopsRange:      func(gb int64) (int64, int64) { return 2500, min(1000*gb, 350000) },
		iopsDefault:    func(gb int64) int64 { return clamp(100*gb, 2500, 350000) },
		capacityMetric: gce.MetricHDXTotalGB, iopsMetric: gce.MetricHDXTotalIOPS,
		compatible: func(mt *MachineTypeSpec) bool {
			s := series(mt.Name)
			return s == "c3" || s == "c4" || s == "m3" || (s == "n2" && mt.CPUs >= 80)
		}},
	"hyperdisk-throughput": {name: "hyperdisk-throughput", defaultGB: 2048, minGB: 2048, maxGB: 32768, throughput: true,
		throughputRange:   func(gb int64) (int64, int64) { return 10 * gb / 1024, min(90*gb/1024, 600) },
		throughputDefault: func(gb int64) int64 { return 10 * gb / 1024 },
		capacityMetric:    gce.MetricHDTTotalGB, throughputMetric: gce.MetricHDTTotalThroughput,
		series: []string{"c3", "c3d", "g2", "h3", "m3", "n2", "n2d", "t2d"}},
	"hyperdisk-ml": {name: "hyperdisk-ml", defaultGB: 100, minGB: 4, maxGB: 65536, throughput: true, accessMode: true,
		throughputRange:   func(gb int64) (int64, int64) { return 400, 1200000 },
		throughputDefault: func(gb int64) int64 { return clamp(24*gb, 400, 1200000) },
		capacityMetric:    gce.MetricHDMLTotalGB, throughputMetric: gce.MetricHDMLTotalThroughput,
		series: []string{"a2", "a3", "c3", "c3d", "g2"}},
}

// localSSD is the disk type of a SCRATCH disk.
const localSSD = "local-ssd"

// localSSDSeries are the series local SSD partitions may be attached to
// (machine types that bundle it take no more).
var localSSDSeries = []string{"n1", "n2", "n2d", "c2", "c2d"}

func diskKindNames() []string {
	names := make([]string, 0, len(diskKinds))
	for n := range diskKinds {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// attaches reports whether disk type k attaches to mt.
func (k *diskKind) attaches(mt *MachineTypeSpec) bool {
	if k.series != nil {
		return slices.Contains(k.series, series(mt.Name))
	}
	return k.compatible(mt)
}

// performance fills in and checks a disk's provisioned IOPS and
// throughput for size gb: GCE's defaults where none is given, and 400
// for a value out of range or one the type doesn't take.
func (k *diskKind) performance(field string, gb int64, iops, throughput *int64) error {
	if *iops != 0 && !k.iops {
		return invalid(field+".provisionedIops", "Disk type %s does not support provisioned IOPS", k.name)
	}
	if *throughput != 0 && !k.throughput {
		return invalid(field+".provisionedThroughput", "Disk type %s does not support provisioned throughput", k.name)
	}
	if k.iops {
		if *iops == 0 {
			*iops = k.iopsDefault(gb)
		}
		if lo, hi := k.iopsRange(gb); *iops < lo || *iops > hi {
			return invalid(field+".provisionedIops", "%d is out of range [%d, %d] for a %d GB %s disk", *iops, lo, hi, gb, k.name)
		}
	}
	if k.throughput {
		if *throughput == 0 {
			*throughput = k.throughputDefault(gb)
		}
		if lo, hi := k.throughputRange(gb); *throughput < lo || *throughput > hi {
			return invalid(field+".provisionedThroughput", "%d is out of range [%d, %d] for a %d GB %s disk", *throughput, lo, hi, gb, k.name)
		}
	}
	return nil
}

// raiseToMinimum is what GCE does to a Hyperdisk's performance on
// resize: raise it to the new size's minimum.
func (k *diskKind) raiseToMinimum(gb int64, iops, throughput *int64) {
	if k.iops {
		lo, _ := k.iopsRange(gb)
		*iops = max(*iops, lo)
	}
	if k.throughput {
		lo, _ := k.throughputRange(gb)
		*throughput = max(*throughput, lo)
	}
}

// checkGPUs checks an instance's GPUs as GCE does: attached ones offered
// in the zone, on a series that takes them, in a count GCE allows; none
// attached to a machine type that bundles its own; and onHostMaintenance
// TERMINATE whenever there are any (Spot defaults to TERMINATE, standard
// to MIGRATE).
func (f *Fake) checkGPUs(zone string, mt *MachineTypeSpec, accels []accel, onHostMaintenance string, spot bool) error {
	if mt.GPUs > 0 && len(accels) > 0 {
		return invalid("resource.guestAccelerators", "Machine type %s comes with its own accelerators; guestAccelerators must be empty", mt.Name)
	}
	for i, a := range accels {
		field := fmt.Sprintf("resource.guestAccelerators[%d].acceleratorType", i)
		spec := f.catalog.acceleratorInZone(zone, a.typ)
		if spec == nil {
			return invalid(field, "Accelerator type %s is not available in zone %s", a.typ, zone)
		}
		if !slices.Contains(spec.AttachSeries, series(mt.Name)) {
			return invalid(field, "Accelerator type %s can't be attached to machine type %s", a.typ, mt.Name)
		}
		if !slices.Contains(spec.AttachCounts, a.count) {
			return invalid(fmt.Sprintf("resource.guestAccelerators[%d].acceleratorCount", i),
				"%d is not a valid count of %s; allowed: %v", a.count, a.typ, spec.AttachCounts)
		}
	}
	if mt.GPUs == 0 && len(accels) == 0 {
		return nil
	}
	ohm := onHostMaintenance
	if ohm == "" {
		ohm = "MIGRATE"
		if spot {
			ohm = "TERMINATE"
		}
	}
	if ohm != "TERMINATE" {
		return invalid("resource.scheduling.onHostMaintenance", "Instances with guest accelerators do not support live migration")
	}
	return nil
}

type accel struct {
	typ   string
	count int64
}
