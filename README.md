# Grain Hub

**Grain Hub** (formerly **Silo**; the repository and Go module remain
`github.com/bwsalmon/silo` until code migration tasks run) is the
**multi-tenant coordination plane that links standalone Grain
deployments together**. The design is [`docs/hub.md`](docs/hub.md).

A **Grain deployment** (a **node** as the hub sees it) is a standalone,
single-tenant execution engine on a workstation, Cloudtop, or VM. It
owns its own backlog, git checkouts, sandboxes, agent sessions, and
merge queue, and is complete on its own. Each Grain deployment is the
Policy Enforcement Point for its own runs: it enforces its own local
policy on which repository and folder sources it trusts and what
capabilities or network access a task may use.

**Grain Hub** sits above single-tenant Grain deployments and is defined
around two core responsibilities:

1. **Multi-tenant UI and reachability across Grain deployments:**
   - **Outbound node reachability (`hub/rendezvous`, `hub/registry`):**
     Each single-tenant Grain deployment dials outbound to the hub over
     WebSocket (`wss://<hub>/v1/nodes/connect`, subprotocol
     `silo-node.v1`), so nodes behind NAT or corporate firewalls need no
     inbound ports.
   - **Human sign-on and access control (`hub/identity`,
     `hub/directory`):** Humans authenticate via Google Cloud IAP or
     OIDC; the team directory governs which users and teams can see and
     interact with which Grain deployments.
   - **Multi-tenant UI, live event mirroring, and view-through
     (`hub/ui`, `hub/live`, `hub/nodeui`):** The hub serves a
     multi-tenant overview UI across all Grain deployments the signed-in
     user can access, mirroring task and process lifecycle and
     conversation events streamed from the nodes (`hub/live`). Diffs,
     tool logs, and each node's own full UI
     (`<node>.nodes.<hub-domain>`, `hub/nodeui`) are streamed on demand
     over the reverse tunnel (`Server.Open` `view` and `http` streams).
     Interactive actions from the UI (chat messages, approvals,
     cancellations) are routed over the tunnel to the Grain deployment
     that owns the task.
2. **Discovery and subscription for `.task.md` and `.process.md`
   Grainfiles:**
   - **Git is the source of truth for cross-repo tasks and processes:**
     Rather than the hub maintaining a parallel task-submission backlog,
     imperative task dispatcher (`offer`/`accept`/`refuse`/`resume`,
     lost-task requeueing), and PDP JWT claim-signing, cross-repository
     tasks and standing processes are authored in git as `.task.md` and
     `.process.md` files ([`docs/grainfiles.md`](docs/grainfiles.md),
     [`docs/grainfiles-spec.md`](docs/grainfiles-spec.md)).
   - **Source authority:** As defined in
     [`docs/grainfiles-spec.md`](docs/grainfiles-spec.md), the
     authoritative source of a `.task.md` or `.process.md` is the URL of
     the git repository and folder where the file lives, and `recipient`
     (a plain `https://` URL or repo-local path) names the deployment
     intended to execute it. Each Grain deployment enforces its own
     local policy on which repository and folder sources it trusts.
   - **Subscriptions (`recipient` + watched repos/processes):** A Grain
     deployment registers and updates its subscriptions with the hub:
     its own `recipient` deployment URL(s), plus any repositories,
     folder paths, or shared `.process.md` / `.task.md` URLs it
     subscribes to.
   - **Indexing and webhook/event notification:** Because Grain
     deployments run behind NAT/firewalls and should not poll every
     repository in an organization, the hub receives forge webhooks
     (`push`, `pull_request`, `merge`) and indexes Grainfile metadata
     across connected repositories (file `https://` URL, commit/ref, and
     parsed front matter: `title`, `recipient`, `requestor`, `draft`,
     `not_before`, `created_at`, `references`, `depends_on`,
     `trigger_with`, `on`, `gate` — metadata only, no source code at
     rest). When a non-draft `.task.md` or `.process.md` matching a
     deployment's `recipient` or subscriptions is added or updated, or
     when a forge event (`push`, `pull_request`, `merge`) matches a
     subscribed `.process.md` trigger, the hub notifies the subscribed
     Grain deployment over its connection (and answers catch-up queries
     on connect/reconnect) so the node can fetch the Grainfile from git
     and execute it directly. When `.task.md` or `.process.md` files
     declare cross-deployment `depends_on` or `trigger_with` references,
     the hub routes completion and trigger notices between the
     subscribed deployments based on the task and process lifecycle
     events nodes report.

Grain Hub **holds no source code at rest, runs no sandboxes, and makes
no git checkouts**. Repository checkouts, diffs, and sandbox logs stay
on the Grain deployment that owns the run; the hub indexes only
Grainfile front-matter metadata and mirrors lifecycle and conversation
events ([`docs/hub.md`](docs/hub.md) §1, §7.2).

