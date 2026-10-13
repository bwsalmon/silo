package hostdoor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// The sizes and values of linux/inet_diag.h and linux/netlink.h this
// file encodes by hand, so that nothing depends on how a Go struct is
// laid out.
const (
	nlmsgHdrLen      = 16
	inetDiagReqLen   = 56 // struct inet_diag_req_v2
	inetDiagMsgLen   = 72 // struct inet_diag_msg
	inetDiagIDLen    = 48 // struct inet_diag_sockid
	nlmsgError       = 2
	nlmsgDone        = 3
	tcpTimeWait      = 6
	tcpNewSynRecv    = 12
	inetDiagNoCookie = ^uint32(0)
)

// InetDiagUID returns the uid that owns the TCP socket whose local
// address is peer and whose remote address is local: the other end, on
// this host, of a connection accepted at local from peer. It asks the
// kernel with NETLINK_SOCK_DIAG (inet_diag), as ss -e does, with an
// exact lookup of that one socket.
//
// A socket that is gone, or that is only a TIME_WAIT or SYN_RECV
// remnant (which the kernel reports as uid 0, since it has no owner), is
// an error: the caller has no uid to act as.
func InetDiagUID(local, peer netip.AddrPort) (uint32, error) {
	local, peer = netip.AddrPortFrom(local.Addr().Unmap(), local.Port()), netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
	family := uint8(unix.AF_INET)
	if peer.Addr().Is6() {
		family = unix.AF_INET6
	}
	if local.Addr().Is6() != peer.Addr().Is6() {
		return 0, fmt.Errorf("inet_diag: %s and %s are different families", local, peer)
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return 0, os.NewSyscallError("inet_diag: socket", err)
	}
	defer unix.Close(fd)
	tv := unix.Timeval{Sec: 2}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return 0, os.NewSyscallError("inet_diag: setsockopt", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, os.NewSyscallError("inet_diag: bind", err)
	}

	const seq = 1
	req := make([]byte, nlmsgHdrLen+inetDiagReqLen)
	ne := binary.NativeEndian
	ne.PutUint32(req[0:], uint32(len(req)))
	ne.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	ne.PutUint16(req[6:], unix.NLM_F_REQUEST) // no NLM_F_DUMP: one socket, by its address
	ne.PutUint32(req[8:], seq)
	r := req[nlmsgHdrLen:]
	r[0] = family
	r[1] = unix.IPPROTO_TCP
	ne.PutUint32(r[4:], ^uint32(0)) // every state; the answer's is checked below
	putSockID(r[8:], peer, local)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, os.NewSyscallError("inet_diag: sendto", err)
	}

	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return 0, os.NewSyscallError("inet_diag: recvfrom", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return 0, fmt.Errorf("inet_diag: %w", err)
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case nlmsgError:
				if len(m.Data) < 4 {
					return 0, errors.New("inet_diag: short error message")
				}
				errno := -int32(ne.Uint32(m.Data))
				if errno == 0 {
					continue
				}
				if syscall.Errno(errno) == syscall.ENOENT {
					return 0, fmt.Errorf("inet_diag: no socket %s -> %s: %w", peer, local, ErrNoPeer)
				}
				return 0, fmt.Errorf("inet_diag: %w", syscall.Errno(errno))
			case nlmsgDone:
				return 0, fmt.Errorf("inet_diag: no socket %s -> %s: %w", peer, local, ErrNoPeer)
			case unix.SOCK_DIAG_BY_FAMILY:
				return diagUID(m.Data, peer, local)
			}
		}
	}
}

// putSockID writes a struct inet_diag_sockid for the socket at src
// connected to dst. Ports and addresses are in network order.
func putSockID(b []byte, src, dst netip.AddrPort) {
	binary.BigEndian.PutUint16(b[0:], src.Port())
	binary.BigEndian.PutUint16(b[2:], dst.Port())
	s, d := src.Addr().AsSlice(), dst.Addr().AsSlice()
	copy(b[4:20], s)
	copy(b[20:36], d)
	// idiag_if 0: any interface. idiag_cookie: none.
	binary.NativeEndian.PutUint32(b[40:], inetDiagNoCookie)
	binary.NativeEndian.PutUint32(b[44:], inetDiagNoCookie)
}

// diagUID reads the uid from a struct inet_diag_msg, once it has
// checked that the message is the socket asked for, and a full one.
func diagUID(b []byte, src, dst netip.AddrPort) (uint32, error) {
	if len(b) < inetDiagMsgLen {
		return 0, errors.New("inet_diag: short answer")
	}
	state := b[1]
	if state == tcpTimeWait || state == tcpNewSynRecv {
		return 0, fmt.Errorf("inet_diag: socket %s -> %s has closed (state %d): %w", src, dst, state, ErrNoPeer)
	}
	want := make([]byte, inetDiagIDLen)
	putSockID(want, src, dst)
	id := b[4 : 4+inetDiagIDLen]
	// The ports and both addresses must be the ones asked for; the
	// interface and cookie are the kernel's to fill in.
	if string(id[:36]) != string(want[:36]) {
		return 0, fmt.Errorf("inet_diag: asked for %s -> %s, the kernel answered with another socket", src, dst)
	}
	return binary.NativeEndian.Uint32(b[4+inetDiagIDLen+12:]), nil
}
