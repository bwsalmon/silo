//go:build unix

package storage

import (
	"io/fs"
	"syscall"
)

type fileID struct{ dev, ino uint64 }

// onDisk is the space fi's file takes, from its allocated blocks, so
// that a sparse file counts what it holds rather than its length. linked
// is whether it has other links, which the caller counts once by id.
func onDisk(fi fs.FileInfo) (bytes int64, id fileID, linked bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.Size(), fileID{}, false
	}
	return int64(st.Blocks) * 512, fileID{uint64(st.Dev), uint64(st.Ino)}, !fi.IsDir() && st.Nlink > 1
}
