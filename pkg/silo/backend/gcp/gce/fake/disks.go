package fake

import (
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

// newDisk checks and fills in d as a new disk in zone, without storing
// it: its type, source, size (GCE's default where none is given), and
// provisioned performance (GCE's defaults likewise).
func (f *Fake) newDisk(p *state, zone string, d *compute.Disk, field string) (*compute.Disk, error) {
	d = clone(d)
	if err := checkName(field+".name", d.Name); err != nil {
		return nil, err
	}
	scope := zoneScope(zone)
	link := p.link(scope, "disks", d.Name)
	if p.disks.get(scope, d.Name) != nil {
		return nil, alreadyExists(link)
	}
	if err := checkLabels(field+".labels", d.Labels); err != nil {
		return nil, err
	}
	typ := "pd-standard"
	if d.Type != "" {
		r := gce.ParseRef(d.Type)
		if r.Zone != "" && r.Zone != zone {
			return nil, invalid(field+".type", "Disk type %s is in zone %s, not %s", r.Name, r.Zone, zone)
		}
		typ = r.Name
	}
	k := diskKinds[typ]
	if k == nil {
		return nil, invalid(field+".type", "Disk type '%s' does not exist in zone '%s'", typ, zone)
	}
	var srcGB int64
	switch {
	case d.SourceImage != "":
		img, err := f.resolveImage(p, d.SourceImage)
		if err != nil {
			return nil, invalid(field+".sourceImage", "%s", err.Error())
		}
		d.SourceImage, d.SourceImageId, srcGB = img.SelfLink, strconv.FormatUint(img.Id, 10), img.DiskSizeGb
	case d.SourceSnapshot != "":
		r := gce.ParseRef(d.SourceSnapshot)
		s := p.snapshots.get("global", r.Name)
		if s == nil || p.pending[s.SelfLink] {
			return nil, invalid(field+".sourceSnapshot", "The resource 'projects/%s/global/snapshots/%s' was not found", p.name, r.Name)
		}
		d.SourceSnapshot, srcGB = s.SelfLink, s.DiskSizeGb
	}
	if d.SizeGb == 0 {
		d.SizeGb = max(srcGB, k.defaultGB)
		if srcGB > 0 {
			d.SizeGb = max(srcGB, k.minGB)
		}
	}
	if d.SizeGb < srcGB {
		return nil, invalid(field+".sizeGb", "Requested disk size cannot be smaller than the image size (%d GB)", srcGB)
	}
	if d.SizeGb < k.minGB || d.SizeGb > k.maxGB {
		return nil, invalid(field+".sizeGb", "Disk size %d GB is out of range %s for %s", d.SizeGb, k.validSize(), typ)
	}
	if err := k.performance(field, d.SizeGb, &d.ProvisionedIops, &d.ProvisionedThroughput); err != nil {
		return nil, err
	}
	if d.AccessMode != "" {
		if !k.accessMode {
			return nil, invalid(field+".accessMode", "Disk type %s does not support accessMode", typ)
		}
		if !slices.Contains([]string{"READ_WRITE_SINGLE", "READ_WRITE_MANY", "READ_ONLY_MANY"}, d.AccessMode) {
			return nil, invalid(field+".accessMode", "%s", d.AccessMode)
		}
	} else if k.accessMode {
		d.AccessMode = "READ_WRITE_SINGLE"
	}
	d.Kind = "compute#disk"
	d.Id = f.nextID()
	d.SelfLink = link
	d.Zone = gce.SelfLinkBase + "projects/" + p.name + "/" + scope
	d.Type = p.link(scope, "diskTypes", typ)
	d.Status = "CREATING"
	d.CreationTimestamp = f.timestamp()
	d.LabelFingerprint = f.fingerprint()
	d.PhysicalBlockSizeBytes = 4096
	d.Users = nil
	return d, nil
}

// resolveImage finds an image by URL or path, including
// projects/<p>/global/images/family/<family>.
func (f *Fake) resolveImage(p *state, ref string) (*compute.Image, error) {
	r := gce.ParseRef(ref)
	proj := p
	if r.Project != "" {
		proj = f.projects[r.Project]
		if proj == nil {
			return nil, notFound(ref)
		}
	}
	if r.Collection == "images" && r.Name == "family" {
		fam := gce.LastSegment(ref)
		if img := latestInFamily(proj, fam); img != nil {
			return img, nil
		}
		return nil, notFound("projects/" + proj.name + "/global/images/family/" + fam)
	}
	img := proj.images.get("global", r.Name)
	if img == nil || proj.pending[img.SelfLink] {
		return nil, notFound(proj.link("global", "images", r.Name))
	}
	return img, nil
}

func latestInFamily(p *state, family string) *compute.Image {
	var best *compute.Image
	for _, img := range p.images.list("global") {
		if img.Family != family || p.pending[img.SelfLink] {
			continue
		}
		if img.Deprecated != nil && img.Deprecated.State != "" && img.Deprecated.State != "ACTIVE" {
			continue
		}
		if best == nil || img.CreationTimestamp > best.CreationTimestamp ||
			(img.CreationTimestamp == best.CreationTimestamp && img.Name > best.Name) {
			best = img
		}
	}
	return best
}

func (f *Fake) disk(p *state, zone, name string) (*compute.Disk, error) {
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	d := p.disks.get(zoneScope(zone), name)
	if d == nil {
		return nil, notFound(p.link(zoneScope(zone), "disks", name))
	}
	return d, nil
}

// insertPending stores nothing itself: it starts the operation for an
// object put already and marked pending, which finishing makes real
// (under the quota check) and failing removes.
func (f *Fake) insertPending(p *state, method, link string, id uint64, ready func(), remove func()) *compute.Operation {
	p.pending[link] = true
	commit := func() *opErr {
		if !p.pending[link] {
			return nil
		}
		return f.withQuota(func() func() {
			delete(p.pending, link)
			ready()
			return func() { p.pending[link] = true }
		})
	}
	abort := func() {
		delete(p.pending, link)
		remove()
	}
	return f.start(p, method, link, id, commit, abort)
}

// InsertDisk implements gce.Client.
func (f *Fake) InsertDisk(ctx context.Context, project, zone string, body *compute.Disk) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	d, err := f.newDisk(p, zone, body, "resource")
	if err != nil {
		return nil, err
	}
	scope := zoneScope(zone)
	p.disks.put(scope, d.Name, d)
	return f.insertPending(p, "disks.insert", d.SelfLink, d.Id,
		func() { d.Status = "READY" },
		func() {
			if p.disks.get(scope, d.Name) == d {
				p.disks.del(scope, d.Name)
			}
		}), nil
}

