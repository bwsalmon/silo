//go:build !linux

package rootless

import (
	"context"
	"errors"
)

// RunscMain is `silod runsc`. The sandbox class needs Linux.
func RunscMain(exe string, args []string) error {
	return errors.New("silod runsc needs Linux")
}

// ContainerdMain is `silod containerd`, which needs Linux.
func ContainerdMain(ctx context.Context, exe string, args []string) error {
	return errors.New("silod containerd needs Linux")
}

// BuildkitdMain is `silod buildkitd`, which needs Linux.
func BuildkitdMain(ctx context.Context, exe string, args []string) error {
	return errors.New("silod buildkitd needs Linux")
}
