package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

// SmallestSubnetBits is GCE's smallest IPv4 subnet range, /29.
const SmallestSubnetBits = 29

func (f *Fake) network(p *state, name string) (*compute.Network, error) {
	n := p.networks.get("global", name)
	if n == nil {
		return nil, notFound(p.link("global", "networks", name))
	}
	return n, nil
}

// networkView is n with its subnetworks and firewall policy filled in.
func (f *Fake) networkView(p *state, n *compute.Network) *compute.Network {
	out := clone(n)
	out.Subnetworks = nil
	for _, s := range p.subnets.all() {
		if s.Network == n.SelfLink {
			out.Subnetworks = append(out.Subnetworks, s.SelfLink)
		}
	}
	for _, pol := range p.policies.all() {
		for _, a := range pol.Associations {
			if a.AttachmentTarget == n.SelfLink {
				out.FirewallPolicy = pol.SelfLink
			}
		}
	}
	return out
}

// InsertNetwork implements gce.Client. Only custom-mode networks are
// modelled: autoCreateSubnetworks is refused.
func (f *Fake) InsertNetwork(ctx context.Context, project string, body *compute.Network) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	n := clone(body)
	if err := checkName("resource.name", n.Name); err != nil {
		return nil, err
	}
	link := p.link("global", "networks", n.Name)
	if p.networks.get("global", n.Name) != nil {
		return nil, alreadyExists(link)
	}
	if n.AutoCreateSubnetworks {
		return nil, invalid("resource.autoCreateSubnetworks", "the fake models custom-mode networks only")
	}
	if n.RoutingConfig == nil {
		n.RoutingConfig = &compute.NetworkRoutingConfig{}
	}
	if n.RoutingConfig.RoutingMode == "" {
		n.RoutingConfig.RoutingMode = "REGIONAL"
	}
	switch n.NetworkFirewallPolicyEnforcementOrder {
	case "":
		n.NetworkFirewallPolicyEnforcementOrder = "AFTER_CLASSIC_FIREWALL"
	case "AFTER_CLASSIC_FIREWALL", "BEFORE_CLASSIC_FIREWALL":
	default:
		return nil, invalid("resource.networkFirewallPolicyEnforcementOrder", "%s", n.NetworkFirewallPolicyEnforcementOrder)
	}
	if n.Mtu == 0 {
		n.Mtu = 1460
	}
	n.Kind = "compute#network"
	n.Id = f.nextID()
	n.SelfLink = link
	n.CreationTimestamp = f.timestamp()
	n.ForceSendFields = append(n.ForceSendFields, "AutoCreateSubnetworks")
	p.networks.put("global", n.Name, n)
	return f.insertPending(p, "networks.insert", link, n.Id, func() {},
		func() {
			if p.networks.get("global", n.Name) == n {
				p.networks.del("global", n.Name)
			}
		}), nil
}

// GetNetwork implements gce.Client.
func (f *Fake) GetNetwork(ctx context.Context, project, name string) (*compute.Network, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	n, err := f.network(p, name)
	if err != nil {
		return nil, err
	}
	return f.networkView(p, n), nil
}

// ListNetworks implements gce.Client. Networks have no labels.
func (f *Fake) ListNetworks(ctx context.Context, project string, lf gce.ListFilter) ([]*compute.Network, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	var out []*compute.Network
	for _, n := range p.networks.list("global") {
		if lf.Match(n.Name, nil) {
			out = append(out, f.networkView(p, n))
		}
	}
	return out, nil
}

// DeleteNetwork implements gce.Client. A network with subnetworks,
// firewall rules or a firewall policy association can't be deleted.
func (f *Fake) DeleteNetwork(ctx context.Context, project, name string) (*compute.Operation, error) {
	return f.globalOp(ctx, project, "networks.delete",
		func(p *state) (string, uint64, error) {
			n, err := f.network(p, name)
			if err != nil {
				return "", 0, err
			}
			v := f.networkView(p, n)
			if len(v.Subnetworks) > 0 || v.FirewallPolicy != "" {
				return "", 0, inUse("The network resource '%s' is already being used", trimBase(n.SelfLink))
			}
			for _, fw := range p.firewalls.all() {
				if fw.Network == n.SelfLink {
					return "", 0, inUse("The network resource '%s' is already being used by '%s'", trimBase(n.SelfLink), trimBase(fw.SelfLink))
				}
			}
			return n.SelfLink, n.Id, nil
		},
		func(p *state) {
			if n := p.networks.get("global", name); n != nil {
				delete(p.pending, n.SelfLink)
				p.networks.del("global", name)
			}
		})
}