// GetDisk implements gce.Client.
func (f *Fake) GetDisk(ctx context.Context, project, zone, name string) (*compute.Disk, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	d, err := f.disk(p, zone, name)
	if err != nil {
		return nil, err
	}
	return clone(d), nil
}

// ListDisks implements gce.Client.
func (f *Fake) ListDisks(ctx context.Context, project, zone string, lf gce.ListFilter) ([]*compute.Disk, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.zoneExists(zone); err != nil {
		return nil, err
	}
	return filtered(p.disks.list(zoneScope(zone)), lf, func(d *compute.Disk) (string, map[string]string) { return d.Name, d.Labels }), nil
}

// diskOp is instanceOp for disks.
func (f *Fake) diskOp(ctx context.Context, project, zone, name, method string, quota bool, check func(p *state, d *compute.Disk) error, apply func(p *state, d *compute.Disk) func()) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	d, err := f.disk(p, zone, name)
	if err != nil {
		return nil, err
	}
	if p.pending[d.SelfLink] && method != "disks.delete" {
		return nil, gce.NewError(http.StatusBadRequest, gce.ReasonNotReady, "The resource '%s' is not ready", trimBase(d.SelfLink))
	}
	if check != nil {
		if err := check(p, d); err != nil {
			return nil, err
		}
	}
	commit := func() *opErr {
		if p.disks.get(zoneScope(zone), name) != d {
			return &opErr{"RESOURCE_NOT_FOUND", http.StatusNotFound, "The resource '" + trimBase(d.SelfLink) + "' was not found"}
		}
		if !quota {
			apply(p, d)
			return nil
		}
		return f.withQuota(func() func() { return apply(p, d) })
	}
	return f.start(p, method, d.SelfLink, d.Id, commit, nil), nil
}

