package fence

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

// Applier applies the compiled policy to GCE, and lets callers wait for
// a change to be in place: it is the fence API's FenceApplier and
// FenceBudget. Only one process should run Apply at a time (the
// leader); WaitApplied works in any, since the applied sequence number
// is in the database.
type Applier struct {
	Store   store.Store
	GCE     gce.Client
	Project string
	// Policy is the policy's name; empty means PolicyName.
	Policy string
	Config Config
	Grants GrantSource
	// Poll is how often WaitApplied reads the applied sequence number
	// when nothing in this process has told it of progress. Zero means
	// a second.
	Poll time.Duration
	// Attempts is how many times Apply starts over when the policy
	// changed under it. Zero means 5.
	Attempts int
	// Guard, if set, is called in the transaction that records the
	// applied sequence number, and refuses it by returning an error:
	// silod's leader checks its epoch there (leader.Guard), so that a
	// leader that lost the lease mid-apply records nothing.
	Guard func(ctx context.Context, tx store.Tx) error
	// OnDrift, if set, is called after an apply that repaired drift
	// (see Repair), whichever of Run, Apply or Repair made it, with the
	// steps that did and whether the apply then succeeded. The
	// reconcile audits it as fence_drift.
	OnDrift func(ctx context.Context, steps []Step, repaired bool)

	applying sync.Mutex // held by Apply
	mu       sync.Mutex
	applied  uint64
	wake     chan struct{}
}

// ErrStale is an apply that found the policy changed under it, by
// someone other than this Applier, more times than Attempts allows.
var ErrStale = errors.New("fence: the policy changed while it was being applied")

func (a *Applier) policy() string {
	if a.Policy == "" {
		return PolicyName
	}
	return a.Policy
}

// CheckBudget implements fenceapi.FenceBudget.
func (a *Applier) CheckBudget(ctx context.Context, tx store.Tx) error {
	return Budget{Config: a.Config, Grants: a.Grants}.CheckBudget(ctx, tx)
}

// Apply brings the policy in GCE up to the database as it is now, rule
// by rule (Plan), and records the change-log sequence number it
// compiled from, waking WaitApplied's callers. It returns that number.
//
// Every step is checked against the policy it was planned for: before
// the first, the policy's fingerprint is sent with a patch of its
// description, which GCE refuses (412) if the policy changed since it
// was read; between steps the policy is read again and must be exactly
// what the steps so far make it. compute/v1's rule methods take no
// fingerprint, so this is how a stale diff is caught. Either way the
// apply starts over from a fresh read.
func (a *Applier) Apply(ctx context.Context) (uint64, error) {
	seq, _, err := a.apply(ctx)
	return seq, err
}

// Repair is Apply for the reconcile (gcp-backend.md, "Reconcile",
// SV-77): it also returns drift, the steps it took to bring back a
// policy that had moved away from what the database compiles to at a
// sequence number already recorded as applied. Steps that only carry
// changes newer than that are not drift, and are not returned. Any
// apply that repairs drift calls OnDrift.
func (a *Applier) Repair(ctx context.Context) (drift []Step, err error) {
	_, drift, err = a.apply(ctx)
	return drift, err
}

func (a *Applier) apply(ctx context.Context) (uint64, []Step, error) {
	a.applying.Lock()
	defer a.applying.Unlock()
	attempts := a.Attempts
	if attempts <= 0 {
		attempts = 5
	}
	var err error
	var drift []Step
	for i := 0; i < attempts; i++ {
		var seq uint64
		var steps []Step
		var drifted bool
		seq, steps, drifted, err = a.applyOnce(ctx)
		// An attempt that went on to find the policy changed under it
		// still took the steps it took.
		if drifted {
			drift = append(drift, steps...)
		}
		if err == nil {
			a.done(seq)
			a.drifted(ctx, drift, true)
			return seq, drift, nil
		}
		if !errors.Is(err, ErrStale) {
			a.drifted(ctx, drift, false)
			return 0, drift, err
		}
	}
	a.drifted(ctx, drift, false)
	return 0, drift, err
}

