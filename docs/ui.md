# The hub's human UI: a new UI in silo, or grain's UI pointed at the hub

Part of the [hub design](hub.md). Status: **a proposal for the owner to
decide.** No code is written for it yet, and no implementation task is
filed until an option is chosen.

People need a page where they file, approve and watch tasks across
teams. The hub (hub.md) has the APIs for it: intake (§6), the directory
and sign-on (§5), and live events with the view-through (§7). It does
not have a page yet. This doc compares two places that page could come
from and recommends one.

- **(a) A new web UI in bwsalmon/silo,** served by `silo hub serve` and
  built only on the hub's public API. It may copy some of grain's
  frontend components.
- **(b) Grain's existing UI made hub-capable:** the same frontend
  (bwsalmon/grain `ui/`, served by `pkg/ui`) talking to the hub's API
  instead of, or as well as, a local grain daemon.

## 1. What the page needs, and what exists

| Surface | Hub API today | Missing under either option |
| --- | --- | --- |
| Backlog across teams | `GET /v1/tasks?team=&state=`, filtered by `Access` | nothing |
| Filing | `POST /v1/tasks`, `cancel`, `requeue` | nothing |
| Proposals | none | a `proposed` state in the backlog, and `POST /v1/tasks/{id}/approve` for members of the owning team |
| Approvals inside a run | `approval_request` is stored as conversation (§7.1) | a way to answer it: an `approve` message the hub writes to the node, on a stream like `cancel`'s |
| Live run view | SSE / WebSocket `GET /v1/tasks/{id}/events?from=` | nothing |
| View-through | `GET /v1/tasks/{id}/artifacts?ref=` | nothing |
| Grain/node directory, node tokens | `GET /v1/nodes`, `/v1/node-tokens` | nothing |
| Team admin | `/v1/me`, `/v1/teams/...` | nothing |

The two gaps are API work in `hub/tasks` and `hub/live`, and both
options need them. With them in place, the hub decides who may approve
(it is the PDP, hub.md §1.2), and records the approver in the task's
`approved_by` claim, which `tasks.Spec` carries and the offer's
attestation signs. The node
carries the decision out under its own local policy (it is the PEP).

Grain's own approval of held changes (named approvers on sensitive
paths, `pkg/ui/approvals.go`), PR state, CI and the merge queue are
**not part of the task interface** (hub.md §1.3,
[hub-interface.md](hub-interface.md)). A customised Grain may have none
of them. The hub sees them, if at all, as conversation text and links.

## 2. Option (a): a new UI in silo

**Shape.** A small single-page app, embedded in the hub binary and
served at `/` from the same origin as `/v1/...`. It is a client of the
public API only, the same API a custom UI or the SDK uses, with no
private endpoints. The pages are: backlog (per team and across the
caller's teams), task (prompt, lifecycle, the event stream, the
artifacts), file a task, proposals and approvals, nodes and node
tokens, and teams.

**Customised Grains.** The UI renders only what the task interface
defines: the lifecycle, the conversation, the token stream and
artifact references, using the schema the hub already enforces (§7.1).
A stock grain and a customised one look the same. Anything richer that
a Grain reports (a PR URL, a CI verdict) can be shown as a link, but
the UI never depends on it.

**Auth.** IAP sits in front of the page and the API alike, so the
browser carries the IAP cookie and the hub verifies the JWT on every
call (`hub/identity`). The UI asks `/v1/me` for the caller's teams and
roles only to decide which buttons to show. Every decision is still
the API's: a team the caller is not in answers 404 whatever the page
does. The page needs no CORS or credential forwarding, and the browser
WebSocket's same-origin rule (§7.1) holds as built.

**Trust boundary.** The task page fetches an artifact from
`/artifacts?ref=` with `fetch`, and shows it as text or a diff in
memory. It never stores the artifact: no service worker, no
`localStorage`, and the hub's `no-store` is respected. If the node is
offline, the page says the artifact is unavailable (503) and offers no
copy. The hub's store still holds no code, because the UI is a viewer
and not a store.

**Work and reuse.** About five screens and a few thousand lines of
frontend, plus `go:embed` and a static handler in `silo hub serve`
(stage 2.5). Use grain's stack (React, MUI, Vite) so that leaf
components can be **copied**, each with a note of the grain commit it
came from: markdown and chat rendering, the `react-diff-view` diff
pane, task state chips, theme and brand. Copying does not reuse grain's
app shell, routing or state, because those are built around grain's
`/api`. Silo CI gains a Node build step, or the built bundle is
committed. Grain's `pkg/ui` uses a placeholder `static/` for the same
problem.

