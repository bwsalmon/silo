package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/internal/testutil"
	"github.com/bwsalmon/silo/internal/vendor"
)

func TestVendorCmd(t *testing.T) {
	base := t.TempDir()
	up := testutil.Remote(t, base, "team/shared", map[string]string{
		".silo/tools/echo/start.sh":  "exec echo v1\n",
		".silo/tools/other/start.py": "pass\n",
	})
	first := strings.TrimSpace(testutil.Git(t, up, "rev-parse", "main"))
	testutil.PushFiles(t, up, "main", map[string]string{".silo/tools/echo/start.sh": "exec echo v2\n"})
	cfg := filepath.Join(base, "silo.yaml")
	os.WriteFile(cfg, []byte("cli: {name: claude}\nrepos: {root: "+filepath.Join(base, "co")+", git_base: "+base+"/}\n"+
		"identity: {name: Ada, email: ada@example.com}\n"), 0o644)
	ctxRepo := t.TempDir()
	testutil.Git(t, ctxRepo, "init", "-q", "-b", "main")
	sub := filepath.Join(ctxRepo, "docs")
	os.MkdirAll(sub, 0o755)

	// Flags after the repository, run from a subdirectory of the
	// context repository.
	wd, _ := os.Getwd()
	os.Chdir(sub)
	err := vendorCmd([]string{"team/shared.git@" + first, "--toolsets", "echo", "--as", "acme", "--config", cfg})
	os.Chdir(wd)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := vendor.ReadLock(ctxRepo, "acme")
	if err != nil || lock.Commit != first || lock.Ref != first || strings.Join(lock.Toolsets, ",") != "echo" {
		t.Fatalf("lock = %+v, %v", lock, err)
	}
	if got, _ := os.ReadFile(filepath.Join(ctxRepo, ".silo/tools/vendor/acme/echo/start.sh")); string(got) != "exec echo v1\n" {
		t.Errorf("start.sh = %q", got)
	}

	if err := vendorCmd([]string{"--config", cfg, "--into", ctxRepo, "--update", "acme", "@main"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(ctxRepo, ".silo/tools/vendor/acme/echo/start.sh")); string(got) != "exec echo v2\n" {
		t.Errorf("start.sh after update = %q", got)
	}
	if lock, _ := vendor.ReadLock(ctxRepo, "acme"); lock.Ref != "main" || lock.Commit == first {
		t.Errorf("updated lock = %+v", lock)
	}

	for _, args := range [][]string{
		{"--config", cfg, "--into", ctxRepo, "team/shared.git"},                       // no --as
		{"--config", cfg, "--into", ctxRepo, "--update", "acme", "main"},              // ref without @
		{"--config", cfg, "--into", ctxRepo, "--update", "acme", "--as", "b"},         // both
		{"--config", cfg, "--into", t.TempDir(), "team/shared.git", "--as", "x"},      // not a git repository
		{"--config", cfg, "--into", ctxRepo, "team/shared.git", "--as", "acme"},       // already vendored
		{"--config", cfg, "--into", ctxRepo, "team/shared.git", "extra", "--as", "z"}, // two repositories
	} {
		if err := vendorCmd(args); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}
