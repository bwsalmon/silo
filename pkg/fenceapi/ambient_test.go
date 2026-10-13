package fenceapi_test

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/fenceapi"
)

// Ambient credentials (local-backend.md §10, call L17): every provider
// has one, nobody adds or removes one, the model proxy carries none, and
// each turn is charged by the usage the harness reports.
func TestAmbientCredentials(t *testing.T) {
	e := newAgentEnv(t, func(o *fenceapi.Options) { o.Ambient = true })
	e.ok(e.root, "PUT", "/silo/v1/modelPrices/anthropic/claude-test", obj{"input": "1", "output": "10", "cache_write": "2", "cache_read": "0.10"})

	l := e.ok(e.root, "GET", "/silo/v1/credentials", nil)
	kinds := map[string]any{}
	for _, c := range l["credentials"].([]any) {
		kinds[c.(obj)["name"].(string)] = c.(obj)["kind"]
	}
	if kinds["ambient-anthropic"] != agents.CredentialAmbient || kinds["ambient-openai"] != agents.CredentialAmbient {
		t.Errorf("listed: %v", kinds)
	}
	if code, _ := e.do(e.root, "POST", "/silo/v1/credentials", obj{"name": "mine", "provider": "anthropic", "kind": "ambient"}); code != 400 {
		t.Errorf("added an ambient credential: %d", code)
	}
	if code, _ := e.do(e.root, "POST", "/silo/v1/credentials", obj{"name": "ambient-anthropic", "provider": "anthropic", "kind": "api_key", "secret": "k"}); code != 400 {
		t.Errorf("took an ambient credential's name: %d", code)
	}
	if _, _, err := e.api.CredentialSecret(ctx, "ambient-anthropic"); err == nil {
		t.Error("the model proxy got an ambient credential")
	}
	c, err := e.api.SessionCredential(ctx, "ambient-anthropic")
	if err != nil || c.Kind != agents.CredentialAmbient || c.Provider != "anthropic" {
		t.Fatalf("session credential: %+v %v", c, err)
	}

	id := e.create(e.root, "team", "a", obj{"harness": obj{"adapter": "claude-code", "credential": "ambient-anthropic"},
		"model": "claude-test", "budget": obj{"usd": "0.03"}})
	if err := e.api.ToolServerConnected(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.api.EndTurn(ctx, id); err != nil {
		t.Fatal(err)
	}
	turn := map[string]any{"source": "harness", "tokens": map[string]any{"input_tokens": float64(1000), "output_tokens": float64(1000),
		"cache_creation_input_tokens": float64(0), "cache_read_input_tokens": float64(100000)}}
	if err := e.api.ReportUsage(ctx, id, turn); err != nil {
		t.Fatal(err)
	}
	a := e.ok(e.root, "GET", "/silo/v1/agents/-"+id, nil)
	if spent := get(a, "budget", "spent_usd"); spent != "0.021" {
		t.Errorf("an ambient turn charged %v, want 0.021", spent)
	}

	// Without ambient harnesses there are no ambient credentials.
	plain := newAgentEnv(t)
	if code, _ := plain.do(plain.root, "POST", "/silo/v1/silos/team/agents", agentBody("b",
		obj{"harness": obj{"adapter": "claude-code", "credential": "ambient-anthropic"}})); code != 400 {
		t.Errorf("an ambient credential without ambient harnesses: %d", code)
	}
}
