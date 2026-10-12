package fenceapi_test

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/internal/pgtest"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/postgres"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/tree"
)

var ctx = context.Background()

type env struct {
	t    *testing.T
	db   store.Database
	api  *fenceapi.Server
	srv  *httptest.Server
	root string
}

type obj = map[string]any

// onPostgres switches newEnv to a throwaway Postgres server, for
// TestPostgres. The tests run on SQLite otherwise, as the go job has.
var onPostgres bool

func newEnv(t *testing.T, opts fenceapi.Options) *env {
	t.Helper()
	return newEnvBody(t, opts, nil)
}

// newEnvBody is newEnv with edit making changes to the bootstrap body.
func newEnvBody(t *testing.T, opts fenceapi.Options, edit func(obj)) *env {
	t.Helper()
	var db store.Database
	var err error
	if onPostgres {
		srv := pgtest.Start(t)
		db, err = postgres.Open(ctx, srv.URL)
	} else {
		db, err = sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	opts.Caller = func(r *http.Request) (fenceapi.Caller, error) {
		if a := r.Header.Get("Silo-Agent"); a != "" {
			// An agent's harness, on its session socket.
			return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaHarness, Agent: a}, nil
		}
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal, Member: r.Header.Get("Silo-Member")}, nil
	}
	e := &env{t: t, db: db}
	e.api = fenceapi.New(db, opts)
	e.srv = httptest.NewServer(e.api)
	t.Cleanup(func() { e.srv.Close(); e.api.Close() })
	body := obj{
		"regions":          []string{"us-central1", "us-east1"},
		"capacity":         obj{"us-central1": obj{"vcpu": 100, "instances": 50}, "us-east1": obj{"vcpu": 10}, "global": obj{"subnetworks": 20}},
		"model_budget_usd": "1000",
		"grant_policy":     obj{"exports": "allow", "links": "allow", "allocators": "allow"},
		"agent_policy":     obj{"credentials": []string{"anthropic-api", "openai-api"}},
		"egress":           obj{"allow": []string{"*", "*:873"}},
		"admins":           []string{"user:admin@example.com"},
	}
	if edit != nil {
		edit(body)
	}
	code, root := e.do("", "POST", "/silod/bootstrap", body)
	if code != 200 {
		t.Fatalf("bootstrap: %d %v", code, root)
	}
	e.root = root["id"].(string)
	return e
}

// do makes a call as caller (a silo id, or "" for none) and returns the
// status and the decoded body.
func (e *env) do(caller, method, path string, body any) (int, obj) {
	e.t.Helper()
	code, out, _ := e.doH(caller, method, path, body)
	return code, out
}

