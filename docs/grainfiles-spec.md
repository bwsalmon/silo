# The Grainfile Format

**Status:** draft for review (bsalmon@google.com). Normative.

This document specifies the Grainfile Format: a portable, git-native format for authoring discrete **tasks** (`<name>.task.md`) and standing **processes** (`<name>.process.md`) that Grain deployments execute directly. It defines the two grainfile kinds, their YAML front matter and Markdown prompt bodies, user and deployment identities (`requestor` and `recipient`), cross-repository and repo-local references (`references`, `depends_on`, `trigger_with`), structured process triggers (`on`) and merge gating (`gate`), the direct execution model, and the lint rules that validate a repository tree.

It is **not** an engine's internal database schema. Live run state, sandbox orchestration, queue ranking, and execution logs are each deployment's own concern (§2.3); deployments watch repositories for `.task.md` and `.process.md` files addressed to them and execute them directly (§9).

The design rationale and architectural background are in [`docs/grainfiles.md`](grainfiles.md). Where the two ever disagree, **this document wins** and the design is corrected. The machine-readable schemas are in [`schemas/grainfiles/`](../schemas/grainfiles/) and the reference linter is [`tools/grainfiles/grainlint.py`](../tools/grainfiles/grainlint.py). The worked example of Appendix A is in [`examples/grainfiles/`](../examples/grainfiles/README.md).

Where this specification makes a normative design choice, the choice is marked **Decision:**, and every decision is summarised in Appendix B.

## Contents

1. Conventions
2. Terminology, scope and conformance classes
3. Versioning and extension
4. Files and names
5. Grainfile syntax
6. Identities and common fields
7. The kinds (`task` and `process`)
8. References and graphs
9. Direct execution model
10. Errors and lint
11. Security considerations
12. Registries of identifiers
- Appendix A. The worked example
- Appendix B. Decisions made by this specification

## 1. Conventions

1.1. The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY" and "OPTIONAL" are to be interpreted as described in BCP 14 (RFC 2119, RFC 8174) when, and only when, they appear in all capitals.

1.2. Grammars use ABNF (RFC 5234). `ALPHA`, `DIGIT` and `HEXDIG` are RFC 5234's core rules; `LOWER = %x61-7A`.

1.3. "Front matter" is the YAML mapping at the head of a grainfile (§5). A field is written `name`, a nested field `parent.child`.

1.4. Times are RFC 3339 timestamps in UTC with a `Z` suffix (`2026-10-16T13:58:30Z`). A "compact UTC time" is the same instant formatted as `YYYYMMDDTHHMMSSZ` (`20261016T135830Z`).

1.5. Lint codes (`GF001`…) are defined in §10. "Is an error (GF030)" means a linter MUST report that code, and every other conformance class MUST treat the condition as §10.2 specifies.

## 2. Terminology, scope and conformance classes

### 2.1 Terms

**grainfile**
: One UTF-8 Markdown file named `<name>.task.md` or `<name>.process.md` (§4.1), with YAML front matter and a Markdown body (§5).

**task**
: A `<name>.task.md` grainfile (§7.1): a discrete unit of work to run once. Its Markdown body is the prompt given directly to the run.

**process**
: A `<name>.process.md` grainfile (§7.2): a standing, repeatable process with structured triggers (`on` and/or `trigger_with`) and an optional `gate` flag. Its Markdown body is the prompt dispatched whenever the process fires.

**deployment**
: An execution engine (such as a Grain deployment) identified by the go-getter URL of a git repository or folder that it controls (§6.2.2).

**requestor**
: Who requested the task or process: either a user identity (`Name <email>`) or a deployment identity (§6.2).

**recipient**
: The deployment identity (`git::https://...` or repo-local folder reference) intended to execute the task or process (§6.2.2).

**asset**
: Any file in a repository that is not a grainfile: source code, design documents, images, configuration files, or `README.md`.

### 2.2 In scope

