use crate::{
    auth::{Grant, Secret},
    error::{invalid, unavailable, Error, Result},
    linux,
    policy::{Manifest, MAX_DOCUMENT},
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
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
pub fn endpoint(path: &Path) -> Result<SocketAddr> {
    linux::private_dir(path.parent().ok_or_else(unavailable)?)?;
    let bytes = linux::read_private(path, 128)?;
    let addr: SocketAddr = serde_json::from_slice(&bytes).map_err(|_| unavailable())?;
    if !matches!(addr, SocketAddr::V4(a) if *a.ip() == Ipv4Addr::LOCALHOST && a.port() != 0) {
        return Err(unavailable());
    }
    Ok(addr)
}
pub fn call(path: &Path, req: Request) -> Result<Value> {
    let mut stream = TcpStream::connect_timeout(&endpoint(path)?, Duration::from_secs(2))
        .map_err(|_| unavailable())?;
    timeouts(&stream)?;
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
