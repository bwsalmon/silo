// Package gateway is silo's root daemon (design §4): the Silo Hub's record
// of every agent. Agents run wherever their registrar put them (a systemd
// unit, a container, a microVM, a subprocess) and dial in with
// `silo connect`; the gateway registers them, hands out and checks their
// tokens, keeps the parent/child tree, routes parent → child messages,
// records page links and secret requests, stops whole subtrees, and writes
// the audit log. Callers reach it over a Unix socket and are identified by
// SO_PEERCRED or by an agent token.
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
	Clock Clock
	Audit *AuditLog
	Store Store
	// LookupUID returns the uid of a user name, or -1 if there is none.
	// It is used to recognise the bridge.
	LookupUID func(name string) (int, error)
	Logf      func(format string, args ...any)
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

// maxDepth is how many levels of agents may sit below root (design §8.3).
const maxDepth = 3

// Bounds on what one caller can make the gateway hold.
const (
	maxInbox       = 100
	maxMessage     = 64 << 10
	maxBrief       = 64 << 10
	maxPendingSecs = 20
	maxPages       = 50 // per owner
)

var (
	agentIDRe  = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,23}[a-z0-9])?$`)
	unixUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	runtimeRe  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,31}$`)
)

var locationKinds = map[string]bool{
	"systemd": true, "container": true, "microvm": true, "subprocess": true, "remote": true, "other": true,
}

// Start seeds the standing agents (root and the default user agent) on
// first start and re-arms any that are not connected, then times out
// connections that went quiet while the gateway was down.
func (g *Gateway) Start(_ context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.d.LookupUID != nil {
		uid, err := g.d.LookupUID(g.cfg.BridgeUser)
		if err != nil {
			return err
		}
		g.bridgeUID = uid
	}
	if _, err := g.d.Audit.Append("gateway", EvGatewayStart, map[string]any{"pid": os.Getpid(), "agents": len(g.st.Agents)}); err != nil {
		return err
	}
	g.expireLocked()
	if g.cfg.RootEnabled() {
		if err := g.ensureStandingLocked(RootAgentID, "", true, g.cfg.RootAgent, true); err != nil {
			return fmt.Errorf("seeding the root agent: %w", err)
		}
	}
	if g.cfg.UserAgentEnabled() {
		// The default user agent is the root agent's child, so root may
		// message it and it may never message root (design §8.7).
		parent := ""
		if g.cfg.RootEnabled() {
			parent = RootAgentID
		}
		if err := g.ensureStandingLocked(g.cfg.UserAgent.Name, parent, false, g.cfg.UserAgent, true); err != nil {
			return fmt.Errorf("seeding the user agent: %w", err)
		}
	}
	return g.saveLocked()
}

// Run times out silent connections periodically until ctx is done.
func (g *Gateway) Run(ctx context.Context) {
	if g.cfg.ReconcileIntervalSec <= 0 {
		return
	}
	t := time.NewTicker(time.Duration(g.cfg.ReconcileIntervalSec) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := g.Reconcile(); err != nil {
				g.d.Logf("reconcile: %v", err)
			}
		}
	}
}

// Reconcile marks connected agents whose wrapper has gone quiet as
// disconnected, re-arming standing ones.
func (g *Gateway) Reconcile() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.expireLocked() {
		return g.saveLocked()
	}
	return nil
}

func (g *Gateway) expireLocked() bool {
	if g.cfg.HeartbeatTimeoutSec <= 0 {
		return false
	}
	now := g.d.Clock.Now()
	limit := time.Duration(g.cfg.HeartbeatTimeoutSec) * time.Second
	changed := false
	for _, a := range g.byDepth() {
		if a.State != StateConnected || a.LastSeen == nil || now.Sub(*a.LastSeen) < limit {
			continue
		}
		g.disconnectLocked(a, "gateway", "no heartbeat for "+now.Sub(*a.LastSeen).Round(time.Second).String())
		changed = true
	}
	return changed
}

