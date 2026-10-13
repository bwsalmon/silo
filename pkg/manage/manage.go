// Package manage is manage grants' shared half (docs/silo/fence-api.md,
// "Manage grants", SV-17 to SV-22): the grants table, their scope and
// lease, and the one check, Allows, that the fence API, the compute/v1
// facade and the routes later tasks add ask before letting a holder act
// on a silo it isn't above.
//
// A grant lets its holder act on the target and the target's
// descendants as an ancestor would, within its scope. Offering,
// revoking and the audit records are the fence API's; this package
// only stores grants and answers what they cover. Every time is
// database time (Tx.Now).
package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// The fence groups a scope may name (fence-api.md, "What a holder may
// do", SV-18). read is always in a scope. grants and agents gate routes
// later tasks add, which call Allows with these names.
const (
	GroupRead     = "read"     // describe, children, audit
	GroupGrants   = "grants"   // grant calls on the target's behalf
	GroupChildren = "children" // create and delete descendants, change their allocations and policies
	GroupBlocks   = "blocks"   // grow, addBlock, releaseBlock below the target; releaseBlock on it
	GroupAgents   = "agents"   // agents in the target and its descendants
)

// Groups are the fence groups, in the order the spec lists them.
var Groups = []string{GroupRead, GroupGrants, GroupChildren, GroupBlocks, GroupAgents}

var groupSet = func() map[string]bool {
	m := map[string]bool{}
	for _, g := range Groups {
		m[g] = true
	}
	return m
}()

// IsGroup reports whether what is a fence group rather than a facade
// resource type.
func IsGroup(what string) bool { return groupSet[what] }

// Access is what a facade scope allows: reads, or reads and writes.
type Access string

const (
	Read  Access = "read"
	Write Access = "write"
)

// The states of a grant. A PENDING grant gives nothing; EXPIRED and
// REVOKED have ended.
const (
	StatePending = "PENDING"
	StateActive  = "ACTIVE"
	StateExpired = "EXPIRED"
	StateRevoked = "REVOKED"
)

// MaxTTL is the longest lease (SV-20).
const MaxTTL = 30 * 24 * time.Hour

// AllResources is the facade scope that names every type, those added
// later included.
const AllResources = "*"

// ambient are the facade types any grant with a facade scope may read:
// the region and machine catalogues, the operation collections a write
// is followed through, and the project. They hold nothing of the
// target's own, and without them no client can wait on the write it
// was allowed to make.
var ambient = map[string]bool{
	"regions": true, "zones": true, "machineTypes": true, "diskTypes": true, "acceleratorTypes": true,
	"zoneOperations": true, "regionOperations": true, "globalOperations": true, "projects": true,
}

// Repos is the resource type of silo repos (local-git.md, "Manage
// grants: the repos resource type"): read clones, fetches, lists and
// gets them, and write also pushes, creates and deletes them. No facade
// serves it; the fence API and the git service ask for it.
const Repos = "repos"

// Registry is the resource type of image repos (local-registry.md,
// "Manage grants: the registry resource type"): read pulls, lists and
// gets them, and write also pushes, untags and deletes them. No facade
// serves it; the fence API and the registry ask for it.
const Registry = "registry"

// DockerResources are the Docker facade's resource types
// (local-backend.md, "Address, and how a request names its silo"): a
// call is charged to the type its path starts with, and exec and the
// system endpoints to containers.
var DockerResources = []string{"containers", "images", "networks", "volumes"}

