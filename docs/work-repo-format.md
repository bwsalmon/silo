# Work repos: tasks, schedules, templates and designs as Markdown in git

**Status: a proposal for review. Nothing here is built.** No
implementation task is filed until the owner has read it. The example
repositories are in [`examples/work-repos/`](../examples/work-repos/README.md),
and the JSON Schemas are in [`docs/work-repo-schemas/`](work-repo-schemas/).

This design changes where the hub ([hub.md](hub.md)) and its Grains keep
their work and how they hand it to each other. Today grain keeps tasks,
runs and comments in its own database. It exports settings as JSON and
YAML to a state repository (grain/config), and repositories declare
standing work as YAML under `.grain/`. The hub keeps its backlog in its
own database (hub.md §6.1) and offers tasks over the rendezvous
([hub-interface.md](hub-interface.md)).

The proposal: **every task, schedule, template, design and comment is one
self-contained Markdown file in a git repository.** Those repositories
reference each other, and together they form the backing store for both
sides and the durable channel between them. The rendezvous stays in
place, carrying only what has to be fast.

Grain's data model already points this way ("Direction: the declaration
moves into a repo", bwsalmon/grain `docs/data-model.md`, and its rule
"whoever writes it, owns it"). This document makes that direction
concrete and extends it across the Silo/Grain boundary.

## 1. The model

### 1.1 A graph of repos

The world is a set of git repositories that hold Markdown objects
referencing each other. A repository's **role** is a convention recorded
in its README metadata (§3). It is not baked into the format, and every
tool works from references alone. The conventional roles are:

| Role | How many | Holds | Written by |
| --- | --- | --- | --- |
| `grain` | one per Grain instance | **The Grain's backlog**: tasks placed on it or filed by it, their threads and closing snapshots, and the Grain's own schedules and templates | the Grain, and Silo for the parts §5.2 gives it |
| `silo-intake` | one or more per hub | unplaced tasks, proposals awaiting approval, and placement tombstones | Silo |
| `shared` | any number | templates, schedules and designs a team or the hub shares | the owning team's humans (by pull request), never agents |
| `archive` | optional | closed tasks moved out of a busy repo (§10) | whoever owned them |
| `code` | any | ordinary code repos, which may keep designs under `docs/design/` and declarations under `.grain/` | as today |

"Silo" is **one or more repos**: a small hub has one intake repo that
also holds its shared templates, while a large one can split intake,
shared templates and designs across several repos. Nothing reads a role
in order to work; roles only drive default write rules and the UI.

### 1.2 What goes in git, and what does not

