package gce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	gceapi "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
	compute "google.golang.org/api/compute/v1"
)

// Pricing (allocators.md, "Classes", "Charging and refunds" and
// "Preview", SV-34 and SV-35). What an object charges is in two halves:
// its measures, the quantities read from the request and from GCE
// (a machine type's guestCpus, a disk's sizeGb), and the price table,
// ledger.Prices, which says which class each measure of each resource
// and variant charges. Nothing here names a class: a new disk type, or
// an instance's GPUs and local SSD, are rows in that table, and a new
// kind of quantity is a new measure.

// The resources of the price table.
const (
	resInstance = "instance"
	resDisk     = "disk"
	resImage    = "image"
	resSnapshot = "snapshot"
)

// The measures the facade reads.
const (
	measureCount     = "count"
	measureGuestCPUs = "guest_cpus"
	measureMemoryMB  = "memory_mb"
	measureSizeGB    = "size_gb"
	measureStorageGB = "storage_gb"
	// An instance's GPUs, attached and bundled, all types; each type's
	// own are ledger.GPUMeasure(type).
	measureGPUs = "gpus"
	// An instance's local SSD, attached and bundled.
	measureLocalSSDGB = "local_ssd_gb"
	// A disk's provisioned IOPS and throughput.
	measureIOPS       = "iops"
	measureThroughput = "throughput"
)

// The variants of an instance.
const (
	variantStandard = "STANDARD"
	variantSpot     = "SPOT"
)

// statusTerminated is GCE's status of a stopped instance, which holds
// no vCPU or memory (call 6).
const statusTerminated = "TERMINATED"

// priced is one object's charge before it is looked up: what it is, its
// variant, where, and its measures.
type priced struct {
	object   string
	resource string
	variant  string
	region   string // the object's region; global classes ignore it
	// stopped leaves out the charges held only while running, and
	// runningOnly all the others.
	stopped     bool
	runningOnly bool
	measures    map[string]int64
	// field is the request field that chose the variant, which a
	// variant with no prices is refused by.
	field string
}

// amounts are p's charges, by the price table and the class table.
func (p priced) amounts(ctx context.Context, tx store.Tx) ([]ledger.Amount, error) {
	rows, err := ledger.Prices(ctx, tx, p.resource, p.variant)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, forbidden("silo: %s %s is not allowed", fieldOr(p.field, p.resource), p.variant)
	}
	// A GPU type the price table has no class for can't be charged, so
	// isn't allowed (allocators.md, "GPU classes": the table is silod's
	// config, and grows as root capacity finds new families).
	priced := map[string]bool{}
	for _, r := range rows {
		priced[r.Measure] = true
	}
	for _, m := range sortedKeys(p.measures) {
		if t, ok := strings.CutPrefix(m, ledger.GPUMeasure("")); ok && p.measures[m] > 0 && !priced[m] {
			return nil, forbidden("silo: accelerator type %s has no GPU quota class in silod's config", t)
		}
	}
	var out []ledger.Amount
	for _, r := range rows {
		if (r.Running && p.stopped) || (!r.Running && p.runningOnly) {
			continue
		}
		n := p.measures[r.Measure]
		if n == 0 {
			continue
		}
		c, err := ledger.LookupClass(ctx, tx, "gce", r.Class)
		if err != nil {
			return nil, err
		}
		region := p.region
		if c.Scope == ledger.ScopeGlobal {
			region = ledger.Global
		}
		out = append(out, ledger.Amount{Key: ledger.Key{Kind: "gce", Class: r.Class, Region: region}, Amount: ledger.Units(n)})
	}
	return out, nil
}

func fieldOr(field, def string) string {
	if field != "" {
		return field
	}
	return def
}

// runningClasses are the classes of resource's prices held only while
// an instance runs.
func runningClasses(ctx context.Context, tx store.Tx, resource, variant string) (map[string]bool, error) {
	rows, err := ledger.Prices(ctx, tx, resource, variant)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if r.Running {
			out[r.Class] = true
		}
	}
	return out, nil
}

