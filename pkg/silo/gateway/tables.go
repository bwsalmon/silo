package gateway

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/bwsalmon/silo/pkg/silo/egress"
	"github.com/bwsalmon/silo/pkg/silo/exports"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/manage"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// Tables are what silod pushes to its gateway, all at one version of
// the change log: the origin table, every silo's effective control
// policy and effective egress policy, the ACTIVE manage grants and the
// exports with ACTIVE imports (gateway.md, "Origin identity", "Layer
// 1", "Policy" and "Proxied exports"). A Tables is immutable once built, so the gateway
// swaps the whole of it and a reader may share it.
type Tables struct {
	// Version is the change log's sequence number as of the snapshot:
	// every change up to and including it is in the tables.
	Version uint64 `json:"version"`
	// Blocks and Own are the origin table: every block, and every own
	// range (a subnet's primary or secondary range), with its holder.
	Blocks []Entry `json:"blocks"`
	Own    []Entry `json:"own"`
	// Silos are every silo's path and effective control and egress
	// policies, by id.
	Silos map[string]Silo `json:"silos"`
	// Grants are the ACTIVE manage grants' holders, by grant id.
	Grants map[string]string `json:"grants"`
	// Exports are the exports with an ACTIVE import, each with those
	// imports alone: what the gateway carries (gateway.md, "Proxied
	// exports").
	Exports []Export `json:"exports"`

	origin  *address.Table
	vpc     []netip.Prefix // every block and own range: the shared VPC's
	byName  map[string]*Export
	byPort  map[int]portEntry
	imports map[string]bool // the ACTIVE imports, by id
}

// Export is an export the gateway carries.
type Export struct {
	ID            string   `json:"id"`
	Exporter      string   `json:"exporter"`
	ExporterPath  string   `json:"exporter_path"`
	Name          string   `json:"name"`
	Protocol      string   `json:"protocol"`
	Target        string   `json:"target"` // ip:port
	ProxyProtocol bool     `json:"proxy_protocol,omitempty"`
	Imports       []Import `json:"imports"`
}

// Import is an ACTIVE import of an Export.
type Import struct {
	ID           string `json:"id"`
	Importer     string `json:"importer"`
	ImporterPath string `json:"importer_path"`
	Port         int    `json:"port,omitempty"` // a tcp export's gateway port
}

type portEntry struct {
	export *Export
	imp    *Import
}

// HTTPExport finds the http export exporter calls name, and caller's
// import of it: nil if it has none.
func (t *Tables) HTTPExport(exporter, name, caller string) (*Export, *Import) {
	if t == nil {
		return nil, nil
	}
	e := t.byName[exporter+"/"+name]
	if e == nil || e.Protocol != exports.HTTP {
		return nil, nil
	}
	for i := range e.Imports {
		if e.Imports[i].Importer == caller {
			return e, &e.Imports[i]
		}
	}
	return nil, nil
}

// TCPExport finds the tcp export and import a gateway port carries.
func (t *Tables) TCPExport(port int) (*Export, *Import) {
	if t == nil {
		return nil, nil
	}
	pe, ok := t.byPort[port]
	if !ok {
		return nil, nil
	}
	return pe.export, pe.imp
}

// ImportLive reports whether the import with id is ACTIVE.
func (t *Tables) ImportLive(id string) bool { return t != nil && t.imports[id] }

// Entry is one row of the origin table.
type Entry struct {
	Range netip.Prefix `json:"range"`
	Silo  string       `json:"silo"`
}

// Silo is what the gateway knows of one silo.
type Silo struct {
	Path    string `json:"path"`
	Control string `json:"control"` // effective: none, read or full
	// Egress is the effective egress policy, as the levels it is the
	// intersection of: the own policy of the root, then of each silo
	// down to this one, which is last. A host must be allowed at every
	// level (egress.Allowed).
	Egress []egress.Policy `json:"egress"`
	// EgressPaths are the paths of the silos Egress's levels are, in
	// the same order, for naming the level that denied a host.
	EgressPaths []string `json:"egress_paths"`
}

