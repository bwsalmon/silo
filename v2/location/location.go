// Adapted from github.com/bwsalmon/grain pkg/agent/host.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

// Package location is where one session's agent CLI runs (design §5):
// the Location interface every kind implements, and the Agent handle on
// a running CLI. Kinds live in subpackages (local; ssh in stage 4b).
package location

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
)

// Session is what a location is told about the session it serves.
type Session struct {
	// ID is the session id, s- plus 12 base32 characters.
	ID string
	// Dir is the session directory on the invoking machine (mode 0700).
	Dir string
	// CLI is the agent CLI: claude, codex, agy or custom.
	CLI string
	// Binary is agent.binary, or empty to find CLI on the agent host's
	// PATH.
	Binary string
	// VersionArgs asks the binary for its version (["--version"]); nil
	// skips the check.
	VersionArgs []string
	// Mode is interactive or headless.
	Mode string
	// Environ is the invoking environment, from which a local location
	// keeps only its allowlist (design §5.2).
	Environ []string
}

// HostInfo is what Prepare learns about the agent host.
type HostInfo struct {
	OS, Arch string
	// Binary is the CLI's absolute path on the agent host.
	Binary string
	// Version is the first line of `<binary> --version`, as printed.
	Version string
	// Home is the user's home directory on the agent host.
	Home string
	// Env is the environment the CLI starts from on the agent host,
	// before the mapping adds its own: for local, the scrubbed invoking
	// environment.
	Env []string
}

// StagedFile is one file the CLI needs on the agent host.
type StagedFile struct {
	// Name is relative to the staging directory, and may have
	// directories in it ("codex-home/config.toml").
	Name string
	Data []byte
	// Mode is 0600 when zero.
	Mode fs.FileMode
	// Link, when set, makes Name a symbolic link to Link (an absolute
	// path on the agent host) instead of a file holding Data.
	Link string
}

// Command is how to start the CLI.
type Command struct {
	// Argv is the full command line, argv[0] the binary.
	Argv []string
	// Env is the CLI's whole environment.
	Env []string
	// Workdir is the working directory; empty means a private empty
	// directory the location makes (design §6.1).
	Workdir string
	// TTY is set for interactive mode: the CLI shares the terminal.
	TTY bool
	// Stdin, for headless mode, is written to the CLI's stdin, which is
	// then closed.
	Stdin []byte
}

// IO is where the CLI's streams go: silo's terminal in interactive mode,
// pipes in headless mode.
type IO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Signal is what Agent.Signal sends.
type Signal int

const (
	Interrupt Signal = iota // SIGINT
	Terminate               // SIGTERM
	Kill                    // SIGKILL
)

func (s Signal) String() string {
	return [...]string{"interrupt", "terminate", "kill"}[s]
}

// ExitStatus is how the CLI ended.
type ExitStatus struct {
	// Code is the exit code, or 128+n when it was killed by signal n.
	Code int
	// Signaled names the signal that killed it, if one did.
	Signaled string
}

// Location is where one session's agent CLI runs (design §5.1).
type Location interface {
	// Prepare checks prerequisites and readies the host. It returns the
	// facts the CLI mapping needs (OS, arch, the CLI binary's path and
	// version, the user's home) or a *PrerequisiteError naming what is
	// missing and how to fix it.
	Prepare(ctx context.Context, s *Session) (HostInfo, error)

	// Gateway makes the gateway reachable from the agent host and returns
	// the URL the CLI's MCP config must name. local: the loopback URL.
	// ssh: a reverse forward. k8s: a port-forward or sidecar. ax: a tunnel.
	Gateway(ctx context.Context, local net.Addr) (url string, err error)

	// Stage writes files the CLI needs (MCP config, settings, prompts)
	// into a session directory on the agent host and returns their paths.
	Stage(ctx context.Context, files []StagedFile) (dir string, err error)

	// Start starts the CLI. For local and ssh silo starts the process.
	// For a hosted runtime the runtime starts it; Start submits the
	// session and returns a handle to it.
	Start(ctx context.Context, cmd Command, io IO) (Agent, error)

	// Teardown removes everything Prepare and Stage made. It must succeed
	// against a host that is half set up, or already gone.
	Teardown(ctx context.Context) error
}

// Agent is the running CLI, however it was started.
type Agent interface {
	Wait() (ExitStatus, error)
	Signal(sig Signal) error     // interrupt, terminate, kill
	Resize(rows, cols int) error // interactive only
}

// PrerequisiteError is a missing prerequisite: silo exits 69.
type PrerequisiteError struct {
	What string // what is missing
	Fix  string // how to fix it
}

func (e *PrerequisiteError) Error() string {
	if e.Fix == "" {
		return e.What
	}
	return fmt.Sprintf("%s: %s", e.What, e.Fix)
}
