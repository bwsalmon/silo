//go:build !linux

package hostdoor

import (
	"fmt"
	"net/netip"
)

// InetDiagUID is Linux's: elsewhere there is no inet_diag, so no peer
// can be found.
func InetDiagUID(local, peer netip.AddrPort) (uint32, error) {
	return 0, fmt.Errorf("inet_diag is Linux's: %w", ErrNoPeer)
}
