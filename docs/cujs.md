# Grain Hub Critical User Journeys (CUJs)

This document defines the canonical suite of **12 Critical User Journeys (CUJs)** for Grain Hub (`hub/server`) and its connected Grain deployment nodes. Every CUJ specified here is continuously verified by an automated end-to-end test in [`hub/cujtest/cuj_test.go`](../hub/cujtest/cuj_test.go) running against a real in-process Hub server (`hub/server`), simulated GitHub forge (`internal/githubsim`), and simulated Grain nodes (`hub/conformance/refgrain`, `internal/fakegrain`, and `hub/rendezvous.Client`).

---

## 1. Architecture & Test Harness Overview

A Grain Hub coordinates human operators, source-code repositories, and connected Grain deployments across seven integrated subsystems:

1. **Accepted-Users Allowlist & Deployment Directory (`hub/directory`)** — Enforces the Hub-wide accepted-users allowlist (`/v1/users`), dynamic system administrators (`sys_admin`), unified deployments (`/v1/deployments` and `/v1/teams`), 5-role RBAC (`owner`, `admin`, `member`, `proposer`, `viewer`), deployment settings (`require_approval`, `anyone_may_propose`, `lost_requeue_after_seconds`), repository claims (`/v1/deployments/{id}/repos/{owner}/{name}`), and host-replacement tokens (`/v1/deployments/{id}/token`, `/v1/deployments/{id}/replace-host`).
2. **Rendezvous & Reverse Tunnel (`hub/rendezvous`, `hub/registry`)** — Authenticates Grain deployment nodes over WebSocket (`GET /v1/nodes/connect`), binds deployment IDs (`node_id`), registers capabilities and UI metadata, automatically subscribes each deployment to its canonical recipient URL (`https://<hub-host>/deployments/<id>`), and multiplexes control, task, live-event, and UI streams back to the node.
3. **Grainfile Coordinator & Content Proxy (`hub/grainfiles`)** — Parses, validates (`grainfile: v1`), resolves, and indexes `.task.grain.md` and `.process.grain.md` (plus `.jinja2` templates and legacy `.task.md` / `.process.md`) from GitHub webhooks (`POST /v1/webhooks/github`), personal Hub Grainfile repos (`/v1/grainfiles/personal/{path...}`), and deployment Hub Grainfile repos (`/v1/grainfiles/team/{team}/{path...}`). Streams authorized Grainfile bodies and `references` on demand via `GET /v1/grainfiles/content` (and Rendezvous mux `fetch_grainfile` / `fetch_reference`) without storing repository source code in database tables at rest.
4. **Task Backlog & Capability Dispatcher (`hub/tasks`)** — Manages the task lifecycle (`proposed` -> `queued` -> `running` -> `done` / `failed` / `cancelled`), enforces the proposal and approval matrix (`anyone_may_propose` x `require_approval`, blocking self-approval), matches tasks to online nodes by repository ownership and required `capabilities`, signs task attestation JWTs (`hub/claims`), and reclaims tasks from lost nodes after `lost_requeue_after_seconds`.
5. **Interactive Live Broker & Inbox (`hub/live`, `hub/tasks`)** — Ingests structured NDJSON task events (`status`, `artifact`, `approval_request`, `awaiting_reply`, `result`) from running nodes, streams them to subscribed browsers over SSE/WebSocket (`GET /v1/tasks/{id}/events`), proxies artifacts view-through (`GET /v1/tasks/{id}/artifacts?ref=...`), relays human approvals (`POST /v1/tasks/{id}/approvals/{req}`) and clarification replies (`POST /v1/tasks/{id}/messages`), and aggregates actionable items in `/v1/inbox` filtered by `/v1/me/inbox`.
6. **Hub Web UI & Node UI Subdomain Proxy (`hub/ui`, `hub/nodeui`)** — Serves the Hub SPA (including `/deployments/<name>` and the deployment switcher `/v1/nodes`), persists user theme preferences (`PUT /v1/me/theme`), and reverse-proxies `<deployment>.<node-ui-domain>` to the connected deployment's own web UI while propagating `X-Grain-Hub-UI-Theme` and the viewer's resolved role (`X-Grain-Hub-Viewer-Role`).

---

## 2. Canonical CUJ Matrix

