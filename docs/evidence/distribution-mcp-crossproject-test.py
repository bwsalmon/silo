"""Supplemental actual-archive two-project MCP test, not a full qualification.
Run with --suite runner --frontend mcp and the extracted binary/helper/report.
The normal full-suite verdict intentionally remains blocked for this partial run.
"""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests"))
import real_worker as r


def suite(workspace, worker, registry, probe):
    image = registry.image({"User":"0:0"}, [r.tar_layer([r.file("probe", probe, mode=0o755)])])
    limits = {"processes":64,"memory_bytes":134217728,"cpu_millis":1000,
        "scratch_bytes":8388608,"connections":1,"duration_secs":60,"max_instances":2}
    template = {"image":image,"command":["/probe","sleep"],"uid":0,"gid":0,"limits":limits,
        "agent":["run","list","status","logs","stop"]}
    # Each supervisor requires its own exclusive cgroup domain.
    (Path(worker["cgroup"])/"cgroup.subtree_control").write_text("+cpu +memory +pids")
    mounts = []
    r.REPORT["project_workers"] = []
    servers = []
    try:
        for project in ("project-a","project-b"):
            document = {"version":1,"project":project,"jobs":{"probe":template},"network":{"external":"deny"}}
            config = copy.deepcopy(worker)
            domain = Path(worker["cgroup"])/project
            domain.mkdir(mode=0o700)
            config["cgroup"] = str(domain)
            storage = workspace/(project+"-storage")
            storage.mkdir(mode=0o700)
            r.run(["sudo","mount","-t","tmpfs","-o",
                   "size=1G,nr_inodes=65536,mode=0700,uid=%d,gid=%d" % (os.getuid(),os.getgid()),
                   "silo-project-supplement",storage])
            mounts.append(storage)
            config["storage"] = str(storage)
            r.REPORT["project_workers"].append(config)
            s = r.Supervisor(r.BINARY, workspace, config, document)
            servers.append(s)
            spec = {"session":"agent","project":project,"policy":s.policy,"expires_at":int(time.time())+180,
                "scope":{"templates":["probe"],"permissions":["run","list","status","logs","stop"],"max_instances":1}}
            path = s.dir / "grant.json"; path.write_bytes(r.encoded(spec))
            s.project = project
            s.token = s.dir / "restricted-token"
            with s.token.open("wb") as output:
                s.call(["operator","issue",path,"--token-fd",output.fileno()],extra_fds=(output.fileno(),))
            s.token.chmod(0o600)
        a,b = servers
        ia = a.call(["run","probe"],a.token)
        ib = b.call(["run","probe"],b.token)
        r.check("two distinct project identities launch real jobs through MCP",
                ia["state"]=="running" and ib["state"]=="running" and ia["id"]!=ib["id"])
        for source, own, foreign in [(a,ia,ib),(b,ib,ia)]:
            listed = source.call(["ps"],source.token)["instances"]
            assert {i["id"] for i in listed} == {own["id"]}
            for operation in ("status","stop","logs"):
                hidden = source.call([operation,foreign["id"]],source.token,expected=3)
                unknown = source.call([operation,"job_"+"0"*64],source.token,expected=3)
                assert hidden==unknown and hidden["error"]["code"]=="not_found"
            # Bypass discovery and invoke the tools directly with forbidden identity,
            # project, credential, endpoint and destination fields.
            client = source.mcp_clients[str(source.token)]
            for field,value in [("project","other"),("identity","other"),("token","secret"),
                                ("endpoint","/other/private/endpoint.json"),("destination","other:5432")]:
                result = client.call("silo_status",{"id":foreign["id"],field:value})
                assert result["error"]["code"]=="invalid_request"
        r.check("direct cross-project list/status/stop/log calls preserve hidden-versus-unknown equivalence",True)
        r.check("direct tool calls cannot override bound project identity credential endpoint or destination",True)
        # A valid project-B credential cannot authenticate to project A, even when
        # supplied by the launcher; no model-visible credential field exists.
        with b.token.open("rb") as auth:
            result = subprocess.run([r.BINARY,"mcp","serve","--endpoint",str(a.endpoint),
                "--auth-fd",str(auth.fileno())],pass_fds=(auth.fileno(),),capture_output=True,timeout=5)
        r.check("foreign project credential fails closed before MCP initialization",
                result.returncode==3 and result.stdout==b"" and b.token.read_bytes().strip() not in result.stderr)
        for source, instance in [(a,ia),(b,ib)]:
            source.call(["stop",instance["id"]],source.token)
            source.terminal(instance,source.token)
            assert not (Path(worker["cgroup"])/source.project/instance["id"]).exists()
        r.check("both project jobs receive verified whole-cgroup teardown through MCP",True)
    finally:
        for server in reversed(servers):
            server.close()
        for mount in reversed(mounts):
            r.run(["sudo","umount",mount])


r.suite = suite
if __name__ == "__main__":
    r.main()
