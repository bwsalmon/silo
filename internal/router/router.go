// Package router is silo's MCP router: the one MCP server the agent CLI
// connects to. It fronts every tool set -- the built-ins and each repo or
// scratch tool set's child server -- under namespaced tool names
// (<toolset>.<tool>), routes calls, asks for approval where a manifest
// requires it, logs every call, and tells connected clients when the
// tool list changes.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/mcp"
)

// Sep joins a tool set's name and a tool's name.
const Sep = "."

// Toolset is one tool set behind the router. Tool names are its own,
// without the namespace.
type Toolset interface {
	Tools(ctx context.Context) ([]mcp.Tool, error)
	Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.ToolResult, error)
	Close() error
}

// Origin says where a tool set came from.
type Origin string

// Origins.
const (
	Builtin Origin = "builtin"
	Repo    Origin = "repo"
	Scratch Origin = "scratch"
)

// Options describe a tool set being added.
type Options struct {
	Origin Origin
	// RequiresApproval names tools whose every call the person approves.
	RequiresApproval []string
}

type entry struct {
	name     string
	set      Toolset
	opts     Options
	tools    []mcp.Tool
	approval map[string]bool
}

// Router aggregates tool sets.
type Router struct {
	approver approval.Approver
	log      audit.Logger
	// Instructions go out in the initialize result.
	Instructions string

	mu    sync.RWMutex
	sets  map[string]*entry
	conns map[*mcp.Conn]bool
}

// New returns an empty router.
func New(approver approval.Approver, log audit.Logger) *Router {
	return &Router{approver: approver, log: log, sets: map[string]*entry{}, conns: map[*mcp.Conn]bool{}}
}

// Add puts a tool set behind the router under name, replacing (and
// closing) any set already there, and tells clients the list changed.
func (r *Router) Add(ctx context.Context, name string, set Toolset, opts Options) error {
	tools, err := set.Tools(ctx)
	if err != nil {
		return fmt.Errorf("list %s's tools: %w", name, err)
	}
	e := &entry{name: name, set: set, opts: opts, tools: tools, approval: map[string]bool{}}
	for _, t := range opts.RequiresApproval {
		e.approval[t] = true
	}
	r.mu.Lock()
	old := r.sets[name]
	r.sets[name] = e
	r.mu.Unlock()
	if old != nil && old.set != set {
		old.set.Close()
	}
	r.log.Log("toolset_loaded", map[string]any{"toolset": name, "origin": string(opts.Origin), "tools": len(tools), "replaced": old != nil})
	r.listChanged()
	return nil
}

// Remove closes and drops a tool set. It reports whether one was there.
func (r *Router) Remove(name string) bool {
	r.mu.Lock()
	old := r.sets[name]
	delete(r.sets, name)
	r.mu.Unlock()
	if old == nil {
		return false
	}
	old.set.Close()
	r.log.Log("toolset_unloaded", map[string]any{"toolset": name})
	r.listChanged()
	return true
}

// Refresh re-reads a set's tool list (after its server said it changed).
func (r *Router) Refresh(ctx context.Context, name string) error {
	r.mu.RLock()
	e := r.sets[name]
	r.mu.RUnlock()
	if e == nil {
		return nil
	}
	tools, err := e.set.Tools(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	e.tools = tools
	r.mu.Unlock()
	r.listChanged()
	return nil
}

// Has reports whether a tool set is loaded.
func (r *Router) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sets[name] != nil
}

