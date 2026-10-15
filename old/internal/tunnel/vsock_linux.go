package tunnel

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// afVsock is AF_VSOCK, which package syscall does not name.
const afVsock = 40

// HostCID is the vsock address of the host as a guest sees it. Under
// cloud-hypervisor's hybrid vsock, a guest's connection to HostCID port
// P arrives on the Unix socket "<the VM's vsock socket>_P" beside the VM
// (granule's /run/granule/vsock.sock_P).
const HostCID = 2

// sockaddrVM is struct sockaddr_vm.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zero      [3]uint8
}

// VsockConn is a connected vsock stream socket.
type VsockConn struct{ *os.File }

// CloseWrite shuts the sending half, passing a half-close on.
func (c VsockConn) CloseWrite() error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) { serr = syscall.Shutdown(int(fd), syscall.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}

// DialVsock connects to port on cid.
func DialVsock(cid, port uint32) (VsockConn, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return VsockConn{}, fmt.Errorf("vsock socket: %w", err)
	}
	sa := sockaddrVM{family: afVsock, port: port, cid: cid}
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa))
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			syscall.Close(fd)
			return VsockConn{}, fmt.Errorf("vsock connect %d:%d: %w", cid, port, errno)
		}
		break
	}
	return VsockConn{os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, port))}, nil
}
