//go:build !linux

package local

import "errors"

// FS is a filesystem, as statfs(2) reads it: its magic number, and its
// size in bytes.
type FS struct {
	Type int64
	Size uint64
}

func statfs(string) (FS, error) { return FS{}, errors.New("the local backend needs Linux") }
