// Package docker is the local backend's per-silo facade: a subset of
// the Docker Engine API, served at docker.silo.internal:2375 inside
// silo-net, on a socket per silo on the host, and at /docker/ on an
// agent's session socket (docs/silo/local-backend.md, §4 "The Docker
// facade").
//
// A call goes through, in order:
//
//  1. the API version: /v1.NN/ outside 1.41 to 1.47 is 400, and an
//     unversioned path is 1.47;
//  2. the route: a route Docker has and the facade refuses gets its
//     refusal (501, or 503 for swarm), and one it doesn't know at all
//     Docker's 404 "page not found" (tables.go);
//  3. the caller, from the request's context (origin identity, the
//     socket's peer uid, or the session socket), and the target: the
//     socket's silo, or the silo a Host of <id>.docker.silo.internal
//     names, or else the caller's own. The caller reaches its own silo,
//     its descendants and, through a manage grant, the grant's target;
//     anything else is the same 404 whether or not it exists;
//  4. the caller's own effective control policy, by the route's class;
//  5. for a write, the body: Silo's label namespace and the refusals
//     that are a fence (privilege, host paths, devices, host
//     namespaces) are 403, and a field outside the route's allowlist
//     set to anything but its default is 400;
//  6. the handler, which keeps the silo's objects in the store
//     (docker_objects), charges the ledger before it calls the runtime
//     driver and refunds on failure, and audits every write as
//     facade.call.
//
// Steps 3, 4, the allowlist of 5 and the audit are pkg/facade's
// shared core. Names are the silo's own: two silos may each have a
// container called web. Ids are 64 hex characters silod makes, and a
// prefix is resolved within the silo only.
//
// The package also answers what the gateway asks of it: silo-net's
// topology for the fence (Topology), the published ports it listens
// on (Published), and the names its DNS answers (Lookup).
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/facade"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The Engine API versions served (call L5).
const (
	APIVersion    = "1.47"
	MinAPIVersion = "1.41"
	minMinor      = 41
	maxMinor      = 47
)

// Domain is the facade's name inside silo-net; <silo id>.Domain names
// another silo.
const Domain = "docker.silo.internal"

// Port is the facade's port on Domain.
const Port = 2375

// Options configure a Server.
type Options struct {
	// Drivers are the classes this host runs.
	Drivers []runtime.Driver
	// Storage keeps volumes. Meter, if set, refuses a mount of a volume
	// over its charge.
	Storage storage.Driver
	Meter   *storage.Meter
	// Fetcher is the pull-through cache's fetcher, which every pull
	// from a registry other than registry.silo.internal goes through,
	// as the caller, held to its egress (local-registry.md, §8).
	Fetcher Fetcher
	// Images pulls images where there is no Fetcher, as the fake driver
	// does. Nil, with no Fetcher, refuses pulls.
	Images Images
	// Content is silo-containerd's content store, which save reads, and
	// load and commit write. Nil refuses them.
	Content registry.Content
	// RunNamespace is the content store namespace the drivers run
	// images from (rootless.ContainerdNamespace). Set, a container's
	// image is recorded there before its driver prepares it; empty,
	// the drivers find images themselves, as the fake ones do.
	RunNamespace string
	// Registry is the local registry, which push writes and pulls of
	// registry.silo.internal names read, as the caller. Nil refuses
	// them.
	Registry LocalRegistry
	// Login validates an upstream credential for docker login, through
	// the egress proxy as silo, and stores nothing. Nil refuses logins
	// to upstreams.
	Login func(ctx context.Context, silo, server, username, password string) error
	// Wire makes and wires a container's namespaces before the driver
	// creates it: a bridge for each of its networks (Network.Bridge;
	// nets are the silo's, by id), and a veth on each, pinned to the
	// endpoint's MAC and address. It names them in spec (NetNS, and
	// UserNS and Root where it makes a user namespace). Nil leaves
	// spec's NetNS a path under /run/silo/netns that nothing made,
	// which only a fake driver accepts.
	Wire func(ctx context.Context, c *Container, nets map[string]*Network, spec *runtime.Spec) error
	// Unwire undoes Wire, once the driver has deleted the container or
	// failed to make it. Nil does nothing.
	Unwire func(ctx context.Context, c *Container) error
	// Listeners are the gateway's published-port listeners: a start
	// opens its container's, and a stop or a remove closes them. Nil
	// opens none.
	Listeners Listeners
	// Caller identifies the caller of a request that carries none in
	// its context (fenceapi.ContextWithCaller).
	Caller func(*http.Request) (fenceapi.Caller, error)
	// Manage is the manage-grant hook; facade.ManageGrants if nil.
	Manage facade.ManageChecker
	// Ready reports whether the store is usable: "" or why not, which
	// makes every call but /_ping 503.
	Ready func() string
	// Version is silod's version, which /version and /info report.
	Version string
	// ToolImage is the image an agent's sandbox-class tool container
	// runs when the agent names none, and VMToolImage a vm-class one's,
	// which carries silo-tools at runtime.ToolsPath (local-backend.md
	// §10). Empty, the agent must name one.
	ToolImage, VMToolImage string
	// PublishPorts is the range a published port with no HostPort is
	// given one from; 32768 to 60999, as Docker's, if zero.
	PublishPorts [2]int
}

