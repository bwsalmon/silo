package netfence

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/store"
)

// TopologySource reads silo-net's topology in the transaction the
// fence's Input is read in.
type TopologySource func(ctx context.Context, tx store.Tx) (*Topology, error)

// Static is a TopologySource that is always t.
func Static(t Topology) TopologySource {
	return func(context.Context, store.Tx) (*Topology, error) { c := t; return &c, nil }
}

// Applier compiles the fence from the database and loads it into
// silo-net (§9 "Applying"): the fence API's FenceApplier and
// FenceBudget, and the backend's Fence. Only one process should run
// Apply at a time (the leader); WaitApplied works in any, since the
// applied sequence number is in the database, as gcefence.Applier
// records it.
type Applier struct {
	Store    store.Store
	NFT      NFT
	Config   Config
	Grants   fence.GrantSource
	Topology TopologySource
	// Poll is how often WaitApplied reads the applied sequence number.
	// Zero means a second.
	Poll time.Duration
	// Guard, if set, is called in the transaction that records the
	// applied sequence number, and refuses it by returning an error.
	Guard func(ctx context.Context, tx store.Tx) error
	// OnDrift, if set, is called after an apply that found silo's
	// tables changed since the last one, with what differed and
	// whether the apply then succeeded.
	OnDrift func(ctx context.Context, drift []string, repaired bool)

	applying sync.Mutex // held by Apply
	listed   string     // the tables as listed after the last apply
	mu       sync.Mutex
	applied  uint64
	wake     chan struct{}
}

// CheckBudget implements fenceapi.FenceBudget.
func (a *Applier) CheckBudget(ctx context.Context, tx store.Tx) error {
	return Budget{Config: a.Config, Grants: a.Grants, Topology: a.Topology}.CheckBudget(ctx, tx)
}

// Apply loads the ruleset the database compiles to now, and records
// the change-log sequence number it compiled from, waking WaitApplied's
// callers. It returns that number.
func (a *Applier) Apply(ctx context.Context) (uint64, error) {
	seq, _, err := a.apply(ctx)
	return seq, err
}

// Repair is Apply for the reconcile: it also returns drift, which is
// non-empty when silo's tables in the kernel were not what this
// Applier last loaded. A silod that has loaded nothing yet finds no
// drift: its first apply replaces whatever is there.
func (a *Applier) Repair(ctx context.Context) ([]string, error) {
	_, drift, err := a.apply(ctx)
	return drift, err
}

func (a *Applier) apply(ctx context.Context) (uint64, []string, error) {
	a.applying.Lock()
	defer a.applying.Unlock()
	var drift []string
	if a.listed != "" {
		now, err := a.NFT.List(ctx)
		if err != nil {
			return 0, nil, err
		}
		if now != a.listed {
			drift = []string{fmt.Sprintf("table inet %s and table bridge %s differ from the ruleset last applied", TableName, TableName)}
		}
	}
	seq, err := a.applyOnce(ctx)
	if len(drift) > 0 && a.OnDrift != nil {
		a.OnDrift(ctx, drift, err == nil)
	}
	if err != nil {
		return 0, drift, err
	}
	a.done(seq)
	return seq, drift, nil
}

func (a *Applier) applyOnce(ctx context.Context) (uint64, error) {
	var in *fence.Input
	var topo *Topology
	var seq uint64
	if err := a.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		if in, err = fence.Load(ctx, tx, a.Grants); err != nil {
			return err
		}
		if a.Topology != nil {
			if topo, err = a.Topology(ctx, tx); err != nil {
				return err
			}
		}
		seq, err = fence.LastSeq(ctx, tx)
		return err
	}); err != nil {
		return 0, err
	}
	rs, err := Compile(in, topo, a.Config)
	if err != nil {
		return 0, err
	}
	// The listing is forgotten before the load: a load that fails half
	// way, or a listing that fails after it, leaves nothing to compare
	// the next apply with, and that apply loads the whole ruleset again.
	a.listed = ""
	if err := a.NFT.Load(ctx, rs); err != nil {
		return 0, err
	}
	listed, err := a.NFT.List(ctx)
	if err != nil {
		return 0, err
	}
	a.listed = listed
	if err := a.Store.Tx(ctx, func(tx store.Tx) error {
		if a.Guard != nil {
			if err := a.Guard(ctx, tx); err != nil {
				return err
			}
		}
		return fence.SetApplied(ctx, tx, seq)
	}); err != nil {
		return 0, err
	}
	return seq, nil
}

// done records, in this process, that the tables carry seq, and wakes
// WaitApplied's callers.
func (a *Applier) done(seq uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq > a.applied {
		a.applied = seq
	}
	if a.wake != nil {
		close(a.wake)
		a.wake = nil
	}
}

func (a *Applier) waitCh() (uint64, <-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.wake == nil {
		a.wake = make(chan struct{})
	}
	return a.applied, a.wake
}

// WaitApplied implements fenceapi.FenceApplier: it returns once the
// tables carry the change log up to seq, as Apply (in this process or
// another) records it, or when ctx ends.
func (a *Applier) WaitApplied(ctx context.Context, seq uint64) error {
	poll := a.Poll
	if poll <= 0 {
		poll = time.Second
	}
	for {
		local, wake := a.waitCh()
		if local >= seq {
			return nil
		}
		var applied uint64
		if err := a.Store.Tx(ctx, func(tx store.Tx) error {
			var err error
			applied, err = fence.Applied(ctx, tx)
			return err
		}); err != nil {
			return err
		}
		if applied >= seq {
			return nil
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// Run follows the change log and applies the fence after each change,
// several at once when they come faster than an apply takes, until ctx
// ends. It applies once at the start, which replaces whatever silo-net
// held before this silod. A failed apply is retried after retry.
func (a *Applier) Run(ctx context.Context, retry time.Duration) error {
	var since uint64
	if err := a.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		since, err = fence.Applied(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	changes, err := a.Store.Changes(ctx, since)
	if err != nil {
		return err
	}
	pending := true
	for {
		if pending {
			if _, err := a.Apply(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(retry):
				}
				continue
			}
			pending = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-changes:
			if !ok {
				return ctx.Err()
			}
			pending = true
		drain:
			for {
				select {
				case _, ok := <-changes:
					if !ok {
						break drain
					}
				default:
					break drain
				}
			}
		}
	}
}
