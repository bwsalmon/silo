---
kind: trigger
schema: grain/trigger@v1
aka: ["repo:schedule:bwsalmon:grain:prune-packages"]
title: Prune GHCR packages
"on":
  schedule:
    weekly: {day: sunday, at: "04:00"}
    timezone: ""
    overlap: skip
    catch_up: {mode: latest-only}
  manual: {}
fires:
  template: ../templates/ghcr/prune-packages.template.md
  inputs: {repo: "https://github.com/bwsalmon/grain.git@candidate"}
enabled: true
team: hub
fires_in: silo
auto_approve_proposals: true
auto_merge: false
created_at: "2026-09-26T04:43:16.568716Z"
---

A trigger with two sources: the weekly cadence, and `manual:`, so a hub
approver (the default `who`) can run it now from Silo's UI. Either
source files the same templated task.

This is exported from grain/config `settings/schedules.yml`
(`repo:schedule:bwsalmon:grain:prune-packages`), which was itself declared
in bwsalmon/grain `.grain/schedules/prune-packages.yml`. The export maps
the schedule's keys onto `on.schedule` and `fires` (docs/task-repos.md
§13): the cadence, `timezone` and `overlap` go under `on.schedule`;
`template` becomes `fires.template`, a reference; and grain's `target`
and `base` become the template's `repo` input. The old row had no
catch-up setting, and grain fires only the latest missed run after
downtime, so the export writes `catch_up: {mode: latest-only}`.

The export drops `next-run-at` and `last-run-at`. They are runtime state,
and the scheduler recomputes them from the cadence; so is whether the
trigger is paused. Each firing records `trigger: {ref, on, key,
fired_at}` on the task it files, as
`hub-intake/tasks/fired/prune-packages/20261011T0400Z.task.md` does.
The template moved to `templates/ghcr/` on 2026-10-16, so the trigger
names its new path. The old path is a tombstone (docs/task-repos.md §2.1).
