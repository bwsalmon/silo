package fenceapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/manage"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The audit kinds of manage grants (fence-api.md, "Audit" under
// "Manage grants", SV-22). Each is recorded under the target.
const (
	KindManageOffer  = "manage.offer"
	KindManageAccept = "manage.accept"
	KindManageRenew  = "manage.renew"
	KindManageRevoke = "manage.revoke"
	KindManageExpire = "manage.expire"
)

// ChangeKindManage is the change-log kind of a manage grant's change of
// state. Its key is the grant's id and its data a ManageChange. The
// gateway closes the holder's API-port connections on one whose state
// has ended (EXPIRED or REVOKED).
const ChangeKindManage = "manage"

// ManageChange is a "manage" change-log entry's data.
type ManageChange struct {
	Grant  string `json:"grant"`
	Name   string `json:"name"`
	Target string `json:"target"`
	Holder string `json:"holder"` // the holder's id, "" if there is no such silo
	State  string `json:"state"`
	Ended  bool   `json:"ended"`
}

func appendManageChange(ctx context.Context, tx store.Tx, g *manage.Grant) error {
	b, err := json.Marshal(ManageChange{Grant: g.ID, Name: g.Name, Target: g.Target, Holder: g.HolderID(), State: g.State,
		Ended: g.State == manage.StateExpired || g.State == manage.StateRevoked})
	if err != nil {
		return err
	}
	_, err = tx.Append(ctx, ChangeKindManage, g.ID, string(b))
	return err
}

// ManageGrants is the ManageChecker for the grants pkg/manage
// keeps; New uses it unless Options name another.
type ManageGrants struct{}

// manageNeed is what a fence operation needs of a grant: a fence group,
// and whether the grant's target itself is out of reach (below), as
// for the target's own allocations and deleting it. An op not listed
// is one a grant never carries: members, control, manage grants.
var manageNeed = map[Op]struct {
	group string
	below bool
}{
	"describe":               {manage.GroupRead, false},
	"children":               {manage.GroupRead, false},
	"audit":                  {manage.GroupRead, false},
	"preview":                {manage.GroupRead, false},
	opGrants:                 {manage.GroupGrants, false},
	"create":                 {manage.GroupChildren, false},
	"patch":                  {manage.GroupChildren, true},
	"delete":                 {manage.GroupChildren, true},
	Op(KindSiloGrow):         {manage.GroupBlocks, true},
	Op(KindSiloAddBlock):     {manage.GroupBlocks, true},
	Op(KindSiloReleaseBlock): {manage.GroupBlocks, false},
	// Agents in the target and its descendants, their status
	// included, which is the first of their control surface.
	opAgentsList:   {manage.GroupAgents, false},
	opAgentsGet:    {manage.GroupAgents, false},
	opAgentsCreate: {manage.GroupAgents, false},
	opAgentsPatch:  {manage.GroupAgents, false},
	opAgentsDelete: {manage.GroupAgents, false},
	opAgentsStop:   {manage.GroupAgents, false},
	opAgentsResume: {manage.GroupAgents, false},
	opAgentsRenew:  {manage.GroupAgents, false},
	// The rest of their control surface (agents.md, §8).
	opAgentsSession:  {manage.GroupAgents, false},
	opAgentsMessages: {manage.GroupAgents, false},
	opAgentsMessage:  {manage.GroupAgents, false},
	opAgentsRequests: {manage.GroupAgents, false},
	opAgentsAnswer:   {manage.GroupAgents, false},
}

// Covers implements ManageChecker.
func (ManageGrants) Covers(ctx context.Context, tx store.Tx, caller string, target *tree.Silo, op Op) (string, error) {
	need, known := manageNeed[op]
	var ok func(*manage.Grant) bool
	switch {
	case !known:
		need.group = manage.GroupRead
		ok = func(*manage.Grant) bool { return false }
	case need.below:
		ok = func(g *manage.Grant) bool { return g.Target != target.ID }
	}
	g, err := manage.AllowsWhere(ctx, tx, caller, target, need.group, manage.Write, ok)
	var d *manage.Denied
	if errors.As(err, &d) {
		var e *Error
		switch {
		case !known:
			e = errorf(PermissionDenied, "a manage grant never allows %s on %s", op, target.Path)
		case need.below && d.Grant.Covers(need.group, manage.Write):
			e = errorf(PermissionDenied, "the manage grant %q on %s allows %s below it, not on %s itself; that is its parent's", d.Grant.Name, d.Grant.TargetPath, op, target.Path)
		default:
			e = errorf(PermissionDenied, "%s", d.Error()).with("needs", need.group)
		}
		return "", grantDetails(e, d.Grant)
	}
	if err != nil || g == nil {
		return "", err
	}
	return g.ID, nil
}

