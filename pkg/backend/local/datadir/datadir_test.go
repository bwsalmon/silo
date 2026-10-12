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
	if err := json.Unmarshal(b, &l); err != nil || l["layout"] != 1.0 || l["data_dir_id"] != d.ID {
		t.Errorf("layout.json %s: %v", b, err)
	}
	for name, mode := range map[string]os.FileMode{"state": 0o700, "keys": 0o700, "silos": 0o755, "cache": 0o755} {
		fi, err := os.Stat(filepath.Join(path, name))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", name, fi, err)
		}
	}
	entries, _ := os.ReadDir(path)
	if len(entries) != 5 {
		t.Errorf("data directory holds %v", entries)
	}
	if d.Store() != filepath.Join(path, "state", "silo.db") || d.StoreSpec() != "sqlite:"+d.Store() ||
		d.Keys() != filepath.Join(path, "keys") || d.Silos() != filepath.Join(path, "silos") || d.Cache() != filepath.Join(path, "cache") {
		t.Errorf("paths: %s %s %s %s", d.Store(), d.Keys(), d.Silos(), d.Cache())
	}

	// Opening it again keeps its id, and makes a missing cache again.
	os.RemoveAll(d.Cache())
	again, err := datadir.Open(path)
	if err != nil || again.ID != d.ID {
		t.Fatalf("reopened: %+v %v, want id %s", again, err, d.ID)
	}
	if _, err := os.Stat(d.Cache()); err != nil {
		t.Errorf("cache/ not made again: %v", err)
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
	os.WriteFile(filepath.Join(newer, "layout.json"), []byte(`{"layout": 2, "data_dir_id": "x"}`), 0o644)
	if _, err := datadir.Open(newer); !errors.Is(err, datadir.ErrNewerLayout) {
		t.Errorf("layout 2: %v", err)
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
