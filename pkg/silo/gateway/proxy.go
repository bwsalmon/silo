package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/egress"
)

// This file is the egress proxy on port 3128 (gateway.md, "Egress
// proxy", "Policy" and "Enforcement"; SV-85, SV-86, SV-87): an explicit
// proxy taking CONNECT host:port, and plain-HTTP requests with an
// absolute URI, from callers known by origin identity, held to their
// silo's effective egress policy.
//
// It checks the policy on the host the client named, then resolves the
// name itself and refuses if any address it resolves to is internal, so
// that a name can't be used to reach another silo, the gateway, the
// control VM or the metadata server. It dials one of the addresses it
// checked, never the name again, so the answer it judged is the one it
// uses. It doesn't terminate TLS: policy is on the CONNECT host only.
//
// Every connection is audited as egress.connect, through silod, with
// its silo, host, port, whether it was allowed, and its bytes each way.
// When new tables no longer allow an open connection's host, it is
// closed at once.

// Resolver resolves the names egress asks for. *net.Resolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// egressTimeout bounds resolving a name and dialing it.
const egressTimeout = 15 * time.Second

// egressConn is one open egress connection: a tunnel, or a plain-HTTP
// request in flight.
type egressConn struct {
	client     netip.Addr // the caller's source IP
	silo       string
	host       string // normalized, as the client named it
	port, def  int
	in, out    atomic.Int64 // bytes from upstream, and to it
	closedWhy  atomic.Pointer[string]
	closeOnce  sync.Once
	closeConns func()
}

func (c *egressConn) close(why string) {
	c.closedWhy.CompareAndSwap(nil, &why)
	c.closeOnce.Do(c.closeConns)
}

// Egress is port 3128's handler.
func (g *Gateway) Egress() http.Handler { return http.HandlerFunc(g.serveEgress) }

// egressRefusal is the proxy's own answer to a request it won't carry:
// plain text, as a proxy's error is, with the reason in a header too.
func egressRefusal(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Silo-Egress-Refusal", msg)
	w.WriteHeader(status)
	io.WriteString(w, msg+"\n")
}

func (g *Gateway) serveEgress(w http.ResponseWriter, r *http.Request) {
	var hostport string
	var def int
	switch {
	case r.Method == http.MethodConnect:
		hostport, def = r.Host, egress.DefaultConnectPort
	case r.URL.IsAbs() && r.URL.Scheme == "http":
		hostport, def = r.URL.Host, egress.DefaultHTTPPort
	case r.URL.IsAbs():
		egressRefusal(w, http.StatusBadRequest, fmt.Sprintf("silo: the egress proxy carries %s only by CONNECT", r.URL.Scheme))
		return
	default:
		egressRefusal(w, http.StatusBadRequest, fmt.Sprintf("silo: port %d is an explicit proxy: send CONNECT host:port, or a request with an absolute http:// URI", EgressPort))
		return
	}
	host, port, err := splitHostPort(hostport, def)
	if err != nil {
		egressRefusal(w, http.StatusBadRequest, "silo: "+err.Error())
		return
	}
	t := g.tables.Load()
	if t == nil {
		egressRefusal(w, http.StatusServiceUnavailable, "silo: the gateway has no origin table from silod yet; retry")
		return
	}
	ip, ok := sourceIP(r)
	if !ok {
		egressRefusal(w, http.StatusForbidden, "silo: the caller's address is not known")
		return
	}
	silo, subnet, ok := t.Resolve(ip)
	info, known := t.Silos[silo]
	if !ok || !known {
		egressRefusal(w, http.StatusForbidden, fmt.Sprintf("silo: %s is in no silo's address space", ip))
		return
	}
	if !subnet {
		egressRefusal(w, http.StatusForbidden, fmt.Sprintf("silo: %s is in %s's blocks but in no subnet of it", ip, info.Path))
		return
	}
	rec := EgressRecord{Silo: silo, Version: t.Version, Method: r.Method, Host: host, Port: port}
	if ok, why := egressAllowed(info, host, port, def); !ok {
		rec.Reason = why
		g.auditEgress(r.Context(), rec)
		egressRefusal(w, http.StatusForbidden, fmt.Sprintf("silo: egress from %s to %s is denied: %s", info.Path, net.JoinHostPort(host, strconv.Itoa(port)), why))
		return
	}
	upstream, addr, status, err := g.dialEgress(r.Context(), t, host, port)
	if err != nil {
		rec.Reason = err.Error()
		rec.Internal = status == http.StatusForbidden
		g.auditEgress(r.Context(), rec)
		egressRefusal(w, status, fmt.Sprintf("silo: egress from %s to %s: %v", info.Path, net.JoinHostPort(host, strconv.Itoa(port)), err))
		return
	}
	rec.Allowed, rec.Addr = true, addr
	ec := &egressConn{client: ip, silo: silo, host: host, port: port, def: def}
	if r.Method == http.MethodConnect {
		g.tunnel(w, r, ec, upstream, rec)
	} else {
		g.forward(w, r, ec, upstream, rec)
	}
}

