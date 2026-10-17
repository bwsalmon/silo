# The Grainfile Format, version 1

**Status:** draft for review (bsalmon@google.com). Normative.

This document specifies the Grainfile Format: how Grain's work (task
definitions, task executions and their runs, backlogs, comments,
templates, triggers, gates, capabilities, designs, memory and
lifecycles) is kept as plain files in git, how those
files refer to each other, who may change them, and how a reader
discovers, resolves and checks them. It is written so that a conforming
reader, writer, executor, hub, forge check or linter can be built from it
alone.

The rationale, the alternatives and the history are in
[`docs/grainfiles.md`](grainfiles.md), the design. The two agree; where
they ever disagree, **this document wins** and the design is corrected.
The machine-readable parts are the JSON Schemas in
[`schemas/grainfiles/`](../schemas/grainfiles/) and the reference
validator, [`tools/grainfiles/grainlint.py`](../tools/grainfiles/grainlint.py).
The worked example of Appendix A is
[`examples/grainfiles/`](../examples/grainfiles/README.md).

Where this specification had to choose something the design left open,
the choice is marked **Decision:**, and every decision is listed in
Appendix B.

## Contents

1. Conventions
2. Terminology and conformance classes
3. Format versioning
4. Files, names and folder boundaries
5. Grainfile syntax
6. Common fields
7. The kinds
8. Identity
9. References
10. `grain.config.md` and the repo settings file
11. One-to-many relations
12. Lifecycles
13. Folder templates and instantiation
14. `context:` materialisation
15. Capabilities
16. Write authority
17. Registry and hub behaviour
18. Grain folders in code repos, and task homes
19. Processing model
20. Errors and lint
21. Security considerations
22. Registries of identifiers
- Appendix A. A complete worked example
- Appendix B. Decisions made by this specification

## 1. Conventions

1.1. The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT",
"SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY" and
"OPTIONAL" are to be interpreted as described in BCP 14 (RFC 2119,
RFC 8174) when, and only when, they appear in all capitals.

1.2. Grammars use ABNF (RFC 5234, RFC 7405 for case-sensitive strings).
`ALPHA`, `DIGIT`, `HEXDIG` and `VCHAR` are RFC 5234's core rules;
`LOWER = %x61-7A`.

1.3. "Front matter" is the YAML mapping at the head of a grainfile (§5).
A field is written `name`, a nested field `parent.child`, and a list
item's field `list[].field`.

1.4. Times are RFC 3339 timestamps in UTC with a `Z` suffix
(`2026-10-16T13:58:30Z`). A "compact UTC time" is the same instant as
`YYYYMMDDTHHMMSSZ` (`20261016T135830Z`).

1.5. Lint codes (`GF001`…) are defined in §20. A sentence that says "is
an error (GF030)" means a linter MUST report that code, and every other
conformance class MUST treat the condition as §20.2 says.

## 2. Terminology and conformance classes

### 2.1 Terms

**grainfile**
: One object, kept as one UTF-8 Markdown file named `<name>.<kind>.md`
  (§4), with front matter (§5). `grain.config.md` is a grainfile of kind
  `config`.

**asset**
: Any file in a grain folder that is not a grainfile: an image, a YAML
  workflow, a `README.md`, a Markdown file with no known kind suffix
  (§7.14).

**grain folder**
: A directory that contains a file named `grain.config.md`. That file is
  the folder's root definition (§10). A grain folder is either a whole
  repository or a subdirectory of one. A repository MAY contain several
  grain folders; grain folders MUST NOT nest (§4.6).

**grain repo**
: A repository whose root directory is a grain folder.

**template folder**
: A directory inside a grain folder whose top holds a folder-template
  manifest (§13.1). Its contents are inert (§13.2).

**deployment** (grain deployment)
: One running Grain. Each deployment has exactly one **root grain
  folder**, whose `grain.config.md` has the deployment form (§10.2).
  Everything else a deployment uses is reached from that file, by root
  entry or by reference. This specification never says "instance" for a
  deployment.

**registry**
: A grain folder whose `grain.config.md` has the registry form (§10.3):
  the list of grain folders a hub knows about.

**forge**
: The git host a repository lives on (GitHub, GitLab, depot/GHOP, a bare
  `git daemon`, …). The forge's protected branch is authoritative for the
  repository's content (§16.1).

**forge identity**
: The account the forge authenticated for a push or a pull request: the
  only identity this specification uses for authorisation (§16.4).

**definition** (task definition)
: A `task` grainfile (§7.1): what to do. It holds no state and no runs.
  It is a file `<name>.task.md`, or a folder `<name>.task/` holding
  `task.md` and the definition's own `thread/` and `assets/` (§4.3).
  It may live in any grain folder.

**execution** (task execution)
: One folder `<name>.exec/` (§7.2) in a lane of the executing
  deployment's root grain folder: `exec.md` (provenance, lifecycle state,
  approvals), a snapshot copy of the definition (`task.md`), the
  execution's own `thread/`, one folder per attempt under `runs/`, and
  `assets/`. Everything that happens to that piece of work is inside it;
  nothing about an execution spans repositories.

**container**
: A definition folder or an execution folder. Its children are found by
  listing it, never by pointers (§11).

**principal**
: A forge-independent name for who acts (§6.3), mapped to forge
  identities by the identity map (§16.5).

### 2.2 Conformance classes

An implementation MAY claim several classes. Each class's requirements
are the sentences that name it, plus §3, §4, §5 and §9 for every class.

| Class | What it does | Must implement |
| --- | --- | --- |
| **reader** | Reads grain folders: lists root entries, parses grainfiles, resolves references. | §3–§11, §12.2 (category lookup), §19 |
| **writer** | Creates or changes grainfiles. | reader; §5.5 (round-trip), §6, §7, §8, §13.3 (if it instantiates), §16.6 |
| **executor** | A deployment: acts on its root grain folder, dispatches executions, writes their runs, comments and state. | reader, writer; §10.2, §11, §12, §14, §15, §16.7, §16.8 |
| **hub** | Silo: the human-facing reader and writer, and the policy decision point for its UI. Creates executions, keeps derived indexes and a registry. | reader, writer; §11, §16.6, §16.8, §17 |
| **forge check** | The required status check that gates a protected branch: validates a change and authorises it against `GRAINOWNERS`. | linter; §16 |
| **linter** | Reports the conditions of §20 for a tree, or for a change between two trees. | reader; §20 |

A **validator** is a linter. The reference linter is
`tools/grainfiles/grainlint.py`; its behaviour is informative where it
differs from this text.

## 3. Format versioning

3.1. **The format version** is the major version of this specification:
`1`. Every grainfile declares its kind's schema identifier in `schema:`:

```abnf
schema-id = "grain/" kind "@v" 1*DIGIT      ; grain/task@v1
```

The `kind` in `schema` MUST equal the file's `kind:` (GF023). The digits
are the kind's major version. In format version 1 every kind is at `v1`.

3.2. **Compatible additions.** A later revision of this specification MAY
add optional fields, enum values in fields this document marks *open*,
kinds, root types and state categories (§22) without changing a
schema's major version. Revisions are numbered `1.1`, `1.2`, …; a
grainfile does not declare a revision.

3.3. **Unknown fields.** A front-matter field that the declared schema
version does not define is *unknown*.

- A reader and an executor MUST ignore an unknown field when deciding
  anything.
- A writer that rewrites a grainfile MUST preserve every unknown field,
  its value and its position (§5.5).
- A writer MUST NOT add an unknown field, except one whose name begins
  with `x_` (an extension field). Extension fields are never defined by
  this specification.
- A linter MUST report an unknown field that does not begin with `x_` as
  a warning (GF022), and MUST NOT report an extension field.

The JSON Schemas are strict (`additionalProperties: false`, with
`patternProperties` admitting `^x_`); the linter maps the resulting
"additional property" failures to GF022 rather than GF020. **Decision:**
strict schemas plus a warning, so that a typo is visible, while a file
written by a newer revision still loads.

3.4. **Two places refuse what they do not know**, rather than ignoring it,
because ignoring would silently change what runs or what lands:

- a trigger's `on:` source (§7.7): a trigger with an unknown source MUST
  NOT fire;
- a gate's `requires:` key (§7.8): a gate with an unknown requirement
  MUST hold every change it matches.

3.5. **Breaking changes** are a new major version (`grain/task@v2`). A
reader MUST refuse a grainfile whose schema major version it does not
know (GF021) and treat it as unreadable. A reader that knows `v2` SHOULD
keep a decoder for `v1` and upgrade on read; a writer MUST NOT rewrite a
file only to bump its version.

3.6. **Negotiation.** An executor advertises the schema versions it knows
to its hub (over the task interface's `hello`). A hub MUST NOT write a
grainfile in a version newer than every reader of the folder it writes
advertises.

3.7. **Field spelling.** Front-matter keys are `snake_case`. For kinds
`template` and `trigger`, a reader MUST also accept the hyphenated keys
of grain's `.grain/` declarations (`auto-merge`, `require-approval`,
`auto-approve-proposals`, `max-passes`) as aliases of the `snake_case`
keys; a writer MUST write the `snake_case` form. A file that sets both
spellings of one key is an error (GF020). Trigger sources keep their
hyphenated names (`design-change`).

## 4. Files, names and folder boundaries

### 4.1 Grammar

```abnf
grainfile-name = stem "." kind ".md"
stem           = segment *( "." segment )          ; may itself contain dots
segment        = 1*( LOWER / DIGIT / "-" / "_" / UPPER-ASSET )
UPPER-ASSET    = %x41-5A                           ; see 4.2
kind           = LOWER *( LOWER / DIGIT / "-" )    ; registered in §22.1
tombstone-name = stem "." kind ".moved.md"
container-name = stem ( %s".task" / %s".exec" )     ; a directory, §4.3
fixed-name     = %s"task.md" / %s"exec.md" / %s"run.md" ; §4.1.3
run-name       = compact-utc [ "-" 1*( LOWER / DIGIT / "-" ) ]
config-name    = %s"grain.config.md"
owners-name    = %s"GRAINOWNERS"
settings-name  = %s".grainconfig"
```

4.1.1. A file name is parsed **from the right**: the last `.md`, then the
longest registered kind suffix immediately before it. `x.template.moved.md`
is a tombstone (kind `moved`) for `x.template.md`; `x.asset.md` is a
sidecar (kind `asset`); `report.png.asset.md` is the sidecar of
`report.png`.

4.1.2. The **name** of a grainfile is its stem. `4310-carry-approver` is
the name of `4310-carry-approver.task.md`.

4.1.3. **Fixed names.** Inside a container the kind is given by a fixed
file name instead of a suffix, and the name is the container's stem:

| Path | Kind | Name |
| --- | --- | --- |
| `<name>.task/task.md` | `task` (a definition) | `<name>` |
| `<name>.exec/exec.md` | `exec` | `<name>` |
| `<name>.exec/task.md` | `task` (the execution's snapshot, §7.2) | `<name>` |
| `<name>.exec/runs/<run>/run.md` | `run` | `<run>`, a `run-name` |

A fixed name anywhere else, a file with suffix `.exec.md` or `.run.md`,
and a container nested in a container are errors (GF007). An execution
is always a folder and a run always `runs/<run>/run.md`.

### 4.2 Characters, case and length

- A grainfile's stem MUST match `[a-z0-9][a-z0-9._-]*`, or begin with a
  compact UTC time (`20261016T135830Z`) followed by `[a-z0-9._-]*`
  (GF001): lower-case ASCII, digits, `.`, `-`, `_`. **Decision:** no upper case in
  grainfile names, so that names are identical on case-insensitive
  file systems.
- An asset's name MAY use any Unicode in NFC except `/`, `\`, NUL and
  control characters, and MUST NOT be `.` or `..`. A sidecar's stem
  inherits its asset's name and is exempt from the lower-case rule.
- A path segment is at most 255 bytes, and a grainfile's path within its
  grain folder at most 1024 bytes (GF006).
- Two paths in one grain folder that differ only in case are an error
  (GF006).

### 4.3 Reserved names

| Name | Meaning | Rule |
| --- | --- | --- |
| `grain.config.md` | the grain folder's root definition | Only at a grain folder's top. Its presence makes the directory a grain folder. |
| `GRAINOWNERS` | write ownership (§16.2) | In any directory of a grain folder. |
| `.grainconfig` | the repo settings file (§10.5) | Only at a repository's root. |
| `archive/` | archived grainfiles (§10.6) | A directory of this name anywhere; never listed by a root entry. |
| `.grain/` | the default grain folder of a code repo (§18) | A convention, not a rule: it is a grain folder only if it holds `grain.config.md`. |
| `<name>.task/` | a definition folder (§7.1, §11.2) | Holds `task.md`, `thread/`, `assets/` and `GRAINOWNERS`, nothing else. |
| `<name>.exec/` | an execution folder (§7.2, §11.3) | Holds `exec.md`, `task.md`, `thread/`, `runs/`, `assets/` and `GRAINOWNERS`, nothing else. MUST be in a lane of a deployment's root grain folder (GF047). |
| `thread/` in a container | the container's thread (§11.2) | Holds only `*.comment.md`, and each comment's attachments in `<comment name>.attachments/`. |
| `runs/<run>/` in an execution | one attempt (§11.3) | Holds `run.md`, that attempt's `*.comment.md`, and assets. |
| `assets/` in a container | the container's attachments | Holds only assets and their sidecars. |
| `*.moved.md` | a tombstone (§8.3) | Never changed, never deleted. |

`thread/`, `runs/` and `assets/` are reserved only directly inside a
container. A file whose kind is not allowed where it is placed MUST NOT
be there (GF007); in particular a comment outside every `thread/` and
run folder is an error, and readers MUST NOT attach it to anything.
A container's required files missing (`exec.md`, the snapshot
`task.md`, a run folder's `run.md`, a definition folder's `task.md`), or
a definition present both as `<name>.task.md` and `<name>.task/`, is
GF045.

**Decision:** a definition with no discussion and no assets MAY stay a
single file `<name>.task.md`. When its first comment or asset arrives,
the writer **folds** it: in one commit it moves `<name>.task.md` to
`<name>.task/task.md` byte for byte and adds the comment. A fold is not a
move: it leaves no tombstone, the definition's identity is unchanged
(§9.3 step 5 resolves either form), and the forge check authorises it
under the rule that lets the actor create the comment (§16.2.6). Writers
SHOULD reference a definition as `<name>.task.md` in either form, so a
fold changes no reference. A folder is never unfolded.

### 4.4 Kinds and assets

