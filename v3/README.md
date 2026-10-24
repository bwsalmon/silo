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

A fresh silo starts with two agents already connected: `root` (user
`silo-root`), the human-gated agent that changes the system, and `main` (user
`silo-user`), an unprivileged coding agent with internet access, ready to use
the moment you open the UI. `main` is root's child. Messages between agents
only ever go from a parent to the children it registered, so a child can never
push instructions into its parent or into root.

silo v3 is not a new OS and does not manage the machine's packages or config.

The design is [docs/design/silo.md](docs/design/silo.md). All v3 code lives
under `v3/`, as the Go module `github.com/bwsalmon/silo/v3`; nothing else in
this repository belongs to v3.

What exists so far:

- `cmd/silo`: the single `silo` binary (`silo gateway`, `silo call`).
- `gateway/`: the root daemon: agent registration with single-use tokens
  for `silo connect`, the two standing agents (`root` and `main`), the
  parent/child tree with cascading stop, parent → child messages, page
  links, secret requests, the audit log, and the JSON-RPC API on
  `/run/silo/gateway.sock`. See [docs/api.md](docs/api.md).

CI (`.grain/workflows/v3.yml`) runs gofmt, build, vet and race tests for this
module on every push.

```sh
cd v3 && go vet ./... && go test ./...
```