**Coupling.** None at build or run time. The copies drift, and that is
accepted. If drift costs more than it saves, the shared components can
be extracted into a package later, once both sides have settled.

**A Grain's local UI.** It stays as it is after task 4175: the
single-tenant console for the node owner's one backlog, which shows the
tasks the hub placed there alongside local ones. It keeps PR, CI and
merge-queue depth. A task placed by the hub can show its hub task ID
and a link back to the hub page; that is a small change in grain.
Approvals for hub tasks happen on the hub, not in a Grain's UI.

## 3. Option (b): grain's UI made hub-capable

**Shape.** Grain's frontend is about 270 components and 157k lines
including tests. It calls some 37 families of grain endpoints (`tasks`,
`repos`, `teams`, `settings`, `ci`, `merge`, `suites`, `schedules`,
...). To make it hub-capable means a second data layer that maps those
screens onto about twenty hub endpoints, plus a "hub mode" that hides
the screens with no hub counterpart. That is most of them: settings,
CI, suites, schedules, the merge queue, secrets, executors.

**Where it is served.**
- *By the hub:* silo builds or vendors grain's frontend, so silo's
  releases follow grain's. Every grain UI change has to keep hub mode
  working, and grain's e2e suite has to test against a hub.
- *By each grain:* the browser calls the hub cross-origin from a page a
  Grain served. This needs CORS, and IAP's cookie does not go
  cross-origin, so either the Grain proxies the user's hub credential
  or the user signs in twice. A Grain that proxies a human's hub
  identity puts that credential inside a PEP that may be running
  customised code, which is the wrong side of the boundary. It also
  means a person needs a running Grain to see their team's backlog.

**Task 4175.** That task removes multi-user accounts and team RBAC from
grain because a Grain is single-tenant. Hub mode would put multi-team
views, team admin and per-team approvals back into grain's frontend.
The backend would be single-tenant but its UI would not be. The
strip-out would be partly reversed.

**Customised Grains.** Grain's screens assume a stock grain: PRs, CI
and the merge queue. In hub mode they show for some nodes and are
empty for others, or they need a capability probe per node. The hub
cannot tell the UI which a node is without a feature-negotiation
protocol that the task interface does not have.

**Trust boundary, auth, work.** Served by the hub, the view-through is
the same as in (a). Served by a Grain, it adds the credential problem
above. The work is larger than (a): a data layer across a large app,
the hub mode, and a cross-repo build. The reuse is high in lines but
low in fit.

## 4. A client SDK

A client SDK on the hub's API, for custom UIs, **strengthens (a) and
does not change the answer.** The hub UI should be the SDK's first
client. A UI that uses only the SDK proves that the public API is enough
to build a full UI, and keeps the hub from growing private endpoints for
its own page. The SDK does not make (b) cheaper, because grain's UI is a
client of grain's `/api`, not of the hub's. A team that wants its own
page, or a customised Grain that wants to show hub context, builds on
the SDK instead of forking either UI.

## 5. Recommendation

**Option (a): a new, small UI in bwsalmon/silo, served by `silo hub
serve`, built only on the public hub API, copying grain's leaf
components.** It fits the division in hub.md §1.1 (the hub owns the
cross-team view, and a Grain owns its own backlog). It keeps task 4175
intact, treats customised Grains the same as stock ones, and leaves
the IAP credential at the hub. The two repositories stay independent.
What it costs is a few thousand lines of new frontend and some copied
components that will drift.

Choose (b) only if one shared look across hub and node matters more
than the coupling, and accept that grain's frontend then has to stay
multi-tenant.

## 6. Staged plan for (a)

Each stage is a candidate task, to be filed once the owner chooses.

| Stage | What | Where |
| --- | --- | --- |
| U1 | `silo hub serve` serves an embedded SPA at `/`. Read-only pages: backlog, task with live events and the view-through, nodes. The frontend build in silo CI, or a committed bundle (the owner's choice). | silo |
| U2 | Filing, cancel and requeue. Team admin. Minting and revoking node tokens. | silo |
| U3 | The two API gaps from §1: a `proposed` state with team-member approval, and answering `approval_request` through the hub to the node. The UI for both. | silo (API and UI), then bwsalmon/grain for the node's side of `approve` |
| U4 | A TypeScript client SDK carved from the UI's API layer, which the UI then uses. | silo |
| U5 | A Grain's local UI shows the hub task ID and links to the hub page for tasks the hub placed. | bwsalmon/grain |
