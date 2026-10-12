package gcp

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/install"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

func computeExtras(t *testing.T, opts []option.ClientOption) (ComputeExtras, *fakeOps) {
	t.Helper()
	svc, err := compute.NewService(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakeOps{}
	return ComputeExtras{s: svc, ops: ops}, ops
}

// running is the operation GCE answers a mutation with, still going.
func running(scope string) map[string]any {
	op := map[string]any{"name": "op-1", "status": "RUNNING"}
	kind, name, _ := strings.Cut(scope, "/")
	op[strings.TrimSuffix(kind, "s")] = "https://www.googleapis.com/compute/v1/projects/p/" + kind + "/" + name
	return op
}

// TestComputeExtrasMutations checks each mutation's request, and that
// it waits for the operation it started.
func TestComputeExtrasMutations(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		do           func(ComputeExtras) error
		method, path string
		body         string
	}{
		// ForceSendFields: turning access off still says so.
		{"SetPrivateIpGoogleAccess off", func(c ComputeExtras) error {
			return c.SetPrivateIpGoogleAccess(ctx, "p", "r", "s", false)
		}, "POST", "/projects/p/regions/r/subnetworks/s/setPrivateIpGoogleAccess", `{"privateIpGoogleAccess":false}`},
		{"SetPrivateIpGoogleAccess on", func(c ComputeExtras) error {
			return c.SetPrivateIpGoogleAccess(ctx, "p", "r", "s", true)
		}, "POST", "/projects/p/regions/r/subnetworks/s/setPrivateIpGoogleAccess", `{"privateIpGoogleAccess":true}`},
		{"InsertRouter", func(c ComputeExtras) error {
			return c.InsertRouter(ctx, "p", "r", &compute.Router{Name: "rt", Network: "n"})
		}, "POST", "/projects/p/regions/r/routers", `{"name":"rt","network":"n"}`},
		// PatchRouter sends only the NATs, whatever else the router has.
		{"PatchRouter", func(c ComputeExtras) error {
			return c.PatchRouter(ctx, "p", "r", &compute.Router{Name: "rt", Network: "n", Description: "d",
				Nats: []*compute.RouterNat{{Name: "nat"}}})
		}, "PATCH", "/projects/p/regions/r/routers/rt", `{"nats":[{"name":"nat"}]}`},
		{"PatchRouter no NATs", func(c ComputeExtras) error {
			return c.PatchRouter(ctx, "p", "r", &compute.Router{Name: "rt", Network: "n"})
		}, "PATCH", "/projects/p/regions/r/routers/rt", `{"nats":[]}`},
		{"DeleteRouter", func(c ComputeExtras) error {
			return c.DeleteRouter(ctx, "p", "r", "rt")
		}, "DELETE", "/projects/p/regions/r/routers/rt", ""},
		{"InsertAddress", func(c ComputeExtras) error {
			return c.InsertAddress(ctx, "p", "r", &compute.Address{Name: "a", AddressType: "INTERNAL"})
		}, "POST", "/projects/p/regions/r/addresses", `{"name":"a","addressType":"INTERNAL"}`},
		{"DeleteAddress", func(c ComputeExtras) error {
			return c.DeleteAddress(ctx, "p", "r", "a")
		}, "DELETE", "/projects/p/regions/r/addresses/a", ""},
		{"InsertHealthCheck", func(c ComputeExtras) error {
			return c.InsertHealthCheck(ctx, "p", "r", &compute.HealthCheck{Name: "hc", Type: "TCP"})
		}, "POST", "/projects/p/regions/r/healthChecks", `{"name":"hc","type":"TCP"}`},
		{"DeleteHealthCheck", func(c ComputeExtras) error {
			return c.DeleteHealthCheck(ctx, "p", "r", "hc")
		}, "DELETE", "/projects/p/regions/r/healthChecks/hc", ""},
		{"InsertInstanceGroupManager", func(c ComputeExtras) error {
			return c.InsertInstanceGroupManager(ctx, "p", "r", &compute.InstanceGroupManager{Name: "m", TargetSize: 2})
		}, "POST", "/projects/p/regions/r/instanceGroupManagers", `{"name":"m","targetSize":2}`},
		// A target size of 0 is still sent; the name and the rest aren't.
		{"PatchInstanceGroupManager", func(c ComputeExtras) error {
			return c.PatchInstanceGroupManager(ctx, "p", "r", &compute.InstanceGroupManager{Name: "m", BaseInstanceName: "b",
				InstanceTemplate: "t", UpdatePolicy: &compute.InstanceGroupManagerUpdatePolicy{Type: "PROACTIVE"},
				AutoHealingPolicies: []*compute.InstanceGroupManagerAutoHealingPolicy{{HealthCheck: "hc", InitialDelaySec: 60}}})
		}, "PATCH", "/projects/p/regions/r/instanceGroupManagers/m",
			`{"instanceTemplate":"t","targetSize":0,"updatePolicy":{"type":"PROACTIVE"},"autoHealingPolicies":[{"healthCheck":"hc","initialDelaySec":60}]}`},
		{"DeleteInstanceGroupManager", func(c ComputeExtras) error {
			return c.DeleteInstanceGroupManager(ctx, "p", "r", "m")
		}, "DELETE", "/projects/p/regions/r/instanceGroupManagers/m", ""},
		{"InsertBackendService", func(c ComputeExtras) error {
			return c.InsertBackendService(ctx, "p", "r", &compute.BackendService{Name: "b", Protocol: "TCP"})
		}, "POST", "/projects/p/regions/r/backendServices", `{"name":"b","protocol":"TCP"}`},
		{"PatchBackendService", func(c ComputeExtras) error {
			return c.PatchBackendService(ctx, "p", "r", &compute.BackendService{Name: "b", Protocol: "TCP",
				Backends: []*compute.Backend{{Group: "g"}}})
		}, "PATCH", "/projects/p/regions/r/backendServices/b", `{"backends":[{"group":"g"}]}`},
		{"PatchBackendService no backends", func(c ComputeExtras) error {
			return c.PatchBackendService(ctx, "p", "r", &compute.BackendService{Name: "b", Protocol: "TCP"})
		}, "PATCH", "/projects/p/regions/r/backendServices/b", `{"backends":[]}`},
		{"DeleteBackendService", func(c ComputeExtras) error {
			return c.DeleteBackendService(ctx, "p", "r", "b")
		}, "DELETE", "/projects/p/regions/r/backendServices/b", ""},
		{"InsertInstanceGroup", func(c ComputeExtras) error {
			return c.InsertInstanceGroup(ctx, "p", "z", &compute.InstanceGroup{Name: "g", Network: "n"})
		}, "POST", "/projects/p/zones/z/instanceGroups", `{"name":"g","network":"n"}`},
		{"AddGroupInstances", func(c ComputeExtras) error {
			return c.AddGroupInstances(ctx, "p", "z", "g", []string{"a", "b"})
		}, "POST", "/projects/p/zones/z/instanceGroups/g/addInstances",
			`{"instances":[{"instance":"projects/p/zones/z/instances/a"},{"instance":"projects/p/zones/z/instances/b"}]}`},
		{"DeleteInstanceGroup", func(c ComputeExtras) error {
			return c.DeleteInstanceGroup(ctx, "p", "z", "g")
		}, "DELETE", "/projects/p/zones/z/instanceGroups/g", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := "regions/r"
			if strings.HasPrefix(tc.path, "/projects/p/zones/") {
				scope = "zones/z"
			}
			a, opts := newAPI(t, map[string]any{tc.method + " " + tc.path: running(scope)})
			c, ops := computeExtras(t, opts)
			if err := tc.do(c); err != nil {
				t.Fatal(err)
			}
			req := a.only(t)
			if req.Method != tc.method || req.Path != tc.path {
				t.Errorf("request %s %s, want %s %s", req.Method, req.Path, tc.method, tc.path)
			}
			jsonEq(t, tc.name, req.Body, tc.body)
			if want := []string{"p " + scope + " op-1"}; !reflect.DeepEqual(ops.waited, want) {
				t.Errorf("waited on %v, want %v", ops.waited, want)
			}
		})
	}
}

