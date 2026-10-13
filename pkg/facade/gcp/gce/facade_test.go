package gce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	gceapi "github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/fake"
	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/facade"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	compute "google.golang.org/api/compute/v1"
)

var ctx = context.Background()

const host = "host-project"

type obj = map[string]any

// env is silod on SQLite, serving the fence API and the facade over a
// fake GCE, with the caller taken from a Silo-Caller header.
type env struct {
	t      *testing.T
	db     store.Database
	gce    *fake.Fake
	facade *Server
	srv    *httptest.Server
	// The tree: root, with team-a (us-central1 only) and its child ci,
	// and team-b; ro has control=read and sealed control=none.
	root, a, ci, b, ro, sealed string
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
	e := &env{t: t, db: db, gce: fake.New(fake.Options{Project: host, ManualOperations: true})}
	e.facade = New(db, Options{GCE: e.gce, HostProject: host, Caller: caller})
	api := fenceapi.New(db, fenceapi.Options{Caller: caller, Pricer: e.facade.Pricer()})
	mux := http.NewServeMux()
	mux.Handle("/compute/v1/", e.facade)
	mux.Handle("/", api)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(func() { e.srv.Close(); e.facade.Close(); api.Close() })

	code, root := e.do("", "POST", "/silod/bootstrap", obj{
		"regions":  []string{"us-central1", "us-east1"},
		"capacity": obj{"us-central1": obj{"vcpu": 100, "memory_mb": 409600, "ssd_disk_gb": 1000}, "us-east1": obj{"vcpu": 10}, "global": obj{"subnetworks": 20, "images": 10, "firewall_rules": 20}},
		"admins":   []string{"user:admin@example.com"},
	})
	if code != 200 {
		t.Fatalf("bootstrap: %d %v", code, root)
	}
	e.root = root["id"].(string)
	mk := func(parent, name string, bits int, extra obj) string {
		body := obj{"name": name, "address": obj{"prefix_length": bits}}
		for k, v := range extra {
			body[k] = v
		}
		code, out := e.do(e.root, "POST", "/silo/v1/silos/"+parent+"/children", body)
		if code != 200 {
			t.Fatalf("create %s/%s: %d %v", parent, name, code, out)
		}
		return out["id"].(string)
	}
	e.a = mk("-"+e.root, "team-a", 16, obj{"regions": []string{"us-central1"},
		"quota": obj{"us-central1": obj{"vcpu": 8, "memory_mb": 32768, "ssd_disk_gb": 100}, "global": obj{"subnetworks": 2, "images": 3, "firewall_rules": 3}}})
	e.ci = mk("team-a", "ci", 24, obj{"regions": []string{"us-central1"}, "quota": obj{"us-central1": obj{"vcpu": 2}}})
	e.b = mk("-"+e.root, "team-b", 16, obj{"regions": []string{"us-central1", "us-east1"}, "quota": obj{"global": obj{"firewall_rules": 2}}})
	e.ro = mk("-"+e.root, "ro", 16, obj{"control": "read"})
	e.sealed = mk("team-a", "sealed", 24, obj{"control": "none"})
	return e
}

func (e *env) do(caller, method, path string, body any) (int, obj) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	if caller != "" {
		req.Header.Set("Silo-Caller", caller)
	}
	req.Header.Set("Authorization", "Bearer anything") // ignored
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := obj{}
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s %s: %d, body %q is not JSON", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode, out
}

// cv is a facade path: /compute/v1/projects/<project>/<rest>.
func cv(project, rest string) string {
	p := "/compute/v1/projects/" + project
	if rest != "" {
		p += "/" + rest
	}
	return p
}

func (e *env) ok(caller, method, path string, body any) obj {
	e.t.Helper()
	code, out := e.do(caller, method, path, body)
	if code != 200 {
		e.t.Fatalf("%s %s as %s: %d %v", method, path, caller, code, out)
	}
	return out
}

// fail makes a call that must fail with GCE's code and reason, and
// returns the message.
func (e *env) fail(code int, reason, caller, method, path string, body any) string {
	e.t.Helper()
	got, out := e.do(caller, method, path, body)
	er, _ := out["error"].(obj)
	items, _ := er["errors"].([]any)
	var r string
	if len(items) > 0 {
		r, _ = items[0].(obj)["reason"].(string)
	}
	if got != code || r != reason || er["code"] != float64(code) {
		e.t.Fatalf("%s %s as %s: %d %v, want %d %s", method, path, caller, got, out, code, reason)
	}
	return er["message"].(string)
}

func items(o obj) []obj {
	l, _ := o["items"].([]any)
	out := make([]obj, len(l))
	for i, v := range l {
		out[i] = v.(obj)
	}
	return out
}

func names(l []obj) []string {
	out := []string{}
	for _, o := range l {
		out = append(out, o["name"].(string))
	}
	return out
}

func quotaOf(o obj, metric string) (limit, usage float64, ok bool) {
	for _, q := range o["quotas"].([]any) {
		m := q.(obj)
		if m["metric"] == metric {
			return m["limit"].(float64), m["usage"].(float64), true
		}
	}
	return 0, 0, false
}

func TestHandlersAreInControlTable(t *testing.T) {
	table := control.FacadeMethods()
	for m := range handlers {
		if _, ok := table[m]; !ok {
			t.Errorf("handler %s is not in pkg/control's facade table", m)
		}
	}
	for m := range requestFields {
		if _, ok := handlers[m]; !ok {
			t.Errorf("requestFields has %s, which has no handler", m)
		}
	}
}

