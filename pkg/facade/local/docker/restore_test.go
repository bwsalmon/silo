package docker

import (
	"database/sql"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local"
	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/objseq"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/leader"
	"github.com/bwsalmon/silo/pkg/reconcile"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

// TestCopyAndRestore copies the store with VACUUM INTO while the facade
// creates containers and volumes (local-rootless-storage.md call L39),
// restores the copy over the running data directory, and checks call
// L40: silod notices the restore by the object sequence, the reconcile
// goes report-only, and every container and volume made after the copy
// is reported as an orphan, left fenced off, and not deleted.
func TestCopyAndRestore(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{e.a, e.b, e.sandboxOnly} {
		e.pull(s, "alpine")
	}
	e.create(e.a, "before", nil)

	// The write load: containers, each with the image's anonymous
	// volume, in two silos, until each is at its quota.
	var wg sync.WaitGroup
	started := make(chan struct{})
	for _, silo := range []string{e.b, e.sandboxOnly} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 3 {
				code, b, _ := e.raw(silo, "POST", fmt.Sprintf("/v1.47/containers/create?name=load-%d", i), obj{"Image": "alpine"}, nil)
				if code != 201 {
					t.Errorf("load create in %s: %d %s", silo, code, b)
				}
				if i == 0 && silo == e.b {
					close(started)
				}
			}
		}()
	}
	<-started
	cp := filepath.Join(t.TempDir(), "copy.db")
	raw, err := sql.Open("sqlite", "file:"+e.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`VACUUM INTO ?`, cp); err != nil {
		t.Fatalf("VACUUM INTO under load: %v", err)
	}
	raw.Close()
	wg.Wait()
	after := e.create(e.a, "after", nil)

	// What the copy has: everything else on disk was made after it.
	copied := map[string]bool{}
	cdb, err := sql.Open("sqlite", "file:"+cp+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := cdb.Query(`SELECT silo, kind, id FROM docker_objects WHERE kind IN ('container', 'volume')`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var silo, kind, id string
		if err := rs.Scan(&silo, &kind, &id); err != nil {
			t.Fatal(err)
		}
		copied[kind+":"+silo+"/"+id] = true
	}
	rs.Close()
	cdb.Close()

	drivers := []runtime.Driver{e.vm, e.sbx}
	sd := &storage.Dir{Root: e.vols}
	var wantOrphans, keep []string
	var later []runtime.Container
	for _, d := range drivers {
		cs, _ := d.List(ctx)
		for _, c := range cs {
			if objseq.Parse(c.Labels[runtime.LabelSeq]) == 0 {
				t.Errorf("container %s has no %s label: %v", c.ID, runtime.LabelSeq, c.Labels)
			}
			if !copied["container:"+c.Labels[runtime.LabelSilo]+"/"+c.ID] {
				wantOrphans = append(wantOrphans, local.KindContainers+":"+string(c.Class)+"/"+c.ID)
				later = append(later, c)
			} else {
				keep = append(keep, local.KindContainers+":"+string(c.Class)+"/"+c.ID)
			}
		}
	}
	es, err := sd.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range es {
		if en.Seq == 0 {
			t.Errorf("%s has no %s", en.Ref, storage.SeqFile)
		}
		if en.Ref.Volume != "" && !copied["volume:"+en.Ref.Silo+"/"+en.Ref.Volume] {
			wantOrphans = append(wantOrphans, local.KindVolumes+":"+en.Ref.Silo+"/"+en.Ref.Volume)
		}
	}
	t.Logf("%d objects made after the copy, %d containers in it", len(wantOrphans), len(keep))
	if len(later) == 0 || !slices.ContainsFunc(later, func(c runtime.Container) bool { return c.ID == after }) {
		t.Fatalf("no container made after the copy: %v", later)
	}

	// The restore: the copy put back over the store's file while the
	// runtime and the volumes keep running, and silod started again.
	e.facade.Close()
	e.db.Close()
	for _, f := range []string{e.path + "-wal", e.path + "-shm"} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	copyFile(t, cp, e.path)
	db, err := sqlite.Open(e.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}

	r, err := local.DetectRestore(ctx, db, drivers, sd, false)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.DiskSeq <= r.StoreSeq || r.Forced {
		t.Fatalf("DetectRestore = %+v, want the disk ahead of the store", r)
	}
	if mode, _, _ := store.GetSetting(ctx, db, store.SettingReconcile); mode != store.ReconcileReportOnly {
		t.Errorf("reconcile is %q after a restore, want report-only", mode)
	}
	if audited(t, db, fenceapi.KindRestoreDetected) != 1 {
		t.Errorf("restore_detected audited %d times, want 1", audited(t, db, fenceapi.KindRestoreDetected))
	}
	// The next start finds nothing new: the high-water mark was raised.
	if again, err := local.DetectRestore(ctx, db, drivers, sd, false); err != nil || again != nil {
		t.Errorf("second DetectRestore = %+v, %v; want none", again, err)
	}

	// Every row has its object, so there is nothing to make again.
	f := New(db, Options{Drivers: drivers, Storage: sd})
	defer f.Close()
	rec, err := f.Recover(ctx)
	if err != nil || len(rec.Recreated)+len(rec.NotRecreated)+len(rec.Missing) != 0 {
		t.Errorf("Recover = %+v, %v; want nothing to do", rec, err)
	}

	epoch, err := leader.Take(ctx, db, "test")
	if err != nil {
		t.Fatal(err)
	}
	lctx := leader.WithTerm(ctx, &leader.Term{Epoch: epoch, Holder: "test"})
	inv := local.Containers{Drivers: drivers, Storage: sd, Store: db, Records: Records}
	rc := reconcile.New(reconcile.Options{Store: db, Inventory: inv, Grace: time.Nanosecond, Logf: t.Logf})
	for range 2 { // the second run is past the grace period
		rep, err := rc.Run(lctx)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.ReportOnly || len(rep.Deleted) != 0 {
			t.Errorf("report-only %v, deleted %v; want report-only, nothing deleted", rep.ReportOnly, rep.Deleted)
		}
		slices.Sort(rep.Orphans)
		slices.Sort(wantOrphans)
		if !slices.Equal(rep.Orphans, wantOrphans) {
			t.Errorf("orphans %v, want %v", rep.Orphans, wantOrphans)
		}
		for _, k := range keep {
			if slices.Contains(rep.Orphans, k) {
				t.Errorf("%s, in the copy, reported as an orphan", k)
			}
		}
	}
	for _, c := range later {
		if !slices.ContainsFunc(listAll(t, drivers), func(x runtime.Container) bool { return x.ID == c.ID }) {
			t.Errorf("orphan %s was deleted", c.ID)
		}
	}
	for _, k := range wantOrphans {
		if rest, ok := strings.CutPrefix(k, local.KindVolumes+":"); ok {
			if _, err := os.Stat(filepath.Join(e.vols, strings.Replace(rest, "/", "/volumes/", 1))); err != nil {
				t.Errorf("orphan volume %s: %v", rest, err)
			}
		}
	}

	// Fenced off: the restored store pins no port of a later
	// container, and a port with no pin drops everything.
	var topo *netfence.Topology
	if err := db.Tx(ctx, func(tx store.Tx) error {
		var err error
		topo, err = Topology(netip.MustParseAddr("10.255.255.1"), netip.MustParseAddr("10.255.255.2"))(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range later {
		for _, p := range topo.Ports {
			if strings.HasPrefix(p.Name, "sv"+c.ID[:7]) {
				t.Errorf("orphan %s has a pinned port %s after the restore", c.ID, p.Name)
			}
		}
	}

	// A container made now is numbered above everything on disk.
	if err := db.Tx(ctx, func(tx store.Tx) error {
		n, err := objseq.Next(ctx, tx)
		if err == nil && n <= r.DiskSeq {
			err = fmt.Errorf("next object %d is not above the disk's %d", n, r.DiskSeq)
		}
		return err
	}); err != nil {
		t.Error(err)
	}
}

// TestRecover: a container row whose record is gone is made again,
// stopped, and one whose volume is gone is not, nor is the volume.
func TestRecover(t *testing.T) {
	e := newEnv(t)
	gone := e.create(e.a, "gone", nil)
	lost := e.create(e.a, "lost", nil)
	drivers := []runtime.Driver{e.vm, e.sbx}
	var lostVol string
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		c, err := row[Container](ctx, tx, e.a, kindContainer, lost)
		if err == nil {
			lostVol = c.Mounts[0].Name
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{gone, lost} {
		for _, d := range drivers {
			d.Delete(ctx, id)
		}
	}
	sd := &storage.Dir{Root: e.vols}
	if err := sd.Delete(ctx, storage.Ref{Silo: e.a, Volume: lostVol}); err != nil {
		t.Fatal(err)
	}

	rec, err := e.facade.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.Recreated, []string{gone}) || rec.NotRecreated[lost] == "" || !slices.Equal(rec.Missing, []string{e.a + "/" + lostVol}) {
		t.Fatalf("Recover = %+v", rec)
	}
	cs := listAll(t, drivers)
	i := slices.IndexFunc(cs, func(c runtime.Container) bool { return c.ID == gone })
	if i < 0 || cs[i].State != runtime.Created || cs[i].Labels[runtime.LabelSeq] == "" {
		t.Errorf("re-created container: %+v", cs)
	}
	if slices.ContainsFunc(cs, func(c runtime.Container) bool { return c.ID == lost }) {
		t.Errorf("container %s, whose volume is missing, was made again", lost)
	}
	if _, err := os.Stat(filepath.Join(e.vols, e.a, "volumes", lostVol)); !os.IsNotExist(err) {
		t.Errorf("missing volume made again: %v", err)
	}
	if n := audited(t, e.db, KindVolumeMissing); n != 1 {
		t.Errorf("volume_missing audited %d times, want 1", n)
	}
	if n := audited(t, e.db, KindContainerRecreated); n != 2 {
		t.Errorf("container.recreated audited %d times, want 2 (one FAILED)", n)
	}
	inspect := e.ok(e.a, "GET", "/v1.47/containers/"+lost+"/json", nil)
	if st, _ := inspect["State"].(map[string]any); !strings.Contains(fmt.Sprint(st["Error"]), "volume "+lostVol+" is missing") {
		t.Errorf("inspect of %s: State %v", lost, inspect["State"])
	}
}

func listAll(t *testing.T, drivers []runtime.Driver) []runtime.Container {
	t.Helper()
	var out []runtime.Container
	for _, d := range drivers {
		cs, err := d.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, cs...)
	}
	return out
}

func audited(t *testing.T, db store.Store, kind string) int {
	t.Helper()
	var n int
	if err := db.Tx(ctx, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit WHERE kind = ?`, kind).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
