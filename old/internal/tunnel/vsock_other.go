//go:build !linux

package tunnel

import (
	"errors"
	"os"
)

// HostCID is the vsock address of the host as a guest sees it.
const HostCID = 2

// VsockConn is a connected vsock stream socket.
type VsockConn struct{ *os.File }

// CloseWrite shuts the sending half.
func (c VsockConn) CloseWrite() error { return errors.ErrUnsupported }

// DialVsock is Linux only: the guests silo forwards from are.
func DialVsock(cid, port uint32) (VsockConn, error) {
	return VsockConn{}, errors.ErrUnsupported
}
