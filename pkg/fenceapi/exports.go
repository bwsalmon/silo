package fenceapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/exports"
	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/imagerepos"
	"github.com/bwsalmon/silo/pkg/ledger/address"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The audit kinds of grants (fence-api.md, "Audit", SV-26).
const (
	KindExportOffer    = "export.offer"
	KindExportAccept   = "export.accept"
	KindExportWithdraw = "export.withdraw"
	KindExportCall     = "export.call"
	KindGrantSuspend   = "grant.suspend"
	KindGrantResume    = "grant.resume"
)

// GrantExports is the grant_policy key exports are allowed by.
const GrantExports = "exports"

// opGrants is what a manage holder needs for a grant call on the
// target's behalf: the grants group.
const opGrants Op = "grants"

// InstanceResolver finds the primary internal IP of silo's instance
// name in zone, for an export whose target is an instance. silod's is
// the host project's GCE; nil refuses instance targets.
type InstanceResolver func(ctx context.Context, silo, zone, name string) (netip.Addr, error)

// ExportChange is an "export" change-log entry's data.
type ExportChange struct {
	Export   string `json:"export"`
	Exporter string `json:"exporter"`
	Import   string `json:"import,omitempty"`
	Importer string `json:"importer,omitempty"`
	What     string `json:"what"` // offer, accept, withdraw, suspend, resume, target
}

func appendExportChange(ctx context.Context, tx store.Tx, d ExportChange) (uint64, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return 0, err
	}
	return tx.Append(ctx, exports.ChangeKind, d.Export, string(b))
}

// ExportsAllowed reports whether silo id's effective grant policy
// allows exports: its own and every ancestor's do (SV-13).
func ExportsAllowed(ctx context.Context, tx store.Tx, id string) (bool, error) {
	chain, err := tree.Ancestry(ctx, tx, id)
	if err != nil {
		return false, err
	}
	eff, err := effectiveOf(ctx, tx, chain, nil)
	if err != nil {
		return false, err
	}
	return eff.Grant[GrantExports] == "allow", nil
}

// exportTarget is an export's target as the API shows it.
type exportTarget struct {
	Instance string `json:"instance,omitempty"`
	Zone     string `json:"zone,omitempty"`
	Port     int    `json:"port,omitempty"`
}

// ExportTo is one silo an export is offered to.
type ExportTo struct {
	Silo    string `json:"silo"`
	State   string `json:"state"`
	Import  string `json:"import,omitempty"`
	Address string `json:"address,omitempty"`
}

// ExportObject is an export, as its exporter sees it.
type ExportObject struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Exporter      string     `json:"exporter"`
	ExporterPath  string     `json:"exporter_path"`
	Owner         string     `json:"owner"`
	Protocol      string     `json:"protocol"`
	Target        any        `json:"target"`
	Address       string     `json:"address_ip,omitempty"`
	ProxyProtocol bool       `json:"proxy_protocol"`
	Repos         []string   `json:"repos,omitempty"`
	To            []ExportTo `json:"to"`
	CreateTime    time.Time  `json:"create_time"`
}

// ImportObject is an import, as its importer sees it, or an offer not
// yet accepted (state PENDING, no id or address). A registry import's
// Repos are the names its importer pulls by: the exporter's own,
// registry.silo.internal/<exporter path>/<repo>, a prefix keeping its
// "*".
type ImportObject struct {
	ID            string   `json:"id,omitempty"`
	Export        string   `json:"export"` // <exporter path>/<name>
	ExportID      string   `json:"export_id"`
	Exporter      string   `json:"exporter"`
	Importer      string   `json:"importer"`
	Name          string   `json:"name"`
	Protocol      string   `json:"protocol"`
	ProxyProtocol bool     `json:"proxy_protocol"`
	State         string   `json:"state"`
	Address       string   `json:"address,omitempty"`
	Port          int      `json:"port,omitempty"`
	Repos         []string `json:"repos,omitempty"`
}

