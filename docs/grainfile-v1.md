# Grainfile `v1` Quick Reference (`docs/grainfile-v1.md`)

**Use Case & Overview:** Grainfiles are version-controlled, declarative Markdown documents (`.task.grain.md` and `.process.grain.md`) stored in Git repositories that coordinate one-shot tasks and standing event-driven processes across Grain deployments via the Silo Hub. A `.task.grain.md` dispatches a discrete unit of work (optionally gated by `depends_on` or `not_before`) to a target `recipient` deployment (such as its canonical Hub URL `https://<hub-host>/deployments/<name>`), while a `.process.grain.md` defines a reusable automation workflow or merge gate triggered by repository, schedule, or manual events (`when`).

## 1. File Naming & Templates (`GF001`–`GF006`, `GF033`, `GF034`)
- **One-shot task:** `<stem>.task.grain.md` — executes once when dependencies and `not_before` are satisfied.
- **Recurring/event process:** `<stem>.process.grain.md` — executes whenever its `when` condition fires.
- **Plain `.grain.md` files are 100% literal:** Zero template evaluation is performed on `<stem>.task.grain.md` or `<stem>.process.grain.md`; `{{ ... }}` inside Markdown or code blocks is literal text.
- **Jinja2 Grainfile templates:** `<stem>.task.grain.md.jinja2` and `<stem>.process.grain.md.jinja2` apply sandboxed Jinja2 (`ImmutableSandboxedEnvironment(loader=None)`) as-is to the entire file (parameterizing both YAML front matter and Markdown body) to produce a `<stem>.task.grain.md` or `<stem>.process.grain.md` document, which is then validated by the standard `v1` Grainfile validator.
  - Forbidden Jinja2 tags (`{% include %}`, `{% extends %}`, `{% import %}`, `{% from %}`) are rejected (`GF034`).
  - Jinja2 syntax or evaluation errors are rejected (`GF033`), and both raw and rendered size must fit within `MaxFileBytes` (`GF006`).
  - Undeclared variables are extracted from the AST in deterministic sorted order (`template_vars`).
- `<stem>` MUST match `^[A-Za-z0-9][A-Za-z0-9._-]*$` (uppercase and lowercase ASCII letters, digits, `.`, `_`, `-`, starting with an ASCII letter or digit, including ISO/compact UTC timestamps like `2026-10-18T143000Z-fix-API.task.grain.md`). Two Grainfiles in the same repository MUST NOT collide when compared case-insensitively (`GF006`).
- Files ending in `.grain.md` or `.grain.md.jinja2` with an unknown `<kind>` suffix (e.g. `foo.unknown.grain.md`) warn with `GF005`. Ordinary `.md` files without `.grain.md` (including `*.task.md` or `*.process.md`) are regular Markdown assets and do not trigger `GF005`.

## 2. Front Matter (`---` YAML `---` + Optional Markdown Body)
Standard YAML 1.2 Core Schema (`GF004`: no anchors `&`, aliases `*`, merge keys `<<`, or tags `!`). Unknown non-`x_*` keys warn (`GF022`); vendor extensions MUST start with `x_`. The Markdown body after the closing `---` delimiter is **optional and may be empty** when the front matter (`title`, `from_template`, `references`, `when`, etc.) carries everything the task or process requires.

The finalized `grainfile: v1` front-matter vocabulary consists of **10 fields** (plus `title` and `x_*` extensions):
- **Shared (5):** `grainfile`, `draft`, `not_before`, `references`, `from_template`
- **`.task.grain.md` only (3):** `recipient` (optional, default `"/"`), `requestor` (optional), `depends_on` (optional; targets must resolve to concrete `.task.grain.md` files, never `.process.grain.md` and never unrendered `.jinja2` templates)
- **`.process.grain.md` only (2):** `when` (required), `gate` (optional, default `false`)

