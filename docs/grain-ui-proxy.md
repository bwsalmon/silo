# Opening a Grain's own UI from the hub UI

Part of the [hub design](hub.md). Status: **implemented** in
`hub/registry`, `hub/directory`, `hub/rendezvous`, `hub/nodeui`,
`hub/identity`, and `hub/server`.

A team admin looking at a Grain in the hub UI ([ui.md](ui.md), option
(a)) sometimes needs that Grain's own UI: its PRs, CI, merge queue and
settings, none of which are in the task interface
([hub-interface.md](hub-interface.md)). Grains often sit behind NAT or a
corporate firewall, so the browser cannot reach them. This doc lets the
hub carry that UI over the Grain's rendezvous connection.

Most of the transport already exists: `hub/nodeui` (hub.md §11.2, stage
2.7) proxies HTTP and WebSockets to a node's UI on its own host
`<node>.<node-ui-domain>`, for the node's **owner** only. Task 4189
concluded that this is the same wire as grain's relay
(`grain-relay`), with one HTTP/1.1 request per mux stream and a 101
spliced both ways, and that the relay can be retired for UI traffic
(§11.4). This doc **extends that one mechanism** rather than adding a
second one. It adds the capability announcement, team-admin access, the
Grain's read-only default, the identity the Grain sees, embedding,
audit, and the IAP and cookie hardening the wider audience needs.

## 1. Requirements (agreed)

1. **Hub-proxied, transparent, nothing stored.** Any HTTP UI can be
   proxied: stock, forked or custom.
2. **Optional.** A Grain announces a UI at registration. Only then does
   the hub UI show "Open Grain UI".
3. **Never on the hub's origin.** Each Grain is served on its own host.
4. **Who.** The node's owner, and admins of the Grain's owning team
   (`hub/directory` roles).
5. **The Grain decides**: off, read-only or read-write. **The default
   is read-only**, and the Grain enforces it.
6. **Identity** is a hub claim, which the Grain takes on trust.

## 2. Flow

```
browser ── https://ws-1.nodes.hub.example.com/ ──▶ GCLB + IAP (grain-UI backend)
        ──▶ silo-hub pod: nodeui.Server (host → node "ws-1"; viewer, role, team)
        ── rendezvous stream ──▶ Grain ws-1: policy → access level → pkg/ui
```

1. The hub UI lists nodes from `GET /v1/nodes`. A node whose entry
   carries `ui.offered` and that the caller may open (§4) shows **Open
   Grain UI**, with the access level the Grain announced. An offline
   node shows the button disabled, with "offline since …".
2. The button opens `https://<node>.<node-ui-domain>/` in a new tab
   (`target=_blank rel="noopener noreferrer"`). §5.4 explains why a tab
   and not an iframe.
3. IAP signs the viewer in on that host. The hub checks the viewer
   (§4), opens an `http` stream to the node (cross-pod through R3's
   `Server.Open`), and proxies the request.
4. The Grain applies its policy to the claimed role and serves the
   request at the resulting access level, or refuses it (§6).

## 3. Announcing a UI (registration)

`registry.Node` (the `hello` line) gains an optional field:

```json
"ui": {"offered": true, "team": "infra", "access": "read-only"}
```

- `offered`: the Grain answers `http` streams. Without it the hub
  answers 404 on the node's host, and the hub UI shows no button.
- `team`: the owning team whose admins may open the UI. The hub accepts
  it only if the owner is a member of that team **and that team owns
  every repository the node serves**. A node serving several teams'
  repositories would otherwise show team A's admin team B's work. If the
  check fails, the hub drops `team` and keeps owner-only access, and the
  directory says why. It does not refuse the node: the UI is not what
  the node registered for.
- `access`: advisory only, so that the hub UI can label the button. The
  Grain's answer on each stream (§6.1) is authoritative.
