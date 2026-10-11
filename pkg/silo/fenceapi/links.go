package fenceapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/ids"
	"github.com/bwsalmon/silo/pkg/silo/links"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// The audit kinds of links (fence-api.md, "Audit", SV-26). Suspension
// and resumption are the shared grant.suspend and grant.resume.
const (
	KindLinkPropose  = "link.propose"
	KindLinkActivate = "link.activate"
	KindLinkDelete   = "link.delete"
)

// GrantLinks is the grant_policy key links are allowed by.
const GrantLinks = "links"

// LinkChange is a "link" change-log entry's data.
type LinkChange struct {
	Proposal string `json:"proposal"`
	Silo     string `json:"silo"`
	Link     string `json:"link,omitempty"`
	What     string `json:"what"` // propose, activate, delete, suspend, resume
}

func appendLinkChange(ctx context.Context, tx store.Tx, d LinkChange) (uint64, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return 0, err
	}
	return tx.Append(ctx, links.ChangeKind, d.Proposal, string(b))
}

// LinksAllowed reports whether silo id's effective grant policy allows
// links: its own and every ancestor's do (SV-13).
func LinksAllowed(ctx context.Context, tx store.Tx, id string) (bool, error) {
	chain, err := tree.Ancestry(ctx, tx, id)
	if err != nil {
		return false, err
	}
	eff, err := effectiveOf(ctx, tx, chain, nil)
	if err != nil {
		return false, err
	}
	return eff.Grant[GrantLinks] == "allow", nil
}

// LinkObject is a link, as one side sees it: its own proposal, and the
// link's state.
type LinkObject struct {
	ID         string       `json:"id,omitempty"` // the link's id, once matched
	Name       string       `json:"name"`
	Silo       string       `json:"silo"`
	Peer       string       `json:"peer"`
	Direction  string       `json:"direction"`
	Rules      []links.Rule `json:"rules"`
	State      string       `json:"state"`
	FenceRules []string     `json:"fence_rules,omitempty"`
	CreateTime time.Time    `json:"create_time"`
}

func linkObject(ctx context.Context, tx store.Tx, l *links.Link) (*LinkObject, error) {
	t, err := tree.Get(ctx, tx, l.Silo)
	if err != nil {
		return nil, err
	}
	o := &LinkObject{ID: l.LinkID, Name: l.Name, Silo: t.Path, Peer: l.PeerPath, Direction: l.Direction,
		Rules: l.Rules, State: l.State, CreateTime: l.Created}
	if o.Rules == nil {
		o.Rules = []links.Rule{}
	}
	if l.State == links.StateActive {
		out, in, revOut, revIn := links.RuleNames(l.LinkID)
		o.FenceRules = []string{out, in}
		if l.Direction == links.Both {
			o.FenceRules = append(o.FenceRules, revOut, revIn)
		}
	}
	return o, nil
}

// linkRequest is POST .../links.
type linkRequest struct {
	Name      string       `json:"name"`
	Peer      string       `json:"peer"`
	Direction string       `json:"direction"`
	Rules     []links.Rule `json:"rules"`
}

