package gcp

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/install"
	crm "google.golang.org/api/cloudresourcemanager/v1"
	dns "google.golang.org/api/dns/v1"
	iam "google.golang.org/api/iam/v1"
)

func TestProjectIamPolicy(t *testing.T) {
	ctx := context.Background()
	a, opts := newAPI(t, map[string]any{
		"POST /v1/projects/p:getIamPolicy": map[string]any{"version": 3, "etag": "BwE=", "bindings": []any{
			map[string]any{"role": "roles/viewer", "members": []string{"user:a@example.com"}},
			map[string]any{"role": "roles/editor", "members": []string{"user:b@example.com"},
				"condition": map[string]any{"title": "t", "expression": "request.time < x"}},
		}},
		"POST /v1/projects/p:setIamPolicy": map[string]any{},
	})
	svc, err := crm.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rm := ResourceManager{svc}
	p, err := rm.GetIamPolicy(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	want := &install.Policy{Version: 3, Etag: "BwE=", Bindings: []*install.Binding{
		{Role: "roles/viewer", Members: []string{"user:a@example.com"}},
		{Role: "roles/editor", Members: []string{"user:b@example.com"},
			Condition: &install.Condition{Title: "t", Expression: "request.time < x"}},
	}}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %+v, want %+v", p, want)
	}
	// Asking for version 3 is what keeps conditional bindings.
	jsonEq(t, "getIamPolicy", a.reqs[0].Body, `{"options":{"requestedPolicyVersion":3}}`)

	p.Version = 1
	p.Bindings[0].Members = append(p.Bindings[0].Members, "user:c@example.com")
	if err := rm.SetIamPolicy(ctx, "p", p); err != nil {
		t.Fatal(err)
	}
	jsonEq(t, "setIamPolicy", a.reqs[1].Body, `{"policy":{"version":3,"etag":"BwE=","bindings":[
		{"role":"roles/viewer","members":["user:a@example.com","user:c@example.com"]},
		{"role":"roles/editor","members":["user:b@example.com"],"condition":{"title":"t","expression":"request.time < x"}}]}}`)

	if _, err := rm.GetIamPolicy(ctx, "gone"); !install.IsNotFound(err) {
		t.Errorf("missing project: %v, want not found", err)
	}
}