func exportObject(ctx context.Context, tx store.Tx, e *exports.Export) (*ExportObject, error) {
	ex, err := tree.Get(ctx, tx, e.Exporter)
	if err != nil {
		return nil, err
	}
	o := &ExportObject{ID: e.ID, Name: e.Name, Exporter: e.Exporter, ExporterPath: ex.Path, Owner: e.Owner,
		Protocol: e.Protocol, ProxyProtocol: e.ProxyProtocol, Repos: e.Repos, To: []ExportTo{}, CreateTime: e.Created}
	switch {
	case e.Protocol == exports.Registry:
		// No target: its repos are what it exports.
	case e.Instance != "":
		o.Target = exportTarget{Instance: e.Instance, Zone: e.Zone, Port: e.TargetPort}
		if t := e.Target(); t.IsValid() {
			o.Address = t.String()
		}
	default:
		o.Target = e.Target().String()
	}
	offers, err := exports.Offers(ctx, tx, e.ID)
	if err != nil {
		return nil, err
	}
	for _, of := range offers {
		to := ExportTo{Silo: of.ToPath, State: exports.StatePending}
		if of.ToSilo != "" {
			im, err := exports.ImportOf(ctx, tx, e.ID, of.ToSilo)
			if err != nil && !errors.Is(err, exports.ErrNotFound) {
				return nil, err
			}
			if im != nil {
				to.State, to.Import, to.Address = im.State, im.ID, exports.Address(e, im)
			}
		}
		o.To = append(o.To, to)
	}
	return o, nil
}

func importObject(ctx context.Context, tx store.Tx, e *exports.Export, importer string, im *exports.Import) (*ImportObject, error) {
	ex, err := tree.Get(ctx, tx, e.Exporter)
	if err != nil {
		return nil, err
	}
	o := &ImportObject{Export: ex.Path + "/" + e.Name, ExportID: e.ID, Exporter: e.Exporter, Importer: importer, Name: e.Name,
		Protocol: e.Protocol, ProxyProtocol: e.ProxyProtocol, State: exports.StatePending}
	if ex.IsRoot() {
		o.Export = "/" + e.Name
	}
	for _, r := range e.Repos {
		o.Repos = append(o.Repos, imagerepos.Ref(ex.Path, r))
	}
	if im != nil {
		o.ID, o.State, o.Address, o.Port = im.ID, im.State, exports.Address(e, im), im.Port
	}
	return o, nil
}

// grantSilo resolves the silo a grant call acts as: the caller itself
// under self, or {silo} for a proper ancestor or a manage holder with
// the grants group (SV-16), which acts exactly as if {silo} made it.
func (s *Server) grantSilo(ctx context.Context, tx store.Tx, q *request, ref string) (*tree.Silo, error) {
	chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, opGrants)
	if err != nil {
		return nil, err
	}
	if rel == relNone {
		return nil, notFound(ref)
	}
	return chain[len(chain)-1], nil
}

// offerRequest is POST .../exports.
type offerRequest struct {
	Name          string          `json:"name"`
	Target        json.RawMessage `json:"target"`
	Protocol      string          `json:"protocol"`
	To            []string        `json:"to"`
	ProxyProtocol bool            `json:"proxy_protocol"`
	Repos         []string        `json:"repos"`
}

// parseTarget reads an export's target: "ip:port", or an instance,
// zone and port.
func (s *Server) parseTarget(ctx context.Context, t *tree.Silo, raw json.RawMessage, e *exports.Export) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errorf(InvalidArgument, "target is required: \"ip:port\", or {\"instance\", \"zone\", \"port\"}").with("field", "target")
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		ap, err := netip.ParseAddrPort(str)
		if err != nil || !ap.Addr().Is4() || ap.Port() == 0 {
			return errorf(InvalidArgument, "target %q: want an IPv4 ip:port", str).with("field", "target")
		}
		e.TargetIP, e.TargetPort = ap.Addr().String(), int(ap.Port())
		return nil
	}
	var it exportTarget
	if err := json.Unmarshal(raw, &it); err != nil || it.Instance == "" || it.Zone == "" || it.Port <= 0 || it.Port > 65535 {
		return errorf(InvalidArgument, "target: want \"ip:port\", or {\"instance\", \"zone\", \"port\"} with all three").with("field", "target")
	}
	if s.opts.Instances == nil {
		return errorf(FailedPrecondition, "this silod has no facade, so it can't resolve instance %s", it.Instance).with("field", "target")
	}
	ip, err := s.opts.Instances(ctx, t.ID, it.Zone, it.Instance)
	if err != nil {
		return errorf(NotFound, "instance %s in %s: %v", it.Instance, it.Zone, err).with("field", "target")
	}
	e.TargetIP, e.TargetPort, e.Instance, e.Zone = ip.String(), it.Port, it.Instance, it.Zone
	return nil
}

