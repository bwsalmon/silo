package router

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/internal/approval"
	"github.com/bwsalmon/silo/internal/audit"
	"github.com/bwsalmon/silo/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.MaybeServe()
	os.Exit(m.Run())
}

// newHost returns a host over a checkout holding the given files.
func newHost(t *testing.T, files map[string]string) (*Host, *Router) {
	t.Helper()
	checkout := t.TempDir()
	testutil.WriteFiles(t, checkout, files)
	rt := New(approval.NewFake(), audit.Discard{})
	t.Cleanup(rt.Close)
	return &Host{Router: rt, Checkout: checkout, SessionDir: t.TempDir(), Log: audit.Discard{}}, rt
}

func child(t *testing.T, h *Host, name string) *Child {
	t.Helper()
	v, ok := h.children.Load(name)
	if !ok {
		t.Fatalf("%s is not loaded", name)
	}
	return v.(*Child)
}

func callText(t *testing.T, rt *Router, tool string, args string) string {
	t.Helper()
	res := rt.Call(context.Background(), tool, json.RawMessage(args))
	if res.IsError {
		t.Fatalf("%s: %s", tool, res.AllText())
	}
	return res.AllText()
}

func TestHostStartsEntrypointWithPort(t *testing.T) {
	t.Setenv("SILO_TEST_SECRET", "do not pass")
	t.Setenv("TICKETS_TOKEN", "passed")
	h, rt := newHost(t, testutil.Prefixed(".silo/tools/echo", testutil.EchoToolset(t, "p", "pass_env: [TICKETS_TOKEN]")))
	report, ok := h.LoadAll(context.Background())
	if want := "loaded echo (tools: echo.crash, echo.echo, echo.env, echo.grow, echo.pwd)"; !ok || report != want {
		t.Fatalf("report = %q, want %q", report, want)
	}
	if got := callText(t, rt, "echo.echo", `{"text":"hi"}`); got != "p:hi" {
		t.Errorf("echo = %q", got)
	}
	env := "\n" + callText(t, rt, "echo.env", `{}`) + "\n"
	for _, want := range []string{"PATH", "SILO_PORT", "SILO_TOOLSET", "SILO_TOOLSET_DIR", "SILO_STATE_DIR", "SILO_SESSION_DIR", "SILO_CHECKOUT", "ECHO_PREFIX", "TICKETS_TOKEN"} {
		if !strings.Contains(env, "\n"+want+"\n") {
			t.Errorf("server env lacks %s:%s", want, env)
		}
	}
	if strings.Contains(env, "SILO_TEST_SECRET") {
		t.Error("server got a variable its manifest did not ask for")
	}
	// The entry point runs in its own directory.
	want, _ := filepath.EvalSymlinks(filepath.Join(h.Checkout, ".silo/tools/echo"))
	if got, _ := filepath.EvalSymlinks(callText(t, rt, "echo.pwd", `{}`)); got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
	if c := child(t, h, "echo"); c.Port() <= 0 || c.Port() > 65535 {
		t.Errorf("port %d", c.Port())
	}
}

