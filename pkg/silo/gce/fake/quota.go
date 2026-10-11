package fake

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	compute "google.golang.org/api/compute/v1"
)

// quotaKey is a metric in a region, or project-wide when region is "".
type quotaKey struct {
	region string
	metric string
}

// SetQuota sets metric's limit in region, or the project-wide limit when
// region is "". Every project in the fake shares the limits.
func (f *Fake) SetQuota(region, metric string, limit float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits[quotaKey{region, metric}] = limit
}

// ClearQuota removes a limit SetQuota set: the metric is then neither
// reported nor enforced.
func (f *Fake) ClearQuota(region, metric string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.limits, quotaKey{region, metric})
}

// regionsGetMetrics are the regional metrics regions.get reports, apart
// from the GPU metrics of accelerator types not CloudQuotasOnly.
var regionsGetMetrics = []string{
	gce.MetricCPUS, gce.MetricPreemptibleCPUS, gce.MetricInstances,
	gce.MetricDisksTotalGB, gce.MetricSSDTotalGB,
	gce.MetricLocalSSDTotalGB, gce.MetricPreemptibleLocalSSDGB,
	gce.MetricPDExtremeIOPS,
	gce.MetricHDBTotalGB, gce.MetricHDBTotalIOPS, gce.MetricHDBTotalThroughput,
}

// inRegionsGet reports whether regions.get reports metric.
func (f *Fake) inRegionsGet(metric string) bool {
	if slices.Contains(regionsGetMetrics, metric) {
		return true
	}
	base := strings.TrimPrefix(metric, gce.PreemptiblePrefix)
	for _, a := range f.catalog.AcceleratorTypes {
		if a.Metric == base {
			return !a.CloudQuotasOnly
		}
	}
	return false
}

// spot is the metric Spot use of metric counts against in region: its
// PREEMPTIBLE_ variant if that has a limit above 0, else metric itself.
func (f *Fake) spot(region, metric string) string {
	if f.limits[quotaKey{region, gce.PreemptiblePrefix + metric}] > 0 {
		return gce.PreemptiblePrefix + metric
	}
	return metric
}

// usage is what every project's objects use, by metric. Objects still
// pending count for nothing.
func (f *Fake) usage() map[quotaKey]float64 {
	u := map[quotaKey]float64{}
	add := func(region, metric string, n float64) {
		if metric != "" && n != 0 {
			u[quotaKey{region, metric}] += n
		}
	}
	for _, p := range f.projects {
		for _, inst := range p.instances.all() {
			if p.pending[inst.SelfLink] {
				continue
			}
			zone := gce.LastSegment(inst.Zone)
			region := gce.ZoneRegion(zone)
			mt := f.catalog.machineType(zone, gce.LastSegment(inst.MachineType))
			if mt == nil {
				continue
			}
			isSpot := isSpot(inst.Scheduling)
			metric := func(m string) string {
				if isSpot {
					return f.spot(region, m)
				}
				return m
			}
			add(region, gce.MetricInstances, 1)
			lssd := mt.LocalSSDPartitions
			for _, d := range inst.Disks {
				if d.Type == "SCRATCH" {
					lssd++
				}
			}
			if isSpot && f.limits[quotaKey{region, gce.MetricPreemptibleLocalSSDGB}] > 0 {
				add(region, gce.MetricPreemptibleLocalSSDGB, float64(lssd*LocalSSDPartitionGB))
			} else {
				add(region, gce.MetricLocalSSDTotalGB, float64(lssd*LocalSSDPartitionGB))
			}
			if inst.Status == "TERMINATED" {
				continue
			}
			add(region, metric(gce.MetricCPUS), float64(mt.CPUs))
			for _, a := range instanceGPUs(inst, mt) {
				if spec := f.catalog.accelerator(a.typ); spec != nil {
					add(region, metric(spec.Metric), float64(a.count))
				}
				add("", gce.MetricGPUsAllRegions, float64(a.count))
			}
		}
		for _, d := range p.disks.all() {
			if p.pending[d.SelfLink] {
				continue
			}
			k := diskKinds[gce.LastSegment(d.Type)]
			if k == nil {
				continue
			}
			region := gce.ZoneRegion(gce.LastSegment(d.Zone))
			add(region, k.capacityMetric, float64(d.SizeGb))
			add(region, k.iopsMetric, float64(d.ProvisionedIops))
			add(region, k.throughputMetric, float64(d.ProvisionedThroughput))
		}
		count := func(metric string, links []string) {
			for _, l := range links {
				if !p.pending[l] {
					add("", metric, 1)
				}
			}
		}
		count(gce.MetricInstanceTemplates, selfLinks(p.templates.all(), func(t *compute.InstanceTemplate) string { return t.SelfLink }))
		count(gce.MetricSubnetworks, selfLinks(p.subnets.all(), func(s *compute.Subnetwork) string { return s.SelfLink }))
		count(gce.MetricFirewalls, selfLinks(p.firewalls.all(), func(fw *compute.Firewall) string { return fw.SelfLink }))
		count(gce.MetricNetworks, selfLinks(p.networks.all(), func(n *compute.Network) string { return n.SelfLink }))
		count(gce.MetricSnapshots, selfLinks(p.snapshots.all(), func(s *compute.Snapshot) string { return s.SelfLink }))
		count(gce.MetricImages, selfLinks(p.images.all(), func(i *compute.Image) string { return i.SelfLink }))
	}
	return u
}

