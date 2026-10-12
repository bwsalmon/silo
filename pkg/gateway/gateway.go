// Package gateway is Silo's gateway (docs/silo/gateway.md): the one
// address every silo reaches, serving the facade and the fence API on
// its API port with origin identity and Layer 1 of the control policy
// (SV-78, SV-79, SV-80), the egress proxy on port 3128 (SV-85, SV-86,
// SV-87, in proxy.go), proxied exports on port 80 and the TCP export
// ports (SV-83, in exports.go), and keeping its other ports for what
// later tasks put there.
//
// The gateway holds no cloud credentials and no store. It knows a
// caller by the TCP source IP of its connection and nothing else,
// looked up in tables silod pushes to it, and passes the answer to
// silod as Silo-Origin over silod's gateway socket. silod's half, which
// publishes the tables and believes that header, is in silod.go.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/silo/pkg/control"
	facade "github.com/bwsalmon/silo/pkg/facade/gcp/gce"
)

// The gateway's ports (gateway.md, "Address", SV-78).
const (
	APIPort    = 8080 // the facade and the fence API
	HTTPPort   = 80   // HTTP exports and the agent tool endpoints
	EgressPort = 3128 // the egress proxy
	FrontDoor  = 8443 // the front door (a later task)
	// TCP exports take one port each from this range.
	FirstTCPExport, LastTCPExport = 20000, 29999
)

// The agent tool endpoints, which port 80 serves with handlers of their
// own (gateway.md, "Agent tool channels"; tools.go).
const (
	ToolsAttachPath = "/silo/v1/agents/-/tools:attach"
	ToolsBinaryPath = "/silo/v1/agents/-/tools/binary"
)

// Options configure a Gateway.
type Options struct {
	// Dial connects to silod's gateway socket.
	Dial func(ctx context.Context) (net.Conn, error)
	// SkipControl skips the gateway's control-policy check, so that a
	// test can show silod's check holds alone. Never set outside tests.
	SkipControl bool
	// SkipControlNow, when it returns true, skips the check as
	// SkipControl does, call by call. Only silod's conformance build
	// (SILO_TEST_SKIP_CONTROL_GATE) sets it; nil everywhere else.
	SkipControlNow func() bool
	// Retry is how long the gateway waits before following silod's
	// tables again after the stream breaks; a second if zero.
	Retry time.Duration
	// Resolver resolves the hosts egress names; net.DefaultResolver if
	// nil. A test gives a fake.
	Resolver Resolver
	// DialEgress dials an address egress has checked; a net.Dialer's if
	// nil. A test points it at local servers.
	DialEgress func(ctx context.Context, network, addr string) (net.Conn, error)
	// DialExport dials an export's target; a net.Dialer's if nil.
	DialExport func(ctx context.Context, network, addr string) (net.Conn, error)
	// ListenTCP opens a TCP export port; net.Listen on TCPHost if nil. A
	// test listens elsewhere.
	ListenTCP func(port int) (net.Listener, error)
	// TCPHost is the address TCP export ports listen on; all if "".
	TCPHost string
	// ToolsDir holds the silo-tools binaries port 80 serves, one an
	// architecture (ToolsBinaryName); none are served if "".
	ToolsDir string
	// SilodWait is how long a facade or fence call waits for silod to
	// be reachable before it fails with 503 (gateway.md "Placement");
	// SilodWaitDefault if zero. Exports and egress don't wait: they are
	// served from the tables the gateway has.
	SilodWait time.Duration
	// HostAddress is the address of the control VM the gateway runs on,
	// invalid on a gateway-only VM: where agent tool attaches for this
	// VM's sessions land.
	HostAddress netip.Addr
}

// SilodWaitDefault is how long facade and fence calls wait for silod.
const SilodWaitDefault = 30 * time.Second

// HostAddress is Options.HostAddress: this gateway's control VM's own
// address, invalid on a gateway-only VM.
func (g *Gateway) HostAddress() netip.Addr { return g.opts.HostAddress }

