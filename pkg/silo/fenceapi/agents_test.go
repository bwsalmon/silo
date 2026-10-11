package fenceapi_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// fakeVMs is a ToolVMs that records what it is asked, and fails a
// create with fail when it is set.
type fakeVMs struct {
	mu      sync.Mutex
	fail    error
	made    map[string]string // instance real name (silo/instance) to the caller that made it
	deleted []string
	script  string
}

func (f *fakeVMs) Create(_ context.Context, c fenceapi.Caller, silo *tree.Silo, agent, instance string, spec fenceapi.ToolVMSpec, script string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	if f.made == nil {
		f.made = map[string]string{}
	}
	who := c.Silo
	if c.Manage != "" {
		who += " via manage"
	}
	f.made[silo.Path+"/"+instance] = who
	f.script = script
	return "10.1.16.3", nil
}

func (f *fakeVMs) Delete(_ context.Context, _ fenceapi.Caller, silo *tree.Silo, agent, zone, instance string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, silo.Path+"/"+instance)
	return nil
}

func (f *fakeVMs) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

func (f *fakeVMs) madeBy(vm string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.made[vm]
}

func (f *fakeVMs) startupScript() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.script
}

func (f *fakeVMs) deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.deleted...)
	sort.Strings(out)
	return out
}

type agentEnv struct {
	*env
	vms    *fakeVMs
	runner *agents.Fake
	team   string // /team: 100 dollars of model budget, 3 sessions
}

func newAgentEnv(t *testing.T) *agentEnv {
	t.Helper()
	vms := &fakeVMs{}
	runner := &agents.Fake{}
	e := &agentEnv{vms: vms, runner: runner}
	e.env = newEnv(t, fenceapi.Options{ToolVMs: vms, Runner: runner,
		Credentials: agents.StaticCredentials{"anthropic-api": "anthropic", "openai-api": "openai", "spare": "anthropic"}})
	runner.Connected = e.api.ToolServerConnected
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		return ledger.SetCapacity(ctx, tx, e.root, []ledger.Amount{{Key: fenceapi.AgentSessions, Amount: ledger.Units(10)}})
	}); err != nil {
		t.Fatal(err)
	}
	e.team = e.mk(e.root, "-"+e.root, "team", obj{"model_budget_usd": "100", "quota": obj{"global": obj{"agent_sessions": 3}}})
	return e
}