## Layout

The repository root is one Go module, `github.com/bwsalmon/silo`
(code identifiers such as `cmd/silo`, `SILO_HUB_*`, and `silo-node.v1`
reflect the current build so `grain-interop` and existing tests remain
unbroken until code migration tasks run).

| Path | What it is |
| --- | --- |
| `hub/registry` | The in-memory node directory (hub.md §3): registration, ownership of node IDs, status, subscriptions, and (in the current build) slot claims. |
| `hub/rendezvous` | The node's outbound WebSocket to the hub and its reverse tunnel (hub.md §4): authentication bound to a node ID, admission, heartbeats, streams both ways, and the per-caller filtered directory. |
| `hub/identity` | Human sign-on (hub.md §5.1): IAP assertion and OIDC ID-token verification against the issuer's JWKS, and the OIDC relying party (code flow with PKCE, signed session cookie). |
| `hub/directory` | The team directory in PostgreSQL (SQLite in development and tests; hub.md §5.2, §5.3): teams, members and owning teams, node tokens stored as hashes, `Admit`, per-team visibility, and its HTTP API. |
| `hub/live` | Live events and view-through (hub.md §7): the event streams nodes open per task/process run, classification by schema (lifecycle and conversation stored, the token stream in an in-memory ring, node-only events kept as references), fan-out over SSE and WebSocket from a cursor, and on-demand view-through to a node's diffs and tool logs. |
| `hub/nodeui` | Per-node UI view-through (`<node>.nodes.<hub-domain>`, hub.md §11): streams a node's own full web UI to authorized viewers over `Server.Open` `http` streams. |
| `hub/ui`, `ui/` | The hub's multi-tenant web UI ([`docs/ui.md`](docs/ui.md)): its React source in `ui/`, built into `hub/ui/static/app` and embedded by `hub/ui`, which serves it at `/`. Without a built bundle the hub embeds a placeholder page. |
| `hub/notify` | The hub's wake-ups and event notices between its pods (hub.md §2.2.4): PostgreSQL `NOTIFY silo_hub`, listened for on a connection of each pod's own. |
| `hub/server` | Grain Hub as one HTTP server over one database (hub.md §2.1): every part above wired together, `/healthz` and `/readyz`, and a graceful shutdown that says goodbye to nodes. |
| `hub/tasks`, `hub/claims` | Legacy imperative task backlog, dispatcher (`offer`/`accept`/`refuse`/`resume`, lost-task requeueing), and JWT claim-signing in the current build (hub.md §6.6), retained alongside `hub/conformance` until the Grainfile subscription migration tasks run. Also carries interactive UI action routing (cancellations, approvals, session messages). |
| `hub/conformance` | The node-interface conformance kit ([`docs/conformance.md`](docs/conformance.md)): it runs a hub, drives a Grain under test through scripted scenarios over the hub's API, and reports a verdict per scenario. Its conformance workloads are in `hub/conformance/workload`, and `hub/conformance/refgrain` is the reference fake Grain that passes it. |
| `cmd/silo` | The `silo` command: `silo hub serve`, `silo hub migrate` (create the tables as the schema's owner and grant the role the hub serves as its rows), `silo hub team` and `silo hub token` to bootstrap a hub's database, and `silo conformance` and `silo conformance grain`, the conformance kit and the reference Grain. |
| `Dockerfile`, `deploy/` | The hub's image, its GKE manifests (`deploy/kubernetes/hub`) and terraform (`deploy/terraform/hub`), for GKE behind IAP: the runbook is [`docs/deploy.md`](docs/deploy.md). |
| `events` | The schema of the events a node streams to the hub for each run (hub.md §7.1), copied from the retired runner's `old/events`. |
| `internal/tunnel/mux` | The stream multiplexer the rendezvous carries, copied from grain's `pkg/tunnel/mux`. |
| `internal/pgtest` | Throwaway PostgreSQL servers for tests. |
| `internal/fakeoidc`, `scripts/` | The `grain-interop` CI job (below): `scripts/grain-interop.sh` runs this hub against a real grain, with a fake OIDC provider and a fake GitHub API. |
| [`docs/hub.md`](docs/hub.md) | Grain Hub's architecture and design around multi-tenant UI/reachability and `.task.md` / `.process.md` Grainfile discovery and subscriptions, plus its implementation stages (§9). |
| [`docs/hub-interface.md`](docs/hub-interface.md) | The Grain Hub node interface: the messages a Grain deployment speaks to the hub over the rendezvous tunnel (registration, Grainfile subscriptions, webhook/trigger notifications and catch-up, cross-deployment completion notices, interactive UI actions, event streaming, and `view` / `http` streams). |
| [`docs/grainfiles.md`](docs/grainfiles.md), [`docs/grainfiles-spec.md`](docs/grainfiles-spec.md) | Grainfiles: discrete tasks (`.task.md`) and standing processes (`.process.md`) kept as Markdown with YAML front matter in git and executed directly by deployments. The design, and **The Grainfile Format**, its normative spec, with the schemas in `schemas/grainfiles/`, the `grainlint` validator in `tools/grainfiles/` (the `grainfiles` CI job) and the worked example in `examples/grainfiles/`. |
| [`docs/deploy.md`](docs/deploy.md) | How to deploy Grain Hub on GKE behind IAP, in `bsalmon-gke-dev`. |
| [`docs/conformance.md`](docs/conformance.md) | The conformance kit: how to check that a Grain speaks the hub interface, the scenarios, and the report. |
| [`docs/ui.md`](docs/ui.md) | The hub's human web UI (hub.md §9, stage 2.6): the options, the chosen one, its stages, and how to run it locally (§7.1). |
| [`docs/threat-model.md`](docs/threat-model.md) | The threat model of the Grain + Grain Hub system: assets, actors, trust boundaries, threats, the numbered security invariants (`INV-`) that audits and security tests cite, and the audit plan. |
| `old/` | Retired code, not part of the hub: the former single-run runner, kept for porting into bwsalmon/grain ([`old/README.md`](old/README.md)). It is a separate Go module, and neither imports the other. |

## Building and testing

```sh
gofmt -l cmd hub internal events
go build ./... && go vet ./... && go test -race ./...
python3 tools/grainfiles/grainlint.py --self-test
python3 tools/grainfiles/grainlint.py examples/grainfiles
```

grain CI runs the Go suite in `.grain/workflows/hub.yml` and the
Grainfile linter checks in `.grain/workflows/grainfiles.yml`. The hub's
store is PostgreSQL on Cloud SQL in every deployment
([`docs/deploy.md`](docs/deploy.md)); SQLite is for local development
and tests only. The stores' tests (`hub/directory`, `hub/tasks`,
`hub/live`, `hub/registry`, `hub/lease`, `hub/notify`, `hub/server`)
run against a throwaway PostgreSQL (`internal/pgtest`) as well as
SQLite where PostgreSQL's server binaries are installed; CI installs
them and sets `SILO_REQUIRE_POSTGRES=1`, which turns a missing server
into a failure rather than a skip.

