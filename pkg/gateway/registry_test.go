package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/fenceapi"
)

// registry80 makes a request on port 80 from ip with Host host.
func (e *env) registry80(ip netip.Addr, host, method, path string, headers ...string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest(method, "http://"+host+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialFrom(ctx, e.web80, ip)
	}}}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Port 80 sends a request whose Host is registry.silo.internal to
// silod's registry, as the caller's silo by origin, and nothing else
// does: /v2/ under any other Host is a 404, and the registry's Host
// reaches nothing but /v2/ (local-registry.md, "The address", call
// L56). Authorization never reaches silod (call L66), except Basic
// credentials on an upstream path, the caller's own for the upstream
// (call L72).
func TestRegistryHostRouting(t *testing.T) {
	reached := make(chan string, 10)
	reg := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := fenceapi.CallerFromContext(r.Context())
		reached <- c.Silo + " " + c.Via + " " + r.URL.Path + " auth=" + r.Header.Get("Authorization")
		io.WriteString(w, "registry")
	})
	e := newEnv(t, envOptions{registry: reg})
	ip := e.ip[e.a]
	for _, host := range []string{"registry.silo.internal", "registry.silo.internal:80", "REGISTRY.silo.internal."} {
		code, body := e.registry80(ip, host, "GET", "/v2/team-a/app/tags/list", "Authorization", "Bearer x")
		if code != 200 || body != "registry" {
			t.Fatalf("%s: %d %q", host, code, body)
		}
		if got := <-reached; got != e.a+" origin /v2/team-a/app/tags/list auth=" {
			t.Fatalf("%s reached the registry as %q", host, got)
		}
	}
	for _, c := range []struct{ path, auth, want string }{
		{"/v2/docker.io/library/alpine/manifests/3", "Basic dTpw", "Basic dTpw"},
		{"/v2/library/alpine/manifests/3?ns=docker.io", "basic dTpw", "basic dTpw"},
		{"/v2/docker.io/library/alpine/manifests/3", "Bearer x", ""},
		{"/v2/team-a/app/manifests/3", "Basic dTpw", ""},
	} {
		if code, body := e.registry80(ip, "registry.silo.internal", "GET", c.path, "Authorization", c.auth); code != 200 || body != "registry" {
			t.Fatalf("%s: %d %q", c.path, code, body)
		}
		if got := <-reached; !strings.HasSuffix(got, " auth="+c.want) {
			t.Fatalf("%s with %q reached the registry as %q", c.path, c.auth, got)
		}
	}
	for _, c := range []struct{ host, path string }{
		{"silo.internal", "/v2/"},
		{"silo.internal", "/v2/team-a/app/manifests/1"},
		{"docker.silo.internal", "/v2/"},
		{"registry.silo.internal:8080", "/v2/"},
		{"registry.silo.internal", "/git/team-a/r.git/info/refs"},
		{"registry.silo.internal", "/x/web/"},
		{"registry.silo.internal", "/silo/v1/self"},
	} {
		code, body := e.registry80(ip, c.host, "GET", c.path)
		if code != 404 || strings.Contains(body, "registry\n") || body == "registry" {
			t.Errorf("%s%s: %d %q, want 404", c.host, c.path, code, body)
		}
	}
	select {
	case got := <-reached:
		t.Fatalf("the registry was reached: %s", got)
	default:
	}
}
