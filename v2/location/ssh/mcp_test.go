package ssh_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bwsalmon/silo/v2/gateway/proxy"
	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
	"github.com/bwsalmon/silo/v2/session"
)

// The remote MCP server tests (design §13.2, stage 5b) run the test MCP
// server, /usr/local/bin/mcpserver in the sshd image, on the agent host.

const secret = "hunter2-very-secret"

// remoteStdio is a headless Silofile plus a server silo starts on the
// agent host, which gets SILO_TEST_SECRET as R_TOKEN.
const remoteStdio = headless + `  - name: rtok
    from: {env: SILO_TEST_SECRET}
    to: [{mcp: r, env: R_TOKEN}]
mcp:
  - name: r
    where: remote
    command: [mcpserver]
    env: {PLAIN: plain}
    tools: [echo, env, pid]
`

// remotePID is the remote server's pid, from the file beside its env file.
func remotePID(t *testing.T, d *sshd, name string) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		if out := strings.TrimSpace(d.exec(t, "cat /tmp/silo.*/mcp-"+name+".pid 2>/dev/null || true")); out != "" {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no pid file for remote server %s", name)
	return ""
}

// waitRemoteGone waits for a remote process to be gone.
func waitRemoteGone(t *testing.T, d *sshd, pid string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if strings.TrimSpace(d.exec(t, "kill -0 "+pid+" 2>/dev/null && echo alive || true")) == "" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("remote process %s outlived the session: %s", pid, d.exec(t, "ps -o pid,args -p "+pid+" || true"))
}

func TestRemoteStdioServer(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	d.fake(t, fakeConfig{Call: "r__env", Args: map[string]any{"name": "R_TOKEN"}, Wait: true})
	type seen struct{ args, envFiles, pid, env string }
	got := make(chan seen, 1)
	sigs := make(chan os.Signal, 1)
	go func() {
		d.report(t)
		s := seen{
			args:     d.exec(t, "ps -eww -o args; for f in /proc/[0-9]*/cmdline; do tr '\\0' ' ' < $f; echo; done 2>/dev/null"),
			envFiles: d.exec(t, "ls /tmp/silo.*/mcp-*.env 2>/dev/null || true"),
			pid:      remotePID(t, d, "r"),
			env:      d.exec(t, "cat /proc/$(cat /tmp/silo.*/mcp-r.pid)/environ | tr '\\0' '\\n'"),
		}
		got <- s
		sigs <- syscall.SIGINT
	}()
	f := r.silofile(remoteStdio)
	out := r.run(f, func(o *session.Options) { o.Signals = sigs })
	if out.code != session.ExitInterrupted {
		t.Fatalf("exit %d, want 130: %v\n%s", out.code, out.err, out.stderr)
	}
	rep := d.report(t)
	if rep.Error != "" || rep.Result != secret {
		t.Errorf("r__env R_TOKEN from the remote CLI: %+v", rep)
	}
	tools := strings.Join(rep.Tools, ",")
	for _, want := range []string{"r__echo", "r__env", "r__pid"} {
		if !strings.Contains(tools, want) {
			t.Errorf("tools %s lack %s", tools, want)
		}
	}
	if strings.Contains(tools, "r__spawn") {
		t.Errorf("tools %s include one outside the allowlist", tools)
	}
	s := <-got
	if strings.Contains(s.args, secret) {
		t.Errorf("a remote argv holds the credential:\n%s", s.args)
	}
	if !strings.Contains(s.args, "mcpserver") {
		t.Errorf("no mcpserver among the remote processes:\n%s", s.args)
	}
	if s.envFiles != "" {
		t.Errorf("env files left while the server runs: %s", s.envFiles)
	}
	// The remote login environment, filtered: the minimal names, the
	// entry's env and the credential, and nothing else.
	var names []string
	for _, kv := range strings.Split(strings.TrimSpace(s.env), "\n") {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "R_TOKEN") || !strings.Contains(got, "PLAIN") || !strings.Contains(got, "HOME") ||
		strings.Contains(got, "SSH_CONNECTION") || strings.Contains(got, "SSH_CLIENT") {
		t.Errorf("the remote server's environment has %s", got)
	}
	waitRemoteGone(t, d, s.pid)
	if dirs := d.remoteDirs(t); len(dirs) != 0 {
		t.Errorf("remote session directories left: %v", dirs)
	}
	var lock map[string]session.MCPResolved
	if err := session.ReadLock(f.Folder, "mcp", &lock); err != nil {
		t.Fatal(err)
	}
	if l := lock["r"]; l.Where != "remote" || l.Host != "silotest" || strings.Join(l.Command, " ") != "mcpserver" || strings.Join(l.Env, ",") != "PLAIN,R_TOKEN" {
		t.Errorf("lock.json: %+v", l)
	}
}

