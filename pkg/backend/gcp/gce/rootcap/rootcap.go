// Package rootcap reads the root silo's capacity from the host
// project's GCE quotas (docs/silo/allocators.md, "Root capacity",
// SV-33). install's root step calls Refresh once, and silod's leader
// every hour after.
//
// A Refresh reads, for each of the root's allowed regions, regions.get's
// quotas; projects.get's quotas; and the Cloud Quotas API's quotaInfos
// for compute.googleapis.com, which reports the limits regions.get
// leaves out (newer GPU families; Hyperdisk Extreme, Throughput and
// ML). A limit regions.get reports wins over Cloud Quotas'. It then sets
// the root's granted, with ledger.SetCapacity, to each limit less what
// the control plane's own objects use. A metric neither API reports, or
// that reports 0, gives the class no capacity.
//
// GPU metrics are discovered: a GPU metric the project has that silod's
// GPU config doesn't name gets a class (and its spot_ variant), and,
// where one of the region's accelerator types matches it, a row in the
// GPU config, so the next read gives it capacity.
//
// When a limit falls below what the tree holds, SetCapacity marks the
// row over-committed and logs quota.over_committed. Refresh logs
// quota.root_resync whenever it changes the root's capacity or the
// class table.
package rootcap

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
	compute "google.golang.org/api/compute/v1"
)

// Kind is the kind of every class Refresh sets.
const Kind = "gce"

// ControlPlanePrefix starts the name of every object install makes for
// the control plane (gcp-backend.md, "Naming"). No tenant object's name
// can start with it: a tenant's is <id>-<name>, with a six-character id.
const ControlPlanePrefix = "silo-"

// Config is what Refresh needs besides GCE and the database.
type Config struct {
	// Project is the host project.
	Project string
	// RootMemoryMB is install's --root-memory: the root's memory_mb and
	// spot_memory_mb in each allowed region, in MiB. GCE has no memory
	// quota, so 0 means unlimited.
	RootMemoryMB int64
	// ImageStorageGB and SnapshotStorageGB are the root's
	// image_storage_gb and snapshot_storage_gb. GCE has no quota for
	// them either, so 0 means unlimited.
	ImageStorageGB, SnapshotStorageGB int64
	// Guard, if set, is called first in the transaction that writes the
	// root's capacity, and refuses the write by returning an error:
	// silod's leader checks its epoch there (leader.Guard).
	Guard func(ctx context.Context, tx store.Tx) error
}

// Reader reads the root's capacity.
type Reader struct {
	db     store.Store
	gce    gce.Client
	config Config
}

// New returns a Reader.
func New(db store.Store, c gce.Client, config Config) *Reader {
	return &Reader{db: db, gce: c, config: config}
}

// regionalMetrics are the regional classes and the GCE metric each
// follows (allocators.md, "Classes"). GPU classes come from the GPU
// config instead.
var regionalMetrics = []struct{ class, metric string }{
	{"vcpu", gce.MetricCPUS},
	{"spot_vcpu", gce.MetricPreemptibleCPUS},
	{"instances", gce.MetricInstances},
	{"standard_disk_gb", gce.MetricDisksTotalGB},
	{"ssd_disk_gb", gce.MetricSSDTotalGB},
	{"pd_extreme_iops", gce.MetricPDExtremeIOPS},
	{"hyperdisk_balanced_gb", gce.MetricHDBTotalGB},
	{"hyperdisk_balanced_iops", gce.MetricHDBTotalIOPS},
	{"hyperdisk_balanced_throughput", gce.MetricHDBTotalThroughput},
	{"hyperdisk_extreme_gb", gce.MetricHDXTotalGB},
	{"hyperdisk_extreme_iops", gce.MetricHDXTotalIOPS},
	{"hyperdisk_throughput_gb", gce.MetricHDTTotalGB},
	{"hyperdisk_throughput_throughput", gce.MetricHDTTotalThroughput},
	{"hyperdisk_ml_gb", gce.MetricHDMLTotalGB},
	{"hyperdisk_ml_throughput", gce.MetricHDMLTotalThroughput},
	{"local_ssd_gb", gce.MetricLocalSSDTotalGB},
	{"spot_local_ssd_gb", gce.MetricPreemptibleLocalSSDGB},
}

