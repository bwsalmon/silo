package install

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

// NetworkName is the shared VPC's fixed name.
const NetworkName = "silo"

// DescriptionPrefix marks what install creates where GCE has no
// labels, such as networks.
const DescriptionPrefix = "silo-install:"

// LabelKey is the label on everything install creates, with the
// install's id as its value.
const LabelKey = "silo-install"

// Record is the install's flags that can't change in place. The network
// step writes it into the shared VPC's description, behind
// DescriptionPrefix and the install's id, and prereqs reads it back to
// stop a re-run that changes one of them.
type Record struct {
	ID           string `json:"id"`
	AddressSpace string `json:"addressSpace"`
	GatewayRange string `json:"gatewayRange"`
	ControlRange string `json:"controlRange"`
	HomeRegion   string `json:"homeRegion"`
}

// RecordOf is cfg's record.
func RecordOf(env *Env) Record {
	c := &env.Config
	return Record{ID: env.ID, AddressSpace: c.AddressSpace.String(), GatewayRange: c.GatewayRange.String(),
		ControlRange: c.ControlRange.String(), HomeRegion: c.HomeRegion()}
}

// Description is the record as a network description.
func (r Record) Description() string {
	return fmt.Sprintf("%s%s address-space=%s gateway-range=%s control-range=%s home-region=%s",
		DescriptionPrefix, r.ID, r.AddressSpace, r.GatewayRange, r.ControlRange, r.HomeRegion)
}

// ParseRecord reads a description Description wrote. ok is false for
// one install didn't write.
func ParseRecord(desc string) (r Record, ok bool) {
	rest, ok := strings.CutPrefix(desc, DescriptionPrefix)
	if !ok {
		return Record{}, false
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return Record{}, false
	}
	r.ID = f[0]
	for _, kv := range f[1:] {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "address-space":
			r.AddressSpace = v
		case "gateway-range":
			r.GatewayRange = v
		case "control-range":
			r.ControlRange = v
		case "home-region":
			r.HomeRegion = v
		}
	}
	return r, true
}

// Differences names the flags whose values differ between an existing
// install's record and this run's, with both values.
func (r Record) Differences(now Record) []string {
	var out []string
	for _, d := range []struct{ flag, was, is string }{
		{"--address-space", r.AddressSpace, now.AddressSpace},
		{"--gateway-range", r.GatewayRange, now.GatewayRange},
		{"--control-range", r.ControlRange, now.ControlRange},
		{"the home region (the first of --regions)", r.HomeRegion, now.HomeRegion},
	} {
		if d.was != "" && d.was != d.is {
			out = append(out, fmt.Sprintf("%s is %s, but the install has %s", d.flag, d.is, d.was))
		}
	}
	return out
}

// Network is step 5, network: the custom-mode VPC silo, with global
// routing and the network firewall policy evaluated before classic
// firewall rules.
type Network struct{ Env *Env }

// NetworkObserved is the VPC silo as GCE has it, or Exists false.
type NetworkObserved struct {
	ComputeEnabled   bool   `json:"computeEnabled"`
	Exists           bool   `json:"exists"`
	Description      string `json:"description,omitempty"`
	AutoSubnets      bool   `json:"autoSubnets,omitempty"`
	RoutingMode      string `json:"routingMode,omitempty"`
	EnforcementOrder string `json:"enforcementOrder,omitempty"`
}

func (s *Network) Name() string          { return "network" }
func (s *Network) NewObserved() Observed { return &NetworkObserved{} }

func (s *Network) Observe(ctx context.Context) (Observed, error) {
	obs := &NetworkObserved{}
	on, err := s.Env.Enabled(ctx, "compute.googleapis.com")
	if err != nil || !on {
		// With compute off, the project has no networks.
		return obs, err
	}
	obs.ComputeEnabled = true
	n, err := s.Env.Cloud.Compute.GetNetwork(ctx, s.Env.Config.Project, NetworkName)
	if gce.IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Exists = true
	obs.Description = n.Description
	obs.AutoSubnets = n.AutoCreateSubnetworks
	obs.EnforcementOrder = n.NetworkFirewallPolicyEnforcementOrder
	if n.RoutingConfig != nil {
		obs.RoutingMode = n.RoutingConfig.RoutingMode
	}
	return obs, nil
}

