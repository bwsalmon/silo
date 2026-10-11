package install

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/ledger/rootcap"
)

// SilodTimeout is how long the root step waits for silod to serve.
var SilodTimeout = 10 * time.Minute

// observeSilod is silod's health through the front door, if the
// control VM exists to reach it on: nil, and why not, otherwise. It
// never fails the observation: a silod that isn't serving yet is what
// the root step waits for.
func observeSilod(ctx context.Context, env *Env) (zone string, h *SilodHealth, why string, err error) {
	on, err := computeOn(ctx, env)
	if err != nil || !on {
		return "", nil, "", err
	}
	if zone, err = env.Cloud.ComputeExtras.InstanceZone(ctx, env.Config.Project, env.Config.HomeRegion(), ControlVM); err != nil || zone == "" {
		return zone, nil, "", err
	}
	if env.Cloud.Silod == nil {
		return zone, nil, "no front door to reach silod through", nil
	}
	got, herr := env.Cloud.Silod.Health(ctx)
	if herr != nil {
		return zone, nil, herr.Error(), nil
	}
	return zone, &got, "", nil
}

// Root is step 13, root: once silod serves, a call to its bootstrap
// endpoint through the front door, as the installer, creates the root
// silo with its block (--address-space, with --gateway-range and
// --control-range held back), --regions, the capacity the project's
// quotas give (rootcap, as silod reads it hourly), --root-memory,
// --egress-allow, --root-grant-policy, --agent-sessions,
// --root-model-budget, --budget-period, and each --admin as a member.
// silod accepts bootstrap only while there is no root, so once there is
// one this step is ok whatever the flags now say: the root is changed
// through the fence API, not by install.
type Root struct{ Env *Env }

// RootObserved is whether silod serves and has a root, and the capacity
// a new root would get.
type RootObserved struct {
	ControlZone string       `json:"controlZone,omitempty"`
	Health      *SilodHealth `json:"health,omitempty"`
	Unreachable string       `json:"unreachable,omitempty"`
	// Capacity is region ("global" for global classes) to class to
	// amount; NoCapacity are the region/class pairs the quotas give
	// nothing.
	Capacity      map[string]map[string]string `json:"capacity,omitempty"`
	NoCapacity    []string                     `json:"noCapacity,omitempty"`
	CapacityError string                       `json:"capacityError,omitempty"`
}

func (s *Root) Name() string          { return "root" }
func (s *Root) NewObserved() Observed { return &RootObserved{} }

func (s *Root) Observe(ctx context.Context) (Observed, error) {
	obs := &RootObserved{}
	var err error
	if obs.ControlZone, obs.Health, obs.Unreachable, err = observeSilod(ctx, s.Env); err != nil {
		return nil, err
	}
	if obs.Health != nil && obs.Health.Root {
		return obs, nil
	}
	on, err := computeOn(ctx, s.Env)
	if err != nil || !on {
		return obs, err
	}
	obs.Capacity, obs.NoCapacity, err = readCapacity(ctx, s.Env)
	if err != nil {
		obs.CapacityError = err.Error()
	}
	return obs, nil
}

// readCapacity is the root's capacity as silod's hourly read would set
// it.
func readCapacity(ctx context.Context, env *Env) (map[string]map[string]string, []string, error) {
	cfg := &env.Config
	res, err := rootcap.Read(ctx, env.Cloud.Compute, rootcap.Config{Project: cfg.Project, RootMemoryMB: cfg.RootMemoryMB}, cfg.Regions, nil)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]map[string]string{}
	for _, a := range res.Capacity {
		if out[a.Region] == nil {
			out[a.Region] = map[string]string{}
		}
		out[a.Region][a.Class] = ledger.FormatAmount(a.Amount)
		if a.Amount == ledger.Unlimited {
			out[a.Region][a.Class] = "unlimited"
		}
	}
	var none []string
	for _, k := range res.NoCapacity {
		none = append(none, k.Region+"/"+k.Class)
	}
	sort.Strings(none)
	return out, none, nil
}

// HeldBack is what --address-space gives up to the gateway and control
// subnets, as one range where they are the two halves of one.
func HeldBack(cfg *Config) []netip.Prefix {
	g, c := cfg.GatewayRange, cfg.ControlRange
	if g.Bits() == c.Bits() && g.Bits() > 0 {
		pg, _ := g.Addr().Prefix(g.Bits() - 1)
		pc, _ := c.Addr().Prefix(c.Bits() - 1)
		if pg == pc {
			return []netip.Prefix{pg}
		}
	}
	out := []netip.Prefix{g, c}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr().Less(out[j].Addr()) })
	return out
}

// summaryClasses are the classes the plan names for each region; every
// GPU and Hyperdisk class the quotas give nothing is named as well.
var summaryClasses = []string{"vcpu", "ssd_disk_gb", "local_ssd_gb"}