Git holds what people **author, decide or need to read later**:
definitions (templates, schedules), intent (a task's spec and prompt),
decisions (approval, cancel, placement), designs, the human-readable
thread, and a final-state snapshot of each task. Each of these is written
a handful of times in its life.

**High-churn runtime state stays in a database**: run attempts, sandboxes
and leases, live status, events, transcripts and logs. It stays in the
Grain's database, or in the hub's database for what the hub already
keeps (hub.md §6.1, §7). Committing it would rewrite the busiest files
every few seconds. Grain already refuses to export these rows to
grain/config for that reason. §5.3 lists the split field by field.

The rendezvous WebSocket stays for low latency: "new commit, pull now"
notifications, live events and the view-through (§8.4). **Git is the
durable mailbox and the record. The WebSocket only makes it fast.** If a
notification is lost, the object arrives on the next poll anyway.

## 2. Objects

### 2.1 One file per object

Each object is a UTF-8 Markdown file with YAML front matter between `---`
lines and a Markdown body:

```markdown
---
kind: task
schema: grain/task@v1
id: silo/task-1207
title: Check requirement IDs in `grain declarations check`
# ... kind-specific fields
---
The body: a task's prompt, a template's text, a schedule's description,
a design's prose, or a comment's words.
```

These fields are common to every kind:

| Field | Required | Meaning |
| --- | --- | --- |
| `kind` | yes | `repo`, `task`, `comment`, `schedule`, `template`, `design` or `tombstone` |
| `schema` | yes | `grain/<kind>@v<major>[.<minor>]` (§9) |
| `id` | yes, except `repo` | the stable global ID (§2.3) |
| `title` | most kinds | one line |
| `aliases` | no | earlier IDs this object answered to (a hub `t-…` ID, a grain `template-1`) |
| `ext` | no | `{<vendor>: {...}}`, for a customised Grain's own fields (§9) |

**A file must make sense on its own.** A task file carries its rendered
prompt, not just a template reference. A comment names its task, and a
tombstone names where the object went. Someone who finds a single file in
a clone, a diff or an email can tell what it is, whose it is and what it
asks for.

A file is at most 256 KiB, and a repo's README may set a lower limit.
Binary attachments are not objects. They are referenced by URL or stay
in the Grain (as node-only artifacts do today, hub.md §7.1).

### 2.2 Layout is loose

Tools find objects by **scanning front matter, never by path**. A file
without front matter, or whose `kind` the reader does not know, is not an
object. The only fixed paths are each repository's root `README.md` (§3)
and, for compatibility, `.grain/` (§12.1). The recommended layout, which
the examples follow, is:

```
README.md                         kind: repo
tasks/<authority>/task-<n>.md     tasks, by ID, so one task never moves within a repo
threads/<authority>/task-<n>/<time>-<writer>.md   comments, one file each
schedules/<name>.md
templates/<name>.md               templates/partials/ for include-only templates
designs/<name>.md  or  docs/design/<name>/README.md
```

Because tasks are filed by ID, placing a task and later tombstoning it
touch the same path, and moving it never renames anything inside a repo.

### 2.3 IDs

An ID is `<authority>/<kind>-<name>`, for example `silo/task-1207`,
`eu/task-4443`, `eu/schedule-1`, `shared/template-workflow-review` or
`grain/design-sample`.

- **The authority** is a short name, `[a-z][a-z0-9-]{1,30}`, that one
  writer allocates under. Each authority is registered once in the hub's
  directory, as hub.md §5.2 node IDs are, and declared as `id-authority`
  in the README of the repository it allocates in. Silo allocates
  `silo/…`, a Grain allocates under its node ID (`eu/…`), and a shared
  repo under its own name.
- **The name** is a decimal counter for tasks, as grain does today
  (`task-4443`). Other kinds may use a slug (`template-workflow-review`)
  because people type them. A counter lives with its allocator: the hub's
  database, the Grain's database, or for a shared repo the highest number
  already in the repo plus one, checked again on push.
- **IDs never change and are never reused.** Moving an object (to
  another Grain, or to an archive) keeps its ID, and a re-placed task is
  still `silo/task-1207` in its new home. Uniqueness only has to hold
  across the authorities one hub registers. A graph that spans two hubs
  needs distinct authority names, and the directory refuses a name that a
  peered hub has already registered.
- **Comment IDs** add to their task's ID: `eu/task-4443/c-20261016t175300z-grain`
  (the UTC time and the writer). Two writers can therefore append to one
  thread without either one allocating a number.
- Today's IDs carry over as `aliases`. The hub's `t-9f2c…` IDs and
  grain/config's `template-1` and `sched-1` become `silo/task-…`,
  `eu/template-1` and `eu/schedule-1` (§12.3).

A task's branch keeps being derived from its ID (`grain/eu/task-4443`),
not self-reported (grain `docs/data-model.md`, rule 3). For a hub task the
derivation is `grain/<grain>/silo-task-1207`.

## 3. Repo metadata: the README front matter

Every work repo's root `README.md` starts with front matter of
`kind: repo`, followed by prose for humans. This is the repo's
declaration of who it is, who may write what, and where its neighbours
are. A reader fetches it first, and a change to it is a reviewed human
pull request. **Silo and Grains never write a README.** For the full
schema see [`repo.schema.json`](work-repo-schemas/repo.schema.json).

```yaml
---
kind: repo
schema: grain/repo@v1
role: grain                          # convention: grain | silo-intake | shared | archive | code
title: Grain "eu" backlog
owner: {grain: eu, principal: "user:bsalmon@google.com"}
hub: https://silo.example.com        # the hub this repo answers to (its claims keys at /v1/claims/keys)
id-authority: eu                     # what IDs this repo may allocate (§2.3)
refs:
  default: main                      # an unpinned reference means this branch
  protected: true                    # force-push and deletion refused by the forge
writers:                             # who can push, and how a reader knows them (§11)
  silo: {forge-identity: "app/silo-hub", attestation: hub}
  grain: {forge-identity: "app/grain-eu"}
write-rules:                         # first matching rule wins; anything unmatched is nobody's
  - match: {kind: task, id: "silo/*"}
    create: [silo]
    fields: {spec: silo, claims: silo, control: silo, status: grain, snapshot: grain}
  - match: {kind: task, id: "eu/*"}
    create: [grain]
    fields: {"*": grain}
  - match: {kind: comment}
    create: [silo, grain]            # append-only: nobody edits or deletes a comment
  - match: {kind: tombstone, id: "silo/*"}
    create: [silo]
  - match: {kind: [schedule, template]}
    create: [grain]
related:
  intake: github.com/bwsalmon/silo-intake
  shared: [github.com/bwsalmon/silo-shared]
  team: default
forwarding:                          # where objects that left went (§4.4)
  - {from: "tasks/**", to: "github.com/bwsalmon/grain-eu-archive/tasks/**", before: 2026-07-01}
reads-allowed: [github.com/bwsalmon/silo-intake, github.com/bwsalmon/silo-shared]
---
# Grain "eu"
Human prose…
```

**Write ownership is partitioned by object, and within a task by
top-level block.** A rule names who may create an object and, for a task,
which writer owns each block of its front matter (§5.1). The body belongs
to the creator. The rules are enforced twice:

1. **By credentials.** Only the writers named in the README can push.
   That means branch protection, plus the forge identities given to Silo
   and to the Grain. Agents and humans are never among them, because an
   agent that could push a task repo could grant itself capabilities in
   it (grain `docs/data-model.md`, "the trust gate"). Humans change a
   work repo through Silo's or the Grain's UI, or through a pull request
   that a writer merges.
2. **By validation on read.** A reader compares each changed object
   against the write rules at the commit that changed it (§11.2). A
   change outside its writer's partition makes the object **invalid**:
   the reader keeps its last valid version and reports the violation.
   **A Grain treats content that no authorized writer wrote as
   invalid.**

`reads-allowed` lists the only other repos a reader of this repo may
pull in while resolving references (§4.5). A reference to anything else
stays unresolved, whatever credentials the reader holds.

## 4. References

### 4.1 Syntax

There are two forms, and only two:

- **Within a repo:** a relative path, with an optional anchor:
  `../templates/release.md`, `threads/eu/task-4443/`, `#D-2`.
- **Across repos:** `github.com/<owner>/<repo>/<path>[@<ref>][#<anchor>]`,
  for example
  `github.com/bwsalmon/grain/docs/design/sample/README.md@fe6bd82de#D-2`.
  The host is part of the reference, so another forge is
  `gitlab.example.com/<group>/<repo>/…`. The first two segments after the
  host are always the owner and the repo. A host whose repo paths are
  deeper declares how many segments it uses in the hub's directory.

`@ref` pins the reference to a **branch, tag or full or abbreviated
sha**. A reference without a pin means the target repo's `refs.default`
at the moment it is read. `#anchor` is a Markdown heading ID: either a
requirement anchor such as `{#D-2}` (§7) or a heading slug.

A front-matter field that holds a reference takes either a string or
`{ref: <reference>, id: <expected id>}`. The `id` form is recommended
for every reference that crosses repos, because it lets the reader check
that it found what the writer meant (§4.4). A reference in a body is an
ordinary Markdown link. It is shown, and it is followed only by tools
that are asked to follow it.

### 4.2 The edges

| From | Field | To | Pinned? |
| --- | --- | --- | --- |
| task | `spec.template: {ref, id, inputs}` | template | **yes**: the filer pins the sha it rendered (§6.3) |
| task | `spec.depends-on: [{ref, id}]` | task, possibly in another Grain's repo | no: dependencies are live |
| task | `spec.origin.schedule: {ref, id, fired-at}` | schedule | yes, to the sha that fired |
| task | `spec.implements: [{ref: …#D-2, rev, digest}]` | design requirement | `rev` pins the doc revision (§7.3) |
| task | `spec.supersedes: [{ref, id}]` | task | no |
| schedule | `template: {ref, id, inputs}` | template | no (pinned when it fires) |
| template | `{% include %}` and `{% from … import %}` | template | optional |
| comment | `on: {ref, id}` | task | no |
| tombstone | `moved-to: {ref, id}` | the object's new location | no |
| design | `supersedes`, `applies-to` | design, repos | no |

### 4.3 Resolution

To resolve a reference, a reader (1) checks that the target repo is
readable under the policy of §4.5, (2) fetches the repo at the ref, (3)
reads the file and parses its front matter, (4) checks `id` if the
reference gave one, and (5) if the file is a tombstone, follows
`moved-to`. Each answer is one of `found`, `moved` (with the chain
followed), `unresolved` (could not fetch or read; retryable) or `invalid`
(found, but failing its schema or write rules).

### 4.4 Cycles, dangling references and moved targets

- **Moved objects** leave a tombstone (`kind: tombstone`, same ID, same
  path) that names where they went. A whole subtree that moved, such as
  an archive or a renamed repo, is covered instead by a `forwarding` rule
  in the README, so an archive move does not leave thousands of
  tombstones. A reader follows at most **8** tombstone or forwarding
  hops and treats a repeat as a cycle.
- **Dangling references** (no such repo, file or anchor) and an `id`
  mismatch are `unresolved`, not fatal. Because there are no cross-repo
  transactions (§8.1), a reference is allowed to dangle for a while.
  Readers retry with backoff for a **grace of 10 minutes**, then report
  the reference in the UI and on the object's thread. Unresolved
  references never pass silently. **A dependency that is unresolved is
  not satisfied**, and a template that is unresolved cannot render, so
  the task waits rather than running without it. Every unresolved state
  fails closed.
- **Lookup by ID.** When a path fails and no tombstone or forwarding rule
  explains it, Silo's index (id → current location, rebuilt from git;
  §10) may suggest the object's new location. The suggestion is a hint,
  and the reader verifies it by fetching the file.
