---
kind: task
schema: grain/task@v1
id: task-4redj39vwnz4
title: Carry a hub task's approver on its offer and attestation
team: hub
repo: https://github.com/bwsalmon/silo.git
state: placed
submitter: "user:bsalmon@google.com"
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T13:55:12Z"
created_at: "2026-10-16T13:41:07Z"
placement:
  grain: eu
  ref: "eu:tasks/task-4redj39vwnz4-carry-approver/task.md"
  generation: 1
  at: "2026-10-16T13:58:00Z"
---

The prompt as proposed and approved stays here unchanged. It is abridged
in this example, because it is byte-identical to the body of the Grain's
copy at `placement.ref`. Silo's placement commit changed only `state` and
`placement`. After placement the Grain's copy is authoritative for state,
thread and outcome (docs/task-repos.md §8.4).
