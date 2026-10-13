package granule

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// Commit is the gke-labs/granule commit the driver is written against:
// the one testdata/record.sh recorded `granule vm create` at, and the
// one silod's pinned granule image is built from.
const Commit = "3ca10a3757c73df54e423b3bccb61047894d5499"

// Paths inside the VM container. GuestDir is granule's guestimage.Dir,
// where the VM container looks for the guest half a wrap writes; IODir
// is the container's directory on the host, bind-mounted, which holds
// the workload's FIFOs, its exit record and the virtiofsd sockets.
const (
	GuestDir = "/var/lib/granule/guest"
	IODir    = "/run/silo"
)

// Entrypoint is the granule image's ENTRYPOINT, and RunArg the mode
// `granule vm create` gives it: the VMM as PID 1 of its container.
const (
	Entrypoint = "/usr/local/bin/granule-shim"
	RunArg     = "run"
)

// Gateway DNS, cmdline and shutdown settings granule derives from its
// own flags, which the driver sets the way `granule vm create` does.
const (
	// netshim's in-container bridge; the control link it would hold is
	// off (NETSHIM_CONTROL_CIDR empty): exec and the workload's streams
	// use vsock, and nothing else needs a second NIC.
	bridge = "granule0"
	// cmdline is what granule's vmspec derives for -disk-mode overlay.
	cmdline = "console=ttyS0 root=/dev/vda rw noresume quiet loglevel=3"
	// shutdownTimeout is Docker's default stop timeout: the power button
	// gets as long as `docker stop` gives a container to exit.
	shutdownTimeout = "10s"
)

// Defaults for a spec that charges no CPU or memory. The facade charges
// the class's defaults (local-backend.md §11), so these are only a
// floor.
const (
	defaultMemoryMB = 512
	minMemoryMB     = 128
)

// Devices, capabilities and security options the VM container gets,
// as `granule vm create` asks Docker for them (granule's
// dockervm.vmSecurityArgs): /dev/kvm for the VMM, /dev/net/tun and
// NET_ADMIN, in its own netns, for netshim's tap, and nothing else.
var (
	Devices      = []string{"/dev/kvm", "/dev/net/tun"}
	Capabilities = []string{"CAP_NET_ADMIN"}
)