The format defines:
- the two grainfile kinds, `task` (`<name>.task.md`) and `process` (`<name>.process.md`) (§7);
- user identities (`Name <email>`) and deployment identities (go-getter repository/folder references) in `requestor` and `recipient` (§6);
- go-getter remote references and repo-local references in `references`, `depends_on`, and `trigger_with` (§8);
- structured process triggers (`on`: `schedule`, `push`, `merge`, `pull_request`, `manual`, `scenario`) and commit/pull-request gating (`gate: true`) (§7.2);
- multi-step workflows and suites expressed by combining `trigger_with` and `depends_on` (§7.2.3, §8.4);
- the direct execution model by which deployments watch repositories and execute matching `.task.md` and `.process.md` files (§9);
- the validation rules and lint codes (§10).

### 2.3 Out of scope

The following are each deployment's internal concern and are not represented in grainfiles:
- internal task queues, priority ranking, and live run state;
- sandbox VM provisioning, model selection, and credential storage;
- execution history storage and internal audit tables.

### 2.4 Conformance classes

| Class | What it does | Must implement |
| --- | --- | --- |
| **reader** | Parses `.task.md` and `.process.md` files and resolves references. | §3–§8 |
| **writer** | Creates or edits grainfiles while preserving unknown fields and formatting. | reader; §5.5 |
| **executor** | A deployment: watches repositories for grainfiles matching its `recipient` identity and executes them directly. | reader; §9 |
| **linter** | Reports the conditions of §10 for a repository or tree of repositories. | reader; §10 |

The reference linter is `tools/grainfiles/grainlint.py`; where it differs from this text, this text wins.

## 3. Versioning and extension

3.1. **Kind by suffix.** A grainfile's kind is determined solely by its filename suffix: `.task.md` (`task`) or `.process.md` (`process`) (§4.1). There is no `kind:` or `schema:` front-matter field, and no directory marker file is required.

3.2. **Compatible additions.** A later revision of this specification MAY add optional fields, kinds, or trigger sources (§12).

3.3. **Unknown fields.** A front-matter field this specification does not define for the file's kind is *unknown*:
- A reader MUST ignore an unknown field when making execution decisions.
- A writer that edits an existing grainfile MUST preserve unknown fields (§5.5).
- A writer MUST NOT add an unknown field unless its name begins with `x_` (an extension field). Extension fields (`x_*`) are reserved for local or experimental metadata and are never defined by this specification.
- A linter MUST report an unknown field that does not begin with `x_` as a warning (GF022), and MUST NOT warn on `x_*` fields.

The JSON Schemas set `additionalProperties: false` and allow `^x_` via `patternProperties`; the linter maps non-`x_` additional properties to GF022.

3.4. **Unknown trigger sources fail closed.** A `.process.md` whose `on:` mapping contains an unknown trigger source MUST NOT fire and is a validation error (GF020), because silently ignoring a trigger condition would alter when a process or gate runs.

## 4. Files and names

### 4.1 Grammar

```abnf
grainfile-name = stem "." kind ".md"
stem           = ( LOWER / DIGIT / compact-utc ) *( LOWER / DIGIT / "." / "-" / "_" )
kind           = %s"task" / %s"process"            ; registered in §12.1
compact-utc    = 8DIGIT "T" 6DIGIT "Z"
```

4.1.1. A filename is parsed from the right: the trailing `.md`, then the kind suffix immediately preceding it (`.task` or `.process`), and everything before that as the `stem`.

4.1.2. The **name** of a grainfile is its stem: `2026-10-16-widget-schema-v2` is the name of `2026-10-16-widget-schema-v2.task.md`.

4.1.3. A stem MUST match the grammar above (GF001): lower-case ASCII letters, digits, `.`, `-`, `_`, optionally starting with a compact UTC timestamp (`20261016T090000Z`). **Decision:** upper-case characters are forbidden in stems so that grainfile names behave identically on case-insensitive and case-sensitive file systems.

