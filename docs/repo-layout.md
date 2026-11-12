# Context repository layout

A context repository is an ordinary git repository. silo reads
everything a session needs from its `.silo/` directory:

```
.silo/
  prompt.md                 # the session's objective (optional)
  config.yaml               # repository defaults (optional)
  context/                  # documents loaded as context (optional)
    overview.md
    ...
  tools/                    # tool sets, one directory each (optional)
    <toolset>/
      start.sh | start.py   # the entry point: exactly one of the two
      toolset.yaml          # metadata (optional)
      ...                   # whatever else the server needs
    vendor/                 # tool sets copied from other repositories
      <alias>/
        VENDOR.lock         # where they came from (written by silo vendor)
        <toolset>/
          start.sh | start.py
          ...
```

Everything else in the repository is content the agent reads with
`repo-read` and changes with `repo-write`. A worked example is in
[examples/context-repo](../examples/context-repo).

## `.silo/prompt.md`

The session's objective: what the agent is for, how it should work, and
what a good change looks like. silo builds the CLI's system prompt from:

1. its own preamble, which explains the tool sets and the approval flow;
2. `# Objective`: this file, verbatim;
3. `# Context`: the files in `.silo/context/`.

## `.silo/context/`

Every file under this directory, recursively, in path order. With
`context.mode: inline` (the default), each text file's contents go into
the system prompt under a `## <path>` heading, up to
`context.max_inline_bytes` in total (default 64 KiB). Files past the
budget, and binary files, are listed instead, and the agent can read
them with `repo-read.read_file`. With `context.mode: list`, every file is
listed and none is inlined.

## `.silo/config.yaml`

```yaml
# Approved commits are pushed to this branch instead of silo/<session-id>.
# silo's own push.branch setting wins over it.
push_branch: runbook-updates

# Tool sets in .silo/tools not to load. silo's config can disable more.
disabled: [experimental]

context:
  mode: inline              # inline | list
  max_inline_bytes: 65536
```

## `.silo/tools/<toolset>/`

