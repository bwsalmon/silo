package fenceeval

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

// The enforcement orders of a network's firewall policy.
const (
	BeforeClassic = "BEFORE_CLASSIC_FIREWALL"
	AfterClassic  = "AFTER_CLASSIC_FIREWALL"
)

// Policy rule actions.
const (
	Allow    = "allow"
	Deny     = "deny"
	GotoNext = "goto_next"
)

// The stages that can decide a packet, as Decision.Stage.
const (
	StagePolicy  = "policy"
	StageVPC     = "vpc"
	StageImplied = "implied"
)

// Names of the implied rules, as Decision.Rule.
const (
	ImpliedAllowEgress = "implied-allow-egress"
	ImpliedDenyIngress = "implied-deny-ingress"
)

// Endpoint is one end of a packet.
type Endpoint struct {
	Addr netip.Addr
	// Tags are the network tags of the VM, for VPC rules' targetTags
	// and sourceTags.
	Tags []string
	// External is set when the endpoint is not a VM NIC in the network:
	// the internet, a Private Service Connect endpoint, a Google API.
	// No firewall is evaluated on its side of the packet.
	External bool
}

// VM is a VM in the network at addr with the given tags. It panics if
// addr doesn't parse.
func VM(addr string, tags ...string) Endpoint {
	return Endpoint{Addr: netip.MustParseAddr(addr), Tags: tags}
}

// Outside is an address that is not a VM in the network. It panics if
// addr doesn't parse.
func Outside(addr string) Endpoint {
	return Endpoint{Addr: netip.MustParseAddr(addr), External: true}
}

func (e Endpoint) String() string {
	switch {
	case e.External:
		return e.Addr.String() + " (outside)"
	case len(e.Tags) > 0:
		return e.Addr.String() + " [" + strings.Join(e.Tags, ",") + "]"
	}
	return e.Addr.String()
}

// Packet is the first packet of a connection.
type Packet struct {
	Src, Dst Endpoint
	// Protocol is a name GCE accepts (tcp, udp, icmp, esp, ah, sctp,
	// ipip) or an IP protocol number.
	Protocol string
	// Port is the destination port, for tcp, udp and sctp.
	Port int
}

// TCP is a TCP packet from src to dst's port.
func TCP(src, dst Endpoint, port int) Packet {
	return Packet{Src: src, Dst: dst, Protocol: "tcp", Port: port}
}

func (p Packet) String() string {
	s := fmt.Sprintf("%s %s -> %s", p.Protocol, p.Src, p.Dst)
	if n, err := parseProto(p.Protocol, false); err == nil && hasPorts(n) {
		s += fmt.Sprintf(" port %d", p.Port)
	}
	return s
}

// Decision is what one side's firewall decided.
type Decision struct {
	Allowed bool
	// Stage is the stage whose rule decided: StagePolicy, StageVPC or
	// StageImplied. It is empty when that side was not evaluated
	// because its endpoint is External.
	Stage string
	// Rule names the deciding rule: a policy rule's ruleName (or
	// "priority N" when it has none), a VPC firewall rule's name, or
	// ImpliedAllowEgress or ImpliedDenyIngress.
	Rule     string
	Priority int64
	// Handoff names the policy rule whose goto_next passed the packet
	// on, when a later stage decided. It is empty when no policy rule
	// matched, or when the policy was not reached.
	Handoff string
}

func (d Decision) String() string {
	if d.Stage == "" {
		return "not evaluated"
	}
	verb := "denied"
	if d.Allowed {
		verb = "allowed"
	}
	s := fmt.Sprintf("%s by %s rule %s", verb, d.Stage, d.Rule)
	if d.Handoff != "" {
		s += " after goto_next " + d.Handoff
	}
	return s
}

// Verdict is the evaluator's answer for a packet: Allowed only when
// both the sender's egress and the receiver's ingress allow it.
type Verdict struct {
	Allowed bool
	Egress  Decision
	Ingress Decision
}

func (v Verdict) String() string {
	verb := "denied"
	if v.Allowed {
		verb = "allowed"
	}
	return fmt.Sprintf("%s (egress %s; ingress %s)", verb, v.Egress, v.Ingress)
}

// policyRule is a parsed, enabled network firewall policy rule.
type policyRule struct {
	name     string
	priority int64
	ingress  bool
	action   string
	src, dst []netip.Prefix // nil: any
	l4       []l4
}