4.1.4. Each path segment MUST be at most 255 bytes, a repository-relative path at most 1024 bytes, and two paths in the same repository MUST NOT differ only in ASCII case (GF006).

### 4.2 Placement, kinds and assets

4.2.1. Grainfiles MAY be placed in any directory of a git repository (by convention under `.grain/` in a source code repository, or in top-level directories such as `tasks/` and `processes/` in a dedicated repository).

4.2.2. A file is a grainfile if and only if its name ends in `.<kind>.md` for a kind registered in §12.1 (`.task.md` or `.process.md`). Every grainfile MUST start with YAML front matter (GF003).

4.2.3. Every other file in the repository is an asset. A Markdown file whose second-to-last dot segment is not a registered kind (for example, `notes/x.widget.md`, excluding `README.md`) is treated as an asset, and a linter SHOULD emit a warning (GF005) to catch misspelled kind suffixes.

4.2.4. A grainfile MUST NOT be stored as a Git LFS pointer (GF003). Assets MAY use Git LFS.

### 4.3 Limits

A reader MUST support at least, and MAY refuse beyond (GF006): a grainfile of 1 MiB in total size and 64 KiB of YAML front matter.

## 5. Grainfile syntax

5.1. **Encoding.** UTF-8 without a byte-order mark (BOM), using LF or CRLF line endings (writers SHOULD emit LF). NUL bytes are forbidden (GF003).

5.2. **Delimiters.** A grainfile MUST begin with a line that is `---` (optional trailing spaces allowed). Front matter consists of every following line up to the next line that is `---` (optional trailing spaces allowed). The **body** is everything after that closing delimiter line. A grainfile without front matter or without a closing delimiter is malformed (GF003).

5.3. **The YAML subset.** Front matter is YAML 1.2 restricted to a single document whose top-level value is a mapping, with plain string keys, no duplicate keys at any level, no YAML anchors (`&`), aliases (`*`), merge keys (`<<`), or explicit tags (`!`) (GF004). Scalar values are strings, integers, booleans (`true`/`false`), `null`, or floats where a schema permits `number`. YAML 1.2 boolean rules apply, so a bare `on` key is parsed as the string `"on"`; writers SHOULD quote `"on"` when writing YAML for compatibility with YAML 1.1 tools.

5.4. **The body.** The Markdown body (CommonMark with GitHub-Flavoured Markdown tables) is the **prompt** given directly to the run when a deployment executes the `.task.md` or `.process.md` (§9).

5.5. **Round-trip editing.** A tool that modifies an existing grainfile MUST preserve the ordering, comments, unknown fields, and body text of everything it did not intend to change.

5.6. **Malformed files.** A grainfile that violates §5.1–§5.3 is unreadable (GF003 or GF004). Readers MUST NOT execute it, MUST report the error, and MUST continue processing other files in the repository.

## 6. Identities and common fields

### 6.1 Common fields

Every `.task.md` and `.process.md` shares the following base fields and MAY include `x_*` extension fields (§3.3):

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | single-line string | Req. | Non-empty one-line summary of the task or process (GF020). |
| `requestor` | *user identity* or *deployment identity* | Req. | Who requested the task or process (§6.2). |
| `recipient` | *deployment identity* | Req. | The deployment intended to execute the task or process (§6.2.2). |
| `created_at` | *time* (§1.4) | Opt. | When the grainfile was created (`YYYY-MM-DDTHH:MM:SSZ`). |
| `depends_on` | list of *ref* (§8) | Opt. | References to `.task.md` or `.process.md` files that must complete first (§8.4). |
| `trigger_with` | *ref* or non-empty list of *ref* (§8) | Opt. | References to `.task.md` or `.process.md` files; when any referenced grainfile is triggered, this grainfile is triggered as well (§7.2.3, §8.4). |
| `references` | list of *ref* (§8) | Opt. | References to relevant documents, code paths, design docs, or grainfiles provided as context to the run. |
| `x_*` | any | Opt. | Extension fields (§3.3). |

