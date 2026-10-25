package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bwsalmon/silo/v3/gateway"
)

// MCPOptions configures the stdio `silo-mcp` (`silo mcp`) server.
type MCPOptions struct {
	SocketPath string
	Token      string
	Stdin      io.Reader
	Stdout     io.Writer
}

// MCPTool describes a single MCP tool in `tools/list`.
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// MCPCallResult is the MCP `tools/call` result envelope.
type MCPCallResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// MCPContent is one item of MCP tool output.
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MCPTools returns the Silo v3 MCP tools exposed by `silo-mcp`.
// Note: there is no custom `send_message` or `message_agent` tool; parents
// drive children using the CLIs' own built-in `--resume` / MCP / ACP session
// mechanisms.
func MCPTools() []MCPTool {
	return []MCPTool{
		{
			Name:        "register_agent",
			Description: "Register a new child agent in Silo and return its registration token (waiting for human approval first if this agent's permission policy is 'ask').",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":          map[string]any{"type": "string", "description": "Short slug for the child agent."},
					"kind":          map[string]any{"type": "string", "description": "Execution environment: process, service, container, or vm."},
					"brief":         map[string]any{"type": "string", "description": "Initial task brief sent when the child attaches via silo connect."},
					"parent":        map[string]any{"type": "string", "description": "Optional parent agent id (defaults to caller)."},
					"uid":           map[string]any{"type": "integer", "description": "Optional local Unix UID."},
					"unit":          map[string]any{"type": "string", "description": "Optional systemd unit, container name, or VM id."},
					"workdir":       map[string]any{"type": "string", "description": "Optional working directory."},
					"permissions":   map[string]any{"type": "string", "description": "Permission policy: ask (default), auto, or deny-shell."},
					"wait_approval": map[string]any{"type": "boolean", "description": "Whether to wait for human approval before returning when permissions='ask' (default true)."},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "list_agents",
			Description: "List registered agents and their status.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"parent": map[string]any{"type": "string", "description": "Optional parent agent id filter."},
				},
			},
		},
		{
			Name:        "read_agent",
			Description: "Read an agent's status and final report (plus recent session transcript for non-root callers).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent": map[string]any{"type": "string", "description": "Agent id to read."},
					"since": map[string]any{"type": "integer", "description": "Optional sequence number to read transcript updates after."},
				},
				"required": []string{"agent"},
			},
		},
		{
			Name:        "stop_agent",
			Description: "Stop a running child agent (sends SIGTERM, or SIGKILL if kill=true).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent": map[string]any{"type": "string", "description": "Agent id to stop."},
					"kill":  map[string]any{"type": "boolean", "description": "Send SIGKILL immediately instead of graceful SIGTERM."},
				},
				"required": []string{"agent"},
			},
		},
		{
			Name:        "register_page",
			Description: "Register a clickable page link in the Silo UI (either an internal port reverse-proxied at /p/<slug>/ or an external http(s):// URL).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"slug":        map[string]any{"type": "string", "description": "URL-safe slug served at /p/<slug>/."},
					"title":       map[string]any{"type": "string", "description": "Human-readable title shown in the UI."},
					"host":        map[string]any{"type": "string", "description": "Target host/IP for internal port proxying (default 127.0.0.1)."},
					"port":        map[string]any{"type": "integer", "description": "Internal TCP port to reverse-proxy at /p/<slug>/."},
					"expose_port": map[string]any{"type": "integer", "description": "Optional public listener port to expose directly."},
					"url":         map[string]any{"type": "string", "description": "External http:// or https:// URL (mutually exclusive with port)."},
				},
				"required": []string{"slug"},
			},
		},
		{
			Name:        "unregister_page",
			Description: "Remove a registered page link by slug.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"slug": map[string]any{"type": "string", "description": "Slug of the page link to remove."},
				},
				"required": []string{"slug"},
			},
		},
		{
			Name:        "list_pages",
			Description: "List registered page links.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"all": map[string]any{"type": "boolean", "description": "Include removed page links."},
				},
			},
		},
		{
			Name:        "request_secret",
			Description: "Ask the human for a secret or credential. Blocks until the human provides it, writes it with 0600 permissions to the local path via silo connect, and returns only {status, name, path} so the secret value never enters the LLM context.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":   map[string]any{"type": "string", "description": "Secret name (e.g. 'github-token' or 'GEMINI_API_KEY')."},
					"reason": map[string]any{"type": "string", "description": "Explanation shown to the human of what the secret is and why it is needed."},
					"path":   map[string]any{"type": "string", "description": "Optional target file path (default ~/.config/silo/secrets/<name>)."},
				},
				"required": []string{"name", "reason"},
			},
		},
		{
			Name:        "file_request",
			Description: "File a structured escalation request to the root agent / human when an unprivileged agent needs a host/system change.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary":  map[string]any{"type": "string", "description": "Short summary of what is needed on the host."},
					"details":  map[string]any{"type": "string", "description": "Detailed context and requirements."},
					"commands": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional suggested commands."},
				},
				"required": []string{"summary"},
			},
		},
		{
			Name:        "report",
			Description: "Record this agent's structured completion report and exit status.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status":    map[string]any{"type": "string", "description": "Completion status (e.g. 'done', 'failed')."},
					"summary":   map[string]any{"type": "string", "description": "Structured summary of the outcome."},
					"exit_code": map[string]any{"type": "integer", "description": "Optional exit code."},
				},
				"required": []string{"summary"},
			},
		},
	}
}

