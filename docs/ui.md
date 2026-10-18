# The hub's human UI: a new UI in silo, or grain's UI pointed at the hub

Part of the [hub design](hub.md). Status: **option (a) chosen by the
owner; stages U1, U2, U3's silo side, U4, U7's silo side, and the
Grainfiles & Subscriptions multi-tenant views built**
(`hub/ui`, `ui/`, `sdk/ts`; §6, §7 for running it, §8 for U2's writes,
§9 for proposals and approvals, §10 for interactive sessions, §11
for the client SDK, and §12 for Grainfiles, subscriptions, and
multi-tenant deployment views).

People need a multi-tenant **Grain Hub** page where they inspect
`.task.md` tasks and `.process.md` standing processes across watched
repositories, view connected Grain deployments and their subscriptions,
open tunneled local Grain UIs, and file, approve and watch tasks across
teams. The hub (hub.md) has the APIs for it: Grainfile discovery and
intake (§6), the directory and sign-on (§5), live events with the
view-through (§7), and the node UI proxy (§11). This doc compares two
places that page could come from and documents the built UI and SDK.

- **(a) A dedicated Grain Hub web UI in bwsalmon/silo,** served by
  `silo hub serve` and built only on the hub's public API. It copies
  grain's leaf frontend components and theme (`theme.ts`, `Markdown.tsx`,
  `DiffView.tsx`, `StateChip.tsx`, `Timeline.tsx`).
- **(b) Grain's existing UI made hub-capable:** the same frontend
  (bwsalmon/grain `ui/`, served by `pkg/ui`) talking to the hub's API
  instead of, or as well as, a local grain daemon.

## 1. What the page needs, and what exists

| Surface | Hub API today | Missing under either option |
| --- | --- | --- |
| Grainfiles & Processes across repositories | `GET /v1/grainfiles`, filterable by `kind` (`task` \| `process`), `recipient`, `repo`, `draft`, `node`, `url` | nothing |
| Backlog across teams | `GET /v1/tasks?team=&state=`, filtered by `Access` | nothing |
| Filing | `POST /v1/tasks`, `cancel`, `requeue` | nothing |
| Proposals | `proposed`, `POST /v1/tasks/{id}/approve` and `/reject` for members of the owning team (U3) | nothing |
| Approvals inside a run | `approval_request` is stored as conversation (§7.1); `POST /v1/tasks/{id}/approvals/{request_id}` answers it with an `approve` message to the node, on the task stream as `cancel` is (U3) | nothing (grain's side of `approve` is its own task) |
| Live run view | SSE / WebSocket `GET /v1/tasks/{id}/events?from=` | nothing |
| View-through | `GET /v1/tasks/{id}/artifacts?ref=` | nothing |
| Grain deployments, subscriptions, tunneled local UIs, node tokens | `GET /v1/nodes` (including `recipients`, `repos`, `folders`, `grainfiles`, `ui`, `ui_url`), `/v1/node-tokens` | nothing |
| Team admin | `/v1/me`, `/v1/teams/...` | nothing |

The two gaps were API work in `hub/tasks` and `hub/live`, which both
options needed; U3 closed them (§9). The hub decides who may approve
(it is the PDP, hub.md §1.2), and records the approver in the task's
`approved_by` claim, which `tasks.Spec` carries and the offer's
attestation signs. The node
carries the decision out under its own local policy (it is the PEP).

Grain's own approval of held changes (named approvers on sensitive
paths, `pkg/ui/approvals.go`), PR state, CI and the merge queue are
**not part of the task interface** (hub.md §1.3,
[hub-interface.md](hub-interface.md)). A customised Grain may have none
of them. The hub sees them, if at all, as conversation text and links.

## 2. Option (a): the Grain Hub UI in silo

