package install

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Env is what every step shares: the flags, the cloud, the install's
// id, and which services the project has enabled.
type Env struct {
	Config Config
	Cloud  Cloud
	// ID is the value of the silo-install label (InstallID).
	ID string
	// NewSuffix names a new database instance silo-db-<suffix>; nil is
	// four random characters. Tests fix it.
	NewSuffix func() string
	// Progress is where steps write what they found (verify's report);
	// nil discards it.
	Progress io.Writer

	mu       sync.Mutex
	services map[string]bool
	// digest is ImageDigest's, once read.
	digest string
	// harnessDigest is the harness image's, with --agent-sessions.
	harnessDigest string
}

// NewEnv returns the Env for cfg, which Validate has passed.
func NewEnv(cfg Config, cloud Cloud) *Env {
	return &Env{Config: cfg, Cloud: cloud, ID: InstallID(cfg.Project)}
}

// Enabled reports whether service is enabled on the project, from one
// listing kept until ForgetServices.
func (e *Env) Enabled(ctx context.Context, service string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.services == nil {
		list, err := e.Cloud.ServiceUsage.ListEnabledServices(ctx, e.Config.Project)
		if err != nil {
			return false, fmt.Errorf("listing the project's enabled services: %w", err)
		}
		e.services = map[string]bool{}
		for _, s := range list {
			e.services[s] = true
		}
	}
	return e.services[service], nil
}

// ForgetServices drops the listing Enabled keeps, after a step has
// enabled services.
func (e *Env) ForgetServices() {
	e.mu.Lock()
	e.services = nil
	e.mu.Unlock()
}

// Steps are install's steps, in order (install.md "What install sets
// up"), install.md's 20.
func Steps(env *Env) []Step {
	return []Step{
		&Prereqs{Env: env},
		&APIs{Env: env},
		&DBInstance{Env: env},
		&SilodAccount{Env: env},
		&SilodRole{Env: env},
		&Network{Env: env},
		GatewaySubnet(env),
		ControlSubnet(env),
		&NAT{Env: env},
		&Fence{Env: env},
		&Address{Env: env},
		&DNSStep{Env: env},
		&Image{Env: env},
		&DB{Env: env},
		&DBEndpointStep{Env: env},
		&Migrate{Env: env},
		&Control{Env: env},
		&Root{Env: env},
		&Admin{Env: env},
		&Verify{Env: env},
	}
}

// PlanStep observes s and plans it.
func PlanStep(ctx context.Context, s Step) StepPlan {
	obs, err := s.Observe(ctx)
	if err != nil {
		return StepPlan{Step: s.Name(), Err: err}
	}
	return StepPlan{Step: s.Name(), Changes: s.Plan(obs)}
}

// PlanAll is --dry-run: every step observed and planned, in order.
// Nothing is changed, so a step after one that would create something
// plans against the cloud as it is now, which its Observe allows for
// (a step needing an API that isn't enabled yet observes nothing).
func PlanAll(ctx context.Context, steps []Step) []StepPlan {
	plans := make([]StepPlan, 0, len(steps))
	for _, s := range steps {
		plans = append(plans, PlanStep(ctx, s))
	}
	return plans
}

// PlanRemoveAll is uninstall's plan: every Remover, in reverse order.
func PlanRemoveAll(ctx context.Context, steps []Step) []StepPlan {
	var plans []StepPlan
	for i := len(steps) - 1; i >= 0; i-- {
		r, ok := steps[i].(Remover)
		if !ok {
			continue
		}
		if noHomeRegion(r) {
			plans = append(plans, StepPlan{Step: r.Name(), Changes: noRecordChanges()})
			continue
		}
		obs, err := r.Observe(ctx)
		if err != nil {
			plans = append(plans, StepPlan{Step: r.Name(), Err: err})
			continue
		}
		plans = append(plans, StepPlan{Step: r.Name(), Changes: r.PlanRemove(obs)})
	}
	return plans
}