// globalMetrics are the global classes GCE limits, and the project
// metric each follows (allocators.md, "Global counts").
var globalMetrics = []struct{ class, metric string }{
	{"subnetworks", gce.MetricSubnetworks},
	{"firewall_rules", gce.MetricFirewalls},
	{"images", gce.MetricImages},
	{"snapshots", gce.MetricSnapshots},
	{"instance_templates", gce.MetricInstanceTemplates},
	{"gpus_all_regions", gce.MetricGPUsAllRegions},
}

// The classes whose capacity is configuration, not a GCE limit.
const (
	classMemory          = "memory_mb"
	classSpotMemory      = "spot_memory_mb"
	classImageStorage    = "image_storage_gb"
	classSnapshotStore   = "snapshot_storage_gb"
	spotClassPrefix      = "spot_"
	gpuMetricSuffix      = "_GPUS"
	committedPrefix      = "COMMITTED_"
	cloudQuotasGPUFamily = "gpus_per_gpu_family"
)

// gpuMetric matches a per-family GPU metric: NVIDIA_L4_GPUS,
// PREEMPTIBLE_NVIDIA_H100_GPUS. GPUS_ALL_REGIONS and the COMMITTED_
// metrics are not one.
var gpuMetric = regexp.MustCompile(`^(PREEMPTIBLE_)?[A-Z][A-Z0-9_]*_GPUS$`)

func isGPUMetric(m string) bool {
	return gpuMetric.MatchString(m) && !strings.HasPrefix(m, committedPrefix) && m != gce.MetricGPUsAllRegions
}

// GPUClassName is the class of a GPU metric: the metric in lower case,
// with PREEMPTIBLE_ written spot_ (allocators.md, "GPU classes").
func GPUClassName(metric string) string {
	if base, ok := strings.CutPrefix(metric, gce.PreemptiblePrefix); ok {
		return spotClassPrefix + strings.ToLower(base)
	}
	return strings.ToLower(metric)
}

// Result is what a Refresh did.
type Result struct {
	// Capacity is the root's capacity as set, every class Refresh owns
	// in every allowed region.
	Capacity []ledger.Amount
	// ControlPlane is what the control plane's own objects use, which
	// was taken off the limits.
	ControlPlane []ledger.Amount
	// NoCapacity are the classes that got none because GCE reports no
	// limit, or 0, for their metric: what install's plan lists.
	NoCapacity []ledger.Key
	// AddedClasses are GPU classes discovered by this read.
	AddedClasses []string
	// Changed are the keys whose capacity this read changed.
	Changed []ledger.Key
	// OverCommitted are the root's over-committed rows after the read.
	OverCommitted []ledger.Row
}

// limits is what GCE reports: a limit per region and metric, region ""
// for a project-wide one.
type limits map[[2]string]float64

// Refresh reads the host project's quotas and sets the root's capacity
// from them. It changes nothing if any read fails, so a GCE outage
// leaves the capacity as it was.
func (r *Reader) Refresh(ctx context.Context) (*Result, error) {
	var root *tree.Silo
	var regions []string
	var gpus []ledger.GPUClass
	err := r.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		if root, err = tree.Root(ctx, tx); err != nil {
			return err
		}
		if regions, err = ledger.Regions(ctx, tx, root.ID); err != nil {
			return err
		}
		gpus, err = ledger.GPUClasses(ctx, tx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("rootcap: %w", err)
	}

	rd, err := r.compute(ctx, regions, gpus)
	if err != nil {
		return nil, err
	}
	res, gpuClasses, newGPUs := rd.Result, rd.gpuClasses, rd.newGPUs

	err = r.db.Tx(ctx, func(tx store.Tx) error {
		if r.config.Guard != nil {
			if err := r.config.Guard(ctx, tx); err != nil {
				return err
			}
		}
		res.AddedClasses, res.Changed, res.OverCommitted = nil, nil, nil
		// Every GPU class gets a row: a new family's, and the spot_
		// variant of one whose Spot metric isn't reported, so a Spot
		// instance of it is charged (and refused: it has no capacity).
		for _, class := range gpuClasses {
			if _, err := ledger.LookupClass(ctx, tx, Kind, class); err == nil {
				continue
			} else if ledger.CodeOf(err) != ledger.InvalidArgument {
				return err
			}
			if err := ledger.AddClass(ctx, tx, ledger.Class{Kind: Kind, Name: class, Unit: "GPU", Sort: ledger.SortHeld, Scope: ledger.ScopeRegional}); err != nil {
				return err
			}
			res.AddedClasses = append(res.AddedClasses, class)
		}
		for _, g := range newGPUs {
			if err := ledger.AddGPU(ctx, tx, g); err != nil {
				return err
			}
		}
		sort.Strings(res.AddedClasses)

		before := map[ledger.Key]int64{}
		for _, a := range res.Capacity {
			row, err := ledger.Get(ctx, tx, root.ID, a.Key)
			if err != nil {
				return err
			}
			before[a.Key] = row.Granted
		}
		if err := ledger.SetCapacity(ctx, tx, root.ID, res.Capacity); err != nil {
			return err
		}
		for _, a := range res.Capacity {
			row, err := ledger.Get(ctx, tx, root.ID, a.Key)
			if err != nil {
				return err
			}
			if row.Granted != before[a.Key] {
				res.Changed = append(res.Changed, a.Key)
			}
		}
		over, err := ledger.OverCommitted(ctx, tx)
		if err != nil {
			return err
		}
		res.OverCommitted = over
		if len(res.Changed) == 0 && len(res.AddedClasses) == 0 {
			return nil
		}
		return r.audit(ctx, tx, root.ID, res)
	})
	if err != nil {
		return nil, fmt.Errorf("rootcap: %w", err)
	}
	return res, nil
}

