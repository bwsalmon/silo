package router

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/audit"
)

// TestExampleContextRepo loads examples/context-repo's tool sets -- a
// start.sh that builds and runs a Go server, and a start.py on the
// official Python MCP SDK -- and calls them. The Python one needs the
// SDK importable by python3 (its own bootstrap would install it from
// the network), so without it only the Go one is tried.
func TestExampleContextRepo(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	withPython := exec.Command("python3", "-c", "import mcp").Run() == nil
	checkout := t.TempDir()
	if err := os.CopyFS(checkout, os.DirFS("../../examples/context-repo")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rt := New(approval.NewFake(), audit.Discard{})
	defer rt.Close()
	h := &Host{Router: rt, Checkout: checkout, SessionDir: t.TempDir(), Log: audit.Discard{}}
	want := "loaded clock (tools: clock.now)\nloaded runbooks (tools: runbooks.list, runbooks.search)"
	if !withPython {
		t.Log("python3 cannot import the MCP SDK; skipping the runbooks tool set")
		h.Disabled = map[string]bool{"runbooks": true}
		want = "loaded clock (tools: clock.now)\nskipped runbooks: disabled by config"
	}
	report, ok := h.LoadAll(ctx)
	if !ok {
		t.Fatalf("load failed:\n%s", report)
	}
	if report != want {
		t.Errorf("report = %q, want %q", report, want)
	}
	res := rt.Call(ctx, "clock.now", json.RawMessage(`{"zone":"UTC"}`))
	if res.IsError || !strings.Contains(res.AllText(), "UTC") {
		t.Errorf("clock.now = %q", res.AllText())
	}
	if _, err := os.Stat(filepath.Join(checkout, ".silo", "tools", "clock", "clock")); !os.IsNotExist(err) {
		t.Error("build output landed in the checkout")
	}
	if withPython {
		res = rt.Call(ctx, "runbooks.search", json.RawMessage(`{"query":"idempotency"}`))
		if res.IsError || !strings.Contains(res.AllText(), filepath.Join("runbooks", "psp-gateway.md")+":6 [Elevated PSP timeouts]") {
			t.Errorf("runbooks.search = %q", res.AllText())
		}
	}
}
