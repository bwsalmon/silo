package address_test

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

var ctx = context.Background()

// clockDB is a store whose database clock runs ahead by skew, so a test
// can step past the reuse delay. Everything in package address reads
// time through Tx.Now, so this is the clock it sees.
type clockDB struct {
	store.Store
	skew *atomic.Int64 // nanoseconds
}

type clockTx struct {
	store.Tx
	skew *atomic.Int64
}

func (c clockDB) Tx(ctx context.Context, fn func(store.Tx) error) error {
	return c.Store.Tx(ctx, func(tx store.Tx) error { return fn(clockTx{tx, c.skew}) })
}

func (c clockTx) Now(ctx context.Context) (time.Time, error) {
	t, err := c.Tx.Now(ctx)
	return t.Add(time.Duration(c.skew.Load())), err
}

func (c clockDB) advance(d time.Duration) { c.skew.Add(int64(d)) }

func newDB(t *testing.T) clockDB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, db.Latest()); err != nil {
		t.Fatal(err)
	}
	return clockDB{Store: db, skew: new(atomic.Int64)}
}

func do(t *testing.T, db store.Store, fn func(tx store.Tx) error) {
	t.Helper()
	if err := db.Tx(ctx, fn); err != nil {
		t.Fatal(err)
	}
}

func want(t *testing.T, err error, code ledger.Code, contains ...string) {
	t.Helper()
	if ledger.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("err = %v, want it to mention %q", err, c)
		}
	}
}

var p = netip.MustParsePrefix

// world is a root over 10.0.0.0/16, with the control range at
// 10.0.254.0/24 and the gateway's at 10.0.255.0/24.
type world struct {
	db   clockDB
	root string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{db: newDB(t)}
	do(t, w.db, func(tx store.Tx) error {
		r, err := tree.CreateRoot(ctx, tx, "root", nil)
		if err != nil {
			return err
		}
		w.root = r.ID
		return address.InitRoot(ctx, tx, r.ID, p("10.0.0.0/16"), p("10.0.255.0/24"), p("10.0.254.0/24"))
	})
	return w
}

func (w *world) child(t *testing.T, parent, name string) string {
	t.Helper()
	var id string
	do(t, w.db, func(tx store.Tx) error {
		c, err := tree.CreateChild(ctx, tx, parent, name, tree.BackingSubnet, nil)
		if err != nil {
			return err
		}
		id = c.ID
		return nil
	})
	return id
}

func (w *world) delegate(child string, want address.Want) (netip.Prefix, error) {
	var got netip.Prefix
	err := w.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		got, err = address.Delegate(ctx, tx, child, want)
		return err
	})
	return got, err
}

func (w *world) mustDelegate(t *testing.T, child string, want address.Want, expect string) {
	t.Helper()
	got, err := w.delegate(child, want)
	if err != nil {
		t.Fatal(err)
	}
	if expect != "" && got != p(expect) {
		t.Fatalf("delegated %v, want %s", got, expect)
	}
}

func (w *world) blocks(t *testing.T, silo string) []netip.Prefix {
	t.Helper()
	var bs []netip.Prefix
	do(t, w.db, func(tx store.Tx) error {
		var err error
		bs, err = address.Blocks(ctx, tx, silo)
		return err
	})
	return bs
}

func (w *world) mapOf(t *testing.T, silo string) []address.Range {
	t.Helper()
	var m []address.Range
	do(t, w.db, func(tx store.Tx) error {
		var err error
		m, err = address.Map(ctx, tx, silo)
		return err
	})
	checkMap(t, w.blocks(t, silo), m)
	return m
}

// checkMap checks that m tiles blocks exactly: every range inside a
// block, none overlapping, and every address of each block covered.
func checkMap(t *testing.T, blocks []netip.Prefix, m []address.Range) {
	t.Helper()
	var total, want uint64
	for _, b := range blocks {
		want += 1 << (32 - b.Bits())
	}
	for i, r := range m {
		total += 1 << (32 - r.Range.Bits())
		inside := false
		for _, b := range blocks {
			inside = inside || (b.Bits() <= r.Range.Bits() && b.Contains(r.Range.Addr()))
		}
		if !inside {
			t.Errorf("map range %v is outside the blocks %v", r.Range, blocks)
		}
		for _, o := range m[:i] {
			if o.Range.Overlaps(r.Range) {
				t.Errorf("map ranges %v and %v overlap", o.Range, r.Range)
			}
		}
	}
	if total != want {
		t.Errorf("map covers %d addresses of %d: %v", total, want, m)
	}
}

