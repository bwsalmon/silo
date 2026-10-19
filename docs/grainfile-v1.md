# Grainfile v1 Quick Reference (`grainfile: v1`)

**Use Case & Overview:** Grainfiles are version-controlled, declarative Markdown documents (`.task.grain.md`, `.process.grain.md`, and `.capability.grain.md`) stored in Git repositories that coordinate one-shot tasks, standing event-driven processes, and named capabilities across Grain deployments via the Silo Hub. A `.task.grain.md` dispatches a discrete unit of work (optionally gated by `depends_on`, `not_before`, and requested `capabilities`) to a target `recipient` deployment (defaulting to `"/"` for the source repository's deployment, or a canonical Hub deployment URL `https://<hub-host>/deployments/<name>`), while a `.process.grain.md` defines a reusable automation workflow or merge gate triggered by repository, CI workflow, schedule, or manual events (`when`). A `.capability.grain.md` defines a named capability, toolset, or credential grant that deployments can offer and that `.task.grain.md` and `.process.grain.md` files can request via `capabilities:`. Grain deployments connect to the Silo Hub, subscribe to the `.task.grain.md` recipient identities they serve (`recipients`) and the `.process.grain.md` / `.capability.grain.md` repositories, folders, or URLs they execute or offer (`repos`, `folders`, `grainfiles`), and automatically include and run matching tasks and processes when their triggers, dependencies, and capabilities are satisfied.

## 1. File Naming & Templates (`GF001`–`GF006`, `GF033`, `GF034`)
- **One-shot task:** `<stem>.task.grain.md` — `kind = "task"`: executes once on its `recipient` deployment when dependencies (`depends_on`), `not_before`, and requested `capabilities` are satisfied.
- **Recurring/event process:** `<stem>.process.grain.md` — `kind = "process"`: standing 1-to-many workflow or merge gate (`gate: true`) that executes whenever its `when` condition fires.
- **Capability definition:** `<stem>.capability.grain.md` — `kind = "capability"`: defines a named capability, toolset, or credential grant that deployments can offer and that `.task.grain.md` and `.process.grain.md` files can request via `capabilities:`. Its Markdown body describes the capability for humans and agents (what it grants, how a deployment provisions it, and how an agent uses it), and its `references:` may point to companion documentation, scripts, or provisioning assets.
- **Plain `.grain.md` files are 100% literal:** Zero template evaluation is performed on `<stem>.task.grain.md`, `<stem>.process.grain.md`, or `<stem>.capability.grain.md`; `{{ ... }}` inside Markdown or code blocks is literal text.
- **Jinja2 Grainfile templates:** `<stem>.task.grain.md.jinja2`, `<stem>.process.grain.md.jinja2`, and `<stem>.capability.grain.md.jinja2` apply sandboxed Jinja2 (`ImmutableSandboxedEnvironment(loader=None)`) as-is to the entire file (parameterizing both YAML front matter and Markdown body) to produce a `<stem>.task.grain.md`, `<stem>.process.grain.md`, or `<stem>.capability.grain.md` document, which is then validated by the standard `v1` Grainfile validator (including the mandatory `grainfile: v1` field).
  - Forbidden Jinja2 tags (`{% include %}`, `{% extends %}`, `{% import %}`, `{% from %}`) are rejected (`GF034`).
  - Jinja2 syntax or evaluation errors are rejected (`GF033`), and both raw and rendered size must fit within `MaxFileBytes` (`GF006`).
  - Undeclared variables are extracted from the AST in deterministic sorted order (`template_vars`).
- `<stem>` MUST match `^[A-Za-z0-9][A-Za-z0-9._-]*$` (uppercase and lowercase ASCII letters, digits, `.`, `_`, `-`, starting with an ASCII letter or digit). Compact ISO timestamps in filename stems MUST use `Z` (UTC), e.g. `2026-10-18T143000Z-fix-API.task.grain.md`. Two Grainfiles in the same repository MUST NOT collide when compared case-insensitively (`GF006`).
- Files ending in `.grain.md` or `.grain.md.jinja2` with an unknown `<kind>` suffix (e.g. `foo.unknown.grain.md`) warn with `GF005`. Ordinary `.md` files without `.grain.md` (such as `README.md` or `docs/design.md`) are regular Markdown assets and do not trigger `GF005`.

## 2. Front Matter (`---` YAML `---` + Optional Markdown Body)
Standard YAML 1.2 Core Schema (`GF004`: no anchors `&`, aliases `*`, merge keys `<<`, or tags `!`). Unknown non-`x_*` keys warn (`GF022`); vendor extensions MUST start with `x_`. The Markdown body after the closing `---` delimiter is **optional and may be empty** when the front matter (`title`, `from_template`, `references`, `when`, etc.) carries everything the task, process, or capability requires.

The finalized `grainfile: v1` front-matter vocabulary consists of **12 fields** (plus `x_*` extensions):
- **Shared across all 3 kinds (`task`, `process`, `capability`) (6):** `grainfile` (`"v1"`, **required**, `GF020`), `title` (**required**, `GF020`), `draft`, `not_before`, `references`, `from_template` (optional reference to a `.grain.md.jinja2` template of the **same kind**: `.task.grain.md.jinja2` on `task`, `.process.grain.md.jinja2` on `process`, `.capability.grain.md.jinja2` on `capability`; `GF035`)
- **Shared by `.task.grain.md` and `.process.grain.md` (1):** `capabilities` (optional list of references to concrete `.capability.grain.md` files; `GF032` if target is not a concrete `.capability.grain.md`, `GF030` if missing in the linted tree; not permitted on `.capability.grain.md` with `GF020`)
- **`.task.grain.md` only (3):** `recipient` (optional, default `"/"`), `requestor` (optional), `depends_on` (optional; targets must resolve to concrete `.task.grain.md` files, never `.process.grain.md`, `.capability.grain.md`, or unrendered `.jinja2` templates; `GF032` / `GF034`)
- **`.process.grain.md` only (2):** `when` (**required**), `gate` (optional, default `false`)
- **`.capability.grain.md`:** Requires `grainfile: v1` and `title` (`GF020`); may include `draft`, `not_before`, `references`, `from_template` (ending in `.capability.grain.md.jinja2`), `x_*` extensions, and an optional Markdown body; must NOT specify `recipient`, `requestor`, `depends_on`, `capabilities`, `when`, or `gate` (`GF020`).

| Field | Kind | Type | Requirement / Default | Description |
| :--- | :--- | :--- | :--- | :--- |
| `grainfile` | all 3 | `"v1"` | **Required** (`"v1"`) | Mandatory format version identifier on every `.task.grain.md`, `.process.grain.md`, and `.capability.grain.md` (and rendered `.grain.md.jinja2` output). Omitting `grainfile` or specifying an unsupported version fails validation (`GF020`). |
| `title` | all 3 | `string` | **Required** | Single-line human-readable summary (`1..200` chars). |
| `draft` | all 3 | `boolean` | `false` | When `true`, indexed for inspection only; never executes or grants `references` access. |
| `not_before` | all 3 | `string` | *(none)* | Earliest activation time: `YYYY-MM-DD` (`00:00:00Z` UTC) or RFC 3339 timestamp with `Z` or explicit `±HH:MM` offset (normalized to UTC; `-00:00` and naive timestamps fail with `GF020`). |
| `references` | all 3 | `string[]` | `[]` | Read-only context files/folders (`https://...`, `/repo-rooted`, `./relative`, `../relative`). |
| `from_template` | all 3 | `string` | *(none)* | Optional provenance link to the source `.task.grain.md.jinja2`, `.process.grain.md.jinja2`, or `.capability.grain.md.jinja2` template matching the Grainfile's kind (`GF035`). Same-repo templates are added to `authorized_references` at `commit_sha`; auto-populated on `.grain.md.jinja2` rendering. |
| `capabilities` | task, process | `string[]` | `[]` | Required capabilities: concrete `<stem>.capability.grain.md` files (`GF032` if target is `.task.grain.md`, `.process.grain.md`, `.jinja2`, asset, or folder; `GF030` if missing). Same-repo capabilities are added to `authorized_references` at `commit_sha`. Forbidden on `.capability.grain.md` (`GF020`). |
| `recipient` | task | `string` | `"/"` | Target Grain deployment identity: canonical Hub deployment URL (`https://<hub-host>/deployments/<name>` or `/deployments/<name>`), forge repo/folder URL (`https://...`), or repo-local path. |
| `requestor` | task | `string` | *(none)* | Attribution (`Name <email@domain>`, canonical Hub deployment URL `https://<hub-host>/deployments/<name>`, or deployment URL/path). |
| `depends_on` | task | `string[]` | `[]` | Prerequisite concrete `.task.grain.md` files that must complete before this task runs (`GF032`). |
| `when` | process | `object` | **Required** | Trigger map: `schedule`, `push`, `pull_request`, `merge`, `workflow`, `manual`, `scenario`. |
| `gate` | process | `boolean` | `false` | Advisory merge-gate signal on `pull_request` / `push` / `merge` / `workflow` triggers. |

### Timestamps, Timezones, and Schedules (`GF020`)
- **Full timestamps (`not_before` and timestamp metadata):** MUST be valid RFC 3339 strings with an explicit timezone designator — either `Z` (UTC) or a numeric UTC offset `+HH:MM` / `-HH:MM` (`00 <= HH <= 23`, `00 <= MM <= 59`). Naive timestamps without an offset (e.g. `2026-10-25T09:00:00`) and RFC 3339's unknown-local-offset `-00:00` are rejected (`GF020`). All parsed timestamps are converted to and compared in UTC (`Z`).
- **Bare calendar dates (`YYYY-MM-DD` in `not_before`):** Always represent `00:00:00Z` on that UTC calendar date.
- **Filename stems:** Compact ISO timestamps in `<stem>` MUST use `Z` (UTC), e.g. `2026-10-18T143000Z-fix-API.task.grain.md`.
- **Cron schedules (`when.schedule`):** Accepts either a single `{ cron, timezone? }` object or a non-empty list of `{ cron, timezone? }` objects.
  - `timezone` defaults to `"UTC"` when omitted and MUST be `"UTC"` or a valid IANA Time Zone Database identifier (e.g. `"America/Los_Angeles"`, `"Europe/Berlin"`, `"Asia/Tokyo"`). Numeric offsets (e.g. `"+02:00"`), unknown names, and non-IANA abbreviations (e.g. `"PST"`, `"EST"`, `"CET"`) are rejected (`GF020`).
  - **DST transitions:** For non-UTC IANA zones, cron fields are evaluated against local wall-clock time in that zone; during spring-forward gaps a nonexistent wall-clock minute does not fire (or fires once at the transition boundary per scheduler policy), and during fall-back overlaps an ambiguous wall-clock minute fires only on its first occurrence.

### `when` Trigger Sources (`.process.grain.md` only)
- `schedule`: `{ cron: "0 2 * * *", timezone?: "UTC" }` or a non-empty list of `{ cron, timezone? }` objects.
- `push`, `pull_request`, `merge`: Either a single filter object or a non-empty list of filter objects (`GF020`). Each filter object supports:
  - `repo?: "owner/repo" | "https://github.com/owner/repo"` and/or `repos?: ["owner/repo", ...]` (defaults to the `.process.grain.md` repository; cross-repo triggers require both repositories to belong to the same team in `hub/directory`).
  - `branches?: ["main", "release/*"]` (branch names or globs).
  - `paths?: ["src/handler.go", "src/auth/", "/src/auth", "schemas/**/*.sql", "https://github.com/acme/payments/tree/main/src/auth"]` (repo-relative file paths, folder paths `dir/` or `/dir` matching `dir/**`, globs, or full forge `https://` file/folder URLs).
  - `ignore?: ["docs/", "**/*.md"]` (excluded file paths, folder paths, or globs).
  - At least one of `repo`, `repos`, `branches`, or `paths` MUST be specified on each filter object (`GF020`).
- `workflow`: Either a single workflow filter object or a non-empty list of workflow filter objects (`GF020`). Fires when a CI workflow run or check suite is triggered, succeeds, or fails:
  - `workflow?: "<name-or-glob-or-path>"` and/or `workflows?: ["<name-or-glob-or-path>", ...]` (e.g. `"grain CI"`, `"e2e-*"`, `".github/workflows/release.yml"`; at least one of `workflow` or `workflows` is required, `GF020`).
  - `events?: ["triggered" | "succeeded" | "failed"]` (aliases `"started"` -> `"triggered"`, `"success"` -> `"succeeded"`, `"failure"` -> `"failed"`; defaults to `["succeeded", "failed"]` when omitted).
  - `repo?: "owner/repo" | "https://github.com/owner/repo"` and/or `repos?: ["owner/repo", ...]` (same-repo by default; cross-repo allowed within the same team).
  - `branches?: ["main", "release/*"]` (branch names or globs matched against the workflow run's head branch).
- `manual: {}`
- `scenario: "Description"` or `scenario: { description: "..." }`

## 3. Minimal Copy-Paste Examples

**`.task.grain.md`:**
```markdown
---
grainfile: v1
title: Migrate widget schema to v2
recipient: /
requestor: Platform Team <platform@example.com>
not_before: "2026-10-25T09:00:00+02:00"
capabilities:
  - /.grain/capabilities/gcp-key.capability.grain.md
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
title: Security review and CI failure triage
gate: true
capabilities:
  - /.grain/capabilities/gcp-key.capability.grain.md
references:
  - /docs/security-checklist.md
when:
  pull_request:
    branches: [main]
    paths: ["schemas/**", "db/**"]
  workflow:
    workflows: ["grain CI", "e2e-*", ".github/workflows/release.yml"]
    events: ["triggered", "succeeded", "failed"]
    branches: ["main", "release/*"]
  schedule:
    - cron: "0 9 * * 1-5"
      timezone: "America/Los_Angeles"
    - cron: "0 9 * * 1-5"
      timezone: "Europe/Berlin"
  manual: {}
---
Review the pull request diff or failing CI workflow run against `/docs/security-checklist.md`.
```

**`.capability.grain.md`:**
```markdown
---
grainfile: v1
title: Provision a short-lived GCP service-account key in the sandbox
references:
  - /docs/gcp-workload-identity.md
---
Grants a short-lived GCP service-account credential mounted at `$GOOGLE_APPLICATION_CREDENTIALS`.
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
- **Idempotency & withdrawal:** A `.task.grain.md` executes once; editing it after completion is a no-op (`task_completed`). Deleting an incomplete `.task.grain.md` or setting `draft: true` emits `task_removed` (`action: "removed"`) and cancels any in-flight run (`dependency_completed` with `cancelled`). Deleting a `.process.grain.md` or setting `draft: true` emits `process_removed` (`action: "removed"`). Deleting a `.capability.grain.md` or setting `draft: true` emits `capability_removed` (`action: "removed"`).
- **1-to-1 tasks vs 1-to-many processes & capabilities:** `.task.grain.md` targets a single `recipient` deployment (defaulting to `/`) and has a single terminal `done` state, whereas `.process.grain.md` and `.capability.grain.md` have no `recipient` and no single completion state — multi-step or cross-task coordination inside a process is expressed in natural language in the `.process.grain.md` prompt itself (where the executing agent can run steps or propose/chain sub-tasks locally).
- **`depends_on` & `capabilities` targets (`GF020`, `GF032`, `GF034`):** `depends_on` is only valid on `.task.grain.md` (`GF020` on `.process.grain.md` and `.capability.grain.md`), and every `depends_on` target MUST resolve to a concrete `.task.grain.md` file (`GF032`), subject to DAG cycle detection (`GF034`). `capabilities` is valid on `.task.grain.md` and `.process.grain.md` (`GF020` on `.capability.grain.md`), and every `capabilities` entry MUST resolve to a concrete `.capability.grain.md` file — never a `.task.grain.md`, `.process.grain.md`, unrendered `.jinja2` template, asset, or directory (`GF032`).
- **Same-origin reference authority:** A repository can only grant read access to its own files/folders at the Grainfile's pinned commit SHA (`authorized_references`, which includes same-repo `references`, `capabilities`, and `from_template`). Cross-repo `references` and `capabilities` require the executing deployment's own credentials.
- **Full specification:** See `docs/grainfiles-spec.md`.

## 5. Lint Codes (`GF001`–`GF036`)
| Code | Severity | Condition |
| :--- | :--- | :--- |
| `GF001` | error | Missing opening `---` on line 1 or missing closing `---` front-matter delimiter |
| `GF002` | error | Malformed YAML front matter or non-mapping root |
| `GF003` | error | Duplicate YAML mapping key |
| `GF004` | error | Disallowed YAML construct (anchor `&`, alias `*`, merge key `<<`, or tag `!`) |
| `GF005` | warning | Unregistered `.<kind>.grain.md` or `.<kind>.grain.md.jinja2` suffix |
| `GF006` | error | Invalid filename `<stem>`, non-UTC compact stem timestamp, case-insensitive path collision, or file/front-matter size limit exceeded |
| `GF020` | error | Schema validation failure: missing/invalid `grainfile: v1`, missing required field (`title`, `when`), kind-mismatch field, invalid timestamp/timezone (`-00:00`, naive timestamp, non-IANA timezone), or invalid trigger filter |
| `GF022` | warning | Unknown top-level front-matter key not prefixed with `x_` |
| `GF030` | error | Unresolvable local `recipient`, `requestor`, `references`, `depends_on`, `capabilities`, or `from_template` path or `#<anchor>` in repository tree |
| `GF031` | error | Unresolvable or invalid `references` entry |
| `GF032` | error | Wrong target kind: `depends_on` target is not a concrete `.task.grain.md` file, `capabilities` target is not a concrete `.capability.grain.md` file, or deployment identity resolves to a file instead of a repository/folder |
| `GF033` | error | Jinja2 template syntax, AST security, or evaluation error in `.grain.md.jinja2` |
| `GF034` | error | Cycle in `depends_on` graph (`A -> ... -> A`) or forbidden Jinja2 tag (`include`, `extends`, `import`, `from`) |
| `GF035` | error | Invalid or unresolvable reference or `from_template` reference (must target a `.task.grain.md.jinja2`, `.process.grain.md.jinja2`, or `.capability.grain.md.jinja2` matching the Grainfile's kind) |
| `GF036` | error | Path traversal (`..`) climbing outside repository root in `recipient`, `requestor`, `references`, `depends_on`, `capabilities`, or `from_template` |
