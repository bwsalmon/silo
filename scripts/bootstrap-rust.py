#!/usr/bin/env python3
"""Install the exact Rust toolchain in a disposable builder."""
import json
from pathlib import Path
import os
from bundle_common import ROOT, fetch, run

lock = json.loads((ROOT / "packaging/inputs.lock.json").read_text())["rust"]
cache = ROOT / ".package-cache"
fetch(lock["manifest_url"], lock["manifest_sha256"], cache / "rust-manifest.toml")
installer = fetch(lock["bootstrap_url"], lock["bootstrap_sha256"], cache / "rustup-init")
installer.chmod(0o700)
run([installer, "-y", "--no-modify-path", "--default-toolchain", "none", "--profile", "minimal"])
cargo_home = Path(os.environ.get("CARGO_HOME", str(Path.home() / ".cargo")))
run([cargo_home / "bin/rustup", "toolchain", "install", lock["version"], "--profile", "minimal",
     "--target", "x86_64-unknown-linux-musl", "--component", "rustfmt", "--component", "clippy"])
