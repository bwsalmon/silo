package install

import (
	"context"
	"time"
)

// Observed is what a step's Observe read: a pointer to the step's own
// struct, which marshals to JSON so that tests can plan against recorded
// observations (SV-97).
type Observed any

// Step is one of install's reconcilers (install.md "Approach", call 18).
type Step interface {
	Name() string
	// Observe reads the cloud. It never changes anything.
	Observe(ctx context.Context) (Observed, error)
	// Plan says what Apply would do about what Observe saw. It is pure:
	// it reads only its argument and the step's configuration.
	Plan(Observed) []Change
	// Apply makes the changes Plan returned. It is safe to repeat: a
	// change already made, by this run or an interrupted one, is not an
	// error.
	Apply(ctx context.Context, changes []Change) error
	// NewObserved is a pointer to the step's zero Observed, for decoding
	// a recorded one.
	NewObserved() Observed
}

// Remover is a step that uninstall undoes. PlanRemove is pure, like
// Plan, and its changes are applied with Apply.
type Remover interface {
	Step
	PlanRemove(Observed) []Change
}

// Action is what a Change does.
type Action string

const (
	// ActionOK is an object that exists and matches, or a check that
	// passed.
	ActionOK Action = "ok"
	// ActionCreate, ActionEnable and ActionApply make something new.
	ActionCreate Action = "create"
	ActionEnable Action = "enable"
	ActionApply  Action = "apply"
	// ActionUpdate changes a field install owns on an existing object.
	ActionUpdate Action = "update"
	// ActionDelete is uninstall's.
	ActionDelete Action = "delete"
	// ActionKeep is something uninstall deliberately leaves.
	ActionKeep Action = "keep"
	// ActionWarn is a check that passed with a warning.
	ActionWarn Action = "warn"
	// ActionDefer is a check that can't run until a later step has
	// enabled an API; the run checks it again after each later step.
	ActionDefer Action = "defer"
	// ActionFail is a check that failed, or an object install can't
	// reconcile. Install stops at the step.
	ActionFail Action = "fail"
)

// Changes reports whether a is one Apply has to act on.
func (a Action) Changes() bool {
	switch a {
	case ActionCreate, ActionEnable, ActionApply, ActionUpdate, ActionDelete:
		return true
	}
	return false
}

// Change is one line of a step's plan.
type Change struct {
	Action Action `json:"action"`
	// Object is what the change is to: a name, a service, a check.
	Object string `json:"object,omitempty"`
	// Detail says more: the object's settings, or what a check found.
	Detail string `json:"detail,omitempty"`
	// Why and Fix are a failed or warning check's: why install needs
	// it, and the command or page that fixes it.
	Why string `json:"why,omitempty"`
	Fix string `json:"fix,omitempty"`
	// Estimate is how long Apply will take over it.
	Estimate time.Duration `json:"-"`
	// Waiting names what most of Estimate is spent waiting for, as
	// "Cloud SQL".
	Waiting string `json:"-"`
}

// StepPlan is one step's plan: its changes, or why it couldn't be
// observed.
type StepPlan struct {
	Step    string
	Changes []Change
	Err     error
}

// Action is the step's own: fail if anything fails, else its first
// change's action that changes something, else ok.
func (p StepPlan) Action() Action {
	if p.Err != nil {
		return ActionFail
	}
	var first Action
	for _, c := range p.Changes {
		if c.Action == ActionFail {
			return ActionFail
		}
		if first == "" && c.Action.Changes() {
			first = c.Action
		}
	}
	if first != "" {
		return first
	}
	for _, c := range p.Changes {
		if c.Action == ActionKeep {
			return ActionKeep
		}
	}
	return ActionOK
}

// Failed reports whether the step can't go ahead.
func (p StepPlan) Failed() bool { return p.Action() == ActionFail }

// Changing reports whether the step has anything for Apply.
func (p StepPlan) Changing() bool {
	for _, c := range p.Changes {
		if c.Action.Changes() {
			return true
		}
	}
	return false
}

// Deferred reports whether some of the step's checks are waiting for a
// later step.
func (p StepPlan) Deferred() bool {
	for _, c := range p.Changes {
		if c.Action == ActionDefer {
			return true
		}
	}
	return false
}
