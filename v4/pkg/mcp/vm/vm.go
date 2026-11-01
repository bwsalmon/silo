package vm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
)

// DirEntry represents an entry inside a remote VM directory.
type DirEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // "file" | "directory" | "symlink"
	SizeBytes   int64  `json:"size_bytes"`
	Permissions string `json:"permissions"`
}

// Runner defines the execution interface against remote GCE instances.
type Runner interface {
	Exec(ctx context.Context, instance, zone, user, command string, timeout time.Duration) (exitCode int, stdout, stderr string, err error)
	ReadFile(ctx context.Context, instance, zone, user, path string) ([]byte, error)
	WriteFile(ctx context.Context, instance, zone, user, path string, content []byte, overwrite bool) error
	EditFile(ctx context.Context, instance, zone, user, path, target, replacement string) (replacements int, err error)
	ListDirectory(ctx context.Context, instance, zone, user, path string) ([]DirEntry, error)
}

// MockRunner provides an in-memory virtual machine runner for tests.
type MockRunner struct {
	mu        sync.RWMutex
	files     map[string]map[string][]byte // instance -> path -> content
	LastCmd   string
	ExitCode  int
	CmdStdout string
	CmdStderr string
}

// NewMockRunner creates an in-memory runner.
func NewMockRunner() *MockRunner {
	return &MockRunner{
		files: make(map[string]map[string][]byte),
	}
}

func (m *MockRunner) Exec(ctx context.Context, instance, zone, user, command string, timeout time.Duration) (int, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastCmd = command
	return m.ExitCode, m.CmdStdout, m.CmdStderr, nil
}

func (m *MockRunner) ReadFile(ctx context.Context, instance, zone, user, path string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	vmFiles, ok := m.files[instance]
	if !ok {
		return nil, fmt.Errorf("instance %q not found", instance)
	}
	content, ok := vmFiles[path]
	if !ok {
		return nil, fmt.Errorf("file %q not found on %s", path, instance)
	}
	return append([]byte(nil), content...), nil
}

func (m *MockRunner) WriteFile(ctx context.Context, instance, zone, user, path string, content []byte, overwrite bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[instance]; !ok {
		m.files[instance] = make(map[string][]byte)
	}
	if !overwrite {
		if _, exists := m.files[instance][path]; exists {
			return errors.New("file exists and overwrite=false")
		}
	}
	m.files[instance][path] = append([]byte(nil), content...)
	return nil
}

func (m *MockRunner) EditFile(ctx context.Context, instance, zone, user, path, target, replacement string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vmFiles, ok := m.files[instance]
	if !ok {
		return 0, fmt.Errorf("instance %q not found", instance)
	}
	content, ok := vmFiles[path]
	if !ok {
		return 0, fmt.Errorf("file %q not found", path)
	}

	strContent := string(content)
	count := strings.Count(strContent, target)
	if count == 0 {
		return 0, fmt.Errorf("target string not found in %q", path)
	}

	newContent := strings.ReplaceAll(strContent, target, replacement)
	m.files[instance][path] = []byte(newContent)
	return count, nil
}

func (m *MockRunner) ListDirectory(ctx context.Context, instance, zone, user, path string) ([]DirEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	vmFiles, ok := m.files[instance]
	if !ok {
		return []DirEntry{}, nil
	}

	var entries []DirEntry
	prefix := path
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	for fPath, content := range vmFiles {
		if strings.HasPrefix(fPath, prefix) {
			rel := strings.TrimPrefix(fPath, prefix)
			if !strings.Contains(rel, "/") {
				entries = append(entries, DirEntry{
					Name:        rel,
					Type:        "file",
					SizeBytes:   int64(len(content)),
					Permissions: "-rw-r--r--",
				})
			}
		}
	}

	return entries, nil
}

// GCloudRunner executes real operations against GCE instances via gcloud compute ssh.
type GCloudRunner struct {
	Project string
}

// NewGCloudRunner creates a new GCloudRunner.
func NewGCloudRunner(project string) *GCloudRunner {
	return &GCloudRunner{Project: project}
}

func (g *GCloudRunner) buildSSHArgs(instance, zone, user string) []string {
	args := []string{"compute", "ssh"}
	target := instance
	if user != "" {
		target = fmt.Sprintf("%s@%s", user, instance)
	}
	args = append(args, target, "--tunnel-through-iap", "--quiet")
	if zone != "" {
		args = append(args, fmt.Sprintf("--zone=%s", zone))
	}
	if g.Project != "" {
		args = append(args, fmt.Sprintf("--project=%s", g.Project))
	}
	return args
}