// RunMCP runs the stdio MCP server against the configured bridge socket.
func RunMCP(ctx context.Context, opts MCPOptions) error {
	sock := opts.SocketPath
	if sock == "" {
		sock = DefaultSocketPath()
	}
	tok := opts.Token
	if tok == "" {
		tok = os.Getenv("SILO_AGENT_TOKEN")
	}
	in := opts.Stdin
	if in == nil {
		in = os.Stdin
	}
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	return ServeMCP(ctx, in, out, NewClient(sock), tok)
}

// ServeMCP serves MCP JSON-RPC 2.0 requests from in and writes responses to out.
func ServeMCP(ctx context.Context, in io.Reader, out io.Writer, client *Client, token string) error {
	br := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		resp := handleMCPMessage(ctx, client, token, line)
		if resp == nil {
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			continue
		}
		raw, mErr := json.Marshal(resp)
		if mErr != nil {
			return mErr
		}
		raw = append(raw, '\n')
		if _, wErr := out.Write(raw); wErr != nil {
			return wErr
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func handleMCPMessage(ctx context.Context, client *Client, token string, line []byte) map[string]any {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   &gateway.Error{Code: gateway.CodeParseError, Message: err.Error()},
		}
	}
	// Notifications (such as notifications/initialized) have no ID and receive no response.
	if len(req.ID) == 0 {
		return nil
	}
	switch req.Method {
	case "initialize":
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "silo-mcp",
					"version": "0.1.0",
				},
			},
		}
	case "ping":
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{},
		}
	case "tools/list":
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"tools": MCPTools(),
			},
		}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   &gateway.Error{Code: gateway.CodeInvalidParams, Message: err.Error()},
			}
		}
		res, err := callMCPTool(ctx, client, token, p.Name, p.Arguments)
		if err != nil {
			return map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": MCPCallResult{
					Content: []MCPContent{{Type: "text", Text: err.Error()}},
					IsError: true,
				},
			}
		}
		raw, _ := json.Marshal(res)
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": MCPCallResult{
				Content: []MCPContent{{Type: "text", Text: string(raw)}},
			},
		}
	default:
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error":   &gateway.Error{Code: gateway.CodeMethodNotFound, Message: fmt.Sprintf("unknown MCP method %q", req.Method)},
		}
	}
}

func callMCPTool(ctx context.Context, client *Client, token string, name string, args map[string]any) (any, error) {
	params := map[string]any{}
	for k, v := range args {
		params[k] = v
	}
	if token != "" {
		if _, exists := params["agent_token"]; !exists {
			params["agent_token"] = token
		}
	}

	var rpcMethod string
	switch name {
	case "register_agent":
		if _, ok := params["wait_approval"]; !ok {
			params["wait_approval"] = true
		}
		rpcMethod = "agent.register"
	case "list_agents":
		rpcMethod = "agent.list"
	case "read_agent":
		rpcMethod = "agent.read"
	case "stop_agent":
		if kill, _ := params["kill"].(bool); kill {
			params["mode"] = "kill"
		}
		rpcMethod = "agent.stop"
	case "register_page":
		rpcMethod = "page.register"
	case "unregister_page":
		rpcMethod = "page.unregister"
	case "list_pages":
		rpcMethod = "page.list"
	case "request_secret":
		if _, ok := params["wait"]; !ok {
			params["wait"] = true
		}
		rpcMethod = "secret.request"
	case "file_request":
		rpcMethod = "request.file"
	case "report":
		rpcMethod = "agent.report"
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}

	var out json.RawMessage
	if err := client.Call(ctx, rpcMethod, params, &out); err != nil {
		return nil, err
	}
	return out, nil
}
