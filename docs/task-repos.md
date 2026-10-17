# Task repos: Grain's backlog and the Silo/Grain channel as plain git

**Status: design for review (bsalmon@google.com). Nothing here is built,
and no implementation task is filed until it is approved.** The examples
are in [`examples/task-repos/`](../examples/task-repos/README.md).

*Amended 2026-10-16 (task 4448), as the owner settled:*

- *an object's file path is its identity, and there is no `id:` field (§2.1);*
- *a backlog is a folder of ranked references to tasks, and tasks stay
  where they were authored (§5.2);*
- *a Grain records its work as execution records in its own repo (§5.3).*

*Amended again 2026-10-16 (task 4450), as the owner settled:*

- *any repo, a code repo included, may hold task-repo objects in a grain
  folder (`.grain/` by convention) declared by its own
  `.grain/README.md`, and a reader finds a file's README by
  nearest-README discovery (§3.1). Grain's existing `.grain/`
  declarations move to the same Markdown format, workflows excepted, and
  design docs in `docs/design/` become objects in place (§12);*
- *objects in a code repo are trusted as that repo's own merge process
  is, not through Silo's hook (§8.13, §11.12);*
- *where an approved task lives, its **task home**, is configured per
  team and per repo: the intake (the default), the code repo, or the
  intake until someone promotes it (§5.5, §8.14).*

*Amended a third time 2026-10-16 (task 4452), as the owner settled. Where
this conflicts with the amendments above, this one wins:*

- *each Grain has a **root folder**, a whole repo or a subtree, marked by
  its README (`role: grain-root`). Names stay global and unique (repo plus
  path), and a Grain's **namespace** is built only by `kind: include`
  objects, which add names unchanged; references never add names (§3.2,
  §3.3);*
- *includes are standalone objects anywhere in a folder's tree, track a
  branch or pin a ref, narrow by `kinds:`, and are always transitive.
  `kind: exclude` objects subtract, scoped to their own folder. Resolution
  is bottom-up and order-independent, and the root's excludes are applied
  last (§3.3);*
- *linking to Silo is an include of Silo's folder for that Grain, in a
  Silo repo, holding its placements and its assigned team triggers. Silo
  no longer writes into Grain repos: **each repo or folder has one
  writer**, and only the team intake keeps field-level rules (§3, §8.1,
  §8.12);*
- *each backlog source is a **lane** with its own ranks, and the root
  orders the lanes (§5.2);*
- *the design borrows ideas, not the envelope, from Backstage's catalog,
  Android `repo` manifests, Kustomize and Plan 9 (§3.4).*

*Amended a fourth time 2026-10-16 (task 4453), as the owner settled. Where
this conflicts with the amendments above, this one wins:*

- *a task declares everything its sandbox holds in one list,
  **`context:`**: repos, repo folders, objects and other files, each a
  reference with a mode (`read`, or `write` for a repo or folder, which
  pushes only the task's own branch), an optional landing `path` and an
  optional `depth`. It replaces grain's target repo, `Reads`, `Writes`
  and ad-hoc pasted context, and 4446's `repo:` and `base:`. Only
  `context:` materialises; every other reference is metadata.
  Transitivity is off unless an entry sets `depth`, under caps. The
  Grain's policy checks every resolved entry before it fetches anything,
  and refuses closed, naming the entry. Includes and `context:` share one
  resolver: one builds a Grain's namespace, the other a sandbox's view
  (§5.6, §8.16, §11.19–§11.26);*
- *every file that is not a `kind:` object is an **asset**, referenceable
  by path like any object but never in a namespace, with optional
  metadata in a `kind: asset` sidecar, size limits, Git LFS allowed and
  never required, external artefacts referenced by URL and digest, and
  grain's task and comment attachments migrated to assets beside their
  objects (§2.2, §13).*

*Amended a fifth time 2026-10-16 (task 4456), as the owner settled. Where
this conflicts with the amendments above, this one wins:*

- *capabilities become objects. A **base capability** is implemented by
  the Grain in trusted provider code, and declares the most it can grant,
  what it materialises and what it implies. A **capability object**
  (`kind: capability`) is a reference to a base plus an ordered chain of
  **reducers** that only narrow it: effective authority is base ∩
  reducer₁ ∩ reducer₂ ∩ …, on the model of macaroons. Objects can narrow
  other objects across repos (§5.7);*
- *reducers are written in **Anselm**, a language this design names and
  deliberately does not specify. It will come from another project and
  be adopted when ready. Until then, a small set of built-in reducer
  types serves as an interim, and a `language: anselm` reducer is
  carried, but refuses the request on a Grain that cannot evaluate it
  (§5.7);*
- *including a capability object makes it available to request, and
  never grants it. A task requests one in `context:` with `mode: use`.
  The Grain's local policy allowlists base types and definition sources,
  and checks the effective capability and everything it implies (repos,
  egress, tools) before it materialises anything. Approval, secrets by
  name and CI's `capabilities:` all map onto the same model (§5.7,
  §8.17, §11.27–§11.35, §13.2).*

*Amended a sixth time 2026-10-17 (task 4479), as the owner settled. Where
this conflicts with the amendments above, this one wins:*

- *there is no `kind: schedule`. A **`kind: trigger`** is "when X
  happens, file Y": `on:` names one or more event sources, each with its
  own options nested under it in the GitHub Actions shape (`schedule:`,
  `push:`, `merge:`, `design-change:` and `manual:`; `task-closed:` is
  reserved for later), and `fires:` is a template plus inputs, or a
  workflow run followed by a review task. The design-doc planner is a
  `design-change` trigger. Last and next fire, pause and run history stay
  in the databases. Includes and excludes work on triggers unchanged
  (§6.2, §7.4, §13.1);*
- *merge gates, `gate: true` in grain's `.grain/triggers` today, are
  their own **`kind: gate`**: `match`, `requires` and `sensitive`. A
  gate never fires work except the reviews it requires, and a trigger
  never holds a merge. Gates apply through Silo's hook and merge queue on
  Silo-hosted repos and through grain's merge queue in code repos;
  included folders cannot weaken them, and gates from a Grain's mandatory
  sources cannot be excluded (§6.3, §8.14, §11.36–§11.40).*

*Amended a seventh time 2026-10-17 (task 4487), as the owner settled.
Where this conflicts with the amendments above, this one wins. It
**replaces** 4452's include and exclude objects, `kind: repo`, Grain root
README markers, spaces and namespaces, and there are no ignore files:*

- *a Grain's root configuration is **`grain.config.md`** at its root
  path, versioned and owned like any object. It declares **typed active
  roots**: `backlog:` lanes, `triggers:` and `memory:`, each a list of
  entries naming one folder, live or pinned. **Being in an active root
  means enabled**; everything else (tasks, templates, designs, capability
  definitions, `context:` and `depends_on` targets) is reached **only by
  reference**, read-only and never active by itself (§3.1, §3.3);*
- *each entry is **self-describing**: it activates the matching objects in
  its folder's subtree, minus its own optional `ignore:` patterns
  (gitignore syntax), minus any `archive/` folder. Nothing is inherited
  from parent folders or other files (§3.3, §3.8);*
- *linking a Grain to Silo is the Grain's owner adding Silo's per-Grain
  folder as a backlog lane (and a trigger root). Nothing Silo writes
  activates anything unless the owner pointed a root at it (§8.12,
  §11.14);*
- *gates are not Grain roots: the merge queue reads `*.gate.md` from the
  merged-into repo's own `.grain/` (§6.3). Capability definitions are
  reached only from `context:` (§5.7);*
- *every object is named **`<name>.<kind>.md`**, and its `kind:` must match
  (§3.2). Repo-wide settings are in **`.grainconfig`** (§3.4), and write
  ownership in **`GRAINOWNERS`**, CODEOWNERS syntax with a field
  extension (§3.5);*
- *discovery reads **git trees only**, parses front matter lazily, and is
  cached by tree and blob hash, so steady-state cost is O(changes);
  fetches are sparse; closed objects move under `archive/` by a fixed
  mapping, with no tombstone; limits fail loudly; and an orphan check
  flags objects nothing reaches (§3.7–§3.10).*

This design stores grain's **tasks, triggers, gates, templates and design docs**
as self-contained Markdown files in a **graph of plain-git repositories**
("task repos"). Each object is one file, and the repos link to each other
by git URL. Task repos become three things:

- every Grain's backlog;
- Silo's backing store for intake;
- the channel through which Silo and Grains exchange work.

It changes what travels over [the task interface](hub-interface.md)
(§9). It leaves alone who decides (Silo, the PDP) and who enforces (each
Grain, the PEP) ([hub.md](hub.md) §1.2).

## 1. Model

**A Grain consumes task repos.** A **standalone Grain**'s backlog is its
own task repo, which can be a purely local git repo
(`file:///var/lib/grain/tasks.git`). That one repo holds the tasks its
owner writes, the backlog that ranks them, and the records of their runs.

Every Grain has a **root folder** (§3.3): a whole repo, or a subtree
such as `.grain/`. Its **`grain.config.md`** says what the Grain acts on,
as **typed active roots**: the backlog lanes whose references it runs,
the trigger folders whose triggers it fires, and the memory folders it
gives its runs (§3.3). The root also holds the Grain's own objects as
ordinary files: its own lane, its execution records and their threads,
its own triggers, tasks and memory. **Only the Grain and its owner write
it.** Everything else a Grain uses, from a task's template to a
capability definition, it reaches **by reference**, read-only, when
something needs it (§3.1).

