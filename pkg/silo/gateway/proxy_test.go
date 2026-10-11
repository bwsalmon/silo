package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// fakeResolver answers from names; a name it doesn't have resolves to
// public, a documentation address standing in for the internet.
type fakeResolver struct {
	mu    sync.Mutex
	names map[string][]netip.Addr
}

var public = netip.MustParseAddr("203.0.113.10")

func (r *fakeResolver) set(name string, addrs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var as []netip.Addr
	for _, a := range addrs {
		as = append(as, netip.MustParseAddr(a))
	}
	r.names[name] = as
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if as, ok := r.names[host]; ok {
		if len(as) == 0 {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return as, nil
	}
	return []netip.Addr{public}, nil
}

// upstream is the internet as far as the tests go: an echo server for
// tunnels, and an HTTP server, on ports 80 and 8080, that names the host and path
// it was asked for.
type upstream struct {
	echo   net.Listener
	web    *httptest.Server
	mu     sync.Mutex
	dialed []string
	// routes, set by route, sends a port somewhere other than echo or
	// web.
	routes map[uint16]string
}

// route has dialing port on the public address reach addr instead.
func (u *upstream) route(port uint16, addr string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.routes == nil {
		u.routes = map[uint16]string{}
	}
	u.routes[port] = addr
}

func newUpstream(t *testing.T) *upstream {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	u := &upstream{echo: l, web: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Connection") != "" || r.Header.Get("Proxy-Authorization") != "" {
			http.Error(w, "proxy headers leaked", http.StatusTeapot)
			return
		}
		fmt.Fprintf(w, "hello from %s%s", r.Host, r.URL.Path)
	}))}
	t.Cleanup(func() { l.Close(); u.web.Close() })
	return u
}

func (u *upstream) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	u.mu.Lock()
	u.dialed = append(u.dialed, addr)
	ap, err := netip.ParseAddrPort(addr)
	routed, isRouted := u.routes[ap.Port()]
	u.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if ap.Addr() != public {
		return nil, errors.New("connection refused")
	}
	to := u.echo.Addr().String()
	if isRouted {
		to = routed
	} else if ap.Port() == 80 || ap.Port() == 8080 {
		to = u.web.Listener.Addr().String()
	}
	var d net.Dialer
	return d.DialContext(ctx, network, to)
}

func (u *upstream) dials() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.dialed...)
}

// tunnel is a CONNECT through the proxy.
type tunnel struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

