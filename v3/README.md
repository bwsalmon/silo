# silo v3

silo v3 is a small, unopinionated layer you install on an ordinary Linux
machine (Debian first, then any systemd + cgroup v2 distribution) to host coding
agents on it. A root-owned gateway daemon runs each agent as its own Unix user
with cgroup limits, and holds every model and cloud credential so agents never
see them. Agent actions that need root go to a human for approval: the human
sees the exact commands and diffs, and the approval is bound to a hash of them.
A session bridge drives the stock Claude Code, Codex and Gemini CLIs over the
Agent Client Protocol, and a web UI shows every agent as a chat session, along
with the approval queue and the agent schedules. `silo up` deploys the whole
thing into a dedicated GCP project behind IAP with a single command.

silo v3 is not a new OS and does not manage the machine's packages or config.

The design is [docs/design/silo.md](docs/design/silo.md). All v3 code lives
under `v3/`, as the Go module `github.com/bwsalmon/silo/v3`; nothing else in
this repository belongs to v3.

What exists so far:

- `cmd/silo`: the single `silo` binary (`silo gateway`, `silo call`).
- `gateway/`: the root daemon: agent users, agents as sandboxed transient
  systemd services, the parent/child tree, the audit log, and the
  JSON-RPC API on `/run/silo/gateway.sock`. See [docs/api.md](docs/api.md).

```sh
cd v3 && go vet ./... && go test ./...
```
