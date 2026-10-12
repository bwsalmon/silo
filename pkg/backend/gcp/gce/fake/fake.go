// Package fake is an in-memory GCE: the compute/v1 subset silod calls,
// and the Cloud Quotas read, behaving as GCE does where silod depends
// on it (docs/silo/testing.md "Fake GCE backend", SV-119). It
// implements gce.Client in process, and Server serves it over HTTP in
// the compute/v1 REST shape so the real client can be tested against
// it.
//
// What it models, and how:
//
//   - Names: GCE's name syntax and 63-character limit (400 invalid),
//     unique per scope (409 alreadyExists).
//   - Operations: every mutation is validated synchronously, as GCE
//     validates a request before it starts an operation, and then takes
//     effect when its operation finishes. Operations finish on their own
//     goroutine after Options.OperationLatency, or only when
//     RunOperations is called if Options.ManualOperations is set.
//     FailOperation makes the next matching operation fail with a
//     quota, stockout or internal error; an insert that fails leaves
//     nothing behind.
//   - Quota: limits are per test (SetQuota). An operation that would
//     take a metric over its limit fails with QUOTA_EXCEEDED, as GCE's
//     do; a metric with no limit set is not enforced. Spot use counts
//     against a PREEMPTIBLE_ metric when that metric's limit is above 0,
//     and against the standard one otherwise, as in GCE. regions.get
//     reports the metrics GCE reports there; the rest (H100 GPUs, the
//     Hyperdisk Extreme, Throughput and ML metrics) are reported only
//     by the Cloud Quotas read, which reports every limit.
//   - The catalog (Catalog): regions, zones, machine types (with GPUs
//     and local SSD they bundle), accelerator types per zone, disk
//     types, and GCE's compatibility rules between them.
//
// Everything the fake returns is a copy; callers may modify it freely.
//
// The fake doesn't evaluate packets: the fence evaluator, in fenceeval,
// decides them from the fake's firewall policy and rules.
package fake

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

