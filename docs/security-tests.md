# The hub's security test suite

`hub/security` is the hub's security suite: automated tests of who may
do what at the hub (`docs/hub.md` §5), which runs in the hub's own
grain CI job (`.grain/workflows/hub.yml`, `go test -race ./...`) with
no external service. IAP is played by a key the suite signs assertions
with, nodes by fake nodes on the real rendezvous, and the database is
SQLite, or PostgreSQL where `internal/pgtest` can start one. CI always
can, because it sets `SILO_REQUIRE_POSTGRES`.

    go test ./hub/security            # everything, about a minute
    go test -short ./hub/security     # without the 30s slow-client test

It is modelled on bwsalmon/grain's `tests/security/routes_test.go`: a
table of every route, which fails when a route is added without an
entry.

## 1. The threat model it encodes

The hub is the multi-tenant Policy Decision Point in front of
single-tenant Grains. It holds the backlog, the team directory, node
tokens and the lifecycle events of every team's tasks. It must hold
none of their code. The suite assumes these attackers:

| Who | Holds | Must not |
|---|---|---|
| Anyone who reaches the load balancer | nothing, or a forged, expired or other-backend IAP assertion | be signed in as anyone |
| A signed-in outsider (`non-member`) | a valid IAP assertion, no team | see, change or learn of any team's tasks, nodes or settings; they may *propose* a task (§6.1) |
| Another team's admin or member | a valid assertion, admin of team B | see, filter for, subscribe to, view, cancel, approve, answer, message or learn of team A's tasks and nodes |
| A team's member | a member's role | do what is the team admin's (members, settings, releasing repositories) or the hub admin's (teams, assigning repositories) |
| A node token holder | a token for their own node, of either team, or a revoked one | use it as a human's sign-on; register another owner's node ID; post events or results for a task not placed on their node; keep a connection after revocation |
| A compromised or buggy node | a live connection | push SILO_ONLY content into the hub's store or logs; forge a human's message or approval |
| A browser on another site | the viewer's cookies | write through the API, or subscribe by WebSocket (identity.SameOrigin, the events handshake's Origin check) |
| A slow or malformed client | a socket | exhaust the hub, or keep a connection open past the hub's timeouts |

Status codes carry the model. 401: not signed in, as a caller of this
kind of route. 404: not yours, answered exactly as if it did not exist,
so that nobody learns another team's task, team or node exists. 403:
yours to see, not yours to do (a member managing the team, a submitter
approving their own proposal, a hub admin outside a team approving).

## 2. What each test asserts

**`routes_test.go`**: the authorization matrix.

- `TestRouteMatrixCoversEveryRoute` lists every route from the hub's
  own routers (`server.Hub.Routes`, which reads `hub/route.Mux`) on all
  four listeners: public, internal (the other pods), metrics (the
  scraper) and node UI (`<node>.<NodeUIDomain>`). It fails when the
  hub serves a route the matrix has no entry for, when an entry names
  a route the hub no longer serves, and when an entry lacks a caller.
- `TestRouteMatrix` asserts every cell. Each route is tried as each of
  the eleven callers: no identity; a forged IAP assertion (another key
  under the real key's ID); an expired one; one for another backend
  (wrong audience); a non-member; a member; the team's admin; another
  team's admin; and a node token of the team's, of another team's, and
  a revoked one. Routes that change what they act on (cancel, approve,
  reject, requeue) get a fresh task for each caller, so every cell is
  decided on its own. Responses let through are checked too:
  artifacts are `no-store`, `nosniff` and `CSP: sandbox`, event streams
  are `no-store`, and minted tokens are `no-store`.
- `TestOIDCRoutesAreOnlyTheSignIn`: a hub with OIDC sign-on adds only
  the sign-in's own routes, which `hub/identity`'s tests cover.

**`isolation_test.go`**: `TestCrossTeamIsolation`. Team A's admin and
member try everything against team B's running task, and team B's
admin and member against team A's: get, list and filter
(`?team=`, `?state=`), subscribe by SSE from cursors 0, 1 and past the
end, by `Last-Event-ID` and by WebSocket, fetch an artifact, list and
answer approvals, read and send messages, cancel, requeue, approve and
reject. They also try the other team itself, its settings, filing to
it by name, its node in `/v1/nodes`, and its node's UI host. Every
refusal must be a 404 whose body matches the one a nonexistent thing
gets. Afterwards the other team's task is still running and its node
was sent nothing.

**`nodetoken_test.go`**:

- `TestRevokedTokenDisconnects`: revoking a token, minting a
  replacement and releasing the node ID each end the live connection
  within 5s (§5.2 says "at once"; the slack is for CI), and the old
  token cannot reconnect.
- `TestNodeIDOwnership`: a node ID stays its first owner's. Another
  owner gets 409 minting for it, a token for one ID cannot register
  another, and the registry returns `registry.ErrOwner` to a second
  owner.
- `TestNodeCannotWriteForAnotherTask`: another team's node, another
  node of the same owner, and the task's own node on a stream bound to
  another task cannot post events or results for it. Nothing they sent
  is kept or shown.
- `TestCredentialsStayOnTheirSide`: a node token is refused on human
  routes however it is presented (bearer, the IAP header, a cookie),
  and a human's assertion is refused at the rendezvous.

**`atrest_test.go`**: `TestNoCodeAtRest`, on SQLite and PostgreSQL.
A fake node sends tool-call arguments and results, transcript tool
items, a diff artifact with its patch inline, an unknown `file` event
with contents, streamed assistant text, stored event types with
content smuggled into extra fields, a forged message, and a result
with node-disk paths. It also serves a diff through the view-through.
Each payload carries its own random sentinel. The test then reads
every row of every table, and every log line the hub wrote, and
requires that no sentinel appears. As a positive control, the dump
must contain the prompt and the diff's reference. The view-through
response must be `no-store`.

**`claims_test.go`**:

- `TestApprovedBy`: `approved_by` and the attestation's `approved_by`
  are empty for a member's own task. They are the approver for a
  proposal approved by a member of its own team, and are never set by
  another team's admin, the hub admin, the submitter (even after
  joining the team), an `approved_by`/`approved_at`/`submitter` field
  in the request, or `Store.Propose` given one. They do not carry over
  from the same person's earlier approved task (4224).