| ID | Journey Name | Primary Subsystems | Key Endpoints & Wire Messages | Automated Test (`hub/cujtest`) |
| :--- | :--- | :--- | :--- | :--- |
| **CUJ-1** | System Admin & Accepted-Users Allowlist + Deployment Creation | `hub/directory`, `hub/ui` | `PUT /v1/users/{email}`, `DELETE /v1/users/{email}`, `POST /v1/deployments`, `GET /deployments/{id}`, `POST /v1/deployments/{id}/token`, `PUT /v1/deployments/{id}/repos/{owner}/{name}` | `TestCUJ01_SysAdminAllowlistAndDeploymentCreation` |
| **CUJ-2** | Multi-Admin Takeover (`ReplaceHost`) & State Recovery | `hub/directory`, `hub/rendezvous`, `hub/tasks`, `hub/grainfiles` | `POST /v1/deployments/{id}/token` (`replace_host: true`), `POST /v1/deployments/{id}/replace-host`, `ReclaimForNode`, `ReplayForNode` | `TestCUJ02_MultiAdminTakeoverAndStateRecovery` |
| **CUJ-3** | Personal Hub Grainfile Repo Authoring & Cross-Deployment Dispatch | `hub/grainfiles`, `hub/rendezvous` | `PUT /v1/grainfiles/personal/{path...}`, `GET /v1/grainfiles/content`, `task_ready`, `process_updated` | `TestCUJ03_PersonalHubGrainfileAuthoringAndCrossDeploymentDispatch` |
| **CUJ-4** | Deployment Hub Grainfile Repo Authoring | `hub/grainfiles`, `hub/directory`, `hub/rendezvous` | `PUT /v1/grainfiles/team/{team}/{path...}`, `GET /v1/grainfiles`, `process_updated` | `TestCUJ04_DeploymentHubGrainfileAuthoring` |
| **CUJ-5** | GitHub Repo Grainfile Authoring, Webhook Push, Cross-Repo Triggers & PR Gate | `internal/githubsim`, `hub/grainfiles`, `hub/rendezvous` | `POST /v1/webhooks/github`, `when.pull_request` (`repo`, `paths`, `gate: true`), `process_triggered` | `TestCUJ05_GitHubGrainfileWebhookCrossRepoTriggerAndGate` |
| **CUJ-6** | Cross-Repo Task Dispatch & Scoped Reference Fetching | `hub/grainfiles`, `hub/rendezvous` | `recipient: https://<hub>/deployments/<id>`, `references`, `GET /v1/grainfiles/content` (`200` vs `403`) | `TestCUJ06_CrossRepoTaskDispatchAndScopedReferenceFetch` |
| **CUJ-7** | Jinja2 Grainfile Templates (`from_template`) & Multi-Task `depends_on` DAG | `hub/grainfiles`, `hub/rendezvous` | `.task.grain.md.jinja2`, `.process.grain.md.jinja2`, `var.<name>`, `X-Grainfile-Rendered: true`, `GF032`, `dependency_completed` | `TestCUJ07_Jinja2TemplatesAndDependsOnDAG` |
| **CUJ-8** | Lifecycle Controls: `draft`, `not_before` Promotion & `grainfile_removed` | `hub/grainfiles`, `hub/server`, `hub/rendezvous` | `draft: true` -> `false`, `not_before` (`PromoteDeferred`), `DELETE` / webhook removal -> `process_removed` (`action: "removed"`) | `TestCUJ08_DraftNotBeforeAndGrainfileRemovalLifecycle` |
| **CUJ-9** | Capability Routing & Lost-Node Reclamation | `hub/tasks`, `hub/rendezvous`, `hub/registry` | `POST /v1/tasks` (`capabilities`), `PUT /v1/deployments/{id}/settings` (`lost_requeue_after_seconds`), `ReclaimLost` | `TestCUJ09_CapabilityRoutingAndLostNodeReclamation` |
| **CUJ-10** | Interactive Task Execution, Live Streaming, Artifacts, Approvals & Inbox | `hub/live`, `hub/tasks`, `hub/directory` | `GET /v1/tasks/{id}/events`, `GET /v1/tasks/{id}/artifacts`, `POST /v1/tasks/{id}/approvals/{req}`, `POST /v1/tasks/{id}/messages`, `GET /v1/inbox`, `PUT /v1/me/inbox` | `TestCUJ10_InteractiveTaskStreamingArtifactsApprovalsAndInbox` |
| **CUJ-11** | Hub Web UI, Deployment Switcher, Theme Propagation & 5-Role Node UI Proxy | `hub/ui`, `hub/nodeui`, `hub/conformance/refgrain` | `GET /v1/nodes`, `GET /deployments/{id}`, `PUT /v1/me/theme`, `<deployment>.<node-ui-domain>` (`X-Grain-Hub-UI-Theme`, `X-Grain-Hub-Viewer-Role`) | `TestCUJ11_HubUIDeploymentSwitcherThemeAndRoleProxy` |
| **CUJ-12** | Task Proposal & Approval Matrix (`anyone_may_propose` x `require_approval`) | `hub/tasks`, `hub/directory`, `hub/rendezvous` | `PUT /v1/deployments/{id}/settings`, `POST /v1/tasks`, `POST /v1/tasks/{id}/approve` (`403` self-approve vs `200` second member) | `TestCUJ12_TaskProposalAndApprovalMatrix` |

---

## 3. Detailed Critical User Journeys

### CUJ-1: System Admin & Accepted-Users Allowlist + Deployment Creation

