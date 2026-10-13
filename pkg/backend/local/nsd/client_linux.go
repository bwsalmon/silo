package nsd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bwsalmon/silo/pkg/backend/local/storage"
)

// Client is silod's connection to the holder. Calls are serialized on
// the one connection.
type Client struct {
	mu   sync.Mutex
	conn *net.UnixConn
}

// Dial connects to the holder's socket (nsd.sock).
func Dial(path string) (*Client, error) {
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, fmt.Errorf("nsd: %w", err)
	}
	return &Client{conn: c}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// call sends req and returns the response and any descriptors with it.
func (c *Client) call(req Request) (Response, []*os.File, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return Response{}, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.conn.Write(b); err != nil {
		return Response{}, nil, fmt.Errorf("nsd %s: %w", req.Op, err)
	}
	buf := make([]byte, 1<<16)
	oob := make([]byte, unix.CmsgSpace(4*maxDialBatch))
	n, oobn, _, _, err := c.conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return Response{}, nil, fmt.Errorf("nsd %s: %w", req.Op, err)
	}
	var files []*os.File
	if oobn > 0 {
		msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return Response{}, nil, err
		}
		for _, m := range msgs {
			fds, err := unix.ParseUnixRights(&m)
			if err != nil {
				return Response{}, nil, err
			}
			for _, fd := range fds {
				unix.CloseOnExec(fd)
				files = append(files, os.NewFile(uintptr(fd), string(req.Op)))
			}
		}
	}
	var resp Response
	if n == 0 {
		return resp, files, fmt.Errorf("nsd %s: connection closed", req.Op)
	}
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return resp, files, err
	}
	if resp.Error != "" {
		for _, f := range files {
			f.Close()
		}
		return resp, nil, errors.New(resp.Error)
	}
	return resp, files, nil
}

func (c *Client) one(req Request) (*os.File, error) {
	_, files, err := c.call(req)
	if err != nil {
		return nil, err
	}
	if len(files) != 1 {
		for _, f := range files {
			f.Close()
		}
		return nil, fmt.Errorf("nsd %s: %d descriptors, want 1", req.Op, len(files))
	}
	return files[0], nil
}

// Info describes the holder: its pid, mapping and storage driver.
func (c *Client) Info() (*Info, error) {
	resp, _, err := c.call(Request{Op: OpInfo})
	if err != nil {
		return nil, err
	}
	if resp.Info == nil {
		return nil, errors.New("nsd info: empty")
	}
	return resp.Info, nil
}

// Netlink returns a netlink socket opened in silo-net: proto is
// "route" or "netfilter". silod owns silo-net's user namespace, so it
// has CAP_NET_ADMIN over it and the kernel accepts its messages there.
func (c *Client) Netlink(proto string) (*os.File, error) {
	return c.one(Request{Op: OpNetlink, Proto: proto})
}

// Listen returns a listener on addr in silo-net.
func (c *Client) Listen(network, addr string) (net.Listener, error) {
	f, err := c.one(Request{Op: OpListen, Network: network, Addr: addr})
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return net.FileListener(f)
}

// ListenPacket returns a UDP socket bound to addr in silo-net.
func (c *Client) ListenPacket(network, addr string) (net.PacketConn, error) {
	f, err := c.one(Request{Op: OpListen, Network: network, Addr: addr})
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return net.FilePacketConn(f)
}

// Container creates and pins a container's user namespace (mapping
// container range n) and its netns.
func (c *Client) Container(id string, n uint32) (*ContainerNS, error) {
	resp, _, err := c.call(Request{Op: OpContainer, Container: id, Range: n})
	if err != nil {
		return nil, err
	}
	return resp.NS, nil
}

// DeleteContainer deletes a container's veth (its host-side name) and
// unpins its namespaces.
func (c *Client) DeleteContainer(id, veth string) error {
	_, _, err := c.call(Request{Op: OpDelete, Container: id, Name: veth})
	return err
}

// Bridge creates a bridge in silo-net with addrs, and sets it up. It is
// idempotent.
func (c *Client) Bridge(name string, addrs ...netip.Prefix) error {
	_, _, err := c.call(Request{Op: OpBridge, Name: name, Addrs: addrs})
	return err
}

// Veth describes a container's link: Name on Bridge in silo-net, eth0
// in the container's netns.
type Veth struct {
	Container, Name, Bridge string
	Addrs                   []netip.Prefix
	MAC                     string
	Gateway                 netip.Addr
}

// Veth wires a container to a bridge.
func (c *Client) Veth(v Veth) error {
	_, _, err := c.call(Request{Op: OpVeth, Container: v.Container, Name: v.Name, Bridge: v.Bridge,
		Addrs: v.Addrs, MAC: v.MAC, Gateway: v.Gateway})
	return err
}

