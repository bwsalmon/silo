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

// TestExampleContextRepo loads examples/context-repo's tool sets -- one
// in Python, one in Go that is built first -- and calls them.
func TestExampleContextRepo(t *testing.T) {
	for _, bin := range []string{"python3", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	checkout := t.TempDir()
	if err := os.CopyFS(checkout, os.DirFS("../../examples/context-repo")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rt := New(approval.NewFake(), audit.Discard{})
	defer rt.Close()
	h := &Host{Router: rt, Checkout: checkout, SessionDir: t.TempDir(), Log: audit.Discard{}}
	report, ok := h.LoadAll(ctx)
	if !ok {
		t.Fatalf("load failed:\n%s", report)
	}
	if want := "loaded clock (tools: clock.now)\nloaded runbooks (tools: runbooks.list, runbooks.search)"; report != want {
		t.Errorf("report = %q, want %q", report, want)
	}
	res := rt.Call(ctx, "runbooks.search", json.RawMessage(`{"query":"idempotency"}`))
	if res.IsError || !strings.Contains(res.AllText(), filepath.Join("runbooks", "psp-gateway.md")+":6 [Elevated PSP timeouts]") {
		t.Errorf("runbooks.search = %q", res.AllText())
	}
	res = rt.Call(ctx, "clock.now", json.RawMessage(`{"zone":"UTC"}`))
	if res.IsError || !strings.Contains(res.AllText(), "UTC") {
		t.Errorf("clock.now = %q", res.AllText())
	}
	if _, err := os.Stat(filepath.Join(checkout, ".silo", "tools", "clock", "clock")); !os.IsNotExist(err) {
		t.Error("build output landed in the checkout")
	}
}
