package rootless

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// RunscFlags are runsc's flags for every sandbox-class container
// (local-backend.md §6, "gVisor driver"): systrap, netstack on the
// container's netns, the root overlay backed by the sandbox's own
// memory, and directfs. The per-container overlay size cap is the
// driver's.
var RunscFlags = map[string]string{
	"platform": "systrap",
	"network":  "sandbox",
	"overlay2": "root:self",
	"directfs": "true",
}

// RunscOptionsTOML is the runsc shim's options file: runsc is reached
// through wrapper, a script that execs `silod runsc`, so that every
// runsc the shim runs starts inside its container's user namespace.
// The shim's own cgroup handling stays on (the systemd driver, through
// the spec's cgroupsPath).
func RunscOptionsTOML(p Paths, wrapper string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by silod; edits are overwritten at its next start.\n")
	fmt.Fprintf(&b, "binary_name = %s\n", quote(wrapper))
	fmt.Fprintf(&b, "root = %s\n\n[runsc_config]\n", quote(p.RunscRoot()))
	for _, k := range []string{"directfs", "network", "overlay2", "platform"} {
		fmt.Fprintf(&b, "  %s = %s\n", k, quote(RunscFlags[k]))
	}
	fmt.Fprintf(&b, "  systemd-cgroup = \"true\"\n")
	return b.String()
}

// RunscWrapperScript is the script the shim's binary_name names. The
// shim takes a path, not a command line.
func RunscWrapperScript(silod, runsc string) string {
	return fmt.Sprintf("#!/bin/sh\nexec %s runsc --runsc %s -- \"$@\"\n", shellQuote(silod), shellQuote(runsc))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ErrNoUserNS is a runsc call whose bundle names no user namespace.
// runsc is never run as the holder's own root, which is silod's uid on
// the host: a gofer escape would land in the account that controls
// the fence (call L29).
var ErrNoUserNS = errors.New("rootless: the bundle names no user namespace (" + AnnotationUserNS + "); refusing to run runsc as the holder's root")

// RunscBundle finds the OCI bundle a runsc command line is about: its
// --bundle (or -bundle) flag, or else cwd, which is where containerd
// starts the shim and so every runsc the shim runs.
func RunscBundle(args []string, cwd string) string {
	for i, a := range args {
		for _, f := range []string{"--bundle", "-bundle", "-b"} {
			if a == f && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, f+"="); ok {
				return v
			}
		}
	}
	return cwd
}

// RunscUserNS reads the user namespace a bundle's container runs in,
// and its root's id in the holder's namespace, from its config.json's
// AnnotationUserNS and AnnotationRoot. A root of 0, the holder's own,
// is refused like a missing namespace.
func RunscUserNS(bundle string) (userns string, root int, err error) {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrNoUserNS, err)
	}
	var spec struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return "", 0, fmt.Errorf("rootless: %s: %w", bundle, err)
	}
	userns = spec.Annotations[AnnotationUserNS]
	if userns == "" {
		return "", 0, ErrNoUserNS
	}
	root, err = strconv.Atoi(spec.Annotations[AnnotationRoot])
	if err != nil || root <= 0 {
		return "", 0, fmt.Errorf("%w: %s is %q", ErrNoUserNS, AnnotationRoot, spec.Annotations[AnnotationRoot])
	}
	return userns, root, nil
}

// prepareBundle gives the bundle directory to the container's root, so
// that runsc, running as it, can write its log and pid file there. The
// files containerd wrote stay the holder's, readable.
func prepareBundle(bundle string, root int) error {
	fi, err := os.Stat(bundle)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == root {
		return nil
	}
	return os.Chown(bundle, root, root)
}

// runscRoot is the --root in a runsc command line, if any.
func runscRoot(args []string) string {
	for i, a := range args {
		if a == "--root" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--root="); ok {
			return v
		}
	}
	return ""
}

// prepareRunscRoot makes runsc's root directory shared and sticky:
// each container's runsc runs as that container's root, a subuid of
// its silo's range, and writes its own state files there. The sticky
// bit keeps one range from removing another's.
func prepareRunscRoot(root string) error {
	if root == "" {
		return nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.Chmod(root, 0o777|os.ModeSticky)
}

// processFile is the --process in a `runsc exec` command line, if any:
// the exec'd process's spec, which the shim writes to its own temporary
// directory, mode 0600, as the holder's root.
func processFile(args []string) string {
	for i, a := range args {
		if (a == "--process" || a == "-process") && i+1 < len(args) {
			return args[i+1]
		}
		for _, f := range []string{"--process=", "-process="} {
			if v, ok := strings.CutPrefix(a, f); ok {
				return v
			}
		}
	}
	return ""
}

// prepareProcessFile gives an exec's process file to the container's
// root, which runsc runs as and which couldn't read it otherwise.
func prepareProcessFile(args []string, root int) error {
	f := processFile(args)
	if f == "" {
		return nil
	}
	return os.Chown(f, root, root)
}

// valueFlags are runsc's global flags the shim may pass with their
// value as the next argument.
var valueFlags = map[string]bool{"root": true, "log": true, "log-format": true, "debug-log": true, "bundle": true, "b": true, "pid-file": true}

// runscCommand is a runsc command line's subcommand: create, start,
// wait and so on.
func runscCommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if !strings.Contains(a, "=") && valueFlags[strings.TrimLeft(a, "-")] {
			i++
		}
	}
	return ""
}

// flagValue is the value of flag name (-name or --name, with = or as
// the next argument) in a runsc command line.
func flagValue(args []string, name string) string {
	for i, a := range args {
		for _, f := range []string{"--" + name, "-" + name} {
			if a == f && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, f+"="); ok {
				return v
			}
		}
	}
	return ""
}