// reading is what compute read: the Result's capacity, and the GPU
// classes and new GPU families a Refresh records.
type reading struct {
	Result     *Result
	gpuClasses []string
	newGPUs    []ledger.GPUClass
}

// Read reads the root's capacity in regions from the host project's
// quotas, as Refresh does, without a database: install's root step
// calls it before the root exists, and hands the result to silod's
// bootstrap. gpus is silod's GPU config; a GPU metric it doesn't name
// gets a class of GPUClassName's.
func Read(ctx context.Context, c gce.Client, config Config, regions []string, gpus []ledger.GPUClass) (*Result, error) {
	rd, err := (&Reader{gce: c, config: config}).compute(ctx, regions, gpus)
	if err != nil {
		return nil, err
	}
	return rd.Result, nil
}

// compute reads the limits and works out the capacity they give.
func (r *Reader) compute(ctx context.Context, regions []string, gpus []ledger.GPUClass) (*reading, error) {
	lim, zones, err := r.read(ctx, regions)
	if err != nil {
		return nil, fmt.Errorf("rootcap: %w", err)
	}
	cp, err := r.controlPlane(ctx, regions, zones)
	if err != nil {
		return nil, fmt.Errorf("rootcap: control plane's use: %w", err)
	}

	// Discover GPU metrics the config doesn't name.
	known := map[string]bool{}
	for _, g := range gpus {
		known[g.Metric] = true
	}
	var newMetrics []string
	seen := map[string]bool{}
	for k := range lim {
		m := k[1]
		if k[0] == "" || !isGPUMetric(m) || known[m] || seen[m] {
			continue
		}
		seen[m] = true
		newMetrics = append(newMetrics, m)
	}
	sort.Strings(newMetrics)
	newGPUs, err := r.discover(ctx, newMetrics, zones, known)
	if err != nil {
		return nil, fmt.Errorf("rootcap: discovering GPU families: %w", err)
	}

	// Every GPU class with its metric: the config's, and the new ones.
	gpuMetrics := map[string]string{}
	for _, g := range gpus {
		gpuMetrics[g.Class] = g.Metric
	}
	for _, m := range newMetrics {
		gpuMetrics[GPUClassName(m)] = m
		if !strings.HasPrefix(m, gce.PreemptiblePrefix) {
			gpuMetrics[GPUClassName(gce.PreemptiblePrefix+m)] = gce.PreemptiblePrefix + m
		}
	}
	gpuClasses := make([]string, 0, len(gpuMetrics))
	for c := range gpuMetrics {
		gpuClasses = append(gpuClasses, c)
	}
	sort.Strings(gpuClasses)

	res := &Result{}
	cpUse := map[ledger.Key]int64{}
	for _, a := range cp {
		cpUse[a.Key] += a.Amount
	}
	set := func(k ledger.Key, limit float64, ok bool) {
		amount := int64(0)
		if ok && limit > 0 {
			amount = max(0, toAmount(limit)-cpUse[k])
		}
		if amount == 0 {
			res.NoCapacity = append(res.NoCapacity, k)
		}
		res.Capacity = append(res.Capacity, ledger.Amount{Key: k, Amount: amount})
	}
	fixed := func(k ledger.Key, units int64) {
		amount := ledger.Unlimited
		if units > 0 {
			amount = ledger.Units(units)
		}
		res.Capacity = append(res.Capacity, ledger.Amount{Key: k, Amount: amount})
	}
	for _, region := range regions {
		key := func(class string) ledger.Key { return ledger.Key{Kind: Kind, Class: class, Region: region} }
		for _, c := range regionalMetrics {
			v, ok := lim[[2]string{region, c.metric}]
			set(key(c.class), v, ok)
		}
		for _, class := range gpuClasses {
			v, ok := lim[[2]string{region, gpuMetrics[class]}]
			set(key(class), v, ok)
		}
		fixed(key(classMemory), r.config.RootMemoryMB)
		fixed(key(classSpotMemory), r.config.RootMemoryMB)
	}
	gkey := func(class string) ledger.Key { return ledger.Key{Kind: Kind, Class: class, Region: ledger.Global} }
	for _, c := range globalMetrics {
		v, ok := lim[[2]string{"", c.metric}]
		set(gkey(c.class), v, ok)
	}
	fixed(gkey(classImageStorage), r.config.ImageStorageGB)
	fixed(gkey(classSnapshotStore), r.config.SnapshotStorageGB)
	res.ControlPlane = cp

	return &reading{Result: res, gpuClasses: gpuClasses, newGPUs: newGPUs}, nil
}

