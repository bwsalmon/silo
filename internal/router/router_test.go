package router

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/mcp"
)

// fakeSet is an in-process tool set whose tools answer with their own
// name.
type fakeSet struct {
	names  []string
	closed bool
}

func (f *fakeSet) Tools(context.Context) ([]mcp.Tool, error) {
	var out []mcp.Tool
	for _, n := range f.names {
		out = append(out, mcp.Tool{Name: n, Description: "tool " + n})
	}
	return out, nil
}

func (f *fakeSet) Call(_ context.Context, tool string, args json.RawMessage) (*mcp.ToolResult, error) {
	return mcp.Text(tool + " " + string(args)), nil
}

func (f *fakeSet) Close() error { f.closed = true; return nil }

// client connects an MCP client to the router and records the
// notifications it gets.
type client struct {
	conn   *mcp.Conn
	notify chan string
}

func connect(t *testing.T, rt *Router) *client {
	t.Helper()
	toRouter, fromClient := io.Pipe()
	toClient, fromRouter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); fromClient.Close(); fromRouter.Close() })
	go rt.Serve(ctx, toRouter, fromRouter)
	c := &client{notify: make(chan string, 16)}
	c.conn = mcp.NewConn(toClient, fromClient, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		c.notify <- method
		return nil, nil
	})
	go c.conn.Run(ctx)
	var init struct {
		Capabilities struct {
			Tools struct {
				ListChanged bool `json:"listChanged"`
			} `json:"tools"`
		} `json:"capabilities"`
	}
	if err := c.conn.Call(ctx, "initialize", map[string]any{"protocolVersion": mcp.ProtocolVersion}, &init); err != nil {
		t.Fatal(err)
	}
	if !init.Capabilities.Tools.ListChanged {
		t.Error("router does not advertise tools.listChanged")
	}
	return c
}

func (c *client) tools(t *testing.T) []string {
	t.Helper()
	var res struct{ Tools []mcp.Tool }
	if err := c.conn.Call(context.Background(), "tools/list", nil, &res); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func (c *client) call(t *testing.T, name string, args any) *mcp.ToolResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	var res mcp.ToolResult
	if err := c.conn.Call(context.Background(), "tools/call", mcp.CallParams{Name: name, Arguments: raw}, &res); err != nil {
		t.Fatal(err)
	}
	return &res
}

func (c *client) waitListChanged(t *testing.T) {
	t.Helper()
	select {
	case m := <-c.notify:
		if m != "notifications/tools/list_changed" {
			t.Fatalf("got %s", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed")
	}
}

func TestAggregationAndNamespacing(t *testing.T) {
	ctx := context.Background()
	log := &audit.Memory{}
	rt := New(approval.NewFake(), log)
	rt.Add(ctx, "repo-read", &fakeSet{names: []string{"read_file", "grep"}}, Options{Origin: Builtin})
	rt.Add(ctx, "scratch", &fakeSet{names: []string{"define"}}, Options{Origin: Builtin})
	rt.Add(ctx, "scratch.tmp", &fakeSet{names: []string{"echo"}}, Options{Origin: Scratch})
	rt.Add(ctx, "tickets", &fakeSet{names: []string{"search"}}, Options{Origin: Repo})
	c := connect(t, rt)

	got := strings.Join(c.tools(t), " ")
	want := "repo-read.grep repo-read.read_file scratch.define scratch.tmp.echo tickets.search"
	if got != want {
		t.Errorf("tools = %q, want %q", got, want)
	}
	for name, want := range map[string]string{
		"tickets.search":   `search {"q":1}`,
		"scratch.tmp.echo": `echo {"q":1}`,
		"scratch.define":   `define {"q":1}`,
	} {
		if res := c.call(t, name, map[string]int{"q": 1}); res.IsError || res.AllText() != want {
			t.Errorf("%s = %q", name, res.AllText())
		}
	}
	for _, bad := range []string{"tickets.nope", "nope.search", "search", "scratch.tmp"} {
		if res := c.call(t, bad, nil); !res.IsError {
			t.Errorf("%s routed: %q", bad, res.AllText())
		}
	}
	if n := log.Count("tool_call"); n != 7 {
		t.Errorf("logged %d tool calls, want 7", n)
	}
}

func TestListChangedOnAddReplaceRemove(t *testing.T) {
	ctx := context.Background()
	rt := New(approval.NewFake(), audit.Discard{})
	c := connect(t, rt)
	first := &fakeSet{names: []string{"a"}}
	rt.Add(ctx, "tickets", first, Options{Origin: Repo})
	c.waitListChanged(t)
	if got := c.tools(t); len(got) != 1 || got[0] != "tickets.a" {
		t.Errorf("tools = %v", got)
	}
	rt.Add(ctx, "tickets", &fakeSet{names: []string{"b"}}, Options{Origin: Repo})
	c.waitListChanged(t)
	if !first.closed {
		t.Error("replaced set not closed")
	}
	if got := c.tools(t); len(got) != 1 || got[0] != "tickets.b" {
		t.Errorf("tools = %v", got)
	}
	rt.Remove("tickets")
	c.waitListChanged(t)
	if got := c.tools(t); len(got) != 0 {
		t.Errorf("tools = %v", got)
	}
}

func TestRequiresApproval(t *testing.T) {
	ctx := context.Background()
	fake := approval.NewFake(approval.Decision{Reason: "not that ticket"}, approval.Decision{Approved: true})
	rt := New(fake, audit.Discard{})
	rt.Add(ctx, "tickets", &fakeSet{names: []string{"search", "create"}}, Options{Origin: Repo, RequiresApproval: []string{"create"}})

	if res := rt.Call(ctx, "tickets.search", nil); res.IsError || fake.Asked() != 0 {
		t.Fatalf("search: %q, asked %d", res.AllText(), fake.Asked())
	}
	res := rt.Call(ctx, "tickets.create", json.RawMessage(`{"title":"x"}`))
	if !res.IsError || !strings.Contains(res.AllText(), "not that ticket") {
		t.Errorf("rejected create = %q", res.AllText())
	}
	if req := fake.Requests[0]; req.Kind != "tool-call" || !strings.Contains(req.Body, `"title": "x"`) {
		t.Errorf("request = %+v", req)
	}
	if res := rt.Call(ctx, "tickets.create", json.RawMessage(`{"title":"x"}`)); res.IsError {
		t.Errorf("approved create = %q", res.AllText())
	}
	for _, tl := range rt.Tools() {
		if tl.Name == "tickets.create" && !strings.Contains(tl.Description, "approval") {
			t.Errorf("description does not mention approval: %q", tl.Description)
		}
	}
}

func TestChangedToolsets(t *testing.T) {
	got := ChangedToolsets([]string{".silo/tools/b/main.go", "README.md", ".silo/tools/a/toolset.yaml", ".silo/tools/b/x", ".silo/prompt.md"})
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("got %v", got)
	}
}
