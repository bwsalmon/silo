package gce

import (
	"context"
	"errors"
	"net/http"
	"sort"

	gceapi "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

// Instance templates (gcp-backend.md, "Instance templates", SV-70). A
// tenant's template, global or regional, is the real <id>-<name>,
// described silo:<id> since a template has no labels of its own. Its
// properties are checked at insert as an instance would be, in a zone
// of the regions the template may make instances in, and rewritten as
// an instance's are: prefixed names and tags, the <id> tag, the silo
// labels, block-project-ssh-keys and no service accounts. It charges
// one of the global count instance_templates and no regional quota.
//
// An instance made from one (instances.insert with
// sourceInstanceTemplate) is checked and charged as the effective
// instance, the body merged over the template's properties, and GCE is
// given the whole of it with the real template's link, so that GCE's own
// merge adds nothing the facade didn't check.

func init() {
	for _, res := range []string{"instanceTemplates", "regionInstanceTemplates"} {
		for m, h := range map[string]handler{
			"insert": (*Server).insertTemplate,
			"get":    (*Server).getTemplate,
			"list":   (*Server).listTemplates,
			"delete": (*Server).deleteTemplate,
		} {
			handlers[res+"."+m] = h
		}
		pricers[res+".insert"] = func(s *Server, q *request, tx store.Tx) ([]ledger.Amount, error) {
			if _, err := s.planTemplate(q); err != nil {
				return nil, err
			}
			return templatesCharge, nil
		}
	}
}

// templatesCharge is what a template charges: one of the global count.
var templatesCharge = []ledger.Amount{{Key: ledger.Key{Kind: "gce", Class: "instance_templates", Region: ledger.Global}, Amount: ledger.Units(1)}}

// templateObject is the ledger's object for a real template in region,
// "" for a global one.
func templateObject(region, real string) string {
	if region == "" {
		region = "global"
	}
	return "instanceTemplate:" + region + "/" + real
}

func (q *request) templatePath(region, name string) string {
	if region == "" {
		return q.globalPath("instanceTemplates", name)
	}
	return "projects/" + q.call.Project + "/regions/" + region + "/instanceTemplates/" + name
}

// planTemplate checks an instance template insert, and returns the
// real template.
func (s *Server) planTemplate(q *request) (*compute.InstanceTemplate, error) {
	var in compute.InstanceTemplate
	if err := decode(q.body, &in); err != nil {
		return nil, err
	}
	if err := checkName(in.Name); err != nil {
		return nil, err
	}
	region := q.call.Region
	if in.Region != "" && gceapi.LastSegment(in.Region) != region {
		return nil, invalid("Invalid value for field 'resource.region': '%s'. It must be the region of the request", in.Region)
	}
	if in.Properties == nil {
		return nil, invalid("Invalid value for field 'resource.properties': ''. Required field not specified")
	}
	zones, err := s.templateZones(q, region, in.Properties)
	if err != nil {
		return nil, err
	}
	// The properties as an instance: the fields they share have the
	// same names. Its name only names the disks the check makes up.
	m, err := toMap(in.Properties)
	if err != nil {
		return nil, err
	}
	var inst compute.Instance
	if err := decode(m, &inst); err != nil {
		return nil, err
	}
	inst.Name = "template"
	// The template passes if an instance of it would in one of its
	// zones; GPUs and machine types are offered per zone.
	var first error
	for _, z := range zones {
		p, err := s.planInstanceIn(q, z, &inst, true)
		if err == nil {
			return q.realTemplate(&in, p.real)
		}
		if first == nil {
			first = err
		}
	}
	if first == nil {
		first = invalid("Invalid value for field 'resource.properties': silo: the template names no zone of the silo's allowed regions")
	}
	return nil, first
}

// templateZones are the zones a template's properties are checked in:
// a regional template's region's, or, for a global one, those of the
// regions its properties name through a subnetwork or a disk source, or
// of every allowed region if they name none. Every region named must be
// allowed; a source disk's zone is the only one.
func (s *Server) templateZones(q *request, region string, props *compute.InstanceProperties) ([]string, error) {
	named := map[string]bool{}
	zone := ""
	for i, nic := range props.NetworkInterfaces {
		if nic == nil || nic.Subnetwork == "" {
			continue
		}
		r, err := q.ref(nic.Subnetwork, "subnetworks", "resource.properties.networkInterfaces["+itoa(i)+"].subnetwork")
		if err != nil {
			return nil, err
		}
		if r.Region != "" {
			named[r.Region] = true
		}
	}
	for i, d := range props.Disks {
		if d == nil || d.Source == "" {
			continue
		}
		r, err := q.ref(d.Source, "disks", "resource.properties.disks["+itoa(i)+"].source")
		if err != nil {
			return nil, err
		}
		if r.Zone != "" {
			named[gceapi.ZoneRegion(r.Zone)] = true
			zone = r.Zone
		}
	}
	for _, r := range sortedKeys(named) {
		if !q.regions[r] {
			return nil, regionNotAllowed(q.target.Path, "region "+r)
		}
		if region != "" && r != region {
			return nil, invalid("Invalid value for field 'resource.properties': silo: a template in region %s names region %s", region, r)
		}
	}
	if zone != "" {
		return []string{zone}, nil
	}
	if region != "" {
		named = map[string]bool{region: true}
	}
	zs, err := s.allowedZones(q)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, z := range zs {
		if len(named) == 0 || named[gceapi.ZoneRegion(z.Name)] {
			out = append(out, z.Name)
		}
	}
	return out, nil
}

// realTemplate is the real template for in, whose properties checked as
// inst: the instance's rewriting, with what a template names by name
// (machine, disk and accelerator types, a source disk) as names, and
// what the tenant left to the instance (a new disk's name and size, an
// image family) left to it.
func (q *request) realTemplate(in *compute.InstanceTemplate, inst *compute.Instance) (*compute.InstanceTemplate, error) {
	props := in.Properties
	out := &compute.InstanceProperties{
		Description: props.Description, MachineType: gceapi.LastSegment(inst.MachineType),
		Labels: inst.Labels, Tags: inst.Tags, Metadata: inst.Metadata, NetworkInterfaces: inst.NetworkInterfaces,
		Scheduling: inst.Scheduling, ServiceAccounts: []*compute.ServiceAccount{}, ReservationAffinity: inst.ReservationAffinity,
		AdvancedMachineFeatures: inst.AdvancedMachineFeatures, ShieldedInstanceConfig: inst.ShieldedInstanceConfig,
		MinCpuPlatform: inst.MinCpuPlatform,
	}
	for _, a := range inst.GuestAccelerators {
		out.GuestAccelerators = append(out.GuestAccelerators, &compute.AcceleratorConfig{
			AcceleratorType: gceapi.LastSegment(a.AcceleratorType), AcceleratorCount: a.AcceleratorCount})
	}
	for i, ad := range inst.Disks {
		d := *ad
		if d.Source != "" {
			d.Source = gceapi.LastSegment(d.Source)
		}
		if ad.InitializeParams != nil {
			ip := *ad.InitializeParams
			orig := props.Disks[i].InitializeParams
			ip.DiskType = gceapi.LastSegment(ip.DiskType)
			ip.DiskSizeGb = orig.DiskSizeGb
			if orig.DiskName == "" {
				ip.DiskName = ""
			}
			for _, src := range []struct{ from, coll string }{{orig.SourceImage, "images"}, {orig.SourceSnapshot, "snapshots"}} {
				if src.from == "" {
					continue
				}
				link, err := q.n.inLink(src.from, src.coll)
				if err != nil {
					return nil, err
				}
				if src.coll == "images" {
					ip.SourceImage = link
				} else {
					ip.SourceSnapshot = link
				}
			}
			d.InitializeParams = &ip
		}
		out.Disks = append(out.Disks, &d)
	}
	return &compute.InstanceTemplate{Name: q.n.realName(in.Name), Description: q.describe(in.Description), Properties: out}, nil
}

func (q *request) ownsTemplate(t *compute.InstanceTemplate) bool {
	_, named := q.n.tenantName(t.Name)
	_, described := q.tenantDescription(t.Description)
	return named && described
}

// template is t as the tenant sees it.
func (q *request) template(t *compute.InstanceTemplate) (map[string]any, error) {
	c := *t
	c.Description, _ = q.tenantDescription(t.Description)
	return q.shape("compute#instanceTemplate", &c)
}

// ownTemplate is the silo's template the call names; another silo's,
// or none, is 404.
func (s *Server) ownTemplate(q *request) (*compute.InstanceTemplate, error) {
	region := q.call.Region
	nf := notFound(q.templatePath(region, q.call.Name))
	if checkName(q.call.Name) != nil {
		return nil, nf
	}
	t, err := s.gce.GetInstanceTemplate(q.ctx(), s.opts.HostProject, region, q.n.realName(q.call.Name))
	if err != nil {
		if gceapi.IsNotFound(err) {
			return nil, nf
		}
		return nil, err
	}
	if !q.ownsTemplate(t) {
		return nil, nf
	}
	return t, nil
}

// sourceTemplate is the template an instance in zone is made from: the
// silo's own, global or in the zone's region. Another silo's is 404 by
// any name form, its project's included.
func (s *Server) sourceTemplate(q *request, src, zone string) (*compute.InstanceTemplate, error) {
	link, err := q.n.inLink(src, "instanceTemplates")
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == http.StatusForbidden {
			return nil, notFound(src)
		}
		return nil, err
	}
	r := gceapi.ParseRef(link)
	if r.Collection != "" && r.Collection != "instanceTemplates" {
		return nil, invalid("Invalid value for field 'sourceInstanceTemplate': '%s'. It must name an instance template", src)
	}
	if r.Project != "" && r.Project != s.opts.HostProject {
		return nil, notFound(src)
	}
	name, _ := q.n.tenantName(r.Name)
	nf := notFound(q.templatePath(r.Region, name))
	if r.Region != "" && r.Region != gceapi.ZoneRegion(zone) {
		if !q.regions[r.Region] {
			return nil, nf
		}
		return nil, invalid("Invalid value for field 'sourceInstanceTemplate': '%s'. A regional template makes instances in its own region, %s", src, r.Region)
	}
	t, err := s.gce.GetInstanceTemplate(q.ctx(), s.opts.HostProject, r.Region, r.Name)
	if err != nil {
		if gceapi.IsNotFound(err) {
			return nil, nf
		}
		return nil, err
	}
	if !q.ownsTemplate(t) {
		return nil, nf
	}
	return t, nil
}