### 6.2 Identities (`requestor` and `recipient`)

**Decision:** Grainfiles use two kinds of identity—**user identities** and **deployment identities**—rather than forge-specific `@handle` strings or custom principal namespaces:

#### 6.2.1 User identity

A **user identity** uses standard git author notation `Name <email>`:

```abnf
user-identity = display-name 1*WSP "<" local-part "@" domain ">"
display-name  = non-angle *( non-angle / WSP )
```

- `display-name` MUST be non-empty and MUST NOT contain `<`, `>`, or line breaks.
- The angle-bracketed address MUST be a valid `<local@domain>` email address with at least one `.` in `domain` and no whitespace or angle brackets (GF020).
- Example: `Bruce Salmon <bsalmon@google.com>`.

#### 6.2.2 Deployment identity

A **deployment identity** is a reference (§8.1) to the git repository or folder that the deployment controls:
- Across repositories, a go-getter repository or folder URL, such as `git::https://github.com/acme/grain-eu.git` or `git::https://github.com/acme/deployments.git//grain-eu`.
- Within the same repository, a repo-relative path (such as `.` or `../grain-eu`) or a `/`-rooted path from the repository root (such as `/grain-eu`).
- A deployment identity MUST NOT carry a `#<anchor>` suffix (GF035) and MUST NOT resolve to a file (GF032). When the target repository is present in the linted tree, the referenced repository or folder MUST exist (GF030).

#### 6.2.3 Rules for `requestor` and `recipient`

- **`requestor`** (REQUIRED on both `.task.md` and `.process.md`) MAY be either a user identity (`Name <email>`) or a deployment identity (when a deployment files or owns a task or process).
- **`recipient`** (REQUIRED on both `.task.md` and `.process.md`) MUST be a deployment identity (GF020). A user identity in `recipient` is an error (GF020).

## 7. The kinds

Each subsection defines one of the two grainfile kinds. The corresponding JSON Schemas are `schemas/grainfiles/task.schema.json` and `schemas/grainfiles/process.schema.json`.

### 7.1 `task` (`<name>.task.md`)

A `task` grainfile describes a discrete unit of work to be executed once by its `recipient` deployment. Its Markdown body is the prompt handed directly to the run.

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Req. | One-line summary. |
| `requestor` | *user identity* or *deployment identity* | Req. | Who requested the task (§6.2). |
| `recipient` | *deployment identity* | Req. | Deployment that executes the task (§6.2.2). |
| `created_at` | *time* | Opt. | Creation timestamp in UTC (`Z`). |
| `depends_on` | list of *ref* to `task` / `process` | Opt. | Tasks or processes that must finish before this task runs. |
| `trigger_with` | *ref* or list of *ref* to `task` / `process` | Opt. | Triggers this task alongside any of the referenced grainfiles. |
| `references` | list of *ref* | Opt. | Relevant files, folders, repositories, design docs, or grainfiles made available to the run. |
| `x_*` | any | Opt. | Extension fields (§3.3). |

Example (`2026-10-16-widget-schema-v2.task.md`):

```markdown
---
title: Add version header to the widget binary schema
requestor: Bruce Salmon <bsalmon@google.com>
recipient: git::https://github.com/acme/grain-eu.git
created_at: "2026-10-16T09:00:00Z"
references:
  - /docs/widget-schema.md#schema-version-header
  - /src/widget.go
---
Update `src/widget.go` so encoded widgets begin with the two-byte v2
schema header described in `docs/widget-schema.md`, while keeping the
decoder compatible with v1 payloads.
```

### 7.2 `process` (`<name>.process.md`)

A `process` grainfile describes a standing, repeatable process registered on its `recipient` deployment. Whenever triggered via `on` or `trigger_with`, the deployment dispatches a run with the `.process.md` Markdown body as the prompt.

