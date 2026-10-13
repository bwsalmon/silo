// Package facade is the request pipeline every per-silo facade shares
// (docs/silo/facades.md, "The pipeline every facade shares";
// local-backend.md, "Extracting the facade core"):
//
//  1. Resolve: the silo a call names, and whether the caller reaches it,
//     as itself, as an ancestor or through a manage grant;
//  2. CheckControl: the caller's own control policy, by pkg/control's
//     classification of the method;
//  3. Table: the method table's lookup, with resource types that are
//     unknown told apart from those whose every method is refused;
//  5. Fields: request and response allowlists over JSON, and Silo's
//     reserved labels;
//  7. Audit: the facade.call record of a write.
//
// Location (step 4), the handlers (step 6), naming, paths and error
// shapes stay with each service: the core's refusals are Refusal
// values, which each facade answers in its own service's shape. The
// GCE facade, pkg/facade/gcp/gce, is its user.
package facade

import (
	"context"
	"errors"
	"fmt"

	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/manage"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// A Refusal is the core's own refusal of a call. Each facade answers
// one in its service's error shape; any other error the core returns is
// the store's.
type Refusal interface {
	error
	refusal()
}

// Unreachable refuses a target the caller can't reach, the same whether
// or not the silo exists.
type Unreachable struct{}

func (*Unreachable) Error() string { return "silo: the target is not reachable" }
func (*Unreachable) refusal()      {}

// GrantDenied refuses a call a manage grant the caller holds doesn't
// cover.
type GrantDenied struct {
	Grant  string // the grant's id
	Reason string // why it doesn't cover the call
}

func (e *GrantDenied) Error() string { return fmt.Sprintf("silo: %s (grant %s)", e.Reason, e.Grant) }
func (*GrantDenied) refusal()        {}

// ControlDenied refuses a call the caller's own control policy doesn't
// allow.
type ControlDenied struct {
	Path   string // the caller's silo path
	Policy string // its effective control policy
}

func (e *ControlDenied) Error() string {
	return fmt.Sprintf("silo: control-plane calls from %s are refused (control=%s)", e.Path, e.Policy)
}
func (*ControlDenied) refusal() {}

// ManageChecker is the hook for manage grants (fence-api.md, "What a
// holder may do"): asked when the caller is neither the target silo nor
// one of its ancestors. It returns the id of an active grant the caller
// holds on the target or one of its ancestors that covers resource (and
// a write, if write is set); "" if the caller has none at all; or a
// *GrantDenied when one exists but doesn't cover the call.
// ManageGrants is the one silod uses.
type ManageChecker interface {
	Covers(ctx context.Context, tx store.Tx, caller string, target *tree.Silo, resource string, write bool) (grant string, err error)
}

// ManageGrants is the ManageChecker for the grants pkg/manage keeps
// (fence-api.md, "What a holder may do", SV-18). A call is charged to
// the resource type the facade names it by, so a GCE instances.insert,
// boot disk and all, is covered by instances.
type ManageGrants struct{}

// Covers implements ManageChecker.
func (ManageGrants) Covers(ctx context.Context, tx store.Tx, caller string, target *tree.Silo, resource string, write bool) (string, error) {
	access := manage.Read
	if write {
		access = manage.Write
	}
	g, err := manage.Allows(ctx, tx, caller, target, resource, access)
	var d *manage.Denied
	if errors.As(err, &d) {
		return "", &GrantDenied{Grant: d.Grant.ID, Reason: d.Error()}
	}
	if err != nil || g == nil {
		return "", err
	}
	return g.ID, nil
}

// Ref is how a call names its target, once the service has parsed it:
// the caller's own silo, or a silo by id. A Ref naming neither is
// unreachable.
type Ref struct {
	Self bool   // the caller's own silo
	Silo string // a silo's id, when not Self
	// Resource is what a manage grant has to cover, and Write whether
	// the call is a write.
	Resource string
	Write    bool
}

// Target is the silo a call reaches, and how.
type Target struct {
	// Silo is the target, and Chain its ancestry, root first.
	Silo  *tree.Silo
	Chain []*tree.Silo
	// Caller is the caller, with Manage set to the grant it reached the
	// target through, if any; CallerChain is its silo's ancestry.
	Caller      fenceapi.Caller
	CallerChain []*tree.Silo
}

// Via is how the caller reached the target, as a charge records it.
func (t *Target) Via() string { return Via(t.Caller, t.Silo.ID) }

// Via is how caller reached the silo target: through a manage grant,
// as an ancestor, or as itself.
func Via(caller fenceapi.Caller, target string) string {
	switch {
	case caller.Manage != "":
		return "manage:" + caller.Silo
	case caller.Silo != target:
		return "ancestor:" + caller.Silo
	}
	return ledger.ViaFacade
}

// Resolve finds the silo ref names, inside tx, and checks caller may
// reach it: its own silo, its descendants, and through mc (which may be
// nil) the targets of manage grants it holds. A caller or target that
// doesn't exist, or that the caller can't reach, is *Unreachable; a
// grant that doesn't cover the call is *GrantDenied.
func Resolve(ctx context.Context, tx store.Tx, mc ManageChecker, caller fenceapi.Caller, ref Ref) (*Target, error) {
	callerChain, err := tree.Ancestry(ctx, tx, caller.Silo)
	if errors.Is(err, tree.ErrNotFound) {
		return nil, &Unreachable{}
	}
	if err != nil {
		return nil, err
	}
	var t *tree.Silo
	switch {
	case ref.Self:
		t = callerChain[len(callerChain)-1]
	case ref.Silo != "":
		t, err = tree.Get(ctx, tx, ref.Silo)
		if errors.Is(err, tree.ErrNotFound) {
			return nil, &Unreachable{}
		}
		if err != nil {
			return nil, err
		}
	default:
		return nil, &Unreachable{}
	}
	chain, err := tree.Ancestry(ctx, tx, t.ID)
	if err != nil {
		return nil, err
	}
	reach := t.ID == caller.Silo
	for _, a := range chain[:len(chain)-1] {
		if a.ID == caller.Silo {
			reach = true
		}
	}
	if !reach && mc != nil {
		g, err := mc.Covers(ctx, tx, caller.Silo, t, ref.Resource, ref.Write)
		if err != nil {
			return nil, err
		}
		if g != "" {
			reach = true
			caller.Manage = g
		}
	}
	if !reach {
		return nil, &Unreachable{}
	}
	return &Target{Silo: t, Chain: chain, Caller: caller, CallerChain: callerChain}, nil
}

// CheckControl holds the caller to its own effective control policy,
// inside tx, for a call of class. A refusal is audited in the caller's
// own silo as control.denied, with op naming the call, and returned as
// *ControlDenied; tx has to commit for the record to stay.
func CheckControl(ctx context.Context, tx store.Tx, t *Target, class control.Class, op, requestID string) error {
	policy, from, err := fenceapi.EffectiveControl(ctx, tx, t.CallerChain)
	if err != nil {
		return err
	}
	if control.Allows(policy, class) {
		return nil
	}
	self := t.CallerChain[len(t.CallerChain)-1]
	if err := fenceapi.Audit(ctx, tx, self, t.Caller, fenceapi.KindControlDenied, requestID, string(fenceapi.ControlDenied),
		map[string]any{"op": op, "class": class.String(), "policy": policy, "effective_from": from}); err != nil {
		return err
	}
	return &ControlDenied{Path: self.Path, Policy: policy}
}