func (s *Network) Plan(o Observed) []Change {
	obs := o.(*NetworkObserved)
	rec := RecordOf(s.Env)
	if !obs.Exists {
		return []Change{{Action: ActionCreate, Object: NetworkName,
			Detail: "(custom, global routing, firewall policy first)", Estimate: 30 * time.Second}}
	}
	got, ours := ParseRecord(obs.Description)
	if !ours {
		return []Change{{Action: ActionFail, Object: NetworkName, Detail: "exists, and install didn't create it",
			Why: "install owns the VPC silo: the fence and every silo's subnets live in it",
			Fix: fmt.Sprintf("delete it (gcloud compute networks delete %s --project %s), or install into another project", NetworkName, s.Env.Config.Project)}}
	}
	var wrong []string
	if got.ID != rec.ID {
		wrong = append(wrong, "it belongs to install "+got.ID)
	}
	if obs.AutoSubnets {
		wrong = append(wrong, "it is auto-mode")
	}
	if obs.RoutingMode != "GLOBAL" {
		wrong = append(wrong, "its routing is "+obs.RoutingMode)
	}
	if obs.EnforcementOrder != "BEFORE_CLASSIC_FIREWALL" {
		wrong = append(wrong, "its firewall policy is evaluated "+obs.EnforcementOrder)
	}
	if len(wrong) > 0 {
		return []Change{{Action: ActionFail, Object: NetworkName, Detail: "differs: " + strings.Join(wrong, "; "),
			Why: "install's own VPC has been changed outside install",
			Fix: "silo uninstall, then install again"}}
	}
	// A record that differs from this run's flags is prereqs' to report.
	return []Change{{Action: ActionOK, Object: NetworkName}}
}

func (s *Network) PlanRemove(o Observed) []Change {
	obs := o.(*NetworkObserved)
	if !obs.Exists {
		return []Change{{Action: ActionOK, Detail: "no VPC " + NetworkName}}
	}
	if got, ours := ParseRecord(obs.Description); !ours || got.ID != s.Env.ID {
		return []Change{{Action: ActionOK, Object: NetworkName, Detail: "is not this install's; left alone"}}
	}
	return []Change{{Action: ActionDelete, Object: NetworkName, Estimate: 30 * time.Second}}
}

func (s *Network) Apply(ctx context.Context, changes []Change) error {
	c := s.Env.Cloud.Compute
	project := s.Env.Config.Project
	for _, ch := range changes {
		var op *compute.Operation
		var err error
		switch ch.Action {
		case ActionCreate:
			op, err = c.InsertNetwork(ctx, project, &compute.Network{
				Name:                                  NetworkName,
				Description:                           RecordOf(s.Env).Description(),
				AutoCreateSubnetworks:                 false,
				RoutingConfig:                         &compute.NetworkRoutingConfig{RoutingMode: "GLOBAL"},
				NetworkFirewallPolicyEnforcementOrder: "BEFORE_CLASSIC_FIREWALL",
				ForceSendFields:                       []string{"AutoCreateSubnetworks"},
			})
			if gce.IsAlreadyExists(err) {
				continue
			}
		case ActionDelete:
			op, err = c.DeleteNetwork(ctx, project, NetworkName)
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

// await waits for op and returns its failure as an error.
func await(ctx context.Context, c gce.Operations, project string, op *compute.Operation) error {
	done, err := gce.Await(ctx, c, project, op)
	if err != nil {
		return err
	}
	if done.Error != nil && len(done.Error.Errors) > 0 {
		e := done.Error.Errors[0]
		return fmt.Errorf("%s %s: %s: %s", done.OperationType, gce.LastSegment(done.TargetLink), e.Code, e.Message)
	}
	return nil
}

// parsePrefix parses a recorded or observed range; invalid is the zero
// Prefix.
func parsePrefix(s string) netip.Prefix {
	p, _ := netip.ParsePrefix(s)
	return p
}

// LoadRecord fills cfg's home region and ranges from the record on the
// VPC silo, where they aren't set: uninstall takes no --regions, and
// its regional steps find their objects in the home region. It changes
// nothing when compute is off or there is no VPC silo of install's.
func LoadRecord(ctx context.Context, cfg *Config, cloud Cloud) error {
	env := NewEnv(*cfg, cloud)
	if on, err := computeOn(ctx, env); err != nil || !on {
		return err
	}
	n, err := cloud.Compute.GetNetwork(ctx, cfg.Project, NetworkName)
	if gce.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rec, ok := ParseRecord(n.Description)
	if !ok {
		return nil
	}
	if len(cfg.Regions) == 0 && rec.HomeRegion != "" {
		cfg.Regions = []string{rec.HomeRegion}
	}
	for _, p := range []struct {
		dst *netip.Prefix
		s   string
	}{{&cfg.AddressSpace, rec.AddressSpace}, {&cfg.GatewayRange, rec.GatewayRange}, {&cfg.ControlRange, rec.ControlRange}} {
		if v := parsePrefix(p.s); v.IsValid() {
			*p.dst = v
		}
	}
	return nil
}
