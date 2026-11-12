package mcp

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"sync"
)

// ToolFunc runs one tool.
type ToolFunc func(ctx context.Context, args json.RawMessage) *ToolResult

// Server is a minimal MCP server with a fixed set of tools. It is what a
// tool set written in Go needs to serve over stdio, and what the tests'
// fake tool sets use.
type Server struct {
	Name    string
	Version string

	mu    sync.Mutex
	tools map[string]Tool
	funcs map[string]ToolFunc
}

// NewServer returns an empty server.
func NewServer(name, version string) *Server {
	return &Server{Name: name, Version: version, tools: map[string]Tool{}, funcs: map[string]ToolFunc{}}
}

// Add registers a tool.
func (s *Server) Add(t Tool, f ToolFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[t.Name] = t
	s.funcs[t.Name] = f
}

// Serve answers requests on r/w until the stream ends.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	conn := NewConn(r, w, s.handle)
	return conn.Run(ctx)
}

func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"serverInfo":      map[string]string{"name": s.Name, "version": s.Version},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		s.mu.Lock()
		defer s.mu.Unlock()
		list := make([]Tool, 0, len(s.tools))
		for _, t := range s.tools {
			list = append(list, t)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p CallParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &Error{Code: InvalidParams, Message: err.Error()}
		}
		s.mu.Lock()
		f := s.funcs[p.Name]
		s.mu.Unlock()
		if f == nil {
			return Errorf("unknown tool %q", p.Name), nil
		}
		return f(ctx, p.Arguments), nil
	}
	if len(method) > 14 && method[:14] == "notifications/" {
		return nil, nil
	}
	return nil, &Error{Code: MethodNotFound, Message: "method not found: " + method}
}