func (r *policyRule) matches(ingress bool, src, dst netip.Addr, proto, port int) bool {
	return r.ingress == ingress &&
		(r.src == nil || inRanges(r.src, src)) &&
		(r.dst == nil || inRanges(r.dst, dst)) &&
		matchL4(r.l4, proto, port)
}

// vpcRule is a parsed, enabled VPC firewall rule.
type vpcRule struct {
	name       string
	priority   int64
	ingress    bool
	deny       bool
	src, dst   []netip.Prefix // nil: no limit, after GCE's defaults
	srcTags    []string
	targetTags []string
	l4         []l4
}

func (r *vpcRule) matches(ingress bool, p *packet) bool {
	if r.ingress != ingress || !matchL4(r.l4, p.proto, p.port) {
		return false
	}
	target := p.src
	if ingress {
		target = p.dst
	}
	if len(r.targetTags) > 0 && !hasAny(target.Tags, r.targetTags) {
		return false
	}
	if r.dst != nil && !inRanges(r.dst, p.dst.Addr) {
		return false
	}
	if !ingress {
		return r.src == nil || inRanges(r.src, p.src.Addr)
	}
	return inRanges(r.src, p.src.Addr) ||
		(len(r.srcTags) > 0 && !p.src.External && hasAny(p.src.Tags, r.srcTags))
}

// Evaluator answers whether packets are allowed by one network's
// firewall policy and VPC firewall rules. It is immutable once made.
type Evaluator struct {
	order  string
	policy []*policyRule
	vpc    []*vpcRule
}

// New makes an Evaluator for a network whose firewall policy is policy
// (nil for none) and whose VPC firewall rules are firewalls, with the
// given enforcement order ("" is BeforeClassic, Silo's). Rules are taken
// as GCE stores them, with GCE's defaults filled in where a field is
// left out. It refuses a rule using something the evaluator doesn't
// model (see the package doc), rather than evaluate it wrongly.
func New(order string, policy *compute.FirewallPolicy, firewalls []*compute.Firewall) (*Evaluator, error) {
	if order == "" {
		order = BeforeClassic
	}
	if order != BeforeClassic && order != AfterClassic {
		return nil, fmt.Errorf("fenceeval: enforcement order %q", order)
	}
	e := &Evaluator{order: order}
	if policy != nil {
		rs, err := parsePolicy(policy.Rules)
		if err != nil {
			return nil, err
		}
		e.policy = rs
	}
	for _, fw := range firewalls {
		r, err := parseFirewall(fw)
		if err != nil {
			return nil, err
		}
		if r != nil {
			e.vpc = append(e.vpc, r)
		}
	}
	sort.SliceStable(e.vpc, func(i, j int) bool {
		a, b := e.vpc[i], e.vpc[j]
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		return a.deny && !b.deny
	})
	return e, nil
}

// Load makes an Evaluator for network in project from what c reports:
// its enforcement order, the network firewall policy associated with
// it, and its VPC firewall rules. c may be the fake or real GCE.
func Load(ctx context.Context, c gce.Client, project, network string) (*Evaluator, error) {
	n, err := c.GetNetwork(ctx, project, network)
	if err != nil {
		return nil, err
	}
	var pol *compute.FirewallPolicy
	if n.FirewallPolicy != "" {
		if pol, err = c.GetFirewallPolicy(ctx, project, gce.ParseRef(n.FirewallPolicy).Name); err != nil {
			return nil, err
		}
	}
	all, err := c.ListFirewalls(ctx, project, gce.ListFilter{})
	if err != nil {
		return nil, err
	}
	var fws []*compute.Firewall
	for _, fw := range all {
		if gce.ParseRef(fw.Network).Name == n.Name {
			fws = append(fws, fw)
		}
	}
	return New(n.NetworkFirewallPolicyEnforcementOrder, pol, fws)
}

func ruleName(r *compute.FirewallPolicyRule) string {
	if r.RuleName != "" {
		return r.RuleName
	}
	return fmt.Sprintf("priority %d", r.Priority)
}

// parsePolicy parses a policy's enabled rules, in priority order.
func parsePolicy(rules []*compute.FirewallPolicyRule) ([]*policyRule, error) {
	var out []*policyRule
	seen := map[int64]bool{}
	for _, r := range rules {
		pr, err := parsePolicyRule(r)
		if err != nil {
			return nil, fmt.Errorf("fenceeval: policy rule %s: %w", ruleName(r), err)
		}
		if seen[r.Priority] {
			return nil, fmt.Errorf("fenceeval: two policy rules have priority %d", r.Priority)
		}
		seen[r.Priority] = true
		if pr != nil {
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].priority < out[j].priority })
	return out, nil
}

