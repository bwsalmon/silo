---
kind: include
schema: grain/include@v1
target: "widgets:.grain/schedules/weekly-release-notes.md@main"
ref_mode: track
kinds: [schedule]
added_by: "user:admin@example.com"
added_at: "2026-10-16T18:10:00Z"
---

The team adopts the widgets repo's weekly release-notes schedule. It
names a single file, so nothing else in that repo's grain folder reaches
the team. Silo fires it for the hub team and files each firing here
(docs/task-repos.md §6.2). The include tracks `main`, so every firing is
filed `proposed`: an approver reads what the repo's latest schedule and
template rendered. To have firings filed `approved`, the admins would pin
it to a commit they reviewed (`@<sha>`, `ref_mode: pin`), and re-pin to
take a change.
