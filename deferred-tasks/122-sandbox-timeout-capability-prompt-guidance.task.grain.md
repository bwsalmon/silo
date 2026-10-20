---
grainfile: v1
title: "[HOLD for grain refactor] pkg/agent, pkg/capability: add sandbox_T (2h/4h) deadline capability and prompt guidance for wait_for_checks and read_file"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2026-10-25T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 122
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-18T23:32:37Z"
x_deferred_until: "2026-10-25T00:00:00Z"
x_capabilities:
  - grain-ci
---
**HOLD UNTIL `bwsalmon/grain` REFACTORING LANDS.**

Implement the agent prompt efficiency improvements and the configurable extended sandbox deadline capability (`sandbox_T`) in `bwsalmon/grain` (`pkg/agent`, `pkg/capability`, `pkg/orchestrator`):

### 1. Extended Run Deadline Capability (`sandbox_T`: `2h` / `4h`) (`pkg/capability`, `pkg/orchestrator`)
- Add a parameterized or tiered run-timeout capability (e.g. `sandbox-2h` and `sandbox-4h`, or `sandbox_timeout`) that tasks can request via `capabilities` in `propose_task` / UI task creation when running long E2E, `kind` cluster, or fault-injection test suites that exceed the default run deadline.

### 2. Base Agent Prompt Guidelines for `wait_for_checks` and `read_file` Chunking (`pkg/agent`)
- Update the base system prompt assembled for sandbox runs based on `grain metrics` findings:
  1. **Use `wait_for_checks` instead of polling `pull_request_status`:** Instruct agents to call `wait_for_checks` once after `git push` (which blocks until CI finishes or fails and credits wait time back to the run budget) rather than calling `pull_request_status` repeatedly in a polling loop.
  2. **Targeted `read_file(offset, limit)` windows:** Instruct agents to pass `offset` and `limit` (e.g. 200–300 lines) when reading large source files so tool outputs stay under the 16 KB truncation cap (`12.2%` of `read_file` calls were previously truncated).
  3. **Verify target paths before `edit_file`:** Remind agents to read the exact target lines immediately before `edit_file` to avoid stale-context mismatches (`13.0%` error rate on `edit_file`).

### Verification
- Add unit tests for the `sandbox_T` deadline override and prompt assembly in `pkg/capability`, `pkg/orchestrator`, and `pkg/agent`, and run `go test -race ./pkg/...`.
