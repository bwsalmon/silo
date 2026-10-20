---
grainfile: v1
title: "[HOLD for grain refactor] pkg/hubnode, pkg/ui, ui: implement 5-role via.Hub UI permissions, resolved light/dark theme, U3/U5/U7, and G1-G4 nodeui"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2026-10-25T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 119
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-18T23:32:34Z"
x_deferred_until: "2026-10-25T00:00:00Z"
x_capabilities:
  - grain-ci
---
**HOLD UNTIL `bwsalmon/grain` REFACTORING LANDS.**

Update `bwsalmon/grain` (`pkg/hubnode`, `pkg/ui`, `pkg/access`, `ui/`) so that when a user switches into a deployment's single-tenant Grain UI from the Grain Hub (`bwsalmon/silo` `hub/nodeui`), the deployment serves its full single-tenant Grain UI with the **exact permissions accorded to the user's Hub role (`owner`, `admin`, `member`, `proposer`, `viewer`)** and the **user's Hub-resolved `light` / `dark` theme**, and supports `U3`, `U5`, `U7`, and `G1–G4`:

### 1. Answer `http` Streams & `via.Hub` Identity/Role/Theme Context (`pkg/hubnode`, `pkg/ui`, `G1–G2`)
- Announce `ui: {"offered": true, "team": "<team>", "access": "..."}` in the Hub rendezvous `hello` message when configured with a Hub.
- Accept `http` streams opened by `hub/nodeui` (`{"type":"http","node":"...","viewer":"...","role":"owner|admin|member|proposer|viewer","team":"...","require_approval":true|false,"theme":"light|dark","request_id":"..."}`), answer `{"type":"ok","access":"..."}`, and serve the proxied HTTP/1.1 request (including SSE and WebSocket upgrades) in-process with a `via.Hub` context mark (`isLoopbackRequest` must return `false`).
- Accept `<node>.<node-ui-domain>` in the Host guard for `via.Hub` requests.

### 2. 5-Role Permission Enforcement in `pkg/ui` & `ui/` (`G3`)
- Map the Hub-claimed `role` (`owner`, `admin`/`team-admin`, `member`, `proposer`, `viewer`) and `require_approval` policy to concrete permissions enforced by an outer middleware on `pkg/ui`'s mux (and surfaced on `/api/me` so the SPA hides disallowed controls cosmetically):
  1. **`owner` and `admin` (`team-admin`):** Full deployment UI access (settings, secrets, capabilities, persistent/ephemeral sandboxes, approve/reject/cancel/requeue tasks, file tasks, and chat with the deployment's standing Director or interactive task sessions). If the deployment owner explicitly configured `hub_ui: read-only` to cap non-owner access, honor that cap.
  2. **`member`:** Full task & interactive session workflow (view all tasks/runs/transcripts/metrics, file tasks, chat with the deployment's Director or interactive sessions, reply to `ask_question` / `approval_request`, cancel/requeue tasks, and approve/reject `proposed` tasks except self-approval; when `require_approval: true`, newly filed tasks land in state `proposed` awaiting another `member`/`admin`'s approval), while deployment-admin settings/secrets/capability mutations return `403 Forbidden`.
  3. **`proposer`:** Read access (`GET`/`HEAD`/`OPTIONS` on safe routes) plus permission to submit/propose tasks (`POST /api/tasks`, which always land in state `proposed` awaiting approval from a `member`/`admin`); all other state-mutating requests (approving/canceling tasks, posting to existing sessions, changing settings/secrets) return `403 Forbidden`.
  4. **`viewer`:** Strictly read-only (`GET`/`HEAD`/`OPTIONS` on safe routes; all `POST`/`PUT`/`PATCH`/`DELETE` requests and any `GET` route returning secrets/credentials/tokens return `403 Forbidden`).
- Add a route-classification test that walks every registered route in `pkg/ui` and verifies that no unclassified route or credential-returning `GET` is accessible to `viewer`/`proposer` over `via.Hub`.
- Log structured audit entries (`request_id`, `viewer`, `role`, `team`, and any `403` role refusal).

### 3. Hub-Resolved `light` / `dark` Theme Propagation (`pkg/ui`, `ui/src/ThemeModeContext.jsx`, `ui/src/AppThemeProvider.jsx`)
- Expose the Hub-passed `theme` (`"light"` or `"dark"`, from `nodeui.Message.Theme` / `X-Grain-Hub-UI-Theme`) on `/api/me` for `via.Hub` requests.
- In `ui/src/ThemeModeContext.jsx` / `ui/src/AppThemeProvider.jsx`, when the SPA is served via the Hub with a Hub-resolved `theme` (`"light"` or `"dark"`), apply that theme directly so switching deployments in the Hub preserves the user's exact `light` / `dark` / `auto` appearance across every deployment origin without requiring per-origin `localStorage` toggling.

### 4. `U3` (In-Run Approvals), `U5` (Hub Task Link), `U7` (Interactive Sessions), & `G4` (Retire Relay for UI)
- **`U3`:** Emit `approval_request` events over the Hub event stream when a run blocks on a human approval/question and resume when `tasks.TypeApprove` arrives.
- **`U5`:** Display the Hub task link (`<hub>/tasks/<id>`) on tasks dispatched or mirrored to the Hub.
- **`U7`:** Advertise `FeatureInteractive`, stream `awaiting_reply` and `message` events to the Hub, and deliver human replies from the Hub into the live session.
- **`G4`:** Stop dialing `grain-relay` for UI traffic when a Hub is configured and no remote executors require the relay.

### Verification
- Run `go test -race ./pkg/hubnode/... ./pkg/ui/...` and `npm test` in `ui/`.