func iamClient(t *testing.T, routes map[string]any) (*api, IAM) {
	t.Helper()
	a, opts := newAPI(t, routes)
	svc, err := iam.NewService(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return a, IAM{svc}
}

func TestServiceAccounts(t *testing.T) {
	ctx := context.Background()
	const sa = "/v1/projects/p/serviceAccounts/silo@p.iam.gserviceaccount.com"
	a, c := iamClient(t, map[string]any{
		"GET " + sa: map[string]any{"email": "silo@p.iam.gserviceaccount.com", "displayName": "Silo",
			"description": "d", "disabled": true, "name": "projects/p/serviceAccounts/silo@p.iam.gserviceaccount.com"},
		"POST /v1/projects/p/serviceAccounts": map[string]any{"email": "silo@p.iam.gserviceaccount.com", "displayName": "Silo", "description": "d"},
		"PATCH " + sa:                         map[string]any{},
		"POST " + sa + ":enable":              map[string]any{},
		"DELETE " + sa:                        map[string]any{},
	})

	got, err := c.GetServiceAccount(ctx, "p", "silo@p.iam.gserviceaccount.com")
	if want := (install.ServiceAccount{Email: "silo@p.iam.gserviceaccount.com", DisplayName: "Silo", Description: "d", Disabled: true}); err != nil || *got != want {
		t.Errorf("GetServiceAccount: %+v, %v", got, err)
	}
	if _, err := c.GetServiceAccount(ctx, "p", "gone@p.iam.gserviceaccount.com"); !install.IsNotFound(err) {
		t.Errorf("GetServiceAccount missing: %v, want not found", err)
	}

	created, err := c.CreateServiceAccount(ctx, "p", "silo", &install.ServiceAccount{DisplayName: "Silo", Description: "d"})
	if want := (install.ServiceAccount{Email: "silo@p.iam.gserviceaccount.com", DisplayName: "Silo", Description: "d"}); err != nil || *created != want {
		t.Errorf("CreateServiceAccount: %+v, %v", created, err)
	}
	jsonEq(t, "create", a.reqs[2].Body, `{"accountId":"silo","serviceAccount":{"displayName":"Silo","description":"d"}}`)

	if err := c.PatchServiceAccount(ctx, "p", &install.ServiceAccount{Email: "silo@p.iam.gserviceaccount.com", DisplayName: "Silo 2"}); err != nil {
		t.Fatal(err)
	}
	// An empty description is in the mask, so it is cleared.
	jsonEq(t, "patch", a.reqs[3].Body, `{"serviceAccount":{"displayName":"Silo 2"},"updateMask":"displayName,description"}`)

	if err := c.EnableServiceAccount(ctx, "p", "silo@p.iam.gserviceaccount.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteServiceAccount(ctx, "p", "silo@p.iam.gserviceaccount.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteServiceAccount(ctx, "p", "gone@p.iam.gserviceaccount.com"); !install.IsNotFound(err) {
		t.Errorf("DeleteServiceAccount missing: %v, want not found", err)
	}
	want := []string{"GET " + sa, "GET /v1/projects/p/serviceAccounts/gone@p.iam.gserviceaccount.com",
		"POST /v1/projects/p/serviceAccounts", "PATCH " + sa, "POST " + sa + ":enable", "DELETE " + sa,
		"DELETE /v1/projects/p/serviceAccounts/gone@p.iam.gserviceaccount.com"}
	if got := a.calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

func TestRoles(t *testing.T) {
	ctx := context.Background()
	const role = "/v1/projects/p/roles/siloInstaller"
	r := &install.Role{Title: "Silo", Description: "d", Permissions: []string{"a.b.c", "d.e.f"}, Stage: "GA"}
	const body = `{"title":"Silo","description":"d","includedPermissions":["a.b.c","d.e.f"],"stage":"GA"}`

	t.Run("get", func(t *testing.T) {
		_, c := iamClient(t, map[string]any{"GET " + role: map[string]any{"name": "projects/p/roles/siloInstaller",
			"title": "Silo", "description": "d", "includedPermissions": []string{"a.b.c", "d.e.f"}, "stage": "GA", "deleted": true}})
		got, err := c.GetRole(ctx, "p", "siloInstaller")
		want := *r
		want.Deleted = true
		if err != nil || !reflect.DeepEqual(*got, want) {
			t.Errorf("got %+v, %v", got, err)
		}
		if _, err := c.GetRole(ctx, "p", "gone"); !install.IsNotFound(err) {
			t.Errorf("missing: %v, want not found", err)
		}
	})
	t.Run("create", func(t *testing.T) {
		a, c := iamClient(t, map[string]any{"POST /v1/projects/p/roles": map[string]any{}})
		if err := c.CreateRole(ctx, "p", "siloInstaller", r); err != nil {
			t.Fatal(err)
		}
		jsonEq(t, "create", a.only(t).Body, `{"roleId":"siloInstaller","role":`+body+`}`)
	})
	t.Run("update", func(t *testing.T) {
		a, c := iamClient(t, map[string]any{
			"GET " + role:   map[string]any{"name": "projects/p/roles/siloInstaller", "etag": "e1"},
			"PATCH " + role: map[string]any{},
		})
		if err := c.UpdateRole(ctx, "p", "siloInstaller", r); err != nil {
			t.Fatal(err)
		}
		if got, want := a.calls(), []string{"GET " + role, "PATCH " + role}; !reflect.DeepEqual(got, want) {
			t.Errorf("calls %v, want %v", got, want)
		}
		if m := a.reqs[1].Query.Get("updateMask"); m != "title,description,includedPermissions,stage" {
			t.Errorf("updateMask %q", m)
		}
		jsonEq(t, "patch", a.reqs[1].Body, body)
	})
	t.Run("update undeletes", func(t *testing.T) {
		a, c := iamClient(t, map[string]any{
			"GET " + role:                map[string]any{"name": "projects/p/roles/siloInstaller", "etag": "e1", "deleted": true},
			"POST " + role + ":undelete": map[string]any{},
			"PATCH " + role:              map[string]any{},
		})
		if err := c.UpdateRole(ctx, "p", "siloInstaller", r); err != nil {
			t.Fatal(err)
		}
		if got, want := a.calls(), []string{"GET " + role, "POST " + role + ":undelete", "PATCH " + role}; !reflect.DeepEqual(got, want) {
			t.Errorf("calls %v, want %v", got, want)
		}
		jsonEq(t, "undelete", a.reqs[1].Body, `{"etag":"e1"}`)
	})
	t.Run("update undelete fails", func(t *testing.T) {
		a, c := iamClient(t, map[string]any{
			"GET " + role:                map[string]any{"name": "projects/p/roles/siloInstaller", "deleted": true},
			"POST " + role + ":undelete": http.StatusForbidden,
		})
		err := c.UpdateRole(ctx, "p", "siloInstaller", r)
		if err == nil || !strings.HasPrefix(err.Error(), "undeleting projects/p/roles/siloInstaller: ") {
			t.Errorf("got %v", err)
		}
		if n := len(a.calls()); n != 2 {
			t.Errorf("%d calls, want no patch after the failed undelete", n)
		}
	})
	t.Run("update missing", func(t *testing.T) {
		_, c := iamClient(t, nil)
		if err := c.UpdateRole(ctx, "p", "siloInstaller", r); !install.IsNotFound(err) {
			t.Errorf("got %v, want not found", err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		a, c := iamClient(t, map[string]any{"DELETE " + role: map[string]any{}})
		if err := c.DeleteRole(ctx, "p", "siloInstaller"); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteRole(ctx, "p", "gone"); !install.IsNotFound(err) {
			t.Errorf("missing: %v, want not found", err)
		}
		if got := a.calls()[0]; got != "DELETE "+role {
			t.Errorf("call %s", got)
		}
	})
}

func dnsClient(t *testing.T, routes map[string]any) (*api, DNS) {
	t.Helper()
	a, opts := newAPI(t, routes)
	svc, err := dns.NewService(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return a, DNS{svc}
}

func TestManagedZones(t *testing.T) {
	ctx := context.Background()
	const zone = "/dns/v1/projects/p/managedZones/silo"
	t.Run("get", func(t *testing.T) {
		_, c := dnsClient(t, map[string]any{"GET " + zone: map[string]any{"name": "silo", "dnsName": "silo.internal.",
			"description": "d", "labels": map[string]string{"silo": "1"}, "visibility": "private",
			"privateVisibilityConfig": map[string]any{"networks": []any{map[string]string{"networkUrl": "n1"}, map[string]string{"networkUrl": "n2"}}}}})
		got, err := c.GetManagedZone(ctx, "p", "silo")
		want := &install.ManagedZone{Name: "silo", DNSName: "silo.internal.", Description: "d",
			Labels: map[string]string{"silo": "1"}, Networks: []string{"n1", "n2"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, %v", got, err)
		}
		if _, err := c.GetManagedZone(ctx, "p", "gone"); !install.IsNotFound(err) {
			t.Errorf("missing: %v, want not found", err)
		}
	})
	t.Run("create private", func(t *testing.T) {
		a, c := dnsClient(t, map[string]any{"POST /dns/v1/projects/p/managedZones": map[string]any{}})
		if err := c.CreateManagedZone(ctx, "p", &install.ManagedZone{Name: "silo", DNSName: "silo.internal.",
			Description: "d", Labels: map[string]string{"silo": "1"}, Networks: []string{"n1"}}); err != nil {
			t.Fatal(err)
		}
		jsonEq(t, "create", a.only(t).Body, `{"name":"silo","dnsName":"silo.internal.","description":"d",
			"labels":{"silo":"1"},"visibility":"private","privateVisibilityConfig":{"networks":[{"networkUrl":"n1"}]}}`)
	})
	t.Run("create public", func(t *testing.T) {
		a, c := dnsClient(t, map[string]any{"POST /dns/v1/projects/p/managedZones": map[string]any{}})
		if err := c.CreateManagedZone(ctx, "p", &install.ManagedZone{Name: "silo", DNSName: "silo.example.com."}); err != nil {
			t.Fatal(err)
		}
		jsonEq(t, "create", a.only(t).Body, `{"name":"silo","dnsName":"silo.example.com."}`)
	})
	t.Run("patch", func(t *testing.T) {
		a, c := dnsClient(t, map[string]any{"PATCH " + zone: map[string]any{}})
		if err := c.PatchManagedZone(ctx, "p", &install.ManagedZone{Name: "silo", DNSName: "other.", Description: "x",
			Networks: []string{"n1", "n2"}}); err != nil {
			t.Fatal(err)
		}
		// Only the networks change.
		jsonEq(t, "patch", a.only(t).Body, `{"privateVisibilityConfig":{"networks":[{"networkUrl":"n1"},{"networkUrl":"n2"}]}}`)
	})
	t.Run("delete", func(t *testing.T) {
		a, c := dnsClient(t, map[string]any{"DELETE " + zone: map[string]any{}})
		if err := c.DeleteManagedZone(ctx, "p", "silo"); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteManagedZone(ctx, "p", "gone"); !install.IsNotFound(err) {
			t.Errorf("missing: %v, want not found", err)
		}
		if got := a.calls()[0]; got != "DELETE "+zone {
			t.Errorf("call %s", got)
		}
	})
}

func TestRecordSets(t *testing.T) {
	ctx := context.Background()
	const rrsets = "/dns/v1/projects/p/managedZones/silo/rrsets"
	const rs = rrsets + "/db.silo.internal./A"
	a, c := dnsClient(t, map[string]any{
		"GET " + rs:      map[string]any{"name": "db.silo.internal.", "type": "A", "ttl": 300, "rrdatas": []string{"10.0.0.2"}},
		"POST " + rrsets: map[string]any{},
		"PATCH " + rs:    map[string]any{},
		"DELETE " + rs:   map[string]any{},
	})
	want := &install.RecordSet{Name: "db.silo.internal.", Type: "A", TTL: 300, Rrdatas: []string{"10.0.0.2"}}
	got, err := c.GetRecordSet(ctx, "p", "silo", "db.silo.internal.", "A")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("GetRecordSet: %+v, %v", got, err)
	}
	if _, err := c.GetRecordSet(ctx, "p", "silo", "db.silo.internal.", "AAAA"); !install.IsNotFound(err) {
		t.Errorf("GetRecordSet missing: %v, want not found", err)
	}
	if err := c.CreateRecordSet(ctx, "p", "silo", want); err != nil {
		t.Fatal(err)
	}
	if err := c.PatchRecordSet(ctx, "p", "silo", want); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRecordSet(ctx, "p", "silo", "db.silo.internal.", "A"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRecordSet(ctx, "p", "silo", "db.silo.internal.", "AAAA"); !install.IsNotFound(err) {
		t.Errorf("DeleteRecordSet missing: %v, want not found", err)
	}
	calls := []string{"GET " + rs, "GET " + rrsets + "/db.silo.internal./AAAA", "POST " + rrsets, "PATCH " + rs,
		"DELETE " + rs, "DELETE " + rrsets + "/db.silo.internal./AAAA"}
	if got := a.calls(); !reflect.DeepEqual(got, calls) {
		t.Errorf("calls %v, want %v", got, calls)
	}
	const body = `{"name":"db.silo.internal.","type":"A","ttl":300,"rrdatas":["10.0.0.2"]}`
	jsonEq(t, "create", a.reqs[2].Body, body)
	jsonEq(t, "patch", a.reqs[3].Body, body)
}
