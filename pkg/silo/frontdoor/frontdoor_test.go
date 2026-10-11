package frontdoor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	facade "github.com/bwsalmon/silo/pkg/silo/facade/gce"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/frontdoor"
	"github.com/bwsalmon/silo/pkg/silo/frontdoor/frontdoortest"
	"github.com/bwsalmon/silo/pkg/silo/gce/fake"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
	compute "google.golang.org/api/compute/v1"
)

var ctx = context.Background()

const host = "host-project"

var audience = frontdoor.Audience(host)

type obj = map[string]any

// env is silod on SQLite, serving the fence API and the facade over a
// fake GCE, with the front door in front of it and tokens signed by a
// local key the front door's verifier trusts.
type env struct {
	t      *testing.T
	db     store.Database
	silod  http.Handler // facade and fence API, the caller from the context
	url    string       // the front door's
	signer *frontdoortest.Signer
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
	gce := fake.New(fake.Options{Project: host})
	if _, err := gce.InsertNetwork(ctx, host, &compute.Network{Name: "silo"}); err != nil {
		t.Fatal(err)
	}
	api := fenceapi.New(db, fenceapi.Options{})
	fc := facade.New(db, facade.Options{GCE: gce, HostProject: host})
	mux := http.NewServeMux()
	mux.Handle("/compute/v1/", fc)
	mux.Handle("/", api)
	s := frontdoortest.New(t)
	srv := httptest.NewServer(frontdoor.Handler(db, frontdoor.Options{Verifier: s.Verifier(audience), API: mux}))
	t.Cleanup(func() {
		srv.Close()
		fc.Close()
		api.Close()
	})
	return &env{t: t, db: db, silod: mux, url: srv.URL, signer: s}
}

// bootstrap creates the root through the front door, as install does,
// with installer's token; admin is mapped to the root.
func (e *env) bootstrap(installer, admin string) string {
	e.t.Helper()
	code, root := e.call(e.signer.Token(installer, audience), "POST", frontdoor.BootstrapPath, obj{
		"regions":  []string{"us-central1"},
		"capacity": obj{"us-central1": obj{"vcpu": 100, "memory_mb": 409600, "ssd_disk_gb": 1000}, "global": obj{"subnetworks": 20, "images": 10, "firewall_rules": 20}},
		"admins":   []string{admin},
	})
	if code != 200 {
		e.t.Fatalf("bootstrap: %d %v", code, root)
	}
	return root["id"].(string)
}

// call calls the front door with token ("" for none), and headers.
func (e *env) call(token, method, path string, body any, headers ...string) (int, obj) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.url+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
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

// as calls silod directly as caller, bypassing the front door, to set
// the scene.
func (e *env) as(caller, method, path string, body any) obj {
	e.t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r = r.WithContext(fenceapi.ContextWithCaller(r.Context(), fenceapi.Caller{Silo: caller, Via: fenceapi.ViaInternal}))
	w := httptest.NewRecorder()
	e.silod.ServeHTTP(w, r)
	out := obj{}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 {
		e.t.Fatalf("%s %s as %s: %d %v", method, path, caller, w.Code, out)
	}
	return out
}

// mk creates a child of the root, as the root, and maps member to it.
func (e *env) mk(root, name, member string, extra obj) string {
	e.t.Helper()
	body := obj{"name": name, "address": obj{"prefix_length": 16}, "regions": []string{"us-central1"},
		"quota": obj{"us-central1": obj{"vcpu": 4}, "global": obj{"subnetworks": 3}}}
	for k, v := range extra {
		body[k] = v
	}
	id := e.as(root, "POST", "/silo/v1/silos/-"+root+"/children", body)["id"].(string)
	if member != "" {
		e.as(root, "PUT", "/silo/v1/silos/"+name+"/members/"+member, obj{})
	}
	return id
}

func fenceCode(out obj) string {
	er, _ := out["error"].(obj)
	c, _ := er["code"].(string)
	return c
}

func fenceMessage(out obj) string {
	er, _ := out["error"].(obj)
	m, _ := er["message"].(string)
	return m
}

func gceReason(out obj) string {
	er, _ := out["error"].(obj)
	errs, _ := er["errors"].([]any)
	if len(errs) == 0 {
		return ""
	}
	r, _ := errs[0].(obj)["reason"].(string)
	return r
}

