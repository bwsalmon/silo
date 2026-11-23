"""Actual executable protocol regressions; these do not qualify a worker."""
import json
import os
from pathlib import Path
import subprocess
import time
import unittest

from mcp_client import Client, Fixture, smoke

ROOT = Path(__file__).resolve().parents[1]
BINARY = Path(os.environ.get("SILO_TEST_BUNDLE_ROOT", ROOT / "target/debug")) / "silo"


@unittest.skipUnless(BINARY.is_file(), "requires compiled silo binary")
class McpTests(unittest.TestCase):
    def setUp(self):
        self.fixture = Fixture(BINARY, ROOT / "examples/foundation.yaml")
        self.addCleanup(self.fixture.close)

    def test_actual_binary_protocol_and_schema(self):
        token, _ = self.fixture.issue()
        with Client(BINARY, self.fixture.endpoint, token) as client:
            tools = client.request("tools/list")["result"]["tools"]
            self.assertEqual(tools, json.loads((ROOT / "docs/mcp-tools.json").read_text())["tools"])
            self.assertEqual({t["name"] for t in tools},
                             {"silo_run","silo_ps","silo_status","silo_stop","silo_logs"})
            self.assertTrue(all(t["inputSchema"]["additionalProperties"] is False for t in tools))
            self.assertEqual(client.call("silo_ps")["data"]["instances"], [])
            for name, args in [
                ("silo_run", {"template":"app","image":"override"}),
                ("silo_run", {"template":"app","entrypoint":[]}),
                ("silo_run", {"template":"app","mounts":[]}),
                ("silo_run", {"template":"app","endpoint":"/secret"}),
                ("silo_ps", {"identity":"other"}),
                ("silo_ps", {"token":"secret"}),
                ("silo_exec", {"command":"id"}),
                ("silo_status", {"id":None}),
                ("silo_status", {"id":"job_"+"0"*64,"path":"/secret"}),
            ]:
                result = client.call(name, args)
                self.assertEqual(result["error"]["code"], "invalid_request")
                self.assertNotIn("secret", json.dumps(result))
            unknown = "job_"+"0"*64
            for name in ("silo_status","silo_stop","silo_logs"):
                self.assertEqual(client.call(name, {"id":unknown})["error"]["code"], "not_found")

    def test_revocation_expiry_discovery_and_redaction(self):
        token, grant = self.fixture.issue("revoke")
        with Client(BINARY, self.fixture.endpoint, token) as client:
            self.fixture.cli(["operator","revoke",grant])
            self.assertEqual(client.call("silo_ps")["error"]["code"], "unauthenticated")
            self.assertIn("error", client.request("tools/list"))
            self.assertEqual(client.diagnostics(), b"")
        token, _ = self.fixture.issue("expiry", expires=int(time.time())+2)
        with Client(BINARY, self.fixture.endpoint, token) as client:
            time.sleep(2.1)
            self.assertEqual(client.call("silo_ps")["error"]["code"], "unauthenticated")
        # Invalid identities fail before protocol traffic; errors never echo input.
        for credential in (token, self.fixture.operator):
            with credential.open("rb") as auth:
                output = subprocess.run([str(BINARY),"mcp","serve","--endpoint",str(self.fixture.endpoint),
                    "--auth-fd",str(auth.fileno())], pass_fds=(auth.fileno(),), capture_output=True)
            self.assertNotEqual(output.returncode, 0)
            self.assertEqual(output.stdout, b"")
            self.assertNotIn(credential.read_bytes().strip(), output.stderr)

    def test_malformed_oversized_batches_and_disconnect_fail_closed(self):
        token, _ = self.fixture.issue()
        for payload in (b'{"broken":', b"x"*16385, b"[]", b"null",
                        b'{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"silo_ps","arguments":null}}',
                        b'{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"silo_ps","token":"secret"}}',
                        b'{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"silo_run","arguments":'):
            client = Client(BINARY, self.fixture.endpoint, token)
            try:
                client.process.stdin.write(payload+b"\n")
                client.process.stdin.flush()
                client.process.wait(timeout=5)
                self.assertEqual(client.process.stdout.read(), b"")
                self.assertEqual(client.diagnostics(), b"")
            finally:
                client.close()
        client = Client(BINARY, self.fixture.endpoint, token)
        client.close()
        self.assertEqual(client.process.returncode, 0)
        self.assertEqual(self.fixture.cli(["ps"])["instances"], [])

    def test_inherited_credential_pipe_never_enters_protocol_or_diagnostics(self):
        # Transfer credentials through a dedicated pipe, never argv/environment.
        # EOF and protocol shutdown must leave no token in output/diagnostics.
        token, _ = self.fixture.issue()
        read_fd, write_fd = os.pipe()
        secret = token.read_bytes()
        os.write(write_fd, secret)
        os.close(write_fd)
        process = subprocess.Popen([str(BINARY),"mcp","serve","--endpoint",str(self.fixture.endpoint),
            "--auth-fd",str(read_fd)], pass_fds=(read_fd,), stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        os.close(read_fd)
        initial = {"jsonrpc":"2.0","id":1,"method":"initialize","params":{
            "protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fd-test","version":"1"}}}
        output, errors = process.communicate(json.dumps(initial).encode()+b"\n"+b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n', timeout=5)
        self.assertNotIn(secret.strip(), output+errors)
        # The only stdout line, if delivered before EOF, is a protocol response.
        for line in output.splitlines():
            self.assertEqual(json.loads(line)["jsonrpc"], "2.0")
        self.assertEqual(errors, b"")
