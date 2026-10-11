package fenceeval

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// anyProto is the protocol "all" in a rule.
const anyProto = -1

// protocols are the names GCE accepts for an IP protocol.
var protocols = map[string]int{
	"icmp": 1,
	"ipip": 4,
	"tcp":  6,
	"udp":  17,
	"esp":  50,
	"ah":   51,
	"sctp": 132,
}

// hasPorts reports whether rules may name ports for protocol p.
func hasPorts(p int) bool { return p == 6 || p == 17 || p == 132 }

// parseProto parses a protocol name or number. allowAll admits "all".
func parseProto(s string, allowAll bool) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "all" && allowAll {
		return anyProto, nil
	}
	if n, ok := protocols[s]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 255 {
		return 0, fmt.Errorf("%q is not a protocol", s)
	}
	return n, nil
}

// protoName is p by name where GCE has one.
func protoName(p int) string {
	for name, n := range protocols {
		if n == p {
			return name
		}
	}
	if p == anyProto {
		return "all"
	}
	return strconv.Itoa(p)
}

type portRange struct{ lo, hi int }

func parsePorts(ss []string) ([]portRange, error) {
	var out []portRange
	for _, s := range ss {
		lo, hi, isRange := strings.Cut(s, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("%q is not a port", s)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, fmt.Errorf("%q is not a port range", s)
			}
		}
		if a < 0 || b > 65535 || a > b {
			return nil, fmt.Errorf("%q is not a port range in [0, 65535]", s)
		}
		out = append(out, portRange{a, b})
	}
	return out, nil
}

// l4 is one protocol entry of a rule: a protocol and its ports, nil
// ports meaning all of them.
type l4 struct {
	proto int
	ports []portRange
}

func parseL4(proto string, ports []string) (l4, error) {
	p, err := parseProto(proto, true)
	if err != nil {
		return l4{}, err
	}
	if len(ports) > 0 && !hasPorts(p) {
		return l4{}, fmt.Errorf("ports are only allowed for tcp, udp and sctp, not %s", proto)
	}
	rs, err := parsePorts(ports)
	if err != nil {
		return l4{}, err
	}
	return l4{proto: p, ports: rs}, nil
}

func (c l4) matches(proto, port int) bool {
	if c.proto != anyProto && c.proto != proto {
		return false
	}
	if len(c.ports) == 0 || !hasPorts(proto) {
		return true
	}
	for _, r := range c.ports {
		if port >= r.lo && port <= r.hi {
			return true
		}
	}
	return false
}

func matchL4(cs []l4, proto, port int) bool {
	for _, c := range cs {
		if c.matches(proto, port) {
			return true
		}
	}
	return false
}

// parseRanges parses CIDRs or bare addresses, as GCE accepts both.
func parseRanges(ss []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, fmt.Errorf("%q is not a range", s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func inRanges(rs []netip.Prefix, a netip.Addr) bool {
	for _, r := range rs {
		if r.Contains(a) {
			return true
		}
	}
	return false
}

func hasAny(have, want []string) bool {
	for _, w := range want {
		for _, h := range have {
			if h == w {
				return true
			}
		}
	}
	return false
}
