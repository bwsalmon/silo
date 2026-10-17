---
kind: config
schema: grain/config@v1
grain: {id: us, silo: https://silo.example.com}
owner: {team: hub}
backlog:
  mode: strict
  lanes:
    - name: silo
      path: "placements:grains/us/backlog/@main"
      ref_mode: track
    - name: own
      path: backlog/
triggers:
  - path: triggers/
  - path: "placements:grains/us/triggers/@main"
    ref_mode: track
    ignore: ["granule-pull.trigger.md"]
  - path: "widgets:.grain/triggers/nightly-ci.trigger.md@main"
    ref_mode: track
memory:
  - path: memory/
    write: true
---

Grain `us`, the hub team's own Grain. Silo's placements come first.
Silo assigns `us` the team's daily granule pull; `us` ignores it and
runs its own weekly copy from `triggers/` (docs/grainfiles.md §10.11).
It also runs the widgets repo's nightly CI, by naming that one trigger.