// Server serves the facade.
type Server struct {
	st      store.Store
	opts    Options
	drivers map[runtime.Class]runtime.Driver

	// mu serialises the calls that change a silo's objects, so that
	// one name, address or port is never given twice by two calls in
	// flight.
	mu sync.Mutex

	// execs are the exec instances made and not yet forgotten, by id.
	execMu sync.Mutex
	execs  map[string]*execInstance

	// health is each started container's HEALTHCHECK state, by id
	// (health.go).
	healthMu sync.Mutex
	health   map[string]*health

	wg   sync.WaitGroup
	bg   context.Context
	stop context.CancelFunc
}

// New returns a Server on st.
func New(st store.Store, opts Options) *Server {
	if opts.Manage == nil {
		opts.Manage = facade.ManageGrants{}
	}
	if opts.PublishPorts == [2]int{} {
		opts.PublishPorts = [2]int{32768, 60999}
	}
	s := &Server{st: st, opts: opts, drivers: map[runtime.Class]runtime.Driver{}, execs: map[string]*execInstance{},
		health: map[string]*health{}}
	for _, d := range opts.Drivers {
		s.drivers[d.Class()] = d
	}
	s.bg, s.stop = context.WithCancel(context.Background())
	return s
}

// Close stops watching running containers and probing their health,
// and waits for the watchers and the probes.
func (s *Server) Close() {
	s.stop()
	s.wg.Wait()
}

type socketKey struct{}

// ContextWithSocket returns ctx for a call on silo's host socket,
// unix:///run/silo/docker/<silo id>.sock: its target is that silo,
// whatever its Host says.
func ContextWithSocket(ctx context.Context, silo string) context.Context {
	return context.WithValue(ctx, socketKey{}, silo)
}

// request is one call being served.
type request struct {
	w      http.ResponseWriter
	r      *http.Request
	route  *route
	params map[string]string
	minor  int // the API version's minor number
	caller fenceapi.Caller
	// target is the silo the call acts on, chain its ancestry, root
	// first.
	target *tree.Silo
	chain  []*tree.Silo
	body   map[string]any
	raw    []byte
}

func (q *request) ctx() context.Context { return q.r.Context() }

// query is a query parameter.
func (q *request) query(k string) string { return q.r.URL.Query().Get(k) }

// flag is a boolean query parameter, as Docker reads one.
func (q *request) flag(k string) bool {
	v := q.query(k)
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// via is how the caller reached the target, as a charge records it.
func (q *request) via() string { return facade.Via(q.caller, q.target.ID) }

var versioned = regexp.MustCompile(`^/v(\d+)\.(\d+)(/.*)$`)

// ServeHTTP serves a Docker Engine API call.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	minor := maxMinor
	if m := versioned.FindStringSubmatch(path); m != nil {
		major, _ := strconv.Atoi(m[1])
		n, _ := strconv.Atoi(m[2])
		path = m[3]
		switch {
		case major < 1 || (major == 1 && n < minMinor):
			writeError(w, invalid("client version %s.%s is too old. Minimum supported API version is %s, please upgrade your client to a newer version", m[1], m[2], MinAPIVersion))
			return
		case major > 1 || n > maxMinor:
			writeError(w, invalid("client version %s.%s is too new. Maximum supported API version is %s", m[1], m[2], APIVersion))
			return
		}
		minor = n
	}
	w.Header().Set("Api-Version", APIVersion)
	w.Header().Set("Server", "Silo (local backend)")
	rt, params, refusal := lookup(r.Method, path)
	if refusal != nil {
		writeError(w, refusal)
		return
	}
	q := &request{w: w, r: r, route: rt, params: params, minor: minor}
	// /_ping answers anyone who can connect: clients negotiate from it
	// before they do anything else.
	if rt.pattern == "/_ping" {
		_ = s.ping(q)
		return
	}
	if s.opts.Ready != nil {
		if why := s.opts.Ready(); why != "" {
			writeError(w, unavailable("silo: silod is not serving: %s", why))
			return
		}
	}
	caller, ok := fenceapi.CallerFromContext(r.Context())
	if !ok && s.opts.Caller != nil {
		var err error
		if caller, err = s.opts.Caller(r); err != nil {
			writeError(w, forbidden("silo: %v", err))
			return
		}
		ok = caller.Silo != ""
	}
	if !ok {
		writeError(w, forbidden("silo: the call carries no caller"))
		return
	}
	q.caller = caller
	if err := s.serve(q); err != nil {
		writeError(w, err)
	}
}

func (s *Server) serve(q *request) error {
	if err := s.resolve(q); err != nil {
		return err
	}
	if q.class() == control.Mutation && !q.route.stream {
		if err := s.readBody(q); err != nil {
			return s.refuse(q, err)
		}
	}
	return q.route.h(s, q)
}

