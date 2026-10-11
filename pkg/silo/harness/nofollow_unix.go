//go:build unix

package harness

import "syscall"

const noFollow = syscall.O_NOFOLLOW
