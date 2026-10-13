// Package runtime is the local backend's driver interface
// (docs/silo/local-backend.md §6, "The driver interface"): a Driver is
// one isolation class, vm (granule) or sandbox (gVisor), running
// containers on silod's private containerd. Package fake is a Driver in
// memory, for tests.
//
// A Spec is backend-neutral and never carries a host path the tenant
// chose, a capability, a device, or a namespace other than the one
// silod made. The facade can't express them; Validate refuses them
// again, and every driver calls it before Create.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Class is an isolation class.
type Class string

const (
	VM      Class = "vm"
	Sandbox Class = "sandbox"
)

// The labels silod puts on every container, which List reads back for
// adoption and the reconcile.
const (
	LabelSilo = "dev.silo.silo" // the owning silo's id
	LabelName = "dev.silo.name" // the container's name in its silo
	LabelAddr = "dev.silo.addr" // its address on its first network
)

// Probe is whether the host can run a class now.
type Probe struct {
	OK bool
	// Why is what is missing, naming the fix, when !OK.
	Why string
}

// Process is a container's workload or an exec'd process.
type Process struct {
	Args    []string
	Env     []string
	User    string
	Workdir string
	TTY     bool
	Stdin   bool
}

// Resources are a container's limits, from its charges.
type Resources struct {
	VCPUm    int64 // thousandths of a vCPU
	MemoryMB int64
	DiskGB   int64
	PIDs     int64
}

// Mount kinds.
const (
	MountVolume = "volume" // a volume's directory, from the storage driver
	MountTmpfs  = "tmpfs"
)

// Mount is a volume or a tmpfs in the container.
type Mount struct {
	Kind string
	// Source is the volume's path, as the storage driver made it; empty
	// for tmpfs.
	Source   string
	Target   string
	ReadOnly bool
	SizeMB   int64 // tmpfs
}

// IDRange is a container's ids, 0 to Size-1, as ids Outside to
// Outside+Size-1 in the holder's namespace (nsd.Range).
type IDRange struct {
	Outside, Size uint32
}

// Spec is everything a driver needs to create a container.
type Spec struct {
	ID   string
	Silo string
	// Image is the image's digest, sha256:<hex>.
	Image     string
	Process   Process
	Resources Resources
	Mounts    []Mount
	// NetNS is the container's network namespace, which silod made and
	// wired before Create; UserNS its user namespace, where the class
	// needs one.
	NetNS, UserNS string
	// Root is where UserNS maps the container's ids in the holder's
	// namespace: its root is Root.Outside. The sandbox class remaps the
	// container's snapshot to it and runs runsc as it.
	Root       IDRange
	Labels     map[string]string
	StopSignal syscall.Signal
}

// Validate refuses a Spec that names anything silod didn't make.
// volumes is the storage driver's root: a volume mount must be inside
// it.
func (s *Spec) Validate(volumes string) error {
	switch {
	case s.ID == "" || strings.ContainsAny(s.ID, "/\x00") || s.Silo == "":
		return errors.New("runtime: a spec needs an id and a silo")
	case !strings.HasPrefix(s.Image, "sha256:"):
		return fmt.Errorf("runtime: image %q is not a digest", s.Image)
	case len(s.Process.Args) == 0:
		return errors.New("runtime: no process")
	case s.NetNS == "" || !filepath.IsAbs(s.NetNS):
		return errors.New("runtime: a container runs only in the network namespace silod made for it")
	case s.Labels[LabelSilo] != s.Silo:
		return fmt.Errorf("runtime: label %s is %q, not the spec's silo %q", LabelSilo, s.Labels[LabelSilo], s.Silo)
	}
	for _, m := range s.Mounts {
		if !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target {
			return fmt.Errorf("runtime: mount target %q", m.Target)
		}
		switch m.Kind {
		case MountTmpfs:
			if m.Source != "" {
				return fmt.Errorf("runtime: tmpfs at %s has a source", m.Target)
			}
		case MountVolume:
			rel, err := filepath.Rel(volumes, m.Source)
			if volumes == "" || err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.Clean(m.Source) != m.Source {
				return fmt.Errorf("runtime: volume source %q is not a volume", m.Source)
			}
		default:
			return fmt.Errorf("runtime: mount kind %q", m.Kind)
		}
	}
	return nil
}