// ensureStandingLocked creates a standing agent if it is missing and
// re-arms it (a fresh registration token, written to its token file) if it
// is not connected. rearmStopped also re-arms one the human stopped, which
// happens only on gateway start.
func (g *Gateway) ensureStandingLocked(id, parent string, root bool, sc StandingAgentConfig, rearmStopped bool) error {
	a := g.st.Agents[id]
	if a != nil && (!a.Standing || a.Root != root) {
		return fmt.Errorf("agent %q exists and is not the standing agent", id)
	}
	if a == nil || (a.State == StateStopped && rearmStopped) {
		if a != nil {
			g.st.History = append(g.st.History, a)
		}
		a = &Agent{
			ID: id, Root: root, Standing: true, User: sc.User, Parent: parent,
			Location: Location{Kind: "systemd", Detail: "silo-agent@" + id + ".service"},
			Runtime:  sc.Runtime, Brief: sc.Brief, Limits: sc.Limits, MayRegister: true,
			State: StateRegistered, Created: g.d.Clock.Now(), RegisteredBy: "gateway",
		}
		g.st.Agents[id] = a
		g.audit("gateway", EvRegister, registerPayload(a))
	}
	switch a.State {
	case StateConnected:
		return nil
	case StateStopped:
		return nil
	}
	return g.armLocked(a)
}

