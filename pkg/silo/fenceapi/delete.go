package fenceapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/ids"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// The states of an operation. A delete is DELETING until it is DONE,
// or DELETE_FAILED until a repeat call resumes it.
const (
	OpDeleting     = tree.StateDeleting
	OpDeleteFailed = tree.StateDeleteFailed
	OpDone         = "DONE"
)

// Operation is a long-running operation, at /silo/v1/operations/{id}.
type Operation struct {
	Name       string    `json:"name"`
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Target     string    `json:"target"`
	TargetID   string    `json:"target_id"`
	State      string    `json:"state"`
	Done       bool      `json:"done"`
	Error      *Error    `json:"error,omitempty"`
	CreateTime time.Time `json:"create_time"`
	UpdateTime time.Time `json:"update_time"`
}

type opRow struct {
	Operation
	caller Caller
}

func readOp(ctx context.Context, tx store.Tx, where string, args ...any) (*opRow, error) {
	var o opRow
	var caller, msg string
	var created, updated int64
	err := tx.QueryRow(ctx, `SELECT id, kind, silo, path, caller, state, done, error, created_at, updated_at
		FROM silo_operations WHERE `+where, args...).
		Scan(&o.ID, &o.Kind, &o.TargetID, &o.Target, &caller, &o.State, &o.Done, &msg, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.Name = "operations/" + o.ID
	o.CreateTime, o.UpdateTime = store.Time(created), store.Time(updated)
	if msg != "" {
		o.Error = &Error{}
		if err := json.Unmarshal([]byte(msg), o.Error); err != nil {
			o.Error = &Error{Code: Internal, Message: msg}
		}
	}
	if err := json.Unmarshal([]byte(caller), &o.caller); err != nil {
		return nil, err
	}
	return &o, nil
}

func setOp(ctx context.Context, tx store.Tx, id, state string, done bool, e *Error) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	msg := ""
	if e != nil {
		b, _ := json.Marshal(e)
		msg = string(b)
	}
	_, err = tx.Exec(ctx, `UPDATE silo_operations SET state = ?, done = ?, error = ?, updated_at = ? WHERE id = ?`,
		state, done, msg, store.Millis(now), id)
	return err
}

