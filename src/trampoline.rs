//! Trusted static executable inside the workload namespaces, after Bubblewrap hardening.
//! fd 0 is a one-use exec handshake, replaced with /dev/null before image execution.
//! Its new error pipe is CLOEXEC: image code inherits no setup/control descriptor.
use crate::{
    error::Result,
    image::Execution,
    worker::{failed, missing},
};
use std::{
    fs::{File, OpenOptions},
    io::{Read, Write},
    os::{
        fd::{AsRawFd, FromRawFd},
        unix::process::CommandExt,
    },
    process::{Command, Stdio},
};

pub fn entry() -> Result<()> {
    let mut fds = [-1; 2];
    // SAFETY: two writable descriptor slots; error pipe is created only inside the trampoline.
    if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
        return Err(failed("Cannot create image exec error pipe"));
    }
    // SAFETY: pipe2 returned two fresh descriptors, transferred to sole owners.
    let (read, mut error) = unsafe { (File::from_raw_fd(fds[0]), File::from_raw_fd(fds[1])) };
    crate::fd_channel::send(0, read.as_raw_fd())?;
    drop(read);
    let result = execute();
    if let Err(failure) = &result {
        let _ = error.write_all(b"E");
        let _ = serde_json::to_writer(&mut error, failure);
    }
    result
}
fn execute() -> Result<()> {
    let mut bytes = Vec::new();
    std::io::stdin()
        .lock()
        .take(256 * 1024 + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| failed("Cannot read internal exec specification"))?;
    if bytes.len() > 256 * 1024 {
        return Err(failed("Internal exec specification exceeds limit"));
    }
    let spec: Execution = serde_json::from_slice(&bytes)
        .map_err(|_| failed("Invalid internal exec specification"))?;
    if spec.argv.is_empty() {
        return Err(failed("Missing image executable"));
    }
    let null = OpenOptions::new()
        .read(true)
        .open("/dev/null")
        .map_err(|_| failed("Missing sandbox null device"))?;
    // SAFETY: the independent error pipe remains live; fd 0 is replaced by non-control input.
    if unsafe { libc::dup2(null.as_raw_fd(), 0) } < 0 {
        return Err(failed("Cannot replace internal exec input"));
    }
    drop(null);
    // SAFETY: fresh single-threaded trampoline; preserve descriptors only until exec, then close all.
    if unsafe {
        libc::syscall(
            libc::SYS_close_range,
            3_u32,
            u32::MAX,
            libc::CLOSE_RANGE_CLOEXEC,
        )
    } != 0
    {
        return Err(missing(
            "Worker lacks close-on-exec descriptor sanitization",
        ));
    }
    std::env::set_current_dir(&spec.workdir)
        .map_err(|_| failed("Image working directory is unavailable"))?;
    let error = Command::new(&spec.argv[0])
        .args(&spec.argv[1..])
        .env_clear()
        .envs(spec.env)
        .stdin(Stdio::inherit())
        .exec();
    Err(failed(&format!("Image exec failed: {}", error.kind())))
}
/// A dynamic supervisor cannot safely run its trampoline in an arbitrary small OCI image.
pub fn require_static() -> Result<()> {
    let binary =
        std::env::current_exe().map_err(|_| missing("Cannot locate trusted Silo executable"))?;
    let bytes = std::fs::read(binary).map_err(|_| missing("Cannot inspect Silo ELF"))?;
    if bytes.len() < 64 || &bytes[..6] != b"\x7fELF\x02\x01" {
        return Err(missing(
            "Runner requires a static Linux x86-64 Silo executable",
        ));
    }
    let offset = u64::from_le_bytes(bytes[32..40].try_into().expect("ELF offset")) as usize;
    let size = u16::from_le_bytes(bytes[54..56].try_into().expect("ELF size")) as usize;
    let count = u16::from_le_bytes(bytes[56..58].try_into().expect("ELF count")) as usize;
    if size < 56
        || offset
            .checked_add(size.saturating_mul(count))
            .filter(|end| *end <= bytes.len())
            .is_none()
    {
        return Err(missing("Invalid Silo ELF program headers"));
    }
    for index in 0..count {
        let start = offset + index * size;
        let kind = u32::from_le_bytes(bytes[start..start + 4].try_into().expect("ELF header"));
        // Static PIE has PT_DYNAMIC relocation metadata, but no interpreter or DT_NEEDED.
        let mut dependencies = kind == 3;
        if kind == 2 {
            let offset = u64::from_le_bytes(
                bytes[start + 8..start + 16]
                    .try_into()
                    .expect("dynamic offset"),
            ) as usize;
            let length = u64::from_le_bytes(
                bytes[start + 32..start + 40]
                    .try_into()
                    .expect("dynamic size"),
            ) as usize;
            let end = offset
                .checked_add(length)
                .filter(|end| *end <= bytes.len())
                .ok_or_else(|| missing("Invalid Silo ELF dynamic section"))?;
            if length % 16 != 0 {
                return Err(missing("Invalid Silo ELF dynamic entries"));
            }
            for item in bytes[offset..end].chunks_exact(16) {
                let tag = u64::from_le_bytes(item[..8].try_into().expect("dynamic tag"));
                if tag == 0 {
                    break;
                }
                if tag == 1 {
                    dependencies = true;
                }
            }
        }
        if dependencies {
            return Err(missing(
                "Runner requires the static musl Silo executable for its trusted exec trampoline",
            ));
        }
    }
    Ok(())
}