func TestTokenVerification(t *testing.T) {
	e := newEnv(t)
	root := e.bootstrap("installer@example.com", "user:admin@example.com")
	a := e.mk(root, "a", "user:alice@example.com", nil)
	e.mk(root, "b", "serviceAccount:ci@proj.iam.gserviceaccount.com", nil)

	other := frontdoortest.New(t)
	expired := frontdoortest.Valid("alice@example.com", audience)
	expired["iat"], expired["exp"] = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	notGoogle := frontdoortest.Valid("alice@example.com", audience)
	notGoogle["iss"] = "https://evil.example.com"
	unverified := frontdoortest.Valid("alice@example.com", audience)
	unverified["email_verified"] = false
	noEmail := frontdoortest.Valid("alice@example.com", audience)
	delete(noEmail, "email")

	for _, tc := range []struct {
		name, token, header string
		want                int
		msg                 string
	}{
		{name: "no token", want: 403, msg: "needs Authorization"},
		{name: "not a bearer", header: "Basic YWxpY2U6eA==", want: 403, msg: "needs Authorization"},
		{name: "garbage", token: "not.a.jwt", want: 403, msg: "not valid"},
		{name: "bad signature", token: other.Token("alice@example.com", audience), want: 403, msg: "not valid"},
		{name: "wrong audience", token: e.signer.Token("alice@example.com", "silo:another-project"), want: 403, msg: "audience"},
		{name: "a Google client's audience", token: e.signer.Token("alice@example.com", "32555940559.apps.googleusercontent.com"), want: 403, msg: "audience"},
		{name: "expired", token: e.signer.Sign(expired), want: 403, msg: "expired"},
		{name: "not issued by Google", token: e.signer.Sign(notGoogle), want: 403, msg: "issuer"},
		{name: "unverified email", token: e.signer.Sign(unverified), want: 403, msg: "not verified"},
		{name: "no email", token: e.signer.Sign(noEmail), want: 403, msg: "no email"},
		{name: "unmapped", token: e.signer.Token("mallory@example.com", audience), want: 403, msg: "user:mallory@example.com is not a member of any silo"},
		{name: "the installer, once the root exists", token: e.signer.Token("installer@example.com", audience), want: 403, msg: "not a member"},
		{name: "mapped", token: e.signer.Token("alice@example.com", audience), want: 200},
		{name: "mapped service account", token: e.signer.Token("ci@proj.iam.gserviceaccount.com", audience), want: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var headers []string
			if tc.header != "" {
				headers = []string{"Authorization", tc.header}
			}
			code, out := e.call(tc.token, "GET", "/silo/v1/silos/self", nil, headers...)
			if code != tc.want {
				t.Fatalf("got %d %v, want %d", code, out, tc.want)
			}
			if tc.want != 200 {
				if fenceCode(out) != string(fenceapi.PermissionDenied) || !strings.Contains(fenceMessage(out), tc.msg) {
					t.Errorf("got %v, want PERMISSION_DENIED saying %q", out, tc.msg)
				}
				// The facade refuses in its own shape.
				code, out := e.call(tc.token, "GET", "/compute/v1/projects/"+host+"/regions/us-central1/subnetworks", nil, headers...)
				if code != 403 || gceReason(out) != facade.ReasonForbidden {
					t.Errorf("facade: got %d %v, want 403 forbidden", code, out)
				}
			}
		})
	}

	// A mapped member is its silo, exactly as a VM inside it is.
	tok := e.signer.Token("alice@example.com", audience)
	if _, out := e.call(tok, "GET", "/silo/v1/silos/self", nil); out["id"] != a || out["path"] != "/a" {
		t.Errorf("alice's self: %v, want /a (%s)", out, a)
	}
	if code, out := e.call(tok, "GET", "/compute/v1/projects/"+host+"/regions/us-central1/subnetworks", nil); code != 200 {
		t.Errorf("alice's facade read: %d %v", code, out)
	}
	// It reaches its own subtree and nothing beside it.
	if code, out := e.call(tok, "GET", "/silo/v1/silos/b", nil); code != 404 {
		t.Errorf("alice reading /b: %d %v, want 404", code, out)
	}
	// Headers meant for silod's other transports buy nothing: an unmapped
	// caller claiming the root is still unmapped, and a mapped one is
	// still its own silo.
	if code, out := e.call(e.signer.Token("mallory@example.com", audience), "GET", "/silo/v1/silos/self", nil,
		"Silo-Caller", root, "Silo-Origin", root+";v=999999"); code != 403 {
		t.Errorf("mallory claiming the root: %d %v, want 403", code, out)
	}
	if _, out := e.call(tok, "GET", "/silo/v1/silos/self", nil, "Silo-Caller", root); out["id"] != a {
		t.Errorf("alice claiming the root: %v, want /a", out)
	}
	// Unmapping takes effect on the next call: the table is read on each.
	e.as(root, "DELETE", "/silo/v1/silos/a/members/user:alice@example.com", obj{})
	if code, out := e.call(tok, "GET", "/silo/v1/silos/self", nil); code != 403 {
		t.Errorf("alice after unmapping: %d %v, want 403", code, out)
	}
	// The front door serves only the APIs.
	if code, _ := e.call(tok, "GET", "/silod/gateway/tables", nil); code != 404 {
		t.Errorf("silod's gateway path: %d, want 404", code)
	}
}