func TestRemoteURLServer(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	d.exec(t, "rm -f ~/http.json")
	if _, err := sh("", "docker", "exec", "-d", "-u", "silo", d.container, "/usr/local/bin/mcpserver", "-http", "/home/silo/http.json"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.exec(t, "pkill -f '^/usr/local/bin/mcpserver -http' || true") })
	var srv struct{ URL, Token string }
	for i := 0; i < 100; i++ {
		if out := d.exec(t, "cat ~/http.json 2>/dev/null || true"); out != "" && json.Unmarshal([]byte(out), &srv) == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if srv.URL == "" {
		t.Fatal("the remote url server did not start")
	}
	// Only the agent host can reach it: it listens on the container's
	// loopback.
	d.fake(t, fakeConfig{Call: "u__echo", Args: map[string]any{"text": "through -L"}})
	f := r.silofile(headless + fmt.Sprintf(`mcp:
  - name: u
    where: remote
    url: %s
    headers: {Authorization: "Bearer %s"}
    tools: [echo]
`, srv.URL, srv.Token))
	out := r.run(f, nil)
	if out.code != 0 {
		t.Fatalf("exit %d: %v\n%s", out.code, out.err, out.stderr)
	}
	if rep := d.report(t); rep.Error != "" || rep.Result != "through -L" || strings.Join(rep.Tools, ",") != "u__echo" {
		t.Errorf("u__echo from the remote CLI: %+v", rep)
	}
	// silo never stops a server it did not start.
	if alive := d.exec(t, "pgrep -f '^/usr/local/bin/mcpserver -http' || true"); strings.TrimSpace(alive) == "" {
		t.Errorf("the url server was stopped")
	}
}

func TestRemoteFromClaudeUser(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	d.exec(t, `printf '%s' '{"mcpServers": {"x": {"command": "mcpserver", "env": {"X_MODE": "remote config"}}, "y": {"command": "no-such-server"}}}' > ~/.claude.json`)
	t.Cleanup(func() { d.exec(t, "rm -f ~/.claude.json") })
	d.fake(t, fakeConfig{Call: "x__env", Args: map[string]any{"name": "X_MODE"}})
	f := r.silofile(headless + `mcp:
  - name: x
    where: remote
    from: claude-user:x
    tools: [env]
`)
	// This machine's home has no ~/.claude.json: the remote one is read.
	if _, err := os.Stat(filepath.Join(r.root, "home", ".claude.json")); err == nil {
		t.Fatal("a local ~/.claude.json")
	}
	checks := session.CheckMCPOn(context.Background(), f, r.environ, sshloc.DirectHost(d.transport()))
	if len(checks) != 1 || !checks[0].OK || !strings.Contains(checks[0].Detail, "/home/silo/.claude.json on silotest") ||
		!strings.Contains(checks[0].Detail, "(/usr/local/bin/mcpserver)") || !strings.Contains(checks[0].Detail, "runs as silo on silotest") {
		t.Errorf("dry run: %+v", checks)
	}
	out := r.run(f, nil)
	if out.code != 0 {
		t.Fatalf("exit %d: %v\n%s", out.code, out.err, out.stderr)
	}
	if rep := d.report(t); rep.Error != "" || rep.Result != "remote config" {
		t.Errorf("x__env X_MODE: %+v", rep)
	}
	var lock map[string]session.MCPResolved
	if err := session.ReadLock(f.Folder, "mcp", &lock); err != nil {
		t.Fatal(err)
	}
	if l := lock["x"]; l.File != "/home/silo/.claude.json" || l.Where != "remote" || strings.Join(l.Env, ",") != "X_MODE" {
		t.Errorf("lock.json: %+v", l)
	}

	// --dry-run's failures: a binary the host lacks, a name its config
	// does not define, and mcp-json with no path on the host.
	f = r.silofile(headless + `mcp:
  - name: y
    where: remote
    from: claude-user:y
  - name: z
    where: remote
    from: claude-user:nope
  - name: j
    where: remote
    from: mcp-json:j
`)
	want := map[string]string{"mcp y": "no-such-server is not found on silotest", "mcp z": "not defined", "mcp j": "name it with mcp-json:j@<path>"}
	checks = session.CheckMCPOn(context.Background(), f, r.environ, sshloc.DirectHost(d.transport()))
	if len(checks) != 3 {
		t.Fatalf("checks: %+v", checks)
	}
	for _, c := range checks {
		if c.OK || !strings.Contains(c.Detail, want[c.Name]) {
			t.Errorf("%s: ok=%v %q, want a failure saying %q", c.Name, c.OK, c.Detail, want[c.Name])
		}
	}
}

