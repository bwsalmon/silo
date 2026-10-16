# The Silo collection hub

This is the design of the Silo **hub**, which is all that this
repository holds (Track 2). Its first parts are built: the node
registry and the rendezvous (stage 2.1, `hub/registry` and
`hub/rendezvous`), identity and teams (stage 2.2, §5, `hub/identity`
and `hub/directory`), task intake and dispatch (stage 2.3, §6,
`hub/tasks`), and live events with the SILO_ONLY view-through (stage
2.4, §7, `hub/live`), and the `silo hub serve` command that runs them
as one server, with its deployment on GKE behind IAP (stage 2.5, §2.1,
`hub/server`, `deploy/`, [deploy.md](deploy.md)), and what it takes to
run in production: metrics, alerts and a dashboard, backups and their
restore, requeueing lost tasks, and the decision to stay on one
replica (stage 2.7, §2.2, §6.3, [hub-operations.md](hub-operations.md)).

## 1. What the hub is

**Grain** is the execution node. A Grain is **single-tenant and manages
one backlog**: its owner's (or one team's) tasks, queue, git checkouts,
sandboxes and agent sessions, and its own merge queue. Where a Grain
runs is a deployment detail: today a workstation, a Cloudtop or a VM,
later possibly a GKE cluster. Wherever it runs, it stays one tenant with
one backlog. In this document a **node** is one such Grain as the hub
sees it: one connection, one registry entry, one backlog behind it.

**The hub** links many Grains into one multi-tenant system. It is the
coordination plane for:

- **Identity:** who a human or a node is.
- **Teams:** which team owns which repositories.
- **Task federation:** tasks submitted to the hub, placed on a Grain
  that may run them.
- **Routing:** reaching a Grain that is behind NAT or a corporate
  firewall.

The hub **holds no source code at rest, runs no sandboxes and makes no
git checkouts.** Anything that can contain code (diffs, file contents,
tool output) stays on the node that produced it. The hub passes it
through to a viewer on demand and never stores it (§7.2). If the hub's
database were lost or leaked, it would expose task metadata,
conversation text and the directory, but no repository content.

### 1.1 Where this sits in the grain/silo division

A Grain is complete on its own: it owns its tasks, its backlog, its
runs, its sandboxes, its pull requests, its CI and its merge queue, and
nothing in Silo runs inside a Grain's run. Silo is only the hub, and the
hub does not change any of that for a single Grain. It adds a layer
above the Grains:

- Its backlog holds tasks that **have not been placed yet**. Once a
  task is placed, the Grain owns it as one of its own tasks, and the
  hub only mirrors its lifecycle (§6, §7).
- Its team directory decides **which Grains may serve which
  repositories, and who may see their tasks** (§5.3).
- **Phase 1:** a repository's tasks go to the Grain of the team that
  owns the repository, and that Grain's own merge queue lands them. The
  hub has no merge queue.
- Many hubs could federate in the same way. This design assumes one.

### 1.2 Policy: the hub decides, each Grain enforces