// TestComputeExtrasGets checks each get's request, its reply, and that
// a missing resource is install.IsNotFound.
func TestComputeExtrasGets(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		do   func(c ComputeExtras, name string) (string, error)
		path string
	}{
		{"GetRouter", func(c ComputeExtras, name string) (string, error) {
			r, err := c.GetRouter(ctx, "p", "r", name)
			if err != nil {
				return "", err
			}
			return r.Name, nil
		}, "/projects/p/regions/r/routers/"},
		{"GetAddress", func(c ComputeExtras, name string) (string, error) {
			r, err := c.GetAddress(ctx, "p", "r", name)
			if err != nil {
				return "", err
			}
			return r.Name, nil
		}, "/projects/p/regions/r/addresses/"},
		{"GetHealthCheck", func(c ComputeExtras, name string) (string, error) {
			r, err := c.GetHealthCheck(ctx, "p", "r", name)
			if err != nil {
				return "", err
			}
			return r.Name, nil
		}, "/projects/p/regions/r/healthChecks/"},
		{"GetInstanceGroupManager", func(c ComputeExtras, name string) (string, error) {
			r, err := c.GetInstanceGroupManager(ctx, "p", "r", name)
			if err != nil {
				return "", err
			}
			return r.Name, nil
		}, "/projects/p/regions/r/instanceGroupManagers/"},
		{"GetBackendService", func(c ComputeExtras, name string) (string, error) {
			r, err := c.GetBackendService(ctx, "p", "r", name)
			if err != nil {
				return "", err
			}
			return r.Name, nil
		}, "/projects/p/regions/r/backendServices/"},
		{"GetInstanceGroup", func(c ComputeExtras, name string) (string, error) {
			r, err := c.GetInstanceGroup(ctx, "p", "z", name)
			if err != nil {
				return "", err
			}
			return r.Name, nil
		}, "/projects/p/zones/z/instanceGroups/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, opts := newAPI(t, map[string]any{"GET " + tc.path + "x": map[string]any{"name": "x"}})
			c, _ := computeExtras(t, opts)
			if got, err := tc.do(c, "x"); got != "x" || err != nil {
				t.Errorf("got %q, %v", got, err)
			}
			if _, err := tc.do(c, "gone"); !install.IsNotFound(err) {
				t.Errorf("missing: %v, want not found", err)
			}
			if got, want := a.calls(), []string{"GET " + tc.path + "x", "GET " + tc.path + "gone"}; !reflect.DeepEqual(got, want) {
				t.Errorf("calls %v, want %v", got, want)
			}
		})
	}
}