**Shape.** A single-page app branded **Grain Hub**, embedded in the hub
binary and served at `/` from the same origin as `/v1/...`. It is a
client of the public API only, the same API a custom UI or the SDK uses,
with no private endpoints. The pages are: backlog (per team and across
the caller's teams), Grainfiles & Processes (`/grainfiles`), task
(prompt, lifecycle, the event stream, the artifacts), file a task,
proposals and approvals, deployments/nodes (`/nodes`, showing each
deployment's `recipient` identities, subscribed repositories and
processes, online/capacity status, tunneled local Grain UI links, and
node tokens), and teams.

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

As built (U4, §11), the SDK is `sdk/ts`, and the hub UI reaches the
API through it and nothing else.

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
| U1 | `silo hub serve` serves an embedded SPA at `/`. Read-only pages: backlog, task with live events and the view-through, nodes. The frontend build in silo CI, or a committed bundle (the owner's choice). **Built:** `hub/ui` and `ui/`, built in silo CI (`.grain/workflows/ui.yml` and the image's Dockerfile); §7. | silo |
| U2 | Filing, cancel and requeue. Team admin. Minting and revoking node tokens. **Built:** `ui/src/pages/FileTask.tsx`, `Teams.tsx`, `TeamPage.tsx`, the task page's actions and `ui/src/components/NodeTokens.tsx`, with `GET /v1/teams/{team}` added to the public API (hub.md §5.3); §8. | silo |
| U3 | The two API gaps from §1: a `proposed` state with team-member approval, and answering `approval_request` through the hub to the node. The UI for both. **Built (silo side):** `proposed` and `rejected` in `hub/tasks`, `approve`, `reject` and `approvals` in its API, the `approve` message on the task stream (hub.md §6.1, §6.2; [hub-interface.md](hub-interface.md)), `ui/src/pages/Proposals.tsx`, `ProposalActions.tsx` and `ApprovalPrompt.tsx`; §9. The node's side of `approve` is bwsalmon/grain's. | silo (API and UI), then bwsalmon/grain for the node's side of `approve` |
| U4 | A TypeScript client SDK carved from the UI's API layer, which the UI then uses. **Built:** `sdk/ts` (`@silo/hub-sdk`, not published yet), which `ui/src/api.ts` instantiates and the UI uses for every request; `ui/src/sdkonly.test.ts` fails on a request of the UI's own, and the SDK's routes test on a route `docs/hub.md` does not document (the team settings routes were added to its §5.3 table); §11. | silo |
| U5 | A Grain's local UI shows the hub task ID and links to the hub page for tasks the hub placed. | bwsalmon/grain |
| U7 | Interactive sessions: long-lived chat tasks with an agent, like grain's chat and director tasks, viewable and answerable from the hub. **Built (silo side):** the `interactive` task kind, a Grain's `interactive` feature in `hello` and placement by it, `POST`/`GET /v1/tasks/{id}/messages`, the `message` message on the task stream, and the `awaiting_reply` and `message` events (hub.md §6.5, §7.1; [hub-interface.md](hub-interface.md)); in the UI, *Start a session*, the reply box and "waiting for you" (`ui/src/components/Session.tsx`); §10. The node's side is bwsalmon/grain's. | silo (API and UI), then bwsalmon/grain for the node's side |
| U6 | Future: "Open Grain UI" for team admins, a Grain's own UI proxied by the hub on its own origin, read-only unless the Grain's owner opts in. Designed in [grain-ui-proxy.md](grain-ui-proxy.md), with its own silo (S1–S5) and grain (G1–G4) stages. | silo, then bwsalmon/grain |

## 7. As built: U1

**Where.** The app's source is `ui/` (React, MUI and Vite, with vitest).
`npm run build` writes its bundle to `hub/ui/static/app`, which is not
committed. Package `hub/ui` embeds it with `go:embed`, and `hub/server`
serves it on every path the API does not have. A Go build without the
bundle embeds `hub/ui/static/placeholder.html` instead, so `go build`
needs no Node. grain CI builds the bundle in `.grain/workflows/ui.yml`
(type-check, build, vitest, then `hub/ui` and `hub/server`'s tests with
the bundle embedded), and the image's Dockerfile builds it in a Node
stage of its own.

**Pages.** The backlog at `/` (every team the caller is in) and
`/teams/{team}`, filtered by state; a task at `/tasks/{id}` (its prompt,
lifecycle, and its events over SSE from `GET /v1/tasks/{id}/events`,
with each artifact the events name viewed on request through
`/artifacts?ref=`); and the node directory at `/nodes`. The app calls
only `/v1/me`, `/v1/tasks`, `/v1/tasks/{id}`, its `events` and
`artifacts`, and `/v1/nodes`. `/v1/me` fills the team selector and
says who is signed in; it decides nothing.

**The trust boundary.** An artifact is fetched with `cache: 'no-store'`
into component state, shown as text or a `react-diff-view` diff, and
dropped when the view is closed. A 503 says the node is offline and
shows no copy. The app names no browser storage API. The hub serves it
under a CSP with `worker-src 'none'`, so the browser refuses a service
worker; `connect-src 'self'`; scripts only from the hub's files; and
styles from its files or a `<style>` carrying a nonce `hub/ui` writes
into each load of the page. `ui/src/trust.test.tsx` checks the view
(no service worker registered, nothing written to storage, the fetch
uncached, the offline case) and that the bundle registers no service
worker.

**Content.** Every title, prompt, reason and event is text: markdown
renders with raw HTML dropped, links only to `http`, `https`, `mailto`
or relative URLs, and images as links rather than loads. React never
sets HTML from a string. `ui/src/content.test.tsx` feeds script-bearing
text through every field and event type (threat-model T-TB4-1, INV-15).

**Copied components.** From bwsalmon/grain `ui/` at commit
`7781f615c603f43e9170cc8bb329a29ad0343c42`, as TypeScript, each file
opening with a comment naming its grain path and that commit and
listing what the copy changes:

- `ui/src/components/Markdown.tsx`, from
  `ui/src/components/Markdown.jsx` (react-markdown with remark-gfm and
  remark-breaks, memoised). The copy drops raw HTML (`skipHtml`)
  where grain shows it escaped; its `safeUrl` also refuses a URL with
  whitespace or a control character in it; a refused link is left as
  text; an image is a link rather than a load; and grain's
  `style.css` `.markdown` rules are MUI `sx` on theme tokens.
- `ui/src/components/DiffView.tsx`, from
  `ui/src/components/DiffView.jsx` (react-diff-view: side by side,
  inline or raw, with within-line marks), and `ui/src/phone.ts`, from
  `ui/src/phone.js`, which picks its default layout. The copy reads
  the theme where grain reads its stylesheet's custom properties.
- `ui/src/theme.ts`, from `ui/src/theme.js`: grain's light and dark
  palettes, each with a reduced-motion variant. `main.tsx` picks one
  from the browser's `prefers-color-scheme` and
  `prefers-reduced-motion`; grain's own in-app switch, which keeps the
  choice in browser storage, is not copied. Grain's `SIDEBAR_WIDTH`
  (its app shell) is left out, and `mono` is the hub's.

`ui/src/components/StateChip.tsx` stays the hub's own: grain draws
its task states with stylesheet classes over its own set of states,
not with a component, so there is nothing that fits the hub's
proposed/queued/offered/running/done/failed/cancelled/rejected/lost to copy; the chip
takes its colours from the copied palette. Grain's chat rendering
(`TaskChat`, on `@chatscope`) is tied to grain's own task, comment and
transcript model and is not copied; the hub's timeline
(`Timeline.tsx`) renders hub/live's events with the copied Markdown.
`ui/src/components/copied.test.tsx` checks the copies as the hub uses
them; `content.test.tsx` and `trust.test.tsx` still check their
safety.

**Auth.** Same-origin behind IAP or the OIDC session. The app sends the
browser's own cookie and no other credential, and the hub answers no
CORS. In local development the hub's `-dev-user-header` signs the
caller in. A browser cannot set that header, so Vite's dev server sets
it on the requests it proxies.

### 7.1 Running the UI locally

With Node 20.19 or later:

```sh
# The hub, on SQLite and signing callers in from a header: both for
# local development only (-dev-user-header allows the SQLite).
go run ./cmd/silo hub serve -database sqlite:/tmp/hub.db \
    -dev-user-header X-Silo-User -admins user:dev@example.com

# The UI with hot reload at http://localhost:5173. It proxies /v1 to
# the hub and adds X-Silo-User: dev@example.com to each request.
cd ui && npm ci && npm run dev
```

`SILO_HUB` (default `http://localhost:8080`), `SILO_DEV_USER_HEADER`
(default `X-Silo-User`) and `SILO_DEV_USER` (default `dev@example.com`)
change what the dev server proxies to and as whom. To see the embedded
UI as the hub serves it, run `npm run build` in `ui/` and then
`go run ./cmd/silo hub serve ...`; the page is at `/`, and needs the
dev user header set by the browser (an extension) or a hub behind a
real sign-on.

`npm test` runs vitest and `npm run typecheck` runs tsc. The trust
boundary's bundle test runs after `npm run build`, and is skipped
without a bundle unless `SILO_UI_REQUIRE_BUNDLE` is set, as CI sets it.

**The browser suite.** `ui/e2e` is a Playwright suite: Chromium against
`silo hub serve` with the bundle embedded, over a SQLite file per worker
(or the database `SILO_E2E_DATABASE` names), and fake Grains
(`internal/fakegrain`), rendezvous clients that accept what they are
offered and emit only the events, results and artifacts a spec scripts
over their control API. Humans sign on with `-dev-user-header`, which
`silo hub serve` refuses on Kubernetes (`server.CheckDevSignOn`). It
covers a member's backlog and another team's task as not found; filing,
placement, live events and the token stream, an artifact through the
view-through and unavailable once its node is offline, and the end; a
reload mid-stream without duplicates; cancel and requeue; a proposal
approved and one rejected; approval requests allowed and denied; a node
token minted, shown once, and revoked, disconnecting its node; an
interactive session; axe (WCAG 2.1 A and AA) on every page; and, after
an artifact is viewed, nothing of it in Web Storage, IndexedDB, Cache
Storage or a service worker. `npm run build && npx playwright install
chromium && npm run e2e` runs it; grain CI runs it as `ui e2e`, on every
push, keeping each failed test's trace and screenshot. It takes under a
minute after the build, and its fakes act only when told, so it is not
split into a nightly tier.

## 8. As built: U2

**Pages and actions.** Each is a call to the public API (`ui/src/api.ts`)
and nothing else:

- *File a task* at `/file` and `/teams/{team}/file`: a team, and either
  one of its repositories or "any of the team's", a title, the prompt
  and an optional node, posted to `POST /v1/tasks` as `{repo}` or
  `{team}`. The teams and their repositories come from `GET /v1/teams`,
  which lists only the caller's (every team, for a hub admin). The new
  task's page opens on success.
- *Cancel* (with an optional reason) and *requeue* on the task page,
  `POST /v1/tasks/{id}/cancel` and `/requeue`, shown to the task's
  submitter, a team admin or a hub admin. They are shown in every state:
  whether a state allows one is the API's to say, with a 409.
- *Teams* at `/teams`: the caller's teams, with a hub admin's
  create-team form (`POST /v1/teams`). *A team* at
  `/teams/{team}/members` (`GET /v1/teams/{team}`, added for it; hub.md
  §5.3): its members and roles, its repositories and its settings. A
  team admin changes roles, adds and removes members, releases
  repositories and edits the settings; a hub admin also assigns a
  repository, with or without its history, and deletes the team.
- *Node tokens* on `/nodes`: the caller's tokens (every owner's, for a
  hub admin), minted with `POST /v1/node-tokens` and revoked with
  `DELETE /v1/node-tokens/{id}`.

