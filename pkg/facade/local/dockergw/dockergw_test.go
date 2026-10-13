package dockergw

import (
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
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime/fake"
	"github.com/bwsalmon/silo/pkg/facade/local/docker"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

var ctx = context.Background()

// env is a store with a root and a child, team-a, and the facade over
// fake drivers.
type env struct {
	t       *testing.T
	db      store.Database
	facade  *docker.Server
	root, a string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	caller := func(r *http.Request) (fenceapi.Caller, error) {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
	}
	api := fenceapi.New(db, fenceapi.Options{Caller: caller})
	t.Cleanup(api.Close)
	e := &env{t: t, db: db, facade: docker.New(db, docker.Options{Drivers: []runtime.Driver{fake.New(runtime.Sandbox)}, Version: "test"})}
	t.Cleanup(e.facade.Close)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	call := func(caller, method, path string, body any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Silo-Caller", caller)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out := map[string]any{}
		json.NewDecoder(res.Body).Decode(&out)
		if res.StatusCode != 200 {
			t.Fatalf("%s %s: %d %v", method, path, res.StatusCode, out)
		}
		return out
	}
	root := call("", "POST", "/silod/bootstrap", map[string]any{
		"backing":  map[string]any{"kind": "local"},
		"capacity": map[string]any{"global": map[string]any{"vcpu_m": 32000, "memory_mb": 32768, "disk_gb": 1000, "containers": 50, "networks": 20, "volumes": 20}},
		"admins":   []string{"user:admin@example.com"},
	})
	e.root = root["id"].(string)
	a := call(e.root, "POST", "/silo/v1/silos/-"+e.root+"/children", map[string]any{"name": "team-a", "address": map[string]any{"prefix_length": 20},
		"quota": map[string]any{"global": map[string]any{"networks": 3}}})
	e.a = a["id"].(string)
	return e
}

// member maps uid to silo, as silo members put does.
func (e *env) member(uid uint32, silo string) {
	e.t.Helper()
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO silo_members (identity, silo, updated_at) VALUES (?, ?, ?)`,
			fenceapi.UIDIdentity(uid), silo, store.Millis(time.Now()))
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// unixClient is an HTTP client on the Unix socket at path.
func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

func get(t *testing.T, c *http.Client, method, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://docker"+path, nil)
	req.Header.Set("Silo-Caller", "forged")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// TestSockets: a socket per silo, mode 0666 in a directory of silod's
// own; the caller is the peer uid's member silo and the target the
// socket's silo, and an unmapped uid gets only /_ping.
func TestSockets(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(t.TempDir(), "docker")
	var uid atomic.Uint32
	uid.Store(4242)
	s := &Sockets{Dir: dir, Store: e.db, Handler: e.facade, PeerUID: func(c net.Conn) (uint32, error) {
		if _, err := PeerCred(c); err != nil {
			return 0, err
		}
		return uid.Load(), nil
	}}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, id := range []string{e.root, e.a} {
		fi, err := os.Stat(s.Path(id))
		if err != nil || fi.Mode().Perm() != 0o666 || fi.Mode()&os.ModeSocket == 0 {
			t.Errorf("%s: %v %v", s.Path(id), fi, err)
		}
	}
	root, a := unixClient(s.Path(e.root)), unixClient(s.Path(e.a))
	if code, _ := get(t, root, "GET", "/_ping"); code != 200 {
		t.Errorf("/_ping from an unmapped uid: %d", code)
	}
	if code, body := get(t, root, "GET", "/v1.47/networks"); code != 403 || !strings.Contains(body, "uid:4242 is not a member") {
		t.Errorf("networks from an unmapped uid: %d %s", code, body)
	}
	// A member of the root reaches the child's socket, whose target is
	// the child.
	e.member(uid.Load(), e.root)
	req, _ := http.NewRequest("POST", "http://docker/v1.47/networks/create", strings.NewReader(`{"Name":"back"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := a.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 201 && res.StatusCode != 200 {
		t.Fatalf("network create on the child's socket: %d %s", res.StatusCode, body)
	}
	if code, body := get(t, a, "GET", "/v1.47/networks"); code != 200 || !strings.Contains(body, `"back"`) {
		t.Errorf("the child's networks: %d %s", code, body)
	}
	if code, body := get(t, root, "GET", "/v1.47/networks"); code != 200 || strings.Contains(body, `"back"`) {
		t.Errorf("the root's networks: %d %s", code, body)
	}
	// A member of the child can't reach the root's socket. The peer's
	// uid is its connection's, so it connects anew.
	uid.Store(4343)
	e.member(uid.Load(), e.a)
	if code, _ := get(t, unixClient(s.Path(e.root)), "GET", "/v1.47/networks"); code != 404 {
		t.Errorf("the root's socket from the child's member: %d", code)
	}
	// A socket a silo no longer has goes with it.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM silos WHERE id = ?`, e.a)
		return err
	}); err == nil {
		if err := s.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(s.Path(e.a)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a deleted silo's socket: %v", err)
		}
	}
}