// bill is what a call charges: one priced object or more, each charged
// to its own object key.
type bill []priced

// lines are b's amounts by object, in b's order.
func (b bill) lines(ctx context.Context, tx store.Tx) ([][]ledger.Amount, error) {
	out := make([][]ledger.Amount, len(b))
	for i, p := range b {
		as, err := p.amounts(ctx, tx)
		if err != nil {
			return nil, err
		}
		out[i] = as
	}
	return out, nil
}

// all is every amount of lines, for the audit and the preview.
func all(lines [][]ledger.Amount) []ledger.Amount {
	var out []ledger.Amount
	for _, l := range lines {
		out = append(out, l...)
	}
	return out
}

// charge charges b to the call's silo in one transaction, after
// checking each new object holds nothing yet (fresh lists the objects
// that must be new). It returns what it charged, by object.
func (s *Server) charge(q *request, b bill, fresh ...string) ([][]ledger.Amount, error) {
	ctx := q.ctx()
	var lines [][]ledger.Amount
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		for _, o := range fresh {
			held, err := ledger.Charges(ctx, tx, o)
			if err != nil {
				return err
			}
			if len(held) > 0 {
				return errorf(http.StatusConflict, ReasonAlreadyExists, "The resource '%s' already exists", q.n.outText(objectPath(q, o)))
			}
		}
		var err error
		if lines, err = b.lines(ctx, tx); err != nil {
			return err
		}
		for i, p := range b {
			if len(lines[i]) == 0 {
				continue
			}
			if err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: p.object, Silo: q.target.ID, Via: q.via(), Amounts: lines[i]}); err != nil {
				return quotaMessage(q, err)
			}
		}
		return nil
	})
	return lines, err
}

