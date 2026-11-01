package subagents

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/audit"
	"github.com/bwsalmon/silo/v4/pkg/auth"
	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
	"github.com/bwsalmon/silo/v4/pkg/quota"
	"github.com/bwsalmon/silo/v4/pkg/supervisor"
)

func setupTestSubagents(t *testing.T, maxSub, maxConc, maxDepth int, canSpawn bool) (*Manager, *protocol.Server, *audit.MemoryLogger, *supervisor.Supervisor) {
	t.Helper()
	tmpDir := t.TempDir()
	baseDir := tmpDir + "/agents"
	repoDir := tmpDir + "/repo"

	ledger := quota.NewLedger("root", maxSub, maxConc, maxDepth, canSpawn)
	tokenProvider := &auth.MockTokenProvider{}
	sup := supervisor.NewSupervisor(ledger, tokenProvider, baseDir, repoDir)
	memLogger := audit.NewMemoryLogger()

	mgr := NewManager("root", ledger, sup, tokenProvider, baseDir, repoDir, memLogger)
	mgr.SetCommandBuilder(func(spec supervisor.AgentSpec) []string {
		return []string{"sleep", "0.05"}
	})

	server := protocol.NewServer("subagents-mcp", "1.0.0")
	mgr.RegisterTools(server)

	return mgr, server, memLogger, sup
}

func callTool(t *testing.T, server *protocol.Server, toolName string, args interface{}) map[string]interface{} {
	t.Helper()
	argsBytes, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args failed: %v", err)
	}

	callParams := map[string]interface{}{
		"name":      toolName,
		"arguments": json.RawMessage(argsBytes),
	}
	paramsBytes, _ := json.Marshal(callParams)

	req := protocol.JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "tools/call",
		Params:  paramsBytes,
	}
	reqBytes, _ := json.Marshal(req)

	var inBuf, outBuf bytes.Buffer
	inBuf.Write(reqBytes)
	inBuf.WriteByte('\n')

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = server.Serve(ctx, &inBuf, &outBuf)

	var resp protocol.JSONRPCMessage
	if err := json.Unmarshal(outBuf.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v; raw: %s", err, outBuf.String())
	}

	var result protocol.ToolResult
	resultBytes, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("unmarshal tool result failed: %v", err)
	}

	if len(result.Content) == 0 {
		t.Fatalf("expected result content, got none")
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &parsed); err != nil {
		t.Fatalf("unmarshal tool text content failed: %v; raw: %s", err, result.Content[0].Text)
	}

	return parsed
}

func TestSubagentsMCP_SpawnAndList(t *testing.T) {
	_, server, logger, _ := setupTestSubagents(t, 5, 2, 2, true)

	// 1. Spawn subagent
	spawnRes := callTool(t, server, "spawn_subagent", map[string]interface{}{
		"name":        "worker-1",
		"description": "worker for tests",
		"prompt":      "perform audit",
		"lifecycle":   "one_shot",
		"initial_memory": map[string]interface{}{
			"foo": "bar",
		},
	})

	agentID, ok := spawnRes["agent_id"].(string)
	if !ok || agentID == "" {
		t.Fatalf("expected agent_id in spawn response, got: %v", spawnRes)
	}

	if spawnRes["name"] != "worker-1" {
		t.Errorf("expected name worker-1, got: %v", spawnRes["name"])
	}

	// Verify audit log
	if logger.CountEvent("SUBAGENT_SPAWN") != 1 {
		t.Errorf("expected SUBAGENT_SPAWN in audit log")
	}

	// 2. List subagents
	listRes := callTool(t, server, "list_subagents", map[string]interface{}{})
	subagents, ok := listRes["subagents"].([]interface{})
	if !ok || len(subagents) != 1 {
		t.Fatalf("expected 1 subagent in list, got: %v", listRes)
	}

	item := subagents[0].(map[string]interface{})
	if item["agent_id"] != agentID {
		t.Errorf("expected agent_id %s, got %v", agentID, item["agent_id"])
	}
}

func TestSubagentsMCP_QuotaEnforcement(t *testing.T) {
	_, server, logger, _ := setupTestSubagents(t, 1, 1, 2, true)

	// First spawn should succeed
	r1 := callTool(t, server, "spawn_subagent", map[string]interface{}{
		"name":   "worker-1",
		"prompt": "do work 1",
	})
	if r1["error_code"] != nil {
		t.Fatalf("first spawn failed: %v", r1)
	}

	// Second spawn should fail with QUOTA_EXCEEDED
	r2 := callTool(t, server, "spawn_subagent", map[string]interface{}{
		"name":   "worker-2",
		"prompt": "do work 2",
	})
	if r2["error_code"] != "QUOTA_EXCEEDED" {
		t.Fatalf("expected QUOTA_EXCEEDED, got: %v", r2)
	}

	if logger.CountEvent("QUOTA_EXCEEDED_REJECTED") != 1 {
		t.Errorf("expected QUOTA_EXCEEDED_REJECTED in audit log")
	}
}