// armLocked gives a standing agent a fresh registration token and writes it
// to its token file, mode 0600 in the root-only state directory.
func (g *Gateway) armLocked(a *Agent) error {
	tok, h := newToken(regTokenPrefix)
	if err := writeFileAtomic(g.cfg.TokenDir(), a.ID, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	a.TokenHash, a.SessionHash, a.State = h, "", StateRegistered
	return nil
}

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

func active(a *Agent) bool { return a != nil && a.State != StateStopped }

// ---- agent.register ---------------------------------------------------------

// RegisterParams is agent.register (design §8.1).
type RegisterParams struct {
	Auth
	// Name is the agent id.
	Name string `json:"name"`
	// Parent may be set only by the human; for agents it is the caller.
	Parent      string   `json:"parent,omitempty"`
	User        string   `json:"user,omitempty"`
	Location    Location `json:"location"`
	Runtime     string   `json:"runtime,omitempty"`
	Brief       string   `json:"brief,omitempty"`
	Limits      Limits   `json:"limits"`
	BudgetUSD   float64  `json:"budget_usd,omitempty"`
	MayRegister bool     `json:"may_register,omitempty"`
	StopCmd     []string `json:"stop_cmd,omitempty"`
}

// RegisterResult carries the single-use registration token. It is shown
// once and never stored; the registrar passes it to `silo connect` in the
// agent's environment as SILO_AGENT_TOKEN.
type RegisterResult struct {
	Agent  string `json:"agent"`
	Token  string `json:"token"`
	HubURL string `json:"hub_url"`
}

// Register records a new agent for caller.
func (g *Gateway) Register(c Caller, p RegisterParams) (*RegisterResult, error) {
	parent := p.Parent
	if !c.Human() {
		ca := g.st.Agents[c.Agent]
		if !ca.MayRegister {
			return nil, g.deny(c, "agent.register", fmt.Sprintf("agent %q may not register agents", c.Agent))
		}
		if p.Parent != "" && p.Parent != c.Agent {
			return nil, g.deny(c, "agent.register", "agents register only their own children")
		}
		parent = c.Agent
	}
	if !agentIDRe.MatchString(p.Name) {
		return nil, invalidParams("name %q: want 1-25 lower-case letters, digits and inner dashes", p.Name)
	}
	if p.Name == RootAgentID {
		return nil, invalidParams("name %q is reserved for the root agent", p.Name)
	}
	if p.User != "" && (!unixUserRe.MatchString(p.User) || p.User == "root" || p.User == g.cfg.RootAgent.User) {
		return nil, invalidParams("user %q: want a Unix user name other than root and %s", p.User, g.cfg.RootAgent.User)
	}
	loc := p.Location
	if loc.Kind == "" {
		loc.Kind = "other"
	}
	if !locationKinds[loc.Kind] {
		return nil, invalidParams("location.kind %q: want systemd, container, microvm, subprocess, remote or other", loc.Kind)
	}
	if len(loc.Detail) > 256 {
		return nil, invalidParams("location.detail: at most 256 bytes")
	}
	if p.Runtime != "" && !runtimeRe.MatchString(p.Runtime) {
		return nil, invalidParams("runtime %q: want a CLI name such as claude", p.Runtime)
	}
	if len(p.Brief) > maxBrief {
		return nil, invalidParams("brief: at most %d bytes", maxBrief)
	}
	if err := p.Limits.Validate(); err != nil {
		return nil, invalidParams("limits: %v", err)
	}
	if p.BudgetUSD < 0 {
		return nil, invalidParams("budget_usd: must not be negative")
	}
	for _, s := range p.StopCmd {
		if s == "" || len(s) > 1024 {
			return nil, invalidParams("stop_cmd: arguments must be 1-1024 bytes")
		}
	}
	if len(p.StopCmd) > 32 {
		return nil, invalidParams("stop_cmd: at most 32 arguments")
	}
	if parent != "" {
		pa := g.st.Agents[parent]
		if !active(pa) {
			return nil, invalidParams("parent %q is not an active agent", parent)
		}
		if len(g.chain(pa)) >= maxDepth {
			return nil, invalidParams("parent %q is already %d levels below root", parent, maxDepth)
		}
	}
	prev := g.st.Agents[p.Name]
	if active(prev) {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("agent %q is already registered (%s)", p.Name, prev.State)}
	}
	if p.User != "" {
		for _, a := range g.st.Agents {
			if active(a) && a.User == p.User && a.Standing {
				return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("user %s belongs to standing agent %q", p.User, a.ID)}
			}
		}
	}
	tok, h := newToken(regTokenPrefix)
	a := &Agent{
		ID: p.Name, User: p.User, Parent: parent, Location: loc, Runtime: p.Runtime, Brief: p.Brief,
		Limits: p.Limits.withDefaults(g.cfg.DefaultLimits), BudgetUSD: p.BudgetUSD, MayRegister: p.MayRegister,
		StopCmd: p.StopCmd, State: StateRegistered, Created: g.d.Clock.Now(), RegisteredBy: c.Actor(),
		TokenHash: h,
	}
	if prev != nil {
		g.st.History = append(g.st.History, prev)
	}
	g.st.Agents[a.ID] = a
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(c.Actor(), EvRegister, registerPayload(a))
	return &RegisterResult{Agent: a.ID, Token: tok, HubURL: g.cfg.HubURL}, nil
}

func registerPayload(a *Agent) map[string]any {
	return map[string]any{
		"agent": a.ID, "root": a.Root, "standing": a.Standing, "parent": a.Parent, "user": a.User,
		"location": a.Location, "runtime": a.Runtime, "brief": a.Brief, "limits": a.Limits,
		"budget_usd": a.BudgetUSD, "may_register": a.MayRegister, "stop_cmd": a.StopCmd,
	}
}

// ---- agent.connect, heartbeat, disconnect ------------------------------------

// ConnectParams is agent.connect: `silo connect` presenting its
// registration token.
type ConnectParams struct {
	Auth
	Token string `json:"token"`
	Host  string `json:"host,omitempty"`
	PID   int    `json:"pid,omitempty"`
}

// ConnectResult is the agent's session: the token every later call from
// the wrapper (and from silo-mcp beside it) carries as agent_token.
type ConnectResult struct {
	Agent        string `json:"agent"`
	SessionToken string `json:"session_token"`
	Parent       string `json:"parent,omitempty"`
	Root         bool   `json:"root,omitempty"`
	Runtime      string `json:"runtime,omitempty"`
	Brief        string `json:"brief,omitempty"`
	// HeartbeatSec is how often the wrapper must call agent.heartbeat.
	HeartbeatSec int `json:"heartbeat_s"`
}

