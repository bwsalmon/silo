package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// DefaultScanInterval is how often Meter measures every volume.
const DefaultScanInterval = 5 * time.Minute

// Volume is a volume as the ledger charges it.
type Volume struct {
	Ref Ref
	// Size is its declared size in bytes: what the ledger charges
	// against the silo's disk_gb.
	Size int64
}

// OverQuota is a volume using more than its charge.
type OverQuota struct {
	Volume Volume
	Usage  Usage
}

// ErrOverQuota is what an OverQuotaError matches with errors.Is.
var ErrOverQuota = errors.New("storage: volume over quota")

// OverQuotaError refuses a new container that mounts a volume over its
// charge. It is sent as 409 Conflict.
type OverQuotaError struct{ OverQuota }

func (e *OverQuotaError) Error() string {
	return fmt.Sprintf("volume %s uses %d bytes, over its size of %d bytes: a new container can't mount it until it is back under",
		e.Volume.Ref.Volume, e.Usage.Bytes, e.Volume.Size)
}

func (e *OverQuotaError) Is(target error) bool { return target == ErrOverQuota }

// HTTPStatus is the status the refusal is sent with: 409.
func (e *OverQuotaError) HTTPStatus() int { return http.StatusConflict }

// Meter measures volumes' real use against their charge, for a driver
// whose sizes aren't enforced (call L38): every Interval, and whenever
// a container that mounted one stops (Measure). A volume found over its
// size is reported once, when it goes over, and new containers that
// mount it are refused (CheckMount) until a measurement finds it back
// under. Writes to it aren't stopped.
//
// Which volumes are over is kept in memory: the first scan after a
// start finds them again, and reports each once more.
type Meter struct {
	Driver Driver
	// Volumes lists every volume there is.
	Volumes func(ctx context.Context) ([]Volume, error)
	// Report is called when a volume goes over its size: AuditOverQuota
	// writes volume_over_quota. Nil reports nothing.
	Report func(ctx context.Context, o OverQuota) error
	// Interval is how often Run scans; DefaultScanInterval if zero.
	Interval time.Duration

	mu   sync.Mutex
	over map[Ref]OverQuota
}

// Run scans now, and then every Interval until ctx is done.
func (m *Meter) Run(ctx context.Context) {
	every := m.Interval
	if every == 0 {
		every = DefaultScanInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := m.Scan(ctx); err != nil && ctx.Err() == nil {
			log.Printf("storage: scanning volumes: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Scan measures every volume. A volume that is no longer listed is
// forgotten; one whose directory is missing is skipped (the reconcile
// reports it). The errors of the others are joined, after every volume
// that could be measured has been.
func (m *Meter) Scan(ctx context.Context) error {
	vols, err := m.Volumes(ctx)
	if err != nil {
		return err
	}
	listed := map[Ref]bool{}
	var errs []error
	for _, v := range vols {
		listed[v.Ref] = true
		if _, err := m.Measure(ctx, v); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	m.mu.Lock()
	for r := range m.over {
		if !listed[r] {
			delete(m.over, r)
		}
	}
	m.mu.Unlock()
	return errors.Join(errs...)
}

// Measure measures one volume, records whether it is over its size,
// and reports it if it has just gone over.
func (m *Meter) Measure(ctx context.Context, v Volume) (Usage, error) {
	u, err := m.Driver.Usage(ctx, v.Ref)
	if err != nil {
		return Usage{}, err
	}
	m.mu.Lock()
	_, was := m.over[v.Ref]
	is := u.Bytes > v.Size
	if is {
		if m.over == nil {
			m.over = map[Ref]OverQuota{}
		}
		m.over[v.Ref] = OverQuota{Volume: v, Usage: u}
	} else {
		delete(m.over, v.Ref)
	}
	m.mu.Unlock()
	if is && !was && m.Report != nil {
		if err := m.Report(ctx, OverQuota{Volume: v, Usage: u}); err != nil {
			return u, fmt.Errorf("storage: reporting volume %s over quota: %w", v.Ref, err)
		}
	}
	return u, nil
}

// CheckMount refuses, with an *OverQuotaError, a new container that
// would mount ref while it is over its size.
func (m *Meter) CheckMount(ref Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.over[ref]; ok {
		return &OverQuotaError{o}
	}
	return nil
}

// Over lists the volumes over their size, by silo and volume, for
// describe.
func (m *Meter) Over() []OverQuota {
	m.mu.Lock()
	out := make([]OverQuota, 0, len(m.over))
	for _, o := range m.over {
		out = append(out, o)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Volume.Ref, out[j].Volume.Ref
		return a.Silo < b.Silo || a.Silo == b.Silo && a.Volume < b.Volume
	})
	return out
}

// AuditOverQuota is a Meter's Report that audits volume_over_quota on
// the volume's silo, as the root's internal call.
func AuditOverQuota(st store.Store) func(ctx context.Context, o OverQuota) error {
	return func(ctx context.Context, o OverQuota) error {
		return st.Tx(ctx, func(tx store.Tx) error {
			root, err := tree.Root(ctx, tx)
			if err != nil {
				return err
			}
			s, err := tree.Get(ctx, tx, o.Volume.Ref.Silo)
			if err != nil {
				return err
			}
			return fenceapi.Audit(ctx, tx, s, fenceapi.Caller{Silo: root.ID, Via: fenceapi.ViaInternal}, "volume_over_quota", "", "OK",
				map[string]any{"volume": o.Volume.Ref.Volume, "size_bytes": o.Volume.Size, "used_bytes": o.Usage.Bytes, "inodes": o.Usage.Inodes})
		})
	}
}