| Field | Kind | Type | Default | Description |
| :--- | :--- | :--- | :--- | :--- |
| `grainfile` | both | `"v1"` | `"v1"` | Format version identifier (resolves to `docs/grainfile-v1.md`). |
| `title` | both | `string` | *(required)* | Single-line human-readable summary (`1..200` chars). |
| `draft` | both | `boolean` | `false` | When `true`, indexed for inspection only; never executes or grants `references` access. |
| `not_before` | both | `string` | *(none)* | Earliest activation time: `YYYY-MM-DD` (00:00:00Z) or RFC 3339 UTC (`YYYY-MM-DDTHH:MM:SSZ`). |
| `references` | both | `string[]` | `[]` | Read-only context files/folders (`https://...`, `/repo-rooted`, `./relative`, `../relative`). |
| `from_template` | both | `string` | *(none)* | Optional provenance link to the source `.task.grain.md.jinja2` or `.process.grain.md.jinja2` template (`GF035`). Same-repo templates are added to `authorized_references` at `commit_sha`; auto-populated on `.grain.md.jinja2` rendering. |
| `recipient` | task | `string` | `"/"` | Target Grain deployment identity: canonical Hub deployment URL (`https://<hub-host>/deployments/<name>` or `/deployments/<name>`), forge repo/folder URL (`https://...`), or repo-local path. |
| `requestor` | task | `string` | *(none)* | Attribution (`Name <email@domain>`, canonical Hub deployment URL `https://<hub-host>/deployments/<name>`, or deployment URL/path). |
| `depends_on` | task | `string[]` | `[]` | Prerequisite concrete `.task.grain.md` files that must complete before this task runs (`GF032`). |
| `when` | process | `object` | *(required)* | Trigger map: `schedule`, `push`, `pull_request`, `merge`, `manual`, `scenario`. |
| `gate` | process | `boolean` | `false` | Advisory merge-gate signal on `pull_request` / `push` / `merge` triggers. |

### `when` Trigger Sources (`.process.grain.md` only)
- `schedule: { cron: "0 2 * * *", timezone?: "UTC" }` (or list of `{ cron, timezone? }` objects)
- `push`, `pull_request`, `merge`: Either a single filter object or a non-empty list of filter objects (`GF020`). Each filter object supports:
  - `repo?: "owner/repo" | "https://github.com/owner/repo"` and/or `repos?: ["owner/repo", ...]` (defaults to the `.process.grain.md` repository; cross-repo triggers require both repositories to belong to the same team in `hub/directory`).
  - `branches?: ["main", "release/*"]` (branch names or globs).
  - `paths?: ["src/handler.go", "src/auth/", "/src/auth", "schemas/**/*.sql", "https://github.com/acme/payments/tree/main/src/auth"]` (repo-relative file paths, folder paths `dir/` or `/dir` matching `dir/**`, globs, or full forge `https://` file/folder URLs).
  - `ignore?: ["docs/", "**/*.md"]` (excluded file paths, folder paths, or globs).
  - At least one of `repo`, `repos`, `branches`, or `paths` MUST be specified on each filter object (`GF020`).
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

**`.task.grain.md.jinja2`:**
```markdown
---
grainfile: v1
title: Roll out widget service to {{ region | default("eu-west-1") | upper }}
recipient: {{ recipient_path | default("/") }}
depends_on:
  - ./prepare-schema.task.grain.md
---
Deploy widget service to `{{ region | default("eu-west-1") }}` in `{{ env | default("staging") | lower }}`.
```

## 4. Core Rules & Invariants
- **Idempotency & withdrawal:** A `.task.grain.md` executes once; editing it after completion is a no-op (`task_completed`). Deleting an incomplete `.task.grain.md` or setting `draft: true` emits `task_removed` (`action: "removed"`) and cancels any in-flight run (`dependency_completed` with `cancelled`). Deleting a `.process.grain.md` or setting `draft: true` emits `process_removed` (`action: "removed"`).
- **1-to-1 tasks vs 1-to-many processes:** `.task.grain.md` targets a single `recipient` deployment (defaulting to `/`) and has a single terminal `done` state, whereas `.process.grain.md` has no `recipient` and no single completion state — multi-step or cross-task coordination inside a process is expressed in natural language in the `.process.grain.md` prompt itself (where the executing agent can run steps or propose/chain sub-tasks locally).
- **`depends_on` targets (`GF020`, `GF032`, `GF034`):** `depends_on` is only valid on `.task.grain.md` (and rendered `.task.grain.md.jinja2`) files (`GF020` on `.process.grain.md`). Every `depends_on` target MUST resolve to a concrete `.task.grain.md` file — never a `.process.grain.md` and never an unrendered `.jinja2` template (`GF032`) — subject to DAG cycle detection (`GF034`).
- **Same-origin reference authority:** A repository can only grant read access to its own files/folders at the Grainfile's pinned commit SHA (`authorized_references`). Cross-repo `references` require the executing deployment's own credentials.
- **Full specification:** See `docs/grainfiles-spec.md`.
