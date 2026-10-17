---
kind: config
schema: grain/config@v1
team: hub
owner: {team: hub}
triggers:
  - path: "widgets:.grain/triggers/weekly-release-notes.trigger.md@main"
    ref_mode: track
---

The hub team's trigger roots, read by Silo (docs/grainfiles.md §6.6). A
team's intake config holds only `triggers:`: the intake's `backlog/` is the
team's queue, and Silo has no memory.

The team adopts one trigger it does not host, the widgets repo's weekly
release notes, by naming that one file. Nothing else in that repo's grain
folder reaches the team: not its other triggers, and not its gates, which
the widgets repo's own merge queue reads. The entry tracks `main`, so
every firing is filed `proposed`; pinning it (`@<sha>`, `ref_mode: pin`)
would let firings be filed `approved`.
