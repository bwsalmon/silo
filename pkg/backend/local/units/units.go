// Package units writes the local backend's systemd units
// (docs/silo/local-rootless-storage.md, calls L33 and L34), in either
// of its two install modes:
//
//   - rootless, the default: user units in silod's user's own manager,
//     which lingering starts at boot and keeps after logout;
//   - system: system units with User=silo, for the system install, which
//     runs the same rootless design under a dedicated account.
//
// The units are silo-nsd.service (the namespace holder),
// silo-containerd.service and silo-buildkitd.service (the runtimes,
// inside the holder's namespaces) and silod.service. The runtimes Want
// the holder rather than BindsTo it: a restart of silo-nsd must leave
// containerd and its shims running, so that the next holder has
// something to adopt the namespaces through.
package units

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode is an install mode.
type Mode string

const (
	Rootless Mode = "rootless"
	System   Mode = "system"
)

// SystemUser is the system install's account.
const SystemUser = "silo"

// The units' names. Silod is last in Names, the order they are written,
// enabled and started in.
const (
	NSD        = "silo-nsd.service"
	Containerd = "silo-containerd.service"
	Buildkitd  = "silo-buildkitd.service"
	Silod      = "silod.service"
)

// Gateway is gw0's addresses in silo-net: silo.internal's and
// docker.silo.internal's, in the root's gateway range.
const Gateway = "10.255.255.1/24,10.255.255.2/24"

// Config is what the units are written from.
type Config struct {
	Mode Mode
	// Silod is the silod binary's absolute path.
	Silod string
	// DataDir is the data directory (call L35).
	DataDir string
	// User is the system install's account; SystemUser if empty. The
	// rootless units run as whoever owns the manager.
	User string
	// Buildkitd writes silo-buildkitd.service, which the vm class's
	// image wraps need. Without buildkitd on the host it would only
	// fail.
	Buildkitd bool
	// RegistryHostPort is the registry's host port for registry clients
	// on 127.0.0.1 (local-registry.md §5), which silod serves with
	// --registry-host-listen; 0 serves none.
	RegistryHostPort int
}

// Unit is one unit file.
type Unit struct {
	Name    string
	Content string
}

// RuntimeDir is where the holder's socket, its state, and the runtimes'
// sockets are, as a unit's ExecStart names it. Rootless, that is
// $XDG_RUNTIME_DIR/silo (%t/silo). The system install can't use
// /run/silo for it: the holder replaces silo there with a directory of
// its own private /run (nsd.PrivateRun), which the host's namespaces,
// silod's included, don't see. It uses /run/silo-nsd instead, and
// /run/silo is the front door's.
func (c Config) RuntimeDir() string {
	if c.Mode == System {
		return "/run/silo-nsd"
	}
	return "%t/silo"
}

// Socket is silod's front door: $XDG_RUNTIME_DIR/silo/silo.sock,
// reachable by silod's user only, or /run/silo/silo.sock, reachable by
// every local user (local-rootless-storage.md, "The one-time root
// steps", and open question 14).
func (c Config) Socket() string {
	if c.Mode == System {
		return "/run/silo/silo.sock"
	}
	return "%t/silo/silo.sock"
}