func parsePolicyRule(r *compute.FirewallPolicyRule) (*policyRule, error) {
	switch r.Action {
	case Allow, Deny, GotoNext:
	default:
		return nil, fmt.Errorf("action %q is not modelled", r.Action)
	}
	if r.Direction != "INGRESS" && r.Direction != "EGRESS" {
		return nil, fmt.Errorf("direction %q", r.Direction)
	}
	if len(r.TargetResources)+len(r.TargetSecureTags)+len(r.TargetServiceAccounts) > 0 {
		return nil, fmt.Errorf("rule targets are not modelled")
	}
	if r.SecurityProfileGroup != "" || r.TlsInspect {
		return nil, fmt.Errorf("security profiles are not modelled")
	}
	m := r.Match
	if m == nil || len(m.Layer4Configs) == 0 {
		return nil, fmt.Errorf("no layer4Configs")
	}
	if len(m.DestAddressGroups)+len(m.DestFqdns)+len(m.DestRegionCodes)+len(m.DestThreatIntelligences)+
		len(m.SrcAddressGroups)+len(m.SrcFqdns)+len(m.SrcRegionCodes)+len(m.SrcThreatIntelligences)+
		len(m.SrcSecureTags)+len(m.SrcNetworks) > 0 ||
		m.DestNetworkType != "" || m.SrcNetworkType != "" || m.DestNetworkContext != "" || m.SrcNetworkContext != "" {
		return nil, fmt.Errorf("matchers other than IP ranges and layer4Configs are not modelled")
	}
	pr := &policyRule{name: ruleName(r), priority: r.Priority, ingress: r.Direction == "INGRESS", action: r.Action}
	var err error
	if pr.src, err = parseRanges(m.SrcIpRanges); err != nil {
		return nil, err
	}
	if pr.dst, err = parseRanges(m.DestIpRanges); err != nil {
		return nil, err
	}
	for _, c := range m.Layer4Configs {
		x, err := parseL4(c.IpProtocol, c.Ports)
		if err != nil {
			return nil, err
		}
		pr.l4 = append(pr.l4, x)
	}
	if r.Disabled {
		return nil, nil
	}
	return pr, nil
}

// parseFirewall parses a VPC firewall rule, nil if it is disabled.
func parseFirewall(fw *compute.Firewall) (*vpcRule, error) {
	r := &vpcRule{name: fw.Name, priority: fw.Priority, ingress: fw.Direction != "EGRESS"}
	if fw.Direction != "" && fw.Direction != "INGRESS" && fw.Direction != "EGRESS" {
		return nil, fmt.Errorf("fenceeval: firewall %s: direction %q", fw.Name, fw.Direction)
	}
	if r.priority == 0 && !forced(fw.ForceSendFields, "Priority") {
		r.priority = 1000
	}
	if r.priority < 0 || r.priority > 65535 {
		return nil, fmt.Errorf("fenceeval: firewall %s: priority %d is out of range", fw.Name, r.priority)
	}
	if len(fw.SourceServiceAccounts)+len(fw.TargetServiceAccounts) > 0 {
		return nil, fmt.Errorf("fenceeval: firewall %s: service accounts are not modelled", fw.Name)
	}
	if (len(fw.Allowed) == 0) == (len(fw.Denied) == 0) {
		return nil, fmt.Errorf("fenceeval: firewall %s: exactly one of allowed and denied must be given", fw.Name)
	}
	if !r.ingress && len(fw.SourceTags) > 0 {
		return nil, fmt.Errorf("fenceeval: firewall %s: an EGRESS rule can't have source tags", fw.Name)
	}
	r.deny = len(fw.Denied) > 0
	r.srcTags, r.targetTags = fw.SourceTags, fw.TargetTags
	var err error
	if r.src, err = parseRanges(fw.SourceRanges); err != nil {
		return nil, fmt.Errorf("fenceeval: firewall %s: %w", fw.Name, err)
	}
	if r.dst, err = parseRanges(fw.DestinationRanges); err != nil {
		return nil, fmt.Errorf("fenceeval: firewall %s: %w", fw.Name, err)
	}
	everywhere := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	if r.ingress && r.src == nil && len(r.srcTags) == 0 {
		r.src = everywhere
	}
	if !r.ingress && r.dst == nil {
		r.dst = everywhere
	}
	for _, a := range fw.Allowed {
		x, err := parseL4(a.IPProtocol, a.Ports)
		if err != nil {
			return nil, fmt.Errorf("fenceeval: firewall %s: %w", fw.Name, err)
		}
		r.l4 = append(r.l4, x)
	}
	for _, d := range fw.Denied {
		x, err := parseL4(d.IPProtocol, d.Ports)
		if err != nil {
			return nil, fmt.Errorf("fenceeval: firewall %s: %w", fw.Name, err)
		}
		r.l4 = append(r.l4, x)
	}
	if fw.Disabled {
		return nil, nil
	}
	return r, nil
}

