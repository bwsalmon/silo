"""Small real stdio JSON-RPC MCP client used by artifact and kernel acceptance.
The Rust integration test independently uses the official rmcp client.
No token is serialized into this client's MCP messages.
"""
import json
import os
from pathlib import Path
import select
import subprocess
import tempfile
import time


class Client:
    def __init__(self, binary, endpoint, credential):
        self.secret = credential.read_bytes().strip()
        self.stderr = tempfile.TemporaryFile()
        with credential.open("rb") as auth:
            self.auth_fd = auth.fileno()
            self.process = subprocess.Popen(
                [str(binary), "mcp", "serve", "--endpoint", str(endpoint),
                 "--auth-fd", str(auth.fileno())],
                pass_fds=(auth.fileno(),), stdin=subprocess.PIPE,
                stdout=subprocess.PIPE, stderr=self.stderr)
        self.sequence = 0
        result = self.request("initialize", {"protocolVersion":"2025-06-18",
            "capabilities":{}, "clientInfo":{"name":"silo-acceptance","version":"1"}})
        assert result["result"]["protocolVersion"] == "2025-06-18", result
        self.send({"jsonrpc":"2.0","method":"notifications/initialized"})

    def send(self, message):
        data = json.dumps(message, separators=(",", ":")).encode()
        assert self.secret not in data
        self.process.stdin.write(data + b"\n")
        self.process.stdin.flush()

    def request(self, method, params=None):
        self.sequence += 1
        message = {"jsonrpc":"2.0","id":self.sequence,"method":method}
        if params is not None:
            message["params"] = params
        self.send(message)
        return self.receive(self.sequence)

    def receive(self, identifier, timeout=310):
        assert select.select([self.process.stdout], [], [], timeout)[0], "MCP response timed out"
        line = self.process.stdout.readline(300000)
        assert line and line.endswith(b"\n"), (self.process.poll(), self.diagnostics())
        assert self.secret not in line
        result = json.loads(line)
        assert result["jsonrpc"] == "2.0" and result["id"] == identifier, result
        return result

    def call(self, name, arguments=None):
        result = self.request("tools/call", {"name":name,"arguments":arguments or {}})
        assert "error" not in result, result
        tool = result["result"]
        value = tool["structuredContent"]
        assert value["schema_version"] == 1
        assert tool["isError"] == (not value["ok"])
        return value

    def diagnostics(self):
        self.stderr.seek(0)
        value = self.stderr.read()
        assert self.secret not in value
        return value

    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try:
                self.process.wait(timeout=315)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
                raise
        if not self.process.stdin.closed: self.process.stdin.close()
        self.diagnostics()
        self.process.stdout.close()
        self.stderr.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


class Fixture:
    """Real unqualified supervisor: only control/protocol checks, never kernel parity."""
    def __init__(self, binary, manifest):
        self.binary = Path(binary).resolve()
        self.manifest = Path(manifest)
        self.temporary = tempfile.TemporaryDirectory(prefix="silo-mcp-smoke-")
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.operator = self.root / "operator"
        self.operator.write_text("a" * 64)
        self.operator.chmod(0o600)
        self.endpoint = self.root / "endpoint.json"
        with self.operator.open("rb") as auth:
            self.supervisor = subprocess.Popen([str(self.binary), "supervisor", "--state-dir", str(self.root),
                "--auth-fd",str(auth.fileno())], pass_fds=(auth.fileno(),),
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.monotonic() + 5
        while not self.endpoint.exists():
            assert time.monotonic() < deadline and self.supervisor.poll() is None
            time.sleep(0.01)
        self.policy = self.cli(["up", str(manifest), "--no-start"])["policy"]

    def cli(self, args, extra_fds=()):
        with self.operator.open("rb") as auth:
            output = subprocess.run([str(self.binary)] + args + ["--endpoint",str(self.endpoint),
                "--auth-fd",str(auth.fileno()),"--json"], pass_fds=(auth.fileno(),)+extra_fds,
                capture_output=True, timeout=10, check=True)
        return json.loads(output.stdout)

    def issue(self, session="smoke", expires=None, permissions=None):
        grant = {"session":session,"project":"demo","policy":self.policy,
            "expires_at":expires or int(time.time())+600,
            "scope":{"permissions":permissions or ["list","status","stop","logs"],
                "templates":["app"],"max_instances":1}}
        # The foundation example's project is authoritative.
        import re
        text = self.manifest.read_text()
        grant["project"] = re.search(r"^project: (\S+)", text, re.M)[1]
        path = self.root / (session+".json")
        path.write_text(json.dumps(grant))
        token = self.root / (session+".token")
        with token.open("wb") as output:
            value = self.cli(["operator","issue",str(path),"--token-fd",str(output.fileno())],
                             (output.fileno(),))
        token.chmod(0o600)
        return token, value["grant"]

    def close(self):
        if self.supervisor.poll() is None:
            self.cli(["operator","shutdown"])
            self.supervisor.wait(timeout=10)
        self.temporary.cleanup()


def smoke(binary, manifest):
    fixture = Fixture(binary, manifest)
    try:
        token, _ = fixture.issue()
        with Client(binary, fixture.endpoint, token) as client:
            tools = client.request("tools/list")["result"]["tools"]
            assert len(tools) == 5
            assert all(t["inputSchema"]["additionalProperties"] is False for t in tools)
            assert client.call("silo_ps")["data"]["instances"] == []
    finally:
        fixture.close()
    return {"protocol":"2025-06-18","transport":"stdio","tools":5,"result":"passed"}