func TestProjects(t *testing.T) {
	e := newEnv(t)
	p := e.ok(e.a, "GET", cv("silo-"+e.a, ""), nil)
	if p["name"] != "silo-"+e.a || p["kind"] != "compute#project" || p["selfLink"] != DefaultEndpoint+"projects/silo-"+e.a {
		t.Errorf("projects.get: %v", p)
	}
	if md := p["commonInstanceMetadata"].(obj); len(md["items"].([]any)) != 0 {
		t.Errorf("commonInstanceMetadata: %v", md)
	}
	if lim, use, ok := quotaOf(p, "SUBNETWORKS"); !ok || lim != 2 || use != 0 {
		t.Errorf("SUBNETWORKS: %v %v %v", lim, use, ok)
	}
	if lim, _, ok := quotaOf(p, "IMAGES"); !ok || lim != 3 {
		t.Errorf("IMAGES: %v %v", lim, ok)
	}
	// The host project is the caller's own silo.
	if p := e.ok(e.a, "GET", cv(host, ""), nil); p["name"] != "silo-"+e.a {
		t.Errorf("projects.get on the host project: %v", p["name"])
	}
	// A descendant, and the root reaching anything.
	e.ok(e.a, "GET", cv("silo-"+e.ci, ""), nil)
	e.ok(e.root, "GET", cv("silo-"+e.ci, ""), nil)
	// A sibling, an ancestor, a silo that doesn't exist and another
	// project all look the same.
	want := ""
	for _, c := range []struct{ caller, project string }{
		{e.a, "silo-" + e.b}, {e.ci, "silo-" + e.a}, {e.a, "silo-" + e.root}, {e.a, "silo-nosuch"}, {e.a, "other-project"},
	} {
		msg := e.fail(403, "forbidden", c.caller, "GET", cv(c.project, "zones"), nil)
		if got := strings.ReplaceAll(msg, c.project, "P"); want == "" {
			want = got
		} else if got != want {
			t.Errorf("%s on %s: %q, want the same as %q", c.caller, c.project, got, want)
		}
	}
	// No caller, or one that doesn't exist.
	e.fail(403, "forbidden", "", "GET", cv(host, ""), nil)
	e.fail(403, "forbidden", "nosuch", "GET", cv(host, ""), nil)
}