func TestRootBlock(t *testing.T) {
	w := newWorld(t)
	m := w.mapOf(t, w.root)
	got := map[string]string{}
	for _, r := range m {
		if r.State == address.StateDelegated {
			got[r.Range.String()] = r.Holder
		}
	}
	if got["10.0.255.0/24"] != address.SystemGateway || got["10.0.254.0/24"] != address.SystemControl || len(got) != 2 {
		t.Errorf("root's delegated ranges = %v, want the gateway and control ranges", got)
	}
	a := w.child(t, w.root, "a")
	_, err := w.delegate(a, address.Want{Exact: p("10.0.255.0/24")})
	want(t, err, ledger.FailedPrecondition, address.SystemGateway)
	_, err = w.delegate(a, address.Want{Exact: p("10.0.254.0/23")})
	want(t, err, ledger.FailedPrecondition)
	do(t, w.db, func(tx store.Tx) error {
		err := address.InitRoot(ctx, tx, w.root, p("10.0.0.0/16"), p("10.0.255.0/24"), p("10.0.254.0/24"))
		want(t, err, ledger.FailedPrecondition)
		return nil
	})

	// The defaults: 10.0.0.0/8 less the two /24s at its top.
	db := newDB(t)
	do(t, db, func(tx store.Tx) error {
		r, err := tree.CreateRoot(ctx, tx, "root", nil)
		if err != nil {
			return err
		}
		want(t, address.InitRoot(ctx, tx, r.ID, p("10.0.0.0/8"), p("11.0.0.0/24"), address.DefaultControl), ledger.InvalidArgument)
		want(t, address.InitRoot(ctx, tx, r.ID, p("10.0.0.0/8"), p("10.255.255.0/24"), p("10.255.255.0/25")), ledger.InvalidArgument)
		return address.InitRoot(ctx, tx, r.ID, address.DefaultSpace, address.DefaultGateway, address.DefaultControl)
	})
}

// TestPlacement walks the buddy allocator's preferences.
func TestPlacement(t *testing.T) {
	run := func(t *testing.T) []netip.Prefix {
		w := newWorld(t)
		a, b, c, d := w.child(t, w.root, "a"), w.child(t, w.root, "b"), w.child(t, w.root, "c"), w.child(t, w.root, "d")
		var out []netip.Prefix
		// The free regions are 10.0.0.0/17, .128/18, .192/19, .224/20,
		// .240/21, .248/22 and .252/23. Every region of /19 or more
		// gives a /20 a free buddy, and the smallest of them is .192/19.
		w.mustDelegate(t, a, address.Want{Bits: 20}, "10.0.192.0/20")
		// .208/20 fits exactly but its buddy is taken. A slot with a
		// free buddy comes first: the smallest region with one is .128/18.
		w.mustDelegate(t, b, address.Want{Bits: 20}, "10.0.128.0/20")
		// Exact fits (.248/22) lose to the smallest region with room for
		// a buddy: .240/21.
		w.mustDelegate(t, c, address.Want{Bits: 22}, "10.0.240.0/22")
		// A /23: the smallest regions with room for its buddy are the
		// /22s, .244 and .248, and the lower wins.
		w.mustDelegate(t, d, address.Want{Bits: 23}, "10.0.244.0/23")
		for _, s := range []string{a, b, c, d} {
			out = append(out, w.blocks(t, s)...)
		}
		w.mapOf(t, w.root)

		// Too big: the largest free region is the /17.
		e := w.child(t, w.root, "e")
		_, err := w.delegate(e, address.Want{Bits: 16})
		want(t, err, ledger.ResourceExhausted, "/17")
		_, err = w.delegate(e, address.Want{Bits: 29})
		want(t, err, ledger.InvalidArgument)

		// Exact ranges: aligned, inside the parent's block, and free.
		_, err = w.delegate(e, address.Want{Exact: netip.PrefixFrom(netip.MustParseAddr("10.0.1.0"), 20)})
		want(t, err, ledger.InvalidArgument, "aligned")
		_, err = w.delegate(e, address.Want{Exact: p("10.1.0.0/20")})
		want(t, err, ledger.InvalidArgument, "10.0.0.0/16")
		_, err = w.delegate(e, address.Want{Exact: p("10.0.192.0/21")})
		want(t, err, ledger.FailedPrecondition, "/a")
		_, err = w.delegate(e, address.Want{Exact: p("10.0.0.0/29")})
		want(t, err, ledger.InvalidArgument)
		_, err = w.delegate(e, address.Want{Exact: p("fd00::/48")})
		want(t, err, ledger.InvalidArgument, "IPv4")
		w.mustDelegate(t, e, address.Want{Exact: p("10.0.16.0/20")}, "10.0.16.0/20")
		w.mapOf(t, w.root)
		return out
	}
	first := run(t)
	if again := run(t); len(again) != len(first) {
		t.Fatalf("placement differs: %v then %v", first, again)
	} else {
		for i := range first {
			if first[i] != again[i] {
				t.Errorf("placement differs: %v then %v", first, again)
			}
		}
	}

	// A full block: nothing of the smallest size left.
	w := newWorld(t)
	f := w.child(t, w.root, "f")
	do(t, w.db, func(tx store.Tx) error { return address.SetBlockCap(ctx, tx, f, address.MaxBlockCap) })
	w.mustDelegate(t, f, address.Want{Bits: 17}, "10.0.0.0/17")
	w.mustDelegate(t, f, address.Want{Bits: 18}, "10.0.128.0/18")
	g := w.child(t, f, "g")
	w.mustDelegate(t, g, address.Want{Bits: 18}, "10.0.0.0/18")
	w.mustDelegate(t, g, address.Want{Bits: 18}, "10.0.64.0/18")
	w.mustDelegate(t, g, address.Want{Bits: 18}, "10.0.128.0/18")
	_, err := w.delegate(g, address.Want{Bits: 28})
	want(t, err, ledger.ResourceExhausted, "nothing is free")
}

