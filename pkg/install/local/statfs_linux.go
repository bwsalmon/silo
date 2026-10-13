package local

import "syscall"

// FS is a filesystem, as statfs(2) reads it: its magic number, and its
// size in bytes.
type FS struct {
	Type int64
	Size uint64
}

func statfs(path string) (FS, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FS{}, err
	}
	return FS{Type: int64(st.Type), Size: st.Blocks * uint64(st.Bsize)}, nil
}
