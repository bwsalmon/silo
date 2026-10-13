package fence

import (
	"context"
	"net/netip"

	"github.com/bwsalmon/silo/pkg/ledger/address"
	"github.com/bwsalmon/silo/pkg/links"
	"github.com/bwsalmon/silo/pkg/store"
)

// OwnSpaces is every holder's own space as the fence names it: its
// blocks, less the gateway and control ranges, and less every other
// holder's block that lies inside one of them, which is a descendant's
// (or the gateway's or control's, inside the root's). Blocks either nest
// or are disjoint, so the result holds exactly the addresses whose
// longest-prefix owner is the holder (ledger/address, "A silo's own
// space"), and no two holders' own spaces overlap.
func OwnSpaces(blocks map[string][]netip.Prefix, gateway, control netip.Prefix) map[string][]netip.Prefix {
	type held struct {
		holder string
		block  netip.Prefix
	}
	var all []held
	for h, bs := range blocks {
		for _, b := range bs {
			all = append(all, held{h, b.Masked()})
		}
	}
	out := map[string][]netip.Prefix{}
	for _, x := range all {
		if x.holder == address.SystemGateway || x.holder == address.SystemControl {
			continue
		}
		holes := []netip.Prefix{gateway, control}
		for _, y := range all {
			if y.block.Bits() > x.block.Bits() && x.block.Contains(y.block.Addr()) {
				holes = append(holes, y.block)
			}
		}
		out[x.holder] = append(out[x.holder], Minus(x.block, holes...)...)
	}
	for h, ps := range out {
		out[h] = Normalize(ps)
	}
	return out
}

// LinkGrant is an ACTIVE link's rules, between a's and b's own spaces
// and nothing else, for l4 alone: link-<l>-out (egress, leaving a) and
// link-<l>-in (ingress, arriving in b) for connections starting in a
// when forward, and the same from b to a when reverse; a two-way
// link's second direction is link-<l>-rev-out and link-<l>-rev-in. A
// side with no own space (every block delegated, or none) leaves the
// link with no rules, since a rule must name both ends.
func LinkGrant(link string, a, b []netip.Prefix, forward, reverse bool, l4 []L4) []Grant {
	if len(a) == 0 || len(b) == 0 || len(l4) == 0 {
		return nil
	}
	out, in, revOut, revIn := links.RuleNames(link)
	var gs []Grant
	pair := func(o, i string, src, dst []netip.Prefix) {
		gs = append(gs,
			Grant{Name: o, Direction: "EGRESS", Src: src, Dst: dst, L4: l4},
			Grant{Name: i, Direction: "INGRESS", Src: src, Dst: dst, L4: l4})
	}
	if forward {
		pair(out, in, a, b)
	}
	if reverse {
		if forward {
			pair(revOut, revIn, b, a)
		} else {
			pair(out, in, b, a)
		}
	}
	return gs
}

// LinkGrants is the links' grant rules (fence-api.md, "Links (L3)",
// SV-15; gcp-backend.md, "The fence", SV-57): for each ACTIVE link,
// LinkGrant between its two sides' own spaces as they are now, so a
// block added on either side is covered at the next apply, and a block
// delegated to a descendant (or a third silo's anywhere) never is. A
// PENDING or SUSPENDED link has no rule.
func LinkGrants(ctx context.Context, tx store.Tx) ([]Grant, error) {
	pairs, err := links.Pairs(ctx, tx)
	if err != nil || len(pairs) == 0 {
		return nil, err
	}
	blocks, err := address.AllBlocks(ctx, tx)
	if err != nil {
		return nil, err
	}
	var gw, ctl netip.Prefix
	if b := blocks[address.SystemGateway]; len(b) == 1 {
		gw = b[0]
	}
	if b := blocks[address.SystemControl]; len(b) == 1 {
		ctl = b[0]
	}
	if !gw.IsValid() || !ctl.IsValid() {
		return nil, nil
	}
	own := OwnSpaces(blocks, gw, ctl)
	var out []Grant
	for _, p := range pairs {
		a, b := p[0], p[1]
		if a.State != links.StateActive || b.State != links.StateActive || !links.Mirrors(a, b) {
			continue
		}
		var l4 []L4
		for _, r := range a.Rules {
			l4 = append(l4, L4{Protocol: r.Protocol, Ports: append([]string(nil), r.Ports...)})
		}
		out = append(out, LinkGrant(a.LinkID, own[a.Silo], own[b.Silo], a.Starts(), a.Accepts(), l4)...)
	}
	return out, nil
}

// AllGrants is every grant rule the fence carries: the exports' and the
// links'.
func AllGrants(ctx context.Context, tx store.Tx) ([]Grant, error) {
	es, err := ExportGrants(ctx, tx)
	if err != nil {
		return nil, err
	}
	ls, err := LinkGrants(ctx, tx)
	if err != nil {
		return nil, err
	}
	return append(es, ls...), nil
}
