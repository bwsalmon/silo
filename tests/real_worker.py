#!/usr/bin/env python3
"""Opt-in real-kernel acceptance suite. Provisions ONLY disposable task-owned resources.
Requires non-root execution, passwordless sudo, cgroup v2 with cpu/memory/pids delegation,
user namespaces, Bubblewrap >=0.8, umoci, skopeo, openssl, musl-gcc and a built silo.
Never changes sysctls, firewall rules, the parent cgroup's limits/controllers, or global CAs.
"""
import argparse
import copy
import gzip
import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import ssl
import subprocess
import tarfile
import tempfile
import threading
import time
import sys

HELLO = "docker.io/library/hello-world@sha256:5e22040d441e5fb3aed38368acbe8486b575d7018df38dbdfbc7311fbb2ef3a9"
OCI = "application/vnd.oci.image."
REPORT = {"kind": "real-worker", "passed": [], "pins": {}}


def run(args, **kwargs):
    result = subprocess.run([str(v) for v in args], capture_output=True, **kwargs)
    if result.returncode:
        raise RuntimeError((args, result.returncode, result.stdout, result.stderr))
    return result


def sha(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def protected(path, data):
    path.write_bytes(data if isinstance(data, bytes) else data.encode())
    path.chmod(0o600)


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


def check(name, condition):
    if not condition:
        raise AssertionError(name)
    REPORT["passed"].append(name)
    print("PASS", name, flush=True)


def eventually(fn, description, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = fn()
        if value:
            return value
        time.sleep(0.02)
    raise AssertionError("Timed out: " + description)


def tar_layer(entries):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w", format=tarfile.USTAR_FORMAT) as tar:
        for name, data, kind, link, uid, gid, mode in entries:
            info = tarfile.TarInfo(name)
            info.uid, info.gid, info.mode, info.mtime = uid, gid, mode, 0
            info.type, info.linkname = kind, link
            info.size = len(data) if kind == tarfile.REGTYPE else 0
            tar.addfile(info, io.BytesIO(data))
    return output.getvalue()


def file(name, data=b"x", uid=0, gid=0, mode=0o644):
    return (name, data, tarfile.REGTYPE, "", uid, gid, mode)


class Registry(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, certs):
        self.objects = {}
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.reply(True)

            def do_HEAD(self):
                self.reply(False)

            def reply(self, body):
                if self.path in ("/v2/", "/v2"):
                    data, media = b"{}", "application/json"
                else:
                    digest = self.path.split("/")[-1]
                    found = self.server.objects.get(digest)
                    if found is None:
                        self.send_error(404)
                        return
                    data, media = found
                self.send_response(200)
                self.send_header("Content-Type", media)
                self.send_header("Content-Length", str(len(data)))
                self.send_header("Docker-Content-Digest", sha(data))
                self.end_headers()
                if body:
                    self.wfile.write(data)

            def log_message(self, *args):
                pass
        super().__init__(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(certs / "ca.crt", certs / "key.pem")
        self.socket = context.wrap_socket(self.socket, server_side=True)
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

    def put(self, data, media):
        digest = sha(data)
        self.objects[digest] = (data, media)
        return {"mediaType": media, "digest": digest, "size": len(data)}

    def image(self, config, layers, index=False):
        desc = []
        diffs = []
        for layer in layers:
            diffs.append(sha(layer))
            desc.append(self.put(gzip.compress(layer, mtime=0), OCI + "layer.v1.tar+gzip"))
        cfg = {"architecture": "amd64", "os": "linux", "config": config,
               "rootfs": {"type": "layers", "diff_ids": diffs}}
        m = {"schemaVersion": 2, "mediaType": OCI + "manifest.v1+json",
             "config": self.put(encoded(cfg), OCI + "config.v1+json"), "layers": desc}
        descriptor = self.put(encoded(m), OCI + "manifest.v1+json")
        if index:
            descriptor["platform"] = {"os": "linux", "architecture": "amd64"}
            descriptor = self.put(encoded({"schemaVersion": 2, "mediaType": OCI + "index.v1+json",
                                          "manifests": [descriptor]}), OCI + "index.v1+json")
        return "localhost:%d/silo/probe@%s" % (self.server_port, descriptor["digest"])


class Supervisor:
    def __init__(self, binary, workspace, worker, manifest, restriction=None):
        self.dir = Path(tempfile.mkdtemp(prefix="state-", dir=workspace))
        self.operator = self.dir / "operator"
        protected(self.operator, secrets.token_hex(32))
        self.endpoint = self.dir / "endpoint.json"
        self.manifest = self.dir / "manifest.json"
        self.manifest.write_bytes(encoded(manifest))
        if worker:
            protected(self.dir / "worker.json", encoded(worker))
        # Deliberately pass a live non-CLOEXEC descriptor and secret environment to the supervisor.
        # The workload must never inherit either.
        self.output = (self.dir / "supervisor-output").open("wb")
        launcher = workspace / "launch-supervisor.py"
        self.pid_file = self.dir / "supervisor.pid"
        self.process = subprocess.Popen(["sudo", "python3", str(launcher), str(os.getuid()), str(os.getgid()),
            str(self.operator), str(workspace / "host-secret"), str(self.pid_file),
            str(restriction or binary)] + ([str(binary)] if restriction else []) + [
            "supervisor", "--state-dir", str(self.dir), "--auth-fd", "3", "--json"],
            stdout=self.output, stderr=subprocess.STDOUT)
        eventually(lambda: self.endpoint.exists() or self.process.poll() is not None, "supervisor startup")
        assert self.endpoint.exists(), (self.dir / "supervisor-output").read_text()
        activation = self.call(["up", self.manifest, "--no-start"])
        self.policy = activation["policy"]

    def call(self, args, credential=None, expected=0, extra_fds=()):
        credential = credential or self.operator
        with credential.open("rb") as auth:
            result = subprocess.run(
                [str(v) for v in [BINARY] + list(args) + ["--endpoint", self.endpoint,
                    "--auth-fd", auth.fileno(), "--json"]],
                pass_fds=(auth.fileno(),) + extra_fds, capture_output=True, timeout=310)
        value = json.loads(result.stdout)
        assert result.returncode == expected, (args, result.returncode, value, result.stderr)
        return value

    def issue(self, session, templates, permissions=None, max_instances=2, expires=None):
        grant = {"session": session, "project": "worker-test", "policy": self.policy,
                 "expires_at": expires or int(time.time()) + 180,
                 "scope": {"templates": templates, "permissions": permissions or ["run", "list", "status", "logs", "stop"],
                           "max_instances": max_instances}}
        path = self.dir / (session + "-grant.json")
        path.write_bytes(encoded(grant))
        token = self.dir / (session + "-token")
        with token.open("wb") as output:
            result = self.call(["operator", "issue", path, "--token-fd", output.fileno()],
                               extra_fds=(output.fileno(),))
        token.chmod(0o600)
        return token, result["grant"]

    def terminal(self, instance, credential=None, timeout=15):
        def poll():
            result = self.call(["status", instance["id"]], credential)
            return result if result["state"] in ("exited", "failed") else None
        return eventually(poll, "verified terminal state", timeout)

    def close(self):
        if self.process.poll() is None:
            try:
                for instance in self.call(["ps"])["instances"]:
                    if instance["state"] not in ("exited", "failed"):
                        self.call(["stop", instance["id"]])
                        self.terminal(instance)
                self.call(["operator", "shutdown"])
                self.process.wait(timeout=10)
            finally:
                if self.process.poll() is None:
                    if self.pid_file.exists():
                        try:
                            os.kill(int(self.pid_file.read_text()), 9)
                        except ProcessLookupError:
                            pass
                    self.process.kill()
                    self.process.wait()
        self.output.close()


def kernel_counters(logs, name):
    return dict((k, int(v)) for k, v in
                (line.split() for line in logs["resources"][name].splitlines()))


def host_processes(group):
    path = group / "cgroup.procs"
    return [int(v) for v in path.read_text().split()] if path.exists() else []


def suite(workspace, worker, registry, probe):
    first = tar_layer([file("probe", probe, mode=0o755), file("invalid-elf", b"not an executable", mode=0o755), file("keep", b"keep"),
        file("old"), file("opaque/stale"),
        ("absolute-link", b"", tarfile.SYMTYPE, "/keep", 0, 0, 0o777)])
    second = tar_layer([file(".wh.old", b""), file("opaque/.wh..wh..opq", b""),
                        file("opaque/fresh"), ("work", b"", tarfile.DIRTYPE, "", 0, 0, 0o755)])
    config = {"Entrypoint": ["/probe", "config"], "Cmd": ["default"],
              "Env": ["IMAGE=from-image", "VALUE=image"], "WorkingDir": "/work", "User": "0:0"}
    image = registry.image(config, [first, second], index=True)
    REPORT["pins"]["fixture_index"] = image
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    port = listener.getsockname()[1]
    limits = {"processes": 64, "memory_bytes": 134217728, "cpu_millis": 1000,
              "scratch_bytes": 8388608, "connections": 1, "duration_secs": 60, "max_instances": 2}
    def job(command=None, ref=image, **overrides):
        value = {"image": ref, "command": command or [], "env": {"VALUE": "operator"},
                 "workdir": "", "uid": 0, "gid": 0, "limits": copy.deepcopy(limits),
                 "agent": ["run", "list", "status", "logs", "stop"]}
        value.update(overrides)
        return value
    jobs = {"hello": job(ref=HELLO), "defaults": job(),
            "override": job(["/probe", "config", "fixed"]),
            "check": job(["/probe", "check", str(workspace / "host-secret"), str(port)]),
            "disk": job(["/probe", "disk"]), "pids": job(["/probe", "pids"]),
            "memory": job(["/probe", "memory"]), "cpu": job(["/probe", "cpu"]),
            "tree": job(["/probe", "tree"]), "orphan": job(["/probe", "orphan"]),
            "sleep": job(["/probe", "sleep"]), "sleep2": job(["/probe", "sleep"]),
            "exit": job(["/probe", "exit"]), "logs": job(["/probe", "logs"]),
            "bad-uid": job(["/probe", "exit"], uid=1),
            "bad-gid": job(["/probe", "exit"], gid=1),
            "bad-setup": job(["/probe", "exit"], workdir="/probe"),
            "bad-elf": job(["/invalid-elf"]),
            "missing": job(["/absent"])}
    jobs["cpu"]["limits"]["cpu_millis"] = 200
    jobs["duration"] = job(["/probe", "sleep"])
    jobs["duration"]["limits"]["duration_secs"] = 2
    for label, cfg in [
        ("image-user", {**config, "User": "1000:1000"}),
        ("image-named-user", {**config, "User": "root"}),
        ("image-volume", {**config, "Volumes": ["invalid-schema"]}),
        ("image-health", {**config, "Healthcheck": {"Test": ["CMD", "probe"]}}),
    ]:
        jobs[label] = job(["/probe", "exit"], ref=registry.image(cfg, [first]))
    for label, entries in [
        ("archive-absolute", [file(str(workspace / "escaped"))]),
        ("archive-parent", [file("../escaped")]),
        ("archive-link", [("x", b"", tarfile.SYMTYPE, "../../outside", 0, 0, 0o777)]),
        ("archive-owner", [file("owned", uid=65535)]),
        ("archive-setuid", [file("owned", mode=0o4755)]),
        ("archive-device", [("device", b"", tarfile.CHRTYPE, "", 0, 0, 0o600)]),
    ]:
        jobs[label] = job(["/probe", "exit"], ref=registry.image(config, [tar_layer(entries)]))
    corrupt = registry.image(config, [tar_layer([file("unique-corrupt-blob")])])
    corrupt_manifest = json.loads(registry.objects[corrupt.split("@")[1]][0])
    layer_digest = corrupt_manifest["layers"][0]["digest"]
    blob, media = registry.objects[layer_digest]
    registry.objects[layer_digest] = (blob[:-1] + bytes([blob[-1] ^ 1]), media)
    jobs["image-corrupt-blob"] = job(["/probe", "exit"], ref=corrupt)
    manifest_tamper = registry.image({**config, "Labels": {"tamper": "raw-manifest"}}, [])
    manifest_digest = manifest_tamper.split("@")[1]
    raw, media = registry.objects[manifest_digest]
    registry.objects[manifest_digest] = (raw + b" ", media)
    jobs["image-corrupt-manifest"] = job(["/probe", "exit"], ref=manifest_tamper)
    # An archive writes through a prior absolute symlink. umoci must scope it inside rootfs.
    pivot_first = tar_layer([file("probe", probe, mode=0o755),
        ("pivot", b"", tarfile.SYMTYPE, str(workspace), 0, 0, 0o777)])
    pivot_image = registry.image(config, [pivot_first, tar_layer([file("pivot/escaped")])])
    jobs["umoci-symlink"] = job(["/probe", "exit"], ref=pivot_image, workdir="/")
    manifest = {"version": 1, "project": "worker-test", "jobs": jobs, "network": {"external": "deny"}}
    supervisors = []
    def server(config=worker, doc=manifest, restriction=None):
        s = Supervisor(BINARY, workspace, config, doc, restriction)
        supervisors.append(s)
        return s
    try:
        s = server()
        token, grant = s.issue("agent-a", list(jobs))
        other, _ = s.issue("agent-b", list(jobs), max_instances=1)
        no_logs, _ = s.issue("no-logs", ["sleep"], ["run", "status", "stop"])
        def execute(name, expected_code=0):
            instance = s.call(["run", name], token)
            terminal = s.terminal(instance, token)
            logs = s.call(["logs", instance["id"]], token)
            if expected_code is not None:
                assert terminal["state"] == "exited" and terminal["exit_code"] == expected_code, (name, terminal, logs)
            assert not (Path(worker["cgroup"]) / instance["id"]).exists()
            return instance, terminal, logs
        stock, _, hello = execute("hello")
        REPORT["pins"]["stock_platform_manifest"] = stock["image_digest"]
        check("authenticated approved stock OCI image runs with verified output", "Hello" in hello["stdout"])
        REPORT["pins"]["stock_image"] = HELLO
        _, _, defaults = execute("defaults")
        check("OCI entrypoint/args/env/workdir defaults", "arg=default cwd=/work image=from-image value=operator" in defaults["stdout"])
        _, _, fixed = execute("override")
        check("fixed template command/env override", "arg=fixed" in fixed["stdout"])

        instance = s.call(["run", "check"], token)
        group = Path(worker["cgroup"]) / instance["id"]
        pids = host_processes(group)
        helper = next(pid for pid in pids if b"__namespace_helper" in Path("/proc/%d/cmdline" % pid).read_bytes())
        ns = run(["sudo", "readlink", "/proc/%d/ns/net" % helper, "/proc/%d/ns/pid" % helper,
                  "/proc/%d/ns/mnt" % helper], text=True).stdout.splitlines()
        deny_join = subprocess.run(["sudo", "-u", "#" + str(worker["agent_uid"]), "nsenter",
            "--target", str(helper), "--net", "true"], capture_output=True)
        REPORT["native_namespace_join"] = {"exit": deny_join.returncode, "stderr": deny_join.stderr.decode()}
        check("native agent cannot join helper namespaces", deny_join.returncode != 0
              and any(error in deny_join.stderr for error in [b"permitted", b"Permission denied"]))
        terminal = s.terminal(instance, token)
        logs = s.call(["logs", instance["id"]], token)
        assert terminal.get("exit_code") == 0, (terminal, logs)
        values = dict(line.split("=", 1) for line in logs["stdout"].splitlines() if "=" in line)
        check("helper shares private net but has separate PID/mount namespaces",
              values["net"] == ns[0] and values["pid"] != ns[1] and values["mnt"] != ns[2])
        check("real descriptor/env noninheritance; denied host files/network/nested namespaces; zero caps",
              "check=ok" in logs["stdout"])
        check("real layered whiteouts and opaque directory handling", "check=ok" in logs["stdout"])
        result = subprocess.run(["sudo", "-u", "#" + str(worker["agent_uid"]), "ls", worker["storage"]],
                                capture_output=True)
        check("native agent OS principal cannot browse protected image storage", result.returncode != 0)

        _, _, disk = execute("disk")
        check("bounded scratch tmpfs reaches actual ENOSPC", "scratch_bytes=4194304" in disk["stdout"])
        _, _, pids = execute("pids")
        check("actual process exhaustion and orphan cleanup", kernel_counters(pids, "pids_events")["max"] > 0)
        _, terminal, memory = execute("memory", None)
        check("actual cgroup memory exhaustion fails without fabricated success",
              terminal["state"] == "failed" and "exit_code" not in terminal
              and kernel_counters(memory, "memory_events")["oom_kill"] > 0)
        _, _, cpu = execute("cpu")
        REPORT["resources"] = {"scratch_stdout": disk["stdout"],
            "pids_events": kernel_counters(pids, "pids_events"),
            "memory_events": kernel_counters(memory, "memory_events"),
            "cpu_stat": kernel_counters(cpu, "cpu_stat"), "cpu_stdout": cpu["stdout"]}
        check("CPU is kernel-throttled", kernel_counters(cpu, "cpu_stat")["nr_throttled"] > 0 and "cpu_ms=" in cpu["stdout"])
        execute("exit", 17)
        check("nonzero workload exit code preserved", True)
        _, _, logs = execute("logs")
        check("stdout snapshot is bounded", len(logs["stdout"]) == 16384)

        for name in [n for n in jobs if n.startswith(("bad-", "missing", "image-", "archive-"))]:
            result = s.call(["run", name], token, expected=5)
            assert result["error"]["code"] in ("launch_failed", "unsupported_worker"), (name, result)
            print("PASS rejected", name, result["error"]["message"], flush=True)
        check("unsupported identities/config/mount requirements and malicious archives rejected",
              not (workspace / "escaped").exists())
        # The pinned upstream umoci scopes this absolute symlink write inside rootfs.
        result = s.call(["run", "umoci-symlink"], token, expected=0)
        s.terminal(result, token)
        staged = Path(worker["storage"]) / ("img_" + hashlib.sha256(pivot_image.encode()).hexdigest()) / "bundle/rootfs"
        check("mature unpacker scopes absolute layer symlinks inside rootfs",
              (staged / str(workspace).lstrip("/") / "escaped").is_file()
              and not (workspace / "escaped").exists())

        tree = s.call(["run", "tree"], token)
        group = Path(worker["cgroup"]) / tree["id"]
        eventually(lambda: len(host_processes(group)) >= 5, "grandchild creation")
        pids = host_processes(group)
        check("cross-principal status invisible", s.call(["status", tree["id"]], other, expected=3)["error"]["code"] == "not_found")
        check("cross-principal stop invisible", s.call(["stop", tree["id"]], other, expected=3)["error"]["code"] == "not_found")
        second = s.call(["run", "sleep2"], token)
        result = s.call(["run", "sleep"], token, expected=4)
        check("worker-wide instance ceiling enforced", result["error"]["code"] == "quota_exceeded")
        s.call(["stop", tree["id"]], token)
        s.terminal(tree, token)
        s.call(["stop", second["id"]], token)
        s.terminal(second, token)
        check("whole process tree including setsid grandchildren removed",
              not group.exists() and all(not Path("/proc/%d" % pid).exists() for pid in pids))
        orphan, _, _ = execute("orphan")
        check("normal parent exit also removes leftover children", not (Path(worker["cgroup"]) / orphan["id"]).exists())
        owned = s.call(["run", "sleep"], other)
        result = s.call(["run", "sleep2"], other, expected=4)
        check("grant instance quota enforced separately", result["error"]["code"] == "quota_exceeded")
        s.call(["stop", owned["id"]], other); s.terminal(owned, other)
        own = s.call(["run", "sleep"], no_logs)
        check("logs require separate permission", s.call(["logs", own["id"]], no_logs, expected=3)["error"]["code"] == "not_found")
        s.call(["stop", own["id"]], no_logs); s.terminal(own, no_logs)
        start = time.time(); duration = s.call(["run", "duration"], token); s.terminal(duration, token)
        check("duration bound enforced", time.time() - start < 4)
        expiring, _ = s.issue("expiring", ["sleep"], expires=int(time.time()) + 3)
        lease = s.call(["run", "sleep"], expiring)
        expiry_start = time.time(); s.terminal(lease)
        check("credential lease independently bounded", time.time() - expiry_start < 4)
        revoked = s.call(["run", "tree"], token)
        s.call(["operator", "revoke", grant])
        s.terminal(revoked)
        check("security revocation tears down grant-owned tree",
              not (Path(worker["cgroup"]) / revoked["id"]).exists())
        s.close()

        no_worker = server(config=None)
        error = no_worker.call(["run", "defaults"], expected=5)
        check("missing worker configuration fails closed precisely",
              error["error"]["code"] == "unsupported_worker" and "worker.json" in error["error"]["message"])
        no_worker.close()
        wrong_pin = copy.deepcopy(worker); wrong_pin["bwrap"]["sha256"] = "0" * 64
        bad_pin = server(config=wrong_pin)
        error = bad_pin.call(["run", "defaults"], expected=5)
        check("wrong Bubblewrap pin rejected", "SHA-256" in error["error"]["message"])
        bad_pin.close()
        wrong_storage = copy.deepcopy(worker); wrong_storage["storage"] = str(workspace)
        bad_storage = server(config=wrong_storage)
        error = bad_storage.call(["run", "defaults"], expected=5)
        check("unbounded/shared storage rejected", "tmpfs" in error["error"]["message"])
        bad_storage.close()
        wrong_group = copy.deepcopy(worker); wrong_group["cgroup"] = str(workspace)
        bad_group = server(config=wrong_group)
        error = bad_group.call(["run", "defaults"], expected=5)
        check("mock cgroup files cannot qualify a real worker", "cgroup v2 filesystem" in error["error"]["message"])
        bad_group.close()

        cpu_control = Path(worker["cgroup"]) / "cpu.max"
        previous_mode = cpu_control.stat().st_mode & 0o777
        cpu_control.chmod(0o400)
        try:
            absent_cpu = server()
            error = absent_cpu.call(["run", "defaults"], expected=5)
            check("actual missing writable CPU controller fails closed precisely",
                  error["error"]["code"] == "unsupported_worker" and "cpu.max" in error["error"]["message"])
            absent_cpu.close()
        finally:
            cpu_control.chmod(previous_mode)

        blocked = server(restriction=workspace / "deny-userns")
        error = blocked.call(["run", "defaults"], expected=5)
        check("kernel denial of user namespaces fails closed precisely",
              error["error"]["code"] == "unsupported_worker" and "CLONE_NEWUSER" in error["error"]["message"])
        blocked.close()

        crash = server()
        active = crash.call(["run", "tree"])
        group = Path(worker["cgroup"]) / active["id"]
        eventually(lambda: len(host_processes(group)) >= 5, "crash tree creation")
        os.kill(int(crash.pid_file.read_text()), 9); crash.process.wait()
        eventually(lambda: "populated 0" in (group / "cgroup.events").read_text(), "parent death tree cleanup")
        with crash.operator.open("rb") as auth:
            recovery = subprocess.run([BINARY, "supervisor", "--state-dir", str(crash.dir),
                "--auth-fd", str(auth.fileno()), "--json"], pass_fds=(auth.fileno(),), capture_output=True, timeout=10)
        check("supervisor crash kills descendants and refuses stale endpoint recovery",
              recovery.returncode == 5 and crash.endpoint.exists())
        group.rmdir()  # verified empty task-owned group only; no reattachment.
    finally:
        for s in reversed(supervisors):
            s.close()
        listener.close()


DENY_SOURCE = r"""
#define _GNU_SOURCE
#include <errno.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
#include <stddef.h>
#include <sys/prctl.h>
#include <sys/syscall.h>
#include <unistd.h>
int main(int argc, char **argv) {
    struct sock_filter f[] = {
        BPF_STMT(BPF_LD|BPF_W|BPF_ABS,0),
        BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K,SYS_unshare,0,1),
        BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ERRNO|EPERM),
        BPF_STMT(BPF_RET|BPF_K,SECCOMP_RET_ALLOW)};
    struct sock_fprog p={.len=4,.filter=f};
    if(argc<2 || prctl(PR_SET_NO_NEW_PRIVS,1,0,0,0) || prctl(PR_SET_SECCOMP,SECCOMP_MODE_FILTER,&p)) return 99;
    execv(argv[1],argv+1);return 98;
}
"""


def main():
    global BINARY
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument("--suite", choices=["runner", "network", "volumes", "all"], default="all")
    args = parser.parse_args()
    BINARY = str(args.binary.resolve())
    assert os.getuid() != 0, "Run as non-root; sudo provisions only task-owned mounts/cgroups"
    for tool in ["sudo", "bwrap", "umoci", "skopeo", "openssl", "musl-gcc"]:
        assert shutil.which(tool), "Missing disposable test prerequisite: " + tool
    workspace = Path(tempfile.mkdtemp(prefix="silo-worker-"))
    workspace.chmod(0o700)
    original_cgroup = Path("/sys/fs/cgroup" + Path("/proc/self/cgroup").read_text().strip().split("::")[1])
    group = Path("/sys/fs/cgroup") / ("silo-test-" + secrets.token_hex(6))
    mounted = False
    moved = False
    registry = None
    try:
        storage = workspace / "storage"; storage.mkdir(mode=0o700)
        run(["sudo", "mkdir", group])
        run(["sudo", "chown", "-R", "%d:%d" % (os.getuid(), os.getgid()), group])
        group.chmod(0o700)
        (group / "cgroup.subtree_control").write_text("+cpu +memory +pids")
        supervisor_group = group / "supervisor"; supervisor_group.mkdir()
        job_group = group / "jobs"; job_group.mkdir()
        # Move only THIS test process, never the deployment's supervisor or agent processes.
        run(["sudo", "sh", "-c", "echo %d > %s/cgroup.procs" % (os.getpid(), supervisor_group)])
        moved = True
        run(["sudo", "mount", "-t", "tmpfs", "-o",
             "size=1G,nr_inodes=65536,mode=0700,uid=%d,gid=%d" % (os.getuid(), os.getgid()),
             "silo-disposable-test", storage])
        mounted = True
        protected(workspace / "host-secret", "host control credential must be unreachable")
        certs = workspace / "certs"; certs.mkdir(mode=0o700)
        run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", certs / "key.pem",
             "-out", certs / "ca.crt", "-days", "1", "-subj", "/CN=localhost",
             "-addext", "subjectAltName=DNS:localhost"])
        # Privilege is used ONLY to drop the test supervisor's own supplementary groups.
        # It immediately becomes the unprivileged worker UID before opening files or exec.
        (workspace / "launch-supervisor.py").write_text("""
import os,sys
uid,gid=map(int,sys.argv[1:3]);auth,sentinel,pidfile=sys.argv[3:6]
os.setgroups([]);os.setresgid(gid,gid,gid);os.setresuid(uid,uid,uid)
os.environ.clear();os.environ['SILO_SENTINEL_SECRET']='never-in-workload'
a=os.open(auth,os.O_RDONLY);s=os.open(sentinel,os.O_RDONLY)
os.dup2(a,3,inheritable=True);os.dup2(s,427,inheritable=True)
os.set_inheritable(3,True);os.set_inheritable(427,True)
if a not in (3,427):os.close(a)
if s not in (3,427):os.close(s)
with open(pidfile,'w') as f:f.write(str(os.getpid()))
os.execv(sys.argv[6],sys.argv[6:])
""")
        source = Path(__file__).parent / "fixtures/probe.c"
        run(["musl-gcc", "-static", "-O2", "-Wall", "-Wextra", "-Werror", source, "-o", workspace / "probe"])
        (workspace / "deny-userns.c").write_text(DENY_SOURCE)
        run(["gcc", "-O2", "-Wall", "-Wextra", "-Werror",
             workspace / "deny-userns.c", "-o", workspace / "deny-userns"])
        def pin(tool):
            path = Path(shutil.which(tool)).resolve()
            result = {"path": str(path), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
            REPORT["pins"][tool] = result
            return result
        worker = {"bwrap": pin("bwrap"), "skopeo": pin("skopeo"), "umoci": pin("umoci"),
                  "agent_uid": 65534, "registry_ca_dir": str(certs),
                  "storage": str(storage), "cgroup": str(job_group),
                  "ceilings": {"processes": 256, "memory_bytes": 536870912, "cpu_millis": 2000,
                  "scratch_bytes": 16777216, "disk_bytes": 1073741824, "duration_secs": 60, "max_instances": 2}}
        REPORT["worker"] = worker
        REPORT["kernel"] = os.uname().release
        REPORT["pins"]["silo_sha256"] = hashlib.sha256(Path(BINARY).read_bytes()).hexdigest()
        elf_headers = run(["readelf", "-l", BINARY], text=True).stdout
        elf_dynamic = run(["readelf", "-d", BINARY], text=True).stdout
        check("static ELF has no interpreter or shared dependencies", "INTERP" not in elf_headers and "NEEDED" not in elf_dynamic)
        registry = Registry(certs)
        if args.suite in ("runner", "all"):
            suite(workspace, worker, registry, (workspace / "probe").read_bytes())
        if args.suite in ("network", "all"):
            sys.dont_write_bytecode = True
            from network_acceptance import suite as network_suite
            network_suite(sys.modules[__name__], workspace, worker, registry, (workspace / "probe").read_bytes())
        if args.suite in ("volumes", "all"):
            sys.dont_write_bytecode = True
            from volume_acceptance import suite as volume_suite
            volume_suite(sys.modules[__name__], workspace, worker, registry, (workspace / "probe").read_bytes())
        REPORT["result"] = "passed"
    except BaseException as error:
        REPORT["result"] = "failed"
        REPORT["error"] = repr(error)
        raise
    finally:
        args.report.resolve().write_text(json.dumps(REPORT, indent=2) + "\n")
        if registry:
            registry.shutdown(); registry.server_close()
        # Suite errors cannot leave untrusted descendants running.
        if group.exists():
            jobs = group / "jobs"
            if jobs.exists():
                for child in jobs.iterdir():
                    if child.is_dir():
                        (child / "cgroup.kill").write_text("1")
                        eventually(lambda: "populated 0" in (child / "cgroup.events").read_text(), "test cleanup")
                        child.rmdir()
        if moved:
            run(["sudo", "sh", "-c", "echo %d > %s/cgroup.procs" % (os.getpid(), original_cgroup)])
        if group.exists():
            for child in group.iterdir():
                if child.is_dir():
                    child.rmdir()
            run(["sudo", "rmdir", group])
        if mounted:
            run(["sudo", "umount", workspace / "storage"])
        args.report.resolve().write_text(json.dumps(REPORT, indent=2) + "\n")
        print("Report:", args.report, flush=True)
        shutil.rmtree(workspace)


if __name__ == "__main__":
    main()