// TestSocketDirOwner: a directory another user owns is refused.
func TestSocketDirOwner(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns every directory it makes")
	}
	e := newEnv(t)
	s := &Sockets{Dir: "/", Store: e.db, Handler: e.facade}
	if err := s.Sync(ctx); err == nil || !strings.Contains(err.Error(), "belongs to uid") {
		t.Errorf("a directory of root's: %v", err)
	}
}

// TestByOrigin: the caller is its source address's silo, and an
// address that is none's gets only /_ping.
func TestByOrigin(t *testing.T) {
	e := newEnv(t)
	var silo string
	srv := httptest.NewServer(ByOrigin(e.facade, func(a netip.Addr) (string, bool) {
		return silo, silo != "" && a.IsLoopback()
	}))
	t.Cleanup(srv.Close)
	c := srv.Client()
	call := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Silo-Caller", e.root)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, _ := call("/_ping"); code != 200 {
		t.Errorf("/_ping from no silo: %d", code)
	}
	if code, body := call("/v1.47/info"); code != 403 {
		t.Errorf("info from no silo: %d %s", code, body)
	}
	silo = e.a
	if code, body := call("/v1.47/info"); code != 200 || !strings.Contains(body, `"OperatingSystem"`) {
		t.Errorf("info from team-a: %d %s", code, body)
	}
}

// lookup is a fake facade Lookup: web is 10.0.0.5 in silo s1.
type lookup struct{}

func (lookup) Lookup(_ context.Context, _ store.Tx, names docker.Names, silo string, _ netip.Addr, name string) (docker.Answer, error) {
	switch {
	case name == "silo.internal.":
		return docker.Answer{Addrs: []netip.Addr{names.API}}, nil
	case name == "web." && silo == "s1":
		return docker.Answer{Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.5")}}, nil
	}
	return docker.Answer{}, nil
}

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b, err := (&dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}}}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestDNS: A queries are Lookup's answers, as the source's silo; an
// unknown name is NXDOMAIN, another type for a known name is empty,
// and a source that is no silo's is refused. It is served over UDP and
// TCP.
func TestDNS(t *testing.T) {
	e := newEnv(t)
	d := &DNS{Store: e.db, Facade: lookup{}, Names: docker.Names{API: netip.MustParseAddr("10.255.255.1")},
		Origin: func(a netip.Addr) (string, bool) { return "s1", a.IsLoopback() }}
	from := netip.MustParseAddr("127.0.0.1")
	answer := func(from netip.Addr, name string, typ dnsmessage.Type) dnsmessage.Message {
		t.Helper()
		b, err := d.Answer(ctx, from, query(t, name, typ))
		if err != nil {
			t.Fatal(err)
		}
		var m dnsmessage.Message
		if err := m.Unpack(b); err != nil {
			t.Fatal(err)
		}
		if m.ID != 7 || !m.Response {
			t.Errorf("%s: header %+v", name, m.Header)
		}
		return m
	}
	addrs := func(m dnsmessage.Message) []string {
		var out []string
		for _, a := range m.Answers {
			out = append(out, netip.AddrFrom4(a.Body.(*dnsmessage.AResource).A).String())
		}
		return out
	}
	if m := answer(from, "web.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeSuccess || fmt.Sprint(addrs(m)) != "[10.0.0.5]" {
		t.Errorf("web: %v %v", m.RCode, addrs(m))
	}
	if m := answer(from, "silo.internal.", dnsmessage.TypeA); fmt.Sprint(addrs(m)) != "[10.255.255.1]" {
		t.Errorf("silo.internal: %v", addrs(m))
	}
	if m := answer(from, "example.com.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("example.com: %v", m.RCode)
	}
	if m := answer(from, "web.", dnsmessage.TypeAAAA); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Errorf("web AAAA: %v %v", m.RCode, m.Answers)
	}
	if m := answer(from, "example.com.", dnsmessage.TypeAAAA); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("example.com AAAA: %v", m.RCode)
	}
	if m := answer(netip.MustParseAddr("192.0.2.1"), "web.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeRefused {
		t.Errorf("from no silo: %v", m.RCode)
	}

	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close(); l.Close() })
	go d.ServeUDP(ctx, pc)
	go d.ServeTCP(ctx, l)
	for _, network := range []string{"udp", "tcp"} {
		addr := pc.LocalAddr().String()
		if network == "tcp" {
			addr = l.Addr().String()
		}
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
		got, err := r.LookupIP(ctx, "ip4", "web")
		if err != nil || fmt.Sprint(got) != "[10.0.0.5]" {
			t.Errorf("over %s: %v %v", network, got, err)
		}
	}
}

