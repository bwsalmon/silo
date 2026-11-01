package vm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
)

func TestVMServer_FileAndExecOperations(t *testing.T) {
	mockRunner := NewMockRunner()
	server := protocol.NewServer("test-vm", "1.0")
	vmServer := NewVMServer(mockRunner)
	vmServer.RegisterTools(server)

	ctx := context.Background()

	// 1. Write File
	writeArgs, _ := json.Marshal(map[string]interface{}{
		"instance_name": "sandbox-vm",
		"path":          "/etc/app.conf",
		"content":       "port=8080\nenv=production\n",
	})
	res, err := vmServer.handleVMWriteFile(ctx, writeArgs)
	if err != nil || res.IsError {
		t.Fatalf("vm_write_file failed: %v", err)
	}

	// 2. Read File
	readArgs, _ := json.Marshal(map[string]interface{}{
		"instance_name": "sandbox-vm",
		"path":          "/etc/app.conf",
	})
	res, err = vmServer.handleVMReadFile(ctx, readArgs)
	if err != nil || res.IsError {
		t.Fatalf("vm_read_file failed: %v", err)
	}
	if res.Content[0].Text != "port=8080\nenv=production\n" {
		t.Errorf("unexpected content: %s", res.Content[0].Text)
	}

	// 3. Edit File
	editArgs, _ := json.Marshal(map[string]interface{}{
		"instance_name": "sandbox-vm",
		"path":          "/etc/app.conf",
		"target":        "port=8080",
		"replacement":   "port=9090",
	})
	res, err = vmServer.handleVMEditFile(ctx, editArgs)
	if err != nil || res.IsError {
		t.Fatalf("vm_edit_file failed: %v", err)
	}

	// Verify modified content
	res, _ = vmServer.handleVMReadFile(ctx, readArgs)
	if res.Content[0].Text != "port=9090\nenv=production\n" {
		t.Errorf("expected modified port=9090, got: %s", res.Content[0].Text)
	}

	// 4. Exec Command
	mockRunner.CmdStdout = "Linux sandbox-vm 6.1"
	mockRunner.ExitCode = 0
	execArgs, _ := json.Marshal(map[string]interface{}{
		"instance_name": "sandbox-vm",
		"command":       "uname -a",
	})
	res, err = vmServer.handleVMExec(ctx, execArgs)
	if err != nil || res.IsError {
		t.Fatalf("vm_exec failed: %v", err)
	}
	if mockRunner.LastCmd != "uname -a" {
		t.Errorf("expected LastCmd 'uname -a', got %s", mockRunner.LastCmd)
	}
}
