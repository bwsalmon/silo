# Grain Hub

This is the design of **Grain Hub** (formerly **Silo**; the repository
and Go module remain `github.com/bwsalmon/silo` and the command remains
`cmd/silo` in the current build until code migration tasks run).

Grain Hub links standalone, single-tenant **Grain** deployments into one
multi-tenant system around two core responsibilities:

1. **Multi-tenant UI and reachability across Grain deployments**
   (`hub/rendezvous`, `hub/registry`, `hub/identity`, `hub/directory`,
   `hub/ui`, `hub/live`, `hub/nodeui`, §1.1, §3–§5, §7, §11).
2. **Discovery and subscription for `.task.md` and `.process.md`
   Grainfiles** ([`docs/grainfiles.md`](grainfiles.md),
   [`docs/grainfiles-spec.md`](grainfiles-spec.md), §1.2, §6).

Its foundational infrastructure is built and deployed on GKE behind IAP
(`hub/server`, `deploy/`, [deploy.md](deploy.md),
[hub-operations.md](hub-operations.md)), running multiple replicas
rolled one pod at a time (`hub/lease`, `hub/notify`, §2.2).

> **Current build note:** Existing code identifiers
> (`github.com/bwsalmon/silo`, `cmd/silo`, `silo hub serve`,
> `SILO_HUB_*`, `silo-node.v1`, `silo_hub`, `silo-open.v1`) and the
> earlier imperative task packages (`hub/tasks`, `hub/claims`,
> `hub/conformance`) are noted where they reflect the current build so
> `grain-interop` and existing tests remain unbroken until the code
> migration tasks run.

## 1. What Grain Hub is

**Grain** is the execution engine. Each Grain deployment is
**single-tenant and manages its own backlog**: its owner's (or one
team's) tasks and standing processes, queue, git checkouts, sandboxes,
agent sessions, and merge queue. Where a Grain deployment runs is a
deployment detail — a workstation, a Cloudtop, a VM, or a cluster — and
wherever it runs, it remains complete on its own. In this document a
**node** is one such Grain deployment as the hub sees it: one outbound
connection, one registry entry, one deployment behind it.

**Grain Hub** (or simply **the hub**) sits above single-tenant Grain
deployments and has two core responsibilities:

### 1.1 Responsibility 1: Multi-tenant UI and reachability across Grain deployments

- **Single-tenant Grain nodes, multi-tenant Hub UI (`hub/rendezvous`,
  `hub/registry`):** Each Grain deployment remains a standalone,
  single-tenant execution engine that owns its own backlog, git
  checkouts, sandboxes, and merge queue. Nodes dial outbound to the hub
  over WebSocket (`wss://<hub>/v1/nodes/connect`, subprotocol
  `silo-node.v1`, §4), so nothing has to be open inbound behind NAT or a
  corporate firewall.
- **Human sign-on and access control (`hub/identity`,
  `hub/directory`):** Humans authenticate via Google Cloud IAP or OIDC
  (§5.1). The team directory (§5.3) governs which users and teams can
  see and interact with which Grain deployments and repositories.
- **Multi-tenant UI, live event mirroring, and view-through (`hub/ui`,
  `hub/live`, `hub/nodeui`):**
  - The hub serves a multi-tenant overview UI ([ui.md](ui.md)) across
    all Grain deployments the signed-in user can access, mirroring task
    and process lifecycle and conversation events streamed from the
    nodes (`hub/live`, §7.1).
  - Diffs, tool logs, and each node's own full UI
    (`<node>.nodes.<hub-domain>`, `hub/nodeui`, §11) are streamed on
    demand over the reverse tunnel (`Server.Open` `view` and `http`
    streams, §7.2, §11.2).
  - Interactive actions from the UI (chat messages, in-run approvals,
    cancellations) are routed over the tunnel to the Grain deployment
    that owns the task (§6.6).
- **No source code at rest:** The hub stores no repository checkouts,
  diffs, file contents, or sandbox/tool logs at rest (§7.2). Anything
  that can contain source code stays on the node that produced it and is
  streamed to an authorized viewer on demand without caching. If the
  hub's database were lost or leaked, it would expose Grainfile
  front-matter metadata, lifecycle/conversation text, and the team
  directory, but no repository source content.

### 1.2 Responsibility 2: Discovery and subscription for `.task.md` and `.process.md` Grainfiles

- **Git is the source of truth for cross-repo tasks and processes:**
  Instead of the hub maintaining its own parallel task-submission
  backlog, imperative task dispatcher (`offer`/`accept`/`refuse`/`resume`,
  lost-task requeueing), and Policy Decision Point (PDP) JWT
  claim-signing (`hub/tasks`, `hub/claims`), cross-repository tasks and
  standing processes are authored in git as `.task.md` and `.process.md`
  files ([`docs/grainfiles.md`](grainfiles.md),
  [`docs/grainfiles-spec.md`](grainfiles-spec.md)).
- **Source authority and local policy enforcement:** As defined in
  [`docs/grainfiles-spec.md`](grainfiles-spec.md) (§6.2, §11.1), the
  authoritative source of a `.task.md` or `.process.md` is the URL of
  the git repository and folder where the file lives, and `recipient` (a
  plain `https://` deployment URL or repo-local folder path) names the
  Grain deployment intended to execute it (with optional `requestor`
  attribution metadata). Write authority comes from the git forge's
  branch protection and `CODEOWNERS` reviews on that repository and
  folder. Each Grain deployment is the Policy Enforcement Point (PEP)
  and enforces its own local policy on which repository and folder
  sources it trusts, failing closed on anything its local policy does
  not allow.
- **Consuming external git repositories (never hosting git repos):**
  Grain Hub **consumes** external git repositories (via a GitHub App or
  forge webhooks and API) rather than hosting git repositories itself
  (§6.1.1):
  - Grainfile source authority ([`docs/grainfiles-spec.md`](grainfiles-spec.md)
    §11.1) relies on the external forge's branch protection, `CODEOWNERS`,
    and pull-request review on the repositories where code and docs live.
  - Consuming external repos keeps the hub stateless with respect to git
    packfiles and preserves the **no source code at rest** invariant
    (§1.1): the hub database stores only Grainfile metadata and YAML
    front matter, never source trees or file bodies.
  - Individual Grain deployments do not need to know about or hold
    standing credentials for every other deployment's or team's
    repositories; each deployment only connects to the hub as its own
    `recipient` identity.
- **Subscriptions (`recipient` + watched repos/processes):** A Grain
  deployment registers and updates its subscriptions with the hub (§6.2):
  its own `recipient` deployment URL(s), plus any repositories, folder
  paths, or shared `.process.md` / `.task.md` URLs it subscribes to.
