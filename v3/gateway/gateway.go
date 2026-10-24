// Package gateway is silo's root daemon (design §4): it creates agent users,
// starts each agent as a sandboxed transient systemd service, keeps the
// parent/child tree, stops whole subtrees, and writes the audit log. Callers
// reach it over a Unix socket and are identified by SO_PEERCRED.
package gateway

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Deps are the gateway's connections to the host.
type Deps struct {
	Systemd  Systemd
	Users    Users
	Procs    Procs
	Clock    Clock
	Launcher Launcher
	Audit    *AuditLog
	Store    Store
	Logf     func(format string, args ...any)
}

// Gateway holds the agent tree. All methods are safe for concurrent use;
// they are serialised by one mutex, which also orders the audit log.
type Gateway struct {
	cfg *Config
	d   Deps

	mu        sync.Mutex
	st        *State
	bridgeUID int // -1 if the bridge user does not exist
}

// New loads the persisted state. Call Start before serving.
func New(cfg *Config, d Deps) (*Gateway, error) {
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	if d.Clock == nil {
		d.Clock = SystemClock{}
	}
	st, err := d.Store.Load()
	if err != nil {
		return nil, err
	}
	return &Gateway{cfg: cfg, d: d, st: st, bridgeUID: -1}, nil
}

// Lifetime is how long an agent lives (design §8.1). In M1a "until-done"
// and "persistent" both mean "until its process exits or it is stopped".
type Lifetime struct {
	Kind    string `json:"kind"`
	Seconds int    `json:"seconds,omitempty"`
}

const (
	LifetimeUntilDone  = "until-done"
	LifetimeTTL        = "ttl"
	LifetimePersistent = "persistent"
)

func (l Lifetime) validate() error {
	switch l.Kind {
	case LifetimeUntilDone, LifetimePersistent:
		if l.Seconds != 0 {
			return fmt.Errorf("lifetime %s takes no seconds", l.Kind)
		}
	case LifetimeTTL:
		if l.Seconds <= 0 {
			return fmt.Errorf("lifetime ttl needs seconds > 0")
		}
	default:
		return fmt.Errorf("lifetime kind %q: want until-done, ttl or persistent", l.Kind)
	}
	return nil
}

// maxDepth is how many levels of agents may sit below root (design §8.3).
const maxDepth = 3

