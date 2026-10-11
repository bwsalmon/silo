package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSiloPath(t *testing.T) {
	c, err := New(Options{Endpoint: "http://silo.internal:8080/silo/v1"})
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]string{
		"":           "self",
		"self":       "self",
		"/team-a/ci": "silos/team-a/ci",
		"/team-a/":   "silos/team-a",
		"k7f3qa":     "silos/-k7f3qa",
		"-k7f3qa":    "silos/-k7f3qa",
	} {
		if got, err := c.SiloPath(context.Background(), ref); err != nil || got != want {
			t.Errorf("SiloPath(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := c.SiloPath(context.Background(), "team-a/ci"); err == nil {
		t.Error("SiloPath(team-a/ci): a relative path is refused")
	}
}

func TestNewRefusesBadEndpoints(t *testing.T) {
	for _, ep := range []string{"silo.internal:8080", "ftp://x/silo/v1/", "http://"} {
		if _, err := New(Options{Endpoint: ep}); err == nil {
			t.Errorf("New(%q): no error", ep)
		}
	}
}

// Do sends the request id and validate_only, escapes the path, and
// turns refusals into *Error, even one that isn't the API's JSON.
func TestDo(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		switch r.URL.Query().Get("mode") {
		case "refuse":
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "QUOTA_EXCEEDED", "message": "no", "details": map[string]any{"class": "vcpu"}}})
		case "proxy":
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html>bad gateway</html>"))
		default:
			json.NewEncoder(w).Encode(Member{Identity: "user:a@example.com", Path: "/t"})
		}
	}))
	defer srv.Close()
	c, err := New(Options{Endpoint: srv.URL + "/silo/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m, err := c.PutMember(ctx, "/t", "user:a@example.com", Mutation{RequestID: "r1", ValidateOnly: true})
	if err != nil || m.Identity != "user:a@example.com" {
		t.Fatalf("PutMember: %+v %v", m, err)
	}
	if got.Method != http.MethodPut || got.URL.Path != "/silo/v1/silos/t/members/user:a@example.com" ||
		got.URL.Query().Get("request_id") != "r1" || got.URL.Query().Get("validate_only") != "true" {
		t.Errorf("PutMember sent %s %s", got.Method, got.URL)
	}

	err = c.Do(ctx, http.MethodGet, "self", map[string][]string{"mode": {"refuse"}}, nil, nil)
	if !IsCode(err, "QUOTA_EXCEEDED") || err.(*Error).Status != 403 || err.(*Error).Details["class"] != "vcpu" {
		t.Errorf("refusal: %#v", err)
	}
	err = c.Do(ctx, http.MethodGet, "self", map[string][]string{"mode": {"proxy"}}, nil, nil)
	if !IsCode(err, "HTTP_502") {
		t.Errorf("non-API error: %#v", err)
	}
}
