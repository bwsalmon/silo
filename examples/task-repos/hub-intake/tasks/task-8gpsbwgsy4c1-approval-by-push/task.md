---
kind: task
schema: grain/task@v1
id: task-8gpsbwgsy4c1
title: Add HS-6 to the hub intake design (approval is a push to the intake repo)
team: hub
repo: https://silo.example.com/git/hub/shared.git
state: proposed
submitter: "user:alice@example.com"
submitted_via: git
created_at: "2026-10-16T17:05:00Z"
---

Proposed by `git push` to this repo (docs/task-repos.md §8.2).

Write requirement HS-6 into `shared:designs/hub-intake.md`: a task is
approved by a push that sets `state: approved` and
`approved_by: user:<pusher>`, which Silo's pre-receive hook accepts only
from an approver for the owning team other than the submitter. Do it on
a `review/` branch, because merging it starts planning (§7.4).
