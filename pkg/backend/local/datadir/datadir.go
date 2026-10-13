// Package datadir is the local backend's data directory: the one
// directory that holds all of silod's persistent state
// (docs/silo/local-rootless-storage.md, "One data directory", call L35).
//
//	<data-dir>/
//	  layout.json   {"layout": 2, "data_dir_id": "<random>"}
//	  state/        silo.db, the SQLite store
//	  keys/         silod's own secrets, mode 0700
//	  silos/<id>/   one subtree per silo, made by the storage driver
//	  containerd/   silo-containerd's root: the content store and its
//	                bolt metadata, which hold pushed images (call L60)
//	  cache/        re-creatable; never data
//	    snapshots/  the overlayfs snapshotter's root_path: writable layers
//
// Nothing persistent is written outside it. The store records the data
// directory's id, and Bind refuses a store and a data directory that
// don't belong together.
//
// Layout 1 had no containerd/, and put containerd's root, snapshots
// included, at cache/containerd/. Open migrates it forward
// (docs/silo/local-registry.md, "Pushed images are data").
package datadir

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bwsalmon/silo/pkg/store"
)

// Layout is the layout version this binary writes and runs on.
const Layout = 2

// SnapshotterDir is the directory under containerd's root where the
// overlayfs snapshotter keeps its state when it has no root_path of its
// own: where layout 1's writable layers are, under cache/containerd/.
const SnapshotterDir = "io.containerd.snapshotter.v1.overlayfs"

// LayoutFile is the name of the file that marks a data directory.
const LayoutFile = "layout.json"

// The subdirectories, and their modes. state/ and keys/ are silod's
// alone; the data directory itself is too, so that nothing under it is
// reachable by another user whatever its own mode.
var dirs = []struct {
	name string
	mode fs.FileMode
}{
	{"state", 0o700},
	{"keys", 0o700},
	{"silos", 0o755},
	{"containerd", 0o700},
	{"cache", 0o755},
	{filepath.Join("cache", "snapshots"), 0o700},
}

// ErrNewerLayout is a data directory written by a newer silod.
var ErrNewerLayout = errors.New("datadir: the data directory's layout is newer than this silod")

// ErrMismatch is a store and a data directory that don't belong
// together, or a data directory and a storage driver.
var ErrMismatch = errors.New("datadir: the store does not belong to this data directory")

// Dir is an open data directory.
type Dir struct {
	// Path is the data directory, absolute.
	Path string
	// ID is its data_dir_id, from layout.json.
	ID string
}

// layoutJSON is layout.json.
type layoutJSON struct {
	Layout    int    `json:"layout"`
	DataDirID string `json:"data_dir_id"`
}

// Default is the data directory when none is given: ~/.local/share/silo
// ($XDG_DATA_HOME/silo) in rootless mode, /var/lib/silo for the system
// install (call L34).
func Default(system bool) (string, error) {
	if system {
		return "/var/lib/silo", nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "silo"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("datadir: no home directory for the default data directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "silo"), nil
}

// Open opens the data directory at path, making it if it doesn't exist
// or is empty: layout.json with a new id, and the subdirectories. It
// refuses a directory that isn't empty and has no layout.json, rather
// than take over something that isn't a data directory, and one with a
// newer layout. Subdirectories missing from an existing data directory
// are made again (each is either data the operator removed, which an
// empty directory doesn't pretend to restore, or cache).
func Open(path string) (*Dir, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("datadir: %w", err)
	}
	d := &Dir{Path: abs}
	b, err := os.ReadFile(d.file(LayoutFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, fmt.Errorf("datadir: %w", err)
		}
		for _, e := range entries {
			// A crash in create leaves its temporary file behind.
			if ok, _ := filepath.Match(".layout-*.json", e.Name()); ok {
				continue
			}
			return nil, fmt.Errorf("datadir: %s is not empty and has no %s: not a data directory", abs, LayoutFile)
		}
		if d.ID, err = d.create(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("datadir: %w", err)
	default:
		var l layoutJSON
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("datadir: %s: %w", d.file(LayoutFile), err)
		}
		switch {
		case l.Layout > Layout:
			return nil, fmt.Errorf("%w: %s has layout %d, this silod runs layout %d", ErrNewerLayout, abs, l.Layout, Layout)
		case l.Layout < 1 || l.DataDirID == "":
			return nil, fmt.Errorf("datadir: %s: no layout or data_dir_id", d.file(LayoutFile))
		}
		d.ID = l.DataDirID
		if l.Layout < 2 {
			if err := d.migrate1to2(); err != nil {
				return nil, err
			}
		}
	}
	for _, sub := range dirs {
		if err := os.MkdirAll(d.file(sub.name), sub.mode); err != nil {
			return nil, fmt.Errorf("datadir: %w", err)
		}
	}
	return d, nil
}

// create writes layout.json for a new data directory, and returns its
// id.
func (d *Dir) create() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if err := d.writeLayout(id); err != nil {
		return "", err
	}
	return id, nil
}

