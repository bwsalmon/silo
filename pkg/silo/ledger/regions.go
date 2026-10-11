package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// A GCE region name: us-central1, europe-west4, me-central2.
var regionName = regexp.MustCompile(`^[a-z]+(-[a-z]+)+[0-9]+$`)

// Regions returns a silo's allowed regions, sorted.
func Regions(ctx context.Context, tx store.Tx, silo string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT region FROM silo_regions WHERE silo = ? ORDER BY region`, silo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func hasRegion(ctx context.Context, tx store.Tx, silo, region string) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM silo_regions WHERE silo = ? AND region = ?`, silo, region).Scan(&n)
	return n > 0, err
}

// SetRegions sets a silo's allowed regions (allocators.md, "Allowed
// regions"):
//
//   - a child's regions must be a subset of its parent's, or it is
//     LOOSER_THAN_PARENT. The root's are what install was given;
//   - removing a region that the silo or any descendant still holds
//     quota in (granted, delegated or used), or still has objects in,
//     is FAILED_PRECONDITION. Otherwise the region is removed from the
//     descendants too, which keeps every child's regions a subset of
//     its parent's;
//   - adding a region grants nothing.
func SetRegions(ctx context.Context, tx store.Tx, silo string, regions []string) error {
	s, err := tree.Get(ctx, tx, silo)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, r := range regions {
		if !regionName.MatchString(r) {
			return invalid("%q is not a region name", r)
		}
		want[r] = true
	}
	if !s.IsRoot() {
		parent, err := Regions(ctx, tx, s.Parent)
		if err != nil {
			return err
		}
		has := map[string]bool{}
		for _, r := range parent {
			has[r] = true
		}
		var extra []string
		for r := range want {
			if !has[r] {
				extra = append(extra, r)
			}
		}
		if len(extra) > 0 {
			sort.Strings(extra)
			return &Error{Code: LooserThanParent, Silo: silo,
				Message: fmt.Sprintf("%s's parent is not allowed %s", s.Path, strings.Join(extra, ", "))}
		}
	}
	cur, err := Regions(ctx, tx, silo)
	if err != nil {
		return err
	}
	var removed, added []string
	for _, r := range cur {
		if !want[r] {
			removed = append(removed, r)
		}
		delete(want, r)
	}
	for r := range want {
		added = append(added, r)
	}
	sort.Strings(added)
	if len(removed) == 0 && len(added) == 0 {
		return nil
	}

	sub, err := tree.Subtree(ctx, tx, silo)
	if err != nil {
		return err
	}
	in := "?" + strings.Repeat(", ?", len(sub)-1)
	for _, r := range removed {
		args := []any{r}
		for _, id := range sub {
			args = append(args, id)
		}
		var silo, kind, class string
		var n int
		err := tx.QueryRow(ctx, `SELECT COUNT(*), COALESCE(MIN(silo), ''), COALESCE(MIN(kind), ''), COALESCE(MIN(class), '')
			FROM quota WHERE region = ? AND silo IN (`+in+`)
			AND (granted <> 0 OR delegated <> 0 OR used <> 0 OR consumed <> 0)`, args...).Scan(&n, &silo, &kind, &class)
		if err != nil {
			return err
		}
		if n > 0 {
			return &Error{Code: FailedPrecondition, Silo: silo, Key: Key{Kind: kind, Class: class, Region: r},
				Message: fmt.Sprintf("can't remove %s from %s: %s or a descendant still holds quota there", r, s.Path, s.Path)}
		}
		err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM charges WHERE region = ? AND home IN (`+in+`)`, args...).Scan(&n)
		if err != nil {
			return err
		}
		if n > 0 {
			return &Error{Code: FailedPrecondition, Silo: silo, Key: Key{Region: r},
				Message: fmt.Sprintf("can't remove %s from %s: %s or a descendant still has objects there", r, s.Path, s.Path)}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM silo_regions WHERE region = ? AND silo IN (`+in+`)`, args...); err != nil {
			return err
		}
	}
	for _, r := range added {
		if _, err := tx.Exec(ctx, `INSERT INTO silo_regions (silo, region) VALUES (?, ?)`, silo, r); err != nil {
			return err
		}
	}
	data, _ := json.Marshal(map[string]any{"added": added, "removed": removed})
	_, err = tx.Append(ctx, "regions", silo, string(data))
	return err
}
