#!/usr/bin/env python3
"""Fetch integrity-pinned builder packages; install only in a disposable Debian 12 builder."""
import argparse
import os
from pathlib import Path
import subprocess
import sys
import tarfile

from bundle_common import ROOT, capture, fetch
import json

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--install", action="store_true", help="requires root in a disposable builder")
parser.add_argument("--cache", type=Path, default=ROOT / ".package-cache")
args = parser.parse_args()
if capture(["uname", "-m"]).strip() != "x86_64":
    sys.exit("Only tested Linux x86-64 builders are supported")
packages = json.loads((ROOT / "packaging/builder.lock.json").read_text())["packages"]
debs = []
for package in packages:
    debs.append(fetch(package["url"], package["sha256"], args.cache / (package["name"] + ".deb")))
if args.install:
    if os.geteuid() != 0:
        sys.exit("--install requires root; never use on a live worker")
    subprocess.run(["apt-get", "install", "-y", "--allow-downgrades"] + [str(p.resolve()) for p in debs], check=True)
for package, deb in zip(packages, debs):
    actual = capture(["dpkg-query", "-W", "-f=${Version}", package["name"]]).strip()
    if actual != package["version"]:
        sys.exit("Builder package version differs: " + package["name"] + "; use builder.py --install in disposable builder")
    # Compare installed regular payloads with the SHA-verified .deb, not dpkg's mutable checksums.
    proc = subprocess.Popen(["dpkg-deb", "--fsys-tarfile", str(deb)], stdout=subprocess.PIPE)
    with tarfile.open(fileobj=proc.stdout, mode="r|") as tar:
        for member in tar:
            # Verify the compiler, interpreter, libraries, headers and pkg-config inputs.
            name = member.name.removeprefix("./")
            inputs = ("bin/", "sbin/", "lib/", "usr/bin/", "usr/sbin/", "usr/lib/", "usr/include/", "usr/share/pkgconfig/")
            if not member.isfile() or not name.startswith(inputs):
                continue
            installed = Path("/") / member.name.removeprefix("./")
            expected = tar.extractfile(member).read()
            if not installed.is_file() or installed.read_bytes() != expected:
                sys.exit("Builder input differs from pinned package: " + str(installed))
    if proc.wait():
        sys.exit("Cannot inspect pinned builder package")
print("Verified", len(packages), "integrity-pinned builder packages")
