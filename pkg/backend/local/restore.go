package local

import (
	"context"
	"errors"
	"fmt"

	"github.com/bwsalmon/silo/pkg/backend/local/objseq"
	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/backend/local/storage"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// DiskSeq is the highest object sequence label on disk: on the drivers'
// containers (runtime.LabelSeq) and on the storage driver's subtrees
// and volumes (storage.SeqFile). Either may be nil.
func DiskSeq(ctx context.Context, drivers []runtime.Driver, st storage.Driver) (uint64, error) {
	var max uint64
	for _, d := range drivers {
		cs, err := d.List(ctx)
		if err != nil {
			return 0, fmt.Errorf("local: listing %s containers: %w", d.Class(), err)
		}
		for _, c := range cs {
			max = maxSeq(max, objseq.Parse(c.Labels[runtime.LabelSeq]))
		}
	}
	if st != nil {
		es, err := st.List(ctx)
		if err != nil {
			return 0, fmt.Errorf("local: listing volumes: %w", err)
		}
		for _, e := range es {
			max = maxSeq(max, e.Seq)
		}
	}
	return max, nil
}

func maxSeq(a, b uint64) uint64 {
	if b > a {
		return b
	}
	return a
}

// DetectRestore is silod's check at start for a restored store
// (local-rootless-storage.md call L40). If an object on disk carries a
// sequence number above the store's high-water mark, the store is older
// than the disk; force says so whatever the numbers (silod
// --after-restore, for a restore that replaced the cache too). Either
// way it, in one transaction:
//
//   - sets the reconcile to report-only, so that the objects made after
//     the copy are reported as orphans and never deleted;
//   - records the restore, which describe shows on the root until silo
//     reconcile --apply;
//   - audits restore_detected under the root, with both numbers;
//   - raises the high-water mark to the disk's, so that what silod makes
//     from now on is numbered above every object already on disk, and
//     the next start doesn't find the same restore again.
//
// It returns the restore, or nil if there was none.
func DetectRestore(ctx context.Context, st store.Store, drivers []runtime.Driver, sd storage.Driver, force bool) (*fenceapi.RestoreDetected, error) {
	disk, err := DiskSeq(ctx, drivers, sd)
	if err != nil {
		return nil, err
	}
	var out *fenceapi.RestoreDetected
	err = st.Tx(ctx, func(tx store.Tx) error {
		out = nil
		hw, err := objseq.HighWater(ctx, tx)
		if err != nil {
			return err
		}
		if disk <= hw && !force {
			return nil
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		r := fenceapi.RestoreDetected{StoreSeq: hw, DiskSeq: disk, Forced: force, At: now}
		if err := store.WriteSetting(ctx, tx, store.SettingReconcile, store.ReconcileReportOnly); err != nil {
			return err
		}
		if err := fenceapi.WriteRestoreDetected(ctx, tx, r); err != nil {
			return err
		}
		if err := objseq.Raise(ctx, tx, disk); err != nil {
			return err
		}
		root, err := tree.Root(ctx, tx)
		switch {
		case errors.Is(err, tree.ErrNotFound):
			// A store from before bootstrap has no root to audit under;
			// the setting is the record.
		case err != nil:
			return err
		default:
			fields := map[string]any{"store_seq": hw, "disk_seq": disk}
			if force {
				fields["forced"] = true
			}
			if err := fenceapi.Audit(ctx, tx, root, fenceapi.Caller{Silo: root.ID, Via: fenceapi.ViaInternal},
				fenceapi.KindRestoreDetected, "", "OK", fields); err != nil {
				return err
			}
		}
		out = &r
		return nil
	})
	return out, err
}
