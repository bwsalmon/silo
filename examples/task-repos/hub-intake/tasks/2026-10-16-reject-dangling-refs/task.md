---
kind: task
schema: grain/task@v1
title: Refuse backlog references whose target does not resolve
team: hub
repo: https://github.com/bwsalmon/silo.git
base: main
state: approved
submitter: "user:bsalmon@google.com"
submitted_via: ui
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T17:28:00Z"
created_at: "2026-10-16T17:24:10Z"
---

An illustrative task, not a real one. In the pre-receive hook, refuse a
new or changed `kind: ref` whose `to:` does not resolve, following
tombstones and repo redirects with the pusher's read rights, to a
`kind: task`. Refuse a second reference to the same task in one backlog
(docs/task-repos.md §5.2, §8.1).
