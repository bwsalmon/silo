//! Real binary and loopback protocol tests in disposable task-owned directories.
//! These are control-plane tests, not runtime qualification.
use serde_json::{json, Value};
use silo::{
    auth::{now, Secret},
    control::{self, Operation, Request},
    policy::{Manifest, MAX_DOCUMENT},
};
use std::{
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    net::TcpStream,
    os::{
        fd::{AsRawFd, FromRawFd},
        unix::{
            fs::{OpenOptionsExt, PermissionsExt},
            process::CommandExt,
        },
    },
    path::{Path, PathBuf},
    process::{Child, Command, Output, Stdio},
    thread,
    time::{Duration, Instant},
};
use tempfile::TempDir;

struct Harness {
    dir: TempDir,
    process: Child,
    endpoint: PathBuf,
    operator: PathBuf,
    manifest: PathBuf,
}
fn protected(path: &Path, bytes: &[u8]) {
    let mut f = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
        .unwrap();
    f.write_all(bytes).unwrap();
}
fn launch(args: &[String], credential: &Path, output: Option<&Path>) -> Command {
    let input = File::open(credential).unwrap();
    let output = output.map(|p| {
        OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(p)
            .unwrap()
    });
    // Duplicate above the fixed child descriptors so dup2 cannot clobber either source.
    let dup = |file: &File| {
        // SAFETY: file is live; duplicate is uniquely owned below.
        let fd = unsafe { libc::fcntl(file.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 20) };
        assert!(fd >= 20);
        // SAFETY: fcntl returned a new descriptor owned by this File.
        unsafe { File::from_raw_fd(fd) }
    };
    let input = dup(&input);
    let output = output.as_ref().map(dup);
    let mut cmd = Command::new(env!("CARGO_BIN_EXE_silo"));
    cmd.args(args)
        .args(["--auth-fd", "3"])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    // SAFETY: the closure uses only async-signal-safe descriptor operations between fork and exec.
    unsafe {
        cmd.pre_exec(move || {
            if libc::dup2(input.as_raw_fd(), 3) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            if let Some(f) = &output {
                if libc::dup2(f.as_raw_fd(), 4) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
            }
            Ok(())
        });
    }
    cmd
}
impl Harness {
    fn start() -> Self {
        let dir = tempfile::tempdir().unwrap();
        fs::set_permissions(dir.path(), fs::Permissions::from_mode(0o700)).unwrap();
        let root = fs::canonicalize(dir.path()).unwrap();
        let operator = root.join("operator-token");
        protected(&operator, "a".repeat(64).as_bytes());
        let manifest = root.join("manifest.yaml");
        protected(&manifest, include_bytes!("../examples/foundation.yaml"));
        let endpoint = root.join("endpoint.json");
        let process = launch(
            &[
                "supervisor".into(),
                "--state-dir".into(),
                root.to_str().unwrap().into(),
            ],
            &operator,
            None,
        )
        .spawn()
        .unwrap();
        let mut h = Self {
            dir,
            process,
            endpoint,
            operator,
            manifest,
        };
        let deadline = Instant::now() + Duration::from_secs(5);
        while !h.endpoint.exists() {
            if let Some(status) = h.process.try_wait().unwrap() {
                let mut diagnostics = String::new();
                h.process
                    .stderr
                    .take()
                    .unwrap()
                    .read_to_string(&mut diagnostics)
                    .unwrap();
                panic!("supervisor exited before startup: {status}: {diagnostics}");
            }
            assert!(Instant::now() < deadline, "supervisor startup timed out");
            thread::sleep(Duration::from_millis(10));
        }
        h
    }
    fn cli(&self, args: &[&str], credential: &Path, out: Option<&Path>) -> Output {
        let mut args: Vec<String> = args.iter().map(|s| (*s).into()).collect();
        args.extend([
            "--endpoint".into(),
            self.endpoint.to_str().unwrap().into(),
            "--json".into(),
        ]);
        launch(&args, credential, out).output().unwrap()
    }
    fn operator(&self, args: &[&str]) -> Output {
        self.cli(args, &self.operator, None)
    }
    fn activate(&self) {
        success(self.operator(&["up", self.manifest.to_str().unwrap(), "--no-start"]));
    }
    fn issue(
        &self,
        session: &str,
        expires: u64,
        permissions: Value,
        project: Option<&str>,
    ) -> (PathBuf, String) {
        let m = Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap();
        let spec = json!({"session":session, "project": project.unwrap_or(&m.project), "policy":m.id(), "expires_at":expires,
            "scope": {"templates":["app"], "permissions": permissions,
                "volumes":["fixtures", "pgdata"], "services":["db"],
                "mounts":{"fixtures":"ro", "results":"rw"}, "links":["db:5432"], "max_instances":1}});
        let path = self.dir.path().join(format!("{session}-grant.json"));
        protected(&path, spec.to_string().as_bytes());
        let token_path = self.dir.path().join(format!("{session}-token"));
        let output = self.cli(
            &[
                "operator",
                "issue",
                path.to_str().unwrap(),
                "--token-fd",
                "4",
            ],
            &self.operator,
            Some(&token_path),
        );
        let v = success(output);
        assert!(v.get("token").is_none());
        let token = fs::read_to_string(&token_path).unwrap();
        assert!(token.trim().len() == 64);
        (token_path, v["grant"].as_str().unwrap().into())
    }
    fn raw(&self, value: Value) -> Value {
        let mut stream = control::connect(&self.endpoint).unwrap();
        control::timeouts(&stream).unwrap();
        control::write_frame(&mut stream, &value).unwrap();
        serde_json::from_slice(&control::read_frame(&mut stream).unwrap()).unwrap()
    }
}
impl Drop for Harness {
    fn drop(&mut self) {
        let _ = self.process.kill();
        let _ = self.process.wait();
    }
}
fn decoded(output: &Output) -> Value {
    assert!(output.stderr.is_empty(), "JSON mode emitted diagnostics");
    serde_json::from_slice(&output.stdout).expect("exactly one JSON value")
}
fn success(output: Output) -> Value {
    let v = decoded(&output);
    assert!(output.status.success(), "unexpected error: {v}");
    v
}
fn failure(output: Output, exit: i32, code: &str) -> Value {
    let v = decoded(&output);
    assert_eq!(output.status.code(), Some(exit));
    assert_eq!(v["error"]["code"], code);
    v
}
#[test]
fn actual_authenticated_cli_activation_and_allow_deny() {
    let mut h = Harness::start();
    h.activate();
    let repeat = success(h.operator(&["up", h.manifest.to_str().unwrap(), "--no-start"]));
    assert_eq!(repeat["started"], false);
    assert_eq!(success(h.operator(&["ps"]))["instances"], json!([]));
    let (agent, id) = h.issue(
        "agent-a",
        now() + 60,
        json!([
            "run",
            "list",
            "status",
            "stop",
            "volume_inspect",
            "service_inspect"
        ]),
        None,
    );
    assert_eq!(
        success(h.cli(&["ps"], &agent, None))["instances"],
        json!([])
    );
    let metadata = success(h.cli(&["volume", "inspect", "fixtures"], &agent, None));
    assert_eq!(metadata["created"], false);
    // Token scope cannot expand operator permissions.
    let hidden = failure(
        h.cli(&["volume", "inspect", "pgdata"], &agent, None),
        3,
        "not_found",
    );
    let unknown = failure(
        h.cli(&["volume", "inspect", "unknown"], &agent, None),
        3,
        "not_found",
    );
    assert_eq!(hidden, unknown);
    failure(
        h.cli(&["service", "inspect", "db"], &agent, None),
        3,
        "not_found",
    );
    failure(
        h.cli(&["run", "app"], &agent, None),
        5,
        "unsupported_worker",
    );
    failure(h.cli(&["run", "db"], &agent, None), 3, "forbidden");
    failure(
        h.cli(
            &["up", h.manifest.to_str().unwrap(), "--no-start"],
            &agent,
            None,
        ),
        3,
        "forbidden",
    );
    failure(h.cli(&["operator", "audit"], &agent, None), 3, "forbidden");
    failure(
        h.cli(&["operator", "revoke", &id], &agent, None),
        3,
        "forbidden",
    );
    failure(
        h.cli(&["volume", "create", "fixtures"], &agent, None),
        3,
        "forbidden",
    );
    failure(
        h.operator(&["volume", "create", "fixtures"]),
        5,
        "unsupported_worker",
    );
    failure(
        h.operator(&["up", h.manifest.to_str().unwrap()]),
        5,
        "unsupported_worker",
    );
    // Allowed launch did not leave a fictitious instance behind.
    assert_eq!(success(h.operator(&["ps"]))["instances"], json!([]));
    success(h.operator(&["operator", "revoke", &id]));
    failure(h.cli(&["ps"], &agent, None), 3, "unauthenticated");
    success(h.operator(&["operator", "shutdown"]));
    assert!(h.process.wait_with_timeout());
}
trait WaitTimeout {
    fn wait_with_timeout(&mut self) -> bool;
}
impl WaitTimeout for Child {
    fn wait_with_timeout(&mut self) -> bool {
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if let Some(s) = self.try_wait().unwrap() {
                return s.success();
            }
            thread::sleep(Duration::from_millis(10));
        }
        false
    }
}
#[test]
fn raw_protocol_cannot_override_policy_or_issue_credentials() {
    let h = Harness::start();
    h.activate();
    let (agent, _) = h.issue("raw-agent", now() + 60, json!(["run"]), None);
    let token = fs::read_to_string(agent).unwrap();
    let base = json!({"version":1,"auth": token.trim(), "operation":{"op":"run","template":"app"}});
    for (key, value) in [
        ("command", json!(["/bin/sh"])),
        ("env", json!({"SECRET":"secret-marker"})),
        ("mounts", json!([])),
        ("image", json!("untrusted")),
    ] {
        let mut v = base.clone();
        v["operation"][key] = value;
        assert_eq!(h.raw(v)["error"]["code"], "invalid_request");
    }
    assert_eq!(
        h.raw(
            json!({"version":1, "auth": token.trim(), "operation":{"op":"issue","grant":{
        "session":"evil","project":"example","policy":"bogus","expires_at":now()+30,
        "scope":{"max_instances":1}}}})
        )["error"]["code"],
        "forbidden"
    );
    assert_eq!(
        h.raw(json!({"version":1, "auth":"b".repeat(64), "operation":{"op":"ps"}}))["error"]
            ["code"],
        "unauthenticated"
    );
    let missing = failure(
        h.cli(&["status", "job_unknown"], &h.operator, None),
        3,
        "not_found",
    );
    assert_eq!(missing["error"]["code"], "not_found");
    let audit = success(h.operator(&["operator", "audit"])).to_string();
    assert!(!audit.contains(token.trim()));
    assert!(!audit.contains("secret-marker"));
    assert!(!audit.contains(&"a".repeat(64)));
}
#[test]
fn expired_credentials_fail_and_invalid_cross_project_issuance_fails() {
    let h = Harness::start();
    h.activate();
    let (agent, _) = h.issue("expiring", now() + 2, json!(["list"]), None);
    let deadline = now() + 3;
    while now() < deadline {
        thread::sleep(Duration::from_millis(50));
    }
    failure(h.cli(&["ps"], &agent, None), 3, "unauthenticated");
    let m = Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap();
    for change in [
        json!({"project":"other"}),
        json!({"policy":"pol_unknown"}),
        json!({"expires_at":now()-1}),
    ] {
        let mut grant = json!({"session":"bad","project":"example","policy":m.id(),"expires_at":now()+60,"scope":{"max_instances":1}});
        for (k, v) in change.as_object().unwrap() {
            grant[k] = v.clone();
        }
        let response = h.raw(
            json!({"version":1, "auth":"a".repeat(64), "operation":{"op":"issue","grant":grant}}),
        );
        assert_eq!(response["error"]["code"], "invalid_request");
    }
}
#[test]
fn policy_replacement_refused_and_survives_clean_restart() {
    let mut h = Harness::start();
    h.activate();
    let mut m = Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap();
    m.jobs
        .get_mut("app")
        .unwrap()
        .command
        .push("--changed".into());
    let changed = h.dir.path().join("changed.json");
    protected(&changed, &m.canonical());
    failure(
        h.operator(&["up", changed.to_str().unwrap(), "--no-start"]),
        4,
        "policy_in_use",
    );
    success(h.operator(&["operator", "shutdown"]));
    assert!(h.process.wait_with_timeout());
    // New process keeps immutable policy, but old agent credentials are deliberately not recovered.
    let mut next = launch(
        &[
            "supervisor".into(),
            "--state-dir".into(),
            h.dir.path().to_str().unwrap().into(),
        ],
        &h.operator,
        None,
    )
    .spawn()
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while !h.endpoint.exists() {
        assert!(Instant::now() < deadline);
        thread::sleep(Duration::from_millis(10));
    }
    failure(
        h.operator(&["up", changed.to_str().unwrap(), "--no-start"]),
        4,
        "policy_in_use",
    );
    success(h.operator(&["operator", "shutdown"]));
    assert!(next.wait_with_timeout());
}
#[test]
fn frame_bounds_and_private_endpoint_fail_closed() {
    let h = Harness::start();
    // Bounds apply both to the initial challenge and the authenticated request.
    for authenticated in [false, true] {
        let mut stream = if authenticated {
            control::connect(&h.endpoint).unwrap()
        } else {
            TcpStream::connect(control::endpoint(&h.endpoint).unwrap()).unwrap()
        };
        stream
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        stream
            .write_all(&((MAX_DOCUMENT + 1) as u32).to_be_bytes())
            .unwrap();
        let v: Value = serde_json::from_slice(&control::read_frame(&mut stream).unwrap()).unwrap();
        assert_eq!(v["error"]["code"], "invalid_request");
    }
    let stale = launch(
        &[
            "supervisor".into(),
            "--state-dir".into(),
            h.dir.path().to_str().unwrap().into(),
            "--json".into(),
        ],
        &h.operator,
        None,
    )
    .output()
    .unwrap();
    failure(stale, 5, "supervisor_unavailable");
    let remote = h.dir.path().join("remote");
    protected(
        &remote,
        &serde_json::to_vec(&json!({
            "address": "8.8.8.8:80",
            "server_key": "c".repeat(64)
        }))
        .unwrap(),
    );
    assert!(control::endpoint(&remote).is_err());
    let legacy = h.dir.path().join("legacy");
    protected(&legacy, b"\"127.0.0.1:12345\"");
    assert!(
        control::endpoint(&legacy).is_err(),
        "no unauthenticated downgrade"
    );
    let mut prefix_only = &b"\x00\x04\x00\x01"[..];
    assert!(control::read_frame(&mut prefix_only).is_err());
}
#[test]
fn diagnostics_and_reserved_helper_do_not_expose_inputs() {
    let output = Command::new(env!("CARGO_BIN_EXE_silo"))
        .args(["run", "app", "--env", "secret-marker", "--json"])
        .output()
        .unwrap();
    failure(output, 2, "invalid_request");
    let out = Command::new(env!("CARGO_BIN_EXE_silo"))
        .args(["__namespace_helper", "--json"])
        .output()
        .unwrap();
    failure(out, 5, "unsupported_worker");
    let dir = tempfile::tempdir().unwrap();
    let bad = dir.path().join("invalid.yaml");
    protected(&bad, b"secret-marker: [bad");
    let out = Command::new(env!("CARGO_BIN_EXE_silo"))
        .args(["validate", bad.to_str().unwrap(), "--json"])
        .output()
        .unwrap();
    let v = failure(out, 2, "invalid_manifest");
    assert!(!v.to_string().contains("secret-marker"));
}
#[test]
fn stale_loopback_port_cannot_collect_bearer_credentials() {
    let mut h = Harness::start();
    let address = control::endpoint(&h.endpoint).unwrap();
    // A valid proof from an earlier exchange cannot authenticate a later one.
    let mut old_connection = TcpStream::connect(address).unwrap();
    control::timeouts(&old_connection).unwrap();
    control::write_frame(&mut old_connection, &json!({"nonce": "d".repeat(64)})).unwrap();
    let recorded_proof: Value =
        serde_json::from_slice(&control::read_frame(&mut old_connection).unwrap()).unwrap();
    drop(old_connection);
    h.process.kill().unwrap();
    h.process.wait().unwrap();
    // The protected endpoint still points at the old port after abnormal loss.
    let impostor = std::net::TcpListener::bind(address).unwrap();
    let capture = thread::spawn(move || {
        let (mut stream, _) = impostor.accept().unwrap();
        control::timeouts(&stream).unwrap();
        let first = control::read_frame(&mut stream).unwrap();
        control::write_frame(&mut stream, &recorded_proof).unwrap();
        let mut remaining = Vec::new();
        stream.read_to_end(&mut remaining).unwrap();
        [first, remaining].concat()
    });
    let result = control::call(
        &h.endpoint,
        Request {
            version: 1,
            auth: Secret("a".repeat(64)),
            operation: Operation::Ps,
        },
    );
    let captured = String::from_utf8_lossy(&capture.join().unwrap()).into_owned();
    assert!(
        !captured.contains(&"a".repeat(64)),
        "peer received bearer token"
    );
    assert!(
        !captured.contains("auth"),
        "request preceded server authentication"
    );
    assert!(result.is_err());
}

#[test]
fn library_client_uses_the_same_authenticated_transport() {
    let h = Harness::start();
    h.activate();
    let v = control::call(
        &h.endpoint,
        Request {
            version: 1,
            auth: Secret("a".repeat(64)),
            operation: Operation::Ps,
        },
    )
    .unwrap();
    assert_eq!(v["instances"], json!([]));
    let mut f = File::open(&h.operator).unwrap();
    let mut token = String::new();
    f.read_to_string(&mut token).unwrap();
    let _ = token; // credentials were never passed in argv or environment.
}
