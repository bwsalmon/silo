#!/usr/bin/env python3
"""Verify installed release compiler inputs against the pinned Rust distribution manifest."""
import json
import os
from pathlib import Path
import tarfile
import tomllib

from bundle_common import ROOT, capture, fetch

lock = json.loads((ROOT / "packaging/inputs.lock.json").read_text())["rust"]
cache = ROOT / ".package-cache"
manifest = tomllib.loads(fetch(lock["manifest_url"], lock["manifest_sha256"], cache / "rust-manifest.toml").read_text())
cargo_bin = Path(os.environ.get("CARGO_HOME", str(Path.home() / ".cargo"))) / "bin"
sysroot = Path(capture([cargo_bin / "rustc", "--print", "sysroot"]).strip())
components = [("rustc", "x86_64-unknown-linux-gnu"), ("cargo", "x86_64-unknown-linux-gnu"),
              ("rust-std", "x86_64-unknown-linux-gnu"), ("rust-std", "x86_64-unknown-linux-musl")]
for name, target in components:
    pin = manifest["pkg"][name]["target"][target]
    archive = fetch(pin["xz_url"], pin["xz_hash"], cache / (name + "-" + target + ".tar.xz"))
    with tarfile.open(archive, "r:xz") as tar:
        for member in tar:
            parts = Path(member.name).parts
            if len(parts) < 4 or not member.isfile():
                continue
            relative = Path(*parts[2:])
            if relative.parts[0] not in ("bin", "lib", "libexec") and not str(relative).startswith("share/doc/rust/"):
                continue
            installed = sysroot / relative
            if not installed.is_file() or installed.read_bytes() != tar.extractfile(member).read():
                raise SystemExit("Rust input differs from pinned component: " + str(installed))
print("Verified installed Rust compiler, Cargo and host/musl standard library payloads")