func TestSubagentsMCP_DelegationPermittedOrDenied(t *testing.T) {
	// Creator without spawn permission
	_, server, _, _ := setupTestSubagents(t, 5, 2, 2, false)

	r := callTool(t, server, "spawn_subagent", map[string]interface{}{
		"name":   "worker-x",
		"prompt": "do work",
	})
	if r["error_code"] != "DELEGATION_NOT_PERMITTED" {
		t.Fatalf("expected DELEGATION_NOT_PERMITTED, got: %v", r)
	}
}

func TestSubagentsMCP_MemoryAndOutput(t *testing.T) {
	_, server, _, sup := setupTestSubagents(t, 5, 2, 2, true)

	spawnRes := callTool(t, server, "spawn_subagent", map[string]interface{}{
		"name":   "memory-worker",
		"prompt": "do work",
		"initial_memory": map[string]interface{}{
			"secret_key": "12345",
			"status":     "in_progress",
		},
	})
	agentID := spawnRes["agent_id"].(string)

	// Read specific key
	memKeyRes := callTool(t, server, "get_subagent_memory", map[string]interface{}{
		"agent_id": agentID,
		"key":      "secret_key",
	})
	if memKeyRes["value"] != "12345" {
		t.Errorf("expected secret_key value 12345, got: %v", memKeyRes["value"])
	}

	// Read all memory
	memAllRes := callTool(t, server, "get_subagent_memory", map[string]interface{}{
		"agent_id": agentID,
	})
	allMem, ok := memAllRes["memory"].(map[string]interface{})
	if !ok || len(allMem) != 2 {
		t.Errorf("expected 2 keys in memory, got: %v", memAllRes)
	}

	// Set output on supervisor and retrieve it
	_ = sup.SetOutput(agentID, "final work report completed")
	outRes := callTool(t, server, "get_subagent_output", map[string]interface{}{
		"agent_id": agentID,
	})
	if outRes["output"] != "final work report completed" {
		t.Errorf("expected output 'final work report completed', got: %v", outRes["output"])
	}
}

func TestSubagentsMCP_PersistentMessagingAndStop(t *testing.T) {
	mgr, server, logger, sup := setupTestSubagents(t, 5, 2, 2, true)
	mgr.SetCommandBuilder(func(spec supervisor.AgentSpec) []string {
		return []string{"sleep", "5"}
	})

	// Spawn persistent subagent
	spawnRes := callTool(t, server, "spawn_subagent", map[string]interface{}{
		"name":      "monitor-1",
		"prompt":    "continuous monitor",
		"lifecycle": "persistent",
	})
	agentID := spawnRes["agent_id"].(string)

	// Send message
	msgRes := callTool(t, server, "send_subagent_message", map[string]interface{}{
		"agent_id": agentID,
		"message":  "check error rates now",
	})
	if msgRes["status"] != "DELIVERED" {
		t.Errorf("expected DELIVERED status, got: %v", msgRes["status"])
	}

	if logger.CountEvent("SUBAGENT_MESSAGE") != 1 {
		t.Errorf("expected SUBAGENT_MESSAGE in audit log")
	}

	// Verify message in child inbox memory
	memRes := callTool(t, server, "get_subagent_memory", map[string]interface{}{
		"agent_id": agentID,
		"key":      "_inbox",
	})
	inbox, ok := memRes["value"].([]interface{})
	if !ok || len(inbox) != 1 {
		t.Fatalf("expected 1 item in child inbox, got: %v", memRes)
	}

	// Stop subagent
	stopRes := callTool(t, server, "stop_subagent", map[string]interface{}{
		"agent_id": agentID,
	})
	if stopRes["status"] != "TERMINATED" {
		t.Errorf("expected TERMINATED status, got: %v", stopRes["status"])
	}

	// Verify agent status is stopped
	proc, err := sup.GetAgent(agentID)
	if err != nil {
		t.Fatalf("GetAgent failed: %v", err)
	}
	if proc.Status != supervisor.StatusStopped {
		t.Errorf("expected StatusStopped, got: %v", proc.Status)
	}
}
