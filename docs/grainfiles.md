# The Grainfile Format: Rationale & Overview

**Status: design for review (bsalmon@google.com).** The worked examples are in [`examples/grainfiles/`](../examples/grainfiles/README.md).

This document explains the design rationale for the Grainfile Format (`grainfile: v1`). The concise agent quick-reference is **[`docs/grainfile-v1.md`](grainfile-v1.md)**, and the normative specification is **[The Grainfile Format](grainfiles-spec.md)** (`docs/grainfiles-spec.md`), accompanied by its JSON Schemas ([`schemas/grainfiles/`](../schemas/grainfiles/)) and reference linter ([`tools/grainfiles/grainlint.py`](../tools/grainfiles/grainlint.py)). Where this document and the specification ever disagree, the specification wins.

## Contents

1. Purpose, versioning (`grainfile: v1`), and scope
2. Two kinds executed directly: `.task.grain.md` and `.process.grain.md`
3. Source authority and task identities: `recipient` (default `/`) and optional `requestor`
4. References and `depends_on` target rules
5. Triggers (`when`) and merge gating (`gate`)
6. Direct execution, task lifecycle, and security invariants
7. Prior art and design evolution

## 1. Purpose, versioning (`grainfile: v1`), and scope

A Grainfile is a plain Markdown file with YAML front matter stored in git (`**/*.grain.md`). Its goal is to let people and deployments author work in ordinary git repositories—reviewed through standard pull requests and protected branches—in a form that a Grain deployment can pick up and run directly without an intermediate translation layer.

Each Grainfile optionally declares `grainfile: v1` (defaulting to `"v1"` when omitted), which maps programmatically in Hub APIs, SDKs, and UI responses to:
- `spec_summary_path`: [`docs/grainfile-v1.md`](grainfile-v1.md) (~50-line low-token agent quick reference)
- `spec_path`: [`docs/grainfiles-spec.md`](grainfiles-spec.md) (full normative specification)
- `schemas/grainfiles/`: JSON Schemas for `common`, `task`, and `process`

Everything needed to specify **what to run, which deployment runs a task, optional requestor attribution, when a process triggers, what it depends on, and what references it reads** lives in two canonical file suffixes with lower-case ASCII stems (`^[a-z0-9][a-z0-9._-]*$`):

- **`<name>.task.grain.md` (`task`):** A discrete 1-to-1 work item directed to a single `recipient` (defaulting to `/`) to execute once per canonical `(repo_url, file_path)` on the repository's default/protected branch.
- **`<name>.process.grain.md` (`process`):** A standing, reusable 1-to-many process with structured triggers (`when`) and an optional `gate` flag that any number of Grain deployments can subscribe to (by repository, folder, or `.process.grain.md` URL).

Everything that belongs to an execution engine's runtime—live task states, queue ordering, sandbox sizing, model routing, and run logs—stays in the deployment's database rather than being committed back and forth into git. Similarly, there is no `created_at` field in front matter because git commit metadata already tracks creation and modification timestamps.

## 2. Two kinds executed directly: `.task.grain.md` and `.process.grain.md`

Earlier drafts of the format had eleven kinds (`config`, `task`, `comment`, `template`, `trigger`, `gate`, `capability`, `design`, `lifecycle`, `moved`, `record`), container folding (`<name>.task/`), Jinja2 template rendering, section SHA-256 requirement hashes, custom `GRAINOWNERS` syntax, and `grain.config.md` alias dirhashes.

Collapsing the format to **`.task.grain.md`** and **`.process.grain.md`** (`**/*.grain.md`) eliminates that indirection:

- **Distinct `.grain.md` suffix.** Using `.task.grain.md` and `.process.grain.md` allows fast `**/*.grain.md` glob discovery without colliding with ordinary prose documentation (`*.md`), while simplifying the stem grammar to pure lower-case ASCII `[a-z0-9][a-z0-9._-]*`.
- **No separate `trigger`, `template`, or `gate` kinds.** A standing job or review check is one `<name>.process.grain.md` file: its front matter says when it fires (`when`) and whether it gates a commit or pull request (`gate: true`), and its Markdown body *is* the prompt given to the agent.
- **No separate `config` or `GRAINOWNERS` files.** Any repository can hold `.task.grain.md` and `.process.grain.md` files (for example under `.grain/` in a code repository or in a dedicated tasks repository) without needing a marker file, and standard forge branch protection and `CODEOWNERS` govern who can merge changes.

## 3. Source authority and task identities: `recipient` (default `/`) and optional `requestor`

The **authoritative source** of a `.task.grain.md` or `.process.grain.md` is the URL of the repository and folder where the file itself lives (spec §6.2, §11.1). Because a front-matter string can be written by anyone who can propose a commit, trust comes from the repository's branch protection, merge policy, and `CODEOWNERS` reviews—not from an unverified string inside YAML front matter.

Accordingly, `.task.grain.md` files (1-to-1 work items) separate routing from optional attribution, while `.process.grain.md` files (1-to-many reusable processes) omit both `recipient` and `requestor` so any subscribed deployment can run them:

- **`recipient` (`.task.grain.md` only; optional, defaults to `"/"`):** Which deployment is intended to execute the task. When omitted, `recipient` defaults to `"/"`, meaning the root of the enclosing repository where the task lives. When set explicitly, it is a **deployment identity**: a plain `https://` URL naming the git repository or folder that the deployment controls (for example, `https://github.com/acme/grain-eu`), or a repo-local relative / `/`-rooted folder path. Rejected on `.process.grain.md` (`GF005`).
- **`requestor` (`.task.grain.md` only; optional):** Attribution metadata recording who requested or owns the task. Rejected on `.process.grain.md` (`GF005`). When present on a task, this may be either:
  - A **user identity** in standard RFC 5322 / git author notation (`Bruce Salmon <bsalmon@google.com>`), or
  - A **deployment / URL identity** (`https://github.com/acme/grain-eu`).

