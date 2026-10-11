package install

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/gwtls"
	compute "google.golang.org/api/compute/v1"
)

// The shared VPC's fixed objects, in the home region.
const (
	GatewaySubnetName = gwtls.GatewaySubnet
	ControlSubnetName = "silo-ctl"
	RouterName        = "silo-router"
	NATName           = "silo-gateway-nat"
	GatewayAddress    = "silo-gateway-ip"
)

func networkPath(project string) string {
	return "projects/" + project + "/global/networks/" + NetworkName
}

func subnetPath(project, region, name string) string {
	return "projects/" + project + "/regions/" + region + "/subnetworks/" + name
}

// computeOn reports whether compute is enabled: with it off, the
// project has none of compute's objects, and a step observes nothing.
func computeOn(ctx context.Context, env *Env) (bool, error) {
	return env.Enabled(ctx, "compute.googleapis.com")
}

// Subnet is steps 6 and 6a, gateway-subnet and control-subnet: one of
// the shared VPC's two system subnets in the home region. The gateway's
// has Private Google Access, the control subnet's (which holds only the
// database's PSC endpoint) doesn't.
type Subnet struct {
	Env           *Env
	step, subnet  string
	rangeOf       func(*Config) netip.Prefix
	privateAccess bool
}

// GatewaySubnet is step 6, gateway-subnet: silo-gateway, at
// --gateway-range, with Private Google Access on.
func GatewaySubnet(env *Env) *Subnet {
	return &Subnet{Env: env, step: "gateway-subnet", subnet: GatewaySubnetName,
		rangeOf: func(c *Config) netip.Prefix { return c.GatewayRange }, privateAccess: true}
}

// ControlSubnet is step 6a, control-subnet: silo-ctl, at
// --control-range, with Private Google Access off.
func ControlSubnet(env *Env) *Subnet {
	return &Subnet{Env: env, step: "control-subnet", subnet: ControlSubnetName,
		rangeOf: func(c *Config) netip.Prefix { return c.ControlRange }}
}

// SystemSubnetObserved is the subnet as GCE has it, or Exists false.
type SystemSubnetObserved struct {
	ComputeEnabled      bool   `json:"computeEnabled"`
	Exists              bool   `json:"exists"`
	Description         string `json:"description,omitempty"`
	Network             string `json:"network,omitempty"`
	Range               string `json:"range,omitempty"`
	PrivateGoogleAccess bool   `json:"privateGoogleAccess,omitempty"`
}

func (s *Subnet) Name() string          { return s.step }
func (s *Subnet) homeRegion() string    { return s.Env.Config.HomeRegion() }
func (s *Subnet) NewObserved() Observed { return &SystemSubnetObserved{} }

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (s *Subnet) Observe(ctx context.Context) (Observed, error) {
	obs := &SystemSubnetObserved{}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	obs.ComputeEnabled = true
	sn, err := s.Env.Cloud.Compute.GetSubnetwork(ctx, s.Env.Config.Project, s.Env.Config.HomeRegion(), s.subnet)
	if gce.IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Exists = true
	obs.Description, obs.Network, obs.Range = sn.Description, gce.LastSegment(sn.Network), sn.IpCidrRange
	obs.PrivateGoogleAccess = sn.PrivateIpGoogleAccess
	return obs, nil
}

func (s *Subnet) Plan(o Observed) []Change {
	obs := o.(*SystemSubnetObserved)
	cfg := &s.Env.Config
	want := s.rangeOf(cfg).String()
	if !obs.Exists {
		return []Change{{Action: ActionCreate, Object: s.subnet,
			Detail:   fmt.Sprintf("%s %s, Private Google Access %s", want, cfg.HomeRegion(), onOff(s.privateAccess)),
			Estimate: 15 * time.Second}}
	}
	if !isOurs(obs.Description, s.Env.ID) {
		return []Change{notOurs(s.Env, s.subnet, "install owns the shared VPC's system subnets",
			fmt.Sprintf("gcloud compute networks subnets delete %s --region %s", s.subnet, cfg.HomeRegion()))}
	}
	var wrong []string
	if obs.Network != NetworkName {
		wrong = append(wrong, "it is in network "+obs.Network)
	}
	if obs.Range != want {
		wrong = append(wrong, fmt.Sprintf("its range is %s, not %s", obs.Range, want))
	}
	if len(wrong) > 0 {
		return []Change{{Action: ActionFail, Object: s.subnet, Detail: "differs: " + strings.Join(wrong, "; "),
			Why: "a subnet's network and range can't change in place", Fix: "silo uninstall, then install again"}}
	}
	if obs.PrivateGoogleAccess != s.privateAccess {
		return []Change{{Action: ActionUpdate, Object: s.subnet, Detail: "Private Google Access " + onOff(s.privateAccess),
			Estimate: 10 * time.Second}}
	}
	return []Change{{Action: ActionOK, Object: s.subnet, Detail: want}}
}

