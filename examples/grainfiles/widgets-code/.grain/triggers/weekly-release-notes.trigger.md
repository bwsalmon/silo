---
kind: trigger
schema: grain/trigger@v1
title: Weekly release notes
"on":
  schedule:
    weekly: {day: friday, at: "16:00"}
    timezone: Europe/London
    overlap: skip
    catch_up: {mode: none}
  manual:
    who: ["team:hub#member"]
    inputs: [since]
fires:
  template: ../templates/release-notes.template.md
  inputs: {since: latest-tag}
enabled: true
team: hub
fires_in: silo
auto_approve_proposals: false
created_at: "2026-10-16T18:05:00Z"
---

The definition lives here, beside the code. Its firings do not: Silo
files each one into the hub intake, at
`tasks/fired/widgets/weekly-release-notes/<key>.task.md`, because the hub
intake adopts this trigger by naming it in its trigger roots
(`hub-intake/grain.config.md`). A trigger that no root names never fires
for anyone (docs/grainfiles.md §6.6, §12.13).

Two sources. The Friday cadence, in London time, skips a week the last
notes are still open and does not catch up after downtime: late notes
for last week are not wanted. And `manual:`, so a hub member can ask for
notes now, choosing `since`; the scheduled firing always uses the latest
tag.

`auto_approve_proposals: false`, so each firing is filed `proposed` and
waits for a hub approver.
