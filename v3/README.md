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

What exists so far:

- `cmd/silo`: the single `silo` binary (`silo gateway`, `silo bridge`,
  `silo connect`, `silo mcp`, `silo page`, `silo secret`, `silo call`).
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

CI (`.grain/workflows/v3.yml`) runs gofmt, build, vet and race tests for this
module on every push.

```sh
cd v3 && go vet ./... && go test ./...
```
