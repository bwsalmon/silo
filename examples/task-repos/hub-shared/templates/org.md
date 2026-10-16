---
kind: include
schema: grain/include@v1
target: "org:templates/@v3"
ref_mode: pin
kinds: [template]
added_by: "user:maintainer@example.com"
added_at: "2026-10-14T11:00:00Z"
---

The org-wide template set, pinned to its `v3` tag: a stable set the team
moves to on purpose, by editing this one line. Anyone who includes this
folder gets these templates too, because includes are always transitive
(docs/task-repos.md §3.3), and gets them at `v3` whatever ref they read
this folder at: a pin follows its own ref.
