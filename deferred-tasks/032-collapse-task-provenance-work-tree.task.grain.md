---
grainfile: v1
title: "Collapse task provenance and work tree by default in the task detail pane"
requestor: Bruce Salmon <bsalmon@google.com>
recipient: https://github.com/bwsalmon/grain
draft: true
not_before: "2099-01-01T00:00:00Z"
capabilities:
  - https://github.com/bwsalmon/grain/capabilities/corp-research
  - https://github.com/bwsalmon/grain/capabilities/grain-ci
x_original_task_id: 32
x_state_at_dump: deferred
x_target_repo: bwsalmon/grain
x_deferred_at: "2026-10-20T23:10:10Z"
x_deferred_until: "2099-01-01T00:00:00Z"
x_capabilities:
  - corp-research
  - grain-ci
---
## Problem

In the task detail pane (`ui/src/components/DetailOverlay.jsx`), task provenance is currently expanded inline near the top of the task:
- The `.freshness` header bar (`DetailOverlay.jsx:290–416`) renders inline provenance links (`generated from`, `filedBySuiteRun`, `origin`, `securityReviewTask`, `supersedes`, `supersededBy`, `reverts`, `revertedBy`, `portOf`, `portedBy`, `not ported`, `Google Chat space`).
- Below the description (`DetailOverlay.jsx:462`), `<WorkTree t={t} tasks={tasks} onOpenTask={onOpenTask} />` (`ui/src/components/detail/WorkTree.jsx`) renders the full tree of tasks grown out of the same origin (`Work from task 1: ...` and every sibling/child task row) expanded by default. For tasks filed from a long-running console session like Director, this work tree can contain dozens of tasks and pushes the actual task status, CI checks, and timeline far down the page.

## Changes Required

1. **Collapse task provenance and work tree by default (`ui/src/components/DetailOverlay.jsx`, `ui/src/components/detail/WorkTree.jsx`)**:
   - Group a task's provenance details (origin, `generatedFrom`, `filedBySuiteRun`, `supersedes`/`supersededBy`, `reverts`/`revertedBy`, `portOf`/`portedBy`, `noPort`, `googleChatSpace`, and the `<WorkTree />` when present) into a collapsible provenance disclosure that **starts collapsed by default** and can be expanded by the user on click.
   - Keep primary non-provenance items in `.freshness` (such as freshness age and the task's own PR link `t.pullRequest` / `Internal PR`) directly visible, while collapsing the provenance trail and work tree under a compact summary/toggle (e.g., `Provenance` / `Work from ... (done X of Y)` summary line that expands to show the full provenance links and `<TaskTree />` on demand).
   - Reset the expanded state when switching between tasks (`t.id`).

2. **Tests (`ui/src/components/DetailOverlay.test.jsx`, `ui/src/components/detail/WorkTree.test.jsx`)**:
   - Update existing provenance and `WorkTree` tests and add tests verifying that provenance starts collapsed by default, can be expanded by the user to inspect provenance links and the work tree, and renders nothing when a task has no provenance metadata.
