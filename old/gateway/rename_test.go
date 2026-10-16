package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/gateway/audit"
	"github.com/bwsalmon/silo/old/mcp"
)

func sleeper(name string, d, timeout time.Duration, blocking bool) Tool {
	return Tool{Name: name, Timeout: timeout, Blocking: blocking, Handler: func(ctx context.Context, _ map[string]any) (mcp.Result, error) {
		select {
		case <-time.After(d):
			return mcp.Result{Text: "done"}, nil
		case <-ctx.Done():
			return mcp.Result{}, ctx.Err()
		}
	}}
}

func TestBlockingToolHasNoTimeout(t *testing.T) {
	g := New(nil)
	g.Register(sleeper("plain", 300*time.Millisecond, 50*time.Millisecond, false), sleeper("blocks", 300*time.Millisecond, 50*time.Millisecond, true))
	if res, _ := g.CallTool(context.Background(), "plain", nil); !res.IsError {
		t.Errorf("plain outlived its timeout: %+v", res)
	}
	if res, _ := g.CallTool(context.Background(), "blocks", nil); res.IsError || res.Text != "done" {
		t.Errorf("blocks: %+v", res)
	}
}

func TestRename(t *testing.T) {
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit.jsonl"), "s-1", audit.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var recs []audit.Record
	log.Observe(func(r audit.Record) { recs = append(recs, r) })
	g := New(log)
	g.Register(sleeper("sandbox_exec", 0, 0, false), sleeper("other", 0, 0, false))
	if err := g.SetRename(map[string]string{"sandbox_exec": "run_command"}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ti := range g.ListTools() {
		names = append(names, ti.Name)
	}
	if len(names) != 2 || names[0] != "run_command" || names[1] != "other" {
		t.Errorf("listed %v", names)
	}
	if res, err := g.CallTool(context.Background(), "run_command", nil); err != nil || res.Text != "done" {
		t.Errorf("run_command: %+v %v", res, err)
	}
	var me *mcp.Error
	if _, err := g.CallTool(context.Background(), "sandbox_exec", nil); !errors.As(err, &me) {
		t.Errorf("a renamed tool answered to its own name: %v", err)
	}
	if len(recs) < 2 || recs[0].Tool != "sandbox_exec" || recs[0].As != "run_command" || recs[1].As != "run_command" {
		t.Errorf("audit: %+v", recs)
	}
	for _, m := range []map[string]string{
		{"sandbox_exec": "x", "other": "x"},
		{"sandbox_exec": "other"},
		{"sandbox_exec": ""},
	} {
		if err := g.SetRename(m); err == nil {
			t.Errorf("%v was accepted", m)
		}
	}
	// A swap is fine.
	if err := g.SetRename(map[string]string{"sandbox_exec": "other", "other": "sandbox_exec"}); err != nil {
		t.Errorf("a swap: %v", err)
	}
}
