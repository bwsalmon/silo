//go:build silo_conformance

package conformance

// The agent cases (docs/silo/testing.md, "Cases", "Agents in a sealed
// silo", SV-122; docs/silo/agents.md): an agent in a sealed silo, made
// by the root, with a tool VM, and what its harness keeps and loses;
// manage holders with and without the agents scope; and an unsealed
// agent's happy path, from create to its lease running out.
//
// What the harness does is driven the only way it can be: by telling
// the agent, in its prompt and in messages, which tools to call, and
// reading what it did from its session's events (tool_call and
// tool_result, usage, turn). That needs a model, so those cases need a
// model credential: an Anthropic API key in the file -silo.model-key
// (SILO_CONFORMANCE_MODEL_KEY) names. Without one the agents are still
// created, with a placeholder credential whose model calls fail, and
// every case that needs a model call is skipped, not passed. The key is
// read from the file at run time and never written anywhere but silod's
// credential store, which uninstall empties.

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/client"
)

// agentModel is the agents' model: the cheapest the price table has
// for the claude-code adapter.
const agentModel = "claude-haiku-4-5-20251001"

// agentCredentialName is the credential the cases add.
const agentCredentialName = "conformance-anthropic"

// agentVMType is the tool VMs' machine type.
const agentVMType = "e2-small"

var (
	credOnce sync.Once
	credErr  error
)

// haveModel says whether this run has a model credential.
func haveModel() bool { return *flagModelKey != "" }

// needModel skips a case that needs a model call when the run has no
// model credential.
func needModel(t *testing.T) {
	t.Helper()
	if !haveModel() {
		t.Skip("no model credential (-silo.model-key or SILO_CONFORMANCE_MODEL_KEY, an Anthropic API key): this case needs a model call, so it is skipped, not passed")
	}
}

// agentCredential adds the cases' credential once: the model key if the
// run has one, or a placeholder that the model provider refuses, so that
// the agents can still be created and their tool VMs tested.
func agentCredential(t *testing.T) {
	t.Helper()
	credOnce.Do(func() {
		secret := "conformance-placeholder-not-a-key"
		if haveModel() {
			b, err := os.ReadFile(*flagModelKey)
			if err != nil {
				credErr = fmt.Errorf("reading the model key: %v", err)
				return
			}
			secret = strings.TrimSpace(string(b))
		}
		_, credErr = run.api.AddCredential(context.Background(), client.CredentialRequest{
			Name: agentCredentialName, Provider: "anthropic", Kind: "api_key", Secret: secret}, client.Mutation{})
	})
	if credErr != nil {
		t.Fatalf("adding the agent credential: %v", credErr)
	}
}

// agentSilo creates path under the root with control (if not ""), a
// model budget, one agent session, room for a tool VM and a probe, and
// a net subnet.
func agentSilo(t *testing.T, path, control string) siloInfo {
	t.Helper()
	ctx := context.Background()
	q := client.QuotaMap{run.region: {
		"vcpu": num(4), "spot_vcpu": num(4), "memory_mb": num(8192), "spot_memory_mb": num(8192),
		"instances": num(3), "standard_disk_gb": num(40), "ssd_disk_gb": num(10),
	}, "global": {"subnetworks": num(2), "agent_sessions": num(1)}}
	s, err := run.api.Create(ctx, path, client.CreateRequest{Regions: []string{run.region}, Quota: q,
		ModelBudgetUSD: "1", Address: &client.AddressWant{PrefixLength: 26}, Control: control}, client.Mutation{})
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	t.Cleanup(func() { run.api.Delete(context.Background(), path, true, client.Mutation{}) })
	blk := netip.MustParsePrefix(s.Address.Blocks[0])
	info := siloInfo{path: path, id: s.ID, project: "silo-" + s.ID, block: blk, subnet: netip.PrefixFrom(blk.Addr(), 24)}
	code, op := facade(t, "POST", "projects/"+info.project+"/regions/"+run.region+"/subnetworks",
		map[string]any{"name": "net", "network": "default", "ipCidrRange": info.subnet.String()})
	if code != 200 {
		t.Fatalf("subnet in %s: %d %v", path, code, op)
	}
	waitOp(t, info.project, op)
	return info
}

