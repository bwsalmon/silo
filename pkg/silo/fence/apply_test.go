package fence_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/fence"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/gce/fake"
	fe "github.com/bwsalmon/silo/pkg/silo/gce/fake/fenceeval"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
	compute "google.golang.org/api/compute/v1"
)

const project = "host-project"

type obj = map[string]any

// env is silod's fence API on SQLite, with the applier running against
// the fake GCE.
type env struct {
	t       *testing.T
	db      store.Database
	gce     *fake.Fake
	applier *fence.Applier
	srv     *httptest.Server
	root    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvBoot(t, nil)
}

// newEnvBoot is newEnv with more bootstrap fields.
func newEnvBoot(t *testing.T, boot obj) *env {
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
	f := fake.New(fake.Options{Project: project})
	op, err := f.InsertFirewallPolicy(ctx, project, &compute.FirewallPolicy{Name: fence.PolicyName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gce.Await(ctx, f, project, op); err != nil {
		t.Fatal(err)
	}
	a := &fence.Applier{Store: db, GCE: f, Project: project, Config: fence.DefaultConfig, Poll: 20 * time.Millisecond}
	api := fenceapi.New(db, fenceapi.Options{
		Fence: a, Budget: a,
		Caller: func(r *http.Request) (fenceapi.Caller, error) {
			return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
		},
	})
	srv := httptest.NewServer(api)
	t.Cleanup(func() { srv.Close(); api.Close() })
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx, 10*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })
	e := &env{t: t, db: db, gce: f, applier: a, srv: srv}
	body := obj{
		"regions":       []string{"us-central1"},
		"address_space": "10.0.0.0/16", "gateway_range": "10.0.255.0/24", "control_range": "10.0.254.0/24",
		"admins": []string{"user:admin@example.com"},
	}
	for k, v := range boot {
		body[k] = v
	}
	root := e.ok("", "POST", "/silod/bootstrap", body)
	e.root = root["id"].(string)
	// Bootstrap doesn't wait for the fence; the tests do.
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

func (e *env) create(caller, parent, name string, bits int, extra obj) string {
	e.t.Helper()
	body := obj{"name": name, "address": obj{"prefix_length": bits}}
	for k, v := range extra {
		body[k] = v
	}
	return e.ok(caller, "POST", "/silo/v1/silos/-"+parent+"/children", body)["id"].(string)
}

// rules is the policy in the fake, by rule name.
func (e *env) rules() (map[string]*compute.FirewallPolicyRule, *compute.FirewallPolicy) {
	e.t.Helper()
	pol, err := e.gce.GetFirewallPolicy(context.Background(), project, fence.PolicyName)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]*compute.FirewallPolicyRule{}
	for _, r := range pol.Rules {
		if r.RuleName != "" {
			out[r.RuleName] = r
		}
	}
	return out, pol
}