// waitDial dials silod with opts.Dial, again and again until it
// answers or SilodWait has passed since the call began, so that a call
// made while silod restarts, or while a gateway replica's control VM is
// replaced, waits rather than failing at once.
func (g *Gateway) waitDial(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, g.opts.SilodWait)
	defer cancel()
	backoff := 100 * time.Millisecond
	for {
		c, err := g.opts.Dial(ctx)
		if err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("silod unreachable for %v: %w", g.opts.SilodWait, err)
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 2*time.Second {
			backoff = 2 * time.Second
		}
	}
}

// Gateway is the gateway process's state: the tables it last had from
// silod, and the API port's open connections.
type Gateway struct {
	opts   Options
	tables atomic.Pointer[Tables]
	proxy  *httputil.ReverseProxy
	// toolsProxy forwards tool channel attaches to silod.
	toolsProxy *httputil.ReverseProxy
	client     *http.Client

	mu    sync.Mutex
	conns map[net.Conn]struct{} // the API port's open connections
	// egress are the open egress connections.
	egress map[*egressConn]struct{}
	newer  chan struct{} // closed, and replaced, on new tables

	ex              exportState
	exportTransport *http.Transport
}

// New returns a Gateway that reaches silod through opts.Dial.
func New(opts Options) *Gateway {
	if opts.Retry == 0 {
		opts.Retry = time.Second
	}
	if opts.SilodWait == 0 {
		opts.SilodWait = SilodWaitDefault
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.DialEgress == nil {
		d := &net.Dialer{Timeout: egressTimeout}
		opts.DialEgress = d.DialContext
	}
	if opts.DialExport == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		opts.DialExport = d.DialContext
	}
	if opts.ListenTCP == nil {
		host := opts.TCPHost
		opts.ListenTCP = func(port int) (net.Listener, error) {
			return net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
		}
	}
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return opts.Dial(ctx) },
		MaxIdleConns:    64,
		IdleConnTimeout: 90 * time.Second,
	}
	g := &Gateway{opts: opts, conns: map[net.Conn]struct{}{}, egress: map[*egressConn]struct{}{}, newer: make(chan struct{}), client: &http.Client{Transport: tr}}
	g.exportsInit()
	g.toolsProxy = g.newToolsProxy(tr)
	g.exportTransport = &http.Transport{DialContext: opts.DialExport, MaxIdleConns: 64, IdleConnTimeout: 90 * time.Second}
	// Facade and fence calls wait for silod (waitDial); their own
	// connections, so that the tables' stream doesn't wait with them.
	apiTr := &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return g.waitDial(ctx) },
		MaxIdleConns:    64,
		IdleConnTimeout: 90 * time.Second,
	}
	g.proxy = &httputil.ReverseProxy{
		Transport: apiTr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "silod", "silod"
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("gateway: forwarding %s %s: %v", r.Method, r.URL.Path, err)
			unavailable("silo: silod is unreachable; retry").write(w, r.URL.Path)
		},
	}
	return g
}

// Tables are the tables the gateway has, nil before silod sent any.
func (g *Gateway) Tables() *Tables { return g.tables.Load() }

// SetTables installs t, if it is newer than what the gateway has, and
// closes the API-port connections it tightens (see closeTightened) and
// the egress connections it no longer allows (closeNarrowed). It is
// what Follow calls with each table silod sends.
func (g *Gateway) SetTables(t *Tables) bool {
	t.index()
	g.mu.Lock()
	old := g.tables.Load()
	if old != nil && t.Version < old.Version {
		g.mu.Unlock()
		return false
	}
	g.tables.Store(t)
	close(g.newer)
	g.newer = make(chan struct{})
	g.mu.Unlock()
	if old != nil {
		g.closeTightened(old, t)
	}
	g.closeNarrowed(t)
	g.revokeExports(t)
	return true
}