- **Cycles.** Silo refuses to place a task whose `depends-on` closes a
  cycle within its index. A Grain that finds a cycle among the tasks it
  can see parks the tasks involved with the reason. Template includes are
  limited to a depth of 4 and refuse to include anything already on the
  include stack. A design `supersedes` chain is checked the same way as
  tombstones.

### 4.5 Resolution needs credentials, and stops at limits

Following a link works only where the reader may read. **Nothing pulls a
repo the reader is not authorized for into its view.** For a Grain, that
means four rules:

- A Grain resolves references using its own forge credential, and only
  into its own repo, the repos in its README's `reads-allowed`, and the
  repos its local policy names. A reference that points anywhere else
  is unresolved by definition, even if the Grain could fetch it. The
  write-only partner repos named in task specs (`reads:`, `writes:`) are
  a task's sandbox scope, not the Grain's resolution scope.
- **Anything the Grain pulls in is a claim.** A template pulled from a
  shared repo, a dependency's state and a requirement's text are all
  inputs. The Grain applies its local policy to the task that results,
  exactly as it does today to the hub's claims (hub.md §1.2).
- Silo resolves with its own credentials, and shows a user only what
  that user could see in the hub's directory (INV-9). It never copies the
  content of a repo a viewer cannot see into one they can. Team views
  (§8.5) go through this filter.
- Limits: no more than 16 repos fetched per resolution, 8 hops, 4
  include levels, and a 30 s fetch budget. Blobless partial clones are
  used (§10).

## 5. Tasks

### 5.1 Blocks and their owners

A task's front matter is grouped into **top-level blocks with one owner
each**, so the README's write rules can name them, and two writers never
edit the same lines:

| Block | Owner (Silo-placed task) | Holds |
| --- | --- | --- |
| identity (`kind`, `schema`, `id`, `title`, `aliases`) and the **body** (the rendered prompt) | the creator | written once. Changing intent means filing a new task that `supersedes` this one |
| `spec` | the creator | target repo and base, template and inputs, `depends-on`, `implements`, `origin`, requested `capabilities`, `reads`, `writes`, `auto-merge`, `model`, sandbox size, network |
| `claims` | Silo | `team`, `submitter`, `approved-by`, `placement: {grain, epoch, at}`, `dependencies` (the state of each dependency, as Silo last saw it), `attestation` (the signed JWT, §11.3) and `seq` |
| `control` | Silo | `approval`, `cancel`, `defer-until` and `seq`: the decisions humans make at the hub |
| `status` | the Grain | `phase`, `at`, `reason`, `node-task`, `seq`: coarse, git-worthy transitions only |
| `snapshot` | the Grain, once | the final state, written on close |

For a task a Grain filed itself (an `eu/…` ID), every block belongs to
the Grain. Its owner's local approvals and cancels are recorded in the
same blocks.

Each writer changes a task the same way: fetch the tip, parse the file,
replace only its own blocks, write the file back with canonical
serialisation (keys in schema order, blocks in the order above), and
push as a fast-forward. If the push is rejected, it repeats from the
fetch. **No textual merge ever runs on a work repo.** Writers re-apply
their own blocks onto the new tip instead, so two writers never conflict.

Each owned block carries a `seq` counter that its writer increments on
every change. A reader refuses a block whose `seq` went backwards, which
defends against replayed commits (§11.4).

### 5.2 Phases in git, states in the database

`status.phase` changes only at transitions that someone else needs to
act on:

```
(no status: placed, not yet answered)
  → accepted → waiting ⇄ accepted → closed
  → refused                                 (Silo re-places it: §8.2)
```

`waiting` means the task is parked on a human: a question, an approval
or a merge it cannot do itself. `status.reason` says which. A task that
nobody approved is identified by `control.approval` being absent, not by
a phase. Queued, running, retrying and awaiting-CI are **runtime
states**, held in the Grain's database and streamed as events. The hub's
own task states (hub.md §6.1) become a projection of the phase plus live
events.

### 5.3 Git-backed and runtime-only fields

| In git (the task file or its thread) | Runtime only (database and events) |
| --- | --- |
| title, prompt, spec, the template ref and inputs | run attempts, sandboxes, leases and backends |
| team, submitter, approval, placement and its epoch | live status: queued, running, retries, awaiting CI |
| cancel, defer-until | event streams, transcripts, tool calls, logs, diffs |
| phase transitions with their reasons | heartbeats, slot counts, the offer and refusal history |
| comments: questions, answers, closing notes | the PR's live checks and mergeability (GitHub owns these) |
| **snapshot**: outcome, closed-at, pull request, merge commit, branch, `implemented` with its revs, summary, closing comment | token costs, model usage |

