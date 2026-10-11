package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/exports"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger/address"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// This file is silod's half: what it serves on the gateway's socket.

// OriginHeader carries the caller's silo from the gateway to silod, as
// "<id>;v=<table version>". silod reads it only on the gateway's own
// socket, and the gateway sets it on every call it forwards after
// removing whatever the caller sent.
const OriginHeader = "Silo-Origin"

// OriginAddressHeader carries the caller's source address from the
// gateway's API port to silod, beside Silo-Origin, so that silod can
// note the agent whose tool VM made a call (from_tool_vm; agents.md,
// §5). It is a Silo-Origin header, so the gateway removes whatever the
// caller sent, and it changes no authority.
const OriginAddressHeader = "Silo-Origin-Address"

// The paths silod serves the gateway itself on its socket. A forwarded
// call never reaches them: the gateway forwards only /compute/v1/ and
// /silo/v1/, and silod serves these only to a request with no
// Silo-Origin.
const (
	TablesPath      = "/silod/gateway/tables"
	AuditPath       = "/silod/gateway/audit"
	EgressAuditPath = "/silod/gateway/egress"
	ExportAuditPath = "/silod/gateway/export"
)

// tableKinds are the change-log kinds the tables are made of. A change
// of any other kind leaves them as they were.
var tableKinds = map[string]bool{
	address.ChangeKind:         true,
	"silo":                     true,
	fenceapi.ChangeKindControl: true,
	fenceapi.ChangeKindManage:  true,
	"policy":                   true, // a silo's own egress policy
	exports.ChangeKind:         true,
}

// Publisher follows the change log and keeps the tables silod pushes
// to its gateway current.
type Publisher struct {
	st store.Store

	mu      sync.Mutex
	cur     *Tables
	changed chan struct{} // closed, and replaced, when cur changes
}

// NewPublisher returns a Publisher on st. Run fills it.
func NewPublisher(st store.Store) *Publisher {
	return &Publisher{st: st, changed: make(chan struct{})}
}

// Current is the newest tables, and a channel closed when there are
// newer ones; nil tables before Run has read any.
func (p *Publisher) Current() (*Tables, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur, p.changed
}

