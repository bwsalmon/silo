package silofile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/silo/v2/internal/safepath"
)

// FileName is the name of the Silofile in a silo folder.
const FileName = "Silofile"

// Load reads and strictly decodes a silo (design §2.2): a silo folder,
// whose Silofile is <dir>/Silofile, or a bare Silofile, whose folder is
// the file's directory. A problem with the Silofile itself is returned
// as Errors; any other error (the path does not exist, cannot be read)
// is returned as it is.
func Load(path string) (*Silofile, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	file := path
	if fi.IsDir() {
		file = filepath.Join(path, FileName)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && fi.IsDir() {
			return nil, fmt.Errorf("%s is not a silo folder: it has no %s", path, FileName)
		}
		return nil, err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data, file)
	if err != nil {
		return nil, err
	}
	f.File, f.Folder = abs, filepath.Dir(abs)
	return f, nil
}

// fileKind is what a path in the folder must be.
type fileKind int

const (
	anyFile fileKind = iota
	regularFile
	directory
)

// isOutside reports whether p is written as a path outside the folder:
// absolute, or relative to a home directory.
func isOutside(p string) bool {
	return filepath.IsAbs(p) || p == "~" || strings.HasPrefix(p, "~/")
}

// resolve checks that p, a path relative to the silo folder, stays in it
// through .. and symlinks and names an existing file of the given kind.
// It returns the resolved path, or a message saying what is wrong.
func resolve(folder, p string, kind fileKind) (string, string) {
	if p == "" {
		return "", "empty path"
	}
	if isOutside(p) {
		return "", fmt.Sprintf("%q is outside the silo folder: paths here are relative to the folder", p)
	}
	real, err := safepath.Within(folder, p)
	if err != nil {
		if errors.Is(err, safepath.ErrOutside) {
			return "", fmt.Sprintf("%q leaves the silo folder", p)
		}
		return "", fmt.Sprintf("%q does not resolve: %v", p, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Sprintf("%q does not exist in the silo folder", p)
		}
		return "", fmt.Sprintf("%q: %v", p, err)
	}
	switch {
	case kind == regularFile && !fi.Mode().IsRegular():
		return "", fmt.Sprintf("%q is not a regular file", p)
	case kind == directory && !fi.IsDir():
		return "", fmt.Sprintf("%q is not a directory", p)
	}
	return real, ""
}
