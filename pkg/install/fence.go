package install

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/gcefence"
	"github.com/bwsalmon/silo/pkg/fence"
	compute "google.golang.org/api/compute/v1"
)

// Fence is step 8, fence: the global network firewall policy
// silo-fence, associated with silo, holding the rules that don't depend
// on the tree: ctl-deny-out, the gw-* rules and catch-in and catch-out.
// They are compiled by package fence from a tree with no silos, so they
// are exactly the rules silod's own compile gives them once the root
// exists. Until then the policy has no silo rules, and catch-* denies
// everything that isn't to or from the gateway.
//
// silod owns the rest of the policy: the grants band and the silos'
// depth bands. The step never touches a rule there, so a re-run of
// install over a live tree changes none of the tree's rules.
type Fence struct{ Env *Env }

// FenceObserved is silo-fence as GCE has it, without GCE's default
// rules.
type FenceObserved struct {
	ComputeEnabled bool                          `json:"computeEnabled"`
	Exists         bool                          `json:"exists"`
	Description    string                        `json:"description,omitempty"`
	Rules          []*compute.FirewallPolicyRule `json:"rules,omitempty"`
	// Associations are the networks the policy is associated with.
	Associations []string `json:"associations,omitempty"`
}

func (s *Fence) Name() string          { return "fence" }
func (s *Fence) NewObserved() Observed { return &FenceObserved{} }

// FenceConfig is the fence's configuration for the install's flags.
func FenceConfig(cfg *Config) gcefence.Config {
	c := gcefence.DefaultConfig
	// Either replica MIG is health checked, from Google's ranges, at the
	// gateway's ports on VMs in the gateway subnet (gw-health-in).
	c.GatewayReplicas = cfg.GatewayReplicas > 0 || cfg.ControlReplicas > 1
	return c
}

// silodOwns reports whether priority is in a band silod's fence applier
// owns: the grants, and the silos' depth bands.
func silodOwns(priority int64) bool {
	return (priority >= gcefence.GrantsBand && priority <= gcefence.GrantsEnd) ||
		(priority >= gcefence.DepthBands && priority < gcefence.CatchAllBand)
}

// fenceDefaults is the first of GCE's own rules in every policy.
const fenceDefaults int64 = 2147483644

// want is the policy's rules: install's, compiled from a tree with no
// silos, and silod's from cur as they are.
func (s *Fence) want(cur []*compute.FirewallPolicyRule) ([]*compute.FirewallPolicyRule, error) {
	cfg := &s.Env.Config
	in := &fence.Input{Gateway: cfg.GatewayRange, Control: cfg.ControlRange,
		Root: fence.Minus(cfg.AddressSpace, cfg.GatewayRange, cfg.ControlRange)}
	rules, err := gcefence.Compile(in, FenceConfig(cfg), nil)
	if err != nil {
		return nil, err
	}
	for _, r := range cur {
		if silodOwns(r.Priority) {
			rules = append(rules, r)
		}
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Priority < rules[j].Priority })
	return rules, nil
}

func (s *Fence) Observe(ctx context.Context) (Observed, error) {
	obs := &FenceObserved{}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	obs.ComputeEnabled = true
	pol, err := s.Env.Cloud.Compute.GetFirewallPolicy(ctx, s.Env.Config.Project, gcefence.PolicyName)
	if gce.IsNotFound(err) {
		return obs, nil
	}
	if err != nil {
		return nil, err
	}
	obs.Exists, obs.Description = true, pol.Description
	for _, r := range pol.Rules {
		if r.Priority < fenceDefaults {
			c := *r
			c.Kind, c.RuleTupleCount = "", 0
			obs.Rules = append(obs.Rules, &c)
		}
	}
	for _, a := range pol.Associations {
		obs.Associations = append(obs.Associations, gce.LastSegment(a.AttachmentTarget))
	}
	return obs, nil
}

// fenceOurs reports whether a policy's description is this install's,
// or silod's: silod's applier stamps the description it applies under.
func (s *Fence) ours(desc string) bool {
	return isOurs(desc, s.Env.ID) || strings.HasPrefix(desc, "silod:")
}

func ruleNames(rs []*compute.FirewallPolicyRule) string {
	var n []string
	for _, r := range rs {
		n = append(n, r.RuleName)
	}
	return strings.Join(n, ", ")
}