// splitHostPort reads "host[:port]", with def when there is no port, and
// normalizes the host.
func splitHostPort(hostport string, def int) (string, int, error) {
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port.
		host, ps = hostport, strconv.Itoa(def)
	}
	host = egress.Normalize(host)
	port, err := strconv.Atoi(ps)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("bad port in %q", hostport)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		if _, err := egress.Parse(host); err != nil || strings.Contains(host, "*") {
			return "", 0, fmt.Errorf("bad host in %q", hostport)
		}
	}
	return host, port, nil
}

// egressAllowed is the effective policy's verdict on host and port, and
// why not when it isn't.
func egressAllowed(info Silo, host string, port, def int) (bool, string) {
	ok, i, by := egress.Allowed(info.Egress, host, port, def)
	if ok {
		return true, ""
	}
	if len(info.Egress) == 0 {
		return false, "the silo has no egress policy"
	}
	at := ""
	if i < len(info.EgressPaths) {
		at = info.EgressPaths[i]
	}
	if by != "" {
		return false, fmt.Sprintf("denied by %q in %s's egress policy", by, at)
	}
	return false, fmt.Sprintf("not allowed by %s's egress policy", at)
}

// dialEgress resolves host, refuses (403) if any address is internal,
// and dials the addresses in turn (502 if none answers).
func (g *Gateway) dialEgress(ctx context.Context, t *Tables, host string, port int) (net.Conn, string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, egressTimeout)
	defer cancel()
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		addrs, err = g.opts.Resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, "", http.StatusBadGateway, fmt.Errorf("resolving %s: %v", host, err)
		}
		if len(addrs) == 0 {
			return nil, "", http.StatusBadGateway, fmt.Errorf("%s has no addresses", host)
		}
	}
	for _, a := range addrs {
		if egress.Internal(a, t.VPC()) {
			return nil, "", http.StatusForbidden, fmt.Errorf("%s resolves to %s, an internal address", host, a.Unmap())
		}
	}
	var errs []error
	for _, a := range addrs {
		addr := net.JoinHostPort(a.Unmap().String(), strconv.Itoa(port))
		c, err := g.opts.DialEgress(ctx, "tcp", addr)
		if err == nil {
			return c, addr, 0, nil
		}
		errs = append(errs, err)
	}
	return nil, "", http.StatusBadGateway, fmt.Errorf("connecting: %v", errors.Join(errs...))
}

// track registers ec as open, unless the tables that came since it was
// checked no longer allow it; then it is closed at once.
func (g *Gateway) track(ec *egressConn) {
	g.mu.Lock()
	g.egress[ec] = struct{}{}
	t := g.tables.Load()
	g.mu.Unlock()
	if why := stillAllowed(t, ec); why != "" {
		ec.close(why)
	}
}

func (g *Gateway) untrack(ec *egressConn) {
	g.mu.Lock()
	delete(g.egress, ec)
	g.mu.Unlock()
}

// stillAllowed is "" if t allows ec, else why not.
func stillAllowed(t *Tables, ec *egressConn) string {
	silo, subnet, ok := t.Resolve(ec.client)
	info, known := t.Silos[silo]
	switch {
	case !ok || !known || !subnet:
		return "the client's address is no longer in a silo's subnet"
	case silo != ec.silo:
		return "the client's address moved to another silo"
	}
	if ok, why := egressAllowed(info, ec.host, ec.port, ec.def); !ok {
		return "the egress policy narrowed: " + why
	}
	return ""
}

// closeNarrowed closes, at once, every open egress connection cur no
// longer allows.
func (g *Gateway) closeNarrowed(cur *Tables) {
	g.mu.Lock()
	type hit struct {
		c   *egressConn
		why string
	}
	var doomed []hit
	for c := range g.egress {
		if why := stillAllowed(cur, c); why != "" {
			doomed = append(doomed, hit{c, why})
			delete(g.egress, c)
		}
	}
	g.mu.Unlock()
	for _, h := range doomed {
		h.c.close(h.why)
	}
	if len(doomed) > 0 {
		log.Printf("gateway: closed %d egress connections at version %d", len(doomed), cur.Version)
	}
}

