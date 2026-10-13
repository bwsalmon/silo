package storage_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/tree"
)

func TestDirLayoutAndIdempotence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d := &storage.Dir{Root: root}
	if d.Name() != "dir" || d.Guarantees().String() != "sizes charged, not enforced; no snapshots" {
		t.Errorf("%s: %s", d.Name(), d.Guarantees())
	}
	silo := storage.Ref{Silo: "k7f3qa"}
	vol := storage.Ref{Silo: "k7f3qa", Volume: "v1"}
	if _, err := d.Create(ctx, vol, 1<<20); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a volume before its silo's subtree: %v", err)
	}
	for range 2 {
		p, err := d.Create(ctx, silo, 0)
		if err != nil || p != filepath.Join(root, "k7f3qa") {
			t.Fatalf("Create subtree: %q %v", p, err)
		}
		p, err = d.Create(ctx, vol, 1<<20)
		if err != nil || p != filepath.Join(root, "k7f3qa", "volumes", "v1") {
			t.Fatalf("Create volume: %q %v", p, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "k7f3qa", "volumes", "v1", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Creating it again keeps what it holds.
	if _, err := d.Create(ctx, vol, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "k7f3qa", "volumes", "v1", "f")); err != nil {
		t.Errorf("Create again lost the volume's contents: %v", err)
	}
	for range 2 {
		if err := d.Delete(ctx, vol); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "k7f3qa", "volumes", "v1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("volume after Delete: %v", err)
	}
	if err := d.Delete(ctx, silo); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("silos/ after deleting the subtree: %v", entries)
	}
	if _, err := d.Snapshot(ctx, silo, "s"); !errors.Is(err, storage.ErrSnapshotUnsupported) {
		t.Errorf("Snapshot: %v", err)
	}
}

func TestDirRefusesPathsOutsideItsRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d := &storage.Dir{Root: filepath.Join(root, "silos")}
	for _, r := range []storage.Ref{
		{}, {Silo: ".."}, {Silo: "."}, {Silo: "a/b"}, {Silo: "/etc"},
		{Silo: "k7f3qa", Volume: ".."}, {Silo: "k7f3qa", Volume: "../../x"}, {Silo: "k7f3qa", Volume: ".hidden"},
	} {
		if _, err := d.Create(ctx, r, 0); err == nil {
			t.Errorf("Create(%+v) succeeded", r)
		}
		if err := d.Delete(ctx, r); err == nil {
			t.Errorf("Delete(%+v) succeeded", r)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("made %v", entries)
	}
}

func TestDirUsage(t *testing.T) {
	ctx := context.Background()
	d := &storage.Dir{Root: t.TempDir()}
	vol := storage.Ref{Silo: "k7f3qa", Volume: "v1"}
	if _, err := d.Usage(ctx, vol); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Usage of a missing volume: %v", err)
	}
	d.Create(ctx, storage.Ref{Silo: "k7f3qa"}, 0)
	p, err := d.Create(ctx, vol, 0)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := d.Usage(ctx, vol)
	if err != nil || empty.Inodes != 1 {
		t.Fatalf("empty volume: %+v %v", empty, err)
	}
	f := filepath.Join(p, "data")
	if err := os.WriteFile(f, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(p, "sub"), 0o755)
	// A second link to the same file is not counted twice.
	if err := os.Link(f, filepath.Join(p, "sub", "again")); err != nil {
		t.Fatal(err)
	}
	u, err := d.Usage(ctx, vol)
	if err != nil {
		t.Fatal(err)
	}
	if u.Inodes != 3 || u.Bytes < 1<<20 || u.Bytes > 1<<20+64<<10 {
		t.Errorf("usage %+v, want 3 inodes and about 1 MiB", u)
	}
	// The subtree's usage includes its volumes.
	if s, err := d.Usage(ctx, storage.Ref{Silo: "k7f3qa"}); err != nil || s.Bytes < u.Bytes || s.Inodes != u.Inodes+2 {
		t.Errorf("subtree usage %+v %v, volume's %+v", s, err, u)
	}
}

func TestMeterOverQuota(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	var child *tree.Silo
	if err := db.Tx(ctx, func(tx store.Tx) error {
		root, err := tree.CreateRoot(ctx, tx, "root", tree.BackingSubnet, nil)
		if err != nil {
			return err
		}
		child, err = tree.CreateChild(ctx, tx, root.ID, "a", "subnet", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	d := &storage.Dir{Root: t.TempDir()}
	d.Create(ctx, storage.Ref{Silo: child.ID}, 0)
	small := storage.Volume{Ref: storage.Ref{Silo: child.ID, Volume: "small"}, Size: 64 << 10}
	big := storage.Volume{Ref: storage.Ref{Silo: child.ID, Volume: "big"}, Size: storage.DefaultVolumeSize}
	gone := storage.Volume{Ref: storage.Ref{Silo: child.ID, Volume: "gone"}, Size: 1}
	p, _ := d.Create(ctx, small.Ref, small.Size)
	d.Create(ctx, big.Ref, big.Size)
	vols := []storage.Volume{small, big, gone}
	m := &storage.Meter{
		Driver:  d,
		Volumes: func(context.Context) ([]storage.Volume, error) { return vols, nil },
		Report:  storage.AuditOverQuota(db),
	}
	audits := func() int {
		var n int
		db.Tx(ctx, func(tx store.Tx) error {
			return tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit WHERE kind = 'volume_over_quota' AND silo = ? AND fields LIKE '%"volume":"small"%'`, child.ID).Scan(&n)
		})
		return n
	}

	// A missing volume (gone) is skipped, not an error.
	if err := m.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckMount(small.Ref); err != nil || len(m.Over()) != 0 || audits() != 0 {
		t.Fatalf("under quota: %v %v %d", err, m.Over(), audits())
	}

	// Over: reported once, and mounts refused with 409; writes go on.
	if err := os.WriteFile(filepath.Join(p, "f"), make([]byte, 256<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.Scan(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := audits(); n != 1 {
		t.Errorf("%d volume_over_quota audits, want 1", n)
	}
	err = m.CheckMount(small.Ref)
	var oq *storage.OverQuotaError
	if !errors.As(err, &oq) || !errors.Is(err, storage.ErrOverQuota) || oq.HTTPStatus() != http.StatusConflict || !strings.Contains(err.Error(), "small") {
		t.Fatalf("mounting an over-quota volume: %v", err)
	}
	if err := m.CheckMount(big.Ref); err != nil {
		t.Errorf("mounting a volume under quota: %v", err)
	}
	if o := m.Over(); len(o) != 1 || o[0].Volume != small || o[0].Usage.Bytes <= small.Size {
		t.Errorf("Over() = %+v", o)
	}

	// Back under, measured at a container's stop: mounts are allowed.
	os.Remove(filepath.Join(p, "f"))
	if _, err := m.Measure(ctx, small); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckMount(small.Ref); err != nil || len(m.Over()) != 0 {
		t.Errorf("back under quota: %v %v", err, m.Over())
	}

	// Over again, then no longer listed: forgotten.
	os.WriteFile(filepath.Join(p, "f"), make([]byte, 256<<10), 0o644)
	m.Scan(ctx)
	if n := audits(); n != 2 {
		t.Errorf("%d volume_over_quota audits after going over again, want 2", n)
	}
	vols = []storage.Volume{big}
	m.Scan(ctx)
	if err := m.CheckMount(small.Ref); err != nil {
		t.Errorf("a deleted volume still refused: %v", err)
	}
}
