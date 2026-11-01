package test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/audit"
	"github.com/bwsalmon/silo/v4/pkg/auth"
	"github.com/bwsalmon/silo/v4/pkg/config"
	"github.com/bwsalmon/silo/v4/pkg/mcp/memory"
	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
	"github.com/bwsalmon/silo/v4/pkg/mcp/subagents"
	"github.com/bwsalmon/silo/v4/pkg/mcp/vm"
	"github.com/bwsalmon/silo/v4/pkg/quota"
	"github.com/bwsalmon/silo/v4/pkg/supervisor"
)

func TestIntegration_MultiAgentOrchestration(t *testing.T) {
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "silo")
	agentsDir := filepath.Join(baseDir, "agents")
	repoDir := filepath.Join(tmpDir, "repo")

	// 1. Setup mock read-only repository
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("failed to create mock repo dir: %v", err)
	}
	repoFile := filepath.Join(repoDir, "infra.tf")
	if err := os.WriteFile(repoFile, []byte("resource \"google_compute_network\" \"vpc\" { name = \"main\" }"), 0644); err != nil {
		t.Fatalf("failed to write mock repo file: %v", err)
	}

	// 2. Setup Config
	maxSub := 5
	maxConc := 3
	cfg := &config.Config{
		Version: "v4",
		Mode:    "project",
		Project: config.ProjectConfig{
			ID:                   "prod-test-project",
			ServiceAccount:       "root-sa@prod-test-project.iam.gserviceaccount.com",
			DownscopeCredentials: true,
		},
		Repo: config.RepoConfig{
			URL: repoDir,
		},
		Agent: config.AgentConfig{
			Prompt: "You are the root cloud orchestrator.",
			Subagents: config.SubagentConfig{
				Enabled:         true,
				MaxSubagents:    &maxSub,
				MaxConcurrent:   &maxConc,
				MaxDepth:        2,
				AllowDelegation: true,
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config validation failed: %v", err)
	}

	// 3. Setup Audit Logging, Auth, Quota, Supervisor
	memLogger := audit.NewMemoryLogger()
	tokenProvider := &auth.MockTokenProvider{}
	ledger := quota.NewLedger("root", *cfg.Agent.Subagents.MaxSubagents, *cfg.Agent.Subagents.MaxConcurrent, cfg.Agent.Subagents.MaxDepth, true)
	sup := supervisor.NewSupervisor(ledger, tokenProvider, agentsDir, repoDir)

	// 4. Setup Root MCP Server & Managers
	rootSubMgr := subagents.NewManager("root", ledger, sup, tokenProvider, agentsDir, repoDir, memLogger)
	rootSubMgr.SetCommandBuilder(func(spec supervisor.AgentSpec) []string {
		return []string{"sleep", "5"}
	})

	rootServer := protocol.NewServer("root-mcp", "1.0.0")
	rootSubMgr.RegisterTools(rootServer)

	// Root memory
	rootMem, err := memory.NewMemoryStore("root", agentsDir, repoDir)
	if err != nil {
		t.Fatalf("failed to create root memory: %v", err)
	}
	rootMem.RegisterTools(rootServer)

	// Root VM Server
	mockRunner := vm.NewMockRunner()
	mockRunner.CmdStdout = "Linux prod-gce-worker 6.1.0"
	vmServer := vm.NewVMServer(mockRunner)
	vmServer.RegisterTools(rootServer)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- STEP 1: Spawn Worker 1 with shared repo and initial memory ---
	spawnArgs1, _ := json.Marshal(map[string]interface{}{
		"name":           "repo-auditor",
		"description":    "inspects repo and reports findings",
		"prompt":         "read infra.tf and report network names",
		"share_repo":     true,
		"allow_vm_tools": true,
		"initial_memory": map[string]interface{}{
			"scan_target": "infra.tf",
		},
	})

	handler, exists := rootServer.GetHandler("spawn_subagent")
	if !exists {
		t.Fatalf("spawn_subagent tool not registered")
	}

	res, err := handler(ctx, spawnArgs1)
	if err != nil || res.IsError {
		t.Fatalf("spawn_subagent failed: %v; res: %+v", err, res)
	}

	var spawnResp1 map[string]interface{}
	_ = json.Unmarshal([]byte(res.Content[0].Text), &spawnResp1)
	worker1ID := spawnResp1["agent_id"].(string)

	if memLogger.CountEvent("SUBAGENT_SPAWN") != 1 {
		t.Errorf("expected 1 SUBAGENT_SPAWN in audit log, got %d", memLogger.CountEvent("SUBAGENT_SPAWN"))
	}

	// --- STEP 2: Worker 1 operates within its sandboxed memory and repo ---
	worker1Mem, err := memory.NewMemoryStore(worker1ID, agentsDir, repoDir)
	if err != nil {
		t.Fatalf("failed to access worker 1 memory: %v", err)
	}
	worker1Server := protocol.NewServer("worker-1-mcp", "1.0.0")
	worker1Mem.RegisterTools(worker1Server)

	// Worker 1 reads repo file
	readRepoHandler, _ := worker1Server.GetHandler("read_repo_file")
	readArgs, _ := json.Marshal(map[string]interface{}{"path": "infra.tf"})
	readRes, err := readRepoHandler(ctx, readArgs)
	if err != nil || readRes.IsError {
		t.Fatalf("read_repo_file failed: %v", err)
	}
	if readRes.Content[0].Text != "resource \"google_compute_network\" \"vpc\" { name = \"main\" }" {
		t.Errorf("unexpected repo file content: %v", readRes.Content[0].Text)
	}

	// Worker 1 writes findings to its memory
	writeMemHandler, _ := worker1Server.GetHandler("write_memory")
	writeArgs, _ := json.Marshal(map[string]interface{}{
		"key": "audit_findings",
		"content": map[string]interface{}{
			"status":   "COMPLIANT",
			"networks": []string{"main"},
		},
	})
	writeRes, err := writeMemHandler(ctx, writeArgs)
	if err != nil || writeRes.IsError {
		t.Fatalf("write_memory failed: %v", err)
	}

	// --- STEP 3: Parent reads child findings via get_subagent_memory ---
	getMemHandler, _ := rootServer.GetHandler("get_subagent_memory")
	getMemArgs, _ := json.Marshal(map[string]interface{}{
		"agent_id": worker1ID,
		"key":      "audit_findings",
	})
	getMemRes, err := getMemHandler(ctx, getMemArgs)
	if err != nil || getMemRes.IsError {
		t.Fatalf("get_subagent_memory failed: %v", err)
	}
	var parentSeenMem map[string]interface{}
	_ = json.Unmarshal([]byte(getMemRes.Content[0].Text), &parentSeenMem)
	findings := parentSeenMem["value"].(map[string]interface{})
	if findings["status"] != "COMPLIANT" {
		t.Errorf("expected status COMPLIANT, got %v", findings["status"])
	}

	// --- STEP 4: Spawn Delegated Manager with restricted quota ---
	delMaxSub := 2
	delMaxConc := 1
	delMaxDepth := 1
	spawnArgs2, _ := json.Marshal(map[string]interface{}{
		"name":      "sub-manager",
		"prompt":    "orchestrate regional audit",
		"lifecycle": "persistent",
		"delegation": map[string]interface{}{
			"can_spawn_subagents": true,
			"max_subagents":      delMaxSub,
			"max_concurrent":     delMaxConc,
			"max_depth":          delMaxDepth,
		},
	})
	res2, err := handler(ctx, spawnArgs2)
	if err != nil || res2.IsError {
		t.Fatalf("spawn manager failed: %v", err)
	}
	var spawnResp2 map[string]interface{}
	_ = json.Unmarshal([]byte(res2.Content[0].Text), &spawnResp2)
	managerID := spawnResp2["agent_id"].(string)

	// Manager MCP Manager
	mgrSubMgr := subagents.NewManager(managerID, ledger, sup, tokenProvider, agentsDir, "", memLogger)
	mgrSubMgr.SetCommandBuilder(func(spec supervisor.AgentSpec) []string {
		return []string{"sleep", "5"}
	})
	mgrServer := protocol.NewServer("mgr-mcp", "1.0.0")
	mgrSubMgr.RegisterTools(mgrServer)

	// Manager spawns Leaf 1
	mgrSpawnHandler, _ := mgrServer.GetHandler("spawn_subagent")
	leafSpawnArgs, _ := json.Marshal(map[string]interface{}{
		"name":   "leaf-worker-1",
		"prompt": "run regional checks",
	})
	leafRes, err := mgrSpawnHandler(ctx, leafSpawnArgs)
	if err != nil || leafRes.IsError {
		t.Fatalf("leaf spawn failed: %v; res: %+v", err, leafRes)
	}

	// Manager attempts to spawn with depth beyond its allowance
	invalidSpawnArgs, _ := json.Marshal(map[string]interface{}{
		"name":   "leaf-worker-invalid",
		"prompt": "run regional checks",
		"delegation": map[string]interface{}{
			"can_spawn_subagents": true,
		},
	})
	invalidRes, err := mgrSpawnHandler(ctx, invalidSpawnArgs)
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !invalidRes.IsError {
		t.Errorf("expected error for leaf attempting to delegate beyond depth 0")
	}

	// --- STEP 5: Cascading Teardown from Root ---
	stopHandler, _ := rootServer.GetHandler("stop_subagent")
	stopArgs, _ := json.Marshal(map[string]interface{}{"agent_id": managerID})
	stopRes, err := stopHandler(ctx, stopArgs)
	if err != nil || stopRes.IsError {
		t.Fatalf("stop_subagent failed: %v", err)
	}

	var stopData map[string]interface{}
	_ = json.Unmarshal([]byte(stopRes.Content[0].Text), &stopData)
	reapedCount := int(stopData["reaped_children_count"].(float64))
	if reapedCount != 1 {
		t.Errorf("expected 1 reaped grandchild, got %d", reapedCount)
	}

	// Verify manager status
	mgrProc, err := sup.GetAgent(managerID)
	if err != nil || mgrProc.Status != supervisor.StatusStopped {
		t.Errorf("expected manager to be STOPPED, got %v", mgrProc.Status)
	}

	// Verify VM tools invocation on root
	vmExecHandler, _ := rootServer.GetHandler("vm_exec")
	vmArgs, _ := json.Marshal(map[string]interface{}{
		"instance_name": "worker-vm-1",
		"command":       "uname -a",
	})
	vmRes, err := vmExecHandler(ctx, vmArgs)
	if err != nil || vmRes.IsError {
		t.Fatalf("vm_exec failed: %v", err)
	}
	var vmOutput map[string]interface{}
	_ = json.Unmarshal([]byte(vmRes.Content[0].Text), &vmOutput)
	if vmOutput["stdout"] != "Linux prod-gce-worker 6.1.0" {
		t.Errorf("unexpected vm stdout: %v", vmOutput["stdout"])
	}

	sup.Shutdown()
}