// TestHostHTTPListChanged has the server add a tool and send
// tools/list_changed over its GET stream; the router re-lists it.
func TestHostHTTPListChanged(t *testing.T) {
	h, rt := newHost(t, testutil.Prefixed(".silo/tools/echo", testutil.EchoToolset(t, "p")))
	if report, ok := h.LoadAll(context.Background()); !ok {
		t.Fatal(report)
	}
	callText(t, rt, "echo.grow", `{}`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if res := rt.Call(context.Background(), "echo.extra", nil); !res.IsError && res.AllText() == "extra" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("echo.extra never appeared")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHostPythonEntrypoint(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	self, _ := os.Executable()
	files := testutil.EchoToolset(t, "py")
	delete(files, "start.sh")
	files["start.py"] = "import os, sys\nos.execv(" + strconv.Quote(self) + ", [" + strconv.Quote(self) + ", sys.argv[1]])\n"
	h, rt := newHost(t, testutil.Prefixed(".silo/tools/pyecho", files))
	if report, ok := h.LoadAll(context.Background()); !ok {
		t.Fatal(report)
	}
	if got := callText(t, rt, "pyecho.echo", `{"text":"x"}`); got != "py:x" {
		t.Errorf("echo = %q", got)
	}
}

func TestHostEntrypointErrors(t *testing.T) {
	both := testutil.EchoToolset(t, "p")
	both["start.py"] = "print('hi')\n"
	neither := testutil.EchoToolset(t, "p")
	delete(neither, "start.sh")
	neither["server.py"] = "print('hi')\n"
	files := testutil.Prefixed(".silo/tools/both", both)
	for p, c := range testutil.Prefixed(".silo/tools/neither", neither) {
		files[p] = c
	}
	files[".silo/tools/old/toolset.yaml"] = "name: old\nrun: [python3, server.py]\n"
	files[".silo/tools/old/start.sh"] = "exit 1\n"
	h, rt := newHost(t, files)
	report, ok := h.LoadAll(context.Background())
	if ok {
		t.Fatalf("loaded: %s", report)
	}
	for _, want := range []string{
		"FAILED both: has both start.sh and start.py",
		"FAILED neither: has no start.sh or start.py",
		`FAILED old: old/toolset.yaml: "run" is no longer a toolset.yaml setting`,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if len(rt.Toolsets()) != 0 {
		t.Errorf("loaded %v", rt.Toolsets())
	}
}

func TestHostReadinessTimeout(t *testing.T) {
	old := StartTimeout
	StartTimeout = time.Second
	t.Cleanup(func() { StartTimeout = old })
	for mode, want := range map[string][]string{
		"never-listen": {"did not listen on 127.0.0.1:", "within 1s", "echo-server: taking my time"},
		"exit":         {"exited (exit status 3) before it served MCP", "echo-server: giving up at once"},
	} {
		// The extra manifest line lands under env:, setting ECHO_MODE.
		h, _ := newHost(t, testutil.Prefixed(".silo/tools/slow", testutil.EchoToolset(t, "p", "  "+testutil.ModeEnv+": "+mode)))
		start := time.Now()
		report, ok := h.LoadAll(context.Background())
		if ok {
			t.Fatalf("%s: loaded", mode)
		}
		for _, w := range want {
			if !strings.Contains(report, w) {
				t.Errorf("%s: report lacks %q:\n%s", mode, w, report)
			}
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s: took %s", mode, d)
		}
	}
}

func TestHostRefusesNonLoopback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the check reads /proc")
	}
	h, rt := newHost(t, testutil.Prefixed(".silo/tools/open", testutil.EchoToolset(t, "p", "  "+testutil.ModeEnv+": bind-all")))
	report, ok := h.LoadAll(context.Background())
	if ok || !(strings.Contains(report, "refused: it listens on 0.0.0.0:") || strings.Contains(report, "refused: it listens on [::]:")) || !strings.Contains(report, "must bind to 127.0.0.1 only") {
		t.Fatalf("report = %s", report)
	}
	if rt.Has("open") {
		t.Error("loaded anyway")
	}
}

func TestHostCrashAndRestartOnReload(t *testing.T) {
	h, rt := newHost(t, testutil.Prefixed(".silo/tools/echo", testutil.EchoToolset(t, "one")))
	ctx := context.Background()
	if report, ok := h.LoadAll(ctx); !ok {
		t.Fatal(report)
	}
	first := child(t, h, "echo")
	rt.Call(ctx, "echo.crash", nil)
	<-first.exited
	res := rt.Call(ctx, "echo.echo", json.RawMessage(`{"text":"x"}`))
	if !res.IsError || !strings.Contains(res.AllText(), "has exited") || !strings.Contains(res.AllText(), "crashing as asked") {
		t.Errorf("call to a crashed set = %q", res.AllText())
	}
	if got := h.ExitedRepoSets(); len(got) != 1 || got[0] != "echo" {
		t.Errorf("exited = %v", got)
	}
	// A changed directory restarts the set on a new port.
	testutil.WriteFiles(t, h.Checkout, testutil.Prefixed(".silo/tools/echo", testutil.EchoToolset(t, "two")))
	report, ok := h.ReloadRepoSets(ctx, []string{"echo"})
	if !ok || !strings.HasPrefix(report, "reloaded echo") {
		t.Fatalf("report = %q", report)
	}
	second := child(t, h, "echo")
	if second == first || second.Pid() == first.Pid() {
		t.Error("not restarted")
	}
	if got := callText(t, rt, "echo.echo", `{"text":"x"}`); got != "two:x" {
		t.Errorf("echo = %q", got)
	}
	if got := h.ExitedRepoSets(); len(got) != 0 {
		t.Errorf("exited = %v", got)
	}
	// Deleting the directory stops it.
	os.RemoveAll(filepath.Join(h.Checkout, ".silo/tools/echo"))
	if report, _ := h.ReloadRepoSets(ctx, []string{"echo"}); report != "removed echo" {
		t.Errorf("report = %q", report)
	}
	if !second.Exited() {
		t.Error("removed set still running")
	}
}

// TestHostStopsProcessGroup: everything an entry point starts goes
// with it, even what ignores SIGTERM.
func TestHostStopsProcessGroup(t *testing.T) {
	old := StopGrace
	StopGrace = 500 * time.Millisecond
	t.Cleanup(func() { StopGrace = old })
	for _, ignoreTerm := range []bool{false, true} {
		files := testutil.EchoToolset(t, "p")
		script := `sleep 300 & echo $! > "$SILO_STATE_DIR/sleeper.pid"` + "\n" + files["start.sh"]
		if ignoreTerm {
			script = "trap '' TERM\n" + script
		}
		files["start.sh"] = script
		h, rt := newHost(t, testutil.Prefixed(".silo/tools/group", files))
		if report, ok := h.LoadAll(context.Background()); !ok {
			t.Fatal(report)
		}
		data, err := os.ReadFile(filepath.Join(h.SessionDir, "state", "group", "sleeper.pid"))
		if err != nil {
			t.Fatal(err)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if !alive(pid) {
			t.Fatal("sleeper not running")
		}
		start := time.Now()
		rt.Remove("group")
		took := time.Since(start)
		deadline := time.Now().Add(5 * time.Second)
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if alive(pid) {
			t.Errorf("ignoreTerm=%v: the entry point's child %d outlived it", ignoreTerm, pid)
		}
		if ignoreTerm && took < StopGrace {
			t.Errorf("SIGKILL came after %s, before the grace period", took)
		}
	}
}

// alive reports whether pid runs (a zombie waiting to be reaped does
// not count).
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return runtime.GOOS != "linux"
	}
	i := strings.LastIndexByte(string(data), ')')
	return i < 0 || !strings.HasPrefix(strings.TrimSpace(string(data[i+1:])), "Z")
}

