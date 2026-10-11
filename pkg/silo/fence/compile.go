// Package fence is the fence compiler and applier for the subnet
// backing: it compiles the global network firewall policy silo-fence
// from silod's database and applies it to GCE rule by rule
// (docs/silo/gcp-backend.md "The fence", SV-57 to SV-60; state.md "What
// the fence does", SV-50; allocators.md "Fence rules and links are
// written per block", SV-43; gateway.md "Layer 2", SV-81).
//
// # Shape
//
// For each silo n with blocks B(n) the policy holds n-intra-in and
// n-intra-out (goto_next, so the tenant's own VPC rules decide inside
// the silo), n-gw-out (TCP 80, 3128 and the export ports to the gateway
// range), n-api-out (TCP 8080 to the gateway range, left out when n's
// effective control policy is none), n-deny-in and n-deny-out. Beside
// them are ctl-deny-out, the gw-* rules, the grants (export-*, link-*)
// and catch-in and catch-out. Every range is written per block: a
// silo's rules name its blocks, never its subnets, so making a subnet
// changes no rule.
//
// # Ordering
//
// Priorities are banded: control 100-999, grants 1000-9999, gateway
// 10000-19999, then a band of 10000 per depth from
// 20000 + (16 - depth) * 10000, deepest first, and the catch-all rules
// from 2147483000. Within its depth's band a silo holds six consecutive
// slots, one per rule in the order above, and keeps them for its life:
// Compile reads the slots from the policy GCE has and gives a new silo
// the lowest slot nobody holds, so adding, removing, sealing or
// unsealing one silo moves no other rule.
package fence

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	compute "google.golang.org/api/compute/v1"
)

// PolicyName is the fence's firewall policy.
const PolicyName = "silo-fence"

// The bands (gcp-backend.md "Ordering", SV-58).
const (
	ControlBand  int64 = 100
	ControlEnd   int64 = 999
	GrantsBand   int64 = 1000
	GrantsEnd    int64 = 9999
	GatewayBand  int64 = 10000
	GatewayEnd   int64 = 19999
	DepthBands   int64 = 20000
	DepthWidth   int64 = 10000
	CatchAllBand int64 = 2147483000
	// MaxDepth is the deepest a silo may be, the root being 0.
	MaxDepth = 16
	// SlotsPerSilo is how many priorities a silo holds in its band.
	SlotsPerSilo = 6
	// gceDefaults is the first of GCE's own default rules, which every
	// network firewall policy has and nobody may change.
	gceDefaults int64 = 2147483644
)

// A silo's rules, by their offset in its slots.
const (
	SlotIntraIn = iota
	SlotIntraOut
	SlotGatewayOut
	SlotAPIOut
	SlotDenyIn
	SlotDenyOut
)

var slotSuffix = [SlotsPerSilo]string{"-intra-in", "-intra-out", "-gw-out", "-api-out", "-deny-in", "-deny-out"}

// The gateway's ports (gateway.md "Address").
const (
	HTTPPort  = 80
	APIPort   = 8080
	ProxyPort = 3128
)

// The fixed rules, with their priorities.
const (
	RuleControlDeny   = "ctl-deny-out"
	RuleGatewayIn     = "gw-in"
	RuleGatewayAdmin  = "gw-admin-in"
	RuleGatewayHealth = "gw-health-in"
	RuleGatewayOut    = "gw-out"
	RuleCatchIn       = "catch-in"
	RuleCatchOut      = "catch-out"
)

var fixedPriority = map[string]int64{
	RuleControlDeny:   ControlBand,
	RuleGatewayIn:     GatewayBand,
	RuleGatewayAdmin:  GatewayBand + 1,
	RuleGatewayHealth: GatewayBand + 2,
	RuleGatewayOut:    GatewayBand + 3,
	RuleCatchIn:       CatchAllBand,
	RuleCatchOut:      CatchAllBand + 1,
}

// IAPRange is where IAP's TCP forwarding comes from.
var IAPRange = netip.MustParsePrefix("35.235.240.0/20")

// HealthCheckRanges are Google's health checkers' ranges.
var HealthCheckRanges = []netip.Prefix{
	netip.MustParsePrefix("35.191.0.0/16"),
	netip.MustParsePrefix("130.211.0.0/22"),
}

