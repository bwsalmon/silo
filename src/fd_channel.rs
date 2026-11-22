//! One-use trusted trampoline channel; never exposed to executed image code.
use crate::{
    error::Result,
    worker::{failed, missing},
};
use std::{
    fs::File,
    io::Read,
    os::fd::{AsRawFd, FromRawFd},
    time::{Duration, Instant},
};

pub fn send(socket: i32, descriptor: i32) -> Result<()> {
    let mut byte = b'R';
    let mut iov = libc::iovec {
        iov_base: (&mut byte as *mut u8).cast(),
        iov_len: 1,
    };
    let mut control = [0_u64; 3];
    // SAFETY: zero initializes optional msghdr pointer fields; live buffers are installed below.
    let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
    msg.msg_iov = &mut iov;
    msg.msg_iovlen = 1;
    msg.msg_control = control.as_mut_ptr().cast();
    msg.msg_controllen = std::mem::size_of_val(&control) as _;
    // SAFETY: aligned control buffer has room for exactly one cmsghdr and one descriptor.
    unsafe {
        let header = libc::CMSG_FIRSTHDR(&msg);
        (*header).cmsg_level = libc::SOL_SOCKET;
        (*header).cmsg_type = libc::SCM_RIGHTS;
        (*header).cmsg_len = libc::CMSG_LEN(std::mem::size_of::<i32>() as u32) as _;
        libc::CMSG_DATA(header).cast::<i32>().write(descriptor);
        if libc::sendmsg(socket, &msg, libc::MSG_NOSIGNAL) != 1 {
            return Err(failed("Cannot send trusted exec error descriptor"));
        }
    }
    Ok(())
}
pub fn receive(socket: i32) -> Result<File> {
    let mut byte = 0_u8;
    let mut iov = libc::iovec {
        iov_base: (&mut byte as *mut u8).cast(),
        iov_len: 1,
    };
    let mut control = [0_u64; 3];
    // SAFETY: zero initializes optional msghdr pointer fields; live buffers are installed below.
    let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
    msg.msg_iov = &mut iov;
    msg.msg_iovlen = 1;
    msg.msg_control = control.as_mut_ptr().cast();
    msg.msg_controllen = std::mem::size_of_val(&control) as _;
    // SAFETY: all receive buffers are writable and live; returned descriptor is made CLOEXEC.
    unsafe {
        if libc::recvmsg(socket, &mut msg, libc::MSG_CMSG_CLOEXEC) != 1 {
            return Err(missing(
                "Trusted trampoline did not return an exec descriptor",
            ));
        }
        let header = libc::CMSG_FIRSTHDR(&msg);
        if header.is_null()
            || (*header).cmsg_level != libc::SOL_SOCKET
            || (*header).cmsg_type != libc::SCM_RIGHTS
            || (*header).cmsg_len as u128
                != libc::CMSG_LEN(std::mem::size_of::<i32>() as u32) as u128
        {
            return Err(failed("Invalid trusted exec descriptor message"));
        }
        let descriptor = libc::CMSG_DATA(header).cast::<i32>().read();
        let file = File::from_raw_fd(descriptor);
        if byte != b'R' || msg.msg_flags & (libc::MSG_CTRUNC | libc::MSG_TRUNC) != 0 {
            return Err(failed("Truncated trusted exec descriptor message"));
        }
        Ok(file)
    }
}
pub fn await_exec(mut file: File) -> Result<()> {
    let deadline = Instant::now() + Duration::from_secs(5);
    let mut output = Vec::new();
    loop {
        let mut poll = libc::pollfd {
            fd: file.as_raw_fd(),
            events: libc::POLLIN | libc::POLLHUP,
            revents: 0,
        };
        let remaining = deadline
            .saturating_duration_since(Instant::now())
            .as_millis()
            .min(5000) as i32;
        // SAFETY: poll points to one initialized descriptor structure.
        if remaining == 0 || unsafe { libc::poll(&mut poll, 1, remaining) } <= 0 {
            return Err(failed("Timed out waiting for image exec"));
        }
        let mut bytes = [0; 1024];
        match file.read(&mut bytes) {
            Ok(0) if output.is_empty() => return Ok(()),
            Ok(0) => return Err(failed("Image exec or working-directory setup failed")),
            Ok(n) => {
                output.extend_from_slice(&bytes[..n]);
                if output.len() > 4096 {
                    return Err(failed("Invalid exec error response"));
                }
            }
            Err(_) => return Err(failed("Cannot verify image exec")),
        }
    }
}
