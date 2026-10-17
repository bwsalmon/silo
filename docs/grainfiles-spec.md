# The Grainfile Format, version 1

**Status:** draft for review (bsalmon@google.com). Normative.

This document specifies the Grainfile Format: an **authoring language
for work** kept as plain files in git. It covers the inputs to work
(task definitions with their discussion, templates, triggers,
capabilities, designs, gates and lifecycles), one lightweight output
(the execution record a deployment writes after it has run something),
how those files refer to each other across repositories, and who may
change them. It is written so that a conforming reader, writer, forge
check or linter can be built from it alone.

It is **not** an engine's runtime model. Backlogs, queues, live state,
placement, runs in progress, memory, hubs and registries are each
engine's own business (§2.3); an engine reads these files as input and
writes execution records as output.

The rationale, the alternatives and the history are in
[`docs/grainfiles.md`](grainfiles.md), the design. Where the two ever
disagree, **this document wins** and the design is corrected. The
machine-readable parts are the JSON Schemas in
[`schemas/grainfiles/`](../schemas/grainfiles/) and the reference
linter, [`tools/grainfiles/grainlint.py`](../tools/grainfiles/grainlint.py).
The worked example of Appendix A is
[`examples/grainfiles/`](../examples/grainfiles/README.md).

Where this specification had to choose something, the choice is marked
**Decision:**, and every decision is listed in Appendix B.

## Contents

1. Conventions
2. Terminology, scope and conformance classes
3. Versioning and extension
4. Files, names and folders
5. Grainfile syntax
6. Common fields and types
7. The kinds
8. References
9. Identity, moves and tombstones
10. `grain.config.md`
11. Templates
12. Capabilities
13. Execution records
14. Write authority
15. Errors and lint
16. Security considerations
17. Registries of identifiers
- Appendix A. The worked example
- Appendix B. Decisions made by this specification
- Appendix C. Migration from grain's `.grain/` declarations

## 1. Conventions

1.1. The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT",
"SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY" and
"OPTIONAL" are to be interpreted as described in BCP 14 (RFC 2119,
RFC 8174) when, and only when, they appear in all capitals.

1.2. Grammars use ABNF (RFC 5234). `ALPHA`, `DIGIT` and `HEXDIG` are
RFC 5234's core rules; `LOWER = %x61-7A`.

1.3. "Front matter" is the YAML mapping at the head of a grainfile (§5).
A field is written `name`, a nested field `parent.child`.

1.4. Times are RFC 3339 timestamps in UTC with a `Z` suffix
(`2026-10-16T13:58:30Z`). A "compact UTC time" is the same instant as
`YYYYMMDDTHHMMSSZ` (`20261016T135830Z`).

1.5. Lint codes (`GF001`…) are defined in §15. "Is an error (GF030)"
means a linter MUST report that code, and every other conformance class
MUST treat the condition as §15.2 says.

## 2. Terminology, scope and conformance classes

### 2.1 Terms

**grainfile**
: One object, kept as one UTF-8 Markdown file named `<name>.<kind>.md`
  (or by a fixed name inside a container, §4.3), with front matter (§5).

**asset**
: Any file in a grain folder that is not a grainfile: an image, a
  Terraform module, a Jinja partial, a `README.md`.

**grain folder**
: A directory that contains a file named `grain.config.md` (§10). It is
  a whole repository or a subdirectory of one. Grain folders MUST NOT
  nest (§4.5).

**container**
: A folder that is one grainfile plus its children: a definition folder
  `<name>.task/`, a capability folder `<name>.capability/` or an
  execution record `<name>.record/` (§4.3).

**definition** (task definition)
: A `task` grainfile: what to do. Its body is the prompt. It holds no
  state and no runs.

**deployment**
: An engine that executes work (one running Grain, for example). For
  this format a deployment **is its forge account** (§14.1): the
  identity that authors its records and comments.

**execution record**
: A `record` grainfile (§13): what one execution of some work did,
  written by the deployment that ran it, after it finished.

**forge**
: The git host a repository lives on. The forge's protected default
  branch is authoritative for the repository's content (§14).

**forge identity**
: An account or team on a forge, written `@login` or `@org/team`
  (§6.3). The only identity this specification uses.

### 2.2 In scope

The format defines: task definitions with their threads and assets
(§7.1); templates and folder templates (§7.4, §11); triggers (§7.5);
gates (§7.6); capabilities (§7.7, §12); designs with per-requirement
hashes (§7.8); lifecycles as state-machine definitions (§7.9); comments
(§7.2); tombstones (§9); execution records (§13); references (§8);
`grain.config.md` (§10); `GRAINOWNERS` and the forge check (§14).

### 2.3 Out of scope

These are each engine's own business. This specification says nothing
normative about them, and a grainfile that tries to carry them uses
fields this format does not define (§3.3) or the opaque `settings:`
mapping (§6.4):

- backlogs, queues, lanes, ranking, priority and the reachability of
  work;
- live execution state, placement, runs in progress, snapshots, and any
  pointer from a definition to its executions;
- how an engine tracks an execution against a lifecycle;
- memory, registries, hubs and their views;
- model, effort, sandbox, network, merge and scheduling policy;
- which of these files an engine chooses to read.

An engine MAY keep all of that in a database and use this format only
for its inputs and its execution records.

### 2.4 Conformance classes

| Class | What it does | Must implement |
| --- | --- | --- |
| **reader** | Parses grain folders and resolves references. | §3–§10, §12.3 |
| **writer** | Creates or changes grainfiles. | reader; §5.5, §9, §14.3 |
| **renderer** | Renders templates. | reader; §11 |
| **executor** | A deployment: runs work described by definitions and templates, and writes execution records. | reader, writer, renderer; §12, §13 |
| **forge check** | The required status check of a protected branch. | linter; §14 |
| **linter** | Reports the conditions of §15 for a tree, or for a change between two trees. | reader, renderer; §15 |

The reference linter is `tools/grainfiles/grainlint.py`; where it
differs from this text, this text wins.

## 3. Versioning and extension

3.1. **The format version** is declared once per grain folder, in
`grain.config.md`: `format: 1`. It applies to every grainfile in the
folder. There is no per-file version and no `kind:` field: a file's
kind is its suffix (§4.1). A reader MUST refuse a grain folder whose
`format` it does not know (GF021) and act on none of its files.

3.2. **Compatible additions.** A later revision MAY add optional fields,
kinds, trigger sources, gate requirements and state categories (§17)
without changing the format version.

3.3. **Unknown fields.** A front-matter field this specification does
not define for the file's kind is *unknown*.

- A reader MUST ignore an unknown field when deciding anything.
- A writer that rewrites a grainfile MUST preserve unknown fields (§5.5).
- A writer MUST NOT add an unknown field, except one whose name begins
  with `x_` (an extension field). Extensions are never defined here.
- A linter MUST report an unknown field that does not begin with `x_`
  as a warning (GF022), and MUST NOT report an extension field.

The JSON Schemas are strict (`additionalProperties: false`, with
`patternProperties` admitting `^x_`); the linter maps "additional
property" failures to GF022.

