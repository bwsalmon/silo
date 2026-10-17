---
kind: trigger
schema: grain/trigger@v1
aka: ["sched-1"]
title: Nightly granule pull
"on":
  schedule:
    daily: {at: "06:00"}
    overlap: skip
    catch_up: {mode: none}
fires:
  template: ../templates/granule-pull.template.md
  inputs: {repo: "https://github.com/bwsalmon/grain.git@candidate"}
enabled: true
team: hub
fires_in: grain
auto_approve_proposals: false
created_at: "2026-10-02T18:30:00Z"
---

The settings-owned `sched-1` of grain/config, which fires `template-1`
(`templates/granule-pull.template.md`). Its cadence here is illustrative.
`catch_up: none`: a pull that missed its 06:00 while the Grain was down
waits for the next morning rather than running late.

It says `fires_in: grain`: Silo does not fire it, and every Grain whose
trigger roots list it does, into its own root: Grain `eu` names this
folder directly. Silo assigns it to Grain `us` by writing a copy into
`us`'s folder in `hub/placements` (docs/grainfiles.md §6.6); `us` ignores
that copy in its entry, in favour of a weekly trigger of its own (§10.11).
