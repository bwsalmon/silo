---
kind: task
schema: grain/task@v1
id: task-wx32yhzasd1r
title: Interop test that a Grain refuses an offer approved by its own submitter
team: hub
repo: https://github.com/bwsalmon/silo.git
state: placed
submitter: "user:bsalmon@google.com"
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T15:20:00Z"
created_at: "2026-10-16T15:02:44Z"
placement:
  grain: us
  ref: "us:tasks/task-wx32yhzasd1r-refuse-self-approved/task.md"
  generation: 2
  at: "2026-10-16T16:46:00Z"
---

The prompt as proposed and approved stays here unchanged. It is abridged
in this example, because it is byte-identical to the body of the Grain's
copy at `placement.ref`. Silo's placement commit changed only `state` and
`placement`. After placement the Grain's copy is authoritative for state,
thread and outcome (docs/task-repos.md §8.4).