var (
	agentIDRe   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,23}[a-z0-9])?$`)
	childUserRe = regexp.MustCompile(`^silo-a-[a-z0-9]([a-z0-9-]{0,23}[a-z0-9])?$`)
)

// Start reconciles the persisted agents with systemd, re-adopting units
// that are still running and tombstoning those that are not, then creates
// the root agent if it is not running.
func (g *Gateway) Start(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if u, err := g.d.Users.Lookup(g.cfg.BridgeUser); err != nil {
		return err
	} else if u != nil {
		g.bridgeUID = u.UID
	}
	if _, err := g.d.Audit.Append("gateway", EvGatewayStart, map[string]any{"pid": os.Getpid(), "agents": len(g.st.Agents)}); err != nil {
		return err
	}
	if err := g.reconcileLocked(ctx, true); err != nil {
		return err
	}
	return g.ensureRootLocked(ctx)
}

// Run reconciles periodically until ctx is done.
func (g *Gateway) Run(ctx context.Context) {
	t := time.NewTicker(time.Duration(g.cfg.ReconcileIntervalSec) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := g.Reconcile(ctx); err != nil {
				g.d.Logf("reconcile: %v", err)
			}
		}
	}
}

// Reconcile notices agents whose units have ended.
func (g *Gateway) Reconcile(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reconcileLocked(ctx, false)
}

func (g *Gateway) reconcileLocked(ctx context.Context, startup bool) error {
	changed := false
	for _, a := range g.byDepth() {
		if a.State != StateRunning {
			continue // stopped earlier in this pass, with its parent
		}
		st, err := g.d.Systemd.Status(ctx, a.Unit)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.ID, err)
		}
		if st.Active() {
			if startup {
				g.audit("gateway", EvAdopt, map[string]any{"agent": a.ID, "unit": a.Unit, "state": st.String(), "main_pid": st.MainPID})
			}
			continue
		}
		cause := "exited"
		if startup {
			cause = "exited while the gateway was down"
		}
		g.tombstone(a, st, cause, StateExited)
		if st.Exists {
			// The tombstone holds the result now; free the unit name.
			_ = g.d.Systemd.ResetFailed(ctx, a.Unit)
		}
		g.audit("gateway", EvExit, map[string]any{"agent": a.ID, "tombstone": a.Tombstone})
		changed = true
		// A parent's exit stops its subtree (on_parent_exit "stop", design
		// §8.4). The root agent's children are left alone.
		if !a.Root {
			if _, err := g.stopSubtreeLocked(ctx, a, false, false, "gateway", ""); err != nil {
				g.d.Logf("stopping children of exited agent %s: %v", a.ID, err)
			}
		}
	}
	if changed {
		if err := g.saveLocked(); err != nil {
			return err
		}
	}
	if !startup {
		if r := g.st.Agents[RootAgentID]; r != nil && r.State == StateExited && r.Tombstone != nil &&
			r.Tombstone.Time.Sub(r.Created) >= rootRestartMinUptime {
			g.d.Logf("root agent exited (%s); restarting it", r.Tombstone.LastState)
			return g.ensureRootLocked(ctx)
		}
	}
	return nil
}

// rootRestartMinUptime keeps a root agent that dies at once from being
// restarted in a loop; it is restarted again on the next gateway start.
const rootRestartMinUptime = time.Minute

// byDepth lists the current agents parents-first, then by id.
func (g *Gateway) byDepth() []*Agent {
	out := make([]*Agent, 0, len(g.st.Agents))
	for _, a := range g.st.Agents {
		out = append(out, a)
	}
	depth := func(a *Agent) int { return len(g.chain(a)) }
	sort.Slice(out, func(i, j int) bool {
		di, dj := depth(out[i]), depth(out[j])
		if di != dj {
			return di < dj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// chain is the list of ids from a's top-level ancestor (excluding root) down
// to a. For root it is empty.
func (g *Gateway) chain(a *Agent) []string {
	if a.Root {
		return nil
	}
	var rev []string
	seen := map[string]bool{}
	for cur := a; cur != nil && !cur.Root && !seen[cur.ID]; cur = g.st.Agents[cur.Parent] {
		seen[cur.ID] = true
		rev = append(rev, cur.ID)
		if cur.Parent == "" {
			break
		}
	}
	out := make([]string, len(rev))
	for i, id := range rev {
		out[len(rev)-1-i] = id
	}
	return out
}

func (g *Gateway) ensureRootLocked(ctx context.Context) error {
	if !g.cfg.RootEnabled() {
		return nil
	}
	if r := g.st.Agents[RootAgentID]; r != nil && r.State == StateRunning {
		return nil
	}
	_, err := g.spawnLocked(ctx, "gateway", spawnSpec{
		ID: RootAgentID, Root: true, User: RootUser, CLI: g.cfg.RootAgent.CLI,
		Brief: g.cfg.RootAgent.Brief, Limits: g.cfg.RootAgent.Limits,
		Lifetime: Lifetime{Kind: LifetimePersistent},
	})
	if err != nil {
		return fmt.Errorf("starting the root agent: %w", err)
	}
	return nil
}

// ---- spawn -----------------------------------------------------------------

// SpawnParams is the spawn request (design §8.1, the parts M1a implements).
type SpawnParams struct {
	Relay
	// Name is the agent id, also used in its unit name.
	Name string `json:"name"`
	// User defaults to silo-a-<name>.
	User string `json:"user,omitempty"`
	// Parent may be set only by a human; for agents it is the caller.
	Parent   string   `json:"parent,omitempty"`
	CLI      string   `json:"cli"`
	Brief    string   `json:"brief"`
	Limits   Limits   `json:"limits"`
	Lifetime Lifetime `json:"lifetime"`
}

// SpawnResult is what spawn returns (design §8.2, without the ACP session,
// which M1b adds).
type SpawnResult struct {
	Agent string `json:"agent"`
	User  string `json:"user"`
	Unit  string `json:"unit"`
	Slice string `json:"slice"`
}

type spawnSpec struct {
	ID, Parent, User, CLI, Brief string
	Root                         bool
	Limits                       Limits
	Lifetime                     Lifetime
}

// Spawn starts a new agent for caller.
func (g *Gateway) Spawn(ctx context.Context, c Caller, p SpawnParams) (*SpawnResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Only the human and the root agent may spawn in M1 (may_spawn is M2).
	if !c.Human() && c.Agent != RootAgentID {
		return nil, g.deny(c, "spawn", "only the human and the root agent may spawn agents")
	}
	parent := p.Parent
	if !c.Human() {
		if p.Parent != "" && p.Parent != c.Agent {
			return nil, g.deny(c, "spawn", "agents spawn only their own children")
		}
		parent = c.Agent
	}
	if !agentIDRe.MatchString(p.Name) {
		return nil, invalidParams("name %q: want 1-25 lower-case letters, digits and inner dashes", p.Name)
	}
	if p.Name == RootAgentID {
		return nil, invalidParams("name %q is reserved for the root agent", p.Name)
	}
	user := p.User
	if user == "" {
		user = ChildUserPrefix + p.Name
	}
	if !childUserRe.MatchString(user) {
		return nil, invalidParams("user %q: agent users are named %s<name>", user, ChildUserPrefix)
	}
	if err := p.Limits.Validate(); err != nil {
		return nil, invalidParams("limits: %v", err)
	}
	lt := p.Lifetime
	if lt.Kind == "" {
		lt.Kind = LifetimeUntilDone
	}
	if err := lt.validate(); err != nil {
		return nil, invalidParams("%v", err)
	}
	if parent != "" {
		pa := g.st.Agents[parent]
		if pa == nil || pa.State != StateRunning {
			return nil, invalidParams("parent %q is not a running agent", parent)
		}
		if len(g.chain(pa)) >= maxDepth {
			return nil, invalidParams("parent %q is already %d levels below root", parent, maxDepth)
		}
	}
	return g.spawnLocked(ctx, c.Actor(), spawnSpec{
		ID: p.Name, Parent: parent, User: user, CLI: p.CLI, Brief: p.Brief,
		Limits: p.Limits.withDefaults(g.cfg.DefaultLimits), Lifetime: lt,
	})
}

func (g *Gateway) spawnLocked(ctx context.Context, actor string, s spawnSpec) (res *SpawnResult, err error) {
	defer func() {
		if err != nil {
			g.audit(actor, EvSpawnFailed, map[string]any{"agent": s.ID, "user": s.User, "parent": s.Parent, "cli": s.CLI, "error": err.Error()})
		}
	}()
	prev := g.st.Agents[s.ID]
	if prev != nil && prev.State == StateRunning {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("agent %q is already running", s.ID)}
	}
	for _, a := range g.st.Agents {
		if a.State == StateRunning && a.User == s.User {
			return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("user %s is in use by agent %q", s.User, a.ID)}
		}
	}
	a := &Agent{
		ID: s.ID, Root: s.Root, User: s.User, CLI: s.CLI, Brief: s.Brief, Parent: s.Parent,
		Unit: unitName(s.ID), Limits: s.Limits, Lifetime: s.Lifetime, State: StateRunning,
		Created: g.d.Clock.Now(), SpawnedBy: actor,
	}
	// The chain walk needs the parent link only, so compute the slice from
	// a provisional view of the tree.
	if s.Root {
		a.Slice = rootSlice
	} else {
		a.Slice = sliceName(append(g.chainOf(s.Parent), s.ID))
	}

	u, err := g.ensureUserLocked(actor, s.User)
	if err != nil {
		return nil, err
	}
	a.UID, a.Home = u.UID, u.Home

	proc, err := g.d.Launcher.Prepare(ctx, LaunchRequest{Agent: a.ID, CLI: a.CLI, Brief: a.Brief, User: *u, Root: a.Root})
	if err != nil {
		return nil, err
	}

	if st, err := g.d.Systemd.Status(ctx, a.Unit); err != nil {
		return nil, err
	} else if st.Active() {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("unit %s is already running but not known to the gateway", a.Unit)}
	} else if st.Exists {
		_ = g.d.Systemd.ResetFailed(ctx, a.Unit)
	}

	env := map[string]string{
		"HOME": u.Home, "USER": u.Name, "LOGNAME": u.Name, "SILO_AGENT": a.ID,
		"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8",
	}
	for k, v := range proc.Env {
		env[k] = v
	}
	spec := UnitSpec{
		Name: a.Unit, Slice: a.Slice, Description: "silo agent " + a.ID,
		Argv: proc.Argv, Env: env, Properties: serviceProps(g.cfg, a, *u),
	}
	if err := g.d.Systemd.StartService(ctx, spec); err != nil {
		return nil, fmt.Errorf("starting %s: %w", a.Unit, err)
	}
	if !a.Root {
		// The subtree's slice carries the limits too, so descendants are
		// capped together with the agent (design §4.2).
		if err := g.d.Systemd.SetProperties(ctx, a.Slice, limitProps(a.Limits)); err != nil {
			_ = g.d.Systemd.Stop(ctx, a.Slice)
			return nil, fmt.Errorf("limiting %s: %w", a.Slice, err)
		}
	}
	if prev != nil {
		g.st.History = append(g.st.History, prev)
	}
	g.st.Agents[a.ID] = a
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(actor, EvSpawn, map[string]any{
		"agent": a.ID, "root": a.Root, "parent": a.Parent, "user": a.User, "uid": a.UID,
		"cli": a.CLI, "brief": a.Brief, "argv": proc.Argv, "limits": a.Limits, "lifetime": a.Lifetime,
		"unit": a.Unit, "slice": a.Slice,
	})
	return &SpawnResult{Agent: a.ID, User: a.User, Unit: a.Unit, Slice: a.Slice}, nil
}

func (g *Gateway) chainOf(id string) []string {
	if a := g.st.Agents[id]; a != nil {
		return g.chain(a)
	}
	return nil
}

// ensureUserLocked returns the silo-managed user name, creating it if it
// does not exist. It never returns uid 0 or a user silo did not create.
func (g *Gateway) ensureUserLocked(actor, name string) (*UserInfo, error) {
	u, err := g.d.Users.Lookup(name)
	if err != nil {
		return nil, err
	}
	if u != nil {
		recorded, ok := g.st.Users[name]
		inRange := u.UID >= g.cfg.UIDMin && u.UID <= g.cfg.UIDMax
		if u.UID == 0 || !inRange || (ok && recorded != u.UID) {
			return nil, &Error{Code: CodeForbidden, Message: fmt.Sprintf("user %s (uid %d) exists and was not created by silo", name, u.UID)}
		}
		if !ok {
			// A user in silo's uid range was made by silo (or its
			// installer); adopt it into the record.
			g.st.Users[name] = u.UID
		}
		return u, nil
	}
	used := map[int]bool{}
	for _, uid := range g.st.Users {
		used[uid] = true
	}
	uid := -1
	for cand := g.cfg.UIDMin; cand <= g.cfg.UIDMax; cand++ {
		if used[cand] {
			continue
		}
		taken, err := g.d.Users.UIDTaken(cand)
		if err != nil {
			return nil, err
		}
		if !taken {
			uid = cand
			break
		}
	}
	if uid < 0 {
		return nil, fmt.Errorf("no free uid in %d-%d", g.cfg.UIDMin, g.cfg.UIDMax)
	}
	if err := os.MkdirAll(g.cfg.HomeRoot, 0o755); err != nil {
		return nil, err
	}
	home := filepath.Join(g.cfg.HomeRoot, name)
	u, err = g.d.Users.Create(name, uid, home)
	if err != nil {
		return nil, fmt.Errorf("creating user %s: %w", name, err)
	}
	if u == nil || u.UID != uid {
		return nil, fmt.Errorf("creating user %s: it does not have uid %d afterwards", name, uid)
	}
	g.st.Users[name] = uid
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(actor, EvUserCreate, map[string]any{"user": name, "uid": uid, "home": home})
	return u, nil
}

// ---- stop ------------------------------------------------------------------

// StopParams is the stop request.
type StopParams struct {
	Relay
	Agent string `json:"agent"`
	// Mode is "graceful" (SIGTERM, then SIGKILL after 30s; the default) or
	// "kill" (SIGKILL at once).
	Mode string `json:"mode,omitempty"`
}

// StopResult lists every agent that was stopped, the target first.
type StopResult struct {
	Stopped   []string   `json:"stopped"`
	Tombstone *Tombstone `json:"tombstone"`
}

// Stop stops an agent and every descendant, leaving tombstones.
func (g *Gateway) Stop(ctx context.Context, c Caller, p StopParams) (*StopResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !c.Human() && !g.isDescendant(p.Agent, c.Agent) {
		return nil, g.deny(c, "stop", fmt.Sprintf("agent %q is not a descendant of %q", p.Agent, c.Agent))
	}
	kill := false
	switch p.Mode {
	case "", "graceful":
	case "kill":
		kill = true
	default:
		return nil, invalidParams("mode %q: want graceful or kill", p.Mode)
	}
	a := g.st.Agents[p.Agent]
	if a == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no agent %q", p.Agent)}
	}
	if a.State != StateRunning {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("agent %q is not running (%s)", p.Agent, a.State)}
	}
	stopped, err := g.stopSubtreeLocked(ctx, a, true, kill, c.Actor(), "stopped by "+c.Actor())
	if err != nil {
		return nil, err
	}
	return &StopResult{Stopped: stopped, Tombstone: a.Tombstone}, nil
}

// stopSubtreeLocked stops a's running descendants and, with self, a itself.
// It tombstones each one, saves, and writes one stop audit entry.
func (g *Gateway) stopSubtreeLocked(ctx context.Context, a *Agent, self, kill bool, actor, cause string) ([]string, error) {
	var targets []*Agent // a (if self) then descendants, parents first
	if self {
		targets = append(targets, a)
	}
	for _, d := range g.byDepth() {
		if d.State == StateRunning && g.isDescendant(d.ID, a.ID) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	last := map[string]UnitStatus{}
	for _, t := range targets {
		st, err := g.d.Systemd.Status(ctx, t.Unit)
		if err != nil {
			return nil, err
		}
		last[t.ID] = st
	}
	if kill {
		for i := len(targets) - 1; i >= 0; i-- {
			_ = g.d.Systemd.Kill(ctx, targets[i].Unit)
		}
	}
	// Stop the deepest first. A non-root agent's slice holds its whole
	// subtree, so stopping it is the one step that matters; the per-unit
	// stops make sure nothing is missed if a unit sits elsewhere.
	var firstErr error
	for i := len(targets) - 1; i >= 0; i-- {
		t := targets[i]
		if !t.Root {
			if err := g.d.Systemd.Stop(ctx, t.Slice); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		_ = g.d.Systemd.Stop(ctx, t.Unit)
	}
	var stopped []string
	var stillUp []string
	for _, t := range targets {
		st, err := g.d.Systemd.Status(ctx, t.Unit)
		if err != nil {
			return nil, err
		}
		if st.Active() {
			stillUp = append(stillUp, t.ID)
			continue
		}
		tc := cause
		switch {
		case t == a && self:
		case t.Parent == a.ID && !self:
			tc = fmt.Sprintf("parent %s exited", t.Parent)
		default:
			tc = fmt.Sprintf("parent %s stopped", t.Parent)
		}
		g.tombstone(t, last[t.ID], tc, StateStopped)
		if st.Exists {
			_ = g.d.Systemd.ResetFailed(ctx, t.Unit)
		}
		stopped = append(stopped, t.ID)
	}
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	payload := map[string]any{"agent": a.ID, "kill": kill, "stopped": stopped}
	if self {
		payload["tombstone"] = a.Tombstone
	}
	if len(stillUp) > 0 {
		payload["still_running"] = stillUp
	}
	g.audit(actor, EvStop, payload)
	if len(stillUp) > 0 {
		if firstErr == nil {
			firstErr = fmt.Errorf("units still active")
		}
		return stopped, fmt.Errorf("could not stop %v: %w", stillUp, firstErr)
	}
	return stopped, nil
}

// tombstone records how a ended. Must be called with g.mu held.
func (g *Gateway) tombstone(a *Agent, last UnitStatus, cause, state string) {
	a.State = state
	a.Tombstone = &Tombstone{
		Time: g.d.Clock.Now(), LastState: last.String(), Result: last.Result,
		ExitStatus: last.ExecMainStatus, Cause: cause,
	}
	if c := g.childrenOf(a.ID); len(c) > 0 {
		a.Tombstone.Children = c
	}
}

func (g *Gateway) childrenOf(id string) []string {
	out := []string{}
	for _, c := range g.st.Agents {
		if c.Parent == id {
			out = append(out, c.ID)
		}
	}
	sort.Strings(out)
	return out
}

// ---- list and status ----------------------------------------------------

// ListParams filters list.
type ListParams struct {
	Relay
	Parent *string  `json:"parent,omitempty"`
	State  []string `json:"state,omitempty"`
	// History includes earlier incarnations of reused ids.
	History bool `json:"history,omitempty"`
}

// AgentInfo is an agent as list and status report it.
type AgentInfo struct {
	Agent
	Children []string    `json:"children"`
	UnitNow  *UnitStatus `json:"unit_status,omitempty"`
}

// List returns the agents the caller may see, sorted by id.
func (g *Gateway) List(_ context.Context, c Caller, p ListParams) ([]AgentInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	want := map[string]bool{}
	for _, s := range p.State {
		want[s] = true
	}
	agents := g.byDepth()
	if p.History {
		agents = append(agents, g.st.History...)
	}
	out := []AgentInfo{}
	for _, a := range agents {
		if !g.canSee(c, a.ID) {
			continue
		}
		if p.Parent != nil && a.Parent != *p.Parent {
			continue
		}
		if len(want) > 0 && !want[a.State] {
			continue
		}
		info := AgentInfo{Agent: *a, Children: g.childrenOf(a.ID)}
		info.Brief = ""
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// StatusParams names one agent.
type StatusParams struct {
	Relay
	Agent string `json:"agent"`
}

// Status returns one agent with its live unit state.
func (g *Gateway) Status(ctx context.Context, c Caller, p StatusParams) (*AgentInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.st.Agents[p.Agent]
	if a == nil || !g.canSee(c, p.Agent) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no agent %q", p.Agent)}
	}
	info := &AgentInfo{Agent: *a, Children: g.childrenOf(a.ID)}
	if a.State == StateRunning {
		st, err := g.d.Systemd.Status(ctx, a.Unit)
		if err != nil {
			return nil, err
		}
		info.UnitNow = &st
	}
	return info, nil
}

// ---- helpers ---------------------------------------------------------------

func (g *Gateway) saveLocked() error { return g.d.Store.Save(g.st) }

// audit appends an entry. A failure to audit is logged loudly; the action
// it describes has already happened.
func (g *Gateway) audit(actor, event string, payload any) {
	if _, err := g.d.Audit.Append(actor, event, payload); err != nil {
		g.d.Logf("AUDIT WRITE FAILED: %s %s: %v", actor, event, err)
	}
}

// deny audits an authorization denial and returns the error for it.
func (g *Gateway) deny(c Caller, method, why string) error {
	g.audit(c.Actor(), EvAuthDeny, map[string]any{"method": method, "uid": c.UID, "pid": c.PID, "reason": why})
	return &Error{Code: CodeForbidden, Message: "permission denied: " + why}
}

func invalidParams(format string, args ...any) error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}
