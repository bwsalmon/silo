package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/gateway"
	"github.com/bwsalmon/silo/v2/internal/mcptest"
	"github.com/bwsalmon/silo/v2/mcp"
)

func TestMain(m *testing.M) {
	mcptest.MaybeServe()
	os.Exit(m.Run())
}

// stdioSpec launches this test binary as the test server.
func stdioSpec(name string, env ...string) Spec {
	return Spec{
		Name:     name,
		Command:  []string{os.Args[0]},
		Env:      append([]string{mcptest.ModeVar + "=stdio", "PATH=" + os.Getenv("PATH")}, env...),
		Required: true,
	}
}

type warnings struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnings) warn(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, fmt.Sprintf(format, args...))
}

func (w *warnings) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.msgs, "\n")
}

func start(t *testing.T, opts Options, specs ...Spec) (*Proxy, []gateway.Tool, error) {
	t.Helper()
	p := New(opts)
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	tools, err := p.Start(context.Background(), specs)
	return p, tools, err
}

func toolNames(tools []gateway.Tool) string { return strings.Join(names(tools), ",") }

func call(t *testing.T, tools []gateway.Tool, name string, args map[string]any) (mcp.Result, error) {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl.Handler(context.Background(), args)
		}
	}
	t.Fatalf("no tool %s in %s", name, toolNames(tools))
	return mcp.Result{}, nil
}

func TestAllowlistFilters(t *testing.T) {
	sp := stdioSpec("t")
	sp.Allow = []string{"echo", "pid"}
	_, tools, err := start(t, Options{}, sp)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(tools); got != "t__echo,t__pid" {
		t.Fatalf("tools = %s, want t__echo,t__pid", got)
	}
	r, err := call(t, tools, "t__echo", map[string]any{"text": "hello"})
	if err != nil || r.Text != "hello" || r.IsError {
		t.Fatalf("echo = %+v, %v", r, err)
	}
}

