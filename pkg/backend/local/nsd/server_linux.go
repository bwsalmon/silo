package nsd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bwsalmon/silo/pkg/backend/local/storage"
)

// Server is the holder's side of the RPC. It runs inside the holder's
// namespaces: the sockets it opens are in silo-net because the holder
// is.
type Server struct {
	// AllowUID is the only peer uid served, as SO_PEERCRED reports it
	// in the holder's user namespace. silod's host uid is root (0)
	// there; every other host user is the overflow uid, and a
	// container's processes are subuids.
	AllowUID uint32
	// Mapping is the holder's own (ReadMapping(os.Getpid())).
	Mapping Mapping
	// PinDir is where containers' namespaces are bind-mounted, in the
	// holder's private mount namespace.
	PinDir string
	// Storage keeps volumes, whose files belong to subuids silod can't
	// touch. Nil refuses the vol_* calls.
	Storage storage.Driver
	// Self is the executable started (with _SILO_NSEXEC=pause) to
	// create a container's namespaces. It must carry package nsexec.
	Self string

	mu sync.Mutex // serializes link and namespace changes
	rt *rtnl
}

// Serve accepts connections on l until ctx is done.
func (s *Server) Serve(ctx context.Context, l *net.UnixListener) error {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serveConn(c)
	}
}

func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return cred.Uid, nil
}

func (s *Server) serveConn(c *net.UnixConn) {
	defer c.Close()
	uid, err := peerUID(c)
	buf := make([]byte, 1<<16)
	if err != nil || uid != s.AllowUID {
		// The refusal answers the first request, so that the client
		// reads it rather than a reset.
		log.Printf("silo-nsd: refused a connection from uid %d (%v)", uid, err)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		c.ReadMsgUnix(buf, nil)
		writeMsg(c, Response{Error: fmt.Sprintf("nsd: uid %d is not silod's", uid)}, nil)
		return
	}
	for {
		n, _, _, _, err := c.ReadMsgUnix(buf, nil)
		if err != nil || n == 0 {
			return
		}
		var req Request
		dec := json.NewDecoder(bytes.NewReader(buf[:n]))
		dec.DisallowUnknownFields()
		var resp Response
		var files []*os.File
		if err := dec.Decode(&req); err != nil {
			resp.Error = "nsd: bad request: " + err.Error()
		} else {
			resp, files, err = s.handle(req)
			if err != nil {
				resp = Response{Error: err.Error()}
			}
		}
		err = writeMsg(c, resp, files)
		for _, f := range files {
			f.Close()
		}
		if err != nil {
			return
		}
	}
}

func writeMsg(c *net.UnixConn, v any, files []*os.File) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var oob []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		oob = unix.UnixRights(fds...)
	}
	_, _, err = c.WriteMsgUnix(b, oob, nil)
	return err
}

var containerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var linkName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)

