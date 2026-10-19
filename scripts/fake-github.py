"""A fake GitHub REST API for scripts/grain-interop.sh.

grain asks a team's git connection who its token is (GET /user) and
whether it may push to a task's repository (GET /repos/{owner}/{repo})
before it files the task. This answers both, for any token and any
repository, at / and at a GitHub Enterprise Server's /api/v3, so the
grain daemon in that job never reaches GitHub. Anything else is a 404.
Every request is logged to stderr.
"""
import json
import re
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def answer(self):
        path = self.path.split("?")[0]
        if path.startswith("/api/v3"):
            path = path[len("/api/v3"):]
        repo = re.fullmatch(r"/repos/([^/]+)/([^/]+)", path)
        if path == "/user":
            body = {"login": "grain-interop", "id": 1, "name": "grain-interop", "type": "User"}
        elif path == "/user/repos":
            if "page=1" in self.path or "page=" not in self.path:
                body = [
                    {
                        "id": 1, "name": "widgets", "full_name": "acme/widgets",
                        "owner": {"login": "acme"}, "private": False, "default_branch": "main",
                        "permissions": {"admin": True, "maintain": True, "push": True, "triage": True, "pull": True},
                    },
                    {
                        "id": 2, "name": "outside", "full_name": "acme/outside",
                        "owner": {"login": "acme"}, "private": False, "default_branch": "main",
                        "permissions": {"admin": True, "maintain": True, "push": True, "triage": True, "pull": True},
                    },
                ]
            else:
                body = []
        elif repo:
            owner, name = repo.groups()
            body = {
                "id": 1, "name": name, "full_name": owner + "/" + name,
                "owner": {"login": owner}, "private": False, "default_branch": "main",
                "permissions": {"admin": True, "maintain": True, "push": True, "triage": True, "pull": True},
            }
        else:
            self.send_response(404)
            self.end_headers()
            return
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    do_GET = do_POST = do_PATCH = do_PUT = answer


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
