//go:build !linux

package procgroup

import "os/exec"

// DieWithParent is a no-op where the OS has no Pdeathsig (macOS): silo
// gc reaps what is left instead (design §3.1).
func DieWithParent(cmd *exec.Cmd) {}
