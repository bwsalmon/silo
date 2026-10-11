package gce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	gceapi "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
	compute "google.golang.org/api/compute/v1"
)

// Operation is a GCE operation the facade started, as silod records it
// (gcp-backend.md, "Operations", SV-73).
type Operation struct {
	Scope  string // zones/z, regions/r or global
	Name   string
	Silo   string // the silo whose facade it was started in
	Caller string // the silo that made the call: the operation's user
	Method string // compute.instances.insert
	// Object is what the call charged, for the refund: the ledger's
	// object key.
	Object string
	// Refund is what to give back if the operation fails; nil gives
	// back everything Object is charged, and noRefund nothing.
	Refund []ledger.Amount
	// Also are further objects the call charged in full, refunded with
	// Object if it fails: the new disks of an instance insert.
	Also []string
	// OnSuccess is what to refund from Object once the operation
	// succeeds: nil nothing, empty everything Object and Also hold (a
	// delete), or the amounts named (a stop's vCPU and memory).
	OnSuccess []ledger.Amount
	Done      bool
	Failed    bool
}

// started records op, which GCE started for q, and watches it until it
// finishes, refunding object's charges if it fails. It returns op as
// the tenant sees it. The write handlers call it once GCE has accepted
// the call.
func (s *Server) started(q *request, op *compute.Operation, object string, refund []ledger.Amount) (map[string]any, error) {
	return s.startedWith(q, op, Operation{Object: object, Refund: refund})
}

// startedWith is started with what the operation settles given in rec:
// its Object, Refund, Also and OnSuccess.
func (s *Server) startedWith(q *request, op *compute.Operation, rec Operation) (map[string]any, error) {
	rec.Scope, rec.Name, rec.Silo, rec.Caller = gceapi.ScopeOf(op).String(), op.Name, q.target.ID, q.caller.Silo
	rec.Method = "compute." + q.call.FullMethod()
	ctx := q.ctx()
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		if err := insertOp(ctx, tx, rec); err != nil {
			return err
		}
		return setReplica(ctx, tx, rec, s.opts.Replica)
	})
	if err != nil {
		return nil, err
	}
	s.watch(rec)
	return q.operation(rec, op)
}

func insertOp(ctx context.Context, tx store.Tx, rec Operation) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	refund, err := jsonOrEmpty(rec.Refund)
	if err != nil {
		return err
	}
	also, err := jsonOrEmpty(rec.Also)
	if err != nil {
		return err
	}
	onSuccess, err := jsonOrEmpty(rec.OnSuccess)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO facade_operations (scope, name, silo, caller, method, object, refund, also, on_success, done, failed, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Scope, rec.Name, rec.Silo, rec.Caller, rec.Method, rec.Object, refund, also, onSuccess, false, false, store.Millis(now), store.Millis(now))
	return err
}

