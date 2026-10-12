// Package address is silod's address-space allocator: the aligned IPv4
// blocks handed down the silo tree, each silo's map of the space in its
// blocks, and the longest-prefix table origin identity reads
// (docs/silo/allocators.md, "Address space", SV-38 to SV-42 and the
// range checks of SV-44). IPv6 comes later.
//
// # The map
//
// Every range in a silo's blocks is exactly one of:
//
//   - delegated: one of a child's blocks (or, in the root's block, the
//     gateway or control range, held by the system entries
//     system:gateway and system:control so they are never handed out);
//   - own: a subnet's primary or secondary range, recorded by the facade
//     with SetOwn;
//   - free: everything else. A block freed in the last hour of database
//     time is free but cooling: nothing is placed in it until then
//     (call 24).
//
// Only blocks, own ranges and cooling ranges are stored. Free space is
// worked out from them, as maximal aligned regions, so free buddies
// merge back together by construction.
//
// A silo's own space is its blocks minus what it delegated. Every
// block lies inside one of its parent's, so the silo whose own space
// holds an address is the one whose block is the longest prefix
// containing it: see Table.
//
// # Transactions
//
// Every function runs inside a store.Tx the caller holds, checks
// everything before it writes, and appends what it changed to the
// change log (kind "address", keyed by the silo whose blocks or own
// ranges changed), which the fence and the gateway follow. Errors are
// *ledger.Error, with the codes the fence API returns.
package address

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The defaults for silo install (allocators.md, "Blocks and subnets";
// state.md, "The control subnet").
var (
	DefaultSpace   = netip.MustParsePrefix("10.0.0.0/8")
	DefaultGateway = netip.MustParsePrefix("10.255.255.0/24")
	DefaultControl = netip.MustParsePrefix("10.255.254.0/24")
)

// The system entries the gateway and control ranges are delegated to.
// They are not silos.
const (
	SystemGateway = "system:gateway"
	SystemControl = "system:control"
)

const (
	// SmallestBlock is the longest prefix a block may have (call 8).
	SmallestBlock = 28
	// SmallestSubnet is the longest prefix an own range may have: GCE's
	// smallest IPv4 primary range.
	SmallestSubnet = 29
	// DefaultBlockCap is how many blocks a silo may hold unless its
	// parent raised it, and MaxBlockCap how far it may be raised.
	DefaultBlockCap = 4
	MaxBlockCap     = 16
	// ReuseDelay is how long a freed block cools before it is placed
	// again, in database time (call 24).
	ReuseDelay = time.Hour
)

// ChangeKind is the change-log kind of every entry this package writes.
const ChangeKind = "address"

// The states of a range in a silo's map.
const (
	StateFree      = "free"
	StateDelegated = "delegated"
	StateOwn       = "own"
)

// Range is one entry of a silo's map.
type Range struct {
	Range netip.Prefix
	State string // StateFree, StateDelegated or StateOwn
	// Holder is, for a delegated range, the child (or system entry)
	// whose block it is, and for an own range the facade's object.
	Holder string
	// Cooling is, for a free range freed within ReuseDelay, when it may
	// be placed again. Zero otherwise.
	Cooling time.Time
}

// OwnRange is a subnet's primary or secondary range, named by the
// facade's object (a subnet, or a subnet and secondary range name).
// Region is the subnet's region: while a silo has an own range in a
// region, the region can't be removed from it (allocators.md, "Allowed
// regions", SV-37).
type OwnRange struct {
	Object string
	Range  netip.Prefix
	Region string
}

// Want is what a block is asked for with: a prefix length, for silod
// to place, or an exact range the parent names.
type Want struct {
	Bits  int          // a prefix length, used when Exact is the zero Prefix
	Exact netip.Prefix // an exact, aligned range
}

// Growth is what Grow did.
type Growth struct {
	Block   netip.Prefix // the widened block, or the new one
	Widened bool         // widened in place, rather than a block added
}

// occupant is a stored range in a holder's map.
type occupant struct {
	span
	state   string // StateDelegated, StateOwn, or StateFree for a cooling range
	who     string // the child, the object, or ""
	cooling time.Time
}

func errorf(code ledger.Code, format string, args ...any) *ledger.Error {
	return &ledger.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *ledger.Error {
	return errorf(ledger.InvalidArgument, format, args...)
}

// silo reads a silo's row, as NOT_FOUND if there is none.
func silo(ctx context.Context, tx store.Tx, id string) (*tree.Silo, error) {
	s, err := tree.Get(ctx, tx, id)
	if errors.Is(err, tree.ErrNotFound) {
		return nil, &ledger.Error{Code: ledger.NotFound, Message: "no silo " + id, Silo: id}
	}
	return s, err
}

// lock locks id's row and each ancestor's, root first, as every
// transaction that changes an allocation does (store.Tx.LockRows), and
// returns id's row.
func lock(ctx context.Context, tx store.Tx, id string) (*tree.Silo, error) {
	chain, err := tree.Ancestry(ctx, tx, id)
	if errors.Is(err, tree.ErrNotFound) {
		return nil, &ledger.Error{Code: ledger.NotFound, Message: "no silo " + id, Silo: id}
	}
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(chain))
	for i, s := range chain {
		keys[i] = s.ID
	}
	if err := tx.LockRows(ctx, "silos", "id", keys...); err != nil {
		return nil, err
	}
	return chain[len(chain)-1], nil
}

// name is how an error refers to a silo or system entry.
func name(ctx context.Context, tx store.Tx, id string) string {
	if s, err := tree.Get(ctx, tx, id); err == nil {
		return s.Path
	}
	return id
}

func logChange(ctx context.Context, tx store.Tx, key string, data map[string]string) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Append(ctx, ChangeKind, key, string(b))
	return err
}