The hub is the **Policy Decision Point (PDP)**. It decides who may run
what: who may submit a task for which repository or team (§6.1), which
Grains are admitted to serve which repositories (§5.3), and where a task
is placed (§6.2). It puts **claims** on each task it dispatches. As
built, the offered task (`tasks.Spec`) carries the requester
(`submitter`, from the caller's sign-on) and the `team`. The other
claims of this model, `approved_by` and the `capabilities` the task is
granted, are not in `tasks.Spec` yet.

Each Grain is a **Policy Enforcement Point (PEP)**. It **takes the hub's
identity claims on trust**: it does not independently verify who
requested or approved a task. It holds every task to a **local policy
that only the Grain's owner can edit**: the repositories it serves, the
capabilities it grants, and the network it allows. A task the local
policy does not allow is refused (§6.2), and anything the policy does
not name is refused too: the Grain fails closed.

**The trust limit.** A compromised hub could forge a task's claims, for
example its requester or its approval. It could not get a task past a
Grain's local policy, so what a forged task can do is bounded by what
that Grain's owner has already allowed. Signed claims that a Grain
could verify independently of the hub are possible future work; the
status quo, trusted claims bounded by local policy, is the design for
now.

### 1.3 The task interface is the contract

The hub depends on nothing in a Grain except the **task interface** it
speaks over the rendezvous: register, accept or refuse a task with its
claims, heartbeat with its capacity, stream events, and report the
result. **Grains can differ.** A user may customise their own Grain, or
run different code altogether, as long as it conforms to that
interface. The interface is written down as a contract in one place,
with its messages as built in `hub/rendezvous` and `hub/tasks`:
[hub-interface.md](hub-interface.md).

## 2. Components

```
 humans, custom UIs, SDK                 Grains (one backlog each, on any machine)
        |  HTTPS (IAP / SSO)                    |  outbound HTTPS only: WebSocket
        v                                       v
 +--------------------------- hub ------------------------------------+
 | intake API (§6)     directory API     rendezvous server (§4)       |
 |      |                   |                  |                       |
 |   backlog ---> dispatcher ---> registry (§3) <-- heartbeats         |
 |      |  (store)        |          (memory)                           |
 |      |                 +--- Server.Open(node) ---> reverse stream    |
 |   event store <--- event fan-out (§7) <--- node-opened streams       |
 |   (metadata only)       |                                            |
 |                         +--> web UI / SDK subscribers                |
 |   team directory (§5, store)                                        |
 +----------------------------------------------------------------------+
```

The **store** is a database: SQLite for one replica, PostgreSQL for
more. It holds teams, the backlog and the event metadata. The
**registry** is in memory. Everything in it is restated by each node
when the node connects, so it would only go stale in a database. A hub
that restarts relearns its nodes within one reconnect backoff.

The hub is deployed on **GKE behind IAP**: a GKE Gateway fronts it
with Google's HTTPS load balancer, and IAP is enabled on that load
balancer's backend service for humans (§4.1, §5.1).

### 2.1 Serving and deployment (built: `hub/server`, `cmd/silo`, `deploy/`)

`silo hub serve` (`hub/server`) runs every part above as one HTTP
server over one database: the directory, the backlog and its
dispatcher, and the event store, each in its own tables of the same
PostgreSQL database (SQLite only for local development), with the
registry and rendezvous in memory. It is configured by flags or their
`SILO_HUB_*` environment variables, and needs exactly one human
sign-on: an IAP audience, an OIDC issuer, or, for development only, a
header taken on trust. `/healthz` is liveness; `/readyz` answers 200
while the database answers and the hub is not shutting down.

On SIGTERM the hub turns unready, sends each node a `goodbye` on its
control stream saying it is shutting down (§4.3), ends open event
subscriptions (which reconnect from their cursor), stops the
dispatcher and closes the stores, within 20 seconds. The node's client
logs the reason and reconnects with its usual backoff, and the new
hub's dispatcher resumes the node's running tasks (§6.2).
`silo hub team` and `silo hub token` bootstrap a new hub straight in
its database, before anyone can reach its API.

**Why GKE and not Cloud Run.** The node WebSocket lives as long as the
load balancer's backend timeout allows: a day, as deployed, where Cloud
Run ends every request at 60 minutes. The client reconnects cleanly
either way, because pods roll. GKE also leaves room for several
replicas later (§2.2): session affinity, and a registry that records
which pod holds each node. The shape is grain's own load balancer and
IAP setup (grain's `deploy/terraform/gcp/iap.tf`, and
`deploy/terraform/relay` with `iap_mode = "load_balancer"`). Cloud
Run remains possible for a small hub that accepts hourly reconnects:
it would serve the same image, with the node path left public and IAP
on the rest through a serverless NEG behind the same kind of load
balancer. It is not built.

**One replica.** The deployment runs one pod and rolls it with
`Recreate`, because the dispatcher assumes it is the only one (§6.2):
two overlapping pods would both recover the backlog. A roll therefore
costs nodes a reconnect of a few seconds. §2.2 is the decision to
stay that way, and what it costs.

**Metrics.** With `-metrics-addr` (`:9091` as deployed) the hub serves
Prometheus metrics at `/metrics` on a listener of its own, which no
Service names, so the load balancer and IAP never reach it. GKE's
Managed Service for Prometheus scrapes it (a `PodMonitoring`), and
Cloud Monitoring alerts on it and charts it (`deploy/terraform/hub/
monitoring.tf`). [hub-operations.md](hub-operations.md) lists the
metrics, the alerts and the dashboard.

[deploy.md](deploy.md) is the runbook: the terraform
(`deploy/terraform/hub`), the manifests (`deploy/kubernetes/hub`) and
how a Grain is pointed at the hub. [hub-operations.md](hub-operations.md)
is the runbook for running it: upgrades, rotating secrets, restoring
the database and reading the dashboard.

### 2.2 One replica, by decision

**Decision: the hub stays one replica.** It is a coordination plane,
not a data path: an hour of a hub that is down a minute at a time
costs nodes a reconnect and humans a retry, and loses nothing, because
everything that matters is in the database and everything in memory
is restated by the nodes (§2). Several replicas would buy availability
the hub does not yet need, at the price of the hub's hardest code.

**What several replicas would take**, which is why it is not built
here. Every node's connection is held by one pod, and the dispatcher
and the view-through reach a node only through the pod holding it
(`Server.Open`). So:

- The registry moves to the database, or each pod's registry learns
  the others', and each entry names the pod that holds the node's
  connection (and its address), with a lease so that a dead pod's
  nodes go offline.
- `Server.Open` on a pod that does not hold the node forwards to the
  pod that does: a pod-to-pod stream (an internal HTTP upgrade to the
  holder's pod IP), which the offer, resume, cancel and view streams
  ride.
- The dispatcher runs on one pod at a time, under a lease in the
  database (v1's leader lease, in this repository's history, is the
  basis), so that two pods never recover or place the same task; or
  each task's update takes a row lock and every pod dispatches.
- The live event fan-out (§7.1) moves to the database or a pub/sub,
  since a node's events reach one pod and its subscribers may be on
  another.
- The rollout becomes `RollingUpdate`, and the Gateway needs no session
  affinity: any pod serves any request.

That is a stage of its own; a follow-up proposal covers it, to be
taken up when one replica's recovery time below is no longer good
enough, or one pod's capacity (the view-through and, if §11 is taken
up, the relay's traffic) runs out.

**Recovery time as deployed.** One pod, a zonal Cloud SQL instance,
nodes that reconnect with backoff (1s doubling to a minute):

| Failure | What happens | Back in |
| --- | --- | --- |
| The hub process crashes or wedges | The kubelet restarts the container (liveness on `/healthz`); nodes reconnect; running tasks are lost until their nodes resume them, which they do on reconnecting | under a minute, plus up to a minute of node backoff |
| An upgrade | `Recreate`: goodbye, at most 20s of shutdown, the new pod starts (§2.1) | about a minute |
| The GKE node dies | The pod is rescheduled once the node is marked unreachable (the default 5-minute toleration) | 5 to 7 minutes |
| The Cloud SQL zone or instance fails | ZONAL Cloud SQL has no standby: the instance is restored from backup or to a point in time into a new instance, and the hub pointed at it ([hub-operations.md](hub-operations.md)) | 30 to 60 minutes, by hand; at most seconds of writes lost (point-in-time recovery) |
| The database is corrupted or wrongly changed | Point-in-time restore to before the change | the same |

Moving the instance to `availability_type = "REGIONAL"` (a standby in
another zone, failover in about a minute) is the next step if the
database's row matters, and is cheaper than several hub replicas.

## 3. The node registry (built: `hub/registry`)

A node registers with a `registry.Node`:

| Field | Meaning |
| --- | --- |
| `id` | Stable across the node's connections. 1-63 characters from `[A-Za-z0-9._-]`. |
| `repos` | The repositories (`owner/name`) the node executes tasks for. They are lower-cased, sorted and deduplicated. |
| `slots` | Concurrency: how many tasks the node runs at once. |
| `version`, `labels` | Shown in the directory. Labels can later become dispatch constraints. |

The hub adds what the node may not claim for itself:

- `owner`, the identity its credential proves (§4.2).
- `status`.
- `free`, as the node last reported it.
- `claimed`, the slots the dispatcher has reserved on the node.
- `connected_at` and `last_seen`.

**Status.** A node is in one of three states:

- `online`: connected, and it heartbeated within `StaleAfter` (45s by
  default, three heartbeats).
- `stale`: connected but silent. It may be wedged, so the dispatcher
  passes it over.
- `offline`: no connection. The entry stays, showing when the node was
  last seen, until `Forget`.

**Ownership.** A node ID belongs to the first owner that registered it.
A different owner naming the same ID is refused (`ErrOwner`), even
while the node is offline. A user cannot take over another user's node
or its repositories by guessing the ID.

**Supersession.** When the same owner registers an ID that is already
online, the new connection replaces the old one. The old lease's `Done`
closes, and the rendezvous drops the old connection. A late `Close` on
the old lease does not take the node offline. This lets a node that
lost its connection, before the hub noticed, come back at once.

**Claims.** `Claim(repo)` reserves one slot on the online node with the
most available slots (`free − claimed`). Two dispatches therefore never
count the same free slot. Each heartbeat reports the node's own free
count, and that count is taken to include every task it has accepted,
so the heartbeat settles all outstanding claims. If the node refuses a
task, the dispatcher releases that claim. Releasing a claim after the
heartbeat that settled it does nothing (each claim carries the
settlement epoch), so it cannot hand back a later claim's slot.

**Changes.** `Changes()` is a coalesced notification channel. The
dispatcher uses it to retry placement, and the UI uses it to push the
directory.

## 4. The rendezvous (built: `hub/rendezvous`)

### 4.1 Transport

The node dials out to `wss://<hub>/v1/nodes/connect`, a WebSocket with
the subprotocol `silo-node.v1`. The hub never dials a node, so nothing
has to be open inbound wherever the Grain runs. A WebSocket, rather than a raw
HTTP Upgrade or gRPC, is what Google's HTTPS load balancer and IAP pass
through unchanged. The hub runs on GKE, so that load balancer is the one
the GKE Ingress or Gateway provisions, with IAP enabled on its backend
service.

The WebSocket carries binary frames, and those frames carry the mux
in `internal/tunnel/mux` (from grain's `pkg/tunnel`). The mux provides independent, flow-controlled
streams and keepalive pings. A dead connection is noticed even when TCP
is not.

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

The node opens the first stream, and it carries JSON lines:

```
node → hub  {"type":"hello","node":{"id":"ws-1","repos":["bwsalmon/silo"],"slots":2,"version":"..."}}
hub → node  {"type":"welcome","owner":"alice@example.com","heartbeat_seconds":15}
            or {"type":"error","error":"registry: node belongs to another owner: ws-1"}, then close
node → hub  {"type":"heartbeat","free":1}       every heartbeat_seconds
hub → node  {"type":"goodbye","error":"the hub is shutting down"}, then close
```

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
  dispatcher offers a task (§6.2), and how a viewer reads a diff or log
  through (§7.2).
- **A node to the hub:** handed to `Server.NodeStream`. These are the
  node's event streams (§7.1). With none set, they are reset.

### 4.5 The node side

`Client.Connect` dials, says hello, waits for welcome and heartbeats
with `Free()`. `Client.Run` keeps the node connected:

- It reconnects with exponential backoff, up to one minute.
- It hands each stream the hub opens to the caller.
- It returns a refusal instead of retrying it.

Grain embeds this client. Grain's own change is a separate task in
bwsalmon/grain (§9).

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

Group membership comes from the IdP's `groups` claim (OIDC) or from the
hub's own team directory, where a group address can be listed as a
member (§5.3). IAP's assertion carries no groups.

### 5.2 Nodes

A node cannot do an interactive sign-in for every connection. Instead:

1. The owner runs `grain hub login` once, through the browser flow
   above (or device flow). It calls `POST /v1/node-tokens
   {"node":"<id>"}`.
2. The hub mints a **node token** bound to that owner and node ID
   (`silo-node_<id>_<secret>`, 256 bits of secret). It is shown once.
   The store keeps only its SHA-256, so a leaked database proves
   nobody. Minting again for the same node revokes the owner's earlier
   token. A node ID that another owner holds a live token for is
   refused, so node IDs stay bound to their owner across hub restarts,
   which the in-memory registry alone would forget.
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

What stays protected: the node backend serves only that one exact path,
so no human API is reachable without IAP; the hub refuses an
unauthenticated handshake with 401 before any WebSocket exists; and on
every other path the hub still verifies `X-Goog-IAP-JWT-Assertion`
against the IAP-enabled backend service's audience, so a request that
reached a pod some other way is refused too. (b) can still be layered
on later for nodes that run on GCP, without changing (a).

### 5.3 Teams

The store (`directory.Store`, SQLite for one replica, PostgreSQL for
more) holds:

- `teams(id, name)`.
- `team_members(team, principal, role: member | admin)`. A principal
  is an email or a group address.
- `team_repos(repo, team)`, keyed by repository, so every repository
  has at most one owning team. Assigning a repository that another team
  owns is refused: it has to be released first, so one team's admin
  cannot take another team's repository.
- `node_tokens(id, hash, owner, node, created_at, revoked_at)`, with at
  most one live token per node ID.

These rules follow:

- `Admit` lets a node serve a repository only if the node's owner is a
  member (or admin) of the team that owns it. A repository with no
  owning team is refused, so nothing is served by accident. Admission
  is by the owner's email only: a node token proves a principal, not
  the groups the IdP put it in when it signed in.
- **A node that may serve only some of its repositories is refused
  whole**, not registered for the admitted subset. The refusal names
  every repository it may not serve, and why (no owning team, or a team
  the owner is not in), so the owner can fix the node's list, or ask a
  team admin, in one round. A node registered for less than it asked
  for would sit in the directory looking healthy while the work it was
  set up for went nowhere. (This settles the question §10 used to ask.)
- A human sees the tasks, events and nodes of their own teams
  (`Access`): `SeesTeam` for a team-targeted task, `SeesRepo` for a
  repository's tasks and events, and `SeesNode` for the directory, where
  a node is visible to its own owner and to the members of any team
  that owns one of its repositories. The node directory and the teams
  list are filtered this way. A team the caller is not in answers 404,
  as one that does not exist would.
- Team admins assign repositories to their team and manage its members.
- A hub-wide admin role creates and deletes teams and sees everything.
  It is the hub's configuration (`API.HubAdmins`, emails or groups),
  not a row a team admin could grant.

Admission is checked when a node connects. A node that is already
connected keeps its registration if its owner later leaves the team;
the dispatcher (§6.2) checks `Admit` again before it offers a task.

The directory's HTTP API (`directory.API`):

```
GET    /v1/me                                          the caller, their teams and roles
GET    /v1/teams                                       the caller's teams (all, for a hub admin)
POST   /v1/teams {"id","name"}                         hub admin
DELETE /v1/teams/{team}                                hub admin; its repositories are left unowned
PUT    /v1/teams/{team}/members/{principal} {"role"}   team admin
DELETE /v1/teams/{team}/members/{principal}            team admin
PUT    /v1/teams/{team}/repos/{owner}/{name}           team admin
DELETE /v1/teams/{team}/repos/{owner}/{name}           team admin
GET    /v1/node-tokens                                 the caller's (all, for a hub admin)
POST   /v1/node-tokens {"node"}                        mint one for the caller
DELETE /v1/node-tokens/{id}                            its owner, or a hub admin
```

## 6. Task intake and federation (built: `hub/tasks`)

### 6.1 The backlog

`POST /v1/tasks` creates a task with these fields:

- **target:** `{repo}` or `{team}`. The caller must be in the team
  that owns the repository, or in the team (a hub admin may submit
  anywhere). A repository that no team owns is refused, because no node
  could be admitted to serve it.
- **prompt** and **title**. The title defaults to the prompt's first
  line.
- **submitter:** the principal who submitted it, from the caller's
  sign-on (§5.1), never from the request body.
- Optional **constraints**: node labels (`{"labels":{"zone":"eu"}}`,
  each of which the node must carry with that value), or a node ID
  (`{"node":"ws-1"}`).

Every task also records its **team**: the team it targets, or the team
that owned its repository when it was submitted. Dispatch is ordered
per team, and the team's members see the task.

Users and custom UIs submit tasks the same way, through the API or the
SDK. The task states are:

```
queued → offered → running → done | failed | cancelled
              ↘ queued (refused, or the node was lost before it accepted)
                          ↘ lost (the node was lost while the task ran)
                              → running (the node came back and resumed it)
                              → queued (the node came back without it, or a human requeued it)
```

Any state that has not finished can be cancelled.

The store (`tasks.Store`: SQLite for one replica, PostgreSQL for more,
and possibly the same database as the directory) keeps the task, its
state, the node it was placed on, the node's own ID for the task, the
reason for its state, the nodes that refused it, and the number of
offers made. It never stores a checkout, a diff or a log. Each update
applies only to the version of the row it read, so a human's cancel
and the dispatcher never both win.

The intake API (`tasks.API`) uses the directory's sign-on and
visibility (§5.3):

```
POST /v1/tasks {"repo" | "team", "title", "prompt", "constraints"}
GET  /v1/tasks?state=queued,running&team=infra&limit=100     the caller's teams' tasks, newest first
GET  /v1/tasks/{id}
POST /v1/tasks/{id}/cancel {"reason"}                        its submitter, a team admin or a hub admin
POST /v1/tasks/{id}/requeue                                  the same
```

A task the caller may not see answers 404, as one that does not exist
would. `requeue` returns a `lost` task to the queue. On a queued task,
it clears the nodes that refused it, so they are offered it again.

### 6.2 The dispatcher

The dispatcher (`tasks.Dispatcher`) wakes on a new task, on registry
`Changes()`, and every `Retry` (30s). It takes queued tasks oldest
first, going round the teams: one task from each team in turn, in the
order of each team's oldest task. A team with a long queue therefore
does not hold back another team's first task. For each task:

1. `ClaimFor(repos, ok)`: the task's repository, or every repository
   its team owns. `ok` passes over nodes that do not meet the
   constraints, and nodes that refused the task. If no node has a free
   slot, the task waits.
2. `Admit` is checked again for the node's owner, against the
   repositories the task may run in, because the owner may have left
   the team since the node connected (§5.3). A node that is no longer
   admitted has its claim released and is passed over for `Retry`.
3. The task is marked `offered`. `Server.Open(node)` opens a task
   stream, and the dispatcher writes `{"type":"offer","task":{...}}`.
4. The node's grain answers on the stream:
   - `accept` with its local task ID: the task is `running`, and the
     next heartbeat settles the claim.
   - `refuse` with a reason, for instance because the Grain's local
     policy does not allow the task (§1.2): the claim is released,
     and the task returns to the queue with that node excluded. It is not offered
     to that node again until a human requeues it. A task that every
     eligible node has refused waits.
   - No answer within `OfferTimeout` (a minute) counts as a refusal.
5. If the node goes offline while the task is `offered`, the claim is
   released and the task is requeued, with the node passed over for
   `Retry` but not excluded. If it goes offline while the task is
   `running`, the task is marked `lost` and stays with its node, unless
   its team has turned on requeueing lost tasks (§6.3). When the node is
   next online, the dispatcher opens a stream and writes
   `{"type":"resume","task":{...}}`. The node answers `accept` (the
   task is `running` again, on a new stream), `result` (it ended while
   the node was away), or `refuse` (the node does not have it, so
   nobody is running it, and the task is requeued for any node).

A team-targeted task goes to any node that serves one of the team's
repositories. That node's grain chooses the repository, as it does for
a task without one.

A task stream stays open for the task's life. The node ends the task
by writing `{"type":"result","state":"done"|"failed","reason":...}`,
and the hub then closes the stream. Cancelling the task marks it
`cancelled` at once and writes `{"type":"cancel"}` down the stream.
A task whose node is away when it is cancelled stays `cancel_pending`.
When the node is back, the cancel is written as the only message on a
stream of its own, `{"type":"cancel","task":{"id":...}}`.

The whole stream protocol, one JSON message per line:

```
hub → node  {"type":"offer","task":{...}}
            {"type":"resume","task":{...}}
            {"type":"cancel","task":{"id":...}}
node → hub  {"type":"accept","node_task":"<local id>"}
            {"type":"refuse","reason":"..."}
            {"type":"result","state":"done"|"failed","reason":"..."}
```

When the dispatcher starts, it has no streams. A task that was
`offered` is requeued, and a task that was `running` is `lost` until
its node resumes it. The dispatcher assumes it is the only one (one
hub replica, §2.2).

### 6.3 Lost running tasks: requeued after a team's grace, off by default

A running task whose node goes away is `lost` (§6.2), and stays with
its node: when the node comes back, it is asked to resume the task. The
question is what happens when the node does not come back.

**The trade.** Requeueing a lost task to another node risks **duplicate
work**: the first node may only be cut off from the hub, and still be
running the task, pushing its branch or opening its pull request,
while a second node starts it again. Two runs of one task can push
conflicting branches, open two pull requests, or each spend the task's
budget. Never requeueing risks a **stuck task**: a node that is gone
for good (a laptop closed for the weekend, a VM deleted) keeps its
tasks `lost` until a human notices and requeues them (`POST
/v1/tasks/{id}/requeue`), and nothing else of the team's backlog waits
on them, but their submitters do.

**Decision.** The hub requeues a lost task automatically **only for a
team that has turned it on**, after a grace the team sets, and the
default is **off**. The reasons:

- The hub cannot tell a dead node from a cut-off one, and a node's own
  runs keep going without the hub: a Grain's task does not stop when
  its rendezvous connection drops. Duplicate work is therefore the
  likely outcome of an early requeue, not an edge case.
- Duplicate work is worse than a stuck task for most teams: a stuck
  task is visible (the backlog's `lost` count, §2.1's metrics) and a
  human fixes it with one call; a duplicate pushes to a repository,
  which is not undone by the hub.
- The right grace depends on the team's nodes. A team of always-on
  VMs can say "an hour away is dead"; a team of laptops cannot. So it
  is a team's setting, not the hub's.
- The hub itself restarting marks every running task lost at once
  (`Recover`), and nodes take up to a minute to reconnect. The grace
  has a floor of five minutes (`directory.MinLostRequeueAfter`), so a
  hub restart never requeues anything.

**As built.** A team admin sets the grace with `PUT
/v1/teams/{team}/settings {"lost_requeue_after_seconds": N}` (`GET`
reads it; 0 turns it off), or `silo hub team -id T
-lost-requeue-after 2h` on the database. It is kept in
`team_settings`, which is deleted with its team. On each pass, the
dispatcher (`Dispatcher.LostRequeueAfter`, filled by
`directory.Store.LostRequeueAfter`) looks at each lost task whose node
is not online: once the task has been lost for longer than its team's
grace, measured from when it was marked lost, it goes back to the queue
(`Store.RequeueLost`) for any node, its old node included, with a
reason saying why. The update applies only if the task is still lost
since that moment, so a task its node resumed in the meantime is left
alone. The node is not excluded: if it comes back first, it may be
offered the task afresh. It is not told the task was taken from it,
since it is away; when it comes back it is not asked to resume the
task, and events it streams for it are refused (§7.1), because the
task is no longer placed on it. Telling a returning node to drop a
task that was requeued from it (a cancel owed to the old node) is the
refinement that would make the duplicate stop early; it is not built.


## 7. Live events and privacy (built: `hub/live`)

### 7.1 Streaming up

For each running task, the node opens one stream to the hub
(`NodeStream`) and writes the run's events on it, one JSON envelope
each (sequence number, time, type, payload), with the hub's task ID
added. The hub sorts each event by class:

| Class | Examples | Stored by the hub | Broadcast live |
| --- | --- | --- | --- |
| Lifecycle | `status`, `result` (exit, reason), usage totals | yes | yes |
| Conversation | chat turns, `approval_request`, assistant final text | yes | yes |
| Token stream | `transcript` assistant text and thinking deltas | no: an in-memory ring per task, for late joiners | yes |
| Node-only (SILO_ONLY) | diffs, file contents, `tool_call` arguments and results, sandbox and tool logs | **never** | as a reference only |

The hub broadcasts to subscribers over
`GET /v1/tasks/{id}/events?from=<seq>`, as Server-Sent Events, or as a
WebSocket for the SDK. A subscriber replays what the hub holds after
`seq`, then follows.
Access is checked against the subscriber's teams at every subscription.

As built (`hub/live`):

- **The upstream stream.** Each line is one event with `"task"` added.
  The first line binds the stream to its task, which must be one the
  dispatcher placed on that node; a stream for another node's task, or
  a later line naming another task, is closed. The hub writes nothing
  back. A node that reconnects may replay its task's events from the
  start: an event at or below the task's last `seq` is a duplicate and
  is dropped. The node keeps a task's `seq` increasing across the
  sessions it runs for it.
- **The classes.** `status`, `usage`, `resumable` and `result` are
  lifecycle. `approval_request` is conversation. A `transcript` item of
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

### 7.2 SILO_ONLY: view-through, never at rest

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
the same access check as the task's events. The view stream carries
one JSON line each way and then the bytes:

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
rejected. Credential redaction (design §8.4) runs on the node before
anything leaves it.

## 8. Security summary

- **Nothing inbound to nodes.** Every connection is node → hub over
  TLS.
- **Node identity comes from the credential.** The hello carries no
  trusted owner. Node IDs are bound to their first owner. Admission is
  checked against the team directory.
- **The hub decides, each Grain enforces** (§1.2). The hub is the PDP
  and puts claims on each task; the Grain is the PEP, takes those
  claims on trust and holds every task to its owner's local policy,
  failing closed.
- **The hub can reach a node only through the streams the protocol
  defines:** offer, resume, cancel and view ([the
  contract](hub-interface.md)). A node serves only those, so a
  compromised hub can ask a node to run a task but cannot read the
  node's files beyond the artifacts that node has itself listed. It
  could forge a task's claims (requester, approval), but it cannot get
  a task past the Grain's local policy. Claims a Grain verifies
  independently are future work, not built.
- **No code at rest on the hub** (§7.2). The hub's store holds
  metadata and conversation text only.

## 9. Stages

| Stage | What | Where |
| --- | --- | --- |
| 2.1 | This design. The registry (`hub/registry`). The rendezvous: WebSocket, auth hook, admission hook, heartbeats, supersession, streams both ways, client with reconnect (`hub/rendezvous`). | here, built |
| 2.2 | Human sign-on (IAP JWT verification and OIDC, `hub/identity`). Node tokens, the team directory, `Admit` and per-team visibility, in a store (`hub/directory`). | here, built |
| 2.3 | The backlog and its store, the intake API, the dispatcher (per-team ordering, constraints, admission rechecked, refusal, lost nodes, resume, cancel) and the task-stream protocol (`hub/tasks`). `registry.ClaimFor`. | here, built |
| 2.4 | Event streaming up (`Hub.NodeStream`), the classification by schema (`live.Classify`), the event store, the token-stream ring, fan-out to SSE and WebSocket from a cursor, and the view-through (`hub/live`). | here, built |
| 2.5 | `silo hub serve` (`hub/server`, `cmd/silo`): every part above as one server, health and readiness, graceful shutdown with `goodbye`. Its image, GKE manifests and terraform for GKE behind IAP (§2.1, §5.2.1, `deploy/`, [deploy.md](deploy.md)). | here, built (not applied to any project) |
| 2.7 | Production readiness: Prometheus metrics on their own listener and a `PodMonitoring`, Cloud Monitoring alert policies and dashboard, Cloud SQL backups and point-in-time recovery with a tested restore, requeueing lost tasks per team (§6.3), and the one-replica decision (§2.2) ([hub-operations.md](hub-operations.md)). | here, built (not applied to any project) |
| 2.6 | Future, not decided: the hub's human web UI for filing, approving and watching tasks across teams. [ui.md](ui.md) compares a new UI in silo with grain's UI pointed at the hub. | silo, or bwsalmon/grain |
| — | Grain embedding `rendezvous.Client`, answering offers, resumes and cancels (§6.2), streaming events | bwsalmon/grain |

## 10. Open questions

None open now. The two that were here are decided: lost running tasks
are requeued only after a grace a team turns on (§6.3), and the hub
stays one replica, with its recovery time written down and several
replicas a follow-up (§2.2).

## 11. The relay and the hub

grain's UI can be reached from outside a node's machine through
grain's relay (`grain-relay`, on Cloud Run): the node holds an outbound
connection to the relay, and the relay carries a viewer's requests to
the node's UI through it. Once a node also holds a rendezvous
connection to the hub, it keeps two outbound tunnels that do the same
thing: a reverse path from a cloud service to the node.

**The hub could carry the relay's traffic.** The rendezvous is
already a multiplexed reverse tunnel. `Server.Open` opens a stream to
any connected node, and the view-through (§7.2) is a request carried
over such a stream to the node, whose bytes are copied to the viewer
and never kept. A UI view-through is the same with HTTP inside: the
hub opens a stream of a new kind, `{"type":"http"}`, writes the
viewer's request, and copies the node's response back, WebSocket
upgrades included. The node answers it only for its own UI, and only
if it has turned the UI view-through on. That would give:

- **One outbound connection per node**, one credential (the node
  token) and one place to revoke it.
- **One sign-on and one access check.** IAP and the team directory
  decide who may open a node's UI (to begin with, its owner; §5.3's
  `SeesNode` for read-only views later), instead of the relay's own
  configuration.
- **No 60-minute cap.** The relay on Cloud Run ends every request at an
  hour, as §2.1 says of the hub; the hub on GKE does not.

What it costs: the hub carries every UI byte, so its bandwidth and the
one replica's capacity matter (§2.2); a hub outage takes the UIs with
it, where today the relay and the hub fail separately; and the node
side has to serve the UI over a mux stream, which is grain's change.
The no-code-at-rest rule (§1) holds, since nothing is stored, but more
code passes through the hub than the artifact view-through carries.

**Recommendation: fold the relay into the hub,** after Grain embeds the
rendezvous client (§9), and retire `grain-relay` for nodes that use a
hub, keeping it for nodes that do not. A follow-up proposal covers the
hub side (the `http` stream, its access check and route) and the
Grain side (answering it from the node's UI server); it is not built
here.
