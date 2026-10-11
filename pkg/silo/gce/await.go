package gce

import (
	"context"

	compute "google.golang.org/api/compute/v1"
)

// Await polls op with WaitOperation until it is DONE or ctx ends, and
// returns it as it finished. A failed operation is not an error: its
// Error says how it failed. An operation already DONE is returned as it
// is.
func Await(ctx context.Context, c Operations, project string, op *compute.Operation) (*compute.Operation, error) {
	for op.Status != "DONE" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := c.WaitOperation(ctx, project, ScopeOf(op), op.Name)
		if err != nil {
			return nil, err
		}
		op = next
	}
	return op, nil
}
