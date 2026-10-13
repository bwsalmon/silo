package hostdoor_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/hostdoor"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

var ctx = context.Background()

type obj = map[string]any

// env is a store, the fence API and the registry over Memory, with the
// host port in front of the registry on 127.0.0.1. Each client dials
// from its own source port, and uids says which uid owns which port:
// what inet_diag would say if the clients were different users'
// processes.
type env struct {
	t      *testing.T
	db     store.Database
	api    *fenceapi.Server
	apiSrv *http.Server
	apiURL string
	port   int
	root   string
	reg    *registry.Service

	mu      sync.Mutex
	uids    map[uint16]uint32
	refused []error
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
	e := &env{t: t, db: db, uids: map[uint16]uint32{}}
	e.api = fenceapi.New(db, fenceapi.Options{Caller: func(r *http.Request) (fenceapi.Caller, error) {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaOrigin}, nil
	}})
	al, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.apiURL = "http://" + al.Addr().String()
	e.apiSrv = &http.Server{Handler: e.api}
	go e.apiSrv.Serve(al)

	reg := registry.New(registry.Options{Store: db, API: e.api, Content: registry.NewMemory()})
	e.reg = reg
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.port = tl.Addr().(*net.TCPAddr).Port
	lookup := func(local, peer netip.AddrPort) (uint32, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		uid, ok := e.uids[peer.Port()]
		if !ok {
			return 0, fmt.Errorf("port %d: %w", peer.Port(), hostdoor.ErrNoPeer)
		}
		return uid, nil
	}
	refused := func(peer net.Addr, err error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.refused = append(e.refused, err)
	}
	srv := &http.Server{Handler: hostdoor.Handler(db, reg), ConnContext: hostdoor.ConnContext}
	go srv.Serve(hostdoor.Listen(tl, lookup, refused))
	t.Cleanup(func() { srv.Close(); e.apiSrv.Close(); e.api.Close() })

	code, root := e.call("", "POST", "/silod/bootstrap", obj{"regions": []string{"us-central1"},
		"capacity": obj{"global": obj{"disk_gb": 100, "image_repos": 100}}, "admins": []string{"user:admin@example.com"}})
	if code != 200 {
		t.Fatalf("bootstrap: %d %v", code, root)
	}
	e.root = root["id"].(string)
	return e
}

// call makes a fence API call as caller.
func (e *env) call(caller, method, path string, body any) (int, obj) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.apiURL+path, bytes.NewReader(b))
	req.Header.Set("Silo-Caller", caller)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := obj{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// child makes a child of the root, as the root, and maps uid to it.
func (e *env) child(name string, uid uint32) string {
	e.t.Helper()
	code, out := e.call(e.root, "POST", "/silo/v1/silos/-"+e.root+"/children", obj{"name": name,
		"address": obj{"prefix_length": 16}, "quota": obj{"global": obj{"disk_gb": 10, "image_repos": 5}}})
	if code != 200 {
		e.t.Fatalf("creating %s: %d %v", name, code, out)
	}
	id := out["id"].(string)
	if code, out := e.call(e.root, "PUT", "/silo/v1/silos/-"+id+"/members/"+fenceapi.UIDIdentity(uid), nil); code != 200 {
		e.t.Fatalf("mapping uid %d to %s: %d %v", uid, name, code, out)
	}
	return id
}

// client is an HTTP client whose every connection the env attributes
// to uid.
func (e *env) client(uid uint32) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		// The source port is chosen first, so that the env knows its
		// owner before the listener accepts.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		e.mu.Lock()
		e.uids[uint16(port)] = uid
		e.mu.Unlock()
		dd := *d
		dd.LocalAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
		return dd.DialContext(ctx, "tcp4", addr)
	}}
	e.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

type resp struct {
	code int
	body []byte
}

func (r resp) errCode() string {
	var out struct {
		Errors []struct{ Code string } `json:"errors"`
	}
	json.Unmarshal(r.body, &out)
	if len(out.Errors) == 0 {
		return ""
	}
	return out.Errors[0].Code
}

// do makes a registry request at localhost:<port>, as the doc names
// the host port, with c.
func (e *env) do(c *http.Client, method, path, ctype string, body []byte) resp {
	e.t.Helper()
	req, _ := http.NewRequest(method, fmt.Sprintf("http://localhost:%d%s", e.port, path), bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, b}
}

func digest(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }

