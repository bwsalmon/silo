package repos_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/repos"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
)

var ctx = context.Background()

func TestNames(t *testing.T) {
	for _, n := range []string{"foo", "a", "0", "my-repo", "my_repo.v2", strings.Repeat("a", 63)} {
		if !repos.ValidName(n) {
			t.Errorf("ValidName(%q) = false", n)
		}
	}
	for _, n := range []string{"", "Foo", "-foo", ".foo", "_foo", "a..b", "foo.git", "foo.lock", "a/b", "a b", strings.Repeat("a", 64)} {
		if repos.ValidName(n) {
			t.Errorf("ValidName(%q) = true", n)
		}
	}
	for _, b := range []string{"main", "trunk", "release/1.2", "feature/x-y"} {
		if !repos.ValidBranch(b) {
			t.Errorf("ValidBranch(%q) = false", b)
		}
	}
	for _, b := range []string{"", "@", "-x", "/x", "x/", "x.", "a..b", "a//b", "a@{b", "a b", "a:b", "a~1", "a^", "a?", "a*", "a[", `a\b`, ".x", "x/.y", "x.lock", "x/y.lock"} {
		if repos.ValidBranch(b) {
			t.Errorf("ValidBranch(%q) = true", b)
		}
	}
	if id := repos.NewID(); !strings.HasPrefix(id, "r-") || len(id) != 8 {
		t.Errorf("NewID = %q", id)
	}
}

func TestTable(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx store.Tx) error {
		a := &repos.Repo{ID: "r-aaaaaa", Silo: "k7f3qa", Name: "foo", DefaultBranch: "main", Seq: 7}
		if err := repos.Create(ctx, tx, a); err != nil {
			return err
		}
		if err := repos.Create(ctx, tx, &repos.Repo{ID: "r-bbbbbb", Silo: "k7f3qa", Name: "foo", DefaultBranch: "main"}); !errors.Is(err, repos.ErrExists) {
			t.Errorf("a second foo: %v", err)
		}
		// Names are per silo.
		if err := repos.Create(ctx, tx, &repos.Repo{ID: "r-cccccc", Silo: "b2b2b2", Name: "foo", DefaultBranch: "trunk"}); err != nil {
			return err
		}
		if err := repos.Create(ctx, tx, &repos.Repo{ID: "r-dddddd", Silo: "k7f3qa", Name: "bar", DefaultBranch: "main"}); err != nil {
			return err
		}
		got, err := repos.ByName(ctx, tx, "k7f3qa", "foo")
		if err != nil {
			return err
		}
		if got.ID != a.ID || got.Seq != 7 || got.DefaultBranch != "main" || got.Created.IsZero() {
			t.Errorf("ByName = %+v", got)
		}
		if err := repos.SetDefaultBranch(ctx, tx, a.ID, "trunk"); err != nil {
			return err
		}
		if err := repos.SetSize(ctx, tx, a.ID, 4096); err != nil {
			return err
		}
		if got, err = repos.Get(ctx, tx, a.ID); err != nil || got.DefaultBranch != "trunk" || got.SizeBytes != 4096 {
			t.Errorf("Get = %+v %v", got, err)
		}
		list, err := repos.Of(ctx, tx, "k7f3qa", "b2b2b2")
		if err != nil {
			return err
		}
		var names []string
		for _, r := range list {
			names = append(names, r.Silo+"/"+r.Name)
		}
		if strings.Join(names, " ") != "k7f3qa/bar k7f3qa/foo b2b2b2/foo" {
			t.Errorf("Of = %v", names)
		}
		if err := repos.Delete(ctx, tx, a.ID); err != nil {
			return err
		}
		if err := repos.Delete(ctx, tx, a.ID); !errors.Is(err, repos.ErrNotFound) {
			t.Errorf("Delete again: %v", err)
		}
		if _, err := repos.ByName(ctx, tx, "k7f3qa", "foo"); !errors.Is(err, repos.ErrNotFound) {
			t.Errorf("ByName after Delete: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
