// Package storage keeps silos' tenant data on the local backend: a
// subtree per silo, a volume per Docker volume
// (docs/silo/local-rootless-storage.md, "The storage driver", call
// L37). The first and only v1 driver is Dir, plain directories; zfs and
// btrfs come later behind the same interface.
//
// Volume sizes are charged against the ledger, not enforced by Dir.
// Meter measures real use, reports a volume over its charge
// (volume_over_quota), and refuses new mounts of one with a 409
// (call L38).
package storage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Driver stores silos' tenant data: a subtree per silo, a volume per
// Docker volume. The first implementation is plain directories.
type Driver interface {
	// Name is "dir", "zfs" or "btrfs". It is recorded in the store
	// at install and never changes for a data directory.
	Name() string

	// Guarantees says what this driver enforces: whether volume
	// sizes are hard limits, whether Snapshot works. describe shows it.
	Guarantees() Guarantees

	// Create makes a silo's subtree (ref.Volume empty) or a volume in it,
	// capped at size where the driver enforces sizes. It is
	// idempotent and returns the path the runtime mounts.
	Create(ctx context.Context, ref Ref, size int64) (path string, err error)

	// Delete removes a volume, or a silo's whole subtree. It is idempotent.
	Delete(ctx context.Context, ref Ref) error

	// Usage reports bytes and inodes in use, for metering and the
	// over-quota check.
	Usage(ctx context.Context, ref Ref) (Usage, error)

	// Snapshot is reserved for backups. It makes a read-only,
	// point-in-time copy of a volume or a subtree under name. Every v1
	// driver returns ErrSnapshotUnsupported. No v1 code path calls it.
	Snapshot(ctx context.Context, ref Ref, name string) (Snapshot, error)
}

// Ref names a silo's subtree, or a volume in it.
type Ref struct {
	Silo   string
	Volume string // empty means the silo's subtree
}

func (r Ref) String() string {
	if r.Volume == "" {
		return r.Silo
	}
	return r.Silo + "/" + r.Volume
}

// name is what a silo id or a volume id may be: one path element, and
// never "." or "..".
var name = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Valid checks that r names something a driver can keep: a silo, and
// optionally a volume, each a single path element.
func (r Ref) Valid() error {
	if !name.MatchString(r.Silo) {
		return fmt.Errorf("storage: invalid silo id %q", r.Silo)
	}
	if r.Volume != "" && !name.MatchString(r.Volume) {
		return fmt.Errorf("storage: invalid volume id %q", r.Volume)
	}
	return nil
}

// Guarantees is what a driver enforces.
type Guarantees struct {
	// EnforcedSizes is whether a volume's size is a hard limit. Where
	// it isn't, sizes are charged and Meter reports overruns.
	EnforcedSizes bool `json:"enforced_sizes"`
	// Snapshots is whether Snapshot works.
	Snapshots bool `json:"snapshots"`
}

// String is the guarantees as describe shows them: "sizes charged, not
// enforced; no snapshots" for Dir.
func (g Guarantees) String() string {
	s := "sizes charged, not enforced"
	if g.EnforcedSizes {
		s = "sizes enforced"
	}
	if g.Snapshots {
		return s + "; snapshots"
	}
	return s + "; no snapshots"
}

// Usage is what a subtree or a volume holds.
type Usage struct {
	// Bytes is the space its files take on disk.
	Bytes int64 `json:"bytes"`
	// Inodes is how many files, directories and links it holds,
	// itself included.
	Inodes int64 `json:"inodes"`
}

// Snapshot is a read-only, point-in-time copy made by Driver.Snapshot.
type Snapshot struct {
	Ref   Ref
	Name  string
	Taken time.Time
}

// ErrSnapshotUnsupported is every v1 driver's answer to Snapshot.
var ErrSnapshotUnsupported = errors.New("storage: this driver does not take snapshots")

// DefaultVolumeSize is the size a volume is charged at when it is made
// without one: 10 GiB (call L38).
const DefaultVolumeSize int64 = 10 << 30
