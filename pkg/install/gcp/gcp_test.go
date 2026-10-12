package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/install"
	artifactregistry "google.golang.org/api/artifactregistry/v1"
	cloudbilling "google.golang.org/api/cloudbilling/v1"
	crm "google.golang.org/api/cloudresourcemanager/v1"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	serviceusage "google.golang.org/api/serviceusage/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// server serves handler, and returns client options pointing at it.
func server(t *testing.T, handler http.HandlerFunc) []option.ClientOption {
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return []option.ClientOption{option.WithEndpoint(srv.URL + "/"), option.WithoutAuthentication()}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestResourceManager(t *testing.T) {
	ctx := context.Background()
	var tested []int
	opts := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/projects/p-1"):
			reply(w, map[string]any{"projectId": "p-1", "projectNumber": "42", "parent": map[string]string{"type": "folder", "id": "7"}})
		case strings.HasSuffix(r.URL.Path, ":testIamPermissions"):
			var req struct{ Permissions []string }
			json.NewDecoder(r.Body).Decode(&req)
			tested = append(tested, len(req.Permissions))
			reply(w, map[string]any{"permissions": req.Permissions[:1]})
		case strings.HasSuffix(r.URL.Path, ":getEffectiveOrgPolicy"):
			var req struct{ Constraint string }
			json.NewDecoder(r.Body).Decode(&req)
			if req.Constraint != "constraints/gcp.resourceLocations" {
				t.Errorf("constraint %q", req.Constraint)
			}
			reply(w, map[string]any{"listPolicy": map[string]any{"allowedValues": []string{"in:europe-locations"}}})
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	})
	svc, err := crm.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rm := ResourceManager{svc}
	p, err := rm.GetProject(ctx, "p-1")
	if err != nil || *p != (install.Project{ID: "p-1", Number: 42, Parent: "folders/7"}) {
		t.Errorf("GetProject: %+v %v", p, err)
	}
	perms := make([]string, 150)
	for i := range perms {
		perms[i] = fmt.Sprintf("x.y.z%d", i)
	}
	got, err := rm.TestIamPermissions(ctx, "p-1", perms)
	if err != nil || !reflect.DeepEqual(tested, []int{100, 50}) || len(got) != 2 {
		t.Errorf("TestIamPermissions: batches %v, got %v %v", tested, got, err)
	}
	pol, err := rm.EffectiveOrgPolicy(ctx, "p-1", "gcp.resourceLocations")
	if err != nil || !pol.Restricts() || pol.Permits("us-central1", "in:us-locations") || !pol.Permits("europe-west4", "in:europe-locations") {
		t.Errorf("EffectiveOrgPolicy: %+v %v", pol, err)
	}
}

func TestServiceUsageEnableWaits(t *testing.T) {
	ctx := context.Background()
	pollEvery = time.Millisecond
	var polls atomic.Int32
	var enabled []string
	opts := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/services:batchEnable"):
			var req struct{ ServiceIds []string }
			json.NewDecoder(r.Body).Decode(&req)
			enabled = append(enabled, req.ServiceIds...)
			reply(w, map[string]any{"name": "operations/op1"})
		case strings.HasSuffix(r.URL.Path, "/operations/op1"):
			reply(w, map[string]any{"name": "operations/op1", "done": polls.Add(1) > 2})
		case strings.HasSuffix(r.URL.Path, "/services"):
			if r.URL.Query().Get("filter") != "state:ENABLED" {
				t.Errorf("filter %q", r.URL.Query().Get("filter"))
			}
			reply(w, map[string]any{"services": []any{map[string]any{"config": map[string]string{"name": "compute.googleapis.com"}}}})
		default:
			http.Error(w, r.URL.Path, http.StatusNotFound)
		}
	})
	svc, err := serviceusage.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	su := ServiceUsage{svc}
	if err := su.EnableServices(ctx, "p-1", install.Services); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(enabled, install.Services) || polls.Load() < 3 {
		t.Errorf("enabled %v after %d polls", enabled, polls.Load())
	}
	list, err := su.ListEnabledServices(ctx, "p-1")
	if err != nil || !reflect.DeepEqual(list, []string{"compute.googleapis.com"}) {
		t.Errorf("ListEnabledServices: %v %v", list, err)
	}
}

