---
kind: include
schema: grain/include@v1
target: "shared:triggers/@main"
ref_mode: track
kinds: [trigger]
added_by: silo
added_at: "2026-10-16T16:50:00Z"
---

The hub team's triggers are assigned to Grain `us`, the team's own
Grain. Silo assigns them by including the team's triggers folder, live,
rather than copying a trigger here, so a change merged in `hub/shared`
reaches `us` at its next fetch.

`us` fires the ones that say `fires_in: grain`. A `fires_in: silo`
trigger is in `us`'s namespace too, and listed there, but only Silo
fires it (docs/task-repos.md §6.2).
