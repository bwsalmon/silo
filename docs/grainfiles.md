# Grainfiles: tasks and processes as plain Markdown in git

**Status: design for review (bsalmon@google.com).** The worked examples are in [`examples/grainfiles/`](../examples/grainfiles/README.md).

This document explains the design rationale for the Grainfile Format. The normative specification is **[The Grainfile Format](grainfiles-spec.md)** (`docs/grainfiles-spec.md`), accompanied by its JSON Schemas ([`schemas/grainfiles/`](../schemas/grainfiles/)) and reference linter ([`tools/grainfiles/grainlint.py`](../tools/grainfiles/grainlint.py)). Where this document and the specification ever disagree, the specification wins.

## Contents

1. Purpose and scope
2. Two kinds executed directly: `.task.md` and `.process.md`
3. Source authority and identities: `recipient` and optional `requestor`
4. References, dependencies, and `trigger_with` grouping
5. Triggers (`on`) and merge gating (`gate`)
6. Direct execution model
7. Prior art and design evolution

## 1. Purpose and scope

A Grainfile is a plain Markdown file with YAML front matter stored in git. Its goal is to let people and deployments author work in ordinary git repositories—reviewed through standard pull requests and protected branches—in a form that a Grain deployment can pick up and run directly without an intermediate translation layer.

Everything needed to specify **what to run, which deployment runs it, optional requestor attribution, when it triggers, what it depends on, and what references it reads** lives in two file suffixes:

- **`<name>.task.md` (`task`):** A discrete task to execute once.
- **`<name>.process.md` (`process`):** A standing, repeatable process with structured triggers (`on` and/or `trigger_with`) and an optional `gate` flag.

Everything that belongs to an execution engine's runtime—live task states, queue ordering, sandbox sizing, model routing, and run logs—stays in the deployment's database rather than being committed back and forth into git.

## 2. Two kinds executed directly: `.task.md` and `.process.md`

Earlier drafts of the format had eleven kinds (`config`, `task`, `comment`, `template`, `trigger`, `gate`, `capability`, `design`, `lifecycle`, `moved`, `record`), container folding (`<name>.task/`), Jinja2 template rendering, section SHA-256 requirement hashes, custom `GRAINOWNERS` syntax, and `grain.config.md` alias dirhashes. In practice, that split a single piece of recurring work across three or four files (a `grain.config.md`, a `.template.md` or folder template, a `.trigger.md` or `.gate.md`, and capability/lifecycle files) and required a template-rendering step before an engine could run anything.

Collapsing the format to **`.task.md`** and **`.process.md`** eliminates that indirection:

- **No separate `trigger`, `template`, or `gate` kinds.** A standing job or review check is one `<name>.process.md` file: its front matter says when it fires (`on` or `trigger_with`) and whether it gates a commit or pull request (`gate: true`), and its Markdown body *is* the prompt given to the agent.
- **No separate `config` or `GRAINOWNERS` files.** Any repository can hold `.task.md` and `.process.md` files (for example under `.grain/` in a code repository or in a dedicated tasks repository) without needing a marker file, and standard forge branch protection and `CODEOWNERS` govern who can merge changes.
- **No Jinja2 rendering or container folders.** Prompts are plain Markdown written directly for the agent, with context linked through `references`.

## 3. Source authority and identities: `recipient` and optional `requestor`

The **authoritative source** of a `.task.md` or `.process.md` is the URL of the repository and folder where the file itself lives (spec §6.2, §11.1). Because a front-matter string can be written by anyone who can propose a commit, trust comes from the repository's branch protection, merge policy, and `CODEOWNERS` reviews—not from an unverified string inside YAML front matter.

Accordingly, `.task.md` and `.process.md` files separate required routing from optional attribution:

- **`recipient` (required):** Which deployment is intended to execute the task or process. This is always a **deployment identity**: a plain `https://` URL naming the git repository or folder that the deployment controls (for example, `https://github.com/acme/grain-eu` or `https://github.com/acme/deployments/tree/main/grain-eu`), or a repo-local relative / `/`-rooted folder path when referencing within the same repository.
- **`requestor` (optional):** Attribution metadata recording who requested or owns the work. When present, this may be either:
  - A **user identity** in standard RFC 5322 / git author notation, `Name <email>` (for example, `Bruce Salmon <bsalmon@google.com>`), matching how git commits themselves identify authors across any forge.
  - A **plain `https://` URL** (or repo-local deployment path), such as a user profile, team URL, or a deployment URL when a deployment files follow-up work or owns a standing process.