func TestSQLAdminVersions(t *testing.T) {
	ctx := context.Background()
	opts := server(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"items": []any{
			map[string]any{"name": "a", "appliesTo": []string{"POSTGRES_17", "POSTGRES_16"}},
			map[string]any{"name": "b", "appliesTo": []string{"POSTGRES_16", "MYSQL_8_0"}},
		}})
	})
	svc, err := sqladmin.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SQLAdmin{svc}.ListDatabaseVersions(ctx, "p-1")
	if err != nil || !reflect.DeepEqual(got, []string{"MYSQL_8_0", "POSTGRES_16", "POSTGRES_17"}) {
		t.Errorf("got %v %v", got, err)
	}
}

// TestInstanceZoneWithNoRegion: with no home region (uninstall with no
// install record), InstanceZone finds nothing rather than asking for
// regions/"", which GCE answers with the region list, whose id ("projects/
// p/regions") doesn't decode as a Region's uint64.
func TestInstanceZoneWithNoRegion(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	opts := server(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		reply(w, map[string]any{"kind": "compute#regionList", "id": "projects/p/regions/", "items": []any{}})
	})
	svc, err := compute.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	zone, err := ComputeExtras{s: svc}.InstanceZone(ctx, "p", "", "silo-control")
	if zone != "" || err != nil {
		t.Errorf("got %q, %v; want no zone and no error", zone, err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d calls to GCE, want none", n)
	}
}

func TestBillingEnabled(t *testing.T) {
	ctx := context.Background()
	_, opts := newAPI(t, map[string]any{
		"GET /v1/projects/on/billingInfo":  map[string]any{"billingEnabled": true, "billingAccountName": "billingAccounts/1"},
		"GET /v1/projects/off/billingInfo": map[string]any{},
		"GET /v1/projects/no/billingInfo":  http.StatusForbidden,
	})
	svc, err := cloudbilling.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	b := Billing{svc}
	if on, err := b.BillingEnabled(ctx, "on"); !on || err != nil {
		t.Errorf("on: %v, %v", on, err)
	}
	if on, err := b.BillingEnabled(ctx, "off"); on || err != nil {
		t.Errorf("off: %v, %v", on, err)
	}
	if on, err := b.BillingEnabled(ctx, "no"); on || err == nil {
		t.Errorf("refused: %v, %v", on, err)
	}
}

