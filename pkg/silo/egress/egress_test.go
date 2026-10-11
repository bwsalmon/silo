package egress

import (
	"net/netip"
	"testing"
)

func TestParse(t *testing.T) {
	for s, want := range map[string]Entry{
		"github.com":       {Host: "github.com"},
		"github.com:443":   {Host: "github.com", Port: 443},
		"*.debian.org":     {Host: "*.debian.org"},
		"*.debian.org:873": {Host: "*.debian.org", Port: 873},
		"*":                {Host: "*"},
		"*:22":             {Host: "*", Port: 22},
	} {
		got, err := Parse(s)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", s, got, err, want)
		}
	}
	for _, s := range []string{"", "not a host", "GitHub.com", "github.com:", "github.com:0", "github.com:65536",
		"github.com:x", "github.com:0443", "a.*.org", "**.org", "*.", "-a.org", "10.0.0.0/8", "github.com:443:1"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) took it", s)
		}
	}
}

func TestDecide(t *testing.T) {
	p := Policy{
		Allow: []string{"*.debian.org", "proxy.golang.org", "github.com:22", "github.com", "*.example.com"},
		Deny:  []string{"*.internal.example.com", "bad.example.com:443"},
	}
	for _, c := range []struct {
		host      string
		port, def int
		want      bool
	}{
		// A wildcard matches any name below its domain, not the domain.
		{"deb.debian.org", 443, 443, true},
		{"a.b.debian.org", 443, 443, true},
		{"debian.org", 443, 443, false},
		{"xdebian.org", 443, 443, false},
		// A port-less entry is 443 on CONNECT and 80 on plain HTTP.
		{"proxy.golang.org", 443, 443, true},
		{"proxy.golang.org", 80, 443, false},
		{"proxy.golang.org", 80, 80, true},
		{"proxy.golang.org", 443, 80, false},
		{"proxy.golang.org", 8443, 443, false},
		// An entry with a port allows that port, whatever the method.
		{"github.com", 22, 443, true},
		{"github.com", 22, 80, true},
		{"github.com", 443, 443, true},
		{"github.com", 23, 443, false},
		// Names compare without case or a trailing dot.
		{"Deb.Debian.ORG.", 443, 443, true},
		// deny beats allow; a port-less deny denies every port, one with a
		// port that port only.
		{"www.example.com", 443, 443, true},
		{"db.internal.example.com", 443, 443, false},
		{"db.internal.example.com", 80, 80, false},
		{"bad.example.com", 443, 443, false},
		{"bad.example.com", 80, 80, true},
		// Matching neither is denied.
		{"pypi.org", 443, 443, false},
	} {
		if got, _ := p.Decide(c.host, c.port, c.def); got != c.want {
			t.Errorf("Decide(%s:%d, default %d) = %v, want %v", c.host, c.port, c.def, got, c.want)
		}
	}
	if ok, by := p.Decide("db.internal.example.com", 443, 443); ok || by != "*.internal.example.com" {
		t.Errorf("deny names its entry: %v %q", ok, by)
	}
	if ok, _ := (Policy{}).Decide("github.com", 443, 443); ok {
		t.Error("an empty policy allowed something")
	}
	if ok, _ := (Policy{Allow: []string{"*"}}).Decide("anything.org", 443, 443); !ok {
		t.Error("* didn't allow a host")
	}
}

func TestAllowedIsTheIntersection(t *testing.T) {
	root := Policy{Allow: []string{"*"}}
	team := Policy{Allow: []string{"*.debian.org", "github.com"}, Deny: []string{"security.debian.org"}}
	ci := Policy{Allow: []string{"*.debian.org", "pypi.org"}}
	levels := []Policy{root, team, ci}
	for _, c := range []struct {
		host   string
		want   bool
		denied int
	}{
		{"deb.debian.org", true, -1},
		// The silo's own allow can't reach past an ancestor's deny ...
		{"security.debian.org", false, 1},
		// ... or what an ancestor doesn't allow.
		{"pypi.org", false, 1},
		// And an ancestor's allow is no use to a silo that doesn't allow it.
		{"github.com", false, 2},
	} {
		ok, denied, _ := Allowed(levels, c.host, 443, 443)
		if ok != c.want || denied != c.denied {
			t.Errorf("Allowed(%s) = %v at level %d, want %v at %d", c.host, ok, denied, c.want, c.denied)
		}
	}
	if ok, _, _ := Allowed(nil, "deb.debian.org", 443, 443); ok {
		t.Error("no levels allowed a host")
	}
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"*", "github.com", true},
		{"*.x.org", "a.x.org", true},
		{"*.x.org", "*.a.x.org", true},
		{"*.x.org", "x.org", false},
		{"github.com", "github.com", true},
		{"github.com:22", "github.com:22", true},
		{"github.com", "github.com:22", false},
		{"github.com", "github.com:443", false},
		{"github.com:443", "github.com", false},
		{"*", "github.com:22", false},
		{"*:22", "github.com:22", true},
	} {
		if got := Covers(c.a, c.b); got != c.want {
			t.Errorf("Covers(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestInternal(t *testing.T) {
	vpc := []netip.Prefix{netip.MustParsePrefix("100.64.0.0/16")}
	for s, want := range map[string]bool{
		"10.1.2.3":             true,
		"172.16.0.1":           true,
		"172.32.0.1":           false,
		"192.168.1.1":          true,
		"127.0.0.1":            true,
		"169.254.169.254":      true,
		"0.0.0.0":              true,
		"100.64.3.4":           true, // the shared VPC's range
		"100.65.0.1":           false,
		"224.0.0.1":            true,
		"8.8.8.8":              false,
		"::1":                  true,
		"::":                   true,
		"fd20:ce::254":         true,
		"fe80::1":              true,
		"2001:4860::8888":      false,
		"::ffff:10.0.0.1":      true,
		"::ffff:169.254.0.1":   true,
		"::ffff:100.64.0.9":    true,
		"::ffff:93.184.216.34": false,
	} {
		if got := Internal(netip.MustParseAddr(s), vpc); got != want {
			t.Errorf("Internal(%s) = %v, want %v", s, got, want)
		}
	}
}
