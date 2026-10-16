// Package server wires the hub's parts into one HTTP server, which is
// what `silo hub serve` runs (docs/hub.md §9, stage 2.5): the team
// directory (hub/directory), the backlog and its dispatcher
// (hub/tasks), live events (hub/live), the node registry
// (hub/registry) and the rendezvous nodes connect to
// (hub/rendezvous), over one database and one human sign-on
// (hub/identity). The dispatcher runs only while this process holds
// the database lease "dispatcher" (hub/lease, docs/hub.md §2.2.1).
//
// It serves:
//
//	GET  /healthz              the process is up (liveness)
//	GET  /readyz               the database answers and the hub is not shutting down (readiness, and the load balancer's health check)
//	GET  /v1/nodes/connect     nodes, authenticated by their node token (docs/hub.md §4)
//	GET  /v1/nodes             the node directory
//	     /v1/me, /v1/teams..., /v1/node-tokens...   the team directory's API (§5)
//	     /v1/tasks...          the backlog's API (§6), its events and artifacts (§7)
//	     /auth/...             the OIDC sign-in, when the hub is not behind IAP
//
// and, on a listener of its own (Config.MetricsAddr) that the load
// balancer never reaches, GET /metrics: its Prometheus metrics
// (docs/hub-operations.md).
//
// Every path but the first three signs its caller in through the
// configured sign-on: IAP's assertion behind Google Cloud IAP, or OIDC.
//
// With NodeUIDomain set, a request to <node>.<NodeUIDomain>, on any
// path, is none of the above: it is that node's own UI, carried through
// to its owner over the node's rendezvous connection (hub/nodeui,
// docs/hub.md §11).
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/silo/hub/directory"
	"github.com/bwsalmon/silo/hub/identity"
	"github.com/bwsalmon/silo/hub/lease"
	"github.com/bwsalmon/silo/hub/live"
	"github.com/bwsalmon/silo/hub/nodeui"
	"github.com/bwsalmon/silo/hub/registry"
	"github.com/bwsalmon/silo/hub/rendezvous"
	"github.com/bwsalmon/silo/hub/tasks"
)

// Paths the server answers without signing anyone in: the kubelet's
// probes and the load balancer's health check.
const (
	HealthzPath = "/healthz"
	ReadyzPath  = "/readyz"
)

// DefaultShutdownGrace is how long Serve waits, once its context ends,
// for nodes to take their goodbye and for requests to finish.
const DefaultShutdownGrace = 20 * time.Second

// Config is a hub's configuration: `silo hub serve`'s flags.
type Config struct {
	// Database is the store the directory, the backlog and the event
	// store share: "postgres://..." in production, "sqlite:PATH" (or
	// "sqlite::memory:") for local development only, since SQLite
	// holds one process's data on one disk.
	Database string
	// NoMigrate opens the stores without creating their tables: they
	// were made by Migrate, as the schema's owner, and Database's role
	// need only read and write their rows (docs/deploy.md). False, the
	// development default, creates any that are missing as Database's
	// role.
	NoMigrate bool

	// IAPAudience, when set, signs humans in from Google Cloud IAP's
	// assertion, checked against this audience: the IAP-enabled
	// backend service, "/projects/NUMBER/global/backendServices/ID"
	// (docs/hub.md §5.1).
	IAPAudience string
	// IAPKeys, when set, are the keys IAP assertions are checked
	// against; nil fetches IAP's published keys. For tests.
	IAPKeys *identity.KeySet

	// OIDCIssuer, when set, makes the hub an OIDC relying party for a
	// hub that is not behind IAP, with OIDCClientID, OIDCClientSecret,
	// OIDCRedirectURL (the hub's callback) and OIDCSessionKey (at
	// least 32 bytes, signing session cookies).
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string
	OIDCSessionKey   []byte

	// DevUserHeader, when set, takes the caller's email, unverified,
	// from this request header. It is for local development and tests
	// only: anyone who can reach the hub can claim to be anyone.
	DevUserHeader string

	// HubAdmins hold the hub-wide admin role (directory.API.HubAdmins):
	// user:<email> or group:<name>.
	HubAdmins []string

	// NodeUIDomain, when set, serves each node's own UI to its owner at
	// <node>.<NodeUIDomain> (nodeui.Server.Domain): a domain whose
	// every name reaches this hub, behind the same IAP. Empty serves
	// none.
	NodeUIDomain string

	// Heartbeat is the interval nodes heartbeat at;
	// rendezvous.DefaultHeartbeat when zero.
	Heartbeat time.Duration
	// ShutdownGrace bounds the shutdown; DefaultShutdownGrace when
	// zero.
	ShutdownGrace time.Duration
	// DispatchRetry is the dispatcher's Retry; tasks.DefaultRetry when
	// zero.
	DispatchRetry time.Duration

	// Holder names this process in the dispatcher's lease;
	// lease.DefaultHolder (the pod's name and a random suffix) when
	// empty.
	Holder string
	// LeaseRetry is how often a process that does not hold the
	// dispatcher's lease asks for it again; lease.DefaultRetry when
	// zero.
	LeaseRetry time.Duration

	// MetricsAddr, when set, is the address Serve serves MetricsPath on:
	// a listener of its own, so that the metrics are never on the
	// routes the load balancer exposes.
	MetricsAddr string

	// Logf, when set, receives the hub's log lines.
	Logf func(format string, args ...any)
}

