package gateway

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/ledger/address"
	"github.com/bwsalmon/silo/pkg/store"
)

// exportUpstream stands in for the exporters' VMs: every export target
// dialed reaches an HTTP server that reports what it was sent, on a
// target port of 8080, or a TCP echo server, on any other, which first
// reports the PROXY header it got, if any. listen opens the gateway's
// TCP export ports on local ports, whose callers say their source IP as
// fakeSource reads it.
type exportUpstream struct {
	web  net.Listener
	echo net.Listener

	mu     sync.Mutex
	dialed []string
	ports  map[int]string // gateway port -> local address
}

func newExportUpstream(t *testing.T) *exportUpstream {
	u := &exportUpstream{ports: map[int]string{}}
	var err error
	if u.web, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			w.WriteHeader(200)
			for {
				if _, err := w.Write([]byte("tick\n")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
		fmt.Fprintf(w, "path=%s caller=%s caller_path=%s other=%q",
			r.URL.RequestURI(), r.Header.Get(CallerHeader), r.Header.Get(CallerPathHeader), r.Header.Values("Silo-Caller-Extra"))
	})}
	go srv.Serve(u.web)
	if u.echo, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := u.echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { srv.Close(); u.echo.Close() })
	return u
}

func (u *exportUpstream) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	u.mu.Lock()
	u.dialed = append(u.dialed, addr)
	u.mu.Unlock()
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, err
	}
	to := u.echo.Addr().String()
	if ap.Port() == 8080 {
		to = u.web.Addr().String()
	}
	var d net.Dialer
	return d.DialContext(ctx, network, to)
}

func (u *exportUpstream) listen(port int) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.ports[port] = l.Addr().String()
	u.mu.Unlock()
	return &fakeSource{Listener: l}, nil
}

func (u *exportUpstream) addr(port int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.ports[port]
}

// from80 calls port 80 from ip.
func (e *env) from80(ip netip.Addr, path string, headers ...string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest("GET", "http://silo.internal"+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialFrom(ctx, e.web80, ip)
	}}}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// allowExports sets each silo's grant policy to allow exports, as the
// root.
func (e *env) allowExports(paths ...string) {
	for _, p := range paths {
		e.ok(e.root, "PATCH", "/silo/v1/silos/"+p, obj{"grant_policy": obj{"exports": "allow"}})
	}
}

// exportRule is the fence's export rule for export id, nil if none.
func (e *env) exportRule(id string) *fence.Grant {
	e.t.Helper()
	var out *fence.Grant
	e.tx(func(tx store.Tx) error {
		gs, err := fence.ExportGrants(ctx, tx)
		for i := range gs {
			if gs[i].Name == "export-"+id+"-in" {
				out = &gs[i]
			}
		}
		return err
	})
	return out
}

func (e *env) auditKinds(silo, kind string) []obj {
	e.t.Helper()
	recs, _ := e.ok(silo, "GET", "/silo/v1/self/audit?kind="+kind, nil)["records"].([]any)
	var out []obj
	for _, r := range recs {
		out = append(out, r.(obj))
	}
	return out
}

