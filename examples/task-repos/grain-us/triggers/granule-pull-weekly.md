---
kind: trigger
schema: grain/trigger@v1
title: Weekly granule pull
derived_from: "shared:triggers/granule-pull.md"
"on":
  schedule:
    weekly: {day: monday, at: "06:00"}
    overlap: skip
    catch_up: {mode: latest-only}
fires:
  template: "shared:templates/granule-pull.md"
  inputs: {repo: "https://github.com/bwsalmon/grain.git@candidate"}
enabled: true
team: hub
fires_in: grain
auto_approve_proposals: false
created_at: "2026-10-16T17:30:00Z"
---

A customised copy of `shared:triggers/granule-pull.md`, which
`no-granule-pull.md` excludes from this Grain's namespace. It is a whole,
self-contained trigger that references the same template: `derived_from`
records where it came from, for people and for Silo's views, and
inherits nothing (docs/task-repos.md §6.2). When the team changes the
original, this copy does not change; Silo's health view shows the two
diverging.

Weekly, a missed Monday matters more than a missed morning, so this copy
catches up the latest missed firing after downtime, where the team's
daily original does not.