- **Actors**:
  - `sysadmin@example.com` (seeded in `Config.HubAdmins`)
  - `outsider@example.com` (unaccepted user)
  - `alice@example.com` (accepted non-admin user)
  - `secondadmin@example.com` (dynamically promoted `sys_admin`)
- **Prerequisites**:
  - Hub is initialized with `HubAdmins: []string{"user:sysadmin@example.com"}`, which seeds `accepted_users` and turns on `EnforceAcceptedUsers`.
- **Journey Steps**:
  1. `outsider@example.com` calls `GET /v1/me` and `POST /v1/deployments` -> receives `403 Forbidden` (`not on the hub accepted users list`).
  2. `sysadmin@example.com` calls `PUT /v1/users/alice@example.com` with `{"sys_admin": false}` -> `200 OK`.
  3. `sysadmin@example.com` calls `PUT /v1/users/secondadmin@example.com` with `{"sys_admin": true}` -> `200 OK`.
  4. `secondadmin@example.com` verifies dynamic sysadmin privileges by calling `GET /v1/users` (`200 OK`) and demoting/removing `sysadmin@example.com`, then attempting `DELETE /v1/users/secondadmin@example.com` (or demoting themselves when they are the last remaining sysadmin) -> rejected with `409 Conflict` (`cannot remove the last system admin`). `sysadmin@example.com` is restored.
  5. `alice@example.com` (an accepted non-admin user) creates a new deployment via `POST /v1/deployments` with `{"id": "payments-prod", "name": "Payments Prod"}` -> `201 Created`. The response includes `url: "https://<hub-host>/deployments/payments-prod"` and `recipient_url: "https://<hub-host>/deployments/payments-prod"`, and `alice@example.com` is recorded as `owner`.
  6. Attempting to create a duplicate deployment with `{"id": "payments-prod"}` returns `409 Conflict` (`deployment already exists`).
  7. `GET /deployments/payments-prod` serves the Hub SPA (`200 OK`), and `GET /v1/deployments/payments-prod` returns the deployment details with `url` and `recipient_url`.
  8. Attempting to claim an unowned repository (`PUT /v1/deployments/payments-prod/repos/acme/payments` or `PUT /v1/deployments/rogue-team/repos/victim-org/private-repo`) before `.grain/hub-claim` on its default branch names the deployment is rejected with `403 Forbidden`. Once `.grain/hub-claim` on `acme/payments`'s default branch names `payments-prod` (or its canonical deployment URL `https://<hub-host>/deployments/payments-prod`), `alice@example.com` claims `acme/payments` via `PUT /v1/deployments/payments-prod/repos/acme/payments` (`204 No Content`) and mints a node token via `POST /v1/deployments/payments-prod/token` (`201 Created`, `Cache-Control: no-store`).
  9. A simulated Grain node connects to `GET /v1/nodes/connect` using the minted token and `NodeID: "payments-prod"`, and appears online in `GET /v1/nodes`.
- **Security Invariants**:
  - Unaccepted users are blocked at `directory.API.Access` with `403 Forbidden`.
  - The Hub refuses to delete or demote the last remaining `sys_admin` (`ErrLastSysAdmin`, `409 Conflict`).
  - Deployment names are globally unique on the Hub (`ErrTeamExists`, `409 Conflict`).
  - Self-service claims of unowned repositories require proof of repository write access via `.grain/hub-claim` on the repository's default branch naming the claiming deployment (`403 Forbidden` without proof); claiming a repository already owned by another deployment returns `409 Conflict`.

---

### CUJ-2: Multi-Admin Takeover (`ReplaceHost`) & State Recovery

- **Actors**:
  - `alice@example.com` (`owner` of `payments-prod`, running Host 1)
  - `bob@example.com` (`admin` of `payments-prod`, provisioning replacement Host 2)
- **Prerequisites**:
  - `payments-prod` has `alice@example.com` (`owner`) and `bob@example.com` (`admin`), owns `acme/payments`, and has Host 1 connected with an active in-flight running task (`State == "running"`) plus an indexed standing `.process.grain.md` and an active `.task.grain.md` targeting `https://<hub-host>/deployments/payments-prod`.
- **Journey Steps**:
  1. Verify Host 1 is online, running task `T1`, and has already advanced its Grainfile subscription delivery cursor past the initial `process_updated` and `task_ready` events.
  2. Host 1 fails or becomes unreachable; `bob@example.com` (the second admin) initiates a host replacement by calling `POST /v1/deployments/payments-prod/token` with `{"replace_host": true}` (or `POST /v1/deployments/payments-prod/replace-host`).
  3. The Hub atomically:
     - Revokes all prior tokens for `payments-prod` and mints a new bearer token owned by `bob@example.com`.
     - Disconnects Host 1's active Rendezvous session and unbinds the node registry owner lock.
     - Reclaims in-flight task `T1` (`ReclaimForNode`) from `running` back to `queued` (`reclaimed_tasks: [T1]`).
     - Resets `payments-prod`'s Grainfile subscription cursor to `0` (`ReplayForNode`) and replays the standing `.process.grain.md` (`process_updated`) and active `.task.grain.md` (`task_ready`) events (`replayed_events >= 2`).
  4. Blank replacement Host 2 connects to `GET /v1/nodes/connect` with `bob@example.com`'s new token as `payments-prod`.
  5. Host 2 immediately receives the replayed `process_updated` and `task_ready` Grainfile notifications AND is offered the reclaimed task `T1`, which it accepts and runs to `done`.