func TestHostVendoredNames(t *testing.T) {
	files := testutil.Prefixed(".silo/tools/vendor/acme/echo", testutil.EchoToolset(t, "acme"))
	for p, c := range testutil.Prefixed(".silo/tools/echo", testutil.EchoToolset(t, "local")) {
		files[p] = c
	}
	files[".silo/tools/vendor/acme/VENDOR.lock"] = "repo: team/shared\n"
	h, rt := newHost(t, files)
	ctx := context.Background()
	report, ok := h.LoadAll(ctx)
	if !ok || !strings.Contains(report, "loaded acme.echo (tools: acme.echo.crash, acme.echo.echo,") {
		t.Fatalf("report = %s", report)
	}
	if got := callText(t, rt, "acme.echo.echo", `{"text":"x"}`); got != "acme:x" {
		t.Errorf("acme.echo.echo = %q", got)
	}
	if got := callText(t, rt, "echo.echo", `{"text":"x"}`); got != "local:x" {
		t.Errorf("echo.echo = %q", got)
	}
	// A vendored set's change is found by its key.
	if got := ChangedToolsets([]string{".silo/tools/vendor/acme/echo/start.sh", ".silo/tools/vendor/acme/VENDOR.lock"}); len(got) != 1 || got[0] != "vendor/acme/echo" {
		t.Errorf("changed = %v", got)
	}

	// An alias that is also a set's name loads neither.
	testutil.WriteFiles(t, h.Checkout, testutil.Prefixed(".silo/tools/acme", testutil.EchoToolset(t, "clash")))
	report, ok = h.ReloadRepoSets(ctx, []string{"acme", "vendor/acme/echo"})
	if ok || strings.Count(report, `"acme" is both a tool set`) != 2 {
		t.Errorf("clash report = %s", report)
	}
	os.RemoveAll(filepath.Join(h.Checkout, ".silo/tools/acme"))

	// Config can disable a whole alias.
	h.Disabled = map[string]bool{"acme": true}
	if report, _ := h.ReloadRepoSets(ctx, []string{"vendor/acme/echo"}); report != "skipped acme.echo: disabled by config" {
		t.Errorf("disabled report = %q", report)
	}
}
