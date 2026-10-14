// Adapted from github.com/bwsalmon/grain pkg/mcp/registry.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

// Package gateway is the session's MCP server (design §4): the one
// server the agent CLI's MCP configuration names. It holds the tools
// the Silofile declares, refuses any call outside them, bounds each call
// with a timeout, and audits every call, refused ones included, in
// audit.jsonl.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/silo/v2/gateway/audit"
	"github.com/bwsalmon/silo/v2/mcp"
)

// DefaultTimeout bounds a call to a tool that sets no timeout of its own
// (design §4.4: proxied MCP calls, 10 minutes by default).
const DefaultTimeout = 10 * time.Minute

// Tool is one tool the gateway serves.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Timeout bounds every call; DefaultTimeout when zero. A tool with a
	// timeout argument of its own bounds itself within it.
	Timeout time.Duration
	// Blocking lifts the timeout: a caller's tool that waits on a person
	// or a long job (grain.md §2.2's wait_for_checks, ask_question) runs
	// until it answers or the session ends.
	Blocking bool
	// Handler runs a call. A *Refusal error refuses the call with an MCP
	// error; any other error is the tool failing, answered as a result
	// with isError set.
	Handler func(ctx context.Context, args map[string]any) (mcp.Result, error)
}

// Refusal is a call the gateway will not run, and why.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// Refusef returns a *Refusal.
func Refusef(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// Gateway serves a set of tools. It is an mcp.Handler.
type Gateway struct {
	audit *audit.Log

	mu     sync.RWMutex
	tools  map[string]Tool
	order  []string
	server *mcp.Server
	// rename maps a tool's name to the name it is listed by; byAlias
	// the reverse (grain.md §2.2's per-session rename map).
	rename  map[string]string
	byAlias map[string]string
}

var _ mcp.Handler = (*Gateway)(nil)

// New returns a gateway auditing to log, which may be nil only in tests
// that are not about audit.
func New(log *audit.Log) *Gateway {
	return &Gateway{audit: log, tools: map[string]Tool{}}
}

// Register adds tools, or replaces ones of the same name in place, and
// tells connected clients the list changed.
func (g *Gateway) Register(tools ...Tool) { g.Update(nil, tools) }

// Update removes the named tools and adds or replaces tools, then tells
// connected clients the list changed: a proxied server's tool set
// changing (design §13.4).
func (g *Gateway) Update(remove []string, tools []Tool) {
	g.mu.Lock()
	if len(remove) > 0 {
		gone := map[string]bool{}
		for _, n := range remove {
			if _, ok := g.tools[n]; ok {
				gone[n] = true
				delete(g.tools, n)
			}
		}
		kept := g.order[:0]
		for _, n := range g.order {
			if !gone[n] {
				kept = append(kept, n)
			}
		}
		g.order = kept
	}
	for _, t := range tools {
		if _, ok := g.tools[t.Name]; !ok {
			g.order = append(g.order, t.Name)
		}
		g.tools[t.Name] = t
	}
	s := g.server
	g.mu.Unlock()
	if s != nil {
		s.NotifyToolsChanged()
	}
}

// SetRename makes the gateway list each tool named in m under the name
// m gives it, and answer calls by that name only (grain.md §2.2): a
// mechanism for a caller that wants its own names for silo's tools.
// The audit log records both names. Two tools may not end up listed
// under one name, and a renamed tool is no longer callable by its own.
func (g *Gateway) SetRename(m map[string]string) error {
	byAlias := map[string]string{}
	for from, to := range m {
		if to == "" {
			return fmt.Errorf("rename: %s is renamed to the empty name", from)
		}
		if other, ok := byAlias[to]; ok {
			return fmt.Errorf("rename: %s and %s are both renamed to %s", other, from, to)
		}
		byAlias[to] = from
	}
	g.mu.Lock()
	for to, from := range byAlias {
		if _, served := g.tools[to]; served && to != from {
			if _, renamed := m[to]; !renamed {
				g.mu.Unlock()
				return fmt.Errorf("rename: %s is renamed to %s, which another tool is already called", from, to)
			}
		}
	}
	g.rename, g.byAlias = m, byAlias
	s := g.server
	g.mu.Unlock()
	if s != nil {
		s.NotifyToolsChanged()
	}
	return nil
}

// listedName is the name tool name is listed by. g.mu is held.
func (g *Gateway) listedName(name string) string {
	if to, ok := g.rename[name]; ok {
		return to
	}
	return name
}

// lookup finds the tool a call names by its listed name. g.mu is held.
func (g *Gateway) lookup(listed string) (Tool, bool) {
	name := listed
	if from, ok := g.byAlias[listed]; ok {
		name = from
	} else if _, renamed := g.rename[listed]; renamed {
		return Tool{}, false
	}
	t, ok := g.tools[name]
	return t, ok
}

// ListTools answers tools/list, in registration order, under the names
// the rename map gives.
func (g *Gateway) ListTools() []mcp.ToolInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]mcp.ToolInfo, 0, len(g.order))
	for _, name := range g.order {
		t := g.tools[name]
		out = append(out, mcp.ToolInfo{Name: g.listedName(t.Name), Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// CallTool answers tools/call.
func (g *Gateway) CallTool(ctx context.Context, name string, args map[string]any) (mcp.Result, error) {
	g.mu.RLock()
	t, ok := g.lookup(name)
	names := make([]string, 0, len(g.order))
	for _, n := range g.order {
		names = append(names, g.listedName(n))
	}
	g.mu.RUnlock()
	tool := name
	if ok {
		tool = t.Name
	}
	var call *audit.Call
	if g.audit != nil {
		var err error
		// Nothing runs that is not on record.
		if call, err = g.audit.StartAs(tool, name, args); err != nil {
			return mcp.Result{}, &mcp.Error{Code: mcp.CodeInternalError, Message: "the call was not run: " + err.Error()}
		}
	}
	end := func(outcome, reason, text string) {
		if call != nil {
			_ = call.End(outcome, reason, text)
		}
	}
	refuse := func(reason string) (mcp.Result, error) {
		end(audit.OutcomeRefused, reason, "")
		return mcp.Result{}, &mcp.Error{Code: mcp.CodeInvalidParams, Message: reason}
	}

	if !ok {
		sort.Strings(names)
		return refuse(fmt.Sprintf("refused: tool %q is not served by this session's gateway, which serves only the tools its Silofile declares (%s)",
			name, strings.Join(names, ", ")))
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cancel := context.CancelFunc(func() {})
	if !t.Blocking {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	res, err := t.Handler(ctx, args)
	var refusal *Refusal
	switch {
	case errors.As(err, &refusal):
		return refuse("refused: " + refusal.Reason)
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded):
		msg := err.Error()
		if err == context.DeadlineExceeded {
			msg = fmt.Sprintf("%s timed out after %s", name, timeout)
		}
		res = withError(res, msg)
		end(audit.OutcomeTimeout, msg, res.Text)
	case err != nil:
		res = withError(res, err.Error())
		end(audit.OutcomeError, err.Error(), res.Text)
	case res.IsError:
		end(audit.OutcomeError, "", res.Text)
	default:
		end(audit.OutcomeOK, "", res.Text)
	}
	return res, nil
}

// withError marks res failed, putting msg before whatever it holds.
func withError(res mcp.Result, msg string) mcp.Result {
	if res.Text == "" {
		res.Text = msg
	} else {
		res.Text = msg + "\n" + res.Text
	}
	res.IsError = true
	return res
}

// Serve starts the gateway's endpoint on a loopback port (design §4.3).
// Calls run under ctx; Close the returned server to stop it.
func (g *Gateway) Serve(ctx context.Context) (*mcp.Server, error) {
	s, err := mcp.Listen(ctx, g)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.server = s
	g.mu.Unlock()
	return s, nil
}
