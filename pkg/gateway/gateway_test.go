package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/fake"
	facade "github.com/bwsalmon/silo/pkg/facade/gcp/gce"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger/address"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/toolchan"
	"github.com/bwsalmon/silo/pkg/tree"
	compute "google.golang.org/api/compute/v1"
)

var ctx = context.Background()

const host = "host-project"

type obj = map[string]any

// env is silod on SQLite, serving the fence API and the facade over a
// fake GCE on its gateway socket, with the gateway in front of it. A
// test calls the gateway from any source IP it likes (from), and sets
// up the tree as silod's own caller (as), bypassing the gateway.
type env struct {
	t     *testing.T
	db    store.Database
	silod http.Handler // facade and fence API, the caller from the context
	gw    *Gateway
	// silodDown makes the gateway's dials to silod fail, as while silod
	// restarts or a replica's control VM is replaced.
	silodDown atomic.Bool
	api       string // the API port's address
	web       string // port 80's, an httptest server's URL
	web80     string // port 80 again, called from any source IP (from80)
	exp       *exportUpstream
	proxy     string // the egress proxy's address
	// resolver is what egress resolves names with, and upstream the
	// hosts it reaches: dialing an address the checks passed reaches
	// echo on any port but 80 and 8080, and web on those. dialed records each
	// address dialed.
	resolver *fakeResolver
	upstream *upstream
	// stopFollow stops the gateway following silod's tables, as a lagging
	// gateway would; it keeps what it has.
	stopFollow context.CancelFunc

	// The tree: root; a (full) and its child kid; ro (control=read);
	// sealed (control=none). Each has a subnet, and ip is a VM address
	// in it, by silo id.
	root, a, kid, ro, sealed string
	ip                       map[string]netip.Addr
	subnet                   map[string]netip.Prefix

	// The agents' side: the fence API itself, the fake GCE under the
	// facade, silod's tool channels and hub, port 80's silo-tools
	// directory, and each tool channel disconnect the hub reported.
	fence    *fenceapi.Server
	fakeGCE  *fake.Fake
	tools    *ToolChannels
	hub      *toolchan.Hub
	toolsDir string
	discMu   sync.Mutex
	discs    []string
}