// Config is what the policy depends on beyond the database: what silo
// install chose.
type Config struct {
	// ExportPorts are the gateway's TCP export ports, "lo-hi".
	ExportPorts string
	// FrontDoorPort is the gateway's front door, reached from IAP.
	FrontDoorPort int
	// GatewayReplicas is set when install made a gateway MIG, whose
	// health checks gw-health-in lets in.
	GatewayReplicas bool
}

// DefaultConfig is install's defaults.
var DefaultConfig = Config{ExportPorts: "20000-29999", FrontDoorPort: 8443}

// Silo is one silo, as the fence sees it.
type Silo struct {
	ID     string
	Depth  int
	Blocks []netip.Prefix
	// Sealed is set when the silo's effective control policy is none.
	Sealed bool
}

// Grant is one rule a grant adds to the grants band: an export's
// export-<x>-in or one direction of a link. Its action is allow. Grants
// are built by later tasks; the compiler takes them as they are.
type Grant struct {
	Name      string // the rule name, unique: "export-x-in", "link-l-out"
	Direction string // "INGRESS" or "EGRESS"
	Src, Dst  []netip.Prefix
	L4        []L4
}

// L4 is a protocol and its ports, as a layer4Config.
type L4 struct {
	Protocol string
	Ports    []string
}

// Input is everything the policy is compiled from.
type Input struct {
	// Gateway and Control are the gateway and control ranges, and Root
	// the root's block less both. A database with no root yet has none
	// of them, and its policy is the catch-all rules alone.
	Gateway, Control netip.Prefix
	Root             []netip.Prefix
	Silos            []Silo
	Grants           []Grant
}

// Compile compiles in into the fence's rules, in priority order, with
// GCE's default rules left out. cur is the policy's rules as GCE has
// them now (nil for none): each silo and grant already there keeps its
// priority, and a new one takes the lowest priority in its band that
// no rule in cur holds, so applying the result moves nothing.
func Compile(in *Input, cfg Config, cur []*compute.FirewallPolicyRule) ([]*compute.FirewallPolicyRule, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	var out []*compute.FirewallPolicyRule
	add := func(name string, priority int64, dir, action string, src, dst []netip.Prefix, l4 ...L4) {
		out = append(out, newRule(name, priority, dir, action, src, dst, l4))
	}
	all := []L4{{Protocol: "all"}}
	anywhere := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	known := in.Gateway.IsValid() && in.Control.IsValid() && len(in.Root) > 0
	if known {
		gw := []netip.Prefix{in.Gateway}
		add(RuleControlDeny, fixedPriority[RuleControlDeny], "EGRESS", "deny", in.Root, []netip.Prefix{in.Control}, all...)
		grants, err := grantRules(in.Grants, cur)
		if err != nil {
			return nil, err
		}
		out = append(out, grants...)
		gwPorts := L4{Protocol: "tcp", Ports: []string{strconv.Itoa(HTTPPort), strconv.Itoa(APIPort), strconv.Itoa(ProxyPort), cfg.ExportPorts}}
		add(RuleGatewayIn, fixedPriority[RuleGatewayIn], "INGRESS", "allow", in.Root, gw, gwPorts)
		add(RuleGatewayAdmin, fixedPriority[RuleGatewayAdmin], "INGRESS", "allow", []netip.Prefix{IAPRange}, gw,
			L4{Protocol: "tcp", Ports: []string{strconv.Itoa(cfg.FrontDoorPort)}})
		if cfg.GatewayReplicas {
			add(RuleGatewayHealth, fixedPriority[RuleGatewayHealth], "INGRESS", "allow", HealthCheckRanges, gw, gwPorts)
		}
		add(RuleGatewayOut, fixedPriority[RuleGatewayOut], "EGRESS", "goto_next", gw, anywhere, all...)
		silos, err := siloRules(in, cfg, cur)
		if err != nil {
			return nil, err
		}
		out = append(out, silos...)
	} else if len(in.Silos) > 0 || len(in.Grants) > 0 {
		return nil, fmt.Errorf("fence: silos or grants with no gateway, control and root ranges")
	}
	add(RuleCatchIn, fixedPriority[RuleCatchIn], "INGRESS", "deny", anywhere, nil, all...)
	add(RuleCatchOut, fixedPriority[RuleCatchOut], "EGRESS", "deny", nil, anywhere, all...)
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

func (c Config) check() error {
	if _, _, err := parsePortRange(c.ExportPorts); err != nil {
		return fmt.Errorf("fence: export ports %q: %v", c.ExportPorts, err)
	}
	if c.FrontDoorPort <= 0 || c.FrontDoorPort > 65535 {
		return fmt.Errorf("fence: front door port %d", c.FrontDoorPort)
	}
	return nil
}

func parsePortRange(s string) (lo, hi int, err error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		b = a
	}
	if lo, err = strconv.Atoi(a); err == nil {
		hi, err = strconv.Atoi(b)
	}
	if err != nil || lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, fmt.Errorf("want a port or a range lo-hi")
	}
	return lo, hi, nil
}

