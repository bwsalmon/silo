package fenceapi

import (
	"context"
	"encoding/json"
	"net/netip"
	"sort"

	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// CreateRequest is the body of create (fence-api.md, "create", SV-7).
type CreateRequest struct {
	Name           string            `json:"name"`
	Backing        *Backing          `json:"backing"`
	Regions        []string          `json:"regions"`
	Quota          QuotaMap          `json:"quota"`
	ModelBudgetUSD string            `json:"model_budget_usd"`
	Address        *AddressWant      `json:"address"`
	MaxBlocks      int               `json:"max_blocks"`
	Egress         *Egress           `json:"egress"`
	GrantPolicy    GrantPolicy       `json:"grant_policy"`
	Control        string            `json:"control"`
	AgentPolicy    *AgentPolicy      `json:"agent_policy"`
	Labels         map[string]string `json:"labels"`
}

// AddressWant asks for a block: a prefix length silod places, or an
// exact range.
type AddressWant struct {
	PrefixLength int    `json:"prefix_length"`
	Range        string `json:"range"`
}

func (a *AddressWant) want() (address.Want, error) {
	if a.Range != "" {
		p, err := netip.ParsePrefix(a.Range)
		if err != nil {
			return address.Want{}, errorf(InvalidArgument, "address range %q: %v", a.Range, err).with("field", "address")
		}
		if p != p.Masked() {
			return address.Want{}, errorf(InvalidArgument, "address range %s is not aligned: did you mean %s?", p, p.Masked()).with("field", "address")
		}
		return address.Want{Exact: p}, nil
	}
	if a.PrefixLength < 1 || a.PrefixLength > address.SmallestBlock {
		return address.Want{}, errorf(InvalidArgument, "address prefix_length %d: want 1 to %d", a.PrefixLength, address.SmallestBlock).with("field", "address")
	}
	return address.Want{Bits: a.PrefixLength}, nil
}

// lockChain locks chain's rows, root first (state.md, SV-46 item 2).
func lockChain(ctx context.Context, tx store.Tx, chain []*tree.Silo) error {
	keys := make([]string, len(chain))
	for i, s := range chain {
		keys[i] = s.ID
	}
	return tx.LockRows(ctx, "silos", "id", keys...)
}

// lastSeq is the change log's newest entry, which a mutation's own
// entries end at.
func lastSeq(ctx context.Context, tx store.Tx) (uint64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT seq FROM change_counter WHERE id = 1`).Scan(&n)
	return uint64(n), err
}

// noted marks the silo a mutation acts on, so a refusal past the point
// of resolving it is audited too.
type noted struct {
	silo *tree.Silo
	kind string
}

func (q *request) note(s *tree.Silo, kind string) {
	q.target = &noted{silo: s, kind: kind}
}

func mustAncestor(rel relation, what string) error {
	if rel == relSelf {
		return errorf(PermissionDenied, "only an ancestor may %s", what)
	}
	return nil
}

func (s *Server) create(q *request, parentRef string) (any, error) {
	ctx := q.r.Context()
	var req CreateRequest
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	var out *Silo
	var seq uint64
	err := s.tx(ctx, func(tx store.Tx) error {
		pchain, rel, err := s.resolve(ctx, tx, &q.caller, parentRef, "create")
		if err != nil {
			return err
		}
		parent := pchain[len(pchain)-1]
		q.note(parent, KindSiloCreate)
		if rel == relManage && req.Control != "" {
			return errorf(PermissionDenied, "a manage grant never carries control").with("field", "control")
		}
		if req.Backing != nil && req.Backing.Kind != "" && req.Backing.Kind != tree.BackingSubnet {
			if req.Backing.Kind == "vpc" || req.Backing.Kind == "project" {
				return errorf(Unimplemented, "backing %q is not supported in v1", req.Backing.Kind).with("backing", req.Backing.Kind)
			}
			return errorf(InvalidArgument, "backing %q: want subnet", req.Backing.Kind).with("field", "backing")
		}
		if !tree.ValidName(req.Name) {
			return errorf(InvalidArgument, "name %q is not a DNS label", req.Name).with("field", "name")
		}
		if req.Address == nil {
			return errorf(InvalidArgument, "address is required: prefix_length or range").with("field", "address")
		}
		want, err := req.Address.want()
		if err != nil {
			return err
		}
		if parent.Depth+1 > tree.MaxDepth {
			return errorf(ResourceExhausted, "%s is at the depth cap of %d", parent.Path, tree.MaxDepth).with("depth_cap", tree.MaxDepth)
		}
		if parent.State != tree.StateActive {
			return errorf(FailedPrecondition, "%s is %s", parent.Path, parent.State)
		}
		if err := lockChain(ctx, tx, pchain); err != nil {
			return err
		}
		peff, err := effectiveOf(ctx, tx, pchain, nil)
		if err != nil {
			return err
		}
		own := &policies{Egress: Egress{Allow: []string{}, Deny: []string{}}, Grant: GrantPolicy{}, Agent: peff.Agent, Control: peff.Control}
		for k, v := range peff.Grant {
			own.Grant[k] = v
		}
		if req.Egress != nil {
			own.Egress = normEgress(*req.Egress)
		}
		if req.GrantPolicy != nil {
			own.Grant = req.GrantPolicy
		}
		if req.AgentPolicy != nil {
			own.Agent = *req.AgentPolicy
		}
		if req.Control != "" {
			own.Control = req.Control
		}
		if err := checkPolicies(own); err != nil {
			return err
		}
		if err := looser(peff, own); err != nil {
			return err
		}
		quota, err := amounts(ctx, tx, req.Quota)
		if err != nil {
			return err
		}
		if req.ModelBudgetUSD != "" {
			m, err := ledger.ParseAmount(req.ModelBudgetUSD)
			if err != nil {
				return errorf(InvalidArgument, "model_budget_usd %q: want a non-negative decimal", req.ModelBudgetUSD).with("field", "model_budget_usd")
			}
			quota = append(quota, ledger.Amount{Key: ledger.ModelUSD, Amount: m})
		}

		child, err := tree.CreateChild(ctx, tx, parent.ID, req.Name, tree.BackingSubnet, req.Labels)
		if err != nil {
			return err
		}
		if q.caller.Agent != "" {
			// Made by an agent's session: deleted with it (call 43).
			if err := recordOwner(ctx, tx, "silo", child.ID, q.caller.Agent); err != nil {
				return err
			}
		}
		chain := append(append([]*tree.Silo{}, pchain...), child)
		regions := req.Regions
		if regions == nil {
			if regions, err = ledger.Regions(ctx, tx, parent.ID); err != nil {
				return err
			}
		}
		if err := ledger.SetRegions(ctx, tx, child.ID, regions); err != nil {
			return err
		}
		if err := writePolicies(ctx, tx, child.ID, own); err != nil {
			return err
		}
		if len(quota) > 0 {
			if err := ledger.Grant(ctx, tx, child.ID, quota); err != nil {
				return err
			}
		}
		if req.MaxBlocks != 0 {
			if err := address.SetBlockCap(ctx, tx, child.ID, req.MaxBlocks); err != nil {
				return err
			}
		}
		if _, err := address.Delegate(ctx, tx, child.ID, want); err != nil {
			return err
		}
		if _, err := tx.Append(ctx, "policy", child.ID, ""); err != nil {
			return err
		}
		if err := appendControlChange(ctx, tx, child, "", own.Control, nil); err != nil {
			return err
		}
		if err := s.checkBudget(ctx, tx); err != nil {
			return err
		}
		if out, err = s.object(ctx, tx, chain); err != nil {
			return err
		}
		via := viaOf(chain, q.caller)
		if err := Audit(ctx, tx, child, q.caller, KindSiloCreate, q.requestID, "OK", map[string]any{
			"parent":  parent.Path,
			"charges": grantLines(parent, child, via, quota, nil),
		}); err != nil {
			return err
		}
		if req.Control != "" {
			if err := Audit(ctx, tx, child, q.caller, KindSiloSetControl, q.requestID, "OK",
				map[string]any{"old": nil, "new": own.Control}); err != nil {
				return err
			}
		}
		if seq, err = lastSeq(ctx, tx); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, seq, err)
}

// checkBudget asks the fence's rule budget about the change tx has
// made so far.
func (s *Server) checkBudget(ctx context.Context, tx store.Tx) error {
	if s.opts.Budget == nil {
		return nil
	}
	return s.opts.Budget.CheckBudget(ctx, tx)
}

// done ends a mutation: a validate_only call's result, or the result
// once the fence carries the change.
func (s *Server) done(q *request, out any, seq uint64, err error) (any, error) {
	if err == errValidateOnly {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if seq > 0 {
		if err := s.opts.Fence.WaitApplied(q.r.Context(), seq); err != nil {
			return nil, errorf(Unavailable, "the change is committed, but the fence has not applied it: %v", err)
		}
	}
	return out, nil
}

// grantLines is what a grant change charges: the parent pays for the
// child, a lowering being a refund. old, if given, is each key's grant
// before.
func grantLines(parent, child *tree.Silo, via string, as []ledger.Amount, old map[ledger.Key]int64) []ChargeLine {
	out := []ChargeLine{}
	for _, a := range as {
		d := a.Amount - old[a.Key]
		if d == 0 {
			continue
		}
		out = append(out, ChargeLine{Payer: parent.Path, Home: child.Path, Via: via,
			Kind: a.Kind, Class: a.Class, Region: a.Region, Amount: jsonAmount(d)})
	}
	return out
}

func checkPolicies(p *policies) error {
	if err := checkEgress(p.Egress); err != nil {
		return err
	}
	if err := checkGrant(p.Grant); err != nil {
		return err
	}
	if err := checkAgent(p.Agent); err != nil {
		return err
	}
	return checkControl(p.Control)
}

// patch is PATCH on a silo (fence-api.md, "Changing allocations",
// SV-9).
func (s *Server) patch(q *request, ref string) (any, error) {
	ctx := q.r.Context()
	var body map[string]json.RawMessage
	if err := decode(q, &body); err != nil {
		return nil, err
	}
	has := func(k string) bool { _, ok := body[k]; return ok }
	var out *Silo
	var seq uint64
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "patch")
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindSiloPatch)
		if rel == relSelf {
			for _, f := range []string{"regions", "quota", "model_budget_usd", "max_blocks", "control", "labels"} {
				if has(f) {
					return errorf(PermissionDenied, "a silo can't change its own %s; only an ancestor can", f).with("field", f)
				}
			}
		}
		if rel == relManage && has("control") {
			return errorf(PermissionDenied, "a manage grant never carries control").with("field", "control")
		}
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		old, err := readPolicies(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		next, err := patchPolicies(old, body)
		if err != nil {
			return err
		}
		if err := checkPolicies(next); err != nil {
			return err
		}
		if rel == relSelf {
			if err := narrower(old, next); err != nil {
				return err
			}
		}
		controlSet := has("control") && string(body["control"]) != "null"
		if !t.IsRoot() {
			peff, err := effectiveOf(ctx, tx, chain[:len(chain)-1], nil)
			if err != nil {
				return err
			}
			// A null control goes back to the parent's effective
			// policy, as on create (call 54).
			if has("control") && !controlSet {
				next.Control = peff.Control
			}
			// A control left alone is not held to the parent's: an
			// ancestor tightening keeps the descendants' own settings,
			// so they come back when it loosens again.
			chk := *next
			if !controlSet {
				chk.Control = ControlNone
			}
			if err := looser(peff, &chk); err != nil {
				return err
			}
		} else if has("control") && !controlSet {
			next.Control = ControlFull
		} else if next.Control != ControlFull {
			return errorf(InvalidArgument, "the root's control policy is always full")
		}
		var controlBefore map[string]ControlEffect
		if next.Control != old.Control {
			if controlBefore, err = subtreeControl(ctx, tx, t.ID); err != nil {
				return err
			}
		}
		changed := map[string]any{}
		if t.IsRoot() {
			for _, f := range []string{"regions", "quota", "model_budget_usd", "max_blocks"} {
				if has(f) {
					return errorf(InvalidArgument, "the root's %s is set at install, not with PATCH", f).with("field", f)
				}
			}
		}
		var regions []string
		if has("regions") {
			if err := json.Unmarshal(body["regions"], &regions); err != nil {
				return errorf(InvalidArgument, "regions: %v", err).with("field", "regions")
			}
			cur, err := ledger.Regions(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			// Add first, so quota can be granted in a new region, and
			// remove last, once quota there has been lowered.
			if err := ledger.SetRegions(ctx, tx, t.ID, union(cur, regions)); err != nil {
				return err
			}
			changed["regions"] = regions
		}
		var quota []ledger.Amount
		if has("quota") {
			var m QuotaMap
			if err := json.Unmarshal(body["quota"], &m); err != nil {
				return errorf(InvalidArgument, "quota: %v", err).with("field", "quota")
			}
			if quota, err = amounts(ctx, tx, m); err != nil {
				return err
			}
		}
		if has("model_budget_usd") {
			var v string
			if err := json.Unmarshal(body["model_budget_usd"], &v); err != nil {
				return errorf(InvalidArgument, "model_budget_usd: want a decimal string").with("field", "model_budget_usd")
			}
			m, err := ledger.ParseAmount(v)
			if err != nil {
				return errorf(InvalidArgument, "model_budget_usd %q: want a non-negative decimal", v).with("field", "model_budget_usd")
			}
			quota = append(quota, ledger.Amount{Key: ledger.ModelUSD, Amount: m})
		}
		var lines []ChargeLine
		if len(quota) > 0 {
			oldGrant := map[ledger.Key]int64{}
			edges, err := ledger.Edges(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			for _, e := range edges {
				oldGrant[e.Key] = e.Amount
			}
			if err := ledger.Grant(ctx, tx, t.ID, quota); err != nil {
				return err
			}
			lines = grantLines(chain[len(chain)-2], t, viaOf(chain, q.caller), quota, oldGrant)
			changed["quota"] = true
		}
		if has("regions") {
			if err := ledger.SetRegions(ctx, tx, t.ID, regions); err != nil {
				return err
			}
		}
		if has("max_blocks") {
			var n int
			if err := json.Unmarshal(body["max_blocks"], &n); err != nil {
				return errorf(InvalidArgument, "max_blocks: want an integer").with("field", "max_blocks")
			}
			if err := address.SetBlockCap(ctx, tx, t.ID, n); err != nil {
				return err
			}
			changed["max_blocks"] = n
		}
		if has("labels") {
			var l map[string]string
			if err := json.Unmarshal(body["labels"], &l); err != nil {
				return errorf(InvalidArgument, "labels: %v", err).with("field", "labels")
			}
			if err := tree.SetLabels(ctx, tx, t.ID, l); err != nil {
				return err
			}
			changed["labels"] = l
		}
		policyChanged := false
		for _, f := range []string{"egress", "grant_policy", "agent_policy"} {
			if has(f) {
				policyChanged = true
				changed[f] = true
			}
		}
		if has("control") && next.Control != old.Control {
			policyChanged = true
		}
		if policyChanged {
			if err := writePolicies(ctx, tx, t.ID, next); err != nil {
				return err
			}
			if _, err := tx.Append(ctx, "policy", t.ID, ""); err != nil {
				return err
			}
			// A grant policy that moved suspends or resumes the grants it
			// covers.
			n, err := SyncGrants(ctx, tx, q.caller, q.requestID)
			if err != nil {
				return err
			}
			seq = max(seq, n)
		}
		if next.Control != old.Control {
			if err := appendControlChange(ctx, tx, t, old.Control, next.Control, controlBefore); err != nil {
				return err
			}
			// Loosening from none gives silos their n-api-out back.
			if old.Control == ControlNone {
				if err := s.checkBudget(ctx, tx); err != nil {
					return err
				}
			}
		}
		if t, err = tree.Get(ctx, tx, t.ID); err != nil {
			return err
		}
		chain[len(chain)-1] = t
		if out, err = s.object(ctx, tx, chain); err != nil {
			return err
		}
		if has("control") && next.Control != old.Control {
			if err := Audit(ctx, tx, t, q.caller, KindSiloSetControl, q.requestID, "OK",
				map[string]any{"old": old.Control, "new": next.Control}); err != nil {
				return err
			}
		}
		if len(changed) > 0 || !has("control") {
			f := map[string]any{"changed": sortedKeys(changed)}
			if lines != nil {
				f["charges"] = lines
			}
			if err := Audit(ctx, tx, t, q.caller, KindSiloPatch, q.requestID, "OK", f); err != nil {
				return err
			}
		}
		if seq, err = lastSeq(ctx, tx); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, seq, err)
}

func sortedKeys(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// patchPolicies applies a PATCH body's policy fields to old. egress
// replaces the lists it names; grant_policy is merged key by key, a
// null value removing the key (which is deny); agent_policy replaces
// the fields it names, a null one going back to inheriting.
func patchPolicies(old *policies, body map[string]json.RawMessage) (*policies, error) {
	next := &policies{
		Egress:  Egress{Allow: append([]string{}, old.Egress.Allow...), Deny: append([]string{}, old.Egress.Deny...)},
		Grant:   GrantPolicy{},
		Agent:   old.Agent,
		Control: old.Control,
	}
	for k, v := range old.Grant {
		next.Grant[k] = v
	}
	bad := func(f string, err error) error {
		return errorf(InvalidArgument, "%s: %v", f, err).with("field", f)
	}
	if raw, ok := body["egress"]; ok {
		var e map[string]*[]string
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, bad("egress", err)
		}
		if v, ok := e["allow"]; ok {
			next.Egress.Allow = []string{}
			if v != nil {
				next.Egress.Allow = *v
			}
		}
		if v, ok := e["deny"]; ok {
			next.Egress.Deny = []string{}
			if v != nil {
				next.Egress.Deny = *v
			}
		}
	}
	if raw, ok := body["grant_policy"]; ok {
		var g map[string]*string
		if err := json.Unmarshal(raw, &g); err != nil {
			return nil, bad("grant_policy", err)
		}
		for k, v := range g {
			if v == nil {
				delete(next.Grant, k)
			} else {
				next.Grant[k] = *v
			}
		}
	}
	if raw, ok := body["agent_policy"]; ok {
		var a map[string]json.RawMessage
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, bad("agent_policy", err)
		}
		for f, dst := range map[string]*[]string{"credentials": &next.Agent.Credentials, "adapters": &next.Agent.Adapters, "models": &next.Agent.Models} {
			v, ok := a[f]
			if !ok {
				continue
			}
			var l []string
			if string(v) != "null" {
				l = []string{}
				if err := json.Unmarshal(v, &l); err != nil {
					return nil, bad("agent_policy."+f, err)
				}
			}
			*dst = l
		}
		if v, ok := a["max_lease"]; ok {
			next.Agent.MaxLease = ""
			if string(v) != "null" {
				if err := json.Unmarshal(v, &next.Agent.MaxLease); err != nil {
					return nil, bad("agent_policy.max_lease", err)
				}
			}
		}
	}
	if raw, ok := body["control"]; ok {
		if err := json.Unmarshal(raw, &next.Control); err != nil {
			return nil, bad("control", err)
		}
	}
	return next, nil
}

// blockAction runs one of the address actions on a silo.
func (s *Server) blockAction(q *request, ref, kind string, selfMay bool, fn func(ctx context.Context, tx store.Tx, t *tree.Silo) (map[string]any, error)) (any, error) {
	ctx := q.r.Context()
	var out *Silo
	var seq uint64
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, Op(kind))
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, kind)
		if !selfMay {
			if err := mustAncestor(rel, "change a silo's address space"); err != nil {
				return err
			}
		}
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		fields, err := fn(ctx, tx, t)
		if err != nil {
			return err
		}
		if out, err = s.object(ctx, tx, chain); err != nil {
			return err
		}
		if err := Audit(ctx, tx, t, q.caller, kind, q.requestID, "OK", fields); err != nil {
			return err
		}
		if seq, err = lastSeq(ctx, tx); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, seq, err)
}

func (s *Server) grow(q *request, ref string) (any, error) {
	var req struct {
		Block          string `json:"block"`
		ToPrefixLength int    `json:"to_prefix_length"`
		AddBlock       bool   `json:"add_block"`
	}
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	var block netip.Prefix
	if req.Block != "" {
		var err error
		if block, err = netip.ParsePrefix(req.Block); err != nil {
			return nil, errorf(InvalidArgument, "block %q: %v", req.Block, err).with("field", "block")
		}
	}
	return s.blockAction(q, ref, KindSiloGrow, false, func(ctx context.Context, tx store.Tx, t *tree.Silo) (map[string]any, error) {
		g, err := address.Grow(ctx, tx, t.ID, block, req.ToPrefixLength, req.AddBlock)
		if err != nil {
			return nil, err
		}
		if err := s.checkBudget(ctx, tx); err != nil {
			return nil, err
		}
		return map[string]any{"block": req.Block, "result_block": g.Block.String(), "widened": g.Widened}, nil
	})
}

func (s *Server) addBlock(q *request, ref string) (any, error) {
	var req AddressWant
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	want, err := req.want()
	if err != nil {
		return nil, err
	}
	return s.blockAction(q, ref, KindSiloAddBlock, false, func(ctx context.Context, tx store.Tx, t *tree.Silo) (map[string]any, error) {
		p, err := address.Delegate(ctx, tx, t.ID, want)
		if err != nil {
			return nil, err
		}
		if err := s.checkBudget(ctx, tx); err != nil {
			return nil, err
		}
		return map[string]any{"block": p.String()}, nil
	})
}

func (s *Server) releaseBlock(q *request, ref string) (any, error) {
	var req struct {
		Block string `json:"block"`
	}
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	block, err := netip.ParsePrefix(req.Block)
	if err != nil {
		return nil, errorf(InvalidArgument, "block %q: %v", req.Block, err).with("field", "block")
	}
	return s.blockAction(q, ref, KindSiloReleaseBlock, true, func(ctx context.Context, tx store.Tx, t *tree.Silo) (map[string]any, error) {
		if err := address.ReleaseBlock(ctx, tx, t.ID, block); err != nil {
			return nil, err
		}
		return map[string]any{"block": block.String()}, nil
	})
}