func (e *env) doH(caller, method, path string, body any) (int, obj, http.Header) {
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
	if caller, agent, ok := strings.Cut(caller, "/agent:"); ok {
		req.Header.Set("Silo-Caller", caller)
		req.Header.Set("Silo-Agent", agent)
	} else if caller != "" {
		req.Header.Set("Silo-Caller", caller)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := obj{}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s %s: %d, body %q is not JSON", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode, out, resp.Header
}

// ok makes a call that must succeed.
func (e *env) ok(caller, method, path string, body any) obj {
	e.t.Helper()
	code, out := e.do(caller, method, path, body)
	if code != 200 {
		e.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out
}

// fail makes a call that must fail with code, and returns the error.
func (e *env) fail(code fenceapi.Code, caller, method, path string, body any) obj {
	e.t.Helper()
	status, out := e.do(caller, method, path, body)
	er, _ := out["error"].(obj)
	if er == nil || er["code"] != string(code) || status != code.Status() {
		e.t.Fatalf("%s %s: %d %v, want %s (%d)", method, path, status, out, code, code.Status())
	}
	return er
}

// mk creates a child of parent (a path without its leading /, or
// -id) as caller, and returns its id.
func (e *env) mk(caller, parent, name string, extra obj) string {
	e.t.Helper()
	bits := 24
	if parent == "-"+e.root || (parent == "self" && caller == e.root) {
		bits = 16
	}
	body := obj{"name": name, "address": obj{"prefix_length": bits}}
	for k, v := range extra {
		body[k] = v
	}
	return e.ok(caller, "POST", "/silo/v1/silos/"+parent+"/children", body)["id"].(string)
}

func get(o obj, path ...string) any {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(obj)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func str(v any) string { return fmt.Sprint(v) }

func TestBootstrapAndDescribe(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	e.fail(fenceapi.AlreadyExists, "", "POST", "/silod/bootstrap", obj{})
	r := e.ok(e.root, "GET", "/silo/v1/self", nil)
	for path, want := range map[string]string{
		"path":                              "/",
		"parent":                            "<nil>",
		"depth":                             "0",
		"backing.kind":                      "subnet",
		"state":                             "ACTIVE",
		"quota.us-central1.vcpu.granted":    "100",
		"quota.global.subnetworks.granted":  "20",
		"model_budget.granted_usd":          "1000.00",
		"model_budget.period":               "month",
		"control.own":                       "full",
		"control.effective":                 "full",
		"control.effective_from":            "/",
		"agent_policy.effective.max_lease":  "168h",
		"grant_policy.effective.allocators": "allow",
		"address.blocks":                    "[10.0.0.0/8]",
		"facade.project":                    "silo-" + e.root,
		"facade.name_prefix":                e.root + "-",
	} {
		if got := str(get(r, strings.Split(path, ".")...)); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	if got := str(r["over_committed"]); got != "[]" {
		t.Errorf("over_committed = %s", got)
	}
	// By id, too, and the same object.
	if r2 := e.ok(e.root, "GET", "/silo/v1/silos/-"+e.root, nil); r2["id"] != e.root {
		t.Errorf("by id: %v", r2["id"])
	}
	// No caller: refused.
	e.fail(fenceapi.PermissionDenied, "", "GET", "/silo/v1/self", nil)
	e.fail(fenceapi.NotFound, "zzzzzz", "GET", "/silo/v1/self", nil)
}

func TestCreateAndChecks(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "-"+e.root, "team-a", obj{
		"regions":          []string{"us-central1"},
		"quota":            obj{"us-central1": obj{"vcpu": 16, "instances": 4}, "global": obj{"subnetworks": 2}},
		"model_budget_usd": "50.00",
		"address":          obj{"range": "10.0.0.0/16"},
		"max_blocks":       6,
		"labels":           obj{"team": "a"},
		"unknown_field":    "ignored",
	})
	d := e.ok(e.root, "GET", "/silo/v1/silos/team-a", nil)
	for path, want := range map[string]string{
		"path": "/team-a", "parent": e.root, "depth": "1", "regions": "[us-central1]",
		"quota.us-central1.vcpu.granted": "16", "quota.global.subnetworks.granted": "2",
		"model_budget.granted_usd": "50.00", "address.max_blocks": "6", "labels.team": "a",
		"egress.own.allow": "[]", "egress.effective.allow": "[]",
		"grant_policy.own.allocators": "allow", "control.own": "full",
		"agent_policy.effective.credentials": "[anthropic-api openai-api]",
	} {
		if got := str(get(d, strings.Split(path, ".")...)); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	if d["id"] != a {
		t.Fatalf("id %v, want %s", d["id"], a)
	}
	r := e.ok(e.root, "GET", "/silo/v1/self", nil)
	if got := str(get(r, "quota", "us-central1", "vcpu", "delegated")); got != "16" {
		t.Errorf("root delegated vcpu = %s", got)
	}
	if got := str(get(r, "model_budget", "delegated_usd")); got != "50.00" {
		t.Errorf("root delegated_usd = %s", got)
	}
	if got := str(get(r, "address", "delegated")); !strings.Contains(got, "/team-a") {
		t.Errorf("root delegated ranges = %s", got)
	}

	// The parent creates, and so does an ancestor of the parent.
	e.mk(a, "self", "ci", obj{"quota": obj{"us-central1": obj{"vcpu": 4}}, "address": obj{"prefix_length": 20}})
	e.mk(e.root, "team-a", "web", obj{"address": obj{"range": "10.0.32.0/20"}})
	if got := str(get(e.ok(e.root, "GET", "/silo/v1/silos/team-a/web", nil), "address", "blocks")); got != "[10.0.32.0/20]" {
		t.Errorf("web blocks = %s", got)
	}

	children := func(body obj) obj { body["address"] = obj{"prefix_length": 24}; return body }
	path := "/silo/v1/silos/team-a/children"
	e.fail(fenceapi.InvalidArgument, a, "POST", path, children(obj{"name": "Bad_Name"}))
	e.fail(fenceapi.AlreadyExists, a, "POST", path, children(obj{"name": "ci"}))
	er := e.fail(fenceapi.QuotaExceeded, a, "POST", path, children(obj{"name": "x", "quota": obj{"us-central1": obj{"vcpu": 13}}}))
	if det := str(er["details"]); !strings.Contains(det, "class:vcpu") || !strings.Contains(det, "region:us-central1") ||
		!strings.Contains(det, "asked:13") || !strings.Contains(det, "free:12") {
		t.Errorf("QUOTA_EXCEEDED details = %s", det)
	}
	e.fail(fenceapi.QuotaExceeded, a, "POST", path, children(obj{"name": "x", "model_budget_usd": "50.01"}))
	e.fail(fenceapi.LooserThanParent, a, "POST", path, children(obj{"name": "x", "regions": []string{"us-east1"}}))
	e.fail(fenceapi.Unimplemented, a, "POST", path, children(obj{"name": "x", "backing": obj{"kind": "vpc"}}))
	e.fail(fenceapi.Unimplemented, a, "POST", path, children(obj{"name": "x", "backing": obj{"kind": "project"}}))
	e.fail(fenceapi.InvalidArgument, a, "POST", path, children(obj{"name": "x", "backing": obj{"kind": "zone"}}))
	e.fail(fenceapi.InvalidArgument, a, "POST", path, obj{"name": "x"})
	e.fail(fenceapi.InvalidArgument, a, "POST", path, children(obj{"name": "x", "quota": obj{"us-central1": obj{"warp_drives": 1}}}))
	er = e.fail(fenceapi.InvalidArgument, a, "POST", path, children(obj{"name": "x", "quota": obj{"global": obj{"ci-pool@q4ci7a/runner_minutes": 1}}}))
	if !strings.Contains(str(er["message"]), "reserved") {
		t.Errorf("reserved class: %v", er)
	}
	e.fail(fenceapi.InvalidArgument, a, "POST", path, obj{"name": "x", "address": obj{"range": "10.0.0.1/24"}})
	e.fail(fenceapi.ResourceExhausted, a, "POST", path, obj{"name": "x", "address": obj{"prefix_length": 8}})
	// Nothing was left behind by the refusals.
	if got := len(e.ok(a, "GET", "/silo/v1/self/children", nil)["silos"].([]any)); got != 2 {
		t.Errorf("team-a has %d children, want 2", got)
	}

	// The depth cap.
	parent := "-" + e.root
	for i := 1; i <= tree.MaxDepth; i++ {
		parent = "-" + e.mk(e.root, parent, fmt.Sprintf("d%d", i), obj{"address": obj{"prefix_length": 11 + i}})
	}
	e.fail(fenceapi.ResourceExhausted, e.root, "POST", "/silo/v1/silos/"+parent+"/children", obj{"name": "x", "address": obj{"prefix_length": 28}})
}

func TestWhoMay(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 8}}, "egress": obj{"allow": []string{"*.debian.org", "pypi.org"}}})
	b := e.mk(e.root, "-"+e.root, "team-b", nil)
	ci := e.mk(a, "self", "ci", nil)

	// The silo itself.
	e.ok(a, "GET", "/silo/v1/self", nil)
	e.ok(a, "GET", "/silo/v1/silos/team-a", nil)
	e.ok(a, "GET", "/silo/v1/self/audit", nil)
	e.ok(a, "PATCH", "/silo/v1/self", obj{"egress": obj{"allow": []string{"deb.debian.org"}}})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"egress": obj{"allow": []string{"deb.debian.org", "github.com"}}})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"quota": obj{"us-central1": obj{"vcpu": 9}}})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"control": "read"})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"labels": obj{"x": "y"}})
	e.fail(fenceapi.PermissionDenied, a, "POST", "/silo/v1/self:grow", obj{"to_prefix_length": 15})
	e.fail(fenceapi.PermissionDenied, a, "POST", "/silo/v1/self:addBlock", obj{"prefix_length": 24})
	e.fail(fenceapi.PermissionDenied, a, "DELETE", "/silo/v1/self", nil)
	e.fail(fenceapi.PermissionDenied, a, "PUT", "/silo/v1/self/members/user:a@example.com", nil)
	e.fail(fenceapi.PermissionDenied, a, "POST", "/silo/v1/quota:preview", obj{"allocation": obj{"silo": "/team-a", "quota": obj{}}})

	// A proper ancestor.
	e.ok(a, "GET", "/silo/v1/silos/team-a/ci", nil)
	e.ok(a, "PATCH", "/silo/v1/silos/team-a/ci", obj{"quota": obj{"us-central1": obj{"vcpu": 2}}, "control": "read", "labels": obj{"x": "y"}})
	e.ok(e.root, "GET", "/silo/v1/silos/-"+ci+"?view=subtree", nil)
	sub := e.ok(e.root, "GET", "/silo/v1/self?view=subtree", nil)
	if got := len(get(sub, "children").([]any)); got != 2 {
		t.Errorf("subtree has %d children", got)
	}

	// Anyone else: NOT_FOUND, never PERMISSION_DENIED, for every call.
	for _, c := range []struct{ caller, method, path string }{
		{b, "GET", "/silo/v1/silos/team-a"},
		{b, "GET", "/silo/v1/silos/team-a/ci"},
		{b, "GET", "/silo/v1/silos/-" + a},
		{b, "GET", "/silo/v1/silos/team-a/children"},
		{b, "PATCH", "/silo/v1/silos/team-a"},
		{b, "DELETE", "/silo/v1/silos/team-a"},
		{b, "POST", "/silo/v1/silos/team-a:grow"},
		{b, "POST", "/silo/v1/silos/team-a/children"},
		{b, "GET", "/silo/v1/silos/team-a/audit"},
		{b, "PUT", "/silo/v1/silos/team-a/members/user:a@example.com"},
		{ci, "GET", "/silo/v1/silos/team-a"},
		{a, "GET", "/silo/v1/silos/-" + e.root},
		{a, "GET", "/silo/v1/silos/no-such"},
	} {
		e.fail(fenceapi.NotFound, c.caller, c.method, c.path, obj{})
	}
}

