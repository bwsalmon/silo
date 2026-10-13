package gcefence

import (
	"fmt"
	"math/rand"
	"net/netip"
	"sort"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/fake/fenceeval"
	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/fence"
	compute "google.golang.org/api/compute/v1"
)

// The address plan every generated tree uses: a /16 with the gateway and
// control ranges held back, as silo install holds them back.
var (
	genSpace   = netip.MustParsePrefix("10.0.0.0/16")
	genGateway = netip.MustParsePrefix("10.0.255.0/24")
	genControl = netip.MustParsePrefix("10.0.254.0/24")
	genGWVM    = netip.MustParseAddr("10.0.255.2")
	genPSC     = netip.MustParseAddr("10.0.254.5")
)

// gsilo is one silo of a generated tree.
type gsilo struct {
	id      string
	parent  string
	depth   int
	blocks  []netip.Prefix
	control string // its own control policy
}

// gtree is a generated tree. Blocks nest as the allocator nests them: a
// child's block is carved from its parent's free space.
type gtree struct {
	silos map[string]*gsilo
	order []string // creation order
	next  int
}

func newTree() *gtree {
	t := &gtree{silos: map[string]*gsilo{}}
	t.silos["root"] = &gsilo{id: "root", blocks: []netip.Prefix{genSpace}, control: control.Full}
	t.order = []string{"root"}
	return t
}

func (t *gtree) clone() *gtree {
	c := &gtree{silos: map[string]*gsilo{}, order: append([]string(nil), t.order...), next: t.next}
	for id, s := range t.silos {
		cp := *s
		cp.blocks = append([]netip.Prefix(nil), s.blocks...)
		c.silos[id] = &cp
	}
	return c
}

func (t *gtree) children(id string) []*gsilo {
	var out []*gsilo
	for _, cid := range t.order {
		if s := t.silos[cid]; s != nil && s.parent == id {
			out = append(out, s)
		}
	}
	return out
}

// ranges is a silo's blocks as the fence names them.
func (t *gtree) ranges(id string) []netip.Prefix {
	var out []netip.Prefix
	for _, b := range t.silos[id].blocks {
		out = append(out, fence.Minus(b, genGateway, genControl)...)
	}
	return fence.Normalize(out)
}

// free is the space in a silo's blocks that no child holds (and, in
// the root's, outside the gateway and control ranges).
func (t *gtree) free(id string) []netip.Prefix {
	var out []netip.Prefix
	for _, b := range t.ranges(id) {
		ps := []netip.Prefix{b}
		for _, c := range t.children(id) {
			for _, cb := range c.blocks {
				var next []netip.Prefix
				for _, p := range ps {
					next = append(next, fence.Minus(p, cb)...)
				}
				ps = next
			}
		}
		out = append(out, ps...)
	}
	return fence.Normalize(out)
}

// carve takes a block of bits from id's free space, or false.
func (t *gtree) carve(r *rand.Rand, id string, bits int) (netip.Prefix, bool) {
	var fits []netip.Prefix
	for _, f := range t.free(id) {
		if f.Bits() <= bits {
			fits = append(fits, f)
		}
	}
	if len(fits) == 0 {
		return netip.Prefix{}, false
	}
	f := fits[r.Intn(len(fits))]
	// The first aligned /bits in f, or a random one of them.
	n := 1 << (bits - f.Bits())
	k := r.Intn(n)
	a := f.Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v += uint32(k) << (32 - bits)
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), bits), true
}

// addChild adds a child of parent with one block, or returns "".
func (t *gtree) addChild(r *rand.Rand, parent string) string {
	p := t.silos[parent]
	if p.depth >= MaxDepth {
		return ""
	}
	b, ok := t.carve(r, parent, minInt(28, p.depthBits()+2+r.Intn(3)))
	if !ok {
		return ""
	}
	t.next++
	id := fmt.Sprintf("s%d", t.next)
	t.silos[id] = &gsilo{id: id, parent: parent, depth: p.depth + 1, blocks: []netip.Prefix{b}, control: randControl(r)}
	t.order = append(t.order, id)
	return id
}