// InitRoot gives the root its block: space, with the gateway and
// control ranges delegated to the system entries inside it. It is run
// once, at install.
func InitRoot(ctx context.Context, tx store.Tx, root string, space, gateway, control netip.Prefix) error {
	r, err := lock(ctx, tx, root)
	if err != nil {
		return err
	}
	if !r.IsRoot() {
		return invalid("silo %s is not the root", r.Path)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM address_blocks WHERE parent = ''`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errorf(ledger.FailedPrecondition, "the root already has its block")
	}
	sp, err := spanOf(space)
	if err != nil {
		return invalid("address space: %v", err)
	}
	if sp.bits > SmallestBlock {
		return invalid("address space %s is smaller than a /%d", sp, SmallestBlock)
	}
	gw, err := spanOf(gateway)
	if err != nil {
		return invalid("gateway range: %v", err)
	}
	ct, err := spanOf(control)
	if err != nil {
		return invalid("control range: %v", err)
	}
	if !sp.contains(gw) || !sp.contains(ct) {
		return invalid("the gateway range %s and control range %s must lie inside the address space %s", gw, ct, sp)
	}
	if gw.overlaps(ct) {
		return invalid("the gateway range %s and control range %s overlap", gw, ct)
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	for _, b := range []struct {
		silo, parent string
		s            span
	}{{root, "", sp}, {SystemGateway, root, gw}, {SystemControl, root, ct}} {
		if err := insertBlock(ctx, tx, b.silo, b.parent, b.s, now); err != nil {
			return err
		}
	}
	return logChange(ctx, tx, root, map[string]string{"op": "init", "block": sp.String(),
		"gateway": gw.String(), "control": ct.String()})
}

func insertBlock(ctx context.Context, tx store.Tx, silo, parent string, s span, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO address_blocks (silo, start, bits, parent, created_at) VALUES (?, ?, ?, ?, ?)`,
		silo, int64(s.start), s.bits, parent, store.Millis(now))
	return err
}

// blocks returns silo's blocks in address order.
func blocks(ctx context.Context, tx store.Tx, silo string) ([]span, error) {
	return spans(ctx, tx, `SELECT start, bits FROM address_blocks WHERE silo = ? ORDER BY start`, silo)
}

