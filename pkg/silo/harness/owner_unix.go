//go:build unix

package harness

import (
	"io/fs"
	"syscall"
)

func fileOwner(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
