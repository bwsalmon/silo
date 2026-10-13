//go:build !linux

package nsd

import (
	"context"
	"errors"
)

// Main is `silod nsd`. The holder needs Linux.
func Main(ctx context.Context, exe string, args []string) error {
	return errors.New("silo-nsd needs Linux")
}
