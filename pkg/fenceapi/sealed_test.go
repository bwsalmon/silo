package fenceapi_test

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/fenceapi"
)

// TestSealedHarnessKeepsControlSurface: an agent's harness in a silo
// whose effective control is none, its own or an ancestor's, keeps the
// control surface of its silo's agents on its session socket (gateway.md,
// "What a sealed silo keeps", SV-82; fence-api.md, "Control policy"),
// and loses every other call.
func TestSealedHarnessKeepsControlSurface(t *testing.T) {
	e := newAgentEnv(t)
	sealed := e.mk(e.root, "team", "sealed", obj{"control": "none", "model_budget_usd": "10", "quota": obj{"global": obj{"agent_sessions": 2}}})
	// inner's own policy is full; its effective one is its parent's none.
	inner := e.mk(e.root, "team/sealed", "inner", obj{"model_budget_usd": "4", "quota": obj{"global": obj{"agent_sessions": 1}}})
	if d := e.ok(e.root, "GET", "/silo/v1/silos/-"+inner, nil); d["control"].(obj)["effective"] != "none" {
		t.Fatalf("inner's control: %v", d["control"])
	}

	for _, c := range []struct{ silo, path string }{{sealed, "team/sealed"}, {inner, "team/sealed/inner"}} {
		// Its ancestor makes the agent: the sealed silo can't.
		id := e.create(e.team, c.path, "op", obj{"budget": obj{"usd": "1"}})
		harness := c.silo + "/agent:" + id

		// Status, by every name for its own silo.
		for _, p := range []string{"/silo/v1/self/agents", "/silo/v1/self/agents/op", "/silo/v1/silos/-" + c.silo + "/agents/op", "/silo/v1/silos/" + c.path + "/agents"} {
			e.ok(harness, "GET", p, nil)
		}
		// The rest of the surface (sessions, messages, requests) gets past
		// the control check to its handler, whatever that answers.
		for _, p := range []string{"/silo/v1/self/agents/op/messages", "/silo/v1/self/agents/op/sessions"} {
			if _, out := e.do(harness, "GET", p, nil); out["error"] != nil && out["error"].(obj)["code"] == string(fenceapi.ControlDenied) {
				t.Errorf("%s: %v", p, out)
			}
		}
		// Everything else is refused: describe, mutations of agents, the
		// silo's own infrastructure.
		e.fail(fenceapi.ControlDenied, harness, "GET", "/silo/v1/self", nil)
		e.fail(fenceapi.ControlDenied, harness, "POST", "/silo/v1/self/agents", agentBody("more", obj{"budget": obj{"usd": "1"}}))
		e.fail(fenceapi.ControlDenied, harness, "PATCH", "/silo/v1/self/agents/op", obj{"budget": obj{"usd": "0.5"}})
		e.fail(fenceapi.ControlDenied, harness, "POST", "/silo/v1/self/agents/op:renew", obj{"ttl": "1h"})
		e.fail(fenceapi.ControlDenied, harness, "POST", "/silo/v1/self/exports", obj{"name": "x", "target": "10.0.0.1:80", "protocol": "http", "to": []string{"/team"}})
		// The exemption is the harness's: the silo's VMs, by origin,
		// get none of it.
		e.fail(fenceapi.ControlDenied, c.silo, "GET", "/silo/v1/self/agents", nil)
	}
	// Nor does it reach another silo's agents, even one the harness's
	// silo could otherwise see.
	id := e.ok(e.root, "GET", "/silo/v1/silos/team/sealed/agents/op", nil)["id"].(string)
	e.fail(fenceapi.ControlDenied, sealed+"/agent:"+id, "GET", "/silo/v1/silos/team/sealed/inner/agents", nil)
}