func (p *Publisher) set(t *Tables) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur != nil && t.Version <= p.cur.Version {
		return
	}
	p.cur = t
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *Publisher) snapshot(ctx context.Context) error {
	var t *Tables
	err := p.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		t, err = Snapshot(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	p.set(t)
	return nil
}

// Run reads the tables, then follows the change log, reading them
// again after every change they are made of, until ctx ends.
func (p *Publisher) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		err := p.follow(ctx)
		if ctx.Err() != nil {
			break
		}
		log.Printf("gateway tables: %v; retrying", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}

func (p *Publisher) follow(ctx context.Context) error {
	if err := p.snapshot(ctx); err != nil {
		return err
	}
	cur, _ := p.Current()
	changes, err := p.st.Changes(ctx, cur.Version)
	if err != nil {
		return err
	}
	for {
		c, ok := <-changes
		if !ok {
			return errors.New("the change log closed")
		}
		stale := tableKinds[c.Kind]
		// Take a burst of changes in one read.
	drain:
		for {
			select {
			case c, ok = <-changes:
				if !ok {
					return errors.New("the change log closed")
				}
				stale = stale || tableKinds[c.Kind]
			default:
				break drain
			}
		}
		if cur, _ := p.Current(); stale && c.Seq > cur.Version {
			if err := p.snapshot(ctx); err != nil {
				return err
			}
		}
	}
}

// ParseOrigin parses a Silo-Origin value: "<id>;v=<version>".
func ParseOrigin(h string) (silo string, version uint64, err error) {
	id, v, ok := strings.Cut(strings.TrimSpace(h), ";v=")
	if !ok || id == "" || strings.ContainsAny(id, " ;,") {
		return "", 0, fmt.Errorf("malformed %s %q", OriginHeader, h)
	}
	version, err = strconv.ParseUint(v, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("malformed %s %q", OriginHeader, h)
	}
	return id, version, nil
}

// FormatOrigin is the Silo-Origin value for silo at version.
func FormatOrigin(silo string, version uint64) string {
	return silo + ";v=" + strconv.FormatUint(version, 10)
}

// Stale returns the newest change after version that could affect who
// silo's origin is, or what it may do: 0 if there is none, and the call
// may go ahead. Such a change is
//
//   - an address change keyed by silo (its blocks, or its own ranges,
//     changed), by one of its children (a block delegated from it, or
//     released back to it), or naming it as the parent;
//   - silo being deleted;
//   - a control change that moved silo's effective policy.
//
// It is deliberately wider than the one IP the call came from, so that
// nothing about the change needs the address: a caller just after one
// gets a retryable UNAVAILABLE until the gateway has the new tables.
func Stale(ctx context.Context, tx store.Tx, silo string, version uint64) (uint64, error) {
	rows, err := tx.Query(ctx, `SELECT seq, kind, key, data FROM changes WHERE seq > ? AND kind IN (?, ?, ?) ORDER BY seq`,
		int64(version), address.ChangeKind, "silo", fenceapi.ChangeKindControl)
	if err != nil {
		return 0, err
	}
	type change struct {
		seq             uint64
		kind, key, data string
	}
	var cs []change
	for rows.Next() {
		var c change
		var seq int64
		if err := rows.Scan(&seq, &c.kind, &c.key, &c.data); err != nil {
			rows.Close()
			return 0, err
		}
		c.seq = uint64(seq)
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var newest uint64
	for _, c := range cs {
		hit, err := affects(ctx, tx, silo, c.kind, c.key, c.data)
		if err != nil {
			return 0, err
		}
		if hit {
			newest = c.seq
		}
	}
	return newest, nil
}

func affects(ctx context.Context, tx store.Tx, silo, kind, key, data string) (bool, error) {
	if key == silo {
		return true, nil
	}
	switch kind {
	case address.ChangeKind:
		var d struct {
			Parent string `json:"parent"`
		}
		_ = json.Unmarshal([]byte(data), &d)
		if d.Parent == silo {
			return true, nil
		}
		// A child's widened block came out of its parent's own space.
		t, err := tree.Get(ctx, tx, key)
		if errors.Is(err, tree.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return t.Parent == silo, nil
	case fenceapi.ChangeKindControl:
		var d fenceapi.ControlChange
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return true, nil // unreadable: assume the worst
		}
		for _, s := range d.Silos {
			if s.ID == silo {
				return true, nil
			}
		}
	}
	return false, nil
}

// SilodOptions configure the handler silod serves on the gateway's
// socket.
type SilodOptions struct {
	// API serves /compute/v1/ and /silo/v1/: the facade and the fence
	// API, which read the caller from the context.
	API http.Handler
	// Publisher supplies the tables streamed to the gateway.
	Publisher *Publisher
	// Tools is silod's end of agents' tool channels, which the gateway
	// forwards to ToolsSilodPath; nil refuses attaches.
	Tools *ToolChannels
	// ToolVMAgent is the agent whose tool VM silod recorded at ip in
	// silo, or "" (fenceapi's Server.ToolVMAgentAt). A call it names is
	// audited with from_tool_vm; nil notes none.
	ToolVMAgent func(ctx context.Context, silo string, ip netip.Addr) (string, error)
}

// Silod returns the handler silod serves on the gateway's Unix socket,
// and only there: it is the one place a Silo-Origin header is believed.
//
// A request with Silo-Origin is a call the gateway forwards: a tool
// channel's attach, on ToolsSilodPath, goes to opts.Tools, and anything
// else to opts.API as a caller of that silo, via origin, once Stale says no
// change since the gateway's tables could affect it. A request without
// one is the gateway's own: the tables stream, and Layer 1's audit.
func Silod(st store.Store, opts SilodOptions) http.Handler {
	return &silod{st: st, opts: opts}
}

type silod struct {
	st   store.Store
	opts SilodOptions
}

func (s *silod) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := r.Header.Values(OriginHeader)
	if len(h) == 0 {
		switch r.URL.Path {
		case TablesPath:
			s.tables(w, r)
		case AuditPath:
			s.audit(w, r)
		case EgressAuditPath:
			s.auditEgress(w, r)
		case ExportAuditPath:
			s.auditExport(w, r)
		default:
			notFound("silo: no such path "+r.URL.Path).write(w, r.URL.Path)
		}
		return
	}
	path := r.URL.Path
	if path == ToolsSilodPath {
		s.attach(w, r)
		return
	}
	if !strings.HasPrefix(path, "/compute/v1/") && !strings.HasPrefix(path, "/silo/v1/") {
		notFound("silo: no such path "+path).write(w, path)
		return
	}
	if len(h) != 1 {
		forbidden("silo: more than one "+OriginHeader).write(w, path)
		return
	}
	id, version, err := ParseOrigin(h[0])
	if err != nil {
		forbidden("silo: "+err.Error()).write(w, path)
		return
	}
	var newest uint64
	err = s.st.Tx(r.Context(), func(tx store.Tx) error {
		var err error
		newest, err = Stale(r.Context(), tx, id, version)
		return err
	})
	if err != nil {
		unavailable("silo: checking the origin: "+err.Error()).write(w, path)
		return
	}
	if newest != 0 {
		e := unavailable(fmt.Sprintf("silo: the gateway's origin table (version %d) is older than change %d, which affects the caller; retry", version, newest))
		e.details = map[string]any{"origin_version": version, "change": newest}
		w.Header().Set("Retry-After", "1")
		e.write(w, path)
		return
	}
	caller := fenceapi.Caller{Silo: id, Via: fenceapi.ViaOrigin}
	if ip, err := netip.ParseAddr(r.Header.Get(OriginAddressHeader)); err == nil && s.opts.ToolVMAgent != nil {
		agent, err := s.opts.ToolVMAgent(r.Context(), id, ip)
		if err != nil {
			unavailable("silo: looking up the caller's tool VM: "+err.Error()).write(w, path)
			return
		}
		caller.FromToolVM = agent
	}
	for k := range r.Header {
		if isSiloHeader(k) {
			r.Header.Del(k)
		}
	}
	ctx := fenceapi.ContextWithCaller(r.Context(), caller)
	s.opts.API.ServeHTTP(w, r.WithContext(ctx))
}

// tables streams the tables to the gateway, one JSON object a line:
// the current ones at once, then each newer one as it is read.
func (s *silod) tables(w http.ResponseWriter, r *http.Request) {
	if s.opts.Publisher == nil {
		notFound("silo: no tables are published here").write(w, r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	var sent uint64
	first := true
	for {
		t, changed := s.opts.Publisher.Current()
		if t != nil && (first || t.Version > sent) {
			if err := enc.Encode(t); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			sent, first = t.Version, false
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		}
	}
}

// AuditRecord is a refusal the gateway made itself, which silod writes
// to the audit log as the gateway has no store.
type AuditRecord struct {
	Silo      string `json:"silo"`
	Version   uint64 `json:"version"`
	Op        string `json:"op"`
	Class     string `json:"class"`
	Policy    string `json:"policy"`
	RequestID string `json:"request_id,omitempty"`
}

func (s *silod) audit(w http.ResponseWriter, r *http.Request) {
	var rec AuditRecord
	if r.Method != http.MethodPost {
		notFound("silo: no such method").write(w, r.URL.Path)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil || rec.Silo == "" {
		(&refusal{status: http.StatusBadRequest, code: fenceapi.InvalidArgument, message: "silo: bad audit record"}).write(w, r.URL.Path)
		return
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		chain, err := tree.Ancestry(ctx, tx, rec.Silo)
		if errors.Is(err, tree.ErrNotFound) {
			return nil // gone since; nothing to record it under
		}
		if err != nil {
			return err
		}
		_, from, err := fenceapi.EffectiveControl(ctx, tx, chain)
		if err != nil {
			return err
		}
		return fenceapi.Audit(ctx, tx, chain[len(chain)-1], fenceapi.Caller{Silo: rec.Silo, Via: fenceapi.ViaOrigin},
			fenceapi.KindControlDenied, rec.RequestID, string(fenceapi.ControlDenied), map[string]any{
				"op": rec.Op, "class": rec.Class, "policy": rec.Policy, "effective_from": from,
				"layer": "gateway", "origin_version": rec.Version,
			})
	})
	if err != nil {
		unavailable("silo: recording the refusal: "+err.Error()).write(w, r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// auditEgress records one of the gateway's egress connections as
// egress.connect: OK if it was allowed, PERMISSION_DENIED if not.
func (s *silod) auditEgress(w http.ResponseWriter, r *http.Request) {
	var rec EgressRecord
	if r.Method != http.MethodPost {
		notFound("silo: no such method").write(w, r.URL.Path)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil || rec.Silo == "" {
		(&refusal{status: http.StatusBadRequest, code: fenceapi.InvalidArgument, message: "silo: bad egress record"}).write(w, r.URL.Path)
		return
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		t, err := tree.Get(ctx, tx, rec.Silo)
		if errors.Is(err, tree.ErrNotFound) {
			return nil // gone since; nothing to record it under
		}
		if err != nil {
			return err
		}
		result := "OK"
		if !rec.Allowed {
			result = string(fenceapi.PermissionDenied)
		}
		fields := map[string]any{
			"method": rec.Method, "host": rec.Host, "port": rec.Port, "allowed": rec.Allowed,
			"bytes_out": rec.BytesOut, "bytes_in": rec.BytesIn, "origin_version": rec.Version,
		}
		for k, v := range map[string]string{"reason": rec.Reason, "addr": rec.Addr, "closed": rec.Closed} {
			if v != "" {
				fields[k] = v
			}
		}
		if rec.Internal {
			fields["internal"] = true
		}
		return fenceapi.Audit(ctx, tx, t, fenceapi.Caller{Silo: rec.Silo, Via: fenceapi.ViaOrigin},
			fenceapi.KindEgressConnect, "", result, fields)
	})
	if err != nil {
		unavailable("silo: recording egress: "+err.Error()).write(w, r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isSiloHeader reports whether a header name is one only the gateway
// sets: Silo-Origin and Silo-Caller*, in any case, with - or _.
func isSiloHeader(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(k, "_", "-"))
	return strings.HasPrefix(k, "silo-origin") || strings.HasPrefix(k, "silo-caller")
}
