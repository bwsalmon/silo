// Package sandbox is the sandbox backend interface (design §7.3): a
// Backend creates sandboxes, and finds them again by the session label
// they carry; a Sandbox runs commands and reads and writes files.
// Backends live in subpackages (container: Docker or Podman, with
// gVisor as a runtime).
package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/bwsalmon/silo/v2/internal/safepath"
	"github.com/bwsalmon/silo/v2/silofile"
)

// SessionLabel is the label every sandbox carries, with the session id
// as its value (design §3.1), so that silo gc and Find can find it.
const SessionLabel = "silo.session"

// UserLabel carries the uid of the user who created the sandbox, so
// that silo gc on a shared Docker daemon touches only its own.
const UserLabel = "silo.uid"

// SessionLister is a Backend that can list the sessions its sandboxes
// belong to, for silo gc.
type SessionLister interface {
	Sessions(ctx context.Context) ([]string, error)
}

// Networker is a Backend that can make a session a private network:
// its sandboxes reach the invoking machine on HostIP through it, and
// nothing else (design §9.3).
type Networker interface {
	CreateNetwork(ctx context.Context, session string) (Network, error)
	// RemoveNetwork removes a network CreateNetwork made, once no
	// sandbox is attached to it. A network already gone is not an
	// error.
	RemoveNetwork(ctx context.Context, name string) error
}

// Network is a private network a Networker made.
type Network struct {
	Name string
	// HostIP is the invoking machine's address on the network: where
	// a service the session's sandboxes reach listens.
	HostIP string
}

// NameLabel carries the sandbox's name in the Silofile.
const NameLabel = "silo.sandbox"

// Backend creates sandboxes.
type Backend interface {
	Create(ctx context.Context, spec SandboxSpec) (Sandbox, error)
	// Find lists sandboxes carrying a session label, for silo gc and
	// re-attach.
	Find(ctx context.Context, session string) ([]Sandbox, error)
}

// Sandbox is one running sandbox.
type Sandbox interface {
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
	// ReadFile returns at most limit bytes of the file at path.
	ReadFile(ctx context.Context, path string, limit int64) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error
	// DeliverFile places a credential file and replaces it atomically
	// on refresh (the GCP token of §11).
	DeliverFile(ctx context.Context, path string, data []byte) error
	Destroy(ctx context.Context) error
}

// SandboxSpec is what a Backend creates: one sandboxes[] entry with its
// paths resolved, plus the session it belongs to.
type SandboxSpec struct {
	Session string
	Name    string
	// Kind is container, gvisor or granule.
	Kind string
	// Image is an OCI image reference (for granule, a granule VM
	// image); Build, when Image is empty, the
	// absolute path of a Dockerfile, built with its directory as the
	// context.
	Image string
	Build string
	// Workdir is the working directory inside the sandbox, created if
	// missing. Empty means the image's own.
	Workdir string
	CPUs    float64
	// Memory is in bytes; 0 is no limit.
	Memory int64
	// Disk is in bytes; 0 is the image's own size. Only granule
	// sandboxes apply it.
	Disk int64
	// Network is "none" (the default) or "open".
	Network string
	// PrivateNetwork, when set, is a network made by Networker.
	// CreateNetwork that the sandbox joins: in place of no network for
	// "none", beside the runtime's default for "open". It is how a
	// sandbox reaches the session's git proxy (design §9.3).
	PrivateNetwork string
	Mounts         []Mount
	// Env is the sandbox's environment on top of the image's: the
	// Silofile's env, and credentials delivered as {sandbox, env}.
	Env map[string]string
}

// Mount is a host directory or file bound into the sandbox.
type Mount struct {
	// From is an absolute path on the invoking machine.
	From string
	// To is an absolute path in the sandbox.
	To string
	RW bool
}

// ExecRequest is one command to run.
type ExecRequest struct {
	// Argv is run as it is, without a shell: sandbox_exec passes
	// /bin/sh -c <command>.
	Argv []string
	// Cwd is the working directory; empty means the sandbox's workdir.
	Cwd string
	// Env is added to the sandbox's environment for this command.
	Env   map[string]string
	Stdin []byte
}

// ExecResult is how a command ended. A command that ran and exited
// non-zero is a result, not an error.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	// Dropped is how many bytes of output were cut from the middle of
	// either stream to keep it bounded.
	Dropped int64
}

// SpecFor returns the spec of the Silofile's sandbox named name, for
// session, with its mounts and build resolved against the silo folder.
// The caller adds credential env (credentials.Delivery).
func SpecFor(f *silofile.Silofile, name, session string) (SandboxSpec, error) {
	for _, s := range f.Sandboxes {
		if s.Name != name {
			continue
		}
		spec := SandboxSpec{
			Session: session, Name: s.Name, Kind: s.Kind, Image: s.Image,
			Workdir: s.Workdir, CPUs: s.CPUs, Network: s.Network.Mode,
			Env: map[string]string{},
		}
		if spec.Network == "" {
			spec.Network = "none"
		}
		if s.Memory != nil {
			spec.Memory = int64(*s.Memory)
		}
		if s.Disk != nil {
			spec.Disk = int64(*s.Disk)
		}
		if s.Build != "" {
			real, err := safepath.Within(f.Folder, s.Build)
			if err != nil {
				return SandboxSpec{}, fmt.Errorf("sandbox %q: build: %w", name, err)
			}
			spec.Build = real
		}
		for _, m := range s.Mounts {
			// Resolved through symlinks and confined to the folder again,
			// so that what is bound is what validation checked.
			real, err := safepath.Within(f.Folder, m.From)
			if err != nil {
				return SandboxSpec{}, fmt.Errorf("sandbox %q: mount %s: %w", name, m.From, err)
			}
			spec.Mounts = append(spec.Mounts, Mount{From: real, To: m.To, RW: m.RW})
		}
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			spec.Env[k] = s.Env[k]
		}
		return spec, nil
	}
	return SandboxSpec{}, fmt.Errorf("no sandbox is named %q", name)
}
