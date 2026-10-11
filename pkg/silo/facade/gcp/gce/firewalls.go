package gce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	gceapi "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/fence"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	compute "google.golang.org/api/compute/v1"
)

// Firewall rules (gcp-backend.md, "Firewall rules", SV-72). A tenant's
// rule becomes the real VPC firewall rule <id>-<name> in the shared
// VPC, pinned to its silo (call 11): its targets are only ever the
// silo's own instances, and its ranges lie in the silo's own space. The
// fence runs first and passes a tenant rule only intra-silo packets, so
// the pinning is what keeps a rule from changing what another silo's
// VMs receive: without it, a rule with no target would apply to every
// instance in the VPC.
//
// The rule as the tenant wrote it is kept in facade_firewalls, and get
// and list show it rather than the intersected real rule.

func init() {
	for m, h := range map[string]handler{
		"firewalls.insert": (*Server).insertFirewall,
		"firewalls.get":    (*Server).getFirewall,
		"firewalls.list":   (*Server).listFirewalls,
		"firewalls.delete": (*Server).deleteFirewall,
		"firewalls.patch":  (*Server).replaceFirewall,
		"firewalls.update": (*Server).replaceFirewall,
	} {
		handlers[m] = h
	}
	// A failed insert forgets the original (the charge is refunded as
	// any is); a failed patch or update puts the previous one back; a
	// delete forgets it and refunds once it has worked.
	opDone["compute.firewalls.insert"] = func(s *Server, ctx context.Context, rec Operation, failed bool) error {
		if !failed {
			return nil
		}
		return s.st.Tx(ctx, func(tx store.Tx) error { return dropOriginal(ctx, tx, rec.Silo, firewallOfObject(rec)) })
	}
	settle := func(s *Server, ctx context.Context, rec Operation, failed bool) error {
		return s.st.Tx(ctx, func(tx store.Tx) error { return settleOriginal(ctx, tx, rec.Silo, firewallOfObject(rec), failed) })
	}
	opDone["compute.firewalls.patch"] = settle
	opDone["compute.firewalls.update"] = settle
	opDone["compute.firewalls.delete"] = func(s *Server, ctx context.Context, rec Operation, failed bool) error {
		if failed {
			return nil
		}
		return s.st.Tx(ctx, func(tx store.Tx) error {
			if err := dropOriginal(ctx, tx, rec.Silo, firewallOfObject(rec)); err != nil {
				return err
			}
			_, err := ledger.Refund(ctx, tx, rec.Object, nil)
			return err
		})
	}
}

// firewallsCharge is what a rule charges: one of the global count.
var firewallsCharge = []ledger.Amount{{Key: ledger.Key{Kind: "gce", Class: "firewall_rules", Region: ledger.Global}, Amount: ledger.Units(1)}}

// firewallObject is the ledger's object for a real rule.
func firewallObject(real string) string { return "firewall:" + real }

// firewallOfObject is the tenant's name of the rule rec's object names.
func firewallOfObject(rec Operation) string {
	real := strings.TrimPrefix(rec.Object, "firewall:")
	return strings.TrimPrefix(real, rec.Silo+"-")
}

// ---- Originals ----

// original is the rule as the tenant wrote it, and whether one is kept.
func original(ctx context.Context, tx store.Tx, silo, name string) (map[string]any, bool, error) {
	var b string
	err := tx.QueryRow(ctx, `SELECT original FROM facade_firewalls WHERE silo = ? AND name = ?`, silo, name).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(b), &m); err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// originals are every rule of silo's as the tenant wrote it, by name.
func originals(ctx context.Context, tx store.Tx, silo string) (map[string]map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT name, original FROM facade_firewalls WHERE silo = ?`, silo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var name, b string
		if err := rows.Scan(&name, &b); err != nil {
			return nil, err
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			return nil, err
		}
		out[name] = m
	}
	return out, rows.Err()
}

// putOriginal keeps orig as the rule's original. keepPrevious holds on
// to the one it replaces until the operation that changes the real
// rule finishes.
func putOriginal(ctx context.Context, tx store.Tx, silo, name string, orig map[string]any, keepPrevious bool) error {
	b, err := json.Marshal(orig)
	if err != nil {
		return err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	prev := ""
	if keepPrevious {
		if err := tx.QueryRow(ctx, `SELECT original FROM facade_firewalls WHERE silo = ? AND name = ?`, silo, name).Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO facade_firewalls (silo, name, original, previous, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (silo, name) DO UPDATE SET original = excluded.original, previous = excluded.previous, updated_at = excluded.updated_at`,
		silo, name, string(b), prev, store.Millis(now))
	return err
}

