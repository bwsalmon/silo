package main

import (
	"net"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/hub/server"
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

// TestServeRefusesSQLiteOutsideDev checks that `silo hub serve` will
// not run on SQLite unless told it is local development (-dev,
// SILO_HUB_DEV or the development user header), and never in a
// Kubernetes pod, where the production store is PostgreSQL on Cloud
// SQL; and that it refuses before it listens.
func TestServeRefusesSQLiteOutsideDev(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	for _, c := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no dev flag", []string{"-database", "sqlite::memory:"}, nil, "local development and tests only"},
		{"bare path", []string{"-database", "/tmp/hub.db"}, nil, "local development and tests only"},
		{"dev env false", []string{"-database", "sqlite::memory:"}, map[string]string{"SILO_HUB_DEV": "false"}, "local development and tests only"},
		{"dev flag on Kubernetes", []string{"-database", "sqlite::memory:", "-dev"}, map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"}, "refused on Kubernetes"},
		{"dev env on Kubernetes", []string{"-database", "sqlite::memory:"}, map[string]string{"SILO_HUB_DEV": "1", "KUBERNETES_SERVICE_HOST": "10.0.0.1"}, "refused on Kubernetes"},
		{"env database on Kubernetes", nil, map[string]string{"SILO_HUB_DATABASE": "sqlite:/data/hub.db", "SILO_HUB_DEV": "true", "KUBERNETES_SERVICE_HOST": "10.0.0.1"}, "refused on Kubernetes"},
		{"no database", nil, nil, "-database is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			err := serve(append([]string{"-addr", addr}, c.args...))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("serve = %v, want an error containing %q", err, c.want)
			}
			l, lerr := net.Listen("tcp", addr)
			if lerr != nil {
				t.Errorf("%s taken after the refusal: %v", addr, lerr)
			} else {
				l.Close()
			}
		})
	}
}

// TestCheckDatabaseAllowsDev checks the cases the hub does start in:
// PostgreSQL anywhere, and SQLite only in local development.
func TestCheckDatabaseAllowsDev(t *testing.T) {
	none := func(string) string { return "" }
	k8s := func(k string) string {
		if k == "KUBERNETES_SERVICE_HOST" {
			return "10.0.0.1"
		}
		return ""
	}
	for _, c := range []struct {
		db     string
		dev    bool
		getenv func(string) string
	}{
		{"postgres://silo@127.0.0.1:5432/silo", false, k8s},
		{"postgresql://silo@127.0.0.1:5432/silo", false, none},
		{"sqlite::memory:", true, none},
		{"sqlite:/tmp/hub.db", true, none},
	} {
		if err := server.CheckDatabase(c.db, c.dev, c.getenv); err != nil {
			t.Errorf("CheckDatabase(%q, %v) = %v, want nil", c.db, c.dev, err)
		}
	}
}
