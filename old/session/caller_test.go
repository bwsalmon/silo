package session_test

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/gateway/audit"
	"github.com/bwsalmon/silo/old/mcp"
	"github.com/bwsalmon/silo/old/session"
)

// callerTools is a caller's MCP server for tests: echo, wait (sleeps
// ms milliseconds) and secret, which no allowlist here admits.
type callerTools struct{}

func (callerTools) ListTools() []mcp.ToolInfo {
	obj := map[string]any{"type": "object"}
	return []mcp.ToolInfo{{Name: "echo", InputSchema: obj}, {Name: "wait", InputSchema: obj}, {Name: "secret", InputSchema: obj}}
}

func (callerTools) CallTool(ctx context.Context, name string, args map[string]any) (mcp.Result, error) {
	switch name {
	case "echo":
		s, _ := args["text"].(string)
		return mcp.Result{Text: s}, nil
	case "wait":
		ms, _ := args["ms"].(float64)
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
			return mcp.Result{Text: "waited"}, nil
		case <-ctx.Done():
			return mcp.Result{}, ctx.Err()
		}
	case "secret":
		return mcp.Result{Text: "not for the agent"}, nil
	}
	return mcp.Result{}, &mcp.Error{Code: mcp.CodeInvalidParams, Message: "no tool " + name}
}

// callerHandler serves h as Streamable HTTP at /mcp, POST only, to
// requests bearing token.
func callerHandler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/mcp" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		resp := mcp.Dispatch(r.Context(), callerTools{}, body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	})
}

// serveCaller serves the caller server on a Unix socket and on loopback
// HTTP, and returns the socket's path and the HTTP URL.
func serveCaller(t *testing.T, token string) (sock, url string) {
	t.Helper()
	// A short directory: a socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "silo-caller")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "mcp.sock")
	for _, ln := range []struct{ network, addr string }{{"unix", sock}, {"tcp4", "127.0.0.1:0"}} {
		l, err := net.Listen(ln.network, ln.addr)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: callerHandler(token)}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
		if ln.network == "tcp4" {
			url = "http://" + l.Addr().String() + "/mcp"
		}
	}
	return sock, url
}

func readAudit(t *testing.T, e *env) []audit.Record {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(e.root, "state", "silo", "sessions", "s-*", "audit.jsonl"))
	if len(paths) != 1 {
		t.Fatalf("audit logs: %v", paths)
	}
	f, err := os.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []audit.Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "tok-") {
			t.Errorf("a caller token is in the audit log: %s", sc.Text())
		}
		var r audit.Record
		json.Unmarshal(sc.Bytes(), &r)
		out = append(out, r)
	}
	return out
}

// TestCallerMCPServers fronts a caller's servers, one on a Unix socket
// and one over HTTP, each with its own per-session token: their tools
// are listed under their names through the allowlist, a call is
// audited, and a blocking tool outlives the gateway's timeout.
func TestCallerMCPServers(t *testing.T) {
	defer session.SetMCPCallTimeout(300 * time.Millisecond)()
	e := newEnv(t)
	f := e.silofile(headless)
	sock, _ := serveCaller(t, "tok-unix-1234")
	_, url2 := serveCaller(t, "tok-http-5678")
	callers := []session.CallerMCP{
		{Name: "grain", URL: "unix:" + sock, Token: "tok-unix-1234", Tools: []string{"echo", "wait"}, Blocking: []string{"wait"}},
		{Name: "web", URL: url2, Token: "tok-http-5678", Tools: []string{"echo", "wait"}},
	}
	e.fake(fakeConfig{Call: "grain__wait", Args: map[string]any{"ms": 900}})
	res := e.run(f, func(o *session.Options) { o.CallerMCP = callers })
	if res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	r := e.report()
	if r.Error != "" || r.Result != "waited" {
		t.Fatalf("the blocking call: %+v", r)
	}
	got := strings.Join(r.Tools, ",")
	for _, want := range []string{"grain__echo", "grain__wait", "web__echo", "web__wait"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools %s lack %s", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("tools %s include one outside the allowlist", got)
	}
	var ok bool
	for _, rec := range readAudit(t, e) {
		if rec.Event == "end" && rec.Tool == "grain__wait" && rec.Outcome == audit.OutcomeOK {
			ok = true
		}
	}
	if !ok {
		t.Error("the audit log has no successful grain__wait")
	}
	if !strings.Contains(res.stderr, "Unix socket") || strings.Contains(res.stderr, "tok-") {
		t.Errorf("stderr:\n%s", res.stderr)
	}

	// The same wait on a tool that is not blocking times out.
	e = newEnv(t)
	f = e.silofile(headless)
	e.fake(fakeConfig{Call: "web__wait", Args: map[string]any{"ms": 900}})
	if res := e.run(f, func(o *session.Options) { o.CallerMCP = callers }); res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	if r := e.report(); !strings.Contains(r.Result, "timed out") {
		t.Errorf("a non-blocking wait: %+v", r)
	}
}

