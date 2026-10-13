package fenceapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/control"
	"github.com/bwsalmon/silo/pkg/egress"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Each silo has seven policies of its own, stored as JSON in
// silo_policies, and an effective value of each worked out down the
// tree from the root: the silo's own narrowed by every ancestor's.

// Egress is the egress policy: hosts a silo's VMs may reach through the
// gateway. A host pattern is a name ("deb.debian.org"), "*.<domain>"
// (any name below it), or "*" (anything), with an optional ":<port>"
// (package egress). Effective allow is the intersection down the tree,
// effective deny the union; that is what the API shows, and the gateway
// decides on the levels themselves (OwnEgress).
type Egress struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// GrantPolicy is an open map of grant kind ("exports", "links", or a
// kind v1 doesn't know) to "allow" or "deny". An absent key is deny,
// and the effective value is the AND down the tree.
type GrantPolicy map[string]string

// AgentPolicy is the agent policy (fence-api.md, "Agent policy"). A nil
// list allows whatever the parent's effective policy allows; an empty
// one allows nothing. A models entry may end in "*". MaxLease "" is
// the parent's.
type AgentPolicy struct {
	Credentials []string `json:"credentials,omitempty"`
	Adapters    []string `json:"adapters,omitempty"`
	Models      []string `json:"models,omitempty"`
	MaxLease    string   `json:"max_lease,omitempty"`
}

// MarshalJSON writes an empty list as [], and a nil list not at all,
// since the two mean different things.
func (p AgentPolicy) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	if p.Credentials != nil {
		m["credentials"] = p.Credentials
	}
	if p.Adapters != nil {
		m["adapters"] = p.Adapters
	}
	if p.Models != nil {
		m["models"] = p.Models
	}
	if p.MaxLease != "" {
		m["max_lease"] = p.MaxLease
	}
	return json.Marshal(m)
}

// Isolation is the isolation policy (local-backend.md, "In a silo's
// policy", call L1): the classes containers in the silo may run in, and
// the one a container that names none gets. The effective allowed is
// the intersection down the tree.
type Isolation struct {
	Allowed []string `json:"allowed"`
	Default string   `json:"default"`
}

// The isolation classes, strongest first.
const (
	IsolationVM      = "vm"
	IsolationSandbox = "sandbox"
)

// IsolationClasses are every isolation class, strongest first. The
// root's isolation allows them all unless install narrows it.
var IsolationClasses = []string{IsolationVM, IsolationSandbox}

// RootIsolation is the root's isolation unless bootstrap is given
// another.
func RootIsolation() Isolation {
	return Isolation{Allowed: append([]string{}, IsolationClasses...), Default: IsolationVM}
}

// ReposPolicy is the repos policy (local-git.md, "What egress
// allowlists mean here", call L54): whether the silo's repos are
// mirrored into the uber repo. Effective mirror is the AND down the
// tree, so an ancestor that turns it off keeps its whole subtree's code
// on the host.
type ReposPolicy struct {
	Mirror bool `json:"mirror"`
}

// The ingress policies (local-backend.md, "Published ports"): whether a
// published port also listens on the host's loopback.
const (
	IngressNone = "none"
	IngressHost = "host"
)

// The control policies, strictest first (fence-api.md, "Control
// policy", SV-12). control.go enforces them.
const (
	ControlNone = control.None
	ControlRead = control.Read
	ControlFull = control.Full
)

var controlRank = map[string]int{ControlNone: 0, ControlRead: 1, ControlFull: 2}

// RootMaxLease is the root's agent max_lease: 7 days.
const RootMaxLease = 7 * 24 * time.Hour

// RootGrantPolicy is the root's own grant policy unless bootstrap is
// given another: exports allowed and links denied, as silo install's
// --root-grant-policy defaults (README, call 14). Links open L3 paths
// between silos, so a tree has them only when its installer says so.
var RootGrantPolicy = GrantPolicy{"exports": "allow", "links": "deny"}

// policies is one silo's own row.
type policies struct {
	Egress    Egress
	Grant     GrantPolicy
	Agent     AgentPolicy
	Control   string
	Isolation Isolation
	Ingress   string
	Repos     ReposPolicy
}