func forced(fields []string, name string) bool {
	for _, f := range fields {
		if f == name {
			return true
		}
	}
	return false
}

// packet is a Packet with its protocol parsed.
type packet struct {
	src, dst    Endpoint
	proto, port int
}

func parsePacket(p Packet) (*packet, error) {
	proto, err := parseProto(p.Protocol, false)
	if err != nil {
		return nil, fmt.Errorf("fenceeval: packet %s: %w", p, err)
	}
	if !p.Src.Addr.IsValid() || !p.Dst.Addr.IsValid() {
		return nil, fmt.Errorf("fenceeval: packet %s: missing an address", p)
	}
	if p.Src.Addr.Is4() != p.Dst.Addr.Is4() {
		return nil, fmt.Errorf("fenceeval: packet %s: mixes IPv4 and IPv6", p)
	}
	if hasPorts(proto) && (p.Port < 0 || p.Port > 65535) {
		return nil, fmt.Errorf("fenceeval: packet %s: port out of range", p)
	}
	return &packet{src: p.Src, dst: p.Dst, proto: proto, port: p.Port}, nil
}

// Evaluate decides p: the sender's egress, unless the sender is
// External, and the receiver's ingress, unless the receiver is. It
// errors only for a malformed packet.
func (e *Evaluator) Evaluate(p Packet) (Verdict, error) {
	pk, err := parsePacket(p)
	if err != nil {
		return Verdict{}, err
	}
	v := Verdict{Allowed: true}
	if !p.Src.External {
		v.Egress = e.decide(false, pk)
		v.Allowed = v.Allowed && v.Egress.Allowed
	}
	if !p.Dst.External {
		v.Ingress = e.decide(true, pk)
		v.Allowed = v.Allowed && v.Ingress.Allowed
	}
	return v, nil
}

// Allowed is Evaluate's answer alone. It panics on a malformed packet,
// for tests that build their packets by hand.
func (e *Evaluator) Allowed(p Packet) bool {
	v, err := e.Evaluate(p)
	if err != nil {
		panic(err)
	}
	return v.Allowed
}

// decide runs one side's firewall: the policy and VPC rules in the
// network's order, then the implied rule.
func (e *Evaluator) decide(ingress bool, p *packet) Decision {
	var handoff string
	policy := func() (Decision, bool) {
		r := firstPolicyMatch(e.policy, ingress, p.src.Addr, p.dst.Addr, p.proto, p.port)
		if r == nil {
			return Decision{}, false
		}
		if r.action == GotoNext {
			handoff = r.name
			return Decision{}, false
		}
		return Decision{Allowed: r.action == Allow, Stage: StagePolicy, Rule: r.name, Priority: r.priority}, true
	}
	vpc := func() (Decision, bool) {
		for _, r := range e.vpc {
			if r.matches(ingress, p) {
				return Decision{Allowed: !r.deny, Stage: StageVPC, Rule: r.name, Priority: r.priority, Handoff: handoff}, true
			}
		}
		return Decision{}, false
	}
	stages := []func() (Decision, bool){policy, vpc}
	if e.order == AfterClassic {
		stages = []func() (Decision, bool){vpc, policy}
	}
	for _, s := range stages {
		if d, ok := s(); ok {
			return d
		}
	}
	if ingress {
		return Decision{Stage: StageImplied, Rule: ImpliedDenyIngress, Priority: 65535, Handoff: handoff}
	}
	return Decision{Allowed: true, Stage: StageImplied, Rule: ImpliedAllowEgress, Priority: 65535, Handoff: handoff}
}

func firstPolicyMatch(rules []*policyRule, ingress bool, src, dst netip.Addr, proto, port int) *policyRule {
	for _, r := range rules {
		if r.matches(ingress, src, dst, proto, port) {
			return r
		}
	}
	return nil
}
