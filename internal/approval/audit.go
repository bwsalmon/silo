package approval

import (
	"context"

	"github.com/bwsalmon/silo/internal/audit"
)

// WithAudit logs every request and its outcome.
func WithAudit(a Approver, log audit.Logger) Approver {
	return audited{a, log}
}

type audited struct {
	a   Approver
	log audit.Logger
}

func (x audited) Approve(ctx context.Context, req Request) Decision {
	x.log.Log("approval_requested", map[string]any{"kind": req.Kind, "title": req.Title, "body": req.Body})
	d := x.a.Approve(ctx, req)
	if d.Approved {
		x.log.Log("approved", map[string]any{"kind": req.Kind, "title": req.Title})
	} else {
		x.log.Log("rejected", map[string]any{"kind": req.Kind, "title": req.Title, "reason": d.Reason})
	}
	return d
}
