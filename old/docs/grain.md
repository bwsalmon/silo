# Silo v2: grain as the first client

Part of the [v2 design](design.md). The operator wants grain
(bwsalmon/grain) to be rebuildable on silo v2: **silo runs one agent
session; grain orchestrates many.** This document says what v2's core
offers grain, grounded in grain's source as of task 3800, and how grain
moves over. Grain's own code changes are separate, later tasks.

## 1. The division

**Grain keeps** tasks and the queue, teams and RBAC, its UI and chat,
pull requests and the merge queue, CI, ports and releases, memory,
schedules and metrics, and the choice of connection and model per run
(`pkg/daemon/agents.go`'s `AgentPool`: rotation, concurrency leases,
usage-limit pauses).

**Silo takes** one run: starting the CLI with the right flags
(`pkg/agent/claude`, `codex`, `antigravity` today), the MCP endpoint the
CLI talks to, the sandbox the tools reach, the git proxy's per-run
policy, the model proxy's per-run token, and the run's cleanup.

Each grain run becomes: **write a Silofile, `silo run` it headless,
read its events, answer its tool calls.** Grain's task tools
(`ask_question`, `comment_on_issue`, `open_pull_request`, `wait_for_checks`,
...) stay grain's, served to the session as a caller-provided MCP server
(§2.2).

A Grain is single-tenant and manages one backlog, whatever machine it
runs on. Linking many Grains through the Silo hub, which decides
placement while each Grain enforces its own local policy, is
[hub.md](../../docs/hub.md).

## 2. Requirements, one by one

### 2.1 A programmatic interface

Grain today calls `agent.Framework.Run(ctx, RunConfig)` and gets a
`Result` (final text, tool calls, transcript, resumable session id),
with a live tee of the transcript, `Addenda` for messages injected
mid-run, a `Deadline`, and `OnResumableSession` (grain
`pkg/agent/agent.go`). silo has to give at least that, for many
concurrent sessions in one process.

**Both a library and a subprocess interface, from one implementation**
(design call; the `session` package drives both):

- **Library:** `silo.Start(ctx, silofile.Silofile, silo.Options) (*silo.Session, error)`,
  where the Silofile is a Go value (grain builds it in code, never as
  YAML on disk), and `Options` carries the caller's MCP servers,
  credentials as values, and an event sink.
  `Session.Events()` (a channel of typed events), `Session.Inject(msg)`,
  `Session.Cancel(reason)`, `Session.ExtendDeadline(t)`, and
  `Session.Wait() (Result, error)`.
- **Subprocess:** `silo run --json <dir>` writes the same events as JSON
  lines on stdout; the session directory's control socket
  (`control.sock`, JSON-RPC over a Unix socket, mode 0600) takes
  `inject`, `cancel`, `extend_deadline`, `approve`. A subprocess is what
  survives a caller restart (§2.7).

