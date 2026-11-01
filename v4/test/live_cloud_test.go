package test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/audit"
	"github.com/bwsalmon/silo/v4/pkg/auth"
	"github.com/bwsalmon/silo/v4/pkg/mcp/memory"
	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
	"github.com/bwsalmon/silo/v4/pkg/mcp/subagents"
	"github.com/bwsalmon/silo/v4/pkg/mcp/vm"
	"github.com/bwsalmon/silo/v4/pkg/quota"
	"github.com/bwsalmon/silo/v4/pkg/supervisor"
)

func TestLive_GCEAndAgentAgainstBSalmonSandbox(t *testing.T) {
	if os.Getenv("LIVE_GCP_TEST") != "1" {
		t.Skip("Skipping live GCP test; set LIVE_GCP_TEST=1 to run against bsalmon-sandbox")
	}

	projectID := "bsalmon-sandbox"
	instanceName := "silo-test-vm"
	zone := "us-central1-a"

	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "agents")
	auditLogPath := filepath.Join(tmpDir, "audit.log")

	auditLogger, err := audit.NewFileLogger(auditLogPath)
	if err != nil {
		t.Fatalf("failed to create audit logger: %v", err)
	}
	defer auditLogger.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. Initialize VM Tools with real GCloudRunner
	runner := vm.NewGCloudRunner(projectID)
	vmServer := vm.NewVMServer(runner)

	server := protocol.NewServer("live-silo-test", "1.0.0")
	vmServer.RegisterTools(server)

	// 2. Test vm_exec (Remote execution via IAP)
	t.Run("vm_exec", func(t *testing.T) {
		handler, ok := server.GetHandler("vm_exec")
		if !ok {
			t.Fatal("vm_exec tool not registered")
		}

		args, _ := json.Marshal(map[string]interface{}{
			"instance_name": instanceName,
			"zone":          zone,
			"command":       "cat /etc/os-release",
		})

		res, err := handler(ctx, args)
		if err != nil || res.IsError {
			t.Fatalf("vm_exec failed: %v, res: %+v", err, res)
		}

		var payload map[string]interface{}
		_ = json.Unmarshal([]byte(res.Content[0].Text), &payload)
		stdout, _ := payload["stdout"].(string)

		if !strings.Contains(stdout, "Debian GNU/Linux") {
			t.Errorf("expected Debian in os-release, got: %s", stdout)
		}
		_ = auditLogger.Log("LIVE_VM_EXEC_SUCCESS", map[string]interface{}{
			"instance": instanceName,
			"stdout":   strings.TrimSpace(stdout[:min(len(stdout), 100)]),
		})
	})

	// 3. Test vm_write_file
	testFilePath := "/tmp/silo_greeting.txt"
	initialContent := "Hello from Silo v4 Project Edition!"
	t.Run("vm_write_file", func(t *testing.T) {
		handler, ok := server.GetHandler("vm_write_file")
		if !ok {
			t.Fatal("vm_write_file tool not registered")
		}

		args, _ := json.Marshal(map[string]interface{}{
			"instance_name": instanceName,
			"zone":          zone,
			"path":          testFilePath,
			"content":       initialContent,
			"overwrite":     true,
		})

		res, err := handler(ctx, args)
		if err != nil || res.IsError {
			t.Fatalf("vm_write_file failed: %v, res: %+v", err, res)
		}
	})

	// 4. Test vm_read_file
	t.Run("vm_read_file", func(t *testing.T) {
		handler, ok := server.GetHandler("vm_read_file")
		if !ok {
			t.Fatal("vm_read_file tool not registered")
		}

		args, _ := json.Marshal(map[string]interface{}{
			"instance_name": instanceName,
			"zone":          zone,
			"path":          testFilePath,
		})

		res, err := handler(ctx, args)
		if err != nil || res.IsError {
			t.Fatalf("vm_read_file failed: %v, res: %+v", err, res)
		}

		content := res.Content[0].Text
		if strings.TrimSpace(content) != initialContent {
			t.Errorf("expected content %q, got %q", initialContent, content)
		}
	})

	// 5. Test vm_edit_file
	t.Run("vm_edit_file", func(t *testing.T) {
		handler, ok := server.GetHandler("vm_edit_file")
		if !ok {
			t.Fatal("vm_edit_file tool not registered")
		}

		args, _ := json.Marshal(map[string]interface{}{
			"instance_name": instanceName,
			"zone":          zone,
			"path":          testFilePath,
			"target":        "Project Edition",
			"replacement":   "Live Cloud Verification",
		})

		res, err := handler(ctx, args)
		if err != nil || res.IsError {
			t.Fatalf("vm_edit_file failed: %v, res: %+v", err, res)
		}

		var payload map[string]interface{}
		_ = json.Unmarshal([]byte(res.Content[0].Text), &payload)
		replacements := int(payload["replacements_made"].(float64))
		if replacements != 1 {
			t.Errorf("expected 1 replacement, got %d", replacements)
		}

		// Read back to confirm edit
		readHandler, _ := server.GetHandler("vm_read_file")
		readArgs, _ := json.Marshal(map[string]interface{}{
			"instance_name": instanceName,
			"zone":          zone,
			"path":          testFilePath,
		})
		readRes, _ := readHandler(ctx, readArgs)
		expectedUpdated := "Hello from Silo v4 Live Cloud Verification!"
		if strings.TrimSpace(readRes.Content[0].Text) != expectedUpdated {
			t.Errorf("expected updated content %q, got %q", expectedUpdated, readRes.Content[0].Text)
		}
	})

	// 6. Test vm_list_directory
	t.Run("vm_list_directory", func(t *testing.T) {
		handler, ok := server.GetHandler("vm_list_directory")
		if !ok {
			t.Fatal("vm_list_directory tool not registered")
		}

		args, _ := json.Marshal(map[string]interface{}{
			"instance_name": instanceName,
			"zone":          zone,
			"path":          "/tmp",
		})

		res, err := handler(ctx, args)
		if err != nil || res.IsError {
			t.Fatalf("vm_list_directory failed: %v, res: %+v", err, res)
		}

		var payload map[string]interface{}
		_ = json.Unmarshal([]byte(res.Content[0].Text), &payload)
		entries := payload["entries"].([]interface{})

		found := false
		for _, e := range entries {
			entryMap := e.(map[string]interface{})
			if entryMap["name"] == "silo_greeting.txt" {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("expected silo_greeting.txt in /tmp listing, but not found in %v", entries)
		}
	})

	// 7. Test memory-mcp and subagents-mcp live integration
	t.Run("agent_memory_and_subagents", func(t *testing.T) {
		memStore, err := memory.NewMemoryStore("root", baseDir, "")
		if err != nil {
			t.Fatalf("failed to create memory store: %v", err)
		}
		memStore.RegisterTools(server)

		ledger := quota.NewLedger("root", 5, 2, 2, true)
		sup := supervisor.NewSupervisor(ledger, &auth.MockTokenProvider{}, baseDir, "")
		subMgr := subagents.NewManager("root", ledger, sup, &auth.MockTokenProvider{}, baseDir, "", auditLogger)
		subMgr.RegisterTools(server)

		// Record VM check result in memory
		writeMemHandler, _ := server.GetHandler("write_memory")
		memArgs, _ := json.Marshal(map[string]interface{}{
			"key": "vm_status",
			"content": map[string]interface{}{
				"instance": instanceName,
				"zone":     zone,
				"status":   "VERIFIED",
			},
		})
		_, err = writeMemHandler(ctx, memArgs)
		if err != nil {
			t.Fatalf("write_memory failed: %v", err)
		}

		// Spawn a subagent to report status
		spawnHandler, _ := server.GetHandler("spawn_subagent")
		spawnArgs, _ := json.Marshal(map[string]interface{}{
			"name":   "cloud-reporter",
			"prompt": "report verified VM status",
			"initial_memory": map[string]interface{}{
				"target_instance": instanceName,
			},
		})
		spawnRes, err := spawnHandler(ctx, spawnArgs)
		if err != nil || spawnRes.IsError {
			t.Fatalf("spawn_subagent failed: %v, res: %+v", err, spawnRes)
		}

		fmt.Printf("Successfully verified all VM and subagent tools against live project %s!\n", projectID)
	})
}