- **Security Invariants**:
  - Only an `owner` or `admin` of `payments-prod` (or a Hub admin) may call `replace_host`; members, proposers, viewers, and non-members receive `403` or `404`.
  - Revoked Host 1 tokens can no longer reconnect or fetch content (`401 Unauthorized`).

---

### CUJ-3: Personal Hub Grainfile Repo Authoring & Cross-Deployment Dispatch

- **Actors**:
  - `alice@example.com` (accepted Hub user authoring Grainfiles in her personal Hub repo)
  - `payments-prod` (connected Grain deployment)
- **Prerequisites**:
  - `payments-prod` is registered and connected to the Hub, automatically subscribed to its canonical deployment recipient URL `https://<hub-host>/deployments/payments-prod`.
- **Journey Steps**:
  1. `alice@example.com` uploads a companion reference document `PUT /v1/grainfiles/personal/docs/runbook.md` containing `# Runbook\n\n## Rollback\nSteps to roll back.\n`.
  2. `alice@example.com` authors a `.task.grain.md` at `PUT /v1/grainfiles/personal/tasks/audit-Rollback.task.grain.md` with:
     - `grainfile: v1`
     - `title: Audit rollback runbook`
     - `recipient: https://<hub-host>/deployments/payments-prod`
     - `references: ["/docs/runbook.md#rollback"]`
     - An **empty Markdown body** (validating the v1 rule that `title` + `references` carry the specification when the body is empty).
  3. `alice@example.com` also authors a personal `.process.grain.md` at `PUT /v1/grainfiles/personal/processes/daily-check.process.grain.md` with `grainfile: v1` and `when: manual`.
  4. The Hub indexes both Grainfiles in `https://<hub-host>/personal/alice-at-example.com` without storing file bodies in SQLite tables, and immediately pushes a `task_ready` event to `payments-prod` over its Rendezvous control stream.
  5. `payments-prod` uses its node token to fetch the `.task.grain.md` body and the referenced `/docs/runbook.md` from `GET /v1/grainfiles/content` (and/or `conn.FetchGrainfile` / `conn.FetchReference`) at the pinned commit SHA -> `200 OK`.
  6. `payments-prod` starts and completes a run linked to `grainfile_url`, recording task completion on the Hub.
- **Security Invariants**:
  - Personal Hub Grainfiles are isolated per user (`https://<hub-host>/personal/<user-slug>`).
  - Cross-deployment access to files in `alice`'s personal repo is granted strictly to the designated `recipient` deployment and scoped to the Grainfile itself plus its declared `references`.

---

### CUJ-4: Deployment Hub Grainfile Repo Authoring

- **Actors**:
  - `dave@example.com` (`member` of `payments-prod`)
  - `nina@example.com` (non-member)
  - `payments-prod` (connected Grain deployment node)
- **Prerequisites**:
  - `payments-prod` deployment exists with `dave@example.com` as `member`; `payments-prod` node is connected to Rendezvous.
- **Journey Steps**:
  1. `nina@example.com` (non-member) attempts `PUT /v1/grainfiles/team/payments-prod/processes/triage.process.grain.md` -> rejected with `404 Not Found` (preventing deployment enumeration).
  2. `dave@example.com` (`member` of `payments-prod`) authors `PUT /v1/grainfiles/team/payments-prod/processes/triage.process.grain.md` with `grainfile: v1`, `title: Triage Production Alerts`, and `when: manual` -> `200 OK`.
  3. The Hub indexes the process under `https://<hub-host>/team/payments-prod` and emits `process_updated`.
  4. Because `payments-prod` is automatically subscribed to its own deployment Grainfile repo (`https://<hub-host>/team/payments-prod`), it immediately receives `process_updated` over Rendezvous, fetches the `.process.grain.md` content via `/v1/grainfiles/content`, and registers the standing process.
- **Security Invariants**:
  - Non-members receive `404 Not Found` on `/v1/grainfiles/team/{team}/...`; `viewer` and `proposer` roles receive `403 Forbidden` on write (`PUT`/`DELETE`).
  - Only `owner`, `admin`, and `member` principals of `{team}` (or Hub admins) may author or delete files in `/v1/grainfiles/team/{team}/...`.

---

### CUJ-5: GitHub Repo Grainfile Authoring, Webhook Push, Cross-Repo Triggers & PR Gate

