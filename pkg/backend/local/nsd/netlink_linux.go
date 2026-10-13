package nsd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// rtnl is the little of rtnetlink the holder needs: bridges, veths,
// addresses, MACs and routes. It is not a general library; silod's
// fence uses the netlink sockets the holder passes it.
type rtnl struct {
	fd  int
	seq atomic.Uint32
}

func openRtnl() (*rtnl, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	return &rtnl{fd: fd}, nil
}

// openRtnlIn opens a NETLINK_ROUTE socket in the netns at path. The
// socket stays in that netns for its life. The thread that entered it
// is never unlocked, so the runtime discards it with the goroutine.
func openRtnlIn(path string) (*rtnl, error) {
	type result struct {
		r   *rtnl
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		ns, err := os.Open(path)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer ns.Close()
		if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- result{err: fmt.Errorf("setns %s: %w", path, err)}
			return
		}
		r, err := openRtnl()
		ch <- result{r, err}
	}()
	res := <-ch
	return res.r, res.err
}

func (r *rtnl) Close() error { return unix.Close(r.fd) }

type attrs []byte

func (a attrs) add(t uint16, data []byte) attrs {
	n := unix.NLA_HDRLEN + len(data)
	hdr := make([]byte, 4)
	binary.NativeEndian.PutUint16(hdr, uint16(n))
	binary.NativeEndian.PutUint16(hdr[2:], t)
	a = append(a, hdr...)
	a = append(a, data...)
	for len(a)%4 != 0 {
		a = append(a, 0)
	}
	return a
}

func (a attrs) str(t uint16, s string) attrs { return a.add(t, append([]byte(s), 0)) }

func (a attrs) u32(t uint16, v uint32) attrs {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return a.add(t, b)
}

func ifinfo(index int32, flags, change uint32) []byte {
	b := make([]byte, unix.SizeofIfInfomsg)
	b[0] = unix.AF_UNSPEC
	binary.NativeEndian.PutUint32(b[4:], uint32(index))
	binary.NativeEndian.PutUint32(b[8:], flags)
	binary.NativeEndian.PutUint32(b[12:], change)
	return b
}

// do sends one request and waits for its acknowledgement, returning
// the payloads of any replies before it.
func (r *rtnl) do(typ uint16, flags uint16, body []byte) ([][]byte, error) {
	seq := r.seq.Add(1)
	msg := make([]byte, unix.NLMSG_HDRLEN, unix.NLMSG_HDRLEN+len(body))
	binary.NativeEndian.PutUint32(msg, uint32(unix.NLMSG_HDRLEN+len(body)))
	binary.NativeEndian.PutUint16(msg[4:], typ)
	binary.NativeEndian.PutUint16(msg[6:], flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	binary.NativeEndian.PutUint32(msg[8:], seq)
	msg = append(msg, body...)
	if err := unix.Sendto(r.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}
	var replies [][]byte
	buf := make([]byte, 1<<16)
	for {
		n, _, err := unix.Recvfrom(r.fd, buf, 0)
		if err != nil {
			return nil, err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case unix.NLMSG_ERROR:
				if code := int32(binary.NativeEndian.Uint32(m.Data)); code != 0 {
					return replies, unix.Errno(-code)
				}
				return replies, nil
			case unix.NLMSG_DONE:
				return replies, nil
			default:
				replies = append(replies, append([]byte(nil), m.Data...))
			}
		}
	}
}

// index returns a link's index by name.
func (r *rtnl) index(name string) (int32, error) {
	replies, err := r.do(unix.RTM_GETLINK, 0, append(ifinfo(0, 0, 0), attrs(nil).str(unix.IFLA_IFNAME, name)...))
	if err != nil {
		return 0, fmt.Errorf("link %s: %w", name, err)
	}
	if len(replies) == 0 || len(replies[0]) < unix.SizeofIfInfomsg {
		return 0, fmt.Errorf("link %s: no reply", name)
	}
	return int32(binary.NativeEndian.Uint32(replies[0][4:])), nil
}

