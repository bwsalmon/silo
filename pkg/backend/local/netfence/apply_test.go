package netfence_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

type obj = map[string]any

// env is silod's fence API on SQLite, with the applier loading into a
// fake nft.
type env struct {
	t       *testing.T
	db      store.Database
	nft     *netfence.Fake
	applier *netfence.Applier
	srv     *httptest.Server
	root    string
	// stop ends the applier's Run loop and waits for it to return.
	stop func()

	mu   sync.Mutex
	topo netfence.Topology
}

func newEnv(t *testing.T, cfg netfence.Config) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, nft: &netfence.Fake{},
		topo: netfence.Topology{API: netip.MustParseAddr("10.255.255.1"), Docker: netip.MustParseAddr("10.255.255.2")}}
	a := &netfence.Applier{Store: db, NFT: e.nft, Config: cfg, Poll: 20 * time.Millisecond,
		Topology: func(context.Context, store.Tx) (*netfence.Topology, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			t := e.topo
			return &t, nil
		}}
	e.applier = a
	api := fenceapi.New(db, fenceapi.Options{
		Fence: a, Budget: a,
		Caller: func(r *http.Request) (fenceapi.Caller, error) {
			return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
		},
	})
	e.srv = httptest.NewServer(api)
	t.Cleanup(func() { e.srv.Close(); api.Close() })
	runCtx, stopRun := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); a.Run(runCtx, 10*time.Millisecond) }()
	e.stop = func() { stopRun(); <-done }
	t.Cleanup(e.stop)
	root := e.ok("", "POST", "/silod/bootstrap", obj{
		"regions":       []string{"us-central1"},
		"address_space": "10.0.0.0/16", "gateway_range": "10.0.255.0/24", "control_range": "10.0.254.0/24",
		"admins": []string{"user:admin@example.com"},
	})
	e.root = root["id"].(string)
	if _, err := a.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(caller, method, path string, body any) (int, obj) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(b))
	if caller != "" {
		req.Header.Set("Silo-Caller", caller)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := obj{}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("%s %s: %d %q", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

func (e *env) ok(caller, method, path string, body any) obj {
	e.t.Helper()
	code, out := e.do(caller, method, path, body)
	if code != 200 {
		e.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out
}

func (e *env) create(parent, name string, bits int, extra obj) (string, netip.Prefix) {
	e.t.Helper()
	body := obj{"name": name, "address": obj{"prefix_length": bits}}
	for k, v := range extra {
		body[k] = v
	}
	s := e.ok(e.root, "POST", "/silo/v1/silos/-"+parent+"/children", body)
	got := e.ok(e.root, "GET", "/silo/v1/silos/-"+s["id"].(string), nil)
	address, _ := got["address"].(obj)
	blocks, _ := address["blocks"].([]any)
	if len(blocks) == 0 {
		e.t.Fatalf("silo %s has no blocks: %v", s["id"], got)
	}
	p, err := netip.ParsePrefix(blocks[0].(string))
	if err != nil {
		e.t.Fatalf("block %v: %v", blocks[0], err)
	}
	return s["id"].(string), p
}

// network adds a network of silo on prefix with one port at its first
// host, and returns the port.
func (e *env) network(silo, id string, subnet netip.Prefix) netfence.Port {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := subnet.Addr().Next().Next()
	b := a.As4()
	p := netfence.Port{Name: "v-" + id, Network: id, MAC: net.HardwareAddr{2, 0, b[0], b[1], b[2], b[3]}, Addr: a}
	e.topo.Networks = append(e.topo.Networks, netfence.Network{ID: id, Silo: silo, Bridge: "sb-" + id, Subnet: subnet})
	e.topo.Ports = append(e.topo.Ports, p)
	return p
}

func (e *env) eval(hook string, p netfence.Packet) string {
	e.t.Helper()
	rs := e.nft.Loaded()
	if rs == nil {
		e.t.Fatal("nothing loaded")
	}
	v, _ := rs.Eval("inet", hook, p)
	return v
}

func TestApplyFollowsTheAPI(t *testing.T) {
	e := newEnv(t, netfence.Config{})
	ctx := context.Background()
	team, teamBlock := e.create(e.root, "team", 20, nil)
	prod, prodBlock := e.create(e.root, "prod", 20, obj{"control": "none"})
	tp := e.network(team, "t1", netip.PrefixFrom(teamBlock.Addr(), 26))
	pp := e.network(prod, "p1", netip.PrefixFrom(prodBlock.Addr(), 26))
	if _, err := e.applier.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	api := netip.MustParseAddr("10.255.255.1")
	toAPI := func(p netfence.Port) netfence.Packet {
		return netfence.Packet{IIF: "sb-" + p.Network, Src: p.Addr, Dst: api, Proto: 6, DPort: netfence.PortAPI}
	}
	if got := e.eval("input", toAPI(tp)); got != "accept" {
		t.Errorf("team to the API: %s", got)
	}
	if got := e.eval("input", toAPI(pp)); got != "drop" {
		t.Errorf("sealed prod to the API: %s", got)
	}
	between := netfence.Packet{IIF: "sb-t1", Src: tp.Addr, Dst: pp.Addr, Proto: 6, DPort: 5432}
	if got := e.eval("forward", between); got != "drop" {
		t.Errorf("team to prod with no link: %s", got)
	}

	// Loosening prod is in the fence once the PATCH returns.
	e.ok(e.root, "PATCH", "/silo/v1/silos/-"+prod, obj{"control": "read"})
	if got := e.eval("input", toAPI(pp)); got != "accept" {
		t.Errorf("prod at control read, to the API: %s", got)
	}

	// Drift: someone changed the tables. Repair reports it and loads
	// the fence again. Run is stopped first: a change it has not yet
	// drained (the PATCH's, say) would have it apply between Tamper and
	// Repair, find the drift itself and leave Repair none to report.
	e.stop()
	var drifted []string
	e.applier.OnDrift = func(_ context.Context, d []string, repaired bool) {
		if repaired {
			drifted = d
		}
	}
	loads := e.nft.Loads()
	if d, err := e.applier.Repair(ctx); err != nil || len(d) != 0 {
		t.Fatalf("Repair with no drift: %v, %v", d, err)
	}
	e.nft.Tamper("add rule inet silo forward accept")
	d, err := e.applier.Repair(ctx)
	if err != nil || len(d) == 0 || len(drifted) == 0 {
		t.Fatalf("Repair after tampering: %v, %v; OnDrift saw %v", d, err, drifted)
	}
	if e.nft.Loads() <= loads {
		t.Errorf("Repair loaded nothing")
	}
	if d, err := e.applier.Repair(ctx); err != nil || len(d) != 0 {
		t.Errorf("Repair after repairing: %v, %v", d, err)
	}
}

func TestSetBudget(t *testing.T) {
	e := newEnv(t, netfence.Config{MaxSetElements: 1})
	code, out := e.do(e.root, "POST", "/silo/v1/silos/-"+e.root+"/children", obj{"name": "a", "address": obj{"prefix_length": 20}})
	if code == 200 {
		// One silo is still one tenants element (it is inside the
		// root's block); a sealed sibling splits api_ok in two.
		code, out = e.do(e.root, "POST", "/silo/v1/silos/-"+e.root+"/children", obj{"name": "b", "address": obj{"prefix_length": 20}, "control": "none"})
	}
	er, _ := out["error"].(obj)
	if er == nil || er["code"] != "RESOURCE_EXHAUSTED" {
		t.Fatalf("create past the set cap: %d %v, want RESOURCE_EXHAUSTED", code, out)
	}
	if details, _ := er["details"].(obj); details["quota"] != netfence.QuotaSetElements {
		t.Errorf("refusal %v doesn't name %s", er, netfence.QuotaSetElements)
	}
}

func TestLoadFailureIsRetried(t *testing.T) {
	e := newEnv(t, netfence.Config{})
	e.nft.Fail(io.ErrUnexpectedEOF)
	if _, err := e.applier.Apply(context.Background()); err == nil {
		t.Fatal("Apply with a failing nft succeeded")
	}
	e.nft.Fail(nil)
	if _, err := e.applier.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
}
