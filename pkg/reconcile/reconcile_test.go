package reconcile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/leader"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/reconcile"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	compute "google.golang.org/api/compute/v1"
)

const (
	project = "host-project"
	region  = "us-central1"
	zone    = "us-central1-a"
)

type obj = map[string]any

// env is silod on SQLite over the fake GCE: the fence API, the applier
// running as the leader's would, and a reconciler, with the VPC and
// install's gateway subnet in place.
type env struct {
	t       *testing.T
	ctx     context.Context // carries the leader's term
	db      store.Database
	gce     *fake.Fake
	applier *fence.Applier
	srv     *httptest.Server
	rec     *reconcile.Reconciler
	root    string
}

func newEnv(t *testing.T, opts ...func(*reconcile.Options)) *env {
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
	e := &env{t: t, db: db, gce: f}
	e.await(f.InsertFirewallPolicy(ctx, project, &compute.FirewallPolicy{Name: fence.PolicyName}))
	e.await(f.InsertNetwork(ctx, project, &compute.Network{Name: "silo",
		NetworkFirewallPolicyEnforcementOrder: "BEFORE_CLASSIC_FIREWALL", ForceSendFields: []string{"AutoCreateSubnetworks"}}))
	e.await(f.InsertSubnetwork(ctx, project, region, &compute.Subnetwork{Name: "silo-gateway",
		Network: "global/networks/silo", IpCidrRange: "10.0.255.0/24"}))

	// The leader's term: the epoch every write below is checked against.
	epoch, err := leader.Take(ctx, db, "test")
	if err != nil {
		t.Fatal(err)
	}
	e.ctx = leader.WithTerm(ctx, &leader.Term{Epoch: epoch, Holder: "test"})

	e.applier = &fence.Applier{Store: db, GCE: f, Project: project, Config: fence.DefaultConfig, Poll: 20 * time.Millisecond,
		Guard: leader.Guard, OnDrift: reconcile.AuditDrift(db, t.Logf)}
	api := fenceapi.New(db, fenceapi.Options{
		Fence: e.applier, Budget: e.applier,
		Caller: func(r *http.Request) (fenceapi.Caller, error) {
			return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
		},
	})
	e.srv = httptest.NewServer(api)
	t.Cleanup(func() { e.srv.Close(); api.Close() })
	done := make(chan struct{})
	go func() { defer close(done); e.applier.Run(e.ctx, 10*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })

	o := reconcile.Options{Store: db, GCE: f, Project: project, Fence: e.applier, Logf: t.Logf}
	for _, fn := range opts {
		fn(&o)
	}
	e.rec = reconcile.New(o)
	e.root = e.ok("", "POST", "/silod/bootstrap", obj{
		"regions":       []string{region},
		"address_space": "10.0.0.0/16", "gateway_range": "10.0.255.0/24", "control_range": "10.0.254.0/24",
		"capacity": obj{region: obj{"vcpu": 100, "ssd_disk_gb": 1000}},
		"admins":   []string{"user:admin@example.com"},
	})["id"].(string)
	return e
}

func (e *env) await(op *compute.Operation, err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
	op, err = gce.Await(context.Background(), e.gce, project, op)
	if err != nil {
		e.t.Fatal(err)
	}
	if op.Error != nil && len(op.Error.Errors) > 0 {
		e.t.Fatalf("operation %s: %s", op.Name, op.Error.Errors[0].Message)
	}
}

func (e *env) do(caller, method, path string, body any) (int, obj) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
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

func (e *env) create(name string, extra obj) string {
	e.t.Helper()
	body := obj{"name": name, "address": obj{"prefix_length": 24}}
	for k, v := range extra {
		body[k] = v
	}
	return e.ok(e.root, "POST", "/silo/v1/silos/-"+e.root+"/children", body)["id"].(string)
}

func (e *env) run() *reconcile.Report {
	e.t.Helper()
	rep, err := e.rec.Run(e.ctx)
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return rep
}

// audits are the kinds and objects of the audit records of kind, in
// the root's subtree.
func (e *env) audits(kind string) []obj {
	e.t.Helper()
	out := e.ok(e.root, "GET", "/silo/v1/silos/-"+e.root+"/audit?subtree=true&kind="+kind, nil)
	recs, _ := out["records"].([]any)
	var fields []obj
	for _, r := range recs {
		f, _ := r.(obj)["fields"].(obj)
		fields = append(fields, f)
	}
	return fields
}