func (s *Fence) Plan(o Observed) []Change {
	obs := o.(*FenceObserved)
	assoc := Change{Action: ActionCreate, Object: gcefence.PolicyName + " -> " + NetworkName, Detail: "association",
		Estimate: 10 * time.Second}
	want, err := s.want(obs.Rules)
	if err != nil {
		return []Change{{Action: ActionFail, Object: gcefence.PolicyName, Detail: err.Error()}}
	}
	if !obs.Exists {
		return []Change{{Action: ActionCreate, Object: gcefence.PolicyName,
			Detail:   fmt.Sprintf("%s (no silo rules until the root exists)", ruleNames(want)),
			Estimate: time.Duration(len(want)+1) * 10 * time.Second}, assoc}
	}
	if !s.ours(obs.Description) {
		return []Change{notOurs(s.Env, gcefence.PolicyName, "the fence is install's and silod's: nothing else may write its rules",
			"gcloud compute network-firewall-policies delete "+gcefence.PolicyName+" --global")}
	}
	var out []Change
	for _, st := range gcefence.Plan(obs.Rules, want) {
		out = append(out, Change{Action: ActionUpdate, Object: gcefence.PolicyName, Detail: st.String(), Estimate: 10 * time.Second})
	}
	associated := false
	for _, a := range obs.Associations {
		associated = associated || a == NetworkName
	}
	if !associated {
		out = append(out, assoc)
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Object: gcefence.PolicyName, Detail: fmt.Sprintf("%d rules, associated with %s", len(obs.Rules), NetworkName)}}
	}
	return out
}

func (s *Fence) PlanRemove(o Observed) []Change {
	obs := o.(*FenceObserved)
	if !obs.Exists {
		return []Change{{Action: ActionOK, Detail: "no firewall policy " + gcefence.PolicyName}}
	}
	if !s.ours(obs.Description) {
		return []Change{{Action: ActionOK, Object: gcefence.PolicyName, Detail: "is not this install's; left alone"}}
	}
	return []Change{{Action: ActionDelete, Object: gcefence.PolicyName, Estimate: 20 * time.Second}}
}

func (s *Fence) Apply(ctx context.Context, changes []Change) error {
	c := s.Env.Cloud.Compute
	project := s.Env.Config.Project
	do := func(op *compute.Operation, err error) error {
		if err != nil {
			return err
		}
		return await(ctx, c, project, op)
	}
	var create, rules, associate, remove bool
	for _, ch := range changes {
		switch {
		case ch.Action == ActionDelete:
			remove = true
		case ch.Object == gcefence.PolicyName && ch.Action == ActionCreate:
			create, rules = true, true
		case ch.Object == gcefence.PolicyName:
			rules = true
		case ch.Action == ActionCreate:
			associate = true
		}
	}
	if remove {
		pol, err := c.GetFirewallPolicy(ctx, project, gcefence.PolicyName)
		if gce.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, a := range pol.Associations {
			if err := do(c.RemoveFirewallPolicyAssociation(ctx, project, gcefence.PolicyName, a.Name)); err != nil && !gce.IsNotFound(err) {
				return err
			}
		}
		if err := do(c.DeleteFirewallPolicy(ctx, project, gcefence.PolicyName)); err != nil && !gce.IsNotFound(err) {
			return err
		}
		return nil
	}
	if create {
		err := do(c.InsertFirewallPolicy(ctx, project, &compute.FirewallPolicy{
			Name: gcefence.PolicyName, Description: owned(s.Env, "the fence")}))
		if err != nil && !gce.IsAlreadyExists(err) {
			return err
		}
	}
	if rules {
		// Planned afresh against the policy as it is now: the rule steps
		// are positional, and an interrupted run may have made some.
		pol, err := c.GetFirewallPolicy(ctx, project, gcefence.PolicyName)
		if err != nil {
			return err
		}
		want, err := s.want(pol.Rules)
		if err != nil {
			return err
		}
		for _, st := range gcefence.Plan(pol.Rules, want) {
			var err error
			switch st.Op {
			case gcefence.AddRule:
				err = do(c.AddFirewallPolicyRule(ctx, project, gcefence.PolicyName, st.Rule))
			case gcefence.PatchRule:
				err = do(c.PatchFirewallPolicyRule(ctx, project, gcefence.PolicyName, st.Priority, st.Rule))
			case gcefence.RemoveRule:
				err = do(c.RemoveFirewallPolicyRule(ctx, project, gcefence.PolicyName, st.Priority))
			}
			if err != nil {
				return fmt.Errorf("%s: %w", st, err)
			}
		}
	}
	if associate || create {
		err := do(c.AddFirewallPolicyAssociation(ctx, project, gcefence.PolicyName, &compute.FirewallPolicyAssociation{
			Name: NetworkName, AttachmentTarget: networkPath(project)}, false))
		if err != nil && !gce.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}
