package datadir_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/datadir"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

func TestOpenMakesTheLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	d, err := datadir.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != path || len(d.ID) != 32 {
		t.Errorf("%+v", d)
	}
	b, err := os.ReadFile(filepath.Join(path, "layout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var l map[string]any
	if err := json.Unmarshal(b, &l); err != nil || l["layout"] != 2.0 || l["data_dir_id"] != d.ID {
		t.Errorf("layout.json %s: %v", b, err)
	}
	for name, mode := range map[string]os.FileMode{
		"state": 0o700, "keys": 0o700, "silos": 0o755, "containerd": 0o700, "cache": 0o755, "cache/snapshots": 0o700,
	} {
		fi, err := os.Stat(filepath.Join(path, name))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", name, fi, err)
		}
	}
	entries, _ := os.ReadDir(path)
	if len(entries) != 6 {
		t.Errorf("data directory holds %v", entries)
	}
	if d.Store() != filepath.Join(path, "state", "silo.db") || d.StoreSpec() != "sqlite:"+d.Store() ||
		d.Keys() != filepath.Join(path, "keys") || d.Silos() != filepath.Join(path, "silos") || d.Cache() != filepath.Join(path, "cache") ||
		d.Containerd() != filepath.Join(path, "containerd") || d.Snapshots() != filepath.Join(path, "cache", "snapshots") {
		t.Errorf("paths: %s %s %s %s %s %s", d.Store(), d.Keys(), d.Silos(), d.Cache(), d.Containerd(), d.Snapshots())
	}

	// Opening it again keeps its id, and makes a missing cache again.
	os.RemoveAll(d.Cache())
	again, err := datadir.Open(path)
	if err != nil || again.ID != d.ID {
		t.Fatalf("reopened: %+v %v, want id %s", again, err, d.ID)
	}
	if _, err := os.Stat(d.Snapshots()); err != nil {
		t.Errorf("cache/snapshots/ not made again: %v", err)
	}
}

// writeLayout1 makes a layout-1 data directory as a layout-1 silod
// left it, and returns its path.
func writeLayout1(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	os.WriteFile(filepath.Join(path, "layout.json"), []byte(`{"layout": 1, "data_dir_id": "0123456789abcdef0123456789abcdef"}`+"\n"), 0o644)
	for _, sub := range []string{"state", "keys", "silos", "cache"} {
		os.Mkdir(filepath.Join(path, sub), 0o755)
	}
	os.WriteFile(filepath.Join(path, "state", "silo.db"), []byte("db"), 0o600)
	return path
}

// checkLayout2 checks that layout.json says layout 2 with id.
func checkLayout2(t *testing.T, path, id string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(path, "layout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var l map[string]any
	if err := json.Unmarshal(b, &l); err != nil || l["layout"] != 2.0 || l["data_dir_id"] != id {
		t.Errorf("layout.json %s: %v", b, err)
	}
}

func TestMigrateFromLayout1(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"

	t.Run("no containerd", func(t *testing.T) {
		path := writeLayout1(t)
		d, err := datadir.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if d.ID != id {
			t.Errorf("id %s, want %s", d.ID, id)
		}
		checkLayout2(t, path, id)
		for _, dir := range []string{d.Containerd(), d.Snapshots()} {
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("%s: %v %v", dir, entries, err)
			}
		}
		if b, _ := os.ReadFile(d.Store()); string(b) != "db" {
			t.Errorf("store %q", b)
		}
	})

	t.Run("with cache/containerd", func(t *testing.T) {
		path := writeLayout1(t)
		old := filepath.Join(path, "cache", "containerd")
		files := map[string]string{
			"io.containerd.content.v1.content/blobs/sha256/aa":   "blob",
			"io.containerd.metadata.v1.bolt/meta.db":             "meta",
			datadir.SnapshotterDir + "/metadata.db":              "snap meta",
			datadir.SnapshotterDir + "/snapshots/1/fs/upper.txt": "upper",
		}
		for name, content := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(old, name)), 0o700)
			os.WriteFile(filepath.Join(old, name), []byte(content), 0o600)
		}
		oldMeta, err := os.Stat(filepath.Join(old, "io.containerd.metadata.v1.bolt", "meta.db"))
		if err != nil {
			t.Fatal(err)
		}

		d, err := datadir.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		checkLayout2(t, path, id)
		if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("cache/containerd/ still there: %v", err)
		}
		// The content store and its metadata are containerd/'s; the
		// snapshotter's state is cache/snapshots/, its root_path.
		for name, content := range files {
			p := filepath.Join(d.Containerd(), name)
			if rest, ok := strings.CutPrefix(name, datadir.SnapshotterDir+"/"); ok {
				p = filepath.Join(d.Snapshots(), rest)
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != content {
				t.Errorf("%s: %q %v, want %q", p, b, err, content)
			}
		}
		if _, err := os.Lstat(filepath.Join(d.Containerd(), datadir.SnapshotterDir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("snapshotter state left in containerd/: %v", err)
		}
		// Moved, not copied.
		if fi, err := os.Stat(filepath.Join(d.Containerd(), "io.containerd.metadata.v1.bolt", "meta.db")); err != nil || !os.SameFile(fi, oldMeta) {
			t.Errorf("meta.db was copied, not renamed: %v", err)
		}

		// Opening it again changes nothing.
		if _, err := datadir.Open(path); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(d.Containerd(), "io.containerd.metadata.v1.bolt", "meta.db")); string(b) != "meta" {
			t.Errorf("reopened: meta.db %q", b)
		}
	})

	t.Run("finishes a migration a crash cut short", func(t *testing.T) {
		// containerd/ moved, layout.json not yet rewritten.
		path := writeLayout1(t)
		os.MkdirAll(filepath.Join(path, "containerd", datadir.SnapshotterDir), 0o700)
		os.WriteFile(filepath.Join(path, "containerd", datadir.SnapshotterDir, "metadata.db"), []byte("snap meta"), 0o600)
		d, err := datadir.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		checkLayout2(t, path, id)
		if b, err := os.ReadFile(filepath.Join(d.Snapshots(), "metadata.db")); err != nil || string(b) != "snap meta" {
			t.Errorf("snapshotter state: %q %v", b, err)
		}
	})

	t.Run("refuses to merge two roots", func(t *testing.T) {
		path := writeLayout1(t)
		os.MkdirAll(filepath.Join(path, "cache", "containerd"), 0o700)
		os.MkdirAll(filepath.Join(path, "containerd", "something"), 0o700)
		if _, err := datadir.Open(path); err == nil {
			t.Fatal("opened with both cache/containerd/ and a non-empty containerd/")
		}
		b, _ := os.ReadFile(filepath.Join(path, "layout.json"))
		if !strings.Contains(string(b), `"layout": 1`) {
			t.Errorf("layout.json rewritten after a failed migration: %s", b)
		}
	})
}

