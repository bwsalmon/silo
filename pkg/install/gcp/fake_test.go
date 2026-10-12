package gcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

// request is one call a wrapper made.
type request struct {
	Method, Path string
	Query        url.Values
	Body         any
}

// api is a fake Google API. routes maps "METHOD /path" to its reply: an
// int is an error with that status, a func(*http.Request) any is called
// for the reply, and anything else is the JSON reply. A route it doesn't
// have is a 404.
type api struct {
	routes map[string]any

	mu   sync.Mutex
	reqs []request
}

// newAPI serves routes, and returns the fake and client options pointing
// at it.
func newAPI(t *testing.T, routes map[string]any) (*api, []option.ClientOption) {
	a := &api{routes: routes}
	return a, server(t, a.serve)
}

func (a *api) serve(w http.ResponseWriter, r *http.Request) {
	var body any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		json.Unmarshal(b, &body)
	}
	a.mu.Lock()
	a.reqs = append(a.reqs, request{r.Method, r.URL.Path, r.URL.Query(), body})
	a.mu.Unlock()
	v, ok := a.routes[r.Method+" "+r.URL.Path]
	if !ok {
		v = http.StatusNotFound
	}
	if f, ok := v.(func(*http.Request) any); ok {
		v = f(r)
	}
	if code, ok := v.(int); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": http.StatusText(code)}})
		return
	}
	reply(w, v)
}

// calls is every request made so far, as "METHOD /path".
func (a *api) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.reqs {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// only is the one request made so far, failing the test if there were
// more or none.
func (a *api) only(t *testing.T) request {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.reqs) != 1 {
		t.Fatalf("%d requests, want 1: %+v", len(a.reqs), a.reqs)
	}
	return a.reqs[0]
}

// jsonEq checks that got, a decoded JSON body, is the JSON want.
func jsonEq(t *testing.T, what string, got any, want string) {
	t.Helper()
	var w any
	if want != "" {
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatalf("bad want %s: %v", want, err)
		}
	}
	if !reflect.DeepEqual(got, w) {
		g, _ := json.Marshal(got)
		t.Errorf("%s body %s, want %s", what, g, want)
	}
}

// fakeOps is gce.Operations for ComputeExtras.wait: every operation it
// is asked to wait on is DONE, as done if that is set.
type fakeOps struct {
	gce.Operations
	done *compute.Operation

	mu     sync.Mutex
	waited []string
}

func (f *fakeOps) WaitOperation(_ context.Context, project string, scope gce.Scope, name string) (*compute.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waited = append(f.waited, project+" "+scope.String()+" "+name)
	if f.done != nil {
		return f.done, nil
	}
	return &compute.Operation{Name: name, Status: "DONE"}, nil
}
