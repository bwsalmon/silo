// Package gce is silod's per-silo compute/v1 facade: GCE's REST API,
// served at /compute/v1/projects/<project>/..., over the one host
// project every silo lives in (docs/silo/gcp-backend.md, "The
// compute/v1 facade", SV-61 to SV-76).
//
// A call goes through, in order:
//
//  1. the resource type: one the facade doesn't know is 404 (SV-63);
//  2. the project: silo-<id>, or the host project's ID meaning the
//     caller's own silo. The caller reaches its own silo and its
//     descendants', and, through the facade.ManageChecker hook, a manage
//     grant's target. Any other project is 403, whether or not it
//     exists (SV-62);
//  3. the caller's own effective control policy, by pkg/control's
//     table (SV-62);
//  4. the method: one not in the handler table is 403 (SV-63);
//  5. the region or zone: one outside the silo's allowed regions is 403
//     regionNotAllowed;
//  6. for a write, the body: labels and tags starting with silo are
//     refused, and so is any field outside the method's allowlist set
//     to anything but its default (SV-63, SV-76);
//  7. the handler, which asks GCE in the host project for the silo's
//     own objects only, and builds the response from allowlisted
//     fields, with names and links mapped back (SV-63, SV-76).
//
// The mechanics of steps 1 to 4 and 6, the response allowlist of 7 and
// the facade.call audit are pkg/facade's shared core; this package supplies GCE's paths, regions, handlers, naming
// and error shape. The allowlists are data, in tables.go; the resource
// write paths add their rows there and their handlers to handlers.
//
// The package is named gce after the facade it serves; it imports the
// GCE client, pkg/backend/gcp/gce, as gceapi.
package gce

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	gceapi "github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/facade"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/manage"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
	compute "google.golang.org/api/compute/v1"
)

// DefaultImageProjects are the public image projects whose images a
// tenant may read (gcp-backend.md, "Covered"). Options.ImageProjects
// replaces the list.
var DefaultImageProjects = []string{
	"debian-cloud", "ubuntu-os-cloud", "cos-cloud", "rocky-linux-cloud", "fedora-coreos-cloud",
	"centos-cloud", "ubuntu-os-pro-cloud", "opensuse-cloud", "suse-cloud", "rhel-cloud", "windows-cloud",
}

// DefaultEndpoint is the facade's base URL inside the cloud.
const DefaultEndpoint = "http://silo.internal:8080/compute/v1/"

// KindFacadeCall is the audit kind of every facade write, accepted or
// refused.
const KindFacadeCall = facade.KindFacadeCall

// KindFacadeRefund is the audit kind of a refund for a failed
// operation.
const KindFacadeRefund = "facade.refund"

// RefundFunc gives back what a failed operation charged, inside tx.
// The default refunds everything rec.Object is charged.
type RefundFunc func(ctx context.Context, tx store.Tx, rec Operation, op *compute.Operation) error

// Options configure a Server.
type Options struct {
	// GCE is the client every call to the host project goes through.
	GCE gceapi.Client
	// HostProject is the host project's ID.
	HostProject string
	// Network is the shared VPC's real name; "silo" by default.
	Network string
	// Endpoint is the facade's base URL, ending /compute/v1/, that
	// selfLinks are written on.
	Endpoint string
	// ImageProjects are the public image projects; DefaultImageProjects
	// if nil.
	ImageProjects []string
	// Compat is GCE's machine-type compatibility table for GPUs, local
	// SSD and disk types; DefaultCompat() if nil.
	Compat *Compat
	// Caller identifies the caller of a request that carries none in
	// its context (fenceapi.ContextWithCaller).
	Caller func(*http.Request) (fenceapi.Caller, error)
	// Manage is the manage-grant hook (gcp-backend.md, "Projects and
	// the caller"), asked with the resource type in the call's path;
	// facade.ManageGrants if nil.
	Manage facade.ManageChecker
	// Refund overrides the refund of a failed operation.
	Refund RefundFunc
	// Ready reports whether the store is usable: "" or why not, which
	// makes every call 503.
	Ready func() string
	// Replica is this silod replica's id, recorded on each operation
	// it starts as the replica watching it, so that the leader can
	// adopt the operations of a replica whose heartbeat stops
	// (state.md "More than one silod, and the leader", SV-52). Empty
	// records nothing, for a schema before store.LeaderSchema.
	Replica string
	// SkipControl skips silod's own control-policy check, so that a test
	// can show the gateway's check holds alone (gateway.md, "Layer 1").
	// Never set outside tests.
	SkipControl bool
}

// Server serves the facade.
type Server struct {
	st     store.Store
	gce    gceapi.Client
	opts   Options
	public map[string]bool

	wg       sync.WaitGroup
	bg       context.Context
	stop     context.CancelFunc
	watching sync.Map // scope/name of the operations being watched
}