func selfLinks[T any](objs []*T, link func(*T) string) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = link(o)
	}
	return out
}

// instanceGPUs are inst's GPUs: what its machine type bundles, or its
// guestAccelerators.
func instanceGPUs(inst *compute.Instance, mt *MachineTypeSpec) []accel {
	if mt.GPUs > 0 {
		return []accel{{mt.GPUType, mt.GPUs}}
	}
	var out []accel
	for _, a := range inst.GuestAccelerators {
		out = append(out, accel{gce.LastSegment(a.AcceleratorType), a.AcceleratorCount})
	}
	return out
}

func isSpot(s *compute.Scheduling) bool {
	return s != nil && (s.ProvisioningModel == "SPOT" || s.Preemptible)
}

// withQuota runs change, under the lock, and undoes it if it takes any
// metric that it raises over its limit, failing as GCE's operation
// would. change returns its undo.
func (f *Fake) withQuota(change func() (undo func())) *opErr {
	before := f.usage()
	undo := change()
	after := f.usage()
	var over []quotaKey
	for k, v := range after {
		if lim, ok := f.limits[k]; ok && v > before[k] && v > lim {
			over = append(over, k)
		}
	}
	if len(over) == 0 {
		return nil
	}
	undo()
	sort.Slice(over, func(i, j int) bool {
		return over[i].region+over[i].metric < over[j].region+over[j].metric
	})
	k := over[0]
	where := "project"
	if k.region != "" {
		where = "region " + k.region
	}
	return &opErr{gce.CodeQuotaExceeded, http.StatusForbidden,
		fmt.Sprintf("Quota '%s' exceeded.  Limit: %.1f in %s.", k.metric, f.limits[k], where)}
}

// regionQuotas is regions.get's quotas: the limits set in region that
// regions.get reports, with what is used.
func (f *Fake) regionQuotas(_ *state, region string) []*compute.Quota {
	u := f.usage()
	var out []*compute.Quota
	for k, lim := range f.limits {
		if k.region == region && f.inRegionsGet(k.metric) {
			out = append(out, &compute.Quota{Metric: k.metric, Limit: lim, Usage: u[k]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}

// GetProject implements gce.Client: the project with its project-wide
// quotas.
func (f *Fake) GetProject(ctx context.Context, project string) (*compute.Project, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	u := f.usage()
	out := &compute.Project{
		Kind:                   "compute#project",
		Name:                   p.name,
		Id:                     uint64(p.number),
		SelfLink:               gce.SelfLinkBase + "projects/" + p.name,
		CommonInstanceMetadata: &compute.Metadata{Kind: "compute#metadata"},
	}
	for k, lim := range f.limits {
		if k.region == "" {
			out.Quotas = append(out.Quotas, &compute.Quota{Metric: k.metric, Limit: lim, Usage: u[k]})
		}
	}
	sort.Slice(out.Quotas, func(i, j int) bool { return out.Quotas[i].Metric < out.Quotas[j].Metric })
	return out, nil
}

// ListComputeQuotaInfos implements gce.Client: every limit set, regional
// and project-wide, in Cloud Quotas' shape. Cloud Quotas reports limits
// only, not usage.
func (f *Fake) ListComputeQuotaInfos(ctx context.Context, project string) ([]*gce.QuotaInfo, error) {
	_, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	byID := map[string]*gce.QuotaInfo{}
	for k, lim := range f.limits {
		id := k.metric + "-per-project"
		if k.region != "" {
			id += "-region"
		}
		qi := byID[id]
		if qi == nil {
			qi = &gce.QuotaInfo{
				Name:      "projects/" + project + "/locations/global/services/" + gce.CloudQuotasService + "/quotaInfos/" + id,
				QuotaID:   id,
				Metric:    gce.CloudQuotasService + "/" + strings.ToLower(k.metric),
				Service:   gce.CloudQuotasService,
				IsPrecise: true,
			}
			byID[id] = qi
		}
		di := &gce.QuotaDimensionsInfo{Details: gce.QuotaDetails{Value: int64(lim)}}
		if k.region != "" {
			di.Dimensions = map[string]string{"region": k.region}
			di.ApplicableLocations = []string{k.region}
		} else {
			di.ApplicableLocations = []string{"global"}
		}
		qi.DimensionsInfos = append(qi.DimensionsInfos, di)
	}
	var out []*gce.QuotaInfo
	for _, qi := range byID {
		sort.Slice(qi.DimensionsInfos, func(i, j int) bool {
			return qi.DimensionsInfos[i].Dimensions["region"] < qi.DimensionsInfos[j].Dimensions["region"]
		})
		out = append(out, qi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QuotaID < out[j].QuotaID })
	return out, nil
}
