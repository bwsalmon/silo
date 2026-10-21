# Deferred Tasks Dump (`bsalmon` Deployment)

This directory preserves all tasks that were in the `deferred` state on the `bsalmon` (`796ba46d`) Grain deployment prior to restarting/reformatting, so they are stored in Git and can be re-imported or re-proposed after the deployment is rebuilt.

## Summary of Deferred Tasks

| Original ID | State at Dump Time | Target Repo | `deferred_at` | `deferred_until` (`not_before`) | Capabilities | Grainfile (`.task.grain.md`) | Title |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `#31` | `deferred` | `bwsalmon/grain` | `2026-10-20T23:10:09Z` | `2099-01-01T00:00:00Z` | `["corp-research", "grain-ci"]` | [`031-stream-live-go-test-progress.task.grain.md`](./031-stream-live-go-test-progress.task.grain.md) | `Stream live progress output during Go test runs in Grain CI workflows` |
| `#32` | `deferred` | `bwsalmon/grain` | `2026-10-20T23:10:10Z` | `2099-01-01T00:00:00Z` | `["corp-research", "grain-ci"]` | [`032-collapse-task-provenance-work-tree.task.grain.md`](./032-collapse-task-provenance-work-tree.task.grain.md) | `Collapse task provenance and work tree by default in the task detail pane` |
| `#33` | `deferred` | `bwsalmon/grain` | `2026-10-20T23:10:11Z` | `2099-01-01T00:00:00Z` | `["corp-research", "grain-ci"]` | [`033-display-pending-question-up-front.task.grain.md`](./033-display-pending-question-up-front.task.grain.md) | `Display the pending question text and reply box up front on tasks waiting for user input` |
| `#34` | `deferred` | `bwsalmon/grain` | `2026-10-20T23:10:13Z` | `2099-01-01T00:00:00Z` | `["corp-research", "grain-ci"]` | [`034-console-session-hide-task-fix-lifecycle.task.grain.md`](./034-console-session-hide-task-fix-lifecycle.task.grain.md) | `Hide underlying task for console sessions and fix session restart, cancel, and delete` |
| `#35` | `deferred` | `bwsalmon/grain` | `2026-10-20T23:14:46Z` | `2099-01-01T00:00:00Z` | `["corp-research", "grain-ci"]` | [`035-prevent-frequent-session-handoffs.task.grain.md`](./035-prevent-frequent-session-handoffs.task.grain.md) | `Prevent frequent handoffs and short session lengths after a session summarizes and hands off` |
| `#36` | `deferred` | `bwsalmon/grain` | `2026-10-20T23:14:47Z` | `2099-01-01T00:00:00Z` | `["corp-research", "grain-ci"]` | [`036-iteratively-remove-dead-code.task.grain.md`](./036-iteratively-remove-dead-code.task.grain.md) | `Iteratively find and remove dead code until no more is found` |
| `#37` | `deferred` | `bwsalmon/grain` | `2026-10-20T23:19:52Z` | `2099-01-01T00:00:00Z` | `["corp-research", "grain-ci"]` | [`037-show-waiting-status-on-quota.task.grain.md`](./037-show-waiting-status-on-quota.task.grain.md) | `Show waiting status on tasks held back by sandbox or provider quota` |
| `#38` | `deferred` | `bwsalmon/grain` | `2026-10-20T23:21:35Z` | `2099-01-01T00:00:00Z` | `["corp-research", "grain-ci"]` | [`038-agent-sandboxes-precedence-over-ci.task.grain.md`](./038-agent-sandboxes-precedence-over-ci.task.grain.md) | `Give agent sandboxes precedence over CI jobs in the shared sandbox budget` |

## Files in This Directory

- **`deferred-tasks.json`:** Machine-readable JSON array of all 8 deferred task records (`id`, `state`, `repo`, `title`, `body`, `capabilities`, `deferred_at`, `deferred_until`, and `grainfile` metadata) for bulk re-import or `propose_task` automation after reformatting the deployment.
- **`*.task.grain.md`:** Individual Grainfile `v1` task definitions with `draft: true` so they are not auto-dispatched until ready. To activate any task via Grainfile subscription on the Hub, set `draft: false` (or remove the `draft` field).

## Validation

Validate all Grainfile `v1` files in this directory using `grainlint.py`:

```bash
python3 tools/grainfiles/grainlint.py deferred-tasks
```
