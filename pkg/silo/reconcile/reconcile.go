// Package reconcile is the leader's reconcile (docs/silo/gcp-backend.md,
// "Reconcile", SV-77): silod's database is the truth for the tree, the
// allocations, the grants and the fence, and GCE is the truth for the
// tenants' objects. A Run:
//
//   - lists every object in the host project's allowed regions, and the
//     global ones, and sorts each by its silo label or <id>- prefix;
//   - audits an orphan, an object with the label or prefix of a silo
//     that doesn't exist, as orphan.found the first time it sees it,
//     and deletes it, as orphan.deleted, once it has been seen for the
//     grace period of database time (call 27), unless reconcile is
//     report-only (state.md "Backups and recovery", call 36);
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
//     applier's OnDrift, so drift any apply finds is audited).
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

	facade "github.com/bwsalmon/silo/pkg/silo/facade/gce"
	"github.com/bwsalmon/silo/pkg/silo/fence"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/gce"
	"github.com/bwsalmon/silo/pkg/silo/ids"
	"github.com/bwsalmon/silo/pkg/silo/leader"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
	compute "google.golang.org/api/compute/v1"
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
	GCE   gce.Client
	// Project is the host project.
	Project string
	// Network is the VPC's name; "silo" by default.
	Network string
	// Fence applies the fence, or nil for no fence.
	Fence *fence.Applier
	// ReportOnly makes this process's reconcile report-only whatever the
	// database says (silod serve --reconcile=report-only).
	ReportOnly bool
	// Grace and ChargeGrace override the package's, for tests.
	Grace, ChargeGrace time.Duration
	// Logf logs; nil is log.Printf.
	Logf func(format string, args ...any)
}

// Reconciler runs the reconcile.
type Reconciler struct {
	o Options
}

// New returns a Reconciler.
func New(o Options) *Reconciler {
	if o.Network == "" {
		o.Network = "silo"
	}
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

// Object is one object in the host project, as the reconcile sorts it.
type Object struct {
	// Collection is compute/v1's: instances, disks, images, snapshots,
	// firewalls, subnetworks or instanceTemplates.
	Collection string
	// Scope is the zone or region, or "" for a global object.
	Scope string
	// Name is the real name.
	Name string
	// Silo is the silo its silo label names, or its <id>- prefix; ""
	// for neither.
	Silo string
	// Labelled is whether the silo came from the label.
	Labelled bool
	// InVPC is whether the object is in the VPC: an instance with an
	// interface on it, a subnetwork or a firewall rule of it.
	InVPC bool
	// Install is whether it is install's own: silo-install labelled,
	// or named silo-...
	Install bool
}

// Key names the object: "<collection>:<scope>/<name>", or
// "<collection>:<name>" for a global one.
func (o Object) Key() string {
	if o.Scope == "" {
		return o.Collection + ":" + o.Name
	}
	return o.Collection + ":" + o.Scope + "/" + o.Name
}

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
	if r.o.GCE != nil && r.o.Project != "" {
		if err := r.objects(ctx, root, regions, rep); err != nil {
			errs = append(errs, err)
		}
	}
	if r.o.Fence != nil {
		if err := r.fence(ctx, rep); err != nil {
			errs = append(errs, err)
		}
	}
	return rep, errors.Join(errs...)
}

