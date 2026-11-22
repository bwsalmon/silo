use crate::error::{manifest, Result};
use serde::{
    de::{MapAccess, Visitor},
    Deserialize, Deserializer, Serialize,
};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    marker::PhantomData,
    path::{Component, Path},
};

pub const MAX_DOCUMENT: usize = 256 * 1024;

fn unique_map<'de, D, V>(d: D) -> std::result::Result<BTreeMap<String, V>, D::Error>
where
    D: Deserializer<'de>,
    V: Deserialize<'de>,
{
    struct Unique<V>(PhantomData<V>);
    impl<'de, V: Deserialize<'de>> Visitor<'de> for Unique<V> {
        type Value = BTreeMap<String, V>;
        fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
            f.write_str("a map with unique names")
        }
        fn visit_map<M: MapAccess<'de>>(
            self,
            mut m: M,
        ) -> std::result::Result<Self::Value, M::Error> {
            let mut values = BTreeMap::new();
            while let Some((key, value)) = m.next_entry::<String, V>()? {
                if values.insert(key, value).is_some() {
                    return Err(serde::de::Error::custom("duplicate name"));
                }
            }
            Ok(values)
        }
    }
    d.deserialize_map(Unique(PhantomData))
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq, PartialOrd, Ord)]
#[serde(rename_all = "snake_case")]
pub enum Permission {
    Run,
    List,
    Status,
    Logs,
    Stop,
    VolumeList,
    VolumeInspect,
    ServiceList,
    ServiceInspect,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Manifest {
    pub version: u32,
    pub project: String,
    #[serde(default, deserialize_with = "unique_map")]
    pub jobs: BTreeMap<String, Template>,
    #[serde(default, deserialize_with = "unique_map")]
    pub services: BTreeMap<String, Service>,
    #[serde(default, deserialize_with = "unique_map")]
    pub volumes: BTreeMap<String, Volume>,
    pub network: Network,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Template {
    pub image: String,
    #[serde(default)]
    pub command: Vec<String>,
    #[serde(default, deserialize_with = "unique_map")]
    pub env: BTreeMap<String, String>,
    #[serde(default)]
    pub workdir: String,
    #[serde(default)]
    pub uid: u32,
    #[serde(default)]
    pub gid: u32,
    #[serde(default)]
    pub executable_inputs: BTreeSet<String>,
    #[serde(default)]
    pub mounts: Vec<Mount>,
    pub limits: Limits,
    #[serde(default)]
    pub agent: BTreeSet<Permission>,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Service {
    pub template: Template,
    pub listen: BTreeSet<u16>,
    #[serde(default)]
    pub agent: BTreeSet<Permission>,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Limits {
    pub processes: u32,
    pub memory_bytes: u64,
    pub cpu_millis: u32,
    pub scratch_bytes: u64,
    pub connections: u32,
    pub duration_secs: u64,
    pub max_instances: u32,
}
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq, PartialOrd, Ord)]
#[serde(rename_all = "lowercase")]
pub enum Mode {
    Ro,
    Rw,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Mount {
    pub volume: String,
    pub target: String,
    pub mode: Mode,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum Sharing {
    ExclusiveWriter,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Volume {
    pub sharing: Sharing,
    pub uid: u32,
    pub gid: u32,
    pub max_bytes: u64,
    #[serde(default)]
    pub agent: BTreeSet<Permission>,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum External {
    Deny,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Protocol {
    Tcp,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Link {
    pub from: String,
    pub to: String,
    pub protocol: Protocol,
    pub port: u16,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Network {
    pub external: External,
    #[serde(default)]
    pub allow: Vec<Link>,
}

pub fn name(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 64
        && s.bytes()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-' || c == b'_')
}
pub fn absolute(s: &str) -> bool {
    s.starts_with('/')
        && !s.contains('\0')
        && !s.contains("//")
        && Path::new(s)
            .components()
            .all(|c| matches!(c, Component::RootDir | Component::Normal(_)))
        && !s.split('/').any(|p| p == "." || p == "..")
}
fn under(path: &str, root: &str) -> bool {
    Path::new(path).starts_with(root)
}
pub fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        out.push(DIGITS[(b >> 4) as usize] as char);
        out.push(DIGITS[(b & 15) as usize] as char);
    }
    out
}
pub fn digest(bytes: &[u8]) -> String {
    hex(&Sha256::digest(bytes))
}
impl Manifest {
    pub fn parse(bytes: &[u8]) -> Result<Self> {
        if bytes.len() > MAX_DOCUMENT {
            return Err(manifest());
        }
        let m: Self = serde_yaml::from_slice(bytes).map_err(|_| manifest())?;
        m.validate()?;
        Ok(m)
    }
    pub fn canonical(&self) -> Vec<u8> {
        serde_json::to_vec(self).expect("typed manifest serializes")
    }
    pub fn id(&self) -> String {
        format!("pol_{}", digest(&self.canonical()))
    }
    pub fn resource_id(&self, kind: &str, name: &str) -> String {
        format!(
            "{kind}_{}",
            digest(format!("{}:{kind}:{name}", self.id()).as_bytes())
        )
    }
    pub fn validate(&self) -> Result<()> {
        if self.version != 1
            || !name(&self.project)
            || self.jobs.len() + self.services.len() + self.volumes.len() > 256
            || self.network.allow.len() > 256
        {
            return Err(manifest());
        }
        for (n, t) in &self.jobs {
            if !name(n) || self.services.contains_key(n) {
                return Err(manifest());
            }
            self.check_template(t)?;
            if !t.agent.iter().all(|p| {
                matches!(
                    p,
                    Permission::Run
                        | Permission::List
                        | Permission::Status
                        | Permission::Logs
                        | Permission::Stop
                )
            }) {
                return Err(manifest());
            }
        }
        for (n, s) in &self.services {
            if !name(n)
                || s.listen.is_empty()
                || s.listen.contains(&0)
                || !s.template.agent.is_empty()
                || !s
                    .agent
                    .iter()
                    .all(|p| matches!(p, Permission::ServiceList | Permission::ServiceInspect))
            {
                return Err(manifest());
            }
            self.check_template(&s.template)?;
        }
        for (n, v) in &self.volumes {
            if !name(n)
                || v.max_bytes == 0
                || !v
                    .agent
                    .iter()
                    .all(|p| matches!(p, Permission::VolumeList | Permission::VolumeInspect))
            {
                return Err(manifest());
            }
        }
        let mut links = BTreeSet::new();
        for l in &self.network.allow {
            if (!self.jobs.contains_key(&l.from) && !self.services.contains_key(&l.from))
                || !self
                    .services
                    .get(&l.to)
                    .map(|s| s.listen.contains(&l.port))
                    .unwrap_or(false)
                || !links.insert((&l.from, &l.to, l.port))
            {
                return Err(manifest());
            }
        }
        if self.canonical().len() > MAX_DOCUMENT / 2 {
            return Err(manifest());
        }
        Ok(())
    }
    fn check_template(&self, t: &Template) -> Result<()> {
        let image_ok = t
            .image
            .rsplit_once("@sha256:")
            .map(|(reference, hash)| {
                !reference.is_empty()
                    && hash.len() == 64
                    && hash
                        .bytes()
                        .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase())
                    && reference
                        .bytes()
                        .all(|c| c.is_ascii_alphanumeric() || b"./:_-".contains(&c))
            })
            .unwrap_or(false);
        let l = &t.limits;
        if !image_ok
            || (!t.command.is_empty() && !absolute(&t.command[0]))
            || t.command.iter().any(|v| v.contains('\0'))
            || (!t.workdir.is_empty() && !absolute(&t.workdir))
            || t.env
                .iter()
                .any(|(k, v)| k.is_empty() || k.contains(['=', '\0']) || v.contains('\0'))
            || l.processes == 0
            || l.memory_bytes == 0
            || l.cpu_millis == 0
            || l.scratch_bytes == 0
            || l.connections == 0
            || l.duration_secs == 0
            || l.duration_secs > 86400
            || l.max_instances == 0
            || l.max_instances > 256
        {
            return Err(manifest());
        }
        let mut targets: Vec<&str> = Vec::new();
        for m in &t.mounts {
            if !self.volumes.contains_key(&m.volume)
                || !absolute(&m.target)
                || [
                    "/", "/proc", "/sys", "/dev", "/etc", "/run", "/tmp", "/bin", "/sbin", "/usr",
                    "/lib", "/lib64",
                ]
                .iter()
                .any(|p| under(p, &m.target) || under(&m.target, p) && *p != "/")
                || targets
                    .iter()
                    .any(|p| under(p, &m.target) || under(&m.target, p))
            {
                return Err(manifest());
            }
            targets.push(&m.target);
        }
        // All executable inputs must remain in the immutable image, never a mounted volume.
        for input in t.executable_inputs.iter().chain(t.command.first()) {
            if !absolute(input)
                || targets.iter().any(|p| under(input, p))
                || ["/tmp", "/run", "/proc", "/sys", "/dev"]
                    .iter()
                    .any(|p| under(input, p))
            {
                return Err(manifest());
            }
        }
        // Refuse obvious interpreter/script/config inputs from mutable mounts.
        // Operators must still enumerate indirect code-loading inputs explicitly.
        if t.command
            .iter()
            .skip(1)
            .any(|arg| targets.iter().any(|p| arg.contains(p)))
        {
            return Err(manifest());
        }
        Ok(())
    }
}