// TestRemoteServerReaped: a server that does not exit when its channel
// closes is killed, with its process group, after the grace.
func TestRemoteServerReaped(t *testing.T) {
	d := server(t)
	s := newSession(t)
	l, _, err := prepare(t, d.transport(), s)
	if err != nil {
		t.Fatal(err)
	}
	// sleep never reads its stdin, so the closed channel does not end it.
	rs, err := l.LaunchMCP("z", []string{"sh", "-c", "sleep 301 & sleep 302"}, []string{"PATH=/usr/bin:/bin", "Z_SECRET=" + secret})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(rs.Argv, " "), secret) {
		t.Fatalf("the argv holds the secret: %v", rs.Argv)
	}
	ctx := context.Background()
	if err := rs.Prelaunch(ctx); err != nil {
		t.Fatal(err)
	}
	if mode := d.exec(t, "stat -c %a "+l.RemoteDir()+"/mcp-z.env"); strings.TrimSpace(mode) != "600" {
		t.Errorf("env file mode %s", mode)
	}
	cmd := exec.Command(rs.Argv[0], rs.Argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := remotePID(t, d, "z")
	for i := 0; i < 100 && !strings.Contains(d.exec(t, "ps -eo args"), "sleep 302"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if left := d.exec(t, "ls "+l.RemoteDir()+"/mcp-z.env 2>/dev/null || true"); left != "" {
		t.Errorf("the preamble left the env file: %s", left)
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	cmd.Wait()
	began := time.Now()
	rs.Reap(ctx, time.Second)
	if took := time.Since(began); took < time.Second {
		t.Errorf("Reap took %s, not the grace", took)
	}
	waitRemoteGone(t, d, pid)
	if ps := d.exec(t, "ps -eo args"); strings.Contains(ps, "sleep 30") {
		t.Errorf("the server's process group outlived Reap:\n%s", ps)
	}
	if left := d.exec(t, "ls "+l.RemoteDir()+"/ 2>/dev/null"); strings.Contains(left, "mcp-z") {
		t.Errorf("left in the session directory: %s", left)
	}
}

// killMaster kills the ssh master under ctl.
func killMaster(t *testing.T, d *sshd, ctl string) {
	t.Helper()
	out, _ := exec.Command("ssh", "-F", d.config, "-S", ctl, "-O", "check", "silotest").CombinedOutput()
	m := regexp.MustCompile(`pid=(\d+)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("no master pid in %q", out)
	}
	var pid int
	fmt.Sscan(m[1], &pid)
	syscall.Kill(pid, syscall.SIGKILL)
}

// TestDroppedConnectionFailsRemoteTools: once the master is gone, a
// remote server's tools are tool errors naming the server and the host.
func TestDroppedConnectionFailsRemoteTools(t *testing.T) {
	d := server(t)
	s := newSession(t)
	l, _, err := prepare(t, d.transport(), s)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := l.LaunchMCP("r", []string{"mcpserver"}, []string{"PATH=/usr/local/bin:/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	p := proxy.New(proxy.Options{StopGrace: time.Second})
	t.Cleanup(func() { p.Close(context.Background()) })
	tools, err := p.Start(context.Background(), []proxy.Spec{{
		Name: "r", Command: rs.Argv, Env: os.Environ(), Required: true,
		Prelaunch: rs.Prelaunch, Reap: rs.Reap, Gone: l.Gone,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var echo func(context.Context, map[string]any) (string, error)
	for _, tl := range tools {
		if tl.Name == "r__echo" {
			h := tl.Handler
			echo = func(ctx context.Context, a map[string]any) (string, error) {
				r, err := h(ctx, a)
				return r.Text, err
			}
		}
	}
	if echo == nil {
		t.Fatalf("tools: %v", tools)
	}
	if got, err := echo(context.Background(), map[string]any{"text": "up"}); err != nil || got != "up" {
		t.Fatalf("echo before the drop: %q %v", got, err)
	}
	pid := remotePID(t, d, "r")
	killMaster(t, d, filepath.Join(s.Dir, "ssh.ctl"))
	<-l.MasterDone()
	_, err = echo(context.Background(), map[string]any{"text": "down"})
	if err == nil || !strings.Contains(err.Error(), "MCP server r is unreachable: the connection to silotest is gone") {
		t.Errorf("echo after the drop: %v", err)
	}
	// sshd hangs the channel up, and the server, reading EOF, exits.
	waitRemoteGone(t, d, pid)
}

// TestDroppedConnectionWithRemoteServer: a session whose master dies
// exits 75 with remote servers running, and they go.
func TestDroppedConnectionWithRemoteServer(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	d.fake(t, fakeConfig{Call: "r__echo", Args: map[string]any{"text": "up"}, Wait: true})
	pids := make(chan string, 1)
	go func() {
		d.report(t)
		pids <- remotePID(t, d, "r")
		ctls, _ := filepath.Glob(filepath.Join(r.root, "run", "silo", "s-*", "ssh.ctl"))
		if len(ctls) != 1 {
			t.Errorf("control sockets %v", ctls)
			return
		}
		killMaster(t, d, ctls[0])
	}()
	out := r.run(r.silofile(remoteStdio), nil)
	if out.code != session.ExitConnLost {
		t.Fatalf("exit %d, want 75: %v\n%s", out.code, out.err, out.stderr)
	}
	if rep := d.report(t); rep.Error != "" || rep.Result != "up" {
		t.Errorf("r__echo before the drop: %+v", rep)
	}
	waitRemoteGone(t, d, <-pids)
	d.exec(t, "rm -rf /tmp/silo.*")
}
