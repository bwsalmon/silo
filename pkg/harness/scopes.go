package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ScopePrefix starts the name of every ambient session's scope.
const ScopePrefix = "silo-agent-"

// ScopeName is the transient systemd scope agent's ambient harness runs
// in, silo-agent-<agent id>.scope (local-backend.md §10, call L17). Its
// cgroup is the session's identity to the session socket (call L18).
func ScopeName(agent string) string { return ScopePrefix + agent + ".scope" }

// Scopes is the Runtime of ambient sessions, the local backend's
// (local-backend.md §10, call L17). The harness CLI is the host's own,
// on the agent user's PATH; silod doesn't install, pin or update it.
// Each turn runs it on the host, as the agent user, with that user's
// HOME, environment and login, in a transient systemd scope,
// silo-agent-<agent id>.scope, in the agent user's systemd manager.
// silod runs as the agent user, so the scope is the user's own
// (systemd-run --user --scope); the agent user is never root.
//
// What fences the session is not a container: the CLI's own tools are
// off, its one MCP server is silod mcp-stdio on the session socket, and
// every command it runs goes through that to the agent's tool
// container. The scope holds the CLI and the processes it starts, its
// limits are the session's, and the session socket accepts a
// connection only from a process in it.
//
// A session's state is the CLI's own session file, in the agent user's
// home, which stays on the host between turns and across silod's
// restarts; Turn returns none for the database, and Restore is not
// used. Each session has a working directory of its own,
// <StateDir>/<agent>, which is the CLI's cwd (and so, for Claude Code,
// where its session file is filed).
type Scopes struct {
	// StateDir holds each session's working directory.
	StateDir string
	// SystemdRun and Systemctl are the commands; "systemd-run" and
	// "systemctl" on PATH if empty.
	SystemdRun, Systemctl string
}

var _ Runtime = (*Scopes)(nil)

func (s *Scopes) systemdRun() string { return cmdOr(s.SystemdRun, "systemd-run") }
func (s *Scopes) systemctl() string  { return cmdOr(s.Systemctl, "systemctl") }

func cmdOr(c, def string) string {
	if c == "" {
		return def
	}
	return c
}

// Dir is agent's working directory.
func (s *Scopes) Dir(agent string) string { return filepath.Join(s.StateDir, agent) }

// ScopeArgv is the systemd-run invocation of one turn of spec: a scope
// named after the agent, with its limits, running spec.Argv.
func ScopeArgv(systemdRun string, spec Spec) []string {
	lim := Limits{MemoryBytes: spec.MemoryBytes, CPUShares: spec.CPUShares, Pids: spec.Pids}.orDefault()
	// systemd's CPUWeight is 1-10000 around 100, as docker's shares
	// are around 1024.
	weight := max(1, lim.CPUShares*100/1024)
	return append([]string{systemdRun, "--user", "--scope", "--quiet", "--collect",
		"--unit=" + ScopeName(spec.Agent),
		"--description=Silo agent " + spec.Agent + " harness",
		"-p", "MemoryMax=" + strconv.FormatInt(lim.MemoryBytes, 10),
		"-p", "MemorySwapMax=0",
		"-p", "TasksMax=" + strconv.FormatInt(lim.Pids, 10),
		"-p", "CPUWeight=" + strconv.FormatInt(weight, 10),
		"--"}, spec.Argv...)
}

// Turn implements Runtime: spec.Argv is the CLI's own command, run in
// the agent's scope in its working directory, with spec.Env added to
// the agent user's environment.
func (s *Scopes) Turn(ctx context.Context, spec Spec, line func([]byte)) (Result, error) {
	if !ValidAgent(spec.Agent) {
		return Result{}, fmt.Errorf("harness: agent %q is not an agent id", spec.Agent)
	}
	if len(spec.Argv) == 0 {
		return Result{}, errors.New("harness: no command")
	}
	dir := s.Dir(spec.Agent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, err
	}
	// A scope left by a turn silod didn't see end (it was killed) would
	// make this one's name taken.
	s.stop(ctx, spec.Agent)
	argv := ScopeArgv(s.systemdRun(), spec)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), spec.Env...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{Exit: 127}, err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line(bytes.Clone(sc.Bytes()))
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		res := Result{Exit: ee.ExitCode()}
		if res.Exit < 0 {
			res.Exit = 128
		}
		if ctx.Err() == nil && stderr.Len() > 0 {
			log.Printf("harness: %s: %s exited %d: %s", spec.Agent, spec.Argv[0], res.Exit, strings.TrimSpace(stderr.String()))
		}
		return res, nil
	case err != nil:
		return Result{Exit: 1}, err
	}
	return Result{}, nil
}

// HostSpec is one turn of agent's ambient session, for Scopes: cmd as
// the adapter built it, and the session's limits.
func HostSpec(agent string, cmd Command, lim Limits) Spec {
	lim = lim.orDefault()
	return Spec{Agent: agent, Argv: cmd.Argv, Env: cmd.Env, Network: NetworkAmbient,
		MemoryBytes: lim.MemoryBytes, CPUShares: lim.CPUShares, Pids: lim.Pids}
}

// Remove implements Runtime: the agent's scope is stopped, which kills
// what is left in it, and its working directory removed. The CLI's
// session file, in the agent user's home, stays, for a resume.
func (s *Scopes) Remove(ctx context.Context, agent string) error {
	if !ValidAgent(agent) {
		return fmt.Errorf("harness: agent %q is not an agent id", agent)
	}
	s.stop(ctx, agent)
	return os.RemoveAll(s.Dir(agent))
}

// stop stops agent's scope; one that isn't there is no error.
func (s *Scopes) stop(ctx context.Context, agent string) {
	exec.CommandContext(ctx, s.systemctl(), "--user", "stop", ScopeName(agent)).Run()
}

// List implements Runtime: the agents with a scope in the agent user's
// manager, which on silod's start are the leftovers it kills.
func (s *Scopes) List(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, s.systemctl(), "--user", "list-units", "--type=scope", "--all",
		"--plain", "--no-legend", "--no-pager", ScopePrefix+"*").Output()
	if err != nil {
		return nil, fmt.Errorf("listing the agent scopes: %w", err)
	}
	var agents []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		a, ok := strings.CutPrefix(f[0], ScopePrefix)
		if a, ok2 := strings.CutSuffix(a, ".scope"); ok && ok2 && ValidAgent(a) {
			agents = append(agents, a)
		}
	}
	return agents, nil
}

// Login implements Runtime: an ambient session has no login files of
// silod's; the CLI keeps its own.
func (s *Scopes) Login(context.Context, string, []string) (map[string][]byte, error) { return nil, nil }

// Version is what the CLI on the agent user's PATH says its version is,
// run with argv (Versioned.VersionArgv).
func (s *Scopes) Version(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("harness: no version command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var e *exec.Error
		if errors.As(err, &e) {
			return "", fmt.Errorf("%s is not installed on this host, or not on the agent user's PATH", argv[0])
		}
		return "", fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	v, ok := ParseVersion(string(out))
	if !ok {
		return "", fmt.Errorf("%s printed no version: %q", strings.Join(argv, " "), strings.TrimSpace(string(out)))
	}
	return v, nil
}

// tailBuffer keeps the last 4 KiB written to it.
type tailBuffer struct{ bytes.Buffer }

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	t.Buffer.Write(p)
	if over := t.Len() - 4096; over > 0 {
		t.Next(over)
	}
	return n, nil
}