func (g *GCloudRunner) Exec(ctx context.Context, instance, zone, user, command string, timeout time.Duration) (int, string, string, error) {
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := g.buildSSHArgs(instance, zone, user)
	args = append(args, "--command", command)

	cmd := exec.CommandContext(ctx, "gcloud", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return -1, stdout.String(), stderr.String(), err
		}
	}

	return exitCode, stdout.String(), stderr.String(), nil
}

func (g *GCloudRunner) ReadFile(ctx context.Context, instance, zone, user, path string) ([]byte, error) {
	// Base64 encode file on remote VM to ensure binary safety
	cmd := fmt.Sprintf("base64 -w 0 %s", path)
	exitCode, stdout, stderr, err := g.Exec(ctx, instance, zone, user, cmd, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("read file: %w (stderr: %s)", err, stderr)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("read file failed with exit code %d: %s", exitCode, stderr)
	}

	return base64.StdEncoding.DecodeString(strings.TrimSpace(stdout))
}

func (g *GCloudRunner) WriteFile(ctx context.Context, instance, zone, user, path string, content []byte, overwrite bool) error {
	encoded := base64.StdEncoding.EncodeToString(content)
	flags := ">"
	if !overwrite {
		flags = "set -o noclobber; cat >"
	}
	cmd := fmt.Sprintf("echo '%s' | base64 -d %s %s", encoded, flags, path)
	exitCode, _, stderr, err := g.Exec(ctx, instance, zone, user, cmd, 60*time.Second)
	if err != nil {
		return fmt.Errorf("write file: %w (stderr: %s)", err, stderr)
	}
	if exitCode != 0 {
		return fmt.Errorf("write file failed with exit code %d: %s", exitCode, stderr)
	}
	return nil
}

func (g *GCloudRunner) EditFile(ctx context.Context, instance, zone, user, path, target, replacement string) (int, error) {
	data, err := g.ReadFile(ctx, instance, zone, user, path)
	if err != nil {
		return 0, err
	}

	strData := string(data)
	count := strings.Count(strData, target)
	if count == 0 {
		return 0, fmt.Errorf("target string not found in %q", path)
	}

	modified := strings.ReplaceAll(strData, target, replacement)
	if err := g.WriteFile(ctx, instance, zone, user, path, []byte(modified), true); err != nil {
		return 0, err
	}

	return count, nil
}

func (g *GCloudRunner) ListDirectory(ctx context.Context, instance, zone, user, path string) ([]DirEntry, error) {
	cmd := fmt.Sprintf("python3 -c 'import os, json; print(json.dumps([{\"name\": f, \"type\": \"directory\" if os.path.isdir(os.path.join(\"%s\", f)) else \"file\", \"size_bytes\": os.path.getsize(os.path.join(\"%s\", f))} for f in os.listdir(\"%s\")]))'", path, path, path)
	exitCode, stdout, stderr, err := g.Exec(ctx, instance, zone, user, cmd, 60*time.Second)
	if err != nil || exitCode != 0 {
		return nil, fmt.Errorf("list dir failed: %v, stderr: %s", err, stderr)
	}

	var entries []DirEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &entries); err != nil {
		return nil, fmt.Errorf("parse dir listing: %w", err)
	}
	return entries, nil
}

// VMServer exposes remote VM tools over MCP.
type VMServer struct {
	runner Runner
}

// NewVMServer creates a new VM server instance.
func NewVMServer(runner Runner) *VMServer {
	return &VMServer{runner: runner}
}