4.4.1. A file is a grainfile exactly when its name ends in `.<kind>.md`
for a kind registered in §22.1, or is `grain.config.md`. A grainfile
MUST have front matter (GF003) whose `kind:` equals its suffix (GF002).

4.4.2. Every other file is an asset. Front matter alone never makes a
grainfile. A Markdown file whose second-to-last suffix is not a
registered kind (`x.widget.md`) is an asset, and a linter SHOULD warn
(GF005).

4.4.3. A grainfile MUST NOT be a Git LFS pointer (GF003). An asset MAY
be.

### 4.5 Limits

A reader MUST support at least, and MAY refuse beyond (GF006):

| What | Limit |
| --- | --- |
| a grainfile | 1 MiB; front matter 64 KiB |
| an asset in git | 10 MiB (larger: an external sidecar, §7.14) |
| grainfiles listed by one root entry | 2,000 (`limits.objects_per_entry`) |
| grainfiles listed by one deployment | 20,000 (`limits.objects`) |
| redirect hops | 8 (§9.4) |
| capability chain depth | 8 (§15.2) |
| template render | 256 KiB of output, 10,000 loop iterations |
| files materialised by one template instantiation | 500 |

`limits:` in `grain.config.md` or `.grainconfig` MAY lower these and
MUST NOT raise them.

### 4.6 Folder boundaries and nesting

4.6.1. A grain folder's **extent** is its directory's subtree. Paths in
a grain folder (in `GRAINOWNERS`, in `/`-rooted references, in root
entries) are relative to its top.

4.6.2. **No nesting.** A directory inside a grain folder MUST NOT contain
`grain.config.md` (GF010). A reader that finds one MUST treat the inner
directory as part of the outer folder and MUST NOT treat it as a grain
folder.

4.6.3. A **template folder** MUST NOT contain `grain.config.md` at any
depth (GF011).

4.6.4. Files of a repository outside every grain folder are not
grainfiles for any purpose of this specification, whatever their names,
except as targets of a reference (they are then assets) and except
`.grainconfig` (§10.5).

## 5. Grainfile syntax

5.1. **Encoding.** UTF-8, without a byte-order mark; LF or CRLF line
endings (a writer SHOULD write LF). NUL is forbidden (GF003).

5.2. **Delimiters.** The file MUST begin with the line `---`. Front
matter is every following line up to the next line that is exactly
`---` (trailing spaces allowed). The **body** is everything after that
line. A file with no closing delimiter is malformed (GF003).

5.3. **The YAML subset.** Front matter is YAML 1.2 restricted to:

- a single document whose top level is a mapping;
- keys that are plain strings (GF004);
- no duplicate keys at any level (GF004);
- no anchors, aliases or merge keys (`&`, `*`, `<<`) (GF004);
- no tags (`!!`, `!x`) (GF004);
- scalars: strings, integers, `true`/`false`, `null`, and floats only
  where a schema says `number`.

A writer MUST quote any string that YAML 1.1 would read as another type
(`on`, `off`, `yes`, `no`, `y`, `n`, times like `09:00`). In particular
the trigger key `on` MUST be written `"on"`. A reader MUST read a bare
key `on` as the string `on`.

5.4. **The body** is CommonMark with GitHub-flavoured tables. It carries
what a human reads (a task's prompt, a design's prose). Links in a body
are for humans and are not references (§9), except that `context:`
`depth:` follows them (§14.4) and a design's requirement anchors are
headings (§7.10).

5.5. **Round-trip.** A writer that changes a grainfile MUST change only
the fields it means to change: it MUST preserve the order, comments and
spelling of every other key and the body byte-for-byte.

5.6. **Malformed files.** A grainfile that fails §5.1–§5.3 is
*unreadable*. Readers MUST NOT act on an unreadable grainfile, MUST
report it, and MUST continue with the rest of the folder. A reference to
it resolves to an error (GF003), never to a guess.

## 6. Common fields

6.1. Every grainfile has these fields. "Req." is REQUIRED; "Opt." is
OPTIONAL.

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `kind` | string, a registered kind (§22.1) | Req. | MUST equal the file's kind suffix (GF002). |
| `schema` | schema-id (§3.1) | Req. | The kind's schema and major version. |
| `aka` | list of strings | Opt. | Identities from before migration (`grain:eu/task/4310`, `repo:template:o:r:n`). Indexed for search, never resolved (§8.5). |
| `moved_from` | list of references | Opt. | Earlier addresses of this grainfile, oldest first (§8.3). |
| `instantiated_from` | string, `<ref>@<sha>` | Opt. | Set on every execution an instantiation created (§13.3). |
| `x_*` | any | Opt. | Extension fields (§3.3). |

There is **no `id` field**: a grainfile is identified by its path (§8). A
file that sets `id` is an error (GF020).

6.2. **Types used below.**

| Type | Definition |
| --- | --- |
| *ref* | a reference string (§9.1) |
| *pinned ref* | a ref whose `@gitref` is a 40-hex commit |
| *edge* | a mapping with `ref` (a *ref*) plus edge fields, such as `{ref, rev}` |
| *sha* | 40 lower-case hex digits |
| *time* | §1.4 |
| *principal* | §6.3 |
| *rank* | `[0-9a-z]*[1-9a-z]`, compared bytewise (§7.2) |
| *state* | a state name of the execution's lifecycle (§12) |

6.3. **Principals.**

```abnf
principal    = "user:" email
             / "team:" team-id "#" team-role
             / "deployment:" dep-id
             / "agent:" dep-id "/" run-path
             / "hub" [ ":trigger" ]
             / "authenticated"
team-role    = %s"member" / %s"approver" / %s"admin" / %s"maintainer"
dep-id       = LOWER *( LOWER / DIGIT / "-" )
```

**Decision:** a deployment is `deployment:<id>` (the design's `grain:<id>`
is accepted by readers as an alias), and the hub is `hub` (the design's
`silo` is accepted as an alias). `agent:` names the agent of one run; it
is an author of comments, never an owner in `GRAINOWNERS`.

## 7. The kinds

Each subsection lists the kind's fields beyond §6.1, then a minimal
example. The JSON Schema `schemas/grainfiles/<kind>.schema.json` is the
machine form; where it and this text disagree, this text wins and the
schema is a bug.

### 7.1 `task` (definition)

What to do, written once where it was proposed; its body is the prompt.
A definition **holds no state and no runs**: its lifecycle state,
approvals and runs are its executions' (§7.2). It is `<name>.task.md`,
or `<name>.task/task.md` when it has a thread or assets (§4.3). It MAY
live in any grain folder: an intake, a code repository's `.grain/`, a
shared library, a deployment's own folder. A folder template's members
are definitions (§13).

| Field | Type | Req. | Default | Meaning |
| --- | --- | --- | --- | --- |
| `title` | string | Req. | | One line. |
| `submitter` | *principal* | Req. | | Who proposed it. |
| `created_at` | *time* | Req. | | |
| `submitted_via` | enum `git` `api` `ui` `cli` `trigger` `gate` `agent` `template` | Opt. | `git` | |
| `lifecycle` | *ref* to a `lifecycle` | Opt. | | The lifecycle its executions use, unless the execution's lane names one (§12.5). |
| `team` | string | Opt. | | Owning team. |
| `context` | list of context entries (§14) | Opt. | `[]` | What a run's sandbox holds. |
| `template` | *edge* `{ref, rev, inputs}` | Opt. | | The single-task template it was rendered from, when a person proposed it from one. |
| `depends_on` | list of *ref* to definitions | Opt. | `[]` | Mapped to executions when executions are created (§11.4, §13.3). |
| `implements` | list of *edge* `{ref, rev, hash}` | Opt. | `[]` | Design requirements (§7.10). |
| `constraints` | mapping `{labels, deployment}` | Opt. | | Where its executions may be created. |
| `filed_by` | `{deployment, run, principal}` | Opt. | | An agent's upward filing. |
| `supersedes` | *ref* to a definition | Opt. | | The definition this one replaces. |
| `origin` | string | Opt. | | An external origin (`owner/repo#N`). |
| `labels` | list of strings | Opt. | | |
| `auto_merge` | boolean | Opt. | | |

**Definitions do not change once executed.** A definition SHOULD NOT be
changed after an execution has been created from it: the execution runs
from its snapshot (§7.2), so the edit would change nothing that runs and
would make the definition disagree with what ran. A linter warns on a
definition whose bytes differ from the `definition_sha256` an execution
recorded (GF046). The forge check MAY refuse such a change unless the
change carries an explicit override (**Decision:** a
`Grain-Definition-Edit: <reason>` trailer on one of its commits). A
changed request is normally a new definition with `supersedes:`. The
definition's `thread/` and `assets/` are not part of the definition and
change freely.

**Refused fields** (GF020): `id`, `repo`, `base`, `reads`, `writes`,
`capabilities`, `closed`, `placement`, and the fields that are an
execution's (`state`, `approved_by`, `approved_at`, `cancel`,
`rejection`) or a pointer to children (`thread`, `runs`, `placements`).

```yaml
---
kind: task
schema: grain/task@v1
title: Carry a hub task's approver on its offer
submitter: user:bsalmon@google.com
created_at: "2026-10-16T09:00:00Z"
context:
  - {ref: "https://github.com/bwsalmon/silo.git@main", mode: write}
---
Carry `approved_by` from the execution onto its offer and its attestation.
```

### 7.2 `exec` (execution)

`<name>.exec/exec.md`: the front matter of one execution of one
definition on one deployment. The folder is the execution (§4.3, §11.3):

```
<name>.exec/
  exec.md        # this kind
  task.md        # the snapshot: a copy of the definition when the execution was created
  thread/        # *.comment.md: discussion of this execution
  runs/          # one folder per attempt: <run>/run.md and that attempt's comments
  assets/        # attachments
```

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `definition` | *ref* `@` *sha* | Req. | The definition (or single-task template, or folder-template member) the snapshot was copied from, pinned to the commit read. |
| `definition_sha256` | `sha256:` + 64 hex | Req. | The SHA-256 of the definition file's bytes at that commit. |
| `inputs` | mapping | Opt. | Input values substituted into the snapshot at creation (§13.5). |
| `instantiated_from` | *ref* `@` *sha* | Opt. | The folder-template manifest, when created by instantiation (§13.3). |
| `state` | *state* | Req. | The lifecycle state of this execution (§12). |
| `lifecycle` | *ref* to a `lifecycle` | Opt. | §12.5. A writer copies the definition's lifecycle here, pinned, when it has one. |
| `created_by`, `created_at` | *principal*, *time* | Req. | Who created the execution (placed the work). |
| `created_via` | enum `git` `api` `ui` `cli` `trigger` `template` `agent` | Opt. | |
| `rank` | *rank* | Req. | Order within the lane. |
| `priority` | enum `urgent` `high` `normal` `low` | Opt. (`normal`) | |
| `expedite` | boolean | Opt. (`false`) | Sorts ahead of every priority in its lane. |
| `ranked_by`, `ranked_at` | *principal*, *time* | Opt. | |
| `depends_on` | list of *ref* to executions | Opt. | What dispatch waits on (§12.3). |
| `approved_by`, `approved_at` | *principal*, *time* | Opt. | Set by the transition out of the initial `proposed`-category state into a `ready`-category state. MUST differ from the definition's `submitter` unless the lifecycle sets `self_approval: true`. |
| `capability_approvals` | list of `{ref, digest, by, at}` | Opt. | §15.4. |
| `cancel`, `rejection` | `{by, at, reason}` | Opt. | `cancel` REQUIRED when `state`'s category is `cancelled` and the state is not `rejected`. |
| `previous` | *ref* to an execution | Opt. | The execution this one re-places (§11.5). |
| `superseded` | `{at, by, reason}` | Opt. | Set by this execution's own deployment when it is re-placed; `by` names the new execution. |
| `trigger` | *edge* `{ref, on, key, fired_at, event}` | Opt. | The firing that created it (§7.6). |
| `attestation` | JWS compact string | Opt. | The hub's signed claims. |
| `labels` | list of strings | Opt. | |

**The snapshot.** Creating an execution copies the definition into
`task.md`. For a definition that is a `task`, the copy is byte for byte,
so `task.md` hashes to `definition_sha256` (GF045 otherwise). For a
template or a folder-template member, the copy is the rendered task:
inputs substituted into body and title (§13.5), with the template's
`context`, `lifecycle` and copied fields. The execution runs from its
snapshot, **never from the live definition**. Relative references in a
snapshot are read as they were in the definition, relative to the
definition's path at `definition`'s commit, never relative to the copy.
The definition's thread is not copied: the execution reaches it through
`definition` (§11.2).

**Fixed at creation** (GF052 in the forge check): `task.md`,
`definition`, `definition_sha256`, `inputs`, `instantiated_from`,
`previous`, `created_by`, `created_at`.

**References out of an execution** SHOULD be `/`-rooted or carry a
locator or alias, so that moving the folder between lanes (§11.3)
changes none of them; references between executions created together
(an instance, §13.3) are relative and move together.

**Effective order within a lane** is: executions whose dependencies are
satisfied (§12.3) first; then `expedite: true`; then `priority`; then
`rank` bytewise; then the path. An execution not in a `ready`-category
state, or whose snapshot is missing or unreadable, is skipped and
reported, not dispatched.

```yaml
---
kind: exec
schema: grain/exec@v1
definition: "hub:intake/tasks/2026-10-16-approval-flow.task.md@0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
definition_sha256: "sha256:a83c…"
lifecycle: "hub:intake/lifecycles/security.lifecycle.md@0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
state: sec-approved
created_by: user:dev@example.com
created_at: "2026-10-16T11:50:00Z"
rank: "t"
approved_by: user:sec@example.com
approved_at: "2026-10-16T12:00:00Z"
---
```

### 7.3 `run` (attempt record)

`<name>.exec/runs/<run>/run.md`: what one attempt of the execution did,
a final-state snapshot updated at coarse status changes. `<run>` is the
compact UTC time the attempt was accepted, with an optional `-<suffix>`
(GF001). Only the execution's deployment writes it (§16.7).

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `deployment` | dep-id | Req. | The executor; MUST be the deployment whose folder holds the execution. |
| `status` | enum (below) | Req. | Run status. |
| `accepted_at` | *time* | Req. | |
| `started_at` | *time* | Opt. | |
| `waiting` | `{on, entry, reason}` | Opt. | REQUIRED when `status` is `awaiting_reply` or `held`. |
| `refusal` | `{reason, at, entry, capability, reducer, secret}` | Opt. | REQUIRED when `status` is `refused`. |
| `context` | `{materialised, skipped}` | Opt. | §14.6. |
| `capabilities` | list | Opt. | §15.6. |
| `superseded` | `{at, by, reason}` | Opt. | REQUIRED when `status` is `superseded`. |
| `closed` | `{outcome, at, summary, change, design_revs, follow_ups}` | Opt. | REQUIRED when the status is terminal. |
| `proposed_transition` | `{to, applied, at}` | Opt. | The execution transition its outcome proposed (§12.6). |