// subnetRanges are every range of every subnetwork of network, but
// skip, with the subnetwork that holds it.
func subnetRanges(p *state, network string, skip *compute.Subnetwork) map[netip.Prefix]string {
	out := map[netip.Prefix]string{}
	for _, s := range p.subnets.all() {
		if s.Network != network || s == skip {
			continue
		}
		out[netip.MustParsePrefix(s.IpCidrRange)] = s.Name
		for _, r := range s.SecondaryIpRanges {
			out[netip.MustParsePrefix(r.IpCidrRange)] = s.Name
		}
	}
	return out
}

func parseRange(field, cidr string) (netip.Prefix, error) {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil || !pfx.Addr().Is4() {
		return netip.Prefix{}, invalid(field, "'%s' is not a valid IPv4 CIDR range", cidr)
	}
	if pfx.Masked() != pfx {
		return netip.Prefix{}, invalid(field, "'%s' has host bits set; it must be %s", cidr, pfx.Masked())
	}
	if pfx.Bits() > SmallestSubnetBits {
		return netip.Prefix{}, invalid(field, "'%s' is smaller than /%d, the smallest range allowed", cidr, SmallestSubnetBits)
	}
	return pfx, nil
}

// checkRanges checks s's ranges against each other and against the
// other subnetworks of its network: no overlap anywhere in a network.
func checkRanges(p *state, s *compute.Subnetwork, self *compute.Subnetwork) error {
	mine := map[netip.Prefix]string{}
	primary, err := parseRange("resource.ipCidrRange", s.IpCidrRange)
	if err != nil {
		return err
	}
	mine[primary] = "resource.ipCidrRange"
	names := map[string]bool{}
	for i, r := range s.SecondaryIpRanges {
		field := fmt.Sprintf("resource.secondaryIpRanges[%d]", i)
		if err := checkName(field+".rangeName", r.RangeName); err != nil {
			return err
		}
		if names[r.RangeName] {
			return invalid(field+".rangeName", "Range name %s is used twice", r.RangeName)
		}
		names[r.RangeName] = true
		pfx, err := parseRange(field+".ipCidrRange", r.IpCidrRange)
		if err != nil {
			return err
		}
		for q, qf := range mine {
			if q.Overlaps(pfx) {
				return invalid(field+".ipCidrRange", "%s overlaps %s (%s)", pfx, q, qf)
			}
		}
		mine[pfx] = field + ".ipCidrRange"
	}
	others := subnetRanges(p, s.Network, self)
	for pfx, field := range mine {
		for q, owner := range others {
			if q.Overlaps(pfx) {
				return invalid(field, "Invalid IPCidrRange: %s conflicts with existing subnetwork '%s' in region (range %s)", pfx, owner, q)
			}
		}
	}
	return nil
}

func (f *Fake) subnet(p *state, region, name string) (*compute.Subnetwork, error) {
	if err := f.regionExists(region); err != nil {
		return nil, err
	}
	s := p.subnets.get(regionScope(region), name)
	if s == nil {
		return nil, notFound(p.link(regionScope(region), "subnetworks", name))
	}
	return s, nil
}

// InsertSubnetwork implements gce.Client. Ranges are IPv4, /29 or
// larger, and overlap nothing else in the network.
func (f *Fake) InsertSubnetwork(ctx context.Context, project, region string, body *compute.Subnetwork) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.regionExists(region); err != nil {
		return nil, err
	}
	s := clone(body)
	if err := checkName("resource.name", s.Name); err != nil {
		return nil, err
	}
	scope := regionScope(region)
	link := p.link(scope, "subnetworks", s.Name)
	if p.subnets.get(scope, s.Name) != nil {
		return nil, alreadyExists(link)
	}
	if s.Network == "" {
		return nil, invalid("resource.network", "Required field not specified")
	}
	n := p.networks.get("global", gce.ParseRef(s.Network).Name)
	if n == nil || p.pending[n.SelfLink] {
		return nil, invalid("resource.network", "The resource '%s' was not found", s.Network)
	}
	s.Network = n.SelfLink
	if err := checkRanges(p, s, nil); err != nil {
		return nil, err
	}
	if s.Purpose == "" {
		s.Purpose = "PRIVATE"
	}
	if s.StackType == "" {
		s.StackType = "IPV4_ONLY"
	}
	s.Kind = "compute#subnetwork"
	s.Id = f.nextID()
	s.SelfLink = link
	s.Region = gce.SelfLinkBase + "projects/" + p.name + "/" + scope
	s.GatewayAddress = netip.MustParsePrefix(s.IpCidrRange).Addr().Next().String()
	s.Fingerprint = f.fingerprint()
	s.CreationTimestamp = f.timestamp()
	p.subnets.put(scope, s.Name, s)
	return f.insertPending(p, "subnetworks.insert", link, s.Id, func() {},
		func() {
			if p.subnets.get(scope, s.Name) == s {
				p.subnets.del(scope, s.Name)
			}
		}), nil
}