The **snapshot** is written in the same commit as `phase: closed`:

```yaml
snapshot:
  outcome: done                    # done | failed | cancelled | nothing-needed | superseded
  closed-at: 2026-10-14T09:12:40Z
  pull-request: github.com/bwsalmon/grain/pull/3901
  merge-commit: 4be1c0a7d…
  branch: grain/eu/silo-task-1207
  implemented: [{ref: "…#D-2", rev: fe6bd82de, digest: "sha256:…"}]
  summary: One line on what was done.
  closing-comment: {ref: "threads/silo/task-1207/2026-10-14t091240z-grain.md"}
```

### 5.4 Comments

Each comment is a file (`kind: comment`) with `on:` (its task), `author`
(a principal, such as `user:…`, `node:eu`, `agent:eu/task-4443` or
`system:eu`), `via` (the writer that committed it: `silo` or `grain`),
`at`, and a `role` (`note`, `question`, `answer`, `closing` or
`review`). Files are only ever created, never edited, so appends never
conflict.

A human who answers a question in the hub's UI produces a Silo-written
comment in the Grain's repo, followed by a notification. A human who
answers in the Grain's own UI produces a Grain-written comment. Either
way, `via` must match the writer that pushed the comment (§11.2). A
comment that claims a human author can only come from the writer that
authenticated that human.

## 6. Templates and schedules

### 6.1 Template front matter

```yaml
kind: template
schema: grain/template@v1
id: shared/template-workflow-review
title: "Review the {{ workflow }} run on {{ branch }}"
inputs:
  - {name: workflow, type: string, required: true}
  - {name: branch,   type: string, default: main, options: [main, candidate]}
  - {name: jobs,     type: list,   default: []}
partial: false            # true: include/import only, never filed directly
defaults:                 # task spec defaults a filer may override
  auto-merge: false
  capabilities: [read-issues]
```

An input's `type` is `string`, `int`, `bool`, `list` (of strings) or
`map` (string to string). `default`, `options` and `required` mean what
grain's `fields:` mean today (bwsalmon/grain `docs/declarations.md`).
A required input with no value is an error, and so is a value outside
`options`. The title and body are both rendered.

### 6.2 A strict Jinja2 subset

The template language is a **strict subset of Jinja2**, small enough to
review. Today's `{{name}}` placeholders (`pkg/repodecl/parse.go`) are
already valid in it.

- **Allowed:** `{{ expr }}` with variables, string, number and list
  literals, `[]` indexing and `.` lookup on inputs only. Comparison,
  `and`, `or`, `not`, `in`, `~` and `+` on strings and lists.
  `{% if %}`, `{% elif %}`, `{% else %}`, `{% for x in list %}` (with
  `loop.index`, `loop.first` and `loop.last`), `{% set %}`,
  `{% macro %}`, `{% raw %}`, comments, and whitespace control (`-`).
- **Filters:** `default`, `lower`, `upper`, `title`, `trim`,
  `replace`, `indent`, `join`, `length`, `first`, `last`, `sort`,
  `unique`, `tojson`, `list` and `string`. No others.
- **Includes and imports by reference:**
  `{% include "github.com/bwsalmon/silo-shared/templates/partials/closing.md@v3" %}`
  and `{% from "../partials/closing.md" import closing %}`. The included
  target must be a `kind: template`. Its front matter is stripped, and
  its own inputs must be a subset of the includer's. The include is
  resolved under §4.5 with the includer's credentials, and a repo that
  is not allowed fails the render.
- **Not allowed:** method calls, attribute access on anything but input
  maps, `extends` and blocks, `call` blocks, tests other than
  `defined`, `none` and `in`, `namespace`, globals (`range`, `dict`,
  `lipsum`, `cycler`), loop controls, and expression statements. The
  parser rejects these **at load**, so a template that uses one never
  fires.
- **Undefined is an error** (`StrictUndefined`). Nothing is reachable but
  the declared inputs, plus a fixed, documented set of filing-time
  variables such as a trigger's `from_sha` and `to_sha`, or a schedule's
  `fired_at`. Each of those must also be declared as an input.
- **Limits:** output of at most 256 KiB, include depth of 4, at most
  10,000 loop iterations, and 1 s of render time.

**Implementation:** gonja (`github.com/nikolalohinski/gonja/v2`), with a
pre-parse pass that walks the AST and rejects everything outside the
subset. Fixtures (`testdata/jinja/<case>.{md,json,out}`) are rendered by
gonja and by Python Jinja2 (`SandboxedEnvironment`, `StrictUndefined`,
`keep_trailing_newline=True` and the default block trimming). CI fails
on any byte difference. The subset is defined as what renders the same
under both, so a fixture where they disagree means narrowing the subset,
not documenting the difference.

**Migration hazard.** Some bodies today hold literal `{{`. A prompt that
quotes GitHub Actions' `${{ github.sha }}` is one example. The exporter
(§12.3) parses each body. Any `{{…}}` that is not one of the template's
declared fields is wrapped in `{% raw %}`, and the change is listed in
the export report.

### 6.3 Rendering happens once, when the task is filed

The filer (Silo, a schedule firing in a Grain, or the planner) renders
the template and writes the **rendered** title and prompt into the task
file. It also records `spec.template: {ref: …@<sha>, id, inputs}`. The
task file is therefore self-contained, and the Grain does not need to
read the template repo in order to run the task. A Grain that can read
the template may re-render it and refuse the task on a mismatch. That
check is optional, and it only makes sense where the template repo is in
the Grain's `reads-allowed`.

### 6.4 Schedules

A schedule (`kind: schedule`) keeps the cadence grammar of today's
declarations (`daily: {at: "09:00"}`, `weekly`, `every`, `timezone`,
`overlap`, `base`, `target`, `enabled`). `template` is a reference with
`inputs`, and the body is the schedule's description for humans. **The
Grain that owns the schedule runs it.** Schedules live in Grain repos,
or in a shared repo with `runs-on: {grain: eu}` or `{team: infra}`. In
the second case Silo files each firing into intake, and placement goes
on from there. `next-run-at` and `last-run-at` are runtime only and are
not in the file. Each firing writes a task whose `spec.origin.schedule`
pins the schedule's sha.

