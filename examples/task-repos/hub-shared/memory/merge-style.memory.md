---
kind: memory
schema: grain/memory@v1
title: Merge style
updated_by: "user:maintainer@example.com"
updated_at: "2026-10-12T10:00:00Z"
---

Prefer merge commits, so that a change's own commits stay in history.
Any Grain that names this folder as a memory root gets this; a Grain's
own memory listed ahead of it takes precedence where they conflict
(docs/task-repos.md §3.3).
