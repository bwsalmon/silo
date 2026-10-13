package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/backend/local/nsd/nsexec"
	"github.com/bwsalmon/silo/pkg/backend/local/rootless"
)

// unshare is `silo unshare [--probe] [-- command...]`: run a command
// (the user's shell by default) as root inside the local backend's
// namespace holder, in its user, mount and network namespaces, as
// `podman unshare` does. --probe instead reports the holder's
// prerequisites on this host.
func (c *cli) unshare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("unshare", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	probe := fs.Bool("probe", false, "check this host's prerequisites for the holder, and change nothing")
	state := fs.String("state", "", "the holder's state file (default $XDG_RUNTIME_DIR/silo/nsd.state)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *probe {
		// The holder's own prerequisites, then the runtimes' (step 16):
		// containerd, runsc, crun, KVM and cgroup delegation. Only the
		// holder's are fatal; a runtime that is missing makes its class
		// unavailable.
		holder := nsd.Probe()
		rt := rootless.Probe(rootless.LocalHost())
		for _, ch := range append(slices.Clone(holder), rt.Checks...) {
			mark := "ok  "
			if !ch.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(c.stdout, "%s %s: %s\n", mark, ch.Name, ch.Detail)
			if ch.Fix != "" {
				fmt.Fprintf(c.stdout, "     fix: %s\n", ch.Fix)
			}
		}
		if cg := rt.Cgroups; len(cg.Missing) > 0 {
			fmt.Fprintf(c.stdout, "cgroups: %s (missing: %s)\n", cg.Enforced, strings.Join(cg.Missing, ", "))
		}
		if n := len(nsd.Failed(holder)); n > 0 {
			return fmt.Errorf("%d prerequisite(s) missing", n)
		}
		return nil
	}
	argv := fs.Args()
	if len(argv) == 0 {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		argv = []string{sh}
	}
	if os.Getenv("_SILO_UNSHARE_JOINED") != "" {
		// nsexec's join stage has run: this process is in the holder.
		os.Unsetenv("_SILO_UNSHARE_JOINED")
		p, err := exec.LookPath(argv[0])
		if err != nil {
			return err
		}
		return syscall.Exec(p, argv, os.Environ())
	}
	if !nsexec.Available {
		return errors.New("this silo was built without cgo, which joining the holder needs")
	}
	if *state == "" {
		rt, err := nsd.RuntimeDir()
		if err != nil {
			return err
		}
		*state = filepath.Join(rt, "nsd.state")
	}
	st, err := nsd.ReadState(*state)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := nsd.JoinCommand(st.Pid, exe, append([]string{"unshare", "--"}, argv...)...)
	cmd.Env = append(cmd.Env, "_SILO_UNSHARE_JOINED=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, c.stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("%s exited with status %d", argv[0], ee.ExitCode())
	}
	return err
}
