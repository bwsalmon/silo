package address

import (
	"fmt"
	"net/netip"
	"strings"
)

// span is an aligned IPv4 range: its first address and prefix length.
type span struct {
	start uint32
	bits  int
}

func (s span) size() uint64 { return 1 << (32 - s.bits) }
func (s span) end() uint64  { return uint64(s.start) + s.size() } // exclusive

func (s span) prefix() netip.Prefix {
	a := netip.AddrFrom4([4]byte{byte(s.start >> 24), byte(s.start >> 16), byte(s.start >> 8), byte(s.start)})
	return netip.PrefixFrom(a, s.bits)
}

func (s span) String() string { return s.prefix().String() }

// contains reports whether o lies wholly inside s.
func (s span) contains(o span) bool {
	return uint64(s.start) <= uint64(o.start) && o.end() <= s.end()
}

func (s span) overlaps(o span) bool {
	return uint64(s.start) < o.end() && uint64(o.start) < s.end()
}

// halves splits s into its two buddies. s.bits must be under 32.
func (s span) halves() (span, span) {
	h := span{start: s.start, bits: s.bits + 1}
	return h, span{start: s.start + uint32(h.size()), bits: s.bits + 1}
}

// widen is the aligned range of bits that contains s.
func (s span) widen(bits int) span {
	mask := uint32(0)
	if bits > 0 {
		mask = ^uint32(0) << (32 - bits)
	}
	return span{start: s.start & mask, bits: bits}
}

// spanOf converts an IPv4 prefix, which must already be masked: a
// range with host bits set is not aligned.
func spanOf(p netip.Prefix) (span, error) {
	if !p.IsValid() {
		return span{}, fmt.Errorf("%q is not a valid CIDR range", p.String())
	}
	addr := p.Addr().Unmap()
	if !addr.Is4() {
		return span{}, fmt.Errorf("%s is not IPv4 (IPv6 comes later)", p)
	}
	if p.Masked().Addr().Unmap() != addr {
		return span{}, fmt.Errorf("%s is not aligned: its first address is %s", p, p.Masked().Addr())
	}
	b := addr.As4()
	return span{start: uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), bits: p.Bits()}, nil
}

// freeRegions returns the maximal free aligned regions of block: the
// largest aligned ranges inside it that overlap nothing in busy. They
// are in address order.
func freeRegions(block span, busy []span) []span {
	var in []span
	for _, b := range busy {
		if b.overlaps(block) {
			if b.contains(block) {
				return nil
			}
			in = append(in, b)
		}
	}
	if len(in) == 0 {
		return []span{block}
	}
	lo, hi := block.halves()
	return append(freeRegions(lo, in), freeRegions(hi, in)...)
}

// placement picks a slot of bits for a new block among the free
// regions, in the preference order of allocators.md, "Not fungible":
//
//  1. a slot whose buddy is also free, so it can later widen in place.
//     A slot inside a larger free region has its buddy free; a slot that
//     is a whole maximal region does not, since otherwise the region
//     would not be maximal;
//  2. the slot inside the smallest free region that holds it;
//  3. the lowest address.
//
// ok is false if no region holds a slot of bits.
func placement(free []span, bits int) (slot span, ok bool) {
	var best span
	for _, r := range free {
		if r.bits > bits {
			continue
		}
		if !ok || better(r, best, bits) {
			best, ok = r, true
		}
	}
	return span{start: best.start, bits: bits}, ok
}

// better reports whether region a is a better home for a slot of bits
// than region b.
func better(a, b span, bits int) bool {
	if ab, bb := a.bits < bits, b.bits < bits; ab != bb {
		return ab
	}
	if a.bits != b.bits {
		return a.bits > b.bits
	}
	return a.start < b.start
}

// largest is the prefix of the largest region in free, or -1.
func largest(free []span) int {
	out := -1
	for _, r := range free {
		if out < 0 || r.bits < out {
			out = r.bits
		}
	}
	return out
}

func list(s []span) string {
	if len(s) == 0 {
		return "none"
	}
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.String()
	}
	return strings.Join(out, ", ")
}