| Field | Type | Req. | Default | Meaning |
| --- | --- | --- | --- | --- |
| `title` | string | Req. | | One-line summary. |
| `requestor` | *user identity* or *deployment identity* | Req. | | Who requested or owns the process (§6.2). |
| `recipient` | *deployment identity* | Req. | | Deployment that registers and executes the process (§6.2.2). |
| `created_at` | *time* | Opt. | | Creation timestamp in UTC (`Z`). |
| `depends_on` | list of *ref* to `task` / `process` | Opt. | | Grainfiles that must complete before this process's run starts. |
| `trigger_with` | *ref* or list of *ref* to `task` / `process` | Req. unless `on` | | Fires this process whenever any referenced grainfile is triggered (§7.2.3). |
| `references` | list of *ref* | Opt. | | Relevant files, folders, repositories, design docs, or grainfiles made available to each run. |
| `on` | mapping of source → options | Req. unless `trigger_with` | | Structured triggers specifying when the process fires (§7.2.1). |
| `gate` | boolean | Opt. | `false` | When `true`, gates the triggering commit or pull request on this process passing (§7.2.2). |
| `x_*` | any | Opt. | | Extension fields (§3.3). |

A `.process.md` MUST specify at least one of `on` or `trigger_with` (GF020), and MAY specify both.

#### 7.2.1 Trigger sources (`on`)

When present, `on` is a non-empty mapping from one or more registered trigger sources (§12.2) to their options. An unknown trigger source key is an error (GF020, §3.4).

- **`schedule: {cron, timezone}`**
  - `cron` (REQUIRED, string): A five-field POSIX `crontab(5)` expression (`minute hour day-of-month month day-of-week`). A `cron` string that does not consist of five whitespace-separated fields is an error (GF020).
  - `timezone` (OPTIONAL, string, default `"UTC"`): An IANA timezone name (such as `"UTC"` or `"Europe/Berlin"`).
- **`push`, `merge`, `pull_request: {branches, paths, ignore}`**
  - `branches` (OPTIONAL, non-empty list of strings): Branch names or glob patterns matched against the target branch.
  - `paths` (OPTIONAL, non-empty list of strings): Repository-relative path globs that must be touched by the change.
  - `ignore` (OPTIONAL, list of strings): Repository-relative path globs excluded from matching.
  - At least one of `branches` or `paths` MUST be specified (GF020).
- **`manual: {}`**
  - Empty mapping `{}`. Allows the process to be triggered on demand by a human or operator.
- **`scenario: <string> | {description: <string>}`**
  - Either a non-empty string or a mapping `{description}` with a non-empty `description` string, describing a specific scenario or condition under which the process applies (for example, `"When cutting a release candidate branch"`).

#### 7.2.2 Merge gating (`gate`)

When `gate: true`, the process acts as a gate on the commit or pull request that triggered it (such as a security review, architectural check, or release verification). The deployment holds merge approval for that commit or pull request until the run dispatched from the `.process.md` completes and approves/passes.

#### 7.2.3 Multi-step groups and suites (`trigger_with` and `depends_on`)

**Decision:** Multi-task groups and suites are expressed directly by combining `trigger_with` and `depends_on` across `.process.md` (or `.task.md`) files, without a separate suite or folder-template kind:
- A lead process (for example, `smoke.process.md`) declares when the group runs in `on:` (such as a nightly schedule).
- Downstream steps (for example, `report.process.md`) set `trigger_with: ["./smoke.process.md"]` and `depends_on: ["./smoke.process.md"]`.
- Whenever `smoke.process.md` fires, every grainfile that names it in `trigger_with` is queued for the same firing, and `depends_on` orders their execution so `report.process.md` runs after `smoke.process.md` succeeds.

Example (`schema-security-review.process.md` and a two-step qualification group):

```markdown
---
title: Security review for widget schema and parser changes
requestor: Bruce Salmon <bsalmon@google.com>
recipient: git::https://github.com/acme/grain-eu.git
"on":
  pull_request:
    branches: ["main"]
    paths: ["src/**"]
    ignore: ["**/*.md"]
gate: true
references:
  - /docs/widget-schema.md
  - /src/widget.go
---
Review the pull request's changes to `src/` against the binary framing
rules in `docs/widget-schema.md`. Leave inline review comments on any
bounds-checking or downgrade issue.
```