3.4. **Two places refuse what they do not know**, because ignoring would
silently change what runs or what lands:

- a trigger with an unknown `on:` source MUST NOT fire (GF020);
- a gate with an unknown `requires:` key MUST hold every change it
  matches (GF020).

3.5. **Breaking changes** are a new format version. A reader that knows
version 2 SHOULD keep reading version 1.

## 4. Files, names and folders

### 4.1 Grammar

```abnf
grainfile-name = stem "." kind ".md"
stem           = ( LOWER / DIGIT / compact-utc ) *( LOWER / DIGIT / "." / "-" / "_" )
kind           = LOWER *( LOWER / DIGIT / "-" )   ; registered in §17.1
tombstone-name = stem "." kind ".moved.md"
container-name = stem "." container-kind          ; a directory, §4.3
container-kind = %s"task" / %s"capability" / %s"record"
compact-utc    = 8DIGIT "T" 6DIGIT "Z"
```

4.1.1. A file name is parsed from the right: the last `.md`, then the
kind suffix immediately before it. `x.template.moved.md` is a tombstone
(kind `moved`) for `x.template.md`.

4.1.2. The **name** of a grainfile is its stem: `2026-10-16-tidy` is the
name of `2026-10-16-tidy.task.md`.

4.1.3. A stem MUST match the grammar (GF001): lower-case ASCII, digits,
`.`, `-`, `_`, optionally beginning with a compact UTC time.
**Decision:** no upper case, so names are identical on case-insensitive
file systems.

4.1.4. A path segment is at most 255 bytes, a path within a grain folder
at most 1024 bytes, and two paths in one grain folder MUST NOT differ
only in case (GF006).

### 4.2 Kinds and assets

4.2.1. A file is a grainfile exactly when its name ends in `.<kind>.md`
for a kind registered in §17.1, is `grain.config.md`, or is a container's
fixed name (§4.3). A grainfile MUST have front matter (GF003).

4.2.2. Every other file is an asset. A Markdown file whose second-to-last
suffix is not a registered kind (`x.widget.md`) is an asset, and a
linter SHOULD warn (GF005).

4.2.3. A grainfile MUST NOT be a Git LFS pointer (GF003). An asset MAY
be. **Large assets** go in Git LFS or are linked by URL from a body;
the format has no external-asset kind.

### 4.3 Reserved names and containers

| Name | Meaning | Rule |
| --- | --- | --- |
| `grain.config.md` | the grain folder's configuration (§10) | Only at a grain folder's top. Makes the directory a grain folder. |
| `GRAINOWNERS` | write ownership (§14.2) | Any directory of a grain folder. |
| `<name>.task/` | a definition folder | Holds `task.md`, `thread/`, `assets/` and `GRAINOWNERS`, nothing else. |
| `<name>.capability/` | a capability folder | Holds `capability.md`, `assets/` and `GRAINOWNERS`, nothing else. |
| `<name>.record/` | an execution record (§13) | Holds `record.md`, `comments/`, `assets/` and `GRAINOWNERS`, nothing else. |
| `thread/` in a definition folder | the definition's discussion | Only `*.comment.md` and `<comment name>.attachments/`. |
| `comments/` in a record | the execution's process log | Only `*.comment.md` and `<comment name>.attachments/`. |
| `assets/` in a container | its attachments | Assets only; never interpreted. |
| `*.moved.md` | a tombstone (§9) | Never changed, never deleted. |
| `.grain/` | a code repository's default grain folder | A convention: a grain folder only if it holds `grain.config.md`. |

`archive/` is not reserved: it is an ordinary directory, and archiving
is a move (§9.2).

4.3.1. **Fixed names.** Inside a container the kind is given by a fixed
file name instead of a suffix, and the name is the container's stem:
`<name>.task/task.md` is the definition `<name>`,
`<name>.capability/capability.md` the capability `<name>`, and
`<name>.record/record.md` the record `<name>`. A fixed name anywhere
else, a file named `*.record.md`, a container nested in a container, a
file a container does not allow, and a comment outside every `thread/`
and `comments/` are errors (GF007).

4.3.2. A container missing its fixed file, or a definition or capability
present both as `<name>.<kind>.md` and `<name>.<kind>/`, is GF045.

4.3.3. **Folding.** **Decision:** a definition with no discussion and no
assets MAY be a bare file `<name>.task.md`; so MAY a capability with no
assets. When the first comment or asset arrives, the writer **folds** it:
in one commit it moves `<name>.<kind>.md` to `<name>.<kind>/<kind>.md`
byte for byte and adds the child. A fold is not a move: it leaves no
tombstone and changes no identity, because a reference to
`<name>.<kind>.md` resolves either form (§8.5). A folder is never
unfolded. Writers SHOULD reference the `<name>.<kind>.md` form.

### 4.4 Limits

A reader MUST support at least, and MAY refuse beyond (GF006): a
grainfile of 1 MiB with 64 KiB of front matter; an asset of 10 MiB in
git (larger: LFS); 8 tombstone hops (§9.3); a template render of
256 KiB of output and 10,000 loop iterations (§11.3); 500 files from one
folder-template render. `check.limits` in `grain.config.md` MAY lower
these and MUST NOT raise them.

### 4.5 Folder boundaries

4.5.1. A grain folder's extent is its directory's subtree. Paths in
`GRAINOWNERS` and in `/`-rooted references are relative to its top.

4.5.2. A directory inside a grain folder MUST NOT contain
`grain.config.md` (GF010); a reader treats it as part of the outer
folder. A template folder MUST NOT contain one at any depth (GF011).

4.5.3. Files outside every grain folder are not grainfiles, whatever
their names, except as targets of a reference (they are then assets).

## 5. Grainfile syntax

5.1. **Encoding.** UTF-8 without a byte-order mark; LF or CRLF line
endings (writers SHOULD write LF). NUL is forbidden (GF003).

5.2. **Delimiters.** The file MUST begin with the line `---`. Front
matter is every following line up to the next line that is exactly `---`
(trailing spaces allowed). The **body** is everything after it. A file
with no closing delimiter is malformed (GF003).

5.3. **The YAML subset.** Front matter is YAML 1.2 restricted to a
single document whose top level is a mapping, with plain-string keys, no
duplicate keys at any level, no anchors, aliases or merge keys, and no
tags (GF004). Scalars are strings, integers, `true`/`false`, `null`, and
floats only where a schema says `number`. A writer MUST quote strings
YAML 1.1 would read as another type; in particular the trigger key `on`
MUST be written `"on"`, and a reader MUST read a bare `on` key as the
string `on`.

5.4. **The body** is CommonMark with GitHub-flavoured tables: what a
human or an agent reads. Links in a body are not references (§8), except
that a design's requirement anchors are headings (§7.8).

5.5. **Round-trip.** A writer that changes a grainfile MUST change only
the fields it means to change, preserving the order, comments and
spelling of every other key and the body byte for byte.