func (a *Applier) drifted(ctx context.Context, steps []Step, repaired bool) {
	if len(steps) > 0 && a.OnDrift != nil {
		a.OnDrift(ctx, steps, repaired)
	}
}

// applyOnce returns the steps it took, and whether they were drift:
// whether seq was already recorded as applied before it started.
func (a *Applier) applyOnce(ctx context.Context) (seq uint64, taken []Step, drifted bool, err error) {
	var in *Input
	var applied uint64
	if err := a.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		if in, err = Load(ctx, tx, a.Grants); err != nil {
			return err
		}
		if applied, err = Applied(ctx, tx); err != nil {
			return err
		}
		seq, err = lastSeq(ctx, tx)
		return err
	}); err != nil {
		return 0, nil, false, err
	}
	// Nothing applied yet is a first apply, not a repair.
	drifted = applied > 0 && applied >= seq
	seq, taken, err = a.applyInput(ctx, in, seq)
	return seq, taken, drifted, err
}

func (a *Applier) applyInput(ctx context.Context, in *Input, seq uint64) (uint64, []Step, error) {
	pol, err := a.GCE.GetFirewallPolicy(ctx, a.Project, a.policy())
	if err != nil {
		return 0, nil, err
	}
	want, err := Compile(in, a.Config, pol.Rules)
	if err != nil {
		return 0, nil, err
	}
	steps := Plan(pol.Rules, want)
	var taken []Step
	if len(steps) > 0 {
		if err := a.stamp(ctx, pol, fmt.Sprintf("silod: applying the change log to %d", seq)); err != nil {
			return 0, nil, err
		}
		expect := withoutDefaults(pol.Rules)
		for i, s := range steps {
			if i > 0 {
				if err := a.check(ctx, expect); err != nil {
					return 0, taken, err
				}
			}
			if err := a.step(ctx, s); err != nil {
				return 0, taken, err
			}
			taken = append(taken, s)
			expect = applyStep(expect, s)
		}
		if err := a.check(ctx, expect); err != nil {
			return 0, taken, err
		}
	}
	if err := a.Store.Tx(ctx, func(tx store.Tx) error {
		if a.Guard != nil {
			if err := a.Guard(ctx, tx); err != nil {
				return err
			}
		}
		return setApplied(ctx, tx, seq)
	}); err != nil {
		return 0, taken, err
	}
	return seq, taken, nil
}

// stamp patches the policy's description with pol's fingerprint, so
// that GCE refuses it if the policy is no longer pol.
func (a *Applier) stamp(ctx context.Context, pol *compute.FirewallPolicy, desc string) error {
	op, err := a.GCE.PatchFirewallPolicy(ctx, a.Project, a.policy(), &compute.FirewallPolicy{Description: desc, Fingerprint: pol.Fingerprint})
	if gce.Code(err) == 412 {
		return fmt.Errorf("%w: %v", ErrStale, err)
	}
	if err != nil {
		return err
	}
	return a.await(ctx, op)
}

// check reads the policy and fails with ErrStale unless its rules are
// expect.
func (a *Applier) check(ctx context.Context, expect []*compute.FirewallPolicyRule) error {
	pol, err := a.GCE.GetFirewallPolicy(ctx, a.Project, a.policy())
	if err != nil {
		return err
	}
	got := withoutDefaults(pol.Rules)
	if len(got) != len(expect) {
		return fmt.Errorf("%w: it has %d rules, where %d were expected", ErrStale, len(got), len(expect))
	}
	for i := range got {
		if !Same(got[i], expect[i]) {
			return fmt.Errorf("%w: rule %d is %s, where %s was expected", ErrStale, got[i].Priority, got[i].RuleName, expect[i].RuleName)
		}
	}
	return nil
}

