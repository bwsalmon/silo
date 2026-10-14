# Silo v2

This is Silo v2, a fresh and smaller design of Silo, under construction.
Stage 1 is in: `silo run` runs `claude`, `codex` or a custom CLI on the
local machine, interactive or headless, with `remote_permissions` none,
all or a subset, its only reach a gateway serving container and gVisor
sandboxes. `silo validate`, `silo schema` and `silo gc` are there too.
Stage 4a adds the local executor: each script the Silofile declares
becomes a gateway tool, `local_<name>`, and `silo approve` answers the
calls of scripts marked `approve: true`. Stage 5a adds local MCP servers
(`mcp[]` with `where: local`), launched or reached by URL, proxied
through the gateway with an allowlist each. Stage 4b adds the `ssh`
location (journey 3): the CLI runs on a remote host with that host's
own login, over one OpenSSH master connection, and reaches the gateway
(and so the `local_<name>` scripts) only through a reverse forward bound
to the remote loopback; `silo gc --host <host>` removes the remote
session directories crashed sessions left there.

The code at the repository root (`cmd/`, `pkg/`, the root `go.mod`) and
its specification in [`docs/silo/`](../docs/silo/README.md) are Silo v1.
v1 keeps working, and keeps being developed, until v2 replaces it.

v2 runs one agent CLI in a chosen location, with access to a declared
set of MCP tools, credentials, cloud permissions, git access and local
scripts, and nothing else. A silo is a `Silofile`, or a folder holding
one; `silo run` carries it out once.

## Design

- [`docs/design.md`](docs/design.md): the design, organised around the
  user journeys (`silo create`, a Google project, a remote agent with
  narrow local reach, existing MCP servers), with the staged
  implementation plan.
- [`docs/cli-mapping.md`](docs/cli-mapping.md): how each setting maps
  onto the real `claude`, `codex` and `agy` flags.
- [`docs/grain.md`](docs/grain.md): grain as silo's first client, and
  its migration path.
- [`docs/examples/`](docs/examples/): example silo folders.

## Module

`v2/` is its own Go module, `github.com/bwsalmon/silo/v2`, the
major-subdirectory form of Go's semantic import versioning. The root
module's `./...` stops at this directory's `go.mod`, so neither module
builds, vets or tests the other's packages.

## Commands

```sh
go run ./cmd/silo run [--dry-run] [--json] [--prompt text] [--caller-mcp name=url ...] <Silofile|dir>   # design §3
go run ./cmd/silo approve <session> [--deny] [<call-id>]   # design §12.3
go run ./cmd/silo gc [--host <host>]        # what crashed sessions left (there)
go run ./cmd/silo validate <Silofile|dir>   # design §2.4
go run ./cmd/silo schema [section]          # the Silofile's JSON Schema
```

`silo run` validates the Silofile, then runs one session: it resolves
credentials, finds the CLI and checks its version, starts the
sandboxes, starts the gateway, writes the CLI's MCP config and settings
(cli-mapping.md), and runs the CLI until it exits, the deadline passes
(exit 124) or Ctrl-C ends it (130). It exits with the CLI's own code
otherwise; a setting the CLI cannot express exits 76, a missing
prerequisite 69. The session directory is
`$XDG_RUNTIME_DIR/silo/<id>/`; the audit log and the final
`session.json` stay in `$XDG_STATE_HOME/silo/sessions/<id>/`.

Each `mcp[]` entry with `where: local` (design §13) is launched over
stdio in its own process group, in the silo folder, with a minimal
environment plus what the entry names, or connected to by URL. Its
allowed tools are served as `<server>__<tool>`. A `from:` entry is read
from one of five named config files (`claude-user`, `claude-project`,
`mcp-json`, `codex`, `agy`), and an inline secret in it is refused unless
the Silofile delivers a credential in its place. `silo run --dry-run`
validates, then checks what the session needs without changing
anything, one line per check, and exits 69 if any check fails. So far it
checks MCP servers.