// New returns a Server on st.
func New(st store.Store, opts Options) *Server {
	if opts.Network == "" {
		opts.Network = "silo"
	}
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultEndpoint
	}
	if !strings.HasSuffix(opts.Endpoint, "/") {
		opts.Endpoint += "/"
	}
	if opts.Compat == nil {
		opts.Compat = DefaultCompat()
	}
	if opts.ImageProjects == nil {
		opts.ImageProjects = DefaultImageProjects
	}
	if opts.Manage == nil {
		opts.Manage = facade.ManageGrants{}
	}
	s := &Server{st: st, gce: opts.GCE, opts: opts, public: map[string]bool{}}
	for _, p := range opts.ImageProjects {
		s.public[p] = true
	}
	s.bg, s.stop = context.WithCancel(context.Background())
	return s
}

// Close stops watching operations, and waits for the watchers. Resume
// picks them up again.
func (s *Server) Close() {
	s.stop()
	s.wg.Wait()
}

// request is one call being served.
type request struct {
	w      http.ResponseWriter
	r      *http.Request
	call   call
	rest   string // the path after /compute/v1/
	caller fenceapi.Caller
	class  control.Class
	// target is the silo whose facade it is, chain its ancestry, root
	// first, and regions its allowed regions.
	target  *tree.Silo
	chain   []*tree.Silo
	regions map[string]bool
	n       *namer
	// public is set for a read of a public image project.
	public bool
	body   map[string]any
}

func (q *request) ctx() context.Context { return q.r.Context() }

// rewrite maps names in a GCE error message back, once the target is
// known.
func (q *request) rewrite(msg string) string {
	if q.n == nil {
		return msg
	}
	return q.n.outText(msg)
}

// ServeHTTP serves a compute/v1 call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/compute/v1/")
	if !ok {
		writeError(w, notFound(r.URL.Path), nil)
		return
	}
	if s.opts.Ready != nil {
		if why := s.opts.Ready(); why != "" {
			writeError(w, unavailable("silo: silod is not serving: %s", why), nil)
			return
		}
	}
	c, ok := parsePath(r.Method, rest)
	if !ok {
		writeError(w, notFound(rest), nil)
		return
	}
	q := &request{w: w, r: r, call: c, rest: rest, class: control.FacadeClass(c.FullMethod())}
	caller, ok := fenceapi.CallerFromContext(r.Context())
	if !ok && s.opts.Caller != nil {
		var err error
		if caller, err = s.opts.Caller(r); err != nil {
			writeError(w, forbidden("silo: %v", err), nil)
			return
		}
		ok = caller.Silo != ""
	}
	if !ok {
		writeError(w, forbidden("silo: the call carries no caller"), nil)
		return
	}
	q.caller = caller
	out, err := s.serve(q)
	if err != nil {
		writeError(w, err, q.rewrite)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) serve(q *request) (any, error) {
	c := q.call
	if !table.Known(c.Resource) {
		return nil, notFound(q.rest)
	}
	q.public = s.public[c.Project] && c.Resource == "images" && q.class == control.Reading
	if err := s.resolve(q); err != nil {
		return nil, err
	}
	h, ok := table.Lookup(c.Resource, c.FullMethod())
	if c.Method == "" || !ok || h == nil {
		return nil, s.refuse(q, methodNotAllowed(c.FullMethod()))
	}
	if err := q.checkScope(); err != nil {
		return nil, s.refuse(q, err)
	}
	if q.class == control.Mutation {
		if err := s.refuseToolVM(q); err != nil {
			return nil, s.refuse(q, err)
		}
		if err := s.readBody(q); err != nil {
			return nil, s.refuse(q, err)
		}
	}
	return h(s, q)
}

// refuse audits a refused write once its silo is known, and returns
// the refusal.
func (s *Server) refuse(q *request, err error) error {
	if q.class == control.Mutation && q.target != nil {
		e := asError(err, q.rewrite)
		if e.Code != http.StatusServiceUnavailable {
			s.audit(q, e.Reason, nil, nil)
		}
	}
	return err
}