// DeleteDisk implements gce.Client. A disk in use can't be deleted.
func (f *Fake) DeleteDisk(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return f.diskOp(ctx, project, zone, name, "disks.delete", false,
		func(_ *state, d *compute.Disk) error {
			if len(d.Users) > 0 {
				return inUse("The disk resource '%s' is already being used by '%s'", trimBase(d.SelfLink), trimBase(d.Users[0]))
			}
			return nil
		},
		func(p *state, d *compute.Disk) func() {
			p.disks.del(zoneScope(zone), name)
			delete(p.pending, d.SelfLink)
			return nil
		})
}

// ResizeDisk implements gce.Client. A disk only grows. A Hyperdisk's
// provisioned performance is raised to the new size's minimum.
func (f *Fake) ResizeDisk(ctx context.Context, project, zone, name string, sizeGb int64) (*compute.Operation, error) {
	return f.diskOp(ctx, project, zone, name, "disks.resize", true,
		func(_ *state, d *compute.Disk) error {
			k := diskKinds[gce.LastSegment(d.Type)]
			if sizeGb <= d.SizeGb {
				return invalid("sizeGb", "New disk size '%d' GB must be larger than existing size '%d' GB", sizeGb, d.SizeGb)
			}
			if sizeGb > k.maxGB {
				return invalid("sizeGb", "Disk size %d GB is out of range %s", sizeGb, k.validSize())
			}
			return nil
		},
		func(_ *state, d *compute.Disk) func() {
			old := *d
			k := diskKinds[gce.LastSegment(d.Type)]
			d.SizeGb = sizeGb
			k.raiseToMinimum(sizeGb, &d.ProvisionedIops, &d.ProvisionedThroughput)
			return func() {
				d.SizeGb, d.ProvisionedIops, d.ProvisionedThroughput = old.SizeGb, old.ProvisionedIops, old.ProvisionedThroughput
			}
		})
}

// UpdateDisk implements gce.Client: disks.update of provisionedIops,
// provisionedThroughput and accessMode, named in paths.
func (f *Fake) UpdateDisk(ctx context.Context, project, zone, name string, body *compute.Disk, paths []string) (*compute.Operation, error) {
	var iops, tp int64
	var mode string
	return f.diskOp(ctx, project, zone, name, "disks.update", true,
		func(_ *state, d *compute.Disk) error {
			k := diskKinds[gce.LastSegment(d.Type)]
			iops, tp, mode = d.ProvisionedIops, d.ProvisionedThroughput, d.AccessMode
			if len(paths) == 0 {
				return invalid("paths", "At least one path must be given")
			}
			for _, path := range paths {
				switch path {
				case "provisionedIops":
					if !k.iops {
						return invalid("resource.provisionedIops", "Disk type %s does not support provisioned IOPS", k.name)
					}
					iops = body.ProvisionedIops
				case "provisionedThroughput":
					if !k.throughput {
						return invalid("resource.provisionedThroughput", "Disk type %s does not support provisioned throughput", k.name)
					}
					tp = body.ProvisionedThroughput
				case "accessMode":
					if !k.accessMode {
						return invalid("resource.accessMode", "Disk type %s does not support accessMode", k.name)
					}
					if len(d.Users) > 0 {
						return inUse("The access mode of a disk in use can't be changed")
					}
					mode = body.AccessMode
				default:
					return invalid("paths", "Field '%s' can't be updated", path)
				}
			}
			if k.iops && iops == 0 || k.throughput && tp == 0 {
				return invalid("resource", "Provisioned performance can't be 0")
			}
			return k.performance("resource", d.SizeGb, &iops, &tp)
		},
		func(_ *state, d *compute.Disk) func() {
			old := *d
			d.ProvisionedIops, d.ProvisionedThroughput, d.AccessMode = iops, tp, mode
			return func() {
				d.ProvisionedIops, d.ProvisionedThroughput, d.AccessMode = old.ProvisionedIops, old.ProvisionedThroughput, old.AccessMode
			}
		})
}

