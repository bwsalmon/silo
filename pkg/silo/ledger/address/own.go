package address

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// The checks the facade makes on a subnet's ranges (allocators.md,
// "Checks the facade makes", checks 2 to 5). Each failure is
// INVALID_ARGUMENT, which the facade returns as GCE's 400 invalid, and
// its message names the silo's blocks.

// CheckOwn checks ranges as silo's own ranges: each a valid, aligned
// IPv4 CIDR, a /29 or larger, inside one of silo's blocks, overlapping
// nothing delegated or cooling, no other own range of silo's, and none
// of the others in ranges. A range whose object is already recorded is
// checked as replacing it, so a subnet's primary and secondary ranges
// are checked together, at insert or on a patch.
func CheckOwn(ctx context.Context, tx store.Tx, siloID string, ranges []OwnRange) error {
	_, err := checkOwn(ctx, tx, siloID, ranges)
	return err
}

func checkOwn(ctx context.Context, tx store.Tx, siloID string, ranges []OwnRange) ([]span, error) {
	s, err := silo(ctx, tx, siloID)
	if err != nil {
		return nil, err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	bs, err := blocks(ctx, tx, siloID)
	if err != nil {
		return nil, err
	}
	occ, err := occupants(ctx, tx, siloID, now)
	if err != nil {
		return nil, err
	}
	asked := map[string]bool{}
	for _, r := range ranges {
		if r.Object == "" {
			return nil, invalid("an own range needs an object")
		}
		if asked[r.Object] {
			return nil, invalid("object %s is given twice", r.Object)
		}
		asked[r.Object] = true
	}
	mapOf := func() string { return mapString(ctx, tx, s, bs, occ) }
	out := make([]span, len(ranges))
	for i, r := range ranges {
		sp, err := spanOf(r.Range)
		if err != nil {
			return nil, invalid("%s: %v", r.Object, err)
		}
		if sp.bits > SmallestSubnet {
			return nil, invalid("%s: %s is smaller than a /%d", r.Object, sp, SmallestSubnet)
		}
		inside := false
		for _, b := range bs {
			inside = inside || b.contains(sp)
		}
		if !inside {
			return nil, invalid("%s: %s is not inside the blocks of silo %s; %s", r.Object, sp, s.Path, mapOf())
		}
		for _, o := range occ {
			if o.overlaps(sp) && !(o.state == StateOwn && asked[o.who]) {
				return nil, invalid("%s: %s overlaps %s in silo %s; %s", r.Object, sp, describe(ctx, tx, o), s.Path, mapOf())
			}
		}
		for j := range i {
			if out[j].overlaps(sp) {
				return nil, invalid("%s: %s overlaps %s, given for %s", r.Object, sp, out[j], ranges[j].Object)
			}
		}
		out[i] = sp
	}
	return out, nil
}

// mapString names a silo's blocks and what it delegated, for errors.
func mapString(ctx context.Context, tx store.Tx, s *tree.Silo, bs []span, occ []occupant) string {
	out := fmt.Sprintf("its blocks are %s", list(bs))
	var del []string
	for _, o := range occ {
		if o.state == StateDelegated {
			del = append(del, fmt.Sprintf("%s to %s", o.span, name(ctx, tx, o.who)))
		}
	}
	if len(del) > 0 {
		out += ", less " + strings.Join(del, ", ")
	}
	return out
}

// SetOwn records ranges as silo's own ranges, after CheckOwn. A range
// whose object is already recorded replaces it.
func SetOwn(ctx context.Context, tx store.Tx, siloID string, ranges []OwnRange) error {
	if _, err := lock(ctx, tx, siloID); err != nil {
		return err
	}
	sp, err := checkOwn(ctx, tx, siloID, ranges)
	if err != nil {
		return err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	for i, r := range ranges {
		if _, err := tx.Exec(ctx, `DELETE FROM address_own WHERE silo = ? AND object = ?`, siloID, r.Object); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO address_own (silo, object, start, bits, region, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			siloID, r.Object, int64(sp[i].start), sp[i].bits, r.Region, store.Millis(now)); err != nil {
			return err
		}
		if err := logChange(ctx, tx, siloID, map[string]string{"op": "own", "object": r.Object, "range": sp[i].String()}); err != nil {
			return err
		}
	}
	return nil
}

// Own returns silo's own range for object.
func Own(ctx context.Context, tx store.Tx, siloID, object string) (netip.Prefix, error) {
	var start int64
	var bits int
	err := tx.QueryRow(ctx, `SELECT start, bits FROM address_own WHERE silo = ? AND object = ?`, siloID, object).Scan(&start, &bits)
	if errors.Is(err, sql.ErrNoRows) {
		return netip.Prefix{}, errorf(ledger.NotFound, "silo %s has no own range %s", name(ctx, tx, siloID), object)
	}
	if err != nil {
		return netip.Prefix{}, err
	}
	return span{start: uint32(start), bits: bits}.prefix(), nil
}

// CheckExpand checks the new range of expandIpCidrRange on object: it
// must contain the current range, be wider, and pass CheckOwn.
func CheckExpand(ctx context.Context, tx store.Tx, siloID, object string, to netip.Prefix) error {
	cur, err := Own(ctx, tx, siloID, object)
	if err != nil {
		return err
	}
	t, err := spanOf(to)
	if err != nil {
		return invalid("%s: %v", object, err)
	}
	c, _ := spanOf(cur)
	if t.bits >= c.bits || !t.contains(c) {
		return invalid("%s: %s does not expand %s: it must be a wider range that contains it", object, t, c)
	}
	return CheckOwn(ctx, tx, siloID, []OwnRange{{Object: object, Range: to}})
}

// Expand widens object's own range to to, after CheckExpand.
func Expand(ctx context.Context, tx store.Tx, siloID, object string, to netip.Prefix) error {
	if _, err := lock(ctx, tx, siloID); err != nil {
		return err
	}
	if err := CheckExpand(ctx, tx, siloID, object, to); err != nil {
		return err
	}
	var region string
	if err := tx.QueryRow(ctx, `SELECT region FROM address_own WHERE silo = ? AND object = ?`, siloID, object).Scan(&region); err != nil {
		return err
	}
	return SetOwn(ctx, tx, siloID, []OwnRange{{Object: object, Range: to, Region: region}})
}

// RemoveOwn forgets silo's own ranges for objects, as when a subnet is
// deleted. An object with none is ignored.
func RemoveOwn(ctx context.Context, tx store.Tx, siloID string, objects ...string) error {
	if _, err := lock(ctx, tx, siloID); err != nil {
		return err
	}
	for _, o := range objects {
		n, err := tx.Exec(ctx, `DELETE FROM address_own WHERE silo = ? AND object = ?`, siloID, o)
		if err != nil {
			return err
		}
		if n > 0 {
			if err := logChange(ctx, tx, siloID, map[string]string{"op": "own-removed", "object": o}); err != nil {
				return err
			}
		}
	}
	return nil
}