func TestApplyFollowsTheAPI(t *testing.T) {
	e := newEnv(t)
	// Bootstrap waited for the fence: the root's rules are in place.
	rs, _ := e.rules()
	for _, n := range []string{fence.RuleControlDeny, fence.RuleGatewayIn, fence.RuleGatewayAdmin, fence.RuleGatewayOut,
		fence.RuleCatchIn, fence.RuleCatchOut, e.root + "-intra-in", e.root + "-deny-out", e.root + "-api-out"} {
		if rs[n] == nil {
			t.Errorf("after bootstrap the policy has no %s", n)
		}
	}
	if got := rs[fence.RuleControlDeny]; got != nil && (got.Priority != fence.ControlBand ||
		strings.Join(got.Match.DestIpRanges, ",") != "10.0.254.0/24" || got.Action != "deny") {
		t.Errorf("ctl-deny-out = %+v", got)
	}
	if _, ok := rs[fence.RuleGatewayHealth]; ok {
		t.Errorf("gw-health-in without gateway replicas")
	}

	// A create returns once its rules are in place, deny rules and all.
	team := e.create(e.root, e.root, "team", 20, nil)
	prod := e.create(e.root, e.root, "prod", 20, obj{"control": "none"})
	web := e.create(e.root, prod, "web", 24, nil)
	rs, pol := e.rules()
	for _, id := range []string{team, prod, web} {
		for _, s := range []string{"-intra-in", "-intra-out", "-gw-out", "-deny-in", "-deny-out"} {
			if rs[id+s] == nil {
				t.Fatalf("no %s%s", id, s)
			}
		}
	}
	if rs[team+"-api-out"] == nil {
		t.Errorf("team, which is full, has no api-out")
	}
	// prod is sealed, and web below it.
	for _, id := range []string{prod, web} {
		if rs[id+"-api-out"] != nil {
			t.Errorf("%s is sealed but has %s-api-out", id, id)
		}
	}
	if d1, d2 := rs[web+"-intra-in"].Priority, rs[prod+"-intra-in"].Priority; d1 >= d2 {
		t.Errorf("web (depth 2) at %d is not ahead of prod (depth 1) at %d", d1, d2)
	}
	ev, err := fe.New(fe.BeforeClassic, pol, nil)
	if err != nil {
		t.Fatal(err)
	}
	webVM := fe.VM(addrIn(t, rs[web+"-intra-in"]))
	gw := fe.VM("10.0.255.2")
	if ev.Allowed(fe.TCP(webVM, gw, 8080)) || !ev.Allowed(fe.TCP(webVM, gw, 3128)) {
		t.Errorf("web: 8080 %v, 3128 %v; want sealed", ev.Allowed(fe.TCP(webVM, gw, 8080)), ev.Allowed(fe.TCP(webVM, gw, 3128)))
	}

	// Loosening prod to read gives prod its api-out back, at its slot,
	// and moves nothing else; web's own setting is still none.
	before := map[string]int64{}
	for n, r := range rs {
		before[n] = r.Priority
	}
	e.ok(e.root, "PATCH", "/silo/v1/silos/-"+prod, obj{"control": "read"})
	rs, _ = e.rules()
	if r := rs[prod+"-api-out"]; r == nil || r.Priority != before[prod+"-intra-in"]+fence.SlotAPIOut {
		t.Errorf("prod-api-out = %+v, want it at its slot", r)
	}
	if rs[web+"-api-out"] != nil {
		t.Errorf("web, whose own control is none, got an api-out")
	}
	for n, p := range before {
		if rs[n] == nil || rs[n].Priority != p {
			t.Errorf("%s moved or went: was at %d", n, p)
		}
	}

	// A new block is in the silo's rules once add_block returns.
	e.ok(e.root, "POST", "/silo/v1/silos/-"+team+":addBlock", obj{"prefix_length": 24})
	rs, _ = e.rules()
	if n := len(rs[team+"-intra-in"].Match.SrcIpRanges); n != 2 {
		t.Errorf("team-intra-in names %d blocks after addBlock, want 2", n)
	}

	// Drift is repaired by the next apply.
	ctx := context.Background()
	op, err := e.gce.RemoveFirewallPolicyRule(ctx, project, fence.PolicyName, rs[team+"-deny-out"].Priority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gce.Await(ctx, e.gce, project, op); err != nil {
		t.Fatal(err)
	}
	if _, err := e.applier.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if rs, _ = e.rules(); rs[team+"-deny-out"] == nil {
		t.Errorf("a removed deny rule was not put back")
	}
}

// addrIn is an address in the first destination range of an intra rule.
func addrIn(t *testing.T, r *compute.FirewallPolicyRule) string {
	t.Helper()
	return netip.MustParsePrefix(r.Match.DestIpRanges[0]).Addr().Next().String()
}

func TestRuleBudget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rs, _ := e.rules()
	// The root's policy now: set the quota so that one more silo, six
	// rules, passes 90% of it.
	n := len(rs)
	quota := (n+3)*100/fence.BudgetPercent + 1
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return fence.SetQuota(ctx, tx, fence.Quota{Rules: quota})
	}); err != nil {
		t.Fatal(err)
	}
	code, out := e.do(e.root, "POST", "/silo/v1/silos/-"+e.root+"/children", obj{"name": "big", "address": obj{"prefix_length": 20}})
	er, _ := out["error"].(obj)
	if er == nil || er["code"] != "RESOURCE_EXHAUSTED" {
		t.Fatalf("create past the budget: %d %v, want RESOURCE_EXHAUSTED", code, out)
	}
	details, _ := er["details"].(obj)
	if details["quota"] != fence.QuotaRules || !strings.Contains(er["message"].(string), "quota") {
		t.Errorf("refusal %v doesn't name the quota", er)
	}
	// Nothing was created.
	code, _ = e.do(e.root, "GET", "/silo/v1/silos/-"+e.root+"/children", nil)
	if after, _ := e.rules(); len(after) != n || code != 200 {
		t.Errorf("a refused change changed the policy: %d rules, was %d", len(after), n)
	}
	// With room, it goes through.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return fence.SetQuota(ctx, tx, fence.Quota{Rules: 1000, Attributes: 10000})
	}); err != nil {
		t.Fatal(err)
	}
	e.create(e.root, e.root, "big", 20, nil)
}

func TestWaitAppliedWaitsForApply(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	f := fake.New(fake.Options{Project: project})
	op, _ := f.InsertFirewallPolicy(ctx, project, &compute.FirewallPolicy{Name: fence.PolicyName})
	gce.Await(ctx, f, project, op)
	a := &fence.Applier{Store: db, GCE: f, Project: project, Config: fence.DefaultConfig, Poll: 10 * time.Millisecond}
	var seq uint64
	if err := db.Tx(ctx, func(tx store.Tx) error {
		var err error
		seq, err = tx.Append(ctx, "test", "x", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := a.WaitApplied(short, seq); err == nil {
		t.Fatal("WaitApplied returned before anything was applied")
	}
	// Another applier (another replica) applies; this one sees it in
	// the database.
	other := &fence.Applier{Store: db, GCE: f, Project: project, Config: fence.DefaultConfig}
	if got, err := other.Apply(ctx); err != nil || got != seq {
		t.Fatalf("Apply = %d, %v; want %d", got, err, seq)
	}
	wctx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	if err := a.WaitApplied(wctx, seq); err != nil {
		t.Fatal(err)
	}
	// The empty policy is the catch-all rules alone.
	pol, _ := f.GetFirewallPolicy(ctx, project, fence.PolicyName)
	var names []string
	for _, r := range pol.Rules {
		if r.RuleName != "" {
			names = append(names, r.RuleName)
		}
	}
	if strings.Join(names, ",") != "catch-in,catch-out" {
		t.Errorf("policy before bootstrap = %v", names)
	}
}