func TestComputeExtrasWait(t *testing.T) {
	ctx := context.Background()
	failed := &compute.Operation{Name: "op-1", Status: "DONE", OperationType: "insert",
		TargetLink: "https://www.googleapis.com/compute/v1/projects/p/regions/r/routers/rt",
		Error: &compute.OperationError{Errors: []*compute.OperationErrorErrors{
			{Code: "QUOTA_EXCEEDED", Message: "Quota 'ROUTERS' exceeded."},
			{Code: "OTHER", Message: "not reported"}}}}
	const want = "insert rt: QUOTA_EXCEEDED: Quota 'ROUTERS' exceeded."

	t.Run("fails after waiting", func(t *testing.T) {
		_, opts := newAPI(t, map[string]any{"POST /projects/p/regions/r/routers": running("regions/r")})
		c, ops := computeExtras(t, opts)
		ops.done = failed
		if err := c.InsertRouter(ctx, "p", "r", &compute.Router{Name: "rt"}); err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
	})
	t.Run("already failed", func(t *testing.T) {
		_, opts := newAPI(t, map[string]any{"POST /projects/p/regions/r/routers": failed})
		c, ops := computeExtras(t, opts)
		if err := c.InsertRouter(ctx, "p", "r", &compute.Router{Name: "rt"}); err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
		if len(ops.waited) != 0 {
			t.Errorf("waited on %v, already done", ops.waited)
		}
	})
	t.Run("call refused", func(t *testing.T) {
		_, opts := newAPI(t, map[string]any{"POST /projects/p/regions/r/routers": http.StatusForbidden})
		c, ops := computeExtras(t, opts)
		err := c.InsertRouter(ctx, "p", "r", &compute.Router{Name: "rt"})
		if err == nil || install.IsNotFound(err) || !strings.Contains(err.Error(), "403") {
			t.Errorf("got %v, want the 403", err)
		}
		if len(ops.waited) != 0 {
			t.Errorf("waited on %v after a failed call", ops.waited)
		}
	})
	t.Run("delete of nothing", func(t *testing.T) {
		_, opts := newAPI(t, nil)
		c, _ := computeExtras(t, opts)
		if err := c.DeleteRouter(ctx, "p", "r", "rt"); !install.IsNotFound(err) {
			t.Errorf("got %v, want not found", err)
		}
	})
	t.Run("succeeds", func(t *testing.T) {
		_, opts := newAPI(t, map[string]any{"DELETE /projects/p/regions/r/routers/rt": map[string]any{"name": "op-1", "status": "DONE"}})
		c, _ := computeExtras(t, opts)
		if err := c.DeleteRouter(ctx, "p", "r", "rt"); err != nil {
			t.Error(err)
		}
	})
}

