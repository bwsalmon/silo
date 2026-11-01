package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// JSONRPCMessage is the base envelope for all JSON-RPC 2.0 messages.
type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError represents standard JSON-RPC 2.0 error codes.
type RPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// Tool defines an exposed tool schema in MCP.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

// ToolContent represents output content from a tool call.
type ToolContent struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// ToolResult represents the response payload for a tools/call request.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolHandler is a function that executes an MCP tool.
type ToolHandler func(ctx context.Context, args json.RawMessage) (*ToolResult, error)

// Server implements an MCP server over stdio.
type Server struct {
	name        string
	version     string
	tools       map[string]Tool
	handlers    map[string]ToolHandler
	mu          sync.RWMutex
	writeMu     sync.Mutex
}

// NewServer creates a new MCP server.
func NewServer(name, version string) *Server {
	return &Server{
		name:     name,
		version:  version,
		tools:    make(map[string]Tool),
		handlers: make(map[string]ToolHandler),
	}
}

// RegisterTool registers a tool definition and its execution handler.
func (s *Server) RegisterTool(tool Tool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[tool.Name] = tool
	s.handlers[tool.Name] = handler
}

// GetHandler returns the handler registered for a given tool name.
func (s *Server) GetHandler(name string) (ToolHandler, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.handlers[name]
	return h, ok
}

// GetTool returns the tool definition registered for a given name.
func (s *Server) GetTool(name string) (Tool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tools[name]
	return t, ok
}

// Serve reads requests from in, dispatches calls, and writes responses to out.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// Buffer up to 10MB per message for large file transfers
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			s.sendError(out, nil, ParseError, "Parse error")
			continue
		}

		// Handle request vs notification
		if msg.Method != "" {
			s.handleMethod(ctx, out, &msg)
		}
	}

	return scanner.Err()
}

func (s *Server) handleMethod(ctx context.Context, out io.Writer, msg *JSONRPCMessage) {
	switch msg.Method {
	case "initialize":
		res := map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"serverInfo": map[string]string{
				"name":    s.name,
				"version": s.version,
			},
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
		}
		s.sendResult(out, msg.ID, res)

	case "notifications/initialized":
		// No response required for notifications

	case "tools/list":
		s.mu.RLock()
		toolsList := make([]Tool, 0, len(s.tools))
		for _, t := range s.tools {
			toolsList = append(toolsList, t)
		}
		s.mu.RUnlock()

		s.sendResult(out, msg.ID, map[string]interface{}{
			"tools": toolsList,
		})

	case "tools/call":
		var callParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &callParams); err != nil {
			s.sendError(out, msg.ID, InvalidParams, "Invalid call params")
			return
		}

		s.mu.RLock()
		handler, exists := s.handlers[callParams.Name]
		s.mu.RUnlock()

		if !exists {
			s.sendError(out, msg.ID, MethodNotFound, fmt.Sprintf("Tool %q not found", callParams.Name))
			return
		}

		result, err := handler(ctx, callParams.Arguments)
		if err != nil {
			s.sendResult(out, msg.ID, &ToolResult{
				Content: []ToolContent{{Type: "text", Text: err.Error()}},
				IsError: true,
			})
			return
		}

		s.sendResult(out, msg.ID, result)

	default:
		if len(msg.ID) > 0 {
			s.sendError(out, msg.ID, MethodNotFound, fmt.Sprintf("Method %q not supported", msg.Method))
		}
	}
}

func (s *Server) sendResult(out io.Writer, id json.RawMessage, result interface{}) {
	resp := JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.writeMessage(out, resp)
}

func (s *Server) sendError(out io.Writer, id json.RawMessage, code int, message string) {
	resp := JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
	s.writeMessage(out, resp)
}

func (s *Server) writeMessage(out io.Writer, msg JSONRPCMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	out.Write(data)
	out.Write([]byte("\n"))
}