// homeRegional is a step whose objects are all keyed by the home
// region. Uninstall takes the home region from the install's record
// (LoadRecord), which the network step writes before any of them
// exists; with no record there is no home region, and nothing of
// theirs to find, so uninstall plans them ok without observing.
type homeRegional interface{ homeRegion() string }

func noHomeRegion(s Step) bool {
	h, ok := s.(homeRegional)
	return ok && h.homeRegion() == ""
}

func noRecordChanges() []Change {
	return []Change{{Action: ActionOK, Detail: "nothing to do: no install record names a home region"}}
}

// Reporter is a step that reports what it found even when it has
// nothing to apply: verify, which prints the root.
type Reporter interface {
	Report(ctx context.Context, w io.Writer) error
}

// FailedError is a step whose plan failed while applying.
type FailedError struct{ Plan StepPlan }

func (e *FailedError) Error() string {
	if e.Plan.Err != nil {
		return fmt.Sprintf("%s: %v", e.Plan.Step, e.Plan.Err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s failed:", e.Plan.Step)
	for _, c := range e.Plan.Changes {
		if c.Action != ActionFail {
			continue
		}
		fmt.Fprintf(&b, "\n  %s", strings.TrimSpace(c.Object+" "+c.Detail))
		if c.Why != "" {
			fmt.Fprintf(&b, "\n    why: %s", c.Why)
		}
		if c.Fix != "" {
			fmt.Fprintf(&b, "\n    fix: %s", c.Fix)
		}
	}
	return b.String()
}

// Apply runs every step in order, each observed and planned afresh just
// before it is applied, so that a re-run finishes an interrupted
// install. It stops at the first step that fails. A step with deferred
// checks is planned again after each later step that changes something,
// until its checks have all run.
func Apply(ctx context.Context, steps []Step, progress io.Writer) error {
	var deferred []Step
	for _, s := range steps {
		p := PlanStep(ctx, s)
		if p.Failed() {
			return &FailedError{Plan: p}
		}
		if r, ok := s.(Reporter); ok && !p.Changing() {
			if err := r.Report(ctx, progress); err != nil {
				return fmt.Errorf("%s: %w", s.Name(), err)
			}
		}
		if p.Changing() {
			fmt.Fprintf(progress, "%s: %s ...\n", s.Name(), p.Action())
			if err := s.Apply(ctx, p.Changes); err != nil {
				return fmt.Errorf("%s: %w", s.Name(), err)
			}
			fmt.Fprintf(progress, "%s: done\n", s.Name())
			still := deferred[:0]
			for _, d := range deferred {
				dp := PlanStep(ctx, d)
				if dp.Failed() {
					return &FailedError{Plan: dp}
				}
				if dp.Deferred() {
					still = append(still, d)
				} else {
					fmt.Fprintf(progress, "%s: every check has now run\n", d.Name())
				}
			}
			deferred = still
		}
		if p.Deferred() {
			deferred = append(deferred, s)
		}
	}
	if len(deferred) > 0 {
		return &FailedError{Plan: StepPlan{Step: deferred[0].Name(), Err: fmt.Errorf("some checks could not run even after every step")}}
	}
	return nil
}

// Remove is uninstall's Apply: every Remover in reverse order, each
// observed and planned afresh.
func Remove(ctx context.Context, steps []Step, progress io.Writer) error {
	for i := len(steps) - 1; i >= 0; i-- {
		r, ok := steps[i].(Remover)
		if !ok || noHomeRegion(r) {
			continue
		}
		obs, err := r.Observe(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", r.Name(), err)
		}
		p := StepPlan{Step: r.Name(), Changes: r.PlanRemove(obs)}
		if p.Failed() {
			return &FailedError{Plan: p}
		}
		if !p.Changing() {
			continue
		}
		fmt.Fprintf(progress, "%s: %s ...\n", r.Name(), p.Action())
		if err := r.Apply(ctx, p.Changes); err != nil {
			return fmt.Errorf("%s: %w", r.Name(), err)
		}
		fmt.Fprintf(progress, "%s: done\n", r.Name())
	}
	return nil
}