// DepthBand is the first priority of depth's band.
func DepthBand(depth int) int64 { return DepthBands + int64(MaxDepth-depth)*DepthWidth }

// slotsPerBand is how many silos one depth's band holds.
const slotsPerBand = int(DepthWidth / SlotsPerSilo)

// SiloSlot reads a rule of cur as one of a silo's: its id, depth and
// slot index in the depth's band. ok is false for any other rule.
func SiloSlot(r *compute.FirewallPolicyRule) (id string, depth, index int, ok bool) {
	if r.Priority < DepthBands || r.Priority >= DepthBand(-1) {
		return "", 0, 0, false
	}
	depth = MaxDepth - int((r.Priority-DepthBands)/DepthWidth)
	off := int(r.Priority - DepthBand(depth))
	index, slot := off/SlotsPerSilo, off%SlotsPerSilo
	id, found := strings.CutSuffix(r.RuleName, slotSuffix[slot])
	if !found || id == "" || index >= slotsPerBand {
		return "", 0, 0, false
	}
	return id, depth, index, true
}

func siloRules(in *Input, cfg Config, cur []*compute.FirewallPolicyRule) ([]*compute.FirewallPolicyRule, error) {
	type key struct {
		id    string
		depth int
	}
	held := map[key]int{}           // the slot index a silo holds in cur
	taken := map[int]map[int]bool{} // depth -> index -> held by some rule in cur
	for _, r := range cur {
		if r.Priority < DepthBands || r.Priority >= DepthBand(-1) {
			continue
		}
		depth := MaxDepth - int((r.Priority-DepthBands)/DepthWidth)
		index := int(r.Priority-DepthBand(depth)) / SlotsPerSilo
		if taken[depth] == nil {
			taken[depth] = map[int]bool{}
		}
		taken[depth][index] = true
		if id, d, i, ok := SiloSlot(r); ok {
			held[key{id, d}] = i
		}
	}
	silos := append([]Silo(nil), in.Silos...)
	sort.Slice(silos, func(i, j int) bool { return silos[i].ID < silos[j].ID })
	seen := map[string]bool{}
	var out []*compute.FirewallPolicyRule
	var fresh []Silo
	place := func(n Silo, index int) {
		b := siloRanges(n.Blocks, in)
		gw := []netip.Prefix{in.Gateway}
		base := DepthBand(n.Depth) + int64(index*SlotsPerSilo)
		all := L4{Protocol: "all"}
		out = append(out,
			newRule(n.ID+slotSuffix[SlotIntraIn], base+SlotIntraIn, "INGRESS", "goto_next", b, b, []L4{all}),
			newRule(n.ID+slotSuffix[SlotIntraOut], base+SlotIntraOut, "EGRESS", "goto_next", b, b, []L4{all}),
			newRule(n.ID+slotSuffix[SlotGatewayOut], base+SlotGatewayOut, "EGRESS", "allow", b, gw,
				[]L4{{Protocol: "tcp", Ports: []string{strconv.Itoa(HTTPPort), strconv.Itoa(ProxyPort), cfg.ExportPorts}}}),
		)
		if !n.Sealed {
			out = append(out, newRule(n.ID+slotSuffix[SlotAPIOut], base+SlotAPIOut, "EGRESS", "allow", b, gw,
				[]L4{{Protocol: "tcp", Ports: []string{strconv.Itoa(APIPort)}}}))
		}
		anywhere := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
		out = append(out,
			newRule(n.ID+slotSuffix[SlotDenyIn], base+SlotDenyIn, "INGRESS", "deny", anywhere, b, []L4{all}),
			newRule(n.ID+slotSuffix[SlotDenyOut], base+SlotDenyOut, "EGRESS", "deny", b, anywhere, []L4{all}),
		)
	}
	for _, n := range silos {
		if n.ID == "" || seen[n.ID] {
			return nil, fmt.Errorf("fence: silo id %q is empty or repeated", n.ID)
		}
		seen[n.ID] = true
		if n.Depth < 0 || n.Depth > MaxDepth {
			return nil, fmt.Errorf("fence: silo %s is at depth %d, past the cap of %d", n.ID, n.Depth, MaxDepth)
		}
		// A silo with no blocks (being created or deleted) has nothing
		// for the fence to name, and holds no slot.
		if len(siloRanges(n.Blocks, in)) == 0 {
			continue
		}
		if i, ok := held[key{n.ID, n.Depth}]; ok {
			place(n, i)
			continue
		}
		fresh = append(fresh, n)
	}
	next := map[int]int{}
	for _, n := range fresh {
		i := next[n.Depth]
		for taken[n.Depth][i] {
			i++
		}
		if i >= slotsPerBand {
			return nil, fmt.Errorf("fence: depth %d's band holds at most %d silos", n.Depth, slotsPerBand)
		}
		if taken[n.Depth] == nil {
			taken[n.Depth] = map[int]bool{}
		}
		taken[n.Depth][i] = true
		next[n.Depth] = i + 1
		place(n, i)
	}
	return out, nil
}