// Connect activates the agent whose registration token this is. The token
// is used up: a second connect with it fails.
func (g *Gateway) Connect(c Caller, cred PeerCred, p ConnectParams) (*ConnectResult, error) {
	if p.Token == "" {
		return nil, invalidParams("token is required")
	}
	h := hashToken(p.Token)
	var a *Agent
	for _, cand := range g.st.Agents {
		if hashEqual(cand.TokenHash, h) {
			a = cand
		}
	}
	if a == nil || a.State == StateStopped {
		g.audit(c.Actor(), EvAuthDeny, map[string]any{"method": "agent.connect", "uid": cred.UID, "pid": cred.PID, "reason": "unknown or used registration token"})
		return nil, &Error{Code: CodeForbidden, Message: "permission denied: unknown or used registration token"}
	}
	sess, sh := newToken(sessionTokenPrefix)
	now := g.d.Clock.Now()
	a.TokenHash, a.SessionHash, a.State = "", sh, StateConnected
	a.ConnectedAt, a.LastSeen = &now, &now
	a.Peer = &Peer{Host: p.Host, PID: p.PID, UID: cred.UID}
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	if a.Standing {
		_ = os.Remove(filepath.Join(g.cfg.TokenDir(), a.ID))
	}
	g.audit("agent:"+a.ID, EvConnect, map[string]any{"agent": a.ID, "host": p.Host, "pid": p.PID, "uid": cred.UID})
	hb := g.cfg.HeartbeatTimeoutSec / 3
	if hb <= 0 {
		hb = 30
	}
	return &ConnectResult{
		Agent: a.ID, SessionToken: sess, Parent: a.Parent, Root: a.Root, Runtime: a.Runtime,
		Brief: a.Brief, HeartbeatSec: hb,
	}, nil
}

// HeartbeatResult tells the wrapper whether to keep going.
type HeartbeatResult struct {
	State string `json:"state"`
	// Stop is "graceful" (SIGTERM the CLI's process group, SIGKILL after
	// 30s) or "kill" (SIGKILL at once) once the agent has been stopped.
	Stop string `json:"stop,omitempty"`
	// Inbox is how many messages are waiting in agent.inbox.
	Inbox int `json:"inbox"`
	// Secrets is how many of the agent's secret requests are pending.
	Secrets int `json:"secrets"`
}

// Heartbeat records that the caller's wrapper is alive.
func (g *Gateway) Heartbeat(c Caller) (*HeartbeatResult, error) {
	a := g.st.Agents[c.Agent]
	if a.State == StateStopped {
		return &HeartbeatResult{State: a.State, Stop: a.StopMode}, nil
	}
	now := g.d.Clock.Now()
	a.LastSeen = &now
	// A heartbeat is not audited and not saved at once: losing one on a
	// crash costs nothing.
	n := 0
	for _, s := range g.st.Secrets {
		if s.Agent == a.ID && s.State == SecretPending {
			n++
		}
	}
	return &HeartbeatResult{State: a.State, Inbox: len(a.Inbox), Secrets: n}, nil
}

// Disconnect is the wrapper saying it is going away: its CLI exited. The
// session token is revoked; a standing agent is re-armed so its unit can
// reconnect when systemd restarts it.
func (g *Gateway) Disconnect(c Caller, reason string) (*AgentInfo, error) {
	a := g.st.Agents[c.Agent]
	if a.State == StateConnected {
		g.disconnectLocked(a, c.Actor(), reason)
	} else {
		a.SessionHash = ""
	}
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	return g.infoLocked(a), nil
}

func (g *Gateway) disconnectLocked(a *Agent, actor, reason string) {
	a.State, a.SessionHash = StateDisconnected, ""
	if reason == "" {
		reason = "disconnected"
	}
	g.audit(actor, EvDisconnect, map[string]any{"agent": a.ID, "reason": reason})
	if a.Standing {
		if err := g.armLocked(a); err != nil {
			g.d.Logf("re-arming standing agent %s: %v", a.ID, err)
		}
	}
}

