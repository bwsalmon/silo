// Package acp defines Agent Client Protocol (ACP) JSON-RPC 2.0 messages,
// `silo connect` control frames, and `/etc/silo/clis.d/` CLI presets.
package acp

import (
	"encoding/json"
	"strings"
)

// ProtocolVersion is the ACP version spoken by the bridge.
const ProtocolVersion = 1

// JSON-RPC 2.0 method names used in ACP.
const (
	MethodInitialize        = "initialize"
	MethodSessionNew        = "session/new"
	MethodSessionLoad       = "session/load"
	MethodSessionPrompt     = "session/prompt"
	MethodSessionCancel     = "session/cancel"
	MethodSessionUpdate     = "session/update"
	MethodRequestPermission = "session/request_permission"
)

// Control frame types carried alongside ACP JSON-RPC between the bridge and
// `silo connect`.
const (
	ControlConnect     = "connect"
	ControlConnected   = "connected"
	ControlStop        = "stop"
	ControlSecretWrite = "secret.write"
	ControlSecretAck   = "secret.ack"
	ControlExit        = "exit"
)

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// Frame is a single line/message on an ACP or `silo connect` stream.
// Standard ACP messages have JSONRPC="2.0"; `silo connect` control messages
// have Silo set to one of the Control* constants.
type Frame struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`

	// Control-channel fields when Silo != "".
	Silo         string `json:"silo,omitempty"`
	Token        string `json:"token,omitempty"`
	Agent        string `json:"agent,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	Runtime      string `json:"runtime,omitempty"`
	Brief        string `json:"brief,omitempty"`
	Permissions  string `json:"permissions,omitempty"`
	HeartbeatSec int    `json:"heartbeat_s,omitempty"`
	Host         string `json:"host,omitempty"`
	PID          int    `json:"pid,omitempty"`
	Mode         string `json:"mode,omitempty"`
	TimeoutMS    int    `json:"timeout_ms,omitempty"`
	ReqID        string `json:"req_id,omitempty"`
	Name         string `json:"name,omitempty"`
	Path         string `json:"path,omitempty"`
	Value        string `json:"value,omitempty"`
	Status       string `json:"status,omitempty"`
	Code         *int   `json:"code,omitempty"`
	Err          string `json:"err,omitempty"`
}

// InitializeParams is the client's `initialize` request.
type InitializeParams struct {
	ProtocolVersion    int            `json:"protocolVersion"`
	ClientCapabilities map[string]any `json:"clientCapabilities,omitempty"`
}

// InitializeResult is the agent's `initialize` response.
type InitializeResult struct {
	ProtocolVersion   int            `json:"protocolVersion"`
	AgentCapabilities map[string]any `json:"agentCapabilities,omitempty"`
	AgentInfo         map[string]any `json:"agentInfo,omitempty"`
}

// MCPServer describes an MCP server passed in `session/new` or `session/load`.
type MCPServer struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     []EnvVar `json:"env,omitempty"`
}

// EnvVar is a name/value pair in MCPServer.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SessionNewParams is `session/new`.
type SessionNewParams struct {
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// SessionNewResult is the response to `session/new`.
type SessionNewResult struct {
	SessionID string `json:"sessionId"`
}

// SessionLoadParams is `session/load`.
type SessionLoadParams struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// ContentBlock is one block in a `session/prompt` message.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// SessionPromptParams is `session/prompt`.
type SessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptText extracts concatenated text from a raw `session/prompt` params
// payload, accepting either `{"prompt":[{"type":"text","text":"..."}]}` or
// `{"prompt":"..."}` or `{"text":"..."}`.
func PromptText(raw json.RawMessage) string {
	var p struct {
		Prompt json.RawMessage `json:"prompt"`
		Text   string          `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if p.Text != "" && len(p.Prompt) == 0 {
		return p.Text
	}
	var s string
	if err := json.Unmarshal(p.Prompt, &s); err == nil {
		return s
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(p.Prompt, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// SessionPromptResult is the response to `session/prompt`.
type SessionPromptResult struct {
	StopReason string `json:"stopReason"`
}

// SessionCancelParams is `session/cancel`.
type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// SessionUpdateParams is a `session/update` notification from the agent.
type SessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// PermissionOption is one choice in a `session/request_permission` request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"` // allow_once, allow_always, reject_once, reject_always
}

// RequestPermissionParams is `session/request_permission` from the agent.
type RequestPermissionParams struct {
	SessionID string             `json:"sessionId,omitempty"`
	ToolCall  json.RawMessage    `json:"toolCall,omitempty"`
	Options   []PermissionOption `json:"options,omitempty"`
	Tool      string             `json:"tool,omitempty"`
	Command   string             `json:"command,omitempty"`
}

// IsShell reports whether the permission request is for executing a shell command.
func (p RequestPermissionParams) IsShell() bool {
	if p.Tool == "bash" || p.Tool == "shell" || p.Tool == "sh" || p.Tool == "execute" || p.Command != "" {
		return true
	}
	if len(p.ToolCall) > 0 {
		var tc struct {
			Kind  string `json:"kind"`
			Title string `json:"title"`
			Name  string `json:"name"`
		}
		if json.Unmarshal(p.ToolCall, &tc) == nil {
			k := strings.ToLower(tc.Kind)
			n := strings.ToLower(tc.Name)
			if k == "execute" || k == "shell" || k == "bash" || n == "bash" || n == "shell" {
				return true
			}
		}
	}
	return false
}

// PickOption returns the optionId matching approve (allow vs reject) from p.Options,
// falling back to "allow-once" or "reject-once".
func (p RequestPermissionParams) PickOption(approve bool) string {
	for _, opt := range p.Options {
		k := strings.ToLower(opt.Kind)
		id := strings.ToLower(opt.OptionID)
		if approve && (strings.Contains(k, "allow") || strings.Contains(id, "allow")) {
			return opt.OptionID
		}
		if !approve && (strings.Contains(k, "reject") || strings.Contains(k, "deny") || strings.Contains(id, "reject") || strings.Contains(id, "deny")) {
			return opt.OptionID
		}
	}
	if approve {
		return "allow-once"
	}
	return "reject-once"
}

// PermissionOutcome is the standard ACP outcome inside RequestPermissionResult.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`            // "selected" or "cancelled"
	OptionID string `json:"optionId,omitempty"` // chosen optionId when Outcome == "selected"
}

// RequestPermissionResult is the response to `session/request_permission`.
type RequestPermissionResult struct {
	Outcome  PermissionOutcome `json:"outcome"`
	Approved bool              `json:"approved"`
	Decision string            `json:"decision"`
	Comment  string            `json:"comment,omitempty"`
}
