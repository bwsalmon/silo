package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bwsalmon/silo/pkg/silo/client"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// toolVMs makes every tool VM at once, at one address.
type toolVMs struct{}

func (toolVMs) Create(context.Context, fenceapi.Caller, *tree.Silo, string, string, fenceapi.ToolVMSpec, string) (string, error) {
	return "10.1.0.2", nil
}

func (toolVMs) Delete(context.Context, fenceapi.Caller, *tree.Silo, string, string, string) error {
	return nil
}

func TestAgentCommands(t *testing.T) {
	d := startSilod(t)
	d.ok(d.root, "create", "/team", "--address", "/20", "--model-budget", "50", "--quota", "global:agent_sessions=2")
	out := d.ok(d.root, "agent", "create", "/team/agents/fixer", "--credential", "anthropic-api", "--model", "claude-opus-5-5",
		"--budget", "20", "--tool-zone", "us-central1-a", "--tool-subnet", "work", "--ttl", "4h", "--prompt", "hello", "--labels", "task=1", "--wait")
	if !strings.Contains(out, "created /team/agents/fixer") {
		t.Errorf("create: %s", out)
	}
	var l struct{ Agents []client.Agent }
	d.json(d.root, &l, "agent", "list", "/team")
	if len(l.Agents) != 1 || l.Agents[0].State != "RUNNING" || l.Agents[0].ToolVM.IP != "10.1.0.2" {
		t.Fatalf("list: %+v", l)
	}
	id := l.Agents[0].ID
	if out := d.ok(d.root, "agent", "get", id); !strings.Contains(out, "/team/agents/fixer") || !strings.Contains(out, "$20.00 per period") {
		t.Errorf("get: %s", out)
	}
	var a client.Agent
	d.json(d.root, &a, "agent", "patch", "/team/agents/fixer", "--budget", "25")
	if a.Budget.USD != "25.00" {
		t.Errorf("patch: %+v", a.Budget)
	}
	if out := d.ok(d.root, "agent", "renew", "/team/agents/fixer", "--ttl", "8h"); !strings.Contains(out, "leased until") {
		t.Errorf("renew: %s", out)
	}
	if out := d.ok(d.root, "agent", "stop", "/team/agents/fixer"); !strings.Contains(out, "STOPPED") {
		t.Errorf("stop: %s", out)
	}
	if out := d.ok(d.root, "agent", "resume", id); !strings.Contains(out, "IDLE") {
		t.Errorf("resume: %s", out)
	}
	if r := d.silo(d.root, "agent", "create", "/team/agents/x", "--credential", "anthropic-api", "--model", "nope",
		"--budget", "1", "--tool-zone", "us-central1-a", "--tool-subnet", "work"); r.code != 1 || !strings.Contains(r.stderr, "INVALID_ARGUMENT") {
		t.Errorf("unpriced: %+v", r)
	}
	if out := d.ok(d.root, "agent", "delete", "/team/agents/fixer"); !strings.Contains(out, "deleted /team/agents/fixer") {
		t.Errorf("delete: %s", out)
	}
	if out := d.ok(d.root, "model-price", "list"); !strings.Contains(out, "claude-opus-5-5") {
		t.Errorf("model-price list: %s", out)
	}
	d.ok(d.root, "model-price", "set", "anthropic/claude-next", "--input-usd", "1", "--output-usd", "2", "--cache-write-usd", "1.25", "--cache-read-usd", "0.1")
	if out := d.ok(d.root, "model-price", "list"); !strings.Contains(out, "claude-next") || !strings.Contains(out, "override") {
		t.Errorf("after set: %s", out)
	}
	d.ok(d.root, "model-price", "set", "anthropic/claude-next", "--delete")
}
