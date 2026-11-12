// Package mcp is the small slice of the Model Context Protocol silo
// needs: newline-delimited JSON-RPC 2.0 over a byte stream, in both
// directions, which the router serves the agent CLI with; and the
// Streamable HTTP transport, whose client (HTTPClient) the router drives
// tool sets' servers with and whose server side (Server.ServeHTTP) the
// tests' fake tool sets use.
//
// Adapted from silo v4's pkg/mcp/protocol.
package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProtocolVersion is the MCP revision silo speaks.
const ProtocolVersion = "2025-06-18"

// Message is a JSON-RPC 2.0 request, notification or response.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message) }

// Standard JSON-RPC error codes.
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// Tool is one tool as tools/list describes it. The input schema is kept
// raw so a child server's schema passes through the router unchanged.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolResult is a tools/call result. Content items are kept raw so
// images and other non-text content pass through unchanged.
type ToolResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

// CallParams are tools/call's parameters.
type CallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Text returns a successful result carrying one text item.
func Text(s string) *ToolResult {
	return &ToolResult{Content: []json.RawMessage{textItem(s)}}
}

// Errorf returns an error result (isError: true) carrying one text item.
func Errorf(format string, args ...any) *ToolResult {
	return &ToolResult{Content: []json.RawMessage{textItem(fmt.Sprintf(format, args...))}, IsError: true}
}

func textItem(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
	return b
}

// AllText joins the text of every text item in r.
func (r *ToolResult) AllText() string {
	var parts []string
	for _, c := range r.Content {
		var item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(c, &item) == nil && item.Type == "text" {
			parts = append(parts, item.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Schema builds a JSON object schema from property schemas and the names
// of the required ones.
func Schema(props map[string]any, required ...string) json.RawMessage {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	b, _ := json.Marshal(s)
	return b
}