- **Node ID as a DNS label.** The rule is already built:
  `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, matched against exactly one
  label below the domain. The wildcard certificate covers one level
  anyway. This stage adds a refusal of `xn--` labels (punycode
  look-alikes of another node's name). A node whose ID does not qualify
  has its `ui` dropped with a reason in the directory, and it still
  registers. Registry IDs are case-sensitive, but a host is not, so only
  lower-case IDs qualify, and two nodes can never share a host.

The registry already keeps an offline node's entry, so the last `ui`
stays visible for the disabled button.

## 4. Who may open it (the hub, as PDP)

`nodeui.Server` replaces "owner only" with:

- **owner**: `e.Owner` equals the viewer, as built. Role `owner`.
- **team-admin**: `acc.Teams[ui.team] == directory.Admin`. This reads
  the store's role directly, **not `AdminOf`**, which is also true for
  hub admins. A hub admin gets in only by being a team admin in the
  directory, as built today ("a node's UI is its owner's grain").
- Anyone else gets **404**, the same answer as for an unknown node, so
  the host does not confirm that a node exists. A node that has not
  offered a UI also gets 404. An offline node gets **503** (§7).

Team members who are not admins stay out. This resolves for admins the
question hub.md §10 left open: it no longer waits on a role in grain,
because read-only is now the Grain's own default (§6).

## 5. The separate origin

### 5.1 Hosts and IAP on the Gateway

Node hosts are `*.<node-ui-domain>`. The default is `nodes.<domain>`,
and deploy.md §8 recommends a **different registrable domain**, for
example `nodes.example-grain-ui.com`. That HTTPRoute, the Certificate
Manager wildcard certificate and the DNS record exist (deploy.md §8).
The change here: the route goes to **a Service of its own**,
`silo-hub-grain-ui`, selecting the same pods, with its own
`GCPBackendPolicy` (IAP on, the same OAuth client, `timeoutSec: 3600`).
GCLB then makes it a separate backend service, which has:

- **its own IAP audience**, `/projects/N/global/backendServices/ID2`.
  `identity.IAP` takes two audiences, each bound to a host class: on a
  node host the assertion must carry the grain-UI audience, and on the
  hub's host the hub's audience. An assertion for one is refused on the
  other. The IAP JWT does not name the host, so the audience is the
  only thing that tells them apart.
- **its own IAP membership** (`roles/iap.httpsResourceAccessor` on that
  backend). It can be narrower than the hub's `iap_members`, for
  example only the groups that hold team admins.

### 5.2 Credentials that must not reach the Grain

A Grain may run any code, so the proxy passes on nothing that works
anywhere else. As built, it already drops `Authorization`, the hub's
OIDC cookies, the plaintext identity headers, and any `Set-Cookie` with
a `Domain`. This design adds:

- **No IAP credential to the node.** Strip `X-Goog-IAP-JWT-Assertion`
  and the `GCP_IAP_*` / `__Host-GCP_IAP_*` cookies. Today's §11.2/§11.3
  forward the assertion so that a Grain can verify the viewer. But a
  Grain that holds a viewer's IAP cookie could replay it, with another
  node's `Host`, to open a second Grain's UI as that admin. A Grain that
  holds the assertion has a bearer token for any path to the pods that
  skips IAP. The viewer is the hub's claim instead (§6.2), which the
  agreed requirement 6 already accepts. §11.3's "add the hub's audience
  to `GRAIN_IAP_AUDIENCE`" is withdrawn.
- **Reserved cookie names on the way back.** As well as `Domain`
  cookies, drop a `Set-Cookie` that names a hub or IAP cookie. Require
  `Secure`, and set `SameSite=Lax` where the Grain gave none.

### 5.3 Cookie scoping both ways

- **Hub to Grain.** The hub's session cookies (`silo_session`,
  `silo_oidc_flow`) are set without `Domain`, so they are host-only and
  the browser never sends them to a node host. S2 renames them with the
  `__Host-` prefix, so that no sibling host can plant a cookie of the
  same name either. The proxy strips them regardless (§5.2). IAP's
  cookies are per host.
- **Grain to hub.** A Grain's cookies are host-only (the `Domain`
  strip), so they reach neither the hub nor sibling nodes. On a
  same-site node domain, the hub's `SameSite=Lax` cookies do ride along
  on a Grain page's requests *to the hub*. INV-16
  (`identity.SameOrigin`) already refuses those writes. The
  separate-registrable-domain deployment removes the exposure for reads
  too, and is the recommended one.

### 5.4 Tab first; iframe only where it works

A new tab is the default, for two reasons. On a separate registrable
domain, every cookie the framed Grain needs (IAP's, and grain's own
session) is a **third-party cookie**, which browsers block or partition.
And IAP's first sign-in redirects to Google's sign-in page, which
refuses to be framed. Embedding is a later, optional stage (S5),
offered only when the node domain is same-site with the hub and the
viewer already holds the IAP cookie from a top-level visit. If the
frame fails to load, the hub UI falls back to the tab. The settings
when it is used:

- **The iframe** (hub UI):
  `sandbox="allow-scripts allow-same-origin allow-forms allow-popups
  allow-popups-to-escape-sandbox allow-downloads"`, with
  `referrerpolicy="no-referrer"` and `allow=""`. `allow-same-origin` is
  safe here and is needed. It grants the frame *its own* origin, the
  node host, not the hub's. Without it, the Grain's own `fetch`,
  cookies and storage would break. **No `allow-top-navigation`**: a
  Grain cannot navigate the hub tab away.
- **The hub's CSP**: `frame-src https://*.<node-ui-domain>`,
  `frame-ancestors 'none'` (no Grain can frame the hub's approve
  buttons), `X-Frame-Options: DENY`.
