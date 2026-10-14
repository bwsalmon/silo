package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/gateway/audit"
	"github.com/bwsalmon/silo/v2/mcp"
	"github.com/bwsalmon/silo/v2/sandbox"
)

// fakeSandbox records what it is asked and answers from a map of files.
type fakeSandbox struct {
	files map[string]string
	execs []sandbox.ExecRequest
	block bool
}

func (f *fakeSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.execs = append(f.execs, req)
	if f.block {
		<-ctx.Done()
		return sandbox.ExecResult{Stdout: []byte("partial")}, ctx.Err()
	}
	return sandbox.ExecResult{ExitCode: 3, Stdout: []byte("out " + req.Env["SECRET"]), Stderr: []byte("err")}, nil
}

func (f *fakeSandbox) ReadFile(_ context.Context, path string, limit int64) ([]byte, error) {
	s, ok := f.files[path]
	if !ok {
		return nil, errors.New(path + ": no such file")
	}
	if int64(len(s)) > limit {
		s = s[:limit]
	}
	return []byte(s), nil
}

func (f *fakeSandbox) WriteFile(_ context.Context, path string, data []byte, _ fs.FileMode) error {
	f.files[path] = string(data)
	return nil
}

func (f *fakeSandbox) DeliverFile(_ context.Context, path string, data []byte) error {
	f.files[path] = string(data)
	return nil
}

func (f *fakeSandbox) Destroy(context.Context) error { return nil }

type fixture struct {
	gw     *Gateway
	client *mcp.Client
	log    string
	sb     *fakeSandbox
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, audit.FileName)
	log, err := audit.Open(path, "s-test", audit.Caller{CLI: "test", Host: "local"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	log.Register("secret", "hunter2-secret")
	sb := &fakeSandbox{files: map[string]string{"/etc/motd": "hello"}}
	gw := New(log)
	gw.Register(SandboxTools(map[string]sandbox.Sandbox{"build": sb, "test": &fakeSandbox{files: map[string]string{}}})...)
	srv, err := gw.Serve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	c := mcp.NewClient(srv.URL, srv.Token)
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &fixture{gw: gw, client: c, log: path, sb: sb}
}

func (f *fixture) records(t *testing.T) []audit.Record {
	t.Helper()
	file, err := os.Open(f.log)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var out []audit.Record
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var r audit.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestToolsListedWithSandboxEnum(t *testing.T) {
	f := newFixture(t)
	tools, err := f.client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		enum := tl.InputSchema["properties"].(map[string]any)["sandbox"].(map[string]any)["enum"]
		if b, _ := json.Marshal(enum); string(b) != `["build","test"]` {
			t.Errorf("%s: sandbox enum %s", tl.Name, b)
		}
	}
	if got := strings.Join(names, ","); got != "sandbox_exec,sandbox_read_file,sandbox_write_file,sandbox_list_dir" {
		t.Fatalf("tools %s", got)
	}
	if SandboxTools(nil) != nil {
		t.Fatal("sandbox tools served with no sandboxes")
	}
}

func TestRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"shell", map[string]any{"command": "ls"}, `tool "shell" is not served`},
		{"sandbox_exec", map[string]any{"sandbox": "prod", "command": "ls"}, `no sandbox is named "prod": this session declares build, test`},
		{"sandbox_exec", map[string]any{"command": "ls"}, `argument "sandbox" is required`},
		{"sandbox_exec", map[string]any{"sandbox": "build", "command": "ls", "user": "root"}, `unknown argument "user"`},
		{"sandbox_exec", map[string]any{"sandbox": "build", "command": 3}, `"command" must be a string`},
	} {
		_, err := f.client.CallTool(ctx, tc.tool, tc.args)
		var rpcErr *mcp.Error
		if !errors.As(err, &rpcErr) || !strings.Contains(rpcErr.Message, tc.want) || !strings.HasPrefix(rpcErr.Message, "refused: ") {
			t.Errorf("%s %v: error %v, want an MCP error containing %q", tc.tool, tc.args, err, tc.want)
		}
	}
	if len(f.sb.execs) != 0 {
		t.Fatalf("a refused call ran: %v", f.sb.execs)
	}
	recs := f.records(t)
	if len(recs) != 10 {
		t.Fatalf("%d audit lines, want a start and an end for each of 5 refused calls", len(recs))
	}
	if recs[0].Event != "start" || recs[0].Tool != "shell" || recs[1].Outcome != audit.OutcomeRefused || !strings.Contains(recs[1].Reason, "not served") {
		t.Fatalf("audit of a refused call: %+v %+v", recs[0], recs[1])
	}
}

