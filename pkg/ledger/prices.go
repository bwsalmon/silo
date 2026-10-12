package ledger

import (
	"context"

	"github.com/bwsalmon/silo/pkg/store"
)

// Price is one row of the compute/v1 facade's price table: an object of
// Resource and Variant charges Class by Measure of it (gcp-backend.md,
// "Instances" and "Disks"). Running is set where the charge is held
// only while the instance isn't TERMINATED.
type Price struct {
	Resource string // instance, disk, image, snapshot
	Variant  string // STANDARD or SPOT for an instance, the type for a disk
	Measure  string // count, guest_cpus, memory_mb, size_gb, storage_gb, gpus, GPUMeasure(type), local_ssd_gb, iops, throughput
	Class    string // a gce class in the class table
	Running  bool
}

// Prices lists the price table's rows for one resource and variant, by
// measure and class. None means the variant isn't one the facade
// charges for, so isn't allowed.
func Prices(ctx context.Context, tx store.Tx, resource, variant string) ([]Price, error) {
	rows, err := tx.Query(ctx, `SELECT resource, variant, measure, class, running FROM facade_prices
		WHERE resource = ? AND variant = ? ORDER BY measure, class`, resource, variant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Price
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.Resource, &p.Variant, &p.Measure, &p.Class, &p.Running); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddPrice adds a row to the price table, or replaces the row with the
// same resource, variant, measure and class. Its class must be in the
// class table, kind gce.
func AddPrice(ctx context.Context, tx store.Tx, p Price) error {
	if p.Resource == "" || p.Measure == "" {
		return invalid("price %+v: resource and measure are required", p)
	}
	if _, err := LookupClass(ctx, tx, "gce", p.Class); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO facade_prices (resource, variant, measure, class, running) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (resource, variant, measure, class) DO UPDATE SET running = excluded.running`,
		p.Resource, p.Variant, p.Measure, p.Class, p.Running)
	return err
}

// GPUMeasure is the measure of an instance's GPUs of one accelerator
// type, which charges that type's class in gpu_classes.
func GPUMeasure(accelerator string) string { return "gpus:" + accelerator }
