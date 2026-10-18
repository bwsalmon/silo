# Grainfile Format `v1` — Agent Quick Reference

## 1. File Naming (`GF001`–`GF006`)
- **One-shot task:** `<stem>.task.grain.md` — executes once when dependencies and `not_before` are satisfied.
- **Recurring/event process:** `<stem>.process.grain.md` — executes whenever its `when` condition fires.
- `<stem>` MUST match `^[a-z0-9][a-z0-9._-]*$` (lowercase ASCII letters, digits, `.`, `_`, `-`; no uppercase `T`/`Z` timestamps).
- Legacy `.task.md` / `.process.md` filenames are rejected by the linter (`GF005`).

## 2. Front Matter (`---` YAML `---` + Non-Empty Markdown Body)
Standard YAML 1.2 Core Schema (`GF010`–`GF014`: no anchors `&`, aliases `*`, merge keys `<<`, or tags `!`). Unknown non-`x_*` keys warn (`GF022`); vendor extensions MUST start with `x_`.

| Field | Kind | Type | Default | Description |
| :--- | :--- | :--- | :--- | :--- |
| `grainfile` | both | `"v1"` | `"v1"` | Format version identifier (resolves to `docs/grainfile-v1.md`). |
| `title` | both | `string` | *(required)* | Single-line human-readable summary (`1..200` chars). |
| `draft` | both | `boolean` | `false` | When `true`, indexed for inspection only; never executes or grants `references` access. |
| `not_before` | both | `string` | *(none)* | Earliest activation time: `YYYY-MM-DD` (00:00:00Z) or RFC 3339 UTC (`YYYY-MM-DDTHH:MM:SSZ`). |
| `references` | both | `string[]` | `[]` | Read-only context files/folders (`https://...`, `/repo-rooted`, `./relative`, `../relative`). |
| `depends_on` | both | `string[]` | `[]` | Prerequisite Grainfiles that must complete before this file runs. |
| `recipient` | task | `string` | `"/"` | Target Grain deployment identity (`https://...` repo/folder URL or repo-local path). |
| `requestor` | task | `string` | *(none)* | Attribution (`Name <email@domain>` or deployment URL/path). |
| `when` | process | `object` | *(required)* | Trigger map: `schedule`, `push`, `pull_request`, `merge`, `manual`, `scenario`. |
| `gate` | process | `boolean` | `false` | Advisory merge-gate signal on `pull_request` / `push` / `merge` triggers. |

### `when` Trigger Sources (`.process.grain.md` only)
- `schedule: { cron: "0 2 * * *", timezone?: "UTC" }` (or list of `{ cron, timezone? }` objects)
- `push: { branches?: ["main"], paths?: ["src/**"] }`
- `pull_request: { branches?: ["main"], paths?: ["src/**"] }` *(evaluated from the PR base branch; untrusted PR diff is passed as delimited data)*
- `merge: { branches?: ["main"], paths?: ["src/**"] }`
- `manual: {}`
- `scenario: "Description"` or `scenario: { description: "..." }` (or list)

## 3. Minimal Copy-Paste Examples

**`.task.grain.md`:**
```markdown
---
grainfile: v1
title: Migrate widget schema to v2
recipient: /
requestor: Platform Team <platform@example.com>
references:
  - /docs/widget-schema.md#v2
depends_on:
  - ./prepare-schema.task.grain.md
---
Migrate the widget table to v2 following `/docs/widget-schema.md#v2` and run all unit tests.
```

**`.process.grain.md`:**
```markdown
---
grainfile: v1
title: Security review on schema changes
gate: true
references:
  - /docs/security-checklist.md
when:
  pull_request:
    branches: [main]
    paths: ["schemas/**", "db/**"]
  manual: {}
---
Review the pull request diff against `/docs/security-checklist.md` and report any security issues.
```

## 4. Core Rules & Invariants
- **Idempotency & withdrawal:** A `.task.grain.md` executes once; editing it after completion is a no-op (`task_completed`). Deleting an incomplete `.task.grain.md` or setting `draft: true` emits `task_removed` (`action: "removed"`) and cancels any in-flight run (`dependency_completed` with `cancelled`). Deleting a `.process.grain.md` or setting `draft: true` emits `process_removed` (`action: "removed"`).
- **1-to-1 tasks vs 1-to-many processes:** `.task.grain.md` targets a single `recipient` deployment (defaulting to `/`), whereas `.process.grain.md` has no `recipient` and can be subscribed to by any number of Grain deployments (by repository, enclosing folder, or `.process.grain.md` URL).
- **`depends_on` targets (`GF032`, `GF034`):** A `.task.grain.md` MAY only depend on `.task.grain.md` files (`GF032`). A `.process.grain.md` MAY depend on `.process.grain.md` or `.task.grain.md` files directly, subject to DAG cycle detection (`GF034`).
- **Same-origin reference authority:** A repository can only grant read access to its own files/folders at the Grainfile's pinned commit SHA (`authorized_references`). Cross-repo `references` require the executing deployment's own credentials.
- **Full specification:** See `docs/grainfiles-spec.md`.