## 7. Design docs

### 7.1 `kind: design`

```yaml
kind: design
schema: grain/design@v1
id: grain/design-sample
title: Design-doc checks in `grain declarations check`
status: accepted            # draft | proposed | accepted | implementing | implemented | superseded | withdrawn
owners: ["user:bsalmon@google.com"]
applies-to: [github.com/bwsalmon/grain]
supersedes: []
requirements:
  prefix: D
  next: 3                   # the next number to allocate
  retired: [D-1]            # never reused
planning:
  enabled: true
  target: {repo: bwsalmon/grain, base: main}
  auto-approve: false
  max-tasks-per-change: 20
```

The body is ordinary design prose. Each requirement heading ends with a
stable anchor, as `{#HS-n}` and `{#D-7}` do today (bwsalmon/grain
`docs/design-docs.md`): `### Retag latest on promotion {#D-7}`. One
requirement per heading, and a requirement's section runs to the next
heading at the same or a higher level.

A design can live anywhere: in a `shared` or Silo repo, or in a code repo
under `docs/design/`. Wherever it lives, it is referenced across repos
the same way. A design file without front matter, at a path that a
`.grain/triggers` design trigger watches, is still read as a design
(legacy mode, §12.1). Its prefix is inferred, and its retired IDs are
whatever git history shows.

### 7.2 Allocating requirement IDs

The next ID is `<prefix>-<requirements.next>`. Whoever adds a
requirement increments `next` in the same change, and the planner does it
for headings that arrive without an ID, as it does today. A removed
requirement's ID goes into `retired`. Validation rejects a change that
reuses a retired ID, reuses an ID found anywhere in the doc's git
history, lowers `next`, or uses the same anchor twice. Because `retired`
and `next` are written in the file, they survive a squash, a
history-losing move (silo's split from grain lost exactly that history)
or a copy into another repo.

### 7.3 Two-way traceability

A task points at its requirements with `spec.implements`. Each entry
records `rev`, the sha of the doc revision the task was filed against,
and `digest`, a sha-256 of the requirement's **normalised section**: the
heading through the end of the section, with the `{#ID}` marker removed,
trailing whitespace stripped, runs of blank lines collapsed, and a final
newline. The closing snapshot's `implemented` records what was actually
built. The implementing commit keeps its trailers unchanged:

```
Design-Item: docs/design/sample/README.md#D-2          (same repo: path#ID, as today)
Design-Item: github.com/bwsalmon/grain/docs/design/sample/README.md#D-2   (design in another repo)
Design-Rev: fe6bd82de
```

The trailers map one-to-one onto `implements`: `Design-Item` is the
`ref`, and `Design-Rev` is the `rev`. `Design-Removed:` corresponds to a
task whose `spec.removes` names the requirement.

The reverse direction, requirement → tasks, is **derived, not written**.
Silo indexes `implements` across every repo it reads, together with the
trailers on the target branch. For each requirement it shows its tasks,
their phases and outcomes, and one of:

- `open`: a task is filed and none has closed done;
- `done`: the latest done snapshot's digest equals the current one;
- `changed since implemented`: the latest done snapshot's digest is not
  the current one;
- `removed`: the anchor has gone and is in `retired`;
- `unplanned`: no task.

Nothing writes the trace back into the design file, so designs stay
human-edited. A hub that wants git-only readers to see the trace can
render it into a generated file in its own repo, but it is a view, not a
record.

### 7.4 Planning is a Silo operation

When a design changes on its `refs.default`, Silo's planner (today the
`design-planner` template running in a Grain) diffs the requirements
between the previous planned revision and the new one, and classifies
each ID with the existing table (`docs/design-docs.md`): new; changed and
open; changed and done; removed and open; removed and done; unchanged.
"Changed" means the digest differs from the one recorded in the open
task's `implements` or the done task's `implemented`. Then it acts:

| Class | Action on task files |
| --- | --- |
| new | file an implementation task and a test task that depends on it, both in intake |
| changed, still open | file replacements with `supersedes:`. Once they are approved, Silo sets `control.cancel` on the old ones, wherever they are placed |
| changed, done | file a delta task covering only what changed |
| removed, open | propose cancelling the open tasks, which a human confirms |
| removed, done | file a removal task with `spec.removes` |
| unchanged | nothing |

Every filed task carries `implements` with the new `rev` and `digest`,
and `spec.origin.design-change: {ref: <design>@<sha>, from: <sha>}`. The
filing is idempotent: the key is `(design id, to-sha, requirement,
class)`, so replaying a plan files nothing twice.

**Guards against a flood.** A malicious or accidental design edit must not
become a hundred running tasks:

- Only a design on its declared default ref plans, so a pull-request
  branch never does. Only designs whose repos the hub has registered for
  planning plan at all.
- Every planned task is **proposed**, and it needs a human approval
  (INV-17), unless `planning.auto-approve` is set **and** the editor of
  the change is one of the design's `owners` **and** the change is within
  the limits.
- Limits: `max-tasks-per-change` (default 20), at most 50 tasks per design
  per day, and at most 100 planned tasks per team per day. A change over
  any limit is held as a whole. Its tasks are filed with
  `control.hold: plan-limit` and `spec.origin.plan: <id>`, and one approval
  by a design owner who is not the editor releases them together.
- A change that removes more than 5 requirements, or rewrites `retired`
  or `next`, is held the same way.

## 8. Communication through repos

### 8.1 No cross-repo transactions

Git offers no atomic commit across repos. Every multi-repo operation is
therefore a **fixed sequence of single-repo commits** that can be retried
step by step. Each step is idempotent and keyed by the task ID and the
placement epoch. Readers tolerate the states between steps. Silo records
the operation in its database before step 1, so a crashed operation is
finished on restart by redoing the steps whose effects are not yet
visible in git.

**Place.** (1) Silo writes the task file into the chosen Grain's repo,
with `claims.placement.epoch = n` and a fresh attestation. (2) Silo
notifies the Grain. (3) Silo replaces the intake copy with a tombstone
pointing at the placed file. Between steps 1 and 3 the ID is live in two
repos. Readers apply the rule **a copy in a `grain` repo beats a copy in
intake, and a higher epoch beats a lower one**. The Grain answers by
writing `status.phase: accepted` or `refused`, then notifying back.

