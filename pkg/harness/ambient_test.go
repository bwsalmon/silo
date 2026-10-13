package harness_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwsalmon/silo/pkg/agents"
	"github.com/bwsalmon/silo/pkg/harness"
)

var mcpCmd = []string{"/usr/libexec/silod", "mcp-stdio", "--socket", "/run/silo/sessions/a1/silo.sock"}

func envHas(env []string, prefix string) bool {
	return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

// An ambient session's CLI keeps the agent user's HOME and login, gets
// no model proxy, and reaches the session socket through its stdio MCP
// server, with its own tools still off.
func TestAmbientCommands(t *testing.T) {
	l := harness.Launch{Model: "claude-opus-5-5", Prompt: "Fix it.", SessionID: "s1", Ambient: true, MCP: mcpCmd, WorkDir: "/data/sessions/a1"}
	cc := harness.ClaudeCode{}.Command(l)
	i := slices.Index(cc.Argv, "--mcp-config")
	if i < 0 {
		t.Fatalf("claude: no --mcp-config in %q", cc.Argv)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cc.Argv[i+1]), &cfg); err != nil {
		t.Fatal(err)
	}
	s := cfg.MCPServers["silo"]
	if len(cfg.MCPServers) != 1 || s.Type != "stdio" || s.Command != mcpCmd[0] || !slices.Equal(s.Args, mcpCmd[1:]) {
		t.Errorf("claude's MCP config: %s", cc.Argv[i+1])
	}
	if j := slices.Index(cc.Argv, "--tools"); j < 0 || cc.Argv[j+1] != "" || !slices.Contains(cc.Argv, "--strict-mcp-config") {
		t.Errorf("claude's own tools aren't off: %q", cc.Argv)
	}
	if envHas(cc.Env, "HOME=") || envHas(cc.Env, "ANTHROPIC_") {
		t.Errorf("claude's ambient env: %q", cc.Env)
	}

	cx := harness.Codex{}.Command(l)
	joined := strings.Join(cx.Argv, "\n")
	if !strings.Contains(joined, `mcp_servers.silo.command="/usr/libexec/silod"`) ||
		!strings.Contains(joined, `mcp_servers.silo.args=["mcp-stdio","--socket","/run/silo/sessions/a1/silo.sock"]`) {
		t.Errorf("codex's MCP server: %q", cx.Argv)
	}
	if strings.Contains(joined, "mcp_servers.silo.url") || strings.Contains(joined, "model_provider") {
		t.Errorf("codex ambient has the forwarder or the model proxy: %q", cx.Argv)
	}
	if j := slices.Index(cx.Argv, "--cd"); j < 0 || cx.Argv[j+1] != l.WorkDir {
		t.Errorf("codex's cwd: %q", cx.Argv)
	}
	if !strings.Contains(joined, "features.shell_tool=false") {
		t.Errorf("codex's shell isn't off: %q", cx.Argv)
	}
	if envHas(cx.Env, "HOME=") || envHas(cx.Env, "CODEX_HOME=") || envHas(cx.Env, "OPENAI_API_KEY=") {
		t.Errorf("codex's ambient env: %q", cx.Env)
	}

	// A container session is as it was.
	if c := (harness.ClaudeCode{}).Command(harness.Launch{Model: "m", Prompt: "p", SessionID: "s"}); !slices.Contains(c.Env, "HOME="+harness.Home) {
		t.Errorf("a container session lost its HOME: %q", c.Env)
	}
}