// State is a container's state.
type State string

const (
	Created State = "created"
	Running State = "running"
	Paused  State = "paused"
	Stopped State = "stopped"
)

// Container is what List reports of a container.
type Container struct {
	ID     string
	Class  Class
	State  State
	Labels map[string]string
	// Exit is set once the container has stopped.
	Exit *ExitStatus
}

// ExitStatus is how a workload ended, as its process reported it.
type ExitStatus struct {
	Code     int
	Signal   syscall.Signal // the signal that killed it, or 0
	Finished time.Time
}

// Stats is a container's use, from its cgroup.
type Stats struct {
	CPUNanos    uint64
	MemoryBytes uint64
	ReadBytes   uint64
	WriteBytes  uint64
	Processes   uint64
	SampledAt   time.Time
}

// Streams are a process's stdio: Stdin is nil unless the process
// asked for it.
type Streams struct {
	Stdin          io.WriteCloser
	Stdout, Stderr io.ReadCloser
}

// ExecIO is an exec'd process's stdio, and its TTY's size changes.
type ExecIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Resize         <-chan [2]uint16 // columns, rows
}

// ErrNotFound is a container the driver doesn't have.
var ErrNotFound = errors.New("runtime: no such container")

// ErrState is an operation the container's state doesn't allow: start
// of a running one, pause of a stopped one.
var ErrState = errors.New("runtime: the container's state doesn't allow it")

// Driver is one isolation class.
type Driver interface {
	Class() Class
	Probe(ctx context.Context) Probe
	// Prepare makes image runnable in this class: nothing for sandbox,
	// the wrap for vm. It is cached and idempotent.
	Prepare(ctx context.Context, image string) error
	// Create makes a container from spec. It does not start it.
	Create(ctx context.Context, spec Spec) error
	Start(ctx context.Context, id string) error
	// Stop sends the spec's stop signal, then kills after timeout.
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Kill(ctx context.Context, id string, sig syscall.Signal) error
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	// Delete removes a stopped or created container.
	Delete(ctx context.Context, id string) error
	// Wait returns once the container has stopped.
	Wait(ctx context.Context, id string) (ExitStatus, error)
	// Exec runs p in the container and returns its exit code.
	Exec(ctx context.Context, id string, p Process, stdio ExecIO) (int, error)
	// Streams are the workload's stdio, apart from any console.
	Streams(ctx context.Context, id string) (Streams, error)
	Stats(ctx context.Context, id string) (Stats, error)
	// CopyIn extracts a tar archive at dir in the container's root;
	// CopyOut is path in its root as a tar archive.
	CopyIn(ctx context.Context, id, dir string, tar io.Reader) error
	CopyOut(ctx context.Context, id, path string) (io.ReadCloser, error)
	// List is every container this driver has, with silod's labels.
	List(ctx context.Context) ([]Container, error)
}

// Committer is a driver that can give a container's writable layer, as
// docker commit needs it (local-registry.md §7, "commit"). The sandbox
// class is one; the vm class, whose writable layer is a guest disk, is
// not.
type Committer interface {
	// RootfsUpper is the container's writable layer as a tar: the
	// files it created and changed, and its deletions as whiteouts,
	// overlayfs's (a 0/0 character device, and the trusted.overlay.opaque
	// xattr on a directory made opaque) or OCI's (.wh.<name>). The
	// caller checks and converts them; the tar is never unpacked on the
	// host.
	RootfsUpper(ctx context.Context, id string) (io.ReadCloser, error)
}
