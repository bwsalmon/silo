package silofile

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const minimal = "silo: v2\nname: t\nagent:\n  cli: claude\n"

func TestParseStrict(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		wantErr    string // substring; empty means it decodes
		wantLine   int
	}{
		{name: "minimal", yaml: minimal},
		{name: "missing silo", yaml: "name: t\nagent: {cli: claude}\n", wantErr: "the first key must be silo: v2", wantLine: 1},
		{name: "silo not first", yaml: "name: t\nsilo: v2\nagent: {cli: claude}\n", wantErr: "silo: v2 must be the first key", wantLine: 1},
		{name: "wrong version", yaml: "silo: v3\nname: t\n", wantErr: `schema version "v3"`, wantLine: 1},
		{name: "empty", yaml: "", wantErr: "empty file"},
		{name: "two documents", yaml: minimal + "---\nsilo: v2\n", wantErr: "one YAML document"},
		{name: "unknown key", yaml: minimal + "remote_permision: none\n", wantErr: `unknown key "remote_permision"`, wantLine: 5},
		{name: "unknown nested key", yaml: minimal + "  modle: x\n", wantErr: `agent.modle: unknown key`, wantLine: 5},
		{name: "repeated key", yaml: minimal + "name: u\n", wantErr: `key "name" is repeated`, wantLine: 5},
		{name: "merge key", yaml: "silo: v2\nname: t\nbase: &b {cli: claude}\nagent:\n  <<: *b\n", wantErr: "unknown key \"base\""},
		{name: "merge key alone", yaml: "silo: v2\nname: t\nagent:\n  <<: {cli: claude}\n  cli: claude\n", wantErr: "merge keys (<<) are refused", wantLine: 4},
		{name: "custom tag", yaml: minimal + "description: !secret x\n", wantErr: "tags are refused (!secret)", wantLine: 5},
		{name: "core tag", yaml: minimal + "description: !!str x\n", wantErr: "tags are refused (!!str)", wantLine: 5},
		{name: "anchor and alias", yaml: "silo: v2\nname: t\nagent:\n  cli: &c claude\n  model: *c\n"},
		{name: "bool wants true or false", yaml: minimal + "sandboxes:\n  - {name: a, kind: container, image: x, mounts: [{from: ., to: /x, rw: yes}]}\n", wantErr: `want true or false, found "yes"`},
		{name: "list where a word goes", yaml: minimal + "location: [local]\n", wantErr: "want a mapping, found a list"},
		{name: "null value", yaml: minimal + "description:\n", wantErr: "description: has no value"},
		{name: "bad duration", yaml: minimal + "  limits: {deadline: 2 hours}\n", wantErr: `"2 hours" is not a duration`},
		{name: "bad size", yaml: minimal + "sandboxes:\n  - {name: a, kind: container, image: x, memory: 4 gigs}\n", wantErr: `"4 gigs" is not a size`},
		{name: "remote_permissions word", yaml: minimal + "remote_permissions: some\n", wantErr: `"some" is not one of none, all`},
		{name: "missing required", yaml: "silo: v2\nname: t\n", wantErr: `missing required key "agent"`},
		{name: "syntax error", yaml: "silo: v2\nname: [\n", wantErr: "Silofile:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse([]byte(tc.yaml), "Silofile")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if f.Agent.CLI != "claude" {
					t.Errorf("agent.cli = %q", f.Agent.CLI)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.wantErr)
			}
			var errs Errors
			if !errors.As(err, &errs) {
				t.Fatalf("error is %T, want Errors", err)
			}
			if tc.wantLine != 0 && errs[0].Line != tc.wantLine {
				t.Errorf("line = %d, want %d (%v)", errs[0].Line, tc.wantLine, err)
			}
		})
	}
}

func TestParseKeepsStrings(t *testing.T) {
	f, err := Parse([]byte(minimal+`  model: no
  effort: on
  workdir: 0700
sandboxes:
  - name: a
    kind: container
    image: "x"
    env: {A: no, B: 0x10, C: 1e3, D: ~x, E: 2026-10-14, F: true}
`), "Silofile")
	if err != nil {
		t.Fatal(err)
	}
	if f.Agent.Model != "no" || f.Agent.Effort != "on" || f.Agent.Workdir != "0700" {
		t.Errorf("agent = %+v", f.Agent)
	}
	want := map[string]string{"A": "no", "B": "0x10", "C": "1e3", "D": "~x", "E": "2026-10-14", "F": "true"}
	for k, v := range want {
		if got := f.Sandboxes[0].Env[k]; got != v {
			t.Errorf("env %s = %q, want %q", k, got, v)
		}
	}
}

func TestParseShapes(t *testing.T) {
	f, err := Parse([]byte(minimal+`  auth: {credential: key}
  limits: {deadline: 1h30m, max_turns: 20}
remote_permissions: [Read, Grep]
sandboxes:
  - {name: a, kind: container, image: x, memory: 4Gi, disk: 10GB, cpus: 0.5, network: open, backend: local}
mcp:
  - {name: m, command: [x], inherit_env: all}
  - {name: n, command: [x], inherit_env: [HOME]}
local: {timeout: 10m, output_cap: 1MiB}
`), "Silofile")
	if err != nil {
		t.Fatal(err)
	}
	if f.Agent.Auth.Credential != "key" || f.Agent.Auth.Ambient() {
		t.Errorf("auth = %+v", f.Agent.Auth)
	}
	if got := time.Duration(*f.Agent.Limits.Deadline); got != 90*time.Minute {
		t.Errorf("deadline = %v", got)
	}
	if f.RemotePermissions.Mode != PermSubset || len(f.RemotePermissions.Tools) != 2 {
		t.Errorf("remote_permissions = %+v", f.RemotePermissions)
	}
	s := f.Sandboxes[0]
	if *s.Memory != 4<<30 || *s.Disk != 10e9 || s.CPUs != 0.5 || s.Network.Mode != "open" {
		t.Errorf("sandbox = %+v", s)
	}
	if !f.MCP[0].InheritEnv.All || f.MCP[1].InheritEnv.Names[0] != "HOME" {
		t.Errorf("inherit_env = %+v, %+v", f.MCP[0].InheritEnv, f.MCP[1].InheritEnv)
	}
	if *f.Local.OutputCap != 1<<20 || time.Duration(*f.Local.Timeout) != 10*time.Minute {
		t.Errorf("local = %+v", f.Local)
	}
	if p := f.Pos("sandboxes[0].memory"); p.Line != 9 {
		t.Errorf("Pos(sandboxes[0].memory) = %+v, want line 9", p)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0": 0, "7d": 7 * 24 * time.Hour, "5m": 5 * time.Minute, "1h30m": 90 * time.Minute, "2w": 14 * 24 * time.Hour, "45s": 45 * time.Second,
	} {
		got, err := ParseDuration(in)
		if err != nil || time.Duration(got) != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, time.Duration(got), err, want)
		}
	}
	for _, in := range []string{"", "d", "7", "7y", "1.5h", "-1m", "5 m", "99999999999999999999d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded", in)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]Size{
		"0": 0, "512": 512, "512B": 512, "1MiB": 1 << 20, "4Gi": 4 << 30, "10GB": 10e9, "2K": 2000, "1Ti": 1 << 40,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Gi", "4gi", "1.5Gi", "4 Gi", "4Gib", "-1", "99999999999Ti"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) succeeded", in)
		}
	}
}