// settleOriginal ends a patch or update: the previous original is
// dropped if it worked, and put back if it failed.
func settleOriginal(ctx context.Context, tx store.Tx, silo, name string, failed bool) error {
	q := `UPDATE facade_firewalls SET previous = '' WHERE silo = ? AND name = ?`
	if failed {
		q = `UPDATE facade_firewalls SET original = previous, previous = '' WHERE silo = ? AND name = ? AND previous <> ''`
	}
	_, err := tx.Exec(ctx, q, silo, name)
	return err
}

func dropOriginal(ctx context.Context, tx store.Tx, silo, name string) error {
	_, err := tx.Exec(ctx, `DELETE FROM facade_firewalls WHERE silo = ? AND name = ?`, silo, name)
	return err
}

// ---- Pinning ----

// ownSpace is silo's own space: its blocks less what it has delegated
// (allocators.md, "Every silo's space map"), as the fewest prefixes.
func ownSpace(ctx context.Context, tx store.Tx, silo string) ([]netip.Prefix, error) {
	m, err := address.Map(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	blocks, err := address.Blocks(ctx, tx, silo)
	if err != nil {
		return nil, err
	}
	var delegated []netip.Prefix
	for _, r := range m {
		if r.State == address.StateDelegated {
			delegated = append(delegated, r.Range)
		}
	}
	var out []netip.Prefix
	for _, b := range blocks {
		out = append(out, fence.Minus(b, delegated...)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr().Less(out[j].Addr()) })
	return out, nil
}

// pinRanges intersects ranges, as the tenant wrote them, with own: the
// part of each that lies in the silo's own space. A bare address is its
// /32. A range that doesn't parse is 400.
func pinRanges(field string, ranges []string, own []netip.Prefix) ([]string, error) {
	var hits []netip.Prefix
	for i, s := range ranges {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, invalid("Invalid value for field 'resource.%s[%d]': '%s'. Must be a valid CIDR range or IP address.", field, i, s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		p = p.Masked()
		for _, o := range own {
			switch {
			case p.Bits() <= o.Bits() && p.Contains(o.Addr()):
				hits = append(hits, o)
			case o.Bits() <= p.Bits() && o.Contains(p.Addr()):
				hits = append(hits, p)
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Addr() != hits[j].Addr() {
			return hits[i].Addr().Less(hits[j].Addr())
		}
		return hits[i].Bits() < hits[j].Bits()
	})
	out := []string{}
	var last netip.Prefix
	for _, h := range hits {
		if last.IsValid() && last.Bits() <= h.Bits() && last.Contains(h.Addr()) {
			continue // inside one already kept
		}
		out = append(out, h.String())
		last = h
	}
	return out, nil
}

// pin builds the real rule for a tenant's rule (gcp-backend.md,
// "Firewall rules"): the name, tags and description prefixed, the
// silo's <id> tag the target when no other is, the ranges intersected
// with the silo's own space, and priority, allowed, denied, disabled
// and logConfig kept. A range given that leaves nothing once
// intersected is 400, so 0.0.0.0/0 means anywhere in this silo; a rule
// with no ranges on its far side gets the whole own space, where GCE
// would have taken 0.0.0.0/0.
func (s *Server) pin(q *request, orig map[string]any, own []netip.Prefix) (*compute.Firewall, error) {
	var in compute.Firewall
	if err := decode(orig, &in); err != nil {
		return nil, err
	}
	if err := checkName(in.Name); err != nil {
		return nil, err
	}
	if in.Network != "" {
		if _, err := q.n.inLink(in.Network, "networks"); err != nil {
			return nil, err
		}
	}
	dir := in.Direction
	if dir == "" {
		dir = "INGRESS"
	}
	if dir != "INGRESS" && dir != "EGRESS" {
		return nil, invalid("Invalid value for field 'resource.direction': '%s'. Must be INGRESS or EGRESS.", in.Direction)
	}
	real := &compute.Firewall{
		Name: q.n.realName(in.Name), Description: q.describe(in.Description), Network: s.networkLink(),
		Direction: dir, Priority: in.Priority, Allowed: in.Allowed, Denied: in.Denied,
		Disabled: in.Disabled, LogConfig: in.LogConfig,
	}
	if _, ok := orig["priority"]; ok {
		real.ForceSendFields = append(real.ForceSendFields, "Priority")
	}
	for _, t := range in.TargetTags {
		real.TargetTags = append(real.TargetTags, q.n.realName(t))
	}
	if len(real.TargetTags) == 0 {
		real.TargetTags = []string{q.target.ID}
	}
	for _, t := range in.SourceTags {
		real.SourceTags = append(real.SourceTags, q.n.realName(t))
	}
	if len(own) == 0 {
		return nil, invalid("silo: silo %s has no address space of its own for a firewall rule to apply to: it has delegated all of its blocks", q.target.Path)
	}
	ownList := make([]string, len(own))
	for i, o := range own {
		ownList[i] = o.String()
	}
	pinned := func(field string, asked []string) ([]string, error) {
		if len(asked) == 0 {
			return nil, nil
		}
		out, err := pinRanges(field, asked, own)
		if err != nil {
			return nil, err
		}
		if len(out) == 0 {
			return nil, invalid("Invalid value for field 'resource.%s': '%s'. silo: none of it lies in the own space of silo %s, which is %s",
				field, strings.Join(asked, ","), q.target.Path, strings.Join(ownList, ", "))
		}
		return out, nil
	}
	var err error
	if real.SourceRanges, err = pinned("sourceRanges", in.SourceRanges); err != nil {
		return nil, err
	}
	if real.DestinationRanges, err = pinned("destinationRanges", in.DestinationRanges); err != nil {
		return nil, err
	}
	switch {
	case dir == "INGRESS" && len(real.SourceRanges) == 0 && len(real.SourceTags) == 0:
		real.SourceRanges = ownList
	case dir == "EGRESS" && len(real.DestinationRanges) == 0:
		real.DestinationRanges = ownList
	}
	return real, nil
}

// pinTx reads the silo's own space and pins orig, in tx.
func (s *Server) pinTx(ctx context.Context, tx store.Tx, q *request, orig map[string]any) (*compute.Firewall, error) {
	own, err := ownSpace(ctx, tx, q.target.ID)
	if err != nil {
		return nil, err
	}
	return s.pin(q, orig, own)
}

// ---- Handlers ----

// ownFirewall is the silo's real rule the call names, or GCE's 404.
func (s *Server) ownFirewall(q *request, name string) (*compute.Firewall, error) {
	nf := notFound("projects/" + q.call.Project + "/global/firewalls/" + name)
	if checkName(name) != nil {
		return nil, nf
	}
	fw, err := s.gce.GetFirewall(q.ctx(), s.opts.HostProject, q.n.realName(name))
	if err != nil {
		if gceapi.IsNotFound(err) {
			return nil, nf
		}
		return nil, err
	}
	if !q.ownsFirewall(fw) {
		return nil, nf
	}
	return fw, nil
}

func (q *request) ownsFirewall(fw *compute.Firewall) bool {
	_, named := q.n.tenantName(fw.Name)
	_, described := q.tenantDescription(fw.Description)
	return named && described && gceapi.LastSegment(fw.Network) == q.n.network
}

// tenantFirewallFields are what get shows of the original rather than
// of the real rule.
var tenantFirewallFields = []string{"description", "sourceRanges", "destinationRanges", "sourceTags", "targetTags"}

// firewall is fw as the tenant sees it: the real rule's own fields
// (id, selfLink, the defaults GCE filled in), with what the tenant
// wrote over what the facade pinned.
func (q *request) firewall(fw *compute.Firewall, orig map[string]any) (map[string]any, error) {
	c := *fw
	c.Description, _ = q.tenantDescription(fw.Description)
	m, err := q.shape("compute#firewall", &c)
	if err != nil || orig == nil {
		return m, err
	}
	for _, k := range tenantFirewallFields {
		if v, ok := orig[k]; ok && !isDefault(v) {
			m[k] = v
		} else {
			delete(m, k)
		}
	}
	return m, nil
}

func (s *Server) insertFirewall(q *request) (any, error) {
	ctx := q.ctx()
	name, _ := q.body["name"].(string)
	if err := checkName(name); err != nil {
		return nil, s.refuse(q, err)
	}
	real := q.n.realName(name)
	if _, err := s.gce.GetFirewall(ctx, s.opts.HostProject, real); err == nil {
		return nil, s.refuse(q, errorf(http.StatusConflict, ReasonAlreadyExists, "The resource 'projects/%s/global/firewalls/%s' already exists", q.call.Project, name))
	} else if !gceapi.IsNotFound(err) {
		return nil, s.refuse(q, err)
	}
	object := firewallObject(real)
	var fw *compute.Firewall
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		if fw, err = s.pinTx(ctx, tx, q, q.body); err != nil {
			return err
		}
		if err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: object, Silo: q.target.ID, Via: q.via(), Amounts: firewallsCharge}); err != nil {
			return err
		}
		return putOriginal(ctx, tx, q.target.ID, name, q.body, false)
	})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.InsertFirewall(ctx, s.opts.HostProject, fw)
	if err != nil {
		_ = s.st.Tx(ctx, func(tx store.Tx) error {
			if err := dropOriginal(ctx, tx, q.target.ID, name); err != nil {
				return err
			}
			_, err := ledger.Refund(ctx, tx, object, nil)
			return err
		})
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", pinnedFields(fw), q.chargeLines(firewallsCharge))
	return s.started(q, op, object, nil)
}

