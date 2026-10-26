# silo v3

silo v3 is a small, unopinionated layer you install on an ordinary Linux
machine (Debian first, then any systemd + cgroup v2 distribution) to host coding
agents on it. Agents run wherever their creator puts them (a systemd unit as
their own Unix user, a container, a microVM, a subprocess) and join the Silo
Hub with `silo connect`. A root-owned gateway daemon registers every agent,
keeps the parent/child tree, and holds every model and cloud credential so
agents never see them. Agent actions that need root go to a human for
approval: the human sees the exact commands and diffs, and the approval is
bound to a hash of them. A session bridge drives the stock Claude Code, Codex
and Gemini CLIs over the Agent Client Protocol, and a web UI shows every agent
as a chat session, along with the approval queue, the agent schedules, the
page links agents publish, and the secrets they ask for. `silo up` deploys the
whole thing into a dedicated GCP project behind IAP with a single command.

A fresh silo starts with two standing agents: `root` (user `silo-root`), the
human-gated agent that changes the system, and `main` (user `silo-user`), an
unprivileged coding agent with internet access, ready to use the moment you
open the UI. `main` is root's child. By default, `root`, `main`, and any newly
registered agent require human approval on anything they do (`permissions: "ask"`
default). Parents drive children using the CLIs' own built-in `--resume` / MCP /
ACP session mechanisms.

silo v3 is not a new OS and does not manage the machine's packages or config.

The design is [docs/design/silo.md](docs/design/silo.md). All v3 code lives
under `v3/`, as the Go module `github.com/bwsalmon/silo/v3`; nothing else in
this repository belongs to v3.

## Quickstart

On a Debian 12 or 13 box with systemd, as root, from a checkout (building
needs Go >= 1.24; `--release [VERSION]` downloads a release binary instead):

```sh
git clone https://github.com/bwsalmon/silo && cd silo/v3
sudo ./deploy/install.sh
```

[`deploy/install.sh`](deploy/install.sh) is idempotent: run it again to
upgrade. It installs `/usr/local/bin/silo`, Node.js and the Claude Code,
Codex and Gemini CLIs with their ACP adapters into `/opt/silo/cli` at the
versions pinned in [`deploy/versions.env`](deploy/versions.env) (`--no-cli`
skips them), creates the users `silo` (bridge and UI), `silo-root` and
`silo-user`, `/etc/silo`, `/var/lib/silo`, `/var/log/silo` and `/run/silo`,
and starts four units:

| unit | runs |
|---|---|
| `silo-gateway.service` | `silo gateway` as root: the registry, `run_as_root`, the audit log |
| `silo-ui.service` | `silo ui` as `silo`: the session bridge and web UI on `127.0.0.1:7070` |
| `silo-root.service` | `silo connect` as `silo-root`: the `root` agent |
| `silo-user.service` | `silo connect` as `silo-user`: `main`, the default user agent |

It ends by printing the UI login token and the tunnel to reach the UI:

```sh
ssh -L 7070:127.0.0.1:7070 you@your-box     # then open http://localhost:7070
```

The token is kept in `/etc/silo/ui-token` (root only);
`install.sh --rotate-token` replaces it. Give the CLIs model credentials
before chatting (the credential proxy that will hold them is not built yet).
`sudo ./deploy/uninstall.sh` removes silo and keeps its users, state and
audit log; `--purge` removes those too.

### The two agents you start with

Both are connected the moment the install finishes, and both default to
`permissions: "ask"`, so their tool calls and anything that creates new
authority wait for you in the UI:

- **`root`** (Unix user `silo-root`) changes the system. It is not uid 0:
  every privileged change is a `run_as_root` batch you approve after seeing
  exactly what will run, bound to the batch's hash and recorded in the
  hash-chained audit log (`/var/log/silo/audit.jsonl`). From a shell:
  `sudo silo approvals list|show|approve|reject`.
- **`main`** (Unix user `silo-user`) is an ordinary unprivileged coding
  agent, root's child: open its chat and start working. It may register its
  own child agents, each of which you approve first.

### Connecting more agents

An agent is anything that dials the hub with `silo connect`. Its creator
(you, or an agent allowed to register children, through the
`register_agent` MCP tool) registers it and gets a single-use token back;
`silo connect` presents the token from wherever the agent should run, starts
the CLI in ACP mode and joins it to the hub:

```sh
# register (as the human; agents use register_agent)
sudo silo call agent.register '{"name":"web","runtime":"claude","location":{"kind":"container"}}'

# a systemd unit on the box: like silo-user.service, with User= the agent's
# user and the token in LoadCredential=token:/path/to/token, then
silo connect

# a rootless container
podman run -d --name web -e SILO_AGENT_TOKEN -e SILO_HUB_URL=unix:///run/silo/bridge.sock \
  -v /run/silo/bridge.sock:/run/silo/bridge.sock img silo connect -- claude-agent-acp

# a microVM, a subprocess or another host: the same, over the bridge's WebSocket
silo connect --url ws://hub:7070/connect --token "$SILO_AGENT_TOKEN" -- gemini --experimental-acp
```

With no command after `--`, `silo connect` runs the agent's registered
`runtime` preset (`/etc/silo/clis.d/*.toml`, which `install.sh` points at the
pinned CLIs). Stopping an agent from the UI (or `stop_agent`) tells its
`silo connect` to stop the CLI.

## End-to-end test

[`e2e/`](e2e) checks milestone 1 against a running stack: both standing
agents connected, a streamed chat with `main`, a `root` batch approved, run
and audited, and a child registered, started with `silo connect --token`,
chatted with and stopped. It uses a scripted ACP agent, so it needs no model
credentials (`SILO_E2E_REAL_CLI=gemini` uses the real Gemini CLI). It runs as
root:

```sh
sudo -E go test -tags e2e -v ./e2e                     # isolated stack in a temp dir
sudo -E SILO_E2E_INSTALL=1 go test -tags e2e -v ./e2e  # via deploy/install.sh on this VM
```

## What exists so far

- `cmd/silo`: the single `silo` binary (`silo gateway`, `silo bridge`,
  `silo ui`, `silo connect`, `silo mcp`, `silo page`, `silo secret`,
  `silo call`, `silo root`, `silo approvals`, `silo instructions`).
- `gateway/`: the root daemon: agent registration with single-use tokens
  for `silo connect`, the two standing agents (`root` and `main`, both
  defaulting to `permissions: "ask"`), the parent/child tree with cascading
  stop, page links, secret requests, agent escalation requests, the audit log,
  and the JSON-RPC API on `/run/silo/gateway.sock`. See [docs/api.md](docs/api.md).
- `bridge/`: the session bridge and HTTP/WebSocket hub: owns ACP sessions from
  `silo connect`, persists JSONL transcripts under `/var/lib/silo/sessions/`,
  holds pending permission prompts under `permissions: "ask"`, reverse-proxies
  `/p/<slug>/...` page links, routes human-provided secrets to `silo connect`
  (`0600` files) without exposing secret values in LLM contexts or logs, and
  serves the `silo-mcp` stdio server.
- `acp/`: ACP JSON-RPC 2.0 protocol types and `/etc/silo/clis.d/` preset loader.
- `instructions/`: shipped default `/usr/share/silo/root.md` and `/etc/silo/root.d/`
  assembler.
- `deploy/`: `install.sh`, `uninstall.sh` and the pinned `versions.env`.
- `e2e/`: the milestone-1 end-to-end test and its scripted ACP agent.

CI (`.grain/workflows/v3.yml`) runs gofmt, build, vet and race tests for this
module on every push.

```sh
cd v3 && go vet ./... && go test ./...
```
