---
kind: task
schema: grain/task@v1
title: Tidy the run notes in memory/
state: active
submitter: user:bsalmon@google.com
created_at: "2026-10-16T09:00:00Z"
approved_by: deployment:eu
approved_at: "2026-10-16T09:05:00Z"
context:
  - {ref: "https://github.com/acme/grain-eu.git@main", mode: write}
---
Merge duplicate notes in `memory/` and keep each under 40 lines.
