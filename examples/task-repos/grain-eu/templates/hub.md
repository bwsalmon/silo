---
kind: include
schema: grain/include@v1
target: "shared:templates/@2026-10-16"
ref_mode: pin
kinds: [template]
added_by: "user:bsalmon@google.com"
added_at: "2026-10-16T09:05:00Z"
---

The hub team's templates, pinned to the team's `2026-10-16` tag so that
what the owner files from here does not change under them. That folder
includes the org-wide templates in turn, and excludes one of them
(`hub-shared/templates/org.md`, `no-legacy-triage.md`). Both apply here:
includes are always transitive, and the team's exclude is scoped to the
team's branch (docs/task-repos.md §3.3).