// tunnel answers CONNECT and copies bytes both ways until either side
// ends, or the policy closes it.
func (g *Gateway) tunnel(w http.ResponseWriter, r *http.Request, ec *egressConn, upstream net.Conn, rec EgressRecord) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		egressRefusal(w, http.StatusInternalServerError, "silo: this connection can't carry a tunnel")
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	ec.closeConns = func() { client.Close(); upstream.Close() }
	g.track(ec)
	defer func() {
		g.untrack(ec)
		ec.close("")
		rec.BytesOut, rec.BytesIn = ec.out.Load(), ec.in.Load()
		if why := ec.closedWhy.Load(); why != nil && *why != "" {
			rec.Closed = *why
		}
		g.auditEgress(context.WithoutCancel(r.Context()), rec)
	}()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		// What the client sent after the CONNECT and is already read.
		n, _ := io.Copy(upstream, io.MultiReader(io.LimitReader(buf.Reader, int64(buf.Reader.Buffered())), client))
		ec.out.Add(n)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(client, upstream)
		ec.in.Add(n)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

// forward carries one plain-HTTP request over upstream, a connection
// already dialed to an address the checks passed.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, ec *egressConn, upstream net.Conn, rec EgressRecord) {
	cc := &countedConn{Conn: upstream, ec: ec}
	ec.closeConns = func() { upstream.Close() }
	g.track(ec)
	used := false
	tr := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used {
				return nil, errors.New("the egress connection is already used")
			}
			used = true
			return cc, nil
		},
	}
	var failed error
	rp := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *pr.In.URL
			pr.Out.URL = &u
			pr.Out.Host = pr.In.Host
			pr.Out.RequestURI = ""
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			failed = err
			egressRefusal(w, http.StatusBadGateway, fmt.Sprintf("silo: egress to %s: %v", rec.Host, err))
		},
	}
	defer func() {
		g.untrack(ec)
		ec.close("")
		// net/http writes a request concurrently with waiting for its
		// response (so a fast server needn't wait for a slow request
		// body), so ServeHTTP can return before cc's writer goroutine
		// has made its last call to Write and counted it. Close the
		// count before reading ec.out: that waits for any such call in
		// flight, which the close above has unblocked, and keeps any
		// later one off the wire, or the audit can report bytes_out as
		// 0 for a request that sent some.
		cc.finish()
		tr.CloseIdleConnections()
		rec.BytesOut, rec.BytesIn = ec.out.Load(), ec.in.Load()
		if why := ec.closedWhy.Load(); why != nil && *why != "" {
			rec.Closed = *why
		} else if failed != nil {
			rec.Closed = failed.Error()
		}
		g.auditEgress(context.WithoutCancel(r.Context()), rec)
	}()
	rp.ServeHTTP(w, r)
}

// countedConn counts an egress connection's bytes each way. finish
// ends the count: it waits for a Write in flight to finish and be
// counted, and every Write after it fails without writing, so that what
// was counted is final whether or not a Write had begun.
//
// (A WaitGroup can't do this: its Add, from zero, must happen before
// Wait, and the Transport's writer goroutine may not have called Write
// yet when forward is done with the request, as when the upstream
// answers without reading the body.)
type countedConn struct {
	net.Conn
	ec *egressConn
	mu sync.RWMutex // held for reading by each Write in flight
	// done is set by finish.
	done bool
}

func (c *countedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.ec.in.Add(int64(n))
	return n, err
}

func (c *countedConn) Write(b []byte) (int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.done {
		return 0, net.ErrClosed
	}
	n, err := c.Conn.Write(b)
	c.ec.out.Add(int64(n))
	return n, err
}

// finish waits for any Write in flight, which must be able to return
// (the connection closed, say), and stops later ones writing.
func (c *countedConn) finish() {
	c.mu.Lock()
	c.done = true
	c.mu.Unlock()
}

// EgressRecord is one egress connection, allowed or refused, which
// silod writes to the audit log as egress.connect.
type EgressRecord struct {
	Silo    string `json:"silo"`
	Version uint64 `json:"version"`
	Method  string `json:"method"` // CONNECT, or the plain-HTTP method
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Allowed bool   `json:"allowed"`
	// Internal is a refusal because the host resolved to an internal
	// address; Reason says why any refusal was made.
	Internal bool   `json:"internal,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Addr     string `json:"addr,omitempty"` // the address dialed
	// BytesOut went to the host, BytesIn came from it.
	BytesOut int64 `json:"bytes_out"`
	BytesIn  int64 `json:"bytes_in"`
	// Closed says why the gateway ended an allowed connection itself,
	// as when the policy narrowed.
	Closed string `json:"closed,omitempty"`
}

// auditEgress has silod record rec. A failure to is logged.
func (g *Gateway) auditEgress(ctx context.Context, rec EgressRecord) {
	b, _ := json.Marshal(rec)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://silod"+EgressAuditPath, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		log.Printf("gateway: auditing egress of %s to %s:%d: %v", rec.Silo, rec.Host, rec.Port, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("gateway: auditing egress of %s to %s:%d: silod answered %s", rec.Silo, rec.Host, rec.Port, resp.Status)
	}
}
