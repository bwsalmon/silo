package nsd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

// NSIDs are a process's user, mount and network namespaces as
// /proc/<pid>/ns reads them ("user:[4026532190]"). The holder records
// its own in nsd.state, so that a holder started after it can find
// them again (call L33).
type NSIDs struct {
	User string `json:"user"`
	Mnt  string `json:"mnt"`
	Net  string `json:"net"`
}

// ReadNSIDs reads pid's namespaces.
func ReadNSIDs(pid int) (NSIDs, error) {
	var ids NSIDs
	var errs []error
	for _, f := range []struct {
		kind string
		to   *string
	}{{"user", &ids.User}, {"mnt", &ids.Mnt}, {"net", &ids.Net}} {
		s, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, f.kind))
		*f.to, errs = s, append(errs, err)
	}
	return ids, errors.Join(errs...)
}

var (
	// ErrHolderRunning is a holder that still answers on nsd.sock: a
	// second one would take its socket away.
	ErrHolderRunning = errors.New("nsd: a holder is already running")
	// ErrNothingToAdopt is no process left in the recorded namespaces,
	// so they are gone: the reboot case, in which the holder makes
	// fresh ones.
	ErrNothingToAdopt = errors.New("nsd: nothing is left in the previous holder's namespaces")
)

// Survivor finds a process still in the user, mount and network
// namespaces st recorded: the previous holder, or a member that
// outlived it (containerd, a shim, buildkitd). Each candidate is held
// by a pidfd while its namespaces are read, and is still alive after,
// so that a pid reused in between is never taken for a member.
func Survivor(st State) (int, error) {
	if st.NS == (NSIDs{}) {
		return 0, ErrNothingToAdopt
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	pids := []int{st.Pid}
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid != st.Pid && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			continue
		}
		ids, err := ReadNSIDs(pid)
		alive := unix.PidfdSendSignal(fd, 0, nil, 0) == nil
		unix.Close(fd)
		if err == nil && alive && ids == st.NS {
			return pid, nil
		}
	}
	return 0, ErrNothingToAdopt
}

// holderAnswers reports whether a holder is listening on socket.
func holderAnswers(socket string) bool {
	c, err := net.Dial("unixpacket", socket)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Adopt starts exe with args as a holder inside the namespaces a
// previous holder left, which st recorded (call L33): it joins them
// through a surviving member, as root of their user namespace, and the
// child then runs Run with Config.Adopt, which takes over the pins,
// silo-net and gw0 as they are. The namespaces must map m, the mapping
// a fresh holder would get, so that the store's recorded mapping and
// every container range still hold. It returns ErrNothingToAdopt when
// nothing is left, and ErrHolderRunning when the previous holder still
// serves socket.
func Adopt(exe string, args, env []string, st State, socket string, m Mapping, stdout, stderr io.Writer) (*exec.Cmd, int, error) {
	if holderAnswers(socket) {
		return nil, 0, fmt.Errorf("%w on %s (pid %d)", ErrHolderRunning, socket, st.Pid)
	}
	pid, err := Survivor(st)
	if err != nil {
		return nil, 0, err
	}
	got, err := ReadMapping(pid)
	if err != nil {
		return nil, 0, err
	}
	if got.String() != m.String() {
		return nil, 0, fmt.Errorf("nsd: the namespaces process %d is in map %s, but this user's ranges now give %s: "+
			"stop silo-containerd and silo-buildkitd, so that nothing is left in them, and start silo-nsd again for fresh ones", pid, got, m)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(append([]string{}, env...), "_SILO_NSEXEC=join", "_SILO_NSEXEC_PID="+strconv.Itoa(pid), "_SILO_NSD_ADOPT=1")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	return cmd, pid, nil
}
