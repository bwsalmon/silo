# old/: the retired Silo v2 single-run runner

This directory is **retired code**. It is Silo v2's single-run runner
(`silo run`: one agent CLI in a chosen location with a gateway, sandboxes,
a git proxy, credentials and local scripts), which used to live in `v2/`.
The meaning of "silo" it implemented, silo running each grain run, is
retired: bwsalmon/silo is now only the collection hub (the
[top-level README](../README.md) and [`docs/hub.md`](../docs/hub.md)).

It is kept, rather than deleted, so its pieces can be **ported into
bwsalmon/grain**. Nothing here is developed further, and neither the hub
nor this directory imports the other.

## What to port into grain

**Local scripts** (design.md §12, "Journey 3: remote agent, narrow local
reach"):

- [`old/localexec/`](localexec/): the local executor: the script
  snapshot and its refusals (`snapshot.go`), running one script per call
  with no shell (`exec.go`), `silo validate`'s warnings (`warn.go`), and
  the approvers (`approve.go`).
- The `local:` section of the Silofile: `type Local` in
  [`old/silofile/types.go`](silofile/types.go), validated in
  [`old/silofile/validate.go`](silofile/validate.go) (`LocalScript`).
- [`old/session/localexec.go`](session/localexec.go): the session's
  local-executor tool provider (`RegisterToolProvider("local", ...)`),
  which turns each script into a `local_<name>` `gateway.Tool`, and the
  approval queue.
- The gateway those tools are served from: the tool registry, refusal
  and timeouts in [`old/gateway/`](gateway/), with the call audit in
  [`old/gateway/audit/`](gateway/audit/).
- `silo approve`: [`old/cmd/silo/approve.go`](cmd/silo/approve.go), over
  the session's control socket ([`old/control/`](control/)).

**Custom agent CLI mapping** (cli-mapping.md §2.4):

- [`old/agentcli/custom/custom.go`](agentcli/custom/custom.go), next to
  the shared `agentcli` interfaces in [`old/agentcli/`](agentcli/).

## Module and paths

`old/` is its own Go module, `github.com/bwsalmon/silo/old` (it was
`github.com/bwsalmon/silo/v2`); the move changed only import paths. The
hub's module at the repository root stops at this directory's `go.mod`.
`old/internal/tunnel/mux` and `old/events` have copies in the root
module, at `internal/tunnel/mux` (the stream multiplexer the rendezvous
carries) and `events` (the event schema `hub/live` reads); each module
keeps its own.

The documents under [`docs/`](docs/) (`design.md`, `cli-mapping.md`,
`events.md`, `grain.md`, `examples/`) are unchanged from `v2/docs/` and
still name paths as `v2/<package>`; read those as `old/<package>`. The
hub's design moved to [`../docs/hub.md`](../docs/hub.md). Silo v1, which
they mention, is deleted from the repository and is in its git history.

grain CI does not test this module by default, and it is not kept
green. `.grain/workflows/old.yml` still builds, vets and tests it, but
runs only when dispatched by hand (`workflow_dispatch`), for checking a
piece while porting it.

## What it was

v2 runs one agent CLI in a chosen location, with access to a declared
set of MCP tools, credentials, cloud permissions, git access and local
scripts, and nothing else. A silo is a `Silofile`, or a folder holding
one; `silo run` carries it out once.

### Design

- [`docs/design.md`](docs/design.md): the design, organised around the
  user journeys (`silo create`, a Google project, a remote agent with
  narrow local reach, existing MCP servers), with the staged
  implementation plan.
- [`docs/cli-mapping.md`](docs/cli-mapping.md): how each setting maps
  onto the real `claude`, `codex` and `agy` flags.
- [`docs/grain.md`](docs/grain.md): grain as silo's first client, and
  its migration path.
- [`../docs/hub.md`](../docs/hub.md): the hub, which is now the whole
  of this repository.
- [`docs/examples/`](docs/examples/): example silo folders.

### Commands

```sh
go run ./cmd/silo run [--dry-run] [--json] [--prompt text] [--caller-mcp name=url ...] <Silofile|dir>   # design §3
go run ./cmd/silo run --detach [--label k=v ...] <Silofile|dir>   # grain.md §2.7: prints the session id
go run ./cmd/silo attach [--from seq] [--json] <session>          # replay and follow its events
go run ./cmd/silo ls [--label k=v ...] [--json]                   # the running sessions
go run ./cmd/silo approve <session> [--deny] [<call-id>]   # design §12.3
go run ./cmd/silo gc [--host <host>]        # what crashed sessions left (there)
go run ./cmd/silo gc [--project p [--account email]]   # and GCP accounts past their ttl
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
the Silofile delivers a credential in its place.

An entry with `where: remote` (design §13.2) lives on the ssh agent
host. A `command:` server runs there as its own channel of the master
(`ssh -S <ctl> -T <host> -- ...`), speaking MCP over that channel's
stdin and stdout, in the remote home directory, with the remote login
environment filtered the same way and the credentials delivered to it;
its environment goes in a 0600 env file in the remote session
directory, which the command's preamble sources and deletes, so no
value is in a remote argv. A `url:` server is reached through a local
forward on the master (`-O forward -L`) whose local end is a Unix
socket beside the control socket. A `from:` entry's file is read on the
agent host. Teardown closes the channel, waits 5 s for the server's
process group to go and then kills it over the master; a dropped
connection makes every remote server's tools fail with a tool error
naming the server and the host, and the session exits 75.
`silo run --dry-run`
validates, then checks what the session needs without changing
anything, one line per check, and exits 69 if any check fails. So far it
checks MCP servers (a remote one on its own BatchMode connection to the
agent host: its config read there, its binary found on its remote
`PATH`), the ssh location, and for granule sandboxes the granule binary,
`granule doctor` (KVM), that each VM image can be pulled, and whether
each Granulefile is built already or will be built at run; for
granule sandboxes with `backend: {k8s: ...}`, that some node offers
`devic.es/kvm` (and `devic.es/tun` with `network: open`).

`silo validate` decodes the Silofile strictly (design §2.1), resolves
every path inside the silo folder (§2.2) and checks names,
cross-references, files, `remote_permissions` against the CLI, and
credential destinations (§2.4). A value v2.0 reserves but does not ship
yet (`lifetime: persistent`, `backend: {k8s: ...}`, ...) is refused with an
error naming the stage that adds it. Errors name the file, line and YAML
path, and exit 65; bad usage exits 64 (§3.3). Local scripts get the
snapshot's refusals as errors (a symlink out of the folder, a file or
directory writable by group or others, a file another user owns) and
best-effort warnings on the obvious holes of §12.4: `eval`, `sh -c`
with a positional parameter, an unquoted `$@`/`$*`, `xargs`, a wrapper
that hands its arguments to an interpreter, and a script that is
itself one. A granule sandbox's `granulefile:` is checked with `granule
granulefile check`, and refused if its target stage builds a container
image; a container sandbox's `build:` file that uses granule's `VM`
instruction is refused too (design §14, journey 10). Without granule
installed the Granulefile is left unchecked, with a warning.

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

`silo gc` also deletes the invoking user's journey 2 service accounts
that have gone unused past their `ttl` (design §11.5), as the user's own
gcloud login: those listed in `$XDG_STATE_HOME/silo/accounts.json`, and
with `--project p` any `silo-*` account in `p` whose description names
the user. `--project p --account <email>` deletes one of the user's
accounts whatever its ttl. Another user's account is never touched. GCP
is not contacted when local state lists no account and `--project` is
not given. Setting accounts up (`gcp.Client.Setup`) is in the `gcp`
package; `silo run` uses it once stage 2b lands.

`--caller-mcp name=url` (or `silo.Options.CallerMCP`) adds a
caller-provided MCP server, on HTTP or a Unix socket, whose tools the
gateway serves as `name__<tool>`; its token is read from
`$SILO_CALLER_MCP_TOKEN_<NAME>` (`docs/events.md`, "Caller-provided MCP
servers").

The schema `silo schema` prints is generated from `silofile`'s Go types
into `silofile/silofile.schema.json` and embedded in the binary. After
changing a type, run `go generate ./silofile` and
`go test ./cmd/silo -update`; tests fail until both are current.

### Layout

| Package | What it is |
| --- | --- |
| `silofile` | The Silofile: types, strict decoding, folder resolution, validation, JSON Schema. |
| `cmd/silo` | The `silo` command. |
| `session` | One `silo run` session (design §3): setup steps and tool providers (registries), teardown, `session.json`, signals, exit codes, the control socket, the local executor's provider and approvals, `silo gc`'s local part. |
| `location` | The `Location` and `Agent` interfaces (design §5.1). |
| `location/local` | The CLI as a child process with a scrubbed environment (design §5.2). |
| `location/ssh` | The CLI on a remote host (design §5.3): the system `ssh` behind a small `Transport` (so stage 10's IAP can replace it), the master connection, the probe, the loopback-only reverse forward to the gateway, staging with `umask 077` and `cat >`, the env file, exit 75 on a dropped connection, the remote half of `silo gc`, and the `--dry-run` checks. `Run` and `ForwardLocal` share the master, and `mcp.go` is what remote MCP servers need of it: `Host` (the remote login environment, files and `PATH` lookups, over the master or, for `--dry-run`, on a connection of its own), `LaunchMCP` (the env file, the preamble, and the reap of the server's process group) and `ForwardMCP` (the `-L` forward and an HTTP client that dials it). |
| `location/k8s` | The CLI in a pod (design §5.4, journey 8), driven with the system `kubectl`: one pod per session labelled with its id, the Secret holding the CLI's credentials (mounted into the agent container, owned by the pod), the probe, staging and the CLI with `kubectl exec` (`-t` for interactive), the gateway through the tunnel sidecar over `kubectl port-forward` (reconnected if it drops), `silo gc --k8s`, and the `--dry-run` checks (context, namespace, permissions, and a short-lived pod that pulls both images). |
| `internal/tunnel` | The gateway tunnel (design §5.4): the sidecar's two loopback listeners, silo's control connection with its hello, and the splice of each CLI connection to the gateway as a stream. `internal/tunnel/mux` is grain's `pkg/tunnel/mux`, copied. |
| `cmd/silo-tunnel` | The tunnel sidecar's binary, and the `Dockerfile` its image is built from. |
| `agentcli` | Per-CLI mappings onto argv, env and staged files: `claude`, `codex` (with the reviewed-version gate for `none`), `custom`. Golden files in `agentcli/testdata`. |
| `mcp` | MCP plumbing: JSON-RPC dispatch, the authenticated loopback server (bearer token, Host and Origin checks), and client connections over Streamable HTTP, HTTP+SSE and stdio (`Conn`). |
| `mcp/sources` | Reads `from: <source>:<name>` server definitions from `~/.claude.json`, `.mcp.json` files, codex's `config.toml` and agy's `mcp_config.json`, and the inline-secret test of call 25. |
| `gateway` | The session's MCP server (design §4): tool registry, refusal, timeouts, and the sandbox tools `sandbox_exec`, `sandbox_read_file`, `sandbox_write_file`, `sandbox_list_dir`. |
| `gateway/proxy` | Fronts MCP servers in the gateway (design §13.4): launch or connect, the 30 s start, allowlists, dead-server errors, `restart: on-failure`, `list_changed`, and SIGTERM-then-SIGKILL teardown of each launched server's process group. A `Spec` may carry a `Dial` function or an HTTP client (a Unix socket's), and blocking tools; `Prelaunch`, `Reap` and `Gone` hooks front a server on the ssh agent host the same way; stage 6b's caller-provided servers are fronted the same way. |
| `gateway/audit` | `audit.jsonl`: a start line before each call, an end line after, credentials redacted. |
| `sandbox` | The `Backend`/`Sandbox` interfaces (design §7.3), specs from a Silofile, and the images in `.silo/lock.json`. |
| `sandbox/k8s` | `kind: granule` with `backend: {k8s: ...}` (journey 9): one unprivileged pod per sandbox from the granule VM image, given `/dev/kvm` by granule's KVM device plugin, reached with `kubectl exec ... granule exec`, labelled like the k8s location's pods so `silo gc` removes them; the git proxy carried into the guest over vsock through a tunnel sidecar; `granulefile:` images built here and pushed to `push_to:`; the `--dry-run` device check. |
| `sandbox/container` | Docker or Podman containers; `kind: gvisor` is the same with `--runtime=runsc`. A session's internal network, which sandboxes with `repos` join to reach the git proxy. |
| `sandbox/granule` | `kind: granule` (design §7.3, journeys 7 and 10): microVMs driven through the `granule` binary (`vm create`, `wait`, `exec`, `delete`), never its Go packages. A `granulefile:` is checked with `granule granulefile check`, built with `granule granulefile build` into `silo-granulefile:<key>`, and booted from there while its content key (the Granulefile, its directory, target and args) is unchanged. `Guest` is what both granule backends share: the environment file, the kill of a cancelled command, the file tools and `DeliverFile`, all through `granule exec`. |
| `localexec` | The local executor (design §12): script snapshot, refusals, `silo validate`'s warnings, one tool per script run with no shell, and the approvers (terminal prompt, and the queue `silo approve` answers). |
| `control` | The session's control socket: JSON-RPC 2.0 over a Unix socket, a method table later stages add to, and a client. |
| `credentials` | Sources `env`, `file`, `ambient` (`gh`, `git-credential`, `gcloud`), and sandbox destinations. `store` comes later. |
| `gcp` | Journey 2's accounts (design §11): the spec and its `silo-` name, the description, local state (`accounts.json`), the setup checks (`Check`, which `silo run --dry-run` calls), create/verify/reuse and resume (`Ensure`), the propagation wait (`WaitReady`), and `GC`. The APIs are behind the `API` interface: `Google` (with `Ambient`, the user's ADC or gcloud login) is the real one. |
| `internal/fakegcp` | An in-memory `gcp.API` for one project, answering with Google's error codes and etag conflicts. |
| `gitproxy` | The git smart-HTTP proxy (design §9), copied and adapted from grain's `pkg/gitproxy` behind a `Policy` interface: `StaticPolicy` for a Silofile's `git:` entries, ref patterns, the fast-forward proof, one audit entry per request. Grain imports it. `silo run` starts one per session whose sandboxes have `repos` (stage 7b, design §9.3): on the session's internal network, a token per sandbox, the repos cloned into each workdir through it. |
| `internal/safepath` | Path confinement, copied from grain's `pkg/safepath`. |
| `internal/procgroup` | Process-group cancellation, copied from grain's `pkg/procgroup`. |
| `internal/lockfile` | `.silo/lock.json`: one key per part of silo (`images`, `mcp`), each written without disturbing the others. |
| `internal/mcptest` | A test MCP server over stdio or HTTP whose tool list can change, used by the proxy and session tests. |
| `internal/githubsim` | A stand-in for github.com's git endpoints (who may fetch and push, with GitHub's statuses) over `git http-backend`, copied from grain's `pkg/github/githubsim`; the session's git proxy tests run against it over HTTPS. |
| `internal/headtail` | Bounded command output, adapted from grain's `pkg/mcp/exec_output.go`. |

### The tunnel sidecar image

The `k8s` location (design §5.4) runs `cmd/silo-tunnel` beside the CLI
in each session's pod. A granule sandbox pod with `repos` runs it too,
listening on the VM's vsock socket for the git proxy. silo copies the
same binary into the guest (`-self`), where it forwards git's
connections over vsock (`-guest-forward`, design §7.3). Its image is
built from this module:

```sh
cd old
docker buildx build --platform linux/amd64,linux/arm64 \
  -f cmd/silo-tunnel/Dockerfile -t ghcr.io/bwsalmon/silo-tunnel:v2 --push .
```

A maintainer publishes it to `ghcr.io/bwsalmon/silo-tunnel:v2` (the
default `location.k8s.tunnel_image`) with that command, by hand, when
`cmd/silo-tunnel` or `internal/tunnel` changes; the control
connection's hello line carries a version, so an old sidecar refuses a
silo that speaks a newer protocol rather than misbehaving. Anyone can
build the same image and push it where their cluster pulls from, and
name it with `location.k8s.tunnel_image`, or for sandboxes with
`backend.k8s.tunnel_image`. `silo run --dry-run` checks that the
cluster can pull it for the location. The `-self` and `-guest-forward`
modes and Unix-socket listeners came with stage 11b. A published image
built before that cannot carry the git proxy to a sandbox.

### Building and testing

```sh
cd old
gofmt -l .
go build ./... && go vet ./... && go test -race ./...
```

`.grain/workflows/old.yml` runs the same in grain CI, but only when
dispatched by hand; no push or pull request runs it.

The gateway's, `sandbox/container`'s and `session`'s tests create real containers,
with `busybox:1.37`, and run the same suite with `kind: gvisor`. They
skip where there is no Docker or Podman, or no runsc registered as a
runtime; CI installs runsc and sets `SILO_V2_REQUIRE_DOCKER=1` and
`SILO_V2_REQUIRE_RUNSC=1`, which turn those skips into failures.

`session`'s tests use the test binary itself as a fake `claude`: it
answers `--version`, reads the staged MCP config, calls the gateway and
exits as told. The library's tests (`internal/fakecli`) add a claude that
answers stream-json messages on stdin, and a fake `codex` whose turn
can be resumed, for injection. No real `claude` or `codex` is needed (the live tests
below use them). The same binary is also the MCP server those tests launch: with
`SILO_MCPTEST=stdio` it serves `internal/mcptest` over stdio.

`location/k8s`'s tests run against a kind (Kubernetes in Docker)
cluster, created for the run (or the one `SILO_V2_KIND_CLUSTER` names),
into which they load two images: busybox with
`location/k8s/testdata/fakecli` as its `claude`, and `cmd/silo-tunnel`
on `scratch`. They run headless and interactive (on a pty) sessions in
which the fake CLI reaches the gateway through the tunnel and calls
`sandbox_exec` on a container sandbox on the invoking machine, and
check that its credentials arrive through the Secret and not the pod's
spec, that teardown deletes the pod and the Secret, that `silo gc`'s
k8s part removes a leaked pod and keeps a live one, and the `--dry-run`
checks. They need Docker, `kind` and `kubectl`, which the sandbox image
carries; `SILO_V2_REQUIRE_DOCKER=1` makes their absence a failure.

`sandbox/k8s`'s unit tests compare pod specs with golden files and
check the no-network pod against the `restricted` Pod Security
Standard. `TestGranulePod` boots a real granule sandbox pod on a kind
cluster whose node has the host's `/dev/kvm`. It loads granule's
device plugin and grain's guest image (`SILO_V2_GRANULE_IMAGE`
overrides it) into the cluster, puts the sandbox in a namespace that
enforces `restricted`, and checks exec, exit codes, stdin, the
environment, that the guest has no network, the file tools,
`DeliverFile`, a cancelled exec, `Find`, `kubectl logs` and `Destroy`.
It also clones a repository through a git proxy on the test's loopback
into the guest, which has no network. The clone runs over the guest's
vsock, a locally built tunnel sidecar and `kubectl port-forward`.
`sandbox/granule`'s `TestPush` pushes an image to a `registry:2`
container and checks that it comes back by digest.
Loading an 8 GB guest image into kind takes about two minutes and
twice the image's size in disk, so the test runs only with
`SILO_V2_REQUIRE_K8S_KVM=1`, which also makes missing Docker, kind,
kubectl or `/dev/kvm` a failure. grain CI sets it. The kind node's udev gives `/dev/kvm` its own kvm
group, which changes it on the host too, so the test gives it back.

`location/ssh`'s tests run against a real `sshd` in a Docker container
(Debian's `openssh-server`, built on first use as `silo-v2-sshd-test`)
with `location/ssh/testdata/fakecli`, built statically, as its `claude`,
and `location/ssh/testdata/mcpserver` (`internal/mcptest` as a binary)
as `/usr/local/bin/mcpserver`.
They cover the probe, an unknown host key, the loopback-only forward
(and the refusal of a server whose `GatewayPorts yes` widens it),
headless and interactive runs (on a pty), exit codes, a killed master
(exit 75), `silo gc --host`, that no secret is in any remote argv, and
journey 3 end to end with `docs/examples/remote-scripts`; and remote MCP
servers: a stdio server's allowlisted tools through the gateway, a
`url:` server through `-L`, `from: claude-user:x` read from the remote
`~/.claude.json`, no credential in a remote argv, the env file deleted
and the server stopped (and reaped after the grace when it ignores the
closed channel), and a killed master turning calls into tool errors
and the session into exit 75. They skip
without Docker unless `SILO_V2_REQUIRE_DOCKER` is set.

The live tests run the real `claude` and `codex` against a model,
through `silo run`, to check `docs/cli-mapping.md`'s assumptions (its
§4). They spend tokens, so they are not part of CI and skip unless
`SILO_V2_LIVE_CLI=1` is set (and without the CLI on `PATH`, or its key):

```sh
cd old
npm i -g @anthropic-ai/claude-code@2.1.197 @openai/codex@0.162.1
SILO_V2_LIVE_CLI=1 ANTHROPIC_API_KEY=... OPENAI_API_KEY=... \
  go test ./cmd/silo ./agentcli/codex -run Live -v -count=1
```

silo hands each CLI its key through `agent.auth: {credential: ...}`.
`ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL`, when set, are delivered to
the CLI as well, for a key that belongs to a proxy (grain's
`claude-proxy` and `openai-proxy` capabilities).
`SILO_V2_CLAUDE_MODEL` and `SILO_V2_CODEX_MODEL` choose the models.
`agentcli/codex`'s `TestLiveCodexNoneIsolation` uses no model and no
key: it runs the codex binary (`SILO_V2_CODEX`, or `codex` on `PATH`)
with silo's `remote_permissions: none` configuration against a stub
Responses API, logs the tools codex advertises and checks that every
native tool call is refused. It is the probe for reviewing a new Codex
version.

`gcp`'s tests run against `internal/fakegcp`. `gcp/live_test.go`
sets up, reuses and garbage-collects a real account when
`SILO_V2_LIVE_GCP=1`, as the ambient login, in `GOOGLE_CLOUD_PROJECT`
(default `bsalmon-fun-sandbox`), granting `roles/logging.viewer`; the
login needs `roles/iam.serviceAccountAdmin`,
`roles/resourcemanager.projectIamAdmin` and `roles/logging.viewer` on
the project. It is not part of the CI gate.
[`deploy/terraform/gcp-agent-pool`](deploy/terraform/gcp-agent-pool/README.md)
grants those roles to a grain `gcp-key` pool, or creates such a pool.

`gitproxy`'s tests clone and push with the real `git` binary, through
the proxy, to `git http-backend` over repositories on disk. They skip
without git; CI sets `SILO_V2_REQUIRE_GIT=1`. `session`'s git tests run
a container sandbox (the `alpine/git` image) that clones and pushes
through a session's proxy to `internal/githubsim` over HTTPS.
`TestGitProxyLiveGitHub` does the same against real GitHub, only with
`SILO_V2_LIVE_GITHUB=1` and the repository and token grain's
`github-sandbox` capability places in `/home/debian`; it is not part of
CI.
