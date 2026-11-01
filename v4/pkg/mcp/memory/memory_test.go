package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
)

func TestMemoryStore_BasicOperations(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewMemoryStore("agent-01", tmpDir, "")
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}

	server := protocol.NewServer("test", "1.0")
	store.RegisterTools(server)

	ctx := context.Background()

	// 1. Write memory
	writeArgs, _ := json.Marshal(map[string]interface{}{
		"key":     "cluster_info",
		"content": map[string]string{"name": "prod-k8s", "zone": "us-central1-a"},
	})
	res, err := store.handleWriteMemory(ctx, writeArgs)
	if err != nil || res.IsError {
		t.Fatalf("write memory failed: %v", err)
	}

	// 2. Read memory
	readArgs, _ := json.Marshal(map[string]interface{}{
		"key": "cluster_info",
	})
	res, err = store.handleReadMemory(ctx, readArgs)
	if err != nil || res.IsError {
		t.Fatalf("read memory failed: %v", err)
	}
	var entry MemoryEntry
	if err := json.Unmarshal([]byte(res.Content[0].Text), &entry); err != nil {
		t.Fatalf("unmarshal read memory: %v", err)
	}
	valMap := entry.Value.(map[string]interface{})
	if valMap["name"] != "prod-k8s" {
		t.Errorf("expected cluster name prod-k8s, got %v", valMap["name"])
	}

	// 3. Append memory
	appendArgs, _ := json.Marshal(map[string]interface{}{
		"key":     "task_log",
		"content": "Step 1 complete",
	})
	_, err = store.handleAppendMemory(ctx, appendArgs)
	if err != nil {
		t.Fatalf("append memory failed: %v", err)
	}

	appendArgs2, _ := json.Marshal(map[string]interface{}{
		"key":     "task_log",
		"content": "Step 2 complete",
	})
	_, err = store.handleAppendMemory(ctx, appendArgs2)
	if err != nil {
		t.Fatalf("append memory 2 failed: %v", err)
	}

	// 4. Persistence across store reload (simulating agent restart)
	reloadedStore, err := NewMemoryStore("agent-01", tmpDir, "")
	if err != nil {
		t.Fatalf("failed to reload store: %v", err)
	}
	res, err = reloadedStore.handleReadMemory(ctx, readArgs)
	if err != nil || res.IsError {
		t.Fatalf("read from reloaded store failed: %v", err)
	}
}

func TestMemoryStore_RepoWorkspaceAndPathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")
	os.MkdirAll(repoDir, 0755)

	// Create test repo files
	os.WriteFile(filepath.Join(repoDir, "main.tf"), []byte("resource \"google_compute_instance\" \"vm\" {}\n"), 0644)
	subDir := filepath.Join(repoDir, "modules")
	os.MkdirAll(subDir, 0755)
	os.WriteFile(filepath.Join(subDir, "vpc.tf"), []byte("// VPC network definition\n"), 0644)

	store, err := NewMemoryStore("agent-02", tmpDir, repoDir)
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}

	ctx := context.Background()

	// 1. Read repo file
	readArgs, _ := json.Marshal(map[string]interface{}{
		"path": "main.tf",
	})
	res, err := store.handleReadRepoFile(ctx, readArgs)
	if err != nil || res.IsError {
		t.Fatalf("read repo file failed: %v", err)
	}
	if res.Content[0].Text != "resource \"google_compute_instance\" \"vm\" {}\n" {
		t.Errorf("unexpected file content: %s", res.Content[0].Text)
	}

	// 2. Search repo
	searchArgs, _ := json.Marshal(map[string]interface{}{
		"query": "google_compute_instance",
	})
	res, err = store.handleSearchRepo(ctx, searchArgs)
	if err != nil || res.IsError {
		t.Fatalf("search repo failed: %v", err)
	}
	var searchRes struct {
		Matches []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
		} `json:"matches"`
	}
	json.Unmarshal([]byte(res.Content[0].Text), &searchRes)
	if len(searchRes.Matches) != 1 || searchRes.Matches[0].Path != "main.tf" {
		t.Errorf("expected 1 match in main.tf, got %v", searchRes.Matches)
	}

	// 3. Path traversal security check
	traversalArgs, _ := json.Marshal(map[string]interface{}{
		"path": "../../etc/shadow",
	})
	res, _ = store.handleReadRepoFile(ctx, traversalArgs)
	if !res.IsError {
		t.Fatalf("expected path traversal to be rejected with error, got success: %v", res)
	}
}