func TestWipingCacheKeepsContainerd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	d, err := datadir.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(d.Containerd(), "io.containerd.content.v1.content", "blobs", "sha256", "aa")
	os.MkdirAll(filepath.Dir(blob), 0o700)
	os.WriteFile(blob, []byte("pushed"), 0o600)
	os.MkdirAll(filepath.Join(d.Snapshots(), "snapshots", "1"), 0o700)

	if err := os.RemoveAll(d.Cache()); err != nil {
		t.Fatal(err)
	}
	if _, err := datadir.Open(path); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(blob); err != nil || string(b) != "pushed" {
		t.Errorf("pushed blob after wiping cache/: %q %v", b, err)
	}
	if entries, err := os.ReadDir(d.Snapshots()); err != nil || len(entries) != 0 {
		t.Errorf("cache/snapshots/ after wiping cache/: %v %v", entries, err)
	}
}

func TestOpenRefuses(t *testing.T) {
	// A directory that holds something else.
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "notes.txt"), nil, 0o644)
	if _, err := datadir.Open(other); err == nil || !strings.Contains(err.Error(), "not a data directory") {
		t.Errorf("a directory with no layout.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "state")); err == nil {
		t.Error("made state/ in a directory that isn't a data directory")
	}

	// A newer layout.
	newer := t.TempDir()
	os.WriteFile(filepath.Join(newer, "layout.json"), []byte(`{"layout": 3, "data_dir_id": "x"}`), 0o644)
	if _, err := datadir.Open(newer); !errors.Is(err, datadir.ErrNewerLayout) {
		t.Errorf("layout 3: %v", err)
	}

	// A layout.json with no id.
	broken := t.TempDir()
	os.WriteFile(filepath.Join(broken, "layout.json"), []byte(`{"layout": 1}`), 0o644)
	if _, err := datadir.Open(broken); err == nil {
		t.Error("layout.json with no data_dir_id opened")
	}
}

func TestBind(t *testing.T) {
	ctx := context.Background()
	a, err := datadir.Open(filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := datadir.Open(filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(a.Store())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	// The first start records the data directory and the driver; later
	// ones check them.
	for range 2 {
		if err := a.Bind(ctx, db, "dir"); err != nil {
			t.Fatal(err)
		}
	}
	if id, ok, err := store.GetSetting(ctx, db, store.SettingDataDirID); err != nil || !ok || id != a.ID {
		t.Errorf("data_dir_id %q %v %v, want %s", id, ok, err, a.ID)
	}
	if err := b.Bind(ctx, db, "dir"); !errors.Is(err, datadir.ErrMismatch) || !strings.Contains(err.Error(), b.Path) {
		t.Errorf("another data directory's store: %v", err)
	}
	if err := a.Bind(ctx, db, "zfs"); !errors.Is(err, datadir.ErrMismatch) {
		t.Errorf("another driver: %v", err)
	}
}

func TestDefault(t *testing.T) {
	if d, _ := datadir.Default(true); d != "/var/lib/silo" {
		t.Errorf("system: %s", d)
	}
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if d, _ := datadir.Default(false); d != "/xdg/silo" {
		t.Errorf("with XDG_DATA_HOME: %s", d)
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/u")
	if d, _ := datadir.Default(false); d != "/home/u/.local/share/silo" {
		t.Errorf("rootless: %s", d)
	}
}
