// Package hostcap is the root silo's capacity of the local backend's
// classes, read from the host (local-backend.md §11, "Root capacity and
// overcommit", call L20): (host total − reserve) × overcommit. install's
// root step sizes the root with Capacity, and silod's leader calls
// Refresh every hour after, as rootcap does on GCP, so that a host given
// more CPUs, memory or disk, or a cache budget that changed, moves the
// root's capacity with it.
//
// A Refresh sets vcpu_m, memory_mb and disk_gb: the classes the host's
// totals bound. The count classes (containers, networks, volumes) and
// the rest of what bootstrap set are left alone. A host that shrank
// can't take back what the tree already holds: a class whose new
// capacity would fall below the root's delegated plus used keeps the
// capacity it had, and Refresh says so, rather than over-committing the
// root as a GCE quota cut does. Refresh logs quota.root_resync whenever
// it changes the root's capacity or refuses to.
package hostcap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Kind is the class kind of the local classes (call L19).
const Kind = "local"

// The classes the host's totals bound, which Refresh sets.
const (
	ClassVCPUm    = "vcpu_m"
	ClassMemoryMB = "memory_mb"
	ClassDiskGB   = "disk_gb"
)

// CPU overcommit (call L20): install's --cpu-overcommit, and silod's.
// Memory and disk aren't overcommitted.
const (
	DefaultCPUOvercommit = 2.0
	MaxCPUOvercommit     = 8.0
)

// Counts are the root's count classes, which the host's totals don't
// bound: as many as any one machine is likely to want. Bootstrap sets
// them; Refresh doesn't.
var Counts = map[string]int64{"containers": 1000, "networks": 256, "volumes": 1000}

// Totals are what the host has: its online CPUs, MemTotal in MiB, and
// the data directory's filesystem in GiB.
type Totals struct {
	CPUs     int   `json:"cpus"`
	MemoryMB int64 `json:"memory_mb"`
	DiskGB   int64 `json:"disk_gb"`
}

// Reserve is what install keeps back besides call L20's fixed reserve
// (1 vCPU, 2 GiB or 10% of memory, 10% of disk): the CPU overcommit,
// and the registry cache's budget, which is charged to nobody
// (local-registry.md call L74) and so is carved from disk_gb.
type Reserve struct {
	// CPUOvercommit is the vcpu_m overcommit; 0 is DefaultCPUOvercommit.
	CPUOvercommit float64
	// CacheGB is the registry cache's budget, in GiB.
	CacheGB float64
}

// Overcommit is r's CPU overcommit, DefaultCPUOvercommit when unset.
func (r Reserve) Overcommit() float64 {
	if r.CPUOvercommit == 0 {
		return DefaultCPUOvercommit
	}
	return r.CPUOvercommit
}

// Capacity is the root's capacity of the classes t bounds, in whole
// units: vcpu_m is (CPUs − 1) × 1000 × the overcommit, memory_mb is
// MemTotal less the larger of 2 GiB and 10%, and disk_gb is the data
// filesystem less 10% and the cache's budget, so that every byte on
// disk is charged to some silo or inside the budget.
func Capacity(t Totals, r Reserve) map[string]int64 {
	return map[string]int64{
		ClassVCPUm:    int64(float64(max(0, t.CPUs-1)*1000) * r.Overcommit()),
		ClassMemoryMB: max(0, t.MemoryMB-max(2048, t.MemoryMB/10)),
		ClassDiskGB:   max(0, t.DiskGB-(t.DiskGB+9)/10-int64(math.Ceil(r.CacheGB))),
	}
}

// MemTotalMB is /proc/meminfo's MemTotal, in MiB; 0 if it has none.
func MemTotalMB(meminfo string) int64 {
	for _, line := range strings.Split(meminfo, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb / 1024
		}
	}
	return 0
}

// ReadHost reads this host's totals, with dataDir's filesystem as the
// disk.
func ReadHost(dataDir string) (Totals, error) {
	t := Totals{CPUs: runtime.NumCPU()}
	var err error
	if t.MemoryMB, err = memTotal(); err != nil {
		return Totals{}, err
	}
	if t.DiskGB, err = diskGB(dataDir); err != nil {
		return Totals{}, err
	}
	return t, nil
}

// Config is what Refresh needs besides the database.
type Config struct {
	// DataDir is the data directory, whose filesystem is the disk.
	DataDir string
	// CPUOvercommit is install's --cpu-overcommit, which silod is given
	// as its own; 0 is DefaultCPUOvercommit.
	CPUOvercommit float64
	// Read reads the host's totals; nil is ReadHost(DataDir).
	Read func() (Totals, error)
	// Guard, if set, is called in the transaction that writes the root's
	// capacity, once there is a root and before anything is read for the
	// write, and refuses the write by returning an error:
	// silod's leader checks its epoch there (leader.Guard).
	Guard func(ctx context.Context, tx store.Tx) error
}

// Refresher sets the root's capacity of the local classes from the
// host.
type Refresher struct {
	db     store.Store
	config Config
}

// New returns a Refresher.
func New(db store.Store, config Config) *Refresher {
	return &Refresher{db: db, config: config}
}

