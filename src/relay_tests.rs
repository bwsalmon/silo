use super::*;
use crate::relay_channel::{Message, ACCEPT, CONNECTED};
use std::os::fd::{AsRawFd, FromRawFd};

fn pair() -> (TcpStream, TcpStream) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let a = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let (b, _) = listener.accept().unwrap();
    for s in [&a, &b] {
        s.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        s.set_write_timeout(Some(Duration::from_secs(2))).unwrap();
    }
    (a, b)
}
fn message(kind: u32, index: u32, token: u64, stream: TcpStream) -> Message {
    Message {
        kind,
        index,
        token,
        fd: Some(stream.into()),
    }
}
fn state(limit: usize) -> (State, Channel) {
    let (src, _src_peer) = Channel::pair().unwrap();
    let (dst, peer) = Channel::pair().unwrap();
    let mut s = State::default();
    s.endpoints.insert(
        "source".into(),
        Endpoint {
            channel: src,
            routes: vec![Route {
                destination: "destination".into(),
                listener: 2,
            }],
            limit,
            deadline: u64::MAX,
            probed: true,
        },
    );
    s.endpoints.insert(
        "destination".into(),
        Endpoint {
            channel: dst,
            routes: vec![],
            limit,
            deadline: u64::MAX,
            probed: true,
        },
    );
    (s, peer)
}
#[test]
fn tcp_descriptors_copy_both_ways_and_preserve_half_close() {
    let (mut s, helper) = state(2);
    let (mut client, accepted) = pair();
    s.message("source", message(ACCEPT, 0, 0, accepted))
        .unwrap();
    let request = helper.receive().unwrap().unwrap();
    assert_eq!((request.kind, request.index), (CONNECT, 2));
    let (destination, mut server) = pair();
    s.message(
        "destination",
        message(CONNECTED, 2, request.token, destination),
    )
    .unwrap();
    client.write_all(b"request").unwrap();
    client.shutdown(Shutdown::Write).unwrap();
    for _ in 0..10 {
        s.streams.retain(|_, stream| stream.advance());
    }
    let mut request_bytes = Vec::new();
    server.read_to_end(&mut request_bytes).unwrap();
    assert_eq!(request_bytes, b"request");
    // Reply after EOF must still travel back on the existing directional link.
    server.write_all(b"reply after EOF").unwrap();
    server.shutdown(Shutdown::Write).unwrap();
    for _ in 0..10 {
        s.streams.retain(|_, stream| stream.advance());
    }
    let mut reply = Vec::new();
    client.read_to_end(&mut reply).unwrap();
    assert_eq!(reply, b"reply after EOF");
    assert!(s.streams.is_empty());
}
#[test]
fn private_channel_authenticates_source_routes_and_destination_responses() {
    let (mut s, helper) = state(2);
    let (_, accepted) = pair();
    assert!(s
        .message("source", message(ACCEPT, 77, 0, accepted))
        .is_err());
    let (_, accepted) = pair();
    s.message("source", message(ACCEPT, 0, 0, accepted))
        .unwrap();
    let request = helper.receive().unwrap().unwrap();
    let (a, _) = pair();
    assert!(s
        .message("source", message(CONNECTED, 2, request.token, a))
        .is_err());
    let (a, _) = pair();
    assert!(s
        .message("destination", message(CONNECTED, 99, request.token, a))
        .is_err());
    let file = std::fs::File::open("/dev/null").unwrap();
    assert!(s
        .message(
            "destination",
            Message {
                kind: CONNECTED,
                index: 2,
                token: request.token,
                fd: Some(file.into())
            }
        )
        .is_err());
}
#[test]
fn quotas_setup_failure_deadlines_and_revocation_release_sockets() {
    let (mut s, helper) = state(1);
    let (mut client, a) = pair();
    s.message("source", message(ACCEPT, 0, 0, a)).unwrap();
    let request = helper.receive().unwrap().unwrap();
    let (_, a) = pair();
    s.message("source", message(ACCEPT, 0, 0, a)).unwrap();
    assert_eq!(s.stats["source"].rejected, 1);
    assert_eq!(s.streams.len(), 1);
    s.message(
        "destination",
        Message {
            kind: FAILED,
            index: 2,
            token: request.token,
            fd: None,
        },
    )
    .unwrap();
    s.count();
    assert_eq!(s.stats["source"].active, 0);
    assert_eq!(client.read(&mut [0]).unwrap(), 0);

    for expired in ["setup", "idle", "lifetime", "revocation"] {
        let (mut client, a) = pair();
        s.message("source", message(ACCEPT, 0, 0, a)).unwrap();
        let request = helper.receive().unwrap().unwrap();
        if expired != "setup" {
            let (a, _server) = pair();
            s.message("destination", message(CONNECTED, 2, request.token, a))
                .unwrap();
        }
        let stream = s.streams.get_mut(&request.token).unwrap();
        match expired {
            "setup" => stream.born = Instant::now() - SETUP,
            "idle" => stream.progress = Instant::now() - IDLE,
            "lifetime" => stream.born = Instant::now() - LIFETIME,
            _ => (),
        }
        if expired == "revocation" {
            s.remove("source");
        } else {
            s.streams.retain(|_, stream| stream.advance());
        }
        assert!(s.streams.is_empty());
        s.count();
        assert_eq!(client.read(&mut [0]).unwrap(), 0);
    }
}
#[test]
fn malformed_rights_packets_close_every_delivered_descriptor() {
    for (payload, count) in [(0, 1_usize), (1, 1), (1, 2), (1, 100)] {
        let (sender, receiver) = Channel::pair().unwrap();
        let mut pipes = [-1; 2];
        // SAFETY: two writable descriptor slots; sole ownership below.
        assert_eq!(
            unsafe { libc::pipe2(pipes.as_mut_ptr(), libc::O_CLOEXEC) },
            0
        );
        let (mut reader, writer) = unsafe {
            (
                std::fs::File::from_raw_fd(pipes[0]),
                std::fs::File::from_raw_fd(pipes[1]),
            )
        };
        // Send malformed short packet with multiple SCM_RIGHTS. Truncated ancillary
        // data must close all delivered FDs; the kernel closes the undisclosed remainder.
        let mut bytes = [0_u8; 1];
        let mut iov = libc::iovec {
            iov_base: bytes.as_mut_ptr().cast(),
            iov_len: payload,
        };
        let mut control = vec![0_u64; 2 + count.div_ceil(2)];
        let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = control.as_mut_ptr().cast();
        msg.msg_controllen = unsafe { libc::CMSG_SPACE((count * 4) as u32) } as _;
        // SAFETY: correctly sized/aligned header; all descriptor integers are live.
        unsafe {
            let h = libc::CMSG_FIRSTHDR(&msg);
            (*h).cmsg_level = libc::SOL_SOCKET;
            (*h).cmsg_type = libc::SCM_RIGHTS;
            (*h).cmsg_len = libc::CMSG_LEN((count * 4) as u32) as _;
            for i in 0..count {
                libc::CMSG_DATA(h)
                    .cast::<i32>()
                    .add(i)
                    .write(writer.as_raw_fd());
            }
            assert_eq!(
                libc::sendmsg(sender.0.as_raw_fd(), &msg, libc::MSG_NOSIGNAL),
                payload as isize
            );
        }
        drop(writer);
        assert!(receiver.receive().is_err());
        // A leaked duplicate must fail promptly, rather than hang the test reader.
        let mut ready = libc::pollfd {
            fd: reader.as_raw_fd(),
            events: libc::POLLIN | libc::POLLHUP,
            revents: 0,
        };
        // SAFETY: one live pipe and one initialized pollfd.
        assert!(
            unsafe { libc::poll(&mut ready, 1, 1000) } > 0,
            "rejected rights descriptor leaked"
        );
        assert_eq!(reader.read(&mut [0]).unwrap(), 0);
    }
}
#[test]
fn descriptor_transfer_is_cloexec_and_backlog_is_bounded() {
    let (sender, receiver) = Channel::pair().unwrap();
    let (_, a) = pair();
    sender.send(ACCEPT, 0, 0, Some(a.as_raw_fd())).unwrap();
    let msg = receiver.receive().unwrap().unwrap();
    // SAFETY: query flags on the received live descriptor.
    assert_ne!(
        unsafe { libc::fcntl(msg.fd.unwrap().as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
        0
    );
    let mut sent = 0;
    while sender.send(ACCEPT, 0, 0, Some(a.as_raw_fd())).is_ok() {
        sent += 1;
        assert!(sent < 128, "unbounded private setup queue");
    }
    assert!(sent > 0);
}
#[test]
fn static_aliases_are_distinct_and_workload_wildcard_conflicts_fail() {
    let mut m =
        crate::policy::Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap();
    let db = m.services["db"].clone();
    m.services.insert("other".into(), db);
    let mut link = m.network.allow[0].clone();
    link.to = "other".into();
    m.network.allow.push(link);
    let ids = [
        ("db".into(), "db-instance".into()),
        ("other".into(), "other-instance".into()),
    ]
    .into_iter()
    .collect();
    let (network, routes) = layout(&m, "app", &ids).unwrap();
    assert_eq!(network.bindings.len(), 2);
    assert_ne!(network.bindings[0].address, network.bindings[1].address);
    assert_ne!(routes[0].destination, routes[1].destination);
    assert!(layout(&m, "unrelated", &ids).unwrap().0.bindings.is_empty());
    m.network.allow[0].from = "other".into();
    assert_eq!(
        layout(&m, "other", &ids).err().unwrap().code,
        Code::PortConflict
    );
}

#[test]
fn aggregate_socket_and_buffer_ceiling_is_independent_of_instance_limits() {
    let (mut s, helper) = state(MAX_STREAMS + 2);
    let mut clients = Vec::new();
    let mut servers = Vec::new();
    for _ in 0..MAX_STREAMS {
        let (client, a) = pair();
        s.message("source", message(ACCEPT, 0, 0, a)).unwrap();
        let request = helper.receive().unwrap().unwrap();
        let (a, server) = pair();
        s.message("destination", message(CONNECTED, 2, request.token, a))
            .unwrap();
        clients.push(client);
        servers.push(server);
    }
    let (_, a) = pair();
    s.message("source", message(ACCEPT, 0, 0, a)).unwrap();
    assert_eq!(s.streams.len(), MAX_STREAMS);
    assert_eq!(s.stats["source"].peak, MAX_STREAMS);
    assert_eq!(s.stats["source"].rejected, 1);
    assert_eq!(
        s.streams
            .values()
            .map(|s| s.ab.bytes.len() + s.ba.bytes.len())
            .sum::<usize>(),
        MAX_STREAMS * 2 * BUFFER
    );
    s.remove("destination");
    for stream in clients.iter_mut().chain(servers.iter_mut()) {
        assert_eq!(stream.read(&mut [0]).unwrap(), 0);
    }
}

#[test]
fn helpers_must_complete_the_rights_probe_before_requesting_routes() {
    use crate::relay_channel::{ACK, PROBE};
    let (mut s, _) = state(1);
    s.endpoints.get_mut("source").unwrap().probed = false;
    let (_, accepted) = pair();
    assert!(s
        .message("source", message(ACCEPT, 0, 0, accepted))
        .is_err());
    let (channel, peer) = Channel::pair().unwrap();
    s.endpoints.get_mut("source").unwrap().channel = channel;
    let file = std::fs::File::open("/dev/null").unwrap();
    s.message(
        "source",
        Message {
            kind: PROBE,
            index: 0,
            token: 0,
            fd: Some(file.into()),
        },
    )
    .unwrap();
    assert_eq!(peer.receive().unwrap().unwrap().kind, ACK);
    assert!(s.endpoints["source"].probed);
    let file = std::fs::File::open("/dev/null").unwrap();
    assert!(s
        .message(
            "source",
            Message {
                kind: PROBE,
                index: 0,
                token: 0,
                fd: Some(file.into())
            }
        )
        .is_err());
}