type envOptions struct {
	gatewaySkipsControl bool // silod's check alone
	silodSkipsControl   bool // the gateway's check alone
	gatewaySkipsNow     func() bool
	silodWait           time.Duration
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	gce := fake.New(fake.Options{Project: host})
	if _, err := gce.InsertNetwork(ctx, host, &compute.Network{Name: "silo"}); err != nil {
		t.Fatal(err)
	}
	fc := facade.New(db, facade.Options{GCE: gce, HostProject: host, SkipControl: o.silodSkipsControl})
	// Agents' tool VMs are made through the facade, as in silod.
	api := fenceapi.New(db, fenceapi.Options{SkipControl: o.silodSkipsControl, ToolVMs: fc.ToolVMs(), Runner: &agents.Fake{},
		Credentials: agents.StaticCredentials{"anthropic-api": "anthropic"}})
	mux := http.NewServeMux()
	mux.Handle("/compute/v1/", fc)
	mux.Handle("/", api)
	e := &env{t: t, db: db, silod: mux, ip: map[string]netip.Addr{}, subnet: map[string]netip.Prefix{}, fence: api, fakeGCE: gce}
	e.hub = &toolchan.Hub{PingEvery: 200 * time.Millisecond, Connected: api.ToolServerConnected, Disconnected: e.disconnected}
	e.tools = &ToolChannels{Agent: api.ToolChannelAgent, Hub: e.hub}
	e.toolsDir = t.TempDir()

	// silod's gateway socket. A short path: Unix socket paths are short.
	dir, err := os.MkdirTemp("", "gw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	pub := NewPublisher(db)
	pctx, stopPub := context.WithCancel(ctx)
	go pub.Run(pctx)
	ssrv := &http.Server{Handler: Silod(db, SilodOptions{API: mux, Publisher: pub, Tools: e.tools})}
	go ssrv.Serve(l)

	e.resolver = &fakeResolver{names: map[string][]netip.Addr{}}
	e.upstream = newUpstream(t)
	e.exp = newExportUpstream(t)
	e.gw = New(Options{SkipControl: o.gatewaySkipsControl, SkipControlNow: o.gatewaySkipsNow, Retry: 50 * time.Millisecond, SilodWait: o.silodWait, Dial: func(ctx context.Context) (net.Conn, error) {
		if e.silodDown.Load() {
			return nil, errors.New("connect: connection refused")
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}, Resolver: e.resolver, DialEgress: e.upstream.dial, DialExport: e.exp.dial, ListenTCP: e.exp.listen, ToolsDir: e.toolsDir})
	fctx, stopFollow := context.WithCancel(ctx)
	e.stopFollow = stopFollow
	go e.gw.Follow(fctx)

	al, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	asrv := &http.Server{Handler: e.gw.API(), ConnState: e.gw.Track}
	go asrv.Serve(&fakeSource{Listener: al})
	e.api = al.Addr().String()
	web := httptest.NewServer(e.gw.HTTP())
	e.web = web.URL
	xl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	xsrv := &http.Server{Handler: e.gw.HTTP()}
	go xsrv.Serve(&fakeSource{Listener: xl})
	e.web80 = xl.Addr().String()
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	psrv := &http.Server{Handler: e.gw.Egress()}
	go psrv.Serve(&fakeSource{Listener: pl})
	e.proxy = pl.Addr().String()
	t.Cleanup(func() {
		psrv.Close()
		xsrv.Close()
		e.gw.closeExports()
		asrv.Close()
		web.Close()
		stopFollow()
		ssrv.Close()
		stopPub()
		fc.Close()
		api.Close()
	})

	code, root := e.as("", "POST", "/silod/bootstrap", obj{
		"regions":  []string{"us-central1"},
		"capacity": obj{"us-central1": obj{"vcpu": 100, "memory_mb": 409600, "ssd_disk_gb": 1000, "instances": 20, "standard_disk_gb": 2000}, "global": obj{"subnetworks": 20, "images": 10, "firewall_rules": 20}},
		"admins":   []string{"user:admin@example.com"},
		"egress":   obj{"allow": []string{"*.example.com", "github.com", "github.com:22", "*.test", "*.test:8080"}},
	})
	if code != 200 {
		t.Fatalf("bootstrap: %d %v", code, root)
	}
	e.root = root["id"].(string)
	quota := obj{"us-central1": obj{"vcpu": 4, "instances": 2, "memory_mb": 32768, "standard_disk_gb": 100}, "global": obj{"subnetworks": 3}}
	e.a = e.mk("-"+e.root, "a", 16, obj{"regions": []string{"us-central1"}, "quota": quota})
	e.kid = e.mk("a", "kid", 24, obj{"regions": []string{"us-central1"}, "quota": obj{"global": obj{"subnetworks": 1}}})
	e.ro = e.mk("-"+e.root, "ro", 16, obj{"regions": []string{"us-central1"}, "quota": quota})
	e.sealed = e.mk("-"+e.root, "sealed", 16, obj{"regions": []string{"us-central1"}, "quota": quota})
	for _, s := range []string{e.a, e.ro, e.sealed} {
		e.mkSubnet(s, false)
	}
	// kid's own range is recorded as the facade records a subnet's, with
	// no object behind it, so that kid can be deleted without force.
	e.mkSubnet(e.kid, true)
	// The policies last, since neither a read nor a sealed silo can make
	// its own subnet.
	e.ok(e.root, "PATCH", "/silo/v1/silos/ro", obj{"control": "read"})
	e.ok(e.root, "PATCH", "/silo/v1/silos/sealed", obj{"control": "none"})
	e.sync()
	return e
}

// mk creates a child of parent, as the root.
func (e *env) mk(parent, name string, bits int, extra obj) string {
	e.t.Helper()
	body := obj{"name": name, "address": obj{"prefix_length": bits}}
	for k, v := range extra {
		body[k] = v
	}
	return e.ok(e.root, "POST", "/silo/v1/silos/"+parent+"/children", body)["id"].(string)
}

// mkSubnet has silo make a /26 subnet, through the facade, in its own
// space, and picks the VM address .10 in it. ownOnly records just the
// subnet's own range, as the facade does.
func (e *env) mkSubnet(silo string, ownOnly bool) {
	e.t.Helper()
	blocks := e.blocks(silo)
	var taken []netip.Prefix
	for _, c := range e.childrenBlocks(silo) {
		taken = append(taken, c)
	}
	var p netip.Prefix
	for i := 0; ; i++ {
		p = sub(blocks[0], 26, i)
		free := true
		for _, t := range taken {
			free = free && !t.Overlaps(p)
		}
		if free {
			break
		}
	}
	if ownOnly {
		e.tx(func(tx store.Tx) error {
			return address.SetOwn(ctx, tx, silo, []address.OwnRange{{Object: "subnetworks/us-central1/web", Range: p}})
		})
	} else {
		e.ok(silo, "POST", "/compute/v1/projects/"+host+"/regions/us-central1/subnetworks", obj{"name": silo + "-web", "ipCidrRange": p.String()})
	}
	e.subnet[silo] = p
	e.ip[silo] = nth(p, 10)
}

func (e *env) blocks(silo string) []netip.Prefix {
	e.t.Helper()
	var out []netip.Prefix
	e.tx(func(tx store.Tx) error {
		var err error
		out, err = address.Blocks(ctx, tx, silo)
		return err
	})
	return out
}

func (e *env) childrenBlocks(silo string) []netip.Prefix {
	e.t.Helper()
	var out []netip.Prefix
	e.tx(func(tx store.Tx) error {
		cs, err := tree.Children(ctx, tx, silo)
		if err != nil {
			return err
		}
		for _, c := range cs {
			bs, err := address.Blocks(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			out = append(out, bs...)
		}
		return nil
	})
	return out
}

func (e *env) tx(fn func(store.Tx) error) {
	e.t.Helper()
	if err := e.db.Tx(ctx, fn); err != nil {
		e.t.Fatal(err)
	}
}

// sync waits for the gateway to have the tables as of now.
func (e *env) sync() {
	e.t.Helper()
	var v uint64
	e.tx(func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT seq FROM change_counter WHERE id = 1`).Scan(&v)
	})
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := e.gw.WaitVersion(c, v); err != nil {
		e.t.Fatalf("the gateway never got tables at version %d: %v", v, err)
	}
}

// as calls silod directly as caller (none if ""), the way silod's own
// tests do; it is for setting the scene, not for what is under test.
func (e *env) as(caller, method, path string, body any) (int, obj) {
	e.t.Helper()
	r := httptest.NewRequest(method, path, jsonBody(e.t, body))
	if caller != "" {
		r = r.WithContext(fenceapi.ContextWithCaller(r.Context(), fenceapi.Caller{Silo: caller, Via: fenceapi.ViaInternal}))
	}
	w := httptest.NewRecorder()
	e.silod.ServeHTTP(w, r)
	out := obj{}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (e *env) ok(caller, method, path string, body any) obj {
	e.t.Helper()
	code, out := e.as(caller, method, path, body)
	if code != 200 {
		e.t.Fatalf("%s %s as %s: %d %v", method, path, caller, code, out)
	}
	return out
}

func jsonBody(t *testing.T, body any) io.Reader {
	if body == nil {
		return nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

// client is an HTTP client whose connections to the API port come from
// ip.
func (e *env) client(ip netip.Addr) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialFrom(ctx, e.api, ip)
	}}}
}

// from calls the gateway's API port from ip, with headers.
func (e *env) from(ip netip.Addr, method, path string, body any, headers ...string) (int, obj) {
	e.t.Helper()
	req, err := http.NewRequest(method, "http://silo.internal:8080"+path, jsonBody(e.t, body))
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	resp, err := e.client(ip).Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := obj{}
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s %s from %s: %d, body %q is not JSON", method, path, ip, resp.StatusCode, b)
	}
	return resp.StatusCode, out
}

// fenceCode is a fence API answer's error code, "" for none.
func fenceCode(out obj) string {
	er, _ := out["error"].(obj)
	c, _ := er["code"].(string)
	return c
}

// gceReason is a facade answer's error reason and message.
func gceReason(out obj) (reason, msg string) {
	er, _ := out["error"].(obj)
	errs, _ := er["errors"].([]any)
	if len(errs) == 0 {
		return "", ""
	}
	it := errs[0].(obj)
	return it["reason"].(string), it["message"].(string)
}

// fakeSource is a listener whose connections say where they come from
// in a first line, "<ip>\n", as a test's stand-in for a VM's real
// source address: the gateway reads RemoteAddr as it would in GCE.
type fakeSource struct{ net.Listener }

func (l *fakeSource) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		var line []byte
		b := make([]byte, 1)
		for {
			if _, err := c.Read(b); err != nil || b[0] == '\n' || len(line) > 64 {
				break
			}
			line = append(line, b[0])
		}
		ip, err := netip.ParseAddr(string(line))
		if err != nil {
			c.Close()
			continue
		}
		return &sourced{Conn: c, from: net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, 40000))}, nil
	}
}

type sourced struct {
	net.Conn
	from net.Addr
}

func (c *sourced) RemoteAddr() net.Addr { return c.from }

func dialFrom(ctx context.Context, addr string, ip netip.Addr) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(c, "%s\n", ip); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// sub is the i-th /bits inside p.
func sub(p netip.Prefix, bits, i int) netip.Prefix {
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v += uint32(i) << (32 - bits)
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), bits)
}

// nth is the n-th address in p.
func nth(p netip.Prefix, n int) netip.Addr {
	a := p.Masked().Addr()
	for range n {
		a = a.Next()
	}
	return a
}

func TestOriginIdentity(t *testing.T) {
	e := newEnv(t, envOptions{})

	// The caller is the silo whose own space holds its source IP.
	for _, s := range []string{e.a, e.kid, e.ro} {
		code, out := e.from(e.ip[s], "GET", "/silo/v1/self", nil)
		if code != 200 || out["id"] != s {
			t.Errorf("self from %s: %d %v, want silo %s", e.ip[s], code, out, s)
		}
	}

	// A forged Silo-Origin, Silo-Caller and an Authorization header
	// change nothing: a's VM is a, whatever it claims.
	code, out := e.from(e.ip[e.a], "GET", "/silo/v1/self", nil,
		"Silo-Origin", e.root+";v=999999", "silo-origin", e.root+";v=999999", "Silo_Origin", e.root+";v=1",
		"Silo-Caller", e.root, "Silo-Caller-Path", "/", "Authorization", "Bearer root", "Cookie", "silo="+e.root)
	if code != 200 || out["id"] != e.a {
		t.Errorf("forged headers from a: %d %v, want a", code, out)
	}
	// On the facade too: a may not reach root's project-wide view by
	// claiming to be it.
	code, out = e.from(e.ip[e.a], "GET", "/compute/v1/projects/silo-"+e.root+"/regions/us-central1/subnetworks", nil,
		"Silo-Origin", e.root+";v=999999", "Authorization", "Bearer root")
	if r, _ := gceReason(out); code != 403 || r != "forbidden" {
		t.Errorf("a reading root's project with a forged origin: %d %v", code, out)
	}
	// From outside every block, forged headers get nothing.
	code, out = e.from(netip.MustParseAddr("192.0.2.7"), "GET", "/silo/v1/self", nil, "Silo-Origin", e.root+";v=999999", "Authorization", "Bearer root")
	if code != 403 || fenceCode(out) != "PERMISSION_DENIED" {
		t.Errorf("from no block: %d %v", code, out)
	}

	// An IP in a's block but in no subnet of it is no VM's: 403.
	var outside netip.Addr
	for i := 0; ; i++ {
		p := sub(e.blocks(e.a)[0], 26, i)
		if !p.Overlaps(e.subnet[e.a]) && !p.Overlaps(e.blocks(e.kid)[0]) {
			outside = nth(p, 10)
			break
		}
	}
	code, out = e.from(outside, "GET", "/silo/v1/self", nil)
	if code != 403 || !strings.Contains(out["error"].(obj)["message"].(string), "no subnet") {
		t.Errorf("from a's block outside its subnets: %d %v", code, out)
	}
	code, out = e.from(outside, "GET", "/compute/v1/projects/"+host+"/regions/us-central1/subnetworks", nil)
	if r, _ := gceReason(out); code != 403 || r != "forbidden" {
		t.Errorf("facade from a's block outside its subnets: %d %v", code, out)
	}

	// Paths that aren't the API's, or don't clean to themselves, never
	// leave the gateway: least of all silod's own.
	for _, p := range []string{"/silod/bootstrap", "/silod/gateway/tables", "/silo/v1/../silod/bootstrap", "/silo/v1//self"} {
		code, _ := e.from(e.ip[e.a], "GET", p, nil)
		if code != 404 {
			t.Errorf("GET %s on the API port: %d, want 404", p, code)
		}
	}
}

func TestStaleTableRefused(t *testing.T) {
	e := newEnv(t, envOptions{})
	if code, out := e.from(e.ip[e.kid], "GET", "/silo/v1/self", nil); code != 200 || out["id"] != e.kid {
		t.Fatalf("kid before: %d %v", code, out)
	}
	// The gateway stops hearing from silod, then kid's subnet and kid go:
	// its address is a's again, but the gateway's table still says kid.
	e.stopFollow()
	e.tx(func(tx store.Tx) error {
		return address.RemoveOwn(ctx, tx, e.kid, "subnetworks/us-central1/web")
	})
	e.ok(e.root, "DELETE", "/silo/v1/silos/a/kid", nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		e.tx(func(tx store.Tx) error { _, err = tree.Get(ctx, tx, e.kid); return nil })
		if errors.Is(err, tree.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kid was never deleted: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s, _, _ := e.gw.Tables().Resolve(e.ip[e.kid]); s != e.kid {
		t.Fatalf("the gateway's table moved on (%s); the test needs it stale", s)
	}
	code, out := e.from(e.ip[e.kid], "GET", "/silo/v1/self", nil)
	if code != 503 || fenceCode(out) != "UNAVAILABLE" || !strings.Contains(out["error"].(obj)["message"].(string), "older than change") {
		t.Errorf("a just-deleted child's IP on a stale table: %d %v", code, out)
	}
	code, out = e.from(e.ip[e.kid], "GET", "/compute/v1/projects/"+host+"/regions/us-central1/subnetworks", nil)
	if code != 503 {
		t.Errorf("facade, a just-deleted child's IP on a stale table: %d %v", code, out)
	}
	// Silos the change doesn't touch carry on with the old table.
	if code, out := e.from(e.ip[e.ro], "GET", "/silo/v1/self", nil); code != 200 || out["id"] != e.ro {
		t.Errorf("ro on the stale table: %d %v", code, out)
	}
}

func TestSilodBelievesOriginOnlyFromTheGateway(t *testing.T) {
	e := newEnv(t, envOptions{})
	h := Silod(e.db, SilodOptions{API: e.silod})
	tab := e.gw.Tables()
	for _, c := range []struct {
		name, path, origin string
		want               int
	}{
		{"malformed", "/silo/v1/self", e.a, 403},
		{"no version", "/silo/v1/self", e.a + ";v=", 403},
		{"silod's own path", "/silod/bootstrap", FormatOrigin(e.a, tab.Version), 404},
		{"good", "/silo/v1/self", FormatOrigin(e.a, tab.Version), 200},
	} {
		r := httptest.NewRequest("GET", c.path, nil)
		r.Header.Set(OriginHeader, c.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body, c.want)
		}
	}
	// silod's ordinary transports ignore Silo-Origin: with no caller of
	// their own, the call is refused.
	r := httptest.NewRequest("GET", "/silo/v1/self", nil)
	r.Header.Set(OriginHeader, FormatOrigin(e.root, tab.Version))
	w := httptest.NewRecorder()
	e.silod.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("Silo-Origin on silod's own socket: %d %s", w.Code, w.Body)
	}
}

func TestPort80(t *testing.T) {
	e := newEnv(t, envOptions{})
	get := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, e.web+path, nil)
		req.Header.Set(OriginHeader, FormatOrigin(e.root, e.gw.Tables().Version))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, p := range []string{"/silo/v1/self", "/silo/v1/silos/a/children", "/compute/v1/projects/" + host + "/zones", "/silo/v1/agents/-/tools", "/silo/v1/", "/compute/v1/"} {
		for _, m := range []string{"GET", "POST", "DELETE"} {
			code, body := get(m, p)
			if code != 404 || !strings.Contains(body, "8080") {
				t.Errorf("%s %s on port 80: %d %q, want 404 naming port 8080", m, p, code, body)
			}
		}
	}
	if code, body := get("GET", "/r/h/e/"); code != 404 || !strings.Contains(body, "reserved") {
		t.Errorf("/r/ on port 80: %d %q", code, body)
	}
	// The tool endpoints have handlers of their own (tools.go,
	// TestToolsEndpointsIsolated), which need a caller in a silo's
	// subnet: a Silo-Origin header makes no caller one.
	for _, p := range []string{ToolsAttachPath, ToolsBinaryPath + "?arch=amd64"} {
		if code, body := get("GET", p); code != 403 {
			t.Errorf("%s on port 80 from outside every silo: %d %q, want 403", p, code, body)
		}
	}
	// An export from an address in no silo's subnet is refused.
	if code, _ := get("GET", "/x/exporter/name/"); code != 403 {
		t.Errorf("/x/ on port 80 from outside every silo: %d, want 403", code)
	}
	// Nothing on port 80 changed anything.
	var n int
	e.tx(func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM silos`).Scan(&n)
	})
	if n != 5 {
		t.Errorf("silos after port 80 calls: %d", n)
	}
}

// TestControl checks every Layer 1 refusal three ways: both layers,
// the gateway's alone, and silod's alone.
func TestControl(t *testing.T) {
	for _, mode := range []struct {
		name string
		o    envOptions
	}{
		{"both", envOptions{}},
		{"gateway-alone", envOptions{silodSkipsControl: true}},
		{"silod-alone", envOptions{gatewaySkipsControl: true}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			e := newEnv(t, mode.o)
			cv := "/compute/v1/projects/" + host + "/"
			type call struct {
				silo, method, path string
				body               any
				refused            bool
			}
			calls := []call{
				// none: every call, reads included.
				{e.sealed, "GET", "/silo/v1/self", nil, true},
				{e.sealed, "GET", "/silo/v1/silos/sealed/children", nil, true},
				{e.sealed, "POST", "/silo/v1/quota:preview", obj{}, true},
				{e.sealed, "GET", cv + "regions/us-central1/subnetworks", nil, true},
				{e.sealed, "POST", cv + "zones/us-central1-a/operations/x/wait", nil, true},
				{e.sealed, "POST", cv + "regions/us-central1/subnetworks", obj{"name": e.sealed + "-x", "ipCidrRange": "10.0.0.0/26"}, true},
				// read: mutations, and anything the table doesn't know.
				{e.ro, "POST", "/silo/v1/silos/ro/children", obj{"name": "c", "address": obj{"prefix_length": 24}}, true},
				{e.ro, "PATCH", "/silo/v1/self", obj{"labels": obj{"x": "y"}}, true},
				{e.ro, "POST", "/silo/v1/self:frobnicate", obj{}, true},
				{e.ro, "POST", "/silo/v1/self/exports", obj{}, true},
				{e.ro, "POST", cv + "regions/us-central1/subnetworks", obj{"name": e.ro + "-x", "ipCidrRange": e.subnet[e.ro].String()}, true},
				{e.ro, "DELETE", cv + "regions/us-central1/subnetworks/" + e.ro + "-web", nil, true},
				{e.ro, "POST", cv + "zones/us-central1-a/instances/" + e.ro + "-vm:frobnicate", obj{}, true},
				// read: reads, by method and not verb, go through.
				{e.ro, "GET", "/silo/v1/self", nil, false},
				{e.ro, "POST", "/silo/v1/quota:preview", obj{}, false},
				{e.ro, "GET", cv + "regions/us-central1/subnetworks", nil, false},
				// full: anything.
				{e.a, "PATCH", "/silo/v1/self", obj{"labels": obj{"x": "y"}}, false},
			}
			for _, c := range calls {
				code, out := e.from(e.ip[c.silo], c.method, c.path, c.body)
				facadeCall := strings.HasPrefix(c.path, "/compute/")
				reason, msg := gceReason(out)
				denied := code == 403 && (fenceCode(out) == "CONTROL_DENIED" || (facadeCall && reason == "forbidden" && strings.Contains(msg, "control-plane calls from")))
				if denied != c.refused {
					t.Errorf("%s %s from %s: %d %v; refused=%v, want %v", c.method, c.path, c.silo, code, out, denied, c.refused)
				}
			}
			// Each refusal is audited as control.denied, by the layer that
			// made it.
			var gw, sd int
			e.tx(func(tx store.Tx) error {
				rows, err := tx.Query(ctx, `SELECT fields FROM audit WHERE kind = ?`, fenceapi.KindControlDenied)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var f string
					rows.Scan(&f)
					if strings.Contains(f, `"layer":"gateway"`) {
						gw++
					} else {
						sd++
					}
				}
				return rows.Err()
			})
			refused := 0
			for _, c := range calls {
				if c.refused {
					refused++
				}
			}
			switch mode.name {
			case "silod-alone":
				if gw != 0 || sd != refused {
					t.Errorf("audit: %d by the gateway, %d by silod; want 0, %d", gw, sd, refused)
				}
			default:
				if gw != refused || sd != 0 {
					t.Errorf("audit: %d by the gateway, %d by silod; want %d, 0", gw, sd, refused)
				}
			}
		})
	}
}

