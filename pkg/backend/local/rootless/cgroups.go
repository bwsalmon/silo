package rootless

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// RootSlice is the top of silod's cgroup tree, in its user's systemd
// manager: user@<uid>.service/silo.slice.
const RootSlice = "silo.slice"

// Slice is the slice of the silo at the end of path, the silo ids from
// the root down. systemd nests slices by name, so the tree is in the
// name: silo-<root>.slice, silo-<root>-<child>.slice, and so on (the
// "silo-<child>.slice" of local-backend.md §11 under the name systemd
// requires). Ids are lower-case base 32, which needs no escaping.
func Slice(path ...string) string {
	if len(path) == 0 {
		return RootSlice
	}
	return "silo-" + strings.Join(path, "-") + ".slice"
}

// CgroupsPath is a container's linux.cgroupsPath for the systemd
// driver: a scope silo-<container>.scope in its silo's slice.
func CgroupsPath(container string, siloPath ...string) string {
	return Slice(siloPath...) + ":silo:" + container
}

// Wanted are the controllers silod's slices use: cpu.max, memory.max
// and pids.max, io.max and io.stat, and cpuset for pinning.
var Wanted = []string{"cpu", "cpuset", "io", "memory", "pids"}

// Required are the controllers without which a silo's grant isn't
// enforced at all. systemd delegates them to user managers by default.
var Required = []string{"cpu", "memory", "pids"}

// DelegateFix is the one-time root step that delegates the rest.
const DelegateFix = "sudo mkdir -p /etc/systemd/system/user@.service.d && printf '[Service]\\nDelegate=cpu cpuset io memory pids\\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf && sudo systemctl daemon-reload, then log in again (or restart user@<uid>.service)"

// Cgroups is what describe on the root says about cgroup enforcement
// on this host (local-rootless-storage.md, "cgroups").
type Cgroups struct {
	// Version is 2 on a unified hierarchy, 1 on a legacy or hybrid
	// one, and 0 when /sys/fs/cgroup can't be read.
	Version int `json:"version"`
	// Manager is the user's systemd manager unit, user@<uid>.service,
	// or empty when it isn't running.
	Manager string `json:"manager,omitempty"`
	// Systemd is the manager's version, when known.
	Systemd int `json:"systemd,omitempty"`
	// Delegated are the controllers the manager can hand to silo.slice.
	Delegated []string `json:"delegated"`
	// Missing are the Wanted controllers that are not delegated.
	Missing []string `json:"missing,omitempty"`
	// Enforced is "all" when every Wanted controller is delegated,
	// "partial" when CPU, memory and pids limits hold but some of the
	// others don't, and "none" when silod can't create slices with
	// limits at all.
	Enforced string `json:"enforced"`
	// Detail says what holds and what doesn't, in a sentence.
	Detail string `json:"detail"`
	// Fix is the root step that would complete delegation.
	Fix string `json:"fix,omitempty"`
}

// CgroupHost is what ProbeCgroups reads. Tests replace it.
type CgroupHost struct {
	ReadFile func(name string) ([]byte, error)
	UID      int
	// SystemdVersion is the user manager's version, or 0 if unknown.
	SystemdVersion func() int
}

// MinSystemd is the oldest systemd whose cgroup driver gVisor supports
// (gVisor's systemd cgroup guide).
const MinSystemd = 244

// ProbeCgroups reports which controllers the user's systemd manager was
// delegated.
func ProbeCgroups(h CgroupHost) Cgroups {
	var c Cgroups
	if _, err := h.ReadFile("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		if _, err1 := h.ReadFile("/proc/self/cgroup"); err1 != nil {
			c.Enforced = "none"
			c.Detail = "/sys/fs/cgroup can't be read: " + err.Error()
			return c
		}
		c.Version = 1
		c.Enforced = "none"
		c.Detail = "the host uses cgroup v1, which can't be delegated to a user: CPU, memory and pids limits are not enforced by the kernel, only by the ledger"
		c.Fix = "boot with systemd.unified_cgroup_hierarchy=1 (cgroup v2)"
		c.Missing = slices.Clone(Wanted)
		return c
	}
	c.Version = 2
	unit := fmt.Sprintf("user@%d.service", h.UID)
	dir := filepath.Join("/sys/fs/cgroup/user.slice", fmt.Sprintf("user-%d.slice", h.UID), unit)
	b, err := h.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		c.Enforced = "none"
		c.Missing = slices.Clone(Wanted)
		c.Detail = "the user's systemd manager (" + unit + ") is not running, so silod has no slices to put limits on: CPU, memory and pids limits are not enforced by the kernel, only by the ledger"
		c.Fix = "sudo loginctl enable-linger <user>, so that the user's manager runs without a login session"
		return c
	}
	c.Manager = unit
	if h.SystemdVersion != nil {
		c.Systemd = h.SystemdVersion()
	}
	c.Delegated = strings.Fields(string(b))
	slices.Sort(c.Delegated)
	for _, w := range Wanted {
		if !slices.Contains(c.Delegated, w) {
			c.Missing = append(c.Missing, w)
		}
	}
	var missingRequired []string
	for _, r := range Required {
		if slices.Contains(c.Missing, r) {
			missingRequired = append(missingRequired, r)
		}
	}
	switch {
	case len(missingRequired) > 0:
		c.Enforced = "none"
		c.Detail = fmt.Sprintf("%s %s not delegated to %s: a silo's grant is enforced only by the ledger", strings.Join(missingRequired, ", "), isAre(missingRequired), unit)
		c.Fix = DelegateFix
	case len(c.Missing) > 0:
		c.Enforced = "partial"
		c.Detail = "CPU, memory and pids limits hold; " + strings.Join(c.Missing, ", ") + " " + isAre(c.Missing) + " not delegated to " + unit
		if slices.Contains(c.Missing, "io") {
			c.Detail += ", so I/O is neither limited nor metered"
		}
		c.Fix = DelegateFix
	default:
		c.Enforced = "all"
		c.Detail = "CPU, memory, pids, I/O and cpuset limits hold"
	}
	if c.Systemd != 0 && c.Systemd < MinSystemd {
		c.Detail += fmt.Sprintf("; systemd %d is older than %d, which gVisor's systemd cgroup driver needs", c.Systemd, MinSystemd)
	}
	return c
}

func isAre(s []string) string {
	if len(s) == 1 {
		return "is"
	}
	return "are"
}

// parseSystemdVersion reads `systemctl --version`'s first line,
// "systemd 252 (252.39-1~deb12u2)".
func parseSystemdVersion(out string) int {
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "systemd" {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}
