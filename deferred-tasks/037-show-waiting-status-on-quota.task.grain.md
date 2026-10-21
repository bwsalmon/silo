---
grainfile: v1
title: "Show waiting status on tasks held back by sandbox or provider quota"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2099-01-01T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/corp-research
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 37
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T23:19:52Z"
x_deferred_until: "2099-01-01T00:00:00Z"
x_capabilities:
  - corp-research
  - grain-ci
---
## Problem

When a queued task cannot start because of a model connection (`ModelWaitCapability = "model"`), `grantAdmitter` (`pkg/orchestrator/cycle.go:1070`) records a `model.GrantWait` via `Store.SetGrantWait`, and `grantWaitActivity` (`pkg/ui/tasks.go:432`) surfaces `"waiting: <reason>"` on the task row in `TaskList`, in `DetailOverlay`, and in `read_grain_task`.

However, when a queued task (including one an operator explicitly clicked **Run now** on) is waiting on **quota / capacity**—either:
1. **Host sandbox resource quota / worker capacity** (`!limits.Admits(live, merger) || !limits.SandboxAdmits()` in `pkg/dispatch/dispatch.go:484, 535, 592`), or
2. **Provider usage-limit / quota pause** (`PausedFor` in `pkg/dispatch/dispatch.go:349` / `pausedByUsageLimit` in `pkg/orchestrator/cycle.go`),

`dispatch.Cycle` skips the candidate before `grantAdmitter` is ever reached (and at line 535 `if !canWorker && !canMerger && len(directorIDs) == 0 { return out, nil }` exits before inspecting the ready queue at all). As a result, no `GrantWait` is recorded and the task shows no `"waiting: ..."` status in the UI or CLI, leaving the operator with no visibility into why the task (or a **Run now** request) has not started.

## Proposed Changes

1. **Add a quota/capacity wait capability in `pkg/model/model_selection.go` and `pkg/ui/tasks.go`**:
   - Define `QuotaWaitCapability` (e.g. `const QuotaWaitCapability = "quota"` or `"capacity"`) alongside `ModelWaitCapability`, `TeamSetupWaitCapability`, `BranchWaitCapability`, and `PolicyWaitCapability` in `pkg/model/model_selection.go`.
   - Include `QuotaWaitCapability` in `grantWaitActivity` (`pkg/ui/tasks.go:433`) so a `GrantWait` with `Capability: model.QuotaWaitCapability` renders `"waiting: " + wait.Resource` with `BySetup: true` just like `ModelWaitCapability`.

2. **Record and clear quota `GrantWait` in `pkg/dispatch/dispatch.go` / `pkg/orchestrator/cycle.go`**:
   - When a `RunNow` task (`requested` loop in `pkg/dispatch/dispatch.go:479`) or a ready candidate (`allCandidates` in `pkg/dispatch/dispatch.go`) cannot start due to sandbox quota or worker/merger limits (`!limits.Admits(live, merger) || !limits.SandboxAdmits()`):
     - Do not return early at line 535 before recording wait reasons on eligible ready candidates (or record the wait via `SetGrantWait` using `capacityReason(limits, live, merger)` / `limits.SandboxVerdict().Reason`).
   - Similarly, when a task is skipped by `PausedFor` because its model connection's credential is paused on a provider usage/quota limit (`pausedByUsageLimit` in `pkg/orchestrator/cycle.go`), record a `GrantWait` with the usage-limit pause reason (which connection/credential is paused and when the quota window resets).
   - Ensure `clearAdmissionWait` / `StartRun` clears the `QuotaWaitCapability` wait as soon as the task is admitted or starts a run, and avoid unnecessary write churn on `task_grant_wait` across ticks when the wait reason has not changed.

3. **Tests**:
   - Add unit tests in `pkg/dispatch/`, `pkg/orchestrator/`, and `pkg/ui/` verifying that:
     - A queued task (and a `RunNow` task) held back by sandbox CPU/memory/disk budget (`limits.SandboxVerdict()`) or worker/merger quota displays `"waiting: ..."` with the quota reason in `ListTasks` and `GetTask`.
     - A queued task held back by a provider usage-limit quota pause (`PausedFor`) displays `"waiting: ..."` with the quota pause reason.
     - Starting the task clears the wait status.
