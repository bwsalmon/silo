#!/usr/bin/env python3
"""Run real full-system acceptance on an extracted archive in disposable task-owned resources."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

from bundle_common import ROOT, run, sha, write_json

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--archive", required=True, type=Path)
parser.add_argument("--sha256", required=True, help="trusted expected archive checksum")
parser.add_argument("--reports", required=True, type=Path, help="new report directory; never reuse old evidence")
args = parser.parse_args()
reports = args.reports.absolute()
reports.mkdir(mode=0o700)  # Existing/stale evidence cannot be reused.
status = {"result":"running", "qualification":{"status":"blocked", "reasons":["run incomplete"]},
          "archive_sha256":args.sha256, "cleanup":{"verified":False}}
write_json(reports / "bundle-qualification.json", status)
try:
    with tempfile.TemporaryDirectory(prefix="silo-artifact-", dir="/var/tmp") as temporary:
        destination = Path(temporary) / "install"
        validation = subprocess.run(["/usr/bin/python3", ROOT / "scripts/validate-bundle.py",
            "--archive", args.archive.resolve(), "--sha256", args.sha256, "--extract-to", destination],
            capture_output=True, text=True, check=True)
        info = json.loads(validation.stdout)
        helper = Path(info["bwrap"]["path"])
        binary = helper.parent.parent / "silo"
        write_json(reports / "installation.json", info)
        # Relocate the complete tree and re-resolve the pin; no old path or PATH fallback.
        relocated = destination / "relocated"
        binary.parent.rename(relocated)
        subprocess.run(["/usr/bin/python3", ROOT / "scripts/validate-bundle.py", "--root", relocated],
                       check=True, stdout=subprocess.DEVNULL)
        helper = relocated / "libexec/bwrap"
        binary = relocated / "silo"
        with (reports / "real-worker.log").open("w") as log:
            run(["/usr/bin/python3", ROOT / "tests/real_worker.py", "--suite", "all",
                 "--binary", binary, "--bwrap", helper, "--report", reports / "worker.json"],
                stdout=log, stderr=subprocess.STDOUT, env=dict(os.environ, PYTHONDONTWRITEBYTECODE="1"))
        worker = json.loads((reports / "worker.json").read_text())
        if worker["qualification"]["status"] != "qualified":
            raise ValueError("real worker qualification is blocked")
        if worker["pins"]["bwrap"] != {"path":str(helper), "sha256":sha(helper)}:
            raise ValueError("acceptance used a different helper")
        if worker["pins"]["silo_sha256"] != sha(binary) or len(worker.get("bundle_installation_denials", [])) != 2:
            raise ValueError("missing artifact or two-principal installation evidence")
        status.update(result="checks_passed", executable_sha256=sha(binary), bwrap_sha256=sha(helper),
                      worker_report_sha256=sha(reports / "worker.json"), relocation="passed")
    status.update(result="passed", cleanup={"verified":True},
                  qualification={"status":"qualified", "reasons":[],
                                 "scope":"only this archive and recorded disposable worker; no production approval"})
except BaseException as error:
    status.update(result="failed", error=repr(error),
                  qualification={"status":"blocked", "reasons":["archive acceptance or cleanup did not complete"]})
    raise
finally:
    write_json(reports / "bundle-qualification.json", status)