## 8. References and graphs

### 8.1 Reference forms

```abnf
reference = ( remote / rooted / relative ) [ "#" anchor ]
remote    = %s"git::" ( %s"https://" / %s"file://" ) repo-path [ "//" sub-path ] [ "?ref=" gitref ]
rooted    = "/" [ sub-path ]
relative  = sub-path                    ; may start with "./" or "../"
anchor    = 1*( ALPHA / DIGIT / "-" / "_" / "." )
```

8.1.1. **Remote references** use HashiCorp `go-getter` syntax: the forced getter prefix `git::`, the repository URL (`https://` or, in tests only, `file://`), an optional `//<sub-path>` inside the repository, and an optional `?ref=<gitref>` query parameter naming a branch, tag, or commit SHA:

```
git::https://github.com/acme/widgets.git//docs/widget-schema.md?ref=main#schema-version-header
```

**Decision:** Only the `git::` getter, only `https://` (and `file://` in tests; an executor MUST refuse `file://` outside tests), and only a single `ref` query parameter are allowed (GF035). Shorthand URLs without `git::`, SSH URLs (`git@...`, `ssh://`), and other query parameters (`depth`, `sshkey`) are errors (GF035).

8.1.2. **Local references** name paths within the same repository at the same commit:
- **Rooted references** begin with `/` and are resolved relative to the **repository root** (`/` alone names the repository root).
- **Relative references** do not begin with `/` (they MAY begin with `./` or `../`) and are resolved relative to the directory containing the referring grainfile. A relative reference MUST NOT climb above the repository root (GF036).

8.1.3. **Anchors.** `#<anchor>` is split off at the first `#` before parsing the path or query string. On a Markdown file (a grainfile or a `.md` asset), `<anchor>` names a heading's GitHub-style slug (lower-cased heading text with punctuation removed and spaces replaced by `-`). On a folder reference, an anchor is not allowed (GF030).

### 8.2 Resolution

To resolve a reference from a grainfile in repository `R`:
1. Split off any `#<anchor>` (§8.1.3).
2. Parse the reference according to §8.1 (reporting GF035 if malformed).
3. For a remote reference, locate the target repository at `ref` (or its default branch if `?ref=` is omitted). In offline linting, a `stubs.yaml` file at the top of the linted workspace maps repository URLs to local subdirectories; unmapped remote repositories are treated as external and not resolved further.
4. For a local reference, resolve `/`-rooted paths from `R`'s root and relative paths from the referring file's directory, reporting GF036 if the normalised path climbs above `R`'s root.
5. Check that the target file or directory exists in the target repository (GF030 if missing). A reference with a trailing `/` (or an empty sub-path on a remote repository) MUST resolve to a directory (GF030 if it does not exist).
6. If `#<anchor>` was given on a Markdown file, verify that a heading with that slug exists in the file's body (GF030 if absent).

### 8.3 Target kinds

References in fields that expect a specific kind of target MUST resolve to that target kind when the target repository is available (GF032):
- `depends_on`: each entry MUST resolve to a `.task.md` (`task`) or `.process.md` (`process`) grainfile.
- `trigger_with`: each entry MUST resolve to a `.task.md` (`task`) or `.process.md` (`process`) grainfile.
- `recipient` (and `requestor` when it is a deployment identity): MUST resolve to a repository or directory, never a file.
- `references`: MAY resolve to any grainfile, asset file, directory, or repository.

### 8.4 Dependency and trigger graphs

8.4.1. **`depends_on` graph.** The directed graph whose edges run from each grainfile to the grainfiles it lists in `depends_on` MUST be acyclic. Any cycle in `depends_on` is an error (GF034).