// SetDiskLabels implements gce.Client. The fingerprint must match.
func (f *Fake) SetDiskLabels(ctx context.Context, project, zone, name string, req *compute.ZoneSetLabelsRequest) (*compute.Operation, error) {
	labels, fp := req.Labels, req.LabelFingerprint
	return f.diskOp(ctx, project, zone, name, "disks.setLabels", false,
		func(_ *state, d *compute.Disk) error {
			if fp != d.LabelFingerprint {
				return conditionNotMet("Labels fingerprint either invalid or resource labels have changed")
			}
			return checkLabels("labels", labels)
		},
		func(_ *state, d *compute.Disk) func() {
			d.Labels, d.LabelFingerprint = labels, f.fingerprint()
			return nil
		})
}

// CreateSnapshot implements gce.Client. The operation is the disk's
// zone's; the snapshot is global.
func (f *Fake) CreateSnapshot(ctx context.Context, project, zone, disk string, body *compute.Snapshot) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	d, err := f.disk(p, zone, disk)
	if err != nil {
		return nil, err
	}
	if p.pending[d.SelfLink] {
		return nil, gce.NewError(http.StatusBadRequest, gce.ReasonNotReady, "The resource '%s' is not ready", trimBase(d.SelfLink))
	}
	s := clone(body)
	if err := checkName("resource.name", s.Name); err != nil {
		return nil, err
	}
	link := p.link("global", "snapshots", s.Name)
	if p.snapshots.get("global", s.Name) != nil {
		return nil, alreadyExists(link)
	}
	if err := checkLabels("resource.labels", s.Labels); err != nil {
		return nil, err
	}
	s.Kind = "compute#snapshot"
	s.Id = f.nextID()
	s.SelfLink = link
	s.SourceDisk = d.SelfLink
	s.SourceDiskId = strconv.FormatUint(d.Id, 10)
	s.DiskSizeGb = d.SizeGb
	s.StorageBytes = d.SizeGb << 28
	s.Status = "CREATING"
	s.CreationTimestamp = f.timestamp()
	s.LabelFingerprint = f.fingerprint()
	p.snapshots.put("global", s.Name, s)
	p.pending[link] = true
	commit := func() *opErr {
		if !p.pending[link] {
			return nil
		}
		return f.withQuota(func() func() {
			delete(p.pending, link)
			s.Status = "READY"
			return func() { p.pending[link] = true }
		})
	}
	abort := func() {
		delete(p.pending, link)
		if p.snapshots.get("global", s.Name) == s {
			p.snapshots.del("global", s.Name)
		}
	}
	return f.start(p, "disks.createSnapshot", d.SelfLink, d.Id, commit, abort), nil
}