func grantDetails(e *Error, g *manage.Grant) *Error {
	return e.with("grant", g.Name).with("grant_id", g.ID).with("target", g.TargetPath)
}

// sweepManage ends the grants whose lease has run out, in a
// transaction of its own, so a call that reads them sees them EXPIRED
// and the gateway hears of it.
func (s *Server) sweepManage(ctx context.Context) error {
	_, err := ExpireManageGrants(ctx, s.st)
	return err
}

// ExpireManageGrants ends every ACTIVE manage grant whose lease has run
// out by database time: EXPIRED, audited manage.expire under its
// target, and written to the change log, which is how the gateway
// learns to close the holder's connections. silod runs it on a timer;
// a call that relied on the grant was refused from the moment it ran
// out, whether or not this has run. It returns how many it ended.
func ExpireManageGrants(ctx context.Context, st store.Store) (int, error) {
	n := 0
	err := st.Tx(ctx, func(tx store.Tx) error {
		n = 0
		due, err := manage.Due(ctx, tx)
		if err != nil {
			return err
		}
		for _, g := range due {
			if ok, err := manage.End(ctx, tx, g, manage.StateExpired); err != nil || !ok {
				return err
			}
			if err := endedGrant(ctx, tx, g, Caller{Via: ViaInternal}, KindManageExpire, "", nil); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// endedGrant audits and logs a grant that has just ended.
func endedGrant(ctx context.Context, tx store.Tx, g *manage.Grant, c Caller, kind, requestID string, extra map[string]any) error {
	if err := appendManageChange(ctx, tx, g); err != nil {
		return err
	}
	t, err := tree.Get(ctx, tx, g.Target)
	if errors.Is(err, tree.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	fields := grantFields(g)
	for k, v := range extra {
		fields[k] = v
	}
	return Audit(ctx, tx, t, c, kind, requestID, "OK", fields)
}

func grantFields(g *manage.Grant) map[string]any {
	return map[string]any{"grant": g.ID, "name": g.Name, "holder": g.Holder, "state": g.State}
}

// endGrantsFor ends the grants on silo t and those it holds, as it is
// deleted (SV-21: deleting the target or the holder ends the grant),
// and drops their rows.
func endGrantsFor(ctx context.Context, tx store.Tx, t *tree.Silo, c Caller, requestID string) error {
	gs, err := manage.Touching(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	for _, g := range gs {
		ok, err := manage.End(ctx, tx, g, manage.StateRevoked)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		why := "target deleted"
		if g.Target != t.ID {
			why = "holder deleted"
		}
		if err := endedGrant(ctx, tx, g, c, KindManageRevoke, requestID, map[string]any{"reason": why}); err != nil {
			return err
		}
	}
	return manage.Forget(ctx, tx, t.ID)
}

// offerManage is POST .../manageGrants: a proper ancestor of the target
// offers a grant on it (SV-21). The control check, already made, is
// what holds the offerer to full.
func (s *Server) offerManage(q *request, ref string) (any, error) {
	ctx := q.r.Context()
	var req manage.Offer
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	var out *manage.Grant
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "manageGrants")
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindManageOffer)
		if rel != relAncestor {
			return errorf(PermissionDenied, "only a proper ancestor of %s may offer a manage grant on it", t.Path)
		}
		if out, err = manage.Create(ctx, tx, t, q.caller.Silo, ids.New(), req); err != nil {
			return err
		}
		f := grantFields(out)
		f["scope"], f["lease"] = out.Scope, out.Lease
		if err := Audit(ctx, tx, t, q.caller, KindManageOffer, q.requestID, "OK", f); err != nil {
			return err
		}
		if err := appendManageChange(ctx, tx, out); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

// revokeManage is DELETE .../manageGrants/{name}, by any proper
// ancestor of the target. Revoking an ended grant changes nothing.
func (s *Server) revokeManage(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var out *manage.Grant
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "manageGrants")
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindManageRevoke)
		if rel != relAncestor {
			return errorf(PermissionDenied, "only a proper ancestor of %s may revoke its manage grants", t.Path)
		}
		g, err := manage.ByName(ctx, tx, t.ID, name)
		if errors.Is(err, manage.ErrNotFound) {
			return errorf(NotFound, "%s has no manage grant %q", t.Path, name).with("name", name)
		}
		if err != nil {
			return err
		}
		ended, err := manage.End(ctx, tx, g, manage.StateRevoked)
		if err != nil {
			return err
		}
		if ended {
			if err := endedGrant(ctx, tx, g, q.caller, KindManageRevoke, q.requestID, nil); err != nil {
				return err
			}
		}
		if out, err = manage.Get(ctx, tx, g.ID); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

type grantList struct {
	Grants        []*manage.Grant `json:"manage_grants"`
	NextPageToken string          `json:"next_page_token,omitempty"`
}

func (s *Server) writeGrants(q *request, list func(ctx context.Context, tx store.Tx) ([]*manage.Grant, error)) {
	ctx := q.r.Context()
	offset, size, err := page(q)
	if err != nil {
		writeError(q.w, err)
		return
	}
	if err := s.sweepManage(ctx); err != nil {
		writeError(q.w, asError(ctx, nil, err))
		return
	}
	out := grantList{Grants: []*manage.Grant{}}
	err = s.tx(ctx, func(tx store.Tx) error {
		gs, err := list(ctx, tx)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		for i, g := range gs {
			if i < offset {
				continue
			}
			if len(out.Grants) == size {
				out.NextPageToken = nextToken(offset, size, true)
				break
			}
			out.Grants = append(out.Grants, g.Shown(now))
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// listManage is GET .../manageGrants, by an ancestor of the target:
// every grant on it, whatever its state.
func (s *Server) listManage(q *request, ref string) {
	s.writeGrants(q, func(ctx context.Context, tx store.Tx) ([]*manage.Grant, error) {
		chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "manageGrants")
		if err != nil {
			return nil, err
		}
		t := chain[len(chain)-1]
		if rel != relAncestor {
			return nil, errorf(PermissionDenied, "only an ancestor of %s may list its manage grants", t.Path)
		}
		return manage.OnTarget(ctx, tx, t.ID)
	})
}

// listHeld is GET /silo/v1/self/manageGrants: the grants offered to
// and held by the caller, each with its target's path.
func (s *Server) listHeld(q *request) {
	s.writeGrants(q, func(ctx context.Context, tx store.Tx) ([]*manage.Grant, error) {
		return manage.HeldBy(ctx, tx, q.caller.Silo)
	})
}

// holderAction runs accept, renew or give-up on one of the caller's
// grants. A grant that isn't the caller's is NOT_FOUND.
func (s *Server) holderAction(q *request, id, kind string, fn func(ctx context.Context, tx store.Tx, g *manage.Grant) (*manage.Grant, bool, error)) (any, error) {
	ctx := q.r.Context()
	if err := s.sweepManage(ctx); err != nil {
		return nil, err
	}
	var out *manage.Grant
	err := s.tx(ctx, func(tx store.Tx) error {
		g, err := manage.Get(ctx, tx, id)
		if errors.Is(err, manage.ErrNotFound) || (err == nil && (g.HolderID() == "" || g.HolderID() != q.caller.Silo)) {
			return errorf(NotFound, "no manage grant %s", id)
		}
		if err != nil {
			return err
		}
		t, err := tree.Get(ctx, tx, g.Target)
		if err != nil {
			return err
		}
		q.caller.Manage = g.ID
		q.note(t, kind)
		next, changed, err := fn(ctx, tx, g)
		if err != nil {
			return err
		}
		if changed {
			if kind == KindManageRevoke {
				if err := endedGrant(ctx, tx, next, q.caller, kind, q.requestID, map[string]any{"reason": "given up"}); err != nil {
					return err
				}
			} else {
				if err := Audit(ctx, tx, t, q.caller, kind, q.requestID, "OK", grantFields(next)); err != nil {
					return err
				}
				if err := appendManageChange(ctx, tx, next); err != nil {
					return err
				}
			}
		}
		if out, err = manage.Get(ctx, tx, g.ID); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

func (s *Server) acceptManage(q *request, id string) (any, error) {
	return s.holderAction(q, id, KindManageAccept, func(ctx context.Context, tx store.Tx, g *manage.Grant) (*manage.Grant, bool, error) {
		was := g.State
		next, err := manage.Accept(ctx, tx, g)
		return next, err == nil && was == manage.StatePending, err
	})
}

func (s *Server) renewManage(q *request, id string) (any, error) {
	return s.holderAction(q, id, KindManageRenew, func(ctx context.Context, tx store.Tx, g *manage.Grant) (*manage.Grant, bool, error) {
		next, err := manage.Renew(ctx, tx, g)
		return next, err == nil, err
	})
}

func (s *Server) giveUpManage(q *request, id string) (any, error) {
	return s.holderAction(q, id, KindManageRevoke, func(ctx context.Context, tx store.Tx, g *manage.Grant) (*manage.Grant, bool, error) {
		ended, err := manage.End(ctx, tx, g, manage.StateRevoked)
		return g, ended, err
	})
}
