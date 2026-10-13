//go:build !linux

package granule

import (
	"context"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
)

// HostDevices are the device paths ProbeHost opens.
type HostDevices struct {
	KVM, TUN string
}

// DefaultHostDevices are the host's own.
var DefaultHostDevices = HostDevices{KVM: "/dev/kvm", TUN: "/dev/net/tun"}

// ProbeHost is the vm class's host check, which needs Linux's KVM.
func ProbeHost(HostDevices) runtime.Probe {
	return runtime.Probe{Why: "the vm class needs Linux and KVM"}
}

// Doctor runs granule's own preflight, which needs Linux.
func Doctor(context.Context, string) runtime.Probe {
	return runtime.Probe{Why: "the vm class needs Linux and KVM"}
}
