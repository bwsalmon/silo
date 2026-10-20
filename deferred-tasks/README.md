# Deferred Tasks Dump (`bsalmon` Deployment)

This directory preserves all tasks that were in the `deferred` state on the `bsalmon` (`e1010c47`) Grain deployment prior to reformatting, so they are stored in Git and can be re-imported or re-proposed after the deployment is rebuilt.

## Summary of Deferred Tasks

| Original ID | State at Dump Time | Target Repo | `deferred_at` | `deferred_until` (`not_before`) | Capabilities | Grainfile (`.task.grain.md`) | Title |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `#49` (supersedes identical `#46`) | `deferred` | `bwsalmon/silo` | `2026-10-18T18:00:54Z` | `null` | `["grain-ci"]` | [`049-hub-cujtest-real-grain-interop.task.grain.md`](./049-hub-cujtest-real-grain-interop.task.grain.md) | `[HOLD for grain refactor] hub/cujtest, interop: run the 12 Grain Hub CUJs against real bwsalmon/grain instances` |
| `#118` | `deferred, closed` (closed `2026-10-19`) | `bwsalmon/grain` | `2026-10-18T23:32:30Z` | `2026-10-25T00:00:00Z` | `["grain-ci"]` | [`118-persistent-sandboxes-staging.task.grain.md`](./118-persistent-sandboxes-staging.task.grain.md) | `[HOLD for grain refactor] pkg/orchestrator, pkg/tunnel, pkg/ui: support persistent sandboxes with auto-restart and host-port / UI sub-path routing` |
| `#119` | `deferred` | `bwsalmon/grain` | `2026-10-18T23:32:34Z` | `2026-10-25T00:00:00Z` | `["grain-ci"]` | [`119-hubnode-5-role-ui-permissions-theme.task.grain.md`](./119-hubnode-5-role-ui-permissions-theme.task.grain.md) | `[HOLD for grain refactor] pkg/hubnode, pkg/ui, ui: implement 5-role via.Hub UI permissions, resolved light/dark theme, U3/U5/U7, and G1-G4 nodeui` |
| `#120` | `deferred` | `bwsalmon/grain` | `2026-10-18T23:32:35Z` | `2026-10-25T00:00:00Z` | `["grain-ci"]` | [`120-hubnode-consume-grainfile-v1.task.grain.md`](./120-hubnode-consume-grainfile-v1.task.grain.md) | `[HOLD for grain refactor] pkg/hubnode, pkg/orchestrator: consume Grainfile v1 (.task.grain.md, .process.grain.md, and .jinja2 templates) over Hub rendezvous` |
| `#121` | `deferred` | `bwsalmon/grain` | `2026-10-18T23:32:36Z` | `2026-10-25T00:00:00Z` | `["grain-ci"]` | [`121-prewarmed-sandbox-pool-base-branch-cache.task.grain.md`](./121-prewarmed-sandbox-pool-base-branch-cache.task.grain.md) | `[HOLD for grain refactor] pkg/orchestrator, pkg/mcp: add pre-warmed sandbox pool and per-(repo, base_SHA) base_branch_status caching` |
| `#122` | `deferred` | `bwsalmon/grain` | `2026-10-18T23:32:37Z` | `2026-10-25T00:00:00Z` | `["grain-ci"]` | [`122-sandbox-timeout-capability-prompt-guidance.task.grain.md`](./122-sandbox-timeout-capability-prompt-guidance.task.grain.md) | `[HOLD for grain refactor] pkg/agent, pkg/capability: add sandbox_T (2h/4h) deadline capability and prompt guidance for wait_for_checks and read_file` |
| `#149` | `deferred` | `bwsalmon/grain` | `2026-10-19T01:17:58Z` | `2026-10-25T00:00:00Z` | `["grain-ci"]` | [`149-hub-shared-chat-gchat-rooms.task.grain.md`](./149-hub-shared-chat-gchat-rooms.task.grain.md) | `[HOLD for grain refactor] pkg/hubnode, pkg/agent: support multi-user Hub shared chat sessions and Google Chat-linked rooms` |
| `#150` | `deferred` | `bwsalmon/grain` | `2026-10-19T01:32:22Z` | `2026-10-25T00:00:00Z` | `["grain-ci"]` | [`150-merge-silo-into-grain-monorepo.task.grain.md`](./150-merge-silo-into-grain-monorepo.task.grain.md) | `[HOLD for grain refactor] repo: merge bwsalmon/silo into bwsalmon/grain as a single Go module preserving git history` |
| `#158` | `deferred, closed` (closed `2026-10-19`) | `bwsalmon/grain` | `2026-10-19T02:41:00Z` | `null` | `[]` | [`158-ui-merged-task-color.task.grain.md`](./158-ui-merged-task-color.task.grain.md) | `new color for merged tassk` |
| `#239` | `deferred, closed` (closed `2026-10-20T04:54:28Z`) | `bwsalmon/grain` | `2026-10-20T01:59:59Z` | `null` | `[]` | [`239-standardize-timestamps-rfc3339.task.grain.md`](./239-standardize-timestamps-rfc3339.task.grain.md) | `Standardize all timestamps across Grain to RFC 3339` |

## Files in This Directory

- **`deferred-tasks.json`:** Machine-readable JSON array of all 10 deferred task records (`id`, `state`, `repo`, `title`, `body`, `capabilities`, `deferred_at`, `deferred_until`, plus `supersedes`, `closed_at`, and `grainfile` metadata) for bulk re-import or `propose_task` automation after reformatting the deployment.
- **`*.task.grain.md`:** Individual Grainfile `v1` task definitions with `draft: true` so they are not auto-dispatched until ready. To activate any task via Grainfile subscription on the Hub, set `draft: false` (or remove the `draft` field).

## Validation

Validate all Grainfile `v1` files in this directory using `grainlint.py`:

```bash
python3 tools/grainfiles/grainlint.py deferred-tasks
```
