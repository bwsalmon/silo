package ssh_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/bwsalmon/silo/v2/location"
	sshloc "github.com/bwsalmon/silo/v2/location/ssh"
	"github.com/bwsalmon/silo/v2/session"
	"github.com/bwsalmon/silo/v2/silofile"
)

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":      "plain",
		"":           "''",
		"a b":        "'a b'",
		"it's":       `'it'\''s'`,
		"$HOME;rm *": `'$HOME;rm *'`,
	} {
		if got := sshloc.Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestArgv(t *testing.T) {
	o := &sshloc.OpenSSH{Target: "devbox", User: "me", Port: 2222, Identity: "/k"}
	got := strings.Join(o.Argv([]string{"-T"}, "true"), " ")
	want := "ssh -o StrictHostKeyChecking=yes -l me -p 2222 -i /k -T devbox -- true"
	if got != want {
		t.Errorf("Argv = %s, want %s", got, want)
	}
}

// newLocation returns a location.Session in a short session directory
// (the master's socket path must fit in a socket address).
func newSession(t *testing.T) *location.Session {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "silo-t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return &location.Session{ID: session.NewID(), Dir: dir, CLI: "claude", VersionArgs: []string{"--version"}, AmbientAuth: true, Mode: "headless"}
}

func prepare(t *testing.T, tr sshloc.Transport, s *location.Session) (*sshloc.Location, location.HostInfo, error) {
	t.Helper()
	l := sshloc.New(sshloc.Config{Transport: tr})
	t.Cleanup(func() { l.Teardown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	info, err := l.Prepare(ctx, s)
	return l, info, err
}

func wantPrereq(t *testing.T, err error, parts ...string) {
	t.Helper()
	var pre *location.PrerequisiteError
	if !errors.As(err, &pre) {
		t.Fatalf("err = %v, want a PrerequisiteError", err)
	}
	for _, p := range parts {
		if !strings.Contains(pre.Error(), p) {
			t.Errorf("error %q does not say %q", pre.Error(), p)
		}
	}
}

func TestProbe(t *testing.T) {
	d := server(t)
	t.Run("found", func(t *testing.T) {
		_, info, err := prepare(t, d.transport(), newSession(t))
		if err != nil {
			t.Fatal(err)
		}
		if info.OS != "linux" || info.Binary != "/usr/local/bin/claude" || info.Home != "/home/silo" || !strings.Contains(info.Version, "2.1.197") {
			t.Errorf("info = %+v", info)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		s := newSession(t)
		s.CLI = "codex"
		_, _, err := prepare(t, d.transport(), s)
		wantPrereq(t, err, "codex was not found", "silo never installs")
	})
	t.Run("missing agent.binary", func(t *testing.T) {
		s := newSession(t)
		s.Binary = "/opt/nope/claude"
		_, _, err := prepare(t, d.transport(), s)
		wantPrereq(t, err, "agent.binary /opt/nope/claude")
	})
	t.Run("missing login", func(t *testing.T) {
		d.exec(t, "rm -f ~/.fake-login")
		_, _, err := prepare(t, d.transport(), newSession(t))
		wantPrereq(t, err, "not logged in", "ssh -t silotest claude auth login")
		// With an API key instead of the CLI's login, it is not checked.
		s := newSession(t)
		s.AmbientAuth = false
		if _, _, err := prepare(t, d.transport(), s); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown host key", func(t *testing.T) {
		tr := &sshloc.OpenSSH{Target: "silotest", Options: []string{"-F", d.empty}}
		_, _, err := prepare(t, tr, newSession(t))
		wantPrereq(t, err, "host key of silotest is not known", "ssh-keyscan -p "+d.port+" 127.0.0.1")
	})
	t.Run("gateway_transport unix", func(t *testing.T) {
		l := sshloc.New(sshloc.Config{Transport: d.transport(), GatewayTransport: "unix"})
		_, err := l.Prepare(context.Background(), newSession(t))
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestGatewayPortsYesIsRefused(t *testing.T) {
	d := server(t)
	tr := d.transport()
	tr.User = "wide"
	s := newSession(t)
	s.AmbientAuth = false
	l, _, err := prepare(t, tr, s)
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Gateway(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})
	wantPrereq(t, err, "GatewayPorts is yes", "clientspecified")
}

func TestDryRunChecks(t *testing.T) {
	d := server(t)
	s := newSession(t)
	d.exec(t, "rm -f ~/.fake-login")
	res := sshloc.Check(context.Background(), d.transport(), s)
	got := map[string]bool{}
	for _, r := range res {
		got[r.Name] = r.OK
	}
	want := map[string]bool{"ssh silotest: reachable": true, "ssh silotest: host key accepted": true, "ssh silotest: agent CLI claude": true, "ssh silotest: claude login": false}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("checks = %+v", res)
	}
	res = sshloc.Check(context.Background(), &sshloc.OpenSSH{Target: "silotest", Options: []string{"-F", d.empty}}, s)
	if len(res) != 2 || res[0].OK || res[1].OK || !strings.Contains(res[1].Detail, "ssh-keyscan") {
		t.Errorf("checks with an unknown key = %+v", res)
	}
}

// run is one silo run against the sshd.
type run struct {
	t       *testing.T
	d       *sshd
	root    string
	environ []string
	loc     *sshloc.Location
}

func newRun(t *testing.T, d *sshd) *run {
	root, err := os.MkdirTemp("/tmp", "silo-r-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	for _, s := range []string{"run", "state", "home"} {
		os.MkdirAll(filepath.Join(root, s), 0o700)
	}
	r := &run{t: t, d: d, root: root, environ: []string{
		"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(root, "home"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "run"), "XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"SILO_TEST_SECRET=hunter2-very-secret",
	}}
	getenv := func(k string) string {
		for _, kv := range r.environ {
			if key, v, _ := strings.Cut(kv, "="); key == k {
				return v
			}
		}
		return ""
	}
	r.loc = sshloc.New(sshloc.Config{Transport: d.transport(), GC: sshloc.GCOptions{Live: session.Live(getenv)}})
	return r
}

func (r *run) silofile(text string) *silofile.Silofile {
	dir := filepath.Join(r.root, "silo")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "Silofile"), []byte(text), 0o644)
	f, err := silofile.Load(dir)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := f.Validate(silofile.Options{}).Err(); err != nil {
		r.t.Fatal(err)
	}
	return f
}

type outcome struct {
	code   int
	err    error
	stderr string
}

func (r *run) run(f *silofile.Silofile, mod func(*session.Options)) outcome {
	var stdout, stderr bytes.Buffer
	opts := session.Options{
		File: f, Environ: r.environ, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		NoGC: true, Grace: 2 * time.Second, Location: r.loc,
	}
	if mod != nil {
		mod(&opts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	code, err := session.Run(ctx, opts)
	return outcome{code, err, stderr.String()}
}

const headless = `silo: v2
name: remote
agent:
  cli: claude
  mode: headless
  prompt: "hello from silo"
location:
  kind: ssh
  ssh: {host: silotest}
remote_permissions: all
credentials:
  - name: secret
    from: {env: SILO_TEST_SECRET}
    to: [{agent: env, name: SECRET_TOKEN}]
`

func TestHeadlessRun(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	d.fake(t, fakeConfig{Exit: 7, Sleep: 3})
	type seen struct{ ps, netstat, files string }
	got := make(chan seen, 1)
	go func() {
		d.report(t)
		got <- seen{
			ps:      d.exec(t, "ps -eo args"),
			netstat: d.exec(t, "netstat -tln"),
			files:   d.exec(t, "ls -a /tmp/silo.*"),
		}
	}()
	out := r.run(r.silofile(headless), nil)
	if out.code != 7 {
		t.Fatalf("exit %d, want the CLI's 7: %v\n%s", out.code, out.err, out.stderr)
	}
	rep := d.report(t)
	if rep.Error != "" || !strings.HasPrefix(rep.URL, "http://127.0.0.1:") {
		t.Errorf("the fake CLI did not reach the gateway: %+v", rep)
	}
	if rep.TTY || !strings.Contains(rep.Stdin, "hello from silo") {
		t.Errorf("headless: tty %v, stdin %q", rep.TTY, rep.Stdin)
	}
	if !strings.HasPrefix(rep.Cwd, "/tmp/silo.") {
		t.Errorf("cwd %s, want the private work directory", rep.Cwd)
	}
	if !strings.Contains(strings.Join(rep.Env, "\n"), "SECRET_TOKEN=hunter2-very-secret") {
		t.Errorf("the delivered credential is not in the CLI's environment")
	}
	s := <-got
	// No secret in any remote argv, and the env file was deleted once
	// sourced.
	if strings.Contains(s.ps, "hunter2") {
		t.Errorf("the secret is in a remote argv:\n%s", s.ps)
	}
	if !strings.Contains(s.ps, "/usr/local/bin/claude") {
		t.Errorf("ps did not see the CLI:\n%s", s.ps)
	}
	if strings.Contains(s.files, "env") {
		t.Errorf("the env file was left: %s", s.files)
	}
	// The forward binds the remote loopback only, even with GatewayPorts
	// yes on the server.
	m := regexp.MustCompile(`127\.0\.0\.1:(\d+)/mcp`).FindStringSubmatch(rep.URL)
	if m == nil {
		t.Fatalf("gateway URL %s", rep.URL)
	}
	var binds []string
	for _, line := range strings.Split(s.netstat, "\n") {
		if f := strings.Fields(line); len(f) > 3 && strings.HasSuffix(f[3], ":"+m[1]) {
			binds = append(binds, f[3])
		}
	}
	ok := len(binds) > 0
	for _, b := range binds {
		ok = ok && b == "127.0.0.1:"+m[1]
	}
	if !ok {
		t.Errorf("the forward binds %v, want 127.0.0.1:%s only\n%s", binds, m[1], s.netstat)
	}
	if dirs := d.remoteDirs(t); len(dirs) != 0 {
		t.Errorf("remote session directories left after a normal exit: %v", dirs)
	}
}

// openPTY opens a pseudo-terminal pair.
func openPTY(t *testing.T) (ptm, pts *os.File) {
	t.Helper()
	ptm, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no ptys: %v", err)
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatal(e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatal(e)
	}
	pts, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ptm.Close(); pts.Close() })
	return ptm, pts
}

func TestInteractiveRun(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	d.fake(t, fakeConfig{Exit: 3})
	ptm, pts := openPTY(t)
	var mu sync.Mutex
	var term bytes.Buffer
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := ptm.Read(b)
			mu.Lock()
			term.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	text := strings.Replace(strings.Replace(headless, "mode: headless", "mode: interactive", 1), `  prompt: "hello from silo"`+"\n", "", 1)
	out := r.run(r.silofile(text), func(o *session.Options) { o.Stdin, o.Stdout = pts, pts })
	if out.code != 3 {
		t.Fatalf("exit %d, want the CLI's 3: %v\n%s", out.code, out.err, out.stderr)
	}
	rep := d.report(t)
	if !rep.TTY || rep.Error != "" {
		t.Errorf("interactive: %+v", rep)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(term.String(), "fake claude on a tty") {
		t.Errorf("the terminal saw %q", term.String())
	}
}

func TestDroppedConnection(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	d.fake(t, fakeConfig{Wait: true})
	go func() {
		d.report(t)
		ctls, _ := filepath.Glob(filepath.Join(r.root, "run", "silo", "s-*", "ssh.ctl"))
		if len(ctls) != 1 {
			t.Errorf("control sockets %v", ctls)
			return
		}
		out, _ := exec.Command("ssh", "-F", d.config, "-S", ctls[0], "-O", "check", "silotest").CombinedOutput()
		m := regexp.MustCompile(`pid=(\d+)`).FindStringSubmatch(string(out))
		if m == nil {
			t.Errorf("no master pid in %q", out)
			return
		}
		var pid int
		fmt.Sscan(m[1], &pid)
		syscall.Kill(pid, syscall.SIGKILL)
	}()
	out := r.run(r.silofile(headless), nil)
	if out.code != session.ExitConnLost {
		t.Fatalf("exit %d, want 75: %v\n%s", out.code, out.err, out.stderr)
	}
	// Local teardown ran; the remote session directory is left.
	if left, _ := filepath.Glob(filepath.Join(r.root, "run", "silo", "s-*")); len(left) != 0 {
		t.Errorf("local session directories left: %v", left)
	}
	dirs := d.remoteDirs(t)
	if len(dirs) != 1 {
		t.Fatalf("remote session directories after a drop: %v", dirs)
	}
	// A directory from another machine, and one from a live session, are
	// kept.
	d.exec(t, "mkdir -m 700 /tmp/silo.otherhst && echo 'id=s-aaaaaaaaaaaa origin=elsewhere:1' > /tmp/silo.otherhst/session")
	live := filepath.Join(r.root, "run", "silo", "s-bbbbbbbbbbbb")
	os.MkdirAll(live, 0o700)
	d.exec(t, "mkdir -m 700 /tmp/silo.livesess && echo 'id=s-bbbbbbbbbbbb origin="+sshloc.Origin()+"' > /tmp/silo.livesess/session")
	t.Cleanup(func() { d.exec(t, "rm -rf /tmp/silo.otherhst /tmp/silo.livesess") })

	removed, err := sshloc.GCHost(context.Background(), d.transport(), sshloc.GCOptions{Live: session.Live(func(k string) string {
		if k == "XDG_RUNTIME_DIR" {
			return filepath.Join(r.root, "run")
		}
		return ""
	})})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != dirs[0] {
		t.Errorf("gc removed %v, want %v", removed, dirs)
	}
	if left := d.remoteDirs(t); len(left) != 2 {
		t.Errorf("after gc: %v, want the other machine's and the live session's", left)
	}
}

func TestSignalHeadless(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	d.fake(t, fakeConfig{Wait: true})
	sigs := make(chan os.Signal, 1)
	go func() {
		d.report(t)
		sigs <- syscall.SIGINT
	}()
	out := r.run(r.silofile(headless), func(o *session.Options) { o.Signals = sigs })
	if out.code != session.ExitInterrupted {
		t.Fatalf("exit %d, want 130: %v\n%s", out.code, out.err, out.stderr)
	}
	if rep := d.report(t); rep.Signal != "interrupt" {
		t.Errorf("the remote CLI got %q, want interrupt", rep.Signal)
	}
	if dirs := d.remoteDirs(t); len(dirs) != 0 {
		t.Errorf("remote session directories left: %v", dirs)
	}
}

// TestJourney3 is journey 3 end to end (design §12): the example
// remote-scripts silo, with the fake remote claude in place of codex,
// calling local_tail_logs on this machine through the reverse forward.
func TestJourney3(t *testing.T) {
	d := server(t)
	r := newRun(t, d)
	dir := filepath.Join(r.root, "silo")
	if err := os.CopyFS(dir, os.DirFS("../../docs/examples/remote-scripts")); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(r.root, "home", ".cache", "preview", "logs")
	os.MkdirAll(logs, 0o700)
	os.WriteFile(filepath.Join(logs, "main.log"), []byte("one\ntwo\nthree\n"), 0o600)
	f, err := silofile.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The fake CLI stands in for codex, in headless mode, with no
	// workdir on the remote host and no credential store.
	f.Agent.CLI, f.Agent.Mode, f.Agent.Workdir = "claude", "headless", ""
	f.Location.SSH.Host = "silotest"
	f.Credentials = nil
	d.fake(t, fakeConfig{Call: "local_tail_logs", Args: map[string]any{"args": []any{"main", "2"}}})
	out := r.run(f, func(o *session.Options) { o.Prompt = "show me the logs" })
	if out.code != 0 {
		t.Fatalf("exit %d: %v\n%s", out.code, out.err, out.stderr)
	}
	rep := d.report(t)
	if rep.Error != "" || !strings.Contains(rep.Result, `"stdout": "two\nthree\n"`) {
		t.Errorf("local_tail_logs from the remote host: %+v", rep)
	}
	if dirs := d.remoteDirs(t); len(dirs) != 0 {
		t.Errorf("remote session directories left: %v", dirs)
	}
}