// WaitVersion waits until the gateway has tables at version v or
// newer, or ctx ends.
func (g *Gateway) WaitVersion(ctx context.Context, v uint64) error {
	for {
		g.mu.Lock()
		t, ch := g.tables.Load(), g.newer
		g.mu.Unlock()
		if t != nil && t.Version >= v {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// Follow reads the tables silod streams, until ctx ends. When the
// stream breaks, as while silod restarts, the gateway goes on serving
// the tables it has and follows again.
func (g *Gateway) Follow(ctx context.Context) {
	for ctx.Err() == nil {
		err := g.follow(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Printf("gateway: following silod's tables: %v; serving version %d meanwhile", err, g.version())
		select {
		case <-ctx.Done():
		case <-time.After(g.opts.Retry):
		}
	}
}

func (g *Gateway) version() uint64 {
	if t := g.tables.Load(); t != nil {
		return t.Version
	}
	return 0
}

func (g *Gateway) follow(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://silod"+TablesPath, nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("silod answered %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for sc.Scan() {
		var t Tables
		if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
			return err
		}
		if !g.SetTables(&t) {
			log.Printf("gateway: ignoring tables at version %d, older than %d", t.Version, g.version())
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("silod ended the stream")
}

// sourceIP is the request's TCP source address: the only thing origin
// identity reads.
func sourceIP(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// API is the API port's handler: /compute/v1/ and /silo/v1/, from a
// caller known by its source IP, held to its silo's effective control
// policy, then forwarded to silod with Silo-Origin.
func (g *Gateway) API() http.Handler { return http.HandlerFunc(g.serveAPI) }

func (g *Gateway) serveAPI(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	isFacade, isFence := strings.HasPrefix(p, "/compute/v1/"), strings.HasPrefix(p, "/silo/v1/")
	if !isFacade && !isFence {
		notFound("silo: no such path "+p+"; this port serves /compute/v1/ and /silo/v1/").write(w, p)
		return
	}
	// silod sees the path as it is written: one that cleans to anything
	// else (dot segments, doubled slashes) never leaves here.
	if clean := path.Clean(p); clean != strings.TrimSuffix(p, "/") && clean != p {
		notFound("silo: no such path "+p).write(w, p)
		return
	}
	t := g.tables.Load()
	if t == nil {
		unavailable("silo: the gateway has no origin table from silod yet; retry").write(w, p)
		return
	}
	ip, ok := sourceIP(r)
	if !ok {
		forbidden("silo: the caller's address is not known").write(w, p)
		return
	}
	silo, subnet, ok := t.Resolve(ip)
	info, known := t.Silos[silo]
	if !ok || !known {
		forbidden(fmt.Sprintf("silo: %s is in no silo's address space", ip)).write(w, p)
		return
	}
	if !subnet {
		forbidden(fmt.Sprintf("silo: %s is in %s's blocks but in no subnet of it", ip, info.Path)).write(w, p)
		return
	}
	if !g.opts.SkipControl && (g.opts.SkipControlNow == nil || !g.opts.SkipControlNow()) {
		var class control.Class
		var op string
		if isFacade {
			m := facade.Method(r.Method, p)
			class, op = control.FacadeClass(m), "compute."+m
			if m == "" {
				op = r.Method + " " + p
			}
		} else {
			fc := control.ParseFence(r.Method, strings.TrimPrefix(p, "/silo/v1/"))
			class, op = control.FenceClass(fc.Op), fc.Op
			if fc.Op == "" {
				op = r.Method + " " + p
			}
		}
		if !control.Allows(info.Control, class) {
			e := controlDenied(info.Path, info.Control)
			g.auditDenied(r, AuditRecord{Silo: silo, Version: t.Version, Op: op, Class: class.String(), Policy: info.Control, RequestID: requestID(r, isFacade)})
			e.write(w, p)
			return
		}
	}
	for k := range r.Header {
		if isSiloHeader(k) {
			r.Header.Del(k)
		}
	}
	r.Header.Set(OriginHeader, FormatOrigin(silo, t.Version))
	r.Header.Set(OriginAddressHeader, ip.String())
	g.proxy.ServeHTTP(w, r)
}

func requestID(r *http.Request, isFacade bool) string {
	if isFacade {
		return r.URL.Query().Get("requestId")
	}
	return r.URL.Query().Get("request_id")
}

// auditDenied has silod record a refusal of the gateway's, as
// control.denied. A failure to is logged: the refusal stands.
func (g *Gateway) auditDenied(r *http.Request, rec AuditRecord) {
	b, _ := json.Marshal(rec)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://silod"+AuditPath, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		log.Printf("gateway: auditing a control refusal of %s: %v", rec.Silo, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("gateway: auditing a control refusal of %s: silod answered %s", rec.Silo, resp.Status)
	}
}

// HTTP is port 80's handler. It has no facade or fence handler mounted
// at all, so no routing mistake here can reach the control plane:
// /compute/v1/ and /silo/v1/ are 404 naming port 8080, except the two
// agent tool endpoints, which have handlers of their own (tools.go):
// the binary is a file, and an attach goes only to silod's
// ToolsSilodPath. /x/ is HTTP exports and /r/ is reserved for served
// resources (after v1).
func (g *Gateway) HTTP() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ToolsAttachPath, g.serveToolsAttach)
	mux.HandleFunc(ToolsBinaryPath, g.serveToolsBinary)
	mux.HandleFunc("/x/", g.serveExport)
	mux.HandleFunc("/r/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "silo: /r/ is reserved for served resources, which come after v1", http.StatusNotFound)
	})
	apiOnly := func(w http.ResponseWriter, r *http.Request) {
		u := url.URL{Scheme: "http", Host: fmt.Sprintf("silo.internal:%d", APIPort), Path: r.URL.Path}
		http.Error(w, fmt.Sprintf("silo: the control APIs are not served on port %d; use port %d: %s", HTTPPort, APIPort, u.String()), http.StatusNotFound)
	}
	mux.HandleFunc("/compute/", apiOnly)
	mux.HandleFunc("/silo/", apiOnly)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "silo: no such path "+r.URL.Path, http.StatusNotFound)
	})
	return mux
}