// Toolsets returns the loaded set names, sorted.
func (r *Router) Toolsets() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for n := range r.sets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Tools returns every tool, namespaced, sorted by name.
func (r *Router) Tools() []mcp.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []mcp.Tool
	for name, e := range r.sets {
		for _, t := range e.tools {
			t.Name = name + Sep + t.Name
			if len(t.InputSchema) == 0 {
				t.InputSchema = json.RawMessage(`{"type":"object"}`)
			}
			if e.approval[strings.TrimPrefix(t.Name, name+Sep)] {
				t.Description = strings.TrimSpace(t.Description + " (Each call needs the person's approval.)")
			}
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// resolve finds the set a namespaced tool belongs to: the longest set
// name that prefixes it, so scratch.<name>.<tool> reaches the scratch set
// <name> rather than the scratch built-in.
func (r *Router) resolve(full string) (*entry, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best *entry
	for name, e := range r.sets {
		if strings.HasPrefix(full, name+Sep) && (best == nil || len(name) > len(best.name)) {
			best = e
		}
	}
	if best == nil {
		return nil, ""
	}
	tool := strings.TrimPrefix(full, best.name+Sep)
	for _, t := range best.tools {
		if t.Name == tool {
			return best, tool
		}
	}
	return nil, ""
}

// Call routes one namespaced tool call, logging it.
func (r *Router) Call(ctx context.Context, full string, args json.RawMessage) *mcp.ToolResult {
	start := time.Now()
	res := r.call(ctx, full, args)
	r.log.Log("tool_call", map[string]any{
		"tool":        full,
		"arguments":   clip(string(args), 64*1024),
		"is_error":    res.IsError,
		"result":      clip(res.AllText(), 4*1024),
		"duration_ms": time.Since(start).Milliseconds(),
	})
	return res
}

func (r *Router) call(ctx context.Context, full string, args json.RawMessage) *mcp.ToolResult {
	e, tool := r.resolve(full)
	if e == nil {
		return mcp.Errorf("no tool %q in this session", full)
	}
	if e.approval[tool] {
		d := r.approver.Approve(ctx, approval.Request{
			Kind:  "tool-call",
			Title: fmt.Sprintf("The agent wants to call %s", full),
			Body:  prettyJSON(args),
		})
		if !d.Approved {
			return mcp.Errorf("the person rejected this call to %s: %s", full, d.Reason)
		}
	}
	res, err := e.set.Call(ctx, tool, args)
	if err != nil {
		return mcp.Errorf("%s: %v", full, err)
	}
	if res == nil {
		return mcp.Errorf("%s returned no result", full)
	}
	return res
}

// Serve answers one MCP client (the agent CLI, through silo's bridge)
// on rw until it disconnects.
func (r *Router) Serve(ctx context.Context, rd io.Reader, w io.Writer) error {
	var conn *mcp.Conn
	conn = mcp.NewConn(rd, w, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return r.handle(ctx, conn, method, params)
	})
	defer func() {
		r.mu.Lock()
		delete(r.conns, conn)
		r.mu.Unlock()
	}()
	return conn.Run(ctx)
}

func (r *Router) handle(ctx context.Context, conn *mcp.Conn, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      any    `json:"clientInfo"`
		}
		json.Unmarshal(params, &p)
		version := p.ProtocolVersion
		if version == "" {
			version = mcp.ProtocolVersion
		}
		r.mu.Lock()
		r.conns[conn] = true
		r.mu.Unlock()
		r.log.Log("client_connected", map[string]any{"client": p.ClientInfo, "protocol": version})
		res := map[string]any{
			"protocolVersion": version,
			"serverInfo":      map[string]string{"name": "silo", "version": "1"},
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		}
		if r.Instructions != "" {
			res["instructions"] = r.Instructions
		}
		return res, nil
	case "ping", "logging/setLevel":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": r.Tools()}, nil
	case "tools/call":
		var p mcp.CallParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &mcp.Error{Code: mcp.InvalidParams, Message: err.Error()}
		}
		return r.Call(ctx, p.Name, p.Arguments), nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	}
	if strings.HasPrefix(method, "notifications/") {
		return nil, nil
	}
	return nil, &mcp.Error{Code: mcp.MethodNotFound, Message: "method not found: " + method}
}

// listChanged sends notifications/tools/list_changed to every client.
func (r *Router) listChanged() {
	r.mu.RLock()
	conns := make([]*mcp.Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.RUnlock()
	for _, c := range conns {
		c.Notify("notifications/tools/list_changed", nil)
	}
	if len(conns) > 0 {
		r.log.Log("tools_list_changed", map[string]any{"clients": len(conns)})
	}
}

// Close closes every tool set.
func (r *Router) Close() {
	r.mu.Lock()
	sets := r.sets
	r.sets = map[string]*entry{}
	r.mu.Unlock()
	for _, e := range sets {
		e.set.Close()
	}
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... [%d bytes clipped]", len(s)-n)
}
