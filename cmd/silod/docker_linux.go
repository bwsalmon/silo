package main

import (
	"context"
	"net"
	"net/netip"

	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
	"github.com/bwsalmon/silo/pkg/facade/local/dockergw"
)

// holderNet is silo-net through silo-nsd's RPC on socket: listeners
// it opens there, its dial pool, and its wiring of containers.
type holderNet struct {
	*dockergw.Wiring
	c    *nsd.Client
	pool *nsd.Pool
}

func openSiloNet(socket string) (siloNet, error) {
	c, err := nsd.Dial(socket)
	if err != nil {
		return nil, err
	}
	return &holderNet{Wiring: &dockergw.Wiring{Holder: c}, c: c, pool: nsd.NewPool(c, "tcp4", 8)}, nil
}

func (h *holderNet) Listen(network, addr string) (net.Listener, error) {
	return h.c.Listen(network, addr)
}

func (h *holderNet) ListenPacket(network, addr string) (net.PacketConn, error) {
	return h.c.ListenPacket(network, addr)
}

func (h *holderNet) Dial(ctx context.Context, local, target netip.AddrPort) (net.Conn, error) {
	return h.pool.Dial(ctx, local, target)
}

func (h *holderNet) Close() error {
	h.pool.Close()
	return h.c.Close()
}
