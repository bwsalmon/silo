"""Distribution helpers. Validation does not qualify a worker."""
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import tarfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, **kwargs)


def capture(args, **kwargs):
    return run(args, stdout=subprocess.PIPE, text=True, **kwargs).stdout


def fetch(url, digest, path):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    if not path.exists():
        temporary = path.with_suffix(".download")
        with urllib.request.urlopen(url, timeout=90) as response, temporary.open("wb") as out:
            import shutil
            shutil.copyfileobj(response, out)
        temporary.replace(path)
    if sha(path) != digest:
        raise ValueError("input checksum mismatch: " + str(path))
    return path


def write_json(path, data):
    Path(path).write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")


def archive(source, output, epoch, prefix=""):
    """Canonical tar.xz: sorted members, fixed modes/ownership/times, no host paths."""
    with tarfile.open(output, "w:xz", preset=9, format=tarfile.USTAR_FORMAT) as tar:
        for path in sorted(Path(source).rglob("*")):
            if path.is_symlink():
                raise ValueError("source archive must not contain symlinks: " + str(path))
            info = tar.gettarinfo(str(path), arcname=prefix + str(path.relative_to(source)))
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            info.mtime = epoch
            info.mode = 0o755 if path.is_dir() or path.stat().st_mode & 0o111 else 0o644
            if path.is_file():
                with path.open("rb") as f:
                    tar.addfile(info, f)
            else:
                tar.addfile(info)


def trusted(path):
    path = Path(path)
    if not path.is_absolute() or path.resolve(strict=True) != path:
        raise ValueError("installation paths must be absolute and symlink-free")
    for index, item in enumerate([path] + list(path.parents)):
        mode = item.stat()
        sticky_root_parent = index > 0 and mode.st_uid == 0 and stat.S_ISDIR(mode.st_mode) and mode.st_mode & stat.S_ISVTX
        if mode.st_uid not in (0, os.getuid()) or mode.st_mode & 0o022 and not sticky_root_parent:
            raise ValueError("installation requires trusted owners and protected permissions: " + str(item))


def elf(path):
    header = capture(["/usr/bin/readelf", "-h", path])
    if "ELF64" not in header or "Advanced Micro Devices X86-64" not in header:
        raise ValueError("only Linux x86-64 ELF64 is supported: " + str(path))
    segments = capture(["/usr/bin/readelf", "-l", path])
    dynamic = capture(["/usr/bin/readelf", "-d", path])
    import re
    needed = re.findall(r"\(NEEDED\).*?\[(.*?)\]", dynamic)
    interpreter = re.findall(r"Requesting program interpreter: (.*?)\]", segments)
    if "(RPATH)" in dynamic or "(RUNPATH)" in dynamic:
        raise ValueError("ELF library search path is forbidden")
    return {"interpreter": interpreter, "needed": sorted(needed)}
