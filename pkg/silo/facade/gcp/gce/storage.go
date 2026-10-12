package gce

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	gceapi "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

// Disks, images and snapshots (gcp-backend.md, "Covered" and "Disks",
// SV-64 and SV-69). A tenant's disk is the real <id>-<name>, labelled
// silo=<id> and silo-managed=true; an image or a snapshot likewise, in
// the host project's global collections. Each charges the price table's
// classes for its resource (allocators.md, "Classes" and "Global
// counts") before GCE is called, refunded if the operation fails and
// once a delete succeeds.

func init() {
	for m, h := range map[string]handler{
		"disks.insert":         (*Server).insertDisk,
		"disks.get":            (*Server).getDisk,
		"disks.list":           (*Server).listDisks,
		"disks.aggregatedList": (*Server).aggregatedDisks,
		"disks.delete":         (*Server).deleteDisk,
		"disks.resize":         (*Server).resizeDisk,
		"disks.update":         (*Server).updateDisk,
		"disks.setLabels":      (*Server).setDiskLabels,
		"disks.createSnapshot": (*Server).createSnapshot,
		"images.insert":        (*Server).insertImage,
		"images.delete":        (*Server).deleteImage,
		"images.setLabels":     (*Server).setImageLabels,
		"images.deprecate":     (*Server).deprecateImage,
		"snapshots.get":        (*Server).getSnapshot,
		"snapshots.list":       (*Server).listSnapshots,
		"snapshots.delete":     (*Server).deleteSnapshot,
		"snapshots.setLabels":  (*Server).setSnapshotLabels,
	} {
		handlers[m] = h
	}
	pricers["disks.insert"] = billPricer(func(s *Server, q *request) (bill, error) {
		p, err := s.planDisk(q)
		return p.bill, err
	})
	pricers["disks.resize"] = billPricer(func(s *Server, q *request) (bill, error) {
		b, _, _, err := s.planResize(q)
		return b, err
	})
	pricers["disks.update"] = func(s *Server, q *request, tx store.Tx) ([]ledger.Amount, error) {
		p, err := s.planUpdate(q)
		if err != nil {
			return nil, err
		}
		up, _, err := delta(q.ctx(), tx, p.old, p.new)
		return up, err
	}
	pricers["disks.createSnapshot"] = billPricer(func(s *Server, q *request) (bill, error) {
		p, err := s.planSnapshot(q)
		return p.bill, err
	})
	pricers["images.insert"] = billPricer(func(s *Server, q *request) (bill, error) {
		p, err := s.planImage(q)
		return p.bill, err
	})
	// An image or a snapshot is charged its source's size, the most its
	// archive can be, until it exists; then the charge comes down to
	// what GCE reports.
	opDone["compute.images.insert"] = func(s *Server, ctx context.Context, rec Operation, failed bool) error {
		if failed {
			return nil
		}
		img, err := s.gce.GetImage(ctx, s.opts.HostProject, strings.TrimPrefix(rec.Object, "image:"))
		if err != nil || img.ArchiveSizeBytes == 0 {
			return ignoreNotFound(err)
		}
		return s.trueUp(ctx, priceStored(resImage, rec.Object, img.ArchiveSizeBytes))
	}
	opDone["compute.disks.createSnapshot"] = func(s *Server, ctx context.Context, rec Operation, failed bool) error {
		if failed {
			return nil
		}
		snap, err := s.gce.GetSnapshot(ctx, s.opts.HostProject, strings.TrimPrefix(rec.Object, "snapshot:"))
		if err != nil || snap.StorageBytes == 0 {
			return ignoreNotFound(err)
		}
		return s.trueUp(ctx, priceStored(resSnapshot, rec.Object, snap.StorageBytes))
	}
}

func ignoreNotFound(err error) error {
	if gceapi.IsNotFound(err) {
		return nil
	}
	return err
}

