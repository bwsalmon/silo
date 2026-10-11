package address

import (
	"context"
	"net/netip"

	"github.com/bwsalmon/silo/pkg/silo/store"
)

// Entry is one row of the origin table: a block or an own range, and
// the silo (or system entry) that holds it.
type Entry struct {
	Range netip.Prefix
	Silo  string
}

// Origin is what Table.Lookup finds for an address.
type Origin struct {
	Silo   string       // whose own space holds the address
	Block  netip.Prefix // the block of Silo's it is in
	Subnet netip.Prefix // the own range of Silo's it is in; zero if none
}

// Table is the origin-identity table (gateway.md, "Origin identity"):
// every block and every own range, at one version of the change log.
// It is immutable, so a reader may share it.
type Table struct {
	// Version is the change log's sequence number as of the snapshot:
	// the table holds every change up to and including it.
	Version uint64
	Blocks  []Entry // in address order
	Own     []Entry // in address order

	blocks [33]map[uint32]string
	own    [33]map[uint32]string
}

// NewTable builds a table from its entries, as a reader that got them
// exported does.
func NewTable(version uint64, blocks, own []Entry) *Table {
	t := &Table{Version: version, Blocks: blocks, Own: own}
	index := func(into *[33]map[uint32]string, es []Entry) {
		for _, e := range es {
			s, err := spanOf(e.Range)
			if err != nil {
				continue
			}
			if into[s.bits] == nil {
				into[s.bits] = map[uint32]string{}
			}
			into[s.bits][s.start] = e.Silo
		}
	}
	index(&t.blocks, blocks)
	index(&t.own, own)
	return t
}

// Lookup finds the silo whose own space holds ip: the holder of the
// longest block containing it, since every silo's blocks lie inside
// its parent's. ok is false for an address in no block.
func (t *Table) Lookup(ip netip.Addr) (o Origin, ok bool) {
	ip = ip.Unmap()
	if !ip.Is4() {
		return Origin{}, false
	}
	b := ip.As4()
	a := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	find := func(in *[33]map[uint32]string, who string) (span, string, bool) {
		for bits := 32; bits >= 0; bits-- {
			s := span{start: a, bits: 32}.widen(bits)
			if h, ok := in[bits][s.start]; ok && (who == "" || h == who) {
				return s, h, true
			}
		}
		return span{}, "", false
	}
	blk, holder, ok := find(&t.blocks, "")
	if !ok {
		return Origin{}, false
	}
	o = Origin{Silo: holder, Block: blk.prefix()}
	if sub, _, ok := find(&t.own, holder); ok && blk.contains(sub) {
		o.Subnet = sub.prefix()
	}
	return o, true
}

// Snapshot reads the origin table, versioned by the change log's
// sequence number as of this transaction.
func Snapshot(ctx context.Context, tx store.Tx) (*Table, error) {
	var version int64
	// change_counter is the store's: it holds the last sequence number
	// handed out, which in this transaction is the last committed one.
	if err := tx.QueryRow(ctx, `SELECT seq FROM change_counter WHERE id = 1`).Scan(&version); err != nil {
		return nil, err
	}
	read := func(query string) ([]Entry, error) {
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []Entry
		for rows.Next() {
			var start int64
			var bits int
			var e Entry
			if err := rows.Scan(&start, &bits, &e.Silo); err != nil {
				return nil, err
			}
			e.Range = span{start: uint32(start), bits: bits}.prefix()
			out = append(out, e)
		}
		return out, rows.Err()
	}
	blocks, err := read(`SELECT start, bits, silo FROM address_blocks ORDER BY start, bits`)
	if err != nil {
		return nil, err
	}
	own, err := read(`SELECT start, bits, silo FROM address_own ORDER BY start, bits`)
	if err != nil {
		return nil, err
	}
	return NewTable(uint64(version), blocks, own), nil
}