func TestDefaultAllowsEverything(t *testing.T) {
	_, tools, err := start(t, Options{}, stdioSpec("t"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != len(mcptest.AllTools) {
		t.Fatalf("tools = %s, want all of %v", toolNames(tools), mcptest.AllTools)
	}
}

func TestUnknownAllowlistedToolIsAnError(t *testing.T) {
	sp := stdioSpec("t")
	sp.Allow = []string{"echo", "ecoh"}
	_, _, err := start(t, Options{}, sp)
	var se *StartError
	if !errors.As(err, &se) || se.Server != "t" || !strings.Contains(err.Error(), "ecoh") || !strings.Contains(err.Error(), "does not offer") {
		t.Fatalf("err = %v, want a StartError naming ecoh", err)
	}
}

func TestRequiredAndOptional(t *testing.T) {
	bad := Spec{Name: "bad", Command: []string{"/nonexistent/server"}, Required: true}
	if _, _, err := start(t, Options{}, stdioSpec("ok"), bad); err == nil || !strings.Contains(err.Error(), "MCP server bad") {
		t.Fatalf("required: err = %v, want MCP server bad", err)
	}
	var w warnings
	bad.Required = false
	_, tools, err := start(t, Options{Warn: w.warn}, stdioSpec("ok"), bad)
	if err != nil {
		t.Fatalf("optional: %v", err)
	}
	for _, tl := range tools {
		if strings.HasPrefix(tl.Name, "bad__") {
			t.Errorf("optional server's tool %s is present", tl.Name)
		}
	}
	if !strings.Contains(w.String(), "MCP server bad did not start") {
		t.Errorf("warnings = %q", w.String())
	}
}

func TestStartTimeout(t *testing.T) {
	var pgid int
	opts := Options{StartTimeout: 300 * time.Millisecond, OnLaunch: func(_ string, p int) { pgid = p }}
	began := time.Now()
	_, _, err := start(t, opts, stdioSpec("slow", mcptest.HangVar+"=1"))
	if err == nil || !strings.Contains(err.Error(), "within 300ms") {
		t.Fatalf("err = %v, want a start timeout", err)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Errorf("took %s", d)
	}
	if pgid == 0 || groupAlive(pgid) {
		t.Errorf("the timed-out server's group %d is still alive", pgid)
	}
}

func pid(t *testing.T, tools []gateway.Tool, name string) int {
	t.Helper()
	r, err := call(t, tools, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(r.Text)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeadServerIsAToolError(t *testing.T) {
	var w warnings
	_, tools, err := start(t, Options{Warn: w.warn}, stdioSpec("t"))
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(pid(t, tools, "t__pid"), syscall.SIGKILL)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := call(t, tools, "t__echo", map[string]any{"text": "x"})
		if err != nil {
			if !strings.Contains(err.Error(), "MCP server t") {
				t.Fatalf("err = %v, want it to name the server", err)
			}
			if strings.Contains(err.Error(), "not running") && !strings.Contains(err.Error(), "restart: never") {
				t.Errorf("err = %v, want restart: never said", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("calls still succeed after the server was killed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRestartAtMostOncePerInterval(t *testing.T) {
	var mu sync.Mutex
	var launches []time.Time
	interval := 700 * time.Millisecond
	opts := Options{RestartInterval: interval, OnLaunch: func(string, int) {
		mu.Lock()
		launches = append(launches, time.Now())
		mu.Unlock()
	}}
	sp := stdioSpec("t")
	sp.Restart = true
	_, tools, err := start(t, opts, sp)
	if err != nil {
		t.Fatal(err)
	}
	first := pid(t, tools, "t__pid")
	_ = syscall.Kill(first, syscall.SIGKILL)
	second := waitPID(t, tools, first)
	_ = syscall.Kill(second, syscall.SIGKILL)
	waitPID(t, tools, second)
	mu.Lock()
	defer mu.Unlock()
	if len(launches) != 3 {
		t.Fatalf("%d launches, want 3", len(launches))
	}
	for i := 1; i < len(launches); i++ {
		if d := launches[i].Sub(launches[i-1]); d < interval-50*time.Millisecond {
			t.Errorf("launch %d came %s after the one before, want at least %s", i, d, interval)
		}
	}
}

// waitPID waits for the server to answer with a pid other than old.
func waitPID(t *testing.T, tools []gateway.Tool, old int) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r, err := call(t, tools, "t__pid", nil)
		if err == nil {
			if n, _ := strconv.Atoi(r.Text); n != old && n != 0 {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the server was not restarted")
	return 0
}

func TestListChangedThroughTheGateway(t *testing.T) {
	g := gateway.New(nil)
	sp := stdioSpec("t")
	sp.Allow = []string{"echo", "pid", "set_tools"}
	p, tools, err := start(t, Options{}, sp)
	if err != nil {
		t.Fatal(err)
	}
	g.Register(tools...)
	srv, err := g.Serve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	p.Attach(g)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := mcp.NewClient(srv.URL, srv.Token)
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	notes, err := c.Notifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The server now offers echo and env: env is not allowlisted, and
	// pid and set_tools are gone.
	r, err := c.CallTool(ctx, "t__set_tools", map[string]any{"names": []any{"echo", "env"}})
	if err != nil || r.IsError {
		t.Fatalf("set_tools = %+v, %v", r, err)
	}
	select {
	case m := <-notes:
		if m != "notifications/tools/list_changed" {
			t.Fatalf("notification %q", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no tools/list_changed")
	}
	listed, err := c.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Name != "t__echo" {
		t.Fatalf("listed %+v, want t__echo only", listed)
	}
}

func TestURLServer(t *testing.T) {
	ts := mcptest.New(nil)
	srv, err := ts.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	g := gateway.New(nil)
	sp := Spec{Name: "u", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer " + srv.Token}, Allow: []string{"echo", "env"}, Required: true}
	p := New(Options{})
	tools, err := p.Start(context.Background(), []Spec{sp})
	if err != nil {
		t.Fatal(err)
	}
	g.Register(tools...)
	p.Attach(g)
	r, err := call(t, tools, "u__echo", map[string]any{"text": "over http"})
	if err != nil || r.Text != "over http" {
		t.Fatalf("echo = %+v, %v", r, err)
	}
	ts.SetTools([]string{"env", "pid"})
	deadline := time.Now().Add(10 * time.Second)
	for got := ""; got != "u__env"; {
		if time.Now().After(deadline) {
			t.Fatalf("gateway lists %q after the server's list changed, want u__env", got)
		}
		time.Sleep(20 * time.Millisecond)
		var n []string
		for _, tl := range g.ListTools() {
			n = append(n, tl.Name)
		}
		got = strings.Join(n, ",")
	}
	// silo never stops a server it did not start.
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(srv.URL, srv.Token)
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("the url server stopped answering after Close: %v", err)
	}
}

func TestBadAuthOnURLServerIsAStartError(t *testing.T) {
	ts := mcptest.New(nil)
	srv, err := ts.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	_, _, err = start(t, Options{}, Spec{Name: "u", URL: srv.URL, Required: true})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want HTTP 401", err)
	}
}

func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// gone reports whether pid has exited: no such process, or a zombie
// nobody has reaped yet.
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	return len(f) > 0 && (f[0] == "Z" || f[0] == "X")
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !gone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTeardownKillsTheProcessGroup(t *testing.T) {
	p, tools, err := start(t, Options{}, stdioSpec("t"))
	if err != nil {
		t.Fatal(err)
	}
	server := pid(t, tools, "t__pid")
	child := pid(t, tools, "t__spawn")
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitGone(t, server)
	waitGone(t, child)
}

func TestTeardownKillsAfterTheGrace(t *testing.T) {
	grace := 400 * time.Millisecond
	p, tools, err := start(t, Options{StopGrace: grace}, stdioSpec("t", mcptest.IgnoreTermVar+"=1"))
	if err != nil {
		t.Fatal(err)
	}
	server := pid(t, tools, "t__pid")
	began := time.Now()
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d < grace {
		t.Errorf("Close took %s; a server ignoring SIGTERM should have had %s", d, grace)
	}
	waitGone(t, server)
}

func TestDialedServer(t *testing.T) {
	ts := mcptest.New([]string{"echo"})
	srv, err := ts.Listen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	sp := Spec{Name: "caller", Required: true, Dial: func(ctx context.Context) (mcp.Conn, error) {
		return mcp.DialHTTP(ctx, srv.URL, map[string]string{"Authorization": "Bearer " + srv.Token})
	}}
	_, tools, err := start(t, Options{}, sp)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(tools); got != "caller__echo" {
		t.Fatalf("tools = %s", got)
	}
}
