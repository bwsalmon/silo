#!/usr/bin/env python3
"""A minimal MCP server (stdio, newline-delimited JSON-RPC) using only the
standard library. It searches the Markdown files under $RUNBOOK_DIR,
relative to the checkout it runs in."""
import json
import os
import sys

ROOT = os.environ.get("RUNBOOK_DIR", "runbooks")

TOOLS = [
    {
        "name": "search",
        "description": "Find runbook sections mentioning a phrase (case-insensitive).",
        "inputSchema": {
            "type": "object",
            "properties": {"query": {"type": "string"}},
            "required": ["query"],
        },
    },
    {
        "name": "list",
        "description": "List the runbooks.",
        "inputSchema": {"type": "object", "properties": {}},
    },
]


def runbooks():
    for dirpath, _, files in os.walk(ROOT):
        for name in sorted(files):
            if name.endswith(".md"):
                yield os.path.join(dirpath, name)


def search(query):
    hits = []
    q = query.lower()
    for path in runbooks():
        section = ""
        with open(path, encoding="utf-8") as f:
            for n, line in enumerate(f, 1):
                if line.startswith("#"):
                    section = line.strip("# \n")
                if q in line.lower():
                    hits.append(f"{path}:{n} [{section}] {line.strip()}")
    return "\n".join(hits) or "no matches"


def call(name, args):
    if name == "search":
        return search(args.get("query", ""))
    if name == "list":
        return "\n".join(runbooks()) or "no runbooks"
    raise ValueError(f"unknown tool {name}")


def reply(msg_id, result=None, error=None):
    msg = {"jsonrpc": "2.0", "id": msg_id}
    if error is not None:
        msg["error"] = error
    else:
        msg["result"] = result
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()


for line in sys.stdin:
    if not line.strip():
        continue
    req = json.loads(line)
    method, msg_id = req.get("method"), req.get("id")
    if msg_id is None:
        continue  # a notification
    if method == "initialize":
        reply(msg_id, {
            "protocolVersion": req["params"].get("protocolVersion", "2025-06-18"),
            "serverInfo": {"name": "runbooks", "version": "1"},
            "capabilities": {"tools": {}},
        })
    elif method == "ping":
        reply(msg_id, {})
    elif method == "tools/list":
        reply(msg_id, {"tools": TOOLS})
    elif method == "tools/call":
        p = req["params"]
        try:
            text, is_error = call(p["name"], p.get("arguments") or {}), False
        except Exception as e:  # report tool failures to the agent
            text, is_error = str(e), True
        reply(msg_id, {"content": [{"type": "text", "text": text}], "isError": is_error})
    else:
        reply(msg_id, error={"code": -32601, "message": f"method not found: {method}"})
