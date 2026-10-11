package gce

import (
	"context"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	gceapi "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

// handler serves one method, after every check in serve, and returns
// the response body.
type handler func(s *Server, q *request) (any, error)

// handlers are the methods the facade serves: the allowlist of methods
// (SV-63). Every one is in pkg/silo/control's table, which a test
// checks. The resource write paths add theirs.
var handlers = map[string]handler{
	"projects.get": (*Server).getProject,

	"regions.get":  (*Server).getRegion,
	"regions.list": (*Server).listRegions,
	"zones.get":    (*Server).getZone,
	"zones.list":   (*Server).listZones,

	"machineTypes.get": catalogGet("compute#machineType", func(s *Server, ctx context.Context, z, n string) (any, error) {
		return s.gce.GetMachineType(ctx, s.opts.HostProject, z, n)
	}),
	"machineTypes.list":           catalogList("compute#machineType", "machineTypes", (*Server).machineTypes),
	"machineTypes.aggregatedList": catalogAggregated("compute#machineType", "machineTypes", (*Server).machineTypes),
	"diskTypes.get": catalogGet("compute#diskType", func(s *Server, ctx context.Context, z, n string) (any, error) {
		return s.gce.GetDiskType(ctx, s.opts.HostProject, z, n)
	}),
	"diskTypes.list":           catalogList("compute#diskType", "diskTypes", (*Server).diskTypes),
	"diskTypes.aggregatedList": catalogAggregated("compute#diskType", "diskTypes", (*Server).diskTypes),
	"acceleratorTypes.get": catalogGet("compute#acceleratorType", func(s *Server, ctx context.Context, z, n string) (any, error) {
		return s.gce.GetAcceleratorType(ctx, s.opts.HostProject, z, n)
	}),
	"acceleratorTypes.list":           catalogList("compute#acceleratorType", "acceleratorTypes", (*Server).acceleratorTypes),
	"acceleratorTypes.aggregatedList": catalogAggregated("compute#acceleratorType", "acceleratorTypes", (*Server).acceleratorTypes),

	"images.get":           (*Server).getImage,
	"images.getFromFamily": (*Server).getImageFromFamily,
	"images.list":          (*Server).listImages,

	"networks.get":  (*Server).getNetwork,
	"networks.list": (*Server).listNetworks,

	"zoneOperations.get":    (*Server).getOperation,
	"zoneOperations.wait":   (*Server).waitOperation,
	"zoneOperations.list":   (*Server).listOperations,
	"regionOperations.get":  (*Server).getOperation,
	"regionOperations.wait": (*Server).waitOperation,
	"regionOperations.list": (*Server).listOperations,
	"globalOperations.get":  (*Server).getOperation,
	"globalOperations.wait": (*Server).waitOperation,
	"globalOperations.list": (*Server).listOperations,
}

// ---- Quotas ----

// regionalMetrics are the GCE metric each regional class is shown
// as in regions.get (allocators.md, "Classes"). Memory has no GCE
// metric, so the facade names its own. GPU classes come from the GPU
// table.
var regionalMetrics = map[string]string{
	"vcpu": gceapi.MetricCPUS, "spot_vcpu": gceapi.MetricPreemptibleCPUS,
	"memory_mb": "SILO_MEMORY_MB", "spot_memory_mb": "SILO_PREEMPTIBLE_MEMORY_MB",
	"instances": gceapi.MetricInstances, "standard_disk_gb": gceapi.MetricDisksTotalGB,
	"ssd_disk_gb": gceapi.MetricSSDTotalGB, "pd_extreme_iops": gceapi.MetricPDExtremeIOPS,
	"hyperdisk_balanced_gb": gceapi.MetricHDBTotalGB, "hyperdisk_balanced_iops": gceapi.MetricHDBTotalIOPS,
	"hyperdisk_balanced_throughput": gceapi.MetricHDBTotalThroughput,
	"hyperdisk_extreme_gb":          gceapi.MetricHDXTotalGB, "hyperdisk_extreme_iops": gceapi.MetricHDXTotalIOPS,
	"hyperdisk_throughput_gb": gceapi.MetricHDTTotalGB, "hyperdisk_throughput_throughput": gceapi.MetricHDTTotalThroughput,
	"hyperdisk_ml_gb": gceapi.MetricHDMLTotalGB, "hyperdisk_ml_throughput": gceapi.MetricHDMLTotalThroughput,
	"local_ssd_gb": gceapi.MetricLocalSSDTotalGB, "spot_local_ssd_gb": gceapi.MetricPreemptibleLocalSSDGB,
}

// globalMetrics are the GCE project metric each global count is shown
// as in projects.get (allocators.md, "Global counts"); one GCE doesn't
// have is SILO_ and the class.
var globalMetrics = map[string]string{
	"subnetworks": gceapi.MetricSubnetworks, "firewall_rules": gceapi.MetricFirewalls,
	"images": gceapi.MetricImages, "snapshots": gceapi.MetricSnapshots,
	"instance_templates": gceapi.MetricInstanceTemplates, "gpus_all_regions": gceapi.MetricGPUsAllRegions,
}

// quotas are the silo's quotas in region (ledger.Global for the global
// counts), in GCE's shape: limit = granted − delegated, usage = used.
func quotas(ctx context.Context, tx store.Tx, silo, region string) ([]map[string]any, error) {
	classes, err := ledger.Classes(ctx, tx)
	if err != nil {
		return nil, err
	}
	gpus, err := ledger.GPUClasses(ctx, tx)
	if err != nil {
		return nil, err
	}
	gpuMetric := map[string]string{}
	for _, g := range gpus {
		gpuMetric[g.Class] = g.Metric
	}
	rows, err := ledger.Quota(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	byClass := map[string]ledger.Row{}
	for _, r := range rows {
		if r.Kind == "gce" && r.Region == region {
			byClass[r.Class] = r
		}
	}
	out := []map[string]any{}
	for _, c := range classes {
		if c.Kind != "gce" || (c.Scope == ledger.ScopeGlobal) != (region == ledger.Global) {
			continue
		}
		metric := regionalMetrics[c.Name]
		if region == ledger.Global {
			metric = globalMetrics[c.Name]
		}
		if metric == "" {
			metric = gpuMetric[c.Name]
		}
		if metric == "" {
			metric = "SILO_" + strings.ToUpper(c.Name)
		}
		r := byClass[c.Name]
		out = append(out, map[string]any{
			"metric": metric,
			"limit":  float64(r.Granted-r.Delegated) / ledger.Micro,
			"usage":  float64(r.Used) / ledger.Micro,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["metric"].(string) < out[j]["metric"].(string) })
	return out, nil
}

// numericID is a stable id for an object the facade synthesizes.
func numericID(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64()>>1, 10)
}

// ---- projects ----

func (s *Server) getProject(q *request) (any, error) {
	var qs []map[string]any
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		qs, err = quotas(q.ctx(), tx, q.target.ID, ledger.Global)
		return err
	})
	if err != nil {
		return nil, err
	}
	p := q.n.project()
	return map[string]any{
		"kind":                   "compute#project",
		"id":                     numericID("project/" + q.target.ID),
		"name":                   p,
		"creationTimestamp":      q.target.Created.UTC().Format("2006-01-02T15:04:05.000-07:00"),
		"description":            "silo " + q.target.Path,
		"commonInstanceMetadata": map[string]any{"kind": "compute#metadata", "items": []any{}},
		"quotas":                 qs,
		"defaultNetworkTier":     "PREMIUM",
		"selfLink":               s.opts.Endpoint + "projects/" + p,
	}, nil
}

