package fenceapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Member maps a front-door identity to its silo (fence-api.md,
// "Members", SV-25).
type Member struct {
	Identity   string    `json:"identity"`
	Silo       string    `json:"silo"`
	Path       string    `json:"path"`
	UpdateTime time.Time `json:"update_time"`
}

var identityPattern = regexp.MustCompile(`^((user|serviceAccount):[^@\s/]+@[^@\s/]+\.[^@\s/]+|uid:(0|[1-9][0-9]{0,9}))$`)

// ValidIdentity reports whether id is user:<email> or
// serviceAccount:<email>, a GCP front door's identities, or uid:<n>, a
// local host user's, as the host's front doors read it from the kernel
// (SO_PEERCRED, or inet_diag on the registry's host port).
func ValidIdentity(id string) bool { return identityPattern.MatchString(id) }

// UIDIdentity is the member identity of host uid: uid:<n>, in decimal.
func UIDIdentity(uid uint32) string { return "uid:" + strconv.FormatUint(uint64(uid), 10) }

// mayMap reports whether a caller may map members of t: a proper
// ancestor of t, or, for the root, which has none, the root itself
// (its members are set by install and by other root members).
func mayMap(rel relation, t *tree.Silo) bool {
	return rel == relAncestor || (t.IsRoot() && rel == relSelf)
}

func setMember(ctx context.Context, tx store.Tx, identity string, t *tree.Silo) error {
	if !ValidIdentity(identity) {
		return errorf(InvalidArgument, "identity %q: want user:<email>, serviceAccount:<email> or uid:<n>", identity).with("field", "identity")
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO silo_members (identity, silo, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (identity) DO UPDATE SET silo = excluded.silo, updated_at = excluded.updated_at`,
		identity, t.ID, store.Millis(now))
	if err != nil {
		return err
	}
	_, err = tx.Append(ctx, "member", identity, "")
	return err
}

func readMember(ctx context.Context, tx store.Tx, identity string) (*Member, error) {
	var m Member
	var at int64
	err := tx.QueryRow(ctx, `SELECT identity, silo, updated_at FROM silo_members WHERE identity = ?`, identity).Scan(&m.Identity, &m.Silo, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.UpdateTime = store.Time(at)
	if t, err := tree.Get(ctx, tx, m.Silo); err == nil {
		m.Path = t.Path
	}
	return &m, nil
}

// MemberSilo returns the silo identity is a member of, or "" if none:
// what the front door asks.
func MemberSilo(ctx context.Context, tx store.Tx, identity string) (string, error) {
	m, err := readMember(ctx, tx, identity)
	if err != nil || m == nil {
		return "", err
	}
	return m.Silo, nil
}

func (s *Server) putMember(q *request, ref, identity string) (any, error) {
	ctx := q.r.Context()
	var out *Member
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "members")
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindMemberPut)
		if !mayMap(rel, t) {
			return errorf(PermissionDenied, "only an ancestor of %s may map its members", t.Path)
		}
		old, err := readMember(ctx, tx, identity)
		if err != nil {
			return err
		}
		fields := map[string]any{"identity": identity}
		if old != nil && old.Silo != t.ID {
			// Moving it needs an ancestor of the old silo too.
			oc, orel, err := s.resolve(ctx, tx, &q.caller, "-"+old.Silo, "members")
			if err != nil || !mayMap(orel, oc[len(oc)-1]) {
				return errorf(PermissionDenied, "%s is mapped to a silo the caller is not an ancestor of; only an ancestor of both may move it", identity).
					with("identity", identity)
			}
			fields["from"] = old.Path
		}
		if err := setMember(ctx, tx, identity, t); err != nil {
			return err
		}
		if out, err = readMember(ctx, tx, identity); err != nil {
			return err
		}
		if err := Audit(ctx, tx, t, q.caller, KindMemberPut, q.requestID, "OK", fields); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

func (s *Server) deleteMember(q *request, ref, identity string) (any, error) {
	ctx := q.r.Context()
	var out any
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "members")
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindMemberDelete)
		if !mayMap(rel, t) {
			return errorf(PermissionDenied, "only an ancestor of %s may map its members", t.Path)
		}
		m, err := readMember(ctx, tx, identity)
		if err != nil {
			return err
		}
		if m == nil || m.Silo != t.ID {
			return errorf(NotFound, "%s is not a member of %s", identity, t.Path).with("identity", identity)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM silo_members WHERE identity = ?`, identity); err != nil {
			return err
		}
		if _, err := tx.Append(ctx, "member", identity, `{"deleted":true}`); err != nil {
			return err
		}
		if err := Audit(ctx, tx, t, q.caller, KindMemberDelete, q.requestID, "OK", map[string]any{"identity": identity}); err != nil {
			return err
		}
		out = map[string]any{}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

func (s *Server) getMember(q *request, ref, identity string) {
	ctx := q.r.Context()
	var out *Member
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, "members")
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		if !mayMap(rel, t) && rel != relSelf {
			return errorf(PermissionDenied, "only %s or an ancestor may read its members", t.Path)
		}
		m, err := readMember(ctx, tx, identity)
		if err != nil {
			return err
		}
		if m == nil || m.Silo != t.ID {
			return errorf(NotFound, "%s is not a member of %s", identity, t.Path).with("identity", identity)
		}
		out = m
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

func (s *Server) listMembers(q *request, ref string) {
	ctx := q.r.Context()
	offset, size, err := page(q)
	if err != nil {
		writeError(q.w, err)
		return
	}
	out := struct {
		Members       []Member `json:"members"`
		NextPageToken string   `json:"next_page_token,omitempty"`
	}{Members: []Member{}}
	err = s.tx(ctx, func(tx store.Tx) error {
		chain, _, err := s.resolve(ctx, tx, &q.caller, ref, "members")
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		rows, err := tx.Query(ctx, `SELECT identity, updated_at FROM silo_members WHERE silo = ? ORDER BY identity LIMIT ? OFFSET ?`,
			t.ID, size+1, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m := Member{Silo: t.ID, Path: t.Path}
			var at int64
			if err := rows.Scan(&m.Identity, &at); err != nil {
				return err
			}
			m.UpdateTime = store.Time(at)
			out.Members = append(out.Members, m)
		}
		more := len(out.Members) > size
		if more {
			out.Members = out.Members[:size]
		}
		out.NextPageToken = nextToken(offset, size, more)
		return rows.Err()
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// audit is GET .../audit (SV-26): the silo itself and its ancestors
// may read its records; subtree=true adds its descendants'.
func (s *Server) audit(q *request, ref string) {
	ctx := q.r.Context()
	offset, size, err := page(q)
	if err != nil {
		writeError(q.w, err)
		return
	}
	aq := auditQuery{offset: offset, limit: size, kind: q.r.URL.Query().Get("kind"), subtree: q.r.URL.Query().Get("subtree") == "true"}
	if v := q.r.URL.Query().Get("since"); v != "" {
		if aq.since, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(q.w, errorf(InvalidArgument, "since %q: want an RFC 3339 time", v))
			return
		}
	}
	out := struct {
		Records       []AuditRecord `json:"records"`
		NextPageToken string        `json:"next_page_token,omitempty"`
	}{}
	err = s.tx(ctx, func(tx store.Tx) error {
		chain, _, err := s.resolve(ctx, tx, &q.caller, ref, "audit")
		if err != nil {
			return err
		}
		aq.silo = chain[len(chain)-1]
		recs, more, err := readAudit(ctx, tx, aq)
		if err != nil {
			return err
		}
		out.Records = recs
		if out.Records == nil {
			out.Records = []AuditRecord{}
		}
		out.NextPageToken = nextToken(offset, size, more)
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}
