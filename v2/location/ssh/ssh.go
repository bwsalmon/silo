// Package ssh is the ssh location (design §5.3, journey 3): the agent
// CLI runs on a remote host with that host's ambient login, while silo,
// the gateway, credentials, sandboxes and the local executor stay on the
// invoking machine. Everything goes through the system OpenSSH binary
// (call 7) over one master connection per session; the only path back
// is one reverse forward to the gateway, bound to the remote loopback.
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v2/agentcli"
	"github.com/bwsalmon/silo/v2/internal/procgroup"
	"github.com/bwsalmon/silo/v2/location"
)

// Config configures the ssh location.
type Config struct {
	// Transport reaches the host.
	Transport Transport
	// GatewayTransport is location.ssh.gateway_transport: tcp, or empty.
	// unix is reserved (design §5.3) and refused.
	GatewayTransport string
	// GC, when its Live is set, has Prepare remove this host's stale
	// remote session directories, as silo gc --host does.
	GC GCOptions
	// Stderr receives the master connection's own messages after it is
	// up; discarded when nil.
	Stderr io.Writer
}

// Location runs the CLI on a remote host over ssh.
type Location struct {
	cfg  Config
	t    Transport
	s    *location.Session
	host string

	ctl, ctlDir string
	master      *exec.Cmd
	masterDone  chan struct{}
	masterErr   *capWriter

	// rdir is the remote session directory.
	rdir string
	// removed lists what the startup gc removed.
	removed []string

	mu   sync.Mutex
	torn bool
}

var _ location.Location = (*Location)(nil)

// New returns an ssh location.
func New(cfg Config) *Location {
	return &Location{cfg: cfg, t: cfg.Transport, host: cfg.Transport.Host()}
}

// Prepare opens the master connection, runs the probe, removes this
// host's stale session directories and creates the remote session
// directory.
func (l *Location) Prepare(ctx context.Context, s *location.Session) (location.HostInfo, error) {
	l.s = s
	switch l.cfg.GatewayTransport {
	case "", "tcp":
	case "unix":
		return location.HostInfo{}, agentcli.Refusef("location.ssh.gateway_transport: unix is reserved and not in v2.0: no CLI speaks HTTP over a Unix socket, so it needs a stdio bridge on the remote host (design §5.3); use tcp")
	default:
		return location.HostInfo{}, agentcli.Refusef("location.ssh.gateway_transport %q: use tcp", l.cfg.GatewayTransport)
	}
	if err := l.openMaster(ctx); err != nil {
		return location.HostInfo{}, err
	}
	info, err := probe(ctx, l.mux, l.host, s)
	if err != nil {
		return info, err
	}
	if l.cfg.GC.Live != nil {
		opts := l.cfg.GC
		opts.Keep = s.ID
		if l.removed, err = gcRemote(ctx, l.mux, opts); err != nil {
			return info, fmt.Errorf("gc on %s: %w", l.host, err)
		}
	}
	origin := l.cfg.GC.Origin
	if origin == "" {
		origin = Origin()
	}
	mk := fmt.Sprintf(`umask 077 && d=$(mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/silo.XXXXXXXX") && printf 'id=%%s origin=%%s\n' %s %s > "$d/%s" && printf '%%s' "$d"`,
		Quote(s.ID), Quote(origin), SessionFile)
	res, err := l.mux(ctx, mk, nil)
	if err != nil {
		return info, err
	}
	if res.Code != 0 || !strings.HasPrefix(res.Stdout, "/") {
		return info, res.err("creating the remote session directory on " + l.host)
	}
	l.rdir = res.Stdout
	return info, nil
}

// RemoteGC lists the stale remote session directories Prepare removed.
func (l *Location) RemoteGC() []string { return l.removed }

// RemoteDir is the remote session directory, once Prepare made it.
func (l *Location) RemoteDir() string { return l.rdir }

// controlPath is the master's socket: $SESSION/ssh.ctl, or a short
// directory of its own when that would not fit in a socket address.
func (l *Location) controlPath() (string, error) {
	p := filepath.Join(l.s.Dir, "ssh.ctl")
	if len(p) < 100 {
		return p, nil
	}
	d, err := os.MkdirTemp("", "silo-ssh-")
	if err != nil {
		return "", err
	}
	l.ctlDir = d
	return filepath.Join(d, "ctl"), nil
}

