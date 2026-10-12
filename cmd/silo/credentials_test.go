package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/client"
)

func TestCredentialCommands(t *testing.T) {
	d := startSilod(t)
	d.stdin = "sk-ant-api03-SECRET\n"
	if out := d.ok(d.root, "credential", "add", "anthropic-api", "--provider", "anthropic", "--kind", "api_key"); !strings.Contains(out, "anthropic api_key, max_sessions 0") {
		t.Errorf("add: %s", out)
	}
	// A subscription: the login runs in a fresh home, whose files are
	// stored, and the home is removed.
	var home string
	old := loginCommand
	defer func() { loginCommand = old }()
	loginCommand = func(_ context.Context, h string, argv []string, _ io.Reader, _, _ io.Writer) error {
		home = h
		if strings.Join(argv, " ") != "claude setup-token" {
			t.Errorf("login ran %v", argv)
		}
		os.MkdirAll(filepath.Join(h, ".claude"), 0o700)
		return os.WriteFile(filepath.Join(h, ".claude", ".credentials.json"), []byte(`{"token":"LOGIN"}`), 0o600)
	}
	if out := d.ok(d.root, "credential", "login", "claude-max-bob", "--provider", "anthropic"); !strings.Contains(out, "subscription, max_sessions 2") {
		t.Errorf("login: %s", out)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the login's home %s is still there: %v", home, err)
	}
	var l struct{ Credentials []client.Credential }
	d.json(d.root, &l, "credential", "list")
	if len(l.Credentials) != 2 || l.Credentials[1].Name != "claude-max-bob" {
		t.Errorf("list: %+v", l)
	}
	if out := d.ok(d.root, "credential", "list"); strings.Contains(out, "SECRET") || strings.Contains(out, "LOGIN") {
		t.Errorf("list shows a secret: %s", out)
	}
	d.ok(d.root, "credential", "remove", "claude-max-bob")
	d.stdin = ""
	if r := d.silo(d.root, "credential", "add", "x", "--provider", "anthropic"); r.code != 2 {
		t.Errorf("add with no key: %+v", r)
	}
}
