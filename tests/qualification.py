"""Qualification bookkeeping. These checks never replace real worker execution."""
from contextlib import contextmanager
import json
import os
from pathlib import Path

REQUIRED_SUITES = {"runner", "network", "volumes"}


def verdict(report):
    reasons = []
    if report.get("result") != "passed":
        reasons.append("real integration checks did not pass")
    if set(report.get("completed_suites", [])) != REQUIRED_SUITES:
        reasons.append("runner, network and volumes suites are all required")
    if report.get("cleanup", {}).get("verified") is not True:
        reasons.append("task-owned resource cleanup is unverified")
    if report.get("architecture") != "x86_64" or not report.get("kernel"):
        reasons.append("tested Linux x86-64 kernel identity is missing")
    for field in ["agent_kernel_profiles", "resources", "postgres_platform_digest",
                  "network_direct_measurements", "network_relay_measurements"]:
        if not report.get(field):
            reasons.append("missing real evidence: " + field)
    if not report.get("pins", {}).get("silo_sha256"):
        reasons.append("tested executable hash is missing")
    return {"status": "blocked" if reasons else "qualified", "reasons": reasons,
            "scope": "only the recorded disposable worker configuration; no production approval"}


def write_report(path, report):
    path = Path(path)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(report, indent=2) + "\n")
    os.replace(temporary, path)


@contextmanager
def finalization(report, path):
    """A cleanup exception must invalidate an otherwise successful run."""
    report["cleanup"] = {"verified": False}
    try:
        yield
    except BaseException as error:
        report["result"] = "failed"
        report["cleanup"]["error"] = repr(error)
        raise
    else:
        report["cleanup"]["verified"] = True
        if report.get("result") == "checks_passed":
            report["result"] = "passed"
    finally:
        report["qualification"] = verdict(report)
        write_report(path, report)


def namespace_policy():
    paths = ["/proc/sys/kernel/unprivileged_userns_clone",
             "/proc/sys/kernel/apparmor_restrict_unprivileged_userns",
             "/proc/sys/user/max_user_namespaces"]
    return {path: Path(path).read_text().strip() if Path(path).exists() else None for path in paths}
