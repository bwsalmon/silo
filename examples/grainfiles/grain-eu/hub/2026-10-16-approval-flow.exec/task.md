---
kind: task
schema: grain/task@v1
title: Refuse self-approved tasks at intake
submitter: user:dev@example.com
created_at: "2026-10-16T09:00:00Z"
team: hub
context:
  - {ref: "https://github.com/acme/hub.git@main", mode: write}
---
Refuse an execution whose `approved_by` equals its definition's `submitter`.