func readPolicies(ctx context.Context, tx store.Tx, silo string) (*policies, error) {
	var eg, gp, ap, ctl, iso, ing, rp string
	err := tx.QueryRow(ctx, `SELECT egress, grant_policy, agent_policy, control, isolation, ingress, repos FROM silo_policies WHERE silo = ?`, silo).
		Scan(&eg, &gp, &ap, &ctl, &iso, &ing, &rp)
	if errors.Is(err, sql.ErrNoRows) {
		return &policies{Egress: Egress{Allow: []string{}, Deny: []string{}}, Grant: GrantPolicy{}, Control: ControlFull,
			Isolation: RootIsolation(), Ingress: IngressNone, Repos: ReposPolicy{Mirror: true}}, nil
	}
	if err != nil {
		return nil, err
	}
	p := &policies{Control: ctl, Ingress: ing}
	for _, f := range []struct {
		s string
		v any
	}{{eg, &p.Egress}, {gp, &p.Grant}, {ap, &p.Agent}, {iso, &p.Isolation}, {rp, &p.Repos}} {
		if err := json.Unmarshal([]byte(f.s), f.v); err != nil {
			return nil, fmt.Errorf("silo %s policies: %w", silo, err)
		}
	}
	p.Egress = normEgress(p.Egress)
	if p.Grant == nil {
		p.Grant = GrantPolicy{}
	}
	p.Isolation = normIsolation(p.Isolation)
	return p, nil
}