func spans(ctx context.Context, tx store.Tx, query string, args ...any) ([]span, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []span
	for rows.Next() {
		var start int64
		var s span
		if err := rows.Scan(&start, &s.bits); err != nil {
			return nil, err
		}
		s.start = uint32(start)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Blocks returns silo's blocks, in address order.
func Blocks(ctx context.Context, tx store.Tx, siloID string) ([]netip.Prefix, error) {
	if _, err := silo(ctx, tx, siloID); err != nil {
		return nil, err
	}
	bs, err := blocks(ctx, tx, siloID)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Prefix, len(bs))
	for i, b := range bs {
		out[i] = b.prefix()
	}
	return out, nil
}

// AllBlocks returns every holder's blocks, in address order: each
// silo's, and the gateway and control ranges under SystemGateway and
// SystemControl. The fence compiler reads it.
func AllBlocks(ctx context.Context, tx store.Tx) (map[string][]netip.Prefix, error) {
	rows, err := tx.Query(ctx, `SELECT silo, start, bits FROM address_blocks ORDER BY start, bits`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]netip.Prefix{}
	for rows.Next() {
		var holder string
		var start int64
		var s span
		if err := rows.Scan(&holder, &start, &s.bits); err != nil {
			return nil, err
		}
		s.start = uint32(start)
		out[holder] = append(out[holder], s.prefix())
	}
	return out, rows.Err()
}

// occupants returns everything stored in holder's map: the blocks
// delegated from it, its own ranges, and the ranges cooling in it as of
// now.
func occupants(ctx context.Context, tx store.Tx, holder string, now time.Time) ([]occupant, error) {
	var out []occupant
	read := func(state, query string, args ...any) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var start, at int64
			o := occupant{state: state}
			if err := rows.Scan(&start, &o.bits, &o.who, &at); err != nil {
				return err
			}
			o.start = uint32(start)
			if state == StateFree {
				o.who, o.cooling = "", store.Time(at).Add(ReuseDelay)
			}
			out = append(out, o)
		}
		return rows.Err()
	}
	if err := read(StateDelegated, `SELECT start, bits, silo, 0 FROM address_blocks WHERE parent = ?`, holder); err != nil {
		return nil, err
	}
	if err := read(StateOwn, `SELECT start, bits, object, 0 FROM address_own WHERE silo = ?`, holder); err != nil {
		return nil, err
	}
	if err := read(StateFree, `SELECT start, bits, '', freed_at FROM address_freed WHERE holder = ? AND freed_at > ?`,
		holder, store.Millis(now.Add(-ReuseDelay))); err != nil {
		return nil, err
	}
	return out, nil
}

func busySpans(occ []occupant) []span {
	out := make([]span, len(occ))
	for i, o := range occ {
		out[i] = o.span
	}
	return out
}

// Map returns silo's space map: every range in its blocks, each
// delegated, own or free, in address order. Free space is given as
// maximal aligned regions, and a cooling range as its own entry.
func Map(ctx context.Context, tx store.Tx, siloID string) ([]Range, error) {
	if _, err := silo(ctx, tx, siloID); err != nil {
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
	var all []occupant
	for _, o := range occ {
		for _, b := range bs {
			if b.contains(o.span) {
				all = append(all, o)
				break
			}
		}
	}
	busy := busySpans(occ)
	for _, b := range bs {
		for _, f := range freeRegions(b, busy) {
			all = append(all, occupant{span: f, state: StateFree})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start < all[j].start })
	out := make([]Range, len(all))
	for i, o := range all {
		out[i] = Range{Range: o.prefix(), State: o.state, Holder: o.who, Cooling: o.cooling}
	}
	return out, nil
}

// BlockCap is how many blocks silo may hold.
func BlockCap(ctx context.Context, tx store.Tx, siloID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT max_blocks FROM address_caps WHERE silo = ?`, siloID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultBlockCap, nil
	}
	return n, err
}

// SetBlockCap sets how many blocks child may hold, which its parent
// may raise to MaxBlockCap. It can't go below what child holds.
func SetBlockCap(ctx context.Context, tx store.Tx, child string, n int) error {
	c, err := lock(ctx, tx, child)
	if err != nil {
		return err
	}
	if c.IsRoot() {
		return invalid("the root has no block cap")
	}
	if n < 1 || n > MaxBlockCap {
		return invalid("block cap %d: want 1 to %d", n, MaxBlockCap)
	}
	bs, err := blocks(ctx, tx, child)
	if err != nil {
		return err
	}
	if len(bs) > n {
		return errorf(ledger.FailedPrecondition, "silo %s holds %d blocks (%s), more than a cap of %d", c.Path, len(bs), list(bs), n)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM address_caps WHERE silo = ?`, child); err != nil {
		return err
	}
	if n != DefaultBlockCap {
		if _, err := tx.Exec(ctx, `INSERT INTO address_caps (silo, max_blocks) VALUES (?, ?)`, child, n); err != nil {
			return err
		}
	}
	return logChange(ctx, tx, child, map[string]string{"op": "cap", "cap": fmt.Sprint(n)})
}