// audit logs quota.root_resync: what changed, and what is now
// over-committed.
func (r *Reader) audit(ctx context.Context, tx store.Tx, root string, res *Result) error {
	type change struct {
		Class   string `json:"class"`
		Region  string `json:"region"`
		Granted string `json:"granted"`
	}
	type over struct {
		Class   string `json:"class"`
		Region  string `json:"region"`
		Granted string `json:"granted"`
		Held    string `json:"held"`
	}
	amounts := map[ledger.Key]int64{}
	for _, a := range res.Capacity {
		amounts[a.Key] = a.Amount
	}
	rec := struct {
		Project       string   `json:"project"`
		Changed       []change `json:"changed,omitempty"`
		AddedClasses  []string `json:"added_classes,omitempty"`
		OverCommitted []over   `json:"over_committed,omitempty"`
	}{Project: r.config.Project, AddedClasses: res.AddedClasses}
	for _, k := range res.Changed {
		g := "unlimited"
		if amounts[k] != ledger.Unlimited {
			g = ledger.FormatAmount(amounts[k])
		}
		rec.Changed = append(rec.Changed, change{k.Class, k.Region, g})
	}
	for _, row := range res.OverCommitted {
		rec.OverCommitted = append(rec.OverCommitted, over{row.Class, row.Region,
			ledger.FormatAmount(row.Granted), ledger.FormatAmount(row.Delegated + row.Used)})
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = tx.Append(ctx, "quota.root_resync", root, string(data))
	return err
}

// toAmount is a GCE limit as a ledger amount. Limits are whole numbers
// in practice; a fraction is kept to the ledger's precision.
func toAmount(limit float64) int64 {
	if limit >= float64(ledger.Unlimited/ledger.Micro) {
		return ledger.Unlimited
	}
	return int64(math.Round(limit * ledger.Micro))
}

// read makes the GCE reads: regions.get for each region, projects.get,
// and Cloud Quotas. It returns the limits and each region's zones.
func (r *Reader) read(ctx context.Context, regions []string) (limits, map[string][]string, error) {
	lim := limits{}
	zones := map[string][]string{}
	// Cloud Quotas first, so regions.get overrides it.
	infos, err := r.gce.ListComputeQuotaInfos(ctx, r.config.Project)
	if err != nil {
		return nil, nil, fmt.Errorf("cloud quotas: %w", err)
	}
	allowed := map[string]bool{}
	for _, region := range regions {
		allowed[region] = true
	}
	cloudQuotas(infos, allowed, lim)

	for _, region := range regions {
		reg, err := r.gce.GetRegion(ctx, r.config.Project, region)
		if err != nil {
			return nil, nil, fmt.Errorf("regions.get %s: %w", region, err)
		}
		for _, q := range reg.Quotas {
			lim[[2]string{region, q.Metric}] = q.Limit
		}
		for _, z := range reg.Zones {
			zones[region] = append(zones[region], gce.LastSegment(z))
		}
		sort.Strings(zones[region])
	}
	proj, err := r.gce.GetProject(ctx, r.config.Project)
	if err != nil {
		return nil, nil, fmt.Errorf("projects.get: %w", err)
	}
	for _, q := range proj.Quotas {
		lim[[2]string{"", q.Metric}] = q.Limit
	}
	return lim, zones, nil
}

// cloudQuotas adds Cloud Quotas' limits for the allowed regions, and
// its project-wide ones, to lim. A quota's metric is
// compute.googleapis.com/<metric in lower case>; per-family GPU quotas
// may instead be gpus_per_gpu_family with a gpu_family dimension. A
// limit for a region named in its dimensions wins over a default that
// applies to the region; a limit split by any other dimension (a VM
// family, say) is not one silod hands down.
func cloudQuotas(infos []*gce.QuotaInfo, allowed map[string]bool, lim limits) {
	explicit := map[[2]string]bool{}
	for _, qi := range infos {
		name, ok := strings.CutPrefix(qi.Metric, gce.CloudQuotasService+"/")
		if !ok {
			continue
		}
		perFamily := strings.HasSuffix(name, cloudQuotasGPUFamily)
		for _, di := range qi.DimensionsInfos {
			metric, region, ok := strings.ToUpper(name), "", !perFamily
			for k, v := range di.Dimensions {
				switch {
				case k == "region":
					region = v
				case k == "gpu_family" && perFamily && v != "":
					metric = strings.ToUpper(strings.TrimSuffix(name, cloudQuotasGPUFamily)+v) + gpuMetricSuffix
					ok = true
				default:
					ok = false
				}
			}
			if !ok {
				continue
			}
			v := float64(di.Details.Value)
			if region != "" {
				if allowed[region] {
					lim[[2]string{region, metric}] = v
					explicit[[2]string{region, metric}] = true
				}
				continue
			}
			locs := di.ApplicableLocations
			for _, loc := range locs {
				switch {
				case loc == "global":
					lim[[2]string{"", metric}] = v
				case allowed[loc] && !explicit[[2]string{loc, metric}]:
					lim[[2]string{loc, metric}] = v
				}
			}
			if len(locs) == 0 {
				lim[[2]string{"", metric}] = v
			}
		}
	}
}

// discover makes a GPU config row for each new standard metric, and its
// Spot variant, when one of the allowed regions' accelerator types
// matches the metric's family. An accelerator type matches the longest
// family its name starts with, ignoring "tesla-": nvidia-h100-80gb is
// NVIDIA_H100_GPUS unless NVIDIA_H100_80GB_GPUS exists.
func (r *Reader) discover(ctx context.Context, metrics []string, zones map[string][]string, known map[string]bool) ([]ledger.GPUClass, error) {
	if len(metrics) == 0 {
		return nil, nil
	}
	families := map[string]string{} // family prefix -> standard metric
	for _, m := range metrics {
		if strings.HasPrefix(m, gce.PreemptiblePrefix) {
			continue
		}
		families[family(m)] = m
	}
	for m := range known {
		if !strings.HasPrefix(m, gce.PreemptiblePrefix) && isGPUMetric(m) {
			families[family(m)] = m
		}
	}
	var all []string
	for _, zs := range zones {
		all = append(all, zs...)
	}
	sort.Strings(all)
	accels := map[string]bool{}
	for _, z := range all {
		list, err := r.gce.ListAcceleratorTypes(ctx, r.config.Project, z)
		if err != nil {
			return nil, err
		}
		for _, a := range list {
			accels[a.Name] = true
		}
	}
	names := make([]string, 0, len(accels))
	for a := range accels {
		names = append(names, a)
	}
	sort.Strings(names)
	var out []ledger.GPUClass
	for _, a := range names {
		norm := strings.Replace(a, "-tesla-", "-", 1)
		best := ""
		for f := range families {
			if (norm == f || strings.HasPrefix(norm, f+"-")) && len(f) > len(best) {
				best = f
			}
		}
		m := families[best]
		if best == "" || known[m] {
			continue
		}
		spot := gce.PreemptiblePrefix + m
		out = append(out,
			ledger.GPUClass{Accelerator: a, Metric: m, Class: GPUClassName(m)},
			ledger.GPUClass{Accelerator: a, Spot: true, Metric: spot, Class: GPUClassName(spot)})
	}
	return out, nil
}

// family is a GPU metric's accelerator-name prefix: NVIDIA_H100_GPUS is
// nvidia-h100.
func family(metric string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSuffix(metric, gpuMetricSuffix)), "_", "-")
}