- `TestSubmitterCannotBeSpoofed`: the submitter is the verified IAP
  email, lower-cased. Identity headers a proxy or client might add are
  ignored, and an assertion without an email signs nobody in.

**`hardening_test.go`**:

- `TestOversizedPayloads`: 8 MiB bodies on every write API and a 2 MiB
  header block are refused with a 4xx, and the hub still answers.
- `TestMalformedRendezvousFrames`: a node, past the handshake, that
  sends non-frames, an over-long frame, an unmasked frame or random
  bytes is cut off. Event lines that are not events, or are over the
  1 MiB line bound, end only their own stream, and the node's good
  events still flow.
- `TestSlowClients`: 40 half-sent requests and 10 rendezvous
  connections that never say hello leave the hub serving people, nodes
  and subscriptions. All of them are closed within 45s (the header
  timeout and the hello timeout are both 30s).

## 3. Accepted exposures, stated

These are decisions, not gaps. The suite asserts them so that changing
one is a visible choice:

- **Proposals.** Anyone signed in may file a task to a repository a
  team they are not in owns. It is filed `proposed`, they see it as its
  submitter, and only a member of the owning team may approve it (the
  `POST /v1/tasks` row: 201 for every signed-in caller).
- **Approval requests are stored with their arguments.** An
  `approval_request` event is conversation class: a human has to see
  what they are approving. Its `args` therefore reach the store. The
  at-rest test does not send code there. A node must not put file
  contents in an approval's arguments (`docs/hub.md` §7).
- **The node directory** answers 200 to every signed-in human,
  filtered to the nodes of their teams (an outsider gets an empty
  list).
- **The probes, the claims keys and the web UI's page** answer anyone.
  None of them holds anything of anyone's.
- **The metrics** answer anyone who reaches their listener. The load
  balancer never does, and the matrix checks the public listener does
  not serve them.

## 4. Extending it

**A new route.** `TestRouteMatrixCoversEveryRoute` fails, naming it.
Add a `row` to `matrix()` in `routes_test.go` with an outcome for
every caller. Use `human(nonMember, member, teamAdmin, otherAdmin)`
for a human route (it fills in 401 for the rest), and give the row a
`task` func if the route changes the task it acts on. If the route is
team- or task-scoped, also add it to the list in
`TestCrossTeamIsolation`, with the path a nonexistent ID would take.
Register the route on a `route.Mux` (or wrap the handler with
`route.Wrap`) so the hub lists it. A handler mounted any other way
hides its sub-routes from the suite.

**A new caller kind** (a service account, a node acting for an agent):
add it to `callers` and give it a credential in `newWorld`. Every row
then needs its outcome, and the coverage test lists the missing cells.

**A new interface message** (task stream, event stream, http stream):
decide its class. Anything that may carry code, diffs, file contents
or tool arguments is node-only. Add its payload, with a sentinel, to
`TestNoCodeAtRest`, and add a malformed and an oversized instance to
`TestMalformedRendezvousFrames`. If a node can send it, add a case to
`TestNodeCannotWriteForAnotherTask` for a task not placed on that
node. For example:

- *Interactive sessions (4229, landed).* `POST
  /v1/tasks/{id}/messages` and `GET .../messages` are rows of the
  matrix and cases of the isolation test. The node's `message` event,
  whose claimed text and sender the hub drops, is in the at-rest test.
  A new message kind (attachments with content, a node-to-human
  question with a diff) needs a sentinel there.
- *The Grain UI proxy (4228; the hub's side, `hub/nodeui`, landed).*
  The node-UI host is one route on the `node-ui` listener, whose paths
  are the node's. If the proxy grows routes of the hub's own on that
  host (a WebSocket for the Grain UI, a path the hub answers itself),
  give each a row, add the other team's node host to the isolation
  test, and add a sentinel served by the fake node's UI handler to the
  at-rest test, since the proxy must pass bytes through as the
  view-through does.
- *Agent filing upward (4233, not landed).* When a node can file a
  task, add: its route to the matrix, with the node-token columns
  holding the outcomes; a rate-limit test in `nodetoken_test.go` that
  files past the limit and expects a refusal; and a case in
  `TestApprovedBy` that files from an approved task into another team
  and expects `approved_by` to be empty and the task `proposed`.

**A new listener**: add it to `server.Hub.Routes`, with a `Listener`
constant, and route the matrix's requests to it in `TestRouteMatrix`.
