use crate::{
    auth::{now, Credentials, Grant, Principal, Secret},
    control::{self, Operation, Request, Response},
    error::{
        forbidden, invalid, not_found, unauthenticated, unavailable, unsupported, Code, Error,
        Result,
    },
    linux,
    policy::{Manifest, Permission, MAX_DOCUMENT},
};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    collections::{BTreeMap, VecDeque},
    fs,
    io::Write,
    net::TcpListener,
    path::{Path, PathBuf},
    thread,
    time::Duration,
};
use zeroize::Zeroize;

const AUDIT_CAPACITY: usize = 512;
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum State {
    Starting,
    Running,
    Stopping,
    Exited,
    Failed,
}
#[derive(Clone, Debug, Serialize)]
pub struct Instance {
    pub id: String,
    pub template: String,
    pub state: State,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
    #[serde(skip)]
    pub project: String,
    #[serde(skip)]
    pub session: String,
    #[serde(skip)]
    pub grant_id: String,
    #[serde(skip)]
    pub policy: String,
    #[serde(skip)]
    pub lease_deadline: u64,
}
impl Instance {
    fn terminal(&self) -> bool {
        matches!(self.state, State::Exited | State::Failed)
    }
}
#[derive(Clone, Debug, Serialize)]
pub struct AuditRecord {
    pub sequence: u64,
    pub time: u64,
    pub principal: String,
    pub operation: String,
    pub resource: String,
    pub allowed: bool,
    pub code: Option<Code>,
}
#[derive(Clone, Debug)]
pub struct Teardown {
    pub instance: String,
    pub reason: &'static str,
}
pub struct Supervisor {
    root: PathBuf,
    policy: Option<Manifest>,
    credentials: Credentials,
    instances: BTreeMap<String, Instance>,
    audit: VecDeque<AuditRecord>,
    sequence: u64,
    teardown: VecDeque<Teardown>,
    pub shutdown: bool,
}
impl Supervisor {
    pub fn new(root: PathBuf, operator: Secret) -> Result<Self> {
        let path = root.join("policy.json");
        let policy = if path.try_exists().map_err(|_| unavailable())? {
            Some(Manifest::parse(&linux::read_private(&path, MAX_DOCUMENT)?)?)
        } else {
            None
        };
        Ok(Self {
            root,
            policy,
            credentials: Credentials::new(operator)?,
            instances: BTreeMap::new(),
            audit: VecDeque::new(),
            sequence: 0,
            teardown: VecDeque::new(),
            shutdown: false,
        })
    }
    fn record(
        &mut self,
        principal: &str,
        operation: &str,
        resource: &str,
        result: &Result<Value>,
        time: u64,
    ) {
        if self.audit.len() == AUDIT_CAPACITY {
            self.audit.pop_front();
        }
        self.sequence += 1;
        self.audit.push_back(AuditRecord {
            sequence: self.sequence,
            time,
            principal: principal.into(),
            operation: operation.into(),
            resource: resource.into(),
            allowed: result.is_ok()
                || matches!(
                    result,
                    Err(Error {
                        code: Code::UnsupportedWorker,
                        ..
                    })
                ),
            code: result.as_ref().err().map(|e| e.code.clone()),
        });
    }
    pub fn malformed(&mut self, time: u64) {
        self.record("unknown", "invalid_request", "-", &Err(invalid()), time);
    }
    pub fn handle(&mut self, req: Request, time: u64) -> Result<Value> {
        self.tick(time);
        let principal = self.credentials.authenticate(&req.auth, time);
        let who = match &principal {
            Ok(Principal::Operator) => "operator".to_string(),
            Ok(Principal::Agent { grant, .. }) => format!("{}:{}", grant.project, grant.session),
            Err(_) => "unknown".to_string(),
        };
        let label = req.operation.label();
        // Never audit arbitrary request text, paths, unknown IDs, manifest values or credentials.
        let resource = self.audit_resource(&req.operation);
        let result = if req.version != 1 {
            Err(invalid())
        } else {
            principal.and_then(|p| self.dispatch(&p, req.operation, time))
        };
        self.record(&who, label, &resource, &result, time);
        result
    }
    fn audit_resource(&self, op: &Operation) -> String {
        let m = match &self.policy {
            Some(m) => m,
            None => return "-".into(),
        };
        match op {
            Operation::Run { template } if m.jobs.contains_key(template) => {
                m.resource_id("tpl", template)
            }
            Operation::Status { id } | Operation::Stop { id } | Operation::Logs { id }
                if self.instances.contains_key(id) =>
            {
                id.clone()
            }
            Operation::VolumeInspect { name }
            | Operation::VolumeCreate { name }
            | Operation::VolumeRm { name }
                if m.volumes.contains_key(name) =>
            {
                m.resource_id("vol", name)
            }
            Operation::ServiceInspect { name } if m.services.contains_key(name) => {
                m.resource_id("svc", name)
            }
            _ => "-".into(),
        }
    }
    fn manifest(&self) -> Result<&Manifest> {
        self.policy.as_ref().ok_or_else(forbidden)
    }
    fn dispatch(&mut self, p: &Principal, op: Operation, time: u64) -> Result<Value> {
        let operator = matches!(p, Principal::Operator);
        match op {
            Operation::Activate { manifest, no_start } => {
                if !operator {
                    return Err(forbidden());
                }
                manifest.validate()?;
                if let Some(active) = &self.policy {
                    if active.id() != manifest.id() {
                        return Err(Error::new(Code::PolicyInUse, "Activated policy is immutable; use a fresh protected state directory after verified cleanup"));
                    }
                } else {
                    // Create-only persistence under the process lock. A partial write fails closed on restart.
                    let mut file = linux::create(&self.root.join("policy.json"))?;
                    file.write_all(&manifest.canonical())
                        .and_then(|_| file.sync_all())
                        .map_err(|_| unavailable())?;
                    fs::File::open(&self.root)
                        .and_then(|f| f.sync_all())
                        .map_err(|_| unavailable())?;
                    self.policy = Some(*manifest);
                }
                if !no_start {
                    return Err(unsupported());
                }
                Ok(json!({"policy": self.manifest()?.id(), "activated": true, "started": false}))
            }
            Operation::Issue { grant } => {
                if !operator {
                    return Err(forbidden());
                }
                let m = self.manifest()?.clone();
                let (id, token) = self.credentials.issue(grant, &m, time)?;
                // Only an authenticated trusted launcher receives this response. CLI sends token to its output FD.
                Ok(json!({"grant": id, "token": token.0}))
            }
            Operation::Revoke { id } => {
                if !operator {
                    return Err(forbidden());
                }
                self.credentials.revoke(&id)?;
                self.tick(time);
                Ok(json!({"revoked": true}))
            }
            Operation::Audit => {
                if !operator {
                    return Err(forbidden());
                }
                Ok(json!({"records": self.audit, "capacity": AUDIT_CAPACITY}))
            }
            Operation::Shutdown => {
                if !operator {
                    return Err(forbidden());
                }
                if self.instances.values().any(|i| !i.terminal()) {
                    return Err(Error::new(
                        Code::CleanupUnverified,
                        "Workload teardown has not been verified",
                    ));
                }
                self.shutdown = true;
                Ok(json!({"stopped": true}))
            }
            Operation::Run { template } => {
                let m = self.manifest()?;
                if !m.jobs.contains_key(&template)
                    || (!operator && !agent(p)?.1.launch(m, &template))
                {
                    return Err(forbidden());
                }
                let live = self
                    .instances
                    .values()
                    .filter(|i| !i.terminal() && i.policy == m.id());
                if live.clone().filter(|i| i.template == template).count()
                    >= m.jobs[&template].limits.max_instances as usize
                    || match p {
                        Principal::Agent { id, grant } => {
                            live.filter(|i| &i.grant_id == id).count()
                                >= grant.scope.max_instances as usize
                        }
                        Principal::Operator => false,
                    }
                {
                    return Err(Error::new(
                        Code::QuotaExceeded,
                        "Live instance quota reached",
                    ));
                }
                // Never allocate an instance or claim launch success before enforcement exists.
                Err(unsupported())
            }
            Operation::Ps => {
                let m = self.manifest()?;
                let visible: Vec<_> = self
                    .instances
                    .values()
                    .filter(|i| operator || self.visible(p, m, i, Permission::List))
                    .collect();
                Ok(json!({"instances": visible}))
            }
            Operation::Status { id } => self.instance_operation(p, &id, Permission::Status),
            Operation::Logs { id } => self.instance_operation(p, &id, Permission::Logs),
            Operation::Stop { id } => self.instance_operation(p, &id, Permission::Stop),
            Operation::VolumeList => {
                let m = self.manifest()?;
                let values: Vec<_> = m
                    .volumes
                    .keys()
                    .filter(|n| {
                        operator || self.metadata_visible(p, m, n, Permission::VolumeList, true)
                    })
                    .map(|n| json!({"id": m.resource_id("vol", n), "name": n, "created": false}))
                    .collect();
                Ok(json!({"volumes": values}))
            }
            Operation::VolumeInspect { name } => {
                let m = self.manifest()?;
                if !m.volumes.contains_key(&name)
                    || (!operator
                        && !self.metadata_visible(p, m, &name, Permission::VolumeInspect, true))
                {
                    return Err(not_found());
                }
                Ok(json!({"id": m.resource_id("vol", &name), "name": name, "created": false}))
            }
            Operation::VolumeCreate { name } | Operation::VolumeRm { name } => {
                if !operator {
                    return Err(forbidden());
                }
                if !self.manifest()?.volumes.contains_key(&name) {
                    return Err(not_found());
                }
                Err(unsupported())
            }
            Operation::ServiceList => {
                let m = self.manifest()?;
                let values: Vec<_> = m
                    .services
                    .keys()
                    .filter(|n| {
                        operator || self.metadata_visible(p, m, n, Permission::ServiceList, false)
                    })
                    .map(|n| json!({"id": m.resource_id("svc", n), "name": n, "started": false}))
                    .collect();
                Ok(json!({"services": values}))
            }
            Operation::ServiceInspect { name } => {
                let m = self.manifest()?;
                if !m.services.contains_key(&name)
                    || (!operator
                        && !self.metadata_visible(p, m, &name, Permission::ServiceInspect, false))
                {
                    return Err(not_found());
                }
                Ok(json!({"id": m.resource_id("svc", &name), "name": name, "started": false}))
            }
        }
    }
    fn metadata_visible(
        &self,
        p: &Principal,
        m: &Manifest,
        name: &str,
        permission: Permission,
        volume: bool,
    ) -> bool {
        let g = match agent(p) {
            Ok((_, g)) => g,
            Err(_) => return false,
        };
        let (scoped, allowed) = if volume {
            (
                g.scope.volumes.contains(name),
                m.volumes.get(name).map(|v| &v.agent),
            )
        } else {
            (
                g.scope.services.contains(name),
                m.services.get(name).map(|v| &v.agent),
            )
        };
        g.project == m.project
            && g.policy == m.id()
            && scoped
            && g.scope.permissions.contains(&permission)
            && allowed.map(|a| a.contains(&permission)).unwrap_or(false)
    }
    fn visible(&self, p: &Principal, m: &Manifest, i: &Instance, permission: Permission) -> bool {
        match agent(p) {
            Ok((id, g)) => {
                i.grant_id == id
                    && i.session == g.session
                    && i.project == g.project
                    && i.policy == g.policy
                    && g.permits(m, permission, &i.template)
            }
            _ => false,
        }
    }
    fn instance_operation(
        &mut self,
        p: &Principal,
        id: &str,
        permission: Permission,
    ) -> Result<Value> {
        let m = self.manifest()?;
        let i = self
            .instances
            .get(id)
            .filter(|i| {
                matches!(p, Principal::Operator) || self.visible(p, m, i, permission.clone())
            })
            .ok_or_else(not_found)?;
        if permission == Permission::Logs {
            return Err(unsupported());
        }
        if permission == Permission::Stop && !i.terminal() {
            self.request_teardown(id, "stop");
        }
        Ok(serde_json::to_value(&self.instances[id]).expect("typed instance serializes"))
    }
    fn request_teardown(&mut self, id: &str, reason: &'static str) {
        if let Some(i) = self.instances.get_mut(id) {
            if !i.terminal() && i.state != State::Stopping {
                i.state = State::Stopping;
                self.teardown.push_back(Teardown {
                    instance: id.into(),
                    reason,
                });
            }
        }
    }
    // Runtime must consume these requests, close links and kill/reap all processes.
    // Scheduling teardown does not release mounts or report a terminal state.
    pub fn take_teardown(&mut self) -> Vec<Teardown> {
        self.teardown.drain(..).collect()
    }
    pub fn tick(&mut self, time: u64) {
        let inactive = self.credentials.inactive(time);
        let due: Vec<_> = self
            .instances
            .values()
            .filter(|i| {
                !i.terminal() && (i.lease_deadline <= time || inactive.contains(&i.grant_id))
            })
            .map(|i| i.id.clone())
            .collect();
        for id in due {
            self.request_teardown(&id, "lease_or_revocation");
        }
    }
}
fn agent(p: &Principal) -> Result<(&str, &Grant)> {
    match p {
        Principal::Agent { id, grant } => Ok((id, grant)),
        _ => Err(unauthenticated()),
    }
}
pub fn serve(root: &Path, operator: Secret) -> Result<()> {
    let root = linux::private_dir(root)?;
    let _lock = linux::lock(&root)?;
    let mut supervisor = Supervisor::new(root.clone(), operator)?;
    let listener = TcpListener::bind(control::loopback()).map_err(|_| {
        Error::new(
            Code::UnsupportedWorker,
            "Worker cannot bind private loopback TCP control transport",
        )
    })?;
    listener.set_nonblocking(true).map_err(|_| unavailable())?;
    let endpoint = root.join("endpoint.json");
    let mut file = linux::create(&endpoint)?; // Refuse stale endpoints; never reattach after loss.
    file.write_all(
        &serde_json::to_vec(&listener.local_addr().map_err(|_| unavailable())?)
            .map_err(|_| unavailable())?,
    )
    .and_then(|_| file.sync_all())
    .map_err(|_| unavailable())?;
    while !supervisor.shutdown {
        supervisor.tick(now());
        match listener.accept() {
            Ok((mut stream, peer)) => {
                if !peer.ip().is_loopback() || control::timeouts(&stream).is_err() {
                    continue;
                }
                handle_connection(&mut supervisor, &mut stream);
            }
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                thread::sleep(Duration::from_millis(20))
            }
            Err(_) => return Err(unavailable()),
        }
    }
    fs::remove_file(endpoint).map_err(|_| unavailable())?;
    Ok(())
}

fn handle_connection(supervisor: &mut Supervisor, stream: &mut std::net::TcpStream) {
    let mut response = match control::read_frame(stream) {
        Ok(mut bytes) => {
            let request = serde_json::from_slice::<Request>(&bytes);
            bytes.zeroize();
            match request {
                Ok(req) => Response::from(supervisor.handle(req, now())),
                Err(_) => {
                    supervisor.malformed(now());
                    Response::from(Err(invalid()))
                }
            }
        }
        Err(e) => {
            supervisor.malformed(now());
            Response::from(Err(e))
        }
    };
    let _ = control::write_frame(stream, &response);
    response.clear_secret();
}

#[cfg(test)]
#[path = "supervisor_tests.rs"]
mod tests;