func (s *Subnet) PlanRemove(o Observed) []Change {
	obs := o.(*SystemSubnetObserved)
	if !obs.Exists {
		return []Change{{Action: ActionOK, Detail: "no subnet " + s.subnet}}
	}
	if !isOurs(obs.Description, s.Env.ID) {
		return []Change{{Action: ActionOK, Object: s.subnet, Detail: "is not this install's; left alone"}}
	}
	return []Change{{Action: ActionDelete, Object: s.subnet, Estimate: 15 * time.Second}}
}

func (s *Subnet) Apply(ctx context.Context, changes []Change) error {
	c := s.Env.Cloud.Compute
	project, region := s.Env.Config.Project, s.Env.Config.HomeRegion()
	for _, ch := range changes {
		var op *compute.Operation
		var err error
		switch ch.Action {
		case ActionCreate:
			op, err = c.InsertSubnetwork(ctx, project, region, &compute.Subnetwork{
				Name:                  s.subnet,
				Description:           owned(s.Env, s.step),
				Network:               networkPath(project),
				IpCidrRange:           s.rangeOf(&s.Env.Config).String(),
				PrivateIpGoogleAccess: s.privateAccess,
				ForceSendFields:       []string{"PrivateIpGoogleAccess"},
			})
			if gce.IsAlreadyExists(err) {
				continue
			}
		case ActionUpdate:
			if err := s.Env.Cloud.ComputeExtras.SetPrivateIpGoogleAccess(ctx, project, region, s.subnet, s.privateAccess); err != nil {
				return err
			}
			continue
		case ActionDelete:
			op, err = c.DeleteSubnetwork(ctx, project, region, s.subnet)
			if gce.IsNotFound(err) {
				continue
			}
		default:
			continue
		}
		if err != nil {
			return err
		}
		if err := await(ctx, c, project, op); err != nil {
			return err
		}
	}
	return nil
}

// NAT is step 7, nat: the Cloud Router silo-router and its Cloud NAT
// silo-gateway-nat, for silo-gateway only. Silos' subnets get no NAT:
// their way out is the gateway's proxy.
type NAT struct{ Env *Env }

// NATObserved is the router and its NAT as GCE has them.
type NATObserved struct {
	ComputeEnabled bool       `json:"computeEnabled"`
	Exists         bool       `json:"exists"`
	Description    string     `json:"description,omitempty"`
	Network        string     `json:"network,omitempty"`
	NAT            *NATConfig `json:"nat,omitempty"`
}

// NATConfig is the part of silo-gateway-nat install owns.
type NATConfig struct {
	IPAllocation string `json:"ipAllocation"`
	RangesToNAT  string `json:"rangesToNat"`
	// Subnets are the subnetworks it serves, each as name:ranges.
	Subnets []string `json:"subnets,omitempty"`
}

func (s *NAT) Name() string          { return "nat" }
func (s *NAT) homeRegion() string    { return s.Env.Config.HomeRegion() }
func (s *NAT) NewObserved() Observed { return &NATObserved{} }

func (s *NAT) wantConfig() NATConfig {
	return NATConfig{IPAllocation: "AUTO_ONLY", RangesToNAT: "LIST_OF_SUBNETWORKS",
		Subnets: []string{GatewaySubnetName + ":ALL_IP_RANGES"}}
}

func (s *NAT) wantNAT() *compute.RouterNat {
	project, region := s.Env.Config.Project, s.Env.Config.HomeRegion()
	return &compute.RouterNat{
		Name:                          NATName,
		NatIpAllocateOption:           "AUTO_ONLY",
		SourceSubnetworkIpRangesToNat: "LIST_OF_SUBNETWORKS",
		Subnetworks: []*compute.RouterNatSubnetworkToNat{{
			Name:                gce.SelfLinkBase + subnetPath(project, region, GatewaySubnetName),
			SourceIpRangesToNat: []string{"ALL_IP_RANGES"},
		}},
	}
}

