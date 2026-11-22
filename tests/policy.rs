use serde_json::{json, Value};
use silo::{
    auth::{Credentials, Grant, Secret},
    cli,
    error::Code,
    policy::{Manifest, Permission, MAX_DOCUMENT},
};
use std::collections::BTreeSet;

fn fixture() -> Manifest {
    Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap()
}
fn doc() -> Value {
    serde_json::to_value(fixture()).unwrap()
}
fn rejected(value: Value) {
    let err = Manifest::parse(&serde_json::to_vec(&value).unwrap()).unwrap_err();
    assert_eq!(err.code, Code::InvalidManifest);
}
#[test]
fn strict_schema_references_limits_and_pins() {
    let cases: Vec<fn(&mut Value)> = vec![
        |v| v["unexpected"] = json!("secret-marker"),
        |v| v["version"] = json!(2),
        |v| v["project"] = json!("../project"),
        |v| v["jobs"]["app"]["unknown"] = json!(true),
        |v| v["jobs"]["app"]["image"] = json!("app:latest"),
        |v| v["jobs"]["app"]["command"] = json!(["app"]),
        |v| v["jobs"]["app"]["command"] = json!([]),
        |v| v["jobs"]["app"]["workdir"] = json!("/app/../tmp"),
        |v| v["jobs"]["app"]["limits"]["memory_bytes"] = json!(0),
        |v| v["jobs"]["app"]["limits"]["duration_secs"] = json!(86401),
        |v| v["jobs"]["app"]["mounts"][0]["volume"] = json!("unknown"),
        |v| v["jobs"]["app"]["mounts"][0]["mode"] = json!("exec"),
        |v| v["jobs"]["app"]["mounts"][0]["target"] = json!("/"),
        |v| v["jobs"]["app"]["agent"] = json!(["volume_inspect"]),
        |v| v["services"]["db"]["template"]["agent"] = json!(["run"]),
        |v| v["network"]["external"] = json!("allow"),
        |v| v["network"]["allow"][0]["protocol"] = json!("udp"),
        |v| v["network"]["allow"][0]["to"] = json!("unknown"),
        |v| v["network"]["allow"][0]["port"] = json!(1234),
        |v| v["volumes"]["pgdata"]["sharing"] = json!("shared"),
        |v| v["volumes"]["pgdata"]["max_bytes"] = json!(0),
    ];
    for mutate in cases {
        let mut v = doc();
        mutate(&mut v);
        rejected(v);
    }
    let yaml = include_str!("../examples/foundation.yaml").replace("jobs:\n", "jobs:\n  app: {}\n");
    assert!(
        Manifest::parse(yaml.as_bytes()).is_err(),
        "duplicate map keys must fail"
    );
    let invalid = b"version: 1\nproject: [secret-marker";
    let error = Manifest::parse(invalid).unwrap_err();
    assert!(!error.message.contains("secret-marker"));
    assert!(Manifest::parse(&vec![b' '; MAX_DOCUMENT + 1]).is_err());
}
#[test]
fn writable_paths_cannot_be_fixed_executable_inputs() {
    for (field, value) in [
        ("command", json!(["/results/script"])),
        ("command", json!(["/bin/sh", "/results/script"])),
        ("executable_inputs", json!(["/fixtures/init.sql"])),
        ("executable_inputs", json!(["/results/config.json"])),
        ("executable_inputs", json!(["/tmp/plugin.so"])),
    ] {
        let mut v = doc();
        v["jobs"]["app"][field] = value;
        rejected(v);
    }
    for target in ["/usr", "/lib", "/etc", "/proc", "/run"] {
        let mut v = doc();
        v["jobs"]["app"]["mounts"][0]["target"] = json!(target);
        rejected(v);
    }
}
#[test]
fn stable_policy_and_resource_ids_follow_content_not_yaml_order() {
    let m = fixture();
    let from_json = Manifest::parse(&m.canonical()).unwrap();
    assert_eq!(m.id(), from_json.id());
    assert_eq!(
        m.resource_id("vol", "fixtures"),
        from_json.resource_id("vol", "fixtures")
    );
    assert_ne!(
        m.resource_id("vol", "fixtures"),
        m.resource_id("vol", "pgdata")
    );
    let mut changed = m.clone();
    changed
        .jobs
        .get_mut("app")
        .unwrap()
        .command
        .push("--fixed".into());
    assert_ne!(m.id(), changed.id());
}
fn grant(m: &Manifest) -> Grant {
    serde_json::from_value(json!({
        "session": "agent-a", "project": m.project, "policy": m.id(), "expires_at": 200,
        "scope": {"templates": ["app"], "permissions": ["run", "status", "stop"],
        "mounts": {"fixtures": "ro", "results": "rw"}, "links": ["db:5432"], "max_instances": 1}
    }))
    .unwrap()
}
#[test]
fn authorization_is_template_policy_and_scope_intersection() {
    let m = fixture();
    let g = grant(&m);
    assert!(g.launch(&m, "app"));
    assert!(!g.launch(&m, "db"));
    assert!(!g.permits(&m, Permission::Logs, "app"));
    let mut no_mount = g.clone();
    no_mount.scope.mounts.clear();
    assert!(!no_mount.launch(&m, "app"));
    let mut no_link = g.clone();
    no_link.scope.links.clear();
    assert!(!no_link.launch(&m, "app"));
    let mut other_project = g.clone();
    other_project.project = "other".into();
    assert!(!other_project.launch(&m, "app"));
    let mut no_operator_allow = m.clone();
    no_operator_allow.jobs.get_mut("app").unwrap().agent.clear();
    let mut rebound = g.clone();
    rebound.policy = no_operator_allow.id();
    assert!(!rebound.launch(&no_operator_allow, "app"));
    let mut no_permission = g.clone();
    no_permission.scope.permissions = BTreeSet::new();
    assert!(!no_permission.launch(&m, "app"));
}
#[test]
fn digest_credentials_expire_revoke_and_scope_is_validated() {
    let m = fixture();
    let mut credentials = Credentials::new(Secret("a".repeat(64))).unwrap();
    let (id, secret) = credentials.issue(grant(&m), &m, 100).unwrap();
    assert!(credentials.authenticate(&secret, 199).is_ok());
    assert!(credentials.authenticate(&secret, 200).is_err());
    credentials.revoke(&id).unwrap();
    assert!(credentials.authenticate(&secret, 101).is_err());
    assert!(credentials
        .authenticate(&Secret("b".repeat(64)), 100)
        .is_err());
    let mut g = grant(&m);
    g.project = "other".into();
    assert!(credentials.issue(g, &m, 100).is_err());
    let mut g = grant(&m);
    g.policy = "pol_other".into();
    assert!(credentials.issue(g, &m, 100).is_err());
    let mut g = grant(&m);
    g.expires_at = 100;
    assert!(credentials.issue(g, &m, 100).is_err());
    let mut g = grant(&m);
    g.scope.templates.insert("db".into());
    assert!(credentials.issue(g, &m, 100).is_err());
}
#[test]
fn cli_rejects_overrides_unknown_flags_duplicate_flags_and_fd_aliases() {
    for args in [
        vec!["run", "app", "--mount", "pgdata:/data:rw"],
        vec!["run", "app", "--image", "other"],
        vec!["run", "app", "--env", "secret-marker"],
        vec!["run", "app", "--command", "sh"],
        vec!["run", "app", "extra"],
        vec!["validate", "x", "--json", "--json"],
        vec!["supervisor", "--state-dir", "/tmp/x", "--auth-fd", "0"],
        vec![
            "operator",
            "issue",
            "x",
            "--auth-fd",
            "3",
            "--token-fd",
            "3",
            "--endpoint",
            "/tmp/x",
        ],
        vec!["__namespace_helper", "--command", "/bin/sh"],
    ] {
        let strings = args.iter().map(|s| (*s).to_owned()).collect::<Vec<_>>();
        let err = match cli::parse(&strings) {
            Err(e) => e,
            Ok(_) => panic!("unexpected accepted arguments"),
        };
        assert_eq!(err.code, Code::InvalidRequest);
        assert!(!err.message.contains("secret-marker"));
    }
}