func TestInstanceZone(t *testing.T) {
	ctx := context.Background()
	region := map[string]any{"name": "r", "zones": []string{
		"https://www.googleapis.com/compute/v1/projects/p/zones/r-a",
		"https://www.googleapis.com/compute/v1/projects/p/zones/r-b",
		"https://www.googleapis.com/compute/v1/projects/p/zones/r-c"}}
	for _, tc := range []struct {
		name     string
		routes   map[string]any
		zone     string
		err      bool
		notFound bool
		calls    []string
	}{
		{name: "second zone", routes: map[string]any{
			"GET /projects/p/regions/r":              region,
			"GET /projects/p/zones/r-b/instances/vm": map[string]any{"name": "vm"},
		}, zone: "r-b", calls: []string{"GET /projects/p/regions/r",
			"GET /projects/p/zones/r-a/instances/vm", "GET /projects/p/zones/r-b/instances/vm"}},
		{name: "nowhere", routes: map[string]any{"GET /projects/p/regions/r": region},
			calls: []string{"GET /projects/p/regions/r", "GET /projects/p/zones/r-a/instances/vm",
				"GET /projects/p/zones/r-b/instances/vm", "GET /projects/p/zones/r-c/instances/vm"}},
		// Anything but not-found stops the search.
		{name: "refused", routes: map[string]any{
			"GET /projects/p/regions/r":              region,
			"GET /projects/p/zones/r-a/instances/vm": http.StatusForbidden,
			"GET /projects/p/zones/r-b/instances/vm": map[string]any{"name": "vm"},
		}, err: true, calls: []string{"GET /projects/p/regions/r", "GET /projects/p/zones/r-a/instances/vm"}},
		{name: "no such region", err: true, notFound: true, calls: []string{"GET /projects/p/regions/r"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, opts := newAPI(t, tc.routes)
			c, _ := computeExtras(t, opts)
			zone, err := c.InstanceZone(ctx, "p", "r", "vm")
			if zone != tc.zone || (err != nil) != tc.err || install.IsNotFound(err) != tc.notFound {
				t.Errorf("got %q, %v; want %q, error %v", zone, err, tc.zone, tc.err)
			}
			if got := a.calls(); !reflect.DeepEqual(got, tc.calls) {
				t.Errorf("calls %v, want %v", got, tc.calls)
			}
		})
	}
}

