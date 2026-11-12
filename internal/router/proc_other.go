//go:build !unix

package router

import (
	"os"
	"os/exec"
	"time"
)

func ownGroup(cmd *exec.Cmd) {}

// stopGroup can only kill the entry point itself here.
func stopGroup(pid int, exited <-chan struct{}, grace time.Duration) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
	<-exited
}
