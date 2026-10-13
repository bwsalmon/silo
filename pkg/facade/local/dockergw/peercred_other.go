//go:build !linux

package dockergw

import (
	"errors"
	"net"
)

// PeerCred is the uid of a Unix socket connection's peer, which needs
// Linux's SO_PEERCRED.
func PeerCred(net.Conn) (uint32, error) {
	return 0, errors.New("dockergw: a peer's uid needs Linux")
}
