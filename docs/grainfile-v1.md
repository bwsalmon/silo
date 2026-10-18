# Grainfile `v1` Quick Reference (`docs/grainfile-v1.md`)

A **Grainfile** is a Markdown file with YAML front matter (`---`) committed to a git repository (`**/*.grain.md`).
- **One-shot task:** `<stem>.task.grain.md` — executes once per canonical `(repo_url, file_path)` on the default/protected branch.
- **Recurring/event process:** `<stem>.process.grain.md` — executes whenever its `when` or `trigger_with` condition fires.
- **Stem grammar:** `<stem>` MUST match `^[a-z0-9][a-z0-9._-]*$` (lowercase ASCII, digits, `.`, `_`, `-`).

## Front-Matter Fields (`grainfile: v1`)

| Field | Kind | Type | Default | Description |
| :--- | :--- | :--- | :--- | :--- |
| `grainfile` | both | `"v1"` | `"v1"` | Spec version (`docs/grainfile-v1.md`, [`docs/grainfiles-spec.md`](grainfiles-spec.md)). |
| `title` | both | `string` | first `#` / filename | Short human-readable summary (`1..200` chars). |
| `requestor` | both | `string` | git commit author | `Name <email>`, `<email>`, `https://...`, or `/path`. |
| `recipient` | both | `string` | `"/"` | Target deployment URL or repo-relative path (`"/"` = enclosing repo). |
| `draft` | both | `boolean` | `false` | When `true`, indexed but never executed. |
| `not_before` | task | `string` | none | `YYYY-MM-DD` (00:00Z) or RFC 3339 timestamp with `Z` / numeric offset. |
| `when` | process | `object` | none* | Event triggers: `schedule`, `pull_request`, `push`, `merge`, `manual`, `scenario`. |
| `gate` | process | `boolean` | `false` | Report pass/fail commit status when triggered by `pull_request` or `push`. |
| `trigger_with` | process | `string[]` | `[]`* | Co-trigger when any listed `.task.grain.md` or `.process.grain.md` triggers. |
| `depends_on` | both | `string[]` | `[]` | Prerequisite Grainfiles that must complete before this file runs. |
| `references` | both | `string[]` | `[]` | Context files/URLs (`https://...`, `/repo-path#anchor`, `./rel-path`). |

*\*A `.process.grain.md` MUST declare at least one of `when` or `trigger_with`.*

## Minimal Examples

```markdown
---
# .grain/tasks/2026-10-16-widget-schema-v2.task.grain.md
grainfile: v1
title: Add version header to the widget binary schema
depends_on: []
references:
  - /docs/widget-schema.md#schema-version-header
  - /src/widget.go
---
Update `src/widget.go` so encoded widgets begin with the v2 header.
```

```markdown
---
# .grain/processes/schema-security-review.process.grain.md
grainfile: v1
title: Security review for widget schema and parser changes
recipient: https://github.com/acme/grain-eu
when:
  pull_request:
    branches: ["main"]
    paths: ["src/**"]
gate: true
references:
  - /docs/widget-schema.md
---
Review changes under `src/` against `docs/widget-schema.md`.
```

## Key Lifecycle & Security Rules
- **`depends_on` targets:** A `.task.grain.md` MAY only depend on `.task.grain.md` files (`GF032`). A `.process.grain.md` MAY depend on a `.task.grain.md` or a `.process.grain.md` in the same `trigger_with` group.
- **Task lifecycle:** Identified by `(repo_url, file_path)`. Completed tasks never re-run on subsequent pushes; editing a queued/deferred task updates it; deleting a queued/running task cancels it; deleting a completed task is a no-op.
- **Security:** Tasks run only from default/protected branches. On `when: {pull_request: ...}`, the `.process.grain.md` definition is always read from the target base branch, never the PR head. Full specification: [`docs/grainfiles-spec.md`](grainfiles-spec.md) | JSON Schemas: [`schemas/grainfiles/`](../schemas/grainfiles/).