func TestCallerMCPWrongToken(t *testing.T) {
	e := newEnv(t)
	f := e.silofile(headless)
	sock, url := serveCaller(t, "tok-right-1234")
	for _, u := range []string{"unix:" + sock, url} {
		e.fake(fakeConfig{})
		res := e.run(f, func(o *session.Options) {
			o.CallerMCP = []session.CallerMCP{{Name: "grain", URL: u, Token: "tok-wrong-9999"}}
		})
		if res.code == 0 || res.err == nil || !strings.Contains(res.err.Error(), "401") {
			t.Errorf("%s: exit %d (%v), want a refusal naming HTTP 401", u, res.code, res.err)
		}
	}
}

func TestCallerMCPRefusesBadSpecs(t *testing.T) {
	e := newEnv(t)
	f := e.silofile(headless)
	for _, c := range []session.CallerMCP{
		{Name: "Grain", URL: "http://127.0.0.1:1/mcp"},
		{Name: "a__b", URL: "http://127.0.0.1:1/mcp"},
		{Name: "grain", URL: "ftp://example.com"},
		{Name: "grain", URL: "unix:relative.sock"},
		{Name: "grain", URL: "http://127.0.0.1:1/mcp", Tools: []string{"echo"}, Blocking: []string{"wait"}},
	} {
		e.fake(fakeConfig{})
		res := e.run(f, func(o *session.Options) { o.CallerMCP = []session.CallerMCP{c} })
		var se *session.Error
		if res.code != session.ExitUsage || !errors.As(res.err, &se) {
			t.Errorf("%+v: exit %d (%v), want 64", c, res.code, res.err)
		}
	}
}

// TestRenameMap lists a caller tool under another name: the agent sees
// and calls only the new name, and the audit log records both.
func TestRenameMap(t *testing.T) {
	e := newEnv(t)
	f := e.silofile(headless)
	sock, _ := serveCaller(t, "tok-unix-1234")
	e.fake(fakeConfig{Call: "say", Args: map[string]any{"text": "renamed"}})
	res := e.run(f, func(o *session.Options) {
		o.CallerMCP = []session.CallerMCP{{Name: "grain", URL: sock, Token: "tok-unix-1234", Tools: []string{"echo"}}}
		o.Rename = map[string]string{"grain__echo": "say"}
	})
	if res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	r := e.report()
	if tools := "," + strings.Join(r.Tools, ",") + ","; r.Result != "renamed" || !strings.Contains(tools, ",say,") || strings.Contains(tools, "grain__echo") {
		t.Fatalf("report: %+v", r)
	}
	var both int
	for _, rec := range readAudit(t, e) {
		if rec.Tool == "grain__echo" && rec.As == "say" {
			both++
		}
	}
	if both != 2 {
		t.Errorf("%d audit lines record both names, want the start and the end", both)
	}

	// Two tools under one name are refused.
	e.fake(fakeConfig{})
	res = e.run(f, func(o *session.Options) {
		o.CallerMCP = []session.CallerMCP{{Name: "grain", URL: sock, Token: "tok-unix-1234", Tools: []string{"echo", "wait"}}}
		o.Rename = map[string]string{"grain__echo": "x", "grain__wait": "x"}
	})
	if res.code != session.ExitUsage {
		t.Errorf("exit %d (%v), want 64 for a clash", res.code, res.err)
	}
}
