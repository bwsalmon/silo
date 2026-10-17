---
kind: task
schema: grain/task@v1
title: Refuse self-approved tasks at intake
state: sec-approved
submitter: user:dev@example.com
created_at: "2026-10-16T09:00:00Z"
approved_by: user:sec@example.com
approved_at: "2026-10-16T11:00:00Z"
context:
  - {ref: "https://github.com/acme/hub.git@main", mode: write}
---
Refuse a task whose `approved_by` equals its `submitter`.