func TestGrow(t *testing.T) {
	w := newWorld(t)
	a, b := w.child(t, w.root, "a"), w.child(t, w.root, "b")
	grow := func(silo string, block string, to int, add bool) (address.Growth, error) {
		var g address.Growth
		err := w.db.Tx(ctx, func(tx store.Tx) error {
			var bp netip.Prefix
			if block != "" {
				bp = p(block)
			}
			var err error
			g, err = address.Grow(ctx, tx, silo, bp, to, add)
			return err
		})
		return g, err
	}

	w.mustDelegate(t, a, address.Want{Bits: 20}, "10.0.192.0/20")
	g, err := grow(a, "", 19, false)
	if err != nil || !g.Widened || g.Block != p("10.0.192.0/19") {
		t.Fatalf("grow a to /19 = %+v, %v; want widened in place to 10.0.192.0/19", g, err)
	}
	// The /18 around it holds the gateway and control ranges.
	_, err = grow(a, "", 18, false)
	want(t, err, ledger.FailedPrecondition, address.SystemControl)
	// No narrowing.
	_, err = grow(a, "", 20, false)
	want(t, err, ledger.InvalidArgument)
	// With add-block, a new /18 wherever it fits.
	g, err = grow(a, "", 18, true)
	if err != nil || g.Widened || g.Block != p("10.0.0.0/18") {
		t.Fatalf("grow a to /18 with add-block = %+v, %v; want a new block 10.0.0.0/18", g, err)
	}
	if bs := w.blocks(t, a); len(bs) != 2 {
		t.Fatalf("a's blocks = %v, want two", bs)
	}
	// Two blocks: name the one to grow.
	_, err = grow(a, "", 17, false)
	want(t, err, ledger.InvalidArgument)
	_, err = grow(a, "10.0.64.0/18", 17, false)
	want(t, err, ledger.NotFound)

	// Widening merges the silo's own buddy block into it.
	w.mustDelegate(t, b, address.Want{Exact: p("10.0.128.0/19")}, "")
	w.mustDelegate(t, b, address.Want{Exact: p("10.0.160.0/19")}, "")
	g, err = grow(b, "10.0.128.0/19", 18, false)
	if err != nil || !g.Widened || g.Block != p("10.0.128.0/18") {
		t.Fatalf("grow b = %+v, %v", g, err)
	}
	if bs := w.blocks(t, b); len(bs) != 1 || bs[0] != p("10.0.128.0/18") {
		t.Fatalf("b's blocks = %v, want just 10.0.128.0/18", bs)
	}
	w.mapOf(t, w.root)

	// A child's block grows inside its parent's, and what it holds stays.
	c := w.child(t, b, "c")
	w.mustDelegate(t, c, address.Want{Exact: p("10.0.128.0/24")}, "")
	do(t, w.db, func(tx store.Tx) error {
		return address.SetOwn(ctx, tx, c, []address.OwnRange{{Object: "s1", Range: p("10.0.128.0/29")}})
	})
	g, err = grow(c, "", 22, false)
	if err != nil || g.Block != p("10.0.128.0/22") {
		t.Fatalf("grow c = %+v, %v", g, err)
	}
	// The root can't grow, nor can its parent's whole block widen.
	_, err = grow(w.root, "", 15, false)
	want(t, err, ledger.InvalidArgument)
	_, err = grow(b, "", 17, false)
	want(t, err, ledger.FailedPrecondition)
	w.mapOf(t, b)
	w.mapOf(t, c)
}