// agentRef is an agent's path.
func agentRef(s siloInfo, name string) string { return s.path + "/agents/" + name }

// createAgent creates name in s, made by the root, with a tool VM, a
// lease of ttl and prompt, and deletes it at the end of the case. It
// returns the create's error, for the case to assert on.
func createAgent(t *testing.T, s siloInfo, name, ttl, prompt string) error {
	t.Helper()
	agentCredential(t)
	var req client.AgentRequest
	req.Name = name
	req.Harness.Adapter, req.Harness.Credential = "claude-code", agentCredentialName
	req.Model = agentModel
	req.Budget.USD = "0.50"
	req.ToolVM = client.ToolVM{Zone: run.zone, Subnet: "net", MachineType: agentVMType, Image: probeImage, DiskGB: 10, Spot: true}
	req.Lease = client.Lease{TTL: ttl}
	req.Prompt = prompt
	_, _, err := run.api.CreateAgent(context.Background(), s.path, req, client.Mutation{})
	if err == nil {
		t.Cleanup(func() {
			ctx := context.Background()
			if _, err := run.api.DeleteAgent(ctx, agentRef(s, name), client.Mutation{}); err != nil {
				return
			}
			// The delete is an operation: wait for the tool VM to go, so
			// that the silo's own delete finds it empty.
			for i := 0; i < 60; i++ {
				if _, err := run.api.Agent(ctx, agentRef(s, name)); apiCode(err) == "NOT_FOUND" {
					return
				}
				time.Sleep(10 * time.Second)
			}
		})
	}
	return err
}

// waitAgent polls an agent until ok says so, and returns it.
func waitAgent(t *testing.T, ref string, d time.Duration, what string, ok func(*client.Agent) bool) *client.Agent {
	t.Helper()
	deadline := time.Now().Add(d)
	var last *client.Agent
	var lastErr error
	for time.Now().Before(deadline) {
		a, err := run.api.Agent(context.Background(), ref)
		if err == nil && ok(a) {
			return a
		}
		last, lastErr = a, err
		time.Sleep(10 * time.Second)
	}
	if last != nil {
		t.Fatalf("%s: never %s: state %s (%s), tool server %s", ref, what, last.State, last.StatusMessage, last.ToolVM.ToolServer)
	}
	t.Fatalf("%s: never %s: %v", ref, what, lastErr)
	return nil
}

// agentPath is an agent's API path, under the client's endpoint.
func agentPath(t *testing.T, ref string) string {
	t.Helper()
	p, err := run.api.AgentPath(context.Background(), ref)
	if err != nil {
		t.Fatalf("%s: %v", ref, err)
	}
	return p
}

// events is an agent's whole session so far.
func events(t *testing.T, ref string) []SessionEvent {
	t.Helper()
	p := agentPath(t, ref) + "/session"
	var all []SessionEvent
	q := url.Values{}
	for {
		var out struct {
			Events []SessionEvent `json:"events"`
			Next   string         `json:"next_page_token"`
		}
		if err := run.api.Do(context.Background(), http.MethodGet, p, q, nil, &out); err != nil {
			t.Fatalf("%s's session: %v", ref, err)
		}
		all = append(all, out.Events...)
		if out.Next == "" {
			return all
		}
		q.Set("page_token", out.Next)
	}
}

