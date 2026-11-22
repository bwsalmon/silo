//! Single-image job runtime. Resource reservations survive stopping and uncertain cleanup.
use crate::{
    auth::now,
    error::{Code, Error, Result},
    helper::{Action, Event, Spec},
    image::{self, Prepared},
    policy::Template,
    process::{self, Context},
    worker::{failed, missing, Worker},
};
use serde_json::{json, Value};
use std::{
    collections::{BTreeMap, VecDeque},
    fs,
    path::Path,
    sync::mpsc::Receiver,
    time::Duration,
};

struct Running {
    context: Context,
    receiver: Option<Receiver<Event>>,
    stopping: bool,
    setup_failed: bool,
    result: Option<(Option<i32>, String, String)>,
}
pub struct Completion {
    pub id: String,
    pub code: Option<i32>,
    pub failed: bool,
}
pub struct Runner {
    pub worker: Worker,
    _storage_lock: fs::File,
    images: BTreeMap<String, Prepared>,
    relay: crate::relay::Relay,
    live: BTreeMap<String, Running>,
    logs: BTreeMap<String, Value>,
    completed: VecDeque<Completion>,
}
impl Runner {
    pub fn new(state: &Path) -> Result<Self> {
        let worker = Worker::load(state)?;
        crate::trampoline::require_static()?;
        let storage_lock = crate::linux::lock(&worker.storage)?;
        Ok(Self {
            worker,
            _storage_lock: storage_lock,
            images: BTreeMap::new(),
            relay: crate::relay::Relay::new()?,
            live: BTreeMap::new(),
            logs: BTreeMap::new(),
            completed: VecDeque::new(),
        })
    }
    pub fn check(&self, t: &Template) -> Result<()> {
        if !t.mounts.is_empty() {
            return Err(missing(
                "Named-volume mounts are unsupported in the single-image runner",
            ));
        }
        if t.uid != 0 || t.gid != 0 {
            return Err(failed(
                "Unsupported template identity: only numeric 0:0 is mapped",
            ));
        }
        self.worker.check_limits(&t.limits)?;
        if self.live.values().any(|r| r.stopping || r.setup_failed) {
            return Err(Error::new(
                Code::CleanupUnverified,
                "Previous whole-cgroup cleanup is still pending",
            ));
        }
        if self.live.len() >= self.worker.ceilings.max_instances as usize {
            return Err(Error::new(
                Code::QuotaExceeded,
                "Qualified worker instance ceiling reached",
            ));
        }
        if self.logs.len() + self.live.len() >= 256 {
            return Err(Error::new(
                Code::QuotaExceeded,
                "Bounded runtime history is full; use a fresh worker after verified shutdown",
            ));
        }
        Ok(())
    }
    pub fn start(
        &mut self,
        id: &str,
        t: &Template,
        deadline: u64,
        network: crate::relay::Network,
        routes: Vec<crate::relay::Route>,
    ) -> Result<String> {
        self.check(t)?;
        let group = self.worker.group(id, &t.limits)?;
        self.live.insert(
            id.into(),
            Running {
                context: Context { group, child: None },
                receiver: None,
                stopping: false,
                setup_failed: false,
                result: None,
            },
        );
        let result = self.setup(id, t, deadline, network, routes);
        if result.is_err() {
            let r = self.live.get_mut(id).expect("reserved instance");
            r.setup_failed = true;
            r.stopping = true;
            // poll retries failures and keeps the reservation until cgroup emptiness is verified.
            self.poll();
        }
        result
    }
    fn setup(
        &mut self,
        id: &str,
        t: &Template,
        deadline: u64,
        network: crate::relay::Network,
        routes: Vec<crate::relay::Route>,
    ) -> Result<String> {
        let running = self.live.get_mut(id).expect("reserved instance");
        running.context.probe()?;
        let prepared = match self.images.get(&t.image) {
            Some(p) => p.clone(),
            None => {
                let dir = self
                    .worker
                    .storage
                    .join(format!("img_{}", crate::policy::digest(t.image.as_bytes())));
                let result = image::prepare(&self.worker, &mut running.context, &t.image, &dir);
                if result.is_err() && running.context.cleanup().is_ok() {
                    let _ = fs::remove_dir_all(&dir);
                }
                let p = result?;
                self.images.insert(t.image.clone(), p.clone());
                p
            }
        };
        let execution = image::resolve_execution(&prepared, t)?;
        for input in &t.executable_inputs {
            image::immutable_path(&prepared.rootfs, input)?;
        }
        for target in ["dev", "proc", "tmp", "run", "etc"] {
            let path = prepared.rootfs.join(target);
            if !path.exists() {
                fs::create_dir(&path)
                    .map_err(|_| failed("Cannot prepare fixed image mountpoint"))?;
            }
            if !fs::symlink_metadata(&path)
                .map_err(|_| failed("Missing image mountpoint"))?
                .is_dir()
            {
                return Err(failed(
                    "Image pseudo-filesystem/scratch mountpoint must be a real directory",
                ));
            }
        }
        let hosts_target = prepared.rootfs.join("etc/hosts");
        match fs::symlink_metadata(&hosts_target) {
            Ok(m) if m.is_file() => (),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                fs::OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .open(&hosts_target)
                    .map_err(|_| failed("Cannot prepare generated hosts mountpoint"))?;
            }
            _ => {
                return Err(failed(
                    "Image /etc/hosts mountpoint must be a real regular file",
                ))
            }
        }
        let remaining = deadline.saturating_sub(now());
        if remaining == 0 {
            return Err(failed("Launch lease expired during image preparation"));
        }
        let (supervisor_channel, helper_channel) = crate::relay_channel::Channel::pair()
            .map_err(|_| missing("Worker denies private SCM_RIGHTS socketpair"))?;
        running.context.child = Some(process::spawn_with_channel(
            &Spec {
                cgroup: running.context.group.path.clone(),
                action: Action::Workload {
                    bwrap: self.worker.bwrap.clone(),
                    rootfs: prepared.rootfs.clone(),
                    argv: execution.argv,
                    env: execution.env,
                    workdir: execution.workdir,
                    scratch_bytes: t.limits.scratch_bytes,
                    deadline_unix_ms: deadline * 1000,
                    network,
                },
            },
            Some(&helper_channel),
        )?);
        drop(helper_channel);
        self.relay.register(
            id,
            supervisor_channel,
            routes,
            t.limits.connections,
            deadline,
        )?;
        let receiver = process::events(running.context.child.as_mut().expect("workload helper"));
        let result = match receiver.recv_timeout(Duration::from_secs(10)) {
            Ok(Event::Ready) => Ok(prepared.digest),
            Ok(Event::Error { error }) => Err(error),
            _ => Err(missing(
                "Trusted helper did not confirm Bubblewrap isolation setup",
            )),
        };
        running.receiver = Some(receiver);
        result
    }
    pub fn stop(&mut self, id: &str) {
        self.relay.remove(id);
        if let Some(r) = self.live.get_mut(id) {
            r.stopping = true;
            let _ = r.context.group.kill();
            if let Some(child) = &mut r.context.child {
                let _ = child.kill();
            }
        }
    }
    pub fn poll(&mut self) {
        for id in self.relay.failed() {
            if let Some(r) = self.live.get_mut(&id) {
                r.setup_failed = true;
            }
            self.stop(&id);
        }
        let mut done = Vec::new();
        for (id, running) in &mut self.live {
            if let Some(receiver) = &running.receiver {
                while let Ok(event) = receiver.try_recv() {
                    match event {
                        Event::Exit {
                            code,
                            stdout,
                            stderr,
                        } => {
                            running.result = Some((code, stdout, stderr));
                        }
                        Event::Error { .. } => running.setup_failed = true,
                        Event::Ready => (),
                    }
                }
            }
            let child_gone = running
                .context
                .child
                .as_mut()
                .map(|c| c.try_wait().map(|s| s.is_some()).unwrap_or(false))
                .unwrap_or(true);
            if running.stopping || running.result.is_some() || child_gone {
                // A reader may be delivering the final event just after waitpid observes exit.
                if child_gone && running.result.is_none() && !running.stopping {
                    if let Some(receiver) = &running.receiver {
                        if let Ok(Event::Exit {
                            code,
                            stdout,
                            stderr,
                        }) = receiver.recv_timeout(Duration::from_millis(100))
                        {
                            running.result = Some((code, stdout, stderr));
                        }
                    }
                }
                self.relay.remove(id);
                let resources = json!({
                    "memory_events": fs::read_to_string(running.context.group.path.join("memory.events")).unwrap_or_default(),
                    "pids_events": fs::read_to_string(running.context.group.path.join("pids.events")).unwrap_or_default(),
                    "cpu_stat": fs::read_to_string(running.context.group.path.join("cpu.stat")).unwrap_or_default(),
                });
                if running.context.cleanup().is_ok() && running.context.group.remove().is_ok() {
                    let verified_exit = running.result.is_some();
                    let (code, stdout, stderr) = running.result.take().unwrap_or_default();
                    self.logs.insert(
                        id.clone(),
                        json!({
                            "id": id, "stdout": stdout, "stderr": stderr, "complete": true,
                            "limit_bytes_per_stream": crate::helper::OUTPUT_LIMIT,
                        "resources": resources,
                        "relay": self.relay.stats(id),
                        "capture": if verified_exit { "bounded" } else { "unavailable_after_helper_loss" },
                        }),
                    );
                    self.completed.push_back(Completion {
                        id: id.clone(),
                        code,
                        failed: running.setup_failed || !running.stopping && !verified_exit,
                    });
                    done.push(id.clone());
                } else {
                    running.stopping = true;
                }
            }
        }
        for id in done {
            self.live.remove(&id);
        }
    }
    pub fn stopping(&self) -> Vec<String> {
        self.live
            .iter()
            .filter(|(_, r)| r.stopping)
            .map(|(id, _)| id.clone())
            .collect()
    }
    pub fn completions(&mut self) -> Vec<Completion> {
        self.completed.drain(..).collect()
    }
    pub fn logs(&self, id: &str) -> Value {
        self.logs.get(id).cloned().unwrap_or_else(|| {
            json!({
                "id": id, "stdout": "", "stderr": "", "complete": false,
                "limit_bytes_per_stream": crate::helper::OUTPUT_LIMIT,
            })
        })
    }
}
impl Drop for Runner {
    fn drop(&mut self) {
        for (id, running) in &mut self.live {
            self.relay.remove(id);
            let _ = running.context.cleanup();
        }
    }
}
