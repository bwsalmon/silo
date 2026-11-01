package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/audit"
	"github.com/bwsalmon/silo/v4/pkg/auth"
	"github.com/bwsalmon/silo/v4/pkg/mcp/memory"
	"github.com/bwsalmon/silo/v4/pkg/mcp/protocol"
	"github.com/bwsalmon/silo/v4/pkg/quota"
	"github.com/bwsalmon/silo/v4/pkg/supervisor"
)

var (
	ErrUnauthorizedAccess = errors.New("access denied: calling agent is not the parent of the target sub-agent")
)

// CommandBuilder generates CLI arguments to launch an agent process.
type CommandBuilder func(spec supervisor.AgentSpec) []string

// Manager handles sub-agent orchestration MCP tools for a parent agent.
type Manager struct {
	mu             sync.RWMutex
	callerAgentID  string
	ledger         *quota.Ledger
	supervisor     *supervisor.Supervisor
	tokenProvider  auth.TokenProvider
	baseDir        string
	repoDir        string
	logger         audit.Logger
	commandBuilder CommandBuilder
}

// NewManager creates a Manager instance.
func NewManager(
	callerAgentID string,
	ledger *quota.Ledger,
	sup *supervisor.Supervisor,
	tokenProvider auth.TokenProvider,
	baseDir, repoDir string,
	logger audit.Logger,
) *Manager {
	return &Manager{
		callerAgentID: callerAgentID,
		ledger:        ledger,
		supervisor:    sup,
		tokenProvider: tokenProvider,
		baseDir:       baseDir,
		repoDir:       repoDir,
		logger:        logger,
	}
}

// SetCommandBuilder sets a custom command generator for spawned sub-agents.
func (m *Manager) SetCommandBuilder(cb CommandBuilder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commandBuilder = cb
}

// RegisterTools registers subagents-mcp tools onto an MCP server.
func (m *Manager) RegisterTools(server *protocol.Server) {
	server.RegisterTool(
		protocol.Tool{
			Name:        "spawn_subagent",
			Description: "Creates and starts a new supervised sub-agent under hierarchical quota control.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":            map[string]interface{}{"type": "string"},
					"description":     map[string]interface{}{"type": "string"},
					"prompt":          map[string]interface{}{"type": "string"},
					"lifecycle":       map[string]interface{}{"type": "string", "enum": []string{"one_shot", "persistent"}},
					"share_repo":      map[string]interface{}{"type": "boolean"},
					"allow_vm_tools":  map[string]interface{}{"type": "boolean"},
					"initial_memory":  map[string]interface{}{"type": "object"},
					"service_account": map[string]interface{}{"type": "object"},
					"delegation":      map[string]interface{}{"type": "object"},
					"timeout_seconds": map[string]interface{}{"type": "integer"},
				},
				"required": []string{"name", "prompt"},
			},
		},
		m.handleSpawnSubagent,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "list_subagents",
			Description: "Returns all active and completed child sub-agents owned by the calling agent.",
			InputSchema: map[string]interface{}{
				"type": "object",
			},
		},
		m.handleListSubagents,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "get_subagent_output",
			Description: "Retrieves the execution logs, status, and final report of a child sub-agent.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{"type": "string"},
				},
				"required": []string{"agent_id"},
			},
		},
		m.handleGetSubagentOutput,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "get_subagent_memory",
			Description: "Allows a parent agent to inspect the working memory area of a child sub-agent.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{"type": "string"},
					"key":      map[string]interface{}{"type": "string"},
				},
				"required": []string{"agent_id"},
			},
		},
		m.handleGetSubagentMemory,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "send_subagent_message",
			Description: "Dispatches an instruction or query to an active persistent sub-agent.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{"type": "string"},
					"message":  map[string]interface{}{"type": "string"},
				},
				"required": []string{"agent_id", "message"},
			},
		},
		m.handleSendSubagentMessage,
	)

	server.RegisterTool(
		protocol.Tool{
			Name:        "stop_subagent",
			Description: "Explicitly terminates a sub-agent and cascades termination to all descendants in its subtree.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_id": map[string]interface{}{"type": "string"},
				},
				"required": []string{"agent_id"},
			},
		},
		m.handleStopSubagent,
	)
}

