---
grainfile: v1
title: "[HOLD for grain refactor] pkg/hubnode, pkg/orchestrator: consume Grainfile v1 (.task.grain.md, .process.grain.md, and .jinja2 templates) over Hub rendezvous"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2026-10-25T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 120
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-18T23:32:35Z"
x_deferred_until: "2026-10-25T00:00:00Z"
x_capabilities:
  - grain-ci
---
**HOLD UNTIL `bwsalmon/grain` REFACTORING LANDS.**

Implement **Grainfile `v1` (`.task.grain.md`, `.process.grain.md`, `.task.grain.md.jinja2`, and `.process.grain.md.jinja2`) subscription, consumption, and execution** in `bwsalmon/grain` (`pkg/hubnode`, `pkg/orchestrator`, `pkg/model`):

### 1. Subscriptions & Event Catch-Up (`pkg/hubnode`)
- Allow a Grain deployment to declare its `recipient` URL(s) and its subscribed repositories, folders, and `.process.grain.md` / `.process.grain.md.jinja2` URLs in the Hub rendezvous `hello` / `subscribe` protocol, persisting the last processed `grainfile_event` cursor in deployment state and sending `catchup` on reconnect.
- Handle all Grainfile event types from the Hub:
  - `task_ready`: Enqueue an idempotent task keyed by `(grainfile_url)` (or `(grainfile_url, commit)` when updated before execution), respecting `requestor` and `depends_on`.
  - `task_removed`: Cancel any queued or running task originated from the removed or re-drafted `.task.grain.md`.
  - `process_updated` / `process_removed`: Register, update, or remove standing process definitions (including local cron/schedule triggers when `when.schedule` is used).
  - `process_triggered`: Enqueue a process run for the triggering webhook/event delivery (`delivery_id` idempotency, passing event variables and honoring `gate: true` by posting a commit status / check run).
  - `dependency_completed`: Unblock waiting `.task.grain.md` items whose `depends_on` upstream tasks have completed.

### 2. Commit-Pinned Content & Same-Repo Reference Fetching (`pkg/hubnode`, `pkg/orchestrator`)
- Fetch the Grainfile markdown body and any authorized same-repo `references` (both individual files and folder/tree references) at the exact indexed `commit_sha` via `/v1/grainfiles/content` or the Hub rendezvous mux stream (`fetch_grainfile` / `fetch_reference`), placing fetched reference files into the sandbox workspace (e.g. `.grain/references/...`) for the agent run.

### 3. `.task.grain.md.jinja2` & `.process.grain.md.jinja2` Template Consumption
- Support consuming parameterized `.task.grain.md.jinja2` and `.process.grain.md.jinja2` templates from the Hub, passing deployment-configured or trigger-supplied template variables (`?var.<name>=<value>`) and validating the rendered `.task.grain.md` / `.process.grain.md` before execution.

### 4. Completion Reporting for Cross-Deployment `depends_on`
- When a `.task.grain.md` run finishes, send `grainfile_completed` (and stream the terminal `result` event over `hub/live`) so the Hub marks the Grainfile completed and unblocks any downstream `.task.grain.md` (on this or another deployment) that `depends_on` it.

### Verification
- Add end-to-end tests in `pkg/hubnode` and `pkg/orchestrator` against the `bwsalmon/silo` Hubserver and run `go test -race ./pkg/...`.
