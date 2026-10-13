//go:build !linux

package main

import (
	"context"
	"errors"
)

// unshare needs Linux: the namespace holder is the local backend's.
func (c *cli) unshare(ctx context.Context, args []string) error {
	return errors.New("silo unshare needs Linux")
}