5.6. **Malformed files** (failing §5.1–§5.3) are *unreadable*. Readers
MUST NOT act on them, MUST report them and MUST continue with the rest
of the folder. A reference to one resolves to an error (GF003).

## 6. Common fields and types

6.1. **Every grainfile MAY have** `x_*` extension fields (§3.3). There is
no `id`, `kind` or `schema` field: a grainfile is identified by its
canonical reference (§8.6) and typed by its suffix. A file that sets
`id` is an error (GF020); `kind` and `schema` are unknown fields
(GF022), so files written to the format's drafts still load.

6.2. **Types.**

| Type | Definition |
| --- | --- |
| *ref* | a reference string (§8) |
| *pinned ref* | a cross-repository reference whose `?ref=` is a 40-hex commit |
| *sha* | 40 lower-case hex digits |
| *time* | §1.4 |
| *identity* | a forge identity (§6.3) |
| *settings* | an opaque mapping (§6.4) |

6.3. **Forge identities.** **Decision:** the syntax is CODEOWNERS':

```abnf
identity  = "@" [ host ":" ] name
name      = login / login "/" team
login     = 1*( ALPHA / DIGIT / "-" / "_" / "." )
team      = 1*( ALPHA / DIGIT / "-" / "_" / "." )
host      = 1*( LOWER / DIGIT / "-" / "." )      ; a forge's host name
```

`@login` is an account and `@org/team` a team on the forge that hosts
the file's repository. The host-qualified form `@gitlab.com:alice`
names an identity on another forge; it is for author fields of records
and comments written about work in another forge's repository, and is
never matched by `GRAINOWNERS`. An email address is not an identity.
Identities compare case-insensitively.

6.4. **`settings:`** is one opaque mapping, allowed in `grain.config.md`
and on definitions, templates and triggers. It belongs to executors:
model, network, `nothing_needed` behaviour, `auto_merge`, the
base branch, sandbox sizes, run policy (count, until-clean), team,
scheduling and limits all go here, under whatever structure the
executor defines. Readers MUST ignore what they do not understand in
it; writers MUST preserve it; a linter MUST NOT look inside it. This
format requires no namespacing or structure within it. Grain's keys are
defined by grain's own documentation (Appendix C).

## 7. The kinds

Each subsection lists the kind's fields. The JSON Schema
`schemas/grainfiles/<kind>.schema.json` is the machine form; where it
and this text disagree, this text wins.

### 7.1 `task` (definition)

What to do. Its body is the prompt. It is `<name>.task.md`, or
`<name>.task/task.md` when it has a thread or assets (§4.3). It MAY live
in any grain folder. A folder template's members are definitions (§11).

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Req. | One line. |
| `submitter` | *identity* | Req. | Who proposed it. |
| `created_at` | *time* | Req. | |
| `inputs` | list of input declarations (§11.4) | Opt. | Parameters an execution supplies; the executor gives their values to the run. A definition's body is never rendered. |
| `context` | list of context entries (§7.1.1) | Opt. | What a run's sandbox holds. |
| `capabilities` | list of *ref* to capabilities | Opt. | Capability requests (§12). |
| `depends_on` | list of *ref* to definitions | Opt. | Work that must finish successfully first. No cycles (GF034). |
| `implements` | list of `{ref, hash}` | Opt. | Design requirements, `ref` with a `#<requirement id>` anchor, `hash` the requirement's hash when written (§7.8). |
| `lifecycle` | *ref* to a lifecycle | Opt. | The state machine an engine tracks this work against (§7.9). |
| `supersedes` | *ref* to a definition | Opt. | The definition this one replaces. |
| `settings` | *settings* | Opt. | §6.4. |

**Changing a definition.** A definition MAY be edited; every execution
record pins the commit it ran (§13), so history is never lost. A changed
request SHOULD be a new definition with `supersedes:`. The `thread/` and
`assets/` change freely.

**Refused fields** (GF020): `id`, and the runtime fields of the format's
drafts: `state`, `placement`, `runs`, `thread`, `approved_by`.

7.1.1. **Context entries.** `{ref, mode, path}` or, in templates only,
`{input, mode, path}`:

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `ref` | *ref* (repository, folder or file) | | Exactly one of `ref` or `input`. |
| `input` | input name | | A `ref`-typed input of the template (§11.4). |
| `mode` | `read` `write` | `read` | `write` only on a repository or folder. |
| `path` | sandbox-relative path | executor's choice | Where it appears. Two entries MUST NOT name overlapping paths (GF020). |

An executor materialises exactly these entries, and nothing else the
definition references, under its own policy. How, and what it does with
a refused entry, is its business.

```yaml
---
title: Carry a task's approver on its offer
submitter: "@bsalmon"
created_at: "2026-10-16T09:00:00Z"
context:
  - {ref: "git::https://github.com/bwsalmon/silo.git", mode: write}
capabilities: ["commons:capabilities/gcp-key.capability.md"]
---
Carry `approved_by` from the task onto its offer.
```

### 7.2 `comment`

A comment is in a definition's `thread/` (discussion) or a record's
`comments/` (process log, §13.3). It is named
`<compact UTC time>-<author slug>.comment.md`, where the author slug is
the identity with every character outside `[a-z0-9]` (after lower-casing)
replaced by `-` and leading `-` removed (GF001).

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `author` | *identity* | Req. | |
| `at` | *time* | Req. | MUST equal the name's time. |
| `in_reply_to` | *ref* to a comment | Opt. | May cross containers. |
| `supersedes` | *ref* to a comment | Opt. | An edit; comments never change (GF052). |
| `record` | *ref* to a record | Opt. | On a comment a deployment's agent wrote outside the record's own `comments/`: the execution it came from. |
| `attachments` | list of *ref* | Opt. | Normally in `<comment name>.attachments/` beside it. |

### 7.3 `config`

`grain.config.md`; §10.

### 7.4 `template`

A template is work with inputs, rendered with Jinja2 (§11).

| Field | Type | Req. | Default | Meaning |
| --- | --- | --- | --- | --- |
| `form` | `task` `folder` | Opt. | `task` | `task`: a single-task template whose body is the prompt. `folder`: the manifest of a folder template (§11.2). |
| `title` | string (rendered) | Req. | | |
| `inputs` | list of input declarations (§11.4) | Opt. | `[]` | |
| `context` | list of context entries | Opt. | | `input:` entries bound at render. |
| `capabilities` | list of *ref* | Opt. | | Requests of every task rendered from it. |
| `lifecycle` | *ref* | Opt. | | |
| `settings` | *settings* | Opt. | | |

Refused (GF020): `form: partial` (partials are assets, §11.3).

### 7.5 `trigger`

