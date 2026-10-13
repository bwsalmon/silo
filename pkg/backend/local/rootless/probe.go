package rootless

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"syscall"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
)

// Host is what Probe reads. Tests replace it.
type Host struct {
	LookPath func(file string) (string, error)
	Stat     func(name string) (fs.FileInfo, error)
	// Groups are the process's supplementary groups.
	Groups []int
	Cgroup CgroupHost
}

// LocalHost is this host, for the current user.
func LocalHost() Host {
	groups, _ := os.Getgroups()
	return Host{
		LookPath: exec.LookPath,
		Stat:     os.Stat,
		Groups:   groups,
		Cgroup: CgroupHost{
			ReadFile: os.ReadFile,
			UID:      os.Getuid(),
			SystemdVersion: func() int {
				out, err := exec.Command("systemctl", "--user", "--version").Output()
				if err != nil {
					return 0
				}
				return parseSystemdVersion(string(out))
			},
		},
	}
}

// Report is what describe on the local root says about the runtimes:
// the checks, and cgroup delegation.
type Report struct {
	Checks  []nsd.Check `json:"checks"`
	Cgroups Cgroups     `json:"cgroups"`
}

// Probe checks the runtimes' prerequisites. The sandbox class needs
// containerd, runsc and its shim; the vm class needs crun and
// /dev/kvm through the kvm group (call L31); buildkitd is the vm
// class's wrapper. cgroup delegation is a check that fails only when
// no limit holds; a partial delegation passes, and Report.Cgroups says
// what is missing.
func Probe(h Host) Report {
	var r Report
	for _, b := range []struct{ bin, class, fix string }{
		{"containerd", "both classes", "install containerd 2.x (Debian, Ubuntu: containerd; or containerd.io)"},
		{"runsc", "the sandbox class", "install gVisor's runsc (gvisor.dev, \"Installation\")"},
		{"containerd-shim-runsc-v1", "the sandbox class", "install gVisor's containerd-shim-runsc-v1 beside runsc"},
		{"crun", "the vm class", "install crun"},
		{"buildkitd", "the vm class's image wraps", "install BuildKit's buildkitd"},
	} {
		c := nsd.Check{Name: b.bin}
		if p, err := h.LookPath(b.bin); err != nil {
			c.Detail = "not found on PATH; needed by " + b.class
			c.Fix = b.fix
		} else {
			c.OK, c.Detail = true, p
		}
		r.Checks = append(r.Checks, c)
	}
	r.Checks = append(r.Checks, kvmCheck(h))
	r.Cgroups = ProbeCgroups(h.Cgroup)
	c := nsd.Check{Name: "cgroup delegation", OK: r.Cgroups.Enforced != "none", Detail: r.Cgroups.Detail}
	if r.Cgroups.Enforced != "all" {
		c.Fix = r.Cgroups.Fix
	}
	r.Checks = append(r.Checks, c)
	return r
}

// kvmCheck is the vm class's /dev/kvm: present, and reachable through
// a group the user is in, which crun's keep_original_groups carries
// into the container.
func kvmCheck(h Host) nsd.Check {
	c := nsd.Check{Name: "/dev/kvm through the kvm group"}
	fi, err := h.Stat("/dev/kvm")
	if err != nil {
		c.Detail = "no /dev/kvm (" + err.Error() + "): the vm class is unavailable"
		c.Fix = "run on a host with KVM (bare metal, or a VM with nested virtualization)"
		return c
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		c.Detail = "can't read /dev/kvm's owner"
		return c
	}
	switch {
	case fi.Mode().Perm()&0o006 == 0o006:
		c.OK, c.Detail = true, "/dev/kvm is open to every user"
	case fi.Mode().Perm()&0o060 == 0o060 && slices.Contains(h.Groups, int(st.Gid)):
		c.OK, c.Detail = true, fmt.Sprintf("in group %d, which owns /dev/kvm", st.Gid)
	default:
		c.Detail = fmt.Sprintf("/dev/kvm is %v, group %d, and the user is not in that group", fi.Mode().Perm(), st.Gid)
		c.Fix = "sudo usermod -aG kvm <user>, then log in again (or restart the user's manager)"
	}
	return c
}
