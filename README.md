# Silo

Silo is the **collection hub that links standalone Grain nodes
together**. The design is [`docs/hub.md`](docs/hub.md).

A **Grain node** is a single-tenant execution engine on one workstation
or VM. It holds that machine's checkouts, runs the sandboxes and the
agent, owns its own merge queue, and is complete on its own. It is the
Policy Enforcement Point: it checks the claims on each incoming task
against its own local policy.

**Silo** is the multi-tenant coordination plane above the nodes, and
the Policy Decision Point that puts claims on each task (it does not
sign them yet: hub.md §1.2). It handles:

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
| `hub/directory` | The team directory in PostgreSQL (SQLite in development and tests; hub.md §5.2, §5.3): teams, members and owning teams, node tokens stored as hashes, `Admit`, per-team visibility, and its HTTP API. |
| `hub/tasks` | The task backlog in PostgreSQL (SQLite in development and tests), its intake API, and the dispatcher (hub.md §6). The dispatcher places tasks on nodes per team, offers each one over a task stream, requeues a refused task without the node that refused it, marks a task lost when its node goes away mid-run and resumes it when the node returns, and cancels tasks down the stream. |
| `hub/live` | Live events (hub.md §7): the event streams nodes open per task, the classification by schema (lifecycle and conversation stored, the token stream in an in-memory ring, node-only events kept as references), fan-out over SSE and WebSocket from a cursor, and the view-through to a node's diffs and tool logs. |
| `hub/notify` | The hub's wake-ups and event notices between its pods (hub.md §2.2.4): PostgreSQL `NOTIFY silo_hub`, listened for on a connection of each pod's own. |
| `hub/server` | The hub as one HTTP server over one database (hub.md §2.1): every part above wired together, `/healthz` and `/readyz`, and a graceful shutdown that says goodbye to nodes. |
| `hub/ui`, `ui/` | The hub's web UI ([`docs/ui.md`](docs/ui.md), stage U1): its React source in `ui/`, built into `hub/ui/static/app` and embedded by `hub/ui`, which serves it at `/`. Without a built bundle the hub embeds a placeholder page. |
| `hub/conformance` | The task-interface conformance kit ([`docs/conformance.md`](docs/conformance.md)): it runs a hub, drives a Grain under test through scripted scenarios over the hub's public API, and reports a verdict per scenario. Its conformance workloads are in `hub/conformance/workload`, and `hub/conformance/refgrain` is the reference fake Grain that passes it. |
| `cmd/silo` | The `silo` command: `silo hub serve`, `silo hub migrate` (create the tables as the schema's owner and grant the role the hub serves as its rows), and `silo hub team` and `silo hub token` to bootstrap a hub's database, and `silo conformance` and `silo conformance grain`, the conformance kit and the reference Grain. |
| `Dockerfile`, `deploy/` | The hub's image, its GKE manifests (`deploy/kubernetes/hub`) and terraform (`deploy/terraform/hub`), for GKE behind IAP: the runbook is [`docs/deploy.md`](docs/deploy.md). |
| `events` | The schema of the events a node streams to the hub for each task (hub.md §7.1), copied from the retired runner's `old/events`. |
| `internal/tunnel/mux` | The stream multiplexer the rendezvous carries, copied from grain's `pkg/tunnel/mux`. |
| `internal/pgtest` | Throwaway PostgreSQL servers for tests. |
| `internal/fakeoidc`, `scripts/` | The `grain-interop` CI job (below): `scripts/grain-interop.sh` runs this hub against a real grain, with a fake OIDC provider and a fake GitHub API. |
| `docs/hub.md` | The hub's design, and its stages (§9): what is built and what comes next. |
| `docs/deploy.md` | How to deploy the hub on GKE behind IAP, in `bsalmon-gke-dev`. |
| `docs/hub-interface.md` | The task interface: the messages a Grain speaks to the hub, and all the hub depends on in a Grain. |
| [`docs/conformance.md`](docs/conformance.md) | The conformance kit: how to check that a Grain speaks the task interface, the scenarios, and the report. |
| [`docs/grainfiles.md`](docs/grainfiles.md), [`docs/grainfiles-spec.md`](docs/grainfiles-spec.md) | Grainfiles: Grain's work as plain git in grain folders on ordinary forges. The design, and **The Grainfile Format**, its normative spec, with the schemas in `schemas/grainfiles/`, the `grainlint` validator in `tools/grainfiles/` (the `grainfiles` CI job) and the worked example in `examples/grainfiles/`. |
| `docs/ui.md` | The hub's human web UI (hub.md §9, stage 2.6): the options, the chosen one, its stages, and how to run it locally (§7.1). |
| [`docs/threat-model.md`](docs/threat-model.md) | The threat model of the Grain + Silo system: assets, actors, trust boundaries, threats, the numbered security invariants (`INV-`) that audits and security tests cite, and the audit plan. |
| `old/` | Retired code, not part of the hub: the former single-run runner, kept for porting into bwsalmon/grain ([`old/README.md`](old/README.md)). It is a separate Go module, and neither imports the other. |

## Building and testing

```sh
gofmt -l cmd hub internal events
go build ./... && go vet ./... && go test -race ./...
```

grain CI runs the same in `.grain/workflows/hub.yml`. The hub's store
is PostgreSQL on Cloud SQL in every deployment
([`docs/deploy.md`](docs/deploy.md)); SQLite is for local development
and tests only. The stores' tests (`hub/directory`, `hub/tasks`,
`hub/live`, `hub/registry`, `hub/lease`, `hub/notify`, `hub/server`)
run against a throwaway PostgreSQL (`internal/pgtest`) as well as
SQLite where PostgreSQL's server binaries are installed; CI installs
them and sets `SILO_REQUIRE_POSTGRES=1`, which turns a missing server
into a failure rather than a skip.

