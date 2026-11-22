use crate::{
    error::{invalid, unauthenticated, Result},
    policy::{digest, name, Manifest, Mode, Permission},
};
use serde::{Deserialize, Serialize};
use std::{
    collections::{BTreeMap, BTreeSet},
    fs::File,
    io::Read,
    time::{SystemTime, UNIX_EPOCH},
};
use zeroize::Zeroize;

#[derive(Deserialize, Serialize)]
#[serde(transparent)]
pub struct Secret(pub String);
impl Drop for Secret {
    fn drop(&mut self) {
        self.0.zeroize();
    }
}
impl Secret {
    pub fn valid(&self) -> bool {
        self.0.len() == 64 && self.0.bytes().all(|c| c.is_ascii_hexdigit())
    }
    pub fn digest(&self) -> String {
        digest(self.0.as_bytes())
    }
}
pub fn random_id(prefix: &str) -> Result<String> {
    let mut bytes = [0u8; 32];
    File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut bytes))
        .map_err(|_| crate::error::unavailable())?;
    let hex = crate::policy::hex(&bytes);
    bytes.zeroize();
    Ok(format!("{prefix}{hex}"))
}
pub fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Scope {
    #[serde(default)]
    pub templates: BTreeSet<String>,
    #[serde(default)]
    pub permissions: BTreeSet<Permission>,
    #[serde(default)]
    pub volumes: BTreeSet<String>,
    #[serde(default)]
    pub services: BTreeSet<String>,
    #[serde(default)]
    pub mounts: BTreeMap<String, Mode>,
    // Canonical service:port identifiers; direction is always the fixed template's.
    #[serde(default)]
    pub links: BTreeSet<String>,
    pub max_instances: u32,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Grant {
    pub session: String,
    pub project: String,
    pub policy: String,
    pub expires_at: u64,
    pub scope: Scope,
}
impl Grant {
    pub fn validate(&self, m: &Manifest, time: u64) -> Result<()> {
        if !name(&self.session)
            || self.project != m.project
            || self.policy != m.id()
            || self.expires_at <= time
            || self.expires_at > time.saturating_add(86400)
            || self.scope.max_instances == 0
            || self.scope.max_instances > 256
            || !self.scope.templates.iter().all(|n| m.jobs.contains_key(n))
            || !self
                .scope
                .volumes
                .iter()
                .chain(self.scope.mounts.keys())
                .all(|n| m.volumes.contains_key(n))
            || !self
                .scope
                .services
                .iter()
                .all(|n| m.services.contains_key(n))
            || !self.scope.links.iter().all(|n| {
                m.network
                    .allow
                    .iter()
                    .any(|l| *n == format!("{}:{}", l.to, l.port))
            })
        {
            return Err(invalid());
        }
        Ok(())
    }
    pub fn permits(&self, m: &Manifest, permission: Permission, template: &str) -> bool {
        self.project == m.project
            && self.policy == m.id()
            && self.scope.permissions.contains(&permission)
            && self.scope.templates.contains(template)
            && m.jobs
                .get(template)
                .map(|t| t.agent.contains(&permission))
                .unwrap_or(false)
    }
    pub fn launch(&self, m: &Manifest, template: &str) -> bool {
        if !self.permits(m, Permission::Run, template) {
            return false;
        }
        let t = &m.jobs[template];
        t.mounts.iter().all(|mount| {
            matches!(
                (self.scope.mounts.get(&mount.volume), &mount.mode),
                (Some(Mode::Rw), _) | (Some(Mode::Ro), Mode::Ro)
            )
        }) && m
            .network
            .allow
            .iter()
            .filter(|l| l.from == template)
            .all(|l| self.scope.links.contains(&format!("{}:{}", l.to, l.port)))
    }
}

#[derive(Clone)]
pub enum Principal {
    Operator,
    Agent { id: String, grant: Grant },
}
struct Stored {
    id: String,
    grant: Grant,
    revoked: bool,
}
pub struct Credentials {
    operator_digest: String,
    entries: BTreeMap<String, Stored>,
}
impl Credentials {
    pub fn new(operator: Secret) -> Result<Self> {
        if !operator.valid() {
            return Err(unauthenticated());
        }
        Ok(Self {
            operator_digest: operator.digest(),
            entries: BTreeMap::new(),
        })
    }
    pub fn authenticate(&self, secret: &Secret, time: u64) -> Result<Principal> {
        if !secret.valid() {
            return Err(unauthenticated());
        }
        let key = secret.digest();
        if key == self.operator_digest {
            return Ok(Principal::Operator);
        }
        match self.entries.get(&key) {
            Some(s) if !s.revoked && s.grant.expires_at > time => Ok(Principal::Agent {
                id: s.id.clone(),
                grant: s.grant.clone(),
            }),
            _ => Err(unauthenticated()),
        }
    }
    pub fn issue(&mut self, grant: Grant, m: &Manifest, time: u64) -> Result<(String, Secret)> {
        grant.validate(m, time)?;
        if self.entries.len() >= 4096 {
            return Err(crate::error::Error::new(
                crate::error::Code::QuotaExceeded,
                "Credential capacity reached",
            ));
        }
        let secret = Secret(random_id("")?);
        let id = random_id("grt_")?;
        self.entries.insert(
            secret.digest(),
            Stored {
                id: id.clone(),
                grant,
                revoked: false,
            },
        );
        Ok((id, secret))
    }
    pub fn revoke(&mut self, id: &str) -> Result<()> {
        let entry = self
            .entries
            .values_mut()
            .find(|s| s.id == id)
            .ok_or_else(crate::error::not_found)?;
        entry.revoked = true;
        Ok(())
    }
    pub fn inactive(&self, time: u64) -> BTreeSet<String> {
        self.entries
            .values()
            .filter(|s| s.revoked || s.grant.expires_at <= time)
            .map(|s| s.id.clone())
            .collect()
    }
}
