// Package rootless runs the local backend's runtimes without root
// (docs/silo/local-rootless-storage.md, calls L27, L30 and L31, and
// "cgroups"; docs/silo/local-backend.md §15, step 16):
//
//   - containerd and buildkitd run inside silo-nsd's user, mount and
//     network namespaces, started through nsexec's join stage, with
//     their state under the data directory's cache/ and the runtime
//     directory;
//   - the sandbox class runs runsc started inside each container's own
//     user namespace (gVisor's "caller-configured" mode), with netstack
//     on the netns silo-nsd made: `silod runsc` is the binary the runsc
//     shim calls, and it enters the namespace the bundle names;
//   - the vm class runs under crun, whose keep_original_groups
//     annotation keeps silod's kvm group into the container, with
//     /dev/kvm and /dev/net/tun bind-mounted;
//   - cgroups are slices in the user's systemd manager, and Cgroups
//     says which controllers it was delegated, for describe.
//
// The drivers (steps 7 and 10) build their containers' specs with
// SandboxPatch and VMPatch; this package doesn't talk to containerd's
// API itself.
package rootless

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Paths is where the runtimes keep their files. Images, snapshots and
// build caches are re-creatable, so they live under the data
// directory's cache/ (call L39: not data). Per-boot state lives in the
// holder's private /run (nsd.PrivateRun), which a container's root can
// traverse, unlike $XDG_RUNTIME_DIR (mode 0700): runsc runs as that
// root and reads its bundle there. The sockets silod dials are in the
// runtime directory, where the host's namespaces see them.
type Paths struct {
	// DataDir is the data directory (--data-dir).
	DataDir string
	// RuntimeDir is $XDG_RUNTIME_DIR/silo, where nsd.sock is.
	RuntimeDir string
}

// ContainerdRoot is containerd's root: its content store, snapshots
// and metadata.
func (p Paths) ContainerdRoot() string { return filepath.Join(p.DataDir, "cache", "containerd") }

// ContainerdState is containerd's per-boot state, bundles included, in
// the holder's mount namespace. The shims' sockets are under
// /run/containerd/s whatever the configuration says.
func (p Paths) ContainerdState() string { return "/run/containerd" }

// TaskBundle is the OCI bundle containerd made for container id's task
// in namespace, in the holder's mount namespace: runtime v2's, under
// ContainerdState.
func (p Paths) TaskBundle(namespace, id string) string {
	return filepath.Join(p.ContainerdState(), "io.containerd.runtime.v2.task", namespace, id)
}

// ContainerdSocket is containerd's API socket. silod reaches it from
// the host's namespaces as an ordinary file: the socket belongs to
// silod's own uid, which is root in the holder.
func (p Paths) ContainerdSocket() string { return filepath.Join(p.RuntimeDir, "containerd.sock") }

// ContainerdConfig is the file ContainerdConfigTOML is written to.
func (p Paths) ContainerdConfig() string { return filepath.Join(p.RuntimeDir, "containerd.toml") }

// BuildkitRoot is buildkitd's root, its build cache.
func (p Paths) BuildkitRoot() string { return filepath.Join(p.DataDir, "cache", "buildkit") }

// BuildkitSocket is buildkitd's API socket.
func (p Paths) BuildkitSocket() string { return filepath.Join(p.RuntimeDir, "buildkitd.sock") }

// RunscRoot is runsc's --root. Each container's runsc runs as that
// container's own root, a different subuid per silo range, so the
// directory is shared with the sticky bit, as /tmp is, in the holder's
// private /run.
func (p Paths) RunscRoot() string { return "/run/silo/runsc" }

// RunscOptions is the runsc shim's options file (ConfigPath in its
// runtime options).
func (p Paths) RunscOptions() string { return filepath.Join(p.RuntimeDir, "runsc.toml") }

// ContainerdNamespace is the containerd namespace silod's containers
// live in; BuildNamespace is buildkitd's, so that build snapshots are
// never mistaken for a silo's containers on adoption.
const (
	ContainerdNamespace = "silo"
	BuildNamespace      = "silo-build"
)

// The runtimes, as containerd names them. The vm class uses runc's
// shim with crun as its binary.
const (
	RuntimeRunsc = "io.containerd.runsc.v1"
	RuntimeCrun  = "io.containerd.runc.v2"
)