// objects lists the host project and settles orphans, foreign objects,
// charges and used.
func (r *Reconciler) objects(ctx context.Context, root *tree.Silo, regions []string, rep *Report) error {
	objs, covered, err := r.list(ctx, regions)
	if err != nil {
		return err
	}
	rep.Objects = len(objs)
	exists := map[string]bool{}
	for _, o := range objs {
		if lo := facade.LedgerObject(o.Collection, o.Scope, o.Name); lo != "" {
			exists[lo] = true
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
		pending, err := facade.PendingObjects(ctx, tx)
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
		return due[i].Collection == "instances" && due[j].Collection != "instances"
	})
	var errs []error
	for _, o := range due {
		// The guard first: a leader that has lost the lease deletes
		// nothing.
		if err := r.o.Store.Tx(ctx, func(tx store.Tx) error { return leader.Guard(ctx, tx) }); err != nil {
			return err
		}
		op, err := r.delete(ctx, o)
		if err != nil && !gce.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting orphan %s: %w", o.Key(), err))
			continue
		}
		fields := objectFields(o)
		if op != nil {
			fields["operation"] = op.Name
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

func (r *Reconciler) delete(ctx context.Context, o Object) (*compute.Operation, error) {
	p := r.o.Project
	switch o.Collection {
	case "instances":
		return r.o.GCE.DeleteInstance(ctx, p, o.Scope, o.Name)
	case "disks":
		return r.o.GCE.DeleteDisk(ctx, p, o.Scope, o.Name)
	case "images":
		return r.o.GCE.DeleteImage(ctx, p, o.Name)
	case "snapshots":
		return r.o.GCE.DeleteSnapshot(ctx, p, o.Name)
	case "firewalls":
		return r.o.GCE.DeleteFirewall(ctx, p, o.Name)
	case "subnetworks":
		return r.o.GCE.DeleteSubnetwork(ctx, p, o.Scope, o.Name)
	case "instanceTemplates":
		return r.o.GCE.DeleteInstanceTemplate(ctx, p, o.Scope, o.Name)
	}
	return nil, fmt.Errorf("no delete for %s", o.Collection)
}

// fence applies the fence. Drift is audited by AuditDrift, which the
// applier calls whichever of its applies finds it.
func (r *Reconciler) fence(ctx context.Context, rep *Report) error {
	drift, err := r.o.Fence.Repair(ctx)
	for _, s := range drift {
		rep.Drift = append(rep.Drift, s.String())
	}
	return err
}

// AuditDrift is a fence.Applier's OnDrift: it logs the drift an apply
// repaired and audits it as fence_drift under the root, in a
// transaction guarded by the leader epoch.
func AuditDrift(st store.Store, logf func(string, ...any)) func(context.Context, []fence.Step, bool) {
	if logf == nil {
		logf = log.Printf
	}
	return func(ctx context.Context, steps []fence.Step, repaired bool) {
		var names []string
		for _, s := range steps {
			names = append(names, s.String())
		}
		logf("fence: the policy had drifted; %d steps to repair it (repaired: %v): %s", len(steps), repaired, strings.Join(names, "; "))
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

// list lists every object the reconcile looks at: instances and disks
// in the allowed regions' zones, subnetworks and regional templates in
// the allowed regions, and the global images, snapshots, firewall rules
// and templates. covered are the ledger object prefixes it listed
// completely ("instance:us-central1-a/").
func (r *Reconciler) list(ctx context.Context, regions []string) ([]Object, []string, error) {
	p := r.o.Project
	allowed := map[string]bool{}
	for _, rg := range regions {
		allowed[rg] = true
	}
	zones, err := r.o.GCE.ListZones(ctx, p)
	if err != nil {
		return nil, nil, fmt.Errorf("listing zones: %w", err)
	}
	var out []Object
	var covered []string
	add := func(collection, scope string) {
		if lo := facade.LedgerObject(collection, scope, ""); lo != "" {
			covered = append(covered, lo)
		}
	}
	var all gce.ListFilter
	for _, z := range zones {
		if !allowed[gce.ZoneRegion(z.Name)] {
			continue
		}
		insts, err := r.o.GCE.ListInstances(ctx, p, z.Name, all)
		if err != nil {
			return nil, nil, fmt.Errorf("listing instances in %s: %w", z.Name, err)
		}
		for _, i := range insts {
			o := r.object("instances", z.Name, i.Name, i.Labels)
			for _, n := range i.NetworkInterfaces {
				o.InVPC = o.InVPC || gce.LastSegment(n.Network) == r.o.Network
			}
			out = append(out, o)
		}
		add("instances", z.Name)
		disks, err := r.o.GCE.ListDisks(ctx, p, z.Name, all)
		if err != nil {
			return nil, nil, fmt.Errorf("listing disks in %s: %w", z.Name, err)
		}
		for _, d := range disks {
			out = append(out, r.object("disks", z.Name, d.Name, d.Labels))
		}
		add("disks", z.Name)
	}
	for _, rg := range regions {
		if rg == ledger.Global {
			continue
		}
		subs, err := r.o.GCE.ListSubnetworks(ctx, p, rg, all)
		if err != nil {
			return nil, nil, fmt.Errorf("listing subnetworks in %s: %w", rg, err)
		}
		for _, s := range subs {
			o := r.object("subnetworks", rg, s.Name, nil)
			o.InVPC = gce.LastSegment(s.Network) == r.o.Network
			out = append(out, o)
		}
		add("subnetworks", rg)
		ts, err := r.o.GCE.ListInstanceTemplates(ctx, p, rg, all)
		if err != nil {
			return nil, nil, fmt.Errorf("listing instance templates in %s: %w", rg, err)
		}
		for _, t := range ts {
			var labels map[string]string
			if t.Properties != nil {
				labels = t.Properties.Labels
			}
			out = append(out, r.object("instanceTemplates", rg, t.Name, labels))
		}
		add("instanceTemplates", rg)
	}
	ts, err := r.o.GCE.ListInstanceTemplates(ctx, p, "", all)
	if err != nil {
		return nil, nil, fmt.Errorf("listing global instance templates: %w", err)
	}
	for _, t := range ts {
		var labels map[string]string
		if t.Properties != nil {
			labels = t.Properties.Labels
		}
		out = append(out, r.object("instanceTemplates", "", t.Name, labels))
	}
	add("instanceTemplates", "global")
	imgs, err := r.o.GCE.ListImages(ctx, p, all)
	if err != nil {
		return nil, nil, fmt.Errorf("listing images: %w", err)
	}
	for _, i := range imgs {
		out = append(out, r.object("images", "", i.Name, i.Labels))
	}
	add("images", "")
	snaps, err := r.o.GCE.ListSnapshots(ctx, p, all)
	if err != nil {
		return nil, nil, fmt.Errorf("listing snapshots: %w", err)
	}
	for _, s := range snaps {
		out = append(out, r.object("snapshots", "", s.Name, s.Labels))
	}
	add("snapshots", "")
	fws, err := r.o.GCE.ListFirewalls(ctx, p, all)
	if err != nil {
		return nil, nil, fmt.Errorf("listing firewall rules: %w", err)
	}
	for _, f := range fws {
		o := r.object("firewalls", "", f.Name, nil)
		o.InVPC = gce.LastSegment(f.Network) == r.o.Network
		o.Install = o.Install || strings.HasPrefix(f.Description, installDescription)
		out = append(out, o)
	}
	add("firewalls", "")
	return out, covered, nil
}

// What marks install's own objects (pkg/silo/install, whose names these
// are; this package may not import it).
const (
	installLabel       = "silo-install"
	installDescription = "silo-install:"
	installPrefix      = "silo-"
	siloLabel          = "silo"
)

// object sorts one object by its label and name.
func (r *Reconciler) object(collection, scope, name string, labels map[string]string) Object {
	o := Object{Collection: collection, Scope: scope, Name: name}
	if _, ok := labels[installLabel]; ok || strings.HasPrefix(name, installPrefix) {
		o.Install = true
		return o
	}
	if id := labels[siloLabel]; id != "" {
		o.Silo, o.Labelled = id, true
		return o
	}
	if id, _, ok := strings.Cut(name, "-"); ok && ids.Valid(id) {
		o.Silo = id
	}
	return o
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
	f := map[string]any{"object": o.Key(), "collection": o.Collection, "name": o.Name}
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
