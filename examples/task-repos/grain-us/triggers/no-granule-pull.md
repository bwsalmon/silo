---
kind: exclude
schema: grain/exclude@v1
names: ["shared:triggers/granule-pull.md"]
added_by: "user:admin@example.com"
added_at: "2026-10-16T17:30:00Z"
reason: This Grain runs the granule pull weekly, from its own trigger.
---

Cuts the team's daily granule-pull trigger out of this Grain's namespace. The
exclude is in the root, so it is applied last, and nothing included can
bring the name back (docs/task-repos.md §3.3). The replacement is
`granule-pull-weekly.md`, beside it: a new object with a name of its own,
not an override of the old one.