func natConfig(n *compute.RouterNat) *NATConfig {
	c := &NATConfig{IPAllocation: n.NatIpAllocateOption, RangesToNAT: n.SourceSubnetworkIpRangesToNat}
	for _, sn := range n.Subnetworks {
		c.Subnets = append(c.Subnets, gce.LastSegment(sn.Name)+":"+strings.Join(sn.SourceIpRangesToNat, ","))
	}
	return c
}

func (s *NAT) Observe(ctx context.Context) (Observed, error) {
	obs := &NATObserved{}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	obs.ComputeEnabled = true
	r, err := s.Env.Cloud.ComputeExtras.GetRouter(ctx, s.Env.Config.Project, s.Env.Config.HomeRegion(), RouterName)
	if IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Exists, obs.Description, obs.Network = true, r.Description, gce.LastSegment(r.Network)
	for _, n := range r.Nats {
		if n.Name == NATName {
			obs.NAT = natConfig(n)
		}
	}
	return obs, nil
}

func (s *NAT) Plan(o Observed) []Change {
	obs := o.(*NATObserved)
	region := s.Env.Config.HomeRegion()
	if !obs.Exists {
		return []Change{{Action: ActionCreate, Object: RouterName,
			Detail:   fmt.Sprintf("%s, with NAT %s for %s only", region, NATName, GatewaySubnetName),
			Estimate: 20 * time.Second}}
	}
	if !isOurs(obs.Description, s.Env.ID) {
		return []Change{notOurs(s.Env, RouterName, "install owns the gateway's router and NAT",
			fmt.Sprintf("gcloud compute routers delete %s --region %s", RouterName, region))}
	}
	if obs.Network != NetworkName {
		return []Change{{Action: ActionFail, Object: RouterName, Detail: "differs: it is in network " + obs.Network,
			Why: "a router's network can't change in place", Fix: "silo uninstall, then install again"}}
	}
	want := s.wantConfig()
	switch {
	case obs.NAT == nil:
		return []Change{{Action: ActionCreate, Object: NATName, Detail: "for " + GatewaySubnetName + " only", Estimate: 20 * time.Second}}
	case obs.NAT.IPAllocation != want.IPAllocation || obs.NAT.RangesToNAT != want.RangesToNAT ||
		strings.Join(obs.NAT.Subnets, " ") != strings.Join(want.Subnets, " "):
		return []Change{{Action: ActionUpdate, Object: NATName, Detail: "for " + GatewaySubnetName + " only, with automatic addresses",
			Estimate: 20 * time.Second}}
	}
	return []Change{{Action: ActionOK, Object: RouterName, Detail: "with " + NATName}}
}

func (s *NAT) PlanRemove(o Observed) []Change {
	obs := o.(*NATObserved)
	if !obs.Exists {
		return []Change{{Action: ActionOK, Detail: "no router " + RouterName}}
	}
	if !isOurs(obs.Description, s.Env.ID) {
		return []Change{{Action: ActionOK, Object: RouterName, Detail: "is not this install's; left alone"}}
	}
	return []Change{{Action: ActionDelete, Object: RouterName, Detail: "and " + NATName, Estimate: 20 * time.Second}}
}