// resolve finds the silo the call's project names, checks the caller
// may reach it, and holds the caller to its control policy.
func (s *Server) resolve(q *request) error {
	ctx := q.ctx()
	c := q.call
	ref := facade.Ref{Resource: c.Resource, Write: q.class == control.Mutation}
	switch {
	case q.public || c.Project == s.opts.HostProject:
		ref.Self = true
	case strings.HasPrefix(c.Project, "silo-"):
		ref.Silo = strings.TrimPrefix(c.Project, "silo-")
	}
	// silod's own call for an agent's tool VM is covered by the
	// grant's agents group, which is what let the holder create or
	// delete the agent.
	if agentCall(ctx) != "" {
		ref.Resource = manage.GroupAgents
	}
	var denied error
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		t, err := facade.Resolve(ctx, tx, s.opts.Manage, q.caller, ref)
		if err == nil && !s.opts.SkipControl {
			err = facade.CheckControl(ctx, tx, t, q.class, "compute."+c.FullMethod(), requestID(q.r))
		}
		if e, ok := q.refusal(err); ok {
			denied = e
			return nil
		}
		if err != nil {
			return err
		}
		regions, err := ledger.Regions(ctx, tx, t.Silo.ID)
		if err != nil {
			return err
		}
		q.caller, q.target, q.chain = t.Caller, t.Silo, t.Chain
		q.regions = map[string]bool{}
		for _, r := range regions {
			q.regions[r] = true
		}
		return nil
	})
	if err != nil {
		return asError(err, nil)
	}
	if denied != nil {
		return denied
	}
	q.n = &namer{silo: q.target.ID, host: s.opts.HostProject, base: s.opts.Endpoint, network: s.opts.Network, public: s.public}
	return nil
}

// refusal answers a refusal of the shared core in GCE's shape.
func (q *request) refusal(err error) (*Error, bool) {
	var (
		unreachable *facade.Unreachable
		grant       *facade.GrantDenied
		ctl         *facade.ControlDenied
		field       *facade.FieldDenied
	)
	switch {
	case errors.As(err, &unreachable):
		return projectForbidden(q.call.FullMethod(), q.call.Project), true
	case errors.As(err, &grant):
		return forbidden("%s", grant.Error()), true
	case errors.As(err, &ctl):
		return forbidden("%s", ctl.Error()), true
	case errors.As(err, &field):
		return forbidden("%s", field.Error()), true
	}
	return nil, false
}

// checkFields refuses the first field of body outside t that is set to
// anything but its default: 403, naming the field.
func checkFields(body any, t facade.Fields) error {
	err := facade.CheckFields(body, t)
	var field *facade.FieldDenied
	if errors.As(err, &field) {
		return forbidden("%s", field.Error())
	}
	return err
}

// checkScope refuses a region, or a zone in a region, outside the
// silo's allowed regions: 403 regionNotAllowed rather than 404, so a
// tenant can tell "not allowed" from "doesn't exist".
func (q *request) checkScope() error {
	switch {
	case q.call.Zone != "":
		if !q.regions[gceapi.ZoneRegion(q.call.Zone)] {
			return regionNotAllowed(q.target.Path, "zone "+q.call.Zone)
		}
	case q.call.Region != "":
		if !q.regions[q.call.Region] {
			return regionNotAllowed(q.target.Path, "region "+q.call.Region)
		}
	}
	return nil
}

// readBody reads a write's body and checks it against the method's
// allowlist and the reserved labels and tags.
func (s *Server) readBody(q *request) error {
	b, err := io.ReadAll(io.LimitReader(q.r.Body, 4<<20))
	if err != nil {
		return invalid("silo: reading the body: %v", err)
	}
	q.body = map[string]any{}
	if strings.TrimSpace(string(b)) != "" {
		if err := json.Unmarshal(b, &q.body); err != nil {
			return invalid("Invalid JSON payload received. %v", err)
		}
	}
	if err := checkReserved(q.body, ""); err != nil {
		return err
	}
	return checkFields(q.body, facade.AllowFields(requestFields[q.call.FullMethod()]))
}

// requestID is the call's requestId, GCE's idempotency key.
func requestID(r *http.Request) string { return r.URL.Query().Get("requestId") }

// audit writes a facade.call record for a write, in the target's
// audit, with the via of how the caller reached it. result is "OK" or
// the refusal's reason; charges are what the call charged.
func (s *Server) audit(q *request, result string, extra map[string]any, charges []fenceapi.ChargeLine) {
	ctx := q.ctx()
	_ = s.st.Tx(ctx, func(tx store.Tx) error {
		return s.auditTx(ctx, tx, q, result, extra, charges)
	})
}

func (s *Server) auditTx(ctx context.Context, tx store.Tx, q *request, result string, extra map[string]any, charges []fenceapi.ChargeLine) error {
	fields := map[string]any{"project": q.call.Project}
	if q.call.Scope() != "global" {
		fields["scope"] = q.call.Scope()
	}
	name := q.call.Name
	if v, ok := q.body["name"].(string); ok && q.call.Method == "insert" {
		name = v
	}
	if name != "" && tenantCollections[q.call.Collection] {
		fields["name"] = name
	}
	for k, v := range extra {
		fields[k] = v
	}
	return facade.Audit(ctx, tx, q.target.ID, q.caller, facade.Call{Method: "compute." + q.call.FullMethod(),
		RequestID: requestID(q.r), Result: result, Charges: charges, Fields: fields})
}

// via is how the caller reached the target, as a charge records it.
func (q *request) via() string { return facade.Via(q.caller, q.target.ID) }