// MasterFlags are the master connection's flags (design §5.3), without
// -f: silo keeps the master as its own child, under Pdeathsig, so its
// exit is one event silo watches and silo's death takes it along.
func masterFlags(ctl string) []string {
	return []string{"-M", "-S", ctl, "-o", "ControlPersist=no", "-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4", "-o", "ExitOnForwardFailure=yes", "-n", "-N"}
}

// muxFlags send a command over the master. ControlMaster=no stops a
// user's ControlMaster=auto from making another master; BatchMode stops
// anything from prompting.
func (l *Location) muxFlags() []string {
	return []string{"-S", l.ctl, "-o", "ControlMaster=no", "-o", "BatchMode=yes"}
}

func (l *Location) openMaster(ctx context.Context) error {
	ctl, err := l.controlPath()
	if err != nil {
		return err
	}
	l.ctl = ctl
	argv := l.t.Argv(masterFlags(ctl), "")
	cmd := exec.Command(argv[0], argv[1:]...)
	l.masterErr = &capWriter{max: 64 << 10, after: l.cfg.Stderr}
	cmd.Stderr = l.masterErr
	procgroup.DieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		return &location.PrerequisiteError{What: fmt.Sprintf("cannot run ssh: %v", err), Fix: "install OpenSSH's ssh client"}
	}
	l.master, l.masterDone = cmd, make(chan struct{})
	go func() {
		cmd.Wait()
		close(l.masterDone)
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-l.masterDone:
			return l.connectError()
		case <-ctx.Done():
			cmd.Process.Kill()
			return ctx.Err()
		case <-tick.C:
		}
		if l.masterUp(ctx) {
			l.masterErr.ready()
			return nil
		}
	}
}

// masterUp asks the master whether it is running.
func (l *Location) masterUp(ctx context.Context) bool {
	select {
	case <-l.masterDone:
		return false
	default:
	}
	if _, err := os.Stat(l.ctl); err != nil {
		return false
	}
	res, err := run(ctx, l.t.Argv([]string{"-S", l.ctl, "-O", "check"}, ""), nil)
	return err == nil && res.Code == 0
}

// MasterGone reports whether the master connection has exited.
func (l *Location) MasterGone() bool {
	if l.masterDone == nil {
		return true
	}
	select {
	case <-l.masterDone:
		return true
	default:
		return false
	}
}

// MasterDone is closed when the master connection exits.
func (l *Location) MasterDone() <-chan struct{} { return l.masterDone }

var unknownKey = regexp.MustCompile(`(?i)host key verification failed|no \S+ host key is known|host key for .* is not known`)

// connectError explains why the master did not come up.
func (l *Location) connectError() error {
	msg := strings.TrimSpace(l.masterErr.String())
	switch {
	case strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return &location.PrerequisiteError{
			What: fmt.Sprintf("the host key of %s has changed: %s", l.host, msg),
			Fix:  "find out why before anything else; silo never replaces a host key",
		}
	case unknownKey.MatchString(msg):
		return &location.PrerequisiteError{What: fmt.Sprintf("the host key of %s is not known (silo never accepts one)", l.host), Fix: l.acceptKeyFix()}
	}
	return &location.PrerequisiteError{
		What: fmt.Sprintf("cannot connect to %s: %s", l.host, msg),
		Fix:  fmt.Sprintf("check that `ssh %s true` works", l.host),
	}
}

// acceptKeyFix is the command that accepts the host's key, which the
// user runs after checking its fingerprint.
func (l *Location) acceptKeyFix() string {
	return AcceptKeyFix(context.Background(), l.t)
}

// AcceptKeyFix is the command that accepts t's host key, after the user
// checks its fingerprint.
func AcceptKeyFix(ctx context.Context, t Transport) string {
	host, port := t.Host(), "22"
	if res, err := run(ctx, t.Argv([]string{"-G"}, ""), nil); err == nil && res.Code == 0 {
		for _, line := range strings.Split(res.Stdout, "\n") {
			k, v, _ := strings.Cut(line, " ")
			switch k {
			case "hostname":
				host = v
			case "port":
				port = v
			}
		}
	}
	return fmt.Sprintf("check the key's fingerprint, then accept it with `ssh-keyscan -p %s %s >> ~/.ssh/known_hosts` (or connect once with `ssh %s` and answer yes)", port, host, t.Host())
}

