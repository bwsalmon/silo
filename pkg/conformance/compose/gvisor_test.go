package compose_test

import (
	"testing"

	"github.com/bwsalmon/silo/pkg/conformance"
	"github.com/bwsalmon/silo/pkg/conformance/targets"
)

// gvisorGaps are §5's "What gVisor won't run": gVisor's documented ABI
// gaps (its Compatibility guide and syscall table), not fence gaps. A
// workload that needs one gets the vm class.
var gvisorGaps = []struct{ name, why string }{
	{"kernel modules", "init_module and finit_module are unimplemented"},
	{"perf_event_open", "perf_event_open is unimplemented"},
	{"ebpf", "eBPF beyond CGROUP_DEVICE programs is unimplemented, and those load and have no effect"},
	{"userfaultfd", "userfaultfd is unimplemented"},
	{"io_uring", "io_uring is off by default, and offers basic operations only"},
	{"iptables and nftables", "iptables and nftables are partial"},
	{"block-device filesystems", "ext4, FAT and other block-device filesystems can't be mounted inside the sandbox"},
	{"custom device files", "custom device files aren't supported"},
	{"kvm", "KVM inside the sandbox isn't supported"},
	{"resource limits within", "resource limits within a sandbox aren't enforced; the host cgroup around it enforces them, as silod does"},
}

// What gVisor won't run, recorded as expected skips of the sandbox
// class on every target that can make its containers, so that each
// shows in local-conformance's log and test history as a known gap
// rather than going unsaid. Docker inside a container, which §5 also
// names, is refused in both classes, and TestTestcontainersRyukRefused
// checks that.
func TestGVisorGaps(t *testing.T) {
	conformance.ForEachFence(t, targets.Pulling(), func(t *testing.T, fe conformance.Fence) {
		for _, g := range gvisorGaps {
			t.Run("sandbox/"+g.name, func(t *testing.T) {
				t.Skipf("expected skip: gVisor won't run it: %s (local-backend.md §5); such a workload uses the vm class", g.why)
			})
		}
	})
}