func TestControlPolicyBindsMembers(t *testing.T) {
	e := newEnv(t)
	root := e.bootstrap("installer@example.com", "user:admin@example.com")
	e.mk(root, "ro", "user:reader@example.com", obj{"control": "read"})
	e.mk(root, "sealed", "user:sealed@example.com", obj{"control": "none"})
	sub := "/compute/v1/projects/" + host + "/regions/us-central1/subnetworks"
	newSubnet := obj{"name": "web", "ipCidrRange": "10.200.0.0/26"}

	// A member of a sealed silo gets nothing on the control APIs, reads
	// included, on either API.
	sealed := e.signer.Token("sealed@example.com", audience)
	for _, c := range []struct{ method, path string }{
		{"GET", "/silo/v1/silos/self"},
		{"GET", "/silo/v1/silos/self/children"},
		{"PATCH", "/silo/v1/silos/self"},
	} {
		if code, out := e.call(sealed, c.method, c.path, obj{}); code != 403 || fenceCode(out) != string(fenceapi.ControlDenied) {
			t.Errorf("sealed member %s %s: %d %v, want CONTROL_DENIED", c.method, c.path, code, out)
		}
	}
	if code, out := e.call(sealed, "GET", sub, nil); code != 403 || gceReason(out) != facade.ReasonForbidden {
		t.Errorf("sealed member's facade read: %d %v, want 403 forbidden", code, out)
	}

	// A member of a read-only silo reads, and changes nothing.
	ro := e.signer.Token("reader@example.com", audience)
	if code, out := e.call(ro, "GET", "/silo/v1/silos/self", nil); code != 200 {
		t.Errorf("read member's describe: %d %v", code, out)
	}
	if code, out := e.call(ro, "GET", sub, nil); code != 200 {
		t.Errorf("read member's facade read: %d %v", code, out)
	}
	if code, out := e.call(ro, "POST", "/silo/v1/silos/self/children", obj{"name": "x", "address": obj{"prefix_length": 24}}); code != 403 || fenceCode(out) != string(fenceapi.ControlDenied) {
		t.Errorf("read member's create: %d %v, want CONTROL_DENIED", code, out)
	}
	if code, out := e.call(ro, "POST", sub, newSubnet); code != 403 || gceReason(out) != facade.ReasonForbidden {
		t.Errorf("read member's facade insert: %d %v, want 403 forbidden", code, out)
	}

	// Both refusals are audited with the member as the actor.
	recs := e.as(root, "GET", "/silo/v1/silos/sealed/audit", nil)["records"].([]any)
	found := false
	for _, r := range recs {
		r := r.(obj)
		actor := r["actor"].(obj)
		if r["kind"] == fenceapi.KindControlDenied && actor["member"] == "user:sealed@example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("no control.denied record naming the sealed member: %v", recs)
	}
}

