//go:build unix

package sqlite

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/bwsalmon/silo/pkg/silo/store"
)

// lockFile takes an exclusive, non-blocking flock on path. flock locks
// belong to the open file, so a second Open in this process conflicts as
// surely as one in another process, and the kernel drops the lock when
// the process dies, so a crash leaves nothing to clean up.
func lockFile(path string) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s is locked)", store.ErrInUse, path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f.Close, nil
}