// InsertImage implements gce.Client: an image from exactly one of a
// disk, an image or a snapshot.
func (f *Fake) InsertImage(ctx context.Context, project string, body *compute.Image) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	img := clone(body)
	if err := checkName("resource.name", img.Name); err != nil {
		return nil, err
	}
	link := p.link("global", "images", img.Name)
	if p.images.get("global", img.Name) != nil {
		return nil, alreadyExists(link)
	}
	if err := checkLabels("resource.labels", img.Labels); err != nil {
		return nil, err
	}
	n := 0
	for _, s := range []string{img.SourceDisk, img.SourceImage, img.SourceSnapshot} {
		if s != "" {
			n++
		}
	}
	if img.RawDisk != nil {
		n++
	}
	if n != 1 {
		return nil, invalid("resource", "Exactly one of sourceDisk, sourceImage, sourceSnapshot and rawDisk must be given")
	}
	switch {
	case img.SourceDisk != "":
		r := gce.ParseRef(img.SourceDisk)
		d := p.disks.get(zoneScope(r.Zone), r.Name)
		if d == nil || p.pending[d.SelfLink] {
			return nil, invalid("resource.sourceDisk", "The resource '%s' was not found", img.SourceDisk)
		}
		img.SourceDisk, img.DiskSizeGb = d.SelfLink, d.SizeGb
	case img.SourceImage != "":
		src, err := f.resolveImage(p, img.SourceImage)
		if err != nil {
			return nil, invalid("resource.sourceImage", "The resource '%s' was not found", img.SourceImage)
		}
		img.SourceImage, img.DiskSizeGb = src.SelfLink, src.DiskSizeGb
	case img.SourceSnapshot != "":
		s := p.snapshots.get("global", gce.ParseRef(img.SourceSnapshot).Name)
		if s == nil || p.pending[s.SelfLink] {
			return nil, invalid("resource.sourceSnapshot", "The resource '%s' was not found", img.SourceSnapshot)
		}
		img.SourceSnapshot, img.DiskSizeGb = s.SelfLink, s.DiskSizeGb
	default:
		if img.DiskSizeGb == 0 {
			img.DiskSizeGb = 10
		}
	}
	img.Kind = "compute#image"
	img.Id = f.nextID()
	img.SelfLink = link
	img.Status = "PENDING"
	img.ArchiveSizeBytes = img.DiskSizeGb << 28
	img.CreationTimestamp = f.timestamp()
	img.LabelFingerprint = f.fingerprint()
	p.images.put("global", img.Name, img)
	return f.insertPending(p, "images.insert", link, img.Id,
		func() { img.Status = "READY" },
		func() {
			if p.images.get("global", img.Name) == img {
				p.images.del("global", img.Name)
			}
		}), nil
}

func (f *Fake) image(p *state, name string) (*compute.Image, error) {
	img := p.images.get("global", name)
	if img == nil {
		return nil, notFound(p.link("global", "images", name))
	}
	return img, nil
}

// GetImage implements gce.Client.
func (f *Fake) GetImage(ctx context.Context, project, name string) (*compute.Image, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	img, err := f.image(p, name)
	if err != nil {
		return nil, err
	}
	return clone(img), nil
}

// GetImageFromFamily implements gce.Client: the newest image of family
// that isn't deprecated.
func (f *Fake) GetImageFromFamily(ctx context.Context, project, family string) (*compute.Image, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	img := latestInFamily(p, family)
	if img == nil {
		return nil, notFound("projects/" + p.name + "/global/images/family/" + family)
	}
	return clone(img), nil
}

// ListImages implements gce.Client.
func (f *Fake) ListImages(ctx context.Context, project string, lf gce.ListFilter) ([]*compute.Image, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	return filtered(p.images.list("global"), lf, func(i *compute.Image) (string, map[string]string) { return i.Name, i.Labels }), nil
}

// globalOp checks the object get finds, then starts method's operation.
func (f *Fake) globalOp(ctx context.Context, project, method string, get func(p *state) (link string, id uint64, err error), apply func(p *state)) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	link, id, err := get(p)
	if err != nil {
		return nil, err
	}
	return f.start(p, method, link, id, func() *opErr { apply(p); return nil }, nil), nil
}

// DeleteImage implements gce.Client.
func (f *Fake) DeleteImage(ctx context.Context, project, name string) (*compute.Operation, error) {
	return f.globalOp(ctx, project, "images.delete",
		func(p *state) (string, uint64, error) {
			img, err := f.image(p, name)
			if err != nil {
				return "", 0, err
			}
			return img.SelfLink, img.Id, nil
		},
		func(p *state) {
			if img := p.images.get("global", name); img != nil {
				delete(p.pending, img.SelfLink)
				p.images.del("global", name)
			}
		})
}

