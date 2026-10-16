---
kind: include
schema: grain/include@v1
target: "placements:grains/us/schedules/@main"
ref_mode: track
kinds: [schedule]
added_by: "user:admin@example.com"
added_at: "2026-10-16T16:55:00Z"
---

The team schedules Silo assigns to this Grain. That folder holds no
schedules itself: it holds Silo's `kind: include` of `shared:schedules/`.
Includes are transitive, so the team's schedules arrive here, narrowed to
`kinds: [schedule]` all the way down (docs/task-repos.md §3.3).
