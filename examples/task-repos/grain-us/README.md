---
kind: repo
schema: grain/repo@v1
role: grain-root
grain: {id: us, silo: https://silo.example.com}
title: Root of Grain "us"
owner:
  grain: us
  team: hub
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
backlog: [backlog/]
lanes:
  mode: strict
  order:
    - {lane: "placements:grains/us/backlog/"}
    - {lane: backlog/}
aliases:
  intake: https://silo.example.com/git/hub/intake.git
  shared: https://silo.example.com/git/hub/shared.git
  placements: https://silo.example.com/git/hub/placements.git
  eu: https://silo.example.com/git/grains/eu.git
writers: ["grain:us", "team:hub#admin"]
---

# Root of Grain "us"

The root of Grain `us`, a team Grain owned by the hub team's admins. Only
the Grain and its owners write here (docs/task-repos.md §8.1).

- `backlog/silo.md` includes Silo's placements for this Grain. The example
  shows a task re-placed here from Grain `eu` (§8.8): Silo moved its
  placement between the two Grains' folders in `hub/placements`, and this
  Grain's execution record names the fenced record in `eu` as
  `previous`. The task depends on a task that Grain `eu` completed, which
  this Grain read from `eu`'s execution record (§8.9).
- `triggers/silo.md` includes the team triggers Silo assigned to this
  Grain, which Silo's folder in turn includes from `hub/shared`.
  `triggers/no-granule-pull.md` excludes one of them, and
  `triggers/granule-pull-weekly.md` is this Grain's own trigger in its
  place, referencing the same template, and the original in
  `derived_from` (§6.2).
- Lanes are strict: Silo's placements first, then the team admins' own
  work in `backlog/`.