func (s *NAT) Apply(ctx context.Context, changes []Change) error {
	x := s.Env.Cloud.ComputeExtras
	project, region := s.Env.Config.Project, s.Env.Config.HomeRegion()
	for _, ch := range changes {
		switch {
		case ch.Object == RouterName && ch.Action == ActionCreate:
			err := x.InsertRouter(ctx, project, region, &compute.Router{
				Name:        RouterName,
				Description: owned(s.Env, "the gateway's router"),
				Network:     networkPath(project),
				Nats:        []*compute.RouterNat{s.wantNAT()},
			})
			if !IsAlreadyExists(err) {
				if err != nil {
					return err
				}
				continue
			}
			fallthrough
		case ch.Action == ActionCreate || ch.Action == ActionUpdate:
			r, err := x.GetRouter(ctx, project, region, RouterName)
			if err != nil {
				return err
			}
			nats := []*compute.RouterNat{s.wantNAT()}
			for _, n := range r.Nats {
				if n.Name != NATName {
					nats = append(nats, n)
				}
			}
			if err := x.PatchRouter(ctx, project, region, &compute.Router{Name: RouterName, Nats: nats}); err != nil {
				return err
			}
		case ch.Action == ActionDelete:
			if err := x.DeleteRouter(ctx, project, region, RouterName); err != nil && !IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// Address is step 9, address: the static internal address
// silo-gateway-ip in silo-gateway, which the control VM (or the gateway
// NLB) holds and silo.internal names.
type Address struct{ Env *Env }

// AddressObserved is the address as GCE has it.
type AddressObserved struct {
	ComputeEnabled bool              `json:"computeEnabled"`
	Exists         bool              `json:"exists"`
	Labels         map[string]string `json:"labels,omitempty"`
	Type           string            `json:"type,omitempty"`
	Subnetwork     string            `json:"subnetwork,omitempty"`
	Address        string            `json:"address,omitempty"`
}

func (s *Address) Name() string          { return "address" }
func (s *Address) homeRegion() string    { return s.Env.Config.HomeRegion() }
func (s *Address) NewObserved() Observed { return &AddressObserved{} }

func (s *Address) Observe(ctx context.Context) (Observed, error) {
	obs := &AddressObserved{}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	obs.ComputeEnabled = true
	a, err := s.Env.Cloud.ComputeExtras.GetAddress(ctx, s.Env.Config.Project, s.Env.Config.HomeRegion(), GatewayAddress)
	if IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Exists, obs.Labels, obs.Type = true, a.Labels, a.AddressType
	obs.Subnetwork, obs.Address = gce.LastSegment(a.Subnetwork), a.Address
	return obs, nil
}

func (s *Address) Plan(o Observed) []Change {
	obs := o.(*AddressObserved)
	region := s.Env.Config.HomeRegion()
	if !obs.Exists {
		return []Change{{Action: ActionCreate, Object: GatewayAddress, Detail: "internal, in " + GatewaySubnetName,
			Estimate: 5 * time.Second}}
	}
	del := fmt.Sprintf("gcloud compute addresses delete %s --region %s", GatewayAddress, region)
	if obs.Labels[LabelKey] != s.Env.ID {
		return []Change{notOurs(s.Env, GatewayAddress, "silo.internal names install's gateway address", del)}
	}
	if obs.Type != "INTERNAL" || obs.Subnetwork != GatewaySubnetName {
		return []Change{{Action: ActionFail, Object: GatewayAddress,
			Detail: fmt.Sprintf("differs: it is a %s address in %q, not an internal one in %s", obs.Type, obs.Subnetwork, GatewaySubnetName),
			Why:    "an address's type and subnet can't change in place", Fix: del}}
	}
	return []Change{{Action: ActionOK, Object: GatewayAddress, Detail: obs.Address}}
}

func (s *Address) PlanRemove(o Observed) []Change {
	obs := o.(*AddressObserved)
	if !obs.Exists {
		return []Change{{Action: ActionOK, Detail: "no address " + GatewayAddress}}
	}
	if obs.Labels[LabelKey] != s.Env.ID {
		return []Change{{Action: ActionOK, Object: GatewayAddress, Detail: "is not this install's; left alone"}}
	}
	return []Change{{Action: ActionDelete, Object: GatewayAddress, Detail: obs.Address}}
}

func (s *Address) Apply(ctx context.Context, changes []Change) error {
	x := s.Env.Cloud.ComputeExtras
	project, region := s.Env.Config.Project, s.Env.Config.HomeRegion()
	for _, ch := range changes {
		var err error
		switch ch.Action {
		case ActionCreate:
			err = x.InsertAddress(ctx, project, region, &compute.Address{
				Name:        GatewayAddress,
				Description: owned(s.Env, "the gateway's address, silo.internal"),
				AddressType: "INTERNAL",
				Subnetwork:  subnetPath(project, region, GatewaySubnetName),
				Labels:      map[string]string{LabelKey: s.Env.ID},
			})
			if IsAlreadyExists(err) {
				err = nil
			}
		case ActionDelete:
			if err = x.DeleteAddress(ctx, project, region, GatewayAddress); IsNotFound(err) {
				err = nil
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}