**Run status** (open: §3.2) is `accepted`, `running`, `awaiting_reply`,
`held`, `done`, `failed`, `interrupted`, `refused`, `cancelled`,
`superseded`. The terminal ones are `done`, `failed`, `interrupted`,
`refused`, `cancelled`, `superseded`; a terminal record MUST NOT change
(GF052). `closed.outcome` is one of `done`, `failed`, `nothing_needed`,
`cancelled`, `interrupted`. Run status is never a lifecycle state
(§12.1). At most one run of an execution is open (`accepted`, `running`,
`awaiting_reply`, `held`) at a time (GF053).

Refused (GF020): `task` and `from`, the back-pointers of the earlier
model; the run's folder says which execution it belongs to.

```yaml
---
kind: run
schema: grain/run@v1
deployment: eu
status: running
accepted_at: "2026-10-16T15:00:00Z"
---
```

### 7.4 `comment`

A comment is in exactly one of three places, and its place is its
parent (§11.2): a definition's `thread/`, an execution's `thread/`, or a
run folder `runs/<run>/`.

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `author` | *principal* (`user:`, `deployment:`, `agent:`, `hub`) | Req. | |
| `at` | *time* | Req. | |
| `via` | enum `git` `hub` `api` `forge` | Opt. | How it was written. |
| `in_reply_to` | *ref* to a comment | Opt. | May name a comment in another of the three places (an execution comment answering one on its definition). |
| `supersedes` | *ref* to a comment | Opt. | An edit; comments are append-only. |
| `attachments` | list of *ref* to assets | Opt. | Normally in `<comment name>.attachments/` beside it. |
| `question` | boolean | Opt. | An agent's question that holds its run (`awaiting_reply`); only in a run folder. |

Refused (GF020): `task` and `run`, the back-pointers of the earlier
model.

A comment's name MUST be `<compact UTC time>-<author slug>`, where the
author slug is the principal with every character outside `[a-z0-9]`
replaced by `-` (GF001). A comment MUST NOT change once written (GF052).

### 7.5 `template`

A template is a definition with inputs. Three forms, distinguished by
`form:` (§13.1):

- **`form: task`** (the default): a **single-task template**, a
  shorthand for a folder template holding one definition. Its body is
  the prompt. Instantiating it creates one execution (§13.4).
- **`form: folder`**: the **manifest** of a folder template. It MUST be
  named `<dir>.template.md` and sit at the top of directory `<dir>/`
  (GF068).
- **`form: partial`**: include-only text for `{% include %}`.

| Field | Type | Req. | Default | Meaning |
| --- | --- | --- | --- | --- |
| `form` | enum `task` `folder` `partial` | Opt. | `task` | |
| `title` | string (templated) | Req. for `task` | | The rendered task's title. |
| `inputs` | list of input declarations (§13.5) | Opt. | `[]` | |
| `context` | list of context entries | Opt. | | Copied into the snapshot, with `input:` entries bound. |
| `lifecycle` | *ref* | Opt. | | Copied, pinned, into each execution's `exec.md`. |
| `team` | string | Opt. | | |
| `model`, `network`, `nothing_needed`, `auto_merge`, `base` | as grain's `.grain/templates` | Opt. | | Copied into the snapshot (§13.8). |
| `run` | run policy (below) | Opt., `folder` only | | What grain's suites carried. |
| `target` | enum `lane` `path` | Opt., `folder` only | `lane` | Which lane instances go to: the one the request names, or a trigger's `fires.into` (§13.3). |

**Run policy** (`run:`), the fields of grain's `.grain/suites/*.yml`:

| Field | Type | Default | Grain's suite key |
| --- | --- | --- | --- |
| `mode` | enum `count` `until-clean` | `count` | `mode` |
| `count` | integer ≥ 1 | 1 | `count` (count mode) |
| `max_passes` | integer ≥ 1 | 5 | `max-passes` (until-clean mode) |
| `require_approval` | boolean | `false` | `require-approval` |
| `auto_merge` | boolean | `true` | `auto-merge` |
| `auto_approve_proposals` | boolean | `false` | `auto-approve-proposals` |
| `team` | string | the template's `team` | `team` |

A suite's `templates:` list is replaced by the folder's task files, and
a suite's `name:` by the manifest's `title:`. `repo:` and `target:` were
refused in a suite and stay refused in `run:` (GF020).

Refused in every form (GF020): `reads`, `writes`, `grants`,
`capabilities` (requests go in `context:` with `mode: use`).

### 7.6 `trigger`

"When X happens, file Y."

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Opt. | |
| `on` | mapping of source → options | Req. | Sources: `schedule`, `push`, `merge`, `pull_request`, `design-change`, `task-state` (§12.4), `manual`; `task-closed` is reserved and refused. Unknown sources: §3.4. |
| `fires` | mapping | Req. | Exactly one of `template` (a *ref* to a template of any form) or `workflow` (a path to a workflow in the trigger's own repo). With `template`: `inputs`, `bind`, `context`, `per` (`event` `requirement` `folder` `change`), `into` (the lane, in the firing deployment's root grain folder, its executions are created in, §13.3). With `workflow`: `branch`, `review: {template, inputs}`. |
| `fires_in` | enum `hub` `deployment` | Opt. (default `deployment`) | Who fires it. `workflow` requires `deployment`. |
| `enabled` | boolean | Opt. (default `true`) | |
| `team`, `auto_merge`, `auto_approve_proposals` | | Opt. | |
| `limits` | `{max_open, max_per_day}` | Opt. | |
| `derived_from` | *ref* to a trigger | Opt. | Provenance only. |

`on.schedule` holds exactly one cadence: `every: {hours}`, `daily: {at}`,
`weekly: {day, at}`, `monthly: {day, at}`, or `cron: "<5-field cron>"`;
plus `timezone` (IANA, default `UTC`), `overlap` (`skip` `allow`, default
`skip`) and `catch_up` (`none` `latest-only` `all`, default
`latest-only`). `on.merge` and `on.pull_request` hold `branches`,
`paths` (REQUIRED), `ignore` and `group_by` (`change` `folder`).
`on.design-change` holds `designs`, `classes`, `debounce`,
`max_tasks_per_change`.

Merge-gate keys (`gate`, `human`, `sensitive`, `requires`) are refused
(GF020): those are a gate's. Runtime state (`last_fire_at`,
`next_fire_at`, `paused`) is refused: it lives in the firer's database.

A firing creates executions, never definitions: a single-task template
fires one execution at `<into>/fired/<trigger name>/<event key>.exec/`,
and a folder template one instance at
`<into>/fired/<trigger name>/<event key>/` (§13.3); each records
`trigger:`. `into` is a lane: for `fires_in: deployment`, a lane path in
the firing deployment's own root grain folder (default: its first lane);
for `fires_in: hub`, a deployment's lane by folder locator, written by
the hub as `hub:trigger` under that folder's `GRAINOWNERS`. The event key
is deterministic, so a retried firing lands on the same path and is a
no-op.

### 7.7 Trigger sources are closed

A reader MUST NOT fire a trigger that names a source it does not know
(§3.4), and a linter MUST report it (GF020).

### 7.8 `gate`

A merge gate on the repository whose grain folder holds it. Read by that
repository's merge queue, never activated by a root entry (GF064 if a
root lists it).

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Opt. | |
| `match` | `{paths, paths_ignore, branches}` | Req. | `paths` REQUIRED. |
| `requires` | mapping | Req. | Keys: `human: {when: always\|on-findings\|never}`, `security_review: {template}`, `second_review: {template}`, `state: {in: [<state or category:…>]}` (§12.7). Unknown keys: §3.4. |
| `sensitive` | boolean | Opt. (default `false`) | |

Refused (GF020): `on`, `fires`, `template`, `reads`, `writes`, `grants`,
`target`.

### 7.9 `capability` and `capability-base`

§15 specifies both. `capability` fields: `title` (Req.), `base` (Req.:
`{type: <base type>}` or `{capability: <ref>}`), `reducers` (list),
`secrets` (slot → secret name), `approval` (`{by: [<principal>],
reason}`), `owners`. Refused: `grants`, `implies`, `maximum` (a
capability can only narrow). `capability-base` fields: `type` (Req.,
`<name>@v<n>`), `deployment` (Req.), `provider` (Req., `{code, rev}`),
`dimensions` (Req.), `materialises` (Req.), `implies` (Req.:
`{repos, egress, tools}`), `proxies`, `secrets`, `approval`,
`declared_at`.

### 7.10 `design`

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Req. | |
| `status` | enum `draft` `review` `approved` `superseded` | Req. | |
| `owners` | list of *principal* | Req. | |
| `requirements` | list of `{id, title, anchor}` | Req. | `id` matches `[A-Z][A-Z0-9]*-[0-9]+`. Each MUST have a heading in the body whose text begins with `id` (GF030). |
| `applies_to`, `supersedes`, `superseded_by`, `planning` | | Opt. | |

A requirement's **hash** is `sha256:` of its section's text (from its
heading to the next heading of the same or higher level), with line
endings normalised to LF and trailing whitespace removed.

### 7.11 `memory`

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `title` | string | Req. | |
| `applies_to` | `{repos, teams, kinds}` | Opt. | Default: every run. |
| `updated_by`, `updated_at` | *principal*, *time* | Opt. | |

Active only through a `memory` root entry (§10.2). Entries are in
precedence order; within one entry, a more specific `applies_to` wins,
then the path.

### 7.12 `moved` (tombstone)

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `to` | *ref* | Req. | The new address, relative to the tombstone. |
| `moved_by`, `moved_at` | *principal*, *time* | Req. | |
| `reason` | string | Opt. | |

### 7.13 `lifecycle`

§12 specifies it. Fields: `title` (Req.), `states` (Req.), `initial`
(Req.), `transitions` (Req.), `aliases` (Opt.), `on_outcome` (Opt.),
`self_approval` (Opt., default `false`), `stalled_after` (Opt.).

### 7.14 `asset` (sidecar)

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `asset` or `external` | *ref* to an asset, or `{url, sha256, bytes, media_type}` | Req., exactly one | `external.url` is `https://` (or `gs://`, `s3://` where a capability allows); `sha256` REQUIRED. |
| `title`, `description`, `alt`, `media_type`, `license`, `provenance`, `sha256`, `bytes` | | Opt. | `sha256` and `bytes`, when present, MUST match the blob (GF030). |

### 7.15 `config`

`grain.config.md`; §10.

### 7.16 Removed: `ref` and `placement`

The backlog reference (`kind: ref`, `*.ref.md`) and the placement
pointer (`kind: placement`, `<name>.placements/`) of the format's first
draft are not part of this format, and neither are the `<name>.thread/`
and `<name>.runs/` sibling folders: an execution folder replaces all four
(§11). A file with suffix `.ref.md` or `.placement.md` is an asset to a
reader and a warning to a linter (GF005); `ref` and `placement` are
retired kind names (§22.1).

### 7.17 Removed: `suite`

`kind: suite` and `.grain/suites/<name>.suite.md` are not part of this
format. A file with suffix `.suite.md` is an error (GF090), whose
message points at §13.8.

## 8. Identity

8.1. **Within a grain folder**, a grainfile is identified by its path
relative to the folder's top: `tasks/4310-carry-approver.task.md`.

8.2. **Globally**, it is identified by its **canonical reference**: the
folder locator of its grain folder (§9.2) without `@gitref`, then the
folder-relative path:

```
https://github.com/acme/hub.git//intake/tasks/4310-carry-approver.task.md
```

Canonicalisation: expand the alias; resolve `.` and `..`; drop a leading
`/` from the path; drop `@gitref` and `#anchor`; lower-case the URL's
scheme and host; remove a trailing `.git` and then add `.git` (so
`…/hub` and `…/hub.git` are one repository). Two references name the
same grainfile exactly when their canonical forms are equal after
following redirects (§9.4).

8.3. **Moves leave tombstones.** A grainfile that is renamed or moved
(within or across grain folders) MUST leave a `moved` grainfile at its
old path with the final `.md` replaced by `.moved.md`, and the new file
MUST list the old canonical reference in `moved_from` (GF044). A
tombstone MUST NOT be changed or deleted (GF052), so a tombstoned path is
never reused. A move within one repository MUST be one commit.

A container moves as a unit, with everything inside it; its tombstone is
`<name>.task.moved.md` (the same path a single-file definition's would
have) or `<name>.exec.moved.md`. **Decision:** moving an execution from
one lane to another of the same deployment (a `git mv` of its folder)
leaves **no** tombstone: an execution's name is unique within its
deployment's root grain folder (GF048), and a reference to it that finds
nothing at its path finds it by name in the deployment's lanes (§9.3
step 5). An execution never moves to another repository; re-placement
creates a new one (§11.5). Folding a definition (§4.3) is not a move.

8.4. **History.** A grainfile's history is its git history at its path
(and, through `moved_from`, its earlier paths). Nothing in the format
records history beyond that. No grainfile is deleted: a grainfile
leaves its path only by becoming a tombstone, by a lane move or fold
(above), or by archival (§10.6). Assets MAY be deleted, except an
execution's snapshot-time attachments and a definition's `assets/`
after an execution has been created from it.

8.5. **`aka`** values MUST NOT be used to resolve anything.

## 9. References

### 9.1 Grammar

```abnf
reference   = [ locator "//" ] rel-path [ "@" gitref ] [ "#" anchor ]
            / alias ":" rel-path [ "@" gitref ] [ "#" anchor ]
locator     = git-url                       ; a repository
git-url     = ( %s"https://" / %s"ssh://" / %s"file://" / %s"http://" ) 1*url-char
                                            ; no scp-style user@host:path
rel-path    = [ "/" ] *( segment-char / "/" )
gitref      = branch / tag / sha            ; the last "@" after the "//"
sha         = 40HEXDIG
anchor      = 1*( ALPHA / DIGIT / "-" / "_" )   ; a heading slug or a requirement ID
alias       = LOWER *( LOWER / DIGIT / "-" )
```

- The `//` separator is the first `//` after the URL's
  `scheme://authority`. `http://` is allowed only for `localhost` and
  for the address of a forge proxy a deployment's policy names.