**Re-place** (a refusal, or a Grain lost past its team's grace,
hub.md §6.3). (1) Silo writes the task into the new Grain's repo with
epoch `n+1` and `spec.origin.replaced-from` naming the old location. (2)
Silo notifies the new Grain. (3) Silo writes a tombstone at the old path
with epoch `n+1`, `moved-to` naming the new location, and the old
file's `status` copied in as `last-status`. If the old Grain returns, it
reads the tombstone, or a higher epoch elsewhere, as a cancel. It stops
the task and writes nothing more about it. A result it pushed between
steps 1 and 3 is preserved in `last-status` and posted to the new
copy's thread as a comment. That matches what hub.md §6.3 does with a
late result today: it is kept as a record, but it does not count.

**Approve.** Silo writes `control.approval` (bumping `control.seq`) in
the file wherever it currently lives, then notifies. If a placement
races the approval, the placement step copies `control` from intake at
its step 1. Silo re-reads intake after step 3, and if the approval
landed in between, it writes the approval into the placed copy.

**Cancel.** Silo writes `control.cancel`, then notifies. The Grain stops
the task and closes it with outcome `cancelled`.

**Dependency completed in another Grain.** The task in Grain A depends on
one in Grain B. (1) Grain B closes its task (`phase: closed` and a
snapshot in one commit), then notifies. (2) Silo reads it and writes
`claims.dependencies[i] = {ref, state: done, outcome, at, commit}` into
A's task file. (3) Silo notifies A. A treats the dependency as satisfied
only through that claim, or by reading B's file directly if B is in its
`reads-allowed`. Rewriting the same state is a no-op.

**Grain-filed proposal.** An agent in Grain A proposes a task that needs
the hub, for example to place it elsewhere or to get a hub approval. (1)
A writes it in its own repo with `spec.route: hub`. (2) Silo copies it
into intake under the **same ID** (IDs survive moves). (3) A replaces
its own copy with a tombstone pointing at intake.

**Close.** The Grain writes the snapshot and `phase: closed` in one
commit, and its closing comment in the same push. It then notifies.
Silo updates its index, its team views and any dependents.

### 8.2 Who writes what, in summary

| Object or block | Silo | Grain |
| --- | --- | --- |
| task file creation, body, `spec` (hub tasks) | ✔ | |
| `claims`, `control` (hub tasks) | ✔ | |
| `status`, `snapshot` | | ✔ |
| everything in a Grain-filed task | | ✔ |
| comments | ✔ (hub UI authors) | ✔ (its agents, system, local UI) |
| tombstones | for `silo/` IDs | for its own IDs |
| intake repo | ✔ | |
| schedules and templates in its own repo | | ✔ (from its owner's edits) |
| READMEs, shared templates, designs | humans, by reviewed pull request | |

### 8.3 Notifications on the rendezvous

Two control-stream messages are added:

```
hub → node  {"type":"pull","repo":"github.com/…","ref":"main","sha":"<new tip>","ids":["silo/task-1207"]}
node → hub  {"type":"pushed","repo":"github.com/…","ref":"main","sha":"<new tip>","ids":["silo/task-1207"]}
```

Both are hints. The receiver fetches, and acts on what git says rather
than on the message. A missed message costs no more than the poll
interval: the Grain polls its own repo every 60 s, and Silo polls every
known repo every 5 minutes as well as on webhooks.

### 8.4 What changes in hub-interface.md

The contract keeps its shape: connection, control, event, view and http
streams. On the **task stream**, the offer becomes a pointer plus a
commit:

- `offer` and `resume` carry `{"task":{"id":"silo/task-1207","ref":"github.com/…/tasks/silo/task-1207.md@<sha>"}}`
  instead of the whole `tasks.Spec`. The Grain fetches that commit and
  reads the file. The spec, the claims and the attestation all come from
  the file, and `requester_attestation` moves into `claims.attestation`.
- `accept` and `refuse` are written as `status.phase` in git. The message
  remains as the fast path, and the hub records whichever it sees first.
  Both carry the same epoch.
- `result` becomes the Grain's closing commit plus `pushed`. `cancel`
  becomes `control.cancel` plus `pull`, and the cancel message remains as
  the fast path.
- Event, view and http streams are unchanged. Live events never touch
  git.

During the transition a Grain declares `"work_repo": "github.com/…"` in
`hello`. The hub uses the git path only for Grains that declare one, and
offers inline `tasks.Spec` to the others. hub-interface.md gains a
"Work repo" section saying exactly this when stage S2 (§14) ships, and
not before, because it documents what is built.

### 8.5 Team views

A team view lists every task of a team across intake and all of its
Grains' repos. **Silo aggregates it**, from an index it rebuilds from git
(ID, kind, location, phase, claims, `implements`, `depends-on` and
snapshot) together with live events. The index is disposable: deleting
it and re-reading the repos reproduces it. Visibility is the hub's
existing rule (INV-9). Silo never reads a repo on a viewer's behalf
beyond what that viewer could already see.

## 9. Schemas and versioning

There is one JSON Schema per kind, in
[`docs/work-repo-schemas/`](work-repo-schemas/): `repo`, `task`,
`comment`, `schedule`, `template`, `design` and `tombstone`. The front
matter is parsed as YAML 1.2 (core schema, so `on` and `no` are strings),
converted to JSON, and validated.

- `schema: grain/<kind>@v<major>.<minor>`, where a missing minor means
  `.0`. **Minor** versions only add optional fields. A reader that knows
  a lower minor ignores fields it does not know, and **a writer
  preserves them**, since it only ever rewrites its own blocks. **Major**
  versions may rename or remove fields. Readers keep an up-converter for
  every major version they have ever shipped, applied in memory. Old
  files are never rewritten in bulk; a file moves to the new major
  version when its owner next writes it.
- A reader that meets a **higher major** version than it knows treats the
  object as `invalid` and reports it. It does not guess.
- `ext.<vendor>` carries a customised Grain's own fields, which other
  readers ignore. It is validated only by its vendor.
- Files without `schema:` exist only under `.grain/` (YAML) and under
  design paths (legacy Markdown), and are read with today's parsers
  (§12.1).

## 10. Scale

- **Repo size.** One task, with its thread, is a few KiB. A Grain that
  closes 300 tasks a day adds about 10 MB a year before compression. A
  closed task stays in place for 90 days. After that, its owner moves
  closed tasks in batches to an `archive` repo (one per year) and adds a
  `forwarding` rule. Git history keeps everything, and blobless clones
  never download it.
- **Fetch cost.** Readers use `git clone --filter=blob:none` and
  incremental fetches, triggered by notification rather than by tight
  polling. A Grain reads only its own repo, plus the templates and
  designs it chooses to verify.
- **Commit batching.** Each writer coalesces its changes into **one commit
  per repo per ≤ 2 s window, capped at 100 files**, and pushes once.
  Silo's dispatcher already batches its work by team.
- **Writing into many Grain repos.** Silo keeps a bare, blobless mirror of
  each Grain repo and builds commits with git plumbing (`hash-object`,
  `mktree`, `commit-tree`), without a working tree. It pushes one repo
  per goroutine with a bounded pool (default 32), and uses a per-repo
  lease so that two replicas never push one repo at once (hub.md §2.2).
  A rejected push is followed by refetch, re-apply and retry, at most 5
  times, after which it waits for the next tick.
- **Forge limits.** Pushes and fetches go through the git protocol, not
  the REST API. Webhooks are an optimisation, and the poll stays as the
  floor.

## 11. Security

This section extends [threat-model.md](threat-model.md), whose TB3 (hub →
Grain) boundary gains git as a second carrier.

### 11.1 Secrets are never stored

No object holds a credential. Capabilities are referenced **by name**
(`gcp-key`, `repo-read:gke-labs/granule`, `read-issues`), and only the
Grain's local policy turns a name into material (grain
`docs/data-model.md`, "the model holds names, never material"). Silo
scans everything it writes with a secret scanner and refuses a commit
that would hold a match, including in a human's comment. A Grain does
the same for comments it writes. The attestation in `claims` is a
signature, not a secret.

