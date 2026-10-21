---
grainfile: v1
title: "Give agent sandboxes precedence over CI jobs in the shared sandbox budget"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2099-01-01T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/corp-research
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 38
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T23:21:35Z"
x_deferred_until: "2099-01-01T00:00:00Z"
x_capabilities:
  - corp-research
  - grain-ci
---
## Problem

Agent runs and CI jobs share the same host sandbox resource budget (`model.Budget` / `model.Ledger`), but in practice queued CI jobs completely starve queued agent sandboxes whenever CI is busy:

1. **Event-driven CI admission vs. periodic `dispatch.Cycle` polling**:
   - In `pkg/ci/gate.go` (`SharedGate.Acquire` / `releaser`), every waiting CI job goroutine is subscribed to `g.wake`. The instant any CI sandbox finishes and releases 2–16 vCPUs, `releaser` closes `g.wake` and another waiting CI job immediately claims the freed capacity in `g.take`.
   - Meanwhile, `dispatch.Cycle` (`pkg/dispatch/dispatch.go`) only checks `limits.SandboxAdmits()` on reconcile ticks (every 10s), does not wake when a CI sandbox releases capacity, and does not register any reservation on `model.Ledger` or `SharedGate` when an eligible agent task is refused for want of sandbox capacity.
2. **Large agent sandboxes (`AgentShape`, e.g. 16 vCPU) never get a hole while small CI jobs (2–4 vCPU) are queued**:
   - Inside `SharedGate`, `aheadOf` and `reserving` (`ReserveAfter`) hold released slivers back so a larger higher-priority CI job can accumulate a hole.
   - However, `SharedGate` only ranks `g.waiters` (CI jobs) and knows nothing about waiting agent runs. Even if an operator clicks **Run now** on a queued task, 2-vCPU and 4-vCPU CI jobs continually consume every freed sliver before `AgentShape` (e.g., 16 vCPU) can ever fit.

## Proposed Changes

1. **Give waiting agent sandboxes precedence over queued CI jobs in the shared sandbox budget (`pkg/model/limits.go`, `pkg/ci/gate.go`, `pkg/dispatch/dispatch.go`, `pkg/orchestrator/cycle.go`)**:
   - Track when `dispatch.Cycle` has one or more otherwise-ready, admissible agent tasks (including `RunNow` requests) that are held back solely by the sandbox resource budget (`!limits.SandboxAdmits()`), or allow `dispatch` / `orchestrator` to register its pending agent sandbox demand (`AgentShape`) on `model.Ledger` (or via a callback on `SharedGate`).
   - In `SharedGate.take` (`pkg/ci/gate.go`), when an agent sandbox is waiting for room in the shared budget, hold back new CI job admissions (`WaitForPriority` / `"waiting behind a queued agent sandbox"`) so released capacity accumulates until the waiting agent run(s) fit and are dispatched.
   - **Deadlock safety (`Stuck` preservation)**: Preserve the existing deadlock escape hatch (`SharedGate.Stuck` / `Sole` rule in `pkg/ci/gate.go:614`): if all currently live sandboxes are agent runs that are themselves blocked in `wait_for_checks` waiting on CI, and the remaining free budget cannot fit the waiting agent sandbox until a live agent run finishes its CI checks, do **not** block the CI jobs that those live agent runs are waiting on.
2. **Wake the dispatcher when CI releases sandbox capacity**:
   - When a CI sandbox is released while an agent task is waiting on sandbox budget, trigger `Wake()` on the reconcile loop so `dispatch.Cycle` immediately claims the freed capacity rather than waiting up to 10s for the next poll tick.
3. **Tests**:
   - Add unit tests in `pkg/ci/gate_test.go` and `pkg/dispatch/` / `pkg/orchestrator/` verifying that:
     - When a 16-vCPU agent task is ready and waiting on sandbox budget alongside queued 4-vCPU CI jobs, releasing 4-vCPU CI sandboxes holds capacity for the agent sandbox and wakes dispatch rather than letting queued CI jobs consume the slivers.
     - Once no agent tasks are waiting on sandbox budget (or once the waiting agent tasks are admitted), queued CI jobs resume admission normally.
     - If live agent runs holding the budget are waiting on a CI job (`Stuck`), that CI job is still allowed through so the deployment cannot deadlock.
