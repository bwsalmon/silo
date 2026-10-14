//go:build linux

package granule

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
// and silod as the shims do; its pid goes to <socket>.pid. The socket is
// made here and passed as --fd: virtiofsd, as the container's root,
// can't reach the containers' directory, which is silod's and 0700.
// With --stop
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
	os.Remove(sock)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return err
	}
	// The socket outlives this listener: virtiofsd has it.
	l.SetUnlinkOnClose(false)
	defer l.Close()
	lf, err := l.File()
	if err != nil {
		return err
	}
	defer lf.Close()
	// granule-shim, as the container's root, connects.
	if err := os.Chmod(sock, 0o666); err != nil {
		return err
	}
	vargs := make([]string, 0, len(argv))
	for _, a := range argv {
		if strings.HasPrefix(a, "--socket-path=") {
			a = "--fd=3"
		}
		vargs = append(vargs, a)
	}
	cmd := nsd.UserNSExec(*userns, exe, vargs...)
	cmd.ExtraFiles = []*os.File{lf}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(sock+".pid", []byte(strconv.Itoa(cmd.Process.Pid)+" "+startTime(cmd.Process.Pid)), 0o644); err != nil {
		cmd.Process.Kill()
		return err
	}
	// A virtiofsd that refuses its command line (an older one, without
	// --readonly) exits at once, which the socket made here would hide
	// until the VM failed to boot.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		os.Remove(sock)
		os.Remove(sock + ".pid")
		b, _ := os.ReadFile(sock + ".log")
		return fmt.Errorf("virtiofsd exited at start (%v): %s", err, strings.TrimSpace(string(b)))
	case <-time.After(virtiofsdSettle):
	}
	// Not waited for past this: init reaps it once this exits.
	return nil
}

// virtiofsdSettle is how long VirtiofsdMain watches a virtiofsd for an
// exit at start.
const virtiofsdSettle = 300 * time.Millisecond

// socketArg is a virtiofsd command line's --socket-path.
func socketArg(argv []string) string {
	for _, a := range argv {
		if s, ok := strings.CutPrefix(a, "--socket-path="); ok {
			return s
		}
	}
	return ""
}

// startTime is a process's start time, in clock ticks since boot
// (proc(5), /proc/pid/stat field 22): with its pid, what names it
// across pid reuse. "" if it has gone.
func startTime(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The command, field 2, is in parentheses and may hold spaces.
	_, rest, ok := bytes.Cut(b, []byte(") "))
	if f := strings.Fields(string(rest)); ok && len(f) > 19 {
		return f[19]
	}
	return ""
}

// stopVirtiofsds kills each virtiofsd whose pid file is in dir, if the
// pid is still that virtiofsd: one whose start time differs from the
// recorded one is a pid reused since, and is left alone.
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
		f := strings.Fields(string(b))
		if len(f) != 2 {
			os.Remove(pf)
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		if st := startTime(pid); pid > 0 && st != "" && st == f[1] {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("virtiofsd %d: %w", pid, err))
			}
		}
		os.Remove(pf)
	}
	return errors.Join(errs...)
}
