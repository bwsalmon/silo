package gce

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	gceapi "github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/ledger/address"
	"github.com/bwsalmon/silo/pkg/store"
	compute "google.golang.org/api/compute/v1"
)

// Subnetworks (gcp-backend.md, "Subnetworks", SV-71). The facade's
// network is default, a fiction over the silo's own space; a tenant's
// subnet is the real <id>-<name> in the shared VPC, described
// silo:<id>. Its ranges are the silo's own ranges in the space map
// (allocators.md, "Checks the facade makes", SV-44), recorded in the
// transaction that charges subnetworks, before GCE is called.

func init() {
	for m, h := range map[string]handler{
		"subnetworks.insert":            (*Server).insertSubnetwork,
		"subnetworks.get":               (*Server).getSubnetwork,
		"subnetworks.list":              (*Server).listSubnetworks,
		"subnetworks.aggregatedList":    (*Server).aggregatedSubnetworks,
		"subnetworks.delete":            (*Server).deleteSubnetwork,
		"subnetworks.patch":             (*Server).patchSubnetwork,
		"subnetworks.expandIpCidrRange": (*Server).expandSubnetwork,
	} {
		handlers[m] = h
	}
	// A failed insert, patch or expand puts the space map back as GCE
	// left the subnet; a delete frees its ranges and refunds once it
	// has worked.
	for _, m := range []string{"insert", "patch", "expandIpCidrRange"} {
		opDone["compute.subnetworks."+m] = func(s *Server, ctx context.Context, rec Operation, failed bool) error {
			if !failed {
				return nil
			}
			return s.resyncSubnet(ctx, rec)
		}
	}
	opDone["compute.subnetworks.delete"] = func(s *Server, ctx context.Context, rec Operation, failed bool) error {
		if failed {
			return nil
		}
		region, name, ok := subnetOfObject(rec)
		if !ok {
			return nil
		}
		return s.st.Tx(ctx, func(tx store.Tx) error {
			if err := removeSubnetOwn(ctx, tx, rec.Silo, region, name); err != nil {
				return err
			}
			_, err := ledger.Refund(ctx, tx, rec.Object, nil)
			return err
		})
	}
}

// subnetsCharge is what a subnet charges: one of the global count.
var subnetsCharge = []ledger.Amount{{Key: ledger.Key{Kind: "gce", Class: "subnetworks", Region: ledger.Global}, Amount: ledger.Units(1)}}

// subnetObject is the ledger's object for a real subnet: what it is
// charged to, and what its operations record.
func subnetObject(region, real string) string { return "subnetwork:" + region + "/" + real }

// subnetOfObject is the region and tenant name of the subnet rec's
// object names.
func subnetOfObject(rec Operation) (region, name string, ok bool) {
	rest, ok := strings.CutPrefix(rec.Object, "subnetwork:")
	if !ok {
		return "", "", false
	}
	region, real, ok := strings.Cut(rest, "/")
	if !ok {
		return "", "", false
	}
	name, ok = strings.CutPrefix(real, rec.Silo+"-")
	return region, name, ok
}

// ownObject is the space map's name for a subnet's range: the subnet's
// own name for its primary range, and the secondary range's name after
// it for a secondary. The address checks' errors name it.
func ownObject(region, name, rangeName string) string {
	o := "subnetworks/" + region + "/" + name
	if rangeName != "" {
		o += "/" + rangeName
	}
	return o
}

// subnetOwn is a subnet's ranges, as own ranges for the space map.
func subnetOwn(region, name, primary string, secondary []*compute.SubnetworkSecondaryRange) ([]address.OwnRange, error) {
	p, err := parseCIDR("resource.ipCidrRange", primary)
	if err != nil {
		return nil, err
	}
	out := []address.OwnRange{{Object: ownObject(region, name, ""), Range: p, Region: region}}
	seen := map[string]bool{}
	for i, r := range secondary {
		field := "resource.secondaryIpRanges[" + itoa(i) + "]"
		if r == nil || r.RangeName == "" {
			return nil, invalid("Invalid value for field '%s.rangeName': ''. Must be a match of regex '[a-z]([-a-z0-9]*[a-z0-9])?'", field)
		}
		if seen[r.RangeName] {
			return nil, invalid("Invalid value for field '%s.rangeName': '%s'. Range names must be unique within a subnetwork", field, r.RangeName)
		}
		seen[r.RangeName] = true
		p, err := parseCIDR(field+".ipCidrRange", r.IpCidrRange)
		if err != nil {
			return nil, err
		}
		out = append(out, address.OwnRange{Object: ownObject(region, name, r.RangeName), Range: p, Region: region})
	}
	return out, nil
}