// Track is the API port's http.Server ConnState: it keeps the open
// connections, so that a tightened silo's can be closed.
func (g *Gateway) Track(c net.Conn, s http.ConnState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch s {
	case http.StateNew:
		g.conns[c] = struct{}{}
	case http.StateClosed, http.StateHijacked:
		delete(g.conns, c)
	}
}

// closeTightened closes, at once, the API-port connections from the
// own space of every silo that cur makes stricter than old did: whose
// effective control policy tightened, which holds a manage grant that
// has ended, or which is gone. A connection is matched by its source
// IP in either table, so one that moved silo goes too.
func (g *Gateway) closeTightened(old, cur *Tables) {
	hit := map[string]bool{}
	for id, o := range old.Silos {
		n, ok := cur.Silos[id]
		if !ok || control.Stricter(n.Control, o.Control) {
			hit[id] = true
		}
	}
	for grant, holder := range old.Grants {
		if _, ok := cur.Grants[grant]; !ok {
			hit[holder] = true
		}
	}
	if len(hit) == 0 {
		return
	}
	g.mu.Lock()
	var doomed []net.Conn
	for c := range g.conns {
		ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
		if err != nil {
			continue
		}
		ip := ap.Addr().Unmap()
		a, _, _ := old.Resolve(ip)
		b, _, _ := cur.Resolve(ip)
		if hit[a] || hit[b] {
			doomed = append(doomed, c)
			delete(g.conns, c)
		}
	}
	g.mu.Unlock()
	for _, c := range doomed {
		c.Close()
	}
	if len(doomed) > 0 {
		log.Printf("gateway: closed %d API-port connections at version %d", len(doomed), cur.Version)
	}
}

// Serve serves the API port on api, port 80 on web and the egress proxy
// on egress (any may be nil) until ctx ends, following silod's tables
// meanwhile.
func (g *Gateway) Serve(ctx context.Context, api, web, egress net.Listener) error {
	go g.Follow(ctx)
	var servers []*http.Server
	errc := make(chan error, 3)
	start := func(l net.Listener, h http.Handler, track func(net.Conn, http.ConnState)) {
		if l == nil {
			return
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ConnState: track}
		servers = append(servers, srv)
		go func() { errc <- srv.Serve(l) }()
	}
	start(api, g.API(), g.Track)
	start(web, g.HTTP(), nil)
	start(egress, g.Egress(), nil)
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	g.closeExports()
	return err
}