- **Proxied responses** (set by the hub, so that a custom Grain gets
  them too): an *added* `Content-Security-Policy: frame-ancestors
  https://<hub-host>`. A second CSP header can only tighten, so the
  Grain's own CSP still applies. Also `Referrer-Policy: no-referrer`
  and `Cross-Origin-Opener-Policy: same-origin`. The hub drops a Grain's
  `X-Frame-Options: DENY`/`SAMEORIGIN` only in S5, and only for the
  hub's host via the CSP above.

## 6. The Grain decides (PEP)

### 6.1 Policy and the answer

Local policy, in stock grain's config:

```yaml
hub_ui:
  owner: read-only        # off | read-only | read-write
  team-admin: read-only   # off | read-only | read-write
```

The default for both is **read-only**. `read-write` is an explicit
opt-in by the Grain's owner, per role, and never set by the hub. A
missing key means read-only; `off` for both means the Grain does not
announce a UI. On each `http` stream the Grain answers
`{"type":"ok","access":"read-only"}`, or `error` (which the viewer sees
as 403) for `off`. The hub logs that answer (§8) and passes it to the
page as `X-Silo-Grain-Access`, so the page can show a banner.

The reason for the default: with full UI access, a compromised hub could
approve, submit, merge and change settings on every Grain whose admins
it can impersonate. That is far beyond the task interface, where the
Grain still checks each offer against its own policy.

### 6.2 Identity: exactly how it is passed

- **Hub to Grain**: in the stream's first line, over the node's
  authenticated rendezvous connection, never as a browser-settable
  header:
  `{"type":"http","node":"ws-1","viewer":"alice@example.com","role":"team-admin","team":"infra","request_id":"…"}`.
- **Inside the Grain**: the stream handler puts it on the request's
  **context**, next to a new `via.Hub` mark (as `via.Relay` does
  today), and `pkg/ui` reads it only from there. Stock grain reads no
  identity header on any listener, so there is nothing for a loopback
  client to forge. A custom Grain whose UI is a separate process may
  set `X-Silo-Viewer`, `-Role`, `-Team` and `-Grain-Access` on its
  internal hop. It must delete any inbound copies first, and serve that
  process on a socket only the Grain process can reach.
- **Hub-proxied versus loopback.** A hub request never arrives on a
  TCP listener. It is parsed off a mux stream by `nodeui.Serve` (or an
  equivalent) and handed to the handler in-process, carrying `via.Hub`.
  `isLoopbackRequest` already returns false for any `via` mark, so
  neither loopback trust, the local API token nor `-ui-trust-relay`
  applies. The hub has no route to the Grain's loopback listener.
- **The principal**: grain serves the request as `hub:<viewer>`, a
  principal of its own kind that is not merged with a local account of
  the same email. Under read-only it has read rights. Under read-write
  it acts with the local rights the owner gave hub sessions, which are
  the owner's own by default.

### 6.3 Enforcing read-only (not just hiding buttons)

The gate is one handler at the outside of `pkg/ui`'s mux, applied only
to requests marked `via.Hub` with access `read-only`:

1. **Methods**: only `GET`, `HEAD` and `OPTIONS`. Everything else gets
   403 "read-only through the hub". That alone covers stock grain's
   writes: approve, submit, settings, console input and resize
   (`POST /api/console/{task}/input`), and database upgrade.
2. **Upgrades**: refused unless the route is on a short allow-list of
   streams that only send. Stock grain streams with SSE, and its only
   upgrade, `/relay/executor`, is never served through the hub.
3. **Reads that act or reveal**: a `GET` with side effects, or one that
   returns credentials (tokens, agent keys, secrets, a DSN), is on a
   deny list. A grain test walks every registered route and fails on any
   that is not classified as safe or denied, so new routes cannot slip
   through.
4. The SPA gets `access` from `/api/me` and hides controls it cannot
   use. That is cosmetic only.

### 6.4 Other stock grain changes

- **No path prefix**: each node is at its host's root, which is what
  `pkg/ui` and the SPA's absolute paths expect.