// push pushes a one-blob image to repo, and returns the manifest's
// status.
func (e *env) push(c *http.Client, repo, tag string) resp {
	e.t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	if r := e.do(c, "POST", "/v2/"+repo+"/blobs/uploads/?digest="+digest(cfg), "application/octet-stream", cfg); r.code != 201 {
		return r
	}
	m, _ := json.Marshal(obj{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": obj{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest(cfg), "size": len(cfg)},
		"layers": []any{}})
	return e.do(c, "PUT", "/v2/"+repo+"/manifests/"+tag, "application/vnd.oci.image.manifest.v1+json", m)
}

func (e *env) catalog(c *http.Client) []string {
	e.t.Helper()
	r := e.do(c, "GET", "/v2/_catalog", "", nil)
	if r.code != 200 {
		e.t.Fatalf("catalog: %d %s", r.code, r.body)
	}
	var out struct{ Repositories []string }
	json.Unmarshal(r.body, &out)
	return out.Repositories
}

// Two host users' connections to localhost:<port> act as their own
// silos: each pushes to its own, sees only its own, and can't reach the
// other's.
func TestTwoUIDsActAsTheirOwnSilos(t *testing.T) {
	e := newEnv(t)
	e.child("a", 1001)
	e.child("b", 1002)
	a, b, nobody := e.client(1001), e.client(1002), e.client(1003)

	if r := e.do(a, "GET", "/v2/", "", nil); r.code != 200 {
		t.Fatalf("GET /v2/ as uid 1001: %d %s", r.code, r.body)
	}
	if r := e.push(a, "a/app", "v1"); r.code != 201 {
		t.Fatalf("uid 1001 pushing a/app: %d %s", r.code, r.body)
	}
	if r := e.push(b, "b/app", "v1"); r.code != 201 {
		t.Fatalf("uid 1002 pushing b/app: %d %s", r.code, r.body)
	}
	if got := e.catalog(a); strings.Join(got, ",") != "a/app" {
		t.Errorf("uid 1001's catalog: %v, want [a/app]", got)
	}
	if got := e.catalog(b); strings.Join(got, ",") != "b/app" {
		t.Errorf("uid 1002's catalog: %v, want [b/app]", got)
	}
	// b can't see a's repo, or push into it.
	if r := e.do(b, "GET", "/v2/a/app/manifests/v1", "", nil); r.code != 404 {
		t.Errorf("uid 1002 reading a/app: %d %s, want 404", r.code, r.body)
	}
	if r := e.push(b, "a/app", "v2"); r.code < 400 {
		t.Errorf("uid 1002 pushing a/app: %d, want refused", r.code)
	}
	if r := e.do(a, "GET", "/v2/a/app/manifests/v1", "", nil); r.code != 200 {
		t.Errorf("uid 1001 reading a/app: %d %s", r.code, r.body)
	}
	// A uid mapped to no silo is a stranger.
	if r := e.do(nobody, "GET", "/v2/", "", nil); r.code != 403 || r.errCode() != "DENIED" {
		t.Errorf("GET /v2/ as unmapped uid 1003: %d %s, want 403 DENIED", r.code, r.body)
	}
	// Headers meant for silod's other transports change nothing.
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://localhost:%d/v2/_catalog", e.port), nil)
	req.Header.Set("Silo-Caller", e.root)
	req.Header.Set("Silo-Origin", e.root)
	res, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(body), "a/app") {
		t.Errorf("uid 1002 with Silo-* headers saw %s", body)
	}
	// Only /v2/ is served.
	if r := e.do(a, "GET", "/silo/v1/silos", "", nil); r.code != 404 {
		t.Errorf("GET /silo/v1/silos on the host port: %d, want 404", r.code)
	}
}

// A connection whose peer the lookup can't find is closed unread.
func TestUnknownPeerIsRefused(t *testing.T) {
	e := newEnv(t)
	c, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", e.port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /v2/ HTTP/1.1\r\nHost: localhost\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("read %d bytes, err %v: want the connection closed", n, err)
	}
	// The listener closes the connection and then reports why, so the
	// report may come just after the close the client saw.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		e.mu.Lock()
		n := len(e.refused)
		e.mu.Unlock()
		if n > 0 {
			break
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.refused) != 1 || !errors.Is(e.refused[0], hostdoor.ErrNoPeer) {
		t.Errorf("refused: %v, want one ErrNoPeer", e.refused)
	}
}
