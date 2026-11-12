package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
)

// ToolFunc runs one tool.
type ToolFunc func(ctx context.Context, args json.RawMessage) *ToolResult

// Server is a minimal MCP server with a set of tools, served over stdio
// (Serve) or Streamable HTTP (ServeHTTP, ListenAndServe). It is what the
// tests' fake tool sets use.
type Server struct {
	Name    string
	Version string

	mu      sync.Mutex
	tools   map[string]Tool
	funcs   map[string]ToolFunc
	streams map[chan []byte]bool
}

// NewServer returns an empty server.
func NewServer(name, version string) *Server {
	return &Server{Name: name, Version: version, tools: map[string]Tool{}, funcs: map[string]ToolFunc{}, streams: map[chan []byte]bool{}}
}

// Add registers a tool.
func (s *Server) Add(t Tool, f ToolFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[t.Name] = t
	s.funcs[t.Name] = f
}

// NotifyToolsChanged sends notifications/tools/list_changed on every
// open HTTP GET stream.
func (s *Server) NotifyToolsChanged() {
	b, _ := json.Marshal(&Message{JSONRPC: "2.0", Method: "notifications/tools/list_changed"})
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.streams {
		select {
		case ch <- b:
		default:
		}
	}
}

// ListenAndServe serves Streamable HTTP at /mcp on addr until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", s)
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	return srv.Serve(l)
}

// ServeHTTP answers one Streamable HTTP request: a POSTed message
// (answered with JSON), a GET for the notification stream, or a DELETE
// ending the session.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var msg Message
		if err := json.NewDecoder(io.LimitReader(r.Body, maxHTTPBody)).Decode(&msg); err != nil {
			http.Error(w, "bad JSON-RPC message", http.StatusBadRequest)
			return
		}
		if msg.Method == "" || len(msg.ID) == 0 {
			if msg.Method != "" {
				s.handle(r.Context(), msg.Method, msg.Params)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		reply := &Message{JSONRPC: "2.0", ID: msg.ID}
		res, err := s.handle(r.Context(), msg.Method, msg.Params)
		if e, ok := err.(*Error); ok {
			reply.Error = e
		} else if err != nil {
			reply.Error = &Error{Code: InternalError, Message: err.Error()}
		} else {
			reply.Result, _ = json.Marshal(res)
		}
		if msg.Method == "initialize" {
			b := make([]byte, 16)
			rand.Read(b)
			w.Header().Set(SessionHeader, hex.EncodeToString(b))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reply)
	case http.MethodGet:
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusMethodNotAllowed)
			return
		}
		ch := make(chan []byte, 16)
		s.mu.Lock()
		s.streams[ch] = true
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.streams, ch)
			s.mu.Unlock()
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case b := <-ch:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				fl.Flush()
			}
		}
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
