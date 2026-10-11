package ledger

import (
	"context"
	"database/sql"
	"errors"
	"regexp"

	"github.com/bwsalmon/silo/pkg/silo/store"
)

// Class is one row of the class table.
type Class struct {
	Kind   string
	Name   string
	Unit   string
	Sort   string // SortHeld or SortMetered
	Scope  string // ScopeRegional or ScopeGlobal
	Period string // "", or a Period for a metered class
}

var className = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// LookupClass reads a class. One that isn't in the table is
// INVALID_ARGUMENT.
func LookupClass(ctx context.Context, tx store.Tx, kind, name string) (*Class, error) {
	c := Class{Kind: kind, Name: name}
	err := tx.QueryRow(ctx, `SELECT unit, sort, scope, period FROM quota_classes WHERE kind = ? AND name = ?`,
		kind, name).Scan(&c.Unit, &c.Sort, &c.Scope, &c.Period)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &Error{Code: InvalidArgument, Message: "unknown quota class " + kind + "/" + name,
			Key: Key{Kind: kind, Class: name}}
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Classes lists the class table, by kind and name.
func Classes(ctx context.Context, tx store.Tx) ([]Class, error) {
	rows, err := tx.Query(ctx, `SELECT kind, name, unit, sort, scope, period FROM quota_classes ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Class
	for rows.Next() {
		var c Class
		if err := rows.Scan(&c.Kind, &c.Name, &c.Unit, &c.Sort, &c.Scope, &c.Period); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddClass adds a class to the table, or does nothing if (kind, name)
// is already there with the same definition. The root-capacity reader
// calls it for a GPU family it discovers; custom allocators, later,
// for the classes they declare.
func AddClass(ctx context.Context, tx store.Tx, c Class) error {
	if !className.MatchString(c.Kind) || !className.MatchString(c.Name) {
		return invalid("class %q/%q: kind and name are lower-case identifiers", c.Kind, c.Name)
	}
	if c.Sort != SortHeld && c.Sort != SortMetered {
		return invalid("class %s/%s: sort %q (want %s or %s)", c.Kind, c.Name, c.Sort, SortHeld, SortMetered)
	}
	if c.Scope != ScopeRegional && c.Scope != ScopeGlobal {
		return invalid("class %s/%s: scope %q (want %s or %s)", c.Kind, c.Name, c.Scope, ScopeRegional, ScopeGlobal)
	}
	if c.Period != "" {
		if c.Sort != SortMetered {
			return invalid("class %s/%s: only a metered class has a period", c.Kind, c.Name)
		}
		if _, err := periodOf(c.Period); err != nil {
			return err
		}
	}
	old, err := LookupClass(ctx, tx, c.Kind, c.Name)
	if err == nil {
		if *old != c {
			return precondition("class %s/%s already exists with a different definition", c.Kind, c.Name)
		}
		return nil
	}
	if CodeOf(err) != InvalidArgument {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO quota_classes (kind, name, unit, sort, scope, period) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Kind, c.Name, c.Unit, c.Sort, c.Scope, c.Period)
	return err
}

// GPUClass is one row of silod's GPU config: an accelerator type, the
// GCE metric that limits it, and the class charged for it.
type GPUClass struct {
	Accelerator string // nvidia-tesla-t4
	Spot        bool
	Metric      string // NVIDIA_T4_GPUS, PREEMPTIBLE_NVIDIA_T4_GPUS
	Class       string // nvidia_t4_gpus, spot_nvidia_t4_gpus
}

// LookupGPU returns the class an accelerator type charges, standard or
// Spot. An accelerator type that isn't in the table is
// INVALID_ARGUMENT.
func LookupGPU(ctx context.Context, tx store.Tx, accelerator string, spot bool) (*GPUClass, error) {
	g := GPUClass{Accelerator: accelerator, Spot: spot}
	err := tx.QueryRow(ctx, `SELECT metric, class FROM gpu_classes WHERE accelerator = ? AND spot = ?`,
		accelerator, spot).Scan(&g.Metric, &g.Class)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalid("unknown accelerator type %q", accelerator)
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// GPUClasses lists the GPU config.
func GPUClasses(ctx context.Context, tx store.Tx) ([]GPUClass, error) {
	rows, err := tx.Query(ctx, `SELECT accelerator, spot, metric, class FROM gpu_classes ORDER BY accelerator, spot`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GPUClass
	for rows.Next() {
		var g GPUClass
		if err := rows.Scan(&g.Accelerator, &g.Spot, &g.Metric, &g.Class); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AddGPU adds a GPU mapping, its class to the class table (kind gce,
// regional, held) and its row to the facade's price table, for a family
// discovered after install.
func AddGPU(ctx context.Context, tx store.Tx, g GPUClass) error {
	if g.Accelerator == "" || g.Metric == "" {
		return invalid("GPU mapping %+v: accelerator and metric are required", g)
	}
	if err := AddClass(ctx, tx, Class{Kind: "gce", Name: g.Class, Unit: "GPU", Sort: SortHeld, Scope: ScopeRegional}); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO gpu_classes (accelerator, spot, metric, class) VALUES (?, ?, ?, ?)
		ON CONFLICT (accelerator, spot) DO UPDATE SET metric = excluded.metric, class = excluded.class`,
		g.Accelerator, g.Spot, g.Metric, g.Class)
	if err != nil {
		return err
	}
	// The facade's price for it, replacing a class mapped before.
	variant := "STANDARD"
	if g.Spot {
		variant = "SPOT"
	}
	if _, err := tx.Exec(ctx, `DELETE FROM facade_prices WHERE resource = ? AND variant = ? AND measure = ?`,
		"instance", variant, GPUMeasure(g.Accelerator)); err != nil {
		return err
	}
	return AddPrice(ctx, tx, Price{Resource: "instance", Variant: variant, Measure: GPUMeasure(g.Accelerator), Class: g.Class, Running: true})
}

// checkKey looks up k's class and checks k's region against its scope.
func checkKey(ctx context.Context, tx store.Tx, k Key) (*Class, error) {
	c, err := LookupClass(ctx, tx, k.Kind, k.Class)
	if err != nil {
		return nil, err
	}
	switch {
	case c.Scope == ScopeGlobal && k.Region != Global:
		return nil, &Error{Code: InvalidArgument, Key: k,
			Message: k.Kind + "/" + k.Class + " is a global class; its region is " + Global + ", not " + k.Region}
	case c.Scope != ScopeGlobal && (k.Region == Global || k.Region == ""):
		return nil, &Error{Code: InvalidArgument, Key: k,
			Message: k.Kind + "/" + k.Class + " is a regional class and needs a region"}
	}
	return c, nil
}
