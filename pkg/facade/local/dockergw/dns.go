package dockergw

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/bwsalmon/silo/pkg/facade/local/docker"
	"github.com/bwsalmon/silo/pkg/store"
)

// Lookuper is what the DNS asks: the facade's Lookup.
type Lookuper interface {
	Lookup(ctx context.Context, tx store.Tx, names docker.Names, silo string, from netip.Addr, name string) (docker.Answer, error)
}

// DNS is the gateway's DNS, at silo.internal's address, port 53, in
// silo-net (local-backend.md, "DNS"). It answers A queries through the
// facade's Lookup, as the silo the query's source address belongs to,
// and NXDOMAIN for every name Lookup doesn't know. A query of another
// type for a name Lookup knows is answered with no records rather than
// NXDOMAIN, so that a resolver that asks for A and AAAA together (musl
// treats an NXDOMAIN on either as the name's) still finds the A.
type DNS struct {
	Store  store.Store
	Facade Lookuper
	Names  docker.Names
	Origin Origin
}

// TTL is the answers' time to live: short, since a container's address
// goes with it.
const TTL = 5

// Answer answers one query, from the address from, in DNS's wire
// format. A message that isn't a query gets no answer (nil).
func (d *DNS) Answer(ctx context.Context, from netip.Addr, msg []byte) ([]byte, error) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || h.Response {
		return nil, err
	}
	resp := dnsmessage.Message{Header: dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true,
		RecursionDesired: h.RecursionDesired, OpCode: h.OpCode}}
	q, err := p.Question()
	if err != nil {
		resp.RCode = dnsmessage.RCodeFormatError
		return resp.Pack()
	}
	resp.Questions = []dnsmessage.Question{q}
	if h.OpCode != 0 || q.Class != dnsmessage.ClassINET {
		resp.RCode = dnsmessage.RCodeNotImplemented
		return resp.Pack()
	}
	silo, ok := d.Origin(from)
	if !ok {
		resp.RCode = dnsmessage.RCodeRefused
		return resp.Pack()
	}
	var a docker.Answer
	err = d.Store.Tx(ctx, func(tx store.Tx) error {
		var err error
		a, err = d.Facade.Lookup(ctx, tx, d.Names, silo, from, q.Name.String())
		return err
	})
	if err != nil {
		resp.RCode = dnsmessage.RCodeServerFailure
		return resp.Pack()
	}
	switch {
	case len(a.Addrs) == 0:
		resp.RCode = dnsmessage.RCodeNameError
	case q.Type == dnsmessage.TypeA:
		for _, addr := range a.Addrs {
			if !addr.Is4() {
				continue
			}
			resp.Answers = append(resp.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: TTL},
				Body:   &dnsmessage.AResource{A: addr.As4()}})
		}
	}
	return resp.Pack()
}

// maxUDP is the largest answer sent over UDP without EDNS0; a larger
// one is truncated, and the client asks again over TCP.
const maxUDP = 512

// ServeUDP answers queries on pc until it is closed.
func (d *DNS) ServeUDP(ctx context.Context, pc net.PacketConn) error {
	buf := make([]byte, 65535)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		from, ok := addrOf(addr)
		if !ok {
			continue
		}
		b, err := d.Answer(ctx, from, buf[:n])
		if err != nil || b == nil {
			continue
		}
		if len(b) > maxUDP {
			b = truncate(b)
		}
		pc.WriteTo(b, addr)
	}
}

// truncate is an answer cut to its header and question, with TC set.
func truncate(b []byte) []byte {
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		return nil
	}
	m.Answers, m.Authorities, m.Additionals = nil, nil, nil
	m.Truncated = true
	out, _ := m.Pack()
	return out
}

// ServeTCP answers queries on l, one connection each, until it is
// closed.
func (d *DNS) ServeTCP(ctx context.Context, l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go d.serveConn(ctx, c)
	}
}

func (d *DNS) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	from, ok := addrOf(c.RemoteAddr())
	if !ok {
		return
	}
	for {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		var n uint16
		if err := binary.Read(c, binary.BigEndian, &n); err != nil {
			return
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(c, msg); err != nil {
			return
		}
		b, err := d.Answer(ctx, from, msg)
		if err != nil || b == nil {
			return
		}
		out := binary.BigEndian.AppendUint16(nil, uint16(len(b)))
		if _, err := c.Write(append(out, b...)); err != nil {
			return
		}
	}
}

// addrOf is a socket address's IP.
func addrOf(a net.Addr) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}
