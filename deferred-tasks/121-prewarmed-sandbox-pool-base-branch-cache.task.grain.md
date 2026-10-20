---
grainfile: v1
title: "[HOLD for grain refactor] pkg/orchestrator, pkg/mcp: add pre-warmed sandbox pool and per-(repo, base_SHA) base_branch_status caching"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2026-10-25T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 121
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-18T23:32:36Z"
x_deferred_until: "2026-10-25T00:00:00Z"
x_capabilities:
  - grain-ci
---
**HOLD UNTIL `bwsalmon/grain` REFACTORING LANDS.**

Implement the two highest-leverage infrastructure throughput and latency optimizations identified from this deployment's `grain metrics` measurements (`pkg/orchestrator`, `pkg/mcp`):

### 1. Pre-Warmed Sandbox Pool (`pkg/orchestrator`)
- **Problem:** Cold sandbox VM creation and boot currently averages ~52s (`p50 38s`, `p90 1m09s`) before the first agent turn can start.
- **Solution:** Maintain a configurable pool of `1–2` pre-warmed idle sandboxes (`max_idle_pool`, default `1`) with the base VM image already booted and ready. When a task is dispatched, claim a pre-warmed sandbox from the pool, clone the target repository/branch, and replenish the idle pool in the background, cutting dispatch-to-first-turn latency from ~52s to `<5s`.

### 2. Per-`(repo, base_branch, head_sha)` Caching for `base_branch_status` (`pkg/mcp`, `pkg/github`)
- **Problem:** `base_branch_status` currently averages `5.8s` per call (`max 55.8s`) because each call queries GitHub check-runs and tail logs for the base branch from scratch.
- **Solution:** Cache the result of `base_branch_status` in the controller keyed by `(repo, base_branch, head_sha)` with a short TTL (`60–120s`, invalidated when a new commit lands on the base branch) so concurrent runs checking the same base branch SHA receive an instant response without redundant GitHub API calls.

### Verification
- Add unit tests for the pre-warmed sandbox pool lifecycle (claim, replenish, shutdown cleanup) and `base_branch_status` cache hit/invalidation, and run `go test -race ./pkg/...`.