"When X happens, run Y."

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Opt. | |
| `on` | mapping source → options | Req. | Exactly one source. Sources: §7.5.1. Unknown sources: §3.4. |
| `fires` | mapping | Req. | Exactly one of `template` (*ref* to a template), `definition` (*ref* to a definition) or `workflow` (a path to a workflow in the trigger's repository). With `template` or `definition`: `inputs` (values) and `per`. With `workflow`: `branch` and `review: {template, inputs}`. |
| `enabled` | boolean | Opt. (`true`) | |
| `settings` | *settings* | Opt. | |

7.5.1. **Sources.**

- `schedule: {cron, timezone, catch_up}`: `cron` is a five-field cron
  expression (minute hour day-of-month month day-of-week, as POSIX
  `crontab(5)`); `timezone` an IANA name, default `UTC`.
  **Decision:** `catch_up` is `latest-only` (fire once for the most
  recent missed time, the default) or `none`. A schedule never fires
  while the work of its previous firing is still running (it is
  skipped); there is no `overlap` field.
- `push`, `merge`, `pull_request: {branches, paths, ignore}`: `paths` is
  REQUIRED.
- `design-change: {designs}`: fires when a listed design's requirement
  hashes change (§7.8).
- `manual: {}`: fired by a person.

`task-closed` and `task-state` are reserved and refused: they key off
live state, which is out of scope (§2.3).

7.5.2. **`per`** says how one event becomes work: `event` (one per
event), `requirement` (one per changed requirement, `design-change`
only), `folder` (one per changed grain folder) or `change` (one per
pull request or push). **Decision:** the default is `event`.

A firing's key (trigger, source, scheduled time or event id, and `per`
item) is deterministic, so a retried firing is recognisable as the same
one. What the firing creates, and where, is the engine's business; its
execution records name the trigger in `trigger:` (§13.2).

Refused (GF020): `fires_in`, `fires.into`, `overlap`, `every`, `daily`,
`weekly`, `monthly`, and merge-gate keys (`gate`, `human`, `requires`).

### 7.6 `gate`

Merge policy a code repository declares for itself. Read by that
repository's merge process.

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Opt. | |
| `match` | `{paths, paths_ignore, branches}` | Req. | `paths` REQUIRED. |
| `requires` | mapping | Req. | Keys: `human: {when: always\|on-findings\|never}`, `security_review: {template}`, `second_review: {template}`. Unknown keys: §3.4. |
| `sensitive` | boolean | Opt. (`false`) | |

### 7.7 `capability`

An opaque, referenced grainfile naming a kind of authority (§12).

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Req. | |
| `description` | string | Opt. | |

Its body and its `assets/` are never interpreted by the format or its
readers. Refused (GF020): `base`, `reducers`, `secrets`, `approval`,
`grants`.

### 7.8 `design`

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Req. | |
| `status` | `draft` `review` `approved` `superseded` | Req. | |
| `owners` | list of *identity* | Req. | |
| `requirements` | list of `{id, title, hash}` | Req. | `id` matches `[A-Z][A-Z0-9]*-[0-9]+` and has a heading in the body whose text begins with `id` (GF030). `hash`, when present, MUST equal the computed hash (GF056). |
| `supersedes`, `superseded_by` | *ref* | Opt. | |

A requirement's **hash** is `sha256:` and the hex SHA-256 of its
section's text, from its heading line to the line before the next
heading of the same or higher level, with line endings normalised to LF,
trailing whitespace removed from each line, and leading and trailing
blank lines removed. An `implements` edge whose `hash` differs from the
requirement's current hash is **stale** (GF057, warning): the
requirement changed after the definition was written. A
`design-change` trigger fires when hashes change.

### 7.9 `lifecycle`

An optional state-machine **definition** a definition or template may
name. How an engine tracks live state against it is the engine's
business.

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Req. | |
| `states` | map name → `{category, title}` | Req. | Names match `[a-z][a-z0-9_-]*`. `category` from §17.3. |
| `initial` | state name | Req. | A `proposed`-category state. |
| `transitions` | list of `{from, to}` | Req. | `from` a state or `*`; `to` a list of states. |

Invalid (GF050): a transition naming an unknown state, `initial` missing
or not `proposed`-category, a category outside §17.3, no `done`-category
state.

**Categories** (closed): `proposed` (not yet accepted), `ready`
(accepted, may start), `active` (running), `waiting` (not runnable, not
finished), `done` (finished successfully), `cancelled` (finished without
success).

### 7.10 `moved` (tombstone)

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `to` | *ref* | Req. | The new address. |
| `moved_by` | *identity* | Req. | |
| `moved_at` | *time* | Req. | |
| `reason` | string | Opt. | |

### 7.11 `record`

The execution record; §13.

## 8. References

### 8.1 Forms

```abnf
reference   = ( remote / aliased / rooted / relative ) [ "#" anchor ]
remote      = %s"git::" ( %s"https://" / %s"file://" ) repo-path [ "//" sub-path ] [ "?ref=" gitref ]
aliased     = alias ":" sub-path
rooted      = "/" sub-path
relative    = sub-path                 ; may start with "./" or "../"
alias       = LOWER *( LOWER / DIGIT / "-" )
anchor      = 1*( ALPHA / DIGIT / "-" / "_" / "." )
```

8.1.1. **Remote references are go-getter strings**, exactly as Terraform
module sources write them (see go-getter's documentation, "URL format",
"Forced getters" and "Subdirectories"): the forced getter `git::`, the
repository URL, `//` and a path inside the repository, and `?ref=` with a
branch, tag or commit:

```
git::https://github.com/acme/commons.git//capabilities/gcp-key.capability.md?ref=v1.4.0
```

**Decision:** only the `git::` getter, only `https://` (and `file://`,
for tests; an executor MUST refuse `file://` outside tests), and only
the `ref` query parameter are allowed (GF035). go-getter's detector
shorthands (`github.com/acme/commons`), `ssh`, `depth` and `sshkey` are
not: one spelling per location keeps identity a string comparison, and
credentials are the reader's, never the reference's.

8.1.2. **Anchors.** `#<anchor>` is this format's own suffix, not part of
the go-getter string: a reader splits the reference at its **first**
`#` before parsing the rest. An anchor names a requirement id in a
design, or a heading's GitHub-style slug in any Markdown grainfile
(GF030 if absent).

8.1.3. **Folder references** are the same form with a trailing `/` on
the path: `git::https://github.com/acme/widgets.git//.grain/?ref=main`.
A folder reference resolves when the directory exists (GF030
otherwise). A repository with no `//` path means the repository root.

8.1.4. **Within a folder**, relative paths (from the referring file's
directory) and `/`-rooted paths (from the referring file's grain
folder's top; **Decision**) are shorthand for the same repository at the
same commit. They are unversioned: they always mean "as of the commit
being read". A relative reference MUST NOT climb above its repository's
root (GF036).

8.1.5. **Aliased references** `alias:path` use an alias declared in the
referring folder's `grain.config.md` (§8.2); an undeclared alias is
GF033.

### 8.2 Versioned aliases

Aliases are **the one place a grain folder pins its dependencies**.

```yaml
aliases:
  commons:
    source: "git::https://github.com/acme/commons.git?ref=v1.4.0"
    sha256: "h1:Kq3v…="
```

8.2.1. `source` is a remote reference (repository, optionally `//path`,
optionally `?ref=`). `commons:capabilities/gcp-key.capability.md`
expands to the source with the path appended to its `//` path:
`git::https://github.com/acme/commons.git//capabilities/gcp-key.capability.md?ref=v1.4.0`.
An alias name MUST NOT be `git`, `https` or `file`.

8.2.2. `sha256`, when present, is the expected content of the aliased
tree. **Decision:** it is Go's `dirhash` `Hash1` (`h1:` and the base64
SHA-256 of the sorted lines `<hex sha256 of file>  <path>\n`, as
`golang.org/x/mod/sumdb/dirhash.HashDir(dir, "", Hash1)` computes it),
over the files of the source's `//` path at its `ref`, paths relative to
that directory, symlinks and submodules excluded. It is reused rather
than invented, it is independent of git's object format, and a reader
can compute it from any checkout. A reader MUST compute it after
fetching and MUST refuse every reference through an alias whose tree
does not match (GF043).