// siloRanges is a silo's blocks as the fence names them: less the
// gateway and control ranges, which lie inside the root's block but
// are no silo's space.
func siloRanges(blocks []netip.Prefix, in *Input) []netip.Prefix {
	var out []netip.Prefix
	for _, b := range blocks {
		out = append(out, Minus(b.Masked(), in.Gateway, in.Control)...)
	}
	return normalize(out)
}

func grantRules(gs []Grant, cur []*compute.FirewallPolicyRule) ([]*compute.FirewallPolicyRule, error) {
	held := map[string]int64{}
	taken := map[int64]bool{}
	for _, r := range cur {
		if r.Priority >= GrantsBand && r.Priority <= GrantsEnd {
			taken[r.Priority] = true
			if _, dup := held[r.RuleName]; !dup && r.RuleName != "" {
				held[r.RuleName] = r.Priority
			}
		}
	}
	gs = append([]Grant(nil), gs...)
	sort.Slice(gs, func(i, j int) bool { return gs[i].Name < gs[j].Name })
	var out []*compute.FirewallPolicyRule
	var fresh []Grant
	seen := map[string]bool{}
	for _, g := range gs {
		if seen[g.Name] || !(strings.HasPrefix(g.Name, "export-") || strings.HasPrefix(g.Name, "link-")) {
			return nil, fmt.Errorf("fence: grant rule %q is repeated, or not an export- or link- rule", g.Name)
		}
		seen[g.Name] = true
		if g.Direction != "INGRESS" && g.Direction != "EGRESS" || len(g.Src) == 0 || len(g.Dst) == 0 || len(g.L4) == 0 {
			return nil, fmt.Errorf("fence: grant rule %s needs a direction, both ends and a protocol", g.Name)
		}
		if p, ok := held[g.Name]; ok {
			out = append(out, newRule(g.Name, p, g.Direction, "allow", g.Src, g.Dst, g.L4))
			continue
		}
		fresh = append(fresh, g)
	}
	p := GrantsBand
	for _, g := range fresh {
		for taken[p] {
			p++
		}
		if p > GrantsEnd {
			return nil, fmt.Errorf("fence: the grants band holds at most %d rules", GrantsEnd-GrantsBand+1)
		}
		taken[p] = true
		out = append(out, newRule(g.Name, p, g.Direction, "allow", g.Src, g.Dst, g.L4))
	}
	return out, nil
}

func newRule(name string, priority int64, dir, action string, src, dst []netip.Prefix, l4 []L4) *compute.FirewallPolicyRule {
	m := &compute.FirewallPolicyRuleMatcher{SrcIpRanges: strs(src), DestIpRanges: strs(dst)}
	for _, c := range l4 {
		m.Layer4Configs = append(m.Layer4Configs, &compute.FirewallPolicyRuleMatcherLayer4Config{
			IpProtocol: c.Protocol, Ports: append([]string(nil), c.Ports...)})
	}
	return &compute.FirewallPolicyRule{
		RuleName: name, Priority: priority, Direction: dir, Action: action, Match: m,
		Description: "silo: compiled by silod",
	}
}

func strs(ps []netip.Prefix) []string {
	ps = normalize(ps)
	if len(ps) == 0 {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}
