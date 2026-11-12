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
build: [go, build, -o, "${BUILD_DIR}/server", .]
run: ["${BUILD_DIR}/server"]
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
	if m.Run[0] != "${BUILD_DIR}/server" || len(m.Tools) != 2 || m.RequiresApproval[0] != "create" || m.PassEnv[0] != "TICKETS_TOKEN" {
		t.Errorf("parsed: %+v", m)
	}
}

func TestParseManifestErrors(t *testing.T) {
	for name, tc := range map[string]struct{ doc, dir string }{
		"name mismatch":     {"name: a\nrun: [x]\n", "b"},
		"bad name":          {"name: A.B\nrun: [x]\n", ""},
		"no run":            {"name: a\n", "a"},
		"unknown field":     {"name: a\nrun: [x]\ndefault: enabled\n", "a"},
		"approval unlisted": {"name: a\nrun: [x]\ntools: [{name: t}]\nrequires_approval: [u]\n", "a"},
		"bad pass_env":      {"name: a\nrun: [x]\npass_env: [A=B]\n", "a"},
	} {
		if _, err := ParseManifest([]byte(tc.doc), tc.dir); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRepoToolsets(t *testing.T) {
	dir := t.TempDir()
	if names, err := RepoToolsets(dir); err != nil || names != nil {
		t.Fatalf("no tools dir: %v %v", names, err)
	}
	for _, d := range []string{"b", "a"} {
		os.MkdirAll(filepath.Join(dir, ToolsDir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, ToolsDir, "README.md"), nil, 0o644)
	names, _ := RepoToolsets(dir)
	if strings.Join(names, ",") != "a,b" {
		t.Errorf("names = %v", names)
	}
}
