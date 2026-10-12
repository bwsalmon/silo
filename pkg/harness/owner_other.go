//go:build !unix

package harness

import "io/fs"

func fileOwner(fs.FileInfo) int { return -1 }