// disabledPlugins are containerd plugins silod never uses. CRI would
// otherwise start a second API with its own CNI and its own idea of
// namespaces; the others need root or a filesystem silod doesn't
// offer.
var disabledPlugins = []string{
	"io.containerd.grpc.v1.cri",
	"io.containerd.cri.v1.images",
	"io.containerd.cri.v1.runtime",
	"io.containerd.podsandbox.controller.v1.podsandbox",
	"io.containerd.nri.v1.nri",
	"io.containerd.snapshotter.v1.aufs",
	"io.containerd.snapshotter.v1.blockfile",
	"io.containerd.snapshotter.v1.btrfs",
	"io.containerd.snapshotter.v1.devmapper",
	"io.containerd.snapshotter.v1.erofs",
	"io.containerd.snapshotter.v1.zfs",
	"io.containerd.tracing.processor.v1.otlp",
	"io.containerd.internal.v1.tracing",
}

// quote is a TOML basic string.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

// ContainerdConfigTOML is silo-containerd's configuration (version 3,
// containerd 2.x): its root and state, its socket, no CRI, no TCP or
// debug listeners, and overlayfs. containerd runs inside the holder as
// its root, so nothing here needs a rootless setting: the user
// namespace is the holder's.
//
// Images are unpacked once, owned by the holder's root. A container's
// snapshot is remapped to its silo's range by the driver
// (containerd's WithRemappedSnapshot, a chown of the copy): idmapped
// mounts would avoid the copy, but making one needs CAP_SYS_ADMIN in
// the initial user namespace.
func ContainerdConfigTOML(p Paths) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by silod; edits are overwritten at its next start.\n")
	fmt.Fprintf(&b, "version = 3\n")
	fmt.Fprintf(&b, "root = %s\n", quote(p.ContainerdRoot()))
	fmt.Fprintf(&b, "state = %s\n", quote(p.ContainerdState()))
	var ds []string
	for _, d := range disabledPlugins {
		ds = append(ds, quote(d))
	}
	fmt.Fprintf(&b, "disabled_plugins = [%s]\n\n", strings.Join(ds, ", "))
	fmt.Fprintf(&b, "[grpc]\n  address = %s\n  tcp_address = \"\"\n\n", quote(p.ContainerdSocket()))
	fmt.Fprintf(&b, "[ttrpc]\n  address = %s\n\n", quote(p.ContainerdSocket()+".ttrpc"))
	fmt.Fprintf(&b, "[debug]\n  address = \"\"\n\n[metrics]\n  address = \"\"\n")
	return b.String()
}

// ContainerdArgs is containerd's command line.
func ContainerdArgs(p Paths, containerd string) []string {
	return []string{containerd, "--config", p.ContainerdConfig()}
}

// ContainerdEnv is env, silo-containerd's environment, with
// XDG_RUNTIME_DIR set to ContainerdState, which its shims inherit. The
// runsc shim writes an exec's process file there, and runsc, running as
// the container's root, must reach it: the holder's /run/containerd is
// mode 0711, but the user's own $XDG_RUNTIME_DIR, which a rootless
// install's units run with, is 0700.
func ContainerdEnv(p Paths, env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, "XDG_RUNTIME_DIR=") {
			out = append(out, e)
		}
	}
	return append(out, "XDG_RUNTIME_DIR="+p.ContainerdState())
}

// BuildkitdArgs is buildkitd's command line: the containerd worker on
// silo-containerd, in its own namespace, and no OCI worker, so that
// every build container is one containerd knows about. Builds run in
// the holder's network namespace, silo-net, which has no route out: a
// wrap needs no network (local-backend.md §6, "Wrapping needs a
// builder").
func BuildkitdArgs(p Paths, buildkitd string) []string {
	return []string{buildkitd,
		"--addr", "unix://" + p.BuildkitSocket(),
		"--root", p.BuildkitRoot(),
		"--oci-worker=false",
		"--containerd-worker=true",
		"--containerd-worker-addr", p.ContainerdSocket(),
		"--containerd-worker-namespace", BuildNamespace,
		"--containerd-worker-snapshotter", "overlayfs",
		"--containerd-worker-net", "host",
	}
}
