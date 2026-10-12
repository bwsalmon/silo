package links_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/links"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/store/sqlite"
)

func open(t *testing.T) store.Database {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background(), db.Latest()); err != nil {
		t.Fatal(err)
	}
	return db
}

func tx(t *testing.T, db store.Database, fn func(store.Tx) error) {
	t.Helper()
	if err := db.Tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func TestCanonical(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []links.Rule
		want []links.Rule
	}{
		{"one port", []links.Rule{{Protocol: "tcp", Ports: []string{"80"}}},
			[]links.Rule{{Protocol: "tcp", Ports: []string{"80"}}}},
		{"case and space", []links.Rule{{Protocol: " TCP ", Ports: []string{" 443 "}}},
			[]links.Rule{{Protocol: "tcp", Ports: []string{"443"}}}},
		{"protocols sorted", []links.Rule{{Protocol: "udp"}, {Protocol: "icmp"}, {Protocol: "tcp"}},
			[]links.Rule{{Protocol: "icmp"}, {Protocol: "tcp"}, {Protocol: "udp"}}},
		{"same protocol merged", []links.Rule{{Protocol: "tcp", Ports: []string{"443"}}, {Protocol: "tcp", Ports: []string{"80"}}},
			[]links.Rule{{Protocol: "tcp", Ports: []string{"80", "443"}}}},
		{"ports sorted numerically", []links.Rule{{Protocol: "tcp", Ports: []string{"1000", "22", "100-200", "100"}}},
			[]links.Rule{{Protocol: "tcp", Ports: []string{"22", "100", "100-200", "1000"}}}},
		{"duplicates and spellings", []links.Rule{{Protocol: "udp", Ports: []string{"53", "53-53", "53"}}},
			[]links.Rule{{Protocol: "udp", Ports: []string{"53"}}}},
		{"whole range after ports", []links.Rule{{Protocol: "tcp", Ports: []string{"80"}}, {Protocol: "tcp"}},
			[]links.Rule{{Protocol: "tcp"}}},
		{"whole range before ports", []links.Rule{{Protocol: "tcp"}, {Protocol: "tcp", Ports: []string{"80"}}},
			[]links.Rule{{Protocol: "tcp"}}},
		{"all alone", []links.Rule{{Protocol: "all"}, {Protocol: "ALL"}},
			[]links.Rule{{Protocol: "all"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := links.Canonical(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Canonical(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCanonicalRejects(t *testing.T) {
	many := make([]links.Rule, links.MaxRules+1)
	for i := range many {
		many[i] = links.Rule{Protocol: "tcp"}
	}
	var ports []string
	for p := 1; p <= links.MaxPorts+1; p++ {
		ports = append(ports, strconv.Itoa(p))
	}
	for _, tc := range []struct {
		name string
		in   []links.Rule
		want string
	}{
		{"none", nil, "at least one rule"},
		{"too many rules", many, "at most"},
		{"unknown protocol", []links.Rule{{Protocol: "gre"}}, `protocol "gre"`},
		{"ports on icmp", []links.Rule{{Protocol: "icmp", Ports: []string{"1"}}}, "takes no ports"},
		{"ports on all", []links.Rule{{Protocol: "all", Ports: []string{"1"}}}, "takes no ports"},
		{"bad port", []links.Rule{{Protocol: "tcp", Ports: []string{"http"}}}, `port "http"`},
		{"too many ports", []links.Rule{{Protocol: "tcp", Ports: ports}}, "at most 32 ports"},
		{"all with another", []links.Rule{{Protocol: "all"}, {Protocol: "udp"}}, "stands alone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := links.Canonical(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Canonical(%v) = %v, want an error containing %q", tc.in, err, tc.want)
			}
		})
	}
}

// TestCanonicalCountsOnlyKeptPorts proves ports a whole-range rule for
// the same protocol drops don't count towards MaxPorts: the canonical
// form names none of them.
func TestCanonicalCountsOnlyKeptPorts(t *testing.T) {
	var ports []string
	for p := 1; p <= links.MaxPorts; p++ {
		ports = append(ports, strconv.Itoa(p))
	}
	in := []links.Rule{{Protocol: "tcp", Ports: ports}, {Protocol: "tcp"}, {Protocol: "udp", Ports: []string{"53"}}}
	got, err := links.Canonical(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []links.Rule{{Protocol: "tcp"}, {Protocol: "udp", Ports: []string{"53"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Canonical = %v, want %v", got, want)
	}
}

func TestCanonicalIsIdempotent(t *testing.T) {
	in := []links.Rule{{Protocol: "UDP", Ports: []string{"9000-9010", "53"}}, {Protocol: "tcp", Ports: []string{"443", "80"}}, {Protocol: "icmp"}}
	once, err := links.Canonical(in)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := links.Canonical(once)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("Canonical(Canonical) = %v, want %v", twice, once)
	}
}

func TestPortRange(t *testing.T) {
	for _, tc := range []struct {
		in     string
		lo, hi int
		ok     bool
	}{
		{"80", 80, 80, true},
		{" 80 ", 80, 80, true},
		{"1-65535", 1, 65535, true},
		{"8000-8000", 8000, 8000, true},
		{"0", 0, 0, false},
		{"65536", 0, 0, false},
		{"90-80", 0, 0, false},
		{"-5", 0, 0, false},
		{"5-", 0, 0, false},
		{"a-b", 0, 0, false},
		{"", 0, 0, false},
		{"1-2-3", 0, 0, false},
	} {
		lo, hi, err := links.PortRange(tc.in)
		if (err == nil) != tc.ok || lo != tc.lo || hi != tc.hi {
			t.Errorf("PortRange(%q) = %d, %d, %v; want %d, %d, ok=%v", tc.in, lo, hi, err, tc.lo, tc.hi, tc.ok)
		}
	}
}

func TestMirror(t *testing.T) {
	for in, want := range map[string]string{links.Outbound: links.Inbound, links.Inbound: links.Outbound, links.Both: links.Both} {
		if got := links.Mirror(in); got != want {
			t.Errorf("Mirror(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestMirrors(t *testing.T) {
	rules := []links.Rule{{Protocol: "tcp", Ports: []string{"80"}}}
	a := &links.Link{Silo: "s1", PeerSilo: "s2", Name: "db", Direction: links.Outbound, Rules: rules}
	b := &links.Link{Silo: "s2", PeerSilo: "s1", Name: "db", Direction: links.Inbound, Rules: rules}
	if !links.Mirrors(a, b) || !links.Mirrors(b, a) {
		t.Fatal("a and b don't mirror each other")
	}
	both := func(l *links.Link) *links.Link { c := *l; c.Direction = links.Both; return &c }
	if !links.Mirrors(both(a), both(b)) {
		t.Error("two-way a and b don't mirror each other")
	}
	for name, mut := range map[string]func(c *links.Link){
		"same direction": func(c *links.Link) { c.Direction = links.Outbound },
		"other name":     func(c *links.Link) { c.Name = "web" },
		"other peer":     func(c *links.Link) { c.PeerSilo = "s3" },
		"other silo":     func(c *links.Link) { c.Silo = "s3" },
		"no peer":        func(c *links.Link) { c.PeerSilo = "" },
		"no silo":        func(c *links.Link) { c.Silo = "" },
		"other rules":    func(c *links.Link) { c.Rules = []links.Rule{{Protocol: "tcp", Ports: []string{"81"}}} },
	} {
		c := *b
		mut(&c)
		if links.Mirrors(a, &c) {
			t.Errorf("%s: Mirrors = true", name)
		}
	}
	// Two silos with no peer don't mirror each other through "".
	x := &links.Link{Name: "n", Direction: links.Both, Rules: rules}
	y := &links.Link{Name: "n", Direction: links.Both, Rules: rules}
	if links.Mirrors(x, y) {
		t.Error("two links with no silo mirror each other")
	}
}

func TestStartsAccepts(t *testing.T) {
	for _, tc := range []struct {
		dir             string
		starts, accepts bool
	}{
		{links.Outbound, true, false},
		{links.Inbound, false, true},
		{links.Both, true, true},
	} {
		l := &links.Link{Direction: tc.dir}
		if l.Starts() != tc.starts || l.Accepts() != tc.accepts {
			t.Errorf("%s: Starts, Accepts = %v, %v; want %v, %v", tc.dir, l.Starts(), l.Accepts(), tc.starts, tc.accepts)
		}
	}
}

func TestRuleNames(t *testing.T) {
	out, in, revOut, revIn := links.RuleNames("abc")
	if out != "link-abc-out" || in != "link-abc-in" || revOut != "link-abc-rev-out" || revIn != "link-abc-rev-in" {
		t.Errorf("RuleNames = %s %s %s %s", out, in, revOut, revIn)
	}
}

func TestStore(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	rules := []links.Rule{{Protocol: "tcp", Ports: []string{"5432"}}}
	a := &links.Link{ID: "p1", Silo: "s1", Name: "db", PeerPath: "/org/b", PeerSilo: "s2", Direction: links.Outbound, Rules: rules, State: links.StatePending}
	b := &links.Link{ID: "p2", Silo: "s2", Name: "db", PeerPath: "/org/a", PeerSilo: "s1", Direction: links.Inbound, Rules: rules, State: links.StatePending}
	c := &links.Link{ID: "p3", Silo: "s1", Name: "cache", PeerPath: "/org/c", Direction: links.Both, Rules: []links.Rule{{Protocol: "all"}}, State: links.StatePending}
	tx(t, db, func(tx store.Tx) error {
		for _, l := range []*links.Link{a, b, c} {
			if err := links.Create(ctx, tx, l); err != nil {
				return err
			}
			if l.Created.IsZero() {
				t.Errorf("%s: Create left Created zero", l.ID)
			}
		}
		return nil
	})
	tx(t, db, func(tx store.Tx) error {
		// (silo, name) is unique.
		dup := *a
		dup.ID = "p9"
		if err := links.Create(ctx, tx, &dup); err == nil {
			t.Error("Create of a second db link in s1 succeeded")
		}
		return nil
	})

	tx(t, db, func(tx store.Tx) error {
		got, err := links.Get(ctx, tx, "p1")
		if err != nil {
			return err
		}
		if got.Name != "db" || got.PeerPath != "/org/b" || got.PeerSilo != "s2" || got.Direction != links.Outbound ||
			!reflect.DeepEqual(got.Rules, rules) || got.State != links.StatePending || got.LinkID != "" || !got.Created.Equal(a.Created) {
			t.Errorf("Get(p1) = %+v, want %+v", got, a)
		}
		if _, err := links.Get(ctx, tx, "nope"); !errors.Is(err, links.ErrNotFound) {
			t.Errorf("Get(nope) = %v, want ErrNotFound", err)
		}
		if got, err := links.ByName(ctx, tx, "s1", "cache"); err != nil || got.ID != "p3" {
			t.Errorf("ByName(s1, cache) = %v, %v", got, err)
		}
		if _, err := links.ByName(ctx, tx, "s2", "cache"); !errors.Is(err, links.ErrNotFound) {
			t.Errorf("ByName(s2, cache) = %v, want ErrNotFound", err)
		}
		of, err := links.Of(ctx, tx, "s1")
		if err != nil {
			return err
		}
		if len(of) != 2 || of[0].Name != "cache" || of[1].Name != "db" {
			t.Errorf("Of(s1) = %v, want cache then db", of)
		}
		if p, err := links.Partner(ctx, tx, a); err != nil || p.ID != "p2" {
			t.Errorf("Partner(p1) = %v, %v; want p2", p, err)
		}
		if _, err := links.Partner(ctx, tx, c); !errors.Is(err, links.ErrNotFound) {
			t.Errorf("Partner(p3) = %v, want ErrNotFound", err)
		}
		if m, err := links.Matched(ctx, tx); err != nil || len(m) != 0 {
			t.Errorf("Matched before any match = %v, %v", m, err)
		}
		return nil
	})

	tx(t, db, func(tx store.Tx) error {
		for _, id := range []string{"p1", "p2"} {
			if err := links.SetMatch(ctx, tx, id, "L1", links.StateActive); err != nil {
				return err
			}
		}
		pairs, err := links.Pairs(ctx, tx)
		if err != nil {
			return err
		}
		if len(pairs) != 1 || pairs[0][0].ID != "p1" || pairs[0][1].ID != "p2" || pairs[0][0].State != links.StateActive {
			t.Errorf("Pairs = %v, want [p1 p2]", pairs)
		}
		return nil
	})

	// A row whose partner has gone is left out of Pairs, and doesn't
	// pair up with the next link's rows.
	tx(t, db, func(tx store.Tx) error {
		d := &links.Link{ID: "p4", Silo: "s3", Name: "x", PeerSilo: "s4", Direction: links.Both, Rules: rules, LinkID: "L0", State: links.StateActive}
		e := &links.Link{ID: "p5", Silo: "s3", Name: "y", PeerSilo: "s4", Direction: links.Both, Rules: rules, LinkID: "L2", State: links.StateSuspended}
		f := &links.Link{ID: "p6", Silo: "s4", Name: "y", PeerSilo: "s3", Direction: links.Both, Rules: rules, LinkID: "L2", State: links.StateSuspended}
		for _, l := range []*links.Link{d, e, f} {
			if err := links.Create(ctx, tx, l); err != nil {
				return err
			}
		}
		pairs, err := links.Pairs(ctx, tx)
		if err != nil {
			return err
		}
		var got []string
		for _, p := range pairs {
			got = append(got, p[0].ID+"+"+p[1].ID)
		}
		if want := []string{"p1+p2", "p5+p6"}; !reflect.DeepEqual(got, want) {
			t.Errorf("Pairs = %v, want %v", got, want)
		}
		return nil
	})

	tx(t, db, func(tx store.Tx) error {
		if err := links.ForgetPeer(ctx, tx, "s2"); err != nil {
			return err
		}
		got, err := links.Get(ctx, tx, "p1")
		if err != nil {
			return err
		}
		if got.PeerSilo != "" || got.PeerPath != "/org/b" {
			t.Errorf("after ForgetPeer(s2), p1 = %+v, want no peer silo and its path kept", got)
		}
		if other, _ := links.Get(ctx, tx, "p2"); other.PeerSilo != "s1" {
			t.Errorf("ForgetPeer(s2) changed p2's peer to %q", other.PeerSilo)
		}
		if err := links.Delete(ctx, tx, "p1"); err != nil {
			return err
		}
		if _, err := links.Get(ctx, tx, "p1"); !errors.Is(err, links.ErrNotFound) {
			t.Errorf("Get after Delete = %v, want ErrNotFound", err)
		}
		return nil
	})
}
