package exports_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bwsalmon/silo/pkg/exports"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

func open(t *testing.T) store.Database {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background(), db.Latest()); err != nil {
		t.Fatal(err)
	}
	return db
}

func tx(t *testing.T, db store.Database, fn func(store.Tx) error) {
	t.Helper()
	if err := db.Tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func deref(os []*exports.Offer) []exports.Offer {
	var out []exports.Offer
	for _, o := range os {
		out = append(out, *o)
	}
	return out
}

func TestTarget(t *testing.T) {
	e := &exports.Export{TargetIP: "10.0.0.5", TargetPort: 8080}
	if got := e.Target().String(); got != "10.0.0.5:8080" {
		t.Errorf("Target = %s", got)
	}
	e.TargetIP = ""
	if e.Target().IsValid() {
		t.Errorf("Target with no address = %s, want invalid", e.Target())
	}
}

func TestAddressAndRuleName(t *testing.T) {
	h := &exports.Export{ID: "e1", Exporter: "s1", Name: "api", Protocol: exports.HTTP}
	if got := exports.Address(h, &exports.Import{}); got != "http://silo.internal/x/s1/api/" {
		t.Errorf("http Address = %s", got)
	}
	tcp := &exports.Export{ID: "e2", Exporter: "s1", Name: "db", Protocol: exports.TCP}
	if got := exports.Address(tcp, &exports.Import{Port: 20003}); got != "silo.internal:20003" {
		t.Errorf("tcp Address = %s", got)
	}
	if got := exports.RuleName(tcp); got != "export-e2-in" {
		t.Errorf("RuleName = %s", got)
	}
}

func TestExports(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	a := &exports.Export{ID: "e1", Exporter: "s1", Name: "web", Owner: exports.OwnerTenant, Protocol: exports.HTTP,
		TargetIP: "10.0.0.2", TargetPort: 80, ProxyProtocol: true}
	b := &exports.Export{ID: "e2", Exporter: "s1", Name: "db", Owner: exports.OwnerTenant, Protocol: exports.TCP,
		TargetPort: 5432, Instance: "vm-1", Zone: "us-central1-a"}
	c := &exports.Export{ID: "e3", Exporter: "s2", Name: "web", Owner: exports.OwnerTenant, Protocol: exports.HTTP,
		TargetIP: "10.0.1.2", TargetPort: 80}
	tx(t, db, func(tx store.Tx) error {
		for _, e := range []*exports.Export{a, b, c} {
			if err := exports.Create(ctx, tx, e); err != nil {
				return err
			}
			if e.Created.IsZero() {
				t.Errorf("%s: Create left Created zero", e.ID)
			}
		}
		return nil
	})
	tx(t, db, func(tx store.Tx) error {
		dup := *a
		dup.ID = "e9"
		if err := exports.Create(ctx, tx, &dup); err == nil {
			t.Error("Create of a second web export from s1 succeeded")
		}
		return nil
	})
	tx(t, db, func(tx store.Tx) error {
		got, err := exports.Get(ctx, tx, "e1")
		if err != nil {
			return err
		}
		if want := *a; !reflect.DeepEqual(*got, want) {
			t.Errorf("Get(e1) = %+v, want %+v", got, want)
		}
		if got, err := exports.Get(ctx, tx, "e2"); err != nil || !reflect.DeepEqual(got, b) {
			t.Errorf("Get(e2) = %+v, %v; want %+v", got, err, b)
		}
		if _, err := exports.Get(ctx, tx, "nope"); !errors.Is(err, exports.ErrNotFound) {
			t.Errorf("Get(nope) = %v, want ErrNotFound", err)
		}
		if got, err := exports.ByName(ctx, tx, "s2", "web"); err != nil || got.ID != "e3" {
			t.Errorf("ByName(s2, web) = %v, %v", got, err)
		}
		of, err := exports.Of(ctx, tx, "s1")
		if err != nil {
			return err
		}
		if len(of) != 2 || of[0].Name != "db" || of[1].Name != "web" {
			t.Errorf("Of(s1) = %v, want db then web", of)
		}
		all, err := exports.All(ctx, tx)
		if err != nil {
			return err
		}
		if len(all) != 3 || all[0].ID != "e1" || all[2].ID != "e3" {
			t.Errorf("All = %v", all)
		}
		if err := exports.SetTarget(ctx, tx, "e2", "10.0.0.9"); err != nil {
			return err
		}
		if got, _ := exports.Get(ctx, tx, "e2"); got.Target().String() != "10.0.0.9:5432" {
			t.Errorf("after SetTarget, e2's target = %s", got.Target())
		}
		return nil
	})
}

func TestOffersAndImports(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	tx(t, db, func(tx store.Tx) error {
		e := &exports.Export{ID: "e1", Exporter: "s1", Name: "db", Owner: exports.OwnerTenant, Protocol: exports.TCP, TargetIP: "10.0.0.2", TargetPort: 5432}
		if err := exports.Create(ctx, tx, e); err != nil {
			return err
		}
		for _, o := range []exports.Offer{{"e1", "/org/b", "s2"}, {"e1", "/org/a", "s3"}, {"e1", "/org/gone", ""}} {
			if err := exports.AddOffer(ctx, tx, o.Export, o.ToPath, o.ToSilo); err != nil {
				return err
			}
		}
		// Offering again changes nothing, not even to_silo.
		if err := exports.AddOffer(ctx, tx, "e1", "/org/b", "s9"); err != nil {
			return err
		}
		offers, err := exports.Offers(ctx, tx, "e1")
		if err != nil {
			return err
		}
		want := []*exports.Offer{{"e1", "/org/a", "s3"}, {"e1", "/org/b", "s2"}, {"e1", "/org/gone", ""}}
		if !reflect.DeepEqual(offers, want) {
			t.Errorf("Offers = %+v, want %+v", deref(offers), deref(want))
		}
		if to, err := exports.OffersTo(ctx, tx, "s2"); err != nil || len(to) != 1 || to[0].ToPath != "/org/b" {
			t.Errorf("OffersTo(s2) = %v, %v", to, err)
		}
		// "" is no silo, not every offer made to no silo.
		if to, err := exports.OffersTo(ctx, tx, ""); err != nil || len(to) != 0 {
			t.Errorf(`OffersTo("") = %v, %v; want none`, to, err)
		}

		i2 := &exports.Import{ID: "i1", Export: "e1", Importer: "s2", State: exports.StateActive}
		i3 := &exports.Import{ID: "i2", Export: "e1", Importer: "s3", State: exports.StateActive}
		for _, i := range []*exports.Import{i2, i3} {
			if err := exports.CreateImport(ctx, tx, i, true); err != nil {
				return err
			}
		}
		if i2.Port != exports.FirstPort || i3.Port != exports.FirstPort+1 {
			t.Errorf("ports = %d, %d; want %d, %d", i2.Port, i3.Port, exports.FirstPort, exports.FirstPort+1)
		}
		if err := exports.CreateImport(ctx, tx, &exports.Import{ID: "i3", Export: "e1", Importer: "s2", State: exports.StateActive}, true); err == nil {
			t.Error("a second import of e1 by s2 succeeded")
		}
		return nil
	})
	tx(t, db, func(tx store.Tx) error {
		got, err := exports.GetImport(ctx, tx, "i1")
		if err != nil {
			return err
		}
		if got.Export != "e1" || got.Importer != "s2" || got.Port != exports.FirstPort || got.State != exports.StateActive || got.Created.IsZero() {
			t.Errorf("GetImport(i1) = %+v", got)
		}
		if _, err := exports.GetImport(ctx, tx, "nope"); !errors.Is(err, exports.ErrNotFound) {
			t.Errorf("GetImport(nope) = %v, want ErrNotFound", err)
		}
		if got, err := exports.ImportOf(ctx, tx, "e1", "s3"); err != nil || got.ID != "i2" {
			t.Errorf("ImportOf(e1, s3) = %v, %v", got, err)
		}
		if got, err := exports.ImportsOf(ctx, tx, "e1"); err != nil || len(got) != 2 {
			t.Errorf("ImportsOf(e1) = %v, %v", got, err)
		}
		if got, err := exports.ImportsBy(ctx, tx, "s3"); err != nil || len(got) != 1 || got[0].ID != "i2" {
			t.Errorf("ImportsBy(s3) = %v, %v", got, err)
		}
		if err := exports.SetState(ctx, tx, "i1", exports.StateSuspended); err != nil {
			return err
		}
		if got, _ := exports.GetImport(ctx, tx, "i1"); got.State != exports.StateSuspended {
			t.Errorf("after SetState, i1 is %s", got.State)
		}

		// Withdrawing the offer to s2 drops s2's import with it, and
		// leaves s3's.
		if err := exports.DeleteOffer(ctx, tx, &exports.Offer{Export: "e1", ToPath: "/org/b", ToSilo: "s2"}); err != nil {
			return err
		}
		if _, err := exports.ImportOf(ctx, tx, "e1", "s2"); !errors.Is(err, exports.ErrNotFound) {
			t.Errorf("s2's import after its offer was withdrawn: %v", err)
		}
		if offers, _ := exports.Offers(ctx, tx, "e1"); len(offers) != 2 {
			t.Errorf("Offers after DeleteOffer = %v", offers)
		}

		// Deleting an import keeps its offer.
		if err := exports.DeleteImport(ctx, tx, "i2"); err != nil {
			return err
		}
		if all, _ := exports.AllImports(ctx, tx); len(all) != 0 {
			t.Errorf("AllImports after DeleteImport = %v", all)
		}
		if to, _ := exports.OffersTo(ctx, tx, "s3"); len(to) != 1 {
			t.Errorf("OffersTo(s3) after DeleteImport = %v, want the offer kept", to)
		}

		// Delete drops the export, its offers and its imports.
		if err := exports.CreateImport(ctx, tx, &exports.Import{ID: "i4", Export: "e1", Importer: "s3", State: exports.StateActive}, true); err != nil {
			return err
		}
		if err := exports.Delete(ctx, tx, "e1"); err != nil {
			return err
		}
		if _, err := exports.Get(ctx, tx, "e1"); !errors.Is(err, exports.ErrNotFound) {
			t.Errorf("Get after Delete = %v", err)
		}
		if offers, _ := exports.Offers(ctx, tx, "e1"); len(offers) != 0 {
			t.Errorf("Offers after Delete = %v", offers)
		}
		if all, _ := exports.AllImports(ctx, tx); len(all) != 0 {
			t.Errorf("AllImports after Delete = %v", all)
		}
		return nil
	})
}

// TestPorts proves a tcp import takes the lowest free port, reuses a
// freed one, an http import takes none, and the range runs out.
func TestPorts(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	imp := func(tx store.Tx, id string, tcp bool) (*exports.Import, error) {
		i := &exports.Import{ID: id, Export: "e-" + id, Importer: "s1", State: exports.StateActive}
		return i, exports.CreateImport(ctx, tx, i, tcp)
	}
	tx(t, db, func(tx store.Tx) error {
		for n := 0; n < 3; n++ {
			if _, err := imp(tx, fmt.Sprintf("t%d", n), true); err != nil {
				return err
			}
		}
		h, err := imp(tx, "h", false)
		if err != nil {
			return err
		}
		if h.Port != 0 {
			t.Errorf("http import got port %d", h.Port)
		}
		if err := exports.DeleteImport(ctx, tx, "t1"); err != nil {
			return err
		}
		i, err := imp(tx, "t3", true)
		if err != nil {
			return err
		}
		if i.Port != exports.FirstPort+1 {
			t.Errorf("after freeing %d, next import got %d", exports.FirstPort+1, i.Port)
		}
		i, err = imp(tx, "t4", true)
		if err != nil {
			return err
		}
		if i.Port != exports.FirstPort+3 {
			t.Errorf("next import got %d, want %d", i.Port, exports.FirstPort+3)
		}
		return nil
	})

	// Fill the rest of the range directly, then ask for one more.
	tx(t, db, func(tx store.Tx) error {
		for p := exports.FirstPort + 4; p <= exports.LastPort; p++ {
			if _, err := tx.Exec(ctx, `INSERT INTO imports (id, export, importer, port, state, created_at, updated_at) VALUES (?, ?, 's1', ?, 'ACTIVE', 0, 0)`,
				fmt.Sprintf("f%d", p), fmt.Sprintf("ef%d", p), p); err != nil {
				return err
			}
		}
		if _, err := imp(tx, "last", true); !errors.Is(err, exports.ErrPortsExhausted) {
			t.Errorf("import with every port taken = %v, want ErrPortsExhausted", err)
		}
		// An http import still goes in.
		if _, err := imp(tx, "h2", false); err != nil {
			t.Errorf("http import with every port taken: %v", err)
		}
		return nil
	})
}

func TestRegistryExports(t *testing.T) {
	e := &exports.Export{ID: "e1", Exporter: "s1", Name: "base-images", Owner: exports.OwnerTenant,
		Protocol: exports.Registry, Repos: []string{"builder", "base-*"}}
	for repo, want := range map[string]bool{
		"builder": true, "builder2": false, "base-": true, "base-alpine": true, "base": false, "app": false,
	} {
		if got := e.Covers(repo); got != want {
			t.Errorf("Covers(%q) = %v, want %v", repo, got, want)
		}
	}
	if all := (&exports.Export{Protocol: exports.Registry, Repos: []string{"*"}}); !all.Covers("anything") {
		t.Error(`"*" doesn't cover every repo`)
	}
	if h := (&exports.Export{Protocol: exports.HTTP, Repos: []string{"*"}}); h.Covers("app") {
		t.Error("an http export covers an image repo")
	}
	if got := exports.Address(e, &exports.Import{}); got != "" {
		t.Errorf("registry Address = %q, want none", got)
	}

	ctx := context.Background()
	db := open(t)
	tx(t, db, func(tx store.Tx) error { return exports.Create(ctx, tx, e) })
	tx(t, db, func(tx store.Tx) error {
		got, err := exports.Get(ctx, tx, "e1")
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got.Repos, e.Repos) || got.Target().IsValid() {
			t.Errorf("Get(e1) = %+v, want repos %v and no target", got, e.Repos)
		}
		return nil
	})
}