func TestLooserThanParent(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "-"+e.root, "team-a", obj{
		"egress":       obj{"allow": []string{"*.debian.org"}},
		"grant_policy": obj{"exports": "allow", "links": "deny"},
		"agent_policy": obj{"credentials": []string{"anthropic-api"}, "models": []string{"claude-*"}, "max_lease": "24h"},
		"control":      "read",
	})
	d := e.ok(e.root, "GET", "/silo/v1/silos/team-a", nil)
	if got := str(get(d, "grant_policy", "effective")); got != "map[allocators:deny exports:allow links:deny]" {
		t.Errorf("grant_policy effective = %s", got)
	}
	path := "/silo/v1/silos/team-a/children"
	for name, body := range map[string]obj{
		"egress":      {"egress": obj{"allow": []string{"github.com"}}},
		"egress-wide": {"egress": obj{"allow": []string{"*"}}},
		"grant":       {"grant_policy": obj{"links": "allow"}},
		"credentials": {"agent_policy": obj{"credentials": []string{"openai-api"}}},
		"models":      {"agent_policy": obj{"models": []string{"gpt-*"}}},
		"max_lease":   {"agent_policy": obj{"max_lease": "48h"}},
		"control":     {"control": "full"},
	} {
		body["name"], body["address"] = "x", obj{"prefix_length": 24}
		er := e.fail(fenceapi.LooserThanParent, e.root, "POST", path, body)
		t.Logf("%s: %v", name, er["message"])
	}
	// What is no wider is accepted, and defaults come from the
	// parent's effective policy.
	ci := e.mk(e.root, "team-a", "ci", obj{
		"egress":       obj{"allow": []string{"deb.debian.org"}},
		"agent_policy": obj{"models": []string{"claude-opus*"}, "max_lease": "8h"},
		"control":      "none",
	})
	d = e.ok(e.root, "GET", "/silo/v1/silos/-"+ci, nil)
	for path, want := range map[string]string{
		"grant_policy.own.exports":           "allow",
		"grant_policy.own.links":             "deny",
		"control.effective":                  "none",
		"control.effective_from":             "/team-a/ci",
		"agent_policy.effective.models":      "[claude-opus*]",
		"agent_policy.effective.credentials": "[anthropic-api]",
		"agent_policy.effective.max_lease":   "8h",
		"egress.effective.allow":             "[deb.debian.org]",
	} {
		if got := str(get(d, strings.Split(path, ".")...)); got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	// PATCH by an ancestor is held to the parent's policy too.
	p := "/silo/v1/silos/team-a/ci"
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", p, obj{"egress": obj{"allow": []string{"github.com"}}})
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", p, obj{"grant_policy": obj{"links": "allow"}})
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", p, obj{"agent_policy": obj{"max_lease": "7d"}})
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", p, obj{"control": "full"})
	e.ok(e.root, "PATCH", p, obj{"control": "read"})

	// Tightening the parent tightens the child at once, and loosening
	// it restores the child's own setting.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"control": "none", "grant_policy": obj{"exports": "deny"}})
	d = e.ok(e.root, "GET", "/silo/v1/silos/-"+ci, nil)
	if str(get(d, "control", "effective_from")) != "/team-a" || str(get(d, "control", "own")) != "read" ||
		str(get(d, "grant_policy", "effective", "exports")) != "deny" {
		t.Errorf("after tightening team-a: control %v, grant %v", d["control"], d["grant_policy"])
	}
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"control": "full"})
	d = e.ok(e.root, "GET", "/silo/v1/silos/-"+ci, nil)
	if str(get(d, "control", "effective")) != "read" {
		t.Errorf("after loosening team-a: control %v", d["control"])
	}

	// The silo narrows its own grant and agent policy, never widens.
	e.ok(a, "PATCH", "/silo/v1/self", obj{"grant_policy": obj{"allocators": nil}, "agent_policy": obj{"max_lease": "12h"}})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"grant_policy": obj{"allocators": "allow"}})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"agent_policy": obj{"max_lease": "13h"}})
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"agent_policy": obj{"credentials": nil}})
	e.fail(fenceapi.InvalidArgument, e.root, "PATCH", "/silo/v1/silos/team-a", obj{"grant_policy": obj{"links": "maybe"}})
	e.fail(fenceapi.InvalidArgument, e.root, "PATCH", "/silo/v1/silos/team-a", obj{"egress": obj{"allow": []string{"not a host"}}})
	e.fail(fenceapi.PermissionDenied, e.root, "PATCH", "/silo/v1/self", obj{"control": "read"})
	e.fail(fenceapi.PermissionDenied, e.root, "PATCH", "/silo/v1/self", obj{"quota": obj{}})
}

