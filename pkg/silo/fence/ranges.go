package fence

import (
	"net/netip"
	"sort"
)

// Minus is p with the given ranges taken out, as the fewest aligned
// prefixes. Invalid holes are ignored.
func Minus(p netip.Prefix, holes ...netip.Prefix) []netip.Prefix {
	ps := []netip.Prefix{p.Masked()}
	for _, h := range holes {
		if !h.IsValid() {
			continue
		}
		var next []netip.Prefix
		for _, q := range ps {
			next = append(next, cut(q, h.Masked())...)
		}
		ps = next
	}
	return ps
}

func cut(p, hole netip.Prefix) []netip.Prefix {
	switch {
	case !p.Overlaps(hole):
		return []netip.Prefix{p}
	case hole.Bits() <= p.Bits():
		return nil
	}
	lo := netip.PrefixFrom(p.Addr(), p.Bits()+1)
	hi := netip.PrefixFrom(lastAddr(lo).Next(), p.Bits()+1)
	return append(cut(lo, hole), cut(hi, hole)...)
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// normalize masks ps, drops any prefix inside another, and sorts them
// by address. Two prefixes either nest or are disjoint, so the result
// covers exactly what ps does.
func normalize(ps []netip.Prefix) []netip.Prefix {
	in := make([]netip.Prefix, 0, len(ps))
	for _, p := range ps {
		if p.IsValid() {
			in = append(in, p.Masked())
		}
	}
	sort.Slice(in, func(i, j int) bool {
		if c := in[i].Addr().Compare(in[j].Addr()); c != 0 {
			return c < 0
		}
		return in[i].Bits() < in[j].Bits()
	})
	var out []netip.Prefix
	for _, p := range in {
		if n := len(out); n > 0 && out[n-1].Contains(p.Addr()) && out[n-1].Bits() <= p.Bits() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// union is everything in a or b.
func union(a, b []netip.Prefix) []netip.Prefix {
	return normalize(append(append([]netip.Prefix(nil), a...), b...))
}

// intersect is everything in both a and b: where two prefixes overlap,
// the narrower one.
func intersect(a, b []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, x := range a {
		for _, y := range b {
			if !x.Overlaps(y) {
				continue
			}
			if x.Bits() >= y.Bits() {
				out = append(out, x)
			} else {
				out = append(out, y)
			}
		}
	}
	return normalize(out)
}

func parsePrefixes(ss []string) ([]netip.Prefix, bool) {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, false
		}
		out = append(out, p)
	}
	return out, true
}