Using plain `https://` URLs and git author strings avoids inventing a forge-specific `@handle` grammar, a custom `git::` getter prefix, or a separate identity-mapping file.

### Cross-repository access: Grainfiles as repo-scoped capability grants

When deployments connect through **[Grain Hub](hub.md)** (§1.2, §6.1.1, §6.4.1), individual Grain deployments do not need to know about or hold standing git credentials for every other deployment's or team's repositories; each deployment only connects to the Hub as its own `recipient` identity, while the Hub consumes external git repositories via a GitHub App or forge webhooks and API (never hosting git repositories itself or storing source code at rest):

- **Grainfile as a repo-scoped capability grant:** When a non-draft `.task.md` or `.process.md` is merged into **Repo A** with `recipient: <Deployment B>`, Repo A's owners (through Repo A's branch protection, merge policy, and `CODEOWNERS` review, spec §11.1) have explicitly authorized `<Deployment B>` to receive the Grainfile itself and to read the **Repo A** files and folders named in its `references` (as well as reading and working on Repo A when the Grainfile in Repo A asks `<Deployment B>` to act on Repo A). `<Deployment B>` fetches the matched `.task.md` / `.process.md` body and its authorized same-repo `references` through the Hub on demand ([`docs/hub-interface.md`](hub-interface.md)) without holding permanent git credentials for Repo A.
- **Same-origin authority rule ("a repository can only grant access to itself"):** A Grainfile committed in **Repo A** can authorize `<Deployment B>` to read `references` inside **Repo A** (resolved at the commit/ref of the Grainfile or the pinned `<ref>` in the URL). It **cannot** grant `<Deployment B>` access to a third private repository (**Repo C**) merely by listing a `https://.../repo-c/...` URL in `references` — reading Repo C requires either `<Deployment B>` already having access to Repo C, or Repo C itself publishing/authorizing it.

## 4. References, dependencies, and `trigger_with` grouping

Grainfiles refer to other files, folders, and repositories using three optional front-matter fields (spec §6.1, §8):

1. **`references`:** A list of references to design docs, source directories, files, or other grainfiles that the deployment makes available as context when the task or process runs.
2. **`depends_on`:** A list of references to other `.task.md` or `.process.md` files that must complete before this task or process runs.
3. **`trigger_with`:** A reference (or list of references) to `.task.md` or `.process.md` files. Whenever any of the referenced grainfiles is triggered, this grainfile is triggered alongside it.

### Reference syntax

- **Remote references** are plain `https://` URLs (and `file://` in local tests), optionally with `#<anchor>` naming a Markdown heading slug:
  ```
  https://github.com/acme/widgets/blob/main/docs/widget-schema.md#schema-version-header
  https://github.com/acme/widgets/tree/main/src
  https://github.com/acme/grain-eu/blob/main/processes/smoke.process.md
  ```
  Standard forge file and tree URL forms are supported—GitHub/Gitea `/<owner>/<repo>/blob/<ref>/<path>` and `/tree/<ref>/<path>`, GitLab `/-/blob/<ref>/<path>` and `/-/tree/<ref>/<path>`, Gitiles `/+/<ref>/<path>`, and direct repository URLs `https://<host>/<owner>/<repo>`. Legacy `git::` getter prefixes, `//` subdirectory separators, and `?ref=` query parameters are not used.
- **Local references** are either `/`-rooted (resolved from the repository root, such as `/docs/widget-schema.md`) or relative to the grainfile's own directory (`./smoke.process.md`, `../tasks/setup.task.md`), and must not climb above the repository root (`GF036`).

### Multi-task groups and suites without folder templates

Combining `trigger_with` and `depends_on` expresses multi-step workflows and qualification suites directly as plain `.process.md` (or `.task.md`) files:

- `smoke.process.md` declares `on: {schedule: {cron: "0 2 * * *", timezone: "Europe/Berlin"}}`.
- `report.process.md` declares `trigger_with: ["./smoke.process.md"]` and `depends_on: ["./smoke.process.md"]`.