- **Cross-repository content access via `recipient` and `references`:**
  - **Grainfile as a repo-scoped capability grant:** When a non-draft
    `.task.md` or `.process.md` is merged into **Repo A** with
    `recipient: <Deployment B>`, Repo A's owners have explicitly
    authorized `<Deployment B>` to receive the Grainfile itself and to
    read the **Repo A** files and folders named in its `references` (as
    well as reading/working on Repo A when the Grainfile in Repo A asks
    `<Deployment B>` to act on Repo A) (§6.4.1).
  - **Same-origin authority rule ("a repository can only grant access to
    itself"):** A Grainfile committed in **Repo A** can authorize
    `<Deployment B>` to read `references` inside **Repo A** (resolved at
    the commit/ref of the Grainfile or the pinned ref in the URL). It
    **cannot** grant `<Deployment B>` access to a third private
    repository (**Repo C**) merely by listing a `https://.../repo-c/...`
    URL in `references` — reading Repo C requires either `<Deployment B>`
    already having access to Repo C, or Repo C itself
    publishing/authorizing it.
  - **On-demand fetch through the hub:** A subscribed Grain deployment
    fetches the matched `.task.md` / `.process.md` body and its
    authorized same-repo `references` through the hub on demand
    ([hub-interface.md](hub-interface.md), §6.4.1) without holding
    permanent git credentials for Repo A; the hub streams them from the
    forge API and never stores them at rest in the hub database.
- **Indexing and webhook/event notification:**
  - Because Grain deployments run on workstations or VMs behind
    NAT/firewalls and should not have to poll every repository in an
    organization, the hub receives forge webhooks (`push`,
    `pull_request`, `merge`) and indexes Grainfile metadata across
    connected repositories (§6.3): the file's `https://` URL,
    commit/ref, and parsed YAML front matter (`grainfile`, `title`,
    `recipient`, `requestor`, `draft`, `not_before`, `references`,
    `depends_on`, `when`, `gate` — metadata only, no source code at rest).
  - When a non-draft `.task.grain.md` or `.process.grain.md` matching a
    deployment's `recipient` or subscriptions is added or updated, or when
    a forge event (`push`, `pull_request`, `merge`) matches a subscribed
    `.process.grain.md` trigger, the hub notifies the subscribed Grain
    deployment over its connection (and answers catch-up queries on
    connect/reconnect, §6.4) so the node can fetch the Grainfile body and
    its authorized same-repo `references` on demand through the hub (or
    directly from git when it holds credentials for that repo) and
    execute it directly.
  - When `.task.grain.md` or `.process.grain.md` files declare
    cross-deployment `depends_on` references, the hub routes completion
    and co-trigger notices between the subscribed deployments based on the
    task and process lifecycle events nodes report (§6.5).

### 1.3 The node–hub interface is the contract

Grain Hub depends on nothing in a Grain deployment except the
**node–hub interface** it speaks over the rendezvous: register, declare
Grainfile subscriptions, heartbeat with capacity, receive Grainfile and
trigger notifications (and catch up on reconnect), fetch matched
Grainfile bodies and authorized same-repo `references` on demand, receive
cross-deployment dependency/co-trigger notices and interactive UI actions
(`cancel`, `approve`, `message`), stream lifecycle and conversation
events, and serve on-demand `view` and `http` streams. **Grain
deployments can differ.** A user may customise their own Grain
deployment, or run different code altogether, as long as it conforms to
that contract: [hub-interface.md](hub-interface.md).

## 2. Components

```
 humans, multi-tenant UI, SDK          forge webhooks (push, pull_request, merge)
        |  HTTPS (IAP / OIDC)                 |  HTTPS webhook intake
        v                                     v
 +-------------------------------- Grain Hub ---------------------------------+
 | identity & directory (§5)        Grainfile indexer & subscriptions (§6)    |
 |   teams, members, node tokens      front-matter index (metadata only)      |
 |            \                              /                                |
 |             v                            v                                 |
 |           rendezvous server (§4) & shared node registry (§3)               |
 |             |                  ^                     |                     |
 |             | grainfile_event  | NodeStream          | Server.Open(node)   |
 |             | trigger_event    | lifecycle/conv      | view / http / UI    |
 |             | dependency_done  | events (§7.1)       | actions (cancel,    |
 |             v                  |                     v approve, message)   |
 |           event store & fan-out (§7) ---> multi-tenant web UI / SDK        |
 |           (metadata & conversation only; diffs/logs via view-through)      |
 +----------------------------------------------------------------------------+
                                      ^
                                      | outbound HTTPS only: WebSocket (silo-node.v1)
                       Grain deployments (one backlog each, on any machine)
```

The **store** is a database: PostgreSQL on Cloud SQL in every
deployment, one replica or several, for its managed backups,
point-in-time recovery, on-demand snapshots and storage that grows
([deploy.md](deploy.md), [hub-operations.md](hub-operations.md)). SQLite
stands in for it only in local development and tests, and `silo hub
serve` refuses it otherwise (`-dev`, §2.1). It holds the team directory,
the Grainfile front-matter metadata index and subscription state (and
the current build's `hub/tasks` rows), and the lifecycle/conversation
event metadata. The **registry** is in memory, on each pod, for the
connections that pod holds, and published to the database for the other
pods (§2.2.2). Everything but a node's owner is restated by each node
when the node connects, so a hub that restarts relearns its nodes and
their subscriptions within one reconnect backoff.

Grain Hub is deployed on **GKE behind IAP**: a GKE Gateway fronts it
with Google's HTTPS load balancer, and IAP is enabled on that load
balancer's backend service for humans (§4.1, §5.1).

### 2.1 Serving and deployment (built: `hub/server`, `cmd/silo`, `deploy/`)

`silo hub serve` (`hub/server`) runs every part above as one HTTP
server over one database: the team directory, the event store, and the
task/subscription handlers, each in its own tables of the same
PostgreSQL database on Cloud SQL, with the rendezvous in memory and the
registry in memory and published to the database (§2.2.2). It is
configured by flags or their `SILO_HUB_*` environment variables, and
needs exactly one human sign-on: an IAP audience, an OIDC issuer, or,
for development only, a header taken on trust. A `sqlite:` database is
for local development and tests only: `silo hub serve` refuses it unless
`-dev` (`SILO_HUB_DEV`) or the development header is set, and refuses it
on Kubernetes whatever is set (`server.CheckDatabase`), as it refuses
the header there (`server.CheckDevSignOn`). `/healthz` is liveness;
`/readyz` answers 200 while the database answers and the hub is not
shutting down.

On SIGTERM the hub turns unready, sends each node a `goodbye` on its
control stream saying it is shutting down (§4.3), ends open event
subscriptions (which reconnect from their cursor), stops background
workers and closes the stores, within 20 seconds. The node's client logs
the reason and reconnects with its usual backoff.
`silo hub team` and `silo hub token` bootstrap a new hub straight in
its database, before anyone can reach its API.

**Why GKE and not Cloud Run.** The node WebSocket lives as long as the
load balancer's backend timeout allows: a day, as deployed, where Cloud
Run ends every request at 60 minutes. The client reconnects cleanly
either way, because pods roll. GKE also runs several replicas (§2.2)
side by side, each reaching the others' pods directly on an internal
port, under a PodDisruptionBudget and a topology spread. The shape is
grain's own load balancer and IAP setup (grain's
`deploy/terraform/gcp/iap.tf`, and `deploy/terraform/relay` with
`iap_mode = "load_balancer"`). Cloud Run remains possible for a small
hub that accepts hourly reconnects: it would serve the same image, with
the node path left public and IAP on the rest through a serverless NEG
behind the same kind of load balancer. It is not built.

**Several replicas.** The deployment runs two pods and rolls them one
at a time (`RollingUpdate`, `maxSurge: 1`, `maxUnavailable: 0`), with
a PodDisruptionBudget of one and the pods spread over zones and GKE
nodes (§2.2.5). Any pod serves any request: the registry is shared
(§2.2.2), streams to a node are forwarded to its pod (§2.2.3), wake-ups
and live events cross pods (§2.2.4), and leader-coordinated background
work runs on one pod at a time under a database lease (§2.2.1). A roll,
a crash or a GKE node's loss costs that pod's nodes a reconnect of a few
seconds to another pod, and never takes the hub wholly down. §2.2 is the
design, in the stages it was built in, and the recovery time of each
failure.

**Metrics.** With `-metrics-addr` (`:9091` as deployed) the hub serves
Prometheus metrics at `/metrics` on a listener of its own, which no
Service names, so the load balancer and IAP never reach it. GKE's
Managed Service for Prometheus scrapes it (a `PodMonitoring`), and
Cloud Monitoring alerts on it and charts it (`deploy/terraform/hub/
monitoring.tf`). [hub-operations.md](hub-operations.md) lists the
metrics, the alerts and the dashboard. With `-internal-addr` (`:9092`
as deployed) it serves the internal listener, for the hub's other pods
alone (§2.2.3).

[deploy.md](deploy.md) is the runbook: the terraform
(`deploy/terraform/hub`), the manifests (`deploy/kubernetes/hub`) and
how a Grain deployment is pointed at the hub.
[hub-operations.md](hub-operations.md) is the runbook for running it:
upgrades, rotating secrets, restoring the database and reading the
dashboard.

### 2.2 Replicas: several, built in stages

**Decision: Grain Hub runs two replicas, rolled one at a time.** It is a
coordination and view-through plane, not a storage engine for source
code: a hub pod that rolls costs its connected nodes a reconnect and
humans a retry, and loses nothing, because everything durable is in the
database and everything in memory is restated by the nodes (§2). Multiple
replicas keep the multi-tenant UI, artifact/node-UI view-through (§7.2,
§11), and Grainfile notifications available across upgrades and single-node
failures, in five built stages (§2.2.1 to §2.2.5).

**Recovery time as deployed.** Two pods on different GKE nodes (and
zones, where the cluster has them), nodes that reconnect with backoff
(1s doubling to a minute):

| Failure | What happens | Back in |
| --- | --- | --- |
| A hub pod's process crashes or wedges | The kubelet restarts the container (liveness on `/healthz`); meanwhile the load balancer sends everything to the other pod, the crashed pod's nodes reconnect to it, and the crashed pod's lease in the registry lapses (fifteen seconds), taking any of its nodes that have not reconnected offline. If it held the coordinator/dispatcher lease, PostgreSQL frees it when its session ends and another pod's term starts | the hub stays up; that pod's nodes back within their backoff, under a minute |
| An upgrade | `RollingUpdate`, one pod at a time: a new pod is ready before an old one says goodbye (§2.2.5); the lease moves within five seconds and the old pod's nodes reconnect to a ready pod within a few seconds | the hub stays up; each pod's nodes reconnect once |
| The GKE node a hub pod runs on is preempted or lost | The other pod keeps serving; the lost pod's registry lease lapses within fifteen seconds, and its lease in PostgreSQL within ten (`keepalives`), while its nodes reconnect to the live pod | the hub stays up; that pod's nodes back within their backoff |
| Cloud SQL failover (`database_availability_type = "REGIONAL"`) | Connections drop; `/readyz` fails while the standby promotes; the stores' pools and the lease reconnect on the next call | 1–2 minutes (Cloud SQL's failover) |
| Cloud SQL zone outage with `ZONAL` (the dev default) | Waits for the zone | the zone's recovery |
| Bad migration or accidental delete | Point-in-time recovery or a snapshot restore into a clone, then swap ([hub-operations.md](hub-operations.md)) | 10–20 minutes, manual |

Each of the five stages below removes one piece of single-pod state:

| Stage | What | State |
| --- | --- | --- |
| R1 | Coordinator/dispatcher under a database lease (`hub/lease`), `database_availability_type` | built |
| R2 | The shared registry: `hub_pods` and `hub_nodes` in the database, each node naming the pod holding it, under that pod's lease (`hub/registry`) | built |
| R3 | `Server.Open` forwarded to the holder over a pod-to-pod stream; the directory read from the shared registry | built |
| R4 | Cross-pod wake-ups and live events: LISTEN/NOTIFY, subscriptions forwarded to the pod holding the task's node | built |
| R5 | Two or more replicas, `RollingUpdate`, a PodDisruptionBudget, the operations runbook and alerts | built |

#### 2.2.1 R1: leader coordination under a lease (built: `hub/lease`)

Every pod runs `lease.Run` for the lease named `dispatcher` (`hub/lease`):

- **The lease** is a session-level PostgreSQL advisory lock
  (`pg_try_advisory_lock`) on the lease name's fixed key, held on a
  connection of its own and outside the stores' pools. A pod that does
  not hold it tries again every five seconds. The holder pings its
  connection every two seconds and gives the lease up on the first ping
  that fails or takes longer than two seconds; PostgreSQL frees the lock
  when the holder's session ends, which the connection's TCP keepalive
  makes about ten seconds after the holder is cut off. On SQLite
  (development and tests only), which is one process, the lease is
  taken at once.
- **A term** is one hold of the lease. Taking it bumps the lease's
  epoch, a row in `hub_leases` (name, epoch, holder, taken_at), so the
  terms are numbered and the holder is visible in the database.
- **What it shows.** `silo_hub_dispatcher_leader` is 1 on the pod
  holding the lease and 0 on the others, and
  `silo_hub_dispatcher_terms_total` counts the terms a pod has taken.
  `/readyz` does not depend on the lease: a pod that is not the lease
  holder still serves nodes and humans.
- **On shutdown** the pod stops its background loop and releases the
  lease explicitly, so another pod takes it within one retry rather than
  after the keepalive.

#### 2.2.2 R2: the shared registry (built: `hub/registry`)

The registry stays in memory on each pod, authoritative for the
connections that pod holds, and each pod publishes it to two tables
(`registry.Store`, with `Registry.Run` keeping the pod in them):

- `hub_pods (pod, addr, lease_until)`: each pod's name (the Kubernetes
  pod name, from the downward API), its internal address (pod IP and
  the internal port, R3), and a lease it renews every five seconds for
  fifteen. A pod's row is deleted when it shuts down, after its node
  connections have closed. The name and address are `-pod` and
  `-pod-addr` (`SILO_HUB_POD`, `SILO_HUB_POD_ADDR`), both the host's
  name when unset, which is the pod's name on Kubernetes and enough for
  one pod; the address, when unset, is the internal listener's. As
  deployed (R5), both come from the downward API: the pod's name, and
  its IP with the internal port.
- `hub_nodes (id, owner, pod, conn, repos, slots, version, labels,
  free, settled, connected_at, last_seen)`: one row per node ever
  registered, `pod` and `conn` (a random ID per connection) naming the
  connection holding it, empty when it is offline. Times are the
  writing pod's clock, in milliseconds.

Each rule of §3 moves into a conditional statement:

- **Ownership.** Registering inserts the row, or updates it only where
  `owner` matches: a different owner gets `ErrOwner` from the database,
  whichever pod either connection reached.
- **Supersession.** Registering sets `pod` and `conn` to the new
  connection's. The old connection's pod learns it was superseded when
  its next heartbeat's update (`WHERE id = ? AND conn = ?`) matches no
  row, and drops the connection; a late close is likewise conditional
  on `conn`, so it never takes the newer connection offline. Each
  renewal of a pod's lease also takes offline any row naming the pod
  whose connection it does not hold.
- **Giving an ID up.** Since the owner now outlives a restart, giving a
  node ID up in the team directory (`DELETE /v1/node-tokens/nodes/
  {node}`, §5.2) deletes its row too (`Registry.Unbind`), whichever pod
  holds it: that pod's next heartbeat matches no row and drops the
  connection, and the next owner can register the ID.
- **Status.** A node is online when its row names a pod whose
  `lease_until` is in the future and its `last_seen` is within
  `StaleAfter`; stale when the pod is live but the node is silent;
  offline when `pod` is empty or the pod's lease has lapsed. A pod that
  dies takes its nodes offline within one pod lease (fifteen seconds),
  with no pod having to notice: the status is computed at read time.
