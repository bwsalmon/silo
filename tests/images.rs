//! Parser/verification regression tests. These do not launch workloads or qualify a worker.
use serde_json::json;
use silo::{
    image::{execution, image_path, select_platform, validate_layer, ArchiveBudget},
    policy::{digest, Manifest},
    worker::{valid_digest, verify_blob, Tool},
};
use std::{
    fs,
    io::Cursor,
    os::unix::fs::{symlink, PermissionsExt},
};

fn sha(bytes: &[u8]) -> String {
    format!("sha256:{}", digest(bytes))
}
fn layer(path: &str, kind: u8, link: &str, uid: u64, gid: u64, mode: u32) -> Vec<u8> {
    let mut h = tar::Header::new_ustar();
    // Deliberately bypass Builder's path checks to produce malicious archive headers.
    h.as_mut_bytes()[..100].fill(0);
    h.as_mut_bytes()[..path.len()].copy_from_slice(path.as_bytes());
    h.set_entry_type(tar::EntryType::new(kind));
    h.set_uid(uid);
    h.set_gid(gid);
    h.set_mode(mode);
    h.set_size(0);
    if !link.is_empty() {
        h.set_link_name(link).unwrap();
    }
    h.set_cksum();
    let mut tar = tar::Builder::new(Vec::new());
    tar.append(&h, &[][..]).unwrap();
    tar.into_inner().unwrap()
}
fn validate(bytes: &[u8]) -> silo::error::Result<()> {
    validate_layer(
        Cursor::new(bytes),
        &sha(bytes),
        &mut ArchiveBudget {
            bytes: 1024 * 1024,
            entries: 100,
        },
    )
}
#[test]
fn platform_selection_verifies_the_index_and_requires_one_exact_platform() {
    let selected = format!("sha256:{}", "a".repeat(64));
    let mut v = json!({"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",
        "manifests":[{"digest":selected,"size":100,"mediaType":"application/vnd.oci.image.manifest.v1+json",
            "platform":{"os":"linux","architecture":"amd64"}}]});
    let b = serde_json::to_vec(&v).unwrap();
    assert_eq!(select_platform(&b, &sha(&b)).unwrap(), selected);
    assert!(select_platform(&b, &format!("sha256:{}", "0".repeat(64))).is_err());
    for (os, arch) in [("linux", "arm64"), ("windows", "amd64")] {
        v["manifests"][0]["platform"]["os"] = json!(os);
        v["manifests"][0]["platform"]["architecture"] = json!(arch);
        let b = serde_json::to_vec(&v).unwrap();
        assert!(select_platform(&b, &sha(&b)).is_err());
    }
    v["manifests"][0]["platform"] = json!({"os":"linux","architecture":"amd64"});
    let first = v["manifests"][0].clone();
    v["manifests"].as_array_mut().unwrap().push(first);
    let b = serde_json::to_vec(&v).unwrap();
    assert!(select_platform(&b, &sha(&b)).is_err());
}
#[test]
fn fetched_blobs_require_exact_hash_and_size() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("blob");
    fs::write(&path, b"verified").unwrap();
    assert_eq!(
        verify_blob(&path, &sha(b"verified"), 8, 10).unwrap(),
        b"verified"
    );
    for (hash, size, max) in [
        (sha(b"wrong"), 8, 10),
        (sha(b"verified"), 7, 10),
        (sha(b"verified"), 8, 7),
    ] {
        assert!(verify_blob(&path, &hash, size, max).is_err());
    }
    assert!(!valid_digest("sha256:../escape"));
    assert!(!valid_digest(&format!("sha256:{}", "A".repeat(64))));
}
#[test]
fn malicious_archive_paths_links_ownership_and_special_files_are_rejected() {
    for name in ["/host", "../host", "a/../../host", "a/../host"] {
        assert!(
            validate(&layer(name, b'0', "", 0, 0, 0o644)).is_err(),
            "{name}"
        );
    }
    for (path, link, kind) in [
        ("x", "../outside", b'2'),
        ("a/x", "../../outside", b'2'),
        ("x", "/host", b'1'),
        ("x", "../host", b'1'),
    ] {
        assert!(validate(&layer(path, kind, link, 0, 0, 0o644)).is_err());
    }
    for (uid, gid, mode) in [
        (1, 0, 0o644),
        (0, 42, 0o644),
        (0, 0, 0o4755),
        (0, 0, 0o2755),
    ] {
        assert!(validate(&layer("file", b'0', "", uid, gid, mode)).is_err());
    }
    for kind in [b'3', b'4', b'6', b'S'] {
        assert!(validate(&layer("special", kind, "", 0, 0, 0o644)).is_err());
    }
    for (path, kind, link) in [
        ("file", b'0', ""),
        ("./dir/", b'5', ""),
        ("dir/.wh.old", b'0', ""),
        ("dir/.wh..wh..opq", b'0', ""),
        ("link", b'2', "/bin/inside"),
        ("dir/link", b'2', "../inside"),
        ("hard", b'1', "inside"),
    ] {
        validate(&layer(path, kind, link, 0, 0, 0o644)).unwrap();
    }
}
#[test]
fn expanded_byte_inode_and_diff_id_budgets_are_independent() {
    let bytes = layer("file", b'0', "", 0, 0, 0o644);
    assert!(validate_layer(
        &bytes[..],
        &sha(&bytes),
        &mut ArchiveBudget {
            bytes: 1024,
            entries: 100
        }
    )
    .is_err());
    assert!(validate_layer(
        &bytes[..],
        &sha(&bytes),
        &mut ArchiveBudget {
            bytes: 10000,
            entries: 0
        }
    )
    .is_err());
    assert!(validate_layer(
        &bytes[..],
        &sha(b"tampered"),
        &mut ArchiveBudget {
            bytes: 10000,
            entries: 100
        }
    )
    .is_err());
    let mut budget = ArchiveBudget {
        bytes: bytes.len() as u64,
        entries: 1,
    };
    validate_layer(&bytes[..], &sha(&bytes), &mut budget).unwrap();
    assert_eq!(budget.bytes, 0);
    assert_eq!(budget.entries, 0);
}
#[test]
fn image_config_defaults_and_fixed_template_overrides_are_supported() {
    let config = json!({"config":{"Entrypoint":["/bin/tool","first"],"Cmd":["default"],
        "Env":["PATH=/bin","VALUE=image"],"WorkingDir":"/image","User":"0:0"}});
    let e = execution(&config, None).unwrap();
    assert_eq!(e.argv, ["/bin/tool", "first", "default"]);
    assert_eq!(e.workdir, "/image");
    let mut m = Manifest::parse(include_bytes!("../examples/foundation.yaml")).unwrap();
    let t = m.jobs.get_mut("app").unwrap();
    t.uid = 0;
    t.gid = 0;
    t.command = vec!["/fixed".into(), "argument".into()];
    t.workdir = "/fixed-dir".into();
    t.env.insert("VALUE".into(), "operator".into());
    let e = execution(&config, Some(t)).unwrap();
    assert_eq!(e.argv, ["/fixed", "argument"]);
    assert_eq!(e.env["VALUE"], "operator");
    assert_eq!(e.workdir, "/fixed-dir");
    t.command.clear();
    t.workdir.clear();
    let e = execution(&config, Some(t)).unwrap();
    assert_eq!(e.argv, ["/bin/tool", "first", "default"]);
    assert_eq!(e.workdir, "/image");
    for user in ["", "0", "0:0"] {
        execution(&json!({"config":{"Cmd":["/tool"],"User":user}}), None).unwrap();
    }
}
#[test]
fn unsupported_identity_mount_and_runtime_requirements_fail_closed() {
    for user in ["1000", "0:1", "1:0", "root", "root:root", "0:0:0"] {
        assert!(
            execution(&json!({"config":{"Cmd":["/tool"],"User":user}}), None).is_err(),
            "{user}"
        );
    }
    for (field, value) in [
        ("Volumes", json!({"/data":{}})),
        ("ExposedPorts", json!({"80/tcp":{}})),
        ("Healthcheck", json!({"Test":["CMD","probe"]})),
        ("OnBuild", json!(["RUN x"])),
        ("Shell", json!(["/bin/sh"])),
        ("ArgsEscaped", json!(true)),
        ("StopSignal", json!("SIGQUIT")),
        ("User", json!(42)),
        ("WorkingDir", json!(false)),
        ("WorkingDir", json!("/../escape")),
        ("Env", json!(["MISSING_EQUALS"])),
        ("Entrypoint", json!("/bin/tool")),
        ("UnknownRuntimeRequirement", json!(true)),
    ] {
        let mut v = json!({"config":{"Cmd":["/tool"]}});
        v["config"][field] = value;
        assert!(execution(&v, None).is_err(), "{field}");
    }
}
#[test]
fn image_symlinks_are_resolved_against_image_root() {
    let dir = tempfile::tempdir().unwrap();
    fs::create_dir(dir.path().join("bin")).unwrap();
    fs::write(dir.path().join("bin/tool"), b"image").unwrap();
    symlink("/bin/tool", dir.path().join("absolute")).unwrap();
    symlink("bin/tool", dir.path().join("relative")).unwrap();
    symlink("../outside", dir.path().join("escape")).unwrap();
    symlink("loop", dir.path().join("loop")).unwrap();
    fs::create_dir(dir.path().join("tmp")).unwrap();
    fs::write(dir.path().join("tmp/input"), b"mutable input").unwrap();
    symlink("/tmp/input", dir.path().join("mutable-alias")).unwrap();
    assert!(silo::image::immutable_path(dir.path(), "/mutable-alias").is_err());
    assert_eq!(
        image_path(dir.path(), "/absolute").unwrap(),
        dir.path().join("bin/tool")
    );
    assert_eq!(
        image_path(dir.path(), "/relative").unwrap(),
        dir.path().join("bin/tool")
    );
    assert!(image_path(dir.path(), "/escape").is_err());
    assert!(image_path(dir.path(), "/loop").is_err());
}
#[test]
fn pinned_executable_rejects_tampering_setid_and_symlink_resolution() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("tool");
    fs::write(&path, b"trusted executable").unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).unwrap();
    let pin = Tool {
        path: path.clone(),
        sha256: digest(b"trusted executable"),
    };
    pin.verify().unwrap();
    fs::write(&path, b"tampered").unwrap();
    assert!(pin.verify().is_err());
    fs::write(&path, b"trusted executable").unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o1777)).unwrap();
    assert!(
        pin.verify().is_err(),
        "sticky bit must never authorize a writable executable"
    );
    fs::set_permissions(&path, fs::Permissions::from_mode(0o4755)).unwrap();
    assert!(pin.verify().is_err());
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).unwrap();
    let link = dir.path().join("link");
    symlink(&path, &link).unwrap();
    assert!(Tool {
        path: link,
        sha256: pin.sha256
    }
    .verify()
    .is_err());
}