8.2.3. **One version per repository per folder.** Within one grain
folder, two aliases whose sources name the same repository (compared in
canonical form, §8.6, without `//` path and `?ref=`) at different refs
is an error (GF039). A remote reference to a repository an alias
already names, instead of through the alias, is a warning (GF038).

8.2.4. **Pinning.** A remote reference (or alias source) with no
`?ref=`, or whose ref is a branch, is **unpinned**: a warning by
default, an error when `check.pinning: required` (GF037).
**Decision:** pinning applies to references to grainfiles and to alias
sources, not to `context:` entries, which name the code a run works on
and are resolved when it runs (the record pins what ran), nor to a
record's own pinned fields. A pinned ref
is a 40-hex commit or a tag. **Decision:** a linter without network
access treats a ref as a tag when it begins `refs/tags/` or matches
`v?<digits>(.<digits>)*` with an optional `-` or `+` suffix, and every
other non-commit ref as a branch.

8.2.5. **Bumping** a dependency is a one-line change to an alias's
`ref` (and `sha256`). A `bump` tool (non-normative) would resolve the
new ref, recompute `h1:` and rewrite the line, preserving the file
(§5.5).

### 8.3 Resolution

To resolve a reference from a grainfile at commit C of repository R:

1. Split off any `#anchor` (§8.1.2).
2. Expand an alias (§8.2.1); expand a relative or `/`-rooted path to R
   at C.
3. For a remote reference, fetch the repository at `ref` (absent: the
   default branch), using credentials the reader holds for that host
   only (§16.3). For an alias with `sha256`, check it (§8.2.2).
4. Read the path. If nothing is there: for `<name>.<kind>.md` try the
   container form `<name>.<kind>/<kind>.md` (§4.3.3); then a tombstone
   `<path minus .md>.moved.md`, following `to` (§9.3).
5. If an anchor was given, check it.

A reference that fails is GF030, never a guess. Within one
resolution every `?ref=` is read once, at one commit.

### 8.4 Fields that expect a kind

A reference in a field that expects a kind MUST resolve to that kind
(GF032): `depends_on` and `supersedes` on a definition → `task`;
`capabilities` → `capability`; `lifecycle` → `lifecycle`; `fires.template`
→ `template`; `fires.definition` → `task`; `implements[].ref` → `design`;
a record's `definition` → `task` or `template`; `in_reply_to` →
`comment`; `deployment.offers` → `capability`.

### 8.5 Container forms

A reference to `<name>.task.md` or `<name>.capability.md` resolves the
bare file or the folder form; a reference to `<name>.record/` names a
record. Identity (§8.6) uses the `<name>.<kind>.md` form in both cases.

### 8.6 Canonical form

The **canonical reference** of a grainfile is its remote reference
written as `git::https://<host>/<repository path>//<path in repository>`
with the host lower-cased, any default port and trailing `.git` removed
from the repository path, no `.` or `..` segments, and the
`<name>.<kind>.md` form for containers. Identity (§9.1), tombstones,
capability matching (§12) and execution records (§13) compare canonical
references; a record adds `?ref=<sha>` (a *pinned ref*).

8.7. **Interoperability** (non-normative). A reader MAY emit a Package
URL (`pkg:github/acme/commons@<sha>#capabilities/gcp-key.capability.md`)
for provenance tooling; purl is not a reference form of this format.

## 9. Identity, moves and tombstones

9.1. A grainfile's identity is its canonical reference without `?ref=`:
its location. There is no id field.

9.2. **Moves.** A move within one repository leaves **no tombstone**: the
writer updates every reference in the repository in the same commit, and
a linter catches any it missed (GF030). Archiving is such a move, into
any directory (conventionally `archive/`). A move **to another
repository** MUST leave a tombstone `<old path minus .md>.moved.md` whose
`to` names the new canonical reference. Tombstones never change and are
never deleted (GF052). A new grainfile MUST NOT be created at a path a
tombstone holds (GF052).

9.3. **Following.** A reader follows at most 8 tombstones per
resolution; a loop or a longer chain is GF031.

9.4. **No grainfile is deleted** once a reference to it may exist
outside its repository; it is moved or superseded instead. Assets MAY be
deleted.

## 10. `grain.config.md`

One file at a grain folder's top, and the only configuration file of a
grain folder. Its body is a human description of the folder.

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `format` | integer | Req. | `1` (§3.1). |
| `title` | string | Opt. | |
| `owner` | *identity* | Req. | Owns the folder: may change anything `GRAINOWNERS` does not give away, and is the only identity that may change `owner` (§14.3). |
| `aliases` | map alias → `{source, sha256}` | Opt. | §8.2. |
| `check` | `{pinning, limits}` | Opt. | `pinning`: `warn` (default) or `required` (§8.2.4). `limits`: lowers §4.4. |
| `deployment` | `{account, offers}` | Opt. | Present when the folder is a deployment's: `account` is its forge identity, `offers` the capability references it offers (§12). |
| `settings` | *settings* | Opt. | §6.4. |

```yaml
---
format: 1
title: Grain eu
owner: "@acme/grain-admins"
aliases:
  commons: {source: "git::https://github.com/acme/commons.git?ref=v1.4.0", sha256: "h1:…"}
check: {pinning: required}
deployment:
  account: "@acme-grain-eu"
  offers: ["commons:capabilities/gcp-key.capability.md"]
settings:
  hub: https://silo.example.com
---
```

Refused (GF020): `role`, `backlog`, `lanes`, `triggers`, `memory`,
`registry`, `folders`, `views`, `lifecycle`, `task_home`, `moved_to`,
`archive_repo`, `enforcement`, `identity_map`. There is no
`.grainconfig`; a file of that name is an asset.

## 11. Templates

### 11.1 The dialect

11.1.1. Templates are **Jinja2** (the Jinja project's "Template Designer
Documentation" for the 3.x series), with the default delimiters,
`autoescape` off, `trim_blocks` and `lstrip_blocks` off, and
`keep_trailing_newline` on. Everything Jinja2 defines is available
except what §11.1.2 removes.

11.1.2. **Sandboxed and deterministic.** A renderer MUST render in
Jinja2's immutable sandbox (`jinja2.sandbox.ImmutableSandboxedEnvironment`
or an equivalent that refuses attribute access to internals and
mutation), with undefined names an error (`StrictUndefined`). The
`random` filter and the `lipsum` global are removed, and no global,
filter or test is added. The output for given inputs MUST be the same
on every renderer.