func TestSQLAdminTiersAndInstances(t *testing.T) {
	ctx := context.Background()
	a, opts := newAPI(t, map[string]any{
		"GET /v1/projects/p/tiers": map[string]any{"items": []any{
			map[string]any{"tier": "db-f1-micro", "region": []string{"us-central1", "europe-west4"}},
			map[string]any{"tier": "db-custom-2-7680"},
		}},
		"GET /v1/projects/p/instances": func(r *http.Request) any {
			if r.URL.Query().Get("pageToken") == "" {
				return map[string]any{"nextPageToken": "2", "items": []any{
					map[string]any{"name": "silo", "region": "europe-west4", "databaseVersion": "POSTGRES_16", "state": "RUNNABLE",
						"settings": map[string]any{"tier": "db-f1-micro", "userLabels": map[string]string{"silo": "1"}}}}}
			}
			return map[string]any{"items": []any{map[string]any{"name": "other", "region": "us-central1"}}}
		},
	})
	svc, err := sqladmin.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	s := SQLAdmin{svc}
	tiers, err := s.ListTiers(ctx, "p")
	want := []install.SQLTier{{Tier: "db-f1-micro", Regions: []string{"us-central1", "europe-west4"}}, {Tier: "db-custom-2-7680"}}
	if err != nil || !reflect.DeepEqual(tiers, want) {
		t.Errorf("ListTiers: %+v, %v", tiers, err)
	}
	ins, err := s.ListInstances(ctx, "p")
	if err != nil || len(ins) != 2 {
		t.Fatalf("ListInstances: %+v, %v", ins, err)
	}
	if in := ins[0]; in.Name != "silo" || in.Region != "europe-west4" || in.DatabaseVersion != "POSTGRES_16" ||
		in.State != "RUNNABLE" || in.Tier != "db-f1-micro" || in.Labels["silo"] != "1" {
		t.Errorf("first instance %+v", in)
	}
	if in := ins[1]; in.Name != "other" || in.Region != "us-central1" {
		t.Errorf("second instance %+v", in)
	}
	if got := a.calls(); len(got) != 3 || a.reqs[2].Query.Get("pageToken") != "2" {
		t.Errorf("calls %v, want tiers then two pages of instances", got)
	}
	if _, err := s.ListTiers(ctx, "gone"); !install.IsNotFound(err) {
		t.Errorf("ListTiers missing: %v, want not found", err)
	}
	if _, err := s.ListInstances(ctx, "gone"); !install.IsNotFound(err) {
		t.Errorf("ListInstances missing: %v, want not found", err)
	}
}

func TestArtifactRegistry(t *testing.T) {
	ctx := context.Background()
	pollEvery = time.Millisecond
	if got := repoParent("p", "europe-west4"); got != "projects/p/locations/europe-west4" {
		t.Errorf("repoParent %q", got)
	}
	const repos = "/v1/projects/p/locations/europe-west4/repositories"
	var polls atomic.Int32
	a, opts := newAPI(t, map[string]any{
		"GET " + repos + "/silo": map[string]any{"name": "projects/p/locations/europe-west4/repositories/silo",
			"format": "DOCKER", "labels": map[string]string{"silo": "1"}},
		"POST " + repos:             map[string]any{"name": "projects/p/locations/europe-west4/operations/op1"},
		"DELETE " + repos + "/silo": map[string]any{"name": "projects/p/locations/europe-west4/operations/op2"},
		"GET /v1/projects/p/locations/europe-west4/operations/op1": func(*http.Request) any {
			return map[string]any{"name": "projects/p/locations/europe-west4/operations/op1", "done": polls.Add(1) > 1}
		},
		"GET /v1/projects/p/locations/europe-west4/operations/op2": map[string]any{"name": "op2", "done": true,
			"error": map[string]any{"code": 9, "message": "repository not empty"}},
	})
	svc, err := artifactregistry.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ar := ArtifactRegistry{svc}
	r, err := ar.GetRepository(ctx, "p", "europe-west4", "silo")
	if want := (&install.Repository{Name: "silo", Format: "DOCKER", Labels: map[string]string{"silo": "1"}}); err != nil || !reflect.DeepEqual(r, want) {
		t.Errorf("GetRepository: %+v, %v", r, err)
	}
	if _, err := ar.GetRepository(ctx, "p", "europe-west4", "gone"); !install.IsNotFound(err) {
		t.Errorf("GetRepository missing: %v, want not found", err)
	}
	if err := ar.CreateRepository(ctx, "p", "europe-west4", &install.Repository{Name: "silo", Format: "DOCKER"}); err != nil {
		t.Fatal(err)
	}
	create := a.reqs[2]
	if create.Query.Get("repositoryId") != "silo" || polls.Load() != 2 {
		t.Errorf("create %+v after %d polls", create, polls.Load())
	}
	jsonEq(t, "create", create.Body, `{"format":"DOCKER"}`)
	if err := ar.DeleteRepository(ctx, "p", "europe-west4", "silo"); err == nil || err.Error() != "deleting repository silo: repository not empty" {
		t.Errorf("DeleteRepository: %v", err)
	}
}
