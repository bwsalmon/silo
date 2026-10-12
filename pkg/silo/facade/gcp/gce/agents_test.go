package gce

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ledger"
	"github.com/bwsalmon/silo/pkg/silo/store"
)

// TestAgentToolVM makes an agent's tool VM through the facade, as the
// fence API does in silod: the facade's checks and charges apply, the
// instance carries the hidden silo-agent label and silod's startup
// script, and the tenant's own writes to it are refused, naming the
// agent (agents.md, "Creation", SV-104; call 40).
func TestAgentToolVM(t *testing.T) {
	e := newComputeEnv(t)
	if err := e.db.Tx(ctx, func(tx store.Tx) error {
		if err := ledger.SetCapacity(ctx, tx, e.root, []ledger.Amount{
			{Key: ledger.ModelUSD, Amount: ledger.Units(100)}, {Key: fenceapi.AgentSessions, Amount: ledger.Units(5)}}); err != nil {
			return err
		}
		return ledger.Grant(ctx, tx, e.a, []ledger.Amount{
			{Key: ledger.ModelUSD, Amount: ledger.Units(50)}, {Key: fenceapi.AgentSessions, Amount: ledger.Units(3)}})
	}); err != nil {
		t.Fatal(err)
	}
	runner := &agents.Fake{}
	api := fenceapi.New(e.db, fenceapi.Options{
		Caller: func(r *http.Request) (fenceapi.Caller, error) {
			return fenceapi.Caller{Silo: r.Header.Get("Silo-Caller"), Via: fenceapi.ViaInternal}, nil
		},
		ToolVMs: e.facade.ToolVMs(), Runner: runner, Credentials: agents.StaticCredentials{"anthropic-api": "anthropic"},
	})
	runner.Connected = api.ToolServerConnected
	srv := httptest.NewServer(api)
	t.Cleanup(func() { srv.Close(); api.Close() })
	fence := func(caller, method, path string, body any) (int, obj) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Silo-Caller", caller)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		out := obj{}
		json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	agent := func(name, machine string) obj {
		return obj{"name": name, "harness": obj{"adapter": "claude-code", "credential": "anthropic-api"},
			"model": "claude-sonnet-5-5", "budget": obj{"usd": "10"}, "lease": obj{"ttl": "2h"},
			"tool_vm": obj{"zone": zoneA, "subnet": e.a + "-web", "machine_type": machine, "image": debian, "disk_gb": 30}}
	}

	// A tool VM past the silo's quota fails the create, and holds
	// nothing after.
	code, out := fence(e.a, "POST", "/silo/v1/self/agents", agent("big", "n2-standard-64"))
	if code != 403 || out["error"].(obj)["code"] != "QUOTA_EXCEEDED" {
		t.Fatalf("too big: %d %v", code, out)
	}
	e.want("refused", e.a, map[string]int64{"vcpu": 0, "instances": 0})
	// A subnet that isn't the silo's.
	if code, out := fence(e.a, "POST", "/silo/v1/self/agents", obj{"name": "x", "harness": obj{"adapter": "claude-code", "credential": "anthropic-api"},
		"model": "claude-sonnet-5-5", "budget": obj{"usd": "1"}, "tool_vm": obj{"zone": zoneA, "subnet": "nope"}}); code != 400 {
		t.Fatalf("bad subnet: %d %v", code, out)
	}

	code, op := fence(e.a, "POST", "/silo/v1/self/agents", agent("fixer", "e2-standard-4"))
	if code != 200 {
		t.Fatalf("create: %d %v", code, op)
	}
	e.run()
	e.want("created", e.a, map[string]int64{"vcpu": 4, "instances": 1})
	_, a := fence(e.a, "GET", "/silo/v1/self/agents/fixer", nil)
	id := a["id"].(string)
	real, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-agent-fixer")
	if err != nil {
		t.Fatal(err)
	}
	if real.Labels[agents.LabelAgent] != id || real.Labels["silo"] != e.a {
		t.Errorf("real labels: %v", real.Labels)
	}
	script := ""
	for _, it := range real.Metadata.Items {
		if it.Key == agents.StartupScriptKey && it.Value != nil {
			script = *it.Value
		}
	}
	if !strings.Contains(script, agents.ToolsBinaryURL) {
		t.Errorf("startup-script: %q", script)
	}
	if ip := a["tool_vm"].(obj)["ip"]; ip == "" || !e.webA.Contains(mustAddr(t, ip.(string))) {
		t.Errorf("tool VM ip %v, want one in %s", ip, e.webA)
	}
	toolChannelBinding(t, e, api, fence, id)
	// The tenant sees it, without the label.
	got := e.ok(e.a, "GET", cv(host, instancesPath+"/"+e.a+"-agent-fixer"), nil)
	if labels, _ := got["labels"].(obj); labels[agents.LabelAgent] != nil {
		t.Errorf("the tenant sees the hidden label: %v", labels)
	}
	// ... and may not write to it.
	for _, w := range []struct{ method, path string }{
		{"DELETE", instancesPath + "/" + e.a + "-agent-fixer"},
		{"POST", instancesPath + "/" + e.a + "-agent-fixer/stop"},
		{"POST", instancesPath + "/" + e.a + "-agent-fixer/setMetadata"},
		{"POST", instancesPath + "/" + e.a + "-agent-fixer/setMachineType"},
	} {
		msg := e.fail(403, ReasonForbidden, e.a, w.method, cv(host, w.path), obj{})
		if !strings.Contains(msg, "/team-a/agents/fixer") {
			t.Errorf("%s %s: %q doesn't name the agent", w.method, w.path, msg)
		}
	}
	// Nor may its ancestor, through the facade.
	e.fail(403, ReasonForbidden, e.root, "DELETE", cv("silo-"+e.a, instancesPath+"/"+e.a+"-agent-fixer"), nil)
	// Deleting the agent deletes it.
	_, op = fence(e.a, "DELETE", "/silo/v1/self/agents/fixer", nil)
	for i := 0; i < 500; i++ {
		e.gce.RunOperations()
		if c, _ := fence(e.a, "GET", "/silo/v1/self/agents/fixer", nil); c == 404 {
			break
		}
	}
	e.run()
	if _, err := e.gce.GetInstance(ctx, host, zoneA, e.a+"-agent-fixer"); err == nil {
		t.Errorf("the tool VM is still there")
	}
	e.want("deleted", e.a, map[string]int64{"vcpu": 0, "instances": 0})
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("address %q: %v", s, err)
	}
	return a
}