type spawnInput struct {
	Name           string                 `json:"name"`
	Description    string                 `json:"description"`
	Prompt         string                 `json:"prompt"`
	Lifecycle      string                 `json:"lifecycle"`
	ShareRepo      bool                   `json:"share_repo"`
	AllowVMTools   *bool                  `json:"allow_vm_tools"`
	InitialMemory  map[string]interface{} `json:"initial_memory"`
	TimeoutSeconds int                    `json:"timeout_seconds"`
	ServiceAccount *struct {
		Mode             string   `json:"mode"`
		Email            string   `json:"email"`
		AllowedResources []string `json:"allowed_resources"`
	} `json:"service_account"`
	Delegation *struct {
		CanSpawnSubagents bool `json:"can_spawn_subagents"`
		MaxSubagents      *int `json:"max_subagents"`
		MaxConcurrent     *int `json:"max_concurrent"`
		MaxDepth          *int `json:"max_depth"`
	} `json:"delegation"`
}

func (m *Manager) handleSpawnSubagent(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input spawnInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	if input.Name == "" {
		return errorResult("INVALID_ARGUMENT", "name is required")
	}
	if input.Prompt == "" {
		return errorResult("INVALID_ARGUMENT", "prompt is required")
	}

	lifecycle := supervisor.LifecycleOneShot
	if input.Lifecycle != "" {
		switch input.Lifecycle {
		case string(supervisor.LifecycleOneShot):
			lifecycle = supervisor.LifecycleOneShot
		case string(supervisor.LifecyclePersistent):
			lifecycle = supervisor.LifecyclePersistent
		default:
			return errorResult("INVALID_ARGUMENT", fmt.Sprintf("invalid lifecycle %q; expected 'one_shot' or 'persistent'", input.Lifecycle))
		}
	}

	allowVMTools := true
	if input.AllowVMTools != nil {
		allowVMTools = *input.AllowVMTools
	}

	timeout := 3600 * time.Second
	if input.TimeoutSeconds > 0 {
		timeout = time.Duration(input.TimeoutSeconds) * time.Second
	}

	// 1. Quota Allocation
	canSpawn := false
	maxSub := -1
	maxConc := -1
	maxDepth := -1
	if input.Delegation != nil && input.Delegation.CanSpawnSubagents {
		canSpawn = true
		if input.Delegation.MaxSubagents != nil {
			maxSub = *input.Delegation.MaxSubagents
		}
		if input.Delegation.MaxConcurrent != nil {
			maxConc = *input.Delegation.MaxConcurrent
		}
		if input.Delegation.MaxDepth != nil {
			maxDepth = *input.Delegation.MaxDepth
		}
	}

	childID := fmt.Sprintf("subagent-%s-%d", input.Name, time.Now().UnixNano()%100000000)

	_, err := m.ledger.AllocateChild(m.callerAgentID, childID, maxSub, maxConc, maxDepth, canSpawn)
	if err != nil {
		if m.logger != nil {
			if errors.Is(err, quota.ErrQuotaExceeded) {
				_ = m.logger.Log("QUOTA_EXCEEDED_REJECTED", map[string]interface{}{
					"parent_id": m.callerAgentID,
					"name":      input.Name,
				})
			} else if errors.Is(err, quota.ErrConcurrencyLimit) {
				_ = m.logger.Log("CONCURRENCY_LIMIT_REJECTED", map[string]interface{}{
					"parent_id": m.callerAgentID,
					"name":      input.Name,
				})
			}
		}

		if errors.Is(err, quota.ErrQuotaExceeded) {
			return errorResult("QUOTA_EXCEEDED", err.Error())
		}
		if errors.Is(err, quota.ErrConcurrencyLimit) {
			return errorResult("CONCURRENCY_LIMIT_REACHED", err.Error())
		}
		if errors.Is(err, quota.ErrDepthLimitExceeded) {
			return errorResult("DEPTH_LIMIT_EXCEEDED", err.Error())
		}
		if errors.Is(err, quota.ErrDelegationNotAllowed) {
			return errorResult("DELEGATION_NOT_PERMITTED", err.Error())
		}
		return errorResult("ALLOCATION_FAILED", err.Error())
	}

	// 2. Initialize Memory Store
	childRepoDir := ""
	if input.ShareRepo && m.repoDir != "" {
		childRepoDir = m.repoDir
	}

	childMem, err := memory.NewMemoryStore(childID, m.baseDir, childRepoDir)
	if err != nil {
		m.ledger.ChildTerminated(childID)
		return nil, fmt.Errorf("initialize child memory: %w", err)
	}

	if len(input.InitialMemory) > 0 {
		for k, v := range input.InitialMemory {
			if err := childMem.Set(k, v); err != nil {
				m.ledger.ChildTerminated(childID)
				return nil, fmt.Errorf("seed initial memory key %q: %w", k, err)
			}
		}
	}

	// 3. Service Account and Tokens
	saEmail := ""
	var cabResources []string
	if input.ServiceAccount != nil {
		saEmail = input.ServiceAccount.Email
		cabResources = input.ServiceAccount.AllowedResources
	}

	// 4. Construct Agent Spec and Spawn
	spec := supervisor.AgentSpec{
		ID:             childID,
		ParentID:       m.callerAgentID,
		Name:           input.Name,
		Description:    input.Description,
		Prompt:         input.Prompt,
		Lifecycle:      lifecycle,
		ServiceAccount: saEmail,
		AllowedCAB:     cabResources,
		ShareRepo:      input.ShareRepo,
		AllowVMTools:   allowVMTools,
		Timeout:        timeout,
	}

	m.mu.RLock()
	if m.commandBuilder != nil {
		spec.Command = m.commandBuilder(spec)
	}
	m.mu.RUnlock()

	proc, err := m.supervisor.Spawn(ctx, spec)
	if err != nil {
		m.ledger.ChildTerminated(childID)
		return nil, fmt.Errorf("spawn child process: %w", err)
	}

	if m.logger != nil {
		_ = m.logger.Log("SUBAGENT_SPAWN", map[string]interface{}{
			"parent_id":       m.callerAgentID,
			"agent_id":        childID,
			"name":            input.Name,
			"lifecycle":       string(lifecycle),
			"service_account": saEmail,
			"share_repo":      input.ShareRepo,
			"allow_vm_tools":  allowVMTools,
		})
	}

	resp := map[string]interface{}{
		"agent_id":   proc.Spec.ID,
		"name":       proc.Spec.Name,
		"status":     string(proc.Status),
		"lifecycle":  string(proc.Spec.Lifecycle),
		"created_at": proc.StartTime.UTC().Format(time.RFC3339),
	}

	bytes, _ := json.Marshal(resp)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(bytes)}},
	}, nil
}

