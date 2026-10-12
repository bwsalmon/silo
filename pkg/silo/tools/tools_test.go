package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/silo/agents"
)

func newServer(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if opts.Dir == "" {
		opts.Dir = dir
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func run(t *testing.T, s *Server, name string, args map[string]any) Result {
	t.Helper()
	// Through JSON, as a call arrives.
	b, _ := json.Marshal(args)
	var a map[string]any
	json.Unmarshal(b, &a)
	return s.Call(context.Background(), name, a)
}

// The version silod's startup script wants is the one silo-tools says.
func TestVersion(t *testing.T) {
	if Version != agents.ToolsVersion {
		t.Errorf("silo-tools is %s; the startup script fetches it unless it is %s", Version, agents.ToolsVersion)
	}
}

func TestExec(t *testing.T) {
	s, dir := newServer(t, Options{})
	r := run(t, s, "exec", map[string]any{"command": "pwd; echo out; echo err >&2; exit 3"})
	if !r.IsError || !strings.HasPrefix(r.Text, "exit=3\n") || !strings.Contains(r.Text, "stdout:\n"+dir+"\nout\n") || !strings.Contains(r.Text, "stderr:\nerr\n") {
		t.Errorf("exec: %+v", r)
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	if r := run(t, s, "exec", map[string]any{"command": "pwd", "workdir": sub}); r.IsError || !strings.Contains(r.Text, sub) {
		t.Errorf("exec in a workdir: %+v", r)
	}
	// bash -l: a login shell.
	if r := run(t, s, "exec", map[string]any{"command": "shopt -q login_shell && echo login"}); !strings.Contains(r.Text, "login") {
		t.Errorf("not a login shell: %+v", r)
	}
	start := time.Now()
	r = run(t, s, "exec", map[string]any{"command": "sleep 30", "timeout_seconds": 1})
	if !r.IsError || !strings.Contains(r.Text, "killed after the timeout") || time.Since(start) > 15*time.Second {
		t.Errorf("timeout: %+v after %s", r, time.Since(start))
	}
}

// exec's environment carries the egress proxy (call 41), and nothing
// of silo-tools' own.
func TestExecEnv(t *testing.T) {
	t.Setenv("SILO_TOOLS_SECRET", "x")
	s, _ := newServer(t, Options{Proxy: "http://silo.internal:3128", NoProxy: "localhost,silo.internal"})
	r := run(t, s, "exec", map[string]any{"command": "env"})
	for _, want := range []string{"HTTPS_PROXY=http://silo.internal:3128", "HTTP_PROXY=http://silo.internal:3128",
		"https_proxy=http://silo.internal:3128", "NO_PROXY=localhost,silo.internal", "HOME="} {
		if !strings.Contains(r.Text, want+"") {
			t.Errorf("env lacks %s:\n%s", want, r.Text)
		}
	}
	if strings.Contains(r.Text, "SILO_TOOLS_SECRET") {
		t.Errorf("silo-tools' own environment leaked into the command")
	}
}

func TestExecCaps(t *testing.T) {
	s, _ := newServer(t, Options{})
	r := run(t, s, "exec", map[string]any{"command": "head -c 200000 /dev/zero | tr '\\0' a; head -c 100000 /dev/zero | tr '\\0' b >&2"})
	out, errOut, _ := strings.Cut(r.Text, "\nstderr:\n")
	if !strings.Contains(out, strings.Repeat("a", MaxOutput)+"\n") || strings.Contains(out, strings.Repeat("a", MaxOutput+1)) {
		t.Errorf("stdout didn't keep exactly %d bytes", MaxOutput)
	}
	if !strings.Contains(out, "134464 more bytes cut") {
		t.Errorf("stdout's cut isn't said: %q", out[len(out)-100:])
	}
	if !strings.Contains(errOut, strings.Repeat("b", MaxOutput)+"\n") || strings.Contains(errOut, strings.Repeat("b", MaxOutput+1)) || !strings.Contains(errOut, "34464 more bytes cut") {
		t.Errorf("stderr: %q", errOut[len(errOut)-100:])
	}
}

// A call interrupted mid-command leaves the command running, and its
// result is fetched by call_id, for ten minutes.
func TestExecResultKept(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	s, dir := newServer(t, Options{Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	marker := filepath.Join(dir, "ran")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := s.Call(ctx, "exec", map[string]any{"command": "sleep 0.5; echo once >> " + marker + "; echo finished", "call_id": "c1"})
	if !r.IsError || !strings.Contains(r.Text, `"c1"`) {
		t.Fatalf("an interrupted call: %+v", r)
	}
	r = run(t, s, "exec", map[string]any{"command": "echo never", "call_id": "c1"})
	if r.IsError || !strings.Contains(r.Text, "finished") {
		t.Errorf("fetched by call_id: %+v", r)
	}
	r = run(t, s, "exec", map[string]any{"command": "echo never", "call_id": "c1"})
	if !strings.Contains(r.Text, "finished") {
		t.Errorf("fetched again: %+v", r)
	}
	if b, _ := os.ReadFile(marker); string(b) != "once\n" {
		t.Errorf("the command ran %q", b)
	}
	mu.Lock()
	now = now.Add(ResultTTL + time.Second)
	mu.Unlock()
	if r := run(t, s, "exec", map[string]any{"command": "echo anew", "call_id": "c1"}); !strings.Contains(r.Text, "anew") {
		t.Errorf("after ten minutes the id is free again: %+v", r)
	}
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	s, _ := newServer(t, Options{Dir: dir})
	p := filepath.Join(dir, "a", "b", "f.txt")
	if r := run(t, s, "write_file", map[string]any{"file_path": p, "content": "one\ntwo\nthree\ntwo\n"}); r.IsError {
		t.Fatalf("write_file: %+v", r)
	}
	if r := run(t, s, "read_file", map[string]any{"file_path": p}); r.Text != "     1\tone\n     2\ttwo\n     3\tthree\n     4\ttwo" {
		t.Errorf("read_file: %q", r.Text)
	}
	if r := run(t, s, "read_file", map[string]any{"file_path": p, "offset": 1, "limit": 2}); r.Text != "     2\ttwo\n     3\tthree" {
		t.Errorf("read_file range: %q", r.Text)
	}
	// A relative path is from the user's home.
	if r := run(t, s, "read_file", map[string]any{"file_path": "a/b/f.txt", "limit": 1}); r.Text != "     1\tone" {
		t.Errorf("read_file relative: %q", r.Text)
	}
	if r := run(t, s, "read_file", map[string]any{"file_path": filepath.Join(dir, "nope")}); !r.IsError {
		t.Errorf("read_file of nothing: %+v", r)
	}
	if r := run(t, s, "edit_file", map[string]any{"file_path": p, "old_string": "two", "new_string": "2"}); !r.IsError || !strings.Contains(r.Text, "2 times") {
		t.Errorf("an ambiguous edit: %+v", r)
	}
	if r := run(t, s, "edit_file", map[string]any{"file_path": p, "old_string": "four", "new_string": "4"}); !r.IsError || !strings.Contains(r.Text, "not found") {
		t.Errorf("an edit of nothing: %+v", r)
	}
	if r := run(t, s, "edit_file", map[string]any{"file_path": p, "old_string": "", "new_string": "4"}); !r.IsError {
		t.Errorf("an empty old_string: %+v", r)
	}
	if r := run(t, s, "edit_file", map[string]any{"file_path": p, "old_string": "three", "new_string": "3"}); r.IsError {
		t.Errorf("edit once: %+v", r)
	}
	if r := run(t, s, "edit_file", map[string]any{"file_path": p, "old_string": "two", "new_string": "2", "replace_all": true}); r.IsError {
		t.Errorf("edit everywhere: %+v", r)
	}
	if b, _ := os.ReadFile(p); string(b) != "one\n2\n3\n2\n" {
		t.Errorf("after the edits: %q", b)
	}
	// read_file is capped, and says where to go on.
	big := filepath.Join(dir, "big")
	os.WriteFile(big, bytes.Repeat([]byte("0123456789\n"), 20000), 0o644)
	if r := run(t, s, "read_file", map[string]any{"file_path": big}); len(r.Text) > MaxOutput+200 || !strings.Contains(r.Text, "offset and limit") {
		t.Errorf("read_file of a big file: %d bytes", len(r.Text))
	}
}

func TestListDir(t *testing.T) {
	s, dir := newServer(t, Options{})
	for _, p := range []string{"a/b/c/d.txt", "a/x.txt", "top.txt"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte("hi"), 0o644)
	}
	os.Symlink("top.txt", filepath.Join(dir, "link"))
	if r := run(t, s, "list_dir", map[string]any{"path": dir}); r.Text != "a/\nlink -> top.txt\ntop.txt (2 bytes)" {
		t.Errorf("list_dir: %q", r.Text)
	}
	if r := run(t, s, "list_dir", map[string]any{"path": dir, "recursive": true, "depth": 2}); r.Text != "a/\na/b/\na/x.txt (2 bytes)\nlink -> top.txt\ntop.txt (2 bytes)" {
		t.Errorf("list_dir to depth 2: %q", r.Text)
	}
	if r := run(t, s, "list_dir", map[string]any{"path": dir, "recursive": true}); !strings.Contains(r.Text, "a/b/c/\n") || strings.Contains(r.Text, "d.txt") {
		t.Errorf("list_dir to the default depth 3: %q", r.Text)
	}
	many := filepath.Join(dir, "many")
	os.Mkdir(many, 0o755)
	for i := 0; i < MaxListEntries+5; i++ {
		os.WriteFile(filepath.Join(many, strings.Repeat("f", 1)+string(rune('a'+i%26))+time.Duration(i).String()), nil, 0o644)
	}
	if r := run(t, s, "list_dir", map[string]any{"path": many}); strings.Count(r.Text, "\n") != MaxListEntries || !strings.Contains(r.Text, "more entries") {
		t.Errorf("list_dir past the cap: %d lines", strings.Count(r.Text, "\n"))
	}
}

// The server speaks MCP's JSON-RPC at /mcp.
func TestMCP(t *testing.T) {
	s, _ := newServer(t, Options{})
	post := func(body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", "/mcp", strings.NewReader(body)))
		out := map[string]any{}
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if _, out := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); out["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "silo-tools" {
		t.Errorf("initialize: %v", out)
	}
	if code, _ := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Errorf("a notification: %d", code)
	}
	_, out := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var names []string
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "exec,read_file,write_file,edit_file,list_dir" {
		t.Errorf("tools: %v", names)
	}
	_, out = post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"exec","arguments":{"command":"echo hi"}}}`)
	if r := out["result"].(map[string]any); r["isError"] != false || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "hi") {
		t.Errorf("tools/call: %v", out)
	}
	if _, out := post(`{"jsonrpc":"2.0","id":4,"method":"nope"}`); out["error"].(map[string]any)["code"] != float64(-32601) {
		t.Errorf("an unknown method: %v", out)
	}
}

// Run as root, commands run as the named user and files are theirs.
func TestRunsAsUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	s, dir := newServer(t, Options{User: "nobody"})
	if r := run(t, s, "exec", map[string]any{"command": "id -un", "workdir": "/"}); !strings.Contains(r.Text, "nobody") {
		t.Errorf("exec as: %+v", r)
	}
	os.Chmod(filepath.Dir(dir), 0o755)
	os.Chmod(dir, 0o755)
	p := filepath.Join(dir, "new", "f")
	run(t, s, "write_file", map[string]any{"file_path": p, "content": "x"})
	if r := run(t, s, "exec", map[string]any{"command": "echo y >> " + p + " && cat " + p, "workdir": "/"}); r.IsError {
		t.Errorf("the user can't change a file write_file made: %+v", r)
	}
}

// exec's commands find the silo's name prefix in SILO_NAME_PREFIX
// (gcp-backend.md, "Naming", SV-76).
func TestNamePrefixEnv(t *testing.T) {
	s, err := New(Options{NamePrefix: "k7f3qa-"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Env(), "SILO_NAME_PREFIX=k7f3qa-") {
		t.Errorf("env %v has no SILO_NAME_PREFIX", s.Env())
	}
	s, _ = New(Options{})
	for _, kv := range s.Env() {
		if strings.HasPrefix(kv, NamePrefixEnv+"=") {
			t.Errorf("no prefix, but %s", kv)
		}
	}
}