When `smoke.process.md` triggers at 02:00 Berlin time, `report.process.md` triggers with it (`trigger_with`) and waits for `smoke.process.md` to finish before starting (`depends_on`). Adding another step to the suite is just adding another `.process.md` file—no manifest or folder-template container is needed, and the linter checks both `depends_on` and `trigger_with` for cycles (`GF034`).

## 5. Triggers (`on`) and merge gating (`gate`)

A `.process.md` specifies when it fires using `on` (required unless `trigger_with` is set; spec §7.2.1):

- **`schedule: {cron, timezone}`:** Standard five-field POSIX cron expression with an optional IANA timezone name (default `UTC`).
- **`push`, `merge`, `pull_request: {branches, paths, ignore}`:** Forge branch and path filters (at least one of `branches` or `paths` is required).
- **`manual: {}`:** On-demand firing by a human or operator.
- **`scenario: <string> | {description}`:** A natural-language scenario description specifying when the process applies (for example, when cutting a release candidate branch).

Setting **`gate: true`** on a `.process.md` marks the process as a merge/commit gate: when triggered by a commit or pull request, the deployment holds that change until the process run passes.

## 6. Direct execution model

Deployments watch their attached repositories—or subscribe via **[Grain Hub](hub.md)** as their `recipient` identity ([`docs/hub-interface.md`](hub-interface.md))—for `.task.md` and `.process.md` files whose `recipient` matches the deployment:

1. **Drafts (`draft: true`):** Authors can commit work-in-progress `.task.md` and `.process.md` files with `draft: true` (default `false`; spec §6.3). The linter still checks draft grainfiles for schema and reference errors so authors get full feedback while iterating, but recipient deployments ignore any grainfile with `draft: true` (neither queuing a task nor registering or firing a process trigger or merge gate) until `draft` is set to `false` or removed.
2. **Deferred execution (`not_before`):** Both `.task.md` and `.process.md` accept an optional `not_before` field (spec §6.4) formatted as an RFC 3339 UTC timestamp (`2026-10-25T09:00:00Z`) or a UTC calendar date (`2026-10-25`, meaning `00:00:00Z` of that day). On a `.task.md`, the deployment holds the task as deferred and does not execute it before `not_before` (in addition to waiting for any `depends_on`). On a `.process.md`, the process does not fire before `not_before`.
3. **Direct task execution:** When a non-draft `.task.md` is present on a watched or Hub-subscribed repository for that `recipient`, the deployment fetches the `.task.md` body and its authorized same-repo `references` (either on demand through Grain Hub via `fetch_grainfile` / `fetch_reference` without standing credentials for the source repository, or directly from git when it holds host-scoped credentials; §3) and queues and runs it directly once its `not_before` time (if any) is reached and its `depends_on` are satisfied. Its Markdown body is the prompt, its `references` are materialised or linked as context, and the source `.task.md` URL (plus any optional `requestor` attribution) is recorded on the task.
4. **Direct process registration:** When a non-draft `.process.md` is present for that `recipient`, the deployment registers its `on` and `trigger_with` triggers (and its `gate` flag), active once `not_before` (if set) is reached. Each firing fetches the `.process.md` body and its authorized same-repo `references` (on demand through Grain Hub or from git) and dispatches a run with the `.process.md` Markdown body as the prompt, ordered by `depends_on`.

## 7. Prior art and design evolution

- **Markdown with YAML front matter** (Jekyll, Hugo, Obsidian) for human-readable, reviewable files in git.
- **Standard forge `https://` URLs** (`/blob/<ref>/<path>`, `/tree/<ref>/<path>`, `/-/blob|tree/<ref>/<path>`, `/+/<ref>/<path>`) for clickable cross-repository references and deployment identities.
- **Git author notation** (`Name <email>`) for optional user attribution.
- **POSIX `crontab(5)` and GitHub Actions path/branch filters** for `on:` trigger sources.
- **Evolution from earlier drafts:** Tasks 4446–4635 explored storing full runtime state, backlogs, lanes, and execution folders in git; Task 4658 narrowed the format to an authoring language with eleven kinds; the current revision simplifies the authoring format further to `.task.md` and `.process.md` executed directly by deployments.