func TestAuditActorIsTheMember(t *testing.T) {
	e := newEnv(t)
	root := e.bootstrap("installer@example.com", "user:admin@example.com")
	admin := e.signer.Token("admin@example.com", audience)
	code, out := e.call(admin, "POST", "/silo/v1/silos/self/children", obj{
		"name": "team", "address": obj{"prefix_length": 20}, "regions": []string{"us-central1"},
	})
	if code != 200 {
		t.Fatalf("admin's create: %d %v", code, out)
	}
	recs := e.as(root, "GET", "/silo/v1/silos/team/audit", nil)["records"].([]any)
	var create obj
	for _, r := range recs {
		if r.(obj)["kind"] == fenceapi.KindSiloCreate {
			create = r.(obj)
		}
	}
	if create == nil {
		t.Fatalf("no silo.create record: %v", recs)
	}
	if actor := create["actor"].(obj); actor["silo"] != root || actor["member"] != "user:admin@example.com" {
		t.Errorf("silo.create: actor %v, want the root, member user:admin@example.com", create["actor"])
	}
}

func TestBootstrapThroughTheFrontDoor(t *testing.T) {
	e := newEnv(t)
	installer := e.signer.Token("installer@example.com", audience)
	// Before the root, a verified caller may see whether silod serves;
	// an unverified one may not.
	if code, out := e.call(installer, "GET", frontdoor.HealthPath, nil); code != 200 {
		t.Errorf("health: %d %v", code, out)
	}
	if code, _ := e.call("", "GET", frontdoor.HealthPath, nil); code != 403 {
		t.Errorf("health with no token: %d, want 403", code)
	}
	// Bootstrap needs a valid token for the audience, like every call.
	if code, out := e.call(e.signer.Token("installer@example.com", "silo:elsewhere"), "POST", frontdoor.BootstrapPath, obj{}); code != 403 {
		t.Errorf("bootstrap with the wrong audience: %d %v, want 403", code, out)
	}
	if code, out := e.call(frontdoortest.New(t).Token("installer@example.com", audience), "POST", frontdoor.BootstrapPath, obj{}); code != 403 {
		t.Errorf("bootstrap with a bad signature: %d %v, want 403", code, out)
	}
	root := e.bootstrap("installer@example.com", "user:admin@example.com")
	// The root's creation is audited with the installer as the member.
	recs := e.as(root, "GET", "/silo/v1/silos/self/audit", nil)["records"].([]any)
	found := false
	for _, r := range recs {
		r := r.(obj)
		if r["kind"] == fenceapi.KindSiloCreate && r["actor"].(obj)["member"] == "user:installer@example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("no silo.create record naming the installer: %v", recs)
	}
	// Once the root exists, bootstrap is refused to everyone, a root
	// member included, at the front door.
	for _, who := range []string{"installer@example.com", "admin@example.com"} {
		if code, out := e.call(e.signer.Token(who, audience), "POST", frontdoor.BootstrapPath, obj{}); code != 403 || !strings.Contains(fenceMessage(out), "root silo exists") {
			t.Errorf("bootstrap by %s after the root: %d %v, want 403", who, code, out)
		}
	}
	// And the admin it mapped is the root.
	if _, out := e.call(e.signer.Token("admin@example.com", audience), "GET", "/silo/v1/silos/self", nil); out["id"] != root {
		t.Errorf("admin's self: %v, want the root", out)
	}
}

func TestIdentity(t *testing.T) {
	for in, want := range map[string]string{
		"alice@example.com":                         "user:alice@example.com",
		"ci@proj.iam.gserviceaccount.com":           "serviceAccount:ci@proj.iam.gserviceaccount.com",
		"123-compute@developer.gserviceaccount.com": "serviceAccount:123-compute@developer.gserviceaccount.com",
	} {
		if got := frontdoor.Identity(in); got != want {
			t.Errorf("Identity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewGoogleNeedsAnAudience(t *testing.T) {
	if _, err := frontdoor.NewGoogle(ctx, frontdoor.Audience("")); err == nil {
		t.Error("NewGoogle with no host project: no error")
	}
	g := &frontdoor.Google{Validator: frontdoortest.New(t).Verifier(audience).Validator}
	if _, err := g.Verify(ctx, "x"); err == nil {
		t.Error("Verify with no audience: no error")
	}
}
