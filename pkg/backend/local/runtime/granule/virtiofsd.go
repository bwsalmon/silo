package granule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Virtiofsd runs one virtiofsd per volume of a VM (local-backend.md §6):
// granule attaches virtio-fs shares but doesn't start their daemons
// (granule docs/volumes.md), so the driver does.
//
// A share's read-only flag is enforced here, on the host. granule-
// container also mounts a read-only share MS_RDONLY, but that is inside
// the guest, whose kernel is the tenant's own, so it is no fence.
type Virtiofsd struct {
	// Binary is virtiofsd's path, the Rust one (1.11 or later, for
	// --readonly; granule tests with 1.13).
	Binary string
	// Start runs argv inside the user namespace userns, as its root,
	// so that what the guest chowns lands on the container's subuids
	// rather than on silod's, and returns once it has started. The
	// process must outlive silod, like the shims.
	Start func(ctx context.Context, userns string, argv []string) error
	// Wait is how long a virtiofsd has to make its socket; 10s when 0.
	Wait time.Duration
}

// Args is the virtiofsd command line serving s. The sandbox is a
// chroot, the one an unprivileged virtiofsd can make (namespace needs
// CAP_SYS_ADMIN), and mknod is dropped from its capabilities, which a
// user namespace's root may not have (granule docs/rootless.md).
func (f Virtiofsd) Args(s Share) []string {
	a := []string{f.Binary,
		"--socket-path=" + s.Socket,
		"--shared-dir=" + s.Source,
		"--sandbox=chroot",
		"--modcaps=-mknod",
	}
	if s.ReadOnly {
		a = append(a, "--readonly")
	}
	return a
}

// StartAll starts the virtiofsd of each of v's shares inside userns
// and waits for their sockets, which granule-shim connects to when the
// VM boots.
func (f Virtiofsd) StartAll(ctx context.Context, v VM, userns string) error {
	if len(v.Shares) == 0 {
		return nil
	}
	if f.Binary == "" || f.Start == nil {
		return errors.New("granule: volumes need virtiofsd, and the driver was given none")
	}
	if err := os.MkdirAll(filepath.Join(v.Dir, "fs"), 0o700); err != nil {
		return err
	}
	wait := f.Wait
	if wait == 0 {
		wait = 10 * time.Second
	}
	for _, s := range v.Shares {
		// A socket left by an earlier start would satisfy the wait
		// below without anything serving it.
		if err := os.Remove(s.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := f.Start(ctx, userns, f.Args(s)); err != nil {
			return fmt.Errorf("granule: virtiofsd for %s: %w", s.Target, err)
		}
	}
	deadline := time.Now().Add(wait)
	for _, s := range v.Shares {
		for {
			if fi, err := os.Stat(s.Socket); err == nil && fi.Mode()&os.ModeSocket != 0 {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("granule: virtiofsd for %s made no socket at %s in %v", s.Target, s.Socket, wait)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	return nil
}