func TestGroupInstances(t *testing.T) {
	const path = "/projects/p/zones/z/instanceGroups/g/listInstances"
	a, opts := newAPI(t, map[string]any{"POST " + path: func(r *http.Request) any {
		if r.URL.Query().Get("pageToken") == "" {
			return map[string]any{"nextPageToken": "2", "items": []any{
				map[string]any{"instance": "https://www.googleapis.com/compute/v1/projects/p/zones/z/instances/a"}}}
		}
		return map[string]any{"items": []any{
			map[string]any{"instance": "https://www.googleapis.com/compute/v1/projects/p/zones/z/instances/b"}}}
	}})
	c, _ := computeExtras(t, opts)
	got, err := c.GroupInstances(context.Background(), "p", "z", "g")
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("got %v, %v", got, err)
	}
	if len(a.reqs) != 2 || a.reqs[1].Query.Get("pageToken") != "2" {
		t.Fatalf("requests %+v, want two pages", a.reqs)
	}
	for _, r := range a.reqs {
		jsonEq(t, "listInstances", r.Body, `{"instanceState":"ALL"}`)
	}

	_, opts = newAPI(t, nil)
	c, _ = computeExtras(t, opts)
	if _, err := c.GroupInstances(context.Background(), "p", "z", "gone"); !install.IsNotFound(err) {
		t.Errorf("missing group: %v, want not found", err)
	}
}

func TestInstanceIamPolicy(t *testing.T) {
	ctx := context.Background()
	const path = "/projects/p/zones/z/instances/vm/"
	a, opts := newAPI(t, map[string]any{
		"GET " + path + "getIamPolicy": map[string]any{"version": 3, "etag": "BwE=", "bindings": []any{
			map[string]any{"role": "roles/compute.osAdminLogin", "members": []string{"user:a@example.com"}},
			map[string]any{"role": "roles/compute.osLogin", "members": []string{"user:b@example.com"},
				"condition": map[string]any{"title": "t", "description": "d", "expression": "request.time < x"}},
		}},
		"POST " + path + "setIamPolicy": map[string]any{},
	})
	c, _ := computeExtras(t, opts)
	p, err := c.GetInstanceIamPolicy(ctx, "p", "z", "vm")
	if err != nil {
		t.Fatal(err)
	}
	want := &install.Policy{Version: 3, Etag: "BwE=", Bindings: []*install.Binding{
		{Role: "roles/compute.osAdminLogin", Members: []string{"user:a@example.com"}},
		{Role: "roles/compute.osLogin", Members: []string{"user:b@example.com"},
			Condition: &install.Condition{Title: "t", Description: "d", Expression: "request.time < x"}},
	}}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %+v, want %+v", p, want)
	}
	if v := a.reqs[0].Query.Get("optionsRequestedPolicyVersion"); v != "3" {
		t.Errorf("requested policy version %q, want 3", v)
	}

	p.Version = 1 // whatever was read, version 3 is written
	if err := c.SetInstanceIamPolicy(ctx, "p", "z", "vm", p); err != nil {
		t.Fatal(err)
	}
	jsonEq(t, "setIamPolicy", a.reqs[1].Body, `{"policy":{"version":3,"etag":"BwE=","bindings":[
		{"role":"roles/compute.osAdminLogin","members":["user:a@example.com"]},
		{"role":"roles/compute.osLogin","members":["user:b@example.com"],
			"condition":{"title":"t","description":"d","expression":"request.time < x"}}]}}`)

	if _, err := c.GetInstanceIamPolicy(ctx, "p", "z", "gone"); !install.IsNotFound(err) {
		t.Errorf("missing instance: %v, want not found", err)
	}
}