// GetSubnetwork implements gce.Client.
func (f *Fake) GetSubnetwork(ctx context.Context, project, region, name string) (*compute.Subnetwork, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	s, err := f.subnet(p, region, name)
	if err != nil {
		return nil, err
	}
	return clone(s), nil
}

// ListSubnetworks implements gce.Client. Subnetworks have no labels.
func (f *Fake) ListSubnetworks(ctx context.Context, project, region string, lf gce.ListFilter) ([]*compute.Subnetwork, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.regionExists(region); err != nil {
		return nil, err
	}
	return filtered(p.subnets.list(regionScope(region)), lf, func(s *compute.Subnetwork) (string, map[string]string) { return s.Name, nil }), nil
}

// subnetOp checks the subnetwork, then check, then starts method's
// operation, whose commit runs apply.
func (f *Fake) subnetOp(ctx context.Context, project, region, name, method string, check func(p *state, s *compute.Subnetwork) error, apply func(p *state, s *compute.Subnetwork)) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	s, err := f.subnet(p, region, name)
	if err != nil {
		return nil, err
	}
	if err := check(p, s); err != nil {
		return nil, err
	}
	return f.start(p, method, s.SelfLink, s.Id, func() *opErr {
		if p.subnets.get(regionScope(region), name) == s {
			apply(p, s)
		}
		return nil
	}, nil), nil
}

// DeleteSubnetwork implements gce.Client. A subnetwork an instance uses
// can't be deleted.
func (f *Fake) DeleteSubnetwork(ctx context.Context, project, region, name string) (*compute.Operation, error) {
	return f.subnetOp(ctx, project, region, name, "subnetworks.delete",
		func(p *state, s *compute.Subnetwork) error {
			for _, inst := range p.instances.all() {
				for _, nic := range inst.NetworkInterfaces {
					if nic.Subnetwork == s.SelfLink {
						return inUse("The subnetwork resource '%s' is already being used by '%s'", trimBase(s.SelfLink), trimBase(inst.SelfLink))
					}
				}
			}
			return nil
		},
		func(p *state, s *compute.Subnetwork) {
			delete(p.pending, s.SelfLink)
			p.subnets.del(regionScope(region), name)
		})
}

// PatchSubnetwork implements gce.Client: description, secondaryIpRanges
// (replaced when given, even empty) and logConfig, with the current
// fingerprint.
func (f *Fake) PatchSubnetwork(ctx context.Context, project, region, name string, body *compute.Subnetwork) (*compute.Operation, error) {
	var next *compute.Subnetwork
	return f.subnetOp(ctx, project, region, name, "subnetworks.patch",
		func(p *state, s *compute.Subnetwork) error {
			if body.Fingerprint == "" {
				return gce.NewError(http.StatusBadRequest, "required", "Required field 'resource.fingerprint' not specified")
			}
			if body.Fingerprint != s.Fingerprint {
				return conditionNotMet("Supplied fingerprint does not match current subnetwork fingerprint")
			}
			if body.IpCidrRange != "" && body.IpCidrRange != s.IpCidrRange {
				return invalid("resource.ipCidrRange", "ipCidrRange can only be changed with expandIpCidrRange")
			}
			if body.Network != "" && gce.ParseRef(body.Network).Name != gce.LastSegment(s.Network) {
				return invalid("resource.network", "network can't be changed")
			}
			next = clone(s)
			if body.Description != "" {
				next.Description = body.Description
			}
			if body.LogConfig != nil {
				next.LogConfig = body.LogConfig
			}
			if body.SecondaryIpRanges != nil || forced(body.ForceSendFields, "SecondaryIpRanges") {
				next.SecondaryIpRanges = body.SecondaryIpRanges
			}
			return checkRanges(p, next, s)
		},
		func(_ *state, s *compute.Subnetwork) {
			s.Description, s.LogConfig, s.SecondaryIpRanges = next.Description, next.LogConfig, next.SecondaryIpRanges
			s.Fingerprint = f.fingerprint()
		})
}

