package fenceapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// ChangeKindControl is the change-log kind of a change to a silo's
// control policy. Its key is the silo set, and its data a
// ControlChange: what the gateway needs to close a tightened subtree's
// API-port connections, and the fence to drop or restore its API port.
const ChangeKindControl = "control"

// ControlChange is a "control" change-log entry's data.
type ControlChange struct {
	Silo string `json:"silo"`
	Path string `json:"path"`
	// Own is the silo's own setting, before and after; Old is "" for a
	// silo just created.
	Own ControlDelta `json:"own"`
	// Tightened is set when some silo's effective policy became
	// stricter.
	Tightened bool `json:"tightened"`
	// Silos are the silos, the set one and its descendants, whose
	// effective policy changed.
	Silos []ControlEffect `json:"silos"`
}

// ControlDelta is an old and a new policy.
type ControlDelta struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// ControlEffect is one silo's effective policy, before and after.
type ControlEffect struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	ControlDelta
	From string `json:"effective_from"`
}

// checkControl refuses a call the caller's own effective control
// policy forbids (fence-api.md, "Control policy", SV-12): under none,
// every call; under read, every call the table in pkg/control
// doesn't class as a read, unknown paths included. It binds whoever
// acts as the caller's silo, whatever the target. An ancestor or a
// manage holder acting on the silo is a caller of its own, and is held
// to its own policy. The refusal is audited as control.denied.
func (s *Server) checkControl(q *request, call control.FenceCall) error {
	ctx := q.r.Context()
	class := control.FenceClass(call.Op)
	var denied *Error
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		chain, err := tree.Ancestry(ctx, tx, q.caller.Silo)
		if errors.Is(err, tree.ErrNotFound) {
			return nil // the handler says NOT_FOUND
		}
		if err != nil {
			return err
		}
		eff, err := effectiveOf(ctx, tx, chain, nil)
		if err != nil {
			return err
		}
		self := chain[len(chain)-1]
		if control.Allows(eff.Control, class) || harnessKeeps(q.caller, call, self) {
			return nil
		}
		// An agent's harness keeps the control surface of its own silo's
		// agents (agents.md, "Agents in a sealed silo", SV-100). The
		// session socket isn't behind the fence, so this is the only
		// check.
		if q.caller.Via == ViaHarness && control.AgentSurface(call.Op) {
			own, err := ownSiloCall(ctx, tx, q.caller, call)
			if err != nil || own {
				return err
			}
		}
		denied = errorf(ControlDenied, "silo: control-plane calls from %s are refused (control=%s)", self.Path, eff.Control).
			with("policy", eff.Control).with("effective_from", eff.ControlFrom)
		op := call.Op
		if op == "" {
			op = q.r.Method + " " + q.r.URL.Path
		}
		return Audit(ctx, tx, self, q.caller, KindControlDenied, q.r.URL.Query().Get("request_id"), string(ControlDenied),
			map[string]any{"op": op, "class": class.String(), "policy": eff.Control, "effective_from": eff.ControlFrom})
	})
	if err != nil {
		return asError(ctx, nil, err)
	}
	if denied != nil {
		return denied
	}
	return nil
}

// harnessKeeps reports whether call is one an agent's harness keeps on
// its session socket whatever its silo's policy (fence-api.md, "Control
// policy", "Agents in a read or sealed silo"; gateway.md, SV-82): the
// control surface of its own silo's agents, that is their status and
// what is under each of them (sessions, messages, requests). Who may do
// which of those is still the handlers' to say (call 38).
func harnessKeeps(c Caller, call control.FenceCall, self *tree.Silo) bool {
	if c.Via != ViaHarness || c.Agent == "" || call.Collection != "agents" {
		return false
	}
	if call.Ref != "self" && call.Ref != "-"+self.ID && "/"+call.Ref != self.Path {
		return false
	}
	return call.Op == control.OpAgentsList || call.Op == control.OpAgentsGet || call.Extra
}

// subtreeControl is the effective control policy of id and each of its
// descendants, by id.
func subtreeControl(ctx context.Context, tx store.Tx, id string) (map[string]ControlEffect, error) {
	ids, err := tree.Subtree(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ControlEffect, len(ids))
	for _, sid := range ids {
		chain, err := tree.Ancestry(ctx, tx, sid)
		if err != nil {
			return nil, err
		}
		eff, err := effectiveOf(ctx, tx, chain, nil)
		if err != nil {
			return nil, err
		}
		out[sid] = ControlEffect{ID: sid, Path: chain[len(chain)-1].Path, ControlDelta: ControlDelta{New: eff.Control}, From: eff.ControlFrom}
	}
	return out, nil
}

// appendControlChange records, in the change log, that t's own control
// policy went from oldOwn to newOwn. before is the subtree's effective
// policies beforehand (nil for a silo just created); the entry lists
// every silo whose effective policy is now different.
func appendControlChange(ctx context.Context, tx store.Tx, t *tree.Silo, oldOwn, newOwn string, before map[string]ControlEffect) error {
	after, err := subtreeControl(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	ch := ControlChange{Silo: t.ID, Path: t.Path, Own: ControlDelta{Old: oldOwn, New: newOwn}, Silos: []ControlEffect{}}
	ids, err := tree.Subtree(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		a := after[id]
		old := before[id].New
		if old == a.New {
			continue
		}
		a.Old = old
		if old != "" && control.Stricter(a.New, old) {
			ch.Tightened = true
		}
		ch.Silos = append(ch.Silos, a)
	}
	b, err := json.Marshal(ch)
	if err != nil {
		return err
	}
	_, err = tx.Append(ctx, ChangeKindControl, t.ID, string(b))
	return err
}

// EffectiveControl is the effective control policy of the silo whose
// ancestry is chain (root first), and the path of the silo whose
// setting wins, for the compute/v1 facade, which holds its callers to
// the same policy.
func EffectiveControl(ctx context.Context, tx store.Tx, chain []*tree.Silo) (policy, from string, err error) {
	eff, err := effectiveOf(ctx, tx, chain, nil)
	if err != nil {
		return "", "", err
	}
	return eff.Control, eff.ControlFrom, nil
}

// EffectiveControls is every silo's effective control policy, by id,
// worked out down the tree as effectiveOf does: the root's is always
// full, and each other silo's is the stricter of its parent's and its
// own. The fence compiler reads it to leave out a sealed silo's
// n-api-out.
func EffectiveControls(ctx context.Context, tx store.Tx) (map[string]string, error) {
	silos, err := tree.All(ctx, tx)
	if err != nil {
		return nil, err
	}
	own := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT silo, control FROM silo_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, c string
		if err := rows.Scan(&id, &c); err != nil {
			return nil, err
		}
		own[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// tree.All is shallowest first, so a parent is worked out before its
	// children.
	out := make(map[string]string, len(silos))
	for _, s := range silos {
		if s.IsRoot() {
			out[s.ID] = ControlFull
			continue
		}
		eff, ok := out[s.Parent]
		if !ok {
			return nil, errors.New("fenceapi: silo " + s.ID + " has no parent " + s.Parent)
		}
		p, ok := own[s.ID]
		if !ok {
			p = ControlFull
		}
		if controlRank[p] < controlRank[eff] {
			eff = p
		}
		out[s.ID] = eff
	}
	return out, nil
}
