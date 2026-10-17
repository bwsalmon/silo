---
kind: trigger
schema: grain/trigger@v1
title: Weekly legacy triage sweep (retired)
"on":
  schedule:
    weekly: {day: friday, at: "09:00"}
    overlap: skip
    catch_up: {mode: none}
fires:
  template: "org:templates/legacy-triage.template.md@v3"
enabled: true
team: hub
fires_in: grain
auto_approve_proposals: false
created_at: "2026-09-01T09:00:00Z"
---

Retired by moving it here, to `triggers/archive/` (docs/task-repos.md
§3.8, §8.15). Every Grain whose trigger root names `shared:triggers/`
skips `archive/`, so it fires nowhere, with no Grain changing its config.
It is still readable at its old identity, `triggers/legacy-triage-sweep.trigger.md`,
through the archive mapping.