11.1.3. **Limits.** A renderer MUST stop with GF065 at 256 KiB of output
or 10,000 loop iterations in one render, and at an include depth of 16.

11.1.4. A template that fails to parse, uses something removed, names an
undefined variable or fails to render is GF065.

### 11.2 Forms

- **Single-task template** (`form: task`): one grainfile
  `<name>.template.md`. Rendering it yields one definition: its `title`
  and body rendered, its other fields copied.
- **Folder template**: a directory `<dir>/` whose top holds the manifest
  `<dir>/<dir>.template.md` with `form: folder` (GF068). Every
  definition inside it (`*.task.md` or `*.task/`) is a **member**.
  Rendering yields one definition per member, with the manifest's
  `context`, `capabilities`, `lifecycle` and `settings` added to each.
  Members' `depends_on` and other references MUST resolve inside the
  template folder (GF061) and MUST NOT form a cycle (GF062), so a
  rendering is a closed graph of work. A template folder MUST NOT
  contain another manifest (GF066), and a `form: task` template MUST NOT
  sit at a template folder's top (GF068).

What an engine does with rendered definitions (queues them, runs them,
writes them somewhere) is its business; its records name the template
or member and the inputs (§13.2).

### 11.3 Where rendering happens

Only a template's or member's **title and Markdown body** are rendered.
**No other front-matter field and no path is rendered.** **Decision:**
this keeps inputs out of everything a reader acts on, so an input can
change what an agent reads but never what runs, depends on or is
authorised. `{% include %}`, `{% import %}` and `{% extends %}` name
files by path relative to the grain folder's top; they MUST stay inside
the grain folder (GF061) and are read at the same commit. **Decision:**
included files are plain assets (conventionally under `partials/`):
there is no partial kind.

### 11.4 Inputs

An input declaration is `{name, type, required, default, options,
description}`:

| Field | Meaning |
| --- | --- |
| `name` | `[a-z][a-z0-9_]*`, unique. |
| `type` | `string` (default), `integer`, `boolean`, `enum` (REQUIRES `options`), `list` (of strings) or `ref`. |
| `required` | Default `false`. |
| `default` | A value of `type`. |

Values are checked against their types before rendering; a missing
required input, a wrong type or an `enum` value outside `options` is
GF063. `ref` inputs are bound only into `context:` entries that say
`input: <name>`, after being resolved like any reference; they are also
available to the body as their string.

### 11.5 Updates

A rendering is an independent copy. Editing a template changes nothing
already rendered; records pin the template's commit.

## 12. Capabilities

12.1. A **capability** is authority a run uses beyond its files: a cloud
service account, a package registry, a write credential. In this format
a capability is only a **name**: a `capability` grainfile whose identity
is its canonical reference (§8.6). Its title and description are for
people; its body and `assets/` (a Terraform module that provisions it,
say) are never interpreted by the format or its readers.

12.2. **Requests and offers.** Definitions and templates **request**
capabilities by reference in `capabilities:`. A deployment's
`grain.config.md` lists the capabilities it **offers** in
`deployment.offers`. A request matches an offer when both resolve to
the same canonical reference.

12.3. **Matching.** An executor MUST NOT run work unless every
capability it requests matches one its deployment offers. A request that
does not resolve is an error (GF030); an offer that does not resolve is
a warning (GF081); a record whose work requested a capability its
deployment's folder does not offer is GF082.

12.4. **Policy is local.** What a capability grants, to which runs, with
which secrets and approvals, is the deployment's local policy, outside
this format. Secrets never appear in grainfiles.

12.5. `GRAINOWNERS` MAY restrict who adds a request with a field rule on
`capabilities` (§14.2).

## 13. Execution records

### 13.1 What a record is

A record is written **once, by the deployment that ran the work, after
the execution completed**. It is a container `<name>.record/`:

```
<name>.record/
  record.md      # front matter (13.2) and a free-form Markdown body
  comments/      # the execution's process log, one *.comment.md per entry
  assets/        # anything else worth keeping
```

**Decision:** `<name>` SHOULD be the compact UTC `started_at` followed by
`-` and a slug of the work (`20261017T090000Z-tidy-run-notes`).

13.1.1. **Location** is the deployment's choice: any grain folder it may
write, any directory in it. The format defines the record, not its
placement.

