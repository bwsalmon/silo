package toolchan

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A channel straight to a Hub, with no gateway between: up, a call
// down it, and back after a drop with backoff.
func TestChannel(t *testing.T) {
	var connects, disconnects atomic.Int32
	hub := &Hub{PingEvery: 50 * time.Millisecond,
		Connected:    func(context.Context, string) error { connects.Add(1); return nil },
		Disconnected: func(context.Context, string) error { disconnects.Add(1); return nil }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hub.Accept(w, r, "ag1") }))
	defer srv.Close()
	tools := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{URL: srv.URL + "/attach", Handler: tools, MinBackoff: 10 * time.Millisecond, Logf: t.Logf}
	go c.Run(ctx)
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	if err := hub.WaitAttached(wctx, "ag1"); err != nil {
		t.Fatal(err)
	}
	if got := string(hub.MCP(ctx, "ag1", []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))); !strings.Contains(got, `"ok":true`) {
		t.Errorf("a call: %s", got)
	}
	if got := hub.MCP(ctx, "ag1", []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); got != nil {
		t.Errorf("a notification answered %s", got)
	}
	hub.Close("ag1")
	for i := 0; i < 500 && (disconnects.Load() < 1 || connects.Load() < 2 || !hub.Attached("ag1")); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if disconnects.Load() < 1 || connects.Load() < 2 || !hub.Attached("ag1") {
		t.Fatalf("after a drop: %d connects, %d disconnects, attached %v", connects.Load(), disconnects.Load(), hub.Attached("ag1"))
	}
	if got := string(hub.MCP(ctx, "nobody", []byte(`{"jsonrpc":"2.0","id":"x","method":"ping"}`))); !strings.Contains(got, `"retryable":true`) || !strings.Contains(got, `"id":"x"`) {
		t.Errorf("a call with no channel: %s", got)
	}
}

// An attach that isn't upgrading is refused, a 403 is ErrRefused, and
// a redirect loop ends.
func TestAttachRefusals(t *testing.T) {
	hub := &Hub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/forbidden":
			http.Error(w, "no agent", http.StatusForbidden)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
		default:
			hub.Accept(w, r, "ag1")
		}
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a plain GET: %d", resp.StatusCode)
	}
	ctx := context.Background()
	if up, err := (&Client{URL: srv.URL + "/forbidden"}).Once(ctx); up || !errors.Is(err, ErrRefused) {
		t.Errorf("403: %v %v", up, err)
	}
	if up, err := (&Client{URL: srv.URL + "/loop"}).Once(ctx); up || err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("a redirect loop: %v %v", up, err)
	}
	if up, err := (&Client{URL: "https://silo.internal/x"}).Once(ctx); up || err == nil {
		t.Errorf("https: %v %v", up, err)
	}
}
