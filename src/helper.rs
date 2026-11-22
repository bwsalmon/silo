//! Single-threaded trusted namespace boundary. The supervisor never calls unshare/setns.
use crate::{
    error::{Error, Result},
    linux,
    worker::{failed, missing, Tool},
};
use serde::{Deserialize, Serialize};
use serde_json::json;
use std::{
    collections::BTreeMap,
    fs::{self, File},
    io::{BufRead, BufReader, Read, Write},
    net::Shutdown,
    os::unix::net::UnixStream,
    os::{
        fd::{AsRawFd, FromRawFd, OwnedFd},
        unix::process::CommandExt,
    },
    path::PathBuf,
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant},
};

pub const OUTPUT_LIMIT: usize = 16384;
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Spec {
    pub cgroup: PathBuf,
    pub action: Action,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum Action {
    Probe,
    Tool {
        tool: Tool,
        args: Vec<String>,
    },
    Workload {
        bwrap: Tool,
        rootfs: PathBuf,
        argv: Vec<String>,
        env: BTreeMap<String, String>,
        workdir: String,
        scratch_bytes: u64,
        deadline_unix_ms: u64,
        network: crate::relay::Network,
        mounts: Vec<crate::volume::Binding>,
        uid: u32,
        gid: u32,
    },
}
#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "event", rename_all = "snake_case", deny_unknown_fields)]
pub enum Event {
    Ready,
    Exit {
        code: Option<i32>,
        stdout: String,
        stderr: String,
    },
    Error {
        error: Error,
    },
}

pub fn entry() -> Result<serde_json::Value> {
    let bytes = linux::read_bounded(std::io::stdin().lock(), 256 * 1024)?;
    let spec: Spec =
        serde_json::from_slice(&bytes).map_err(|_| failed("Invalid internal helper request"))?;
    // Keep exactly the one private supervisor channel for a workload, CLOEXEC before any spawn.
    let relay_channel = if matches!(spec.action, Action::Workload { .. }) {
        // SAFETY: process::spawn_with_channel installs this one owned descriptor in the fresh helper.
        if unsafe {
            libc::fcntl(
                crate::relay_channel::HELPER_FD,
                libc::F_SETFD,
                libc::FD_CLOEXEC,
            )
        } < 0
        {
            return Err(missing("Missing private relay descriptor"));
        }
        Some(crate::relay_channel::Channel(unsafe {
            OwnedFd::from_raw_fd(crate::relay_channel::HELPER_FD)
        }))
    } else {
        None
    };
    // No other inherited descriptors survive.
    // SAFETY: fresh single-threaded helper; only its explicitly owned relay fd 3 is preserved.
    if unsafe {
        libc::syscall(
            libc::SYS_close_range,
            if relay_channel.is_some() {
                4_u32
            } else {
                3_u32
            },
            u32::MAX,
            0_u32,
        )
    } != 0
    {
        return Err(missing("Worker lacks close_range descriptor sanitization"));
    }
    let parent = unsafe { libc::getppid() };
    // SAFETY: prctl affects only this single-threaded helper.
    if parent <= 1
        || unsafe { libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL) } != 0
        || unsafe { libc::getppid() } != parent
    {
        return Err(missing("Cannot establish helper parent-death protection"));
    }
    fs::write(
        spec.cgroup.join("cgroup.procs"),
        std::process::id().to_string(),
    )
    .map_err(|_| missing("Cannot enter delegated per-job cgroup: require migration permission on target and common-ancestor cgroup.procs"))?;
    // SAFETY: irreversible privilege-gain prevention in this fresh helper, including OCI tools.
    if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
        return Err(missing("Worker lacks no_new_privs"));
    }
    // Reduce inherited file/descriptor/core ceilings for trusted setup tools and workloads.
    limit(libc::RLIMIT_CORE as u64, 0)?;
    limit(libc::RLIMIT_NOFILE as u64, 256)?;
    match spec.action {
        Action::Probe => {
            namespace()?;
            emit(&Event::Ready)?;
        }
        Action::Tool { tool, args } => {
            tool.verify()?;
            let e = Command::new(tool.path)
                .args(args)
                .env_clear()
                .env("PATH", "/usr/bin:/bin")
                .env("HOME", "/nonexistent")
                .env("LANG", "C")
                .stdin(Stdio::null())
                .exec();
            return Err(failed(&format!(
                "Cannot execute pinned OCI tool: {}",
                e.kind()
            )));
        }
        Action::Workload {
            bwrap,
            rootfs,
            argv,
            env,
            workdir,
            scratch_bytes,
            deadline_unix_ms,
            network,
            mounts,
            uid,
            gid,
        } => {
            bwrap.verify()?;
            let remaining = Duration::from_millis(deadline_unix_ms).saturating_sub(
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap_or_default(),
            );
            let deadline = Instant::now() + remaining;
            let mut sources = Vec::new();
            for mount in &mounts {
                use std::os::unix::fs::MetadataExt;
                let source = crate::volume::open_source(&mount.source)?;
                let metadata = source
                    .metadata()
                    .map_err(|_| failed("Cannot inspect opened volume source"))?;
                if metadata.dev() != mount.device || metadata.ino() != mount.inode {
                    return Err(failed("Volume source changed during launch"));
                }
                sources.push(source);
            }
            namespace_as(uid, gid)?; // No threads exist yet.
            relay_channel
                .as_ref()
                .expect("private workload channel")
                .handshake()
                .map_err(|e| {
                    missing(&format!(
                        "Worker cannot transfer SCM_RIGHTS over the private helper socketpair: {}",
                        e.kind()
                    ))
                })?;
            let listeners = network.bind()?;
            let hosts = data_file(network.hosts().as_bytes())?;
            inherit(hosts.as_raw_fd())?;
            // SAFETY: no_new_privs is irreversible and confined to this helper's descendants.
            if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
                return Err(missing("Worker lacks no_new_privs"));
            }
            let (status_read, status_write) = pipe()?;
            let filter = seccomp_file()?;
            inherit(status_write.as_raw_fd())?;
            inherit(filter.as_raw_fd())?;
            // Round each tmpfs down to a page so kernel rounding cannot exceed the byte ceiling.
            let half = scratch_bytes / 2 / 4096 * 4096;
            let other_half = (scratch_bytes - half) / 4096 * 4096;
            let (mut exec_channel, exec_input) =
                UnixStream::pair().map_err(|_| missing("Worker denies private exec socketpair"))?;
            exec_channel
                .set_read_timeout(Some(Duration::from_secs(5)))
                .map_err(|_| missing("Cannot bound exec handshake"))?;
            let executable =
                std::env::current_exe().map_err(|_| failed("Cannot locate trusted trampoline"))?;
            let execution = crate::image::Execution { argv, env, workdir };

            let mut cmd = Command::new(&bwrap.path);
            cmd.env_clear()
                .stdin(Stdio::from(OwnedFd::from(exec_input)))
                .stdout(Stdio::piped())
                .stderr(Stdio::piped())
                .args([
                    "--unshare-user",
                    "--unshare-pid",
                    "--unshare-ipc",
                    "--unshare-uts",
                    "--unshare-cgroup",
                    "--disable-userns",
                    "--assert-userns-disabled",
                    "--cap-drop",
                    "ALL",
                    "--new-session",
                    "--die-with-parent",
                    "--clearenv",
                ])
                .args(["--uid", &uid.to_string(), "--gid", &gid.to_string()])
                .arg("--ro-bind")
                .arg(rootfs)
                .arg("/")
                .args([
                    "--proc",
                    "/proc",
                    "--remount-ro",
                    "/proc",
                    "--dev",
                    "/dev",
                    "--remount-ro",
                    "/dev",
                    "--size",
                ])
                .arg(half.to_string())
                .args(["--tmpfs", "/tmp", "--size"])
                .arg(other_half.to_string())
                .args(["--tmpfs", "/run"])
                .arg("--ro-bind-data")
                .arg(hosts.as_raw_fd().to_string())
                .arg("/etc/hosts")
                .arg("--seccomp")
                .arg(filter.as_raw_fd().to_string())
                .arg("--json-status-fd")
                .arg(status_write.as_raw_fd().to_string())
                .arg("--ro-bind")
                .arg(executable)
                .arg("/run/.silo-exec");
            for (mount, source) in mounts.iter().zip(&sources) {
                inherit(source.as_raw_fd())?;
                cmd.arg(if mount.readonly {
                    "--ro-bind-fd"
                } else {
                    "--bind-fd"
                })
                .arg(source.as_raw_fd().to_string())
                .arg(&mount.target);
            }
            cmd.args(["--chdir", "/", "--", "/run/.silo-exec", "__workload_exec"]);
            let mut child = cmd
                .spawn()
                .map_err(|_| missing("Cannot execute pinned Bubblewrap"))?;
            drop(cmd);
            drop(status_write);
            drop(filter);
            drop(hosts);
            drop(sources);
            serde_json::to_writer(&mut exec_channel, &execution)
                .map_err(|_| failed("Cannot send trusted exec specification"))?;
            exec_channel
                .shutdown(Shutdown::Write)
                .map_err(|_| failed("Cannot finish trusted exec specification"))?;
            // Threads start only after namespace mutation is complete. Workload streams are separate
            // pipes; the helper's protocol stdout and setup descriptors are never workload stdio.
            let out = child.stdout.take().expect("piped stdout");
            let err = child.stderr.take().expect("piped stderr");
            let out = thread::spawn(move || drain(out));
            let err = thread::spawn(move || drain(err));
            let (sender, receiver) = std::sync::mpsc::sync_channel(1);
            thread::spawn(move || {
                let mut line = String::new();
                let mut reader = BufReader::new(File::from(status_read));
                let result = reader.by_ref().take(4097).read_line(&mut line);
                let started = result.is_ok()
                    && line.len() <= 4096
                    && serde_json::from_str::<serde_json::Value>(&line)
                        .ok()
                        .and_then(|v| v.get("child-pid").and_then(|v| v.as_u64()))
                        .is_some();
                let _ = sender.send(started);
                // Bubblewrap writes another record on exit; keep the read end open to avoid SIGPIPE.
                let _ = std::io::copy(&mut reader, &mut std::io::sink());
            });
            if !receiver
                .recv_timeout(Duration::from_secs(5))
                .unwrap_or(false)
            {
                let _ = child.kill();
                let _ = child.wait();
                let diagnostics = err.join().unwrap_or_default();
                return Err(setup_error(
                    "Bubblewrap PID/mount/userns/seccomp/tmpfs setup failed",
                    &diagnostics,
                ));
            }
            let exec_error = match crate::fd_channel::receive(exec_channel.as_raw_fd()) {
                Ok(fd) => fd,
                Err(error) => {
                    let _ = child.kill();
                    let _ = child.wait();
                    let diagnostics = err.join().unwrap_or_default();
                    if source_changed(&diagnostics) {
                        return Err(failed(
                            "Bubblewrap rejected a changed mount source before image execution",
                        ));
                    }
                    return Err(Error::new(
                        error.code,
                        &format!(
                            "{}: {}",
                            error.message,
                            String::from_utf8_lossy(&diagnostics)
                        ),
                    ));
                }
            };
            drop(exec_channel);
            if let Err(error) = crate::fd_channel::await_exec(exec_error) {
                let _ = child.kill();
                let _ = child.wait();
                return Err(error);
            }
            if Instant::now() >= deadline {
                let _ = child.kill();
                let _ = child.wait();
                return Err(failed("Workload lease expired during namespace setup"));
            }
            emit(&Event::Ready)?;
            let status = loop {
                network.service(
                    relay_channel.as_ref().expect("private workload channel"),
                    &listeners,
                )?;
                if let Some(status) = child
                    .try_wait()
                    .map_err(|_| failed("Cannot monitor Bubblewrap"))?
                {
                    break status;
                }
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    break child
                        .wait()
                        .map_err(|_| failed("Cannot reap expired Bubblewrap"))?;
                }
                thread::sleep(Duration::from_millis(2));
            };
            let stdout = String::from_utf8_lossy(&out.join().unwrap_or_default()).into_owned();
            let stderr = String::from_utf8_lossy(&err.join().unwrap_or_default()).into_owned();
            emit(&Event::Exit {
                code: status.code(),
                stdout,
                stderr,
            })?;
        }
    }
    Ok(json!({"helper": "complete"}))
}
pub fn emit(event: &Event) -> Result<()> {
    let mut out = std::io::stdout().lock();
    serde_json::to_writer(&mut out, event).map_err(|_| failed("Helper control channel closed"))?;
    out.write_all(b"\n")
        .and_then(|_| out.flush())
        .map_err(|_| failed("Helper control channel closed"))
}
fn limit(resource: u64, value: u64) -> Result<()> {
    let limit = libc::rlimit {
        rlim_cur: value,
        rlim_max: value,
    };
    // SAFETY: pointer is initialized; applies only to the helper and its descendants.
    if unsafe { libc::setrlimit(resource as _, &limit) } != 0 {
        return Err(missing("Cannot apply helper rlimits"));
    }
    Ok(())
}
fn source_changed(diagnostics: &[u8]) -> bool {
    let text = String::from_utf8_lossy(diagnostics);
    [
        "Race condition binding dirfd",
        "Can't get type of source",
        "Can't find source path",
    ]
    .iter()
    .any(|message| text.contains(message))
}
fn setup_error(context: &str, diagnostics: &[u8]) -> Error {
    if source_changed(diagnostics) {
        failed("Bubblewrap rejected a changed mount source before image execution")
    } else {
        missing(&format!(
            "{context}: {}",
            String::from_utf8_lossy(diagnostics)
        ))
    }
}
fn namespace() -> Result<()> {
    namespace_as(0, 0)
}
fn namespace_as(container_uid: u32, container_gid: u32) -> Result<()> {
    let uid = linux::uid();
    // SAFETY: getgid has no preconditions.
    let gid = unsafe { libc::getgid() };
    // SAFETY: only the new single-threaded helper mutates its namespaces.
    if unsafe { libc::unshare(libc::CLONE_NEWUSER) } != 0 {
        return Err(missing("Worker denies CLONE_NEWUSER"));
    }
    fs::write("/proc/self/setgroups", "deny")
        .map_err(|_| missing("Worker denies setgroups isolation"))?;
    fs::write("/proc/self/uid_map", format!("{container_uid} {uid} 1"))
        .map_err(|_| missing("Worker denies single UID mapping"))?;
    fs::write("/proc/self/gid_map", format!("{container_gid} {gid} 1"))
        .map_err(|_| missing("Worker denies single GID mapping"))?;
    // SAFETY: mappings have just been established in this single-threaded process.
    if unsafe { libc::setresgid(container_gid, container_gid, container_gid) } != 0
        || unsafe { libc::setresuid(container_uid, container_uid, container_uid) } != 0
    {
        return Err(missing("Worker denies mapped helper identity"));
    }
    // SAFETY: network namespace is private; helper remains outside workload PID/mount namespaces.
    if unsafe { libc::unshare(libc::CLONE_NEWNET) } != 0 {
        return Err(missing("Worker denies CLONE_NEWNET"));
    }
    // No netlink, veth, host-network joins, addresses, or default routes.
    // SAFETY: socket returns a new descriptor or -1.
    let fd = unsafe { libc::socket(libc::AF_INET, libc::SOCK_DGRAM | libc::SOCK_CLOEXEC, 0) };
    if fd < 0 {
        return Err(missing("Worker denies loopback interface ioctl socket"));
    }
    // SAFETY: sole ownership of successful socket result.
    let fd = unsafe { OwnedFd::from_raw_fd(fd) };
    // SAFETY: zero is a valid initial representation for ifreq.
    let mut req: libc::ifreq = unsafe { std::mem::zeroed() };
    req.ifr_name[0] = b'l' as libc::c_char;
    req.ifr_name[1] = b'o' as libc::c_char;
    // SAFETY: req has valid interface name and writable union storage.
    if unsafe { libc::ioctl(fd.as_raw_fd(), libc::SIOCGIFFLAGS as _, &mut req) } != 0 {
        return Err(missing("Worker denies SIOCGIFFLAGS for private loopback"));
    }
    // SAFETY: SIOCGIFFLAGS initialized the flags union.
    unsafe { req.ifr_ifru.ifru_flags |= libc::IFF_UP as i16 };
    // SAFETY: req now contains the intended loopback flags.
    if unsafe { libc::ioctl(fd.as_raw_fd(), libc::SIOCSIFFLAGS as _, &req) } != 0 {
        return Err(missing("Worker denies SIOCSIFFLAGS for private loopback"));
    }
    Ok(())
}
fn pipe() -> Result<(OwnedFd, OwnedFd)> {
    let mut fds = [-1; 2];
    // SAFETY: fds points to two writable integers.
    if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
        return Err(failed("Cannot allocate helper pipe"));
    }
    // SAFETY: pipe2 created two distinct descriptors, transferred to sole owners.
    Ok(unsafe { (OwnedFd::from_raw_fd(fds[0]), OwnedFd::from_raw_fd(fds[1])) })
}
fn inherit(fd: i32) -> Result<()> {
    // SAFETY: fd is live and intentionally inherited by trusted Bubblewrap only.
    if unsafe { libc::fcntl(fd, libc::F_SETFD, 0) } < 0 {
        return Err(failed("Cannot pass Bubblewrap setup descriptor"));
    }
    Ok(())
}
pub fn drain(mut input: impl Read) -> Vec<u8> {
    let mut retained = Vec::new();
    let mut buffer = [0; 4096];
    loop {
        match input.read(&mut buffer) {
            Ok(0) | Err(_) => return retained,
            Ok(n) => {
                let keep = n.min(OUTPUT_LIMIT.saturating_sub(retained.len()));
                retained.extend_from_slice(&buffer[..keep]);
            }
        }
    }
}