- **Actors**:
  - Simulated GitHub forge (`internal/githubsim.Server`)
  - `payments-prod` deployment owning both `acme/payments` and `acme/payments-api`
- **Prerequisites**:
  - `payments-prod` claims both `acme/payments` and `acme/payments-api` in `hub/directory`, and its connected node subscribes to `https://github.com/acme/payments`.
- **Journey Steps**:
  1. In `internal/githubsim`, commit a mixed-case stem standing process `workflows/AuthGate-v2.process.grain.md` and a task `tasks/Bootstrap-DB.task.grain.md` to `acme/payments` on `main`.
     - `workflows/AuthGate-v2.process.grain.md` declares `grainfile: v1`, `gate: true`, and a cross-repo `when.pull_request` trigger watching `repo: https://github.com/acme/payments-api`, `branches: ["main"]`, `paths: ["pkg/auth/"]`.
  2. Deliver a signed `push` webhook (`X-Hub-Signature-256`) for `acme/payments` to `POST /v1/webhooks/github` -> Hub fetches both Grainfiles from `githubsim`, indexes them, and pushes `process_updated` and `task_ready` to `payments-prod`.
  3. Open a pull request in `acme/payments-api` targeting `main` that modifies `pkg/auth/handler.go`, and deliver the signed `pull_request` webhook to `POST /v1/webhooks/github`.
  4. The Hub verifies that `acme/payments` and `acme/payments-api` belong to the same deployment/team (`payments-prod`), matches the `paths: ["pkg/auth/"]` filter, records a `process_triggered` event with `gate: true`, `trigger_source: "pull_request"`, `trigger_repo: "https://github.com/acme/payments-api"`, `pr_number`, and `changed_paths: ["pkg/auth/handler.go"]`, and pushes `process_triggered` to `payments-prod`.
- **Security Invariants**:
  - Unsigned or tampered webhooks are rejected with `401 Unauthorized`.
  - Cross-repo `when:` triggers only fire when both the defining repo and the watched repo are owned by the same team/deployment in `hub/directory`.
  - Untrusted `pull_request` webhooks never index or overwrite Grainfiles from the PR head branch; they only evaluate existing base-branch `.process.grain.md` triggers.

---

### CUJ-6: Cross-Repo Task Dispatch & Scoped Reference Fetching

- **Actors**:
  - Team A (`team-a`) owning `acme/repo-a`
  - Deployment `grain-b` (`team-b`) connected to Hub
- **Prerequisites**:
  - `acme/repo-a` contains `tasks/cross.task.grain.md` with `recipient: https://<hub-host>/deployments/grain-b` and `references: ["/docs/spec.md#api"]`, plus `/docs/spec.md` and `/docs/secret.md`.
- **Journey Steps**:
  1. A signed push webhook for `acme/repo-a` indexes `tasks/cross.task.grain.md` at commit SHA `C1`.
  2. Connected node `grain-b` receives `task_ready` because its canonical deployment URL matches `recipient: https://<hub-host>/deployments/grain-b`.
  3. `grain-b` fetches `tasks/cross.task.grain.md` at `C1` via `GET /v1/grainfiles/content` -> `200 OK`.
  4. `grain-b` fetches the authorized reference `/docs/spec.md` at `C1` via `GET /v1/grainfiles/content` -> `200 OK`, returning the exact contents of `docs/spec.md` at `C1`.
  5. `grain-b` attempts to fetch `/docs/secret.md` (which is in `acme/repo-a` but NOT listed in `references`) -> rejected with `403 Forbidden`.
  6. `grain-b` attempts to fetch `/docs/spec.md` at a non-indexed commit SHA `C2` -> rejected with `403 Forbidden`.
- **Security Invariants**:
  - Same-origin reference authority (`INV-23`, `INV-24`): a Grainfile in `acme/repo-a` can only authorize access to files in `acme/repo-a` that are explicitly listed in its `references`, and only at the exact indexed commit SHA.

---

### CUJ-7: Jinja2 Grainfile Templates (`from_template`) & Multi-Task `depends_on` DAG

- **Actors**:
  - `payments-prod` deployment node and `acme/payments` repository
- **Prerequisites**:
  - `acme/payments` is owned by `payments-prod`.
