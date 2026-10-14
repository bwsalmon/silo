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