// migrate1to2 moves a layout-1 data directory forward. containerd's
// root, if layout 1 has one, moves from cache/containerd/ to
// containerd/ with one rename, and the overlayfs snapshotter's state in
// it moves to cache/snapshots/, its root_path in layout 2, with
// another; nothing is copied. layout.json is rewritten last, so a
// crash part way leaves layout 1, and the next start finishes the
// steps not yet done.
func (d *Dir) migrate1to2() error {
	if err := d.moveDir(d.file("cache", "containerd"), d.Containerd()); err != nil {
		return err
	}
	if err := d.moveDir(filepath.Join(d.Containerd(), SnapshotterDir), d.Snapshots()); err != nil {
		return err
	}
	return d.writeLayout(d.ID)
}

// moveDir renames from to to, if from exists. A to that exists and is
// empty, left by a start that made it before the rename, is replaced;
// one that isn't empty is refused, rather than merge two roots. The two
// must be on one filesystem: the migration never copies.
func (d *Dir) moveDir(from, to string) error {
	if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("datadir: migrating to layout 2: %w", err)
	}
	if err := os.Remove(to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("datadir: migrating to layout 2: %s and %s both exist: %w", from, to, err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("datadir: migrating to layout 2: %w", err)
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("datadir: migrating to layout 2: moving %s to %s (they must be on one filesystem): %w", from, to, err)
	}
	return nil
}

// writeLayout writes layout.json with this binary's layout and id. It
// writes a temporary file and renames it, so that a crash leaves either
// the old layout.json, or none, or a whole new one.
func (d *Dir) writeLayout(id string) error {
	j, err := json.Marshal(layoutJSON{Layout: Layout, DataDirID: id})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.Path, ".layout-*.json")
	if err != nil {
		return fmt.Errorf("datadir: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(j, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), d.file(LayoutFile))
	}
	if err != nil {
		return fmt.Errorf("datadir: writing %s: %w", LayoutFile, err)
	}
	return nil
}

func (d *Dir) file(name ...string) string {
	return filepath.Join(append([]string{d.Path}, name...)...)
}

// Store is the SQLite store's file, state/silo.db.
func (d *Dir) Store() string { return d.file("state", "silo.db") }

// StoreSpec is the --store value for the data directory's store.
func (d *Dir) StoreSpec() string { return "sqlite:" + d.Store() }

// Keys is keys/, where silod's own secrets are kept as files.
func (d *Dir) Keys() string { return d.file("keys") }

// Silos is silos/, under which the storage driver keeps each silo's
// subtree.
func (d *Dir) Silos() string { return d.file("silos") }

// Containerd is containerd/, silo-containerd's root: the content store
// and its bolt metadata. Content a registry repo references is the only
// copy of a pushed image, so it is data, outside cache/ (call L60).
func (d *Dir) Containerd() string { return d.file("containerd") }

// Cache is cache/: writable layers, buildkit, wrapped images and
// runtimes. Nothing in it is data, and wiping it loses no pushed image.
func (d *Dir) Cache() string { return d.file("cache") }

// Snapshots is cache/snapshots/, the overlayfs snapshotter's root_path:
// containers' writable layers, which are never data.
func (d *Dir) Snapshots() string { return d.file("cache", "snapshots") }

// Bind checks that st belongs with d and with the storage driver
// named driver. The first start on a store records both; every later
// one compares them, and refuses with ErrMismatch: a store moved to
// another data directory, or a data directory given another store,
// would otherwise run with every silo's subtree missing or foreign. A
// data directory keeps its driver (call L37).
func (d *Dir) Bind(ctx context.Context, st store.Store, driver string) error {
	return st.Tx(ctx, func(tx store.Tx) error {
		for _, want := range []struct{ name, value, what string }{
			{store.SettingDataDirID, d.ID, "data directory " + d.Path},
			{store.SettingStorageDriver, driver, "storage driver"},
		} {
			got, ok, err := store.ReadSetting(ctx, tx, want.name)
			if err != nil {
				return err
			}
			if !ok {
				if err := store.WriteSetting(ctx, tx, want.name, want.value); err != nil {
					return err
				}
				continue
			}
			if got != want.value {
				return fmt.Errorf("%w: the store's %s is %q, the %s's is %q", ErrMismatch, want.name, got, want.what, want.value)
			}
		}
		return nil
	})
}
