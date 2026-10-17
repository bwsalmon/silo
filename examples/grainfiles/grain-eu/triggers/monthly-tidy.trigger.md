---
kind: trigger
schema: grain/trigger@v1
title: Monthly tidy of eu's run notes
"on":
  schedule:
    monthly: {day: 1, at: "07:00"}
    overlap: skip
    catch_up: {mode: latest-only}
fires:
  template: ../templates/tidy-run-notes.template.md
enabled: true
team: hub
fires_in: grain
auto_approve_proposals: true
created_at: "2026-10-16T09:00:00Z"
---

`eu`'s own trigger, in its own trigger root. It is in the root's own repo,
so its firings may be filed `approved` (docs/grainfiles.md §6.6).