// SetPrivateIpGoogleAccess is subnetworks.setPrivateIpGoogleAccess.
// It isn't in gce.Client, since silod never calls it: silo install
// does, through its own fake (install/fakecloud).
func (f *Fake) SetPrivateIpGoogleAccess(ctx context.Context, project, region, name string, on bool) (*compute.Operation, error) {
	return f.subnetOp(ctx, project, region, name, "subnetworks.setPrivateIpGoogleAccess",
		func(*state, *compute.Subnetwork) error { return nil },
		func(_ *state, s *compute.Subnetwork) {
			s.PrivateIpGoogleAccess = on
			s.Fingerprint = f.fingerprint()
		})
}

func forced(fields []string, name string) bool {
	for _, f := range fields {
		if f == name {
			return true
		}
	}
	return false
}

// ExpandIpCidrRange implements gce.Client: the new range must contain
// the old one and overlap nothing else.
func (f *Fake) ExpandIpCidrRange(ctx context.Context, project, region, name, cidr string) (*compute.Operation, error) {
	return f.subnetOp(ctx, project, region, name, "subnetworks.expandIpCidrRange",
		func(p *state, s *compute.Subnetwork) error {
			pfx, err := parseRange("ipCidrRange", cidr)
			if err != nil {
				return err
			}
			old := netip.MustParsePrefix(s.IpCidrRange)
			if pfx.Bits() >= old.Bits() || !contains(pfx, old) {
				return invalid("ipCidrRange", "%s must be larger than and contain the current range %s", cidr, old)
			}
			next := clone(s)
			next.IpCidrRange = cidr
			return checkRanges(p, next, s)
		},
		func(_ *state, s *compute.Subnetwork) {
			s.IpCidrRange = cidr
			s.Fingerprint = f.fingerprint()
		})
}

// checkFirewall checks and fills in a VPC firewall rule as GCE does.
func checkFirewall(p *state, fw *compute.Firewall) error {
	if err := checkName("resource.name", fw.Name); err != nil {
		return err
	}
	netName := "default"
	if fw.Network != "" {
		netName = gce.ParseRef(fw.Network).Name
	}
	n := p.networks.get("global", netName)
	if n == nil || p.pending[n.SelfLink] {
		return invalid("resource.network", "The resource 'projects/%s/global/networks/%s' was not found", p.name, netName)
	}
	fw.Network = n.SelfLink
	if fw.Direction == "" {
		fw.Direction = "INGRESS"
	}
	if fw.Direction != "INGRESS" && fw.Direction != "EGRESS" {
		return invalid("resource.direction", "%s", fw.Direction)
	}
	if fw.Priority == 0 && !forced(fw.ForceSendFields, "Priority") {
		fw.Priority = 1000
	}
	if fw.Priority < 0 || fw.Priority > 65535 {
		return invalid("resource.priority", "%d is out of range [0, 65535]", fw.Priority)
	}
	if (len(fw.Allowed) == 0) == (len(fw.Denied) == 0) {
		return invalid("resource", "Exactly one of allowed and denied must be given")
	}
	if len(fw.TargetTags) > 0 && len(fw.TargetServiceAccounts) > 0 {
		return invalid("resource.targetTags", "targetTags and targetServiceAccounts can't both be given")
	}
	if fw.Direction == "INGRESS" {
		if len(fw.DestinationRanges) > 0 && len(fw.SourceRanges)+len(fw.SourceTags)+len(fw.SourceServiceAccounts) == 0 {
			fw.SourceRanges = []string{"0.0.0.0/0"}
		}
		if len(fw.SourceRanges)+len(fw.SourceTags)+len(fw.SourceServiceAccounts) == 0 {
			fw.SourceRanges = []string{"0.0.0.0/0"}
		}
	} else {
		if len(fw.SourceTags) > 0 || len(fw.SourceServiceAccounts) > 0 {
			return invalid("resource.sourceTags", "An EGRESS rule can't have source tags or service accounts")
		}
		if len(fw.DestinationRanges) == 0 {
			fw.DestinationRanges = []string{"0.0.0.0/0"}
		}
	}
	for i, r := range append(append([]string{}, fw.SourceRanges...), fw.DestinationRanges...) {
		if _, err := netip.ParsePrefix(r); err != nil {
			if _, err := netip.ParseAddr(r); err != nil {
				return invalid(fmt.Sprintf("resource.ranges[%d]", i), "'%s' is not a valid range", r)
			}
		}
	}
	fw.Kind = "compute#firewall"
	fw.SelfLink = p.link("global", "firewalls", fw.Name)
	return nil
}

