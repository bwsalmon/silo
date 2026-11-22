//! Archive parser regression tests; extraction/isolation is separately exercised on a real worker.
use silo::{
    image::{validate_layer, ArchiveBudget},
    policy::digest,
};
fn validate(bytes: &[u8]) -> silo::error::Result<()> {
    validate_layer(
        bytes,
        &format!("sha256:{}", digest(bytes)),
        &mut ArchiveBudget {
            bytes: 1024 * 1024,
            entries: 100,
        },
    )
}
#[test]
fn pax_ownership_xattrs_sparse_and_path_traversal_fail_closed() {
    for (key, value) in [
        ("uid", b"1000".as_slice()),
        ("gid", b"1000".as_slice()),
        ("SCHILY.xattr.security.capability", b"capability".as_slice()),
        ("GNU.sparse.size", b"100000".as_slice()),
        ("path", b"../outside".as_slice()),
        ("linkpath", b"../../outside".as_slice()),
    ] {
        let mut archive = tar::Builder::new(Vec::new());
        archive.append_pax_extensions([(key, value)]).unwrap();
        let mut header = tar::Header::new_ustar();
        header.set_path("inside").unwrap();
        header.set_uid(0);
        header.set_gid(0);
        header.set_mode(0o644);
        header.set_size(0);
        if key == "linkpath" {
            header.set_entry_type(tar::EntryType::Symlink);
            header.set_link_name("safe").unwrap();
        }
        header.set_cksum();
        archive.append(&header, &[][..]).unwrap();
        let bytes = archive.into_inner().unwrap();
        assert!(validate(&bytes).is_err(), "{key}");
    }
}
#[test]
fn nonzero_data_after_tar_end_cannot_bypass_validation() {
    let mut archive = tar::Builder::new(Vec::new());
    let mut header = tar::Header::new_ustar();
    header.set_path("safe").unwrap();
    header.set_uid(0);
    header.set_gid(0);
    header.set_mode(0o644);
    header.set_size(0);
    header.set_cksum();
    archive.append(&header, &[][..]).unwrap();
    let mut bytes = archive.into_inner().unwrap();
    validate(&bytes).unwrap();
    bytes.extend_from_slice(b"unvalidated second archive");
    assert!(validate(&bytes).is_err());
}