**Linking a Grain to Silo is an edit to `grain.config.md`.** Silo keeps a
folder for each Grain in a Silo repo (the team's `placements` repo),
holding that Grain's **placements** (backlog references to tasks) and the
team triggers assigned to it. The Grain's owner adds that folder as a
backlog lane, and its `triggers/` as a trigger root, tracking Silo's
branch (§8.12). **Nothing Silo writes activates anything unless the
owner pointed a root at it.** Silo writes only its own repos; the Grain
reads them. Silo also grants the Grain read access to the repos those
references point into: the team's intake, where the tasks themselves
live, and the shared repos the tasks reference.

**Unlinking** removes those entries. Nothing moves: the root, with all of
its history, was the Grain's all along.

A linked Grain's root is usually a repo Silo hosts, because Grains sit
behind NAT and firewalls and Silo never dials them (hub.md §4), and Silo
needs to read the execution records. **Hosting is not writing:** Silo's
hook lets only the Grain and its owner push there. The Grain **pulls from
and pushes to** its root, and **only pulls** from every other repo,
Silo's placements included. The existing rendezvous WebSocket stays, and
carries:

- `pull` notifications ("repo `<url>` moved to `<sha>`");
- live events;
- the view-through and node UI streams (hub.md §7, §11).

**The Grain stays the PEP.** Every placement Silo writes for a Grain is
a **claim**. The Grain validates the claim and holds the task
it points at to its local policy, and it may refuse it as it refuses an
offer today.

### 1.1 A graph of repos

Repo **roles** are conventions, recorded only in prose. The format does
not depend on them:

| Role | How many | Holds | Typical writers |
| --- | --- | --- | --- |
| **grain-root** | one per Grain instance: a repo, or a subtree of one | that Grain's root (§3.3): `grain.config.md`, its own lane of backlog references, its execution records and their threads, its own tasks, triggers and memory | **that Grain and its owner only**; `grain.config.md` the owner's alone |
| **placements** | one per team, in Silo | a folder per Grain serving the team: its placements (`backlog/`) and the team triggers assigned to it (`triggers/`, copies or the team's own files), each active only once the Grain's config names it | **Silo only**, on its own behalf and for the people who reorder through its UI |
| **intake** | one per team, in Silo | the team's tasks: proposals, approvals and cancels; the team's ranking of unplaced work; discussion on tasks; the team's own triggers | any authenticated user (new proposals only), the team's approvers, the team's members (ranks), Silo. The one role with several writers (§3.5, §8.1) |
| **shared** | any number, in Silo or elsewhere | templates, triggers, design docs, template partials, memory and capability definitions, for Grains to name in their roots or reference | the team's maintainers, through review branches |
| **code** | the code repos themselves | a grain folder (`.grain/`): templates, triggers, gates, suites and workflows, tasks homed there (§5.5), and design docs in place in `docs/design/` (§3.6, §12) | whoever the repo's own review and merge process lets land a change (§8.13) |

"Silo" is **one or more repos**, not a special store. Everything works by
references alone:

- a backlog reference in a Grain's placements, or in its root, points at
  a task in an intake, a Grain root, or a code repo's grain folder (§5.5);
- a Grain's `grain.config.md` names the folders whose objects it acts on (§3.3);
- an execution record points at its task and at the backlog reference it
  came from;
- a task points at its template in a shared repo;
- a design requirement's tasks, and a task's runs, are found by following
  the links back (§7.3, §5.3).

### 1.2 What goes in git, and what does not

Git holds what people review and refer to:

- definitions, intent and decisions;
- designs;
- each backlog's order;
- the human-readable thread;
- for each run of a task, a final-state snapshot.

**High-churn runtime state stays in the Grain's or Silo's database:**
attempts, live status, heartbeats, events, transcripts and logs. Two
rules keep the split clean:

- **State lives in front matter, never in file location.** A file is
  written once, at a stable path that is its identity (§2.1), and never
  moves to change state.
- **Each repo or folder has one writer** (§3.5, §8.1), except a team's
  intake, where each field has one. The databases may mirror a field for
  queries, but they never compete with git for it.

| Git-backed | Runtime only (database) |
| --- | --- |
| task: `title`, body (the prompt), `team`, `context` (§5.6), its `attachments/` | the attempt list, attempt numbers, sandboxes, leases |
| task: `submitter`, `approved_by`, `approved_at`, `capability_approvals`, `origin`, `filed_by`, `state` (proposed, approved, rejected, cancelled), `cancel` | the live `running` sub-states (sandboxing, agent turn, waiting for CI) |
| task: `template` (+ `inputs`, `rev`), `trigger` (the firing that filed it), `depends_on`, `implements`, `constraints`; capability objects and Grains' base declarations (§5.7) | events, `seq`, transcripts, tool calls, diffs, logs |
| backlog reference: `to`, `spec_rev`, `rank`, `priority`, `expedite`, `attestation`, who added it and when | heartbeats, slot counts, offer timers |
| execution record: `task`, `from`, `state` at the coarse grain of §5.3, timestamps, `refusal`, `superseded`, `previous`, the `capabilities` it used (§5.7) | the dispatcher's claim, `refused_by` lists, offer counts; secret material, minted credentials and the per-use decision log (audit events) |
| execution record: `closed`: outcome, change link, merge commit, summary, requirement revisions | merge-queue position, CI run ids, cost and token counts (summarised into `closed` if wanted) |
| trigger and gate definitions: `on`, `fires`, `enabled`, `limits`; `match`, `requires`, `sensitive` (§6.2, §6.3) | a trigger's last and next fire, debounce windows, catch-up cursor, paused or suspended state and run history; a gate's hold on each change and its reviews' progress |

A coarse state change, such as a run going `accepted → running → done`,
is a commit. A fine-grained step, such as "agent turn 14", is an event.

## 2. Objects: one self-contained Markdown file each

Every object is one UTF-8 file. The file has **YAML front matter**
between `---` lines, then a **Markdown body**. Every file declares:

- `kind:`, one of `config`, `task`, `ref`, `run`, `comment`, `template`,
  `trigger`, `gate`, `suite`, `design`, `memory`, `moved`, `asset` (a
  sidecar, §2.2), `capability` or `capability-base` (§5.7). It must match
  the kind suffix of the file's name, `<name>.<kind>.md` (§3.2);
- `schema:`, as in `grain/task@v1` (§10).

There is **no `id:` field.** An object is named by where it is (§2.1).
Every other file in a repo, an image or a CSV or a Markdown file with no
kind suffix, is an **asset**: referenceable the same way, but not an
object (§2.2).

The body carries what a human reads: a task's prompt, a template's text,
a design's prose. A file means something on its own. You can read it,
mail it or `cat` it without knowing which repo it came from.

**Layout is loosely specified.** Nothing scans for objects: a Grain
lists the folders its `grain.config.md` names, by kind suffix, and
resolves references to everything else (§3.7). The recommended folders
below are conventions only. The one place where a folder matters is a
backlog: the root entry that lists a reference is its **lane** (§5.2).

A Grain's root (§3.3):

```
grain.config.md                            kind: config  (its typed active roots, §3.3)
GRAINOWNERS                                write ownership (§3.5)
.grainconfig                               repo settings: aliases, limits (§3.4), at the repo root
backlog/<name>.ref.md                      kind: ref     (the Grain's own lane)
backlog/fired/<name>.ref.md                kind: ref     (its triggers' firings, §5.2)
backlog/gated/<name>.ref.md                kind: ref     (its merge queue's gate reviews, a lane of their own, §6.3)
triggers/<name>.trigger.md                 kind: trigger (its own; a trigger root)
memory/<name>.memory.md                    kind: memory  (its own; the write: true memory root)
capabilities/bases/<type>.capability-base.md  kind: capability-base (written by the Grain from its providers)
tasks/<name>.task.md                       kind: task    (its owner's own work, referenced from backlog/)
tasks/fired/<trigger>/<key>.task.md        kind: task    (a trigger's firing, §6.2)
tasks/gated/<repo>/<change>-….task.md      kind: task    (a gate's review, §6.3)
runs/<name>/<utc>.run.md                   kind: run     (one execution record of task <name>)
runs/<name>/comments/<utc>-<author>.comment.md  kind: comment (the run's thread)
runs/<name>/comments/<utc>-<author>/<file> an asset    (a reply's attachment, §2.2)
runs/archive/<name>/…                      archived runs (§3.8)
```

Silo's placements repo, one folder per Grain:

```
.grainconfig                               owner, aliases
GRAINOWNERS                                * @silo
grains/<grain>/backlog/<name>.ref.md       kind: ref     (one placement; a lane once the Grain names it)
grains/<grain>/triggers/<name>.trigger.md  kind: trigger (a team trigger assigned to that Grain)
```

An intake or a shared repo:

```
.grainconfig                               owner, aliases, task_home (§3.4)
GRAINOWNERS                                who writes what, field-level in an intake (§3.5)
README.md                                  for humans: an asset
tasks/<name>.task.md                       kind: task
tasks/<name>/attachments/<file>            an asset      (attached to the task, §2.2)
tasks/<name>/comments/<utc>-<author>.comment.md  kind: comment (discussion of the task)
tasks/archive/…                            closed tasks (§3.8)
backlog/<name>.ref.md                      kind: ref     (one ranked reference to a task)
templates/<name>.template.md               kind: template
templates/partials/<name>.template.md      kind: template (include-only, inputs declared)
triggers/<name>.trigger.md                 kind: trigger (a trigger root for Grains that name it)
triggers/archive/<name>.trigger.md         a retired trigger, active nowhere (§3.8)
memory/<name>.memory.md                    kind: memory  (a read-only memory root for Grains that name it)
designs/<name>.design.md                   kind: design
capabilities/<name>.capability.md          kind: capability (a team's or org's definitions, §5.7)
designs/img/<file>                         an asset      (an image the design embeds)
designs/img/<file>.asset.md                kind: asset   (its optional sidecar, §2.2)
<an old path, as name.moved.md>            kind: moved   (a tombstone, §2.1)
```

A code repo keeps the same kinds in its grain folder, beside the code
(§3.6):

```
README.md                                  the code's own README: an asset
.grainconfig                               optional (§3.4)
.grain/templates/<name>.template.md        kind: template
.grain/triggers/<name>.trigger.md          kind: trigger (standing work, active for Grains that name the folder, §6.2)
.grain/gates/<name>.gate.md                kind: gate    (merge gates on this repo, read by its merge queue, §6.3)
.grain/suites/<name>.suite.md              kind: suite
.grain/workflows/<name>.yml                a GitHub Actions workflow: YAML, an asset (§12.2)
.grain/tasks/<name>.task.md                kind: task    (homed here, §5.5)
docs/design/<name>.design.md               kind: design  (in place, §12.3)
```

**A tombstone's file name** is the old file name plus `.moved.md`:
moving `templates/prune-packages.template.md` leaves
`templates/prune-packages.template.moved.md`. A reference still names the
old path; a resolver that finds nothing there looks for the tombstone
beside it, then for the archive mapping (§3.8). Each is one
deterministic lookup, and no index is needed.

**Names.** Whoever creates an object picks its path, once:

- a new task or run: `<yyyy-mm-dd>-<slug>`, with `-2`, `-3`… if that is
  taken. Two writers racing for one name collide as an ordinary git
  conflict, and the second one picks the next suffix;
- a migrated object: `<legacy number>-<slug>` (§13);
- a generated object: a name derived from what generated it, so a retry
  lands on the same path. A trigger's firings use
  `tasks/fired/<trigger>/<event key>` (§6.2), planning among them
  (§7.4);
- a backlog reference: its target's name
  (`backlog/2026-10-16-approval-flow.ref.md`);
- an execution record: the UTC time it was accepted
  (`runs/4310-carry-approver/20261016T135800Z.run.md`);
- a comment: `<utc>-<author>`, such as `20261016T144710Z-grain-eu.comment.md`.
  Each author writes its own comments one at a time, so two authors
  appending to one thread never conflict.

The slug is taken from the title when the object is created. A later
title change leaves the path alone.

### 2.1 Identity is the path

- **Within a repo**, an object is its repo-relative path:
  `tasks/4310-carry-approver.task.md`.
- **Globally**, it is the git URL plus that path, which is exactly the
  cross-repo reference form of §4:
  `https://silo.example.com/git/hub/intake.git//tasks/4310-carry-approver.task.md`.

An alias (`intake:tasks/4310-carry-approver.task.md`) or a relative path
(`../4310-carry-approver.task.md`) names the same identity once it is
resolved. The **canonical form** is the URL form with the alias
expanded, `.` and `..` removed, the path's leading `/` dropped, and no
`@gitref` or `#anchor`. Two references name the same object exactly when
their canonical forms are equal after following redirects (§4).

**Paths are stable by design.** State lives in front matter (§1.2), so
an object never moves to change state. A task is proposed, approved,
placed, run and closed without its path changing, unless its task home
moves it, once, into a code repo (§5.5). Placement adds a
reference to it in Silo's folder for a Grain (§5.2), and running it adds
an execution record in that Grain's root (§5.3).

**A real move leaves a tombstone.** A rename, a reorganisation, or a
move to another repo leaves a `kind: moved` object beside the old path,
named for it (`<old file name>.moved.md`, §2). Archiving is not a real
move, and leaves none (§3.8):

```yaml
---
kind: moved
schema: grain/moved@v1
to: ghcr/prune-packages.md         # a reference, relative to this file or across repos
moved_by: "user:bsalmon@google.com"
moved_at: "2026-10-16T17:40:00Z"
reason: Group the GHCR templates under templates/ghcr/.
---
```

The object at its new path carries `moved_from:`, the list of its
earlier addresses, oldest first. Readers follow a tombstone the way a
browser follows an HTTP redirect, with a bounded chain length and cycle
detection (§4). A move within one repo is a single commit: the tombstone
and the new file together. A move between repos writes the new home
first and the tombstone second (§8.11).

**No path reuse.** A tombstoned path is permanently that object's old
address. Silo's hook refuses any change to a tombstone and its deletion,
so no new object can ever be created there (§8.1). **Only backlog
references are ever deleted**: they are pointers, not addresses anyone
keeps (§5.2). Every other
object leaves its path only by becoming a tombstone. Assets are not
objects, and are deleted or moved like any file (§2.2), except a task's
`attachments/`, which are frozen with its spec (§5.1).

**Repo moves.** References name repos by URL, usually through an alias
declared in the referring repo's `.grainconfig` (§3.4). Moving a repo
therefore means:

- changing the alias in the `.grainconfig` files that declare it, one
  line each, and any root entry that names the URL directly;
- optionally setting `moved_to: <new git URL>` in the old repo's
  `.grainconfig`, as a **repo-level redirect**. The old repo stays
  readable. A reader that resolves into a repo whose `.grainconfig` has
  `moved_to` retries at the new URL with the same path, and that counts
  as one hop.

**Earlier names are kept for search, not resolution.** `aka:` lists an
object's identities from before migration, such as `grain:eu/task/4310`,
`silo:hub/task/812` or `repo:template:bwsalmon:grain:prune-packages`.
Silo indexes them so the old names can be looked up. References never use
them.

### 2.2 Assets: every other file

**Any file in a repo can be referenced**: an image, a PDF, JSON or CSV
data, a diagram, a binary, or Markdown without `kind:` front matter. Only
Markdown files with `kind:` are **objects**. Every other file is an
**asset**. An asset's identity follows the same rule as an object's: its
repo plus its path (§2.1), named by a relative path, an alias or a git
URL, plus an optional `@ref` (§4). `shared:designs/img/claims-flow.png`
is an asset. So is a design doc that has no front matter yet (§12.3).

What an asset is for, and what it is not:

- **Reachable by reference.** A `context:` entry (§5.6), a link or image
  in a Markdown body, or a sidecar's `asset:` (below) may name one. A
  field that expects a kind (`template:`, `depends_on:`, `implements:`,
  a backlog reference's `to:`) never resolves to an asset: an asset
  there is reported as the wrong kind, as a template named in
  `depends_on` would be.
- **Materialised only when a task lists it** in `context:`, or when an
  entry's `depth:` reaches it from a Markdown file that links to it, such
  as an image embedded in a design doc by a relative link (§5.6).
- **Never active.** Assets are not work. A root entry lists only its
  type's kind suffix (§3.3), so an asset under a root's folder is never
  listed. Nothing ranks, places, fires or runs an asset.
- **Never executed** by Silo or a Grain: an asset is data, whatever its
  name or mode bits say (§11.24).

**Tombstones work for objects only.** A `kind: moved` tombstone is a
Markdown object, and cannot sit at an asset's path (`img/flow.png`). An
asset that moves leaves nothing behind. References to it dangle, unless
it has a sidecar, which moves with it and leaves a tombstone of its own.
So **give an asset that others reference from other repos a sidecar**,
and reference the sidecar. Within one repo, a move is one commit, and the
same commit fixes the links.

#### Sidecars: optional metadata for an asset

An asset has no front matter, so its metadata, where it needs any, is in
a **sidecar**: a `kind: asset` object that references it.

```yaml
---
kind: asset
schema: grain/asset@v1
asset: claims-flow.png              # the asset: a reference, usually relative
title: How a signed claim travels from Silo to a Grain
alt: >-
  Silo signs a placement's attestation; the Grain verifies it against
  silo_issuer before it accepts the run.
media_type: image/png
license: CC-BY-4.0                  # an SPDX identifier
provenance:
  author: "user:bsalmon@google.com"
  created_at: "2026-10-16T19:10:00Z"
  source: drawn for this design
sha256: "…"                         # optional for an asset in git; checked when present
bytes: 250
---
<an optional longer description, for humans and for agents>
```

This is [`hub-shared/designs/img/claims-flow.png.asset.md`](../examples/task-repos/hub-shared/designs/img/claims-flow.png.asset.md).
Schema: [`schemas/asset.schema.json`](../examples/task-repos/schemas/asset.schema.json).

- **Where it lives.** By convention, beside the asset, named for it with
  `.asset.md` appended: `img/claims-flow.png.asset.md` describes `img/claims-flow.png`.
  A reader finds that sidecar by opening one path, with no index. A
  sidecar may live elsewhere, describing an asset in a repo its author
  cannot write. Silo's reverse index (§4) maps each asset to every
  sidecar that names it. Where the two disagree, the one beside the asset
  wins for display.
- **What it carries:** `asset` (or `external`, below), `title`,
  `description` or a body, `alt` (required for an image someone will
  show), `media_type`, `license`, `provenance` (`author`, `created_at`,
  `source`, `generated_by` for a generated file, `derived_from` as a
  reference), and optionally `bytes` and `sha256`. Silo's hook checks that
  `asset` resolves to a file that is not an object, and checks `sha256`
  and `bytes` against the blob when they are present.
- **What it is not.** A sidecar is metadata about data. It is never
  ranked, placed or run, and no root type lists it.
- **Optional.** No asset needs one. An image in a design doc needs only
  the doc's own alt text in the link.

#### Size and storage

Assets live in git, and every clone carries every version of them, so
task repos keep them small:

| What | Recommended | Enforced |
| --- | --- | --- |
| an image or a diagram | ≤ 1 MiB (PNG, JPEG or WebP; SVG as data only, §11.25) | Silo's hook warns over 1 MiB |
| any one asset | ≤ 10 MiB | Silo's hook refuses a blob over 10 MiB, which is also its push cap (§14) |
| a repo's assets, all versions | ≤ 1 GiB | Silo's health view warns |
| anything larger | an external artefact (below) | the materialiser's caps (§5.6) |

- **Git LFS is allowed where the host supports it**, because it is still
  git: a pointer file in the tree, the content on the host's LFS
  endpoint. The materialiser fetches an LFS object through the Grain's
  git proxy when the host serves LFS and the Grain's policy allows that
  host, under the same size caps as any file. A pointer that cannot be
  resolved is skipped and recorded (§5.6), and never handed over as if
  it were the content. **Nothing in this design requires LFS**: no
  object may be an LFS pointer (the hook refuses a `*.md` with `kind:`
  that is one), and Silo's own git host does not serve LFS in the stages
  of §16.
- **Very large artefacts are referenced where they live**, not copied in:
  a build output, a dataset, a model checkpoint, a video. The reference
  is a sidecar with `external:` in place of `asset:`:

  ```yaml
  ---
  kind: asset
  schema: grain/asset@v1
  title: Widget corpus, October snapshot
  external:
    url: https://storage.googleapis.com/widgets-corpus/2026-10/corpus.tar.zst
    sha256: "9f1c…"                 # required: it pins the content, as a commit pins a tree
    bytes: 2147483648
    media_type: application/zstd
  license: proprietary
  ---
  ```

  Everything else names the sidecar, a git path, so identity, moves and
  references work as for any object. The `sha256` makes the reference
  immutable: what the URL serves must match it, or nothing is
  materialised. `url` is `https://`. A Grain may also accept `gs://` or
  `s3://` where a capability gives it a credential for that store. Only
  the Grain fetches it, never the sandbox, and only when the Grain's
  network policy allows the host (§5.6, §11.21).

#### Attachments become assets

Grain today stores a file attached to a task or to a comment as a blob
on that task or comment, with its file name and content type. With task
repos, **an attachment is an asset stored next to the object it belongs
to, and referenced from it**:

| Attached to | Stored at | Referenced from |
| --- | --- | --- |
| a task | `tasks/<name>/attachments/<file name>` | the task's `context:`, as a `read` entry (`attachments/<file name>`), because only `context:` materialises (§5.6); and from the body, where the prompt mentions it |
| a comment | `comments/<utc>-<author>/<file name>`, a folder named for the comment beside its file | the comment's `attachments:` list, references relative to the comment, and its body |

- **New attachments** are commits like everything else. Silo's UI and API
  add the file in the same commit as the task or comment it belongs to,
  so the intake's `authenticated` rule covers it: its `kinds` list
  `asset`, which in a `writers` rule means files that are not objects, and
  its `paths` limit those to the new task's `attachments/` (§3). A Grain
  commits the attachments of a reply in its root with the reply (§5.4).
- **They are part of the task's spec.** A relative `context:` entry reads
  the task's own commit (§5.6), and the hook freezes a task's
  `attachments/` with the rest of its spec on approval (§5.1). So what the
  approver saw is what the agent gets.
- **The migration**, in `grain tasks export` (§13): each attachment is
  written at its path above, with its file name made safe (Unicode NFC,
  no `/`, `\`, `..` or control characters, at most 255 bytes, and `-2`,
  `-3`… on a collision). Each task attachment gains a `context:` entry. A
  content type the extension does not imply gets a sidecar with
  `media_type`. An attachment over 10 MiB goes to the deployment's
  artefact bucket and is exported as an external sidecar with its
  `sha256`.

## 3. Roots and repo configuration

*This section was rewritten by the seventh amendment (task 4487). It
replaces 4452's include and exclude objects, Grain root README markers,
`kind: repo`, spaces and namespaces.*

### 3.1 Active roots and references

There are two ways a Grain can reach an object, and they are deliberately
different:

- **Being in an active root means it is enabled.** A Grain's root
  configuration, `grain.config.md` (§3.3), names folders of three types.
  The Grain *acts* on what they hold: it runs the `*.ref.md` backlog
  references in its **backlog** roots, fires the `*.trigger.md` triggers
  in its **trigger** roots, and gives the `*.memory.md` documents in its
  **memory** roots to its runs.
- **Everything else is reached only by reference.** It is used when
  something needs it, read-only, and is never active by itself.
  Templates, design docs, the tasks a backlog reference points at,
  capability definitions, `context:` targets and `depends_on` targets
  are all reached this way. A reference resolves for the object that
  holds it, with the reader's own rights (§4, §11.4). It never makes
  anything active, and never materialises anything except through a
  task's `context:` (§5.6).

**The test** for which of the two something needs: *if this target
changed or disappeared, should the Grain start or stop **doing**
something?* If yes, it must be in a root. If no, it is a reference.

- A new trigger in the team's `triggers/` should start firing on every
  Grain that points a trigger root at it, so trigger folders are roots.
- A template that a trigger fires should not, by being edited, start
  anything; the trigger's own `fires.template` names it, so it is a
  reference.
- A new reference in Silo's folder for Grain `eu` should make `eu` run
  something, so that folder is a backlog root of `eu`. The task it points
  at is a reference: it exists whether or not anyone runs it.
- A capability definition changes what a run *may* be given, never
  whether something runs. It is a reference from a task's `context:`
  (§5.7), allowed or refused by the Grain's local policy.
- A gate holds merges into the repo it belongs to. It is not a Grain's
  root at all: the merge queue reads it from the merged-into repo's own
  `.grain/` (§6.3).

There are **exactly three root types**: `backlog`, `triggers` and
`memory`. There is no generic include, no transitivity and no exclude
object: a root entry is one folder, and the objects in it are active for
the type the entry declares, nothing else.

### 3.2 Object files are named `<name>.<kind>.md`

Every object's **kind is in its file name**, so a reader can list a
folder's objects, and know each one's kind, from a git tree alone (§3.7):

| Suffix | Kind | Where it is active, or how it is reached |
| --- | --- | --- |
| `grain.config.md` | `config` | a Grain's root configuration (§3.3); the file name is fixed |
| `*.ref.md` | `ref` | active in a **backlog** root (§5.2); also an intake's ranking (§5.2) |
| `*.trigger.md` | `trigger` | active in a **trigger** root (§6.2) |
| `*.memory.md` | `memory` | active in a **memory** root (§3.3) |
| `*.gate.md` | `gate` | read from the merged-into repo's `.grain/` by its merge queue (§6.3) |
| `*.task.md` | `task` | by reference: a backlog reference's `to:`, `depends_on`, a comment's `on:` |
| `*.run.md` | `run` | written in the Grain's own `runs/`; by reference from its thread and from Silo's views (§5.3) |
| `*.comment.md` | `comment` | beside the task or run it discusses (§5.4) |
| `*.template.md` | `template` | by reference: a trigger's `fires.template`, a task's `template.ref`, a partial's include |
| `*.suite.md` | `suite` | by reference from the workflows and tasks that run it |
| `*.design.md` | `design` | by reference: `implements:`, a `design-change` trigger's `paths`, `context:` |
| `*.capability.md` | `capability` | by reference from a task's `context:` (`mode: use`) and from other capability objects (§5.7) |
| `*.capability-base.md` | `capability-base` | published by the Grain from its providers; by reference (§5.7) |
| `*.asset.md` | `asset` | a sidecar describing another file (§2.2) |
| `*.moved.md` | `moved` | a tombstone, read when a reference lands on the old path (§2.1) |

- **The front matter's `kind:` must match the suffix.** Silo's hook
  refuses a push that adds or changes a file whose `kind:` disagrees with
  its name, and `grain lint` reports it as an error on any host (§8.15).
  A file with a known suffix and no front matter is refused the same way.
- **Every other file is an asset** (§2.2): a `README.md`, an image, a
  `.yml` workflow, and a Markdown file whose name has no known kind
  suffix, whatever its front matter says. Front matter alone never makes
  an object.
- **A suffix nobody knows** (`x.widget.md`) is an asset, and `grain lint`
  warns, so a newer schema's kind is visible to older readers without
  being acted on.
- **Names before the suffix** follow §2's rules: a task `2026-10-16-approval-flow.task.md`,
  its reference in a lane `2026-10-16-approval-flow.ref.md`, a run
  `<utc>.run.md`, a comment `<utc>-<author>.comment.md`.

**Code-repo design docs become `*.design.md`.** A design doc in a code
repo is an object as soon as it is renamed (`docs/design/hub.md` →
`docs/design/hub.design.md`, leaving `hub.moved.md`), and an asset until
then. bwsalmon/grain's design-doc planner keys on `docs/design/` today:
as a `design-change` trigger its `paths` stay `docs/design/**`, which
matches both forms, so the planner keeps working through the migration
and nothing has to be renamed at once (§7.4, §12.3). We chose the rename
over a per-folder rule ("every `.md` in `docs/design/` is a design")
because that rule would make kind depend on location, which §3.7 needs
not to.

### 3.3 A Grain's root configuration: `grain.config.md`

A Grain's **root** is a folder in a repo: a whole repo
(`grains/eu.git`), a subtree of one (`.grain/` in its owner's own repo),
or a local `file:///var/lib/grain/tasks.git`. **`grain.config.md` at that
path** is the Grain's root configuration. It is an object like any other,
so it is versioned, reviewed and owned through `GRAINOWNERS` (§3.5), and
the hook validates it against
[`schemas/config.schema.json`](../examples/task-repos/schemas/config.schema.json):

```yaml
---
kind: config
schema: grain/config@v1
grain: {id: eu, silo: https://silo.example.com}
owner: {user: bsalmon@google.com}

backlog:                              # each entry is a lane
  mode: weighted                      # strict (the default) | weighted | round_robin
  expedite: lane                      # Silo's expedite reorders within its lane (default), or global
  lanes:                              # listed highest priority first
    - name: silo
      path: "placements:grains/eu/backlog/@main"
      ref_mode: track                 # follow the branch's tip
      weight: 3
    - name: own
      path: backlog/                  # relative: this repo, the same commit as this file
      weight: 1
      ignore: ["drafts/"]             # gitignore syntax, relative to the entry's folder

triggers:
  - path: triggers/
  - path: "placements:grains/eu/triggers/@main"
    ref_mode: track
  - path: "shared:triggers/@main"
    ref_mode: track
    ignore: ["granule-pull.trigger.md"]

memory:                               # listed highest precedence first
  - path: memory/
    write: true                       # the Grain's memory agent writes here
  - path: "shared:memory/@main"
    ref_mode: track
  - path: "org-memory:memory/@v4"
    ref_mode: pin

limits: {objects_per_entry: 2000, objects: 20000}   # optional: can only lower the defaults
---
Grain `eu`: bsalmon@google.com's own Grain. Silo's lane first, three to one.
```

This is [`grain-eu/grain.config.md`](../examples/task-repos/grain-eu/grain.config.md).

**An entry** names one folder:

- `path`: a relative or `/`-rooted path in the Grain's own repo, read at
  the same commit as `grain.config.md`; or an alias or git URL plus
  `@ref` (§4), with **`ref_mode`**: `track` (the `@ref` is a branch, read
  at its tip on every fetch: Silo's folders, a team's triggers) or `pin`
  (a tag or commit: a reviewed release of an org's memory). A folder path
  ends in `/`. An entry may also name a single file, which activates that
  one object.
- `ignore`: optional patterns in **gitignore syntax**, relative to the
  entry's folder (`granule-pull.trigger.md`, `experimental/`,
  `!keep.trigger.md`). They only ever remove from what the entry
  activates.
- In `backlog.lanes` only: `name` (a stable label for status and for
  Silo's views), `weight` (in `weighted` mode).
- In `memory` only: `write: true` on at most one entry, in the Grain's own
  repo, which the Grain's memory agent writes (below).

**Each entry is self-describing.** It activates exactly:

> the files in its folder's subtree whose suffix is the root type's kind,
> **minus** the paths its own `ignore:` matches, **minus** anything under a
> folder named **`archive/`** (a built-in convention, §3.8).

Nothing is inherited from parent folders, from other entries or from
other files: there are no ignore files, cascading or otherwise. So what an
entry activates can be told from **the folder plus that one entry**, and
an edit anywhere else can never change it. Objects of other kinds in the
folder are not active for this entry; a `README.md` or an image there is
an asset, and nothing.

**Overlapping entries.** Two entries of one type may name nested folders
(`backlog/` and `backlog/gated/`, to give gate reviews a lane of their
own). A file both would activate belongs only to the **most specific**
entry, the longer path, so nothing is active twice and a reference's lane
is still determined by its folder (§5.2). The same file in two root
*types* is fine and meaningless: a `*.ref.md` file is only ever a
backlog object.

**Producers control their folders by their contents.** A team that does
not want a trigger to fire anywhere does not put it in `triggers/`, or
moves it to `triggers/archive/`. A Grain that does not want one of a
team's triggers adds it to its own entry's `ignore:` (§8.15). Neither
needs the other's cooperation, and neither can change what the other's
entry means.

**Backlog lanes.** Each lane is one entry. Ranks compare within a lane
only, and the lanes are ordered by their position in `lanes`, with
`mode` choosing strict priority, smooth weighted round-robin or
round-robin across them (§5.2). An entry is a lane whatever writes it:
the Grain's own `backlog/`, Silo's folder for this Grain, a colleague's
shared queue.

**Trigger roots.** Every `*.trigger.md` an entry activates fires, under
4479's semantics (§6.2): its `on:` sources are registered, and its
firings are filed where §6.2 says. The trigger's identity is still its
path, so the same trigger reached by two entries (a team folder and a
pinned copy of it) is one trigger, registered once.

**Memory roots.** Every `*.memory.md` an entry activates is made
available to the Grain's runs: its front matter says which runs it
applies to (`applies_to: {repos, teams, kinds}`, all runs by default) and
its body is the guidance. **Entries are listed in precedence order.**
When two documents give conflicting guidance, the one from the earlier
entry wins, and a run's memory is rendered in that order with each
document's source shown, so the agent and a reviewer can see which won.
Within one entry, a more specific `applies_to` wins over a general one,
then the path orders them.

```yaml
---
kind: memory
schema: grain/memory@v1
title: Deploy notes for the widgets repo
applies_to: {repos: ["https://git.example.com/hub/widgets.git"]}
updated_by: "grain:eu"
updated_at: "2026-10-16T21:00:00Z"
---
The preview deploy needs the `widgets-deployer` capability; ask for it
in `context:` rather than pasting a key.
```

**Grain's memory today maps onto this:**

| Today | In task repos |
| --- | --- |
| memory documents filed under the deployment | `memory/<name>.memory.md` in the Grain's own `write: true` entry |
| memory documents filed under a repository | the same entry, `memory/repos/<owner>/<name>/<doc>.memory.md`, with `applies_to.repos` set; or, when the repo's team wants to share them, its `memory/` in a shared repo that Grains add as a read-only entry |
| memory documents filed under a team | the team's shared `memory/`, a read-only entry of each of the team's Grains |
| `memory.json`, the memory agent's distilled notes about the deployment | one `memory/learned/<topic>.memory.md` per note, written by the memory agent, so each is reviewable and revertable on its own |
| the memory agent's writes | commits by `grain:<id>` to the `write: true` entry only; read-only entries are never written, whatever the agent proposes. A proposal for a shared folder is a review branch there (§8.11) |

**How a Grain learns its root path.** The only local setting a Grain has
about task repos is **where its `grain.config.md` is**: a git URL, a path
in it, and a branch. Everything else is in that file.

- `grain init` creates a standalone Grain: a local repo at
  `file:///var/lib/grain/tasks.git` (or a URL the owner gives with
  `--root <url>//<path>@<branch>`), and commits a starter
  `grain.config.md` there with one lane (`backlog/`), one trigger root
  (`triggers/`) and one memory root (`memory/`, `write: true`).
- `grain hub login` links the Grain to Silo. Silo creates the Grain's
  root repo it hosts (`grains/<id>.git`), seeds it with the local root's
  history if there is one, and returns its URL and Silo's per-Grain
  folder. The Grain sets its local root to the hosted repo, and **the
  command, acting as the owner, commits the link** (§8.12): it shows the
  diff to `grain.config.md` that adds Silo's lane and trigger entry, and
  commits it with the owner's credential only when they confirm. Silo
  never writes the file.
- `grain root set <url>//<path>@<branch>` moves an existing Grain to
  another root. The Grain refuses a root whose `grain.config.md` names a
  different `grain.id`.

A root without `grain.config.md` is an error the Grain reports at start,
and it acts on nothing. There is no default root set.

### 3.4 Repo-wide settings: `.grainconfig`

Settings that belong to a whole repo, not to one Grain, are in one file
at the repo root, **`.grainconfig`**: YAML, not Markdown, and not an
object (nothing activates it; every reader of the repo reads it). Schema:
[`schemas/grainconfig.schema.json`](../examples/task-repos/schemas/grainconfig.schema.json).

```yaml
# .grainconfig: hub team intake
schema: grain/repo-config@v1
owner: {team: hub, silo: https://silo.example.com}
host: silo                          # silo: Silo's hook enforces (§8); other: readers validate (§8.10)
default_ref: main                   # what an unpinned reference into this repo resolves to
ref_policy: {protected: [main], fast_forward_only: true, review_prefix: review/}
aliases:                            # shorthand used by references in this repo: alias → git URL
  shared: https://silo.example.com/git/hub/shared.git
  placements: https://silo.example.com/git/hub/placements.git
task_home: {mode: intake}           # where this team's approved tasks live (§5.5)
grain_folder: .grain/               # in a code repo: where its gates and other objects are (§3.6)
orphans: warn                       # warn (default) | refuse (§3.9)
limits: {objects_per_entry: 5000}   # optional: lower the defaults for entries into this repo
moved_to: null                      # set when the repo has moved: a repo-level redirect (§2.1)
```

- **Paths** in `.grainconfig` are repo-relative.
- **Aliases** are this repo's own: a reference in any file of the repo,
  `grain.config.md` included, expands them from here. A repo without the
  file has no aliases and the defaults (`default_ref: main`,
  `host: other`, `orphans: warn`).
- **Changing `.grainconfig`** is the repo owner's alone (`GRAINOWNERS`
  must say so for `/.grainconfig`; the hook refuses a rule that gives it
  to anyone else), and `owner` cannot be changed by a push at all.
- `README.md` files carry **no front matter** that anything reads. They
  are for humans, and they are assets.

### 3.5 Write ownership: `GRAINOWNERS`

Who may write what is in **`GRAINOWNERS`** files, in **CODEOWNERS
syntax**: one rule per line, a gitignore-style pattern and then its
owners. They **cascade**: every folder may have one, and for a changed
file **the nearest `GRAINOWNERS` with a matching line wins**; within one
file, as in CODEOWNERS, the last matching line wins. Ownership inherited
from a parent folder is the normal case.

**Ownership never affects what is active.** It says who may write a file,
never whether a Grain acts on it; only root entries do that (§3.3). So a
cascading ownership file is safe where a cascading ignore file would not
be (§15).

```
# GRAINOWNERS at the root of Grain eu's repo
*                    @grain:eu @user:bsalmon@google.com
/grain.config.md     @user:bsalmon@google.com
/memory/             @grain:eu @user:bsalmon@google.com
/runs/               @grain:eu
```

**Owners** are principals:

- `@silo`, Silo's own service identity;
- `@grain:<node-id>`, a Grain's node credential, bound to its owner;
- `@user:<email>`, or a bare email, as in CODEOWNERS;
- `@team:<name>#member`, `#approver` or `#admin` (from Silo's directory,
  hub.md §5.3);
- `@authenticated`, any signed-in human.

An owner may be narrowed to some **verbs** with a suffix:
`@authenticated(create)` may create matching files but not change them.
The verbs are `create`, `update`, `delete` and `move` (a move leaves a
tombstone, §2.1; `delete` is only ever allowed on `*.ref.md`).

**The field-level extension.** A pattern may end with `:` and a list of
front-matter fields. That line governs only those fields, and overrides
the whole-file owners for them; `body` names the Markdown body. Only a
folder with several writers needs it, and in this design that is a
team's intake:

```
# GRAINOWNERS at the root of hub/intake
*                                    @team:hub#admin
/tasks/                              @authenticated(create) @silo
/tasks/**/*.task.md:state,approved_by,approved_at   @team:hub#approver
/tasks/**/*.task.md:cancel           @team:hub#approver @silo
/backlog/                            @silo
/backlog/*.ref.md:rank,priority,ranked_by,ranked_at  @team:hub#member @silo
/.grainconfig                        @team:hub#admin
/GRAINOWNERS                         @team:hub#admin
```

What the old `writers` rules said with `bind`, `forbid`, `where` and
`transition` is now **fixed per kind in the schema**, checked by the hook
whatever `GRAINOWNERS` says, so it cannot be configured away:

- a new task is `state: proposed`, with `submitter` equal to the pusher;
- `approved_by` equals the pusher, and never the `submitter`; the only
  transitions are those of §5.1;
- `ranked_by` equals the pusher, or the principal Silo acted for;
- a run's terminal states are final, and a withdrawn placement fences it
  (§8.8);
- a tombstone never changes and is never deleted (§2.1).

**Who may change `GRAINOWNERS`.** Only the owners its own nearest rule for
`GRAINOWNERS` names, and a file may not give a subfolder's `GRAINOWNERS`
to anyone its own rule for that path does not already own. A Grain's
root keeps `grain.config.md` its owner's: `GRAINOWNERS` cannot give it to
`@silo`, and the hook refuses a rule that tries (§11.14).

**Enforcement.** On Silo-hosted repos the pre-receive hook enforces
`GRAINOWNERS` on every push (§8.1). Elsewhere it is **validated on read**:
a reader that finds a commit to a file its owners did not make, on a host
that cannot enforce it, treats the change as it treats any unauthorised
write on a non-Silo host (§8.10, §8.13).

**One writer per folder, still.** The table below is the same split as
before, now written as `GRAINOWNERS`:

| Repo or folder | Its owners | Field-level lines? |
| --- | --- | --- |
| a Grain's root | the Grain and its owner: its own lane and its ranks, its execution records and every state of them, the thread, its own tasks and triggers, its memory, and `grain.config.md` (the owner's alone) | none |
| Silo's per-Grain folders | Silo: placements, their ranks (for whoever asked, with `ranked_by`), `expedite`, the assigned triggers | none |
| a team's intake | submitters (new proposals), approvers (`approved`/`rejected`), members (ranks of unplaced work), Silo (API proposals, firings, cancels, the intake's references), the team's admins | **yes**: a task has several writers over its life (§8.2, §8.3) |
| a shared repo | its maintainers, directly or by merging review branches (§8.11) | none |
| a code repo's `.grain/` | whoever the repo's own merge process lets land a change (§8.13) | none: `GRAINOWNERS` there is validated on read, and documents intent |

### 3.6 Grain folders inside any repo

**Any repo may hold task-repo objects**: a code repo, a docs repo, a
monorepo. An object is any file with a kind suffix (§3.2), wherever it
is, and nothing is located by scanning. What matters is how something
reaches it:

- a Grain's **root entries** name folders in it, for refs, triggers or
  memory;
- **references** name its templates, tasks, designs and capability
  definitions;
- its **merge queue** reads its gates from its grain folder, `.grain/` by
  default (`grain_folder` in `.grainconfig`), when a change merges into
  it (§6.3).

A code repo needs no `.grainconfig` at all unless it has aliases, a
task home other than the intake, or a different grain folder. Its
top-level `README.md` belongs to the code, and is left alone.

```
README.md                                         the code's own README: an asset
.grainconfig                                      optional: task_home, aliases (§3.4)
GRAINOWNERS                                       optional: documents intent; validated on read
.grain/templates/release-notes.template.md        kind: template
.grain/triggers/weekly-release-notes.trigger.md   kind: trigger: active only for a Grain whose trigger root names .grain/triggers/
.grain/gates/grain-declarations.gate.md           kind: gate: read by this repo's merge queue
.grain/workflows/ci.yml                           a GitHub Actions workflow: YAML, an asset (§12.2)
.grain/tasks/<name>.task.md                       kind: task (homed here, §5.5)
docs/design/widgets.design.md                     kind: design (in place, §12.3)
```

**Identity does not change.** An object in a grain folder is named by its
repo URL and its **repo-relative** path, as anywhere else (§2.1):
`https://git.example.com/hub/widgets.git//.grain/templates/release-notes.template.md`.

**A code repo's triggers fire only for Grains that point a trigger root
at them.** Committing `.grain/triggers/nightly-ci.trigger.md` is a
proposal to every Grain; a Grain (or Silo's per-Grain folder for it)
enables it with an entry (§6.2).

### 3.7 The resolver: tree-only listing, lazy parsing, references

**Reachability, never scanning.** A Grain's **active set** is exactly
the objects its root entries activate. Referenced objects are resolved on
demand, one at a time. A file under neither is never read, so a repo may
hold any number of tasks, designs and archived runs without costing a
Grain anything until something points at them.

**Listing an entry reads trees only.** To list an entry, the resolver
reads the git **tree** of the entry's folder and its subtrees, at the
commit the entry names, and nothing else: no ancestor folder, no blob.
The kind of each file is in its name (§3.2), so filtering by type, by
`ignore:` and by `archive/` needs no blob at all.

```
DEFAULT_LIMITS = {objects_per_entry: 5000, objects: 50000, entries: 64,
                  bytes_per_object: 256 KiB}
                 # .grainconfig and grain.config.md may lower these, never raise them

active_set(config_ref):                        # config_ref = the Grain's local root setting
    C = parse(blob at config_ref)              # grain.config.md: the one blob always read
    check C.kind == config, C.grain.id == local id
    limits = min(DEFAULT_LIMITS, C.limits, each entry repo's .grainconfig limits)
    if |entries(C)| > limits.entries: fail loudly
    result = {}
    for (type, e) in entries(C):               # backlog lanes, triggers, memory
        L = list_entry(type, e, limits)
        if L is FAILED:  result[e] = UNKNOWN   # unreadable or over a limit (below)
        else:            result[e] = L
    drop from each entry the files a more specific entry of the same type also lists
    if total objects > limits.objects: fail loudly
    return result

list_entry(type, e, limits):
    (repo, folder, commit) = resolve_location(e.path, e.ref_mode, C's repo and commit)  # §4
    T = tree_hash(repo, commit, folder)                        # one tree read, or the cache
    key = (T, type, canonical(e.ignore), RESOLVER_VERSION)
    if key in listing_cache: return listing_cache[key]
    names = []
    walk the trees under T, depth first:
        skip a subtree whose name is "archive"                 # the built-in convention
        skip a path that e.ignore matches (gitignore rules,   # relative to folder; a matched
             negations included)                               # directory prunes the walk
        keep a file whose name ends in "." + suffix(type) + ".md"
        if |names| > limits.objects_per_entry: return FAILED   # never truncate
    listing_cache[key] = names                                 # (path, blob hash) pairs
    return names
```

**Lazy parsing.** A listing is names and blob hashes. Front matter and
bodies are parsed only when an operation needs them, each through the
front-matter cache (below):

| Operation | What it parses | Which fields |
| --- | --- | --- |
| list an entry, count, show the active set | nothing | names and kinds, from the tree |
| lane ranking | every ref in the lane | `rank`, `priority`, `expedite` |
| dispatch the next pick | that ref, then its task | the ref's `to`, `spec_rev`, `attestation`; the task's `state`, `depends_on`, `context`, `constraints`, `cancel` |
| dependency check (§8.9) | each `depends_on` target's runs | `state`, `closed.outcome` |
| policy checks before fetching (§5.6) | the task, each `context:` entry's target, each capability chain | `context`, the capability objects' `base` and `reducers` |
| register or re-register triggers | every trigger in the trigger roots | `on`, `enabled`, `limits` (the `fires` body is read when it fires) |
| fire a trigger | the trigger, then its template | `fires`; the template's `inputs` and body |
| give memory to a run | every memory document in the memory roots | `applies_to`, then the body of those that apply |
| render a prompt | the task, its template and partials | bodies |
| resolve a reference | the target, and any tombstone on the way | `kind`, `to` (a tombstone), `moved_to` (a repo's `.grainconfig`) |

**Reference resolution is one function**, shared by every consumer that
names another object: a backlog reference's `to:`, `context:` entries,
capability objects and their bases, a trigger's or task's template,
template partials, `depends_on`, `implements`, `derived_from`.

```
resolve_reference(ref, from, expected_kinds):
    T = resolve(ref, from, reader's credential, ALLOW)   # §5.6: the one resolver
    if T is dangling: report; return DANGLING            # never widened
    check kind(T) in expected_kinds, else WRONG_KIND     # from the suffix: no blob read
    return T                                             # parsed only when the caller needs fields
```

Resolving is a **read** with the reader's own credential (§11.18). It
never adds anything to the active set: a task's template, a capability
definition and a design are read when needed and are never fired,
ranked or run on their own.

**An entry the Grain cannot read is unknown, not empty.** While Silo's
folder is unreadable or over a limit, the Grain starts nothing new from
it, keeps the runs it has, and keeps its last good listing for display;
only a placement that is readably gone withdraws a run (§8.8).

**Limits fail loudly.** An entry over `objects_per_entry`, a config over
`entries`, or an active set over `objects` is never truncated, because a
truncated listing would silently choose which triggers fire. The entry
(or, for the total, every entry listed after the limit was crossed) is
`UNKNOWN`: the Grain shows the error in its status and Silo's health
view, alerts the owner, and Silo's hook refuses a push that would take a
folder it hosts over the limit of any root entry it knows names it. A
`*.ref.md`, `*.trigger.md` or `*.memory.md` over `bytes_per_object` is
refused by the hook and skipped and reported by readers.

### 3.8 Archival: the `archive/` convention

A Grain's `runs/` and a team's `tasks/` grow without bound, and nothing
needs the closed ones to be listed. **Closed tasks, references, runs and
threads move under an `archive/` folder**, and any folder named
`archive/` is skipped by every root entry (§3.3), by the orphan check
(§3.9) and by default listings in Silo's UI.

**The archive mapping, not tombstones.** Archiving is a move, and a moved
object normally leaves a tombstone (§2.1). For archival that would leave
one tombstone per closed task forever, which is exactly the growth
archiving is meant to stop. Instead, the move is **structural**:

> an object at `<top>/<rest>` is archived to `<top>/archive/<rest>`, where
> `<top>` is the first folder of its path in its grain folder (`tasks/`,
> `runs/`, `backlog/`, `triggers/`, `memory/`).

So `tasks/4310-carry-approver.task.md` becomes
`tasks/archive/4310-carry-approver.task.md`, and the whole folder
`runs/4310-carry-approver/` becomes `runs/archive/4310-carry-approver/`,
thread and attachments included. A reference to the old path that finds
nothing there tries the **archive mapping**, one deterministic extra
lookup, before it is dangling (§3.7). No tombstone is written, so a
repo's tombstones grow only with real renames.

- **Identity is unchanged.** The object's identity is still its live
  path; the archive path is where it is stored. References keep naming
  the live path, and Silo's reverse index (§4) resolves both.
- **No path reuse.** The hook refuses a new object at a live path whose
  archive path exists, as it refuses one at a tombstone (§8.1). A
  reopened task is moved back, by its owner, in one commit.
- **Who archives.** A path's owners in `GRAINOWNERS` (§3.5): Silo
  archives an intake's closed tasks after a retention period (90 days by
  default, in Silo's settings), and a Grain archives its own runs. Each
  archive is one commit with `move` on the paths.
- **References are still deleted, not archived,** where nothing needs
  them: a closed placement is removed (§5.2). An owner may archive a
  ref instead, to keep it for reading; it is inactive either way.
- **Retiring a trigger** is moving it to `triggers/archive/` (§8.15):
  every Grain whose trigger root names `triggers/` stops firing it, and
  the definition is still readable at the same identity by the firings
  that name it.
- **A really old archive** may be split into a separate repo: the
  repo's `.grainconfig` sets `archive_repo: <git URL>`, and the mapping
  looks there second. This is optional and not needed in the stages of
  §16.

### 3.9 The orphan check

A `*.<kind>.md` file that no root activates and nothing references does
nothing, and is usually a mistake: a trigger committed to the wrong
folder, a reference left in a folder no Grain lists. `grain lint` (and,
optionally, Silo's hook) reports it.

**Known**, for Silo, means:

- every **root entry** in a `grain.config.md` Silo knows about: the
  roots of Grains linked to it, and any Grain root it hosts;
- every object one of those entries activates, and every object
  **reachable by references** from them, transitively, under the
  resolver's limits: tasks from refs, templates from triggers and tasks,
  partials, designs from `implements:` and from `design-change`
  triggers' `paths`, `context:` targets, capability objects and their
  bases, `depends_on` targets, and the targets of tombstones;
- a repo's own `grain.config.md`, `.grainconfig` and `GRAINOWNERS`, its
  gates under its grain folder (§6.3), and its execution records and
  comments under a known task or run.

For `grain lint` on a single repo, known means the same with only the
`grain.config.md` files in that repo and the ones its `--config` flags
name.

A file under `archive/` is never an orphan. An orphan is a **warning**
in lint and in Silo's health view, unless the repo's `.grainconfig` sets
`orphans: refuse`, in which case Silo's hook refuses a push that creates
one. A newly authored task is not an orphan if the same push adds a
reference to it, or if its task home's intake references it (§8.15).

### 3.10 Caches and sparse fetch

**Steady-state cost is O(changes).** Two content-addressed caches make it
so:

- **The listing cache**: a resolved entry's listing, keyed by **the
  entry folder's tree hash**, the root type, the entry's normalised
  `ignore:` and the resolver version. A tree hash covers its whole
  subtree, so an unchanged folder hits the cache whatever happened
  elsewhere in the repo.
- **The front-matter cache**: parsed front matter, keyed by **blob
  hash** and schema version. Bodies are cached separately and only for
  the objects that needed them (templates, memory, prompts being
  rendered).

**Invalidation is automatic**, because the keys are hashes:

- a change in a folder changes that folder's tree hash and its
  ancestors', so only entries whose folder (or a subfolder) changed miss;
- an edit to `grain.config.md` changes the set of entries. Entries whose
  folder, commit and `ignore:` are unchanged still hit; an edited
  `ignore:` misses for that entry only;
- a new `@ref` tip for a tracked entry is a new commit; if the entry's
  folder tree is unchanged, its tree hash is the same and it hits.

**Where the caches live.** On a Grain, in its local database beside its
other runtime state (§1.2), bounded and evicted least-recently-used. In
Silo, in its own database, shared across every Grain whose entries name
the same folder (the hook updates the listing cache for the folders a
push changed, so Silo's views and its orphan check are warm). Nothing is
cached in git, and losing either cache costs one cold start.

**Sparse fetch.** A Grain clones nothing whole. For each repo its entries
name it keeps a partial clone (`--filter=blob:none`, and
`--filter=tree:0` where the host supports it) and fetches:

- the **trees** along each entry's path and under its folder, at the
  entry's commit;
- **blobs on demand**, only when an operation in the table of §3.7
  parses them;
- **referenced objects individually**, when a reference resolves (a
  task's file, its template, a design), as single blobs at the named
  commit, with the trees along their path.

A `pull` notification from Silo ("repo moved to `<sha>`") triggers a
fetch of the new commit's trees for the entries in that repo, and nothing
else.

**Estimates: a Grain with 10k tasks across 5 repos.** Say the 10k tasks
are in two intakes and three code repos, of which 400 are open and
placed or queued; the Grain has 3 lanes (its own 50 refs, Silo's 300,
a colleague's 50), 3 trigger entries (60 triggers), 2 memory entries
(120 documents), and 4,000 runs in `runs/archive/` plus 200 live.

| | Cold start | Warm, after a one-file push |
| --- | --- | --- |
| trees read | ~25 (the entry folders and the paths to them; no ancestors' siblings) | 2–4 (the changed folder and its parents up to the entry) |
| blobs read to list | 0 | 0 |
| blobs to rank lanes | 400 refs, ~0.5 KB each, ~200 KB | 1 (the changed ref) |
| blobs to register triggers | 60, ~60 KB | 0, or 1 if it was a trigger |
| blobs for memory, at the first run | 120, ~300 KB | 0, or 1 |
| tasks read | 1 per dispatch (plus its `depends_on` targets' runs), never the other 9,999 | 0 |
| bytes over the wire | ~1 MB, dominated by refs' and memory's blobs | a few KB |
| time | about a second on a cold link, most of it round trips | milliseconds |

The 10k tasks, the 4,000 archived runs, every design doc and every
template not in use cost nothing: they are under no entry and read only
when something references them.

### 3.11 Prior art, and what is borrowed

- **Backstage's software catalog.** Every entity is a YAML document with
  `apiVersion`, `kind`, `metadata.name` and `spec`, and `Location`
  entities point at more of them transitively. The design takes **a kind
  plus a spec and a body**, and **relations as references**. It does not
  take `Location`'s transitive discovery: an active set here is listed
  per typed root entry, and everything else is a reference. Nor does it
  take the envelope: `metadata.name` cannot be a path, references are
  resolved in a catalog database rather than by location, and the
  catalog does not read Markdown. An optional exporter (`silo catalog
  export`) emits Backstage-compatible YAML for a repo's objects, one
  entity per object with the source path in an annotation, and never
  reads it back. A task may name catalog entities as labels (`catalog:
  [component:default/payments]`); they activate nothing and grant no
  read.
- **CODEOWNERS** gives `GRAINOWNERS` its syntax, its nearest-file
  cascade and its last-match rule; the field suffix is the one addition.
- **gitignore** gives each entry's `ignore:` its pattern syntax, but not
  its cascade: patterns live on the entry, never in the folder (§15).
- **Plan 9's per-process namespaces** gave 4452 its model of a union
  built per Grain. The typed roots keep the useful part (two Grains can
  act on different sets of the same global objects, and names never
  change) and drop the union's transitivity.

## 4. References: plain git, no forge


```
ref     = [ repo "//" ] path [ "@" gitref ] [ "#" anchor ]
        | alias ":" path [ "@" gitref ] [ "#" anchor ]
repo    = a git URL: https://…, ssh://…, file://…      (no scp-style user@host:path)
path    = in-repo path, "/"-rooted, or relative to the referring file
gitref  = branch | tag | 40-hex sha                     (the last "@" after the "//")
anchor  = a heading slug or a requirement ID            (HS-3, D-2)
```

Examples:

- `../templates/prune-packages.md`: the same repo, relative to the
  referring file.
- `https://silo.example.com/git/hub/shared.git//designs/hub-intake.md@main#HS-3`:
  across repos, by the repo's URL. Silo-hosted repos, GitHub, Gerrit and a
  bare `git daemon` all use the same form, because the URL is just a
  `git clone` argument. The `//` separator is the one Terraform uses, and
  it is the first `//` after the URL's `scheme://authority`.
- `shared:designs/hub-intake.md#HS-3`: an **alias**, declared in the
  referring repo's `.grainconfig` (§3.4). It resolves to exactly the URL form. An alias
  that is not declared is an error, never a guess.

**A reference in front matter is a string** in this grammar. Where an
edge records more than its target, it is an object whose `ref:` is that
string, as in `template: {ref, rev, inputs}`. The path is the target's
identity (§2.1), so there is nothing else to check: a reference names
exactly the object at that address, or the object a tombstone there
redirects to. The Markdown body may use bare refs in ordinary links.
Those are for humans and carry no meaning.

**Resolution:**

1. **Canonicalise.** Expand the alias, and resolve a relative path
   against the referring file, to get the canonical global identity
   (§2.1). The aliases are those of the referring repo's `.grainconfig`
   (§3.4), one file at the repo root; nothing is discovered by walking
   folders. A `/`-rooted path is rooted at the repo.
2. **Unpinned references** resolve to the tip of the target repo's
   `default_ref` at the moment they are read. Anything recorded for
   traceability records the commit sha that was actually used: a task's
   `template.rev` and `implements[].rev`, a backlog reference's
   `spec_rev`, and an execution record's `task.rev`, `from.rev` and
   `closed.design_revs`. So a reference reads live and is recorded
   pinned.
3. **Pinned references** (`@v3`, `@<sha>`) resolve to exactly that ref.
   A branch pin follows the branch's tip.
4. **Redirects.** If the target repo's `.grainconfig` has `moved_to`,
   retry at that URL with the same path. If the path is missing and a
   tombstone, `<path>.moved.md`, sits beside it, follow its `to:`,
   resolved relative to the tombstone. If neither, try the archive
   mapping (§3.8), which is not a hop. Each redirect is one hop:
   - a reader follows **at most 8 hops** in total;
   - it keeps the set of canonical identities it has visited. Arriving at
     one twice is a **redirect loop**;
   - a loop, or a chain longer than 8, is reported and treated as
     dangling.

   Silo shortens chains it owns: when a backlog reference's `to:` lands
   on a tombstone, Silo rewrites `to:` to the end of the chain on its next
   pass.
5. **Cycles:**
   - `depends_on` cycles are rejected on write in Silo-hosted repos. A
     reader treats every task on a cycle as blocked and reports the cycle.
   - Include/import cycles are rejected at render time.
   - Redirects are bounded as in step 4.
6. **Dangling references:** the target is missing, the reader may not
   read it (§11.4), or a redirect chain breaks. Silo's hook refuses a
   *new* reference that does not resolve with the pusher's rights (§8.1).
   A reference that resolved when it was written can still dangle later,
   because no transaction spans repos (§8.11). So readers handle it at
   read time:
   - a dependency stays **blocked**, with the reason shown;
   - a template reference makes the task **unrenderable**;
   - an `implements` reference is shown as **unresolved** in
     traceability;
   - a backlog reference is **skipped and reported** (§5.2).

   Silo re-checks references on every push it receives, and lists the
   dangling ones in the repo's health view.

**One resolver.** These steps are one function, `resolve`, with a gate
supplied by its caller. Namespaces (§3.3) call it with the reader's
credential as the only gate; a task's `context:` calls it with the
Grain's policy as the gate too, checked before every fetch (§5.6).

**Indexes.** Nothing needs an id index any more, because a reader opens
the path. Silo does keep **reverse indexes**, rebuilt from git on start
and updated on each received push: task → backlog references, task →
execution records, task → dependents, requirement → tasks, asset →
sidecars, and `aka` → path.

**The edges:**

| Edge | Field | Notes |
| --- | --- | --- |
| backlog reference → task | `to: <ref>` + `spec_rev` | `spec_rev` is the commit of the task's repo the placement was made against (§5.2) |
| execution record → task | `task: {ref, rev}` | `rev` is the commit of the task that was run |
| execution record → backlog reference | `from: {ref, rev}` | `rev` is the commit of the reference's repo (Silo's placements, or the root) it was read at, so it still resolves after the reference is removed |
| execution record → earlier run | `previous: <ref>` | set on re-placement (§8.8) |
| task → template | `template: {ref, rev, inputs: {…}}` | `rev` is the template commit rendered; `inputs` are the values used |
| task → dependencies | `depends_on: [<ref>]` | "done" means the target task has a `done` execution record (§8.9) |
| task → trigger of origin | `trigger: {ref, on, key, fired_at, event}` | written by whoever fired it (§6.2) |
| task → design requirements | `implements: [{ref: …#HS-6, rev, hash}]` | the requirement and its revision (§7.2) |
| task → the run that filed it | `filed_by: {grain, run: <ref>}` | an agent's upward filing (§8.12) |
| trigger → template | `fires: {template: <ref>, inputs, bind}` | unpinned, so a firing uses the template's current revision |
| trigger → workflow | `fires: {workflow: <path>}` | a plain path to a workflow file in the trigger's own repo (§12.2) |
| trigger → watched designs | `on.design-change.designs: [<ref>]` | read on each event; adds no name (§7.4) |
| gate → its review templates | `requires.security_review.template`, `requires.second_review.template` | optional; grain's built-in otherwise (§6.3) |
| comment → what it discusses | `run: <ref>` or `task: <ref>` | usually `../<utc>.run.md` or `../../<name>.task.md` (§5.4) |
| tombstone → new address | `to: <ref>` | the moved object lists the old address in `moved_from` |
| Grain config → its active roots | `backlog.lanes[].path`, `triggers[].path`, `memory[].path`, each with `ref_mode` and `ignore` | **the only edges that make anything active** (§3.3) |
| trigger → the trigger it was customised from | `derived_from: <ref>` | provenance only: inherits nothing (§6.2) |
| repo → its new URL | `.grainconfig` `moved_to` | a repo-level redirect |
| task → what its sandbox holds | `context: [{ref, mode, path, depth}]` | **the only edge that materialises anything** (§5.6); a relative entry reads the task's own commit |
| comment → its attachments | `attachments: [<ref>]` | assets beside the comment (§2.2) |
| capability → its base | `base: {capability: <ref>}`, or `{type: <base type>}` | narrows that object further; a chain is resolved with cycle detection and a depth limit, and every link must be a sanctioned source (§5.7) |
| task → a capability it requests | `context: [{ref, mode: use, reducers}]` | a request, never a grant: the Grain's policy decides (§5.7) |
| sidecar → its asset | `asset: <ref>`, or `external: {url, sha256}` | metadata only (§2.2) |

## 5. Tasks, backlogs, execution records and comments

A task is written once, in the intake where it was proposed, and is
never copied. It stays there unless its task home says otherwise, in
which case it moves once, leaving a tombstone (§5.5). Three other kinds
point at it, by path, wherever it lives:

- a **backlog reference** puts it in a queue, at a rank (§5.2);
- an **execution record** says what one Grain did with it (§5.3);
- **comments** discuss it, or discuss one run of it (§5.4).

```yaml
---
kind: task
schema: grain/task@v1
aka: [grain:eu/task/4310]
title: Carry a hub task's approver on its offer and attestation
team: hub
context:                    # what its sandbox holds (§5.6); the first write entry is its repo
  - {ref: "https://github.com/bwsalmon/silo.git@main", mode: write}
  - {ref: "shared:designs/hub-intake.md"}
state: approved             # proposed | approved | rejected | cancelled (§5.1)
submitter: user:bsalmon@google.com
approved_by: user:approver@example.com
approved_at: …
implements:
  - {ref: "shared:designs/hub-intake.md#HS-2", rev: <design commit>, hash: "sha256:…"}
created_at: …
---
<the prompt, in Markdown>
```

This file is `intake:tasks/4310-carry-approver/task.md`. The full
examples are in [`examples/task-repos/`](../examples/task-repos/README.md),
and `python3 examples/task-repos/check.py` checks them against the
schemas, resolves every reference, follows the tombstones, computes each
backlog's order, and renders the template fixture.

### 5.1 Task states

```
proposed → approved → cancelled
    ↘ rejected  ↘ (placed, run and closed: recorded elsewhere, §5.2–§5.3)
proposed → cancelled
```

Each state has one writer:

- `proposed`: the submitter.
- `approved` and `rejected`: an approver.
- `cancelled`, with `cancel: {by, at, reason}`: Silo, on a user's
  request or when an approved plan supersedes the task (§7.4). A Grain
  honours it (§8.7).

On a standalone Grain, the Grain and its owner hold every role.

**After approval, the spec is frozen.** The hook refuses any change to
`title`, the body, `context` (its capability requests included, §5.7),
`template`, `depends_on`, `implements` or `constraints`, and any change
to the task's `attachments/` (§2.2). `capability_approvals` is not spec:
like `approved_by`, it is an approval, appended after the task's own
(§5.7). An attestation's
`spec_digest` therefore stays true, and a changed request is a new task
with `supersedes:`.

**How a task is going is derived, never written on the task.** The task
file holds only what its author and Silo decided. Its progress is read
from the objects that point at it:

| Derived status | When |
| --- | --- |
| unplaced | approved, and no Grain backlog references it |
| queued on `<grain>` | a Grain backlog references it, and that Grain has no open run of it |
| `accepted`, `running`, `waiting` | from its open execution record |
| `done`, `failed`, `refused`, `cancelled` | from its newest execution record that is not `superseded` |
| cancelled | the task says `cancelled` and no run is open |

**Silo writes no outcome onto the task at close.** It could mirror a
`closed:` summary into the task file, but that has three costs:

- every close would become a cross-repo transaction (Grain repo, then
  intake), with a window in which the two disagree;
- Silo would become a second writer of a file whose spec is frozen;
- two copies of an outcome drift, as 4446's copied tasks would have.

The cost of deriving is that the intake alone does not show outcomes.
Silo's views, `silo task show` and the API serve them from the reverse
index (§4). A reader without Silo finds the same answer in git, by
scanning the Grain repos it can read for `kind: run` files whose `task`
is the task. Where the outcome is needed outside git, Silo mirrors it
there, as it does today: a comment on a GitHub `origin`, or the hub's
database projection.

### 5.2 Backlogs: folders of ranked references

A repo's backlog is a folder of `*.ref.md` files (`backlog/` by
convention). A Grain's queue is every reference its `backlog:` lanes
activate (§3.3): its own lane, Silo's folder for it, and any other lane
its owner names. Each reference is small:

```yaml
---
kind: ref
schema: grain/ref@v1
to: "intake:tasks/2026-10-16-approval-flow.task.md"
spec_rev: 0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80   # the task commit placement read
rank: "t"                   # LexoRank-style key, compared bytewise
priority: high              # optional: urgent | high | normal (default) | low
expedite: false             # optional; Silo's (at_front)
added_by: silo
added_at: "2026-10-16T17:12:00Z"
ranked_by: "user:bsalmon@google.com"
ranked_at: "2026-10-16T17:20:00Z"
attestation: <signed claims JWT, hub.md §6.4>
---
```

**Ranks.** A rank is a string over `0-9a-z` that never ends in `0`, and
ranks compare bytewise. To put a reference between ranks `a` and `b`,
pick any string strictly between them. The midpoint in base 36 works,
lengthening by one digit when `a` and `b` are adjacent. There is always
room, so ranks never need rebalancing. An owner who wants tidier keys
may rewrite them, which is one commit. This is the LexoRank idea without
its bucket prefix.

**Filenames stay stable.** Order comes from `rank` inside the
reference, never from renaming files. A reorder edits one field in one
file. Two people reordering different references never conflict. Two
reordering the same one conflict on one field, and the second push
rebases and re-applies its move.

**Lanes.** A Grain's queue comes from several sources, each with its own
writer: the root's own `backlog/`, Silo's folder for this Grain, perhaps a
second team's placements or a colleague's shared queue. **Each backlog
entry in `grain.config.md` is a lane** (§3.3): a reference belongs to the
most specific entry that lists it, so its lane is fixed by its folder.
**Ranks are compared only within a lane.** Two writers' ranks are not
comparable (Silo's `t` and the owner's `t` mean nothing to each other), so
merging them would let whichever writer picked more extreme keys win.

**Effective order within a lane:**

1. references whose task's dependencies are all satisfied (§8.9), ahead
   of those still blocked;
2. then `expedite: true` ahead of everything else;
3. then `priority`: `urgent`, `high`, `normal`, `low`;
4. then `rank`;
5. then the task's `created_at`, and finally the reference's path.

**Between lanes, the root decides**, with `backlog:` in its
`grain.config.md`:

```yaml
backlog:
  mode: weighted            # strict (the default) | weighted | round_robin
  expedite: lane            # Silo's expedite reorders within its lane (default), or global
  lanes:                    # first is highest
    - {name: silo, path: "placements:grains/eu/backlog/@main", ref_mode: track, weight: 3}
    - {name: own, path: backlog/, weight: 1}
```

- **`strict`**: take the first runnable reference of the first listed
  lane that has one. A lane whose references are all blocked does not
  hold up the next lane.
- **`weighted`**: smooth weighted round-robin over the lanes that have a
  runnable reference. Each pick, every such lane gains its weight in
  credit; the lane with the most credit (ties in list order) gives its
  next reference, and pays the total. With weights 3 and 1, Silo's lane
  gets three picks in four. The credits are runtime state, in the
  Grain's database, not in git.
- **`round_robin`**: `weighted` with every weight 1.

A Grain skips references whose task is `cancelled`, or which it already
has an open or closed run of, and picks a task referenced from two lanes
once, at whichever comes first.

**Why lane priority is declared in `grain.config.md`:** it is one
decision about the set of lanes, not a property of any one of them, and
the root's owner owns it. A lane's writer (Silo, for its folder) cannot
promote its own lane, because nothing it writes is read for priority.
Every lane is an entry in the same file, so there are no unlisted lanes.

In the examples, Grain `eu` weights Silo's lane 3 to 1 against its own,
and `check.py` prints the order it would take its runnable references:
the expedited `2026-10-16-reject-dangling-refs` and the `high`
`2026-10-16-approval-flow` from Silo's lane, then the owner's
`2026-10-16-tidy-run-notes`, then `2026-10-16-widget-schema-v2` (§8.15).

**Who writes what: one writer per lane.**

- **Silo's placements** (`placements:grains/<grain>/backlog/`): Silo adds
  references (placement) and removes them (re-placement, cancel, close).
  It sets `rank`, `priority` and `expedite`, and rewrites `to:` to the end
  of a redirect chain. Whoever reorders a Grain's placements, the Grain's
  owner included, does it through Silo's UI or API (`grain backlog move`
  calls it for a placement), and Silo commits with `ranked_by` set to
  them. The owner's last word over their Grain is the lane order, and
  whether Silo's folder is a lane at all, not Silo's ranks.
- **The root's own lane** (`backlog/`): the Grain and its owner, for
  everything: the references, their ranks, adding and removing them.
- **In an intake's backlog**, the team's members own `rank` and
  `priority`. They push directly, or reorder through Silo's UI, in which
  case Silo commits with `ranked_by` set to the member it acted for.
  Silo adds and removes the intake's references.

**Lanes for triggered and gated work.** A trigger's firing on a Grain
(§6.2) writes its reference in the root, in `backlog/fired/`, and the
review tasks a gate files on a Grain's merge queue (§6.3) in
`backlog/gated/`. Both are under the own lane's folder, so by default
they sort with the owner's own work. An owner who wants standing work
weighed against ad-hoc work, or held merges cleared first, adds an entry
for the subfolder (`{name: gated, path: backlog/gated/, weight: 4}`),
which, being more specific, takes those references out of `backlog/`'s
lane (§3.3). A firing in Silo is placed like any task: its reference is
a placement, in Silo's lane, or waits in the intake's backlog for a Grain
(§6.2). A gate review that Silo files sits in the intake's backlog, ahead
of the team's unplaced work, because a merge is waiting on it.

**One task, many views.** The intake's references are the team's
ordering of its unplaced work. The same task can be referenced from the
intake and from a Grain's placements, each at its own rank. Placement
does not remove the intake's reference. When a task closes, or is
cancelled or rejected, Silo removes every reference to it from the
repos it writes. Silo's folder for a Grain is therefore that Grain's
open placements, and the Grain's `runs/` is its history.

**Placement reads the intake's order.** A new placement copies the rank
and priority of the task's intake reference, if it has one, so the
team's order is kept among its tasks. Otherwise it is ranked after the
lane's last reference.

**The attestation lives on the placement.** It is Silo's signed claim to
this Grain (`aud`), about this task (its canonical global identity), with
`spec_digest` computed over the task's spec at `spec_rev` (§11.7).

**Dangling references.** A reference whose `to:` no longer resolves:

- if a tombstone redirects it, readers follow the tombstone (§4) and
  Silo rewrites `to:`;
- otherwise readers **skip it and report it**. The Grain shows it in its
  status and never runs it. Silo lists it in the repo's health view and
  removes it once it has told the backlog's owner.

On Silo-hosted repos the hook **refuses a new or changed reference whose
`to:` does not resolve** at push time, following redirects, with the
pusher's read rights, to a `kind: task` (§8.1). It also refuses a second
reference to the same task in the same lane, which makes placement
idempotent.

### 5.3 Execution records (`kind: run`)

A Grain records its work on a task **in its own repo**, as an execution
record. One record covers one placement of one task on one Grain,
whatever number of attempts it took. The attempts themselves are
runtime state (§1.2).

```yaml
---
kind: run
schema: grain/run@v1
task: {ref: "intake:tasks/4310-carry-approver.task.md", rev: <intake commit>}
from: {ref: "placements:grains/eu/backlog/4310-carry-approver.ref.md", rev: <placements commit where it was read>}
grain: eu
state: done                 # accepted | running | waiting | done | failed | refused | cancelled | superseded
accepted_at: "2026-10-16T13:58:30Z"
started_at: "2026-10-16T13:58:31Z"
closed:
  outcome: done             # done | failed | cancelled | nothing_needed
  at: "2026-10-16T14:58:30Z"
  summary: …
  change: {url: …, branch: grain/eu/task-4310, merge_commit: 5623aa25…}
  design_revs: [{item: …#HS-2, rev: …, hash: …}]
  follow_ups: ["intake:tasks/2026-10-16-approval-flow.task.md"]
---
<optional: the closing report, in prose>
```

This file is `eu:runs/4310-carry-approver/20261016T135830Z.run.md`. Once closed
for the Grain's retention period, the Grain archives the folder to
`runs/archive/4310-carry-approver/`, and references to it still resolve
through the archive mapping (§3.8).

```
accepted → running ⇄ waiting → done | failed | cancelled
    ↘ refused (by local policy, with refusal: {reason, at})
accepted | running | waiting → superseded (the Grain, once it sees its placement withdrawn, §8.8)
```

- **`waiting`** is a coarse state for a run held on something outside
  it: a dependency, a human's answer, a held gate, or the Grain's own
  policy refusing a `context:` entry (§5.6). `waiting: {on, entry,
  reason}` says which.
- **The Grain writes every state**, `superseded` included: its root has
  one writer. When Silo withdraws a placement (re-placement, §8.8), it
  writes only in its own repo. An open run whose `from` is a placement
  that is **readably gone** is **fenced**: readers treat it as
  superseded, and Silo's hook refuses every push to it except the
  Grain's own `state: superseded`, with `superseded: {at, by, withdrawn_at,
  reason}`. `done`, `failed`, `refused`, `cancelled` and `superseded` are
  terminal, so a returning Grain cannot close a run that was taken away
  from it.
- **The Grain never writes into another repo**: not Silo's, and not the
  task author's. Silo reads Grain repos to build the hub's views and its
  reverse indexes, and mirrors outcomes where they are needed outside git
  (§5.1).
- **Re-placement keeps both attempts.** The old Grain's record stays
  where it is, `superseded`. The new Grain starts its own with
  `previous:` pointing at the old one, so history shows both.

### 5.4 Comments

A comment has `kind: comment` and exactly one of `run: <ref>` or
`task: <ref>`. It also has `author` (`user:…`, `grain:eu`, or
`agent:grain:eu/<run path>`), `at`, an optional `in_reply_to`, and a
body. Comments are append-only: an edit is a new comment with
`supersedes:`.

**The thread of the work references the execution record**, and lives
beside it in the Grain's root (`runs/<name>/comments/`):

- the Grain writes only its own repo, and the agent's thread is written
  while it runs;
- a thread belongs to one attempt. After re-placement each Grain's
  thread stays with its own record, so neither is split or merged;
- a person replying to a running agent writes through Silo's UI or API.
  Silo relays the reply to the Grain over its node connection, and the
  **Grain** commits it under the run, with the person as `author` and
  `via: silo`. Silo never writes the Grain's root. The Grain's owner may
  also push a comment directly.

**Discussion of the task itself**, before it is placed or about whether
to do it at all, references the task, and lives beside it in its repo
(`tasks/<name>/comments/`).

The hub shows one thread per task: the task's comments and every run's
comments, merged by time and labelled by Grain.

### 5.5 Where a task lives: the task home

A task's **home** is the repo that holds its file. Every task starts in
the team's intake; where it lives once it is approved is **an option**,
`task_home`, set per team, with an optional per-repo override:

| Mode | Where an approved task's file lives | For |
| --- | --- | --- |
| **`intake`** (the default) | the team's intake, where it was proposed. Code repos hold only the definitions someone deliberately committed there | most work: nothing is written to a code repo but the work itself |
| **`code-repo`** | the target code repo's grain folder (`.grain/tasks/<name>.task.md`), committed on approval through that repo's own review and merge. The intake path becomes a tombstone | teams that want every task reviewed and versioned with the code |
| **`promote`** | the intake, until an explicit **promote** moves it into the code repo, leaving a tombstone | long-lived or design-linked work, chosen task by task |

**Configuring it.** The team's setting is in its intake's `.grainconfig`;
a code repo's is in its own `.grainconfig` (§3.4):

```yaml
# hub-intake/.grainconfig: the team
task_home:
  mode: intake                          # the team's default
  allow: [intake, promote, code-repo]   # what a code repo may choose for itself (default: all three)
  repos:                                # per-repo overrides
    https://git.example.com/hub/widgets.git: promote

# widgets/.grainconfig: the code repo
task_home: {mode: promote, path: .grain/tasks/}  # path: where tasks are committed, repo-relative
```

A bare mode (`task_home: promote`) is short for `{mode: promote}`. The
**effective mode** for an approved task is the first of these that is
set, for the code repo that is the task's primary repo, its first
`write` entry (§5.6):

1. that code repo's `.grainconfig` `task_home.mode`, if the team's
   `allow` lists it;
2. the team's `task_home.repos` entry for that repo;
3. the team's `task_home.mode`;
4. `intake`.

A task with no `write` entry (team-targeted, or read-only) is always
homed in the intake.
The code repo's choice comes first because nothing lands in it except
through its own review anyway, so its maintainers have the last say over
what is committed there. The team's `allow` bounds it because a task's
prompt, once committed to a code repo, is readable by everyone who can
read that repo, which may be more people than can read the intake.

**Rules for every mode:**

- **Proposals always start in the Silo intake** (§8.2). Proposing never
  needs GitHub, a code-repo account or code-repo write access.
- **Approval always happens in the intake**, under the hook's approver
  rules (§8.3). Only an approved task leaves the intake, so a task in a
  code repo always carries `moved_from:` naming its intake tombstone.
  Silo places a task homed in a code repo only if that tombstone points
  back at it, and its spec digest equals the spec that was approved in
  the intake. A task someone commits straight into a code repo is a
  definition Silo will not place: to run it, propose it in the intake.
  A standalone Grain whose local policy trusts that repo may run it
  (§8.13).
- **Trigger-fired task instances are never committed to code repos.**
  Only trigger definitions are (§6.2). `task_home` never applies to a
  task with `trigger:`, Silo never promotes one, and readers reject a
  `kind: task` with `trigger:` in a code repo's grain folder. That holds
  for every source, a schedule's firing or a push's, and for the review
  tasks a gate files (§6.3).
- **Backlogs and execution records do not care which mode is used.** A
  backlog reference's `to:`, a run's `task:` and a dependency's
  `depends_on` name the task by path or git URL wherever it is (§4).
- **The spec stays frozen** across the move (§5.1). The file at its new
  home is the intake's file with `moved_from` added. The only other
  change allowed is to how its references are written: an alias or a
  relative path that meant something in the intake is re-expressed for
  the new home's `.grainconfig`, and must name the same canonical identity
  (§2.1). The spec digest is computed over canonical references, so it
  does not change. The example task's `implements:` was
  `widgets:docs/design/widgets.design.md#WG-2` in the intake, and is
  `/docs/design/widgets.design.md#WG-2` in the widgets repo.

- **A move follows the identity rules of §2.1:** the new home first, then
  the tombstone in the intake, then Silo rewrites the references it owns
  (§8.11). It rewrites a backlog reference's `to:` to the new address and
  its `spec_rev` to the commit the task landed in. The attestation is
  left as it was: it names the task's intake identity, which now
  redirects to `to:`, and its `spec_digest` still matches (§11.7).
  Execution records already written keep naming the intake address, and
  resolve through the tombstone.

**How `code-repo` mode lands the file.** On approval, Silo commits the
task into the code repo's grain folder through the repo's normal path:

- **a direct push**, where the host lets Silo's identity write that path
  without review: a GitHub ruleset bypass for Silo's app limited to
  `.grain/tasks/**`, or the equivalent Gerrit or depot permission. One
  commit on the default branch;
- otherwise **a small change**: Silo pushes one commit adding the file to
  a branch `silo/task-home/<name>`, and opens a pull request or CL. The
  repo's checks, triggers and gates run on it as on any change, and
  grain's merge queue lands it. In a repo whose `.grain/**` gate requires
  a human (as bwsalmon/silo's `grain-declarations` gate does, §6.3), that
  is a human's Submit per task. That cost is the repo's choice, and one
  reason `intake` is the default.

**While that change is pending**, the task is still at home in the
intake, and is fully usable there. Silo places it from its intake
address without waiting, and a Grain can run it. A cancel is written on
the intake file as usual. Silo records the pending change in its
database, and as a comment on the intake task naming the change's URL.
Then:

- **the change merges:** Silo reads the merged file and checks it: its
  spec digest equals the intake's, and `moved_from` names the intake
  path. Then it commits the tombstone in the intake and rewrites the
  references. A file edited in review fails the check: Silo writes no
  tombstone, reports the divergence on the task, and the task stays in
  the intake. The file in the code repo is then a definition nobody
  places, and readers report its `moved_from` as not a tombstone;
- **the change is closed unmerged,** or is still open after 7 days: Silo
  closes it, comments on the task, and the task stays in the intake;
- **the task is cancelled first:** Silo closes the change. Nothing needs
  to move.

**Promote** works the same way, started by hand instead of by approval:

- an **owning-team member** (`team:<team>#member` of the task's team)
  runs `silo task promote <task>`, or uses the UI;
- an **agent** proposes it: a `propose_promotion` request over its node
  connection, as `propose_task` is today. Silo commits a comment on the
  intake task recording the proposal, and a member confirms it. Nothing
  moves on an agent's word alone.

Any approved task whose home is the intake may be promoted, closed
ones included: its runs keep resolving through the tombstone. A
proposal cannot be promoted (approval happens in the intake), and
neither can a rejected, cancelled or trigger-fired task. The target is the
task's primary repo (§5.6), and the promotion fails if that
repo's effective mode is `intake`. A promotion is a move, so it cannot be
undone by moving back to a tombstoned path (§2.1). A task promoted by
mistake is cancelled where it now lives.

**Cancelling a task homed in a code repo** writes `cancelled` where the
task lives, through the same direct push or change. Until that lands,
Silo acts in its own repos, at once: it removes the task's placements
and intake reference, so no Grain starts it, which fences any open run
as re-placement does (§8.8), and it comments on the intake task with
`cancel pending in <change URL>`. A cancel committed by the code repo's own
maintainers is honoured too: it only ever stops work.

The worked flows, with the commits each repo receives, are in §8.14.

### 5.6 What a run sees: `context:`

A task declares **everything its sandbox holds** in one list, `context:`.
Each entry is a reference (§4) to a repo, a folder of a repo, or a single
file, with a mode. When a Grain runs the task, it resolves the list,
holds every entry to its local policy, and materialises exactly that in
the sandbox. **Nothing else reaches the sandbox.**

```yaml
context:
  - ref: "https://github.com/bwsalmon/silo.git@main"                 # a repo
    mode: write                                                       # push the task's branch, based on main
  - ref: "https://github.com/bwsalmon/grain.git//pkg/daemon/@candidate" # a folder: sparse checkout
    mode: read
  - ref: "shared:designs/hub-intake.md"                               # an object
    depth: 1                                                          # and the files its body links to
  - ref: attachments/dangling-ref-error.png                           # an asset beside the task (§2.2)
    path: context/screenshot.png                                      # optional: where it lands
```

This is the `context:` of
[`hub-intake/tasks/2026-10-16-reject-dangling-refs/task.md`](../examples/task-repos/hub-intake/tasks/2026-10-16-reject-dangling-refs/task.md),
walked through in §8.16. A bare string is short for `{ref: <string>,
mode: read}`.

**It replaces four things.** Grain's `model.Task` has a target repo
(`Target`, with `Base`), extra repos to read (`Reads`) and extra repos to
land work in (`Writes`). Ad-hoc context (attachments, pasted file
contents, "see this doc") travels in the prompt. Those are four
mechanisms for one idea, *what this run may see and change*. Each is now
an entry. 4446's `repo:` and `base:` on a task, template and schedule (now a trigger) are
gone: the task's **primary repo** is its first `write` entry, and its
base is that entry's `@ref` (below). §13 maps the old fields.

**Why `context`.** The field says what the list is for: the context the
work is done in, which is also how agent frameworks name what a model is
given. Other names were weighed:

- `mounts:` describes one mechanism, and implies a filesystem mount. An
  entry may be a git checkout, a sparse checkout or a copied file, and
  a `write` entry grants a push, which no mount does.
- `workspace:` suggests the one writable checkout. Most entries are
  read-only.
- `sandbox:` already names a workflow job's resources (`cpus`,
  `memory-mb`, in `.grain/workflows/`).
- `inputs:` is a template's parameters (§6.1).
- `reads:` and `writes:` are the two lists this replaces. Splitting by
  mode puts one target in two places when a task needs a read of one
  folder and a write of another in the same repo, and leaves the target
  repo a third field.

#### Entries

| Field | Meaning |
| --- | --- |
| `ref` | what to materialise: a repo (a git URL or alias with no path), a folder (a path ending in `/`), or a file (an object or an asset, §2.2), with an optional `@ref` (§4) |
| `mode` | `read` (the default), `write` or `use`. **`write` is valid only for a repo or a repo folder**, and means the run may push **its own task branch** to that repo, based on the entry's `@ref`, and nothing else (below). Every other target is read-only. **`use` is valid only for a `kind: capability` object**, and requests that capability: nothing of the file lands, only what the capability materialises (§5.7) |
| `path` | optional: where it lands, relative to the sandbox's working root. Otherwise the default layout below |
| `depth` | optional, file entries only: also materialise the files this one's Markdown body links to, `n` links deep (below). The default is `0` |
| `input` | in a template only, in place of `ref`: take the reference from a template input of type `ref` (§6.1) |
| `reducers` | `use` entries only: reducers of the task's own, applied after the capability's chain, which can only narrow it further (§5.7) |

**Which ref an entry reads:**

- **A relative or `/`-rooted entry** (`attachments/x.png`,
  `/designs/a.md`) reads **the task's own commit**, the `spec_rev` it was
  placed at, as a relative root entry reads its `grain.config.md`'s
  (§3.3). So a
  task's attachments are exactly the ones that were approved.
- **An entry in another repo** reads its `@ref`. With no `@`, it reads the
  target repo's `default_ref`, or the remote's `HEAD` for a repo with no
  `.grainconfig`, at the moment the run is materialised. Either way the
  execution record notes the commit it used (below), as every unpinned
  reference is recorded pinned (§4).
- **A `write` entry must name a branch** (`@main`, `@candidate`). It is
  the base: the run's branch starts there, and its change targets it.
  A tag, a commit or no `@` is refused for `write` on push (§8.1).

**The task's branch.** Each run pushes to one branch,
`grain/<grain>/<run name>` (`grain/eu/2026-10-16-reject-dangling-refs`),
in every repo it has a `write` entry for. It may create it, push to it
and rewrite it. It may not push to any other ref: not the base, another
branch, a tag, or a deletion. A `write` folder narrows that further: the
push is refused if any of its commits changes a path outside the
folder's `write` entries (§11.22). **The primary repo** is the first
`write` entry. It is what `repo:` meant: the code repo whose `task_home`
applies (§5.5), the change `closed.change` records, the repo the hub
groups the task under. A task with no `write` entry is team-targeted
(§5.5) or read-only, such as a review or an investigation. A run with
several `write` entries records one change per repo, `closed.change` as
a list.

**Duplicates.** Two entries naming the same target at the same ref are
one entry, with the stronger mode and the larger `depth`. Two `write`
entries naming one repo at different branches are refused: a run has one
base per repo.

#### The sandbox layout

The sandbox's working root (`$HOME` in grain's sandboxes today) holds
exactly this:

```
task.md                                 the task as placed, at spec_rev: read-only
run.md                                  the execution record, kept current by the Grain: read-only
thread/<utc>-<author>.md                replies relayed to the running agent (§5.4), and their
thread/<utc>-<author>/<file name>         attachments: read-only
work/<repo name>/                       a repo entry, or the folder entries of one repo, at the resolved commit
context/<host>/<repo path>/<path>       a file entry, an object or an asset: read-only
```

- **`<repo name>`** is the last element of the repo's URL path without
  `.git` (`silo`, `grain`), as for a trigger's firings (§6.2). If two
  repos' names collide, **each** of them lands at `work/<host>/<repo
  path>/` instead, so the layout does not depend on the order of the
  entries. One repo at two refs, a `write` base and a `read` of an older
  tag say, gives the second ref its own checkout,
  `work/<repo name>@<ref>/`, with `/` in the ref written as `-`.
- **Folder entries** of one repo at one ref share that repo's checkout,
  as one sparse checkout of their union. A repo entry for the same repo
  and ref makes it a full checkout. A `write` checkout is writable; a
  `read` checkout is mounted read-only.
- **`<host>`** is the URL's host (with `_<port>` if it has one), and
  `local` for `file://`. **`<repo path>`** is the URL's path without the
  leading `/` and the `.git`: `context/silo.example.com/git/hub/shared/designs/hub-intake.md`.
- **A file inside a checkout** at the same commit is not copied again. Its
  place in the record is its path under `work/`.
- **An external artefact** (§2.2) lands at its sidecar's path with `.md`
  dropped, beside the sidecar, which is materialised too.
- **`path:`** is relative to the working root, must not contain `..`, and
  must not be or contain `task.md`, `run.md` or `thread/`, or overlap
  another entry's path. Silo's hook refuses a task that breaks these
  rules (§8.1), and the Grain checks them again.

The agent sees **only** these files. The Grain writes `run.md`, never
the agent. Its `context:` section is the record of what was materialised
and what was skipped, so it is also the agent's manifest:

```yaml
# in runs/<name>/run.md, written with the run's `running` commit
context:
  entries:
    - {ref: "https://github.com/bwsalmon/silo.git@main", mode: write, rev: <commit>, path: work/silo/, branch: grain/eu/2026-10-16-reject-dangling-refs}
    - {ref: "https://github.com/bwsalmon/grain.git//pkg/daemon/@candidate", mode: read, rev: <commit>, path: work/grain/}
    - {ref: "shared:designs/hub-intake.md", mode: read, rev: <commit>, path: "context/silo.example.com/git/hub/shared/designs/hub-intake.md", bytes: 3230}
    - {ref: "shared:designs/img/claims-flow.png", via: "shared:designs/hub-intake.md", mode: read, rev: <commit>, path: "context/silo.example.com/git/hub/shared/designs/img/claims-flow.png", bytes: 250}
    - {ref: "intake:tasks/2026-10-16-reject-dangling-refs/attachments/dangling-ref-error.png", mode: read, rev: <commit>, path: context/screenshot.png, bytes: 150}
  skipped:
    - {ref: "grain-code:pkg/daemon/", via: "shared:designs/hub-intake.md", reason: not a file}
  limits: {reached_files: 1, reached_bytes: 250}
```

#### Only `context:` materialises

Tasks hold many other references, and **none of them pulls anything into
a sandbox**: `depends_on`, `template`, `implements`, `supersedes`,
`trigger`, a backlog reference's `to`, `moved_from`, `filed_by`, a
comment's `run` or `task`. They are metadata. The Grain reads some of
them itself, outside the sandbox (a dependency's runs, §8.9; the
template, §6.1), and shows them in `task.md`, but the agent gets the
target only if `context:` lists it. A task that depends on another task
in another repo does not get that repo; a task that implements a
requirement does not get the design doc unless it lists it, which most
should.

**Transitivity is off by default.** A design doc in `context:` that links
to other docs brings only itself. With `depth: n`, the Grain also follows
the links in the Markdown body of each file it materialised for that
entry, `n` levels deep:

- **What counts as a link:** an inline link or image (`[t](x)`,
  `![t](x)`) or a reference-style definition, whose target is a
  relative or `/`-rooted path, a declared alias, or a git URL with the
  `//` separator. An ordinary web link (`https://example.com/page`) is not
  a reference, and is ignored. Front-matter fields are never followed:
  depth reaches what the text shows a reader, not the doc's metadata.
- **What is followed:** files only, objects or assets. A link to a
  folder or a repo is skipped and recorded: those are whole checkouts,
  and a task that wants one lists it. Only Markdown files have links, so
  an image ends its branch.
- **The commit:** a relative link reads the linking file's commit; a
  link with `@ref` reads that ref; an unpinned link into another repo
  reads its `default_ref`, as any reference does.
- **Order and caps:** breadth-first, in entry order and then document
  order, so the result is deterministic. Each reached file counts against
  `reached_files` and `reached_bytes` (below). Once a cap is reached, the
  rest are skipped and recorded, never silently dropped.

#### Limits

| Limit | Default | Over it |
| --- | --- | --- |
| `entries` | 64 listed entries | the task is refused (below) |
| `depth` | 3 | an entry's `depth` is clamped, and the clamp recorded |
| `reached_files` | 256 files reached through `depth` | the rest are skipped and recorded |
| `reached_bytes` | 64 MiB reached through `depth` | the rest are skipped and recorded |
| `file_bytes` | 32 MiB for one file from git, LFS content included | a listed file refuses the task; a reached one is skipped |
| `files_bytes` | 512 MiB for every file entry and reached file together | as `file_bytes` |
| `external_bytes` | 1 GiB for one external artefact, as its sidecar's `bytes` and as downloaded | as `file_bytes` |
| `checkout_bytes` | 8 GiB for one checkout, as inflated on disk | the task is refused |
| `hops` | 8 redirects per reference (§4) | dangling |

A Grain's local policy may lower any of these (`context_limits`), and
nothing can raise them. **A listed entry is never cut**: the task's
author asked for it, so a listed entry the Grain cannot materialise in
full refuses the task, with the entry named. **A reached file may be
skipped**: nobody asked for it by name, so the run goes ahead and the
record says what is missing.

#### The PEP checks before fetching

The Grain's local policy decides what a task may see, as it decides
today which repos a Grain serves, which capabilities it grants and which
network it allows (hub.md §1.2). **Every resolved entry is checked
before the Grain reads a byte of it**, including those reached through
`depth`, every hop of a redirect, and an external artefact's URL:

- a repo or folder against the policy's `repos:`, for its mode: `read`,
  or `read` and `write`;
- a file against `repos:` for `read`;
- an external artefact's host against the policy's `network`, and its
  store's credential against `capabilities` (§5.7);
- a `use` entry's whole capability chain, its effective authority and
  everything it implies (§5.7);
- LFS content against `repos:` and `network` for the LFS endpoint's host.

The Grain resolves outside the sandbox, with its own credential, into a
staging area. A file it must read to find links is fetched only after it
has passed. **Nothing enters the sandbox until the whole set has
passed**, and then the git proxy's scope is set to exactly that set.
This generalises grain task 4303's fix, which made the policy check the
repositories a capability implies: no repo, file or URL reaches a sandbox
unseen by policy. *Amended by 4456:* a capability may still imply a repo,
but **only by declaring it** in its base's `implies`, and the policy
checks each declared repo as it checks an entry, before anything is
materialised (§5.7). There is no hidden route. A task's
`repo-write:<repo>` capability becomes a `write` entry (§13), and a
workflow's becomes reducers over the git base (§5.7).

**Refusal fails closed, naming the entry.** A listed entry that the
policy refuses, that does not resolve, or that is over a limit, stops
the run before anything is fetched into the sandbox:

- **A placed task** gets an execution record in `refused`, with
  `refusal: {reason, at, entry}`, where `entry` names the entry by its
  index and canonical reference and gives the rule that refused it.
  Silo shows that as the task's waiting reason: "waiting: refused by
  `eu`: context entry 2, `https://github.com/gke-labs/granule.git` (read),
  is not in this Grain's `repos`". Silo may place the task on a Grain
  whose policy admits it (§8.8).
- **A task from the Grain's own lane** has nowhere else to go. Its record
  goes to `waiting`, with `waiting: {on: policy, entry, reason}`, until
  the owner widens the policy or cancels the task. The Grain re-checks
  the record whenever its policy changes.

A **reached** file the policy refuses is skipped and recorded with
`reason: policy`. It was never fetched, so this too fails closed.

#### Templates and triggers contribute entries

A template carries a `context:` list like a task's. Its entries may take
their reference from a **template input of type `ref`**, so one template
serves many repos:

```yaml
inputs:
  - name: repo
    type: ref
    ref_kind: repo                       # repo | folder | file: what the value must resolve to
    default: "https://github.com/bwsalmon/grain.git@candidate"
    label: The repo the change lands in, with its base branch
context:
  - {input: repo, mode: write}
  - {ref: "https://github.com/gke-labs/granule.git@main", mode: read}
```

- **The value is a reference, not text.** It is checked against §4's
  grammar and `ref_kind`, and never rendered by Jinja. Front matter is
  still never rendered (§6.1), so a template cannot compute an entry. An
  input of `type: list` with `of: ref` expands to one entry per item.
- **A relative value** resolves against the object that supplied it: the
  trigger, or the task for a task filed in Silo's UI. The API requires a
  URL or an alias of the intake's README.
- **A trigger** supplies the values in its `fires.inputs:` (and
  `fires.bind:`, from the event), and may add entries of its own in
  `fires.context:`, after the template's. `prune-packages`'s trigger
  sets `repo:` to grain at `candidate` (§8.16).
- **A filed task records the result.** Filing, by hand or by a trigger
  (planning included), writes the concrete `context:` into the task, every
  `input:` replaced by its value, and `template.inputs` records the
  values used. The task's `context:` is what the approver reads, what
  `spec_digest` covers, and what the Grain's policy checks. The template
  is never consulted again.
- **In a code repo's grain folder,** a template's or trigger's entries,
  and its `ref` inputs' defaults and values, may name only that repo.
  Readers refuse anything else, as they refuse `reads` and `writes`
  there today (§12.1). An adopting team that wants another repo
  supplies it from its own trigger.

#### One resolver, three consumers

Root entries (§3.3), references (§3.7) and `context:` use the **same
reference resolution**: canonicalisation, aliases from the repo's
`.grainconfig`, refs and pins, redirects (tombstones beside the old path,
repo `moved_to`) with the hop limit, the archive mapping, cycle
detection, and dangling handling (§4). They differ in what they build:

| | Root entries | References | `context:` |
| --- | --- | --- | --- |
| builds | a **Grain's active set**: what it runs, fires and gives its runs | one object, read when an operation needs it | a **sandbox's view**: what one run sees and may write |
| accepts | a folder (or one file), listed by its type's kind suffix | one object of the kinds the field expects | repos, folders, objects and assets, with a mode |
| transitive | never; each entry is one folder's subtree less `ignore:` and `archive/` | never: each reference is resolved by whatever needs it | off, unless an entry sets `depth` (at most 3), over body links only |
| on a bad target | the entry is unknown: nothing new starts from it | dangling: reported, never widened | a listed entry refuses the run; a reached file is skipped |
| gate | the reader's credential (§11.18) | the reader's credential | the reader's credential **and** the Grain's policy, before each fetch |
| written by | the Grain's owner, in `grain.config.md` | the referring object's writers | the task's author, frozen on approval |

So one package, `taskrepo`, has one `resolve` with a gate supplied by its
caller:

```
resolve(ref, from, cred, gate):                 # §4; shared by every consumer
    id   = canonicalise(ref, from)              # alias from from's .grainconfig, relative, /-rooted; keeps @ref
    seen = []
    loop at most LIMITS.hops times:
        if id in seen: return dangling("redirect loop")
        seen += id
        if not gate(id): return refused(id, gate.rule)   # before any read of id's repo
        if id.repo's .grainconfig has moved_to: id = id at moved_to; continue
        t = stat(id, at id.ref or id.repo's default_ref, cred)      # trees only (§3.10)
        if t is missing:
            if stat(id + ".moved.md") exists:  id = canonicalise(its to, from = id); continue
            if stat(archive_mapping(id)) exists: t = that                       # §3.8, not a hop
            else: return dangling(t.reason)
        if t is unreadable: return dangling(t.reason)
        return Target(id, commit = t.commit,
                      what = repo | folder | object(kind from the suffix) | asset)
    return dangling("more than 8 redirects")

# Root entries (§3.7): gate = always true; the credential is the gate.
# list_entry(...) calls resolve(e.path, grain.config.md, reader's credential, ALLOW)
# for the folder, then lists its trees.
# References (§3.7): the same, then a kind check against what the field expects.

materialise(task, grain):
    L     = min(CONTEXT_LIMITS, grain.policy.context_limits)
    gate  = lambda mode: lambda id: grain.policy.admits(id, mode)  # repos, network, capabilities
    if |task.context| > L.entries: return refuse(task, entry = None, "too many entries")
    plan, skipped, queue = [], [], []
    for n, e in enumerate(task.context):                       # listed entries: never cut
        T = resolve(e.ref, task @ spec_rev, grain.cred, gate(e.mode))
        if T is dangling or refused:     return refuse(task, entry = n, T.reason)
        if e.mode == write and (T.what not in (repo, folder) or e.ref's @ is not a branch):
                                         return refuse(task, entry = n, "write needs a repo or folder at a branch")
        if T.what in (object, asset) and size(T) > L.file_bytes:
                                         return refuse(task, entry = n, "over file_bytes")
        plan += (e, T)
        if e.depth: queue += (T, min(e.depth, L.depth), n)
    while queue:                                               # reached files: breadth-first
        (F, d, n) = pop front of queue
        if F is not Markdown: continue
        body = fetch(F into staging)                           # F has already passed the gate
        for link in markdown_links(body) in document order:
            R = resolve(link, F, grain.cred, gate(read))
            if R is not a reference:           continue        # a web link
            if R is dangling or refused:       skipped += (link, via F, R.reason); continue
            if R.what in (repo, folder):       skipped += (link, via F, "not a file"); continue
            if R in plan:                      continue
            if over L.reached_files, L.reached_bytes or L.file_bytes:
                                               skipped += (link, via F, "limit"); continue
            plan += (read entry for R, via F)
            if d > 1: queue += (R, d - 1, n)
    if total file bytes of plan > L.files_bytes: return refuse(task, entry = the first over it, "over files_bytes")
    # Every entry, listed or reached, has passed the gate. Only now:
    proxy.scope = {(T.repo, folders or all, commit, mode) for repos and folders in plan}
                  + push: refs/heads/grain/<grain>/<run> in each write repo, paths ⊆ its write folders
    for repos and folders: clone through the proxy (partial, sparse for folders); abort over L.checkout_bytes
    for files: copy from staging, read-only, executable bits cleared; external: download, verify sha256
    caps  = [effective_capability(e, task, grain) for e in task.context if e.mode == use]   # §5.7
    if any is refused:                   return refuse(task, entry = its index, its reason)
    # Repo and folder entries are uses of git@v1, and the proxy scope below is that capability.
    for c in caps: resolve its secrets from grain's store; missing → refuse(task, entry, secret)
    for c in caps: mint c's credentials, narrowed; load its use reducers into its proxies
    record run.context = {entries: plan with commits and paths, skipped, limits used}
    record run.capabilities = [chain with commits, digest, effective, implies, materialised, secret names]
```

`refuse` is the record of the PEP section above: `refused` for a placed
task, `waiting` for one from the Grain's own lane. Silo uses the same
resolver to show a task's context in its views and to check it on push,
with the pusher's rights as the gate (§8.1).

### 5.7 Capabilities: base capabilities, and objects that only narrow them

A **capability** is authority a run uses beyond its files: a cloud
credential, a model API, a throwaway GitHub repo, tools that read the
Grain's own logs. In grain today these are opaque names: `gcp-key`,
`gemini-key`, `github-sandbox`, `self-debug`, `grain-ci`,
`package-publish`, and `repo-read:`/`repo-write:` in workflows. Their
behaviour is in Go providers in bwsalmon/grain (`pkg/capability/...`,
`docs/ci-capabilities.md`), and what one *implies* is in code the local
policy cannot read. `model.CapabilityRepos` maps a name to the repos it
reaches, and nothing showed that to the policy until grain task 4303
found a repo reaching a sandbox by that route.

This section makes capabilities things the policy can read. There are
three layers, and a grant is a fourth step, which only the Grain takes:

| Layer | What it is | Who writes it | Where it lives |
| --- | --- | --- | --- |
| **base capability** | trusted provider code with a declared surface: the most it can grant, what it materialises, what it implies | the Grain's implementer | in the Grain's code. The Grain publishes a `kind: capability-base` declaration of it in its root |
| **capability object** | a reference to a base, plus an ordered chain of **reducers**, each of which only narrows | anyone, in any task repo; a Grain honours it only from a source its policy sanctions | `kind: capability`, usually in a team's or org's shared repo |
| **request** | a `context:` entry with `mode: use` | the task's author, frozen on approval | the task |
| **grant** | the Grain's decision to materialise the effective capability for one run | the Grain's local policy, after Silo's claim | the run's record |

**The core rule is monotonicity.** The effective authority of a
capability object is

```
effective(C) = maximum(base type, on this Grain) ∩ reducer₁ ∩ reducer₂ ∩ … ∩ reducerₙ
```

with the reducers of every object in its chain applied base-most first.
**A reducer can never add.** Nothing in a capability object, or in a
task, can name authority the base does not have.

**Macaroons are the reference model** (Birgisson, Politz, Erlingsson,
Taly, Vrable and Lentczner, *Macaroons: Cookies with Contextual Caveats
for Decentralized Authorization in the Cloud*, NDSS 2014). A macaroon is
a bearer credential to which any holder may append **caveats**, each of
which restricts it further. The verifier checks every caveat, and one
it cannot discharge means deny. This design borrows the same rules:
anyone may attenuate without asking the issuer, attenuation is
append-only and ordered, and an unknown restriction fails closed. It
differs in where the chain lives. A macaroon carries its caveats inside
the token, chained by HMAC so that none can be removed. Here the chain is
a set of files in git, named by path (§2.1). The Grain, which holds the
base and so plays the issuer's and the verifier's part, re-resolves the
chain itself. Integrity comes from the commit and the policy's
definition-source allowlist, not from a MAC. Where a provider's own
credentials support attenuation, credential reducers compile into it
(below).

#### Base capabilities

A **base type** has a name and a contract version, `gcp-service-account@v1`.
The type is the contract: its **dimensions** (the axes its authority is
measured on), the shape of what it materialises, and what it implies. A
Grain that implements a type publishes a declaration of **its**
implementation in its root, at `capabilities/bases/<type>.md`, written by
the Grain from its provider code
([`grain-eu/capabilities/bases/gcp-service-account.md`](../examples/task-repos/grain-eu/capabilities/bases/gcp-service-account.md),
schema [`capability-base.schema.json`](../examples/task-repos/schemas/capability-base.schema.json)):

```yaml
---
kind: capability-base
schema: grain/capability-base@v1
type: gcp-service-account@v1
grain: eu
provider: {code: "https://github.com/bwsalmon/grain.git//pkg/capability/gcp/", rev: <sha>}
dimensions:                                # each: kind, maximum on this Grain, where it can be narrowed
  accounts: {kind: set, maximum: ["grain-sb-agent-*@bsalmon-fun-sandbox.iam.gserviceaccount.com", "widgets-preview@acme-widgets.iam.gserviceaccount.com"], narrow: [credential], required: true}
  projects: {kind: set, maximum: [bsalmon-fun-sandbox, acme-widgets], narrow: [credential], required: true}
  roles: {kind: set, maximum: ["*"], narrow: [credential]}
  token_lifetime: {kind: quantity, unit: seconds, maximum: 3600, narrow: [credential]}
  calls: {kind: set, maximum: ["*"], narrow: [use]}
materialises:                              # everything it puts in a sandbox, and nothing else
  files: [{path: .gcp-credentials.json, holds: "a credential config naming eu's token endpoint; no key"}]
  env: {GOOGLE_APPLICATION_CREDENTIALS: .gcp-credentials.json}
implies:                                   # what using it reaches, checked by the policy
  egress: [oauth2.googleapis.com, sts.googleapis.com, "*.googleapis.com"]
  repos: []
  tools: []
proxies: [token-endpoint, google-api-proxy]
approval: none                             # none | required | local
---
```

- **`dimensions`** are what reducers narrow. A `set` narrows to a
  subset, a `quantity` to a lower bound, a `time` to an earlier end.
  `narrow` says where the Grain can enforce a reducer on that dimension:
  on the **credential** it mints, on each **use** at one of its proxies,
  or both. `required` means that if the dimension is narrowed to empty,
  the capability is empty and the request is refused.
- **`materialises`** lists every file, environment variable and MCP tool
  the base puts in a sandbox. Credential files are files in the
  sandbox's working root, as they are today (`~/.gcp-service-account.json`,
  `~/.github-sandbox-token`).
- **`implies`** is what a use reaches: **repos**, network **egress** and
  **tools**. It is either a fixed list, or `{from: <dimension>}` for a
  dimension's narrowed values. The git base's repos are its `repos`
  dimension, so narrowing the dimension narrows the implication. This is
  `model.CapabilityRepos` made a declared part of the contract.
- **`secrets`** lists the slots a base fills from the Grain's secret
  store (below), and **`approval`** says whether a human must approve
  its use (below).

**The declaration is published, never trusted by its own Grain.** The
Grain decides from its providers, and regenerates the file whenever
they change. Silo reads it to place a task only on Grains that implement
what it requests, and to show what a capability would be on each Grain.
Readers use it to compute the effective capability by hand. A Grain
never reads its own declaration, or anyone else's, to decide what to
grant, so a commit to the file cannot widen anything.

**A custom Grain** either implements a base type, with that type's
dimensions, materialisation and implications, or refuses tasks that
need it. Its waiting reason says so: "`gcp-service-account@v1` is not
implemented by this Grain". **The declared surface is the contract**:
a type's `v1` dimensions mean the same on every Grain, and only the
maximums differ. Its own extensions are a new type, or a `v2`.

The bases sketched so far are not exhaustive or final:

| Base type | Dimensions (maximum on the Grain) | Materialises | Implies | Narrowed on |
| --- | --- | --- | --- | --- |
| `gcp-service-account@v1` | `accounts`, `projects`, `roles`, `token_lifetime` (≤ 1h), `calls` | a credential config naming the Grain's token endpoint (no key), `GOOGLE_APPLICATION_CREDENTIALS` | egress to Google's token and API hosts | credential: which account is minted for (only one whose roles in the remaining projects are all within `roles`), and the token's lifetime; use: each API call at the Grain's Google API proxy |
| `git@v1` | `repos`, `folders`, `operations` (`fetch`, `push`), `push_refs`, `lifetime` | `~/.git-credentials` naming the git proxy, and `insteadOf` rules | `repos: {from: repos}`. Its only egress is the proxy | use: every fetch and push, at the git proxy. credential: a run-scoped proxy token |
| `model-api@v1` | `providers`, `models`, `tokens`, `spend` | the model proxy's address and a run token | egress to the model proxy only | credential: the token's budget; use: each request |
| `github-sandbox-repo@v1` | one fresh repo in the sandbox org, for the run's lifetime | `~/.github-sandbox-repo`, `~/.github-sandbox-token` | egress to `github.com` and `api.github.com`; that repo | credential: the token's scope and lifetime |
| `self-debug@v1` | `tools` (`read_grain_source`, `list_grain_source`, the Grain's logs) | those MCP tools | `repos: [https://github.com/bwsalmon/grain.git]` for fetch (task 3204's grant); `tools` | use: each tool call |

#### Capability objects

A capability object names a base and narrows it
([`hub-shared/capabilities/widgets-deployer.md`](../examples/task-repos/hub-shared/capabilities/widgets-deployer.md),
schema [`capability.schema.json`](../examples/task-repos/schemas/capability.schema.json)):

```yaml
---
kind: capability
schema: grain/capability@v1
title: Deploy widgets previews to Cloud Run (acme-widgets), 1-hour tokens
base:
  capability: "org-caps:gcp/acme-widgets.md@v1"   # or {type: gcp-service-account@v1}
reducers:                                         # applied in order, after the base's own chain
  - {language: builtin, id: roles, type: subset, dimension: roles, allow: [roles/run.developer, roles/iam.serviceAccountUser]}
  - {language: builtin, id: lifetime, type: limit, dimension: token_lifetime, max: 1h}
  - language: anselm
    id: preview-services-only
    "on": use
    version: "0"
    body: |
      <opaque Anselm source: not interpreted until Anselm is adopted>
secrets: {}                                       # slot: <secret name>, for bases that have slots
approval: {by: ["team:hub#approver"], scope: task}  # optional, and only ever adds a condition
---
```

- **`base`** is exactly one of `{type: <base type>}`, a base the Grain
  implements, or `{capability: <ref>}`, another capability object to
  narrow (below).
- **`reducers`** is an ordered list. Each has a `language`, `builtin`
  for the interim types or `anselm`, and may say `on: credential` or
  `on: use`. Front matter quotes `"on"`, for the reason given in §12.1.
  An `id` labels it in the audit log; it is always also named by its
  index.
- **`secrets`** binds the base's secret slots to names in the Grain's
  store (below). **`approval`** opts in to a human's approval (below).
- A capability object has **no field that grants**: no `maximum`, no
  `implies`, no `grants`. The schema refuses them.

**Reducers act on the credential, on each use, or both.**

- **On the credential**, where the provider can narrow what it mints:
  a shorter token lifetime, a narrower scope, a budget, a more limited
  account. This is applied once, at materialisation, and the sandbox
  only ever holds the narrower credential. Where a provider has native
  attenuation (macaroon-based tokens, or a cloud's downscoped
  credentials), the Grain compiles the reducer into it.
- **On each use**, for a capability exercised through one of the
  Grain's proxies: the git proxy, the model proxy, the Google API proxy,
  the token endpoint, the MCP tool gateway. The proxy evaluates the
  chain's `use` reducers on every request, allows or denies it, and logs
  the decision.
- **A reducer the base can enforce neither way is refused**, whichever
  language it is written in: a dimension the base does not declare, or
  `on: credential` for a dimension that narrows only on use. Ignoring a
  reducer would widen the capability, so the request fails closed and
  the reducer is named.

#### Anselm: the reducer language

The language reducers are written in is named **Anselm**, after the
artist Anselm Kiefer. **Its syntax and semantics are deliberately not
specified here.** The owner expects Anselm to come from another project
that is in flight now, and this design adopts it once it is ready.
Until then the design names it, reserves `language: anselm` in the
schema, and fixes only what any implementation must provide, so that
files written now stay valid when it arrives.

**What an Anselm implementation must provide:**

- **Inputs.**
  - *The capability being narrowed:* its base type and declared
    dimensions, the values those dimensions hold after every earlier
    reducer in the chain, and its implications.
  - *For an `on: use` reducer, the request:* the shape the base's proxy
    sees (for git: repo, operation, ref, paths; for a model: model and
    tokens; for an HTTP API: method, host, path and the call's name),
    plus the run's context (Grain, run, task, the request's time). Secret
    material is never an input.
  - *Its own body.*
- **Output.** For `on: credential`, a **narrowed capability**: for every
  dimension, a value within its input. For `on: use`, **allow or deny**,
  with a reason for the log.
- **Required properties:**
  1. **Monotonic narrowing only.** An output is never wider than its
     input. The Grain checks this on every result, and treats a result
     that is not within its input as a deny and a reducer bug, which it
     reports.
  2. **Deterministic.** The same inputs give the same output: no clock
     except the request's time, no randomness, nothing that depends on
     where or when it is evaluated.
  3. **Sandboxed, with no side effects.** No network, no filesystem, no
     state kept between evaluations, and no calls back into the Grain.
  4. **Bounded cost.** A size limit on the body, and an evaluation
     budget (steps, memory, time) the Grain sets. Running out is a deny.
  5. **Auditable.** Every decision is logged with the reducer that made
     it: the capability's canonical identity and commit, the link of the
     chain, the reducer's index and `id`, the decision, its reason, and
     a digest of its inputs.

  An evaluation error is a deny. A body declares the Anselm `version`
  it is written for, and a Grain that cannot evaluate that version
  refuses the request.

Examples in this design use a **neutral pseudo-notation**. It is
illustrative only, and is **not** Anselm syntax:

```
# illustrative pseudo-notation, NOT Anselm syntax
restrict projects to {acme-widgets}
restrict repos to those under https://github.com/acme/
token lifetime ≤ 1h
operations: fetch only, no push
on each use: allow only Cloud Run calls on services named preview-*, in europe-west1
```

**Before Anselm exists: built-in reducers, as an interim.** A small,
fixed set of reducer types with parameters, `language: builtin`, covers
the common narrowings. Each is monotone by construction, so the Grain
can apply it without a general evaluator:

| `type` | Parameters | Meaning | Enforced |
| --- | --- | --- | --- |
| `subset` | `dimension`, `allow: [<value or glob>]` | a set dimension keeps only what `allow` admits | wherever the base enforces that dimension |
| `limit` | `dimension`, `max` (a number in the dimension's unit, or a duration: `30m`, `1h`) | a quantity dimension becomes `min(current, max)` | as `subset` |
| `not_after` | `at` (a time) | the capability is valid until `at` at the latest | credential, and each use |
| `require_approval` | `by: [<principal>]`, `scope: task \| run` | adds a human's approval as a condition of use (below). It only makes the capability harder to use | at materialisation |

These are first-party caveats with fixed predicates, in macaroon
terms. They are flagged **interim** throughout: the schema accepts them
in `grain/capability@v1`, and nothing new is added to the set while
Anselm is pending.

**A `language: anselm` reducer written now** is valid, is carried, and is
covered by digests and approvals like any other. But a Grain that cannot
evaluate Anselm **refuses a request whose chain contains one**, naming
it: "capability entry 2, `shared:capabilities/widgets-deployer.md`,
reducer 3 (`preview-services-only`) is `language: anselm`, which `eu`
cannot evaluate yet". It **never skips it**, because skipping a reducer
widens the capability. Teams should not depend on Anselm reducers before
adoption. The example's placeholder is there to show that the files stay
valid.

**When Anselm arrives:**

1. **`builtin` stays valid.** Loaders keep every decoder they ever
   accepted (§10), so a `builtin` chain keeps working and nothing has to
   be rewritten.
2. **Each built-in type gets a defined Anselm equivalent,** shipped with
   the adoption. The meanings are fixed now (the table above), so each
   equivalent is checked against its built-in on the examples and a
   fixture set, as the Jinja subset is checked against Python (§6.1).
3. **A chain may mix languages.** `grain capabilities convert` may
   rewrite built-ins as Anselm in one reviewed commit, which changes each
   digest (below) and so needs any approval again. Converting is
   optional.
4. **Anselm bodies written before adoption** that do not parse under the
   adopted version fail closed: the request is refused, never widened.

#### Composition across repos

A capability object's base may be **another capability object**, so a
broad definition can be narrowed in layers, each kept by whoever owns
that layer:

- the org's `org-caps:gcp/acme-widgets.md` is `gcp-service-account@v1`
  with `subset projects [acme-widgets]`, released at tag `v1`;
- the hub team's `shared:capabilities/widgets-deployer.md` takes that
  object, pinned at `v1`, as its base, and narrows it to two roles,
  1-hour tokens and a per-use check;
- a task's `use` entry may narrow it once more, with `reducers:` of its
  own: 30-minute tokens, valid for a week.

**Each layer only tightens.** A team cannot get more than the org
defined, and a task cannot get more than the team defined. The chain is
resolved with the reference rules of §4 (path identity, aliases, git
URLs and `@ref`, `moved_to` and tombstones with the hop limit), plus
cycle detection and a depth limit:

```
CAPABILITY_LIMITS = {chain: 8, reducers: 32, task_reducers: 8}

effective_capability(entry, task, grain):           # entry: a context: entry with mode: use
    chain, seen, ref, from = [], [], entry.ref, task @ spec_rev
    loop:
        C = resolve(ref, from, grain.cred, gate = sanctioned_source)   # §4: redirects, hops, tombstones
        if C is dangling or refused:     return refuse(entry, C.reason)  # refused before C's repo is read
        if C is not a kind: capability:  return refuse(entry, "not a capability")
        if C.id in seen:                 return refuse(entry, "capability cycle: " + seen + C.id)
        seen += C.id; chain = [C] + chain              # base-most first
        if |chain| > CAPABILITY_LIMITS.chain: return refuse(entry, "chain longer than 8")
        if C.base.type: break
        ref, from = C.base.capability, C               # a relative base reads C's commit
    B = grain.providers[chain[0].base.type]            # the Grain's code, never a published file
    if B is missing:                     return refuse(entry, "base not implemented")
    if B.type not in grain.policy.capabilities.bases:  return refuse(entry, "base not allowed")
    for C in chain:
        if not grain.policy.sanctions(C.source, B.type):       return refuse(entry, link = C, "unsanctioned source")
        if C.secrets names one the source may not bind:        return refuse(entry, link = C, "secret not allowed")
    cap = B.maximum
    for r in reducers of chain (base-most first) + entry.reducers:
        if r.language is not evaluable here:           return refuse(entry, reducer = r, "cannot evaluate")
        if r is not enforceable by B (dimension, on):  return refuse(entry, reducer = r, "not enforceable")
        if r.on is use:  cap.use_checks += r; continue # evaluated by the proxy, per request
        next = apply(r, cap)
        if not (next ⊆ cap):                           return refuse(entry, reducer = r, "reducer widened")
        cap = next
        if a required dimension of cap is empty:       return refuse(entry, reducer = r, "empty")
    implied = B.implies(cap)                           # repos, egress, tools, as narrowed
    for x in implied: if not grain.policy.admits(x):   return refuse(entry, implication = x)
    approvals = B.approval + every approval and require_approval in the chain
    secrets   = B's slots, bound by the chain or by B's defaults
    digest    = sha256(canonical(B.type, B's declaration rev, [(C.id, C.commit) for C in chain], entry.reducers))
    return Effective(chain, cap, implied, approvals, secrets, digest)
```

- **Cycles** are detected over canonical identities, so a loop through an
  alias, a URL and a tombstone is still one loop. A cycle, or a chain
  over the limit, refuses the request. It is never truncated, because a
  truncated chain has lost reducers.
- **Pins.** Each link reads the commit its own reference names, as any
  reference does (§4). A team should pin the org object it builds on
  (`@v1`). Tracking it is safe, because each link still narrows, but
  every upstream change changes the digest.
- **Monotonic within one resolution, not over time.** The org can edit
  its object and drop a reducer, and that widens every object built on
  it. That is the org's right, as a template's author may edit a
  template. The **digest** is what holds an approved task to what was
  approved: Silo's grant and any approval name the digest, and a Grain
  that computes a different one refuses the claim (below; §11.28).

#### Catalog and grant

**A capability definition is reached only by reference, and referencing
one never grants it.** There is no capability root: a definition
changes what a run *may* be given, never whether anything runs, so by the
test of §3.1 it is a reference. A task names it in `context:` with
`mode: use`, and the Grain resolves the chain then. Silo's **catalog**,
the list it shows a task's author, is a view of the capability objects
in the repos the team's `.grainconfig` aliases and the sources Grains'
policies sanction; it is a convenience for authors, and nothing a Grain
reads.

**A task requests a capability in `context:`**, alongside its repos and
files (§5.6), with `mode: use`:

```yaml
context:
  - ref: "https://git.example.com/hub/widgets.git@main"
    mode: write
  - ref: "shared:capabilities/widgets-deployer.capability.md"
    mode: use                                     # request it; nothing of the file lands
    reducers:                                     # optional: the task's own, only narrowing
      - {language: builtin, type: limit, dimension: token_lifetime, max: 30m}
      - {language: builtin, type: not_after, at: "2026-10-24T00:00:00Z"}
```

- **`mode: use` is valid only for a file that resolves to a
  `kind: capability`.** A bare string is `mode: read`, so a capability
  object listed as a bare string is only a copy of its definition, with
  no authority: nothing becomes a grant by accident.
- A `use` entry takes no `path` or `depth`. What lands is what the base
  materialises, at the paths its declaration names.
- **One list, one check.** A task's grain-era `capabilities:` field is
  gone, as `reads` and `writes` went: each name becomes a `use` entry
  (§13.2). The entry is part of the spec, so the approver reads it, the
  hook freezes it, and `spec_digest` covers it (§5.1, §11.7).
- **Repo and folder entries are uses of the git base.** A `read` entry
  is `git@v1` narrowed to that repo or folder, its commit and
  `operations: [fetch]`. A `write` entry adds `push` and
  `push_refs: [refs/heads/grain/<grain>/<run>]`, with folders for a
  `write` folder. The proxy's scope in §5.6 is exactly that effective
  capability, so the `context:` resolver and the capability resolver are
  the same check.

**The Grain's local policy grants.** It allowlists **base types** and
**definition sources**: which repos and folders may define capability
objects it will honour, and for which bases. **This allowlist is the
whole control**: a capability object from anywhere else is refused,
whoever references it, so no Silo or team folder can introduce authority
the owner did not sanction:

```yaml
# Grain eu's local policy: its own configuration, not a task-repo file
capabilities:
  bases: [git@v1, gcp-service-account@v1, model-api@v1, self-debug@v1]
  sources:                                   # who may define capability objects eu honours
    - source: "https://silo.example.com/git/grains/eu.git//"           # its own root (always, implicitly)
      bases: "*"
    - source: "https://silo.example.com/git/org/capabilities.git//"
      bases: [gcp-service-account@v1]
    - source: "https://silo.example.com/git/hub/shared.git//capabilities/"
      bases: [gcp-service-account@v1, model-api@v1]
      secrets: []                            # the secret names objects from here may bind
  approval: {local: [github-sandbox-repo@v1]}  # bases only eu's owner may approve, per run
  anselm: none                               # no Anselm evaluator yet
repos: …                                     # §5.6, unchanged
network: {egress: [oauth2.googleapis.com, sts.googleapis.com, "*.googleapis.com", …]}
tools: …
```

**Every link of the chain must come from a sanctioned source, for that
base.** A sanctioned team object built on an unsanctioned base object is
refused. The base object is where the authority is defined, so a
sanctioned folder cannot launder a definition the owner never accepted.

**Before materialising anything, the policy checks the effective
capability and everything it implies:** the base type, every source in
the chain, every secret name, and each implied repo against `repos:`
(for the narrowed operations), each egress host against `network`, and
each tool against `tools`. This is the general form of what grain
approximates today with `model.CapabilityRepos`. A refusal fails closed,
before any credential is minted or file written, and names the entry,
the link of the chain, and the reducer or implication that decided, in
`refusal.entry` or `waiting.entry` (§5.6).

#### Approval

A capability can require a human's approval before it is used:

- **a base** whose declaration says `approval: required`. This is
  grain's *escalated capabilities* today, such as package publishing;
- **an object or a reducer** that opts in, with `approval: {by, scope}`
  or `type: require_approval`. Adding an approval only narrows: it can
  make a capability harder to use, never easier.

`by` names principals (`team:hub#approver`, `user:…`). `scope` is `task`
(once per task, the default) or `run` (before every run).

**How it meets Silo's approval flow:**

1. **The task's own approval may be enough.** If the task's `approved_by`
   is among the principals the capability's approval names, that one
   approval covers both, and Silo records it as such.
2. **Otherwise it is a second approval, after the task's.** A principal
   the capability names appends an entry to the task's
   **`capability_approvals`**: `{ref, digest, by, at}`. The intake's
   rules let only such a principal append it, with `by` bound to the
   pusher and never equal to the submitter, after the task is `approved`
   ([`hub-intake/GRAINOWNERS`](../examples/task-repos/hub-intake/GRAINOWNERS), and
   the hook's fixed rule that `by` is the pusher, §3.5).
   It is the approval, like `approved_by`, and not part of the frozen
   spec. Until it is there, Silo shows the task as "waiting: capability
   approval of `<ref>` by `<by>`", and does not place it.
3. **The claim travels in the attestation.** Silo's placement attestation
   carries `granted_capabilities: [{ref, digest, approved_by,
   approved_at}]` beside `approved_by` and `spec_digest` (hub.md §6.4).
   The Grain re-resolves the chain, recomputes the digest, and refuses
   the claim if they differ. An approval is of one resolved chain, and
   any change to any link is a new approval.
4. **`scope: run`, or a base the Grain's policy lists under
   `approval.local`, is approved by the Grain's owner only,** and
   before each run. Silo's claim never satisfies it. The run waits with
   `waiting: {on: human, entry}`, and the owner approves in the node UI
   or through Silo's relay of their reply. The Grain records
   `approved: {by, at, via: local}` in the run's `capabilities`.

What a Grain trusts here is §11.8's accepted risk: Silo's claim that an
approver approved. Signed commits are the upgrade for
`capability_approvals` as for `approved_by`, and `local` approval is the
owner's option when they would rather not trust it at all (§11.34).

#### Secrets never go in git

A capability object **names** a secret, and never holds it:

- **Slots and names.** A base declares slots (`secrets: [{slot: token,
  required: true}]`), and an object binds them, as in
  `secrets: {token: ghcr/publish-token}`. A name is `<secret>` or
  `<secret>/<key>`, grain's secret-store naming today (`github-app/app-id`).
  A base may give a slot a default name, so most objects bind nothing.
- **Resolved at materialisation,** from the materialising Grain's own
  store, into the file the base's declaration names, and nowhere else:
  not into git, `run.md`, events, logs or Silo. The run's record lists
  the names only. **Names are per Grain**: one object, run on two
  Grains, resolves to each Grain's own material.
- **Naming a secret is a source of authority** that the base's maximum
  does not bound, because the material can do whatever it can. So the
  policy allowlists the names each definition source may bind
  (`sources[].secrets`), and an object binding a name its source may not
  is refused.
- **A missing secret refuses, with a clear waiting reason.** Nothing is
  materialised in part. A placed task's run is `refused` with
  `refusal: {reason, entry, secret}`; one from the Grain's own lane goes
  to `waiting: {on: secret, entry, secret}`. Silo shows "waiting: `eu`
  has no secret `ghcr/publish-token` (slot `token` of
  `github-packages@v1`, capability entry 3)". The owner sets it, as
  grain's request-secret flow does today, and the Grain re-checks
  whenever its store changes.

#### CI

Grain CI jobs declare capabilities in workflow files today
(`capabilities: [gcp-key, repo-read:bwsalmon/grain]`). Workflows stay
YAML (§12.2), and the key keeps its shape. Each item is resolved by the
Grain into a capability object and checked as a `use` entry would be:

- **a reference to a capability object** (`"caps:gcp/ci-deployer.capability.md"`,
  with the alias from the repo's `.grainconfig`), which is new;
- **a legacy name** (`gcp-key`, `self-debug`, `github-sandbox`, …),
  which resolves to that name's legacy capability object (§13.2);
- **`repo-read:<owner/name>` and `repo-write:<owner/name>` become
  reducers over the base git capability.** They are shorthand, and the
  Grain expands them without anyone writing a file:
  - `repo-read:o/n` is `git@v1` with `subset repos [<forge URL of o/n>]`
    and `subset operations [fetch]`;
  - `repo-write:o/n` is the same with `operations [fetch, push]`, and
    `push_refs` as grain CI allows a job to push today.

  The job's own checkout is the same base, narrowed to the job's repo
  and commit.

**The trust rules of grain CI become policy over the effective
capability**, not lists of names. A scheduled run (an `on.schedule` trigger's) is untrusted: today it
gets no write-scoped capability (`repo-write:*`, `package-publish`,
`github-sandbox`) and no live provider secret, and a job that declares
one errors with "this deployment does not provide it". With objects, the
policy for untrusted runs refuses any effective capability that includes
`push`, binds a secret, or uses a base marked write-scoped. The same
names are refused for the same reason, and a new capability is covered
by what it can do, not by whether someone remembered to add its name to
a list. A workflow in a code repo names capability objects only from
sources the policy sanctions. Its `repo-read:` or `repo-write:` of
another repo is still the settings-owned grant it is today (§12.1).

#### What a run records

The run's `running` commit records each capability it materialised,
beside its `context:` (§5.6):

```yaml
capabilities:
  - entry: 2
    ref: "shared:capabilities/widgets-deployer.md"
    chain:                                          # base-most first, each at the commit used
      - {ref: "https://silo.example.com/git/org/capabilities.git//gcp/acme-widgets.md@v1", rev: <commit>}
      - {ref: "https://silo.example.com/git/hub/shared.git//capabilities/widgets-deployer.md", rev: <commit>}
    base: {type: gcp-service-account@v1, declaration: {ref: "capabilities/bases/gcp-service-account.md", rev: <commit>}}
    digest: "sha256:…"
    effective: {accounts: [widgets-preview@acme-widgets.iam.gserviceaccount.com], projects: [acme-widgets], roles: [roles/run.developer, roles/iam.serviceAccountUser], token_lifetime: 1800, not_after: "2026-10-24T00:00:00Z"}
    implies: {egress: [oauth2.googleapis.com, sts.googleapis.com, "*.googleapis.com"]}
    materialised: [.gcp-credentials.json]
    secrets: []                                     # names only
```

At close, the Grain adds each capability's count of per-use `denials`.
The decisions themselves are events in the Grain's audit log (runtime,
§1.2): every credential narrowing and every per-use decision, with the
reducer that made it.

## 6. Templates, triggers and gates


### 6.1 Templating: a strict Jinja2 subset

Template front matter declares its **inputs**. Each input has:

- `name`;
- `type`: `string`, `text`, `int`, `bool`, `enum`, `list` (with `of:`
  for its items' type) or `ref`, a reference with an optional `ref_kind`
  (`repo`, `folder` or `file`), which only a `context:` entry's `input:`
  may use and Jinja never renders (§5.6);
- an optional `default`, `options` (for `enum`) and `required`;
- an optional `label`.

The template body, and its `title`, are rendered with a **strict subset
of Jinja2**. The other front-matter fields are not rendered.

- **Allowed:**
  - `{{ expr }}`, with input names, literals, `.`/`[]` access into
    `list` inputs, comparison and boolean operators, and `~`;
  - the filters `default`, `lower`, `upper`, `trim`, `replace`, `join`,
    `indent`, `length`, `first`, `last`, `wordwrap` and `tojson`;
  - `{% if %}`/`{% elif %}`/`{% else %}`, `{% for %}` (with `loop.index`,
    `loop.first` and `loop.last`) and `{% set %}`;
  - `{% include "<ref>" %}`, and `{% import "<ref>" as m %}` with
    `{% macro %}`. An included or imported file is itself a
    `kind: template` file. Its inputs must be bound by a surrounding
    `{% with name=… %}` block or by macro arguments, and are checked
    like any template's;
  - `{% with %}`.
- **Refused:**
  - `extends`/`block`;
  - method calls (`.items()`, `.format()`);
  - `namespace`, `cycler`, `range` above 1000, and `self`;
  - any name that is not a declared input or a loop/`set` variable;
  - autoescape (output is Markdown, not HTML).
- **Strict.** An undefined variable is an error, and so is a missing
  required input or an `enum` value outside its `options`. Rendering is
  capped at 256 KiB of output and 10,000 loop iterations. Nothing beyond
  the declared inputs is reachable: no environment, no clock, no
  filesystem, and no network except includes, which are resolved by
  reference under the reader's own credentials (§11.4).

The engine is **gonja v2** (Go), behind a parser pass that refuses
everything outside the subset before rendering. The subset is pinned by
fixture tests in `testdata/jinja/`: each fixture is a template, its
inputs as JSON, and the expected output. CI renders every fixture with
gonja and with **Python Jinja2** (`StrictUndefined`,
`keep_trailing_newline=True`, autoescape off) and requires identical
bytes. A construct the two render differently is removed from the subset,
not special-cased.

Today's `{{name}}` placeholders (`pkg/repodecl`) are already valid Jinja.
A trigger's variables (`{{group}}`, `{{from_sha}}`) become declared
inputs.

### 6.2 Triggers (`kind: trigger`)

A **trigger** says "when X happens, file Y". It is the one kind for
standing work: a cadence is one of its event sources, not a kind of its
own. Its front matter has three parts: **`on:`**, the event sources, each
with its own options nested under it, in the shape of a GitHub Actions
workflow's `on:` (which grain's `.grain/workflows` already follow);
**`fires:`**, what it files; and who fires it and how firings are
approved. Its body is optional notes. Schema:
[`trigger.schema.json`](../examples/task-repos/schemas/trigger.schema.json).

```yaml
---
kind: trigger
schema: grain/trigger@v1
title: Prune GHCR packages
"on":                                  # quoted: YAML 1.1 reads a bare on as true (§12.1)
  schedule:
    weekly: {day: sunday, at: "04:00"} # or daily: {at}, hourly, every, monthly, cron
    timezone: ""                       # IANA; empty is UTC
    overlap: skip                      # skip | queue | cancel-previous
    catch_up: {mode: latest-only}      # none | latest-only | all (with max, ≤ 24)
  manual: {}                           # "run now"; who defaults to the team's approvers
fires:
  template: ../templates/ghcr/prune-packages.md
  inputs: {repo: "https://github.com/bwsalmon/grain.git@candidate"}
team: hub
fires_in: silo                         # silo | grain
auto_approve_proposals: true
enabled: true
---
```

This is [`hub-shared/triggers/prune-packages.md`](../examples/task-repos/hub-shared/triggers/prune-packages.md).

**Event sources (`on:`).** A trigger names one or more. Each fires the
same `fires:`; the task records which one did (below).

| Source | Fires when | Its options | Event fields a firing may bind |
| --- | --- | --- | --- |
| `schedule:` | the cadence comes round | exactly one cadence key, as grain writes them today (`daily: {at}`, `weekly: {day, at}`, `hourly`, `every`, `monthly`, `cron`); `timezone`; `overlap`; `catch_up` | `fired_at` |
| `push:` | a matching branch moves, by any push | `branches` (default: the repo's protected refs, and only protected refs may be named), `paths` and `paths_ignore` (Actions' globs), `group_by` (`none`, `folder` or `file`: one firing per changed folder or file, grain's `{{group}}`), `debounce` | `repo`, `branch`, `from_sha`, `to_sha`, `paths`, `group`, `pusher` |
| `merge:` | a reviewed change lands on a matching branch: a merge-queue merge, a merged review branch (§8.11), a submitted CL | as `push:` | as `push:`, plus `change` (its URL) and `merged_by` |
| `design-change:` | a design's requirements change on its `default_ref`: the planner of §7.4 | `designs` (files, or folders ending in `/`), `classes` (`new`, `changed`, `removed`; default all), `debounce`, `max_tasks_per_change` | as `merge:`, plus `design`; with `per: requirement`, also `requirement`, `class`, `action`, `hash` |
| `manual:` | someone asks for it now, in Silo's UI, the API or the CLI (`silo trigger run`, `grain trigger run`) | `who` (default: the team's approvers, or the Grain's owner), `inputs` (the template inputs the requester may set) | `requested_by`, and the inputs they set |

`overlap` reads the previous firing's derived status (§5.1). While that
firing is unplaced, queued or has an open run, `skip` files nothing;
`queue` files the new one with `depends_on` the previous, so it waits;
`cancel-previous` files the new one and cancels the previous (§8.7). The
default is `skip`. `catch_up` says what a trigger does about firings it
missed while nothing could fire it (Silo or the Grain was down): `none`
fires nothing late, `latest-only` fires the most recent one, and `all`
fires each, oldest first, but never more than `max` (default 5, at most
24), and never one older than `within` if that is set. A firing that
catches up records the time it was due, not the time it ran, so its path
is the one an on-time firing would have taken. `debounce` coalesces
events that close together into one firing, with the first event's
`from_sha` and the last one's `to_sha`.

**`task-closed:` is not included now,** and is reserved: v1 readers
refuse a trigger that names it. Chaining known work is already
`depends_on` (§8.9), which needs no trigger. A trigger on a task closing
is a loop waiting to happen (a trigger whose own firing's close fires it
again, possibly across Grains), and needs cycle detection across repos
and a story for whose close counts before it is worth having. It can be
added in a later minor version of `grain/trigger` without changing
anything else here.

**What it fires (`fires:`)** is exactly one of:

- **a template plus inputs**: `template:` (a reference, unpinned, so a
  firing renders the template's current revision and records it in
  `template.rev`), `inputs:` (literal values), `bind:` (template input
  name → event field, such as `from_sha: event.from_sha`; data, not Jinja,
  because front matter is never rendered, §6.1), and `context:` entries
  added after the template's (§5.6). A firing files one task, which lands
  where the firing's home says (below). A trigger's template inputs are
  the template's declared inputs, so grain's trigger variables
  (`{{group}}`, `{{from_sha}}`) are inputs bound from the event;
- **a workflow run followed by a review task**: `workflow:` (a
  `.grain/workflows/*.yml` file in the trigger's own repo, by plain path,
  §12.2), `branch:` (where it runs; default `default_ref`) and an optional
  `review: {template, inputs}`. The Grain runs the workflow, as a
  workflow's `on.schedule` runs in grain today (bwsalmon/grain
  `docs/declarations.md`, workflow schedules), and when the run finishes
  files a review task carrying its result, from `review.template` or
  grain's built-in workflow-review template. Only a Grain runs CI, so a
  workflow trigger must say `fires_in: grain`. It is
  [`widgets-code/.grain/triggers/nightly-ci.md`](../examples/task-repos/widgets-code/.grain/triggers/nightly-ci.md).

A workflow's own `on.schedule` stays in its YAML, where Actions reads it,
and grain still runs it as today: it behaves as an implicit trigger of
the second form, firing on that repo's Grain. A repo that wants to
change how one behaves (its overlap, its review template) moves the
cadence out of the workflow into a trigger.

**A trigger never holds a merge.** It has no `gate`, `human`,
`sensitive` or `requires` key, and its schema refuses them. Holding a
change for review is a gate (§6.3).

**A firing creates a task and a reference to it**, at a path derived from
the trigger and the event, so a retried firing writes nothing new:
`tasks/fired/<trigger>/<key>/task.md`. The key is the event's identity:

| Source | Key |
| --- | --- |
| `schedule:` | the time it was due, `yyyymmddThhmmZ` |
| `push:`, `merge:` | the first 12 hex of `to_sha`, plus `-<group slug>` with `group_by` |
| `design-change:` | `<design name>/<to_sha, 12 hex>`; with `per: requirement`, `<design name>/<HS-6>-<action>-<first 8 hex of the hash>` (§7.4) |
| `manual:` | the request time, `yyyymmddThhmmssZ`, plus `-<requester slug>` |

The task records `trigger: {ref, on, key, fired_at, event}`: the
trigger, which source fired, the key, when, and the event fields it
bound. `submitted_via: trigger`.

- **A `fires_in: grain` trigger in one of a Grain's trigger roots fires
  on that Grain** (§3.3), locally: the Grain renders the task into its own
  root, adds a reference to it in its `backlog/fired/` folder (§5.2), and
  runs it, so it never needs Silo. That covers the Grain's own
  `triggers/` and every other folder its `triggers:` entries name, less
  each entry's `ignore:` and any `archive/`. A trigger from another repo
  fires at `tasks/fired/<repo name>/<trigger>/<key>.task.md` in the root,
  so two repos' triggers of one name never collide. The Grain watches the
  sources itself: its clock, and the repos it fetches for `push:`,
  `merge:` and `design-change:`.
- **A team trigger is assigned to a Grain through Silo's folder for it.**
  Silo writes the trigger, or a copy of the team's, into
  `placements:grains/<grain>/triggers/`, and it fires on that Grain only
  if the Grain's owner has named that folder as a trigger root (§8.12).
  Silo assigns each `fires_in: grain` team trigger to one Grain, and
  moves the file to re-assign it. A Grain that wants every team trigger,
  unassigned, names the team's `shared:triggers/` folder itself instead
  (§8.15).
- **A `fires_in: silo` trigger fires in Silo** for the team it names, if
  it is in one of **the team's trigger roots**: the intake's own
  `grain.config.md`, which says `team: hub` where a Grain's says `grain:`,
  and may hold only `triggers:` entries (the intake's `backlog/` is the
  team's queue, and Silo has no memory). Silo renders the task into the
  team's intake, approved if the trigger's approval policy allows it, and
  places it by adding a placement (§8.12). If no Grain can take it now,
  Silo adds a reference to the intake's backlog instead, and the task
  waits there in the team's order. Silo watches the repos it hosts on
  receive, and code repos by webhook or on its next pass (§8.13). A Grain
  whose trigger root lists a `fires_in: silo` trigger shows it, and never
  fires it.

**Approval of a firing.** A firing in a Grain's root is the Grain's own
task. It is filed `approved` (with `approved_by: grain:<id>`) only if the
trigger says `auto_approve_proposals` **and** the trigger is in the
root's own repo, or reached through an entry that is pinned
(`ref_mode: pin`), so that what fires is exactly what the owner reviewed.
Through a tracking entry it is filed `proposed`, and the Grain's owner
approves it, or the Grain's local policy does. Silo files `approved_by:
silo:trigger` under the same rule, for the team's trigger roots. Whatever
the trigger says, a firing **caused by someone** is approved no further
than they could approve: a `push:` or `merge:` firing whose pusher or
merger is not an approver of the firing's team, and a `manual:` firing
requested by one who is not, are filed `proposed`. `design-change:`
firings keep §7.4's rule.

**Runtime state is not committed.** The last and next fire times, the
debounce windows, the catch-up cursor, whether a trigger is **paused**
(an operator's switch, in Silo's or the Grain's UI) or **suspended** (by
Silo or the Grain after repeated failures, or by the rate limits of
§11.36), and the run history all live in Silo's or the Grain's
database, like other runtime state (§1.2). `enabled:` is the definition's
own reviewed switch, and stays in git. The task each firing produces
records `trigger.fired_at`, so the history of what fired is in git
anyway, as tasks.

**Customising a team's trigger** is an `ignore:` plus a new object.
Grain `us` names the team's `shared:triggers/` as a trigger root, and
wants the daily `granule-pull.trigger.md` weekly. It does not edit or
override it: its entry says `ignore: [granule-pull.trigger.md]`, and it
adds its own `triggers/granule-pull-weekly.trigger.md`, a whole,
self-contained trigger that references the same template, whose
`derived_from` names the original. `derived_from` is a reference: it
activates nothing and inherits nothing, and Silo's views use it to show
the two diverging when the team changes the original (§8.15).

**Retiring a trigger** is the producer's move: the team moves it to
`triggers/archive/`, and it stops firing everywhere that names
`triggers/`, without any Grain changing its config (§3.8, §8.15).

**Triggers in code repos.** A trigger's *definition* may live in a code
repo's grain folder, beside the code it is about
(`widgets:.grain/triggers/weekly-release-notes.trigger.md`). Its *instances*
never do:

- **In Silo**, a trigger that Silo does not host fires for a team only
  if that team **adopts** it, with an entry for it in its intake's
  `grain.config.md` `triggers:` (written by the team's admins). A code repo cannot make a team's intake receive firings
  by naming the team. The instances go into the adopting team's intake,
  at `tasks/fired/<repo name>/<trigger>/<key>.task.md`, where
  `<repo name>` is the last element of the code repo's URL without
  `.git`. So two code repos' triggers of the same name never collide.
  A firing is filed `approved` only if the trigger says
  `auto_approve_proposals` **and** the adopting entry is **pinned** to
  a commit the admins reviewed, at which the template it names also
  resolves (a relative template reference reads that same commit).
  Otherwise it is filed `proposed` (§11.13). A pin replaces 4450's
  `auto_approve: {digest}`: it fixes the same bytes, and taking a change
  is re-pinning, one line.
- **On a Grain** whose trigger root names a code repo's trigger, a
  firing goes into the Grain's own root as above, or is an **inline
  firing**: a backlog reference whose `to:` is the trigger, with
  `fired_at` (and the key), and then the execution record. No task file
  is written, because the task is exactly the trigger rendered for that
  event. `spec_rev` pins the trigger's commit, and with it the
  template's, so an inline firing is allowed only when the template is in
  the same repo as the trigger, and only for a firing that binds no event
  fields beyond `fired_at`.

**Why trigger-fired instances are kept out of code repos.** A daily
trigger fires 365 times a year, and a push trigger as often as the repo
moves. In a code repo, each firing would be:

- a commit in the code's history, showing up in `git log`, blame and
  bisect;
- a change through the repo's review and CI, and, in a repo whose
  `.grain/**` is gated for a human (§6.3), a human's Submit every time;
- a review of nothing, because an instance is the trigger plus an event,
  rendered deterministically.

What deserves review is the definition, which changes rarely, so that is
what lives with the code. `task_home` never applies to a firing, Silo
never promotes one, and readers reject a `kind: task` with `trigger:` in
a code repo's grain folder (§5.5).

### 6.3 Gates (`kind: gate`)

A **gate** holds a change on its way into a repo until what it requires
is done. It is policy about merges, and **never fires work**, except the
review tasks it requires. Grain's `.grain/triggers/*.yml` hold both
kinds today, told apart by `gate: true`. This design splits them: firing
work and holding a merge are different things, with different authors,
different failure modes and different security properties, and a gate
must not be weakened by the machinery that customises triggers. Schema:
[`gate.schema.json`](../examples/task-repos/schemas/gate.schema.json).

```yaml
---
kind: gate
schema: grain/gate@v1
title: Repository declarations
match:
  paths: [".grain/**"]               # repo-relative globs; paths_ignore too
  branches: [main]                   # optional; default the protected refs
requires:
  security_review: {}                # a security review task; template optional
  human:
    when: always                     # always | on-findings
    from: ["team:hub#approver"]      # named approvers; count defaults to 1
  second_review: {}                  # optional: an independent second review task
sensitive: true
---
```

This is [`widgets-code/.grain/gates/grain-declarations.gate.md`](../examples/task-repos/widgets-code/.grain/gates/grain-declarations.gate.md)
(without `branches` and `second_review`), converted from bwsalmon/silo's own
`.grain/triggers/grain-declarations.yml`.

- **`match`** says which changes it holds: `paths` and `paths_ignore`,
  repo-relative, and `branches`, the target branches. **A gate belongs to
  the repo being merged into**, and governs only that repo. A gate in a
  mandatory gate source (below) also names the repos it governs in
  `match.repos`.
- **`requires`** lists what must be done, and each key only adds:
  - `human`: a Submit from a human approver. `when: always` is grain's
    `human: required`; `when: on-findings` is grain's `human:
    on-findings`, a human only if a required review found something.
    `from` names the approvers, by principal (§3.5), and defaults to the
    repo's own (its forge's reviewers, or the owning team's approvers in
    Silo); `count` defaults to 1. The change's author, its task's
    submitter, and the agent and Grain that made it never count;
  - `security_review`: a security review task, filed on every matching
    change, from `template` or grain's built-in;
  - `second_review`: an independent review task, run by a different
    Grain or model than the one that made the change.
- **`sensitive: true`** says the match is security-sensitive: auto-merge
  is withdrawn for the change, a security review is required whether or
  not `requires` names one, and the change and the run that made it are
  labelled sensitive in Silo's views. `when: on-findings` without a
  review to find anything is refused by the schema.

**Every gate that matches must be satisfied.** Gates combine by
conjunction. There is no negative gate, no `requires: none`, and no
key that releases another gate's hold, so adding a gate, from anywhere,
can only hold more. **A gate is read at the target branch's tip, never
from the change it holds**, so a change cannot weaken or delete the gate
that is holding it. A change to a gate file is held by the gates that
match it, and a repo's `.grain/**` gate matches its own gates.

**The review tasks a gate files** are the gate's only work. Each is
filed once per revision of the change (its head commit), gate and
requirement, at
`tasks/gated/<repo name>/<change id>-<head, 12 hex>-<gate name>-<requirement>.task.md`
in whichever root or intake runs the merge queue (below). Re-queuing the
same revision files nothing new; a new revision is reviewed again, and a
human's Submit counts only for the revision it was given on. Like a
trigger's firings, these tasks are never committed to the code repo
(§5.5).

**How gates apply in each task home** (§5.5):

- **In a code repo**, through grain's merge queue. For a change into
  repo R at branch B, the queue lists R's own gates (the `*.gate.md`
  files under R's grain folder, `.grain/` unless R's `.grainconfig` says
  otherwise, at B's tip, by the same tree-only listing as a root entry,
  §3.7) and any gate from the landing Grain's mandatory gate sources
  (below) whose `match.repos` covers R. It matches each against the change's paths,
  files the review tasks each requires (into the Grain's own root, as the
  Grain's work), withdraws auto-merge if any is sensitive, and holds the
  change until every requirement is met. The forge's own branch
  protection still applies; gates add to it, never replace it. This is
  how a task homed in a code repo (`code-repo` or `promote`) lands: its
  file is a `.grain/**` change, held by the repo's declarations gate
  (§5.5, §8.14). A forge that lands a change outside grain's merge queue
  bypasses gates, as it bypasses path triggers today; a repo that relies
  on them also requires grain's merge-queue check in its forge's branch
  protection.
- **In a Silo-hosted repo** (an intake, a shared repo, a Grain root),
  through Silo's pre-receive hook and Silo's merge queue. The hook
  refuses a push to a protected ref that touches a path a gate matches,
  unless it is a merge Silo's merge queue made: a review branch's merge
  (§8.11) that records, in the merge commit's trailers, the review tasks'
  outcomes and each human's Submit, which the hook checks against the
  gate at the ref's current tip. Silo files the required review tasks
  into the owning team's intake, `approved`, as Silo's own
  (`submitted_via: gate`). The gates that apply are the repo's own, in
  its grain folder (`.grain/gates/` by convention in a Silo repo too),
  plus Silo's mandatory sources (below).
- **In an intake, Silo's field rules come first.** A proposal, an
  approval or a cancel is checked against the intake's `GRAINOWNERS` and
  the hook's fixed rules (§3.5), and is not a gated change unless a gate
  matches its path. An intake that wants, say, every change to
  `GRAINOWNERS` reviewed says so with a gate.

**Gates are not Grain roots.** There is no `gates:` entry in
`grain.config.md`, and a Grain never activates a gate by naming a
folder. A gate is policy of the repo being merged into, so the merge
queue finds it there, at the target branch's tip, when that repo's
change is queued. Three things follow:

- **A team's `triggers/` can never hold a Grain's merges.** A trigger root
  lists `*.trigger.md` only; a `*.gate.md` beside them is not listed.
- **No Grain can switch off a repo's gate.** Its `ignore:` patterns apply
  to its own root entries, and gates are in none of them.
- **A Grain cannot add a gate to someone else's repo** by naming a
  folder. It adds one by proposing it to that repo, through that repo's
  review, or through a mandatory source (below) for the repos its own
  merge queue lands.

**Mandatory gate sources, for rules enforced across repos.** A Grain's
local policy may name **mandatory gate sources**
(`gates.mandatory: ["org-gates:gates/@main"]`), as it names sanctioned
capability sources (§5.7). The merge queue reads those itself, with the
Grain's credential, and applies each gate whose `match.repos` covers the
repo being merged into. Nothing in any task repo removes them, and a
mandatory source the queue cannot read holds every change it might cover
(unknown is not empty, §3.7). Silo's own policy may name mandatory gate
sources for every repo it hosts in the same way.

## 7. Design docs (`kind: design`)

```yaml
---
kind: design
schema: grain/design@v1
title: Hub task intake, approval and claims
status: accepted            # draft | proposed | accepted | implementing | implemented | superseded | withdrawn
owners: [user:bsalmon@google.com]
applies_to: ["silo-code:/", "grain-code:pkg/daemon/hub.go"]
supersedes: []
requirements: {prefix: HS, next: 6, retired: [HS-4]}
planning: {team: hub, max_tasks_per_change: 10, approval: required}
---
```

### 7.1 Requirements and their anchors

A requirement is a heading ending in `{#<PREFIX>-<n>}`, which
generalises grain's `{#D-7}` and the `HS-n` and `SV-n` series. Its
**section** runs from the heading to the next heading of the same or a
higher level.

**IDs are never reused.** The allocation rule:

- `requirements.next` only ever increases;
- a new requirement takes `next`, and `next` is bumped in the same
  commit;
- a removed requirement's ID goes to `retired`.

Silo's hook enforces this on receive. It refuses a push that:

- decreases `next`;
- reuses a retired or higher-numbered ID;
- uses one ID twice in a doc;
- changes another doc's prefix.

A heading with no ID is allowed in a draft. Planning assigns it the next
ID in a Silo commit, as the planner's own pull request does today.

### 7.2 Requirement revisions

A requirement's **revision** is the pair (design commit sha, section
hash). The **hash** is SHA-256 of the section text with three things
normalised away:

- the `{#ID}` marker;
- trailing whitespace;
- runs of blank lines.

So a reflow or ID assignment does not count as a change.

**What a task and its runs record:**

- **When the task is filed:** `implements: [{ref, rev, hash}]`.
- **When a run closes:** the Grain copies the revisions it worked
  against into its execution record's `closed.design_revs`. Commits carry them in the existing trailers,
  unchanged:

  ```
  Design-Item: <design ref>#HS-6      (the in-repo path for a doc in the code repo,
                                       e.g. docs/design/foo.md#D-7; the alias or URL form otherwise)
  Design-Rev: <design commit sha>
  ```

  `Design-Removed:` keeps its meaning.

### 7.3 Traceability both ways

- **From a task to its requirements:** read `implements:`.
- **From a requirement to its tasks:** Silo keeps an index from every
  `implements` edge in every repo it hosts. It is rebuilt from git on
  start and updated on each received push.

For each requirement, the index shows its tasks, their derived status
(from their execution records, §5.1), and **changed since implemented**.
A requirement counts as changed when its current hash differs from the
hash in the `closed.design_revs` of the task's `done` run. Designs outside Silo, in a code repo or on GitHub,
are indexed the same way when a Silo repo references them and Silo can
read them.

### 7.4 Planning is a trigger on design changes

Task 4446 made planning a Silo operation. It is now a **trigger**: a
`kind: trigger` with `on: design-change` (§6.2), authored, reviewed,
named in trigger roots, ignored and customised like any other. What used to be the
planner is two things: the **`design-change` event source**, which
detects and classifies requirement changes, and the trigger's
**`fires:`**, which says what to file for them.

```yaml
---
kind: trigger
schema: grain/trigger@v1
title: Plan design-doc changes
"on":
  design-change:
    designs: ["../designs/"]
    classes: [new, changed, removed]
    debounce: 5m
    max_tasks_per_change: 10
fires:
  template: ../templates/design-plan.md
  per: event                           # or requirement: the table below
  inputs: {mode: plan}
  bind: {group: event.group, paths: event.paths, from_sha: event.from_sha, to_sha: event.to_sha, branch: event.branch}
team: hub
fires_in: silo
---
```

This is [`hub-shared/triggers/plan-designs.md`](../examples/task-repos/hub-shared/triggers/plan-designs.md).

**The event.** When a push to a watched design's `default_ref` is
accepted (in Silo, on receive; for a code repo, when Silo or the Grain
reads it), the source diffs every requirement between the old and new
tips. It compares sections by ID and uses the hashes of §7.2. A push that
changes no requirement's hash is no event. One that does is an event per
design, carrying the range, the paths and each requirement's class.

**What fires.** `per: event` files one task for the event: an agent
planner, rendered from a template with the event's fields bound to its
inputs, as bwsalmon/grain's in-repo planner trigger
(`.grain/triggers/design-docs.yml`) does today. `per: requirement` files
the tasks directly, by the planner's existing table (bwsalmon/grain
`docs/design-docs.md`), one firing per row, with `requirement`, `class`,
`action` and `hash` bound:

| Class | Action (files go to the trigger's team's intake) |
| --- | --- |
| new | an implementation task plus a test task that depends on it, `state: proposed` |
| changed, task still open | a proposal superseding the open task (`supersedes: <ref>`). Approving it makes Silo cancel the old task, which its Grain honours (§8.7) |
| changed, done | a delta task covering only the changed text |
| removed, open | a proposal to cancel the open tasks. Nothing is cancelled without approval |
| removed, done | a removal task, whose commit carries `Design-Removed:` |
| unchanged | nothing |

**Idempotency.** Each firing's key is derived from the event (§6.2):
`tasks/fired/<trigger>/<design name>/<to_sha, 12 hex>/task.md` for
`per: event`, and
`tasks/fired/<trigger>/<design name>/<HS-6>-<action>-<first 8 hex of the hash>/task.md`
for `per: requirement`. Re-running on the same push finds every path
already there, and writes nothing new.

**A design opts in with `planning:`.** The design's `planning: {team,
max_tasks_per_change, approval}` says it wants planning, for which team,
within what limits. A `fires_in: silo` design-change trigger fires only
for a design whose `planning.team` is its own team; a `fires_in: grain`
one (an in-repo planner, §12.3) never fires for a design that has
`planning:`, which is Silo's to plan. So one design is planned by one
trigger, and doing both is refused, as before. Where the design and the
trigger both set a limit, the lower one applies.

**Flood control** is the trigger's, and can only be lowered:

- At most `max_tasks_per_change` tasks are filed per event (default 10,
  at most 50). A larger plan files **one** plan proposal listing every
  intended task. Approving it files them.
- Planning is capped per team per day (default 50), across every
  design-change trigger firing for the team (§11.36).
- `debounce` coalesces a run of merges into one event.
- Planned tasks are always `proposed`, unless the design says
  `approval: inherit` **and** the pusher whose change was merged is an
  approver for the planning team. `auto_approve_proposals` on a
  design-change trigger is refused.
- A design change pushed by a non-approver lands only through a review
  branch an approver merged (§8.11).

## 8. Write authority: Silo is the git host

### 8.1 Hosting and enforcement

Silo serves its repos and the Grain repos over **git smart HTTP**
(`/git/<owner>/<name>.git`):

- **humans** reach it behind IAP, as the hub API (hub.md §5.1);
- **Grains** reach it with their node credential, on the IAP-exempt node
  path, as `/v1/nodes/connect` (hub.md §5.2.1).

A **pre-receive hook** runs in Silo's own process. It checks every
pushed commit, not only the tip, against the repo's `GRAINOWNERS` files
(§3.5), for the pusher identity that authentication established.

**One writer makes the hook simple.** Every Silo-hosted repo but an
intake gives its whole tree to one set of owners in `GRAINOWNERS`: Silo for its placements,
the Grain and its owner for a Grain root, the maintainers for a shared
repo. For those, authorization is one check, *is the pusher listed*, and
everything after it is validation: schemas, path rules, transitions and
references. Only an intake's rules work at the level of fields. For an
intake, it works at the level of kinds, files and front-matter fields:

1. **Refs.** It refuses non-fast-forward updates and deletions on
   protected refs. A new ref is allowed only under `review_prefix`.
2. **Changes.** For each commit, it diffs the commit against its parent.
   For each changed `.md` file, it parses the front matter at both
   revisions and computes the changed fields (`body` counts as one
   field). It then finds each file's owners (the nearest `GRAINOWNERS`,
   last matching line, with field lines overriding for their fields) and
   checks the pusher is one of them, for that verb, for every changed
   field; then the kind's fixed rules (§3.5). A file nobody owns refuses
   the push, with a message naming the file, the field and the
   `GRAINOWNERS` line that was nearest.
   **Names.** A file whose name has a kind suffix must have front matter
   whose `kind:` equals it, and a file whose front matter has `kind:` but
   whose name has no suffix is refused too, so an object can never be
   mistaken for an asset (§3.2).
3. **Paths.** Identity is the path (§2.1), so:
   - a `kind: moved` file can never be changed or deleted, so no new
     object can be created at a tombstoned path;
   - only `*.ref.md` files, and assets outside a frozen task's
     `attachments/`, may be deleted. Any other object leaves its path
     only by becoming a tombstone, or by the archive mapping (§3.8), under
     a `move` its owners allow;
   - no new object may be created at a path whose tombstone or archive
     path exists;
   - a tombstone's `to:` must resolve to an object whose `moved_from`
     lists the tombstone's global address. Within one repo, both are in
     the same push. Across repos, the new home must already exist
     (§8.11).
4. **Validation.**
   - Each changed file must pass its kind's JSON Schema, and its
     `schema` version must be one the hook knows.
   - A new or changed reference must resolve, following redirects, with
     **the pusher's** read rights (§11.4).
   - A backlog reference must resolve to a `kind: task`, and no other
     reference in the same lane may point at the same task.
   - A `grain.config.md` must pass its schema: each entry's `path`, in
     another repo, carries its `@ref` and `ref_mode` (`track` names a
     branch, `pin` a tag or a commit), its `ignore:` patterns parse, at
     most one memory entry is `write: true` and it is in the same repo,
     and `grain.id` and `owner` are unchanged. Each entry's folder must
     resolve with the pusher's rights when it is added (§11.4).
   - A push that would take a folder over the `objects_per_entry` limit
     of any root entry Silo knows names it is refused (§3.7).
   - With `orphans: refuse` in the repo's `.grainconfig`, a push that
     leaves a new orphan is refused (§3.9).
   - An execution record's `task` must resolve, and its `from` must have
     existed at `from.rev`. Its state changes must follow §5.3, and
     terminal states are final. A **fenced** run, whose `from` is a
     placement Silo has since removed, takes only `state: superseded`
     (§8.8).
   - A task's spec fields are frozen once it is approved (§5.1).
   - A task's `context:` entries must resolve with the pusher's rights.
     `write` must name a repo or folder at a branch, `depth` is at most 3
     and only on files, and no two entries may land at overlapping paths
     (§5.6). A template's `input:` entries must name a declared input of
     type `ref`.
   - A `kind: asset` sidecar's `asset` must resolve to a file that is not
     an object, matching its `sha256` and `bytes` where given. A blob over
     10 MiB is refused, and an object may not be an LFS pointer (§2.2).
   - A push that adds a `depends_on` cycle within Silo's graph is
     refused.
5. **Secrets scan.** Content is checked against secret patterns (§11.6).
6. **Post-receive.** Silo updates its database projection (tasks,
   backlogs, runs, each Grain's active set) and its reverse indexes (§4),
   and refreshes its listing cache for the folders that changed (§3.10).
   It sends `pull` notifications to the Grains whose root entries name a
   folder that changed, removes the placements and intake references of tasks
   that just closed (§8.5), and fires the triggers watching what moved,
   planning among them (§6.2, §7.4).

Plain git cannot express per-path permissions, but the server that
receives the push can. **No pull requests and no GitHub are needed.**

### 8.2 Propose

**By git:** a user clones the team's intake repo, adds
`tasks/<yyyy-mm-dd>-<slug>/task.md` with `state: proposed`, commits and
pushes.

**The hook allows it** under the intake's `authenticated` rule:

- the push may only *create* files;
- each new file must be a `kind: task` with `state: proposed`;
- `submitter` must equal the pusher.

**Silo's UI, CLI and API** (`POST /v1/tasks`) make the **same commit**,
authored by Silo, with `submitter` taken from the caller's sign-on. Silo
pushes as `silo` under its own rule, which requires
`submitted_via: api`. If the caller also ranks it, the same commit adds
a reference in the intake's `backlog/`.

Commits: **intake**, 1.

### 8.3 Approve: accepted and rejected

**Accepted.** An approver edits the file to `state: approved`,
`approved_by: user:<self>` and `approved_at`, then pushes. The hook
checks four things:

- the pusher is `team:<team>#approver`, according to Silo's directory at
  push time;
- `approved_by == $pusher`;
- `approved_by != submitter` (INV-17 in
  [threat-model.md](threat-model.md));
- the only fields changed are `state`, `approved_by` and `approved_at`,
  and `state` went from `proposed` to `approved`.

Silo then places the task (§8.4), and in `code-repo` mode starts moving
it to its code repo (§5.5). Commits: **intake**, 1, by the approver.

**Rejected: wrong pusher.** `mallory@example.com`, who is not an
approver, pushes `state: approved, approved_by: user:approver@example.com`.
The hook refuses the whole push, so the ref does not move:

```
! [remote rejected] main -> main (pre-receive hook declined)
remote: tasks/2026-10-16-approval-by-push/task.md: field state: no writers rule lets
remote:   user:mallory@example.com update kind task (team:hub#approver required for proposed→approved)
remote: tasks/2026-10-16-approval-by-push/task.md: approved_by must equal the pusher (user:mallory@example.com)
```

Commits: none. An approver who sets `state: rejected` (plus a reason) is
accepted under the same rule, and Silo removes the task's intake
reference. Commits: **intake**, 1 by the approver, then 1 by Silo.

### 8.4 Place on a Grain

The dispatcher chooses a Grain the same way it does today (hub.md §6.2),
among the Grains whose roots include their folder in the team's
placements repo with `kinds` covering `ref` (§8.12). Placement writes
**one file**, in Silo's own repo:

1. **The team's placements repo:** Silo creates
   `grains/<grain>/backlog/<task name>.md`, a `kind: ref` with:
   - `to:` the task, and `spec_rev:` the commit of the task's repo it read;
   - `rank` and `priority` copied from the task's intake reference, or
     ranked after the lane's last reference (§5.2);
   - `added_by: silo` and `added_at`;
   - `attestation`: the signed-claims JWT of hub.md §6.4, with `aud`
     this Grain, `task` the task's global identity, and `spec_digest`
     computed over the task's canonical spec at `spec_rev`.

   Then Silo notifies the Grain over the WebSocket:
   `{"type":"pull","repo":…,"sha":…}`.

**The task is untouched, and is not copied, and the Grain's root is not
written.** The intake's reference, if any, stays as the team's view. The
placement is idempotent: Silo writes nothing if the Grain's folder
already references the task. A Grain that is
offline is not chosen, as today. A Grain that was notified but does not
create a run within the offer timeout is handled as a refusal (§8.8).

Commits: **placements**, 1, by Silo. **Grain root**: none. **Intake**:
none.

### 8.5 The Grain runs it

1. The Grain fetches the trees of its root entries (prompted by the
   notify, and also every few minutes in case a notify was missed), and
   re-lists the entries whose folder's tree hash changed (§3.7, §3.10). It finds a new
   reference, resolves its `to:` at `spec_rev`, and validates the
   reference and its attestation (§11.7). It reads the task's tip for its state, so it sees
   a cancel (§8.7). It then holds the task to local policy, as it does
   an offer today.
2. **Accept:** the Grain creates `runs/<yyyy-mm-dd>-<slug>/run.md` in
   its root, with `state: accepted`, `task: {ref, rev}`, `from: {ref,
   rev}`, `grain` and `accepted_at`, and pushes. When the lanes give it
   the task (§5.2) and a slot is free, it resolves the task's
   `context:`, holds every entry to its policy, and materialises the
   sandbox (§5.6). Then it commits `state: running`, with the record's
   `context:` saying what was materialised and skipped. An entry the
   policy refuses makes the run `refused` instead, naming it.
   **Refuse:** it creates the record with `state: refused` and
   `refusal: {reason, at}` instead.
3. **While it runs:** the thread is pushed as comment files under the
   run, at most one push every 5 seconds, batched. Coarse waits are
   `state: waiting`. Events stream over the WebSocket as they do today.
4. **On finish:** one commit sets `state: done|failed` and writes the
   `closed:` snapshot:
   - `outcome`, `at` and `summary`;
   - `change: {url, merge_commit, branch}`;
   - `design_revs`;
   - `follow_ups: [<ref>]`.
5. **Silo's post-receive** sees the closed run, and removes the task's
   placement and the intake's reference, both in Silo's own repos. A
   task from the Grain's own lane has its reference removed by the Grain,
   in the closing commit. The run's `from.rev` still resolves the
   reference it came from.

Commits:

- **Grain root**: 1 (accepted) + 1 (running) + ⌈comments/batch⌉ + 1
  (closed), all by `grain:<id>`;
- **placements**: 1 by Silo, removing the reference;
- **intake**: 1 by Silo, removing the team's reference, if there was one;
- the task file: none, ever.

The Grain writes only its root. It cannot touch a placement, because the
placement is in Silo's repo, and it cannot write the task at all,
because the task is in another repo. So a Grain cannot rewrite what it
was asked to do, or claim to have been asked.

### 8.6 Reorder

- **The Grain's owner reorders their own lane:** edit `rank` in one
  reference in the root's `backlog/` (`grain backlog move …` or by hand)
  and push. Commits: **Grain root**, 1, by the owner or by `grain:<id>`
  on the owner's behalf.
- **The owner reorders Silo's placements:** `grain backlog move` on a
  placement calls Silo's API, or the owner drags it in Silo's UI. Silo
  commits the new `rank` with `ranked_by` the owner. Commits:
  **placements**, 1, by Silo.
- **The owner reweights the lanes:** edit `backlog:` in the root's
  `grain.config.md`.
  Commits: **Grain root**, 1, by the owner.
- **The team reorders its unplaced work:** a member drags it in Silo's
  UI, or edits `rank` in the intake's reference and pushes. Commits:
  **intake**, 1.
- **Silo expedites a task already placed** (an operator's `at_front`):
  set `expedite: true` on the placement. Commits: **placements**, 1, by
  Silo. It reorders within Silo's lane only, unless the root's
  `lanes.expedite` is `global`.

### 8.7 Cancel

A user asks in Silo's UI, or an approver asks through the API. Silo
decides, and writes on the **task**:

1. **The intake:** `state: cancelled` and `cancel: {by, at, reason}`.
   Then Silo notifies every Grain whose backlog references the task
   (from the reverse index): `{"type":"pull","repo":<intake>,…}`.
2. **The Grain honours it.** It reads the task's tip whenever it pulls,
   and before each attempt. A Grain with an open run stops it and commits
   `state: cancelled` with `closed: {outcome: cancelled, at}`. A Grain
   with no run yet never starts one.
3. **Silo removes the references**, as for any close (§8.5).

A Grain that is away sees the cancel when it next pulls, which replaces
today's `cancel_pending`. A task homed in a code repo is cancelled there,
and Silo fences its runs at once while that lands (§5.5).

Commits:

- **intake**, 1 by Silo (the cancel, and the intake reference's removal
  in the same commit);
- **Grain root**, 1 by the Grain (its run cancelled);
- **placements**, 1 by Silo (the placement removed).

### 8.8 Re-placement

Re-placement happens after a lost Grain (the team's grace, hub.md
§6.3), a refusal, or an offer timeout. **The task does not move, so
nothing is tombstoned**: the task keeps its address, and only its
placement changes. **Re-placement is Silo moving the reference between
two Grains' folders** in its own placements repo:

1. **The placements repo, one commit:** delete
   `grains/<old>/backlog/<task>.md` and add `grains/<new>/backlog/<task>.md`,
   a placement as in §8.4, with `previous:` naming the old Grain's run if
   there was one. The two Grains of one team share the team's placements
   repo, so this is atomic: no reader ever sees the task in both folders,
   or in neither. Across two teams' placements repos it is two commits,
   removal first.
2. **A comment on the task, in the intake**, saying why (`2026-10-16-refuse-self-approved/comments/20261016T164600Z-silo.md`
   in the examples). The old Grain's root is not Silo's to write.
3. Silo notifies both Grains. The new Grain accepts and starts **its own**
   run, copying `previous:`.

Each step is a no-op if its effect is already in git, so a retry after a
crash is safe. Where the move takes two commits, **removal comes first**,
because the two gaps are not equal. Between them the task is briefly
unplaced, which Silo's dispatcher notices and finishes. The other order
would leave two Grains holding the task, which can produce two changes.

**The fence** is the removed placement. An open run whose `from` names a
placement that is readably gone is **fenced** (§5.3):

- readers treat it as superseded at once, without any write in the old
  Grain's root: the derived status of the task follows the new run;
- Silo's hook refuses every push to that run except one, the old
  Grain's own `state: superseded` with `superseded: {at, by, withdrawn_at,
  reason}`;
- if the old Grain comes back mid-task, its fetch shows the placement
  gone. It stops, and writes that one commit. A branch or pull request
  it had already pushed is reported in the hub as belonging to a
  superseded run.

A Grain that cannot *read* its placements (Silo down, a revoked
credential) is not fenced: an unreadable space is unknown, not empty
(§3.3). It finishes what it is running and starts nothing new from that
lane.

**History shows both attempts**: the old record and its thread stay in
the old Grain's root, and the new record's `previous:` links to them.

Commits: **placements**, 1 by Silo; **intake**, 1 by Silo (the comment);
**old Grain root**, 1 by the old Grain when it returns; **new Grain
root**, the new run's own commits.

### 8.9 A cross-Grain dependency completes

Task B, `intake:tasks/2026-10-16-refuse-self-approved/task.md`, has
`depends_on: [../4310-carry-approver/task.md]`, which is task A. B is
placed on Grain `us`, and A ran on Grain `eu`.

**A dependency is satisfied when the target task has an execution record
in state `done` that is not superseded.** `failed`, `cancelled` and
dangling dependencies keep B blocked, with the reason shown.

1. Grain `eu` pushes `state: done` on `eu:runs/4310-carry-approver/run.md`.
2. Silo's post-receive looks A up in its reverse indexes: B depends on
   A, and B is referenced from `us`'s backlog. It notifies `us` to pull
   `eu`.
3. `us` reads A's runs from `eu`. It finds them through Silo's run index
   (`GET /v1/runs?task=<A>`), or by scanning `kind: run` in the repos it
   can read. Both give the same answer, because git is the truth. It
   sees `done` and unblocks B.

`us` needs read access to `eu`, which Silo granted when it placed B,
because B's dependency's runs live there (§11.4). A Grain without that
access sees the dependency as dangling, and keeps B blocked.

Commits: **eu**, 1 (A's close), then 1 by Silo (A's reference removed).
The dependency needs no commit in `us`. B's next state change is its own
run's `running` commit.

### 8.10 Content on other hosts

Silo cannot enforce writes to repos it does not host:

- a standalone Grain's local repo;
- GitHub, Gerrit or a bare server holding shared templates or designs.

So for those a Grain applies the same rules **on read**:

- **Validation.** It checks schema, the path rules and allowed
  transitions, and the repo's `GRAINOWNERS`, as far as a commit's author
  can be trusted. It cannot check who pushed. Where the repo's
  `.grainconfig` says `require_signed: true`, it also checks
  commit signatures against the `keys` listed for each principal (§11.8).
- **Placed tasks.** A Grain runs only tasks referenced from **its own**
  backlog lanes: its root's own lane, or a folder its `grain.config.md`
  names as a lane. It never runs a task merely
  because a foreign repo contains one, ranks one, or references one.
- **Foreign content is claims.** Templates, triggers and designs from
  another host are data that a task the Grain already accepted may use.
  They grant nothing (§11.4).
- **A standalone Grain's own repo** is trusted as its owner's: the owner
  has the filesystem anyway.

**Code repos** are other hosts too, with their own review: §8.13.

### 8.11 No cross-repo transactions

Every multi-repo flow above writes in a fixed order, and records the
step it has reached. A reader that catches the gap sees one of these:

- an approved task that no backlog references yet: unplaced, and the
  dispatcher finishes the placement;
- a placement in a new Grain's folder with the old Grain's run still
  open: that run is fenced, so readers already treat it as superseded;
- a closed run whose placement has not yet been removed: a Grain skips
  it, because it already has a closed run of the task;
- a placement in a folder no Grain's `grain.config.md` names yet (link
  in progress): Silo does not place there until the entry exists, and a
  placement made just before an unlink is re-placed;
- a moved object at its new address whose old address is not yet a
  tombstone: readers of the old address still find the old object, which
  is unchanged.

Each step is keyed by (task identity, operation, Grain), so a retry is a
no-op. Silo's dispatcher records the step it has reached in its database
row, as it records an offer today. After a crash it resumes from the
first step whose effect is missing in git.

| Flow | Order |
| --- | --- |
| place | placements (reference) → notify |
| close | Grain root (run closed) → Silo removes the references (placements, intake) |
| cancel | intake (task `cancelled`, intake reference removed) → notify → the Grain's run `cancelled` → placement removed |
| re-place | placements (one commit: the reference leaves the old Grain's folder and enters the new one's) → intake (Silo's comment) → notify; later, the old Grain's `superseded` |
| link | Silo's folder for the Grain (created, if new) → the owner's commit adding it to the Grain's `grain.config.md` |
| unlink | the owner's commit removing those entries → Silo re-places what had no run |
| move across repos | new home (with `moved_from`) → tombstone at the old path → Silo rewrites references' `to:` |
| upward filing | intake (proposed, committed by Silo) → the Grain's comment on its run naming it |
| trigger firing in Silo | intake (approved or proposed task, deterministic path) → placements (reference); last fire recorded in Silo's DB |
| trigger firing on a Grain | Grain root only: the task (deterministic path) and its reference in the root's `backlog/fired/` lane, in one commit; last fire recorded in the Grain's DB |
| planning (a `design-change` firing) | intake proposals and their references (deterministic paths) → the design's last planned revision recorded in Silo's DB |
| gated change in a Silo repo | the gate's review tasks (intake, Silo's) → their runs close → Silo's merge of the review branch, with the outcomes in its trailers |
| definitions via review branch | `review/*` branch → the approver's merge commit on `main` |
| home a task in a code repo (`code-repo` on approval, or `promote`) | code repo (the task with `moved_from`, by direct push or a merged change) → intake (tombstone) → Silo rewrites `to:` and `spec_rev` in the backlogs it hosts |
| cancel a task homed in a code repo | placements (references removed, which fences open runs) → code repo (`cancelled`, by direct push or change) |

**Definitions** (templates, triggers, gates and designs) are pushed straight
to `main` by maintainers, or through **review branches**. Anyone with
`create` rights may push to `review/*`. The hook accepts a commit on
`main` that touches a definition only if one of these holds:

- the pusher is a maintainer;
- the commit is a merge whose second parent is a `review/*` tip, and the
  merge's pusher is an approver other than the branch's author.

On receive, Silo checks schema and references, applies the repo's
gates (§6.3), and fires the `design-change` and `push:`/`merge:`
triggers that watch what moved (§6.2, §7.4).

### 8.12 The remaining worked flows

**An agent's upward filing.** An agent on Grain `eu` calls
`propose_task` for another team's repo. The Grain does not push to that
team's intake, because it never writes other repos. It asks Silo, over
its node connection, as `propose_task` does today. Silo checks that the
Grain has an open run whose task has that submitter, then commits to the
team's intake a new task with:

- `state: proposed` and `submitted_via: agent`;
- `submitter`: the human who submitted the originating task;
- `filed_by: {grain: eu, run: "eu:runs/4310-carry-approver/20261016T135830Z.run.md"}`.

Silo returns the new task's path. The Grain writes a comment on its run
naming it, and lists it in `closed.follow_ups` when the run closes.

Commits:

- **intake (other team)**, 1, by Silo;
- **eu**, 1, by the Grain: the comment on its run.

It then needs approval like any proposal. A standalone Grain files into
its own repo instead.

**A schedule firing a templated task.** The `prune-packages` trigger in
`hub/shared` (`on.schedule: {weekly: {day: sunday, at: "04:00"}}`)
comes round at 04:00 on Sunday. Silo renders the template its `fires:`
names at its current tip `<sha>`, and commits
`tasks/fired/prune-packages/20261011T0400Z.task.md` to the team intake
with:

- `state: approved` and `approved_by: silo:trigger`. This is allowed
  only because the trigger's `auto_approve_proposals` was itself
  approved when the trigger merged, and no person caused the firing;
- `template: {ref, rev: <sha>, inputs: {…}}`;
- `trigger: {ref, on: schedule, key: 20261011T0400Z, fired_at}`.

Silo's database records the fire and computes the next one; nothing about
it is committed to `hub/shared`. Had Silo been down at 04:00, `catch_up:
{mode: latest-only}` would fire it once on restart, at the same path. A
hub approver's `silo trigger run shared:triggers/prune-packages.trigger.md`
(its `manual:` source) files the same task at
`tasks/fired/prune-packages/<request time>-<approver>.task.md`.

Silo then places it, by adding a placement to a Grain's folder. Commits:

- **intake**, 1;
- **placements**, 1.

If no Grain can take it, the second commit is a reference in the
intake's backlog instead.

**A design edit fires the planning trigger.** The `review/hs-approval-by-push`
branch adds `### Approval is a push {#HS-6}` and edits HS-3. An approver
merges it to `main` in `hub/shared`, and the hook accepts it under
§8.11. Post-receive runs the `design-change` source for the triggers
watching `designs/`, which classifies the requirements:

- HS-6 is **new**;
- HS-3 is **changed, done**;
- the rest are unchanged.

With a trigger that says `fires.per: requirement`, Silo commits three
proposals to `hub/intake` in one commit, at the deterministic paths of
§7.4:

- implement HS-6;
- test HS-6;
- a delta for HS-3.

Each has `implements: [{…#HS-n, rev, hash}]`. The same commit adds an
intake reference for each, ranked in plan order (the implementation
ahead of its test) after the team's current last reference. Commits:
**intake**, 1. Approving each one then follows §8.3 and §8.4. The
example's `plan-designs` trigger says `per: event` instead, so the same
merge files one planner task,
`tasks/fired/plan-designs/hub-intake/<to_sha>.task.md`, whose agent
proposes the tasks; `fixtures/design-plan.inputs.json` is its binding.

**Link, then unlink, a standalone Grain.** Linking is an edit to the
Grain's `grain.config.md` by its owner, and unlinking is the reverse
edit. **Nothing Silo writes can activate anything on a Grain unless the
Grain's owner pointed a root at it** (§11.14): that is the PEP property
the whole channel rests on.

- **Link:** `grain hub login`, then `grain tasks link --team hub`.
  1. Silo creates the Grain's folders in the team's placements repo,
     `grains/<id>/backlog/` and `grains/<id>/triggers/`, if they are new.
     If the Grain's root is a local repo, Silo also creates a hosted
     repo for it, `grains/<id>.git`, whose `GRAINOWNERS` gives it to the
     Grain and its owner only, and the Grain pushes its local history
     there. This first push is allowed for the owner's credential only,
     and only into an empty repo. A root that already lives where Silo
     can read it stays where it is.
  2. `grain tasks link` shows the owner this diff, and commits it with
     their credential when they confirm:

     ```diff
      backlog:
        mode: weighted
        lanes:
     +    - {name: silo, path: "placements:grains/eu/backlog/@main", ref_mode: track, weight: 3}
          - {name: own, path: backlog/, weight: 1}
      triggers:
        - path: triggers/
     +  - {path: "placements:grains/eu/triggers/@main", ref_mode: track}
     ```

     plus the `placements` alias in the repo's `.grainconfig`. The
     trigger entry is added only if the owner wants Silo to assign team
     triggers.
  3. Silo reads the root's `grain.config.md`, sees its folder named as a
     lane, and from then on places work on the Grain.

  The Grain's objects keep their paths, so every reference inside the
  root still resolves. If the root moved from `file://` to the hosted
  URL and something outside referenced it, the local repo's
  `.grainconfig` gets `moved_to`. Commits: **placements**, 0 or 1 by
  Silo; **Grain root**, its history (if newly hosted) plus 1 by the
  owner.
- **Unlink:** `grain tasks unlink`, or removing the entries by hand.
  1. The owner's commit removes Silo's lane and trigger entry from
     `grain.config.md`. From that commit the Grain lists nothing of
     Silo's, so it starts nothing from Silo, and nothing Silo writes can
     reach it.
  2. Silo sees the entries gone. It re-places the placements that have
     no run (§8.8), each a move out of this Grain's folder. Open runs of
     placed tasks must close first; with `--force` the owner unlinks
     anyway, and those runs are fenced as their placements move.
  3. Silo stops placing on the Grain, and archives its folder under
     `grains/archive/<id>/`, so references into it (`from`, `previous`)
     still resolve through the archive mapping (§3.8).

  The root, with all of its history, stays the Grain's. A Silo-hosted
  root becomes a remote the Grain may keep pushing to, or the owner sets
  `moved_to` and goes back to a local repo. Commits: **Grain root**, 1 by
  the owner; **placements**, 1 per re-placed task.

### 8.13 Code repos: write authority is the repo's own

Silo's pre-receive hook (§8.1) runs only on the repos Silo hosts. In a
code repo, on GitHub, Gerrit, a depot or anything else, **write
authority comes from that repo's own host and review process**: whatever
its branch protection, required reviews, gates and merge queue
let land on `default_ref` is what the repo says. Silo neither proxies
nor mirrors those pushes. Its `.grainconfig`, if it has one, says
`host: other`, and its `GRAINOWNERS` describes what belongs there, but
nobody can check who pushed. Readers treat a code repo's objects like this:

- **Trust follows the repo's merge process.** An object on the code
  repo's `default_ref` is trusted exactly as much as code merged beside
  it: no more, because Silo did not check its pusher, and no less,
  because the repo's own review did. A repo that wants its grain folder
  reviewed more strictly than its code says so with a gate on
  `.grain/**` (§6.3), as bwsalmon/silo already does with a path trigger.
- **Silo validates on read.** Silo reads a code repo when one of its
  repos references it, on the host's push webhook where there is one,
  and otherwise on its next pass. For each changed file in a grain
  folder, it checks what the hook would: the name and kind suffix
  (§3.2), schema, the path rules (§2.1), `GRAINOWNERS` on read (§8.10),
  references
  (§4), design ID allocation (§7.1), and the invariants of §5.5. It
  resolves references from a code repo with the rights of whoever is
  using them, never with its own (§11.4). An object that fails is shown
  in the repo's health view, and as a comment on the change if Silo
  opened it, and is not used. Silo never writes to a code repo except
  through §5.5's direct push or change.
- **Grains are the PEPs, as everywhere.** A Grain runs a task from a
  code repo only through a placement in one of its lanes, whose
  attestation's `spec_digest` must match the file (§11.7). Its local
  policy may limit the code repos it reads templates, triggers and
  designs from. Definitions are data that an accepted task may use; they
  grant nothing (§8.10).
- **What a code-repo object can do is bounded by the design, not by its
  `GRAINOWNERS`.** Anyone who can merge to the repo can edit it and its
  `.grainconfig`, so they are advisory there, and the limits below hold
  whatever they say:
  - a task there is placed only with an intake approval behind it (§5.5);
  - a template, trigger, gate or suite carries no `reads`, `writes`,
    `grants` or `target` (§12.1);
  - a trigger fires only where a Grain or a team named it in a trigger
    root (§6.2), and
    a workflow trigger runs only that repo's own workflows;
  - a gate there governs only that repo: it carries no `match.repos`;
  - a `task_home` there is bounded by the team's `allow` (§5.5).

A standalone Grain may trust a code repo further, since its owner's
policy decides. It may run tasks committed there by hand, for instance,
if its policy names that repo.

### 8.14 Worked flows for each task home

The same work in each mode, with the commits each repo receives. "Placements /
root" is Silo's folder for the placed Grain, then that Grain's own root, and "the work" is the change the
agent makes, which lands in the code repo through its review in every
mode.

**`intake` (the default).** Task 4310 in the examples: proposed, approved,
placed on `eu` and run there, all with the task at
`intake:tasks/4310-carry-approver.task.md` throughout.

| Step | Intake | Code repo | Placements / root |
| --- | --- | --- | --- |
| propose (§8.2) | 1: the task, `proposed` (and its reference, if ranked) | — | — |
| approve (§8.3) | 1, by the approver | — | — |
| place (§8.4) | — | — | placements: 1, by Silo: a reference to the intake path |
| run (§8.5) | — | the work | root: `accepted`, `running`, the thread's batches |
| close (§8.5) | 1, by Silo: the team's reference removed | none | root: 1, the run closed; then placements: 1, by Silo: the reference removed |

**`code-repo`.** The hub team sets
`task_home.repos: {https://git.example.com/hub/widgets.git: code-repo}`.
A member proposes `tasks/2026-10-17-widget-lint/task.md` for the widgets
repo, and an approver approves it.

| Step | Intake | Code repo | Placements / root |
| --- | --- | --- | --- |
| propose | 1: the task, `proposed` | — | — |
| approve | 1, by the approver | — | — |
| place the task's file | 1, by Silo: a comment naming the pending change | 1 commit on `silo/task-home/2026-10-17-widget-lint`, adding `.grain/tasks/2026-10-17-widget-lint/task.md` with `moved_from`; the merge queue lands it as 1 merge on `main` (or 1 direct push, where the host allows it) | — |
| place the work, without waiting | — | — | placements: 1, by Silo: a reference to the intake path |
| the change merges | 1, by Silo: the tombstone, plus the team's reference rewritten, in one commit | — | placements: 1, by Silo: `to:` and `spec_rev` rewritten |
| run | — | the work | root: `accepted`, `running`, the thread |
| close | 1, by Silo: the team's reference removed | none: the task file is never touched again | root: 1, the run closed; then placements: 1, by Silo: the reference removed |

If the change merges before the task is placed, the reference is created
pointing at the code repo, and there is nothing to rewrite. A run
accepted before the merge names the intake path in `task.ref`, and keeps
resolving it through the tombstone.

**`promote`.** The example's
`widgets:.grain/tasks/2026-10-16-widget-schema-v2/task.md`. The widgets
repo says `task_home: promote`. The task, which implements WG-2 of the
widgets design, was proposed and approved in the intake, and placed on
`eu`. The agent working on a related task then proposed promoting it,
since it will be revisited with each format version, and bsalmon@google.com
confirmed.

| Step | Intake | Code repo | Placements / root |
| --- | --- | --- | --- |
| propose | 1: the task, `proposed` | — | — |
| approve | 1, by the approver | — | — |
| place | — | — | placements: 1, by Silo: a reference to the intake path |
| propose the promotion (agent) | 1, by Silo: a comment recording the proposal | — | root: 1, by the Grain: a comment on the agent's run naming it |
| a member confirms | 1, by Silo: a comment naming the pending change | 1 commit on `silo/task-home/2026-10-16-widget-schema-v2`, then the merge queue's merge on `main`, after the repo's `.grain/**` gate is satisfied (below) | — |
| the change merges | 1, by Silo: the tombstone (`kind: moved`, `moved_by` the member) | — | placements: 1, by Silo: `to:` and `spec_rev` rewritten |
| run | — | the work | root: `accepted`, `running`, the thread |
| close | 1, by Silo: the team's reference removed, if there was one | none | root: 1, the run closed; then placements: 1, by Silo: the reference removed |

A member who promotes directly skips the proposal row. In the examples,
the tombstone is
[`hub-intake/tasks/2026-10-16-widget-schema-v2/task.md`](../examples/task-repos/hub-intake/tasks/2026-10-16-widget-schema-v2/task.md),
Silo's comment beside it records the change, and `eu`'s
[placement](../examples/task-repos/hub-placements/grains/eu/backlog/2026-10-16-widget-schema-v2.md)
names the code-repo path, while the other placements in that folder
name the intake.

**A gated change held for a human approver.** The promotion above is a
change to `.grain/tasks/2026-10-16-widget-schema-v2/task.md` in the
widgets repo, and the widgets repo's
[`.grain/gates/grain-declarations.md`](../examples/task-repos/widgets-code/.grain/gates/grain-declarations.md)
matches `.grain/**`, `requires: {security_review: {}, human: {when:
always, from: ["team:hub#approver"]}}`, `sensitive: true`. Silo opens
the change from `silo/task-home/2026-10-16-widget-schema-v2` and puts it
in grain's merge queue, which Grain `eu` runs for the widgets repo:

1. The queue reads the widgets repo's gates at `main`'s tip, not at the
   change's head, and finds this one matching. No other gate matches.
2. The match is sensitive, so the queue withdraws the change's
   auto-merge and labels it sensitive.
3. It files the security review the gate requires, as `eu`'s own task:
   `eu:tasks/gated/widgets/<change>-<head>-grain-declarations-security-review/task.md`,
   `approved` (`approved_by: grain:eu`, `submitted_via: gate`), with the
   change in its `context:` to read. It is the only work the gate files.
   Nothing is filed in the widgets repo.
4. The review runs and closes `done` with no findings. The change still
   waits: `when: always` needs a human whatever the review found.
5. bsalmon@google.com, a hub approver, Submits. The author of the
   change (Silo) and the member who confirmed the promotion would not
   have counted; an approver who was neither does. The queue checks the
   approver against `from` at `main`'s tip, merges, and the tombstone
   step above follows.

| Step | Intake | Code repo | Grain root (`eu`) |
| --- | --- | --- | --- |
| the change opens | — | the change, held: no merge | — |
| the gate's review | — | — | 1: the review task, with its reference in the root's `backlog/gated/` lane; then the run's own commits |
| the human's Submit | — | 1: the merge queue's merge on `main` | — |
| the change merges | 1, by Silo: the tombstone | — | — |

Had the gate said `when: on-findings` (as bwsalmon/silo's
`hub-trust-boundary` path trigger does today), step 4's clean review
would have released the change with no human. The same change into a
Silo-hosted repo is held by Silo's pre-receive hook instead: Silo's merge
queue files the review in the owning team's intake, and the hook accepts
the review branch's merge only with the review's outcome and the
approver's Submit in its trailers, checked against the gate at the ref's
tip (§6.3).

### 8.15 Worked flows for roots

Each flow below says what changes, what the Grain reads, and the commits
it makes. The examples hold every file named.

**Adding a task.** `eu`'s owner wants to tidy the run notes.

1. The task home says where the task lives (§5.5). For the owner's own
   work that is the root, so they write
   `tasks/2026-10-16-tidy-run-notes.task.md` (`kind: task`, `state:
   approved`, the owner being the Grain's approver for its own root).
2. In the same commit they add
   `backlog/2026-10-16-tidy-run-notes.ref.md` (`to:
   ../tasks/2026-10-16-tidy-run-notes.task.md`, rank `i`) in the `own`
   lane.
3. The Grain lists `backlog/` (a tree read), finds one new `*.ref.md`,
   parses it and its task, and ranks it in the lane.

The task file alone would do nothing: it is reached only from the
reference. A team member's task goes to the intake instead (§8.2), and is
active on a Grain only once Silo places a reference to it in a folder
the Grain names (§8.4). Commits: **Grain root**, 1.

**Silo placement and re-placement.** Silo places
`2026-10-16-refuse-self-approved` on `eu` by writing
`grains/eu/backlog/2026-10-16-refuse-self-approved.ref.md` in the
placements repo (§8.4). `eu` lists it because its `silo` lane names
`placements:grains/eu/backlog/`. When `eu` goes quiet, Silo re-places it
with **one commit** that moves the reference to `grains/us/backlog/`
(§8.8). `us` lists it through its own Silo lane; `eu`'s listing loses it,
so `eu`'s open run is fenced. Neither Grain's `grain.config.md` changes.
Commits: **placements**, 1 per placement or move.

**Linking and unlinking** are §8.12: the owner's commit adds or removes
Silo's entries in `grain.config.md`.

**A team trigger firing through a trigger root.** `eu`'s config names
`shared:triggers/@main` (tracking) as a trigger root. At 06:00 on
2026-10-16:

1. `eu`'s listing of that entry holds `granule-pull.trigger.md`,
   `prune-packages.trigger.md` and `plan-designs.trigger.md`; it skips
   `archive/`. `granule-pull` says `fires_in: grain` and
   `on.schedule: {daily: {at: "06:00"}}`, so `eu` registered it when it
   parsed the entry's triggers. The other two say `fires_in: silo`:
   listed, never fired here.
2. `eu` renders the template the trigger's `fires:` names, at its tip,
   and commits to its own root, in one commit,
   `tasks/fired/shared/granule-pull/20261016T0600Z.task.md` and a
   reference to it in `backlog/fired/`. The entry tracks a branch and the
   trigger says `auto_approve_proposals: false`, so the task is
   `proposed`, and the owner approves it.
3. It runs from the `own` lane like any local task.

Commits: **Grain root**, 1 (the firing), 1 (the approval), then the
run's own. **Silo**: none.

**Ignoring one team trigger via the entry's `ignore:`.** Grain `us`
wants the team's granule pull weekly, not daily. Silo assigned the daily
one to `us` (`placements:grains/us/triggers/granule-pull.trigger.md`).
`us`'s owner commits:

```diff
 triggers:
   - path: triggers/
   - path: "placements:grains/us/triggers/@main"
     ref_mode: track
+    ignore: ["granule-pull.trigger.md"]
```

and adds `triggers/granule-pull-weekly.trigger.md`, a whole trigger with
`derived_from` naming the original. The edit changes one entry's
`ignore:`, so only that entry's listing-cache key changes (§3.10); its
next listing drops the daily trigger, and the own entry's listing gains
the weekly one. Nothing in Silo's repos changes, and no other Grain is
affected. Commits: **Grain root**, 1.

**A team retiring a trigger by moving it to `archive/`.** The hub team
stops the legacy triage sweep. A maintainer merges one commit to
`hub/shared` that moves `triggers/legacy-triage-sweep.trigger.md` to
`triggers/archive/legacy-triage-sweep.trigger.md` (the archive mapping,
§3.8: no tombstone). Every Grain whose trigger root names
`shared:triggers/` re-lists that folder on its next fetch (its tree hash
changed), skips `archive/`, and unregisters the sweep. No Grain's config
changes, and tasks it fired before still resolve their `trigger.ref`
through the archive mapping. Commits: **shared**, 1.

**Memory precedence between the Grain's own and team memory.** `eu`'s
`memory:` lists its own `memory/` first and `shared:memory/@main` second.
Both apply to runs against the widgets repo:

- `eu:memory/deploy-notes.memory.md`: "use squash merges for the widgets
  repo";
- `shared:memory/merge-style.memory.md`: "use merge commits everywhere".

A run on the widgets repo is given both, in entry order, each headed by
its source. The run's prompt says that earlier entries take precedence,
so the agent squashes. If the team's guidance should win, the owner moves
the shared entry first, a one-line edit to `grain.config.md`. The
memory agent writes only to `memory/` (`write: true`); a note it would
add to the team's folder becomes a review branch there (§8.11).
Commits: none to read; **Grain root**, 1 per note the memory agent
writes.

**A gate enforced from a code repo's `.grain/`.** A change to
`widgets:.grain/templates/release-notes.template.md` enters grain's
merge queue for `main`. The queue lists `.grain/` at `main`'s tip (trees
only) and finds `gates/grain-declarations.gate.md`, whose `match.paths`
is `.grain/**`. It files the security review into the landing Grain's
`tasks/gated/widgets/…`, withdraws auto-merge (`sensitive: true`), and
holds the change until the review is done and a hub approver submits
(§6.3, §8.14). No Grain's `grain.config.md` mentions the gate, and no
Grain's `ignore:` could release it. Commits: **Grain root**, 1 (the
review task) and its run's; **widgets**, the merge once released.

**A capability requested by reference and allowed by policy.** The task
`2026-10-17-widgets-preview-deploy.task.md` lists
`shared:capabilities/widgets-deployer.capability.md` with `mode: use`.
Nothing made that file active: `eu` resolves it only when it materialises
the run (§5.6). It walks the chain to
`org-capabilities:gcp/acme-widgets.capability.md@v1` and its base
`gcp-service-account@v1`, checks that every link's repo is a sanctioned
source for that base in `eu`'s local policy, applies the reducers, and
checks the implications (§5.7, §8.17). A capability object in a repo the
policy does not list would be refused at this step, whoever referenced
it. Commits: none until the run's record.

**A `GRAINOWNERS`-rejected push.** A teammate with read access tries to
put their own task at the front of `eu`'s queue by pushing
`backlog/urgent-thing.ref.md` to `eu`'s root:

```
$ git push silo main
remote: refused: backlog/urgent-thing.ref.md
remote:   user:alex@example.com may not create this file
remote:   nearest GRAINOWNERS: /GRAINOWNERS, line 1: *  @grain:eu @user:bsalmon@google.com
```

Silo's own service identity gets the same answer for any file in `eu`'s
root, `grain.config.md` included. The teammate proposes to the intake
instead. Commits: none.

**A suffix mismatch.** The owner saves a reference under the wrong name,
`backlog/2026-10-17-fix-dns.task.md`, with `kind: ref` in its front
matter:

```
remote: refused: backlog/2026-10-17-fix-dns.task.md
remote:   the name says kind task, the front matter says kind ref (§3.2)
```

On a host Silo does not run, `grain lint` reports the same error, and a
reader treats the file as invalid: it is neither listed as a reference
(its suffix is not `.ref.md`) nor resolved as a task. Commits: none.

**A warm one-file push.** Silo commits one placement,
`grains/eu/backlog/2026-10-17-fix-dns.ref.md`, and notifies `eu`.

1. `eu` fetches the new commit's trees for its entries in the
   placements repo: the root tree, `grains/`, `grains/eu/`,
   `grains/eu/backlog/` (4 trees, a few KB), and `grains/eu/triggers/`'s
   hash, which is unchanged.
2. The Silo lane's tree hash changed, so its listing misses the cache;
   the walk finds one name not in the old listing. Every other entry's
   key is unchanged and hits.
3. It reads one blob, the new reference, parses it (one front-matter
   cache miss), re-ranks the lane, and, when it is picked, reads the task
   it points at.

Cost: 4 trees and 2 blobs. The 10k tasks and every other folder are not
touched.

**A sparse cold fetch.** A fresh `eu` with an empty cache and the config
of §3.3:

1. It reads `grain.config.md` (one blob) and `.grainconfig` (one blob)
   from its root.
2. For each of its 6 entries it fetches the trees along the path and
   under the folder, at the entry's commit, with `--filter=blob:none`:
   nothing else in those repos. The intake's `tasks/` and the root's
   `runs/archive/` are never listed.
3. It lists each entry from trees alone: names and kinds, and the
   `ignore:` and `archive/` cuts.
4. It reads the blobs it needs to start: every reference in its lanes
   (to rank them), every trigger (to register them), and memory documents
   when the first run starts. Then, for the first pick, the one task and
   whatever its `context:` names.

**A Grain picks its next task across lanes.** Grain `eu` has two lanes
with runnable references, `backlog.mode: weighted`, with Silo's lane at
weight 3 and its own `backlog/` at 1:

| Lane | Its order (§5.2) |
| --- | --- |
| `silo`: `placements:grains/eu/backlog/` | `2026-10-16-reject-dangling-refs` (expedite), `2026-10-16-approval-flow` (`high`, rank `t`), `2026-10-16-widget-schema-v2` (rank `w`); the prune firing is skipped, because `eu` has an open run of it |
| `own`: `backlog/` | `2026-10-16-tidy-run-notes` (rank `i`) |

Credits start at 0. Pick 1: Silo's lane has 3, the root's 1, so Silo's
lane gives `reject-dangling-refs` and pays 4 (−1 against 1). Pick 2: 2
against 2, a tie, which list order breaks for Silo's lane:
`approval-flow` (−2 against 2). Pick 3: 1 against 3: the root's lane
gives `tidy-run-notes`. Pick 4: only Silo's lane is left:
`widget-schema-v2`. `check.py` prints the same: `next on eu (weighted):
2026-10-16-reject-dangling-refs, 2026-10-16-approval-flow,
2026-10-16-tidy-run-notes, 2026-10-16-widget-schema-v2`. Under
`mode: strict` with the root's lane first, `tidy-run-notes` would go
first and Silo's three after it. Nothing is written to pick: the choice
shows up as the next run's `accepted` commit in the root.

### 8.16 Worked flows for context and assets

**A task with a write repo, a read folder, a design doc and an image.**
`intake:tasks/2026-10-16-reject-dangling-refs/task.md` is placed on
Grain `eu`. Its `context:` is the one at the top of §5.6: silo at `main`
to write, grain's `pkg/daemon/` at `candidate` to read, the hub intake
design with `depth: 1`, and a screenshot attached to the task, at
`attachments/dangling-ref-error.png` beside it.

1. **Resolve and check.** `eu` resolves each entry with its node
   credential, and holds each to its policy before reading it. Its
   `repos:` allows `https://github.com/bwsalmon/*` for `read` and
   `write`, and `https://silo.example.com/git/hub/*` for `read`. All four
   pass. The screenshot is relative, so it reads the task's `spec_rev`.
2. **Follow the design's links** (`depth: 1`). The doc is fetched into
   staging, and its body has one image, `![…](img/claims-flow.png)`, and a
   link to `grain-code:pkg/daemon/`. The image resolves to
   `shared:designs/img/claims-flow.png`, passes the policy for `read`,
   and is 250 bytes, inside the caps: it is planned, `via` the doc. The
   folder link is skipped (`not a file`): the task lists that folder
   itself. The sidecar beside the image is not linked, so it is not
   brought; a task that wants it lists it.
3. **Scope the proxy and materialise.** The proxy is scoped to silo
   (read, and push to `grain/eu/2026-10-16-reject-dangling-refs` only)
   and to grain's `pkg/daemon/` at the resolved `candidate` commit (read).
   The sandbox gets `work/silo/` (writable), `work/grain/` (a read-only
   sparse checkout of `pkg/daemon/`), the design and the image under
   `context/silo.example.com/git/hub/shared/designs/`, and the screenshot
   at its `path:`, `context/screenshot.png`.
4. **Record it.** `eu` commits `state: running` with the run's `context:`
   (§5.6): five entries with their commits and paths, and one skipped
   link.
5. **Work and push.** The agent changes `work/silo/` and pushes its
   branch. A push to `main`, or to grain, is refused by the proxy.

Commits: **eu**, the run's own (§8.5). Nothing in any other repo.

**A refused task: one entry is outside the policy.** The weekly granule
pull (`us:triggers/granule-pull-weekly.md`) fires on Grain `us`, and its
task's `context:` is grain at `candidate` to write and
`https://github.com/gke-labs/granule.git@main` to read (§6.2). Suppose
`us`'s policy admits only `https://github.com/bwsalmon/*`:

1. `us` resolves entry 1, grain: admitted. It reaches entry 2, granule,
   and the gate refuses it before anything is read from granule. Nothing
   is fetched into the sandbox, and no sandbox is built.
2. The firing is in `us`'s own lane, so there is no other Grain to try.
   `us` commits its run as `waiting`:

   ```yaml
   state: waiting
   waiting:
     on: policy
     entry: {index: 2, ref: "https://github.com/gke-labs/granule.git@main", mode: read}
     reason: "not in this Grain's repos policy (allowed: https://github.com/bwsalmon/*)"
   ```

3. Silo's views show the task as "waiting on `us`'s policy: context entry
   2, granule (read)". When the owner adds `https://github.com/gke-labs/*`
   for `read`, `us` re-checks, admits it, and commits `running`. The same
   task placed by Silo would instead be `refused`, with the same `entry`
   in `refusal`, and Silo could place it elsewhere (§8.8).

Commits: **us**, 1 (`waiting`), then the run's own once the policy admits
it.

**A template whose input chooses the target repo.**
`shared:templates/ghcr/prune-packages.md` declares an input `repo` of
type `ref`, `ref_kind: repo`, defaulting to grain at `candidate`, and has
`context: [{input: repo, mode: write}]`. Its trigger,
`shared:triggers/prune-packages.md`, says `fires.inputs: {repo:
"https://github.com/bwsalmon/grain.git@candidate"}`. When it fires in
Silo:

1. Silo checks the value against §4's grammar and `ref_kind`: a repo, with
   a branch. A value naming a file, or `@<tag>`, files nothing and is
   reported on the trigger.
2. Silo writes the task at
   `intake:tasks/fired/prune-packages/20261011T0400Z/task.md` with
   `context: [{ref: "https://github.com/bwsalmon/grain.git@candidate",
   mode: write}]` and `template.inputs.repo` recording the value. No
   `input:` survives into a task.
3. The placement's `spec_digest` covers that `context:`, and the Grain's
   policy checks it like any task's. Another team's trigger can use the
   same template for its own repo by setting `repo` to it.

Commits: **intake**, 1, as any firing (§8.12).

**A design doc whose embedded images arrive through `depth: 1`.** The
same task as the first flow, but say its author had listed only the
design doc, with no `depth`:

- **`depth: 0`** (the default): the sandbox gets `hub-intake.md` and
  nothing else. Its image link points at a file the agent does not have,
  and the run records nothing as skipped, because nothing was followed.
- **`depth: 1`**: the image comes too, at its own `context/…` path, so
  the doc's relative link still works inside the sandbox: the doc and
  its images keep their relative layout under `context/<host>/<repo
  path>/`. The run records the image with `via:` the doc.
- **`depth: 2`** would also follow the links in every Markdown file the
  doc links to. A design that links to twenty other designs, each with
  their own images, can reach `reached_files` quickly. Past the caps, the
  rest are skipped and recorded, and the agent sees which in `run.md`.

**An attachment, before and after migration.** The illustrative task
`2026-10-16-reject-dangling-refs` has one attachment, a
screenshot of the hook's refusal. Today it is a blob on the task. After
`grain tasks export`, it is
`intake:tasks/2026-10-16-reject-dangling-refs/attachments/dangling-ref-error.png`,
next to `task.md`, and the task's `context:` lists it (§2.2). A reply
with an attachment on a running task arrives through Silo's relay. The
Grain commits the comment and its file, at
`runs/<name>/comments/<utc>-<author>.md` and
`runs/<name>/comments/<utc>-<author>/<file name>`, and drops both into
the sandbox's `thread/`.

### 8.17 Worked flows for capabilities

**A task requests a narrowed GCP capability, defined by a team on top of
an org definition.** `intake:tasks/2026-10-17-widgets-preview-deploy/task.md`
asks for the widgets repo at `main` to write, and for
`shared:capabilities/widgets-deployer.md` with `mode: use`, narrowed
further to 30-minute tokens valid until 2026-10-24 (§5.7). It is placed
on `eu`.

1. **By reference only.** Nothing in `eu`'s `grain.config.md` names the
   team's `capabilities/`: there is no capability root. The task's
   `context:` entry is the only thing that reaches the definition, and
   reaching it grants nothing.
2. **Approval and placement.** The approver approved the task, reading
   its `context:`. The capability asks for no approval of its own. Silo
   resolves the chain as `eu` would, using `eu`'s published base
   declarations. It places the task on `eu`, whose declarations include
   `gcp-service-account@v1`, with an attestation that carries
   `granted_capabilities: [{ref, digest}]`.
3. **Resolve the chain.** `eu` resolves entry 2 with its node credential.
   Before each read, the gate checks that the link's source is
   sanctioned. The team's object is in `hub/shared`'s `capabilities/`,
   which `eu`'s policy sanctions for `gcp-service-account@v1`. Its base
   is `org-caps:gcp/acme-widgets.md@v1`, in the org's capabilities repo,
   which is sanctioned for that base too. That object's base is
   `{type: gcp-service-account@v1}`, so the chain ends: two links, no
   cycle, and the type is in `eu`'s `bases`.
4. **Narrow.** Starting from `eu`'s maximum for the base (its providers,
   which its declaration mirrors):
   - the org's `subset projects [acme-widgets]` leaves one project, and
     so only `widgets-preview@acme-widgets.iam.gserviceaccount.com` of
     `eu`'s accounts;
   - the team's `subset roles` keeps `roles/run.developer` and
     `roles/iam.serviceAccountUser`. That account's roles in
     `acme-widgets` are within them, so it still qualifies;
   - the team's `limit token_lifetime 1h`, then the task's 30 minutes,
     leave 1800 seconds; the task's `not_after` ends it on 2026-10-24;
   - the team's third reducer, `preview-services-only`, is
     `language: anselm`, `on: use`.
5. **Today, before Anselm, `eu` stops here.** It cannot evaluate Anselm,
   so it refuses the placement, naming the reducer:

   ```yaml
   state: refused
   refusal:
     reason: "reducer 3 (preview-services-only) is language: anselm, which eu cannot evaluate yet"
     entry: {index: 2, ref: "shared:capabilities/widgets-deployer.md", mode: use, link: "shared:capabilities/widgets-deployer.md", reducer: 3, rule: capabilities.anselm}
   ```

   Silo shows that as the task's waiting reason. Placing it elsewhere
   does not help, because no Grain evaluates Anselm yet. The team can
   drop the reducer, which is a new digest and, for an approved task, a
   new task with `supersedes:` (§5.1), or wait. `eu` never skips the
   reducer.
6. **Once `eu` evaluates Anselm,** the reducer is loaded into the Google
   API proxy as a per-use check. The implications are egress to
   `oauth2.googleapis.com`, `sts.googleapis.com` and `*.googleapis.com`,
   with no repos and no tools, and `eu`'s `network` allows them. The
   digest matches the attestation's. `eu` mints nothing yet. It writes
   `.gcp-credentials.json`, a credential config naming its token
   endpoint, and records the run's `capabilities` (§5.7).
7. **Use.** The agent runs `gcloud run deploy preview-schema-v2 --region
   europe-west1`. The token endpoint mints a 30-minute token for the one
   account. Each API call passes the proxy, the Anselm reducer allows
   it, and each decision is logged with the reducer that made it.
   `check.py` computes the same effective capability from the example
   files and prints it.

Commits: **eu**, the run's own. Nothing in the org's or the team's repo.

**A refused request: the definition comes from an unsanctioned
source.** The widgets repo's grain folder gains
`.grain/capabilities/widgets-admin.md`: `gcp-service-account@v1`, project
`acme-widgets`, no other reducers. A task in the intake requests it with
`mode: use`, and the team's approver approves it.

1. Silo places it on `eu`. `eu` resolves entry 2, and the gate checks the
   first link's source **before reading it**: `https://git.example.com/hub/widgets.git//.grain/capabilities/`
   is not in `eu`'s `capabilities.sources`. Nothing is read from the
   widgets repo for it, and nothing is minted.
2. `eu` commits `refused`, with `refusal.entry: {index: 2, ref, mode:
   use, link: <that ref>, rule: capabilities.sources}`. Silo shows:
   "refused by `eu`: capability entry 2 is defined in
   `widgets:.grain/capabilities/`, which `eu` does not sanction for
   `gcp-service-account@v1`".
3. **Laundering does not help.** Suppose the team's maintainers instead
   add `shared:capabilities/widgets-admin.md`, a sanctioned source, with
   `base: {capability: "widgets:.grain/capabilities/widgets-admin.md"}`.
   The first link passes, and the second is refused for the same reason.
   Every link must be sanctioned, because the base-most link is where
   the authority is defined (§11.30).

Commits: **eu**, 1 (`refused`).

**A refused request: implied egress the policy does not allow.** A task
placed on `us` requests `shared:capabilities/sandbox-repo.md`, an object
over `github-sandbox-repo@v1` that narrows its lifetime to the run.
`us`'s policy sanctions the base and the source. Its `network`, though,
allows only the git and model proxies.

1. The chain resolves and narrows without trouble. `us` then checks the
   implications: `github-sandbox-repo@v1` declares egress to
   `github.com` and `api.github.com`, which no reducer can remove,
   because the capability is a GitHub repo.
2. `api.github.com` is not in `us`'s `network`, so `us` refuses before
   creating the throwaway repo or writing a token:

   ```yaml
   state: refused
   refusal:
     reason: "capability entry 3 implies egress to api.github.com:443, which us's network policy does not allow"
     entry: {index: 3, ref: "shared:capabilities/sandbox-repo.md", mode: use, rule: network.egress}
   ```

3. Silo may place the task on a Grain whose policy allows that egress
   (§8.8). Silo can tell from the Grains' published declarations which
   bases they implement, but a Grain's policy stays its own, so a
   refusal is how Silo learns it.

Commits: **us**, 1 (`refused`).

**A capability that needs approval.** The weekly GHCR prune
(`shared:triggers/prune-packages.md`) has `auto_approve_proposals: true`.
After migration its template's `capabilities: [package-publish]` becomes
a `use` entry for `shared:capabilities/legacy/package-publish.md`, over
`github-packages@v1`. That base is one of grain's escalated
capabilities, so its declaration says `approval: required`, and the
object adds `approval: {by: ["team:hub#approver"], scope: task}`.

1. **The firing.** Silo files
   `intake:tasks/fired/prune-packages/<key>/task.md`, and the
   trigger's auto-approval approves the task (`approved_by:
   silo:trigger`). That approves the task, not the capability.
   `silo:trigger` is not a human, and not among the principals the
   approval names, so Silo does not place the task. It shows "waiting:
   capability approval of `shared:capabilities/legacy/package-publish.md`
   by a `team:hub#approver`".
2. **The approval.** An approver reads the task in Silo's UI and approves
   the capability. Silo commits, for them,
   `capability_approvals: [{ref, digest, by: "user:approver@example.com",
   at}]` on the task, under the intake's rule: `by` bound to the pusher,
   never the submitter (§5.7). A push of the same field by the approver
   is the same commit.
3. **The claim.** Silo places the task on `eu`. The attestation's
   `granted_capabilities` carries the ref, the digest and `approved_by`.
4. **The check.** `eu` resolves the chain and recomputes the digest,
   which matches. It checks that the approver is a principal the
   approval names. That rests on Silo's claim, the accepted risk of
   §11.8, or on a signed approval commit where `eu` requires one. `eu`'s
   policy does not list `github-packages@v1` under `approval.local`, so
   no owner approval is needed. It resolves the slot `token` to its own
   secret `ghcr/publish-token`, writes the token file, and records
   `approved: {by: "user:approver@example.com", at, via: attestation}`.
5. **Had the owner listed the base under `approval.local`,** `eu` would
   also wait, `waiting: {on: human, entry: {index: 2, …}}`, until its
   owner approved the run in the node UI. Silo's claim cannot satisfy
   that.
6. **Had the secret been missing,** the run would be `refused` with
   `refusal.secret: ghcr/publish-token`, and Silo would show "`eu` has no
   secret `ghcr/publish-token`".

Commits: **intake**, 2 (the firing, the capability approval); **eu**,
the run's own.

**A per-use denial, logged by a proxy.** The run of
`intake:tasks/2026-10-16-reject-dangling-refs/task.md` on `eu` has
grain's `pkg/daemon/` at `candidate` to read, entry 2 (§8.16). That
entry is `git@v1` narrowed to that repo and folder, the resolved commit
and `operations: [fetch]`. The agent commits a fix in `work/grain/` and
runs `git push origin HEAD:refs/heads/fix-daemon`.

1. The git proxy receives a `receive-pack` request for
   `https://github.com/bwsalmon/grain.git`. It evaluates the run's
   effective git capability: the `repos` reducers allow grain, and the
   `operations` reducer from entry 2's `mode: read` allows only `fetch`.
   It denies the push with 403, "denied by context entry 2
   (`grain-code:pkg/daemon/@candidate`, read): push is not in
   operations {fetch}".
2. The proxy logs the decision to `eu`'s audit log:

   ```json
   {"at": "2026-10-16T18:02:11Z", "grain": "eu", "run": "runs/2026-10-16-reject-dangling-refs/run.md",
    "use": {"base": "git@v1", "proxy": "git-proxy", "operation": "push", "repo": "https://github.com/bwsalmon/grain.git", "ref": "refs/heads/fix-daemon"},
    "capability": {"entry": 2, "ref": "https://github.com/bwsalmon/grain.git//pkg/daemon/@candidate", "digest": "sha256:…"},
    "decision": "deny",
    "reducer": {"link": "context entry 2", "index": 2, "language": "builtin", "type": "subset", "dimension": "operations", "allow": ["fetch"]},
    "reason": "push ∉ {fetch}"}
   ```

3. The run goes on. A denial is an answer to one request, not a
   refusal of the run. At close, `eu` records `denials: 1` for that
   entry in the run's `capabilities`, and the hub's run view links the
   audit entry. An Anselm `use` reducer's denial is logged the same way,
   with `language: anselm`, its `id`, and a digest of its inputs.

Commits: **eu**, none for the denial; the count goes in with the run's
close.

## 9. How the task interface reduces


| hub-interface.md today | With task repos |
| --- | --- |
| `hello` / `welcome` / `heartbeat` / `goodbye` | unchanged (the registry and slots stay live) |
| `offer` (task + attestation) | Silo's commit of a placement (with `attestation`) in the Grain's folder of its placements repo + `{"type":"pull"}` |
| `resume` | gone: the placement is in Silo's repo, the Grain's config names its folder as a lane, and a returning Grain pulls |
| `accept` / `refuse` | the Grain's commit of an execution record in `accepted` / `refused` + `refusal` |
| `result` | the Grain's commit of the record's `done\|failed` + `closed` |
| `cancel` | Silo's commit of `cancelled` + `cancel` on the task, + `pull`; the run's `cancelled` commit |
| re-placement (today a `cancel` to one Grain and an `offer` to another) | Silo's one commit moving the placement between Grains' folders; the old run is fenced |
| a user's reply to a running agent | relayed over the node connection; the Grain commits it in its root |
| `propose_task` (upward filing) | unchanged as a request; Silo makes the intake commit |
| event stream, view stream, http stream | unchanged (runtime; never in git) |

The contract becomes:

- the repo schema (§3, §10);
- `grain.config.md` and its typed roots, the kind suffixes, the
  resolver's listing rules (`ignore:`, `archive/`) and lanes (§3.3–§3.8,
  §5.2);
- the layout of Silo's folder for a Grain: `backlog/` and `triggers/`;
- triggers and their firings, and gates (§6.2, §6.3);
- a task's `context:`, its resolution and limits, and the sandbox layout
  (§5.6);
- capability objects, their chains and reducers, the base types a Grain
  implements and its published base declarations (§5.7);
- the `pull` notification.

A customised Grain conforms by reading and writing those files. Until
stage 4 of §16, both paths run, and the WebSocket messages are derived
from the commits.

## 10. Schemas and versioning

Each kind has a JSON Schema (2020-12) in
[`examples/task-repos/schemas/`](../examples/task-repos/schemas/):
`common` (shared definitions), `config`, `task`, `ref`, `run`, `comment`,
`template`, `trigger`, `gate`, `suite`, `design`, `memory`, `moved`,
`asset`, `capability` and `capability-base`, plus `grainconfig` for the
`.grainconfig` YAML file. `GRAINOWNERS` is not YAML and has no JSON
Schema; its grammar is §3.5.

*For 4487:* `repo`, `include` and `exclude` are gone. `config` is new:
`grain` (or, in a team's intake, `team`), `owner`, `backlog`
(`mode`, `expedite`, `lanes` of `{name, path, ref_mode, weight, ignore}`),
`triggers` and `memory` (lists of `{path, ref_mode, ignore}`, and
`write` on a memory entry) and `limits`. `memory` is new: `title`,
`applies_to` (`repos`, `teams`, `kinds`), `updated_by`, `updated_at`,
and a body. `grainconfig` takes over the old `repo` schema's repo-wide
fields: `owner`, `host`, `default_ref`, `ref_policy`, `aliases`,
`task_home`, `grain_folder`, `orphans`, `limits`, `archive_repo`,
`moved_to` and `require_signed`; `writers` moves to `GRAINOWNERS`.
Every schema's `kind` must equal the file's suffix (§3.2), which the hook
and lint check outside JSON Schema.

*For 4479:* `schedule` is gone, folded into `trigger`,
which is rewritten: `on` (one or more of `schedule`, `push`, `merge`,
`design-change` and `manual`, each with its own options, and
`task-closed` reserved and refused), `fires` (a template with `inputs`,
`bind`, `context` and `per`, or a `workflow` with its `branch` and
`review`), `fires_in`, `limits` and `derived_from`; and it refuses the
merge-gate keys (`gate`, `human`, `sensitive`, `paths`, `requires`).
`gate` is new: `match` (`paths`, `paths_ignore`, `branches`, `repos`),
`requires` (`human`, `security_review`, `second_review`) and
`sensitive`, refusing every trigger key. `task`'s `schedule` becomes
`trigger: {ref, on, key, fired_at, event}`, and `submitted_via` gains
`trigger` and `gate` in place of `schedule` and `planning`. The principal
`silo:schedule` becomes `silo:trigger`. `ref` gains
`fired_at` for inline firings (§6.2). A trigger has `derived_from`.
`run`'s `superseded` is written by the Grain and records `withdrawn_at`
(§8.8).
`task` and `template` replace `repo` and `base` with `context` (§5.6), and
`template` inputs gain `type: ref`; a trigger has neither, and may
carry `fires.context`. `run` gains `context` (what was materialised and
skipped) and `waiting`, and `refusal` gains `entry`. `comment` gains
`attachments`. `asset` is new: a sidecar (§2.2). The shared definition of
a `context:` entry is in `common`. *For 4456:* `capability` is new: a base
reference, a reducer chain (`language: builtin` with `type` and
parameters, or `language: anselm` with an opaque `version` and `body`),
an optional `approval`, and secret slots bound by name. So is
`capability-base`, the declaration a Grain publishes for each base it
implements: its dimensions with their maximums and where each narrows,
what it materialises, what it implies, its proxies, its secret slots and
its approval. A `context:` entry gains `mode: use` and `reducers`; `task`
loses `capabilities` and gains `capability_approvals`; `run` gains
`capabilities`, and its `refusal` and `waiting` name a capability's
link, reducer or secret (§5.7).

- **Every file names its schema** (`grain/task@v1`). A minor addition,
  an optional field, keeps `v1`. Readers ignore fields they do not know
  and preserve them when they rewrite a file.
- **A breaking change is `v2`.** Loaders keep a decoder for every
  version they ever accepted, and upgrade on read. Old files therefore
  keep loading, and are never rewritten just to bump the version.
- **Silo's hook takes the newest version both ends know.** It refuses a
  push in a version newer than it knows. A Grain advertises the
  versions it knows in `hello`, and Silo writes nothing newer into its
  repo.
- Front-matter keys are `snake_case`, like the hub's JSON. For `template`
  and `trigger`, the loader also accepts the hyphenated keys of
  `.grain/` declarations (`auto-merge`, `auto-approve-proposals`) as
  aliases, so those keys move over verbatim (§12.1). The `on:` sources
  keep Actions' and grain's hyphenated names (`design-change`,
  `task-closed`).
- **Two places refuse what they do not know, rather than ignoring it:**
  a trigger's `on:` source and a gate's `requires:` key. A trigger with
  an unknown source does not fire, and a gate with an unknown
  requirement holds every change it matches until a reader that
  understands it is deployed. Ignoring either would silently change what
  runs or what lands.

## 11. Security

This adds a boundary to [threat-model.md](threat-model.md): **pusher →
Silo git host**. It also changes TB3 (hub → Grain) from WebSocket
messages to commits plus notifications.

1. **The enforcement point is the pre-receive hook** (§8.1). It runs in
   Silo's process and reads the identity that authentication
   established. Commit author and committer fields are display text and
   are never trusted.
2. **Identity on push:**
   - humans authenticate with IAP (the `x-goog-iap-jwt-assertion` header,
     as the API does);
   - CLI users use an IAP-issued token through a git credential helper
     (`silo auth git-credential`);
   - Grains use their node token, bound to the node's owner (hub.md
     §5.2).

   Directory roles are evaluated at push time.
3. **Replay and history rewriting.** Force pushes and ref deletion are
   refused on protected refs. A replayed old commit is either already an
   ancestor (a no-op) or not a fast-forward (refused). Re-pushing old
   *content* in a new commit is judged as a new change against the
   current file: it is a state transition like any other, and allowed
   transitions never go backward from a terminal state.
4. **Reference resolution is authorized.** A reader follows a reference
   only with its own credential. Nothing pulls in a repo the reader is
   not authorized for. Silo grants a Grain read on its own folder in the
   team's placements repo, and on exactly the repos its placements need:
   the task's own repo, the repos the task references (template,
   template includes, designs), and the Grain roots holding its
   dependencies' execution records (§8.9), for as long as the placement
   exists. Following an include is a read like any other (item 18). Silo cannot grant read on a code repo it does not host:
   a Grain reads a task or definition there with its own credential, as
   it must to work on that repo anyway. The hook checks new references

   with the pusher's rights, so nobody can use a reference to make Silo
   reveal a repo the pusher cannot read. Included files render only
   under the same check.
5. **Template injection.**
   - Inputs are data. Rendered output is never rendered again.
   - The subset has no access beyond inputs (§6.1).
   - Only `title` and the body are rendered, so a template can never
     render itself capabilities, `reads` or approvals.
   - What remains is prompt injection into the agent. That is the
     existing TB7 concern, bounded by the Grain's local policy.
6. **Secrets are never stored.**
   - A capability object's `secrets: {token: ghcr/publish-token}`, a
     workflow's `capabilities: [gcp-key, repo-write:bwsalmon/grain]` and
     `secret: github-app/app-id` are **names**. They resolve only in the
     Grain's local policy and secret store, or in Silo's grant table
     (§5.7, item 33).
   - A capability in a task file is a **request**, a `use` entry in its
     `context:`. Its grant is Silo's claim, carried in the attestation's
     `granted_capabilities` with the digest of its resolved chain, and the
     Grain's local policy has the last word (items 27–35).
   - The hook rejects files that match secret patterns.
7. **How a Grain verifies a file came from Silo.**
   - It fetches over TLS from the URL its own config names.
   - A backlog reference carries Silo's signed `attestation` (hub.md
     §6.4). Its `aud` is this Grain, its `task` is the task's canonical
     global identity, and its `spec_digest` covers the task's canonical
     spec at `spec_rev`: its spec fields, with every reference in its
     canonical form (§2.1), so a move that re-expresses a reference for
     its new home leaves the digest unchanged (§5.5). A Grain whose policy names `silo_issuer` refuses
     a reference without a valid one.
   - **A redirect cannot change what was placed.** If the task has moved
     since, the Grain follows the tombstone and recomputes the digest on
     what it finds. A spec that no longer matches is refused, so a
     tombstone can only ever lead to the same task.
   - Any Silo-owned field that changes later (a task's `cancel`, a
     placement's removal, which fences a run) is accepted only if it is
     signed or attested the same way, or read from Silo's own repo over
     the same TLS connection.
8. **The accepted risk, and the upgrade.** Today a Grain trusts Silo's
   claims (hub.md §1.2). With task repos it also trusts Silo's hook to
   have checked who pushed. **Signed commits are the natural upgrade.**
   - The repo's `.grainconfig` lists each principal's SSH or GPG
     signing keys.
   - Approvers sign their approval commits.
   - Grains verify that a task's `approved_by` change is in a commit
     signed by that approver's key.

   That makes approval verifiable end to end, with no need to trust Silo,
   which the JWT alone cannot do. It also covers repos on other hosts.
9. **A rogue writer** can only do what its rules allow. Examples:
   - a compromised Grain can change only its own root: its execution
     records, its own lane and its memory. Its owner's `grain.config.md`
     is not the Grain's to write (§3.5). It cannot write any task,
     so it cannot rewrite what it was asked to do. It cannot write a
     placement, so it cannot claim it was asked, and it cannot write
     another Grain's records, so it cannot claim another Grain's work;
   - Silo, or a credential for it, can write placements and the intake,
     and nothing in any Grain's root. It cannot rewrite a Grain's history,
     forge its runs, or make anything active on a Grain beyond the
     references and triggers in the folders that Grain's config names;
   - a compromised member can reorder the team's unplaced work, and
     nothing else;
   - a compromised approver can approve, and the commit names them.

   Every write is an attributable commit, and the hook logs every refusal
   to the audit log.
10. **A malicious design edit** can file at most a capped number of
    *proposed* tasks, which still need approval (§7.4), through the
    `design-change` triggers that watch it, under the flood limits of
    item 36. Approval itself
    can only be given by an approver other than the task's submitter.
11. **Content is untrusted where it is displayed.** Silo renders Markdown
    from these files as it renders agent-written text today (TB4).
12. **Code repos are trusted as their merge process is** (§8.13). This
    adds no boundary to the threat model: a code repo's `default_ref` is
    already trusted for the code an agent works on. What is new is that
    Silo and Grains read objects from it. The model:
    - Silo's hook does not run there, so nothing in a grain folder is
      attributed to a pusher. An object is as trusted as the repo's
      review made it, and no more.
    - Silo validates every object it reads from a code repo (schema,
      paths, references, ID allocation, the §5.5 invariants) and uses
      none that fails.
    - The design's limits do not depend on the repo's `GRAINOWNERS` or
      `.grainconfig`, which anyone who can merge may edit (§8.13).
    - Grains apply local policy as PEPs. A placed task's spec must match
      the attestation's digest, and a Grain's policy may refuse code
      repos it does not trust for templates, triggers or designs.
    - **Disclosure.** A task homed in a code repo is readable by
      everyone who can read that repo. The team's `task_home.allow`
      decides whether a code repo may choose that for itself, so a team
      with a private intake can keep its prompts out of a public repo
      (§5.5).
13. **A malicious `.grain/` change in a code repo**, merged by someone
    who can get a change through that repo's review, can do no more than
    this:
    - **Edit a homed task's spec after approval.** The digest no longer
      matches the intake's approved spec, or the attestation's
      `spec_digest`. Silo stops placing it and reports it, and a Grain
      refuses it. The commit that did it is in the repo's history.
    - **Add a task, `state: approved`, by hand.** No intake tombstone
      points at it, so Silo never places it (§5.5).
    - **Forge `moved_from`** to claim an intake origin. Silo checks the
      other end: the intake path must be a tombstone pointing back, and
      only the team's admins and Silo can write a tombstone in the
      intake, under the hook (§8.1).
    - **Edit a template or trigger.** A task already filed pins
      `template.rev`. A trigger renders its template's tip, so a firing
      after the edit uses it. That firing is filed `proposed`, and an
      approver reads the rendered prompt, unless the trigger root that
      names it is pinned to a reviewed commit: then an edit does not reach
      the team at all until an admin re-pins it. What remains is prompt injection, bounded by
      local policy (item 5).
    - **Grant itself access.** A template, trigger, gate or suite
      with `reads`, `writes`, `grants` or `target`, or with a `context:`
      entry or `ref` input naming another repo, is refused (§12.1). A
      capability named in a homed task is a request, granted only in
      Silo's attestation (item 6). A capability object it adds under
      `.grain/` is honoured by no Grain whose policy does not sanction
      that folder as a source (item 30).
    - **Flood a team.** A trigger naming a team the team has not adopted
      never fires (§6.2), and an adopted one is rate-limited (item 36). Firings never land in the code repo itself. A
      design edit files at most a capped number of proposals (item 10).
      Silo reads at most 1,000 changed files per code-repo push, as the
      hook caps pushes (§14).
    - **Widen its own rules.** It can edit its `GRAINOWNERS`, which is
      advisory there, and its `.grainconfig`'s `task_home`, which the
      team's `allow` bounds. It cannot make anything active on a Grain:
      only Grains' configs do that.
    - **Reference a private Silo repo** to make Silo reveal it. Silo
      resolves code-repo references with the rights of the principal
      using them, never its own (item 4).
    - **Cancel a homed task.** That is honoured, because it only stops
      work, and the commit names who merged it.
    - **Weaken its own gates.** A change that edits or deletes a gate
      is held by the gates at the target branch's tip, which still
      include the one it edits (§6.3). Once merged through them, the
      edit is the repo's own policy, as any merged code is. It cannot
      weaken a mandatory gate source, which the Grain reads itself.

    The repo's own defence is to review `.grain/**` as strictly as it
    reviews code that touches credentials, which bwsalmon/silo already
    does with a path trigger that requires a human (`grain-declarations`),
    a gate in this design (§6.3).
14. **A root entry is a trust grant, and only the Grain's owner makes
    one.** Adding an entry to `grain.config.md` says "act on what this
    folder holds, of this type". **Nothing Silo writes can activate
    anything on a Grain unless the Grain's owner pointed a root at it**:
    Silo can fill its own per-Grain folder, but a folder no entry names
    is never listed, and `GRAINOWNERS` keeps `grain.config.md` the
    owner's, never Silo's (§3.5). That is the PEP property of §1, and it
    holds for every root type:
    - **The type narrows it.** A backlog entry lists `*.ref.md` only, so
      Silo's placements can only ever be references, whatever that folder
      holds; a trigger entry lists `*.trigger.md` only, so a team's
      triggers folder cannot bring a gate, a capability or a reference.
    - **References are not grants.** A task's template, a capability
      definition or a design is read when something already active needs
      it, with the reader's rights, and makes nothing active (§3.1). A
      foreign template is still data (§8.10).
15. **Nothing outside an entry can change what it activates.** An entry
    activates its folder's subtree, less its own `ignore:` and any
    `archive/`. There are no ignore files and no inherited patterns, so
    no file in a parent folder, in another repo, or written by another
    principal can hide or add an object for that entry. A producer can
    change its folder's contents (that is its right: the entry trusts
    that folder), and nothing else can.
16. **The owner's `ignore:` is the final word for their entries.** A
    Grain can cut any object a root would activate, and no producer can
    undo the cut except by renaming the object, which the owner's
    status shows as a new arrival. Gates are not cut this way, because
    they are not in any root (§6.3, item 39).
17. **Listing is bounded and fails loudly.** An attacker who controls a
    folder a Grain names can fill it, but cannot make the Grain read more
    than `objects_per_entry` names from it, or more than `objects` in
    total: over a limit the entry is unknown, and nothing is truncated,
    so the attacker cannot choose which of its objects survive (§3.7).
    Entries are not transitive, so there is no chain to bound and no
    cycle to cut. References are bounded per resolution (8 hops, cycle
    detection) and by `context:`'s limits (§5.6).
18. **Listing and resolving are reads, with the reader's own
    credential.** A Grain lists its entries and resolves references with
    its node credential, Silo with the rights of the principal it acts
    for, and nobody with Silo's own. An entry naming a repo the reader
    cannot read is unknown, not an error that widens anything (§3.7).
    The hook checks a new entry's folder with the pusher's rights, so
    `grain.config.md` cannot be used to make Silo reveal a repo, and
    Silo lists a Grain's entries for its views with the Grain's grants,
    not its own.
19. **`context:` is the sandbox's whole world.** A run sees its task file,
    its execution record and thread, and the entries of `context:`, and
    nothing else: no other repo, no other file, no reference that is not
    an entry (§5.6). Every other reference field is metadata, so a task
    cannot reach a repo by depending on a task there, implementing a
    design there, or using a template from there. The list is part of the
    spec: the approver reads it, the hook freezes it on approval (§5.1),
    and `spec_digest` covers it in canonical form (item 7). A Grain that
    materialises more than the list is a bug in the Grain, not a grant.
20. **The proxy's scope is exactly the resolved set.** The sandbox's git
    proxy serves the repos and folders the run's plan holds, at the
    commits it resolved, and refuses everything else (403, "not in scope
    for this sandbox", as today). For a **folder** entry it serves the
    commit and its trees with `--filter=blob:none`, and serves blobs only
    from the folder's subtree at that commit, so a sparse checkout is
    enforced by the proxy, not merely chosen by the checkout. The rest of
    the repo's **file names and commit messages** are still visible in
    those trees and commits. A policy that must hide them grants no
    folder of that repo, or serves the folder from a repo of its own.
    Files and external artefacts are not served by the proxy at all: the
    Grain copied them in.
21. **The PEP checks before it fetches.** The Grain's policy sees every
    entry, every entry reached through `depth`, every redirect hop, every
    LFS endpoint and every external URL, before the Grain reads a byte of
    it, and before anything enters the sandbox (§5.6). A listed entry it
    refuses stops the run with the entry named; a reached one it refuses
    is skipped and recorded. Neither is fetched. This is task 4303's fix
    made general: a capability reaches a repo only by declaring it, and
    the declared repo is checked as an entry is (item 31), so there is no
    second, unseen route by which a repo reaches a sandbox.
22. **`write` is the task's branch, and nothing else.** A `write` entry
    lets the run push `grain/<grain>/<run name>` to that repo, created
    from the entry's base branch. The proxy refuses any other ref, any
    tag, any deletion outside that branch, and, for a `write` folder,
    any commit that changes a path outside its `write` folders. Landing
    the branch is the merge queue's job, under the repo's own rules, as
    today. `write` on a file is refused on push, and by the Grain if it
    gets that far.
23. **Transitive reach and archive bombs are bounded.** `depth` is off by
    default, at most 3, follows body links only, and stops at
    `reached_files` and `reached_bytes`. Every file is capped at
    `file_bytes` and the set at `files_bytes`, counted as fetched, not as
    declared (§5.6). Checkouts are capped at `checkout_bytes` as inflated
    on disk, so a pack of highly compressible blobs aborts the clone
    rather than filling the disk. The materialiser never unpacks an
    archive: a `.zip` or `.tar.zst` asset arrives as the file it is, and
    an agent that unpacks it does so inside its own sandbox's quota.
    External artefacts are downloaded with their declared `bytes` as a
    hard limit, and checked against `sha256` before they are moved into
    place.
24. **Assets are data, never executed by Silo or a Grain.** Neither runs,
    imports, opens with a plugin or interprets an asset. The Grain's
    staging clones run with hooks disabled, no `filter.*`, `core.fsmonitor`
    or `core.hooksPath`, and no smudge filters but LFS's own, so a
    hostile `.gitattributes` or config cannot run anything while it
    fetches. Copied files are read-only with their executable bits
    cleared, a symlink is never followed out of its checkout, and a
    single-file entry that is a symlink is refused. Whatever the agent
    then does with an asset is the agent's, inside its sandbox, bounded
    by the Grain's policy, as with any repo content today (TB7).
25. **Assets are untrusted where they are shown.** Silo's UI and the
    Grain's node UI serve an asset by content type, decided from an
    allowlist and checked against the bytes, never taken from the
    asset's name or a sidecar alone:
    - **inline** only for raster images (PNG, JPEG, GIF, WebP), after the
      server checks their dimensions (at most 50 megapixels) and
      re-encodes a thumbnail, so a decompression bomb never reaches the
      browser;
    - **never inline: SVG, HTML, XML, PDF**, or anything else. SVG and
      HTML can carry script. They are served as downloads
      (`Content-Disposition: attachment`), or SVG is rasterised by the
      server, sandboxed, without network or scripts;
    - always with `X-Content-Type-Options: nosniff` and a
      `Content-Security-Policy: sandbox` response, from a separate origin
      for user content, never from the origin that holds the UI's
      session;
    - Markdown is rendered as it is today (item 11), with image links
      rewritten to that origin, and never to an external URL, so that
      viewing a design does not report to a third party.
26. **`context:` cannot be used to read what its author cannot.** Silo's
    hook checks every new or changed entry with **the pusher's** read
    rights, as it checks every reference (item 4). Silo grants a Grain
    read on the Silo-hosted repos a placement's `context:` names, for as
    long as the placement exists, and no more. A repo on another host is
    read with the Grain's own credential, which its policy already
    governs.
27. **Monotonicity is the core invariant.** A capability's effective
    authority is its base's maximum on this Grain, intersected with every
    reducer in its chain and in the task's entry (§5.7). Nothing in git
    adds authority. Only two things are sources of it: **base
    capabilities**, which are code the Grain runs, and **secrets**, which
    are the Grain's own and which policy allowlists by name (item 33).
    - The schema gives a capability object no field that grants
      (`maximum`, `implies`, `grants`).
    - Built-in reducers are monotone by construction. Every Anselm
      result is checked to be within its input, and one that is not is a
      deny and a reported bug.
    - A reducer that cannot be evaluated (an unknown language or
      version, Anselm before adoption) or cannot be enforced (an
      undeclared dimension, or the wrong `on`) **refuses the request**.
      Skipping a reducer would widen the capability, so no reducer is
      ever skipped, truncated or approximated.
    - A Grain never reads a published base declaration, its own
      included, to decide what to grant. Its providers decide, so a
      commit to `capabilities/bases/` cannot widen anything.
28. **Monotonic within a resolution, bound over time by the digest.** An
    upstream owner can widen their own object later, as a template's
    author can change a template. What stops that reaching an approved
    task unseen is the **digest** of the resolved chain: the base type,
    its declaration's commit, every link's identity and commit, and the
    entry's reducers. Silo's `granted_capabilities` and every
    `capability_approvals` entry name it. A Grain that computes a
    different digest refuses the claim, so a changed chain needs a new
    grant, and a new approval where one is required. A policy may also
    require pinned links (`capabilities.require_pinned`).
29. **Referencing never grants.** A capability definition is reached
    only from a task's `context:`; there is no capability root, and
    Silo's catalog is a view for authors (§5.7). A grant needs all of
    these: a task's `use` entry, frozen on approval; Silo's claim for a
    placed task; and the Grain's policy admitting the base, every source
    in the chain, every secret and every implication.
30. **Definition sources are allowlisted, for every link.** The Grain's
    policy lists the repos and folders whose capability objects it
    honours, and for which bases (`capabilities.sources`). Only the
    Grain's own root is sanctioned by default. Every link of a chain
    must pass, checked before its repo is read, so a sanctioned folder
    cannot launder a definition from an unsanctioned one (§8.17). A code
    repo's grain folder, Silo's placements and a team's intake are not
    sources unless the owner says so. A compromised shared repo can only
    redefine the capabilities it was sanctioned to define, within the
    bases it was sanctioned for, and every such change alters the digests
    of the chains that use it (item 28).
31. **Implications are declared and checked.** A base's declaration names
    the repos, egress and tools a use reaches, as narrowed by the
    capability. The policy checks each one with `repos:`, `network` and
    `tools` before anything is minted or materialised, and refuses
    closed, naming the implication. This replaces `model.CapabilityRepos`,
    a mapping the policy could not see, with part of the contract. A
    base that reaches something its declaration does not name is a bug
    in the base, and the proxies' scopes are built from the declared
    implications, so it cannot reach it through them.
32. **Every decision is auditable.** Each credential narrowing at
    materialisation, and each per-use allow or deny at a proxy, is
    logged to the Grain's audit log with the run, the capability's
    canonical identity, digest and link, the reducer's index, `id` and
    language, the decision, its reason, and a digest of the request. It
    never includes secret material. The run's record keeps the summary
    (its `capabilities`, and `denials` at close), and the log stays
    runtime, as events do (§1.2). Silo's views link a refusal or denial
    to the reducer that made it.
33. **Secrets are resolved by name, at materialisation, by the Grain.**
    Capability objects name secret slots' bindings, and never hold
    material. The hook's secret-pattern scan (§8.1) applies to them as to
    every file. The Grain resolves the names from its own store when it
    materialises, into the file its base declares, and nowhere else: not
    git, not `run.md`, not events, not Silo. Because a secret is
    authority the base's maximum does not bound, the policy allowlists
    the names each source may bind. A missing secret refuses the run with
    the name, never part of it (§5.7).
34. **What a Grain trusts Silo for, and what it does not.** Today a Grain
    trusts Silo's claims (item 8, hub.md §1.2). For capabilities, Silo's
    claim covers **who approved**: the task's `approved_by`, its
    `capability_approvals`, and the `granted_capabilities` in the
    attestation. It does not cover **what authority exists**. The Grain
    resolves the chain itself, from git, under its own source
    allowlist, against its own providers, so a compromised Silo cannot
    make a Grain honour a base, source, secret or implication its policy
    does not admit. A compromised Silo could still claim an approval
    nobody gave, for a capability the Grain does admit. That is the
    existing accepted risk. Signed commits upgrade it here as for
    `approved_by` (item 8). For the bases where the owner will not accept
    it, the policy's `approval.local` makes the Grain's owner the only
    approver, per run, and Silo's claim never satisfies that.
35. **Reducers run sandboxed and bounded.** A per-use reducer runs inside
    the Grain's proxies, on every request, so the properties §5.7
    requires of Anselm are security properties: no side effects, no
    network or filesystem, determinism, a size limit and an evaluation
    budget whose exhaustion denies. A hostile reducer can therefore only
    deny, which a run's author can already do by not requesting the
    capability. Its only cost is the budget, which the Grain sets.

*For 4479, triggers and gates:*

36. **A trigger is a way to file work, so it is rate-limited.** Every
    firing is a task filed in someone's intake or root, and a cheap
    event (a push, a design save, a click) must not become an expensive
    flood. The limits, each of which a trigger's own `limits:` or the
    design's `planning:` can lower but never raise:
    - per trigger: at most 24 firings an hour and 200 a day by default
      (`limits: {per_hour, per_day}`); a firing over the limit is not
      filed, and the trigger is **suspended** in the database, with the
      reason in Silo's or the Grain's health view, until an owner
      resumes it;
    - per team, in Silo: at most 200 trigger-filed tasks a day across
      all of its triggers, and 50 a day from `design-change` triggers
      (§7.4); per Grain, the same caps over its own root, set in its
      local policy;
    - per event: `push:` and `merge:` with `group_by` file at most 20
      firings for one push, and `design-change` at most
      `max_tasks_per_change` tasks (default 10, at most 50) before it
      files one plan proposal instead; `debounce` coalesces bursts;
    - `catch_up: all` fires at most `max` (≤ 24) missed firings, so a
      Grain back from a month away does not file a month of work;
    - `manual:` requests are limited per requester (at most 10 an hour
      per trigger) as well as by the trigger's own limits.

    A suspended or rate-limited trigger fails closed: it files nothing,
    and says so.
37. **Who may author triggers, and where.** A trigger fires only where
    its definition was admitted by the writer of the folder that holds
    it, and only for a team that chose it:
    - in a **Grain root**, only the Grain and its owner (§3.5); it fires
      on that Grain only;
    - in a **shared repo**, the team's maintainers, or a review branch an
      approver merged (§8.11); a `fires_in: silo` trigger fires only for
      the team the repo belongs to, and a `fires_in: grain` one only on
      a Grain whose trigger root names its folder (Silo's per-Grain
      folder, for an assigned trigger);
    - in an **intake**, only the team's admins, as the team's own
      triggers or as entries in the intake's `grain.config.md`
      (adoption);
    - in a **code repo**, whoever its review lets merge (§8.13); it
      fires only where a trigger root names it, and its firings never
      land in the code repo (§6.2).

    `push:` and `merge:` may watch only protected refs, so a contributor
    cannot fire a trigger by pushing a branch of their own. A firing
    caused by a person (a push, a merge, a `manual:` request) is
    approved no further than that person could approve (§6.2), so a
    trigger with `auto_approve_proposals` does not turn a non-approver's
    push into approved work. And a trigger grants nothing: its firing is
    a task, held to the Grain's policy like any other (items 21, 29).
38. **A gate is policy of the repo it guards, and no Grain can weaken
    it.** Gates combine by conjunction: there is no negative gate, and no
    key in any object releases another's hold (§6.3). A gate is never in
    a Grain's roots, so no entry, no `ignore:` and no `archive/` move in
    someone else's folder adds or removes one; the merge queue reads the
    gates in the merged-into repo's own grain folder, at the target
    branch's tip, so the change it holds cannot edit them away. And a
    trigger never holds or releases a merge: the two kinds share no
    keys, and their schemas refuse each other's.
39. **Mandatory gate sources are read directly.** The Grain's local
    policy (and Silo's, for the repos it hosts) may name mandatory gate
    sources, which the merge queue and the hook read themselves; a
    mandatory source that cannot be read holds every change it might
    cover. So an org's "every `**/*.tf` change gets a security review"
    holds wherever it is mandatory, whatever any Grain's config says.
40. **A gate's review tasks are bounded too.** A gate files at most the
    tasks its `requires` names, once per change per gate (their paths
    are derived from the change and the gate, so a re-push files no
    more), and they count against item 36's caps. A gate that cannot
    file its review, because a cap is reached or its template does not
    resolve, holds the change rather than releasing it.

## 12. Grain folders in code repos: `.grain/` and `docs/design/`

bwsalmon/grain's `.grain/` declarations (`docs/declarations.md`,
`docs/declaration-schema.md`) **converge on the task-repo format**: each
becomes a Markdown file with front matter, read by the same parser as
every other object, in a grain folder governed by `.grain/README.md`
(§3.1). Workflows are the one exception (§12.2). Templates and suites
keep exactly the keys `pkg/declschema` defines for them today. Schedules
and triggers do not map one to one: a schedule becomes a `kind: trigger`
with `on.schedule`, and a path trigger becomes a `kind: trigger` if it
files work, or a `kind: gate` if it holds a merge (`gate: true`). Their
keys are re-nested, not reinvented, by the mapping in §13.1. The loader
accepts the hyphenated spellings (`auto-merge`) as aliases of the
`snake_case` ones (§10).

### 12.1 The mapping

| `.grain/` today | Becomes | `kind:` | Front matter | Body |
| --- | --- | --- | --- | --- |
| `templates/<name>.yml` | `templates/<name>.md` | `template` | every template key, verbatim; `fields` become `inputs` (§13) | the prompt, now Markdown rather than a YAML block scalar; `{{name}}` placeholders are already Jinja (§6.1) |
| `schedules/<name>.yml` | `triggers/<name>.md` | `trigger` | `on.schedule` holds the cadence, `timezone`, `overlap` and `catch_up`; `fires` holds the template (or `workflow:`) and its inputs; the rest stay top-level (§13.1) | notes: the YAML file's leading comment |
| `suites/<name>.yml` | `suites/<name>.md` | `suite` | every suite key, verbatim | the suite's description: the leading comment |
| `triggers/<name>.yml` that files work | `triggers/<name>.md` | `trigger` | `on: push` with `paths` becomes `on.push: {branches, paths, group_by}` (or `on.design-change` for the design-doc planner); its template becomes `fires.template`, a reference, and its variables (`{{group}}`, `{{from_sha}}`) become that template's declared inputs, bound by `fires.bind` (§13.1) | the leading comment |
| `triggers/<name>.yml` with `gate: true` | `gates/<name>.md` | `gate` | `paths` becomes `match.paths`; `human` and `sensitive` become `requires` and `sensitive` (§13.1) | the leading comment |
| `workflows/<name>.yml` | unchanged | — | — | — |

The new kinds' schemas are
[`suite.schema.json`](../examples/task-repos/schemas/suite.schema.json),
[`trigger.schema.json`](../examples/task-repos/schemas/trigger.schema.json) and
[`gate.schema.json`](../examples/task-repos/schemas/gate.schema.json).
[`widgets-code/.grain/gates/grain-declarations.md`](../examples/task-repos/widgets-code/.grain/gates/grain-declarations.md)
is bwsalmon/silo's own `.grain/triggers/grain-declarations.yml`,
converted. One spelling needs care: front matter should quote the key
`"on"`, because a YAML 1.1 reader (PyYAML) reads a bare `on` as `true`.
The Go loader, on YAML 1.2, accepts both. A gate has no `on:` at all.

- **Identity carries over.** A declaration's existing name,
  `repo:<kind>:<owner>:<repo>:<name>`, comes from its name and not its
  file extension, so it is the same for the `.md` file, and goes in its
  `aka:`. Settings rows in grain/config that cite it keep working. A
  schedule's stays `repo:schedule:…` in the trigger's `aka:`, and a gate
  keeps its `repo:trigger:…`: the old kind in the name is history, not
  a claim about the object.
- **The refusals of `docs/declaration-schema.md` still hold, for every
  kind:** a repo declaration carries no `reads`, `writes`, `grants` or
  `target`, and its `context:` entries, and its `ref` inputs' defaults
  and values, name only its own repo (§5.6). The schemas say so, and readers enforce it on every object
  in a code repo's grain folder, whatever its README says (§8.13). A job
  that needs another repo's access stays a settings-owned row.
- **A code repo may define capability objects** in its grain folder
  (`.grain/capabilities/`), and they grant nothing there either: a Grain
  honours one only if its policy sanctions that folder as a source, and
  no policy does by default (§5.7, §8.17).
- **One parser.** The `taskrepo` package's front-matter reader (§16)
  serves `pkg/repodecl` for `.md` declarations, Silo's code-repo reader,
  and `grain declarations check`.

### 12.2 Workflows stay YAML

**Decision: `.grain/workflows/*.yml` stay GitHub Actions YAML,** and are
not `kind:` objects. The reasons:

- **They must stay Actions-compatible.** Grain runs the same dialect
  GitHub does (`on:`, `jobs:`, `steps:`), and the same file can run on
  GitHub. `actionlint`, editors' workflow schemas and Dependabot's action
  updates all read it. A Markdown wrapper would break each of them, and
  would have to be unwrapped before any of them could run.
- **They are programs, not prose.** A workflow has no body for Markdown
  to carry. What the format buys for templates (a reviewable prompt
  rather than a YAML block scalar) buys nothing here.
- **What one parser would give is not needed for them.** Nothing names a
  workflow with a `kind:`-checked edge. A suite or a trigger that names
  one (`fires.workflow`) uses its plain path (`.grain/workflows/hub.yml`),
  resolved as a file, like a reference into code. A workflow's
  `on.schedule` still runs it, as today: grain reads it as an implicit
  trigger of the workflow form, on that repo's Grain (§6.2).

The cost is that grain keeps its workflow parser beside the front-matter
parser. It has that parser already, and needs it for GitHub
compatibility anyway. A grain folder's `scope` covers `workflows/`, but
only `*.md` files with `kind:` are objects, so YAML there is never misread.

### 12.3 Design docs in place

Design docs already in code repos (`docs/design/*.md`, with `{#D-n}` or
`{#HS-n}` anchors) are referenceable as `kind: design` objects **where
they are**. Nothing moves:

- The repo's `.grain/README.md` lists `/docs/design/` in its `scope`, so
  it governs them (§3.1).
- Each doc gains this front matter, and nothing else:

  ```yaml
  ---
  kind: design
  schema: grain/design@v1
  title: Widget description format
  status: implementing              # as §7
  owners: ["user:bsalmon@google.com"]
  requirements: {prefix: WG, next: 3, retired: []}
  ---
  ```

  `applies_to` defaults to the repo (`["/"]`). `requirements.next`
  starts at one more than the highest ID in the doc, and `retired` lists
  the IDs that `Design-Removed:` trailers in the repo's history have
  removed. `planning:` is added only if Silo should plan the doc (below).
  A doc that already has front matter for another tool keeps its keys,
  and gains these beside them.
- **A doc without front matter is still referenceable** by path and
  anchor (`grain-code:docs/design/<name>.md#D-7`), as a plain file:
  its anchors resolve and its section hashes compute (§7.2). It is not an
  object, so readers check no ID allocation on it and Silo does not plan
  it.
- **Trailers carry over unchanged:** `{#D-n}` and `{#HS-n}` anchors, and
  `Design-Item:`, `Design-Rev:` and `Design-Removed:`. A doc in a code
  repo keeps its in-repo `Design-Item:` path, and a Silo-hosted design is
  cited by its ref. `pkg/designdoc`'s parsing becomes the shared parser.
- **ID allocation (§7.1) is checked on read**, because Silo's hook does
  not run in a code repo: by Silo when it reads the repo, and by
  `grain declarations check` in the repo's own CI, which can refuse the
  change before it merges.
- **Planning is one or the other.** A design in a code repo is planned by
  the in-repo planner, now a `fires_in: grain` trigger with
  `on: design-change` (`.grain/triggers/design-docs.md`), unless its
  front matter has `planning:`. With `planning:`, the team's
  `fires_in: silo` design-change trigger plans it on each change to
  `default_ref` that Silo reads (§7.4), and the in-repo trigger skips it.
  A design in a Silo repo is always planned by a team's Silo-fired
  trigger. Doing both is refused.

## 13. Migration

`grain tasks export` writes grain's database into task-repo files:

- tasks, with their comments;
- the backlog's order, as references;
- each dispatched task's runs, as execution records;
- schedules, as triggers, and templates, from the database or from
  grain/config `settings/*.yml` (§13.1 maps the fields).

**Rules:**

- **Stable paths, derived from the old id and a slug.** A task becomes
  `tasks/<legacy number>-<slug>/task.md`, such as
  `tasks/4310-carry-approver/task.md`. The slug is the title, lower-cased,
  with every run of other characters turned into `-`, cut to its first
  few words and at most 40 characters. Templates keep their declared
  names (`templates/prune-packages.md`), and a schedule's becomes its
  trigger's (`triggers/prune-packages.md`). An export run twice
  therefore produces the same paths. The legacy id goes in `aka:`.
- **Where tasks go.** A standalone Grain exports everything into its own
  root. For a Grain linked to Silo, tasks submitted at the hub are
  exported into the team's intake from Silo's database, their open
  placements into the Grain's folder in the team's placements repo, and
  the tasks the Grain authored itself stay in its root, with references
  in its own lane. `grain tasks export` writes the root's includes of
  Silo's folders, as `grain tasks link` does (§8.12).
- **The old backlog order becomes ranks.** The queued tasks, in the old
  dispatch order, each get a reference in `backlog/` with evenly spaced
  ranks (`"1"`, `"2"`, … `"z"`, lengthening as needed). `at_front`
  becomes `expedite: true`, and the old priority becomes the `priority`
  band.
- **Old runs become execution records.** Each task the Grain dispatched
  gets one `runs/<legacy number>-<slug>/run.md`:
  - its attempts are collapsed into the record, and stay in the database;
  - `accepted_at` and `started_at` come from the first attempt;
  - `state` and `closed` come from the final attempt, the pull request
    and the merge commit.

  A task that was re-placed between Grains gets one record per Grain,
  the earlier ones `superseded`. The task's comments go under the run
  they were made during, or beside the task if it was never dispatched.
- **Runtime columns are dropped.** `next-run-at`, `last-run-at`, a
  schedule's paused state and the attempt rows are not exported; they
  stay in the database (§6.2).
- **Templates.** A template's `fields` (`model.TemplateField`) become
  `inputs`.
- **Target, reads and writes become `context:`** (§5.6). For a task, in
  this order:

  | grain today (`model.Task`) | `context:` entry |
  | --- | --- |
  | `Target: owner/name`, `Base: b` | `{ref: "<forge URL of owner/name>@b", mode: write}`, first, so it is the primary repo |
  | each `Writes[i]: {repo, base}` | `{ref: "<URL>@<base, or Base>", mode: write}` |
  | each `Reads[i]: owner/name` | `{ref: "<URL>", mode: read}`, unpinned |
  | a `repo-write:owner/name` capability | a `write` entry for that repo, if none exists, and the capability is dropped: a repo reaches a sandbox only as an entry, or as a declared implication (§5.7) |
  | each other capability name (`gcp-key`, …) | a `use` entry for that name's legacy capability object (§13.2) |
  | each attachment | an asset beside the task and a `read` entry for it (§2.2) |
  | no `Target` (team-targeted) | no `write` entry |

  `<forge URL>` is the deployment's configured forge,
  `https://github.com/owner/name.git` for these deployments. Pasted
  context stays in the prompt, where it was; an author who wants it as a
  file attaches it instead.
- **Templates and schedules (now triggers)** with `repo` and `base` get an input `repo`
  of type `ref`, defaulting to that repo at that base, and
  `context: [{input: repo, mode: write}]`; a template's `reads` become
  `read` entries. A schedule's `target` and `base` become its trigger's
  `fires.inputs.repo` (§8.16).
- **Attachments** on tasks and comments become assets beside them, as
  §2.2 describes.

`grain tasks import` reads the files back. The test is the round trip:

```
export(db) → import → export  is byte-identical
```

For every git-backed field, the database fields match.

**Round-trip sample.** These are real rows from grain/config. `settings/schedules.yml` holds:

```yaml
- id: repo:schedule:bwsalmon:grain:prune-packages
  title: Prune GHCR packages
  template: repo:template:bwsalmon:grain:prune-packages
  weekly: {day: sunday, at: "04:00"}
  overlap: skip
  base: candidate
  auto-approve-proposals: true
  team: default
  target: bwsalmon/grain
  next-run-at: "2026-10-18T04:00:00Z"        # runtime: not exported
  last-run-at: "2026-10-11T04:00:01.752013Z" # runtime: not exported
```

It exports as the trigger
[`hub-shared/triggers/prune-packages.md`](../examples/task-repos/hub-shared/triggers/prune-packages.md),
by the mapping in §13.1: `weekly` and `overlap` under `on.schedule`,
`template` as `fires.template`, `target` and `base` as
`fires.inputs.repo`, and the runtime columns dropped.
The template exports as `templates/prune-packages.md`. The examples then
move it to
[`hub-shared/templates/ghcr/prune-packages.md`](../examples/task-repos/hub-shared/templates/ghcr/prune-packages.md),
leaving a tombstone at its old path, to show one (§2.1). The
settings-owned `template-1`/`sched-1` pair, which has `reads`, is
[`templates/granule-pull.md`](../examples/task-repos/hub-shared/templates/granule-pull.md).
Importing them yields the same rows minus the runtime columns, which the
scheduler recomputes from the cadence.

### 13.1 Existing `.grain/*.yml` declarations

Code repos move from `.grain/*.yml` to `.grain/*.md` one repo at a time,
and **both forms stay readable throughout**:

1. **Readers accept both.** `pkg/repodecl` loads `templates/`,
   `schedules/`, `suites/` and `triggers/` as `.yml`, and `templates/`,
   `triggers/`, `gates/` and `suites/` as `.md`, using the `taskrepo`
   parser for `.md`. A `.yml` schedule and the `.md` trigger converted
   from it, or a `.yml` gate trigger and its `.md` gate, are one name. If one name exists in both forms, the
   `.yml` is used and the conflict is reported by
   `grain declarations check`. So a half-finished conversion changes
   nothing that runs.
2. **A `.grain/` folder with no README** reads as before (§3.1). A repo
   adds `.grain/README.md` when it wants a `scope` beyond `.grain/`, a
   `task_home`, or aliases.
3. **`grain declarations convert`** rewrites a repo's declarations in one
   commit, which lands through the repo's normal review (a `.grain/**`
   change, so human-gated where the repo says so). For each file it
   writes the `.md` with the same keys, moves the leading comment into
   the body (and a template's prompt into the body), maps each schedule
   and path trigger to a `kind: trigger` or `kind: gate` by the tables
   below, turns template names into references, and deletes the `.yml`
   in the same commit. **No tombstones are needed:** nothing references a
   declaration by path today, only by the name id, which does not change
   (§12.1).
4. **bwsalmon/grain and bwsalmon/silo convert first.** Then
   `grain declarations check` warns on `.yml` templates, schedules,
   suites and triggers, and on a `.md` with `kind: schedule`, which no
   reader accepts. It keeps loading them until a `v2` of the
   declaration format, which this design does not schedule.

**Schedules become triggers.** A `.grain/schedules/<name>.yml` and a
grain/config `settings/schedules.yml` row map the same way, to
`triggers/<name>.md`:

| grain today | `kind: trigger` |
| --- | --- |
| the file name, or the row's `id` (`repo:schedule:…`, `sched-N`) | the file name, and the old name in `aka:` |
| `title`, `enabled`, `team`, `auto-approve-proposals`, `auto-merge` | the same keys at the top level (`auto_approve_proposals`, `auto_merge`); grain's team `default` becomes the Silo team (§13) |
| `template: <name>` (or a settings row's `repo:template:…`) | `fires.template: ../templates/<name>.md`, a reference (a settings id resolves by `aka` to the exported path) |
| `workflow: <path>` | `fires.workflow: /<path>`, with `fires_in: grain`; the review task uses grain's built-in template unless the row names one |
| `base: <branch>`, with `target: owner/name` on a settings row | for a template: `fires.inputs.repo: "<forge URL of target, or of the declaring repo>@<branch>"` (§5.6); for a workflow: `fires.branch: <branch>` |
| `daily: {at: "09:00"}` | `on.schedule.daily.at: "09:00"`; likewise `weekly: {day, at}`, `hourly`, `every`, `monthly` and `cron`, verbatim under `on.schedule` |
| `timezone` | `on.schedule.timezone` |
| `overlap: skip` or `queue` | `on.schedule.overlap`, the same value |
| `overlap: allow` | `on.schedule.overlap: queue`, and `convert` reports it: the settled policies are `skip`, `queue` and `cancel-previous`, so a firing that runs alongside its predecessor has no equivalent |
| — (no key today) | `on.schedule.catch_up: {mode: latest-only}`, written explicitly so the behaviour is reviewable: a scheduler that finds `next-run-at` in the past fires once and recomputes |
| — | `fires_in`: `grain` for a repo's own declaration, which its Grain fires as today; for an exported settings row, `silo` if the team's Silo fires it (`prune-packages`), `grain` if one Grain keeps it (`sched-1`) |
| `next-run-at`, `last-run-at`, paused | dropped: runtime, kept in the database |

A workflow's own `on.schedule` cron is not converted: it stays in the
workflow, and grain reads it as an implicit trigger (§12.2).

**Path triggers become triggers or gates.** A `.grain/triggers/<name>.yml`
is one or the other, decided by `gate:`:

| grain today | `gate: true` → `gates/<name>.md` | otherwise → `triggers/<name>.md` |
| --- | --- | --- |
| `on: pull_request` | implied: a gate holds changes on their way in; no key | — |
| `on: push` | — | `on.push` (or, for the design-doc planner, `on.design-change`, §7.4) |
| `paths` (and `branches`, where given) | `match.paths` (`match.branches`) | `on.push.paths` (`on.push.branches`) |
| `gate: true` | the kind itself | — |
| `human: required` | `requires.human: {when: always}` | refused: a trigger never holds a merge |
| `human: on-findings` | `requires.human: {when: on-findings}` | refused |
| `sensitive: true` | `sensitive: true`, plus `requires.security_review: {}` written out | refused |
| its template and `{{group}}`, `{{from_sha}}`, `{{to_sha}}`, `{{paths}}` | — | `fires.template`, with `fires.bind` from the event, and `on.push.group_by: folder` where it grouped by folder |
| its name (`repo:trigger:…`) | `aka:` | `aka:` |

A `.yml` trigger that both gates and files work becomes two files, one of
each kind. bwsalmon/silo's four path triggers (`grain-declarations`,
`hub-contract-docs`, `hub-deploy`, `hub-trust-boundary`) are all gates;
`grain-declarations` is in the examples.

Workflows are never converted (§12.2). Design docs gain front matter
whenever their owners choose (§12.3); until then they are referenceable
as plain files.

### 13.2 Capabilities

Today's capability names become base capabilities, and capability
objects over them, with nothing about what a run gets changing on the
day of the switch:

| grain today | Becomes | Declared implications |
| --- | --- | --- |
| `gcp-key` (a key from a pool of service accounts, `~/.gcp-service-account.json`) | base `gcp-service-account@v1`, at first in its **legacy key mode**: the same key file, which cannot narrow `token_lifetime`, so a reducer on it is refused. The token-endpoint mode of §5.7 follows | egress to Google's APIs |
| `gemini-key` (`~/.gemini-api-key`) | an object over base `api-key@v1` (a secret materialised as a file), binding the deployment's Gemini secret and `subset hosts [generativelanguage.googleapis.com]`; later `model-api@v1` through the model proxy | egress to that host |
| `github-sandbox` (`~/.github-sandbox-repo`, `~/.github-sandbox-token`) | base `github-sandbox-repo@v1` | egress to `github.com`, `api.github.com`; the throwaway repo |
| `self-debug` | base `self-debug@v1`: `read_grain_source`, `list_grain_source` and the Grain's log tools | `repos: [https://github.com/bwsalmon/grain.git]`, fetch only: task 3204's grant, until now a `model.CapabilityRepos` entry |
| `grain-ci` | base `grain-ci@v1`: the CI tools (`wait_for_checks`, `retrigger_ci`, …) | the Grain's own CI service; no egress |
| `package-publish` | base `github-packages@v1`, with a secret slot `token`, marked `approval: required` | egress to `ghcr.io`, `api.github.com` |
| `repo-read:o/n`, `repo-write:o/n` | in a task, a `context:` entry (§13); in a workflow, `git@v1` with built-in reducers (§5.7) | `repos: {from: repos}` |
| `model.CapabilityRepos` | each base's declared `implies.repos` | |
| `docs/ci-capabilities.md` | the base declarations, generated from the providers, which the doc then indexes | |
| escalated capabilities | `approval: required` in the base's declaration, or `approval.local` in the Grain's policy | |

- **Legacy objects.** For each name, `grain tasks export` writes one
  capability object with no reducers over that name's base. It writes them
  in the Grain's root at `capabilities/legacy/<name>.md`, and, for a
  linked Grain, in the team's shared repo
  (`shared:capabilities/legacy/<name>.md`), so that an intake task
  requests a team-level definition every Grain resolves against its own
  base. Each carries the old name in `aka:`. A workflow's bare legacy
  name resolves to the Grain's own legacy object.
- **The policy that matches today.** Export also writes the matching
  policy: the bases the deployment provides, the Grain's root and the
  team's `capabilities/legacy/` as sanctioned sources, and the current
  rules for scheduled runs as policy over effective authority (§5.7,
  CI). A run of a migrated task gets what it got before, and narrowing
  becomes possible.
- **A task's `capabilities:`** becomes `use` entries after its other
  `context:` entries, in the old order. A template's `capabilities:`
  stays in the file, as §12.1 keeps declaration keys verbatim, and
  filing turns each name into a `use` entry in the task.
- **Silo's grant table** names capability objects by reference instead of
  bare names, and its attestation's `granted_capabilities` gains each
  one's digest (§5.7).

## 14. Running Silo as a git host

- **Storage.** A `silo-git` StatefulSet holds the repos as bare
  repositories on a **regional persistent disk**, which is replicated
  across two zones. Smart HTTP is served by Silo's Go server, which
  calls `git http-backend` or `git upload-pack`/`receive-pack` with the
  hook in-process. **Cloud SQL stays the hub's database**, holding the
  directory, runtime state and the projection and indexes of git. All
  of that can be rebuilt from git plus the runtime tables. A
  git-on-object-storage server, such as JGit DFS on GCS, is the scale-out
  option. It is not needed at first.
- **Backup and restore.**
  - Disk snapshots run hourly.
  - After every push to a repo, a `git bundle` of that repo goes to a GCS
    bucket with object versioning and retention.
  - A restore is a disk from a snapshot plus the bundles newer than it.
    The drill is the one in hub-operations.md. Every Grain also holds a
    full clone of its own repo and can re-push it after a loss. That
    push is allowed only into an empty or behind repo, and checked
    commit by commit.
- **Hook performance.**
  - It parses front matter only, for changed `.md` files only.
  - It caches directory lookups for the length of the push.
  - It caps a push at 1,000 changed files and 10 MiB, and so any one
    asset at 10 MiB (§2.2).
  - Target: p99 under 200 ms for a 10-file push.
  - Writes to one repo are serialised, and different repos run in
    parallel.
- **Scale.** Assume thousands of Grain repos, each running about 10⁴
  tasks a year. An execution record with its thread is roughly 5 KiB,
  which is about 50 MiB a year per Grain before compression. A backlog
  reference is under 1 KiB, and `backlog/` holds only open work (§5.2).
  The tasks themselves are in the intakes, so a team's intake grows
  with the team's tasks, at a similar size per task.
  - **Commit batching.** Silo makes one commit per dispatcher pass per
    repo, Grains push at most every 5 seconds, and notifies are
    coalesced (as hub.md §2.2.4 does today).
  - **Fetch cost.** Grains use partial clone (`--filter=blob:none`), so
    a fetch costs what changed.
  - **Archival.** Closed tasks and closed runs never move. After a year
    a Grain repo, or an intake, can be **rolled over**: a new repo
    continues it, and the old one becomes read-only under an alias
    (`eu-2026`). A rollover is not a move, so there are no tombstones and
    no `moved_to`: every old reference still resolves, because it names
    the old repo's URL. A Grain rolls its root over itself: it copies its
    includes and its own lane's open references into the new root
    (references and includes are pointers, so that is allowed), and the
    rollover waits for open runs to close. Silo's placements are not in
    the root, so they need nothing. In an
    intake, open tasks stay in the old repo until they close, and new
    tasks go to the new one.
- **Availability.** While Silo's git is down:
  - A Grain keeps running what it has.
  - It commits locally and pushes on recovery, rebasing onto the new tip
    with a front-matter-field 3-way merge. Its root has no other writer,
    so the rebase only ever replays its own commits, and Silo's
    placements, which changed meanwhile, are in another repo.
  - Nothing new is placed.
  - Standalone and locally scheduled work continues.

## 15. Alternatives considered

| Alternative | Why not |
| --- | --- |
| GitHub pull requests as the proposal and approval mechanism | Needs GitHub, its accounts and its API. Approval would be a forge setting, not a team rule Silo decides. Repos on other hosts and local repos would be second class |
| Move files between folders to change state (`queued/`, `done/`) | Splits `git log --follow` history, breaks every reference by path, and turns each state change into a delete plus an add that races with concurrent edits |
| One repo per team, Grains included | Every Grain would fetch every other Grain's churn, and per-Grain write rules would become per-path rules in a shared repo with more contention. A root per Grain matches the single-tenant Grain. The team's placements repo is shared, but it holds only placements, which are small and few, and has one writer; the churn, execution records and threads, stays in each Grain's root |
| A single repo with filtered views | Read permission in git is per repo, so filtering would mean Silo serving synthetic repos. That is a large custom server, and it breaks plain-git interop |
| YAML only (no Markdown bodies) | Prompts and designs are prose. YAML block scalars are hostile to review and to Markdown tooling. Front matter keeps YAML where it fits |
| Go `text/template` | Not portable outside Go. It has method calls on values, and it cannot be checked against a reference implementation. Jinja2 has a spec-like reference (Python) and a Go port |
| Database only (today) | No offline or standalone history, no review, and no way to reference across Grains or keep history when unlinking. Silo would remain the only channel |
| An API-only task interface without git | Keeps the WebSocket protocol growing for every kind. A Grain behind NAT that missed a message needs `resume`/`cancel_pending` machinery that git's "pull the tip" gives for free |
| Separate ids (`id: task-4redj39vwnz4`, 4446's first draft) | A second name that drifts from the first. Every reference had to carry both a path and an id, readers needed an id index per commit to find files that had moved, and the hook had to police uniqueness across the graph. The path already names the object. With objects that do not move to change state, it is stable, and the rare real move is handled by a tombstone (§2.1) |
| Silo copies the task into the Grain's repo on placement (4446's first draft) | Two copies of one spec, with an "authoritative after placement" rule to say which one wins. Re-placement had to tombstone a task that had not really moved, and a closed task's outcome lived in a copy far from its author. A reference to the task, plus an execution record, says the same without a copy (§5.2, §5.3) |
| Rank on the task itself | Mixes the backlog owner's ordering into the author's file, so the author and the queue's owner would both write one file. A task in two queues would need two ranks, and placement would have to copy the task to give it a Grain-local rank |
| A single ordering file per repo (`backlog.yml` listing paths in order) | Every reorder edits the same file, so concurrent reorders, placements and removals all conflict. The file is not self-contained: a line in it means nothing without the file it names, and it cannot carry per-entry ownership (`expedite` is Silo's, `rank` the owner's) |
| Filename-prefix ordering (`backlog/0010-…`, `backlog/0020-…`) | A reorder becomes a rename. That conflicts with path-as-identity, and makes `git log` and every record that names the reference (`from:`) follow renames |
| Silo writes a `closed:` summary onto the task when a run closes | Makes every close a cross-repo transaction, adds a second writer to a frozen spec, and leaves two copies of the outcome to drift. The outcome is derived from execution records instead (§5.1) |
| Re-placement as a move of the task, with a tombstone | Nothing about the task changes on re-placement, only which queue holds it. Moving it would break its address for its author, its dependents and its intake references, for no gain |
| Includes listed in a central file, such as the root README (`includes: […]`) | Not self-contained: a line in the README means nothing without the file it names, and cannot carry its own ref mode, kinds and history. One file for every include is contended: Silo linking, the owner adding a template set and a teammate adding triggers would all edit it. Standalone include objects sit where they apply (`backlog/silo.md`), and appear and disappear as files (§3.3) |
| Any object able to include (a template's `includes:`, a task's `brings:`) | Silent namespace widening: every kind would become a way to add names, and an object written for one purpose (a placement, a template from another team) could pull a folder into a Grain's namespace. References already reach anything; only `kind: include` adds names, so the trust grants are exactly the include objects (§11.14) |
| Optional transitivity (`transitive: false` on an include) | A half-working copy: the included folder's own includes are part of what it means (its partials, its upstream set, its cuts), so following one level gives a set its author never had. Excluding an include object turns off one branch where that is really wanted (§3.3, rule 4) |
| Global, unscoped excludes (an exclude anywhere in the graph cuts the name everywhere) | An included folder could hide objects from elsewhere: a placement, the root's own task, another team's template. Scoping an exclude to its own space makes an included folder able to hide only itself, and leaves the root's excludes as the final word (§11.15) |
| Merged ranks across sources (one sort over every reference in the namespace) | Ranks are not comparable across writers: Silo's `t` and the owner's `t` were chosen against different neighbours, so a merge lets whoever picks more extreme keys win. Lanes keep each writer's order intact, and the root decides between them (§5.2) |
| Lane priority on each include object (`lane: {priority: 20}`) | Numbers spread over files with different writers are a merged rank again, and an include deep in a branch could promote its own lane. A lane can also arrive with no include in the root. The root README holds it as metadata (§5.2) |
| Silo writing into Grain repos (4448's placements in the Grain's `backlog/`, Silo writing `superseded`) | Two writers in one repo: field-level rules in every Grain repo's README, a hook that must tell Silo's fields from the owner's, rebases against each other's commits, and a Grain whose own history Silo can change. Silo's folder for each Grain, included by the Grain, gives the same channel with one writer per repo, and re-placement becomes one commit (§8.8) |
| Adopting Backstage's entity envelope verbatim (`apiVersion`, `metadata.name`, `spec`, `Location`) | `metadata.name` is mandatory, at most 63 characters and slash-free, so it cannot be a path and brings back a separate id. References are `kind:namespace/name`, resolved in a catalog database rather than by location. The catalog ingests YAML documents, not Markdown with front matter. `apiVersion` and custom-kind processors are ceremony here, `relations` and `status` are computed by the catalog rather than kept in files, and `Location` has no excludes and keeps its kind rules in app config. The ideas are borrowed instead, with an optional exporter (§3.4) |
| A grain folder at a fixed path only (`.grain/`, found by name) | The layout is loose everywhere else (§2), and a monorepo wants one folder per project. Nearest-README discovery finds the governing README wherever the folder is, and `.grain/` stays the convention |
| Code-repo metadata in the repo's top-level `README.md` | That file belongs to the code: forges render it as the repo's front page (GitHub shows front matter as a table), documentation tools read it, and its edits are code changes. A grain folder's own README keeps grain's metadata with grain's objects |
| A separate metadata file (`.grain/repo.yml`) | A second format, and a file that explains nothing to a human. A README with front matter is what a whole-repo task repo uses, so one rule covers both |
| Keep `.grain/` declarations YAML for good | Two parsers, two schema sets and two spellings of every key, for the same kinds. Prompts stay in YAML block scalars, which review badly |
| Workflows as Markdown too | Breaks GitHub Actions compatibility and the tools that read workflows, for a body workflows do not have (§12.2) |
| Silo's hook in front of code repos (Silo as the code repo's proxy or mirror) | Silo would have to sit between developers and their forge, or the two would disagree. The forge's review already decides what lands; Silo validates what it reads instead (§8.13) |
| Task home always the code repo | Proposing would need forge accounts and write access, every task would cost a review in the code repo, and team-targeted work has no code repo to go to |
| Task home always the intake | The owner's case: long-lived, design-linked work belongs beside the design and the code, versioned and reviewed with them. `promote` gives that task by task, without making it the default |
| Copy the task into the code repo, keeping the intake's | Two copies of one spec, the problem 4448 removed. A move with a tombstone keeps one |
| Commit the task file in the work's own change | The definition would not exist in the code repo until the work merged, a failed run would leave none, and the task file would be reviewed as part of a change it describes |
| Placement waits for the code-repo commit to land | Delays the work by a review, for nothing: the spec is the same at both addresses, and references follow the move (§5.5) |
| Trigger-fired tasks committed to code repos | A commit, and a review, per firing, in the code's history, for content that is the trigger plus an event (§6.2) |
| Let a code repo name the team its triggers fire for, with no adoption | Any repo could fill any team's intake. Adoption makes the team's admins the ones who choose (§6.2) |
| Materialise every reference a task holds | `depends_on` would pull other tasks' repos into the sandbox, `template` and `moved_from` would bring files nobody meant the agent to have, and a placement's `to:` would bring the intake. The sandbox would be whatever the graph happened to touch. Only `context:` materialises, and the rest stays metadata (§5.6) |
| Transitive `context:` by default | Unbounded sandboxes: one design that links to its neighbours brings the whole docs tree, and what a run sees would change whenever someone adds a link. Off by default, `depth: n` where wanted, under file and byte caps that skip and record (§5.6) |
| Keep separate `Target`, `Reads` and `Writes` fields | Three mechanisms for one idea, plus attachments and pasted context as a fourth and fifth. Each needed its own policy check, and task 4303 was a repo that reached a sandbox by a route the policy did not see. One list with a mode is one check (§5.6, §11.21) |
| Require Git LFS for assets | Not plain git everywhere: Silo's git host, a bare `git daemon`, a `file://` root and many mirrors do not serve it, and a clone without LFS gets pointer files. LFS is allowed where the host has it; size limits and external artefacts cover the rest (§2.2) |
| Other names for `context:` (`mounts`, `workspace`, `sandbox`, `inputs`, `reads`/`writes`) | Each names one mechanism or collides with an existing field (§5.6) |
| A sidecar for every asset | Metadata most assets do not need: an image in a design has its alt text in the link. Sidecars are optional, and are where an asset needs a license, provenance, or an external location (§2.2) |
| Attachments inline in front matter (base64) | Front matter is for review; a megabyte of base64 is not. Every diff, digest and parse would carry it. A file beside the object is plain git, and renders in any forge (§2.2) |

| Opaque capability names (`gcp-key`, `self-debug`, the status quo) | What a name grants and implies lives in provider code (`model.CapabilityRepos`), invisible to the local policy, which is how task 4303's repo reached a sandbox. A name cannot be narrowed (one `gcp-key` for every project and role), composed across an org and a team, or explained to an approver. Objects over declared bases make the surface readable and the narrowing explicit (§5.7) |
| Capability objects able to grant beyond their base (an `extra_roles:`, or a reducer that adds) | Breaks monotonicity, the one property that makes every other check local: with it, effective authority depends on every object in the chain, the policy must re-derive the maximum, and an included folder could escalate any capability built on it. Authority comes only from bases and allowlisted secrets; everything else narrows (§11.27) |
| Granting on include (a Grain honours whatever capability objects it includes) | Makes every include of a shared folder a grant, so a team repo's maintainer could hand any Grain that includes it new authority. A catalog decides what a Grain offers; the policy's source allowlist decides what it honours (§11.29, §11.30) |
| A separate `capabilities:` list on tasks | A second list beside `context:`, with its own check: the split 4453 removed for reads and writes. One list, with `mode: use` for a request, is one check, and the approver reads one thing (§5.6, §5.7) |
| Choosing a reducer language now: **CEL** (Google's Common Expression Language), **WASM** modules, **Rego** (Open Policy Agent), or **macaroon caveat strings** (and Biscuit's Datalog caveats) | The owner's decision: Anselm comes from a project in flight, and this design fixes only the contract any implementation must meet (§5.7). These are the prior art Anselm will be compared against. CEL is non-Turing-complete, side-effect free and cost-bounded, but has no notion of narrowing: monotonicity would be checked by the Grain, not given. WASM sandboxes anything and bounds it with fuel, at the price of opaque binaries in a review-first format. Rego is a mature policy language with partial evaluation, but evaluates against a data document rather than narrowing a capability, and is larger than a reducer needs. Macaroon caveat strings are the model itself, but first-party caveats are an uninterpreted string for each verifier to parse, and so are not a language. Biscuit's Datalog is attenuation-native, the closest prior art. The built-in reducers are a deliberately small interim, not a candidate |
| Skipping reducers a Grain cannot evaluate (best effort until Anselm) | Skipping a narrowing widens. Two Grains would grant one object differently, and a placeholder would silently be a no-op. A Grain that cannot evaluate a reducer refuses, naming it (§5.7) |
| Keep a separate `kind: schedule` beside `kind: trigger` | Duplicates the trigger concept: a schedule is "when the clock says, file this", one event source among several. Two kinds would carry the same `fires`, approval, overlap, rate limits, adoption and customisation rules twice, and every reader, `kinds:` list and security rule would name both. One kind with `on.schedule` is how GitHub Actions and grain's own workflows already say it (§6.2) |
| One `kind: trigger` that also covers merge gates (grain's `gate: true` today) | Firing work and holding a merge are different things, and mixing them is what makes today's `.grain/triggers` messy: one file shape where some keys file tasks and others block merges, so neither can be reasoned about alone. A gate must be monotone and not weakened by the machinery that customises triggers (excluding a team's trigger must never release a merge), and must be read from the base, not the change. Separate kinds let `kinds: [trigger]` admit standing work without admitting policy, and let each schema refuse the other's keys (§6.3, §11 items 38–39) |
| Gates always non-excludable, with no opt-out for the root | Would make every include of a shared gate set mandatory, so including one to try it would bind the Grain for good, and the root's excludes would stop being the final word for one kind. Mandatory gate sources in local policy give an org the non-excludable case without taking adoption away from everyone else (§6.3) |
| Include `on: task-closed` now, for chaining | `depends_on` already chains known work. A close-fired trigger can fire itself through its own firing's close, across Grains, and needs cycle detection and a rule for whose close counts. Reserved, and refused by v1 readers, until that design exists (§6.2) |
| Pause and next-fire time in the trigger's front matter | Runtime state in git: a commit per fire, per pause and per resume, contended between Silo, the Grain and the definition's authors. `enabled:` is the reviewed switch; the rest is in the database (§6.2, §1.2) |

## 16. Staged implementation plan

| Stage | silo (bwsalmon/silo) | grain (bwsalmon/grain) |
| --- | --- | --- |
| **1. Format** | a `taskrepo` package: front-matter parse and write (field-preserving), JSON Schemas (`suite`, `trigger` with its `on.*` sources and `fires`, `gate`, `include` and `exclude` included), the reference grammar and resolver with redirects (tombstones and repo `moved_to`), nearest-README discovery and `scope`, the namespace resolver of §3.3 with its limits, the path rules, rank keys, lanes and backlog ordering, `task_home` resolution, and `writers` evaluation (principal lists, and the intake's rules); the `context:` resolver shared with namespaces, with its limits and default layout, and the `asset` kind; the Jinja subset on gonja with the Python fixture CI job | pin `taskrepo`; `grain tasks export/import` with the round-trip test on a copy of the database; `pkg/repodecl` and `grain declarations check` read `.md` declarations beside `.yml` (§13.1); `grain declarations convert`; convert bwsalmon/grain's own `.grain/` |
| **2. Standalone Grain on a task repo** | none | a local root becomes the backlog's source of truth for git-backed fields, with the database as runtime and index: tasks, ranked references in `backlog/`, execution records in `runs/`, comments as files; triggers from the namespace (`schedule:` and `manual:` sources, `fires` of both forms, overlap, catch-up and rate limits, with last and next fire, pause and suspension in the Grain's database); gates on the Grain's merge queue, with mandatory gate sources in local policy; includes of shared folders; `context:` materialisation: the PEP before fetch, staging, the proxy's scope and push rule, the sandbox layout, and the run's `context:` record; attachments as assets |
| **3. Silo hosts git** | `silo-git`: smart HTTP behind IAP and node tokens, the pre-receive hook, protected refs, bundles to GCS, restore drill, metrics; intake and placements repos per team; hosted Grain roots writable by the Grain only; propose and approve by push and through the API (the same commit); assets served from a user-content origin (§11.25); the threat-model boundary | `grain tasks link/unlink` as include objects; push and pull to a Silo remote; lanes |
| **4. Placement by commit** | the dispatcher adds, moves and removes placements in its own repo, and writes cancels on tasks, in §8.11 order; the fence on withdrawn placements; the run index and dependency resolution from execution records; the `pull` notify; WebSocket offers derived from commits behind a flag, then retired | accept, refuse and close as execution records, attestation on the reference, the removed-reference fence, upward filing through Silo; then drop `offer`/`resume` handling |
| **5. Shared definitions and planning** | templates and triggers fired in Silo (`schedule:`, `push:`, `merge:`, `manual:`), with their runtime state and rate limits in Silo's database; the design index, and the `design-change` source with its flood control; gates on Silo-hosted repos (the hook, Silo's merge queue and its trailers); the review-branch gate | render shared templates locally; `implements`/`closed.design_revs`; trailers from refs |
| **5b. Code repos and task homes** | the code-repo reader (webhook or poll) with validation on read (§8.13); trigger adoption; `push:`/`merge:`/`design-change` from code repos; designs planned in place by an in-repo `design-change` trigger; `task_home`: landing a task by direct push or a change through grain's merge queue, the pending-change record, the tombstone and reference rewrite, `silo task promote`; cancels of homed tasks | `propose_promotion`; inline firings; local policy for code repos a Grain trusts |

| **5c. Capabilities** | the `capability` and `capability-base` schemas; the chain resolver with cycle detection, digests and the built-in reducers in `taskrepo`; Silo's catalog view, placement against Grains' published bases, `capability_approvals` in the intake's rules, and digests in `granted_capabilities`; the legacy objects of §13.2 in each team's shared repo | providers declare their bases and the Grain publishes them; the policy's `capabilities` (bases, sources, secrets, `approval.local`) and the implication check, replacing `model.CapabilityRepos`; credential narrowing in the GCP, git and model providers; per-use reducers in the git, model and Google API proxies, with the audit log; CI's `capabilities:` resolved to objects, `repo-read`/`repo-write` as git reducers. **Anselm** is adopted only when that project is ready, as a later stage of its own |
| **6. Signed commits** | keys in README rules; verifying signatures in the hook | verifying the approver's signature against policy (`require_signed`) |

Each stage is a design-item set to be filed after this review. None is
filed by this change.