- **Mirroring and changes.** Every pod's registry is a mirror of the
  others' nodes, which it refills from `hub_nodes` (joined with
  `hub_pods`) at each renewal of its own lease, every five seconds, and
  on every change notice (R4, §2.2.4). A pod's own registry changes are
  written through at once, outside the registry's lock; heartbeats are
  written at most every heartbeat interval. Each pod's metrics count
  every node in the shared registry, not only its own.
- **SQLite** (development and tests only), one process, uses the same
  tables, so ownership outlives a restart there too.

The alternative, gossiping registries between pods, was rejected: the
database is already shared, consistent and the thing every pod must
reach to be ready, where gossip would add membership, a protocol and
its split-brain, for a table of at most a few thousand rows.

#### 2.2.3 R3: `Server.Open` across pods (built)

Each pod serves a second listener, the **internal listener**
(`-internal-addr`, `:9092` as deployed), on its pod IP, which no
Service names: the load balancer and IAP never reach it. It answers
one route, `GET /internal/v1/nodes/{id}/open`, an HTTP upgrade
(`Upgrade: silo-open.v1`). The holder checks that it still holds the
node's connection, opens a mux stream to the node as `Server.Open`
does locally, answers `101`, and splices the two until both directions
end: a direction that ends cleanly half-closes the other side, as a
local stream's `CloseWrite` does, and one that fails ends both.

`Server.Open(ctx, node)` opens locally when this pod holds the node,
and otherwise reads the holder's `addr` afresh from `hub_nodes` and
`hub_pods` (`Registry.Holder`; a pod whose lease has lapsed holds
nothing), dials it, upgrades, and returns the upgraded connection
as the `net.Conn`. Everything that opens a stream to a node rides it
unchanged: Grainfile and trigger notifications (§6.4), cross-deployment
dependency notices (§6.5), interactive UI actions (`cancel`, `approve`,
`message`, §6.6), the artifact `view` stream (on the viewer's pod,
§7.2), and the node UI's `http` streams (on the browser's pod, §11.2),
as well as the current build's `hub/tasks` streams. A holder that
answers 404 (it lost the node since the read) is retried once after a
fresh read; then `Open` fails as for an offline node.

**Authentication.** Internal requests carry
`Authorization: Bearer <HMAC>`: HMAC-SHA256, in hex, under a key all
pods share (a Kubernetes Secret, `silo-hub-internal`, mounted as a
file and named by `-internal-key-file`, at least 32 bytes), over the
target pod's name and the minute (Unix time over 60), so a token cannot
be replayed to another pod or later. The holder accepts the minutes
either side of its own. A NetworkPolicy admits the internal port from
hub pods only, and no Service names it (`validate.sh` checks both).
Without a key the hub serves no internal listener and opens only the
nodes it holds. The caller has already checked the human's or the
router's right to the node; the holder trusts that check, as one pod
trusts another's database writes.

**The directory** (`GET /v1/nodes`) and the node UI's host check read
the shared registry, through the pod's mirror of it (§2.2.2), so any
pod answers either.

The cost is one more hop for view-through and node-UI bytes when the
browser's pod is not the node's. Session affinity on the Gateway
cannot avoid it, since a browser and a node are different clients.

#### 2.2.4 R4: wake-ups and live events across pods (built: `hub/notify`)

**Wake-ups and notices.** Cross-pod signals — registry changes,
Grainfile webhook/subscription notices, interactive UI actions (`cancel`,
`approve`, `message`), and live event notifications — are sent as
PostgreSQL `NOTIFY` messages on the channel `silo_hub`, with a short
JSON payload (`{"wake":true}`, `{"cancel":"<task>"}`,
`{"node":"<id>"}`, `{"approve":"<task>"}`, `{"message":"<task>"}`,
`{"event":"<task>","seq":N}`), and every pod `LISTEN`s on a connection
of its own (`hub/notify`), reconnecting with backoff. Every pod refills
its registry mirror on `node` at once. SQLite (development and tests
only), one process, keeps the in-memory signals.

**Sending.** No caller waits on the database for a wake-up, a `node`
or an `event` notice: each is queued (`Bus.Post`), and one goroutine a
pod sends what is queued, gathered for 2ms and while its last send is
in flight, as one `SELECT pg_notify(...) FROM unnest(...)`: one
transaction per batch, of at most 500 notices. Queued notices about
the same thing are coalesced, since every receiver reads the state
again at a notice: one `wake`, one `node` per node, one `event` per
task with the highest seq.

**Live events.** A run's events arrive on the pod holding its node,
which stores the lifecycle and conversation events and keeps the
token ring (§7.1) in memory. A subscriber's request lands on any pod:

