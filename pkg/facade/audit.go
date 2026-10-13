package facade

import (
	"context"

	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// KindFacadeCall is the audit kind of every facade write, accepted or
// refused once its silo is known.
const KindFacadeCall = "facade.call"

// Call is one facade.call record.
type Call struct {
	// Method names the call in the service's terms:
	// compute.instances.insert.
	Method string
	// RequestID is the call's idempotency key, if it has one.
	RequestID string
	// Result is "OK" or the refusal's reason.
	Result string
	// Charges are what the call charged; none is recorded as an empty
	// list.
	Charges []fenceapi.ChargeLine
	// Fields are the service's own: where the object is and its name,
	// and anything the handler adds. They are written over method and
	// charges.
	Fields map[string]any
}

// Audit writes c as a facade.call record in the audit of silo, the
// call's target, inside tx, for caller, whose Manage says how it
// reached the target. A target gone since writes nothing.
func Audit(ctx context.Context, tx store.Tx, silo string, caller fenceapi.Caller, c Call) error {
	t, err := tree.Get(ctx, tx, silo)
	if err != nil {
		return nil // gone since
	}
	charges := c.Charges
	if charges == nil {
		charges = []fenceapi.ChargeLine{}
	}
	fields := map[string]any{"method": c.Method, "charges": charges}
	for k, v := range c.Fields {
		fields[k] = v
	}
	return fenceapi.Audit(ctx, tx, t, caller, KindFacadeCall, c.RequestID, c.Result, fields)
}
