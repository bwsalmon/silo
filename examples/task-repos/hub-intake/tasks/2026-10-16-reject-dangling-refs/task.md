---
kind: task
schema: grain/task@v1
title: Refuse backlog references whose target does not resolve
team: hub
context:
  - ref: "https://github.com/bwsalmon/silo.git@main"
    mode: write
  - ref: "https://github.com/bwsalmon/grain.git//pkg/daemon/@candidate"
    mode: read
  - ref: "shared:designs/hub-intake.md"
    depth: 1
  - ref: attachments/dangling-ref-error.png
    path: context/screenshot.png
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

The screenshot, `context/screenshot.png` in the sandbox, shows the
refusal the hook should print. It was attached to the task when it was
filed, and is stored beside it at `attachments/dangling-ref-error.png`
(§2.2). Its `context:` is walked through in §8.16.
