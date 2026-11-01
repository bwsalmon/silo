package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
)

var (
	ErrPathTraversal = errors.New("access denied: path traversal outside repository root")
	ErrKeyRequired   = errors.New("key is required")
)

// MemoryEntry represents a stored key-value memory item.
type MemoryEntry struct {
	Key       string      `json:"key"`
	Value     interface{} `json:"value"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// MemoryStore manages memory persistence for a single agent.
type MemoryStore struct {
	agentID  string
	baseDir  string
	repoDir  string
	filePath string
	mu       sync.RWMutex
	data     map[string]MemoryEntry
}

// NewMemoryStore initializes a memory store for an agent.
func NewMemoryStore(agentID, baseDir, repoDir string) (*MemoryStore, error) {
	agentDir := filepath.Join(baseDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("create agent memory dir: %w", err)
	}

	store := &MemoryStore{
		agentID:  agentID,
		baseDir:  agentDir,
		repoDir:  repoDir,
		filePath: filepath.Join(agentDir, "memory.json"),
		data:     make(map[string]MemoryEntry),
	}

	// Load existing state if present (crash recovery / restart)
	if err := store.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load memory store: %w", err)
	}

	return store, nil
}

func (m *MemoryStore) load() error {
	bytes, err := os.ReadFile(m.filePath)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, &m.data)
}

func (m *MemoryStore) save() error {
	bytes, err := json.MarshalIndent(m.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.filePath, bytes, 0644)
}

// RegisterTools registers all memory and repo tools onto the MCP server.
func (m *MemoryStore) RegisterTools(server *protocol.Server) {
	server.RegisterTool(
		protocol.Tool{
			Name:        "read_memory",
			Description: "Reads stored working memory by key, or returns the entire memory index if key is empty.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"key": map[string]interface{}{"type": "string"},
				},
			},
		},
		m.handleReadMemory,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "write_memory",
			Description: "Creates or updates an entry in the agent's editable memory store.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"key":     map[string]interface{}{"type": "string"},
					"content": map[string]interface{}{"type": "object"},
				},
				"required": []string{"key", "content"},
			},
		},
		m.handleWriteMemory,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "append_memory",
			Description: "Appends an entry to a list or appends text to an ongoing log under the specified memory key.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"key":     map[string]interface{}{"type": "string"},
					"content": map[string]interface{}{},
				},
				"required": []string{"key", "content"},
			},
		},
		m.handleAppendMemory,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "list_memory",
			Description: "Lists all memory keys currently stored in the agent's memory area.",
			InputSchema: map[string]interface{}{
				"type": "object",
			},
		},
		m.handleListMemory,
	)

	// If repository workspace is mounted, register read-only repo tools
	if m.repoDir != "" {
		server.RegisterTool(
			protocol.Tool{
				Name:        "read_repo_file",
				Description: "Reads textual or code contents of a file in the read-only mounted repository workspace.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
			m.handleReadRepoFile,
		)

		server.RegisterTool(
			protocol.Tool{
				Name:        "list_repo_files",
				Description: "Lists files and directories within a given repository directory path.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"directory": map[string]interface{}{"type": "string"},
					},
				},
			},
			m.handleListRepoFiles,
		)

		server.RegisterTool(
			protocol.Tool{
				Name:        "search_repo",
				Description: "Performs a regex or substring search across files in the mounted repository workspace.",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{"type": "string"},
					},
					"required": []string{"query"},
				},
			},
			m.handleSearchRepo,
		)
	}
}

func (m *MemoryStore) handleReadMemory(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(args, &input)

	m.mu.RLock()
	defer m.mu.RUnlock()

	if input.Key == "" {
		keys := make([]string, 0, len(m.data))
		for k := range m.data {
			keys = append(keys, k)
		}
		res, _ := json.Marshal(map[string]interface{}{"keys": keys})
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
		}, nil
	}

	entry, exists := m.data[input.Key]
	if !exists {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: fmt.Sprintf("Memory key %q not found", input.Key)}},
			IsError: true,
		}, nil
	}

	res, _ := json.Marshal(entry)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}

func (m *MemoryStore) handleWriteMemory(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		Key     string      `json:"key"`
		Content interface{} `json:"content"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if input.Key == "" {
		return nil, ErrKeyRequired
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.data[input.Key] = MemoryEntry{
		Key:       input.Key,
		Value:     input.Content,
		UpdatedAt: time.Now(),
	}

	if err := m.save(); err != nil {
		return nil, fmt.Errorf("persist memory: %w", err)
	}

	res, _ := json.Marshal(map[string]string{"status": "SAVED", "key": input.Key})
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}

func (m *MemoryStore) handleAppendMemory(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		Key     string      `json:"key"`
		Content interface{} `json:"content"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if input.Key == "" {
		return nil, ErrKeyRequired
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, exists := m.data[input.Key]
	if !exists {
		// New entry as single element array
		m.data[input.Key] = MemoryEntry{
			Key:       input.Key,
			Value:     []interface{}{input.Content},
			UpdatedAt: time.Now(),
		}
	} else {
		switch v := existing.Value.(type) {
		case []interface{}:
			existing.Value = append(v, input.Content)
		case string:
			if strVal, ok := input.Content.(string); ok {
				existing.Value = v + "\n" + strVal
			} else {
				existing.Value = []interface{}{v, input.Content}
			}
		default:
			existing.Value = []interface{}{v, input.Content}
		}
		existing.UpdatedAt = time.Now()
		m.data[input.Key] = existing
	}

	if err := m.save(); err != nil {
		return nil, fmt.Errorf("persist memory: %w", err)
	}

	res, _ := json.Marshal(map[string]string{"status": "APPENDED", "key": input.Key})
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}

func (m *MemoryStore) handleListMemory(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	type Item struct {
		Key       string    `json:"key"`
		UpdatedAt time.Time `json:"updated_at"`
	}

	items := make([]Item, 0, len(m.data))
	for k, v := range m.data {
		items = append(items, Item{Key: k, UpdatedAt: v.UpdatedAt})
	}

	res, _ := json.Marshal(map[string]interface{}{"items": items})
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}

// sanitizeRepoPath verifies that a relative path does not escape the repository root.
func (m *MemoryStore) sanitizeRepoPath(relPath string) (string, error) {
	cleanPath := filepath.Clean(relPath)
	if strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(cleanPath) {
		return "", ErrPathTraversal
	}
	targetPath := filepath.Join(m.repoDir, cleanPath)
	rel, err := filepath.Rel(m.repoDir, targetPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", ErrPathTraversal
	}
	return targetPath, nil
}

func (m *MemoryStore) handleReadRepoFile(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	targetPath, err := m.sanitizeRepoPath(input.Path)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: fmt.Sprintf("read file %q: %v", input.Path, err)}},
			IsError: true,
		}, nil
	}

	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(data)}},
	}, nil
}

func (m *MemoryStore) handleListRepoFiles(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		Directory string `json:"directory"`
	}
	_ = json.Unmarshal(args, &input)

	targetDir, err := m.sanitizeRepoPath(input.Directory)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: fmt.Sprintf("list dir: %v", err)}},
			IsError: true,
		}, nil
	}

	type Entry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
	}
	list := make([]Entry, 0, len(entries))
	for _, e := range entries {
		list = append(list, Entry{Name: e.Name(), IsDir: e.IsDir()})
	}

	res, _ := json.Marshal(map[string]interface{}{"files": list})
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}

func (m *MemoryStore) handleSearchRepo(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	re, err := regexp.Compile(input.Query)
	if err != nil {
		return &protocol.ToolResult{
			Content: []protocol.ToolContent{{Type: "text", Text: fmt.Sprintf("invalid regex %q: %v", input.Query, err)}},
			IsError: true,
		}, nil
	}

	type Match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}

	var matches []Match
	err = filepath.Walk(m.repoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		// Ignore git metadata
		if strings.Contains(path, "/.git/") {
			return nil
		}

		relPath, _ := filepath.Rel(m.repoDir, path)
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				matches = append(matches, Match{
					Path: relPath,
					Line: i + 1,
					Text: strings.TrimSpace(line),
				})
				if len(matches) >= 100 {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	res, _ := json.Marshal(map[string]interface{}{"matches": matches})
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(res)}},
	}, nil
}

// Get retrieves a value by key.
func (m *MemoryStore) Get(key string) (interface{}, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.data[key]
	if !ok {
		return nil, false
	}
	return entry.Value, true
}

// GetAll returns a map of all stored key-value pairs.
func (m *MemoryStore) GetAll() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make(map[string]interface{}, len(m.data))
	for k, v := range m.data {
		res[k] = v.Value
	}
	return res
}

// Set stores a key-value pair and persists it to disk.
func (m *MemoryStore) Set(key string, val interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = MemoryEntry{
		Key:       key,
		Value:     val,
		UpdatedAt: time.Now(),
	}
	return m.save()
}