func TestBlockCap(t *testing.T) {
	w := newWorld(t)
	a := w.child(t, w.root, "a")
	for range address.DefaultBlockCap {
		w.mustDelegate(t, a, address.Want{Bits: 28}, "")
	}
	_, err := w.delegate(a, address.Want{Bits: 28})
	want(t, err, ledger.ResourceExhausted, "cap")
	first := w.blocks(t, a)[0]
	err = w.db.Tx(ctx, func(tx store.Tx) error {
		_, err := address.Grow(ctx, tx, a, first, 20, true)
		return err
	})
	// The /20 around the first /28 holds the gateway and control
	// ranges, so it can't widen, and add-block meets the cap.
	want(t, err, ledger.ResourceExhausted, "cap")

	setCap := func(silo string, n int) error {
		return w.db.Tx(ctx, func(tx store.Tx) error { return address.SetBlockCap(ctx, tx, silo, n) })
	}
	want(t, setCap(a, address.MaxBlockCap+1), ledger.InvalidArgument)
	want(t, setCap(a, 3), ledger.FailedPrecondition)
	want(t, setCap(w.root, 8), ledger.InvalidArgument)
	if err := setCap(a, 6); err != nil {
		t.Fatal(err)
	}
	w.mustDelegate(t, a, address.Want{Bits: 28}, "")
	w.mustDelegate(t, a, address.Want{Bits: 28}, "")
	_, err = w.delegate(a, address.Want{Bits: 28})
	want(t, err, ledger.ResourceExhausted, "cap")
	do(t, w.db, func(tx store.Tx) error {
		n, err := address.BlockCap(ctx, tx, a)
		if n != 6 {
			t.Errorf("BlockCap = %d, want 6", n)
		}
		return err
	})
}

