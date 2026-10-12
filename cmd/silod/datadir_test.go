package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/datadir"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// runAsSilod is the environment variable that makes the test binary
// run silod's main instead of the tests, so that a test can run silod
// as a process of its own.
const runAsSilod = "SILOD_TEST_RUN_AS_SILOD"

func TestMain(m *testing.M) {
	if os.Getenv(runAsSilod) != "" {
		os.Args = append([]string{"silod"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestNothingPersistentOutsideTheDataDir runs silod with --data-dir in
// a scratch filesystem tree: its own home, XDG directories, temporary
// directory and working directory, all under one root. Afterwards,
// everything under the root that isn't the data directory, or the
// runtime directory that holds the socket, must be as it was
// (local-rootless-storage.md call L35).
func TestNothingPersistentOutsideTheDataDir(t *testing.T) {
	root := t.TempDir()
	dirs := map[string]string{}
	for _, d := range []string{"home", "config", "cache", "state", "share", "tmp", "cwd", "run"} {
		dirs[d] = filepath.Join(root, d)
		if err := os.Mkdir(dirs[d], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(root, "data")
	sock := filepath.Join(dirs["run"], "silod.sock")
	before := walk(t, root)

	start := func(args ...string) (*exec.Cmd, *bytes.Buffer) {
		cmd := exec.Command(os.Args[0], append([]string{"serve", "--data-dir=" + data, "--socket=" + sock}, args...)...)
		cmd.Dir = dirs["cwd"]
		cmd.Env = []string{
			runAsSilod + "=1",
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + dirs["home"],
			"TMPDIR=" + dirs["tmp"],
			"XDG_CONFIG_HOME=" + dirs["config"],
			"XDG_CACHE_HOME=" + dirs["cache"],
			"XDG_STATE_HOME=" + dirs["state"],
			"XDG_DATA_HOME=" + dirs["share"],
			"XDG_RUNTIME_DIR=" + dirs["run"],
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, &out
	}
	stop := func(cmd *exec.Cmd, out *bytes.Buffer) {
		t.Helper()
		cmd.Process.Signal(syscall.SIGTERM)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("silod: %v\n%s", err, out)
		}
	}
	healthy := func(cmd *exec.Cmd, out *bytes.Buffer) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			code, body, err := get(t, sock, "/silod/health")
			if err == nil && code == 200 {
				return
			}
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatalf("silod never healthy: %d %v %v\n%s", code, body, err, out)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Twice: the first start makes the data directory and the store,
	// the second opens and checks them.
	for range 2 {
		cmd, out := start("--migrate", "--dev-caller-header")
		healthy(cmd, out)
		if code, _, err := get(t, sock, "/silo/v1/self"); err != nil || code != 403 {
			t.Errorf("no caller: %d %v", code, err)
		}
		stop(cmd, out)
	}

	after := walk(t, root)
	for p, was := range before {
		if now, ok := after[p]; !ok || now != was {
			t.Errorf("%s changed: %q, now %q (present %v)", p, was, now, ok)
		}
	}
	var inData int
	for p := range after {
		if _, ok := before[p]; ok {
			continue
		}
		switch {
		case p == "data" || strings.HasPrefix(p, "data/"):
			inData++
		case strings.HasPrefix(p, "run/"):
			// Runtime state, never data.
		default:
			t.Errorf("silod wrote %s, outside the data directory", p)
		}
	}
	for _, want := range []string{"data/layout.json", "data/state/silo.db", "data/keys", "data/silos", "data/cache"} {
		if _, ok := after[want]; !ok {
			t.Errorf("%s missing", want)
		}
	}
	if inData == 0 {
		t.Error("silod wrote nothing in the data directory")
	}
}

// walk lists every file and directory under root, relative to it, with
// what it is: a directory, or a file's length and modification time.
func walk(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := e.Info()
		if err != nil {
			return err
		}
		if fi.IsDir() {
			out[rel] = "dir"
		} else {
			out[rel] = fmt.Sprintf("%s %d %s", fi.Mode(), fi.Size(), fi.ModTime())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDataDirSelectsTheStore: --data-dir puts the store in its
// state/silo.db, refuses a --store naming another, and a store that
// belongs to another data directory.
func TestDataDirSelectsTheStore(t *testing.T) {
	ctx := context.Background()
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	err := serve(ctx, []string{"--data-dir=" + a, "--store=sqlite:" + filepath.Join(t.TempDir(), "x.db"), "--listen=127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "--data-dir") {
		t.Errorf("--store elsewhere with --data-dir: %v", err)
	}
	if err := migrate(ctx, []string{"--data-dir=" + a}); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, []string{"--data-dir=" + b}); err != nil {
		t.Fatal(err)
	}
	da, err := datadir.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(da.Store())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := db.Schema(ctx); err != nil || v != db.Latest() {
		t.Errorf("migrate --data-dir: schema %d %v", v, err)
	}
	// The store of a, given to b's data directory.
	if err := store.SetSetting(ctx, db, store.SettingDataDirID, "someone-else"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	err = serve(ctx, []string{"--data-dir=" + a, "--listen=127.0.0.1:0"})
	if !errors.Is(err, datadir.ErrMismatch) {
		t.Errorf("a store that belongs to another data directory: %v", err)
	}
}