// checkOwnTarget refuses a target outside silo's own space, the gateway
// and control ranges included (they are no silo's own space).
func checkOwnTarget(ctx context.Context, tx store.Tx, t *tree.Silo, ip string) error {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return errorf(InvalidArgument, "target %s is not an address", ip).with("field", "target")
	}
	tab, err := address.Snapshot(ctx, tx)
	if err != nil {
		return err
	}
	if o, ok := tab.Lookup(a); !ok || o.Silo != t.ID {
		return errorf(InvalidArgument, "target %s is not in %s's own space", ip, t.Path).with("field", "target")
	}
	return nil
}

func cleanPath(p string) string { return "/" + strings.Trim(p, "/") }

// offerExport is POST .../exports (SV-14).
func (s *Server) offerExport(q *request, ref string) (any, error) {
	ctx := q.r.Context()
	var req offerRequest
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	if !tree.ValidName(req.Name) {
		return nil, errorf(InvalidArgument, "name %q: want a DNS label", req.Name).with("field", "name")
	}
	switch req.Protocol {
	case exports.HTTP:
		if req.ProxyProtocol {
			return nil, errorf(InvalidArgument, "proxy_protocol is for tcp exports").with("field", "proxy_protocol")
		}
	case exports.TCP:
	case exports.Registry:
		if req.ProxyProtocol {
			return nil, errorf(InvalidArgument, "proxy_protocol is for tcp exports").with("field", "proxy_protocol")
		}
		if len(req.Target) != 0 && string(req.Target) != "null" {
			return nil, errorf(InvalidArgument, "a registry export has no target; repos names what it exports").with("field", "target")
		}
		var err error
		if req.Repos, err = checkRegistryRepos(req.Repos); err != nil {
			return nil, err
		}
	default:
		return nil, errorf(InvalidArgument, "protocol %q: want http, tcp or registry", req.Protocol).with("field", "protocol")
	}
	if req.Protocol != exports.Registry && len(req.Repos) > 0 {
		return nil, errorf(InvalidArgument, "repos is for registry exports").with("field", "repos")
	}
	if len(req.To) == 0 {
		return nil, errorf(InvalidArgument, "to names no silo").with("field", "to")
	}
	var out *ExportObject
	err := s.tx(ctx, func(tx store.Tx) error {
		t, err := s.grantSilo(ctx, tx, q, ref)
		if err != nil {
			return err
		}
		q.note(t, KindExportOffer)
		if ok, err := ExportsAllowed(ctx, tx, t.ID); err != nil {
			return err
		} else if !ok {
			return errorf(PermissionDenied, "%s's effective grant_policy doesn't allow exports", t.Path).with("kind", GrantExports)
		}
		e := &exports.Export{ID: ids.New(), Exporter: t.ID, Name: req.Name, Owner: exports.OwnerTenant,
			Protocol: req.Protocol, ProxyProtocol: req.ProxyProtocol, Repos: req.Repos}
		if e.Protocol != exports.Registry {
			if err := s.parseTarget(ctx, t, req.Target, e); err != nil {
				return err
			}
			if err := checkOwnTarget(ctx, tx, t, e.TargetIP); err != nil {
				return err
			}
		}
		old, err := exports.ByName(ctx, tx, t.ID, req.Name)
		switch {
		case errors.Is(err, exports.ErrNotFound):
			if err := exports.Create(ctx, tx, e); err != nil {
				return err
			}
		case err != nil:
			return err
		case old.Protocol != e.Protocol || old.TargetIP != e.TargetIP || old.TargetPort != e.TargetPort ||
			old.Instance != e.Instance || old.ProxyProtocol != e.ProxyProtocol || !slices.Equal(old.Repos, e.Repos):
			return errorf(AlreadyExists, "%s already exports %s, to another target; withdraw it first", t.Path, req.Name).with("name", req.Name)
		default:
			// The same export offered again, to more silos.
			e = old
		}
		var to []string
		for _, p := range req.To {
			p = cleanPath(p)
			id := ""
			// Whether the path holds a silo is never told the exporter: an
			// offer to nobody is kept, and shown, like any other.
			if o, err := tree.ByPath(ctx, tx, p); err == nil {
				id = o.ID
			} else if !errors.Is(err, tree.ErrNotFound) {
				return err
			}
			if id == t.ID {
				return errorf(InvalidArgument, "%s can't offer an export to itself", t.Path).with("field", "to")
			}
			if err := exports.AddOffer(ctx, tx, e.ID, p, id); err != nil {
				return err
			}
			to = append(to, p)
		}
		f := map[string]any{"export": e.ID, "name": e.Name, "protocol": e.Protocol, "to": to}
		if e.Protocol == exports.Registry {
			f["repos"] = e.Repos
		} else {
			f["target"] = e.Target().String()
		}
		if err := Audit(ctx, tx, t, q.caller, KindExportOffer, q.requestID, "OK", f); err != nil {
			return err
		}
		if _, err = appendExportChange(ctx, tx, ExportChange{Export: e.ID, Exporter: t.ID, What: "offer"}); err != nil {
			return err
		}
		if out, err = exportObject(ctx, tx, e); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, 0, err)
}

// findOffered finds the export ref names ("/team-a/pkgcache": the
// exporter's path and the export's name) among those offered to silo,
// NOT_FOUND otherwise, so an importer learns nothing of exports not
// offered to it.
func findOffered(ctx context.Context, tx store.Tx, silo, ref string) (*exports.Export, error) {
	ref = cleanPath(ref)
	i := strings.LastIndex(ref, "/")
	exPath, name := ref[:i], ref[i+1:]
	if exPath == "" {
		exPath = "/"
	}
	nf := errorf(NotFound, "no export %s is offered to this silo", ref).with("export", ref)
	ex, err := tree.ByPath(ctx, tx, exPath)
	if errors.Is(err, tree.ErrNotFound) {
		return nil, nf
	}
	if err != nil {
		return nil, err
	}
	e, err := exports.ByName(ctx, tx, ex.ID, name)
	if errors.Is(err, exports.ErrNotFound) {
		return nil, nf
	}
	if err != nil {
		return nil, err
	}
	offers, err := exports.Offers(ctx, tx, e.ID)
	if err != nil {
		return nil, err
	}
	for _, o := range offers {
		if o.ToSilo == silo {
			return e, nil
		}
	}
	return nil, nf
}

// acceptExport is POST .../imports: {"export": "/team-a/pkgcache"}.
// It returns once the fence lets the gateway reach the export.
func (s *Server) acceptExport(q *request, ref string) (any, error) {
	ctx := q.r.Context()
	var req struct {
		Export string `json:"export"`
	}
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	if strings.Trim(req.Export, "/") == "" {
		return nil, errorf(InvalidArgument, "export is required: the exporter's path and the export's name").with("field", "export")
	}
	var out *ImportObject
	var seq uint64
	err := s.tx(ctx, func(tx store.Tx) error {
		t, err := s.grantSilo(ctx, tx, q, ref)
		if err != nil {
			return err
		}
		q.note(t, KindExportAccept)
		e, err := findOffered(ctx, tx, t.ID, req.Export)
		if err != nil {
			return err
		}
		if ok, err := ExportsAllowed(ctx, tx, t.ID); err != nil {
			return err
		} else if !ok {
			return errorf(PermissionDenied, "%s's effective grant_policy doesn't allow exports", t.Path).with("kind", GrantExports)
		}
		im, err := exports.ImportOf(ctx, tx, e.ID, t.ID)
		if errors.Is(err, exports.ErrNotFound) {
			state := exports.StateActive
			if ok, err := ExportsAllowed(ctx, tx, e.Exporter); err != nil {
				return err
			} else if !ok {
				state = exports.StateSuspended
			}
			im = &exports.Import{ID: ids.New(), Export: e.ID, Importer: t.ID, State: state}
			if err := exports.CreateImport(ctx, tx, im, e.Protocol == exports.TCP); errors.Is(err, exports.ErrPortsExhausted) {
				return errorf(ResourceExhausted, "the gateway has no free TCP export port")
			} else if err != nil {
				return err
			}
			if err := auditGrant(ctx, tx, e, im, q.caller, KindExportAccept, q.requestID, nil); err != nil {
				return err
			}
			if seq, err = appendExportChange(ctx, tx, ExportChange{Export: e.ID, Exporter: e.Exporter, Import: im.ID, Importer: t.ID, What: "accept"}); err != nil {
				return err
			}
			if err := s.checkBudget(ctx, tx); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if out, err = importObject(ctx, tx, e, t.ID, im); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, seq, err)
}

// auditGrant records a grant event under both sides: the exporter and
// the importer each read their own audit.
func auditGrant(ctx context.Context, tx store.Tx, e *exports.Export, im *exports.Import, c Caller, kind, requestID string, extra map[string]any) error {
	fields := map[string]any{"export": e.ID, "name": e.Name, "exporter": e.Exporter, "import": im.ID, "importer": im.Importer, "protocol": e.Protocol}
	for k, v := range extra {
		fields[k] = v
	}
	for _, id := range []string{e.Exporter, im.Importer} {
		t, err := tree.Get(ctx, tx, id)
		if errors.Is(err, tree.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if err := Audit(ctx, tx, t, c, kind, requestID, "OK", fields); err != nil {
			return err
		}
	}
	return nil
}

// withdrawExport is DELETE .../exports/{name}: the exporter withdraws it
// for everyone. to, if set, is DELETE .../exports/{name}/to/{silo}: for
// that one silo.
func (s *Server) withdrawExport(q *request, ref, name, to string) (any, error) {
	ctx := q.r.Context()
	var out *ExportObject
	var seq uint64
	err := s.tx(ctx, func(tx store.Tx) error {
		t, err := s.grantSilo(ctx, tx, q, ref)
		if err != nil {
			return err
		}
		q.note(t, KindExportWithdraw)
		e, err := exports.ByName(ctx, tx, t.ID, name)
		if errors.Is(err, exports.ErrNotFound) {
			return errorf(NotFound, "%s has no export %q", t.Path, name).with("name", name)
		}
		if err != nil {
			return err
		}
		if e.Owner != exports.OwnerTenant {
			return errorf(PermissionDenied, "the export %s is owned by %s, not the silo", name, e.Owner)
		}
		if out, err = exportObject(ctx, tx, e); err != nil {
			return err
		}
		ims, err := exports.ImportsOf(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		offers, err := exports.Offers(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		fields := map[string]any{"export": e.ID, "name": e.Name}
		if to == "" {
			if err := exports.Delete(ctx, tx, e.ID); err != nil {
				return err
			}
		} else {
			to = cleanPath(to)
			var of *exports.Offer
			for _, o := range offers {
				if o.ToPath == to {
					of = o
				}
			}
			if of == nil {
				return errorf(NotFound, "%s's export %s is not offered to %s", t.Path, name, to).with("name", name).with("to", to)
			}
			if err := exports.DeleteOffer(ctx, tx, of); err != nil {
				return err
			}
			fields["to"] = to
			kept := ims[:0]
			for _, im := range ims {
				if im.Importer == of.ToSilo {
					kept = append(kept, im)
				}
			}
			ims = kept
			out.To = nil
		}
		// One record under the exporter, and one under each importer cut
		// off, whose traffic stops.
		if err := Audit(ctx, tx, t, q.caller, KindExportWithdraw, q.requestID, "OK", fields); err != nil {
			return err
		}
		for _, im := range ims {
			if it, err := tree.Get(ctx, tx, im.Importer); err == nil {
				f := map[string]any{"export": e.ID, "name": e.Name, "import": im.ID, "exporter": e.Exporter}
				if err := Audit(ctx, tx, it, q.caller, KindExportWithdraw, q.requestID, "OK", f); err != nil {
					return err
				}
			}
		}
		if seq, err = appendExportChange(ctx, tx, ExportChange{Export: e.ID, Exporter: e.Exporter, What: "withdraw"}); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, seq, err)
}

// deleteImport is DELETE .../imports/{id}: the importer gives it up.
// The offer stays, so it may be accepted again.
func (s *Server) deleteImport(q *request, ref, id string) (any, error) {
	ctx := q.r.Context()
	var out *ImportObject
	var seq uint64
	err := s.tx(ctx, func(tx store.Tx) error {
		t, err := s.grantSilo(ctx, tx, q, ref)
		if err != nil {
			return err
		}
		q.note(t, KindExportWithdraw)
		im, err := exports.GetImport(ctx, tx, id)
		if errors.Is(err, exports.ErrNotFound) || (err == nil && im.Importer != t.ID) {
			return errorf(NotFound, "%s has no import %s", t.Path, id)
		}
		if err != nil {
			return err
		}
		e, err := exports.Get(ctx, tx, im.Export)
		if err != nil {
			return err
		}
		if out, err = importObject(ctx, tx, e, t.ID, im); err != nil {
			return err
		}
		if err := exports.DeleteImport(ctx, tx, im.ID); err != nil {
			return err
		}
		if err := auditGrant(ctx, tx, e, im, q.caller, KindExportWithdraw, q.requestID, map[string]any{"by": "importer"}); err != nil {
			return err
		}
		if seq, err = appendExportChange(ctx, tx, ExportChange{Export: e.ID, Exporter: e.Exporter, Import: im.ID, Importer: t.ID, What: "withdraw"}); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	return s.done(q, out, seq, err)
}

// listGrants serves the three lists, reads: GET .../exports, .../imports
// and .../offers.
func (s *Server) listGrants(q *request, ref, what string) {
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
		switch what {
		case "exports":
			es, err := exports.Of(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			for _, e := range es {
				o, err := exportObject(ctx, tx, e)
				if err != nil {
					return err
				}
				items = append(items, o)
			}
		case "imports":
			ims, err := exports.ImportsBy(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			for _, im := range ims {
				e, err := exports.Get(ctx, tx, im.Export)
				if err != nil {
					return err
				}
				o, err := importObject(ctx, tx, e, t.ID, im)
				if err != nil {
					return err
				}
				items = append(items, o)
			}
		case "offers":
			offers, err := exports.OffersTo(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			for _, of := range offers {
				if _, err := exports.ImportOf(ctx, tx, of.Export, t.ID); err == nil {
					continue // accepted: it is an import
				} else if !errors.Is(err, exports.ErrNotFound) {
					return err
				}
				e, err := exports.Get(ctx, tx, of.Export)
				if err != nil {
					return err
				}
				o, err := importObject(ctx, tx, e, t.ID, nil)
				if err != nil {
					return err
				}
				items = append(items, o)
			}
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	out := map[string]any{what: []any{}}
	if offset < len(items) {
		end := min(len(items), offset+size)
		out[what] = items[offset:end]
		if end < len(items) {
			out["next_page_token"] = nextToken(offset, size, true)
		}
	}
	writeJSON(q.w, http.StatusOK, out)
}

// SyncGrants brings every import's and every matched link's state in
// line with both sides' effective grant policies (SV-13): a grant
// either side no longer allows is SUSPENDED, and one both allow again
// is ACTIVE, each audited
// (grant.suspend, grant.resume) under both sides and written to the
// change log, which is what stops and restarts its traffic. c is who
// made the change that moved a policy. It returns the last change it
// wrote, 0 for none.
func SyncGrants(ctx context.Context, tx store.Tx, c Caller, requestID string) (uint64, error) {
	ims, err := exports.AllImports(ctx, tx)
	if err != nil {
		return 0, err
	}
	allowed := map[string]bool{}
	allows := func(id string) (bool, error) {
		if v, ok := allowed[id]; ok {
			return v, nil
		}
		v, err := ExportsAllowed(ctx, tx, id)
		if errors.Is(err, tree.ErrNotFound) {
			v, err = false, nil
		}
		allowed[id] = v
		return v, err
	}
	var seq uint64
	for _, im := range ims {
		e, err := exports.Get(ctx, tx, im.Export)
		if err != nil {
			return 0, err
		}
		a, err := allows(e.Exporter)
		if err != nil {
			return 0, err
		}
		b, err := allows(im.Importer)
		if err != nil {
			return 0, err
		}
		want := exports.StateSuspended
		if a && b {
			want = exports.StateActive
		}
		if want == im.State {
			continue
		}
		if err := exports.SetState(ctx, tx, im.ID, want); err != nil {
			return 0, err
		}
		kind, what := KindGrantSuspend, "suspend"
		var denied []string
		if !a {
			denied = append(denied, e.Exporter)
		}
		if !b {
			denied = append(denied, im.Importer)
		}
		if want == exports.StateActive {
			kind, what = KindGrantResume, "resume"
		}
		im.State = want
		f := map[string]any{"grant": "export", "state": want}
		if len(denied) > 0 {
			sort.Strings(denied)
			f["denied_by"] = denied
		}
		if err := auditGrant(ctx, tx, e, im, c, kind, requestID, f); err != nil {
			return 0, err
		}
		if seq, err = appendExportChange(ctx, tx, ExportChange{Export: e.ID, Exporter: e.Exporter, Import: im.ID, Importer: im.Importer, What: what}); err != nil {
			return 0, err
		}
	}
	n, err := syncLinks(ctx, tx, c, requestID)
	if err != nil {
		return 0, err
	}
	return max(seq, n), nil
}

// endExportsFor withdraws silo t's exports and drops its imports and
// the offers made to it, as it is deleted.
func endExportsFor(ctx context.Context, tx store.Tx, t *tree.Silo, c Caller, requestID string) error {
	es, err := exports.Of(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	for _, e := range es {
		if err := exports.Delete(ctx, tx, e.ID); err != nil {
			return err
		}
		if _, err := appendExportChange(ctx, tx, ExportChange{Export: e.ID, Exporter: e.Exporter, What: "withdraw"}); err != nil {
			return err
		}
	}
	ims, err := exports.ImportsBy(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	for _, im := range ims {
		if err := exports.DeleteImport(ctx, tx, im.ID); err != nil {
			return err
		}
		e, err := exports.Get(ctx, tx, im.Export)
		if err != nil {
			return err
		}
		if err := auditGrant(ctx, tx, e, im, c, KindExportWithdraw, requestID, map[string]any{"reason": "importer deleted"}); err != nil {
			return err
		}
		if _, err := appendExportChange(ctx, tx, ExportChange{Export: e.ID, Exporter: e.Exporter, Import: im.ID, Importer: t.ID, What: "withdraw"}); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE export_offers SET to_silo = '' WHERE to_silo = ?`, t.ID)
	return err
}

// FollowInstanceTargets moves every instance export whose instance's
// address has changed to its new one, so an export follows its
// instance (SV-14). silod runs it on a timer. It returns how many it
// moved.
func FollowInstanceTargets(ctx context.Context, st store.Store, resolve InstanceResolver) (int, error) {
	var es []*exports.Export
	if err := st.Tx(ctx, func(tx store.Tx) error {
		var err error
		es, err = exports.All(ctx, tx)
		return err
	}); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range es {
		if e.Instance == "" {
			continue
		}
		ip, err := resolve(ctx, e.Exporter, e.Zone, e.Instance)
		next := ""
		if err == nil {
			next = ip.String()
		}
		if next == e.TargetIP {
			continue
		}
		err = st.Tx(ctx, func(tx store.Tx) error {
			cur, err := exports.Get(ctx, tx, e.ID)
			if errors.Is(err, exports.ErrNotFound) {
				return nil
			}
			if err != nil || cur.TargetIP != e.TargetIP {
				return err
			}
			ex, err := tree.Get(ctx, tx, e.Exporter)
			if err != nil {
				return err
			}
			// An address that has left the exporter's own space is no
			// target at all.
			if next != "" && checkOwnTarget(ctx, tx, ex, next) != nil {
				next = ""
			}
			if err := exports.SetTarget(ctx, tx, e.ID, next); err != nil {
				return err
			}
			_, err = appendExportChange(ctx, tx, ExportChange{Export: e.ID, Exporter: e.Exporter, What: "target"})
			return err
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