func (f *Fake) firewall(p *state, name string) (*compute.Firewall, error) {
	fw := p.firewalls.get("global", name)
	if fw == nil {
		return nil, notFound(p.link("global", "firewalls", name))
	}
	return fw, nil
}

// InsertFirewall implements gce.Client.
func (f *Fake) InsertFirewall(ctx context.Context, project string, body *compute.Firewall) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	fw := clone(body)
	fw.ForceSendFields = body.ForceSendFields
	if err := checkFirewall(p, fw); err != nil {
		return nil, err
	}
	if p.firewalls.get("global", fw.Name) != nil {
		return nil, alreadyExists(fw.SelfLink)
	}
	fw.Id = f.nextID()
	fw.CreationTimestamp = f.timestamp()
	p.firewalls.put("global", fw.Name, fw)
	return f.insertPending(p, "firewalls.insert", fw.SelfLink, fw.Id, func() {},
		func() {
			if p.firewalls.get("global", fw.Name) == fw {
				p.firewalls.del("global", fw.Name)
			}
		}), nil
}

// GetFirewall implements gce.Client.
func (f *Fake) GetFirewall(ctx context.Context, project, name string) (*compute.Firewall, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	fw, err := f.firewall(p, name)
	if err != nil {
		return nil, err
	}
	return clone(fw), nil
}

// ListFirewalls implements gce.Client. Firewall rules have no labels.
func (f *Fake) ListFirewalls(ctx context.Context, project string, lf gce.ListFilter) ([]*compute.Firewall, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	return filtered(p.firewalls.list("global"), lf, func(fw *compute.Firewall) (string, map[string]string) { return fw.Name, nil }), nil
}

// DeleteFirewall implements gce.Client.
func (f *Fake) DeleteFirewall(ctx context.Context, project, name string) (*compute.Operation, error) {
	return f.globalOp(ctx, project, "firewalls.delete",
		func(p *state) (string, uint64, error) {
			fw, err := f.firewall(p, name)
			if err != nil {
				return "", 0, err
			}
			return fw.SelfLink, fw.Id, nil
		},
		func(p *state) {
			if fw := p.firewalls.get("global", name); fw != nil {
				delete(p.pending, fw.SelfLink)
				p.firewalls.del("global", name)
			}
		})
}

// PatchFirewall implements gce.Client: the fields body sets replace the
// rule's.
func (f *Fake) PatchFirewall(ctx context.Context, project, name string, body *compute.Firewall) (*compute.Operation, error) {
	return f.replaceFirewall(ctx, project, name, body, "firewalls.patch", true)
}

// UpdateFirewall implements gce.Client: body replaces the rule.
func (f *Fake) UpdateFirewall(ctx context.Context, project, name string, body *compute.Firewall) (*compute.Operation, error) {
	return f.replaceFirewall(ctx, project, name, body, "firewalls.update", false)
}

func (f *Fake) replaceFirewall(ctx context.Context, project, name string, body *compute.Firewall, method string, patch bool) (*compute.Operation, error) {
	var next *compute.Firewall
	return f.globalOp(ctx, project, method,
		func(p *state) (string, uint64, error) {
			fw, err := f.firewall(p, name)
			if err != nil {
				return "", 0, err
			}
			if body.Name != "" && body.Name != name {
				return "", 0, invalid("resource.name", "name can't be changed")
			}
			next = clone(body)
			if patch {
				base, _ := toMap(fw)
				over, _ := toMap(body)
				b, _ := json.Marshal(mergeMaps(base, over))
				next = &compute.Firewall{}
				_ = json.Unmarshal(b, next)
			}
			next.Name = name
			if err := checkFirewall(p, next); err != nil {
				return "", 0, err
			}
			next.Id, next.CreationTimestamp = fw.Id, fw.CreationTimestamp
			return fw.SelfLink, fw.Id, nil
		},
		func(p *state) {
			if p.firewalls.get("global", name) != nil {
				p.firewalls.put("global", name, next)
			}
		})
}