`.grain/workflows/ui.yml` builds and tests the web UI (`ui/`, Node 20.19
or later) and the TypeScript SDK (`sdk/ts`): `npm ci`, `npm run build`
(tsc and Vite), `npm test` (vitest), and then `hub/ui`'s and
`hub/server`'s tests with the bundle embedded. The Go build above needs
no Node: without the bundle it embeds a placeholder page.
[`docs/ui.md`](docs/ui.md) §7.1 says how to run the UI locally.

`.grain/workflows/deploy.yml` builds the image (the UI's bundle
included), validates the manifests on a kind cluster
(`deploy/kubernetes/hub/validate.sh`) and runs `terraform validate` and
`terraform test` (mock providers) in `deploy/terraform/hub`. Nothing is
applied anywhere.

`.grain/workflows/grain-interop.yml` tests this commit's hub against
**real grain**, which the fake Grains in `hub/*`'s own tests can't
stand in for. The job reads bwsalmon/grain through the git proxy
(`capabilities: ["repo-read:bwsalmon/grain"]`) and builds grain's
`main` twice: once as it is, with the client it pins, and once against
this commit's hub packages. It runs on every push and pull request, and
takes about two and a half minutes. `scripts/grain-interop.sh` does the
work. It starts `silo hub serve -dev` on SQLite, signing a test user in
through a fake OIDC provider (`internal/fakeoidc`), and starts a grain
daemon pointed at it. The daemon runs with a local policy and a fake
GitHub API (`scripts/fake-github.py`), and with no sandboxes and no CI,
so no agent or model runs. It then checks the current build's node
interface ([`docs/hub-interface.md`](docs/hub-interface.md)) as far as
grain supports it:

- grain builds, and its hub tests pass, against this commit;
- `grain hub login` mints the node token;
- grain shows `online` in `GET /v1/nodes`;
- a task filed through `POST /v1/tasks` is accepted;
- a task for a repository outside grain's policy is refused;
- a cancel on the hub closes the task on grain;
- a task ended on grain reaches the hub as its result.

Stages grain doesn't support yet (events, view-through, merge) are
skipped, and the log says so. A red job is a real mismatch. On a
failure the script reruns the same check against the repository's base
commit, and its last lines say which side changed, by both commits'
short SHAs. To run it by hand:
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
