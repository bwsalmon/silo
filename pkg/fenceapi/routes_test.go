package fenceapi

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/control"
)

// TestRoutesInTable checks that every route silod serves is in the
// shared method table, and that the table names nothing silod doesn't
// serve, so the gateway and silod can't disagree about a method.
func TestRoutesInTable(t *testing.T) {
	ops := control.FenceOps()
	for op := range handlers {
		if _, ok := ops[op]; !ok {
			t.Errorf("route %s is missing from pkg/control's table", op)
		}
	}
	for op := range ops {
		if handlers[op] == nil {
			t.Errorf("the table names %s, which silod doesn't route", op)
		}
	}
}