// RegisterTools registers VM execution and file tools on the MCP server.
func (v *VMServer) RegisterTools(server *protocol.Server) {
	server.RegisterTool(
		protocol.Tool{
			Name:        "vm_exec",
			Description: "Executes a non-interactive shell command on a target GCE virtual machine.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"instance_name":   map[string]interface{}{"type": "string"},
					"command":         map[string]interface{}{"type": "string"},
					"zone":            map[string]interface{}{"type": "string"},
					"user":            map[string]interface{}{"type": "string"},
					"timeout_seconds": map[string]interface{}{"type": "integer"},
				},
				"required": []string{"instance_name", "command"},
			},
		},
		v.handleVMExec,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "vm_read_file",
			Description: "Reads textual content from a file on a target GCE virtual machine.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"instance_name": map[string]interface{}{"type": "string"},
					"path":          map[string]interface{}{"type": "string"},
					"zone":          map[string]interface{}{"type": "string"},
					"user":          map[string]interface{}{"type": "string"},
				},
				"required": []string{"instance_name", "path"},
			},
		},
		v.handleVMReadFile,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "vm_write_file",
			Description: "Writes or replaces file contents on a target GCE virtual machine.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"instance_name": map[string]interface{}{"type": "string"},
					"path":          map[string]interface{}{"type": "string"},
					"content":       map[string]interface{}{"type": "string"},
					"overwrite":     map[string]interface{}{"type": "boolean"},
					"zone":          map[string]interface{}{"type": "string"},
					"user":          map[string]interface{}{"type": "string"},
				},
				"required": []string{"instance_name", "path", "content"},
			},
		},
		v.handleVMWriteFile,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "vm_edit_file",
			Description: "Performs exact search-and-replace string editing on a file on a target GCE virtual machine.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"instance_name": map[string]interface{}{"type": "string"},
					"path":          map[string]interface{}{"type": "string"},
					"target":        map[string]interface{}{"type": "string"},
					"replacement":   map[string]interface{}{"type": "string"},
					"zone":          map[string]interface{}{"type": "string"},
					"user":          map[string]interface{}{"type": "string"},
				},
				"required": []string{"instance_name", "path", "target", "replacement"},
			},
		},
		v.handleVMEditFile,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "vm_list_directory",
			Description: "Lists files and subdirectories on a target GCE virtual machine.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"instance_name": map[string]interface{}{"type": "string"},
					"path":          map[string]interface{}{"type": "string"},
					"zone":          map[string]interface{}{"type": "string"},
					"user":          map[string]interface{}{"type": "string"},
				},
				"required": []string{"instance_name", "path"},
			},
		},
		v.handleVMListDirectory,
	)
}

func (v *VMServer) handleVMExec(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		InstanceName   string `json:"instance_name"`
		Command        string `json:"command"`
		Zone           string `json:"zone"`
		User           string `json:"user"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	timeout := time.Duration(input.TimeoutSeconds) * time.Second
	exitCode, stdout, stderr, err := v.runner.Exec(ctx, input.InstanceName, input.Zone, input.User, input.Command, timeout)

	res := map[string]interface{}{
		"exit_code": exitCode,
		"stdout":    stdout,
		"stderr":    stderr,
	}
	if err != nil {
		res["error"] = err.Error()
	}

	bytes, _ := json.Marshal(res)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(bytes)}},
		IsError: exitCode != 0 || err != nil,
	}, nil
}

func (v *VMServer) handleVMReadFile(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		InstanceName string `json:"instance_name"`
		Path         string `json:"path"`
		Zone         string `json:"zone"`
		User         string `json:"user"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	data, err := v.runner.ReadFile(ctx, input.InstanceName, input.Zone, input.User, input.Path)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}

	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(data)}},
	}, nil
}

func (v *VMServer) handleVMWriteFile(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		InstanceName string `json:"instance_name"`
		Path         string `json:"path"`
		Content      string `json:"content"`
		Overwrite    bool   `json:"overwrite"`
		Zone         string `json:"zone"`
		User         string `json:"user"`
	}
	input.Overwrite = true
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	err := v.runner.WriteFile(ctx, input.InstanceName, input.Zone, input.User, input.Path, []byte(input.Content), input.Overwrite)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}

	res, _ := json.Marshal(map[string]interface{}{
		"status":        "WRITTEN",
		"path":          input.Path,
		"bytes_written": len(input.Content),
	})
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}

func (v *VMServer) handleVMEditFile(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		InstanceName string `json:"instance_name"`
		Path         string `json:"path"`
		Target       string `json:"target"`
		Replacement  string `json:"replacement"`
		Zone         string `json:"zone"`
		User         string `json:"user"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	count, err := v.runner.EditFile(ctx, input.InstanceName, input.Zone, input.User, input.Path, input.Target, input.Replacement)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}

	res, _ := json.Marshal(map[string]interface{}{
		"status":            "MODIFIED",
		"path":              input.Path,
		"replacements_made": count,
	})
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}

func (v *VMServer) handleVMListDirectory(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		InstanceName string `json:"instance_name"`
		Path         string `json:"path"`
		Zone         string `json:"zone"`
		User         string `json:"user"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	entries, err := v.runner.ListDirectory(ctx, input.InstanceName, input.Zone, input.User, input.Path)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}

	res, _ := json.Marshal(map[string]interface{}{"entries": entries})
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}
