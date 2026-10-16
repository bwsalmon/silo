---
kind: schedule
schema: grain/schedule@v1
title: Weekly granule pull
derived_from: "shared:schedules/granule-pull.md"
template: "shared:templates/granule-pull.md"
inputs: {repo: "https://github.com/bwsalmon/grain.git@candidate"}
weekly: {day: monday, at: "06:00"}
overlap: skip
enabled: true
team: hub
fires_in: grain
auto_approve_proposals: false
created_at: "2026-10-16T17:30:00Z"
---

A customised copy of `shared:schedules/granule-pull.md`, which
`no-granule-pull.md` excludes from this Grain's namespace. It is a whole,
self-contained schedule: `derived_from` records where it came from, for
people and for Silo's views, and inherits nothing (docs/task-repos.md
§6.2). When the team changes the original, this copy does not change;
Silo's health view shows the two diverging.