// trueUp refunds what p's object holds beyond p's own amounts.
func (s *Server) trueUp(ctx context.Context, p priced) error {
	return s.st.Tx(ctx, func(tx store.Tx) error {
		want, err := p.amounts(ctx, tx)
		if err != nil {
			return err
		}
		held, err := ledger.Charges(ctx, tx, p.object)
		if err != nil {
			return err
		}
		var over []ledger.Amount
		for _, h := range held {
			w := int64(0)
			for _, a := range want {
				if a.Key == h.Key {
					w += a.Amount
				}
			}
			if h.Amount > w {
				over = append(over, ledger.Amount{Key: h.Key, Amount: h.Amount - w})
			}
		}
		if len(over) == 0 {
			return nil
		}
		_, err = ledger.Refund(ctx, tx, p.object, over)
		return err
	})
}

// orNoRefund is amounts as an operation's Refund: what it charged, or
// noRefund if it charged nothing.
func orNoRefund(amounts []ledger.Amount) []ledger.Amount {
	if len(amounts) == 0 {
		return noRefund
	}
	return amounts
}

// siloLabels are labels with the facade's own added.
func (q *request) siloLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+2)
	for k, v := range labels {
		out[k] = v
	}
	out["silo"] = q.target.ID
	out["silo-managed"] = "true"
	return out
}

// ---- references ----

// ref resolves a reference the tenant wrote to an object of coll, bare,
// partial or a full URL, into the host project's (Naming, SV-76). A
// reference to another silo's project is 403; to another collection,
// 400.
func (q *request) ref(s, coll, field string) (gceapi.Ref, error) {
	link, err := q.n.inLink(s, coll)
	if err != nil {
		return gceapi.Ref{}, err
	}
	r := gceapi.ParseRef(link)
	if r.Collection != "" && r.Collection != coll {
		return r, invalid("Invalid value for field '%s': '%s'. It must name a resource of type %s", field, s, coll)
	}
	return r, nil
}

func (q *request) zonalPath(zone, coll, name string) string {
	return "projects/" + q.call.Project + "/zones/" + zone + "/" + coll + "/" + name
}

func (q *request) globalPath(coll, name string) string {
	return "projects/" + q.call.Project + "/global/" + coll + "/" + name
}

// ownDisk is the silo's disk real in zone; another silo's, or none, is
// 404.
func (s *Server) ownDisk(q *request, zone, real string) (*compute.Disk, error) {
	nf := notFound(q.zonalPath(zone, "disks", real))
	d, err := s.gce.GetDisk(q.ctx(), s.opts.HostProject, zone, real)
	if err != nil {
		if gceapi.IsNotFound(err) {
			return nil, nf
		}
		return nil, err
	}
	if !q.owns(d.Name, d.Labels) {
		return nil, nf
	}
	return d, nil
}

// callDisk is the disk the call names.
func (s *Server) callDisk(q *request) (*compute.Disk, error) {
	if err := q.n.checkName(q.call.Name); err != nil {
		return nil, err
	}
	return s.ownDisk(q, q.call.Zone, q.call.Name)
}

// ownImage is the silo's image real.
func (s *Server) ownImage(q *request, real string) (*compute.Image, error) {
	nf := notFound(q.globalPath("images", real))
	img, err := s.gce.GetImage(q.ctx(), s.opts.HostProject, real)
	if err != nil {
		if gceapi.IsNotFound(err) {
			return nil, nf
		}
		return nil, err
	}
	if !q.owns(img.Name, img.Labels) {
		return nil, nf
	}
	return img, nil
}

// ownSnapshot is the silo's snapshot real.
func (s *Server) ownSnapshot(q *request, real string) (*compute.Snapshot, error) {
	nf := notFound(q.globalPath("snapshots", real))
	snap, err := s.gce.GetSnapshot(q.ctx(), s.opts.HostProject, real)
	if err != nil {
		if gceapi.IsNotFound(err) {
			return nil, nf
		}
		return nil, err
	}
	if !q.owns(snap.Name, snap.Labels) {
		return nil, nf
	}
	return snap, nil
}

