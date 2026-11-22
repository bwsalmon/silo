use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Code {
    InvalidRequest,
    InvalidManifest,
    Unauthenticated,
    Forbidden,
    NotFound,
    VolumeBusy,
    QuotaExceeded,
    PolicyInUse,
    PortConflict,
    UnsupportedWorker,
    LaunchFailed,
    SupervisorUnavailable,
    CleanupUnverified,
}
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Error {
    pub code: Code,
    pub message: String,
}
pub type Result<T> = std::result::Result<T, Error>;
impl Error {
    pub fn new(code: Code, message: &str) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
    pub fn exit(&self) -> i32 {
        match self.code {
            Code::InvalidRequest | Code::InvalidManifest => 2,
            Code::Unauthenticated | Code::Forbidden | Code::NotFound => 3,
            Code::VolumeBusy | Code::QuotaExceeded | Code::PolicyInUse | Code::PortConflict => 4,
            _ => 5,
        }
    }
    pub fn value(&self) -> Value {
        json!({"error": self})
    }
}
pub fn invalid() -> Error {
    Error::new(Code::InvalidRequest, "Invalid request")
}
pub fn manifest() -> Error {
    Error::new(Code::InvalidManifest, "Invalid manifest schema or policy")
}
pub fn unauthenticated() -> Error {
    Error::new(
        Code::Unauthenticated,
        "Authentication required or credential invalid",
    )
}
pub fn forbidden() -> Error {
    Error::new(Code::Forbidden, "Operation is not allowed")
}
pub fn not_found() -> Error {
    Error::new(Code::NotFound, "No visible resource with that ID or name")
}
pub fn unavailable() -> Error {
    Error::new(
        Code::SupervisorUnavailable,
        "Private supervisor unavailable",
    )
}
pub fn unsupported() -> Error {
    Error::new(Code::UnsupportedWorker,
        "Services, TCP relay and named-volume operations are unsupported in the single-image job runner")
}