// target is the Ref the call names: its socket's silo, the silo a Host
// of <id>.docker.silo.internal names, or the caller's own.
func target(r *http.Request) facade.Ref {
	if silo, ok := r.Context().Value(socketKey{}).(string); ok {
		return facade.Ref{Silo: silo}
	}
	host := r.Host
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if id, ok := strings.CutSuffix(host, "."+Domain); ok && id != "" && !strings.Contains(id, ".") {
		return facade.Ref{Silo: id}
	}
	return facade.Ref{Self: true}
}

// resolve finds the call's target, checks the caller may reach it, and
// holds the caller to its control policy.
func (s *Server) resolve(q *request) error {
	ctx := q.ctx()
	ref := target(q.r)
	ref.Resource, ref.Write = q.route.resource, q.class() == control.Mutation
	var denied error
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		t, err := facade.Resolve(ctx, tx, s.opts.Manage, q.caller, ref)
		if err == nil {
			err = facade.CheckControl(ctx, tx, t, q.class(), "docker."+q.r.Method+" "+q.route.pattern, "")
		}
		var refusal facade.Refusal
		if errors.As(err, &refusal) {
			denied = err
			return nil
		}
		if err != nil {
			return err
		}
		q.caller, q.target, q.chain = t.Caller, t.Silo, t.Chain
		return nil
	})
	if err != nil {
		return err
	}
	return denied
}

// refuse audits a refused write once its silo is known, and returns
// the refusal.
func (s *Server) refuse(q *request, err error) error {
	if q.target != nil {
		s.audit(q, asError(err).Message, nil, nil)
	}
	return err
}

// readBody reads a write's JSON body, if it has one, and checks it
// against the route's allowlist.
func (s *Server) readBody(q *request) error {
	b, err := io.ReadAll(io.LimitReader(q.r.Body, 4<<20))
	if err != nil {
		return invalid("silo: reading the body: %v", err)
	}
	q.raw = b
	q.body = map[string]any{}
	allow, ok := requestFields[q.route.pattern]
	if !ok {
		return nil
	}
	if strings.TrimSpace(string(b)) != "" {
		if err := json.Unmarshal(b, &q.body); err != nil {
			return invalid("invalid JSON: %v", err)
		}
	}
	if err := checkLabels(q.body); err != nil {
		return err
	}
	if q.route.pattern == "/containers/create" {
		if err := refuseHost(q.body); err != nil {
			return err
		}
	}
	if p, _ := q.body["Privileged"].(bool); p && q.route.pattern == "/containers/{id}/exec" {
		return forbidden("silo: a privileged exec is not allowed in a silo")
	}
	if err := facade.CheckFields(q.body, allow); err != nil {
		return asError(err)
	}
	return nil
}

// checkLabels refuses a label in Silo's namespace, wherever a body
// carries labels.
func checkLabels(body map[string]any) error {
	var maps []any
	maps = append(maps, body["Labels"])
	if hc, ok := body["HostConfig"].(map[string]any); ok {
		if ms, ok := hc["Mounts"].([]any); ok {
			for _, m := range ms {
				if vo, ok := dig(m, "VolumeOptions").(map[string]any); ok {
					maps = append(maps, vo["Labels"])
				}
			}
		}
	}
	for _, l := range maps {
		m, _ := l.(map[string]any)
		for k := range m {
			if reservedLabel(k) {
				return forbidden("silo: label %q is in Silo's own namespace (dev.silo., silo.), which is not the tenant's", k)
			}
		}
	}
	return nil
}

// dig is v[k] for a JSON object v, or nil.
func dig(v any, k string) any {
	m, _ := v.(map[string]any)
	return m[k]
}

// decode is the call's body as T.
func decode[T any](q *request) (T, error) {
	var t T
	if strings.TrimSpace(string(q.raw)) == "" {
		return t, nil
	}
	if err := json.Unmarshal(q.raw, &t); err != nil {
		return t, invalid("invalid JSON: %v", err)
	}
	return t, nil
}

// audit writes a facade.call record for a write, in the target's
// audit. result is "OK" or the refusal's message.
func (s *Server) audit(q *request, result string, fields map[string]any, charges []fenceapi.ChargeLine) {
	ctx := context.WithoutCancel(q.ctx())
	_ = s.st.Tx(ctx, func(tx store.Tx) error {
		return s.auditTx(ctx, tx, q, result, fields, charges)
	})
}

func (s *Server) auditTx(ctx context.Context, tx store.Tx, q *request, result string, fields map[string]any, charges []fenceapi.ChargeLine) error {
	if fields == nil {
		fields = map[string]any{}
	}
	for k, v := range q.params {
		fields[k] = v
	}
	return facade.Audit(ctx, tx, q.target.ID, q.caller, facade.Call{
		Method: "docker." + q.r.Method + " " + q.route.pattern, Result: result, Charges: charges, Fields: fields})
}

// noContent answers 204, as Docker answers most writes.
func noContent(q *request) error {
	q.w.WriteHeader(http.StatusNoContent)
	return nil
}

func ok(q *request, v any) error {
	writeJSON(q.w, http.StatusOK, v)
	return nil
}
