# Threat model: the Grain + Silo system

This is the threat model the security audits of the Grain + Silo system
audit against. It covers the Silo hub (this repository), the Grains
that connect to it (bwsalmon/grain, or a customised Grain speaking
[hub-interface.md](hub-interface.md)), and the flows between them,
their users, their forges and their model providers. It is a document
of record: an audit cites its IDs (`A-`, `ACT-`, `TB-`, `F-`, `T-`,
`INV-`), a security test cites the `INV-` it checks, and a change that
moves a boundary changes this file in the same pull request.

Status: written against silo `main` at a1e4434 and bwsalmon/grain
`main` at 7f7329269 (2026-10-16). Built and planned parts are marked
as such throughout; a planned surface is modelled now so that its
design is reviewed against the model, not after it.

Contents:

1. [The system and its security posture](#1-the-system-and-its-security-posture)
2. [Assets](#2-assets)
3. [Actors and trust levels](#3-actors-and-trust-levels)
4. [Trust boundaries and data flows](#4-trust-boundaries-and-data-flows)
5. [Threats per boundary](#5-threats-per-boundary)
6. [Security invariants](#6-security-invariants)
7. [Open findings](#7-open-findings)
8. [The audit plan](#8-the-audit-plan)

## 1. The system and its security posture

- **Silo** is the multi-tenant hub, on GKE behind IAP ([hub.md](hub.md)
  §2.1, [deploy.md](deploy.md)). It is the **Policy Decision Point
  (PDP)**: it authenticates humans (`hub/identity`) and nodes (node
  tokens, `hub/directory`), owns the team directory and which team
  owns which repository (`hub/directory`), holds the backlog of tasks
  not yet placed and dispatches them (`hub/tasks`), is the rendezvous
  Grains dial out to (`hub/rendezvous`, a mux over one WebSocket), and
  fans out live events and passes artifacts and a node's own UI
  through to viewers (`hub/live`, `hub/nodeui`). It **stores no code at
  rest** (hub.md §1, §7.2).
- A **Grain** is single-tenant, with one backlog, and is the **Policy
  Enforcement Point (PEP)**. It holds every task to a local policy only
  its owner edits (grain `pkg/policy`, grain `docs/local-policy.md`):
  the repositories, capabilities and network a task may have. It runs
  agents in sandboxes (container, granule VM, Kubernetes pod, or a host
  directory with opt-in ambient credentials: `-host-sandboxes`,
  `-host-sandbox-env`, grain tasks 4159 and 4192), and has its own git
  proxy, CI and merge queue. A Grain may be **customised**, or be
  different code altogether, as long as it speaks
  [hub-interface.md](hub-interface.md).
- **The accepted risk (owner decision).** A Grain takes the hub's
  identity claims (`submitter`, `team`, and later `approved_by`) on
  trust. A compromised hub can forge who asked for, or approved, a
  task. It must not be able to get a task past a Grain's local policy.
  The hub signs its claims for each node when it has a key (hub.md
  §6.4, `hub/claims`), and grain verifies them when its policy names
  `silo_issuer` (grain `pkg/policy/silo.go`, `ApplySiloClaims`). That
  stops anything between the hub and a Grain from altering them; it
  does not stop a compromised hub, which holds the signing key.
- **What follows from it.** The hub's compromise is bounded by the sum
  of the Grains' local policies, so each Grain owner's policy is a
  security control, not a preference; and every hub control that
  decides *who* is only as strong as the hub itself. Section 5 is
  written with both in mind.

**Built vs planned.** Built in silo: the registry, rendezvous,
identity, directory, backlog and dispatcher, live events and the
view-through, `silo hub serve` and its GKE deployment (not applied to
any project), the node UI's `http` stream and per-node hosts (task
4228's hub side, `hub/nodeui`, hub.md §11), the dispatcher lease
(R1), and the hub UI's read-only stage U1 (`hub/ui`, `ui/`, ui.md §7). Built in grain: the node client (`pkg/hubnode`), offers, resumes
and cancels, and local policy. **Planned, not built:** the rest of
the hub UI ([ui.md](ui.md) U2–U5, tasks 4223–4227); grain's end of the node UI stream
(hub.md §11.3); interactive sessions (tasks 4229, 4230: `message` and
`approve` from hub to Grain); upward task filing and cross-Grain
dependencies (tasks 4233, 4235); the conformance kit (task 4236);
several hub replicas (R2–R5, hub.md §2.2); and the security test
suites (tasks 4239, 4242).

## 2. Assets

| ID | Asset | Where it lives | What its loss costs |
| --- | --- | --- | --- |
| A-1 | **Source code and diffs**, file contents, tool output | On the Grain that produced them (checkouts, sandboxes, transcripts). Passes *through* the hub on a view (hub.md §7.2) or a node UI request (hub.md §11), never at rest there. | Confidentiality of private repositories; secrets that happen to be in code. |
| A-2 | **Corp and ambient credentials on Grains**: the owner's git credentials behind the Grain's git proxy, `-host-sandbox-env` variables (`SSH_AUTH_SOCK`, `CLOUDSDK_CONFIG`, proxies), capability credentials (`gcp-key`, `repo-write:*`), the Grain's own state | The Grain's host and its sandboxes. Never sent to the hub. | Acting as the Grain's owner anywhere those credentials reach: pushing code, cloud access, corp systems. The highest-impact asset in the system. |
| A-3 | **Node tokens** (`silo-node_<id>_<secret>`) | On the Grain (`HubTokenPath`); the hub keeps only SHA-256 (`hub/directory/tokens.go`, `hashToken`). | Registering as that node: receiving its owner's team's offered tasks (prompts), forging its events and results. |
| A-4 | **Human sessions**: IAP's cookie and signed assertion (`x-goog-iap-jwt-assertion`), the hub's OIDC session cookie, OIDC ID tokens used as bearers | Browsers, CLIs/SDKs; the assertion passes the hub and, for the node UI, reaches the node. | Acting as that human at the hub: filing, cancelling, administering teams, minting node tokens, reading their teams' tasks. |
| A-5 | **The team directory**: teams, members and roles, repository ownership, node tokens' hashes, team settings, hub admins (configuration) | The hub's database (`hub/directory/store.go`). | Its integrity *is* the hub's authorization: a forged membership or repository ownership admits nodes and viewers it should not. |
| A-6 | **Task metadata and conversation**: prompts, titles, submitters, states, refusal and result reasons, lifecycle and conversation events | The hub's database (`hub/tasks`, `hub/live/store.go`); the token stream in memory (`hub/live/hub.go`, the ring); the Grain's own store. | Prompts and conversation may hold sensitive intent, customer data, or code a careless node or agent put in text. Cross-team disclosure. |
| A-7 | **Model credentials** (API keys, OAuth for agent frameworks) | On the Grain (or its model proxy). Never on the hub. | Spend, and access to whatever the provider account holds. |
| A-8 | **Merge rights on repositories**: the Grain's GitHub identity, its merge queue, approvers on held changes | The Grain and the forge. | Landing code on a protected branch: the supply chain of everything built from it. |
| A-9 | **The hub's own secrets**: the OIDC session key, the IAP OAuth client secret, the database credentials, the internal HMAC key (R3, planned) | Kubernetes Secrets, Cloud SQL, workload identity (`deploy/`). | Forging sessions (OIDC key), reading or rewriting A-5 and A-6. |
| A-10 | **Availability** of the hub (dispatch, events) and of each Grain's slots | — | Work stops; a team's backlog is starved. |

## 3. Actors and trust levels

Trust levels, from most to least trusted: **T0** trusted for
everything it holds; **T1** trusted within its own scope; **T2**
authenticated but not trusted; **T3** unauthenticated.

| ID | Actor | Trust | What it legitimately can do | What it must not be able to do |
| --- | --- | --- | --- | --- |
| ACT-1 | **Team member** (signed in, in `team_members`) | T1 for their teams | File, list, watch tasks of their teams; cancel/requeue their own; mint node tokens for their own nodes; view their teams' artifacts; open their own node's UI | See another team's tasks, events, artifacts or nodes' details; act as another person; get a task past a Grain's policy |
| ACT-2 | **Non-member** (signed in to IAP/OIDC, in no team) | T2 | `/v1/me`; mint node tokens (useless without admission) | Anything team-scoped: every team resource answers 404 |
| ACT-3 | **Team admin** | T1 for their team | Manage members of their team, team settings, release its repositories, cancel/requeue any of its tasks | Assign any repository to a team, so take another team's or an unowned one (SA1-M2: hub admin only), create or delete teams, see other teams |
| ACT-4 | **Hub admin** (`API.HubAdmins`, configuration) | T0 for the directory | Create/delete teams, assign repositories (with or without their history), see everything, revoke any node token | Open a node's UI (`hub/nodeui`: owners only); bypass a Grain's policy |
| ACT-5 | **Another team** (its members, admins and nodes) | T2 relative to this team | Its own work | Read this team's tasks, or have its nodes offered this team's tasks |
| ACT-6 | **External contributor**: a person outside the company whose content reaches the system (a pull request, an issue, a comment, a dependency) | T3 | Nothing directly: they have no IAP identity | Have their content treated as instructions with a member's authority (prompt injection, ACT-9b) |
| ACT-7 | **Stock Grain** (bwsalmon/grain, unmodified, owner's policy) | T1 for its owner's work | Register for repositories its owner's teams own; accept, run, report its tasks; stream events | Register as another node; touch other tasks; be driven outside the task interface |
| ACT-8 | **Customised or malicious Grain**: a Grain running arbitrary code that speaks the protocol, or a stock Grain whose host is compromised | T2: authenticated as its owner, nothing it says is trusted | Whatever ACT-7 can, plus lie: about slots, results, events, artifacts | Everything ACT-7 must not, and: attack hub users through content (§5 TB4), exhaust the hub (DoS), escape the classification of events |
| ACT-9 | **Agent** (the model-driven process in a Grain's sandbox), including **ACT-9b, an agent following prompt injection** from repository content, task text, tool output, fetched pages, or another Grain's output | T3 for intent; capabilities are what its sandbox grants | Use the tools and credentials its sandbox was given for the task | Use credentials beyond the task's grant; file or approve work with a human's authority; reach the host |
| ACT-10 | **CI runner** (grain CI's sandboxes for silo's and grain's workflows) | T1 for the capabilities its workflow declares | Build, test; read repositories its workflow declares (`repo-read:*`) | Write anywhere a workflow did not declare; leak declared credentials |
| ACT-11 | **Compromised hub** (its code, image, pod or database) | Assumed hostile in the Grain's model | Forge claims; offer, cancel, view and open UI streams to any connected node; read A-4 assertions and A-6 | Get a task past a Grain's local policy; read a node's files beyond what the node lists or its UI shows; obtain A-2 |
| ACT-12 | **GCP/GKE operator** (project owners, cluster admins, Cloud SQL admins) | T0 for the hub | Run, upgrade and restore the hub | — (equivalent to ACT-11; the model does not defend the hub against its operator) |

## 4. Trust boundaries and data flows

```
                 ACT-1..4 humans                           ACT-6 external contributor
            browser / CLI / SDK                              (PRs, issues, deps)
                   |                                                |
           F-1 HTTPS, IAP cookie                                    | content
                   v                                                v
   ===== TB1 ==== Google LB + IAP ====                       +-------------+
                   |  F-2 x-goog-iap-jwt-assertion            |   forge     |
                   v      (or OIDC session / bearer ID token) |  (GitHub)   |
   +----------------------------- Silo hub (GKE) ------+      +-------------+
   | identity  directory  backlog/dispatcher  live     |            ^
   | registry  rendezvous  nodeui                       |            | F-8 git over the
   |   |                                                |            |   Grain's git proxy,
   |   +-- TB5 --> Cloud SQL (A-5, A-6), Secrets, logs  |            |   Grain's GitHub identity
   +----------------------------------------------------+            |
        ^   | F-4 offer/resume/cancel (task stream)                  |
        |   | F-5 view (artifact), F-6 http (node UI)                |
        |   | F-7 planned: approve, message, upward filing           |
   F-3  |   v                                                        |
   ====|== TB2/TB3: wss /v1/nodes/connect, IAP off, node token ======|===
        |   |                                                        |
   +--------------------------- Grain (one owner) ------------------+------+
   |  hubnode client  -->  local policy (PEP)  -->  orchestrator            |
   |  git proxy, CI, merge queue, model proxy                               |
   |   |  ==== TB7: tools / MCP / run_command ====                          |
   |   v                                                                    |
   |  sandbox (container | granule VM | k8s pod | host dir + ambient env)   |
   |     ACT-9 agent                                                        |
   +-------------------------------|----------------------------------------+
                                   | F-10 prompts, code context
                                   v           (TB8)
                           model provider (A-7)

   TB4: hub -> browser: what the hub serves back (events, artifacts, node UI)
   TB9: CI and the supply chain: workflows, images, modules, grain main in silo CI
```

| Flow | From → to | What crosses | Authenticated by | Authorised by (code) |
| --- | --- | --- | --- | --- |
| F-1 | Browser → Google LB/IAP | Every human request | IAP sign-in (Google identity) | IAP's access list (`deploy/terraform/hub/main.tf`, `google_iap_web_backend_service_iam_member`) |
| F-2 | IAP → hub | The request with IAP's signed assertion | `identity.IAP.Principal` (`hub/identity/iap.go`): ES256 signature against IAP's keys, issuer, audience = the backend service, expiry. Or `identity.OIDC` (session cookie HMAC, or bearer ID token). Never `x-goog-authenticated-user-email`. | `directory.Store.Access` (`hub/directory/access.go`): `SeesTeam`, `SeesTask`, `SeesRepo`, `SeesNode`, `AdminOf`, hub admin from configuration |
| F-3 | Grain → hub, `GET /v1/nodes/connect` (IAP off, exact path, hub.md §5.2.1) | WebSocket + mux: hello (id, repos, slots, labels), heartbeats, node-opened event streams (§7.1) | Node token as bearer, checked before upgrade (`rendezvous.Server.serveConnect` → `directory.Store.Authenticate`); token bound to one node ID (`serveNode`) | `directory.Store.Admit` at hello; registry ownership of node IDs (`registry.Register`, `ErrOwner`); event stream bound to a task placed on that node (`live.Hub.NodeStream`) |
| F-4 | Hub → Grain, task stream | `offer`/`resume` with `tasks.Spec` (id, team, repo, title, prompt, submitter, constraints); `cancel`; Grain answers `accept`/`refuse`/`result` | The hub is the far end of the Grain's own TLS connection to the hub's URL; no per-message signature | The hub: dispatcher + `Admit` rechecked (`tasks.Dispatcher.place`, `admit`). The Grain: its local policy (`pkg/policy`), and grain's own team/repo rules (`pkg/daemon/hub.go`, `hubTasks.Start`) |
| F-5 | Hub → Grain, view stream | `{"type":"view","task","node_task","ref"}`, then artifact bytes back | As F-4 | The hub: viewer sees the task (`live.Hub.task`), and asks only for a ref the node listed (`Log.HasRef`, SA1-M3). The Grain: serves only refs it listed (hub-interface.md); grain does not serve views yet (`hubnode.Serve` closes them) |
| F-6 | Hub → Grain, http stream (node UI) | `{"type":"http","node","viewer"}`, then one HTTP request with the browser's IAP assertion and the node's own cookies | As F-4; the node may verify the IAP assertion itself (hub.md §11.3) | The hub: viewer is the node's owner (`nodeui.Server.ServeHTTP`). The Grain: the owner turned it on, and its own sign-in (grain side not built) |
| F-7 | Hub → Grain (planned) | `approve`, `message` (tasks 4229, 4230, ui.md U3); Grain → hub task filing and dependencies (4233, 4235) | As F-4 / F-3 | Not designed yet: see T-TB3-5, T-TB1-7 |
| F-8 | Grain → forge | Clones, pushes, PRs, merges | The Grain's GitHub identity, through its git proxy, scoped per sandbox (grain `pkg/gitproxy`) | The Grain's policy `repos`, the task's `reads`/`writes`, branch protection on the forge |
| F-9 | Agent → Grain tools | MCP tool calls, `run_command`, git through the proxy | Per-sandbox bearer token (grain design.md, "Sandbox identity") | The task's capabilities and network (local policy); the sandbox backend's isolation |
| F-10 | Grain → model provider | Prompts with repository content and tool output | The Grain's model credential (A-7) | The Grain owner's choice of provider; not visible to the hub |
| F-11 | Hub → browser | JSON API answers, SSE/WebSocket events, artifact bytes, node UI responses | — (the hub's origin, or a node's own host) | INV-13, INV-14, INV-15 |
| F-12 | Hub → Cloud SQL, Secrets, logs | A-5, A-6, A-9; log lines | Workload identity, Cloud SQL proxy (`deploy/kubernetes/hub/base/deployment.yaml`) | GCP IAM (ACT-12) |

The trust boundaries:

- **TB1**: browser, CLI or SDK → IAP → hub (F-1, F-2).
- **TB2**: Grain → hub over the rendezvous (F-3).
- **TB3**: hub → Grain over the rendezvous (F-4 to F-7).
- **TB4**: hub → browser: what the hub serves back, including content a
  Grain or agent wrote (F-11).
- **TB5**: hub → its own platform: database, secrets, logs, operator
  (F-12).
- **TB6**: Grain → forge (F-8).
- **TB7**: agent → Grain tools and the sandbox boundary (F-9).
- **TB8**: Grain → model provider (F-10).
- **TB9**: CI and the supply chain.

## 5. Threats per boundary

Each threat has an ID `T-<boundary>-<n>`, its STRIDE class (**S**poofing,
**T**ampering, **R**epudiation, **I**nformation disclosure, **D**enial of
service, **E**levation of privilege), the existing mitigation with the
code that implements it, the residual risk, and a status:

- **mitigated**: the residual is small and nothing is owed;
- **accepted**: a known residual the owner has decided to carry;
- **open**: owed work, listed again in §7 as an `O-` finding.

### TB1: browser / CLI / SDK → IAP → hub

**T-TB1-1 (S) Forged human identity at the hub.** *Mitigation:* behind
IAP, every request is authenticated by the signed assertion, not a
header a client could set: `identity.IAP.Principal`
(`hub/identity/iap.go`) and `identity.Verify` (`hub/identity/jwt.go`)
check ES256 against IAP's keys, issuer, audience = the hub's backend
service, and expiry; algorithm must match the key type
(`TestAlgorithmMustMatchKey`), so `none` and HMAC-with-public-key are
refused. Without IAP, OIDC with PKCE, state and nonce, verified ID
token, `email_verified` required, HMAC-signed session cookie
(`hub/identity/oidc.go`). Exactly one sign-on must be configured
(`server.Config.auth`). A request that reaches a pod without the load
balancer still needs a valid assertion. A group claim never stands in for a user,
nor an email for a group: directory principals and `HubAdmins` are
`user:<email>` or `group:<name>`, each matched only against its own
claim (SA1-L6, O-14 closed). *Residual:* the development
sign-on `-dev-user-header` trusts a plain header; nothing in `deploy/`
sets it and nothing checks that it stays so (O-12). *Status:* mitigated;
the dev-header guard is open (O-12). INV-1, INV-2, INV-20.

**T-TB1-2 (I) Cross-team data leak through the API.** A member reads
another team's tasks, events, conversation, artifacts or nodes.
*Mitigation:* every read is filtered by `directory.Access`:
`tasks.API.list/get` (`hub/tasks/api.go`, `sees`), `live.Hub.task`
(`hub/live/http.go`) for events and artifacts, `directory.API.Visible`
for the node directory, team routes by membership. A resource the
caller may not see answers 404 like one that does not exist, so IDs
cannot be probed (`TestAPIVisibility`, `live` `TestAccess`, `directory`
`TestAPI`, `TestAccess`). Access is checked at every subscription.
A task is shown to the team recorded on it, and to the team that owns
its repository now only if a hub admin gave that team the repository's
history (`Access.SeesTask`, `Store.TransferRepo`); only a hub admin
assigns a repository at all, so no team admin can claim an unowned one
and read what was filed in it (SA1-M2, `hub/tasks`
`TestRepoSquatting`, `hub/directory` `TestSeesTask`).
*Residual:* (a) a hub admin who transfers a repository with
`{"history": true}` shows its whole history to the new team, while the
old team keeps seeing tasks recorded under its team; a team recreated
with a deleted team's ID sees that team's tasks; (b) a node's directory entry
lists all its repositories, so a member of team A sees the names of
team B's repositories served by a node whose owner is in both
(`Access.SeesNode`); (c) a subscription already open when a member is
removed keeps streaming until it ends. *Status:* (a) and (c) accepted
as documented behaviour, to be re-decided with the hub UI; (b) open,
low (O-9). INV-9.

**T-TB1-3 (S/E) Submitting as somebody else.** *Mitigation:* the
submitter is the signed-in caller, never the body: `tasks.API.submit`
sets `Submitter` from `acc.Email`, and the strict decoder refuses a
`submitter` field (`TestAPISubmit`). A repository with no owning team is
refused. Cancel and requeue need the submitter, a team admin or a hub
admin (`manages`). *Status:* mitigated. INV-10.

**T-TB1-4 (S/E) Cross-site request forgery.** A page on another site
makes a signed-in member's browser `POST /v1/tasks` (filing an
attacker's prompt under the member's name, which the Grain then runs
with the member as requester), cancel a task, mint a node token (which
revokes the member's existing one, `MintNodeToken`), or, for a hub
admin, create a team. *Mitigation:* the WebSocket subscription checks
`Origin` (`live.Hub.serveWebSocket`); the OIDC cookie is
`SameSite=Lax`, which keeps it off cross-site POSTs. `SameSite` is not
enough on its own: behind IAP the browser's credential is IAP's own
cookie, whose `SameSite` attribute the hub does not control, and a
node's UI host (`<node>.nodes.<domain>`, hub.md §11.2) is the hub's
*site*, so a Grain's own UI page gets the hub's cookies on a
no-preflight POST, marked `Sec-Fetch-Site: same-site` (audit SA1-M1).
So every `POST`, `PUT` and `DELETE` of the JSON APIs
(`tasks.API.Handler`, `directory.API.Handler`) and the OIDC sign-out is
refused unless `Sec-Fetch-Site` is absent (not a browser) or exactly
`same-origin`, an `Origin`, if sent, is the hub's own scheme and host,
and a body is `Content-Type: application/json`, which no form and no
fetch without a CORS preflight can send (`identity.SameOrigin`;
`hub/identity` `TestSameOrigin`, `TestOIDCSignIn`; `hub/tasks` and
`hub/directory` `TestAPISameOrigin`). The hub answers no CORS
preflight. *Residual:* a browser that sends neither `Origin` nor
`Sec-Fetch-Site` on a POST (none current) is held only by the content
type; deploy.md §8 recommends a node UI domain on another registrable
domain, which keeps the cookies off altogether. *Status:* mitigated
(O-1, SA1-M1 closed). INV-16.

**T-TB1-5 (S) Session or token theft and replay (humans).**
*Mitigation:* IAP assertions are short-lived and audience-bound; OIDC
sessions are HMAC-signed with a ≥32-byte key, `HttpOnly`, `Secure` on
https, 12 hours; `localReturn` refuses open redirects
(`TestLocalReturn`); node tokens are shown once and served
`Cache-Control: no-store` (`directory.API.mintToken`). *Residual:* the
OIDC session cannot be revoked before it expires (no server-side
session table); IAP sessions are governed by IAP. The node UI hands the
viewer's IAP assertion to the node (T-TB3-4). *Status:* accepted (12h
OIDC sessions) and open for the node UI (O-2).

**T-TB1-6 (E) A team admin escalating.** *Mitigation:* assigning a
repository to a team is hub-admin only (SA1-M2), and `AssignRepo`
refuses a repository another team owns (it must be released first);
hub-admin is configuration, not a row a team admin can write
(`API.HubAdmins`); team creation and deletion are hub-admin only.
*Residual:* a team admin can add anyone to their team, including a
principal who runs a node; that node is then admitted for the team's
repositories. That is the role's purpose. *Status:* mitigated.

**T-TB1-7 (E) Approval laundering.** A task reaches a Grain as if a
human approved it, when nobody independent did. Routes: CSRF (T-TB1-4);
an agent filing a task upward (task 4233) that comes back down to a
Grain as an ordinary hub task; an agent answering an
`approval_request` through a planned `approve` path; a cross-Grain
dependency (task 4235) feeding one task's output into another's
prompt. *Mitigation today:* there is no upward filing and no approval
path. The hub carries `approved_by` on the offer (`tasks.Spec`) and
signs it as the attestation's own claim, never equal to the submitter;
a submission cannot set it, so it is empty on every hub task today.
grain files a hub offer that names no approver other than its
submitter as a proposal a local human has to approve (grain
`pkg/daemon/hub.go`, `hubOffer.ApprovedBy`; grain task 4310, GSA1-11),
so a hub submission is no longer an approval at the Grain. Before
that, grain filed every hub offer as **already approved** with the
submitter as its principal, and grain's own approval rule ("approved
by somebody other than the requester", `docs/local-policy.md`) was
satisfied by the hub's say-so. *Residual:* the hub has no approval
flow and no `proposed` state; once it accepts tasks from nodes or
agents, what records `approved_by` decides whether this is closed.
*Status:* **open, must be closed before tasks 4233 and 4235 ship**
(O-4): an agent- or node-filed task enters the hub as `proposed` with
a non-human requester, and only a signed-in human other than the
requester may approve it (ui.md U3), recorded as `approved_by`.
INV-17.

**T-TB1-8 (D) A user exhausting the hub.** *Mitigation:* bodies are
bounded (`readJSON`: 64 KiB for the directory, the prompt limit plus
64 KiB for tasks; prompts ≤ 256 KiB, titles ≤ 200, `hub/tasks/task.go`);
dispatch is fair across teams (`fair`, `TestFair`), so one team's queue
does not hold back another's first task; a slow subscriber is dropped
after 512 events behind (`TestSlowSubscriber`). *Residual:* no rate
limit or quota on filing, subscriptions, or artifact views per user;
the database grows with every task. *Status:* open, medium (O-5).

**T-TB1-9 (R) Denying an action.** *Mitigation:* tasks record their
submitter and every state change with its reason; the dispatcher logs
offers, refusals and results (`tasks.Dispatcher`). *Residual:* there is
no audit log of directory changes (who added a member, minted or
revoked a token) beyond the token's own row and request logs at the
load balancer. Deleting a team and assigning or releasing a repository
are logged as `directory: audit:` lines with the caller (SA1-M2), in
the hub's ordinary log, which nothing protects from the hub itself.
*Status:* open, medium (O-10).

### TB2: Grain → hub (the rendezvous)

**T-TB2-1 (S) A Grain impersonating another Grain.** *Mitigation:* the
handshake is authenticated before the upgrade (`serveConnect`, 401
otherwise, `TestUnauthenticated`); the owner is the credential's, never
the hello's; a node token is bound to one node ID and a hello naming
any other is refused (`serveNode`, `TestNodeBoundCredential`); a node
ID belongs to its first owner (`registry.Register`, `ErrOwner`,
`TestOwner`) and, across restarts and revocation, to the first owner
to mint a token for it (`MintNodeToken` refuses a node ID another owner
has any unreleased token for, live or revoked, until that owner or a
hub admin releases it: `ReleaseNodeID`, `TestNodeIDReusableAfterRevoke`,
SA1-H1). A task records its node's owner as well as its ID, and is
resumed, told of a cancel, streamed into and asked for artifacts only
through a node that owner registers (`Dispatcher.bound`,
`live.Hub.holds`, `TestResumeOnlyToOwner`, `TestCancelOnlyToOwner`,
`TestAnotherOwnersNode`). A same-owner
reconnect supersedes the old connection (`TestSupersede`,
`TestReconnectSupersedes`). *Residual:* a stolen node token is a full
impersonation of that node until revoked (T-TB2-5); the same owner can
run two processes under one ID, each superseding the other. *Status:*
mitigated. INV-5, INV-6.

**T-TB2-2 (E) A Grain admitted for repositories its owner's teams do
not own.** *Mitigation:* `directory.Store.Admit` refuses the whole node
if any repository it lists has no owning team or a team its owner is
not in (`TestAdmit`); the dispatcher rechecks `Admit` before each offer
and each resume (`Dispatcher.admit`, `TestAdmissionCheckedAgain`,
`TestResumeChecksAdmission`), so an owner who left the team is offered
nothing and resumes nothing: their lost task is requeued (SA1-H1). Admission is by email only, not by groups
an IdP asserted at sign-in. *Status:* mitigated. INV-7.

**T-TB2-3 (T/S) A Grain injecting events into another task.**
*Mitigation:* the first line of an event stream binds it to a task,
which must be placed on that node, registered by the owner it was
offered to (`live.Hub.NodeStream`: `task.Node != nodeID` refuses, and
`holds` refuses another owner's node on the ID, SA1-H1); a later line naming another task closes the stream
(`TestStreamForAnotherNode`); events at or below the task's last `seq`
are dropped. A task requeued away from a node no longer names it, so
the old node's events are refused (hub.md §6.3). *Residual:* a node may
write anything into the tasks placed on it, including false lifecycle
events and a false `result` (T-TB2-6). *Status:* mitigated. INV-8.

**T-TB2-4 (I/T) A Grain smuggling code or secrets into the hub's store,
or abusing the classification.** *Mitigation:* the hub classifies by
schema (`live.Classify`, `hub/live/classify.go`): only lifecycle and
conversation are stored (`Class.Stored`); unknown types and schema
versions are node-only and reduced to `{kind, ref, size}` (`reference`);
any field outside a stored type's schema rejects the event; transcript
items carrying a tool's `id`, `tool` or `args` are rejected; a result's
disk paths are dropped (`TestClassify`, `TestClassifyDropsNodePaths`,
`TestNodeOnlyNeverStored`). The store itself refuses any class but
lifecycle and conversation. *Residual:* the schema bounds the *shape*,
not the *content*: a conversation text, an `approval_request`, a refusal
or result `reason` (stored in the task row, `Store.Requeue`/`Finish`) can
carry pasted code or a secret if the node or its agent puts it there.
Credential redaction runs on the node (hub.md §7.2), which a customised
Grain may skip. *Status:* accepted: the no-code-at-rest guarantee is
by class, and a node's text is that node's responsibility; INV-3 is
written to be testable on that basis. A refusal or result reason is
cut to 1024 characters before it is stored (O-7, closed by SA1-L3).

**T-TB2-5 (S/I) Node token theft and replay.** *Mitigation:* 256 bits
of secret; only its SHA-256 is stored, so a database leak proves nobody
(`hashToken`); bound to one node ID; minting again revokes the earlier
token; revoking, or minting again, ends the live connection at once
(`Server.Disconnect`, `directory.API.revokeToken` and `mintToken`,
SA1-L2) and refuses its reconnect (`TestNodeTokens`, `TestAPI`).
Revoking does not free the node ID for another owner: it stays its
first owner's until released (SA1-H1), so a revoked owner's lost tasks
are not handed to whoever mints the ID next.
*Residual:* the token is a bearer secret in a file on the Grain's host,
readable by anything running as the Grain's user, including a host
sandbox's agent (T-TB7-2). It has no expiry. A thief gets the owner's
team's task prompts and can forge that node's events and results; it
does not get A-2. *Status:* accepted for now; token expiry and
rotation are open, low (O-11). INV-6.

**T-TB2-6 (T/R) A Grain lying about its work.** It reports `done` for
work it did not do, accepts tasks it never runs (a black hole), streams
invented conversation, or reports false capacity. *Mitigation:* nothing
can verify a Grain's account of its own work; the forge is the source of
truth for what landed (PRs, merge commits with `Requested-by:` and
`Approved-by:` trailers, grain `docs/local-policy.md`). A refusing node
is excluded from that task (`TestRefuseRequeuesElsewhere`). *Residual:*
a malicious Grain admitted for a team can attract the team's work by
reporting many free slots (`registry.Validate` caps neither slots nor
repositories; `Claim` prefers the most available) and swallow it.
*Status:* accepted for integrity (a Grain is its owner's agent); the
uncapped slots are open (O-5).

**T-TB2-7 (D) A node exhausting the hub.** *Mitigation:* each line is
bounded (1 MiB control, task and event lines; 64 KiB node UI answers);
the hello must come within 30s; the token ring is bounded (1024 events
or 1 MiB per task, `TestRingBound`); a node is admitted only with a
valid token. The mux (`internal/tunnel/mux`) bounds what one session
can hold, whatever the peer does (SA1-H2): a stream buffers at most its
1 MiB window, also for a peer that does not announce flow control; a
session buffers at most 16 MiB unread across its streams and holds at
most 1024 open streams, a stream past either cap being reset; pings are
answered by one control writer that owes at most one pong (later pings
replace it) and resets go through a bounded queue whose overflow closes
the session; only a pong echoing one of our outstanding pings counts as
alive; every write has the ping timeout (10s) as its deadline, so a
node that stops reading loses its session (`abuse_test.go`
`TestPingFloodFromNonReadingPeerIsBounded`,
`TestNonReadingPeerHitsWriteDeadline`,
`TestUnsolicitedPongsDoNotKeepSessionAlive`,
`TestLegacyPeerBuffersNoMoreThanWindow`, `TestSessionCapsBufferedBytes`,
`TestSessionCapsStreams`, `TestControlQueueOverflowClosesSession`).
*Residual:* each of a node's up to 1024 streams is a goroutine
(`rendezvous.Server.acceptNodeStreams`), and an event stream that never
writes its first line is held for the connection's life
(`NodeStream` has no first-line deadline); the number of nodes, and so
of sessions each with its own caps, is not bounded per user; the event
store has no per-task or per-node quota; one connection's bandwidth
through the pod holding it is unbounded. *Status:* open, medium (O-5).
INV-19.

### TB3: hub → Grain (offers, cancels, views, the node UI, planned messages)

**T-TB3-1 (S/E) A compromised hub forging claims.** It offers a task
as if a member requested it. *Mitigation:* the Grain's local policy
bounds what any task may do, whoever it claims to come from: the
repositories, capabilities and network (`pkg/policy`), checked before
dispatch and again before the sandbox is built, failing closed at
startup if the policy is unreadable (grain `docs/local-policy.md`).
*Residual:* within that policy, a forged task runs with the forged
requester's name in its commits. *Status:* **accepted (owner
decision)**; signed claims (grain's `silo_issuer`) are future work on
the hub's side.

**T-TB3-2 (E) A Grain with no local policy.** grain enforces nothing
when started without `-policy` ("With no `-policy` flag the daemon
enforces nothing", grain `docs/local-policy.md`), and nothing stops
such a daemon from also passing `-hub-url`. Then the trust limit of §1
does not exist for that Grain: whatever the hub offers within the
Grain's team configuration is run. *Status:* **open, high** (O-3):
grain should refuse `-hub-url` without `-policy` (a grain change, not
made from this repository); the conformance kit (task 4236) should
check that a Grain refuses a task outside its declared policy.
INV-12.

**T-TB3-3 (E) The hub driving a Grain beyond the task interface.**
*Mitigation:* the hub reaches a node only through the streams the
contract defines (hub-interface.md): `offer`, `resume`, `cancel`, `view`
and, where the owner turned it on, `http`. grain answers offer, resume
and cancel and closes anything else unread (grain `pkg/hubnode`,
`Config.Serve`); a view is served only for a ref the node itself listed
(hub-interface.md; grain does not serve views yet), and the hub asks for
no other: it keeps each task's listed refs in `task_refs` and answers 404
to a view of any other ref, a `../` path among them, without reaching the
node (`live.Hub.serveArtifact`, `TestViewOnlyListedRefs`, SA1-M3). An offer is filed
through grain's own task API as the submitter, so grain's own team
rules also apply (`hubTasks.Start`). *Residual:* the `http` stream,
once grain serves it, gives the hub whatever the node's UI gives its
owner, as that owner, for as long as the owner's IAP assertion is
fresh: a compromised hub that holds a viewer's assertion can drive the
owner's Grain UI (settings, secrets panes) within that window. A
customised Grain may serve more than the contract. *Status:* mitigated
for the task interface; the `http` stream's residual is open (O-2) and
must be re-reviewed when grain's side (hub.md §11.3) lands. INV-11.

**T-TB3-4 (I/S) The node UI passing the viewer's credentials to the
node.** `hub/nodeui` forwards the browser's request with IAP's
assertion (for the hub's own audience) and every cookie except the hub's
OIDC cookies (`rewrite`, `hubCookies`, `clientHeaders`). *Mitigation:*
each node has its own host, so a node's pages never run on the hub's
origin; only the owner may open it, not hub admins
(`nodeui.Server.ServeHTTP`, `TestAccess`); `Authorization` and the
hub's cookies are dropped; a `Set-Cookie` with a `Domain` is dropped
(`modifyResponse`), so a node cannot set cookies on the hub or its
siblings; WebSocket upgrades must come from the node's own origin
(`sameOrigin`); a node's pages are the hub's *site* under the default
domain, so they would carry the viewer's hub cookies to the hub, but
the hub refuses every write not from its own origin (T-TB1-4, INV-16).
*Residual:* (a) a malicious node receives a valid IAP
assertion for the hub's audience, usable within its lifetime against
anything that verifies that audience without IAP in front of it: other
Grains that add the hub's audience (hub.md §11.3), or a hub pod reached
other than through the load balancer; (b) whether IAP passes its own
session cookie through to the backend, and therefore to the node, is
not verified; if it does, `rewrite` must drop it too. *Status:* **open,
medium** (O-2): verify (b), and strip IAP's cookies; consider a
per-node audience or a hub-minted, node-scoped viewer assertion in
place of IAP's. INV-13.

**T-TB3-5 (E) Planned `approve` and `message` streams (tasks 4229,
4230, ui.md U3).** A message typed into an interactive session is a
prompt with a human's authority, and an `approve` is the approval
itself. *Required before they ship:* both carry the signed-in human as
the hub's claim and are authorised by the hub as the PDP (who may
approve: a member of the owning team, other than the requester); the
Grain treats a hub `approve` like any other approval under its policy
(`approvers`), and refuses a message for a task that is not its own
running task; neither is accepted from a node or an agent principal.
*Status:* open, design-time (O-4). INV-17.

**T-TB3-6 (D) The hub flooding a Grain.** *Mitigation:* the Grain sets
its own slots and refuses what it will not run; a refusal excludes the
node from that task. *Residual:* view and http streams are not bounded
per node. *Status:* accepted (the hub is the Grain's chosen
coordinator; a Grain may disconnect).

### TB4: hub → browser (content a Grain or agent wrote)

**T-TB4-1 (T/E) A malicious Grain or an injected agent attacking hub
users through stored or streamed content (XSS).** Everything in the
conversation, the token stream, titles and prompts, refusal and result
reasons, artifact kinds and refs, and artifact bytes is text a node, an
agent, or an external contributor (via the agent) controls.
*Mitigation as built:* the API answers JSON (`application/json`) and
SSE (`text/event-stream`), never HTML; SSE `event:` names come from a
fixed set of types (`classOf`, `reference`), so a node cannot inject
SSE fields; artifact kinds match `^[a-z0-9][a-z0-9_.-]{0,63}$` and refs
are printable and ≤ 512 bytes (`validRef`), and a viewer may view only
a ref the node listed in the task's events, so a viewer cannot steer a
view to a ref of their own choosing such as a path to the node owner's
credentials (`Log.HasRef`, SA1-M3); artifact bytes are served
`text/plain`, `nosniff`, `Content-Security-Policy: sandbox`, `no-store`
(`live.Hub.serveArtifact`), so they never render as the hub's page; a
node's own UI is on its own host (hub.md §11.2). *Residual:* the hub
UI (tasks 4222–4227) will render this content in the hub's origin, where
a script runs with the viewer's IAP session against the whole API.
*Required of the hub UI:* render node and agent text as text, or as
markdown through a sanitiser that allows no raw HTML, no `javascript:`
or `data:` URLs, and no remote images by default; render diffs as text;
fetch artifacts and show them in memory, never navigate to them; ship a
strict CSP (`default-src 'self'`, no `unsafe-inline`, no `unsafe-eval`,
`frame-ancestors 'none'`) with `nosniff`; never use
`dangerouslySetInnerHTML` or its equivalent on node content. *As built
in U1 (ui.md §7):* text is React text; markdown drops raw HTML, keeps
only `http`, `https`, `mailto` and relative links, and shows images as
links; diffs render through `react-diff-view` as text; artifacts are
fetched into memory; the page's CSP is `default-src 'self'`,
`script-src 'self'`, styles only from the hub's files or a per-load
nonce, `img-src 'self'`, `worker-src 'none'`, `frame-ancestors 'none'`,
with `nosniff` (`hub/ui`). *Status:* mitigated for the API; built for
the hub UI's U1, **open until U1's per-stage audit** (§8.2, O-6).
INV-15.

**T-TB4-2 (I) Content cached in browsers or intermediaries.**
*Mitigation:* artifacts, SSE and minted tokens are `no-store`; ui.md
(a) says the page keeps no artifact in storage, no service worker.
*Status:* mitigated; the UI must keep it so. INV-4.

**T-TB4-3 (S) A page on another origin subscribing with the viewer's
sign-on.** *Mitigation:* browser WebSockets must come from the hub's
origin (`serveWebSocket`), node UI WebSockets from the node's
(`sameOrigin`); SSE responses carry no CORS headers, so a cross-origin
`EventSource` cannot read them. *Status:* mitigated. INV-14.

### TB5: hub → its platform (database, secrets, logs, operator)

**T-TB5-1 (I) The database leaking.** *Mitigation:* it holds no code by
class (INV-3), node tokens only as hashes (INV-6), no disk paths, no
artifact bytes. *Residual:* prompts, conversation, the directory (A-5,
A-6) are in it, and backups hold the same (hub-operations.md). *Status:*
accepted: this is the stated scope of the hub's data (hub.md §1).

**T-TB5-2 (T/E) The database or its operator rewriting the
directory.** Adding a membership or a repository ownership admits a
node and a viewer. *Mitigation:* Cloud SQL through workload identity and
the Cloud SQL proxy; no public listener (`deploy/`). The hub serves as
its service account's IAM database user, which may read and write the
hub's rows and nothing more: no DDL, and not a `cloudsqlsuperuser`
member; `silo hub migrate` refuses a runtime role that could change
the schema. The schema owner's password is given only to the pod's
`migrate` init container (SA1-L8, O-15 closed). *Residual:* a
compromised hub process can still rewrite or delete every row of the
hub's tables, which serving needs; ACT-12 is trusted; there is no audit trail of directory changes in the
hub (O-10). *Status:* accepted for the operator; audit trail open.

**T-TB5-3 (I) Logs leaking code or secrets.** *Mitigation:* the hub
logs IDs, owners, states and errors, not prompts or payloads: the
dispatcher logs task and node IDs (`tasks.Dispatcher.logf` calls), the
rendezvous owner, node ID and repositories, `live` the rejection reason
of an event (a field name or a JSON error, not the payload), `nodeui`
the method and path. Every node-supplied string the hub logs (a reason, a task ID, a
message type, a node task, an error carrying them) goes through
`internal/logsafe.Quote`, which quotes it and cuts it to 200 bytes, so
a node can neither forge a log line nor flood the log
(`hub/tasks` `TestNodeStringsLogged`); a reason is cut to 1024
characters in the task row. *Residual:* a node UI or artifact path may
carry tokens in a query string at the load balancer's request log.
Grain's own logs (its agent transcripts, CI logs, `grainlog`) hold code
and are the Grain's: out of the hub's scope, in the Grain's audit.
*Status:* mitigated (O-7 closed by SA1-L3). INV-18.

**T-TB5-4 (E) A pod reached without IAP.** *Mitigation:* every path but
`ConnectPath` verifies the IAP assertion itself; the IAP-off backend is
an exact match on `/v1/nodes/connect` (`deploy/kubernetes/hub/base/
gateway.yaml`), and the node-UI hosts route only to the IAP backend
(checked by `validate.sh`); metrics are on their own listener that no
Service names (checked by `validate.sh`); the containers run as the numeric user 65532
with a read-only root filesystem (checked by `validate.sh`, SA1-L9); a
default-deny NetworkPolicy admits 8080 only from Google's load balancer
and health-check ranges, 9091 only from the Prometheus collectors and
9092, the internal listener, only from the hub's own pods (checked by
`validate.sh`, SA1-L5). The internal listener is on no Service (checked
by `validate.sh`) and refuses any request without an HMAC, under the
`silo-hub-internal` key, bound to its own pod and the minute (hub.md
§2.2.3; `hub/server` `TestForwardedOpen`). *Residual:* `validate.sh` does not check
that the IAP-off route stays an exact match or that the dev sign-on is
never set (O-12). *Status:* mitigated; checks open. INV-2, INV-20.

**T-TB5-5 (D) A stalled dispatcher running a task twice.** A second
dispatcher term re-offering while a stalled holder's late `accept`
lands (hub.md §2.2.1). *Status:* accepted (documented window of two
seconds; the epoch guard is not built).

### TB6: Grain → forge

**T-TB6-1 (E) A task writing where it should not.** *Mitigation:* the
Grain's policy `repos` and the task's `reads`/`writes` scope the git
proxy per sandbox (grain `pkg/gitproxy`); the merge queue and the
forge's branch protection decide what lands; held changes on sensitive
paths need named approvers (grain `pkg/ui/approvals.go`). The hub has
no merge rights and no merge queue (hub.md §1.1). *Residual:* a
forged-claim task (T-TB3-1) or an injected agent can push to any branch
of any repository the policy allows and open PRs there. *Status:*
accepted, bounded by policy and branch protection.

**T-TB6-2 (R) Who asked for a change.** *Mitigation:* commits carry
`Requested-by:`/`Approved-by:` from the task's claims (grain
`docs/local-policy.md`). *Residual:* those are the hub's claims, which a
compromised hub forges (T-TB3-1). *Status:* accepted.

### TB7: agent → Grain tools and the sandbox

**T-TB7-1 (E) Prompt injection reaching ambient credentials on host
sandboxes.** An agent reads instructions from repository content, an
issue, task text, tool output or another Grain's output (ACT-6 →
ACT-9b), and uses what its sandbox carries. In a host-directory sandbox
(`-host-sandboxes`) the run uses the host's own network and, with
`-host-sandbox-env`, the operator's `SSH_AUTH_SOCK`, gcloud
configuration or proxy, "acting as the operator wherever that variable
reaches" (grain `docs/design.md`). *Mitigation:* host sandboxes are
refused on a daemon others can reach unless
`-allow-unisolated-host-sandboxes` (`CheckHostSandboxes`); a run's
`HOME` is pinned to its own directory and only an allowlist of the
daemon's environment is passed; `HOME` cannot be set through
`-host-sandbox-env`, and the flag needs `-host-sandboxes`
(`pkg/daemon/host_sandbox_env_test.go`); a task asking for network
`none` is refused rather than run on the host. *Residual:* **every task
the hub places on such a Grain** runs with those credentials: the
Grain's policy limits repositories, capabilities and network, but has no
rule that keeps hub-placed (non-owner) tasks off a host sandbox with
ambient credentials. A team member's prompt, or an external
contributor's text the agent reads, then acts as the Grain's owner.
*Status:* **open, high** (O-8): grain should refuse non-local
(`local_requesters`) tasks on host sandboxes with `-host-sandbox-env`
unless the policy says otherwise, and the Grain should register with
the hub as unisolated so the hub can say so to submitters. The
recommendation to owners meanwhile: do not connect a Grain that runs
host sandboxes with ambient credentials to a hub.

**T-TB7-2 (I/E) An agent reading the Grain's own secrets.** The node
token (A-3), model credentials (A-7) and the Grain's state are files on
the host. *Mitigation:* isolated sandbox backends do not mount them;
capability credentials are placed per run. *Residual:* a host-directory
sandbox runs as the Grain's user and can read them. *Status:* covered
by O-8.

**T-TB7-3 (E) An agent filing or approving work.** *Mitigation:* no
hub path exists for it today. *Residual:* tasks 4233 and 4235 add one.
*Status:* open, design-time (O-4, T-TB1-7).

**T-TB7-4 (E) Sandbox escape.** Out of this document's scope: grain's
sandbox design (grain `docs/design.md`) is its own threat model; this
model assumes isolated backends hold and host backends do not.

### TB8: Grain → model provider

**T-TB8-1 (I) Code and prompts sent to the model provider.**
*Mitigation:* the provider is the Grain owner's choice and credential;
the hub never talks to a model. *Residual:* a task placed by the hub on
a Grain sends that team's code to that Grain's provider; the hub cannot
see or constrain which. *Status:* accepted (admission requires the
owner to be in the owning team, who answers for their provider).

**T-TB8-2 (T) Model output as an injection source.** Treated as ACT-9b.

### TB9: CI and the supply chain

**T-TB9-1 (T/E) A CI workflow reading more than it should.**
`.grain/workflows/grain-interop.yml` declares
`repo-read:bwsalmon/grain`, clones grain's `main` unpinned and runs its
build and tests in silo's CI; `old.yml` declares
`repo-read:gke-labs/granule`. *Mitigation:* read-only scopes; scheduled
runs get no write-scoped capabilities or secrets (grain CI);
`hub-nightly.yml` needs none. *Residual:* a change on grain's `main`
runs inside silo's CI with silo's read capability for grain; silo's
CI holds no write capability, so the blast radius is read access to the
declared repositories. *Status:* accepted; any new capability in a
silo workflow is a review item (§8.1). INV-22.

**T-TB9-2 (T) Build inputs not pinned.** The `Dockerfile` builds from
`golang:1.26-bookworm` and ships `gcr.io/distroless/static-debian12:
nonroot` by tag; the deployment runs `cloud-sql-proxy:2.14.1` by tag;
modules come through `GOPROXY` with `go.sum` checked. *Residual:* a
retagged base image changes the hub's image silently; publishing and
signing the hub's image is a deployment's own step (deploy.md).
*Status:* open, medium (O-13): pin images by digest, and sign and
verify the published image.

**T-TB9-3 (T) Copied code drifting.** `internal/tunnel/mux` is copied
from grain's `pkg/tunnel/mux`, and `events` from `old/events`.
*Residual:* a security fix on one side is not on the other; the SA1-H2
bounds (one control writer, matched pongs, write deadline, window for
legacy peers, session caps) are in silo's copy and not yet in grain's. *Status:*
accepted; the per-stage audit compares them (§8.2).

## 6. Security invariants

Each invariant is a property a test or an audit can check. The **Test**
column names the tests that already check it, in this repository
unless a path says otherwise; a security suite (tasks 4239, 4242) or a
conformance case (task 4236) that checks one cites its ID in the test's
name or comment. **Gap** means nothing checks it yet.

| ID | Invariant | Test |
| --- | --- | --- |
| INV-1 | The hub learns a human's identity only from a verified IAP assertion (signature, issuer, audience, expiry, an algorithm matching the key), a verified OIDC session cookie, or a verified OIDC ID token; never from `x-goog-authenticated-user-email` or any other plaintext header, except under `-dev-user-header`. | `hub/identity` `TestIAP`, `TestAlgorithmMustMatchKey`, `TestAudienceArray`, `TestOIDCSignIn`, `TestOIDCRefusesUnverifiedEmail`, `TestOIDCStateMismatch`, `TestOIDCBearerIDToken`; `hub/server` `TestConfigSignOn` |
| INV-2 | The only route a request without human sign-on reaches is `/v1/nodes/connect` (plus `/healthz`, `/readyz`, `/metrics` on its own listener, and the internal listener's open, which needs the pods' shared key), and it answers 401 before any WebSocket exists unless the bearer is a live node token. | `hub/rendezvous` `TestUnauthenticated`; `validate.sh` (metrics and internal ports on no Service); `hub/server` `TestForwardedOpen` (internal tokens); gap: a test that walks every registered route without credentials |
| INV-3 | The hub's database never contains SILO_ONLY content: every stored event is of the lifecycle or conversation class and validates against its type's schema with no extra field; node-only events are stored as nothing and broadcast only as `{kind, ref, size}`; no result disk path is kept. | `hub/live` `TestNodeOnlyNeverStored`, `TestClassify`, `TestClassifyDropsNodePaths` |
| INV-4 | Artifact bytes and node UI bytes are never written to the hub's disk or database and never cached; artifacts are answered `text/plain`, `nosniff`, `Content-Security-Policy: sandbox`, `Cache-Control: no-store`. | `hub/live` `TestViewThrough` (the bytes pass through); `hub/faulttest` `TestViewThroughDisconnect`; gap: an assertion on all four headers, and that nothing reaches the store |
| INV-5 | A node is registered only under the owner its credential proves, under the node ID its token was minted for, and never under an ID another owner registered or has had a token for that it has not released, live or revoked. A task is resumed, cancelled, streamed into and viewed only through a node registered by the owner it was offered to. | `hub/rendezvous` `TestNodeBoundCredential`, `TestRefusals`; `hub/registry` `TestOwner`; `hub/directory` `TestNodeTokens`, `TestNodeIDReusableAfterRevoke`; `hub/tasks` `TestResumeOnlyToOwner`, `TestCancelOnlyToOwner`; `hub/live` `TestAnotherOwnersNode` |
| INV-6 | Node tokens are stored only as SHA-256 hashes; each node ID has at most one live token; revoking a token ends its node's connection at once and refuses its reconnect. | `hub/directory` `TestNodeTokens`; gap: the disconnect-on-revoke path through `directory.API` |
| INV-7 | A node is admitted, and a task offered to it or resumed on it, only if its owner is a member of the team that owns every repository the node lists (at registration) and the task's repositories (at every offer and resume). | `hub/directory` `TestAdmit`; `hub/tasks` `TestAdmissionCheckedAgain`, `TestResumeChecksAdmission` |
| INV-8 | A node's event stream is accepted only for a task the hub has placed on that same node, registered by the same owner, and carries one task only. | `hub/live` `TestStreamForAnotherNode`, `TestAnotherOwnersNode` |
| INV-9 | A caller sees a task, its events and its artifacts only as a member of its team or of the team a hub admin gave its repository with its history (or as a hub admin), and a node only as its owner, a member of a team owning one of its repositories, or a hub admin; anything else answers 404 exactly as a missing resource does. | `hub/tasks` `TestAPIVisibility`, `TestRepoSquatting`; `hub/live` `TestAccess`; `hub/directory` `TestAccess`, `TestAPI`, `TestSeesTask`; `hub/rendezvous` `TestVisibleDirectory` |
| INV-10 | A task's `submitter` is the signed-in caller; a request body naming one is refused. | `hub/tasks` `TestAPISubmit` |
| INV-11 | The hub opens only `offer`, `resume`, `cancel`, `view` and `http` streams to a node, and a `view` only of a ref the node listed in the task's artifact events; a conforming Grain closes any other stream unread, serves a view only of a ref it listed, and an `http` stream only when its owner turned it on. | `hub/live` `TestViewOnlyListedRefs`, `TestViewListedBeforeRestart` (the hub's side, SA1-M3); grain `pkg/hubnode` tests (offer, resume, cancel); task 4236 (conformance kit: a Grain refuses an unlisted and a `..` ref); gap: view and http on grain's side |
| INV-12 | A Grain refuses, and never runs, a task its local policy does not allow, whatever claims the task carries; a Grain connected to a hub runs with a local policy. | `scripts/grain-interop.sh` (a task outside grain's policy is refused); grain `pkg/policy` tests; gap: the second clause (O-3) |
| INV-13 | A node's UI is served only on that node's own host, only to the node's owner (not hub admins), never with the hub's `Authorization` header or session cookies, never setting a cookie with a `Domain`, and accepts WebSocket upgrades only from its own origin. | `hub/nodeui` `TestNode`, `TestAccess`, `TestWebSocket`, `TestViewThrough`; `hub/server` `TestNodeUIHosts`; gap: IAP's own cookies (O-2) |
| INV-14 | A browser's WebSocket subscription to task events is accepted only from the hub's own origin. | `hub/live` `TestWebSocket` |
| INV-15 | No text a node, an agent or a submitter supplied is ever interpreted as HTML or script in the hub's origin. | `ui/src/content.test.tsx` (a script-bearing title, prompt, reason, status, conversation event, result, kind and artifact, rendered inert); `hub/ui` `TestHandler` (the CSP); gap: a security suite case through a fake Grain into a browser |
| INV-16 | The hub refuses a state-changing request (`POST`, `PUT`, `DELETE`) a browser sent from any origin but the hub's own, a node UI host on the hub's site included, and one whose body is not `application/json`. | `hub/identity` `TestSameOrigin`, `TestOIDCSignIn`; `hub/tasks` `TestAPISameOrigin`; `hub/directory` `TestAPISameOrigin` |
| INV-17 | Only a human signed in to the hub can be a task's `approved_by`, never its requester, a node or an agent; a task filed by a node or an agent is `proposed` until such an approval; a Grain does not treat a hub task as approved unless it carries `approved_by`. | `hub/tasks` `TestCreate` (never the submitter), `TestAPISubmit` (a submission cannot set it), `TestOfferApprovedBy` (offered and signed); gap: the hub's approval flow and `proposed` (O-4) |
| INV-18 | The hub never logs a prompt, an event payload, artifact or node UI bytes, a token, a cookie or an IAP assertion; node-supplied strings it logs are truncated and quoted. | `internal/logsafe` `TestQuote`; `hub/tasks` `TestNodeStringsLogged` (the dispatcher's log) |
| INV-19 | Everything a node sends is bounded: a control, task or event line ≤ 1 MiB, a node UI answer line ≤ 64 KiB, a ref ≤ 512 printable bytes, a kind by pattern, the token ring ≤ 1024 events or 1 MiB per task, the listed refs kept ≤ 4096 per task; a mux session ≤ 1024 open streams, 1 MiB unread per stream and 16 MiB unread in all, one owed pong, 1024 queued resets, and a write that waits ≤ the ping timeout; and (open) nodes per user, slots, repositories and stored events per task. | `hub/live` `TestRingBound`, `TestRefsBound`, `TestClassify`; `hub/faulttest` `TestLoad`; `internal/tunnel/mux` `abuse_test.go`; gap: the open bounds (O-5) |
| INV-20 | A deployed hub has exactly one human sign-on, and it is IAP or OIDC, never `-dev-user-header`; the IAP-off backend serves exactly `/v1/nodes/connect`; node UI hosts route only to the IAP backend; containers run nonroot with a read-only root filesystem. | `hub/server` `TestConfigSignOn`; `deploy/kubernetes/hub/validate.sh` (node UI route, metrics, NetworkPolicy, numeric user); gap: dev sign-on and the exact match (O-12) |
| INV-21 | A Grain passes the operator's ambient credentials only to host-directory sandboxes, only for names its operator listed, never `HOME`, and runs host sandboxes on a reachable daemon only with `-allow-unisolated-host-sandboxes`. | grain `pkg/daemon/host_sandbox_env_test.go`, `pkg/agent/sandbox_env_args_test.go`; gap: hub-placed tasks (O-8) |
| INV-22 | silo's CI workflows declare no write-scoped capability, and each read capability names a repository the workflow's own comment justifies. | gap: a check over `.grain/workflows` |
| INV-23 | The hub's store holds a node token's secret in no form but its hash, and a minted token is returned once with `Cache-Control: no-store`. | `hub/directory` `TestNodeTokens`; gap: the header |

## 7. Open findings

The `O-` findings from §5, in the order they should be closed. Each is
a candidate task; none is fixed by this document.

| ID | Finding | Severity | Threat | Where it is fixed |
| --- | --- | --- | --- | --- |
| O-3 | A Grain connected to a hub may run without a local policy. | high | T-TB3-2 | grain (`-hub-url` requires `-policy`); conformance kit (4236) |
| O-4 | No hub approval flow to record `approved_by` (carried and signed since task 4406; grain files offers without one for local approval); no design yet for agent-filed tasks, `approve` or `message`. | high, before 4229/4230/4233/4235 | T-TB1-7, T-TB3-5, T-TB7-3 | silo (`proposed`, an approval flow), grain (`hubTasks.Start`) |
| O-8 | Hub-placed tasks reach host sandboxes with ambient credentials. | high | T-TB7-1 | grain (policy rule; register as unisolated) |
| O-2 | The node UI forwards the viewer's IAP assertion, and possibly IAP's cookie, to the node. | medium | T-TB3-3, T-TB3-4 | silo (`nodeui.rewrite`), before grain's side ships |
| O-6 | The hub UI must render node and agent content safely (§5 TB4). Built in U1 (T-TB4-1's "as built"), awaiting U1's per-stage audit. | medium, blocks U1 | T-TB4-1 | silo, tasks 4222–4227 |
| O-5 | No caps on a node's slots, repositories, first-line wait, or stored events, or on a user's nodes; no per-user rate limits. (Streams per node are capped by the mux, SA1-H2.) | medium | T-TB1-8, T-TB2-6, T-TB2-7 | silo |
| O-13 | Images pinned by tag, not digest; the published image is not signed. | medium | T-TB9-2 | silo `Dockerfile`, `deploy/` |
| O-10 | No audit log of directory changes (repository assignments, releases and team deletions are logged lines only). | medium | T-TB1-9, T-TB5-2 | silo |
| O-12 | `validate.sh` does not check that the dev sign-on is unset and the IAP-off route is an exact match. | low | T-TB1-1, T-TB5-4 | silo `deploy/` |
| O-9 | The node directory shows a node's repositories of other teams. | low | T-TB1-2 | silo (`SeesNode` filtering the entry) |
| O-11 | Node tokens never expire. | low | T-TB2-5 | silo `hub/directory` |

Closed with security audit 1's medium finding SA1-M1: O-1 (every
state-changing route of the JSON APIs and the OIDC sign-out refuses a
browser's request from any origin but the hub's, including a node UI
host on the hub's own site, and a body that is not
`application/json`: `identity.SameOrigin`; T-TB1-4, INV-16).

Closed by security audit 1's low findings: O-7 (node-supplied strings
are quoted and cut in logs, reasons cut in the task row: SA1-L3),
O-14 (principals are `user:<email>` or `group:<name>`, in
`team_members` and `HubAdmins` alike, and each is matched only against
its own claim, so a group named like an email or like an admin group
grants nothing it is not: SA1-L6; `hub/directory` `TestAccess`,
`TestIsHubAdmin`, `TestLegacyPrincipals`) and O-15 (the hub serves as a least-privileged database role, its service
account's IAM database user with DML on its own tables alone, and only
the `migrate` init container, as the schema's owner, changes the
schema: SA1-L8; `hub/server` `TestMigrateThenServeLeastPrivileged`).

The accepted risks, for the record: forged claims from a compromised hub
(T-TB3-1, T-TB6-2), a Grain's account of its own work (T-TB2-6), text a
node chose to put in conversation (T-TB2-4), visibility following a
repository a hub admin transferred with its history (T-TB1-2), 12-hour OIDC sessions (T-TB1-5),
the operator (T-TB5-1, T-TB5-2), the model provider (T-TB8-1), the
dispatcher's two-second window (T-TB5-5), and silo CI's read access to
grain (T-TB9-1).

## 8. The audit plan

Three passes, at three cadences. Each pass records its result in
`docs/test-runs/` (a dated file: what was checked, against which
commits, which `INV-`/`T-` IDs passed, what was filed), and files a
task for each new finding with the `T-`/`INV-` IDs it touches.

### 8.1 Per-PR review (every pull request in silo; grain's that touch `pkg/hubnode`, `pkg/policy`, `pkg/daemon/hub.go`, sandbox backends)

A reviewer, human or agent, checks the change against the invariants
its paths touch, in this order:

1. **Does it move a boundary?** A new route, stream type, message,
   claim, listener, Service, capability, or stored field. If so, this
   file changes in the same PR: a flow in §4, threats in §5, an
   invariant in §6.
2. **The invariants its paths own:**

   | Paths | Invariants |
   | --- | --- |
   | `hub/identity`, `hub/server` (`auth`) | INV-1, INV-2, INV-20 |
   | `hub/directory` | INV-5, INV-6, INV-7, INV-9, INV-23 |
   | `hub/rendezvous`, `hub/registry`, `internal/tunnel/mux` | INV-2, INV-5, INV-19 |
   | `hub/tasks` | INV-7, INV-9, INV-10, INV-17 |
   | `hub/live`, `events` | INV-3, INV-4, INV-8, INV-14, INV-18 |
   | `hub/nodeui` | INV-13 |
   | the hub UI (planned) | INV-15, INV-16 |
   | `deploy/`, `Dockerfile` | INV-2, INV-20 |
   | `.grain/workflows` | INV-22 |
   | grain `pkg/hubnode`, `pkg/daemon/hub.go`, `pkg/policy` | INV-11, INV-12, INV-17 |
   | grain sandbox backends, `-host-sandbox-env` | INV-21 |

3. **Node and agent input is hostile:** every field a node or agent
   sends is bounded, validated, and never rendered, logged unquoted, or
   used as a path or a query without escaping.
4. **The test:** a change to an invariant's code keeps or adds the
   test the table in §6 names.

### 8.2 Per-stage audit (each stage of hub.md §9, ui.md §6, and tasks 4228–4236, before the stage is called done)

One pass over the stage's code and its interaction with what is built,
in this order:

1. **Identity and the edge** (TB1, TB5): sign-on configuration, the
   route table against INV-2, the Gateway and backend services.
2. **Tenancy** (TB1): every new read and write path against INV-9 and
   INV-7, with a second team and a non-member in the test.
3. **Node trust** (TB2, TB3): every new message or stream, from both
   sides: what a malicious Grain can send (T-TB2-*), what a
   compromised hub can send (T-TB3-*), what the stock Grain does with
   it.
4. **Content** (TB4): every new place node, agent or submitter text is
   shown or logged.
5. **The PEP** (TB3, TB7): what the stage lets a hub task do on a
   Grain, against the Grain's policy and its sandbox backend.
6. **Supply chain** (TB9): new dependencies, images, capabilities, and
   the copied `mux` and `events` against their origin.
7. **Availability** (T-TB1-8, T-TB2-7): new unbounded inputs.

Stages with a named focus:

| Stage | Focus |
| --- | --- |
| UI U1–U2 (tasks 4222–4224) | T-TB4-1, INV-15, INV-16 (O-6) |
| UI U3, interactive sessions (4229, 4230) | T-TB1-7, T-TB3-5, INV-17 (O-4) |
| Node UI, grain's side (4228, hub.md §11.3) | T-TB3-3, T-TB3-4, INV-13 (O-2) |
| Upward filing, cross-Grain dependencies (4233, 4235) | T-TB1-7, T-TB7-3, T-TB7-1 (O-4, O-8): a dependency's output is untrusted input to the next task |
| Conformance kit (4236) | INV-11, INV-12 as conformance cases |
| Replicas R2–R5 | T-TB5-4 (the internal listener, its HMAC and NetworkPolicy), INV-8 across pods |

### 8.3 Pre-launch audit (once, before the hub serves a team beyond its developers; again before any hub serves another company's Grains)

The whole of §8.2 over the whole system, plus:

1. **Every invariant in §6 has a passing test**, or a written waiver from
   the owner naming the residual.
2. **Every open finding in §7 rated high or medium is closed** or
   accepted by the owner in this file.
3. **The deployed configuration**, not only the manifests: IAP's access
   list, the backend services (IAP on and off), the IAP cookie's
   attributes (for O-1 and O-2), Cloud SQL's network and IAM, workload
   identity, Secrets, backups' access, and who holds ACT-12's roles.
4. **A black-box test from outside**: unauthenticated requests to every
   route and host, a node token for one node used for another, CSRF
   from another origin, script-bearing content through a fake Grain
   into the hub UI, and a fake Grain's resource exhaustion.
5. **A malicious-Grain exercise**: a customised Grain (the conformance
   kit's harness, or `hub/faulttest`'s fake grain) driven to every
   T-TB2 threat.
6. **A Grain owner's checklist** published with the launch: run with
   `-policy`, keep host sandboxes with ambient credentials off a hub
   (O-8 until fixed), narrow `repos`, `capabilities` and `network`.

### 8.4 Cadence

| Pass | When | Who |
| --- | --- | --- |
| Per-PR review (§8.1) | Every PR in silo; grain PRs touching the paths listed | The PR's reviewer; an automated review template may apply §8.1's checklist |
| Per-stage audit (§8.2) | Before a stage is marked built in hub.md §9 or ui.md §6 | A reviewer who did not write the stage |
| Pre-launch audit (§8.3) | Once before launch, then yearly and after any change to the accepted risks in §1 | The owner and a security reviewer |
| Threat model refresh | With every stage audit, and whenever an `O-` closes or a new boundary appears | Whoever runs the audit |