func (a *Applier) step(ctx context.Context, s Step) error {
	var op *compute.Operation
	var err error
	switch s.Op {
	case AddRule:
		op, err = a.GCE.AddFirewallPolicyRule(ctx, a.Project, a.policy(), s.Rule)
	case PatchRule:
		op, err = a.GCE.PatchFirewallPolicyRule(ctx, a.Project, a.policy(), s.Priority, s.Rule)
	case RemoveRule:
		op, err = a.GCE.RemoveFirewallPolicyRule(ctx, a.Project, a.policy(), s.Priority)
	default:
		return fmt.Errorf("fence: unknown step %s", s)
	}
	if err != nil {
		// A refusal of the step itself (a priority taken, or gone)
		// means the policy isn't what it was planned against.
		if c := gce.Code(err); c == 400 || c == 404 || c == 409 || c == 412 {
			return fmt.Errorf("%w: %s: %v", ErrStale, s, err)
		}
		return fmt.Errorf("fence: %s: %w", s, err)
	}
	if err := a.await(ctx, op); err != nil {
		return fmt.Errorf("fence: %s: %w", s, err)
	}
	return nil
}

func (a *Applier) await(ctx context.Context, op *compute.Operation) error {
	op, err := gce.Await(ctx, a.GCE, a.Project, op)
	if err != nil {
		return err
	}
	if op.Error != nil && len(op.Error.Errors) > 0 {
		e := op.Error.Errors[0]
		return fmt.Errorf("%w: operation %s failed: %s: %s", ErrStale, op.Name, e.Code, e.Message)
	}
	return nil
}

func withoutDefaults(rs []*compute.FirewallPolicyRule) []*compute.FirewallPolicyRule {
	var out []*compute.FirewallPolicyRule
	for _, r := range rs {
		if r.Priority < gceDefaults {
			out = append(out, r)
		}
	}
	return out
}

// applyStep is rs with s applied, in priority order.
func applyStep(rs []*compute.FirewallPolicyRule, s Step) []*compute.FirewallPolicyRule {
	var out []*compute.FirewallPolicyRule
	for _, r := range rs {
		if r.Priority != s.Priority {
			out = append(out, r)
		}
	}
	if s.Op != RemoveRule {
		r := *s.Rule
		r.Priority = s.Priority
		out = append(out, &r)
	}
	sortByPriority(out)
	return out
}

func sortByPriority(rs []*compute.FirewallPolicyRule) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Priority < rs[j-1].Priority; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// done records, in this process, that the policy carries seq, and wakes
// WaitApplied's callers.
func (a *Applier) done(seq uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq > a.applied {
		a.applied = seq
	}
	if a.wake != nil {
		close(a.wake)
		a.wake = nil
	}
}

func (a *Applier) waitCh() (uint64, <-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.wake == nil {
		a.wake = make(chan struct{})
	}
	return a.applied, a.wake
}

// WaitApplied implements fenceapi.FenceApplier: it returns once the
// policy in GCE carries the change log up to seq, as Apply (in this
// process or another) records it, or when ctx ends.
func (a *Applier) WaitApplied(ctx context.Context, seq uint64) error {
	poll := a.Poll
	if poll <= 0 {
		poll = time.Second
	}
	for {
		local, wake := a.waitCh()
		if local >= seq {
			return nil
		}
		var applied uint64
		if err := a.Store.Tx(ctx, func(tx store.Tx) error {
			var err error
			applied, err = Applied(ctx, tx)
			return err
		}); err != nil {
			return err
		}
		if applied >= seq {
			return nil
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// Run follows the change log and applies the policy after each change,
// several at once when they come faster than an apply takes, until ctx
// ends. A failed apply is retried after retry. It is for the leader to
// run (the leader task decides who that is).
func (a *Applier) Run(ctx context.Context, retry time.Duration) error {
	var since uint64
	if err := a.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		since, err = Applied(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	changes, err := a.Store.Changes(ctx, since)
	if err != nil {
		return err
	}
	pending := true // apply once at the start, to repair drift
	for {
		if pending {
			if _, err := a.Apply(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(retry):
				}
				continue
			}
			pending = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-changes:
			if !ok {
				return ctx.Err()
			}
			pending = true
		drain:
			for {
				select {
				case _, ok := <-changes:
					if !ok {
						break drain
					}
				default:
					break drain
				}
			}
		}
	}
}