func (m *Manager) handleListSubagents(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	children := m.supervisor.ListChildren(m.callerAgentID)

	type subagentItem struct {
		AgentID       string `json:"agent_id"`
		Name          string `json:"name"`
		Lifecycle     string `json:"lifecycle"`
		Status        string `json:"status"`
		UptimeSeconds int    `json:"uptime_seconds"`
		ExitCode      int    `json:"exit_code,omitempty"`
		Restarts      int    `json:"restarts,omitempty"`
	}

	list := make([]subagentItem, 0, len(children))
	now := time.Now()
	for _, c := range children {
		uptime := int(now.Sub(c.StartTime).Seconds())
		if c.Status == supervisor.StatusCompleted || c.Status == supervisor.StatusFailed || c.Status == supervisor.StatusStopped {
			if !c.EndTime.IsZero() {
				uptime = int(c.EndTime.Sub(c.StartTime).Seconds())
			}
		}

		item := subagentItem{
			AgentID:       c.Spec.ID,
			Name:          c.Spec.Name,
			Lifecycle:     string(c.Spec.Lifecycle),
			Status:        string(c.Status),
			UptimeSeconds: uptime,
			ExitCode:      c.ExitCode,
			Restarts:      c.Restarts,
		}
		list = append(list, item)
	}

	resp := map[string]interface{}{
		"subagents": list,
	}

	bytes, _ := json.Marshal(resp)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(bytes)}},
	}, nil
}

func (m *Manager) handleGetSubagentOutput(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	proc, err := m.supervisor.GetAgent(input.AgentID)
	if err != nil {
		return errorResult("NOT_FOUND", fmt.Sprintf("sub-agent %s not found", input.AgentID))
	}
	if proc.Spec.ParentID != m.callerAgentID {
		return errorResult("UNAUTHORIZED", ErrUnauthorizedAccess.Error())
	}

	resp := map[string]interface{}{
		"agent_id":         proc.Spec.ID,
		"status":           string(proc.Status),
		"output":           proc.Output,
		"tool_calls_count": 0,
	}
	if !proc.EndTime.IsZero() {
		resp["completed_at"] = proc.EndTime.UTC().Format(time.RFC3339)
	}

	bytes, _ := json.Marshal(resp)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(bytes)}},
	}, nil
}