func writePolicies(ctx context.Context, tx store.Tx, silo string, p *policies) error {
	eg, _ := json.Marshal(normEgress(p.Egress))
	gp, _ := json.Marshal(p.Grant)
	ap, _ := json.Marshal(p.Agent)
	iso, _ := json.Marshal(normIsolation(p.Isolation))
	rp, _ := json.Marshal(p.Repos)
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO silo_policies (silo, egress, grant_policy, agent_policy, control, isolation, ingress, repos, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (silo) DO UPDATE SET egress = excluded.egress, grant_policy = excluded.grant_policy,
			agent_policy = excluded.agent_policy, control = excluded.control, isolation = excluded.isolation,
			ingress = excluded.ingress, repos = excluded.repos, updated_at = excluded.updated_at`,
		silo, string(eg), string(gp), string(ap), p.Control, string(iso), p.Ingress, string(rp), store.Millis(now))
	return err
}

func normEgress(e Egress) Egress {
	if e.Allow == nil {
		e.Allow = []string{}
	}
	if e.Deny == nil {
		e.Deny = []string{}
	}
	return e
}

// normIsolation writes allowed strongest first, and fills in a missing
// default with the strongest class allowed.
func normIsolation(i Isolation) Isolation {
	out := Isolation{Allowed: []string{}, Default: i.Default}
	for _, c := range IsolationClasses {
		if slices.Contains(i.Allowed, c) {
			out.Allowed = append(out.Allowed, c)
		}
	}
	// A class this release doesn't know stays, for checkIsolation to
	// refuse.
	for _, c := range i.Allowed {
		if !slices.Contains(IsolationClasses, c) && !slices.Contains(out.Allowed, c) {
			out.Allowed = append(out.Allowed, c)
		}
	}
	if out.Default == "" && len(out.Allowed) > 0 {
		out.Default = out.Allowed[0]
	}
	return out
}

// narrowIsolation is parent's effective isolation narrowed by own:
// the classes both allow, and own's default if that is one of them, or
// else the strongest that is. A silo whose ancestors have since
// narrowed past all its own classes may run nothing, and has no
// default.
func narrowIsolation(parent, own Isolation) Isolation {
	out := Isolation{Allowed: []string{}}
	for _, c := range parent.Allowed {
		if slices.Contains(own.Allowed, c) {
			out.Allowed = append(out.Allowed, c)
		}
	}
	switch {
	case slices.Contains(out.Allowed, own.Default):
		out.Default = own.Default
	case len(out.Allowed) > 0:
		out.Default = out.Allowed[0]
	}
	return out
}

// effective is a silo's effective policies.
type effective struct {
	Egress      Egress
	Grant       GrantPolicy
	Agent       AgentPolicy // lists nil where every ancestor allows everything
	Control     string
	ControlFrom string // the path of the silo whose setting wins
	Isolation   Isolation
	Ingress     string // host only where every silo up to the root has host
	Repos       ReposPolicy
	ReposFrom   string // the path of the silo whose setting wins
}

// effectiveOf works out the effective policies of chain's last silo,
// chain being its ancestry, root first. own, if given, replaces the
// last silo's stored policies: what a change would make effective.
func effectiveOf(ctx context.Context, tx store.Tx, chain []*tree.Silo, own *policies) (*effective, error) {
	var eff *effective
	for i, s := range chain {
		p := own
		if i < len(chain)-1 || own == nil {
			var err error
			if p, err = readPolicies(ctx, tx, s.ID); err != nil {
				return nil, err
			}
		}
		if eff == nil {
			eff = &effective{
				Egress:      normEgress(Egress{Allow: append([]string(nil), p.Egress.Allow...), Deny: append([]string(nil), p.Egress.Deny...)}),
				Grant:       GrantPolicy{},
				Agent:       AgentPolicy{MaxLease: formatDuration(RootMaxLease)},
				Control:     ControlFull,
				ControlFrom: s.Path,
				Isolation:   narrowIsolation(RootIsolation(), p.Isolation),
				Ingress:     p.Ingress,
				Repos:       p.Repos,
				ReposFrom:   s.Path,
			}
			for k, v := range p.Grant {
				eff.Grant[k] = v
			}
			eff.Agent = narrowAgent(eff.Agent, p.Agent)
			continue
		}
		eff.Egress = Egress{Allow: intersect(eff.Egress.Allow, p.Egress.Allow, hostCovers), Deny: union(eff.Egress.Deny, p.Egress.Deny)}
		g := GrantPolicy{}
		for k := range eff.Grant {
			g[k] = "deny"
		}
		for k := range p.Grant {
			g[k] = "deny"
		}
		for k := range g {
			if eff.Grant[k] == "allow" && p.Grant[k] == "allow" {
				g[k] = "allow"
			}
		}
		eff.Grant = g
		eff.Agent = narrowAgent(eff.Agent, p.Agent)
		if controlRank[p.Control] < controlRank[eff.Control] {
			eff.Control, eff.ControlFrom = p.Control, s.Path
		}
		eff.Isolation = narrowIsolation(eff.Isolation, p.Isolation)
		if p.Ingress != IngressHost {
			eff.Ingress = IngressNone
		}
		if eff.Repos.Mirror && !p.Repos.Mirror {
			eff.Repos, eff.ReposFrom = p.Repos, s.Path
		}
	}
	return eff, nil
}

// narrowAgent is parent's effective agent policy narrowed by own.
func narrowAgent(parent, own AgentPolicy) AgentPolicy {
	out := AgentPolicy{MaxLease: parent.MaxLease}
	out.Credentials = narrowList(parent.Credentials, own.Credentials, exactCovers)
	out.Adapters = narrowList(parent.Adapters, own.Adapters, exactCovers)
	out.Models = narrowList(parent.Models, own.Models, modelCovers)
	if own.MaxLease != "" {
		if d, err := parseDuration(own.MaxLease); err == nil {
			if pd, err := parseDuration(parent.MaxLease); err != nil || d < pd {
				out.MaxLease = formatDuration(d)
			}
		}
	}
	return out
}

// narrowList intersects allowlists, nil meaning "everything".
func narrowList(parent, own []string, covers func(a, b string) bool) []string {
	switch {
	case own == nil && parent == nil:
		return nil
	case own == nil:
		return append([]string{}, parent...)
	case parent == nil:
		return append([]string{}, own...)
	}
	return intersect(parent, own, covers)
}

// intersect is the patterns matched by both a and b, where covers(x,
// y) says every name y matches is matched by x.
func intersect(a, b []string, covers func(x, y string) bool) []string {
	set := map[string]bool{}
	for _, x := range a {
		for _, y := range b {
			switch {
			case covers(x, y):
				set[y] = true
			case covers(y, x):
				set[x] = true
			}
		}
	}
	return sorted(set)
}

func union(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range append(append([]string{}, a...), b...) {
		set[x] = true
	}
	return sorted(set)
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func exactCovers(a, b string) bool { return a == b }

// modelCovers: "claude-*" covers "claude-opus" and "claude-o*".
func modelCovers(a, b string) bool {
	if p, ok := strings.CutSuffix(a, "*"); ok {
		return strings.HasPrefix(strings.TrimSuffix(b, "*"), p)
	}
	return a == b
}

// hostCovers: "*" covers everything, "*.x.org" covers "a.x.org" and
// "*.a.x.org", and a name covers itself, each at the same port
// (egress.Covers).
func hostCovers(a, b string) bool { return egress.Covers(a, b) }

// coveredBy reports whether every pattern of b is covered by one of a.
func coveredBy(a, b []string, covers func(x, y string) bool) (bool, string) {
	for _, y := range b {
		ok := false
		for _, x := range a {
			ok = ok || covers(x, y)
		}
		if !ok {
			return false, y
		}
	}
	return true, ""
}

func checkEgress(e Egress) error {
	if err := (egress.Policy{Allow: e.Allow, Deny: e.Deny}).Valid(); err != nil {
		return errorf(InvalidArgument, "%v", err).with("field", "egress")
	}
	return nil
}

// OwnEgress is every silo's own egress policy, by id, for the gateway,
// which works out a silo's effective policy from its ancestry's own
// policies level by level (gateway.md, "Policy", SV-86). A silo with no
// row has an empty policy, which allows nothing.
func OwnEgress(ctx context.Context, tx store.Tx) (map[string]egress.Policy, error) {
	rows, err := tx.Query(ctx, `SELECT silo, egress FROM silo_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]egress.Policy{}
	for rows.Next() {
		var id, eg string
		if err := rows.Scan(&id, &eg); err != nil {
			return nil, err
		}
		var e Egress
		if err := json.Unmarshal([]byte(eg), &e); err != nil {
			return nil, fmt.Errorf("silo %s egress policy: %w", id, err)
		}
		e = normEgress(e)
		out[id] = egress.Policy{Allow: e.Allow, Deny: e.Deny}
	}
	return out, rows.Err()
}

