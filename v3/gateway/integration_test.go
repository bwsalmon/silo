//go:build integration

// The integration test really spawns and stops agents on this host. It needs
// root and systemd, and it creates (and removes) the users silo-a-itest*:
//
//	cd v3 && go test -c -tags integration -o /var/tmp/gw.test ./gateway &&
//	  sudo /var/tmp/gw.test -test.v -test.run Integration
package gateway

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntegrationSpawnStop(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("needs systemd as init")
	}
	ctx := context.Background()
	dir, err := os.MkdirTemp("/var/tmp", "silo-itest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	os.Chmod(dir, 0o755)
	off := false
	cfg := &Config{
		StateDir:  filepath.Join(dir, "state"),
		HomeRoot:  filepath.Join(dir, "home"),
		AuditLog:  filepath.Join(dir, "log", "audit.jsonl"),
		CLIsDir:   filepath.Join(dir, "clis.d"),
		RootAgent: RootAgentConfig{Enabled: &off},
	}
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.CLIsDir, 0o755)
	// The agent writes its id into its home (proving its home is
	// writable), then sleeps.
	os.WriteFile(filepath.Join(cfg.CLIsDir, "sleep.toml"), []byte(`command = ["/bin/sh", "-c", "id -un > \"$HOME/whoami\"; test ! -e /var/tmp/`+filepath.Base(dir)+`/state && echo hidden >> \"$HOME/whoami\"; exec sleep 600"]`), 0o644)

	users := []string{"silo-a-itest", "silo-a-itest-kid"}
	cleanupUsers := func() {
		for _, u := range users {
			exec.Command("systemctl", "stop", "silo-agent-"+strings.TrimPrefix(u, "silo-a-")+".service").Run()
			exec.Command("userdel", u).Run()
		}
		exec.Command("systemctl", "stop", `silo-agent-itest.slice`).Run()
	}
	cleanupUsers()
	t.Cleanup(cleanupUsers)

	newGW := func() *Gateway {
		clis, err := LoadCLIs(cfg.CLIsDir)
		if err != nil {
			t.Fatal(err)
		}
		audit, err := OpenAuditLog(cfg.AuditLog, SystemClock{}, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { audit.Close() })
		g, err := New(cfg, Deps{
			Systemd: NewSystemdRun(), Users: NewShadowUsers(), Procs: ProcFS{}, Clock: SystemClock{},
			Launcher: &CLILauncher{CLIs: clis}, Audit: audit, Store: Store{Dir: cfg.StateDir}, Logf: t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return g
	}
	g := newGW()
	if _, err := g.Spawn(ctx, admin, SpawnParams{Name: "itest", CLI: "sleep", Limits: Limits{MemoryMax: "256M", TasksMax: 64}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Spawn(ctx, admin, SpawnParams{Name: "itest-kid", Parent: "itest", CLI: "sleep"}); err != nil {
		t.Fatal(err)
	}
	show := func(unit, prop string) string {
		out, _ := exec.Command("systemctl", "show", "--value", "--property="+prop, unit).Output()
		return strings.TrimSpace(string(out))
	}
	for _, unit := range []string{"silo-agent-itest.service", "silo-agent-itest-kid.service"} {
		if s := show(unit, "ActiveState"); s != "active" {
			t.Fatalf("%s: %s", unit, s)
		}
	}
	if s := show("silo-agent-itest.service", "User"); s != "silo-a-itest" {
		t.Fatalf("User=%s", s)
	}
	if s := show("silo-agent-itest.service", "MemoryMax"); s != "268435456" {
		t.Fatalf("MemoryMax=%s", s)
	}
	if s := show(`silo-agent-itest.slice`, "MemoryMax"); s != "268435456" {
		t.Fatalf("slice MemoryMax=%s", s)
	}
	if s := show("silo-agent-itest-kid.service", "ControlGroup"); !strings.HasSuffix(s, `/silo-agent-itest.slice/silo-agent-itest-itest\x2dkid.slice/silo-agent-itest-kid.service`) {
		t.Fatalf("kid cgroup %s is not nested in its parent's slice", s)
	}
	home := g.st.Agents["itest"].Home
	fi, err := os.Stat(home)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("home %s: %v %v", home, fi.Mode(), err)
	}
	var who []byte
	for i := 0; i < 50; i++ {
		if who, _ = os.ReadFile(filepath.Join(home, "whoami")); strings.Contains(string(who), "\n") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := string(who); !strings.HasPrefix(got, "silo-a-itest\n") || !strings.Contains(got, "hidden") {
		t.Fatalf("agent wrote %q, want its user name and that the gateway state is hidden", got)
	}

	// A restarted gateway re-adopts the running units.
	g = newGW()
	if g.st.Agents["itest"].State != StateRunning || g.st.Agents["itest-kid"].State != StateRunning {
		t.Fatal("not re-adopted")
	}

	res, err := g.Stop(ctx, admin, StopParams{Agent: "itest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stopped) != 2 || res.Tombstone == nil || res.Tombstone.LastState != "active/running" {
		t.Fatalf("stop: %+v %+v", res, res.Tombstone)
	}
	for _, unit := range []string{"silo-agent-itest.service", "silo-agent-itest-kid.service"} {
		if s := show(unit, "ActiveState"); s == "active" {
			t.Fatalf("%s still active", unit)
		}
	}
	if v, err := g.d.Audit.Verify(); err != nil || !v.OK {
		t.Fatalf("audit chain %+v %v", v, err)
	}
}