// DelLink deletes a link in silo-net.
func (c *Client) DelLink(name string) error {
	_, _, err := c.call(Request{Op: OpDelLink, Name: name})
	return err
}

// DialSockets returns n unconnected TCP sockets created in silo-net.
func (c *Client) DialSockets(network string, n int) ([]*os.File, error) {
	_, files, err := c.call(Request{Op: OpDialPool, Network: network, Count: n})
	return files, err
}

// Pool is the dial pool (call L28): unconnected TCP sockets from
// silo-net, refilled a batch per RPC, so that silod can connect into
// silo-net from the host's namespace once per connection without a
// round trip to the holder.
type Pool struct {
	c       *Client
	network string
	batch   int

	mu    sync.Mutex
	files []*os.File
}

// NewPool returns a pool of network ("tcp4" or "tcp6") sockets that
// refills batch at a time.
func NewPool(c *Client, network string, batch int) *Pool {
	return &Pool{c: c, network: network, batch: min(max(batch, 1), maxDialBatch)}
}

func (p *Pool) take() (*os.File, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.files) == 0 {
		files, err := p.c.DialSockets(p.network, p.batch)
		if err != nil {
			return nil, err
		}
		p.files = files
	}
	f := p.files[len(p.files)-1]
	p.files = p.files[:len(p.files)-1]
	if len(p.files) < p.batch/2 {
		go p.refill()
	}
	return f, nil
}

func (p *Pool) refill() {
	files, err := p.c.DialSockets(p.network, p.batch)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.files = append(p.files, files...)
	p.mu.Unlock()
}

// Close closes the sockets left in the pool.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.files {
		f.Close()
	}
	p.files = nil
}

func sockaddr(ap netip.AddrPort) unix.Sockaddr {
	if ap.Addr().Is4() {
		return &unix.SockaddrInet4{Addr: ap.Addr().As4(), Port: int(ap.Port())}
	}
	return &unix.SockaddrInet6{Addr: ap.Addr().As16(), Port: int(ap.Port())}
}

// Dial connects a pooled socket from local (the gateway's address; a
// zero value leaves it to the kernel) to remote, in silo-net.
func (p *Pool) Dial(ctx context.Context, local, remote netip.AddrPort) (net.Conn, error) {
	f, err := p.take()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fd := int(f.Fd())
	if local.IsValid() {
		if err := unix.Bind(fd, sockaddr(local)); err != nil {
			return nil, fmt.Errorf("nsd dial: bind %s: %w", local, err)
		}
	}
	if d, ok := ctx.Deadline(); ok {
		tv := unix.NsecToTimeval(max(time.Until(d), time.Millisecond).Nanoseconds())
		unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
	}
	if err := unix.Connect(fd, sockaddr(remote)); err != nil {
		return nil, fmt.Errorf("nsd dial %s: %w", remote, err)
	}
	return net.FileConn(f)
}

// Storage is the holder's storage driver, behind the RPC: volumes'
// files belong to subuids that silod's own account can't touch.
func (c *Client) Storage() (storage.Driver, error) {
	info, err := c.Info()
	if err != nil {
		return nil, err
	}
	if info.Driver == "" {
		return nil, errors.New("nsd: the holder has no storage driver")
	}
	return &remoteDriver{c: c, name: info.Driver, g: info.Guarantees}, nil
}

type remoteDriver struct {
	c    *Client
	name string
	g    storage.Guarantees
}

func (d *remoteDriver) Name() string                   { return d.name }
func (d *remoteDriver) Guarantees() storage.Guarantees { return d.g }

func (d *remoteDriver) Create(ctx context.Context, ref storage.Ref, size int64) (string, error) {
	resp, _, err := d.c.call(Request{Op: OpVolCreate, Volume: &ref, Size: size})
	return resp.Path, err
}

func (d *remoteDriver) Delete(ctx context.Context, ref storage.Ref) error {
	_, _, err := d.c.call(Request{Op: OpVolDelete, Volume: &ref})
	return err
}

func (d *remoteDriver) Usage(ctx context.Context, ref storage.Ref) (storage.Usage, error) {
	resp, _, err := d.c.call(Request{Op: OpVolUsage, Volume: &ref})
	if err != nil {
		return storage.Usage{}, err
	}
	if resp.Usage == nil {
		return storage.Usage{}, errors.New("nsd vol_usage: empty")
	}
	return *resp.Usage, nil
}

func (d *remoteDriver) Snapshot(ctx context.Context, ref storage.Ref, name string) (storage.Snapshot, error) {
	return storage.Snapshot{}, storage.ErrSnapshotUnsupported
}
