// Adapted from github.com/bwsalmon/grain pkg/mcp/managed_http_test.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type echoHandler struct{}

func (echoHandler) ListTools() []ToolInfo {
	return []ToolInfo{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}
}

func (echoHandler) CallTool(_ context.Context, name string, args map[string]any) (Result, error) {
	if name != "echo" {
		return Result{}, &Error{Code: CodeInvalidParams, Message: "no tool " + name}
	}
	s, _ := args["text"].(string)
	return Result{Text: s}, nil
}

func listen(t *testing.T) *Server {
	t.Helper()
	s, err := Listen(context.Background(), echoHandler{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestServerRoundTrip(t *testing.T) {
	s := listen(t)
	if !strings.HasPrefix(s.URL, "http://127.0.0.1:") || len(s.Token) != 64 {
		t.Fatalf("URL %q, token of %d hex digits: want loopback and 256 bits", s.URL, len(s.Token))
	}
	ctx := context.Background()
	c := NewClient(s.URL, s.Token)
	v, err := c.Initialize(ctx)
	if err != nil || v != ProtocolVersion {
		t.Fatalf("Initialize = %q, %v", v, err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	r, err := c.CallTool(ctx, "echo", map[string]any{"text": "hi"})
	if err != nil || r.Text() != "hi" || r.IsError {
		t.Fatalf("CallTool = %+v, %v", r, err)
	}
	_, err = c.CallTool(ctx, "nope", nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams || !strings.Contains(rpcErr.Message, "no tool nope") {
		t.Fatalf("CallTool(nope) error = %v, want the handler's refusal", err)
	}
	if _, err := c.Call(ctx, "resources/list", nil); !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("unknown method error = %v", err)
	}
}

func TestServerRefusesWithoutAuthOrWithForeignHostOrOrigin(t *testing.T) {
	s := listen(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"ok", func(*http.Request) {}, http.StatusOK},
		{"no token", func(r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("0", 64)) }, http.StatusUnauthorized},
		{"token not as bearer", func(r *http.Request) { r.Header.Set("Authorization", s.Token) }, http.StatusUnauthorized},
		{"rebinding host", func(r *http.Request) { r.Host = strings.Replace(r.Host, "127.0.0.1", "evil.example", 1) }, http.StatusForbidden},
		{"localhost host", func(r *http.Request) { r.Host = strings.Replace(r.Host, "127.0.0.1", "localhost", 1) }, http.StatusForbidden},
		{"any origin", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1") }, http.StatusForbidden},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden},
		{"wrong path", func(r *http.Request) { r.URL.Path = "/other" }, http.StatusNotFound},
		{"not json", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, s.URL, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+s.Token)
			tc.mutate(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestNotificationsCarryToolsListChanged(t *testing.T) {
	s := listen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := NewClient(s.URL, s.Token).Notifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The stream is registered once the server has sent its headers,
	// which Notifications waited for.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.streams)
		s.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.NotifyToolsChanged()
	select {
	case m := <-ch:
		if m != "notifications/tools/list_changed" {
			t.Fatalf("notification %q", m)
		}
	case <-ctx.Done():
		t.Fatal("no notification")
	}
	if _, err := NewClient(s.URL, "").Notifications(ctx); err == nil {
		t.Fatal("notification stream opened without the token")
	}
}

func TestDispatchNotificationHasNoResponse(t *testing.T) {
	if r := Dispatch(context.Background(), echoHandler{}, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); r != nil {
		t.Fatalf("response %s to a notification", r)
	}
	r := Dispatch(context.Background(), echoHandler{}, []byte(`{not json`))
	if !strings.Contains(string(r), `"id":null`) || !strings.Contains(string(r), "-32700") {
		t.Fatalf("parse error response %s", r)
	}
}
