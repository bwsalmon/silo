// Package reconcile is the leader's reconcile (docs/silo/gcp-backend.md,
// "Reconcile", SV-77): silod's database is the truth for the tree, the
// allocations, the grants and the fence, and GCE is the truth for the
// tenants' objects. It reaches the backend only through package
// backend's Inventory and Fence (local-backend.md, call L14). A Run:
//
//   - lists every object in the backend's allowed regions, and the
//     global ones (backend.Inventory, which sorts each by its silo
//     label or <id>- prefix on GCP);
//   - audits an orphan, an object with the label or prefix of a silo
//     that doesn't exist, as orphan.found the first time it sees it,
//     and deletes it, as orphan.deleted, once it has been seen for the
//     grace period of database time (call 27), unless reconcile is
//     report-only (state.md "Backups and recovery", call 36);
//   - audits the same way an object of a live silo that the store has
//     no row for (backend.Object.Unrecorded): on the local backend, one
//     made after the point a restored store was copied at;
//   - reports a foreign object, one in the VPC with neither label nor
//     prefix and not install's own, as foreign.found, and leaves it
//     alone;
//   - drops the charges of an object that doesn't exist and that no
//     operation is pending for: a call charged for, whose silod died
//     before it reached GCE. Then it recounts each silo's used from
//     the charges it pays, grouped by payer (ledger.Recount);
//   - compiles the fence again and applies the difference. A policy
//     that had drifted from what was recorded as applied is repaired
//     and audited as fence_drift (AuditDrift, which silod makes the
//     applier's OnDrift, so drift any apply finds is audited);
//   - compares the registry's manifest rows with its silo-registry
//     records (Options.Registry, local-registry.md §10): registry_drift,
//     image_manifest_missing, and records newer than a restored store,
//     reported as orphans and never deleted.
//
// Every write it makes calls leader.Guard: only the current leader's
// reconcile writes anything.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend"
	"github.com/bwsalmon/silo/pkg/backend/gcp/gce/gcefence"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/leader"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/registry"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The audit kinds the reconcile writes.
const (
	KindOrphanFound   = "orphan.found"
	KindOrphanDeleted = "orphan.deleted"
	KindForeignFound  = "foreign.found"
	KindFenceDrift    = "fence_drift"
	KindRecount       = "quota.recount"
	KindChargeDropped = "quota.charge_dropped"
)

// Every is how often the leader reconciles, besides once on taking the
// lease.
const Every = 10 * time.Minute

// Grace is how long an orphan is reported before it is deleted (call
// 27), so that a half-finished delete can complete first.
const Grace = time.Hour

// ChargeGrace is how old a charge must be, in database time, before the
// reconcile drops it for an object that doesn't exist. A facade call
// charges, commits and then calls GCE, all within seconds; one older
// than this, with no operation pending, never reached GCE.
const ChargeGrace = 10 * time.Minute

// Options configure a Reconciler.
type Options struct {
	Store store.Store
	// Inventory lists and deletes the backend's objects, or nil for
	// none to reconcile.
	Inventory backend.Inventory
	// Fence applies the fence, or nil for no fence.
	Fence backend.Fence
	// Registry is the local registry, or nil for none.
	Registry Registry
	// ReportOnly makes this process's reconcile report-only whatever the
	// database says (silod serve --reconcile=report-only).
	ReportOnly bool
	// Grace and ChargeGrace override the package's, for tests.
	Grace, ChargeGrace time.Duration
	// Logf logs; nil is log.Printf.
	Logf func(format string, args ...any)
}

// Registry is the local registry's half of the reconcile
// (local-registry.md, §10 "Garbage collection"); *registry.Service is
// it.
type Registry interface {
	Reconcile(ctx context.Context, reportOnly bool) (*registry.DriftReport, error)
}

// Reconciler runs the reconcile.
type Reconciler struct {
	o Options
}

