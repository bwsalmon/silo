//go:build unix

package router

import (
	"os/exec"
	"syscall"
	"time"
)

// ownGroup puts the entry point in a process group of its own, so that
// everything it starts can be stopped with it.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// stopGroup sends SIGTERM to process group pgid, then SIGKILL once grace
// has passed with any member still running. exited closes when the
// group's leader has been reaped.
func stopGroup(pgid int, exited <-chan struct{}, grace time.Duration) {
	syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.After(grace)
	select {
	case <-exited:
	case <-deadline:
	}
	// The leader may have gone while the rest of its group lives on.
	for groupAlive(pgid) {
		select {
		case <-deadline:
			syscall.Kill(-pgid, syscall.SIGKILL)
			<-exited
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
	<-exited
}

func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}
