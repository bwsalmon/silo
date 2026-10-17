---
kind: trigger
schema: grain/trigger@v1
title: Nightly granule pull (assigned to us)
derived_from: "shared:triggers/granule-pull.trigger.md"
"on":
  schedule:
    daily: {at: "06:00"}
    overlap: skip
    catch_up: {mode: none}
fires:
  template: "shared:templates/granule-pull.template.md"
  inputs: {repo: "https://github.com/bwsalmon/grain.git@candidate"}
enabled: true
team: hub
fires_in: grain
auto_approve_proposals: false
created_at: "2026-10-16T16:50:00Z"
---

Silo's assignment of the team's daily granule pull to Grain `us`: a copy
in `us`'s folder, which fires only because `us`'s `grain.config.md` names
this folder as a trigger root (docs/grainfiles.md §6.6). `us` ignores it
in that entry and runs its own weekly copy instead (§10.11).