// sourceImage is an image a new disk or image may be made from: one of
// the silo's own, or a public image project's, by name or family.
func (s *Server) sourceImage(q *request, ref, field string) (*compute.Image, error) {
	link, err := q.n.inLink(ref, "images")
	if err != nil {
		return nil, err
	}
	r := gceapi.ParseRef(link)
	if r.Collection != "" && r.Collection != "images" {
		return nil, invalid("Invalid value for field '%s': '%s'. It must name an image", field, ref)
	}
	family := ""
	if i := strings.Index(link, "images/family/"); i >= 0 {
		family = link[i+len("images/family/"):]
	}
	if r.Project != "" && r.Project != s.opts.HostProject {
		// A public image project: inLink refused any other.
		if family != "" {
			return s.gce.GetImageFromFamily(q.ctx(), r.Project, family)
		}
		return s.gce.GetImage(q.ctx(), r.Project, r.Name)
	}
	if family != "" {
		l, err := s.ownImages(q)
		if err != nil {
			return nil, err
		}
		var best *compute.Image
		for _, img := range l {
			if img.Family == family && (img.Deprecated == nil || img.Deprecated.State == "" || img.Deprecated.State == "ACTIVE") &&
				(best == nil || img.CreationTimestamp > best.CreationTimestamp) {
				best = img
			}
		}
		if best == nil {
			return nil, notFound(q.globalPath("images", "family/"+family))
		}
		return best, nil
	}
	return s.ownImage(q, r.Name)
}

// sourceSnapshot is one of the silo's own snapshots.
func (s *Server) sourceSnapshot(q *request, ref, field string) (*compute.Snapshot, error) {
	r, err := q.ref(ref, "snapshots", field)
	if err != nil {
		return nil, err
	}
	return s.ownSnapshot(q, r.Name)
}

// sourceDiskIn is one of the silo's own disks, which must be in zone
// if zone is given.
func (s *Server) sourceDiskIn(q *request, ref, zone, field string) (*compute.Disk, error) {
	r, err := q.ref(ref, "disks", field)
	if err != nil {
		return nil, err
	}
	switch {
	case r.Zone == "" && zone == "":
		return nil, invalid("Invalid value for field '%s': '%s'. The disk must be named with its zone", field, ref)
	case r.Zone == "":
		r.Zone = zone
	case zone != "" && r.Zone != zone:
		return nil, invalid("Invalid value for field '%s': '%s'. silo: the disk must be in zone %s", field, ref, zone)
	}
	if !q.regions[gceapi.ZoneRegion(r.Zone)] {
		return nil, regionNotAllowed(q.target.Path, "zone "+r.Zone)
	}
	return s.ownDisk(q, r.Zone, r.Name)
}

// diskTypeIn is a disk type reference's name, which must be in zone.
func diskTypeIn(ref, zone, field string) (string, error) {
	typ := diskTypeOf(ref)
	if r := gceapi.ParseRef(ref); r.Zone != "" && r.Zone != zone {
		return "", invalid("Invalid value for field '%s': '%s'. Disk type must be in zone %s", field, ref, zone)
	}
	return typ, nil
}

func (s *Server) diskTypeLink(zone, typ string) string {
	return gceapi.SelfLinkBase + "projects/" + s.opts.HostProject + "/zones/" + zone + "/diskTypes/" + typ
}

// newDiskSource checks a new disk's source image or snapshot, if any,
// and returns them with their real links.
func (s *Server) newDiskSource(q *request, image, snapshot, field string) (*compute.Image, *compute.Snapshot, error) {
	var img *compute.Image
	var snap *compute.Snapshot
	var err error
	if image != "" {
		if img, err = s.sourceImage(q, image, field+"sourceImage"); err != nil {
			return nil, nil, err
		}
	}
	if snapshot != "" {
		if snap, err = s.sourceSnapshot(q, snapshot, field+"sourceSnapshot"); err != nil {
			return nil, nil, err
		}
	}
	return img, snap, nil
}

// ---- disks ----

func (q *request) disk(d *compute.Disk) (map[string]any, error) { return q.shape("compute#disk", d) }

// diskPlan is a disks.insert, checked and priced.
type diskPlan struct {
	bill bill
	real *compute.Disk
}