`silo validate` decodes the Silofile strictly (design §2.1), resolves
every path inside the silo folder (§2.2) and checks names,
cross-references, files, `remote_permissions` against the CLI, and
credential destinations (§2.4). A value v2.0 reserves but does not ship
yet (`kind: granule`, `lifetime: persistent`, ...) is refused with an
error naming the stage that adds it. Errors name the file, line and YAML
path, and exit 65; bad usage exits 64 (§3.3). Local scripts get the
snapshot's refusals as errors (a symlink out of the folder, a file or
directory writable by group or others, a file another user owns) and
best-effort warnings on the obvious holes of §12.4: `eval`, `sh -c`
with a positional parameter, an unquoted `$@`/`$*`, `xargs`, a wrapper
that hands its arguments to an interpreter, and a script that is
itself one.

The local executor (design §12) snapshots the scripts at session start
(name, path, size, mode, SHA-256) and re-hashes each before every call,
refusing one that changed. A call runs the script with exactly the
agent's `args`, no shell, in `local.workdir`, with the invoking
environment minus `SILO_*` and every resolved credential, plus the
credentials delivered `{local: <script>, env: NAME}`. Its result is
JSON: `exit_code`, `signal` and `timed_out` when they apply,
`duration`, `truncated`, `stdout_bytes`, `stderr_bytes`, `stdout`,
`stderr`. A timeout sends SIGTERM to the script's process group and
SIGKILL 5 seconds later. A script with `approve: true` waits, for up
to its timeout, for the user: on silo's terminal in headless mode when
silo has one, and otherwise for `silo approve`, with a desktop
notification in interactive mode (`notify-send` or `osascript`, when
installed) and the request printed on stderr in headless mode.

Every session has a control socket, `control.sock` in the session
directory (mode 0600): JSON-RPC 2.0, one message per line, with the
methods `inject`, `cancel`, `extend_deadline` and `approve`
(`docs/events.md`, "The control socket"). `approve` takes `{"id":
"<call>", "deny": true}` (both optional; with no id it answers the one
call waiting). `silo approve` names the session by its id or a unique
prefix of it. The Go library's `Session` has the same four as methods.