// Default rules GCE puts in every network firewall policy, at the
// lowest priorities, all goto_next.
func defaultPolicyRules() []*compute.FirewallPolicyRule {
	var out []*compute.FirewallPolicyRule
	for i, r := range []struct{ dir, cidr string }{
		{"EGRESS", "::/0"}, {"INGRESS", "::/0"}, {"EGRESS", "0.0.0.0/0"}, {"INGRESS", "0.0.0.0/0"},
	} {
		m := &compute.FirewallPolicyRuleMatcher{Layer4Configs: []*compute.FirewallPolicyRuleMatcherLayer4Config{{IpProtocol: "all"}}}
		if r.dir == "INGRESS" {
			m.SrcIpRanges = []string{r.cidr}
		} else {
			m.DestIpRanges = []string{r.cidr}
		}
		out = append(out, &compute.FirewallPolicyRule{
			Kind: "compute#firewallPolicyRule", Priority: 2147483644 + int64(i), Action: "goto_next",
			Direction: r.dir, Match: m, Description: "default " + r.dir + " rule",
		})
	}
	return out
}

func isDefaultRule(priority int64) bool { return priority >= 2147483644 }

func (f *Fake) policy(p *state, name string) (*compute.FirewallPolicy, error) {
	pol := p.policies.get("global", name)
	if pol == nil {
		return nil, notFound(p.link("global", "firewallPolicies", name))
	}
	return pol, nil
}

func sortRules(rs []*compute.FirewallPolicyRule) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Priority < rs[j-1].Priority; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// InsertFirewallPolicy implements gce.Client: a global network firewall
// policy, with GCE's default rules.
func (f *Fake) InsertFirewallPolicy(ctx context.Context, project string, body *compute.FirewallPolicy) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	pol := clone(body)
	if err := checkName("resource.name", pol.Name); err != nil {
		return nil, err
	}
	link := p.link("global", "firewallPolicies", pol.Name)
	if p.policies.get("global", pol.Name) != nil {
		return nil, alreadyExists(link)
	}
	if len(pol.Rules) > 0 || len(pol.Associations) > 0 {
		return nil, invalid("resource.rules", "Rules and associations are added with addRule and addAssociation")
	}
	pol.Kind = "compute#firewallPolicy"
	pol.Id = f.nextID()
	pol.SelfLink = link
	pol.SelfLinkWithId = p.link("global", "firewallPolicies", fmt.Sprint(pol.Id))
	pol.CreationTimestamp = f.timestamp()
	pol.Fingerprint = f.fingerprint()
	pol.Rules = defaultPolicyRules()
	p.policies.put("global", pol.Name, pol)
	return f.insertPending(p, "networkFirewallPolicies.insert", link, pol.Id, func() {},
		func() {
			if p.policies.get("global", pol.Name) == pol {
				p.policies.del("global", pol.Name)
			}
		}), nil
}

// GetFirewallPolicy implements gce.Client. Rules are in priority order.
func (f *Fake) GetFirewallPolicy(ctx context.Context, project, name string) (*compute.FirewallPolicy, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	pol, err := f.policy(p, name)
	if err != nil {
		return nil, err
	}
	return clone(pol), nil
}

// ListFirewallPolicies implements gce.Client.
func (f *Fake) ListFirewallPolicies(ctx context.Context, project string) ([]*compute.FirewallPolicy, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	return cloneAll(p.policies.list("global")), nil
}

// policyOp checks the policy, then check, then starts method's
// operation. apply runs when it finishes, and may still fail it (a
// priority taken meanwhile); each change bumps the fingerprint.
func (f *Fake) policyOp(ctx context.Context, project, name, method string, check func(p *state, pol *compute.FirewallPolicy) error, apply func(p *state, pol *compute.FirewallPolicy) error) (*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	pol, err := f.policy(p, name)
	if err != nil {
		return nil, err
	}
	if err := check(p, pol); err != nil {
		return nil, err
	}
	return f.start(p, method, pol.SelfLink, pol.Id, func() *opErr {
		if p.policies.get("global", name) != pol {
			return &opErr{"RESOURCE_NOT_FOUND", http.StatusNotFound, "The resource '" + trimBase(pol.SelfLink) + "' was not found"}
		}
		if err := apply(p, pol); err != nil {
			return &opErr{"INVALID_USAGE", http.StatusBadRequest, err.Error()}
		}
		sortRules(pol.Rules)
		pol.Fingerprint = f.fingerprint()
		return nil
	}, nil), nil
}

// DeleteFirewallPolicy implements gce.Client. An associated policy
// can't be deleted.
func (f *Fake) DeleteFirewallPolicy(ctx context.Context, project, name string) (*compute.Operation, error) {
	return f.policyOp(ctx, project, name, "networkFirewallPolicies.delete",
		func(_ *state, pol *compute.FirewallPolicy) error {
			if len(pol.Associations) > 0 {
				return inUse("The firewall policy '%s' is associated with %s", name, trimBase(pol.Associations[0].AttachmentTarget))
			}
			return nil
		},
		func(p *state, pol *compute.FirewallPolicy) error {
			delete(p.pending, pol.SelfLink)
			p.policies.del("global", name)
			return nil
		})
}