func (s *Server) planDisk(q *request) (diskPlan, error) {
	var in compute.Disk
	if err := decode(q.body, &in); err != nil {
		return diskPlan{}, err
	}
	if err := q.n.checkName(in.Name); err != nil {
		return diskPlan{}, err
	}
	zone := q.call.Zone
	typ, err := diskTypeIn(in.Type, zone, "resource.type")
	if err != nil {
		return diskPlan{}, err
	}
	image := in.SourceImage
	if v := q.r.URL.Query().Get("sourceImage"); v != "" {
		image = v
	}
	img, snap, err := s.newDiskSource(q, image, in.SourceSnapshot, "resource.")
	if err != nil {
		return diskPlan{}, err
	}
	size, err := s.diskSize(q.ctx(), zone, typ, in.SizeGb, img, snap)
	if err != nil {
		return diskPlan{}, err
	}
	iops, throughput, err := s.performance(typ, size, in.ProvisionedIops, in.ProvisionedThroughput, "resource.")
	if err != nil {
		return diskPlan{}, err
	}
	if in.AccessMode != "" && !s.opts.Compat.DiskTypes[typ].AccessMode {
		return diskPlan{}, invalid("Invalid value for field 'resource.accessMode': '%s'. Disk type %s does not support accessMode", in.AccessMode, typ)
	}
	// GCE fills in the performance the tenant didn't give; the disk is
	// charged its default, and shows what GCE set.
	real := &compute.Disk{Name: in.Name, Description: in.Description, SizeGb: size,
		Type: s.diskTypeLink(zone, typ), Labels: q.siloLabels(in.Labels),
		ProvisionedIops: in.ProvisionedIops, ProvisionedThroughput: in.ProvisionedThroughput, AccessMode: in.AccessMode}
	if img != nil {
		real.SourceImage = img.SelfLink
	}
	if snap != nil {
		real.SourceSnapshot = snap.SelfLink
	}
	return diskPlan{bill: bill{priceDisk(zone, real.Name, typ, size, iops, throughput, "type")}, real: real}, nil
}