// Dir is the directory the units are written to: the user's
// $XDG_CONFIG_HOME/systemd/user, or /etc/systemd/system.
func (c Config) Dir() (string, error) {
	if c.Mode == System {
		return "/etc/systemd/system", nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// Validate checks c.
func (c Config) Validate() error {
	var errs []error
	if c.Mode != Rootless && c.Mode != System {
		errs = append(errs, fmt.Errorf("units: mode %q: want %s or %s", c.Mode, Rootless, System))
	}
	if !filepath.IsAbs(c.Silod) {
		errs = append(errs, fmt.Errorf("units: silod %q is not an absolute path", c.Silod))
	}
	if !filepath.IsAbs(c.DataDir) {
		errs = append(errs, fmt.Errorf("units: data directory %q is not an absolute path", c.DataDir))
	}
	if c.RegistryHostPort < 0 || c.RegistryHostPort > 65535 {
		errs = append(errs, fmt.Errorf("units: registry host port %d: want 1 to 65535, or 0 for none", c.RegistryHostPort))
	}
	for _, s := range []string{c.Silod, c.DataDir, c.User} {
		if strings.ContainsAny(s, " \t\n\"'\\%$") {
			errs = append(errs, fmt.Errorf("units: %q has characters a unit file would need quoted", s))
		}
	}
	return errors.Join(errs...)
}

// Names are the units c writes, in order.
func (c Config) Names() []string {
	names := []string{NSD, Containerd}
	if c.Buildkitd {
		names = append(names, Buildkitd)
	}
	return append(names, Silod)
}

// Units are c's unit files, in Names' order.
func Units(c Config) ([]Unit, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rt := c.RuntimeDir()
	user := c.User
	if user == "" {
		user = SystemUser
	}
	// system is the system install's lines for every unit: the account,
	// and the data directory as the state directory, which systemd
	// makes and keeps owned by it.
	system := func(extra ...string) []string {
		if c.Mode != System {
			return nil
		}
		lines := []string{"User=" + user, "Group=" + user}
		if c.DataDir == "/var/lib/silo" {
			lines = append(lines, "StateDirectory=silo", "StateDirectoryMode=0700")
		}
		return append(lines, extra...)
	}
	target := "default.target"
	if c.Mode == System {
		target = "multi-user.target"
	}
	data := c.DataDir
	var out []Unit
	add := func(name, desc string, unit []string, svc []string) {
		var b strings.Builder
		fmt.Fprintf(&b, "# Written by silo install (%s mode); it is rewritten by the next install.\n", c.Mode)
		b.WriteString("[Unit]\nDescription=" + desc + "\n")
		b.WriteString("Documentation=https://github.com/bwsalmon/silo/blob/main/docs/silo/local-rootless-storage.md\n")
		for _, l := range unit {
			b.WriteString(l + "\n")
		}
		b.WriteString("\n[Service]\n")
		for _, l := range svc {
			b.WriteString(l + "\n")
		}
		b.WriteString("Restart=on-failure\nRestartSec=2\n\n[Install]\nWantedBy=" + target + "\n")
		out = append(out, Unit{Name: name, Content: b.String()})
	}

	nsdArgs := []string{c.Silod, "nsd", "--silos", filepath.Join(data, "silos"), "--addr", Gateway}
	if c.Mode == System {
		nsdArgs = append(nsdArgs, "--socket", rt+"/nsd.sock", "--state", rt+"/nsd.state")
	}
	add(NSD, "Silo namespace holder (silo-nsd): the user namespace, silo-net and gw0", nil,
		append(system("RuntimeDirectory=silo-nsd", "RuntimeDirectoryPreserve=yes"),
			"ExecStart="+strings.Join(nsdArgs, " ")))

	// containerd's shims are the containers' parents and must outlive
	// it (KillMode=process), as in containerd's own unit.
	runtime := func(name, cmd, desc string, after ...string) {
		deps := append([]string{NSD}, after...)
		add(name, desc,
			[]string{"Wants=" + strings.Join(deps, " "), "After=" + strings.Join(deps, " ")},
			append(system(),
				fmt.Sprintf("ExecStart=%s %s --data-dir %s --runtime-dir %s", c.Silod, cmd, data, rt),
				"Delegate=yes", "KillMode=process"))
	}
	runtime(Containerd, "containerd", "Silo's containerd, in silo-nsd's namespaces")
	if c.Buildkitd {
		runtime(Buildkitd, "buildkitd", "Silo's buildkitd, in silo-nsd's namespaces", Containerd)
	}

	deps := strings.Join(c.Names()[:len(c.Names())-1], " ")
	serve := fmt.Sprintf("ExecStart=%s serve --backend=local --data-dir %s --migrate --socket %s", c.Silod, data, c.Socket())
	if c.Mode == System {
		serve += " --nsd-state " + rt + "/nsd.state"
	}
	if c.RegistryHostPort != 0 {
		serve += fmt.Sprintf(" --registry-host-listen 127.0.0.1:%d", c.RegistryHostPort)
	}
	add(Silod, "Silo's control plane (silod), local backend",
		[]string{"Wants=" + deps, "After=" + deps},
		append(system("RuntimeDirectory=silo", "RuntimeDirectoryMode=0755", "Delegate=yes"), serve))
	return out, nil
}