func TestCallsAuditedAndRedacted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r, err := f.client.CallTool(ctx, "sandbox_exec", map[string]any{
		"sandbox": "build", "command": "echo hunter2-secret", "env": map[string]any{"SECRET": "hunter2-secret"}, "cwd": "/w", "stdin": "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError || r.Text() != "exit=3\nstdout:\nout hunter2-secret\nstderr:\nerr" {
		t.Fatalf("result %+v %q", r, r.Text())
	}
	req := f.sb.execs[0]
	if strings.Join(req.Argv, " ") != "/bin/sh -c echo hunter2-secret" || req.Cwd != "/w" || string(req.Stdin) != "in" {
		t.Fatalf("exec request %+v", req)
	}
	r, err = f.client.CallTool(ctx, "sandbox_read_file", map[string]any{"sandbox": "build", "path": "/etc/motd", "limit": 3})
	if err != nil || r.IsError || !strings.HasPrefix(r.Text(), "hel\n... [/etc/motd is longer than 3 bytes") {
		t.Fatalf("read: %+v %v", r, err)
	}
	r, err = f.client.CallTool(ctx, "sandbox_read_file", map[string]any{"sandbox": "build", "path": "/nope"})
	if err != nil || !r.IsError || !strings.Contains(r.Text(), "no such file") {
		t.Fatalf("read of a missing file: %+v %v", r, err)
	}
	if _, err := f.client.CallTool(ctx, "sandbox_write_file", map[string]any{"sandbox": "build", "path": "/x", "content": "data"}); err != nil || f.sb.files["/x"] != "data" {
		t.Fatalf("write: %v %v", err, f.sb.files)
	}

	data, _ := os.ReadFile(f.log)
	if strings.Contains(string(data), "hunter2") {
		t.Fatalf("a credential reached the audit log:\n%s", data)
	}
	recs := f.records(t)
	if len(recs) != 8 {
		t.Fatalf("%d audit lines, want 8", len(recs))
	}
	start, end := recs[0], recs[1]
	if args, _ := start.Args.(map[string]any); args["command"] != "echo [credential:secret]" {
		t.Errorf("start args %v", start.Args)
	}
	if end.Outcome != audit.OutcomeError || !strings.Contains(end.Head, "out [credential:secret]") || end.Caller.CLI != "test" || end.Session != "s-test" {
		t.Errorf("end %+v", end)
	}
	if recs[3].Outcome != audit.OutcomeOK || recs[5].Outcome != audit.OutcomeError || recs[7].Outcome != audit.OutcomeOK {
		t.Errorf("outcomes %+v", recs)
	}
}

func TestExecTimeout(t *testing.T) {
	f := newFixture(t)
	f.sb.block = true
	start := time.Now()
	r, err := f.client.CallTool(context.Background(), "sandbox_exec", map[string]any{"sandbox": "build", "command": "sleep 100", "timeout": 1})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s", d)
	}
	if !r.IsError || !strings.Contains(r.Text(), "timed out after 1s") || !strings.Contains(r.Text(), "partial") {
		t.Fatalf("result %q", r.Text())
	}
	recs := f.records(t)
	if recs[len(recs)-1].Outcome != audit.OutcomeTimeout {
		t.Fatalf("end %+v", recs[len(recs)-1])
	}
}

func TestToolTimeoutAndListChanged(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := f.client.Notifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	f.gw.Register(Tool{
		Name: "slow", InputSchema: map[string]any{"type": "object"}, Timeout: 50 * time.Millisecond,
		Handler: func(ctx context.Context, _ map[string]any) (mcp.Result, error) {
			<-ctx.Done()
			return mcp.Result{}, ctx.Err()
		},
	})
	select {
	case m := <-ch:
		if m != "notifications/tools/list_changed" {
			t.Fatalf("notification %q", m)
		}
	case <-ctx.Done():
		t.Fatal("no tools/list_changed after Register")
	}
	r, err := f.client.CallTool(ctx, "slow", nil)
	if err != nil || !r.IsError || r.Text() != "slow timed out after 50ms" {
		t.Fatalf("slow: %+v %v", r, err)
	}
}