// openConn is a keep-alive connection to the API port, with one call
// made on it.
type openConn struct {
	c  net.Conn
	br *bufio.Reader
}

func (e *env) open(ip netip.Addr) *openConn {
	e.t.Helper()
	c, err := dialFrom(ctx, e.api, ip)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close() })
	o := &openConn{c: c, br: bufio.NewReader(c)}
	fmt.Fprintf(c, "GET /silo/v1/self HTTP/1.1\r\nHost: silo.internal:8080\r\n\r\n")
	resp, err := http.ReadResponse(o.br, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		e.t.Fatalf("a call on a new connection from %s: %d", ip, resp.StatusCode)
	}
	return o
}

// closed waits for the gateway to close the connection.
func (o *openConn) closed(within time.Duration) bool {
	o.c.SetReadDeadline(time.Now().Add(within))
	_, err := o.br.ReadByte()
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

func TestTighteningClosesConnections(t *testing.T) {
	e := newEnv(t, envOptions{})
	a, kid, ro := e.open(e.ip[e.a]), e.open(e.ip[e.kid]), e.open(e.ip[e.ro])
	// a goes to read: a and kid, whose effective policy follows, lose
	// their API-port connections at once; ro keeps its own.
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"control": "read"})
	if !a.closed(5 * time.Second) {
		t.Error("a's connection is still open after a was tightened")
	}
	if !kid.closed(5 * time.Second) {
		t.Error("kid's connection is still open after its parent was tightened")
	}
	if ro.closed(200 * time.Millisecond) {
		t.Error("ro's connection was closed, though nothing about ro changed")
	}
	// Loosening closes nothing.
	e.sync()
	a = e.open(e.ip[e.a])
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"control": "full"})
	e.sync()
	if a.closed(200 * time.Millisecond) {
		t.Error("loosening a closed its connection")
	}
}

