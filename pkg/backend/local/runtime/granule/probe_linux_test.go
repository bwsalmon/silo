//go:build linux

package granule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeHostRefuses(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "kvm")
	if p := ProbeHost(HostDevices{KVM: missing, TUN: "/dev/net/tun"}); p.OK || !strings.Contains(p.Why, "nested virtualization") {
		t.Errorf("no /dev/kvm: %+v", p)
	}
	// A file that opens but isn't KVM: what an emulated or broken
	// device looks like from here.
	if p := ProbeHost(HostDevices{KVM: "/dev/null", TUN: "/dev/net/tun"}); p.OK || !strings.Contains(p.Why, "KVM_GET_API_VERSION") {
		t.Errorf("/dev/null as KVM: %+v", p)
	}
	if os.Getuid() != 0 {
		unreadable := filepath.Join(dir, "unreadable")
		if err := os.WriteFile(unreadable, nil, 0); err != nil {
			t.Fatal(err)
		}
		if p := ProbeHost(HostDevices{KVM: unreadable, TUN: "/dev/net/tun"}); p.OK || !strings.Contains(p.Why, "kvm group") {
			t.Errorf("unreadable /dev/kvm: %+v", p)
		}
	}
}

// TestProbeHostKVM runs the probe on the host's own /dev/kvm, where
// this user can open it.
func TestProbeHostKVM(t *testing.T) {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no usable /dev/kvm here: %v", err)
	}
	f.Close()
	if p := ProbeHost(DefaultHostDevices); !p.OK {
		t.Errorf("ProbeHost: %s", p.Why)
	}
	if p := ProbeHost(HostDevices{KVM: "/dev/kvm", TUN: filepath.Join(t.TempDir(), "tun")}); p.OK || !strings.Contains(p.Why, "modprobe tun") {
		t.Errorf("no /dev/net/tun: %+v", p)
	}
}
