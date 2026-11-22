//! Linux ownership and descriptor boundary. No workload spawning exists in this slice.
use crate::{
    auth::Secret,
    error::{invalid, unauthenticated, unavailable, Code, Error, Result},
    policy::MAX_DOCUMENT,
};
use std::{
    fs::{self, File, OpenOptions},
    io::Read,
    os::{
        fd::{AsRawFd, FromRawFd, OwnedFd},
        unix::fs::{MetadataExt, OpenOptionsExt},
    },
    path::{Path, PathBuf},
};

pub fn uid() -> u32 {
    // SAFETY: getuid has no preconditions.
    unsafe { libc::getuid() }
}
pub fn private_dir(path: &Path) -> Result<PathBuf> {
    let canonical = fs::canonicalize(path).map_err(|_| unavailable())?;
    let m = fs::symlink_metadata(path).map_err(|_| unavailable())?;
    if !path.is_absolute()
        || canonical != path
        || !m.is_dir()
        || m.uid() != uid()
        || m.mode() & 0o077 != 0
    {
        return Err(Error::new(Code::UnsupportedWorker, "Control directory must be an absolute, symlink-free, owner-only directory owned by the supervisor user"));
    }
    for parent in path.ancestors().skip(1) {
        let m = fs::symlink_metadata(parent).map_err(|_| unavailable())?;
        if !m.is_dir()
            || (m.uid() != 0 && m.uid() != uid())
            || (m.mode() & 0o022 != 0 && m.mode() & 0o1000 == 0)
        {
            return Err(Error::new(
                Code::UnsupportedWorker,
                "Control directory ancestors must have trusted owners and prevent untrusted rename",
            ));
        }
    }
    Ok(canonical)
}
pub fn create(path: &Path) -> Result<File> {
    OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
        .map_err(|_| unavailable())
}
pub fn read_private(path: &Path, limit: usize) -> Result<Vec<u8>> {
    let f = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
        .map_err(|_| unavailable())?;
    let m = f.metadata().map_err(|_| unavailable())?;
    if !m.is_file() || m.uid() != uid() || m.mode() & 0o077 != 0 || m.len() > limit as u64 {
        return Err(unavailable());
    }
    read_bounded(f, limit)
}
pub fn read_bounded(reader: impl Read, limit: usize) -> Result<Vec<u8>> {
    let mut bytes = Vec::new();
    reader
        .take(limit as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| invalid())?;
    if bytes.len() > limit {
        return Err(invalid());
    }
    Ok(bytes)
}
pub fn lock(root: &Path) -> Result<File> {
    let f = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(root.join("lock"))
        .map_err(|_| unavailable())?;
    let m = f.metadata().map_err(|_| unavailable())?;
    if !m.is_file() || m.uid() != uid() || m.mode() & 0o077 != 0 {
        return Err(unavailable());
    }
    // SAFETY: the live file owns the descriptor; flock does not take ownership.
    if unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
        return Err(unavailable());
    }
    Ok(f)
}
pub fn take_fd(fd: i32) -> Result<File> {
    if fd < 3 {
        return Err(invalid());
    }
    // SAFETY: fcntl validates the descriptor before ownership is taken; CLI is single threaded.
    if unsafe { libc::fcntl(fd, libc::F_GETFD) } < 0 {
        return Err(invalid());
    }
    // SAFETY: sole ownership is transferred by the trusted launcher contract; no other code owns fd.
    let owned = unsafe { OwnedFd::from_raw_fd(fd) };
    // SAFETY: the owned descriptor remains live, and the flag prevents future exec inheritance.
    if unsafe { libc::fcntl(owned.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
        return Err(invalid());
    }
    Ok(File::from(owned))
}
pub fn credential(fd: i32) -> Result<Secret> {
    let mut bytes = zeroize::Zeroizing::new(Vec::new());
    {
        let f = take_fd(fd)?;
        f.take(66)
            .read_to_end(&mut bytes)
            .map_err(|_| unauthenticated())?;
    } // Credential descriptor closed before any request or future child launch.
    if bytes.len() > 65 {
        return Err(unauthenticated());
    }
    if bytes.last() == Some(&b'\n') {
        bytes.pop();
    }
    if !bytes.is_ascii() {
        return Err(unauthenticated());
    }
    let token =
        Secret(String::from_utf8(std::mem::take(&mut *bytes)).map_err(|_| unauthenticated())?);
    if !token.valid() {
        return Err(unauthenticated());
    }
    Ok(token)
}
pub fn operator_manifest(path: &Path) -> Result<Vec<u8>> {
    // This path is used locally by validate/up only and never sent to the supervisor.
    let file = File::open(path).map_err(|_| crate::error::manifest())?;
    read_bounded(file, MAX_DOCUMENT).map_err(|_| crate::error::manifest())
}