// effective is the instance body merged over t's properties as the
// tenant sees them, as GCE merges them: objects field by field, the
// body winning; lists replaced, but metadata items merged by key.
func (q *request) effective(t *compute.InstanceTemplate, body map[string]any) (map[string]any, error) {
	base := map[string]any{}
	if t.Properties != nil {
		m, err := toMap(t.Properties)
		if err != nil {
			return nil, err
		}
		base, _ = q.n.out(m, "").(map[string]any)
	}
	out := mergeOver(base, body)
	bm, _ := base["metadata"].(map[string]any)
	om, _ := body["metadata"].(map[string]any)
	if bm != nil && om != nil {
		out["metadata"].(map[string]any)["items"] = mergeItems(bm["items"], om["items"])
	}
	return out, nil
}

// mergeOver is over merged into base: objects recursively, anything
// else replaced.
func mergeOver(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		bm, ok1 := out[k].(map[string]any)
		om, ok2 := v.(map[string]any)
		if ok1 && ok2 {
			out[k] = mergeOver(bm, om)
		} else {
			out[k] = v
		}
	}
	return out
}

// mergeItems are metadata items, over's replacing base's of the same
// key.
func mergeItems(base, over any) []any {
	var out []any
	idx := map[any]int{}
	for _, list := range []any{base, over} {
		items, _ := list.([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			key := m["key"]
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

func (s *Server) insertTemplate(q *request) (any, error) {
	ctx := q.ctx()
	t, err := s.planTemplate(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	region := q.call.Region
	if _, err := s.gce.GetInstanceTemplate(ctx, s.opts.HostProject, region, t.Name); err == nil {
		name, _ := q.n.tenantName(t.Name)
		return nil, s.refuse(q, errorf(http.StatusConflict, ReasonAlreadyExists, "The resource '%s' already exists", q.templatePath(region, name)))
	} else if !gceapi.IsNotFound(err) {
		return nil, s.refuse(q, err)
	}
	// Charge, in the transaction that checks, before GCE is called.
	object := templateObject(region, t.Name)
	err = s.st.Tx(ctx, func(tx store.Tx) error {
		err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: object, Silo: q.target.ID, Via: q.via(), Amounts: templatesCharge})
		return quotaMessage(q, err)
	})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.InsertInstanceTemplate(ctx, s.opts.HostProject, region, t)
	if err != nil {
		rctx := context.WithoutCancel(ctx)
		_ = s.st.Tx(rctx, func(tx store.Tx) error {
			_, err := ledger.Refund(rctx, tx, object, nil)
			return err
		})
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, q.chargeLines(templatesCharge))
	return s.started(q, op, object, nil)
}

func (s *Server) getTemplate(q *request) (any, error) {
	t, err := s.ownTemplate(q)
	if err != nil {
		return nil, err
	}
	return q.template(t)
}

func (s *Server) listTemplates(q *request) (any, error) {
	l, err := s.gce.ListInstanceTemplates(q.ctx(), s.opts.HostProject, q.call.Region, gceapi.ListFilter{NamePrefix: q.n.prefix()})
	if err != nil {
		return nil, err
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	var items []map[string]any
	for _, t := range l {
		if !q.ownsTemplate(t) {
			continue
		}
		m, err := q.template(t)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#instanceTemplateList", items)
}

func (s *Server) deleteTemplate(q *request) (any, error) {
	t, err := s.ownTemplate(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.DeleteInstanceTemplate(q.ctx(), s.opts.HostProject, q.call.Region, t.Name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	// The charge is given back once the delete has worked.
	return s.startedWith(q, op, Operation{Object: templateObject(q.call.Region, t.Name), Refund: noRefund, OnSuccess: []ledger.Amount{}})
}
