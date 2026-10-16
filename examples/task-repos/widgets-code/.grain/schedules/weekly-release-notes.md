---
kind: schedule
schema: grain/schedule@v1
title: Weekly release notes
template: ../templates/release-notes.md
inputs: {since: latest-tag}
weekly: {day: friday, at: "16:00"}
overlap: skip
enabled: true
base: main
team: hub
repo: https://git.example.com/hub/widgets.git
fires_in: silo
auto_approve_proposals: false
created_at: "2026-10-16T18:05:00Z"
---

The definition lives here, beside the code. Its firings do not: Silo
files each one into the hub intake, at
`tasks/scheduled/widgets/weekly-release-notes/<fired_at>/task.md`, because
the hub intake's README adopts this schedule. A schedule that a team has
not adopted never fires for it (docs/task-repos.md §6.2, §11.13).

`auto_approve_proposals: false`, so each firing is filed `proposed` and
waits for a hub approver.