func (s *Server) handle(req Request) (Response, []*os.File, error) {
	ctx := context.Background()
	switch req.Op {
	case OpInfo:
		info := &Info{Pid: os.Getpid(), Mapping: s.Mapping}
		if s.Storage != nil {
			info.Driver, info.Guarantees = s.Storage.Name(), s.Storage.Guarantees()
		}
		return Response{Info: info}, nil, nil
	case OpNetlink:
		proto := map[string]int{"route": unix.NETLINK_ROUTE, "netfilter": unix.NETLINK_NETFILTER}
		p, ok := proto[req.Proto]
		if !ok {
			return Response{}, nil, fmt.Errorf("nsd: netlink protocol %q is not served", req.Proto)
		}
		fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, p)
		if err != nil {
			return Response{}, nil, err
		}
		return Response{}, []*os.File{os.NewFile(uintptr(fd), "netlink")}, nil
	case OpListen:
		f, err := listen(req.Network, req.Addr)
		if err != nil {
			return Response{}, nil, err
		}
		return Response{}, []*os.File{f}, nil
	case OpDialPool:
		if req.Count < 1 || req.Count > maxDialBatch {
			return Response{}, nil, fmt.Errorf("nsd: dial pool batch %d is not 1-%d", req.Count, maxDialBatch)
		}
		fam := unix.AF_INET
		if req.Network == "tcp6" {
			fam = unix.AF_INET6
		}
		var files []*os.File
		for range req.Count {
			fd, err := unix.Socket(fam, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				for _, f := range files {
					f.Close()
				}
				return Response{}, nil, err
			}
			files = append(files, os.NewFile(uintptr(fd), "dial"))
		}
		return Response{}, files, nil
	case OpContainer:
		ns, err := s.container(req.Container, req.Range)
		return Response{NS: ns}, nil, err
	case OpDelete:
		return Response{}, nil, s.deleteContainer(req.Container, req.Name)
	case OpBridge:
		return Response{}, nil, s.bridge(req.Name, req.Addrs)
	case OpVeth:
		return Response{}, nil, s.veth(req)
	case OpDelLink:
		if !linkName.MatchString(req.Name) {
			return Response{}, nil, fmt.Errorf("nsd: invalid link name %q", req.Name)
		}
		return Response{}, nil, s.withRtnl(func(r *rtnl) error { return r.delLink(req.Name) })
	case OpVolList:
		if s.Storage == nil {
			return Response{}, nil, errors.New("nsd: no storage driver")
		}
		es, err := s.Storage.List(ctx)
		return Response{Entries: es}, nil, err
	case OpVolCreate, OpVolDelete, OpVolUsage, OpVolLabel:
		if s.Storage == nil {
			return Response{}, nil, errors.New("nsd: no storage driver")
		}
		if req.Volume == nil {
			return Response{}, nil, errors.New("nsd: no volume")
		}
		ref := *req.Volume
		if err := ref.Valid(); err != nil {
			return Response{}, nil, err
		}
		switch req.Op {
		case OpVolCreate:
			p, err := s.Storage.Create(ctx, ref, req.Size)
			return Response{Path: p}, nil, err
		case OpVolDelete:
			return Response{}, nil, s.Storage.Delete(ctx, ref)
		case OpVolLabel:
			return Response{}, nil, s.Storage.Label(ctx, ref, req.Seq)
		default:
			u, err := s.Storage.Usage(ctx, ref)
			return Response{Usage: &u}, nil, err
		}
	}
	return Response{}, nil, fmt.Errorf("nsd: no operation %q", req.Op)
}

// listen opens a listening socket in silo-net. Ports under 1024 are
// allowed: the holder has CAP_NET_BIND_SERVICE in silo-net's owner.
func listen(network, addr string) (*os.File, error) {
	if _, err := netip.ParseAddrPort(addr); err != nil {
		return nil, fmt.Errorf("nsd: listen address: %w", err)
	}
	switch network {
	case "tcp", "tcp4", "tcp6":
		l, err := net.Listen(network, addr)
		if err != nil {
			return nil, err
		}
		defer l.Close()
		return l.(*net.TCPListener).File()
	case "udp", "udp4", "udp6":
		c, err := net.ListenPacket(network, addr)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		return c.(*net.UDPConn).File()
	}
	return nil, fmt.Errorf("nsd: network %q is not served", network)
}

func (s *Server) withRtnl(f func(*rtnl) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rt == nil {
		r, err := openRtnl()
		if err != nil {
			return err
		}
		s.rt = r
	}
	return f(s.rt)
}

func (s *Server) bridge(name string, addrs []netip.Prefix) error {
	if !linkName.MatchString(name) {
		return fmt.Errorf("nsd: invalid link name %q", name)
	}
	return s.withRtnl(func(r *rtnl) error {
		if err := r.bridge(name, true); err != nil {
			return err
		}
		for _, a := range addrs {
			if err := r.addr(name, a); err != nil {
				return err
			}
		}
		return r.up(name)
	})
}

func (s *Server) pins(id string) (user, netns string) {
	return filepath.Join(s.PinDir, id+".user"), filepath.Join(s.PinDir, id+".net")
}

