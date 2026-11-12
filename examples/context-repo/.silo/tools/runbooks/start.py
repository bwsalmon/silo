"""The runbooks tool set: an MCP server built on the official Python MCP
SDK (https://github.com/modelcontextprotocol/python-sdk), searching the
Markdown files under $RUNBOOK_DIR.

silo runs `python3 start.py <port>` in this directory; the server must
answer MCP over Streamable HTTP at http://127.0.0.1:<port>/mcp. If the
SDK is not importable, this script first installs it into a virtualenv
it keeps under ~/.cache/silo/runbooks-venv and re-runs itself there:
how a tool set gets its dependencies is up to its entry point.
"""
import os
import subprocess
import sys
import venv

try:
    from mcp.server.mcpserver import MCPServer  # mcp 2.x
except ImportError:
    try:
        from mcp.server.fastmcp import FastMCP as MCPServer  # mcp 1.x
    except ImportError:
        MCPServer = None


def bootstrap():
    """Install the SDK into a cached virtualenv and re-run this script."""
    cache = os.environ.get("XDG_CACHE_HOME") or os.path.join(os.path.expanduser("~"), ".cache")
    env_dir = os.path.join(cache, "silo", "runbooks-venv")
    python = os.path.join(env_dir, "bin", "python3")
    marker = os.path.join(env_dir, ".installed")
    if os.environ.get("RUNBOOKS_BOOTSTRAPPED"):
        sys.exit(f"runbooks: the MCP SDK is still not importable from {env_dir}")
    if not os.path.exists(marker):
        print(f"runbooks: installing the MCP SDK into {env_dir}", file=sys.stderr)
        venv.create(env_dir, with_pip=True, clear=True)
        subprocess.check_call([python, "-m", "pip", "install", "--quiet", "mcp>=1.10"], stdout=sys.stderr)
        open(marker, "w").close()
    os.environ["RUNBOOKS_BOOTSTRAPPED"] = "1"
    os.execv(python, [python, os.path.abspath(__file__), *sys.argv[1:]])


CHECKOUT = os.environ.get("SILO_CHECKOUT", ".")
ROOT = os.environ.get("RUNBOOK_DIR", os.path.join(CHECKOUT, "runbooks"))


def runbooks():
    for dirpath, _, files in os.walk(ROOT):
        for name in sorted(files):
            if name.endswith(".md"):
                yield os.path.join(dirpath, name)


def main():
    port = int(sys.argv[1])
    if MCPServer is None:
        bootstrap()
    # Bind to loopback only: silo refuses a tool set that listens
    # anywhere else.
    if MCPServer.__name__ == "FastMCP":
        server = MCPServer("runbooks", host="127.0.0.1", port=port)
        run = lambda: server.run(transport="streamable-http")
    else:
        server = MCPServer("runbooks")
        run = lambda: server.run(transport="streamable-http", host="127.0.0.1", port=port)

    @server.tool(name="search")
    def search(query: str) -> str:
        """Find runbook sections mentioning a phrase (case-insensitive)."""
        hits = []
        q = query.lower()
        for path in runbooks():
            section = ""
            with open(path, encoding="utf-8") as f:
                for n, line in enumerate(f, 1):
                    if line.startswith("#"):
                        section = line.strip("# \n")
                    if q in line.lower():
                        hits.append(f"{os.path.relpath(path, CHECKOUT)}:{n} [{section}] {line.strip()}")
        return "\n".join(hits) or "no matches"

    @server.tool(name="list")
    def list_runbooks() -> str:
        """List the runbooks."""
        return "\n".join(os.path.relpath(p, CHECKOUT) for p in runbooks()) or "no runbooks"

    run()


if __name__ == "__main__":
    main()
