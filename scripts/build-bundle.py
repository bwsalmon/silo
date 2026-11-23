#!/usr/bin/env python3
"""Build one reproducible Linux x86-64 archive; never publish it."""
import argparse
import json
import os
from pathlib import Path
import shutil
import tempfile
import tomllib

from bundle_common import ROOT, archive, capture, elf, fetch, run, sha, write_json

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--out", type=Path, default=ROOT / "dist")
args = parser.parse_args()
out = args.out.resolve()
out.mkdir(parents=True, exist_ok=True)
lock = json.loads((ROOT / "packaging/inputs.lock.json").read_text())
if os.uname().machine != lock["architecture"]:
    raise SystemExit("Only tested Linux x86-64 builds are supported")
run(["/usr/bin/python3", ROOT / "scripts/builder.py"])
run(["/usr/bin/python3", ROOT / "scripts/verify-rust.py"])
cache = ROOT / ".package-cache"
fetch(lock["rust"]["manifest_url"], lock["rust"]["manifest_sha256"], cache / "rust-manifest.toml")
cargo_bin = Path(os.environ.get("CARGO_HOME", str(Path.home() / ".cargo"))) / "bin"
cargo = cargo_bin / "cargo"
if not capture([cargo_bin / "rustc", "--version"]).startswith("rustc 1.85.1 (4eb161250 "):
    raise SystemExit("Run scripts/bootstrap-rust.py first")
