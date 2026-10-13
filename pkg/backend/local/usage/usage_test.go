package usage_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/usage"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSliceDir(t *testing.T) {
	m := usage.ManagerDir(1000)
	if m != "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service" {
		t.Errorf("ManagerDir: %s", m)
	}
	if got := usage.SliceDir("/m", "r00t", "kid"); got != "/m/silo.slice/silo-r00t.slice/silo-r00t-kid.slice" {
		t.Errorf("SliceDir: %s", got)
	}
}

func TestMeter(t *testing.T) {
	ctx := context.Background()
	manager := t.TempDir()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	write(t, usage.SliceDir(manager, "r"), map[string]string{"cpu.stat": "usage_usec 9000\nuser_usec 5000\n", "memory.current": "4096\n"})
	write(t, usage.SliceDir(manager, "r", "a"), map[string]string{
		"cpu.stat":       "usage_usec 1500\nuser_usec 1000\nsystem_usec 500\n",
		"memory.current": "1048576\n", "memory.peak": "2097152\n",
		"io.stat": "8:0 rbytes=100 wbytes=200 rios=1 wios=2 dbytes=0 dios=0\n259:0 rbytes=10 wbytes=20 rios=1 wios=1 dbytes=0 dios=0\n",
	})
	agentDir := filepath.Join(manager, "app.slice", "silo-agent-x1.scope")
	write(t, agentDir, map[string]string{"cpu.stat": "usage_usec 300\n", "memory.current": "100\n"})
	silos := map[string][]string{"r": {"r"}, "a": {"r", "a"}, "b": {"r", "b"}}
	m := &usage.Meter{Manager: manager, Now: func() time.Time { return now },
		Silos: func(context.Context) (map[string][]string, error) { return silos, nil },
		Agents: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"x1": {agentDir, filepath.Join(manager, "gone")}}, nil
		}}
	if err := m.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	a, ok := m.Silo("a")
	want := usage.Sample{SampledAt: now, CPUUsec: 1500, MemoryBytes: 1 << 20, MemoryPeakBytes: 2 << 20, ReadBytes: 110, WriteBytes: 220}
	if !ok || a != want {
		t.Errorf("a: %+v %v, want %+v", a, ok, want)
	}
	if r, ok := m.Silo("r"); !ok || r.CPUUsec != 9000 || r.MemoryBytes != 4096 || r.ReadBytes != 0 {
		t.Errorf("r, without io or memory.peak: %+v %v", r, ok)
	}
	if _, ok := m.Silo("b"); ok {
		t.Error("a silo with no slice has a reading")
	}
	if x, ok := m.Agent("x1"); !ok || x.CPUUsec != 300 || x.MemoryBytes != 100 {
		t.Errorf("agent: %+v %v", x, ok)
	}
	// A deleted silo's reading goes.
	delete(silos, "a")
	if err := m.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Silo("a"); ok {
		t.Error("a deleted silo kept its reading")
	}
	// An agent between turns, its scope gone, keeps its last.
	os.RemoveAll(agentDir)
	if err := m.Sample(ctx); err != nil {
		t.Fatal(err)
	}
	if x, ok := m.Agent("x1"); !ok || x.CPUUsec != 300 {
		t.Errorf("agent between turns: %+v %v", x, ok)
	}
	found, err := usage.HarnessScopes(manager)(ctx)
	if err != nil || len(found) != 0 {
		t.Errorf("HarnessScopes with none: %v %v", found, err)
	}
	write(t, agentDir, map[string]string{"cpu.stat": "usage_usec 1\n"})
	if found, _ := usage.HarnessScopes(manager)(ctx); len(found["x1"]) != 1 || found["x1"][0] != agentDir {
		t.Errorf("HarnessScopes: %v", found)
	}
}