- In a reference without a locator or alias, a path beginning `/` is
  rooted at the **referring file's grain folder**; any other path is
  relative to the referring file's directory. **Decision:** `/` roots at
  the grain folder, not the repository, so that a grain folder can be
  moved within its repository by changing only its locator.
- In a reference with a locator, `rel-path` is relative to the
  **repository** root; the grain folder is the one containing the path
  (§9.2).
- A folder reference ends in `/`.
- A reference MUST NOT contain whitespace, `\`, or a `..` that would
  climb above the repository root (GF036).

### 9.2 Locating a grain folder

A **folder locator** names a grain folder:

```abnf
folder-locator = git-url "//" [ folder-path ] [ "@" gitref ]
folder-path    = 1*( segment-char / "/" ) "/"     ; ends in "/", repo-relative
```

`https://github.com/acme/widgets.git//.grain/@main` is the grain folder
`.grain/` of the widgets repository at `main`;
`https://github.com/acme/hub.git//@main` (empty path) is a grain repo.
The directory named MUST contain `grain.config.md` (GF030). Registry
entries (§10.3) and the deployment's own root setting (§10.2.6) use this
form. The grain folder of a reference with a locator is the nearest
directory at or above the target that contains `grain.config.md`; a
target in no grain folder is an asset.

### 9.3 Resolution

Resolution is one function, `resolve(reference, referrer, gate)`.

1. **Expand the alias**, from the referrer's repository's `.grainconfig`
   (§10.5). An undeclared alias is an error (GF033), never a guess.
2. **Canonicalise** (§8.2), resolving a relative path against the
   referrer's directory and a `/` path against the referrer's grain
   folder.
3. **Choose the commit.**
   - A relative or `/`-rooted reference without `@gitref` reads the
     **same commit** as the referrer.
   - A reference with a locator and no `@gitref` reads the tip of the
     target repository's `default_ref` (§10.5) at the time of reading.
   - `@<branch>` reads its tip; `@<tag>` and `@<sha>` read exactly that.
   - A reader MUST act only on commits reachable from the target's
     protected branch (§16.7), unless the reference is pinned to a sha
     that the referrer's own protected history reviewed.
4. **Gate.** Call `gate(target)`; the reader's credential is always part
   of it, and for `context:` the executor's policy is too (§14.5). A
   refusal is an error (GF030, "not readable").
5. **Look up the path.** A folder reference to a container names its
   fixed file: `<name>.task/` is `<name>.task/task.md`, `<name>.exec/`
   is `<name>.exec/exec.md`. If the path exists, go to 7. Then, none
   counting as a hop: a missing `<name>.task.md` is looked up as
   `<name>.task/task.md` (a folded definition, §4.3); a missing
   `<name>.exec/` inside a deployment's root grain folder is looked up by
   name in each of that deployment's lanes (a lane move, §8.3), one
   listing per lane. If one of these exists, go to 7.
6. **Redirect.** In order, each counting as one hop except the last:
   1. the target repository's `.grainconfig` `moved_to`: retry at that
      URL with the same path;
   2. a tombstone beside the path (`<path minus .md>.moved.md`): follow
      its `to`, relative to the tombstone;
   3. the archive mapping (§10.6), not a hop.
   If none applies, the reference is **dangling** (GF030).
7. **Check the kind.** If the referring field expects a kind (§9.6) and
   the target's kind suffix differs, the reference is the wrong kind
   (GF032). An asset never satisfies a field that expects a kind.
8. **Anchor.** `#anchor` MUST match a requirement `id` of a design, or a
   heading slug (GitHub's algorithm) of a Markdown body (GF030).

The result is the target's canonical reference, the commit it was read
at, and its contents. A writer that records an edge for traceability
(`template.rev`, `implements[].rev`, `definition`, `instantiated_from`)
MUST record the commit actually used.

### 9.4 Redirect limits and cycles

A reader MUST follow at most 8 hops and MUST keep the set of canonical
identities visited; reaching one twice, or a ninth hop, is an error
(GF031) and the reference is treated as dangling.

`depends_on` cycles are an error (GF034): every task on the cycle is
blocked. Template include and import cycles are an error at render time
(GF065). Capability chains are bounded by §15.2.

### 9.5 Cross-forge credentials

A reader resolves each reference with the credential it holds for the
target's forge (a deployment's forge proxy, a hub user's delegated
token, a linter's token or none). It MUST NOT use a credential issued for
one host on another, and MUST NOT follow an HTTP redirect to another
host with a credential. A reference that needs a credential the reader
lacks is "not readable" (GF030); it is never resolved by guessing a
mirror.

### 9.6 Fields that expect a kind

| Field | Expected kind |
| --- | --- |
| `exec.definition` | `task` or `template` (a single-task template, or a member of a folder template) |
| `task.depends_on[]`, `task.supersedes` | `task` |
| `exec.depends_on[]`, `exec.previous`, `exec.superseded.by` | `exec` (a folder reference to `<name>.exec/`) |
| `task.template.ref`, `trigger.fires.template`, `exec.instantiated_from` | `template` |
| `task.lifecycle`, `exec.lifecycle`, lane `lifecycle` | `lifecycle` |
| `task.implements[].ref` | `design` (with `#anchor`) |
| `comment.in_reply_to`, `comment.supersedes` | `comment` |
| `capability.base.capability` | `capability` |
| `moved.to` | the same kind as the tombstone's |
| `asset.asset` | an asset (not a grainfile) |
| root entry `path` | a folder |

### 9.7 Errors

| Condition | Code | Effect |
| --- | --- | --- |
| malformed reference | GF035 | the field is invalid |
| undeclared alias | GF033 | unresolvable |
| missing, unreadable or redirect chain broken | GF030 | dangling: a dependency stays blocked; a template makes the execution uncreatable; an unresolvable `definition` is shown unresolved (the execution still runs from its snapshot); an `implements` is shown unresolved |
| redirect loop or over 8 hops | GF031 | dangling |
| wrong kind | GF032 | as dangling |
| `depends_on` cycle | GF034 | every execution on the cycle is blocked |

## 10. `grain.config.md` and the repo settings file

`grain.config.md` is the root definition of a grain folder, a
`kind: config` grainfile at the folder's top. It has exactly one of three
forms, chosen by `role:` (GF020 if missing).

**Decision:** a third form, `role: library`, for a grain folder that no
deployment is rooted at (a team's intake, a shared folder of templates, a
code repo's `.grain/`). Such folders need a boundary (for `GRAINOWNERS`,
for `/` references, for the no-nesting rule) and an entry in a registry,
and `grain.config.md` is what makes a directory a grain folder (§2.1).
A library folder activates nothing.

### 10.1 Fields of every form

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `role` | enum `deployment` `registry` `library` | Req. | |
| `title` | string | Opt. | |
| `owner` | `{user}` / `{team}` | Req. | Who owns the folder; the only principal who may change `role` and `owner`. |
| `lifecycle` | *ref* | Opt. | The default lifecycle: for definitions in this folder, copied onto their executions; in a deployment's folder, for its executions (§12.5). |
| `ignore` | list of gitignore patterns | Opt. | Paths in the folder that no reader lists (they are still assets). |
| `limits` | `{objects_per_entry, objects}` | Opt. | Lower §4.5. |

### 10.2 Deployment form

```yaml
---
kind: config
schema: grain/config@v1
role: deployment
deployment: {id: eu, hub: https://silo.example.com}
owner: {user: bsalmon@google.com}
backlog:
  mode: weighted                 # strict (default) | weighted | round_robin
  lanes:                         # highest priority first
    - name: hub
      path: hub/                 # where the hub creates executions
      weight: 3
    - name: own
      path: backlog/
      lifecycle: lifecycles/ops.lifecycle.md
triggers:
  - path: triggers/
memory:
  - path: memory/
    write: true
---
```

10.2.1. `deployment.id` (Req.) is the dep-id; `deployment.hub` (Opt.)
the hub it links to.

10.2.2. **Root entries.** There are exactly three root types: `backlog`
(its `lanes`), `triggers` and `memory`. An entry has `path` (Req.: a
folder *ref* ending `/`, or one file), `ref_mode` (`track` or `pin`;
REQUIRED when `path` has a locator or alias), and `ignore` (gitignore
patterns, relative to the entry's folder). A lane also has `name` (Req.,
unique), `weight` (weighted mode) and `lifecycle` (a *ref*).
**Decision:** a lane's `path` MUST be a folder of the deployment's own
root grain folder, never a locator or alias (GF047): executions are
written only in the executing deployment's repository, so a backlog move
never touches another repository. A memory
entry MAY have `write: true`; at most one entry, inside the root folder,
may.

10.2.3. **What an entry activates** is exactly: the files in its folder's
subtree of the root type's kind (for a lane, every `<name>.exec/`
folder; for the others, files with suffix `.trigger.md`, `.memory.md`),
minus the paths its `ignore` matches, minus anything under a
directory named `archive/`, minus anything inside a template folder
(GF064 if one would otherwise be listed), minus anything inside a
reserved folder (§4.3). Nothing is inherited; nothing is transitive.

10.2.4. **Overlap.** A file two entries of one type would activate
belongs only to the entry with the longer path. Entries of different
types never conflict.

10.2.5. **The reachability rule.** A deployment acts on a grainfile only
if it is activated by a root entry of its root grain folder's
`grain.config.md`, or is reached from such a grainfile by resolving a
reference (§9). A reference never activates anything; only root entries
do. A grainfile in the deployment's grain folder that is neither is an
**orphan** (GF040, warning), unless it is inside a container (§11), a
template folder or `archive/`, or is a kind that is reached only by
reference (`task`, `template`, `lifecycle`, `design`, `capability`,
`capability-base`, `gate`, `asset`, `moved`). **Decision:** definitions
are inert, like templates: a definition in a deployment's folder that no
execution names yet is not an orphan. An execution outside every lane
is not an orphan but an error (GF047).

10.2.6. **The root setting.** An executor's only local setting about
grain folders is its root grain folder's folder locator (§9.2) and
branch. It MUST refuse to start on a folder whose `grain.config.md` is
missing, unreadable, not `role: deployment`, or names another
`deployment.id`.

### 10.3 Registry form

```yaml
---
kind: config
schema: grain/config@v1
role: registry
registry: {hub: https://silo.example.com, poll_interval: 5m}
owner: {team: platform}
folders:
  - {name: hub-intake, at: "https://github.com/acme/hub.git//intake/@main", team: hub, role: library}
  - {name: eu, at: "https://github.com/acme/grain-eu.git//@main", role: deployment}
  - {name: widgets, at: "https://github.com/acme/widgets.git//.grain/@main", team: hub, role: library}
views:
  - {name: hub-definitions, folders: [hub-intake, widgets], kinds: [task]}
  - {name: hub-open, folders: [eu], kinds: [exec], categories: [proposed, ready, active, waiting]}
---
```

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `registry.hub` | URL | Req. | The hub that serves this registry. |
| `registry.poll_interval` | duration | Opt. (default `5m`) | §17.3. |
| `folders` | list of `{name, at, team, role}` | Req. | `at` is a folder locator with `@branch`. `name` unique. `role` MUST equal the target's `grain.config.md` `role` (GF032). |
| `views` | list of `{name, folders, kinds, categories, states}` | Opt. | Saved cross-folder views (§17.2). `categories` and `states` filter executions. |

Registering a folder is a commit to the registry (§17.1). A registry has
no lanes (`lanes:` is refused, GF020): every lane is a deployment's.

### 10.4 Library form

`role: library` with the fields of §10.1 only.

### 10.5 The repo settings file: `.grainconfig`

One YAML file (not a grainfile) at a repository's root, read by every
reader of any grain folder in the repository.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `schema` | `grain/repo-config@v1` | Req. | |
| `default_ref` | branch | `main` | What an unpinned locator reference resolves to; the protected branch. |
| `enforcement` | mapping (§16.3) | `{mode: reader}` | Which enforcement readers rely on. |
| `aliases` | map alias → git URL | `{}` | Used by references in this repository. |
| `identity_map` | *ref* to an identity map | | §16.5. |
| `moved_to` | git URL or null | null | Repository-level redirect. |
| `archive_repo` | git URL | | Second place the archive mapping looks. |
| `orphans` | enum `warn` `refuse` | `warn` | Whether the forge check fails on GF040. |
| `limits` | mapping | | Lower §4.5. |

A repository without `.grainconfig` has the defaults. **Decision:**
`host`, `ref_policy` and `grain_folder` from the design's earlier
`.grainconfig` are removed: hosting is the forge's (§16), and grain
folders are found by `grain.config.md`.

### 10.6 The `archive/` convention

A grainfile at `<top>/<rest>`, where `<top>` is the first directory of
its folder-relative path, MAY be moved to `<top>/archive/<rest>` with no
tombstone. Its identity is unchanged. A reference that finds nothing at
`<top>/<rest>` tries `<top>/archive/<rest>`, then the same path in
`archive_repo`. A new grainfile MUST NOT be created at a path whose
archive path exists (GF052). A container is archived whole: its folder
moves to the archive path with everything in it.

## 11. One-to-many relations

### 11.1 The principle: containment

**A parent contains its children.** Every one-to-many relation that
decides anything is a directory: a reader finds a parent's children by
listing the parent's own folder (§19.1). No grainfile points down to its
children, and no child points back up to its parent: there are no
`thread:`, `runs:` or `placements:` fields and no back-pointers, and so
no check that a pointer and a location agree.

