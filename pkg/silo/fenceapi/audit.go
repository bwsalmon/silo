package fenceapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/ids"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// AuditRetention is how long audit records are kept, in database time
// (fence-api.md, "Audit", SV-26).
const AuditRetention = 90 * 24 * time.Hour

// The audit kinds this package writes. Later parts of silod add theirs.
const (
	KindSiloCreate       = "silo.create"
	KindSiloDelete       = "silo.delete"
	KindSiloPatch        = "silo.patch"
	KindSiloSetControl   = "silo.set_control"
	KindSiloGrow         = "silo.grow"
	KindSiloAddBlock     = "silo.add_block"
	KindSiloReleaseBlock = "silo.release_block"
	KindMemberPut        = "member.put"
	KindMemberDelete     = "member.delete"
	KindControlDenied    = "control.denied"
	KindEgressConnect    = "egress.connect"
)

// AuditSilo is the silo of a record: its id, and its path at the time.
type AuditSilo struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// AuditActor is who made the call: a silo, plus the member identity
// for a front-door call.
type AuditActor struct {
	Silo   string `json:"silo"`
	Member string `json:"member,omitempty"`
}

// AuditRecord is one record.
type AuditRecord struct {
	ID        string         `json:"id"`
	Time      time.Time      `json:"time"`
	Silo      AuditSilo      `json:"silo"`
	Actor     AuditActor     `json:"actor"`
	Via       string         `json:"via,omitempty"`
	Kind      string         `json:"kind"`
	RequestID string         `json:"request_id,omitempty"`
	Result    string         `json:"result"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// ChargeLine is one line of who pays for a change: the shape of
// quota:preview's charges, and of the charges an audit record that
// moves quota carries (custom-allocators.md items 8 and 9). A negative
// amount is a refund.
type ChargeLine struct {
	Payer  string `json:"payer"`
	Home   string `json:"home,omitempty"`
	Via    string `json:"via,omitempty"`
	Kind   string `json:"kind"`
	Class  string `json:"class"`
	Region string `json:"region"`
	Amount any    `json:"amount"`
}

// Audit writes one record about silo, inside tx. The result is "OK",
// or an error code.
func Audit(ctx context.Context, tx store.Tx, silo *tree.Silo, c Caller, kind, requestID, result string, fields map[string]any) error {
	chain, err := tree.Ancestry(ctx, tx, silo.ID)
	if err != nil {
		return err
	}
	var lineage strings.Builder
	lineage.WriteString("/")
	for _, s := range chain {
		lineage.WriteString(s.ID + "/")
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	if fields == nil {
		fields = map[string]any{}
	}
	if c.Agent != "" {
		// The harness's agent is the record's agent (agents.md, §5); an
		// agent the call was about is its target_agent.
		if target, ok := fields["agent"]; ok && target != c.Agent {
			fields["target_agent"] = target
		}
		fields["agent"] = c.Agent
	}
	if c.FromToolVM != "" {
		fields["from_tool_vm"] = c.FromToolVM
	}
	fj, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	// n orders records written in the same millisecond.
	var n int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(n), 0) + 1 FROM audit`).Scan(&n); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (id, n, at, silo, silo_path, lineage, actor, member, via, kind, request_id, result, fields)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ids.New()+ids.New(), n, store.Millis(now), silo.ID, silo.Path, lineage.String(), c.Silo, c.Member,
		viaOf(chain, c), kind, requestID, result, string(fj))
	return err
}

// viaOf is how the caller reached the silo whose ancestry is chain:
// "" for the silo itself, "harness" for its agent's harness,
// "ancestor:<id>" or "manage:<holder id>".
func viaOf(chain []*tree.Silo, c Caller) string {
	target := chain[len(chain)-1]
	switch {
	case c.Manage != "":
		return "manage:" + c.Silo
	case c.Silo == target.ID && c.Via == ViaHarness:
		return "harness"
	case c.Silo == target.ID:
		return ""
	}
	for _, s := range chain[:len(chain)-1] {
		if s.ID == c.Silo {
			return "ancestor:" + c.Silo
		}
	}
	return ""
}

// PruneAudit drops audit records older than AuditRetention of database
// time, and request_id results older than RequestTTL, and returns how
// many audit records it dropped. silod's leader runs it.
func PruneAudit(ctx context.Context, st store.Store) (int64, error) {
	return PruneAuditGuarded(ctx, st, nil)
}

// PruneAuditGuarded is PruneAudit with guard called first in its
// transaction, refusing it by returning an error: the leader's epoch
// check (leader.Guard).
func PruneAuditGuarded(ctx context.Context, st store.Store, guard func(context.Context, store.Tx) error) (int64, error) {
	var n int64
	// Session transcripts are kept as long (agents.md, §8), on a schema
	// that has them.
	v, err := st.Schema(ctx)
	if err != nil {
		return 0, err
	}
	err = st.Tx(ctx, func(tx store.Tx) error {
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if n, err = tx.Exec(ctx, `DELETE FROM audit WHERE at < ?`, store.Millis(now.Add(-AuditRetention))); err != nil {
			return err
		}
		if v >= SessionSchema {
			if err := pruneSessions(ctx, tx, now); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM requests WHERE at < ? AND request_id LIKE ?`,
			store.Millis(now.Add(-RequestTTL)), requestPrefix+"%")
		return err
	})
	return n, err
}

// EverAudited reports whether the audit log holds a record of silo id
// or of one of its descendants: whether this database has known the
// silo, though it may since have been deleted.
func EverAudited(ctx context.Context, tx store.Tx, id string) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit WHERE silo = ? OR lineage LIKE ?`, id, "%/"+id+"/%").Scan(&n)
	return n > 0, err
}

type auditQuery struct {
	silo    *tree.Silo
	since   time.Time
	kind    string
	subtree bool
	offset  int
	limit   int
}

func readAudit(ctx context.Context, tx store.Tx, q auditQuery) ([]AuditRecord, bool, error) {
	where := `(silo = ?`
	args := []any{q.silo.ID}
	if q.subtree {
		where = `(lineage LIKE ?`
		args = []any{"%/" + q.silo.ID + "/%"}
	}
	// A manage holder's own audit shows the records of what it did
	// through its grants, which are written under their targets
	// (SV-22).
	where += ` OR (actor = ? AND via = ?))`
	args = append(args, q.silo.ID, "manage:"+q.silo.ID)
	if !q.since.IsZero() {
		where += ` AND at >= ?`
		args = append(args, store.Millis(q.since))
	}
	if q.kind != "" {
		if p, ok := strings.CutSuffix(q.kind, "*"); ok {
			where += ` AND kind LIKE ?`
			args = append(args, p+"%")
		} else {
			where += ` AND kind = ?`
			args = append(args, q.kind)
		}
	}
	args = append(args, q.limit+1, q.offset)
	rows, err := tx.Query(ctx, `SELECT id, at, silo, silo_path, actor, member, via, kind, request_id, result, fields
		FROM audit WHERE `+where+` ORDER BY n LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var at int64
		var fields string
		if err := rows.Scan(&r.ID, &at, &r.Silo.ID, &r.Silo.Path, &r.Actor.Silo, &r.Actor.Member, &r.Via,
			&r.Kind, &r.RequestID, &r.Result, &fields); err != nil {
			return nil, false, err
		}
		r.Time = store.Time(at)
		if err := json.Unmarshal([]byte(fields), &r.Fields); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	more := len(out) > q.limit
	if more {
		out = out[:q.limit]
	}
	return out, more, rows.Err()
}