13.1.2. **Immutability.** **Decision:** `record.md`, and every comment
written with it, never change once committed (GF052). Comments MAY be
**appended** afterwards (new files in `comments/`, never edits), so a
person or the deployment can annotate an outcome ("this failure was the
flaky runner"); an appended comment's `at` is after `finished_at`, which
is how readers tell the process log from later discussion. The record
is a log, not a state.

### 13.2 Fields

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Req. | |
| `definition` | *pinned ref* | Req. unless `task` | The definition, single-task template or folder-template member that ran, as a canonical reference with `?ref=<40-hex sha>` (GF083 otherwise). |
| `task` | `{title, prompt}` | Req. unless `definition` | **Decision:** work that had no definition carries its text inline, `prompt` being the whole prompt as given. Exactly one of `definition` or `task` (GF083). |
| `inputs` | mapping | Opt. | Input values used. |
| `trigger` | *pinned ref* | Opt. | The trigger that fired it. |
| `outcome` | enum (13.2.1) | Req. | |
| `deployment` | *identity* | Req. | The deployment's forge account. |
| `started_at`, `finished_at` | *time* | Req. | `finished_at` not before `started_at` (GF020). |
| `links` | `{pull_request, branch, commit}` | Opt. | URLs (`https://`), a branch name, a 40-hex commit. |

13.2.1. **Outcomes** (closed): `succeeded`, `failed`, `cancelled`,
`nothing_needed`, and, **Decision:** `refused`, for work the deployment
would not start (a capability not offered, a reference that did not
resolve, policy), since that is neither a failure of the work nor a
person's cancellation. An interrupted execution that the deployment
retried and finished has one record; one it gave up on is `failed`.

13.2.2. **Body.** Free-form Markdown: summary, links, anything else.

### 13.3 The process log

`comments/` holds the execution's process log: one comment per entry
(§7.2), authored by the deployment's account or by the people who
replied to it, in time order by name.

### 13.4 Derived indexes

Definition → its records, design requirement → implementing definitions,
and definition → dependents are **derived indexes**: an engine or a hub
builds them by reading records and definitions. No grainfile points from
a definition to its records, or from a design to its tasks.

## 14. Write authority

### 14.1 The model

Repositories live on forges, and **the forge's protected default branch
is authoritative**. **Decision:** there is one enforcement mode:

1. A grain folder's repository MUST protect its default branch, with
   direct pushes off and the status check `grainfiles/authorize` (the
   **forge check**) required.
2. The forge check runs the linter over the head tree, then authorises
   the change against `GRAINOWNERS` (§14.4).
3. **Readers act only on commits of the protected default branch**, or
   on commits reachable from a tag or commit an alias pins. A reader
   SHOULD confirm the protection through the forge's API where it can.

A deployment is its forge account; everything it writes (records,
comments) is authored and pushed as that account, through the same
check.

### 14.2 `GRAINOWNERS`

14.2.1. **Placement.** In any directory of a grain folder; patterns are
relative to its directory and govern only paths in its grain folder.

14.2.2. **Grammar.** **Decision:** CODEOWNERS lines, plus optional field
rules and a create-only marker:

```abnf
line    = [ rule ] [ "#" *char ] / blank
rule    = pattern [ ":" field *( "," field ) ] 1*WSP owners
pattern = gitignore-pattern            ; CODEOWNERS semantics
field   = LOWER *( LOWER / DIGIT / "_" / "." ) / %s"body"
owners  = owner *( 1*WSP owner ) / %s"@none"
owner   = identity [ %s"(create)" ] / "*" [ %s"(create)" ]
```

An owner is `@login` or `@org/team` (§6.3; host-qualified identities are
GF071). `*` is any forge identity the forge lets open a pull request.
`(create)` allows only adding new files. `@none`: nobody. A malformed
line is GF070.

14.2.3. **Precedence.** For a changed path, the governing file is the
nearest `GRAINOWNERS` (closest ancestor inside the grain folder) with a
matching line; in it, the last matching whole-path line gives the
path's owners, and for each field the last matching line naming it
overrides those for that field. A path with no governing rule may be
changed only by the folder's `owner` (GF072, warning).

14.2.4. **Example** (a deployment's folder):

```
*                       @acme/grain-admins
/records/               @acme-grain-eu
/records/**/comments/   *(create)
/tasks/                 @acme/hub
/tasks/**/thread/       *(create)
/tasks/:capabilities   @acme/security
```

### 14.3 Fixed rules

Whatever `GRAINOWNERS` says:

- a comment's `author`, a new definition's `submitter`, a tombstone's
  `moved_by` and a record's `deployment` are the actor;
- records, comments and tombstones never change once committed (GF052);
- a fold (§4.3.3) is authorised by permission to create the child it
  adds;
- `grain.config.md`'s `owner` changes only by its `owner`;
- `GRAINOWNERS` changes only by the owners its own governing rule names.

### 14.4 The forge check

Inputs: the base tree (the protected branch's tip), the head tree, and
the **actor**, the forge identity that authored the pull request, with
its team memberships. Algorithm:

1. Lint every grain folder the change touches (§15); any error fails.
2. Read `GRAINOWNERS` and `grain.config.md` **from the base tree**, so a
   change cannot widen its own authority.
3. For each added, changed or deleted path, and for each changed field
   of a changed grainfile, require the actor to be an owner (GF073), and
   apply §14.3 (GF052).

The check MAY also accept the forge's approving reviews from owners, as
CODEOWNERS does.

### 14.5 Compiling to CODEOWNERS

A writer MAY compile `GRAINOWNERS` to the forge's CODEOWNERS so the
forge requests reviews from owners. **Decision:** the compile copies
whole-path rules with identity owners verbatim; field rules, `*` owners
and `(create)` markers have no CODEOWNERS equivalent and are enforced by
the check alone.

## 15. Errors and lint

15.1. A linter reports each condition with its code, severity, path and a
message. The reference linter's messages are informative.

15.2. **Severity.** An *error* fails the forge check and makes the
grainfile or the reference unusable to readers. A *warning* fails
nothing.

| Code | Severity | Condition |
| --- | --- | --- |
| GF001 | error | A grainfile or comment name violates §4.1 or §7.2. |
| GF003 | error | Malformed: no front matter, unclosed delimiter, not UTF-8, NUL, LFS pointer, YAML that does not parse. |
| GF004 | error | Front matter outside the YAML subset (§5.3). |
| GF005 | warning | A Markdown file with an unregistered kind suffix. |
| GF006 | error | A limit of §4.4 exceeded, or two paths differ only in case. |
| GF007 | error | A reserved name misused (§4.3): a file a container does not allow, a nested container, a fixed name out of place, a comment outside `thread/` and `comments/`, `grain.config.md` not at a folder top. |
| GF010 | error | Nested grain folder. |
| GF011 | error | `grain.config.md` inside a template folder. |
| GF020 | error | Schema validation failure: a field's type, a required field, a refused field, an unknown trigger source or gate requirement. |
| GF021 | error | `format` is not a version the reader knows. |
| GF022 | warning | Unknown field (§3.3). |
| GF030 | error | A reference does not resolve (missing, unreadable, bad anchor, missing folder). |
| GF031 | error | Tombstone loop or more than 8 hops. |
| GF032 | error | A reference resolves to the wrong kind. |
| GF033 | error | Undeclared alias. |
| GF034 | error | `depends_on` cycle. |
| GF035 | error | Malformed reference: not a go-getter string of §8.1, a getter other than `git::`, a scheme other than `https`/`file`, a query parameter other than `ref`. |
| GF036 | error | A reference climbs above its repository root. |
| GF037 | warning | An unpinned or branch-pinned remote reference or alias source (§8.2.4); an error when `check.pinning: required`. |
| GF038 | warning | A remote reference to a repository an alias names. |
| GF039 | error | Two aliases name one repository at different refs. |
| GF043 | error | An alias's tree does not match its `sha256`. |
| GF045 | error | A container lacks its fixed file, or a grainfile exists both as a file and as a container. |
| GF050 | error | Invalid lifecycle (§7.9). |
| GF052 | error | A forbidden change: a record, comment or tombstone changed, a path a tombstone holds reused. |
| GF056 | error | A design requirement's `hash` differs from its computed hash. |
| GF057 | warning | A stale `implements` edge: its `hash` differs from the requirement's. |
| GF061 | error | A template member's reference, or an include, leaves the template folder or grain folder. |
| GF062 | error | A cycle among a folder template's members' `depends_on`. |
| GF063 | error | A required input missing, or a value of the wrong type or outside `options`. |
| GF065 | error | A template fails to render in the sandbox (§11.1). |
| GF066 | error | A template folder contains another manifest. |
| GF068 | error | A `form: folder` manifest is not `<dir>/<dir>.template.md`, or a `form: task` template is at a template folder's top. |
| GF070 | error | `GRAINOWNERS` syntax error. |
| GF071 | error | `GRAINOWNERS` names an owner that is not a forge identity of §14.2.2. |
| GF072 | warning | A grainfile has no governing `GRAINOWNERS` rule. |
| GF073 | error | Unauthorised change of a path or field (forge check). |
| GF081 | warning | A capability a deployment offers does not resolve. |
| GF082 | error | A record's work requested a capability its deployment does not offer. |
| GF083 | error | A record's `definition` is not pinned to a 40-hex commit, or it has both or neither of `definition` and `task`. |

## 16. Security considerations

16.1. **Identity comes from the forge.** Git author and committer fields
are claims; authorisation uses the forge-verified actor (§14.4), and
author fields in front matter are checked against it.

16.2. **Unprotected branches** hold proposals. Readers act only on the
protected branch and on what aliases pin (§14.1).

16.3. **Credentials** are bound to hosts: a reference cannot make a
reader send one forge's token to another, and references never carry
credentials (§8.1.1).

16.4. **Pinning.** An unpinned dependency changes under a reader without
review; `check.pinning: required` and alias hashes make every remote
input reviewed and reproducible.

16.5. **Templates** render in Jinja2's sandbox with limits (§11.1), and
inputs reach only titles and bodies (§11.3), so a hostile input cannot
add a dependency, a capability request or a path. A rendered body is a
prompt and is still untrusted text to an executor.

16.6. **Capabilities** are names; a request is never a grant. A
deployment decides what an offer grants (§12.4).

16.7. **Assets are data**: never executed or interpreted by a reader.

16.8. **Denial of service.** §4.4's limits bound parses, renders and
tombstone chains; readers fail loudly rather than truncating.

## 17. Registries of identifiers

17.1. **Kinds:** `config`, `task`, `comment`, `template`, `trigger`,
`gate`, `capability`, `design`, `lifecycle`, `moved`, `record`.
(`suite`, `ref`, `placement`, `exec`, `run`, `memory`, `asset` and
`capability-base` are retired and MUST NOT be reused.)

17.2. **Trigger sources:** `schedule`, `push`, `merge`, `pull_request`,
`design-change`, `manual`; reserved: `task-closed`, `task-state`.

17.3. **State categories:** `proposed`, `ready`, `active`, `waiting`,
`done`, `cancelled`.

17.4. **Gate requirements:** `human`, `security_review`,
`second_review`.

17.5. **Record outcomes:** `succeeded`, `failed`, `cancelled`,
`nothing_needed`, `refused`.

17.6. **Adding identifiers** takes a revision of this specification,
which updates this section, the schemas and the linter together.

17.7. **Retired lint codes:** `GF002`, `GF023` (per-file `kind`/`schema`),
`GF040` (orphans), `GF041`, `GF042` (pointer checks), `GF044`
(`moved_from`), `GF046`, `GF047`, `GF048`, `GF051`, `GF053`, `GF054`,
`GF055`, `GF064`, `GF067`, `GF069`, `GF074` (executions, lanes, live
state and root entries), `GF080` (`.grainconfig`), `GF090` (suites).
They MUST NOT be reused.

## Appendix A. The worked example

[`examples/grainfiles/`](../examples/grainfiles/README.md) is a tree of
example repositories; `python3 tools/grainfiles/grainlint.py
examples/grainfiles` checks them all.

- **`commons/`**: a shared library with
  `capabilities/gcp-key.capability/` (the capability, and a Terraform
  module in its `assets/`), a lifecycle and a Jinja partial.
- **`widgets/`**: a code repository whose `.grain/` holds a design with
  requirement hashes, a definition implementing one requirement, with a
  thread of two comments, and a gate.
- **`grain-eu/`**: a deployment's folder: `grain.config.md` offering
  `gcp-key` and pinning commons through a versioned alias with an `h1:`
  hash; a folder template using a loop and an include; a cron trigger;
  and two execution records with process-log comments, one `succeeded`
  with a pull-request link and one `failed`.

Broken fixtures, one per lint code, are under
`tools/grainfiles/testdata/bad/`, generated by
`tools/grainfiles/testdata/mkfixtures.py`.

## Appendix B. Decisions made by this specification

1. Unknown fields are a warning, with `x_` extensions (§3.3).
2. Grainfile stems are lower-case ASCII (§4.1.3).
3. A definition (or capability) with no thread or assets may be a bare `<name>.<kind>.md`, folded into `<name>.<kind>/` by its first child, with no tombstone (§4.3.3).
4. Forge identities use CODEOWNERS syntax, `@login` and `@org/team`, with `@host:name` for other forges, never matched by `GRAINOWNERS` (§6.3).
5. `schedule.catch_up` is `latest-only` (default) or `none`; schedules always skip overlapping firings (§7.5.1).
6. `per` defaults to `event` (§7.5.2).
7. Only the `git::` getter, `https://` (and `file://` for tests) and the `ref` query parameter are allowed in references (§8.1.1).
8. A `/`-rooted reference is rooted at the referring file's grain folder (§8.1.4).
9. Alias `sha256` is Go's `dirhash` `h1:` over the aliased directory (§8.2.2).
10. An offline linter tells tags from branches by pattern; pinning does not apply to `context:` entries (§8.2.4).
11. Rendering happens only in titles and bodies (§11.3).
12. Included Jinja files are plain assets; there is no partial kind (§11.3).
13. Record folders are `<name>.record/` with `record.md`, `comments/` and `assets/`, named by start time and slug (§13.1).
14. Records never change, but comments may be appended after completion (§13.1.2).
15. Work with no definition carries `task: {title, prompt}` inline (§13.2).
16. The outcome set adds `refused` (§13.2.1).
17. One enforcement mode: the required `grainfiles/authorize` check on a protected default branch (§14.1).
18. `GRAINOWNERS` keeps field rules and adds `*` and `(create)`; only whole-path identity rules compile to CODEOWNERS (§14.2.2, §14.5).

## Appendix C. Migration from grain's `.grain/` declarations

Grain's repo declarations map onto this format as follows. Every
execution setting moves into `settings:`, whose keys grain's own
documentation defines; the names below are grain's current keys, carried
unchanged.

| grain today | Grainfile Format |
| --- | --- |
| `.grain/templates/<name>.yml` (`title`, `body`, `fields`) | `<name>.template.md`, `form: task`: `title`, the body as Markdown, `fields` as `inputs`. Grain's `{{name}}` placeholders are valid Jinja2; braces meant literally go in `{% raw %}`. |
| a template's `model`, `network`, `nothing-needed`, `auto-merge`, `base`, `team`, `sandbox` (`cpus`, `memory-mb`, `disk-gb`), `push` | `settings:` |
| a template's `capabilities: [gcp-key, …]` | `capabilities:` references to capability grainfiles a library publishes (`gcp-key`, `package-publish`, `github-sandbox`, …); `repo-read:`/`repo-write:` (grain's `reads`/`writes`) become `context:` entries |
| `.grain/suites/<name>.yml` | a folder template `<name>/<name>.template.md`, members from the suite's `templates:`; `mode`, `count`, `max-passes`, `require-approval`, `auto-merge`, `auto-approve-proposals`, `team` in the manifest's `settings:` |
| `.grain/schedules/<name>.yml` with `every`/`daily`/`weekly`/`monthly` and `timezone` | a trigger with `on.schedule.cron` and the same `timezone` (grain's default is UTC, as here); `overlap: skip` is the only behaviour; `template`/`suite` become `fires.template`, `workflow` stays `fires.workflow`, `base` goes to `settings:` (or `fires.branch` for a workflow) |
| `.grain/settings.yml` | `grain.config.md` `settings:` |
| grain's gates | `gate` grainfiles |