// Delegate carves a new block for child out of its parent's map. With
// want.Bits, silod places it (see placement); with want.Exact, the
// range must be aligned, inside one of the parent's blocks, and free.
// It fails with RESOURCE_EXHAUSTED when no slot is free, naming the
// largest prefix that would fit, or when child is at its block cap.
func Delegate(ctx context.Context, tx store.Tx, child string, want Want) (netip.Prefix, error) {
	c, err := lock(ctx, tx, child)
	if err != nil {
		return netip.Prefix{}, err
	}
	if c.IsRoot() {
		return netip.Prefix{}, invalid("the root's block is the address space set at install")
	}
	var exact *span
	if want.Exact.IsValid() {
		s, err := spanOf(want.Exact)
		if err != nil {
			return netip.Prefix{}, invalid("%v", err)
		}
		exact, want.Bits = &s, s.bits
	} else if want.Exact != (netip.Prefix{}) {
		return netip.Prefix{}, invalid("%q is not a valid CIDR range", want.Exact.String())
	}
	if want.Bits < 0 || want.Bits > SmallestBlock {
		return netip.Prefix{}, invalid("a block is a /%d or larger, not a /%d", SmallestBlock, want.Bits)
	}
	limit, err := BlockCap(ctx, tx, child)
	if err != nil {
		return netip.Prefix{}, err
	}
	held, err := blocks(ctx, tx, child)
	if err != nil {
		return netip.Prefix{}, err
	}
	if len(held) >= limit {
		return netip.Prefix{}, errorf(ledger.ResourceExhausted,
			"silo %s holds %d blocks (%s), its cap; its parent can raise the cap to %d", c.Path, len(held), list(held), MaxBlockCap)
	}
	s, err := carve(ctx, tx, c.Parent, exact, want.Bits)
	if err != nil {
		return netip.Prefix{}, err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return netip.Prefix{}, err
	}
	if err := insertBlock(ctx, tx, child, c.Parent, s, now); err != nil {
		return netip.Prefix{}, err
	}
	if err := logChange(ctx, tx, child, map[string]string{"op": "delegate", "block": s.String(), "parent": c.Parent}); err != nil {
		return netip.Prefix{}, err
	}
	return s.prefix(), nil
}

// carve finds a free range for a new block in parent's map: exact if
// given, else a slot of bits placed by preference.
func carve(ctx context.Context, tx store.Tx, parent string, exact *span, bits int) (span, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return span{}, err
	}
	pbs, err := blocks(ctx, tx, parent)
	if err != nil {
		return span{}, err
	}
	occ, err := occupants(ctx, tx, parent, now)
	if err != nil {
		return span{}, err
	}
	if exact != nil {
		inside := false
		for _, b := range pbs {
			inside = inside || b.contains(*exact)
		}
		if !inside {
			return span{}, invalid("%s is not inside the blocks of %s (%s)", exact, name(ctx, tx, parent), list(pbs))
		}
		for _, o := range occ {
			if o.overlaps(*exact) {
				return span{}, errorf(ledger.FailedPrecondition, "%s is not free in the map of %s: it overlaps %s",
					exact, name(ctx, tx, parent), describe(ctx, tx, o))
			}
		}
		return *exact, nil
	}
	var free []span
	for _, b := range pbs {
		free = append(free, freeRegions(b, busySpans(occ))...)
	}
	s, ok := placement(free, bits)
	if !ok {
		fits := "nothing is free"
		if l := largest(free); l >= 0 && l <= SmallestBlock {
			fits = fmt.Sprintf("the largest that would fit is a /%d", l)
		} else if l >= 0 {
			fits = fmt.Sprintf("not even a /%d is free", SmallestBlock)
		}
		return span{}, errorf(ledger.ResourceExhausted, "no free /%d in the blocks of %s (%s): %s",
			bits, name(ctx, tx, parent), list(pbs), fits)
	}
	return s, nil
}