// x86-64 classic BPF; wrong architectures and x32 are killed. Namespace creation/join,
// mount and kernel-control operations are denied even after a hypothetical capability leak.
fn filter() -> Vec<libc::sock_filter> {
    fn stmt(code: u16, k: u32) -> libc::sock_filter {
        libc::sock_filter {
            code,
            jt: 0,
            jf: 0,
            k,
        }
    }
    fn jump(k: u32, jt: u8, jf: u8) -> libc::sock_filter {
        libc::sock_filter {
            code: 0x15,
            jt,
            jf,
            k,
        }
    }
    const DENY: u32 = 0x00050000 | libc::EPERM as u32;
    let mut f = vec![
        stmt(0x20, 4),
        jump(0xc000003e, 1, 0),
        stmt(0x06, 0x80000000),
        stmt(0x20, 0), // syscall number
        libc::sock_filter {
            code: 0x45,
            jt: 0,
            jf: 1,
            k: 0x40000000,
        },
        stmt(0x06, 0x80000000),
    ];
    // Non-IP families such as AF_VSOCK can bypass IP network namespaces. Only local
    // Unix sockets and IPv4/IPv6 in the private namespace are admitted.
    for nr in [libc::SYS_socket, libc::SYS_socketpair] {
        f.extend([
            jump(nr as u32, 0, 6),
            stmt(0x20, 16),
            jump(libc::AF_UNIX as u32, 3, 0),
            jump(libc::AF_INET as u32, 2, 0),
            jump(libc::AF_INET6 as u32, 1, 0),
            stmt(0x06, DENY),
            stmt(0x06, 0x7fff0000),
        ]);
    }
    // clone3 cannot be inspected by seccomp; ENOSYS permits libc's safe clone fallback.
    f.extend([
        jump(libc::SYS_clone3 as u32, 0, 1),
        stmt(0x06, 0x00050000 | libc::ENOSYS as u32),
    ]);
    for nr in [
        libc::SYS_unshare,
        libc::SYS_setns,
        libc::SYS_mount,
        libc::SYS_umount2,
        libc::SYS_pivot_root,
        libc::SYS_chroot,
        libc::SYS_open_by_handle_at,
        libc::SYS_ptrace,
        libc::SYS_process_vm_readv,
        libc::SYS_process_vm_writev,
        libc::SYS_bpf,
        libc::SYS_perf_event_open,
        libc::SYS_keyctl,
        libc::SYS_add_key,
        libc::SYS_request_key,
        libc::SYS_kexec_load,
        libc::SYS_kexec_file_load,
        libc::SYS_reboot,
        libc::SYS_swapon,
        libc::SYS_swapoff,
        libc::SYS_userfaultfd,
        libc::SYS_io_uring_setup,
        libc::SYS_fsopen,
        libc::SYS_fsmount,
        libc::SYS_move_mount,
        libc::SYS_open_tree,
        libc::SYS_mount_setattr,
    ] {
        f.extend([jump(nr as u32, 0, 1), stmt(0x06, DENY)]);
    }
    let flags = (libc::CLONE_NEWUSER
        | libc::CLONE_NEWNET
        | libc::CLONE_NEWNS
        | libc::CLONE_NEWPID
        | libc::CLONE_NEWIPC
        | libc::CLONE_NEWUTS
        | libc::CLONE_NEWCGROUP) as u32;
    f.extend([
        jump(libc::SYS_clone as u32, 0, 4),
        stmt(0x20, 16), // args[0], clone flags
        libc::sock_filter {
            code: 0x45,
            jt: 0,
            jf: 1,
            k: flags,
        },
        stmt(0x06, DENY),
        stmt(0x06, 0x7fff0000),
        stmt(0x06, 0x7fff0000),
    ]);
    f
}
fn seccomp_file() -> Result<File> {
    // SAFETY: constant NUL-terminated name; successful result is a new owned descriptor.
    let fd = unsafe { libc::memfd_create(c"silo-seccomp".as_ptr(), libc::MFD_CLOEXEC) };
    if fd < 0 {
        return Err(missing("Worker lacks memfd seccomp support"));
    }
    // SAFETY: sole owner of newly created memfd.
    let mut file = unsafe { File::from_raw_fd(fd) };
    let rules = filter();
    // SAFETY: sock_filter consists only of initialized integer fields and has no padding.
    let bytes = unsafe {
        std::slice::from_raw_parts(
            rules.as_ptr().cast::<u8>(),
            std::mem::size_of_val(rules.as_slice()),
        )
    };
    file.write_all(bytes)
        .map_err(|_| failed("Cannot write seccomp filter"))?;
    use std::io::{Seek, SeekFrom};
    file.seek(SeekFrom::Start(0))
        .map_err(|_| failed("Cannot rewind seccomp filter"))?;
    Ok(file)
}

fn data_file(bytes: &[u8]) -> Result<File> {
    // SAFETY: constant name; successful result is a new owned memfd.
    let fd = unsafe { libc::memfd_create(c"silo-hosts".as_ptr(), libc::MFD_CLOEXEC) };
    if fd < 0 {
        return Err(missing("Worker lacks memfd for generated hosts"));
    }
    // SAFETY: sole ownership of the successful syscall result.
    let mut file = unsafe { File::from_raw_fd(fd) };
    file.write_all(bytes)
        .map_err(|_| failed("Cannot write generated hosts"))?;
    use std::io::{Seek, SeekFrom};
    file.seek(SeekFrom::Start(0))
        .map_err(|_| failed("Cannot rewind hosts"))?;
    Ok(file)
}
