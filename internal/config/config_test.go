package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
cli:
  name: claude
  args: [--model, opus]
  env: {FOO: bar}
repos:
  root: /srv/context
  git_base: "git@github.com:"
identity: {name: Ada, email: ada@example.com}
toolsets:
  disabled: [slow]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CLI.Binary != "claude" || cfg.CLI.ListChanged != "auto" || cfg.Push.Remote != "origin" {
		t.Errorf("defaults not filled: %+v", cfg)
	}
	if got := cfg.RepoURL("team/payments-context"); got != "git@github.com:team/payments-context" {
		t.Errorf("RepoURL = %q", got)
	}
	cfg.Repos.GitBase = "https://example.com/git"
	if got := cfg.RepoURL("a/b"); got != "https://example.com/git/a/b" {
		t.Errorf("RepoURL = %q", got)
	}
	if !strings.HasSuffix(cfg.SessionsDir, filepath.Join(".silo", "sessions")) {
		t.Errorf("SessionsDir = %q", cfg.SessionsDir)
	}
	if cfg.Toolsets.Disabled[0] != "slow" {
		t.Errorf("disabled = %v", cfg.Toolsets.Disabled)
	}
}

func TestParseConfigErrors(t *testing.T) {
	base := "repos: {root: /r}\nidentity: {name: a, email: a@b}\n"
	for name, doc := range map[string]string{
		"no cli":           base,
		"bad cli":          "cli: {name: vim}\n" + base,
		"no root":          "cli: {name: codex}\nidentity: {name: a, email: a@b}\n",
		"no identity":      "cli: {name: codex}\nrepos: {root: /r}\n",
		"unknown field":    "cli: {name: codex, colour: red}\n" + base,
		"bad list_changed": "cli: {name: codex, list_changed: maybe}\n" + base,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := ExpandHome("~/x"); got != filepath.Join(home, "x") {
		t.Errorf("ExpandHome = %q", got)
	}
	if got := ExpandHome("/abs"); got != "/abs" {
		t.Errorf("ExpandHome = %q", got)
	}
}

func TestValidateRepoName(t *testing.T) {
	for _, ok := range []string{"a", "team/payments-context", "x.y/z_1"} {
		if err := ValidateRepoName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "a/../b", "..", "a//b", "a/", "a b"} {
		if err := ValidateRepoName(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestParseRepoConfig(t *testing.T) {
	rc, err := ParseRepoConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Context.Mode != "inline" || rc.Context.MaxInlineBytes != 65536 {
		t.Errorf("defaults: %+v", rc)
	}
	rc, err = ParseRepoConfig([]byte("push_branch: ctx-updates\ndisabled: [a]\ncontext: {mode: list}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rc.PushBranch != "ctx-updates" || rc.Context.Mode != "list" || rc.Disabled[0] != "a" {
		t.Errorf("parsed: %+v", rc)
	}
	if _, err := ParseRepoConfig([]byte("context: {mode: everything}\n")); err == nil {
		t.Error("bad mode accepted")
	}
}

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte(`
name: tickets
description: Search and file tickets.
env: {TICKETS_URL: "https://tickets.example.com"}
pass_env: [TICKETS_TOKEN]
tools:
  - name: search
  - name: create
    description: File a ticket.
requires_approval: [create]
`), "tickets")
	if err != nil {
		t.Fatal(err)
	}
	if m.Description != "Search and file tickets." || len(m.Tools) != 2 || m.RequiresApproval[0] != "create" || m.PassEnv[0] != "TICKETS_TOKEN" {
		t.Errorf("parsed: %+v", m)
	}
	if m, err := ParseManifest(nil, "anything"); err != nil || m.Name != "" {
		t.Errorf("empty manifest: %+v, %v", m, err)
	}
}

func TestParseManifestErrors(t *testing.T) {
	for name, tc := range map[string]struct{ doc, dir string }{
		"name mismatch":     {"name: a\n", "b"},
		"bad name":          {"name: A.B\n", ""},
		"unknown field":     {"name: a\ndefault: enabled\n", "a"},
		"approval unlisted": {"tools: [{name: t}]\nrequires_approval: [u]\n", "a"},
		"bad pass_env":      {"pass_env: [A=B]\n", "a"},
	} {
		if _, err := ParseManifest([]byte(tc.doc), tc.dir); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// The retired launch settings get an error that says what replaced them.
	for _, doc := range []string{"name: a\nrun: [x]\n", "build: [make]\n"} {
		_, err := ParseManifest([]byte(doc), "a")
		if err == nil || !strings.Contains(err.Error(), "start.sh or start.py") {
			t.Errorf("%q: err = %v", doc, err)
		}
	}
}

func TestEntrypoint(t *testing.T) {
	for _, tc := range []struct {
		files []string
		want  string
		err   string
	}{
		{[]string{"start.sh", "README.md"}, "start.sh", ""},
		{[]string{"start.py"}, "start.py", ""},
		{[]string{"start.sh", "start.py"}, "", "both"},
		{[]string{"server.py", "toolset.yaml"}, "", "has no start.sh or start.py"},
	} {
		dir := t.TempDir()
		for _, f := range tc.files {
			os.WriteFile(filepath.Join(dir, f), nil, 0o644)
		}
		got, err := DirEntrypoint(dir)
		if got != tc.want || (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%v: got %q, %v", tc.files, got, err)
		}
	}
	if Interpreter(StartPy) != "python3" || Interpreter(StartSh) != "bash" {
		t.Error("interpreters")
	}
}

func TestRepoToolsets(t *testing.T) {
	dir := t.TempDir()
	if names, err := RepoToolsets(dir); err != nil || names != nil {
		t.Fatalf("no tools dir: %v %v", names, err)
	}
	for _, d := range []string{"b", "a", "vendor/acme/tickets", "vendor/acme/clock", "vendor/other/x"} {
		os.MkdirAll(filepath.Join(dir, ToolsDir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, ToolsDir, "README.md"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, VendorDir, "acme", VendorLock), nil, 0o644)
	keys, _ := RepoToolsets(dir)
	if got := strings.Join(keys, ","); got != "a,b,vendor/acme/clock,vendor/acme/tickets,vendor/other/x" {
		t.Errorf("keys = %v", got)
	}
	var names []string
	for _, k := range keys {
		names = append(names, ToolsetName(k))
	}
	if got := strings.Join(names, ","); got != "a,b,acme.clock,acme.tickets,other.x" {
		t.Errorf("names = %v", got)
	}
	for _, bad := range []string{"vendor", "vendor/A/x", "vendor/a", "a.b"} {
		if ValidToolsetKey(bad) == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}
