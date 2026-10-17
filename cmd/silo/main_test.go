package main

import (
	"net"
	"strings"
	"testing"
)

// TestServeRefusesDevSignOnOnKubernetes checks that `silo hub serve`
// will not sign humans in from the development user header in a
// Kubernetes pod, where the hub runs in production, whether the header
// is named by flag or by environment variable; and that it refuses
// before it listens.
func TestServeRefusesDevSignOnOnKubernetes(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	for name, args := range map[string][]string{
		"flag": {"-addr", addr, "-database", "sqlite::memory:", "-dev-user-header", "X-User"},
		"env":  {"-addr", addr, "-database", "sqlite::memory:"},
	} {
		if name == "env" {
			t.Setenv("SILO_HUB_DEV_USER_HEADER", "X-User")
		}
		err := serve(args)
		if err == nil || !strings.Contains(err.Error(), "refused on Kubernetes") {
			t.Errorf("%s: serve = %v, want the dev sign-on refused", name, err)
		}
		// Nothing listened.
		l, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			t.Errorf("%s: %s taken after the refusal: %v", name, addr, lerr)
		} else {
			l.Close()
		}
	}
}