func agentBody(name string, extra obj) obj {
	b := obj{"name": name, "harness": obj{"adapter": "claude-code", "credential": "anthropic-api"},
		"model": "claude-opus-5-5", "budget": obj{"usd": "40.00"},
		"tool_vm": obj{"zone": "us-central1-a", "subnet": "work"}, "lease": obj{"ttl": "4h"},
		"prompt": "Fix the off-by-one.", "labels": obj{"task": "1945"}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// create makes an agent in silo (a path without its leading /) as
// caller and returns its id, its create operation not yet done.
func (e *agentEnv) create(caller, silo, name string, extra obj) string {
	e.t.Helper()
	op := e.ok(caller, "POST", "/silo/v1/silos/"+silo+"/agents", agentBody(name, extra))
	if op["done"] != false || op["kind"] != "agent.create" {
		e.t.Fatalf("create %s: %v", name, op)
	}
	return e.ok(caller, "GET", "/silo/v1/silos/"+silo+"/agents/"+name, nil)["id"].(string)
}

// used is silo's used amount of key, in whole units.
func (e *agentEnv) used(silo string, k ledger.Key) string {
	e.t.Helper()
	var r ledger.Row
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		var err error
		r, err = ledger.Get(ctx, tx, silo, k)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return ledger.FormatAmount(r.Used)
}

func (e *agentEnv) wantUsed(what, silo, usd, sessions string) {
	e.t.Helper()
	if got := e.used(silo, ledger.ModelUSD); got != usd {
		e.t.Errorf("%s: model/usd used %s, want %s", what, got, usd)
	}
	if got := e.used(silo, fenceapi.AgentSessions); got != sessions {
		e.t.Errorf("%s: agent_sessions used %s, want %s", what, got, sessions)
	}
}

func (e *agentEnv) records(silo, kind string) []obj {
	e.t.Helper()
	var out []obj
	for _, r := range e.ok(e.root, "GET", "/silo/v1/silos/-"+silo+"/audit?kind="+kind, nil)["records"].([]any) {
		if r.(obj)["result"] == "OK" {
			out = append(out, r.(obj))
		}
	}
	return out
}

func (e *agentEnv) gone(silo, name string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, _ := e.do(e.root, "GET", "/silo/v1/silos/"+silo+"/agents/"+name, nil)
		if code == 404 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("agent %s/%s is still there", silo, name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentLifecycle(t *testing.T) {
	e := newAgentEnv(t)
	op := e.ok(e.root, "POST", "/silo/v1/silos/team/agents", agentBody("fixer", nil))
	if op["done"] != false || op["state"] != "PROVISIONING" || op["target"] != "/team/agents/fixer" {
		t.Fatalf("create: %v", op)
	}
	a := e.ok(e.root, "GET", "/silo/v1/silos/team/agents/fixer", nil)
	id := a["id"].(string)
	if a["state"] != "PROVISIONING" || get(a, "tool_vm", "instance") != "agent-fixer" || get(a, "tool_vm", "tool_server") != "PENDING" ||
		get(a, "tool_vm", "ip") != "10.1.16.3" || get(a, "harness", "version") != agents.Adapters["claude-code"].Version ||
		get(a, "budget", "usd") != "40.00" || get(a, "budget", "spent_usd") != "0.00" || a["silo_path"] != "/team" || a["owner"] != nil ||
		get(a, "tool_vm", "machine_type") != fenceapi.DefaultToolMachineType || get(a, "labels", "task") != "1945" {
		t.Fatalf("agent: %v", a)
	}
	if id == e.team || id == e.root || len(id) != 6 {
		t.Errorf("agent id %q", id)
	}
	if e.vms.madeBy("/team/agent-fixer") != e.root {
		t.Errorf("tool VM made by %v, want the root, the caller", e.vms.madeBy("/team/agent-fixer"))
	}
	if !strings.Contains(e.vms.startupScript(), agents.ToolsBinaryURL) || !strings.Contains(e.vms.startupScript(), "systemctl enable --now silo-tools") {
		t.Errorf("startup script:\n%s", e.vms.startupScript())
	}
	e.wantUsed("created", e.team, "40", "1")
	if got := e.runner.Log(); len(got) != 1 || got[0] != "start "+id {
		t.Errorf("runner: %v", got)
	}
	// The operation finishes when the tool server connects.
	if err := e.runner.Connect(ctx, id); err != nil {
		t.Fatal(err)
	}
	if op := e.waitOp(e.root, op["name"].(string)); op["done"] != true || op["state"] != "DONE" {
		t.Fatalf("operation: %v", op)
	}
	a = e.ok(e.team, "GET", "/silo/v1/agents/-"+id, nil)
	if a["state"] != "RUNNING" || get(a, "tool_vm", "tool_server") != "CONNECTED" || get(a, "session", "id") == nil {
		t.Fatalf("connected: %v", a)
	}
	e.ok(e.team, "GET", "/silo/v1/self/agents/-"+id, nil)
	if l := e.ok(e.team, "GET", "/silo/v1/self/agents", nil)["agents"].([]any); len(l) != 1 {
		t.Errorf("list: %v", l)
	}
	other := e.mk(e.root, "-"+e.root, "other", nil)
	e.fail(fenceapi.NotFound, other, "GET", "/silo/v1/agents/-"+id, nil)
	e.fail(fenceapi.NotFound, other, "GET", "/silo/v1/silos/team/agents", nil)
	if recs := e.records(e.team, "agent.create"); len(recs) != 1 || get(recs[0], "fields", "agent") != id {
		t.Errorf("audit: %v", recs)
	}

	// Stop gives the session slot back; resume takes it again.
	if a := e.ok(e.team, "POST", "/silo/v1/self/agents/fixer:stop", nil); a["state"] != "STOPPED" {
		t.Fatalf("stop: %v", a)
	}
	e.wantUsed("stopped", e.team, "40", "0")
	if a := e.ok(e.team, "POST", "/silo/v1/self/agents/fixer:resume", nil); a["state"] != "IDLE" {
		t.Fatalf("resume: %v", a)
	}
	e.wantUsed("resumed", e.team, "40", "1")
	if got := e.runner.Log(); got[len(got)-2] != "stop "+id || got[len(got)-1] != "resume "+id {
		t.Errorf("runner: %v", got)
	}
	// Renew, within max_lease (the root's 7 days).
	a = e.ok(e.team, "POST", "/silo/v1/self/agents/fixer:renew", obj{"ttl": "48h"})
	if exp, _ := time.Parse(time.RFC3339, get(a, "lease", "expire_time").(string)); time.Until(exp) < 47*time.Hour {
		t.Errorf("renew: %v", a["lease"])
	}
	e.fail(fenceapi.PermissionDenied, e.team, "POST", "/silo/v1/self/agents/fixer:renew", obj{"ttl": "200h"})
	// PATCH: the budget, up and down, and labels; nothing else.
	a = e.ok(e.team, "PATCH", "/silo/v1/self/agents/fixer", obj{"budget": obj{"usd": "60"}, "labels": obj{"x": "y"}})
	if get(a, "budget", "usd") != "60.00" || get(a, "labels", "x") != "y" {
		t.Errorf("patch: %v", a)
	}
	e.wantUsed("raised", e.team, "60", "1")
	er := e.fail(fenceapi.QuotaExceeded, e.team, "PATCH", "/silo/v1/self/agents/fixer", obj{"budget": obj{"usd": "101"}})
	if get(er, "details", "class") != "model_usd" {
		t.Errorf("over budget: %v", er)
	}
	e.ok(e.team, "PATCH", "/silo/v1/self/agents/fixer", obj{"budget": obj{"usd": "10"}})
	e.wantUsed("lowered", e.team, "10", "1")
	e.fail(fenceapi.InvalidArgument, e.team, "PATCH", "/silo/v1/self/agents/fixer", obj{"model": "claude-sonnet-5-5"})
	for _, k := range []string{"agent.stop", "agent.resume", "agent.renew"} {
		if len(e.records(e.team, k)) != 1 {
			t.Errorf("no %s record", k)
		}
	}

	// Delete: a long-running operation; the tool VM goes and the budget
	// comes back.
	op = e.ok(e.team, "DELETE", "/silo/v1/self/agents/fixer", nil)
	if op["kind"] != "agent.delete" {
		t.Fatalf("delete: %v", op)
	}
	if op := e.waitOp(e.team, op["name"].(string)); op["done"] != true {
		t.Fatalf("delete operation: %v", op)
	}
	e.gone("team", "fixer")
	e.wantUsed("deleted", e.team, "0", "0")
	if d := e.vms.deletes(); len(d) != 1 || d[0] != "/team/agent-fixer" {
		t.Errorf("tool VM deletes: %v", d)
	}
	if len(e.records(e.team, "agent.delete")) != 1 {
		t.Error("no agent.delete record")
	}
	if len(e.runner.Running()) != 0 {
		t.Errorf("still running: %v", e.runner.Running())
	}
}

func TestAgentCreateChecks(t *testing.T) {
	e := newAgentEnv(t)
	path := "/silo/v1/silos/team/agents"
	e.create(e.root, "team", "fixer", nil)
	type tc struct {
		code  fenceapi.Code
		body  obj
		field string // details.field, or details.class for QUOTA_EXCEEDED
	}
	for name, c := range map[string]tc{
		"bad name":         {fenceapi.InvalidArgument, agentBody("Fixer_1", nil), "name"},
		"dup":              {fenceapi.AlreadyExists, agentBody("fixer", nil), ""},
		"no zone":          {fenceapi.InvalidArgument, agentBody("a", obj{"tool_vm": obj{"subnet": "work"}}), "tool_vm.zone"},
		"no subnet":        {fenceapi.InvalidArgument, agentBody("a", obj{"tool_vm": obj{"zone": "us-central1-a"}}), "tool_vm.subnet"},
		"startup-script":   {fenceapi.InvalidArgument, agentBody("a", obj{"tool_vm": obj{"zone": "us-central1-a", "subnet": "work", "metadata": obj{"startup-script": "rm -rf /"}}}), "tool_vm.metadata"},
		"unknown adapter":  {fenceapi.InvalidArgument, agentBody("a", obj{"harness": obj{"adapter": "aider", "credential": "anthropic-api"}}), "harness.adapter"},
		"unknown cred":     {fenceapi.InvalidArgument, agentBody("a", obj{"harness": obj{"adapter": "claude-code", "credential": "nope"}}), "harness.credential"},
		"cred not in root": {fenceapi.PermissionDenied, agentBody("a", obj{"harness": obj{"adapter": "claude-code", "credential": "spare"}}), "credentials"},
		"wrong provider":   {fenceapi.InvalidArgument, agentBody("a", obj{"harness": obj{"adapter": "codex", "credential": "anthropic-api"}}), "harness.credential"},
		"unpriced model":   {fenceapi.InvalidArgument, agentBody("a", obj{"model": "claude-imaginary"}), "model"},
		"model of other":   {fenceapi.InvalidArgument, agentBody("a", obj{"model": "gpt-5"}), "model"},
		"lease too long":   {fenceapi.PermissionDenied, agentBody("a", obj{"lease": obj{"ttl": "169h"}}), "max_lease"},
		"lease past":       {fenceapi.InvalidArgument, agentBody("a", obj{"lease": obj{"expire_time": "2020-01-01T00:00:00Z"}}), "lease"},
		"no budget":        {fenceapi.InvalidArgument, agentBody("a", obj{"budget": obj{}}), "budget.usd"},
		"over budget":      {fenceapi.QuotaExceeded, agentBody("a", obj{"budget": obj{"usd": "60.01"}}), "model_usd"},
	} {
		er := e.fail(c.code, e.root, "POST", path, c.body)
		key := "field"
		if c.code == fenceapi.QuotaExceeded {
			key = "class"
		}
		if c.field != "" && get(er, "details", key) != c.field {
			t.Errorf("%s: %v, want %s %s", name, er, key, c.field)
		}
	}
	// A refused create holds nothing.
	e.wantUsed("after refusals", e.team, "40", "1")

	// agent_sessions: team has 3.
	e.create(e.root, "team", "b", obj{"budget": obj{"usd": "1"}})
	e.create(e.root, "team", "c", obj{"budget": obj{"usd": "1"}})
	er := e.fail(fenceapi.QuotaExceeded, e.root, "POST", path, agentBody("d", obj{"budget": obj{"usd": "1"}}))
	if get(er, "details", "class") != "agent_sessions" {
		t.Errorf("sessions: %v", er)
	}
	e.wantUsed("full", e.team, "42", "3")
	// The control VMs' slots.
	e.ok(e.root, "POST", "/silo/v1/silos/team/agents/c:stop", nil)
	e.runner.SetSlots(len(e.runner.Running()))
	e.fail(fenceapi.ResourceExhausted, e.root, "POST", path, agentBody("d", obj{"budget": obj{"usd": "1"}}))
	e.runner.SetSlots(0)
	// The facade's refusal of the tool VM fails the create, and gives
	// back what it held.
	e.vms.setFail(&fenceapi.Error{Code: fenceapi.QuotaExceeded, Message: "tool VM: us-central1 vcpu", Details: map[string]any{"class": "vcpu"}})
	er = e.fail(fenceapi.QuotaExceeded, e.root, "POST", path, agentBody("d", obj{"budget": obj{"usd": "1"}}))
	if get(er, "details", "class") != "vcpu" {
		t.Errorf("tool VM quota: %v", er)
	}
	e.vms.setFail(nil)
	e.wantUsed("tool VM refused", e.team, "42", "2")
	e.fail(fenceapi.NotFound, e.root, "GET", path+"/d", nil)
	// validate_only runs the checks and makes nothing.
	v := e.ok(e.root, "POST", path+"?validate_only=true", agentBody("d", obj{"budget": obj{"usd": "1"}}))
	if v["validate_only"] != true || get(v, "agent", "name") != "d" {
		t.Errorf("validate_only: %v", v)
	}
	e.fail(fenceapi.NotFound, e.root, "GET", path+"/d", nil)
	e.wantUsed("validate only", e.team, "42", "2")
	// A path under an agent that names no part of its control surface.
	e.fail(fenceapi.NotFound, e.root, "GET", path+"/fixer/transcript", nil)
}

func TestAgentPolicy(t *testing.T) {
	e := newAgentEnv(t)
	// /team narrows to claude-* and 24h; /team/child inherits the
	// adapters and credentials it doesn't list, and narrows models.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team", obj{"agent_policy": obj{"models": []string{"claude-*"}, "max_lease": "24h"}})
	e.mk(e.root, "team", "child", obj{"model_budget_usd": "10", "quota": obj{"global": obj{"agent_sessions": 1}},
		"agent_policy": obj{"models": []string{"claude-sonnet-*", "claude-opus-5-5"}}})
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", "/silo/v1/silos/team/child", obj{"agent_policy": obj{"models": []string{"gpt-*"}}})
	e.fail(fenceapi.LooserThanParent, e.root, "PATCH", "/silo/v1/silos/team/child", obj{"agent_policy": obj{"max_lease": "48h"}})
	child := "/silo/v1/silos/team/child/agents"
	er := e.fail(fenceapi.PermissionDenied, e.root, "POST", child, agentBody("a", obj{"model": "claude-haiku-4-5-20251001", "budget": obj{"usd": "1"}}))
	if get(er, "details", "field") != "models" {
		t.Errorf("model: %v", er)
	}
	er = e.fail(fenceapi.PermissionDenied, e.root, "POST", child, agentBody("a", obj{"budget": obj{"usd": "1"}, "lease": obj{"ttl": "25h"}}))
	if get(er, "details", "field") != "max_lease" {
		t.Errorf("lease: %v", er)
	}
	// The codex adapter and its credential are inherited from the root,
	// but its models aren't claude-*.
	e.fail(fenceapi.PermissionDenied, e.root, "POST", child, agentBody("a", obj{"harness": obj{"adapter": "codex", "credential": "openai-api"}, "model": "gpt-5", "budget": obj{"usd": "1"}}))
	e.create(e.root, "team/child", "a", obj{"budget": obj{"usd": "1"}, "lease": obj{"ttl": "20h"}})
	// An empty list allows nothing.
	e.ok(e.root, "PATCH", "/silo/v1/silos/team/child", obj{"agent_policy": obj{"models": []string{"claude-opus-5-5"}, "adapters": []string{}}})
	er = e.fail(fenceapi.PermissionDenied, e.root, "POST", child, agentBody("b", obj{"budget": obj{"usd": "1"}}))
	if get(er, "details", "field") != "adapters" {
		t.Errorf("empty adapters: %v", er)
	}
	// Narrowing doesn't stop a running agent; it applies at resume and
	// renewal.
	if a := e.ok(e.root, "GET", child+"/a", nil); a["state"] != "PROVISIONING" {
		t.Errorf("after narrowing: %v", a["state"])
	}
	e.fail(fenceapi.PermissionDenied, e.root, "POST", child+"/a:renew", obj{"ttl": "1h"})
	e.ok(e.root, "POST", child+"/a:stop", nil)
	e.fail(fenceapi.PermissionDenied, e.root, "POST", child+"/a:resume", nil)
}

func TestAgentOwnSession(t *testing.T) {
	e := newAgentEnv(t)
	id := e.create(e.team, "team", "fixer", nil)
	e.runner.Connect(ctx, id)
	harness := e.team + "/agent:" + id
	// Call 38: its own session may not renew its lease or raise its
	// budget; it may lower it. Anyone else in the silo may do either.
	e.fail(fenceapi.PermissionDenied, harness, "POST", "/silo/v1/self/agents/fixer:renew", obj{"ttl": "8h"})
	e.fail(fenceapi.PermissionDenied, harness, "PATCH", "/silo/v1/self/agents/fixer", obj{"budget": obj{"usd": "50"}})
	e.ok(harness, "PATCH", "/silo/v1/self/agents/fixer", obj{"budget": obj{"usd": "30"}})
	e.ok(e.team, "POST", "/silo/v1/self/agents/fixer:renew", obj{"ttl": "8h"})
	e.ok(e.team, "PATCH", "/silo/v1/self/agents/fixer", obj{"budget": obj{"usd": "35"}})

	// What its session creates is owned by it (call 43) and goes with
	// it: a sandbox child silo with an agent of its own, and a helper
	// agent beside it.
	sandbox := e.mk(harness, "self", "sandbox", obj{"model_budget_usd": "20", "quota": obj{"global": obj{"agent_sessions": 1}}})
	if d := e.ok(e.team, "GET", "/silo/v1/silos/team/sandbox", nil); d["owner_agent"] != id {
		t.Errorf("sandbox owner_agent: %v", d["owner_agent"])
	}
	inner := e.create(harness, "team/sandbox", "inner", obj{"budget": obj{"usd": "5"}})
	helper := e.create(harness, "team", "helper", obj{"budget": obj{"usd": "5"}})
	if a := e.ok(e.team, "GET", "/silo/v1/self/agents/helper", nil); a["owner"] != id {
		t.Errorf("helper owner: %v", a["owner"])
	}
	e.wantUsed("before", e.team, "40", "2") // fixer 35 and helper 5; the sandbox's 20 is delegated
	op := e.ok(e.root, "DELETE", "/silo/v1/silos/team/agents/fixer", nil)
	if op := e.waitOp(e.root, op["name"].(string)); op["done"] != true {
		t.Fatalf("delete: %v", op)
	}
	e.gone("team", "fixer")
	e.gone("team", "helper")
	e.fail(fenceapi.NotFound, e.root, "GET", "/silo/v1/silos/-"+sandbox, nil)
	if d := e.vms.deletes(); strings.Join(d, ",") != "/team/agent-fixer,/team/agent-helper,/team/sandbox/agent-inner" {
		t.Errorf("tool VM deletes: %v", d)
	}
	e.wantUsed("after", e.team, "0", "0")
	_ = inner
	_ = helper
}

func TestAgentSiloForceDelete(t *testing.T) {
	e := newAgentEnv(t)
	e.create(e.root, "team", "a", nil)
	e.create(e.root, "team", "b", obj{"budget": obj{"usd": "1"}})
	// Without force, the agents' charges keep the silo.
	e.fail(fenceapi.FailedPrecondition, e.root, "DELETE", "/silo/v1/silos/team", nil)
	op := e.ok(e.root, "DELETE", "/silo/v1/silos/team?force=true", nil)
	if op := e.waitOp(e.root, op["name"].(string)); op["done"] != true {
		t.Fatalf("delete: %v", op)
	}
	if d := e.vms.deletes(); strings.Join(d, ",") != "/team/agent-a,/team/agent-b" {
		t.Errorf("tool VM deletes: %v", d)
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM agents`).Scan(&n); err != nil || n != 0 {
			return errors.Join(err, errors.New("agents left"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSealedAndManage(t *testing.T) {
	e := newAgentEnv(t)
	sealed := e.mk(e.root, "team", "sealed", obj{"control": "none", "model_budget_usd": "10", "quota": obj{"global": obj{"agent_sessions": 2}}})
	// A sealed silo can't create its own; its ancestor can, and the tool
	// VM is made under the ancestor's authority.
	e.fail(fenceapi.ControlDenied, sealed, "POST", "/silo/v1/self/agents", agentBody("op", obj{"budget": obj{"usd": "1"}}))
	e.create(e.team, "team/sealed", "op", obj{"budget": obj{"usd": "1"}})
	if e.vms.madeBy("/team/sealed/agent-op") != e.team {
		t.Errorf("tool VM made by %v, want /team", e.vms.madeBy("/team/sealed/agent-op"))
	}
	e.fail(fenceapi.ControlDenied, sealed, "POST", "/silo/v1/self/agents/op:renew", obj{"ttl": "1h"})

	deploy := e.mk(e.root, "-"+e.root, "deploy", nil)
	grant := func(name string, fence []string) {
		g := e.ok(e.root, "POST", "/silo/v1/silos/team/sealed/manageGrants", obj{"name": name, "holder": "/deploy",
			"scope": obj{"fence": fence}, "lease": obj{"ttl": "3600s"}})
		e.ok(deploy, "POST", "/silo/v1/self/manageGrants/"+g["id"].(string)+":accept", nil)
	}
	// Without the agents group: no agents.
	grant("readonly", []string{"read"})
	er := e.fail(fenceapi.PermissionDenied, deploy, "GET", "/silo/v1/silos/-"+sealed+"/agents", nil)
	if get(er, "details", "needs") != "agents" {
		t.Errorf("without agents: %v", er)
	}
	e.fail(fenceapi.PermissionDenied, deploy, "POST", "/silo/v1/silos/-"+sealed+"/agents", agentBody("x", obj{"budget": obj{"usd": "1"}}))
	// With it: create, stop, resume, renew, and delete, but not raise a
	// budget.
	grant("ops", []string{"read", "agents"})
	e.ok(deploy, "GET", "/silo/v1/silos/-"+sealed+"/agents", nil)
	op := e.ok(deploy, "POST", "/silo/v1/silos/-"+sealed+"/agents", agentBody("x", obj{"budget": obj{"usd": "2"}}))
	if e.vms.madeBy("/team/sealed/agent-x") != deploy+" via manage" {
		t.Errorf("tool VM made by %v", e.vms.madeBy("/team/sealed/agent-x"))
	}
	if recs := e.records(sealed, "agent.create"); len(recs) != 2 || !strings.Contains(str(recs[0]["via"])+" "+str(recs[1]["via"]), "manage:"+deploy) {
		t.Errorf("audit: %v", recs)
	}
	e.fail(fenceapi.PermissionDenied, deploy, "PATCH", "/silo/v1/silos/-"+sealed+"/agents/x", obj{"budget": obj{"usd": "3"}})
	e.ok(deploy, "PATCH", "/silo/v1/silos/-"+sealed+"/agents/x", obj{"budget": obj{"usd": "1"}})
	e.ok(deploy, "POST", "/silo/v1/silos/-"+sealed+"/agents/x:renew", obj{"ttl": "2h"})
	op = e.ok(deploy, "DELETE", "/silo/v1/silos/-"+sealed+"/agents/x", nil)
	if op := e.waitOp(deploy, op["name"].(string)); op["done"] != true {
		t.Fatalf("delete: %v", op)
	}
	e.wantUsed("sealed", sealed, "1", "1")
}

func TestAgentExpire(t *testing.T) {
	e := newAgentEnv(t)
	id := e.create(e.root, "team", "a", nil)
	if n, err := e.api.ExpireAgents(ctx); n != 0 || err != nil {
		t.Fatalf("expire early: %d %v", n, err)
	}
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agents SET expire_at = ? WHERE id = ?`, store.Millis(time.Now().Add(-time.Second)), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.api.ExpireAgents(ctx); n != 1 || err != nil {
		t.Fatalf("expire: %d %v", n, err)
	}
	e.gone("team", "a")
	if recs := e.records(e.team, "agent.expire"); len(recs) != 1 || get(recs[0], "fields", "agent") != id {
		t.Errorf("audit: %v", recs)
	}
	e.wantUsed("expired", e.team, "0", "0")
}

func TestModelPrices(t *testing.T) {
	e := newAgentEnv(t)
	l := e.ok(e.team, "GET", "/silo/v1/modelPrices", nil)["model_prices"].([]any)
	found := false
	for _, p := range l {
		found = found || (p.(obj)["model"] == "claude-opus-5-5" && p.(obj)["source"] == "release")
	}
	if !found {
		t.Fatalf("prices: %v", l)
	}
	price := obj{"input": "2", "output": "8", "cache_write": "2.5", "cache_read": "0.2"}
	e.fail(fenceapi.PermissionDenied, e.team, "PUT", "/silo/v1/modelPrices/anthropic/claude-next", price)
	e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/silos/team/agents", agentBody("a", obj{"model": "claude-next"}))
	if p := e.ok(e.root, "PUT", "/silo/v1/modelPrices/anthropic/claude-next", price); p["source"] != "override" || p["cache_read"] != "0.20" {
		t.Errorf("set: %v", p)
	}
	e.create(e.root, "team", "a", obj{"model": "claude-next"})
	e.ok(e.root, "DELETE", "/silo/v1/modelPrices/anthropic/claude-next", nil)
	e.fail(fenceapi.InvalidArgument, e.root, "POST", "/silo/v1/silos/team/agents", agentBody("b", obj{"model": "claude-next"}))
	e.fail(fenceapi.NotFound, e.root, "DELETE", "/silo/v1/modelPrices/anthropic/claude-next", nil)
}
