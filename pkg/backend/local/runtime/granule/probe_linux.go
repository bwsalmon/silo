//go:build linux

package granule

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// KVM's ioctls on /dev/kvm (linux/kvm.h): _IO(KVMIO, 0x00) and
// _IO(KVMIO, 0x01).
const (
	kvmGetAPIVersion = 0xAE00
	kvmCreateVM      = 0xAE01
	// kvmAPIVersion is the only version KVM has ever reported.
	kvmAPIVersion = 12
)

// HostDevices are the device paths ProbeHost opens; tests point them
// elsewhere.
type HostDevices struct {
	KVM, TUN string
}

// DefaultHostDevices are the host's own.
var DefaultHostDevices = HostDevices{KVM: "/dev/kvm", TUN: "/dev/net/tun"}

// ProbeHost is the vm class's host check (local-backend.md §3,
// "Detection"), as silod's own user: /dev/kvm opens read-write, speaks
// KVM's API and creates a VM, and /dev/net/tun opens. Creating a VM is
// the part a bare open misses: under nested virtualization a /dev/kvm
// that opens can still refuse to run one.
//
// Nothing here falls back to emulation. A host without KVM can't run
// the vm class at all: TCG is QEMU's "non-virtualization use case",
// whose bugs QEMU doesn't treat as security bugs, so it is not a fence
// (§3, "TCG is not a fence", call L3). cloud-hypervisor has no TCG.
func ProbeHost(d HostDevices) runtime.Probe {
	kvm, err := os.OpenFile(d.KVM, os.O_RDWR, 0)
	if err != nil {
		why := fmt.Sprintf("no usable %s: %v", d.KVM, err)
		switch {
		case errors.Is(err, os.ErrNotExist):
			why += "; run on a host with KVM (bare metal, or a VM with nested virtualization)"
		case errors.Is(err, os.ErrPermission):
			why += "; silod's user needs the kvm group (sudo usermod -aG kvm <user>, then log in again)"
		}
		return runtime.Probe{Why: why}
	}
	defer kvm.Close()
	v, _, errno := unix.Syscall(unix.SYS_IOCTL, kvm.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		return runtime.Probe{Why: fmt.Sprintf("no usable %s: it doesn't answer KVM_GET_API_VERSION (%v)", d.KVM, errno)}
	}
	if v != kvmAPIVersion {
		return runtime.Probe{Why: fmt.Sprintf("no usable %s: KVM API version %d, not %d", d.KVM, v, kvmAPIVersion)}
	}
	vm, _, errno := unix.Syscall(unix.SYS_IOCTL, kvm.Fd(), kvmCreateVM, 0)
	if errno != 0 {
		return runtime.Probe{Why: fmt.Sprintf("no usable %s: it opens but can't create a VM (KVM_CREATE_VM: %v); check nested virtualization", d.KVM, errno)}
	}
	unix.Close(int(vm))
	tun, err := os.OpenFile(d.TUN, os.O_RDWR, 0)
	if err != nil {
		return runtime.Probe{Why: fmt.Sprintf("no usable %s: %v; load the tun module (modprobe tun)", d.TUN, err)}
	}
	tun.Close()
	return runtime.Probe{OK: true}
}

// Doctor runs granule's own preflight, `granule doctor -only kvm,tun`
// (granule README, "Checking a new host"), with granule the binary's
// path, and reports what it says when it fails.
func Doctor(ctx context.Context, granule string) runtime.Probe {
	cmd := exec.CommandContext(ctx, granule, "doctor", "-only", "kvm,tun")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return runtime.Probe{Why: fmt.Sprintf("granule doctor: %v: %s", err, strings.TrimSpace(out.String()))}
	}
	return runtime.Probe{OK: true}
}