Each directory is one **tool set**: an MCP server, as code, plus an
entry point that starts it. The directory name is the tool set's name. It
must be lowercase letters, digits, `-` and `_`, at most 40 characters,
and not one of the built-ins (`repo-read`, `repo-write`, `scratch`,
`silo`) or `vendor`, which holds [vendored tool sets](#vendored-tool-sets).

**Every tool set committed here is enabled.** It is loaded when the
session starts. It is also loaded, or restarted if it changed, as soon
as a `repo-write` commit you approve, or a `silo.refresh`, brings a
change to its directory. There is no separate enable step. The only way
to keep a committed set out of a session is the `disabled` list in
`.silo/config.yaml` or in silo's own config.

The agent sees the set's tools as `<toolset>.<tool>`.

### The entry point: `start.sh` or `start.py`

A tool set's directory holds **exactly one** entry point, `start.sh` or
`start.py`. Having both, or neither, is an error reported when the set
loads. To start the set, silo:

1. picks a free port on loopback;
2. runs, in the tool set's directory, `bash start.sh <port>` or
   `python3 start.py <port>` (the interpreter is looked up on silo's
   `PATH`; the file needs no execute bit), with `SILO_PORT=<port>` in its
   environment as well;
3. waits for the server to accept connections on `127.0.0.1:<port>` and
   to answer an MCP `initialize` there, for up to 2 minutes.

The server must speak MCP over the **Streamable HTTP** transport at
`http://127.0.0.1:<port>/mcp`, the default path of the official SDKs.
Each request is a POST, and the server may answer with JSON or with a
stream of server-sent events. If the server offers a GET stream on the
same endpoint, silo listens to it for `notifications/tools/list_changed`
and re-lists the set's tools when one comes.

What the entry point does is its own business: `pip install` into a
virtualenv, `go build` or `go run`, `npm start`, or `exec` a prebuilt
binary. silo builds nothing. Build output and caches belong outside the
checkout, in `$SILO_STATE_DIR` or under `$HOME`, so the working tree
stays clean. An entry point that does slow work first, such as an
install, should cache it, because the set starts again on every reload.
Ending the script with `exec`, so the server replaces the shell, keeps
the process tree simple.

**Loopback only.** The server must bind to `127.0.0.1`, never to
`0.0.0.0`, `::` or another interface, since anyone who can reach the
port can call its tools. On Linux silo checks this once the server is
ready. It reads `/proc/net/tcp` and `/proc/net/tcp6`, and refuses the
set, stopping it, if anything listens on its port on a non-loopback
address, or if any process in the set's process group listens on a
non-loopback address on any port. On other systems the rule is not
checked, only documented.

**Environment.** The entry point and everything it starts run with only
these variables:

| Variable | Value |
|---|---|
| `PATH`, `HOME`, `TMPDIR`, `GO*` | copied from silo's environment, so interpreters, toolchains and their caches can be found |
| `SILO_PORT` | the port to listen on, also passed as the first argument |
| `SILO_TOOLSET` | the tool set's name (`<alias>.<set>` for a vendored one, `scratch.<name>` for a one-off) |
| `SILO_TOOLSET_DIR` | this directory, in the checkout (or the one-off's directory) |
| `SILO_STATE_DIR` | a per-session directory for this set, outside the checkout, kept across its restarts and deleted when the session ends |
| `SILO_SESSION_DIR` | the session's directory, `~/.silo/sessions/<id>` |
| `SILO_CHECKOUT` | the checkout's root |

They also get what `toolset.yaml`'s `env` sets and what its `pass_env`
names. Anything else in silo's environment, such as tokens, is not
passed unless `pass_env` names it.

**Lifetime.** Each entry point runs in a process group of its own. To
stop a set, on reload, on removal or at the end of the session, silo
ends its MCP session, sends `SIGTERM` to the whole group, and sends
`SIGKILL` to the group 5 seconds later if anything in it is still
running. A process that leaves the group (with `setsid`, say) escapes
this, so don't do that. A changed set starts again on a new port before
its old server is stopped. If the new one fails to start, the old one
keeps running. A server that exits on its own stays down: calls to its
tools return an error with its output, and the next `silo.refresh`
restarts it.

**Errors.** An entry point that is missing, a duplicate, a server that
exits or doesn't answer in time, and a server bound beyond loopback are
reported:

- at session start, in silo's startup output;
- after a commit or a refresh, as a tool error back to the agent.

Each report includes the end of what the server printed since it
started. Everything a set's processes print to stdout or stderr goes to
`~/.silo/sessions/<id>/logs/<toolset>.log`.

**Writing a server.** Any language works. The server must answer
`initialize`, `tools/list` and `tools/call`, and may answer `ping`. The
official MCP SDKs handle all of this. The example repository has one
tool set of each kind:

- `.silo/tools/runbooks/start.py` uses the official Python MCP SDK's
  Streamable HTTP server. If the SDK isn't installed, it installs it into
  a cached virtualenv first.
- `.silo/tools/clock/start.sh` builds a small Go server, which uses only
  the standard library, into `$SILO_STATE_DIR` and `exec`s it.

```python
# start.py, with the official Python MCP SDK (mcp 2.x; in 1.x the class
# is mcp.server.fastmcp.FastMCP and takes host and port itself)
import sys
from mcp.server.mcpserver import MCPServer

server = MCPServer("tickets")

@server.tool()
def search(query: str) -> str:
    """Search the tracker."""
    ...

server.run(transport="streamable-http", host="127.0.0.1", port=int(sys.argv[1]))
```

```sh
# start.sh, for a Node server
set -euo pipefail
npm ci --silent --prefix "$SILO_STATE_DIR" >&2   # or keep node_modules under $HOME
exec node server.js "$1"                         # listens on 127.0.0.1:$1, serves /mcp
```

### `toolset.yaml` (optional)

Metadata about the set. Every field is optional, and so is the file.

```yaml
name: tickets                     # if set, must equal the directory name
description: Search and file tickets in the tracker.

# Environment the entry point gets, on top of the variables above.
# ${CHECKOUT}, ${TOOLSET_DIR}, ${STATE_DIR}, ${SESSION_DIR}, ${PORT} and
# ${TOOLSET} are substituted in the values.
env:
  TICKETS_URL: https://tickets.example.com
pass_env: [TICKETS_TOKEN]         # copied from silo's own environment

# Optional allowlist. Only these of the server's tools are exposed, and
# a description here overrides the server's own.
tools:
  - name: search
  - name: create
    description: File a new ticket. Use only after searching for duplicates.

# Tools whose every call needs your approval, with the arguments shown.
requires_approval: [create]
```

> **Changed:** `toolset.yaml` used to say how the server started, with
> `build` and `run` commands, and the server spoke MCP on stdin and
> stdout. Those keys are gone. A manifest that still has them fails to
> load with an error that points here. To move a tool set over, put what
> `build` and `run` did into a `start.sh` (`go build -o
> "$SILO_STATE_DIR/x" . && exec "$SILO_STATE_DIR/x" "$1"`), and switch
> the server to the Streamable HTTP transport on the port it is given.

## Vendored tool sets

To use tool sets from another context repository, **vendor** them: copy
them into this repository, where they are committed like any other tool
set. Sessions never fetch tool sets themselves, so a session runs only
code that is in its own repository's history, and you see every change
to it in a diff.

```sh
cd ~/context/payments-context
silo vendor team/shared-tools@v3 --toolsets tickets,clock --as shared
git add -A .silo/tools/vendor/shared && git diff --cached   # review it
git commit -m "Vendor tickets and clock from team/shared-tools@v3"
```

`silo vendor <repo>[@<ref>] [--toolsets a,b] --as <alias>`:

- `<repo>` is a name relative to `repos.root`, as for `silo run`, and is
  cloned from `repos.git_base` if it isn't there yet. silo fetches it,
  then reads the tool sets from the committed tree at `<ref>`: a branch
  (the remote's copy), a tag or a commit. The default is the default
  branch. Uncommitted files in the upstream checkout are never copied.
- `--toolsets` picks which of the upstream's `.silo/tools/<set>/` to copy.
  The default is all of them.
- Each set is copied to `.silo/tools/vendor/<alias>/<set>/`. Everything
  that was under `.silo/tools/vendor/<alias>/` before is replaced.
- `.silo/tools/vendor/<alias>/VENDOR.lock` records the upstream:

  ```yaml
  # Written by silo vendor. Do not edit: update with
  #   silo vendor --update shared [@<ref>]
  repo: team/shared-tools
  ref: v3                # the ref asked for, or the default branch's name
  commit: 4f1c9e0b...    # what it resolved to; the copied files are this commit's
  toolsets: [clock, tickets]
  ```

- `--into DIR` names the context repository. The default is the git
  repository around the current directory. silo writes the files but
  does not commit them, so you review and commit them yourself.

`silo vendor --update <alias> [@<ref>]` copies the alias's sets again from
the repository in its lock, at `<ref>` if you give one and otherwise at
the locked ref. A branch moves to its new tip, while a tag or commit
stays where it is. `--toolsets` changes which sets are kept.

**Inside a session**, the agent can do the same with `silo.vendor`
(`repo`, `ref`, `toolsets`, `alias`, and `update` to re-vendor). The
result is a `repo-write` proposal: silo shows you the full diff of the
vendored code and the lock, and commits only if you approve. Read it like
any code you are about to run, because committed tool sets start
automatically.

Vendored sets are ordinary committed tool sets. They load at session
start, hot-load when a commit or `silo.refresh` changes them, and use the
same entry-point convention. The agent sees their tools as
`<alias>.<set>.<tool>`. Rules that keep names unique:

- An alias follows the tool-set name rules. It may not be a built-in's
  name, `vendor`, or the name of one of this repository's own tool sets.
  If `.silo/tools/<x>/` and `.silo/tools/vendor/<x>/` both exist, neither
  loads, and the error says why.
- `disabled` in `.silo/config.yaml` or silo's config takes
  `<alias>.<set>` for one vendored set, or `<alias>` for all of them.
- **No transitive includes.** The upstream's own `.silo/tools/vendor/` is
  never copied, and `silo vendor` warns when it skips one. To use those
  sets, vendor them from the repository they came from.
- Only plain files are copied. A symlink or submodule in a vendored set
  is an error. Execute bits are kept, and everything is capped at 8 MiB.

## One-off tool sets

The agent can also write a tool set that lasts only for the session,
with `scratch.define`. A one-off uses the same convention: exactly one
`start.sh` or `start.py`, other files as needed, and an optional
`toolset.yaml`.

- **Location.** One-offs are written under
  `~/.silo/sessions/<id>/tools/<name>/`, never into the checkout. Their
  entry point runs there.
- **Naming.** The agent sees a one-off's tools as
  `scratch.<name>.<tool>`.
- **Approval.** One-off code runs on your machine, so silo shows you all
  of it before it is created, and again, with a diff, before any change.
- **Lifetime.** One-offs are deleted when the session ends.
- **Promotion.** `scratch.promote` proposes a commit that adds the
  one-off to `.silo/tools/<name>/`. Once you approve it, the set loads as
  `<name>.<tool>` and the one-off is removed.