// ---- agent.stop ----------------------------------------------------------------

// StopParams is agent.stop.
type StopParams struct {
	Auth
	Agent string `json:"agent"`
	// Mode is "graceful" (SIGTERM, then SIGKILL after 30s; the default) or
	// "kill" (SIGKILL at once).
	Mode string `json:"mode,omitempty"`
}

// StopResult lists every agent that was stopped, the target first. StopCmds
// are the registrars' stop commands for the stopped agents that have one
// (a `docker rm -f`, say), for the caller's `silo stop` to run as the
// caller once the wrappers have had their chance. The gateway never runs
// them itself: it would be running a registrar's command as root.
type StopResult struct {
	Stopped   []string            `json:"stopped"`
	Tombstone *Tombstone          `json:"tombstone"`
	StopCmds  map[string][]string `json:"stop_cmds,omitempty"`
}

// Stop stops an agent and every descendant, leaving tombstones.
func (g *Gateway) Stop(c Caller, p StopParams) (*StopResult, error) {
	if !c.Human() && !g.isDescendant(p.Agent, c.Agent) {
		return nil, g.deny(c, "agent.stop", fmt.Sprintf("agent %q is not a descendant of %q", p.Agent, c.Agent))
	}
	switch p.Mode {
	case "":
		p.Mode = "graceful"
	case "graceful", "kill":
	default:
		return nil, invalidParams("mode %q: want graceful or kill", p.Mode)
	}
	a := g.st.Agents[p.Agent]
	if a == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no agent %q", p.Agent)}
	}
	if a.State == StateStopped {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("agent %q is already stopped", p.Agent)}
	}
	targets := []*Agent{a}
	for _, d := range g.byDepth() {
		if active(d) && g.isDescendant(d.ID, a.ID) {
			targets = append(targets, d)
		}
	}
	res := &StopResult{}
	now := g.d.Clock.Now()
	for _, t := range targets {
		cause := "stopped by " + c.Actor()
		if t != a {
			cause = fmt.Sprintf("parent %s stopped", t.Parent)
		}
		// The session token stays valid for agent.heartbeat only, which
		// is how the wrapper learns to signal its CLI; every other method
		// refuses a stopped agent.
		t.State, t.StopMode, t.TokenHash = StateStopped, p.Mode, ""
		t.Inbox = nil
		t.Tombstone = &Tombstone{Time: now, Cause: cause}
		if ch := g.childrenOf(t.ID); len(ch) > 0 {
			t.Tombstone.Children = ch
		}
		if len(t.StopCmd) > 0 {
			if res.StopCmds == nil {
				res.StopCmds = map[string][]string{}
			}
			res.StopCmds[t.ID] = t.StopCmd
		}
		for _, pg := range g.st.Pages {
			if pg.Owner == t.ID {
				pg.Active = false
			}
		}
		for _, s := range g.st.Secrets {
			if s.Agent == t.ID && s.State == SecretPending {
				s.State, s.Resolved = SecretCancelled, &now
			}
		}
		if t.Standing {
			_ = os.Remove(filepath.Join(g.cfg.TokenDir(), t.ID))
		}
		res.Stopped = append(res.Stopped, t.ID)
	}
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	res.Tombstone = a.Tombstone
	g.audit(c.Actor(), EvStop, map[string]any{"agent": a.ID, "mode": p.Mode, "stopped": res.Stopped, "tombstone": a.Tombstone})
	return res, nil
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

// ---- agent.message and agent.inbox ---------------------------------------------

// MessageParams is agent.message (design §8.7).
type MessageParams struct {
	Auth
	Agent string `json:"agent"`
	Text  string `json:"text"`
}

// MessageResult says where the message went.
type MessageResult struct {
	Agent string `json:"agent"`
	Seq   int64  `json:"seq"`
	// Queued is how many messages now wait for the child, this one
	// included.
	Queued int `json:"queued"`
}