func TestRegionQuotas(t *testing.T) {
	e := newEnv(t)
	r := e.ok(e.a, "GET", cv("silo-"+e.a, "regions/us-central1"), nil)
	// team-a has 8 vCPU, 2 of them delegated to ci.
	for metric, want := range map[string]float64{"CPUS": 6, "SILO_MEMORY_MB": 32768, "SSD_TOTAL_GB": 100, "SILO_PREEMPTIBLE_MEMORY_MB": 0, "NVIDIA_L4_GPUS": 0, "PREEMPTIBLE_NVIDIA_L4_GPUS": 0, "HDB_TOTAL_GB": 0} {
		if lim, use, ok := quotaOf(r, metric); !ok || lim != want || use != 0 {
			t.Errorf("%s: limit %v usage %v (%v), want limit %v", metric, lim, use, ok, want)
		}
	}
	if _, _, ok := quotaOf(r, "SUBNETWORKS"); ok {
		t.Errorf("a global count is in regions.get")
	}
	if r["selfLink"] != DefaultEndpoint+"projects/silo-"+e.a+"/regions/us-central1" {
		t.Errorf("selfLink %v", r["selfLink"])
	}
	for _, z := range r["zones"].([]any) {
		if !strings.HasPrefix(z.(string), DefaultEndpoint+"projects/silo-"+e.a+"/zones/us-central1-") {
			t.Errorf("zone link %v", z)
		}
	}
	// Usage follows the ledger.
	err := e.db.Tx(ctx, func(tx store.Tx) error {
		return ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: "x", Silo: e.a, Via: ledger.ViaFacade,
			Amounts: []ledger.Amount{{Key: ledger.Key{Kind: "gce", Class: "vcpu", Region: "us-central1"}, Amount: ledger.Units(4)}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	r = e.ok(e.a, "GET", cv(host, "regions/us-central1"), nil)
	if lim, use, _ := quotaOf(r, "CPUS"); lim != 6 || use != 4 {
		t.Errorf("CPUS after a charge: limit %v usage %v", lim, use)
	}
	e.fail(403, "regionNotAllowed", e.a, "GET", cv(host, "regions/us-east1"), nil)
	e.fail(403, "regionNotAllowed", e.a, "GET", cv(host, "regions/nowhere"), nil)
	if got := names(items(e.ok(e.a, "GET", cv(host, "regions"), nil))); fmt.Sprint(got) != "[us-central1]" {
		t.Errorf("regions.list: %v", got)
	}
	if got := names(items(e.ok(e.b, "GET", cv(host, "regions"), nil))); fmt.Sprint(got) != "[us-central1 us-east1]" {
		t.Errorf("team-b regions.list: %v", got)
	}
}

func TestZonesAndCatalog(t *testing.T) {
	e := newEnv(t)
	zs := names(items(e.ok(e.a, "GET", cv(host, "zones"), nil)))
	if fmt.Sprint(zs) != "[us-central1-a us-central1-b us-central1-c us-central1-f]" {
		t.Errorf("zones.list: %v", zs)
	}
	z := e.ok(e.a, "GET", cv(host, "zones/us-central1-a"), nil)
	if z["region"] != DefaultEndpoint+"projects/silo-"+e.a+"/regions/us-central1" {
		t.Errorf("zone region %v", z["region"])
	}
	e.fail(403, "regionNotAllowed", e.a, "GET", cv(host, "zones/us-east1-b"), nil)
	e.fail(403, "regionNotAllowed", e.a, "GET", cv(host, "zones/us-east1-b/machineTypes"), nil)
	e.fail(404, "notFound", e.a, "GET", cv(host, "zones/us-central1-z"), nil)

	mt := e.ok(e.a, "GET", cv(host, "zones/us-central1-a/machineTypes/n2-standard-8"), nil)
	if mt["guestCpus"] != float64(8) || mt["selfLink"] != DefaultEndpoint+"projects/silo-"+e.a+"/zones/us-central1-a/machineTypes/n2-standard-8" {
		t.Errorf("machineTypes.get: %v", mt)
	}
	e.ok(e.a, "GET", cv(host, "zones/us-central1-a/diskTypes/pd-ssd"), nil)
	e.ok(e.a, "GET", cv(host, "zones/us-central1-a/acceleratorTypes/nvidia-l4"), nil)

	// Paging: every machine type in us-central1-a, two at a time.
	all := names(items(e.ok(e.a, "GET", cv(host, "zones/us-central1-a/machineTypes"), nil)))
	var paged []string
	tok := ""
	for i := 0; ; i++ {
		l := e.ok(e.a, "GET", cv(host, "zones/us-central1-a/machineTypes?maxResults=2&pageToken="+tok), nil)
		paged = append(paged, names(items(l))...)
		next, _ := l["nextPageToken"].(string)
		if next == "" {
			break
		}
		if strings.Contains(next, "machineType") || i > 100 {
			t.Fatalf("page token %q", next)
		}
		tok = next
	}
	if len(all) < 3 || fmt.Sprint(paged) != fmt.Sprint(all) {
		t.Errorf("paged %v, want %v", paged, all)
	}
	e.fail(400, "invalid", e.a, "GET", cv(host, "zones/us-central1-a/machineTypes?pageToken=bogus"), nil)
	e.fail(400, "invalid", e.a, "GET", cv(host, "zones/us-central1-a/machineTypes?maxResults=501"), nil)

	for _, coll := range []string{"machineTypes", "diskTypes", "acceleratorTypes"} {
		agg := e.ok(e.a, "GET", cv(host, "aggregated/"+coll), nil)
		groups := agg["items"].(obj)
		if len(groups) == 0 {
			t.Errorf("%s aggregatedList is empty", coll)
		}
		for scope, g := range groups {
			if !strings.HasPrefix(scope, "zones/us-central1-") {
				t.Errorf("%s aggregatedList has %s", coll, scope)
			}
			if len(g.(obj)[coll].([]any)) == 0 {
				t.Errorf("%s aggregatedList %s is empty", coll, scope)
			}
		}
	}
	// aggregatedList pages across scopes.
	agg := e.ok(e.a, "GET", cv(host, "aggregated/diskTypes?maxResults=3"), nil)
	n := 0
	for _, g := range agg["items"].(obj) {
		n += len(g.(obj)["diskTypes"].([]any))
	}
	if n != 3 || agg["nextPageToken"] == nil {
		t.Errorf("aggregatedList page: %d items, token %v", n, agg["nextPageToken"])
	}
}

func TestDefaultDeny(t *testing.T) {
	e := newEnv(t)
	// A type the facade doesn't know.
	e.fail(404, "notFound", e.a, "GET", cv(host, "global/fooBars"), nil)
	e.fail(404, "notFound", e.a, "GET", cv(host, "zones/us-central1-a/widgets/x"), nil)
	// Types and methods it refuses, named.
	for _, c := range []struct{ method, path, name string }{
		{"GET", "global/routes", "routes.list"},
		{"POST", "regions/us-central1/addresses", "addresses.insert"},
		{"POST", "global/networks", "networks.insert"},
		{"DELETE", "global/networks/default", "networks.delete"},
		{"POST", "global/networks/default/addPeering", "networks.addPeering"},
		{"POST", "setCommonInstanceMetadata", "projects.setCommonInstanceMetadata"},
		{"POST", "setUsageExportBucket", "projects.setUsageExportBucket"},
		{"GET", "global/machineImages", "machineImages.list"},
		{"GET", "zones/us-central1-a/instanceGroupManagers", "instanceGroupManagers.list"},
		{"POST", "regions/us-central1/subnetworks/x/setPrivateIpGoogleAccess", "subnetworks.setPrivateIpGoogleAccess"},
		{"GET", "aggregated/regions", "regions.aggregatedList"},
		{"POST", "global/routes", "routes.insert"},
		{"POST", "regions/us-central1/routers", "routers.insert"},
		{"POST", "regions/us-central1/vpnGateways", "vpnGateways.insert"},
		{"POST", "regions/us-central1/serviceAttachments", "serviceAttachments.insert"},
		{"POST", "regions/us-central1/forwardingRules", "forwardingRules.insert"},
		{"POST", "global/machineImages", "machineImages.insert"},
		{"POST", "zones/us-central1-a/nodeGroups", "nodeGroups.insert"},
		{"POST", "zones/us-central1-a/storagePools", "storagePools.insert"},
		{"POST", "zones/us-central1-a/instanceGroupManagers", "instanceGroupManagers.insert"},
		{"POST", "regions/us-central1/instanceGroupManagers", "regionInstanceGroupManagers.insert"},
		{"POST", "zones/us-central1-a/autoscalers", "autoscalers.insert"},
		{"POST", "setDefaultNetworkTier", "projects.setDefaultNetworkTier"},
		{"POST", "zones/us-central1-a/reservations", "reservations.insert"},
	} {
		msg := e.fail(403, "forbidden", e.a, c.method, cv(host, c.path), obj{})
		if !strings.Contains(msg, "silo: method not allowed") || !strings.Contains(msg, c.name) {
			t.Errorf("%s %s: %q, want it to name %s", c.method, c.path, msg, c.name)
		}
	}
	// A refused write is audited, under the silo, with the method.
	recs := e.audit(e.a, KindFacadeCall)
	if len(recs) == 0 {
		t.Fatal("no facade.call audit records for refused writes")
	}
	last := recs[len(recs)-1]
	if last["result"] != "forbidden" || last["fields"].(obj)["method"] != "compute.reservations.insert" {
		t.Errorf("audit: %v", last)
	}
	// Reads aren't audited.
	n := len(recs)
	e.ok(e.a, "GET", cv(host, "zones"), nil)
	if len(e.audit(e.a, KindFacadeCall)) != n {
		t.Error("a read was audited")
	}
}

// audit lists silo's audit records of kind.
func (e *env) audit(silo, kind string) []obj {
	e.t.Helper()
	out := e.ok(e.root, "GET", "/silo/v1/silos/-"+silo+"/audit?kind="+kind, nil)
	var l []obj
	for _, r := range out["records"].([]any) {
		l = append(l, r.(obj))
	}
	return l
}

func TestControlPolicy(t *testing.T) {
	e := newEnv(t)
	// Under read, reads work and writes are refused, in any project.
	e.ok(e.ro, "GET", cv(host, "zones"), nil)
	msg := e.fail(403, "forbidden", e.ro, "POST", cv(host, "zones/us-central1-a/disks"), obj{"name": e.ro + "-d"})
	if msg != "silo: control-plane calls from /ro are refused (control=read)" {
		t.Errorf("read: %q", msg)
	}
	// Under none, nothing.
	msg = e.fail(403, "forbidden", e.sealed, "GET", cv(host, "zones"), nil)
	if msg != "silo: control-plane calls from /team-a/sealed are refused (control=none)" {
		t.Errorf("none: %q", msg)
	}
	e.fail(403, "forbidden", e.sealed, "GET", cv("debian-cloud", "global/images"), nil)
	// The refusal is audited as control.denied, under the caller.
	if len(e.audit(e.sealed, fenceapi.KindControlDenied)) == 0 {
		t.Error("no control.denied record")
	}
	// The target's own policy doesn't matter: team-a reads its sealed
	// child's facade.
	e.ok(e.a, "GET", cv("silo-"+e.sealed, "zones"), nil)
}

func TestImages(t *testing.T) {
	e := newEnv(t)
	l := e.ok(e.a, "GET", cv("debian-cloud", "global/images"), nil)
	if got := names(items(l)); len(got) != 2 {
		t.Errorf("debian-cloud images: %v", got)
	}
	img := e.ok(e.a, "GET", cv("debian-cloud", "global/images/family/debian-12"), nil)
	if img["name"] != "debian-12-bookworm-v20260901" ||
		img["selfLink"] != DefaultEndpoint+"projects/debian-cloud/global/images/debian-12-bookworm-v20260901" {
		t.Errorf("getFromFamily: %v", img)
	}
	e.ok(e.a, "GET", cv("debian-cloud", "global/images/debian-12-bookworm-v20260801"), nil)
	// Only reads, and only listed projects.
	e.fail(403, "forbidden", e.a, "POST", cv("debian-cloud", "global/images"), obj{"name": "x"})
	e.fail(403, "forbidden", e.a, "GET", cv("someone-cloud", "global/images"), nil)

	// The silo's own images, and another silo's it can't see.
	e.gce.AddImage(host, &compute.Image{Name: e.a + "-mine", Family: "app", DiskSizeGb: 10,
		Labels: map[string]string{"silo": e.a, "silo-managed": "true", "env": "dev"}})
	e.gce.AddImage(host, &compute.Image{Name: e.b + "-theirs", Labels: map[string]string{"silo": e.b, "silo-managed": "true"}})
	own := items(e.ok(e.a, "GET", cv(host, "global/images"), nil))
	if fmt.Sprint(names(own)) != "["+e.a+"-mine]" {
		t.Fatalf("own images: %v", names(own))
	}
	if fmt.Sprint(own[0]["labels"]) != "map[env:dev]" {
		t.Errorf("labels shown: %v", own[0]["labels"])
	}
	if own[0]["selfLink"] != DefaultEndpoint+"projects/silo-"+e.a+"/global/images/"+e.a+"-mine" {
		t.Errorf("selfLink %v", own[0]["selfLink"])
	}
	if got := e.ok(e.a, "GET", cv("silo-"+e.a, "global/images/family/app"), nil); got["name"] != e.a+"-mine" {
		t.Errorf("own getFromFamily: %v", got)
	}
	e.fail(404, "notFound", e.a, "GET", cv(host, "global/images/"+e.a+"-theirs"), nil)
	// b's name carries b's prefix, which isn't a's; a bare name none.
	msg := e.fail(400, "invalid", e.a, "GET", cv(host, "global/images/"+e.b+"-theirs"), nil)
	if !strings.Contains(msg, `names in this silo must start with "`+e.a+`-"`) {
		t.Errorf("another silo's prefix: %q", msg)
	}
	e.fail(400, "invalid", e.a, "GET", cv(host, "global/images/mine"), nil)
	e.fail(400, "invalid", e.a, "GET", cv(host, "global/images/"+e.a+"-"+strings.Repeat("x", 57)), nil)
	// The root, an ancestor, reads team-a's image in team-a's project.
	if got := e.ok(e.root, "GET", cv("silo-"+e.a, "global/images/"+e.a+"-mine"), nil); got["name"] != e.a+"-mine" {
		t.Errorf("ancestor images.get: %v", got)
	}
}

// TestListFilters: filter, orderBy, maxResults and pageToken are the
// facade's, on the tenant's view (SV-74).
func TestListFilters(t *testing.T) {
	e := newEnv(t)
	for i, n := range []string{"mine", "app-2", "base"} {
		e.gce.AddImage(host, &compute.Image{Name: e.a + "-" + n, Family: "app", DiskSizeGb: int64(10 * (i + 1)),
			CreationTimestamp: fmt.Sprintf("2026-09-0%dT00:00:00.000-07:00", 3-i),
			Labels:            map[string]string{"silo": e.a, "silo-managed": "true", "env": []string{"dev", "prod", "dev"}[i]}})
	}
	e.gce.AddImage(host, &compute.Image{Name: e.b + "-theirs", Labels: map[string]string{"silo": e.b, "silo-managed": "true", "env": "dev"}})
	list := func(query string) []string {
		t.Helper()
		return names(items(e.ok(e.a, "GET", cv(host, "global/images?"+query), nil)))
	}
	q := func(k, v string) string { return k + "=" + url.QueryEscape(v) }
	// Names are the real ones, prefixed: a filter on a name is on it.
	p := e.a + "-"
	in := func(l ...string) string {
		for i := range l {
			l[i] = p + l[i]
		}
		return fmt.Sprint(l)
	}
	for _, c := range []struct{ query, want string }{
		{"", in("app-2", "base", "mine")},
		{q("filter", "labels.env = dev"), in("base", "mine")},
		{q("filter", `name eq ".*\bmine\b.*"`), in("mine")},
		{q("filter", "labels.env:* AND (name eq \""+p+"a.*\")"), in("app-2")},
		{q("filter", "diskSizeGb > 15 OR name = "+p+"mine"), in("app-2", "base", "mine")},
		{q("filter", "NOT diskSizeGb > 15"), in("mine")},
		{q("filter", "name = "+p+"mine"), in("mine")},
		// A bare name is no object's.
		{q("filter", "name = mine"), "[]"},
		// Another silo's objects are never there to match.
		{q("filter", "name = theirs OR name = "+e.b+"-theirs"), "[]"},
		// The hidden labels match nothing.
		{q("filter", "labels.silo = "+e.a), "[]"},
		{q("filter", "labels.silo-managed:*"), "[]"},
		{q("filter", "labels:silo"), "[]"},
		{q("orderBy", "creationTimestamp desc"), in("mine", "app-2", "base")},
		{q("orderBy", "diskSizeGb desc"), in("base", "app-2", "mine")},
		{q("filter", "labels.env = dev") + "&" + q("orderBy", "diskSizeGb desc"), in("base", "mine")},
	} {
		if got := fmt.Sprint(list(c.query)); got != c.want {
			t.Errorf("images.list?%s: %s, want %s", c.query, got, c.want)
		}
	}
	// Paging is over the filtered, ordered list.
	var paged []string
	tok := ""
	for i := 0; i < 5; i++ {
		l := e.ok(e.a, "GET", cv(host, "global/images?maxResults=1&"+q("orderBy", "name desc")+"&"+q("filter", "name != "+p+"base")+"&pageToken="+tok), nil)
		paged = append(paged, names(items(l))...)
		tok, _ = l["nextPageToken"].(string)
		if tok == "" {
			break
		}
	}
	if fmt.Sprint(paged) != in("mine", "app-2") {
		t.Errorf("paged: %v", paged)
	}
	// Outside the grammar is 400 invalid.
	for _, f := range []string{"name ~ mine", "(name = mine", "name:(a b)", "name ="} {
		e.fail(400, "invalid", e.a, "GET", cv(host, "global/images?"+q("filter", f)), nil)
	}
	e.fail(400, "invalid", e.a, "GET", cv(host, "global/images?"+q("orderBy", "name sideways")), nil)

	// The catalog lists too: terraform's google_compute_zones, and an
	// aggregatedList.
	zs := names(items(e.ok(e.a, "GET", cv(host, "zones?"+q("filter", " (status eq UP)")), nil)))
	if len(zs) == 0 {
		t.Error("zones with (status eq UP): none")
	}
	if got := items(e.ok(e.a, "GET", cv(host, "zones?"+q("filter", "status eq DOWN")), nil)); len(got) != 0 {
		t.Errorf("zones with status eq DOWN: %v", names(got))
	}
	agg := e.ok(e.a, "GET", cv(host, "aggregated/machineTypes?"+q("filter", "name = n2-standard-8")), nil)
	n := 0
	for scope, g := range agg["items"].(obj) {
		for _, mt := range g.(obj)["machineTypes"].([]any) {
			n++
			if mt.(obj)["name"] != "n2-standard-8" {
				t.Errorf("aggregatedList %s has %v", scope, mt.(obj)["name"])
			}
		}
	}
	if n == 0 {
		t.Error("aggregatedList filter matched nothing")
	}
	e.fail(400, "invalid", e.a, "GET", cv(host, "aggregated/machineTypes?"+q("filter", "name ~ n2")), nil)
}

func TestNetworks(t *testing.T) {
	e := newEnv(t)
	// The shared VPC and a subnet of team-a's, and one of team-b's.
	if _, err := e.gce.InsertNetwork(ctx, host, &compute.Network{Name: "silo", AutoCreateSubnetworks: false, ForceSendFields: []string{"AutoCreateSubnetworks"}}); err != nil {
		t.Fatal(err)
	}
	e.gce.RunOperations()
	blocks := e.ok(e.a, "GET", "/silo/v1/self", nil)["address"].(obj)["blocks"].([]any)
	for _, sn := range []struct{ name, cidr string }{{e.a + "-sub1", strings.Replace(blocks[0].(string), "/16", "/24", 1)}, {e.b + "-sub9", "10.200.0.0/24"}} {
		if _, err := e.gce.InsertSubnetwork(ctx, host, "us-central1", &compute.Subnetwork{Name: sn.name, Network: gceapi.SelfLinkBase + "projects/" + host + "/global/networks/silo", IpCidrRange: sn.cidr}); err != nil {
			t.Fatal(err)
		}
	}
	e.gce.RunOperations()
	n := e.ok(e.a, "GET", cv(host, "global/networks/default"), nil)
	if n["name"] != "default" || n["autoCreateSubnetworks"] != false || n["routingConfig"].(obj)["routingMode"] != "GLOBAL" {
		t.Errorf("networks.get: %v", n)
	}
	if fmt.Sprint(n["siloAddressBlocks"]) != fmt.Sprint(blocks) || !strings.Contains(n["description"].(string), blocks[0].(string)) {
		t.Errorf("blocks: %v / %v, want %v", n["siloAddressBlocks"], n["description"], blocks)
	}
	if fmt.Sprint(n["subnetworks"]) != "["+DefaultEndpoint+"projects/silo-"+e.a+"/regions/us-central1/subnetworks/"+e.a+"-sub1]" {
		t.Errorf("subnetworks: %v", n["subnetworks"])
	}
	if l := items(e.ok(e.a, "GET", cv(host, "global/networks"), nil)); fmt.Sprint(names(l)) != "[default]" {
		t.Errorf("networks.list: %v", names(l))
	}
	e.fail(404, "notFound", e.a, "GET", cv(host, "global/networks/silo"), nil)
}

func TestResponseAllowlist(t *testing.T) {
	e := newEnv(t)
	e.gce.AddImage(host, &compute.Image{Name: e.a + "-leaky", Labels: map[string]string{"silo": e.a},
		RawDisk: &compute.ImageRawDisk{Source: "gs://host-bucket/secret.tar.gz"}, SatisfiesPzs: true,
		ShieldedInstanceInitialState: &compute.InitialStateConfig{Pk: &compute.FileContentBuffer{Content: "c2VjcmV0"}}})
	img := e.ok(e.a, "GET", cv(host, "global/images/"+e.a+"-leaky"), nil)
	allowed := map[string]bool{}
	for _, f := range responseFields["compute#image"] {
		allowed[f] = true
	}
	for k := range img {
		if !allowed[k] {
			t.Errorf("images.get shows %s, which isn't allowlisted", k)
		}
	}
	if _, ok := img["rawDisk"]; ok {
		t.Error("rawDisk leaked")
	}
	// A kind with no allowlist is never shown.
	q := &request{n: &namer{silo: "s", host: host}}
	if _, err := q.shape("compute#route", &compute.Route{Name: "r"}); err == nil {
		t.Error("shaped a kind with no allowlist")
	}
}

func TestNaming(t *testing.T) {
	n := &namer{silo: "k7f3qa", host: host, base: DefaultEndpoint, network: "silo", public: map[string]bool{"debian-cloud": true}}
	// In: names pass unchanged, only the project and the shared VPC are
	// mapped.
	for _, c := range []struct{ in, coll, want string }{
		{"k7f3qa-builder", "instances", "k7f3qa-builder"},
		{"default", "networks", "silo"},
		{"n2-standard-8", "machineTypes", "n2-standard-8"},
		{"zones/us-central1-a/disks/k7f3qa-data", "disks", "zones/us-central1-a/disks/k7f3qa-data"},
		{"projects/silo-k7f3qa/zones/us-central1-a/instances/k7f3qa-builder", "", "projects/" + host + "/zones/us-central1-a/instances/k7f3qa-builder"},
		{"projects/" + host + "/global/networks/default", "", "projects/" + host + "/global/networks/silo"},
		{"global/images/k7f3qa-base", "", "global/images/k7f3qa-base"},
		{"http://silo.internal:8080/compute/v1/projects/silo-k7f3qa/zones/us-central1-a/instances/k7f3qa-builder", "",
			gceapi.SelfLinkBase + "projects/" + host + "/zones/us-central1-a/instances/k7f3qa-builder"},
		{"https://www.googleapis.com/compute/v1/projects/silo-k7f3qa/global/images/k7f3qa-base", "",
			gceapi.SelfLinkBase + "projects/" + host + "/global/images/k7f3qa-base"},
		{"https://compute.googleapis.com/compute/v1/projects/silo-k7f3qa/regions/us-central1/subnetworks/k7f3qa-sub1", "",
			gceapi.SelfLinkBase + "projects/" + host + "/regions/us-central1/subnetworks/k7f3qa-sub1"},
		{"projects/silo-k7f3qa/zones/us-central1-a/machineTypes/n2-standard-8", "",
			"projects/" + host + "/zones/us-central1-a/machineTypes/n2-standard-8"},
		{"projects/debian-cloud/global/images/family/debian-12", "", "projects/debian-cloud/global/images/family/debian-12"},
		{"https://www.googleapis.com/compute/v1/projects/debian-cloud/global/images/debian-12-x", "",
			gceapi.SelfLinkBase + "projects/debian-cloud/global/images/debian-12-x"},
		{"k7f3qa-" + strings.Repeat("a", 56), "instances", "k7f3qa-" + strings.Repeat("a", 56)},
	} {
		got, err := n.inLink(c.in, c.coll)
		if err != nil || got != c.want {
			t.Errorf("inLink(%q, %q) = %q, %v; want %q", c.in, c.coll, got, err, c.want)
		}
	}
	const hint = `silo: names in this silo must start with "k7f3qa-"`
	for _, c := range []struct {
		in, coll string
		code     int
		reason   string
		message  string
	}{
		// A bare name, as tenants gave them before SV-76 was revised,
		// in every form.
		{"builder", "instances", 400, "invalid", hint},
		{"zones/z/disks/data", "", 400, "invalid", hint},
		{"projects/silo-k7f3qa/global/images/base", "", 400, "invalid", hint},
		{DefaultEndpoint + "projects/silo-k7f3qa/zones/z/instances/builder", "", 400, "invalid", hint},
		// Another silo's prefix, in this silo's project.
		{"b2c4de-builder", "instances", 400, "invalid", hint},
		{"zones/z/instances/b2c4de-builder", "", 400, "invalid", hint},
		// The prefix alone, and GCE's own limits.
		{"k7f3qa-", "instances", 400, "invalid", "Must be a match of regex"},
		{"k7f3qa-" + strings.Repeat("a", 57), "instances", 400, "invalid", "Must be a match of regex"},
		{"zones/z/instances/k7f3qa-" + strings.Repeat("a", 57), "", 400, "invalid", "Must be a match of regex"},
		{"k7f3qa-Builder", "instances", 400, "invalid", "Must be a match of regex"},
		{"projects/silo-other/zones/z/instances/other-x", "", 403, "forbidden", ""},
		{"https://www.googleapis.com/compute/v1/projects/elsewhere/global/images/x", "", 403, "forbidden", ""},
		{"projects/silo-k7f3qa/global/networks/other", "", 404, "notFound", ""},
		{"other", "networks", 404, "notFound", ""},
	} {
		_, err := n.inLink(c.in, c.coll)
		e := asError(err, nil)
		if err == nil || e.Code != c.code || e.Reason != c.reason || !strings.Contains(e.Message, c.message) {
			t.Errorf("inLink(%q) = %v, want %d %s %q", c.in, err, c.code, c.reason, c.message)
		}
	}
	// A facade on another endpoint (--facade-endpoint, silo proxy)
	// takes back the selfLinks it writes: an instance made from a
	// template sends the template's network as one.
	local := *n
	local.base = "http://127.0.0.1:8642/compute/v1/"
	for _, c := range []struct{ in, want string }{
		{local.base + "projects/silo-k7f3qa/global/networks/default", gceapi.SelfLinkBase + "projects/" + host + "/global/networks/silo"},
		{local.base + "projects/silo-k7f3qa/regions/us-central1/subnetworks/k7f3qa-sub1", gceapi.SelfLinkBase + "projects/" + host + "/regions/us-central1/subnetworks/k7f3qa-sub1"},
	} {
		if got, err := local.inLink(c.in, ""); err != nil || got != c.want {
			t.Errorf("on %s, inLink(%q) = %q, %v; want %q", local.base, c.in, got, err, c.want)
		}
	}
	if _, err := local.inLink(local.base+"projects/silo-other/zones/z/instances/other-x", ""); asError(err, nil).Code != 403 {
		t.Errorf("on %s, another silo's link: %v, want 403", local.base, err)
	}
	// 63 characters, GCE's own limit, prefix included.
	if err := n.checkName("k7f3qa-" + strings.Repeat("a", 56)); err != nil {
		t.Errorf("a 63-character name: %v", err)
	}
	// Out: names unchanged in every form and field; the project and the
	// shared VPC mapped, links on the facade.
	inst := obj{
		"name":     "k7f3qa-builder",
		"selfLink": gceapi.SelfLinkBase + "projects/" + host + "/zones/us-central1-a/instances/k7f3qa-builder",
		"zone":     "https://compute.googleapis.com/compute/v1/projects/" + host + "/zones/us-central1-a",
		"disks":    []any{obj{"source": gceapi.SelfLinkBase + "projects/" + host + "/zones/us-central1-a/disks/k7f3qa-boot"}},
		"networkInterfaces": []any{obj{
			"network":    gceapi.SelfLinkBase + "projects/" + host + "/global/networks/silo",
			"subnetwork": "projects/" + host + "/regions/us-central1/subnetworks/k7f3qa-sub1",
		}},
		"sourceImage": gceapi.SelfLinkBase + "projects/debian-cloud/global/images/debian-12-x",
		"users":       []any{gceapi.SelfLinkBase + "projects/" + host + "/zones/us-central1-a/instances/k7f3qa-a"},
		"labels":      obj{"silo": "k7f3qa", "silo-managed": "true", "team": "x"},
		"tags":        obj{"items": []any{"k7f3qa", "k7f3qa-web"}},
		"description": "made by k7f3qa-builder in projects/" + host + "/zones/z",
	}
	n.out(inst, "")
	base := DefaultEndpoint + "projects/silo-k7f3qa/"
	for path, want := range map[string]any{
		"name": "k7f3qa-builder", "selfLink": base + "zones/us-central1-a/instances/k7f3qa-builder",
		"zone":        base + "zones/us-central1-a",
		"sourceImage": DefaultEndpoint + "projects/debian-cloud/global/images/debian-12-x",
		"description": "made by k7f3qa-builder in projects/silo-k7f3qa/zones/z",
	} {
		if inst[path] != want {
			t.Errorf("out %s = %v, want %v", path, inst[path], want)
		}
	}
	if got := inst["disks"].([]any)[0].(obj)["source"]; got != base+"zones/us-central1-a/disks/k7f3qa-boot" {
		t.Errorf("disks[].source = %v", got)
	}
	ni := inst["networkInterfaces"].([]any)[0].(obj)
	if ni["network"] != base+"global/networks/default" || ni["subnetwork"] != "projects/silo-k7f3qa/regions/us-central1/subnetworks/k7f3qa-sub1" {
		t.Errorf("networkInterfaces: %v", ni)
	}
	if fmt.Sprint(inst["users"]) != "["+base+"zones/us-central1-a/instances/k7f3qa-a]" {
		t.Errorf("users = %v", inst["users"])
	}
	// The facade's own label and tag go; the tenant's prefixed tag stays.
	if fmt.Sprint(inst["labels"]) != "map[team:x]" || fmt.Sprint(inst["tags"].(obj)["items"]) != "[k7f3qa-web]" {
		t.Errorf("labels %v tags %v", inst["labels"], inst["tags"])
	}
	// Free text keeps names as GCE wrote them.
	if got := n.outText("The resource 'projects/" + host + "/zones/z/instances/k7f3qa-builder' already exists"); got != "The resource 'projects/silo-k7f3qa/zones/z/instances/k7f3qa-builder' already exists" {
		t.Errorf("outText = %q", got)
	}
}

func TestRequestChecks(t *testing.T) {
	tr := facade.AllowFields([]string{"name", "labels", "networkInterfaces[].subnetwork", "metadata.items"})
	ok := []string{
		`{"name": "x", "labels": {"a": "b"}}`,
		`{"networkInterfaces": [{"subnetwork": "s", "accessConfigs": []}]}`,
		`{"canIpForward": false, "metadata": {"items": [{"key": "k"}]}}`,
		`{"scheduling": {"preemptible": false}}`,
	}
	for _, b := range ok {
		var v map[string]any
		json.Unmarshal([]byte(b), &v)
		if err := checkFields(v, tr); err != nil {
			t.Errorf("%s: %v", b, err)
		}
	}
	for b, field := range map[string]string{
		`{"networkInterfaces": [{"subnetwork": "s"}, {"accessConfigs": [{"type": "ONE_TO_ONE_NAT"}]}]}`: "networkInterfaces[1].accessConfigs",
		`{"canIpForward": true}`:                         "canIpForward",
		`{"scheduling": {"nodeAffinities": [{"k": 1}]}}`: "scheduling",
		`{"metadata": {"fingerprint": "x"}}`:             "metadata.fingerprint",
	} {
		var v map[string]any
		json.Unmarshal([]byte(b), &v)
		err := checkFields(v, tr)
		e := asError(err, nil)
		if err == nil || e.Code != 403 || e.Message != "silo: field "+field+" is not allowed" {
			t.Errorf("%s: %v, want field %s refused", b, err, field)
		}
	}
	for _, b := range []string{`{"labels": {"silo": "x"}}`, `{"labels": {"silo-managed": "true"}}`, `{"tags": {"items": ["silo-web"]}}`,
		`{"targetTags": ["siloX"]}`, `{"properties": {"labels": {"silox": "1"}}}`} {
		var v map[string]any
		json.Unmarshal([]byte(b), &v)
		if err := checkReserved(v, ""); err == nil || asError(err, nil).Code != 403 {
			t.Errorf("%s: %v, want it refused", b, err)
		}
	}
	var v map[string]any
	json.Unmarshal([]byte(`{"labels": {"team": "x"}, "tags": {"items": ["web"]}, "metadata": {"items": [{"key": "silo-ish"}]}}`), &v)
	if err := checkReserved(v, ""); err != nil {
		t.Errorf("tenant labels refused: %v", err)
	}
}

func TestParsePath(t *testing.T) {
	for _, c := range []struct{ verb, path, want string }{
		{"GET", "projects/p", "projects.get"},
		{"POST", "projects/p/setCommonInstanceMetadata", "projects.setCommonInstanceMetadata"},
		{"GET", "projects/p/zones", "zones.list"},
		{"GET", "projects/p/zones/z", "zones.get"},
		{"GET", "projects/p/aggregated/machineTypes", "machineTypes.aggregatedList"},
		{"POST", "projects/p/zones/z/instances", "instances.insert"},
		{"POST", "projects/p/zones/z/instances/i/start", "instances.start"},
		{"GET", "projects/p/zones/z/instances/i/serialPort", "instances.getSerialPortOutput"},
		{"POST", "projects/p/zones/z/operations/o/wait", "zoneOperations.wait"},
		{"GET", "projects/p/regions/r/operations/o", "regionOperations.get"},
		{"GET", "projects/p/global/operations", "globalOperations.list"},
		{"GET", "projects/p/global/images/family/f", "images.getFromFamily"},
		{"POST", "projects/p/regions/r/instanceTemplates", "regionInstanceTemplates.insert"},
		{"PATCH", "projects/p/regions/r/subnetworks/s", "subnetworks.patch"},
		{"PUT", "projects/p/global/firewalls/f", "firewalls.update"},
		{"DELETE", "projects/p/global/snapshots/s", "snapshots.delete"},
	} {
		got, ok := parsePath(c.verb, c.path)
		if !ok || got.FullMethod() != c.want {
			t.Errorf("parsePath(%s %s) = %s, want %s", c.verb, c.path, got.FullMethod(), c.want)
		}
	}
}

// TestWritesAndOperations drives the framework's write path with a
// disks.insert the test registers, as the resource tasks will: the
// field allowlist, charging, audit with payer, home and via, the
// operation rewritten, who may see it, and the refund on failure.
func TestWritesAndOperations(t *testing.T) {
	// A stand-in disks.insert, so this test reads as the framework alone;
	// the real one is restored after.
	realInsert, realFields := handlers["disks.insert"], requestFields["disks.insert"]
	handlers["disks.insert"] = func(s *Server, q *request) (any, error) {
		name, _ := q.body["name"].(string)
		if err := q.n.checkName(name); err != nil {
			return nil, err
		}
		size, _ := q.body["sizeGb"].(string)
		var gb int64
		fmt.Sscan(size, &gb)
		object := "disk:" + q.call.Zone + "/" + name
		amount := []ledger.Amount{{Key: ledger.Key{Kind: "gce", Class: "ssd_disk_gb", Region: gceapi.ZoneRegion(q.call.Zone)}, Amount: ledger.Units(gb)}}
		err := s.st.Tx(q.ctx(), func(tx store.Tx) error {
			return ledger.Charge(q.ctx(), tx, ledger.ChargeRequest{Object: object, Silo: q.target.ID, Via: q.via(), Amounts: amount})
		})
		if err != nil {
			s.audit(q, asError(err, nil).Reason, nil, nil)
			return nil, err
		}
		op, err := s.gce.InsertDisk(q.ctx(), s.opts.HostProject, q.call.Zone, &compute.Disk{Name: name, SizeGb: gb,
			Type: "zones/" + q.call.Zone + "/diskTypes/pd-ssd", Labels: map[string]string{"silo": q.target.ID, "silo-managed": "true"}})
		if err != nil {
			s.st.Tx(q.ctx(), func(tx store.Tx) error { _, err := ledger.Refund(q.ctx(), tx, object, nil); return err })
			return nil, err
		}
		s.audit(q, "OK", nil, []fenceapi.ChargeLine{{Payer: q.target.ID, Home: q.target.ID, Via: q.via(), Kind: "gce", Class: "ssd_disk_gb", Region: "us-central1", Amount: gb}})
		return s.started(q, op, object, nil)
	}
	requestFields["disks.insert"] = []string{"name", "sizeGb", "labels"}
	t.Cleanup(func() { handlers["disks.insert"], requestFields["disks.insert"] = realInsert, realFields })

	e := newEnv(t)
	used := func(silo string) float64 {
		r := e.ok(silo, "GET", cv("silo-"+silo, "regions/us-central1"), nil)
		_, u, _ := quotaOf(r, "SSD_TOTAL_GB")
		return u
	}
	zone := "zones/us-central1-a/disks"
	msg := e.fail(403, "forbidden", e.a, "POST", cv(host, zone), obj{"name": e.a + "-d1", "sizeGb": "10", "storagePool": "p"})
	if msg != "silo: field storagePool is not allowed" {
		t.Errorf("field refusal: %q", msg)
	}
	e.fail(403, "forbidden", e.a, "POST", cv(host, zone), obj{"name": e.a + "-d1", "labels": obj{"silo": "x"}})
	e.fail(400, "invalid", e.a, "POST", cv(host, zone), obj{"name": e.a + "-" + strings.Repeat("d", 57), "sizeGb": "1"})
	e.fail(403, "regionNotAllowed", e.a, "POST", cv(host, "zones/us-east1-b/disks"), obj{"name": e.a + "-d1"})
	e.fail(403, "quotaExceeded", e.a, "POST", cv(host, zone), obj{"name": e.a + "-big", "sizeGb": "1000"})

	op := e.ok(e.a, "POST", cv(host, zone+"?requestId=r1"), obj{"name": e.a + "-d1", "sizeGb": "10", "labels": obj{"env": "x"}})
	opPath := "zones/us-central1-a/operations/" + op["name"].(string)
	base := DefaultEndpoint + "projects/silo-" + e.a + "/"
	if op["user"] != "silo:"+e.a || op["targetLink"] != base+"zones/us-central1-a/disks/"+e.a+"-d1" ||
		op["selfLink"] != base+opPath || op["zone"] != base+"zones/us-central1-a" {
		t.Errorf("operation: %v", op)
	}
	if used(e.a) != 10 {
		t.Errorf("used %v after insert", used(e.a))
	}
	recs := e.audit(e.a, KindFacadeCall)
	last := recs[len(recs)-1]
	f := last["fields"].(obj)
	if last["result"] != "OK" || f["name"] != e.a+"-d1" || f["method"] != "compute.disks.insert" ||
		last["via"] != nil || last["request_id"] != "r1" {
		t.Errorf("audit: %v", last)
	}
	if ch := f["charges"].([]any)[0].(obj); ch["payer"] != e.a || ch["home"] != e.a || ch["via"] != "facade" {
		t.Errorf("charges: %v", ch)
	}

	// Who may see the operation: the silo and its ancestors, in its
	// project; nobody in another project.
	e.ok(e.a, "GET", cv(host, opPath), nil)
	e.ok(e.root, "GET", cv("silo-"+e.a, opPath), nil)
	e.fail(404, "notFound", e.root, "GET", cv(host, opPath), nil)
	e.fail(404, "notFound", e.ci, "GET", cv(host, opPath), nil)
	e.fail(403, "forbidden", e.b, "GET", cv("silo-"+e.a, opPath), nil)
	e.fail(404, "notFound", e.a, "POST", cv("silo-"+e.ci, opPath+"/wait"), nil)
	if l := items(e.ok(e.a, "GET", cv(host, "zones/us-central1-a/operations"), nil)); len(l) != 1 || l[0]["user"] != "silo:"+e.a {
		t.Errorf("operations.list: %v", l)
	}
	if l := items(e.ok(e.b, "GET", cv(host, "zones/us-central1-a/operations"), nil)); len(l) != 0 {
		t.Errorf("team-b sees %v", l)
	}
	e.gce.RunOperations()
	if w := e.ok(e.a, "POST", cv(host, opPath+"/wait"), nil); w["status"] != "DONE" {
		t.Errorf("wait: %v", w)
	}

	// An ancestor writes into team-a: team-a's object and quota, audited
	// as via=ancestor:<root>, the operation's user the root.
	e.gce.FailOperation(fake.Failure{Method: "disks.insert", Kind: fake.FailQuota})
	op2 := e.ok(e.root, "POST", cv("silo-"+e.a, zone), obj{"name": e.a + "-d2", "sizeGb": "20"})
	if op2["user"] != "silo:"+e.root {
		t.Errorf("ancestor's operation user: %v", op2["user"])
	}
	recs = e.audit(e.a, KindFacadeCall)
	if last := recs[len(recs)-1]; last["via"] != "ancestor:"+e.root || last["fields"].(obj)["charges"].([]any)[0].(obj)["via"] != "ancestor:"+e.root {
		t.Errorf("ancestor audit: %v", last)
	}
	if used(e.a) != 30 {
		t.Errorf("used %v after the ancestor's insert", used(e.a))
	}
	// The operation fails; silod sees it and refunds.
	e.gce.RunOperations()
	deadline := time.Now().Add(10 * time.Second)
	for used(e.a) != 10 {
		if time.Now().After(deadline) {
			t.Fatalf("no refund: used %v", used(e.a))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(e.audit(e.a, KindFacadeRefund)) != 1 {
		t.Error("no facade.refund record")
	}
	var recsOps []Operation
	e.db.Tx(ctx, func(tx store.Tx) error { var err error; recsOps, err = Operations(ctx, tx, e.a); return err })
	sort.Slice(recsOps, func(i, j int) bool { return recsOps[i].Object < recsOps[j].Object })
	if len(recsOps) != 2 || !recsOps[1].Failed || !recsOps[1].Done || recsOps[1].Caller != e.root {
		t.Errorf("operation records: %+v", recsOps)
	}
}

func TestUnavailable(t *testing.T) {
	e := newEnv(t)
	why := "schema_mismatch"
	e.facade.opts.Ready = func() string { return why }
	e.fail(503, "backendError", e.a, "GET", cv(host, "zones"), nil)
	why = ""
	e.db.Close()
	e.fail(503, "backendError", e.a, "GET", cv(host, "zones"), nil)
}
