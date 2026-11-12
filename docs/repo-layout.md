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
      toolset.yaml          # the manifest
      server.py | main.go | index.js | ...   # the MCP server's code
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

Each directory is one **tool set**: an MCP server, as code, plus its
manifest. The directory name is the tool set's name. It must be
lowercase letters, digits, `-` and `_`, at most 40 characters, and not
one of the built-ins (`repo-read`, `repo-write`, `scratch`, `silo`).

**Every tool set committed here is enabled.** It is loaded when the
session starts. It is also loaded, or restarted in place if it changed,
as soon as a `repo-write` commit you approve, or a `silo.refresh`, brings
a change to its directory. There is no separate enable step. The only way
to keep a committed set out of a session is the `disabled` list in
`.silo/config.yaml` or in silo's own config.

The agent sees the set's tools as `<toolset>.<tool>`.

### `toolset.yaml`

```yaml
name: tickets                     # must equal the directory name
description: Search and file tickets in the tracker.

# Optional. Runs once before the server starts, in this directory.
# Write output to ${BUILD_DIR}, a per-session directory outside the
# checkout, so builds never dirty the working tree.
build: [go, build, -o, "${BUILD_DIR}/tickets", .]

# Required. Starts the MCP server, which speaks newline-delimited
# JSON-RPC on stdin and stdout. It runs in the checkout's root.
run: ["${BUILD_DIR}/tickets"]

# Environment the server gets, on top of PATH and the SILO_* variables.
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

Substitutions in `build`, `run` and `env` values:

| Variable | Value |
|---|---|
| `${TOOLSET_DIR}` | this directory, in the checkout (or the one-off's directory) |
| `${BUILD_DIR}` | a fresh per-session build directory for this set |
| `${CHECKOUT}` | the checkout's root |

A `run` or `build` command that starts with `./` or `../` is resolved
against the tool set's directory. Any other command is looked up on
silo's `PATH`.

**Environment.** The server runs with only these variables:

- `PATH`;
- `SILO_TOOLSET_DIR`, `SILO_BUILD_DIR` and `SILO_CHECKOUT`;
- what `env` sets;
- what `pass_env` names.

`HOME` isn't passed, so pass or set it if the server needs it. The
`build` step also gets `HOME`, `TMPDIR` and silo's `GO*` variables, so
that toolchains can find their caches.

**Errors.** A build that fails, a server that fails to start, or a
failed MCP handshake (30 s limit) is reported:

- at session start, in silo's startup output;
- after a commit or a refresh, as a tool error back to the agent, with
  the build output or the server's stderr.

If a changed set fails to reload, its previous version keeps running.
Each server's stderr goes to `~/.silo/sessions/<id>/logs/<toolset>.stderr`.

**Writing a server.** Any language works. The server must answer:

- `initialize`;
- `tools/list`;
- `tools/call`;
- `ping` (optional).

It may send `notifications/tools/list_changed`, and silo re-lists its
tools when it does. The example repository has a Python server that uses
only the standard library (`.silo/tools/runbooks/server.py`) and a Go one
(`.silo/tools/clock/main.go`).

## One-off tool sets

The agent can also write a tool set that lasts only for the session,
with `scratch.define`. A one-off uses the same format: a `toolset.yaml`
manifest plus files.

- **Location.** One-offs are written under
  `~/.silo/sessions/<id>/tools/<name>/`, never into the checkout.
- **Naming.** The agent sees a one-off's tools as
  `scratch.<name>.<tool>`.
- **Approval.** One-off code runs on your machine, so silo shows you all
  of it before it is created, and again, with a diff, before any change.
- **Lifetime.** One-offs are deleted when the session ends.
- **Promotion.** `scratch.promote` proposes a commit that adds the
  one-off to `.silo/tools/<name>/`. Once you approve it, the set loads as
  `<name>.<tool>` and the one-off is removed.