// jsonOrEmpty is v as JSON, or "" for a nil slice.
func jsonOrEmpty[T any](v []T) (string, error) {
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

const opColumns = `scope, name, silo, caller, method, object, refund, also, on_success, done, failed`

func scanOp(row interface{ Scan(...any) error }) (Operation, error) {
	var r Operation
	var refund, also, onSuccess string
	if err := row.Scan(&r.Scope, &r.Name, &r.Silo, &r.Caller, &r.Method, &r.Object, &refund, &also, &onSuccess, &r.Done, &r.Failed); err != nil {
		return r, err
	}
	for _, f := range []struct {
		s    string
		into any
	}{{refund, &r.Refund}, {also, &r.Also}, {onSuccess, &r.OnSuccess}} {
		if f.s != "" {
			if err := json.Unmarshal([]byte(f.s), f.into); err != nil {
				return r, err
			}
		}
	}
	return r, nil
}

func readOps(ctx context.Context, tx store.Tx, where string, args ...any) ([]Operation, error) {
	rows, err := tx.Query(ctx, `SELECT `+opColumns+` FROM facade_operations WHERE `+where+` ORDER BY created_at, name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		r, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Operations lists the operations the facade started in silo's facade.
func Operations(ctx context.Context, tx store.Tx, silo string) ([]Operation, error) {
	return readOps(ctx, tx, `silo = ?`, silo)
}

// operation is op as the tenant sees it: rewritten like any other
// object, with user the silo that started it.
func (q *request) operation(rec Operation, op *compute.Operation) (map[string]any, error) {
	m, err := q.shape("compute#operation", op)
	if err != nil {
		return nil, err
	}
	m["user"] = "silo:" + rec.Caller
	return m, nil
}

// findOp is the call's operation, if it was started in the facade
// being called; anyone else's is 404, as GCE's own would be.
func (s *Server) findOp(q *request) (Operation, error) {
	ctx := q.ctx()
	var rec Operation
	found := false
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		rec, err = scanOp(tx.QueryRow(ctx, `SELECT `+opColumns+` FROM facade_operations WHERE scope = ? AND name = ? AND silo = ?`,
			q.call.Scope(), q.call.Name, q.target.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return rec, err
	}
	if !found {
		return rec, notFound("projects/" + q.call.Project + "/" + q.call.Scope() + "/operations/" + q.call.Name)
	}
	return rec, nil
}

func scopeOf(c call) gceapi.Scope { return gceapi.Scope{Zone: c.Zone, Region: c.Region} }

func (s *Server) getOperation(q *request) (any, error) {
	rec, err := s.findOp(q)
	if err != nil {
		return nil, err
	}
	op, err := s.gce.GetOperation(q.ctx(), s.opts.HostProject, scopeOf(q.call), rec.Name)
	if err != nil {
		return nil, err
	}
	return q.operation(rec, op)
}

func (s *Server) waitOperation(q *request) (any, error) {
	rec, err := s.findOp(q)
	if err != nil {
		return nil, err
	}
	op, err := s.gce.WaitOperation(q.ctx(), s.opts.HostProject, scopeOf(q.call), rec.Name)
	if err != nil {
		return nil, err
	}
	return q.operation(rec, op)
}

func (s *Server) listOperations(q *request) (any, error) {
	ctx := q.ctx()
	var recs []Operation
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		recs, err = readOps(ctx, tx, `silo = ? AND scope = ?`, q.target.ID, q.call.Scope())
		return err
	})
	if err != nil {
		return nil, err
	}
	mine := map[string]Operation{}
	for _, r := range recs {
		mine[r.Name] = r
	}
	ops, err := s.gce.ListOperations(ctx, s.opts.HostProject, scopeOf(q.call))
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	for _, op := range ops {
		rec, ok := mine[op.Name]
		if !ok {
			continue
		}
		m, err := q.operation(rec, op)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#operationList", items)
}

// ---- Watching ----

// Resume watches every operation the facade started that silod hasn't
// seen finish: those of a silod that stopped, or of another replica.
func (s *Server) Resume(ctx context.Context) error {
	var recs []Operation
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		recs, err = readOps(ctx, tx, `done = ?`, false)
		return err
	})
	if err != nil {
		return err
	}
	for _, r := range recs {
		s.watch(r)
	}
	return nil
}

// watch follows rec until GCE says it is DONE, then finishes it.
func (s *Server) watch(rec Operation) {
	key := rec.Scope + "/" + rec.Name
	if _, loaded := s.watching.LoadOrStore(key, true); loaded {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.watching.Delete(key)
		scope := parseScope(rec.Scope)
		backoff := 100 * time.Millisecond
		for s.bg.Err() == nil {
			op, err := s.gce.WaitOperation(s.bg, s.opts.HostProject, scope, rec.Name)
			switch {
			case err == nil && op.Status == "DONE":
				if err := s.finishOp(s.bg, rec, op); err != nil && s.bg.Err() == nil {
					log.Printf("facade: finishing operation %s: %v", key, err)
					break
				}
				return
			case gceapi.IsNotFound(err):
				// GCE has forgotten it: nothing to refund from.
				_ = s.finishOp(s.bg, rec, &compute.Operation{Name: rec.Name, Status: "DONE"})
				return
			case err != nil:
				if s.bg.Err() == nil {
					log.Printf("facade: watching operation %s: %v", key, err)
				}
			default:
				continue // not done yet: wait again
			}
			select {
			case <-s.bg.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 30*time.Second)
		}
	}()
}

// noRefund is the Refund of a call that charged nothing, though its
// Object is charged: a patch, or a delete, whose refund waits for
// success.
var noRefund = []ledger.Amount{}

// opDone finishes a method's own bookkeeping once its operation is
// done, after the charges are settled: a subnet's own ranges, a firewall
// rule's original, a delete's refund. failed is whether it failed. The
// write paths register theirs by method (compute.subnetworks.insert).
// It runs once, on the replica that saw the operation finish, outside
// the transaction that marked it done, so it may call GCE; a crash in
// between leaves the difference to the reconcile.
var opDone = map[string]func(s *Server, ctx context.Context, rec Operation, failed bool) error{}

// finishOp marks rec done and, if op failed, refunds what the call
// charged, once: only the first replica to see it done does. That
// replica then runs the method's opDone.
func (s *Server) finishOp(ctx context.Context, rec Operation, op *compute.Operation) error {
	failed := op.Error != nil && len(op.Error.Errors) > 0
	var cur Operation
	finished := false
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		finished = false
		var err error
		cur, err = scanOp(tx.QueryRow(ctx, `SELECT `+opColumns+` FROM facade_operations WHERE scope = ? AND name = ?`, rec.Scope, rec.Name))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.Done {
			return nil
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE facade_operations SET done = ?, failed = ?, updated_at = ? WHERE scope = ? AND name = ?`,
			true, failed, store.Millis(now), rec.Scope, rec.Name); err != nil {
			return err
		}
		finished = true
		if !failed {
			return settleSuccess(ctx, tx, cur)
		}
		refund := s.opts.Refund
		if refund == nil {
			refund = defaultRefund
		}
		if err := refund(ctx, tx, cur, op); err != nil {
			return err
		}
		t, err := tree.Get(ctx, tx, cur.Silo)
		if errors.Is(err, tree.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		code := ""
		if len(op.Error.Errors) > 0 {
			code = op.Error.Errors[0].Code
		}
		return fenceapi.Audit(ctx, tx, t, fenceapi.Caller{Silo: cur.Caller, Via: fenceapi.ViaInternal}, KindFacadeRefund, "", "OK",
			map[string]any{"method": cur.Method, "operation": cur.Name, "object": cur.Object, "error": code})
	})
	if err != nil || !finished {
		return err
	}
	if done := opDone[cur.Method]; done != nil {
		if err := done(s, ctx, cur, failed); err != nil {
			log.Printf("facade: finishing %s for operation %s: %v", cur.Method, cur.Name, err)
		}
	}
	return nil
}