8.4.2. **`trigger_with` graph.** The directed graph whose edges run from each grainfile to the grainfiles it lists in `trigger_with` MUST be acyclic. Any cycle in `trigger_with` is an error (GF034).

### 8.5 Canonical form

The **canonical reference** of a grainfile, directory, or repository is `git::https://<host>/<repo-path>[//<sub-path>]` with `<host>` lower-cased, default port `:443` and trailing `.git` stripped from `<repo-path>`, and `.` or `..` segments normalised. Deployments compare `recipient` identities and grainfile identities using canonical form.

## 9. Direct execution model

9.1. **Repository watching.** A Grain deployment watches its attached repositories for `.task.md` and `.process.md` files whose `recipient` matches the deployment's own canonical identity (or a local reference that resolves to the deployment's folder).

9.2. **No translation step.** Grainfiles are executed directly without an intermediate template-rendering or task-compilation agent:
- **Executing a `.task.md`:** Once every grainfile in its `depends_on` list has completed successfully (and, if `trigger_with` is set, when a referenced grainfile fires), the deployment queues and runs the task directly. The `.task.md`'s `title` is the task title, its Markdown body is the prompt, its `references` are provided as context to the run, and the source `.task.md` reference is recorded on the task.
- **Executing a `.process.md`:** The deployment registers the `.process.md` directly as a standing trigger (and merge gate when `gate: true`). Whenever any condition in `on` matches—or whenever any grainfile referenced in `trigger_with` is triggered—the deployment dispatches a run once `depends_on` are satisfied, using the `.process.md`'s Markdown body as the prompt and `references` as context. When `gate: true`, the deployment holds the triggering commit or pull request until the run succeeds.

## 10. Errors and lint

10.1. A linter reports each finding with its lint code, severity (`error` or `warning`), file path, and human-readable message.

10.2. **Severity.** An *error* fails validation and causes a deployment to refuse the broken grainfile. A *warning* reports a non-fatal condition (such as an unknown non-`x_` front-matter field or a Markdown file with an unregistered kind suffix) without failing validation.

| Code | Severity | Condition |
| --- | --- | --- |
| GF001 | error | A grainfile stem violates §4.1 (`<stem>.<kind>.md` naming grammar). |
| GF003 | error | Malformed file: missing or unclosed `---` front matter, invalid UTF-8, NUL byte or BOM, Git LFS pointer, or unparseable YAML (§4.2.4, §5.1, §5.2). |
| GF004 | error | Front matter outside the YAML subset: not a mapping, non-string key, duplicate key, or uses YAML anchors, aliases, merge keys, or tags (§5.3). |
| GF005 | warning | A Markdown file has an unregistered `.<kind>.md` suffix (§4.2.3). |
| GF006 | error | A path or file size limit of §4.1.4 or §4.3 is exceeded, or two paths in a repository differ only in case. |
| GF020 | error | Schema or field validation failure: missing required field (`title`, `requestor`, `recipient`, or `on`/`trigger_with` on `process`), wrong field type, multiline `title`, invalid `requestor` or `recipient` identity, unknown `on` trigger source, or invalid `schedule.cron` / event filter (§6, §7). |
| GF022 | warning | Unknown front-matter field not beginning with `x_` (§3.3). |
| GF030 | error | A reference does not resolve: missing file, missing directory, or missing Markdown heading `#<anchor>` (§8.2). |
| GF032 | error | A reference resolves to the wrong kind: `depends_on` or `trigger_with` target is not a `task` or `process` grainfile, or a deployment identity resolves to a file instead of a repository or folder (§6.2.2, §8.3). |
| GF034 | error | Cycle in `depends_on` or `trigger_with` (§8.4). |
| GF035 | error | Malformed reference: not a valid `git::https://` (or `git::file://`) go-getter string or local path, uses an unsupported getter, scheme, or query parameter, or attaches `#<anchor>` to a deployment identity (§6.2.2, §8.1). |
| GF036 | error | A relative reference climbs above its repository root (§8.1.2). |

