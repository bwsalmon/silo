"""Real Bubblewrap/OCI network acceptance; provisioning is shared with real_worker.py."""
import copy
import hashlib
import hmac
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import struct
import time

def raw(s, token, operation):
    endpoint = json.loads(s.endpoint.read_bytes())
    host, port = endpoint["address"].rsplit(":", 1)
    def read_exact(stream, n):
        data = b""
        while len(data) < n:
            chunk = stream.recv(n - len(data))
            assert chunk, "short private frame"
            data += chunk
        return data
    def read(stream):
        n, = struct.unpack(">I", read_exact(stream, 4))
        assert 0 < n <= 262144
        return json.loads(read_exact(stream, n))
    def write(stream, value):
        data = json.dumps(value, separators=(",", ":")).encode()
        stream.sendall(struct.pack(">I", len(data)) + data)
    with socket.create_connection((host, int(port)), timeout=310) as stream:
        nonce = secrets.token_hex(32)
        write(stream, {"nonce": nonce})
        proof = read(stream)
        expected = hmac.digest(endpoint["server_key"].encode(),
                               b"silo-control-server-v1\0" + nonce.encode(), "sha256")
        assert hmac.compare_digest(bytes(proof["proof"]), expected)
        write(stream, {"version": 1, "auth": token.read_text().strip(), "operation": operation})
        return read(stream)

def resources(s):
    pid = int(s.pid_file.read_text())
    status = Path("/proc/%d/status" % pid).read_text()
    values = dict(line.split(":", 1) for line in status.splitlines() if ":" in line)
    ticks = Path("/proc/%d/stat" % pid).read_text().split()
    return {"fds": len(list(Path("/proc/%d/fd" % pid).iterdir())),
            "threads": int(values["Threads"]), "rss_kib": int(values["VmRSS"].split()[0]),
            "cpu_ticks": int(ticks[13]) + int(ticks[14])}