// ---- regions and zones ----

func (s *Server) region(q *request, r *compute.Region) (map[string]any, error) {
	out, err := q.shape("compute#region", r)
	if err != nil {
		return nil, err
	}
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		qs, err := quotas(q.ctx(), tx, q.target.ID, r.Name)
		out["quotas"] = qs
		return err
	})
	return out, err
}

func (s *Server) getRegion(q *request) (any, error) {
	r, err := s.gce.GetRegion(q.ctx(), s.opts.HostProject, q.call.Region)
	if err != nil {
		return nil, err
	}
	return s.region(q, r)
}

func (s *Server) listRegions(q *request) (any, error) {
	rs, err := s.gce.ListRegions(q.ctx(), s.opts.HostProject)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	for _, r := range rs {
		if !q.regions[r.Name] {
			continue
		}
		m, err := s.region(q, r)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#regionList", items)
}

func (s *Server) getZone(q *request) (any, error) {
	z, err := s.gce.GetZone(q.ctx(), s.opts.HostProject, q.call.Zone)
	if err != nil {
		return nil, err
	}
	return q.shape("compute#zone", z)
}

// allowedZones are the zones in the silo's allowed regions, by name.
func (s *Server) allowedZones(q *request) ([]*compute.Zone, error) {
	zs, err := s.gce.ListZones(q.ctx(), s.opts.HostProject)
	if err != nil {
		return nil, err
	}
	var out []*compute.Zone
	for _, z := range zs {
		if q.regions[gceapi.ZoneRegion(z.Name)] {
			out = append(out, z)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) listZones(q *request) (any, error) {
	zs, err := s.allowedZones(q)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	for _, z := range zs {
		m, err := q.shape("compute#zone", z)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#zoneList", items)
}

// ---- the zonal catalog: machine, disk and accelerator types ----

type zoneLister func(s *Server, ctx context.Context, zone string) ([]any, error)

func (s *Server) machineTypes(ctx context.Context, zone string) ([]any, error) {
	l, err := s.gce.ListMachineTypes(ctx, s.opts.HostProject, zone)
	return anys(l), err
}

func (s *Server) diskTypes(ctx context.Context, zone string) ([]any, error) {
	l, err := s.gce.ListDiskTypes(ctx, s.opts.HostProject, zone)
	return anys(l), err
}

func (s *Server) acceleratorTypes(ctx context.Context, zone string) ([]any, error) {
	l, err := s.gce.ListAcceleratorTypes(ctx, s.opts.HostProject, zone)
	return anys(l), err
}

func anys[T any](l []T) []any {
	out := make([]any, len(l))
	for i, v := range l {
		out[i] = v
	}
	return out
}

func catalogGet(kind string, get func(s *Server, ctx context.Context, zone, name string) (any, error)) handler {
	return func(s *Server, q *request) (any, error) {
		o, err := get(s, q.ctx(), q.call.Zone, q.call.Name)
		if err != nil {
			return nil, err
		}
		return q.shape(kind, o)
	}
}

func catalogList(kind, coll string, list zoneLister) handler {
	return func(s *Server, q *request) (any, error) {
		l, err := list(s, q.ctx(), q.call.Zone)
		if err != nil {
			return nil, err
		}
		var items []map[string]any
		for _, o := range l {
			m, err := q.shape(kind, o)
			if err != nil {
				return nil, err
			}
			items = append(items, m)
		}
		return s.list(q, kind+"List", items)
	}
}

func catalogAggregated(kind, coll string, list zoneLister) handler {
	return func(s *Server, q *request) (any, error) {
		zs, err := s.allowedZones(q)
		if err != nil {
			return nil, err
		}
		var items []scoped
		for _, z := range zs {
			l, err := list(s, q.ctx(), z.Name)
			if err != nil {
				return nil, err
			}
			for _, o := range l {
				m, err := q.shape(kind, o)
				if err != nil {
					return nil, err
				}
				items = append(items, scoped{scope: "zones/" + z.Name, item: m})
			}
		}
		return s.aggregated(q, kind+"AggregatedList", coll, items)
	}
}

// ---- images ----

// ownImage reports whether img is the silo's: labelled silo=<id> and
// named with its prefix.
func (q *request) owns(name string, labels map[string]string) bool {
	_, ok := q.n.tenantName(name)
	return ok && labels["silo"] == q.target.ID
}

func (s *Server) getImage(q *request) (any, error) {
	if q.public {
		img, err := s.gce.GetImage(q.ctx(), q.call.Project, q.call.Name)
		if err != nil {
			return nil, err
		}
		return q.shape("compute#image", img)
	}
	if err := checkName(q.call.Name); err != nil {
		return nil, err
	}
	img, err := s.gce.GetImage(q.ctx(), s.opts.HostProject, q.n.realName(q.call.Name))
	if err != nil || !q.owns(img.Name, img.Labels) {
		if err == nil || gceapi.IsNotFound(err) {
			return nil, notFound("projects/" + q.call.Project + "/global/images/" + q.call.Name)
		}
		return nil, err
	}
	return q.shape("compute#image", img)
}

func (s *Server) ownImages(q *request) ([]*compute.Image, error) {
	l, err := s.gce.ListImages(q.ctx(), s.opts.HostProject, gceapi.ListFilter{Labels: map[string]string{"silo": q.target.ID}, NamePrefix: q.n.prefix()})
	if err != nil {
		return nil, err
	}
	var out []*compute.Image
	for _, img := range l {
		if q.owns(img.Name, img.Labels) {
			out = append(out, img)
		}
	}
	return out, nil
}

func (s *Server) getImageFromFamily(q *request) (any, error) {
	if q.public {
		img, err := s.gce.GetImageFromFamily(q.ctx(), q.call.Project, q.call.Name)
		if err != nil {
			return nil, err
		}
		return q.shape("compute#image", img)
	}
	l, err := s.ownImages(q)
	if err != nil {
		return nil, err
	}
	var best *compute.Image
	for _, img := range l {
		if img.Family != q.call.Name || (img.Deprecated != nil && img.Deprecated.State != "" && img.Deprecated.State != "ACTIVE") {
			continue
		}
		if best == nil || img.CreationTimestamp > best.CreationTimestamp {
			best = img
		}
	}
	if best == nil {
		return nil, notFound("projects/" + q.call.Project + "/global/images/family/" + q.call.Name)
	}
	return q.shape("compute#image", best)
}

func (s *Server) listImages(q *request) (any, error) {
	var l []*compute.Image
	var err error
	if q.public {
		l, err = s.gce.ListImages(q.ctx(), q.call.Project, gceapi.ListFilter{})
	} else {
		l, err = s.ownImages(q)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	var items []map[string]any
	for _, img := range l {
		m, err := q.shape("compute#image", img)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#imageList", items)
}

// ---- networks ----

// network is the facade's one network, default: a fiction over the
// silo's own space in the shared VPC (gcp-backend.md, "Subnetworks").
func (s *Server) network(q *request) (map[string]any, error) {
	var blocks []string
	err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
		bs, err := address.Blocks(q.ctx(), tx, q.target.ID)
		for _, b := range bs {
			blocks = append(blocks, b.String())
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if blocks == nil {
		blocks = []string{}
	}
	subnets := []string{}
	for r := range q.regions {
		l, err := s.gce.ListSubnetworks(q.ctx(), s.opts.HostProject, r, gceapi.ListFilter{NamePrefix: q.n.prefix()})
		if err != nil && !gceapi.IsNotFound(err) {
			return nil, err
		}
		for _, sn := range l {
			if gceapi.LastSegment(sn.Network) == s.opts.Network {
				subnets = append(subnets, sn.SelfLink)
			}
		}
	}
	sort.Strings(subnets)
	n := map[string]any{
		"kind":                  "compute#network",
		"id":                    numericID("network/" + q.target.ID),
		"creationTimestamp":     q.target.Created.UTC().Format("2006-01-02T15:04:05.000-07:00"),
		"name":                  facadeNetwork,
		"description":           "silo " + q.target.Path + " address blocks: " + strings.Join(blocks, ", "),
		"autoCreateSubnetworks": false,
		"routingConfig":         map[string]any{"routingMode": "GLOBAL"},
		"subnetworks":           anys(subnets),
		"siloAddressBlocks":     blocks,
		"selfLink":              s.opts.Endpoint + "projects/" + q.call.Project + "/global/networks/" + facadeNetwork,
	}
	return q.shape("compute#network", n)
}

func (s *Server) getNetwork(q *request) (any, error) {
	if q.call.Name != facadeNetwork {
		return nil, notFound("projects/" + q.call.Project + "/global/networks/" + q.call.Name)
	}
	return s.network(q)
}

func (s *Server) listNetworks(q *request) (any, error) {
	n, err := s.network(q)
	if err != nil {
		return nil, err
	}
	return s.list(q, "compute#networkList", []map[string]any{n})
}