- **Journey Steps**:
  1. **Rule `GF032` Validation**: Attempt to parse/index a `.task.grain.md` whose `depends_on` references a `.process.grain.md` (`./workflows/ci.process.grain.md`) -> rejected with finding code `GF032` (`.task.grain.md` `depends_on` may only target `.task.grain.md` files).
  2. **Jinja2 Template Rendering (`from_template`)**:
     - Index `templates/deploy.task.grain.md.jinja2` (declaring `{{ region }}`) and `templates/audit.process.grain.md.jinja2` (declaring `{{ env }}`).
     - Node requests `GET /v1/grainfiles/content?grainfile_url=.../templates/deploy.task.grain.md.jinja2&var.region=eu-west-1` and `GET /v1/grainfiles/content?grainfile_url=.../templates/audit.process.grain.md.jinja2&var.env=staging`.
     - Verify both responses return `200 OK`, `X-Grainfile-Rendered: true`, `X-Grainfile-From-Template` set to the canonical `.jinja2` blob URL, and rendered YAML frontmatter with `from_template` automatically populated.
  3. **Multi-Task `depends_on` DAG Progression**:
     - Index `tasks/step-1.task.grain.md` and `tasks/step-2.task.grain.md` (where `step-2` declares `depends_on: ["./step-1.task.grain.md"]`), both targeting `https://<hub-host>/deployments/payments-prod`.
     - Verify `DependenciesSatisfied` for `step-2` is initially `false`.
     - Node executes `step-1.task.grain.md` and reports completion (`conn.CompleteGrainfile` / task terminal state `done`).
     - Hub emits `dependency_completed` for `step-2.task.grain.md` (`dependency_url: ".../step-1.task.grain.md"`, `dependency_state: "done"`), `DependenciesSatisfied` becomes `true`, and `payments-prod` receives the `dependency_completed` notification to execute `step-2`.
- **Security Invariants**:
  - Jinja2 templates are rendered in a strict sandbox without filesystem/network access and re-validated as canonical Grainfiles after rendering.
  - Only the authorized recipient node of `step-1` may record its completion (`RecordDependencyCompletedByNode`).

---

### CUJ-8: Lifecycle Controls: `draft`, `not_before` Promotion & `grainfile_removed`

- **Actors**:
  - `payments-prod` deployment node and `acme/payments` repository with a controllable clock
- **Prerequisites**:
  - `payments-prod` node is connected and subscribed to `https://github.com/acme/payments` and `https://<hub-host>/deployments/payments-prod`.
- **Journey Steps**:
  1. **Draft Suppression & Promotion**:
     - Push `tasks/feature.task.grain.md` with `draft: true` -> indexed in `GET /v1/grainfiles?include_drafts=true`, but NO `task_ready` event is emitted and `/v1/grainfiles/content` refuses content access (`403 Forbidden: draft grainfile cannot authorize content access`).
     - Push an updated commit flipping `draft: false` -> Hub emits `task_ready` and `/v1/grainfiles/content` now serves the file (`200 OK`).
  2. **Deferred `not_before` Holding & Promotion**:
     - Push `tasks/scheduled.task.grain.md` with `not_before: 2026-12-01T00:00:00Z` while Hub clock is `2026-11-30T23:00:00Z` -> indexed with `ready_emitted = false`, no `task_ready` event emitted yet.
     - Advance Hub clock to `2026-12-01T00:05:00Z` and run `PromoteDeferred` -> Hub emits `task_ready` and pushes it to `payments-prod`.
  3. **Process Deletion (`grainfile_removed`)**:
     - Push a commit removing `workflows/AuthGate-v2.process.grain.md` -> Hub marks the index record removed and emits `process_removed` with `action: "removed"` to `payments-prod`.
- **Security Invariants**:
  - Draft Grainfiles never trigger execution or authorize reference content fetches.
  - Deferred Grainfiles (`not_before` in the future) do not emit `task_ready` or `process_updated` until `not_before` is reached.

---

### CUJ-9: Capability Routing & Lost-Node Reclamation

- **Actors**:
  - `ml-team` deployment owning `acme/ml`
  -3 simulated nodes connected for `ml-team`:
    - `node-cpu` (`capabilities: ["cpu"]`)
    - `node-gpu-1` (`capabilities: ["gpu", "bazel-remote"]`)
    - `node-gpu-2` (`capabilities: ["gpu", "bazel-remote"]`)
- **Prerequisites**:
  - `ml-team` configures `PUT /v1/deployments/ml-team/settings` with `{"lost_requeue_after_seconds": 60}`.
- **Journey Steps**:
  1. First connect only `node-cpu` (`capabilities: ["cpu"]`) and file a task via `POST /v1/tasks` with `{"repo": "acme/ml", "prompt": "Train model", "capabilities": ["gpu", "bazel-remote"]}` -> task remains `queued` because `node-cpu` lacks `"gpu"` and `"bazel-remote"`.
  2. Connect `node-gpu-1` (`capabilities: ["gpu", "bazel-remote"]`) -> Dispatcher immediately offers the task to `node-gpu-1`, which accepts it; task transitions to `running` on `node-gpu-1`.
  3. Disconnect `node-gpu-1` mid-run (simulating a sudden VM crash) -> task remains `running` (with node marked offline) until the deployment's `lost_requeue_after_seconds` window elapses.
  4. Connect `node-gpu-2` (`capabilities: ["gpu", "bazel-remote"]`) and advance the clock past `60s` (`ReclaimLost`) -> Hub reclaims the task back to `queued` and dispatches it to `node-gpu-2`, which completes the task (`State == "done"`).