// mux runs remote over the master.
func (l *Location) mux(ctx context.Context, remote string, stdin []byte) (result, error) {
	if l.MasterGone() {
		return result{Code: 255}, fmt.Errorf("the connection to %s is gone", l.host)
	}
	return run(ctx, l.t.Argv(append(l.muxFlags(), "-T"), remote), stdin)
}

// Run runs a command line on the host over the master connection, for
// what later shares it (remote MCP servers, stage 5b). It returns the
// command's stdout, stderr and exit code.
func (l *Location) Run(ctx context.Context, remote string, stdin []byte) (stdout, stderr string, code int, err error) {
	res, err := l.mux(ctx, remote, stdin)
	return res.Stdout, res.Stderr, res.Code, err
}

// ForwardLocal asks the master for a local forward (-L spec), for remote
// MCP servers (stage 5b).
func (l *Location) ForwardLocal(ctx context.Context, spec string) error {
	return l.forward(ctx, "-L", spec)
}

func (l *Location) forward(ctx context.Context, kind, spec string) error {
	if l.MasterGone() {
		return fmt.Errorf("the connection to %s is gone", l.host)
	}
	res, err := run(ctx, l.t.Argv(append(l.muxFlags(), "-O", "forward", kind, spec), ""), nil)
	if err == nil && res.Code != 0 {
		err = res.err("ssh -O forward " + kind + " " + spec)
	}
	return err
}