// container creates a user namespace nested in the holder's, mapping
// container range n, and a netns inside it, and pins both. The netns is
// the user namespace's child so that the container's NET_ADMIN covers
// it and runsc can scrape it (call L29).
func (s *Server) container(id string, n uint32) (*ContainerNS, error) {
	if !containerID.MatchString(id) {
		return nil, fmt.Errorf("nsd: invalid container id %q", id)
	}
	rng, err := ContainerRange(s.Mapping, n)
	if err != nil {
		return nil, err
	}
	userPin, netPin := s.pins(id)
	ns := &ContainerNS{User: userPin, Net: netPin, Range: rng}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(netPin); err == nil {
		return ns, nil
	}
	m := []syscall.SysProcIDMap{{ContainerID: 0, HostID: int(rng.Outside), Size: int(rng.Size)}}
	cmd := exec.Command(s.Self)
	cmd.Env = []string{"_SILO_NSEXEC=pause"}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: m, GidMappings: m, GidMappingsEnableSetgroups: true,
		Pdeathsig: syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("nsd: container namespaces: %w", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if err := os.MkdirAll(s.PinDir, 0o755); err != nil {
		return nil, err
	}
	for _, p := range []struct{ kind, pin string }{{"user", userPin}, {"net", netPin}} {
		if err := os.WriteFile(p.pin, nil, 0o444); err != nil {
			return nil, err
		}
		src := fmt.Sprintf("/proc/%d/ns/%s", cmd.Process.Pid, p.kind)
		if err := unix.Mount(src, p.pin, "", unix.MS_BIND, ""); err != nil {
			s.unpin(id)
			return nil, fmt.Errorf("nsd: pin %s: %w", p.pin, err)
		}
	}
	return ns, nil
}

func (s *Server) unpin(id string) error {
	var errs []error
	userPin, netPin := s.pins(id)
	for _, p := range []string{netPin, userPin} {
		if err := unix.Unmount(p, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			errs = append(errs, err)
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deleteContainer deletes the container's veth (its host side, veth)
// and unpins its namespaces. Both are idempotent.
func (s *Server) deleteContainer(id, veth string) error {
	if !containerID.MatchString(id) {
		return fmt.Errorf("nsd: invalid container id %q", id)
	}
	if veth != "" {
		if !linkName.MatchString(veth) {
			return fmt.Errorf("nsd: invalid link name %q", veth)
		}
		if err := s.withRtnl(func(r *rtnl) error { return r.delLink(veth) }); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unpin(id)
}

// veth wires a container: a veth from req.Bridge in silo-net to eth0 in
// the container's netns, with its addresses, MAC and default route.
func (s *Server) veth(req Request) error {
	if !containerID.MatchString(req.Container) {
		return fmt.Errorf("nsd: invalid container id %q", req.Container)
	}
	if !linkName.MatchString(req.Name) || !linkName.MatchString(req.Bridge) {
		return fmt.Errorf("nsd: invalid link names %q, %q", req.Name, req.Bridge)
	}
	var hw net.HardwareAddr
	if req.MAC != "" {
		var err error
		if hw, err = net.ParseMAC(req.MAC); err != nil {
			return err
		}
	}
	_, netPin := s.pins(req.Container)
	nsf, err := os.Open(netPin)
	if err != nil {
		return fmt.Errorf("nsd: container %s has no namespaces: %w", req.Container, err)
	}
	defer nsf.Close()
	err = s.withRtnl(func(r *rtnl) error {
		if err := r.veth(req.Name, "eth0", int(nsf.Fd())); err != nil {
			return err
		}
		if err := r.master(req.Name, req.Bridge); err != nil {
			return err
		}
		return r.up(req.Name)
	})
	if err != nil {
		return err
	}
	cr, err := openRtnlIn(netPin)
	if err != nil {
		return err
	}
	defer cr.Close()
	if hw != nil {
		if err := cr.mac("eth0", hw); err != nil {
			return err
		}
	}
	for _, a := range req.Addrs {
		if err := cr.addr("eth0", a); err != nil {
			return err
		}
	}
	for _, l := range []string{"lo", "eth0"} {
		if err := cr.up(l); err != nil {
			return err
		}
	}
	if req.Gateway.IsValid() {
		return cr.defaultRoute(req.Gateway)
	}
	return nil
}