// waitTool waits for the agent's session to have a finished call of
// tool after event seq after, and returns the last such call.
func waitTool(t *testing.T, ref, tool string, after int64, d time.Duration) ToolOutcome {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var since []SessionEvent
		for _, e := range events(t, ref) {
			if e.Seq > after {
				since = append(since, e)
			}
		}
		var found *ToolOutcome
		for _, o := range ToolOutcomes(since) {
			if o.Tool == tool && o.Done {
				o := o
				found = &o
			}
		}
		if found != nil {
			return *found
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("%s never finished a call of %s", ref, tool)
	return ToolOutcome{}
}

// lastSeq is the seq of the session's last event, 0 with none.
func lastSeq(t *testing.T, ref string) int64 {
	ev := events(t, ref)
	if len(ev) == 0 {
		return 0
	}
	return ev[len(ev)-1].Seq
}

// tell sends the agent a message from the root.
func tell(t *testing.T, ref, text string) {
	t.Helper()
	if err := run.api.Do(context.Background(), http.MethodPost, agentPath(t, ref)+"/messages", nil, map[string]any{"text": text}, nil); err != nil {
		t.Fatalf("a message to %s: %v", ref, err)
	}
}

// idle waits for the agent to finish its turn.
func idle(t *testing.T, ref string) *client.Agent {
	t.Helper()
	return waitAgent(t, ref, 10*time.Minute, "IDLE", func(a *client.Agent) bool { return a.State == "IDLE" })
}

const steps = "You are being run by an automated conformance test. Do exactly what this message says, in order, and nothing else. " +
	"Don't retry a tool call that fails: note the error and go on to the next step. "

// TestAgentsInASealedSilo: the root creates an agent in a sealed silo,
// with a tool VM; the agent keeps its tools, the model proxy, ask and
// messages, and loses the fence and facade, on its socket and from its
// tool VM; a manage holder with the agents scope runs its lifecycle, and
// one without it can't.
func TestAgentsInASealedSilo(t *testing.T) {
	installed(t)
	ctx := context.Background()
	s := agentSilo(t, "/as", "none")
	ref := agentRef(s, "op")
	prompt := steps + "1. Call the exec tool with the command `hostname`. " +
		"2. Call the ask tool with the question `may I continue?`, and wait for its answer. " +
		"3. Reply with the single word DONE."
	err := createAgent(t, s, "op", "2h", prompt)
	t.Run("CreateWorks", func(t *testing.T) {
		if err != nil {
			t.Fatalf("the root's create of an agent in sealed /as: %v", err)
		}
	})
	if err != nil {
		t.FailNow()
	}
	t.Run("ToolServerAttaches", func(t *testing.T) {
		a := waitAgent(t, ref, 15*time.Minute, "CONNECTED", func(a *client.Agent) bool { return a.ToolVM.ToolServer == "CONNECTED" })
		if a.ToolVM.Instance != "agent-op" {
			t.Errorf("tool VM instance %q, want agent-op", a.ToolVM.Instance)
		}
		if code, _ := facade(t, "GET", instances(s)+"/agent-op", nil); code != 200 {
			t.Errorf("the tool VM isn't an instance of /as's facade: %d", code)
		}
		// The tool server attaches on the gateway's port 80: the fence
		// drops /as's port 8080, so a CONNECTED tool server came in on 80.
	})
	t.Run("ExecRunsOnToolVM", func(t *testing.T) {
		needModel(t)
		o := waitTool(t, ref, "exec", 0, 10*time.Minute)
		if o.IsError || !strings.Contains(o.Content, "agent-op") {
			t.Errorf("exec of hostname: is_error=%v: %s", o.IsError, o.Content)
		}
	})
	t.Run("ModelProxyAnswers", func(t *testing.T) {
		needModel(t)
		if n := CountEvents(events(t, ref), "usage"); n == 0 {
			t.Error("no usage event: the model proxy metered no call")
		}
	})
	t.Run("AskWaitsForAPerson", func(t *testing.T) {
		needModel(t)
		waitAgent(t, ref, 10*time.Minute, "WAITING", func(a *client.Agent) bool { return a.State == "WAITING" })
		var reqs struct {
			Requests []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"requests"`
		}
		if err := run.api.Do(ctx, http.MethodGet, agentPath(t, ref)+"/requests", url.Values{"state": {"open"}}, nil, &reqs); err != nil {
			t.Fatal(err)
		}
		if len(reqs.Requests) != 1 || reqs.Requests[0].Kind != "question" {
			t.Fatalf("open requests: %+v, want one question", reqs.Requests)
		}
		if err := run.api.Do(ctx, http.MethodPost, agentPath(t, ref)+"/requests/"+reqs.Requests[0].ID+":answer", nil,
			map[string]any{"text": "yes, continue"}, nil); err != nil {
			t.Fatalf("answering: %v", err)
		}
		idle(t, ref)
		if CountEvents(events(t, ref), "answer") == 0 {
			t.Error("no answer event in the session")
		}
	})
	t.Run("MessageStartsATurn", func(t *testing.T) {
		needModel(t)
		idle(t, ref)
		after := lastSeq(t, ref)
		tell(t, ref, steps+"Call the exec tool with the command `echo second-turn`, then reply DONE.")
		o := waitTool(t, ref, "exec", after, 10*time.Minute)
		if !strings.Contains(o.Content, "second-turn") {
			t.Errorf("the message's exec: %s", o.Content)
		}
		var since []SessionEvent
		for _, e := range events(t, ref) {
			if e.Seq > after {
				since = append(since, e)
			}
		}
		if CountEvents(since, "turn") == 0 {
			t.Error("the message started no turn")
		}
	})
	createRefused := func(t *testing.T, after int64, child, agent string) {
		t.Helper()
		for _, tool := range []string{"silo_create_child", "agent_create"} {
			if o := waitTool(t, ref, tool, after, 10*time.Minute); !o.IsError {
				t.Errorf("%s worked from sealed /as: %s", tool, o.Content)
			}
		}
		if _, err := run.api.Describe(ctx, s.path+"/"+child, false); apiCode(err) != "NOT_FOUND" {
			t.Errorf("/as/%s exists after a refused silo_create_child: %v", child, err)
		}
		if l, err := run.api.Agents(ctx, s.path); err != nil || len(l) != 1 {
			t.Errorf("/as's agents after a refused agent_create: %d, %v", len(l), err)
		}
	}
	t.Run("HarnessFenceAndComputeRefused", func(t *testing.T) {
		needModel(t)
		idle(t, ref)
		after := lastSeq(t, ref)
		tell(t, ref, steps+fmt.Sprintf("Call each of these tools once, in this order: "+
			"1. silo_create_child, to create a child silo named x-child with a /28 address block. "+
			"2. agent_create, to create an agent named x-agent like yourself. "+
			"3. compute, to send POST projects/%[1]s/zones/%[2]s/instances with the body "+
			`{"name":"x-agent-vm","machineType":"zones/%[2]s/machineTypes/e2-micro","disks":[{"boot":true,"autoDelete":true,"initializeParams":{"sourceImage":"%[3]s"}}],"networkInterfaces":[{"subnetwork":"regions/%[4]s/subnetworks/net"}]}. `+
			"4. silo_describe, for your own silo. Then reply DONE.", s.project, run.zone, probeImage, run.region))
		createRefused(t, after, "x-child", "x-agent")
		for _, tool := range []string{"compute", "silo_describe"} {
			if o := waitTool(t, ref, tool, after, 10*time.Minute); !o.IsError {
				t.Errorf("%s worked from sealed /as: %s", tool, o.Content)
			}
		}
		if code, _ := facade(t, "GET", instances(s)+"/x-agent-vm", nil); code != 404 {
			t.Errorf("x-agent-vm exists after a refused compute insert (%d)", code)
			facade(t, "DELETE", instances(s)+"/x-agent-vm", nil)
		}
	})
	t.Run("UnderRead", func(t *testing.T) {
		needModel(t)
		idle(t, ref)
		if _, err := run.api.Patch(ctx, s.path, client.Patch{"control": "read"}, client.Mutation{}); err != nil {
			t.Fatalf("PATCH /as control read: %v", err)
		}
		defer run.api.Patch(ctx, s.path, client.Patch{"control": "none"}, client.Mutation{})
		after := lastSeq(t, ref)
		tell(t, ref, steps+fmt.Sprintf("Call each of these tools once, in this order: "+
			"1. silo_describe, for your own silo. "+
			"2. compute, to send GET projects/%s/zones/%s/instances. "+
			"3. silo_create_child, to create a child silo named x-child2 with a /28 address block. "+
			"4. agent_create, to create an agent named x-agent2 like yourself. Then reply DONE.", s.project, run.zone))
		for _, tool := range []string{"silo_describe", "compute"} {
			if o := waitTool(t, ref, tool, after, 10*time.Minute); o.IsError {
				t.Errorf("%s refused under read: %s", tool, o.Content)
			}
		}
		createRefused(t, after, "x-child2", "x-agent2")
	})
	t.Run("ExecToAPIPortTimesOut", func(t *testing.T) {
		needModel(t)
		idle(t, ref)
		after := lastSeq(t, ref)
		tell(t, ref, steps+fmt.Sprintf("Call the exec tool once, with exactly this command, and then reply DONE: "+
			"`CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE=http://silo.internal:8080/compute/v1/ timeout 60 gcloud compute instances list --project %s --zones %s; echo exit=$?`",
			s.project, run.zone))
		o := waitTool(t, ref, "exec", after, 10*time.Minute)
		if !strings.Contains(o.Content, "exit=124") {
			t.Errorf("gcloud against silo.internal:8080 from the tool VM didn't time out: %s", o.Content)
		}
	})
	t.Run("ManageAgentsScope", func(t *testing.T) {
		manageAgents(t, s, ref)
	})
}

