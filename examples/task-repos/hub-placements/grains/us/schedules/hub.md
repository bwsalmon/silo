---
kind: include
schema: grain/include@v1
target: "shared:schedules/@main"
ref_mode: track
kinds: [schedule]
added_by: silo
added_at: "2026-10-16T16:50:00Z"
---

The hub team's schedules are assigned to Grain `us`, the team's own
Grain. Silo assigns them by including the team's schedules folder, live,
rather than copying a schedule here, so a change merged in `hub/shared`
reaches `us` at its next fetch.

`us` fires the ones that say `fires_in: grain`. A `fires_in: silo`
schedule is in `us`'s namespace too, and listed there, but only Silo
fires it (docs/task-repos.md §6.2).