// waitExportAudit waits for silo to have n records of kind: the gateway
// audits a call after it ends, in the background.
func (e *env) waitExportAudit(silo, kind string, n int) []obj {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := e.auditKinds(silo, kind)
		if len(recs) >= n || time.Now().After(deadline) {
			return recs
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHTTPExport(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.allowExports("a", "a/kid", "ro")
	target := netip.AddrPortFrom(nth(e.subnet[e.a], 20), 8080)

	// Targets outside the exporter's own space are refused: a child's
	// space, the gateway and control ranges, another silo's.
	for _, bad := range []string{netip.AddrPortFrom(e.ip[e.kid], 80).String(), netip.AddrPortFrom(e.ip[e.ro], 80).String(), "8.8.8.8:80"} {
		code, out := e.as(e.a, "POST", "/silo/v1/self/exports", obj{"name": "x", "target": bad, "protocol": "http", "to": []string{"/a/kid"}})
		if code != 400 || fenceCode(out) != "INVALID_ARGUMENT" {
			t.Errorf("target %s: %d %v", bad, code, out)
		}
	}
	var gw, ct netip.Prefix
	e.tx(func(tx store.Tx) error {
		bs, err := address.AllBlocks(ctx, tx)
		gw, ct = bs[address.SystemGateway][0], bs[address.SystemControl][0]
		return err
	})
	if ct.IsValid() {
		code, out := e.as(e.root, "POST", "/silo/v1/self/exports", obj{"name": "x", "target": netip.AddrPortFrom(ct.Addr().Next(), 80).String(), "protocol": "http", "to": []string{"/a"}})
		if code != 400 {
			t.Errorf("a target in the control range: %d %v", code, out)
		}
	}

	// Offered, to kid and to a path that holds no silo: the answer is
	// the same either way.
	ex := e.ok(e.a, "POST", "/silo/v1/self/exports", obj{"name": "web", "target": target.String(), "protocol": "http", "to": []string{"/a/kid", "/nobody/here"}})
	id := ex["id"].(string)
	if ex["owner"] != "tenant" || len(ex["to"].([]any)) != 2 {
		t.Fatalf("offer: %v", ex)
	}
	for _, to := range ex["to"].([]any) {
		if to.(obj)["state"] != "PENDING" {
			t.Errorf("offer to %v: want PENDING", to)
		}
	}
	e.sync()
	path := "/x/" + e.a + "/web/hello?q=1"
	// Offered but not accepted: no path, and no fence rule.
	if code, body := e.from80(e.ip[e.kid], path); code != 404 {
		t.Errorf("before accept: %d %s", code, body)
	}
	if r := e.exportRule(id); r != nil {
		t.Errorf("a fence rule before accept: %+v", r)
	}
	offers := e.ok(e.kid, "GET", "/silo/v1/self/offers", nil)["offers"].([]any)
	if len(offers) != 1 || offers[0].(obj)["export"] != "/a/web" {
		t.Fatalf("kid's offers: %v", offers)
	}
	// Accepting what wasn't offered finds nothing, whether or not it
	// exists.
	for _, ref := range []string{"/a/web", "/a/nope"} {
		code, out := e.as(e.ro, "POST", "/silo/v1/silos/ro/imports", obj{"export": ref})
		if code != 404 && fenceCode(out) != "CONTROL_DENIED" {
			t.Errorf("ro accepting %s: %d %v", ref, code, out)
		}
		code, out = e.as(e.root, "POST", "/silo/v1/silos/ro/imports", obj{"export": ref})
		if code != 404 {
			t.Errorf("ro accepting %s, by the root: %d %v", ref, code, out)
		}
	}

	im := e.ok(e.kid, "POST", "/silo/v1/self/imports", obj{"export": "/a/web"})
	if want := "http://silo.internal/x/" + e.a + "/web/"; im["address"] != want || im["state"] != "ACTIVE" {
		t.Fatalf("accept: %v, want address %s", im, want)
	}
	if again := e.ok(e.kid, "POST", "/silo/v1/self/imports", obj{"export": "/a/web"}); again["id"] != im["id"] {
		t.Errorf("accepting twice made a second import: %v", again)
	}
	e.sync()
	r := e.exportRule(id)
	if r == nil || r.Direction != "INGRESS" || len(r.Src) != 1 || len(r.Dst) != 1 || r.Dst[0] != netip.PrefixFrom(target.Addr(), 32) || r.L4[0].Ports[0] != "8080" {
		t.Fatalf("fence rule: %+v", r)
	}
	if gw.IsValid() && r.Src[0] != gw {
		t.Errorf("fence rule source %v, want the gateway range %v", r.Src, gw)
	}

	// The call, with the prefix gone and the caller named; whatever
	// Silo-Caller* the caller sent is dropped.
	code, body := e.from80(e.ip[e.kid], path, "Silo-Caller", "evil", "Silo-Caller-Path", "/evil", "Silo-Caller-Extra", "x", "silo_caller", "y")
	if want := fmt.Sprintf(`path=/hello?q=1 caller=%s caller_path=/a/kid other=[]`, e.kid); code != 200 || body != want {
		t.Errorf("the call: %d %q, want %q", code, body, want)
	}
	// Nobody else gets through: not the exporter, not a silo with no
	// import.
	for _, s := range []string{e.a, e.ro} {
		if code, body := e.from80(e.ip[s], path); code != 404 {
			t.Errorf("from %s: %d %s", s, code, body)
		}
	}
	recs := e.waitExportAudit(e.a, "export.call", 1)
	if len(recs) == 0 {
		t.Fatalf("no export.call record")
	}
	f := recs[0]["fields"].(obj)
	if recs[0]["actor"].(obj)["silo"] != e.kid || f["method"] != "GET" || f["path"] != "/hello" || f["status"] != float64(200) || f["importer_path"] != "/a/kid" {
		t.Errorf("export.call: %v", recs[0])
	}
	if l := e.auditKinds(e.kid, "export.accept"); len(l) != 1 {
		t.Errorf("kid's export.accept records: %v", l)
	}

	// Revocation: a call in flight ends within a second of the export
	// being withdrawn, and the next is refused.
	req, _ := http.NewRequest("GET", "http://silo.internal/x/"+e.a+"/web/slow", nil)
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialFrom(ctx, e.web80, e.ip[e.kid])
	}}}
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("slow call: %v %v", resp, err)
	}
	ended := make(chan time.Time, 1)
	go func() {
		io.Copy(io.Discard, resp.Body)
		ended <- time.Now()
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	e.ok(e.a, "DELETE", "/silo/v1/self/exports/web", nil)
	select {
	case at := <-ended:
		if d := at.Sub(start); d > time.Second {
			t.Errorf("the call ended %v after the withdrawal", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the call outlived the withdrawal")
	}
	e.sync()
	if code, _ := e.from80(e.ip[e.kid], path); code != 404 {
		t.Errorf("after withdrawal: %d", code)
	}
	if r := e.exportRule(id); r != nil {
		t.Errorf("a fence rule after withdrawal: %+v", r)
	}
	if l := e.auditKinds(e.kid, "export.withdraw"); len(l) != 1 {
		t.Errorf("kid's export.withdraw records: %v", l)
	}
}

// TestTCPExportAndSuspension has the root offer ro's export on its
// behalf (ro is read-only), kid accept it, and an ancestor of kid's
// policy suspend and resume it.
func TestTCPExportAndSuspension(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.allowExports("a", "a/kid", "ro")
	target := netip.AddrPortFrom(nth(e.subnet[e.ro], 20), 5432)

	// ro can't offer for itself; the root can, for it.
	if code, out := e.from(e.ip[e.ro], "POST", "/silo/v1/self/exports", obj{"name": "db", "target": target.String(), "protocol": "tcp", "to": []string{"/a/kid"}}); code != 403 || fenceCode(out) != "CONTROL_DENIED" {
		t.Errorf("ro offering: %d %v", code, out)
	}
	e.ok(e.root, "POST", "/silo/v1/silos/ro/exports", obj{"name": "db", "target": target.String(), "protocol": "tcp", "to": []string{"/a/kid"}, "proxy_protocol": true})
	offer := e.auditKinds(e.ro, "export.offer")
	if len(offer) != 1 || offer[0]["actor"].(obj)["silo"] != e.root || offer[0]["via"] != "ancestor:"+e.root {
		t.Errorf("export.offer on ro's behalf: %v", offer)
	}
	im := e.ok(e.kid, "POST", "/silo/v1/self/imports", obj{"export": "/ro/db"})
	port := int(im["port"].(float64))
	if im["address"] != fmt.Sprintf("silo.internal:%d", port) || port < 20000 || port > 29999 {
		t.Fatalf("accept: %v", im)
	}
	addr := e.listening(port)

	// From kid: spliced, with a PROXY v2 header naming kid's address.
	conn, err := dialFrom(ctx, addr, e.ip[e.kid])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hdr := make([]byte, 28)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatalf("reading the PROXY header back: %v", err)
	}
	if !bytes.Equal(hdr[:12], proxySig) || hdr[12] != 0x21 || hdr[13] != 0x11 {
		t.Fatalf("PROXY header %x", hdr)
	}
	if src := netip.AddrFrom4([4]byte(hdr[16:20])); src != e.ip[e.kid] {
		t.Errorf("PROXY source %s, want kid's %s", src, e.ip[e.kid])
	}
	fmt.Fprintf(conn, "ping\n")
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo: %q %v", line, err)
	}
	// From anywhere else: refused.
	for _, s := range []string{e.a, e.ro} {
		other, err := dialFrom(ctx, addr, e.ip[s])
		if err != nil {
			t.Fatal(err)
		}
		other.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, err := other.Read(make([]byte, 1)); n != 0 || err == nil || isTimeout(err) {
			t.Errorf("a connection from %s was carried: %d %v", s, n, err)
		}
		other.Close()
	}

	// kid's ancestor a stops allowing exports: kid's own policy still
	// does, but the grant is suspended and the connection closed.
	start := time.Now()
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"grant_policy": obj{"exports": "deny"}})
	if d := closedWithin(conn, time.Second); d < 0 {
		t.Errorf("the connection outlived the suspension")
	} else if took := time.Since(start); took > 2*time.Second {
		t.Errorf("closing took %v", took)
	}
	imports := e.ok(e.kid, "GET", "/silo/v1/self/imports", nil)["imports"].([]any)
	if len(imports) != 1 || imports[0].(obj)["state"] != "SUSPENDED" {
		t.Fatalf("kid's imports after suspension: %v", imports)
	}
	e.sync()
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, ok := e.gw.TCPExportPorts()[port]; !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("the gateway still listens on %d while suspended", port)
			break
		}
	}
	sus := e.auditKinds(e.ro, "grant.suspend")
	if len(sus) != 1 || !strings.Contains(fmt.Sprint(sus[0]["fields"].(obj)["denied_by"]), e.kid) {
		t.Errorf("grant.suspend: %v", sus)
	}

	// Allowed again: resumed, on the same port.
	e.allowExports("a")
	e.sync()
	if l := e.auditKinds(e.kid, "grant.resume"); len(l) != 1 {
		t.Errorf("grant.resume: %v", l)
	}
	again, err := dialFrom(ctx, e.listening(port), e.ip[e.kid])
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(again, hdr); err != nil {
		t.Fatalf("after resuming: %v", err)
	}

	// The importer gives it up: closed within a second.
	if d := func() time.Duration {
		e.ok(e.kid, "DELETE", "/silo/v1/self/imports/"+im["id"].(string), nil)
		return closedWithin(again, time.Second)
	}(); d < 0 {
		t.Errorf("the connection outlived the import")
	}
	recs := e.waitExportAudit(e.ro, "export.call", 4)
	ok := 0
	for _, r := range recs {
		if r["result"] == "OK" && r["actor"].(obj)["silo"] == e.kid {
			ok++
		}
	}
	if ok < 2 {
		t.Errorf("export.call records: %v", recs)
	}
	// The offer stays: kid may accept it again.
	if l := e.ok(e.kid, "GET", "/silo/v1/self/offers", nil)["offers"].([]any); len(l) != 1 {
		t.Errorf("offers after giving up the import: %v", l)
	}
}

// listening waits for the gateway to listen on TCP export port port,
// which it opens just after it has the tables, and returns where.
func (e *env) listening(port int) string {
	e.t.Helper()
	e.sync()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := e.gw.TCPExportPorts()[port]; ok {
			return e.exp.addr(port)
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("the gateway isn't listening on port %d", port)
	return ""
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// closedWithin reads c until it closes, and returns how long that took,
// or -1 if it is still open after d.
func closedWithin(c net.Conn, d time.Duration) time.Duration {
	start := time.Now()
	c.SetReadDeadline(start.Add(d))
	b := make([]byte, 64)
	for {
		_, err := c.Read(b)
		if err == nil {
			continue
		}
		if isTimeout(err) {
			return -1
		}
		return time.Since(start)
	}
}
