package fenceapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/ids"
	"github.com/bwsalmon/silo/pkg/ledger"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Agents (fence-api.md, "Agents", SV-23 and SV-24; agents.md, SV-98 to
// SV-104). An agent is a row in its silo, a tool VM made through the
// facade (ToolVMs), and a harness session (agents.SessionRunner). It
// holds model budget out of its silo's model/usd, as the ledger
// charge on agent:<id>, and, while its state holds one, a slot of its
// silo's agent_sessions, as the charge on agent-session:<id>.

// The audit kinds of agents (fence-api.md, "Audit").
const (
	KindAgentCreate = "agent.create"
	KindAgentDelete = "agent.delete"
	KindAgentExpire = "agent.expire"
	KindAgentStop   = "agent.stop"
	KindAgentResume = "agent.resume"
	KindAgentRenew  = "agent.renew"
)

// The kinds of an agent's operations in silo_operations.
const (
	opKindAgentCreate = "agent.create"
	opKindAgentDelete = "agent.delete"
)

// AgentSessions is the global count class an agent's session holds.
var AgentSessions = ledger.Key{Kind: "gce", Class: "agent_sessions", Region: ledger.Global}

// The defaults of a tool VM the request leaves out.
const (
	DefaultToolMachineType = "e2-standard-2"
	DefaultToolImage       = "projects/debian-cloud/global/images/family/debian-12"
	DefaultToolDiskGB      = 20
)

// ToolVMSpec is the shape of an agent's tool VM, as the request gives
// it.
type ToolVMSpec struct {
	Zone        string `json:"zone"`
	Subnet      string `json:"subnet"`
	MachineType string `json:"machine_type"`
	Image       string `json:"image"`
	DiskGB      int64  `json:"disk_gb"`
	Spot        bool   `json:"spot"`
	// Metadata is more instance metadata; startup-script is silod's.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ToolVMs makes and deletes agents' tool VMs through the facade
// (agents.md, "Creation", SV-104). Every call is made as caller, on the
// facade of silo: the agent's silo itself when it creates its own
// agent, and otherwise the ancestor or manage holder that does, so
// that every check, charge and rewrite applies and a sealed silo's
// tool VM is made under its ancestor's authority. Package
// pkg/facade/gcp/gce implements it; an error is an *Error.
type ToolVMs interface {
	// Create makes instance (agents.InstanceName) with the hidden
	// label silo-agent=<agent> and startupScript, and returns its IP.
	Create(ctx context.Context, caller Caller, silo *tree.Silo, agent, instance string, spec ToolVMSpec, startupScript string) (ip string, err error)
	// Delete deletes it; one that is gone already is not an error.
	Delete(ctx context.Context, caller Caller, silo *tree.Silo, agent, zone, instance string) error
}

// ToolVMIdentifier is a ToolVMs that can say which GCE instance a tool
// VM is now: its instance id and its IP. silod records the id it made
// and checks it again on every attach of a tool channel (agents.md,
// "The tool channel", SV-106). A ToolVMs without it can't have its
// tool servers attach.
type ToolVMIdentifier interface {
	Identify(ctx context.Context, caller Caller, silo *tree.Silo, agent, zone, instance string) (id, ip string, err error)
}

// Agent is the agent object (agents.md, "The agent object", SV-101).
type Agent struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Silo          string            `json:"silo"`
	SiloPath      string            `json:"silo_path"`
	Harness       AgentHarness      `json:"harness"`
	Model         string            `json:"model"`
	Budget        AgentBudget       `json:"budget"`
	ToolVM        AgentToolVM       `json:"tool_vm"`
	Lease         AgentLease        `json:"lease"`
	Prompt        string            `json:"prompt"`
	Owner         *string           `json:"owner"`
	State         string            `json:"state"`
	StatusMessage string            `json:"status_message,omitempty"`
	Session       *AgentSession     `json:"session"`
	CreateTime    time.Time         `json:"create_time"`
	Labels        map[string]string `json:"labels"`
}

// AgentHarness is an agent's harness.
type AgentHarness struct {
	Adapter    string `json:"adapter"`
	Version    string `json:"version"`
	Credential string `json:"credential"`
}

// AgentBudget is an agent's model budget, in dollars.
type AgentBudget struct {
	USD         string    `json:"usd"`
	SpentUSD    string    `json:"spent_usd"`
	PeriodStart time.Time `json:"period_start"`
}

// AgentToolVM is an agent's tool VM: its shape, and what silod filled
// in.
type AgentToolVM struct {
	ToolVMSpec
	Instance string `json:"instance"`
	// InstanceID is the GCE instance id of the VM silod made, which a
	// tool channel's attach must come from.
	InstanceID string `json:"instance_id,omitempty"`
	IP         string `json:"ip"`
	ToolServer string `json:"tool_server"`
}

// AgentLease is when an agent is deleted unless renewed.
type AgentLease struct {
	ExpireTime time.Time `json:"expire_time"`
}

// AgentSession is an agent's harness session.
type AgentSession struct {
	ID       string    `json:"id"`
	Started  time.Time `json:"started"`
	Turns    int       `json:"turns"`
	Restarts int       `json:"restarts"`
}

// AgentRequest is a create's body.
type AgentRequest struct {
	Name    string `json:"name"`
	Harness struct {
		Adapter    string `json:"adapter"`
		Credential string `json:"credential"`
	} `json:"harness"`
	Model  string `json:"model"`
	Budget struct {
		USD string `json:"usd"`
	} `json:"budget"`
	ToolVM ToolVMSpec        `json:"tool_vm"`
	Lease  leaseRequest      `json:"lease"`
	Prompt string            `json:"prompt"`
	Labels map[string]string `json:"labels"`
}

// leaseRequest is a lease asked for: an expire_time, or a ttl from now.
type leaseRequest struct {
	ExpireTime string `json:"expire_time"`
	TTL        string `json:"ttl"`
}

// agentRow is an agent as stored.
type agentRow struct {
	Agent
	budget, spent int64
	expire        time.Time
	owner         string
	op            string
}

const agentColumns = `id, silo, name, adapter, version, credential, model, budget_micros, spent_micros, period_start,
	tool_vm, expire_at, prompt, owner, state, status_message, session, labels, op, created_at`

func scanAgent(row interface{ Scan(...any) error }) (*agentRow, error) {
	a := &agentRow{}
	var period, expire, created int64
	var vm, session, labels string
	err := row.Scan(&a.ID, &a.Silo, &a.Name, &a.Harness.Adapter, &a.Harness.Version, &a.Harness.Credential, &a.Model,
		&a.budget, &a.spent, &period, &vm, &expire, &a.Prompt, &a.owner, &a.State, &a.StatusMessage, &session, &labels, &a.op, &created)
	if err != nil {
		return nil, err
	}
	a.Budget = AgentBudget{USD: usd(a.budget), SpentUSD: usd(a.spent), PeriodStart: store.Time(period)}
	a.expire = store.Time(expire)
	a.Lease.ExpireTime = a.expire
	a.CreateTime = store.Time(created)
	if err := json.Unmarshal([]byte(vm), &a.ToolVM); err != nil {
		return nil, err
	}
	if session != "" {
		a.Session = &AgentSession{}
		if err := json.Unmarshal([]byte(session), a.Session); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal([]byte(labels), &a.Labels); err != nil {
		return nil, err
	}
	if a.Labels == nil {
		a.Labels = map[string]string{}
	}
	if a.owner != "" {
		o := a.owner
		a.Owner = &o
	}
	return a, nil
}

func readAgent(ctx context.Context, tx store.Tx, where string, args ...any) (*agentRow, error) {
	a, err := scanAgent(tx.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func readAgents(ctx context.Context, tx store.Tx, where string, args ...any) ([]*agentRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+agentColumns+` FROM agents WHERE `+where+` ORDER BY silo, name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*agentRow
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// save writes an agent's changeable columns back.
func (a *agentRow) save(ctx context.Context, tx store.Tx) error {
	now, err := tx.Now(ctx)
	if err != nil {
		return err
	}
	vm, _ := json.Marshal(a.ToolVM)
	labels, _ := json.Marshal(a.Labels)
	session := ""
	if a.Session != nil {
		b, _ := json.Marshal(a.Session)
		session = string(b)
	}
	_, err = tx.Exec(ctx, `UPDATE agents SET budget_micros = ?, spent_micros = ?, tool_vm = ?, expire_at = ?, state = ?,
		status_message = ?, session = ?, labels = ?, op = ?, updated_at = ? WHERE id = ?`,
		a.budget, a.spent, string(vm), store.Millis(a.expire), a.State, a.StatusMessage, session, string(labels), a.op,
		store.Millis(now), a.ID)
	return err
}

// shown is the agent object, with its silo's path.
func (a *agentRow) shown(ctx context.Context, tx store.Tx) (*Agent, error) {
	out := a.Agent
	out.Budget = AgentBudget{USD: usd(a.budget), SpentUSD: usd(a.spent), PeriodStart: a.Budget.PeriodStart}
	out.Lease.ExpireTime = a.expire
	if t, err := tree.Get(ctx, tx, a.Silo); err == nil {
		out.SiloPath = t.Path
	} else if !errors.Is(err, tree.ErrNotFound) {
		return nil, err
	}
	return &out, nil
}

// AgentPath is how an agent is named by path, /team-a/t-1945/agents/fixer,
// for a message that names one; "-<id>" if there is no such agent.
func AgentPath(ctx context.Context, tx store.Tx, id string) (string, error) {
	a, err := readAgent(ctx, tx, `id = ?`, id)
	if err != nil || a == nil {
		return "-" + id, err
	}
	t, err := tree.Get(ctx, tx, a.Silo)
	if err != nil {
		return "-" + id, nil
	}
	return strings.TrimSuffix(t.Path, "/") + "/agents/" + a.Name, nil
}

// newAgentID draws an id from the silo id generator that is neither an
// agent's nor a silo's (agents.md, "The agent object").
func newAgentID(ctx context.Context, tx store.Tx) (string, error) {
	for {
		id := ids.New()
		var n int
		if err := tx.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM agents WHERE id = ?) + (SELECT COUNT(*) FROM silos WHERE id = ?)`, id, id).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return id, nil
		}
	}
}

// chargeVia is how the caller reached the silo, as the ledger records a
// charge.
func chargeVia(chain []*tree.Silo, c Caller) string {
	switch v := viaOf(chain, c); {
	case strings.HasPrefix(v, "manage:"):
		return "manage:" + c.Manage
	case strings.HasPrefix(v, "ancestor:"):
		return v
	}
	return ledger.ViaFacade
}

func sessionObject(agent string) string { return "agent-session:" + agent }

// holdSession charges an agent's session slot to its silo: QUOTA_EXCEEDED
// with class agent_sessions if the silo has none free, and
// RESOURCE_EXHAUSTED if the control VMs have none.
func (s *Server) holdSession(ctx context.Context, tx store.Tx, chain []*tree.Silo, c Caller, agent, credential string) error {
	t := chain[len(chain)-1]
	if err := s.credentialSessions(ctx, tx, credential, agent); err != nil {
		return err
	}
	if err := ledger.Charge(ctx, tx, ledger.ChargeRequest{Object: sessionObject(agent), Silo: t.ID, Via: chargeVia(chain, c),
		Amounts: []ledger.Amount{{Key: AgentSessions, Amount: ledger.Units(1)}}}); err != nil {
		return err
	}
	if s.opts.Runner != nil {
		ok, err := s.opts.Runner.HasSlot(ctx)
		if err != nil {
			return errorf(Unavailable, "asking the control VMs for a session slot: %v", err)
		}
		if !ok {
			return errorf(ResourceExhausted, "the control VMs have no session slot free").with("class", "agent_sessions")
		}
	}
	return nil
}

// releaseSession gives an agent's session slot back, if it holds one.
func releaseSession(ctx context.Context, tx store.Tx, agent string) error {
	held, err := ledger.Charges(ctx, tx, sessionObject(agent))
	if err != nil || len(held) == 0 {
		return err
	}
	_, err = ledger.Refund(ctx, tx, sessionObject(agent), nil)
	return err
}

// budgetError is a model budget refusal with the class the spec names:
// model_usd.
func budgetError(err error) error {
	var le *ledger.Error
	if errors.As(err, &le) && le.Key == ledger.ModelUSD && le.Code == ledger.QuotaExceeded {
		return (&Error{Code: QuotaExceeded, Message: "model budget: " + le.Message}).
			with("class", "model_usd").with("asked", usd(le.Asked)).with("free", usd(le.Free))
	}
	return err
}

// agentPolicy checks an agent's harness and model against its silo's
// effective agent policy (fence-api.md, "Agent policy", SV-23), and
// returns the policy's max_lease. An adapter, credential or model this
// deployment doesn't have, or a model with no price, is
// INVALID_ARGUMENT; one the policy doesn't allow is PERMISSION_DENIED.
func (s *Server) agentPolicy(ctx context.Context, tx store.Tx, chain []*tree.Silo, adapter, credential, model string) (time.Duration, error) {
	eff, err := effectiveOf(ctx, tx, chain, nil)
	if err != nil {
		return 0, err
	}
	pol := eff.Agent
	t := chain[len(chain)-1]
	deny := func(field, value string) error {
		return errorf(PermissionDenied, "%s's agent policy doesn't allow %s %s", t.Path, strings.TrimSuffix(field, "s"), value).
			with("policy", "agent_policy").with("field", field).with("value", value)
	}
	ad, ok := agents.Adapters[adapter]
	if !ok {
		return 0, errorf(InvalidArgument, "harness.adapter %q: want one of %s", adapter, strings.Join(agents.AdapterNames(), ", ")).with("field", "harness.adapter")
	}
	if pol.Adapters != nil && !contains(pol.Adapters, adapter) {
		return 0, deny("adapters", adapter)
	}
	cred, found, err := s.credential(ctx, tx, credential)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, errorf(InvalidArgument, "harness.credential %q is not one of this deployment's credentials", credential).with("field", "harness.credential")
	}
	if pol.Credentials != nil && !contains(pol.Credentials, credential) {
		return 0, deny("credentials", credential)
	}
	if !ad.Takes(cred.Provider) {
		return 0, errorf(InvalidArgument, "adapter %s doesn't take credential %s, which is %s's", adapter, credential, cred.Provider).with("field", "harness.credential")
	}
	if _, ok, err := modelPrice(ctx, tx, cred.Provider, model); err != nil {
		return 0, err
	} else if !ok {
		return 0, errorf(InvalidArgument, "model %q has no %s price, so can't be used", model, cred.Provider).with("field", "model")
	}
	if pol.Models != nil {
		ok := false
		for _, p := range pol.Models {
			ok = ok || modelCovers(p, model)
		}
		if !ok {
			return 0, deny("models", model)
		}
	}
	max, err := parseDuration(pol.MaxLease)
	if err != nil {
		max = RootMaxLease
	}
	return max, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// leaseEnd is when a lease asked for ends: an expire_time, a ttl from
// now, or, with neither, max from now. Past max from now is
// PERMISSION_DENIED (the policy's max_lease); not in the future is
// INVALID_ARGUMENT.
func leaseEnd(l leaseRequest, now time.Time, max time.Duration, path string) (time.Time, error) {
	var end time.Time
	switch {
	case l.ExpireTime != "" && l.TTL != "":
		return end, errorf(InvalidArgument, "give expire_time or ttl, not both").with("field", "lease")
	case l.ExpireTime != "":
		t, err := time.Parse(time.RFC3339, l.ExpireTime)
		if err != nil {
			return end, errorf(InvalidArgument, "expire_time %q: want an RFC 3339 time", l.ExpireTime).with("field", "lease.expire_time")
		}
		end = t.UTC()
	case l.TTL != "":
		d, err := parseDuration(l.TTL)
		if err != nil || d <= 0 {
			return end, errorf(InvalidArgument, "ttl %q: want a positive duration such as 4h", l.TTL).with("field", "lease.ttl")
		}
		end = now.Add(d)
	default:
		end = now.Add(max)
	}
	if !end.After(now) {
		return end, errorf(InvalidArgument, "the lease must end in the future").with("field", "lease")
	}
	if end.Sub(now) > max+time.Second {
		return end, errorf(PermissionDenied, "a lease to %s is longer than %s's agent_policy max_lease of %s from now", end.Format(time.RFC3339), path, formatDuration(max)).
			with("policy", "agent_policy").with("field", "max_lease").with("max_lease", formatDuration(max))
	}
	return end, nil
}

// resolveAgent resolves silo ref, as resolve does for op, and the agent
// named name (or -<id>) in it: NOT_FOUND if there is none.
func (s *Server) resolveAgent(ctx context.Context, tx store.Tx, q *request, ref, name string, op Op) ([]*tree.Silo, relation, *agentRow, error) {
	chain, rel, err := s.resolve(ctx, tx, &q.caller, ref, op)
	if err != nil {
		return nil, relNone, nil, err
	}
	t := chain[len(chain)-1]
	var a *agentRow
	if id, ok := strings.CutPrefix(name, "-"); ok {
		a, err = readAgent(ctx, tx, `id = ? AND silo = ?`, id, t.ID)
	} else {
		a, err = readAgent(ctx, tx, `silo = ? AND name = ?`, t.ID, name)
	}
	if err != nil {
		return nil, relNone, nil, err
	}
	if a == nil {
		return nil, relNone, nil, errorf(NotFound, "no agent %s in %s", name, t.Path).with("agent", name)
	}
	return chain, rel, a, nil
}

// ownSession reports whether the call is the agent's own session
// (call 38). The session socket fills in Caller.Agent.
func ownSession(c Caller, a *agentRow) bool {
	return c.Via == ViaHarness && c.Agent != "" && c.Agent == a.ID
}

// startOp records a long-running operation on an agent.
func startOp(ctx context.Context, tx store.Tx, kind string, t *tree.Silo, a *agentRow, c Caller, state string) (*opRow, error) {
	now, err := tx.Now(ctx)
	if err != nil {
		return nil, err
	}
	cj, _ := json.Marshal(c)
	id := ids.New() + ids.New()
	if _, err := tx.Exec(ctx, `INSERT INTO silo_operations (id, kind, silo, path, caller, state, done, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		id, kind, t.ID, t.Path+"/agents/"+a.Name, string(cj), state, false, store.Millis(now), store.Millis(now)); err != nil {
		return nil, err
	}
	a.op = id
	return readOp(ctx, tx, `id = ?`, id)
}

func agentFields(a *agentRow, more map[string]any) map[string]any {
	f := map[string]any{"agent": a.ID, "name": a.Name}
	for k, v := range more {
		f[k] = v
	}
	return f
}

// createAgent is POST .../agents (fence-api.md, "Operations" under
// Agents, SV-24). Every check of the fence API's runs in one
// transaction, which reserves the budget and the session slot and
// records the agent PROVISIONING; then the tool VM is made through the
// facade, whose own checks (its quota, the subnet, the image) refuse
// the create synchronously and undo the first transaction. It returns
// the create's operation, done once the tool server connects.
func (s *Server) createAgent(q *request, ref string) (any, error) {
	ctx := q.r.Context()
	var req AgentRequest
	if err := decode(q, &req); err != nil {
		return nil, err
	}
	if err := agents.ValidName(req.Name); err != nil {
		return nil, errorf(InvalidArgument, "%v", err).with("field", "name")
	}
	vm := req.ToolVM
	if vm.Zone == "" {
		return nil, errorf(InvalidArgument, "tool_vm.zone is required").with("field", "tool_vm.zone")
	}
	if vm.Subnet == "" {
		return nil, errorf(InvalidArgument, "tool_vm.subnet is required: one of the silo's subnetworks").with("field", "tool_vm.subnet")
	}
	for k := range vm.Metadata {
		if strings.EqualFold(k, agents.StartupScriptKey) {
			return nil, errorf(InvalidArgument, "tool_vm.metadata %s is silod's: it starts silo-tools", k).with("field", "tool_vm.metadata")
		}
	}
	if vm.MachineType == "" {
		vm.MachineType = DefaultToolMachineType
	}
	if vm.Image == "" {
		vm.Image = DefaultToolImage
	}
	if vm.DiskGB == 0 {
		vm.DiskGB = DefaultToolDiskGB
	}
	if vm.DiskGB < 0 {
		return nil, errorf(InvalidArgument, "tool_vm.disk_gb %d: want a positive size", vm.DiskGB).with("field", "tool_vm.disk_gb")
	}
	if req.Budget.USD == "" {
		return nil, errorf(InvalidArgument, "budget.usd is required").with("field", "budget.usd")
	}
	micros, err := ledger.ParseAmount(req.Budget.USD)
	if err != nil {
		return nil, errorf(InvalidArgument, "budget.usd %q: %v", req.Budget.USD, err).with("field", "budget.usd")
	}
	if s.opts.ToolVMs == nil {
		return nil, errorf(Unavailable, "silod has no facade to make tool VMs with")
	}
	var a *agentRow
	var t *tree.Silo
	var out any
	err = s.tx(ctx, func(tx store.Tx) error {
		chain, _, err := s.resolve(ctx, tx, &q.caller, ref, opAgentsCreate)
		if err != nil {
			return err
		}
		t = chain[len(chain)-1]
		q.note(t, KindAgentCreate)
		if t.State != tree.StateActive {
			return errorf(FailedPrecondition, "%s is %s", t.Path, t.State).with("silo", t.Path)
		}
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		if dup, err := readAgent(ctx, tx, `silo = ? AND name = ?`, t.ID, req.Name); err != nil {
			return err
		} else if dup != nil {
			return errorf(AlreadyExists, "%s already has an agent %s", t.Path, req.Name).with("agent", req.Name)
		}
		max, err := s.agentPolicy(ctx, tx, chain, req.Harness.Adapter, req.Harness.Credential, req.Model)
		if err != nil {
			return err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		end, err := leaseEnd(req.Lease, now, max, t.Path)
		if err != nil {
			return err
		}
		id, err := newAgentID(ctx, tx)
		if err != nil {
			return err
		}
		if err := ledger.ReserveModelBudget(ctx, tx, t.ID, id, chargeVia(chain, q.caller), micros); err != nil {
			return budgetError(err)
		}
		if err := s.holdSession(ctx, tx, chain, q.caller, id, req.Harness.Credential); err != nil {
			return err
		}
		period, err := ledger.BudgetPeriod(ctx, tx)
		if err != nil {
			return err
		}
		start, err := ledger.PeriodStart(period, now)
		if err != nil {
			return err
		}
		labels := req.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		a = &agentRow{Agent: Agent{ID: id, Name: req.Name, Silo: t.ID, SiloPath: t.Path,
			Harness: AgentHarness{Adapter: req.Harness.Adapter, Version: agents.Adapters[req.Harness.Adapter].Version, Credential: req.Harness.Credential},
			Model:   req.Model, Prompt: req.Prompt, State: agents.StateProvisioning, Labels: labels, CreateTime: now,
			ToolVM: AgentToolVM{ToolVMSpec: vm, Instance: agents.InstanceName(t.ID, req.Name), ToolServer: agents.ToolServerPending},
			Budget: AgentBudget{PeriodStart: start}},
			budget: micros, expire: end, owner: q.caller.Agent}
		vmj, _ := json.Marshal(a.ToolVM)
		lj, _ := json.Marshal(labels)
		if _, err := tx.Exec(ctx, `INSERT INTO agents (`+agentColumns+`, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, '', '', ?, '', ?, ?)`,
			a.ID, a.Silo, a.Name, a.Harness.Adapter, a.Harness.Version, a.Harness.Credential, a.Model, micros,
			store.Millis(start), string(vmj), store.Millis(end), a.Prompt, a.owner, a.State, string(lj),
			store.Millis(now), store.Millis(now)); err != nil {
			return err
		}
		if a.owner != "" {
			if err := recordOwner(ctx, tx, "agent", a.ID, a.owner); err != nil {
				return err
			}
		}
		if q.validateOnly {
			shown, err := a.shown(ctx, tx)
			if err != nil {
				return err
			}
			out = map[string]any{"validate_only": true, "agent": shown}
			return errValidateOnly
		}
		op, err := startOp(ctx, tx, opKindAgentCreate, t, a, q.caller, agents.StateProvisioning)
		if err != nil {
			return err
		}
		out = op.Operation
		return a.save(ctx, tx)
	})
	if errors.Is(err, errValidateOnly) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	ip, verr := s.opts.ToolVMs.Create(ctx, q.caller, t, a.ID, a.ToolVM.Instance, a.ToolVM.ToolVMSpec, agents.StartupScript())
	if verr != nil {
		e := asError(ctx, nil, verr)
		if err := s.tx(ctx, func(tx store.Tx) error {
			if err := s.forgetAgent(ctx, tx, a.ID); err != nil {
				return err
			}
			return setOp(ctx, tx, a.op, agents.StateFailed, true, e)
		}); err != nil {
			return nil, err
		}
		return nil, e
	}
	// The instance id the attach is checked against. One GCE doesn't
	// show yet stays empty, and no attach matches it.
	var instanceID string
	if idf, ok := s.opts.ToolVMs.(ToolVMIdentifier); ok {
		id, at, err := idf.Identify(ctx, q.caller, t, a.ID, a.ToolVM.Zone, a.ToolVM.Instance)
		if err == nil {
			instanceID = id
			if ip == "" {
				ip = at
			}
		}
	}
	err = s.tx(ctx, func(tx store.Tx) error {
		cur, err := readAgent(ctx, tx, `id = ?`, a.ID)
		if err != nil || cur == nil {
			return err
		}
		cur.ToolVM.IP, cur.ToolVM.InstanceID = ip, instanceID
		if err := cur.save(ctx, tx); err != nil {
			return err
		}
		op, err := readOp(ctx, tx, `id = ?`, cur.op)
		if err != nil {
			return err
		}
		if op != nil {
			out = op.Operation
		}
		if err := Audit(ctx, tx, t, q.caller, KindAgentCreate, q.requestID, "OK", agentFields(cur, map[string]any{
			"adapter": cur.Harness.Adapter, "credential": cur.Harness.Credential, "model": cur.Model,
			"budget_usd": usd(cur.budget), "expire_time": cur.expire, "instance": cur.ToolVM.Instance, "owner_agent": cur.owner,
		})); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	if err != nil {
		return nil, err
	}
	if s.opts.Runner != nil {
		if err := s.opts.Runner.Start(ctx, s.session(a, false)); err != nil {
			s.failAgent(ctx, a.ID, "the harness didn't start: "+err.Error())
		}
	}
	return out, nil
}

// The Op each agent call resolves its silo with, for the
// ManageChecker: all of them need a grant's agents group.
const (
	opAgentsList   Op = "agents.list"
	opAgentsGet    Op = "agents.get"
	opAgentsCreate Op = "agents.create"
	opAgentsPatch  Op = "agents.patch"
	opAgentsDelete Op = "agents.delete"
	opAgentsStop   Op = "agents.stop"
	opAgentsResume Op = "agents.resume"
	opAgentsRenew  Op = "agents.renew"
)

func (s *Server) session(a *agentRow, resume bool) agents.Session {
	return agents.Session{Agent: a.ID, Silo: a.Silo, Adapter: a.Harness.Adapter, Credential: a.Harness.Credential,
		Model: a.Model, Prompt: a.Prompt, Resume: resume}
}

// failAgent makes an agent FAILED with why, giving up its session slot.
func (s *Server) failAgent(ctx context.Context, id, why string) {
	_ = s.tx(ctx, func(tx store.Tx) error {
		a, err := readAgent(ctx, tx, `id = ?`, id)
		if err != nil || a == nil || a.State == agents.StateDeleting {
			return err
		}
		a.State, a.StatusMessage = agents.StateFailed, why
		if err := releaseSession(ctx, tx, a.ID); err != nil {
			return err
		}
		if a.op != "" {
			if err := setOp(ctx, tx, a.op, agents.StateFailed, true, errorf(FailedPrecondition, "%s", why)); err != nil {
				return err
			}
			a.op = ""
		}
		return a.save(ctx, tx)
	})
}

// forgetAgent removes an agent's row and returns what it holds of its
// silo: its budget and its session slot.
func (s *Server) forgetAgent(ctx context.Context, tx store.Tx, id string) error {
	if err := ledger.ReturnModelBudget(ctx, tx, id); err != nil {
		return err
	}
	if err := releaseSession(ctx, tx, id); err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM agent_owned WHERE kind = 'agent' AND object = ?`, `DELETE FROM agents WHERE id = ?`} {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// recordOwner records that agent's session created object (call 43).
func recordOwner(ctx context.Context, tx store.Tx, kind, object, agent string) error {
	_, err := tx.Exec(ctx, `INSERT INTO agent_owned (kind, object, agent) VALUES (?, ?, ?)`, kind, object, agent)
	return err
}

// ownerAgent is the agent whose session created object, or "".
func ownerAgent(ctx context.Context, tx store.Tx, kind, object string) (string, error) {
	var a string
	err := tx.QueryRow(ctx, `SELECT agent FROM agent_owned WHERE kind = ? AND object = ?`, kind, object).Scan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return a, err
}

// ToolServerConnected records that an agent's tool server has
// connected: the agent leaves PROVISIONING for RUNNING, on its prompt's
// first turn, and its create's operation is done. The tool channel's
// hub calls it on every attach (agents.md, "The tool channel"; package
// toolchan); a fake SessionRunner may too.
func (s *Server) ToolServerConnected(ctx context.Context, agent string) error {
	return s.tx(ctx, func(tx store.Tx) error {
		a, err := readAgent(ctx, tx, `id = ?`, agent)
		if err != nil {
			return err
		}
		if a == nil {
			return errorf(NotFound, "no agent -%s", agent)
		}
		if a.State == agents.StateDeleting {
			return nil
		}
		a.ToolVM.ToolServer = agents.ToolServerConnected
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		if a.Session == nil {
			a.Session = &AgentSession{ID: "s-" + ids.New(), Started: now}
		}
		if a.State == agents.StateProvisioning {
			// Its first turn, on its prompt.
			if err := s.beginTurn(ctx, tx, a, now); err != nil {
				return err
			}
		}
		// The create is done, whatever happened to the agent since.
		if a.op != "" {
			if err := setOp(ctx, tx, a.op, OpDone, true, nil); err != nil {
				return err
			}
			a.op = ""
		}
		return a.save(ctx, tx)
	})
}

// ToolServerDisconnected records that an agent's tool channel has
// dropped: its tool_server is DISCONNECTED until the tool server
// attaches again.
func (s *Server) ToolServerDisconnected(ctx context.Context, agent string) error {
	return s.tx(ctx, func(tx store.Tx) error {
		a, err := readAgent(ctx, tx, `id = ?`, agent)
		if err != nil || a == nil || a.State == agents.StateDeleting {
			return err
		}
		a.ToolVM.ToolServer = agents.ToolServerDisconnected
		return a.save(ctx, tx)
	})
}

// ToolChannelAgent binds a tool channel's attach to an agent
// (agents.md, "The tool channel", SV-106): silo is the caller's silo by
// origin identity, and ip the source address the gateway saw. It is
// the agent in silo whose tool VM silod recorded at ip, and only if the
// GCE instance at that address now is the one silod made for it: the
// instance silod named is read again, and both its id and its address
// must match what was recorded. Anything else is PERMISSION_DENIED, as
// a 403: an address with no agent, an agent being deleted, an instance
// silod has no id for, or a different instance.
func (s *Server) ToolChannelAgent(ctx context.Context, silo string, ip netip.Addr) (string, error) {
	var a *agentRow
	var t *tree.Silo
	err := s.tx(ctx, func(tx store.Tx) error {
		list, err := readAgents(ctx, tx, `silo = ? AND state <> ?`, silo, agents.StateDeleting)
		if err != nil {
			return err
		}
		for _, x := range list {
			if at, err := netip.ParseAddr(x.ToolVM.IP); err == nil && at.Unmap() == ip.Unmap() {
				a = x
				break
			}
		}
		if a == nil {
			return nil
		}
		t, err = tree.Get(ctx, tx, silo)
		return err
	})
	if err != nil {
		return "", err
	}
	if a == nil {
		return "", errorf(PermissionDenied, "silo: no agent's tool VM is at %s", ip)
	}
	idf, ok := s.opts.ToolVMs.(ToolVMIdentifier)
	if !ok || a.ToolVM.InstanceID == "" {
		return "", errorf(PermissionDenied, "silo: the instance at %s can't be checked against agent %s's tool VM", ip, a.ID)
	}
	c, err := s.internalCaller(ctx)
	if err != nil {
		return "", err
	}
	id, at, err := idf.Identify(ctx, c, t, a.ID, a.ToolVM.Zone, a.ToolVM.Instance)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && (e.Code == NotFound || e.Code == InvalidArgument) {
			return "", errorf(PermissionDenied, "silo: agent %s's tool VM is gone", a.ID)
		}
		return "", err
	}
	if id != a.ToolVM.InstanceID {
		return "", errorf(PermissionDenied, "silo: the instance at %s is not the tool VM silod made for agent %s", ip, a.ID)
	}
	if got, err := netip.ParseAddr(at); err != nil || got.Unmap() != ip.Unmap() {
		return "", errorf(PermissionDenied, "silo: agent %s's tool VM is no longer at %s", a.ID, ip)
	}
	return a.ID, nil
}

// listAgents is GET .../agents.
func (s *Server) listAgents(q *request, ref string) {
	ctx := q.r.Context()
	var out map[string]any
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, _, err := s.resolve(ctx, tx, &q.caller, ref, opAgentsList)
		if err != nil {
			return err
		}
		offset, size, err := page(q)
		if err != nil {
			return err
		}
		all, err := readAgents(ctx, tx, `silo = ?`, chain[len(chain)-1].ID)
		if err != nil {
			return err
		}
		list := []*Agent{}
		for i := offset; i < len(all) && i < offset+size; i++ {
			a, err := all[i].shown(ctx, tx)
			if err != nil {
				return err
			}
			list = append(list, a)
		}
		out = map[string]any{"agents": list}
		if tok := nextToken(offset, size, offset+size < len(all)); tok != "" {
			out["next_page_token"] = tok
		}
		return nil
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// getAgent is GET .../agents/{agent}, and with ref "" GET
// /silo/v1/agents/-{id}.
func (s *Server) getAgent(q *request, ref, name string) {
	ctx := q.r.Context()
	var out *Agent
	err := s.tx(ctx, func(tx store.Tx) error {
		if ref == "" {
			a, err := readAgent(ctx, tx, `id = ?`, strings.TrimPrefix(name, "-"))
			if err != nil {
				return err
			}
			if a == nil {
				return errorf(NotFound, "no agent %s", name).with("agent", name)
			}
			if _, _, err := s.resolve(ctx, tx, &q.caller, "-"+a.Silo, opAgentsGet); err != nil {
				var e *Error
				if errors.As(err, &e) && e.Code == NotFound {
					return errorf(NotFound, "no agent %s", name).with("agent", name)
				}
				return err
			}
			out, err = a.shown(ctx, tx)
			return err
		}
		_, _, a, err := s.resolveAgent(ctx, tx, q, ref, name, opAgentsGet)
		if err != nil {
			return err
		}
		out, err = a.shown(ctx, tx)
		return err
	})
	if err != nil {
		writeError(q.w, err)
		return
	}
	writeJSON(q.w, http.StatusOK, out)
}

// AgentPatch is a PATCH's body: budget.usd and labels, nothing else.
type AgentPatch struct {
	Budget *struct {
		USD string `json:"usd"`
	} `json:"budget"`
	Labels *map[string]string `json:"labels"`
}

// patchAgent is PATCH .../agents/{agent}: a new budget.usd, reserved or
// given back to the silo, or new labels. An agent's own session, and
// a manage holder, may not raise its budget (call 38; fence-api.md,
// "What a holder may do").
func (s *Server) patchAgent(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var p AgentPatch
	if err := decode(q, &p); err != nil {
		return nil, err
	}
	body, _ := q.r.Context().Value(bodyKey{}).(map[string]any)
	for k := range body {
		if k != "budget" && k != "labels" && k != "request_id" && !strings.HasPrefix(k, "\x00") {
			return nil, errorf(InvalidArgument, "%s can't be changed; PATCH changes budget.usd and labels", k).with("field", k)
		}
	}
	var out *Agent
	var start, stop bool
	var a *agentRow
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, _, row, err := s.resolveAgent(ctx, tx, q, ref, name, opAgentsPatch)
		if err != nil {
			return err
		}
		a = row
		t := chain[len(chain)-1]
		q.note(t, "agent.patch")
		if a.State == agents.StateDeleting {
			return errorf(FailedPrecondition, "agent %s is being deleted", a.Name)
		}
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		if p.Budget != nil {
			micros, err := ledger.ParseAmount(p.Budget.USD)
			if err != nil {
				return errorf(InvalidArgument, "budget.usd %q: %v", p.Budget.USD, err).with("field", "budget.usd")
			}
			if micros > a.budget {
				if ownSession(q.caller, a) {
					return errorf(PermissionDenied, "an agent's own session may not raise its own budget").with("agent", a.ID)
				}
				if q.caller.Manage != "" {
					return errorf(PermissionDenied, "a manage grant may not raise an agent's budget; that is an ancestor's").with("agent", a.ID)
				}
			}
			if err := ledger.ReserveModelBudget(ctx, tx, t.ID, a.ID, chargeVia(chain, q.caller), micros); err != nil {
				return budgetError(err)
			}
			a.budget = micros
			switch {
			case a.State == agents.StateBudgetExhausted && a.spent < a.budget:
				// Raising the budget puts it back to IDLE, resumed.
				if err := s.holdSession(ctx, tx, chain, q.caller, a.ID, a.Harness.Credential); err != nil {
					return err
				}
				a.State, a.StatusMessage, start = agents.StateIdle, "", true
			case agents.HoldsSession(a.State) && a.State != agents.StateProvisioning && a.spent >= a.budget:
				if stop, err = s.exhaust(ctx, tx, t, a); err != nil {
					return err
				}
			}
		}
		if p.Labels != nil {
			a.Labels = *p.Labels
			if a.Labels == nil {
				a.Labels = map[string]string{}
			}
		}
		if q.validateOnly {
			out, err = a.shown(ctx, tx)
			if err != nil {
				return err
			}
			return errValidateOnly
		}
		if err := a.save(ctx, tx); err != nil {
			return err
		}
		if out, err = a.shown(ctx, tx); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	if errors.Is(err, errValidateOnly) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	s.runnerAfter(ctx, a, start, stop)
	return out, nil
}

// runnerAfter starts (resumed) or stops an agent's session once the
// transaction that decided it has committed.
func (s *Server) runnerAfter(ctx context.Context, a *agentRow, start, stop bool) {
	if s.opts.Runner == nil {
		return
	}
	if stop {
		_ = s.opts.Runner.Stop(ctx, a.ID)
	}
	if start {
		if err := s.opts.Runner.Start(ctx, s.session(a, true)); err != nil {
			s.failAgent(ctx, a.ID, "the harness didn't resume: "+err.Error())
		}
	}
}

// stopAgent is POST .../agents/{agent}:stop: the harness stops, its
// session is saved, the tool VM is kept, and the session slot goes back
// to the silo.
func (s *Server) stopAgent(q *request, ref, name string) (any, error) {
	return s.agentAction(q, ref, name, opAgentsStop, KindAgentStop, func(ctx context.Context, tx store.Tx, chain []*tree.Silo, a *agentRow) (bool, bool, error) {
		switch a.State {
		case agents.StateStopped:
			return false, false, nil
		case agents.StateProvisioning, agents.StateRunning, agents.StateIdle, agents.StateWaiting, agents.StateBudgetExhausted:
		default:
			return false, false, errorf(FailedPrecondition, "agent %s is %s; a failed or deleting agent can't be stopped", a.Name, a.State)
		}
		if err := releaseSession(ctx, tx, a.ID); err != nil {
			return false, false, err
		}
		a.State, a.StatusMessage = agents.StateStopped, ""
		return false, true, nil
	})
}

// resumeAgent is POST .../agents/{agent}:resume: the harness starts
// again from its saved session. The silo's agent policy is checked
// again, since narrowing it applies here, and the agent takes a
// session slot.
func (s *Server) resumeAgent(q *request, ref, name string) (any, error) {
	return s.agentAction(q, ref, name, opAgentsResume, KindAgentResume, func(ctx context.Context, tx store.Tx, chain []*tree.Silo, a *agentRow) (bool, bool, error) {
		switch a.State {
		case agents.StateRunning, agents.StateIdle, agents.StateWaiting:
			return false, false, nil
		case agents.StateStopped, agents.StateFailed:
		default:
			return false, false, errorf(FailedPrecondition, "agent %s is %s; only a stopped or failed agent can be resumed", a.Name, a.State)
		}
		max, err := s.agentPolicy(ctx, tx, chain, a.Harness.Adapter, a.Harness.Credential, a.Model)
		if err != nil {
			return false, false, err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return false, false, err
		}
		if a.expire.Sub(now) > max+time.Second {
			return false, false, errorf(PermissionDenied, "the agent's lease, to %s, is longer than %s's agent_policy max_lease of %s from now; renew it shorter first",
				a.expire.Format(time.RFC3339), chain[len(chain)-1].Path, formatDuration(max)).with("policy", "agent_policy").with("field", "max_lease")
		}
		if a.spent >= a.budget {
			return false, false, errorf(FailedPrecondition, "agent %s has spent its budget for this period; raise budget.usd", a.Name)
		}
		if err := s.holdSession(ctx, tx, chain, q.caller, a.ID, a.Harness.Credential); err != nil {
			return false, false, err
		}
		a.State, a.StatusMessage = agents.StateIdle, ""
		return true, false, nil
	})
}

// renewAgent is POST .../agents/{agent}:renew, with {"expire_time"} or
// {"ttl"}: within the silo's effective max_lease from now, and never
// by the agent's own session (call 38).
func (s *Server) renewAgent(q *request, ref, name string) (any, error) {
	var l leaseRequest
	if err := decode(q, &l); err != nil {
		return nil, err
	}
	return s.agentAction(q, ref, name, opAgentsRenew, KindAgentRenew, func(ctx context.Context, tx store.Tx, chain []*tree.Silo, a *agentRow) (bool, bool, error) {
		if ownSession(q.caller, a) {
			return false, false, errorf(PermissionDenied, "an agent's own session may not renew its own lease").with("agent", a.ID)
		}
		if a.State == agents.StateDeleting {
			return false, false, errorf(FailedPrecondition, "agent %s is being deleted", a.Name)
		}
		if l.ExpireTime == "" && l.TTL == "" {
			return false, false, errorf(InvalidArgument, "give expire_time or ttl").with("field", "lease")
		}
		max, err := s.agentPolicy(ctx, tx, chain, a.Harness.Adapter, a.Harness.Credential, a.Model)
		if err != nil {
			return false, false, err
		}
		now, err := tx.Now(ctx)
		if err != nil {
			return false, false, err
		}
		end, err := leaseEnd(l, now, max, chain[len(chain)-1].Path)
		if err != nil {
			return false, false, err
		}
		a.expire = end
		return false, false, nil
	})
}

// agentAction runs one of :stop, :resume and :renew: fn changes the
// agent in the transaction, and says whether its session is to start
// or stop once it commits. Each is audited under kind.
func (s *Server) agentAction(q *request, ref, name string, op Op, kind string, fn func(ctx context.Context, tx store.Tx, chain []*tree.Silo, a *agentRow) (start, stop bool, err error)) (any, error) {
	ctx := q.r.Context()
	var out *Agent
	var a *agentRow
	var start, stop bool
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, _, row, err := s.resolveAgent(ctx, tx, q, ref, name, op)
		if err != nil {
			return err
		}
		a = row
		t := chain[len(chain)-1]
		q.note(t, kind)
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		before := a.State
		if start, stop, err = fn(ctx, tx, chain, a); err != nil {
			return err
		}
		if out, err = a.shown(ctx, tx); err != nil {
			return err
		}
		if q.validateOnly {
			return errValidateOnly
		}
		if err := a.save(ctx, tx); err != nil {
			return err
		}
		if err := Audit(ctx, tx, t, q.caller, kind, q.requestID, "OK", agentFields(a, map[string]any{
			"state": a.State, "from_state": before, "expire_time": a.expire,
		})); err != nil {
			return err
		}
		return s.finish(ctx, tx, q, out)
	})
	if errors.Is(err, errValidateOnly) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	s.runnerAfter(ctx, a, start, stop)
	return out, nil
}

// deleteAgent is DELETE .../agents/{agent}: a long-running operation
// that stops the session, deletes the tool VM and what the agent owns,
// and returns its budget.
func (s *Server) deleteAgent(q *request, ref, name string) (any, error) {
	ctx := q.r.Context()
	var out any
	var opID, agentID string
	err := s.tx(ctx, func(tx store.Tx) error {
		chain, _, a, err := s.resolveAgent(ctx, tx, q, ref, name, opAgentsDelete)
		if err != nil {
			return err
		}
		t := chain[len(chain)-1]
		q.note(t, KindAgentDelete)
		if q.validateOnly {
			owned, err := ownedBy(ctx, tx, a.ID)
			if err != nil {
				return err
			}
			out = map[string]any{"validate_only": true, "deletes": owned}
			return errValidateOnly
		}
		op, err := s.markDeleting(ctx, tx, t, a, q.caller)
		if err != nil {
			return err
		}
		opID, agentID, out = op.ID, a.ID, op.Operation
		return s.finish(ctx, tx, q, out)
	})
	if errors.Is(err, errValidateOnly) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	s.startAgentDelete(opID, agentID, KindAgentDelete)
	return out, nil
}

// markDeleting makes an agent DELETING with a delete operation, or
// returns the one it has.
func (s *Server) markDeleting(ctx context.Context, tx store.Tx, t *tree.Silo, a *agentRow, c Caller) (*opRow, error) {
	if a.State == agents.StateDeleting && a.op != "" {
		op, err := readOp(ctx, tx, `id = ?`, a.op)
		if err != nil {
			return nil, err
		}
		if op != nil {
			if err := setOp(ctx, tx, op.ID, agents.StateDeleting, false, nil); err != nil {
				return nil, err
			}
			return readOp(ctx, tx, `id = ?`, op.ID)
		}
	}
	if a.op != "" {
		// A create still waiting on its tool server ends here.
		if err := setOp(ctx, tx, a.op, OpDone, true, errorf(FailedPrecondition, "agent %s was deleted", a.Name)); err != nil {
			return nil, err
		}
	}
	op, err := startOp(ctx, tx, opKindAgentDelete, t, a, c, agents.StateDeleting)
	if err != nil {
		return nil, err
	}
	if err := releaseSession(ctx, tx, a.ID); err != nil {
		return nil, err
	}
	a.State, a.StatusMessage = agents.StateDeleting, ""
	return op, a.save(ctx, tx)
}

// ownedBy lists what an agent owns, as "silo:<path>" and
// "agent:<path>".
func ownedBy(ctx context.Context, tx store.Tx, agent string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT kind, object FROM agent_owned WHERE agent = ? ORDER BY kind, object`, agent)
	if err != nil {
		return nil, err
	}
	type ref struct{ kind, id string }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.kind, &r.id); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range refs {
		switch r.kind {
		case "silo":
			if t, err := tree.Get(ctx, tx, r.id); err == nil {
				out = append(out, "silo:"+t.Path)
			}
		case "agent":
			p, err := AgentPath(ctx, tx, r.id)
			if err != nil {
				return nil, err
			}
			out = append(out, "agent:"+p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// startAgentDelete runs an agent's delete operation in the background,
// unless it is already running.
func (s *Server) startAgentDelete(opID, agent, kind string) {
	if _, busy := s.running.LoadOrStore(opID, true); busy {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.running.Delete(opID)
		ctx := s.bg
		if err := s.runAgentDelete(ctx, agent, kind); err != nil && ctx.Err() == nil {
			_ = s.st.Tx(ctx, func(tx store.Tx) error {
				return setOp(ctx, tx, opID, OpDeleteFailed, false, asError(ctx, nil, err))
			})
		}
	}()
}

// runAgentDelete deletes a DELETING agent (agents.md, "The lease runs
// out", call 43): its session stops, its tool VM goes, then every
// child silo and agent its session created, bottom-up as delete
// --force does, then its budget goes back to its silo and its row is
// removed. A step that fails leaves it DELETING, for a repeat delete
// to resume.
func (s *Server) runAgentDelete(ctx context.Context, agent, kind string) error {
	var a *agentRow
	var t *tree.Silo
	var c Caller
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		if a, err = readAgent(ctx, tx, `id = ?`, agent); err != nil || a == nil {
			return err
		}
		if t, err = tree.Get(ctx, tx, a.Silo); err != nil {
			return err
		}
		op, err := readOp(ctx, tx, `id = ?`, a.op)
		if err != nil {
			return err
		}
		if op != nil {
			c = op.caller
		}
		return nil
	})
	if err != nil || a == nil {
		return err
	}
	if c.Silo == "" {
		if c, err = s.internalCaller(ctx); err != nil {
			return err
		}
	}
	if s.opts.Runner != nil {
		if err := s.opts.Runner.Stop(ctx, a.ID); err != nil {
			return errorf(Unavailable, "stopping agent %s's session: %v", a.Name, err)
		}
	}
	if s.opts.ToolVMs != nil {
		if err := s.opts.ToolVMs.Delete(ctx, c, t, a.ID, a.ToolVM.Zone, a.ToolVM.Instance); err != nil {
			return err
		}
	}
	// What it owns: agents first, then silos, each to the end.
	for {
		var kindOwned, object string
		err := s.st.Tx(ctx, func(tx store.Tx) error {
			err := tx.QueryRow(ctx, `SELECT kind, object FROM agent_owned WHERE agent = ? ORDER BY kind, object`, a.ID).Scan(&kindOwned, &object)
			if errors.Is(err, sql.ErrNoRows) {
				kindOwned = ""
				return nil
			}
			return err
		})
		if err != nil {
			return err
		}
		if kindOwned == "" {
			break
		}
		switch kindOwned {
		case "agent":
			if err := s.deleteAgentNow(ctx, object, c); err != nil {
				return err
			}
		case "silo":
			if err := s.deleteSiloNow(ctx, object, c); err != nil {
				return err
			}
		}
		// Whatever is left of the row goes, so the loop moves on.
		if err := s.st.Tx(ctx, func(tx store.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM agent_owned WHERE kind = ? AND object = ?`, kindOwned, object)
			return err
		}); err != nil {
			return err
		}
	}
	return s.tx(ctx, func(tx store.Tx) error {
		cur, err := readAgent(ctx, tx, `id = ?`, a.ID)
		if err != nil || cur == nil {
			return err
		}
		t, err := tree.Get(ctx, tx, cur.Silo)
		if err != nil {
			return err
		}
		if err := Audit(ctx, tx, t, c, kind, "", "OK", agentFields(cur, map[string]any{
			"budget_usd": usd(cur.budget), "spent_usd": usd(cur.spent), "instance": cur.ToolVM.Instance,
		})); err != nil {
			return err
		}
		if cur.op != "" {
			if err := setOp(ctx, tx, cur.op, OpDone, true, nil); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM agent_owned WHERE agent = ?`, cur.ID); err != nil {
			return err
		}
		return s.forgetAgent(ctx, tx, cur.ID)
	})
}

// deleteAgentNow marks an agent DELETING and deletes it, in this
// goroutine: an owned agent, or one in a silo being deleted.
func (s *Server) deleteAgentNow(ctx context.Context, agent string, c Caller) error {
	err := s.tx(ctx, func(tx store.Tx) error {
		a, err := readAgent(ctx, tx, `id = ?`, agent)
		if err != nil || a == nil {
			return err
		}
		t, err := tree.Get(ctx, tx, a.Silo)
		if err != nil {
			return err
		}
		_, err = s.markDeleting(ctx, tx, t, a, c)
		return err
	})
	if err != nil {
		return err
	}
	return s.runAgentDelete(ctx, agent, KindAgentDelete)
}

// deleteSiloNow deletes a silo and its subtree, as delete with force
// does, in this goroutine.
func (s *Server) deleteSiloNow(ctx context.Context, silo string, c Caller) error {
	var opID string
	err := s.tx(ctx, func(tx store.Tx) error {
		t, err := tree.Get(ctx, tx, silo)
		if errors.Is(err, tree.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		chain, err := tree.Ancestry(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
		op, err := s.startSiloDelete(ctx, tx, t, c)
		if err != nil {
			return err
		}
		opID = op.ID
		return nil
	})
	if err != nil || opID == "" {
		return err
	}
	if _, busy := s.running.LoadOrStore(opID, true); !busy {
		s.runDelete(ctx, opID)
		s.running.Delete(opID)
	}
	var op *opRow
	if err := s.st.Tx(ctx, func(tx store.Tx) error {
		op, err = readOp(ctx, tx, `id = ?`, opID)
		return err
	}); err != nil {
		return err
	}
	if op != nil && !op.Done {
		if op.Error != nil {
			return op.Error
		}
		return errorf(Unavailable, "deleting %s did not finish", op.Target)
	}
	return nil
}

// deleteAgentsIn deletes every agent in a silo, before the silo goes
// (fence-api.md: deleting a silo with force deletes its agents first).
func (s *Server) deleteAgentsIn(ctx context.Context, silo string, c Caller) error {
	var list []*agentRow
	if err := s.st.Tx(ctx, func(tx store.Tx) error {
		var err error
		list, err = readAgents(ctx, tx, `silo = ?`, silo)
		return err
	}); err != nil {
		return err
	}
	for _, a := range list {
		if err := s.deleteAgentNow(ctx, a.ID, c); err != nil {
			return err
		}
	}
	return nil
}

// internalCaller is silod acting on its own, as the root: an expiry,
// or a deletion whose caller isn't recorded.
func (s *Server) internalCaller(ctx context.Context) (Caller, error) {
	var c Caller
	err := s.st.Tx(ctx, func(tx store.Tx) error {
		root, err := tree.Root(ctx, tx)
		if err != nil {
			return err
		}
		c = Caller{Silo: root.ID, Via: ViaInternal}
		return nil
	})
	return c, err
}

// ExpireAgents deletes every agent whose lease has run out by database
// time, audited agent.expire (agents.md, "The lease runs out"), and
// returns how many it started on. silod's leader runs it.
func (s *Server) ExpireAgents(ctx context.Context) (int, error) {
	c, err := s.internalCaller(ctx)
	if err != nil {
		if errors.Is(err, tree.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	var due []string
	err = s.tx(ctx, func(tx store.Tx) error {
		now, err := tx.Now(ctx)
		if err != nil {
			return err
		}
		list, err := readAgents(ctx, tx, `expire_at <= ? AND state <> ?`, store.Millis(now), agents.StateDeleting)
		if err != nil {
			return err
		}
		due = due[:0]
		for _, a := range list {
			t, err := tree.Get(ctx, tx, a.Silo)
			if err != nil {
				return err
			}
			if _, err := s.markDeleting(ctx, tx, t, a, c); err != nil {
				return err
			}
			due = append(due, a.ID)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, id := range due {
		if err := s.runAgentDelete(ctx, id, KindAgentExpire); err != nil {
			return len(due), err
		}
	}
	return len(due), nil
}
