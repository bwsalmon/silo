package fence_test

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	fe "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce/fake/fenceeval"
	"github.com/bwsalmon/silo/pkg/silo/fence"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

// blocks is a silo's blocks.
func (e *env) blocks(id string) []netip.Prefix {
	e.t.Helper()
	var bs []netip.Prefix
	if err := e.db.Tx(context.Background(), func(tx store.Tx) error {
		var err error
		bs, err = address.Blocks(context.Background(), tx, id)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return bs
}

// last is the last address of p, which a test keeps clear of children.
func last(p netip.Prefix) string {
	a := p.Addr().As4()
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	n |= 1<<(32-p.Bits()) - 1
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}).String()
}

// open is the policy in the fake, decided with tenant rules that allow
// everything everywhere, so only the fence stands in the way.
func (e *env) open() *fe.Evaluator {
	e.t.Helper()
	_, pol := e.rules()
	all := []*compute.FirewallAllowed{{IPProtocol: "all"}}
	ev, err := fe.New(fe.BeforeClassic, pol, []*compute.Firewall{
		{Name: "open-in", Direction: "INGRESS", Allowed: all, SourceRanges: []string{"0.0.0.0/0"}},
		{Name: "open-out", Direction: "EGRESS", Allowed: all, DestinationRanges: []string{"0.0.0.0/0"}},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func (e *env) linkRules() []string {
	e.t.Helper()
	rs, _ := e.rules()
	var out []string
	for n, r := range rs {
		if strings.HasPrefix(n, "link-") {
			if r.Priority < fence.GrantsBand || r.Priority > fence.GrantsEnd || r.Action != "allow" {
				e.t.Errorf("%s at %d, %s: want an allow in the grants band", n, r.Priority, r.Action)
			}
			out = append(out, n)
		}
	}
	return out
}

func pk(proto string, src, dst string, port int) fe.Packet {
	return fe.Packet{Src: fe.VM(src), Dst: fe.VM(dst), Protocol: proto, Port: port}
}

// TestLinksAtTheFence walks L3 links through silod's fence API with the
// applier running, and decides packets against the policy it leaves
// (fence-api.md, "Links (L3)", SV-15; testing.md, "Crossing silos
// without consent"): a one-sided proposal opens nothing; the mirror
// image opens exactly the named port, one way, between the two sides'
// own spaces and not a descendant's carved out of them; a new block is
// covered; delete closes it; an ancestor/descendant link names each
// side's own space; and an ancestor's links: deny suspends it.
func TestLinksAtTheFence(t *testing.T) {
	e := newEnvBoot(t, obj{"grant_policy": obj{"exports": "allow", "links": "allow"}})
	a := e.create(e.root, e.root, "a", 20, nil)
	b := e.create(e.root, e.root, "b", 20, nil)
	c := e.create(e.root, e.root, "c", 20, nil)
	ac := e.create(e.root, a, "ac", 24, nil)
	ab, bb, cb, acb := e.blocks(a)[0], e.blocks(b)[0], e.blocks(c)[0], e.blocks(ac)[0]
	if ab.Contains(netip.MustParseAddr(last(ab))) && acb.Contains(netip.MustParseAddr(last(ab))) {
		t.Fatalf("a's last address %s is ac's (%s)", last(ab), acb)
	}
	aVM, bVM, cVM, acVM := last(ab), last(bb), last(cb), last(acb)
	ctl := "10.0.254.5"

	// a proposes; b hasn't: no path, no rule.
	l := e.ok(a, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/b", "direction": "outbound",
		"rules": []obj{{"protocol": "tcp", "ports": []string{"5432"}}}})
	if l["state"] != "PENDING" {
		t.Fatalf("one-sided: %v", l)
	}
	if n := e.linkRules(); len(n) != 0 {
		t.Errorf("one-sided proposal wrote %v", n)
	}
	if e.open().Allowed(pk("tcp", aVM, bVM, 5432)) {
		t.Errorf("a reaches b with only a's proposal")
	}

	// b mirrors it: a reaches b on 5432 and nothing else, one way; the
	// call returns once the rules are there.
	l = e.ok(b, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/" + "a", "direction": "inbound",
		"rules": []obj{{"protocol": "TCP", "ports": []string{"5432", "5432"}}}})
	if l["state"] != "ACTIVE" {
		t.Fatalf("mirrored: %v", l)
	}
	if n := e.linkRules(); len(n) != 2 {
		t.Errorf("a one-way link's rules: %v, want 2", n)
	}
	ev := e.open()
	for _, c := range []struct {
		p    fe.Packet
		want bool
	}{
		{pk("tcp", aVM, bVM, 5432), true},
		{pk("tcp", aVM, bVM, 5433), false},
		{pk("udp", aVM, bVM, 5432), false},
		{pk("icmp", aVM, bVM, 0), false},
		{pk("tcp", bVM, aVM, 5432), false},  // one way
		{pk("tcp", acVM, bVM, 5432), false}, // a's descendant is carved out
		{pk("tcp", aVM, cVM, 5432), false},  // a third silo
		{pk("tcp", cVM, bVM, 5432), false},  // a third silo
		{pk("tcp", aVM, ctl, 5432), false},  // the control range
		{pk("tcp", aVM, acVM, 5432), false}, // still no path into ac
	} {
		if got := ev.Allowed(c.p); got != c.want {
			v, _ := ev.Evaluate(c.p)
			t.Errorf("%s: allowed %v, want %v: %s", c.p, got, c.want, v)
		}
	}

	// b grows: its new block is covered too, once addBlock returns.
	e.ok(e.root, "POST", "/silo/v1/silos/-"+b+":addBlock", obj{"prefix_length": 24})
	bs := e.blocks(b)
	if len(bs) != 2 {
		t.Fatalf("b's blocks: %v", bs)
	}
	nb := bs[0]
	if nb == bb {
		nb = bs[1]
	}
	if !e.open().Allowed(pk("tcp", aVM, nb.Addr().Next().String(), 5432)) {
		t.Errorf("a doesn't reach b's new block %s", nb)
	}

	// Either side deletes it: closed again, and the other side's
	// proposal is PENDING.
	e.ok(b, "DELETE", "/silo/v1/self/links/db", nil)
	if n := e.linkRules(); len(n) != 0 {
		t.Errorf("after delete: %v", n)
	}
	if e.open().Allowed(pk("tcp", aVM, bVM, 5432)) {
		t.Errorf("a still reaches b after the delete")
	}
	if l := e.ok(a, "GET", "/silo/v1/self/links", nil)["links"].([]any); len(l) != 1 || l[0].(obj)["state"] != "PENDING" {
		t.Errorf("a's side after b deleted: %v", l)
	}
	e.ok(a, "DELETE", "/silo/v1/self/links/db", nil)

	// An ancestor and its descendant: both ways, udp 53, between a's own
	// space (ac carved out) and ac's (its own child carved out).
	acc := e.create(e.root, ac, "acc", 28, nil)
	accVM := e.blocks(acc)[0].Addr().Next().String()
	e.ok(a, "POST", "/silo/v1/self/links", obj{"name": "dns", "peer": "/a/ac", "direction": "both", "rules": []obj{{"protocol": "udp", "ports": []string{"53"}}}})
	// On ac's behalf, by its ancestor a.
	l = e.ok(a, "POST", "/silo/v1/silos/a/ac/links", obj{"name": "dns", "peer": "/a", "direction": "both", "rules": []obj{{"protocol": "udp", "ports": []string{"53"}}}})
	if l["state"] != "ACTIVE" {
		t.Fatalf("a <-> ac: %v", l)
	}
	if n := e.linkRules(); len(n) != 4 {
		t.Errorf("a two-way link's rules: %v, want 4", n)
	}
	ev = e.open()
	for _, c := range []struct {
		p    fe.Packet
		want bool
	}{
		{pk("udp", aVM, acVM, 53), true},
		{pk("udp", acVM, aVM, 53), true},
		{pk("udp", aVM, acVM, 54), false},
		{pk("tcp", aVM, acVM, 53), false},
		{pk("udp", accVM, aVM, 53), false}, // ac's child is carved out of ac
		{pk("udp", aVM, accVM, 53), false},
		{pk("udp", acVM, accVM, 53), false},
		{pk("udp", acVM, bVM, 53), false},
	} {
		if got := ev.Allowed(c.p); got != c.want {
			v, _ := ev.Evaluate(c.p)
			t.Errorf("%s: allowed %v, want %v: %s", c.p, got, c.want, v)
		}
	}

	// The root denies links to a's subtree: suspended, and closed; then
	// allowed again: open again.
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"grant_policy": obj{"exports": "allow", "links": "deny"}})
	if l := e.ok(ac, "GET", "/silo/v1/self/links", nil)["links"].([]any); len(l) != 1 || l[0].(obj)["state"] != "SUSPENDED" {
		t.Errorf("after the root's deny: %v", l)
	}
	if n := e.linkRules(); len(n) != 0 || e.open().Allowed(pk("udp", aVM, acVM, 53)) {
		t.Errorf("a suspended link still has rules %v", n)
	}
	e.ok(e.root, "PATCH", "/silo/v1/silos/a", obj{"grant_policy": obj{"exports": "allow", "links": "allow"}})
	if !e.open().Allowed(pk("udp", aVM, acVM, 53)) {
		t.Errorf("a resumed link is still closed")
	}
}

