package procgroup

import (
	"os/exec"
	"syscall"
)

// DieWithParent has the kernel kill cmd when silo dies (design §3.1).
func DieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