func (r *rtnl) newLink(name string, info attrs, extra attrs) error {
	a := attrs(nil).str(unix.IFLA_IFNAME, name).add(unix.IFLA_LINKINFO, info)
	a = append(a, extra...)
	_, err := r.do(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, append(ifinfo(0, 0, 0), a...))
	if err != nil {
		return fmt.Errorf("create link %s: %w", name, err)
	}
	return nil
}

// bridge creates a bridge. An existing link of that name is an error
// unless exist is true.
func (r *rtnl) bridge(name string, exist bool) error {
	err := r.newLink(name, attrs(nil).str(unix.IFLA_INFO_KIND, "bridge"), nil)
	if exist && errors.Is(err, unix.EEXIST) {
		return nil
	}
	return err
}

// veth creates a veth pair whose peer is moved into the netns open as
// peerNS on creation.
func (r *rtnl) veth(name, peer string, peerNS int) error {
	peerAttrs := append(ifinfo(0, 0, 0), attrs(nil).str(unix.IFLA_IFNAME, peer).u32(unix.IFLA_NET_NS_FD, uint32(peerNS))...)
	info := attrs(nil).str(unix.IFLA_INFO_KIND, "veth").add(unix.IFLA_INFO_DATA, attrs(nil).add(1 /* VETH_INFO_PEER */, peerAttrs))
	return r.newLink(name, info, nil)
}

func (r *rtnl) setLink(name string, flags, change uint32, a attrs) error {
	idx, err := r.index(name)
	if err != nil {
		return err
	}
	if _, err := r.do(unix.RTM_NEWLINK, 0, append(ifinfo(idx, flags, change), a...)); err != nil {
		return fmt.Errorf("set link %s: %w", name, err)
	}
	return nil
}

func (r *rtnl) up(name string) error { return r.setLink(name, unix.IFF_UP, unix.IFF_UP, nil) }

func (r *rtnl) master(name, bridge string) error {
	m, err := r.index(bridge)
	if err != nil {
		return err
	}
	return r.setLink(name, 0, 0, attrs(nil).u32(unix.IFLA_MASTER, uint32(m)))
}

func (r *rtnl) mac(name string, hw net.HardwareAddr) error {
	return r.setLink(name, 0, 0, attrs(nil).add(unix.IFLA_ADDRESS, hw))
}

func (r *rtnl) delLink(name string) error {
	idx, err := r.index(name)
	if errors.Is(err, unix.ENODEV) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = r.do(unix.RTM_DELLINK, 0, ifinfo(idx, 0, 0))
	return err
}

// addr adds an address to a link. An address already there is not an
// error, so that the holder's start is idempotent.
func (r *rtnl) addr(name string, p netip.Prefix) error {
	idx, err := r.index(name)
	if err != nil {
		return err
	}
	fam, key := byte(unix.AF_INET), p.Addr().AsSlice()
	if p.Addr().Is6() {
		fam = unix.AF_INET6
	}
	b := []byte{fam, byte(p.Bits()), 0, unix.RT_SCOPE_UNIVERSE, 0, 0, 0, 0}
	binary.NativeEndian.PutUint32(b[4:], uint32(idx))
	a := attrs(nil).add(unix.IFA_LOCAL, key).add(unix.IFA_ADDRESS, key)
	_, err = r.do(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, append(b, a...))
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("address %s on %s: %w", p, name, err)
	}
	return nil
}

// defaultRoute adds a default route through gw.
func (r *rtnl) defaultRoute(gw netip.Addr) error {
	fam := byte(unix.AF_INET)
	if gw.Is6() {
		fam = unix.AF_INET6
	}
	b := []byte{fam, 0, 0, 0, unix.RT_TABLE_MAIN, unix.RTPROT_BOOT, unix.RT_SCOPE_UNIVERSE, unix.RTN_UNICAST, 0, 0, 0, 0}
	_, err := r.do(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL, append(b, attrs(nil).add(unix.RTA_GATEWAY, gw.AsSlice())...))
	if err != nil {
		return fmt.Errorf("default route via %s: %w", gw, err)
	}
	return nil
}