**Who sees which button.** `/v1/me` decides only what is shown
(`ui/src/may.ts`): edit actions for a team admin of that team or a hub
admin, a hub admin's own actions for a hub admin. It is a mirror of
the API's rules, not a check: every action is sent, and the API allows
or refuses it.

**Refusals.** A write's refusal is shown beside the button that sent it,
as the API's status and its own words ("Not allowed (403): forbidden",
"Conflict (409): ..."), by `useAction` (`ui/src/action.ts`). The UI
checks nothing in advance beyond a form's required fields: no state,
role or ownership check of its own stands in for the API's.

**A minted token** is answered once, uncached (`cache: 'no-store'`, as
the hub's own `Cache-Control: no-store`), and held only in the node
tokens component's state. It is shown with a copy button
(`navigator.clipboard`, or select-and-copy where a browser gives the
page no clipboard) until the caller dismisses it, and then dropped.
The token list shows each token's ID and record, never the token,
and the hub keeps only its hash, so nothing shows it again.

**Writes and the same-origin check.** Each write is a same-origin
`fetch` with the browser's own credentials and, for a body,
`Content-Type: application/json`, which is what the hub's
`identity.SameOrigin` admits. Under `npm run dev` the Vite proxy sets
the hub's own `Origin` on each request, since the browser's is the dev
server's.

**Tests.** `ui/src/actions.test.tsx` (vitest) checks that each API call
sends the right method, path and body with same-origin credentials and
no `Authorization`, and that each form and button sends it, is shown
only where `/v1/me` allows, and shows the API's refusal; and that a
minted token is copied, dropped on dismiss and never stored.
`hub/server`'s `TestWebUIActions` sends each of those writes, as a
browser sends it, to a whole hub, and checks each reaches its endpoint
and is allowed or refused there by the caller's role; and that a write
signed out (401), or from another origin or site (403), never does.

## 9. As built: U3 (silo side)

**Proposals.** A task filed in a repository by someone outside the team
that owns it is `proposed` (hub.md §6.1) and is not dispatched. The
team's backlog links to *Proposals* at `/teams/{team}/proposals`
(`GET /v1/tasks?team=&state=proposed`), where each proposal has
*Approve* and *Reject* (with an optional reason, confirmed once):
`POST /v1/tasks/{id}/approve` and `/reject`. The same buttons are on a
proposed task's page. They are shown to a member of the task's team, in
either role, who is not its submitter (`mayApprove`, `ui/src/may.ts`);
a hub admin outside the team is not shown them, as the API refuses
them. A proposed or rejected task's page opens no event stream, since
no node has run it. Its submitter withdraws it with *Cancel*.

**Who approved.** The task page shows `approved_by` and `approved_at`
under *Approved by*, the claims the offer carries to the node.

**Approvals inside a run.** An `approval_request` event in a running
task's timeline shows *Allow* and *Deny*, with an optional reason, to a
member of the task's team (`memberOf`), sent as `POST
/v1/tasks/{id}/approvals/{request_id}`. Once answered, it shows who
allowed or denied it, and whether the node has been sent the answer,
from `GET /v1/tasks/{id}/approvals`, which the page reads while the
task has a request. A request answered already, past its deadline or
of a task no longer running is the API's 409, shown beside the
buttons; the UI does not check it first.

**Tests.** `ui/src/approvals.test.tsx` (vitest) checks each of these
calls' method, path and body with same-origin credentials, that each
button sends it, is shown only where `/v1/me` allows, and shows the
API's refusal. `hub/tasks`' `approvals_test.go` checks the store (a
proposal is never dispatchable, is approved or rejected once, not by
its submitter), the API's rules (non-members, the submitter and a hub
admin outside the team refused; an unknown request 404; an answered,
expired or finished one 409), and, over the rendezvous to a test node,
that an `approve` reaches the node on the task's stream, including an
answer kept before the node accepted. `hub/server`'s `TestWebUIActions`
sends approve, reject and an answer as the browser does.

## 10. As built: U7 (silo side)

**Starting a session.** The file-task form (`FileTask.tsx`) has *Start
a session* beside *File task*: the same form, filed with
`"kind":"interactive"` (hub.md §6.5). The prompt is the session's
opening message. The task waits in the queue until a Grain that lists
the `interactive` feature has a free slot.

**The session's page.** An interactive task's page shows *Kind:
Interactive session*, and its events panel is titled *Session*. Its
conversation is the event timeline: each `message` event renders as a
turn with its sender, as the hub recorded them (never as the node
said), and attachments as links (an `https` ref) or as their ref. A
message sent but not yet taken in by the session is listed below the
timeline, from `GET /v1/tasks/{id}/messages` (read every 5s while the
session is open), as *waiting to be sent to the session* or *sent to
the session*, until its `message` event arrives.

**Waiting for you.** While the session's last `awaiting_reply` event
is `waiting` and the task is not over, the panel shows a filled
*Waiting for you* banner, with the agent's question if it asked one,
and a *waiting for you* chip by its title. A `resumed` event clears
both. In the timeline, a wait is a warning row and a resume a quiet
*resumed*.

**Replying.** The task's submitter and the members of its team, in
either role (`mayMessage`, `ui/src/may.ts`, mirroring the API), get a
reply box under the conversation while the task is not over; a hub
admin outside the team watches but gets none. *Send* (or Ctrl+Enter)
posts `{"id","text"}` to `POST /v1/tasks/{id}/messages`. Each draft
has its own message ID, kept until the hub has the message, so sending
again after an error is the same message retried, which the hub keeps
and sends once. A refusal (a session that is over: 409) is shown as
the API says it. The box takes text only: attachments go through the
API, by reference.

**Ending a session.** *Cancel task* ends it, as for any task; the Grain
may also close it itself.

**Tests.** `ui/src/sessions.test.tsx` (vitest) checks the calls'
method, path and body with same-origin credentials, that *Start a
session* files an interactive task, that "waiting for you" follows the
`awaiting_reply` events, that messages render as they stream and a
pending one is replaced by its event, that a failed send is retried
under the same ID, who is offered the reply box, and the API's 409.
`hub/tasks`' `messages_test.go` checks the store (a retried ID is the
same message, another text under it is refused), the API's rules (a
non-member 404, a hub admin outside the team 403; queued and lost tasks
take messages, a finished, proposed or ordinary one 409), that the
dispatcher places an interactive task only on a node with the feature,
and, over the rendezvous to a test node, that a message reaches the
node on the task's stream, including one kept before the node
accepted. `hub/server`'s `TestInteractiveSession` runs a session
through a whole hub: a fake node is offered it, sends `awaiting_reply`,
is sent a member's message, echoes it with a forged sender, which the
subscriber sees replaced by the hub's record, and resumes.

## 11. As built: U4

**The package.** `sdk/ts` is `@silo/hub-sdk`, a TypeScript client for
the hub's public API with no runtime dependencies, for browsers and
Node 20 or later. Its README has the examples: listing a team's
backlog, filing a task, following its events, and fetching an artifact
without persisting it. `HubClient` has one method per route the UI
used: the caller, the backlog and its writes, proposals and
approvals, sessions' messages, the event stream, the view-through,
nodes, node tokens, teams and team settings. The models are
`src/models.ts`, and `src/errors.ts` turns the API's refusals into an
`ApiError` with a subclass for each common status (400, 401, 403, 404,
409, 503).

**Sign-on.** With no options the client acts as the UI always has:
requests are same-origin and carry only the browser's own credentials.
Given `baseURL` and `token` (a string or a function that refreshes it),
it sends `Authorization: Bearer`, or another header via `tokenHeader`
(`Proxy-Authorization` behind IAP). That is how it works from Node.

**Events.** `follow(task, {from, onEvent, onState})` and the async
iterable `events(task, {from})` deliver each seq once and in order,
and reconnect from the cursor. Two transports carry the stream. The
browser's `EventSource` is the default with no token, since it can
send only the browser's cookie and it reconnects by itself through
Last-Event-ID. A streamed `fetch`, parsed by the SDK's `SSEParser`,
carries a token and reconnects with `?from=`. A 4xx closes the stream
and is reported as its `ApiError`. The SDK does not use the API's
WebSocket form of the same stream.

**The UI on the SDK.** `ui/src/api.ts` creates the one `HubClient` and
re-exports its methods and types. `ui/src/events.ts`'s
`useEventStream` is `hub.follow`. The UI builds the SDK from source:
`ui/tsconfig.json`'s `paths` and `ui/vite.config.ts`'s alias map
`@silo/hub-sdk` to `sdk/ts/src`, and the image's Dockerfile copies it
in. `ui/src/sdkonly.test.ts` reads every app file (not tests) with its
comments removed, and fails on a call to `fetch`, a string starting
`/v1/`, an `EventSource`, a `WebSocket`, `XMLHttpRequest` or
`sendBeacon`.

**No private endpoints.** The extraction found none. Every route the
UI calls was already in hub.md, but the team settings routes were
only in §6.3's prose, so they were added to §5.3's route table.
`sdk/ts/test/routes.test.ts` checks that every `HubClient` method
sends the method and path hub.md documents for it, and that every
method is covered, so a new one fails until hub.md names its route.

**Tests.** `sdk/ts/test/*.test.ts` (vitest, in Node) cover the
browser and token modes, writes, the error mapping, the view-through
(uncached, 503 as unavailable), the SSE parser, and fetch-stream
resume after a drop with each seq once. They also cover closing on a
refusal and on an abort signal, the async iterator, and the
EventSource transport's URL and dedupe. `sdk/ts/test/integration`
builds `silo` and `internal/fakegrain` from the tree and runs the
README's examples in Node against `silo hub serve`: set up a team
through the SDK, mint a node token, file a task and list the backlog,
follow its events, close, then resume from the cursor until the end.
It also fetches an artifact through the view-through and checks the
404 and 409 errors. Grain CI runs all of it in `ui.yml`'s `sdk` job.

**Publishing.** Not yet. The package is `private: true`, and the
README's *Publishing* section records the decision: publish when a
client outside this repository needs it, versioned by the hub API
(`0.x` until `/v1` is declared stable, then the major version matches
the API's), and released from a tag by a workflow with the credential.

## 12. As built: Grain Hub Branding, Grainfiles & Subscriptions, and Deployment Views

**Branding and navigation.** The web UI (`ui/index.html`,
`hub/ui/static/placeholder.html`, `ui/src/App.tsx`) is branded **Grain
Hub**, retaining the shared Grain MUI theme (`ui/src/theme.ts`) and
shared leaf components (`Markdown.tsx`, `DiffView.tsx`, `StateChip.tsx`,
`Timeline.tsx`). The top navigation bar includes **Grainfiles**
(`/grainfiles`) alongside **Backlog** (`/`), **Teams** (`/teams`),
**Nodes** (`/nodes`), and **File a task** (`/file`).

**TypeScript SDK (`sdk/ts`, `@silo/hub-sdk`).** `HubClient` provides
typed methods and models (`Grainfile`, `GrainfileKind`,
`GrainfileTriggers`, `GrainfileSubscriptions`, `GrainfileEvent`,
`GrainfilesOverview`, `GrainfileInspection`, `GrainfileQuery`, `NodeUI`)
for the Grainfile discovery and subscription endpoints ([hub.md](hub.md)
§6):

- `getGrainfilesOverview(query)` — fetches `GET /v1/grainfiles` filtered
  by `kind` (`'task' | 'process'`), `recipient`, `repo`, `draft`,
  `include_drafts`, `node`, or `url`, normalizing `on` trigger metadata
  and timestamps.
- `listGrainfiles(query)` — lists indexed `.task.md` tasks and
  `.process.md` standing processes matching `query`.
- `inspectGrainfile(urlOrPath)` — inspects a single Grainfile's resolved
  front-matter metadata (`title`, `recipient`, `requestor`, `draft`,
  `not_before`, `created_at`, `references`, `authorized_references`,
  `depends_on`, `trigger_with`, `on`, `gate`), matching node
  subscriptions, and recent trigger/execution events.
- `listGrainfileSubscriptions(nodeId)` — lists node Grainfile
  subscriptions (`recipients`, `repos`, `folders`, `grainfiles`).
- `listGrainfileEvents(query)` — lists discoverable Grainfile trigger
  and completion events (`task_ready`, `process_updated`,
  `process_triggered`, `dependency_completed`).
- `listNodes()` (`GET /v1/nodes`) — returns each node's `recipients`,
  subscribed `repos`, `folders`, and `grainfiles`, and tunneled local UI
  metadata (`ui`, `ui_url`) when `nodeui` is offered.

**Grainfiles & Processes page (`/grainfiles`, `ui/src/pages/Grainfiles.tsx`).**
Displays indexed `.task.md` tasks and `.process.md` standing processes
across watched repositories, filterable by `kind` (`task` | `process`),
`draft` status (`all` | `active` | `draft`), `recipient` deployment URL,
and `repo`:

- Each row displays its `kind` chip (`GrainfileKindChip`), `title`,
  source repository and file path link (`safeUrl`), `recipient`
  deployment URL, optional `requestor`, `Draft` and `Not before`
  badges, `on` trigger summary (`schedule`, `push`, `merge`,
  `pull_request`, `manual`, `scenario`) and `Gate` badge, and
  `depends_on` / `trigger_with` target links.
- Selecting any Grainfile opens the inspection panel showing its full
  resolved front-matter metadata (`title`, `recipient`, `requestor`,
  `draft`, `not_before`, `created_at`, `commit_sha`, `references`,
  `authorized_references`, `depends_on`, `trigger_with`, `on`, `gate`),
  subscribed deployments, and recent trigger and execution events.

**Deployments / Nodes page (`/nodes`, `ui/src/pages/Nodes.tsx`).**
Displays each connected Grain deployment's:

- `recipient` identity URL(s) (`recipients`), linked safely via
  `safeUrl`.
- Subscribed repositories (`repos`), folders (`folders`), and explicit
  standing processes / Grainfiles (`grainfiles`).
- Online/stale/offline status (`NodeStatusChip`) and free slot capacity
  (`free - claimed of slots`).
- Direct link (**Open Grain UI**) to open the deployment's tunneled
  local Grain UI ([grain-ui-proxy.md](grain-ui-proxy.md)) when `nodeui`
  is offered and the deployment is online (and `Offline (nodeui)` when
  offline).

**Invariants preserved.** All UI requests go exclusively through
`@silo/hub-sdk` (`ui/src/sdkonly.test.ts`), all untrusted/external links
pass through `safeUrl` (`ui/src/content.test.tsx`), and no source code,
tokens, or artifacts are ever written to browser storage or service
workers (`ui/src/trust.test.tsx`, `sdk/ts/test/trust.test.ts`).