// defaultRefund gives back what the failed call charged.
func defaultRefund(ctx context.Context, tx store.Tx, rec Operation, _ *compute.Operation) error {
	if rec.Object == "" || (rec.Refund != nil && len(rec.Refund) == 0) {
		return nil
	}
	held, err := ledger.Charges(ctx, tx, rec.Object)
	if err != nil {
		return err
	}
	if len(held) > 0 {
		if _, err := ledger.Refund(ctx, tx, rec.Object, rec.Refund); err != nil {
			return err
		}
	}
	return refundAll(ctx, tx, rec.Also)
}

// settleSuccess refunds what rec gives back once it succeeds.
func settleSuccess(ctx context.Context, tx store.Tx, rec Operation) error {
	switch {
	case rec.OnSuccess == nil:
		return nil
	case len(rec.OnSuccess) == 0:
		return refundAll(ctx, tx, append([]string{rec.Object}, rec.Also...))
	}
	_, err := ledger.Refund(ctx, tx, rec.Object, rec.OnSuccess)
	return err
}

// refundAll refunds everything each of objects holds.
func refundAll(ctx context.Context, tx store.Tx, objects []string) error {
	for _, o := range objects {
		if o == "" {
			continue
		}
		held, err := ledger.Charges(ctx, tx, o)
		if err != nil {
			return err
		}
		if len(held) == 0 {
			continue
		}
		if _, err := ledger.Refund(ctx, tx, o, nil); err != nil {
			return err
		}
	}
	return nil
}

// parseScope reads a scope written by gce.Scope.String.
func parseScope(s string) gceapi.Scope {
	kind, name, _ := strings.Cut(s, "/")
	switch kind {
	case "zones":
		return gceapi.Zonal(name)
	case "regions":
		return gceapi.Regional(name)
	}
	return gceapi.Global
}