// delete is DELETE on a silo (fence-api.md, "delete", SV-8).
func (s *Server) delete(q *request, ref string) {
	force := q.r.URL.Query().Get("force") == "true"
	var opID string
	s.mutating(q, func(q *request) (any, error) {
		ctx := q.r.Context()
		var out any
		err := s.tx(ctx, func(tx store.Tx) error {
			chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "delete")
			if err != nil {
				return err
			}
			t := chain[len(chain)-1]
			if t.IsRoot() {
				return errorf(FailedPrecondition, "the root can't be deleted through the API; that is silo uninstall")
			}
			q.note(t, KindSiloDelete)
			if err := mustAncestor(rel, "delete a silo"); err != nil {
				return err
			}
			if err := lockChain(ctx, tx, chain); err != nil {
				return err
			}
			sub, err := tree.Subtree(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			if !force {
				if len(sub) > 1 {
					return errorf(FailedPrecondition, "%s has children; delete them first, or delete with force", t.Path).with("silo", t.Path)
				}
				var n int
				if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM charges WHERE payer = ? OR home = ?`, t.ID, t.ID).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					return errorf(FailedPrecondition, "%s still holds objects; delete them first, or delete with force", t.Path).with("silo", t.Path)
				}
			}
			if q.validateOnly {
				var paths []string
				for _, id := range sub {
					if d, err := tree.Get(ctx, tx, id); err == nil {
						paths = append(paths, d.Path)
					}
				}
				out = map[string]any{"validate_only": true, "deletes": paths}
				return errValidateOnly
			}
			op, err := s.startSiloDelete(ctx, tx, t, q.caller)
			if err != nil {
				return err
			}
			opID, out = op.ID, op.Operation
			return s.finish(ctx, tx, q, out)
		})
		if err == errValidateOnly {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		s.startDelete(opID)
		return out, nil
	}, true)
}

// startSiloDelete makes a silo and its subtree DELETING, with a delete
// operation: a new one, or the one a failed delete left, resumed.
func (s *Server) startSiloDelete(ctx context.Context, tx store.Tx, t *tree.Silo, c Caller) (*opRow, error) {
	sub, err := tree.Subtree(ctx, tx, t.ID)
	if err != nil {
		return nil, err
	}
	op, err := readOp(ctx, tx, `silo = ? AND kind = ? AND done = ?`, t.ID, "delete", false)
	if err != nil {
		return nil, err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	if op == nil {
		cj, _ := json.Marshal(c)
		id := ids.New() + ids.New()
		if _, err := tx.Exec(ctx, `INSERT INTO silo_operations (id, kind, silo, path, caller, state, done, error, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
			id, "delete", t.ID, t.Path, string(cj), OpDeleting, false, store.Millis(now), store.Millis(now)); err != nil {
			return nil, err
		}
	} else if err := setOp(ctx, tx, op.ID, OpDeleting, false, nil); err != nil {
		return nil, err
	}
	for _, id := range sub {
		if err := tree.SetState(ctx, tx, id, tree.StateDeleting); err != nil {
			return nil, err
		}
	}
	return readOp(ctx, tx, `silo = ? AND kind = ? AND done = ?`, t.ID, "delete", false)
}

// startDelete runs a delete operation in the background, unless it is
// already running.
func (s *Server) startDelete(opID string) {
	if _, busy := s.running.LoadOrStore(opID, true); busy {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.running.Delete(opID)
		s.runDelete(s.bg, opID)
	}()
}

// runDelete deletes an operation's subtree bottom-up, one silo per
// transaction, each after the Deleter has removed what it holds.
func (s *Server) runDelete(ctx context.Context, opID string) {
	fail := func(silo string, e *Error) {
		_ = s.st.Tx(ctx, func(tx store.Tx) error {
			if silo != "" {
				if err := tree.SetState(ctx, tx, silo, tree.StateDeleteFailed); err != nil && !errors.Is(err, tree.ErrNotFound) {
					return err
				}
			}
			return setOp(ctx, tx, opID, OpDeleteFailed, false, e)
		})
	}
	for ctx.Err() == nil {
		var op *opRow
		var leaf *tree.Silo
		err := s.st.Tx(ctx, func(tx store.Tx) error {
			var err error
			if op, err = readOp(ctx, tx, `id = ?`, opID); err != nil || op == nil {
				return err
			}
			sub, err := tree.Subtree(ctx, tx, op.TargetID)
			if err != nil || len(sub) == 0 {
				return err
			}
			// The last of a breadth-first walk is a leaf.
			leaf, err = tree.Get(ctx, tx, sub[len(sub)-1])
			if err != nil {
				return err
			}
			kids, err := tree.Children(ctx, tx, leaf.ID)
			for err == nil && len(kids) > 0 {
				leaf = kids[len(kids)-1]
				kids, err = tree.Children(ctx, tx, leaf.ID)
			}
			return err
		})
		if err != nil {
			fail("", asError(ctx, nil, err))
			return
		}
		if op == nil {
			return
		}
		if leaf == nil {
			_ = s.st.Tx(ctx, func(tx store.Tx) error { return setOp(ctx, tx, opID, OpDone, true, nil) })
			return
		}
		// Its agents go first, tool VMs and all.
		if err := s.deleteAgentsIn(ctx, leaf.ID, op.caller); err != nil {
			fail(leaf.ID, asError(ctx, nil, err).with("silo", leaf.Path))
			return
		}
		if s.opts.Deleter != nil {
			if err := s.opts.Deleter.DeleteContents(ctx, leaf); err != nil {
				fail(leaf.ID, errorf(FailedPrecondition, "deleting what %s holds: %v", leaf.Path, err).with("silo", leaf.Path))
				return
			}
		}
		var seq uint64
		err = s.tx(ctx, func(tx store.Tx) error {
			chain, err := tree.Ancestry(ctx, tx, leaf.ID)
			if err != nil {
				return err
			}
			if err := lockChain(ctx, tx, chain); err != nil {
				return err
			}
			if err := s.deleteOne(ctx, tx, leaf, op.caller, ""); err != nil {
				return err
			}
			seq, err = lastSeq(ctx, tx)
			return err
		})
		if err != nil {
			e := asError(ctx, nil, err)
			fail(leaf.ID, e.with("silo", leaf.Path))
			return
		}
		if err := s.opts.Fence.WaitApplied(ctx, seq); err != nil {
			fail("", errorf(Unavailable, "the fence has not applied the deletion of %s: %v", leaf.Path, err))
			return
		}
	}
}

// deleteOne deletes a silo with no children: returns its quota and
// blocks to its parent, drops its policies and members, removes its
// row, and audits it.
func (s *Server) deleteOne(ctx context.Context, tx store.Tx, t *tree.Silo, c Caller, requestID string) error {
	chain, err := tree.Ancestry(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	parent := chain[len(chain)-2]
	edges, err := ledger.Edges(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	var refunds []ChargeLine
	via := viaOf(chain, c)
	for _, e := range edges {
		refunds = append(refunds, ChargeLine{Payer: parent.Path, Home: t.Path, Via: via,
			Kind: e.Kind, Class: e.Class, Region: e.Region, Amount: jsonAmount(-e.Amount)})
	}
	blocks, err := address.Blocks(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	if err := address.ReleaseSilo(ctx, tx, t.ID); err != nil {
		return err
	}
	if err := ledger.ReleaseSilo(ctx, tx, t.ID); err != nil {
		return err
	}
	// The record is written while the silo's row is there to name it.
	var bs []string
	for _, b := range blocks {
		bs = append(bs, b.String())
	}
	if err := Audit(ctx, tx, t, c, KindSiloDelete, requestID, "OK", map[string]any{"blocks": bs, "charges": refunds}); err != nil {
		return err
	}
	if err := endGrantsFor(ctx, tx, t, c, requestID); err != nil {
		return err
	}
	if err := endExportsFor(ctx, tx, t, c, requestID); err != nil {
		return err
	}
	if err := endLinksFor(ctx, tx, t, c, requestID); err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM silo_policies WHERE silo = ?`, `DELETE FROM silo_members WHERE silo = ?`,
		`DELETE FROM agent_owned WHERE kind = 'silo' AND object = ?`} {
		if _, err := tx.Exec(ctx, q, t.ID); err != nil {
			return err
		}
	}
	return tree.Delete(ctx, tx, t.ID)
}

func (s *Server) getOperation(q *request, id string) {
	ctx := q.r.Context()
	var out *Operation
	err := s.tx(ctx, func(tx store.Tx) error {
		op, err := readOp(ctx, tx, `id = ?`, id)
		if err != nil {
			return err
		}
		if op == nil {
			return errorf(NotFound, "no operation %s", id)
		}
		// The caller that started it, or a silo whose subtree held
		// its target, may read it.
		me, err := tree.Get(ctx, tx, q.caller.Silo)
		if err != nil && !errors.Is(err, tree.ErrNotFound) {
			return err
		}
		if op.caller.Silo != q.caller.Silo && (me == nil || !(me.IsRoot() || strings.HasPrefix(op.Target+"/", me.Path+"/"))) {
			return errorf(NotFound, "no operation %s", id)
		}
		out = &op.Operation
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// BootstrapRequest creates the root silo (install.md, step 13). It is
// accepted only while there is no root.
type BootstrapRequest struct {
	Name           string            `json:"name"`
	AddressSpace   string            `json:"address_space"`
	GatewayRange   string            `json:"gateway_range"`
	ControlRange   string            `json:"control_range"`
	Regions        []string          `json:"regions"`
	Capacity       QuotaMap          `json:"capacity"`
	ModelBudgetUSD string            `json:"model_budget_usd"`
	BudgetPeriod   string            `json:"budget_period"`
	Egress         *Egress           `json:"egress"`
	GrantPolicy    GrantPolicy       `json:"grant_policy"`
	AgentPolicy    *AgentPolicy      `json:"agent_policy"`
	Admins         []string          `json:"admins"`
	Labels         map[string]string `json:"labels"`
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errorf(NotFound, "bootstrap is POST"))
		return
	}
	var req BootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errorf(InvalidArgument, "bad request body: %v", err))
		return
	}
	ctx := r.Context()
	prefix := func(v string, def netip.Prefix, field string) (netip.Prefix, error) {
		if v == "" {
			return def, nil
		}
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return p, errorf(InvalidArgument, "%s %q: %v", field, v, err).with("field", field)
		}
		return p, nil
	}
	var out *Silo
	err := s.tx(ctx, func(tx store.Tx) error {
		if _, err := tree.Root(ctx, tx); err == nil {
			return errorf(AlreadyExists, "the root silo exists; bootstrap is accepted only before it does")
		} else if !errors.Is(err, tree.ErrNotFound) {
			return err
		}
		space, err := prefix(req.AddressSpace, address.DefaultSpace, "address_space")
		if err != nil {
			return err
		}
		gw, err := prefix(req.GatewayRange, address.DefaultGateway, "gateway_range")
		if err != nil {
			return err
		}
		ctl, err := prefix(req.ControlRange, address.DefaultControl, "control_range")
		if err != nil {
			return err
		}
		name := req.Name
		if name == "" {
			name = "root"
		}
		root, err := tree.CreateRoot(ctx, tx, name, req.Labels)
		if err != nil {
			return err
		}
		if err := address.InitRoot(ctx, tx, root.ID, space, gw, ctl); err != nil {
			return err
		}
		if err := ledger.SetRegions(ctx, tx, root.ID, req.Regions); err != nil {
			return err
		}
		if req.BudgetPeriod != "" {
			if err := ledger.SetBudgetPeriod(ctx, tx, req.BudgetPeriod); err != nil {
				return err
			}
		}
		// A GPU family install read that silod's GPU config doesn't name
		// yet has no class to hold it: the leader's first root capacity
		// read discovers the family and gives it one (rootcap).
		for _, regions := range req.Capacity {
			for class := range regions {
				if !strings.HasSuffix(class, "_gpus") {
					continue
				}
				if _, err := ledger.LookupClass(ctx, tx, "gce", class); ledger.CodeOf(err) == ledger.InvalidArgument {
					delete(regions, class)
				}
			}
		}
		capacity, err := amounts(ctx, tx, req.Capacity)
		if err != nil {
			return err
		}
		if req.ModelBudgetUSD != "" {
			m, err := ledger.ParseAmount(req.ModelBudgetUSD)
			if err != nil {
				return err
			}
			capacity = append(capacity, ledger.Amount{Key: ledger.ModelUSD, Amount: m})
		}
		if err := ledger.SetCapacity(ctx, tx, root.ID, capacity); err != nil {
			return err
		}
		// The root's egress is install's --egress-allow, and empty, which
		// allows nothing, by default (gateway.md, "Policy", SV-86).
		p := &policies{Egress: Egress{Allow: []string{}, Deny: []string{}}, Grant: GrantPolicy{}, Control: ControlFull}
		for k, v := range RootGrantPolicy {
			p.Grant[k] = v
		}
		if req.Egress != nil {
			p.Egress = normEgress(*req.Egress)
		}
		if req.GrantPolicy != nil {
			p.Grant = req.GrantPolicy
		}
		if req.AgentPolicy != nil {
			p.Agent = *req.AgentPolicy
		}
		if err := checkPolicies(p); err != nil {
			return err
		}
		if err := writePolicies(ctx, tx, root.ID, p); err != nil {
			return err
		}
		if _, err := tx.Append(ctx, "policy", root.ID, ""); err != nil {
			return err
		}
		// The root is the actor; the installer's identity, when the
		// call came through the front door, is the member.
		c := Caller{Silo: root.ID, Via: ViaInternal}
		if fc, ok := CallerFromContext(ctx); ok && fc.Via == ViaFrontDoor {
			c.Member = fc.Member
		}
		for _, a := range req.Admins {
			if err := setMember(ctx, tx, a, root); err != nil {
				return err
			}
			if err := Audit(ctx, tx, root, c, KindMemberPut, "", "OK", map[string]any{"identity": a}); err != nil {
				return err
			}
		}
		if out, err = s.object(ctx, tx, []*tree.Silo{root}); err != nil {
			return err
		}
		return Audit(ctx, tx, root, c, KindSiloCreate, "", "OK", map[string]any{"bootstrap": true})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
