package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI answers each path with the next of its answers, the last one
// repeating, and records the requests.
type fakeAPI struct {
	mu      sync.Mutex
	answers map[string][]any
	reqs    []*http.Request
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/silo/v1/")
	l := f.answers[key]
	if len(l) == 0 {
		http.Error(w, "no answer for "+key, http.StatusNotFound)
		return
	}
	a := l[0]
	if len(l) > 1 {
		f.answers[key] = l[1:]
	}
	json.NewEncoder(w).Encode(a)
}

func newFake(t *testing.T, answers map[string][]any) (*Client, *fakeAPI) {
	f := &fakeAPI{answers: answers}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Options{Endpoint: srv.URL + "/silo/v1", UserAgent: "test-agent"})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func TestWaitOperation(t *testing.T) {
	ctx := context.Background()
	t.Run("done", func(t *testing.T) {
		c, f := newFake(t, map[string][]any{"GET operations/op1": {
			Operation{ID: "op1", State: OpDeleting}, Operation{ID: "op1", State: OpDeleting}, Operation{ID: "op1", Done: true, State: "DELETED"}}})
		op, err := c.WaitOperation(ctx, "operations/op1", time.Millisecond)
		if err != nil || !op.Done || op.State != "DELETED" {
			t.Fatalf("WaitOperation = %+v, %v", op, err)
		}
		if len(f.reqs) != 3 {
			t.Errorf("%d polls, want 3", len(f.reqs))
		}
		if f.reqs[0].Header.Get("User-Agent") != "test-agent" || f.reqs[0].Header.Get("Accept") != "application/json" {
			t.Errorf("headers %v", f.reqs[0].Header)
		}
	})
	t.Run("failed", func(t *testing.T) {
		c, _ := newFake(t, map[string][]any{"GET operations/op2": {
			Operation{ID: "op2", Done: true, Error: &Error{Code: "FAILED_PRECONDITION", Message: "in use"}}}})
		op, err := c.WaitOperation(ctx, "op2", time.Millisecond)
		if !IsCode(err, "FAILED_PRECONDITION") || op == nil || op.ID != "op2" {
			t.Errorf("WaitOperation = %+v, %v", op, err)
		}
	})
	t.Run("delete failed without an error", func(t *testing.T) {
		c, _ := newFake(t, map[string][]any{"GET operations/op3": {Operation{ID: "op3", State: OpDeleteFailed}}})
		op, err := c.WaitOperation(ctx, "op3", time.Millisecond)
		if !IsCode(err, "INTERNAL") || op == nil {
			t.Errorf("WaitOperation = %+v, %v", op, err)
		}
	})
	t.Run("lookup fails", func(t *testing.T) {
		c, _ := newFake(t, nil)
		if op, err := c.WaitOperation(ctx, "gone", time.Millisecond); !IsCode(err, "HTTP_404") || op != nil {
			t.Errorf("WaitOperation = %+v, %v", op, err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		c, _ := newFake(t, map[string][]any{"GET operations/op4": {Operation{ID: "op4", State: OpDeleting}}})
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		op, err := c.WaitOperation(cctx, "op4", time.Hour)
		if !errors.Is(err, context.DeadlineExceeded) || op == nil || op.State != OpDeleting {
			t.Errorf("WaitOperation = %+v, %v", op, err)
		}
	})
}

func TestReconcile(t *testing.T) {
	c, f := newFake(t, map[string][]any{
		"GET reconcile":        {map[string]string{"mode": "report-only"}},
		"POST reconcile:apply": {map[string]string{"mode": "apply"}},
	})
	ctx := context.Background()
	if mode, err := c.ReconcileMode(ctx); err != nil || mode != "report-only" {
		t.Errorf("ReconcileMode = %q, %v", mode, err)
	}
	if mode, err := c.ReconcileApply(ctx, Mutation{RequestID: "r1"}); err != nil || mode != "apply" {
		t.Errorf("ReconcileApply = %q, %v", mode, err)
	}
	last := f.reqs[len(f.reqs)-1]
	if last.URL.Query().Get("request_id") != "r1" || last.Header.Get("Content-Type") != "application/json" {
		t.Errorf("ReconcileApply sent %s %v", last.URL, last.Header)
	}
}

func TestRemoveAllCredentials(t *testing.T) {
	c, f := newFake(t, map[string][]any{"DELETE credentials": {map[string][]string{"deleted": {"a", "b"}}}})
	names, err := c.RemoveAllCredentials(context.Background(), Mutation{ValidateOnly: true})
	if err != nil || strings.Join(names, ",") != "a,b" {
		t.Errorf("RemoveAllCredentials = %v, %v", names, err)
	}
	if f.reqs[0].URL.Query().Get("validate_only") != "true" {
		t.Errorf("sent %s", f.reqs[0].URL)
	}
	c, _ = newFake(t, nil)
	if _, err := c.RemoveAllCredentials(context.Background(), Mutation{}); err == nil {
		t.Error("RemoveAllCredentials on a refusal: no error")
	}
}

// TestRootSiloPath proves "/" is found through self, and only by the
// root's own callers.
func TestRootSiloPath(t *testing.T) {
	ctx := context.Background()
	c, _ := newFake(t, map[string][]any{"GET self": {map[string]any{"id": "r00t", "parent": nil}}})
	if p, err := c.SiloPath(ctx, "/"); err != nil || p != "silos/-r00t" {
		t.Errorf(`SiloPath("/") as the root = %q, %v`, p, err)
	}
	c, _ = newFake(t, map[string][]any{"GET self": {map[string]any{"id": "k7f3qa", "parent": "r00t"}}})
	if _, err := c.SiloPath(ctx, "/"); !IsCode(err, "NOT_FOUND") {
		t.Errorf(`SiloPath("/") below the root = %v, want NOT_FOUND`, err)
	}
}

func TestDoDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c, err := New(Options{Endpoint: srv.URL + "/silo/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := c.Do(context.Background(), http.MethodGet, "self", nil, nil, &out); err == nil || !strings.Contains(err.Error(), "not the JSON expected") {
		t.Errorf("Do = %v", err)
	}
	// With nothing to decode into, the answer isn't read as JSON.
	if err := c.Do(context.Background(), http.MethodGet, "self", nil, nil, nil); err != nil {
		t.Errorf("Do with no out = %v", err)
	}
}

func TestNewDefaults(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Endpoint().String(); got != DefaultEndpoint {
		t.Errorf("Endpoint = %s, want %s", got, DefaultEndpoint)
	}
	if c.hc.Timeout != 30*time.Second || c.ua != "silo-client" {
		t.Errorf("timeout %s, user agent %q", c.hc.Timeout, c.ua)
	}
	// Endpoint is a copy: changing it changes nothing.
	c.Endpoint().Path = "/elsewhere/"
	if c.Endpoint().Path != "/silo/v1/" {
		t.Errorf("Endpoint was changed through its copy: %s", c.Endpoint())
	}
}