func (s *Root) Plan(o Observed) []Change {
	obs := o.(*RootObserved)
	cfg := &s.Env.Config
	if obs.Health != nil && obs.Health.Root {
		return []Change{{Action: ActionOK, Object: "/", Detail: "exists (changed through the fence API, not by install)"}}
	}
	if obs.CapacityError != "" {
		return []Change{{Action: ActionFail, Object: "/", Detail: "reading the project's quotas: " + obs.CapacityError,
			Why: "the root's capacity is the project's quota", Fix: "check the installer can read compute regions and Cloud Quotas"}}
	}
	var held []string
	for _, p := range HeldBack(cfg) {
		held = append(held, p.String())
	}
	wait := 10 * time.Second
	if obs.Health == nil {
		wait = 2 * time.Minute // silod starting on a new control VM
	}
	out := []Change{{Action: ActionCreate, Object: "/",
		Detail:   fmt.Sprintf("blocks %s (minus %s)", cfg.AddressSpace, strings.Join(held, ", ")),
		Estimate: wait}}
	if obs.Capacity == nil {
		out = append(out, Change{Action: ActionCreate, Detail: strings.Join(cfg.Regions, ", ") + ": capacity read from the project's quotas once compute is on"})
		return out
	}
	none := map[string]bool{}
	for _, k := range obs.NoCapacity {
		none[k] = true
	}
	var regions []string
	for _, r := range cfg.Regions {
		caps := obs.Capacity[r]
		var parts []string
		for _, c := range summaryClasses {
			if v, ok := caps[c]; ok {
				parts = append(parts, c+" "+v)
			}
		}
		var classes []string
		for c := range caps {
			if (strings.HasSuffix(c, "_gpus") || strings.HasPrefix(c, "hyperdisk_")) && none[r+"/"+c] {
				classes = append(classes, c)
			}
		}
		sort.Strings(classes)
		for _, c := range classes {
			parts = append(parts, c+" 0 (no quota)")
		}
		if cfg.RootMemoryMB == 0 {
			parts = append(parts, "memory unlimited")
		} else {
			parts = append(parts, fmt.Sprintf("memory_mb %d", cfg.RootMemoryMB))
		}
		regions = append(regions, r+" "+strings.Join(parts, ", "))
	}
	out = append(out, Change{Action: ActionCreate, Detail: strings.Join(regions, "; ")})
	var policy []string
	egress := "egress none"
	if len(cfg.EgressAllow) > 0 {
		egress = "egress " + strings.Join(cfg.EgressAllow, ", ")
	}
	policy = append(policy, egress, "grants "+cfg.RootGrantPolicy)
	if cfg.AgentSessions > 0 {
		policy = append(policy, fmt.Sprintf("agent_sessions %d", cfg.AgentSessions))
	}
	if cfg.RootModelBudget > 0 {
		policy = append(policy, fmt.Sprintf("model budget $%s per %s", ledger.FormatAmount(int64(cfg.RootModelBudget*ledger.Micro)), cfg.BudgetPeriod))
	}
	out = append(out, Change{Action: ActionCreate, Detail: strings.Join(policy, "; ")})
	return out
}

// Request is the bootstrap call that creates the root.
func (s *Root) Request(capacity map[string]map[string]string) *BootstrapRequest {
	cfg := &s.Env.Config
	req := &BootstrapRequest{
		AddressSpace: cfg.AddressSpace.String(),
		GatewayRange: cfg.GatewayRange.String(),
		ControlRange: cfg.ControlRange.String(),
		Regions:      cfg.Regions,
		Capacity:     capacity,
		BudgetPeriod: cfg.BudgetPeriod,
		Egress:       &RootEgress{Allow: append([]string{}, cfg.EgressAllow...), Deny: []string{}},
		GrantPolicy:  map[string]string{},
		Admins:       cfg.Admins,
		Labels:       map[string]string{LabelKey: s.Env.ID},
	}
	for _, kv := range strings.Split(cfg.RootGrantPolicy, ",") {
		k, v, _ := strings.Cut(kv, "=")
		req.GrantPolicy[k] = v
	}
	if cfg.RootModelBudget > 0 {
		req.ModelBudgetUSD = ledger.FormatAmount(int64(cfg.RootModelBudget * ledger.Micro))
	}
	if cfg.AgentSessions > 0 {
		for _, r := range cfg.Regions {
			if req.Capacity[r] == nil {
				req.Capacity[r] = map[string]string{}
			}
		}
		if req.Capacity[ledger.Global] == nil {
			req.Capacity[ledger.Global] = map[string]string{}
		}
		req.Capacity[ledger.Global]["agent_sessions"] = fmt.Sprint(cfg.AgentSessions)
	}
	return req
}

// waitSilod polls silod's health until it serves.
func waitSilod(ctx context.Context, env *Env) (*SilodHealth, error) {
	if env.Cloud.Silod == nil {
		return nil, fmt.Errorf("no front door to reach silod through")
	}
	ctx, cancel := context.WithTimeout(ctx, SilodTimeout)
	defer cancel()
	var last error
	for {
		h, err := env.Cloud.Silod.Health(ctx)
		if err == nil && h.Status == "ok" {
			return &h, nil
		}
		if err == nil {
			err = fmt.Errorf("silod says %q", h.Status)
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("silod on %s isn't serving after %s (its serial console and Cloud Logging say why): %w", ControlVM, SilodTimeout, last)
		case <-time.After(PollInterval):
		}
	}
}

func (s *Root) Apply(ctx context.Context, changes []Change) error {
	if len(changes) == 0 || changes[0].Action != ActionCreate {
		return nil
	}
	h, err := waitSilod(ctx, s.Env)
	if err != nil {
		return err
	}
	if h.Root {
		return nil // an interrupted run's bootstrap went through
	}
	capacity, _, err := readCapacity(ctx, s.Env)
	if err != nil {
		return fmt.Errorf("reading the root's capacity: %w", err)
	}
	if err := s.Env.Cloud.Silod.Bootstrap(ctx, s.Request(capacity)); err != nil {
		// Another run may have bootstrapped meanwhile; silod refuses a
		// second root either way.
		if h, herr := s.Env.Cloud.Silod.Health(ctx); herr == nil && h.Root {
			return nil
		}
		return fmt.Errorf("bootstrapping the root: %w", err)
	}
	return nil
}
