//! Private SOCK_SEQPACKET setup channel. It never carries application bytes.
use std::{
    io,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
};
pub const HELPER_FD: i32 = 3;
pub const ACCEPT: u32 = 1;
pub const CONNECT: u32 = 2;
pub const CONNECTED: u32 = 3;
pub const FAILED: u32 = 4;
pub const PROBE: u32 = 5;
pub const ACK: u32 = 6;
pub struct Channel(pub OwnedFd);
pub struct Message {
    pub kind: u32,
    pub index: u32,
    pub token: u64,
    pub fd: Option<OwnedFd>,
}
fn invalid() -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, "Invalid private relay message")
}
impl Channel {
    pub fn pair() -> io::Result<(Self, Self)> {
        let mut fds = [-1; 2];
        // SAFETY: writable pair; successful descriptors have separate ownership.
        if unsafe {
            libc::socketpair(
                libc::AF_UNIX,
                libc::SOCK_SEQPACKET | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
                fds.as_mut_ptr(),
            )
        } != 0
        {
            return Err(io::Error::last_os_error());
        }
        let pair = unsafe {
            (
                Self(OwnedFd::from_raw_fd(fds[0])),
                Self(OwnedFd::from_raw_fd(fds[1])),
            )
        };
        for c in [&pair.0, &pair.1] {
            for option in [libc::SO_SNDBUF, libc::SO_RCVBUF] {
                let size: i32 = 4096;
                // SAFETY: live socket and initialized integer option.
                if unsafe {
                    libc::setsockopt(
                        c.0.as_raw_fd(),
                        libc::SOL_SOCKET,
                        option,
                        (&size as *const i32).cast(),
                        4,
                    )
                } != 0
                {
                    return Err(io::Error::last_os_error());
                }
            }
        }
        Ok(pair)
    }
    pub fn handshake(&self) -> io::Result<()> {
        // Prove cross-namespace SCM_RIGHTS in both setup participants before workload exec.
        // The duplicate is discarded by the supervisor and never exposed to the image.
        self.send(PROBE, 0, 0, Some(self.0.as_raw_fd()))?;
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
        loop {
            if let Some(m) = self.receive()? {
                return if m.kind == ACK && m.index == 0 && m.token == 0 && m.fd.is_none() {
                    Ok(())
                } else {
                    Err(invalid())
                };
            }
            if std::time::Instant::now() >= deadline {
                return Err(io::ErrorKind::TimedOut.into());
            }
            std::thread::sleep(std::time::Duration::from_millis(2));
        }
    }
    pub fn send(&self, kind: u32, index: u32, token: u64, fd: Option<i32>) -> io::Result<()> {
        let mut bytes = [0_u8; 16];
        bytes[..4].copy_from_slice(&kind.to_le_bytes());
        bytes[4..8].copy_from_slice(&index.to_le_bytes());
        bytes[8..].copy_from_slice(&token.to_le_bytes());
        let mut iov = libc::iovec {
            iov_base: bytes.as_mut_ptr().cast(),
            iov_len: bytes.len(),
        };
        let mut control = [0_u64; 3];
        // SAFETY: zero initializes optional fields; buffers remain live through sendmsg.
        let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        if let Some(fd) = fd {
            msg.msg_control = control.as_mut_ptr().cast();
            msg.msg_controllen = std::mem::size_of_val(&control) as _;
            // SAFETY: aligned storage has room for one rights header and descriptor.
            unsafe {
                let h = libc::CMSG_FIRSTHDR(&msg);
                (*h).cmsg_level = libc::SOL_SOCKET;
                (*h).cmsg_type = libc::SCM_RIGHTS;
                (*h).cmsg_len = libc::CMSG_LEN(4) as _;
                libc::CMSG_DATA(h).cast::<i32>().write(fd);
            }
        }
        // SAFETY: initialized message contains live buffers; no signal on closed peer.
        if unsafe {
            libc::sendmsg(
                self.0.as_raw_fd(),
                &msg,
                libc::MSG_NOSIGNAL | libc::MSG_DONTWAIT,
            )
        } != 16
        {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }
    pub fn receive(&self) -> io::Result<Option<Message>> {
        let mut bytes = [0_u8; 16];
        let mut iov = libc::iovec {
            iov_base: bytes.as_mut_ptr().cast(),
            iov_len: bytes.len(),
        };
        // Larger than the valid message so every delivered rights descriptor can be closed on reject.
        let mut control = [0_u64; 40];
        // SAFETY: zero initializes optional fields; all receive buffers are writable.
        let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = control.as_mut_ptr().cast();
        msg.msg_controllen = std::mem::size_of_val(&control) as _;
        let n = unsafe {
            libc::recvmsg(
                self.0.as_raw_fd(),
                &mut msg,
                libc::MSG_DONTWAIT | libc::MSG_CMSG_CLOEXEC,
            )
        };
        if n < 0 {
            let e = io::Error::last_os_error();
            return if e.kind() == io::ErrorKind::WouldBlock {
                Ok(None)
            } else {
                Err(e)
            };
        }
        let mut descriptors = Vec::new();
        let mut bad = n != 16 || msg.msg_flags & (libc::MSG_TRUNC | libc::MSG_CTRUNC) != 0;
        // SAFETY: the kernel initialized headers inside the aligned receive buffer.
        unsafe {
            let mut h = libc::CMSG_FIRSTHDR(&msg);
            while !h.is_null() {
                if (*h).cmsg_level == libc::SOL_SOCKET && (*h).cmsg_type == libc::SCM_RIGHTS {
                    let len = ((*h).cmsg_len as usize).saturating_sub(libc::CMSG_LEN(0) as usize);
                    for i in 0..len / 4 {
                        descriptors.push(OwnedFd::from_raw_fd(
                            libc::CMSG_DATA(h).cast::<i32>().add(i).read(),
                        ));
                    }
                } else {
                    bad = true;
                }
                h = libc::CMSG_NXTHDR(&msg, h);
            }
        }
        if n == 0 && descriptors.is_empty() && msg.msg_flags & libc::MSG_CTRUNC == 0 {
            return Err(io::ErrorKind::UnexpectedEof.into());
        }
        let kind = u32::from_le_bytes(bytes[..4].try_into().unwrap());
        let needs_fd = matches!(kind, ACCEPT | CONNECTED | PROBE);
        if bad
            || !matches!(kind, ACCEPT | CONNECT | CONNECTED | FAILED | PROBE | ACK)
            || descriptors.len() != usize::from(needs_fd)
        {
            return Err(invalid());
        }
        Ok(Some(Message {
            kind,
            index: u32::from_le_bytes(bytes[4..8].try_into().unwrap()),
            token: u64::from_le_bytes(bytes[8..].try_into().unwrap()),
            fd: descriptors.pop(),
        }))
    }
}