`.grain/workflows/ui.yml` builds and tests the web UI (`ui/`, Node 20.19
or later): `npm ci`, `npm run build` (tsc and Vite), `npm test`
(vitest), and then `hub/ui`'s and `hub/server`'s tests with the bundle
embedded. The Go build above needs no Node: without the bundle it
embeds a placeholder page. [`docs/ui.md`](docs/ui.md) §7.1 says how to
run the UI locally.

`.grain/workflows/deploy.yml` builds the image (the UI's bundle
included), validates the
manifests on a kind cluster (`deploy/kubernetes/hub/validate.sh`) and
runs `terraform validate` and `terraform test` (mock providers) in
`deploy/terraform/hub`. Nothing is applied anywhere.

`.grain/workflows/grain-interop.yml` tests this commit's hub against
**real grain**, which the fake Grains in `hub/*`'s own tests can't
stand in for. The job reads bwsalmon/grain through the git proxy
(`capabilities: ["repo-read:bwsalmon/grain"]`) and builds grain's
`main` twice: once as it is, with the silo client it pins, and once
against this commit's hub packages. It runs on every push and pull
request, and takes about two and a half minutes.
`scripts/grain-interop.sh` does the work. It starts `silo hub serve
-dev` on SQLite, signing a test user in through a fake OIDC provider
(`internal/fakeoidc`), and starts a grain daemon pointed at it. The
daemon runs with a local policy and a fake GitHub API
(`scripts/fake-github.py`), and with no sandboxes and no CI, so no
agent or model runs. It then checks the task interface
([`docs/hub-interface.md`](docs/hub-interface.md)) as far as grain
supports it:

- grain builds, and its hub tests pass, against this commit;
- `grain hub login` mints the node token;
- grain shows `online` in `GET /v1/nodes`;
- a task filed through `POST /v1/tasks` is accepted;
- a task for a repository outside grain's policy is refused;
- a cancel on the hub closes the task on grain;
- a task ended on grain reaches the hub as its result.

Stages grain doesn't support yet (events, view-through, merge) are
skipped, and the log says so. A red job is a real mismatch. On a
failure the script reruns the same check against silo's base commit,
and its last lines say which side changed, by both commits' short
SHAs. To run it by hand:
`GRAIN_SRC=/path/to/grain scripts/grain-interop.sh`.

The retired runner in `old/` is not tested by default: its workflow,
`.grain/workflows/old.yml`, runs only when dispatched by hand
(`workflow_dispatch`), and nothing on a push or pull request builds it
([`old/README.md`](old/README.md)).

To run a hub locally, on SQLite and signing callers in from a header
(both refused outside local development, and always on Kubernetes):

```sh
go run ./cmd/silo hub serve -database sqlite:/tmp/hub.db -dev-user-header X-User -admins user:me@example.com
```

`silo hub serve` takes a `sqlite:` database only with `-dev`
(`SILO_HUB_DEV`) or the development header; anything else needs
`-database postgres://...`.
