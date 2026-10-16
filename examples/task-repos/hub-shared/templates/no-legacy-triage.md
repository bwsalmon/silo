---
kind: exclude
schema: grain/exclude@v1
names: ["org:templates/legacy-triage.md"]
added_by: "user:maintainer@example.com"
added_at: "2026-10-14T11:00:00Z"
reason: The hub team triages through the intake, not the org's legacy queue.
---

Subtracts one org template from what this folder contributes. It is
scoped to this folder: a Grain that includes `hub/shared` does not see
`legacy-triage.md`, but a Grain that also includes `org/templates`
directly does, and nothing here can hide an object anywhere else
(docs/task-repos.md §3.3, §11.15).