// controlPlane is what the control plane's own objects use: the
// objects named silo-* (the control VMs, their disks and templates,
// the gateway and control subnets, install's firewall rules) in the
// allowed regions and globally.
func (r *Reader) controlPlane(ctx context.Context, regions []string, zones map[string][]string) ([]ledger.Amount, error) {
	p := r.config.Project
	f := gce.ListFilter{NamePrefix: ControlPlanePrefix}
	use := map[ledger.Key]int64{}
	machineTypes := map[string]*compute.MachineType{}
	for _, region := range regions {
		key := func(class string) ledger.Key { return ledger.Key{Kind: Kind, Class: class, Region: region} }
		for _, z := range zones[region] {
			insts, err := r.gce.ListInstances(ctx, p, z, f)
			if err != nil {
				return nil, err
			}
			for _, inst := range insts {
				use[key("instances")] += ledger.Units(1)
				if inst.Status == "TERMINATED" {
					continue
				}
				name := gce.LastSegment(inst.MachineType)
				mt := machineTypes[z+"/"+name]
				if mt == nil {
					if mt, err = r.gce.GetMachineType(ctx, p, z, name); err != nil {
						return nil, err
					}
					machineTypes[z+"/"+name] = mt
				}
				cpu := "vcpu"
				if s := inst.Scheduling; s != nil && (s.ProvisioningModel == "SPOT" || s.Preemptible) {
					cpu = "spot_vcpu"
				}
				use[key(cpu)] += ledger.Units(mt.GuestCpus)
			}
			disks, err := r.gce.ListDisks(ctx, p, z, f)
			if err != nil {
				return nil, err
			}
			for _, d := range disks {
				for class, n := range diskUse(d) {
					use[key(class)] += ledger.Units(n)
				}
			}
		}
		subs, err := r.gce.ListSubnetworks(ctx, p, region, f)
		if err != nil {
			return nil, err
		}
		use[global("subnetworks")] += ledger.Units(int64(len(subs)))
		tpls, err := r.gce.ListInstanceTemplates(ctx, p, region, f)
		if err != nil {
			return nil, err
		}
		use[global("instance_templates")] += ledger.Units(int64(len(tpls)))
	}
	fws, err := r.gce.ListFirewalls(ctx, p, f)
	if err != nil {
		return nil, err
	}
	use[global("firewall_rules")] += ledger.Units(int64(len(fws)))
	tpls, err := r.gce.ListInstanceTemplates(ctx, p, "", f)
	if err != nil {
		return nil, err
	}
	use[global("instance_templates")] += ledger.Units(int64(len(tpls)))

	var out []ledger.Amount
	for k, v := range use {
		if v != 0 {
			out = append(out, ledger.Amount{Key: k, Amount: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out, nil
}

func global(class string) ledger.Key {
	return ledger.Key{Kind: Kind, Class: class, Region: ledger.Global}
}

// diskUse is what a disk counts against, by class (allocators.md,
// "Classes").
func diskUse(d *compute.Disk) map[string]int64 {
	switch gce.LastSegment(d.Type) {
	case "pd-standard", "":
		return map[string]int64{"standard_disk_gb": d.SizeGb}
	case "pd-balanced", "pd-ssd":
		return map[string]int64{"ssd_disk_gb": d.SizeGb}
	case "pd-extreme":
		return map[string]int64{"ssd_disk_gb": d.SizeGb, "pd_extreme_iops": d.ProvisionedIops}
	case "hyperdisk-balanced":
		return map[string]int64{"hyperdisk_balanced_gb": d.SizeGb, "hyperdisk_balanced_iops": d.ProvisionedIops,
			"hyperdisk_balanced_throughput": d.ProvisionedThroughput}
	case "hyperdisk-extreme":
		return map[string]int64{"hyperdisk_extreme_gb": d.SizeGb, "hyperdisk_extreme_iops": d.ProvisionedIops}
	case "hyperdisk-throughput":
		return map[string]int64{"hyperdisk_throughput_gb": d.SizeGb, "hyperdisk_throughput_throughput": d.ProvisionedThroughput}
	case "hyperdisk-ml":
		return map[string]int64{"hyperdisk_ml_gb": d.SizeGb, "hyperdisk_ml_throughput": d.ProvisionedThroughput}
	}
	return nil
}