func (c *Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// auth builds the configured sign-on, and the handler for its own
// paths (OIDC's sign-in), if it has any. Exactly one must be set.
func (c *Config) auth() (identity.Authenticator, http.Handler, []string, error) {
	n := 0
	for _, set := range []bool{c.IAPAudience != "", c.OIDCIssuer != "", c.DevUserHeader != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, nil, nil, errors.New("server: configure exactly one human sign-on: an IAP audience, an OIDC issuer, or (for development) a user header")
	}
	switch {
	case c.IAPAudience != "":
		if !strings.HasPrefix(c.IAPAudience, "/projects/") {
			return nil, nil, nil, fmt.Errorf("server: IAP audience %q: want /projects/NUMBER/global/backendServices/ID", c.IAPAudience)
		}
		return &identity.IAP{Audience: c.IAPAudience, Keys: c.IAPKeys}, nil, nil, nil
	case c.OIDCIssuer != "":
		if c.OIDCClientID == "" || c.OIDCRedirectURL == "" || len(c.OIDCSessionKey) < 32 {
			return nil, nil, nil, errors.New("server: OIDC needs a client ID, a redirect URL and a session key of at least 32 bytes")
		}
		cb, err := url.Parse(c.OIDCRedirectURL)
		if err != nil || cb.Path == "" {
			return nil, nil, nil, fmt.Errorf("server: OIDC redirect URL %q: want the hub's callback URL", c.OIDCRedirectURL)
		}
		o := &identity.OIDC{
			Issuer:       c.OIDCIssuer,
			ClientID:     c.OIDCClientID,
			ClientSecret: c.OIDCClientSecret,
			RedirectURL:  c.OIDCRedirectURL,
			SessionKey:   c.OIDCSessionKey,
		}
		return o, o.Handler(), []string{identity.LoginPath, identity.LogoutPath, cb.Path}, nil
	default:
		return headerAuth(c.DevUserHeader), nil, nil, nil
	}
}

// headerAuth is DevUserHeader's sign-on.
type headerAuth string

func (h headerAuth) Principal(r *http.Request) (identity.Principal, error) {
	email := strings.ToLower(strings.TrimSpace(r.Header.Get(string(h))))
	if email == "" {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return identity.Principal{Email: email}, nil
}

// Hub is the hub's parts, wired together.
type Hub struct {
	cfg Config

	Directory  *directory.Store
	Backlog    *tasks.Store
	Events     *live.Store
	Leases     *lease.Store
	Registry   *registry.Registry
	Rendezvous *rendezvous.Server
	Dispatcher *tasks.Dispatcher
	Live       *live.Hub
	NodeUI     *nodeui.Server

	handler  http.Handler
	metrics  *metrics
	draining atomic.Bool
}

// New opens the stores, creating their tables if they are missing
// (unless cfg.NoMigrate), and wires the hub's parts. Close closes the
// stores.
func New(ctx context.Context, cfg Config) (*Hub, error) {
	if cfg.Database == "" {
		return nil, errors.New("server: no database configured")
	}
	auth, authHandler, authPaths, err := cfg.auth()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(cfg.Database, "postgres://") && !strings.HasPrefix(cfg.Database, "postgresql://") {
		cfg.logf("hub: using SQLite (%s): for local development only", cfg.Database)
	}
	h := &Hub{cfg: cfg}
	h.metrics = newMetrics(h)
	fail := func(err error) (*Hub, error) {
		h.Close()
		return nil, err
	}
	openDirectory, openBacklog, openEvents, openLeases := directory.Open, tasks.Open, live.Open, lease.Open
	if cfg.NoMigrate {
		openDirectory, openBacklog, openEvents, openLeases = directory.OpenExisting, tasks.OpenExisting, live.OpenExisting, lease.OpenExisting
	}
	if h.Directory, err = openDirectory(ctx, cfg.Database); err != nil {
		return fail(fmt.Errorf("server: opening the team directory: %w", err))
	}
	if h.Backlog, err = openBacklog(ctx, cfg.Database); err != nil {
		return fail(fmt.Errorf("server: opening the backlog: %w", err))
	}
	if h.Events, err = openEvents(ctx, cfg.Database); err != nil {
		return fail(fmt.Errorf("server: opening the event store: %w", err))
	}
	if h.Leases, err = openLeases(ctx, cfg.Database); err != nil {
		return fail(fmt.Errorf("server: opening the leases: %w", err))
	}

	h.Registry = registry.New()
	hb := cfg.Heartbeat
	if hb <= 0 {
		hb = rendezvous.DefaultHeartbeat
	}
	h.Registry.StaleAfter = 3 * hb
	for _, adm := range cfg.HubAdmins {
		if p, legacy := directory.HubAdminPrincipal(adm); legacy {
			cfg.logf("hub: hub admin %q has no user: or group: prefix; treating it as %s (write group:NAME if a group was meant)", adm, p)
		}
	}
	dirAPI := &directory.API{Store: h.Directory, Auth: auth, HubAdmins: cfg.HubAdmins}
	h.Rendezvous = &rendezvous.Server{
		Registry:     h.Registry,
		Authenticate: h.Directory.Authenticate,
		Admit: func(owner string, n registry.Node) error {
			err := h.Directory.Admit(owner, n)
			if err != nil && !errors.Is(err, directory.ErrNotAdmitted) {
				// The directory could not be read: not a refusal.
				err = fmt.Errorf("%w: %w", rendezvous.ErrUnavailable, err)
			}
			return err
		},
		Visible:   dirAPI.Visible,
		Heartbeat: hb,
		Logf:      cfg.Logf,
		Observe:   h.metrics.observeRendezvous,
	}
	dirAPI.Disconnect = h.Rendezvous.Disconnect
	h.Dispatcher = &tasks.Dispatcher{
		Store:     h.Backlog,
		Registry:  h.Registry,
		Open:      h.Rendezvous.Open,
		Directory: h.Directory,
		Retry:     cfg.DispatchRetry,
		Logf:      cfg.Logf,
		Observe:   h.metrics.observeDispatch,

		LostRequeueAfter: h.Directory.LostRequeueAfter,
	}
	h.Live = &live.Hub{
		Log:       h.Events,
		Tasks:     h.Backlog,
		Directory: dirAPI,
		Open:      h.Rendezvous.Open,
		NodeOwner: func(id string) (string, bool) {
			e, ok := h.Registry.Get(id)
			return e.Owner, ok
		},
		Logf:     cfg.Logf,
		Ingested: h.metrics.observeIngest,
	}
	h.Rendezvous.NodeStream = h.Live.NodeStream
	h.Dispatcher.Finished = func(t tasks.Task) { h.Live.Finished(t.ID) }
	tasksAPI := &tasks.API{Store: h.Backlog, Dispatcher: h.Dispatcher, Directory: dirAPI}

	m := http.NewServeMux()
	m.HandleFunc("GET "+HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	m.HandleFunc("GET "+ReadyzPath, h.serveReady)
	rv := h.Rendezvous.Handler()
	m.Handle(rendezvous.ConnectPath, rv)
	m.Handle(rendezvous.DirectoryPath, rv)
	dir := dirAPI.Handler()
	for _, p := range []string{directory.MePath, directory.TeamsPath, directory.NodeTokensPath} {
		m.Handle(p, dir)
		m.Handle(p+"/", dir)
	}
	tk := tasksAPI.Handler()
	m.Handle(tasks.TasksPath, tk)
	m.Handle(tasks.TasksPath+"/", tk)
	lv := h.Live.Handler()
	m.Handle("GET "+live.EventsPath, h.metrics.subscriptions(lv))
	m.Handle("GET "+live.ArtifactsPath, h.metrics.viewThrough(lv))
	for _, p := range authPaths {
		m.Handle(p, authHandler)
	}
	h.handler = bodyDeadline(m, bodyTimeout)
	if cfg.NodeUIDomain != "" {
		h.NodeUI = &nodeui.Server{
			Domain:    strings.TrimSuffix(strings.ToLower(cfg.NodeUIDomain), "."),
			Registry:  h.Registry,
			Directory: dirAPI,
			Open:      h.Rendezvous.Open,
			Logf:      cfg.Logf,
		}
		h.handler = h.NodeUI.Hosts(h.handler)
	}
	// The node UI's hosts are counted as one route: their paths are the
	// nodes' own.
	h.handler = h.metrics.instrument(h.handler, func(r *http.Request) string {
		if h.NodeUI != nil {
			if _, ok := h.NodeUI.Node(r.Host); ok {
				return "node-ui"
			}
		}
		return ""
	})
	return h, nil
}

// Handler is the hub's HTTP handler.
func (h *Hub) Handler() http.Handler { return h.handler }

// MetricsHandler serves the hub's Prometheus metrics: what Serve serves
// on MetricsAddr.
func (h *Hub) MetricsHandler() http.Handler { return h.metrics.handler() }

func (h *Hub) serveReady(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.Directory.Ping(ctx); err != nil {
		h.metrics.storeErrors.WithLabelValues("database").Inc()
		http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

// Close closes the stores.
func (h *Hub) Close() error {
	var errs []error
	if h.Directory != nil {
		errs = append(errs, h.Directory.Close())
	}
	if h.Backlog != nil {
		errs = append(errs, h.Backlog.Close())
	}
	if h.Events != nil {
		errs = append(errs, h.Events.Close())
	}
	if h.Leases != nil {
		errs = append(errs, h.Leases.Close())
	}
	return errors.Join(errs...)
}

// Serve runs a hub on l until ctx ends (SIGTERM, for `silo hub
// serve`), and then shuts it down: /readyz turns unready, every node is
// sent a goodbye saying the hub is shutting down, so that it
// reconnects to the hub's replacement, the requests still open
// (event subscriptions among them) are ended, the dispatcher stops,
// and the stores are closed. The whole shutdown takes at most
// cfg.ShutdownGrace.
func Serve(ctx context.Context, l net.Listener, cfg Config) error {
	h, err := New(ctx, cfg)
	if err != nil {
		l.Close()
		return err
	}
	defer h.Close()
	return h.Serve(ctx, l)
}

// Serve is the package's Serve on an already wired hub. It does not
// close the stores.
func (h *Hub) Serve(ctx context.Context, l net.Listener) error {
	if h.cfg.MetricsAddr != "" {
		ml, err := net.Listen("tcp", h.cfg.MetricsAddr)
		if err != nil {
			l.Close()
			return fmt.Errorf("server: the metrics listener: %w", err)
		}
		mm := http.NewServeMux()
		mm.Handle("GET "+MetricsPath, h.MetricsHandler())
		msrv := &http.Server{Handler: mm, ReadHeaderTimeout: 10 * time.Second}
		go msrv.Serve(ml)
		// Closed last, so that a scrape during the shutdown still sees
		// the hub unready.
		defer msrv.Close()
		h.cfg.logf("hub: serving metrics on %s%s", ml.Addr(), MetricsPath)
	}
	// Requests, hijacked node connections included, live in reqCtx:
	// cancelling it ends the event subscriptions, which never go idle
	// for http.Server.Shutdown to wait for.
	reqCtx, endRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer endRequests()
	srv := &http.Server{
		Handler:           h.handler,
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ReadHeaderTimeout: 30 * time.Second,
		// Not ReadTimeout: it would end the event subscriptions and the
		// nodes' WebSockets, which bodyDeadline leaves alone.
		IdleTimeout: 2 * time.Minute,
	}
	dispatchCtx, stopDispatch := context.WithCancel(context.WithoutCancel(ctx))
	defer stopDispatch()
	// The dispatcher runs only while this process holds its lease, and
	// each term starts it afresh: its Run recovers the backlog first.
	// A term whose dispatcher fails gives the lease up and asks again.
	dispatched := make(chan error, 1)
	go func() {
		dispatched <- lease.Run(dispatchCtx, lease.Options{
			Store:  h.Leases,
			Name:   "dispatcher",
			Holder: h.cfg.Holder,
			Retry:  h.cfg.LeaseRetry,
			Run: func(ctx context.Context, _ lease.Term) error {
				return h.Dispatcher.Run(ctx)
			},
			OnTerm: h.metrics.observeTerm,
			Logf:   h.cfg.Logf,
		})
	}()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	h.cfg.logf("hub: serving on %s", l.Addr())

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
	}

	grace := h.cfg.ShutdownGrace
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	h.cfg.logf("hub: shutting down")
	h.draining.Store(true)
	if err := h.Rendezvous.Shutdown(sctx, "the hub is shutting down"); err != nil {
		h.cfg.logf("hub: nodes did not all close their connections: %v", err)
	}
	endRequests()
	if err := srv.Shutdown(sctx); err != nil {
		h.cfg.logf("hub: requests did not all finish: %v", err)
		srv.Close()
	}
	stopDispatch()
	select {
	case <-dispatched:
	case <-sctx.Done():
		h.cfg.logf("hub: the dispatcher did not stop in time")
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	h.cfg.logf("hub: stopped")
	return nil
}
