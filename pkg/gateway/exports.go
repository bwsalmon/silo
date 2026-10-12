package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// This file is proxied exports (gateway.md, "Proxied exports", SV-83):
// HTTP exports on port 80 under /x/, and a TCP port per accepted tcp
// import. The gateway carries only what its tables list as an ACTIVE
// import, and only from the importer's own space. When a table arrives
// without an import (withdrawn by either side, or suspended by a grant
// policy), its calls in flight are cancelled and its connections
// closed at once: the tables follow the change log, so that is well
// within a second of the change committing.

// The headers an HTTP export's target gets, naming the caller. Any the
// caller sent are removed first (isSiloHeader).
const (
	CallerHeader     = "Silo-Caller"
	CallerPathHeader = "Silo-Caller-Path"
)

// ExportRecord is one HTTP call or TCP connection through an export,
// which silod writes to the audit log as export.call.
type ExportRecord struct {
	Export   string `json:"export"`
	Name     string `json:"name,omitempty"`
	Exporter string `json:"exporter"`
	Import   string `json:"import,omitempty"`
	Importer string `json:"importer"` // the caller's silo
	Protocol string `json:"protocol"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
	Method   string `json:"method,omitempty"`
	Path     string `json:"path,omitempty"`
	Status   int    `json:"status,omitempty"`
	BytesIn  int64  `json:"bytes_in"`  // from the caller
	BytesOut int64  `json:"bytes_out"` // to the caller
	Closed   string `json:"closed,omitempty"`
	Version  uint64 `json:"version"`
}

// exportState is the gateway's live export traffic.
type exportState struct {
	mu        sync.Mutex
	calls     map[*exportCall]struct{}
	listeners map[int]*tcpExport
	conns     map[*spliced]struct{}
	closed    bool
}

type exportCall struct {
	imp    string
	cancel context.CancelFunc
	revoke atomic.Bool
}

// tcpExport is one TCP export port's listener, for one import.
type tcpExport struct {
	port int
	imp  string
	l    net.Listener
}

// spliced is one TCP export connection.
type spliced struct {
	imp      string
	down     net.Conn
	revoked  atomic.Bool
	in, out  atomic.Int64
	mu       sync.Mutex
	up       net.Conn
	isClosed bool
}

func (c *spliced) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		return
	}
	c.isClosed = true
	c.down.Close()
	if c.up != nil {
		c.up.Close()
	}
}

// setUp records the connection to the export, false if c was closed
// meanwhile (and so up is closed too).
func (c *spliced) setUp(up net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed {
		up.Close()
		return false
	}
	c.up = up
	return true
}

func (g *Gateway) exportsInit() {
	g.ex.calls = map[*exportCall]struct{}{}
	g.ex.listeners = map[int]*tcpExport{}
	g.ex.conns = map[*spliced]struct{}{}
}

// revokeExports cancels the HTTP calls and closes the TCP connections
// of every import t no longer has, and moves the TCP export listeners to
// match t.
func (g *Gateway) revokeExports(t *Tables) {
	g.ex.mu.Lock()
	var cancels []*exportCall
	for c := range g.ex.calls {
		if !t.ImportLive(c.imp) {
			cancels = append(cancels, c)
		}
	}
	var doomed []*spliced
	for c := range g.ex.conns {
		if !t.ImportLive(c.imp) {
			doomed = append(doomed, c)
			delete(g.ex.conns, c)
		}
	}
	var stale []*tcpExport
	for port, l := range g.ex.listeners {
		if _, im := t.TCPExport(port); im == nil || im.ID != l.imp {
			stale = append(stale, l)
			delete(g.ex.listeners, port)
		}
	}
	var fresh []portEntry
	if !g.ex.closed {
		for port, pe := range t.byPort {
			if _, ok := g.ex.listeners[port]; !ok {
				fresh = append(fresh, pe)
			}
		}
	}
	g.ex.mu.Unlock()
	for _, c := range cancels {
		c.revoke.Store(true)
		c.cancel()
	}
	for _, c := range doomed {
		c.revoked.Store(true)
		c.close()
	}
	for _, l := range stale {
		l.l.Close()
	}
	if n := len(cancels) + len(doomed); n > 0 {
		log.Printf("gateway: revoked %d export calls and connections at version %d", n, t.Version)
	}
	for _, pe := range fresh {
		g.listenExport(pe.imp.Port, pe.imp.ID)
	}
}

// closeExports stops every TCP export listener and connection, as the
// gateway stops.
func (g *Gateway) closeExports() {
	g.ex.mu.Lock()
	g.ex.closed = true
	ls, cs := g.ex.listeners, g.ex.conns
	g.ex.listeners, g.ex.conns = map[int]*tcpExport{}, map[*spliced]struct{}{}
	g.ex.mu.Unlock()
	for _, l := range ls {
		l.l.Close()
	}
	for c := range cs {
		c.close()
	}
}

// TCPExportPorts is the ports the gateway listens on for TCP exports
// now, by port: the import each carries.
func (g *Gateway) TCPExportPorts() map[int]string {
	g.ex.mu.Lock()
	defer g.ex.mu.Unlock()
	out := map[int]string{}
	for p, l := range g.ex.listeners {
		out[p] = l.imp
	}
	return out
}

func (g *Gateway) listenExport(port int, imp string) {
	l, err := g.opts.ListenTCP(port)
	if err != nil {
		log.Printf("gateway: TCP export port %d: %v", port, err)
		return
	}
	te := &tcpExport{port: port, imp: imp, l: l}
	g.ex.mu.Lock()
	if _, taken := g.ex.listeners[port]; taken || g.ex.closed {
		g.ex.mu.Unlock()
		l.Close()
		return
	}
	g.ex.listeners[port] = te
	g.ex.mu.Unlock()
	// A table that arrived while the listener opened may already have
	// moved the port on.
	if t := g.tables.Load(); t != nil {
		if _, im := t.TCPExport(port); im == nil || im.ID != imp {
			g.revokeExports(t)
		}
	}
	go g.acceptExport(te)
}

func (g *Gateway) acceptExport(te *tcpExport) {
	for {
		c, err := te.l.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		go g.spliceExport(te, c)
	}
}

// spliceExport carries one TCP export connection: only from the
// importer's own space, and only while its import is ACTIVE.
func (g *Gateway) spliceExport(te *tcpExport, c net.Conn) {
	t := g.tables.Load()
	e, im := t.TCPExport(te.port)
	if e == nil || im.ID != te.imp {
		c.Close()
		return
	}
	rec := ExportRecord{Export: e.ID, Name: e.Name, Exporter: e.Exporter, Import: im.ID, Protocol: e.Protocol, Version: t.Version}
	ip, ok := addrOf(c.RemoteAddr())
	silo, subnet, known := t.Resolve(ip)
	rec.Importer = silo
	if !ok || !known || !subnet || silo != im.Importer {
		c.Close()
		rec.Reason = fmt.Sprintf("%s is not in the importer %s's own space", ip, im.ImporterPath)
		if known && subnet {
			go g.auditExport(rec)
		}
		return
	}
	sc := &spliced{imp: im.ID, down: c}
	g.ex.mu.Lock()
	g.ex.conns[sc] = struct{}{}
	g.ex.mu.Unlock()
	defer func() {
		g.ex.mu.Lock()
		delete(g.ex.conns, sc)
		g.ex.mu.Unlock()
		sc.close()
		rec.BytesIn, rec.BytesOut = sc.in.Load(), sc.out.Load()
		if sc.revoked.Load() {
			rec.Closed = "revoked"
		}
		go g.auditExport(rec)
	}()
	// Revoked between the lookup and the registration.
	if !g.tables.Load().ImportLive(im.ID) {
		rec.Closed = "revoked"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	up, err := g.opts.DialExport(ctx, "tcp", e.Target)
	cancel()
	if err != nil {
		rec.Reason = "dialing the export: " + err.Error()
		return
	}
	rec.Allowed = true
	if !sc.setUp(up) {
		return
	}
	if e.ProxyProtocol {
		if _, err := up.Write(proxyV2(c.RemoteAddr(), c.LocalAddr())); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	go func() { n, _ := io.Copy(up, c); sc.in.Add(n); done <- struct{}{} }()
	go func() { n, _ := io.Copy(c, up); sc.out.Add(n); done <- struct{}{} }()
	<-done
}

func addrOf(a net.Addr) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// proxySig is the PROXY protocol v2 signature.
var proxySig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// proxyV2 is a PROXY protocol v2 header saying the connection came from
// src to dst: for an export that asked for one, so the exporter learns
// the caller's address and not only the gateway's.
func proxyV2(src, dst net.Addr) []byte {
	var b bytes.Buffer
	b.Write(proxySig)
	s, ok1 := netip.ParseAddrPort(src.String())
	d, ok2 := netip.ParseAddrPort(dst.String())
	if ok1 != nil || ok2 != nil || !s.Addr().Unmap().Is4() || !d.Addr().Unmap().Is4() {
		b.Write([]byte{0x20, 0x00, 0x00, 0x00}) // LOCAL, UNSPEC
		return b.Bytes()
	}
	b.Write([]byte{0x21, 0x11, 0x00, 12}) // PROXY, TCP over IPv4, 12 bytes
	sa, da := s.Addr().Unmap().As4(), d.Addr().Unmap().As4()
	b.Write(sa[:])
	b.Write(da[:])
	binary.Write(&b, binary.BigEndian, s.Port())
	binary.Write(&b, binary.BigEndian, d.Port())
	return b.Bytes()
}

// serveExport serves /x/<exporter-id>/<name>/... on port 80: an HTTP
// export, for a caller whose silo holds an ACTIVE import of it.
func (g *Gateway) serveExport(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/x/")
	exporter, rest, _ := strings.Cut(rest, "/")
	name, rest, _ := strings.Cut(rest, "/")
	t := g.tables.Load()
	if t == nil {
		http.Error(w, "silo: the gateway has no tables from silod yet; retry", http.StatusServiceUnavailable)
		return
	}
	ip, ok := sourceIP(r)
	silo, subnet, known := t.Resolve(ip)
	if !ok || !known || !subnet {
		http.Error(w, fmt.Sprintf("silo: %s is in no silo's subnet", ip), http.StatusForbidden)
		return
	}
	e, im := t.HTTPExport(exporter, name, silo)
	if e == nil {
		// The same answer whether or not the export exists: a silo that
		// hasn't accepted it learns nothing of it.
		http.Error(w, fmt.Sprintf("silo: %s holds no accepted import of /x/%s/%s", t.Silos[silo].Path, exporter, name), http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	call := &exportCall{imp: im.ID, cancel: cancel}
	g.ex.mu.Lock()
	g.ex.calls[call] = struct{}{}
	g.ex.mu.Unlock()
	defer func() {
		g.ex.mu.Lock()
		delete(g.ex.calls, call)
		g.ex.mu.Unlock()
	}()
	rec := ExportRecord{Export: e.ID, Name: e.Name, Exporter: e.Exporter, Import: im.ID, Importer: silo, Protocol: e.Protocol,
		Allowed: true, Method: r.Method, Path: "/" + rest, Version: t.Version}
	// Revoked between the lookup and the registration.
	if !g.tables.Load().ImportLive(im.ID) {
		http.Error(w, "silo: the import was revoked", http.StatusForbidden)
		return
	}
	for k := range r.Header {
		if isSiloHeader(k) {
			r.Header.Del(k)
		}
	}
	r.Header.Set(CallerHeader, silo)
	r.Header.Set(CallerPathHeader, im.ImporterPath)
	body := &countingReader{r: r.Body}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = body
	}
	cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		rec.Status, rec.BytesIn, rec.BytesOut = cw.status, body.n, cw.n
		if call.revoke.Load() {
			rec.Closed = "revoked"
		}
		go g.auditExport(rec)
	}()
	target := e.Target
	proxy := &httputil.ReverseProxy{
		Transport: g.exportTransport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", target
			pr.Out.URL.Path, pr.Out.URL.RawPath = "/"+rest, ""
			pr.Out.Host = target
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if call.revoke.Load() {
				http.Error(w, "silo: the import was revoked", http.StatusForbidden)
				return
			}
			http.Error(w, "silo: the export is unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(cw, r.WithContext(ctx))
}

type countingReader struct {
	r io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

type countingWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (c *countingWriter) WriteHeader(s int) { c.status = s; c.ResponseWriter.WriteHeader(s) }

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// auditExport has silod record an export call. A failure to is logged.
func (g *Gateway) auditExport(rec ExportRecord) {
	b, _ := json.Marshal(rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://silod"+ExportAuditPath, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		log.Printf("gateway: auditing an export call of %s: %v", rec.Export, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("gateway: auditing an export call of %s: silod answered %s", rec.Export, resp.Status)
	}
}

// auditExport records one export call or connection as export.call,
// under the exporter: OK if it was carried, PERMISSION_DENIED if not.
func (s *silod) auditExport(w http.ResponseWriter, r *http.Request) {
	var rec ExportRecord
	if r.Method != http.MethodPost {
		notFound("silo: no such method").write(w, r.URL.Path)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil || rec.Exporter == "" {
		(&refusal{status: http.StatusBadRequest, code: fenceapi.InvalidArgument, message: "silo: bad export record"}).write(w, r.URL.Path)
		return
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		t, err := tree.Get(ctx, tx, rec.Exporter)
		if errors.Is(err, tree.ErrNotFound) {
			return nil // gone since; nothing to record it under
		}
		if err != nil {
			return err
		}
		result := "OK"
		if !rec.Allowed {
			result = string(fenceapi.PermissionDenied)
		}
		fields := map[string]any{
			"export": rec.Export, "name": rec.Name, "import": rec.Import, "importer": rec.Importer, "protocol": rec.Protocol,
			"bytes_in": rec.BytesIn, "bytes_out": rec.BytesOut, "origin_version": rec.Version,
		}
		if p, err := tree.Get(ctx, tx, rec.Importer); err == nil {
			fields["importer_path"] = p.Path
		}
		for k, v := range map[string]string{"method": rec.Method, "path": rec.Path, "reason": rec.Reason, "closed": rec.Closed} {
			if v != "" {
				fields[k] = v
			}
		}
		if rec.Status != 0 {
			fields["status"] = rec.Status
		}
		return fenceapi.Audit(ctx, tx, t, fenceapi.Caller{Silo: rec.Importer, Via: fenceapi.ViaOrigin},
			fenceapi.KindExportCall, "", result, fields)
	})
	if err != nil {
		unavailable("silo: recording an export call: "+err.Error()).write(w, r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