// uncharge gives back what charge charged, after GCE refused the call
// itself: a refusal silod didn't predict is passed through, and the
// charge refunded (allocators.md, "Root capacity").
func (s *Server) uncharge(q *request, b bill, lines [][]ledger.Amount) {
	ctx := context.WithoutCancel(q.ctx())
	_ = s.st.Tx(ctx, func(tx store.Tx) error {
		for i, p := range b {
			if len(lines[i]) == 0 {
				continue
			}
			if _, err := ledger.Refund(ctx, tx, p.object, lines[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// quotaMessage is a ledger refusal as GCE words its quota errors, with
// the silo's path (gcp-backend.md, "Errors").
func quotaMessage(q *request, err error) error {
	var le *ledger.Error
	if !errors.As(err, &le) || le.Code != ledger.QuotaExceeded {
		return err
	}
	return errorf(http.StatusForbidden, ReasonQuotaExceeded, "Quota '%s' exceeded. Limit: %s free in region %s (silo %s).",
		le.Key.Class, ledger.FormatAmount(le.Free), le.Key.Region, q.target.Path).domain("usageLimits")
}

// objectPath is an object key as a resource path, for messages.
func objectPath(q *request, object string) string {
	kind, rest, _ := strings.Cut(object, ":")
	scope, name, ok := strings.Cut(rest, "/")
	if !ok {
		name, scope = rest, ""
	}
	coll := map[string]string{"instance": "instances", "disk": "disks", "image": "images", "snapshot": "snapshots"}[kind]
	if scope == "" {
		return "projects/" + q.call.Project + "/global/" + coll + "/" + name
	}
	return "projects/" + q.call.Project + "/zones/" + scope + "/" + coll + "/" + name
}

func instanceObject(zone, real string) string { return "instance:" + zone + "/" + real }
func diskObject(zone, real string) string     { return "disk:" + zone + "/" + real }
func imageObject(real string) string          { return "image:" + real }
func snapshotObject(real string) string       { return "snapshot:" + real }

// ---- what the objects measure ----

// instanceVariant is the variant an instance's scheduling prices it
// as: SPOT for a Spot or preemptible instance.
func instanceVariant(sc *compute.Scheduling) string {
	if sc != nil && (sc.ProvisioningModel == variantSpot || sc.Preemptible) {
		return variantSpot
	}
	return variantStandard
}

// extras are what an instance holds besides its machine type's vCPUs
// and memory: its attached GPUs, by type, and its attached local SSD
// partitions. What its machine type bundles is read from the type.
type extras struct {
	gpus    map[string]int64
	scratch int64
}

// extrasOf are a GCE instance's attached GPUs and local SSD.
func extrasOf(inst *compute.Instance) extras {
	x := extras{gpus: map[string]int64{}}
	for _, a := range inst.GuestAccelerators {
		if a != nil {
			x.gpus[gceapi.LastSegment(a.AcceleratorType)] += a.AcceleratorCount
		}
	}
	for _, d := range inst.Disks {
		if d != nil && d.Type == "SCRATCH" {
			x.scratch++
		}
	}
	return x
}

// priceInstance is what an instance of machine type mt, with x attached,
// charges in zone, running or stopped: its vCPUs and memory, its GPUs
// (gcp-backend.md, "GPUs") and its local SSD ("Local SSD"), attached and
// bundled.
func (s *Server) priceInstance(zone, real string, mt *compute.MachineType, x extras, variant string, stopped bool) priced {
	m := map[string]int64{measureCount: 1, measureGuestCPUs: mt.GuestCpus, measureMemoryMB: mt.MemoryMb}
	gpus := bundledGPUs(mt)
	for t, n := range x.gpus {
		gpus[t] += n
	}
	for t, n := range gpus {
		m[ledger.GPUMeasure(t)] += n
		m[measureGPUs] += n
	}
	// A machine type that bundles local SSD takes no more; GCE lists its
	// own as SCRATCH disks too, so they are counted once.
	partitions := bundledLocalSSDs(mt)
	if partitions == 0 {
		partitions = x.scratch
	}
	m[measureLocalSSDGB] = partitions * s.opts.Compat.LocalSSDPartitionGB
	return priced{object: instanceObject(zone, real), resource: resInstance, variant: variant,
		region: gceapi.ZoneRegion(zone), stopped: stopped, field: "scheduling.provisioningModel", measures: m}
}

// priceDisk is what a disk of type typ, size and provisioned
// performance charges in zone.
func priceDisk(zone, real, typ string, sizeGB, iops, throughput int64, field string) priced {
	return priced{object: diskObject(zone, real), resource: resDisk, variant: typ, region: gceapi.ZoneRegion(zone),
		field: field, measures: map[string]int64{measureSizeGB: sizeGB, measureIOPS: iops, measureThroughput: throughput}}
}

// performance is a new disk's provisioned IOPS and throughput: what the
// tenant gave, or GCE's default for the type and size, which is what
// the disk is charged (gcp-backend.md, "Disks"). A value the type
// doesn't take is refused, as GCE would.
func (s *Server) performance(typ string, sizeGB, iops, throughput int64, field string) (int64, int64, error) {
	dt := s.opts.Compat.DiskTypes[typ]
	if iops != 0 && dt.IOPS == nil {
		return 0, 0, invalid("Invalid value for field '%sprovisionedIops': '%d'. Disk type %s does not support provisioned IOPS", field, iops, typ)
	}
	if throughput != 0 && dt.Throughput == nil {
		return 0, 0, invalid("Invalid value for field '%sprovisionedThroughput': '%d'. Disk type %s does not support provisioned throughput", field, throughput, typ)
	}
	if iops == 0 && dt.IOPS != nil {
		iops = dt.IOPS.DefaultAt(sizeGB)
	}
	if throughput == 0 && dt.Throughput != nil {
		throughput = dt.Throughput.DefaultAt(sizeGB)
	}
	return iops, throughput, nil
}

// delta is what changing an object from old's charge to new's charges
// (up) and gives back (down), class by class.
func delta(ctx context.Context, tx store.Tx, old, new priced) (up, down []ledger.Amount, err error) {
	was, err := old.amounts(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	now, err := new.amounts(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	d := map[ledger.Key]int64{}
	var keys []ledger.Key
	for _, a := range now {
		if _, ok := d[a.Key]; !ok {
			keys = append(keys, a.Key)
		}
		d[a.Key] += a.Amount
	}
	for _, a := range was {
		if _, ok := d[a.Key]; !ok {
			keys = append(keys, a.Key)
		}
		d[a.Key] -= a.Amount
	}
	for _, k := range keys {
		switch n := d[k]; {
		case n > 0:
			up = append(up, ledger.Amount{Key: k, Amount: n})
		case n < 0:
			down = append(down, ledger.Amount{Key: k, Amount: -n})
		}
	}
	return up, down, nil
}

// rechargeObject charges object the increase up, before GCE is called;
// it is refunded if the call or its operation fails.
func (s *Server) rechargeObject(q *request, object string, plan func(tx store.Tx) (up, down []ledger.Amount, err error)) (up, down []ledger.Amount, err error) {
	ctx := q.ctx()
	err = s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		if up, down, err = plan(tx); err != nil || len(up) == 0 {
			return err
		}
		return quotaMessage(q, ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: object, Silo: q.target.ID, Via: q.via(), Amounts: up}))
	})
	return up, down, err
}

// refundObject gives back up after GCE refused the call itself.
func (s *Server) refundObject(q *request, object string, up []ledger.Amount) {
	if len(up) == 0 {
		return
	}
	ctx := context.WithoutCancel(q.ctx())
	_ = s.st.Tx(ctx, func(tx store.Tx) error {
		_, err := ledger.Refund(ctx, tx, object, up)
		return err
	})
}

// priceStored is what an image or a snapshot of storage bytes charges.
func priceStored(resource, object string, bytes int64) priced {
	return priced{object: object, resource: resource, measures: map[string]int64{measureCount: 1, measureStorageGB: gbUp(bytes)}}
}

// gbUp is bytes in GB, rounded up.
func gbUp(bytes int64) int64 { return (bytes + 1<<30 - 1) >> 30 }

// diskTypeOf is a disk type link's type name, pd-standard if empty, as
// GCE defaults it.
func diskTypeOf(link string) string {
	if link == "" {
		return "pd-standard"
	}
	return gceapi.LastSegment(link)
}

// diskSize is the size a new disk gets: the size asked for, or its
// source image's or snapshot's, or the type's default.
func (s *Server) diskSize(ctx context.Context, zone, typ string, size int64, image *compute.Image, snap *compute.Snapshot) (int64, error) {
	switch {
	case size > 0:
		return size, nil
	case image != nil:
		return image.DiskSizeGb, nil
	case snap != nil:
		return snap.DiskSizeGb, nil
	}
	dt, err := s.gce.GetDiskType(ctx, s.opts.HostProject, zone, typ)
	if err != nil {
		if gceapi.IsNotFound(err) {
			return 0, invalid("Invalid value for field 'resource.type': '%s'. Disk type does not exist in zone %s", typ, zone)
		}
		return 0, err
	}
	return dt.DefaultDiskSizeGb, nil
}

// ---- quota:preview ----

// Pricer prices facade requests for the fence API's quota:preview
// (fence-api.md, "Quota preview", SV-10): the same checks and the same
// bill as the call would make, charging nothing.
func (s *Server) Pricer() fenceapi.Pricer { return pricer{s} }

type pricer struct{ s *Server }

// Price implements fenceapi.Pricer. It runs inside the preview's
// transaction, so it reads the tree there, and asks GCE what the call
// would (the machine type, a source image's size).
func (p pricer) Price(ctx context.Context, tx store.Tx, caller fenceapi.Caller, fr fenceapi.FacadeRequest) (string, []ledger.Amount, error) {
	s := p.s
	path := fr.Path
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	rest, ok := strings.CutPrefix(path, "/compute/v1/")
	if !ok {
		return "", nil, fenceErr(fenceapi.InvalidArgument, "facade_request.path %q is not a compute/v1 path", fr.Path)
	}
	method := strings.ToUpper(fr.Method)
	if method == "" {
		method = http.MethodPost
	}
	c, ok := parsePath(method, rest)
	if !ok || c.Method == "" {
		return "", nil, fenceErr(fenceapi.InvalidArgument, "facade_request.path %q is not a facade method", fr.Path)
	}
	price := pricers[c.FullMethod()]
	if price == nil {
		return "", nil, fenceErr(fenceapi.InvalidArgument, "compute.%s charges no quota, so has nothing to preview", c.FullMethod())
	}
	var t *tree.Silo
	var err error
	switch {
	case c.Project == s.opts.HostProject:
		t, err = tree.Get(ctx, tx, caller.Silo)
	case strings.HasPrefix(c.Project, "silo-"):
		t, err = tree.Get(ctx, tx, strings.TrimPrefix(c.Project, "silo-"))
	default:
		return "", nil, fenceErr(fenceapi.NotFound, "project %s is not a silo's", c.Project)
	}
	if err != nil {
		return "", nil, err
	}
	// The request keeps its query: disks.update's paths, a sourceImage.
	r, _ := http.NewRequestWithContext(ctx, method, fr.Path, nil)
	q := &request{r: r, call: c, rest: rest, caller: caller, target: t, body: map[string]any{}, regions: map[string]bool{}}
	q.n = &namer{silo: t.ID, host: s.opts.HostProject, base: s.opts.Endpoint, network: s.opts.Network, public: s.public}
	regions, err := ledger.Regions(ctx, tx, t.ID)
	if err != nil {
		return "", nil, err
	}
	for _, r := range regions {
		q.regions[r] = true
	}
	if err := q.checkScope(); err != nil {
		return "", nil, previewError(err)
	}
	if len(fr.Body) > 0 && string(fr.Body) != "null" {
		if err := json.Unmarshal(fr.Body, &q.body); err != nil {
			return "", nil, fenceErr(fenceapi.InvalidArgument, "facade_request.body: %v", err)
		}
	}
	if err := checkReserved(q.body, ""); err != nil {
		return "", nil, previewError(err)
	}
	if err := checkFields(q.body, treeOf(requestFields[c.FullMethod()]), ""); err != nil {
		return "", nil, previewError(err)
	}
	as, err := price(s, q, tx)
	if err != nil {
		return "", nil, previewError(err)
	}
	return t.ID, as, nil
}

// pricers are the methods quota:preview can price, each working out
// what its handler would charge, in the preview's transaction.
var pricers = map[string]func(s *Server, q *request, tx store.Tx) ([]ledger.Amount, error){}

// billPricer is the pricer of a method whose handler charges the bill
// plan builds.
func billPricer(plan func(s *Server, q *request) (bill, error)) func(s *Server, q *request, tx store.Tx) ([]ledger.Amount, error) {
	return func(s *Server, q *request, tx store.Tx) ([]ledger.Amount, error) {
		b, err := plan(s, q)
		if err != nil {
			return nil, err
		}
		lines, err := b.lines(q.ctx(), tx)
		if err != nil {
			return nil, err
		}
		return all(lines), nil
	}
}

// previewError is a facade refusal as the fence API words it.
func previewError(err error) error {
	e, ok := err.(*Error)
	if !ok {
		return err
	}
	code := fenceapi.InvalidArgument
	switch e.Code {
	case http.StatusForbidden:
		code = fenceapi.PermissionDenied
	case http.StatusNotFound:
		code = fenceapi.NotFound
	case http.StatusConflict:
		code = fenceapi.AlreadyExists
	}
	return fenceErr(code, "%s", e.Message)
}

func fenceErr(code fenceapi.Code, format string, args ...any) *fenceapi.Error {
	return &fenceapi.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// sortedRegions are q's allowed regions, sorted.
func (q *request) sortedRegions() []string {
	out := make([]string, 0, len(q.regions))
	for r := range q.regions {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