func (s *Server) insertDisk(q *request) (any, error) {
	p, err := s.planDisk(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	object := p.bill[0].object
	lines, err := s.charge(q, p.bill, object)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.InsertDisk(q.ctx(), s.opts.HostProject, q.call.Zone, p.real)
	if err != nil {
		s.uncharge(q, p.bill, lines)
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, q.chargeLines(all(lines)))
	return s.started(q, op, object, nil)
}

func (s *Server) getDisk(q *request) (any, error) {
	d, err := s.callDisk(q)
	if err != nil {
		return nil, err
	}
	return q.disk(d)
}

// ownDisks are the silo's disks in zone, by name.
func (s *Server) ownDisks(q *request, zone string) ([]*compute.Disk, error) {
	l, err := s.gce.ListDisks(q.ctx(), s.opts.HostProject, zone, gceapi.ListFilter{Labels: map[string]string{"silo": q.target.ID}, NamePrefix: q.n.prefix()})
	if err != nil && !gceapi.IsNotFound(err) {
		return nil, err
	}
	var out []*compute.Disk
	for _, d := range l {
		if q.owns(d.Name, d.Labels) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) listDisks(q *request) (any, error) {
	l, err := s.ownDisks(q, q.call.Zone)
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	for _, d := range l {
		m, err := q.disk(d)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#diskList", items)
}

func (s *Server) aggregatedDisks(q *request) (any, error) {
	zs, err := s.allowedZones(q)
	if err != nil {
		return nil, err
	}
	var items []scoped
	for _, z := range zs {
		l, err := s.ownDisks(q, z.Name)
		if err != nil {
			return nil, err
		}
		for _, d := range l {
			m, err := q.disk(d)
			if err != nil {
				return nil, err
			}
			items = append(items, scoped{scope: "zones/" + z.Name, item: m})
		}
	}
	return s.aggregated(q, "compute#diskAggregatedList", "disks", items)
}

func (s *Server) deleteDisk(q *request) (any, error) {
	d, err := s.callDisk(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.DeleteDisk(q.ctx(), s.opts.HostProject, q.call.Zone, d.Name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.startedWith(q, op, Operation{Object: diskObject(q.call.Zone, d.Name), Refund: noRefund, OnSuccess: []ledger.Amount{}})
}

// planResize prices a resize: the GB it adds, to the type's class, and
// for a type with provisioned performance, whatever GCE raises its IOPS
// and throughput by to the new size's minimum. GCE refuses a disk
// getting smaller itself.
func (s *Server) planResize(q *request) (bill, *compute.Disk, int64, error) {
	var in compute.DisksResizeRequest
	if err := decode(q.body, &in); err != nil {
		return nil, nil, 0, err
	}
	d, err := s.callDisk(q)
	if err != nil {
		return nil, nil, 0, err
	}
	var b bill
	if grow := in.SizeGb - d.SizeGb; grow > 0 {
		typ := diskTypeOf(d.Type)
		dt := s.opts.Compat.DiskTypes[typ]
		var iops, throughput int64
		if dt.IOPS != nil {
			iops = max(0, dt.IOPS.Minimum(in.SizeGb)-d.ProvisionedIops)
		}
		if dt.Throughput != nil {
			throughput = max(0, dt.Throughput.Minimum(in.SizeGb)-d.ProvisionedThroughput)
		}
		b = bill{priceDisk(q.call.Zone, d.Name, typ, grow, iops, throughput, "type")}
	}
	return b, d, in.SizeGb, nil
}

func (s *Server) resizeDisk(q *request) (any, error) {
	b, d, size, err := s.planResize(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	lines, err := s.charge(q, b)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.ResizeDisk(q.ctx(), s.opts.HostProject, q.call.Zone, d.Name, size)
	if err != nil {
		s.uncharge(q, b, lines)
		return nil, s.refuse(q, err)
	}
	charged := all(lines)
	s.audit(q, "OK", nil, q.chargeLines(charged))
	return s.started(q, op, diskObject(q.call.Zone, d.Name), orNoRefund(charged))
}

// updatePaths are the fields disks.update may change (gcp-backend.md,
// "Disks"), each allowed on the types that have it.
var updatePaths = map[string]bool{"provisionedIops": true, "provisionedThroughput": true, "accessMode": true}

// updatePlan is a disks.update: the disk priced as it is and as it
// would be, and the request GCE gets.
type updatePlan struct {
	disk     *compute.Disk
	body     *compute.Disk
	paths    []string
	old, new priced
}

// planUpdate checks a disks.update. Its paths name the fields to change,
// repeated or comma-separated.
func (s *Server) planUpdate(q *request) (updatePlan, error) {
	var in compute.Disk
	if err := decode(q.body, &in); err != nil {
		return updatePlan{}, err
	}
	d, err := s.callDisk(q)
	if err != nil {
		return updatePlan{}, err
	}
	if in.Name != "" && in.Name != q.call.Name {
		return updatePlan{}, invalid("Invalid value for field 'resource.name': '%s'. A disk's name can't be changed", in.Name)
	}
	var paths []string
	for _, v := range q.r.URL.Query()["paths"] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) == 0 {
		return updatePlan{}, invalid("Required parameter 'paths' is missing: name the fields to update")
	}
	typ := diskTypeOf(d.Type)
	dt := s.opts.Compat.DiskTypes[typ]
	iops, throughput := d.ProvisionedIops, d.ProvisionedThroughput
	body := &compute.Disk{Name: d.Name}
	for _, p := range paths {
		if !updatePaths[p] {
			return updatePlan{}, forbidden("silo: field %s is not allowed", p)
		}
		switch p {
		case "provisionedIops":
			if dt.IOPS == nil {
				return updatePlan{}, invalid("Invalid value for field 'resource.provisionedIops': Disk type %s does not support provisioned IOPS", typ)
			}
			if in.ProvisionedIops <= 0 {
				return updatePlan{}, invalid("Invalid value for field 'resource.provisionedIops': '%d'", in.ProvisionedIops)
			}
			iops, body.ProvisionedIops = in.ProvisionedIops, in.ProvisionedIops
		case "provisionedThroughput":
			if dt.Throughput == nil {
				return updatePlan{}, invalid("Invalid value for field 'resource.provisionedThroughput': Disk type %s does not support provisioned throughput", typ)
			}
			if in.ProvisionedThroughput <= 0 {
				return updatePlan{}, invalid("Invalid value for field 'resource.provisionedThroughput': '%d'", in.ProvisionedThroughput)
			}
			throughput, body.ProvisionedThroughput = in.ProvisionedThroughput, in.ProvisionedThroughput
		case "accessMode":
			if !dt.AccessMode {
				return updatePlan{}, invalid("Invalid value for field 'resource.accessMode': Disk type %s does not support accessMode", typ)
			}
			body.AccessMode = in.AccessMode
		}
	}
	return updatePlan{disk: d, body: body, paths: paths,
		old: priceDisk(q.call.Zone, d.Name, typ, d.SizeGb, d.ProvisionedIops, d.ProvisionedThroughput, "type"),
		new: priceDisk(q.call.Zone, d.Name, typ, d.SizeGb, iops, throughput, "type")}, nil
}

// updateDisk charges an increase in IOPS or throughput before GCE is
// called, and gives back a decrease once the operation has worked
// (allocators.md, "Charging and refunds"). GCE's own limit on how often
// a disk's performance may change is passed through.
func (s *Server) updateDisk(q *request) (any, error) {
	ctx := q.ctx()
	p, err := s.planUpdate(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	object := p.old.object
	up, down, err := s.rechargeObject(q, object, func(tx store.Tx) ([]ledger.Amount, []ledger.Amount, error) {
		return delta(ctx, tx, p.old, p.new)
	})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.UpdateDisk(ctx, s.opts.HostProject, q.call.Zone, p.disk.Name, p.body, p.paths)
	if err != nil {
		s.refundObject(q, object, up)
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, q.chargeLines(up))
	rec := Operation{Object: object, Refund: orNoRefund(up)}
	if len(down) > 0 {
		rec.OnSuccess = down
	}
	return s.startedWith(q, op, rec)
}

func (s *Server) setDiskLabels(q *request) (any, error) {
	var in compute.ZoneSetLabelsRequest
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	d, err := s.callDisk(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.SetDiskLabels(q.ctx(), s.opts.HostProject, q.call.Zone, d.Name,
		&compute.ZoneSetLabelsRequest{Labels: q.siloLabels(in.Labels), LabelFingerprint: in.LabelFingerprint})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, diskObject(q.call.Zone, d.Name), noRefund)
}

// ---- storage locations (call 25) ----

// multiRegions are the multi-regions an image or a snapshot may be
// stored in, by the prefix of the regions each contains.
var multiRegions = map[string]string{"us-": "us", "europe-": "eu", "asia-": "asia"}

func multiRegionOf(region string) string {
	for p, m := range multiRegions {
		if strings.HasPrefix(region, p) {
			return m
		}
	}
	return ""
}

// storageLocations checks an image's or a snapshot's storageLocations:
// each an allowed region, or the multi-region that contains one. None
// given is near, the region the source is in if it is allowed, and the
// silo's first allowed region if not, so GCE never picks one outside.
func (q *request) storageLocations(locs []string, near string) ([]string, error) {
	if len(locs) == 0 {
		if !q.regions[near] {
			rs := q.sortedRegions()
			if len(rs) == 0 {
				return nil, regionNotAllowed(q.target.Path, "every region")
			}
			near = rs[0]
		}
		return []string{near}, nil
	}
	for i, l := range locs {
		ok := q.regions[l]
		for r := range q.regions {
			if multiRegionOf(r) == l {
				ok = true
			}
		}
		if !ok {
			return nil, regionNotAllowed(q.target.Path, fmt.Sprintf("storageLocations[%d] %s", i, l))
		}
	}
	return locs, nil
}

// ---- snapshots ----

func (q *request) snapshot(sn *compute.Snapshot) (map[string]any, error) {
	return q.shape("compute#snapshot", sn)
}

type snapshotPlan struct {
	bill bill
	disk *compute.Disk
	real *compute.Snapshot
}

// planSnapshot prices a createSnapshot at the disk's size, the most
// its storage can be, until GCE reports the real size.
func (s *Server) planSnapshot(q *request) (snapshotPlan, error) {
	var in compute.Snapshot
	if err := decode(q.body, &in); err != nil {
		return snapshotPlan{}, err
	}
	if err := q.n.checkName(in.Name); err != nil {
		return snapshotPlan{}, err
	}
	d, err := s.callDisk(q)
	if err != nil {
		return snapshotPlan{}, err
	}
	locs, err := q.storageLocations(in.StorageLocations, gceapi.ZoneRegion(q.call.Zone))
	if err != nil {
		return snapshotPlan{}, err
	}
	real := &compute.Snapshot{Name: in.Name, Description: in.Description, Labels: q.siloLabels(in.Labels), StorageLocations: locs}
	return snapshotPlan{bill: bill{priceStored(resSnapshot, snapshotObject(real.Name), d.SizeGb<<30)}, disk: d, real: real}, nil
}

func (s *Server) createSnapshot(q *request) (any, error) {
	p, err := s.planSnapshot(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	object := p.bill[0].object
	lines, err := s.charge(q, p.bill, object)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.CreateSnapshot(q.ctx(), s.opts.HostProject, q.call.Zone, p.disk.Name, p.real)
	if err != nil {
		s.uncharge(q, p.bill, lines)
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", map[string]any{"snapshot": strings.TrimPrefix(p.real.Name, q.n.prefix())}, q.chargeLines(all(lines)))
	return s.started(q, op, object, nil)
}

func (s *Server) callSnapshot(q *request) (*compute.Snapshot, error) {
	if err := q.n.checkName(q.call.Name); err != nil {
		return nil, err
	}
	return s.ownSnapshot(q, q.call.Name)
}

func (s *Server) getSnapshot(q *request) (any, error) {
	sn, err := s.callSnapshot(q)
	if err != nil {
		return nil, err
	}
	return q.snapshot(sn)
}

func (s *Server) listSnapshots(q *request) (any, error) {
	l, err := s.gce.ListSnapshots(q.ctx(), s.opts.HostProject, gceapi.ListFilter{Labels: map[string]string{"silo": q.target.ID}, NamePrefix: q.n.prefix()})
	if err != nil {
		return nil, err
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	var items []map[string]any
	for _, sn := range l {
		if !q.owns(sn.Name, sn.Labels) {
			continue
		}
		m, err := q.snapshot(sn)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#snapshotList", items)
}

func (s *Server) deleteSnapshot(q *request) (any, error) {
	sn, err := s.callSnapshot(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.DeleteSnapshot(q.ctx(), s.opts.HostProject, sn.Name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.startedWith(q, op, Operation{Object: snapshotObject(sn.Name), Refund: noRefund, OnSuccess: []ledger.Amount{}})
}

func (s *Server) setSnapshotLabels(q *request) (any, error) {
	var in compute.GlobalSetLabelsRequest
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	sn, err := s.callSnapshot(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.SetSnapshotLabels(q.ctx(), s.opts.HostProject, sn.Name,
		&compute.GlobalSetLabelsRequest{Labels: q.siloLabels(in.Labels), LabelFingerprint: in.LabelFingerprint})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, snapshotObject(sn.Name), noRefund)
}

// ---- images ----

type imagePlan struct {
	bill bill
	real *compute.Image
}

// planImage checks an image's source, the silo's own disk, image or
// snapshot or a public image (never rawDisk, which the allowlist
// refuses), and prices it at the source's size until GCE reports the
// archive's.
func (s *Server) planImage(q *request) (imagePlan, error) {
	var in compute.Image
	if err := decode(q.body, &in); err != nil {
		return imagePlan{}, err
	}
	if err := q.n.checkName(in.Name); err != nil {
		return imagePlan{}, err
	}
	n := 0
	for _, src := range []string{in.SourceDisk, in.SourceImage, in.SourceSnapshot} {
		if src != "" {
			n++
		}
	}
	if n != 1 {
		return imagePlan{}, invalid("Invalid value for field 'resource': silo: an image needs exactly one of sourceDisk, sourceImage and sourceSnapshot")
	}
	real := &compute.Image{Name: in.Name, Description: in.Description, Family: in.Family,
		Labels: q.siloLabels(in.Labels), GuestOsFeatures: in.GuestOsFeatures, Architecture: in.Architecture}
	var bytes int64
	near := ""
	switch {
	case in.SourceDisk != "":
		d, err := s.sourceDiskIn(q, in.SourceDisk, "", "resource.sourceDisk")
		if err != nil {
			return imagePlan{}, err
		}
		real.SourceDisk, bytes, near = d.SelfLink, d.SizeGb<<30, gceapi.ZoneRegion(gceapi.LastSegment(d.Zone))
	case in.SourceImage != "":
		img, err := s.sourceImage(q, in.SourceImage, "resource.sourceImage")
		if err != nil {
			return imagePlan{}, err
		}
		real.SourceImage, bytes = img.SelfLink, img.ArchiveSizeBytes
		if bytes == 0 {
			bytes = img.DiskSizeGb << 30
		}
	default:
		sn, err := s.sourceSnapshot(q, in.SourceSnapshot, "resource.sourceSnapshot")
		if err != nil {
			return imagePlan{}, err
		}
		real.SourceSnapshot, bytes = sn.SelfLink, sn.StorageBytes
		if bytes == 0 {
			bytes = sn.DiskSizeGb << 30
		}
	}
	locs, err := q.storageLocations(in.StorageLocations, near)
	if err != nil {
		return imagePlan{}, err
	}
	real.StorageLocations = locs
	return imagePlan{bill: bill{priceStored(resImage, imageObject(real.Name), bytes)}, real: real}, nil
}

func (s *Server) insertImage(q *request) (any, error) {
	p, err := s.planImage(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	object := p.bill[0].object
	lines, err := s.charge(q, p.bill, object)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.InsertImage(q.ctx(), s.opts.HostProject, p.real)
	if err != nil {
		s.uncharge(q, p.bill, lines)
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, q.chargeLines(all(lines)))
	return s.started(q, op, object, nil)
}

func (s *Server) callImage(q *request) (*compute.Image, error) {
	if err := q.n.checkName(q.call.Name); err != nil {
		return nil, err
	}
	return s.ownImage(q, q.call.Name)
}

func (s *Server) deleteImage(q *request) (any, error) {
	img, err := s.callImage(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.DeleteImage(q.ctx(), s.opts.HostProject, img.Name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.startedWith(q, op, Operation{Object: imageObject(img.Name), Refund: noRefund, OnSuccess: []ledger.Amount{}})
}

func (s *Server) setImageLabels(q *request) (any, error) {
	var in compute.GlobalSetLabelsRequest
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	img, err := s.callImage(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.SetImageLabels(q.ctx(), s.opts.HostProject, img.Name,
		&compute.GlobalSetLabelsRequest{Labels: q.siloLabels(in.Labels), LabelFingerprint: in.LabelFingerprint})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, imageObject(img.Name), noRefund)
}

func (s *Server) deprecateImage(q *request) (any, error) {
	var in compute.DeprecationStatus
	if err := decode(q.body, &in); err != nil {
		return nil, s.refuse(q, err)
	}
	img, err := s.callImage(q)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	if in.Replacement != "" {
		rep, err := s.sourceImage(q, in.Replacement, "replacement")
		if err != nil {
			return nil, s.refuse(q, err)
		}
		in.Replacement = rep.SelfLink
	}
	op, err := s.gce.DeprecateImage(q.ctx(), s.opts.HostProject, img.Name, &in)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, imageObject(img.Name), noRefund)
}

// queryBool is a query parameter as a bool, def if it isn't given.
func (q *request) queryBool(name string, def bool) (bool, error) {
	v := q.r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, invalid("Invalid value for parameter '%s': '%s'", name, v)
	}
	return b, nil
}
