---
kind: run
schema: grain/run@v1
aka: ["grain:eu/task/4310"]
task: {ref: "intake:tasks/4310-carry-approver/task.md", rev: 7a1c3e5b9d2f4a6c8e0b1d3f5a7c9e2b4d6f8a01}
from: {ref: /backlog/4310-carry-approver.md, rev: a1b2c3d4e5f60718293a4b5c6d7e8f9012345601}
grain: eu
state: done
accepted_at: "2026-10-16T13:58:30Z"
started_at: "2026-10-16T13:58:31Z"
closed:
  outcome: done
  at: "2026-10-16T14:58:30Z"
  summary: >-
    tasks.Spec gains approved_by, stored in a new approved_by column, offered
    with the rest of the spec and signed as the attestation's approved_by
    claim. A spec whose approver is its own submitter is refused, and the
    submit API does not accept the field. The approval flow itself remains
    open, and is proposed as a follow-up.
  change:
    url: https://github.com/bwsalmon/silo/commit/5623aa2544627ce1b119ca5b5d2eb1f2f490c123
    branch: grain/eu/task-4310
    merge_commit: 5623aa2544627ce1b119ca5b5d2eb1f2f490c123
  design_revs:
    - item: "https://silo.example.com/git/hub/shared.git//designs/hub-intake.md#HS-2"
      rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
      hash: "sha256:54a64d8a703a5e4ca442f5008a56871fbeccf84d5ba01e729b0e2a2ed2aca330"
  follow_ups:
    - "intake:tasks/2026-10-16-approval-flow/task.md"
---

Grain `eu`'s execution record of `intake:tasks/4310-carry-approver/task.md`.
Its backlog reference, `backlog/4310-carry-approver.md`, was removed by
Silo when this record closed. `from.rev` is the commit where it still
exists. The thread is in `comments/`.
