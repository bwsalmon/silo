//! Synthetic lifecycle fixtures exercise the real authenticated protocol handler.
//! No fixture represents a launched process or qualifies namespace/storage isolation.
use super::*;
use crate::auth::Scope;
use std::{net::TcpStream, os::unix::fs::PermissionsExt};

fn fixture() -> (tempfile::TempDir, Supervisor) {
    let root = tempfile::tempdir().unwrap();
    fs::set_permissions(root.path(), fs::Permissions::from_mode(0o700)).unwrap();
    let mut s = Supervisor::new(root.path().into(), Secret("a".repeat(64))).unwrap();
    s.policy = Some(Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap());
    (root, s)
}
fn grant(s: &Supervisor, session: &str, permissions: &[Permission]) -> Grant {
    Grant {
        session: session.into(),
        project: s.manifest().unwrap().project.clone(),
        policy: s.manifest().unwrap().id(),
        expires_at: now() + 3600,
        scope: Scope {
            templates: ["app".into()].into_iter().collect(),
            permissions: permissions.iter().cloned().collect(),
            mounts: [
                ("fixtures".into(), crate::policy::Mode::Ro),
                ("results".into(), crate::policy::Mode::Rw),
            ]
            .into_iter()
            .collect(),
            links: ["db:5432".into()].into_iter().collect(),
            max_instances: 1,
            ..Scope::default()
        },
    }
}
fn issue(s: &mut Supervisor, grant: Grant) -> (String, Secret) {
    let m = s.manifest().unwrap().clone();
    s.credentials.issue(grant, &m, now()).unwrap()
}
fn insert(s: &mut Supervisor, id: &str, grant_id: &str, g: &Grant, state: State) {
    s.instances.insert(
        id.into(),
        Instance {
            id: id.into(),
            template: "app".into(),
            state,
            exit_code: Some(17),
            project: g.project.clone(),
            session: g.session.clone(),
            grant_id: grant_id.into(),
            policy: g.policy.clone(),
            lease_deadline: g.expires_at,
        },
    );
}
fn exchange(s: &mut Supervisor, token: &Secret, operation: Operation) -> Result<Value> {
    let listener = TcpListener::bind(control::loopback()).unwrap();
    let address = listener.local_addr().unwrap();
    std::thread::scope(|scope| {
        let server = scope.spawn(|| {
            let (mut stream, _) = listener.accept().unwrap();
            control::timeouts(&stream).unwrap();
            handle_connection(s, &mut stream, &Secret("c".repeat(64)));
        });
        let mut stream = TcpStream::connect(address).unwrap();
        control::timeouts(&stream).unwrap();
        control::verify_server(&mut stream, &Secret("c".repeat(64))).unwrap();
        control::write_frame(
            &mut stream,
            &Request {
                version: 1,
                auth: Secret(token.0.clone()),
                operation,
            },
        )
        .unwrap();
        let response: Response =
            serde_json::from_slice(&control::read_frame(&mut stream).unwrap()).unwrap();
        server.join().unwrap();
        response.result()
    })
}
#[test]
fn real_protocol_ownership_list_status_stop_and_logs_are_separate() {
    let (_root, mut s) = fixture();
    let ga = grant(
        &s,
        "a",
        &[
            Permission::List,
            Permission::Status,
            Permission::Stop,
            Permission::Logs,
        ],
    );
    let gb = grant(
        &s,
        "b",
        &[
            Permission::List,
            Permission::Status,
            Permission::Stop,
            Permission::Logs,
        ],
    );
    let (a_id, a) = issue(&mut s, ga.clone());
    let (b_id, b) = issue(&mut s, gb.clone());
    insert(&mut s, "job_a", &a_id, &ga, State::Exited);
    insert(&mut s, "job_b", &b_id, &gb, State::Exited);
    let listed = exchange(&mut s, &a, Operation::Ps).unwrap();
    assert_eq!(listed["instances"].as_array().unwrap().len(), 1);
    assert_eq!(listed["instances"][0]["id"], "job_a");
    assert!(listed["instances"][0].get("project").is_none());
    assert_eq!(
        exchange(&mut s, &a, Operation::Status { id: "job_a".into() }).unwrap()["exit_code"],
        17
    );
    let hidden = exchange(&mut s, &b, Operation::Status { id: "job_a".into() }).unwrap_err();
    let unknown = exchange(
        &mut s,
        &b,
        Operation::Status {
            id: "job_unknown".into(),
        },
    )
    .unwrap_err();
    assert_eq!(hidden, unknown);
    assert_eq!(
        exchange(&mut s, &b, Operation::Stop { id: "job_a".into() }).unwrap_err(),
        hidden
    );
    // Even a token asking for logs cannot expand the template's denied log permission.
    assert_eq!(
        exchange(&mut s, &a, Operation::Logs { id: "job_a".into() }).unwrap_err(),
        hidden
    );
    let first = exchange(&mut s, &a, Operation::Stop { id: "job_a".into() }).unwrap();
    let second = exchange(&mut s, &a, Operation::Stop { id: "job_a".into() }).unwrap();
    assert_eq!(first, second);
    assert_eq!(first["state"], "exited");

    let g = grant(&s, "status-only", &[Permission::Status]);
    let (id, token) = issue(&mut s, g.clone());
    insert(&mut s, "job_status", &id, &g, State::Exited);
    assert!(
        exchange(&mut s, &token, Operation::Ps).unwrap()["instances"]
            .as_array()
            .unwrap()
            .is_empty()
    );
    assert!(exchange(
        &mut s,
        &token,
        Operation::Status {
            id: "job_status".into()
        }
    )
    .is_ok());
    assert_eq!(
        exchange(
            &mut s,
            &token,
            Operation::Stop {
                id: "job_status".into()
            }
        )
        .unwrap_err(),
        hidden
    );
}
#[test]
fn project_policy_and_grant_ownership_all_bind_visibility() {
    let (_root, mut s) = fixture();
    let g = grant(&s, "same-session", &[Permission::Status]);
    let (id, token) = issue(&mut s, g.clone());
    insert(&mut s, "job_original", &id, &g, State::Exited);
    let (_, other_grant_same_session) = issue(&mut s, g.clone());
    assert_eq!(
        exchange(
            &mut s,
            &other_grant_same_session,
            Operation::Status {
                id: "job_original".into()
            }
        )
        .unwrap_err()
        .code,
        Code::NotFound
    );
    // A credential valid in a different project's policy must still reveal no current resources.
    let mut other = s.manifest().unwrap().clone();
    other.project = "other".into();
    let mut different = g.clone();
    different.project = other.project.clone();
    different.policy = other.id();
    let (foreign_id, foreign_token) = s
        .credentials
        .issue(different.clone(), &other, now())
        .unwrap();
    insert(
        &mut s,
        "job_foreign",
        &foreign_id,
        &different,
        State::Exited,
    );
    assert_eq!(
        exchange(
            &mut s,
            &foreign_token,
            Operation::Status {
                id: "job_foreign".into()
            }
        )
        .unwrap_err()
        .code,
        Code::NotFound
    );
    assert_eq!(
        exchange(
            &mut s,
            &token,
            Operation::Status {
                id: "job_foreign".into()
            }
        )
        .unwrap_err()
        .code,
        Code::NotFound
    );
    assert_eq!(
        exchange(&mut s, &foreign_token, Operation::Ps).unwrap()["instances"],
        json!([])
    );
}
#[test]
fn leases_revocation_and_stop_schedule_teardown_without_claiming_completion() {
    let (_root, mut s) = fixture();
    let g = grant(
        &s,
        "leased",
        &[Permission::Run, Permission::Status, Permission::Stop],
    );
    let (id, token) = issue(&mut s, g.clone());
    insert(&mut s, "job_live", &id, &g, State::Running);
    assert_eq!(
        exchange(
            &mut s,
            &token,
            Operation::Run {
                template: "app".into()
            }
        )
        .unwrap_err()
        .code,
        Code::QuotaExceeded
    );
    let stopped = exchange(
        &mut s,
        &token,
        Operation::Stop {
            id: "job_live".into(),
        },
    )
    .unwrap();
    assert_eq!(stopped["state"], "stopping");
    assert_eq!(
        exchange(
            &mut s,
            &token,
            Operation::Run {
                template: "app".into()
            }
        )
        .unwrap_err()
        .code,
        Code::QuotaExceeded
    );
    let pending = s.take_teardown();
    assert_eq!(pending.len(), 1);
    assert_eq!(pending[0].reason, "stop");
    s.tick(g.expires_at + 1);
    assert_eq!(s.instances["job_live"].state, State::Stopping);
    assert_eq!(
        exchange(&mut s, &Secret("a".repeat(64)), Operation::Shutdown)
            .unwrap_err()
            .code,
        Code::CleanupUnverified
    );

    // Fresh fixture for revocation: denial of future requests and immediate teardown scheduling.
    let (_root, mut s) = fixture();
    let g = grant(&s, "revoked", &[Permission::Status]);
    let (id, token) = issue(&mut s, g.clone());
    insert(&mut s, "job_live", &id, &g, State::Running);
    exchange(&mut s, &Secret("a".repeat(64)), Operation::Revoke { id }).unwrap();
    assert_eq!(s.instances["job_live"].state, State::Stopping);
    assert_eq!(s.take_teardown()[0].instance, "job_live");
    assert_eq!(
        exchange(
            &mut s,
            &token,
            Operation::Status {
                id: "job_live".into()
            }
        )
        .unwrap_err()
        .code,
        Code::Unauthenticated
    );

    let (_root, mut s) = fixture();
    let g = grant(&s, "expired", &[Permission::Status]);
    let (id, _) = issue(&mut s, g.clone());
    insert(&mut s, "job_lease", &id, &g, State::Running);
    s.instances.get_mut("job_lease").unwrap().lease_deadline = now() - 1;
    s.tick(now());
    assert_eq!(s.instances["job_lease"].state, State::Stopping);
    assert_eq!(s.take_teardown()[0].reason, "lease_or_revocation");
}
#[test]
fn audits_are_bounded_redacted_and_record_allowed_but_unavailable_operations() {
    let (_root, mut s) = fixture();
    let secret = Secret("a".repeat(64));
    for _ in 0..600 {
        s.handle(
            Request {
                version: 1,
                auth: Secret(secret.0.clone()),
                operation: Operation::Status {
                    id: "secret-marker".into(),
                },
            },
            now(),
        )
        .unwrap_err();
    }
    assert_eq!(s.audit.len(), AUDIT_CAPACITY);
    assert_eq!(s.audit.front().unwrap().sequence, 89);
    let value = exchange(&mut s, &secret, Operation::Audit)
        .unwrap()
        .to_string();
    assert!(!value.contains("secret-marker"));
    assert!(!value.contains(&secret.0));
    exchange(
        &mut s,
        &secret,
        Operation::Run {
            template: "app".into(),
        },
    )
    .unwrap_err();
    let entry = s.audit.back().unwrap();
    assert!(entry.allowed);
    assert_eq!(entry.code, Some(Code::UnsupportedWorker));
    assert!(entry.resource.starts_with("tpl_"));
}
