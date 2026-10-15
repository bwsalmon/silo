# Silo

Silo is the **collection hub that links standalone Grain nodes
together**. The design is [`docs/hub.md`](docs/hub.md).

A **Grain node** is a single-tenant execution engine on one workstation
or VM. It holds that machine's checkouts, runs the sandboxes and the
agent, owns its own merge queue, and is complete on its own. It is the
Policy Enforcement Point: it checks the claims on each incoming task
against its own local policy.

**Silo** is the multi-tenant coordination plane above the nodes, and
the Policy Decision Point that signs task claims. It handles:

- **Identity:** who a human (IAP or OIDC sign-on) or a node (a node
  token) is.
- **Teams:** which team owns which repositories, and so which nodes may
  serve them and who may see their tasks.
- **The backlog:** tasks not yet placed on a node, and dispatch to a
  node that can run them (`hub/tasks`).
- **Routing and events:** reaching a node behind NAT or a firewall over
  the connection the node opened, and relaying its events to viewers
  (`hub/live`).

Silo **holds no source at rest, runs no sandboxes and makes no git
checkouts**. Once a task is placed, the node's grain owns it; the hub
only mirrors its lifecycle (hub.md §1.1).

## Layout

The repository root is one Go module, `github.com/bwsalmon/silo`.

| Path | What it is |
| --- | --- |
| `hub/registry` | The in-memory node directory (hub.md §3): registration, ownership of node IDs, status, slot claims. |
| `hub/rendezvous` | The node's outbound WebSocket to the hub and its reverse tunnel (hub.md §4): authentication bound to a node ID, admission, heartbeats, streams both ways, the per-caller filtered directory. |
| `hub/identity` | Human sign-on (hub.md §5.1): IAP assertion and OIDC ID-token verification against the issuer's JWKS, and the OIDC relying party (code flow with PKCE, signed session cookie). |
| `hub/directory` | The team directory in SQLite or PostgreSQL (hub.md §5.2, §5.3): teams, members and owning teams, node tokens stored as hashes, `Admit`, per-team visibility, and its HTTP API. |
| `hub/tasks` | The task backlog in SQLite or PostgreSQL, its intake API, and the dispatcher (hub.md §6). The dispatcher places tasks on nodes per team, offers each one over a task stream, requeues a refused task without the node that refused it, marks a task lost when its node goes away mid-run and resumes it when the node returns, and cancels tasks down the stream. |
| `hub/live` | Live events (hub.md §7): the event streams nodes open per task, the classification by schema (lifecycle and conversation stored, the token stream in an in-memory ring, node-only events kept as references), fan-out over SSE and WebSocket from a cursor, and the view-through to a node's diffs and tool logs. |
| `events` | The schema of the events a node streams to the hub for each task (hub.md §7.1), copied from the retired runner's `old/events`. |
| `internal/tunnel/mux` | The stream multiplexer the rendezvous carries, copied from grain's `pkg/tunnel/mux`. |
| `internal/pgtest` | Throwaway PostgreSQL servers for tests. |
| `docs/hub.md` | The hub's design, and its stages (§9): what is built and what comes next. |
| `docs/hub-interface.md` | The task interface: the messages a Grain speaks to the hub, and all the hub depends on in a Grain. |
| `docs/ui.md` | A proposal, not decided: the hub's human web UI (hub.md §9, stage 2.6). |
| `old/` | Retired code, not part of the hub: the former single-run runner, kept for porting into bwsalmon/grain ([`old/README.md`](old/README.md)). It is a separate Go module, and neither imports the other. |

## Building and testing

```sh
gofmt -l hub internal events
go build ./... && go vet ./... && go test -race ./...
```

grain CI runs the same in `.grain/workflows/hub.yml`. `hub/directory`'s
store tests run against PostgreSQL as well as SQLite where PostgreSQL's
server binaries are installed; CI installs them and sets
`SILO_REQUIRE_POSTGRES=1`, which turns a missing server into a
failure.
