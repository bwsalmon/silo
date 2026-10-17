---
kind: schedule
schema: grain/schedule@v1
aka: ["sched-1"]
title: Nightly granule pull
template: ../templates/granule-pull.md
inputs: {repo: "https://github.com/bwsalmon/grain.git@candidate"}
daily: {at: "06:00"}
overlap: skip
enabled: true
team: hub
fires_in: grain
auto_approve_proposals: false
created_at: "2026-10-02T18:30:00Z"
---

The settings-owned `sched-1` of grain/config, which fires `template-1`
(`templates/granule-pull.md`). Its cadence here is illustrative.

It says `fires_in: grain`: Silo does not fire it, and every Grain whose
namespace contains it does, into its own root. Silo assigns it by
including this folder from a Grain's folder in `hub/placements`
(docs/task-repos.md §6.2). Grain `us` is assigned it, and excludes it in
favour of a weekly copy of its own.