| Parent | Children | Found by listing |
| --- | --- | --- |
| a definition `<name>.task/` | its comments | `thread/` |
| an execution `<name>.exec/` | its comments | `thread/` |
| an execution | its runs | `runs/` (each `runs/<run>/`) |
| a run | its comments (the agent's thread, and replies relayed to it) | `runs/<run>/` |
| a definition or an execution | its attachments | `assets/` |
| a comment | its attachments | `thread/<comment name>.attachments/`, and its `attachments` list |

A child is placed by being written in its parent's folder; it moves when
the parent moves; and a child outside every parent of its kind is an
error (GF007), never attached to anything by guesswork. A single-file
definition `<name>.task.md` has no children until it is folded (§4.3).

### 11.2 Two threads, two scopes

- **The definition's thread**, `<name>.task/thread/`, is about the
  definition itself: "this spec is wrong", design questions, the
  background of the request. Anyone `GRAINOWNERS` lets comment writes it,
  in the definition's repository.
- **The execution's thread**, `<name>.exec/thread/`, is about that one
  execution: approval discussion, "re-run this on candidate", what one
  deployment should do differently. It is written in the deployment's
  repository.
- **A run's comments**, in `runs/<run>/`, are the agent's conversation
  for that attempt: its reports, its questions (`question: true`), and
  people's replies to them, which the deployment relays and commits there
  (§16.7).

The definition's thread is never copied into an execution. A reader
showing an execution (Silo, grain, a CLI) MUST show both threads:

1. Read `exec.md` and list `thread/` and each `runs/<run>/`.
2. Resolve `definition` **without** its `@rev`, at the tip of the
   definition's repository (discussion is live; the prompt is the
   snapshot's), and list its `thread/` if it is a folder.
3. Label every comment with its **scope**: `definition`, `execution`, or
   `run <run>`; and show the definition scope as belonging to the
   definition, with a link to it, since other executions share it.
4. Either list the scopes as separate sections in that order, or merge
   them into one timeline ordered by `at`, then by scope in that order,
   then by name, keeping each comment's label. `in_reply_to` MAY cross
   scopes; a reader shows the replied-to comment's scope.
5. A definition thread the reader cannot read is reported as not
   readable (GF030), and the execution is still shown.

A definition's own page shows its thread, and lists its executions from
the derived index (§11.6), each a link, never their threads inline.

### 11.3 Executions, placement and moves

- **Placement is creating an execution folder** in a lane of the target
  deployment's root grain folder: one commit, in one repository, that
  writes `<lane>/<name>.exec/exec.md` and the snapshot `task.md` (§7.2).
  The hub does it through the forge API as the person placing the work
  (§16.6); a deployment does it for its own definitions and firings.
- `<name>` is the definition's name unless that name is already an
  execution of this deployment, in which case the writer appends `-2`,
  `-3`, … (GF048).
- **Reordering** within a lane changes `rank` (and `priority`,
  `expedite`) in `exec.md`: one file, in the one repository.
- **Changing lanes** is a `git mv` of the folder within the one
  repository, leaving no tombstone (§8.3), authorised by the `move` verb
  (§16.2.5).
- **Runs** are written only by the executing deployment, in
  `runs/<run>/`, one folder per attempt. Lifecycle state lives on
  `exec.md` (§12); run status lives on each `run.md` (§7.3).
- An execution never leaves its deployment's repository except by
  archival. Nothing about an execution is written in any other
  repository.

### 11.4 Dependencies

`depends_on` in `exec.md` names executions, and is what dispatch reads
(§12.3). A definition's `depends_on` names definitions, and is mapped
when executions are created:

- Executions created together from a folder template (§13.3) map each
  member's relative `depends_on` to the sibling execution created from
  that member.
- Otherwise the creator chooses, for each definition dependency, an
  execution of it (the hub offers the dependency's executions from the
  definition → executions index, §11.6) and writes it, as a locator
  reference when it is in another deployment. **Decision:** a creator
  MUST map every definition dependency or refuse to create the
  execution; it is never left implicit, because only `exec.md`
  `depends_on` decides anything.

### 11.5 Re-placement

To move work to another deployment:

1. The hub creates a **new execution** in the new deployment's lane,
   with the same `definition` (normally at the same `@rev`, so the
   snapshot is identical) and `previous:` naming the old execution by
   locator. **Decision:** the new execution carries the old one's
   `approved_by` and `approved_at` when the snapshot is identical, since
   the approval was of that content; otherwise it starts in its
   lifecycle's `initial` state.
2. The hub asks the old deployment, over its task interface, to
   withdraw. **Only the old execution's own deployment writes it**: it
   sets `superseded: {at, by, reason}` (`by` naming the new execution),
   moves the old execution to a `cancelled`-category state with
   `cancel`, and marks any open run `superseded`. A deployment that is
   gone is withdrawn by its root folder's owner, the only other writer
   `GRAINOWNERS` gives that folder.
3. **The fence.** The new execution's deployment MUST NOT dispatch it
   until the `previous` execution, read by reference, is superseded or
   in a `done`- or `cancelled`-category state with no open run. So two
   deployments never run the same work at once, and nothing is written
   in the new deployment's repository by the old or vice versa.

### 11.6 Derived indexes

These relations are **not** containment and are served by **derived
indexes** only:

- **definition → its executions** (from each `exec.md`'s `definition`);
- **design → implementing definitions** (from `implements`);
- **execution → dependents** and **definition → dependents** (from
  `depends_on`);
- template → instances (from `instantiated_from`); lifecycle →
  executions using it; asset → sidecars; `aka` → path.

**Why.** Each is many-to-many, or crosses repositories and ownership: a
definition's owners do not write in the deployments that execute it, a
design's owners do not write every task that implements it, and a
task's author does not own its dependents. Making any of them a folder
would force a write into a repository the writer does not own for every
new edge.

A **derived index** is a cache. It MUST be rebuildable from the grain
folders alone; it MUST be keyed by the git tree or blob hashes it was
computed from, so that a stale entry is detectable; and nothing that
decides what runs, what is authorised or what is cancelled may depend on
it. Dependency satisfaction is evaluated forwards, from the dependent's
`depends_on` (§12.3); traceability forwards from `implements`. The
indexes serve views ("this definition's executions"), lint warnings
(GF046 needs only the execution itself) and wake-ups ("a dependency of
yours is done; re-check"). Losing one costs a rebuild, never a wrong
answer. The hub keeps them over the registry's grain folders (§17.2); an
executor MAY keep the same kinds of cache for its root.

### 11.7 Other relations

| Parent | Navigation | Children |
| --- | --- | --- |
| a deployment | its `grain.config.md` root entries | lanes, trigger folders, memory folders |
| a registry | its `folders` list | registered grain folders |
| a lane (root entry) | listing the folder for `<name>.exec/` | its executions |
| a design | its `requirements` list and body headings | requirements |
| a trigger | the deterministic path `<into>/fired/<trigger name>/` | its firings (executions or instances) |
| a folder template | its directory | its member definitions |
| an asset → its sidecars | the sidecar beside it (`<asset>.asset.md`); others by derived index | sidecars |

### 11.8 Worked flows

**A person comments on a definition.** Through the hub, the person
writes `<name>.task/thread/<utc>-user-<slug>.comment.md` with
`author: user:…`, `via: hub`, in the definition's repository, with their
delegated token (§16.6). If the definition was a single file, the same
commit folds it (§4.3). The forge check verifies that the author matches
the forge identity (§16.4) and that `GRAINOWNERS` lets them create in
`thread/`.

**A person comments on an execution.** The same, at
`<name>.exec/thread/` in the deployment's repository.

**An agent comments during a run.** The deployment writes
`runs/<run>/<utc>-agent-<dep>.comment.md` with
`author: agent:<dep>/<execution path>/runs/<run>`, in its own root,
under its own identity. A question also sets `question: true` and moves
the run to `awaiting_reply`. A person's reply through the hub is relayed
to the deployment, which commits it in the same run folder with the
person as `author` and `via: hub`.

**Viewing an execution by listing only.** Read `exec.md` and `task.md`;
list `thread/`, `runs/` and each run folder; resolve `definition` and
list its `thread/`; label and merge (§11.2). Every step is a tree listing
of a known folder, in at most two repositories; no repository is
searched.

**Moving within a backlog.** Reorder: one commit changing `rank` in one
`exec.md`. Change lanes: one commit, a `git mv` of one folder. Both in
the deployment's repository only.

**Re-placement.** §11.5: one commit creating the new execution in the
new deployment's repository; one commit by the old deployment in its
own. No third repository is written.

**A misplaced comment caught by lint.** A `*.comment.md` in `notes/`, or
directly in `<name>.exec/`, is GF007; the forge check fails the change
that adds it.

### 11.9 Alternatives rejected

- **Pointers to child folders** (`thread:`, `runs:`, `placements:`) and
  back-pointers on children, the format's first draft: a backlog move
  touched several repositories, the hub had to write placement records
  into task repositories, and every pointer needed a lint (the retired
  GF041/GF042) to catch it disagreeing with where the child was.
- **Backlog references** (`*.ref.md`) in lanes pointing at tasks
  elsewhere, with hub-managed lanes in other repositories: re-placement
  was a three-step withdraw / new reference / new placement transaction
  across three repositories.
- **Back-reference scans.** Finding children by their back-pointers
  costs O(repository), or O(every repository a reader can see), and its
  answer depends on which repositories were searched.
- **A global comments folder.** One folder per repository for every
  comment needs a scan or an index to answer "this execution's
  comments", and mixes owners in one directory.
- **Executions that read the live definition.** An edit would change
  what a queued execution runs, after approval and without review in the
  deployment's repository; the snapshot makes what runs reviewable where
  it runs.

## 12. Lifecycles

### 12.1 Model

A `lifecycle` grainfile declares an execution's state machine.
**Lifecycle state** is the decision about one execution of the work
(proposed, approved, done, …) and lives on the execution, in `exec.md`'s
`state:`; a definition has none (§7.1). **Run status** (§7.3) is what
one attempt is doing and lives on run records only. A lifecycle MUST NOT declare run statuses.

```yaml
---
kind: lifecycle
schema: grain/lifecycle@v1
title: Security-reviewed work
initial: proposed
states:
  proposed:      {category: proposed}
  triage:        {category: proposed}
  sec-approved:  {category: ready}
  in-progress:   {category: active}
  blocked:       {category: waiting}
  shipped:       {category: done}
  dropped:       {category: cancelled}
transitions:
  - {from: proposed, to: [triage, dropped]}
  - {from: triage, to: [sec-approved, dropped]}
  - {from: sec-approved, to: [in-progress, dropped]}
  - {from: in-progress, to: [blocked, shipped, sec-approved]}
  - {from: blocked, to: [in-progress, dropped]}
aliases: {approved: sec-approved}
on_outcome:
  accepted: in-progress
  done: shipped
  failed: sec-approved
stalled_after: 72h
---
```

| Field | Type | Req. | Meaning |
| --- | --- | --- | --- |
| `states` | map state name → `{category, title}` | Req. | Names match `[a-z][a-z0-9_-]*`, opaque to Grain. |
| `initial` | state name | Req. | MUST be a `proposed`-category state. |
| `transitions` | list of `{from, to}` | Req. | `from` is a state name or `*`; `to` a list of state names. |
| `aliases` | map old name → state name | Opt. | Old names readers accept (§12.8). An alias MUST NOT equal a state name (GF054). |
| `on_outcome` | map run event → state name | Opt. | Transitions run events propose (§12.6). |
| `self_approval` | boolean | Opt. (`false`) | Whether a definition's submitter may approve its executions. |
| `stalled_after` | duration | Opt. (`72h`) | §12.4. |

A lifecycle is invalid (GF050) if a transition or `on_outcome` names an
unknown state, `initial` is missing or not `proposed`-category, a
category is not in §12.2, or no state has category `done`.

### 12.2 Categories

**Decision:** the closed set is exactly the six the design names:

| Category | Meaning to Grain |
| --- | --- |
| `proposed` | not yet accepted; never dispatched |
| `ready` | accepted and dispatchable |
| `active` | a deployment has accepted a run of it |
| `waiting` | not dispatchable and not finished: held on a human, a dependency, a failure streak, a merge |
| `done` | finished successfully; satisfies dependents |
| `cancelled` | finished without success (rejected, dropped, closed); never dispatched again |

No refinement was needed: grain's ten states (§12.9) all fit, and every
behaviour Grain has keys off one of these six. New categories are added
only by a revision of this specification (§22.3).

### 12.3 Behaviour keys only off categories

- **Dispatch.** An executor MAY dispatch an execution only when its
  state has category `ready`, all its dependencies are satisfied, and
  its `previous` (if any) is fenced off (§11.5).
- **Dependency satisfaction.** A dependency is satisfied when the
  execution it names is in a `done`-category state. A dependency in a `cancelled`-category state
  is **failed**: the dependent is not dispatched, is reported "blocked:
  dependency cancelled" (warning GF055), and stays
  blocked until a human removes the edge (a reviewed edit of the
  dependent's `exec.md` by its lane's owners) or reopens the
  dependency. **Decision:** cancelled never
  satisfies, because silently running work whose prerequisite was
  dropped is the worse failure.
- **Merge queue.** A change produced by a run is eligible for the merge
  queue while its execution is `active` or `waiting`; an execution that
  becomes `cancelled` withdraws its change from the queue.
- **Stalled detection.** An execution `active` longer than
  `stalled_after` with no run record change, or `ready` with no run for
  that long, is reported stalled (informational).
- **Exact names** are never consulted by these behaviours.

### 12.4 Exact names

Exact state names MAY be matched by:

- **triggers**: the source `on.task-state: {to: [<selector>], from:
  [<selector>], folders: [<folder ref>]}` fires when an execution in a
  listed deployment folder makes a matching transition;
- **gates**: `requires.state: {in: [sec-approved, "category:done"]}`
  holds a change until the execution that produced it is in one of
  those states;
- **`GRAINOWNERS` transition rules** (§16.2.4).

A *state selector* is a state name, `*`, or `category:<category>`.

### 12.5 Selection

An execution's lifecycle is, in order: `exec.md`'s `lifecycle:`; else
the `lifecycle` of the lane it is in; else the `lifecycle` of the
deployment folder's `grain.config.md`; else the built-in `grain/default`
(§12.9). When it creates an execution, a writer copies the definition's
lifecycle into `exec.md`, pinned: the definition's own `lifecycle:`,
else the `lifecycle` of the definition folder's `grain.config.md`.
**Decision:** a definition folder's default is copied at creation rather
than consulted at dispatch, so that an intake can choose a lifecycle for
everything proposed there while every input to dispatch is in the
deployment's repository. An execution whose lifecycle cannot be resolved
is not dispatched (GF030).

### 12.6 Outcomes propose transitions

The executor applies `on_outcome` (defaults in §12.9): when a run event
occurs (`accepted`, `done`, `failed`, `nothing_needed`, `cancelled`,
`interrupted`, `failure_streak`), the mapped state is a **proposed
transition**. The executor writes it on `exec.md`, under its own identity,
only if the transition is allowed by the lifecycle **and**
`GRAINOWNERS` grants that transition to the deployment (§16.2.4);
otherwise it records `proposed_transition: {to, applied: false}` on the
run and a person decides. Run status never changes lifecycle state except
through this mechanism.

### 12.7 Transitions and who makes them

A change of `state` from A to B is allowed only if some transition has
`from` A (or `*`) and B in `to` (GF052). Setting a `ready`-category state
from the initial state also sets `approved_by` and `approved_at`; setting
a `cancelled`-category state other than by rejection sets `cancel`.

### 12.8 Changing a lifecycle in use

A lifecycle MAY be edited. An execution whose `state` is not a state
(nor an alias) of its lifecycle is an error (GF051) until it is
migrated; readers MUST NOT dispatch it. An alias maps a renamed state:
readers treat the alias as the state, and a writer SHOULD rewrite the
execution's `state` to the new name on its next change. Changing an
execution's `lifecycle:` is allowed only while it is in its initial
state.

### 12.9 The built-in default lifecycle, `grain/default`

It reproduces grain's task states (bwsalmon/grain `pkg/model/state.go`,
`StateOf`). A grain task is one definition plus one execution: it
carries its prompt, its state, one comment thread (`Comment.TaskID`) and
its runs (`Run.TaskID`) together. In grain, state is derived; here the
task-level states are written on the execution, and the run-level ones
move to run status.

| grain state | Here | Category |
| --- | --- | --- |
| `proposed` | state `proposed` (initial) | proposed |
| `queued` | state `queued` | ready |
| `deferred` | state `deferred` | waiting |
| `running` | state `active`; run status `running` | active |
| `awaiting_reply` | state `active`; run status `awaiting_reply` | active |
| `failed` (5 consecutive failing runs, `MaxConsecutiveFailures`) | state `failed` | waiting |
| `awaiting_submit` | state `awaiting_submit` | waiting |
| `awaiting_ack` | state `awaiting_ack` | waiting |
| `completed` | state `completed` | done |
| `closed` (without completing) | state `closed` | cancelled |
| — (rejected proposal) | state `rejected` | cancelled |

Run outcomes `paused`, `capacity` and `interrupted`, which grain skips
when counting failures, are run statuses (`interrupted`) and propose no
transition.

Transitions: `proposed → queued | rejected | deferred | closed`;
`queued → active | deferred | closed`; `deferred → queued | proposed |
closed`; `active → queued | failed | awaiting_submit | awaiting_ack |
completed | closed`; `failed → queued | closed`; `awaiting_submit →
completed | active | closed`; `awaiting_ack → completed | active |
closed`; `completed → queued` (reopen); `closed → queued` (reopen).

`on_outcome`: `accepted → active`; `done → completed` (or
`awaiting_submit` when the work does not auto-merge, `awaiting_ack`
when it asks for acknowledgement); `nothing_needed → completed`; `failed →
queued`; `failure_streak → failed`; `interrupted → queued`;
`cancelled → closed`.

The file form is `schemas/grainfiles/lifecycles/default.lifecycle.md`;
a reader MUST behave as if it were resolved at the reference
`grain/default`.

### 12.10 Prior art

Jira's status categories (To Do, In Progress, Done) and Linear's state
types (backlog, unstarted, started, completed, canceled) make the same
split: teams name states freely, tools act on a small fixed set of
types. Fully opaque states were rejected because Grain could not tell
whether an execution is done or dispatchable.

## 13. Folder templates and instantiation

### 13.1 Forms

A **folder template** is a directory `<dir>/` inside a grain folder
whose top holds the manifest `<dir>.template.md` with `form: folder`.
Everything else under `<dir>/` is the template's **members**: definitions
(`<name>.task.md` or `<name>.task/`), and optionally designs,
lifecycles and assets they use. Instantiating a folder template creates
**one execution per member definition**.

A **single-task template** is a `*.template.md` with `form: task`
anywhere in a grain folder (outside template folders). It is, by
definition, equivalent to a folder template whose only member is one
definition rendered from it. **Decision:** the single-file shorthand is kept,
because nearly every template today is one task, and a directory per
template would double the files without saying anything more. A
trigger's `fires.template` and a task's `template.ref` may name either
form.

### 13.2 Rules

1. A template folder MUST NOT contain `grain.config.md` (GF011).
2. A template folder's contents are **inert**: never dispatched, never
   fired, never given to a run as memory, and never listed by a root
   entry (GF064 if a root entry's folder would otherwise list one).
3. Members MAY use relative references among themselves
   (`depends_on: [../stage-1.task.md]`); such a reference MUST resolve
   inside the template folder (GF061).
4. A cycle among members' `depends_on` is an error (GF062).
5. **Nested inclusion is deferred.** A template folder MUST NOT contain
   another `form: folder` manifest (GF066). A member MAY reference a
   single-task template or a partial outside the folder; it is resolved
   at instantiation like any escaping reference (§13.3).
6. Members hold no state (§7.1); each execution created gets its
   lifecycle's `initial` state, or the state `run.require_approval`
   implies (§13.8).
7. A template folder MUST NOT contain an execution (GF047).

### 13.3 Instantiation

Instantiation takes a folder template, input values and a **lane** of a
deployment, and writes **one commit**, in that deployment's repository,
that creates one execution per member definition.

1. **Lane.** By default (`target: lane`) the lane the request names; or
   the lane a trigger's `fires.into` names (§7.6).
2. **Instance name.** The executions go in `<lane>/<instance>/`, where
   `<instance>` is `<template name>-<yyyy-mm-dd>`, with `-2`, `-3`, …
   appended on collision; a trigger firing uses
   `<template name>-<event key>` instead, so a retry lands on the same
   path and is a no-op. Collisions are detected against the target
   commit; a racing writer's conflict is an ordinary git conflict, and it
   retries with the next suffix.
3. **One execution per member.** A member definition at `<dir>/<p>.task.md`
   (or `<dir>/<p>.task/`) becomes `<lane>/<instance>/<p>.exec/`, with
   `definition: <member's canonical ref>@<sha>` and its
   `definition_sha256`.
4. **Snapshot.** Each `task.md` is the member with inputs substituted
   (§13.5) into body and title, recorded in `exec.md` `inputs`.
5. **Dependencies.** A member's relative `depends_on` on another member
   becomes `depends_on: [../<q>.exec/]` on its execution, naming the
   sibling execution created from that member (§11.4). Escaping
   references in the snapshot are read relative to the member (§7.2), so
   none is rewritten.
6. **Provenance.** Every `exec.md` gets `instantiated_from:
   "<manifest canonical ref>@<sha>"`, `created_via: template`, and the
   template's (or the member's) lifecycle, pinned.
7. **Rank.** The executions are `rank`ed after the lane's last
   execution, in member path order.
8. The commit is made by the instantiating principal (§16.6), and
   `GRAINOWNERS` is evaluated for the paths written (§16.2):
   instantiating requires write authority for every file written.

An instance MUST hold exactly the executions steps 3 and 5 make, with
those definitions and those `depends_on` (GF069); an execution in an
instance folder without `instantiated_from` is GF067. A dry run
(`grainlint --instantiate TREE MANIFEST [name=value …]`) performs steps
2–5 in memory and prints the executions it would create.

### 13.4 Single-task instantiation

A `form: task` template creates one execution,
`<lane>/<template name>-<date>.exec/` (or the trigger path of §7.6),
with `definition` naming the template at its commit and the rendered
task as the snapshot. A person proposing a definition from a template
instead writes a definition with `template: {ref, rev, inputs}`.

### 13.5 Inputs and substitution

**Declaration.** `inputs:` is a list of `{name, type, required, default,
options, of, ref_kind, label}`. `name` matches `[a-z][a-z0-9_]*`. `type`
is `string`, `text`, `int`, `bool`, `enum` (with `options`), `list`
(with `of`), or `ref` (with optional `ref_kind`: `repo`, `folder`,
`file`). A required input without a default that is not supplied is an
error (GF063).

**Syntax.** The strict Jinja2 subset of the design (§6.5 of
`grainfiles.md`): `{{ expr }}`, the filters `default lower upper trim
replace join indent length first last wordwrap tojson`, `if`/`elif`/
`else`, `for` (with `loop.index`, `loop.first`, `loop.last`), `set`,
`with`, `include` and `import`/`macro` of `form: partial` templates.
Refused: `extends`, `block`, method calls, `namespace`, `cycler`, `self`,
`range` over 1000, autoescape. Undefined names are errors (GF065).

**Where substitution happens.** Only in a member's (or single-task
template's) Markdown body and its `title`, when the snapshot is written. **No other front-matter field
and no path is substituted.** `ref`-typed inputs are never rendered as
text: they are bound only into `context:` entries that say `input:
<name>`, after being resolved and checked as references. **Decision:**
this keeps substitution out of everything a reader acts on, so an input
can change what an agent reads but never what is dispatched, depended
on or authorised.

**Escaping.** `{% raw %}…{% endraw %}` emits text verbatim. A literal
`{{` outside `raw` is a template expression.

**Values** are checked against their types before rendering; an `enum`
outside `options` is GF063.

### 13.6 Partials

`form: partial` templates are include-only text. They declare inputs,
bound by the including `{% with %}` or macro arguments. They are never
instantiated.

### 13.7 Updates

An instance is an **independent copy**: each execution runs from its
snapshot. Changing a template never changes an existing instance; there
is no live inheritance, and a template edited after an instance was
created is GF046 on the member, as for any definition. The
`instantiated_from` revision lets a later tool diff an instance against
the template's newer revision and propose an upgrade as an ordinary
change.

### 13.8 From grain's suites and templates

`.grain/suites/<name>.yml` plus the `.grain/templates/*.yml` it names
become one folder template:

| grain today | Folder template |
| --- | --- |
| `.grain/suites/<name>.yml` | `.grain/templates/<name>/<name>.template.md`, `form: folder` |
| `name:` | `title:` |
| `templates: [a, b]` | member definitions `a.task.md`, `b.task.md`, each from `.grain/templates/a.yml` (its `body` becomes the Markdown body, `title` stays) |
| `mode`, `count`, `max-passes` | `run.mode`, `run.count`, `run.max_passes` |
| `require-approval` | `run.require_approval`: `false` creates the executions in the lifecycle's first `ready`-category state reachable from `initial`, with `approved_by` the instantiating principal; `true` leaves them in `initial` |
| `auto-merge`, `auto-approve-proposals`, `team` | `run.auto_merge`, `run.auto_approve_proposals`, `run.team` |
| a template's `base`, `auto-merge`, `model`, … | the member definition's own fields |
| a schedule's `suite: <name>` | a trigger whose `fires.template` names the folder template |

A single `.grain/templates/<name>.yml` not used by a suite becomes
`.grain/templates/<name>.template.md`, `form: task`. Appendix A.5 shows
release qualification converted.

## 14. `context:` materialisation

14.1. The `context:` of an execution's snapshot (`task.md`) is the
**only** thing that places files in a run's sandbox. Nothing else a
definition, execution or template references is materialised, and the
live definition is never read.

14.2. **Entry.** `{ref, mode, path, depth, input, rev, reducers}`:

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `ref` | *ref* (repo, folder or file) | | Exactly one of `ref` or `input` (templates only). |
| `input` | input name | | A `ref`-typed input of the template. |
| `mode` | `read` `write` `use` | `read` | `write` only on a repository or folder at a branch; `use` only on a `capability` (§15). |
| `path` | sandbox-relative path | §14.3 | Where it appears. |
| `depth` | 0–3 | 0 | For a Markdown file: follow relative body links this many levels. |

14.3. **Layout.** The first `write` entry is the execution's primary
repository, at `work/`. Other repositories and folders appear at
`work/<repo name>/`; files at `context/<repo name>/<path>`. Two entries
MUST NOT map to overlapping sandbox paths (GF020).

14.4. **Depth** follows only relative links in a Markdown body to files
in the same repository, at the same commit.

14.5. **Gate.** The executor resolves each entry with `resolve` (§9.3),
with its local policy as part of the gate, checked before every fetch.
A refused entry is skipped and recorded, or holds the run (`waiting`)
if the entry is `write`.

14.6. **Record.** The run's `context.materialised` lists each entry with
the commit used; `context.skipped` lists each skipped entry with its
reason.

14.7. **Limits.** At most 64 entries, 10,000 files and 1 GiB per run.

14.8. In a code repository's grain folder, `context:` entries and `ref`
input defaults MUST name only that repository (GF020).

## 15. Capabilities

15.1. A **capability** is authority a run uses beyond its files. A
**base** (`capability-base`) is what an executor's provider implements,
published by the executor in its root at
`capabilities/bases/<type>.capability-base.md`. A **capability
grainfile** names a base, directly or through another capability, and a
chain of **reducers** that only narrow it.

15.2. **Chain resolution.** Follow `base.capability` with `resolve`
until a `base.type`, with cycle detection and at most 8 links (GF031).
Every link MUST come from a definition source the executor's policy
sanctions; otherwise the request is refused.

15.3. **The reducer interface.** A reducer is a pure function

```
reduce(base_declaration, grant_so_far, parameters) -> grant' | refusal
```

where `grant'` MUST be a subset of `grant_so_far` in every dimension the
base declares (it may only narrow), and it may not read anything but its
arguments. A reducer is either `language: builtin` with `type`
(`limit`, `allow`, `deny`, `ttl`, `scope`) and parameters, or
`language: anselm` with an opaque `version` and `body`. **The syntax and
semantics of Anselm are not specified here** and are out of scope for
this format; a reader that cannot evaluate a reducer MUST refuse the
request (fail closed).

15.4. **Requests and approval.** A definition requests a capability
with a `context:` entry `{ref, mode: use, reducers}`. A request is never
a grant: the executor's local policy decides. A capability whose
`approval` names approvers needs a matching `capability_approvals`
entry on the execution's `exec.md`, whose `digest` is `sha256:` of the canonical JSON of the
resolved chain.

15.5. Secrets never appear in grainfiles: `secrets:` binds slot names to
secret names the executor resolves.

15.6. The run's `capabilities` lists each grant with its chain digest.

## 16. Write authority

### 16.1 The model

Repositories live on **external forges**. **The repositories are
authoritative**; every index a hub or executor keeps is a cache. The
forge enforces who may change a protected branch:

1. The **default branch is protected**, with direct pushes off for
   everyone the forge allows (§16.3 says what is relied on).
2. A **required status check**, the **forge check**, runs on every pull
   request (and, where the forge supports push rules, on every push) to
   that branch. It runs the linter over the head tree and then
   authorises the change against `GRAINOWNERS` (§16.4).
3. Readers act only on commits reachable from the protected branch, and
   re-check where the forge cannot be relied on (§16.7).

There is no privileged bot. The hub writes as the user it acts for; a
deployment writes as itself (§16.6).

### 16.2 `GRAINOWNERS`

16.2.1. **Placement and scope.** `GRAINOWNERS` files MAY be in any
directory of a grain folder. Patterns are relative to the directory of
the file containing them. A `GRAINOWNERS` governs only paths inside its
grain folder; paths outside every grain folder are not governed by any
`GRAINOWNERS` (§16.9).

16.2.2. **Grammar.**

```abnf
file        = *( line LF )
line        = [ rule ] [ comment ] / blank
comment     = "#" *VCHAR-OR-SP
rule        = pattern [ ":" fields ] 1*WSP owners
pattern     = gitignore-pattern            ; CODEOWNERS semantics
fields      = field *( "," field )
field       = field-name / %s"body" / transition
field-name  = LOWER *( LOWER / DIGIT / "_" / "." )
transition  = %s"state(" selector "->" selector ")"
selector    = state-name / "*" / %s"category:" category
owners      = owner *( 1*WSP owner )
owner       = "@" principal [ "(" verbs ")" ]  ; principal of §6.3
            / "@none"
verbs       = verb *( "," verb )
verb        = %s"create" / %s"update" / %s"delete" / %s"move" / %s"archive"
```

A malformed line is an error (GF070); an owner that is not a §6.3
principal is GF071. `@none` means nobody may change the matched paths.

16.2.3. **Precedence.** For a changed path, the governing rules are those
of the **nearest** `GRAINOWNERS` (the closest ancestor directory, inside
the grain folder) that has at least one matching line. Within that file,
**the last matching whole-file line** gives the file's owners, and for
each field the **last matching line naming that field** gives the
field's owners, overriding the whole-file owners for that field. A field
rule that names a field the kind does not have is GF070.

16.2.4. **Transition rules.** A field `state(A->B)` governs a change of
the `state` field from a state matching selector A to one matching B.
For a state change, the governing owners are those of the last matching
transition rule whose selectors both match; if none matches, the owners
of a plain `state` field rule; if none, the whole-file owners. Example:

```
/hub/**/*.exec/exec.md:state(*->sec-approved)   @team:security#approver
/hub/**/*.exec/exec.md:state(category:ready->category:active),state(category:active->*)  @deployment:eu
```

16.2.4a. **Per-subfolder authority.** Because each container holds
everything about one definition or one execution, `GRAINOWNERS` grants
authority per subfolder. The intended split, which the worked example
uses:

| Path | Owners |
| --- | --- |
| a definition's `task.md` (or `<name>.task.md`) | the definition's owners (its folder's team) |
| `**/thread/` of either kind of container | commenters, normally `@authenticated(create)` |
| an execution's `runs/` | the executing deployment, `@deployment:<id>` |
| an execution's `exec.md`: `state(...)` transitions out of `ready` and on from `active`, `superseded` | the executing deployment |
| an execution's `exec.md`: `approved_by`, `approved_at`, the approving `state(...)` transition, `capability_approvals` | the approvers |
| an execution's `exec.md`: `rank`, `priority`, `expedite`, `ranked_by`, `ranked_at` | the lane's members |
| creating (and moving) an execution folder in a lane | whoever may place work there, with `(create,move)` |

For example, a deployment's root:

```
*                                     @user:bsalmon@google.com
/hub/**/*.exec/                       @team:hub#member(create,move) @user:bsalmon@google.com
/hub/**/*.exec/exec.md:rank,priority,expedite,ranked_by,ranked_at  @team:hub#member
/hub/**/*.exec/exec.md:approved_by,approved_at,state(*->sec-approved)  @team:security#approver
/**/*.exec/exec.md:state(category:ready->category:active),state(category:active->*),superseded  @deployment:eu
/**/*.exec/runs/                      @deployment:eu
/**/*.exec/thread/                    @authenticated(create)
```

and an intake:

```
*                          @team:hub#admin
/tasks/                    @authenticated(create) @team:hub#member
/tasks/**/thread/          @authenticated(create)
```

16.2.5. **Verbs.** An owner with no verbs may create, update, move and
archive; `delete` MUST be granted explicitly and is allowed only on
assets (GF073 otherwise; no grainfile is deleted, §8.4). `(create)`
allows only adding the file. A rule on a directory pattern (ending `/`)
governs every file under it; a lane move of an execution folder is
authorised by `move` on its `exec.md`.

16.2.6. **Fixed rules** hold whatever `GRAINOWNERS` says:

- a new definition's `submitter` is the actor (§16.4);
- a new execution's `state` is its lifecycle's `initial`, or a
  `ready`-category state reachable from it when `approved_by` is the
  actor and the actor holds that transition; its `created_by` is the
  actor;
- `approved_by` is the actor and differs from the definition's
  `submitter` (unless the lifecycle allows self-approval);
- `ranked_by`, `created_by`, `author`, `moved_by` and `cancel.by` are
  the actor, or the principal the actor is delegated by;
- terminal run records, comments and tombstones never change;
- an execution's snapshot and the fields of §7.2 fixed at creation never
  change (GF052);
- a fold (§4.3) is authorised by permission to create the comment it
  carries, or to move the definition;
- `grain.config.md`'s `role` and `owner` change only by its `owner`;
- `GRAINOWNERS` itself changes only by the owners its own nearest rule
  names, and a file may not grant a path to anyone its parent's rule
  for that path does not already own.

16.2.7. **No owner.** A grainfile with no governing rule may be changed
only by the grain folder's `owner` (GF072 warning).

### 16.3 The repo settings file's `enforcement`

```yaml
enforcement:
  mode: required-check      # required-check | codeowners | reader
  check: grainfiles/authorize
  branch: main
  codeowners: compiled      # absent | compiled
```

- `required-check`: the forge protects `branch` and requires the status
  `check` before merge; direct pushes are off.
- `codeowners`: the forge protects `branch` and enforces a CODEOWNERS
  file compiled from `GRAINOWNERS` (§16.8), with no required check.
- `reader`: the forge enforces nothing readers can rely on.

A reader MUST NOT rely on more than `enforcement` claims. **A reader
MUST perform reader verification (§16.7) unless `mode` is
`required-check`**, and SHOULD verify that the claim is true where the
forge API lets it.

### 16.4 The forge check

**Inputs:** the base tree (the protected branch's tip), the head tree,
the commits between them, and the **actor**: the forge identity of the
pull request's author (for a pull request) or of the pusher (for a push
rule). Reviews and approvals on the pull request are inputs only where
a rule's owners are satisfied by "approved by" (§16.8).

**Algorithm:**

1. Lint the head tree for every grain folder touched (§20); any error
   fails the check. `orphans: refuse` makes GF040 an error. A change to
   a definition that an execution has snapshotted (GF046, found through
   the hub's definition → executions index where the check has one) MAY
   fail the check unless it carries the override of §7.1.
2. Map the actor to principals with the identity map (§16.5): the
   user principal, and every team role the map gives it. A deployment's
   forge identity maps to `deployment:<id>`.
3. For each changed path inside a grain folder, compute the operation
   (`create`, `update`, `delete`, `move`, `archive`) and, for an update
   of a grainfile, the set of changed fields (front-matter keys, and
   `body`); for each changed `state`, the transition.
4. Authorise each (path, operation, field, transition) against
   `GRAINOWNERS` (§16.2) and the fixed rules; an unauthorised change is
   GF073 (path or field) or GF074 (transition), naming the rule.
5. Author fields (`submitter`, `created_by`, `author`, `approved_by`, …) are checked
   against the actor's principals, never against git author or
   committer fields.

The check MUST evaluate `GRAINOWNERS` and lifecycles **from the base
tree**, so a change cannot authorise itself; a change to `GRAINOWNERS`
is authorised by the base tree's rules for that file.

### 16.5 Identity map

Principals map to forge identities through an identity map: a YAML file
(`<name>.identities.yaml`, an asset referenced by `.grainconfig`
`identity_map`), mapping `user:` principals to forge logins per forge
host, teams to forge teams, and deployments to their forge accounts. A
hub MAY serve the same map from its directory. A principal with no
mapping matches no actor.

### 16.6 The hub as writer

The hub writes through the forge API (a pull request, or a commit where
the user may push), **as the user** it acts for, with that user's
delegated token. It MUST NOT write with a credential that has more
authority than that user. It creates executions (§11.3), comments on
definitions and executions, approvals and ranks this way; a trigger it
fires writes as `hub:trigger` under the target folder's `GRAINOWNERS`.
**Decision:** the hub never writes into a definition's repository on
behalf of placement: placing work is one commit in the deployment's
repository. A hub MAY hold a service identity for reading.

### 16.7 Readers and the deployment as writer

- A reader MUST act only on commits reachable from the protected branch
  of the folder's repository (`enforcement.branch`, else `default_ref`).
- **Reader verification.** A reader that performs it re-runs §16.4
  steps 3–5 for each commit (first-parent history) since the last
  commit it verified, using the commit's forge-attributed pusher where
  the forge records it, and **refuses** (does not act on) any change it
  cannot authorise, reporting GF073/GF074. It is REQUIRED when
  `enforcement.mode` is not `required-check` and OPTIONAL otherwise.
- **A deployment writes** its executions' `runs/` (records and the
  comments in them), outcome-driven transitions on `exec.md` (§12.6) and
  `superseded` on re-placement (§11.5), under its own forge identity.
  `GRAINOWNERS` in its root MUST grant exactly those: create and update
  under `*.exec/runs/`, and the `state(...)` transitions its
  lifecycles' `on_outcome` needs. A deployment writes only its own root
  grain folder; it never writes a definition's repository.

### 16.8 Compiling to CODEOWNERS and rulesets

Where the forge supports CODEOWNERS (GitHub, GitLab) a writer MAY
compile `GRAINOWNERS` into the forge's CODEOWNERS file: each whole-file
rule becomes a CODEOWNERS line with the pattern prefixed by the grain
folder's path, and owners mapped through the identity map. The mapping's
limits:

- CODEOWNERS means "must approve", not "may author", so compiled rules
  enforce review by an owner, not authorship;
- field-level and transition rules, verbs and the fixed rules cannot be
  expressed and are **dropped** (the compiler reports each one);
- `@authenticated` cannot be expressed.

So `codeowners` mode is weaker than `required-check`, and readers MUST
verify (§16.7). Rulesets (path-restricted push rules) MAY additionally
enforce `@none` and the deployment-only `runs/` folders.

### 16.9 A grain folder inside a code repository

The forge check runs only on changes that touch a grain folder, and
judges only paths inside grain folders. The rest of the repository is
governed by the repository's own review rules. A change that touches
both MUST satisfy both. `.grainconfig` at the repository root is
governed by the repository's own rules plus, if any grain folder is the
repository root, that folder's `GRAINOWNERS`.

## 17. Registry and hub behaviour

17.1. **Registering** a grain folder is a commit to the registry adding a
`folders` entry; it is reviewed like any change, under the registry's
`GRAINOWNERS`. Unregistering removes the entry. A hub MUST NOT read a
folder as part of a team's views unless it is registered.

17.2. **Cross-folder views** in the registry are derived indexes
(§11.6) over the registered folders: Silo's lists of definitions, of
executions across deployments, and of a definition's executions are
caches over the registry's grain folders, rebuilt from them. A registry
has no lanes; ranking is always an edit of an execution in its
deployment's lane.

17.3. **Freshness.** A hub refreshes a registered folder on a forge
webhook for its branch, and otherwise polls it at `poll_interval`
(default 5 minutes). A view MUST show the commit each folder was last
read at. A hub MUST NOT make a decision (placement, re-placement,
cancel) from an index older than the folder's tip it last saw; it
re-reads the execution (or the definition) first.

17.4. **The hub is the PDP for its UI**: it decides what a user may
attempt, using the same `GRAINOWNERS` evaluation as the forge check, so
the UI does not offer what the check would refuse. The forge check
remains the enforcement point for the repository, and each executor the
enforcement point for what it runs.

17.5. **Placement.** To place a definition on a deployment, the hub
creates an execution in a lane that deployment's `grain.config.md` names
(§11.3): one commit in the deployment's repository, as the placing user.
Nothing the hub writes activates anything unless it is in a lane a
deployment's owner declared. Re-placement is §11.5.

17.6. **Showing an execution** follows §11.2: the execution's thread and
runs, plus the definition's thread read through `definition`, each
comment labelled with its scope.

## 18. Grain folders in code repos, and task homes

18.1. A code repository MAY contain grain folders; `.grain/` is the
conventional one. It is a grain folder only if it holds
`grain.config.md`, normally `role: library`. **Decision:** grain's
existing `.grain/` declarations without `grain.config.md` are read by
grain's own loader as today; they become grainfiles once the folder has
a `grain.config.md` (§13.8 and the design's migration).

18.2. **Where definitions and executions live.** Definitions, with
their threads, MAY live in any grain folder, including a code
repository's `.grain/`; executions live only in a deployment's root
grain folder (§11.3). **Decision:** the `task_home` setting and its
`intake`, `promote` and `code-repo` modes are removed (`task_home` is
refused, GF020). They existed to decide where an approved task, with
its state and its pointers, should live; a definition now holds no
state, its discussion is inside it, and approval is on the execution,
so nothing has to move when work is approved. A team that wants a
definition next to its code writes it there, or moves it there once with
a tombstone (§8.3), its thread moving with it.

18.3. In a code repository's grain folder, `context:` names only that
repository (§14.8), and templates carry no `reads`, `writes`, `grants`
or `target`. A code repository's grain folder holds no executions
(GF047).

18.4. Workflows (`.grain/workflows/*.yml`) stay YAML assets; a
trigger's `fires.workflow` names one by path in the same repository.

## 19. Processing model

19.1. **Tree-only discovery.** A reader lists a folder from the git tree
alone and knows each entry's kind from its name (§4.1). It MUST NOT open
a blob to decide what a file is. Nothing scans a repository for
grainfiles: a reader lists only root entries' folders, containers and
the folders a reference names.

19.2. **Lazy parsing.** A reader parses a grainfile's front matter only
when it needs a field, and the body only when it needs the body.

19.3. **Caching.** Listings are cached by tree hash and parsed front
matter by blob hash; both are immutable, so neither needs invalidation.

19.4. **Limits** (§4.5) fail loudly: a listing over a limit is an error
for that entry (GF006), and the entry activates nothing, rather than
silently truncating.

## 20. Errors and lint

20.1. A linter reports each condition with its code, severity, the path
and a message. The reference linter's messages are informative.

20.2. **Severity.** An *error* fails the forge check and makes the
grainfile (or the edge) unusable to readers as the section that defines
it says. A *warning* fails nothing.

| Code | Severity | Condition |
| --- | --- | --- |
| GF001 | error | A grainfile name violates §4.1–§4.2, or a comment or run folder is not named as §7 requires. |
| GF002 | error | `kind:` differs from the file's kind suffix. |
| GF003 | error | Malformed: no front matter, unclosed delimiter, not UTF-8, NUL, LFS pointer, YAML that does not parse. |
| GF004 | error | Front matter outside the YAML subset (§5.3): duplicate keys, anchors, tags, non-string keys, top level not a mapping. |
| GF005 | warning | A Markdown file with an unregistered kind suffix. |
| GF006 | error | A limit of §4.5 exceeded, or two paths differ only in case. |
| GF007 | error | A reserved name misused: a file a container does not allow (§4.3), a container nested in a container, a fixed name (`task.md`, `exec.md`, `run.md`) outside its place, a `*.exec.md` or `*.run.md` file, a comment outside every `thread/` and run folder, `grain.config.md` not at a folder top, `.grainconfig` not at a repository root. |
| GF010 | error | Nested grain folder. |
| GF011 | error | `grain.config.md` inside a template folder. |
| GF020 | error | Schema validation failure (a field's type, a required field, a refused field, an unknown trigger source or gate requirement, a mutually exclusive pair). |
| GF021 | error | Unknown schema major version. |
| GF022 | warning | Unknown field (§3.3). |
| GF023 | error | `schema:` names another kind. |
| GF030 | error | Reference does not resolve (missing, unreadable, broken redirect, bad anchor, folder locator without `grain.config.md`). |
| GF031 | error | Redirect loop, more than 8 hops, or capability chain too deep. |
| GF032 | error | Reference resolves to the wrong kind. |
| GF033 | error | Undeclared alias. |
| GF034 | error | `depends_on` cycle. |
| GF035 | error | Malformed reference. |
| GF036 | error | Reference climbs above its repository root. |
| GF040 | warning | Orphan (§10.2.5). |
| GF044 | warning | A tombstone's target does not list it in `moved_from`, or a `moved_from` entry has no tombstone. |
| GF045 | error | A container lacks a required file (an execution's `exec.md` or snapshot `task.md`, a run folder's `run.md`, a definition folder's `task.md`), a definition exists both as `<name>.task.md` and `<name>.task/`, or a snapshot of a plain definition does not hash to `definition_sha256`. |
| GF046 | warning | A definition was edited after an execution snapshotted it: its bytes differ from that execution's `definition_sha256`. |
| GF047 | error | An execution outside every lane of a deployment's root grain folder (in a library folder, a code repository, a template folder, or a non-lane directory), or a lane whose `path` is outside the root grain folder. |
| GF048 | error | Two executions with the same name in one deployment's root grain folder. |
| GF050 | error | Invalid lifecycle (§12.1). |
| GF051 | error | An execution's `state` is not a state or alias of its lifecycle. |
| GF052 | error | A forbidden change: a transition the lifecycle does not allow, an execution's snapshot or a field fixed at creation changed, a terminal record, comment or tombstone changed, a path reused. |
| GF053 | error | Two open runs in one execution. |
| GF054 | error | A lifecycle alias equals a state name, or aliases form a chain. |
| GF055 | warning | A dependency is in a `cancelled`-category state. |
| GF061 | error | A template member's internal reference does not resolve inside the template folder. |
| GF062 | error | A cycle among a template's members' `depends_on`. |
| GF063 | error | A required input is missing, or an input value has the wrong type or is outside `options`. |
| GF064 | error | A template member, gate or other inert grainfile is listed by a root entry. |
| GF065 | error | A template uses a construct outside the subset, an undefined name, or fails to render. |
| GF066 | error | A template folder contains another folder-template manifest. |
| GF067 | warning | An execution inside an instance folder lacks `instantiated_from`. |
| GF068 | error | A `form: folder` manifest is not `<dir>/<dir>.template.md`, or a `form: task` template sits at a template folder's top. |
| GF069 | error | An instance does not match its folder template's dry run: an execution missing or extra, a `definition` that is not its member, or `depends_on` that does not map the members' (§13.3). |
| GF070 | error | `GRAINOWNERS` syntax error. |
| GF071 | error | `GRAINOWNERS` names an unknown principal form. |
| GF072 | warning | A grainfile has no governing `GRAINOWNERS` rule. |
| GF073 | error | Unauthorised change of a path or field (authorisation mode). |
| GF074 | error | Unauthorised lifecycle transition (authorisation mode). |
| GF080 | error | `.grainconfig` is invalid. |
| GF090 | error | `kind: suite` or a `*.suite.md` file (removed; §13.8). |

## 21. Security considerations

21.1. **Identity comes from the forge.** Git author and committer fields
are claims anyone can write. Authorisation MUST use the forge-verified
actor (§16.4) and never those fields; author fields in front matter are
checked against the actor.

21.2. **Unprotected branches.** Anything on a branch other than the
protected branch is a proposal. A reader that acts on an unprotected
branch acts on unreviewed content; readers MUST NOT (§16.7). A
`ref_mode: track` entry on a branch that is not protected is refused by
an executor unless the folder's owner is the deployment's owner.

21.3. **Forge tokens.** A hub's delegated user tokens MUST be scoped to
the repositories of registered folders and stored as secrets; a hub that
loses one MUST NOT fall back to a broader credential. A deployment's
forge identity SHOULD be able to write only its own root and the
transitions `GRAINOWNERS` grants it. Tokens never appear in grainfiles.

21.4. **Self-authorisation.** The forge check reads `GRAINOWNERS`,
lifecycles and `.grainconfig` from the base tree (§16.4), so a change
cannot widen its own authority.

21.5. **Template input injection.** Inputs are substituted only into
bodies and titles (§13.5), so a hostile input cannot add a dependency,
a capability request, a write entry or a path. Bodies are prompts: an
input can still steer an agent, so an executor treats a rendered body as
untrusted text, and a reviewer sees the rendered body before approval.
`ref` inputs are resolved and gated like any reference.

21.6. **Assets are data**: never executed by a reader, whatever their
names or modes.

21.7. **Denial of service.** The limits of §4.5 bound every listing,
parse, render and chain; readers fail loudly rather than truncating.

21.8. **Cross-forge confusion.** Credentials are bound to hosts (§9.5);
a reference cannot cause a reader to send one forge's token to another.

## 22. Registries of identifiers

22.1. **Kinds:** `config`, `task`, `exec`, `run`, `comment`, `template`,
`trigger`, `gate`, `capability`, `capability-base`, `design`, `memory`,
`moved`, `lifecycle`, `asset`. (`suite`, `ref` and `placement` are
retired and MUST NOT be reused.)

22.2. **Root types:** `backlog`, `triggers`, `memory`.

22.3. **State categories:** `proposed`, `ready`, `active`, `waiting`,
`done`, `cancelled`.

22.4. **Trigger sources:** `schedule`, `push`, `merge`, `pull_request`,
`design-change`, `task-state`, `manual`; reserved: `task-closed`.

22.5. **Gate requirements:** `human`, `security_review`,
`second_review`, `state`.

22.6. **Adding identifiers.** A new kind, root type, category, source or
requirement is added only by a revision of this specification (§3.2),
which updates this section, the JSON Schemas and the linter together.
Experimental kinds use names beginning `x-` and are assets to readers
that do not know them.

22.7. **Retired lint codes:** `GF041` (misplaced child) and `GF042`
(stray child) were the pointer model's checks that a back-pointer agreed
with its declaring parent; containment (§11.1) leaves nothing to
disagree, and a child outside its parent is GF007. They MUST NOT be
reused.

## Appendix A. A complete worked example

The files are in [`examples/grainfiles/`](../examples/grainfiles/README.md),
and `python3 tools/grainfiles/grainlint.py examples/grainfiles` checks
them all.

A.1. **An intake with a custom lifecycle, and a definition with its
thread** — `examples/grainfiles/hub-intake/`: a `role: library` folder
whose `grain.config.md` names `lifecycles/security.lifecycle.md`; the
definition `tasks/2026-10-16-approval-flow.task/` with `task.md` and a
`thread/` of two comments, one replying to the other; the tombstone of a
definition that moved to a code repository; and `GRAINOWNERS` giving
definitions to the team and their threads to any commenter.

A.2. **A grain folder in a code repo** — `examples/grainfiles/widgets/`:
a code repository whose `.grain/` is a `role: library` grain folder with
a gate, a single-task template and a definition homed there with its
thread; the code outside `.grain/` is not governed by its `GRAINOWNERS`.

A.3. **A deployment with executions** — `examples/grainfiles/grain-eu/`:
deployment `eu`, `grain.config.md` (`role: deployment`) with a `hub`
lane and an `own` lane, a trigger and memory, and `GRAINOWNERS`
granting per subfolder as in §16.2.4a.
`hub/2026-10-16-approval-flow.exec/` is an execution of the intake's
definition: `exec.md` in `in-progress` of the security lifecycle,
approved by the security team; its snapshot; its own `thread/` with a
comment replying to one on the definition's thread; and two runs, the
first `failed`, the second `running` with an agent comment.
`backlog/2026-10-16-tidy-run-notes.exec/` executes a single-file
definition of eu's own, with a run awaiting a reply.

A.4. **A registry** — `examples/grainfiles/registry/`: `role: registry`,
listing the intake, the widgets `.grain/` folder and both deployments,
with a definitions view and an executions view.

A.5. **A folder template and an instance** —
`examples/grainfiles/hub-intake/intake/templates/release-qualification/`:
grain's release-qualification suite converted (four member definitions,
a `qualification-report` definition that `depends_on` all four, and
`run:` from the suite file), and its instance in eu's hub lane,
`grain-eu/hub/release-qualification-2026-10-17/`: one execution per
member, each with `instantiated_from`, `inputs` and its rendered
snapshot, the report's `depends_on` mapped to its four sibling
executions. **Note:** grain's suite runs its four templates
independently; the report is added to show a DAG.

A.6. **A re-placement** — the widgets definition's execution
`grain-eu/hub/2026-10-16-widget-schema-v2.exec/` is `superseded` (state
`closed`, its run `superseded`), written by eu; its replacement
`examples/grainfiles/grain-us/hub/2026-10-16-widget-schema-v2.exec/`, in
deployment `us`'s repository, has `previous:` naming it and an identical
snapshot.

A.7. **Fixtures** — `tools/grainfiles/testdata/bad/`: one tree per
lint code, each with `expect.txt` listing the codes it must produce;
`tools/grainfiles/testdata/good/`: changes the forge check authorises,
including a commenter folding a definition and a member moving an
execution between lanes.

## Appendix B. Decisions made by this specification

1. Strict schemas with unknown fields as a warning, and `x_` extensions (§3.3).
2. Grainfile stems are lower-case ASCII (§4.2).
3. `deployment:<id>` and `hub` principals, with `grain:` and `silo` accepted as aliases (§6.3).
4. A `/`-rooted reference is rooted at the referring file's grain folder (§9.1).
5. `grain.config.md` has a third form, `role: library` (§10).
6. `.grainconfig` loses `host`, `ref_policy` and `grain_folder`, and gains `enforcement` and `identity_map` (§10.5).
7. A definition with no thread or assets may stay a single file `<name>.task.md`; its first comment folds it into `<name>.task/task.md`, byte for byte, in one commit, with no tombstone (§4.3).
8. Definition → executions, design → implementing definitions and execution/definition → dependents are derived indexes, the only relations not served by containment (§11.6).
9. A definition edited after a snapshot is a warning (GF046); the forge check's override is a `Grain-Definition-Edit:` commit trailer (§7.1).
10. The category set is the six given, unrefined (§12.2).
11. A `cancelled` dependency never satisfies; its dependents stay blocked (§12.3).
12. Lifecycle selection is execution, lane, deployment folder, built-in; a definition folder's default lifecycle is copied, pinned, onto each execution at creation (§12.5).
13. A `task-state` trigger source and a gate `requires.state` match exact states (§12.4).
14. grain's `failed`, `deferred`, `awaiting_submit` and `awaiting_ack` are `waiting`-category execution states; `running` and `awaiting_reply` are run statuses under state `active` (§12.9).
15. The single-task template file is kept as a shorthand, with `form:` distinguishing it from a manifest and a partial (§13.1).
16. Instances are named `<template>-<date>[-N]`, or `<template>-<event key>` for a trigger, and are a folder `<lane>/<instance>/` of executions (§13.3).
17. Substitution only in bodies and titles; never in other fields or paths (§13.5).
18. Nested folder-template inclusion is deferred (§13.2).
19. `GRAINOWNERS` transition rules are written `state(<from>-><to>)` field rules with state, `*` and `category:` selectors (§16.2.4).
20. Reader verification is REQUIRED unless `enforcement.mode` is `required-check` (§16.3).
21. The forge check evaluates rules from the base tree (§16.4).
22. A hub's registry is polled every 5 minutes absent webhooks (§17.3).
23. `.grain/` without `grain.config.md` stays grain's legacy declaration folder (§18.1).
24. Executions are folders `<name>.exec/` holding `exec.md`, a snapshot `task.md`, `thread/`, `runs/<run>/run.md` and `assets/`; definitions with discussion are `<name>.task/` holding `task.md`, `thread/` and `assets/` (§4.3, §7.2).
25. The snapshot records `definition_sha256` beside `definition: <ref>@<rev>`, so a reader without history can check the copy and tell whether the definition changed (§7.2).
26. A snapshot's relative references are read relative to its definition, never to the copy; references out of an execution should be `/`-rooted (§7.2).
27. Moving an execution between lanes leaves no tombstone; an execution is found by name across its deployment's lanes, and names are unique per deployment (§8.3, §9.3).
28. A lane's `path` must be inside the deployment's own root grain folder; registries have no lanes (§10.2.2, §10.3).
29. Definitions are inert in a deployment folder and are never orphans (§10.2.5).
30. A creator maps every definition dependency to an execution, or refuses to create the execution (§11.4).
31. A re-placement carries the old approval when the snapshot is identical, and the new execution is fenced until the old one is superseded or finished (§11.5).
32. A deployment that is gone is withdrawn by its root folder's owner (§11.5).
33. Readers show an execution's thread, its runs' comments and its definition's thread (read at the tip), each labelled with its scope, as sections or one timeline ordered by `at` (§11.2).
34. The hub never writes into a definition's repository to place work (§16.6).
35. `task_home` and its modes are removed: definitions live anywhere, executions only in deployment folders (§18.2).
36. GF041 and GF042 are retired; a stray child is GF007, and GF045–GF048 and GF069 are new (§20, §22.7).
37. Triggers create executions, never definitions; `fires.into` names a lane (§7.6).