// Egress entries may name a port, which is part of what they allow: a
// port-less entry is 443 on CONNECT and 80 on plain HTTP, so it doesn't
// cover an entry with a port (gateway.md, "Policy", SV-86).
func TestEgressPorts(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	// The root's "*" is any host on the default ports, not any port.
	e.fail(fenceapi.LooserThanParent, e.root, "POST", "/silo/v1/silos/-"+e.root+"/children",
		obj{"name": "x", "address": obj{"prefix_length": 24}, "egress": obj{"allow": []string{"github.com:22"}}})
	a := e.mk(e.root, "-"+e.root, "team-a", obj{"egress": obj{"allow": []string{"*.debian.org", "deb.debian.org:873"}, "deny": []string{"bad.debian.org:443"}}})
	d := e.ok(a, "GET", "/silo/v1/self", nil)
	if got := str(get(d, "egress", "own", "allow")); got != "[*.debian.org deb.debian.org:873]" {
		t.Errorf("own allow %s", got)
	}
	e.fail(fenceapi.PermissionDenied, a, "PATCH", "/silo/v1/self", obj{"egress": obj{"allow": []string{"*.debian.org", "deb.debian.org:8080"}}})
	e.ok(a, "PATCH", "/silo/v1/self", obj{"egress": obj{"allow": []string{"deb.debian.org", "deb.debian.org:873"}, "deny": []string{"bad.debian.org:443"}}})
	for _, bad := range []string{"github.com:0", "github.com:", "github.com:http", "10.0.0.0/8"} {
		e.fail(fenceapi.InvalidArgument, e.root, "PATCH", "/silo/v1/silos/team-a", obj{"egress": obj{"allow": []string{bad}}})
	}
}

// The root's egress is install's --egress-allow, and allows nothing
// when bootstrap is given none.
func TestRootEgressDefaultsToNothing(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background(), db.Latest()); err != nil {
		t.Fatal(err)
	}
	api := fenceapi.New(db, fenceapi.Options{Caller: func(r *http.Request) (fenceapi.Caller, error) {
		return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
	}})
	t.Cleanup(func() { api.Close() })
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("POST", "/silod/bootstrap", strings.NewReader(`{"regions":["us-central1"]}`)))
	var root obj
	if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil || w.Code != 200 {
		t.Fatalf("bootstrap: %d %s", w.Code, w.Body)
	}
	if got := str(get(root, "egress", "own", "allow")); got != "[]" {
		t.Errorf("the root's default egress allow is %s, want []", got)
	}
}

// A silo may not take a name a fence API path reads as a collection,
// or one reserved for custom allocators: it could not be addressed by
// path (/silo/v1/silos/team-a/exports is team-a's exports).
func TestReservedSiloNames(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	e.mk(e.root, "-"+e.root, "team-a", obj{"address": obj{"range": "10.0.0.0/16"}})
	var names []string
	for name := range control.Collections {
		names = append(names, name)
	}
	for name := range control.AllocatorCollections {
		names = append(names, name)
	}
	n := 0
	for _, name := range names {
		if !tree.ValidName(name) {
			continue
		}
		n++
		er := e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/silos/team-a/children", obj{"name": name, "address": obj{"prefix_length": 24}})
		if msg := str(er["message"]); !strings.Contains(msg, "reserved") || !strings.Contains(msg, name) {
			t.Errorf("message %q, want it to name %s as reserved", msg, name)
		}
	}
	if n < 12 {
		t.Errorf("%d reserved names checked, want the 8 DNS-label collections and the 4 allocator names", n)
	}

	// Nor may the root.
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background(), db.Latest()); err != nil {
		t.Fatal(err)
	}
	api := fenceapi.New(db, fenceapi.Options{})
	t.Cleanup(func() { api.Close() })
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("POST", "/silod/bootstrap", strings.NewReader(`{"name":"children","regions":["us-central1"]}`)))
	if w.Code != fenceapi.InvalidArgument.Status() || !strings.Contains(w.Body.String(), "reserved") {
		t.Errorf("bootstrap a root named children: %d %s, want INVALID_ARGUMENT", w.Code, w.Body)
	}
}

