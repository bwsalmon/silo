//go:build !linux

package rootless

import "errors"

// RunscMain is `silod runsc`. The sandbox class needs Linux.
func RunscMain(exe string, args []string) error {
	return errors.New("silod runsc needs Linux")
}