### 11.2 Who may write, and validation on read

The README declares the writers. The forge enforces who can push, and
readers check the write rules on every change. The trouble is that git
does not record who pushed: a commit's author is whatever the writer
typed. **Validation on read can therefore only be as strong as the
evidence it is shown.** Today that evidence is the following:

- **Silo's blocks carry a signature.** `claims.attestation` is the hub's
  JWT (hub.md §6.4), signed for this Grain's node ID. Its `spec_digest`
  covers the canonical serialisation of the identity, the body, `spec`,
  `claims` (minus the attestation) and `control`, and its claims include
  `epoch` and `seq`. A Grain whose policy names `silo_issuer` verifies it
  and refuses a Silo-owned block without a valid one, so **a task file
  that Silo did not sign is not a Silo task**. Its `exp` is the
  placement time plus 7 days. A task that waits longer is re-signed by
  Silo, which bumps `claims.seq`.
- The Grain's own blocks are trusted by Silo the way a Grain's results
  are today: as its account of its own work (T-TB2-6, an accepted risk).
- Comments are bound to their writer by `via`. A Silo-written comment
  carries a short signature of its own (the same key, with `kid` and a
  digest of the comment).

**The natural upgrade is signed commits.** Every writer would sign with
a key declared in the README (`writers.<name>.signing-key`: SSH or
Sigstore gitsign). A reader then verifies each commit's signer against
the write rule for every path the commit touches. That would make
validation-on-read complete without per-object signatures. It is not
needed for v1, because the attestation already covers Silo's blocks, the
only ones a Grain acts on. It is the stage S6 item (§14).

### 11.3 How a Grain knows a task came from Silo

A placed task is acted on only if all of these hold:

1. It arrived on the protected default branch of the Grain's own repo.
2. Its `claims.attestation` verifies against the hub's keys, its `aud` is
   this Grain, and its `spec_digest` matches the file.
3. Its `epoch` and `seq` are at least as high as any the Grain has seen
   for that ID.
4. Its local policy allows it (INV-12).

**The accepted risk stays as it is.** Grains trust Silo's claims, and a
compromised hub can forge a task within the bounds of each Grain's local
policy (hub.md §1.2, T-TB3-1).

### 11.4 Threats

| Threat | Defence |
| --- | --- |
| A rogue or forged writer (a leaked forge token, a human with push) | Branch protection limits pushes to the declared writers. Silo blocks without a valid attestation are refused (§11.3). A Grain's own blocks are only its own account. Signed commits (S6) close the rest |
| An agent writing a work repo | Agents never hold a work repo's credential. The git proxy refuses every work repo for sandbox use (a new invariant, INV-24) |
| **Replay of old commits** (an old approval pushed again, or force-push to an older tip) | Force-push is refused on protected refs. Readers refuse a `seq` or `epoch` that went backwards, and the attestation's `iat` must be newer than the last one seen. A Grain remembers the last tip it processed and requires a fast-forward |
| **Template injection** | Inputs are data: a rendered value is never parsed again, so `{{` inside an input stays literal. The subset has no method or attribute access and nothing but declared inputs, and includes are resolved under §4.5. Prompt injection through input text is the existing agent risk (TB7), not a template one |
| Reference resolution reaching unauthorized repos | `reads-allowed`, the Grain's local policy, the reader's own credentials, and Silo's per-viewer filtering (§4.5) |
| **A malicious design edit** | Plans only from the default ref, proposed by default, rate-limited, and held for owner approval above the limits (§7.4) |
| Oversized or hostile content | Size limits per file and per resolution. Markdown is rendered as text in the hub UI (INV-15) |
| A lost Grain's late writes | Epoch fencing and tombstones (§8.1) |

New invariants for the threat model's §6, once built: **INV-24**, no
sandbox credential can push a work repo. **INV-25**, a Grain acts on a
Silo-owned block only with a valid attestation for itself, and never on
a lower `seq` or `epoch`. **INV-26**, Silo never writes into one repo
content it read from a repo the target's readers could not see.

## 12. Interop and migration

### 12.1 `.grain/` declarations and `docs/design/`

Converge on one format, the Markdown object of §2. During the transition
both formats are read:

- `.grain/templates/<name>.md` and `.grain/schedules/<name>.md`, with the
  same keys in front matter as today's YAML and the body as Markdown, are
  accepted beside the `.yml` files. A repo may not declare the same name
  in both. `pkg/declschema` remains the one list of what a repo may
  declare, and its rules (no `id`, no firing times, no grants beyond the
  team) carry over unchanged. Repo declarations keep their derived IDs
  (`repo:schedule:bwsalmon:grain:prune-packages`).
- `docs/design/**` stays where it is. Front matter is optional. Existing
  `{#HS-n}` and `{#D-n}` anchors, `Design-Item:`, `Design-Rev:` and
  `Design-Removed:` trailers, and the planner template keep working
  **unchanged**. Adding front matter only adds `retired`, `next`,
  `owners` and `planning`.

### 12.2 grain's database and grain/config

The exporter, `grain export work-repo`, writes:

| Source | Becomes |
| --- | --- |
| `task` rows | `tasks/eu/task-<n>.md`. Title, body, target, base, reads, writes, grants (as capability names), links (`depends-on`, `proposed-by` and `child-of` become `spec` fields), approval, deferral, `auto-merge`, model and sandbox size map to blocks. Closed tasks get a snapshot built from their final run and pull request |
| comments, questions and answers, closing notes | `threads/eu/task-<n>/<time>-<writer>.md` |
| `settings/schedules.yml` entries | `schedules/<id>.md`. `next-run-at`, `last-run-at` and `created-at` stay runtime |
| `settings/templates.yml` entries | `templates/<id>.md`. `fields` become `inputs` |
| runs, observations, leases and CI runs | nothing: they stay in SQLite |
| other `tables/*.json` (repo config, boards, custom fields) | unchanged for now. They are settings, not work |

The round trip is the test: export, import into an empty database, and
export again must produce byte-identical files, with the exporter listing
every field it dropped as runtime. The worked conversion of grain/config's
`sched-1` and `template-1` is in
[`examples/work-repos/grain-eu/`](../examples/work-repos/grain-eu/), and
[`examples/work-repos/ROUNDTRIP.md`](../examples/work-repos/ROUNDTRIP.md)
shows the exact field mapping in both directions.

### 12.3 The hub's database

The hub's tasks (`t-<hex>`) are exported to intake, or to the repo of the
Grain each is placed on, as `silo/task-<n>`, with the old ID in
`aliases`. The `hub/tasks` store keeps the runtime columns it has today,
such as offers, refusals and the reason. It gains a column for each
task's location and epoch, and loses `prompt`, `title` and `constraints`
as columns of record: they are now read from the file.

## 13. Alternatives considered

- **One repo per team** (Grains and Silo all writing one team repo).
  Rejected because write ownership collapses: every Grain could push
  every other Grain's tasks, and the forge cannot restrict pushes by
  path. Per-Grain repos let credentials do half of the enforcement. It
  also concentrates churn and push contention in one repo.
- **A single repo with filtered views.** The same problem, worse: one
  credential boundary for everything, and every reader can see every
  team. Filtering at read time is the hub's job, not something to bolt
  onto git.
- **YAML only**, with no Markdown body. Prompts, designs and threads are
  prose. In YAML they become block scalars that are miserable to review,
  and designs are Markdown already. Front matter plus a body is the form
  `.grain` templates were straining toward.
- **Go `text/template`.** It is Go-only, so the Python grain and tools
  could not render the same template. Its undefined handling is weak,
  and it allows method calls on whatever it is given. Jinja's `{{name}}`
  is also what templates already use.
- **Database only**, which is today's arrangement. There is no
  reviewable, attributable record of what was asked and decided; no
  offline or forkable backlog; and no channel that survives the hub
  being down. Grain's own direction (`docs/data-model.md`) already moves
  declarations into a repo.
- **An API-only task interface without git.** It works, as today's
  rendezvous shows, but every reader needs a live hub, history is
  whatever the hub kept, and a Grain cannot audit what it was told. Git
  gives the durable mailbox, and the API remains as the fast path (§8.3).

## 14. Staged implementation plan

Each stage is one or more tasks, in the repo named. Each stage ships
with tests that cite this document's section, and none starts until the
previous one has merged.

| Stage | Repo | What |
| --- | --- | --- |
| **S0** | silo | Accept this design. Check in the schemas, and add a CI check (`scripts/check-work-repos`) that validates `examples/work-repos/` against them and resolves every reference inside the examples |
| **S1** | grain | `pkg/workrepo`: parse and serialise objects canonically, validate against the schemas, resolve references (§4) with limits and tombstones, check write rules. Add the gonja subset renderer with the cross-implementation fixture suite (Python Jinja2 in CI). Read `.grain/*.md` declarations alongside `.yml` |
| **S2** | grain | `grain export work-repo` with the round-trip test against grain/config, and a Grain that mirrors its own tasks, threads and snapshots into its repo, **write-only**, with the database still authoritative |
| **S3** | silo | `hub/workrepo`: an intake repo writer, the plumbing-based multi-repo pusher with per-repo leases, the git-rebuilt index, and the `pull` and `pushed` control messages. Placement writes the file and offers the ref (§8.4), and the inline `tasks.Spec` path stays for Grains without a `work_repo` |
| **S4** | grain | Grains read placed tasks from git: verify the attestation, epoch and seq (§11.3), write `status`, the thread and the snapshot, and handle tombstones as cancels. The git copy becomes authoritative for declarations |
| **S5** | silo + grain | Planning as a Silo operation (§7.4) with its limits and holds, derived requirement traces in the hub UI, and the cross-Grain dependency claims (§8.1) |
| **S6** | silo + grain | Signed commits (§11.2), INV-24 to INV-26 in threat-model.md with tests, archival and forwarding (§10), and retirement of the inline offer |

## 15. Open questions

- Should a hub-placed task's `spec` be editable by its submitter after
  placement, instead of being superseded? This design says no, because a
  new task is cheaper than a rewrite whose attestation every reader must
  re-verify.
- Where does a team's shared schedule run when the team has several
  Grains: always on one named Grain, or placed fresh at each firing?
  `runs-on: {team: …}` is the second option.
- Should intake's tombstones for placed tasks expire, to keep intake
  small? They could be dropped after the task closes, with Silo's index
  as the lookup.