// Message queues text for agent p.Agent. Messages go strictly downward: an
// agent may message only the agents it registered itself (whose parent it
// is); the human may message any agent. A child can never reach its
// parent, root or a sibling this way, so a prompt-injected child cannot
// inject instructions upward.
func (g *Gateway) Message(c Caller, p MessageParams) (*MessageResult, error) {
	t := g.st.Agents[p.Agent]
	if !c.Human() && (t == nil || t.Parent != c.Agent) {
		return nil, g.deny(c, "agent.message", fmt.Sprintf("agent %q may message only agents it registered, and %q is not one", c.Agent, p.Agent))
	}
	if t == nil {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no agent %q", p.Agent)}
	}
	if t.State == StateStopped {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("agent %q is stopped", p.Agent)}
	}
	if p.Text == "" || len(p.Text) > maxMessage {
		return nil, invalidParams("text: must be 1-%d bytes", maxMessage)
	}
	if len(t.Inbox) >= maxInbox {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("agent %q has %d undelivered messages", p.Agent, len(t.Inbox))}
	}
	t.NextMsg++
	m := Message{Seq: t.NextMsg, FromAgent: c.Agent, From: c.Actor(), Text: p.Text, Time: g.d.Clock.Now()}
	t.Inbox = append(t.Inbox, m)
	if err := g.saveLocked(); err != nil {
		return nil, err
	}
	g.audit(c.Actor(), EvMessage, map[string]any{"agent": t.ID, "from_agent": c.Agent, "seq": m.Seq, "text": m.Text})
	return &MessageResult{Agent: t.ID, Seq: m.Seq, Queued: len(t.Inbox)}, nil
}

// Inbox hands the caller's wrapper its waiting messages, oldest first, and
// forgets them. The wrapper injects each into the ACP session as a prompt
// tagged from_agent, queued behind a turn in progress.
func (g *Gateway) Inbox(c Caller) ([]Message, error) {
	a := g.st.Agents[c.Agent]
	out := a.Inbox
	if out == nil {
		out = []Message{}
	}
	if len(a.Inbox) > 0 {
		a.Inbox = nil
		if err := g.saveLocked(); err != nil {
			return nil, err
		}
		seqs := make([]int64, len(out))
		for i, m := range out {
			seqs[i] = m.Seq
		}
		g.audit(c.Actor(), EvDeliver, map[string]any{"agent": a.ID, "seqs": seqs})
	}
	return out, nil
}

// ---- agent.list and agent.status ------------------------------------------------

// ListParams filters agent.list.
type ListParams struct {
	Auth
	Parent *string  `json:"parent,omitempty"`
	State  []string `json:"state,omitempty"`
	// History includes earlier incarnations of reused ids.
	History bool `json:"history,omitempty"`
}

// AgentInfo is an agent as list and status report it. Tokens and the inbox
// are never reported.
type AgentInfo struct {
	Agent
	Children []string `json:"children"`
	Inbox    int      `json:"inbox"`
}

func (g *Gateway) infoLocked(a *Agent) *AgentInfo {
	info := &AgentInfo{Agent: *a, Children: g.childrenOf(a.ID), Inbox: len(a.Inbox)}
	info.TokenHash, info.SessionHash, info.Agent.Inbox = "", "", nil
	return info
}

// List returns the agents the caller may see, sorted by id.
func (g *Gateway) List(c Caller, p ListParams) ([]AgentInfo, error) {
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
		info := g.infoLocked(a)
		info.Brief = ""
		out = append(out, *info)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// StatusParams names one agent.
type StatusParams struct {
	Auth
	Agent string `json:"agent"`
}

// Status returns one agent.
func (g *Gateway) Status(c Caller, p StatusParams) (*AgentInfo, error) {
	a := g.st.Agents[p.Agent]
	if a == nil || !g.canSee(c, p.Agent) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no agent %q", p.Agent)}
	}
	return g.infoLocked(a), nil
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
