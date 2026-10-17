---
kind: task
schema: grain/task@v1
title: Tidy the run notes in memory/
submitter: user:bsalmon@google.com
created_at: "2026-10-16T09:00:00Z"
context:
  - {ref: "https://github.com/acme/grain-eu.git@main", mode: write}
---
Merge duplicate notes in `memory/` and keep each under 40 lines.