// TestLinkRuleBudget: a link's rules count against the fence's rule
// budget, and the proposal that would activate one past it is refused
// with RESOURCE_EXHAUSTED, leaving the peer's proposal PENDING.
func TestLinkRuleBudget(t *testing.T) {
	e := newEnvBoot(t, obj{"grant_policy": obj{"exports": "allow", "links": "allow"}})
	a := e.create(e.root, e.root, "a", 20, nil)
	b := e.create(e.root, e.root, "b", 20, nil)
	rs, _ := e.rules()
	// Room for one more rule within 90%, not for two.
	quota := (len(rs)+1)*100/fence.BudgetPercent + 1
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return fence.SetQuota(ctx, tx, fence.Quota{Rules: quota})
	}); err != nil {
		t.Fatal(err)
	}
	pg := []obj{{"protocol": "tcp", "ports": []string{"5432"}}}
	e.ok(a, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/b", "direction": "outbound", "rules": pg})
	code, out := e.do(b, "POST", "/silo/v1/self/links", obj{"name": "db", "peer": "/a", "direction": "inbound", "rules": pg})
	if er, _ := out["error"].(obj); er == nil || er["code"] != "RESOURCE_EXHAUSTED" {
		t.Fatalf("activating past the budget: %d %v", code, out)
	}
	if l := e.ok(b, "GET", "/silo/v1/self/links", nil)["links"].([]any); len(l) != 0 {
		t.Errorf("a refused proposal was kept: %v", l)
	}
	if l := e.ok(a, "GET", "/silo/v1/self/links", nil)["links"].([]any); len(l) != 1 || l[0].(obj)["state"] != "PENDING" {
		t.Errorf("a's side: %v", l)
	}
}
