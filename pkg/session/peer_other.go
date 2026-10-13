//go:build !linux

package session

import (
	"fmt"
	"net"
)

// ScopePeer is the local backend's peer check, which needs Linux's
// cgroups and pidfds: elsewhere it refuses every connection.
func ScopePeer(func(agent string) string) PeerCheck {
	return func(string, net.Conn) error { return fmt.Errorf("%w: peer checks need Linux", ErrPeer) }
}
