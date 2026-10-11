package fenceapi

import (
	"net/http"

	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// KindReconcileApply is the audit kind of silo reconcile --apply.
const KindReconcileApply = "reconcile.apply"

// The reconcile's mode, as /silo/v1/reconcile reports it.
const (
	ReconcileModeApply      = "apply"
	ReconcileModeReportOnly = "report-only"
)

// ReconcileMode is the answer of /silo/v1/reconcile and
// /silo/v1/reconcile:apply.
type ReconcileMode struct {
	// Mode is report-only after install adopts a restored database
	// (state.md "Backups and recovery", SV-54), until an operator runs
	// silo reconcile --apply; apply otherwise.
	Mode string `json:"mode"`
}

// reconcilePath reports whether rest is one of the reconcile's paths,
// which are silod's own rather than a fence operation on a silo: GET
// reconcile reports the mode, POST reconcile:apply leaves report-only.
// Only the root silo's callers may use them.
func reconcilePath(rest string) bool { return rest == "reconcile" || rest == "reconcile:apply" }

func (s *Server) reconcile(q *request, rest string) {
	ctx := q.r.Context()
	apply := rest == "reconcile:apply"
	switch {
	case apply && q.r.Method != http.MethodPost:
		writeError(q.w, errorf(NotFound, "reconcile:apply is POST"))
		return
	case !apply && q.r.Method != http.MethodGet:
		writeError(q.w, errorf(NotFound, "reconcile is GET"))
		return
	}
	var out ReconcileMode
	err := s.tx(ctx, func(tx store.Tx) error {
		root, err := tree.Root(ctx, tx)
		if err != nil {
			return err
		}
		if q.caller.Silo != root.ID || q.caller.Manage != "" {
			return errorf(PermissionDenied, "only the root silo's callers may see or change the reconcile")
		}
		mode, ok, err := store.ReadSetting(ctx, tx, store.SettingReconcile)
		if err != nil {
			return err
		}
		reportOnly := ok && mode == store.ReconcileReportOnly
		if apply && !q.validateOnly {
			if err := store.WriteSetting(ctx, tx, store.SettingReconcile, store.ReconcileApply); err != nil {
				return err
			}
			if err := Audit(ctx, tx, root, q.caller, KindReconcileApply, "", "OK",
				map[string]any{"was": modeName(reportOnly)}); err != nil {
				return err
			}
			reportOnly = false
		}
		out.Mode = modeName(reportOnly)
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

func modeName(reportOnly bool) string {
	if reportOnly {
		return ReconcileModeReportOnly
	}
	return ReconcileModeApply
}