// PatchFirewallPolicy implements gce.Client: the description, with the
// current fingerprint, or 412.
func (f *Fake) PatchFirewallPolicy(ctx context.Context, project, name string, body *compute.FirewallPolicy) (*compute.Operation, error) {
	desc := body.Description
	return f.policyOp(ctx, project, name, "networkFirewallPolicies.patch",
		func(_ *state, pol *compute.FirewallPolicy) error {
			if body.Fingerprint != pol.Fingerprint {
				return conditionNotMet("Supplied fingerprint does not match current firewall policy fingerprint")
			}
			if len(body.Rules) > 0 {
				return invalid("resource.rules", "Rules are changed with addRule, patchRule and removeRule")
			}
			return nil
		},
		func(_ *state, pol *compute.FirewallPolicy) error {
			pol.Description = desc
			return nil
		})
}

func ruleAt(pol *compute.FirewallPolicy, priority int64) *compute.FirewallPolicyRule {
	for _, r := range pol.Rules {
		if r.Priority == priority {
			return r
		}
	}
	return nil
}

func checkRule(r *compute.FirewallPolicyRule) error {
	switch r.Action {
	case "allow", "deny", "goto_next":
	default:
		return invalid("resource.action", "'%s' is not one of allow, deny, goto_next", r.Action)
	}
	if r.Direction != "INGRESS" && r.Direction != "EGRESS" {
		return invalid("resource.direction", "'%s'", r.Direction)
	}
	if r.Priority < 0 || r.Priority >= 2147483644 {
		return invalid("resource.priority", "%d is out of range", r.Priority)
	}
	if r.Match == nil || len(r.Match.Layer4Configs) == 0 {
		return invalid("resource.match.layer4Configs", "Required field not specified")
	}
	for _, cidr := range append(append([]string{}, r.Match.SrcIpRanges...), r.Match.DestIpRanges...) {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return invalid("resource.match", "'%s' is not a valid range", cidr)
		}
	}
	return nil
}

// AddFirewallPolicyRule implements gce.Client. Priorities are unique.
func (f *Fake) AddFirewallPolicyRule(ctx context.Context, project, name string, body *compute.FirewallPolicyRule) (*compute.Operation, error) {
	r := clone(body)
	r.Kind = "compute#firewallPolicyRule"
	return f.policyOp(ctx, project, name, "networkFirewallPolicies.addRule",
		func(_ *state, pol *compute.FirewallPolicy) error {
			if err := checkRule(r); err != nil {
				return err
			}
			if ruleAt(pol, r.Priority) != nil {
				return invalid("resource.priority", "A rule with priority %d already exists in the policy", r.Priority)
			}
			return nil
		},
		func(_ *state, pol *compute.FirewallPolicy) error {
			if ruleAt(pol, r.Priority) != nil {
				return fmt.Errorf("a rule with priority %d already exists in the policy", r.Priority)
			}
			pol.Rules = append(pol.Rules, r)
			return nil
		})
}

// GetFirewallPolicyRule implements gce.Client.
func (f *Fake) GetFirewallPolicyRule(ctx context.Context, project, name string, priority int64) (*compute.FirewallPolicyRule, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	pol, err := f.policy(p, name)
	if err != nil {
		return nil, err
	}
	r := ruleAt(pol, priority)
	if r == nil {
		return nil, invalid("priority", "No rule with priority %d", priority)
	}
	return clone(r), nil
}

// PatchFirewallPolicyRule implements gce.Client: the fields body sets
// replace the rule's, which may move it to body.Priority.
func (f *Fake) PatchFirewallPolicyRule(ctx context.Context, project, name string, priority int64, body *compute.FirewallPolicyRule) (*compute.Operation, error) {
	var next *compute.FirewallPolicyRule
	return f.policyOp(ctx, project, name, "networkFirewallPolicies.patchRule",
		func(_ *state, pol *compute.FirewallPolicy) error {
			cur := ruleAt(pol, priority)
			if cur == nil {
				return invalid("priority", "No rule with priority %d", priority)
			}
			if isDefaultRule(priority) {
				return invalid("priority", "The default rules can't be changed")
			}
			base, _ := toMap(cur)
			over, _ := toMap(body)
			b, _ := json.Marshal(mergeMaps(base, over))
			next = &compute.FirewallPolicyRule{}
			_ = json.Unmarshal(b, next)
			if next.Priority != priority && ruleAt(pol, next.Priority) != nil {
				return invalid("resource.priority", "A rule with priority %d already exists in the policy", next.Priority)
			}
			return checkRule(next)
		},
		func(_ *state, pol *compute.FirewallPolicy) error {
			for i, r := range pol.Rules {
				if r.Priority == priority {
					pol.Rules[i] = next
					return nil
				}
			}
			return fmt.Errorf("no rule with priority %d", priority)
		})
}

