package session

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ScopePeer is the local backend's peer check (local-backend.md §10,
// call L18). Every ambient session runs as the agent user, so the
// socket's file mode can't keep one session off another's socket; this
// accepts a connection only from a process whose cgroup is scope(agent),
// the session's own systemd scope (harness.ScopeName), in silod's own
// user manager when silod runs in one.
//
// The peer is held by a pidfd, from SO_PEERPIDFD where the kernel has
// it (6.5) and otherwise from SO_PEERCRED's pid and pidfd_open, and its
// cgroup is read through /proc and then the pidfd checked still alive:
// a pid that exited and was reused while it was being checked fails.
//
// What it does not do is stand against a process of the agent user's
// that means harm: that user owns its manager's cgroups, and can move a
// process into a session's scope. It keeps each session's harness, and
// whatever it starts, off every other session's socket.
func ScopePeer(scope func(agent string) string) PeerCheck {
	manager := ""
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		manager = UserManager(cgroupV2(string(b)))
	}
	return func(agent string, c net.Conn) error {
		pidfd, pid, err := peerPidfd(c)
		if err != nil {
			return err
		}
		defer unix.Close(pidfd)
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
		if err != nil {
			return fmt.Errorf("%w: pid %d: %v", ErrPeer, pid, err)
		}
		path := cgroupV2(string(b))
		if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
			return fmt.Errorf("%w: pid %d exited while it was checked", ErrPeer, pid)
		}
		if !InCgroup(path, scope(agent), manager) {
			return fmt.Errorf("%w: pid %d is in %s, not %s", ErrPeer, pid, path, scope(agent))
		}
		return nil
	}
}

// cgroupV2 is the unified hierarchy's line of /proc/<pid>/cgroup.
func cgroupV2(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return p
		}
	}
	return ""
}

// peerPidfd is a pidfd of c's peer, and its pid.
func peerPidfd(c net.Conn) (pidfd, pid int, err error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return -1, 0, fmt.Errorf("%w: not a Unix socket", ErrPeer)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1, 0, err
	}
	var cred *unix.Ucred
	var serr error
	pidfd = -1
	if err := raw.Control(func(fd uintptr) {
		if p, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD); err == nil {
			pidfd = p
			return
		}
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return -1, 0, err
	}
	if pidfd >= 0 {
		pid, err := pidfdPid(pidfd)
		if err != nil {
			unix.Close(pidfd)
			return -1, 0, err
		}
		return pidfd, pid, nil
	}
	if serr != nil {
		return -1, 0, fmt.Errorf("%w: SO_PEERCRED: %v", ErrPeer, serr)
	}
	if cred.Pid <= 0 {
		return -1, 0, fmt.Errorf("%w: no peer pid", ErrPeer)
	}
	pidfd, err = unix.PidfdOpen(int(cred.Pid), 0)
	if err != nil {
		return -1, 0, fmt.Errorf("%w: pid %d: %v", ErrPeer, cred.Pid, err)
	}
	return pidfd, int(cred.Pid), nil
}

// pidfdPid is the pid a pidfd holds, from its fdinfo.
func pidfdPid(pidfd int) (int, error) {
	b, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(pidfd))
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Pid:"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || pid <= 0 {
				return 0, fmt.Errorf("%w: the peer has exited", ErrPeer)
			}
			return pid, nil
		}
	}
	return 0, errors.New("session: no Pid in a pidfd's fdinfo")
}
