package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStdioConn(t *testing.T) {
	cr, sw := io.Pipe() // server to client
	sr, cw := io.Pipe() // client to server
	go func() {
		br := bufio.NewReader(sr)
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				sw.Close()
				return
			}
			if strings.Contains(string(line), `"echo"`) {
				// A server asking something of the client, a stray log
				// line, and a notification, before the answer.
				fmt.Fprintln(sw, `{"jsonrpc":"2.0","id":"s1","method":"ping"}`)
				fmt.Fprintln(sw, `not json`)
				fmt.Fprintln(sw, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
			}
			if resp := Dispatch(context.Background(), echoHandler{}, line); resp != nil {
				sw.Write(append(resp, '\n'))
			}
		}
	}()
	c := NewStdioConn(cr, cw)
	ctx := context.Background()
	if v, err := c.Initialize(ctx); err != nil || v != ProtocolVersion {
		t.Fatalf("Initialize = %q, %v", v, err)
	}
	r, err := c.CallTool(ctx, "echo", map[string]any{"text": "over stdio"})
	if err != nil || r.Text() != "over stdio" {
		t.Fatalf("CallTool = %+v, %v", r, err)
	}
	select {
	case <-c.Changed():
	case <-time.After(5 * time.Second):
		t.Fatal("no list_changed")
	}
	c.Close()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not done after Close")
	}
	if _, err := c.ListTools(ctx); err == nil {
		t.Fatal("a call after Close succeeded")
	}
}

func TestStdioConnServerExit(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go io.Copy(io.Discard, sr) // an OS pipe buffers; io.Pipe needs a reader
	c := NewStdioConn(cr, cw)
	errc := make(chan error, 1)
	go func() {
		_, err := c.ListTools(context.Background())
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	sw.Close()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "closed its stdout") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call in flight was not failed when the server went away")
	}
}

// sseServer is an MCP server on the older HTTP+SSE transport.
func sseServer(t *testing.T) *httptest.Server {
	msgs := make(chan []byte, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: endpoint\ndata: /messages?session=1\n\n")
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case m := <-msgs:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", m)
				fl.Flush()
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("session") != "1" {
			http.Error(w, "no session", http.StatusBadRequest)
			return
		}
		if resp := Dispatch(context.Background(), echoHandler{}, body); resp != nil {
			msgs <- resp
		}
		w.WriteHeader(http.StatusAccepted)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestDialHTTPFallsBackToSSE(t *testing.T) {
	s := sseServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := DialHTTP(ctx, s.URL+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	r, err := c.CallTool(ctx, "echo", map[string]any{"text": "over sse"})
	if err != nil || r.Text() != "over sse" {
		t.Fatalf("CallTool = %+v, %v", r, err)
	}
}

func TestDialHTTPStreamable(t *testing.T) {
	s := listen(t)
	ctx := context.Background()
	c, err := DialHTTP(ctx, s.URL, map[string]string{"Authorization": "Bearer " + s.Token})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s.NotifyToolsChanged()
	// The notification stream may open after the notification; send
	// until one arrives.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-c.Changed():
			return
		case <-deadline:
			t.Fatal("no list_changed over the notification stream")
		case <-time.After(50 * time.Millisecond):
			s.NotifyToolsChanged()
		}
	}
}
