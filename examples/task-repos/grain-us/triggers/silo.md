---
kind: include
schema: grain/include@v1
target: "placements:grains/us/triggers/@main"
ref_mode: track
kinds: [trigger]
added_by: "user:admin@example.com"
added_at: "2026-10-16T16:55:00Z"
---

The team triggers Silo assigns to this Grain. That folder holds no
triggers itself: it holds Silo's `kind: include` of `shared:triggers/`.
Includes are transitive, so the team's triggers arrive here, narrowed to
`kinds: [trigger]` all the way down (docs/task-repos.md §3.3). That
narrowing is also why no gate the team's folder ever holds can reach
this Grain's namespace through this include: it accepts triggers only.
