package gce

import (
	"context"
	"errors"

	"github.com/bwsalmon/silo/pkg/manage"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// ManageGrants is the ManageChecker for the grants pkg/manage
// keeps (fence-api.md, "What a holder may do", SV-18); New uses it
// unless Options name another. A call is charged to the resource type
// in its path, so an instances.insert, boot disk and all, is covered by
// instances.
type ManageGrants struct{}

// Covers implements ManageChecker.
func (ManageGrants) Covers(ctx context.Context, tx store.Tx, caller string, target *tree.Silo, resource string, write bool) (string, error) {
	access := manage.Read
	if write {
		access = manage.Write
	}
	g, err := manage.Allows(ctx, tx, caller, target, resource, access)
	var d *manage.Denied
	if errors.As(err, &d) {
		return "", forbidden("silo: %s (grant %s)", d.Error(), d.Grant.ID)
	}
	if err != nil || g == nil {
		return "", err
	}
	return g.ID, nil
}