def suite(r, workspace, worker, registry, unused_probe):
    check, eventually = r.check, r.eventually
    r.run(["musl-gcc", "-static", "-pthread", "-O2", "-Wall", "-Wextra", "-Werror",
           Path(__file__).parent / "fixtures/tcp_probe.c", "-o", workspace / "tcp-probe"])
    busybox = Path(shutil.which("busybox") or "/missing-busybox-static")
    assert busybox.is_file(), "Missing disposable prerequisite: busybox-static"
    elf = r.run(["readelf", "-l", busybox], text=True).stdout
    assert "INTERP" not in elf, "Acceptance requires the unmodified static BusyBox package"
    r.REPORT["pins"]["stock_busybox"] = {"sha256": hashlib.sha256(busybox.read_bytes()).hexdigest(),
        "version": r.run([busybox], text=True).stdout.splitlines()[0]}
    image = registry.image({"Env": ["PATH=/bin"], "User": "0:0"}, [r.tar_layer([
        r.file("bin/busybox", busybox.read_bytes(), mode=0o755),
        r.file("tcp-probe", (workspace / "tcp-probe").read_bytes(), mode=0o755),
        r.file("etc/nsswitch.conf", b"hosts: files\n"),
        r.file("www-db/index.html", b"approved-db\n"),
        r.file("www-other/index.html", b"approved-other\n"),
        r.file("www-secret/index.html", b"forbidden-service\n"),
    ])])
    r.REPORT["pins"]["network_fixture"] = image
    limits = {"processes": 64, "memory_bytes": 134217728, "cpu_millis": 1000,
              "scratch_bytes": 8388608, "connections": 4, "duration_secs": 60, "max_instances": 2}
    def job(command, **overrides):
        value = {"image": image, "command": command, "limits": copy.deepcopy(limits),
                 "agent": ["run", "list", "status", "logs", "stop"]}
        value.update(overrides)
        return value
    def sh(script):
        return ["/bin/busybox", "sh", "-ec", script]
    stock_script = """
a=$(/bin/busybox timeout 5 /bin/busybox wget -qO- http://db.silo:8080)
b=$(/bin/busybox timeout 5 /bin/busybox wget -qO- http://other.silo:8080)
c=$(/bin/busybox timeout 5 /bin/busybox wget -qO- http://127.77.0.1:8080)
test "$a" = approved-db
test "$b" = approved-other
test "$c" = approved-db
/bin/busybox cat /etc/hosts
if echo poison >> /etc/hosts; then exit 93; fi
echo stock-named-numeric=ok
"""
    jobs = {
        "stock": job(sh(stock_script)),
        "unrelated": job(sh("""
if /bin/busybox timeout 5 /bin/busybox wget -qO- http://db.silo:8080; then exit 93; fi
if /bin/busybox timeout 5 /bin/busybox wget -qO- http://127.77.0.1:8080; then exit 94; fi
echo unrelated-named-numeric=denied
""")),
        "direct": job(["/tcp-probe", "direct-bench"]),
        "reverse-live": job(["/tcp-probe", "reverse-live", "db.silo"]),
        "lease": job(["/tcp-probe", "hold", "db.silo"]),
        "half": job(["/tcp-probe", "half", "db.silo"]),
        "reset": job(["/tcp-probe", "reset", "db.silo"]),
        "repeat": job(["/tcp-probe", "repeat", "db.silo"]),
        "parallel": job(["/tcp-probe", "parallel", "db.silo"]),
        "quota": job(["/tcp-probe", "quota", "db.silo"]),
        "backpressure": job(["/tcp-probe", "backpressure", "db.silo"]),
        "hold": job(["/tcp-probe", "hold", "db.silo"]),
        "conflict": job(["/tcp-probe", "conflict"]),
        "other-port": job(["/tcp-probe", "denied", "127.77.0.1", "8081"]),
        "other-service": job(["/tcp-probe", "denied", "127.77.0.3", "8080"]),
        "local-address": job(["/tcp-probe", "denied", "127.0.0.1", "8080"]),
        "outbound": job(["/tcp-probe", "denied", "1.1.1.1", "80"]),
        "no-listener": job(["/tcp-probe", "half", "db.silo"]),
    }
    # A fixed approved endpoint with no workload listener exercises bounded setup failure.
    jobs["lease"]["limits"]["duration_secs"] = 2
    jobs["no-listener"]["command"] = sh("""
if /bin/busybox timeout 5 /bin/busybox wget -qO- http://db.silo:9001; then exit 93; fi
echo setup-failure=closed
""")
    def service(command, ports):
        t = job(command, agent=[])
        t["limits"].update(connections=32, duration_secs=240, max_instances=1)
        return {"template": t, "listen": ports}
    services = {
        "db": service(sh("""
/bin/busybox httpd -f -p 127.0.0.1:8080 -h /www-db &
/bin/busybox httpd -f -p 127.0.0.1:8081 -h /www-secret &
exec /tcp-probe server
"""), [8080, 8081, 9000, 9001]),
        "other": service(["/bin/busybox", "httpd", "-f", "-p", "127.0.0.1:8080", "-h", "/www-other"], [8080]),
        "secret": service(["/bin/busybox", "httpd", "-f", "-p", "127.0.0.1:8080", "-h", "/www-secret"], [8080]),
    }
    links = []
    linked_jobs = set(jobs) - {"unrelated", "direct", "outbound"}
    for source in sorted(linked_jobs):
        for dest, port in [("db", 8080), ("db", 9000), ("other", 8080)]:
            links.append({"from": source, "to": dest, "protocol": "tcp", "port": port})
    links.append({"from": "no-listener", "to": "db", "protocol": "tcp", "port": 9001})
    doc = {"version": 1, "project": "worker-test", "jobs": jobs, "services": services,
           "network": {"external": "deny", "allow": links}}
    config = copy.deepcopy(worker)
    config["ceilings"].update(max_instances=6, scratch_bytes=50331648, memory_bytes=1073741824,
                             processes=512, duration_secs=300)
    r.REPORT["network_worker"] = config
    supervisors = []
    def server(manifest=doc):
        s = r.Supervisor(r.BINARY, workspace, config, manifest)
        supervisors.append(s)
        return s
    def issue(s, session, templates, links=None, expires=None):
        grant = {"session": session, "project": "worker-test", "policy": s.policy,
                 "expires_at": expires or int(time.time()) + 240,
                 "scope": {"templates": templates, "permissions": ["run", "list", "status", "logs", "stop"],
                           "links": links or [], "max_instances": 2}}
        path = s.dir / (session + "-grant.json"); path.write_bytes(r.encoded(grant))
        token = s.dir / (session + "-token")
        with token.open("wb") as out:
            result = s.call(["operator", "issue", path, "--token-fd", out.fileno()], extra_fds=(out.fileno(),))
        token.chmod(0o600)
        return token, result["grant"]
    scope_links = ["db:8080", "db:9000", "db:9001", "other:8080"]
    try:
        s = server()
        token, grant = issue(s, "network-a", list(jobs), scope_links)
        other, _ = issue(s, "network-b", ["unrelated", "stock"])
        # Raw launch cannot add links even when the caller may run the template.
        result = raw(s, other, {"op": "run", "template": "stock"})
        check("missing immutable link grant denies template launch over raw protocol",
              result["error"]["code"] == "forbidden")
        for extra in [{"destination": "1.1.1.1:80"}, {"links": ["secret:8080"]},
                      {"env": {"HTTP_PROXY": "http://host"}}, {"command": ["/bin/busybox", "sh"]}]:
            result = raw(s, token, {"op": "run", "template": "stock", **extra})
            assert result["error"]["code"] == "invalid_request", result
        check("raw protocol rejects destination/link/command/environment overrides", True)
        s.call(["run", "stock", "--destination", "secret:8080"], token, expected=2)
        check("CLI rejects endpoint overrides", True)

        activation = s.call(["up", s.manifest])
        check("operator up starts actual services", activation["started"])
        instances = {i["template"]: i for i in s.call(["ps"])["instances"]}
        service_ids = {name: instances[name]["id"] for name in services}
        assert len(service_ids) == 3
        s.call(["up", s.manifest])
        check("repeated identical up is idempotent", len(s.call(["ps"])["instances"]) == 3)
        for name, instance in service_ids.items():
            assert s.call(["service", "inspect", name])["instance"] == instance
            result = raw(s, token, {"op": "stop", "id": instance})
            assert result["error"]["code"] == "not_found"
        check("agents cannot stop or see operator service instances", s.call(["ps"], token)["instances"] == [])

        baseline = resources(s)
        r.REPORT["network_supervisor_baseline"] = baseline
        r.REPORT["clock_ticks_per_second"] = os.sysconf("SC_CLK_TCK")
        def execute(name, credential=token, expected=0):
            before = resources(s)
            instance = s.call(["run", name], credential)
            peaks = []
            if name == "parallel":
                def sample():
                    peaks.append(resources(s))
                    result = s.call(["status", instance["id"]], credential)
                    return result if result["state"] in ("exited", "failed") else None
                terminal = eventually(sample, "parallel stream completion", timeout=45)
            else:
                terminal = s.terminal(instance, credential, timeout=45)
            after = resources(s)
            r.REPORT.setdefault("network_scenarios", {})[name] = {
                "supervisor_cpu_ticks_delta": after["cpu_ticks"] - before["cpu_ticks"],
                "supervisor_peak_observed": {k: max((p[k] for p in peaks), default=after[k])
                    for k in ["fds", "threads", "rss_kib"]}}
            if name == "parallel":
                assert max(p["fds"] for p in peaks) <= baseline["fds"] + 12
                assert max(p["threads"] for p in peaks) <= baseline["threads"] + 1
            logs = s.call(["logs", instance["id"]], credential)
            assert terminal.get("exit_code") == expected, (name, terminal, logs)
            assert logs["relay"]["active"] == 0
            assert not (Path(config["cgroup"]) / instance["id"]).exists()
            return logs
        logs = execute("stock")
        check("unmodified stock wget reaches only approved named and numeric endpoints",
              "stock-named-numeric=ok" in logs["stdout"])
        check("distinct service destinations share a port with readonly generated hosts",
              "127.77.0.1 db.silo" in logs["stdout"] and "127.77.0.2 other.silo" in logs["stdout"])
        check("other-agent job denied named and numeric destination", "unrelated-named-numeric=denied" in execute("unrelated", other)["stdout"])
        for name in ["other-port", "other-service", "local-address", "outbound"]:
            check(name + " is denied inside actual isolated image", "denied=ok" in execute(name)["stdout"])
        check("workload wildcard bind cannot steal helper port", "wildcard-conflict=ok" in execute("conflict")["stdout"])
        check("missing destination workload listener closes setup", "setup-failure=closed" in execute("no-listener")["stdout"])
        for name, marker in [("reverse-live", "reverse-live=denied"), ("half", "half-close=ok"), ("reset", "reset-recovery=ok"),
                             ("repeat", "repeat-duplex=ok"), ("parallel", "parallel=ok")]:
            logs = execute(name)
            check(name + " with real Bubblewrap kernel TCP descriptors", marker in logs["stdout"])
            if name == "repeat": r.REPORT["network_relay_measurements"] = logs["stdout"]
            if name == "parallel":
                check("parallel stream accounting stays bounded", 2 <= logs["relay"]["peak"] <= 4)
        r.REPORT["network_direct_measurements"] = execute("direct")["stdout"]
        logs = execute("quota")
        check("excess parallel connection is rejected within per-instance limit",
              "quota=ok" in logs["stdout"] and logs["relay"]["rejected"] >= 1 and logs["relay"]["peak"] == 4)
        logs = execute("backpressure")
        check("stalled reader triggers bounded buffers and idle deadline", "backpressure_bytes=" in logs["stdout"])
        r.REPORT["network_backpressure"] = logs["stdout"]

        # Inspect a live job's /proc fd table from the protected worker OS identity.
        def open_stream(credential, template="hold"):
            instance = s.call(["run", template], credential)
            group = Path(config["cgroup"]) / instance["id"]
            helper = eventually(lambda: next((pid for pid in r.host_processes(group)
                if b"__namespace_helper" in Path("/proc/%d/cmdline" % pid).read_bytes()), None), "network helper")
            def established():
                table = r.run(["sudo", "cat", "/proc/%d/net/tcp" % helper], text=True).stdout
                inodes = {line.split()[9] for line in table.splitlines()[1:]
                          if line.split()[1].endswith(":2328") and line.split()[3] == "01"}
                supervisor_pid = int(s.pid_file.read_text())
                fds = r.run(["sudo", "ls", "-l", "/proc/%d/fd" % supervisor_pid], text=True).stdout
                return sorted(i for i in inodes if "socket:[%s]" % i in fds)
            inodes = eventually(established, "actual established TCP descriptor transferred to supervisor")
            r.REPORT.setdefault("established_source_socket_inodes", []).append(inodes)
            assert s.call(["status", instance["id"]], credential)["state"] == "running"
            return instance, r.host_processes(group)
        stream, pids = open_stream(token)
        live = resources(s)
        r.REPORT["network_supervisor_live"] = live
        check("live relay descriptors remain outside workload and in fixed supervisor budget",
              live["fds"] <= baseline["fds"] + 8 and live["threads"] <= baseline["threads"] + 1)
        result = raw(s, other, {"op": "status", "id": stream["id"]})
        assert result["error"]["code"] == "not_found"
        start = time.monotonic()
        s.call(["operator", "revoke", grant])
        s.terminal(stream)
        check("security revocation closes open stream and removes grant-owned processes",
              time.monotonic() - start < 3 and all(not Path("/proc/%d" % pid).exists() for pid in pids))
        for id in service_ids.values():
            assert s.call(["status", id])["state"] == "running"
        check("operator services survive caller security revocation", True)

        expiring, _ = issue(s, "expiring", ["hold"], scope_links, int(time.time()) + 4)
        stream, pids = open_stream(expiring)
        s.terminal(stream, timeout=8)  # operator remains able to observe an expired grant's job.
        check("credential expiry closes established relay and stops job",
              all(not Path("/proc/%d" % pid).exists() for pid in pids))
        fresh, _ = issue(s, "fresh", ["stock", "hold", "lease"], scope_links)
        start = time.monotonic()
        leased, pids = open_stream(fresh, "lease")
        s.terminal(leased, timeout=6)
        check("template lease closes established link before credential expiry", time.monotonic() - start < 4
              and all(not Path("/proc/%d" % pid).exists() for pid in pids))
        check("service still accepts fresh approved grant after revocation/expiry",
              "stock-named-numeric=ok" in execute("stock", fresh)["stdout"])
        after = resources(s)
        r.REPORT["network_supervisor_after"] = after
        check("repeated parallel failure and revoke paths return supervisor FD/thread counts",
              after["fds"] == baseline["fds"] and after["threads"] == baseline["threads"])
        check("only operator service cgroups remain after caller cleanup",
              sorted(p.name for p in Path(config["cgroup"]).iterdir() if p.is_dir()) == sorted(service_ids.values()))
        s.close()
        # A direct reverse dial from an operator service has no helper listener either.
        reverse_doc = copy.deepcopy(doc)
        reverse_doc["services"] = {"db": service(["/tcp-probe", "reverse"], [8080])}
        reverse_doc["jobs"] = {}
        reverse_doc["network"]["allow"] = []
        rev = server(reverse_doc); rev.call(["up", rev.manifest])
        reverse_instance = rev.call(["ps"])["instances"][0]
        terminal = rev.terminal(reverse_instance)
        logs = rev.call(["logs", reverse_instance["id"]])
        check("directional service link grants no new reverse connection",
              terminal["exit_code"] == 0 and "reverse=denied" in logs["stdout"])

        rev.close()
        denial_source = workspace / "deny-sendmsg.c"
        denial_source.write_text(r.DENY_SOURCE.replace("SYS_unshare", "SYS_sendmsg"))
        denial = workspace / "deny-sendmsg"
        r.run(["gcc", "-O2", "-Wall", "-Wextra", "-Werror", denial_source, "-o", denial])
        denied_worker = r.Supervisor(r.BINARY, workspace, config, reverse_doc, denial)
        supervisors.append(denied_worker)
        result = denied_worker.call(["up", denied_worker.manifest], expected=5)
        check("missing real SCM_RIGHTS capability fails before workload execution",
              result["error"]["code"] == "unsupported_worker" and "SCM_RIGHTS" in result["error"]["message"])
        denied_worker.close()

        conflict_doc = copy.deepcopy(reverse_doc)
        conflict_doc["services"]["db"]["template"]["command"] = ["/tcp-probe", "server"]
        conflict_doc["services"]["other"] = service(["/tcp-probe", "server"], [8080])
        conflict_doc["network"]["allow"] = [{"from": "db", "to": "other", "port": 8080, "protocol": "tcp"}]
        conflict = server(conflict_doc)
        result = conflict.call(["up", conflict.manifest], expected=4)
        check("declared workload/wildcard relay conflict fails activation before exec",
              result["error"]["code"] == "port_conflict" and conflict.call(["ps"])["instances"] == [])
    finally:
        for s in reversed(supervisors): s.close()
    check("all network workload/helper cgroups removed", not any(p.is_dir() for p in Path(config["cgroup"]).iterdir()))
