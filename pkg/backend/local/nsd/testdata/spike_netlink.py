#!/usr/bin/env python3
# spike_netlink.py holder <sock>: open NETLINK_ROUTE and
#   NETLINK_NETFILTER sockets in the current netns (silo-net) and pass
#   them to the first connection on <sock> with SCM_RIGHTS.
# spike_netlink.py silod <sock>: receive them, create bridge br-spike
#   with RTM_NEWLINK and table "inet spike" with an nf_tables batch, and
#   fail unless the kernel acknowledges both.
import os, socket, struct, sys

mode, path = sys.argv[1], sys.argv[2]


def attr(t, data):
    n = 4 + len(data)
    return struct.pack("HH", n, t) + data + b"\0" * (-n % 4)


def msg(t, flags, seq, payload):
    return struct.pack("IHHII", 16 + len(payload), t, flags, seq, 0) + payload


def ack(s):
    while True:
        d = s.recv(65536)
        off = 0
        while off < len(d):
            n, t = struct.unpack_from("IH", d, off)
            if t == 2:  # NLMSG_ERROR
                return struct.unpack_from("i", d, off + 16)[0]
            off += (n + 3) & ~3


if mode == "holder":
    l = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    l.bind(path)
    l.listen(1)
    route = socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, 0)
    nf = socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, 12)
    c, _ = l.accept()
    socket.send_fds(c, [b"x"], [route.fileno(), nf.fileno()])
    c.recv(1)
    sys.exit(0)

c = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
c.connect(path)
_, fds, _, _ = socket.recv_fds(c, 1, 2)
route, nf = socket.socket(fileno=fds[0]), socket.socket(fileno=fds[1])
print("silod: uid", os.getuid(), os.readlink("/proc/self/ns/net"))

REQ, ACK, EXCL, CREATE = 0x1, 0x4, 0x200, 0x400
ifinfo = struct.pack("BxHiII", 0, 0, 0, 0, 0)
route.send(msg(16, REQ | ACK | CREATE | EXCL, 1,  # RTM_NEWLINK
               ifinfo + attr(3, b"br-spike\0") + attr(18, attr(1, b"bridge\0"))))
err = ack(route)
print("RTM_NEWLINK:", err)

NFT = 10  # NFNL_SUBSYS_NFTABLES
gen = lambda fam, res=0: struct.pack("BBH", fam, 0, socket.htons(res))
nf.send(msg(0x10, REQ, 1, gen(0, NFT))  # NFNL_MSG_BATCH_BEGIN
        + msg(NFT << 8, REQ | ACK | CREATE, 2, gen(1) + attr(1, b"spike\0"))  # NEWTABLE inet
        + msg(0x11, REQ, 3, gen(0, NFT)))  # NFNL_MSG_BATCH_END
err2 = ack(nf)
print("NFT_MSG_NEWTABLE:", err2)
c.send(b"x")
sys.exit(1 if err or err2 else 0)
