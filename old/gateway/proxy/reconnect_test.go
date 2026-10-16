package proxy

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/silo/old/internal/mcptest"
	"github.com/bwsalmon/silo/old/mcp"
)

// serveUnix serves the test server's tools on a Unix socket at path,
// as Streamable HTTP at /mcp to requests bearing token, until the
// returned function stops it.
func serveUnix(t *testing.T, path, token string) (stop func()) {
	t.Helper()
	ts := mcptest.New(nil)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		resp := mcp.Dispatch(r.Context(), ts, body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	})}
	go srv.Serve(l)
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			srv.Close()
		}
	}
	t.Cleanup(stop)
	return stop
}

// TestReconnectingServer is a caller's server that goes away and comes
// back on the same socket path (grain.md §2.7): calls made while it is
// gone fail as retryable, and calls after it is back succeed.
func TestReconnectingServer(t *testing.T) {
	dir, err := os.MkdirTemp("", "silo-rc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "mcp.sock")
	stop := serveUnix(t, sock, "tok")
	var w warnings
	sp := Spec{Name: "grain", URL: mcp.UnixURL, HTTP: mcp.UnixClient(sock), Headers: map[string]string{"Authorization": "Bearer tok"},
		Allow: []string{"echo"}, Required: true, Reconnect: true}
	_, tools, err := start(t, Options{Warn: w.warn}, sp)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := call(t, tools, "grain__echo", map[string]any{"text": "one"}); err != nil || r.Text != "one" {
		t.Fatalf("echo = %+v, %v", r, err)
	}

	stop()
	for i := 0; i < 2; i++ {
		_, err := call(t, tools, "grain__echo", map[string]any{"text": "gone"})
		var ue *UnavailableError
		if !errors.As(err, &ue) || !ue.Retryable() {
			t.Fatalf("call %d while the server is gone: %v, want an UnavailableError", i, err)
		}
	}

	serveUnix(t, sock, "tok")
	if r, err := call(t, tools, "grain__echo", map[string]any{"text": "back"}); err != nil || r.Text != "back" {
		t.Fatalf("echo once the server is back = %+v, %v\nwarnings:\n%s", r, err, w.String())
	}
	if r, err := call(t, tools, "grain__echo", map[string]any{"text": "again"}); err != nil || r.Text != "again" {
		t.Fatalf("a second echo = %+v, %v", r, err)
	}
}

// TestNoReconnectWithoutTheSpec keeps a url: server's failure as it
// was: an error that is not retryable.
func TestNoReconnectWithoutTheSpec(t *testing.T) {
	dir, err := os.MkdirTemp("", "silo-rc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "mcp.sock")
	stop := serveUnix(t, sock, "tok")
	sp := Spec{Name: "u", URL: mcp.UnixURL, HTTP: mcp.UnixClient(sock), Headers: map[string]string{"Authorization": "Bearer tok"}, Required: true}
	_, tools, err := start(t, Options{}, sp)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	_, err = call(t, tools, "u__echo", map[string]any{"text": "gone"})
	var ue *UnavailableError
	if err == nil || errors.As(err, &ue) {
		t.Fatalf("call to a gone server: %v", err)
	}
}