func TestValidateOnlyAndReplay(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	body := obj{"name": "team-a", "address": obj{"range": "10.0.0.0/16"}, "quota": obj{"us-central1": obj{"vcpu": 10}}}
	r := e.ok(e.root, "POST", "/silo/v1/self/children?validate_only=true", body)
	if r["path"] != "/team-a" {
		t.Errorf("validate_only create returned %v", r)
	}
	e.fail(fenceapi.NotFound, e.root, "GET", "/silo/v1/silos/team-a", nil)
	e.fail(fenceapi.QuotaExceeded, e.root, "POST", "/silo/v1/self/children?validate_only=true",
		obj{"name": "x", "address": obj{"prefix_length": 16}, "quota": obj{"us-central1": obj{"vcpu": 1000}}})

	body["request_id"] = "6f1c1c1e-0000-4000-8000-000000000001"
	first := e.ok(e.root, "POST", "/silo/v1/self/children", body)
	code, again, h := e.doH(e.root, "POST", "/silo/v1/self/children", body)
	if code != 200 || again["id"] != first["id"] || h.Get("Silo-Replayed") != "true" {
		t.Errorf("replay: %d %v %v, want the first result %v", code, again["id"], h, first["id"])
	}
	// Another caller's request ids are its own.
	b := e.mk(e.root, "self", "team-b", nil)
	e.fail(fenceapi.NotFound, b, "POST", "/silo/v1/silos/team-a/children", obj{"name": "x", "request_id": body["request_id"]})

	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a?validate_only=true", obj{"quota": obj{"us-central1": obj{"vcpu": 20}}})
	e.ok(e.root, "POST", "/silo/v1/silos/team-a:addBlock?validate_only=true", obj{"prefix_length": 24})
	e.ok(e.root, "DELETE", "/silo/v1/silos/team-a?validate_only=true", nil)
	e.ok(e.root, "PUT", "/silo/v1/silos/team-a/members/user:x@example.com?validate_only=true", nil)
	d := e.ok(e.root, "GET", "/silo/v1/silos/team-a", nil)
	if str(get(d, "quota", "us-central1", "vcpu", "granted")) != "10" || str(get(d, "address", "blocks")) != "[10.0.0.0/16]" || d["state"] != "ACTIVE" {
		t.Errorf("validate_only changed something: %v %v %v", d["quota"], d["address"], d["state"])
	}
	e.fail(fenceapi.NotFound, e.root, "GET", "/silo/v1/silos/team-a/members/user:x@example.com", nil)
	// PATCH replays too.
	p := obj{"quota": obj{"us-central1": obj{"vcpu": 11}}, "request_id": "r-2"}
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", p)
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 12}}})
	if r := e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", p); str(get(r, "quota", "us-central1", "vcpu", "granted")) != "11" {
		t.Errorf("replayed PATCH returned %v, want the first result", r["quota"])
	}
}

func TestPatchAllocations(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "self", "team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 10}, "us-east1": obj{"vcpu": 2}}, "model_budget_usd": "10"})
	e.mk(a, "self", "ci", obj{"quota": obj{"us-central1": obj{"vcpu": 6}, "us-east1": obj{"vcpu": 1}}})
	p := "/silo/v1/silos/team-a"
	e.fail(fenceapi.FailedPrecondition, e.root, "PATCH", p, obj{"quota": obj{"us-central1": obj{"vcpu": 5}}})
	e.fail(fenceapi.QuotaExceeded, e.root, "PATCH", p, obj{"quota": obj{"us-central1": obj{"vcpu": 101}}})
	e.fail(fenceapi.FailedPrecondition, e.root, "PATCH", p, obj{"regions": []string{"us-central1"}})
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", p, obj{"regions": []string{"us-central1", "europe-west1"}})
	e.fail(fenceapi.InvalidArgument, e.root, "PATCH", p, obj{"max_blocks": 99})
	r := e.ok(e.root, "PATCH", p, obj{"quota": obj{"us-central1": obj{"vcpu": 20}}, "model_budget_usd": "15.5", "max_blocks": 8})
	if str(get(r, "quota", "us-central1", "vcpu", "granted")) != "20" || str(get(r, "model_budget", "granted_usd")) != "15.50" ||
		str(get(r, "address", "max_blocks")) != "8" || str(get(r, "quota", "us-east1", "vcpu", "granted")) != "2" {
		t.Errorf("after PATCH: %v %v %v", r["quota"], r["model_budget"], r["address"])
	}
	// Lowering the child's east quota to 0 lets the region go.
	e.ok(a, "PATCH", "/silo/v1/silos/team-a/ci", obj{"quota": obj{"us-east1": obj{"vcpu": 0}}})
	r = e.ok(e.root, "PATCH", p, obj{"regions": []string{"us-central1"}, "quota": obj{"us-east1": obj{"vcpu": 0}}})
	if str(r["regions"]) != "[us-central1]" {
		t.Errorf("regions = %v", r["regions"])
	}
	ci := e.ok(a, "GET", "/silo/v1/silos/team-a/ci", nil)
	if str(ci["regions"]) != "[us-central1]" {
		t.Errorf("ci regions = %v, want narrowed with its parent", ci["regions"])
	}
}

func TestBlocks(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "self", "team-a", obj{"address": obj{"range": "10.1.16.0/20"}})
	p := "/silo/v1/silos/team-a"
	r := e.ok(e.root, "POST", p+":grow", obj{"block": "10.1.16.0/20", "to_prefix_length": 19})
	if str(get(r, "address", "blocks")) != "[10.1.0.0/19]" {
		t.Errorf("after grow: %v", get(r, "address", "blocks"))
	}
	e.fail(fenceapi.InvalidArgument, e.root, "POST", p+":grow", obj{"block": "10.1.0.0/19", "to_prefix_length": 20})
	e.fail(fenceapi.NotFound, e.root, "POST", p+":grow", obj{"block": "10.9.0.0/19", "to_prefix_length": 18})
	r = e.ok(e.root, "POST", p+":addBlock", obj{"range": "10.2.0.0/22"})
	if str(get(r, "address", "blocks")) != "[10.1.0.0/19 10.2.0.0/22]" {
		t.Errorf("after addBlock: %v", get(r, "address", "blocks"))
	}
	e.mk(a, "self", "ci", obj{"address": obj{"range": "10.1.0.0/24"}})
	e.fail(fenceapi.FailedPrecondition, a, "POST", "/silo/v1/self:releaseBlock", obj{"block": "10.1.0.0/19"})
	r = e.ok(a, "POST", "/silo/v1/self:releaseBlock", obj{"block": "10.2.0.0/22"})
	if str(get(r, "address", "blocks")) != "[10.1.0.0/19]" {
		t.Errorf("after releaseBlock: %v", get(r, "address", "blocks"))
	}
	e.fail(fenceapi.InvalidArgument, a, "POST", "/silo/v1/self:releaseBlock", obj{"block": "nonsense"})
	e.fail(fenceapi.NotFound, a, "POST", "/silo/v1/self:frobnicate", obj{})
}