**Events** (one schema for both): `status` (setting up, running,
tearing down, with the step), `transcript` (one CLI transcript item,
normalised across CLIs: assistant text, thinking where exposed, tool
call, tool result), `tool_call` (every gateway call, start and end, as
audited), `usage` (tokens in and out, cache, per model, as the CLI
reports it), `approval_request`, `resumable` (the CLI's session id),
and finally `result`.

**Result**: `{exit: {code, reason: completed | cancelled | deadline |
cli_error | connection_lost | setup_failed}, final_text, cli_session,
usage_totals, audit_path, transcript_path}`. Grain's run-failure
classification (`pkg/agent/runfailure.go`, `usagelimit.go`, `refusal.go`)
moves into silo's per-CLI transcript parsing, because it is a property
of the CLI's output, and arrives as the `reason` plus a `cli_error`
detail (`usage_limit`, `refused`, `overloaded`, ...).

**Injection**: claude and agy take a user message on stdin in
stream-json mode, mid-run (grain's `Addenda` is delivered that way for
agy today, and by MCP tool-result piggyback for others). Codex takes one
between turns. `Inject` picks the CLI's mechanism and says in its return
value whether the message was delivered now or queued for the next turn.

As built in stage 6b, the socket's methods and the library's `Session`
methods share one implementation ([`events.md`](events.md), "The control
socket"). `Session.Approve(id, allow)` was added to the library, so that
a library caller can answer `approval_request` as `silo approve` does.
`Inject` returns `(Delivery, error)`.
- **claude**: the message is written to stdin until the turn's
  `result`. After that it is queued, and silo resumes the session with
  `claude -p --resume <id>`. The design names only stdin for claude; the
  resume fallback keeps a message that arrives just after the result
  from being refused.
- **codex**: the message is always queued for `codex exec resume <id>
  -`. `codex queue` is not used, because its semantics are still
  unchecked (cli-mapping.md §4, item 8).
- **agy**: not driven by `silo run` yet.

### 2.2 Caller-provided MCP servers

Grain serves its tools today with `grain mcpserver`, a child process the
CLI starts over stdio (claude, agy) or that grain fronts with
authenticated loopback HTTP (codex, grain `docs/codex-isolation.md`).
Its arguments carry the sandbox, the task, the grants (`GrantArgs`,
`ProposeRepoArgs`, `RunDeadlineArgs`, ...).

In silo, grain passes **caller MCP servers** to the session: each a URL
(or Unix socket path) plus a per-session token, served by grain, which
the gateway connects to as a client and exposes under its name. This is
journey 4's `url:` server, given by the caller instead of the Silofile,
with three differences: it may be a Unix socket; its token is per
session; and the caller may mark tools as **blocking** (grain's
`wait_for_checks`, `ask_question`), so the gateway does not apply its
default timeout to them. Allowlisting and audit apply as for any server.

As built in stage 6b ([`events.md`](events.md), "Caller-provided MCP
servers"), this is `silo.Options.CallerMCP` and `silo run --caller-mcp`:
- **Socket form.** A Unix socket is `unix:/path`, or a bare absolute
  path, serving Streamable HTTP at `/mcp`.
- **Token.** The token is sent as a bearer token and redacted like a
  credential. On the command line it comes from
  `$SILO_CALLER_MCP_TOKEN_<NAME>`, never argv.
- **Required.** Every caller server is required. A wrong token fails
  setup.
- **Blocking tools.** They run with no gateway timeout. Their presence
  raises the CLI's own MCP timeout to 7 days.
- **Reconnecting.** As built in stage 9 (§2.7), a caller server that goes
  away is dialled again on the next call, so a caller that comes back on
  the same address is used again. Calls made while it is gone fail with
  a retryable error.

The split of grain's tools: the **sandbox tools** (`run_command`,
`read_file`, `write_file`, `edit_file`) become silo's own `sandbox_*`
tools. Grain's **task tools** stay grain's and come through the caller
server. Grain's tool names (which its prompts, its transcript parser
and its memory notes use) can be kept by a per-session rename map in
the gateway, so a migrated run looks the same to the model (open
question 19 in the design). As built in stage 6b, the map is
`silo.Options.Rename`, keyed by silo's name and valued by the name the
model sees. The audit log records both names. Silo ships no map: the
names are grain's to choose, when it migrates.

### 2.3 Sandboxes good enough for grain

Grain's backends today: host-rooted directories, containers
(`orchestrator/container_sandboxes.go`) and granule VMs
(`pkg/granule`, `GranuleVM` in `RunConfig`), with an image build per
repo (`pkg/sandboximage`) and a package cache (`pkg/pkgcache`: apt, Go
proxy, npm, PyPI, Docker, Playwright).

silo's sandbox spec covers these with fields reserved in the design
(§7.2):

- **granule VMs and containers, with sizing**: `kind: granule` or
  `container`, `cpus`, `memory`, `disk` (stage 8 for granule).
- **A persistent repo workspace through the git proxy**:
  `lifetime: persistent` plus a caller-chosen sandbox id. silo does not
  destroy a persistent sandbox at teardown, and the next session naming
  the same id gets it back. The checkout is made through the session's
  git proxy, and its remote URL is rewritten to each new session's proxy
  token when re-attached.
- **Network policy**: `network: none | open | {allow: [...]}`. The
  `allow` form is an egress proxy that only connects to the listed
  hosts; for granule, through granule's own network mode.
- **A package-cache hook**: `package_cache: {url: ..., ca: ...}` on a
  sandbox. silo does not run a package cache. It injects the caller's
  (grain's `pkgcache`) proxy settings and CA into the sandbox, the way
  grain's `pkgcache/sandbox.go` does today. The cache stays grain's.

### 2.4 Git proxy parity

From grain's `pkg/gitproxy` (design §9.2):

- **Push only to the session branch plus declared refs**: grain writes
  `push: [refs/heads/grain/eu/task-3800, <declared refs>]`. Grain's
  "declared push" (`RefGrant`, `DeclaredPushLookup`) becomes extra
  patterns, and the audit entry's `caller` map carries the template and
  declaration that allowed each.
- **The fast-forward proof**: `force: false`, from grain's
  `ancestry.go`/`pack.go`, copied.
- **Per-host credentials, including ambient** (grain #3719): credentials
  per `git[]` entry, with `{ambient: gh}` and `{ambient: git-credential}`
  sources, per host (grain's `hosts.go`).
- **Audit the caller can read**: in the session's audit log and as
  `tool_call`-like `git` events in the stream.

The shared code moves to silo's `v2/gitproxy` and grain imports it
(design call 13).

### 2.5 In-sandbox model access and usage reporting

Grain's `pkg/modelproxy` gives a sandbox a per-run token that is worth
nothing except against the proxy, which forwards to the team's real
connection (Anthropic, OpenAI, Google, Claude on Vertex) with a cap on
concurrent requests per run.

silo v2 gets a **model proxy** next to the git proxy, built from the same
code, enabled per sandbox (`model_access: [anthropic, openai, google]`),
setting `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL` and the Gemini
equivalent in the sandbox. The upstream credential is a credential
delivered `to: {proxy: model}`, so grain hands silo the connection's key
per session.

**Usage reporting** (grain #3070): the proxy counts tokens per response
(the vendors return usage in every response, streaming included) and
emits `usage` events tagged `source: sandbox`, next to the CLI's own
`usage` events (`source: agent`). Grain meters both per connection and
pauses with its existing `orchestrator.Pause`. The proxy also takes a
`pause` command on the control socket, so a paused connection stops a
running session's sandbox traffic at once, not only the next dispatch.

### 2.6 A pool of GCP identities

Grain's `gcp-key` capability hands a run one of a pool of service
accounts (`gcp_service_account_count`, `grain-sb-agent-N@...`) as a key
file. That is different from journey 2's per-user accounts: the pool is
the deployment's, pre-made, and shared across runs one at a time.

silo supports it as a **caller-provided identity**: a `projects[]` entry
may name `account: <email>` instead of `roles:`, and the caller passes
a credential that can mint tokens for it (or, today, its key file).
silo delivers a short-lived token exactly as in journey 2 (§11.4) and
skips account setup. Choosing which pool account a run gets stays
grain's. Moving grain from key files to minted tokens is an improvement
grain can take later.

### 2.7 Surviving the caller's restart

Grain already solved this for itself: a run's CLI runs in a **run host**,
a container that is a sibling of the daemon, writing an append-only
spool the daemon tails from a cursor; a new daemon **adopts** run hosts
by their labels (grain `pkg/runhost`, `adopt.go`,
`docs/non-disruptive-upgrade.md`).

silo v2 does the same per session (stage 9):

- `silo run --detach` starts a **per-session supervisor**, a `silo`
  process that is not a child of the caller (double-forked, or started
  in its own run-host container by the caller). It owns the CLI, the
  gateway and teardown.
- The event stream is written to `events.jsonl` in the session
  directory, each event numbered. `silo attach <id> --from <seq>`
  (or `silo.Attach(id, seq)` in the library) replays from the cursor
  and then follows. The control socket stays where it was.
- `silo ls` lists live sessions from the session directories. Grain's
  adoption pass becomes: list sessions labelled with grain's run ids,
  attach to each from the cursor it stored.
- Caller MCP servers (§2.2) are reconnected by the gateway when the
  caller comes back on the same socket path. Calls made while it was
  gone fail with a retryable error, which grain's tools already have to
  tolerate across a restart.

As built in stage 9 ([`events.md`](events.md), "Detach and re-attach"):

- **Supervisor.** `silo run --detach [--label k=v ...] <Silofile|dir>`
  re-executes silo as `silo __supervise <same arguments>`, in a session
  of its own (`setsid`), with stdin, stdout and stderr on `/dev/null`. It
  waits on a pipe until the session has an id and a directory, prints
  the id and exits; the supervisor is then no child of the caller. A
  failure before that is reported and exits as `silo run` would. A
  failure later in setup is the session's `setup_failed` result, which
  `attach` shows. `--detach` needs `agent.mode: headless` and is refused
  with `--json`. The supervisor's own log is `supervisor.log` beside
  the audit log. `session.json` records `detached: true`, and its `pid`
  is the supervisor's, so `silo gc` treats a killed supervisor as any
  crashed `silo run`: it kills the CLI's process group and removes what
  the session recorded.
- **Events.** Every session writes `events.jsonl`, not only a detached
  one, and the library's too. It is in the **state directory**
  (`$XDG_STATE_HOME/silo/sessions/<id>/events.jsonl`, beside
  `audit.jsonl`), not the session directory. The session directory is
  removed at teardown, before the result event is written, and a caller
  that comes back after the session ended needs that result. Each line
  is one event of the stream, with its `seq`.
- **The cursor** is the seq of the last event the caller has handled:
  `silo attach <id> --from <seq>` and `silo.Attach(ctx, id, seq, opts)`
  deliver the events after it, then follow until the result. `--from 0`
  replays everything. Grain's run host keeps a byte offset; a seq is the
  same idea in the stream's own terms. Attaching to an ended session
  replays it to its result. A session whose process died without one
  ends `attach` with an error (`silo.ErrNoResult`), and exit 1 for the
  command.
- **Labels.** `silo run --label k=v` and `silo.Options.Labels` are kept in
  `session.json`. `silo ls [--label k=v ...] [--json]` and
  `silo.List(labels, opts)` list the live sessions carrying them, so
  grain's adoption pass is: list by its run-id label, then attach from
  the cursor it stored. The library's `Attached` handle sends `Inject`,
  `Cancel`, `ExtendDeadline` and `Approve` to the control socket.
- **Caller servers.** Every caller server is marked to reconnect. A call
  whose request never reached the server (a refused dial, or HTTP 404
  for a session the restarted server does not know) is made once more
  on a fresh connection, so the first call after the caller is back
  succeeds. A call that fails otherwise returns a tool error saying it
  is retryable (`proxy.UnavailableError`), and the next call dials
  again.
- **Not built.** A detached session is started from the command line;
  the library has no detached `Start`, because the supervisor reads a
  Silofile from disk. A library caller writes one and runs
  `silo run --detach`. CLI-level resume (`claude --resume`, `codex exec
  resume`, cli-mapping.md §3.4) is not used to restart a CLI: the
  supervisor keeps it alive. The detach covers silo's caller, not an
  ssh session's link to its agent host (design §5.3). Persistent
  sandboxes, and who collects them (open question 4), are untouched.

### 2.8 Sandbox-only sessions (CI)

Grain CI (`pkg/ci`) runs workflow jobs in sandboxes with no agent at all,
with its own caching by content fingerprint, artifacts and failure
summaries.

**Decision: grain CI keeps its own path in the first migration.** It
would gain little from silo beyond the sandbox backends, and it has
requirements a session does not (job graphs, fingerprints, artifacts).
silo offers `silo sandbox run <Silofile> -- <command>`: a session with
no agent, which sets up the declared sandboxes, git proxy and
credentials, runs one command, and tears down. When grain CI wants
granule-on-k8s sandboxes (journey 9), it can switch its sandbox creation
to that, and keep everything else.

### 2.9 Run hosts and remote executors onto locations

| grain today | silo location |
| --- | --- |
| CLI as a child of the daemon (`agent.LocalHost`) | `local` |
| Run host container next to the daemon (`pkg/runhost` docker driver) | `local`, inside a supervisor container the caller starts (§2.7): the run host becomes where the detached supervisor lives |
| Run host on Kubernetes (`pkg/runhost/kube.go`) | `k8s` (journey 8) |
| Remote executor on a developer's VM (`grain executor`/`modelexec`, connected outbound through the Cloud Run proxy and IAP; drives the developer's CLIs with their ambient login, tool calls tunnelled back to grain's sandboxes) | `capsule` (journey 6), or `ssh` when grain can reach the host. The remote executor is exactly journey 3's shape: an agent on a remote host with its ambient credentials, with tools served from elsewhere. What it adds is an **outbound** connection, which is the `ax`/`capsule` tunnel question (design open question 14). |
| `UnconfinedHost` (native tools kept on remote executors) | `remote_permissions: all` |

## 3. Migration path

Behind a per-template (or per-team) flag, `runner: silo`, run by run,
with the existing path as the default until each step has run clean:

1. **Silo stages 1 and 6 land.** Grain adds a `silo` agent framework:
   an `agent.Framework` whose `Run` builds a Silofile value from
   `RunConfig` (local location, `remote_permissions: none`, a
   `container` sandbox matching the run's, grain's `mcpserver` passed as
   a caller MCP server for the task tools) and calls `silo.Start`.
   Enabled for one internal template.
2. **Sandbox tools move.** The run's sandbox tools come from silo's
   gateway instead of `grain mcpserver`, with the rename map keeping
   their names. Compare transcripts on the flag.
3. **Silo stage 7 lands; grain imports `v2/gitproxy`.** Runs on the
   flag use silo's per-session proxy with grain's policy.
4. **Silo stage 8 (granule) lands.** Granule runs move.
5. **Silo stage 9 (detach) lands.** Grain's run-host adoption switches
   to silo's attach for runs on the flag.
6. **Model proxy and usage events** (§2.5) move.
7. **Default flips**, per CLI, once the transcript and outcome numbers
   match. The old frameworks are deleted one release later.
8. **Remote executors** move last, once `capsule` exists.

Grain CI stays on its own path throughout (§2.8).

## 4. What silo must not take from grain

Teams, RBAC, the task queue, connection selection, and anything that
outlives one session other than what §2.3 (persistent sandboxes) and
§2.7 (detached sessions) name. If a later requirement needs silo to
know about more than one session at a time, it belongs in grain.

## 5. Open questions for grain's adoption

1. Library or subprocess per run? Library is cheaper; subprocess
   survives a daemon restart without a supervisor. This design offers
   both; grain should pick one for step 1.
2. Should grain keep its tool names through a rename map (§2.2) or move
   to silo's names, and pay for it in prompts and memory notes?
3. Does grain need the CLI's raw transcript as well as silo's normalised
   one (grain's `TranscriptPath` today is raw)? This design keeps both.
4. Persistent sandboxes: who owns their garbage collection, silo
   (`silo gc` with a ttl) or grain (it knows which tasks are done)?
5. Does grain's `custom` agent backend (`pkg/agent/custom`,
   `docs/custom-agent-backends.md`) map onto silo's `cli: custom`?