func describe(ctx context.Context, tx store.Tx, o occupant) string {
	switch {
	case o.state == StateDelegated:
		return fmt.Sprintf("%s, delegated to %s", o.span, name(ctx, tx, o.who))
	case o.state == StateOwn:
		return fmt.Sprintf("%s, own range %s", o.span, o.who)
	}
	return fmt.Sprintf("%s, freed and not reusable until %s", o.span, o.cooling.Format(time.RFC3339))
}

// Grow widens one of child's blocks to a /to (call 7). block names
// which; the zero Prefix means child's only block. It widens in place
// when the rest of the /to is free in the parent's map, or holds only
// child's other blocks, which merge into it. Otherwise it fails with
// FAILED_PRECONDITION, unless addBlock is set, when it delegates a new
// /to wherever one fits instead.
func Grow(ctx context.Context, tx store.Tx, child string, block netip.Prefix, to int, addBlock bool) (Growth, error) {
	c, err := lock(ctx, tx, child)
	if err != nil {
		return Growth{}, err
	}
	if c.IsRoot() {
		return Growth{}, invalid("the root's block is the address space set at install")
	}
	held, err := blocks(ctx, tx, child)
	if err != nil {
		return Growth{}, err
	}
	var b span
	switch {
	case block.IsValid():
		s, err := spanOf(block)
		if err != nil {
			return Growth{}, invalid("%v", err)
		}
		found := false
		for _, h := range held {
			found = found || h == s
		}
		if !found {
			return Growth{}, errorf(ledger.NotFound, "%s is not a block of %s (%s)", s, c.Path, list(held))
		}
		b = s
	case len(held) == 1:
		b = held[0]
	default:
		return Growth{}, invalid("silo %s holds %d blocks (%s): name the one to grow", c.Path, len(held), list(held))
	}
	if to >= b.bits || to < 0 {
		return Growth{}, invalid("%s can only grow to a shorter prefix than /%d, not /%d: blocks are not narrowed", b, b.bits, to)
	}

	now, err := tx.Now(ctx)
	if err != nil {
		return Growth{}, err
	}
	target := b.widen(to)
	pbs, err := blocks(ctx, tx, c.Parent)
	if err != nil {
		return Growth{}, err
	}
	occ, err := occupants(ctx, tx, c.Parent, now)
	if err != nil {
		return Growth{}, err
	}
	why := ""
	inside := false
	for _, p := range pbs {
		inside = inside || p.contains(target)
	}
	var merge []span
	if !inside {
		why = fmt.Sprintf("%s is not inside the blocks of %s (%s)", target, name(ctx, tx, c.Parent), list(pbs))
	} else {
		for _, o := range occ {
			if !o.overlaps(target) || (o.state == StateDelegated && o.who == child && o.span == b) {
				continue
			}
			if o.state == StateDelegated && o.who == child {
				merge = append(merge, o.span)
				continue
			}
			why = fmt.Sprintf("the rest of %s is not free in the map of %s: it holds %s", target, name(ctx, tx, c.Parent), describe(ctx, tx, o))
			break
		}
	}
	if why == "" {
		for _, m := range merge {
			if _, err := tx.Exec(ctx, `DELETE FROM address_blocks WHERE silo = ? AND start = ?`, child, int64(m.start)); err != nil {
				return Growth{}, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE address_blocks SET start = ?, bits = ? WHERE silo = ? AND start = ?`,
			int64(target.start), target.bits, child, int64(b.start)); err != nil {
			return Growth{}, err
		}
		if err := logChange(ctx, tx, child, map[string]string{"op": "widen", "block": b.String(), "to": target.String()}); err != nil {
			return Growth{}, err
		}
		return Growth{Block: target.prefix(), Widened: true}, nil
	}
	if !addBlock {
		return Growth{}, errorf(ledger.FailedPrecondition, "%s can't widen in place to %s: %s; ask to add a block instead", b, target, why)
	}
	p, err := Delegate(ctx, tx, child, Want{Bits: to})
	if err != nil {
		return Growth{}, err
	}
	return Growth{Block: p}, nil
}

// ReleaseBlock gives one of silo's blocks back to its parent. Nothing
// in it may be own or delegated. The block cools in the parent's map
// for ReuseDelay.
func ReleaseBlock(ctx context.Context, tx store.Tx, siloID string, block netip.Prefix) error {
	s, err := lock(ctx, tx, siloID)
	if err != nil {
		return err
	}
	if s.IsRoot() {
		return invalid("the root's block is the address space set at install")
	}
	b, err := spanOf(block)
	if err != nil {
		return invalid("%v", err)
	}
	held, err := blocks(ctx, tx, siloID)
	if err != nil {
		return err
	}
	found := false
	for _, h := range held {
		found = found || h == b
	}
	if !found {
		return errorf(ledger.NotFound, "%s is not a block of %s (%s)", b, s.Path, list(held))
	}
	if err := checkEmpty(ctx, tx, s, []span{b}); err != nil {
		return err
	}
	if err := free(ctx, tx, s, b); err != nil {
		return err
	}
	return logChange(ctx, tx, siloID, map[string]string{"op": "release", "block": b.String(), "parent": s.Parent})
}

// ReleaseSilo returns all of a deleted silo's blocks to its parent's
// map, where they cool for ReuseDelay. Deletion is bottom-up, so
// nothing in them may still be own or delegated.
func ReleaseSilo(ctx context.Context, tx store.Tx, siloID string) error {
	s, err := lock(ctx, tx, siloID)
	if err != nil {
		return err
	}
	if s.IsRoot() {
		return invalid("the root's block is the address space set at install")
	}
	held, err := blocks(ctx, tx, siloID)
	if err != nil {
		return err
	}
	if err := checkEmpty(ctx, tx, s, held); err != nil {
		return err
	}
	for _, b := range held {
		if err := free(ctx, tx, s, b); err != nil {
			return err
		}
	}
	for _, q := range []string{`DELETE FROM address_caps WHERE silo = ?`, `DELETE FROM address_freed WHERE holder = ?`} {
		if _, err := tx.Exec(ctx, q, siloID); err != nil {
			return err
		}
	}
	return logChange(ctx, tx, siloID, map[string]string{"op": "release-silo", "blocks": list(held), "parent": s.Parent})
}

// checkEmpty fails if anything in bs is own or delegated.
func checkEmpty(ctx context.Context, tx store.Tx, s *tree.Silo, bs []span) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	occ, err := occupants(ctx, tx, s.ID, now)
	if err != nil {
		return err
	}
	for _, o := range occ {
		if o.state == StateFree {
			continue
		}
		for _, b := range bs {
			if b.contains(o.span) {
				return errorf(ledger.FailedPrecondition, "block %s of %s is not empty: it holds %s", b, s.Path, describe(ctx, tx, o))
			}
		}
	}
	return nil
}

// free moves b from s to cooling in s's parent's map.
func free(ctx context.Context, tx store.Tx, s *tree.Silo, b span) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM address_blocks WHERE silo = ? AND start = ?`, s.ID, int64(b.start)); err != nil {
		return err
	}
	// What cooled inside b in s's own map is covered by b cooling now.
	if _, err := tx.Exec(ctx, `DELETE FROM address_freed WHERE holder = ? AND start >= ? AND start < ?`,
		s.ID, int64(b.start), int64(b.end())); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM address_freed WHERE holder = ? AND freed_at <= ?`,
		s.Parent, store.Millis(now.Add(-ReuseDelay))); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO address_freed (holder, start, bits, freed_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (holder, start, bits) DO UPDATE SET freed_at = excluded.freed_at`,
		s.Parent, int64(b.start), b.bits, store.Millis(now))
	return err
}
