package nsd

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Check is one prerequisite of the holder, with the root step that
// fixes it (docs/silo/local-rootless-storage.md, "The one-time root
// steps").
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Host is what the probes read. Tests replace it; Probe uses the real
// one.
type Host struct {
	ReadFile func(name string) ([]byte, error)
	LookPath func(file string) (string, error)
	Stat     func(name string) (fs.FileInfo, error)
	User     string
	UID, GID int
	// TryUserNS starts a process in a new user namespace and reports
	// why it could not.
	TryUserNS func() error
}

// Probe runs every check against this host for the current user.
func Probe() []Check {
	return ProbeHost(Host{
		ReadFile:  os.ReadFile,
		LookPath:  exec.LookPath,
		Stat:      os.Stat,
		User:      currentUser(),
		UID:       os.Getuid(),
		GID:       os.Getgid(),
		TryUserNS: tryUserNS,
	})
}

// Failed returns the checks that did not pass.
func Failed(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// ProbeHost runs every check against h.
func ProbeHost(h Host) []Check {
	var out []Check
	sysctl := func(name string) (string, bool) {
		b, err := h.ReadFile(filepath.Join("/proc/sys", strings.ReplaceAll(name, ".", "/")))
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(b)), true
	}

	c := Check{Name: "unprivileged user namespaces", OK: true, Detail: "allowed"}
	if v, ok := sysctl("kernel.unprivileged_userns_clone"); ok && v == "0" {
		c = Check{Name: c.Name, Detail: "kernel.unprivileged_userns_clone is 0",
			Fix: "sudo sysctl -w kernel.unprivileged_userns_clone=1, and set it in /etc/sysctl.d"}
	} else if v, ok := sysctl("user.max_user_namespaces"); ok && v == "0" {
		c = Check{Name: c.Name, Detail: "user.max_user_namespaces is 0",
			Fix: "sudo sysctl -w user.max_user_namespaces=15000, and set it in /etc/sysctl.d"}
	} else if h.TryUserNS != nil {
		if err := h.TryUserNS(); err != nil {
			c = Check{Name: c.Name, Detail: "creating one failed: " + err.Error(),
				Fix: "allow unprivileged user namespaces for this user"}
		}
	}
	out = append(out, c)

	c = Check{Name: "AppArmor user namespace restriction", OK: true, Detail: "not restricted"}
	if v, ok := sysctl("kernel.apparmor_restrict_unprivileged_userns"); ok && v == "1" {
		c.Detail = "kernel.apparmor_restrict_unprivileged_userns is 1"
		if h.TryUserNS != nil && h.TryUserNS() == nil {
			c.Detail += ", but this binary is allowed by its profile"
		} else {
			c.OK = false
			c.Fix = "install an AppArmor profile granting `userns` to silod and silo-nsd (as rootlesskit's packages do), or sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0"
		}
	}
	out = append(out, c)

	for _, tool := range []string{"newuidmap", "newgidmap"} {
		c := Check{Name: tool}
		p, err := h.LookPath(tool)
		switch {
		case err != nil:
			c.Detail = "not found on PATH"
			c.Fix = "sudo apt-get install uidmap (Debian, Ubuntu) or shadow-utils (Fedora)"
		default:
			fi, err := h.Stat(p)
			if err != nil {
				c.Detail = err.Error()
			} else if fi.Mode()&fs.ModeSetuid == 0 && !hasFileCaps(h, p) {
				c.Detail = p + " is neither setuid nor carries file capabilities"
				c.Fix = "reinstall the uidmap package"
			} else {
				c.OK, c.Detail = true, p
			}
		}
		out = append(out, c)
	}

	for _, f := range []struct{ file, flag string }{{"/etc/subuid", "--add-subuids"}, {"/etc/subgid", "--add-subgids"}} {
		c := Check{Name: "range in " + f.file}
		fix := fmt.Sprintf("sudo usermod %s 100000-165535 %s", f.flag, h.User)
		b, err := h.ReadFile(f.file)
		if err != nil {
			c.Detail, c.Fix = err.Error(), fix
			out = append(out, c)
			continue
		}
		sub, ok, err := ParseSubID(strings.NewReader(string(b)), h.User, h.UID)
		own := uint32(h.UID)
		if f.file == "/etc/subgid" {
			own = uint32(h.GID)
		}
		switch {
		case err != nil:
			c.Detail = err.Error()
		case !ok:
			c.Detail, c.Fix = "no range for "+h.User, fix
		case sub.Count < 65536:
			c.Detail, c.Fix = fmt.Sprintf("%d ids from %d; containers need 65536", sub.Count, sub.Start), fix
		case own >= sub.Start && own-sub.Start < sub.Count:
			// Container root would be silod's own account (call L29).
			c.Detail = fmt.Sprintf("the range %d+%d contains the user's own id %d", sub.Start, sub.Count, own)
			c.Fix = "give the user a range that does not contain its own id"
		default:
			c.OK = true
			c.Detail = fmt.Sprintf("%d ids from %d (%d container ranges)", sub.Count, sub.Start, sub.Count/65536)
		}
		out = append(out, c)
	}
	return out
}

// hasFileCaps reports whether p carries a security.capability xattr,
// as Fedora's newuidmap does instead of setuid.
func hasFileCaps(h Host, p string) bool {
	return getxattr(p, "security.capability")
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if out, err := exec.Command("id", "-un").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}
