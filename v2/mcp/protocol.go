// Adapted from github.com/bwsalmon/grain pkg/mcp/protocol.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

// Package mcp is silo's MCP plumbing: JSON-RPC 2.0 over Streamable HTTP,
// with no dependency on an MCP SDK, as grain's pkg/mcp has none. It has
// a server half (Handler, Dispatch, Serve) shared by the gateway and,
// later, by anything else silo serves, and a client half (Client) for
// talking to an MCP server over HTTP: the gateway's own tests, and later
// the proxied servers of design §13.
//
// Only the methods silo needs are spoken: initialize, ping, tools/list,
// tools/call, and the server-to-client notifications/tools/list_changed.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// ProtocolVersion is the MCP revision that introduced Streamable
	// HTTP, the only transport silo speaks.
	ProtocolVersion = "2025-03-26"
	serverName      = "silo-gateway"
	serverVersion   = "2.0.0-dev"
)

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error. A Handler returns one from CallTool to
// refuse a call outright, as opposed to a tool that ran and failed,
// which is a Result with IsError set.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// nullID is the id of an answer to a request whose own id could not be
// read. JSON-RPC 2.0 says that id must be null, not absent.
var nullID = json.RawMessage("null")

// ToolInfo is one entry of a tools/list response.
type ToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Result is a tool's outcome: text, and whether the tool failed. A
// failed tool is still a successful RPC that answered "it failed".
type Result struct {
	Text    string
	IsError bool
}

// Handler is what an MCP server serves: its tool list and its calls.
type Handler interface {
	ListTools() []ToolInfo
	// CallTool answers tools/call. A non-nil error refuses the call with
	// a JSON-RPC error: an *Error as it is, anything else as an internal
	// error carrying its message.
	CallTool(ctx context.Context, name string, args map[string]any) (Result, error)
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Every value marshalled here is built by this package from
		// strings and maps, so a failure is a bug in it.
		panic(err)
	}
	return b
}

func errorResponse(id json.RawMessage, code int, msg string) []byte {
	if len(id) == 0 {
		id = nullID
	}
	return marshal(rpcResponse{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: msg}})
}

// Dispatch answers one JSON-RPC request and returns the response to
// write, or nil for a notification, which has none.
func Dispatch(ctx context.Context, h Handler, body []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return errorResponse(nil, CodeParseError, "parse error: "+err.Error())
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return errorResponse(req.ID, CodeInvalidRequest, "invalid JSON-RPC 2.0 request")
	}
	notification := len(req.ID) == 0 || string(req.ID) == "null"
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		tools := h.ListTools()
		if tools == nil {
			tools = []ToolInfo{}
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return errorResponse(req.ID, CodeInvalidParams, "invalid params: "+err.Error())
			}
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		r, err := h.CallTool(ctx, p.Name, p.Arguments)
		if err != nil {
			var rpcErr *Error
			if errors.As(err, &rpcErr) {
				return errorResponse(req.ID, rpcErr.Code, rpcErr.Message)
			}
			return errorResponse(req.ID, CodeInternalError, err.Error())
		}
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": r.Text}},
			"isError": r.IsError,
		}
	default:
		if notification {
			return nil
		}
		return errorResponse(req.ID, CodeMethodNotFound, "unknown method "+req.Method)
	}
	if notification {
		return nil
	}
	return marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}
