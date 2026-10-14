//go:build linux

package granule

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
)

// VirtiofsdArg is the subcommand that runs VirtiofsdMain: `silod
// runtime-virtiofsd`.
const VirtiofsdArg = "runtime-virtiofsd"

// VirtiofsdMain is `silod runtime-virtiofsd`, run inside the holder,
// which owns every container's user namespace. With --userns, it starts
// the virtiofsd command line after "--" inside that namespace as its
// root, through exe's nsexec constructor, in a session of its own and
// with its output in <socket>.log, so that it outlives both this command
// and silod as the shims do; its pid goes to <socket>.pid. With --stop
// dir, it kills every virtiofsd whose pid file is in dir: silod, on the
// host, can't signal a process of a container's subuids, and the holder's
// root can.
func VirtiofsdMain(exe string, args []string) error {
	fs := flag.NewFlagSet(VirtiofsdArg, flag.ContinueOnError)
	userns := fs.String("userns", "", "the container's user namespace")
	stop := fs.String("stop", "", "kill the virtiofsds whose pid files are in this directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stop != "" {
		return stopVirtiofsds(*stop)
	}
	argv := fs.Args()
	sock := socketArg(argv)
	if *userns == "" || len(argv) == 0 || sock == "" {
		return errors.New("runtime-virtiofsd: --userns and a virtiofsd command line with --socket-path are required")
	}
	logf, err := os.OpenFile(sock+".log", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := nsd.UserNSExec(*userns, exe, argv...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(sock+".pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		cmd.Process.Kill()
		return err
	}
	// Not waited for: init reaps it once this exits.
	return cmd.Process.Release()
}

// socketArg is a virtiofsd command line's --socket-path.
func socketArg(argv []string) string {
	for _, a := range argv {
		if s, ok := strings.CutPrefix(a, "--socket-path="); ok {
			return s
		}
	}
	return ""
}

// stopVirtiofsds kills each virtiofsd whose pid file is in dir, if the
// pid is still that virtiofsd: its command line names the socket beside
// the pid file, so a pid reused since is left alone.
func stopVirtiofsds(dir string) error {
	pids, err := filepath.Glob(filepath.Join(dir, "*.sock.pid"))
	if err != nil {
		return err
	}
	var errs []error
	for _, pf := range pids {
		b, err := os.ReadFile(pf)
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid <= 0 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err == nil && bytes.Contains(cmdline, []byte("--socket-path="+strings.TrimSuffix(pf, ".pid")+"\x00")) {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("virtiofsd %d: %w", pid, err))
			}
		}
		os.Remove(pf)
	}
	return errors.Join(errs...)
}