// auditLink records a link event under both sides, each of which reads
// its own audit. b may be nil, for a side not (or no longer) matched.
func auditLink(ctx context.Context, tx store.Tx, a, b *links.Link, c Caller, kind, requestID string, extra map[string]any) error {
	for _, l := range []*links.Link{a, b} {
		if l == nil {
			continue
		}
		t, err := tree.Get(ctx, tx, l.Silo)
		if errors.Is(err, tree.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		fields := map[string]any{"name": l.Name, "proposal": l.ID, "peer": l.PeerPath, "direction": l.Direction, "rules": l.Rules}
		if l.LinkID != "" {
			fields["link"] = l.LinkID
		}
		for k, v := range extra {
			fields[k] = v
		}
		if err := Audit(ctx, tx, t, c, kind, requestID, "OK", fields); err != nil {
			return err
		}
	}
	return nil
}

// linkState is the state a matched link between a's and b's silos
// takes: ACTIVE if both sides' effective grant policies allow links,
// SUSPENDED otherwise, with the sides whose policy doesn't.
func linkState(ctx context.Context, tx store.Tx, a, b string) (string, []string, error) {
	var denied []string
	for _, id := range []string{a, b} {
		ok, err := LinksAllowed(ctx, tx, id)
		if errors.Is(err, tree.ErrNotFound) {
			ok, err = false, nil
		}
		if err != nil {
			return "", nil, err
		}
		if !ok {
			denied = append(denied, id)
		}
	}
	if len(denied) > 0 {
		sort.Strings(denied)
		return links.StateSuspended, denied, nil
	}
	return links.StateActive, nil, nil
}

// proposeLink is POST .../links (SV-15): this side's half of a link. It
// is PENDING until the peer proposes the mirror image, and the call
// that completes the pair returns once the fence carries its rules.
func (s *Server) proposeLink(q *request, ref string) (any, error) {
	ctx := q.r.Context()
	var req linkRequest
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	if !tree.ValidName(req.Name) {
		return nil, errorf(InvalidArgument, "name %q: want a DNS label", req.Name).with("field", "name")
	}
	switch req.Direction {
	case links.Outbound, links.Inbound, links.Both:
	default:
		return nil, errorf(InvalidArgument, "direction %q: want outbound, inbound or both", req.Direction).with("field", "direction")
	}
	if req.Peer == "" {
		return nil, errorf(InvalidArgument, "peer is required: the other silo's path").with("field", "peer")
	}
	rules, err := links.Canonical(req.Rules)
	if err != nil {
		return nil, errorf(InvalidArgument, "rules: %v", err).with("field", "rules")
	}
	var out *LinkObject
	var seq uint64
	err = s.tx(ctx, func(tx store.Tx) error {
		t, err := s.grantSilo(ctx, tx, q, ref)
		if err != nil {
			return err
		}
		q.note(t, KindLinkPropose)
		if ok, err := LinksAllowed(ctx, tx, t.ID); err != nil {
			return err
		} else if !ok {
			return errorf(PermissionDenied, "%s's effective grant_policy doesn't allow links", t.Path).with("kind", GrantLinks)
		}
		peer := cleanPath(req.Peer)
		peerID := ""
		// Whether the path holds a silo is never told the proposer: a
		// proposal to nobody is kept, and shown, like any other.
		if o, err := tree.ByPath(ctx, tx, peer); err == nil {
			peerID = o.ID
		} else if !errors.Is(err, tree.ErrNotFound) {
			return err
		}
		if peerID == t.ID {
			return errorf(InvalidArgument, "%s can't link to itself", t.Path).with("field", "peer")
		}
		l := &links.Link{ID: ids.New(), Silo: t.ID, Name: req.Name, PeerPath: peer, PeerSilo: peerID,
			Direction: req.Direction, Rules: rules, State: links.StatePending}
		old, err := links.ByName(ctx, tx, t.ID, req.Name)
		switch {
		case errors.Is(err, links.ErrNotFound):
		case err != nil:
			return err
		default:
			if old.PeerPath != l.PeerPath || old.PeerSilo != l.PeerSilo || old.Direction != l.Direction || !sameRules(old.Rules, l.Rules) {
				return errorf(AlreadyExists, "%s already has a link %s, proposed otherwise; delete it first", t.Path, req.Name).with("name", req.Name)
			}
			// The same proposal again changes nothing.
			if out, err = linkObject(ctx, tx, old); err != nil {
				return err
			}
			return s.finish(ctx, tx, q, out)
		}
		if err := links.Create(ctx, tx, l); err != nil {
			return err
		}
		if err := auditLink(ctx, tx, l, nil, q.caller, KindLinkPropose, q.requestID, nil); err != nil {
			return err
		}
		if seq, err = appendLinkChange(ctx, tx, LinkChange{Proposal: l.ID, Silo: t.ID, What: "propose"}); err != nil {
			return err
		}
		// The peer's mirror image, if it has proposed it, completes the
		// link. A peer proposal under the same name that isn't the mirror
		// image leaves both PENDING.
		p, err := links.Partner(ctx, tx, l)
		if err != nil && !errors.Is(err, links.ErrNotFound) {
			return err
		}
		if p != nil && p.LinkID == "" && links.Mirrors(l, p) {
			state, denied, err := linkState(ctx, tx, l.Silo, p.Silo)
			if err != nil {
				return err
			}
			// The earlier proposal's id names the link and its rules.
			for _, x := range []*links.Link{l, p} {
				if err := links.SetMatch(ctx, tx, x.ID, p.ID, state); err != nil {
					return err
				}
				x.LinkID, x.State = p.ID, state
			}
			f := map[string]any{"state": state}
			if len(denied) > 0 {
				f["denied_by"] = denied
			}
			if err := auditLink(ctx, tx, l, p, q.caller, KindLinkActivate, q.requestID, f); err != nil {
				return err
			}
			if seq, err = appendLinkChange(ctx, tx, LinkChange{Proposal: l.ID, Silo: t.ID, Link: p.ID, What: "activate"}); err != nil {
				return err
			}
			if err := s.checkBudget(ctx, tx); err != nil {
				return err
			}
		}
		if out, err = linkObject(ctx, tx, l); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, seq, err)
}

func sameRules(a, b []links.Rule) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

// unlink drops l and returns its partner, if matched, to PENDING: its
// proposal stands, and the link comes back if the other side proposes
// it again. Both sides' audit records it.
func unlink(ctx context.Context, tx store.Tx, l *links.Link, c Caller, requestID string, extra map[string]any) (uint64, error) {
	var p *links.Link
	if l.LinkID != "" {
		var err error
		p, err = links.Partner(ctx, tx, l)
		if err != nil && !errors.Is(err, links.ErrNotFound) {
			return 0, err
		}
	}
	if err := links.Delete(ctx, tx, l.ID); err != nil {
		return 0, err
	}
	if err := auditLink(ctx, tx, l, p, c, KindLinkDelete, requestID, extra); err != nil {
		return 0, err
	}
	if p != nil {
		if err := links.SetMatch(ctx, tx, p.ID, "", links.StatePending); err != nil {
			return 0, err
		}
	}
	return appendLinkChange(ctx, tx, LinkChange{Proposal: l.ID, Silo: l.Silo, Link: l.LinkID, What: "delete"})
}

// deleteLink is DELETE .../links/{name}: either side ends the link, and
// the fence's rules for it go before this returns. Established
// connections then fail at their next packet.
func (s *Server) deleteLink(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var out *LinkObject
	var seq uint64
	err := s.tx(ctx, func(tx store.Tx) error {
		t, err := s.grantSilo(ctx, tx, q, ref)
		if err != nil {
			return err
		}
		q.note(t, KindLinkDelete)
		l, err := links.ByName(ctx, tx, t.ID, name)
		if errors.Is(err, links.ErrNotFound) {
			return errorf(NotFound, "%s has no link %q", t.Path, name).with("name", name)
		}
		if err != nil {
			return err
		}
		if out, err = linkObject(ctx, tx, l); err != nil {
			return err
		}
		out.FenceRules = nil
		if seq, err = unlink(ctx, tx, l, q.caller, q.requestID, map[string]any{"by": t.ID}); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, seq, err)
}

// listLinks is GET .../links: the silo's own proposals, matched or not.
func (s *Server) listLinks(q *request, ref string) {
	ctx := q.r.Context()
	offset, size, err := page(q)
	if err != nil {
		writeError(q.w, err)
		return
	}
	var items []any
	err = s.tx(ctx, func(tx store.Tx) error {
		t, err := s.grantSilo(ctx, tx, q, ref)
		if err != nil {
			return err
		}
		ls, err := links.Of(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		for _, l := range ls {
			o, err := linkObject(ctx, tx, l)
			if err != nil {
				return err
			}
			items = append(items, o)
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	out := map[string]any{"links": []any{}}
	if offset < len(items) {
		end := min(len(items), offset+size)
		out["links"] = items[offset:end]
		if end < len(items) {
			out["next_page_token"] = nextToken(offset, size, true)
		}
	}
	writeJSON(q.w, http.StatusOK, out)
}

// syncLinks is SyncGrants for links: a matched link either side's
// effective policy no longer allows is SUSPENDED, which takes its fence
// rules away, and one both allow again is ACTIVE.
func syncLinks(ctx context.Context, tx store.Tx, c Caller, requestID string) (uint64, error) {
	pairs, err := links.Pairs(ctx, tx)
	if err != nil {
		return 0, err
	}
	var seq uint64
	for _, p := range pairs {
		a, b := p[0], p[1]
		want, denied, err := linkState(ctx, tx, a.Silo, b.Silo)
		if err != nil {
			return 0, err
		}
		if want == a.State && want == b.State {
			continue
		}
		for _, x := range []*links.Link{a, b} {
			if err := links.SetMatch(ctx, tx, x.ID, x.LinkID, want); err != nil {
				return 0, err
			}
			x.State = want
		}
		kind, what := KindGrantSuspend, "suspend"
		if want == links.StateActive {
			kind, what = KindGrantResume, "resume"
		}
		f := map[string]any{"grant": "link", "state": want}
		if len(denied) > 0 {
			f["denied_by"] = denied
		}
		if err := auditLink(ctx, tx, a, b, c, kind, requestID, f); err != nil {
			return 0, err
		}
		if seq, err = appendLinkChange(ctx, tx, LinkChange{Proposal: a.ID, Silo: a.Silo, Link: a.LinkID, What: what}); err != nil {
			return 0, err
		}
	}
	return seq, nil
}

// endLinksFor deletes silo t's proposals, returning their partners to
// PENDING, and forgets t as anyone's peer, as t is deleted.
func endLinksFor(ctx context.Context, tx store.Tx, t *tree.Silo, c Caller, requestID string) error {
	ls, err := links.Of(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	for _, l := range ls {
		if _, err := unlink(ctx, tx, l, c, requestID, map[string]any{"reason": "silo deleted"}); err != nil {
			return err
		}
	}
	return links.ForgetPeer(ctx, tx, t.ID)
}
