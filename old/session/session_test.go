package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/old/gateway"
	"github.com/bwsalmon/silo/old/mcp"
	"github.com/bwsalmon/silo/old/sandbox"
	"github.com/bwsalmon/silo/old/sandbox/container"
	"github.com/bwsalmon/silo/old/session"
	"github.com/bwsalmon/silo/old/silofile"
)

// env is one test's invoking environment: a bin directory holding the
// fake claude, and private HOME and XDG directories.
type env struct {
	t       *testing.T
	root    string
	home    string
	environ []string
	// timeout bounds each run; zero is a minute.
	timeout time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root, home: filepath.Join(root, "home")}
	bin := filepath.Join(root, "bin")
	for _, d := range []string{bin, e.home, filepath.Join(root, "run"), filepath.Join(root, "state")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	e.environ = []string{
		"PATH=" + bin, "HOME=" + e.home, "LANG=C.UTF-8", "LC_ALL=C",
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"),
		"XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"AWS_SECRET_ACCESS_KEY=stray-secret",
		"SILO_TEST_API_KEY=sk-from-env",
		"SILO_TEST_DELIVERED=delivered-value",
	}
	return e
}

func (e *env) fake(cfg fakeConfig) {
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(e.home, "fake.json"), b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) report() fakeReport {
	e.t.Helper()
	var r fakeReport
	b, err := os.ReadFile(filepath.Join(e.home, "report.json"))
	if err != nil {
		e.t.Fatalf("the fake CLI left no report: %v", err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) silofile(text string) *silofile.Silofile {
	e.t.Helper()
	dir := filepath.Join(e.root, "silo")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "Silofile"), []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	f, err := silofile.Load(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := f.Validate(silofile.Options{}).Err(); err != nil {
		e.t.Fatal(err)
	}
	return f
}

type result struct {
	code   int
	err    error
	stderr string
}

func (e *env) run(f *silofile.Silofile, mod func(*session.Options)) result {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	opts := session.Options{
		File: f, Environ: e.environ, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		NoGC: true, Grace: 2 * time.Second, Backend: &fakeBackend{},
	}
	if mod != nil {
		mod(&opts)
	}
	timeout := e.timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	code, err := session.Run(ctx, opts)
	return result{code, err, stderr.String()}
}

// sessionGone checks teardown: the session directory is gone, and the
// final session.json and audit log are kept in the state directory.
func (e *env) sessionGone() session.Record {
	e.t.Helper()
	runs, _ := os.ReadDir(filepath.Join(e.root, "run", "silo"))
	if len(runs) != 0 {
		e.t.Fatalf("session directories left behind: %v", runs)
	}
	states, _ := filepath.Glob(filepath.Join(e.root, "state", "silo", "sessions", "s-*"))
	if len(states) != 1 {
		e.t.Fatalf("state directories: %v", states)
	}
	rec, err := session.ReadRecord(states[0])
	if err != nil {
		e.t.Fatal(err)
	}
	if rec.Result == nil {
		e.t.Fatal("session.json has no result")
	}
	if _, err := os.Stat(filepath.Join(states[0], "audit.jsonl")); err != nil {
		e.t.Fatalf("audit log not kept: %v", err)
	}
	return rec
}

const headless = `silo: v2
name: t
agent:
  cli: claude
  mode: headless
  prompt: "say hello"
  auth: {credential: key}
credentials:
  - name: key
    from: {env: SILO_TEST_API_KEY}
  - name: shared
    from: {env: SILO_TEST_DELIVERED}
    to: [{agent: env, name: DELIVERED}]
`

// The order the test steps and their undos ran in.
var (
	orderMu sync.Mutex
	order   []string
)

func logStep(s string) {
	orderMu.Lock()
	order = append(order, s)
	orderMu.Unlock()
}

func init() {
	for _, n := range []int{25, 35, 45, 55, 65} {
		name := "probe" + string(rune('0'+n/10))
		session.RegisterStep(session.Step{Name: name, Order: n, Run: func(ctx context.Context, s *session.Session) (session.Undo, error) {
			logStep(name)
			return func(context.Context) error { logStep("undo-" + name); return nil }, nil
		}})
	}
	session.RegisterToolProvider("echo", func(ctx context.Context, s *session.Session) ([]gateway.Tool, session.Undo, error) {
		return []gateway.Tool{{
			Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"},
			Handler: func(ctx context.Context, args map[string]any) (mcp.Result, error) {
				return mcp.Result{Text: "echo:" + args["text"].(string)}, nil
			},
		}}, nil, nil
	})
}

func TestHeadlessSessionEndToEnd(t *testing.T) {
	e := newEnv(t)
	e.fake(fakeConfig{Call: "echo", Args: map[string]any{"text": "hi"}, Exit: 7})
	orderMu.Lock()
	order = nil
	orderMu.Unlock()
	res := e.run(e.silofile(headless), nil)
	if res.code != 7 {
		t.Fatalf("exit %d (%v), want the CLI's 7\n%s", res.code, res.err, res.stderr)
	}
	r := e.report()
	if r.Error != "" || r.Result != "echo:hi" {
		t.Fatalf("gateway call: result %q error %q", r.Result, r.Error)
	}
	if !strings.Contains(r.Stdin, "say hello") {
		t.Fatalf("stdin %q has no prompt", r.Stdin)
	}
	envs := strings.Join(r.Env, "\n")
	for _, want := range []string{"ANTHROPIC_API_KEY=sk-from-env", "DELIVERED=delivered-value", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "MCP_TOOL_TIMEOUT="} {
		if !strings.Contains(envs, want) {
			t.Errorf("CLI env lacks %s:\n%s", want, envs)
		}
	}
	if strings.Contains(envs, "AWS_SECRET_ACCESS_KEY") || strings.Contains(envs, "SILO_TEST_API_KEY") {
		t.Errorf("the scrubbed environment leaked:\n%s", envs)
	}
	if !strings.Contains(res.stderr, `credential "shared" is delivered to the agent's environment`) {
		t.Errorf("no warning for the {agent: env} delivery:\n%s", res.stderr)
	}
	if !strings.HasSuffix(r.Cwd, "/work") {
		t.Errorf("cwd %s is not the private workdir", r.Cwd)
	}
	if entries, _ := os.ReadDir(r.Cwd); len(entries) != 0 {
		t.Errorf("workdir not empty")
	}
	rec := e.sessionGone()
	if rec.CLIVersion != "2.1.197" || rec.Result.ExitCode != 7 || rec.Mode != "headless" {
		t.Errorf("session.json: %+v", rec)
	}
	if !session.ValidID(rec.ID) {
		t.Errorf("session id %q", rec.ID)
	}
	audit, _ := os.ReadFile(filepath.Join(rec.StateDir, "audit.jsonl"))
	if !strings.Contains(string(audit), `"tool":"echo"`) {
		t.Errorf("audit has no call:\n%s", audit)
	}
	orderMu.Lock()
	got := strings.Join(order, " ")
	orderMu.Unlock()
	want := "probe2 probe3 probe4 probe5 probe6 undo-probe6 undo-probe5 undo-probe4 undo-probe3 undo-probe2"
	if got != want {
		t.Errorf("step order:\n got %s\nwant %s", got, want)
	}
}

func TestDeadlineEndsTheSession(t *testing.T) {
	e := newEnv(t)
	e.fake(fakeConfig{Wait: true, IgnoreInt: true})
	f := e.silofile(headless)
	f.Agent.Limits = &silofile.Limits{}
	d := silofile.Duration(time.Second)
	f.Agent.Limits.Deadline = &d
	res := e.run(f, nil)
	if res.code != session.ExitDeadline {
		t.Fatalf("exit %d (%v), want 124\n%s", res.code, res.err, res.stderr)
	}
	pid := e.report().PID
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("the CLI (pid %d) is still running", pid)
	}
	if rec := e.sessionGone(); rec.Result.Reason != "deadline" {
		t.Errorf("reason %q", rec.Result.Reason)
	}
}

func TestSignals(t *testing.T) {
	for _, c := range []struct {
		name  string
		mode  string
		fake  fakeConfig
		sigs  []os.Signal
		want  int
		after time.Duration
	}{
		{"headless SIGINT, the CLI stops", "headless", fakeConfig{Wait: true, IntExit: 3}, []os.Signal{syscall.SIGINT}, 130, 0},
		{"headless SIGINT, grace runs out", "headless", fakeConfig{Wait: true, IgnoreInt: true}, []os.Signal{syscall.SIGINT}, 130, 0},
		{"headless second SIGINT", "headless", fakeConfig{Wait: true, IgnoreInt: true}, []os.Signal{syscall.SIGINT, syscall.SIGINT}, 130, 0},
		{"SIGTERM", "headless", fakeConfig{Wait: true, IgnoreInt: true}, []os.Signal{syscall.SIGTERM}, 143, 0},
		{"interactive SIGINT is the CLI's", "interactive", fakeConfig{Wait: true, IntExit: 3}, []os.Signal{syscall.SIGINT}, 3, 0},
		{"interactive second SIGINT", "interactive", fakeConfig{Wait: true, IgnoreInt: true}, []os.Signal{syscall.SIGINT, syscall.SIGINT}, 130, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake(c.fake)
			text := strings.Replace(headless, "mode: headless", "mode: "+c.mode, 1)
			sigs := make(chan os.Signal, 4)
			go func() {
				// Once the CLI is up and has reported.
				for i := 0; i < 200; i++ {
					if _, err := os.Stat(filepath.Join(e.home, "report.json")); err == nil {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				for _, s := range c.sigs {
					sigs <- s
					time.Sleep(100 * time.Millisecond)
				}
			}()
			start := time.Now()
			res := e.run(e.silofile(text), func(o *session.Options) { o.Signals = sigs; o.Grace = time.Second })
			if res.code != c.want {
				t.Fatalf("exit %d (%v), want %d\n%s", res.code, res.err, c.want, res.stderr)
			}
			if time.Since(start) > 20*time.Second {
				t.Errorf("took %s", time.Since(start))
			}
			if syscall.Kill(e.report().PID, 0) == nil {
				t.Error("the CLI is still running")
			}
			e.sessionGone()
		})
	}
}

func TestRefusalsAndPrerequisites(t *testing.T) {
	e := newEnv(t)
	res := e.run(e.silofile(strings.Replace(headless, "auth: {credential: key}", "auth: {credential: key}\nremote_permissions: [Read, Teleport]", 1)), nil)
	if res.code != session.ExitRefused || !strings.Contains(res.err.Error(), "Teleport") {
		t.Fatalf("exit %d (%v), want 76", res.code, res.err)
	}
	e.sessionGone()

	e = newEnv(t)
	f := e.silofile(headless)
	f.Agent.Binary = "/nonexistent/claude"
	if res := e.run(f, nil); res.code != session.ExitPrerequisite {
		t.Fatalf("missing binary: exit %d (%v), want 69", res.code, res.err)
	}

	e = newEnv(t)
	e.environ = e.environ[:len(e.environ)-2] // SILO_TEST_API_KEY is unset
	if res := e.run(e.silofile(headless), nil); res.code != session.ExitPrerequisite {
		t.Fatalf("missing credential: exit %d (%v), want 69", res.code, res.err)
	}
}

// fakeBackend is a sandbox backend holding containers by session.
type fakeBackend struct {
	mu        sync.Mutex
	sessions  map[string]int
	destroyed []string
}

type fakeSandbox struct {
	sandbox.Sandbox
	b       *fakeBackend
	session string
}

func (s *fakeSandbox) Destroy(ctx context.Context) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.b.destroyed = append(s.b.destroyed, s.session)
	s.b.sessions[s.session]--
	if s.b.sessions[s.session] <= 0 {
		delete(s.b.sessions, s.session)
	}
	return nil
}

func (b *fakeBackend) Create(ctx context.Context, spec sandbox.SandboxSpec) (sandbox.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions == nil {
		b.sessions = map[string]int{}
	}
	b.sessions[spec.Session]++
	return &fakeSandbox{b: b, session: spec.Session}, nil
}

func (b *fakeBackend) Find(ctx context.Context, id string) ([]sandbox.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []sandbox.Sandbox
	for i := 0; i < b.sessions[id]; i++ {
		out = append(out, &fakeSandbox{b: b, session: id})
	}
	return out, nil
}

func (b *fakeBackend) Sessions(ctx context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for id := range b.sessions {
		out = append(out, id)
	}
	return out, nil
}

func TestGCRemovesStaleSessionsAndLabelledContainers(t *testing.T) {
	e := newEnv(t)
	root := filepath.Join(e.root, "run", "silo")
	getenv := func(k string) string {
		for _, kv := range e.environ {
			if strings.HasPrefix(kv, k+"=") {
				return kv[len(k)+1:]
			}
		}
		return ""
	}
	// A stale session: its process is gone. Its headless CLI's process
	// group is recorded too, and is gone as well.
	stale := session.NewID()
	os.MkdirAll(filepath.Join(root, stale, "agent"), 0o700)
	b, _ := json.Marshal(session.Record{ID: stale, PID: 1 << 22, Resources: []session.Resource{
		{Kind: session.KindContainers, Runtime: "fake"}, {Kind: session.KindProcessGroup, PGID: 1 << 22},
	}})
	os.WriteFile(filepath.Join(root, stale, session.RecordFile), b, 0o600)
	// A live session: this process.
	live := session.NewID()
	os.MkdirAll(filepath.Join(root, live), 0o700)
	b, _ = json.Marshal(session.Record{ID: live, PID: os.Getpid()})
	os.WriteFile(filepath.Join(root, live, session.RecordFile), b, 0o600)
	// A container of a session with no directory at all.
	orphan := session.NewID()
	fb := &fakeBackend{sessions: map[string]int{stale: 2, live: 1, orphan: 1}}

	rep, err := session.GC(context.Background(), session.GCOptions{Getenv: getenv, Backend: fb})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, stale)); err == nil {
		t.Error("the stale session directory is still there")
	}
	if _, err := os.Stat(filepath.Join(root, live)); err != nil {
		t.Error("the live session directory was removed")
	}
	if fb.sessions[stale] != 0 || fb.sessions[orphan] != 0 || fb.sessions[live] != 1 {
		t.Errorf("containers left: %v", fb.sessions)
	}
	if len(rep.Removed) != 2 {
		t.Errorf("report: %v", rep.Removed)
	}

	// silo run does the same at start.
	os.MkdirAll(filepath.Join(root, stale), 0o700)
	b, _ = json.Marshal(session.Record{ID: stale, PID: 1 << 22})
	os.WriteFile(filepath.Join(root, stale, session.RecordFile), b, 0o600)
	e.fake(fakeConfig{})
	res := e.run(e.silofile(headless), func(o *session.Options) { o.NoGC = false; o.Backend = fb })
	if res.code != 0 || !strings.Contains(res.stderr, "removed stale session directory") {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	var left []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() && session.ValidID(d.Name()) {
			left = append(left, d.Name())
		}
		return nil
	})
	if len(left) != 1 || left[0] != live {
		t.Errorf("session directories left: %v", left)
	}
}