func TestVersions(t *testing.T) {
	for out, want := range map[string]string{"2.1.4 (Claude Code)\n": "2.1.4", "codex-cli 0.46.0": "0.46.0", "v10.0.12-beta": "10.0.12"} {
		if v, ok := harness.ParseVersion(out); !ok || v != want {
			t.Errorf("ParseVersion(%q) = %q %v", out, v, ok)
		}
	}
	if _, ok := harness.ParseVersion("unknown"); ok {
		t.Error("a version in no version")
	}
	for _, c := range []struct {
		a, b  string
		older bool
	}{{"2.1.3", "2.1.4", true}, {"2.1.4", "2.1.4", false}, {"2.10.0", "2.9.9", false}, {"0.45.9", "0.46.0", true}, {"1.0.0", "0.46.0", false}} {
		if got := harness.OlderVersion(c.a, c.b); got != c.older {
			t.Errorf("OlderVersion(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// hostRuntime is a fake Runtime on the host, with a CLI of version.
type hostRuntime struct {
	runtime
	version string
	asked   [][]string
}

func (h *hostRuntime) Version(_ context.Context, argv []string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, argv)
	return h.version, nil
}

// versionControl records SessionVersion.
type versionControl struct {
	*control
	vmu      sync.Mutex
	versions []string
}

func (v *versionControl) SessionVersion(_ context.Context, agent, version string) error {
	v.vmu.Lock()
	defer v.vmu.Unlock()
	v.versions = append(v.versions, agent+" "+version)
	return nil
}

// ambientLogins: ambient-anthropic is ambient, anything else an API key.
type ambientLogins struct{ logins }

func (*ambientLogins) SessionCredential(_ context.Context, name string) (agents.Credential, error) {
	if name == "ambient-anthropic" {
		return agents.Credential{Name: name, Provider: "anthropic", Kind: agents.CredentialAmbient}, nil
	}
	return agents.Credential{Name: name, Provider: "anthropic", Kind: agents.CredentialAPIKey}, nil
}

func ambientSession(id string) agents.Session {
	s := session(id, "claude-code")
	s.Credential = "ambient-anthropic"
	return s
}

func newAmbientRunner(c *versionControl, rt *hostRuntime) *harness.Runner {
	r := &harness.Runner{Host: "host", Slots: 2, Runtime: rt, Control: c, Logins: &ambientLogins{}, Layout: layout,
		SyncEvery: 5 * time.Millisecond, RetryAfter: time.Millisecond, Poll: 5 * time.Millisecond,
		MCP: func(socket string) []string { return []string{"/usr/libexec/silod", "mcp-stdio", "--socket", socket} }}
	return r
}

func TestAmbientRunner(t *testing.T) {
	ctx := context.Background()
	c := &versionControl{control: newControl()}
	rt := &hostRuntime{runtime: runtime{lines: transcript(t, "claude-code.jsonl")}, version: "2.3.0"}
	r := newAmbientRunner(c, rt)
	c.add(ambientSession("a1"), agents.StateProvisioning, "")
	if err := r.Start(ctx, ambientSession("a1")); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(ctx, "a1")
	c.set("a1", agents.StateRunning)
	r.StartTurn(ctx, "a1")
	waitEnd(t, c.control, "a1")
	specs := rt.turns()
	if len(specs) != 1 {
		t.Fatalf("%d turns", len(specs))
	}
	sp := specs[0]
	if sp.Argv[0] != "claude" || sp.Network != harness.NetworkAmbient || sp.Agent != "a1" {
		t.Errorf("the turn runs %q on %q", sp.Argv, sp.Network)
	}
	if !strings.Contains(strings.Join(sp.Argv, " "), layout.Socket("a1")) {
		t.Errorf("the MCP server isn't on the session's socket: %q", sp.Argv)
	}
	if len(rt.asked) != 1 || !slices.Equal(rt.asked[0], []string{"claude", "--version"}) {
		t.Errorf("version asked %q", rt.asked)
	}
	if !slices.Equal(c.versions, []string{"a1 2.3.0"}) {
		t.Errorf("versions recorded %q", c.versions)
	}
	// Its next turn doesn't ask again.
	c.mu.Lock()
	c.msgs["a1"] = []string{"More."}
	c.mu.Unlock()
	c.set("a1", agents.StateRunning)
	r.StartTurn(ctx, "a1")
	waitEnd(t, c.control, "a1")
	if len(rt.asked) != 1 {
		t.Errorf("version asked %d times", len(rt.asked))
	}
}

// A CLI older than its adapter knows, or a credential that isn't
// ambient, doesn't start; three times in a row and the agent is FAILED.
func TestAmbientRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		version, credential, want string
	}{
		"old cli":     {"2.0.9", "ambient-anthropic", "needs 2.1.4 or later"},
		"api key":     {"2.3.0", "anthropic-api", "takes only an ambient credential"},
		"on gcp host": {"", "ambient-anthropic", "only a local backend's host runs"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c := &versionControl{control: newControl()}
			rt := &hostRuntime{version: tc.version}
			r := newAmbientRunner(c, rt)
			if tc.version == "" {
				r.MCP = nil
			}
			s := ambientSession("a1")
			s.Credential = tc.credential
			c.add(s, agents.StateRunning, "")
			if err := r.Start(ctx, s); err != nil {
				t.Fatal(err)
			}
			defer r.Stop(ctx, "a1")
			deadline := time.Now().Add(5 * time.Second)
			for c.get("a1") != agents.StateFailed && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if c.get("a1") != agents.StateFailed || len(rt.turns()) != 0 {
				t.Fatalf("state %s after %d turns", c.get("a1"), len(rt.turns()))
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if !slices.ContainsFunc(c.events, func(e string) bool { return strings.Contains(e, tc.want) }) {
				t.Errorf("events %q: want %q", c.events, tc.want)
			}
		})
	}
}

// mcp-stdio carries each line to the socket's /mcp and writes each
// answer back as a line; a notification gets none, and a request the
// socket can't take gets a JSON-RPC error.
func TestMCPStdio(t *testing.T) {
	dir, err := os.MkdirTemp("", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		switch {
		case len(m.ID) == 0:
			w.WriteHeader(http.StatusAccepted)
			return
		case m.Method == "slow":
			<-release
		case m.Method == "broken":
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"method":"` + m.Method + `"}}` + "\n"))
	})}
	go srv.Serve(l)
	defer srv.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- harness.MCPStdio(context.Background(), sock, inR, outW); outW.Close() }()
	lines := bufio.NewScanner(outR)
	next := func() string {
		if !lines.Scan() {
			t.Fatal("no line")
		}
		return lines.Text()
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"slow"}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	// The slow call doesn't hold up the next.
	if got := next(); !strings.Contains(got, `"id":2`) || !strings.Contains(got, "tools/list") {
		t.Errorf("first answer %s", got)
	}
	close(release)
	if got := next(); !strings.Contains(got, `"id":1`) {
		t.Errorf("second answer %s", got)
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","id":"x","method":"broken"}`+"\n")
	if got := next(); !strings.Contains(got, `"id":"x"`) || !strings.Contains(got, `"error"`) {
		t.Errorf("a failed call: %s", got)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

// fakeSystemd writes systemd-run and systemctl scripts: systemd-run
// records its arguments and runs the command after --; systemctl
// records its own and lists units from a file.
func fakeSystemd(t *testing.T) (dir string, s *harness.Scopes) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir = t.TempDir()
	run := `#!/bin/sh
echo "$@" >> "` + dir + `/run.log"
while [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`
	ctl := `#!/bin/sh
echo "$@" >> "` + dir + `/ctl.log"
if [ "$2" = list-units ]; then cat "` + dir + `/units" 2>/dev/null; fi
`
	for name, body := range map[string]string{"systemd-run": run, "systemctl": ctl} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, &harness.Scopes{StateDir: filepath.Join(dir, "sessions"),
		SystemdRun: filepath.Join(dir, "systemd-run"), Systemctl: filepath.Join(dir, "systemctl")}
}

func TestScopes(t *testing.T) {
	ctx := context.Background()
	dir, s := fakeSystemd(t)
	spec := harness.HostSpec("a1", harness.Command{Argv: []string{"sh", "-c", `echo "$SILO_X"; pwd; echo line2; exit 3`}, Env: []string{"SILO_X=from-the-adapter"}}, harness.Limits{})
	var got []string
	res, err := s.Turn(ctx, spec, func(b []byte) { got = append(got, string(b)) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != 3 || res.State != nil {
		t.Errorf("result %+v", res)
	}
	if len(got) != 3 || got[0] != "from-the-adapter" || got[1] != s.Dir("a1") || got[2] != "line2" {
		t.Errorf("lines %q", got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "run.log"))
	for _, want := range []string{"--user --scope", "--unit=silo-agent-a1.scope", "MemoryMax=1073741824", "TasksMax=256", "CPUWeight=20"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("systemd-run %s: no %q", b, want)
		}
	}

	os.WriteFile(filepath.Join(dir, "units"), []byte("silo-agent-a1.scope loaded active running Silo agent a1 harness\nsilo-agent-BAD.scope loaded active running x\nother.scope loaded active running y\n"), 0o644)
	left, err := s.List(ctx)
	if err != nil || !slices.Equal(left, []string{"a1"}) {
		t.Errorf("List: %q %v", left, err)
	}
	if err := s.Remove(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir("a1")); !os.IsNotExist(err) {
		t.Errorf("the working directory stayed: %v", err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "ctl.log"))
	if !strings.Contains(string(b), "--user stop silo-agent-a1.scope") {
		t.Errorf("systemctl %s", b)
	}

	// The CLI's version, from the agent user's PATH.
	v, err := s.Version(ctx, []string{"sh", "-c", "echo '2.1.9 (Claude Code)'"})
	if err != nil || v != "2.1.9" {
		t.Errorf("Version: %q %v", v, err)
	}
	if _, err := s.Version(ctx, []string{"silo-no-such-cli", "--version"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("a missing CLI: %v", err)
	}
}