- If the run's node is held by another pod, the subscriber's pod
  checks access as today, then forwards the subscription over the
  internal listener (`GET /internal/v1/tasks/{id}/events?from=`, under
  the same token as R3's route) to that pod, which serves it from its
  ring and store as Server-Sent Events, and copies the events, the
  keepalives and the `end` through, as SSE or WebSocket messages. The
  ring stays on one pod, so late joiners see the token history as
  today. A pod that does not hold the node answers 404, and the
  subscriber's pod reads the registry once more, as `Open` does. A
  subscription a pod serves because it holds the node, forwarded or
  its own, ends without `end` when the pod stops holding it, and a
  forwarded one that goes silent for three keepalives (45s) has lost
  its pod and ends the same way.
- If no pod holds the node, the subscriber's pod serves the stored
  events from the database and follows new ones on `NOTIFY silo_hub
  {"event":"<task>","seq":N}`, which the ingesting pod sends after it
  stores events (coalesced, as above); it reads the store again on the
  task's `cancel`, on the listener reconnecting, and every 15 seconds,
  and ends with the task's result, or once the task is over. Token
  events, which no store keeps, are not seen this way. A holder that
  cannot be reached is served the same way.
- A hub without an internal key, which cannot forward, serves every
  subscription from its own memory and store, as one pod did before.

Publishing every event through `NOTIFY` (or a pub/sub) was rejected:
token deltas are many and `NOTIFY` payloads are capped at 8000 bytes,
and the subscription would still have to hold a ring for late joiners
on every pod.

#### 2.2.5 R5: the Deployment (built)

- `replicas: 2`, `strategy: RollingUpdate` with `maxSurge: 1` and
  `maxUnavailable: 0`, a `PodDisruptionBudget` of `minAvailable: 1`,
  and a topology spread across zones and nodes. The pod's name and IP
  come from the downward API (`SILO_HUB_POD`, and `SILO_HUB_POD_ADDR`
  as the IP and the internal port). The zone spread is
  `ScheduleAnyway`, so a zonal cluster still schedules; the node
  spread is `DoNotSchedule`, so the cluster needs two GKE nodes. Both
  count the pods of one revision (`matchLabelKeys: pod-template-hash`),
  so a roll's surge pod is not held back by the old ones.
  `validate.sh` checks these in place of its one-replica check.
- The Gateway keeps no session affinity: any pod serves any request.
- **A roll.** Each old pod says goodbye to its nodes, which reconnect
  to a ready pod; if it held the lease, it releases it and another
  pod's term starts within five seconds. Nodes reconnect within their
  backoff, and the hub is never wholly down.
- **Alerts.** "The hub is down" becomes "no pod is ready" (no scrape,
  the sum of `up` over the hub's pods 0, or no pod reporting
  `silo_hub_ready` 1); fewer pods ready than terraform's
  `hub_replicas` while some serve is a warning; and
  `silo_hub_dispatcher_leader` summing to 0 for five minutes stays an
  alert of its own. Every pod reports the whole registry, so gauges are
  taken as the max across pods, not the sum.

## 3. The node registry (built: `hub/registry`)

Each pod keeps the registry in memory for the connections it holds.
With several pods it is shared through the database (§2.2.2), and each
rule below is a conditional statement there, so it holds whichever pod
each connection reached.

A node registers with a `registry.Node`:

| Field | Meaning |
| --- | --- |
| `id` | Stable across the node's connections. 1-63 characters from `[A-Za-z0-9._-]`. |
| `repos` | The repositories (`owner/name`) the node serves and watches. They are lower-cased, sorted and deduplicated. |
| `slots` | Concurrency: how many runs the node executes at once. |
| `version`, `labels` | Shown in the directory. |
| `features` | Optional interface capabilities the node supports (`interactive`, `approvals`, `grainfiles`). |
| `subscriptions` | The deployment's Grainfile subscriptions (§6.2): `recipients` deployment identity URLs, watched `repos` and `folders`, and explicit `grainfiles` URLs. |

The hub adds what the node may not claim for itself:

- `owner`, the identity its credential proves (§4.2).
- `status`.
- `free`, as the node last reported it (and `claimed` in the current
  build's `hub/tasks` slot reservations).
- `connected_at` and `last_seen`.

**Status.** A node is in one of three states:

- `online`: connected, and it heartbeated within `StaleAfter` (45s by
  default, three heartbeats).
- `stale`: connected but silent.
- `offline`: no connection, or the pod holding it has let its lease
  lapse (§2.2.2). The entry stays, showing when the node was last seen,
  until `Forget`, or `Unbind` when its ID is given up (§5.2).

**Ownership.** A node ID belongs to the first owner that registered it.
A different owner naming the same ID is refused (`ErrOwner`), even
while the node is offline, on any pod and across restarts (`hub_nodes`
keeps the owner). A user cannot take over another user's node
or its repositories by guessing the ID.

**Supersession.** When the same owner registers an ID that is already
online, the new connection replaces the old one. The old lease's `Done`
closes, and the rendezvous drops the old connection. A late `Close` on
the old lease does not take the node offline. When the old connection
is on another pod, that pod's next heartbeat finds the node's row on
another connection and ends the old lease (§2.2.2). This lets a node that
lost its connection, before the hub noticed, come back at once.

**Slot claims (current `hub/tasks` build).** In the current build,
`Claim(repo)` reserves one slot on the online node with the most
available slots (`free − claimed`), settled by each heartbeat. Under the
Grainfile subscription model (§6), each Grain deployment owns its own
backlog and schedules its own slots locally.

**Changes.** `Changes()` is a coalesced notification channel used by the
UI and subscription router to track online nodes.

## 4. The rendezvous (built: `hub/rendezvous`)

### 4.1 Transport

The node dials out to `wss://<hub>/v1/nodes/connect`, a WebSocket with
the subprotocol `silo-node.v1` (retained as the wire identifier in the
current build). Grain Hub never dials a node, so nothing has to be open
inbound wherever the Grain deployment runs. A WebSocket, rather than a
raw HTTP Upgrade or gRPC, is what Google's HTTPS load balancer and IAP
pass through unchanged. The hub runs on GKE, so that load balancer is
the one the GKE Ingress or Gateway provisions, with IAP enabled on its
backend service.

The WebSocket carries binary frames, and those frames carry the mux
in `internal/tunnel/mux` (from grain's `pkg/tunnel`). The mux provides
independent, flow-controlled streams and keepalive pings. A dead
connection is noticed even when TCP is not.

### 4.2 Authentication

The hub authenticates the handshake request **before** it upgrades the
connection (`Server.Authenticate`). An unauthenticated node gets a 401,
never a WebSocket. The owner a node is registered under is always the
owner its credential proves. The hello message carries no owner field
that the hub would trust.

`Authenticate` returns an `Identity{Owner, Node}`. `Node` is set when
the credential was minted for one node ID, as a node token is (§5.2): a
hello naming any other ID is refused, so one leaked token cannot
register a different node. Production uses the team directory's
`Store.Authenticate`.

`Server.Admit(owner, node)` is the hook that the team directory fills
(`Store.Admit`, §5.3). It refuses a node whose owner may not serve one
of the repositories it lists. With no `Admit` set, every authenticated
node is admitted.

`Server.Visible`, when set, authenticates a request for the directory
(`GET /v1/nodes`) and filters it to the nodes the caller may see
(`directory.API.Visible`, §5.3). `Server.Disconnect(node)` ends a
node's connection, which is what revoking its token does.

### 4.3 The control stream

The node opens the first stream, and it carries JSON lines
([hub-interface.md](hub-interface.md)):

```
node → hub  {"type":"hello","node":{"id":"ws-1","repos":["bwsalmon/silo"],"slots":2,"version":"...","features":["interactive","approvals","grainfiles"],"subscriptions":{"recipients":["https://github.com/bwsalmon/silo"],"repos":["https://github.com/bwsalmon/silo"]}}}
hub → node  {"type":"welcome","owner":"alice@example.com","heartbeat_seconds":15}
            or {"type":"error","error":"registry: node belongs to another owner: ws-1"}, then close
node → hub  {"type":"heartbeat","free":1}       every heartbeat_seconds
node → hub  {"type":"subscribe","request_id":"s-1","subscriptions":{...}}
hub → node  {"type":"subscribed","request_id":"s-1","revision":2}
node → hub  {"type":"catchup","request_id":"c-1","since_cursor":"..."}
hub → node  {"type":"catchup_result","request_id":"c-1","cursor":"...","grainfiles":[...],"triggers":[...],"dependencies":[...]}
hub → node  {"type":"goodbye","error":"the hub is shutting down"}, then close
```

(In the current `hub/tasks` build, the control stream also accepts
`file_task` and answers `filed`; see §6.6.1.)

`goodbye` is `Server.Shutdown`: the hub is going away, and the node
should reconnect (to its replacement) as after any drop. While it shuts
down, the hub answers new connections 503.

The node has 30s after the handshake to say hello. A heartbeat updates
`free` and `last_seen`. The node goes offline when the connection ends,
whether because the node closed it, the mux's pings went unanswered,
or a newer connection superseded it.

### 4.4 Other streams

Either side opens further streams once the node is welcomed:

- **The hub to a node:** `Server.Open(ctx, nodeID)`. This is how the
  hub delivers Grainfile and trigger notifications (§6.4),
  cross-deployment `depends_on` notices (§6.5), and
  interactive UI actions (`cancel`, `approve`, `message`, §6.6), how an
  authorized viewer reads a diff or tool log through (`view`, §7.2), and
  how an authorized viewer reaches a node's own full UI (`http`, §11).
- **A node to the hub:** handed to `Server.NodeStream`. These are the
  node's live event streams for its task and process runs (§7.1) and
  its on-demand fetch streams for matched `.task.md` / `.process.md`
  bodies and authorized same-repo `references` (`fetch_grainfile`,
  `fetch_reference`, §6.4.1, [hub-interface.md](hub-interface.md)). With
  none set, they are reset.

### 4.5 The node side

`Client.Connect` dials, says hello, waits for welcome and heartbeats
with `Free()`. `Client.Run` keeps the node connected:

- It reconnects with exponential backoff, up to one minute.
- It hands each stream the hub opens to the caller.
- It returns a refusal instead of retrying it.

Each Grain deployment embeds this client.

## 5. Identity and access (built: `hub/identity`, `hub/directory`)

### 5.1 Humans

There are two ways to sign humans in (`hub/identity`). Each is an
`identity.Authenticator`, and both produce the same
`Principal{email, groups}`, with the email lower-cased:

- **Behind Google Cloud IAP (the default), `identity.IAP`:** the hub
  verifies the `x-goog-iap-jwt-assertion` header on every request. It
  checks the signature against IAP's published keys (ES256), the issuer
  (`https://cloud.google.com/iap`), the audience against the hub's
  backend service (`/projects/N/global/backendServices/ID`), and the
  expiry, with a minute's leeway. The verified `email` is the
  principal. The hub never reads `x-goog-authenticated-user-email`,
  because a request that bypasses the load balancer could forge it.
- **Without IAP, `identity.OIDC`:** the hub is an OIDC relying party
  for the company's SSO. `/auth/login` starts the authorization-code
  flow with PKCE (S256), a state and a nonce, kept in a short-lived
  signed cookie. The callback checks the state, exchanges the code with
  the verifier, verifies the ID token (signature against the provider's
  `jwks_uri`, issuer, audience = the client ID, expiry, nonce), and
  requires `email_verified`. It then sets an HMAC-signed session cookie
  (12 hours by default). An SDK or CLI that holds an ID token from the
  same provider can present it as a bearer token instead.

Keys are fetched from the issuer and cached for an hour. A token naming
an unknown key ID refetches them, at most once a minute. Only ES256 and
RS256 are accepted, and the algorithm must match the key's type, so
neither `none` nor an HMAC algorithm keyed with a public key can be
slipped in.

Group membership comes from the IdP's `groups` claim (OIDC). The
hub's own team directory can list a group as a member, as
`group:<name>` (§5.3). IAP's assertion carries no groups.

### 5.2 Nodes

A node cannot do an interactive sign-in for every connection. Instead:

1. The owner runs `grain hub login` once, through the browser flow
   above (or device flow). It calls `POST /v1/node-tokens
   {"node":"<id>"}`.
2. The hub mints a **node token** bound to that owner and node ID
   (`silo-node_<id>_<secret>`, 256 bits of secret). It is shown once.
   The store keeps only its SHA-256, so a leaked database proves
   nobody. Minting again for the same node revokes the owner's earlier
   token. **A node ID belongs to the first owner to mint a token for
   it, for good** (SA1-H1): another owner's mint is refused while any
   token of the first's for it, live or revoked, is unreleased. So node
   IDs stay bound to their owner across hub restarts, as the shared
   registry's `hub_nodes` also keeps them (§2.2.2), and across
   revocation. The owner, or a hub admin, gives the ID up with
   `DELETE /v1/node-tokens/nodes/{node}`, which revokes the owner's
   tokens for it (every owner's, for a hub admin), marks them released,
   and unbinds the ID in the registry (`Registry.Unbind`).
3. The node presents the token as a bearer token at the handshake.
   `Store.Authenticate` returns its owner and node ID, and the
   rendezvous refuses a hello for any other ID (§4.2).
4. The token can be revoked from the directory (`DELETE
   /v1/node-tokens/{id}`, by its owner or a hub admin). Revoking ends
   the node's live connection at once (`Server.Disconnect`), and its
   reconnect is refused.

### 5.2.1 Getting a node past IAP

A node authenticates with its node token, not a human's IAP sign-in,
so `ConnectPath` has to get past IAP. There were two ways:

- **(a) A path with IAP off.** The Gateway routes exactly
  `/v1/nodes/connect` to a second Service over the same pods, whose
  backend service has IAP off and a day's timeout; every other path
  goes to the IAP-enabled backend. The hub checks the node token
  itself, before it upgrades (§4.2).
- **(b) A Google identity as well.** The node presents a Google-signed
  ID token for a service account IAP admits, and its node token beside
  it.

**The choice is (a).** The hub already proves a node's identity from
its token, bound to one node ID and revocable at once (§5.2), so IAP
in front of it would add a second credential, not a stronger check.
(b) would make every node owner hold a Google credential IAP admits:
a workstation or a VM outside GCP would need a service-account key,
which is what nobody should be minting, or an OAuth client of its own
to refresh a user's ID token, and IAP's access list would duplicate the
team directory as a second place to grant a node. IAP also takes the
`Authorization` header for its own token, so the node token would move
to another header. With (a) the only cost is the routing: one more
Service, `GCPBackendPolicy` and `HealthCheckPolicy`, and an exact path
match, which the Gateway API expresses directly.

(In the current build, `/v1/claims/keys` also takes this route; §6.6.1.)

What stays protected: the node backend serves only those exact paths,
so no human API is reachable without IAP; the hub refuses an
unauthenticated handshake with 401 before any WebSocket exists; and on
every other path the hub still verifies `X-Goog-IAP-JWT-Assertion`
against the IAP-enabled backend service's audience, so a request that
reached a pod some other way is refused too. (b) can still be layered
on later for nodes that run on GCP, without changing (a).

### 5.3 Teams and multi-tenant visibility

The store (`directory.Store`: PostgreSQL; SQLite in development and
tests) holds:

- `teams(id, name)`.
- `team_members(team, principal, role: viewer | proposer | member | admin)`. A principal
  is IAM-style, `user:<email>` or `group:<name>`, lower-cased, and is
  matched only against its own claim: `user:` against the signed-in
  email, `group:` against each of the IdP's groups. When a user matches
  more than one principal on a team, the highest-ranked role wins
  (`viewer < proposer < member < admin`). The four team roles are:
  - **`admin`:** full team management (members, repositories, team
    settings), node admission (`Admit`), team-admin Node UI proxy
    access (`nodeui`), approving/rejecting proposals and answering
    in-run `approval_request`s, posting to interactive sessions, and
    submitting tasks (filed `queued` immediately unless the team's
    `require_approval` is `true`).
  - **`member`:** node admission (`Admit`), approving/rejecting
    proposals and answering in-run `approval_request`s, posting to team
    interactive sessions, viewing all team
    tasks/events/artifacts/nodes/Grainfiles, and submitting tasks
    (filed `queued` immediately unless `require_approval` is `true`).
  - **`proposer`:** read visibility into the team (same read access as
    `viewer`) plus permission to submit tasks as `proposed` (waiting for
    a `member` or `admin` of the team to approve) even when the team
    sets `anyone_may_propose: false`. Cannot approve/reject proposals,
    cannot answer in-run `approval_request`s, cannot post to interactive
    sessions they did not submit, and cannot register/admit nodes
    (`Store.Admit` and `AuthorizeOwnerRepo` require `member` or
    `admin`).
  - **`viewer`:** read-only visibility into the team
    (`GET /v1/teams/{team}`, `GET /v1/tasks`,
    `GET /v1/tasks/{id}/events`, `GET /v1/tasks/{id}/artifacts`,
    `GET /v1/nodes`, `GET /v1/grainfiles`). Cannot submit tasks when
    `anyone_may_propose: false` (when `anyone_may_propose: true`, a
    `viewer` can submit a `proposed` task like any signed-in user),
    cannot approve/reject proposals, cannot answer in-run
    `approval_request`s, cannot post to interactive sessions they did
    not submit, and cannot register/admit nodes (`Store.Admit` requires
    `member` or `admin`).
  A group named like
  an email (`group:alice@corp.example`) is still only a group, so an
  IdP that lets users create or rename groups cannot make one stand in
  for a user, or for a group an admin listed under `user:` (SA1-L6).
  A principal with neither prefix is refused: Google groups look like
  emails, so the hub does not guess. Rows from before principals had
  prefixes are made `user:` when the tables are migrated (the only
  deployment then was behind IAP, which has no groups).
- `team_settings(team, lost_requeue_after_seconds, anyone_may_propose, require_approval)`:
  per-team settings including `lost_requeue_after_seconds` (0 for never),
  `anyone_may_propose` (default `true`; when `false`, only `admin`,
  `member`, or `proposer` on the owning team may submit a task to that
  team or its repositories, and any other caller receives `403 Forbidden`),
  and `require_approval` (default `false`; when `true`, even tasks
  submitted by a `member` or `admin` of the owning team are filed in
  state `proposed` and require two-person approval from a different
  `member` or `admin` of the team before entering `queued`).
- `team_repos(repo, team, history)`, keyed by repository, so every
  repository has at most one owning team. Assigning a repository that
  another team owns is refused: it has to be released first, so no team
  can take another team's repository. `history` says whether the team
  was given the earlier tasks recorded for the repository (below).
- `node_tokens(id, hash, owner, node, created_at, revoked_at,
  released_at)`, with at most one live token per node ID, and an ID's
  unreleased rows all of one owner (§5.2).

These rules govern which users and teams can see and interact with which
Grain deployments:

- `Admit` lets a node serve or subscribe to a repository only if the
  node's owner is a member (or admin) of the team that owns it. A
  repository with no owning team is refused, so nothing is served by
  accident. Admission is by the owner's email only, as `user:<email>`: a
  node token proves a principal, not the groups the IdP put it in when
  it signed in.
- **A node that may serve only some of its repositories is refused
  whole**, not registered for the admitted subset. The refusal names
  every repository it may not serve, and why (no owning team, or a team
  the owner is not in), so the owner can fix the node's list, or ask a
  team admin, in one round.
- A human sees the tasks, process runs, events, and nodes of their own
  teams (`Access`): `SeesTask` for a task or process run, its events and
  its artifacts, and `SeesNode` for the directory, where a node is
  visible to its own owner and to the members of any team that owns one
  of its repositories. The node directory and the teams list are
  filtered this way. A team the caller is not in answers 404, as one
  that does not exist would.
- **Only a hub admin assigns a repository to a team** (SA1-M2). Team
  admins manage their team's members and settings, and may release a
  repository of their team, but not claim one: nothing ties a
  repository to a team except the assignment, so a team admin who
  could assign would claim, first come first served, any repository no
  team owned at that moment.
- **A task stays with the team it was recorded under.** `SeesTask` shows
  a task to the members of the team recorded on it (`Task.Team`) and, for
  a repository's task, to the members of the team that owns the
  repository now only if that team was given the repository's history:
  a hub admin's `PUT .../repos/{owner}/{name} {"history": true}`
  (`Store.TransferRepo`). An assignment without it, the default, shows
  the new team only the tasks recorded under it, so neither a release
  nor deleting a team hands a repository's past prompts, conversation
  and events to whoever owns it next.
- Every change to who owns what is logged as a `directory: audit:`
  line naming the signed-in caller: deleting a team, assigning a
  repository (with whether its history went with it), releasing one.
- A hub-wide admin role creates and deletes teams and sees everything.
  It is the hub's configuration (`API.HubAdmins`, `SILO_HUB_ADMINS`),
  not a row a team admin could grant. Its entries are principals as
  above, `user:<email>` or `group:<name>`, each matched only against
  its own claim.

The directory's HTTP API (`directory.API`):

```
GET    /v1/me                                          the caller, their teams, roles, theme, and inbox_nodes
PUT    /v1/me/theme {"theme":"light"|"dark"}           the caller's resolved light/dark theme
GET    /v1/me/inbox                                    the caller's selected Unified Inbox deployments
PUT    /v1/me/inbox {"nodes":[...]}                    update the caller's selected Unified Inbox deployments
GET    /v1/users                                       sys admin: accepted users allowlist and sys-admin roles
PUT    /v1/users/{email} {"sys_admin":bool}            sys admin: add or update an accepted user
DELETE /v1/users/{email}                               sys admin: remove an accepted user (refuses last sys admin)
GET    /v1/deployments                                 the caller's deployments (all, for a sys admin)
POST   /v1/deployments {"id","name"}                   any accepted user (claims open id as admin)
GET    /v1/deployments/{id}                            one deployment as /v1/deployments lists it: its members, or a sys admin
DELETE /v1/deployments/{id}                            deployment admin or sys admin; its repositories are left unowned
PUT    /v1/deployments/{id}/members/{principal} {"role"}   deployment admin; principal is user:EMAIL or group:NAME
DELETE /v1/deployments/{id}/members/{principal}        deployment admin
PUT    /v1/deployments/{id}/repos/{owner}/{name} [{"history": true}]   deployment admin (for unowned repos) or sys admin
DELETE /v1/deployments/{id}/repos/{owner}/{name}       deployment admin
GET    /v1/deployments/{id}/settings                   its members (§5.3, §6.6.1)
PUT    /v1/deployments/{id}/settings {"lost_requeue_after_seconds","anyone_may_propose","require_approval"}   deployment admin (§5.3, §6.6.1)
POST   /v1/deployments/{id}/token [{"replace_host": true}]   deployment admin or sys admin: mint a connection token for {id}
POST   /v1/deployments/{id}/replace-host               deployment admin or sys admin: mint replacement token, reclaim active tasks, and replay Grainfiles
GET    /v1/teams                                       backward-compatible alias for /v1/deployments
POST   /v1/teams {"id","name"}                         backward-compatible alias for POST /v1/deployments
GET    /v1/teams/{team}                                backward-compatible alias for GET /v1/deployments/{id}
DELETE /v1/teams/{team}                                backward-compatible alias for DELETE /v1/deployments/{id}
PUT    /v1/teams/{team}/members/{principal} {"role"}   backward-compatible alias for PUT /v1/deployments/{id}/members/{principal}
DELETE /v1/teams/{team}/members/{principal}            backward-compatible alias for DELETE /v1/deployments/{id}/members/{principal}
PUT    /v1/teams/{team}/repos/{owner}/{name} [{"history": true}]   backward-compatible alias for PUT /v1/deployments/{id}/repos/{owner}/{name}
DELETE /v1/teams/{team}/repos/{owner}/{name}           backward-compatible alias for DELETE /v1/deployments/{id}/repos/{owner}/{name}
GET    /v1/teams/{team}/settings                       backward-compatible alias for GET /v1/deployments/{id}/settings
PUT    /v1/teams/{team}/settings {"lost_requeue_after_seconds","anyone_may_propose","require_approval"}   backward-compatible alias for PUT /v1/deployments/{id}/settings
GET    /v1/node-tokens                                 the caller's (all, for a hub admin)
POST   /v1/node-tokens {"node"}                        mint one for the caller (or for deployment {node} when caller is admin)
DELETE /v1/node-tokens/{id}                            its owner, or a hub admin
```

`GET /v1/teams/{team}` was added for the web UI's team page
([ui.md](ui.md) §8), which would otherwise list every team to show one;
like every other route, it answers 404 for a team the caller is not
in. The web UI uses only these routes, §6.6's, `GET /v1/nodes` (§4.2)
and §7's, as any client can, and calls them only through the
TypeScript client SDK (`sdk/ts`, [ui.md](ui.md) §11), whose tests fail
on a route this document does not name.

## 6. Grainfile discovery, subscriptions, and interactive UI actions

Now that the **Grainfile Format** ([`docs/grainfiles.md`](grainfiles.md),
[`docs/grainfiles-spec.md`](grainfiles-spec.md)) is established, cross-repo
tasks and standing processes are authored directly in git as
`<name>.task.md` and `<name>.process.md` files rather than submitted into
a separate Hub-owned imperative task queue.

### 6.1 Git is the source of truth and source authority

In the Grainfile model ([`docs/grainfiles-spec.md`](grainfiles-spec.md)):

- **Two directly executed kinds:**
  - **`<name>.task.grain.md` (`task`):** A discrete 1-to-1 unit of work to execute
    once. Its YAML front matter declares metadata and references; its
    Markdown body is the prompt given directly to the run.
  - **`<name>.process.grain.md` (`process`):** A standing, reusable 1-to-many process
    with structured triggers (`when`) and an
    optional `gate: true` flag for commit/pull-request gating. Its
    Markdown body is the prompt dispatched whenever the process fires.
- **Authoritative source and `recipient` (`.task.grain.md` only):** The authoritative source of
  a `.task.grain.md` or `.process.grain.md` is the `https://` URL of the git
  repository and folder where the file lives
  ([`docs/grainfiles-spec.md`](grainfiles-spec.md) §6.2.3, §11.1). On
  `.task.grain.md`, the optional `recipient` field (defaulting to `/`,
  or a plain `https://` deployment repository or `/tree/<ref>/<folder>`
  URL, or a repo-local path) names the Grain deployment intended to
  execute the task, and the optional `requestor` field records
  attribution (`Name <email>` or a plain `https://` URL). On
  `.process.grain.md`, `recipient` and `requestor` are prohibited so any
  number of deployments can subscribe to the process by `repo`,
  enclosing `folder`, or `grainfile_url`.
- **Local policy enforcement on each Grain deployment:** Because write
  access to a git repository and folder is governed by the forge's
  branch protection, merge rules, and `CODEOWNERS` reviews, the hub does
  not act as a Policy Decision Point (PDP) that signs JWT task claims.
  Instead, each Grain deployment enforces its own local policy on which
  repository and folder sources it trusts to send work to its
  `recipient` URL, failing closed on any source its policy does not
  admit.

#### 6.1.1 Why Grain Hub consumes external repositories (never hosts git repos)

Grain Hub **consumes** external git repositories (via a GitHub App or
forge webhooks and API) rather than hosting git repositories itself:

1. **Forge-enforced source authority
   ([`docs/grainfiles-spec.md`](grainfiles-spec.md) §11.1):** Grainfile
   source authority relies on the external forge's branch protection,
   `CODEOWNERS`, and pull-request review on the repositories where code,
   documentation, and workflows already live. Hosting separate git
   repositories on the hub would duplicate or bypass the forge review
   and branch-protection boundary that governs write authority.
2. **Stateless with respect to git packfiles and no source code at
   rest:** Consuming external repositories via forge webhooks and API
   keeps the hub stateless with respect to git objects and packfiles and
   preserves the **no source code at rest** invariant (§1.1, §6.3): the
   hub database stores only Grainfile metadata and YAML front matter,
   never source trees, git packfiles, or file bodies.
3. **Decoupled deployment credentials:** Individual Grain deployments do
   not need to know about or hold standing git credentials for every
   other deployment's or team's repositories across an organization; each
   deployment only connects to the hub as its own `recipient` identity
   and receives notifications and authorized content through the hub
   (§6.4.1).

### 6.2 Subscriptions (`recipient` and watched repositories/processes)

A Grain deployment registers its subscriptions with Grain Hub in its
`hello` message when opening the control stream, and can update them at
any time with a `subscribe` message on the control stream
([hub-interface.md](hub-interface.md)):

- **`recipients`:** The deployment's own `recipient` identity URL(s)
  (for example, `https://github.com/acme/grain-eu` or
  `https://github.com/acme/deployments/tree/main/grain-eu`). The hub
  matches any `.task.grain.md` whose resolved `recipient` equals one of
  these URLs.
- **`repos` and `folders`:** Repositories (`https://github.com/owner/name`
  or `owner/name`) and repository folder paths (`https://.../tree/<ref>/<path>`)
  that the deployment watches for `.task.grain.md` and `.process.grain.md` files and
  forge trigger events.
- **`grainfiles`:** Specific shared `.process.grain.md` or `.task.grain.md`
  `https://` URLs the deployment subscribes to — including cross-repository
  or cross-deployment `depends_on` targets executed
  by other Grain deployments (§6.5).

Subscriptions are checked against `directory.Store.Admit` (§5.3) for the
node's authenticated owner so a deployment can only subscribe to
repositories and Grainfile sources its owner's teams are permitted to
access.

### 6.3 Forge webhooks and Grainfile metadata indexing (no source code at rest)

Because Grain deployments run on workstations, Cloudtops, or VMs behind
NAT and corporate firewalls — and should not have to poll every
repository in an organization — Grain Hub receives forge webhooks
(`push`, `pull_request`, `merge`) and indexes Grainfile metadata across
connected repositories:

- **What the hub indexes (metadata only):** For each `.task.grain.md` and
  `.process.grain.md` in a connected repository (discovered via initial
  repository tree scan on `installation` / `installation_repositories`
  or `SyncRepository`, and kept current on `push` / `merge` webhooks),
  the hub stores:
  - The file's canonical `https://` URL, repository URL, branch/ref,
    commit SHA, and repository-relative path.
  - The parsed YAML front matter ([`docs/grainfiles-spec.md`](grainfiles-spec.md)
    §5–§8):
    - `grainfile` (schema version, `"v1"`, default `"v1"`)
    - `recipient` (`.task.grain.md` only; resolved to a canonical `https://` deployment URL, default `"/"`)
    - `requestor` (`.task.grain.md` only; optional attribution)
    - `draft` (boolean, default `false`)
    - `not_before` (optional RFC 3339 UTC timestamp or `YYYY-MM-DD` UTC
      date, promoted automatically by the leader-leased `PromoteDeferred` ticker)
    - `references` (resolved list of `https://` or repo-local file or folder
      URLs)
    - `depends_on` (resolved list of `.task.grain.md` / `.process.grain.md` URLs)
    - `when` (`schedule`, `push`, `merge`, `pull_request`, `manual`,
      `scenario` trigger definitions on process files)
    - `gate` (boolean merge/commit gate flag on process files)
- **No source code at rest:** The hub never stores repository checkouts,
  source files, or Grainfile bodies at rest. It indexes only the
  front-matter metadata needed to match subscriptions, evaluate webhook
  triggers, and route cross-deployment dependency notices.

### 6.4 Webhook notifications and catch-up queries (`grainfile_event`, `trigger_event`, `catchup`)

When the hub processes a forge webhook or a node connects/reconnects
([hub-interface.md](hub-interface.md)):

1. **Grainfile addition, update, or removal (`grainfile_event`):**
   - When a `push` or `merge` adds or updates a non-draft (`draft: false`)
     `.task.md` or `.process.md` whose `recipient`, repository, folder,
     or URL matches a deployment's subscriptions, the hub sends a
     `grainfile_event` (`action: "added"` or `"updated"`) over the
     node's connection with the file's `https://` URL, `commit`, `ref`,
     and parsed front-matter metadata.
   - Grainfiles committed with `draft: true` are ignored for execution
     and trigger firing ([`docs/grainfiles-spec.md`](grainfiles-spec.md)
     §6.3). If a previously active Grainfile is edited to `draft: true`
     or deleted from the branch, the hub sends `grainfile_event` with
     `action: "removed"` so the subscribed deployment unregisters its
     triggers or withdraws the unstarted task.
   - If `not_before` is set ([`docs/grainfiles-spec.md`](grainfiles-spec.md)
     §6.4), it is carried in the front-matter metadata; the recipient
     deployment holds the task as deferred or keeps the process trigger
     inactive until `not_before` is reached.
   - Upon receiving `grainfile_event`, the subscribed Grain deployment
     verifies the source repository/folder against its local policy,
     fetches the `.task.md` or `.process.md` body and any authorized
     same-repo `references` at `commit` (on demand through the hub via
     `fetch_grainfile` / `fetch_reference`, §6.4.1, or directly from git
     when the deployment holds credentials for that repository), and
     executes or registers it.
2. **Forge trigger matching (`trigger_event`):**
   - When a forge webhook event (`push`, `pull_request`, `merge`)
     matches the `on` branch and path filters (`branches`, `paths`,
     `ignore`) of a non-draft `.process.md` that a deployment subscribes
     to (and whose `not_before`, if any, has passed), the hub sends a
     `trigger_event` notification to the subscribed deployment naming
     the `.process.md` URL, the trigger source (`push`, `pull_request`,
     `merge`), `gate`, and the commit/ref/PR details.
   - The subscribed deployment fetches the `.process.md` (and any
     authorized same-repo `references`) on demand through the hub or
     from git and dispatches a process run (holding the commit or pull
     request merge gate when `gate: true`).
3. **Catch-up on connect/reconnect (`catchup` / `catchup_result`):**
   - When a Grain deployment connects or reconnects, it sends `catchup`
     with its last seen cursor (or empty for a full snapshot). The hub
     replies with `catchup_result` containing all current non-draft
     `.task.md` and `.process.md` metadata matching the node's
     subscriptions and any missed trigger or dependency notices since
     that cursor.

#### 6.4.1 Cross-repository content access via `recipient` and `references`

A Grainfile in **Repo A** enables **Deployment B** (`recipient`) to read
content from Repo A through the hub without Deployment B holding
permanent git credentials for Repo A ([hub-interface.md](hub-interface.md),
[`docs/grainfiles.md`](grainfiles.md)):

- **Grainfile as a repo-scoped capability grant:** When a non-draft
  `.task.md` or `.process.md` is merged into **Repo A** with
  `recipient: <Deployment B>`, Repo A's owners (via Repo A's protected
  branch and `CODEOWNERS` review,
  [`docs/grainfiles-spec.md`](grainfiles-spec.md) §11.1) have explicitly
  authorized `<Deployment B>` to receive the Grainfile itself and to
  read the **Repo A** files and folders named in its `references` (as
  well as reading/working on Repo A when the Grainfile in Repo A asks
  `<Deployment B>` to act on Repo A).
- **Same-origin authority rule ("a repository can only grant access to
  itself"):** A Grainfile committed in **Repo A** can authorize
  `<Deployment B>` to read `references` inside **Repo A** (resolved at
  the commit/ref of the Grainfile or the pinned ref in the URL). It
  **cannot** grant `<Deployment B>` access to a third private repository
  (**Repo C**) merely by listing a `https://.../repo-c/...` URL in
  `references` — reading Repo C requires either `<Deployment B>` already
  having access to Repo C, or Repo C itself publishing/authorizing it.
- **On-demand fetch through the hub (`fetch_grainfile`,
  `fetch_reference`):** When `<Deployment B>` acts on a matched
  `.task.md` or `.process.md` from Repo A, it opens a stream to the hub
  ([hub-interface.md](hub-interface.md)) to fetch the `.task.md` /
  `.process.md` Markdown body and its authorized same-repo `references`
  on demand at the pinned commit/ref. The hub verifies that the calling
  node's admitted `subscriptions.recipients` includes the Grainfile's
  `recipient` (or that the node's owner is admitted to Repo A in
  `hub/directory`) and, for `fetch_reference`, that the requested
  reference is declared in the Grainfile's `references` and resolves
  within **Repo A** (same-origin check). The hub then reads the file or
  tree from the external forge API using its GitHub App / forge
  credentials and streams the bytes directly to the node — never storing
  Grainfile bodies or source files at rest in the hub database.

### 6.5 Cross-deployment `depends_on` routing

`.task.grain.md` and `.process.grain.md` files can form multi-step workflows and
qualification suites across repositories and across Grain deployments
using `depends_on`
([`docs/grainfiles-spec.md`](grainfiles-spec.md) §7.2.3, §8.3):

- **Subscription:** When Deployment A has a `.task.grain.md` or `.process.grain.md`
  that declares `depends_on: ["https://.../b.task.grain.md"]` or
  `depends_on: ["https://.../smoke.process.grain.md"]` executed by
  Deployment B, Deployment A includes that target Grainfile URL in its
  `subscriptions.grainfiles`.
- **Completion routing (`depends_on`):** When Deployment B reports on
  its event stream (§7.1, or `grainfile_complete`) that a run of `b.task.grain.md` or
  `smoke.process.grain.md` has finished (`result` with `done`, `failed`, or
  `cancelled`), the hub sends a `dependency_done` notice to Deployment A
  naming the `grainfile_url`, `run_id`, terminal `state`, and `reason`.
  Deployment A unblocks its waiting `.task.grain.md` or `.process.grain.md` run on
  `done`, or fails/cancels the dependent run according to its local
  policy on `failed` or `cancelled`.

### 6.6 Interactive actions from the multi-tenant UI

Through Responsibility 1's multi-tenant UI (`hub/ui`, [ui.md](ui.md)),
signed-in humans view tasks and process runs across all Grain
deployments their teams can access (`GET /v1/tasks`,
`GET /v1/tasks/{id}`) and trigger interactive actions that the hub
routes over `Server.Open` to the Grain deployment that owns the run
([hub-interface.md](hub-interface.md)):

- **Cancellations (`POST /v1/tasks/{id}/cancel {"reason"}`):** Authorized
  callers (the requester, a team admin, or a hub admin) can cancel an
  active run; the hub forwards `cancel` to the owning node (or queues it
  as `cancel_pending` if the node is temporarily offline).
- **In-run approvals (`GET /v1/tasks/{id}/approvals`,
  `POST /v1/tasks/{id}/approvals/{request_id} {"decision","reason"}`):**
  When a Grain deployment pauses before a sensitive operation and emits
  an `approval_request` conversation event (§7.1), a member of the
  owning team answers `allow` or `deny` in the hub UI, and the hub
  writes `approve` to the node over the tunnel.
- **Interactive session messages (`GET /v1/tasks/{id}/messages`,
  `POST /v1/tasks/{id}/messages {"id","text","attachments"}`):** For
  multi-turn interactive sessions (`"kind":"interactive"`), authorized
  users send chat messages through the hub UI. Attachments are
  references only (`{"name","ref","media_type","size"}`), never uploaded
  file bytes, preserving the no-code-at-rest invariant (§1.1). The hub
  routes `message` to the owning node, which acknowledges ingestion with
  a `message` event on its event stream (§7.1) and signals waiting state
  with `awaiting_reply` (`waiting` / `resumed`).
- **Shared chat sessions (`GET /v1/chats`, `POST /v1/chats`,
  `GET /v1/chats/{id}`, `PATCH /v1/chats/{id}`,
  `POST /v1/chats/{id}/messages`, `GET /v1/chats/{id}/events`):**
  Multi-user and multi-agent collaborative chat rooms (`docs/shared-chats.md`)
  backed by a primary deployment's standing interactive session plus optional
  `linked_deployments`, invited participants (`participant` / `viewer` from
  `accepted_users`), `@mention` vs `all` reply modes, attributed message
  delivery (`[sender]: text`), and extensible `external_bindings` (e.g.
  Google Chat spaces).

The Grainfile discovery (`grainfiles.API`), task, and interactive-action
HTTP API routes (`tasks.API`, verified by `sdk/ts/test/routes.test.ts`):

```
GET   /v1/inbox?nodes=node-1,node-2&kind=awaiting_reply|approval_request|proposal|failed|chat_mention   actionable items across the caller's selected deployments and shared chats
GET   /v1/grainfiles?kind=task|process&recipient=...&repo=...&draft=true|false&node=...&url=...   indexed Grainfile metadata, subscriptions, and events
POST  /v1/tasks {"repo" | "team", "kind", "title", "prompt", "constraints", "depends_on"}
GET   /v1/tasks?state=queued,running&team=infra&limit=100     the caller's teams' tasks, newest first
GET   /v1/tasks/{id}
POST  /v1/tasks/{id}/cancel {"reason"}                        its submitter, a team admin or a hub admin
POST  /v1/tasks/{id}/requeue                                  the same (current build)
POST  /v1/tasks/{id}/approve                                  a member of its team, not its submitter
POST  /v1/tasks/{id}/reject {"reason"}                        the same
GET   /v1/tasks/{id}/approvals                                the answers to its approval requests
POST  /v1/tasks/{id}/approvals/{request_id} {"decision","reason"}   a member of its team: allow or deny one
GET   /v1/tasks/{id}/messages                                 the messages sent to an interactive task
POST  /v1/tasks/{id}/messages {"id","text","attachments"}     its submitter or a member of its team: send it one
GET   /v1/chats                                               shared chat sessions the caller has access to
POST  /v1/chats {"title","deployment","linked_deployments","reply_mode","participants","repos","references","prompt","external_bindings"}
GET   /v1/chats/{id}                                          shared chat session metadata, participants, external bindings, and transcript
PATCH /v1/chats/{id} {"title","reply_mode","linked_deployments","participants","repos","references","external_bindings"}
POST  /v1/chats/{id}/messages {"id","text","attachments","trigger_agent"}
GET   /v1/chats/{id}/events                                   live SSE stream of shared chat messages, agent replies, approvals, and proposed tasks
```

#### 6.6.1 Legacy imperative backlog and signed claims in the current build (`hub/tasks`, `hub/claims`)

In the current Go codebase (`hub/tasks`, `hub/claims`, `cmd/silo`, and
`sdk/ts`), the routes `POST /v1/tasks`, `POST /v1/tasks/{id}/requeue`,
`POST /v1/tasks/{id}/approve`, `POST /v1/tasks/{id}/reject`, and
`GET /v1/teams/{team}/settings` / `PUT /v1/teams/{team}/settings
{"lost_requeue_after_seconds"}` along with the imperative dispatcher
(`offer`, `accept`, `refuse`, `resume`, `attest`, `file_task`) and JWT
claim-signing (`-claims-issuer` / `SILO_HUB_CLAIMS_ISSUER`,
`-claims-key-file` / `SILO_HUB_CLAIMS_KEY_FILE`, `GET /v1/claims/keys`)
remain wired in `silo hub serve` so that `grain-interop`
(`scripts/grain-interop.sh`), `silo conformance`, and the existing Go
and TypeScript SDK test suites continue to pass without disruption until
the code migration tasks replace `hub/tasks` and `hub/claims` with the
Grainfile webhook indexer and subscription router described in
§6.1–§6.5.

## 7. Live events and privacy (built: `hub/live`)

### 7.1 Streaming up

For each running task or process, the node opens one stream to the hub
(`NodeStream`) and writes the run's events on it, one JSON envelope
each (sequence number, time, type, payload), with the run's task ID (and
`grainfile` URL when executing a `.task.md` or `.process.md`) added.
Grain Hub sorts each event by class:

| Class | Examples | Stored by the hub | Broadcast live |
| --- | --- | --- | --- |
| Lifecycle | `status`, `result` (exit, reason), usage totals, `awaiting_reply` | yes | yes |
| Conversation | chat turns (`message`), `approval_request`, assistant final text | yes | yes |
| Token stream | `transcript` assistant text and thinking deltas | no: an in-memory ring per task, for late joiners | yes |
| Node-only (`SILO_ONLY`) | diffs, file contents, `tool_call` arguments and results, sandbox and tool logs | **never** | as a reference only |

The hub broadcasts to subscribers over
`GET /v1/tasks/{id}/events?from=<seq>`, as Server-Sent Events, or as a
WebSocket for the SDK. A subscriber replays what the hub holds after
`seq`, then follows.
Access is checked against the subscriber's teams at every subscription.

As built (`hub/live`):

- **The upstream stream.** Each line is one event with `"task"` added.
  The first line binds the stream to its task on that node; a stream for
  another node's task, or a later line naming another task, is closed.
  The hub writes nothing back. A node that reconnects may replay its
  task's events from the start: an event at or below the task's last
  `seq` is a duplicate and is dropped. The node keeps a task's `seq`
  increasing across the sessions it runs for it.
- **The classes.** `status`, `usage`, `resumable`, `result` and
  `awaiting_reply` are lifecycle. `approval_request` is conversation,
  and a member of the task's team answers it through the hub (§6.6).
  `message` is conversation: the hub keeps its own record of the
  message the event names, not what the node sent, and rejects one
  naming a message it never sent the task (§6.6). A `transcript` item of
  kind `text`, `thinking` or `error` is the token stream. Everything
  else is node-only: `tool_call`, transcript `tool_call` and
  `tool_result` items, `artifact`, any type the hub does not know, and
  any schema version it does not read. A `result`'s paths on the node's
  disk (`audit_path` and the transcript paths) are not kept.
- **Rejection.** A lifecycle, conversation or token event with a field
  outside its type's schema, at the top level or anywhere in its
  payload, is neither kept nor broadcast. A token item carrying a tool's
  `id`, `tool` or `args` is rejected the same way.
- **The ring.** Each task's token and node-only references are kept in
  memory, the last 1024 events or 1 MiB of them. A task's ring is
  dropped 10 minutes after its last stream and subscriber have gone.
- **Subscribers.** An SSE message is `id: <seq>`, `event: <type>` and
  `data: <the event>`. An `EventSource` reconnecting sends
  `Last-Event-ID`, which is the cursor when `from` is absent. A
  WebSocket gets one text message per event; a browser's must come from
  the hub's own origin. Both end with `{"type":"end"}` (SSE: `event:
  end`) after the task's `result`, or when the task is over and its
  node's last stream has closed. A subscriber that falls 512 events
  behind is dropped without `end`, and reconnects from its last `seq`.
  Seqs have gaps: rejected events, and token events the ring no longer
  holds, are not replayed.

### 7.2 Node-only (`SILO_ONLY`): view-through, never at rest

A node-only event reaches the hub as a reference only:
`{"type":"artifact","kind":"diff","ref":"<opaque>","size":...}`. When an
authorised viewer opens it:

1. The hub calls `Server.Open(node)` and writes
   `{"type":"view","ref":...}`.
2. The node streams the bytes, and the hub copies them to the viewer's
   HTTP response.
3. Nothing is written to the hub's disk or database, and no cache is
   kept.

If the node is offline, the artifact is unavailable. This is the
intended trade.

As built, a viewer asks `GET /v1/tasks/{id}/artifacts?ref=<ref>`, with
the same access check as the task's events. The hub asks the node only
for a ref the node itself listed in the task's artifact events, and
answers 404 to any other without reaching the node (audit SA1-M3): it
keeps the listed refs, and only the refs, in `task_refs`, the newest
4096 per task (`live.MaxRefs`), so they outlast the token ring and a
hub restart. The view stream carries one JSON line each way and then
the bytes:

```
hub → node  {"type":"view","task":"<hub id>","node_task":"<local id>","ref":"<ref>"}
node → hub  {"type":"ok"}, then the artifact's bytes until the node closes the stream
            or {"type":"error","error":"..."}   (answered 404)
```

The hub flushes each write to the viewer, and answers with `text/plain`,
`nosniff`, a sandboxing CSP and `Cache-Control: no-store`, so the bytes
are never rendered as the hub's own page or cached on the way. An
offline node answers 503. `live.ServeView` is the node's end. Whatever
the node streams, a node-only event reaches the hub's store in no
form: only its reference is broadcast, and the store itself refuses
any class but lifecycle and conversation.

The **node** classifies each event, because only the node knows what it
holds. The **hub** enforces the classification by schema. An event type
the hub does not know is treated as node-only, and its payload is
dropped. A payload field outside the stored classes' schemas is
rejected. Credential redaction runs on the node before anything leaves
it.

## 8. Security summary

The system's threat model, which audits and the security test suites
cite, is [threat-model.md](threat-model.md): its assets, actors, trust
boundaries, threats per boundary, numbered invariants (`INV-`) and the
audit plan. This section is Grain Hub's part of it in brief. The hub's
security test suite (`hub/security`), which asserts these properties
route by route and caller by caller, and says how to extend it, is
[security-tests.md](security-tests.md).

- **Nothing inbound to nodes.** Every connection is node → hub over
  TLS (`wss://<hub>/v1/nodes/connect`).
- **Node identity comes from the credential.** The hello carries no
  trusted owner. Node IDs are bound to their first owner. Admission and
  subscriptions are checked against the team directory (§5.3).
- **Git source authority, repo-scoped capability grants, and local policy
  enforcement** (§1.2, §6.1, §6.4.1,
  [`docs/grainfiles-spec.md`](grainfiles-spec.md) §11.1). Cross-repo
  `.task.md` and `.process.md` Grainfiles take their authority from the
  external git repository and folder where they live, protected by the
  forge's branch protection and `CODEOWNERS`. A non-draft Grainfile in
  **Repo A** naming **Deployment B** as `recipient` acts as a
  repo-scoped capability grant authorizing Deployment B to fetch the
  Grainfile and its same-repo `references` in Repo A through the hub on
  demand, subject to the **same-origin authority rule** ("a repository
  can only grant access to itself" — Repo A can never grant access to a
  third private repository Repo C). Each Grain deployment is the PEP
  that verifies repository/folder source authority against its own local
  policy, failing closed.
- **The hub can reach a node only through the streams the protocol
  defines** ([the contract](hub-interface.md)): Grainfile/trigger/dependency
  notifications, interactive UI actions (`cancel`, `approve`,
  `message`), artifact `view`, and, where the node's owner has turned it
  on, `http` ([hub-interface.md](hub-interface.md)). A node serves only
  those, so a compromised hub cannot read a node's files beyond the
  artifacts that node has itself listed, and what its own UI shows a
  signed-in user (§11).
- **No source code at rest on the hub** (§1.1, §6.1.1, §6.3, §6.4.1,
  §7.2). The hub consumes external repositories via forge webhooks and
  API rather than hosting git repositories itself; neither the Grainfile
  metadata index nor the event store holds git packfiles, repository
  checkouts, Grainfile bodies, file contents, diffs, or sandbox/tool
  logs at rest.

## 9. Stages

| Stage | What | Where |
| --- | --- | --- |
| 2.1 | This design. The registry (`hub/registry`). The rendezvous: WebSocket, auth hook, admission hook, heartbeats, supersession, streams both ways, client with reconnect (`hub/rendezvous`). | here, built |
| 2.2 | Human sign-on (IAP JWT verification and OIDC, `hub/identity`). Node tokens, the team directory, `Admit` and per-team visibility, in a store (`hub/directory`). | here, built |
| 2.3 | Initial task store, intake API, and interactive action streams (`hub/tasks`). | here, built (migrating to Grainfile subscriptions, §6) |
| 2.4 | Event streaming up (`Hub.NodeStream`), the classification by schema (`live.Classify`), the event store, the token-stream ring, fan-out to SSE and WebSocket from a cursor, and the view-through (`hub/live`). | here, built |
| 2.5 | `silo hub serve` (`hub/server`, `cmd/silo`): every part above as one server, health and readiness, graceful shutdown with `goodbye`. Its image, GKE manifests and terraform for GKE behind IAP (§2.1, §5.2.1, `deploy/`, [deploy.md](deploy.md)). | here, built (not applied to any project) |
| 2.6 | The hub's multi-tenant human web UI across deployments ([ui.md](ui.md)): built in `hub/ui` and `ui/`. | here, built |
| 2.7 | A node's own UI through the hub, to its owner: the `http` stream, per-node hosts, `-node-ui-domain` (`hub/nodeui`, §11). | here, built |
| 2.8 | Production readiness: Prometheus metrics on their own listener and a `PodMonitoring`, Cloud Monitoring alert policies and dashboard, Cloud SQL backups and point-in-time recovery with a tested restore, and the recovery time of each failure (§2.2) ([hub-operations.md](hub-operations.md)). | here, built (not applied to any project) |
| 2.9 | Several replicas, in stages (§2.2): R1, database lease (`hub/lease`) and a REGIONAL Cloud SQL option; R2, the shared registry (`hub/registry`'s `Store`); R3, `Server.Open` across pods (`hub/rendezvous`'s internal listener); R4, wake-ups and live events across pods (`hub/notify`); R5, the rolling Deployment. | here, built (not applied to any project) |
| 2.10 | The Grainfile Format (`docs/grainfiles.md`, `docs/grainfiles-spec.md`, `schemas/grainfiles/`, `tools/grainfiles/grainlint.py`, `examples/grainfiles/`) and redefining Grain Hub around multi-tenant UI/reachability and `.task.md` / `.process.md` discovery and subscriptions (§1, §6, [hub-interface.md](hub-interface.md)). | spec & docs built; code migration from `hub/tasks` & `hub/claims` to Grainfile subscription router next |
| 2.11 | Future, designed, not filed: a Grain's UI opened from the hub UI by its owner or its owning team's admins, read-only by default at the Grain, with a capability in `hello`, a separate IAP backend and audience for node hosts, and audit ([grain-ui-proxy.md](grain-ui-proxy.md), stages S1–S5 here and G1–G4 in grain). It amends §11.2–§11.3: the IAP assertion and cookies are no longer passed to the node. | here, then bwsalmon/grain |

## 10. Open questions

- **Team members' views of a node's UI.** §11 lets in a node's owner
  only. A team member who `SeesNode` could later be given a read-only
  view, but the hub cannot tell a read from a write in grain's UI (a
  GET can be a long poll; a WebSocket carries both), so it would have
  to be grain's decision: the hub passing `viewer` and the IAP
  assertion, and the Grain serving that viewer as its own access list
  says. Until grain has a read-only role to map them to, owners only.
  [grain-ui-proxy.md](grain-ui-proxy.md) proposes the answer for team
  admins: the hub passes the viewer's role, and the Grain serves hub
  sessions read-only unless its owner opts in.

## 11. A node's UI through the hub (built: `hub/nodeui`; grain's side to come)

(Extended for team admins, read-only at the Grain, and the hub UI's
button by [grain-ui-proxy.md](grain-ui-proxy.md), a design under review.)

### 11.1 What grain's relay carries

grain's UI can be reached from outside a node's machine through
grain's relay (`grain-relay`, `pkg/relay` and `cmd/grain-relay` in
bwsalmon/grain, deployed by its `deploy/terraform/relay`). As built
there:

- **The tunnel.** The daemon dials `/relay/daemon` (a WebSocket
  upgrade carrying grain's `pkg/tunnel/mux`, the mux this repository
  copied into `internal/tunnel/mux`) and holds it. Cloud Run ends every
  request at the service's timeout, so the daemon rotates its tunnel
  make-before-break every 240s, and checks `/relay/status` every ~20s
  to re-attach after a redeploy. The service runs exactly one instance,
  because the tunnel lives in that instance's memory.
- **What rides it.** Every request to the relay's host that is not one
  of its own paths (`/relay/*`, `/_grain/*`, `/healthz`) is the UI's:
  the relay opens a mux stream, tags it `http`, writes the request on
  it as HTTP/1.1 (one request per stream), reads the response back and
  flushes each write, so SSE streams. A `101 Switching Protocols`
  splices the browser's connection to the stream both ways, which is
  how WebSockets pass. The relay also carries **remote executors**:
  `/relay/executor` streams, tagged `executor`, to the daemon's
  executor handler.
- **No path prefix.** The UI is served at the relay host's root, and
  it uses absolute paths (`/api/...`, `/grain-mark-light.png`). It
  cannot be put under a prefix without changing grain's UI.
- **Who the viewer is.** The relay decides nothing about it. IAP admits
  identities to the whole Cloud Run service; the relay strips the
  plaintext identity headers only a client could have set
  (`X-Forwarded-User`, `X-User-LDAP`, `X-Grain-User`, `Grain-User`)
  and passes the rest, IAP's `X-Goog-IAP-JWT-Assertion` and grain's own
  session cookie or bearer token among them. The daemon verifies the
  assertion against its own `GRAIN_IAP_AUDIENCE` (the relay's), or
  takes its own sign-in, and applies its own teams and access list.
  It marks each relayed request (`via.Relay`) so that it is not
  mistaken for an in-process call, and `-ui-trust-relay` decides
  whether such requests count as loopback. grain keeps the relay's IAP
  binding in step with its teams (`docs/beyondcorp.md`).
- **The hour.** The relay's request timeout is 3600s, so each browser
  stream (an SSE subscription, a WebSocket) and each executor
  connection through it ends at an hour. The tunnel itself does not
  hit the cap; it rotates.

### 11.2 The hub's `http` stream

Once a node holds a rendezvous connection, Grain Hub carries the same
traffic over it. As built (`hub/nodeui`), with `silo hub serve
-node-ui-domain=nodes.hub.example.com` (`SILO_HUB_NODE_UI_DOMAIN`):

- **Each node has its own host**, `<node>.nodes.hub.example.com`, and
  every path on it is the node's UI, at its root as grain expects. A
  node's host never reaches the hub's API. This, rather than a route
  such as `/v1/nodes/{id}/ui/...`, is deliberate: grain's UI has no
  base path, and a node's pages served from the hub's own origin could
  script the hub's API (and every other node's UI) with the viewer's
  sign-on. On its own host, a node's UI reaches only itself. A node
  whose ID is not a DNS label (capitals, `.` or `_`) has no host.
- **Access.** The viewer is signed in by the hub's sign-on and their
  effective role (`owner`, `admin`, `member`, `proposer`, or `viewer`)
  on the deployment is resolved from ownership and `ui.team` (inferred
  from the node's single owning team when `ui.team` is omitted): any
  other caller, and a node the hub does not know, gets 404. An offline
  node is 503. Hub admins outside the team are not let in.
- **The stream.** The hub opens a stream with `Server.Open` and, like
  the artifact view (§7.2), writes one line and reads one:

  ```
  hub → node  {"type":"http","node":"ws-1","viewer":"alice@example.com","role":"owner","team":"infra","require_approval":false,"theme":"light"}
  node → hub  {"type":"ok"}, then the node reads one HTTP/1.1 request and writes its response
              or {"type":"error","error":"..."}   (answered 403)
  ```

  After `ok` the stream is an HTTP/1.1 connection for one request, as
  the relay's are: the hub writes the request, copies the response to
  the viewer, flushing each write, and on a 101 splices the two
  connections (WebSockets). Nothing is stored or cached. `viewer` is
  the hub's claim; a node that wants proof verifies the IAP assertion
  itself (below).
- **What is passed on.** The request goes as the browser sent it, Host
  included (the node's host), with `X-Forwarded-For`, `-Host` and
  `-Proto` set, and IAP's assertion. The hub drops its own credentials,
  `Authorization` and its OIDC cookies, which are not the node's to
  see, and the plaintext identity headers the relay drops. From the
  response it drops a `Set-Cookie` with a `Domain`, so one node's UI
  cannot set cookies on its siblings or on the hub. A WebSocket
  upgrade must come from the node's own origin (or none, which is not
  a browser).
- **Same site, other origin.** A node's host is its own origin, but
  under the hub's domain it is the hub's *site*, so the browser sends
  the hub's `SameSite=Lax` cookies (its OIDC session, and IAP's) on a
  node UI page's requests to the hub, and `Sec-Fetch-Site` says
  `same-site`, not `cross-site`. A Grain's UI could otherwise call
  write routes on the hub as its owner (audit SA1-M1). So every `POST`,
  `PUT` and `DELETE` of the directory's and the task APIs, and the OIDC
  sign-out, is refused unless `Sec-Fetch-Site` is absent (not a browser)
  or `same-origin`, any `Origin` is the hub's own scheme and host, and a
  body is `Content-Type: application/json` (`identity.SameOrigin`;
  415 for the last). Clients of the API send that content type. Where
  it can, a deployment puts the node UI domain on a different
  registrable domain from the hub's ([deploy.md](deploy.md) §8), so that
  the hub's cookies are not sent at all.
- **Sign-on.** The node hosts need IAP: an OIDC session cookie is the
  hub host's alone, so behind OIDC a node host signs nobody in. The
  same IAP backend serves both, so a node verifying the assertion uses
  the hub's audience.
- **Deployment.** The hub's Gateway needs the wildcard hostname, its
  Certificate Manager certificate a `*.nodes.<domain>` name (DNS
  authorization allows wildcards) and the DNS zone a wildcard record to
  the hub's address. `deploy/` has them (`node_ui_domain` in
  `deploy/terraform/hub`, the HTTPRoute `silo-hub-node-ui`);
  [deploy.md](deploy.md) §8 turns them on.

