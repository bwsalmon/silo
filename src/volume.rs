//! Protected managed storage. Names never enter a worker path; durable journals fail closed.
use crate::{
    auth::random_id,
    error::{Code, Error, Result},
    linux,
    policy::{Manifest, Mode, Template, Volume},
    worker::{failed, missing, trusted_path, Worker},
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    collections::BTreeMap,
    fs::{self, File},
    io::Write,
    os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
};

const JOURNAL_LIMIT: usize = 65536;
fn encode(entry: &Entry) -> Result<Vec<u8>> {
    let bytes = serde_json::to_vec(entry).expect("typed entry");
    if bytes.len() > JOURNAL_LIMIT {
        return Err(Error::new(
            Code::QuotaExceeded,
            "Volume attachment journal capacity reached",
        ));
    }
    Ok(bytes)
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Binding {
    pub source: PathBuf,
    pub target: String,
    pub readonly: bool,
    pub device: u64,
    pub inode: u64,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Attachment {
    instance: String,
    writer: bool,
    cgroup: PathBuf,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Entry {
    id: String,
    policy: String,
    name: String,
    slot: String,
    uid: u32,
    gid: u32,
    max_bytes: u64,
    attachments: Vec<Attachment>,
    deleting: bool,
    identity: Option<(u64, u64)>,
}
pub struct Catalog {
    root: PathBuf,
    _lock: File,
    entries: BTreeMap<String, Entry>,
    cgroup: PathBuf,
    disk_bytes: u64,
    poisoned: bool,
}
fn busy() -> Error {
    Error::new(
        Code::VolumeBusy,
        "Volume has active or uncertain attachments",
    )
}
fn sync(path: &Path) -> Result<()> {
    File::open(path)
        .and_then(|f| f.sync_all())
        .map_err(|_| failed("Cannot sync durable volume catalog"))
}
impl Catalog {
    pub fn open(w: &Worker) -> Result<Self> {
        let root = w.volume_root.clone().ok_or_else(|| missing("Managed volumes require a protected persistent volume_root with bounded filesystem slots"))?;
        trusted_path(&root, true)?;
        if root.starts_with(&w.storage) || w.storage.starts_with(&root) {
            return Err(missing(
                "Persistent volume_root must be separate from image storage",
            ));
        }
        let file = File::open(&root).map_err(|_| missing("Cannot open persistent volume root"))?;
        use std::os::fd::AsRawFd;
        let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
        // SAFETY: file is live; stat is writable, and only read on success.
        if unsafe { libc::fstatfs(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
            return Err(missing("Cannot inspect persistent catalog filesystem"));
        }
        let stat = unsafe { stat.assume_init() };
        if stat.f_type != 0xef53 && stat.f_type != 0x58465342 {
            return Err(missing(
                "Persistent volume catalog requires ext4 or XFS, never tmpfs",
            ));
        }
        let lock = linux::lock(&root)?;
        for dir in ["catalog", "slots"] {
            let path = root.join(dir);
            if !path.exists() {
                fs::create_dir(&path)
                    .map_err(|_| missing("Cannot initialize managed volume root"))?;
                fs::set_permissions(&path, fs::Permissions::from_mode(0o700))
                    .map_err(|_| missing("Cannot protect volume root"))?;
            }
            trusted_path(&path, true)?;
        }
        sync(&root)?;
        let mut entries = BTreeMap::new();
        for file in
            fs::read_dir(root.join("catalog")).map_err(|_| failed("Cannot read volume catalog"))?
        {
            let path = file
                .map_err(|_| failed("Cannot read volume catalog"))?
                .path();
            let entry: Entry = serde_json::from_slice(&linux::read_private(&path, JOURNAL_LIMIT)?)
                .map_err(|_| failed("Incomplete volume catalog; operator recovery required"))?;
            if !entry.id.starts_with("vol_")
                || entry.id.len() != 68
                || !entry.id[4..].bytes().all(|b| b.is_ascii_hexdigit())
                || !slot_name(&entry.slot)
                || path.file_name().and_then(|v| v.to_str()) != Some(&entry.id)
                || entries.values().any(|e: &Entry| {
                    e.slot == entry.slot || e.policy == entry.policy && e.name == entry.name
                })
            {
                return Err(failed("Invalid volume catalog identity"));
            }
            if entries.len() >= 256 {
                return Err(missing("Volume catalog exceeds 256 managed entries"));
            }
            entries.insert(entry.id.clone(), entry);
        }
        Ok(Self {
            root,
            _lock: lock,
            entries,
            cgroup: w.cgroup.clone(),
            disk_bytes: w.ceilings.disk_bytes,
            poisoned: false,
        })
    }
    fn healthy(&self) -> Result<()> {
        if self.poisoned {
            return Err(Error::new(
                Code::CleanupUnverified,
                "Volume catalog commit uncertain; verified operator recovery required",
            ));
        }
        Ok(())
    }
    pub fn id(&self, m: &Manifest, name: &str) -> Option<String> {
        self.find(m, name).map(|e| e.id.clone())
    }
    fn find(&self, m: &Manifest, name: &str) -> Option<&Entry> {
        self.entries
            .values()
            .find(|e| e.policy == m.id() && e.name == name)
    }
    fn persist(&self, entry: &Entry, create: bool) -> Result<()> {
        let bytes = encode(entry)?;
        let dir = self.root.join("catalog");
        let path = dir.join(&entry.id);
        if create {
            let mut f = linux::create(&path)?;
            f.write_all(&bytes)
                .and_then(|_| f.sync_all())
                .map_err(|_| failed("Cannot commit volume catalog"))?;
        } else {
            // Atomic replacement. An interrupted write retains the old conservative journal.
            let tmp = dir.join(random_id("pending_")?);
            let mut f = linux::create(&tmp)?;
            f.write_all(&bytes)
                .and_then(|_| f.sync_all())
                .map_err(|_| failed("Cannot commit attachment journal"))?;
            fs::rename(&tmp, path).map_err(|_| failed("Cannot commit attachment journal"))?;
        }
        sync(&dir)
    }
    pub fn metadata(&self, m: &Manifest, name: &str) -> Value {
        match self.find(m, name) {
            Some(e) => {
                json!({"id": e.id, "name": name, "created": true, "uid": e.uid, "gid": e.gid,
                "max_bytes": e.max_bytes, "sharing": "exclusive-writer", "attachments": e.attachments.len(),
                "writer": e.attachments.iter().any(|a| a.writer), "uncertain_or_active": !e.attachments.is_empty(), "deleting": e.deleting, "initialized": e.identity.is_some()})
            }
            None => json!({"name": name, "created": false}),
        }
    }
    pub fn create(&mut self, m: &Manifest, name: &str, v: &Volume) -> Result<Value> {
        self.healthy()?;
        if let Some(e) = self.find(m, name) {
            if e.identity.is_none() || e.deleting {
                return Err(failed(
                    "Volume initialization/deletion is incomplete; operator recovery required",
                ));
            }
            return Ok(self.metadata(m, name));
        }
        if self.entries.len() >= 256
            || self
                .entries
                .values()
                .map(|e| e.max_bytes as u128)
                .sum::<u128>()
                + u128::from(v.max_bytes)
                > u128::from(self.disk_bytes)
        {
            return Err(Error::new(
                Code::QuotaExceeded,
                "Persistent volume aggregate disk/catalog ceiling reached",
            ));
        }

        let mut available: Option<(String, u64)> = None;
        for dir in fs::read_dir(self.root.join("slots"))
            .map_err(|_| missing("Missing bounded volume slots"))?
        {
            let dir = dir.map_err(|_| missing("Cannot inspect volume slot"))?;
            let slot = dir.file_name().to_string_lossy().into_owned();
            if !slot_name(&slot) || self.entries.values().any(|e| e.slot == slot) {
                continue;
            }
            let path = dir.path();
            if let Ok(capacity) = check_slot(&path, v.max_bytes) {
                if fs::read_dir(&path)
                    .map(|mut i| i.next().is_none())
                    .unwrap_or(false)
                    && available
                        .as_ref()
                        .map(|(_, best)| capacity > *best)
                        .unwrap_or(true)
                {
                    // Prefer the closest ceiling so a large request does not consume a
                    // smaller slot needed by a later smaller declared volume.
                    available = Some((slot, capacity));
                }
            }
        }
        let (slot, _) = available.ok_or_else(|| missing("No empty protected persistent filesystem slot fits the declared disk ceiling (ext4 or XFS, separate mount, nosuid,nodev,noexec, bounded bytes/inodes required)"))?;
        let mut entry = Entry {
            id: random_id("vol_")?,
            policy: m.id(),
            name: name.into(),
            slot,
            uid: v.uid,
            gid: v.gid,
            max_bytes: v.max_bytes,
            attachments: Vec::new(),
            deleting: false,
            identity: None,
        };
        // Reserve durably before creating data. Any partial initialization remains operator-visible.
        self.poisoned = true;
        self.persist(&entry, true)?;
        self.entries.insert(entry.id.clone(), entry.clone());
        let data = self.root.join("slots").join(&entry.slot).join("data");
        fs::create_dir(&data).map_err(|_| failed("Volume initialization incomplete"))?;
        fs::set_permissions(&data, fs::Permissions::from_mode(0o700))
            .map_err(|_| failed("Cannot initialize volume ownership"))?;
        // A single container UID/GID maps to the supervisor's OS identity; never recursive chown.
        sync(&data)?;
        sync(data.parent().expect("slot"))?;
        let metadata =
            fs::symlink_metadata(&data).map_err(|_| failed("Cannot inspect initialized volume"))?;
        entry.identity = Some((metadata.dev(), metadata.ino()));
        self.persist(&entry, false)?;
        self.entries.insert(entry.id.clone(), entry);
        self.poisoned = false;
        Ok(self.metadata(m, name))
    }
    pub fn acquire(&mut self, m: &Manifest, t: &Template, instance: &str) -> Result<Vec<Binding>> {
        self.healthy()?;
        let mut planned = Vec::new();
        for mount in &t.mounts {
            let e = self
                .find(m, &mount.volume)
                .ok_or_else(|| failed("Declared volume has not been explicitly created"))?;
            if e.deleting || e.uid != t.uid || e.gid != t.gid {
                return Err(failed(
                    "Volume ownership does not match the supported image identity",
                ));
            }
            let writer = mount.mode == Mode::Rw;
            if !e.attachments.is_empty() && (writer || e.attachments.iter().any(|a| a.writer)) {
                return Err(busy());
            }
            let source = self.root.join("slots").join(&e.slot).join("data");
            check_slot(source.parent().expect("slot"), e.max_bytes)?;
            // data's parent is never given to image code. No image can rename this mount root.
            let fd = open_source(&source)?;
            let metadata = fd
                .metadata()
                .map_err(|_| failed("Cannot inspect volume source"))?;
            if e.identity != Some((metadata.dev(), metadata.ino())) {
                return Err(failed(
                    "Managed volume source identity changed or initialization is incomplete",
                ));
            }
            if fd
                .metadata()
                .map_err(|_| failed("Cannot inspect volume data"))?
                .mode()
                & 0o077
                != 0
            {
                return Err(failed("Volume root permissions are no longer protected"));
            }
            planned.push((
                e.id.clone(),
                writer,
                Binding {
                    source,
                    target: mount.target.clone(),
                    readonly: !writer,
                    device: fd
                        .metadata()
                        .map_err(|_| failed("Cannot inspect source"))?
                        .dev(),
                    inode: fd
                        .metadata()
                        .map_err(|_| failed("Cannot inspect source"))?
                        .ino(),
                },
            ));
        }
        // Preflight every bounded record before changing any attachment.
        let mut updates = Vec::new();
        for (id, writer, _) in &planned {
            let mut e = self.entries[id].clone();
            e.attachments.push(Attachment {
                instance: instance.into(),
                writer: *writer,
                cgroup: self.cgroup.join(instance),
            });
            encode(&e)?;
            updates.push(e);
        }
        // Journals are committed before any process starts. Partial commits stay locked on error.
        self.poisoned = true;
        for e in updates {
            self.persist(&e, false)?;
            self.entries.insert(e.id.clone(), e);
        }
        self.poisoned = false;
        Ok(planned.into_iter().map(|(_, _, b)| b).collect())
    }
    /// Caller has killed/reaped the helper AND verified/removes the whole cgroup.
    pub fn release_verified(&mut self, instance: &str) -> Result<()> {
        self.healthy()?;
        let ids: Vec<_> = self
            .entries
            .values()
            .filter(|e| e.attachments.iter().any(|a| a.instance == instance))
            .map(|e| e.id.clone())
            .collect();
        self.poisoned = true;
        for id in ids {
            let mut e = self.entries[&id].clone();
            e.attachments.retain(|a| a.instance != instance);
            self.persist(&e, false)?;
            self.entries.insert(id, e);
        }
        self.poisoned = false;
        Ok(())
    }
    pub fn recover(&mut self, m: &Manifest, name: &str, cgroup: &Path) -> Result<Value> {
        let e = self
            .find(m, name)
            .ok_or_else(crate::error::not_found)?
            .clone();
        self.healthy()?;
        for a in &e.attachments {
            if !(a.instance.starts_with("job_") || a.instance.starts_with("svc_"))
                || a.instance.len() != 68
                || !a.instance[4..].bytes().all(|b| b.is_ascii_hexdigit())
                || a.cgroup != cgroup.join(&a.instance)
                || a.cgroup.try_exists().map_err(|_| busy())?
            {
                return Err(busy());
            }
        }
        // Removing a populated cgroup is impossible. The exclusive root lock excludes another
        // supervisor; absent journaled cgroups prove the old whole trees were removed.
        for a in e.attachments {
            self.release_verified(&a.instance)?;
        }
        Ok(self.metadata(m, name))
    }
    pub fn remove(&mut self, m: &Manifest, name: &str) -> Result<Value> {
        self.healthy()?;
        let Some(mut e) = self.find(m, name).cloned() else {
            return Ok(json!({"name":name,"removed":true}));
        };
        if !e.attachments.is_empty() {
            return Err(busy());
        }
        self.poisoned = true;
        e.deleting = true;
        self.persist(&e, false)?;
        self.entries.insert(e.id.clone(), e.clone());
        let path = self.root.join("slots").join(&e.slot);
        check_slot(&path, e.max_bytes)?;
        let data = path.join("data");
        // std remove_dir_all does not follow symlinks, including during concurrent traversal.
        if data
            .try_exists()
            .map_err(|_| failed("Cannot inspect data"))?
        {
            let source = open_source(&data)?;
            let metadata = source
                .metadata()
                .map_err(|_| failed("Cannot inspect data for deletion"))?;
            if e.identity.is_some() && e.identity != Some((metadata.dev(), metadata.ino())) {
                return Err(failed("Refusing deletion of a substituted volume root"));
            }
            fs::remove_dir_all(&data).map_err(|_| failed("Cannot remove volume data"))?;
        }
        sync(&path)?;
        fs::remove_file(self.root.join("catalog").join(&e.id))
            .map_err(|_| failed("Cannot remove catalog entry"))?;
        sync(&self.root.join("catalog"))?;
        self.entries.remove(&e.id);
        self.poisoned = false;
        Ok(json!({"id":e.id,"name":name,"removed":true}))
    }
}
fn slot_name(s: &str) -> bool {
    s.strip_prefix("slot_")
        .map(|n| !n.is_empty() && n.len() <= 8 && n.bytes().all(|b| b.is_ascii_digit()))
        .unwrap_or(false)
}
pub fn open_source(path: &Path) -> Result<File> {
    // Every component is protected against rename, and O_NOFOLLOW refuses a swapped final link.
    trusted_path(
        path.parent()
            .ok_or_else(|| failed("Missing source parent"))?,
        true,
    )?;
    let file = fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
        .map_err(|_| failed("Volume source must be a real managed directory"))?;
    if file
        .metadata()
        .map_err(|_| failed("Cannot inspect source"))?
        .uid()
        != linux::uid()
    {
        return Err(failed("Volume source ownership changed"));
    }
    Ok(file)
}
fn check_slot(path: &Path, max: u64) -> Result<u64> {
    trusted_path(path, true)?;
    let mountinfo = fs::read_to_string("/proc/self/mountinfo")
        .map_err(|_| missing("Cannot inspect persistent mount"))?;
    let text = path.to_str().ok_or_else(|| missing("Invalid slot path"))?;
    let bounded = mountinfo.lines().any(|l| {
        let parts: Vec<_> = l.split_whitespace().collect();
        parts.get(4) == Some(&text)
            && parts
                .get(5)
                .map(|s| {
                    s.split(',').any(|v| v == "nosuid")
                        && s.split(',').any(|v| v == "nodev")
                        && s.split(',').any(|v| v == "noexec")
                })
                .unwrap_or(false)
            && (l.contains(" - ext4 ") || l.contains(" - xfs "))
    });
    let file = File::open(path).map_err(|_| missing("Missing volume slot"))?;
    use std::os::fd::AsRawFd;
    let mut stat = std::mem::MaybeUninit::<libc::statfs>::uninit();
    // SAFETY: live descriptor, writable output.
    if !bounded || unsafe { libc::fstatfs(file.as_raw_fd(), stat.as_mut_ptr()) } != 0 {
        return Err(missing(
            "Volume requires a separate persistent ext4/XFS mount with nosuid,nodev,noexec",
        ));
    }
    let stat = unsafe { stat.assume_init() };
    if i128::from(stat.f_blocks) * i128::from(stat.f_bsize) > i128::from(max)
        || stat.f_files > 65536
    {
        return Err(missing(
            "Volume filesystem exceeds declared byte/inode ceiling",
        ));
    }
    Ok((i128::from(stat.f_blocks) * i128::from(stat.f_bsize)) as u64)
}

#[cfg(test)]
mod tests {
    // Journal unit tests only. No synthetic file below is worker qualification evidence.
    use super::*;
    fn journal() -> (tempfile::TempDir, Catalog, Manifest, String) {
        let dir = tempfile::tempdir().unwrap();
        fs::set_permissions(dir.path(), fs::Permissions::from_mode(0o700)).unwrap();
        fs::create_dir(dir.path().join("catalog")).unwrap();
        let m = Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap();
        let id = format!("vol_{}", "a".repeat(64));
        let instance = format!("job_{}", "b".repeat(64));
        let cgroup = dir.path().join("groups");
        fs::create_dir(&cgroup).unwrap();
        let e = Entry {
            id: id.clone(),
            policy: m.id(),
            name: "results".into(),
            slot: "slot_0".into(),
            uid: 1000,
            gid: 1000,
            max_bytes: 1024,
            attachments: vec![Attachment {
                instance: instance.clone(),
                writer: true,
                cgroup: cgroup.join(&instance),
            }],
            deleting: false,
            identity: Some((1, 2)),
        };
        let catalog = Catalog {
            root: dir.path().into(),
            _lock: linux::lock(dir.path()).unwrap(),
            entries: [(id, e)].into_iter().collect(),
            cgroup,
            disk_bytes: 2048,
            poisoned: false,
        };
        catalog
            .persist(catalog.entries.values().next().unwrap(), true)
            .unwrap();
        (dir, catalog, m, instance)
    }
    #[test]
    fn journal_blocks_deletion_and_changed_parent_recovery_until_verified_release() {
        let (_dir, mut c, m, instance) = journal();
        assert_eq!(c.remove(&m, "results").unwrap_err().code, Code::VolumeBusy);
        assert_eq!(
            c.recover(&m, "results", Path::new("/different-parent"))
                .unwrap_err()
                .code,
            Code::VolumeBusy
        );
        fs::create_dir(c.cgroup.join(&instance)).unwrap();
        assert_eq!(
            c.recover(&m, "results", &c.cgroup.clone())
                .unwrap_err()
                .code,
            Code::VolumeBusy
        );
        c.release_verified(&instance).unwrap();
        let entry: Entry = serde_json::from_slice(
            &fs::read(c.root.join("catalog").join(c.id(&m, "results").unwrap())).unwrap(),
        )
        .unwrap();
        assert!(entry.attachments.is_empty());
        assert_eq!(c.metadata(&m, "results")["attachments"], 0);
    }
    #[test]
    fn partial_commit_poison_and_uninitialized_creation_cannot_report_success() {
        let (_dir, mut c, m, _) = journal();
        c.entries.values_mut().next().unwrap().identity = None;
        assert_eq!(
            c.create(&m, "results", &m.volumes["results"])
                .unwrap_err()
                .code,
            Code::LaunchFailed
        );
        c.poisoned = true;
        assert_eq!(
            c.remove(&m, "results").unwrap_err().code,
            Code::CleanupUnverified
        );
        assert_eq!(
            c.release_verified("unknown").unwrap_err().code,
            Code::CleanupUnverified
        );
    }
    #[test]
    fn oversized_journal_is_rejected_before_replacing_durable_state() {
        let (_dir, c, m, _) = journal();
        let id = c.id(&m, "results").unwrap();
        let path = c.root.join("catalog").join(&id);
        let original = fs::read(&path).unwrap();
        let mut entry = c.entries[&id].clone();
        entry.attachments = vec![entry.attachments[0].clone(); 2048];
        assert_eq!(
            c.persist(&entry, false).unwrap_err().code,
            Code::QuotaExceeded
        );
        assert_eq!(fs::read(&path).unwrap(), original);
    }
    #[test]
    fn nofollow_source_refuses_symlinks_and_unbounded_slots() {
        use std::os::unix::fs::symlink;
        let dir = tempfile::tempdir().unwrap();
        fs::set_permissions(dir.path(), fs::Permissions::from_mode(0o700)).unwrap();
        fs::create_dir(dir.path().join("real")).unwrap();
        symlink("real", dir.path().join("link")).unwrap();
        assert!(open_source(&dir.path().join("link")).is_err());
        assert!(check_slot(dir.path(), u64::MAX).is_err());
        for name in [
            "../slot_0",
            "slot_",
            "slot_/host",
            "slot_000000000",
            "/slot_1",
        ] {
            assert!(!slot_name(name));
        }
    }
}
