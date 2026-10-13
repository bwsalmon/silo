package netfence

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Packet is what the evaluator runs through a hook: an IPv4 packet in
// inet silo, or a frame in bridge silo. A field that doesn't apply is
// left zero, and a match on it fails, as nft's implicit dependencies
// make it (`ip saddr` never matches an IPv6 packet, `th dport` only
// matches with a protocol that has ports).
type Packet struct {
	IIF, OIF string
	// EtherType is "ip", "arp", "ip6", "vlan" or another name, for a
	// frame; inet silo sees only "ip" and "ip6".
	EtherType string
	// EtherSrc is the frame's source MAC, and ArpSHA the ARP sender's.
	EtherSrc, ArpSHA string
	// Src and Dst are the IPv4 source and destination, or the ARP
	// sender's and target's addresses.
	Src, Dst netip.Addr
	Proto    uint8
	DPort    uint16
	// CtState is conntrack's state: "new" (the default), "established"
	// or "related".
	CtState string
}

// Eval runs p through the base chain on hook in the family's table and
// returns its verdict, and the index of the rule that decided it (-1
// for the chain's policy). A ruleset without that table or chain
// accepts, as the kernel would.
func (rs *Ruleset) Eval(family, hook string, p Packet) (verdict string, rule int) {
	t := rs.Table(family)
	if t == nil {
		return "accept", -1
	}
	c := t.Chain(hook)
	if c == nil {
		return "accept", -1
	}
	if family == "inet" && p.EtherType == "" {
		p.EtherType = "ip"
	}
	for i, r := range c.Rules {
		if t.matches(r, p) {
			return r.Verdict, i
		}
	}
	return c.Policy, -1
}

func (t *Table) matches(r Rule, p Packet) bool {
	for _, m := range r.Matches {
		if !t.match(m, p) {
			return false
		}
	}
	return true
}

func (t *Table) match(m Match, p Packet) bool {
	vals := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		v, ok := p.field(f)
		if !ok {
			return false
		}
		vals[i] = v
	}
	var hit bool
	if m.Set != "" {
		s := t.Set(m.Set)
		if s == nil {
			return false // nft refuses a rule naming a missing set
		}
		for _, e := range s.Elems {
			if elemMatches(m.Fields, e, vals) {
				hit = true
				break
			}
		}
	} else {
		for _, v := range m.Values {
			if valueMatches(m.Fields[0], v, vals[0]) {
				hit = true
				break
			}
		}
	}
	return hit != m.Neg
}

func elemMatches(fs []Field, e Elem, vals []string) bool {
	if len(e) != len(fs) {
		return false
	}
	for i := range fs {
		if !valueMatches(fs[i], e[i], vals[i]) {
			return false
		}
	}
	return true
}

// field is p's value of f, and whether p has one.
func (p Packet) field(f Field) (string, bool) {
	ip := p.EtherType == "ip"
	switch f {
	case IIF:
		return p.IIF, p.IIF != ""
	case OIF:
		return p.OIF, p.OIF != ""
	case SAddr:
		return p.Src.String(), ip && p.Src.Is4()
	case DAddr:
		return p.Dst.String(), ip && p.Dst.Is4()
	case L4Proto:
		return strconv.Itoa(int(p.Proto)), ip
	case DPort:
		return strconv.Itoa(int(p.DPort)), ip && (p.Proto == 6 || p.Proto == 17 || p.Proto == 132)
	case CtState:
		if p.CtState == "" {
			return "new", true
		}
		return p.CtState, true
	case EtherType:
		return p.EtherType, p.EtherType != ""
	case EtherSrc:
		return strings.ToLower(p.EtherSrc), p.EtherSrc != ""
	case ArpSHA:
		return strings.ToLower(p.ArpSHA), p.EtherType == "arp" && p.ArpSHA != ""
	case ArpSPA:
		return p.Src.String(), p.EtherType == "arp" && p.Src.Is4()
	}
	return "", false
}

// valueMatches is whether v, a rule's or element's value for f (a
// single value, a prefix or a range), covers got, the packet's.
func valueMatches(f Field, v, got string) bool {
	switch f {
	case SAddr, DAddr, ArpSPA:
		a, err := netip.ParseAddr(got)
		if err != nil {
			return false
		}
		return addrIn(v, a)
	case L4Proto:
		n, _ := strconv.Atoi(got)
		lo, hi, err := protoRange(v)
		return err == nil && lo <= n && n <= hi
	case DPort:
		n, _ := strconv.Atoi(got)
		lo, hi, err := portRange(v)
		return err == nil && lo <= n && n <= hi
	case EtherSrc, ArpSHA:
		return strings.EqualFold(v, got)
	}
	return v == got
}

func addrIn(v string, a netip.Addr) bool {
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		return err == nil && p.Contains(a)
	}
	if lo, hi, ok := strings.Cut(v, "-"); ok {
		l, err1 := netip.ParseAddr(lo)
		h, err2 := netip.ParseAddr(hi)
		return err1 == nil && err2 == nil && l.Compare(a) <= 0 && a.Compare(h) <= 0
	}
	x, err := netip.ParseAddr(v)
	return err == nil && x == a
}

func protoRange(v string) (int, int, error) {
	if n, ok := protoNumbers[v]; ok {
		return n, n, nil
	}
	return numRange(v, 255)
}

func portRange(v string) (int, int, error) { return numRange(v, 65535) }

func numRange(v string, max int) (int, int, error) {
	lo, hi, isRange := strings.Cut(v, "-")
	if !isRange {
		hi = lo
	}
	l, err1 := strconv.Atoi(lo)
	h, err2 := strconv.Atoi(hi)
	if err1 != nil || err2 != nil || l < 0 || h > max || l > h {
		return 0, 0, fmt.Errorf("bad range %q", v)
	}
	return l, h, nil
}