func checkGrant(g GrantPolicy) error {
	for k, v := range g {
		if k == "" || (v != "allow" && v != "deny") {
			return errorf(InvalidArgument, "grant_policy %q: %q, want allow or deny", k, v).with("field", "grant_policy")
		}
	}
	return nil
}

func checkAgent(a AgentPolicy) error {
	if a.MaxLease != "" {
		if d, err := parseDuration(a.MaxLease); err != nil || d <= 0 {
			return errorf(InvalidArgument, "agent_policy max_lease %q: want a positive duration such as 24h", a.MaxLease).with("field", "agent_policy")
		}
	}
	return nil
}

func checkIsolation(i Isolation) error {
	bad := func(format string, args ...any) error {
		return errorf(InvalidArgument, "isolation: "+format, args...).with("field", "isolation")
	}
	if len(i.Allowed) == 0 {
		return bad("allowed is empty: want one or more of %s", strings.Join(IsolationClasses, ", "))
	}
	for _, c := range i.Allowed {
		if !slices.Contains(IsolationClasses, c) {
			return bad("class %q: want %s", c, strings.Join(IsolationClasses, " or "))
		}
	}
	if !slices.Contains(i.Allowed, i.Default) {
		return bad("default %q is not in allowed %v", i.Default, i.Allowed)
	}
	return nil
}

func checkIngress(i string) error {
	if i != IngressHost && i != IngressNone {
		return errorf(InvalidArgument, "ingress %q: want host or none", i).with("field", "ingress")
	}
	return nil
}

func checkControl(c string) error {
	if _, ok := controlRank[c]; !ok {
		return errorf(InvalidArgument, "control %q: want full, read or none", c).with("field", "control")
	}
	return nil
}

