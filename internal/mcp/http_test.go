package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHTTPClientSSE drives the client against a server that answers
// with event streams, carrying a notification and a ping ahead of the
// response, and checks the headers the transport requires.
func TestHTTPClientSSE(t *testing.T) {
	var mu sync.Mutex
	var problems []string
	pinged := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "no stream", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			return
		}
		var msg Message
		json.NewDecoder(r.Body).Decode(&msg)
		mu.Lock()
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") || !strings.Contains(r.Header.Get("Accept"), "application/json") {
			problems = append(problems, msg.Method+": Accept "+r.Header.Get("Accept"))
		}
		if msg.Method != "initialize" && (r.Header.Get(SessionHeader) != "s-1" || r.Header.Get(ProtocolHeader) != "2025-03-26") {
			problems = append(problems, fmt.Sprintf("%s/%s: headers %v", msg.Method, msg.ID, r.Header))
		}
		mu.Unlock()
		if msg.Method == "" && string(msg.ID) == `"srv-ping"` { // our answer to the ping
			pinged <- struct{}{}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch msg.Method {
		case "initialize":
			w.Header().Set(SessionHeader, "s-1")
			result = `{"protocolVersion":"2025-03-26","capabilities":{}}`
		case "tools/list":
			result = `{"tools":[{"name":"a","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"called"}]}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": comment\n\n")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"srv-ping\",\"method\":\"ping\"}\n\n")
		// A response split over two data lines.
		fmt.Fprintf(w, "id: 7\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":%s,\"result\":%s}\n\n", msg.ID, result)
	}))
	defer srv.Close()

	changed := make(chan struct{}, 8)
	c := NewHTTPClient(srv.URL + "/mcp")
	c.OnToolsChanged = func() { changed <- struct{}{} }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Start(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "a" {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	res, err := c.CallTool(ctx, "a", nil)
	if err != nil || res.AllText() != "called" {
		t.Fatalf("call = %v, %v", res, err)
	}
	for _, ch := range []chan struct{}{changed, pinged} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("a server message was not handled")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Errorf("transport problems:\n%s", strings.Join(problems, "\n"))
	}
}

func TestHTTPClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, "http://example.com/mcp", http.StatusTemporaryRedirect)
		case r.URL.Path == "/html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html>")
		default:
			http.Error(w, "session not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	for path, want := range map[string]string{
		"/redirect": "HTTP 307",
		"/html":     `unexpected response content type "text/html"`,
		"/mcp":      "HTTP 404 Not Found: session not found",
	} {
		err := NewHTTPClient(srv.URL+path).Start(ctx, "test")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", path, err, want)
		}
	}
}

// TestServerHTTP runs the client against Server's own Streamable HTTP
// handler, including a tools/list_changed sent on the GET stream.
func TestServerHTTP(t *testing.T) {
	s := NewServer("t", "1")
	s.Add(Tool{Name: "echo", InputSchema: Schema(map[string]any{})}, func(_ context.Context, args json.RawMessage) *ToolResult {
		return Text("echo " + string(args))
	})
	srv := httptest.NewServer(s)
	defer srv.Close()
	changed := make(chan struct{}, 8)
	c := NewHTTPClient(srv.URL)
	c.OnToolsChanged = func() { changed <- struct{}{} }
	ctx := context.Background()
	if err := c.Start(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"x":1}`)); err != nil || res.AllText() != `echo {"x":1}` {
		t.Fatalf("call = %v, %v", res, err)
	}
	if res, _ := c.CallTool(ctx, "nope", nil); res == nil || !res.IsError {
		t.Errorf("unknown tool = %v", res)
	}
	// The GET stream opens asynchronously; notify until it is heard.
	deadline := time.After(5 * time.Second)
	for {
		s.NotifyToolsChanged()
		select {
		case <-changed:
			return
		case <-deadline:
			t.Fatal("tools/list_changed never arrived")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