- **Security Invariants**:
  - A task with `capabilities` is never offered to a node that does not advertise a superset of those capabilities.
  - Lost-node reclamation honors each deployment's configured `lost_requeue_after_seconds`.

---

### CUJ-10: Interactive Task Execution, Live Streaming, Artifacts, Approvals & Inbox

- **Actors**:
  - `alice@example.com` (`owner` of `payments-prod`)
  - `payments-prod` interactive Grain node
- **Prerequisites**:
  - `payments-prod` owns `acme/payments` and is connected to Rendezvous.
- **Journey Steps**:
  1. `alice@example.com` files an interactive task via `POST /v1/tasks` (`{"repo": "acme/payments", "prompt": "Deploy schema v2", "kind": "interactive"}`).
  2. `payments-prod` accepts the task and streams live NDJSON events over `/v1/nodes/connect`:
     - `status` (`phase: "running", state: "started"`)
     - `artifact` (`kind: "diff", ref: "schema.diff", title: "Schema Diff", size: 42`)
     - `approval_request` (`id: "req-deploy", tool: "kubectl_apply", summary: "Apply schema v2"`)
     - `awaiting_reply` (`message: "Which shard should be migrated first?"`)
  3. `alice@example.com` subscribes to `GET /v1/tasks/{id}/events?from=0` (SSE) and receives the replayed and live events in order.
  4. `alice@example.com` fetches `GET /v1/tasks/{id}/artifacts?ref=schema.diff` -> Hub opens a view-through `artifact` stream to `payments-prod` and streams the diff bytes with `Cache-Control: no-store` and `Content-Security-Policy: sandbox`.
  5. `alice@example.com` checks `GET /v1/inbox` -> sees the task listed with both the pending approval request (`req-deploy`) and `awaiting_reply: true`.
  6. `alice@example.com` configures her inbox filter via `PUT /v1/me/inbox` with `{"nodes": ["other-node"]}` -> `GET /v1/inbox` is now empty; updating `PUT /v1/me/inbox` to `{"nodes": ["payments-prod"]}` restores the item in `GET /v1/inbox`.
  7. `alice@example.com` approves the tool call via `POST /v1/tasks/{id}/approvals/req-deploy` (`{"decision": "allow", "reason": "LGTM"}`) and answers the question via `POST /v1/tasks/{id}/messages` (`{"text": "Migrate shard-0 first"}`).
  8. `payments-prod` receives both control messages over its task stream, emits a final `result` (`status: "done"`), and the task transitions to `done` (and leaves `GET /v1/inbox`).
- **Security Invariants**:
  - Artifact contents are streamed view-through from the node (`Cache-Control: no-store`) and never persisted in Hub SQL tables.
  - Only members/admins/owners of the deployment may approve tool calls or send messages to the running task.

---

### CUJ-11: Hub Web UI, Deployment Switcher, Theme Propagation & 5-Role Node UI Proxy

- **Actors**:
  - `owner@example.com` (`owner` of `payments-prod`)
  - `admin@example.com` (`admin` of `payments-prod`)
  - `member@example.com` (`member` of `payments-prod`)
  - `proposer@example.com` (`proposer` of `payments-prod`)
  - `outsider@example.com` (accepted Hub user not in `payments-prod`, tested with `anyone_may_propose: true` and `false`)
  - `viewer@example.com` (`viewer` of `payments-prod`)
- **Prerequisites**:
  - Hub is configured with `NodeUIDomain: "nodes.hub.test"`.
  - `payments-prod` is connected with `UI: &registry.NodeUI{Offered: true, Team: "payments-prod", Access: "interactive"}` and serves a reference Grain Node UI (`hub/conformance/refgrain` / `nodeui` handler) that inspects `X-Grain-Hub-Viewer-Role`, `X-Grain-Hub-Viewer`, and `X-Grain-Hub-UI-Theme`.
- **Journey Steps**:
  1. **Hub Web UI & Deployment Switcher**:
     - `GET /deployments/payments-prod` serves the Hub SPA (`200 OK`).
     - `GET /v1/nodes` returns `payments-prod` with `ui_url: "https://payments-prod.nodes.hub.test"` and each caller's `viewer_role` (`owner`, `admin`, `member`, `proposer`, `viewer`).
  2. **Dark/Light Theme Persistence & Proxy Propagation**:
     - `owner@example.com` sets `PUT /v1/me/theme` with `{"theme": "dark"}` -> `GET /v1/me` returns `"theme": "dark"`.
     - When `owner@example.com` requests `https://payments-prod.nodes.hub.test/api/me`, the Node UI proxy forwards `X-Grain-Hub-UI-Theme: dark`, `X-Grain-Hub-Viewer-Role: owner`, and `X-Grain-Hub-Access: interactive`.
  3. **5-Role Enforcement on `<deployment>.<node-ui-domain>`**:
     - `owner` and `admin`: full read/write/admin access (`GET /api/tasks` -> `200`, `POST /api/tasks` -> `201`, `POST /api/admin/action` -> `200`).
     - `member`: read and task-creation access (`GET /api/tasks` -> `200`, `POST /api/tasks` -> `201`), admin actions blocked (`POST /api/admin/action` -> `403`).
     - `proposer` (and non-member when `anyone_may_propose: true`): can access the proposal UI and submit proposed tasks (`GET /api/me` -> `viewer_role: "proposer"`, `POST /api/tasks` -> `201` with `state: "proposed"`), cannot run member/admin mutations (`POST /api/admin/action` -> `403`).
     - `viewer`: read-only access (`GET /api/tasks` -> `200`), all state-mutating requests (`POST`, `PUT`, `DELETE`) blocked by `hub/nodeui` with `403 Forbidden`.
     - Unauthorized user (when `anyone_may_propose: false` and not in `payments-prod`): receives `404 Not Found` on `payments-prod.nodes.hub.test`.