What it costs: the hub carries every UI byte, so its bandwidth and its
pods' capacity matter (§2.2), with one more hop when the browser's pod
is not the node's (§2.2.3); a hub outage takes the UIs with it, where
today the relay and the hub fail separately. The no-code-at-rest rule
(§1.1) holds, since nothing is stored, but much more code passes through
the hub than the artifact view carries.

### 11.3 The node's end (bwsalmon/grain, after the rendezvous client)

Grain answers a stream whose first line is `http` only if its owner has
turned the UI view-through on; otherwise it refuses (`nodeui.Refuse`,
which the viewer sees as 403). It serves the request with the same
handler the relay client hands relayed requests (`pkg/ui`'s server),
by `nodeui.Serve` or its own equivalent of the relay client's
`serveHTTP`, and:

- marks the request as coming through a public edge, as `via.Relay`
  does (a new `via.Hub`), so it is never an in-process call, and does
  not let `-ui-trust-relay` or loopback trust apply to it;
- adds the hub's IAP audience (`/projects/NUMBER/global/backendServices/ID`)
  to `GRAIN_IAP_AUDIENCE`, so that the viewer is whoever IAP signed,
  checked by the node and not only by the hub; grain's own sign-in still
  works through it, its cookie on the node's host;
- accepts the node's host in its host guard, as it does the relay's.

### 11.4 Retiring `grain-relay`

- **A node that uses Grain Hub** turns the UI view-through on, and its
  owner opens `https://<node>.nodes.<hub domain>`. Once that works, it
  stops dialling the relay (`-relay-url` unset) **unless it has remote
  executors that reach it through `/relay/executor`**: the hub carries
  no executor traffic, so such a node keeps the relay for its
  executors until executors have another way in. A relay serving only
  executors still needs its IAP and daemon allow-list, and no longer
  its UI members.
- **A node without a hub** keeps the relay exactly as it is.
- **A deployment none of whose nodes still dials the relay** removes
  `module "relay"` (`enable_cloudrun_relay = false`), with its load
  balancer or Cloud Run IAP, its token secret and its image
  repository.

Nothing in the relay changes: it is retired by not being used.
