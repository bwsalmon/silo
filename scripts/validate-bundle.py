#!/usr/bin/env python3
"""Verify/extract a distribution and print the exact protected worker.json bwrap pin."""
import argparse
import json
import os
from pathlib import Path, PurePosixPath
import stat
import sys
import tarfile

from bundle_common import ROOT, capture, elf, sha, trusted
sys.path.insert(0, str(ROOT / "tests"))
from mcp_client import smoke as mcp_smoke


def extract(archive, destination, expected):
    if sha(archive) != expected:
        raise ValueError("archive checksum mismatch; obtain checksum through a trusted channel")
    if destination.exists():
        raise ValueError("extraction destination must not exist")
    trusted(destination.parent)
    with tarfile.open(archive, "r:xz") as tar:
        members = tar.getmembers()
        names = set()
        for m in members:
            path = PurePosixPath(m.name)
            if path.is_absolute() or ".." in path.parts or m.name in names or not (m.isdir() or m.isfile()):
                raise ValueError("unsafe or duplicate archive entry")
            if m.mode != (0o755 if m.isdir() or m.name.endswith(("/silo", "/libexec/bwrap")) else 0o644):
                # Source files can be executable in the source tar, not in the outer bundle.
                raise ValueError("unexpected archive permissions: " + m.name)
            names.add(m.name)
        roots = {PurePosixPath(m.name).parts[0] for m in members}
        if len(roots) != 1:
            raise ValueError("archive requires one installation root")
        destination.mkdir(mode=0o700)
        try:
            for m in members:
                path = destination / m.name
                if m.isdir():
                    path.mkdir(parents=True, exist_ok=True, mode=0o755)
                else:
                    path.parent.mkdir(parents=True, exist_ok=True)
                    with path.open("xb") as out, tar.extractfile(m) as source:
                        import shutil
                        shutil.copyfileobj(source, out)
                    path.chmod(m.mode)
        except BaseException:
            import shutil
            shutil.rmtree(destination)
            raise
    return destination / roots.pop()


def validate(root):
    root = Path(root)
    trusted(root)
    manifest_path = root / "bundle.json"
    trusted(manifest_path)
    manifest = json.loads(manifest_path.read_text())
    if manifest["format"] != 1 or manifest["architecture"] != "x86_64":
        raise ValueError("unsupported bundle format or architecture")
    if os.uname().machine != "x86_64":
        raise ValueError("only tested Linux x86-64 installations are supported")
    actual = set()
    for p in root.rglob("*"):
        trusted(p)
        if stat.S_IMODE(p.stat().st_mode) & 0o7000:
            raise ValueError("special bundle permissions are forbidden")
        if p.is_dir():
            continue
        if not p.is_file():
            raise ValueError("bundle must contain regular non-setuid/non-setgid files")
        name = str(p.relative_to(root))
        actual.add(name)
        if name == "bundle.json":
            if stat.S_IMODE(p.stat().st_mode) != 0o644:
                raise ValueError("bad manifest mode")
            continue
        record = manifest["files"].get(name)
        if record is None or sha(p) != record["sha256"] or format(stat.S_IMODE(p.stat().st_mode), "04o") != record["mode"]:
            raise ValueError("bundle file checksum/mode mismatch: " + name)
    if actual != set(manifest["files"]) | {"bundle.json"}:
        raise ValueError("bundle has missing or unlisted files")
    for binary in ("silo", "libexec/bwrap"):
        if elf(root / binary) != manifest["elf"][binary]:
            raise ValueError("ELF requirements differ: " + binary)
    if manifest["elf"]["silo"] != {"interpreter": [], "needed": []}:
        raise ValueError("Silo is not static")
    if manifest["elf"]["libexec/bwrap"] != {
        "interpreter":["/lib64/ld-linux-x86-64.so.2"], "needed":["libc.so.6", "libcap.so.2"]}:
        raise ValueError("unexpected helper interpreter/dependencies")
    helper = root / "libexec/bwrap"
    env = {"PATH":"/usr/bin:/bin", "LC_ALL":"C"}
    # --list resolves actual loader/library requirements without starting any job.
    resolved = capture(["/lib64/ld-linux-x86-64.so.2", "--list", helper], env=env)
    for line in resolved.splitlines():
        words = line.strip().split()
        if "=>" in words:
            library = words[words.index("=>") + 1]
            if not library.startswith("/"):
                raise ValueError("helper library is unavailable")
            trusted(Path(library).resolve(strict=True))
    version = capture([helper, "--version"], env=env).strip()
    if version != "bubblewrap " + manifest["bwrap_version"]:
        raise ValueError("unexpected helper version")
    capture([root / "silo", "--help"], env=env)
    for example in ("foundation.yaml", "job.yaml", "postgres.yaml"):
        result = json.loads(capture([root / "silo", "validate", root / "examples" / example, "--json"], env=env))
        if result.get("valid") is not True:
            raise ValueError("bundled example validation failed")
    mcp = mcp_smoke(root / "silo", root / "examples/foundation.yaml")
    return {"mcp":mcp, "bwrap":{"path":str(helper), "sha256":manifest["files"]["libexec/bwrap"]["sha256"]},
            "silo_sha256":manifest["files"]["silo"]["sha256"],
            "library_resolution":resolved, "qualification":"not performed; run full real-worker acceptance"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path)
    parser.add_argument("--archive", type=Path)
    parser.add_argument("--sha256")
    parser.add_argument("--extract-to", type=Path)
    args = parser.parse_args()
    if args.archive:
        if not args.sha256 or not args.extract_to or args.root:
            parser.error("--archive requires --sha256 and --extract-to, without --root")
        root = extract(args.archive.resolve(), args.extract_to.absolute(), args.sha256)
    else:
        if not args.root or args.sha256 or args.extract_to:
            parser.error("use --root or --archive with --sha256 and --extract-to")
        root = args.root.absolute()
    print(json.dumps(validate(root), indent=2))


if __name__ == "__main__":
    main()