- **Security Invariants**:
  - `hub/nodeui` strips any client-supplied `X-Grain-Hub-*` headers before injecting verified identity, role, access mode, and theme headers.
  - `viewer` role is enforced at the reverse proxy layer (`read-only` access mode rejects non-`GET`/`HEAD`/`OPTIONS` requests with `403 Forbidden`).

---

### CUJ-12: Task Proposal & Approval Matrix (`anyone_may_propose` x `require_approval`)

- **Actors**:
  - `owner@example.com` (`owner` of `payments-prod`)
  - `member1@example.com` (`member` of `payments-prod`)
  - `member2@example.com` (`member` of `payments-prod`)
  - `proposer@example.com` (`proposer` of `payments-prod`)
  - `nonmember@example.com` (accepted Hub user not in `payments-prod`)
  - `viewer@example.com` (`viewer` of `payments-prod`)
- **Prerequisites**:
  - `payments-prod` owns `acme/payments` and has a connected Grain node ready to execute queued tasks.
- **Journey Steps**:
  1. **Cell 1: `anyone_may_propose: false`, `require_approval: false` (Default)**:
     - `nonmember@example.com` files `POST /v1/tasks` (`{"repo": "acme/payments", "prompt": "..."}`) -> `404 Not Found`.
     - `viewer@example.com` files `POST /v1/tasks` -> `403 Forbidden`.
     - `proposer@example.com` files `POST /v1/tasks` -> `201 Created` in `state: "proposed"` (`proposer` always lands in `proposed`).
     - `member1@example.com` files `POST /v1/tasks` -> `201 Created` directly in `state: "queued"` (and dispatched to the node).
  2. **Cell 2: `anyone_may_propose: true`, `require_approval: false`**:
     - `owner@example.com` updates `PUT /v1/deployments/payments-prod/settings` with `{"anyone_may_propose": true, "require_approval": false}`.
     - `nonmember@example.com` files `POST /v1/tasks` -> `201 Created` in `state: "proposed"`.
     - `member1@example.com` files `POST /v1/tasks` -> `201 Created` directly in `state: "queued"`.
  3. **Cell 3: `anyone_may_propose: true`, `require_approval: true`**:
     - `owner@example.com` updates `PUT /v1/deployments/payments-prod/settings` with `{"anyone_may_propose": true, "require_approval": true}`.
     - `member1@example.com` files `POST /v1/tasks` -> `201 Created` in `state: "proposed"` (held for peer approval because `require_approval: true`).
     - `member1@example.com` attempts to approve their own proposal via `POST /v1/tasks/{id}/approve` -> rejected with `403 Forbidden` (self-approval is forbidden when `require_approval: true`).
     - `member2@example.com` (a second member) calls `POST /v1/tasks/{id}/approve` -> `200 OK`, transitioning the task to `state: "queued"` with `approved_by: "member2@example.com"`.
     - The connected `payments-prod` node immediately receives the task offer, executes it, and transitions it to `state: "done"`.
  4. **Cell 4: `anyone_may_propose: false`, `require_approval: true`**:
     - `owner@example.com` updates `PUT /v1/deployments/payments-prod/settings` with `{"anyone_may_propose": false, "require_approval": true}`.
     - `nonmember@example.com` files `POST /v1/tasks` -> `404 Not Found`.
     - `proposer@example.com` files `POST /v1/tasks` -> `201 Created` in `state: "proposed"`, and `member1@example.com` approves it (`POST /v1/tasks/{id}/approve` -> `queued` -> `done`).
- **Security Invariants**:
  - A `proposer` or non-member (when `anyone_may_propose: true`) can never bypass `proposed` state or approve a task (`POST /v1/tasks/{id}/approve` returns `403`/`404`).
  - Under `require_approval: true`, the proposer (`filed_by`) cannot approve their own task (`ErrSelfApproval`, `403 Forbidden`).

---

## 4. Running the CUJ Suite

Run the complete automated 12-CUJ end-to-end suite with the Go race detector enabled:

```bash
go test -race -v ./hub/cujtest/...
```