func parseCIDR(field, s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, invalid("Invalid value for field '%s': '%s'. Must be a valid IPv4 CIDR range.", field, s)
	}
	return p, nil
}

// subnetOwnObjects are the space map's objects for one of silo's
// subnets: its primary range and every secondary.
func subnetOwnObjects(ctx context.Context, tx store.Tx, silo, region, name string) ([]string, error) {
	m, err := address.Map(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	primary := ownObject(region, name, "")
	var out []string
	for _, r := range m {
		if r.State == address.StateOwn && (r.Holder == primary || strings.HasPrefix(r.Holder, primary+"/")) {
			out = append(out, r.Holder)
		}
	}
	return out, nil
}

// removeSubnetOwn frees all of a subnet's ranges.
func removeSubnetOwn(ctx context.Context, tx store.Tx, silo, region, name string) error {
	objs, err := subnetOwnObjects(ctx, tx, silo, region, name)
	if err != nil || len(objs) == 0 {
		return err
	}
	return address.RemoveOwn(ctx, tx, silo, objs...)
}

// setSubnetOwn makes a subnet's ranges in the space map exactly ranges,
// after the address checks.
func setSubnetOwn(ctx context.Context, tx store.Tx, silo, region, name string, ranges []address.OwnRange) error {
	objs, err := subnetOwnObjects(ctx, tx, silo, region, name)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, r := range ranges {
		keep[r.Object] = true
	}
	var drop []string
	for _, o := range objs {
		if !keep[o] {
			drop = append(drop, o)
		}
	}
	if len(drop) > 0 {
		if err := address.RemoveOwn(ctx, tx, silo, drop...); err != nil {
			return err
		}
	}
	return address.SetOwn(ctx, tx, silo, ranges)
}

// resyncSubnet puts the space map back as GCE has rec's subnet, after
// an operation on it failed: its ranges if it exists, none if not.
func (s *Server) resyncSubnet(ctx context.Context, rec Operation) error {
	region, name, ok := subnetOfObject(rec)
	if !ok {
		return nil
	}
	sn, err := s.gce.GetSubnetwork(ctx, s.opts.HostProject, region, rec.Silo+"-"+name)
	if err != nil && !gceapi.IsNotFound(err) {
		return err
	}
	return s.st.Tx(ctx, func(tx store.Tx) error {
		if sn == nil {
			return removeSubnetOwn(ctx, tx, rec.Silo, region, name)
		}
		ranges, err := subnetOwn(region, name, sn.IpCidrRange, sn.SecondaryIpRanges)
		if err != nil {
			return err
		}
		return setSubnetOwn(ctx, tx, rec.Silo, region, name, ranges)
	})
}

// decode reads a request body into a GCE object.
func decode(body map[string]any, into any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return invalid("Invalid JSON payload received. %v", err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		return invalid("Invalid JSON payload received. %v", err)
	}
	return nil
}

// describe is the real object's description: silo:<id>, then the
// tenant's (gcp-backend.md, "Layout").
func (q *request) describe(d string) string {
	if d == "" {
		return "silo:" + q.target.ID
	}
	return "silo:" + q.target.ID + " " + d
}

// tenantDescription is the tenant's description of a real object, and
// whether the object is described as the silo's.
func (q *request) tenantDescription(d string) (string, bool) {
	tag := "silo:" + q.target.ID
	if d == tag {
		return "", true
	}
	rest, ok := strings.CutPrefix(d, tag+" ")
	return rest, ok
}

// networkLink is the shared VPC's link in the host project.
func (s *Server) networkLink() string {
	return gceapi.SelfLinkBase + "projects/" + s.opts.HostProject + "/global/networks/" + s.opts.Network
}

// checkSubnetFields checks the fields a subnet body may carry only one
// value of: its network, region, stack type and purpose.
func (q *request) checkSubnetFields(in *compute.Subnetwork) error {
	if in.Network != "" {
		if _, err := q.n.inLink(in.Network, "networks"); err != nil {
			return err
		}
	}
	if in.Region != "" && gceapi.LastSegment(in.Region) != q.call.Region {
		return invalid("Invalid value for field 'resource.region': '%s'. It must be the region of the request, %s", in.Region, q.call.Region)
	}
	if in.StackType != "" && in.StackType != "IPV4_ONLY" {
		return forbidden("silo: field stackType may only be IPV4_ONLY")
	}
	if in.Purpose != "" && in.Purpose != "PRIVATE" {
		return forbidden("silo: field purpose may only be PRIVATE")
	}
	return nil
}

// ownSubnet is the silo's real subnet the call names, or GCE's 404.
func (s *Server) ownSubnet(q *request) (*compute.Subnetwork, error) {
	nf := notFound("projects/" + q.call.Project + "/regions/" + q.call.Region + "/subnetworks/" + q.call.Name)
	if err := q.n.checkName(q.call.Name); err != nil {
		return nil, err
	}
	sn, err := s.gce.GetSubnetwork(q.ctx(), s.opts.HostProject, q.call.Region, q.call.Name)
	if err != nil {
		if gceapi.IsNotFound(err) {
			return nil, nf
		}
		return nil, err
	}
	if !q.ownsSubnet(sn) {
		return nil, nf
	}
	return sn, nil
}

func (q *request) ownsSubnet(sn *compute.Subnetwork) bool {
	named := q.n.ours(sn.Name)
	_, described := q.tenantDescription(sn.Description)
	return named && described && gceapi.LastSegment(sn.Network) == q.n.network
}

// subnet is sn as the tenant sees it.
func (q *request) subnet(sn *compute.Subnetwork) (map[string]any, error) {
	c := *sn
	c.Description, _ = q.tenantDescription(sn.Description)
	return q.shape("compute#subnetwork", &c)
}

func (s *Server) insertSubnetwork(q *request) (any, error) {
	ctx := q.ctx()
	var in compute.Subnetwork
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	if err := q.n.checkName(in.Name); err != nil {
		return nil, s.refuse(q, err)
	}
	if err := q.checkSubnetFields(&in); err != nil {
		return nil, s.refuse(q, err)
	}
	region := q.call.Region
	ranges, err := subnetOwn(region, q.n.key(in.Name), in.IpCidrRange, in.SecondaryIpRanges)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	real := in.Name
	object := subnetObject(region, real)
	err = s.st.Tx(ctx, func(tx store.Tx) error {
		if _, err := address.Own(ctx, tx, q.target.ID, ownObject(region, q.n.key(in.Name), "")); err == nil {
			return errorf(http.StatusConflict, ReasonAlreadyExists, "The resource 'projects/%s/regions/%s/subnetworks/%s' already exists", q.call.Project, region, in.Name)
		} else if ledger.CodeOf(err) != ledger.NotFound {
			return err
		}
		if err := setSubnetOwn(ctx, tx, q.target.ID, region, in.Name, ranges); err != nil {
			return err
		}
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: object, Silo: q.target.ID, Via: q.via(), Amounts: subnetsCharge})
	})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.InsertSubnetwork(ctx, s.opts.HostProject, region, &compute.Subnetwork{
		Name: real, Description: q.describe(in.Description), Network: s.networkLink(),
		IpCidrRange: in.IpCidrRange, SecondaryIpRanges: in.SecondaryIpRanges, LogConfig: in.LogConfig,
		StackType: "IPV4_ONLY", Purpose: "PRIVATE",
	})
	if err != nil {
		_ = s.st.Tx(ctx, func(tx store.Tx) error {
			if err := removeSubnetOwn(ctx, tx, q.target.ID, region, q.n.key(in.Name)); err != nil {
				return err
			}
			_, err := ledger.Refund(ctx, tx, object, nil)
			return err
		})
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", map[string]any{"ranges": rangeStrings(ranges)}, q.chargeLines(subnetsCharge))
	return s.started(q, op, object, nil)
}