// TestFreed: a deleted silo's blocks go back to its parent, merge with
// their free buddies, and cool for an hour of database time.
func TestFreed(t *testing.T) {
	w := newWorld(t)
	a, b := w.child(t, w.root, "a"), w.child(t, w.root, "b")
	w.mustDelegate(t, a, address.Want{Exact: p("10.0.0.0/18")}, "")
	w.mustDelegate(t, b, address.Want{Exact: p("10.0.64.0/18")}, "")
	ga := w.child(t, a, "ga")
	w.mustDelegate(t, ga, address.Want{Bits: 24}, "")
	do(t, w.db, func(tx store.Tx) error {
		return address.SetOwn(ctx, tx, a, []address.OwnRange{{Object: "s", Range: p("10.0.32.0/24")}})
	})
	release := func(silo string) error {
		return w.db.Tx(ctx, func(tx store.Tx) error { return address.ReleaseSilo(ctx, tx, silo) })
	}
	// Bottom-up: not while a still delegates or owns anything.
	want(t, release(a), ledger.FailedPrecondition, "/a/ga")
	if err := release(ga); err != nil {
		t.Fatal(err)
	}
	want(t, release(a), ledger.FailedPrecondition, "own range s")
	do(t, w.db, func(tx store.Tx) error { return address.RemoveOwn(ctx, tx, a, "s") })
	if err := release(a); err != nil {
		t.Fatal(err)
	}
	if err := release(b); err != nil {
		t.Fatal(err)
	}
	if bs := w.blocks(t, a); len(bs) != 0 {
		t.Errorf("a still holds %v", bs)
	}
	cooling := 0
	for _, r := range w.mapOf(t, w.root) {
		if !r.Cooling.IsZero() {
			cooling++
		}
	}
	if cooling != 2 {
		t.Errorf("root's map has %d cooling ranges, want 2", cooling)
	}

	c := w.child(t, w.root, "c")
	_, err := w.delegate(c, address.Want{Exact: p("10.0.0.0/17")})
	want(t, err, ledger.FailedPrecondition, "reusable")
	_, err = w.delegate(c, address.Want{Bits: 17})
	want(t, err, ledger.ResourceExhausted, "/18")
	w.db.advance(59 * time.Minute)
	_, err = w.delegate(c, address.Want{Bits: 17})
	want(t, err, ledger.ResourceExhausted)
	w.db.advance(2 * time.Minute)
	// The two freed /18s merged back into the /17.
	w.mustDelegate(t, c, address.Want{Bits: 17}, "10.0.0.0/17")

	// A released block cools too, and only an empty one is released.
	d := w.child(t, w.root, "d")
	w.mustDelegate(t, d, address.Want{Exact: p("10.0.128.0/20")}, "")
	w.mustDelegate(t, d, address.Want{Exact: p("10.0.144.0/20")}, "")
	do(t, w.db, func(tx store.Tx) error {
		return address.SetOwn(ctx, tx, d, []address.OwnRange{{Object: "s", Range: p("10.0.144.0/24")}})
	})
	rel := func(block string) error {
		return w.db.Tx(ctx, func(tx store.Tx) error { return address.ReleaseBlock(ctx, tx, d, p(block)) })
	}
	want(t, rel("10.0.144.0/20"), ledger.FailedPrecondition, "own range s")
	want(t, rel("10.0.160.0/20"), ledger.NotFound)
	if err := rel("10.0.128.0/20"); err != nil {
		t.Fatal(err)
	}
	_, err = w.delegate(c, address.Want{Exact: p("10.0.128.0/20")})
	want(t, err, ledger.FailedPrecondition, "reusable")
	w.db.advance(time.Hour)
	w.mustDelegate(t, c, address.Want{Exact: p("10.0.128.0/20")}, "")
}

func TestOwnRanges(t *testing.T) {
	w := newWorld(t)
	a := w.child(t, w.root, "a")
	w.mustDelegate(t, a, address.Want{Exact: p("10.0.192.0/20")}, "")
	g := w.child(t, a, "g")
	w.mustDelegate(t, g, address.Want{Exact: p("10.0.192.0/22")}, "")
	set := func(rs ...address.OwnRange) error {
		return w.db.Tx(ctx, func(tx store.Tx) error { return address.SetOwn(ctx, tx, a, rs) })
	}
	own := func(o, r string) address.OwnRange {
		return address.OwnRange{Object: o, Range: netip.MustParsePrefix(r)}
	}
	if err := set(own("sub", "10.0.200.0/24"), own("sub/pods", "10.0.204.0/22")); err != nil {
		t.Fatal(err)
	}
	want(t, set(own("x", "10.0.208.0/30")), ledger.InvalidArgument, "/29")
	want(t, set(own("x", "10.0.0.0/24")), ledger.InvalidArgument, "10.0.192.0/20")
	want(t, set(own("x", "10.0.193.0/24")), ledger.InvalidArgument, "delegated to /a/g", "10.0.192.0/20")
	want(t, set(own("x", "10.0.200.128/25")), ledger.InvalidArgument, "own range sub")
	want(t, set(own("x", "10.0.202.0/24"), own("y", "10.0.202.0/25")), ledger.InvalidArgument, "given for x")
	want(t, set(own("x", "10.0.208.1/24")), ledger.InvalidArgument, "aligned")
	want(t, set(own("x", "fd00::/64")), ledger.InvalidArgument, "IPv4")
	// Replacing a range checks it against the others, not itself.
	if err := set(own("sub", "10.0.200.0/25")); err != nil {
		t.Fatal(err)
	}
	// Expanding.
	expand := func(to string) error {
		return w.db.Tx(ctx, func(tx store.Tx) error { return address.Expand(ctx, tx, a, "sub", p(to)) })
	}
	if err := expand("10.0.200.0/23"); err != nil {
		t.Fatal(err)
	}
	want(t, expand("10.0.200.0/23"), ledger.InvalidArgument, "wider")
	want(t, expand("10.0.208.0/22"), ledger.InvalidArgument)
	want(t, expand("10.0.200.0/21"), ledger.InvalidArgument, "own range sub/pods")
	want(t, expand("10.0.192.0/19"), ledger.InvalidArgument, "not inside")
	err := w.db.Tx(ctx, func(tx store.Tx) error { return address.Expand(ctx, tx, a, "nope", p("10.0.208.0/22")) })
	want(t, err, ledger.NotFound)
	w.mapOf(t, a)

	// A delegation can't land on an own range either.
	h := w.child(t, a, "h")
	_, err = w.delegate(h, address.Want{Exact: p("10.0.200.0/24")})
	want(t, err, ledger.FailedPrecondition, "own range sub")
}

