//! Subprocess transport: only the helper mutates namespaces or enters workload cgroups.
use crate::{
    error::{Code, Error, Result},
    helper::{Action, Event, Spec},
    worker::{failed, Group, Tool},
};
use std::{
    io::{BufRead, BufReader, Read, Write},
    process::{Child, Command, Stdio},
    sync::mpsc::{self, Receiver},
    thread,
    time::{Duration, Instant},
};
pub fn spawn(spec: &Spec) -> Result<Child> {
    spawn_with_channel(spec, None)
}
pub fn spawn_with_channel(
    spec: &Spec,
    channel: Option<&crate::relay_channel::Channel>,
) -> Result<Child> {
    use std::os::{fd::AsRawFd, unix::process::CommandExt};
    let exe =
        std::env::current_exe().map_err(|_| failed("Cannot locate trusted Silo executable"))?;
    crate::worker::trusted_path(&exe, false)?;
    let mut command = Command::new(exe);
    let supervisor_pid = std::process::id() as libc::pid_t;
    // Establish this before exec/request parsing: adoption by a subreaper must never
    // make a delayed helper accept a replacement parent after supervisor loss.
    // SAFETY: protect_parent uses only async-signal-safe operations in the fork child.
    unsafe {
        command.pre_exec(move || protect_parent(supervisor_pid));
    }
    if let Some(channel) = channel {
        let fd = channel.0.as_raw_fd();
        // SAFETY: pre_exec uses only async-signal-safe descriptor operations in the fork child.
        unsafe {
            command.pre_exec(move || {
                if libc::dup2(fd, crate::relay_channel::HELPER_FD) < 0
                    || libc::fcntl(crate::relay_channel::HELPER_FD, libc::F_SETFD, 0) < 0
                {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
    }
    let mut child = command
        .arg("__namespace_helper")
        .env_clear()
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|_| failed("Cannot spawn trusted namespace helper"))?;
    let write =
        serde_json::to_writer(child.stdin.as_mut().expect("helper stdin"), spec).and_then(|_| {
            child
                .stdin
                .as_mut()
                .expect("helper stdin")
                .flush()
                .map_err(serde_json::Error::io)
        });
    drop(child.stdin.take()); // EOF is part of the bounded internal protocol.
    if write.is_err() {
        let _ = child.kill();
        let _ = child.wait();
        return Err(failed("Cannot send namespace helper request"));
    }
    Ok(child)
}
fn protect_parent(expected: libc::pid_t) -> std::io::Result<()> {
    // SAFETY: these calls affect/read only the current process and allocate nothing.
    if unsafe { libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL) } != 0 {
        return Err(std::io::Error::last_os_error());
    }
    if unsafe { libc::getppid() } != expected {
        return Err(std::io::Error::from_raw_os_error(libc::ECHILD));
    }
    Ok(())
}
pub fn events(child: &mut Child) -> Receiver<Event> {
    let output = child.stdout.take().expect("helper stdout");
    let (sender, receiver) = mpsc::sync_channel(4);
    thread::spawn(move || {
        let mut reader = BufReader::new(output);
        loop {
            let mut line = String::new();
            let count = reader.by_ref().take(256 * 1024 + 1).read_line(&mut line);
            if !matches!(count, Ok(1..)) || line.len() > 256 * 1024 {
                break;
            }
            match serde_json::from_str(&line) {
                Ok(event) => {
                    if sender.send(event).is_err() {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
    });
    receiver
}
pub fn cleanup(group: &Group, child: &mut Child) -> Result<()> {
    group.kill()?;
    // kill also covers a helper which has not entered its cgroup yet.
    let _ = child.kill();
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        if child
            .try_wait()
            .map_err(|_| Error::new(Code::CleanupUnverified, "Cannot reap helper"))?
            .is_some()
            && group.empty()?
        {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(Error::new(
                Code::CleanupUnverified,
                "Workload cgroup teardown could not be verified",
            ));
        }
        thread::sleep(Duration::from_millis(10));
    }
}
pub struct Context {
    pub group: Group,
    pub child: Option<Child>,
}
impl Context {
    pub fn cleanup(&mut self) -> Result<()> {
        if let Some(child) = &mut self.child {
            cleanup(&self.group, child)?;
            self.child = None;
        } else {
            self.group.kill()?;
            if !self.group.empty()? {
                return Err(Error::new(
                    Code::CleanupUnverified,
                    "Setup cgroup remains populated",
                ));
            }
        }
        Ok(())
    }
    pub fn probe(&mut self) -> Result<()> {
        self.child = Some(spawn(&Spec {
            cgroup: self.group.path.clone(),
            action: Action::Probe,
        })?);
        let receiver = events(self.child.as_mut().expect("probe child"));
        let result = match receiver.recv_timeout(Duration::from_secs(5)) {
            Ok(Event::Ready) => Ok(()),
            Ok(Event::Error { error }) => Err(error),
            _ => Err(crate::worker::missing(
                "Namespace/loopback helper probe did not complete",
            )),
        };
        self.cleanup()?;
        result
    }
    pub fn tool(&mut self, tool: &Tool, args: &[String]) -> Result<Vec<u8>> {
        self.child = Some(spawn(&Spec {
            cgroup: self.group.path.clone(),
            action: Action::Tool {
                tool: tool.clone(),
                args: args.to_vec(),
            },
        })?);
        let child = self.child.as_mut().expect("tool child");
        let output = child.stdout.take().expect("tool stdout");
        let reader = thread::spawn(move || bounded_drain(output, 1024 * 1024));
        let deadline = Instant::now() + Duration::from_secs(60);
        let status = loop {
            if let Some(status) = child
                .try_wait()
                .map_err(|_| failed("Cannot monitor OCI tool"))?
            {
                break Some(status);
            }
            if Instant::now() >= deadline {
                break None;
            }
            thread::sleep(Duration::from_millis(10));
        };
        self.cleanup()?;
        let bytes = reader
            .join()
            .map_err(|_| failed("OCI tool output reader failed"))?;
        if !status.map(|s| s.success()).unwrap_or(false) {
            return Err(failed(
                "Pinned OCI tool failed or exceeded its 60 second setup deadline",
            ));
        }
        if bytes.len() > 1024 * 1024 {
            return Err(failed("OCI tool metadata output exceeds limit"));
        }
        Ok(bytes)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::process::CommandExt;

    #[test]
    fn helper_rejects_replacement_parent_before_exec() {
        let mut command = Command::new("/bin/sh");
        command.args(["-c", "exit 99"]);
        // SAFETY: the hook only installs/checks the guard in the fork child.
        unsafe {
            command.pre_exec(|| protect_parent(0));
        }
        // A mismatched parent must fail spawn, rather than execute even a trusted helper.
        assert_eq!(
            command.spawn().unwrap_err().raw_os_error(),
            Some(libc::ECHILD)
        );
    }

    #[test]
    fn helper_installs_sigkill_guard_before_exec() {
        let parent = std::process::id() as libc::pid_t;
        let mut command = Command::new("/bin/sh");
        command.args(["-c", "exit 99"]);
        // SAFETY: the hook uses only the parent guard and prctl with a live stack pointer.
        unsafe {
            command.pre_exec(move || {
                protect_parent(parent)?;
                let mut signal: libc::c_int = 0;
                if libc::prctl(libc::PR_GET_PDEATHSIG, &mut signal) != 0 {
                    return Err(std::io::Error::last_os_error());
                }
                if signal != libc::SIGKILL {
                    return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
                }
                Ok(())
            });
        }
        assert_eq!(command.status().unwrap().code(), Some(99));
    }
}

fn bounded_drain(mut file: impl Read, limit: usize) -> Vec<u8> {
    let mut output = Vec::new();
    let mut buf = [0; 8192];
    while let Ok(n @ 1..) = file.read(&mut buf) {
        let keep = n.min((limit + 1).saturating_sub(output.len()));
        output.extend_from_slice(&buf[..keep]);
    }
    output
}
