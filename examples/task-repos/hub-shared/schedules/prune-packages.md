---
kind: schedule
schema: grain/schedule@v1
aka: ["repo:schedule:bwsalmon:grain:prune-packages"]
title: Prune GHCR packages
template: ../templates/ghcr/prune-packages.md
inputs: {}
weekly: {day: sunday, at: "04:00"}
overlap: skip
timezone: ""
enabled: true
base: candidate
team: hub
repo: https://github.com/bwsalmon/grain.git
fires_in: silo
auto_approve_proposals: true
auto_merge: false
created_at: "2026-09-26T04:43:16.568716Z"
---

This is exported from grain/config `settings/schedules.yml`
(`repo:schedule:bwsalmon:grain:prune-packages`), which was itself declared
in bwsalmon/grain `.grain/schedules/prune-packages.yml`.

The export drops `next-run-at` and `last-run-at`. They are runtime state,
and the scheduler recomputes them from the cadence. Each firing records
`schedule.fired_at` on the task it files, as
`hub-intake/tasks/scheduled/prune-packages/20261011T0400Z/task.md` does.
The template moved to `templates/ghcr/` on 2026-10-16, so the schedule
names its new path. The old path is a tombstone (docs/task-repos.md §2.1).