func TestLookup(t *testing.T) {
	w := newWorld(t)
	a := w.child(t, w.root, "a")
	w.mustDelegate(t, a, address.Want{Exact: p("10.0.192.0/20")}, "")
	g := w.child(t, a, "g")
	w.mustDelegate(t, g, address.Want{Exact: p("10.0.192.0/22")}, "")
	do(t, w.db, func(tx store.Tx) error {
		if err := address.SetOwn(ctx, tx, a, []address.OwnRange{{Object: "s", Range: p("10.0.200.0/24")}}); err != nil {
			return err
		}
		return address.SetOwn(ctx, tx, g, []address.OwnRange{{Object: "s", Range: p("10.0.192.0/24")}})
	})
	snap := func() *address.Table {
		var tab *address.Table
		do(t, w.db, func(tx store.Tx) error {
			var err error
			tab, err = address.Snapshot(ctx, tx)
			return err
		})
		return tab
	}
	tab := snap()
	for _, c := range []struct {
		ip, silo, block, subnet string
	}{
		{"10.0.192.5", g, "10.0.192.0/22", "10.0.192.0/24"},
		{"10.0.195.255", g, "10.0.192.0/22", ""},
		{"10.0.196.1", a, "10.0.192.0/20", ""},
		{"10.0.200.9", a, "10.0.192.0/20", "10.0.200.0/24"},
		{"::ffff:10.0.200.9", a, "10.0.192.0/20", "10.0.200.0/24"},
		{"10.0.1.1", w.root, "10.0.0.0/16", ""},
		{"10.0.255.7", address.SystemGateway, "10.0.255.0/24", ""},
		{"10.0.254.7", address.SystemControl, "10.0.254.0/24", ""},
		{"11.0.0.1", "", "", ""},
		{"fd00::1", "", "", ""},
	} {
		o, ok := tab.Lookup(netip.MustParseAddr(c.ip))
		if c.silo == "" {
			if ok {
				t.Errorf("Lookup(%s) = %+v, want none", c.ip, o)
			}
			continue
		}
		var sub netip.Prefix
		if c.subnet != "" {
			sub = p(c.subnet)
		}
		if !ok || o.Silo != c.silo || o.Block != p(c.block) || o.Subnet != sub {
			t.Errorf("Lookup(%s) = %+v, %v; want %s in %s, subnet %q", c.ip, o, ok, c.silo, c.block, c.subnet)
		}
	}

	// The version is the change log's: each change moves it on, and the
	// change it moves to is the address change just made.
	var last uint64
	do(t, w.db, func(tx store.Tx) error {
		return tx.QueryRow(ctx, `SELECT MAX(seq) FROM changes`).Scan(&last)
	})
	if tab.Version != last {
		t.Errorf("table version %d, change log at %d", tab.Version, last)
	}
	do(t, w.db, func(tx store.Tx) error { return address.RemoveOwn(ctx, tx, g, "s") })
	tab2 := snap()
	if tab2.Version != tab.Version+1 {
		t.Errorf("version after one change = %d, want %d", tab2.Version, tab.Version+1)
	}
	if o, _ := tab2.Lookup(netip.MustParseAddr("10.0.192.5")); o.Subnet.IsValid() {
		t.Errorf("subnet still found after it was removed: %+v", o)
	}
	// An exported table rebuilds the same.
	re := address.NewTable(tab.Version, tab.Blocks, tab.Own)
	if o, _ := re.Lookup(netip.MustParseAddr("10.0.192.5")); o.Silo != g || o.Subnet != p("10.0.192.0/24") {
		t.Errorf("rebuilt table: %+v", o)
	}
}