// New returns a Reconciler.
func New(o Options) *Reconciler {
	if o.Grace <= 0 {
		o.Grace = Grace
	}
	if o.ChargeGrace <= 0 {
		o.ChargeGrace = ChargeGrace
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	return &Reconciler{o: o}
}

// Object is one object in the backend, as the reconcile sorts it.
type Object = backend.Object

// Report is what one Run found and did.
type Report struct {
	// Epoch is the leader epoch it ran under.
	Epoch int64
	// ReportOnly is whether orphans were only reported.
	ReportOnly bool
	// Objects is how many objects it listed.
	Objects int
	// Orphans are every orphan seen this run, by Key; Found those seen
	// for the first time, Deleted those it deleted.
	Orphans, Found, Deleted []string
	// Foreign are the foreign objects, by Key.
	Foreign []string
	// Dropped are the ledger objects whose charges it dropped.
	Dropped []string
	// Recounted are the silos whose used it changed.
	Recounted []string
	// Drift are the fence steps that repaired drift.
	Drift []string
	// Registry is what the registry's reconcile found, if it ran.
	Registry *registry.DriftReport
}

// Run reconciles once. ctx must carry the leader's term (leader.Guard).
func (r *Reconciler) Run(ctx context.Context) (*Report, error) {
	rep := &Report{ReportOnly: r.o.ReportOnly}
	if t := leader.TermOf(ctx); t != nil {
		rep.Epoch = t.Epoch
	}
	if !rep.ReportOnly {
		mode, ok, err := store.GetSetting(ctx, r.o.Store, store.SettingReconcile)
		if err != nil {
			return nil, err
		}
		rep.ReportOnly = ok && mode == store.ReconcileReportOnly
	}
	var root *tree.Silo
	var regions []string
	err := r.o.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		root, err = tree.Root(ctx, tx)
		if errors.Is(err, tree.ErrNotFound) {
			root = nil
			return nil
		}
		if err != nil {
			return err
		}
		regions, err = ledger.Regions(ctx, tx, root.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if root == nil {
		// Nothing to reconcile before bootstrap: no tree, no fence.
		return rep, nil
	}
	var errs []error
	if r.o.Inventory != nil {
		if err := r.objects(ctx, root, regions, rep); err != nil {
			errs = append(errs, err)
		}
	}
	if r.o.Fence != nil {
		if err := r.fence(ctx, rep); err != nil {
			errs = append(errs, err)
		}
	}
	if r.o.Registry != nil {
		err := r.o.Store.Tx(ctx, func(tx store.Tx) error { return leader.Guard(ctx, tx) })
		if err == nil {
			rep.Registry, err = r.o.Registry.Reconcile(ctx, rep.ReportOnly)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("registry: %w", err))
		}
	}
	return rep, errors.Join(errs...)
}

// objects lists the host project and settles orphans, foreign objects,
// charges and used.
func (r *Reconciler) objects(ctx context.Context, root *tree.Silo, regions []string, rep *Report) error {
	l, err := r.o.Inventory.List(ctx, regions)
	if err != nil {
		return err
	}
	objs, covered := l.Objects, l.Covered
	rep.Objects = len(objs)
	exists := map[string]bool{}
	for _, o := range objs {
		if o.Ledger != "" {
			exists[o.Ledger] = true
		}
	}
	var due []Object
	err = r.o.Store.Tx(ctx, func(tx store.Tx) error {
		*rep = Report{Epoch: rep.Epoch, ReportOnly: rep.ReportOnly, Objects: rep.Objects}
		due = nil
		if err := leader.Guard(ctx, tx); err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		silos, err := tree.All(ctx, tx)
		if err != nil {
			return err
		}
		live := map[string]bool{}
		for _, s := range silos {
			live[s.ID] = true
		}
		seen, err := readSeen(ctx, tx)
		if err != nil {
			return err
		}
		current := map[string]bool{}
		for _, o := range objs {
			kind := ""
			switch {
			case o.Install:
			case o.Unrecorded:
				kind = seenOrphan
			case o.Silo != "" && !live[o.Silo]:
				orphan := o.Labelled
				if !orphan {
					// A prefix alone is a weaker sign than the label: a
					// foreign object's name can start with six letters
					// and a dash. It is an orphan only if this database
					// knew the silo it names.
					if orphan, err = fenceapi.EverAudited(ctx, tx, o.Silo); err != nil {
						return err
					}
				}
				if orphan {
					kind = seenOrphan
				} else if o.InVPC {
					kind = seenForeign
				}
			case o.Silo == "" && o.InVPC:
				kind = seenForeign
			}
			if kind == "" {
				continue
			}
			key := o.Key()
			current[key] = true
			prev, ok := seen[key]
			if !ok || prev.kind != kind {
				if err := writeSeen(ctx, tx, key, o.Silo, kind, now); err != nil {
					return err
				}
				prev = seenRow{kind: kind, first: now}
				akind := KindOrphanFound
				if kind == seenForeign {
					akind = KindForeignFound
				}
				if err := audit(ctx, tx, root, root, akind, objectFields(o)); err != nil {
					return err
				}
				if kind == seenOrphan {
					rep.Found = append(rep.Found, key)
				}
			}
			if kind == seenForeign {
				rep.Foreign = append(rep.Foreign, key)
				continue
			}
			rep.Orphans = append(rep.Orphans, key)
			if !now.Before(prev.first.Add(r.o.Grace)) {
				due = append(due, o)
			}
		}
		for key := range seen {
			if !current[key] {
				if err := dropSeen(ctx, tx, key); err != nil {
					return err
				}
			}
		}

		// Charges for objects that don't exist and that nothing is
		// pending for.
		pending, err := r.o.Inventory.Pending(ctx, tx)
		if err != nil {
			return err
		}
		charged, err := ledger.ChargedObjects(ctx, tx, now.Add(-r.o.ChargeGrace))
		if err != nil {
			return err
		}
		byID := map[string]*tree.Silo{}
		for _, s := range silos {
			byID[s.ID] = s
		}
		for _, c := range charged {
			if exists[c.Object] || pending[c.Object] || !coveredBy(covered, c.Object) {
				continue
			}
			rows, err := ledger.Refund(ctx, tx, c.Object, nil)
			if err != nil {
				return err
			}
			rep.Dropped = append(rep.Dropped, c.Object)
			for _, p := range c.Payers {
				s := byID[p]
				if s == nil {
					s = root
				}
				var lines []map[string]any
				for _, row := range rows {
					if row.Payer == p {
						lines = append(lines, map[string]any{"key": row.Key.String(), "amount": ledger.FormatAmount(row.Amount)})
					}
				}
				if err := audit(ctx, tx, root, s, KindChargeDropped, map[string]any{"object": c.Object, "refunded": lines}); err != nil {
					return err
				}
			}
		}

		// used, from the charges, by payer.
		for _, s := range silos {
			drift, err := ledger.Recount(ctx, tx, s.ID)
			if err != nil {
				return err
			}
			if len(drift) == 0 {
				continue
			}
			rep.Recounted = append(rep.Recounted, s.ID)
			var lines []map[string]any
			for _, d := range drift {
				lines = append(lines, map[string]any{"key": d.Key.String(), "was": ledger.FormatAmount(d.Was), "now": ledger.FormatAmount(d.Now)})
			}
			if err := audit(ctx, tx, root, s, KindRecount, map[string]any{"used": lines}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, k := range rep.Foreign {
		r.o.Logf("reconcile: foreign object %s, left alone", k)
	}
	for _, k := range rep.Orphans {
		r.o.Logf("reconcile: orphan %s", k)
	}
	if rep.ReportOnly {
		if len(due) > 0 {
			r.o.Logf("reconcile: report-only: %d orphans past the grace period are not deleted (silo reconcile --apply)", len(due))
		}
		return nil
	}
	return r.deleteOrphans(ctx, root, due, rep)
}

// deleteOrphans deletes due, instances first, since a disk attached to
// one can't be deleted until it is gone. One that fails is left for the
// next run.
func (r *Reconciler) deleteOrphans(ctx context.Context, root *tree.Silo, due []Object, rep *Report) error {
	sort.SliceStable(due, func(i, j int) bool {
		return due[i].Kind == "instances" && due[j].Kind != "instances"
	})
	var errs []error
	for _, o := range due {
		// The guard first: a leader that has lost the lease deletes
		// nothing.
		if err := r.o.Store.Tx(ctx, func(tx store.Tx) error { return leader.Guard(ctx, tx) }); err != nil {
			return err
		}
		op, err := r.o.Inventory.Delete(ctx, o)
		if err != nil {
			errs = append(errs, fmt.Errorf("deleting orphan %s: %w", o.Key(), err))
			continue
		}
		fields := objectFields(o)
		if op != "" {
			fields["operation"] = op
		}
		err = r.o.Store.Tx(ctx, func(tx store.Tx) error {
			if err := leader.Guard(ctx, tx); err != nil {
				return err
			}
			if err := dropSeen(ctx, tx, o.Key()); err != nil {
				return err
			}
			return audit(ctx, tx, root, root, KindOrphanDeleted, fields)
		})
		if err != nil {
			return err
		}
		rep.Deleted = append(rep.Deleted, o.Key())
		r.o.Logf("reconcile: deleted orphan %s", o.Key())
	}
	return errors.Join(errs...)
}

// fence applies the fence. Drift is audited by AuditDrift, which the
// applier calls whichever of its applies finds it.
func (r *Reconciler) fence(ctx context.Context, rep *Report) error {
	drift, err := r.o.Fence.Repair(ctx)
	rep.Drift = append(rep.Drift, drift...)
	return err
}

// AuditDrift is a gcefence.Applier's OnDrift: it logs the drift an apply
// repaired and audits it as fence_drift under the root, in a
// transaction guarded by the leader epoch.
func AuditDrift(st store.Store, logf func(string, ...any)) func(context.Context, []gcefence.Step, bool) {
	audit := AuditDriftNames(st, logf)
	return func(ctx context.Context, steps []gcefence.Step, repaired bool) {
		var names []string
		for _, s := range steps {
			names = append(names, s.String())
		}
		audit(ctx, names, repaired)
	}
}

// AuditDriftNames is AuditDrift for a fence whose drift is described
// by name alone, as the local backend's netfence.Applier reports it.
func AuditDriftNames(st store.Store, logf func(string, ...any)) func(context.Context, []string, bool) {
	if logf == nil {
		logf = log.Printf
	}
	return func(ctx context.Context, names []string, repaired bool) {
		logf("fence: the policy had drifted; %d steps to repair it (repaired: %v): %s", len(names), repaired, strings.Join(names, "; "))
		err := st.Tx(ctx, func(tx store.Tx) error {
			if err := leader.Guard(ctx, tx); err != nil {
				return err
			}
			root, err := tree.Root(ctx, tx)
			if err != nil {
				return err
			}
			return audit(ctx, tx, root, root, KindFenceDrift, map[string]any{"steps": names, "repaired": repaired})
		})
		if err != nil && ctx.Err() == nil {
			logf("fence: auditing drift: %v", err)
		}
	}
}

// coveredBy reports whether object is in one of the scopes the list
// read completely.
func coveredBy(covered []string, object string) bool {
	for _, p := range covered {
		if rest, ok := strings.CutPrefix(object, p); ok && rest != "" && !strings.Contains(rest, "/") {
			return true
		}
	}
	return false
}

func objectFields(o Object) map[string]any {
	f := map[string]any{"object": o.Key(), "collection": o.Kind, "name": o.Name}
	if o.Scope != "" {
		f["scope"] = o.Scope
	}
	if o.Silo != "" {
		f["silo"] = o.Silo
	}
	return f
}

// audit writes a record of the reconcile's, made by the root, under s.
func audit(ctx context.Context, tx store.Tx, root, s *tree.Silo, kind string, fields map[string]any) error {
	return fenceapi.Audit(ctx, tx, s, fenceapi.Caller{Silo: root.ID, Via: fenceapi.ViaInternal}, kind, "", "OK", fields)
}

// ---- reconcile_objects ----

const (
	seenOrphan  = "orphan"
	seenForeign = "foreign"
)

type seenRow struct {
	kind  string
	first time.Time
}

// The table records foreign objects as well as orphans, with silo ”
// for a foreign one. Its name is the migration's.
func readSeen(ctx context.Context, tx store.Tx) (map[string]seenRow, error) {
	rows, err := tx.Query(ctx, `SELECT object, silo, first_seen FROM reconcile_orphans`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]seenRow{}
	for rows.Next() {
		var key, silo string
		var first int64
		if err := rows.Scan(&key, &silo, &first); err != nil {
			return nil, err
		}
		kind := seenOrphan
		if silo == "" {
			kind = seenForeign
		}
		out[key] = seenRow{kind: kind, first: store.Time(first)}
	}
	return out, rows.Err()
}

func writeSeen(ctx context.Context, tx store.Tx, key, silo, kind string, now time.Time) error {
	if kind == seenForeign {
		silo = ""
	}
	_, err := tx.Exec(ctx, `INSERT INTO reconcile_orphans (object, silo, first_seen) VALUES (?, ?, ?)
		ON CONFLICT (object) DO UPDATE SET silo = excluded.silo, first_seen = excluded.first_seen`, key, silo, store.Millis(now))
	return err
}

func dropSeen(ctx context.Context, tx store.Tx, key string) error {
	_, err := tx.Exec(ctx, `DELETE FROM reconcile_orphans WHERE object = ?`, key)
	return err
}