// TestPorts: a published port answers only its silo's callers and
// forwards to their container; two silos share a port; the host
// listener is audited, refused when taken, and every listener closes
// with its container.
func TestPorts(t *testing.T) {
	echo := func(tag string) netip.AddrPort {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); io.WriteString(c, tag) }()
			}
		}()
		return netip.MustParseAddrPort(l.Addr().String())
	}
	t1, t2 := echo("one"), echo("two")
	var mu sync.Mutex
	inner := map[int]net.Listener{}
	host := map[int]net.Listener{}
	var audits []string
	silo := "s1"
	takenHost := map[int]bool{}
	p := &Ports{
		Origin: func(netip.Addr) (string, bool) { mu.Lock(); defer mu.Unlock(); return silo, silo != "" },
		ListenSiloNet: func(port int) (net.Listener, error) {
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			mu.Lock()
			inner[port] = l
			mu.Unlock()
			return l, err
		},
		ListenHost: func(port int) (net.Listener, error) {
			if takenHost[port] {
				return nil, &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
			}
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			host[port] = l
			return l, err
		},
		Dial: func(ctx context.Context, target netip.AddrPort) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", target.String())
		},
		Audit: func(_ context.Context, pp docker.Published, event string) {
			audits = append(audits, fmt.Sprintf("%s %s %d", event, pp.Silo, pp.Port))
		},
	}
	read := func(l net.Listener) string {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			return "refused"
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(c)
		return string(b)
	}
	if err := p.Open(ctx, []docker.Published{{Silo: "s1", Proto: "tcp", Port: 8080, Target: t1, Host: true, Container: "c1"},
		{Silo: "s1", Proto: "udp", Port: 53, Target: t1, Container: "c1"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Open(ctx, []docker.Published{{Silo: "s2", Proto: "tcp", Port: 8080, Target: t2, Container: "c2"}}); err != nil {
		t.Fatal(err)
	}
	if len(inner) != 1 {
		t.Errorf("listeners in silo-net: %v, want one for 8080", inner)
	}
	if got := read(inner[8080]); got != "one" {
		t.Errorf("s1 on 8080: %q", got)
	}
	mu.Lock()
	silo = "s2"
	mu.Unlock()
	if got := read(inner[8080]); got != "two" {
		t.Errorf("s2 on 8080: %q", got)
	}
	mu.Lock()
	silo = "s3"
	mu.Unlock()
	if got := read(inner[8080]); got != "" {
		t.Errorf("s3 on 8080: %q", got)
	}
	if got := read(host[8080]); got != "one" {
		t.Errorf("the host's 8080: %q", got)
	}
	// Another silo's host port, or one the host has, is taken.
	err := p.Open(ctx, []docker.Published{{Silo: "s2", Proto: "tcp", Port: 8080, Target: t2, Host: true, Container: "c3"}})
	if !errors.Is(err, docker.ErrPortTaken) {
		t.Errorf("a host port s1 has: %v", err)
	}
	takenHost[9090] = true
	err = p.Open(ctx, []docker.Published{{Silo: "s2", Proto: "tcp", Port: 9090, Target: t2, Host: true, Container: "c3"}})
	if !errors.Is(err, docker.ErrPortTaken) || inner[9090] != nil && read(inner[9090]) != "refused" {
		t.Errorf("a host port the host has: %v", err)
	}
	p.Close("c1")
	if got := read(host[8080]); got != "refused" {
		t.Errorf("the host's 8080 after the container stopped: %q", got)
	}
	mu.Lock()
	silo = "s2"
	mu.Unlock()
	if got := read(inner[8080]); got != "two" {
		t.Errorf("s2 on 8080 after s1's container stopped: %q", got)
	}
	// Sync closes what is no longer published, and opens what is.
	p.Sync(ctx, []docker.Published{{Silo: "s1", Proto: "tcp", Port: 7070, Target: t1, Container: "c4"}})
	if got := read(inner[8080]); got != "refused" {
		t.Errorf("8080 after a Sync without it: %q", got)
	}
	mu.Lock()
	silo = "s1"
	mu.Unlock()
	if got := read(inner[7070]); got != "one" {
		t.Errorf("7070 after a Sync with it: %q", got)
	}
	if fmt.Sprint(audits) != "[open s1 8080 close s1 8080]" {
		t.Errorf("audits: %v", audits)
	}
}
