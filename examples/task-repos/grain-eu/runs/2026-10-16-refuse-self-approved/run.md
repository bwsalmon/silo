---
kind: run
schema: grain/run@v1
task: {ref: "intake:tasks/2026-10-16-refuse-self-approved/task.md", rev: 9b3d5f7a1c2e4a6c8e0b2d4f6a8c1e3b5d7f9a03}
from: {ref: /backlog/2026-10-16-refuse-self-approved.md, rev: a1b2c3d4e5f60718293a4b5c6d7e8f9012345603}
grain: eu
state: superseded
accepted_at: "2026-10-16T15:21:30Z"
started_at: "2026-10-16T15:22:00Z"
superseded:
  at: "2026-10-16T16:46:00Z"
  by: silo
  reason: Grain eu was lost at 15:40 while running this; the hub team's 60-minute grace ran out.
---

Grain `eu` was running this when it was lost. Silo removed the backlog
reference and wrote `superseded` in one commit (docs/task-repos.md §8.8).
The record and its thread stay here. The next attempt is
`us:runs/2026-10-16-refuse-self-approved/run.md`.