func rangeStrings(rs []address.OwnRange) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Range.String()
	}
	return out
}

// chargeLines are amounts as the audit records a charge.
func (q *request) chargeLines(amounts []ledger.Amount) []fenceapi.ChargeLine {
	var out []fenceapi.ChargeLine
	for _, a := range amounts {
		out = append(out, fenceapi.ChargeLine{Payer: q.target.ID, Home: q.target.ID, Via: q.via(),
			Kind: a.Kind, Class: a.Class, Region: a.Region, Amount: a.Amount / ledger.Micro})
	}
	return out
}

func (s *Server) getSubnetwork(q *request) (any, error) {
	sn, err := s.ownSubnet(q)
	if err != nil {
		return nil, err
	}
	return q.subnet(sn)
}

// ownSubnets are the silo's subnets in region, by name.
func (s *Server) ownSubnets(q *request, region string) ([]*compute.Subnetwork, error) {
	l, err := s.gce.ListSubnetworks(q.ctx(), s.opts.HostProject, region, gceapi.ListFilter{NamePrefix: q.n.prefix()})
	if err != nil && !gceapi.IsNotFound(err) {
		return nil, err
	}
	var out []*compute.Subnetwork
	for _, sn := range l {
		if q.ownsSubnet(sn) {
			out = append(out, sn)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) listSubnetworks(q *request) (any, error) {
	l, err := s.ownSubnets(q, q.call.Region)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	for _, sn := range l {
		m, err := q.subnet(sn)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#subnetworkList", items)
}

func (s *Server) aggregatedSubnetworks(q *request) (any, error) {
	regions := make([]string, 0, len(q.regions))
	for r := range q.regions {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	var items []scoped
	for _, r := range regions {
		l, err := s.ownSubnets(q, r)
		if err != nil {
			return nil, err
		}
		for _, sn := range l {
			m, err := q.subnet(sn)
			if err != nil {
				return nil, err
			}
			items = append(items, scoped{scope: "regions/" + r, item: m})
		}
	}
	return s.aggregated(q, "compute#subnetworkAggregatedList", "subnetworks", items)
}

func (s *Server) deleteSubnetwork(q *request) (any, error) {
	sn, err := s.ownSubnet(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.DeleteSubnetwork(q.ctx(), s.opts.HostProject, q.call.Region, sn.Name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, subnetObject(q.call.Region, sn.Name), noRefund)
}

// patchSubnetwork changes a subnet's secondary ranges, after the same
// address checks as at insert, its logConfig or its description.
func (s *Server) patchSubnetwork(q *request) (any, error) {
	ctx := q.ctx()
	var in compute.Subnetwork
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	if err := q.checkSubnetFields(&in); err != nil {
		return nil, s.refuse(q, err)
	}
	if in.Name != "" && in.Name != q.call.Name {
		return nil, s.refuse(q, invalid("Invalid value for field 'resource.name': '%s'. A subnetwork's name can't be changed", in.Name))
	}
	cur, err := s.ownSubnet(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	if in.IpCidrRange != "" && in.IpCidrRange != cur.IpCidrRange {
		return nil, s.refuse(q, invalid("Invalid value for field 'resource.ipCidrRange': '%s'. Use expandIpCidrRange to change a subnetwork's range", in.IpCidrRange))
	}
	region, name := q.call.Region, q.n.key(q.call.Name)
	patch := &compute.Subnetwork{Fingerprint: in.Fingerprint, LogConfig: in.LogConfig}
	if _, ok := q.body["description"]; ok {
		patch.Description = q.describe(in.Description)
	}
	var ranges []address.OwnRange
	if _, ok := q.body["secondaryIpRanges"]; ok {
		if ranges, err = subnetOwn(region, name, cur.IpCidrRange, in.SecondaryIpRanges); err != nil {
			return nil, s.refuse(q, err)
		}
		patch.SecondaryIpRanges = in.SecondaryIpRanges
		patch.ForceSendFields = []string{"SecondaryIpRanges"}
		if err := s.st.Tx(ctx, func(tx store.Tx) error {
			return setSubnetOwn(ctx, tx, q.target.ID, region, name, ranges)
		}); err != nil {
			return nil, s.refuse(q, err)
		}
	}
	op, err := s.gce.PatchSubnetwork(ctx, s.opts.HostProject, region, cur.Name, patch)
	if err != nil {
		if ranges != nil {
			_ = s.restoreSubnet(ctx, q.target.ID, region, name, cur)
		}
		return nil, s.refuse(q, err)
	}
	extra := map[string]any{}
	if ranges != nil {
		extra["ranges"] = rangeStrings(ranges)
	}
	s.audit(q, "OK", extra, nil)
	return s.started(q, op, subnetObject(region, cur.Name), noRefund)
}

// restoreSubnet puts the space map back to cur's ranges, after GCE
// refused a change to it.
func (s *Server) restoreSubnet(ctx context.Context, silo, region, name string, cur *compute.Subnetwork) error {
	ranges, err := subnetOwn(region, name, cur.IpCidrRange, cur.SecondaryIpRanges)
	if err != nil {
		return err
	}
	return s.st.Tx(ctx, func(tx store.Tx) error { return setSubnetOwn(ctx, tx, silo, region, name, ranges) })
}

func (s *Server) expandSubnetwork(q *request) (any, error) {
	ctx := q.ctx()
	to, _ := q.body["ipCidrRange"].(string)
	p, err := parseCIDR("ipCidrRange", to)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	cur, err := s.ownSubnet(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	region, name := q.call.Region, q.n.key(q.call.Name)
	err = s.st.Tx(ctx, func(tx store.Tx) error {
		return address.Expand(ctx, tx, q.target.ID, ownObject(region, name, ""), p)
	})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.ExpandIpCidrRange(ctx, s.opts.HostProject, region, cur.Name, to)
	if err != nil {
		_ = s.restoreSubnet(ctx, q.target.ID, region, name, cur)
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", map[string]any{"ranges": []string{p.String()}}, nil)
	return s.started(q, op, subnetObject(region, cur.Name), noRefund)
}