## 11. Security considerations

11.1. **Protected branches.** Deployments SHOULD act only on `.task.md` and `.process.md` files from protected branches (or verified pull-request events for `on.pull_request`), using the forge's branch protection and `CODEOWNERS` reviews as the write-authority boundary.

11.2. **Host-scoped credentials.** References never carry credentials (§8.1.1). When fetching a cross-repository reference, a deployment MUST only send credentials configured for that reference's host.

11.3. **No local file URLs in production.** `git::file://` references exist only for local test suites; an executor MUST refuse `file://` references outside tests (§8.1.1).

11.4. **Resource limits.** Readers enforce the file, front-matter, and path-length bounds of §4.1.4 and §4.3 before parsing YAML or dispatching runs.

## 12. Registries of identifiers

12.1. **Kinds:** `task`, `process`.

12.2. **Trigger sources:** `schedule`, `push`, `merge`, `pull_request`, `manual`, `scenario`.

12.3. **Adding identifiers** requires a revision of this specification updating this section, `schemas/grainfiles/`, and `tools/grainfiles/grainlint.py` together.

## Appendix A. The worked example

[`examples/grainfiles/`](../examples/grainfiles/README.md) contains two example repositories mapped via `stubs.yaml`; `python3 tools/grainfiles/grainlint.py examples/grainfiles` validates them all:

- **`widgets/`** (`https://github.com/acme/widgets.git`): a code repository with design notes (`docs/widget-schema.md`), Go source (`src/widget.go`), two discrete tasks in `.grain/tasks/` (`2026-10-16-widget-schema-v2.task.md` and `2026-10-16-widget-schema-v2-migration.task.md` linked with `depends_on`), and two processes in `.grain/processes/` (`schema-security-review.process.md` with `gate: true` on pull requests and pushes, and `release-cut-check.process.md` with `scenario` and `manual` triggers).
- **`grain-eu/`** (`https://github.com/acme/grain-eu.git`): a deployment repository with a two-step nightly qualification group in `processes/qualification/`: `smoke.process.md` (with adeployment URL `requestor` and `recipient`, `on.schedule` in `Europe/Berlin`, and cross-repo `references`) and `report.process.md` (paired via `trigger_with: ["./smoke.process.md"]` and ordered via `depends_on: ["./smoke.process.md"]`).

Broken fixtures exercising the lint codes of §10 are under `tools/grainfiles/testdata/bad/`, generated by `tools/grainfiles/testdata/mkfixtures.py` and checked by `python3 tools/grainfiles/grainlint.py --self-test`.

## Appendix B. Decisions made by this specification

1. **Two directly executed kinds (`task` and `process`).** Discrete one-off tasks are `<name>.task.md` and standing/repeatable processes are `<name>.process.md`, both executed directly with their Markdown body as the prompt (§2.1, §7, §9).
2. **Lower-case ASCII stems.** Grainfile stems use lower-case ASCII letters, digits, `.`, `-`, `_` (and optional compact UTC timestamps) so filenames are portable across case-insensitive file systems (§4.1.3).
3. **Unknown fields warn; `x_*` extends; unknown trigger sources fail closed.** Unrecognised fields emit a warning (`GF022`) while `x_*` extension fields are silent; unknown `on` trigger sources are an error (`GF020`) (§3.3, §3.4).
4. **Git author user identities and go-getter deployment identities.** `requestor` accepts either `Name <email>` or a deployment repository/folder reference; `recipient` requires a deployment repository/folder reference (§6.2).
5. **Multi-step workflows via `trigger_with` and `depends_on`.** Co-triggering (`trigger_with`) combined with ordering (`depends_on`) expresses multi-task groups and suites without a separate suite or folder-template kind (§7.2.3, §8.4).
6. **Restricted go-getter and repo-local references.** Remote references allow only `git::`, `https://` (`file://` in tests), and `?ref=`; `/`-rooted references are relative to the repository root (§8.1).
