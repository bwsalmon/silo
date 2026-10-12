//go:build !unix

package storage

import "io/fs"

type fileID struct{}

// onDisk is fi's length: without unix's stat there are no blocks or
// links to read.
func onDisk(fi fs.FileInfo) (bytes int64, id fileID, linked bool) {
	return fi.Size(), fileID{}, false
}