func (m *Manager) handleGetSubagentMemory(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		AgentID string `json:"agent_id"`
		Key     string `json:"key"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	proc, err := m.supervisor.GetAgent(input.AgentID)
	if err != nil {
		return errorResult("NOT_FOUND", fmt.Sprintf("sub-agent %s not found", input.AgentID))
	}
	if proc.Spec.ParentID != m.callerAgentID {
		return errorResult("UNAUTHORIZED", ErrUnauthorizedAccess.Error())
	}

	childMem, err := memory.NewMemoryStore(input.AgentID, m.baseDir, "")
	if err != nil {
		return nil, fmt.Errorf("read child memory store: %w", err)
	}

	resp := map[string]interface{}{
		"agent_id": input.AgentID,
	}

	if input.Key != "" {
		val, exists := childMem.Get(input.Key)
		if !exists {
			return errorResult("NOT_FOUND", fmt.Sprintf("key %q not found in sub-agent memory", input.Key))
		}
		resp["key"] = input.Key
		resp["value"] = val
	} else {
		resp["memory"] = childMem.GetAll()
	}

	bytes, _ := json.Marshal(resp)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(bytes)}},
	}, nil
}

func (m *Manager) handleSendSubagentMessage(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		AgentID string `json:"agent_id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	if input.AgentID == "" || input.Message == "" {
		return errorResult("INVALID_ARGUMENT", "agent_id and message are required")
	}

	proc, err := m.supervisor.GetAgent(input.AgentID)
	if err != nil {
		return errorResult("NOT_FOUND", fmt.Sprintf("sub-agent %s not found", input.AgentID))
	}
	if proc.Spec.ParentID != m.callerAgentID {
		return errorResult("UNAUTHORIZED", ErrUnauthorizedAccess.Error())
	}

	if proc.Spec.Lifecycle != supervisor.LifecyclePersistent {
		return errorResult("INVALID_LIFECYCLE", "send_subagent_message is only valid for persistent sub-agents")
	}

	if proc.Status == supervisor.StatusStopped || proc.Status == supervisor.StatusCompleted || proc.Status == supervisor.StatusFailed {
		return errorResult("INVALID_STATE", fmt.Sprintf("sub-agent status is %s; must be active to receive messages", proc.Status))
	}

	// Persist message to child's inbox memory
	childMem, err := memory.NewMemoryStore(input.AgentID, m.baseDir, "")
	if err != nil {
		return nil, fmt.Errorf("open child memory store: %w", err)
	}

	type inboxItem struct {
		From      string `json:"from"`
		Message   string `json:"message"`
		Timestamp string `json:"timestamp"`
	}

	var inbox []inboxItem
	if existing, ok := childMem.Get("_inbox"); ok {
		if raw, err := json.Marshal(existing); err == nil {
			_ = json.Unmarshal(raw, &inbox)
		}
	}

	inbox = append(inbox, inboxItem{
		From:      m.callerAgentID,
		Message:   input.Message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})

	_ = childMem.Set("_inbox", inbox)

	if m.logger != nil {
		_ = m.logger.Log("SUBAGENT_MESSAGE", map[string]interface{}{
			"sender_id":    m.callerAgentID,
			"recipient_id": input.AgentID,
			"message":      input.Message,
		})
	}

	resp := map[string]interface{}{
		"agent_id": input.AgentID,
		"status":   "DELIVERED",
	}

	bytes, _ := json.Marshal(resp)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(bytes)}},
	}, nil
}

func (m *Manager) handleStopSubagent(ctx context.Context, args json.RawMessage) (*protocol.ToolResult, error) {
	var input struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}

	proc, err := m.supervisor.GetAgent(input.AgentID)
	if err != nil {
		return errorResult("NOT_FOUND", fmt.Sprintf("sub-agent %s not found", input.AgentID))
	}
	if proc.Spec.ParentID != m.callerAgentID {
		return errorResult("UNAUTHORIZED", ErrUnauthorizedAccess.Error())
	}

	reapedCount, err := m.supervisor.Stop(input.AgentID)
	if err != nil {
		return nil, fmt.Errorf("stop sub-agent: %w", err)
	}

	if m.logger != nil {
		_ = m.logger.Log("SUBAGENT_STOP_REQUESTED", map[string]interface{}{
			"parent_id":             m.callerAgentID,
			"agent_id":              input.AgentID,
			"reaped_children_count": reapedCount,
		})
	}

	resp := map[string]interface{}{
		"agent_id":              input.AgentID,
		"status":                "TERMINATED",
		"reaped_children_count": reapedCount,
	}

	bytes, _ := json.Marshal(resp)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(bytes)}},
	}, nil
}

func errorResult(code, message string) (*protocol.ToolResult, error) {
	resp := map[string]interface{}{
		"error_code": code,
		"message":    message,
	}
	bytes, _ := json.Marshal(resp)
	return &protocol.ToolResult{
		Content: []protocol.ToolContent{{Type: "text", Text: string(bytes)}},
		IsError: true,
	}, nil
}
