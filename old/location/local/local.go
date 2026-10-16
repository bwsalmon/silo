// Adapted from github.com/bwsalmon/grain pkg/agent/host.go (LocalHost) at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

// Package local is the local location (design §5.2): the agent CLI is a
// child of silo run, with a scrubbed environment, sharing silo's
// terminal in interactive mode and on pipes in headless mode.
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/old/internal/procgroup"
	"github.com/bwsalmon/silo/old/location"
)

// Allowlist is the invoking environment the CLI keeps (design §5.2,
// call 6): these names, and any LC_* or XDG_* variable. Everything else
// is dropped, so a stray AWS_SECRET_ACCESS_KEY never reaches the agent.
var Allowlist = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TMPDIR"}

// Scrub returns the entries of environ the allowlist keeps.
func Scrub(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		keep := strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "XDG_")
		for _, a := range Allowlist {
			keep = keep || k == a
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

// Location runs the CLI on the invoking machine.
type Location struct {
	s        *location.Session
	stageDir string
	workDir  string
}

var _ location.Location = (*Location)(nil)

// New returns a local location.
func New() *Location { return &Location{} }

func getenv(env []string, k string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if key, v, ok := strings.Cut(env[i], "="); ok && key == k {
			return v
		}
	}
	return ""
}

// lookPath finds name on the PATH of env, not silo's own.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		fi, err := os.Stat(name)
		if err != nil {
			return "", err
		}
		if fi.IsDir() || fi.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s is not an executable file", name)
		}
		return filepath.Abs(name)
	}
	for _, dir := range filepath.SplitList(getenv(env, "PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return filepath.Abs(p)
		}
	}
	return "", exec.ErrNotFound
}

// Prepare finds the binary, asks it for its version, and makes the
// private working directory.
func (l *Location) Prepare(ctx context.Context, s *location.Session) (location.HostInfo, error) {
	l.s = s
	env := Scrub(s.Environ)
	info := location.HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, Env: env, Home: getenv(env, "HOME")}
	if info.Home == "" {
		info.Home, _ = os.UserHomeDir()
	}
	name := s.Binary
	if name == "" {
		name = s.CLI
	}
	bin, err := lookPath(name, env)
	if err != nil {
		fix := fmt.Sprintf("install %s, or set agent.binary to its path", s.CLI)
		if s.Binary != "" {
			fix = "check agent.binary"
		}
		return info, &location.PrerequisiteError{What: fmt.Sprintf("the agent CLI %s was not found (%v)", name, err), Fix: fix}
	}
	info.Binary = bin
	if len(s.VersionArgs) > 0 {
		vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(vctx, bin, s.VersionArgs...)
		cmd.Env = env
		procgroup.DieWithParent(cmd)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return info, &location.PrerequisiteError{
				What: fmt.Sprintf("%s %s failed: %v: %s", bin, strings.Join(s.VersionArgs, " "), err, strings.TrimSpace(errb.String())),
				Fix:  "check that the CLI is installed and runs",
			}
		}
		info.Version, _, _ = strings.Cut(strings.TrimSpace(out.String()), "\n")
	}
	return info, nil
}

// Gateway returns the loopback URL: the CLI is on the same machine.
func (l *Location) Gateway(ctx context.Context, local net.Addr) (string, error) {
	return "http://" + local.String() + "/mcp", nil
}

// Stage writes files into <session dir>/agent, mode 0700, each file
// 0600.
func (l *Location) Stage(ctx context.Context, files []location.StagedFile) (string, error) {
	if l.s == nil {
		return "", errors.New("local: Stage before Prepare")
	}
	dir := filepath.Join(l.s.Dir, "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	l.stageDir = dir
	for _, f := range files {
		p := filepath.Join(dir, f.Name)
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return "", fmt.Errorf("local: staged file %q is outside the staging directory", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", err
		}
		if f.Link != "" {
			if err := os.Symlink(f.Link, p); err != nil {
				return "", err
			}
			continue
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o600
		}
		if err := os.WriteFile(p, f.Data, mode); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Start starts the CLI as a child process.
func (l *Location) Start(ctx context.Context, c location.Command, io location.IO) (location.Agent, error) {
	if len(c.Argv) == 0 {
		return nil, errors.New("local: empty command")
	}
	wd := c.Workdir
	if wd == "" {
		if l.s == nil {
			return nil, errors.New("local: Start before Prepare")
		}
		wd = filepath.Join(l.s.Dir, "work")
		if err := os.MkdirAll(wd, 0o700); err != nil {
			return nil, err
		}
		l.workDir = wd
	}
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = wd
	cmd.Stdout, cmd.Stderr = io.Stdout, io.Stderr
	if c.TTY {
		// The CLI shares the terminal and its foreground process group,
		// so the terminal's Ctrl-C reaches it directly (design §3.1).
		cmd.Stdin = io.Stdin
	} else {
		// Headless: its own process group, so silo's signals reach the
		// CLI and everything it started, and the terminal's do not.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin = bytes.NewReader(c.Stdin)
		if c.Input != nil {
			cmd.Stdin = c.Input
		}
	}
	procgroup.DieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a := &agent{cmd: cmd, group: !c.TTY, done: make(chan struct{})}
	go func() {
		a.err = cmd.Wait()
		close(a.done)
	}()
	return a, nil
}

// Teardown removes the staged files and the private working directory.
func (l *Location) Teardown(ctx context.Context) error {
	var errs []error
	for _, d := range []string{l.stageDir, l.workDir} {
		if d != "" {
			if err := os.RemoveAll(d); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

type agent struct {
	cmd   *exec.Cmd
	group bool
	done  chan struct{}
	err   error
}

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
	return location.ExitStatus{Code: ee.ExitCode()}, nil
}

func (a *agent) Signal(sig location.Signal) error {
	select {
	case <-a.done:
		return nil
	default:
	}
	s := map[location.Signal]syscall.Signal{location.Interrupt: syscall.SIGINT, location.Terminate: syscall.SIGTERM, location.Kill: syscall.SIGKILL}[sig]
	pid := a.cmd.Process.Pid
	if a.group {
		if err := syscall.Kill(-pid, s); err == nil {
			return nil
		}
	}
	if err := syscall.Kill(pid, s); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// Resize does nothing: the CLI holds the terminal itself, and the
// kernel tells it of size changes.
func (a *agent) Resize(rows, cols int) error { return nil }

// PGID is the CLI's process group in headless mode, which session.json
// records for silo gc; 0 in interactive mode, where it has none of its
// own.
func (a *agent) PGID() int {
	if !a.group {
		return 0
	}
	return a.cmd.Process.Pid
}
