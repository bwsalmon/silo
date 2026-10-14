// Package mcptest is a small MCP server for tests of the gateway's proxy
// (design §13): it serves over stdio, as a server silo launches, or over
// Streamable HTTP, as a url: server, and its tool list can be changed
// while it runs.
//
// A test binary becomes the stdio server when it calls MaybeServe from
// TestMain and SILO_MCPTEST=stdio is in its environment, so a test
// launches it with its own os.Args[0] as the command.
package mcptest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/bwsalmon/silo/v2/mcp"
)

// Environment variables the stdio server reads.
const (
	// ModeVar set to stdio makes MaybeServe serve.
	ModeVar = "SILO_MCPTEST"
	// ToolsVar is a comma-separated list of the tools offered at start;
	// every tool when unset.
	ToolsVar = "SILO_MCPTEST_TOOLS"
	// HangVar set to 1 makes the server read requests and never answer.
	HangVar = "SILO_MCPTEST_HANG"
	// IgnoreTermVar set to 1 makes the server ignore SIGTERM.
	IgnoreTermVar = "SILO_MCPTEST_IGNORE_TERM"
)

// AllTools are the tools the server knows:
//
//   - echo {text}: returns text.
//   - env {name}: returns the server's value of the variable, or
//     "<unset>".
//   - cwd: returns the server's working directory.
//   - pid: returns the server's process id.
//   - spawn: starts a child that sleeps, in the server's process group,
//     and returns its pid.
//   - set_tools {names}: offers exactly the named tools from now on,
//     and sends notifications/tools/list_changed. Every known tool stays
//     callable.
var AllTools = []string{"echo", "env", "cwd", "pid", "spawn", "set_tools"}

// Server is the test server. It is an mcp.Handler.
type Server struct {
	mu      sync.Mutex
	offered []string
	// OnChange is called after set_tools changes the list.
	OnChange func()
}

// New returns a server offering tools, or every tool when nil.
func New(tools []string) *Server {
	if tools == nil {
		tools = AllTools
	}
	return &Server{offered: append([]string(nil), tools...)}
}

// SetTools changes the offered tools and calls OnChange.
func (s *Server) SetTools(names []string) {
	s.mu.Lock()
	s.offered = append([]string(nil), names...)
	fn := s.OnChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (s *Server) ListTools() []mcp.ToolInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mcp.ToolInfo, 0, len(s.offered))
	for _, n := range s.offered {
		out = append(out, mcp.ToolInfo{
			Name:        n,
			Description: "mcptest's " + n,
			InputSchema: map[string]any{"type": "object"},
		})
	}
	return out
}

func (s *Server) CallTool(_ context.Context, name string, args map[string]any) (mcp.Result, error) {
	switch name {
	case "echo":
		t, _ := args["text"].(string)
		return mcp.Result{Text: t}, nil
	case "env":
		n, _ := args["name"].(string)
		v, ok := os.LookupEnv(n)
		if !ok {
			v = "<unset>"
		}
		return mcp.Result{Text: v}, nil
	case "cwd":
		wd, err := os.Getwd()
		return mcp.Result{Text: wd}, err
	case "pid":
		return mcp.Result{Text: fmt.Sprint(os.Getpid())}, nil
	case "spawn":
		cmd := exec.Command("sleep", "300")
		if err := cmd.Start(); err != nil {
			return mcp.Result{}, err
		}
		go func() { _ = cmd.Wait() }()
		return mcp.Result{Text: fmt.Sprint(cmd.Process.Pid)}, nil
	case "set_tools":
		var names []string
		list, _ := args["names"].([]any)
		for _, n := range list {
			if s, ok := n.(string); ok {
				names = append(names, s)
			}
		}
		sort.Strings(names)
		s.SetTools(names)
		return mcp.Result{Text: strings.Join(names, ",")}, nil
	}
	return mcp.Result{}, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "no tool " + name}
}

// Listen serves s over Streamable HTTP on loopback, sending
// tools/list_changed on its notification stream when the list changes.
func (s *Server) Listen(ctx context.Context) (*mcp.Server, error) {
	srv, err := mcp.Listen(ctx, s)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.OnChange = srv.NotifyToolsChanged
	s.mu.Unlock()
	return srv, nil
}

// ServeStdio serves s over newline-delimited JSON-RPC until r ends.
func (s *Server) ServeStdio(r io.Reader, w io.Writer) {
	var wmu sync.Mutex
	write := func(b []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = w.Write(append(b, '\n'))
	}
	s.mu.Lock()
	s.OnChange = func() { write([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)) }
	s.mu.Unlock()
	hang := os.Getenv(HangVar) == "1"
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && !hang {
			if resp := mcp.Dispatch(context.Background(), s, line); resp != nil {
				write(resp)
			}
		}
		if err != nil {
			return
		}
	}
}

// MaybeServe serves over stdio and exits when ModeVar is stdio, and
// otherwise returns. Call it first thing in TestMain.
func MaybeServe() {
	if os.Getenv(ModeVar) != "stdio" {
		return
	}
	if os.Getenv(IgnoreTermVar) == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	var tools []string
	if t, ok := os.LookupEnv(ToolsVar); ok {
		tools = strings.Split(t, ",")
	}
	New(tools).ServeStdio(os.Stdin, os.Stdout)
	os.Exit(0)
}