## 4. References and `depends_on` target rules

Grainfiles use two reference lists (`references` and `depends_on`), both sharing the same plain `https://` and repo-local reference syntax (spec §8.1):

- **Cross-repository references** use standard clickable forge URLs (`https://github.com/acme/widgets/blob/main/docs/widget-schema.md#schema-version-header`, `https://github.com/acme/widgets/tree/main/src`).
- **Repository-local references** use `/`-rooted paths from the repository root (`/docs/widget-schema.md`, `/src/widget.go`) or relative paths (`./2026-10-16-widget-schema-v2.task.grain.md`).

### `depends_on` target rules (spec §7, §8.3)

- **`depends_on` on a `.task.grain.md`** MUST resolve only to `.task.grain.md` files (`GF032` if a task depends on a process).
- **`depends_on` on a `.process.grain.md`** MUST resolve either to a `.process.grain.md` or to a `.task.grain.md` (`GF032` otherwise), subject to DAG cycle detection (`GF033`).

For example, in `examples/grainfiles/grain-eu/processes/qualification/`:
- `smoke.process.grain.md` declares `when: {schedule: {cron: "0 2 * * *", timezone: "Europe/Berlin"}}`.
- `report.process.grain.md` declares `when: {schedule: {cron: "0 2 * * *", timezone: "Europe/Berlin"}}` and `depends_on: ["./smoke.process.grain.md"]`.

When both processes fire on their schedule at 02:00 Berlin time, `report.process.grain.md` waits for `smoke.process.grain.md` to finish before starting (`depends_on`).

## 5. Triggers (`when`) and merge gating (`gate`)

A `.process.grain.md` specifies when it fires using **`when`** (required on every `.process.grain.md`; spec §7.2.1). Using `when` rather than `on` ensures that YAML 1.1 parsers never misparse an unquoted `on:` key as boolean `true`:

- **`schedule: {cron, timezone}`:** Standard five-field POSIX cron expression with an optional IANA timezone name (default `UTC`).
- **`push`, `merge`, `pull_request: {branches, paths, ignore}`:** Forge branch and path filters (at least one of `branches` or `paths` is required).
- **`manual: {}`:** On-demand firing by a human or operator.
- **`scenario: <string> | {description}`:** A natural-language scenario description specifying when the process applies.

Setting **`gate: true`** on a `.process.grain.md` marks the process as a merge/commit gate: when triggered by a commit or pull request, the deployment holds that change until the process run passes.

## 6. Direct execution, task lifecycle, and security invariants

Deployments watch their attached repositories—or subscribe via **[Grain Hub](hub.md)** (`recipients`, `repos`, `folders`, `grainfiles`; [`docs/hub-interface.md`](hub-interface.md))—for `.task.grain.md` files targeting their `recipient` identity and `.process.grain.md` files matching their subscribed repositories, folders, or process URLs:

1. **`.task.grain.md` lifecycle idempotency, edits, and deletion (spec §9.3):**
   - A task's identity is its canonical `(repo_url, file_path)`.
   - Once completed, subsequent pushes do not re-run it.
   - Editing a queued or deferred task updates its indexed definition in place.
   - Deleting a queued or running task (or setting `draft: true`) emits a removal event and cancels it; deleting a completed task is a no-op.
2. **Base-branch execution & `pull_request` trigger safety (spec §9, §11.1):**
   - `.task.grain.md` files execute only from default/protected branches, never from unmerged pull-request heads.
   - On `when: {pull_request: ...}`, the `.process.grain.md` definition (prompt, `when`, `gate`, `references`) is always read from the target base branch, never from the untrusted pull-request head.
3. **Directory-verified subscription & `recipient` authorization (spec §11.2):**
   - Grain Hub verifies that a node's authenticated owner is admitted for the repository of any `recipient`, `repos`, `folders`, or `grainfiles` URL the node subscribes to, fetches `/v1/grainfiles/content` (or mux-stream `fetch_grainfile` / `fetch_reference`) for, or reports `grainfile_complete` for.
4. **Commit-SHA pinning & regular-file check (spec §11.3):**
   - `authorized_references` are pinned to the exact commit SHA at which the Grainfile was indexed, and `GitHubFetcher` rejects `symlink` and `submodule` git objects.
5. **Bounded `depends_on` DAG depth (spec §11.4):**
   - `depends_on` evaluation enforces a visited set (`GF033`) and a maximum transitive depth of 16.
6. **Prompt/reference separation (spec §11.5):**
   - Executors structurally delimit the Grainfile Markdown body (trusted instructions) from materialized `references` and PR diffs (tagged with source URL and commit SHA).

## 7. Prior art and design evolution

- **Markdown with YAML front matter** (Jekyll, Hugo, Obsidian) for human-readable, reviewable files in git.
- **Standard forge `https://` URLs** (`/blob/<ref>/<path>`, `/tree/<ref>/<path>`, `/-/blob|tree/<ref>/<path>`, `/+/<ref>/<path>`) for clickable cross-repository references and deployment identities.
- **Git author notation** (`Name <email>`) for optional user attribution.
- **POSIX `crontab(5)` and GitHub Actions path/branch filters** for `when:` trigger sources.
- **Evolution from earlier drafts:** Tasks 4446–4635 explored storing full runtime state, backlogs, lanes, and execution folders in git; Task 4658 narrowed the format to an authoring language with eleven kinds; the `grainfile: v1` specification finalizes the two-kind `.task.grain.md` and `.process.grain.md` format with `when:` triggers, default `/` task recipient, 1-to-many reusable processes, deterministic task lifecycle rules, and Hub security invariants.