// backdate moves every first sighting d into the past.
func (e *env) backdate(table, column string, d time.Duration) {
	e.t.Helper()
	if err := e.db.Tx(context.Background(), func(tx store.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE `+table+` SET `+column+` = `+column+` - ?`, d.Milliseconds())
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func key(collection, scope, name string) string {
	return reconcile.Object{Collection: collection, Scope: scope, Name: name}.Key()
}

func sorted(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }

// TestOrphans: objects labelled or prefixed for a silo that doesn't
// exist are reported once, kept for the grace period of database time,
// then deleted and audited; a prefix alone counts only for a silo the
// database once knew.
func TestOrphans(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	gone := e.create("gone", nil)
	e.ok(e.root, "DELETE", "/silo/v1/silos/-"+gone, nil)
	live := e.create("live", nil)

	// gone's: a labelled disk, and a firewall rule with only the prefix.
	e.await(e.gce.InsertDisk(ctx, project, zone, &compute.Disk{Name: gone + "-data", SizeGb: 10, Labels: map[string]string{"silo": gone}}))
	e.await(e.gce.InsertFirewall(ctx, project, &compute.Firewall{Name: gone + "-allow", Network: "global/networks/silo",
		Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp"}}, SourceRanges: []string{"10.0.0.0/8"}}))
	// A label for a silo never known is an orphan on the label alone.
	e.await(e.gce.InsertImage(ctx, project, &compute.Image{Name: "zzzzzz-img", SourceDisk: "zones/" + zone + "/disks/" + gone + "-data",
		Labels: map[string]string{"silo": "zzzzzz"}}))
	// live's own objects are nobody's orphans.
	e.await(e.gce.InsertDisk(ctx, project, zone, &compute.Disk{Name: live + "-data", SizeGb: 10, Labels: map[string]string{"silo": live}}))

	want := sorted([]string{key("disks", zone, gone+"-data"), key("firewalls", "", gone+"-allow"), key("images", "", "zzzzzz-img")})
	rep := e.run()
	if got := sorted(rep.Found); !slices.Equal(got, want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	if len(rep.Deleted) != 0 {
		t.Fatalf("deleted %v inside the grace period", rep.Deleted)
	}
	if n := len(e.audits(reconcile.KindOrphanFound)); n != 3 {
		t.Errorf("%d orphan.found records, want 3", n)
	}

	// Seen again: reported, not found again, not deleted.
	rep = e.run()
	if len(rep.Found) != 0 || len(rep.Deleted) != 0 || !slices.Equal(sorted(rep.Orphans), want) {
		t.Fatalf("second run: found %v, deleted %v, orphans %v", rep.Found, rep.Deleted, rep.Orphans)
	}
	if n := len(e.audits(reconcile.KindOrphanFound)); n != 3 {
		t.Errorf("%d orphan.found records after a second run, want 3", n)
	}

	// Not quite an hour of database time: still kept.
	e.backdate("reconcile_orphans", "first_seen", 59*time.Minute)
	if rep := e.run(); len(rep.Deleted) != 0 {
		t.Fatalf("deleted %v before the hour was up", rep.Deleted)
	}
	// Past the hour: the image goes first (it holds the disk), and the
	// disk at the next run if GCE refused it while the image existed.
	e.backdate("reconcile_orphans", "first_seen", 2*time.Minute)
	deleted := map[string]bool{}
	for i := 0; i < 3 && len(deleted) < 3; i++ {
		rep, err := e.rec.Run(e.ctx)
		if err != nil {
			t.Logf("run %d: %v", i, err)
		}
		for _, k := range rep.Deleted {
			deleted[k] = true
		}
	}
	for _, k := range want {
		if !deleted[k] {
			t.Errorf("%s not deleted after the grace period (deleted %v)", k, deleted)
		}
	}
	if n := len(e.audits(reconcile.KindOrphanDeleted)); n != 3 {
		t.Errorf("%d orphan.deleted records, want 3", n)
	}
	if _, err := e.gce.GetDisk(ctx, project, zone, gone+"-data"); !gce.IsNotFound(err) {
		t.Errorf("the orphan disk is still there: %v", err)
	}
	if _, err := e.gce.GetDisk(ctx, project, zone, live+"-data"); err != nil {
		t.Errorf("a live silo's disk: %v", err)
	}
	if rep := e.run(); len(rep.Orphans) != 0 {
		t.Errorf("orphans left after deleting them: %v", rep.Orphans)
	}
}

// TestForeign: an object in the VPC with neither label nor prefix,
// other than install's own, is reported once and left alone; so is one
// whose id-shaped prefix names no silo this database has known.
func TestForeign(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.await(e.gce.InsertFirewall(ctx, project, &compute.Firewall{Name: "allow-ssh", Network: "global/networks/silo",
		Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"22"}}}, SourceRanges: []string{"0.0.0.0/0"}}))
	e.await(e.gce.InsertFirewall(ctx, project, &compute.Firewall{Name: "webapp-http", Network: "global/networks/silo",
		Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp", Ports: []string{"80"}}}, SourceRanges: []string{"0.0.0.0/0"}}))
	// install's own, by name and by description.
	e.await(e.gce.InsertFirewall(ctx, project, &compute.Firewall{Name: "silo-allow-iap", Network: "global/networks/silo",
		Allowed: []*compute.FirewallAllowed{{IPProtocol: "tcp"}}, SourceRanges: []string{"35.235.240.0/20"}}))
	// Outside the VPC, and unlabelled: not the reconcile's business.
	e.await(e.gce.InsertDisk(ctx, project, zone, &compute.Disk{Name: "scratch", SizeGb: 10}))

	want := sorted([]string{key("firewalls", "", "allow-ssh"), key("firewalls", "", "webapp-http")})
	for i := 0; i < 2; i++ {
		rep := e.run()
		if got := sorted(rep.Foreign); !slices.Equal(got, want) {
			t.Fatalf("run %d: foreign %v, want %v", i, got, want)
		}
		if len(rep.Orphans) != 0 {
			t.Fatalf("run %d: orphans %v", i, rep.Orphans)
		}
	}
	if n := len(e.audits(reconcile.KindForeignFound)); n != 2 {
		t.Errorf("%d foreign.found records over two runs, want 2", n)
	}
	e.backdate("reconcile_orphans", "first_seen", 2*time.Hour)
	if rep := e.run(); len(rep.Deleted) != 0 {
		t.Errorf("deleted foreign objects: %v", rep.Deleted)
	}
	for _, n := range []string{"allow-ssh", "webapp-http", "silo-allow-iap"} {
		if _, err := e.gce.GetFirewall(ctx, project, n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

// TestReportOnly: after a restore, orphans past the grace period are
// only reported until a root caller runs silo reconcile --apply; and
// --reconcile=report-only holds the process to it whatever the
// database says.
func TestReportOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := store.SetSetting(ctx, e.db, store.SettingReconcile, store.ReconcileReportOnly); err != nil {
		t.Fatal(err)
	}
	e.await(e.gce.InsertDisk(ctx, project, zone, &compute.Disk{Name: "qqqqqq-data", SizeGb: 10, Labels: map[string]string{"silo": "qqqqqq"}}))
	e.run()
	e.backdate("reconcile_orphans", "first_seen", 2*time.Hour)
	rep := e.run()
	if !rep.ReportOnly || len(rep.Deleted) != 0 || len(rep.Orphans) != 1 {
		t.Fatalf("report-only: %+v", rep)
	}
	if _, err := e.gce.GetDisk(ctx, project, zone, "qqqqqq-data"); err != nil {
		t.Fatalf("report-only deleted the orphan: %v", err)
	}

	if got := e.ok(e.root, "GET", "/silo/v1/reconcile", nil)["mode"]; got != "report-only" {
		t.Errorf("mode %v, want report-only", got)
	}
	child := e.create("team", nil)
	if code, out := e.do(child, "POST", "/silo/v1/reconcile:apply", obj{}); code != http.StatusForbidden {
		t.Errorf("a child's reconcile:apply: %d %v, want 403", code, out)
	}
	if got := e.ok(e.root, "POST", "/silo/v1/reconcile:apply?validate_only=true", obj{})["mode"]; got != "report-only" {
		t.Errorf("validate_only changed the mode to %v", got)
	}
	if got := e.ok(e.root, "POST", "/silo/v1/reconcile:apply", obj{})["mode"]; got != "apply" {
		t.Errorf("mode after reconcile:apply %v, want apply", got)
	}
	if n := len(e.audits(fenceapi.KindReconcileApply)); n != 1 {
		t.Errorf("%d reconcile.apply records, want 1", n)
	}

	// The flag still holds this process to report-only.
	flagged := reconcile.New(reconcile.Options{Store: e.db, GCE: e.gce, Project: project, ReportOnly: true, Logf: t.Logf})
	if rep, err := flagged.Run(e.ctx); err != nil || !rep.ReportOnly || len(rep.Deleted) != 0 {
		t.Fatalf("--reconcile=report-only: %+v, %v", rep, err)
	}

	rep = e.run()
	if rep.ReportOnly || !slices.Equal(rep.Deleted, []string{key("disks", zone, "qqqqqq-data")}) {
		t.Fatalf("after reconcile:apply: %+v", rep)
	}
}

// TestRecount: a charge whose object never reached GCE (silod died
// between the charge and the call) is dropped once it is older than
// the charge grace and no operation is pending for it; then used is
// recounted from the charges, by payer.
func TestRecount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	team := e.create("team", obj{"quota": obj{region: obj{"vcpu": 10, "ssd_disk_gb": 100}}})
	vcpu := ledger.Key{Kind: "gce", Class: "vcpu", Region: region}
	charge := func(object string, n int64) {
		t.Helper()
		if err := e.db.Tx(ctx, func(tx store.Tx) error {
			return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: object, Silo: team, Via: ledger.ViaFacade,
				Amounts: []ledger.Amount{{Key: vcpu, Amount: ledger.Units(n)}}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	ghost := "instance:" + zone + "/" + team + "-ghost"     // charged, never created
	real := "disk:" + zone + "/" + team + "-real"           // charged, exists
	pending := "instance:" + zone + "/" + team + "-pending" // charged, operation running
	charge(ghost, 2)
	charge(real, 1)
	charge(pending, 3)
	e.await(e.gce.InsertDisk(ctx, project, zone, &compute.Disk{Name: team + "-real", SizeGb: 10, Labels: map[string]string{"silo": team}}))
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO facade_operations (scope, name, silo, caller, method, object, refund, also, on_success, done, failed, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '', '', '', ?, ?, 0, 0)`, "zones/"+zone, "op-pending", team, team, "compute.instances.insert", pending, false, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	used := func() int64 {
		t.Helper()
		var r ledger.Row
		if err := e.db.Tx(ctx, func(tx store.Tx) error {
			var err error
			r, err = ledger.Get(ctx, tx, team, vcpu)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return r.Used
	}
	if u := used(); u != ledger.Units(6) {
		t.Fatalf("used %d before, want 6 vCPU", u)
	}

	// Fresh charges are left alone: the call may be on its way to GCE.
	if rep := e.run(); len(rep.Dropped) != 0 {
		t.Fatalf("dropped fresh charges: %v", rep.Dropped)
	}
	e.backdate("charges", "updated_at", time.Hour)
	rep := e.run()
	if !slices.Equal(rep.Dropped, []string{ghost}) {
		t.Fatalf("dropped %v, want only %s", rep.Dropped, ghost)
	}
	if u := used(); u != ledger.Units(4) {
		t.Errorf("used %d after the recount, want 4 vCPU", u)
	}
	if n := len(e.audits(reconcile.KindChargeDropped)); n != 1 {
		t.Errorf("%d quota.charge_dropped records, want 1", n)
	}

	// used that drifted from the charges is put back.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE quota SET used = used + ? WHERE silo = ? AND kind = ? AND class = ? AND region = ?`,
			ledger.Units(5), team, vcpu.Kind, vcpu.Class, vcpu.Region)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rep = e.run()
	if !slices.Equal(rep.Recounted, []string{team}) || used() != ledger.Units(4) {
		t.Errorf("recount: %v, used %d; want [%s], 4 vCPU", rep.Recounted, used(), team)
	}
	if n := len(e.audits(reconcile.KindRecount)); n != 1 {
		t.Errorf("%d quota.recount records, want 1", n)
	}
}

// TestFenceDrift: a policy that differs from what the database
// compiles to is repaired at once and audited as fence_drift, a missing
// n-api-out and an extra one alike.
func TestFenceDrift(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	open := e.create("open", nil)
	sealed := e.create("sealed", obj{"control": "none"})
	waitFor := func() {
		t.Helper()
		if _, err := e.applier.Apply(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	waitFor()
	if rep := e.run(); len(rep.Drift) != 0 {
		t.Fatalf("drift on a policy just applied: %v", rep.Drift)
	}
	rules := func() map[string]*compute.FirewallPolicyRule {
		pol, err := e.gce.GetFirewallPolicy(ctx, project, fence.PolicyName)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*compute.FirewallPolicyRule{}
		for _, r := range pol.Rules {
			out[r.RuleName] = r
		}
		return out
	}
	rs := rules()
	apiOut, ok := rs[open+"-api-out"]
	if !ok {
		t.Fatalf("no %s-api-out in the policy", open)
	}
	gwOut, ok := rs[sealed+"-gw-out"]
	if !ok {
		t.Fatalf("no %s-gw-out in the policy", sealed)
	}
	if _, ok := rs[sealed+"-api-out"]; ok {
		t.Fatalf("a sealed silo has %s-api-out", sealed)
	}

	// Someone removes open's API rule and gives sealed one.
	e.await(e.gce.RemoveFirewallPolicyRule(ctx, project, fence.PolicyName, apiOut.Priority))
	extra := *apiOut
	extra.RuleName, extra.Priority = sealed+"-api-out", gwOut.Priority+1
	extra.Match = &compute.FirewallPolicyRuleMatcher{}
	*extra.Match = *apiOut.Match
	extra.Match.SrcIpRanges = gwOut.Match.SrcIpRanges
	e.await(e.gce.AddFirewallPolicyRule(ctx, project, fence.PolicyName, &extra))

	// The reconcile repairs it, unless the fence's own loop, which
	// also repairs at its start and on each change, got there first;
	// either way it is audited.
	e.run()
	rs = rules()
	if _, ok := rs[open+"-api-out"]; !ok {
		t.Errorf("%s-api-out not put back", open)
	}
	if _, ok := rs[sealed+"-api-out"]; ok {
		t.Errorf("%s-api-out not removed", sealed)
	}
	recs := e.audits(reconcile.KindFenceDrift)
	if len(recs) != 1 {
		t.Fatalf("%d fence_drift records, want 1", len(recs))
	}
	steps, _ := recs[0]["steps"].([]any)
	var all []string
	for _, s := range steps {
		all = append(all, s.(string))
	}
	if j := strings.Join(all, "; "); !strings.Contains(j, "add "+open+"-api-out") || !strings.Contains(j, fmt.Sprintf("remove %d", extra.Priority)) {
		t.Errorf("fence_drift steps %q don't name both repairs", j)
	}
	if rep := e.run(); len(rep.Drift) != 0 {
		t.Errorf("drift again after the repair: %v", rep.Drift)
	}
}

// TestStaleTerm: a reconcile under a term that is no longer the
// current one writes nothing.
func TestStaleTerm(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.await(e.gce.InsertDisk(ctx, project, zone, &compute.Disk{Name: "pppppp-data", SizeGb: 10, Labels: map[string]string{"silo": "pppppp"}}))
	if _, err := leader.Take(ctx, e.db, "someone-else"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rec.Run(e.ctx); !errors.Is(err, leader.ErrNotLeader) {
		t.Fatalf("a stale term's reconcile: %v, want ErrNotLeader", err)
	}
	if n := len(e.audits(reconcile.KindOrphanFound)); n != 0 {
		t.Errorf("a stale term wrote %d orphan.found records", n)
	}
	if _, err := e.rec.Run(context.Background()); !errors.Is(err, leader.ErrNotLeader) {
		t.Errorf("a reconcile with no term: %v, want ErrNotLeader", err)
	}
}