- **Host guard**: accept `<node>.<node-ui-domain>` (as it accepts the
  relay's host), only for `via.Hub` requests.
- **CSRF**: grain's `http.CrossOriginProtection` stays. The hub keeps
  `Host`, `Origin` and `Sec-Fetch-Site` as the browser sent them, so a
  Grain page's own writes pass as same-origin. A sibling Grain's page
  (`same-site`, not `same-origin`) is refused, as is any other site.
  This matters in read-write. In read-only, writes never get that far.
- **WebSocket and SSE**: as built, the proxy flushes every write
  (`FlushInterval: -1`) and splices a 101. The grain-UI backend's
  3600-second timeout ends each stream after an hour, and the SPA's
  `EventSource` reconnects. The hub's `sameOrigin` check on upgrades
  stays.

## 7. Offline nodes

- **Before opening**: the button is disabled and shows "last seen".
- **On the host**: the hub answers 503 with a short page of its own
  (no Grain content) that names the node and its last-seen time, with
  `Retry-After: 30` and `Cache-Control: no-store`. A stale node is
  still tried, and a failed open is 502.
- **Mid-session**: the node's streams close. SSE and WebSockets end,
  and the SPA's reconnects get 503 until the node is back. Nothing is
  buffered on the hub. A Grain that moves pods is found again through
  `Server.Open` across pods.

## 8. Audit

- **Hub**: a structured line `grain_ui.open` for the first request of
  each (viewer, node, access) in a 30-minute window and for every
  upgrade. A line `grain_ui.refused` for each 404/403/503. Each line
  records the viewer, role, team, node, the Grain's `access` answer and
  the `request_id`. This moves into the directory audit store when O-10
  is closed.
- **Grain**: the same `request_id`, viewer, role and granted access per
  session, and **every read-only refusal** (method and path), so the
  two logs can be joined.

## 9. The relay

This is the relay's job on the hub's wire (hub.md §11.1/§11.4), so the
two are one mechanism. A node that uses a hub announces `ui`, and once
its owner opens it through the hub, it stops dialling `grain-relay` for
UI traffic. It keeps the relay only for `/relay/executor` remote
executors, which the hub does not carry. Relay mode stays as it is for
Grains without a hub. One difference is deliberate: through the relay,
grain authenticates the viewer itself, using the relay's IAP and its own
access list. Through the hub, the viewer is the hub's claim, and access
is capped by §6.1. Nothing changes in the relay.

## 10. Alternatives considered

- **Link out to a directly reachable Grain URL.** Rejected. Most Grains
  are behind NAT or a firewall, so the link would be dead. Making it
  reachable means exposing each Grain to the internet with its own
  certificate and IAP or sign-in, which is the job the relay exists to
  avoid. The hub could not apply its team roles, and nothing would be
  audited in one place.
- **A path on the hub's own origin** (`/v1/nodes/{id}/ui/…`). Rejected
  as a hard requirement. The Grain's JavaScript would run with the
  admin's hub session and could call the hub API as them: approve
  proposals, mint node tokens, read other teams' tasks. Every node
  would also share one origin with every other node. It would not work
  anyway, because grain's UI uses absolute paths.
- **The hub rendering Grain data itself** (no proxy). That is the hub UI
  as it is, limited to the task interface. A Grain's PR, CI and
  settings depth is exactly what is missing.

## 11. Staged plan

The silo stages (S) and grain stages (G) can proceed in parallel up to
the point where each needs the other. S4 needs ui.md's U1 (task 4222).
G1 is hub.md §9's "Grain embedding `rendezvous.Client`".

| Stage | What | Where |
| --- | --- | --- |
| S1 | `registry.Node.UI` (`offered`, `team`, `access`) with the `team` check (§3), and the `xn--` refusal. `nodeui` answers 404 for a node that has not offered a UI. `GET /v1/nodes` returns `ui` and the node's URL. | silo |
| S2 | Team-admin access (§4). `role`, `team` and `request_id` on the `http` line, and `access` in `ok`. Strip IAP cookies and the assertion, and the reserved `Set-Cookie` names (§5.2). `__Host-` names for the hub's cookies (§5.3). Security headers on proxied responses (§5.4). The offline page (§7). Audit lines (§8). hub.md §11 and threat-model.md updated. | silo |
| S3 | The `silo-hub-grain-ui` Service and its `GCPBackendPolicy`. A second IAP audience bound to node hosts (`identity.IAP`). Terraform outputs it, and `validate.sh` checks the two are distinct. | silo (`deploy/`, `hub/identity`) |
| S4 | Hub UI: "Open Grain UI" in a new tab, with access label and offline state, and the hub's own CSP and `frame-ancestors 'none'`. | silo, after U1 |
| S5 | Optional: an embedded sandboxed iframe for same-site deployments, falling back to the tab (§5.4). | silo, after S4 |
| G1 | The rendezvous client (prerequisite, already planned). | bwsalmon/grain |
| G2 | Answer `http` streams: `via.Hub`, identity on the context, the `hub_ui` policy with read-only default, the `ok` answer with access, the host guard, the `ui` announcement in `hello`. | bwsalmon/grain, after G1 and S1 |
| G3 | The read-only gate (§6.3) and its route-classification test. A banner in the SPA. Audit lines. | bwsalmon/grain, with G2 |
| G4 | Stop dialling the relay for UI traffic when a hub is configured and no remote executors need it (§9). | bwsalmon/grain, after G3 and S2 |

G2 and G3 ship together: a Grain must never answer `http` without the
read-only gate.