`--caller-mcp name=url` (or `silo.Options.CallerMCP`) adds a
caller-provided MCP server, on HTTP or a Unix socket, whose tools the
gateway serves as `name__<tool>`; its token is read from
`$SILO_CALLER_MCP_TOKEN_<NAME>` (`docs/events.md`, "Caller-provided MCP
servers").

The schema `silo schema` prints is generated from `silofile`'s Go types
into `silofile/silofile.schema.json` and embedded in the binary. After
changing a type, run `go generate ./silofile` and
`go test ./cmd/silo -update`; tests fail until both are current.

## Layout

| Package | What it is |
| --- | --- |
| `silofile` | The Silofile: types, strict decoding, folder resolution, validation, JSON Schema. |
| `cmd/silo` | The `silo` command. |
| `session` | One `silo run` session (design §3): setup steps and tool providers (registries), teardown, `session.json`, signals, exit codes, the control socket, the local executor's provider and approvals, `silo gc`'s local part. |
| `location` | The `Location` and `Agent` interfaces (design §5.1). |
| `location/local` | The CLI as a child process with a scrubbed environment (design §5.2). |
| `location/ssh` | The CLI on a remote host (design §5.3): the system `ssh` behind a small `Transport` (so stage 10's IAP can replace it), the master connection, the probe, the loopback-only reverse forward to the gateway, staging with `umask 077` and `cat >`, the env file, exit 75 on a dropped connection, the remote half of `silo gc`, and the `--dry-run` checks. `Run` and `ForwardLocal` share the master with stage 5b's remote MCP servers. |
| `agentcli` | Per-CLI mappings onto argv, env and staged files: `claude`, `codex` (with the reviewed-version gate for `none`), `custom`. Golden files in `agentcli/testdata`. |
| `mcp` | MCP plumbing: JSON-RPC dispatch, the authenticated loopback server (bearer token, Host and Origin checks), and client connections over Streamable HTTP, HTTP+SSE and stdio (`Conn`). |
| `mcp/sources` | Reads `from: <source>:<name>` server definitions from `~/.claude.json`, `.mcp.json` files, codex's `config.toml` and agy's `mcp_config.json`, and the inline-secret test of call 25. |
| `gateway` | The session's MCP server (design §4): tool registry, refusal, timeouts, and the sandbox tools `sandbox_exec`, `sandbox_read_file`, `sandbox_write_file`, `sandbox_list_dir`. |
| `gateway/proxy` | Fronts MCP servers in the gateway (design §13.4): launch or connect, the 30 s start, allowlists, dead-server errors, `restart: on-failure`, `list_changed`, and SIGTERM-then-SIGKILL teardown of each launched server's process group. A `Spec` may carry a `Dial` function or an HTTP client (a Unix socket's), and blocking tools; stage 6b's caller-provided servers are fronted the same way. |
| `gateway/audit` | `audit.jsonl`: a start line before each call, an end line after, credentials redacted. |
| `sandbox` | The `Backend`/`Sandbox` interfaces (design §7.3), specs from a Silofile, and the images in `.silo/lock.json`. |
| `sandbox/container` | Docker or Podman containers; `kind: gvisor` is the same with `--runtime=runsc`. |
| `localexec` | The local executor (design §12): script snapshot, refusals, `silo validate`'s warnings, one tool per script run with no shell, and the approvers (terminal prompt, and the queue `silo approve` answers). |
| `control` | The session's control socket: JSON-RPC 2.0 over a Unix socket, a method table later stages add to, and a client. |
| `credentials` | Sources `env`, `file`, `ambient` (`gh`, `git-credential`, `gcloud`), and sandbox destinations. `store` comes later. |
| `gitproxy` | The git smart-HTTP proxy (design §9), copied and adapted from grain's `pkg/gitproxy` behind a `Policy` interface: `StaticPolicy` for a Silofile's `git:` entries, ref patterns, the fast-forward proof, one audit entry per request. Grain imports it. Wired into `silo run` by stage 7b. |
| `internal/safepath` | Path confinement, copied from grain's `pkg/safepath`. |
| `internal/procgroup` | Process-group cancellation, copied from grain's `pkg/procgroup`. |
| `internal/lockfile` | `.silo/lock.json`: one key per part of silo (`images`, `mcp`), each written without disturbing the others. |
| `internal/mcptest` | A test MCP server over stdio or HTTP whose tool list can change, used by the proxy and session tests. |
| `internal/headtail` | Bounded command output, adapted from grain's `pkg/mcp/exec_output.go`. |

## Building and testing

```sh
cd v2
gofmt -l .
go build ./... && go vet ./... && go test -race ./...
```

grain CI runs the same in `.grain/workflows/v2.yml`.

The gateway's, `sandbox/container`'s and `session`'s tests create real containers,
with `busybox:1.37`, and run the same suite with `kind: gvisor`. They
skip where there is no Docker or Podman, or no runsc registered as a
runtime; CI installs runsc and sets `SILO_V2_REQUIRE_DOCKER=1` and
`SILO_V2_REQUIRE_RUNSC=1`, which turn those skips into failures.

`session`'s tests use the test binary itself as a fake `claude`: it
answers `--version`, reads the staged MCP config, calls the gateway and
exits as told. The library's tests (`internal/fakecli`) add a claude that
answers stream-json messages on stdin, and a fake `codex` whose turn
can be resumed, for injection. No real `claude` or `codex` is needed (stage 1d checks
those). The same binary is also the MCP server those tests launch: with
`SILO_MCPTEST=stdio` it serves `internal/mcptest` over stdio.

`location/ssh`'s tests run against a real `sshd` in a Docker container
(Debian's `openssh-server`, built on first use as `silo-v2-sshd-test`)
with `location/ssh/testdata/fakecli`, built statically, as its `claude`.
They cover the probe, an unknown host key, the loopback-only forward
(and the refusal of a server whose `GatewayPorts yes` widens it),
headless and interactive runs (on a pty), exit codes, a killed master
(exit 75), `silo gc --host`, that no secret is in any remote argv, and
journey 3 end to end with `docs/examples/remote-scripts`. They skip
without Docker unless `SILO_V2_REQUIRE_DOCKER` is set.

`gitproxy`'s tests clone and push with the real `git` binary, through
the proxy, to `git http-backend` over repositories on disk. They skip
without git; CI sets `SILO_V2_REQUIRE_GIT=1`.
