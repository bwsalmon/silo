---
kind: task
schema: grain/task@v1
id: task-s23ve0hq82dn
title: Sign task offers with the claims grain's local policy verifies
team: hub
repo: https://github.com/bwsalmon/silo.git
state: placed
submitter: "user:bsalmon@google.com"
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T09:12:40Z"
created_at: "2026-10-16T08:50:02Z"
placement:
  grain: eu
  ref: "eu:tasks/task-s23ve0hq82dn-sign-offers/task.md"
  generation: 1
  at: "2026-10-16T09:13:00Z"
---

The prompt as proposed and approved stays here unchanged. It is abridged
in this example, because it is byte-identical to the body of the Grain's
copy at `placement.ref`. Silo's placement commit changed only `state` and
`placement`. After placement the Grain's copy is authoritative for state,
thread and outcome (docs/task-repos.md §8.4).