// manageAgents: /ah holds manage on s with the agents scope, /an holds
// one without it. /ah renews and stops the agent; /an gets
// PERMISSION_DENIED for both.
func manageAgents(t *testing.T, s siloInfo, ref string) {
	ctx := context.Background()
	sp, err := run.api.SiloPath(ctx, s.path)
	if err != nil {
		t.Fatal(err)
	}
	holders := []struct {
		path, grant string
		fence       []string
	}{{"/ah", "ops", []string{"read", "agents"}}, {"/an", "viewer", []string{"read"}}}
	ids := map[string]string{}
	infos := map[string]siloInfo{}
	for _, h := range holders {
		infos[h.path] = mkSilo(t, h.path, 26, "", nil)
		out := map[string]any{}
		if err := run.api.Do(ctx, http.MethodPost, sp+"/manageGrants", nil, map[string]any{
			"name": h.grant, "holder": h.path,
			"scope": map[string]any{"fence": h.fence},
			"lease": map[string]any{"ttl": "1800s"},
		}, &out); err != nil {
			t.Fatalf("offering %s a grant on %s: %v", h.path, s.path, err)
		}
		ids[h.path], _ = out["id"].(string)
		grant := h.grant
		t.Cleanup(func() { run.api.Do(context.Background(), http.MethodDelete, sp+"/manageGrants/"+grant, nil, nil, nil) })
	}
	before, err := run.api.Agent(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	agentURL := api + "/silo/v1/" + agentPath(t, ref)
	checks := func(holder string) []Check {
		return []Check{
			{ID: "accept", Kind: "http", Method: "POST", URL: api + "/silo/v1/self/manageGrants/" + ids[holder] + ":accept"},
			{ID: "renew", Kind: "http", Method: "POST", URL: agentURL + ":renew", Body: `{"ttl":"3h"}`},
			{ID: "stop", Kind: "http", Method: "POST", URL: agentURL + ":stop", Body: `{}`},
		}
	}
	// The holder without the scope goes first, so that its stop finds
	// the agent running.
	without := probe(t, infos["/an"], "probe-an", checks("/an"))
	with := probe(t, infos["/ah"], "probe-ah", checks("/ah"))
	t.Run("WithoutAgentsScopeDenied", func(t *testing.T) {
		expectStatus(t, without, "accept", 200)
		expectStatus(t, without, "renew", 403, "PERMISSION_DENIED")
		expectStatus(t, without, "stop", 403, "PERMISSION_DENIED")
	})
	t.Run("WithAgentsScopeRenewsAndStops", func(t *testing.T) {
		expectStatus(t, with, "accept", 200)
		expectStatus(t, with, "renew", 200)
		after, err := run.api.Agent(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if !after.Lease.ExpireTime.After(before.Lease.ExpireTime) {
			t.Errorf("the renew didn't move the lease: %v, then %v", before.Lease.ExpireTime, after.Lease.ExpireTime)
		}
		if before.State == "FAILED" {
			// With no model credential the first turn's model calls fail,
			// and the agent may have failed before the stop: then the stop
			// may be refused on its state, but not on the caller's scope.
			if r := with["stop"]; HTTPStatus(r) != 200 && strings.Contains(r.Detail, "PERMISSION_DENIED") {
				t.Errorf("stop by an agents-scope holder: %s", r.Detail)
			}
			t.Logf("the agent was FAILED (%s) before the stop: the stop's state change isn't checked", before.StatusMessage)
			return
		}
		expectStatus(t, with, "stop", 200)
		if after.State != "STOPPED" {
			t.Errorf("after the holder's stop the agent is %s, want STOPPED", after.State)
		}
	})
}

// TestAgentUnsealed: an agent in an ordinary silo, from create to a turn
// that uses a tool, its budget charged per model.call, and its short
// lease running out, which deletes it and its tool VM.
func TestAgentUnsealed(t *testing.T) {
	installed(t)
	ctx := context.Background()
	s := agentSilo(t, "/au", "")
	ref := agentRef(s, "worker")
	const ttl = 15 * time.Minute
	err := createAgent(t, s, "worker", ttl.String(), steps+"Call the exec tool with the command `uname -n`, then reply DONE.")
	created := time.Now()
	t.Run("CreateWorks", func(t *testing.T) {
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		waitAgent(t, ref, 15*time.Minute, "CONNECTED", func(a *client.Agent) bool { return a.ToolVM.ToolServer == "CONNECTED" })
	})
	if err != nil {
		t.FailNow()
	}
	t.Run("TurnUsesATool", func(t *testing.T) {
		needModel(t)
		o := waitTool(t, ref, "exec", 0, 10*time.Minute)
		if o.IsError || !strings.Contains(o.Content, "agent-worker") {
			t.Errorf("exec of uname -n: is_error=%v: %s", o.IsError, o.Content)
		}
	})
	t.Run("BudgetChargedPerModelCall", func(t *testing.T) {
		needModel(t)
		a := idle(t, ref)
		recs, err := run.api.Audit(ctx, s.path, client.AuditQuery{Kind: "model.call"})
		if err != nil {
			t.Fatal(err)
		}
		usage := CountEvents(events(t, ref), "usage")
		if len(recs) == 0 || len(recs) != usage {
			t.Errorf("%d model.call audit records for %d usage events", len(recs), usage)
		}
		if a.Budget.SpentUSD == "" || strings.Trim(a.Budget.SpentUSD, "0.") == "" {
			t.Errorf("spent_usd is %q after %d model calls", a.Budget.SpentUSD, len(recs))
		}
	})
	t.Run("LeaseRunsOutDeletesToolVM", func(t *testing.T) {
		if code, _ := facade(t, "GET", instances(s)+"/agent-worker", nil); code != 200 {
			t.Fatalf("no tool VM before the lease ran out: %d", code)
		}
		time.Sleep(time.Until(created.Add(ttl)))
		deadline := time.Now().Add(10 * time.Minute)
		for {
			_, aerr := run.api.Agent(ctx, ref)
			code, _ := facade(t, "GET", instances(s)+"/agent-worker", nil)
			if apiCode(aerr) == "NOT_FOUND" && code == 404 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("10m after the lease ran out: agent %v, tool VM %d", aerr, code)
			}
			time.Sleep(15 * time.Second)
		}
	})
}
