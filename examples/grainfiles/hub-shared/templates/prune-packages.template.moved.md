---
kind: moved
schema: grain/moved@v1
to: ghcr/prune-packages.template.md
moved_by: "user:bsalmon@google.com"
moved_at: "2026-10-16T17:40:00Z"
reason: Group the GHCR templates under templates/ghcr/.
---

A tombstone (docs/grainfiles.md §2.2). This path is permanently the
template's old address: readers follow `to:`, and Silo's hook refuses any
new object here. The task fired on 2026-10-11 still names this path in
`template.ref`, and resolves through it.
