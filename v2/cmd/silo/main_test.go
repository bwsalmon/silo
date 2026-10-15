package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/schema.golden.json")

func TestSchemaGolden(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"schema"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	golden := filepath.Join("testdata", "schema.golden.json")
	if *update {
		if err := os.WriteFile(golden, stdout.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("silo schema differs from %s; if the change is intended, run go test ./cmd/silo -update", golden)
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good/Silofile", "silo: v2\nname: good\nagent: {cli: claude}\n")
	bad := write("bad/Silofile", "silo: v2\nname: bad\nagent:\n  cli: agy\n")
	strict := write("strict.Silofile", "silo: v2\nname: s\nagent: {cli: claude}\nremote_permision: none\n")

	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		out, err string
	}{
		{name: "no command", args: nil, code: 64, err: "usage"},
		{name: "unknown command", args: []string{"frob"}, code: 64, err: `unknown command "frob"`},
		{name: "bad flag", args: []string{"validate", "--frob", good}, code: 64, err: "flag provided but not defined"},
		{name: "validate no args", args: []string{"validate"}, code: 64, err: "usage: silo validate"},
		{name: "validate dir", args: []string{"validate", filepath.Dir(good)}, code: 0, out: "valid"},
		{name: "validate file", args: []string{"validate", good}, code: 0, out: "valid"},
		{name: "validate invalid", args: []string{"validate", filepath.Dir(bad)}, code: 65, err: "bad/Silofile:4:8: remote_permissions: agy cannot express none"},
		{name: "validate strict", args: []string{"validate", strict}, code: 65, err: `strict.Silofile:4:1: remote_permision: unknown key`},
		{name: "validate missing", args: []string{"validate", filepath.Join(dir, "nope")}, code: 65, err: "no such file"},
		{name: "schema section", args: []string{"schema", "agent"}, code: 0, out: `"cli"`},
		{name: "schema unknown section", args: []string{"schema", "nope"}, code: 64, err: `no section "nope"`},
		{name: "schema two args", args: []string{"schema", "a", "b"}, code: 64, err: "usage: silo schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			if code != tc.code {
				t.Errorf("exit %d, want %d; stderr: %s", code, tc.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.out) {
				t.Errorf("stdout = %q, want %q", stdout.String(), tc.out)
			}
			if !strings.Contains(stderr.String(), tc.err) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.err)
			}
		})
	}
}

func TestRunUsageAndInvalid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("no Silofile: exit %d", code)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Silofile"), []byte("silo: v2\nname: x\nagent: {cli: claude, mode: attached}\n"), 0o644)
	stderr.Reset()
	if code := run([]string{"run", dir}, &stdout, &stderr); code != exitInvalid || !strings.Contains(stderr.String(), "attached") {
		t.Fatalf("attached: exit %d: %s", code, stderr.String())
	}
}

func TestRunDryRun(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "mcp"), 0o755)
	os.WriteFile(filepath.Join(dir, "mcp", "servers.json"), []byte(`{"mcpServers": {"docs": {"command": "sh"}, "web": {"url": "http://127.0.0.1:1/mcp"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "Silofile"), []byte(`silo: v2
name: x
agent: {cli: claude}
mcp:
  - name: docs
    from: mcp-json:docs
    tools: [search]
  - name: web
    from: mcp-json:web
  - name: gone
    from: mcp-json:gone
`), 0o644)
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--dry-run", dir}, &stdout, &stderr)
	out := stdout.String()
	for _, want := range []string{
		"ok    silofile: ",
		"ok    mcp docs: mcp-json:docs in " + filepath.Join(dir, "mcp", "servers.json") + ": launches sh; runs as ",
		"with access to their files",
		"ok    mcp web: mcp-json:web in ",
		"connects to http://127.0.0.1:1/mcp, which silo neither starts nor stops",
		"FAIL  mcp gone: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if code != 69 {
		t.Errorf("exit %d, want 69 for the failed check; stderr: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".silo")); err == nil {
		t.Error("--dry-run wrote .silo/")
	}
}

func TestCallerMCPFlags(t *testing.T) {
	env := map[string]string{"SILO_CALLER_MCP_TOKEN_GRAIN_TASK": "tok"}
	c := &callerFlags{getenv: func(k string) string { return env[k] }}
	for _, v := range []string{"grain-task=unix:/run/g.sock", "web=http://127.0.0.1:1/mcp"} {
		if err := c.server(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.server("web=http://x"); err == nil {
		t.Error("a server given twice was accepted")
	}
	c.tools("grain-task=ask_question,wait_for_checks")
	c.blocking("grain-task=wait_for_checks")
	got, err := c.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Token != "tok" || strings.Join(got[0].Tools, ",") != "ask_question,wait_for_checks" ||
		strings.Join(got[0].Blocking, ",") != "wait_for_checks" || got[1].Tools != nil || got[1].Token != "" {
		t.Errorf("%+v", got)
	}
	c.blocking("nobody=x")
	if _, err := c.list(); err == nil {
		t.Error("a qualifier for an unknown server was accepted")
	}
	if err := c.server("novalue"); err == nil {
		t.Error("a server without a URL was accepted")
	}
}

// TestValidateChecksGranulefile: silo validate runs granule granulefile
// check (here a fake granule, as SILO_GRANULE) on a granule sandbox's
// Granulefile, and refuses one whose target builds a container image.
func TestValidateChecksGranulefile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "silo", "sandboxes"), 0o755)
	os.WriteFile(filepath.Join(dir, "silo", "sandboxes", "dev.Granulefile"), []byte("FROM busybox\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "silo", "Silofile"), []byte("silo: v2\nname: g\nagent: {cli: claude}\nsandboxes:\n  - {name: dev, kind: granule, granulefile: sandboxes/dev.Granulefile}\n"), 0o644)
	fake := filepath.Join(dir, "granule")
	for _, tc := range []struct {
		name, plan string
		code       int
		err        string
	}{
		{name: "vm", plan: "target: stage 0, which produces a granule VM image", code: 0},
		{name: "container", plan: "target: stage 0, which produces an OCI container image", code: exitInvalid, err: "sandboxes[0].granulefile: its target stage builds a container image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$1 $2\" = \"granulefile check\" ] || exit 2\necho '"+tc.plan+"'\n"), 0o755)
			t.Setenv("SILO_GRANULE", fake)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"validate", filepath.Join(dir, "silo")}, &stdout, &stderr); code != tc.code || !strings.Contains(stderr.String(), tc.err) {
				t.Errorf("exit %d, stderr %q; want %d, %q", code, stderr.String(), tc.code, tc.err)
			}
		})
	}
	t.Run("no granule", func(t *testing.T) {
		t.Setenv("SILO_GRANULE", filepath.Join(dir, "missing"))
		var stdout, stderr bytes.Buffer
		if code := run([]string{"validate", filepath.Join(dir, "silo")}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "warning:") {
			t.Errorf("exit %d, stderr %q; want valid with a warning", code, stderr.String())
		}
	})
}
