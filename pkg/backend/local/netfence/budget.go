package netfence

import (
	"context"
	"fmt"

	"github.com/bwsalmon/silo/pkg/fence"
	"github.com/bwsalmon/silo/pkg/store"
)

// QuotaSetElements is the budget's name, as a refusal names it.
const QuotaSetElements = "nftables_set_elements"

// BudgetError is a change refused because one of the fence's sets
// would grow past the cap.
type BudgetError struct {
	Set         string
	Limit, Want int
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("the fence's set %s would hold %d elements, past its cap of %d", e.Set, e.Want, e.Limit)
}

// Budget reports the refusal for the fence API.
func (e *BudgetError) Budget() (quota string, limit, want int) {
	return QuotaSetElements, e.Limit, e.Want
}

// Budget is the fence API's FenceBudget for the local fence. nftables
// has no rule quota of GCE's kind (§9 "Applying"), so the only refusal
// is a set past Config.MaxSetElements, which defaults to the number of
// addresses in the root's address space.
type Budget struct {
	Config   Config
	Grants   fence.GrantSource
	Topology TopologySource
}

// CheckBudget compiles the fence a change would leave, in the change's
// own transaction, and returns a *BudgetError for one past the cap.
func (b Budget) CheckBudget(ctx context.Context, tx store.Tx) error {
	in, err := fence.Load(ctx, tx, b.Grants)
	if err != nil {
		return err
	}
	var topo *Topology
	if b.Topology != nil {
		if topo, err = b.Topology(ctx, tx); err != nil {
			return err
		}
	}
	rs, err := Compile(in, topo, b.Config)
	if err != nil {
		return err
	}
	return CheckSets(rs, b.cap(in))
}

func (b Budget) cap(in *fence.Input) int {
	if b.Config.MaxSetElements > 0 {
		return b.Config.MaxSetElements
	}
	n := 0
	for _, p := range in.Root {
		n += 1 << (32 - p.Bits())
	}
	return n
}

// CheckSets refuses a ruleset with a set past limit. A limit of 0 is no
// limit.
func CheckSets(rs *Ruleset, limit int) error {
	if limit <= 0 {
		return nil
	}
	for _, t := range rs.Tables {
		for _, s := range t.Sets {
			if len(s.Elems) > limit {
				return &BudgetError{Set: t.Family + " " + s.Name, Limit: limit, Want: len(s.Elems)}
			}
		}
	}
	return nil
}
