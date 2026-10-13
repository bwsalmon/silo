// Package objseq is the local backend's object sequence
// (docs/silo/local-rootless-storage.md, "Restoring", call L40): a
// number in the store that only goes up, given to every container,
// network, volume and subtree silod makes. Each is labelled on disk
// with its number (runtime.LabelSeq in containerd, storage.SeqFile
// beside a volume or in a subtree), so a store copied before an object
// was made has a high-water mark below the object's label, and silod
// can tell at start that the store is older than the disk.
package objseq

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bwsalmon/silo/pkg/store"
)

// HighWater is the last number Next gave, or 0.
func HighWater(ctx context.Context, tx store.Tx) (uint64, error) {
	v, ok, err := store.ReadSetting(ctx, tx, store.SettingObjectSeq)
	if err != nil || !ok {
		return 0, err
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("objseq: setting %s is %q: %w", store.SettingObjectSeq, v, err)
	}
	return n, nil
}

// Next gives the next number, in tx: the object it labels is made
// once tx has committed.
func Next(ctx context.Context, tx store.Tx) (uint64, error) {
	n, err := HighWater(ctx, tx)
	if err != nil {
		return 0, err
	}
	n++
	return n, store.WriteSetting(ctx, tx, store.SettingObjectSeq, strconv.FormatUint(n, 10))
}

// Raise moves the high-water mark up to n, if it is below it: after a
// restore, so that the objects made from then on are numbered above
// every one already on disk.
func Raise(ctx context.Context, tx store.Tx, n uint64) error {
	hw, err := HighWater(ctx, tx)
	if err != nil || n <= hw {
		return err
	}
	return store.WriteSetting(ctx, tx, store.SettingObjectSeq, strconv.FormatUint(n, 10))
}

// Parse reads a label's number. An empty or malformed label is 0: an
// object made before the sequence, which can't date the store.
func Parse(label string) uint64 {
	n, err := strconv.ParseUint(label, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Format is n as a label.
func Format(n uint64) string { return strconv.FormatUint(n, 10) }