// Gateway asks the master for the reverse forward: a port the server
// allocates on the remote loopback, to the gateway's local port.
func (l *Location) Gateway(ctx context.Context, local net.Addr) (string, error) {
	tcp, ok := local.(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("ssh: the gateway is at %s, not a TCP address", local)
	}
	if l.MasterGone() {
		return "", fmt.Errorf("the connection to %s is gone", l.host)
	}
	spec := fmt.Sprintf("127.0.0.1:0:127.0.0.1:%d", tcp.Port)
	res, err := run(ctx, l.t.Argv(append(l.muxFlags(), "-O", "forward", "-R", spec), ""), nil)
	if err != nil {
		return "", err
	}
	port, perr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if res.Code != 0 || perr != nil || port <= 0 || port > 65535 {
		return "", res.err("the reverse forward to the gateway on " + l.host)
	}
	binds, err := l.listening(ctx, port)
	if err != nil {
		return "", err
	}
	for _, b := range binds {
		if !loopback(b) {
			_, _ = run(ctx, l.t.Argv(append(l.muxFlags(), "-O", "cancel", "-R", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", port, tcp.Port)), ""), nil)
			return "", &location.PrerequisiteError{
				What: fmt.Sprintf("sshd on %s bound the gateway's reverse forward to %s, not the loopback address silo asked for: its GatewayPorts is yes, which overrides the client's bind address", l.host, b),
				Fix:  "set GatewayPorts to no (the default) or clientspecified in the host's sshd_config",
			}
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", port), nil
}

// listening lists the addresses the host has a listening TCP socket on
// at port: from /proc/net on Linux, or netstat elsewhere.
func (l *Location) listening(ctx context.Context, port int) ([]string, error) {
	script := fmt.Sprintf(`if [ -r /proc/net/tcp ]; then
  for f in /proc/net/tcp /proc/net/tcp6; do [ -r "$f" ] && cat "$f"; done | awk '$4 == "0A" && $2 ~ /:%04X$/ {print "proc " $2}'
else
  netstat -an 2>/dev/null | awk '/LISTEN/ {print "netstat " $4}' | grep -E '[.:]%d$'
fi
exit 0
`, port, port)
	res, err := l.mux(ctx, "sh -s", []byte(script))
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, res.err("checking the reverse forward's bind address on " + l.host)
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			out = append(out, f[0]+":"+f[1])
		}
	}
	return out, nil
}

// loopback reports whether a listening address from listening is on
// loopback only.
func loopback(b string) bool {
	kind, addr, _ := strings.Cut(b, ":")
	if kind == "proc" {
		host, _, _ := strings.Cut(addr, ":")
		switch len(host) {
		case 8: // IPv4, little-endian: 127.x.x.x ends in 7F
			return strings.HasSuffix(host, "7F")
		case 32:
			return host == "00000000000000000000000001000000"
		}
		return false
	}
	return strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "::1")
}

// Stage writes each file into <remote session dir>/agent with umask 077
// and cat > over the master.
func (l *Location) Stage(ctx context.Context, files []location.StagedFile) (string, error) {
	if l.rdir == "" {
		return "", errors.New("ssh: Stage before Prepare")
	}
	dir := l.rdir + "/agent"
	for _, f := range files {
		name := path.Clean(f.Name)
		if path.IsAbs(name) || name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return "", fmt.Errorf("ssh: staged file %q is outside the staging directory", f.Name)
		}
		p := dir + "/" + name
		script := "umask 077 && mkdir -p -- " + Quote(path.Dir(p)) + " && "
		var stdin []byte
		if f.Link != "" {
			script += "ln -sfn -- " + Quote(f.Link) + " " + Quote(p)
		} else {
			script += "cat > " + Quote(p)
			if f.Mode != 0 && f.Mode.Perm() != 0o600 {
				script += fmt.Sprintf(" && chmod %o %s", f.Mode.Perm(), Quote(p))
			}
			stdin = f.Data
		}
		res, err := l.mux(ctx, script, stdin)
		if err != nil {
			return "", err
		}
		if res.Code != 0 {
			return "", res.err("staging " + f.Name + " on " + l.host)
		}
	}
	return dir, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Start writes the CLI's environment to a 0600 env file, which the
// command's preamble sources and deletes, so no value appears in a
// remote argv, and starts the CLI with ssh -t (interactive) or ssh -T
// (headless).
func (l *Location) Start(ctx context.Context, c location.Command, sio location.IO) (location.Agent, error) {
	if len(c.Argv) == 0 {
		return nil, errors.New("ssh: empty command")
	}
	if l.rdir == "" {
		return nil, errors.New("ssh: Start before Prepare")
	}
	if l.MasterGone() {
		return nil, fmt.Errorf("the connection to %s is gone", l.host)
	}
	envFile, pidFile := l.rdir+"/env", l.rdir+"/pid"
	var env bytes.Buffer
	for _, kv := range c.Env {
		k, v, _ := strings.Cut(kv, "=")
		if !envName.MatchString(k) {
			return nil, fmt.Errorf("ssh: %q is not an environment variable name", k)
		}
		fmt.Fprintf(&env, "export %s=%s\n", k, Quote(v))
	}
	if env.Len() > 0 {
		res, err := l.mux(ctx, "umask 077 && cat > "+Quote(envFile), env.Bytes())
		if err != nil {
			return nil, err
		}
		if res.Code != 0 {
			return nil, res.err("writing the agent's environment on " + l.host)
		}
	}
	wd := c.Workdir
	pre := ""
	if wd == "" {
		wd = l.rdir + "/work"
		pre = "mkdir -m 700 -p -- " + Quote(wd) + " && "
	}
	// The remote login shell starts this; sh then becomes the CLI with
	// exec, so its pid is that of the session sshd made, and of the
	// session's process group, which Signal signals.
	script := pre + "cd -- " + Quote(wd) + " || exit 1; " +
		"if [ -f " + Quote(envFile) + " ]; then . " + Quote(envFile) + "; rm -f -- " + Quote(envFile) + "; fi; " +
		"echo $$ > " + Quote(pidFile) + "; exec " + QuoteArgv(c.Argv)
	flags := l.muxFlags()
	if c.TTY {
		flags = append(flags, "-t")
		if !isTerminal(sio.Stdin) {
			flags = append(flags, "-t")
		}
	} else {
		flags = append(flags, "-T")
	}
	argv := l.t.Argv(flags, "exec /bin/sh -c "+Quote(script))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = sio.Stdout, sio.Stderr
	if c.TTY {
		// ssh puts silo's terminal in raw mode and carries window-size
		// changes itself.
		cmd.Stdin = sio.Stdin
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin = bytes.NewReader(c.Stdin)
		if c.Input != nil {
			// ssh -T carries it to the CLI's stdin as it is written.
			cmd.Stdin = c.Input
		}
	}
	procgroup.DieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a := &agent{l: l, cmd: cmd, pidFile: pidFile, done: make(chan struct{})}
	go func() {
		a.err = cmd.Wait()
		close(a.done)
	}()
	return a, nil
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Teardown removes the remote session directory and closes the master.
// Against a connection that is already gone it removes nothing remote:
// the directory is left for silo gc --host, or the next silo run to the
// host. It runs once; later calls do nothing.
func (l *Location) Teardown(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.torn {
		return nil
	}
	l.torn = true
	var errs []error
	if l.rdir != "" && !l.MasterGone() {
		res, err := l.mux(ctx, "rm -rf -- "+Quote(l.rdir), nil)
		if err == nil && res.Code != 0 {
			err = res.err("removing " + l.rdir + " on " + l.host)
		}
		errs = append(errs, err)
	}
	if l.master != nil && !l.MasterGone() {
		_, _ = run(ctx, l.t.Argv([]string{"-S", l.ctl, "-O", "exit"}, ""), nil)
		select {
		case <-l.masterDone:
		case <-time.After(5 * time.Second):
			l.master.Process.Kill()
			<-l.masterDone
		}
	}
	if l.ctlDir != "" {
		errs = append(errs, os.RemoveAll(l.ctlDir))
	}
	return errors.Join(errs...)
}

type agent struct {
	l       *Location
	cmd     *exec.Cmd
	pidFile string
	done    chan struct{}
	err     error
}

// Wait returns the remote command's exit status, which ssh passes on.
// ssh's own 255 becomes ConnLost (exit 75) once the master is confirmed
// gone; with the master still up, it is the remote command's 255.
func (a *agent) Wait() (location.ExitStatus, error) {
	<-a.done
	if a.err == nil {
		return location.ExitStatus{}, nil
	}
	var ee *exec.ExitError
	if !errors.As(a.err, &ee) {
		return location.ExitStatus{Code: 1}, a.err
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return location.ExitStatus{Code: 128 + int(ws.Signal()), Signaled: ws.Signal().String()}, nil
	}
	code := ee.ExitCode()
	if code == 255 {
		// The master notices a dead connection at the same moment as its
		// clients, or within its keep-alive period; give it a moment.
		select {
		case <-a.l.masterDone:
		case <-time.After(2 * time.Second):
		}
		if a.l.MasterGone() || !a.l.masterUp(context.Background()) {
			return location.ExitStatus{Code: 75, ConnLost: true}, nil
		}
	}
	return location.ExitStatus{Code: code}, nil
}

// Signal signals the remote CLI's process group over the master. A
// kill also ends the local ssh, which tears down the channel; so does
// any signal once the master is gone.
func (a *agent) Signal(sig location.Signal) error {
	select {
	case <-a.done:
		return nil
	default:
	}
	name := map[location.Signal]string{location.Interrupt: "INT", location.Terminate: "TERM", location.Kill: "KILL"}[sig]
	sent := false
	if !a.l.MasterGone() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p := Quote(a.pidFile)
		res, err := a.l.mux(ctx, fmt.Sprintf(`p=$(cat %s 2>/dev/null) && [ -n "$p" ] && { kill -s %s -- "-$p" 2>/dev/null || kill -s %s "$p"; }`, p, name, name), nil)
		sent = err == nil && res.Code == 0
	}
	if sig == location.Kill || !sent {
		if err := a.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	return nil
}

// Resize does nothing: ssh -t carries window-size changes itself.
func (a *agent) Resize(rows, cols int) error { return nil }

// PGID is the local ssh's process group in headless mode, which
// session.json records for silo gc; 0 in interactive mode.
func (a *agent) PGID() int {
	if a.cmd.SysProcAttr == nil || !a.cmd.SysProcAttr.Setpgid {
		return 0
	}
	return a.cmd.Process.Pid
}

// capWriter keeps the first max bytes the master writes, for the error
// that explains a failed connection, and once the master is up passes
// what it writes on to after.
type capWriter struct {
	mu    sync.Mutex
	max   int
	buf   bytes.Buffer
	up    bool
	after io.Writer
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.up {
		if w.after != nil {
			w.after.Write(p)
		}
		return len(p), nil
	}
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

func (w *capWriter) ready() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.up = true
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
