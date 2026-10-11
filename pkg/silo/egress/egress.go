// Package egress is the egress policy (docs/silo/gateway.md, "Policy"
// and "Enforcement", SV-86 and SV-87): what a policy entry is, how one
// level's allow and deny lists decide a host and port, how the levels
// down the tree combine, and which addresses count as internal. The
// fence API validates and narrows policies with it, and the gateway's
// proxy enforces them with it, so the two can't disagree about an
// entry.
package egress

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// The ports an entry without one of its own allows: 443 for CONNECT, 80
// for plain HTTP.
const (
	DefaultConnectPort = 443
	DefaultHTTPPort    = 80
)

// Policy is one level's egress policy: a silo's own, as stored. It has
// the same JSON shape as the fence API's.
type Policy struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// Entry is one parsed policy entry: a host pattern, and a port or 0 for
// none.
type Entry struct {
	// Host is a name ("deb.debian.org"), "*.<domain>" (any name below
	// the domain, not the domain itself) or "*" (any host).
	Host string
	Port int
}

var hostPattern = regexp.MustCompile(`^(\*|(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*)$`)

// Parse reads an entry: a host pattern, with an optional ":<port>".
func Parse(s string) (Entry, error) {
	h, p, hasPort := strings.Cut(s, ":")
	if !hostPattern.MatchString(h) {
		return Entry{}, fmt.Errorf("egress host %q: want a host name, *.<domain> or *, with an optional :<port>", s)
	}
	e := Entry{Host: h}
	if hasPort {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return Entry{}, fmt.Errorf("egress host %q: the port must be a number from 1 to 65535", s)
		}
		e.Port = n
	}
	return e, nil
}

// Valid checks every entry of p.
func (p Policy) Valid() error {
	for _, s := range append(append([]string{}, p.Allow...), p.Deny...) {
		if _, err := Parse(s); err != nil {
			return err
		}
	}
	return nil
}

// matchHost reports whether pattern matches host, a normalized name.
func matchHost(pattern, host string) bool {
	if pattern == "*" || pattern == host {
		return true
	}
	if d, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+d)
	}
	return false
}

// Covers reports whether every host and port allow entry b lets
// through is let through by allow entry a: a's host pattern covers b's
// ("*" covers everything, "*.x.org" covers "a.x.org" and "*.a.x.org"),
// and the two have the same port. A port-less entry stands for 443 on
// CONNECT and 80 on plain HTTP together, so it neither covers nor is
// covered by an entry with a port. Unparseable entries cover nothing.
func Covers(a, b string) bool {
	ea, err := Parse(a)
	if err != nil {
		return false
	}
	eb, err := Parse(b)
	if err != nil || ea.Port != eb.Port {
		return false
	}
	if ea.Host == "*" || ea.Host == eb.Host {
		return true
	}
	if d, ok := strings.CutPrefix(ea.Host, "*."); ok {
		return eb.Host != "*" && strings.HasSuffix(eb.Host, "."+d)
	}
	return false
}

// Normalize is host as the policy compares it: lower case, without a
// trailing dot or IPv6 brackets.
func Normalize(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return host
}

// Decide is one level's verdict on host and port, def being the port a
// port-less allow entry stands for (DefaultConnectPort or
// DefaultHTTPPort). deny beats allow, and a host matching neither is
// denied. A port-less deny entry denies the host on every port, so that
// denying a name can't be got round by asking for another port of it.
func (p Policy) Decide(host string, port, def int) (allowed bool, by string) {
	host = Normalize(host)
	for _, s := range p.Deny {
		e, err := Parse(s)
		if err != nil {
			continue
		}
		if matchHost(e.Host, host) && (e.Port == 0 || e.Port == port) {
			return false, s
		}
	}
	for _, s := range p.Allow {
		e, err := Parse(s)
		if err != nil {
			continue
		}
		want := e.Port
		if want == 0 {
			want = def
		}
		if matchHost(e.Host, host) && port == want {
			return true, s
		}
	}
	return false, ""
}

// Allowed is the effective policy's verdict: levels are a silo's
// ancestry's own policies, root first and the silo's own last, and host
// is allowed only if every level allows it. denied is the index of the
// first level that doesn't, and by the deny entry that matched there
// ("" if nothing matched).
func Allowed(levels []Policy, host string, port, def int) (ok bool, denied int, by string) {
	if len(levels) == 0 {
		return false, 0, ""
	}
	for i, p := range levels {
		if ok, by := p.Decide(host, port, def); !ok {
			return false, i, by
		}
	}
	return true, -1, ""
}

// fixedInternal are the ranges no egress may reach whatever the shared
// VPC's range is: "this network", RFC 1918, loopback, link-local
// (169.254.0.0/16, the metadata server's), ULA, and IPv6's unspecified,
// loopback and link-local addresses.
var fixedInternal = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

// Internal reports whether a is an address egress must not reach: one
// in vpc (the shared VPC's ranges, which hold every silo's blocks and
// the gateway and control ranges), in a fixed internal range, or a
// multicast address. An IPv4-mapped IPv6 address is judged as the IPv4
// address it is.
func Internal(a netip.Addr, vpc []netip.Prefix) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range fixedInternal {
		if p.Contains(a) {
			return true
		}
	}
	for _, p := range vpc {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
