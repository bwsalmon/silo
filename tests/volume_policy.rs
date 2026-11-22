//! Pure policy checks. Actual filesystem/namespace qualification is in real_worker.py.
use serde_json::{json, Value};
use silo::{cli, image::execution, policy::Manifest};

fn policy() -> Value {
    serde_json::to_value(Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap())
        .unwrap()
}
fn rejected(v: Value) {
    assert!(Manifest::parse(&serde_json::to_vec(&v).unwrap()).is_err());
}
#[test]
fn volume_names_are_references_and_never_bind_sources() {
    for name in [
        "../pgdata",
        "/private/pgdata",
        "fixtures/child",
        "fixtures/../pgdata",
        "Fixtures",
    ] {
        let mut v = policy();
        v["jobs"]["app"]["mounts"][0]["volume"] = json!(name);
        rejected(v);
    }
    for key in ["source", "host_path", "bind", "id"] {
        let mut v = policy();
        v["jobs"]["app"]["mounts"][0][key] = json!("/private/pgdata");
        rejected(v);
    }
    let mut v = policy();
    let mut repeated = v["jobs"]["app"]["mounts"][0].clone();
    repeated["target"] = json!("/other-target");
    v["jobs"]["app"]["mounts"]
        .as_array_mut()
        .unwrap()
        .push(repeated);
    rejected(v);
}
#[test]
fn protected_overlapping_and_init_hook_targets_are_rejected() {
    for path in [
        "/",
        "/proc/other",
        "/docker-entrypoint-initdb.d",
        "/docker-entrypoint-initdb.d/hook",
        "/fixtures/nested",
        "/fixtures/../escape",
        "//other",
    ] {
        let mut v = policy();
        v["jobs"]["app"]["mounts"][1]["target"] = json!(path);
        rejected(v);
    }
    for identity in [65535, u32::MAX] {
        for field in ["uid", "gid"] {
            let mut v = policy();
            v["volumes"]["results"][field] = json!(identity);
            rejected(v);
        }
    }
}
#[test]
fn mutable_inputs_cannot_become_protected_service_code() {
    for (field, value) in [
        ("command", json!(["/bin/sh", "/results/agent-script"])),
        ("executable_inputs", json!(["/results/plugin.so"])),
        ("executable_inputs", json!(["/fixtures/init.sql"])),
        ("env", json!({"BASH_ENV":"/results/agent-script"})),
        ("env", json!({"LD_PRELOAD":"/results/plugin.so"})),
        ("env", json!({"PYTHONPATH":"/results/plugins"})),
    ] {
        let mut v = policy();
        // This template already has a protected app -> db SQL link.
        v["jobs"]["app"][field] = value;
        rejected(v);
    }
    for args in [
        vec!["volume", "rm", "results", "--force"],
        vec!["volume", "recover", "results", "--force"],
        vec!["run", "app", "--config", "/results/plugin"],
        vec!["run", "app", "--env", "BASH_ENV=/results/script"],
        vec!["run", "app", "--entrypoint", "/bin/sh"],
    ] {
        let mut args: Vec<_> = args.iter().map(|v| v.to_string()).collect();
        args.extend(["--endpoint", "/private/endpoint.json", "--auth-fd", "3"].map(String::from));
        assert!(cli::parse(&args).is_err());
    }
}
#[test]
fn numeric_image_user_must_match_explicit_template_identity() {
    let mut m = Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap();
    let t = m.jobs.get_mut("app").unwrap();
    t.uid = 70;
    t.gid = 70;
    let config = json!({"config":{"User":"70:70","Cmd":["/bin/tool"],
        "Volumes":{"/unapproved":{}},"ExposedPorts":{"5432/tcp":{}},"StopSignal":"SIGINT"}});
    execution(&config, Some(t)).unwrap();
    t.uid = 0;
    assert!(execution(&config, Some(t)).is_err());
}