sysroot = Path(capture([cargo_bin / "rustc", "--print", "sysroot"]).strip())
bwrap_input = fetch(lock["bubblewrap"]["url"], lock["bubblewrap"]["sha256"], cache / "bubblewrap.tar.xz")
epoch = lock["source_date_epoch"]
with tempfile.TemporaryDirectory(prefix="silo-package-") as temp:
    work = Path(temp)
    src = work / "source"; src.mkdir()
    # Snapshot the exact tracked/new source inputs, excluding ignored build outputs.
    files = capture(["git", "-C", ROOT, "ls-files", "--cached", "--others", "--exclude-standard"]).splitlines()
    # Measured reports are attestations about an archive, not inputs to that archive.
    files = [name for name in files if name != "docs/distribution-evidence.md"
             and not name.startswith("docs/evidence/distribution-")]
    for name in sorted(set(files)):
        source = ROOT / name
        if source.is_symlink() or not source.is_file():
            raise ValueError("Only regular source inputs are supported: " + name)
        target = src / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
    archive(src, work / "silo-source.tar.xz", epoch)
    source_sha = sha(work / "silo-source.tar.xz")
    env = dict(HOME=str(Path.home()), PATH=str(cargo_bin) + ":/usr/bin:/bin", CC="/usr/bin/gcc",
               CARGO_HOME=str(cargo_bin.parent),
               RUSTUP_HOME=os.environ.get("RUSTUP_HOME", str(Path.home() / ".rustup")),
               LC_ALL="C", TZ="UTC", SOURCE_DATE_EPOCH=str(epoch), CARGO_INCREMENTAL="0")
    vendor = work / "vendor"
    run([cargo, "vendor", "--locked", "--versioned-dirs", vendor], cwd=src, env=env, stdout=open(work / "vendor-config", "w"))
    cargo_home = work / "cargo-home"; cargo_home.mkdir()
    (cargo_home / "config.toml").write_text((work / "vendor-config").read_text())
    env.update(CARGO_HOME=str(cargo_home), CARGO_TARGET_DIR=str(work / "target"),
               RUSTFLAGS="--remap-path-prefix=" + str(work) + "=/build",
               CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER="/usr/bin/gcc")
    run([cargo, "build", "--frozen", "--offline", "--release", "--target", lock["target"]], cwd=src, env=env)
    metadata = json.loads(capture([cargo, "metadata", "--frozen", "--offline", "--format-version", "1"], cwd=src, env=env))
    # The unmodified, checksum-verified upstream source and exact build options travel with the binary.
    run(["/usr/bin/tar", "-xf", bwrap_input, "-C", work])
    bsrc = work / ("bubblewrap-" + lock["bubblewrap"]["version"])
    bbuild = work / "bwrap-build"
    options = lock["bubblewrap"]["meson_options"]
    run(["/usr/bin/meson", "setup", bbuild, bsrc] + options +
        ["-Dc_args=-ffile-prefix-map=" + str(work) + "=/build"], env=env)
    run(["/usr/bin/ninja", "-C", bbuild], env=env)
    bundle = work / "bundle"; (bundle / "libexec").mkdir(parents=True)
    shutil.copyfile(work / "target" / lock["target"] / "release/silo", bundle / "silo")
    shutil.copyfile(bbuild / "bwrap", bundle / "libexec/bwrap")
    run(["/usr/bin/strip", "--strip-all", bundle / "libexec/bwrap"])
    for path in [bundle / "silo", bundle / "libexec/bwrap"]:
        path.chmod(0o755)
    silo_elf = elf(bundle / "silo")
    bwrap_elf = elf(bundle / "libexec/bwrap")
    if silo_elf != {"interpreter": [], "needed": []}:
        raise ValueError("Silo must have no ELF interpreter or dynamic dependencies")
    if bwrap_elf != {"interpreter": ["/lib64/ld-linux-x86-64.so.2"], "needed": ["libc.so.6", "libcap.so.2"]}:
        raise ValueError("Unexpected Bubblewrap dependencies: " + str(bwrap_elf))
    (bundle / "source").mkdir()
    shutil.copyfile(bwrap_input, bundle / "source" / ("bubblewrap-" + lock["bubblewrap"]["version"] + ".tar.xz"))
    shutil.copyfile(work / "silo-source.tar.xz", bundle / "source/silo.tar.xz")
    archive(vendor, bundle / "source/rust-dependencies.tar.xz", epoch)
    notices = bundle / "notices"; notices.mkdir()
    shutil.copyfile(src / "LICENSE", notices / "Silo-Apache-2.0.txt")
    shutil.copyfile(bsrc / "COPYING", notices / "Bubblewrap-LGPL-2.0-or-later.txt")
    shutil.copytree(sysroot / "share/doc/rust", notices / "rust")
    shutil.copytree(src / "docs", bundle / "docs")
    shutil.copyfile(src / "README.md", bundle / "README.md")
    shutil.copytree(src / "examples", bundle / "examples")
    shutil.copyfile(src / "packaging/inputs.lock.json", bundle / "inputs.lock.json")
    shutil.copyfile(src / "packaging/builder.lock.json", bundle / "builder.lock.json")
    checksums = {p["name"] + "@" + p["version"]: p.get("checksum") for p in tomllib.loads((src / "Cargo.lock").read_text())["package"]}
    inventory = [{"name":p["name"], "version":p["version"], "license":p["license"],
                  "license_file":bool(p["license_file"]), "checksum":checksums.get(p["name"] + "@" + p["version"])}
                 for p in metadata["packages"]]
    write_json(bundle / "inventory.json", {
        "rust_packages_including_build_and_test": sorted(inventory, key=lambda p:(p["name"], p["version"])),
        "bubblewrap":{"version":lock["bubblewrap"]["version"],"license":"LGPL-2.0-or-later",
                      "source_sha256":lock["bubblewrap"]["sha256"],"changes":"none"},
        "runtime_requirements":{"silo":silo_elf,"libexec/bwrap":bwrap_elf},
        "external_runtime_libraries":"glibc >= 2.34 and libcap.so.2; not shipped; test on each worker",
        "notices":"Rust toolchain COPYRIGHT includes musl and compiler runtime notices; crate notices are in complete vendored source"})
    payload = {str(p.relative_to(bundle)):{"sha256":sha(p), "mode":format(p.stat().st_mode & 0o7777, "04o")}
               for p in sorted(bundle.rglob("*")) if p.is_file()}
    write_json(bundle / "bundle.json", {
        "format":1, "architecture":lock["architecture"], "target":lock["target"],
        "source_archive_sha256":source_sha, "source_date_epoch":epoch,
        "bwrap_version":lock["bubblewrap"]["version"],
        "elf":{"silo":silo_elf,"libexec/bwrap":bwrap_elf}, "files":payload})
    name = "silo-0.1.0-linux-x86_64"
    artifact = out / (name + ".tar.xz")
    archive(bundle, artifact, epoch, prefix=name + "/")
    (out / "SHA256SUMS").write_text(sha(artifact) + "  " + artifact.name + "\n")
    shutil.copyfile(bundle / "inventory.json", out / "inventory.json")
    print("Built", artifact, sha(artifact), flush=True)