// looser checks own against the parent's effective policies, and
// returns LOOSER_THAN_PARENT naming the first thing own has that the
// parent lacks.
func looser(parent *effective, own *policies) error {
	if ok, h := coveredBy(parent.Egress.Allow, own.Egress.Allow, hostCovers); !ok {
		return errorf(LooserThanParent, "egress host %s is not allowed to the parent", h).with("policy", "egress").with("host", h)
	}
	for k, v := range own.Grant {
		if v == "allow" && parent.Grant[k] != "allow" {
			return errorf(LooserThanParent, "grant kind %s is not allowed to the parent", k).with("policy", "grant_policy").with("kind", k)
		}
	}
	for _, l := range []struct {
		field     string
		par, mine []string
		covers    func(a, b string) bool
	}{
		{"credentials", parent.Agent.Credentials, own.Agent.Credentials, exactCovers},
		{"adapters", parent.Agent.Adapters, own.Agent.Adapters, exactCovers},
		{"models", parent.Agent.Models, own.Agent.Models, modelCovers},
	} {
		if l.par == nil || l.mine == nil {
			continue
		}
		if ok, x := coveredBy(l.par, l.mine, l.covers); !ok {
			return errorf(LooserThanParent, "agent_policy %s %s is not allowed to the parent", l.field, x).
				with("policy", "agent_policy").with("field", l.field).with("value", x)
		}
	}
	if own.Agent.MaxLease != "" {
		d, _ := parseDuration(own.Agent.MaxLease)
		pd, _ := parseDuration(parent.Agent.MaxLease)
		if d > pd {
			return errorf(LooserThanParent, "agent_policy max_lease %s is longer than the parent's %s", own.Agent.MaxLease, parent.Agent.MaxLease).
				with("policy", "agent_policy").with("field", "max_lease")
		}
	}
	if controlRank[own.Control] > controlRank[parent.Control] {
		return errorf(LooserThanParent, "control %s is looser than the parent's %s (from %s)", own.Control, parent.Control, parent.ControlFrom).
			with("policy", "control").with("parent", parent.Control)
	}
	for _, c := range own.Isolation.Allowed {
		if !slices.Contains(parent.Isolation.Allowed, c) {
			return errorf(LooserThanParent, "isolation class %s is not allowed to the parent", c).
				with("policy", "isolation").with("class", c)
		}
	}
	if own.Ingress == IngressHost && parent.Ingress != IngressHost {
		return errorf(LooserThanParent, "ingress host is not allowed to the parent").
			with("policy", "ingress").with("parent", parent.Ingress)
	}
	if own.Repos.Mirror && !parent.Repos.Mirror {
		return errorf(LooserThanParent, "repos mirror is off for the parent (from %s)", parent.ReposFrom).
			with("policy", "repos").with("from", parent.ReposFrom)
	}
	return nil
}

// narrower checks that a silo changing its own policies from old to
// next only narrows them: no host, grant kind, credential, adapter or
// model it didn't allow itself, no deny entry dropped, no longer
// max_lease, no isolation class it didn't allow, no ingress host it
// didn't have, no repos mirror it had turned off. Anything wider is PERMISSION_DENIED, since only an
// ancestor may widen a silo's policies.
func narrower(old, next *policies) error {
	deny := func(policy, format string, args ...any) error {
		return errorf(PermissionDenied, "a silo may only narrow its own %s: "+format, append([]any{policy}, args...)...).with("policy", policy)
	}
	if ok, h := coveredBy(old.Egress.Allow, next.Egress.Allow, hostCovers); !ok {
		return deny("egress", "%s is not in its allow list", h)
	}
	if ok, h := coveredBy(next.Egress.Deny, old.Egress.Deny, exactCovers); !ok {
		return deny("egress", "%s can't be taken off its deny list", h)
	}
	for k, v := range next.Grant {
		if v == "allow" && old.Grant[k] != "allow" {
			return deny("grant_policy", "%s is not allowed now", k)
		}
	}
	for _, l := range []struct {
		field     string
		old, next []string
		covers    func(a, b string) bool
	}{
		{"credentials", old.Agent.Credentials, next.Agent.Credentials, exactCovers},
		{"adapters", old.Agent.Adapters, next.Agent.Adapters, exactCovers},
		{"models", old.Agent.Models, next.Agent.Models, modelCovers},
	} {
		if l.old == nil {
			continue
		}
		if l.next == nil {
			return deny("agent_policy", "%s can't go back to inheriting", l.field)
		}
		if ok, x := coveredBy(l.old, l.next, l.covers); !ok {
			return deny("agent_policy", "%s %s is not allowed now", l.field, x)
		}
	}
	if old.Agent.MaxLease != "" {
		od, _ := parseDuration(old.Agent.MaxLease)
		nd, err := parseDuration(next.Agent.MaxLease)
		if next.Agent.MaxLease == "" || err != nil || nd > od {
			return deny("agent_policy", "max_lease can't be longer than %s", old.Agent.MaxLease)
		}
	}
	for _, c := range next.Isolation.Allowed {
		if !slices.Contains(old.Isolation.Allowed, c) {
			return deny("isolation", "class %s is not allowed now", c)
		}
	}
	if next.Ingress == IngressHost && old.Ingress != IngressHost {
		return deny("ingress", "host is not allowed now")
	}
	if next.Repos.Mirror && !old.Repos.Mirror {
		return deny("repos", "mirror is off now")
	}
	return nil
}

// parseDuration reads a duration, also accepting a whole number of
// days ("7d").
func parseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var d int
		if _, err := fmt.Sscanf(n, "%d", &d); err != nil || fmt.Sprint(d) != n {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// formatDuration writes whole hours as "168h", and anything else as Go
// does.
func formatDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return d.String()
}
