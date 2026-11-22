use crate::{
    auth::{random_id, Grant, Secret},
    error::{invalid, unavailable, Error, Result},
    linux,
    policy::{Manifest, MAX_DOCUMENT},
};
use hmac::{Hmac, Mac};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::Sha256;
use std::{
    io::{Read, Write},
    net::{Ipv4Addr, SocketAddr, SocketAddrV4, TcpStream},
    path::Path,
    time::Duration,
};
use zeroize::Zeroize;

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Request {
    pub version: u32,
    pub auth: Secret,
    pub operation: Operation,
}
#[derive(Clone, Deserialize, Serialize)]
#[serde(tag = "op", rename_all = "snake_case", deny_unknown_fields)]
pub enum Operation {
    Activate {
        manifest: Box<Manifest>,
        no_start: bool,
    },
    Run {
        template: String,
    },
    Ps,
    Status {
        id: String,
    },
    Logs {
        id: String,
    },
    Stop {
        id: String,
    },
    VolumeList,
    VolumeInspect {
        name: String,
    },
    VolumeCreate {
        name: String,
    },
    VolumeRm {
        name: String,
    },
    ServiceList,
    ServiceInspect {
        name: String,
    },
    Issue {
        grant: Grant,
    },
    Revoke {
        id: String,
    },
    Audit,
    Shutdown,
}
impl Operation {
    pub fn label(&self) -> &'static str {
        match self {
            Self::Activate { .. } => "activate",
            Self::Run { .. } => "run",
            Self::Ps => "list",
            Self::Status { .. } => "status",
            Self::Logs { .. } => "logs",
            Self::Stop { .. } => "stop",
            Self::VolumeList => "volume_list",
            Self::VolumeInspect { .. } => "volume_inspect",
            Self::VolumeCreate { .. } => "volume_create",
            Self::VolumeRm { .. } => "volume_rm",
            Self::ServiceList => "service_list",
            Self::ServiceInspect { .. } => "service_inspect",
            Self::Issue { .. } => "issue",
            Self::Revoke { .. } => "revoke",
            Self::Audit => "audit",
            Self::Shutdown => "shutdown",
        }
    }
}
#[derive(Deserialize, Serialize)]
#[serde(untagged)]
pub enum Response {
    Failure { error: Error },
    Success(Value),
}
impl Response {
    pub fn clear_secret(&mut self) {
        if let Self::Success(value) = self {
            if let Some(Value::String(token)) = value.get_mut("token") {
                token.zeroize();
            }
        }
    }
    pub fn result(self) -> Result<Value> {
        match self {
            Self::Failure { error } => Err(error),
            Self::Success(v) => Ok(v),
        }
    }
}
impl From<Result<Value>> for Response {
    fn from(r: Result<Value>) -> Self {
        match r {
            Ok(v) => Self::Success(v),
            Err(error) => Self::Failure { error },
        }
    }
}
pub fn timeouts(s: &TcpStream) -> Result<()> {
    s.set_read_timeout(Some(Duration::from_secs(2)))
        .map_err(|_| unavailable())?;
    s.set_write_timeout(Some(Duration::from_secs(2)))
        .map_err(|_| unavailable())
}
pub fn read_frame(reader: &mut impl Read) -> Result<Vec<u8>> {
    let mut header = [0u8; 4];
    reader.read_exact(&mut header).map_err(|_| invalid())?;
    let len = u32::from_be_bytes(header) as usize;
    if len == 0 || len > MAX_DOCUMENT {
        return Err(invalid());
    }
    let mut buf = vec![0; len];
    if reader.read_exact(&mut buf).is_err() {
        buf.zeroize();
        return Err(invalid());
    }
    Ok(buf)
}
pub fn write_frame(writer: &mut impl Write, value: &impl Serialize) -> Result<()> {
    let mut buf = serde_json::to_vec(value).map_err(|_| invalid())?;
    if buf.len() > MAX_DOCUMENT {
        buf.zeroize();
        return Err(invalid());
    }
    let result = writer
        .write_all(&(buf.len() as u32).to_be_bytes())
        .and_then(|_| writer.write_all(&buf))
        .map_err(|_| unavailable());
    buf.zeroize();
    result
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Endpoint {
    pub address: SocketAddr,
    // Per-start server identity, shared only with the protected client/broker.
    // It never authenticates an operator or grants access to a resource.
    pub server_key: Secret,
}
impl Endpoint {
    fn load(path: &Path) -> Result<Self> {
        linux::private_dir(path.parent().ok_or_else(unavailable)?)?;
        let bytes = zeroize::Zeroizing::new(linux::read_private(path, 512)?);
        let endpoint: Self = serde_json::from_slice(&bytes).map_err(|_| unavailable())?;
        if !matches!(endpoint.address, SocketAddr::V4(a) if *a.ip() == Ipv4Addr::LOCALHOST && a.port() != 0)
            || !endpoint.server_key.valid()
        {
            return Err(unavailable());
        }
        Ok(endpoint)
    }
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Challenge {
    nonce: String,
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct ServerProof {
    proof: [u8; 32],
}
fn server_mac(key: &Secret, nonce: &str) -> Hmac<Sha256> {
    let mut mac = Hmac::<Sha256>::new_from_slice(key.0.as_bytes()).expect("HMAC accepts any key");
    mac.update(b"silo-control-server-v1\0");
    mac.update(nonce.as_bytes());
    mac
}
// Authenticate the server before sending any bearer credential. Fresh challenges
// prevent replay if a different local principal takes over a stale endpoint port.
pub fn verify_server(stream: &mut TcpStream, key: &Secret) -> Result<()> {
    let challenge = Challenge {
        nonce: random_id("")?,
    };
    write_frame(stream, &challenge)?;
    let bytes = read_frame(stream).map_err(|_| unavailable())?;
    let proof: ServerProof = serde_json::from_slice(&bytes).map_err(|_| unavailable())?;
    server_mac(key, &challenge.nonce)
        .verify_slice(&proof.proof)
        .map_err(|_| unavailable())
}
pub fn prove_server(stream: &mut TcpStream, key: &Secret) -> Result<()> {
    let bytes = read_frame(stream)?;
    let challenge: Challenge = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
    if challenge.nonce.len() != 64 || !challenge.nonce.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(invalid());
    }
    let proof = server_mac(key, &challenge.nonce)
        .finalize()
        .into_bytes()
        .into();
    write_frame(stream, &ServerProof { proof })
}
pub fn endpoint(path: &Path) -> Result<SocketAddr> {
    Ok(Endpoint::load(path)?.address)
}
pub fn connect(path: &Path) -> Result<TcpStream> {
    let endpoint = Endpoint::load(path)?;
    let mut stream = TcpStream::connect_timeout(&endpoint.address, Duration::from_secs(2))
        .map_err(|_| unavailable())?;
    timeouts(&stream)?;
    verify_server(&mut stream, &endpoint.server_key)?;
    Ok(stream)
}
pub fn call(path: &Path, req: Request) -> Result<Value> {
    let mut stream = connect(path)?;
    write_frame(&mut stream, &req)?;
    drop(req); // Zeroize token before waiting for a reply.
    let mut buf = read_frame(&mut stream).map_err(|_| unavailable())?;
    let response = serde_json::from_slice::<Response>(&buf).map_err(|_| unavailable());
    buf.zeroize();
    response?.result()
}
pub fn loopback() -> SocketAddr {
    SocketAddrV4::new(Ipv4Addr::LOCALHOST, 0).into()
}