// SetImageLabels implements gce.Client.
func (f *Fake) SetImageLabels(ctx context.Context, project, name string, req *compute.GlobalSetLabelsRequest) (*compute.Operation, error) {
	return f.globalOp(ctx, project, "images.setLabels",
		func(p *state) (string, uint64, error) {
			img, err := f.image(p, name)
			if err != nil {
				return "", 0, err
			}
			if req.LabelFingerprint != img.LabelFingerprint {
				return "", 0, conditionNotMet("Labels fingerprint either invalid or resource labels have changed")
			}
			return img.SelfLink, img.Id, checkLabels("labels", req.Labels)
		},
		func(p *state) {
			if img := p.images.get("global", name); img != nil {
				img.Labels, img.LabelFingerprint = req.Labels, f.fingerprint()
			}
		})
}

// DeprecateImage implements gce.Client.
func (f *Fake) DeprecateImage(ctx context.Context, project, name string, status *compute.DeprecationStatus) (*compute.Operation, error) {
	status = clone(status)
	return f.globalOp(ctx, project, "images.deprecate",
		func(p *state) (string, uint64, error) {
			img, err := f.image(p, name)
			if err != nil {
				return "", 0, err
			}
			if !slices.Contains([]string{"", "ACTIVE", "DEPRECATED", "OBSOLETE", "DELETED"}, status.State) {
				return "", 0, invalid("resource.state", "%s", status.State)
			}
			return img.SelfLink, img.Id, nil
		},
		func(p *state) {
			if img := p.images.get("global", name); img != nil {
				img.Deprecated = status
				if status.State == "" || status.State == "ACTIVE" {
					img.Deprecated = nil
				}
			}
		})
}

func (f *Fake) snapshot(p *state, name string) (*compute.Snapshot, error) {
	s := p.snapshots.get("global", name)
	if s == nil {
		return nil, notFound(p.link("global", "snapshots", name))
	}
	return s, nil
}

// GetSnapshot implements gce.Client.
func (f *Fake) GetSnapshot(ctx context.Context, project, name string) (*compute.Snapshot, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	s, err := f.snapshot(p, name)
	if err != nil {
		return nil, err
	}
	return clone(s), nil
}

// ListSnapshots implements gce.Client.
func (f *Fake) ListSnapshots(ctx context.Context, project string, lf gce.ListFilter) ([]*compute.Snapshot, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	return filtered(p.snapshots.list("global"), lf, func(s *compute.Snapshot) (string, map[string]string) { return s.Name, s.Labels }), nil
}

// DeleteSnapshot implements gce.Client.
func (f *Fake) DeleteSnapshot(ctx context.Context, project, name string) (*compute.Operation, error) {
	return f.globalOp(ctx, project, "snapshots.delete",
		func(p *state) (string, uint64, error) {
			s, err := f.snapshot(p, name)
			if err != nil {
				return "", 0, err
			}
			return s.SelfLink, s.Id, nil
		},
		func(p *state) {
			if s := p.snapshots.get("global", name); s != nil {
				delete(p.pending, s.SelfLink)
				p.snapshots.del("global", name)
			}
		})
}

// SetSnapshotLabels implements gce.Client.
func (f *Fake) SetSnapshotLabels(ctx context.Context, project, name string, req *compute.GlobalSetLabelsRequest) (*compute.Operation, error) {
	return f.globalOp(ctx, project, "snapshots.setLabels",
		func(p *state) (string, uint64, error) {
			s, err := f.snapshot(p, name)
			if err != nil {
				return "", 0, err
			}
			if req.LabelFingerprint != s.LabelFingerprint {
				return "", 0, conditionNotMet("Labels fingerprint either invalid or resource labels have changed")
			}
			return s.SelfLink, s.Id, checkLabels("labels", req.Labels)
		},
		func(p *state) {
			if s := p.snapshots.get("global", name); s != nil {
				s.Labels, s.LabelFingerprint = req.Labels, f.fingerprint()
			}
		})
}