// Resources are the resource types a scope may name: the facade types
// gcp-backend.md "Covered" lists, as pkg/control's table has them,
// the Docker facade's, repos and registry.
func Resources() []string {
	seen := map[string]bool{Repos: true, Registry: true}
	for _, r := range DockerResources {
		seen[r] = true
	}
	for m := range control.FacadeMethods() {
		seen[strings.SplitN(m, ".", 2)[0]] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Scope is what a grant covers (SV-19).
type Scope struct {
	Facade *FacadeScope `json:"facade,omitempty"`
	Fence  []string     `json:"fence"`
}

// FacadeScope is a scope's facade half.
type FacadeScope struct {
	Resources []string `json:"resources"`
	Access    Access   `json:"access"`
}

// Lease is a grant's lease, as the API shows it.
type Lease struct {
	TTL        string     `json:"ttl"`
	RenewUntil *time.Time `json:"renew_until,omitempty"`
}

// Grant is one manage grant.
type Grant struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Target     string     `json:"target"`      // the target's id
	TargetPath string     `json:"target_path"` // its path now
	Holder     string     `json:"holder"`      // the path the offerer named
	State      string     `json:"state"`
	Scope      Scope      `json:"scope"`
	Lease      Lease      `json:"lease"`
	OfferedBy  string     `json:"offered_by"`
	CreateTime time.Time  `json:"create_time"`
	AcceptTime *time.Time `json:"accept_time,omitempty"`
	ExpireTime *time.Time `json:"expire_time,omitempty"`

	holderID   string
	ttl        time.Duration
	renewUntil time.Time
	expiresAt  time.Time
}

// HolderID is the holder's silo id, "" when no silo had the holder's
// path at the offer.
func (g *Grant) HolderID() string { return g.holderID }

// TTL is the lease's length.
func (g *Grant) TTL() time.Duration { return g.ttl }

// Covers reports whether g's scope covers what (a fence group, or a
// facade resource type) with access. A fence group needs no access.
func (g *Grant) Covers(what string, access Access) bool {
	if IsGroup(what) {
		if what == GroupRead {
			return true
		}
		for _, f := range g.Scope.Fence {
			if f == what {
				return true
			}
		}
		return false
	}
	f := g.Scope.Facade
	if f == nil {
		return false
	}
	if ambient[what] && access == Read {
		return true
	}
	if access == Write && f.Access != Write {
		return false
	}
	for _, r := range f.Resources {
		if r == AllResources || r == what {
			return true
		}
	}
	return false
}

// Denied is Allows' refusal: the caller holds an active grant on the
// silo, which the call isn't within. The fence API answers it
// PERMISSION_DENIED and the facade 403 forbidden, each naming Grant.
type Denied struct {
	Grant  *Grant
	What   string
	Access Access
}

func (d *Denied) Error() string {
	if IsGroup(d.What) {
		return fmt.Sprintf("the manage grant %q on %s does not cover the fence group %q", d.Grant.Name, d.Grant.TargetPath, d.What)
	}
	return fmt.Sprintf("the manage grant %q on %s does not cover %s on %q", d.Grant.Name, d.Grant.TargetPath, d.Access, d.What)
}

// Errors from the grant operations, which the fence API turns into its
// codes.
var (
	ErrNotFound = errors.New("manage: no such grant")
	ErrExists   = errors.New("manage: a live grant with that name exists")
)

// Invalid is a request the API answers INVALID_ARGUMENT, about Field.
type Invalid struct {
	Field   string
	Message string
}

func (e *Invalid) Error() string { return e.Message }

func invalid(field, format string, args ...any) error {
	return &Invalid{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Precondition is a call the grant's state refuses: FAILED_PRECONDITION.
type Precondition struct{ Message string }

func (e *Precondition) Error() string { return e.Message }

// Active returns the grants caller holds that are ACTIVE and unexpired
// by database time, on target or one of its ancestors: those that let
// caller act on target. They come in order from the nearest target up.
func Active(ctx context.Context, tx store.Tx, caller string, target *tree.Silo) ([]*Grant, error) {
	if caller == "" {
		return nil, nil
	}
	chain, err := tree.Ancestry(ctx, tx, target.ID)
	if err != nil {
		return nil, err
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	gs, err := query(ctx, tx, `holder = ? AND state = ? AND expires_at > ?`, caller, StateActive, store.Millis(now))
	if err != nil {
		return nil, err
	}
	var out []*Grant
	for i := len(chain) - 1; i >= 0; i-- {
		for _, g := range gs {
			if g.Target == chain[i].ID {
				g.TargetPath = chain[i].Path
				out = append(out, g)
			}
		}
	}
	return out, nil
}

// Allows is the check every route a holder can reach asks: whether
// caller may act on target, within what (a fence group, or a facade
// resource type) and access, through a manage grant. It returns the
// grant that covers the call; nil and no error when caller holds no
// active grant over target at all, so target stays hidden from it; or
// a *Denied naming a grant caller holds when none of them covers the
// call. It doesn't know whether caller is an ancestor of target, which
// needs no grant; ask only when it isn't.
func Allows(ctx context.Context, tx store.Tx, caller string, target *tree.Silo, what string, access Access) (*Grant, error) {
	return AllowsWhere(ctx, tx, caller, target, what, access, nil)
}

// AllowsWhere is Allows, with ok, if given, further limiting which
// covering grants count: the fence API uses it for the calls a grant
// allows on the target's descendants but not on the target itself.
func AllowsWhere(ctx context.Context, tx store.Tx, caller string, target *tree.Silo, what string, access Access, ok func(*Grant) bool) (*Grant, error) {
	gs, err := Active(ctx, tx, caller, target)
	if err != nil || len(gs) == 0 {
		return nil, err
	}
	for _, g := range gs {
		if g.Covers(what, access) && (ok == nil || ok(g)) {
			return g, nil
		}
	}
	return nil, &Denied{Grant: gs[0], What: what, Access: access}
}

// Offer is an offer's body.
type Offer struct {
	Name   string `json:"name"`
	Holder string `json:"holder"`
	Scope  *Scope `json:"scope"`
	Lease  *struct {
		TTL        string `json:"ttl"`
		RenewUntil string `json:"renew_until"`
	} `json:"lease"`
}

func under(p, anc string) bool {
	return anc == "/" || p == anc || strings.HasPrefix(p, anc+"/")
}

// ParseDuration reads a duration in the API's form, seconds with an s
// suffix ("86400s", "1.5s"), or Go's ("24h").
func ParseDuration(s string) (time.Duration, error) {
	return time.ParseDuration(s)
}

// Create stores offer as a PENDING grant on target, made by offeredBy
// (a proper ancestor of target, which the caller checks). An ended
// grant with the same name is replaced.
func Create(ctx context.Context, tx store.Tx, target *tree.Silo, offeredBy, id string, o Offer) (*Grant, error) {
	if !tree.ValidName(o.Name) {
		return nil, invalid("name", "name %q is not a DNS label", o.Name)
	}
	holder := o.Holder
	if !strings.HasPrefix(holder, "/") {
		return nil, invalid("holder", "holder %q: want a silo path, starting /", holder)
	}
	if holder != "/" {
		holder = strings.TrimSuffix(holder, "/")
	}
	if under(holder, target.Path) {
		return nil, invalid("holder", "the holder %s is %s or below it; a silo can't be given the means to change itself", holder, target.Path)
	}
	if under(target.Path, holder) {
		return nil, invalid("holder", "the holder %s is an ancestor of %s, which a grant adds nothing to", holder, target.Path)
	}
	scope := Scope{Fence: []string{GroupRead}}
	if o.Scope != nil {
		if f := o.Scope.Facade; f != nil {
			if f.Access != Read && f.Access != Write {
				return nil, invalid("scope.facade.access", "scope.facade.access %q: want read or write", f.Access)
			}
			known := map[string]bool{}
			for _, r := range Resources() {
				known[r] = true
			}
			if len(f.Resources) == 0 {
				return nil, invalid("scope.facade.resources", "scope.facade.resources is empty: name resource types, or [\"*\"]")
			}
			for _, r := range f.Resources {
				if r == AllResources {
					if len(f.Resources) != 1 {
						return nil, invalid("scope.facade.resources", "scope.facade.resources: \"*\" stands alone")
					}
					continue
				}
				if !known[r] {
					return nil, invalid("scope.facade.resources", "scope.facade.resources: %q is not a facade resource type", r)
				}
			}
			scope.Facade = &FacadeScope{Resources: append([]string{}, f.Resources...), Access: f.Access}
		}
		for _, g := range o.Scope.Fence {
			if !IsGroup(g) {
				return nil, invalid("scope.fence", "scope.fence: %q is not a group; want one of %s", g, strings.Join(Groups, ", "))
			}
			if g != GroupRead && !contains(scope.Fence, g) {
				scope.Fence = append(scope.Fence, g)
			}
		}
	}
	if o.Lease == nil || o.Lease.TTL == "" {
		return nil, invalid("lease.ttl", "lease.ttl is required")
	}
	ttl, err := ParseDuration(o.Lease.TTL)
	if err != nil || ttl <= 0 {
		return nil, invalid("lease.ttl", "lease.ttl %q: want a positive duration such as \"86400s\"", o.Lease.TTL)
	}
	if ttl > MaxTTL {
		return nil, invalid("lease.ttl", "lease.ttl %s is past the limit of 30 days", o.Lease.TTL)
	}
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	var renewUntil int64
	if o.Lease.RenewUntil != "" {
		t, err := time.Parse(time.RFC3339, o.Lease.RenewUntil)
		if err != nil {
			return nil, invalid("lease.renew_until", "lease.renew_until %q: want an RFC 3339 time", o.Lease.RenewUntil)
		}
		if !t.After(now) {
			return nil, invalid("lease.renew_until", "lease.renew_until %s has passed", o.Lease.RenewUntil)
		}
		renewUntil = store.Millis(t)
	}
	holderID := ""
	if h, err := tree.ByPath(ctx, tx, holder); err == nil {
		holderID = h.ID
	} else if !errors.Is(err, tree.ErrNotFound) {
		return nil, err
	}
	old, err := query(ctx, tx, `target = ? AND name = ?`, target.ID, o.Name)
	if err != nil {
		return nil, err
	}
	if len(old) > 0 {
		if old[0].Live(now) {
			return nil, ErrExists
		}
		if _, err := tx.Exec(ctx, `DELETE FROM manage_grants WHERE id = ?`, old[0].ID); err != nil {
			return nil, err
		}
	}
	sj, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	ms := store.Millis(now)
	if _, err := tx.Exec(ctx, `INSERT INTO manage_grants (id, target, name, holder, holder_path, offered_by, scope, ttl_ms,
		renew_until, state, created_at, accepted_at, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?)`,
		id, target.ID, o.Name, holderID, holder, offeredBy, string(sj), ttl.Milliseconds(), renewUntil, StatePending, ms, ms); err != nil {
		return nil, err
	}
	return Get(ctx, tx, id)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Live reports whether g is PENDING, or ACTIVE and unexpired at now.
func (g *Grant) Live(now time.Time) bool {
	return g.State == StatePending || (g.State == StateActive && g.expiresAt.After(now))
}

// Get returns a grant by id.
func Get(ctx context.Context, tx store.Tx, id string) (*Grant, error) {
	gs, err := query(ctx, tx, `id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(gs) == 0 {
		return nil, ErrNotFound
	}
	return gs[0], nil
}

// ByName returns target's grant called name.
func ByName(ctx context.Context, tx store.Tx, target, name string) (*Grant, error) {
	gs, err := query(ctx, tx, `target = ? AND name = ?`, target, name)
	if err != nil {
		return nil, err
	}
	if len(gs) == 0 {
		return nil, ErrNotFound
	}
	return gs[0], nil
}

// OnTarget lists target's grants, every state, by name.
func OnTarget(ctx context.Context, tx store.Tx, target string) ([]*Grant, error) {
	return query(ctx, tx, `target = ? ORDER BY name`, target)
}

// HeldBy lists the grants offered to and held by holder: PENDING, and
// ACTIVE ones unexpired by database time.
func HeldBy(ctx context.Context, tx store.Tx, holder string) ([]*Grant, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	return query(ctx, tx, `holder = ? AND (state = ? OR (state = ? AND expires_at > ?)) ORDER BY created_at, id`,
		holder, StatePending, StateActive, store.Millis(now))
}

// Accept makes a PENDING grant ACTIVE, its lease starting now. An
// ACTIVE one is left as it is.
func Accept(ctx context.Context, tx store.Tx, g *Grant) (*Grant, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case g.State == StateActive && g.Live(now):
		return g, nil
	case g.State != StatePending:
		return nil, &Precondition{Message: fmt.Sprintf("the grant %s is %s", g.ID, stateAt(g, now))}
	}
	if _, err := tx.Exec(ctx, `UPDATE manage_grants SET state = ?, accepted_at = ?, expires_at = ?, updated_at = ? WHERE id = ?`,
		StateActive, store.Millis(now), store.Millis(now.Add(g.ttl)), store.Millis(now), g.ID); err != nil {
		return nil, err
	}
	return Get(ctx, tx, g.ID)
}

// Renew restarts an ACTIVE grant's lease from now, up to renew_until.
func Renew(ctx context.Context, tx store.Tx, g *Grant) (*Grant, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case !g.Live(now) || g.State != StateActive:
		return nil, &Precondition{Message: fmt.Sprintf("the grant %s is %s; only an ACTIVE grant can be renewed", g.ID, stateAt(g, now))}
	case g.renewUntil.IsZero():
		return nil, &Precondition{Message: fmt.Sprintf("the grant %s has no renew_until, so it can't be renewed", g.ID)}
	case !now.Before(g.renewUntil):
		return nil, &Precondition{Message: fmt.Sprintf("the grant %s can't be renewed past its renew_until, %s", g.ID, g.renewUntil.Format(time.RFC3339))}
	}
	exp := now.Add(g.ttl)
	if exp.After(g.renewUntil) {
		exp = g.renewUntil
	}
	if _, err := tx.Exec(ctx, `UPDATE manage_grants SET expires_at = ?, updated_at = ? WHERE id = ?`,
		store.Millis(exp), store.Millis(now), g.ID); err != nil {
		return nil, err
	}
	return Get(ctx, tx, g.ID)
}

// End ends a live grant: REVOKED for a revoke, give-up or deletion,
// EXPIRED once its lease ran out. It reports whether g was live.
func End(ctx context.Context, tx store.Tx, g *Grant, state string) (bool, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return false, err
	}
	if g.State != StatePending && g.State != StateActive {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE manage_grants SET state = ?, updated_at = ? WHERE id = ?`, state, store.Millis(now), g.ID); err != nil {
		return false, err
	}
	g.State = state
	return true, nil
}

// Due lists the ACTIVE grants whose lease has run out by database
// time, which End(StateExpired) ends.
func Due(ctx context.Context, tx store.Tx) ([]*Grant, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	return query(ctx, tx, `state = ? AND expires_at <= ? ORDER BY expires_at`, StateActive, store.Millis(now))
}

// AllActive lists every ACTIVE grant, for the gateway's grant table.
// One whose lease has run out stays here until the sweep ends it.
func AllActive(ctx context.Context, tx store.Tx) ([]*Grant, error) {
	return query(ctx, tx, `state = ? ORDER BY id`, StateActive)
}

// Touching lists the grants whose target or holder is silo.
func Touching(ctx context.Context, tx store.Tx, silo string) ([]*Grant, error) {
	return query(ctx, tx, `target = ? OR holder = ? ORDER BY id`, silo, silo)
}

// Forget deletes the rows of grants whose target or holder is silo,
// once they have been ended: silo is going.
func Forget(ctx context.Context, tx store.Tx, silo string) error {
	_, err := tx.Exec(ctx, `DELETE FROM manage_grants WHERE target = ? OR holder = ?`, silo, silo)
	return err
}

// stateAt is g's state as the API shows it at now: an ACTIVE grant
// whose lease has run out is EXPIRED, whether or not the sweep has
// caught up with it.
func stateAt(g *Grant, now time.Time) string {
	if g.State == StateActive && !g.expiresAt.After(now) {
		return StateExpired
	}
	return g.State
}

// Shown sets g.State to what the API shows at now.
func (g *Grant) Shown(now time.Time) *Grant {
	g.State = stateAt(g, now)
	return g
}

func query(ctx context.Context, tx store.Tx, where string, args ...any) ([]*Grant, error) {
	rows, err := tx.Query(ctx, `SELECT id, target, name, holder, holder_path, offered_by, scope, ttl_ms, renew_until, state,
		created_at, accepted_at, expires_at FROM manage_grants WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Grant
	for rows.Next() {
		var g Grant
		var scope string
		var ttl, renew, created, accepted, expires int64
		if err := rows.Scan(&g.ID, &g.Target, &g.Name, &g.holderID, &g.Holder, &g.OfferedBy, &scope, &ttl, &renew, &g.State,
			&created, &accepted, &expires); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scope), &g.Scope); err != nil {
			return nil, err
		}
		g.ttl = time.Duration(ttl) * time.Millisecond
		g.Lease.TTL = fmt.Sprintf("%gs", g.ttl.Seconds())
		if renew != 0 {
			g.renewUntil = store.Time(renew)
			g.Lease.RenewUntil = &g.renewUntil
		}
		g.CreateTime = store.Time(created)
		if accepted != 0 {
			t := store.Time(accepted)
			g.AcceptTime = &t
		}
		if expires != 0 {
			g.expiresAt = store.Time(expires)
			g.ExpireTime = &g.expiresAt
		}
		out = append(out, &g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range out {
		if t, err := tree.Get(ctx, tx, g.Target); err == nil {
			g.TargetPath = t.Path
		} else if !errors.Is(err, tree.ErrNotFound) {
			return nil, err
		}
	}
	return out, nil
}
