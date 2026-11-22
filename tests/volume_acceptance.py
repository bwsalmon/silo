"""Real persistent filesystem, stock Postgres/psql and two-OS-identity acceptance."""
import copy
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import subprocess
import tempfile
import threading
import tarfile
import time

POSTGRES = "docker.io/library/postgres@sha256:f2f98726820d0342055351c6e9d36e705f5bfffb3dd53ff556325114b08afb6d"
DATA = "/var/lib/postgresql/data"


def suite(r, workspace, worker, registry, probe):
    from network_acceptance import raw
    check, eventually = r.check, r.eventually
    root = Path(tempfile.mkdtemp(prefix="silo-persistent-", dir="/var/tmp"))
    slots = root / "slots"; slots.mkdir(mode=0o700)
    mounted = []
    servers = []
    agent_root = Path("/tmp") / ("silo-agents-" + secrets.token_hex(6))
    config = copy.deepcopy(worker)
    config["volume_root"] = str(root)
    config["ceilings"].update(disk_bytes=2147483648, memory_bytes=2147483648,
                              max_instances=8, duration_secs=600, processes=512)
    r.REPORT["pins"]["stock_postgres"] = POSTGRES
    r.REPORT["storage_worker"] = config
    r.REPORT["agent_os_profile"] = {"uids":[65534,65533], "private_pid_and_mount_namespaces":True, "private_procfs":True, "no_new_privs":True, "supplementary_groups":[] }
    limits = {"processes": 64, "memory_bytes": 805306368, "cpu_millis": 1000,
              "scratch_bytes": 16777216, "connections": 8, "duration_secs": 300, "max_instances": 4}
    def job(command, mounts=(), agent=True, image=POSTGRES, uid=70, **overrides):
        value = {"image": image, "uid": uid, "gid": uid, "command": command,
                 "limits": copy.deepcopy(limits), "mounts": list(mounts),
                 "agent": ["run", "list", "status", "stop"] if agent else []}
        value.update(overrides)
        return value
    def mount(volume, mode="rw", target=None):
        return {"volume": volume, "target": target or "/" + volume, "mode": mode}
    def sh(script):
        return ["/bin/sh", "-ec", script]
    pgmount = mount("pgdata", target=DATA)
    # Fixed operator code and disposable application passwords; never control credentials.
    init = """
test "$(id -u)" = 70
test "$(id -g)" = 70
test "$(stat -c %u "$PGDATA")" = 70
echo image-and-data-owner=70:70
printf '%s\n' disposable-operator > /tmp/password
initdb -D "$PGDATA" -U operator --pwfile=/tmp/password --auth-host=scram-sha-256 --auth-local=reject
postgres -D "$PGDATA" -c unix_socket_directories= -c listen_addresses=127.0.0.1 -c shared_buffers=16MB &
pid=$!
trap 'kill -TERM "$pid" 2>/dev/null || true; wait "$pid" || true' EXIT
export PGHOST=127.0.0.1 PGUSER=operator PGPASSWORD=disposable-operator
for i in 1 2 3 4 5 6 7 8 9 10; do
  if psql -d postgres -v ON_ERROR_STOP=1 -c "SELECT 1"; then break; fi
  sleep 0.2
done
psql -d postgres -v ON_ERROR_STOP=1 -c "
CREATE ROLE app LOGIN PASSWORD 'disposable-app';
CREATE TABLE allowed(value text);
CREATE TABLE secret(value text);
INSERT INTO secret VALUES ('operator-only');
GRANT SELECT,INSERT ON allowed TO app;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO app;
"
kill -TERM "$pid"
wait "$pid"
trap - EXIT
"""
    # All scripts are operator-pinned command strings; no script/config/init-hook is mounted.
    readiness = """
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
 if psql -X -At -v ON_ERROR_STOP=1 -c 'SELECT 1' >/dev/null 2>&1; then break; fi
 sleep 0.1
done
"""
    app = readiness + """
test "$(cat /fixtures/fixture)" = fixture-value
if echo poison >> /fixtures/fixture; then exit 91; fi
test ! -e /other/private
psql -X -v ON_ERROR_STOP=1 -c "INSERT INTO allowed VALUES ('durable-row'); SELECT value FROM allowed"
printf '%s\n' results-value > /results/result
"""
    verify = sh(readiness + "exec /usr/local/bin/psql -X -At -v ON_ERROR_STOP=1 -c 'SELECT value FROM allowed'")
    env = {"PGHOST": "db.silo", "PGPORT": "5432", "PGDATABASE": "postgres",
           "PGUSER": "app", "PGPASSWORD": "disposable-app", "PGCONNECT_TIMEOUT": "3"}
    jobs = {
        "initialize": job(sh(init), [pgmount], agent=False, env={"PGDATA": DATA}),
        "seed": job(sh("echo fixture-value > /fixtures/fixture; echo private > /other/private"),
                    [mount("fixtures"), mount("other")], agent=False),
        "app": job(sh(app), [mount("fixtures", "ro"), mount("results")], env=env),
        "verify": job(verify, env=env),
        "unrelated": job(sh("""
if psql -X -At -c 'SELECT 1'; then exit 91; fi
if PGHOST=127.77.0.1 psql -X -At -c 'SELECT 1'; then exit 92; fi
echo unrelated-denied
"""), env=env),
        "sql-deny": job(sh("""
for q in "SELECT * FROM secret" "SELECT pg_read_file('/etc/passwd')" "COPY allowed TO PROGRAM 'id'" "SET ROLE operator"; do
 if psql -X -v ON_ERROR_STOP=1 -c "$q"; then exit 93; fi
done
echo sql-denied
"""), env=env),
        "writer": job(sh("sleep 120 & sleep 120 & wait"), [mount("results")]),
        "reader": job(sh("test -f /fixtures/fixture; sleep 120"), [mount("fixtures", "ro")]),
        "fixture-writer": job(sh("sleep 120"), [mount("fixtures")], agent=False),
        "pg-writer": job(sh("sleep 120"), [pgmount], agent=False),
        "disk": job(sh("if dd if=/dev/zero of=/results/full bs=1048576 count=100; then exit 94; fi; rm /results/full; echo disk-ceiling-enforced"), [mount("results")]),
        "symlink": job(sh("""
ln -s /other/private /results/link
if cat /results/link; then exit 95; fi
ln -s /proc/1/root/other/private /results/proc-link
if cat /results/proc-link; then exit 96; fi
echo symlink-escape-denied
"""), [mount("results")]),
    }
    jobs["verify"]["agent"].append("logs")
    jobs["result-reader"] = job(sh("sleep 120"), [mount("results", "ro")])
    jobs["race"] = job(sh("test ! -f /results/private; echo race-safe"), [mount("results")])
    jobs["wrong-owner"] = job(sh("sleep 120"), [mount("results")], uid=71, agent=False)
    alias_image = registry.image({"User":"70:70"}, [r.tar_layer([
        r.file("probe", probe, mode=0o755), r.file("results/script", b"never-executed", mode=0o755),
        r.file("bin/busybox", Path(shutil.which("busybox")).read_bytes(), mode=0o755),
        ("sub", b"", tarfile.DIRTYPE, "", 0, 0, 0o755),
        ("alias", b"", tarfile.SYMTYPE, "/results/script", 0, 0, 0o777),
        ("pivot", b"", tarfile.SYMTYPE, "/results", 0, 0, 0o777),
    ])])
    jobs["poison-input"] = job(sh("printf '%s\\n' 'echo escalation > /results/escalated' > /results/script"), [mount("results")])
    jobs["unsafe-relative"] = job(["/bin/busybox", "sh", "alias"], [mount("results")], image=alias_image)
    jobs["unsafe-traversal"] = job(["/bin/busybox", "sh", "sub/../alias"], [mount("results")], image=alias_image)
    jobs["unsafe-alias"] = job(["/bin/busybox", "sh", "/alias"], [mount("results")], image=alias_image)
    jobs["unsafe-target"] = job(["/probe", "exit"], [mount("results", target="/pivot/data")], image=alias_image)
    jobs["unsafe-input"] = job(["/probe", "exit"], [mount("results")], image=alias_image, executable_inputs=["/alias"])
    jobs["unsafe-protected"] = job(["/probe", "exit"], [mount("results"), pgmount], agent=False, image=alias_image, executable_inputs=["/alias"])
    jobs["unsafe-env"] = job(["/probe", "exit"], [mount("results")], image=alias_image, env={"BASH_ENV":"/alias"})
    server = job(["/usr/local/bin/postgres", "-D", DATA, "-c", "unix_socket_directories=",
                  "-c", "listen_addresses=127.0.0.1", "-c", "shared_buffers=16MB"],
                 [pgmount], agent=False, env={"PGDATA": DATA})
    server["limits"]["duration_secs"] = 600
    doc = {"version": 1, "project": "worker-test", "jobs": jobs,
           "services": {"db": {"template": server, "listen": [5432]}},
           "network": {"external": "deny", "allow": [
               {"from": name, "to": "db", "protocol": "tcp", "port": 5432}
               for name in ["app", "verify", "sql-deny", "unsafe-alias", "unsafe-input", "unsafe-env", "unsafe-relative", "unsafe-traversal"]]},
           "volumes": {name: {"sharing": "exclusive-writer", "uid": 70, "gid": 70,
                              "max_bytes": size * 1048576,
                              "agent": ["volume_list", "volume_inspect"] if name == "fixtures" else []}
                       for name, size in [("fixtures", 64), ("results", 64), ("pgdata", 256), ("other", 64)]}}
    doc["volumes"]["too-small"] = {"sharing":"exclusive-writer", "uid":70, "gid":70, "max_bytes":1024}
    doc["volumes"]["too-large"] = {"sharing":"exclusive-writer", "uid":70, "gid":70, "max_bytes":3221225472}
    example = copy.deepcopy(doc)
    example["jobs"] = {k:v for k,v in example["jobs"].items() if k in ("initialize", "seed", "app", "verify", "unrelated", "sql-deny")}
    example["network"]["allow"] = [l for l in example["network"]["allow"] if l["from"] in example["jobs"]]
    example["volumes"] = {k:v for k,v in example["volumes"].items() if not k.startswith("too-")}
    r.REPORT["postgres_example"] = example
    def new_server(document=doc, conf=config):
        s = r.Supervisor(r.BINARY, workspace, conf, document); servers.append(s); return s
    transcript = []
    def issue(s, session, templates, logs=False):
        grant = {"session": session, "project": doc["project"], "policy": s.policy,
                 "expires_at": int(time.time()) + 900,
                 "scope": {"templates": templates,
                           "permissions": ["run", "list", "status", "stop", "volume_list", "volume_inspect"] + (["logs"] if logs else []),
                           "volumes": ["fixtures"], "mounts": {"fixtures": "ro", "results": "rw"},
                           "links": ["db:5432"], "max_instances": 6}}
        path = s.dir / (session + ".json"); path.write_bytes(r.encoded(grant))
        token = s.dir / (session + ".token")
        with token.open("wb") as output:
            s.call(["operator", "issue", path, "--token-fd", output.fileno()], extra_fds=(output.fileno(),))
        token.chmod(0o600)
        return token
    def execute(s, name, token=None, expected=0):
        i = s.call(["run", name], token)
        state = s.terminal(i, token, timeout=90)
        logs = s.call(["logs", i["id"]])
        assert state["exit_code"] == expected and state["state"] == "exited", (name, state, logs)
        return i, logs
    def agent_prefix(uid):
        # Separate UIDs protect data/control, but Linux's ordinary procfs exposes cmdline.
        # The trusted native-agent launcher must isolate the process view too.
        return ["sudo", "unshare", "--pid", "--fork", "--mount-proc", "setpriv", "--no-new-privs",
                "--reuid", str(uid), "--regid", str(uid), "--clear-groups"]
    def agent_cli(s, identity, args, expected=0):
        uid, directory = identity
        token = directory / "token"
        # Files belong to the actual restricted principal; no worker UID shell is used.
        code = """import os,sys
f=os.open(sys.argv[1],os.O_RDONLY);os.set_inheritable(f,True)
os.execv(sys.argv[2],[sys.argv[2]]+sys.argv[3:]+['--auth-fd',str(f),'--json'])
"""
        result = subprocess.run(agent_prefix(uid) + ["python3", "-c", code,
                                 str(token), str(agent_root / "silo")] + [str(a) for a in args] +
                                ["--endpoint", str(directory / "endpoint.json")],
                                capture_output=True, timeout=310)
        assert result.stdout, (args, result.returncode, result.stderr)
        value = json.loads(result.stdout)
        assert result.returncode == expected, (args, result.returncode, value, result.stderr)
        transcript.append({"identity": uid, "argv": [str(a) for a in args], "exit": expected, "response": value})
        return value
    try:
        for index, size in enumerate([64, 64, 256, 64]):
            slot = slots / ("slot_" + str(index)); slot.mkdir(mode=0o700)
            image = root / ("volume-%d.ext4" % index)
            r.run(["truncate", "-s", "%dM" % size, image])
            r.run(["/usr/sbin/mkfs.ext4", "-q", "-F", "-N", "4096", "-m", "0", image])
            r.run(["sudo", "mount", "-o", "loop,nosuid,nodev,noexec", image, slot])
            mounted.append(slot)
            r.run(["sudo", "rmdir", slot / "lost+found"])
            r.run(["sudo", "chown", "%d:%d" % (os.getuid(), os.getgid()), slot])
            slot.chmod(0o700)
        s = new_server()
        validation = json.loads(r.run([r.BINARY, "validate", s.manifest, "--json"]).stdout)
        assert validation["valid"]
        failure = s.call(["up", s.manifest], expected=5)
        assert "Service db setup failed" in failure["error"]["message"]
        check("validate and no-start are non-mutating; up reports missing explicitly-created service volume", all(not (p / "data").exists() for p in mounted))
        assert s.call(["volume", "create", "too-small"], expected=5)["error"]["code"] == "unsupported_worker"
        assert s.call(["volume", "create", "too-large"], expected=4)["error"]["code"] == "quota_exceeded"
        check("missing bounded storage capability and aggregate disk overcommit fail closed", True)
        ids = {}
        for name in ["fixtures", "results", "pgdata", "other"]:
            meta = s.call(["volume", "create", name]); ids[name] = meta["id"]
            assert s.call(["volume", "create", name])["id"] == meta["id"]
        check("explicit idempotent creation allocates protected managed IDs", len(set(ids.values())) == 4)
        check("catalog metadata exposes no host paths", str(root) not in json.dumps(s.call(["volume", "list"])))
        execute(s, "seed")
        _, logs = execute(s, "initialize")
        check("stock Postgres initdb runs as actual numeric image owner with TCP-only bootstrap", "database system is shut down" in logs["stderr"] and "image-and-data-owner=70:70" in logs["stdout"])
        s.call(["up", s.manifest])
        services = s.call(["ps"])["instances"]
        db = next(i for i in services if i["template"] == "db" and i["state"] == "running")
        s.call(["up", s.manifest])
        check("identical up is idempotent and starts only services", len([i for i in s.call(["ps"])["instances"] if i["template"] == "db" and i["state"] == "running"]) == 1)
        token_a = issue(s, "agent-a", ["app", "verify", "sql-deny", "writer", "reader", "disk", "symlink", "unrelated", "race", "result-reader", "unsafe-alias", "unsafe-target", "unsafe-input", "unsafe-env", "unsafe-relative", "unsafe-traversal", "poison-input"])
        token_b = issue(s, "agent-b", ["unrelated"])
        # Create a root-owned bridge directory, isolated OS principals and their own endpoint copies.
        r.run(["sudo", "mkdir", "-m", "755", agent_root])
        r.run(["sudo", "cp", r.BINARY, agent_root / "silo"])
        r.run(["sudo", "chmod", "755", agent_root / "silo"])
        identities = []
        for uid, token in [(65534, token_a), (65533, token_b)]:
            directory = agent_root / str(uid)
            r.run(["sudo", "mkdir", "-m", "700", directory])
            for source, dest in [(token, "token"), (s.endpoint, "endpoint.json")]:
                r.run(["sudo", "cp", source, directory / dest])
                r.run(["sudo", "chmod", "600", directory / dest])
            r.run(["sudo", "chown", "-R", "%d:%d" % (uid, uid), directory])
            identities.append((uid, directory))
        a, b = identities
        profiles = []
        for uid, _ in identities:
            code = """import json,os
from pathlib import Path
status=dict(line.split(':',1) for line in Path('/proc/self/status').read_text().splitlines() if ':' in line)
print(json.dumps({'uid':os.getuid(),'gid':os.getgid(),'groups':os.getgroups(),
 'no_new_privs':int(status['NoNewPrivs'].strip()),
 'capabilities':{k:status[k].strip() for k in ['CapEff','CapPrm','CapInh','CapAmb']},
 'pid_namespace_inode':os.stat('/proc/self/ns/pid').st_ino,
 'mount_namespace_inode':os.stat('/proc/self/ns/mnt').st_ino,
 'visible_pids':[p.name for p in Path('/proc').iterdir() if p.name.isdigit()]}))
"""
            profile = json.loads(r.run(agent_prefix(uid) + ["python3", "-c", code]).stdout)
            assert profile['uid'] == uid and profile['gid'] == uid and profile['groups'] == [] and profile['no_new_privs'] == 1
            assert all(int(v,16) == 0 for v in profile['capabilities'].values())
            assert profile['pid_namespace_inode'] != os.stat('/proc/self/ns/pid').st_ino
            assert profile['mount_namespace_inode'] != os.stat('/proc/self/ns/mnt').st_ino
            assert s.pid_file.read_text().strip() not in profile['visible_pids']
            profiles.append(profile)
        r.REPORT['agent_kernel_profiles'] = profiles
        check('kernel confirms both agent UID/GID, empty groups, zero capabilities, no_new_privs and private process views', True)
        instance = agent_cli(s, a, ["run", "app"])
        state = eventually(lambda: (v if (v := agent_cli(s, a, ["status", instance["id"]]))["state"] in ("exited", "failed") else None), "restricted app exit", 30)
        logs = s.call(["logs", instance["id"]])
        r.REPORT["postgres_platform_digest"] = instance["image_digest"]
        check("restricted stock psql writes authorized SQL and results, reads fixtures and cannot write RO fixtures", state["exit_code"] == 0 and "durable-row" in logs["stdout"] and "Read-only file system" in logs["stderr"])
        agent_cli(s, b, ["status", instance["id"]], 3)
        agent_cli(s, a, ["run", "db"], 3)
        agent_cli(s, a, ["volume", "inspect", "pgdata"], 3)
        agent_cli(s, a, ["run", "app", "--mount", "pgdata:/data:rw"], 2)
        agent_cli(s, a, ["stop", instance["id"]])
        agent_cli(s, a, ["logs", instance["id"]], 3)
        check("documented two-identity JSON CLI transcript uses actual restricted OS contexts", True)
        visible = agent_cli(s, a, ["ps"])["instances"]
        check("ps filters instances by grant and hides service ownership", all(i["id"] == instance["id"] for i in visible))
        for args in [["status", db["id"]], ["logs", db["id"]], ["stop", db["id"]],
                     ["volume", "create", "pgdata"], ["volume", "rm", "pgdata"], ["volume", "recover", "pgdata"]]:
            agent_cli(s, a, args, 3)
        pid = int(s.pid_file.read_text())
        for uid in [65534, 65533]:
            for target in [root, root / "catalog", mounted[2] / "data", worker["storage"],
                           "/proc/%d/environ" % pid, "/proc/%d/cmdline" % pid, "/proc/%d/fd" % pid]:
                result = subprocess.run(agent_prefix(uid) + ["ls" if Path(target).is_dir() else "cat", str(target)], capture_output=True)
                assert result.returncode != 0, (uid, target, result.stdout)
            assert subprocess.run(agent_prefix(uid) + ["kill", "-0", str(pid)], capture_output=True).returncode != 0
            assert subprocess.run(agent_prefix(uid) + ["nsenter", "-t", str(pid), "-m", "-p", "true"], capture_output=True).returncode != 0
        helper_pid = r.host_processes(Path(config["cgroup"]) / db["id"])[0]
        tcp_table = Path("/proc/%d/net/tcp" % helper_pid).read_text()
        assert any(line.split()[1].endswith(":1538") and line.split()[3] == "0A" for line in tcp_table.splitlines()[1:])
        r.REPORT["postgres_listening_tcp"] = tcp_table
        check("actual restricted OS identity cannot browse data/catalog/images/process state or signal supervisor", True)
        _, logs = execute(s, "sql-deny", token_a)
        check("restricted SQL role cannot read secret table/files, COPY PROGRAM or assume operator role", "sql-denied" in logs["stdout"] and all(message in logs["stderr"] for message in ["permission denied for table secret", "permission denied for function pg_read_file", "pg_execute_server_program", 'permission denied to set role "operator"']))
        _, logs = execute(s, "unrelated", token_b)
        check("unrelated client cannot connect by name or numeric address while Postgres listens", "unrelated-denied" in logs["stdout"] and s.call(["status", db["id"]])["state"] == "running")
        for op in [
            {"op": "run", "template": "app", "mounts": [{"volume": "pgdata", "target": "/results", "mode": "rw"}]},
            {"op": "run", "template": "app", "command": ["/bin/sh"]},
            {"op": "volume_inspect", "name": "../pgdata"},
            {"op": "volume_inspect", "name": ids["pgdata"]},
            {"op": "volume_inspect", "name": str(mounted[2] / "data")},
        ]:
            assert "error" in raw(s, token_a, op)
        check("raw API cannot override commands/mounts or turn names, IDs and host paths into authority", True)
        # A distinct logs grant is scoped to instances created under that grant.
        logs_token = issue(s, "logs-agent", ["verify"], logs=True)
        i, _ = execute(s, "verify", logs_token)
        assert s.call(["logs", i["id"]], logs_token)["complete"]
        assert s.call(["logs", instance["id"]], logs_token, expected=3)["error"]["code"] == "not_found"
        check("bounded raw logs require separate permission and matching grant ownership", True)
        # Exclusion is bidirectional, readers can share only without a writer.
        writer = s.call(["run", "writer"], token_a)
        writer_group = Path(config["cgroup"]) / writer["id"]
        writer_pids = eventually(lambda: (p if len(p := r.host_processes(writer_group)) >= 5 else None), "writer and live children")
        for args in [["run", "writer"], ["run", "result-reader"], ["volume", "rm", "results"], ["volume", "recover", "results"]]:
            assert s.call(args, token_a if args[0] == "run" else None, expected=4)["error"]["code"] == "volume_busy"
        s.call(["stop", writer["id"]], token_a); s.terminal(writer, token_a)
        eventually(lambda: all(not Path('/proc/%d' % pid).exists() for pid in writer_pids), "writer and all children exited")
        check('volume writer reservation releases only after actual parent/children exit and whole cgroup removal', not writer_group.exists())
        readers = [s.call(["run", "reader"], token_a) for _ in range(2)]
        assert s.call(["run", "fixture-writer"], expected=4)["error"]["code"] == "volume_busy"
        for i in readers: s.call(["stop", i["id"]], token_a); s.terminal(i, token_a)
        check("exclusive writer rejects all competing attachments and deletion; multiple readers share without writer", True)
        _, logs = execute(s, "disk", token_a)
        check("persistent filesystem enforces actual disk ENOSPC", "disk-ceiling-enforced" in logs["stdout"] and "No space left on device" in logs["stderr"])
        _, logs = execute(s, "symlink", token_a)
        check("mounted data symlinks cannot escape workload root or expose other volume", "symlink-escape-denied" in logs["stdout"])
        assert s.call(["run", "wrong-owner"], expected=5)["error"]["code"] == "launch_failed"
        for name in ["unsafe-alias", "unsafe-target", "unsafe-input", "unsafe-env", "unsafe-relative", "unsafe-traversal"]:
            assert s.call(["run", name], token_a, expected=5)["error"]["code"] == "launch_failed"
        error = s.call(["run", "unsafe-protected"], expected=5)["error"]
        assert error["code"] == "launch_failed" and "Executable input" in error["message"], error
        results_entry = next(json.loads(p.read_bytes()) for p in (root / "catalog").iterdir() if json.loads(p.read_bytes())["name"] == "results")
        assert not (slots / results_entry["slot"] / "data/escalated").exists()
        check("image target symlinks, wrong data ownership and mutable executable/config/environment aliases fail on actual supervisor", True)
        # Source substitution fails before bind; restoring original inode is safe.
        meta_file = next(p for p in (root / "catalog").iterdir() if json.loads(p.read_bytes())["name"] == "results")
        entry = json.loads(meta_file.read_bytes()); data = slots / entry["slot"] / "data"
        saved = data.with_name("saved-data"); data.rename(saved)
        data.symlink_to(mounted[3] / "data", target_is_directory=True)
        assert s.call(["run", "writer"], token_a, expected=5)["error"]["code"] == "launch_failed"
        data.unlink(); saved.rename(data)
        check("symlink-swapped managed source fails closed without leaking the alternate data", True)
        racing = threading.Event(); racing.set()
        def swap_source():
            while racing.is_set():
                data.rename(saved)
                data.symlink_to(mounted[3] / "data", target_is_directory=True)
                time.sleep(0.0005)
                data.unlink(); saved.rename(data)
                time.sleep(0.0005)
        thread = threading.Thread(target=swap_source); thread.start()
        successes = 0
        try:
            for _ in range(16):
                value = raw(s, token_a, {"op":"run", "template":"race"})
                if "error" in value:
                    assert value["error"]["code"] == "launch_failed", value
                else:
                    state = s.terminal(value, token_a)
                    logs = s.call(["logs", value["id"]])
                    assert state["exit_code"] == 0 and "race-safe" in logs["stdout"], (state, logs)
                    successes += 1
        finally:
            racing.clear(); thread.join()
        r.REPORT["symlink_race"] = {"attempts":16, "safe_launches":successes, "rejected_before_exec":16-successes}
        check("repeated concurrent source symlink swaps either reject or bind the catalogued directory inode", True)
        # Restricted native agents cannot perform even one parent/source replacement.
        result = subprocess.run(agent_prefix(65534) + ["ln", "-s", str(mounted[3] / "data"), str(data)], capture_output=True)
        assert result.returncode != 0
        check("actual restricted agent cannot rename or substitute source paths", True)
        # Reusing a friendly name creates a new resource, never authority for an old token.
        old_fixture = ids["fixtures"]
        s.call(["volume", "rm", "fixtures"])
        new_fixture = s.call(["volume", "create", "fixtures"])
        assert new_fixture["id"] != old_fixture
        agent_cli(s, a, ["volume", "inspect", "fixtures"], 3)
        agent_cli(s, a, ["run", "app"], 3)
        rebound = issue(s, "rebound", ["reader"])
        assert s.call(["volume", "inspect", "fixtures"], rebound)["id"] == new_fixture["id"]
        check("old grants cannot follow removed/recreated friendly volume names; fresh issuance binds the new managed ID", True)
        # Stop jobs, then replace the static service with a fresh stable ID.
        s.call(["stop", db["id"]]); s.terminal(db)
        s.call(["up", s.manifest])
        replacement = next(i for i in s.call(["ps"])["instances"] if i["template"] == "db" and i["state"] == "running")
        _, logs = execute(s, "verify", token_a)
        check("row persists after real server replacement using the same volume ID", replacement["id"] != db["id"] and "durable-row" in logs["stdout"] and s.call(["volume", "inspect", "pgdata"])["id"] == ids["pgdata"])
        # Kill the actual supervisor while a writer is live. Preserve endpoint and journal.
        live_group = Path(config["cgroup"]) / replacement["id"]
        r.REPORT['crash_writer_process_count'] = len(r.host_processes(live_group))
        check("crash test starts with live writer and populated whole cgroup", "populated 1" in (live_group / "cgroup.events").read_text() and r.REPORT['crash_writer_process_count'] >= 5)
        os.kill(int(s.pid_file.read_text()), signal.SIGKILL)
        s.process.wait(timeout=10)
        s.output.close()
        assert json.loads(next(p for p in (root / "catalog").iterdir() if json.loads(p.read_bytes())["name"] == "pgdata").read_bytes())["attachments"]
        # Fresh state cannot evade existing cgroup cleanup.
        blocked = new_server()
        err = blocked.call(["up", blocked.manifest], expected=5)
        assert err["error"]["code"] == "cleanup_unverified", err
        blocked.close()
        check("supervisor death with live writer retains journal and prevents reattachment from fresh state", True)
        # Trusted recovery only kills this task's recorded cgroups and checks the kernel's emptiness.
        for group in Path(config["cgroup"]).iterdir():
            if group.is_dir():
                (group / "cgroup.kill").write_text("1")
                eventually(lambda: "populated 0" in (group / "cgroup.events").read_text(), "old writer cgroup empty")
                group.rmdir()
        recovered = new_server()
        assert recovered.call(["up", recovered.manifest], expected=4)["error"]["code"] == "volume_busy"
        recovered.call(["volume", "rm", "pgdata"], expected=4)
        recovered.call(["volume", "recover", "pgdata"])
        recovered.call(["up", recovered.manifest])
        fresh = issue(recovered, "fresh", ["verify"])
        _, logs = execute(recovered, "verify", fresh)
        check("explicit recovery after verified tree removal preserves committed Postgres row", "durable-row" in logs["stdout"])
        for instance in recovered.call(["ps"])["instances"]:
            if instance["state"] not in ("exited", "failed"):
                recovered.call(["stop", instance["id"]]); recovered.terminal(instance)
        recovered.call(["volume", "rm", "other"]); recovered.call(["volume", "rm", "other"])
        check("operator-only unmounted deletion is idempotent and preserves unrelated volume IDs", recovered.call(["volume", "inspect", "pgdata"])["id"] == ids["pgdata"])
        r.REPORT["cli_transcript"] = transcript
    finally:
        for s in reversed(servers):
            s.close()
        for slot in reversed(mounted):
            r.run(["sudo", "umount", slot])
        if agent_root.exists():
            r.run(["sudo", "rm", "-rf", agent_root])
        shutil.rmtree(root)
    check("persistent acceptance removes every task-owned mount and workload cgroup", not any(p.is_dir() for p in Path(config["cgroup"]).iterdir()))
