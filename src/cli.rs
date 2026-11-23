use crate::{
    auth::Grant,
    control::{self, Operation, Request},
    error::{invalid, Result},
    linux,
    policy::Manifest,
    supervisor,
};
use serde_json::{json, Value};
use std::{
    io::Write,
    path::{Path, PathBuf},
};
use zeroize::Zeroize;

pub const HELP: &str = "silo: authenticated rootless jobs, private services and managed volumes
MCP: mcp serve --endpoint /absolute/private/endpoint.json --auth-fd FD\nLocal: validate MANIFEST
Control: up MANIFEST [--no-start] | run TEMPLATE | ps | status ID | logs ID | stop ID
Metadata: volume list|inspect NAME; service list|inspect NAME
Managed storage: volume create|rm|recover NAME
Trusted launcher: supervisor --state-dir DIR --auth-fd FD
Trusted operator: operator issue GRANT --token-fd FD | operator revoke ID | operator audit | operator shutdown
Control commands require --endpoint /absolute/private/endpoint.json --auth-fd FD
Output: --json (one JSON value, including errors)
Jobs require a qualified worker and the static musl supervisor; services and managed volumes require qualified protected storage.
See docs/runner.md and docs/foundation.md for qualification, schema and transport.";

pub enum Command {
    Help,
    Mcp,
    Validate(PathBuf),
    Serve(PathBuf),
    Remote(Operation),
    Up(PathBuf, bool),
    Issue(PathBuf),
    Helper,
}
pub struct Args {
    pub json: bool,
    pub auth_fd: Option<i32>,
    pub token_fd: Option<i32>,
    pub endpoint: Option<PathBuf>,
    pub command: Command,
}
pub fn parse(args: &[String]) -> Result<Args> {
    let mut json = false;
    let mut auth_fd = None;
    let mut token_fd = None;
    let mut endpoint = None;
    let mut state_dir = None;
    let mut no_start = false;
    let mut positional = Vec::new();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--json" if !json => json = true,
            "--no-start" if !no_start => no_start = true,
            "--auth-fd" | "--token-fd" | "--endpoint" | "--state-dir" => {
                let key = args[i].as_str();
                i += 1;
                let value = args.get(i).ok_or_else(invalid)?;
                match key {
                    "--auth-fd" if auth_fd.is_none() => auth_fd = Some(parse_fd(value)?),
                    "--token-fd" if token_fd.is_none() => token_fd = Some(parse_fd(value)?),
                    "--endpoint" if endpoint.is_none() => endpoint = Some(PathBuf::from(value)),
                    "--state-dir" if state_dir.is_none() => state_dir = Some(PathBuf::from(value)),
                    _ => return Err(invalid()),
                }
            }
            "-h" | "--help" if args.len() == 1 => {
                return Ok(Args {
                    json,
                    auth_fd,
                    token_fd,
                    endpoint,
                    command: Command::Help,
                })
            }
            v if v.starts_with('-') => return Err(invalid()),
            v => positional.push(v),
        }
        i += 1;
    }
    let command = match positional.as_slice() {
        ["mcp", "serve"] => Command::Mcp,
        ["validate", path] => Command::Validate(PathBuf::from(path)),
        ["up", path] => Command::Up(PathBuf::from(path), no_start),
        ["supervisor"] => Command::Serve(state_dir.clone().ok_or_else(invalid)?),
        ["__namespace_helper"] => Command::Helper,
        ["run", template] => Command::Remote(Operation::Run {
            template: (*template).into(),
        }),
        ["ps"] => Command::Remote(Operation::Ps),
        ["status", id] => Command::Remote(Operation::Status { id: (*id).into() }),
        ["logs", id] => Command::Remote(Operation::Logs { id: (*id).into() }),
        ["stop", id] => Command::Remote(Operation::Stop { id: (*id).into() }),
        ["volume", "list"] => Command::Remote(Operation::VolumeList),
        ["volume", "inspect", name] => Command::Remote(Operation::VolumeInspect {
            name: (*name).into(),
        }),
        ["volume", "create", name] => Command::Remote(Operation::VolumeCreate {
            name: (*name).into(),
        }),
        ["volume", "recover", name] => Command::Remote(Operation::VolumeRecover {
            name: (*name).into(),
        }),
        ["volume", "rm", name] => Command::Remote(Operation::VolumeRm {
            name: (*name).into(),
        }),
        ["service", "list"] => Command::Remote(Operation::ServiceList),
        ["service", "inspect", name] => Command::Remote(Operation::ServiceInspect {
            name: (*name).into(),
        }),
        ["operator", "issue", path] => Command::Issue(PathBuf::from(path)),
        ["operator", "revoke", id] => Command::Remote(Operation::Revoke { id: (*id).into() }),
        ["operator", "audit"] => Command::Remote(Operation::Audit),
        ["operator", "shutdown"] => Command::Remote(Operation::Shutdown),
        _ => return Err(invalid()),
    };
    let remote = matches!(
        command,
        Command::Mcp | Command::Remote(_) | Command::Up(..) | Command::Issue(_)
    );
    if (matches!(command, Command::Mcp) && (json || auth_fd.is_none()))
        || (remote && endpoint.is_none())
        || (!remote && endpoint.is_some())
        || (state_dir.is_some() && !matches!(command, Command::Serve(_)))
        || (no_start && !matches!(command, Command::Up(..)))
        || (token_fd.is_some() != matches!(command, Command::Issue(_)))
        || (token_fd.is_some() && token_fd == auth_fd)
        || (!remote && !matches!(command, Command::Serve(_)) && auth_fd.is_some())
    {
        return Err(invalid());
    }
    Ok(Args {
        json,
        auth_fd,
        token_fd,
        endpoint,
        command,
    })
}
fn parse_fd(s: &str) -> Result<i32> {
    let fd = s.parse::<i32>().map_err(|_| invalid())?;
    if fd < 3 {
        return Err(invalid());
    }
    Ok(fd)
}
fn local_manifest(path: &Path) -> Result<Manifest> {
    Manifest::parse(&linux::operator_manifest(path)?)
}
pub fn execute(args: Args) -> Result<Value> {
    match args.command {
        Command::Help => Ok(json!({"help": HELP})),
        Command::Mcp => {
            crate::mcp::serve(
                args.endpoint.as_deref().ok_or_else(invalid)?,
                args.auth_fd.ok_or_else(invalid)?,
            )?;
            // Tokio stdin can have an outstanding blocking read after protocol shutdown.
            // All service/credential owners have dropped before this process exit.
            std::process::exit(0);
        }
        Command::Helper => Err(crate::error::unsupported()),
        Command::Validate(path) => {
            let m = local_manifest(&path)?;
            Ok(json!({"valid": true, "policy": m.id()}))
        }
        Command::Serve(path) => {
            let token = linux::credential(args.auth_fd.ok_or_else(crate::error::unauthenticated)?)?;
            supervisor::serve(&path, token)?;
            Ok(json!({"stopped": true}))
        }
        command => {
            // Read and close credentials before any operation or future child launch.
            let auth = linux::credential(args.auth_fd.ok_or_else(crate::error::unauthenticated)?)?;
            let operation = match command {
                Command::Remote(op) => op,
                Command::Up(path, no_start) => Operation::Activate {
                    manifest: Box::new(local_manifest(&path)?),
                    no_start,
                },
                Command::Issue(path) => {
                    let data = linux::operator_manifest(&path)?;
                    let grant: Grant = serde_json::from_slice(&data).map_err(|_| invalid())?;
                    Operation::Issue { grant }
                }
                _ => unreachable!(),
            };
            let mut result = control::call(
                &args.endpoint.ok_or_else(invalid)?,
                Request {
                    version: 1,
                    auth,
                    operation,
                },
            )?;
            if let Some(fd) = args.token_fd {
                let mut secret = match result.as_object_mut().ok_or_else(invalid)?.remove("token") {
                    Some(Value::String(value)) => value,
                    _ => return Err(invalid()),
                };
                let output = linux::take_fd(fd).and_then(|mut f| {
                    f.write_all(secret.as_bytes())
                        .and_then(|_| f.write_all(b"\n"))
                        .and_then(|_| f.flush())
                        .map_err(|_| invalid())
                });
                secret.zeroize();
                output?;
            }
            Ok(result)
        }
    }
}