// ErrHeld is a Refresh that left a class's capacity as it was because
// the new one would fall below what the tree holds.
var ErrHeld = errors.New("hostcap: the host's capacity is below what the tree holds")

// Result is what a Refresh did.
type Result struct {
	// Totals are the host's, as read.
	Totals Totals
	// Reserve is what was kept back: the overcommit, and the cache's
	// budget as the store has it.
	Reserve Reserve
	// Capacity is the capacity the host gives, every class Refresh
	// owns, whether set or refused.
	Capacity []ledger.Amount
	// Changed are the keys whose capacity this read changed.
	Changed []ledger.Key
	// Refused are the root's rows, as they stay, whose new capacity
	// would have fallen below their delegated plus used.
	Refused []ledger.Row
}

// Refresh reads the host's totals and sets the root's capacity from
// them. A failed read changes nothing. A class whose new capacity would
// put what the tree holds over keeps its old one; the rest are still
// set, and Refresh returns the Result with an error wrapping ErrHeld.
// Before bootstrap it returns tree.ErrNotFound.
func (r *Refresher) Refresh(ctx context.Context) (*Result, error) {
	read := r.config.Read
	if read == nil {
		read = func() (Totals, error) { return ReadHost(r.config.DataDir) }
	}
	totals, err := read()
	if err != nil {
		return nil, fmt.Errorf("hostcap: %w", err)
	}
	res := &Result{Totals: totals}
	err = r.db.Tx(ctx, func(tx store.Tx) error {
		res.Capacity, res.Changed, res.Refused = nil, nil, nil
		root, err := tree.Root(ctx, tx)
		if err != nil {
			return err
		}
		if r.config.Guard != nil {
			if err := r.config.Guard(ctx, tx); err != nil {
				return err
			}
		}
		cache, err := fenceapi.ReadCacheSettings(ctx, tx)
		if err != nil {
			return err
		}
		res.Reserve = Reserve{CPUOvercommit: r.config.CPUOvercommit, CacheGB: cache.SizeLimitGB}
		caps := Capacity(totals, res.Reserve)
		classes := make([]string, 0, len(caps))
		for c := range caps {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		var set []ledger.Amount
		for _, c := range classes {
			a := ledger.Amount{Key: Key(c), Amount: ledger.Units(caps[c])}
			res.Capacity = append(res.Capacity, a)
			row, err := ledger.Get(ctx, tx, root.ID, a.Key)
			if err != nil {
				return err
			}
			switch {
			case a.Amount == row.Granted:
			case a.Amount < row.Granted && a.Amount < row.Delegated+row.Used:
				res.Refused = append(res.Refused, row)
			default:
				set = append(set, a)
				res.Changed = append(res.Changed, a.Key)
			}
		}
		if len(set) > 0 {
			if err := ledger.SetCapacity(ctx, tx, root.ID, set); err != nil {
				return err
			}
		}
		if len(res.Changed) == 0 && len(res.Refused) == 0 {
			return nil
		}
		return audit(ctx, tx, root.ID, res)
	})
	if err != nil {
		return nil, fmt.Errorf("hostcap: %w", err)
	}
	if len(res.Refused) > 0 {
		var held []string
		for _, row := range res.Refused {
			held = append(held, fmt.Sprintf("%s holds %s", row.Class, ledger.FormatAmount(row.Delegated+row.Used)))
		}
		return res, fmt.Errorf("%w (%s): their capacity is left as it was", ErrHeld, strings.Join(held, ", "))
	}
	return res, nil
}

// Key is a local class's quota row key: every one is global.
func Key(class string) ledger.Key { return ledger.Key{Kind: Kind, Class: class, Region: ledger.Global} }

// audit logs quota.root_resync: the host's totals, what changed, and
// what was refused.
func audit(ctx context.Context, tx store.Tx, root string, res *Result) error {
	type change struct {
		Class   string `json:"class"`
		Granted string `json:"granted"`
	}
	type refused struct {
		Class   string `json:"class"`
		Granted string `json:"granted"`
		Wanted  string `json:"wanted"`
		Held    string `json:"held"`
	}
	amounts := map[ledger.Key]int64{}
	for _, a := range res.Capacity {
		amounts[a.Key] = a.Amount
	}
	rec := struct {
		Host          Totals    `json:"host"`
		CPUOvercommit float64   `json:"cpu_overcommit"`
		CacheGB       float64   `json:"cache_gb"`
		Changed       []change  `json:"changed,omitempty"`
		Refused       []refused `json:"refused,omitempty"`
	}{Host: res.Totals, CPUOvercommit: res.Reserve.Overcommit(), CacheGB: res.Reserve.CacheGB}
	for _, k := range res.Changed {
		rec.Changed = append(rec.Changed, change{k.Class, ledger.FormatAmount(amounts[k])})
	}
	for _, row := range res.Refused {
		rec.Refused = append(rec.Refused, refused{row.Class, ledger.FormatAmount(row.Granted),
			ledger.FormatAmount(amounts[row.Key]), ledger.FormatAmount(row.Delegated + row.Used)})
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = tx.Append(ctx, "quota.root_resync", root, string(data))
	return err
}