// RemoveFirewallPolicyRule implements gce.Client. The default rules
// can't be removed.
func (f *Fake) RemoveFirewallPolicyRule(ctx context.Context, project, name string, priority int64) (*compute.Operation, error) {
	return f.policyOp(ctx, project, name, "networkFirewallPolicies.removeRule",
		func(_ *state, pol *compute.FirewallPolicy) error {
			if ruleAt(pol, priority) == nil {
				return invalid("priority", "No rule with priority %d", priority)
			}
			if isDefaultRule(priority) {
				return invalid("priority", "The default rules can't be removed")
			}
			return nil
		},
		func(_ *state, pol *compute.FirewallPolicy) error {
			for i, r := range pol.Rules {
				if r.Priority == priority {
					pol.Rules = append(pol.Rules[:i:i], pol.Rules[i+1:]...)
					return nil
				}
			}
			return nil
		})
}

// AddFirewallPolicyAssociation implements gce.Client. A network has at
// most one network firewall policy; replaceExisting moves it.
func (f *Fake) AddFirewallPolicyAssociation(ctx context.Context, project, name string, body *compute.FirewallPolicyAssociation, replaceExisting bool) (*compute.Operation, error) {
	a := clone(body)
	var other *compute.FirewallPolicy
	return f.policyOp(ctx, project, name, "networkFirewallPolicies.addAssociation",
		func(p *state, pol *compute.FirewallPolicy) error {
			n := p.networks.get("global", gce.ParseRef(a.AttachmentTarget).Name)
			if n == nil || p.pending[n.SelfLink] {
				return invalid("resource.attachmentTarget", "The resource '%s' was not found", a.AttachmentTarget)
			}
			a.AttachmentTarget = n.SelfLink
			if a.Name == "" {
				a.Name = "network-" + n.Name
			}
			for _, x := range pol.Associations {
				if x.Name == a.Name || x.AttachmentTarget == a.AttachmentTarget {
					return alreadyExists(pol.SelfLink + "/associations/" + x.Name)
				}
			}
			for _, q := range p.policies.all() {
				for _, x := range q.Associations {
					if q != pol && x.AttachmentTarget == n.SelfLink {
						if !replaceExisting {
							return invalid("resource.attachmentTarget", "Network %s already has firewall policy %s", n.Name, q.Name)
						}
						other = q
					}
				}
			}
			a.FirewallPolicyId = fmt.Sprint(pol.Id)
			a.ShortName = pol.Name
			return nil
		},
		func(_ *state, pol *compute.FirewallPolicy) error {
			if other != nil {
				other.Associations = removeAssociation(other.Associations, func(x *compute.FirewallPolicyAssociation) bool { return x.AttachmentTarget == a.AttachmentTarget })
				other.Fingerprint = f.fingerprint()
			}
			pol.Associations = append(pol.Associations, a)
			return nil
		})
}

func removeAssociation(as []*compute.FirewallPolicyAssociation, drop func(*compute.FirewallPolicyAssociation) bool) []*compute.FirewallPolicyAssociation {
	var out []*compute.FirewallPolicyAssociation
	for _, a := range as {
		if !drop(a) {
			out = append(out, a)
		}
	}
	return out
}

// RemoveFirewallPolicyAssociation implements gce.Client.
func (f *Fake) RemoveFirewallPolicyAssociation(ctx context.Context, project, name, association string) (*compute.Operation, error) {
	return f.policyOp(ctx, project, name, "networkFirewallPolicies.removeAssociation",
		func(_ *state, pol *compute.FirewallPolicy) error {
			for _, a := range pol.Associations {
				if a.Name == association {
					return nil
				}
			}
			return invalid("name", "No association named %s", association)
		},
		func(_ *state, pol *compute.FirewallPolicy) error {
			pol.Associations = removeAssociation(pol.Associations, func(x *compute.FirewallPolicyAssociation) bool { return x.Name == association })
			return nil
		})
}