func TestEndedGrantClosesHolderConnections(t *testing.T) {
	e := newEnv(t, envOptions{})
	g := e.ok(e.root, "POST", "/silo/v1/silos/ro/manageGrants", obj{"name": "ops", "holder": "/a", "lease": obj{"ttl": "3600s"}})
	id := g["id"].(string)
	e.ok(e.a, "POST", "/silo/v1/self/manageGrants/"+id+":accept", obj{})
	e.sync()
	if _, ok := e.gw.Tables().Grants[id]; !ok {
		t.Fatalf("the gateway's grant table lacks %s: %v", id, e.gw.Tables().Grants)
	}
	a, target := e.open(e.ip[e.a]), e.open(e.ip[e.ro])
	e.ok(e.root, "DELETE", "/silo/v1/silos/ro/manageGrants/ops", nil)
	if !a.closed(5 * time.Second) {
		t.Error("the holder's connection is still open after its grant was revoked")
	}
	if target.closed(200 * time.Millisecond) {
		t.Error("the target's connection was closed")
	}
}

// TestTablesSurviveSilodRestart: with silod's stream gone, the gateway
// serves what it has.
func TestTablesSurviveSilodRestart(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.stopFollow()
	v := e.gw.Tables().Version
	// A table older than the gateway's is refused.
	if e.gw.SetTables(&Tables{Version: v - 1}) {
		t.Error("the gateway took an older table")
	}
	if code, out := e.from(e.ip[e.a], "GET", "/silo/v1/self", nil); code != 200 || out["id"] != e.a {
		t.Errorf("with silod's stream gone: %d %v", code, out)
	}
}
