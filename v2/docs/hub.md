# The Silo collection hub

Part of the [v2 design](design.md). This is Track 2: the architecture
of the **hub**. Its first parts are built: the node registry and the
rendezvous (stage 2.1, `v2/hub/registry` and `v2/hub/rendezvous`), and
identity and teams (stage 2.2, §5, `v2/hub/identity` and
`v2/hub/directory`). Task intake and dispatch (§6) and live events (§7)
are designed here and built in later stages (§9).

## 1. What the hub is

**Grain** is the execution node. A grain runs on one workstation or
Cloudtop, holds that machine's git checkouts and sandboxes, and runs
agent sessions there. With v2, each of those sessions is one `silo
run` ([grain.md](grain.md)). **The hub** links many grain nodes into
one multi-tenant system. It is the coordination plane for:

- **Identity:** who a human or a node is.
- **Teams:** which team owns which repositories.
- **Task federation:** one backlog, placed on whichever node can run it.
- **Routing:** reaching a node that is behind NAT or a corporate
  firewall.

The hub **holds no source code at rest, runs no sandboxes and makes no
git checkouts.** Anything that can contain code (diffs, file contents,
tool output) stays on the node that produced it. The hub passes it
through to a viewer on demand and never stores it (§7.2). If the hub's
database were lost or leaked, it would expose task metadata,
conversation text and the directory, but no repository content.

### 1.1 Where this sits in the grain/silo division

[grain.md](grain.md) §1 gives tasks, the queue and teams to grain, and
one run to silo. The hub does not change that for a single node. Each
grain still owns its own queue, runs, pull requests and CI. The hub
adds a layer above the nodes:

- Its backlog holds tasks that **have not been placed yet**. Once a
  task is placed, the node's grain owns it as one of its own tasks, and
  the hub only mirrors its lifecycle (§6, §7).
- Its team directory decides **which nodes may serve which
  repositories, and who may see their tasks**. A node still enforces
  its own grants inside a run.
- Many hubs could federate in the same way. This design assumes one.

## 2. Components

```
 humans, custom UIs, SDK                 grain nodes (workstations, Cloudtops)
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
more, as in v1. It holds teams, the backlog and the event metadata. The
**registry** is in memory. Everything in it is restated by each node
when the node connects, so it would only go stale in a database. A hub
that restarts relearns its nodes within one reconnect backoff.

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
has to be open inbound on a workstation. A WebSocket, rather than a raw
HTTP Upgrade or gRPC, is what Google's HTTPS load balancer and IAP pass
through unchanged.

The WebSocket carries binary frames, and those frames carry the mux
that v2 already uses to reach k8s pods (`internal/tunnel/mux`, from
grain's `pkg/tunnel`). The mux provides independent, flow-controlled
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
```

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

`ConnectPath` sits outside IAP's browser enforcement but still behind
the HTTPS load balancer. As an alternative, a node on GCP could present
a Google-signed ID token for its service account, and the hub would map
that token's email to an owner the team directory has registered. That
alternative is not built yet.

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
the dispatcher (§6.2) is to check `Admit` again before it offers a task.

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

## 6. Task intake and federation (stage 2.3)

### 6.1 The backlog

`POST /v1/tasks` creates a task with these fields:

- **target:** `{repo}` or `{team}`.
- **prompt** and **title**.
- **submitter:** the principal who submitted it.
- Optional **constraints**: node labels, or a node ID.

Users and custom UIs submit tasks the same way, through the API or the
SDK. The task states are:

```
queued → offered → running → done | failed | cancelled
              ↘ queued (refused, or the node was lost before it accepted)
```

The store keeps the task, its state, the node it was placed on and its
lifecycle metadata. It never stores a checkout, a diff or a log.

### 6.2 The dispatcher

The dispatcher wakes on a new task and on registry `Changes()`. It
takes queued tasks oldest first, per team:

1. `Claim(repo)`, honouring constraints. If no node has a free slot,
   the task waits.
2. `Server.Open(node)` opens a task stream, and the dispatcher writes
   `{"type":"offer","task":{...}}`.
3. The node's grain answers on the stream:
   - `accept` with its local task ID: the task is `running`, and the
     next heartbeat settles the claim.
   - `refuse` with a reason: the claim is released, and the task
     returns to the queue with that node excluded for this attempt.
4. If the node goes offline while the task is `offered`, the task is
   requeued. If it goes offline while the task is `running`, the task
   is marked `lost`. The node resumes it when it comes back (it still
   owns the task), or a human requeues it.

A team-targeted task goes to any node that serves one of the team's
repositories. That node's grain chooses the repository, as it does for
a task without one.

A task stream stays open for the task's life. Cancelling the task is a
`{"type":"cancel"}` written down the stream.

## 7. Live events and privacy (stage 2.4)

### 7.1 Streaming up

For each running task, the node opens one stream to the hub
(`NodeStream`) and writes the v2 event envelope ([events.md](events.md))
with the hub's task ID added. Grain already reads those events from
`silo run --json`. The hub sorts each event by class:

| Class | Examples | Stored by the hub | Broadcast live |
| --- | --- | --- | --- |
| Lifecycle | `status`, `result` (exit, reason), usage totals | yes | yes |
| Conversation | chat turns, `approval_request`, assistant final text | yes | yes |
| Token stream | `transcript` assistant text and thinking deltas | no: an in-memory ring per task, for late joiners | yes |
| Node-only (SILO_ONLY) | diffs, file contents, `tool_call` arguments and results, sandbox and tool logs | **never** | as a reference only |

The hub broadcasts to subscribers over
`GET /v1/tasks/{id}/events?from=<seq>`, as Server-Sent Events, or as a
WebSocket for the SDK. Subscribers get the same cursor semantics as
`silo attach`: they replay what the hub holds after `seq`, then follow.
Access is checked against the subscriber's teams at every subscription.

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
- **The hub can reach a node only through the streams the protocol
  defines:** offer, cancel and view. A node serves only those, so a
  compromised hub can ask a node to run a task but cannot read the
  node's files beyond the artifacts that node has itself listed.
- **No code at rest on the hub** (§7.2). The hub's store holds
  metadata and conversation text only.

## 9. Stages

| Stage | What | Where |
| --- | --- | --- |
| 2.1 | This design. The registry (`hub/registry`). The rendezvous: WebSocket, auth hook, admission hook, heartbeats, supersession, streams both ways, client with reconnect (`hub/rendezvous`). | here, built |
| 2.2 | Human sign-on (IAP JWT verification and OIDC, `hub/identity`). Node tokens, the team directory, `Admit` and per-team visibility, in a store (`hub/directory`). | here, built |
| 2.3 | The backlog, the intake API, the dispatcher and the task-stream protocol | silo |
| 2.4 | Event streaming up, the classification, fan-out to SSE and WebSocket, view-through | silo |
| 2.5 | A `silo hub serve` command and its deployment (Cloud Run or GKE behind IAP) | silo |
| — | Grain embedding `rendezvous.Client`, accepting offers, streaming events | bwsalmon/grain |

## 10. Open questions

- **Lost running tasks.** Should the hub requeue a `lost` running task
  automatically after a grace period? A second node would then redo
  work that the first may still be pushing.
- **Several hubs.** Run one hub replica, or several? Several replicas
  need the registry to say which replica holds each node's connection,
  so that `Server.Open` can be routed. The leader or lease machinery in
  v1's store is the obvious basis.