// index builds the lookup table; it is called once, before the Tables
// is shared.
func (t *Tables) index() *Tables {
	conv := func(es []Entry) []address.Entry {
		out := make([]address.Entry, len(es))
		for i, e := range es {
			out[i] = address.Entry{Range: e.Range, Silo: e.Silo}
		}
		return out
	}
	t.origin = address.NewTable(t.Version, conv(t.Blocks), conv(t.Own))
	t.vpc = nil
	for _, e := range append(append([]Entry{}, t.Blocks...), t.Own...) {
		t.vpc = append(t.vpc, e.Range)
	}
	if t.Silos == nil {
		t.Silos = map[string]Silo{}
	}
	if t.Grants == nil {
		t.Grants = map[string]string{}
	}
	t.byName, t.byPort, t.imports = map[string]*Export{}, map[int]portEntry{}, map[string]bool{}
	for i := range t.Exports {
		e := &t.Exports[i]
		t.byName[e.Exporter+"/"+e.Name] = e
		for j := range e.Imports {
			im := &e.Imports[j]
			t.imports[im.ID] = true
			if e.Protocol == exports.TCP && im.Port > 0 {
				t.byPort[im.Port] = portEntry{e, im}
			}
		}
	}
	return t
}

// Resolve finds the silo whose own space holds ip. subnet is false
// for an address in a silo's block but in no subnet silod made for it,
// which can't be a real VM's.
func (t *Tables) Resolve(ip netip.Addr) (silo string, subnet, ok bool) {
	if t == nil || t.origin == nil {
		return "", false, false
	}
	o, ok := t.origin.Lookup(ip)
	if !ok {
		return "", false, false
	}
	return o.Silo, o.Subnet.IsValid(), true
}

// Snapshot reads the tables in tx, versioned by the change log as of
// the transaction.
func Snapshot(ctx context.Context, tx store.Tx) (*Tables, error) {
	origin, err := address.Snapshot(ctx, tx)
	if err != nil {
		return nil, err
	}
	t := &Tables{Version: origin.Version, Silos: map[string]Silo{}, Grants: map[string]string{}}
	for _, e := range origin.Blocks {
		t.Blocks = append(t.Blocks, Entry{Range: e.Range, Silo: e.Silo})
	}
	for _, e := range origin.Own {
		t.Own = append(t.Own, Entry{Range: e.Range, Silo: e.Silo})
	}
	silos, err := tree.All(ctx, tx)
	if err != nil {
		return nil, err
	}
	eff, err := fenceapi.EffectiveControls(ctx, tx)
	if err != nil {
		return nil, err
	}
	own, err := fenceapi.OwnEgress(ctx, tx)
	if err != nil {
		return nil, err
	}
	// tree.All is shallowest first, so a parent's levels are there
	// before its children's.
	for _, s := range silos {
		info := Silo{Path: s.Path, Control: eff[s.ID]}
		if p, ok := t.Silos[s.Parent]; ok {
			info.Egress = append(info.Egress, p.Egress...)
			info.EgressPaths = append(info.EgressPaths, p.EgressPaths...)
		} else if !s.IsRoot() {
			return nil, fmt.Errorf("gateway tables: silo %s has no parent %s", s.ID, s.Parent)
		}
		p := own[s.ID]
		if p.Allow == nil {
			p.Allow = []string{}
		}
		if p.Deny == nil {
			p.Deny = []string{}
		}
		info.Egress = append(info.Egress, p)
		info.EgressPaths = append(info.EgressPaths, s.Path)
		t.Silos[s.ID] = info
	}
	grants, err := manage.AllActive(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		if h := g.HolderID(); h != "" {
			t.Grants[g.ID] = h
		}
	}
	if err := t.readExports(ctx, tx); err != nil {
		return nil, err
	}
	return t.index(), nil
}

func (t *Tables) readExports(ctx context.Context, tx store.Tx) error {
	ims, err := exports.AllImports(ctx, tx)
	if err != nil {
		return err
	}
	live := map[string][]Import{}
	for _, im := range ims {
		if im.State != exports.StateActive {
			continue
		}
		info, ok := t.Silos[im.Importer]
		if !ok {
			continue
		}
		live[im.Export] = append(live[im.Export], Import{ID: im.ID, Importer: im.Importer, ImporterPath: info.Path, Port: im.Port})
	}
	es, err := exports.All(ctx, tx)
	if err != nil {
		return err
	}
	for _, e := range es {
		target := e.Target()
		info, ok := t.Silos[e.Exporter]
		if len(live[e.ID]) == 0 || !target.IsValid() || !ok {
			continue
		}
		t.Exports = append(t.Exports, Export{ID: e.ID, Exporter: e.Exporter, ExporterPath: info.Path, Name: e.Name,
			Protocol: e.Protocol, Target: target.String(), ProxyProtocol: e.ProxyProtocol, Imports: live[e.ID]})
	}
	return nil
}

// VPC is the shared VPC's ranges as the tables know them: every block,
// which holds the gateway and control ranges, and every own range.
// Egress refuses any address in them (gateway.md, "Enforcement").
func (t *Tables) VPC() []netip.Prefix {
	if t == nil {
		return nil
	}
	return t.vpc
}