// failing is a SiloDeleter that refuses one silo until told not to.
type failing struct{ refuse atomic.Value }

func (f *failing) DeleteContents(_ context.Context, s *tree.Silo) error {
	if p, _ := f.refuse.Load().(string); p == s.Path {
		return errors.New("an instance won't go")
	}
	return nil
}

func (e *env) waitOp(caller, name string) obj {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		op := e.ok(caller, "GET", "/silo/v1/"+name, nil)
		if op["done"] == true || op["state"] == fenceapi.OpDeleteFailed || time.Now().After(deadline) {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDelete(t *testing.T) {
	f := &failing{}
	e := newEnv(t, fenceapi.Options{Deleter: f})
	a := e.mk(e.root, "self", "team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 10}}, "model_budget_usd": "20"})
	e.mk(a, "self", "ci", obj{"quota": obj{"us-central1": obj{"vcpu": 4}}, "address": obj{"prefix_length": 20}})
	e.mk(a, "team-a/ci", "job-1", obj{"quota": obj{"us-central1": obj{"vcpu": 1}}, "address": obj{"prefix_length": 24}})
	e.mk(a, "self", "web", obj{"address": obj{"prefix_length": 20}})

	e.fail(fenceapi.FailedPrecondition, e.root, "DELETE", "/silo/v1/self", nil)
	e.fail(fenceapi.FailedPrecondition, e.root, "DELETE", "/silo/v1/silos/team-a", nil)

	// Without force, an empty silo goes.
	op := e.ok(a, "DELETE", "/silo/v1/silos/team-a/web", nil)
	if op = e.waitOp(a, op["name"].(string)); op["state"] != fenceapi.OpDone {
		t.Fatalf("delete web: %v", op)
	}
	e.fail(fenceapi.NotFound, a, "GET", "/silo/v1/silos/team-a/web", nil)

	// With force, bottom-up; a failure stops it at DELETE_FAILED, and
	// a repeat call resumes.
	f.refuse.Store("/team-a")
	op = e.ok(e.root, "DELETE", "/silo/v1/silos/team-a?force=true", nil)
	op = e.waitOp(e.root, op["name"].(string))
	if op["state"] != fenceapi.OpDeleteFailed || !strings.Contains(str(op["error"]), "won't go") {
		t.Fatalf("op = %v, want DELETE_FAILED", op)
	}
	d := e.ok(e.root, "GET", "/silo/v1/silos/team-a", nil)
	if d["state"] != "DELETE_FAILED" {
		t.Errorf("team-a state %v", d["state"])
	}
	e.fail(fenceapi.NotFound, e.root, "GET", "/silo/v1/silos/team-a/ci", nil)
	f.refuse.Store("")
	again := e.ok(e.root, "DELETE", "/silo/v1/silos/team-a?force=true", nil)
	if again["id"] != op["id"] {
		t.Errorf("resume made a new operation %v, want %v", again["id"], op["id"])
	}
	if op = e.waitOp(e.root, again["name"].(string)); op["state"] != fenceapi.OpDone || op["done"] != true {
		t.Fatalf("resumed op = %v", op)
	}
	e.fail(fenceapi.NotFound, e.root, "GET", "/silo/v1/silos/team-a", nil)

	// Everything came back to the root.
	r := e.ok(e.root, "GET", "/silo/v1/self", nil)
	if str(get(r, "quota", "us-central1", "vcpu", "delegated")) != "0" || str(get(r, "model_budget", "delegated_usd")) != "0.00" ||
		str(get(r, "address", "delegated")) != "[map[child:system:control range:10.255.254.0/24] map[child:system:gateway range:10.255.255.0/24]]" {
		t.Errorf("root after delete: %v %v %v", r["quota"], r["model_budget"], get(r, "address", "delegated"))
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		bad, err := ledger.Check(ctx, tx)
		for _, b := range bad {
			t.Error(b)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Bottom-up, in the audit.
	recs := e.ok(e.root, "GET", "/silo/v1/self/audit?subtree=true&kind=silo.delete", nil)["records"].([]any)
	var order []string
	for _, rec := range recs {
		if rec.(obj)["result"] != "OK" {
			continue // the refused delete without force
		}
		order = append(order, str(get(rec.(obj), "silo", "path")))
	}
	if strings.Join(order, " ") != "/team-a/web /team-a/ci/job-1 /team-a/ci /team-a" {
		t.Errorf("deleted in order %v", order)
	}
	last := recs[len(recs)-1].(obj)
	if !strings.Contains(str(get(last, "fields", "charges")), "amount:-10") || get(last, "via") != "ancestor:"+e.root {
		t.Errorf("delete record %v", last)
	}
	// Someone else can't read the operation.
	b := e.mk(e.root, "self", "team-b", nil)
	e.fail(fenceapi.NotFound, b, "GET", "/silo/v1/"+op["name"].(string), nil)
}

func TestMembers(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "self", "team-a", nil)
	ci := e.mk(a, "self", "ci", nil)
	web := e.mk(a, "self", "web", nil)
	b := e.mk(e.root, "self", "team-b", nil)
	id := "user:alice@example.com"

	e.fail(fenceapi.InvalidArgument, a, "PUT", "/silo/v1/silos/team-a/ci/members/alice", nil)
	m := e.ok(a, "PUT", "/silo/v1/silos/team-a/ci/members/"+id, nil)
	if m["silo"] != ci || m["path"] != "/team-a/ci" {
		t.Errorf("member %v", m)
	}
	e.ok(ci, "GET", "/silo/v1/self/members/"+id, nil)
	e.fail(fenceapi.NotFound, web, "GET", "/silo/v1/self/members/"+id, nil)
	// Moving needs an ancestor of both: team-a is, team-b isn't.
	e.mk(b, "self", "x", nil)
	e.fail(fenceapi.PermissionDenied, b, "PUT", "/silo/v1/silos/team-b/x/members/"+id, nil)
	e.ok(a, "PUT", "/silo/v1/silos/team-a/web/members/"+id, nil)
	e.fail(fenceapi.NotFound, ci, "GET", "/silo/v1/self/members/"+id, nil)
	if l := e.ok(a, "GET", "/silo/v1/silos/team-a/web/members", nil)["members"].([]any); len(l) != 1 {
		t.Errorf("web members %v", l)
	}
	e.fail(fenceapi.PermissionDenied, web, "DELETE", "/silo/v1/self/members/"+id, nil)
	e.ok(a, "DELETE", "/silo/v1/silos/team-a/web/members/"+id, nil)
	e.fail(fenceapi.NotFound, a, "DELETE", "/silo/v1/silos/team-a/web/members/"+id, nil)

	// Root members: install's admin is there, and root members set
	// more.
	e.ok(e.root, "GET", "/silo/v1/self/members/user:admin@example.com", nil)
	e.ok(e.root, "PUT", "/silo/v1/self/members/serviceAccount:ci@p.iam.gserviceaccount.com", nil)

	recs := e.ok(e.root, "GET", "/silo/v1/self/audit?subtree=true&kind=member.*", nil)["records"].([]any)
	if len(recs) < 5 {
		t.Errorf("member records %v", recs)
	}
}

func TestAudit(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "self", "team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 4}}, "request_id": "req-1"})
	ci := e.mk(a, "self", "ci", nil)
	e.fail(fenceapi.QuotaExceeded, e.root, "PATCH", "/silo/v1/silos/team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 1000}}})
	e.ok(e.root, "PATCH", "/silo/v1/silos/team-a", obj{"control": "read"})

	recs := e.ok(a, "GET", "/silo/v1/self/audit", nil)["records"].([]any)
	var kinds []string
	for _, r := range recs {
		kinds = append(kinds, str(r.(obj)["kind"])+":"+str(r.(obj)["result"]))
	}
	if strings.Join(kinds, " ") != "silo.create:OK silo.patch:QUOTA_EXCEEDED silo.set_control:OK" {
		t.Errorf("team-a records %v", kinds)
	}
	c := recs[0].(obj)
	for path, want := range map[string]string{
		"silo.id": a, "silo.path": "/team-a", "actor.silo": e.root, "via": "ancestor:" + e.root, "request_id": "req-1",
		"fields.charges": "[map[amount:4 class:vcpu home:/team-a kind:gce payer:/ region:us-central1 via:ancestor:" + e.root + "]]",
	} {
		if got := str(get(c, strings.Split(path, ".")...)); got != want {
			t.Errorf("create record %s = %s, want %s", path, got, want)
		}
	}
	// ci's own create is in team-a's subtree, and ci can read its own.
	if n := len(e.ok(a, "GET", "/silo/v1/self/audit?subtree=true", nil)["records"].([]any)); n != 4 {
		t.Errorf("subtree has %d records, want 4", n)
	}
	if n := len(e.ok(ci, "GET", "/silo/v1/self/audit", nil)["records"].([]any)); n != 1 {
		t.Errorf("ci has %d records", n)
	}
	if n := len(e.ok(a, "GET", "/silo/v1/self/audit?since="+time.Now().Add(time.Hour).UTC().Format(time.RFC3339), nil)["records"].([]any)); n != 0 {
		t.Errorf("since an hour from now: %d records", n)
	}
	e.fail(fenceapi.InvalidArgument, a, "GET", "/silo/v1/self/audit?since=yesterday", nil)
	pg := e.ok(e.root, "GET", "/silo/v1/self/audit?subtree=true&page_size=2", nil)
	if len(pg["records"].([]any)) != 2 || pg["next_page_token"] == nil {
		t.Fatalf("page 1: %v", pg)
	}
	pg2 := e.ok(e.root, "GET", "/silo/v1/self/audit?subtree=true&page_size=2&page_token="+pg["next_page_token"].(string), nil)
	if len(pg2["records"].([]any)) != 2 {
		t.Errorf("page 2: %v", pg2)
	}

	// Pruning goes by database time: a record 91 days old goes.
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		now, _ := tx.Now(ctx)
		_, err := tx.Exec(ctx, `UPDATE audit SET at = ? WHERE kind = 'silo.set_control'`, store.Millis(now.Add(-91*24*time.Hour)))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	n, err := fenceapi.PruneAudit(ctx, e.db)
	if err != nil || n != 1 {
		t.Fatalf("PruneAudit = %d, %v; want 1", n, err)
	}
	if n := len(e.ok(a, "GET", "/silo/v1/self/audit", nil)["records"].([]any)); n != 2 {
		t.Errorf("after pruning team-a has %d records, want 2", n)
	}
}

func TestPreview(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	a := e.mk(e.root, "self", "team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 10}}})
	e.mk(a, "self", "ci", obj{"quota": obj{"us-central1": obj{"vcpu": 2}}})
	r := e.ok(a, "POST", "/silo/v1/quota:preview", obj{"allocation": obj{"silo": "/team-a/ci", "quota": obj{"us-central1": obj{"vcpu": 5}}}})
	if r["fits"] != true || str(r["charges"]) != "[map[amount:3 class:vcpu home:/team-a/ci kind:gce payer:/team-a region:us-central1 via:ancestor:"+a+"]]" {
		t.Errorf("preview %v", r)
	}
	if str(r["levels"]) == "[]" {
		t.Errorf("no levels: %v", r)
	}
	r = e.ok(a, "POST", "/silo/v1/quota:preview", obj{"allocation": obj{"silo": "/team-a/ci", "quota": obj{"us-central1": obj{"vcpu": 50}}}})
	if r["fits"] != false || str(get(r, "refusal", "code")) != "QUOTA_EXCEEDED" {
		t.Errorf("preview that doesn't fit: %v", r)
	}
	// Nothing changed.
	if got := str(get(e.ok(a, "GET", "/silo/v1/silos/team-a/ci", nil), "quota", "us-central1", "vcpu", "granted")); got != "2" {
		t.Errorf("preview changed the grant to %s", got)
	}
	e.fail(fenceapi.Unimplemented, a, "POST", "/silo/v1/quota:preview", obj{"facade_request": obj{"method": "POST", "path": "/compute/v1/x"}})
	e.fail(fenceapi.InvalidArgument, a, "POST", "/silo/v1/quota:preview", obj{})
	e.fail(fenceapi.NotFound, a, "POST", "/silo/v1/quota:preview", obj{"allocation": obj{"silo": "/team-b", "quota": obj{}}})
}

// pricer prices every facade request at one vcpu in the caller's silo.
type pricer struct{}

func (pricer) Price(_ context.Context, _ store.Tx, c fenceapi.Caller, _ fenceapi.FacadeRequest) (string, []ledger.Amount, error) {
	return c.Silo, []ledger.Amount{{Key: ledger.Key{Kind: "gce", Class: "vcpu", Region: "us-central1"}, Amount: ledger.Units(1)}}, nil
}

func TestPreviewFacadeForm(t *testing.T) {
	e := newEnv(t, fenceapi.Options{Pricer: pricer{}})
	a := e.mk(e.root, "self", "team-a", obj{"quota": obj{"us-central1": obj{"vcpu": 10}}})
	r := e.ok(a, "POST", "/silo/v1/quota:preview", obj{"facade_request": obj{"method": "POST", "path": "/compute/v1/x"}})
	if r["fits"] != true || str(r["charges"]) != "[map[amount:1 class:vcpu home:/team-a kind:gce payer:/team-a region:us-central1 via:facade]]" {
		t.Errorf("preview %v", r)
	}
}

func TestReservedAndConventions(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	e.mk(e.root, "self", "team-a", nil)
	for _, p := range []string{
		"/silo/v1/self/allocators", "/silo/v1/self/allocator-offers", "/silo/v1/self/allocator-grants", "/silo/v1/self/resources",
		"/silo/v1/silos/team-a/allocators", "/silo/v1/silos/team-a/allocator-offers/x", "/silo/v1/silos/team-a/allocator-grants",
		"/silo/v1/silos/team-a/resources", "/silo/v1/allocators", "/silo/v1/allocators/x",
	} {
		e.fail(fenceapi.Unimplemented, e.root, "GET", p, nil)
	}
	e.fail(fenceapi.NotFound, e.root, "GET", "/silo/v1/nothing", nil)
	for i := 0; i < 3; i++ {
		e.mk(e.root, "self", fmt.Sprintf("c%d", i), nil)
	}
	var names []string
	tok := ""
	for i := 0; i < 10; i++ {
		r := e.ok(e.root, "GET", "/silo/v1/self/children?page_size=3&page_token="+tok, nil)
		for _, s := range r["silos"].([]any) {
			names = append(names, s.(obj)["name"].(string))
		}
		t, _ := r["next_page_token"].(string)
		if t == "" {
			break
		}
		tok = t
	}
	if strings.Join(names, " ") != "c0 c1 c2 team-a" {
		t.Errorf("paged children %v", names)
	}
	e.fail(fenceapi.InvalidArgument, e.root, "GET", "/silo/v1/self/children?page_token=junk", nil)
	e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/self/children", "not an object")
	if code, _ := e.do("", "GET", "/silod/health", nil); code != 200 {
		t.Errorf("health %d", code)
	}
}

func TestNotReady(t *testing.T) {
	e := newEnv(t, fenceapi.Options{})
	api := fenceapi.New(e.db, fenceapi.Options{Ready: func() string { return "schema_mismatch" }})
	srv := httptest.NewServer(api)
	defer srv.Close()
	defer api.Close()
	for path, want := range map[string]int{"/silod/health": 503, "/silo/v1/self": 503, "/silod/bootstrap": 503} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d %s", path, resp.StatusCode, b)
		}
		if path == "/silod/health" && !strings.Contains(string(b), "schema_mismatch") {
			t.Errorf("health says %s", b)
		}
	}
}

// TestPostgres runs the suite again on Postgres, where PostgreSQL is
// installed (and must be, with SILO_REQUIRE_POSTGRES set).
func TestPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	pgtest.Start(t) // skips the lot without PostgreSQL
	onPostgres = true
	defer func() { onPostgres = false }()
	for name, fn := range map[string]func(*testing.T){
		"BootstrapAndDescribe": TestBootstrapAndDescribe, "CreateAndChecks": TestCreateAndChecks, "WhoMay": TestWhoMay,
		"LooserThanParent": TestLooserThanParent, "ValidateOnlyAndReplay": TestValidateOnlyAndReplay,
		"PatchAllocations": TestPatchAllocations, "Blocks": TestBlocks, "Delete": TestDelete, "Members": TestMembers,
		"Audit": TestAudit, "Preview": TestPreview, "ReservedAndConventions": TestReservedAndConventions,
		"ControlPolicy": TestControlPolicy, "ManageGrants": TestManageGrants, "Exports": TestExports, "Links": TestLinks,
		"AgentLifecycle": TestAgentLifecycle, "AgentOwnSession": TestAgentOwnSession, "AgentSealedAndManage": TestAgentSealedAndManage,
	} {
		t.Run(name, fn)
	}
}
