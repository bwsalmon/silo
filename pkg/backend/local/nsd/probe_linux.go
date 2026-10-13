package nsd

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func getxattr(path, name string) bool {
	n, err := unix.Getxattr(path, name, nil)
	return err == nil && n > 0
}

// tryUserNS runs /bin/true in a new user namespace.
func tryUserNS() error {
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER}
	return cmd.Run()
}