// pinnedFields are what the audit records of a real rule: what the
// facade pinned the tenant's to.
func pinnedFields(fw *compute.Firewall) map[string]any {
	return map[string]any{"target_tags": fw.TargetTags, "source_ranges": fw.SourceRanges, "destination_ranges": fw.DestinationRanges}
}

func (s *Server) getFirewall(q *request) (any, error) {
	fw, err := s.ownFirewall(q, q.call.Name)
	if err != nil {
		return nil, err
	}
	var orig map[string]any
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		orig, _, err = original(q.ctx(), tx, q.target.ID, q.call.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return q.firewall(fw, orig)
}

func (s *Server) listFirewalls(q *request) (any, error) {
	l, err := s.gce.ListFirewalls(q.ctx(), s.opts.HostProject, gceapi.ListFilter{NamePrefix: q.n.prefix()})
	if err != nil {
		return nil, err
	}
	var origs map[string]map[string]any
	err = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		var err error
		origs, err = originals(q.ctx(), tx, q.target.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	var items []map[string]any
	for _, fw := range l {
		if !q.ownsFirewall(fw) {
			continue
		}
		name, _ := q.n.tenantName(fw.Name)
		m, err := q.firewall(fw, origs[name])
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return s.list(q, "compute#firewallList", items)
}

func (s *Server) deleteFirewall(q *request) (any, error) {
	fw, err := s.ownFirewall(q, q.call.Name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.DeleteFirewall(q.ctx(), s.opts.HostProject, fw.Name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", nil, nil)
	return s.started(q, op, firewallObject(fw.Name), noRefund)
}

// replaceFirewall serves patch and update. Either way the tenant's new
// original is pinned whole and the real rule replaced with the result,
// so that a field the tenant clears is cleared in the real rule too: a
// patch's original is the old one with the body's fields over it, an
// update's the body.
func (s *Server) replaceFirewall(q *request) (any, error) {
	ctx := q.ctx()
	name := q.call.Name
	if n, ok := q.body["name"].(string); ok && n != "" && n != name {
		return nil, s.refuse(q, invalid("Invalid value for field 'resource.name': '%s'. A firewall rule's name can't be changed", n))
	}
	cur, err := s.ownFirewall(q, name)
	if err != nil {
		return nil, s.refuse(q, err)
	}
	var fw *compute.Firewall
	err = s.st.Tx(ctx, func(tx store.Tx) error {
		orig := map[string]any{}
		if q.call.Method == "patch" {
			old, ok, err := original(ctx, tx, q.target.ID, name)
			if err != nil {
				return err
			}
			if !ok {
				// No original kept: the real rule as the tenant would see it.
				if old, err = q.firewall(cur, nil); err != nil {
					return err
				}
			}
			for k, v := range old {
				if k == "name" || k == "description" || k == "network" || k == "priority" || k == "direction" ||
					k == "allowed" || k == "denied" || k == "disabled" || k == "logConfig" || contains(tenantFirewallFields, k) {
					orig[k] = v
				}
			}
			for k, v := range q.body {
				if v == nil {
					delete(orig, k)
				} else {
					orig[k] = v
				}
			}
		} else {
			for k, v := range q.body {
				orig[k] = v
			}
		}
		orig["name"] = name
		var err error
		if fw, err = s.pinTx(ctx, tx, q, orig); err != nil {
			return err
		}
		return putOriginal(ctx, tx, q.target.ID, name, orig, true)
	})
	if err != nil {
		return nil, s.refuse(q, err)
	}
	op, err := s.gce.UpdateFirewall(ctx, s.opts.HostProject, cur.Name, fw)
	if err != nil {
		_ = s.st.Tx(ctx, func(tx store.Tx) error { return settleOriginal(ctx, tx, q.target.ID, name, true) })
		return nil, s.refuse(q, err)
	}
	s.audit(q, "OK", pinnedFields(fw), nil)
	return s.started(q, op, firewallObject(cur.Name), noRefund)
}

func contains(l []string, s string) bool {
	for _, e := range l {
		if e == s {
			return true
		}
	}
	return false
}