// TestContainerSandboxEndToEnd is stage 1's deliverable: a silo with a
// container sandbox runs end to end, the fake CLI calling sandbox_exec
// through the gateway.
func TestContainerSandboxEndToEnd(t *testing.T) {
	b, err := container.New()
	if err == nil {
		err = exec.Command(b.Runtime, "info").Run()
	}
	if err != nil {
		if os.Getenv("SILO_V2_REQUIRE_DOCKER") != "" {
			t.Fatalf("SILO_V2_REQUIRE_DOCKER is set, but there is no container runtime: %v", err)
		}
		t.Skipf("no container runtime: %v", err)
	}
	e := newEnv(t)
	e.fake(fakeConfig{Call: "sandbox_exec", Args: map[string]any{"sandbox": "build", "command": "echo from-$HOSTNAME $TOKEN; cat /etc/os-release | head -1"}})
	f := e.silofile(headless + `sandboxes:
  - name: build
    kind: container
    image: busybox:1.37
  - name: spare
    kind: container
    image: busybox:1.37
`)
	f.Credentials[1].To = append(f.Credentials[1].To, silofile.Destination{Sandbox: "build", Env: "TOKEN"})
	var id string
	res := e.run(f, func(o *session.Options) { o.Backend = b })
	if res.code != 0 {
		t.Fatalf("exit %d (%v)\n%s", res.code, res.err, res.stderr)
	}
	r := e.report()
	if r.Error != "" || !strings.Contains(r.Result, "from-build delivered-value") {
		t.Fatalf("sandbox_exec: %q (error %q)", r.Result, r.Error)
	}
	if strings.Join(r.Tools, ",") != "echo,sandbox_exec,sandbox_read_file,sandbox_write_file,sandbox_list_dir" {
		t.Errorf("tools: %v", r.Tools)
	}
	rec := e.sessionGone()
	id = rec.ID
	if len(rec.Resources) == 0 || rec.Resources[0].Kind != session.KindContainers {
		t.Errorf("session.json resources: %+v", rec.Resources)
	}
	left, err := b.Find(context.Background(), id)
	if err != nil || len(left) != 0 {
		t.Fatalf("containers left after teardown: %v %v", left, err)
	}
	audit, _ := os.ReadFile(filepath.Join(rec.StateDir, "audit.jsonl"))
	if strings.Contains(string(audit), "delivered-value") {
		t.Error("the audit log holds a credential's value")
	}

	// A labelled container whose session is gone is collected by gc.
	stale := session.NewID()
	spec := sandbox.SandboxSpec{Session: stale, Name: "orphan", Kind: "container", Image: "busybox:1.37"}
	if _, err := b.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return agentcliGetenv(e.environ, k) }
	if _, err := session.GC(context.Background(), session.GCOptions{Getenv: getenv, Backend: b}); err != nil {
		t.Fatal(err)
	}
	if left, _ := b.Find(context.Background(), stale); len(left) != 0 {
		t.Fatalf("gc left %d containers of %s", len(left), stale)
	}
}

func agentcliGetenv(env []string, k string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, k+"=") {
			return kv[len(k)+1:]
		}
	}
	return ""
}
