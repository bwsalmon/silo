package tree_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/store/sqlite"
	"github.com/bwsalmon/silo/pkg/tree"
)

func TestTree(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx store.Tx) error {
		root, err := tree.CreateRoot(ctx, tx, "root", tree.BackingSubnet, nil)
		if err != nil {
			return err
		}
		if !ids.Valid(root.ID) || root.Path != "/" || root.Depth != 0 || !root.IsRoot() {
			t.Errorf("root = %+v", root)
		}
		if _, err := tree.CreateRoot(ctx, tx, "again", tree.BackingSubnet, nil); !errors.Is(err, tree.ErrExists) {
			t.Errorf("a second root: %v", err)
		}
		a, err := tree.CreateChild(ctx, tx, root.ID, "team-a", tree.BackingSubnet, map[string]string{"team": "a"})
		if err != nil {
			return err
		}
		if a.Path != "/team-a" || a.Depth != 1 || a.Parent != root.ID {
			t.Errorf("/team-a = %+v", a)
		}
		if _, err := tree.CreateChild(ctx, tx, root.ID, "team-a", tree.BackingSubnet, nil); !errors.Is(err, tree.ErrExists) {
			t.Errorf("a sibling with the same name: %v", err)
		}
		for _, bad := range []string{"Team", "-a", "a-", "a_b", ""} {
			if _, err := tree.CreateChild(ctx, tx, root.ID, bad, tree.BackingSubnet, nil); !errors.Is(err, tree.ErrInvalid) {
				t.Errorf("name %q: %v", bad, err)
			}
		}
		if _, err := tree.CreateChild(ctx, tx, root.ID, "v", "vpc", nil); !errors.Is(err, tree.ErrInvalid) {
			t.Errorf("backing vpc: %v", err)
		}
		// A tree has its root's backing throughout.
		if _, err := tree.CreateChild(ctx, tx, root.ID, "l", tree.BackingLocal, nil); !errors.Is(err, tree.ErrInvalid) {
			t.Errorf("a local child of a subnet root: %v", err)
		}

		// Down to the depth cap, and no further.
		cur := a
		for d := 2; d <= tree.MaxDepth; d++ {
			if cur, err = tree.CreateChild(ctx, tx, cur.ID, fmt.Sprintf("l%d", d), tree.BackingSubnet, nil); err != nil {
				return err
			}
		}
		if cur.Depth != tree.MaxDepth {
			t.Errorf("deepest = %d", cur.Depth)
		}
		if _, err := tree.CreateChild(ctx, tx, cur.ID, "deeper", tree.BackingSubnet, nil); !errors.Is(err, tree.ErrTooDeep) {
			t.Errorf("past the cap: %v", err)
		}
		chain, err := tree.Ancestry(ctx, tx, cur.ID)
		if err != nil {
			return err
		}
		if len(chain) != tree.MaxDepth+1 || chain[0].ID != root.ID || chain[1].ID != a.ID || chain[len(chain)-1].ID != cur.ID {
			t.Errorf("ancestry of the deepest: %d silos", len(chain))
		}
		sub, err := tree.Subtree(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if len(sub) != tree.MaxDepth {
			t.Errorf("subtree of /team-a: %d silos", len(sub))
		}
		got, err := tree.Get(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if got.Labels["team"] != "a" || got.Created.IsZero() {
			t.Errorf("Get(/team-a) = %+v", got)
		}
		kids, err := tree.Children(ctx, tx, root.ID)
		if err != nil {
			return err
		}
		if len(kids) != 1 || kids[0].ID != a.ID {
			t.Errorf("children of the root: %+v", kids)
		}
		if _, err := tree.Get(ctx, tx, "zzzzzz"); !errors.Is(err, tree.ErrNotFound) {
			t.Errorf("Get(zzzzzz): %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIDsStartWithALetter: a silo id prefixes its objects' real GCE
// names, which must start with a letter.
func TestIDsStartWithALetter(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx store.Tx) error {
		root, err := tree.CreateRoot(ctx, tx, "root", tree.BackingSubnet, nil)
		if err != nil {
			return err
		}
		for i := 0; i < 64; i++ {
			c, err := tree.CreateChild(ctx, tx, root.ID, fmt.Sprintf("c%d", i), tree.BackingSubnet, nil)
			if err != nil {
				return err
			}
			if c.ID[0] < 'a' || c.ID[0] > 'z' {
				t.Errorf("silo id %q starts with %q", c.ID, c.ID[0])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