// Options configure a Fake.
type Options struct {
	// Project is the host project. More projects may be added with
	// AddProject.
	Project string
	// Catalog is what GCE offers. Nil means DefaultCatalog().
	Catalog *Catalog
	// OperationLatency is how long an operation runs before it finishes,
	// when ManualOperations is not set.
	OperationLatency time.Duration
	// ManualOperations leaves every operation RUNNING until
	// RunOperations is called.
	ManualOperations bool
	// WaitTimeout caps WaitOperation, as GCE caps it at about two
	// minutes. Zero means two minutes.
	WaitTimeout time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Fake is an in-memory GCE. The zero value is not usable; call New.
type Fake struct {
	opts    Options
	catalog *Catalog

	mu       sync.Mutex
	seq      uint64
	projects map[string]*state
	limits   map[quotaKey]float64
	failures []Failure
	opOrder  []*op
}

var _ gce.Client = (*Fake)(nil)

// New returns a Fake with opts.Project and the public image projects of
// PublicImages.
func New(opts Options) *Fake {
	if opts.Project == "" {
		opts.Project = "host-project"
	}
	if opts.Catalog == nil {
		opts.Catalog = DefaultCatalog()
	}
	if opts.WaitTimeout == 0 {
		opts.WaitTimeout = 2 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	f := &Fake{
		opts:     opts,
		catalog:  opts.Catalog,
		projects: map[string]*state{},
		limits:   map[quotaKey]float64{},
	}
	f.AddProject(opts.Project)
	for _, img := range PublicImages() {
		f.AddImage(gce.ParseRef(img.SelfLink).Project, img)
	}
	return f
}

// AddProject makes project known, so calls naming it succeed. Calls
// naming an unknown project get GCE's 403.
func (f *Fake) AddProject(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.projects[name] == nil {
		f.seq++
		f.projects[name] = newProject(name, 100000000000+int64(f.seq))
	}
}

// AddImage adds img to project directly, with no operation: for public
// images and test fixtures. img.Name must be set.
func (f *Fake) AddImage(projectName string, img *compute.Image) {
	f.AddProject(projectName)
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[projectName]
	img = clone(img)
	img.Kind = "compute#image"
	img.Id = f.nextID()
	img.SelfLink = gce.SelfLinkBase + "projects/" + projectName + "/global/images/" + img.Name
	if img.Status == "" {
		img.Status = "READY"
	}
	if img.CreationTimestamp == "" {
		img.CreationTimestamp = f.timestamp()
	}
	p.images.put("global", img.Name, img)
}

type state struct {
	name   string
	number int64

	instances  table[compute.Instance]
	disks      table[compute.Disk]
	images     table[compute.Image]
	snapshots  table[compute.Snapshot]
	templates  table[compute.InstanceTemplate]
	networks   table[compute.Network]
	subnets    table[compute.Subnetwork]
	firewalls  table[compute.Firewall]
	policies   table[compute.FirewallPolicy]
	operations table[op]

	// pending holds the selfLinks of objects whose insert operation
	// hasn't finished. They hold their name, and count for nothing.
	pending map[string]bool
}

func newProject(name string, number int64) *state {
	return &state{
		name: name, number: number,
		instances: table[compute.Instance]{}, disks: table[compute.Disk]{},
		images: table[compute.Image]{}, snapshots: table[compute.Snapshot]{},
		templates: table[compute.InstanceTemplate]{}, networks: table[compute.Network]{},
		subnets: table[compute.Subnetwork]{}, firewalls: table[compute.Firewall]{},
		policies: table[compute.FirewallPolicy]{}, operations: table[op]{},
		pending: map[string]bool{},
	}
}

// link is the selfLink of name in collection under scope ("global",
// "zones/z" or "regions/r").
func (p *state) link(scope, collection, name string) string {
	return gce.SelfLinkBase + "projects/" + p.name + "/" + scope + "/" + collection + "/" + name
}

// table holds one collection: scope ("global", a zone or a region) →
// name → object.
type table[T any] map[string]map[string]*T

func (t table[T]) get(scope, name string) *T { return t[scope][name] }

func (t table[T]) put(scope, name string, v *T) {
	if t[scope] == nil {
		t[scope] = map[string]*T{}
	}
	t[scope][name] = v
}

func (t table[T]) del(scope, name string) { delete(t[scope], name) }

// list is scope's objects in name order.
func (t table[T]) list(scope string) []*T {
	names := make([]string, 0, len(t[scope]))
	for n := range t[scope] {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*T, 0, len(names))
	for _, n := range names {
		out = append(out, t[scope][n])
	}
	return out
}

// all is every object, by scope then name.
func (t table[T]) all() []*T {
	scopes := make([]string, 0, len(t))
	for s := range t {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	var out []*T
	for _, s := range scopes {
		out = append(out, t.list(s)...)
	}
	return out
}

// clone deep-copies a compute/v1 object through its JSON.
func clone[T any](v *T) *T {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out := new(T)
	if err := json.Unmarshal(b, out); err != nil {
		panic(err)
	}
	return out
}

func cloneAll[T any](vs []*T) []*T {
	out := make([]*T, len(vs))
	for i, v := range vs {
		out[i] = clone(v)
	}
	return out
}

func (f *Fake) nextID() uint64 {
	f.seq++
	return 1000000000000 + f.seq
}

func (f *Fake) fingerprint() string {
	f.seq++
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], f.seq)
	return base64.StdEncoding.EncodeToString(b[:])
}

func (f *Fake) timestamp() string {
	return f.opts.Now().UTC().Format(time.RFC3339Nano)
}

// lock takes the fake's lock and finds project, failing as GCE does
// for one the caller can't see.
func (f *Fake) lock(ctx context.Context, name string) (*state, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	p := f.projects[name]
	if p == nil {
		f.mu.Unlock()
		return nil, gce.NewError(http.StatusForbidden, gce.ReasonForbidden,
			"Required 'compute.projects.get' permission for 'projects/%s'", name)
	}
	return p, nil
}

func (f *Fake) unlock() { f.mu.Unlock() }

// checkName refuses a name GCE refuses, naming field.
func checkName(field, name string) error {
	if name == "" {
		return gce.NewError(http.StatusBadRequest, "required", "Required field '%s' not specified", field)
	}
	if !gce.ValidName(name) {
		return invalid(field, "Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'")
	}
	return nil
}

func invalid(field, format string, args ...any) error {
	return gce.NewError(http.StatusBadRequest, gce.ReasonInvalid,
		"Invalid value for field '%s': %s", field, fmt.Sprintf(format, args...))
}

func badRequest(format string, args ...any) error {
	return gce.NewError(http.StatusBadRequest, gce.ReasonInvalid, format, args...)
}

func alreadyExists(link string) error {
	return gce.NewError(http.StatusConflict, gce.ReasonAlreadyExists,
		"The resource '%s' already exists", trimBase(link))
}

func notFound(link string) error { return gce.NotFound(trimBase(link)) }

func conditionNotMet(format string, args ...any) error {
	return gce.NewError(http.StatusPreconditionFailed, gce.ReasonConditionNotMet, format, args...)
}

func inUse(format string, args ...any) error {
	return gce.NewError(http.StatusBadRequest, gce.ReasonResourceInUse, format, args...)
}

func trimBase(link string) string {
	if len(link) > len(gce.SelfLinkBase) && link[:len(gce.SelfLinkBase)] == gce.SelfLinkBase {
		return link[len(gce.SelfLinkBase):]
	}
	return link
}

func (f *Fake) zoneExists(zone string) error {
	if f.catalog.zone(zone) == nil {
		return gce.NotFound("projects/" + f.opts.Project + "/zones/" + zone)
	}
	return nil
}

func (f *Fake) regionExists(region string) error {
	if region == "" {
		return badRequest("Invalid value for field 'region': ''")
	}
	if f.catalog.region(region) == nil {
		return gce.NotFound("projects/" + f.opts.Project + "/regions/" + region)
	}
	return nil
}