func (s *gsilo) depthBits() int {
	bits := 32
	for _, b := range s.blocks {
		if b.Bits() < bits {
			bits = b.Bits()
		}
	}
	return bits
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func randControl(r *rand.Rand) string {
	return []string{control.Full, control.Full, control.Read, control.None}[r.Intn(4)]
}

// randomTree is a tree of up to n silos below the root.
func randomTree(r *rand.Rand, n int) *gtree {
	t := newTree()
	for i := 0; i < n; i++ {
		ids := t.ids()
		t.addChild(r, ids[r.Intn(len(ids))])
	}
	// A second block for some.
	for _, id := range t.ids() {
		s := t.silos[id]
		if id != "root" && r.Intn(3) == 0 {
			if b, ok := t.carve(r, s.parent, minInt(28, s.depthBits()+r.Intn(2))); ok {
				s.blocks = append(s.blocks, b)
			}
		}
	}
	return t
}

func (t *gtree) ids() []string {
	var out []string
	for _, id := range t.order {
		if t.silos[id] != nil {
			out = append(out, id)
		}
	}
	return out
}

// effective is a silo's effective control policy: the root's is full,
// and each other's the stricter of its parent's and its own.
func (t *gtree) effective(id string) string {
	s := t.silos[id]
	if s.parent == "" {
		return control.Full
	}
	return control.Strictest(t.effective(s.parent), s.control)
}

func (t *gtree) sealed(id string) bool { return fence.Sealed(t.effective(id)) }

func (t *gtree) input() *fence.Input {
	in := &fence.Input{Gateway: genGateway, Control: genControl, Root: fence.Minus(genSpace, genGateway, genControl)}
	for _, id := range t.ids() {
		s := t.silos[id]
		in.Silos = append(in.Silos, fence.Silo{ID: id, Depth: s.depth, Blocks: s.blocks, Sealed: t.sealed(id)})
	}
	return in
}

// owner is the silo whose own space holds a: the deepest whose ranges
// contain it, or "" outside every silo.
func (t *gtree) owner(a netip.Addr) string {
	best, depth := "", -1
	for _, id := range t.ids() {
		for _, b := range t.ranges(id) {
			if b.Contains(a) && t.silos[id].depth > depth {
				best, depth = id, t.silos[id].depth
			}
		}
	}
	return best
}

// vms are a few addresses in each silo's own space: its free space's
// first and last addresses, and one in the middle.
func (t *gtree) vms(r *rand.Rand) map[string][]netip.Addr {
	out := map[string][]netip.Addr{}
	for _, id := range t.ids() {
		for _, f := range t.free(id) {
			last := fence.LastAddr(f)
			out[id] = append(out[id], f.Addr(), last)
			if f.Bits() < 31 {
				a := f.Addr()
				for i := r.Intn(1 << (32 - f.Bits() - 1)); i > 0; i-- {
					a = a.Next()
				}
				out[id] = append(out[id], a)
			}
		}
		sort.Slice(out[id], func(i, j int) bool { return out[id][i].Less(out[id][j]) })
	}
	return out
}

// policy is rules as a policy GCE would hold.
func policyOf(rules []*compute.FirewallPolicyRule) *compute.FirewallPolicy {
	return &compute.FirewallPolicy{Name: PolicyName, Rules: rules}
}

func evalSteps(steps []Step) []fenceeval.Step {
	out := make([]fenceeval.Step, len(steps))
	for i, s := range steps {
		out[i] = fenceeval.Step{Op: fenceeval.Op(s.Op), Priority: s.Priority, Rule: s.Rule}
	}
	return out
}

// tenantRules are what a hostile set of tenants can write: each silo's
// rules allow everything to and from anywhere, pinned to the silo's
// tag the way the facade pins them, and one rule with no target that
// allows everything everywhere, as an unpinned rule would.
func (t *gtree) tenantRules() []*compute.Firewall {
	all := []*compute.FirewallAllowed{{IPProtocol: "all"}}
	var out []*compute.Firewall
	for _, id := range t.ids() {
		out = append(out,
			&compute.Firewall{Name: id + "-all-in", Direction: "INGRESS", Allowed: all, SourceRanges: []string{"0.0.0.0/0"}, TargetTags: []string{id}},
			&compute.Firewall{Name: id + "-all-out", Direction: "EGRESS", Allowed: all, DestinationRanges: []string{"0.0.0.0/0"}, TargetTags: []string{id}},
		)
	}
	return append(out,
		&compute.Firewall{Name: "wide-open-in", Direction: "INGRESS", Allowed: all, SourceRanges: []string{"0.0.0.0/0"}},
		&compute.Firewall{Name: "wide-open-out", Direction: "EGRESS", Allowed: all, DestinationRanges: []string{"0.0.0.0/0"}},
	)
}

// hostileGrants are grant rules written as badly as a grant could be:
// allowing every protocol from every silo's space to the control range
// and to everywhere, both ways.
func (t *gtree) hostileGrants(r *rand.Rand) []fence.Grant {
	anywhere := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	all := []fence.L4{{Protocol: "all"}}
	var out []fence.Grant
	for i, id := range t.ids() {
		b := t.ranges(id)
		out = append(out,
			fence.Grant{Name: fmt.Sprintf("link-%d-ctl-out", i), Direction: "EGRESS", Src: b, Dst: []netip.Prefix{genControl}, L4: all},
			fence.Grant{Name: fmt.Sprintf("export-%d-in", i), Direction: "INGRESS", Src: anywhere, Dst: []netip.Prefix{genControl}, L4: all},
		)
		if r.Intn(2) == 0 {
			out = append(out, fence.Grant{Name: fmt.Sprintf("link-%d-any-out", i), Direction: "EGRESS", Src: b, Dst: anywhere, L4: all})
		}
	}
	return out
}
