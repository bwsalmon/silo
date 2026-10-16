---
kind: schedule
schema: grain/schedule@v1
id: sch-7tpkw2psymx2
aka: ["repo:schedule:bwsalmon:grain:prune-packages"]
title: Prune GHCR packages
template: {ref: ../templates/prune-packages.md, id: tmpl-7txvezb4bfz9}
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
`grain-eu/tasks/task-m0jpyjbjz6p3-prune-ghcr/task.md` does.
