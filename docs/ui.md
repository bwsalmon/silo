# The hub's human UI: a new UI in silo, or grain's UI pointed at the hub

Part of the [hub design](hub.md). Status: **option (a) chosen by the
owner; stage U1 built** (`hub/ui`, `ui/`; §6, and §7 for running it).

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
| U1 | `silo hub serve` serves an embedded SPA at `/`. Read-only pages: backlog, task with live events and the view-through, nodes. The frontend build in silo CI, or a committed bundle (the owner's choice). **Built:** `hub/ui` and `ui/`, built in silo CI (`.grain/workflows/ui.yml` and the image's Dockerfile); §7. | silo |
| U2 | Filing, cancel and requeue. Team admin. Minting and revoking node tokens. | silo |
| U3 | The two API gaps from §1: a `proposed` state with team-member approval, and answering `approval_request` through the hub to the node. The UI for both. | silo (API and UI), then bwsalmon/grain for the node's side of `approve` |
| U4 | A TypeScript client SDK carved from the UI's API layer, which the UI then uses. | silo |
| U5 | A Grain's local UI shows the hub task ID and links to the hub page for tasks the hub placed. | bwsalmon/grain |
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
queued/offered/running/done/failed/cancelled/lost to copy; the chip
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
# The hub, on SQLite, signing callers in from a header.
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
