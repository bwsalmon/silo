# Silo v2: design

**Status:** design; stage 1a (`silofile`, `silo validate`,
`silo schema`) is implemented. Written in task 3800 from the core
model bsalmon set out for v2. Where that model decided something, this
document says so plainly. Where it did not, the author made a call; each
call is marked **(call n)** and listed under [Calls made in this
draft](#calls-made-in-this-draft), so a reviewer can check them in one
place. Open questions are [at the end](#open-questions).

Companion documents:

- [`cli-mapping.md`](cli-mapping.md): how each `agent`, `remote_permissions`
  and mode setting turns into real `claude`, `codex` and `agy` flags, and
  which of those flags were checked against a real binary.
- [`grain.md`](grain.md): grain as silo's first client, what v2's core
  has to offer it, and a migration path.
- [`examples/`](examples/): complete silo folders for journeys 1–4 and a
  grain-style headless run.

## Contents

1. [What v2 is](#1-what-v2-is)
2. [The Silofile and silo folders](#2-the-silofile-and-silo-folders)
3. [`silo run`: one session, start to finish](#3-silo-run-one-session-start-to-finish)
4. [The gateway](#4-the-gateway)
5. [Locations](#5-locations)
6. [Remote permissions and CLI modes](#6-remote-permissions-and-cli-modes)
7. [Sandboxes](#7-sandboxes)
8. [Credentials](#8-credentials)
9. [The git proxy](#9-the-git-proxy)
10. [Journey 1: `silo create`](#10-journey-1-silo-create)
11. [Journey 2: Google project](#11-journey-2-google-project)
12. [Journey 3: remote agent, narrow local reach](#12-journey-3-remote-agent-narrow-local-reach)
13. [Journey 4: local and remote MCP servers](#13-journey-4-local-and-remote-mcp-servers)
14. [Future journeys 5–10](#14-future-journeys-510)
15. [Security model](#15-security-model)
16. [Package layout](#16-package-layout)
17. [Staged implementation plan](#17-staged-implementation-plan)
18. [Calls made in this draft](#calls-made-in-this-draft)
19. [Open questions](#open-questions)

## 1. What v2 is

Silo v2 does one thing: **run one agent CLI in a chosen location, with
access to a declared set of MCP tools, credentials, cloud permissions,
git access and local scripts. Nothing else.**

A silo is a declaration of that one thing, and `silo run` carries it out
once. There is no daemon, no tree of silos, no fence API, no facades, no
blocks or ledger, no grants and no network fence. All of those were v1
([`docs/silo/`](../../docs/silo/README.md)). v1's gateway was a network
fence around a whole virtual cloud. v2's gateway is one MCP server for
one agent session, and the only thing the agent is told about.

### 1.1 Vocabulary

| Term | Meaning |
| --- | --- |
| **silo** | A Silofile, or a silo folder holding one. |
| **session** | One `silo run` of a silo: setup, one agent CLI process, teardown. |
| **invoking machine** | Where `silo run` was typed. silo, the gateway, credentials and the local executor always live here in v2.0. |
| **agent host** | Where the agent CLI process runs: the invoking machine for `local`, another machine for `ssh`. |
| **gateway** | silo's one MCP server per session. The CLI's MCP config names it and nothing else. |
| **sandbox** | A named place where commands run on the agent's behalf (a container, a gVisor container, later a granule VM). Reached only through gateway tools. |
| **remote permissions** | What the CLI's *own* built-in tools may do on the agent host. |
| **local executor** | The gateway's tools that run declared scripts on the invoking machine (journey 3). |

### 1.2 Principles

1. **Declared or refused.** Anything the agent can reach is in the
   Silofile. A gateway call to anything undeclared is refused, and the
   refusal is audited.
2. **Refuse rather than approximate.** If a CLI cannot express what the
   Silofile asks for exactly, silo refuses to start. It never runs a
   looser configuration and hopes.
3. **One path back.** The gateway (and, for git, the git proxy behind
   it) is the only route from the agent to anything silo holds. For a
   remote agent that route is one `ssh -R` forward.
4. **Credentials stay with silo.** A secret reaches the agent's own
   process only when the Silofile delivers it there by name.
5. **One-shot.** `silo run` sets up, runs, and tears down. What it leaves
   behind on purpose is listed per journey; anything else is a bug.

## 2. The Silofile and silo folders

### 2.1 Format: YAML, strictly decoded

The Silofile is **YAML** (the starting proposal, kept). The reasons:

- Agents and people both write it. YAML is what both already write for
  CI, compose and Kubernetes, and journey 1 has an agent writing it.
- It nests cleanly: credentials with several destinations, MCP servers
  with allowlists. TOML's nested arrays of tables are harder to read.
- Comments matter in a file that grants permissions. JSON has none.

HCL and CUE were considered. HCL adds a language and tooling for no
gain at this size. CUE's constraints are attractive, but they make the
file harder to write for exactly the people (and agents) who write it.

YAML's known traps are closed by how silo reads it **(call 1)**:

- Decoding is **strict**: an unknown key is an error, never ignored. A
  misspelt `remote_permision: none` must not quietly mean the default.
- Every scalar silo reads as a string is read as a string. `no`, `on`,
  and `0700` are not reinterpreted. A field that is a duration
  (`ttl: 7d`) or a number has its own parser.
- Anchors and aliases are allowed. Merge keys (`<<:`) and tags are
  refused, because they make "what does this file grant?" depend on
  YAML features a reviewer may not know. That includes the core tags
  (`!!str`, `!!int`): silo already reads each value by its key, so a
  tag can only confuse.
- The first key is `silo: v2`, the schema version. A file without it is
  refused.
- A JSON Schema for the Silofile is generated from silo's Go types and
  shipped in the binary (`silo schema`). Editors can use it, and
  `silo create` hands it to its agent.

Keys are `snake_case`.

### 2.2 The silo folder

A **silo folder** is a directory with a `Silofile` at its root and the
things it references beside it. Every relative path in a Silofile is
relative to the folder. A path may not leave the folder (`..` or a
symlink pointing out is refused). Absolute paths are allowed only where
the schema says so (an ssh identity file, an MCP server config source,
a `{file: ...}` credential source, all of which live on the invoking
machine outside the folder by nature); `~/` counts as absolute.

```text
my-silo/
├── Silofile              # required
├── prompts/              # prompt files: agent.prompt, agent.system_prompt
│   └── task.md
├── scripts/              # the local executor's scripts (journey 3)
│   ├── deploy-preview
│   └── tail-logs
├── mcp/                  # MCP server configs referenced by mcp[].from
│   └── servers.json      # standard .mcp.json shape
├── sandboxes/            # sandbox definitions
│   ├── tools.Dockerfile  # built into a container sandbox's image
│   └── dev.Granulefile   # a granule sandbox (journey 10, later)
└── .silo/                # silo's own, not hand-edited; git-ignorable
    ├── lock.json         # resolved digests and script hashes of the last run
    └── backup/           # silo create's copies before an edit (§10)
```

Only `Silofile` is required. The directory names above are conventions
`silo create` follows, not rules: the Silofile names every path it uses.
The one exception is `scripts/`, which is the default of
`local.scripts.dir` when `local:` is present without one.

A **bare Silofile** (`silo run ./ops.Silofile`) is a silo folder whose
folder is the file's directory, so `prompts/x.md` beside it still
resolves. `silo run <dir>` reads `<dir>/Silofile`.

### 2.3 The schema

What follows is the whole v2.0 schema in one place. Each section below
says what its parts do. Fields marked *later* are reserved now so that
journeys 5–10 add values, not new shapes.

```yaml
silo: v2                      # schema version, required
name: gcp-ops                 # [a-z0-9-], used in session ids, audit and SA descriptions
description: Read-only ops on the staging project.

agent:
  cli: claude                 # claude | codex | agy | custom
  binary: /opt/claude/bin/claude   # optional; otherwise found on PATH on the agent host
  model: claude-opus-5-5      # optional; the CLI's default otherwise
  effort: high                # optional; passed where the CLI has a flag for it
  auth: ambient               # ambient (the CLI's own login) | {credential: <name>}
  mode: interactive           # interactive | headless | attached
  prompt: prompts/task.md     # file or inline string; required for headless
  system_prompt: prompts/system.md   # optional, appended to the CLI's own
  workdir: ~/src/widgets      # optional, on the agent host; default: a private empty dir
  limits:                     # optional
    deadline: 2h              # wall clock, then the session is ended
    max_turns: 200            # where the CLI supports it
  custom:                     # only for cli: custom; see cli-mapping.md
    argv: {interactive: [...], headless: [...]}
    mcp_config: {format: claude-json, arg: ["--mcp-config", "{file}"]}
    permissions: {none: [...], all: []}

location:
  kind: local                 # local | ssh | k8s | vm (later) | ax (later) | capsule (later)
  ssh:                        # only for kind: ssh
    host: devbox              # a Host alias in ~/.ssh/config, or a hostname
    user: alice               # optional; ~/.ssh/config otherwise
    port: 22                  # optional
    identity: ~/.ssh/id_ed25519   # optional
    gateway_transport: tcp    # tcp (default) | unix (later); see §5.3
  k8s:                        # only for kind: k8s; see §5.4
    context: kind-dev         # optional; kubectl's current context otherwise
    namespace: silo           # optional; the context's namespace otherwise
    image: ghcr.io/acme/agent:claude-2.1   # needs /bin/sh and the CLI
    tunnel_image: ...         # optional; silo's published sidecar otherwise
    resources: {cpu: "2", memory: 4Gi}
    identity: {gcp_account_for: acme-staging}   # later

remote_permissions: none      # none (default) | all | [Read, Grep, Glob, Edit]

sandboxes:
  - name: build
    kind: container           # container | gvisor | granule (later)
    image: golang:1.25        # or build: sandboxes/tools.Dockerfile
    workdir: /work
    cpus: 2
    memory: 4Gi
    network: none             # none | open | {allow: [host, ...]} (later)
    mounts:                   # optional; read-only unless rw: true
      - {from: ., to: /silo, rw: false}
    repos: [widgets]          # git[] entries checked out into workdir through the git proxy
    env: {GOFLAGS: -mod=mod}
    backend: local            # local | {k8s: {...}} (later, journey 9)
    lifetime: session         # session | persistent (later, grain.md §2.3)
    package_cache: {url: ..., ca: ...}   # later, grain.md §2.3
    model_access: [anthropic] # later: the model proxy, grain.md §2.5
  # kind: granule (later) takes image: <granule VM image> or
  #   granulefile: sandboxes/dev.Granulefile, with target: and args:
  # kind: folder (built-in silos only) takes path: and rw:

mcp:
  - name: github
    where: local              # local (invoking machine) | remote (agent host)
    from: claude-user:github  # or command: [...] or url: ...
    tools: [get_issue, search_code]   # optional allowlist; default all
    env: {LOG_LEVEL: info}
    inherit_env: [HTTPS_PROXY]  # names passed through from silo's environment; or: all
    required: true            # default true
    restart: never            # never (default) | on-failure

credentials:
  - name: gh
    from: {ambient: gh}       # env | file | store | ambient
    to:
      - {mcp: github, env: GITHUB_TOKEN}
      - proxy: git

projects:                     # GCP (journey 2)
  - project: acme-staging
    roles: [roles/viewer, roles/logging.viewer]
    ttl: 7d                   # idle time before silo gc may delete the account; 0 = delete on exit
    allow_basic_roles: false  # roles/owner, roles/editor, roles/viewer need this
    deliver_to: [cloud]       # sandboxes that get the token
  # - project: p, account: <email>   # a caller-provided identity instead of roles (grain.md §2.6)
  # attach_to: vm                    # later, journey 5

git:                          # through the git proxy (stage 7)
  - name: widgets
    repo: github.com/acme/widgets
    access: push              # read | push
    push: [refs/heads/silo/*] # ref patterns a push may update
    force: false              # false (default): non-fast-forward pushes refused
    credential: gh

local:                        # the local executor (journey 3)
  scripts:
    dir: scripts              # every executable in it, snapshotted at start
    list:                     # and/or listed one by one
      - path: scripts/deploy-preview
        description: Deploy a preview build of a branch.
        approve: true
  workdir: .
  timeout: 5m
  output_cap: 1MiB
```

`agent.prompt` and `agent.system_prompt` take a file or inline text: a
single line with no whitespace is a path in the folder, and anything
else is the text itself.

Built-in silos (§10) may also use `builtin_tools:` and the `folder`
sandbox kind. A user Silofile that uses either is refused in v2.0.

### 2.4 Validation

`silo validate <Silofile|dir>` checks, without touching anything outside
the folder:

- the file decodes strictly against the schema version it declares;
- every path resolves inside the folder, and every referenced file
  exists;
- names are unique within their section, and every cross-reference
  resolves (`to: {sandbox: x}`, `sandboxes[].repos`, `git[].credential`,
  `projects[].deliver_to`);
- the agent's `remote_permissions` can be expressed by its `cli`, by the
  table in [`cli-mapping.md`](cli-mapping.md); `agy` with `none` fails
  here, not at run time;
- every credential has at least one destination, and none is delivered
  somewhere its source forbids (§8);
- scripts pass the static checks of §12.3, with warnings for the
  obvious holes.

`silo run --dry-run` does everything `silo validate` does, and then asks
the world, still changing nothing: binaries present (locally, and over
ssh on the agent host), the CLI's login present, ssh reachable with host
key accepted, MCP servers resolvable and their `from:` names found,
images pullable, and for each `projects[]` entry whether the invoking
user holds what setup needs (§11.2). It prints a checklist, one line per
check, and exits non-zero on any failure. `silo create`'s agent calls
the same code through its `silo_check` tool.

## 3. `silo run`: one session, start to finish

`silo run [--dry-run] [--json] [--prompt ...] <Silofile|dir>` is
one-shot: there is no daemon in v2.0.

1. **Load.** Parse and validate the Silofile. Resolve the folder.
   Create the session: a random id (`s-` + 12 base32 characters) and a
   private session directory, `$XDG_RUNTIME_DIR/silo/<id>/` (mode 0700),
   holding the audit log, the control socket (§3.2), the gateway's state
   and a `session.json` describing what was set up, so cleanup after a
   crash knows what to undo.
2. **Resolve credentials** from their sources (§8). A source that is
   missing fails the session here, before anything is created.
3. **Prepare the location** (§5): for `ssh`, open the master connection,
   check prerequisites, create the remote session directory, and set up
   the reverse forward.
4. **Start what the gateway fronts**, in parallel: sandboxes (§7), MCP
   servers silo launches (§13), the local executor's script snapshot
   (§12), the git proxy (§9), and the cloud identities of `projects[]`
   (§11). Each reports progress on the terminal.
5. **Start the gateway** (§4), listening on a loopback port with a
   per-session bearer token, and confirm every declared tool is listed.
6. **Write the CLI's configuration** on the agent host: an MCP config
   naming only the gateway, the CLI-specific settings that implement
   `remote_permissions`, and the prompt files.
7. **Start the CLI** in the chosen mode (§6), wired to the terminal for
   `interactive`, to silo's event stream for `headless`.
8. **Serve** until the CLI exits, the deadline passes, the user presses
   Ctrl-C, or the ssh connection drops.
9. **Tear down**, in reverse order, each step independent of the others
   so one failure does not leave the rest behind: stop the CLI (if
   still running), remove its config and prompt files from the agent
   host, stop the gateway, stop launched MCP servers and sandboxes, drop
   cloud tokens and record last-used times, close the ssh connection,
   and write the session's result. The audit log stays.
10. **Exit** with the CLI's exit code, or silo's own (§3.3) when silo
    ended the session.

### 3.1 Signals and crashes

- **First Ctrl-C (SIGINT)** in `interactive` mode belongs to the CLI:
  silo passes it through (locally the CLI shares the terminal's process
  group; over `ssh -t` the remote pty delivers it). In `headless` mode
  silo sends SIGINT to the CLI and starts a 10-second grace period.
- **Second Ctrl-C**, or SIGTERM to silo: silo stops the CLI (SIGTERM,
  then SIGKILL after 10 s) and tears down.
- **SIGKILL to silo or a host crash:** nothing runs. What is left
  behind is whatever step 9 would have removed. Every resource silo
  creates is labelled with the session id (containers carry
  `silo.session=<id>`; remote files live under the remote session
  directory; MCP server processes are in a process group recorded in
  `session.json`). **`silo gc`** reads stale session directories (whose
  `silo run` process is gone) and removes what they list, and finds
  labelled containers whose session is gone. `silo run` runs the cheap
  local part of `silo gc` at startup **(call 2)**. Cloud-side leftovers
  are covered in §11.6.
- **Child processes** (launched MCP servers, script runs, `ssh`) are
  started with `Pdeathsig` on Linux so they die with silo where the OS
  allows. On macOS, where there is none, they are reaped by `silo gc`.

### 3.2 Machine interfaces

`silo run --json` writes a JSON-lines event stream on stdout (transcript
items, tool calls, usage, status changes, the exit reason) and a final
`result` event. The schema, versioned, is [`events.md`](events.md). It is
shared with the Go library (`silo.Start`, `v2/silo.go`), whose
`Session.Events()` delivers the same values. As built in stage 6a,
`--json` needs `agent.mode: headless`, and the CLI's own output is kept
in the session's raw transcript rather than written to stdout. A control socket in the session directory accepts
`inject`, `cancel` and `extend_deadline`. Both exist for grain and are
designed in [`grain.md`](grain.md) §2.1; journeys 1–4 do not need them
(apart from answering a script's `approve: true` under `--json`, §12.3).
`--caller-mcp <name>=<url>` adds a caller-provided MCP server
([`grain.md`](grain.md) §2.2). `--detach` runs the session under a
supervisor of its own and prints its id; `silo attach <id> --from <seq>`
replays and follows its numbered `events.jsonl`, and `silo ls` lists the
running sessions by label ([`grain.md`](grain.md) §2.7, as built in
stage 9).

As built in stage 6b, the socket's methods (`inject`, `cancel`,
`extend_deadline`, `approve`) and their params are documented in
[`events.md`](events.md). The library's `Session` methods share the same
implementation. A caller server's token is read from
`$SILO_CALLER_MCP_TOKEN_<NAME>` rather than the command line, so it
never shows in `ps`. `--caller-mcp-tools` and `--caller-mcp-blocking`
qualify a `--caller-mcp` server.

### 3.3 Exit codes

`silo run` exits with the CLI's own exit code when the CLI ran to its
end. Codes silo itself owns are 64 and above, chosen from the BSD
`sysexits` range so they do not collide with the CLIs' usual 0/1/2
**(call 3)**:

| Code | Meaning |
| --- | --- |
| 64 | Usage: bad flags. |
| 65 | The Silofile is invalid. |
| 69 | A prerequisite is missing (binary, login, permission, image). |
| 75 | The agent host connection dropped (ssh). |
| 76 | Refused: a setting the CLI cannot express (`agy` with `none`). |
| 124 | The session deadline passed (the `timeout(1)` convention). |
| 130 | Ended by Ctrl-C (128 + SIGINT). |
| 143 | Ended by SIGTERM to silo (128 + SIGTERM). |

As built in stage 1c: any other failure during setup (a sandbox that
does not start, an image that does not pull, a missing credential
source) exits 69. In headless mode, a session whose first Ctrl-C the
CLI obeyed still exits 130, since silo sent the signal. The session
directory holds `session.json` and the staged CLI files (`agent/`) and
the private workdir (`work/`); the audit log is written straight to
`$XDG_STATE_HOME/silo/sessions/<id>/audit.jsonl`, so a crash keeps it,
and the final `session.json` is copied beside it. As built in stage 9,
the event stream is kept there too, as `events.jsonl`. Where
`XDG_RUNTIME_DIR` is unset, session directories go in
`$TMPDIR/silo-<uid>/`. Containers also carry `silo.uid=<uid>`, so
`silo gc` on a shared Docker daemon removes only the invoking user's.

## 4. The gateway

One gateway per session. It is an MCP server (Streamable HTTP) inside
the `silo run` process, and the CLI's MCP configuration names it and
nothing else.

### 4.1 What it serves

| Tool family | Name | Present when |
| --- | --- | --- |
| Sandbox | `sandbox_exec`, `sandbox_read_file`, `sandbox_write_file`, `sandbox_list_dir` | `sandboxes[]` is not empty. Each takes a `sandbox` argument whose JSON Schema `enum` is the declared names. |
| Local scripts | `local_<script>` | `local:` is present. One tool per script (§12). |
| Declared MCP servers | `<server>__<tool>` | `mcp[]`: each server's allowed tools, renamed with the server's name as prefix. |
| Cloud | `gcloud`, `gsutil`, `bq` | `projects[]` is present and a sandbox receives its token (§11). They are `sandbox_exec` with the program fixed. |
| Built-in, journey 1 only | `silo_schema`, `silo_validate`, `silo_check`, `silo_docs`, `silo_examples`, `workspace_*` | The built-in `silo create` silo (§10). |
| Caller's MCP servers | `<server>__<tool>` | Given on the command line or by a library caller (grain, [`grain.md`](grain.md) §2.2). |

`sandbox_exec` takes `command` as a string run by `/bin/sh -c` **inside
the sandbox**, where a shell is harmless because the sandbox is the
boundary, plus `cwd`, `env`, `timeout` and `stdin`. That differs on
purpose from the local executor, which never uses a shell (§12).

### 4.2 Refusal and audit

- A call to a tool the gateway did not list is refused with an MCP error
  naming the reason. So is a call whose `sandbox` names no declared
  sandbox, and a call to an MCP server tool outside its allowlist.
- **Every call is audited**, refused ones included: one JSON line in
  `audit.jsonl` in the session directory with the session id, the
  caller (the CLI type and agent host), the tool, its arguments
  (secrets redacted, §8.4), the outcome, the result's size and first
  bytes, and the duration. A call's start is written before it runs, so
  a crash mid-call still leaves a record.
- The audit log is kept after the session ends, under
  `$XDG_STATE_HOME/silo/sessions/<id>/` **(call 4)**. `silo audit <id>`
  prints it. `silo gc --audit-older-than 30d` prunes old ones.

### 4.3 How the CLI reaches it

The gateway listens on `127.0.0.1` on a random port and requires
`Authorization: Bearer <token>`, a 256-bit random token per session. It
refuses an unexpected `Host` header and any `Origin` header, so a
browser on the same machine cannot reach it (DNS rebinding). This is
grain's design for its Codex endpoint (grain `docs/codex-isolation.md`),
reused as is.

- **`local`:** the MCP config names `http://127.0.0.1:<port>/mcp` with
  the token in a header (for Codex too: `http_headers`, as grain writes
  it, in the private `CODEX_HOME`, cli-mapping.md §2.2).
- **`ssh`:** silo asks the ssh master connection for a reverse forward
  from a remote loopback port to the gateway, and the remote MCP config
  names that remote port (§5.3).

All three CLIs accept an HTTP MCP server with a bearer header, so one
transport serves every CLI and location **(call 5)**. A stdio bridge
(`silo mcp-bridge`) is kept as a fallback for a `custom` CLI that only
speaks stdio.

The token is not a secret *from the agent*: the agent may well be able
to read its own MCP config. It does not need to be. The gateway is the
boundary: an agent that bypasses its CLI and calls the gateway directly
gets exactly the tools the CLI would have. The token keeps *other* local
users and processes on the agent host out.

### 4.4 Gateway timeouts

Each tool has a timeout (sandbox and script calls: their own `timeout`
argument, capped by the Silofile; MCP proxied calls: 10 minutes by
default). CLIs have their own MCP call timeouts, which silo raises where
it can so the CLI does not give up first (Claude Code: the
`MCP_TOOL_TIMEOUT` environment variable, which grain already sets for
the same reason).

As built in stage 6b, a caller's tool marked blocking ([`grain.md`](grain.md)
§2.2) has no gateway timeout at all. In a session with one, the CLI's
own timeout is raised from 32 minutes to 7 days: `MCP_TOOL_TIMEOUT` for
claude, `tool_timeout_sec` for codex. Raising it means the CLI never
abandons a call that waits on a person (§6.2). The session's deadline
still bounds such a call.

As built in stage 1b: `sandbox_exec`'s `timeout` is in seconds, default
5 minutes. The Silofile has no field to cap it yet, so the cap is a
fixed 30 minutes until one is added. The file tools are bounded at 2
minutes, and any other tool at 10. A call whose start line cannot be
written to the audit log is not run.

## 5. Locations

### 5.1 The interface

A location is where the agent CLI runs. Each kind implements one Go
interface. It is shaped after grain's `agent.Host` (stage files, start
one process), plus the two things grain's needs did not include: a path
back to the gateway, and owning a process silo does not start itself
(the hosted runtimes of journey 6).

```go
// Location is where one session's agent CLI runs.
type Location interface {
	// Prepare checks prerequisites and readies the host. It returns the
	// facts the CLI mapping needs (OS, arch, the CLI binary's path and
	// version, the user's home) or a Prerequisite error naming what is
	// missing and how to fix it.
	Prepare(ctx context.Context, s *Session) (HostInfo, error)

	// Gateway makes the gateway reachable from the agent host and returns
	// the URL the CLI's MCP config must name. local: the loopback URL.
	// ssh: a reverse forward. k8s: a port-forward or sidecar. ax: a tunnel.
	Gateway(ctx context.Context, local net.Addr) (url string, err error)

	// Stage writes files the CLI needs (MCP config, settings, prompts)
	// into a session directory on the agent host and returns their paths.
	Stage(ctx context.Context, files []StagedFile) (dir string, err error)

	// Start starts the CLI. For local and ssh silo starts the process.
	// For a hosted runtime the runtime starts it; Start submits the
	// session and returns a handle to it.
	Start(ctx context.Context, cmd Command, io IO) (Agent, error)

	// Teardown removes everything Prepare and Stage made. It must succeed
	// against a host that is half set up, or already gone.
	Teardown(ctx context.Context) error
}

// Agent is the running CLI, however it was started.
type Agent interface {
	Wait() (ExitStatus, error)
	Signal(sig Signal) error   // interrupt, terminate, kill
	Resize(rows, cols int) error // interactive only
}
```

`Command` carries argv, env, workdir and whether it wants a TTY. `IO` is
either a terminal (interactive) or pipes (headless). A hosted runtime
whose CLI cannot be given a TTY refuses `interactive` in `Prepare`.

| Kind | v2.0? | Prepare | Gateway | Start |
| --- | --- | --- | --- | --- |
| `local` | yes | look up binary | loopback URL | `exec` with the terminal or pipes |
| `ssh` | yes | ssh master; remote checks | `ssh -R` | `ssh -t` (interactive) or `ssh -T` |
| `vm` | later (journey 5) | create or reuse VM, then as `ssh` (or IAP) | as `ssh` | as `ssh` |
| `k8s` | yes (stage 11a, journey 8) | create the pod and Secret, wait ready, probe | tunnel sidecar, reached by `kubectl port-forward` | `kubectl exec -i -t` (interactive) or `kubectl exec -i` |
| `ax`, `capsule` | later (journey 6) | submit or attach a hosted session | outbound tunnel from the runtime | runtime-started; handle only |

Sandboxes are not part of the location in v2.0: they run on the
invoking machine for `local`, `ssh` and `k8s` (§7.3).

### 5.2 `local`

The CLI is a child of `silo run`. It gets a scrubbed environment
**(call 6)**: `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`,
`LANG`/`LC_*`, `TMPDIR`, `XDG_*`, plus what the CLI's auth needs (for
`auth: ambient` the CLI reads its login from `HOME`; for
`auth: {credential: x}` silo sets the CLI's API-key variable) and any
credential delivered `to: {agent: ...}`. Nothing else of the user's
environment is passed, so a stray `AWS_SECRET_ACCESS_KEY` in the shell
does not reach the agent. `remote_permissions: all` changes nothing
here. The CLI's tools can still read the user's files, and that is what
`all` means.

### 5.3 `ssh`

silo, the gateway, credentials, sandboxes and the local executor stay on
the invoking machine. Only the CLI runs on the remote host.

**Configuration.** `location.ssh.host` is passed to the system `ssh`
binary, so `~/.ssh/config` applies in full (aliases, `ProxyJump`,
identity files, certificates, `ControlPath` options). The Silofile's
`user`, `port` and `identity` are optional overrides, passed as `-l`,
`-p` and `-i`. silo uses OpenSSH rather than a Go ssh client **(call
7)** so the user's agent, jump hosts, hardware keys and known-hosts file
behave exactly as at their own prompt.

**Host keys.** silo always passes `-o StrictHostKeyChecking=yes` unless
the user's own config is stricter. An unknown host fails the session
with the `ssh-keyscan`/`ssh` command to accept it. silo never adds a
key itself.

**The connection.** One master connection per session:

```sh
ssh -M -S $SESSION/ssh.ctl -o ControlPersist=no -o ServerAliveInterval=15 \
    -o ServerAliveCountMax=4 -o ExitOnForwardFailure=yes -fN <host>
```

Every later step multiplexes over it (`ssh -S $SESSION/ssh.ctl ...`),
so the user authenticates once and the connection's death is one event
silo can watch. The keep-alive settings mean a dead network is noticed
in about a minute.

**Prerequisites.** Over the master, silo runs one probe script (sent on
stdin to `sh -s`, so nothing is installed): `uname -sm`, `$HOME`,
`command -v <cli>` (or test `agent.binary`), `<cli> --version`, and the
CLI's own login check (`claude auth status`, `codex login status`; for
agy, the presence of its credential file, see
[`cli-mapping.md`](cli-mapping.md)). A missing binary or login fails
with the command to run on the remote host. silo never installs a CLI
or logs it in **(call 8)**. With `agent.binary` set, that absolute path
is used; otherwise the binary is whatever `command -v` finds in the
remote login shell's `PATH`.

**The path back.** The gateway is reached through one reverse forward,
asked of the master connection:

```sh
ssh -S $SESSION/ssh.ctl -O forward -R 127.0.0.1:0:127.0.0.1:<gateway-port> <host>
```

With port 0, OpenSSH has the server allocate a free port and prints it
on stdout. The forward binds the remote host's **loopback** only. A
server with `GatewayPorts yes` overrides the client's bind address and
binds the wildcard instead (measured against OpenSSH 9.2's sshd; `no`,
the default, and `clientspecified` keep `127.0.0.1`), so silo checks
the listening address after asking for the forward and refuses the
session, with the `sshd_config` fix, if it is not loopback. No port is
opened to the network on either side. Other users
on the remote host can connect to the loopback port. The bearer token
(§4.3) is what turns them away.

`gateway_transport: unix` is reserved for a later hardening: forward to
a remote Unix socket in the remote session directory
(`-R $RSESSION/gw.sock:...`, with `StreamLocalBindMask=0177`), so other
users cannot even connect. No CLI speaks HTTP over a Unix socket, so it
needs a stdio bridge on the remote, which means getting a binary there.
That is why it is not the default.

**Staging.** silo creates a remote session directory with
`mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/silo.XXXXXXXX"` (mode 0700) and
writes the MCP config, CLI settings and prompt files into it, each with
`cat > file` over the master (mode 0600, via `umask 077`). The CLI is
pointed at them by flag or by environment (`CODEX_HOME`, `HOME` for
agy). Teardown removes the directory. A crash leaves it, and the next
`silo gc` (or the next `silo run` to that host) removes stale
`silo.*` directories whose session is gone, by checking the session id
recorded inside each against the local session directories.

**Starting the CLI.** Interactive: `ssh -S ... -t <host> -- <argv>`,
with silo's own terminal in raw mode passed straight through, and
window-size changes carried by ssh. Headless: `ssh -S ... -T <host> --
<argv>` with stdin, stdout and stderr as pipes. The argv is quoted for
the remote shell with POSIX single-quoting. The remote environment is
the remote login environment: the agent **inherits the remote host's
ambient credentials and models** by design (journey 3), plus anything
the Silofile delivers `to: {agent: ...}`, which silo writes to a 0600
env file in the remote session directory and sources then deletes in
the command's own preamble, so no secret appears in a remote argv.

**Exit codes and signals.** `ssh` returns the remote command's exit
status, which silo passes on. It returns 255 for its own errors; silo
maps 255 to its code 75 (connection lost) after checking the master is
gone. Ctrl-C in interactive mode reaches the remote CLI through the
remote pty. In headless mode silo sends a signal by closing the CLI's
stdin and, after the grace period, by tearing down the session channel,
which makes `sshd` send SIGHUP to the remote process group.

**A dropped connection ends the session on both sides.** On the
invoking side the master exits, silo sees it, and tears down as in §3
with exit code 75. On the remote side, `sshd` hangs up the CLI's
session, and the CLI loses its gateway at the same moment because the
reverse forward lived on the master connection. A CLI that survives
SIGHUP finds every gateway call failing and has nothing left to do. silo
cannot do better than that without an agent on the remote host, and v2.0
deliberately has none. There is no reconnect in v2.0. The detach and
re-attach of [`grain.md`](grain.md) §2.7 (stage 9) covers silo's
caller, not this link: a detached session whose ssh connection drops
still ends with exit 75.

**As built in stage 4b** (`location/ssh`):

- The master is started without `-f`: silo keeps it as its own child,
  under `Pdeathsig`, and waits for `ssh -O check` to succeed. With `-f`
  the backgrounded master would outlive a killed silo, and its exit
  would not be an event silo can watch. A passphrase prompt still
  reaches the user through `/dev/tty`. Every later step is
  `ssh -S <ctl> -o ControlMaster=no -o BatchMode=yes ...`, and the
  control socket moves to a short temporary directory when
  `$SESSION/ssh.ctl` would not fit in a socket address.
- The remote session directory holds `session`
  (`id=<session> origin=<hostname>:<uid>`), the staged files under
  `agent/`, the private workdir `work/`, the env file while it lives,
  and `pid`. `silo gc --host` (and `silo run` to the host, at startup)
  removes only directories whose origin is this machine and user and
  whose session is not live here, so it never removes a live session
  that another machine is running against the same remote account.
- The command is `exec /bin/sh -c '<preamble>; exec <argv>'`, so the
  CLI keeps the pid of the session `sshd` made, which leads its process
  group. **Headless signals** are sent as real signals to that group
  over the master (`kill -s INT -- -<pid>`). Closing stdin is not
  enough, because stdin is already closed once the prompt is written.
  Tearing down a channel without a pty does not make `sshd` send SIGHUP.
  A kill also ends the local `ssh`, which closes the channel, and so
  does any signal once the master is gone.
- The gateway also answers to the forward's own `Host`
  (`127.0.0.1:<remote port>`), still loopback, so its DNS-rebinding
  check stands.
- `--dry-run` adds four checks for `kind: ssh`: reachable
  (`ssh -o BatchMode=yes <host> true`), host key accepted, the CLI
  binary and its version, and the CLI's login when `auth` is ambient.
  The login checks are `claude auth status`, `codex login status`, and
  for agy the presence of `~/.gemini/oauth_creds.json`.
- `gateway_transport: unix` is refused, by `silo validate` and again by
  the location.

### 5.4 `k8s`

Journey 8. As for `ssh`, silo, the gateway, credentials, sandboxes and
the local executor stay on the invoking machine; only the CLI runs in
the cluster, in one pod per session made from `location.k8s.image`.

**Configuration.** silo drives the cluster with the system `kubectl`
(`--context` and `--namespace` from the Silofile, both optional), for
the reason it uses OpenSSH **(call 7)**: the user's kubeconfig, exec
credential plugins such as `gke-gcloud-auth-plugin`, and proxies behave
exactly as at their own prompt, and silo takes on no client library.
The image needs `/bin/sh` and the CLI on its `PATH` (or at
`agent.binary`); silo never installs a CLI.

**The pod** is `silo-<session id>`, labelled
`app.kubernetes.io/managed-by: silo` and `silo.dev/session: <id>`, and
annotated `silo.dev/origin: <hostname>:<uid>` (an annotation, because a
label value cannot hold the colon). It has `restartPolicy: Never`, no
service account token (`automountServiceAccountToken: false`, so the
agent has no credentials for the cluster), a 5 s termination grace, an
`emptyDir` at `/silo` (staged files under `agent/`, the private workdir
`work/`, the env file while it lives, and `pid`), and two containers:

- `agent`, the image, whose command holds
  (`while :; do sleep 3600 & wait; done`, ending at once on SIGTERM)
  until silo execs the CLI into it, with `resources` as both requests
  and limits;
- `tunnel`, the gateway sidecar (below), non-root, with a read-only
  root filesystem and no capabilities.

`Prepare` removes this origin's stale pods in the namespace (as
`silo gc --k8s` does), creates the pod, then the Secret, waits for the
pod to be ready (failing at once on `ErrImagePull`,
`ImagePullBackOff` and the like, naming the image), and runs §5.3's
probe in it with `kubectl exec -i ... sh -s`. `Teardown` deletes the
pod and Secret by label, so that a pod whose create call failed half
way goes too, without waiting for the pod to finish terminating.
`session.json` records the context and namespace before the pod is
made, so `silo gc` deletes the pod of a session that crashed.

**The CLI is started with `kubectl exec` in both modes**: `-i -t` for
interactive (kubectl puts the terminal in raw mode and carries window
size changes) and `-i` for headless, with the same
`cd; . env; rm env; echo $$ > pid; exec <argv>` preamble as `ssh`. This
differs from journey 8's sketch, which ran a headless CLI as the
container's command and read its logs: the pod has to exist for
`Prepare` to probe the CLI and for `Stage` to write its files, before
the command is known, and `exec` gives headless mode the same stdin
(the prompt, injected messages), exit code and signals as `ssh`, with
no second code path. kubectl passes the CLI's exit code on; a failure
with the pod no longer running is exit 75. Signals go as real signals
to the pid file's process group with another `exec`; a kill also ends
the local `kubectl`.

**The gateway** is a tunnel, because Kubernetes has no reverse
port-forward. The sidecar (`cmd/silo-tunnel`) listens twice on the
pod's loopback: on `127.0.0.1:7811`, the URL the CLI's MCP config names
(`http://127.0.0.1:7811/mcp`), and on `127.0.0.1:7812` for silo, which
reaches it with `kubectl port-forward pod/<pod> :7812`. Silo's
connection opens with a hello line and then carries a stream mux
(`internal/tunnel/mux`, copied from grain's `pkg/tunnel/mux`); each
connection the CLI makes to 7811 becomes a stream, which silo splices
to the gateway on the invoking machine. If the port-forward or the
connection drops, silo opens them again, and a newer connection
replaces the old one at the sidecar. Nothing in the cluster can reach
either port, since both are bound to the pod's loopback, and the
gateway's bearer token still guards every call.

**The sidecar's image** is built from `cmd/silo-tunnel/Dockerfile`
(from `v2/`) and published as `ghcr.io/bwsalmon/silo-tunnel:v2` by a
maintainer with `docker buildx build --push`, as the v2 README says;
`location.k8s.tunnel_image` names another (a mirror, or a private
registry the cluster can pull from), and `--dry-run` checks that it can
be pulled.

**Credentials.** Every variable of the CLI's environment that carries
a credential (`agent.auth: {credential: ...}`, and credentials
delivered `to: {agent: ...}`) goes into a Secret, `silo-<session id>`,
labelled as the pod is and owned by it (so the cluster deletes it with
the pod even when silo cannot). It is mounted read-only into the
`agent` container only, at `/run/silo/secret`, one file per variable,
and the preamble reads each into the CLI's environment, so no value is
in the pod's spec or any argv. The other variables go in the env file,
written over `exec`'s stdin with `umask 077`. **Workload identity**
(`identity: {gcp_account_for: ...}`, the pod's Kubernetes service
account bound to journey 2's per-user GCP account) is later:
`silo validate` refuses `identity:` naming this section.

**`--dry-run`** adds, for `kind: k8s`: the context is reachable
(`kubectl version`), the namespace exists, silo may create and delete
pods and Secrets and use `pods/exec` and `pods/portforward` there
(`kubectl auth can-i`), and both images can be pulled and the CLI is in
the agent's. Only the cluster's nodes can say whether they can pull an
image, so this last check runs a short-lived pod (labelled
`silo.dev/check`, deleted at once, and removed by `silo gc --k8s` if it
is left behind) in which the agent image runs the probe and the tunnel
image its `-check`. It is the one thing `--dry-run` makes.

**`silo gc --k8s [--context c] [--namespace ns]`** deletes the pods
and Secrets in the namespace whose origin is this machine and user and
whose session is not live here, and check pods older than ten minutes.

## 6. Remote permissions and CLI modes

### 6.1 Remote permissions

`remote_permissions` says what the CLI's **own built-in tools** may do
on the agent host. Silo's gateway is available whatever it says.

- **`none`** (the default): every built-in tool is off. The gateway is
  the agent's only capability.
- **`all`**: the built-ins are on as the CLI ships them. On `ssh`, this
  is "the agent may do anything the remote user can".
- **A list** of the CLI's own tool names, such as `[Read, Grep, Glob,
  Edit]`, in that CLI's own spelling. silo does not invent a portable
  vocabulary of tool names **(call 9)**. The names are the CLI's, and
  the list is checked against the CLI's real roster when it is known.

Where a CLI cannot express a setting exactly, silo refuses to start (exit
76) and says why. In summary (the full mapping, flags and versions are
in [`cli-mapping.md`](cli-mapping.md)):

| | `none` | `all` | subset |
| --- | --- | --- | --- |
| `claude` | `--tools ""` | default roster | `--tools <list>` |
| `codex` | private `CODEX_HOME` with no execution environment (grain's reviewed config) | default | **refused**: Codex has no per-tool allowlist |
| `agy` | **refused**: agy has no switch that withholds its native tools (grain found this, agy 1.1.26–1.2.2) | default | **refused** |
| `custom` | the Silofile's `custom.permissions.none` args, or refused | `custom.permissions.all`, or refused | refused |

Two things follow and are said here, not left for a user to discover:

- **agy runs only with `remote_permissions: all` in v2.0.** That is
  safe on a host the agent may own anyway (journey 3's remote host,
  journey 5's VM), and it is wrong for journeys 1, 2 and 7. `silo create`
  and the Google project journey therefore do not offer agy.
- **Codex's `none` depends on its version.** Grain reviewed its
  no-environment configuration against `codex-cli 0.153.0` and refuses
  other versions. silo keeps the same rule: a table of reviewed Codex
  versions for `none`, a refusal for any other, and a reviewed-version
  bump as a deliberate change. 0.162.1, current while writing this,
  was added in stage 1d on a behavioural probe ([`cli-mapping.md`](cli-mapping.md)
  §2.2).

Every mapping also takes the CLI's *other* ways of reaching the host off
the table for `none` and subsets: MCP servers from the user's own config
(Claude's `--strict-mcp-config`; a private `CODEX_HOME`; a private agy
`HOME`), project instructions and settings from the working directory
(the CLI starts in an empty private directory unless `agent.workdir` is
set), hooks, plugins and skills that run host commands. A hook is a
built-in tool in all but name.

### 6.2 CLI modes

| Mode | What it is | `claude` | `codex` | `agy` |
| --- | --- | --- | --- | --- |
| `interactive` | A person typing live in the terminal. | `claude [prompt]` | `codex [PROMPT]` | `agy` / `agy -i <prompt>` |
| `headless` | One prompt to completion; events machine-readable. | `claude -p --output-format stream-json --verbose` (`--input-format stream-json` for injection) | `codex exec --json` | `agy -p --output-format stream-json --input-format stream-json` |
| `attached` | The CLI runs at the location but is driven through its own remote-control channel. | `--remote-control [name]` | `codex remote-control` / `app-server` (experimental) | `agy remote-control` |

`attached` is reserved and its flags are recorded, but v2.0 does not
ship it: all three implementations are new or experimental, and none of
their semantics were exercised for this document (see
[`cli-mapping.md`](cli-mapping.md) §3). Every location must support
`interactive` and `headless`, except a hosted runtime that cannot give a
TTY, which refuses `interactive`.

As built in stage 6b, a headless claude's stdin stays open until its
turn's `result`, so that an injected message can be written to it. Codex
takes an injected message as a further turn, `codex exec resume <thread>
-`, and so does claude after its turn has ended (`claude -p --resume`).
Both are described in [`events.md`](events.md).

## 7. Sandboxes

A sandbox is a named place where commands run. The agent picks one per
call (`sandbox_exec {sandbox: build, command: ...}`).

### 7.1 Kinds

| Kind | v2.0? | What it is |
| --- | --- | --- |
| `container` | yes | An OCI container run by the local Docker or Podman, rootless where available. `image:`, or `build:` a Dockerfile in the folder. |
| `gvisor` | yes | A `container` run with the `runsc` runtime. Refused at `--dry-run` if the runtime is not installed. Same fields. |
| `granule` | later (stage 8) | A granule microVM: `image:` (a granule VM image) or `granulefile:` (journeys 7, 10). |
| `folder` | yes, built-in only | A directory on the invoking machine served by file tools only, path-confined, with no exec. `silo create` uses it (§10). Not offered in user Silofiles in v2.0 **(call 10)**. |

v2.0 ships `container` and `gvisor` **(call 11)**. They need nothing
silo does not already need for journey 2 (a container runtime), and
`gvisor` is one flag more. Granule waits for stage 8 because it needs
granule's own driver, and the deployment notes say granule's Go module
cannot yet be fetched by CI.

### 7.2 Fields

`image` or `build`; `workdir`; `cpus`, `memory`, `disk` (applied where
the kind supports them); `network`: `none` (the default) or `open`,
with an `allow:` list reserved for the egress proxy grain needs
([`grain.md`](grain.md) §2.3); `mounts` from the folder (read-only by
default); `repos`, the `git[]` entries checked out into `workdir`
through the git proxy; `env`; and `lifetime: session` (the default) or
`persistent`, reserved for grain's persistent workspaces.

A sandbox starts with no host files: nothing of `$HOME`, no Docker
socket, no host network. Every mount is declared.

### 7.3 Where sandboxes run

In v2.0, sandboxes run **on the invoking machine** for both `local` and
`ssh`, next to the gateway that serves them. A remote agent's
`sandbox_exec` crosses the reverse tunnel to the invoking machine and
runs there. That keeps the ssh location simple (nothing to install or
start on the remote), and it keeps credentials delivered to sandboxes on
the machine silo trusts.

The sandbox backend is an interface, so that a later backend can run
sandboxes elsewhere: on the remote host over the same ssh connection, in
a VM (journey 5) or on Kubernetes (journey 9):

```go
type Backend interface {
	Create(ctx context.Context, spec SandboxSpec) (Sandbox, error)
	// Find lists sandboxes carrying a session label, for silo gc and
	// re-attach.
	Find(ctx context.Context, session string) ([]Sandbox, error)
}

type Sandbox interface {
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
	ReadFile(ctx context.Context, path string, limit int64) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error
	// DeliverFile places a credential file and replaces it atomically
	// on refresh (the GCP token of §11).
	DeliverFile(ctx context.Context, path string, data []byte) error
	Destroy(ctx context.Context) error
}
```

A sandbox entry may name a backend (`backend: local` by default,
`backend: {k8s: {context: ..., namespace: ...}}` later).

As built in stage 1b (`sandbox/container`): the runtime is
`SILO_CONTAINER_RUNTIME` if set, else Podman (rootless as an ordinary
user) if installed, else Docker. A sandbox runs with
`--security-opt no-new-privileges`, `--network none` unless `open` (the
runtime's default network, never the host's), and its credential
variables passed by name so values never appear on a command line. The
file tools and `DeliverFile` run through `exec` with the image's own
`sh`, `cat`, `head`, `ls` and `mv`, not the runtime's copy API, because
gVisor keeps a container's writes in its own overlay where the runtime
cannot see them. An image therefore needs those, as any image with
`/bin/sh` has. A cancelled `Exec` kills the command inside the sandbox,
not only the runtime CLI. Each sandbox's image is resolved to a digest
(a repository digest when pulled, the image id when built) and recorded
under `images` in `.silo/lock.json`; stage 3a adds the grants summary
beside it.

## 8. Credentials

### 8.1 Sources

| `from:` | Meaning |
| --- | --- |
| `{env: NAME}` | The invoking environment's variable, read once at start. |
| `{file: path}` | A file on the invoking machine, read at start. The file must not be group- or world-readable. |
| `{store: name}` | silo's secret store: the OS keyring (Secret Service on Linux, Keychain on macOS) under the service `silo`. `silo secret set <name>` writes one **(call 12)**. |
| `{ambient: gh}` | `gh auth token --hostname <host>`, asked when needed and cached for 5 minutes (grain's `ambient.go`). |
| `{ambient: git-credential}` | `git credential fill` for a host. |
| `{ambient: gcloud}` | The user's gcloud login. Usable **only by silo itself**, to set up journey 2's accounts and mint their tokens. silo refuses to deliver it anywhere. |

### 8.2 Destinations

A credential is delivered only where its `to:` list says:

| `to:` | Effect |
| --- | --- |
| `{sandbox: x, env: NAME}` | Set in that sandbox's environment. |
| `{sandbox: x, file: /path}` | Written into that sandbox, mode 0400, replaced atomically on refresh. |
| `{mcp: x, env: NAME}` | Set in the environment of an MCP server silo launches (§13.3). |
| `{local: script, env: NAME}` | Set in the environment of one local-executor script when it runs (§12.3). |
| `{agent: env, name: NAME}` | Set in the agent CLI's own environment. The agent can read it. silo prints a warning at start for every credential delivered here. |
| `{proxy: git}` | Held by the git proxy and used upstream. Never visible to the agent. |
| `{proxy: model}` | Held by the model proxy ([`grain.md`](grain.md) §2.5). Never visible to the agent. |

A credential with no destination is a validation error, so that a
declared but unused secret is noticed.

`agent.auth: {credential: x}` is the one implicit destination: the
named credential becomes the CLI's own API key (`ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`, `GEMINI_API_KEY`), which the agent can read.

### 8.3 Ambient by accident

Nothing reaches the agent, a sandbox or a launched MCP server by
inheritance. The agent CLI's environment is scrubbed (§5.2). A sandbox
starts from its image's environment. A launched MCP server starts from a
minimal environment (§13.3). The exception is deliberate and named:
on `ssh`, the agent inherits the remote host's login environment, which
is journey 3's point.

### 8.4 Redaction

Every resolved credential value is registered with the audit writer and
the event stream, which replace any occurrence of it with
`[credential:<name>]` before writing. That is best-effort: a secret
transformed by a command (base64, split across lines) is not caught.

As built in stage 1b: values shorter than 4 bytes are not redacted
(they would rewrite ordinary text all over the log while hiding nothing
a guess would not find). The ambient `gh` and `git-credential` sources
ask for `github.com` until the git proxy (stage 7) chooses the host per
`git[]` entry. A `{file:}` source drops one trailing newline.
`{store:}` is refused as not yet supported.

## 9. The git proxy

The agent never holds a git credential. A sandbox's repositories are
cloned and pushed through a per-session git proxy on the invoking
machine, which holds the credential, checks each request against the
Silofile's `git:` entries, and audits it. It is the same idea as grain's
`pkg/gitproxy`, and v2 builds on that code rather than writing a new
one.

### 9.1 What the proxy enforces

- **Per-repo allowlist.** Each `git[]` entry names one repository and
  `access: read | push`. A request for any other repository is refused
  with a message naming the Silofile.
- **Ref policy.** A push's ref updates are read from the receive-pack
  command list *before* forwarding, and every updated ref must match the
  entry's `push:` patterns. Deleting a ref and updating a tag are each
  refused unless a pattern names them. A body whose command list cannot
  be read in full is refused (fail closed). In `v2/gitproxy` a pattern
  is a `RefRule` (grain's `model.RefPush`): `refs/heads/...` or
  `refs/tags/...`, so only a `refs/tags/` pattern names a tag, with
  `force` and `delete` flags, so only a rule with `delete` names a
  deletion. How a Silofile's `push:` strings ask for `delete` is
  stage 7b's to settle.
- **Fast-forward proof**, optional per entry (`force: false`, the
  default): a non-fast-forward update is refused, proved from the pushed
  pack's commit graph and the upstream's history, as grain does.
- **Credentials** come from `credentials[]` via `{proxy: git}` and the
  entry's `credential:`, chosen per host. `{ambient: gh}` and
  `{ambient: git-credential}` give grain #3719's ambient credentials.
- **Audit:** one record per request (repository, action, refs, outcome,
  credential name), written to the session's audit log, so the caller
  reads it from the same place.

### 9.2 What to copy, adapt and drop from grain's `pkg/gitproxy`

| grain file | Fate | Why |
| --- | --- | --- |
| `protocol.go`, `pack.go`, `ancestry.go`, `spool.go`, `forward.go` (the HTTP forwarding, `ValidateForwardTarget`, no redirects), `refupdates.go`'s receive-pack parser (`ParseReceivePackCommands`, `decodedPush`) | **Copy** | Protocol mechanics with no grain dependency apart from small types. These are the hard, tested parts. |
| `core.go` (`GitProxy.HandleRequest`) | **Adapt** | Keep the request pipeline (body limits, spool, ref check, forward). Replace grain's lookups by one `Policy` built from the Silofile. |
| `authorize.go` (`ModelAuthorizer`), `refupdates.go`'s `AuthorizeRefUpdates`, `RefGrant`, `declaredUpdate` | **Adapt** | Grain's authorizer reads `model.Store` (task branches, team grants, declarations). silo's reads a static per-session policy: repos, access, ref patterns, force (`StaticPolicy`). Grain's "only the run's own branch" becomes a `push:` pattern the caller writes. `declaredUpdate` and grain's `model.RefPushSet` become `RefRules.Check`, in `v2/gitproxy`, so grain's own `Policy` checks its branch and declared refs with the same code. |
| `ambient.go` | **Adapt**, in `v2/credentials` | Stage 1b already adapted the ambient sources (`gh`, `git-credential`) into `credentials.Resolver`, which caches them. The proxy does not copy them a second time: a `StaticPolicy` credential is a `CredentialSource` whose `Token` func asks for the secret on every request, and an upstream 401 calls `CredentialRefused` so a cached token is dropped. |
| `hosts.go` | **Adapt** | `NormalizeHost` (grain's `model.NormalizeGitHost`) and a per-host base URL (`GitProxy.Hosts`). Grain's per-credential `<name>.host.json` becomes `Credential.Host`, which the proxy still checks: a credential for another host is refused, not sent. |
| `audit.go` | **Adapt** | Same entry shape. `Sandbox`/`Task`/`Template`/`Declaration` become a free-form `caller` map, for grain to fill: the caller's on every entry, and each allowed ref's own (`RefGrant.Caller`). Exactly one entry per request, 401s included: the per-ref grants and the fast-forward proof's upstream fetches, which grain logs as entries of their own, are fields of the push's entry. Written through an `AuditLog` interface stage 7b points at the session's audit log. |
| `server.go`, `tokens.go` | **Adapt** | One proxy per session (`Serve`), one token, in memory (`SessionToken`). Grain's sealed token file exists for a long-lived daemon. |
| `refusals.go` | **Mostly later** | Only `isPush` and an upstream 401's handling carry over: it is answered as a 403 naming the credential, so git does not ask for a new password for the proxy. Grain's rewriting of GitHub's 403/404 bodies to name the credential and where to fix it is grain-shaped (team settings, `credentials.json`) and can come back with silo's own wording. |
| `credentials.go` (pattern ladder, extra names), `apptoken.go`, `teamaudit.go`, `readscope.go`/`WriteScopes`/`SandboxTeams`, `packages.go`, `forbidden.go`, `reach.go`, `reach_refusal.go` | **Drop** for silo | Multi-tenant daemon concerns: teams, credential ladders, GitHub App minting, per-sandbox scope maps, grain's own state repository, credential reach queries. A Silofile names its credential directly. App-token minting can come back as a credential source later. |
| `local.go` | **Kept as a test upstream** | `LocalForwarder`: git http-backend over bare repositories on disk, under the same receive hardening, behind the `Forwarder` interface. The package's integration tests clone and push through the proxy to it with real git; stage 7b's can too. |
| `sshforward.go`, `sshkey.go` | **Later** | Git-over-ssh upstreams. v2.0 is HTTPS only. |

`v2/gitproxy` was copied from grain at commit
`5789160c875aa7abb6b4978c3fa337ea6f985a94`; its package doc records
it, and each file names the grain file it came from, so a later sync
diffs grain's `pkg/gitproxy` from there.

**Where the shared code lives** **(call 13)**: in silo v2, as
`v2/gitproxy`, a package with no dependency on grain, holding the
copied and adapted core above behind a small `Policy` interface. Grain
then imports it and keeps its own `Policy` implementation over
`model.Store`, which retires the duplicated mechanics in grain's own
`pkg/gitproxy`. Putting the shared copy in grain instead would make silo
depend on grain, the wrong way round for "grain is silo's client". The
split is done as stage 7, and grain's switch is its own later task.

## 10. Journey 1: `silo create`

The user runs **`silo create [dir]`** and builds a silo together with an
agent in their terminal.

### 10.1 What happens

1. silo picks the CLI: `--cli` if given, else `create.cli` from the user
   config (`$XDG_CONFIG_HOME/silo/config.yaml`), else the first of
   `claude`, then `codex`, found on `PATH`. agy is not offered (§6.1).
   No CLI found: silo says which to install and exits 69.
2. silo picks the target: `dir` (default `.`). If it holds a
   `Silofile`, this is an edit (§10.4). Otherwise silo creates the
   directory if it is missing.
3. silo runs its **built-in silo**, a Silofile embedded in the binary
   ([`examples/create/Silofile`](examples/create/Silofile)), with the
   target directory substituted. It is an ordinary `silo run` of an
   ordinary Silofile, through the same gateway and the same
   `remote_permissions` code. Nothing about `silo create` is special
   apart from where its Silofile comes from.
4. The agent asks what the user wants, proposes sections one at a time
   (where the agent runs, which tools, which credentials), and writes the
   files. It calls `silo_validate` as it goes.
5. When the user is done, the agent calls `silo_check` (the dry run) and
   reports what it found. When the CLI exits, silo validates the folder
   itself and prints either the exact command to run, for example

   ```text
   ✓ gcp-ops is valid.
     Run it with:  silo run ./gcp-ops
   ```

   or what is still invalid and how to resume (`silo create ./gcp-ops`).

### 10.2 The agent's tools

The agent runs `local`, with `remote_permissions: none`. Everything it
can do is a gateway tool:

| Tool | What it does |
| --- | --- |
| `workspace_list`, `workspace_read`, `workspace_write`, `workspace_delete` | File operations confined to the target directory: the `folder` sandbox kind. Paths are resolved beneath the directory (`openat2` with `RESOLVE_BENEATH` on Linux, a component-by-component `O_NOFOLLOW` walk elsewhere), so neither `..` nor a symlink leaves it. `workspace_write` can set the executable bit, for scripts. |
| `silo_schema` | The JSON Schema of the Silofile, or one section of it. |
| `silo_docs` | Lists and reads the embedded documentation: this design, `cli-mapping.md`, the user guide. |
| `silo_examples` | Lists and reads the embedded example folders. |
| `silo_source` | Reads the embedded copy of silo's own schema types and validators (the `silofile` package's `.go` files), for questions the docs do not answer. |
| `silo_validate` | `silo validate` on the target directory. |
| `silo_check` | `silo run --dry-run` on the target directory: binaries, ssh reachability, MCP servers resolvable, IAM permissions the user holds. Read-only. |

**MCP tools rather than files in the workspace** **(call 14)**. With
`remote_permissions: none` the agent has no file tools of its own, so
"files in its workspace" would be gateway tools anyway. Tools work the
same for every CLI and every location, cannot be edited by the agent,
and do not clutter the user's folder.

Everything the tools read is **embedded in the `silo` binary** (Go
`embed`): the schema, the docs under `v2/docs/`, the examples, and the
schema package's source. `silo create` needs no network and no checkout,
only the CLI's own login. `silo schema`, `silo docs` and `silo examples`
print the same material on the command line.

`silo_check` is the only tool that touches anything outside the target.
It runs as the user and uses their credentials, read-only: `command -v`,
`ssh -o BatchMode=yes <host> true`, reading MCP config files, and
`testIamPermissions`. It never creates an account or a container. Its
output goes to the agent, which is what the user wants here: the agent
uses it to fix the Silofile.

### 10.3 What the agent is told

The built-in Silofile's system prompt tells the agent its job (build a
runnable silo with the user), the tools above, and three rules: ask
before granting anything wide (`remote_permissions: all`, `roles/owner`,
`roles/editor`, push to a default branch, a credential `to: {agent:
...}`); prefer the narrowest setting that does the job; and finish with
`silo_check`. The rules are prompt guidance. The guarantees are the
gateway's: the agent cannot write outside the target directory and
cannot run anything at all.

### 10.4 Refining an existing silo

`silo create <dir>` on a folder that already has a `Silofile` starts the
same session in edit mode: the agent is told it is editing, and reads
the existing files first. `silo edit <dir>` is an alias **(call 15)**.
Before the session, silo copies the folder's current files (not
`.silo/`) to `.silo/backup/<timestamp>/`, so an edit the user regrets
can be undone. At exit, if the result does not validate, silo says so
and keeps both versions. It never restores on its own.

### 10.5 Lifecycle

| Event | What happens | Left behind |
| --- | --- | --- |
| Normal exit | Validate, print the run command, tear down. | The folder, `.silo/backup/`, the audit log. |
| Ctrl-C | Passed to the CLI (interactive). A second one ends the session. Partial files stay; validation runs and reports. | Partial folder, backup, audit log. |
| silo killed | The CLI dies with it (same process group on the terminal). | Partial folder, session directory, which the next `silo run` removes. |

### 10.6 Security

- **Reach:** read and write inside the target directory; read the
  embedded material; run `silo validate` and the read-only dry run.
- **Guaranteed** (enforced by the gateway and the CLI flags): no shell,
  no file access outside the directory, no network tool. Claude's
  `--tools ""` and Codex's reviewed no-environment configuration are
  what make "no built-in tools" true. That is why agy is not offered.
- **Best-effort:** that the agent writes a *sensible* Silofile. The user
  reviews it; `silo run` shows a summary of what it grants before
  starting, and asks for confirmation the first time a folder is run, or
  when its grants changed since the last run, which `.silo/lock.json`
  records **(call 16)**.
- **A compromised agent** could write a Silofile that grants too much,
  or a script that does harm when the user later runs the silo. It
  cannot run either. The summary and confirmation at first `silo run`
  are the defence.

## 11. Journey 2: Google project

The agent runs **locally** and is given `gcloud`, `gsutil` and `bq` on
a target project with declared roles. It has no local permissions: no
built-in tools, no scripts, no checkout. It acts in the cloud only.

The example is [`examples/google-project/`](examples/google-project/).

### 11.1 The account

Each `projects[]` entry gets a **per-user, reusable service account**.

- **Spec.** The canonical spec is
  `{v: 1, project: <project id>, user: <invoking principal email>, roles: [sorted, unique], conditions: []}`,
  serialised as JSON with sorted keys. `conditions` is empty in v2.0
  (project-level, unconditional roles only) and is in the hash from the
  start, so adding conditions later changes no existing name.
- **Name.** `silo-` + the first 20 characters of the lowercase
  base32 (RFC 4648 alphabet, no padding) encoding of SHA-256(spec): 25
  characters in all. A service account id must be 6–30 characters,
  start with a letter, and hold only lowercase letters, digits and
  hyphens. 100 bits of hash make a collision negligible. The email is
  `silo-xxxx@<project>.iam.gserviceaccount.com`.
- **What the account records about itself.** GCP service accounts have
  a display name (≤100 characters) and a description (≤256 characters),
  but **no labels**: the core model's "description and labels carry the
  spec" cannot be done literally **(call 17)**. silo writes:
  - display name: `silo: <user>` (truncated);
  - description: one line, `silo/v2 spec=<full sha256 hex> user=<email> created=<date> used=<date>`.
    The role list does not fit in 256 characters in general, so the
    description carries its hash, and the full spec is kept in the
    user's local state (`$XDG_STATE_HOME/silo/accounts.json`).
- **Verified on reuse, against the source of truth.** The description is
  only a hint. On reuse silo reads the project's IAM policy and checks
  that the account's bindings are exactly the declared roles: no fewer,
  and no more. It also checks that the description's `spec=` and `user=`
  match, and that only the invoking user holds
  `roles/iam.serviceAccountTokenCreator` on the account. Any mismatch
  is refused, naming what differs. silo does not "fix" an account it
  finds altered, because something other than silo altered it **(call
  18)**. `silo gc --project p --account <email>` deletes it, and the
  next run recreates it.

### 11.2 Setup, as the invoking user

silo uses the user's own gcloud login (`{ambient: gcloud}`: Application
Default Credentials, or the `gcloud` CLI's active account), on the
invoking machine only.

1. **Can the user set this up?** One
   `projects.testIamPermissions` call on the project for
   `iam.serviceAccounts.create`, `iam.serviceAccounts.get`,
   `iam.serviceAccounts.update`, `iam.serviceAccounts.delete`,
   `iam.serviceAccounts.setIamPolicy`,
   `resourcemanager.projects.getIamPolicy` and
   `resourcemanager.projects.setIamPolicy`. That is what
   `roles/iam.serviceAccountAdmin` plus
   `roles/resourcemanager.projectIamAdmin`, or `roles/owner`, give. Any
   missing: refuse, listing them and the roles that grant them.
2. **Can the user grant each role?** A user with `setIamPolicy` can, in
   GCP's own rules, grant any role. silo is stricter: it refuses a role
   unless the user **holds every permission in it** themselves **(call
   19)**, so the agent can never do something the user cannot. silo
   reads each role's permissions (`roles.get`; custom project roles
   too), and tests them in batches of 100 with `testIamPermissions`.
   Permissions that cannot be tested at project level are listed as a
   warning rather than a refusal. The role must also appear in
   `roles.queryGrantableRoles` for the project. Basic roles
   (`roles/owner`, `roles/editor`, `roles/viewer`) are allowed only if
   the Silofile sets `allow_basic_roles: true` on the entry.
3. **Create or reuse.** If the account exists and verifies (§11.1), go
   on. If it does not exist: create it, then add one binding per role to
   the project policy (read-modify-write with the policy's `etag`,
   retried on conflict), then grant the user
   `roles/iam.serviceAccountTokenCreator` on the account itself (the
   account's own IAM policy, not the project's).
4. **Wait for propagation, and verify.** IAM changes take from seconds
   to several minutes to apply. silo mints a token for the account
   (retrying while `generateAccessToken` returns 403, which is how a new
   token-creator grant shows up) and then calls
   `projects.testIamPermissions` *as the account*, for a sample of each
   role's permissions, until all are granted. Progress is shown as a
   list of roles with ticks. It gives up after 10 minutes with exit 69,
   leaving the account in place for the next run.

`silo run --dry-run` runs steps 1 and 2 only.

### 11.3 Startup

If the account exists and verifies, silo mints a token at once:
`iamcredentials.projects.serviceAccounts.generateAccessToken` with the
`cloud-platform` scope and a **lifetime of 15 minutes** **(call 20)**,
using the user's credentials, which hold token-creator on the account.
Typical startup cost for a reused account is two API calls.

### 11.4 Running

The agent's only capability is the `gcloud`, `gsutil` and `bq` gateway
tools. Each takes `args: [string]` and runs that program with exactly
that argv in the `cloud` sandbox:

- a `container` sandbox from the Cloud SDK image
  (`gcr.io/google.com/cloudsdktool/google-cloud-cli:stable`, pinned by
  digest in `.silo/lock.json` on first run), with no host mounts and
  `network: open` (it has to reach Google);
- the token is delivered as a file, `/run/silo/gcp/token` (mode 0400),
  with `CLOUDSDK_AUTH_ACCESS_TOKEN_FILE` pointing at it and
  `CLOUDSDK_CORE_PROJECT` set to the project;
- silo refreshes the token every 10 minutes and replaces the file
  atomically, so a token is never more than 15 minutes from expiry;
- as a second path for client libraries and tools that do not read
  gcloud's settings, silo also serves a **metadata-server emulator** to
  the sandbox (`GCE_METADATA_HOST` pointing at a silo-owned listener on
  the sandbox's network) that answers the token, project id and account
  email endpoints, and nothing else.

`gsutil` and `bq` are run through the Cloud SDK image's gcloud-managed
wrappers, which use gcloud's credentials. That is an assumption to check
in stage 2 (open question 6). The emulator is the fallback if they do
not.

The user's own credentials never enter the sandbox or the agent's
process. Only the account's short-lived token does.

`sandbox_exec` on the `cloud` sandbox is not offered unless the Silofile
asks for it: with only the three tools, the agent cannot read the token
file directly, though it could still print a token with
`gcloud auth print-access-token`. That is the same capability it already
has, for at most 15 minutes **(call 21)**.

### 11.5 Teardown

On a normal exit, or the first Ctrl-C after the CLI exits: stop
refreshing, delete the token file and the sandbox, and update `used=` in
the account's description (one `serviceAccounts.patch`). **The account
and its grants are kept**, so the next run starts at once.

- `ttl:` on the entry (default `7d`) is how long the account may sit
  unused. `silo gc` lists the user's accounts from local state, and for
  each project also lists `silo-*` accounts whose description names the
  invoking user (so a lost local state file does not strand them). An
  account whose `used=` is older than its ttl is deleted: first its
  project bindings are removed (bindings of a deleted account linger as
  `deleted:serviceAccount:...` otherwise), then the account.
- `ttl: 0` deletes the account and its bindings on exit.
- `silo gc` never touches an account another user created.

### 11.6 Lifecycle

| Event | What happens | Left behind |
| --- | --- | --- |
| Normal exit | As §11.5. | Account and grants (unless `ttl: 0`); audit log. |
| Ctrl-C during setup | Setup stops after its current API call. A half-made account (created, roles partly bound) is recorded in local state as `incomplete`. The next run finishes it, after verifying what is there matches the spec. | The incomplete account. |
| Ctrl-C while running | As normal exit. | As normal exit. |
| silo killed or host crash | Nothing runs. The last token stays valid until it expires (≤15 minutes). The sandbox container is removed by the next `silo gc`. `used=` is stale. With `ttl: 0` the account survives until `silo gc`, which treats an account with `ttl: 0` in local state and no live session as expired. | Token valid ≤15 min; container; account. |
| Propagation timeout | Exit 69. | Account, for the next run. |

Access tokens minted by `generateAccessToken` cannot be revoked early.
The 15-minute lifetime is the bound on how long a token outlives its
session.

### 11.7 Limits and failures

- **Quota.** A project allows 100 service accounts by default. One
  account per (user, role set) is usually few, but a user who edits
  roles often makes many. silo counts `silo-*` accounts at setup, warns
  above 50, and refuses above 90 with the `silo gc` command to run.
  Changing roles makes a *new* account, and the old one ages out under
  its ttl.
- **Policy size.** A project IAM policy has size limits (1,500
  principals per policy). Each account adds one principal. The quota
  check above keeps silo well within it.
- **Partial failures.** Creation and each role binding are separate
  steps. silo records each in local state as it completes. A failure
  mid-way stops, reports what was done, and the next run resumes it.
  Failed IAM writes are retried with backoff on 409 (etag conflict) and
  429/503, and never on 403.
- **Organization policy** can forbid service-account creation
  (`iam.disableServiceAccountCreation`) or restrict who may be granted
  roles. silo reports the API's own error and the constraint's name.
- **Deleted and recreated** accounts get a new unique id. Bindings to
  the old one do not carry over. silo binds by email after creation,
  and verifies by email and unique id.

### 11.8 Security

- **Reach:** the project, with exactly the declared roles, through
  three CLI programs in a container. Nothing local: no built-in tools,
  no host files, no scripts.
- **Guaranteed:** the agent's cloud identity is the account, never the
  user. The roles are a subset of the user's own permissions. Built-in
  tools are off (claude or codex only).
- **Best-effort:** the container's isolation from the host (a container
  is not a VM; `gvisor` is stronger and can be chosen); the token's
  15-minute tail after a crash.
- **A compromised agent** can do anything the roles allow in the
  project, for the session plus at most 15 minutes, and can send what it
  reads to the internet (the sandbox has open network to reach Google).
  Choose roles accordingly. It cannot reach the user's other projects,
  files or credentials.

## 12. Journey 3: remote agent, narrow local reach

The agent runs on a remote host over `ssh` and inherits that host's
ambient credentials and models. It may call **specific scripts on the
local host, with arbitrary arguments**, and nothing else local.

The example is [`examples/remote-scripts/`](examples/remote-scripts/).

### 12.1 The path

The `ssh -R` reverse forward of §5.3 is the only path back to the
invoking machine. The local executor is part of the gateway at its other
end, so it needs no separate tunnel or listener.

### 12.2 Which scripts

- **Listed** (`local.scripts.list`): each entry names a file in the silo
  folder.
- **A folder** (`local.scripts.dir`, default `scripts/`): every regular,
  executable file directly in it (not in subdirectories). Dotfiles are
  ignored.
- At session start silo **snapshots** the set: for each script its
  name, path, size, mode and SHA-256. Scripts added to the folder later
  are not served. A script whose content changes is refused at its next
  call: silo re-hashes before every call **(call 22)**. The agent cannot
  write to the invoking machine, so only the user could cause that, and
  silo says so rather than running something nobody snapshotted.
- **Refused at snapshot:** a symlink whose target is outside the silo
  folder; a file or any of its parent directories (up to the folder)
  writable by group or world; a file not owned by the invoking user. A
  refused script fails the session, with the reason, rather than being
  dropped silently.

### 12.3 One tool per script

- **Name:** `local_<name>`, with the file name lowercased and anything
  outside `[a-z0-9_]` turned into `_`. Two scripts that map to the same
  name fail validation.
- **Description:** from the Silofile entry's `description:`, else from
  `# silo:` lines in the script's first 20 lines, else
  `Run <name> on the user's machine.` silo also reads an optional
  `# silo-args:` line as a one-line usage summary added to the
  description.
- **Input schema:** `{args: [string], stdin: string?}`. Nothing else.
- **Execution:** `execve(path, [path, args...], env)`, **no shell**,
  with exactly that argv. Arguments are passed byte for byte. NUL bytes
  are refused (they cannot be in an argv).
- **As whom:** the invoking user, with their current permissions and
  their **current environment** (the one `silo run` was started with,
  minus `SILO_*` and minus every credential silo resolved, plus any
  credential delivered `to: {local: <script>, env: NAME}`).
- **Where:** `local.workdir` (default: the silo folder). Fixed. The
  agent cannot change it.
- **Limits:** `timeout` (default 5 minutes, per entry or for all), after
  which the script's process group gets SIGTERM, then SIGKILL 5 seconds
  later. `output_cap` (default 1 MiB each for stdout and stderr). Output
  past the cap is dropped and the result says how much.
- **Result:** `{exit_code, stdout, stderr, truncated, duration}`. A
  non-zero exit is a normal result, not an MCP error.
- **`approve: true`:** the call waits for the user to approve it. In
  `headless` mode on a terminal, silo prompts on its own terminal. In
  `interactive` mode the terminal belongs to the CLI, so silo prompts
  through a desktop notification and `silo approve <session>` in another
  terminal, and the call waits up to the script's timeout. Under `--json`
  it is an `approval_request` event answered on the control socket.
  A denied or timed-out approval is a tool error.
- **Audited** like every gateway call, with argv, exit code, sizes and
  duration.

### 12.4 Nothing else local

The gateway on a remote session serves the scripts, plus whatever the
Silofile declares (sandboxes, MCP servers). Nothing else. There is no
generic local exec, file read or environment read.

**The trust model, stated plainly:** a script that takes arbitrary
arguments is only as safe as the script. `eval "$@"`, `sh -c "$1"`,
`bash -c`, `xargs` on the arguments, `ssh host "$@"`, passing an
argument to `--config`, `-o ProxyCommand=`, or to any program that
treats one as code, are holes that give the agent general execution as
the user. silo cannot see inside a script, so it cannot guarantee
otherwise. `silo validate` warns, best-effort, on the obvious cases: a
script containing `eval`, `sh -c`/`bash -c` with a positional
parameter, an unquoted `$@` or `$*`, or `xargs`, and on a script that
is itself an interpreter or shell (a symlink to `/bin/sh`, `python3`)
**(call 23)**. A warning is not a refusal. The user wrote the script.

As built in stage 4a (§12.2–§12.4):

- The result also carries `signal` and `timed_out` when they apply, and
  `stdout_bytes`/`stderr_bytes`, how much the script wrote; past
  `output_cap` silo keeps the first and last halves of the cap and
  marks the cut, so the difference is what was dropped. `exit_code`
  comes first, so the audit log's head of the result always holds it.
- An approval waits up to the script's timeout, and the run then has
  its own timeout. A script that changed while its call waited for
  approval is refused then too.
- In `headless` mode without a terminal, approvals go through
  `silo approve` as in `interactive` mode, with the request printed on
  silo's stderr instead of a desktop notification. `silo approve` takes
  the session id or a unique prefix of it, and the call id may be left
  out when only one call is waiting.
- The control socket exists in every session from the start of setup:
  JSON-RPC 2.0, one message per line. `approve` takes
  `{"id"?: string, "deny"?: bool}` and answers with the call it
  answered. Under `--json` (stage 6) the same queue answers, announced
  by an `approval_request` event.
- A symlink to an interpreter outside the folder is already refused at
  snapshot, so the interpreter warning covers a file in the folder that
  is not a `#!` script and is named like a shell or interpreter, and a
  wrapper that hands `"$@"` straight to one (`exec python3 "$@"`).

### 12.5 On the remote host

`remote_permissions` governs what the agent does there: `none`, `all`,
or a subset. Journey 3's default case is `all` (the remote host is the
agent's to use, with its own ambient credentials), but nothing requires
it. The remote environment, credentials and model login are the remote
user's, not silo's.

### 12.6 Lifecycle

| Event | What happens | Left behind |
| --- | --- | --- |
| Normal exit | The CLI exits, ssh returns its code. silo removes the remote session directory, ends running scripts (SIGTERM, SIGKILL), closes the master. | Audit log. |
| Ctrl-C | Reaches the remote CLI through the pty. Second Ctrl-C: silo ends the session. | Audit log. |
| Connection drops | The master exits. silo kills running scripts, tears down locally, exits 75. On the remote, sshd hangs up the CLI, and its gateway is gone in any case. | The remote session directory (holding the MCP config with the dead session's token, and prompts), removed by the next `silo run` to that host or `silo gc --host`. |
| silo killed | ssh dies with it (Pdeathsig), so as a drop. Running scripts die with their process group. | Remote session directory; local session directory. |
| Remote reboot | As a drop. | Nothing remote, if `XDG_RUNTIME_DIR` was used. |

### 12.7 Security

- **Reach:** on the remote host, whatever `remote_permissions` allows
  with the remote user's ambient credentials. On the invoking machine,
  the listed scripts only, with any arguments.
- **Guaranteed:** no route back except the reverse-forwarded gateway
  port, which binds remote loopback only and wants the session token.
  Only snapshotted scripts run, with no shell between the agent and
  `execve`.
- **Best-effort:** everything a script does with its arguments; the
  warnings in §12.4.
- **A compromised agent** owns the remote host as the remote user (with
  `all`), and on the invoking machine can do whatever the scripts allow
  with any arguments. If any script is a hole, that means anything the
  user can do.

## 13. Journey 4: local and remote MCP servers

The user gives the agent MCP servers that already exist, on the
invoking machine or on the agent host, not only servers silo starts.

The example is [`examples/mcp-servers/`](examples/mcp-servers/).

### 13.1 Referencing a server

Each `mcp[]` entry has a `name` (the gateway's prefix for its tools),
`where: local | remote` (default `local`), and exactly one of:

- **`command: [argv...]`** (plus `env:`): silo starts it, over stdio.
- **`url: http(s)://...`** (plus `headers:`): an already-running server,
  over Streamable HTTP (or SSE, for older servers). silo connects to it.
- **`from: <source>:<name>`**: a server defined in an existing config
  file, resolved to a `command` or `url` entry.

Plus `tools:` (an allowlist of the server's tool names; default: every
tool it lists), `required:` (default true) and `restart:` (§13.4).

**Config sources** silo reads **(call 24)**:

| Source | File | Where the name is looked up |
| --- | --- | --- |
| `claude-user` | `~/.claude.json` | top-level `mcpServers.<name>` |
| `claude-project` | `~/.claude.json` | `projects["<dir>"].mcpServers.<name>`, with `dir` given as `from: claude-project:<name>@<dir>` |
| `mcp-json` | a `.mcp.json` file: `from: mcp-json:<name>@<path>`; `<path>` defaults to the silo folder's `mcp/servers.json`, then `.mcp.json` | `mcpServers.<name>` |
| `codex` | `$CODEX_HOME/config.toml` (default `~/.codex/config.toml`) | `[mcp_servers.<name>]` |
| `agy` | `~/.gemini/config/mcp_config.json` | `mcpServers.<name>` |

Every source must be named. silo does not search all of them for a
bare name, because two configs defining `github` differently would make
the Silofile's meaning depend on which was found first. For
`where: remote`, the file is read on the agent host over ssh. The
resolved entry (command, url, env names) is printed by `--dry-run` and
recorded in `.silo/lock.json`. Secrets that a config file holds inline
(an `env` value, an `Authorization` header) are **not** used: silo
refuses an entry with inline secret-looking values unless the Silofile
re-declares them as credentials, so that a server never gets a
credential the Silofile does not name **(call 25)**.

### 13.2 Routing

**Every server's tools go through silo's gateway.** The CLI's MCP config
still names only the gateway. Allowlisting and audit apply to every
server the same way.

- **A local server** is reached by the gateway directly: silo starts it
  (stdio), or connects to its URL. A remote agent reaches it only
  through the existing reverse forward to the gateway.
- **A remote server** lives on the agent host. **silo starts it there
  and proxies it into the gateway** **(call 26)**, rather than letting
  the remote CLI connect to it directly:
  - `command:`: silo runs it on the agent host over the ssh master
    (`ssh -S ... -T <host> -- <argv>`) and speaks MCP over that
    channel's stdin and stdout. Its credentials go in a 0600 env file in
    the remote session directory, sourced and deleted by the command's
    own preamble, as for the agent (§5.3).
  - `url:` (`http://localhost:8080` on the agent host): silo adds a
    local forward on the master (`-O forward -L`) and connects to it
    through that.

  Why proxy: a direct connection would mean writing the server into the
  CLI's own MCP config, which breaks "the gateway is the only MCP
  server", loses the allowlist and audit, and does not work with
  `remote_permissions: none`, where the CLI must see one server only.
  The cost is one ssh round trip per call, which is small next to a
  model turn.

  With `remote_permissions: all` the agent can of course start or call
  any server on its host itself, through its shell. The proxy route
  controls what silo *offers*. It does not confine an agent that owns
  the host.

### 13.3 What a server inherits

| Server | Environment | Everything else |
| --- | --- | --- |
| Local, started by silo | A minimal environment: `PATH`, `HOME`, `USER`, `LANG`, `TMPDIR`, plus the entry's `env:`, plus credentials delivered `to: {mcp: <name>, ...}`, plus names listed in `inherit_env:` (or the whole environment with `inherit_env: all`). | It runs as the invoking user, in the silo folder, with that user's full filesystem and network. silo controls its environment, not its reach: a server that reads `~/.config/gh/hosts.yml` gets the user's GitHub login. |
| Remote, started by silo | The remote login environment, filtered the same way, plus delivered credentials. | As the remote user. |
| Existing (`url:`) | Whatever it was started with. silo does not know or change it. | As whoever started it. |

So "never ambiently by accident" holds for *environment variables*,
which is where most MCP servers look for keys. It cannot hold for files
a server reads by itself, and silo says so in the `--dry-run` output for
every launched server: "runs as <user> with access to their files".

### 13.4 Lifecycle and failures

- **silo starts and stops the servers it launches.** Each starts in its
  own process group (locally) or ssh channel (remotely). Teardown sends
  SIGTERM (locally) or closes the channel (remotely, which hangs it up),
  then SIGKILL after 5 seconds.
- **silo never stops a server it did not start** (`url:` entries).
- **At start**, silo connects to each server and lists its tools,
  within 30 seconds. A tool named in `tools:` that the server does not
  offer is an error (a typo would otherwise silently drop a tool). A
  server that fails to start, or a URL that does not answer:
  `required: true` fails the session (exit 69); `required: false`
  starts without it, and its tools are absent.
- **During a session**, a call to a server that has died returns a tool
  error naming the server. `restart: on-failure` restarts a launched
  server once per 60 seconds at most. The default is `never` **(call
  27)**, because a restarted server has lost whatever state its tools
  had built. When a server's tool set changes (restart, or a `url:`
  server sending `tools/list_changed`), the gateway re-filters it
  through the allowlist and sends `notifications/tools/list_changed` to
  the CLI.

As built in stage 5a (`where: local`; stage 5b's `where: remote`
follows):

- **Transports.** A `url:` server is tried as Streamable HTTP first. If
  the POST of `initialize` is answered 400, 404 or 405, silo falls back
  to the older HTTP+SSE transport (the MCP spec's
  backwards-compatibility rule), so the entry needs no transport field.
  Proxied results are passed on as text: content blocks other than text
  are dropped, as the gateway serves text only.
- **Reading configs.** Besides `url`, the JSON sources accept Gemini
  CLI's `httpUrl` and agy's `serverUrl`. claude-project's `<dir>` and
  mcp-json's `<path>` may be absolute, `~/...`, or relative to the silo
  folder, and claude-project looks the cleaned absolute path up exactly.
  Settings silo does not apply are printed as notes by `--dry-run`: a
  config's `cwd` (servers run in the silo folder), codex's `env_vars`
  (list them in `inherit_env` instead) and a `disabled` or
  `enabled = false` flag (naming the server in the Silofile runs it).
- **Secrets in configs (call 25).** One rule covers env variables and
  headers: a credential delivered `to: {mcp: <name>, env: KEY}` replaces
  the config's value for `KEY`, whether `KEY` is an env variable of a
  launched server or a header of a `url:` one. A value with no such
  credential that looks like a secret (its name says token, key,
  password, secret, credential or auth; or it starts like a known token,
  `Bearer ...`, or is a URL with a password) is refused. A `${VAR}`
  reference, as Claude Code expands it (codex's `bearer_token_env_var`
  and `env_http_headers` are read as the same references), is filled
  from a credential delivered with `env: VAR`, or for a launched server
  from silo's environment when `VAR` is in `inherit_env`; otherwise it
  is refused. So the Silofile names every secret a server gets.
- **Environment.** The minimal environment takes `PATH`, `HOME`, `USER`,
  `LANG` and `TMPDIR` from silo's own, where set. Then, each overriding
  the one before: `inherit_env`, the config's `env`, the entry's `env:`,
  and delivered credentials.
- **Lifecycle details.** A launched server's stderr goes to
  `mcp-<name>.stderr` beside the audit log. Its process group is
  recorded in `session.json` before it is connected to, so `silo gc`
  kills it after a crash. A dead server is noticed when its stdout
  closes or it exits, and its group is killed then too. `restart:
  on-failure` retries at the same rate, once per 60 s at most, until it
  starts or the session ends. After a restart or a `list_changed`, an
  allowlisted tool the server no longer offers is a warning rather than
  an error: the session is running, and the tool is simply absent.
- **`--dry-run`.** Stage 5a adds a minimal `silo run --dry-run`, a
  check registry in `session` that stage 3a extends. It resolves every
  `from:`, applies the secret rule with credentials by name, finds a
  launched server's binary on the `PATH` it will have, and prints one
  line per server, with "runs as <user> with access to their files"
  for launched ones. It does not contact `url:` servers. `silo run`
  records the resolved entries (names of variables and headers, never
  values) under `mcp` in `.silo/lock.json`; `--dry-run` writes nothing.

As built in stage 5b (`where: remote`):

- **Launch.** A `command:` server runs as
  `ssh -S <ctl> -T <host> -- exec /bin/sh -c '<preamble>'`, a channel of
  the master that is the proxy's process for that server, so its
  allowlist, restart and dead-server handling are a local server's. The
  preamble records its pid (sshd makes it the leader of its own session
  and process group) beside the env file, then runs
  `env -i /bin/sh -c '. <env file>; rm -f <env file>; exec "$@"'`, so the
  server has exactly the filtered environment, and the file is gone
  before the server starts. Before each launch, the env file is written
  with `umask 077` and `cat >` over the master. A remote server runs in
  the remote home directory (the silo folder is not on the agent host).
- **Environment.** The remote login environment is what `env` prints
  over the master, read once per session; the minimal names,
  `inherit_env` and `${VAR}` references are filled from it as from
  silo's own for a local server.
- **`url:`.** The local end of the `-L` forward is a Unix socket beside
  the master's control socket (in the 0700 session directory), not a
  loopback port, so no other local user can reach the server through
  it. silo dials the socket but keeps the entry's URL, so the `Host`
  header and TLS name the server as the agent host sees it. A forward
  that fails is the server not answering: `required:` decides.
- **Reading configs.** A `from:` file is read with `cat` over the
  master (on its own connection, for `--dry-run`), with `HOME` and
  `CODEX_HOME` from the remote login environment. A relative
  `mcp-json` path is the remote home directory's, and `mcp-json:<name>`
  with no `@<path>` is refused for `where: remote`: its defaults are in
  the silo folder, which is not there. `where: remote` with
  `location.kind: local` is refused when the session starts (and by
  `--dry-run`): there is no agent host but this machine.
- **Teardown.** Closing a remote server is SIGTERM to its local ssh,
  which closes the channel; sshd hangs the server's stdin and stdout
  up. Then, over the master, silo waits up to 5 s for the server's
  process group to go, sends it SIGKILL, and removes its pid and env
  files. After a crash, `silo gc` kills the local ssh (its process group
  is in `session.json`), which closes the channel the same way; the
  remote session directory is removed by the next `silo run` to that
  host or `silo gc --host`, which does not kill by recorded pid, since a
  pid on another host may have been reused since.
- **A dropped connection.** Once the master has exited, every remote
  server's tools answer "MCP server <name> is unreachable: the
  connection to <host> is gone", a server whose channel ended with it is
  not restarted whatever `restart:` says, and the session exits 75 as
  in §12.
- **`--dry-run`** reaches the agent host on its own BatchMode
  connection, reads the login environment and the `from:` file there,
  finds a launched server's binary with `command -v` on the `PATH` it
  will have, and prints "runs as <remote user> on <host> with access to
  their files". It does not contact a remote `url:` server.

### 13.5 Security

- **Reach:** the allowed tools of the declared servers, and through
  them whatever those servers can do as their user.
- **Guaranteed:** the agent sees only allowlisted tools, every call is
  audited, and no server is in the CLI's config except the gateway.
- **Best-effort:** what a server does with its own credentials and
  files. A server is trusted code running as the user.
- **A compromised agent** can call the allowed tools with any arguments.
  An over-broad tool (a "run SQL" or "execute" tool) is a hole of the
  same kind as journey 3's `eval`.

## 14. Future journeys 5–10

None of these is built in the first set. Each must fit the core without
redoing it. For each: what it reuses, what it adds, and a sketch.

### Journey 5: agent in a VM inside a project

silo creates or reuses a VM in the project. The VM's attached service
account is journey 2's per-user hashed account, and the CLI runs on the
VM over ssh or IAP.

- **Reuses:** journey 2's account machinery whole (naming, verification,
  ttl, `silo gc`); the `ssh` location's connection, staging, reverse
  forward and teardown, once the VM is up; sandboxes and the local
  executor as for `ssh`.
- **Adds:** a `vm` location whose `Prepare` creates or finds the VM and
  then hands off to the `ssh` code.
  - **VM lifecycle:** named like the account (`silo-<hash>` over the
    account spec plus the machine spec), labelled (VMs, unlike accounts,
    have labels) with the spec hash, user and last-used time. Stopped,
    not deleted, on exit; deleted by `silo gc` after `ttl`.
  - **Image:** a public image plus a startup script that installs the
    CLI, or a user image (`vm.image:`). The CLI is a remote prerequisite
    checked exactly as for `ssh`.
  - **Reaching it:** `gcloud compute ssh --tunnel-through-iap` as the
    ssh transport, so the VM needs no external IP. The `ssh` location
    takes a transport command for this.
  - **The model credential:** with `auth: ambient` the CLI's login must
    already be on the image, which is unusual. So journey 5 is where
    `auth: {credential: x}` matters: silo delivers the API key into the
    CLI's environment through the staged env file. On Vertex AI, the
    VM's own account can be given the Vertex user role, and the CLI uses
    the metadata server, with no key at all.

```yaml
location:
  kind: vm
  vm:
    project: acme-staging
    zone: europe-west4-a
    machine_type: e2-standard-4
    image: projects/acme/global/images/agent-base
    ttl: 7d
    via: iap
projects:
  - project: acme-staging
    roles: [roles/compute.viewer, roles/aiplatform.user]
    attach_to: vm          # the VM runs as this account
agent: {cli: claude, auth: ambient, mode: headless, prompt: prompts/task.md}
remote_permissions: all
```

### Journey 6: agent on a hosted runtime (Google AX, capsule)

silo does not run the CLI process: the runtime does.

- **Reuses:** the gateway, the local executor, credentials, the event
  stream; the `Location` interface as written (§5.1), which already
  allows `Start` to return a handle to a process silo did not start.
- **Adds:** an `ax` (or `capsule`) location whose `Prepare` submits or
  attaches to a hosted session and whose `Gateway` returns a URL that
  the runtime reaches through a tunnel silo opens *outbound* (the
  runtime cannot ssh back to a laptop). Grain's `pkg/tunnel` (a mux over
  a WebSocket through IAP) is the model for that tunnel. The staged
  files (MCP config, prompt) are passed in the submission instead of
  being written to a disk silo can see.
- **Open questions:** see open questions 13–17.

```yaml
location:
  kind: ax
  ax: {project: acme-agents, runtime: default}
agent: {cli: claude, mode: headless, prompt: prompts/task.md}
local:
  scripts: {dir: scripts}
```

### Journey 7: local agent with a granule sandbox (the grain model)

The agent runs locally with `remote_permissions: none`, and every
command runs in a granule microVM with the repo checked out through the
git proxy.

- **Reuses:** the gateway's sandbox tools; the git proxy; the sandbox
  `Backend` interface.
- **Adds:** the `granule` sandbox kind, a backend driving granule's CLI
  (`granule vm create <name>`, `granule vm exec`, `granule vm wait`,
  `granule vm delete`) on the local Docker, which is granule's model:
  "VMs in containers". It needs `/dev/kvm` (checked by `granule doctor`
  in `--dry-run`), granule on `PATH` (or its Go packages, once its module
  can be fetched), a guest image with git, and the git proxy reachable
  from inside the guest (the proxy listens where the VM's network can
  reach it; the clone URL carries a per-sandbox token, as in grain).
- **Bridge to grain:** this is a grain run with silo underneath:
  [`grain.md`](grain.md).

```yaml
agent: {cli: claude, mode: headless, prompt: prompts/task.md}
remote_permissions: none
sandboxes:
  - name: dev
    kind: granule
    image: ghcr.io/acme/dev-guest:2026-10
    cpus: 4
    memory: 8Gi
    repos: [widgets]
git:
  - {name: widgets, repo: github.com/acme/widgets, access: push,
     push: [refs/heads/silo/task-42], credential: gh}
```

### Journey 8: agent in a container image on Kubernetes

- **Reuses:** the `Location` interface; the gateway; credentials.
- **Adds:** a `k8s` location.
  - **Pod lifecycle:** silo creates one pod from the given image
    (labelled with the session id), waits for it to be ready, starts the
    CLI with `exec` (interactive: a TTY attach) or as the container's
    command (headless), and deletes the pod at teardown. `silo gc`
    deletes pods by label.
  - **Logs and TTY attach:** the Kubernetes exec/attach API with a TTY
    for interactive, and logs for headless.
  - **The gateway:** silo's process is outside the cluster, so the path
    is a tunnel: a small sidecar in the pod that serves the gateway URL
    on the pod's loopback and carries it back to silo over a connection
    silo opens (a port-forward run in reverse is not offered by the
    Kubernetes API, so the sidecar dials out or silo uses
    `kubectl port-forward` to the sidecar, which then relays). Grain's
    `pkg/tunnel` is the model. Stage 11a built this with
    `kubectl port-forward` (§5.4), and starts the CLI with `exec` in
    headless mode too, not as the container's command (§5.4 says why).
  - **Credentials:** delivered as a Secret mounted into the pod (deleted
    at teardown), or as workload identity: the pod's Kubernetes service
    account bound to journey 2's per-user GCP account. Stage 11a
    delivers the Secret; workload identity is later.

```yaml
location:
  kind: k8s
  k8s:
    context: gke_acme_europe-west4_agents
    namespace: silo
    image: ghcr.io/acme/agent:claude-2.1
    resources: {cpu: "2", memory: 4Gi}
    identity: {gcp_account_for: acme-staging}
agent: {cli: claude, auth: {credential: anthropic}, mode: headless, prompt: prompts/task.md}
```

### Journey 9: granule sandboxes on Kubernetes

- **Reuses:** the `granule` sandbox kind of journey 7; the sandbox
  `Backend` interface.
- **Adds:** a Kubernetes backend for sandboxes, chosen per sandbox with
  `backend: {k8s: ...}`. granule runs on unmodified Kubernetes as a pod
  whose container image is a granule VM image, given the
  `devic.es/kvm` (and, with network, `devic.es/tun`) resources by
  granule's KVM device plugin (granule `deploy/k8s/`). A guest with no
  network needs neither `NET_ADMIN` nor `tun` and passes the
  `restricted` Pod Security Standard. `kubectl exec` reaches the guest
  through granule's in-guest agent, which is what `Sandbox.Exec` maps
  to; `kubectl logs` gives the guest console. The cluster needs nodes
  with nested virtualisation and the device plugin; `--dry-run` checks
  for the device resource on some node.
- **Grain:** grain's `docs/k8s-granule-design.md` records what running
  granule pods in a cluster settled for grain (privileged vs device
  plugin, node pools). silo should follow it rather than decide again.

```yaml
sandboxes:
  - name: dev
    kind: granule
    image: ghcr.io/acme/dev-guest:2026-10
    backend:
      k8s: {context: gke_acme_europe-west4_sandboxes, namespace: silo-sandboxes}
```

### Journey 10: a granule sandbox defined by a Granulefile

A Granulefile is granule's build definition: a strict superset of the
Dockerfile format with one added instruction, `VM` (granule
`docs/granulefile.md`). With no `VM` instruction it builds a container
image; with `VM WRAP`, `VM PACK` or a `VM FROM` stage it builds a
granule VM image, which is still an OCI image.

- **Reuses:** everything of journeys 7 and 9; `container` sandboxes'
  `build:` of a Dockerfile.
- **Adds:**
  - **Reference:** `granulefile: sandboxes/dev.Granulefile` in a
    sandbox entry (instead of `image:`), with `target:` and `args:` for
    the stage and build arguments. Because a Dockerfile is a Granule, a
    `container` sandbox's `build:` and a `granule` sandbox's
    `granulefile:` are the same field read by two builders.
  - **Build:** `granule granulefile check` in `silo validate`, and
    `granule granulefile build` at `silo run`. A Granulefile whose final
    stage is a container stage is refused for `kind: granule` (it would
    build a container) and vice versa.
  - **Cache:** keyed by the SHA-256 of the Granulefile, its build
    context and its args, with the resulting image digest recorded in
    `.silo/lock.json`, so a second run does not rebuild. For a remote
    backend (k8s), the image is pushed to a registry the Silofile names
    (`push_to:`) and referenced by digest.

```yaml
sandboxes:
  - name: dev
    kind: granule
    granulefile: sandboxes/dev.Granulefile
    target: guest
    push_to: europe-docker.pkg.dev/acme/silo   # only needed for a remote backend
```

## 15. Security model

Each journey's section has its own security model: §10.6, §11.8, §12.7
and §13.5. This section says what holds for all of them.

**What silo guarantees, given an honest silo binary and an honest CLI
binary:**

- The CLI is configured with exactly one MCP server, the gateway, and
  the built-in tools `remote_permissions` allows, or silo did not start
  it.
- The gateway serves only declared tools, and audits every call.
- Credentials are delivered only to their declared destinations, and
  the agent's environment is scrubbed (on `local`).
- Git credentials and the user's cloud credentials never reach the
  agent.
- The only network path from a remote agent to the invoking machine is
  the reverse-forwarded gateway port on the remote's loopback.

**What silo does not guarantee:**

- **That the CLI honours its flags.** The CLI is trusted code. silo
  relies on `--tools ""` meaning no tools, and on Codex's environment
  manifest. This is why the Codex mapping is pinned to reviewed versions
  and why agy's `none` is refused rather than approximated.
- **Isolation stronger than its mechanism.** A container is not a VM.
  `gvisor` and (later) `granule` are stronger.
- **That tools are narrow.** A script with `eval`, an MCP server with an
  "execute" tool, or `roles/editor` are as wide as they look.
- **Confidentiality of what the agent can read.** A sandbox with open
  network, or a remote host with `all`, can send anything it reads
  anywhere.

**Against a compromised agent**, the design's answer is that what it can
do is what the Silofile says, plus whatever the declared tools allow
when given hostile arguments. The Silofile is the thing to review, and
`silo run`'s first-run summary (§10.6) is where a person reviews it.

**Against a compromised agent host** (`ssh`): the attacker gets the
gateway token and, through the forward, the same tools the agent has.
Nothing more of the invoking machine.

## 16. Package layout

```text
v2/
├── cmd/silo/              # main: run, create/edit, validate, schema, docs,
│                          #   examples, gc, audit, approve, secret
├── silofile/              # types, strict YAML decoding, folder resolution,
│                          #   validation, JSON Schema generation
├── session/               # one session: setup order, teardown, session.json,
│                          #   signals, exit codes, event stream, control socket
├── events/                # the event schema shared by --json and the
│                          #   library (events.md)
├── gateway/               # the MCP server: tool registry, routing, refusal,
│                          #   audit, redaction
│   └── audit/
├── mcp/                   # MCP client and server plumbing shared by gateway
│                          #   and proxied servers; config source readers
│   └── sources/           # claude-user, claude-project, mcp-json, codex, agy
├── location/              # the Location interface
│   ├── local/
│   ├── ssh/               # master connection, probe, forwards, staging
│   ├── vm/                # later
│   ├── k8s/               # pod, Secret, tunnel, exec, gc (stage 11a)
│   └── hosted/            # later: ax, capsule
├── agentcli/              # per-CLI mapping: modes, remote_permissions,
│   ├── claude/            #   MCP config format, auth, version checks,
│   ├── codex/             #   transcript parsing into events
│   ├── agy/
│   └── custom/
├── sandbox/               # the Backend interface
│   ├── container/         # docker/podman, gvisor via runtime
│   ├── folder/            # silo create's workspace
│   └── granule/           # later
├── localexec/             # script snapshot, checks, warnings, execve
├── credentials/           # sources, destinations, keyring store
├── gcp/                   # journey 2: account spec, setup, tokens,
│                          #   metadata emulator, gc
├── gitproxy/              # shared with grain (§9.2)
├── create/                # the built-in silo create Silofile and its tools
├── docs/                  # these documents; embedded into the binary
│   └── examples/          # embedded too
└── silo.go                # the library entry point (grain.md §2.1)
```

Packages are top-level under the module, not under `pkg/` or
`internal/`, because grain imports several of them (`silo`,
`silofile`, `gitproxy`) **(call 28)**. Anything not meant for import
goes under `internal/` when it is written.

## 17. Staged implementation plan

Each stage is small enough to file as its own task, and ends with
something runnable and tested. The suggested order is kept, with one
change argued below.

| # | Stage | Delivers | Depends on |
| --- | --- | --- | --- |
| 1 | **Core** | `silofile` (types, strict decoding, folder, validation, schema), `silo validate`, `silo schema`, `session` (setup/teardown, signals, session dir, exit codes), `gateway` (registry, refusal, audit), `location/local`, `agentcli/claude` and `agentcli/codex` for `interactive`/`headless` with `none`/`all`/subset, `sandbox/container` (+gvisor), `credentials` (env, file, ambient). A silo with a container sandbox runs end to end. | — |
| 2 | **Journey 2** | `gcp`: account spec and naming, setup checks, create/verify/reuse, propagation wait, token minting and refresh, delivery, metadata emulator, `silo gc` for accounts. | 1 |
| 3 | **Journey 1** | `create`: the built-in Silofile, `sandbox/folder`, the `silo_*` tools, embedded docs and examples, `silo run --dry-run`, `silo edit`, the first-run summary. | 1, 2 (for `silo_check`'s IAM checks) |
| 4 | **Journey 3** | `location/ssh`, `localexec`, approvals, `silo gc --host`. Built as 4a (`localexec`, approvals) and 4b (`location/ssh`, `silo gc --host`). | 1 |
| 5 | **Journey 4** | `mcp/sources`, launched and URL servers, local and remote, allowlists, lifecycle. | 1, 4 (remote servers) |
| 6 | **Programmatic interface** | `--json` events, control socket, the Go library API, caller-provided MCP servers. | 1 |
| 7 | **Git proxy** | `gitproxy` copied and adapted from grain, `git:` and `sandboxes[].repos`. | 1 |
| 8 | **Granule** | `sandbox/granule`, Granulefiles (journeys 7, 10). | 1, 7 |
| 9 | **Detach and re-attach** | per-session supervisor, resumable event stream. | 6 |
| 10 | **`vm`** (journey 5) | | 2, 4 |
| 11 | **`k8s`** (journeys 8, 9) | 11a: `location/k8s`, the tunnel sidecar, Secrets, `silo gc --k8s` (journey 8). 11b: granule sandboxes on Kubernetes (journey 9). | 6, 8 (11a also waits on 9) |
| 12 | **`ax`/`capsule`** (journey 6) | | 6, open questions 13–17 |

**One change to the suggested order:** stages 6 and 7 could come before
stage 5 if grain's adoption is the priority. Journey 4 is the only one of
1–4 grain does not need. Grain needs 1, 6, 7 and 8, roughly in that
order (see [`grain.md`](grain.md) §3). The order above keeps the
operator's, and stages 4–8 depend only on stage 1 and can run in
parallel, so the choice is about attention, not dependencies.

Stage 1 is the largest. If it needs splitting, the seams are:
1a `silofile` + `silo validate` + `silo schema`; 1b `gateway` +
`sandbox/container` with a test client instead of a CLI; 1c `session`,
`location/local` and the `claude`/`codex` mappings.

## Calls made in this draft

Decisions the core model did not settle, made by the author, for review:

1. Strict YAML: unknown keys are errors; no merge keys or tags; `silo: v2` required. (§2.1)
2. `silo run` runs the local part of `silo gc` at startup. (§3.1)
3. silo's own exit codes are 64+ (sysexits), 124 for deadline, 130 for Ctrl-C. (§3.3)
4. Audit logs are kept under `$XDG_STATE_HOME/silo/sessions/`. (§4.2)
5. The gateway is Streamable HTTP on loopback with a bearer token, for every CLI and location. (§4.3)
6. `local` scrubs the CLI's environment to a fixed allowlist. (§5.2)
7. `ssh` uses the system OpenSSH binary, not a Go client. (§5.3)
8. silo never installs or logs in a CLI on a remote host. (§5.3)
9. `remote_permissions` subsets use the CLI's own tool names. (§6.1)
10. The `folder` sandbox kind is built-in only in v2.0. (§7.1)
11. v2.0 ships `container` and `gvisor` sandboxes; `granule` is stage 8. (§7.1)
12. The `store:` credential source is the OS keyring. (§8.1)
13. The shared git proxy lives in silo v2 and grain imports it. (§9.2)
14. `silo create`'s reference material is served as MCP tools, not files. (§10.2)
15. `silo edit` is an alias for `silo create` on an existing folder, with a backup. (§10.4)
16. `silo run` shows a grants summary and asks for confirmation on first run and whenever grants change. (§10.6)
17. Service accounts have no labels; the description carries a spec hash, and local state the full spec. (§11.1)
18. A reused account that does not match its spec is refused, not repaired. (§11.1)
19. A role is grantable only if the user holds every permission in it. Basic roles need `allow_basic_roles: true`. (§11.2)
20. Tokens are minted for 15 minutes and refreshed every 10. (§11.3)
21. `sandbox_exec` on the cloud sandbox is off unless asked for. (§11.4)
22. Scripts are re-hashed before every call. (§12.2)
23. `silo validate` warns on obvious argument-to-code holes in scripts. (§12.4)
24. The MCP config sources are the five in §13.1, always named explicitly.
25. Inline secrets in an existing MCP config are refused unless re-declared as credentials. (§13.1)
26. Remote MCP servers are started by silo and proxied through the gateway. (§13.2)
27. Launched MCP servers are not restarted by default. (§13.4)
28. Packages are top-level in the module, not under `pkg/`. (§16)

## Open questions

**agy and Codex**

1. Should agy get a best-effort `none` (private `HOME`, a single MCP
   server, the prompt preamble grain uses) behind an explicit opt-in
   such as `remote_permissions: {none: best_effort}`? This draft
   refuses, per "refuses to start".
2. Codex has no per-tool allowlist. Should a Codex-specific value such
   as `codex_sandbox: read-only` be offered as its "subset", or is
   refusing subsets right?
3. Who reviews new Codex versions for `none`, and is grain's reviewed
   table (`ReviewedCLIVersion`) the shared source of truth?

**CLI flags** (see [`cli-mapping.md`](cli-mapping.md) §4 for the full
list of unverified assumptions)

4. `claude --bare` would harden `none` further (no hooks, plugins,
   CLAUDE.md), but per its help it only reads API-key auth, never
   OAuth. Use it whenever `auth` is a credential?
5. The semantics of `attached` mode for each CLI (`--remote-control`,
   `codex remote-control`/`app-server`, `agy remote-control`) are
   unexercised. Is `attached` wanted in v2 at all?

**Google Cloud**

6. Do the Cloud SDK image's `gsutil` and `bq` honour
   `CLOUDSDK_AUTH_ACCESS_TOKEN_FILE`? If not, is the metadata emulator
   enough, or should `gsutil` be replaced by `gcloud storage`?
7. Is "the user must hold every permission in a role" (call 19) the
   intended reading of "refuses any role the user couldn't grant"?
8. Conditions (IAM Conditions) and resource-level grants (a bucket, a
   dataset): wanted in the second iteration, or later?
9. Should accounts be shared between users with the same role set
   (fewer accounts, but weaker attribution)? This draft says no: the
   user is in the hash.

**MCP config sources**

10. Are the five sources (§13.1) the right set? Gemini CLI's
    `~/.gemini/settings.json` and VS Code's `mcp.json` were left out.
11. Should `claude-project` default to the silo folder's path, so that
    `from: claude-project:x` works without `@<dir>`?

**ssh**

12. Is a TCP loopback forward with a bearer token acceptable on shared
    remote hosts, or is `gateway_transport: unix` needed in v2.0?

**AX and capsule (journey 6)**

13. How does a hosted runtime receive the MCP config and prompt: in the
    session submission, or from a URL silo serves?
14. How does the runtime reach the gateway: does it accept an outbound
    tunnel endpoint (IAP, a relay like grain's Cloud Run proxy), or must
    silo register a URL?
15. Is there a TTY for interactive mode, or is a hosted session always
    headless?
16. Who holds the model credential: the runtime (ambient) or silo?
17. Is `capsule` the same interface as `ax` with a different backend,
    or does it differ (it is grain's "User Capsule", a developer VM,
    which looks more like journey 5)?

**Grain** (see [`grain.md`](grain.md) §5)

18. Does grain want silo as a library (in-process) or as a subprocess
    per run? This draft offers both, from the same `session` package.
19. Which of grain's MCP tools move to the gateway (sandbox tools) and
    which stay grain's (task tools)? Stage 6b built the mechanism, a
    per-session rename map (`silo.Options.Rename`), and chose no names.