// Override is granule's containerspec.Override, CHV_CONTAINER_OVERRIDE:
// the process granule-container runs in the guest instead of the one
// the image was wrapped with.
type Override struct {
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
	Env        []string `json:"env,omitempty"`
	User       string   `json:"user,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
}

// Share is one volume, crossing into the guest over virtio-fs: a
// virtiofsd serving Source on Socket (both host paths), attached under
// Tag and mounted by granule-container at Target inside the wrapped
// container's root.
type Share struct {
	Tag      string
	Source   string
	Socket   string
	Target   string
	ReadOnly bool
}

// entry is the share as a CHV_VOLUMES entry, with the socket as the VM
// container sees it.
func (s Share) entry() string {
	e := "tag=" + s.Tag + ",socket=" + path.Join(IODir, "fs", s.Tag+".sock") + ",destination=" + s.Target
	if s.ReadOnly {
		e += ",readonly=on"
	}
	return e
}

// VM is everything the driver runs a vm-class container as: one
// granule VM container, described in granule's own terms.
type VM struct {
	// Name is the VM's name, the container's id.
	Name string
	// Guest is the wrap's output on the host, mounted at GuestDir; Dir
	// is the container's directory on the host, mounted at IODir.
	Guest, Dir string
	CPUs       int
	MemoryMB   int64
	DiskSizeMB int64
	// DNS is the silo's gateway.
	DNS      string
	Override Override
	TTY      bool
	Stdin    bool
	Shares   []Share
}

// NewVM describes spec's container. guest is its image's wrap, dir the
// container's directory, and gateway its network's gateway, which
// answers DNS.
func NewVM(spec runtime.Spec, guest, dir, gateway string) (VM, error) {
	if guest == "" || dir == "" || gateway == "" {
		return VM{}, errors.New("granule: a VM needs its image's wrap, a directory and a gateway")
	}
	v := VM{
		Name:     spec.ID,
		Guest:    guest,
		Dir:      dir,
		CPUs:     int((spec.Resources.VCPUm + 999) / 1000),
		MemoryMB: spec.Resources.MemoryMB,
		DNS:      gateway,
		Override: Override{
			// The facade composes Docker's entrypoint and cmd into the
			// argv, so the guest is given it whole, with no cmd of the
			// image's left to append.
			Entrypoint: append([]string{}, spec.Process.Args...),
			Cmd:        []string{},
			Env:        spec.Process.Env,
			User:       spec.Process.User,
			WorkingDir: spec.Process.Workdir,
		},
		TTY:   spec.Process.TTY,
		Stdin: spec.Process.Stdin,
	}
	if v.CPUs < 1 {
		v.CPUs = 1
	}
	if v.MemoryMB == 0 {
		v.MemoryMB = defaultMemoryMB
	}
	if v.MemoryMB < minMemoryMB {
		return VM{}, fmt.Errorf("granule: %d MiB is too little for a VM (at least %d)", v.MemoryMB, minMemoryMB)
	}
	if spec.Resources.DiskGB > 0 {
		v.DiskSizeMB = spec.Resources.DiskGB * 1024
	}
	for i, m := range spec.Mounts {
		switch m.Kind {
		case runtime.MountVolume:
			tag := "v" + strconv.Itoa(i)
			v.Shares = append(v.Shares, Share{Tag: tag, Source: m.Source, Socket: filepath.Join(dir, "fs", tag+".sock"), Target: m.Target, ReadOnly: m.ReadOnly})
		default:
			// granule's override has no tmpfs; a tmpfs in the guest is
			// a granule change.
			return VM{}, fmt.Errorf("granule: the vm class can't mount a %s at %s yet", m.Kind, m.Target)
		}
	}
	return v, nil
}

// Env is the VM container's environment: the settings `granule vm
// create` derives from its flags, and the ones the driver passes it
// with -e.
func (v VM) Env() ([]string, error) {
	o, err := json.Marshal(v.Override)
	if err != nil {
		return nil, err
	}
	env := []string{
		"CHV_DISK_MODE=overlay",
		"CHV_CMDLINE=" + cmdline,
		"CHV_NET_MODE=inline",
		"NETSHIM_VM=" + v.Name,
		"NETSHIM_BRIDGE=" + bridge,
		"NETSHIM_CONTROL_CIDR=",
		"NETSHIM_EXTERNAL_IFACE=auto",
		"NETSHIM_DNS=" + v.DNS,
		// The local fence is IPv4 only (local-backend.md §14).
		"NETSHIM_IPV6=off",
		"CHV_CPUS=" + strconv.Itoa(v.CPUs),
		"CHV_MEMORY_MB=" + strconv.FormatInt(v.MemoryMB, 10),
		"CHV_SHUTDOWN_TIMEOUT=" + shutdownTimeout,
		"CHV_CONTAINER_OVERRIDE=" + string(o),
		"GRANULE_WORKLOAD_STDOUT=" + path.Join(IODir, "stdout"),
		"GRANULE_WORKLOAD_EXIT_FILE=" + path.Join(IODir, "exit.json"),
	}
	if v.DiskSizeMB > 0 {
		env = append(env, "CHV_DISK_SIZE_MB="+strconv.FormatInt(v.DiskSizeMB, 10))
	}
	if v.TTY {
		// A terminal holds all three streams.
		env = append(env, "GRANULE_WORKLOAD_TTY=true", "GRANULE_WORKLOAD_RESIZE="+path.Join(IODir, "resize"))
	} else {
		env = append(env, "GRANULE_WORKLOAD_STDERR="+path.Join(IODir, "stderr"))
	}
	if v.Stdin {
		env = append(env, "GRANULE_WORKLOAD_STDIN="+path.Join(IODir, "stdin"))
	}
	if len(v.Shares) > 0 {
		var es []string
		for _, s := range v.Shares {
			es = append(es, s.entry())
		}
		env = append(env, "CHV_VOLUMES="+strings.Join(es, ";"))
	}
	return env, nil
}

// Bind is a host directory in the VM container.
type Bind struct {
	Source, Target string
	ReadOnly       bool
}

// Binds are the VM container's mounts: the guest half, read-only, and
// the container's directory. The devices are separate (Devices).
func (v VM) Binds() []Bind {
	return []Bind{
		{Source: v.Guest, Target: GuestDir, ReadOnly: true},
		{Source: v.Dir, Target: IODir},
	}
}

// Args is the VM container's process: the granule image's entrypoint
// in run mode.
func (v VM) Args() []string { return []string{Entrypoint, RunArg} }
