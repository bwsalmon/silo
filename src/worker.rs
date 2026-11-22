//! Trusted operator worker configuration and kernel-enforced aggregate/per-job ceilings.
use crate::{
    error::{Code, Error, Result},
    linux,
    policy::{digest, Limits},
};
use serde::{Deserialize, Serialize};
use std::{
    fs::{self, File},
    io::Read,
    os::unix::fs::MetadataExt,
    path::{Path, PathBuf},
};

pub fn missing(message: &str) -> Error {
    Error::new(Code::UnsupportedWorker, message)
}
pub fn failed(message: &str) -> Error {
    Error::new(Code::LaunchFailed, message)
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Tool {
    pub path: PathBuf,
    pub sha256: String,
}
impl Tool {
    pub fn verify(&self) -> Result<()> {
        trusted_path(&self.path, false)?;
        let m = fs::metadata(&self.path).map_err(|_| missing("Pinned executable is missing"))?;
        if !m.is_file() || m.mode() & 0o6111 != 0o111 || m.len() > 128 * 1024 * 1024 {
            return Err(missing(
                "Pinned executable must be executable and non-setuid/non-setgid",
            ));
        }
        let bytes = fs::read(&self.path).map_err(|_| missing("Cannot read pinned executable"))?;
        if self.sha256.len() != 64 || digest(&bytes) != self.sha256 {
            return Err(missing("Pinned executable SHA-256 does not match"));
        }
        Ok(())
    }
}
pub fn trusted_path(path: &Path, private: bool) -> Result<()> {
    if !path.is_absolute() || fs::canonicalize(path).ok().as_deref() != Some(path) {
        return Err(missing("Worker paths must be absolute and symlink-free"));
    }
    for (index, p) in path.ancestors().enumerate() {
        let m = fs::symlink_metadata(p).map_err(|_| missing("Worker path is unavailable"))?;
        if (m.uid() != 0 && m.uid() != linux::uid())
            || m.mode() & 0o022 != 0
                && (index == 0 || !m.is_dir() || m.uid() != 0 || m.mode() & 0o1000 == 0)
            || index == 0 && private && (m.uid() != linux::uid() || m.mode() & 0o077 != 0)
        {
            return Err(missing(
                "Worker paths require trusted owners and protected permissions",
            ));
        }
    }
    Ok(())
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Ceilings {
    pub processes: u32,
    pub memory_bytes: u64,
    pub cpu_millis: u32,
    pub scratch_bytes: u64,
    pub disk_bytes: u64,
    pub duration_secs: u64,
    pub max_instances: u32,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Worker {
    pub bwrap: Tool,
    pub skopeo: Tool,
    pub umoci: Tool,
    pub storage: PathBuf,
    #[serde(default)]
    pub volume_root: Option<PathBuf>,
    pub cgroup: PathBuf,
    /// Restricted native agents must run under this separate, unprivileged OS principal.
    pub agent_uid: u32,
    #[serde(default)]
    pub registry_ca_dir: Option<PathBuf>,
    pub ceilings: Ceilings,
}
impl Worker {
    pub fn load(state: &Path) -> Result<Self> {
        let data = linux::read_private(&state.join("worker.json"), 65536)
            .map_err(|_| missing("Missing private worker.json: trusted tool pins, protected bounded storage and delegated cgroup v2 are required"))?;
        let w: Self = serde_json::from_slice(&data)
            .map_err(|_| missing("Invalid trusted worker.json schema"))?;
        w.qualify()?;
        Ok(w)
    }
    pub fn qualify(&self) -> Result<()> {
        if cfg!(not(target_arch = "x86_64")) {
            return Err(missing("Only Linux x86-64 workers are supported"));
        }
        if self.agent_uid == 0 || self.agent_uid == linux::uid() || linux::uid() == 0 {
            return Err(missing(
                "Rootless supervisor and restricted agents require separate non-root OS principals",
            ));
        }
        if let Some(path) = &self.registry_ca_dir {
            trusted_path(path, true)?;
        }
        // Rootless gid_map setup disables setgroups. Reject inherited authority that cannot
        // be represented by the single supported GID instead of silently retaining it.
        // SAFETY: first query obtains the required size; the second writes only that many entries.
        let count = unsafe { libc::getgroups(0, std::ptr::null_mut()) };
        if count < 0 {
            return Err(missing("Cannot inspect inherited supplementary groups"));
        }
        let mut groups = vec![0; count as usize];
        if unsafe { libc::getgroups(count, groups.as_mut_ptr()) } != count
            || groups
                .iter()
                .any(|group| *group != unsafe { libc::getgid() })
        {
            return Err(missing("Single-GID rootless worker requires a supervisor launched without supplementary groups beyond its primary GID"));
        }
        let status = fs::read_to_string("/proc/self/status")
            .map_err(|_| missing("Cannot inspect rootless supervisor capabilities"))?;
        for field in ["CapEff:", "CapPrm:", "CapInh:", "CapAmb:"] {
            let value = status
                .lines()
                .find_map(|line| line.strip_prefix(field))
                .and_then(|v| u64::from_str_radix(v.trim(), 16).ok());
            if value != Some(0) {
                return Err(missing("Rootless supervisor must have no effective/permitted/inheritable/ambient host capabilities"));
            }
        }
        self.bwrap.verify()?;
        self.skopeo.verify()?;
        self.umoci.verify()?;
        trusted_path(&self.storage, true)?;
        trusted_path(&self.cgroup, false)?;
        let c = &self.ceilings;
        if c.processes < 8
            || c.memory_bytes < 32 * 1024 * 1024
            || c.cpu_millis == 0
            || c.scratch_bytes < 8192
            || c.disk_bytes == 0
            || c.duration_secs == 0
            || c.duration_secs > 86400
            || c.max_instances == 0
            || c.max_instances > 256
        {
            return Err(missing("Worker ceilings are missing or invalid"));
        }
        // Require an exclusive bounded mount, not a directory on an unbounded/shared disk.
        let mountinfo = fs::read_to_string("/proc/self/mountinfo")
            .map_err(|_| missing("Cannot inspect protected storage mount"))?;
        let storage = self
            .storage
            .to_str()
            .ok_or_else(|| missing("Invalid storage path"))?;
        if storage.bytes().any(|b| b <= b' ' || b == b'\\')
            || !mountinfo
                .lines()
                .any(|l| l.split_whitespace().nth(4) == Some(storage) && l.contains(" - tmpfs "))
        {
            return Err(missing(
                "Image storage requires its own protected size-bounded tmpfs mount",
            ));
        }
        let f = File::open(&self.storage).map_err(|_| missing("Cannot open image storage"))?;
        let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
        use std::os::fd::AsRawFd;
        // SAFETY: stat points to writable storage and f is live.
        if unsafe { libc::fstatfs(f.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
            return Err(missing("Cannot inspect storage capacity"));
        }
        // SAFETY: fstatfs initialized stat on success.
        let stat = unsafe { stat.assume_init() };
        if i128::from(stat.f_type) != i128::from(libc::TMPFS_MAGIC)
            || i128::from(stat.f_blocks).saturating_mul(i128::from(stat.f_bsize))
                > i128::from(c.disk_bytes)
            || stat.f_files > 65536
        {
            return Err(missing(
                "Storage tmpfs exceeds worker disk/inode ceiling (maximum 65536 inodes)",
            ));
        }
        let cgroup_file = File::open(&self.cgroup)
            .map_err(|_| missing("Missing delegated cgroup v2 directory"))?;
        let mut cgroup_stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
        // SAFETY: cgroup_file is live and cgroup_stat is writable.
        if unsafe { libc::fstatfs(cgroup_file.as_raw_fd(), cgroup_stat.as_mut_ptr()) } != 0
            || i128::from(unsafe { cgroup_stat.assume_init() }.f_type)
                != i128::from(libc::CGROUP2_SUPER_MAGIC)
        {
            return Err(missing(
                "Worker resource controls require a real cgroup v2 filesystem",
            ));
        }
        let metadata = fs::metadata(&self.cgroup)
            .map_err(|_| missing("Delegated cgroup v2 is unavailable"))?;
        if metadata.uid() != linux::uid() {
            return Err(missing("Worker requires an exclusively delegated cgroup v2 subtree owned by the supervisor"));
        }
        if read(&self.cgroup, "cgroup.type")? != "domain"
            || !read(&self.cgroup, "cgroup.procs")?.is_empty()
        {
            return Err(missing(
                "Worker cgroup must be an empty domain separate from the supervisor",
            ));
        }
        for entry in
            fs::read_dir(&self.cgroup).map_err(|_| missing("Cannot inspect delegated cgroup"))?
        {
            let entry = entry.map_err(|_| missing("Cannot inspect delegated cgroup"))?;
            if entry
                .file_type()
                .map_err(|_| missing("Cannot inspect delegated cgroup"))?
                .is_dir()
            {
                return Err(Error::new(Code::CleanupUnverified, "Existing cgroup descendants require verified cleanup; recovery reattachment is refused"));
            }
        }
        apply_limits(&self.cgroup, c.processes, c.memory_bytes, c.cpu_millis)?;
        fs::write(
            self.cgroup.join("cgroup.subtree_control"),
            "+cpu +memory +pids",
        )
        .map_err(|_| missing("Missing cgroup v2 cpu/memory/pids delegation"))?;
        let controllers = read(&self.cgroup, "cgroup.subtree_control")?;
        for required in ["cpu", "memory", "pids"] {
            if !controllers.split_whitespace().any(|s| s == required) {
                return Err(missing("Missing cgroup v2 cpu/memory/pids controllers"));
            }
        }
        Ok(())
    }
    pub fn check_limits(&self, l: &Limits) -> Result<()> {
        let c = &self.ceilings;
        if l.processes < 8
            || l.processes > c.processes
            || l.memory_bytes > c.memory_bytes
            || l.cpu_millis > c.cpu_millis
            || l.scratch_bytes > c.scratch_bytes
            || l.scratch_bytes < 8192
            || l.duration_secs > c.duration_secs
            || l.max_instances > c.max_instances
        {
            return Err(missing("Template exceeds qualified worker process/memory/CPU/scratch/duration/instance ceilings"));
        }
        Ok(())
    }
    pub fn group(&self, id: &str, limits: &Limits) -> Result<Group> {
        self.check_limits(limits)?;
        let path = self.cgroup.join(id);
        fs::create_dir(&path).map_err(|_| missing("Cannot create per-instance cgroup v2"))?;
        let group = Group { path };
        if let Err(e) = apply_limits(
            &group.path,
            limits.processes,
            limits.memory_bytes,
            limits.cpu_millis,
        ) {
            let _ = fs::remove_dir(&group.path);
            return Err(e);
        }
        Ok(group)
    }
}
fn read(path: &Path, file: &str) -> Result<String> {
    fs::read_to_string(path.join(file))
        .map(|s| s.trim().to_owned())
        .map_err(|_| missing("Missing readable cgroup v2 resource control"))
}
fn write_checked(path: &Path, file: &str, value: &str) -> Result<()> {
    fs::write(path.join(file), value)
        .map_err(|_| missing(&format!("Missing writable cgroup v2 {file}")))?;
    if read(path, file)? != value {
        return Err(missing(&format!("Kernel did not enforce cgroup v2 {file}")));
    }
    Ok(())
}
fn apply_limits(path: &Path, processes: u32, memory: u64, cpu: u32) -> Result<()> {
    write_checked(path, "pids.max", &processes.to_string())?;
    write_checked(path, "memory.max", &memory.to_string())?;
    write_checked(path, "memory.swap.max", "0")?;
    write_checked(path, "memory.oom.group", "1")?;
    write_checked(path, "cpu.max", &format!("{} 100000", u64::from(cpu) * 100))?;
    // Opening does not kill anything; lack of cgroup.kill must be detected before launch.
    fs::OpenOptions::new()
        .write(true)
        .open(path.join("cgroup.kill"))
        .map_err(|_| missing("Missing cgroup v2 whole-tree kill capability"))?;
    Ok(())
}
pub struct Group {
    pub path: PathBuf,
}
impl Group {
    pub fn kill(&self) -> Result<()> {
        fs::write(self.path.join("cgroup.kill"), "1")
            .map_err(|_| Error::new(Code::CleanupUnverified, "Cannot kill whole workload cgroup"))
    }
    pub fn empty(&self) -> Result<bool> {
        Ok(read(&self.path, "cgroup.events")?
            .lines()
            .any(|l| l == "populated 0"))
    }
    pub fn remove(&self) -> Result<()> {
        if !self.empty()? {
            return Err(Error::new(
                Code::CleanupUnverified,
                "Workload cgroup remains populated",
            ));
        }
        fs::remove_dir(&self.path)
            .map_err(|_| Error::new(Code::CleanupUnverified, "Cannot remove workload cgroup"))
    }
}
pub fn verify_blob(path: &Path, expected: &str, size: u64, max: u64) -> Result<Vec<u8>> {
    if !valid_digest(expected) || size > max {
        return Err(failed("Unsupported OCI digest or blob size"));
    }
    let mut f = File::open(path).map_err(|_| failed("OCI blob is missing"))?;
    if !f
        .metadata()
        .map_err(|_| failed("Cannot inspect OCI blob"))?
        .is_file()
    {
        return Err(failed("OCI blob must be a regular file"));
    }
    let mut data = Vec::new();
    f.by_ref()
        .take(max + 1)
        .read_to_end(&mut data)
        .map_err(|_| failed("Cannot read OCI blob"))?;
    if data.len() as u64 != size || format!("sha256:{}", digest(&data)) != expected {
        return Err(failed("OCI blob size or SHA-256 mismatch"));
    }
    Ok(data)
}
pub fn valid_digest(s: &str) -> bool {
    s.strip_prefix("sha256:")
        .map(|v| {
            v.len() == 64
                && v.bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        })
        .unwrap_or(false)
}