// connect sends CONNECT target from ip, and returns the status and,
// on 200, the tunnel.
func (e *env) connect(ip netip.Addr, target string) (int, string, *tunnel) {
	e.t.Helper()
	c, err := dialFrom(ctx, e.proxy, ip)
	if err != nil {
		e.t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		e.t.Fatalf("CONNECT %s: %v", target, err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		c.Close()
		return resp.StatusCode, string(b), nil
	}
	tn := &tunnel{t: e.t, c: c, r: br}
	e.t.Cleanup(func() { c.Close() })
	return 200, "", tn
}

// echo sends msg down the tunnel and reports whether it came back.
func (tn *tunnel) echo(msg string) bool {
	tn.c.SetDeadline(time.Now().Add(5 * time.Second))
	defer tn.c.SetDeadline(time.Time{})
	if _, err := io.WriteString(tn.c, msg); err != nil {
		return false
	}
	b := make([]byte, len(msg))
	if _, err := io.ReadFull(tn.r, b); err != nil {
		return false
	}
	return string(b) == msg
}

// closed reports whether the gateway closes the tunnel within d.
func (tn *tunnel) closed(d time.Duration) bool {
	tn.c.SetReadDeadline(time.Now().Add(d))
	defer tn.c.SetReadDeadline(time.Time{})
	_, err := tn.r.ReadByte()
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// get fetches rawURL through the proxy, from ip, as plain HTTP.
func (e *env) get(ip netip.Addr, rawURL string) (int, string) {
	e.t.Helper()
	proxy, _ := url.Parse("http://" + e.proxy)
	c := &http.Client{Transport: &http.Transport{
		Proxy:       http.ProxyURL(proxy),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dialFrom(ctx, e.proxy, ip) },
	}}
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// egressAudit is the egress.connect records' fields, oldest first.
func (e *env) egressAudit() []obj {
	e.t.Helper()
	var out []obj
	e.tx(func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT silo, result, fields FROM audit WHERE kind = ? ORDER BY n`, fenceapi.KindEgressConnect)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var silo, result, f string
			if err := rows.Scan(&silo, &result, &f); err != nil {
				return err
			}
			o := obj{}
			json.Unmarshal([]byte(f), &o)
			o["silo"], o["result"] = silo, result
			out = append(out, o)
		}
		return rows.Err()
	})
	return out
}

// waitAudit waits for an egress.connect record that match accepts.
func (e *env) waitAudit(match func(obj) bool) obj {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, r := range e.egressAudit() {
			if match(r) {
				return r
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no such egress.connect record; have %v", e.egressAudit())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEgressPolicy(t *testing.T) {
	e := newEnv(t, envOptions{})
	// a allows a subset of the root's, and denies one name its own
	// wildcard would let through.
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{
		"allow": []string{"*.example.com", "github.com:22", "*.test", "web.test:8080"},
		"deny":  []string{"bad.example.com"},
	}})
	e.sync()
	from := e.ip[e.a]
	for _, c := range []struct {
		target string
		want   int
	}{
		{"www.example.com:443", 200},
		{"a.b.example.com:443", 200},
		{"WWW.Example.COM.:443", 200},
		{"www.example.com", 200}, // no port is 443
		// A wildcard is a name below the domain, not the domain itself.
		{"example.com:443", 403},
		// A port-less entry is 443 on CONNECT; another port needs its own.
		{"www.example.com:8443", 403},
		{"www.example.com:80", 403},
		{"github.com:22", 200},
		{"github.com:23", 403},
		// The root allows github.com:443, but a doesn't.
		{"github.com:443", 403},
		// deny beats allow at one level, on every port.
		{"bad.example.com:443", 403},
		// Matching nothing is denied.
		{"pypi.org:443", 403},
		{"evil.org:443", 403},
	} {
		code, body, tn := e.connect(from, c.target)
		if code != c.want {
			t.Errorf("CONNECT %s from a: %d %q, want %d", c.target, code, body, c.want)
		}
		if tn != nil {
			if !tn.echo("ping " + c.target) {
				t.Errorf("CONNECT %s: the tunnel doesn't carry bytes", c.target)
			}
			tn.c.Close()
		}
	}
	for _, d := range e.upstream.dials() {
		if !strings.HasSuffix(d, ":443") && !strings.HasSuffix(d, ":22") {
			t.Errorf("the gateway dialed %s, which no allowed CONNECT names", d)
		}
	}

	// Plain HTTP: forwarded if Host is allowed, 80 being the default.
	if code, body := e.get(from, "http://www.example.com/pkg"); code != 200 || body != "hello from www.example.com/pkg" {
		t.Errorf("GET http://www.example.com/pkg: %d %q", code, body)
	}
	for _, u := range []string{"http://bad.example.com/", "http://www.example.com:8080/", "http://pypi.org/simple/"} {
		if code, body := e.get(from, u); code != 403 {
			t.Errorf("GET %s: %d %q, want 403", u, code, body)
		}
	}
	// An entry with a port allows it on plain HTTP too.
	if code, body := e.get(from, "http://web.test:8080/x"); code != 200 || body != "hello from web.test:8080/x" {
		t.Errorf("GET http://web.test:8080/x: %d %q, want 200", code, body)
	}
	if code, body := e.get(from, "http://other.test:8080/x"); code != 403 {
		t.Errorf("GET http://other.test:8080/x: %d %q, want 403", code, body)
	}

	// A caller in no silo's space, or in a block but no subnet, gets
	// nothing.
	if code, _, _ := e.connect(netip.MustParseAddr("192.0.2.1"), "www.example.com:443"); code != 403 {
		t.Errorf("CONNECT from outside every silo: %d, want 403", code)
	}
	if code, _, _ := e.connect(nth(e.blocks(e.a)[0], 65000), "www.example.com:443"); code != 403 {
		t.Errorf("CONNECT from a's block outside its subnets: %d, want 403", code)
	}
	// The proxy is only a proxy.
	resp, err := e.client(e.ip[e.a]).Get("http://" + e.proxy + "/")
	if err == nil {
		resp.Body.Close()
	}
	c, _ := dialFrom(ctx, e.proxy, from)
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: silo.internal\r\n\r\n")
	if r, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil || r.StatusCode != 400 {
		t.Errorf("GET / on the proxy port: %v %v, want 400", r, err)
	}
	c.Close()
}

func TestEgressIntersectionDownTheTree(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{
		"allow": []string{"*.example.com", "github.com"},
		"deny":  []string{"secret.example.com"},
	}})
	// kid allows all of example.com: its parent's deny still wins.
	e.ok(e.root, "PATCH", "/silo/v1/silos/a/kid", obj{"egress": obj{"allow": []string{"*.example.com"}}})
	e.sync()
	for _, c := range []struct {
		silo, target string
		want         int
	}{
		{e.kid, "www.example.com:443", 200},
		{e.kid, "secret.example.com:443", 403}, // a's deny
		{e.kid, "github.com:443", 403},         // a allows it; kid doesn't
		{e.a, "github.com:443", 200},
		{e.a, "x.test:443", 403}, // the root allows it; a doesn't
		{e.ro, "x.test:443", 403},
	} {
		code, body, tn := e.connect(e.ip[c.silo], c.target)
		if code != c.want {
			t.Errorf("CONNECT %s from %s: %d %q, want %d", c.target, c.silo, code, body, c.want)
		}
		if tn != nil {
			tn.c.Close()
		}
	}
	// The refusal names the level that denied it.
	_, body, _ := e.connect(e.ip[e.kid], "secret.example.com:443")
	if !strings.Contains(body, `"secret.example.com"`) || !strings.Contains(body, "/a's") {
		t.Errorf("refusal %q doesn't name a's deny entry", body)
	}
	// The tables carry each silo's levels, root first.
	tab := e.gw.Tables()
	if got := tab.Silos[e.kid].EgressPaths; !slices.Equal(got, []string{"/", "/a", "/a/kid"}) {
		t.Errorf("kid's egress levels are %v", got)
	}
	// A silo with no egress of its own allows nothing.
	if len(tab.Silos[e.ro].Egress) != 2 || len(tab.Silos[e.ro].Egress[1].Allow) != 0 {
		t.Errorf("ro's egress is %+v", tab.Silos[e.ro].Egress)
	}
}

func TestEgressRefusesInternalAddresses(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{"allow": []string{"*.test", "*.example.com"}}})
	e.sync()
	tab := e.gw.Tables()
	var ctl netip.Prefix
	for _, b := range tab.Blocks {
		if b.Silo == address.SystemControl {
			ctl = b.Range
		}
	}
	if !ctl.IsValid() {
		t.Fatal("the tables have no control range")
	}
	e.resolver.set("intranet.test", "10.20.30.40")
	e.resolver.set("control.test", nth(ctl, 5).String())
	e.resolver.set("peer.test", e.ip[e.ro].String())
	e.resolver.set("metadata.test", "169.254.169.254")
	e.resolver.set("home.test", "192.168.1.1")
	e.resolver.set("loop.test", "127.0.0.1")
	e.resolver.set("ula.test", "fd20:ce::254")
	e.resolver.set("mapped.test", "::ffff:10.0.0.1")
	// One internal address among public ones is enough.
	e.resolver.set("mixed.test", public.String(), "172.16.5.5")
	e.resolver.set("nowhere.test")
	before := len(e.upstream.dials())
	for _, name := range []string{"intranet.test", "control.test", "peer.test", "metadata.test", "home.test", "loop.test", "ula.test", "mapped.test", "mixed.test"} {
		code, body, tn := e.connect(e.ip[e.a], name+":443")
		if code != 403 || !strings.Contains(body, "internal address") {
			t.Errorf("CONNECT %s: %d %q, want 403 naming an internal address", name, code, body)
		}
		if tn != nil {
			tn.c.Close()
		}
		if code, body := e.get(e.ip[e.a], "http://"+name+"/"); code != 403 {
			t.Errorf("GET http://%s/: %d %q, want 403", name, code, body)
		}
	}
	if code, _, _ := e.connect(e.ip[e.a], "nowhere.test:443"); code != 502 {
		t.Errorf("CONNECT to a name with no addresses: %d, want 502", code)
	}
	if d := e.upstream.dials()[before:]; len(d) != 0 {
		t.Errorf("the gateway dialed %v for names it refused", d)
	}
	// The same name resolving publicly is fine.
	e.resolver.set("intranet.test", public.String())
	if code, body, tn := e.connect(e.ip[e.a], "intranet.test:443"); code != 200 {
		t.Errorf("CONNECT intranet.test resolving publicly: %d %q", code, body)
	} else {
		tn.c.Close()
	}
	// The refusal is audited, as internal.
	r := e.waitAudit(func(r obj) bool { return r["host"] == "control.test" && r["internal"] == true })
	if r["allowed"] != false || r["result"] != string(fenceapi.PermissionDenied) || r["silo"] != e.a {
		t.Errorf("internal refusal audited as %v", r)
	}
}

func TestEgressAudit(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{"allow": []string{"*.example.com"}}})
	e.sync()
	_, _, tn := e.connect(e.ip[e.a], "www.example.com:443")
	if tn == nil || !tn.echo("12345") || !tn.echo("67890abc") {
		t.Fatal("the tunnel doesn't carry bytes")
	}
	tn.c.Close()
	r := e.waitAudit(func(r obj) bool { return r["host"] == "www.example.com" && r["method"] == "CONNECT" })
	if r["allowed"] != true || r["result"] != "OK" || r["silo"] != e.a || r["port"] != float64(443) ||
		r["bytes_out"] != float64(13) || r["bytes_in"] != float64(13) || r["addr"] != public.String()+":443" {
		t.Errorf("allowed CONNECT audited as %v", r)
	}
	e.connect(e.ip[e.a], "evil.org:443")
	r = e.waitAudit(func(r obj) bool { return r["host"] == "evil.org" })
	if r["allowed"] != false || r["result"] != string(fenceapi.PermissionDenied) || r["port"] != float64(443) ||
		r["bytes_out"] != float64(0) || !strings.Contains(fmt.Sprint(r["reason"]), "egress policy") {
		t.Errorf("denied CONNECT audited as %v", r)
	}
	e.get(e.ip[e.a], "http://www.example.com/x")
	r = e.waitAudit(func(r obj) bool { return r["host"] == "www.example.com" && r["method"] == "GET" })
	if r["allowed"] != true || r["port"] != float64(80) || r["bytes_out"].(float64) <= 0 || r["bytes_in"].(float64) <= 0 {
		t.Errorf("plain HTTP audited as %v", r)
	}
}

func TestEgressNarrowingClosesTunnels(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{"allow": []string{"*.example.com", "github.com:22", "*.test"}}})
	e.ok(e.root, "PATCH", "/silo/v1/silos/a/kid", obj{"egress": obj{"allow": []string{"*.example.com", "github.com:22"}}})
	e.sync()
	open := func(silo, target string) *tunnel {
		t.Helper()
		code, body, tn := e.connect(e.ip[silo], target)
		if code != 200 || !tn.echo("hi") {
			t.Fatalf("CONNECT %s from %s: %d %q", target, silo, code, body)
		}
		return tn
	}
	kidWeb, kidGit, aWeb, aTest := open(e.kid, "www.example.com:443"), open(e.kid, "github.com:22"), open(e.a, "www.example.com:443"), open(e.a, "x.test:443")

	// The parent narrows: kid's own allow doesn't keep its tunnel open.
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"egress": obj{"allow": []string{"github.com:22", "*.test"}}})
	for name, tn := range map[string]*tunnel{"kid's to www.example.com": kidWeb, "a's to www.example.com": aWeb} {
		if !tn.closed(5 * time.Second) {
			t.Errorf("%s is still open after a's policy narrowed", name)
		}
	}
	for name, tn := range map[string]*tunnel{"kid's to github.com:22": kidGit, "a's to x.test": aTest} {
		if tn.closed(200*time.Millisecond) || !tn.echo("still here") {
			t.Errorf("%s was closed, though it is still allowed", name)
		}
	}
	r := e.waitAudit(func(r obj) bool { return r["silo"] == e.kid && r["host"] == "www.example.com" })
	if !strings.Contains(fmt.Sprint(r["closed"]), "narrowed") || r["allowed"] != true || r["bytes_in"] != float64(2) {
		t.Errorf("the closed tunnel is audited as %v", r)
	}
	// A deny added by the silo itself closes too.
	e.ok(e.a, "PATCH", "/silo/v1/self", obj{"egress": obj{"deny": []string{"x.test"}}})
	if !aTest.closed(5 * time.Second) {
		t.Error("a's tunnel to x.test is still open after a denied it itself")
	}
	// The root narrowing its own closes everything below it.
	e.ok(e.root, "PATCH", "/silo/v1/self", obj{"egress": obj{"allow": []string{"*.example.com", "github.com", "*.test"}}})
	if !kidGit.closed(5 * time.Second) {
		t.Error("kid's tunnel to github.com:22 is still open after the root narrowed")
	}
	// A new CONNECT is refused at once.
	if code, _, _ := e.connect(e.ip[e.kid], "github.com:22"); code != 403 {
		t.Errorf("CONNECT after narrowing: %d, want 403", code)
	}
}
