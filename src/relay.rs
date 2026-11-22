//! Bounded supervisor-side TCP copying. Channels authenticate helpers, routes are immutable.
use crate::{
    error::{Code, Error, Result},
    policy::Manifest,
    relay_channel::{Channel, ACCEPT, ACK, CONNECT, CONNECTED, FAILED, PROBE},
    worker::failed,
};
use serde::{Deserialize, Serialize};
use std::{
    collections::{BTreeMap, BTreeSet},
    io::{self, Read, Write},
    net::{Ipv4Addr, Shutdown, SocketAddrV4, TcpListener, TcpStream},
    os::fd::{AsRawFd, OwnedFd},
    sync::{Arc, Mutex},
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};
pub const MAX_STREAMS: usize = 128;
pub const MAX_ENDPOINTS: usize = 256;
pub const MAX_BINDINGS: usize = 32;
pub const BUFFER: usize = 16384;
pub const KERNEL_BUFFER: i32 = 65536;
const SETUP: Duration = Duration::from_secs(3);
const IDLE: Duration = Duration::from_secs(15);
const LIFETIME: Duration = Duration::from_secs(300);

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Network {
    pub bindings: Vec<Binding>,
    pub listen: Vec<u16>,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Binding {
    pub address: Ipv4Addr,
    pub name: String,
    pub port: u16,
}
#[derive(Clone)]
pub struct Route {
    pub destination: String,
    pub listener: u32,
}
pub fn layout(
    m: &Manifest,
    caller: &str,
    services: &BTreeMap<String, String>,
) -> Result<(Network, Vec<Route>)> {
    let mut net = Network::default();
    if let Some(s) = m.services.get(caller) {
        net.listen = s.listen.iter().copied().collect();
    }
    let mut routes = Vec::new();
    for l in m.network.allow.iter().filter(|l| l.from == caller) {
        let index = m
            .services
            .keys()
            .position(|s| s == &l.to)
            .expect("validated destination")
            + 1;
        net.bindings.push(Binding {
            address: Ipv4Addr::new(127, 77, (index / 256) as u8, (index % 256) as u8),
            name: format!("{}.silo", l.to),
            port: l.port,
        });
        routes.push(Route {
            destination: services
                .get(&l.to)
                .ok_or_else(|| failed("Approved service has not been started"))?
                .clone(),
            listener: m.services[&l.to]
                .listen
                .iter()
                .position(|p| *p == l.port)
                .unwrap() as u32,
        });
    }
    if net.bindings.len() > MAX_BINDINGS || net.listen.len() > MAX_BINDINGS {
        return Err(failed(
            "At most 32 relay bindings/listeners per namespace are supported",
        ));
    }
    // A declared workload may bind wildcard addresses. Reject the ambiguous port before exec.
    if net.bindings.iter().any(|b| net.listen.contains(&b.port)) {
        return Err(Error::new(
            Code::PortConflict,
            "Workload listener port overlaps a relay binding, including wildcard binds",
        ));
    }
    Ok((net, routes))
}
impl Network {
    pub fn hosts(&self) -> String {
        let mut hosts = String::from("127.0.0.1 localhost\n::1 localhost\n");
        let mut seen = BTreeSet::new();
        for b in &self.bindings {
            if seen.insert(&b.name) {
                hosts.push_str(&format!("{} {}\n", b.address, b.name));
            }
        }
        hosts
    }
    pub fn bind(&self) -> Result<Vec<TcpListener>> {
        self.bindings
            .iter()
            .map(|b| {
                let l = TcpListener::bind(SocketAddrV4::new(b.address, b.port)).map_err(|_| {
                    Error::new(
                        Code::PortConflict,
                        "Approved private relay port conflicts with an existing/wildcard listener",
                    )
                })?;
                // SAFETY: live listening socket; cap its kernel accept backlog independently.
                if unsafe { libc::listen(l.as_raw_fd(), 16) } != 0 {
                    return Err(failed("Cannot bound relay backlog"));
                }
                l.set_nonblocking(true)
                    .map_err(|_| failed("Cannot bound relay accepts"))?;
                Ok(l)
            })
            .collect()
    }
    pub fn service(&self, channel: &Channel, listeners: &[TcpListener]) -> Result<()> {
        // Bound work per turn and socketpair queues; failed sends close the descriptor immediately.
        for (i, listener) in listeners.iter().enumerate() {
            for _ in 0..4 {
                match listener.accept() {
                    Ok((stream, _)) => {
                        configure(&stream)
                            .map_err(|_| failed("Cannot bound accepted TCP socket"))?;
                        let _ = channel.send(ACCEPT, i as u32, 0, Some(stream.as_raw_fd()));
                    }
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => break,
                    Err(_) => return Err(failed("Private relay accept failed")),
                }
            }
        }
        for _ in 0..8 {
            let Some(msg) = channel
                .receive()
                .map_err(|_| failed("Private relay channel failed"))?
            else {
                break;
            };
            if msg.kind != CONNECT || msg.fd.is_some() {
                return Err(failed("Invalid private relay connect command"));
            }
            let port = self
                .listen
                .get(msg.index as usize)
                .ok_or_else(|| failed("Unapproved workload listener"))?;
            // Only a server-configured listener on workload loopback; no request-controlled address.
            let target = SocketAddrV4::new(Ipv4Addr::LOCALHOST, *port).into();
            match TcpStream::connect_timeout(&target, Duration::from_millis(100)) {
                Ok(stream) => {
                    configure(&stream)
                        .map_err(|_| failed("Cannot bound destination TCP socket"))?;
                    let _ = channel.send(CONNECTED, msg.index, msg.token, Some(stream.as_raw_fd()));
                }
                Err(_) => {
                    let _ = channel.send(FAILED, msg.index, msg.token, None);
                }
            }
        }
        Ok(())
    }
}
fn configure(stream: &TcpStream) -> io::Result<()> {
    stream.set_nonblocking(true)?;
    stream.set_nodelay(true)?;
    for option in [libc::SO_SNDBUF, libc::SO_RCVBUF] {
        let value: i32 = KERNEL_BUFFER;
        // SAFETY: initialized integer option on a live TCP socket.
        if unsafe {
            libc::setsockopt(
                stream.as_raw_fd(),
                libc::SOL_SOCKET,
                option,
                (&value as *const i32).cast(),
                4,
            )
        } != 0
        {
            return Err(io::Error::last_os_error());
        }
    }
    Ok(())
}
fn tcp(fd: OwnedFd) -> io::Result<TcpStream> {
    let stream = TcpStream::from(fd);
    // peer_addr/local_addr reject pipes, Unix sockets and unconnected descriptors.
    if !matches!((stream.peer_addr()?, stream.local_addr()?),
        (std::net::SocketAddr::V4(p), std::net::SocketAddr::V4(l)) if p.ip().is_loopback() && l.ip().is_loopback())
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "Expected private connected TCP socket",
        ));
    }
    let mut kind = 0_i32;
    let mut len = 4;
    // SAFETY: live socket and writable integer option.
    if unsafe {
        libc::getsockopt(
            stream.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_TYPE,
            (&mut kind as *mut i32).cast(),
            &mut len,
        )
    } != 0
        || kind != libc::SOCK_STREAM
    {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "Expected TCP stream",
        ));
    }
    configure(&stream)?;
    Ok(stream)
}
#[derive(Default, Clone, Serialize)]
pub struct Stats {
    pub accepted: u64,
    pub rejected: u64,
    pub peak: usize,
    pub active: usize,
}
struct Endpoint {
    channel: Channel,
    routes: Vec<Route>,
    limit: usize,
    deadline: u64,
    probed: bool,
}
struct Direction {
    bytes: Box<[u8; BUFFER]>,
    start: usize,
    end: usize,
    eof: bool,
    closed: bool,
}
impl Direction {
    fn new() -> Self {
        Self {
            bytes: Box::new([0; BUFFER]),
            start: 0,
            end: 0,
            eof: false,
            closed: false,
        }
    }
    fn copy(&mut self, from: &mut TcpStream, to: &mut TcpStream) -> io::Result<bool> {
        let mut progress = false;
        if self.start == self.end {
            self.start = 0;
            self.end = 0;
            if !self.eof {
                match from.read(&mut self.bytes[..]) {
                    Ok(0) => self.eof = true,
                    Ok(n) => {
                        self.end = n;
                        progress = true;
                    }
                    Err(e)
                        if matches!(
                            e.kind(),
                            io::ErrorKind::WouldBlock | io::ErrorKind::Interrupted
                        ) => {}
                    Err(e) => return Err(e),
                }
            }
        }
        if self.start < self.end {
            match to.write(&self.bytes[self.start..self.end]) {
                Ok(0) => return Err(io::ErrorKind::WriteZero.into()),
                Ok(n) => {
                    self.start += n;
                    progress = true;
                }
                Err(e)
                    if matches!(
                        e.kind(),
                        io::ErrorKind::WouldBlock | io::ErrorKind::Interrupted
                    ) => {}
                Err(e) => return Err(e),
            }
        }
        if self.eof && self.start == self.end && !self.closed {
            to.shutdown(Shutdown::Write)?;
            self.closed = true;
            progress = true;
        }
        Ok(progress)
    }
}
struct Stream {
    source: String,
    destination: String,
    route: usize,
    listener: u32,
    a: TcpStream,
    b: Option<TcpStream>,
    ab: Direction,
    ba: Direction,
    born: Instant,
    progress: Instant,
}
impl Drop for Stream {
    fn drop(&mut self) {
        let _ = self.a.shutdown(Shutdown::Both);
        if let Some(b) = &self.b {
            let _ = b.shutdown(Shutdown::Both);
        }
    }
}
impl Stream {
    fn advance(&mut self) -> bool {
        let now = Instant::now();
        if now.duration_since(self.born) >= LIFETIME || now.duration_since(self.progress) >= IDLE {
            return false;
        }
        let Some(b) = &mut self.b else {
            return now.duration_since(self.born) < SETUP;
        };
        let (Ok(ab), Ok(ba)) = (self.ab.copy(&mut self.a, b), self.ba.copy(b, &mut self.a)) else {
            return false;
        };
        if ab || ba {
            self.progress = now;
        }
        !(self.ab.closed && self.ba.closed)
    }
}
#[derive(Default)]
struct State {
    endpoints: BTreeMap<String, Endpoint>,
    streams: BTreeMap<u64, Stream>,
    stats: BTreeMap<String, Stats>,
    failed: BTreeSet<String>,
    serial: u64,
    shutdown: bool,
}
impl State {
    fn remove(&mut self, id: &str) {
        self.endpoints.remove(id);
        self.streams
            .retain(|_, s| s.source != id && s.destination != id);
        self.count();
    }
    fn count(&mut self) {
        for s in self.stats.values_mut() {
            s.active = 0;
        }
        for s in self.streams.values() {
            for id in [&s.source, &s.destination] {
                let stats = self.stats.entry(id.clone()).or_default();
                stats.active += 1;
                stats.peak = stats.peak.max(stats.active);
            }
        }
    }
    fn message(&mut self, id: &str, msg: crate::relay_channel::Message) -> io::Result<()> {
        if msg.kind == PROBE {
            let e = self
                .endpoints
                .get_mut(id)
                .expect("authenticated helper channel");
            if e.probed || msg.index != 0 || msg.token != 0 {
                return Err(io::ErrorKind::PermissionDenied.into());
            }
            drop(msg.fd);
            e.channel.send(ACK, 0, 0, None)?;
            e.probed = true;
            return Ok(());
        }
        if !self.endpoints[id].probed {
            return Err(io::ErrorKind::PermissionDenied.into());
        }
        if msg.kind == ACCEPT {
            let endpoint = &self.endpoints[id];
            let route = endpoint
                .routes
                .get(msg.index as usize)
                .ok_or_else(|| io::Error::from(io::ErrorKind::PermissionDenied))?
                .clone();
            if msg.token != 0 {
                return Err(io::ErrorKind::InvalidData.into());
            }
            let a = tcp(msg.fd.unwrap())?;
            let allowed = self.streams.len() < MAX_STREAMS
                && [&id.to_string(), &route.destination].iter().all(|key| {
                    self.endpoints
                        .get(*key)
                        .map(|e| {
                            e.probed
                                && e.deadline > crate::auth::now()
                                && self.stats.get(*key).map(|s| s.active).unwrap_or(0) < e.limit
                        })
                        .unwrap_or(false)
                })
                && self
                    .streams
                    .values()
                    .filter(|s| s.source == id && s.route == msg.index as usize)
                    .count()
                    < endpoint.limit;
            if !allowed {
                self.stats.entry(id.into()).or_default().rejected += 1;
                return Ok(());
            }
            self.serial = self
                .serial
                .checked_add(1)
                .ok_or_else(|| io::Error::from(io::ErrorKind::Other))?;
            let token = self.serial;
            if self.endpoints[&route.destination]
                .channel
                .send(CONNECT, route.listener, token, None)
                .is_err()
            {
                self.stats.entry(id.into()).or_default().rejected += 1;
                return Ok(());
            }
            self.streams.insert(
                token,
                Stream {
                    source: id.into(),
                    destination: route.destination,
                    route: msg.index as usize,
                    listener: route.listener,
                    a,
                    b: None,
                    ab: Direction::new(),
                    ba: Direction::new(),
                    born: Instant::now(),
                    progress: Instant::now(),
                },
            );
            self.stats.entry(id.into()).or_default().accepted += 1;
            self.count();
        } else if matches!(msg.kind, CONNECTED | FAILED) {
            // A late setup response is harmless; its descriptor is dropped.
            if let Some(s) = self.streams.get_mut(&msg.token) {
                if s.destination != id || s.listener != msg.index || s.b.is_some() {
                    return Err(io::ErrorKind::PermissionDenied.into());
                }
                if msg.kind == FAILED {
                    self.streams.remove(&msg.token);
                } else {
                    s.b = Some(tcp(msg.fd.unwrap())?);
                }
            }
        } else {
            return Err(io::ErrorKind::PermissionDenied.into());
        }
        Ok(())
    }
    fn tick(&mut self) {
        let ids: Vec<_> = self.endpoints.keys().cloned().collect();
        for id in ids {
            if self.endpoints[&id].deadline <= crate::auth::now() {
                self.remove(&id);
                continue;
            }
            for _ in 0..8 {
                match self.endpoints[&id].channel.receive() {
                    Ok(Some(m)) => {
                        if self.message(&id, m).is_err() {
                            self.failed.insert(id.clone());
                            self.remove(&id);
                            break;
                        }
                    }
                    Ok(None) => break,
                    Err(e) => {
                        if e.kind() != io::ErrorKind::UnexpectedEof {
                            self.failed.insert(id.clone());
                        }
                        self.remove(&id);
                        break;
                    }
                }
            }
        }
        self.streams.retain(|_, s| s.advance());
        self.count();
    }
}
pub struct Relay {
    state: Arc<Mutex<State>>,
    thread: Option<JoinHandle<()>>,
}
impl Relay {
    pub fn new() -> Result<Self> {
        let state = Arc::new(Mutex::new(State::default()));
        let shared = state.clone();
        let thread = thread::Builder::new()
            .name("silo-tcp-relay".into())
            .spawn(move || loop {
                {
                    let mut s = shared.lock().expect("relay lock");
                    if s.shutdown {
                        break;
                    }
                    s.tick();
                }
                thread::sleep(Duration::from_millis(2));
            })
            .map_err(|_| failed("Cannot start bounded TCP relay"))?;
        Ok(Self {
            state,
            thread: Some(thread),
        })
    }
    pub fn register(
        &self,
        id: &str,
        channel: Channel,
        routes: Vec<Route>,
        limit: u32,
        deadline: u64,
    ) -> Result<()> {
        let mut state = self.state.lock().expect("relay lock");
        if state.endpoints.len() >= MAX_ENDPOINTS
            || state.endpoints.contains_key(id)
            || state.stats.len() >= 256
        {
            return Err(failed("Relay endpoint/history ceiling reached"));
        }
        state.stats.insert(id.into(), Stats::default());
        state.endpoints.insert(
            id.into(),
            Endpoint {
                channel,
                routes,
                limit: (limit as usize).min(MAX_STREAMS),
                deadline,
                probed: false,
            },
        );
        Ok(())
    }
    pub fn remove(&self, id: &str) {
        self.state.lock().expect("relay lock").remove(id);
    }
    pub fn failed(&self) -> Vec<String> {
        std::mem::take(&mut self.state.lock().expect("relay lock").failed)
            .into_iter()
            .collect()
    }
    pub fn stats(&self, id: &str) -> Stats {
        self.state
            .lock()
            .expect("relay lock")
            .stats
            .get(id)
            .cloned()
            .unwrap_or_default()
    }
}
impl Drop for Relay {
    fn drop(&mut self) {
        {
            let mut s = self.state.lock().expect("relay lock");
            s.shutdown = true;
            s.streams.clear();
            s.endpoints.clear();
        }
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
    }
}

#[cfg(test)]
#[path = "relay_tests.rs"]
mod tests;
