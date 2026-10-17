# Grainfiles and grain repos: Grain's work as plain git

**Status: design for review (bsalmon@google.com). Nothing here is built,
and no implementation task is filed until it is approved.** The examples
are in [`examples/grainfiles/`](../examples/grainfiles/README.md), the
worked example of the spec's Appendix A. A link below that lands on its
README names a file of the earlier, larger illustration, which is in git
history at 161fb9f (`examples/grainfiles/`).

This document is the rationale and the design. The normative
specification is **[The Grainfile Format](grainfiles-spec.md)**
(`docs/grainfiles-spec.md`): the exact grammar, the schemas
([`schemas/grainfiles/`](../schemas/grainfiles/)), the resolver's
algorithm, the lint codes and the validator
([`tools/grainfiles/grainlint.py`](../tools/grainfiles/grainlint.py)).
Where the two disagree, the spec wins and this document is corrected.

**Terms** (spec §2): a **grainfile** is one kind-suffixed Markdown object
file; a **grain folder** is a directory holding a `grain.config.md`, the
root definition of a grain deployment, and is either a whole repo (a
**grain repo**) or a subdirectory of an ordinary code repo; a repo may
hold several grain folders, which never nest; a **deployment** is one
running Grain, with exactly one root grain folder. Repos live on
ordinary **forges** (GitHub, depot/GHOP, GitLab, …), which are
authoritative; Silo reads and writes them and is not their host (§8).

Grain's tasks, triggers, gates, templates, capabilities, memory and
design docs become **grainfiles**: self-contained Markdown files, one per
object, each named `<name>.<kind>.md` (`4310-carry-approver.task.md`,
`prune-packages.trigger.md`), kept in **grain folders**. They are plain
git on ordinary forges, linked to each other by git URL and folder path,
and they become three things:

- every Grain's backlog;
- Silo's backing store for intake;
- the channel through which Silo and Grains exchange work.

(Earlier drafts of this design called grain repos "task repos";
Appendix A has the history.)

The model in one paragraph: a grainfile is named by its path, and never
moves to change state. A Grain acts only on what its **`grain.config.md`**
activates, as **typed roots**: backlog lanes, trigger folders and memory
folders. Work is split in two: a **task definition** says what to do and
holds no state, and may live in any grain folder; a **task execution** is
one folder, `<name>.exec/`, in a lane of the deployment that runs it,
holding a snapshot copy of the definition, its state, its own thread and
its runs (§5, §6.1–§6.3). Everything else is reached **by reference**,
read-only. A parent **contains** its children: a one-to-many relation is
a folder, never a pointer. A snapshot's **`context:`** is the only thing
that puts files in a sandbox. Who may write what is in **`GRAINOWNERS`**,
enforced by each forge's protected branch and a required check (the
**forge check**), and re-checked by readers where the forge cannot be
relied on (§8). An execution's state is a **lifecycle** state whose
category Grain acts on (§6.12), and standing multi-task work is a **folder
template** (§6.13).

This changes what travels over [the task interface](hub-interface.md)
(§10.14). It leaves alone who decides (Silo, the PDP) and who enforces
(each Grain, the PEP) ([hub.md](hub.md) §1.2).


## 1. Purpose and framing

**A Grain consumes grain repos.** A **standalone Grain**'s backlog is its
own grain repo, which can be a purely local git repo
(`file:///var/lib/grain/tasks.git`). That one repo holds the definitions
its owner writes and the executions of them in its lanes, each with its
thread and the records of its runs.

Every Grain has a **root folder** (§4.2): a whole repo, or a subtree
such as `.grain/`. Its **`grain.config.md`** says what the Grain acts on,
as **typed active roots**: the backlog lanes whose executions it runs,
the trigger folders whose triggers it fires, and the memory folders it
gives its runs (§4.2). The root also holds the Grain's own grainfiles as
ordinary files: its lanes and every execution in them, with their
threads and runs, and its own triggers, definitions and memory. **Only the Grain and its owner write
it.** Everything else a Grain uses, from a task's template to a
capability definition, it reaches **by reference**, read-only, when
something needs it (§4.1).

**Linking a Grain to Silo is an edit to its root.** The Grain's owner
adds a lane for Silo (`hub/`) to `grain.config.md`, and a `GRAINOWNERS`
rule letting the team's members create execution folders there, and
registers the root in Silo's registry (§8.7). Placing work on the Grain
is then Silo **creating an execution folder in that lane**, through the
forge API as the person placing it (§10.3): one commit in the Grain's own
repo. **Nothing Silo writes activates anything unless it is in a lane
the owner declared**, and Silo never writes anything about an execution
anywhere else. Silo also grants the Grain read access to the repos the
snapshot's `context:` names, and to the definition's repo, for its
thread.

**Unlinking** removes those entries. Nothing moves: the root, with all of
its history, was the Grain's all along.

A linked Grain's root is a grain folder on an ordinary forge, like every
other. Grains sit behind NAT and firewalls and Silo never dials them
(hub.md §4); both sides talk to the forge instead. The Grain **pulls
from and pushes to** its root, and **only pulls** from every other
folder: its executions, their states and their runs are all in its root,
so it has nothing to write anywhere else. The existing
rendezvous WebSocket stays, and carries:

- `pull` notifications ("repo `<url>` moved to `<sha>`");
- live events;
- the view-through and node UI streams (hub.md §7, §11).

**Silo is a reader and writer, not a host.** Silo stays what hub.md
makes it: the team directory, the approver of work, the dispatcher that
decides which Grain runs what, the views over all of it, and the policy
decision point for its UI. The grain folders themselves (each team's
intake, its shared folders, the Grains' roots, and the
grain folders in code repos) live on **external forges**, and **the
repos are authoritative**: Silo's and the Grains' indexes are caches.
Silo writes through the forge API **as the user it acts for**, with that
user's delegated token, never as a privileged bot (§8.3). Silo's own
**registry** of the grain folders it knows is itself a grain folder on a
forge (§8.7).

**The Grain stays the PEP.** Every execution Silo creates for a Grain is
a **claim**. The Grain validates the claim and holds the snapshot to its
local policy, and it may refuse it (a `refused` run) as it refuses an
offer today.

### 1.1 A graph of grain repos

Repo **roles** are conventions, recorded only in prose. The format does
not depend on them:

| Role | How many | Holds | Typical writers |
| --- | --- | --- | --- |
| **Grain root** | one per deployment: a grain repo, or a grain folder in a repo | that Grain's root (§4.2): `grain.config.md`, its lanes and the executions in them (snapshots, state, threads, runs), its own definitions, triggers and memory | the Grain (runs, outcome transitions), its owner, and, per subfolder, the people `GRAINOWNERS` lets create, rank, approve and comment on executions (§8.2) |
| **intake** | one per team, on a forge | the team's definitions and their discussion; the team's own triggers and templates | any authenticated user (new definitions and comments), the team's members (§8.2, §8.3) |
| **shared** | any number, on any forge | templates, triggers, design docs, template partials, memory and capability definitions, for Grains to name in their roots or reference | the team's maintainers, through review branches |
| **code** | the code repos themselves | a grain folder (`.grain/`): templates (folder templates included), triggers, gates and workflows, definitions homed there with their threads (§9.2), and design docs in place in `docs/design/` (§9.1, §9.3) | whoever the repo's own review and merge process lets land a change (§9.6) |

| **registry** | one per Silo, on a forge | Silo's `grain.config.md` in its registry form: the registered grain folders (URL plus path) and saved cross-folder views (§8.7) | the registry's maintainers, by reviewed commits |

"Silo" is **one or more grain folders**, not a special store. Everything works by
references and declared folders alone:

- an execution in a Grain's lane names the definition it snapshotted, in
  an intake, a Grain root, or a code repo's grain folder (§9.2), pinned
  to the commit it copied;
- a Grain's `grain.config.md` names the folders whose grainfiles it acts on (§4.2);
- a definition may name a template in a shared repo;
- a one-to-many relation is a folder inside its parent: a definition's
  thread, an execution's thread and runs; Silo's indexes of the rest
  (a definition's executions, a design's implementers) are caches (§5).

### 1.2 What goes in git, and what does not

Git holds what people review and refer to:

- definitions, intent and decisions;
- designs;
- each backlog's order, as the executions in each lane;
- the human-readable threads, of definitions and of executions;
- for each run of an execution, a final-state snapshot.

**High-churn runtime state stays in the Grain's or Silo's database:**
attempts, live status, heartbeats, events, transcripts and logs. Two
rules keep the split clean:

- **State lives in front matter, never in file location.** A file is
  written once, at a stable path that is its identity (§2.2), and never
  moves to change state.
- **Each subfolder has one kind of writer** (§8.2, §8.3): a definition's
  owners its `task.md`, commenters its `thread/`, the Grain an
  execution's `runs/`, approvers an execution's approval fields. The databases may mirror a field for
  queries, but they never compete with git for it.

| Git-backed | Runtime only (database) |
| --- | --- |
| definition: `title`, body (the prompt), `team`, `context` (§7), its `assets/`, its `thread/` and the comments in it (§5) | the attempt list, attempt numbers, sandboxes, leases |
| definition: `submitter`, `origin`, `filed_by`, `template` (+ `inputs`, `rev`), `depends_on`, `implements`, `constraints`; capability grainfiles and Grains' base declarations (§6.8) | the live `running` sub-states (sandboxing, agent turn, waiting for CI) |
| execution (`exec.md`): `definition` and `definition_sha256`, `inputs`, `state`, `approved_by`, `approved_at`, `capability_approvals`, `cancel`, `rank`, `priority`, `expedite`, `depends_on`, `previous`, `superseded`, `trigger`, `attestation`, who created it and when; its snapshot `task.md`; its `thread/` | events, `seq`, transcripts, tool calls, diffs, logs |
| run (`runs/<run>/run.md`): `status` at the coarse grain of §6.3, timestamps, `refusal`, `superseded`, the `capabilities` it used (§6.8), and the run's comments beside it | heartbeats, slot counts, offer timers; the dispatcher's claim, `refused_by` lists, offer counts; secret material, minted credentials and the per-use decision log (audit events) |
| run: `closed`: outcome, change link, merge commit, summary, requirement revisions | merge-queue position, CI run ids, cost and token counts (summarised into `closed` if wanted) |
| trigger and gate definitions: `on`, `fires`, `enabled`, `limits`; `match`, `requires`, `sensitive` (§6.6, §6.7) | a trigger's last and next fire, debounce windows, catch-up cursor, paused or suspended state and run history; a gate's hold on each change and its reviews' progress |

A coarse state change, such as a run going `accepted → running → done`,
is a commit. A fine-grained step, such as "agent turn 14", is an event.

## 2. Grainfiles

A **grainfile** is one object of Grain's (a definition, an execution, a
run, a comment, a trigger, a gate, a capability, a design, a memory document, a
tombstone, …) as one self-contained UTF-8 file. The file has **YAML front matter**
between `---` lines, then a **Markdown body**. Every file declares:

- `kind:`, one of `config`, `task` (a definition), `exec` (an
  execution), `run`, `comment`, `template`, `trigger`, `gate`, `design`,
  `memory`, `moved`, `asset` (a sidecar, §2.3), `capability`,
  `capability-base` (§6.8) or `lifecycle` (§6.12). `suite`, `ref` and
  `placement` are removed (§6.13, Appendix A). It must match the kind
  suffix of the file's name, `<name>.<kind>.md` (§2.1), or the fixed
  name the file has inside its container (`task.md`, `exec.md`,
  `run.md`);
- `schema:`, as in `grain/task@v1` (§2.4).

There is **no `id:` field.** A grainfile is named by where it is (§2.2).
Every other file in a repo, an image or a CSV or a Markdown file with no
kind suffix, is an **asset**: referenceable the same way, but not an
grainfile (§2.3).

The body carries what a human reads: a task's prompt, a template's text,
a design's prose. A file means something on its own. You can read it,
mail it or `cat` it without knowing which repo it came from.

**Layout is loosely specified.** Nothing scans for grainfiles: a Grain
lists the folders its `grain.config.md` names, by kind suffix, and
resolves references to everything else (§11.1). The recommended folders
below are conventions only. Folders carry meaning in two places only:
the root entry that lists an execution is its **lane** (§6.2), and a
container (`<name>.task/`, `<name>.exec/`) holds its children (§5).

A Grain's root (§4.2):

```
grain.config.md                            kind: config  (its typed active roots, §4.2)
GRAINOWNERS                                write ownership, per subfolder (§8.2)
.grainconfig                               repo settings: aliases, limits (§8.1), at the repo root
hub/<name>.exec/                           an execution Silo created in the Grain's hub lane (§6.2)
  exec.md                                  kind: exec    (provenance, state, approvals, rank)
  task.md                                  kind: task    (the snapshot: the definition as it was)
  thread/<utc>-<author>.comment.md         kind: comment (discussion of this execution)
  runs/<utc>/run.md                        kind: run     (one attempt, §6.3)
  runs/<utc>/<utc>-<author>.comment.md     kind: comment (that attempt's agent thread)
  assets/<file>                            an asset
hub/<instance>/<member>.exec/              a folder template's instance, one execution per member (§6.13)
backlog/<name>.exec/                       an execution in the Grain's own lane
backlog/fired/<trigger>/<key>.exec/        its triggers' firings (§6.6)
backlog/gated/<repo>/<change>.exec/        its merge queue's gate reviews, a lane of their own (§6.7)
tasks/<name>.task.md                       kind: task    (its owner's own definitions)
triggers/<name>.trigger.md                 kind: trigger (its own; a trigger root)
memory/<name>.memory.md                    kind: memory  (its own; the write: true memory root)
capabilities/bases/<type>.capability-base.md  kind: capability-base (written by the Grain from its providers)
lifecycles/<name>.lifecycle.md             kind: lifecycle (§6.12)
hub/archive/…, backlog/archive/…           archived executions (§4.3)
```

An intake or a shared repo:

```
.grainconfig                               owner, aliases (§8.1)
GRAINOWNERS                                who writes what (§8.2)
README.md                                  for humans: an asset
tasks/<name>.task.md                       kind: task    (a definition with no discussion yet)
tasks/<name>.task/                         a definition with discussion or assets (§5)
  task.md                                  kind: task
  thread/<utc>-<author>.comment.md         kind: comment (discussion of the definition)
  assets/<file>                            an asset      (attached to the definition, §2.3)
tasks/archive/…                            retired definitions (§4.3)
templates/<name>.template.md               kind: template
templates/<name>/<name>.template.md        kind: template (a folder template's manifest, §6.13)
templates/<name>/<member>.task.md          kind: task    (its member definitions)
templates/partials/<name>.template.md      kind: template (include-only, inputs declared)
triggers/<name>.trigger.md                 kind: trigger (a trigger root for Grains that name it)
triggers/archive/<name>.trigger.md         a retired trigger, active nowhere (§4.3)
memory/<name>.memory.md                    kind: memory  (a read-only memory root for Grains that name it)
designs/<name>.design.md                   kind: design
capabilities/<name>.capability.md          kind: capability (a team's or org's definitions, §6.8)
designs/img/<file>                         an asset      (an image the design embeds)
designs/img/<file>.asset.md                kind: asset   (its optional sidecar, §2.3)
<old name>.moved.md                        kind: moved   (a tombstone, §2.2)
```

A code repo keeps the same kinds in its grain folder, beside the code
(§9.1):

```
README.md                                  the code's own README: an asset
.grainconfig                               optional (§8.1)
.grain/templates/<name>.template.md        kind: template
.grain/triggers/<name>.trigger.md          kind: trigger (standing work, active for Grains that name the folder, §6.6)
.grain/gates/<name>.gate.md                kind: gate    (merge gates on this repo, read by its merge queue, §6.7)
.grain/templates/<name>/<name>.template.md kind: template (a folder template: replaces a suite, §6.13)
.grain/workflows/<name>.yml                a GitHub Actions workflow: YAML, an asset (§9.4)
.grain/tasks/<name>.task.md                kind: task    (a definition homed here, §9.2; or <name>.task/ with its thread)
docs/design/<name>.design.md               kind: design  (in place, §9.5)
```

**A tombstone's file name** is the old path with its final `.md`
replaced by `.moved.md`: moving `templates/prune-packages.template.md`
leaves `templates/prune-packages.template.moved.md`. A reference still names the
old path; a resolver that finds nothing there looks for the tombstone
beside it, then for the archive mapping (§4.3). Each is one
deterministic lookup, and no index is needed.

**Names.** Whoever creates a grainfile picks its path, once:

- a new definition: `<yyyy-mm-dd>-<slug>`, with `-2`, `-3`… if that is
  taken. Two writers racing for one name collide as an ordinary git
  conflict, and the second one picks the next suffix;
- a migrated grainfile: `<legacy number>-<slug>` (§13);
- a generated grainfile: a name derived from what generated it, so a retry
  lands on the same path. A trigger's firings use
  `<lane>/fired/<trigger>/<event key>.exec/` (§6.6), planning among them
  (§6.9);
- an execution: its definition's name, with `-2`, `-3`… if the Grain
  already has one by that name (`hub/2026-10-16-approval-flow.exec/`);
- a run: the UTC time it was accepted
  (`<name>.exec/runs/20261016T135800Z/run.md`);
- a comment: `<utc>-<author>`, such as `20261016T144710Z-grain-eu.comment.md`.
  Each author writes its own comments one at a time, so two authors
  appending to one thread never conflict.

The slug is taken from the title when the grainfile is created. A later
title change leaves the path alone.

### 2.1 File names carry the kind: `<name>.<kind>.md`

Every grainfile's **kind is in its file name**, so a reader can list a
folder's grainfiles, and know each one's kind, from a git tree alone (§11.1):

| Suffix | Kind | Where it is active, or how it is reached |
| --- | --- | --- |
| `grain.config.md` | `config` | a Grain's root configuration (§4.2); the file name is fixed |
| `<name>.exec/exec.md` | `exec` | an execution folder, active in a **backlog** lane of the Grain whose root holds it (§6.2) |
| `*.trigger.md` | `trigger` | active in a **trigger** root (§6.6) |
| `*.memory.md` | `memory` | active in a **memory** root (§4.2) |
| `*.gate.md` | `gate` | read from the merged-into repo's `.grain/` by its merge queue (§6.7) |
| `*.task.md`, `<name>.task/task.md`, `<name>.exec/task.md` | `task` | a definition, by reference: an execution's `definition`, `depends_on`; or an execution's snapshot (§6.1) |
| `<name>.exec/runs/<run>/run.md` | `run` | written by the Grain inside its execution (§6.3) |
| `*.comment.md` | `comment` | in the `thread/` of the definition or execution it discusses, or in the run folder (§6.4) |
| `*.template.md` | `template` | by reference: a trigger's `fires.template`, a task's `template.ref`, a partial's include |
| `*.lifecycle.md` | `lifecycle` | by reference: an execution's, a definition's, a lane's or a folder's `lifecycle:` (§6.12) |
| `*.design.md` | `design` | by reference: `implements:`, a `design-change` trigger's `paths`, `context:` |
| `*.capability.md` | `capability` | by reference from a definition's `context:` (`mode: use`) and from other capability grainfiles (§6.8) |
| `*.capability-base.md` | `capability-base` | published by the Grain from its providers; by reference (§6.8) |
| `*.asset.md` | `asset` | a sidecar describing another file (§2.3) |
| `*.moved.md` | `moved` | a tombstone, read when a reference lands on the old path (§2.2) |

- **The front matter's `kind:` must match the suffix.** the forge check
  refuses a push that adds or changes a file whose `kind:` disagrees with
  its name, and `grain lint` reports it as an error on any host (§10.11).
  A file with a known suffix and no front matter is refused the same way.
- **Every other file is an asset** (§2.3): a `README.md`, an image, a
  `.yml` workflow, and a Markdown file whose name has no known kind
  suffix, whatever its front matter says. Front matter alone never makes
  a grainfile.
- **A suffix nobody knows** (`x.widget.md`) is an asset, and `grain lint`
  warns, so a newer schema's kind is visible to older readers without
  being acted on.
- **Names before the suffix** follow §2's rules: a definition
  `2026-10-16-approval-flow.task.md`, its execution in a lane
  `2026-10-16-approval-flow.exec/`, a run `runs/<utc>/run.md`, a comment
  `<utc>-<author>.comment.md`.

**Code-repo design docs become `*.design.md`.** A design doc in a code
repo is a grainfile as soon as it is renamed (`docs/design/hub.md` →
`docs/design/hub.design.md`, leaving `hub.moved.md`), and an asset until
then. bwsalmon/grain's design-doc planner keys on `docs/design/` today:
as a `design-change` trigger its `paths` stay `docs/design/**`, which
matches both forms, so the planner keeps working through the migration
and nothing has to be renamed at once (§6.9, §9.5). We chose the rename
over a per-folder rule ("every `.md` in `docs/design/` is a design")
because that rule would make kind depend on location, which §11.1 needs
not to.

### 2.2 Identity is the path, and moves leave tombstones

- **Within a repo**, a grainfile is its repo-relative path:
  `tasks/4310-carry-approver.task.md`.
- **Globally**, it is the git URL plus that path, which is exactly the
  cross-repo reference form of §3:
  `https://silo.example.com/git/hub/intake.git//tasks/4310-carry-approver.task.md`.

An alias (`intake:tasks/4310-carry-approver.task.md`) or a relative path
(`../4310-carry-approver.task.md`) names the same identity once it is
resolved. The **canonical form** is the URL form with the alias
expanded, `.` and `..` removed, the path's leading `/` dropped, and no
`@gitref` or `#anchor`. Two references name the same grainfile exactly when
their canonical forms are equal after following redirects (§3).

**Paths are stable by design.** State lives in front matter (§1.2), so
a grainfile never moves to change state. A definition is written once,
where it was proposed; placing it creates an execution in a Grain's lane
(§6.2); that execution is approved, run and closed without its path
changing, except a `git mv` between lanes of the same Grain, which needs
no tombstone because an execution is found by name across its Grain's
lanes (spec §8.3).

**A real move leaves a tombstone.** A rename, a reorganisation, or a
move to another repo leaves a `kind: moved` grainfile beside the old path,
named for it (the old path with `.md` replaced by `.moved.md`, §2). Archiving is not a real
move, and leaves none (§4.3):

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

The grainfile at its new path carries `moved_from:`, the list of its
earlier addresses, oldest first. Readers follow a tombstone the way a
browser follows an HTTP redirect, with a bounded chain length and cycle
detection (§3). A move within one repo is a single commit: the tombstone
and the new file together. A move between repos writes the new home
first and the tombstone second (§8.5).

**No path reuse.** A tombstoned path is permanently that grainfile's old
address. the forge check refuses any change to a tombstone and its deletion,
so no new grainfile can ever be created there (§8.3). **No grainfile is
deleted**: a grainfile leaves its path only by becoming a tombstone, by
a lane move or a fold (§5), or by archival. Assets are not grainfiles,
and are deleted or moved like any file (§2.3), except a definition's
`assets/` once an execution has been created from it (§6.1).

**Repo moves.** References name repos by URL, usually through an alias
declared in the referring repo's `.grainconfig` (§8.1). Moving a repo
therefore means:

- changing the alias in the `.grainconfig` files that declare it, one
  line each, and any root entry that names the URL directly;
- optionally setting `moved_to: <new git URL>` in the old repo's
  `.grainconfig`, as a **repo-level redirect**. The old repo stays
  readable. A reader that resolves into a repo whose `.grainconfig` has
  `moved_to` retries at the new URL with the same path, and that counts
  as one hop.

**Earlier names are kept for search, not resolution.** `aka:` lists an
grainfile's identities from before migration, such as `grain:eu/task/4310`,
`silo:hub/task/812` or `repo:template:bwsalmon:grain:prune-packages`.
Silo indexes them so the old names can be looked up. References never use
them.

### 2.3 Assets: every other file

**Any file in a repo can be referenced**: an image, a PDF, JSON or CSV
data, a diagram, a binary, or Markdown without `kind:` front matter. Only
Markdown files with `kind:` are **grainfiles**. Every other file is an
**asset**. An asset's identity follows the same rule as a grainfile's: its
repo plus its path (§2.2), named by a relative path, an alias or a git
URL, plus an optional `@ref` (§3). `shared:designs/img/claims-flow.png`
is an asset. So is a design doc that has no front matter yet (§9.5).

What an asset is for, and what it is not:

- **Reachable by reference.** A `context:` entry (§7), a link or image
  in a Markdown body, or a sidecar's `asset:` (below) may name one. A
  field that expects a kind (`template:`, `depends_on:`, `implements:`,
  an execution's `definition:`) never resolves to an asset: an asset
  there is reported as the wrong kind, as a template named in
  `depends_on` would be.
- **Materialised only when a task lists it** in `context:`, or when an
  entry's `depth:` reaches it from a Markdown file that links to it, such
  as an image embedded in a design doc by a relative link (§7).
- **Never active.** Assets are not work. A root entry lists only its
  type's kind suffix (§4.2), so an asset under a root's folder is never
  listed. Nothing ranks, places, fires or runs an asset.
- **Never executed** by Silo or a Grain: an asset is data, whatever its
  name or mode bits say (§12.24).

**Tombstones work for grainfiles only.** A `kind: moved` tombstone is a
Markdown grainfile, and cannot sit at an asset's path (`img/flow.png`). An
asset that moves leaves nothing behind. References to it dangle, unless
it has a sidecar, which moves with it and leaves a tombstone of its own.
So **give an asset that others reference from other repos a sidecar**,
and reference the sidecar. Within one repo, a move is one commit, and the
same commit fixes the links.

#### Sidecars: optional metadata for an asset

An asset has no front matter, so its metadata, where it needs any, is in
a **sidecar**: a `kind: asset` grainfile that references it.

```yaml
---
kind: asset
schema: grain/asset@v1
asset: claims-flow.png              # the asset: a reference, usually relative
title: How a signed claim travels from Silo to a Grain
alt: >-
  Silo signs an execution's attestation; the Grain verifies it against
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

This is [`hub-shared/designs/img/claims-flow.png.asset.md`](../examples/grainfiles/README.md).
Schema: [`schemas/asset.schema.json`](../schemas/grainfiles/asset.schema.json).

- **Where it lives.** By convention, beside the asset, named for it with
  `.asset.md` appended: `img/claims-flow.png.asset.md` describes `img/claims-flow.png`.
  A reader finds that sidecar by opening one path, with no index. A
  sidecar may live elsewhere, describing an asset in a repo its author
  cannot write. Silo's derived index (a cache, §5) maps each asset to every
  sidecar that names it. Where the two disagree, the one beside the asset
  wins for display.
- **What it carries:** `asset` (or `external`, below), `title`,
  `description` or a body, `alt` (required for an image someone will
  show), `media_type`, `license`, `provenance` (`author`, `created_at`,
  `source`, `generated_by` for a generated file, `derived_from` as a
  reference), and optionally `bytes` and `sha256`. the forge check checks that
  `asset` resolves to a file that is not a grainfile, and checks `sha256`
  and `bytes` against the blob when they are present.
- **What it is not.** A sidecar is metadata about data. It is never
  ranked, placed or run, and no root type lists it.
- **Optional.** No asset needs one. An image in a design doc needs only
  the doc's own alt text in the link.

#### Size and storage

Assets live in git, and every clone carries every version of them, so
grain repos keep them small:

| What | Recommended | Enforced |
| --- | --- | --- |
| an image or a diagram | ≤ 1 MiB (PNG, JPEG or WebP; SVG as data only, §12.25) | the forge check warns over 1 MiB |
| any one asset | ≤ 10 MiB | the forge check refuses a blob over 10 MiB, which is also its push cap (§8.6) |
| a repo's assets, all versions | ≤ 1 GiB | Silo's health view warns |
| anything larger | an external artefact (below) | the materialiser's caps (§7) |

- **Git LFS is allowed where the host supports it**, because it is still
  git: a pointer file in the tree, the content on the host's LFS
  endpoint. The materialiser fetches an LFS object through the Grain's
  git proxy when the host serves LFS and the Grain's policy allows that
  host, under the same size caps as any file. A pointer that cannot be
  resolved is skipped and recorded (§7), and never handed over as if
  it were the content. **Nothing in this design requires LFS**: no
  grainfile may be an LFS pointer (the forge check refuses a `*.md` with `kind:`
  that is one), and Silo's own git host does not serve LFS in the stages
  of §15.
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
  references work as for any grainfile. The `sha256` makes the reference
  immutable: what the URL serves must match it, or nothing is
  materialised. `url` is `https://`. A Grain may also accept `gs://` or
  `s3://` where a capability gives it a credential for that store. Only
  the Grain fetches it, never the sandbox, and only when the Grain's
  network policy allows the host (§7, §12.21).

#### Attachments become assets

Grain today stores a file attached to a task or to a comment as a blob
on that task or comment, with its file name and content type. With task
repos, **an attachment is an asset stored next to the grainfile it belongs
to, and referenced from it**:

| Attached to | Stored at | Referenced from |
| --- | --- | --- |
| a task | `tasks/<name>/attachments/<file name>`, in the folder named for the task beside `tasks/<name>.task.md` | the task's `context:`, as a `read` entry (`<name>/attachments/<file name>`), because only `context:` materialises (§7); and from the body, where the prompt mentions it |
| a comment | `comments/<utc>-<author>/<file name>`, a folder named for the comment beside its file | the comment's `attachments:` list, references relative to the comment, and its body |

- **New attachments** are commits like everything else. Silo's UI and API
  add the file in the same commit as the task or comment it belongs to,
  so the intake's `GRAINOWNERS` line `/tasks/ @authenticated(create)`
  covers it, and the forge check limits a new task's assets to its own
  `<name>/attachments/` (§8.2). A Grain
  commits the attachments of a reply in its root with the reply (§6.4).
- **They are part of the task's spec.** A relative `context:` entry reads
  the task's own commit (§7), and the forge check freezes a task's
  `attachments/` with the rest of its spec on approval (§6.1). So what the
  approver saw is what the agent gets.
- **The migration**, in `grain tasks export` (§13): each attachment is
  written at its path above, with its file name made safe (Unicode NFC,
  no `/`, `\`, `..` or control characters, at most 255 bytes, and `-2`,
  `-3`… on a collision). Each task attachment gains a `context:` entry. A
  content type the extension does not imply gets a sidecar with
  `media_type`. An attachment over 10 MiB goes to the deployment's
  artefact bucket and is exported as an external sidecar with its
  `sha256`.

### 2.4 Schemas and versioning

Each kind has a JSON Schema (2020-12) in
[`schemas/grainfiles/`](../schemas/grainfiles/): `common` (shared
definitions), `config`, `task`, `exec`, `run`, `comment`, `template`,
`trigger`, `gate`, `design`, `memory`, `moved`, `asset`, `capability`,
`capability-base` and `lifecycle`, plus `grainconfig` for
the `.grainconfig` YAML file. `GRAINOWNERS` is not YAML and has no JSON
Schema; its grammar is spec §16.2.

The spec is authoritative for them, and `grainlint --self-test` fails
when the spec, the schemas and the linter drift apart. What the main
ones hold (the spec has every field):

- **`config`** (`grain.config.md`): `grain` (or, in a team's intake,
  `team`), `owner`, `backlog` (`mode`, `expedite`, `lanes` of `{name,
  path, ref_mode, weight, ignore}`), `triggers` and `memory` (lists of
  `{path, ref_mode, ignore}`, and `write` on a memory entry) and
  `limits` (§4.2).
- **`grainconfig`** (`.grainconfig`, YAML): `owner`, `host`,
  `default_ref`, `ref_policy`, `aliases`, `grain_folder`,
  `orphans`, `limits`, `archive_repo`, `moved_to` and `require_signed`
  (§8.1). Write rules are in `GRAINOWNERS`, not here.
- **`task`** (a definition): what to do (`title`, the body, `context`,
  `template`, `depends_on`, `implements`, `constraints`, `lifecycle`),
  who proposed it (`submitter`, `filed_by`, `submitted_via`, `origin`),
  `aka` and `moved_from`. No state, no approvals, no pointers (§6.1).
- **`exec`** (`<name>.exec/exec.md`): `definition` and
  `definition_sha256`, `inputs`, `instantiated_from`, `state` and
  `lifecycle`, `approved_by`, `capability_approvals`, `cancel`, `rank`,
  `priority`, `expedite`, `depends_on`, `previous`, `superseded`,
  `trigger`, the `attestation`, and who created it (§6.2).
- **`run`** (`runs/<run>/run.md`): `deployment`, `status`, `context`
  (what was materialised and skipped), `capabilities`, `waiting`,
  `refusal` (naming an entry, a capability link, a reducer or a secret),
  `superseded`, and `closed` (§6.3).
- **`comment`**: `author`, `at`, `in_reply_to`, `supersedes`,
  `attachments`, `question`; its folder is its parent (§6.4).
- **`template`**: `inputs` (with `type: ref` for references), `context`,
  and the keys `.grain/` templates have (§6.5).
- **`trigger`**: `on` (one or more of `schedule`, `push`, `merge`,
  `design-change` and `manual`, each with its own options; `task-closed`
  reserved and refused), `fires` (a template with `inputs`, `bind`,
  `context` and `per`, or a `workflow` with its `branch` and `review`),
  `fires_in`, `enabled`, `limits` and `derived_from`. It refuses the
  merge-gate keys (§6.6).
- **`gate`**: `match` (`paths`, `paths_ignore`, `branches`, `repos`),
  `requires` (`human`, `security_review`, `second_review`) and
  `sensitive`, refusing every trigger key (§6.7).
- **`capability`**: a base reference, a reducer chain (`language:
  builtin` with `type` and parameters, or `language: anselm` with an
  opaque `version` and `body`), an optional `approval`, and secret slots
  bound by name. **`capability-base`** is the declaration a Grain
  publishes for each base it implements: its dimensions with their
  maximums and where each narrows, what it materialises, what it implies,
  its proxies, its secret slots and its approval (§6.8).
- **`memory`**: `title`, `applies_to` (`repos`, `teams`, `kinds`),
  `updated_by`, `updated_at`, and a body (§6.10).
- **`design`**, **`moved`**, **`asset`** and **`lifecycle`**: §6.9,
  §2.2, §2.3 and §6.12. The shared definition of a `context:` entry and of a
  reference string is in `common`.

Every schema's `kind` must equal the file's suffix (§2.1), which the hook
and lint check outside JSON Schema. The principal a trigger files as in
Silo is `silo:trigger`.

- **Every file names its schema** (`grain/task@v1`). A minor addition,
  an optional field, keeps `v1`. Readers ignore fields they do not know
  and preserve them when they rewrite a file.
- **A breaking change is `v2`.** Loaders keep a decoder for every
  version they ever accepted, and upgrade on read. Old files therefore
  keep loading, and are never rewritten just to bump the version.
- **The forge check takes the newest version both ends know.** It fails a
  change in a version newer than it knows. A Grain advertises the
  versions it knows in `hello`, and Silo writes nothing newer into the
  folders that Grain reads.
- Front-matter keys are `snake_case`, like the hub's JSON. For `template`
  and `trigger`, the loader also accepts the hyphenated keys of
  `.grain/` declarations (`auto-merge`, `auto-approve-proposals`) as
  aliases, so those keys move over verbatim (§9.3). The `on:` sources
  keep Actions' and grain's hyphenated names (`design-change`,
  `task-closed`).
- **Two places refuse what they do not know, rather than ignoring it:**
  a trigger's `on:` source and a gate's `requires:` key. A trigger with
  an unknown source does not fire, and a gate with an unknown
  requirement holds every change it matches until a reader that
  understands it is deployed. Ignoring either would silently change what
  runs or what lands.

## 3. References: plain git, no forge

A grainfile names another grainfile, an asset, a folder or a repo with a
**reference**: a string that `git clone` and a path can resolve, with no
forge API.

```
ref     = [ repo "//" ] path [ "@" gitref ] [ "#" anchor ]
        | alias ":" path [ "@" gitref ] [ "#" anchor ]
repo    = a git URL: https://…, ssh://…, file://…      (no scp-style user@host:path)
path    = in-repo path, "/"-rooted, or relative to the referring file
gitref  = branch | tag | 40-hex sha                     (the last "@" after the "//")
anchor  = a heading slug or a requirement ID            (HS-3, D-2)
```

Examples:

- `../templates/prune-packages.template.md`: the same repo, relative to the
  referring file.
- `https://silo.example.com/git/hub/shared.git//designs/hub-intake.design.md@main#HS-3`:
  across repos, by the repo's URL. protected branches, GitHub, Gerrit and a
  bare `git daemon` all use the same form, because the URL is just a
  `git clone` argument. The `//` separator is the one Terraform uses, and
  it is the first `//` after the URL's `scheme://authority`.
- `shared:designs/hub-intake.design.md#HS-3`: an **alias**, declared in the
  referring repo's `.grainconfig` (§8.1). It resolves to exactly the URL form. An alias
  that is not declared is an error, never a guess.

**A reference in front matter is a string** in this grammar. Where an
edge records more than its target, it is a grainfile whose `ref:` is that
string, as in `template: {ref, rev, inputs}`. The path is the target's
identity (§2.2), so there is nothing else to check: a reference names
exactly the grainfile at that address, or the grainfile a tombstone there
redirects to. The Markdown body may use bare refs in ordinary links.
Those are for humans and carry no meaning.

**Resolution:**

1. **Canonicalise.** Expand the alias, and resolve a relative path
   against the referring file, to get the canonical global identity
   (§2.2). The aliases are those of the referring repo's `.grainconfig`
   (§8.1), one file at the repo root; nothing is discovered by walking
   folders. A `/`-rooted path is rooted at the repo.
2. **Unpinned references** resolve to the tip of the target repo's
   `default_ref` at the moment they are read. Anything recorded for
   traceability records the commit sha that was actually used: a
   definition's `template.rev` and `implements[].rev`, an execution's
   `definition` and `instantiated_from`, and a run's
   `closed.design_revs`. So a reference reads live and is recorded
   pinned.
3. **Pinned references** (`@v3`, `@<sha>`) resolve to exactly that ref.
   A branch pin follows the branch's tip.
4. **Redirects.** If the target repo's `.grainconfig` has `moved_to`,
   retry at that URL with the same path. If the path is missing and a
   tombstone (the path with `.md` replaced by `.moved.md`) sits beside it, follow its `to:`,
   resolved relative to the tombstone. If neither, try the archive
   mapping (§4.3), which is not a hop. Each redirect is one hop:
   - a reader follows **at most 8 hops** in total;
   - it keeps the set of canonical identities it has visited. Arriving at
     one twice is a **redirect loop**;
   - a loop, or a chain longer than 8, is reported and treated as
     dangling.

   An execution's `definition` is pinned and never rewritten; a reader
   follows its chain, and Silo shows where the definition is now.
5. **Cycles:**
   - `depends_on` cycles are rejected on write in protected branches. A
     reader treats every execution on a cycle as blocked and reports the
     cycle.
   - Include/import cycles are rejected at render time.
   - Redirects are bounded as in step 4.
6. **Dangling references:** the target is missing, the reader may not
   read it (§12.4), or a redirect chain breaks. the forge check refuses a
   *new* reference that does not resolve with the pusher's rights (§8.3).
   A reference that resolved when it was written can still dangle later,
   because no transaction spans repos (§8.5). So readers handle it at
   read time:
   - a dependency stays **blocked**, with the reason shown;
   - a template reference makes the execution **uncreatable**;
   - an `implements` reference is shown as **unresolved** in
     traceability;
   - an unresolvable `definition` is shown unresolved, and the execution
     still runs from its snapshot (§6.2).

   Silo re-checks references on every push it receives, and lists the
   dangling ones in the repo's health view.

**One resolver.** These steps are one function, `resolve`, with a gate
supplied by its caller. Root entries (§4.2) and references call it with
the reader's credential as the only gate; a snapshot's `context:` calls it with the
Grain's policy as the gate too, checked before every fetch (§7).

**Indexes.** Resolving a reference needs no index, because a reader
opens the path. Silo's derived indexes (definition → executions,
execution → dependents, requirement → definitions, asset → sidecars,
`aka` → path) are caches for its views, never needed to resolve
anything (§5).

**The edges:**

| Edge | Field | Notes |
| --- | --- | --- |
| execution → definition | `definition: <ref>@<sha>` + `definition_sha256` | the commit and bytes the snapshot was copied from (§6.2) |
| execution → folder template | `instantiated_from: <ref>@<sha>` | set when an instantiation created it (§6.13) |
| execution → the execution it re-places | `previous: <ref>` | set on re-placement (§10.7); the old one's `superseded.by` names the new |
| execution → dependencies | `depends_on: [<ref>]` | "done" means the named execution is in a `done`-category state (§10.8) |
| execution → trigger of origin | `trigger: {ref, on, key, fired_at, event}` | written by whoever fired it (§6.6) |
| task → template | `template: {ref, rev, inputs: {…}}` | `rev` is the template commit rendered; `inputs` are the values used |
| definition → dependencies | `depends_on: [<ref>]` | mapped to executions when executions are created (§5) |
| task → design requirements | `implements: [{ref: …#HS-6, rev, hash}]` | the requirement and its revision (§6.9) |
| task → the run that filed it | `filed_by: {grain, run: <ref>}` | an agent's upward filing (§10.9) |
| trigger → template | `fires: {template: <ref>, inputs, bind}` | unpinned, so a firing uses the template's current revision |
| trigger → workflow | `fires: {workflow: <path>}` | a plain path to a workflow file in the trigger's own repo (§9.4) |
| trigger → watched designs | `on.design-change.designs: [<ref>]` | read on each event; adds no name (§6.9) |
| gate → its review templates | `requires.security_review.template`, `requires.second_review.template` | optional; grain's built-in otherwise (§6.7) |
| comment → the comment it answers | `in_reply_to: <ref>` | may cross from an execution's thread to its definition's (§6.4); what a comment discusses is the folder it is in, not a field |
| tombstone → new address | `to: <ref>` | the moved grainfile lists the old address in `moved_from` |
| Grain config → its active roots | `backlog.lanes[].path`, `triggers[].path`, `memory[].path`, each with `ref_mode` and `ignore` | **the only edges that make anything active** (§4.2) |
| trigger → the trigger it was customised from | `derived_from: <ref>` | provenance only: inherits nothing (§6.6) |
| repo → its new URL | `.grainconfig` `moved_to` | a repo-level redirect |
| task → what its sandbox holds | `context: [{ref, mode, path, depth}]` | **the only edge that materialises anything** (§7); a relative entry reads the task's own commit |
| comment → its attachments | `attachments: [<ref>]` | assets beside the comment (§2.3) |
| capability → its base | `base: {capability: <ref>}`, or `{type: <base type>}` | narrows that grainfile further; a chain is resolved with cycle detection and a depth limit, and every link must be a sanctioned source (§6.8) |
| task → a capability it requests | `context: [{ref, mode: use, reducers}]` | a request, never a grant: the Grain's policy decides (§6.8) |
| sidecar → its asset | `asset: <ref>`, or `external: {url, sha256}` | metadata only (§2.3) |

## 4. Typed active roots

What a Grain *does* is decided in one file it owns, `grain.config.md`.
This section says what that file activates, and how; everything else a
Grain touches is reached by reference (§3).

### 4.1 Active roots and references

There are two ways a Grain can reach a grainfile, and they are deliberately
different:

- **Being in an active root means it is enabled.** A Grain's root
  configuration, `grain.config.md` (§4.2), names folders of three types.
  The Grain *acts* on what they hold: it runs the `<name>.exec/`
  executions in its **backlog** lanes, fires the `*.trigger.md` triggers
  in its **trigger** roots, and gives the `*.memory.md` documents in its
  **memory** roots to its runs.
- **Everything else is reached only by reference.** It is used when
  something needs it, read-only, and is never active by itself.
  Templates, design docs, the definitions executions were copied from,
  capability definitions, `context:` targets and `depends_on` targets
  are all reached this way. A reference resolves for the grainfile that
  holds it, with the reader's own rights (§3, §12.4). It never makes
  anything active, and never materialises anything except through a
  snapshot's `context:` (§7).

**The test** for which of the two something needs: *if this target
changed or disappeared, should the Grain start or stop **doing**
something?* If yes, it must be in a root. If no, it is a reference.

- A new trigger in the team's `triggers/` should start firing on every
  Grain that points a trigger root at it, so trigger folders are roots.
- A template that a trigger fires should not, by being edited, start
  anything; the trigger's own `fires.template` names it, so it is a
  reference.
- A new execution in `eu`'s `hub/` lane should make `eu` run something,
  so that folder is a backlog root of `eu`. The definition it was copied
  from is a reference: it exists whether or not anyone runs it, and
  editing it changes nothing that runs.
- A capability definition changes what a run *may* be given, never
  whether something runs. It is a reference from a snapshot's `context:`
  (§6.8), allowed or refused by the Grain's local policy.
- A gate holds merges into the repo it belongs to. It is not a Grain's
  root at all: the merge queue reads it from the merged-into repo's own
  `.grain/` (§6.7).

There are **exactly three root types**: `backlog`, `triggers` and
`memory`. A root entry is one folder, and the grainfiles in it are
active for the type the entry declares, nothing else. Nothing is
transitive: a grainfile in a root never activates anything by naming it.

### 4.2 A Grain's root configuration: `grain.config.md`

A Grain's **root** is a folder in a repo: a whole repo
(`grains/eu.git`), a subtree of one (`.grain/` in its owner's own repo),
or a local `file:///var/lib/grain/tasks.git`. **`grain.config.md` at that
path** is the Grain's root configuration. It is a grainfile like any other,
so it is versioned, reviewed and owned through `GRAINOWNERS` (§8.2), and
the forge check validates it against
[`schemas/config.schema.json`](../schemas/grainfiles/config.schema.json):

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
    - name: hub
      path: hub/                      # Silo creates executions here, as the placing user
      weight: 3
    - name: own
      path: backlog/                  # relative: this repo, the same commit as this file
      weight: 1
      ignore: ["drafts/"]             # gitignore syntax, relative to the entry's folder

triggers:
  - path: triggers/
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

This is [`grain-eu/grain.config.md`](../examples/grainfiles/grain-eu/grain.config.md).

**An entry** names one folder:

- `path`: a relative or `/`-rooted path in the Grain's own repo, read at
  the same commit as `grain.config.md` (a lane's path is always one of
  these: executions live only in the Grain's own repo); or, for a
  trigger or memory entry, an alias or git URL plus `@ref` (§3), with
  **`ref_mode`**: `track` (the `@ref` is a branch, read at its tip on
  every fetch: a team's triggers) or `pin`
  (a tag or commit: a reviewed release of an org's memory). A folder path
  ends in `/`. An entry may also name a single file, which activates that
  one grainfile.
- `ignore`: optional patterns in **gitignore syntax**, relative to the
  entry's folder (`granule-pull.trigger.md`, `experimental/`,
  `!keep.trigger.md`). They only ever remove from what the entry
  activates.
- In `backlog.lanes` only: `name` (a stable label for status and for
  Silo's views), `weight` (in `weighted` mode).
- In `memory` only: `write: true` on at most one entry, in the Grain's own
  repo, which the Grain's memory agent writes (below).

**Each entry is self-describing.** It activates exactly:

> the files in its folder's subtree whose suffix is the root type's kind
> (for a lane, the `<name>.exec/` folders),
> **minus** the paths its own `ignore:` matches, **minus** anything under a
> folder named **`archive/`** (a built-in convention, §4.3).

Nothing is inherited from parent folders, from other entries or from
other files. So what an
entry activates can be told from **the folder plus that one entry**, and
an edit anywhere else can never change it. Grainfiles of other kinds in the
folder are not active for this entry; a `README.md` or an image there is
an asset, and nothing.

**Overlapping entries.** Two entries of one type may name nested folders
(`backlog/` and `backlog/gated/`, to give gate reviews a lane of their
own). A file both would activate belongs only to the **most specific**
entry, the longer path, so nothing is active twice and a reference's lane
is still determined by its folder (§6.2). The same file in two root
*types* is fine and meaningless: an execution folder is only ever a
backlog grainfile.

**Producers control their folders by their contents.** A team that does
not want a trigger to fire anywhere does not put it in `triggers/`, or
moves it to `triggers/archive/`. A Grain that does not want one of a
team's triggers adds it to its own entry's `ignore:` (§10.11). Neither
needs the other's cooperation, and neither can change what the other's
entry means.

**Backlog lanes.** Each lane is one entry. Ranks compare within a lane
only, and the lanes are ordered by their position in `lanes`, with
`mode` choosing strict priority, smooth weighted round-robin or
round-robin across them (§6.2). An entry is a lane whoever creates
executions in it: the Grain's owner in `backlog/`, Silo in `hub/`, a
colleague `GRAINOWNERS` lets create there.

**Trigger roots.** Every `*.trigger.md` an entry activates fires, as
§6.6 describes: its `on:` sources are registered, and its
firings are filed where §6.6 says. The trigger's identity is still its
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
then the path orders them. The kind itself is §6.10.

**How a Grain learns its root path.** The only local setting a Grain has
about grain repos is **where its `grain.config.md` is**: a git URL, a path
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
  command, acting as the owner, commits the link** (§10.9): it shows the
  diff to `grain.config.md` that adds Silo's lane and trigger entry, and
  commits it with the owner's credential only when they confirm. Silo
  never writes the file. (The diff adds Silo's `hub/` lane and the
  `GRAINOWNERS` rule letting the team create executions in it.)
- `grain root set <url>//<path>@<branch>` moves an existing Grain to
  another root. The Grain refuses a root whose `grain.config.md` names a
  different `grain.id`.

A root without `grain.config.md` is an error the Grain reports at start,
and it acts on nothing. There is no default root set.

### 4.3 Archival: the `archive/` convention

A Grain's lanes and a team's `tasks/` grow without bound, and nothing
needs the closed ones to be listed. **Closed executions (with their
threads and runs) and retired definitions move under an `archive/`
folder**, and any folder named
`archive/` is skipped by every root entry (§4.2), by the orphan check
(§11.3) and by default listings in Silo's UI.

**The archive mapping, not tombstones.** Archiving is a move, and a moved
grainfile normally leaves a tombstone (§2.2). For archival that would leave
one tombstone per closed task forever, which is exactly the growth
archiving is meant to stop. Instead, the move is **structural**:

> a grainfile at `<top>/<rest>` is archived to `<top>/archive/<rest>`, where
> `<top>` is the first folder of its path in its grain folder (`tasks/`,
> `hub/`, `backlog/`, `triggers/`, `memory/`).

So `tasks/4310-carry-approver.task.md` becomes
`tasks/archive/4310-carry-approver.task.md`, and the whole folder
`hub/4310-carry-approver.exec/` becomes
`hub/archive/4310-carry-approver.exec/`, thread, runs and assets
included. A reference to the old path that finds
nothing there tries the **archive mapping**, one deterministic extra
lookup, before it is dangling (§11.1). No tombstone is written, so a
repo's tombstones grow only with real renames.

- **Identity is unchanged.** The grainfile's identity is still its live
  path; the archive path is where it is stored. References keep naming
  the live path, and Silo's derived indexes (§5) show both.
- **No path reuse.** The forge check refuses a new grainfile at a live path whose
  archive path exists, as it refuses one at a tombstone (§8.3). A
  reopened execution is moved back, by its owner, in one commit.
- **Who archives.** A path's owners in `GRAINOWNERS` (§8.2): Silo
  archives an intake's retired definitions after a retention period (90
  days by default, in Silo's settings), and a Grain archives its own
  closed executions. Each archive is one commit with `move` on the
  paths.
- **Retiring a trigger** is moving it to `triggers/archive/` (§10.11):
  every Grain whose trigger root names `triggers/` stops firing it, and
  the definition is still readable at the same identity by the firings
  that name it.
- **A really old archive** may be split into a separate repo: the
  repo's `.grainconfig` sets `archive_repo: <git URL>`, and the mapping
  looks there second. This is optional and not needed in the stages of
  §15.

## 5. One-to-many relations: containment

**A parent contains its children.** Every one-to-many relation that
decides anything is a folder inside its parent, and a reader finds the
children by listing it. Nothing points down at children, and nothing
points back up at a parent: there are no `thread:`, `runs:` or
`placements:` fields and no back-pointers (spec §11 is normative).

| Parent | Children | Folder | Written by |
| --- | --- | --- | --- |
| a definition, `<name>.task/` | its discussion | `thread/` | commenters, through Silo with their own delegated tokens, in the definition's repo |
| a definition | its attachments | `assets/` | the definition's owners |
| an execution, `<name>.exec/` | its discussion | `thread/` | commenters, in the deployment's repo |
| an execution | its attempts | `runs/<run>/`, each with `run.md` | the deployment, under its own identity |
| a run | the agent's thread and the replies relayed to it | `runs/<run>/` | the deployment |
| an execution | its attachments | `assets/` | its creator |
| a deployment's `grain.config.md` | its lanes, trigger folders and memory folders | its root entries (§4.2) | the deployment's owner |
| a lane | its executions | the lane's folder | whoever `GRAINOWNERS` lets create there |
| the registry's `grain.config.md` | the registered grain folders | `folders:` (§8.7) | the registry's maintainers |
| a design | its requirement sections | `requirements:` and its anchors (§6.9) | the design's owners |
| a trigger | its firings | `fired/<trigger>/` under the lane it fires into | whoever fires it |
| a folder template | its member definitions | its own directory (§6.13) | the template's owners |

**Why containment.** The first draft of this design navigated from the
parent by **pointers**: a task named its `thread:` and `placements:`
folders, a backlog reference named its `runs:` folder, and every child
carried a back-pointer that lint checked against the folder it was in.
Each placement wrote a reference in Silo's folder for the Grain, a
pointer in the task's repo, and a runs folder in the Grain's root, so a
backlog move touched several repos and the hub wrote into task repos
(Appendix A). With containment, everything that happens to one piece of
work is in one folder in one repo, and a child cannot disagree with its
parent because it has nowhere else to say who its parent is.

**A definition with no discussion stays one file.** `<name>.task.md` is
a definition with no thread and no assets. When its first comment
arrives, the writer **folds** it into `<name>.task/task.md`, byte for
byte, in the same commit as the comment. A fold is not a move: there is
no tombstone, references to `<name>.task.md` still resolve (to the
folder form, spec §9.3), and the forge check authorises it as part of
creating the comment.

**Two threads, two scopes.** A definition's thread is about the
definition ("this spec is wrong", design questions); an execution's
thread is about that execution (approval, what this deployment should do
differently); a run's comments are the agent's conversation in that
attempt. The definition's thread is never copied into an execution.
Silo and grain show both on an execution's page: the execution's
thread and its runs' comments, plus the definition's thread read through
`definition` at the tip of the definition's repo, **each comment
labelled with its scope** (`definition`, `execution`, `run <run>`),
either as sections or merged into one timeline by time (spec §11.2).

**Derived indexes are the exceptions.** Definition → executions, design
→ implementing definitions, and execution or definition → dependents are
many-to-many and cross ownership: a definition's owners do not write in
the deployments that run it, the owner of a design does not write every
definition that implements it, and a task's author does not own its
dependents. A folder would force a write into someone else's repo for
every new edge. So these are **derived indexes**: caches, rebuildable
from the folders alone and keyed by the tree and blob hashes they were
computed from. Nothing that decides what runs uses them: dependency
satisfaction is checked forwards from the dependent's `depends_on`
(§6.12), traceability forwards from `implements`, and the "definition
edited after a snapshot" warning from the execution's own
`definition_sha256`. Silo keeps them over the registry's grain folders,
with template → instances, lifecycle → executions, asset → sidecars and
`aka` → path. A Grain keeps the same kind of cache for its root (§11.2).

**Worked flows.**

- *A person comments on a definition:* Silo commits
  `<name>.task/thread/<utc>-user-<slug>.comment.md` in the definition's
  repo with the person's delegated token, folding the definition first
  if it was one file; the forge check checks `author` against the forge
  identity and `GRAINOWNERS` for `thread/`.
- *A person comments on an execution:* the same, in
  `<name>.exec/thread/`, in the deployment's repo.
- *An agent comments during a run:* the Grain writes
  `runs/<run>/<utc>-agent-<grain>.comment.md` inside the execution, in
  its own root and under its own identity; a reply through Silo is
  relayed to the Grain, which commits it there.
- *Viewing an execution, by listing only:* read `exec.md` and the
  snapshot; list `thread/` and each `runs/<run>/`; resolve `definition`
  and list its `thread/`; label and merge. At most two repos, no search.
- *Moving within a backlog:* a `rank` edit in one `exec.md`, or a
  `git mv` of one folder to another lane, in the deployment's repo only.
- *A misplaced comment caught by lint:* a `*.comment.md` outside every
  `thread/` and run folder is GF007 and fails the forge check (fixture
  `tools/grainfiles/testdata/bad/gf007-reserved-folder`).

**Alternatives rejected:** pointers to child folders, with back-pointers
and the lints to keep them honest (above, and Appendix A); back-reference
scans, which cost O(repo) (or O(every repo a reader can see)) and whose
answer depends on which repos were searched; a global comments folder,
which needs a scan or an index to answer "this execution's comments";
and executions that read the live definition, which would let an edit
change what an approved execution runs without review where it runs.


## 6. The kinds

Each kind is a grainfile with its own suffix and schema (§2.1, §2.4).
This section describes them one at a time: what each holds, who writes
it, and how it is reached. The relations between them are §5's.


### 6.1 Task definitions (`kind: task`)

A **definition** says *what* to do: the title, the prompt, the inputs a
template declares, `context:`, constraints and capability requests. It
**holds no state and no runs**: whether it is approved, running or done
is a fact about one execution of it (§6.2), not about the definition. It
is written once, where it was proposed: a team's intake, a code repo's
`.grain/` (§9.2), a shared library, or a Grain's own `tasks/`. It may be
executed any number of times, on any number of Grains, each execution a
folder in that Grain's lane.

```yaml
---
kind: task
schema: grain/task@v1
aka: [grain:eu/task/4310]
title: Carry a hub task's approver on its offer and attestation
team: hub
context:                    # what its sandbox holds (§7); the first write entry is its repo
  - {ref: "https://github.com/bwsalmon/silo.git@main", mode: write}
  - {ref: "shared:designs/hub-intake.design.md"}
submitter: user:bsalmon@google.com
implements:
  - {ref: "shared:designs/hub-intake.design.md#HS-2", rev: <design commit>, hash: "sha256:…"}
created_at: …
---
<the prompt, in Markdown>
```

This file is `intake:tasks/4310-carry-approver.task.md`; with a thread it
is `intake:tasks/4310-carry-approver.task/task.md`, beside `thread/`.
The full examples are in
[`examples/grainfiles/`](../examples/grainfiles/README.md), and
`python3 tools/grainfiles/grainlint.py examples/grainfiles` checks them
against the schemas, resolves every reference, follows the tombstones,
checks every snapshot and dry-runs every folder template.

**Definitions do not change once executed.** An execution runs from its
own snapshot, so an edit to the definition changes nothing that runs and
leaves the definition disagreeing with what ran. A definition SHOULD NOT
be changed after an execution has been created from it: lint warns
(GF046) when its bytes differ from an execution's `definition_sha256`,
and the forge check MAY require an explicit override (a
`Grain-Definition-Edit:` trailer). A changed request is normally a new
definition with `supersedes:`. The definition's `thread/` keeps growing
freely; it is not part of the definition.

**A grain task is a definition plus one execution.** grain keeps a
task's prompt, its state, its one comment thread (`Comment.TaskID`) and
its runs (`Run.TaskID`) together (bwsalmon/grain `pkg/model/task.go`).
Here the prompt is the definition, and the state, the thread and the
runs are one execution's; a migrated grain task becomes both (§13).

### 6.2 Task executions (`kind: exec`)

An **execution** is one folder, in **one repo**: the executing Grain's
root, in one of its lanes. Everything that happens to that piece of work
lives inside it, and nothing about an execution spans repos.

```
hub/2026-10-16-approval-flow.exec/
  exec.md        # definition source <ref>@<rev>, inputs, lifecycle state, approvals, rank, previous/superseded
  task.md        # a copy of the definition as it was when the execution was created (the snapshot)
  thread/        # *.comment.md: discussion of this execution
  runs/          # one folder per attempt: <utc>/run.md and that attempt's agent comments
  assets/        # attachments
```

```yaml
---
kind: exec
schema: grain/exec@v1
definition: "hub:intake/tasks/2026-10-16-approval-flow.task.md@0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
definition_sha256: "sha256:…"   # the definition's bytes at that commit
lifecycle: "hub:intake/lifecycles/security.lifecycle.md@0c4e9a7b…"
state: in-progress              # a state of its lifecycle (§6.12)
created_by: user:dev@example.com
created_at: "2026-10-16T11:50:00Z"
rank: "t"                       # LexoRank-style key, compared bytewise
priority: high                  # optional: urgent | high | normal (default) | low
expedite: false                 # optional; Silo's (at_front)
approved_by: user:sec@example.com
approved_at: "2026-10-16T12:00:00Z"
attestation: <signed claims JWT, hub.md §6.4>
---
```

This is
[`grain-eu/hub/2026-10-16-approval-flow.exec/exec.md`](../examples/grainfiles/grain-eu/hub/2026-10-16-approval-flow.exec/exec.md).

**The snapshot.** Creating an execution copies the definition's
`task.md` into the folder and records `definition: <ref>@<rev>` and the
SHA-256 of the bytes copied. The execution runs from its copy, never
from the live definition: what runs is reviewable in the repo where it
runs, and an approval stays an approval of exactly that text. Inputs are
substituted at creation, as for folder templates (§6.13), and recorded
in `inputs`. Relative references in the snapshot are read as they were
in the definition, relative to the definition's path, so the copy is
byte for byte and its hash checks.

**Placement is creating the folder.** Silo places a definition on a
Grain by committing `<lane>/<name>.exec/` (the `exec.md` and the
snapshot) in that Grain's repo, through the forge API as the person
placing it (§10.3). A Grain creates executions of its own definitions,
and its triggers' firings, in its own lanes. **Reordering** edits `rank`
in one `exec.md`; **changing lanes** is a `git mv` of the folder within
the one repo, with no tombstone, because an execution's name is unique
in its Grain and a reference that misses it finds it by name across the
Grain's lanes. **Re-placement** on another Grain creates a new execution
there, with `previous:` naming the old one; the old one is marked
`superseded` by its own Grain only (§10.7).

**Ranks.** A rank is a string over `0-9a-z` that never ends in `0`, and
ranks compare bytewise. To put an execution between ranks `a` and `b`,
pick any string strictly between them. The midpoint in base 36 works,
lengthening by one digit when `a` and `b` are adjacent. There is always
room, so ranks never need rebalancing. Order comes from `rank`, never
from renaming folders: a reorder edits one field in one file, two people
reordering different executions never conflict, and two reordering the
same one conflict on one field, and the second push rebases.

**Lanes.** A Grain's queue comes from several lanes, each one entry in
`grain.config.md` (§4.2) and each a folder of the Grain's own root:
`hub/`, where Silo creates executions; `backlog/`, the owner's own;
perhaps `ops/` for a colleague `GRAINOWNERS` lets create there. An
execution belongs to the most specific entry that lists it. **Ranks are
compared only within a lane**: two writers' ranks are not comparable,
so merging them would let whichever writer picked more extreme keys win.

**Effective order within a lane:**

1. executions whose dependencies are all satisfied (§10.8), ahead of
   those still blocked;
2. then `expedite: true` ahead of everything else;
3. then `priority`: `urgent`, `high`, `normal`, `low`;
4. then `rank`;
5. then `created_at`, and finally the path.

**Between lanes, the root decides**, with `backlog:` in its
`grain.config.md`:

```yaml
backlog:
  mode: weighted            # strict (the default) | weighted | round_robin
  expedite: lane            # Silo's expedite reorders within its lane (default), or global
  lanes:                    # first is highest
    - {name: hub, path: hub/, weight: 3}
    - {name: own, path: backlog/, weight: 1}
```

- **`strict`**: take the first runnable execution of the first listed
  lane that has one. A lane whose executions are all blocked does not
  hold up the next lane.
- **`weighted`**: smooth weighted round-robin over the lanes that have a
  runnable execution. Each pick, every such lane gains its weight in
  credit; the lane with the most credit (ties in list order) gives its
  next execution, and pays the total. With weights 3 and 1, the hub lane
  gets three picks in four. The credits are runtime state, in the
  Grain's database, not in git.
- **`round_robin`**: `weighted` with every weight 1.

A Grain skips executions not in a `ready`-category state, and those
whose `previous` is not yet fenced off (§10.7).

**Why lane priority is declared in `grain.config.md`:** it is one
decision about the set of lanes, not a property of any one of them, and
the root's owner owns it. Whoever creates executions in a lane cannot
promote the lane, because nothing they write is read for priority.

**Who writes what, per subfolder** (§8.2): whoever may place work in a
lane creates execution folders there (`(create,move)`); the lane's
members own `rank`, `priority` and `expedite`; approvers own
`approved_by`, `approved_at`, `capability_approvals` and the approving
transition; the Grain owns `runs/`, the outcome transitions and
`superseded`; commenters own `thread/`. Silo reorders and approves
through its UI with `ranked_by` and `approved_by` set to the person it
acted for. The owner's last word over their Grain is the lane order, and
whether `hub/` is a lane at all.

**Lanes for triggered and gated work.** A trigger's firing on a Grain
(§6.6) creates its execution in `backlog/fired/<trigger>/`, and the
review a gate files on a Grain's merge queue (§6.7) in
`backlog/gated/`. Both are under the own lane's folder, so by default
they sort with the owner's own work. An owner who wants standing work
weighed against ad-hoc work, or held merges cleared first, adds an entry
for the subfolder (`{name: gated, path: backlog/gated/, weight: 4}`),
which, being more specific, takes those executions out of `backlog/`'s
lane (§4.2).

**Unplaced work** is a definition with no live execution. Silo's
"unplaced" view is the definition → executions index; there is no
ranking of unplaced work in git, because a rank only means something
within one Grain's lane.

**The attestation lives on the execution.** It is Silo's signed claim to
this Grain (`aud`), about this execution (its canonical global
identity), with `spec_digest` computed over the snapshot (§12.7). The
snapshot never changes (the forge check refuses it), so the digest stays
true.

**Fixed at creation.** The snapshot, `definition`,
`definition_sha256`, `inputs`, `instantiated_from`, `previous`,
`created_by` and `created_at` never change; the forge check refuses an
edit (GF052). An execution's own lint: its `exec.md` and snapshot exist
(GF045), it is in a lane (GF047), and its name is unique in the Grain
(GF048).

### 6.3 Runs (`kind: run`)

A Grain records each attempt of an execution **inside the execution**,
as `runs/<run>/run.md`, where `<run>` is the compact UTC time the
attempt was accepted. Lifecycle state stays on `exec.md`; **run status**
is on each `run.md`. The finer detail of an attempt (sandboxes, turns,
logs) is runtime state (§1.2).

```yaml
---
kind: run
schema: grain/run@v1
deployment: eu
status: done                # accepted | running | awaiting_reply | held | done | failed | interrupted | refused | cancelled | superseded
accepted_at: "2026-10-16T13:58:30Z"
started_at: "2026-10-16T13:58:31Z"
closed:
  outcome: done             # done | failed | cancelled | nothing_needed | interrupted
  at: "2026-10-16T14:58:30Z"
  summary: …
  change: {url: …, branch: grain/eu/task-4310, merge_commit: 5623aa25…}
  design_revs: [{item: …#HS-2, rev: …, hash: …}]
  follow_ups: ["intake:tasks/2026-10-16-approval-flow.task.md"]
---
<optional: the closing report, in prose>
```

This file is
`eu:hub/4310-carry-approver.exec/runs/20261016T135830Z/run.md`. Once the
execution has been closed for the Grain's retention period, the Grain
archives the whole execution folder to
`hub/archive/4310-carry-approver.exec/`, and references to it still
resolve through the archive mapping (§4.3).

```
accepted → running ⇄ awaiting_reply | held → done | failed | interrupted | cancelled
    ↘ refused (by local policy, with refusal: {reason, at})
accepted | running | awaiting_reply | held → superseded (the Grain, on re-placement, §10.7)
```

- **`awaiting_reply` and `held`** are coarse statuses for a run held on
  something outside it: a human's answer, a dependency, a held gate, or
  the Grain's own policy refusing a `context:` entry (§7).
  `waiting: {on, entry, reason}` says which.
- **The Grain writes every run**, `superseded` included: an execution's
  `runs/` has one writer. `done`, `failed`, `interrupted`, `refused`,
  `cancelled` and `superseded` are terminal, and a terminal record never
  changes. At most one run of an execution is open at a time (GF053).
- **The Grain never writes into another repo**: not Silo's, not the
  definition's. Silo reads Grains' roots to build its views and derived
  indexes (§5), and mirrors outcomes where they are needed outside git.
- **Re-placement keeps both histories.** The old execution, with its
  runs, stays in the old Grain's repo, `superseded`. The new Grain's
  execution starts with `previous:` pointing at the old one, so history
  shows both.

### 6.4 Comments (`kind: comment`)

A comment has `kind: comment`, `author` (`user:…`, `deployment:eu`, or
`agent:eu/<execution path>/runs/<run>`), `at`, an optional
`in_reply_to`, and a body. Comments are append-only: an edit is a new
comment with `supersedes:`.

**Where a comment lives is its parent.** It is in exactly one of three
places: a definition's `thread/`, an execution's `thread/`, or a run
folder. There is no `task:` or `run:` field to agree or disagree with
the folder; a comment anywhere else is an error (GF007) and is attached
to nothing.

- **Discussion of the definition** ("this spec is wrong", design
  questions) goes in the definition's `thread/`, in its repo, written by
  people through Silo with their own tokens.
- **Discussion of one execution** (approval, "re-run this on
  candidate") goes in the execution's `thread/`, in the Grain's repo.
- **The agent's thread** goes in its run's folder. A person replying to a
  running agent writes through Silo's UI or API; Silo relays the reply
  to the Grain over its node connection, and the **Grain** commits it in
  that run folder, with the person as `author` and `via: hub`.
- After re-placement each Grain's threads stay with its own execution,
  so neither is split or merged.

`in_reply_to` may cross scopes: an execution comment may answer one on
its definition's thread, as in
[`grain-eu/hub/2026-10-16-approval-flow.exec/thread/`](../examples/grainfiles/grain-eu/hub/2026-10-16-approval-flow.exec/thread/20261016T120500Z-user-sec-example-com.comment.md).

Silo shows an execution with its own thread, each run's comments, and
its definition's thread, each comment labelled with its scope (§5); a
definition's page shows its thread and links to its executions.



### 6.5 Templates (`kind: template`): a strict Jinja2 subset

Template front matter declares its **inputs**. Each input has:

- `name`;
- `type`: `string`, `text`, `int`, `bool`, `enum`, `list` (with `of:`
  for its items' type) or `ref`, a reference with an optional `ref_kind`
  (`repo`, `folder` or `file`), which only a `context:` entry's `input:`
  may use and Jinja never renders (§7);
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
  reference under the reader's own credentials (§12.4).

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

### 6.6 Triggers (`kind: trigger`)

A **trigger** says "when X happens, file Y". It is the one kind for
standing work: a cadence is one of its event sources, not a kind of its
own. Its front matter has three parts: **`on:`**, the event sources, each
with its own options nested under it, in the shape of a GitHub Actions
workflow's `on:` (which grain's `.grain/workflows` already follow);
**`fires:`**, what it files; and who fires it and how firings are
approved. Its body is optional notes. Schema:
[`trigger.schema.json`](../schemas/grainfiles/trigger.schema.json).

```yaml
---
kind: trigger
schema: grain/trigger@v1
title: Prune GHCR packages
"on":                                  # quoted: YAML 1.1 reads a bare on as true (§9.3)
  schedule:
    weekly: {day: sunday, at: "04:00"} # or daily: {at}, hourly, every, monthly, cron
    timezone: ""                       # IANA; empty is UTC
    overlap: skip                      # skip | queue | cancel-previous
    catch_up: {mode: latest-only}      # none | latest-only | all (with max, ≤ 24)
  manual: {}                           # "run now"; who defaults to the team's approvers
fires:
  template: ../templates/ghcr/prune-packages.template.md
  inputs: {repo: "https://github.com/bwsalmon/grain.git@candidate"}
team: hub
fires_in: silo                         # silo | grain
auto_approve_proposals: true
enabled: true
---
```

This is [`hub-shared/triggers/prune-packages.trigger.md`](../examples/grainfiles/README.md).

**Event sources (`on:`).** A trigger names one or more. Each fires the
same `fires:`; the task records which one did (below).

| Source | Fires when | Its options | Event fields a firing may bind |
| --- | --- | --- | --- |
| `schedule:` | the cadence comes round | exactly one cadence key, as grain writes them today (`daily: {at}`, `weekly: {day, at}`, `hourly`, `every`, `monthly`, `cron`); `timezone`; `overlap`; `catch_up` | `fired_at` |
| `push:` | a matching branch moves, by any push | `branches` (default: the repo's protected refs, and only protected refs may be named), `paths` and `paths_ignore` (Actions' globs), `group_by` (`none`, `folder` or `file`: one firing per changed folder or file, grain's `{{group}}`), `debounce` | `repo`, `branch`, `from_sha`, `to_sha`, `paths`, `group`, `pusher` |
| `merge:` | a reviewed change lands on a matching branch: a merge-queue merge, a merged review branch (§8.5), a submitted CL | as `push:` | as `push:`, plus `change` (its URL) and `merged_by` |
| `design-change:` | a design's requirements change on its `default_ref`: the planner of §6.9 | `designs` (files, or folders ending in `/`), `classes` (`new`, `changed`, `removed`; default all), `debounce`, `max_tasks_per_change` | as `merge:`, plus `design`; with `per: requirement`, also `requirement`, `class`, `action`, `hash` |
| `manual:` | someone asks for it now, in Silo's UI, the API or the CLI (`silo trigger run`, `grain trigger run`) | `who` (default: the team's approvers, or the Grain's owner), `inputs` (the template inputs the requester may set) | `requested_by`, and the inputs they set |

`overlap` reads the previous firing's derived status (§6.1). While that
firing is unplaced, queued or has an open run, `skip` files nothing;
`queue` files the new one with `depends_on` the previous, so it waits;
`cancel-previous` files the new one and cancels the previous (§10.6). The
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
`depends_on` (§10.8), which needs no trigger. A trigger on a task closing
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
  because front matter is never rendered, §6.5), and `context:` entries
  added after the template's (§7). A firing files one task, which lands
  where the firing's home says (below). A trigger's template inputs are
  the template's declared inputs, so grain's trigger variables
  (`{{group}}`, `{{from_sha}}`) are inputs bound from the event;
- **a workflow run followed by a review task**: `workflow:` (a
  `.grain/workflows/*.yml` file in the trigger's own repo, by plain path,
  §9.4), `branch:` (where it runs; default `default_ref`) and an optional
  `review: {template, inputs}`. The Grain runs the workflow, as a
  workflow's `on.schedule` runs in grain today (bwsalmon/grain
  `docs/declarations.md`, workflow schedules), and when the run finishes
  files a review task carrying its result, from `review.template` or
  grain's built-in workflow-review template. Only a Grain runs CI, so a
  workflow trigger must say `fires_in: grain`. It is
  [`widgets-code/.grain/triggers/nightly-ci.trigger.md`](../examples/grainfiles/README.md).

A workflow's own `on.schedule` stays in its YAML, where Actions reads it,
and grain still runs it as today: it behaves as an implicit trigger of
the second form, firing on that repo's Grain. A repo that wants to
change how one behaves (its overlap, its review template) moves the
cadence out of the workflow into a trigger.

**A trigger never holds a merge.** It has no `gate`, `human`,
`sensitive` or `requires` key, and its schema refuses them. Holding a
change for review is a gate (§6.7).

**A firing creates a task and a reference to it**, at a path derived from
the trigger and the event, so a retried firing writes nothing new:
`tasks/fired/<trigger>/<key>.task.md`. The key is the event's identity:

| Source | Key |
| --- | --- |
| `schedule:` | the time it was due, `yyyymmddThhmmZ` |
| `push:`, `merge:` | the first 12 hex of `to_sha`, plus `-<group slug>` with `group_by` |
| `design-change:` | `<design name>/<to_sha, 12 hex>`; with `per: requirement`, `<design name>/<HS-6>-<action>-<first 8 hex of the hash>` (§6.9) |
| `manual:` | the request time, `yyyymmddThhmmssZ`, plus `-<requester slug>` |

A firing creates **executions, never definitions**: the template (at the
commit fired) is the definition, and the rendered task is the snapshot.
The execution records `trigger: {ref, on, key, fired_at, event}`: the
trigger, which source fired, the key, when, and the event fields it
bound, with `created_via: trigger`.

- **A `fires_in: grain` trigger in one of a Grain's trigger roots fires
  on that Grain** (§4.2), locally: the Grain creates the execution in
  its own lane, at `backlog/fired/<trigger>/<key>.exec/` (or the lane
  `fires.into` names), and runs it, so it never needs Silo. That covers
  the Grain's own `triggers/` and every other folder its `triggers:`
  entries name, less each entry's `ignore:` and any `archive/`. A
  trigger from another repo fires at
  `backlog/fired/<repo name>/<trigger>/<key>.exec/`, so two repos'
  triggers of one name never collide. The Grain watches the sources
  itself: its clock, and the repos it fetches for `push:`, `merge:` and
  `design-change:`.
- **A Grain takes a team's triggers by naming the folder.** A Grain
  that wants the team's triggers names the team's `shared:triggers/`
  folder (or a subfolder of it) as a trigger root, with `ignore:` for
  any it does not want (§10.11). Nothing is copied into a folder of
  Silo's, because Silo has no folder per Grain.
- **A `fires_in: silo` trigger fires in Silo** for the team it names, if
  it is in one of **the team's trigger roots**: the intake's own
  `grain.config.md`, which may hold only `triggers:` entries. Silo
  renders the template and **creates the execution in the lane its
  `fires.into` names**, a Grain's `hub/` lane by locator, through the
  forge API as `hub:trigger` under that Grain's `GRAINOWNERS` (§10.9).
  A `fires_in: silo` trigger with no `into` does not fire, and Silo
  reports it. Silo watches the repos it hosts on receive, and code
  repos by webhook or on its next pass (§9.6). A Grain whose trigger
  root lists a `fires_in: silo` trigger shows it, and never fires it.

**Approval of a firing.** A firing in a Grain's root is the Grain's own
execution. It is created in its lifecycle's first `ready`-category
state (with `approved_by: deployment:<id>`) only if the
trigger says `auto_approve_proposals` **and** the trigger is in the
root's own repo, or reached through an entry that is pinned
(`ref_mode: pin`), so that what fires is exactly what the owner reviewed.
Through a tracking entry it is created in its `initial` state, and the
Grain's owner approves it, or the Grain's local policy does. Silo
creates executions `approved_by: hub:trigger` under the same rule, for
the team's trigger roots. Whatever
the trigger says, a firing **caused by someone** is approved no further
than they could approve: a `push:` or `merge:` firing whose pusher or
merger is not an approver of the firing's team, and a `manual:` firing
requested by one who is not, are created in their `initial` state. `design-change:`
firings keep §6.9's rule.

**Runtime state is not committed.** The last and next fire times, the
debounce windows, the catch-up cursor, whether a trigger is **paused**
(an operator's switch, in Silo's or the Grain's UI) or **suspended** (by
Silo or the Grain after repeated failures, or by the rate limits of
§12.36), and the run history all live in Silo's or the Grain's
database, like other runtime state (§1.2). `enabled:` is the definition's
own reviewed switch, and stays in git. The execution each firing
creates records `trigger.fired_at`, so the history of what fired is in
git anyway, as executions.

**Customising a team's trigger** is an `ignore:` plus a new grainfile.
Grain `us` names the team's `shared:triggers/` as a trigger root, and
wants the daily `granule-pull.trigger.md` weekly. It does not edit or
override it: its entry says `ignore: [granule-pull.trigger.md]`, and it
adds its own `triggers/granule-pull-weekly.trigger.md`, a whole,
self-contained trigger that references the same template, whose
`derived_from` names the original. `derived_from` is a reference: it
activates nothing and inherits nothing, and Silo's views use it to show
the two diverging when the team changes the original (§10.11).

**Retiring a trigger** is the producer's move: the team moves it to
`triggers/archive/`, and it stops firing everywhere that names
`triggers/`, without any Grain changing its config (§4.3, §10.11).

**Triggers in code repos.** A trigger's *definition* may live in a code
repo's grain folder, beside the code it is about
(`widgets:.grain/triggers/weekly-release-notes.trigger.md`). Its *instances*
never do:

- **In Silo**, a trigger that Silo does not host fires for a team only
  if that team **adopts** it, with an entry for it in its intake's
  `grain.config.md` `triggers:` (written by the team's admins). A code
  repo cannot make a team receive firings by naming the team. The
  executions go into the lane the trigger's `fires.into` names, at
  `<lane>/fired/<repo name>/<trigger>/<key>.exec/`, where `<repo name>`
  is the last element of the code repo's URL without `.git`. So two code
  repos' triggers of the same name never collide. A firing is created
  approved only if the trigger says `auto_approve_proposals` **and** the
  adopting entry is **pinned** to a commit the admins reviewed, at which
  the template it names also resolves (a relative template reference
  reads that same commit). Otherwise it is created in its `initial`
  state (§12.13). A pin fixes the bytes the admins reviewed, and taking
  a change is re-pinning, one line.
- **On a Grain** whose trigger root names a code repo's trigger, a
  firing is an execution in the Grain's own lane, as above. Its
  `definition` pins the template's commit, so what ran is exactly the
  template at that commit rendered for that event, and the snapshot
  shows it.

**Why trigger-fired instances are kept out of code repos.** A daily
trigger fires 365 times a year, and a push trigger as often as the repo
moves. In a code repo, each firing would be:

- a commit in the code's history, showing up in `git log`, blame and
  bisect;
- a change through the repo's review and CI, and, in a repo whose
  `.grain/**` is gated for a human (§6.7), a human's Submit every time;
- a review of nothing, because an instance is the trigger plus an event,
  rendered deterministically.

What deserves review is the definition, which changes rarely, so that is
what lives with the code. A firing is an execution, and executions live
only in Grains' roots, never in a code repo's grain folder (lint GF047,
§9.2).

### 6.7 Gates (`kind: gate`)

A **gate** holds a change on its way into a repo until what it requires
is done. It is policy about merges, and **never fires work**, except the
review tasks it requires. Grain's `.grain/triggers/*.yml` hold both
kinds today, told apart by `gate: true`. This design splits them: firing
work and holding a merge are different things, with different authors,
different failure modes and different security properties, and a gate
must not be weakened by the machinery that customises triggers. Schema:
[`gate.schema.json`](../schemas/grainfiles/gate.schema.json).

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

This is [`widgets-code/.grain/gates/grain-declarations.gate.md`](../examples/grainfiles/README.md)
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
    `from` names the approvers, by principal (§8.2), and defaults to the
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
(§9.2).

**How gates apply in each kind of repo** (§9.2):

- **In a code repo**, through grain's merge queue. For a change into
  repo R at branch B, the queue lists R's own gates (the `*.gate.md`
  files under R's grain folder, `.grain/` unless R's `.grainconfig` says
  otherwise, at B's tip, by the same tree-only listing as a root entry,
  §11.1) and any gate from the landing Grain's mandatory gate sources
  (below) whose `match.repos` covers R. It matches each against the change's paths,
  files the review tasks each requires (into the Grain's own root, as the
  Grain's work), withdraws auto-merge if any is sensitive, and holds the
  change until every requirement is met. The forge's own branch
  protection still applies; gates add to it, never replace it. This is
  how a definition homed in a code repo lands: its file is a `.grain/**`
  change, held by the repo's declarations gate (§9.2, §10.10). A forge that lands a change outside grain's merge queue
  bypasses gates, as it bypasses path triggers today; a repo that relies
  on them also requires grain's merge-queue check in its forge's branch
  protection.
- **In a protected branch** (an intake, a shared repo, a Grain root),
  through the forge check and Silo's merge queue. The hook
  refuses a push to a protected ref that touches a path a gate matches,
  unless it is a merge Silo's merge queue made: a review branch's merge
  (§8.5) that records, in the merge commit's trailers, the review tasks'
  outcomes and each human's Submit, which the forge check checks against the
  gate at the ref's current tip. Silo creates the required reviews as
  executions, approved, in the lane of the Grain that serves the owning
  team (`created_via: api`, the review template as the definition). The gates that apply are the repo's own, in
  its grain folder (`.grain/gates/` by convention in a Silo repo too),
  plus Silo's mandatory sources (below).
- **In an intake, `GRAINOWNERS` comes first.** A new definition or a
  comment is checked against the intake's `GRAINOWNERS` and the forge
  check's fixed rules (§8.2), and is not a gated change unless a gate
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
capability sources (§6.8). The merge queue reads those itself, with the
Grain's credential, and applies each gate whose `match.repos` covers the
repo being merged into. Nothing in any grain repo removes them, and a
mandatory source the queue cannot read holds every change it might cover
(unknown is not empty, §11.1). Silo's own policy may name mandatory gate
sources for every repo it hosts in the same way.

### 6.8 Capabilities: base capabilities, and grainfiles that only narrow them

A **capability** is authority a run uses beyond its files: a cloud
credential, a model API, a throwaway GitHub repo, tools that read the
Grain's own logs. In grain today these are opaque names: `gcp-key`,
`gemini-key`, `github-sandbox`, `self-debug`, `grain-ci`,
`package-publish`, and `repo-read:`/`repo-write:` in workflows. Their
behaviour is in Go providers in bwsalmon/grain (`pkg/capability/...`,
`docs/ci-capabilities.md`), and what one *implies* is in code the local
policy cannot read. Which repos a name reaches is hard-coded in provider and
orchestrator code (for `self-debug`, `model.GrainSourceRepo()` and the
checkout in `pkg/orchestrator/checkout.go`, scoped by the provider's
`DeploymentWideTeams`), and nothing showed that to the policy until grain task 4303
found a repo reaching a sandbox by that route.

This section makes capabilities things the policy can read. There are
three layers, and a grant is a fourth step, which only the Grain takes:

| Layer | What it is | Who writes it | Where it lives |
| --- | --- | --- | --- |
| **base capability** | trusted provider code with a declared surface: the most it can grant, what it materialises, what it implies | the Grain's implementer | in the Grain's code. The Grain publishes a `kind: capability-base` declaration of it in its root |
| **capability grainfile** | a reference to a base, plus an ordered chain of **reducers**, each of which only narrows | anyone, in any grain repo; a Grain honours it only from a source its policy sanctions | `kind: capability`, usually in a team's or org's shared repo |
| **request** | a `context:` entry with `mode: use` | the definition's author, fixed in each execution's snapshot | the definition |
| **grant** | the Grain's decision to materialise the effective capability for one run | the Grain's local policy, after Silo's claim | the run's record |

**The core rule is monotonicity.** The effective authority of a
capability grainfile is

```
effective(C) = maximum(base type, on this Grain) ∩ reducer₁ ∩ reducer₂ ∩ … ∩ reducerₙ
```

with the reducers of every grainfile in its chain applied base-most first.
**A reducer can never add.** Nothing in a capability grainfile, or in a
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
a set of files in git, named by path (§2.2). The Grain, which holds the
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
([`grain-eu/capabilities/bases/gcp-service-account.capability-base.md`](../examples/grainfiles/README.md),
schema [`capability-base.schema.json`](../schemas/grainfiles/capability-base.schema.json)):

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
  `the hard-coded repo grants (`model.GrainSourceRepo()`)` made a declared part of the contract.
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

#### Capability grainfiles

A capability grainfile names a base and narrows it
([`hub-shared/capabilities/widgets-deployer.capability.md`](../examples/grainfiles/README.md),
schema [`capability.schema.json`](../schemas/grainfiles/capability.schema.json)):

```yaml
---
kind: capability
schema: grain/capability@v1
title: Deploy widgets previews to Cloud Run (acme-widgets), 1-hour tokens
base:
  capability: "org-caps:gcp/acme-widgets.capability.md@v1"   # or {type: gcp-service-account@v1}
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
  implements, or `{capability: <ref>}`, another capability grainfile to
  narrow (below).
- **`reducers`** is an ordered list. Each has a `language`, `builtin`
  for the interim types or `anselm`, and may say `on: credential` or
  `on: use`. Front matter quotes `"on"`, for the reason given in §9.3.
  An `id` labels it in the audit log; it is always also named by its
  index.
- **`secrets`** binds the base's secret slots to names in the Grain's
  store (below). **`approval`** opts in to a human's approval (below).
- A capability grainfile has **no field that grants**: no `maximum`, no
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
it: "capability entry 2, `shared:capabilities/widgets-deployer.capability.md`,
reducer 3 (`preview-services-only`) is `language: anselm`, which `eu`
cannot evaluate yet". It **never skips it**, because skipping a reducer
widens the capability. Teams should not depend on Anselm reducers before
adoption. The example's placeholder is there to show that the files stay
valid.

**When Anselm arrives:**

1. **`builtin` stays valid.** Loaders keep every decoder they ever
   accepted (§2.4), so a `builtin` chain keeps working and nothing has to
   be rewritten.
2. **Each built-in type gets a defined Anselm equivalent,** shipped with
   the adoption. The meanings are fixed now (the table above), so each
   equivalent is checked against its built-in on the examples and a
   fixture set, as the Jinja subset is checked against Python (§6.5).
3. **A chain may mix languages.** `grain capabilities convert` may
   rewrite built-ins as Anselm in one reviewed commit, which changes each
   digest (below) and so needs any approval again. Converting is
   optional.
4. **Anselm bodies written before adoption** that do not parse under the
   adopted version fail closed: the request is refused, never widened.

#### Composition across repos

A capability grainfile's base may be **another capability grainfile**, so a
broad definition can be narrowed in layers, each kept by whoever owns
that layer:

- the org's `org-caps:gcp/acme-widgets.capability.md` is `gcp-service-account@v1`
  with `subset projects [acme-widgets]`, released at tag `v1`;
- the hub team's `shared:capabilities/widgets-deployer.capability.md` takes that
  grainfile, pinned at `v1`, as its base, and narrows it to two roles,
  1-hour tokens and a per-use check;
- a task's `use` entry may narrow it once more, with `reducers:` of its
  own: 30-minute tokens, valid for a week.

**Each layer only tightens.** A team cannot get more than the org
defined, and a task cannot get more than the team defined. The chain is
resolved with the reference rules of §3 (path identity, aliases, git
URLs and `@ref`, `moved_to` and tombstones with the hop limit), plus
cycle detection and a depth limit:

```
CAPABILITY_LIMITS = {chain: 8, reducers: 32, task_reducers: 8}

effective_capability(entry, task, grain):           # entry: a context: entry with mode: use
    chain, seen, ref, from = [], [], entry.ref, snapshot @ its definition's rev
    loop:
        C = resolve(ref, from, grain.cred, gate = sanctioned_source)   # §3: redirects, hops, tombstones
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
  reference does (§3). A team should pin the org grainfile it builds on
  (`@v1`). Tracking it is safe, because each link still narrows, but
  every upstream change changes the digest.
- **Monotonic within one resolution, not over time.** The org can edit
  its grainfile and drop a reducer, and that widens every grainfile built on
  it. That is the org's right, as a template's author may edit a
  template. The **digest** is what holds an approved task to what was
  approved: Silo's grant and any approval name the digest, and a Grain
  that computes a different one refuses the claim (below; §12.28).

#### Catalog and grant

**A capability definition is reached only by reference, and referencing
one never grants it.** There is no capability root: a definition
changes what a run *may* be given, never whether anything runs, so by the
test of §4.1 it is a reference. A task names it in `context:` with
`mode: use`, and the Grain resolves the chain then. Silo's **catalog**,
the list it shows a task's author, is a view of the capability grainfiles
in the repos the team's `.grainconfig` aliases and the sources Grains'
policies sanction; it is a convenience for authors, and nothing a Grain
reads.

**A task requests a capability in `context:`**, alongside its repos and
files (§7), with `mode: use`:

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
  grainfile listed as a bare string is only a copy of its definition, with
  no authority: nothing becomes a grant by accident.
- A `use` entry takes no `path` or `depth`. What lands is what the base
  materialises, at the paths its declaration names.
- **One list, one check.** A task's grain-era `capabilities:` field is
  gone, as `reads` and `writes` went: each name becomes a `use` entry
  (§13.2). The entry is part of the spec, so the approver reads it, the
  hook freezes it, and `spec_digest` covers it (§6.1, §12.7).
- **Repo and folder entries are uses of the git base.** A `read` entry
  is `git@v1` narrowed to that repo or folder, its commit and
  `operations: [fetch]`. A `write` entry adds `push` and
  `push_refs: [refs/heads/grain/<grain>/<run>]`, with folders for a
  `write` folder. The proxy's scope in §7 is exactly that effective
  capability, so the `context:` resolver and the capability resolver are
  the same check.

**The Grain's local policy grants.** It allowlists **base types** and
**definition sources**: which repos and folders may define capability
grainfiles it will honour, and for which bases. **This allowlist is the
whole control**: a capability grainfile from anywhere else is refused,
whoever references it, so no Silo or team folder can introduce authority
the owner did not sanction:

```yaml
# Grain eu's local policy: its own configuration, not a grainfile
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
repos: …                                     # §7, unchanged
network: {egress: [oauth2.googleapis.com, sts.googleapis.com, "*.googleapis.com", …]}
tools: …
```

**Every link of the chain must come from a sanctioned source, for that
base.** A sanctioned team grainfile built on an unsanctioned base grainfile is
refused. The base grainfile is where the authority is defined, so a
sanctioned folder cannot launder a definition the owner never accepted.

**Before materialising anything, the policy checks the effective
capability and everything it implies:** the base type, every source in
the chain, every secret name, and each implied repo against `repos:`
(for the narrowed operations), each egress host against `network`, and
each tool against `tools`. This is the general form of what grain
approximates today with `the hard-coded repo grants (`model.GrainSourceRepo()`)`. A refusal fails closed,
before any credential is minted or file written, and names the entry,
the link of the chain, and the reducer or implication that decided, in
`refusal.entry` or `waiting.entry` (§7).

#### Approval

A capability can require a human's approval before it is used:

- **a base** whose declaration says `approval: required`. This is
  grain's *escalated capabilities* today, such as package publishing;
- **a grainfile or a reducer** that opts in, with `approval: {by, scope}`
  or `type: require_approval`. Adding an approval only narrows: it can
  make a capability harder to use, never easier.

`by` names principals (`team:hub#approver`, `user:…`). `scope` is `task`
(once per task, the default) or `run` (before every run).

**How it meets Silo's approval flow:**

1. **The execution's own approval may be enough.** If the execution's
   `approved_by` is among the principals the capability's approval
   names, that one approval covers both, and Silo records it as such.
2. **Otherwise it is a second approval, after the execution's.** A
   principal the capability names appends an entry to the execution's
   **`capability_approvals`**: `{ref, digest, by, at}`. The lane's
   rules let only such a principal append it, with `by` bound to the
   pusher and never equal to the submitter, after the execution is approved
   ([`hub-intake/GRAINOWNERS`](../examples/grainfiles/README.md), and
   the forge check's fixed rule that `by` is the pusher, §8.2).
   It is an approval, like `approved_by`, written on the execution's
   `exec.md`, never on the definition. Until it is there, Silo shows the
   execution as "waiting: capability approval of `<ref>` by `<by>`", and
   the Grain does not dispatch it.
3. **The claim travels in the attestation.** Silo's attestation on the
   execution
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

What a Grain trusts here is §12.8's accepted risk: Silo's claim that an
approver approved. Signed commits are the upgrade for
`capability_approvals` as for `approved_by`, and `local` approval is the
owner's option when they would rather not trust it at all (§12.34).

#### Secrets never go in git

A capability grainfile **names** a secret, and never holds it:

- **Slots and names.** A base declares slots (`secrets: [{slot: token,
  required: true}]`), and a grainfile binds them, as in
  `secrets: {token: ghcr/publish-token}`. A name is `<secret>` or
  `<secret>/<key>`, grain's secret-store naming today (`github-app/app-id`).
  A base may give a slot a default name, so most grainfiles bind nothing.
- **Resolved at materialisation,** from the materialising Grain's own
  store, into the file the base's declaration names, and nowhere else:
  not into git, `run.md`, events, logs or Silo. The run's record lists
  the names only. **Names are per Grain**: one grainfile, run on two
  Grains, resolves to each Grain's own material.
- **Naming a secret is a source of authority** that the base's maximum
  does not bound, because the material can do whatever it can. So the
  policy allowlists the names each definition source may bind
  (`sources[].secrets`), and a grainfile binding a name its source may not
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
YAML (§9.4), and the key keeps its shape. Each item is resolved by the
Grain into a capability grainfile and checked as a `use` entry would be:

- **a reference to a capability grainfile** (`"caps:gcp/ci-deployer.capability.md"`,
  with the alias from the repo's `.grainconfig`), which is new;
- **a legacy name** (`gcp-key`, `self-debug`, `github-sandbox`, …),
  which resolves to that name's legacy capability grainfile (§13.2);
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
one errors with "this deployment does not provide it". With grainfiles, the
policy for untrusted runs refuses any effective capability that includes
`push`, binds a secret, or uses a base marked write-scoped. The same
names are refused for the same reason, and a new capability is covered
by what it can do, not by whether someone remembered to add its name to
a list. A workflow in a code repo names capability grainfiles only from
sources the policy sanctions. Its `repo-read:` or `repo-write:` of
another repo is still the settings-owned grant it is today (§9.3).

#### What a run records

The run's `running` commit records each capability it materialised,
beside its `context:` (§7):

```yaml
capabilities:
  - entry: 2
    ref: "shared:capabilities/widgets-deployer.capability.md"
    chain:                                          # base-most first, each at the commit used
      - {ref: "https://silo.example.com/git/org/capabilities.git//gcp/acme-widgets.capability.md@v1", rev: <commit>}
      - {ref: "https://silo.example.com/git/hub/shared.git//capabilities/widgets-deployer.capability.md", rev: <commit>}
    base: {type: gcp-service-account@v1, declaration: {ref: "capabilities/bases/gcp-service-account.capability-base.md", rev: <commit>}}
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

### 6.9 Design docs (`kind: design`)

A design doc is a grainfile whose requirement headings carry anchors
(`{#HS-3}`), so tasks can say which requirement they implement and a
trigger can file work when one changes.

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

#### Requirements and their anchors

A requirement is a heading ending in `{#<PREFIX>-<n>}`, which
generalises grain's `{#D-7}` and the `HS-n` and `SV-n` series. Its
**section** runs from the heading to the next heading of the same or a
higher level.

**IDs are never reused.** The allocation rule:

- `requirements.next` only ever increases;
- a new requirement takes `next`, and `next` is bumped in the same
  commit;
- a removed requirement's ID goes to `retired`.

the forge check enforces this on receive. It refuses a push that:

- decreases `next`;
- reuses a retired or higher-numbered ID;
- uses one ID twice in a doc;
- changes another doc's prefix.

A heading with no ID is allowed in a draft. Planning assigns it the next
ID in a Silo commit, as the planner's own pull request does today.

#### Requirement revisions

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
  against into its run record's `closed.design_revs`. Commits carry them in the existing trailers,
  unchanged:

  ```
  Design-Item: <design ref>#HS-6      (the in-repo path for a doc in the code repo,
                                       e.g. docs/design/foo.design.md#D-7; the alias or URL form otherwise)
  Design-Rev: <design commit sha>
  ```

  `Design-Removed:` keeps its meaning.

#### Traceability both ways

- **From a task to its requirements:** read `implements:`.
- **From a requirement to its tasks:** Silo keeps an index from every
  `implements` edge in every repo it hosts. It is rebuilt from git on
  start and updated on each received push.

For each requirement, the index shows its definitions, their
executions' states (§6.2), and **changed since implemented**. A
requirement counts as changed when its current hash differs from the
hash in the `closed.design_revs` of the `done` run of an execution of
the definition. Designs outside Silo, in a code repo or on GitHub,
are indexed the same way when a Silo repo references them and Silo can
read them.

#### Planning is a trigger on design changes

Planning is a **trigger**: a `kind: trigger` with `on: design-change`
(§6.6), authored, reviewed, named in trigger roots, ignored and
customised like any other. bwsalmon/grain's design-doc planner becomes
two things: the **`design-change` event source**, which
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
  template: ../templates/design-plan.template.md
  per: event                           # or requirement: the table below
  inputs: {mode: plan}
  bind: {group: event.group, paths: event.paths, from_sha: event.from_sha, to_sha: event.to_sha, branch: event.branch}
team: hub
fires_in: silo
---
```

This is [`hub-shared/triggers/plan-designs.trigger.md`](../examples/grainfiles/README.md).

**The event.** When a push to a watched design's `default_ref` is
accepted (in Silo, on receive; for a code repo, when Silo or the Grain
reads it), the source diffs every requirement between the old and new
tips. It compares sections by ID and uses the hashes of §6.9. A push that
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
| changed, task still open | a proposal superseding the open task (`supersedes: <ref>`). Approving it makes Silo cancel the old task, which its Grain honours (§10.6) |
| changed, done | a delta task covering only the changed text |
| removed, open | a proposal to cancel the open tasks. Nothing is cancelled without approval |
| removed, done | a removal task, whose commit carries `Design-Removed:` |
| unchanged | nothing |

**Idempotency.** Each firing's key is derived from the event (§6.6):
`tasks/fired/<trigger>/<design name>/<to_sha, 12 hex>.task.md` for
`per: event`, and
`tasks/fired/<trigger>/<design name>/<HS-6>-<action>-<first 8 hex of the hash>.task.md`
for `per: requirement`. Re-running on the same push finds every path
already there, and writes nothing new.

**A design opts in with `planning:`.** The design's `planning: {team,
max_tasks_per_change, approval}` says it wants planning, for which team,
within what limits. A `fires_in: silo` design-change trigger fires only
for a design whose `planning.team` is its own team; a `fires_in: grain`
one (an in-repo planner, §9.5) never fires for a design that has
`planning:`, which is Silo's to plan. So one design is planned by one
trigger, and doing both is refused, as before. Where the design and the
trigger both set a limit, the lower one applies.

**Flood control** is the trigger's, and can only be lowered:

- At most `max_tasks_per_change` tasks are filed per event (default 10,
  at most 50). A larger plan files **one** plan proposal listing every
  intended task. Approving it files them.
- Planning is capped per team per day (default 50), across every
  design-change trigger firing for the team (§12.36).
- `debounce` coalesces a run of merges into one event.
- Planned tasks are always `proposed`, unless the design says
  `approval: inherit` **and** the pusher whose change was merged is an
  approver for the planning team. `auto_approve_proposals` on a
  design-change trigger is refused.
- A design change pushed by a non-approver lands only through a review
  branch an approver merged (§8.5).

### 6.10 Memory (`kind: memory`)

A memory document is guidance a Grain gives its runs: a deployment's
conventions, a repo's quirks, what earlier runs learned. It is active in
a **memory** root (§4.2), and only there.

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

This is [`grain-eu/memory/deploy-notes.memory.md`](../examples/grainfiles/grain-eu/memory/deploy-notes.memory.md).

- **`applies_to`** says which runs it is for: `repos`, `teams` and
  `kinds`, all runs by default. Only the documents that apply to a run
  have their bodies read and rendered (§11.1).
- **Precedence** is the order of the memory entries in
  `grain.config.md`, then specificity, then path (§4.2). The run's memory
  is rendered in that order, with each document's source shown.
- **Who writes it.** The Grain's memory agent commits, as `grain:<id>`,
  only to the one `write: true` entry in the Grain's own root. Every
  other memory root is read-only to the Grain; a proposal for a shared
  folder is a review branch there (§8.5).

**Grain's memory today maps onto this:**

| Today | As grainfiles |
| --- | --- |
| memory documents filed under the deployment | `memory/<name>.memory.md` in the Grain's own `write: true` entry |
| memory documents filed under a repository | the same entry, `memory/repos/<owner>/<name>/<doc>.memory.md`, with `applies_to.repos` set; or, when the repo's team wants to share them, its `memory/` in a shared repo that Grains add as a read-only entry |
| memory documents filed under a team | the team's shared `memory/`, a read-only entry of each of the team's Grains |
| `memory.json`, the memory agent's distilled notes about the deployment | one `memory/learned/<topic>.memory.md` per note, written by the memory agent, so each is reviewable and revertable on its own |
| the memory agent's writes | commits by `grain:<id>` to the `write: true` entry only; read-only entries are never written, whatever the agent proposes. A proposal for a shared folder is a review branch there (§8.5) |


### 6.11 Tombstones (`kind: moved`), and the other kinds

A **tombstone** is the grainfile a real move leaves at the old path,
named for it with `.md` replaced by `.moved.md`. It carries `to:`, who
moved it, when and why, and never changes or goes away, so the old path
is never reused (§2.2). Readers follow it like an HTTP redirect, at most
8 hops (§3). Archiving is not a move and leaves none (§4.3).

The remaining kinds are described where they are used: `config` is
`grain.config.md` (§4.2); `suite` keeps grain's `.grain/suites` keys
(§9.3); `asset` is a sidecar describing another file (§2.3);
`capability-base` is a Grain's published base (§6.8).


### 6.12 Lifecycles (`kind: lifecycle`)

A **lifecycle** declares an execution's state machine: its states (opaque names),
the allowed transitions, and an initial state. **Each state has a
`category:`** from a closed set, and **Grain's behaviour keys only off
categories**; exact names are for people, for gates, for triggers and
for `GRAINOWNERS` transition rules (spec §12 is normative).

| Category | Grain does |
| --- | --- |
| `proposed` | nothing: not yet accepted, never dispatched |
| `ready` | dispatches it, once its dependencies are satisfied |
| `active` | a deployment has a run of it; stalled detection watches it |
| `waiting` | holds it: on a human, a failure streak, a submit or an acknowledgement |
| `done` | counts it as satisfying its dependents |
| `cancelled` | never dispatches it again; its dependents stay **blocked** (they are not satisfied, and lint warns, GF055) |

**Decision:** the six categories need no refinement: all ten of grain's
states fit (below), and every Grain behaviour (dispatch, dependency
satisfaction, the merge queue, stalled detection) keys off one of them.

```yaml
---
kind: lifecycle
schema: grain/lifecycle@v1
title: Security-reviewed work
initial: proposed
states:
  proposed: {category: proposed}
  triage: {category: proposed}
  sec-approved: {category: ready}
  in-progress: {category: active}
  blocked: {category: waiting}
  shipped: {category: done}
  dropped: {category: cancelled}
transitions:
  - {from: proposed, to: [triage, dropped]}
  - {from: triage, to: [sec-approved, dropped]}
  - {from: sec-approved, to: [in-progress, dropped]}
  - {from: in-progress, to: [blocked, shipped, sec-approved]}
  - {from: blocked, to: [in-progress, dropped]}
aliases: {approved: sec-approved}      # an old name, accepted after a rename
on_outcome: {accepted: in-progress, done: shipped, failed: sec-approved}
---
```

This is [`hub-intake/intake/lifecycles/security.lifecycle.md`](../examples/grainfiles/hub-intake/intake/lifecycles/security.lifecycle.md).

- **State lives on the execution**, in `exec.md`; a definition has
  none. **Run status is not a state.** Running, awaiting a reply,
  failed, interrupted and the rest live on each run's `run.md` (§6.3). A run's
  outcome may **propose** a transition, configured per lifecycle by
  `on_outcome`; the deployment applies it only if `GRAINOWNERS` grants it
  that transition, and otherwise records it on the run for a person.
- **Selection:** the execution's `lifecycle:`, otherwise its lane's (a
  lane entry in `grain.config.md` may say `lifecycle:`), otherwise the
  Grain root's `grain.config.md` `lifecycle:`, otherwise the built-in
  default. When an execution is created, the definition's lifecycle (its
  own `lifecycle:`, else its folder's, as the intake's security
  lifecycle above) is copied into `exec.md`, pinned, so dispatch never
  reads the definition's repo.
- **Exact names can be matched** by a gate's `requires.state`, by a
  `task-state` trigger source, and by `GRAINOWNERS` rules such as
  `:state(*->sec-approved) @team:security#approver`, which the forge
  check enforces against the base tree's lifecycle.
- **Changing a lifecycle in use:** an execution whose state is no longer
  a state (or alias) of its lifecycle is a lint error (GF051) and is not
  dispatched until it is migrated; `aliases:` covers renames.

**The built-in default lifecycle**, `grain/default`
([`schemas/grainfiles/lifecycles/default.lifecycle.md`](../schemas/grainfiles/lifecycles/default.lifecycle.md)),
reproduces today's grain states (bwsalmon/grain `pkg/model/state.go`,
`StateOf`, where they are derived rather than stored), separating the
execution's state from each run's status:

| grain today | Execution state | Category | Run status |
| --- | --- | --- | --- |
| `proposed` | `proposed` (initial) | proposed | — |
| `queued` | `queued` | ready | — |
| `deferred` | `deferred` | waiting | — |
| `running` | `active` | active | `running` |
| `awaiting_reply` | `active` | active | `awaiting_reply` |
| `failed` (5 consecutive failing runs) | `failed` | waiting | each run `failed` |
| `awaiting_submit` | `awaiting_submit` | waiting | `done` |
| `awaiting_ack` | `awaiting_ack` | waiting | `done` |
| `completed` | `completed` | done | `done` |
| `closed` (without completing) | `closed` | cancelled | `cancelled` |
| (a rejected proposal) | `rejected` | cancelled | — |

grain's `paused`, `capacity` and `interrupted` run outcomes, which it does
not count against a task, are the run status `interrupted` and propose a
return to `queued`.

**Fully opaque states were rejected:** Grain could not tell whether an
execution is done or dispatchable. The prior art makes the same split: Jira's
status categories (To Do, In Progress, Done) and Linear's state types
(backlog, unstarted, started, completed, canceled).

### 6.13 Folder templates (replacing `kind: suite`)

A **folder template** is a directory of grainfiles (member definitions,
and optionally designs and lifecycles they use) with a manifest
`<name>.template.md` (`form: folder`) at its top. The manifest declares
`inputs:` and, in `run:`, the run policy grain's suites carry today.
**Relative references inside the folder work as written**, so
`depends_on: [../stage-1.task.md]` (or a sibling `stage-1.task.md`)
builds a DAG out of files. `kind: suite` and `.grain/suites/*.suite.md`
are removed (lint GF090).

```
.grain/templates/release-qualification/
  release-qualification.template.md      form: folder, inputs, run: {mode, count, require_approval, auto_merge}
  qualification-mergequeue.task.md
  qualification-ci-diagnostics.task.md
  qualification-base-triage.task.md
  qualification-system-smoke.task.md
  qualification-report.task.md           depends_on: the four above
```

**Instantiation creates one execution per member definition**, in
**one commit** in the target Grain's repo, at
`<lane>/<template>-<yyyy-mm-dd>/` (or `-2`, `-3`… on collision, or
`<lane>/fired/<trigger>/<template>-<event key>/` for a trigger's firing,
so a retry is a no-op). Each execution's `definition` names its member
at the template's commit, its snapshot is the member with the inputs
substituted into body and title only (recorded in `inputs`), and its
`exec.md` records `instantiated_from: <manifest ref>@<rev>`. **A
member's relative `depends_on` maps to the sibling execution** created
from that member (`depends_on: [../qualification-mergequeue.exec/]`), so
the DAG is between the executions created together. Instantiating
requires write authority in that lane in the Grain's `GRAINOWNERS`.

- **One mechanism.** A single-task template is a folder template with
  one definition, and instantiating it creates one execution. **Decision:** `<name>.template.md` on its own (`form: task`,
  the default) stays as the shorthand for that case, because nearly every
  template is one task and a directory each would double the files for
  nothing. A trigger's `fires:` can name either form. Partials
  (`form: partial`) survive, include-only.
- **Rules.** A template folder never contains `grain.config.md`; its
  contents are inert (never dispatched, and never listed by a root);
  nested folder templates are **deferred**.
- **Lints:** dangling internal references (GF061), a `depends_on` cycle
  (GF062), a missing input (GF063), template content reached by a root
  (GF064), an execution inside a template folder (GF047), and an
  instance that does not match the template's dry run (GF069: an
  execution missing or extra, a `definition` that is not its member, or
  `depends_on` that does not map the members'); `grainlint` dry-runs
  every folder template, and `grainlint --instantiate TREE MANIFEST
  [name=value …]` prints the executions an instantiation would create.
- **Updates.** Each execution runs from its snapshot. There is no live
  inheritance; editing a member after an instance was created is GF046,
  and `instantiated_from` lets a later tool diff an instance against its
  template and propose an upgrade.

**The `suite` mapping** (bwsalmon/grain `docs/declarations.md`,
`pkg/declschema` `suiteSchema`):

| `.grain/suites/<name>.yml` | Folder template |
| --- | --- |
| the file | `.grain/templates/<name>/<name>.template.md`, `form: folder` |
| `name` | `title` |
| `templates: [a, b]` (stems of `.grain/templates/*.yml`) | member definitions `a.task.md`, `b.task.md` (the template's `body` becomes the Markdown body) |
| `mode: count \| until-clean`, `count`, `max-passes` | `run.mode`, `run.count`, `run.max_passes` |
| `require-approval` | `run.require_approval` (`false`: the executions are created in the first `ready`-category state, approved by whoever instantiated) |
| `auto-merge`, `auto-approve-proposals`, `team` | `run.auto_merge`, `run.auto_approve_proposals`, `run.team` |
| `repo:`, `target:` (refused in a suite) | still refused |
| a schedule's `suite: <name>` | a trigger whose `fires.template` names the folder template |

grain's release-qualification suite converted is in
[`hub-intake/intake/templates/release-qualification/`](../examples/grainfiles/hub-intake/intake/templates/release-qualification/release-qualification.template.md),
and an instance of it, five executions in eu's hub lane, in
[`grain-eu/hub/release-qualification-2026-10-17/`](../examples/grainfiles/grain-eu/hub/release-qualification-2026-10-17/qualification-report.exec/exec.md).
(grain runs the four qualification templates independently; the report
that depends on them is added to show a DAG.)

## 7. What a run sees: `context:`

A task declares **everything its sandbox holds** in one list, `context:`.
Each entry is a reference (§3) to a repo, a folder of a repo, or a single
file, with a mode. When a Grain runs the task, it resolves the list,
holds every entry to its local policy, and materialises exactly that in
the sandbox. **Nothing else reaches the sandbox.**

```yaml
context:
  - ref: "https://github.com/bwsalmon/silo.git@main"                 # a repo
    mode: write                                                       # push the task's branch, based on main
  - ref: "https://github.com/bwsalmon/grain.git//pkg/daemon/@candidate" # a folder: sparse checkout
    mode: read
  - ref: "shared:designs/hub-intake.design.md"                               # an object
    depth: 1                                                          # and the files its body links to
  - ref: 2026-10-16-reject-dangling-refs/attachments/dangling-ref-error.png   # an asset beside the task (§2.3)
    path: context/screenshot.png                                      # optional: where it lands
```

This is the `context:` of
[`hub-intake/tasks/2026-10-16-reject-dangling-refs.task.md`](../examples/grainfiles/README.md),
walked through in §10.12. A bare string is short for `{ref: <string>,
mode: read}`.

**It replaces four things.** Grain's `model.Task` has a target repo
(`Target`, with `Base`), extra repos to read (`Reads`) and extra repos to
land work in (`Writes`). Ad-hoc context (attachments, pasted file
contents, "see this doc") travels in the prompt. Those are four
mechanisms for one idea, *what this run may see and change*. Each is
an entry here. There is no separate repo or base field on a task, a
template or a trigger: the task's **primary repo** is its first `write`
entry, and its base is that entry's `@ref` (below). §13 maps grain's
fields.

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
- `inputs:` is a template's parameters (§6.5).
- `reads:` and `writes:` are the two lists this replaces. Splitting by
  mode puts one target in two places when a task needs a read of one
  folder and a write of another in the same repo, and leaves the target
  repo a third field.

#### Entries

| Field | Meaning |
| --- | --- |
| `ref` | what to materialise: a repo (a git URL or alias with no path), a folder (a path ending in `/`), or a file (a grainfile or an asset, §2.3), with an optional `@ref` (§3) |
| `mode` | `read` (the default), `write` or `use`. **`write` is valid only for a repo or a repo folder**, and means the run may push **its own task branch** to that repo, based on the entry's `@ref`, and nothing else (below). Every other target is read-only. **`use` is valid only for a `kind: capability` grainfile**, and requests that capability: nothing of the file lands, only what the capability materialises (§6.8) |
| `path` | optional: where it lands, relative to the sandbox's working root. Otherwise the default layout below |
| `depth` | optional, file entries only: also materialise the files this one's Markdown body links to, `n` links deep (below). The default is `0` |
| `input` | in a template only, in place of `ref`: take the reference from a template input of type `ref` (§6.5) |
| `reducers` | `use` entries only: reducers of the task's own, applied after the capability's chain, which can only narrow it further (§6.8) |

**Which ref an entry reads:**

- **A relative or `/`-rooted entry** (`<name>/attachments/x.png`,
  `/designs/a.design.md`) reads **the definition's commit**, the `@rev`
  in the execution's `definition` (a snapshot's relative references are
  read relative to its definition, spec §7.2), as a relative root entry
  reads its `grain.config.md`'s (§4.2). So a definition's assets are
  exactly the ones that were copied and approved.
- **An entry in another repo** reads its `@ref`. With no `@`, it reads the
  target repo's `default_ref`, or the remote's `HEAD` for a repo with no
  `.grainconfig`, at the moment the run is materialised. Either way the
  run record notes the commit it used (below), as every unpinned
  reference is recorded pinned (§3).
- **A `write` entry must name a branch** (`@main`, `@candidate`). It is
  the base: the run's branch starts there, and its change targets it.
  A tag, a commit or no `@` is refused for `write` on push (§8.3).

**The task's branch.** Each run pushes to one branch,
`grain/<grain>/<run name>` (`grain/eu/2026-10-16-reject-dangling-refs`),
in every repo it has a `write` entry for. It may create it, push to it
and rewrite it. It may not push to any other ref: not the base, another
branch, a tag, or a deletion. A `write` folder narrows that further: the
push is refused if any of its commits changes a path outside the
folder's `write` entries (§12.22). **The primary repo** is the first
`write` entry. It is what `repo:` meant: the code repo the change
`closed.change` records is in, and the repo the hub groups the
definition and its executions under. A definition with no `write` entry
is team-targeted or read-only, such as a review or an investigation. A run with
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
task.md                                 the execution's snapshot: read-only
run.md                                  this attempt's run record, kept current by the Grain: read-only
thread/<utc>-<author>.md                replies relayed to the running agent (§6.4), and their
thread/<utc>-<author>/<file name>         attachments: read-only
work/<repo name>/                       a repo entry, or the folder entries of one repo, at the resolved commit
context/<host>/<repo path>/<path>       a file entry, an object or an asset: read-only
```

- **`<repo name>`** is the last element of the repo's URL path without
  `.git` (`silo`, `grain`), as for a trigger's firings (§6.6). If two
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
  leading `/` and the `.git`: `context/silo.example.com/git/hub/shared/designs/hub-intake.design.md`.
- **A file inside a checkout** at the same commit is not copied again. Its
  place in the record is its path under `work/`.
- **An external artefact** (§2.3) lands at its sidecar's path with `.md`
  dropped, beside the sidecar, which is materialised too.
- **`path:`** is relative to the working root, must not contain `..`, and
  must not be or contain `task.md`, `run.md` or `thread/`, or overlap
  another entry's path. the forge check refuses a task that breaks these
  rules (§8.3), and the Grain checks them again.

The agent sees **only** these files. The Grain writes `run.md`, never
the agent. Its `context:` section is the record of what was materialised
and what was skipped, so it is also the agent's manifest:

```yaml
# in runs/<name>/<utc>.run.md, written with the run's `running` commit
context:
  entries:
    - {ref: "https://github.com/bwsalmon/silo.git@main", mode: write, rev: <commit>, path: work/silo/, branch: grain/eu/2026-10-16-reject-dangling-refs}
    - {ref: "https://github.com/bwsalmon/grain.git//pkg/daemon/@candidate", mode: read, rev: <commit>, path: work/grain/}
    - {ref: "shared:designs/hub-intake.design.md", mode: read, rev: <commit>, path: "context/silo.example.com/git/hub/shared/designs/hub-intake.design.md", bytes: 3230}
    - {ref: "shared:designs/img/claims-flow.png", via: "shared:designs/hub-intake.design.md", mode: read, rev: <commit>, path: "context/silo.example.com/git/hub/shared/designs/img/claims-flow.png", bytes: 250}
    - {ref: "intake:tasks/2026-10-16-reject-dangling-refs/attachments/dangling-ref-error.png", mode: read, rev: <commit>, path: context/screenshot.png, bytes: 150}
  skipped:
    - {ref: "grain-code:pkg/daemon/", via: "shared:designs/hub-intake.design.md", reason: not a file}
  limits: {reached_files: 1, reached_bytes: 250}
```

#### Only `context:` materialises

Tasks hold many other references, and **none of them pulls anything into
a sandbox**: `depends_on`, `template`, `implements`, `supersedes`,
`trigger`, an execution's `definition` and `previous`, `moved_from`,
`filed_by`, a comment's `in_reply_to`. They are metadata. The Grain reads some of
them itself, outside the sandbox (a dependency's runs, §10.8; the
template, §6.5), and shows them in the sandbox's `task.md`, but the agent gets the
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
- **What is followed:** files only, grainfiles or assets. A link to a
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
| `hops` | 8 redirects per reference (§3) | dangling |

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
  store's credential against `capabilities` (§6.8);
- a `use` entry's whole capability chain, its effective authority and
  everything it implies (§6.8);
- LFS content against `repos:` and `network` for the LFS endpoint's host.

The Grain resolves outside the sandbox, with its own credential, into a
staging area. A file it must read to find links is fetched only after it
has passed. **Nothing enters the sandbox until the whole set has
passed**, and then the git proxy's scope is set to exactly that set.
This generalises grain task 4303's fix, which made the policy check the
repositories a capability implies: no repo, file or URL reaches a sandbox
unseen by policy. A capability may imply a repo, but **only by declaring
it** in its base's `implies`, and the policy
checks each declared repo as it checks an entry, before anything is
materialised (§6.8). There is no hidden route. A task's
`repo-write:<repo>` capability becomes a `write` entry (§13), and a
workflow's becomes reducers over the git base (§6.8).

**Refusal fails closed, naming the entry.** A listed entry that the
policy refuses, that does not resolve, or that is over a limit, stops
the run before anything is fetched into the sandbox:

- **A placed execution** gets a run in `refused`, with
  `refusal: {reason, at, entry}`, where `entry` names the entry by its
  index and canonical reference and gives the rule that refused it.
  Silo shows that as the task's waiting reason: "waiting: refused by
  `eu`: context entry 2, `https://github.com/gke-labs/granule.git` (read),
  is not in this Grain's `repos`". Silo may place the task on a Grain
  whose policy admits it (§10.7).
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

- **The value is a reference, not text.** It is checked against §3's
  grammar and `ref_kind`, and never rendered by Jinja. Front matter is
  still never rendered (§6.5), so a template cannot compute an entry. An
  input of `type: list` with `of: ref` expands to one entry per item.
- **A relative value** resolves against the grainfile that supplied it: the
  trigger, or the task for a task filed in Silo's UI. The API requires a
  URL or an alias of the intake's `.grainconfig`.
- **A trigger** supplies the values in its `fires.inputs:` (and
  `fires.bind:`, from the event), and may add entries of its own in
  `fires.context:`, after the template's. `prune-packages`'s trigger
  sets `repo:` to grain at `candidate` (§10.12).
- **A filed task records the result.** Filing, by hand or by a trigger
  (planning included), writes the concrete `context:` into the task, every
  `input:` replaced by its value, and `template.inputs` records the
  values used. The task's `context:` is what the approver reads, what
  `spec_digest` covers, and what the Grain's policy checks. The template
  is never consulted again.
- **In a code repo's grain folder,** a template's or trigger's entries,
  and its `ref` inputs' defaults and values, may name only that repo.
  Readers refuse anything else, as they refuse `reads` and `writes`
  there today (§9.3). An adopting team that wants another repo
  supplies it from its own trigger.

#### One resolver, three consumers

Root entries (§4.2), references (§11.1) and `context:` use the **same
reference resolution**: canonicalisation, aliases from the repo's
`.grainconfig`, refs and pins, redirects (tombstones beside the old path,
repo `moved_to`) with the hop limit, the archive mapping, cycle
detection, and dangling handling (§3). They differ in what they build:

| | Root entries | References | `context:` |
| --- | --- | --- | --- |
| builds | a **Grain's active set**: what it runs, fires and gives its runs | one grainfile, read when an operation needs it | a **sandbox's view**: what one run sees and may write |
| accepts | a folder (or one file), listed by its type's kind suffix | one grainfile of the kinds the field expects | repos, folders, grainfiles and assets, with a mode |
| transitive | never; each entry is one folder's subtree less `ignore:` and `archive/` | never: each reference is resolved by whatever needs it | off, unless an entry sets `depth` (at most 3), over body links only |
| on a bad target | the entry is unknown: nothing new starts from it | dangling: reported, never widened | a listed entry refuses the run; a reached file is skipped |
| gate | the reader's credential (§12.18) | the reader's credential | the reader's credential **and** the Grain's policy, before each fetch |
| written by | the Grain's owner, in `grain.config.md` | the referring grainfile's writers | the definition's author, fixed in each snapshot |

So one package, `grainfile`, has one `resolve` with a gate supplied by its
caller:

```
resolve(ref, from, cred, gate):                 # §3; shared by every consumer
    id   = canonicalise(ref, from)              # alias from from's .grainconfig, relative, /-rooted; keeps @ref
    seen = []
    loop at most LIMITS.hops times:
        if id in seen: return dangling("redirect loop")
        seen += id
        if not gate(id): return refused(id, gate.rule)   # before any read of id's repo
        if id.repo's .grainconfig has moved_to: id = id at moved_to; continue
        t = stat(id, at id.ref or id.repo's default_ref, cred)      # trees only (§11.2)
        if t is missing:
            if stat(stone(id)) exists:  id = canonicalise(its to, from = id); continue   # .md -> .moved.md
            if stat(archive_mapping(id)) exists: t = that                       # §4.3, not a hop
            else: return dangling(t.reason)
        if t is unreadable: return dangling(t.reason)
        return Target(id, commit = t.commit,
                      what = repo | folder | object(kind from the suffix) | asset)
    return dangling("more than 8 redirects")

# Root entries (§11.1): gate = always true; the credential is the gate.
# list_entry(...) calls resolve(e.path, grain.config.md, reader's credential, ALLOW)
# for the folder, then lists its trees.
# References (§11.1): the same, then a kind check against what the field expects.

materialise(task, grain):
    L     = min(CONTEXT_LIMITS, grain.policy.context_limits)
    gate  = lambda mode: lambda id: grain.policy.admits(id, mode)  # repos, network, capabilities
    if |task.context| > L.entries: return refuse(task, entry = None, "too many entries")
    plan, skipped, queue = [], [], []
    for n, e in enumerate(task.context):                       # listed entries: never cut
        T = resolve(e.ref, snapshot @ definition rev, grain.cred, gate(e.mode))
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
    caps  = [effective_capability(e, task, grain) for e in task.context if e.mode == use]   # §6.8
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
with the pusher's rights as the gate (§8.3).

## 8. Write authority

Three files decide who may write what: the repo's settings file,
`.grainconfig` (§8.1); its ownership rules, `GRAINOWNERS` (§8.2); and
fixed per-kind rules in the schemas. **The forge enforces them**: its
protected branch and a required check, the **forge check** (§8.3).
Readers act only on the protected branch, and re-check where the forge
cannot be relied on (§8.4). Spec §16 is normative.


### 8.1 Repo-wide settings: `.grainconfig`

Settings that belong to a whole repo, not to one Grain, are in one file
at the repo root, **`.grainconfig`**: YAML, not Markdown, and not an
grainfile (nothing activates it; every reader of the repo reads it). Schema:
[`schemas/grainconfig.schema.json`](../schemas/grainfiles/grainconfig.schema.json).

```yaml
# .grainconfig: hub team intake
schema: grain/repo-config@v1
owner: {team: hub, silo: https://silo.example.com}
default_ref: main                   # what an unpinned reference into this repo resolves to; the protected branch
enforcement: {mode: required-check, check: grainfiles/authorize, branch: main}   # what readers may rely on (§8.3)
aliases:                            # shorthand used by references in this repo: alias → git URL
  shared: https://silo.example.com/git/hub/shared.git
  eu: https://github.com/acme/grain-eu.git
orphans: warn                       # warn (default) | refuse (§11.3)
limits: {objects_per_entry: 5000}   # optional: lower the defaults for entries into this repo
moved_to: null                      # set when the repo has moved: a repo-level redirect (§2.2)
```

- **Paths** in `.grainconfig` are repo-relative.
- **Aliases** are this repo's own: a reference in any file of the repo,
  `grain.config.md` included, expands them from here. A repo without the
  file has no aliases and the defaults (`default_ref: main`,
  `enforcement: {mode: reader}`, `orphans: warn`). Grain folders are
  found by their `grain.config.md`, so there is no `grain_folder` key,
  and hosting is the forge's, so there is no `host` key.
- **Changing `.grainconfig`** is the repo owner's alone (`GRAINOWNERS`
  must say so for `/.grainconfig`; the forge check refuses a rule that gives it
  to anyone else), and `owner` cannot be changed by a push at all.
- `README.md` files carry **no front matter** that anything reads. They
  are for humans, and they are assets.

### 8.2 Write ownership: `GRAINOWNERS`

Who may write what is in **`GRAINOWNERS`** files, in **CODEOWNERS
syntax**: one rule per line, a gitignore-style pattern and then its
owners. They **cascade**: every folder may have one, and for a changed
file **the nearest `GRAINOWNERS` with a matching line wins**; within one
file, as in CODEOWNERS, the last matching line wins. Ownership inherited
from a parent folder is the normal case.

**Ownership never affects what is active.** It says who may write a file,
never whether a Grain acts on it; only root entries do that (§4.2). That
is why ownership may cascade through folders while activation never
does.

```
# GRAINOWNERS at the root of Grain eu's repo
*                                     @user:bsalmon@google.com
/grain.config.md                      @user:bsalmon@google.com
/hub/**/*.exec/                       @team:hub#member(create,move) @user:bsalmon@google.com
/hub/**/*.exec/exec.md:rank,priority,expedite,ranked_by,ranked_at  @team:hub#member
/hub/**/*.exec/exec.md:approved_by,approved_at,state(*->sec-approved)  @team:security#approver
/**/*.exec/exec.md:state(category:ready->category:active),state(category:active->*),superseded  @deployment:eu
/**/*.exec/runs/                      @deployment:eu
/**/*.exec/thread/                    @authenticated(create)
```

This is [`grain-eu/GRAINOWNERS`](../examples/grainfiles/grain-eu/GRAINOWNERS).
Because an execution is one folder holding everything about it,
**authority is granted per subfolder**: the executing Grain's identity
on `runs/**` and on the state transitions in `exec.md`; commenters on
both kinds of `thread/**`; approvers on the approval fields; the lane's
members on the rank fields; and, in an intake, the definition's owners
on `task.md`.

**Owners** are principals:

- `@hub` (formerly `@silo`), only for what Silo writes for itself, such
  as its registry's derived files; every write Silo makes for a person is
  that person's (§8.3);
- `@deployment:<id>` (formerly `@grain:<node-id>`), a deployment's own
  forge identity;
- `@user:<email>`, or a bare email, as in CODEOWNERS;
- `@team:<name>#member`, `#approver` or `#admin` (from Silo's directory,
  hub.md §5.3);
- `@authenticated`, any signed-in human.

An owner may be narrowed to some **verbs** with a suffix:
`@authenticated(create)` may create matching files but not change them.
The verbs are `create`, `update`, `delete` and `move` (a move leaves a
tombstone, §2.2, except an execution's lane move; `delete` is only ever
allowed on assets, since no grainfile is deleted).

**The field-level extension.** A pattern may end with `:` and a list of
front-matter fields. That line governs only those fields, and overrides
the whole-file owners for them; `body` names the Markdown body. Only a
file with several writers needs it, and in this design that is an
execution's `exec.md` (above). An intake needs none, because a
definition holds no state:

```
# GRAINOWNERS at the root of hub/intake
*                          @team:hub#admin
/tasks/                    @authenticated(create) @team:hub#member
/tasks/**/thread/          @authenticated(create)
/templates/                @team:hub#member
/lifecycles/               @team:hub#admin
```

**Transition rules.** A field rule may name a lifecycle transition,
`state(<from>-><to>)`, where each side is a state name, `*` or
`category:<category>`; it governs only that change of `state` (spec
§16.2.4). So in eu's root above only the security team may move an
execution into `sec-approved`, and the deployment may make exactly the
outcome transitions it needs, out of `ready` and on from `active`.

Some rules are **fixed per kind in the schema**, checked by the forge
check whatever `GRAINOWNERS` says, so it cannot be configured away:

- a new definition's `submitter` is the forge identity of the change's
  author;
- a new execution is in its lifecycle's initial state (or approved by
  its creator, if they may approve), with `created_by` that identity;
- `approved_by` is that identity, and never the definition's
  `submitter`; the only transitions are those its lifecycle allows
  (§6.12);
- `ranked_by` is that identity, or the principal Silo acted for;
- an execution's snapshot and the fields fixed at its creation never
  change, and a run's terminal states are final;
- a fold of `<name>.task.md` into `<name>.task/` is allowed to whoever
  may create the comment it carries;
- a tombstone never changes and is never deleted (§2.2).

**Who may change `GRAINOWNERS`.** Only the owners its own nearest rule for
`GRAINOWNERS` names, and a file may not give a subfolder's `GRAINOWNERS`
to anyone its own rule for that path does not already own. A Grain's
root keeps `grain.config.md` its owner's: `GRAINOWNERS` cannot give it to
`@hub`, and the forge check refuses a rule that tries (§12.14).

**Enforcement.** The forge check enforces `GRAINOWNERS` on every change
to the protected branch (§8.3). Where the repo's `enforcement` does not
claim a required check, readers re-run the check themselves and refuse
what they cannot authorise (§8.4).

**One kind of writer per subfolder.** The split, written as `GRAINOWNERS`:

| Repo or folder | Its owners | Field-level lines? |
| --- | --- | --- |
| a Grain's root | its owner (`grain.config.md` alone, its own definitions, triggers, memory); in each execution, the Grain (`runs/`, outcome transitions, `superseded`), approvers (approval fields), the lane's members (ranks), commenters (`thread/`), and whoever may place work (creating the folder) | **yes**, on `exec.md` only |
| a team's intake | submitters (new definitions), the team's members (definitions), commenters (`thread/`), the team's admins | none |
| a shared repo | its maintainers, directly or by merging review branches (§8.5) | none |
| a code repo's `.grain/` | whoever the repo's own merge process lets land a change, and the forge check for paths inside the grain folder (§9.6) | as needed |



### 8.3 Forges enforce: the protected branch and the forge check

Repos live on **ordinary external forges**. **The repos are
authoritative**; every index Silo or a Grain keeps is a cache. Each forge
enforces who may change a grain folder (spec §16):

- **The default branch is protected**, with direct pushes off.
- **A required status check, the forge check,** runs the validator
  (`grainlint`) and then a `GRAINOWNERS` authorisation over each pull
  request (or push, where the forge has push rules). It maps each changed
  path, each changed field and each lifecycle transition to the **forge
  identity** of the pull request's author or the pusher, and fails what
  that identity is not granted. Identity comes from the forge, never from
  git author fields. It reads `GRAINOWNERS` and lifecycles from the
  **base** tree, so a change cannot authorise itself.
  `grainlint --authorize BASE HEAD --actor …` is the reference
  implementation.
- **`GRAINOWNERS` may compile to CODEOWNERS** or rulesets where the forge
  supports them. CODEOWNERS means "must approve", not "may write", and
  cannot express field rules, transitions, verbs or the fixed rules, so
  the compiler drops those and readers must verify (spec §16.8).
- **The repo settings file says which enforcement is relied on**:
  `.grainconfig` `enforcement: {mode: required-check | codeowners |
  reader, check, branch}`.
- **A grain folder inside a code repo** is judged only for paths inside
  it; the rest of the repo keeps its own review rules, and a change that
  touches both must satisfy both (§9.6).

**Silo as writer.** Silo writes through the forge API, as pull requests
or commits, **as the user** it acts for, with that user's delegated
token: a person's proposal, comment, placement (creating an execution),
approval or rank is that person's change, in the one repo it concerns.
Silo never writes into a definition's repo to place work. Silo is never a
privileged bot.

**Deployment as writer.** A deployment writes its executions' `runs/`
(records and the comments in them), the outcome-driven transitions on
their `exec.md` (§6.12) and `superseded` on re-placement, under its own
forge identity, in its own root only; `GRAINOWNERS` grants it exactly
those.

### 8.4 Readers verify what the forge cannot

- **Readers act only on commits reachable from the protected branch.**
  Anything elsewhere is a proposal.
- **Reader verification.** A reader MAY re-run the forge check itself,
  commit by commit, and refuse what it cannot authorise. It is
  **required** wherever `enforcement.mode` is not `required-check`: a
  forge without required checks, a CODEOWNERS-only repo, a bare
  `git daemon`, a standalone Grain's local repo.
- **Placed work.** A Grain runs only executions in **its own** lanes,
  folders of its own root its `grain.config.md` names. It never runs a
  definition merely because a foreign repo contains one, and it runs an
  execution from its snapshot, never from the definition.
- **Foreign content is claims.** Templates, triggers and designs from
  another folder are data that a task the Grain already accepted may
  use. They grant nothing (§12.4).
- **A standalone Grain's own repo** is trusted as its owner's: the owner
  has the filesystem anyway.

**Code repos** are forges too, with their own review: §9.6.

### 8.5 No cross-repo transactions

With executions, **most flows touch one repo**: placing, reordering,
changing lanes, approving, commenting on an execution, running and
closing all happen in the executing Grain's root, and proposing and
discussing a definition in its own repo. The few flows that touch two
write in a fixed order, and record the step they have reached. A reader
that catches the gap sees one of these:

- a new execution on the new Grain while the old Grain's execution is
  not yet `superseded`: the new one is **fenced** (it is not dispatched
  until its `previous` is superseded or finished), so nothing runs twice;
- a moved definition at its new address whose old address is not yet a
  tombstone: readers of the old address still find the old definition,
  which is unchanged, and every execution runs from its own snapshot
  anyway.

Each step is keyed by (definition identity, operation, Grain), so a
retry is a no-op. Silo's dispatcher records the step it has reached in
its database row, as it records an offer today. After a crash it resumes
from the first step whose effect is missing in git.

| Flow | Order |
| --- | --- |
| place | Grain root (the execution folder: `exec.md` and snapshot, one commit) → notify |
| reorder, change lanes | Grain root only (a `rank` edit, or a `git mv` of the folder) |
| approve, cancel | Grain root only (`exec.md`) → notify; the Grain marks an open run `cancelled` |
| close | Grain root only (run closed, outcome transition) |
| re-place | new Grain's root (new execution with `previous:`) → notify the old Grain → old Grain's root (`superseded`, its open run `superseded`) |
| link | the owner's commit adding the `hub/` lane and its `GRAINOWNERS` rule → the registry's entry |
| unlink | the owner's commit removing the lane → Silo re-places what had no run |
| move a definition across repos | new home (with `moved_from`, its thread moving with it) → tombstone at the old path |
| upward filing | intake (the definition, committed by Silo) → the Grain's comment on its run naming it |
| trigger firing in Silo | the target Grain's root (the execution, deterministic path); last fire recorded in Silo's DB |
| trigger firing on a Grain | Grain root only: the execution at a deterministic path in its `backlog/fired/` lane, one commit; last fire recorded in the Grain's DB |
| planning (a `design-change` firing) | intake definitions (deterministic paths) → the design's last planned revision recorded in Silo's DB |
| gated change in a Silo repo | the gate's review executions (in a Grain's lane) → their runs close → Silo's merge of the review branch, with the outcomes in its trailers |
| definitions via review branch | `review/*` branch → the approver's merge commit on `main` |

**Definitions** (templates, triggers, gates and designs) are pushed straight
to `main` by maintainers, or through **review branches**. Anyone with
`create` rights may push to `review/*`. The forge check accepts a commit on
`main` that touches a definition only if one of these holds:

- the pusher is a maintainer;
- the commit is a merge whose second parent is a `review/*` tip, and the
  merge's pusher is an approver other than the branch's author.

On receive, Silo checks schema and references, applies the repo's
gates (§6.7), and fires the `design-change` and `push:`/`merge:`
triggers that watch what moved (§6.6, §6.9).

### 8.6 Scale and availability

- **Scale.** Assume thousands of deployments, each running about 10⁴
  tasks a year. A run record with its comments is roughly 5 KiB, about
  50 MiB a year per deployment before compression. Silo batches its
  writes (one commit per dispatcher pass per repo), Grains push at most
  every 5 seconds, and both fetch with partial clone
  (`--filter=blob:none`). Old roots and intakes can be **rolled over**
  into a new repo, the old one staying readable under an alias.
- **Availability.** While a forge is down, a Grain keeps running what it
  has, commits locally and pushes on recovery; nothing new is placed
  there. Silo's views keep serving their caches, marked stale (§8.7).

### 8.7 Silo's registry

**Silo's central registry is itself a grain folder** on a forge. Its
`grain.config.md`, in its registry form, lists the registered grain
folders, each as a git URL plus a folder path (`folders: [{name, at:
"https://github.com/acme/hub.git//intake/@main", team, role}]`), and
optionally saved cross-folder views; it has no lanes, because every lane
is a Grain's. **Registering a folder is a
reviewable commit** to the registry; there is no per-repo `kind: repo`
file (removed by task 4487). Silo's cross-folder views (definitions,
executions across Grains, a definition's executions) are derived
indexes over the registered folders, caches over them, refreshed by forge webhooks and
otherwise by polling every 5 minutes; each view shows the commit it read
each folder at, and Silo re-reads a parent before deciding anything from
it (spec §17).

**Roles.** A deployment is the **executor**; Silo is the human-facing
**reader and writer**, and the **policy decision point** for its UI,
which uses the same `GRAINOWNERS` evaluation as the forge check so it
never offers what the check would fail. Both are conformance classes of
the same spec (spec §2.2).

## 9. Definitions and `.grain/` in code repos

Grainfiles are not confined to repos Silo hosts. A code repo can keep
its own in a grain folder, `.grain/` by default, and design docs in
place, and a team can keep definitions there, with their threads.
Executions never live there: they live only in Grains' roots.

### 9.1 Grain folders inside any repo

**Any repo may hold grainfiles**: a code repo, a docs repo, a
monorepo. A grainfile is any file with a kind suffix (§2.1), wherever it
is, and nothing is located by scanning. What matters is how something
reaches it:

- a Grain's **root entries** name folders in it, for triggers or memory
  (never for lanes, which are always the Grain's own);
- **references** name its templates, definitions, designs and capability
  definitions;
- its **merge queue** reads its gates from its grain folder, `.grain/` by
  default (`grain_folder` in `.grainconfig`), when a change merges into
  it (§6.7).

A code repo needs no `.grainconfig` at all unless it has aliases or a
different grain folder. Its
top-level `README.md` belongs to the code, and is left alone.

```
README.md                                         the code's own README: an asset
.grainconfig                                      optional: aliases (§8.1)
GRAINOWNERS                                       optional: documents intent; validated on read
.grain/templates/release-notes.template.md        kind: template
.grain/triggers/weekly-release-notes.trigger.md   kind: trigger: active only for a Grain whose trigger root names .grain/triggers/
.grain/gates/grain-declarations.gate.md           kind: gate: read by this repo's merge queue
.grain/workflows/ci.yml                           a GitHub Actions workflow: YAML, an asset (§9.4)
.grain/tasks/<name>.task/task.md                  kind: task (a definition homed here, with its thread/, §9.2)
docs/design/widgets.design.md                     kind: design (in place, §9.5)
```

**Identity does not change.** A grainfile in a grain folder is named by its
repo URL and its **repo-relative** path, as anywhere else (§2.2):
`https://git.example.com/hub/widgets.git//.grain/templates/release-notes.template.md`.

**A code repo's triggers fire only for Grains that point a trigger root
at them.** Committing `.grain/triggers/nightly-ci.trigger.md` is a
proposal to every Grain; a Grain enables it with an entry (§6.6).

### 9.2 Where definitions and executions live

**Definitions live anywhere; executions only in deployment folders.** A
definition, with its thread, may be in a team's intake, in a code repo's
`.grain/`, in a shared library or in a Grain's own `tasks/`. An
execution is always a folder in a lane of the Grain that runs it (lint
GF047 anywhere else, a code repo included). That is the whole rule.

**`task_home` is removed.** The first draft had a `task_home` setting
with three modes: `intake` (approved tasks stay where proposed),
`code-repo` (committed into the code repo on approval) and `promote`
(moved there by hand later), with per-repo overrides, an `allow` list,
a pending-change record while the move was in review, digest checks
across the move, reference rewriting, and cancels written back into the
code repo. All of it existed to decide where an *approved task, with its
state and its pointers*, should live. Now:

- a definition holds no state, so approval never touches it: approval is
  on the execution, in the Grain's repo;
- its discussion is inside it, so it moves with it and nothing is left
  behind to re-point;
- executions copy the definition, so an execution never depends on
  where the definition is now.

So nothing has to move when work is approved, and there is nothing for
a mode to choose. A team that wants a definition next to its code
**writes it there**, through the repo's own review, or **moves it there
once**, as any grainfile moves: the new home with `moved_from`, its
thread moving with it, and a tombstone in the intake (§2.2). Existing
executions keep naming the old address, which resolves through the
tombstone, and they run from their snapshots regardless. The schema
refuses `task_home` (spec §18.2).

**What still holds:**

- **Proposals start wherever the proposer may write.** Through Silo,
  that is the team's intake; proposing never needs a code-repo account
  or code-repo write access.
- **Trigger-fired work is never committed to code repos.** A firing is
  an execution (§6.6), and executions are never in code repos.
- **A definition in a code repo is readable by everyone who can read
  that repo**, which may be more people than can read the intake. A team
  writes one there knowing that.
- **Placing a code-repo definition** is the same as placing any other:
  Silo creates the execution in a Grain's lane, copying the definition at
  the commit it read, if the Grain's local policy trusts the repo the
  definition is in (§9.6).

The worked flows are in §10.10.

### 9.3 Existing `.grain/` declarations become grainfiles

bwsalmon/grain's `.grain/` declarations (`docs/declarations.md`,
`docs/declaration-schema.md`) **converge on the grainfile format**: each
becomes a Markdown file with front matter, read by the same parser as
every other grainfile, named with its kind suffix
(`.grain/templates/<name>.template.md`, §2.1) in the repo's grain folder
(§9.1). Workflows are the one exception (§9.4). Templates keep exactly
the keys `pkg/declschema` defines for them today, and a suite becomes a
folder template (§6.13). Schedules
and triggers do not map one to one: a schedule becomes a `kind: trigger`
with `on.schedule`, and a path trigger becomes a `kind: trigger` if it
files work, or a `kind: gate` if it holds a merge (`gate: true`). Their
keys are re-nested, not reinvented, by the mapping in §13.1. The loader
accepts the hyphenated spellings (`auto-merge`) as aliases of the
`snake_case` ones (§2.4).

#### The mapping

| `.grain/` today | Becomes | `kind:` | Front matter | Body |
| --- | --- | --- | --- | --- |
| `templates/<name>.yml` | `templates/<name>.md` | `template` | every template key, verbatim; `fields` become `inputs` (§13) | the prompt, now Markdown rather than a YAML block scalar; `{{name}}` placeholders are already Jinja (§6.5) |
| `schedules/<name>.yml` | `triggers/<name>.md` | `trigger` | `on.schedule` holds the cadence, `timezone`, `overlap` and `catch_up`; `fires` holds the template (or `workflow:`) and its inputs; the rest stay top-level (§13.1) | notes: the YAML file's leading comment |
| `suites/<name>.yml` | `templates/<name>/<name>.template.md` and one task per template it names | `template` (`form: folder`) | `run:` holds the suite's keys (§6.13) | the suite's description: the leading comment |
| `triggers/<name>.yml` that files work | `triggers/<name>.md` | `trigger` | `on: merge` (or `pull_request`) with `paths` becomes `on.merge: {branches, paths, ignore, group_by}` (or `on.design-change` for the design-doc planner); its template becomes `fires.template`, a reference, and its variables (`{{group}}`, `{{from_sha}}`) become that template's declared inputs, bound by `fires.bind` (§13.1) | the leading comment |
| `triggers/<name>.yml` with `gate: true` | `gates/<name>.md` | `gate` | `paths` becomes `match.paths`; `human` and `sensitive` become `requires` and `sensitive` (§13.1) | the leading comment |
| `workflows/<name>.yml` | unchanged | — | — | — |

The new kinds' schemas are
[`template.schema.json`](../schemas/grainfiles/template.schema.json),
[`trigger.schema.json`](../schemas/grainfiles/trigger.schema.json) and
[`gate.schema.json`](../schemas/grainfiles/gate.schema.json).
[`widgets-code/.grain/gates/grain-declarations.gate.md`](../examples/grainfiles/README.md)
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
  a claim about the grainfile.
- **The refusals of `docs/declaration-schema.md` still hold, for every
  kind:** a repo declaration carries no `reads`, `writes`, `grants` or
  `target`, and its `context:` entries, and its `ref` inputs' defaults
  and values, name only its own repo (§7). The schemas say so, and readers enforce it on every grainfile
  in a code repo's grain folder, whatever its `GRAINOWNERS` says (§9.6). A job
  that needs another repo's access stays a settings-owned row.
- **A code repo may define capability grainfiles** in its grain folder
  (`.grain/capabilities/`), and they grant nothing there either: a Grain
  honours one only if its policy sanctions that folder as a source, and
  no policy does by default (§6.8, §10.13).
- **One parser.** The `grainfile` package's front-matter reader (§15)
  serves `pkg/repodecl` for `.md` declarations, Silo's code-repo reader,
  and `grain declarations check`.

### 9.4 Workflows stay YAML

**Decision: `.grain/workflows/*.yml` stay GitHub Actions YAML,** and are
not `kind:` grainfiles. The reasons:

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
  trigger of the workflow form, on that repo's Grain (§6.6).

The cost is that grain keeps its workflow parser beside the front-matter
parser. It has that parser already, and needs it for GitHub
compatibility anyway. Only files with a kind suffix are grainfiles (§2.1),
so YAML in `.grain/workflows/` is never misread.

### 9.5 Design docs in place

Design docs already in code repos (`docs/design/*.md`, with `{#D-n}` or
`{#HS-n}` anchors) are referenceable as `kind: design` grainfiles **where
they are**, renamed in place to the `design` suffix
(`docs/design/widgets.md` → `docs/design/widgets.design.md`, with a
tombstone `widgets.moved.md`; §2). Nothing else moves:

- The planner's `design-change` trigger watches `docs/design/**`, which
  matches both names, so bwsalmon/grain's planner keeps working while
  docs are renamed one at a time.
- Each renamed doc gains this front matter, and nothing else:

  ```yaml
  ---
  kind: design
  schema: grain/design@v1
  title: Widget description format
  status: implementing              # as §6.9
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
- **A doc not yet renamed is still referenceable** by path and
  anchor (`grain-code:docs/design/<name>.md#D-7`), as an asset:
  its anchors resolve and its section hashes compute (§6.9). It is not an
  grainfile, so readers check no ID allocation on it and Silo does not plan
  it.
- **Trailers carry over unchanged:** `{#D-n}` and `{#HS-n}` anchors, and
  `Design-Item:`, `Design-Rev:` and `Design-Removed:`. A doc in a code
  repo keeps its in-repo `Design-Item:` path, and a Silo-hosted design is
  cited by its ref. `pkg/designdoc`'s parsing becomes the shared parser.
- **ID allocation (§6.9) is checked on read**, because the forge check does
  not run in a code repo: by Silo when it reads the repo, and by
  `grain declarations check` in the repo's own CI, which can refuse the
  change before it merges.
- **Planning is one or the other.** A design in a code repo is planned by
  the in-repo planner, now a `fires_in: grain` trigger with
  `on: design-change` (`.grain/triggers/design-docs.trigger.md`), unless its
  front matter has `planning:`. With `planning:`, the team's
  `fires_in: silo` design-change trigger plans it on each change to
  `default_ref` that Silo reads (§6.9), and the in-repo trigger skips it.
  A design in a Silo repo is always planned by a team's Silo-fired
  trigger. Doing both is refused.

### 9.6 Write authority in code repos

the forge check (§8.3) runs only on the repos Silo hosts. In a
code repo, on GitHub, Gerrit, a depot or anything else, **write
authority comes from that repo's own host and review process**: whatever
its branch protection, required reviews, gates and merge queue
let land on `default_ref` is what the repo says. Silo neither proxies
nor mirrors those pushes. Its `.grainconfig`, if it has one, says
`host: other`, and its `GRAINOWNERS` describes what belongs there, but
nobody can check who pushed. Readers treat a code repo's grainfiles like this:

- **Trust follows the repo's merge process.** A grainfile on the code
  repo's `default_ref` is trusted exactly as much as code merged beside
  it: no more, because Silo did not check its pusher, and no less,
  because the repo's own review did. A repo that wants its grain folder
  reviewed more strictly than its code says so with a gate on
  `.grain/**` (§6.7), as bwsalmon/silo already does with a path trigger.
- **Silo validates on read.** Silo reads a code repo when one of its
  repos references it, on the host's push webhook where there is one,
  and otherwise on its next pass. For each changed file in a grain
  folder, it checks what the forge check would: the name and kind suffix
  (§2.1), schema, the path rules (§2.2), `GRAINOWNERS` on read (§8.4),
  references
  (§3), design ID allocation (§6.9), and the invariants of §9.2. It
  resolves references from a code repo with the rights of whoever is
  using them, never with its own (§12.4). A grainfile that fails is shown
  in the repo's health view, and as a comment on the change if Silo
  opened it, and is not used. Silo never writes to a code repo except
  as a person, for a definition or a comment they write there.
- **Grains are the PEPs, as everywhere.** A Grain runs a code repo's
  definition only through an execution in one of its lanes, whose
  attestation's `spec_digest` must match the snapshot (§12.7). Its local
  policy may limit the code repos it reads templates, triggers and
  designs from. Definitions are data that an accepted task may use; they
  grant nothing (§8.4).
- **What a code-repo grainfile can do is bounded by the design, not by its
  `GRAINOWNERS`.** Anyone who can merge to the repo can edit it and its
  `.grainconfig`, so they are advisory there, and the limits below hold
  whatever they say:
  - a definition there runs only as an execution in a Grain's lane, approved there (§9.2);
  - a template, trigger, gate or suite carries no `reads`, `writes`,
    `grants` or `target` (§9.3);
  - no execution is ever there (GF047);
  - a trigger fires only where a Grain or a team named it in a trigger
    root (§6.6), and
    a workflow trigger runs only that repo's own workflows;
  - a gate there governs only that repo: it carries no `match.repos`.

A standalone Grain may trust a code repo further, since its owner's
policy decides. It may create executions of definitions committed there
by hand, for instance, if its policy names that repo.

## 10. Flows

Each flow below lists the commits each repo receives. §10.1–§10.8
follow one definition from proposal to a cross-Grain dependency; the
rest are worked flows for linking, definitions in code repos, roots,
`context:` and capabilities, each matched to files in
[`examples/grainfiles/`](../examples/grainfiles/README.md).


### 10.1 Propose

**By git:** a user clones the team's intake repo, adds
`tasks/<yyyy-mm-dd>-<slug>.task.md`, commits and pushes.

**The forge check allows it** under the intake's `authenticated` rule:

- the push may only *create* files;
- each new file must be a `kind: task` with no state (a definition
  refuses `state`, `approved_by` and the rest, GF020);
- `submitter` must equal the pusher.

**Silo's UI, CLI and API** (`POST /v1/tasks`) make the **same commit**,
as the caller, with their delegated token, `submitted_via: api`.

**Discussing it** adds `thread/` comments; the first one folds the file
into `tasks/<name>.task/task.md` in the same commit (§5), as
[`hub-intake/intake/tasks/2026-10-16-approval-flow.task/`](../examples/grainfiles/hub-intake/intake/tasks/2026-10-16-approval-flow.task/task.md)
shows.

Commits: **intake**, 1, and 1 per comment.

### 10.2 Place, and approve

**Placing is creating an execution** (§6.2). The dispatcher chooses a
Grain the same way it does today (hub.md §6.2), among the Grains whose
roots have a `hub/` lane the placing user may create in, and Silo, **as
that user**, commits **one folder in the Grain's root**:

1. `hub/<definition name>.exec/exec.md`, with:
   - `definition:` the definition's canonical reference at the commit it
     read, and `definition_sha256` of its bytes;
   - `lifecycle:` the definition's (or its folder's) lifecycle, pinned;
   - `state:` the lifecycle's `initial`, or, when the placing user is an
     approver and the lifecycle allows it, the first `ready`-category
     state with `approved_by` the user;
   - `rank` and `priority` (after the lane's last execution unless the
     user says otherwise), `created_by`, `created_at`;
   - `depends_on`: each definition dependency mapped to an execution
     (§10.8);
   - `attestation`: the signed-claims JWT of hub.md §6.4, with `aud`
     this Grain, the execution's global identity, and `spec_digest`
     computed over the snapshot.
2. `hub/<definition name>.exec/task.md`, the snapshot: the definition's
   bytes at that commit.

Then Silo notifies the Grain over the WebSocket:
`{"type":"pull","repo":…,"sha":…}`. **The definition is untouched**,
and nothing is written in its repo.

**Approving** (if the execution was created in its initial state): an
approver edits `exec.md`, `state` to the approving state and
`approved_by: user:<self>`, `approved_at`, through Silo or by a push to
the Grain's repo. The forge check checks:

- the pusher holds the approving transition in the Grain root's
  `GRAINOWNERS` (`state(*->sec-approved)` for the security team, in the
  example);
- `approved_by == $pusher`;
- `approved_by != ` the definition's `submitter` (INV-17 in
  [threat-model.md](threat-model.md));
- the only fields changed are `state`, `approved_by` and `approved_at`.

**Rejected: wrong pusher.** `mallory@example.com`, who is not an
approver, pushes `state: sec-approved, approved_by:
user:approver@example.com`. The forge check refuses it:

```
grainfiles/authorize — failing: GF074, GF073 (the forge check, on the pull request)
hub/2026-10-16-approval-flow.exec/exec.md: ['user:mallory@example.com'] may not move this execution triage -> sec-approved
hub/2026-10-16-approval-flow.exec/exec.md: ['user:mallory@example.com'] may not change field 'approved_by'
```

Commits: **Grain root**, 1 (the execution), plus 1 by the approver if
it was not created approved. **Intake**: none.

### 10.3 Unplaced work, and the team's view

A definition with no live execution is **unplaced**. Silo's intake view
lists them from the definition → executions index (§5), with each
definition's thread. There is no ranking of unplaced work in git: a
rank means something only within one Grain's lane. A team that wants an
ordered queue for a Grain places work in its lane and ranks it there.

Commits: none.

### 10.4 The Grain runs it

1. The Grain fetches the trees of its root entries (prompted by the
   notify, and also every few minutes in case a notify was missed), and
   re-lists the entries whose folder's tree hash changed (§11.1, §11.2).
   It finds a new execution folder, validates `exec.md`, the snapshot
   and the attestation (§12.7), and holds the snapshot to local policy,
   as it does an offer today. It reads nothing from the definition's
   repo to decide anything.
2. **Accept:** when the lanes give it the execution (§6.2), it is in a
   `ready`-category state, and a slot is free, the Grain creates
   `runs/<utc>/run.md` inside the execution with `status: accepted`,
   applies the `accepted` outcome transition on `exec.md`, and pushes.
   It resolves the snapshot's `context:`, holds every entry to its
   policy, and materialises the sandbox (§7). Then it commits
   `status: running`, with the record's `context:` saying what was
   materialised and skipped. An entry the policy refuses makes the run
   `refused` instead, naming it.
3. **While it runs:** the agent's thread is pushed as comment files in
   the run folder, at most one push every 5 seconds, batched. Coarse
   waits are `awaiting_reply` or `held`. Events stream over the
   WebSocket as they do today.
4. **On finish:** one commit sets `status: done|failed` and writes the
   `closed:` snapshot (`outcome`, `at`, `summary`, `change: {url,
   merge_commit, branch}`, `design_revs`, `follow_ups: [<ref>]`), and
   applies the outcome transition on `exec.md` (`done → completed`, or
   `shipped` in the security lifecycle).
5. **A failed run** proposes its transition (`failed → queued`, or
   `sec-approved`); the next attempt is a new run folder in the same
   execution, as
   [`grain-eu/hub/2026-10-16-approval-flow.exec/runs/`](../examples/grainfiles/grain-eu/hub/2026-10-16-approval-flow.exec/exec.md)
   shows with a failed run and a running one.

Commits:

- **Grain root**: 1 (accepted) + 1 (running) + ⌈comments/batch⌉ + 1
  (closed), all by `deployment:<id>`;
- the definition: none, ever.

The Grain writes only its root, and only `runs/` and the outcome
transitions of its executions. It cannot change the snapshot (the forge
check refuses it), and it cannot write the definition at all, because
the definition is in another repo. So a Grain cannot rewrite what it was
asked to do, or claim to have been asked.

### 10.5 Reorder, and change lanes

- **Anyone with the rank fields** (the Grain's owner, the lane's
  members through Silo) edits `rank` (or `priority`, `expedite`) in one
  `exec.md`. Silo commits with `ranked_by` the person it acted for.
  Commits: **Grain root**, 1.
- **Changing lanes** is a `git mv` of the execution folder, say from
  `hub/` to `backlog/`, authorised by `move` on it. No tombstone: the
  execution is found by name across the Grain's lanes. Commits: **Grain
  root**, 1.
- **The owner reweights the lanes:** edit `backlog:` in the root's
  `grain.config.md`. Commits: **Grain root**, 1, by the owner.
- **Silo expedites** (an operator's `at_front`): `expedite: true` on
  the execution. It reorders within its lane only, unless the root's
  `lanes.expedite` is `global`. Commits: **Grain root**, 1.

Every one of these is one commit in one repo.

### 10.6 Cancel

A user asks in Silo's UI, or an approver through the API. Silo, as that
user, writes on the **execution**:

1. **The Grain root:** `state` to a `cancelled`-category state and
   `cancel: {by, at, reason}` on `exec.md`, if `GRAINOWNERS` grants the
   user that transition. Silo notifies the Grain.
2. **The Grain honours it.** It reads `exec.md` whenever it pulls, and
   before each attempt. A Grain with an open run stops it and commits
   `status: cancelled` with `closed: {outcome: cancelled, at}`. A Grain
   with no run yet never starts one.

To stop all work on a definition, Silo cancels each of its live
executions (from the definition → executions index), one commit in each
Grain's root; the definition itself is untouched, and a comment on its
thread may say why.

Commits: **Grain root**, 1 by the user (the cancel) and 1 by the Grain
(its run cancelled).

### 10.7 Re-placement

Re-placement happens after a lost Grain (the team's grace, hub.md
§6.3), a refusal, an offer timeout, or a person moving the work. **The
old execution does not move**; a new one is created on the new Grain:

1. **The new Grain's root, one commit by Silo as the user:** a new
   execution, as in §10.2, with the same `definition` at the same
   commit (so the snapshot is identical, and the approval carries over
   with it) and `previous:` naming the old execution by URL.
2. **Silo asks the old Grain to withdraw**, over its node connection
   (and, if it is offline, when it returns).
3. **The old Grain's root, one commit by the old Grain:** `superseded:
   {at, by, reason}` on the old `exec.md`, naming the new execution, its
   state moved to a `cancelled`-category state with `cancel`, and any
   open run `superseded`. **Only a Grain writes its own executions.** A
   Grain that is gone for good is withdrawn by its root's owner, the only
   other writer its `GRAINOWNERS` has.

**The fence** is on the new side, and needs no write in the old Grain's
root. The new Grain does not dispatch an execution whose `previous` is
not yet superseded or finished (`done`- or `cancelled`-category, no open
run), reading it by reference. So two Grains never run the same work at
once:

- if the old Grain comes back mid-task, it is told to withdraw, stops,
  and writes the one commit. A branch or pull request it had already
  pushed is reported in the hub as belonging to a superseded execution;
- until it does, the new execution waits, shown as "waiting for
  `<previous>` to withdraw"; an owner may withdraw it by hand.

**History shows both**: the old execution, with its thread and runs,
stays in the old Grain's root, and the new execution's `previous:` links
to it. The example is
[`grain-eu/hub/2026-10-16-widget-schema-v2.exec/`](../examples/grainfiles/grain-eu/hub/2026-10-16-widget-schema-v2.exec/exec.md)
and its replacement
[`grain-us/hub/2026-10-16-widget-schema-v2.exec/`](../examples/grainfiles/grain-us/hub/2026-10-16-widget-schema-v2.exec/exec.md).

Commits: **new Grain root**, 1 by Silo as the user; **old Grain root**,
1 by the old Grain. No third repo is written.

### 10.8 A cross-Grain dependency completes

Definition B, `intake:tasks/2026-10-16-refuse-self-approved.task.md`,
has `depends_on: [../4310-carry-approver.task.md]`, which is definition
A. A's execution ran on Grain `eu`; B is placed on Grain `us`.

**Placing B maps the dependency to an execution.** A definition's
`depends_on` names definitions, but only an execution can be done. When
Silo creates B's execution on `us`, it offers A's executions from the
definition → executions index, and the placing user picks one (normally
the live one); Silo writes it into B's `exec.md` as
`depends_on: ["https://github.com/acme/grain-eu.git//hub/4310-carry-approver.exec/"]`.
A dependency with no execution to map to is refused at placement: it is
never left implicit.

**A dependency is satisfied when the execution it names is in a
`done`-category state.** `cancelled` and dangling dependencies keep B
blocked, with the reason shown.

1. Grain `eu` closes A's run and moves A's execution to `completed`
   (`eu:hub/4310-carry-approver.exec/exec.md`).
2. Silo's post-receive looks A up in its derived indexes (§5), a hint
   only: B's execution depends on it. It notifies `us` to pull `eu`.
3. `us` reads A's `exec.md` from `eu`, by the reference in B's
   `depends_on`, sees a `done`-category state, and unblocks B.

`us` needs read access to `eu`, which Silo granted when it placed B,
because B's dependency lives there (§12.4). A Grain without that access
sees the dependency as dangling, and keeps B blocked.

Commits: **eu**, 1 (A's close). The dependency needs no commit in `us`.

### 10.9 The remaining worked flows

**An agent's upward filing.** An agent on Grain `eu` calls
`propose_task` for another team's repo. The Grain does not push to that
team's intake, because it never writes other repos. It asks Silo, over
its node connection, as `propose_task` does today. Silo checks that the
Grain has an open run whose definition has that submitter, then commits
to the team's intake a new definition with:

- `submitted_via: agent`;
- `submitter`: the human who submitted the originating definition;
- `filed_by: {deployment: eu, run: "eu:hub/4310-carry-approver.exec/runs/20261016T135830Z/run.md"}`.

Silo returns the new definition's path. The Grain writes a comment in
its run folder naming it, and lists it in `closed.follow_ups` when the
run closes.

Commits:

- **intake (other team)**, 1, by Silo;
- **eu**, 1, by the Grain: the comment in its run folder.

It is then placed and approved like any definition. A standalone Grain
files into its own repo instead.

**A schedule firing a templated task.** The `prune-packages` trigger in
`hub/shared` (`on.schedule: {weekly: {day: sunday, at: "04:00"}}`,
`fires_in: silo`, `fires.into: "https://github.com/acme/grain-eu.git//hub/"`)
comes round at 04:00 on Sunday. Silo renders the template its `fires:`
names at its current tip `<sha>`, and commits
`hub/fired/prune-packages/20261011T0400Z.exec/` to eu's root with:

- `definition: "shared:templates/prune-packages.template.md@<sha>"` and
  the rendered task as the snapshot, `inputs` recorded;
- `state` the first `ready`-category state and `approved_by:
  hub:trigger`. This is allowed only because the trigger's
  `auto_approve_proposals` was itself approved when the trigger merged,
  and no person caused the firing;
- `trigger: {ref, on: schedule, key: 20261011T0400Z, fired_at}`.

Silo's database records the fire and computes the next one; nothing
about it is committed to `hub/shared`. Had Silo been down at 04:00,
`catch_up: {mode: latest-only}` would fire it once on restart, at the
same path. A hub approver's
`silo trigger run shared:triggers/prune-packages.trigger.md` (its
`manual:` source) creates the same execution at
`hub/fired/prune-packages/<request time>-<approver>.exec/`.

Commits: **Grain root**, 1. **Intake**: none.

**A design edit fires the planning trigger.** The
`review/hs-approval-by-push` branch adds `### Approval is a push
{#HS-6}` and edits HS-3. An approver merges it to `main` in
`hub/shared`, and the forge check accepts it under §8.5. Post-receive
runs the `design-change` source for the triggers watching `designs/`,
which classifies the requirements:

- HS-6 is **new**;
- HS-3 is **changed, done**;
- the rest are unchanged.

With a trigger that says `fires.per: requirement`, Silo commits three
**definitions** to `hub/intake` in one commit, at the deterministic
paths of §6.9 (planning proposes work; it does not run it):

- implement HS-6;
- test HS-6;
- a delta for HS-3.

Each has `implements: [{…#HS-n, rev, hash}]`. Commits: **intake**, 1.
Placing and approving each one then follows §10.2. The example's
`plan-designs` trigger says `per: event` instead, so the same merge
creates one planner execution in a Grain's lane, whose agent proposes
the definitions; `fixtures/design-plan.inputs.json` is its binding.

**Link, then unlink, a standalone Grain.** Linking is an edit to the
Grain's root by its owner, and unlinking is the reverse edit. **Nothing
Silo writes can activate anything on a Grain unless it is in a lane the
Grain's owner declared** (§12.14): that is the PEP property the whole
channel rests on.

- **Link:** `grain hub login`, then `grain tasks link --team hub`.
  1. If the Grain's root is a local repo, Silo creates a hosted repo for
     it, `grains/<id>.git`, and the Grain pushes its local history
     there. This first push is allowed for the owner's credential only,
     and only into an empty repo. A root that already lives where Silo
     can read it stays where it is.
  2. `grain tasks link` shows the owner this diff, and commits it with
     their credential when they confirm:

     ```diff
      backlog:
        mode: weighted
        lanes:
     +    - {name: hub, path: hub/, weight: 3}
          - {name: own, path: backlog/, weight: 1}
     ```

     and, in `GRAINOWNERS`, the rules that let the team's members create
     and rank executions in `hub/`, its approvers approve them, and any
     signed-in user comment on them (§8.2).
  3. A registry maintainer registers the root (§8.7). Silo reads the
     root's `grain.config.md`, sees the `hub/` lane, and from then on
     places work on the Grain by creating executions there.

  Commits: **Grain root**, its history (if newly hosted) plus 1 by the
  owner; **registry**, 1.
- **Unlink:** `grain tasks unlink`, or removing the lane by hand.
  1. The owner's commit removes the `hub/` lane from `grain.config.md`
     (and its `GRAINOWNERS` rules). From that commit the Grain lists
     nothing in `hub/`, so it starts nothing from Silo.
  2. Silo sees the lane gone. It re-places the executions there that
     have no open run (§10.7) on other Grains; this Grain marks the old
     ones `superseded`. Open runs must close first; with `--force` the
     owner unlinks anyway and withdraws them.
  3. Silo stops placing on the Grain. The executions stay in the
     Grain's root, with their history, and the owner may archive them
     (§4.3).

  The root, with all of its history, was the Grain's all along.
  Commits: **Grain root**, 1 by the owner, plus 1 per withdrawn
  execution; **other Grains' roots**, 1 per re-placed execution.

### 10.10 Worked flows for definitions in code repos

The same work with the definition in the intake and in a code repo, with
the commits each repo receives. "The work" is the change the agent
makes, which lands in the code repo through its review either way.

**In the intake.** Definition 4310 in the examples: proposed, discussed,
placed on `eu`, approved and run there, the definition at
`intake:tasks/4310-carry-approver.task.md` throughout.

| Step | Intake | Code repo | Grain root (`eu`) |
| --- | --- | --- | --- |
| propose (§10.1) | 1: the definition | — | — |
| discuss | 1 per comment (the first folds it) | — | — |
| place (§10.2) | — | — | 1, by Silo as the user: the execution and its snapshot |
| approve (§10.2) | — | — | 1, by the approver (or none, if placed approved) |
| run (§10.4) | — | the work | `accepted`, `running`, the thread's batches |
| close (§10.4) | — | none | 1, the run closed and the outcome transition |

**In a code repo.** The example's
[`widgets:.grain/tasks/2026-10-16-widget-schema-v2.task/`](../examples/grainfiles/widgets/.grain/tasks/2026-10-16-widget-schema-v2.task/task.md),
which implements WG-2 of the widgets design and will be revisited with
each format version. It was proposed in the intake, and a member moved
it next to the code with its thread, leaving
[`hub-intake/intake/tasks/2026-10-16-widget-schema-v2.task.moved.md`](../examples/grainfiles/hub-intake/intake/tasks/2026-10-16-widget-schema-v2.task.moved.md).

| Step | Intake | Code repo | Grain roots |
| --- | --- | --- | --- |
| propose | 1: the definition | — | — |
| move it next to the code | 1: the tombstone, after the code repo's merge | 1 change adding `.grain/tasks/2026-10-16-widget-schema-v2.task/` with `moved_from` and its thread, through the repo's review and its `.grain/**` gate (below) | — |
| discuss | — | 1 per comment, in `thread/`, through the repo's own rules | — |
| place on `eu` | — | — | `eu`: 1, the execution, copying the definition at the code repo's commit |
| re-place on `us` (§10.7) | — | — | `us`: 1, the new execution with `previous:`; `eu`: 1, `superseded` |
| run, close | — | the work | `us`: the run's commits |

A definition written straight into a code repo, never proposed in an
intake, is the same from the second row on.

**A gated change held for a human approver.** The move above is a
change to `.grain/tasks/**` in the widgets repo, and the widgets repo's
[`.grain/gates/grain-declarations.gate.md`](../examples/grainfiles/widgets/.grain/gates/grain-declarations.gate.md)
matches `.grain/**`, `requires: {human: {when: always}}`,
`sensitive: true`. Grain `eu` runs grain's merge queue for the widgets
repo:

1. The queue reads the widgets repo's gates at `main`'s tip, not at the
   change's head, and finds this one matching. No other gate matches.
2. The match is sensitive, so the queue withdraws the change's
   auto-merge and labels it sensitive.
3. Any review the gate requires is filed as `eu`'s own execution, in its
   `backlog/gated/` lane, approved (`approved_by: deployment:eu`), with
   the change in its snapshot's `context:` to read. Nothing is filed in
   the widgets repo.
4. bsalmon@google.com, a hub approver, Submits. The author of the
   change and the member who moved the definition would not have
   counted; an approver who was neither does. The queue checks the
   approver at `main`'s tip, merges, and the tombstone step follows.

| Step | Intake | Code repo | Grain root (`eu`) |
| --- | --- | --- | --- |
| the change opens | — | the change, held: no merge | — |
| the gate's review, if any | — | — | 1: the review execution in `backlog/gated/`; then its run's commits |
| the human's Submit | — | 1: the merge queue's merge on `main` | — |
| the change merges | 1: the tombstone | — | — |

Had the gate said `when: on-findings` (as bwsalmon/silo's
`hub-trust-boundary` path trigger does today), a clean review would have
released the change with no human. The same change into a Silo-hosted
repo is held by the forge check instead: the review branch's merge is
accepted only with the review's outcome and the approver's Submit in its
trailers, checked against the gate at the ref's tip (§6.7).

### 10.11 Worked flows for roots

Each flow below says what changes, what the Grain reads, and the commits
it makes. The examples hold every file named.

**Adding work.** `eu`'s owner wants to tidy the run notes.

1. They write the definition in their own root,
   [`tasks/2026-10-16-tidy-run-notes.task.md`](../examples/grainfiles/grain-eu/tasks/2026-10-16-tidy-run-notes.task.md),
   no state.
2. In the same commit they create its execution in the `own` lane,
   [`backlog/2026-10-16-tidy-run-notes.exec/`](../examples/grainfiles/grain-eu/backlog/2026-10-16-tidy-run-notes.exec/exec.md):
   `exec.md` (`definition: "/tasks/2026-10-16-tidy-run-notes.task.md@<sha>"`,
   rank `m`, approved by the owner) and the snapshot.
3. The Grain lists `backlog/` (a tree read), finds one new `.exec/`
   folder, parses `exec.md`, and ranks it in the lane.

The definition alone would do nothing: it is reached only from an
execution. A team member's definition goes to the intake instead
(§10.1), and is active on a Grain only once Silo creates an execution of
it in a lane the Grain names (§10.2). Commits: **Grain root**, 1.

**Silo placement and re-placement.** Silo places
`widget-schema-v2` on `eu` by creating
`hub/2026-10-16-widget-schema-v2.exec/` in `eu`'s root, as the placing
user (§10.2). `eu` lists it because its `hub` lane is `hub/`. When `eu`
cannot run it (no widgets toolchain), Silo creates
`hub/2026-10-16-widget-schema-v2.exec/` in `us`'s root with `previous:`
naming `eu`'s, and `eu` marks its own `superseded` (§10.7). Neither
Grain's `grain.config.md` changes. Commits: **eu**, 1 (the execution),
then 1 (`superseded`); **us**, 1 (the new execution).

**Linking and unlinking** are §10.9: the owner's commit adds or removes
the `hub/` lane in `grain.config.md`.

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
   and commits to its own root, in one commit, the execution
   `backlog/fired/shared/granule-pull/20261016T0600Z.exec/` (the
   template at that commit as its `definition`, the rendered task as its
   snapshot). The entry tracks a branch and the trigger says
   `auto_approve_proposals: false`, so the execution is in its `initial`
   state, and the owner approves it.
3. It runs from the `own` lane like any local execution.

Commits: **Grain root**, 1 (the firing), 1 (the approval), then the
run's own. **Silo**: none.

**Ignoring one team trigger via the entry's `ignore:`.** Grain `us`
wants the team's granule pull weekly, not daily. `us` names the team's
`shared:triggers/@main` as a trigger root. `us`'s owner commits:

```diff
 triggers:
   - path: triggers/
   - path: "shared:triggers/@main"
     ref_mode: track
+    ignore: ["granule-pull.trigger.md"]
```

and adds `triggers/granule-pull-weekly.trigger.md`, a whole trigger with
`derived_from` naming the original. The edit changes one entry's
`ignore:`, so only that entry's listing-cache key changes (§11.2); its
next listing drops the daily trigger, and the own entry's listing gains
the weekly one. Nothing in Silo's repos changes, and no other Grain is
affected. Commits: **Grain root**, 1.

**A team retiring a trigger by moving it to `archive/`.** The hub team
stops the legacy triage sweep. A maintainer merges one commit to
`hub/shared` that moves `triggers/legacy-triage-sweep.trigger.md` to
`triggers/archive/legacy-triage-sweep.trigger.md` (the archive mapping,
§4.3: no tombstone). Every Grain whose trigger root names
`shared:triggers/` re-lists that folder on its next fetch (its tree hash
changed), skips `archive/`, and unregisters the sweep. No Grain's config
changes, and executions it fired before still resolve their
`trigger.ref` through the archive mapping. Commits: **shared**, 1.

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
add to the team's folder becomes a review branch there (§8.5).
Commits: none to read; **Grain root**, 1 per note the memory agent
writes.

**A gate enforced from a code repo's `.grain/`.** A change to
`widgets:.grain/templates/release-notes.template.md` enters grain's
merge queue for `main`. The queue lists `.grain/` at `main`'s tip (trees
only) and finds `gates/grain-declarations.gate.md`, whose `match.paths`
is `.grain/**`. It files any review the gate requires as an execution in
the landing Grain's `backlog/gated/widgets/…`, withdraws auto-merge
(`sensitive: true`), and holds the change until the review is done and a
hub approver submits (§6.7, §10.10). No Grain's `grain.config.md`
mentions the gate, and no Grain's `ignore:` could release it. Commits:
**Grain root**, 1 (the review execution) and its run's; **widgets**, the
merge once released.

**A capability requested by reference and allowed by policy.** The
definition `2026-10-17-widgets-preview-deploy.task.md` lists
`shared:capabilities/widgets-deployer.capability.md` with `mode: use`.
Nothing made that file active: `eu` resolves it only when it materialises
the run (§7). It walks the chain to
`org-capabilities:gcp/acme-widgets.capability.md@v1` and its base
`gcp-service-account@v1`, checks that every link's repo is a sanctioned
source for that base in `eu`'s local policy, applies the reducers, and
checks the implications (§6.8, §10.13). A capability grainfile in a repo the
policy does not list would be refused at this step, whoever referenced
it. Commits: none until the run's record.

**A `GRAINOWNERS`-rejected push.** A teammate with read access tries to
put their own work at the front of `eu`'s queue by pushing
`backlog/urgent-thing.exec/` to `eu`'s own lane:

```
grainfiles/authorize — failing: GF073
backlog/urgent-thing.exec/exec.md: ['user:alex@example.com'] may not create this file
  nearest GRAINOWNERS: /GRAINOWNERS, line 1: *  @user:bsalmon@google.com
```

Only `hub/` lets the team create executions, and only as themselves;
the teammate places it there through Silo, where it is ranked within the
hub lane and approved by an approver. Commits: none.

**A misplaced file.** The owner saves a comment directly in an
execution folder, `backlog/2026-10-16-tidy-run-notes.exec/note.comment.md`,
instead of in its `thread/`:

```
grainfiles/authorize — failing: GF007, GF001
backlog/2026-10-16-tidy-run-notes.exec/note.comment.md: a comment may not be at note.comment.md inside a .exec/ folder (spec §4.3)
```

A reader treats the file as attached to nothing. Commits: none.

**A warm one-file push.** Silo commits one execution,
`hub/2026-10-17-fix-dns.exec/`, in `eu`'s root and notifies `eu`.

1. `eu` fetches the new commit's trees for its root: the root tree,
   `hub/`, and the new folder (3 trees, a few KB); `backlog/`'s and
   `triggers/`'s hashes are unchanged.
2. The hub lane's tree hash changed, so its listing misses the cache;
   the walk finds one folder not in the old listing. Every other entry's
   key is unchanged and hits.
3. It reads one blob, the new `exec.md`, parses it (one front-matter
   cache miss), and re-ranks the lane; when it is picked, it reads the
   snapshot beside it.

Cost: 3 trees and 2 blobs. The definition's repo is not touched.

**A sparse cold fetch.** A fresh `eu` with an empty cache and the config
of §4.2:

1. It reads `grain.config.md` (one blob) and `.grainconfig` (one blob)
   from its root.
2. For each of its 6 entries it fetches the trees along the path and
   under the folder, at the entry's commit, with `--filter=blob:none`:
   nothing else in those repos. No intake is listed, and the root's
   `hub/archive/` and `backlog/archive/` are never listed.
3. It lists each entry from trees alone: names and kinds, and the
   `ignore:` and `archive/` cuts.
4. It reads the blobs it needs to start: every `exec.md` in its lanes
   (to rank them), every trigger (to register them), and memory documents
   when the first run starts. Then, for the first pick, its snapshot and
   whatever the snapshot's `context:` names.

**A Grain picks its next execution across lanes.** Grain `eu` has two
lanes with runnable executions, `backlog.mode: weighted`, with the hub
lane at weight 3 and its own `backlog/` at 1:

| Lane | Its order (§6.2) |
| --- | --- |
| `hub`: `hub/` | `2026-10-16-reject-dangling-refs` (expedite), `2026-10-16-approval-flow` (`high`, rank `t`), `2026-10-16-widget-schema-v2` (rank `w`); the prune firing is skipped, because it already has an open run |
| `own`: `backlog/` | `2026-10-16-tidy-run-notes` (rank `i`) |

Credits start at 0. Pick 1: Silo's lane has 3, the root's 1, so Silo's
lane gives `reject-dangling-refs` and pays 4 (−1 against 1). Pick 2: 2
against 2, a tie, which list order breaks for Silo's lane:
`approval-flow` (−2 against 2). Pick 3: 1 against 3: the root's lane
gives `tidy-run-notes`. Pick 4: only Silo's lane is left:
`widget-schema-v2`. the earlier `check.py` printed the same: `next on eu (weighted):
2026-10-16-reject-dangling-refs, 2026-10-16-approval-flow,
2026-10-16-tidy-run-notes, 2026-10-16-widget-schema-v2`. Under
`mode: strict` with the root's lane first, `tidy-run-notes` would go
first and Silo's three after it. Nothing is written to pick: the choice
shows up as the next run's `accepted` commit in the root.

### 10.12 Worked flows for context and assets

**A task with a write repo, a read folder, a design doc and an image.**
`intake:tasks/2026-10-16-reject-dangling-refs.task.md` is placed on
Grain `eu`. Its `context:` is the one at the top of §7: silo at `main`
to write, grain's `pkg/daemon/` at `candidate` to read, the hub intake
design with `depth: 1`, and a screenshot attached to the task, at
`attachments/dangling-ref-error.png` beside it.

1. **Resolve and check.** `eu` resolves each entry with its node
   credential, and holds each to its policy before reading it. Its
   `repos:` allows `https://github.com/bwsalmon/*` for `read` and
   `write`, and `https://silo.example.com/git/hub/*` for `read`. All four
   pass. The screenshot is relative, so it reads the definition at the
   execution's `definition` commit.
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
   (§7): five entries with their commits and paths, and one skipped
   link.
5. **Work and push.** The agent changes `work/silo/` and pushes its
   branch. A push to `main`, or to grain, is refused by the proxy.

Commits: **eu**, the run's own (§10.4). Nothing in any other repo.

**A refused task: one entry is outside the policy.** The weekly granule
pull (`us:triggers/granule-pull-weekly.trigger.md`) fires on Grain `us`, and its
task's `context:` is grain at `candidate` to write and
`https://github.com/gke-labs/granule.git@main` to read (§6.6). Suppose
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
   in `refusal`, and Silo could place it elsewhere (§10.7).

Commits: **us**, 1 (`waiting`), then the run's own once the policy admits
it.

**A template whose input chooses the target repo.**
`shared:templates/ghcr/prune-packages.template.md` declares an input `repo` of
type `ref`, `ref_kind: repo`, defaulting to grain at `candidate`, and has
`context: [{input: repo, mode: write}]`. Its trigger,
`shared:triggers/prune-packages.trigger.md`, says `fires.inputs: {repo:
"https://github.com/bwsalmon/grain.git@candidate"}`. When it fires in
Silo:

1. Silo checks the value against §3's grammar and `ref_kind`: a repo, with
   a branch. A value naming a file, or `@<tag>`, files nothing and is
   reported on the trigger.
2. Silo writes the task at
   `intake:tasks/fired/prune-packages/20261011T0400Z.task.md` with
   `context: [{ref: "https://github.com/bwsalmon/grain.git@candidate",
   mode: write}]` and `template.inputs.repo` recording the value. No
   `input:` survives into a task.
3. The execution's snapshot holds that `context:`, the attestation's
   `spec_digest` covers it, and the Grain's policy checks it like any
   snapshot's. Another team's trigger can use the
   same template for its own repo by setting `repo` to it.

Commits: **intake**, 1, as any firing (§10.9).

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
screenshot of the forge check's refusal. Today it is a blob on the task. After
`grain tasks export`, it is
`intake:tasks/2026-10-16-reject-dangling-refs/attachments/dangling-ref-error.png`,
next to `task.md`, and the task's `context:` lists it (§2.3). A reply
with an attachment on a running task arrives through Silo's relay. The
Grain commits the comment and its file, at
`runs/<name>/comments/<utc>-<author>.md` and
`runs/<name>/comments/<utc>-<author>/<file name>`, and drops both into
the sandbox's `thread/`.

### 10.13 Worked flows for capabilities

**A task requests a narrowed GCP capability, defined by a team on top of
an org definition.** `intake:tasks/2026-10-17-widgets-preview-deploy.task.md`
asks for the widgets repo at `main` to write, and for
`shared:capabilities/widgets-deployer.capability.md` with `mode: use`, narrowed
further to 30-minute tokens valid until 2026-10-24 (§6.8). It is placed
on `eu`.

1. **By reference only.** Nothing in `eu`'s `grain.config.md` names the
   team's `capabilities/`: there is no capability root. The task's
   `context:` entry is the only thing that reaches the definition, and
   reaching it grants nothing.
2. **Placement and approval.** The approver approved the execution,
   reading its snapshot's `context:`. The capability asks for no
   approval of its own. Silo resolves the chain as `eu` would, using
   `eu`'s published base declarations. It created the execution on `eu`, whose declarations include
   `gcp-service-account@v1`, with an attestation that carries
   `granted_capabilities: [{ref, digest}]`.
3. **Resolve the chain.** `eu` resolves entry 2 with its node credential.
   Before each read, the gate checks that the link's source is
   sanctioned. The team's grainfile is in `hub/shared`'s `capabilities/`,
   which `eu`'s policy sanctions for `gcp-service-account@v1`. Its base
   is `org-caps:gcp/acme-widgets.capability.md@v1`, in the org's capabilities repo,
   which is sanctioned for that base too. That grainfile's base is
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
   so it refuses the run, naming the reducer:

   ```yaml
   state: refused
   refusal:
     reason: "reducer 3 (preview-services-only) is language: anselm, which eu cannot evaluate yet"
     entry: {index: 2, ref: "shared:capabilities/widgets-deployer.capability.md", mode: use, link: "shared:capabilities/widgets-deployer.capability.md", reducer: 3, rule: capabilities.anselm}
   ```

   Silo shows that as the task's waiting reason. Placing it elsewhere
   does not help, because no Grain evaluates Anselm yet. The team can
   drop the reducer, which is a new digest and, for an approved task, a
   new task with `supersedes:` (§6.1), or wait. `eu` never skips the
   reducer.
6. **Once `eu` evaluates Anselm,** the reducer is loaded into the Google
   API proxy as a per-use check. The implications are egress to
   `oauth2.googleapis.com`, `sts.googleapis.com` and `*.googleapis.com`,
   with no repos and no tools, and `eu`'s `network` allows them. The
   digest matches the attestation's. `eu` mints nothing yet. It writes
   `.gcp-credentials.json`, a credential config naming its token
   endpoint, and records the run's `capabilities` (§6.8).
7. **Use.** The agent runs `gcloud run deploy preview-schema-v2 --region
   europe-west1`. The token endpoint mints a 30-minute token for the one
   account. Each API call passes the proxy, the Anselm reducer allows
   it, and each decision is logged with the reducer that made it.
   the earlier `check.py` computed the same effective capability from the example
   files and prints it.

Commits: **eu**, the run's own. Nothing in the org's or the team's repo.

**A refused request: the definition comes from an unsanctioned
source.** The widgets repo's grain folder gains
`.grain/capabilities/widgets-admin.capability.md`: `gcp-service-account@v1`, project
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
   add `shared:capabilities/widgets-admin.capability.md`, a sanctioned source, with
   `base: {capability: "widgets:.grain/capabilities/widgets-admin.capability.md"}`.
   The first link passes, and the second is refused for the same reason.
   Every link must be sanctioned, because the base-most link is where
   the authority is defined (§12.30).

Commits: **eu**, 1 (`refused`).

**A refused request: implied egress the policy does not allow.** A task
placed on `us` requests `shared:capabilities/sandbox-repo.capability.md`, a grainfile
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
     entry: {index: 3, ref: "shared:capabilities/sandbox-repo.capability.md", mode: use, rule: network.egress}
   ```

3. Silo may place the task on a Grain whose policy allows that egress
   (§10.7). Silo can tell from the Grains' published declarations which
   bases they implement, but a Grain's policy stays its own, so a
   refusal is how Silo learns it.

Commits: **us**, 1 (`refused`).

**A capability that needs approval.** The weekly GHCR prune
(`shared:triggers/prune-packages.trigger.md`) has `auto_approve_proposals: true`.
After migration its template's `capabilities: [package-publish]` becomes
a `use` entry for `shared:capabilities/legacy/package-publish.capability.md`, over
`github-packages@v1`. That base is one of grain's escalated
capabilities, so its declaration says `approval: required`, and the
grainfile adds `approval: {by: ["team:hub#approver"], scope: task}`.

1. **The firing.** Silo files
   `intake:tasks/fired/prune-packages/<key>.task.md`, and the
   trigger's auto-approval approves the task (`approved_by:
   silo:trigger`). That approves the task, not the capability.
   `silo:trigger` is not a human, and not among the principals the
   approval names, so Silo does not place the task. It shows "waiting:
   capability approval of `shared:capabilities/legacy/package-publish.capability.md`
   by a `team:hub#approver`".
2. **The approval.** An approver reads the task in Silo's UI and approves
   the capability. Silo commits, for them,
   `capability_approvals: [{ref, digest, by: "user:approver@example.com",
   at}]` on the task, under the intake's rule: `by` bound to the pusher,
   never the submitter (§6.8). A push of the same field by the approver
   is the same commit.
3. **The claim.** Silo places the task on `eu`. The attestation's
   `granted_capabilities` carries the ref, the digest and `approved_by`.
4. **The check.** `eu` resolves the chain and recomputes the digest,
   which matches. It checks that the approver is a principal the
   approval names. That rests on Silo's claim, the accepted risk of
   §12.8, or on a signed approval commit where `eu` requires one. `eu`'s
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
`intake:tasks/2026-10-16-reject-dangling-refs.task.md` on `eu` has
grain's `pkg/daemon/` at `candidate` to read, entry 2 (§10.12). That
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
   {"at": "2026-10-16T18:02:11Z", "grain": "eu", "run": "hub/2026-10-16-reject-dangling-refs.exec/runs/20261016T180000Z/run.md",
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

### 10.14 How the task interface reduces

| hub-interface.md today | With grain repos |
| --- | --- |
| `hello` / `welcome` / `heartbeat` / `goodbye` | unchanged (the registry and slots stay live) |
| `offer` (task + attestation) | Silo's commit, as the placing user, of an execution folder (`exec.md` with `attestation`, and the snapshot) in the Grain's `hub/` lane + `{"type":"pull"}` |
| `resume` | gone: the execution is in the Grain's own repo, and a returning Grain pulls |
| `accept` / `refuse` | the Grain's commit of `runs/<run>/run.md` in `accepted` / `refused` + `refusal` |
| `result` | the Grain's commit of the run's `done\|failed` + `closed`, and the outcome transition on `exec.md` |
| `cancel` | Silo's commit, as the user, of a `cancelled`-category state + `cancel` on `exec.md`, + `pull`; the run's `cancelled` commit |
| re-placement (today a `cancel` to one Grain and an `offer` to another) | a new execution with `previous:` on the new Grain, a `withdraw` to the old one, and the old Grain's `superseded` commit; the new execution is fenced until then |
| a user's reply to a running agent | relayed over the node connection; the Grain commits it in the run's folder |
| `propose_task` (upward filing) | unchanged as a request; Silo makes the intake commit |
| event stream, view stream, http stream | unchanged (runtime; never in git) |

The contract becomes:

- the repo schema (§4, §2.4);
- `grain.config.md` and its typed roots, the kind suffixes, the
  resolver's listing rules (`ignore:`, `archive/`) and lanes (§4.2–§4.3,
  §6.2);
- the layout of Silo's folder for a Grain: `backlog/` and `triggers/`;
- triggers and their firings, and gates (§6.6, §6.7);
- a task's `context:`, its resolution and limits, and the sandbox layout
  (§7);
- capability grainfiles, their chains and reducers, the base types a Grain
  implements and its published base declarations (§6.8);
- the `pull` notification.

A customised Grain conforms by reading and writing those files. Until
stage 4 of §15, both paths run, and the WebSocket messages are derived
from the commits.

## 11. Resolution and performance

A Grain's cost should grow with what it acts on, not with what its repos
hold. Discovery reads git trees only, parses front matter lazily, caches
by content hash, fetches sparsely, and fails loudly at its limits.

### 11.1 The resolver: tree-only listing, lazy parsing, references

**Reachability, never scanning.** A Grain's **active set** is exactly
the grainfiles its root entries activate. Referenced grainfiles are resolved on
demand, one at a time. A file under neither is never read, so a repo may
hold any number of tasks, designs and archived runs without costing a
Grain anything until something points at them.

**Listing an entry reads trees only.** To list an entry, the resolver
reads the git **tree** of the entry's folder and its subtrees, at the
commit the entry names, and nothing else: no ancestor folder, no blob.
The kind of each file is in its name (§2.1), so filtering by type, by
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
    (repo, folder, commit) = resolve_location(e.path, e.ref_mode, C's repo and commit)  # §3
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
| lane ranking | every `exec.md` in the lane | `rank`, `priority`, `expedite`, `state` |
| dispatch the next pick | that `exec.md`, then its snapshot | `state`, `depends_on`, `previous`, `attestation`; the snapshot's `context`, `constraints` |
| dependency check (§10.8) | each `depends_on` target's `exec.md` | `state` |
| policy checks before fetching (§7) | the snapshot, each `context:` entry's target, each capability chain | `context`, the capability grainfiles' `base` and `reducers` |
| register or re-register triggers | every trigger in the trigger roots | `on`, `enabled`, `limits` (the `fires` body is read when it fires) |
| fire a trigger | the trigger, then its template | `fires`; the template's `inputs` and body |
| give memory to a run | every memory document in the memory roots | `applies_to`, then the body of those that apply |
| render a prompt | the task, its template and partials | bodies |
| resolve a reference | the target, and any tombstone on the way | `kind`, `to` (a tombstone), `moved_to` (a repo's `.grainconfig`) |

**Reference resolution is one function**, shared by every consumer that
names another grainfile: an execution's `definition:`, `context:` entries,
capability grainfiles and their bases, a trigger's or task's template,
template partials, `depends_on`, `implements`, `derived_from`.

```
resolve_reference(ref, from, expected_kinds):
    T = resolve(ref, from, reader's credential, ALLOW)   # §7: the one resolver
    if T is dangling: report; return DANGLING            # never widened
    check kind(T) in expected_kinds, else WRONG_KIND     # from the suffix: no blob read
    return T                                             # parsed only when the caller needs fields
```

Resolving is a **read** with the reader's own credential (§12.18). It
never adds anything to the active set: a task's template, a capability
definition and a design are read when needed and are never fired,
ranked or run on their own.

**An entry the Grain cannot read is unknown, not empty.** While a
trigger or memory folder is unreadable or over a limit, the Grain fires
or gives nothing new from it and keeps its last good listing for
display. Lanes are in the Grain's own repo, so they are always readable;
only the Grain itself withdraws an execution (§10.7).

**Limits fail loudly.** An entry over `objects_per_entry`, a config over
`entries`, or an active set over `objects` is never truncated, because a
truncated listing would silently choose which triggers fire. The entry
(or, for the total, every entry listed after the limit was crossed) is
`UNKNOWN`: the Grain shows the error in its status and Silo's health
view, alerts the owner, and the forge check refuses a push that would take a
folder it hosts over the limit of any root entry it knows names it. A
`exec.md`, `*.trigger.md` or `*.memory.md` over `bytes_per_object` is
refused by the forge check and skipped and reported by readers.

### 11.2 Caches and sparse fetch

**Steady-state cost is O(changes).** Two content-addressed caches make it
so:

- **The listing cache**: a resolved entry's listing, keyed by **the
  entry folder's tree hash**, the root type, the entry's normalised
  `ignore:` and the resolver version. A tree hash covers its whole
  subtree, so an unchanged folder hits the cache whatever happened
  elsewhere in the repo.
- **The front-matter cache**: parsed front matter, keyed by **blob
  hash** and schema version. Bodies are cached separately and only for
  the grainfiles that needed them (templates, memory, prompts being
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
the same folder (the forge check updates the listing cache for the folders a
push changed, so Silo's views and its orphan check are warm). Nothing is
cached in git, and losing either cache costs one cold start.

**Sparse fetch.** A Grain clones nothing whole. For each repo its entries
name it keeps a partial clone (`--filter=blob:none`, and
`--filter=tree:0` where the host supports it) and fetches:

- the **trees** along each entry's path and under its folder, at the
  entry's commit;
- **blobs on demand**, only when an operation in the table of §11.1
  parses them;
- **referenced grainfiles individually**, when a reference resolves (a
  task's file, its template, a design), as single blobs at the named
  commit, with the trees along their path.

A `pull` notification from Silo ("repo moved to `<sha>`") triggers a
fetch of the new commit's trees for the entries in that repo, and nothing
else.

**Estimates: a Grain with 10k tasks across 5 repos.** Say the 10k tasks
are in two intakes and three code repos, of which 400 are open and
placed or queued; the Grain has 3 lanes (its own 50 executions, the
hub lane's 300, a colleague's 50), 3 trigger entries (60 triggers), 2
memory entries (120 documents), and 4,000 closed executions under its
lanes' `archive/` plus 200 live.

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

### 11.3 The orphan check

A `*.<kind>.md` file that no root activates and nothing references does
nothing, and is usually a mistake: a trigger committed to the wrong
folder, a reference left in a folder no Grain lists. `grain lint` (and,
optionally, the forge check) reports it.

**Known**, for Silo, means:

- every **root entry** in a `grain.config.md` Silo knows about: the
  roots of Grains linked to it, and any Grain root it hosts;
- every grainfile one of those entries activates, and every grainfile
  **reachable by references** from them, transitively, under the
  resolver's limits: tasks from refs, templates from triggers and tasks,
  partials, designs from `implements:` and from `design-change`
  triggers' `paths`, `context:` targets, capability grainfiles and their
  bases, `depends_on` targets, and the targets of tombstones;
- a repo's own `grain.config.md`, `.grainconfig` and `GRAINOWNERS`, its
  gates under its grain folder (§6.7), every definition in a team's
  intake (proposals and unplaced work), the partials a known template
  includes, and everything inside a known definition or execution
  folder (§5).

For `grain lint` on a single repo, known means the same with only the
`grain.config.md` files in that repo and the ones its `--config` flags
name.

A file under `archive/` is never an orphan. An orphan is a **warning**
in lint and in Silo's health view, unless the repo's `.grainconfig` sets
`orphans: refuse`, in which case the forge check refuses a push that creates
one. A definition is never an orphan: definitions are inert, like
templates, and are reached only by an execution's `definition` (spec
§10.2.5).

## 12. Security

This adds a boundary to [threat-model.md](threat-model.md): **pusher →
Silo git host**. It also changes TB3 (hub → Grain) from WebSocket
messages to commits plus notifications.

1. **The enforcement point is the forge check** (§8.3). It runs in
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
   not authorized for. Silo grants a Grain read on exactly the repos its
   executions need: each definition's repo (for its thread), the repos
   the snapshot references (template, template includes, designs), and
   the Grain roots holding its dependencies' executions (§10.8), for as
   long as the execution is open. Listing a root entry is a read like any other (item 18). Silo cannot grant read on a code repo it does not host:
   a Grain reads a task or definition there with its own credential, as
   it must to work on that repo anyway. The forge check checks new references

   with the pusher's rights, so nobody can use a reference to make Silo
   reveal a repo the pusher cannot read. Included files render only
   under the same check.
5. **Template injection.**
   - Inputs are data. Rendered output is never rendered again.
   - The subset has no access beyond inputs (§6.5).
   - Only `title` and the body are rendered, so a template can never
     render itself capabilities, `reads` or approvals.
   - What remains is prompt injection into the agent. That is the
     existing TB7 concern, bounded by the Grain's local policy.
6. **Secrets are never stored.**
   - A capability grainfile's `secrets: {token: ghcr/publish-token}`, a
     workflow's `capabilities: [gcp-key, repo-write:bwsalmon/grain]` and
     `secret: github-app/app-id` are **names**. They resolve only in the
     Grain's local policy and secret store, or in Silo's grant table
     (§6.8, item 33).
   - A capability in a task file is a **request**, a `use` entry in its
     `context:`. Its grant is Silo's claim, carried in the attestation's
     `granted_capabilities` with the digest of its resolved chain, and the
     Grain's local policy has the last word (items 27–35).
   - The forge check rejects files that match secret patterns.
7. **How a Grain verifies a file came from Silo.**
   - It fetches over TLS from the URL its own config names.
   - An execution's `exec.md` carries Silo's signed `attestation`
     (hub.md §6.4). Its `aud` is this Grain, its `task` is the
     execution's canonical global identity, and its `spec_digest` covers
     the snapshot, which never changes, so the digest stays true for the
     execution's whole life. A Grain whose policy names `silo_issuer`
     refuses an execution in the hub lane without a valid one.
   - **A redirect cannot change what was placed.** If the task has moved
     since, the Grain follows the tombstone and recomputes the digest on
     what it finds. A spec that no longer matches is refused, so a
     tombstone can only ever lead to the same task.
   - Any later change made through Silo (an execution's `cancel`, a
     re-placement's withdrawal) is accepted only if it is signed or
     attested the same way, or authorised by the forge check in the
     Grain's own repo.
8. **The accepted risk, and the upgrade.** Today a Grain trusts Silo's
   claims (hub.md §1.2). With grain repos it also trusts the forge check to
   have checked who pushed. **Signed commits are the natural upgrade.**
   - The repo's `.grainconfig` lists each principal's SSH or GPG
     signing keys.
   - Approvers sign their approval commits.
   - Grains verify that an execution's `approved_by` change is in a
     commit signed by that approver's key.

   That makes approval verifiable end to end, with no need to trust Silo,
   which the JWT alone cannot do. It also covers repos on other hosts.
9. **A rogue writer** can only do what its rules allow. Examples:
   - a compromised Grain can change only what its root's `GRAINOWNERS`
     gives it: its executions' `runs/`, their outcome transitions and
     `superseded`, and its memory. Its owner's `grain.config.md` is not
     the Grain's to write (§8.2). It cannot write any definition, and it
     cannot change a snapshot (the forge check refuses it), so it cannot
     rewrite what it was asked to do. It cannot create an execution in
     the hub lane, so it cannot claim it was asked, and it cannot write
     another Grain's executions, so it cannot claim another Grain's work;
   - Silo, or a credential for it, writes only as the users it acts
     for: it can create, rank, approve and cancel executions only as far
     as those users' own rules allow, and it can never write runs,
     forge outcomes, or make anything active on a Grain beyond the lanes
     and triggers that Grain's config names;
   - a compromised member can create and reorder executions in the hub
     lanes the team may write, and nothing else;
   - a compromised approver can approve, and the commit names them.

   Every write is an attributable commit, and the forge check logs every refusal
   to the audit log.
10. **A malicious design edit** can file at most a capped number of
    *proposed* tasks, which still need approval (§6.9), through the
    `design-change` triggers that watch it, under the flood limits of
    item 36. Approval itself
    can only be given by an approver other than the task's submitter.
11. **Content is untrusted where it is displayed.** Silo renders Markdown
    from these files as it renders agent-written text today (TB4).
12. **Code repos are trusted as their merge process is** (§9.6). This
    adds no boundary to the threat model: a code repo's `default_ref` is
    already trusted for the code an agent works on. What is new is that
    Silo and Grains read grainfiles from it. The model:
    - the forge check does not run there, so nothing in a grain folder is
      attributed to a pusher. A grainfile is as trusted as the repo's
      review made it, and no more.
    - Silo validates every grainfile it reads from a code repo (schema,
      paths, references, ID allocation, and that it holds no
      executions) and uses none that fails.
    - The design's limits do not depend on the repo's `GRAINOWNERS` or
      `.grainconfig`, which anyone who can merge may edit (§9.6).
    - Grains apply local policy as PEPs. An execution's snapshot must
      match the attestation's digest, and a Grain's policy may refuse code
      repos it does not trust for templates, triggers or designs.
    - **Disclosure.** A definition homed in a code repo is readable by
      everyone who can read that repo. A team with a private intake keeps
      its prompts out of a public repo by not writing them there (§9.2);
      executions, with their snapshots, are only ever in Grains' roots.
13. **A malicious `.grain/` change in a code repo**, merged by someone
    who can get a change through that repo's review, can do no more than
    this:
    - **Edit a homed definition after it was placed.** Every execution
      runs from its own snapshot, so nothing that runs changes; lint
      reports the definition as edited after a snapshot (GF046), and the
      commit that did it is in the repo's history. New placements copy
      the edited text, and their approvers read it.
    - **Add a definition and claim it approved.** A definition holds no
      state, and the schema refuses `approved_by` (GF020): approval is
      only ever on an execution, in a Grain's root, under that root's
      `GRAINOWNERS`.
    - **Forge `moved_from`** to claim an intake origin. It gains
      nothing: provenance decides nothing about what runs, and Silo
      shows the claim with whether the other end is a tombstone pointing
      back.
    - **Edit a template or trigger.** An execution already created pins
      its `definition`'s commit. A trigger renders its template's tip, so
      a firing after the edit uses it. That firing is created in its
      `initial` state, and an
      approver reads the rendered prompt, unless the trigger root that
      names it is pinned to a reviewed commit: then an edit does not reach
      the team at all until an admin re-pins it. What remains is prompt injection, bounded by
      local policy (item 5).
    - **Grant itself access.** A template, trigger, gate or suite
      with `reads`, `writes`, `grants` or `target`, or with a `context:`
      entry or `ref` input naming another repo, is refused (§9.3). A
      capability named in a homed definition is a request, granted only
      in Silo's attestation on an execution (item 6). A capability grainfile it adds under
      `.grain/` is honoured by no Grain whose policy does not sanction
      that folder as a source (item 30).
    - **Flood a team.** A trigger naming a team the team has not adopted
      never fires (§6.6), and an adopted one is rate-limited (item 36). Firings never land in the code repo itself. A
      design edit files at most a capped number of proposals (item 10).
      Silo reads at most 1,000 changed files per code-repo push, as the
      hook caps pushes (§8.6).
    - **Widen its own rules.** It can edit its `GRAINOWNERS`, which is
      advisory there. It cannot make anything active on a Grain: only
      Grains' configs do that, and it cannot hold an execution (GF047).
    - **Reference a private Silo repo** to make Silo reveal it. Silo
      resolves code-repo references with the rights of the principal
      using them, never its own (item 4).
    - **Weaken its own gates.** A change that edits or deletes a gate
      is held by the gates at the target branch's tip, which still
      include the one it edits (§6.7). Once merged through them, the
      edit is the repo's own policy, as any merged code is. It cannot
      weaken a mandatory gate source, which the Grain reads itself.

    The repo's own defence is to review `.grain/**` as strictly as it
    reviews code that touches credentials, which bwsalmon/silo already
    does with a path trigger that requires a human (`grain-declarations`),
    a gate in this design (§6.7).
14. **A root entry is a trust grant, and only the Grain's owner makes
    one.** Adding an entry to `grain.config.md` says "act on what this
    folder holds, of this type". **Nothing Silo writes can activate
    anything on a Grain unless the Grain's owner pointed a root at it**:
    Silo can create executions only in a lane the owner declared and
    `GRAINOWNERS` opened to the team, a folder no entry names is never
    listed, and `GRAINOWNERS` keeps `grain.config.md` the owner's, never
    Silo's (§8.2). That is the PEP property of §1, and it
    holds for every root type:
    - **The type narrows it.** A backlog entry lists `<name>.exec/`
      folders only, so what Silo creates in a lane can only ever be
      executions, whatever else that folder holds; a trigger entry lists
      `*.trigger.md` only, so a team's
      triggers folder cannot bring a gate, a capability or a reference.
    - **References are not grants.** A task's template, a capability
      definition or a design is read when something already active needs
      it, with the reader's rights, and makes nothing active (§4.1). A
      foreign template is still data (§8.4).
15. **Nothing outside an entry can change what it activates.** An entry
    activates its folder's subtree, less its own `ignore:` and any
    `archive/`. Nothing is inherited from other files, so
    no file in a parent folder, in another repo, or written by another
    principal can hide or add a grainfile for that entry. A producer can
    change its folder's contents (that is its right: the entry trusts
    that folder), and nothing else can.
16. **The owner's `ignore:` is the final word for their entries.** A
    Grain can cut any grainfile a root would activate, and no producer can
    undo the cut except by renaming the grainfile, which the owner's
    status shows as a new arrival. Gates are not cut this way, because
    they are not in any root (§6.7, item 39).
17. **Listing is bounded and fails loudly.** An attacker who controls a
    folder a Grain names can fill it, but cannot make the Grain read more
    than `objects_per_entry` names from it, or more than `objects` in
    total: over a limit the entry is unknown, and nothing is truncated,
    so the attacker cannot choose which of its grainfiles survive (§11.1).
    Entries are not transitive, so there is no chain to bound and no
    cycle to cut. References are bounded per resolution (8 hops, cycle
    detection) and by `context:`'s limits (§7).
18. **Listing and resolving are reads, with the reader's own
    credential.** A Grain lists its entries and resolves references with
    its node credential, Silo with the rights of the principal it acts
    for, and nobody with Silo's own. An entry naming a repo the reader
    cannot read is unknown, not an error that widens anything (§11.1).
    The forge check checks a new entry's folder with the pusher's rights, so
    `grain.config.md` cannot be used to make Silo reveal a repo, and
    Silo lists a Grain's entries for its views with the Grain's grants,
    not its own.
19. **`context:` is the sandbox's whole world.** A run sees its
    snapshot, its run record and the replies relayed to it, and the
    entries of `context:`, and
    nothing else: no other repo, no other file, no reference that is not
    an entry (§7). Every other reference field is metadata, so a task
    cannot reach a repo by depending on a task there, implementing a
    design there, or using a template from there. The list is part of the
    spec: the approver reads it, the forge check freezes it on approval (§6.1),
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
    it, and before anything enters the sandbox (§7). A listed entry it
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
    declared (§7). Checkouts are capped at `checkout_bytes` as inflated
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
    read on the protected branches an execution's snapshot's `context:`
    names, for as long as the execution is open, and no more. A repo on another host is
    read with the Grain's own credential, which its policy already
    governs.
27. **Monotonicity is the core invariant.** A capability's effective
    authority is its base's maximum on this Grain, intersected with every
    reducer in its chain and in the task's entry (§6.8). Nothing in git
    adds authority. Only two things are sources of it: **base
    capabilities**, which are code the Grain runs, and **secrets**, which
    are the Grain's own and which policy allowlists by name (item 33).
    - The schema gives a capability grainfile no field that grants
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
    upstream owner can widen their own grainfile later, as a template's
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
    Silo's catalog is a view for authors (§6.8). A grant needs all of
    these: a `use` entry in the execution's snapshot, which never
    changes; Silo's claim for the execution; and the Grain's policy
    admitting the base, every source
    in the chain, every secret and every implication.
30. **Definition sources are allowlisted, for every link.** The Grain's
    policy lists the repos and folders whose capability grainfiles it
    honours, and for which bases (`capabilities.sources`). Only the
    Grain's own root is sanctioned by default. Every link of a chain
    must pass, checked before its repo is read, so a sanctioned folder
    cannot launder a definition from an unsanctioned one (§10.13). A code
    repo's grain folder and a team's intake are not sources unless the
    owner says so. A compromised shared repo can only
    redefine the capabilities it was sanctioned to define, within the
    bases it was sanctioned for, and every such change alters the digests
    of the chains that use it (item 28).
31. **Implications are declared and checked.** A base's declaration names
    the repos, egress and tools a use reaches, as narrowed by the
    capability. The policy checks each one with `repos:`, `network` and
    `tools` before anything is minted or materialised, and refuses
    closed, naming the implication. This replaces `the hard-coded repo grants (`model.GrainSourceRepo()`)`,
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
    Capability grainfiles name secret slots' bindings, and never hold
    material. The hook's secret-pattern scan (§8.3) applies to them as to
    every file. The Grain resolves the names from its own store when it
    materialises, into the file its base declares, and nowhere else: not
    git, not `run.md`, not events, not Silo. Because a secret is
    authority the base's maximum does not bound, the policy allowlists
    the names each source may bind. A missing secret refuses the run with
    the name, never part of it (§6.8).
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
    the Grain's proxies, on every request, so the properties §6.8
    requires of Anselm are security properties: no side effects, no
    network or filesystem, determinism, a size limit and an evaluation
    budget whose exhaustion denies. A hostile reducer can therefore only
    deny, which a run's author can already do by not requesting the
    capability. Its only cost is the budget, which the Grain sets.

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
      (§6.9); per Grain, the same caps over its own root, set in its
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
    - in a **Grain root**, only the Grain and its owner (§8.2); it fires
      on that Grain only;
    - in a **shared repo**, the team's maintainers, or a review branch an
      approver merged (§8.5); a `fires_in: silo` trigger fires only for
      the team the repo belongs to, and a `fires_in: grain` one only on
      a Grain whose trigger root names its folder (Silo's per-Grain
      folder, for an assigned trigger);
    - in an **intake**, only the team's admins, as the team's own
      triggers or as entries in the intake's `grain.config.md`
      (adoption);
    - in a **code repo**, whoever its review lets merge (§9.6); it
      fires only where a trigger root names it, and its firings never
      land in the code repo (§6.6).

    `push:` and `merge:` may watch only protected refs, so a contributor
    cannot fire a trigger by pushing a branch of their own. A firing
    caused by a person (a push, a merge, a `manual:` request) is
    approved no further than that person could approve (§6.6), so a
    trigger with `auto_approve_proposals` does not turn a non-approver's
    push into approved work. And a trigger grants nothing: its firing is
    a task, held to the Grain's policy like any other (items 21, 29).
38. **A gate is policy of the repo it guards, and no Grain can weaken
    it.** Gates combine by conjunction: there is no negative gate, and no
    key in any grainfile releases another's hold (§6.7). A gate is never in
    a Grain's roots, so no entry, no `ignore:` and no `archive/` move in
    someone else's folder adds or removes one; the merge queue reads the
    gates in the merged-into repo's own grain folder, at the target
    branch's tip, so the change it holds cannot edit them away. And a
    trigger never holds or releases a merge: the two kinds share no
    keys, and their schemas refuse each other's.
39. **Mandatory gate sources are read directly.** The Grain's local
    policy (and Silo's, for the repos it hosts) may name mandatory gate
    sources, which the merge queue and the forge check read themselves; a
    mandatory source that cannot be read holds every change it might
    cover. So an org's "every `**/*.tf` change gets a security review"
    holds wherever it is mandatory, whatever any Grain's config says.
40. **A gate's review tasks are bounded too.** A gate files at most the
    tasks its `requires` names, once per change per gate (their paths
    are derived from the change and the gate, so a re-push files no
    more), and they count against item 36's caps. A gate that cannot
    file its review, because a cap is reached or its template does not
    resolve, holds the change rather than releasing it.

## 13. Migration from today's grain

`grain tasks export` writes grain's database into grainfiles:

- tasks, as definitions plus one execution each, the comments in the
  execution's thread;
- the backlog's order, as executions' ranks;
- each dispatched task's runs, as run folders in its execution;
- schedules, as triggers, and templates, from the database or from
  grain/config `settings/*.yml` (§13.1 maps the fields).

**Rules:**

- **Stable paths, derived from the old id and a slug.** A task becomes
  `tasks/<legacy number>-<slug>.task.md`, such as
  `tasks/4310-carry-approver.task.md`. The slug is the title, lower-cased,
  with every run of other characters turned into `-`, cut to its first
  few words and at most 40 characters. Templates keep their declared
  names (`templates/prune-packages.template.md`), and a schedule's becomes its
  trigger's (`triggers/prune-packages.trigger.md`). An export run twice
  therefore produces the same paths. The legacy id goes in `aka:`.
- **Where tasks go.** A standalone Grain exports everything into its own
  root. Each grain task becomes **a definition plus one execution**
  (§6.1): the prompt and spec the definition, and the state, the comment
  thread and the runs the execution, with the definition copied as its
  snapshot. For a Grain linked to Silo, the definitions of tasks
  submitted at the hub are exported into the team's intake from Silo's
  database, and their executions into the Grain's `hub/` lane; the tasks
  the Grain authored itself are both in its root, the execution in its
  own lane. `grain tasks export` writes the root's `grain.config.md`,
  with the `hub/` lane, as `grain tasks link` does (§10.9). Grain's memory documents and `memory.json` are exported to
  the root's `memory/` as `*.memory.md` files (§4.2).
- **The old backlog order becomes ranks.** The queued tasks, in the old
  dispatch order, each get a reference in `backlog/` with evenly spaced
  ranks (`"1"`, `"2"`, … `"z"`, lengthening as needed). `at_front`
  becomes `expedite: true`, and the old priority becomes the `priority`
  band.
- **Old runs become run folders.** Each task the Grain dispatched gets
  one execution `<lane>/<legacy number>-<slug>.exec/`, and each attempt
  one `runs/<utc>/run.md` in it:
  - an attempt's fine detail stays in the database;
  - `accepted_at` and `started_at` come from the first attempt;
  - `state` and `closed` come from the final attempt, the pull request
    and the merge commit.

  A task that was re-placed between Grains gets one execution per
  Grain, linked by `previous:`, the earlier ones `superseded`. grain's
  one thread per task (`Comment.TaskID`) goes to the execution's
  `thread/`, except an agent's comments, which go in the folder of the
  run they were made during; a task never dispatched gets a definition
  folder with the thread instead.
- **Runtime columns are dropped.** `next-run-at`, `last-run-at`, a
  schedule's paused state and the attempt rows are not exported; they
  stay in the database (§6.6).
- **Templates.** A template's `fields` (`model.TemplateField`) become
  `inputs`.
- **Target, reads and writes become `context:`** (§7). For a task, in
  this order:

  | grain today (`model.Task`) | `context:` entry |
  | --- | --- |
  | `Target: owner/name`, `Base: b` | `{ref: "<forge URL of owner/name>@b", mode: write}`, first, so it is the primary repo |
  | each `Writes[i]: {repo, base}` | `{ref: "<URL>@<base, or Base>", mode: write}` |
  | each `Reads[i]: owner/name` | `{ref: "<URL>", mode: read}`, unpinned |
  | a `repo-write:owner/name` capability | a `write` entry for that repo, if none exists, and the capability is dropped: a repo reaches a sandbox only as an entry, or as a declared implication (§6.8) |
  | each other capability name (`gcp-key`, …) | a `use` entry for that name's legacy capability grainfile (§13.2) |
  | each attachment | an asset beside the task and a `read` entry for it (§2.3) |
  | no `Target` (team-targeted) | no `write` entry |

  `<forge URL>` is the deployment's configured forge,
  `https://github.com/owner/name.git` for these deployments. Pasted
  context stays in the prompt, where it was; an author who wants it as a
  file attaches it instead.
- **Templates and schedules** (which become triggers) with `repo` and `base` get an input `repo`
  of type `ref`, defaulting to that repo at that base, and
  `context: [{input: repo, mode: write}]`; a template's `reads` become
  `read` entries. A schedule's `target` and `base` become its trigger's
  `fires.inputs.repo` (§10.12).
- **Attachments** on tasks and comments become assets beside them, as
  §2.3 describes.

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
[`hub-shared/triggers/prune-packages.trigger.md`](../examples/grainfiles/README.md),
by the mapping in §13.1: `weekly` and `overlap` under `on.schedule`,
`template` as `fires.template`, `target` and `base` as
`fires.inputs.repo`, and the runtime columns dropped.
The template exports as `templates/prune-packages.template.md`. The examples then
move it to
[`hub-shared/templates/ghcr/prune-packages.template.md`](../examples/grainfiles/README.md),
leaving a tombstone at its old path, to show one (§2.2). The
settings-owned `template-1`/`sched-1` pair, which has `reads`, is
[`templates/granule-pull.template.md`](../examples/grainfiles/README.md).
Importing them yields the same rows minus the runtime columns, which the
scheduler recomputes from the cadence.

### 13.1 Existing `.grain/*.yml` declarations

Code repos move from `.grain/*.yml` to `.grain/*.md` one repo at a time,
and **both forms stay readable throughout**:

1. **Readers accept both.** `pkg/repodecl` loads `templates/`,
   `schedules/`, `suites/` and `triggers/` as `.yml`, and `templates/`
   (folder templates included), `triggers/` and `gates/` as `.md`, using the `grainfile`
   parser for `.md`. A `.yml` schedule and the `.md` trigger converted
   from it, or a `.yml` gate trigger and its `.md` gate, are one name. If one name exists in both forms, the
   `.yml` is used and the conflict is reported by
   `grain declarations check`. So a half-finished conversion changes
   nothing that runs.
2. **A repo with no `.grainconfig`** reads with the defaults (§8.1), and
   its `.grain/` stays its grain folder. A repo adds `.grainconfig` when
   it wants aliases or another grain folder, and
   `GRAINOWNERS` when it wants its ownership documented.
3. **`grain declarations convert`** rewrites a repo's declarations in one
   commit, which lands through the repo's normal review (a `.grain/**`
   change, so human-gated where the repo says so). For each file it
   writes the `.md` with the same keys, moves the leading comment into
   the body (and a template's prompt into the body), maps each schedule
   and path trigger to a `kind: trigger` or `kind: gate` by the tables
   below, turns template names into references, and deletes the `.yml`
   in the same commit. **No tombstones are needed:** nothing references a
   declaration by path today, only by the name id, which does not change
   (§9.3).
4. **bwsalmon/grain and bwsalmon/silo convert first.** Then
   `grain declarations check` warns on `.yml` templates, schedules,
   suites and triggers, and refuses a `.md` whose `kind:` is not one of
   this design's kinds. It keeps loading them until a `v2` of the
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
| `base: <branch>`, with `target: owner/name` on a settings row | for a template: `fires.inputs.repo: "<forge URL of target, or of the declaring repo>@<branch>"` (§7); for a workflow: `fires.branch: <branch>` |
| `daily: {at: "09:00"}` | `on.schedule.daily.at: "09:00"`; likewise `weekly: {day, at}`, `every: {hours}` and `monthly: {day, at}`, verbatim under `on.schedule` (grain has no `hourly` or `cron` key; `cron` is new to triggers) |
| `timezone` (parsed by `pkg/repodecl`; schedules otherwise run on the deployment's `Config.TimeZone`) | `on.schedule.timezone`, defaulting to the deployment's zone on conversion |
| `overlap: skip` (the default; grain fires a held occurrence once its predecessor closes) or `allow` | `on.schedule.overlap`, the same value |
| — (no key today: grain's `skip` already folds missed occurrences into one late firing) | `on.schedule.catch_up: latest-only`, written explicitly so the behaviour is reviewable |
| — | `fires_in`: `grain` for a repo's own declaration, which its Grain fires as today; for an exported settings row, `silo` if the team's Silo fires it (`prune-packages`), `grain` if one Grain keeps it (`sched-1`) |
| `next-run-at`, `last-run-at`, paused | dropped: runtime, kept in the database |

A workflow's own `on.schedule` cron is not converted: it stays in the
workflow, and grain reads it as an implicit trigger (§9.4).

**Path triggers become triggers or gates.** A `.grain/triggers/<name>.yml`
is one or the other, decided by `gate:`:

| grain today | `gate: true` → `gates/<name>.md` | otherwise → `triggers/<name>.md` |
| --- | --- | --- |
| `on: pull_request` | implied: a gate holds changes on their way in; no key | — |
| `on: merge` | — | `on.merge` (or, for the design-doc planner, `on.design-change`, §6.9) |
| `paths`, `ignore` (and `branches`, where given) | `match.paths`, `match.paths_ignore` (`match.branches`) | `on.merge.paths`, `on.merge.ignore` (`on.merge.branches`) |
| `gate: true` | the kind itself | — |
| `human: required` | `requires.human: {when: always}` | refused: a trigger never holds a merge |
| `human: on-findings` | `requires.human: {when: on-findings}` | refused |
| `sensitive: true` | `sensitive: true`, plus `requires.security_review: {}` written out | refused |
| its template and `{{group}}`, `{{from_sha}}`, `{{to_sha}}`, `{{paths}}`, `{{branch}}`; `per: change \| folder`; `auto-approve` | — | `fires.template`, with `fires.bind` from the event; `on.merge.group_by: folder` for `per: folder`; `auto_approve_proposals` |
| its name (`repo:trigger:…`) | `aka:` | `aka:` |

A `.yml` trigger that both gates and files work becomes two files, one of
each kind. bwsalmon/silo's four path triggers (`grain-declarations`,
`hub-contract-docs`, `hub-deploy`, `hub-trust-boundary`) are all gates;
`grain-declarations` is in the examples.

Workflows are never converted (§9.4). Design docs gain front matter
whenever their owners choose (§9.5); until then they are referenceable
as plain files.

### 13.2 Capabilities

Today's capability names become base capabilities, and capability
grainfiles over them, with nothing about what a run gets changing on the
day of the switch:

| grain today | Becomes | Declared implications |
| --- | --- | --- |
| `gcp-key` (a key from a pool of service accounts, `~/.gcp-service-account.json`) | base `gcp-service-account@v1`, at first in its **legacy key mode**: the same key file, which cannot narrow `token_lifetime`, so a reducer on it is refused. The token-endpoint mode of §6.8 follows | egress to Google's APIs |
| `gemini-key` (`~/.gemini-api-key`) | a grainfile over base `api-key@v1` (a secret materialised as a file), binding the deployment's Gemini secret and `subset hosts [generativelanguage.googleapis.com]`; later `model-api@v1` through the model proxy | egress to that host |
| `github-sandbox` (`~/.github-sandbox-repo`, `~/.github-sandbox-token`) | base `github-sandbox-repo@v1` | egress to `github.com`, `api.github.com`; the throwaway repo |
| `self-debug` | base `self-debug@v1`: `read_grain_source`, `list_grain_source` and the Grain's log tools | `repos: [https://github.com/bwsalmon/grain.git]`, fetch only: task 3204's grant, until now a `the hard-coded repo grants (`model.GrainSourceRepo()`)` entry |
| `grain-ci` | base `grain-ci@v1`: the CI tools (`wait_for_checks`, `retrigger_ci`, …) | the Grain's own CI service; no egress |
| `package-publish` | base `github-packages@v1`, with a secret slot `token`, marked `approval: required` | egress to `ghcr.io`, `api.github.com` |
| `repo-read:o/n`, `repo-write:o/n` | in a task, a `context:` entry (§13); in a workflow, `git@v1` with built-in reducers (§6.8) | `repos: {from: repos}` |
| `the hard-coded repo grants (`model.GrainSourceRepo()`)` | each base's declared `implies.repos` | |
| `docs/ci-capabilities.md` | the base declarations, generated from the providers, which the doc then indexes | |
| escalated capabilities | `approval: required` in the base's declaration, or `approval.local` in the Grain's policy | |

- **Legacy grainfiles.** For each name, `grain tasks export` writes one
  capability grainfile with no reducers over that name's base. It writes them
  in the Grain's root at `capabilities/legacy/<name>.md`, and, for a
  linked Grain, in the team's shared repo
  (`shared:capabilities/legacy/<name>.md`), so that an intake task
  requests a team-level definition every Grain resolves against its own
  base. Each carries the old name in `aka:`. A workflow's bare legacy
  name resolves to the Grain's own legacy grainfile.
- **The policy that matches today.** Export also writes the matching
  policy: the bases the deployment provides, the Grain's root and the
  team's `capabilities/legacy/` as sanctioned sources, and the current
  rules for scheduled runs as policy over effective authority (§6.8,
  CI). A run of a migrated task gets what it got before, and narrowing
  becomes possible.
- **A task's `capabilities:`** becomes `use` entries after its other
  `context:` entries, in the old order. A template's `capabilities:`
  stays in the file, as §9.3 keeps declaration keys verbatim, and
  filing turns each name into a `use` entry in the task.
- **Silo's grant table** names capability grainfiles by reference instead of
  bare names, and its attestation's `granted_capabilities` gains each
  one's digest (§6.8).

## 14. Prior art, and what is borrowed

- **Backstage's software catalog.** Every entity is a YAML document with
  `apiVersion`, `kind`, `metadata.name` and `spec`, and `Location`
  entities point at more of them transitively. The design takes **a kind
  plus a spec and a body**, and **relations as references**. It does not
  take `Location`'s transitive discovery: an active set here is listed
  per typed root entry, and everything else is a reference. Nor does it
  take the envelope: `metadata.name` cannot be a path, references are
  resolved in a catalog database rather than by location, and the
  catalog does not read Markdown. An optional exporter (`silo catalog
  export`) emits Backstage-compatible YAML for a repo's grainfiles, one
  entity per grainfile with the source path in an annotation, and never
  reads it back. A task may name catalog entities as labels (`catalog:
  [component:default/payments]`); they activate nothing and grant no
  read.
- **CODEOWNERS** gives `GRAINOWNERS` its syntax, its nearest-file
  cascade and its last-match rule; the field suffix is the one addition.
- **gitignore** gives each entry's `ignore:` its pattern syntax, but not
  its cascade: patterns live on the entry, never in the folder.
- **Plan 9's per-process namespaces**: two processes see different
  views of the same global files. Typed roots keep that much (two Grains
  act on different sets of the same global grainfiles, and names never
  change by being activated), and nothing transitive.

## 15. Staged implementation plan

| Stage | silo (bwsalmon/silo) | grain (bwsalmon/grain) |
| --- | --- | --- |
| **1. Format** | a `grainfile` package: front-matter parse and write (field-preserving), JSON Schemas (`lifecycle`, `exec`, folder templates, `trigger` with its `on.*` sources and `fires`, `gate`, `config` in all three forms and `memory` included, from `schemas/grainfiles/`), the forge check (`grainlint --authorize`) as a required check, the kind-suffix rule, the reference grammar and resolver with redirects (tombstones, repo `moved_to`) and the archive mapping, `.grainconfig`, `GRAINOWNERS` evaluation (the field extension, and the forge check's fixed per-kind rules), the root-entry lister of §11.1 with `ignore:`, `archive/`, its limits and both caches, the path rules, containers (`.task/`, `.exec/`), folds and snapshots, rank keys, lanes and backlog ordering, and `grain lint` with the orphan check and the definition-edited warning; the `context:` resolver sharing `resolve` with references, with its limits and default layout, and the `asset` kind; the Jinja subset on gonja with the Python fixture CI job | pin `grainfile`; `grain tasks export/import` with the round-trip test on a copy of the database; `pkg/repodecl` and `grain declarations check` read `.md` declarations beside `.yml` (§13.1); `grain declarations convert`; convert bwsalmon/grain's own `.grain/` |
| **2. Standalone Grain on a grain repo** | none | a local root becomes the backlog's source of truth for git-backed fields, with the database as runtime and index: definitions, and executions in its lanes with their snapshots, threads and `runs/`; triggers from its trigger roots (`schedule:` and `manual:` sources, `fires` of both forms, overlap, catch-up and rate limits, with last and next fire, pause and suspension in the Grain's database); gates read from the merged-into repo's `.grain/` by the Grain's merge queue, with mandatory gate sources in local policy; `grain.config.md` with shared folders as trigger and memory roots; memory roots given to runs; sparse fetch; `context:` materialisation: the PEP before fetch, staging, the proxy's scope and push rule, the sandbox layout, and the run's `context:` record; attachments as assets |
| **3. Forge enforcement and the registry** | the forge check as a required check on each team's repos (protected default branch, direct pushes off), `GRAINOWNERS` → CODEOWNERS compilation, delegated user tokens and forge-API writes, the registry grain folder with webhook and polling refresh | reader verification where `enforcement.mode` is not `required-check`; the deployment's forge identity |
| **4. Placement by commit** | the dispatcher creates executions in Grains' `hub/` lanes as the placing user, ranks, approves and cancels them there, and re-places by a new execution with `previous:`; the definition → executions index; mapping definition dependencies to executions; showing an execution with both threads, labelled; the `pull` notify; WebSocket offers derived from commits behind a flag, then retired | accept, refuse and close as run folders, outcome transitions on `exec.md`, attestation on the execution, `withdraw` and `superseded`, the `previous` fence, upward filing through Silo; then drop `offer`/`resume` handling |
| **5. Shared definitions and planning** | templates and triggers fired in Silo (`schedule:`, `push:`, `merge:`, `manual:`), with their runtime state and rate limits in Silo's database; the design index, and the `design-change` source with its flood control; gates on protected branches (the forge check, Silo's merge queue and its trailers); the review-branch gate | render shared templates locally; `implements`/`closed.design_revs`; trailers from refs |
| **5b. Code repos** | the code-repo reader (webhook or poll) with validation on read (§9.6); trigger adoption; `push:`/`merge:`/`design-change` from code repos; designs planned in place by an in-repo `design-change` trigger; definitions and their threads in code repos, and moving a definition there with a tombstone | local policy for code repos a Grain trusts |

| **5c. Capabilities** | the `capability` and `capability-base` schemas; the chain resolver with cycle detection, digests and the built-in reducers in `grainfile`; Silo's catalog view, placement against Grains' published bases, `capability_approvals` on executions, and digests in `granted_capabilities`; the legacy grainfiles of §13.2 in each team's shared repo | providers declare their bases and the Grain publishes them; the policy's `capabilities` (bases, sources, secrets, `approval.local`) and the implication check, replacing `the hard-coded repo grants (`model.GrainSourceRepo()`)`; credential narrowing in the GCP, git and model providers; per-use reducers in the git, model and Google API proxies, with the audit log; CI's `capabilities:` resolved to grainfiles, `repo-read`/`repo-write` as git reducers. **Anselm** is adopted only when that project is ready, as a later stage of its own |
| **6. Signed commits** | keys in `.grainconfig`; verifying signatures in the forge check | verifying the approver's signature against policy (`require_signed`) |

Each stage is a design-item set to be filed after this review. None is
filed by this change.

## Appendix A. How we got here

This design was first written as "task repos" by task 4446, and amended
eight times before it was consolidated here and renamed (grain repos
were formerly called task repos; grainfiles were "task-repo objects").
The main text describes only the result. In order:

| Task | What it settled | Later reversed |
| --- | --- | --- |
| 4446 | Markdown grainfiles in plain git as the backlog and the Silo/Grain channel; Silo-hosted git with a pre-receive hook | separate `id:` fields; Silo copying tasks into a Grain's repo on placement |
| 4448 | path as identity, with tombstones; execution records in the Grain's own repo | Silo writing placements and `superseded` into each Grain's repo (by 4452); backlogs as folders of ranked references (by 4635) |
| 4450 | grainfiles in any repo, `.grain/` in code repos, design docs in place; trust in code repos through their own merge process | nearest-README discovery of grain folders (by 4487); configurable task homes (by 4635) |
| 4452 | each Grain has a root; backlog lanes | `kind: include` and `kind: exclude`, `kind: repo`, README root markers, spaces and namespaces with a bottom-up union (by 4487); one writer per repo, Silo writing only its own placements repo (by 4635: per-subfolder writers, Silo writing as users in Grains' lanes) |
| 4453 | `context:` as the one list of what a sandbox holds; assets and sidecars; attachments as assets | — |
| 4456 | capabilities as base capabilities plus grainfiles that only narrow them, with reducers in Anselm (unspecified) | — |
| 4479 | `kind: trigger` with `on:` sources, the design-doc planner as a `design-change` trigger; `kind: gate` for merge gates | `kind: schedule` |
| 4487 | `grain.config.md` with typed active roots (backlog, triggers, memory), per-entry `ignore:`, `archive/`, kind suffixes in file names, `.grainconfig`, `GRAINOWNERS`, tree-only discovery, caches, limits, the orphan check | `space.md` files and cascading `.grainignore` files, both added and removed before it settled |
| 4493 | one-to-many relations navigated from the parent, never by scanning; derived indexes as caches | lookups by scanning for back-references; the parent's declared folders (`thread:`, `runs:`, placements) and back-pointers as checks (by 4635) |
| 4531 | this consolidation, and the names grainfile and grain repo | — |
| 4635 | **task definitions** (no state, no runs, anywhere, with their own thread in `<name>.task/`) and **single-folder task executions** (`<name>.exec/` in one lane of one deployment: a snapshot of the definition, `exec.md` with state and approvals, the execution's own thread, `runs/<run>/run.md`); containment as the relation; placement as creating an execution; a backlog move as a rank edit or a `git mv` in one repo; re-placement as a new execution with `previous:`; definition → executions as a derived index; per-subfolder `GRAINOWNERS` | `kind: ref`, `kind: placement`, `<name>.thread/`, `<name>.runs/`, `<name>.placements/`, the `thread:`, `runs:` and `placements:` pointers and the back-pointers, GF041/GF042, the team's placements repo, hub-managed and intake lanes, the three-step re-placement, `task_home` and its modes (below) |

The alternatives each amendment weighed and rejected, and why:

| Alternative | Why not |
| --- | --- |
| **Keep the name "task repos"** | The repos hold triggers, gates, templates, capabilities, memory and designs as much as tasks, and a code repo with a `.grain/` folder is not a "task repo" in any sense. A grainfile is named for the format; a grain repo is any repo that holds them |
| **Finding children by their back-pointers** (a task's runs and comments, a requirement's tasks, by searching readable repos for files that name them) | Cost grows with everything a reader can see, and the answer depends on which repos it searched. The parent contains its children instead (§5), and Silo's indexes are caches |
| **Forward lists of children on the parent** (a task listing its comments or its runs by path) | Every new child would edit the parent, a contended file, and in another repo for runs. A container folder takes new children without touching the parent's file (§5) |
| **Generic transitive includes and excludes** (4452: `kind: include`, `kind: exclude`, `kinds:` intersection, bottom-up union per space, README root markers, `kind: repo`) | Taken by 4452, replaced by 4487. Activation should be **explicit and typed**: with generic includes, what a Grain fires depended on a chain of include grainfiles in folders other people write, intersected kinds, scoped excludes and a depth limit, so no one file said what was active. References already reach everything that is only *used*. Typed roots in `grain.config.md` say what is active in one reviewed file, and each entry is one folder (§4.1, §4.2) |
| **Scanning front matter** to find grainfiles (every `*.md` with `kind:`) | Every listing would read every blob in a repo, and cost would grow with the repo, not with what the Grain uses: 10k tasks read to find 400 references. A file's kind could only be known by opening it. Kind suffixes let a tree say it (§2.1, §11.1) |
| **Index files** (a generated `index.md` per folder listing its grainfiles) | A second source of truth that drifts from the tree, a contended file every writer must edit in the same commit, and a merge conflict on every concurrent add. A git tree already is an index, content-addressed and conflict-free (§11.1) |
| **Explicit space files** (a `space.md` declaring a folder as a unit others can include) | Brings back a namespace notion the typed roots do not need: an entry names a folder directly, and the folder needs no declaration to be named (§4.2) |
| **Cascading `.grainignore` files** (gitignore-style files in folders, inherited by subfolders) | A root entry's contents would depend on ancestor files the entry does not mention, so a parent folder's edit could silently change what fires for every Grain whose entry is below it. Per-entry `ignore:` keeps each entry self-describing: the folder plus that one entry (§4.2, §12.15). Ownership may cascade (`GRAINOWNERS`) because it never changes what is active |
| **Kind only in front matter** (any `*.md` with `kind:`) | Listing a folder by kind would need every blob, defeating tree-only discovery and the listing cache; and a file's role would be invisible in `ls`, a diff or a forge's file browser. A suffix makes the kind visible and cheap, and the forge check keeps the two in step (§2.1) |
| **Root configuration in the Grain's local daemon settings** | Not versioned or reviewable: what a Grain acts on would change with no commit, no owner and no history, and Silo could not read it for its views or its orphan check. `grain.config.md` is a grainfile, owned through `GRAINOWNERS`; the only local setting is where it is (§4.2) |
| **`.grain/repo.yml` or README front matter for repo settings** | README front matter is the code's (forges render it), and a per-folder metadata file brings back nearest-file discovery. One `.grainconfig` at the repo root is found by one path (§8.1) |
| **Tombstones for archival** | One tombstone per closed task, forever, which is exactly the growth archiving is meant to stop. The archive mapping is a fixed rule a resolver applies with one extra lookup (§4.3) |
| GitHub pull requests as the proposal and approval mechanism | Needs GitHub, its accounts and its API. Approval would be a forge setting, not a team rule Silo decides. Repos on other hosts and local repos would be second class |
| Move files between folders to change state (`queued/`, `done/`) | Splits `git log --follow` history, breaks every reference by path, and turns each state change into a delete plus an add that races with concurrent edits |
| One repo per team, Grains included | Every Grain would fetch every other Grain's churn, and per-Grain write rules would become per-path rules in a shared repo with more contention. A root per Grain matches the single-tenant Grain; the churn, executions with their threads and runs, stays in each Grain's root |
| A single repo with filtered views | Read permission in git is per repo, so filtering would mean Silo serving synthetic repos. That is a large custom server, and it breaks plain-git interop |
| YAML only (no Markdown bodies) | Prompts and designs are prose. YAML block scalars are hostile to review and to Markdown tooling. Front matter keeps YAML where it fits |
| Go `text/template` | Not portable outside Go. It has method calls on values, and it cannot be checked against a reference implementation. Jinja2 has a spec-like reference (Python) and a Go port |
| Database only (today) | No offline or standalone history, no review, and no way to reference across Grains or keep history when unlinking. Silo would remain the only channel |
| An API-only task interface without git | Keeps the WebSocket protocol growing for every kind. A Grain behind NAT that missed a message needs `resume`/`cancel_pending` machinery that git's "pull the tip" gives for free |
| Separate ids (`id: task-4redj39vwnz4`, 4446's first draft) | A second name that drifts from the first. Every reference had to carry both a path and an id, readers needed an id index per commit to find files that had moved, and the forge check had to police uniqueness across the graph. The path already names the grainfile. With grainfiles that do not move to change state, it is stable, and the rare real move is handled by a tombstone (§2.2) |
| Silo copies the task into the Grain's root on placement (4446's first draft) | Rejected then because the copy competed with the original: an "authoritative after placement" rule said which one won, re-placement tombstoned a task that had not moved, and the outcome lived far from its author. **Revisited by 4635**, which does copy, as the execution's snapshot, without those costs: the definition holds no state to compete, the snapshot is never written back and never wins anything but what runs, re-placement makes a new execution instead of moving anything, and the outcome is the execution's (§6.2) |
| Rank on the task itself | Mixes the backlog owner's ordering into the author's file, so the author and the queue's owner would both write one file. A task in two queues would need two ranks, and placement would have to copy the task to give it a Grain-local rank |
| A single ordering file per repo (`backlog.yml` listing paths in order) | Every reorder edits the same file, so concurrent reorders, placements and removals all conflict. The file is not self-contained: a line in it means nothing without the file it names, and it cannot carry per-entry ownership (`expedite` is Silo's, `rank` the owner's) |
| Filename-prefix ordering (`backlog/0010-…`, `backlog/0020-…`) | A reorder becomes a rename. That conflicts with path-as-identity, and makes `git log` and every record that names the reference (`from:`) follow renames |
| Silo writes a `closed:` summary onto the task when a run closes | Makes every close a cross-repo transaction, adds a second writer to a frozen spec, and leaves two copies of the outcome to drift. The outcome is derived from execution records instead (§6.1) |
| Re-placement as a move of the task, with a tombstone | Nothing about the task changes on re-placement, only which queue holds it. Moving it would break its address for its author, its dependents and its intake references, for no gain |
| Merged ranks across sources (one sort over every reference in every lane) | Ranks are not comparable across writers: Silo's `t` and the owner's `t` were chosen against different neighbours, so a merge lets whoever picks more extreme keys win. Lanes keep each writer's order intact, and the root decides between them (§6.2) |
| Lane priority on each lane's own folder (`lane: {priority: 20}` in a file there) | Numbers spread over files with different writers are a merged rank again, and a folder's writer (Silo, for its folder) could promote its own lane. `grain.config.md` holds it, written by the Grain's owner (§6.2) |
| Silo writing into each Grain's root as itself (4448: Silo's placements in the Grain's `backlog/`, Silo writing `superseded`) | Two writers in one repo with no rule between them, and a Grain whose own history Silo could change. 4452 moved Silo to a placements repo of its own; 4635 brings placement back into the Grain's root, but **as the users Silo acts for**, only in the `hub/` lane the owner declared, under per-subfolder `GRAINOWNERS` rules the forge check enforces, and never `runs/` or `superseded`, which stay the Grain's alone (§8.2, §10.2) |
| Adopting Backstage's entity envelope verbatim (`apiVersion`, `metadata.name`, `spec`, `Location`) | `metadata.name` is mandatory, at most 63 characters and slash-free, so it cannot be a path and brings back a separate id. References are resolved in a catalog database rather than by location. The catalog ingests YAML documents, not Markdown with front matter. `apiVersion` and custom-kind processors are ceremony here, `relations` and `status` are computed by the catalog rather than kept in files, and `Location` discovery is transitive. The ideas are borrowed instead, with an optional exporter (§14) |
| Keep `.grain/` declarations YAML for good | Two parsers, two schema sets and two spellings of every key, for the same kinds. Prompts stay in YAML block scalars, which review badly |
| Workflows as Markdown too | Breaks GitHub Actions compatibility and the tools that read workflows, for a body workflows do not have (§9.4) |
| the forge check in front of code repos (Silo as the code repo's proxy or mirror) | Silo would have to sit between developers and their forge, or the two would disagree. The forge's review already decides what lands; Silo validates what it reads instead (§9.6) |
| Task home always the code repo | Proposing would need forge accounts and write access, every task would cost a review in the code repo, and team-targeted work has no code repo to go to |
| Task home always the intake | The owner's case: long-lived, design-linked work belongs beside the design and the code, versioned and reviewed with them. 4450's `promote` and `code-repo` modes gave that; 4635 removed the modes, since a stateless definition can simply be written or moved there (§9.2) |
| Copy the task into the code repo, keeping the intake's | Two copies of one spec, the problem 4448 removed. A move with a tombstone keeps one |
| Commit the task file in the work's own change | The definition would not exist in the code repo until the work merged, a failed run would leave none, and the task file would be reviewed as part of a change it describes |
| Placement waits for the code-repo commit to land | Delays the work by a review, for nothing: the spec is the same at both addresses, and references follow the move (§9.2) |
| Trigger-fired tasks committed to code repos | A commit, and a review, per firing, in the code's history, for content that is the trigger plus an event (§6.6) |
| Let a code repo name the team its triggers fire for, with no adoption | Any repo could fill any team's intake. Adoption makes the team's admins the ones who choose (§6.6) |
| Materialise every reference a task holds | `depends_on` would pull other tasks' repos into the sandbox, `template` and `moved_from` would bring files nobody meant the agent to have, and a placement's `to:` would bring the intake. The sandbox would be whatever the graph happened to touch. Only `context:` materialises, and the rest stays metadata (§7) |
| Transitive `context:` by default | Unbounded sandboxes: one design that links to its neighbours brings the whole docs tree, and what a run sees would change whenever someone adds a link. Off by default, `depth: n` where wanted, under file and byte caps that skip and record (§7) |
| Keep separate `Target`, `Reads` and `Writes` fields | Three mechanisms for one idea, plus attachments and pasted context as a fourth and fifth. Each needed its own policy check, and task 4303 was a repo that reached a sandbox by a route the policy did not see. One list with a mode is one check (§7, §12.21) |
| Require Git LFS for assets | Not plain git everywhere: Silo's git host, a bare `git daemon`, a `file://` root and many mirrors do not serve it, and a clone without LFS gets pointer files. LFS is allowed where the host has it; size limits and external artefacts cover the rest (§2.3) |
| Other names for `context:` (`mounts`, `workspace`, `sandbox`, `inputs`, `reads`/`writes`) | Each names one mechanism or collides with an existing field (§7) |
| A sidecar for every asset | Metadata most assets do not need: an image in a design has its alt text in the link. Sidecars are optional, and are where an asset needs a license, provenance, or an external location (§2.3) |
| Attachments inline in front matter (base64) | Front matter is for review; a megabyte of base64 is not. Every diff, digest and parse would carry it. A file beside the grainfile is plain git, and renders in any forge (§2.3) |

| Opaque capability names (`gcp-key`, `self-debug`, the status quo) | What a name grants and implies lives in provider code (`the hard-coded repo grants (`model.GrainSourceRepo()`)`), invisible to the local policy, which is how task 4303's repo reached a sandbox. A name cannot be narrowed (one `gcp-key` for every project and role), composed across an org and a team, or explained to an approver. Grainfiles over declared bases make the surface readable and the narrowing explicit (§6.8) |
| Capability grainfiles able to grant beyond their base (an `extra_roles:`, or a reducer that adds) | Breaks monotonicity, the one property that makes every other check local: with it, effective authority depends on every grainfile in the chain, the policy must re-derive the maximum, and an included folder could escalate any capability built on it. Authority comes only from bases and allowlisted secrets; everything else narrows (§12.27) |
| Granting on reference, or a capability root (a Grain honours whatever capability grainfiles a root of it lists) | Makes naming a shared folder a grant, so a team repo's maintainer could hand any Grain that names it new authority. A definition changes what may be given, not whether anything runs, so it is a reference; the policy's source allowlist decides what it honours (§4.1, §12.29, §12.30) |
| A separate `capabilities:` list on tasks | A second list beside `context:`, with its own check: the split 4453 removed for reads and writes. One list, with `mode: use` for a request, is one check, and the approver reads one thing (§7, §6.8) |
| Choosing a reducer language now: **CEL** (Google's Common Expression Language), **WASM** modules, **Rego** (Open Policy Agent), or **macaroon caveat strings** (and Biscuit's Datalog caveats) | The owner's decision: Anselm comes from a project in flight, and this design fixes only the contract any implementation must meet (§6.8). These are the prior art Anselm will be compared against. CEL is non-Turing-complete, side-effect free and cost-bounded, but has no notion of narrowing: monotonicity would be checked by the Grain, not given. WASM sandboxes anything and bounds it with fuel, at the price of opaque binaries in a review-first format. Rego is a mature policy language with partial evaluation, but evaluates against a data document rather than narrowing a capability, and is larger than a reducer needs. Macaroon caveat strings are the model itself, but first-party caveats are an uninterpreted string for each verifier to parse, and so are not a language. Biscuit's Datalog is attenuation-native, the closest prior art. The built-in reducers are a deliberately small interim, not a candidate |
| Skipping reducers a Grain cannot evaluate (best effort until Anselm) | Skipping a narrowing widens. Two Grains would grant one grainfile differently, and a placeholder would silently be a no-op. A Grain that cannot evaluate a reducer refuses, naming it (§6.8) |
| Keep a separate `kind: schedule` beside `kind: trigger` | Duplicates the trigger concept: a schedule is "when the clock says, file this", one event source among several. Two kinds would carry the same `fires`, approval, overlap, rate limits, adoption and customisation rules twice, and every reader, `kinds:` list and security rule would name both. One kind with `on.schedule` is how GitHub Actions and grain's own workflows already say it (§6.6) |
| One `kind: trigger` that also covers merge gates (grain's `gate: true` today) | Firing work and holding a merge are different things, and mixing them is what makes today's `.grain/triggers` messy: one file shape where some keys file tasks and others block merges, so neither can be reasoned about alone. A gate must be monotone and not weakened by the machinery that customises triggers (excluding a team's trigger must never release a merge), and must be read from the base, not the change. Separate kinds let a trigger root admit standing work without admitting policy, and let each schema refuse the other's keys (§6.7, §12 items 38–39) |
| Include `on: task-closed` now, for chaining | `depends_on` already chains known work. A close-fired trigger can fire itself through its own firing's close, across Grains, and needs cycle detection and a rule for whose close counts. Reserved, and refused by v1 readers, until that design exists (§6.6) |
| Pause and next-fire time in the trigger's front matter | Runtime state in git: a commit per fire, per pause and per resume, contended between Silo, the Grain and the definition's authors. `enabled:` is the reviewed switch; the rest is in the database (§6.6, §1.2) |
| 4539 | **Terminology**: grainfile, the Grainfile Format, grain folder (a directory with `grain.config.md`; a grain repo when it is the whole repo; never nested), deployment (never "instance"); folders located by URL plus path. **External forges**: repos are authoritative on ordinary forges, enforced by a protected branch and a required forge check over `GRAINOWNERS`; Silo writes as the user through the forge API and keeps a registry that is itself a grain folder. **Lifecycles** with six state categories, run status on runs. **Folder templates** replacing `kind: suite`. §5 reconciled with 4493's decisions (`.thread/`, `.runs/`, `placements:` pointer files, derived indexes only for dependents and implementing tasks). Grain-side claims corrected: `model.CapabilityRepos` does not exist (repo grants are hard-coded, `model.GrainSourceRepo()`); grain's path triggers are `on: merge`/`pull_request`, not `push`; schedules have no `hourly`, `cron` or `queue` overlap; suites' `mode` is `count`/`until-clean`. The normative spec, `docs/grainfiles-spec.md`, with schemas and `grainlint` | Silo as git host and its pre-receive hook (by 4539, below); `.grainconfig` `host`, `ref_policy` and `grain_folder`; `kind: suite`; task progress as derived-only status |

**Rejected by 4539: suites as lists.** A suite named templates by stem
and ran them as a list. Inter-task dependencies and shared content could
not be expressed as files with relative references, so the suite became
a folder template (§6.13), whose members are ordinary grainfiles.

**Rejected by 4539: fully opaque lifecycle states.** Grain could not tell
whether a task was done or dispatchable (§6.12).

### Superseded by 4635: backlog references, placements and pointers

Until task 4635 (decided by the owner on 2026-10-17), work was modelled
as one **task** that held both the request and its state, and three
kinds of pointer around it:

- a task grainfile in the team's intake carried `state`, `approved_by`
  and `cancel`, a frozen spec after approval, and two folder pointers:
  `thread:` (default the sibling `<name>.thread/`) for its comments and
  `placements:` (default `<name>.placements/`) for one `kind: placement`
  grainfile per placement, written by Silo into the task's repo with the
  approver's delegation;
- each placement was a **backlog reference**, `kind: ref`
  (`<name>.ref.md`: `to`, `spec_rev`, `rank`, `runs`, `attestation`), in
  a **hub-managed lane**: Silo's per-Grain folder
  (`grains/<grain>/backlog/`) in the team's **placements repo**, which
  the Grain's `grain.config.md` named as a lane by locator; the intake
  also had a `backlog/` of references ranking unplaced work;
- each reference declared `runs:`, a folder in the Grain's own root
  (`runs/<deployment>/<name>.runs/` for Silo's placements), holding
  `*.run.md` records with `task:` and `from:` back-pointers and the
  run's comments with `run:` back-pointers;
- every child carried a back-pointer, and lint checked it against the
  folder it was in: GF041 (misplaced child) and GF042 (stray child);
- **re-placement** was three steps across three repos: mark the old
  placement `withdrawn` and remove the old reference; write a new
  reference with its own runs folder and `previous:` naming the old run;
  write a new placement; and a run whose reference was readably gone was
  fenced until its Grain marked it `superseded`;
- moving a task into a code repo (`task_home` modes `intake`,
  `code-repo`, `promote`) re-expressed its `thread` and `placements` as
  locator references, so its discussion and placements stayed in the
  intake while the task moved.

**Why it was replaced.** The owner found it too complex: **moving
things in a backlog touched several repos** (a placement was a commit in
the placements repo, one in the task's repo and a runs folder in the
Grain's), **the hub had to write into task repos** (every placement
pointer), and every pointer needed a lint to keep it agreeing with where
its children were. The replacement keeps the request and its execution
apart: a **definition** holds no state and lives anywhere, with its own
thread inside it; an **execution** is one folder in one lane of one
deployment, holding a snapshot of the definition, its state, its own
thread and its runs. Containment replaces every pointer, a backlog move
is a rank edit or a `git mv` in one repo, re-placement is one new
execution plus the old Grain marking its own, and the hub writes only
where the work is, as the user it acts for.

### Superseded by 4539: Silo as the git host

Until task 4539, this design made Silo the git host for every team's
intake, placements and shared repos and for the Grains' roots, served
over git smart HTTP (`/git/<owner>/<name>.git`) from a `silo-git`
StatefulSet on a regional persistent disk, with bundles to GCS. Silo's
**pre-receive hook** enforced `GRAINOWNERS`, the schemas and the fixed
per-kind rules on every push, and repos on other hosts were validated on
read only. It was replaced because hosting git is a large operational
surface (storage, backup, availability, push performance) that ordinary
forges already provide, teams already keep their code and review there,
and a protected branch with a required check enforces the same rules
without Silo holding source at rest. The text it replaced, for the
record:

> ### 8.3 Silo is the git host: hosting and the pre-receive hook
>
> Silo serves its repos and the Grains' root repos over **git smart HTTP**
> (`/git/<owner>/<name>.git`):
>
> - **humans** reach it behind IAP, as the hub API (hub.md §5.1);
> - **Grains** reach it with their node credential, on the IAP-exempt node
>   path, as `/v1/nodes/connect` (hub.md §5.2.1).
>
> A **pre-receive hook** runs in Silo's own process. It checks every
> pushed commit, not only the tip, against the repo's `GRAINOWNERS` files
> (§8.2), for the pusher identity that authentication established.
>
> **One writer makes the hook simple.** Every Silo-hosted repo but an
> intake gives its whole tree to one set of owners in `GRAINOWNERS`: Silo for its placements,
> the Grain and its owner for a Grain root, the maintainers for a shared
> repo. For those, authorization is one check, *is the pusher listed*, and
> everything after it is validation: schemas, path rules, transitions and
> references. Only an intake's rules work at the level of fields. For an
> intake, it works at the level of kinds, files and front-matter fields:
>
> 1. **Refs.** It refuses non-fast-forward updates and deletions on
>    protected refs. A new ref is allowed only under `review_prefix`.
> 2. **Changes.** For each commit, it diffs the commit against its parent.
>    For each changed `.md` file, it parses the front matter at both
>    revisions and computes the changed fields (`body` counts as one
>    field). It then finds each file's owners (the nearest `GRAINOWNERS`,
>    last matching line, with field lines overriding for their fields) and
>    checks the pusher is one of them, for that verb, for every changed
>    field; then the kind's fixed rules (§8.2). A file nobody owns refuses
>    the push, with a message naming the file, the field and the
>    `GRAINOWNERS` line that was nearest.
>    **Names.** A file whose name has a kind suffix must have front matter
>    whose `kind:` equals it, and a file whose front matter has `kind:` but
>    whose name has no suffix is refused too, so a grainfile can never be
>    mistaken for an asset (§2.1).
> 3. **Paths.** Identity is the path (§2.2), so:
>    - a `kind: moved` file can never be changed or deleted, so no new
>      grainfile can be created at a tombstoned path;
>    - only `*.ref.md` files, and assets outside a frozen task's
>      `attachments/`, may be deleted. Any other grainfile leaves its path
>      only by becoming a tombstone, or by the archive mapping (§4.3), under
>      a `move` its owners allow;
>    - no new grainfile may be created at a path whose tombstone or archive
>      path exists;
>    - a tombstone's `to:` must resolve to a grainfile whose `moved_from`
>      lists the tombstone's global address. Within one repo, both are in
>      the same push. Across repos, the new home must already exist
>      (§8.5).
> 4. **Validation.**
>    - Each changed file must pass its kind's JSON Schema, and its
>      `schema` version must be one the hook knows.
>    - A new or changed reference must resolve, following redirects, with
>      **the pusher's** read rights (§12.4).
>    - A backlog reference must resolve to a `kind: task`, and no other
>      reference in the same lane may point at the same task.
>    - A `grain.config.md` must pass its schema: each entry's `path`, in
>      another repo, carries its `@ref` and `ref_mode` (`track` names a
>      branch, `pin` a tag or a commit), its `ignore:` patterns parse, at
>      most one memory entry is `write: true` and it is in the same repo,
>      and `grain.id` and `owner` are unchanged. Each entry's folder must
>      resolve with the pusher's rights when it is added (§12.4).
>    - A push that would take a folder over the `objects_per_entry` limit
>      of any root entry Silo knows names it is refused (§11.1).
>    - With `orphans: refuse` in the repo's `.grainconfig`, a push that
>      leaves a new orphan is refused (§11.3).
>    - An execution record's `task` must resolve, and its `from` must have
>      existed at `from.rev`. Its state changes must follow §6.3, and
>      terminal states are final. A **fenced** run, whose `from` is a
>      placement Silo has since removed, takes only `state: superseded`
>      (§10.7).
>    - A task's spec fields are frozen once it is approved (§6.1).
>    - A task's `context:` entries must resolve with the pusher's rights.
>      `write` must name a repo or folder at a branch, `depth` is at most 3
>      and only on files, and no two entries may land at overlapping paths
>      (§7). A template's `input:` entries must name a declared input of
>      type `ref`.
>    - A `kind: asset` sidecar's `asset` must resolve to a file that is not
>      a grainfile, matching its `sha256` and `bytes` where given. A blob over
>      10 MiB is refused, and a grainfile may not be an LFS pointer (§2.3).
>    - A push that adds a `depends_on` cycle within Silo's graph is
>      refused.
> 5. **Secrets scan.** Content is checked against secret patterns (§12.6).
> 6. **Post-receive.** Silo updates its database projection (tasks,
>    backlogs, runs, each Grain's active set) and its derived indexes (§5),
>    and refreshes its listing cache for the folders that changed (§11.2).
>    It sends `pull` notifications to the Grains whose root entries name a
>    folder that changed, removes the placements and intake references of tasks
>    that just closed (§10.4), and fires the triggers watching what moved,
>    planning among them (§6.6, §6.9).
>
> Plain git cannot express per-path permissions, but the server that
> receives the push can. **No pull requests and no GitHub are needed.**
>
> ### 8.6 Running Silo as a git host
>
> - **Storage.** A `silo-git` StatefulSet holds the repos as bare
>   repositories on a **regional persistent disk**, which is replicated
>   across two zones. Smart HTTP is served by Silo's Go server, which
>   calls `git http-backend` or `git upload-pack`/`receive-pack` with the
>   hook in-process. **Cloud SQL stays the hub's database**, holding the
>   directory, runtime state and the projection and indexes of git. All
>   of that can be rebuilt from git plus the runtime tables. A
>   git-on-grainfile-storage server, such as JGit DFS on GCS, is the scale-out
>   option. It is not needed at first.
> - **Backup and restore.**
>   - Disk snapshots run hourly.
>   - After every push to a repo, a `git bundle` of that repo goes to a GCS
>     bucket with grainfile versioning and retention.
>   - A restore is a disk from a snapshot plus the bundles newer than it.
>     The drill is the one in hub-operations.md. Every Grain also holds a
>     full clone of its own repo and can re-push it after a loss. That
>     push is allowed only into an empty or behind repo, and checked
>     commit by commit.
> - **Hook performance.**
>   - It parses front matter only, for changed `.md` files only.
>   - It caches directory lookups for the length of the push.
>   - It caps a push at 1,000 changed files and 10 MiB, and so any one
>     asset at 10 MiB (§2.3).
>   - Target: p99 under 200 ms for a 10-file push.
>   - Writes to one repo are serialised, and different repos run in
>     parallel.
> - **Scale.** Assume thousands of Grain roots, each running about 10⁴
>   tasks a year. An execution record with its thread is roughly 5 KiB,
>   which is about 50 MiB a year per Grain before compression. A backlog
>   reference is under 1 KiB, and `backlog/` holds only open work (§6.2).
>   The tasks themselves are in the intakes, so a team's intake grows
>   with the team's tasks, at a similar size per task.
>   - **Commit batching.** Silo makes one commit per dispatcher pass per
>     repo, Grains push at most every 5 seconds, and notifies are
>     coalesced (as hub.md §2.2.4 does today).
>   - **Fetch cost.** Grains use partial clone (`--filter=blob:none`), so
>     a fetch costs what changed.
>   - **Archival.** Closed tasks and closed runs never move. After a year
>     a Grain's root repo, or an intake, can be **rolled over**: a new repo
>     continues it, and the old one becomes read-only under an alias
>     (`eu-2026`). A rollover is not a move, so there are no tombstones and
>     no `moved_to`: every old reference still resolves, because it names
>     the old repo's URL. A Grain rolls its root over itself: it copies its
>     `grain.config.md`, its memory and its own lane's open references into
>     the new root (references are pointers, so that is allowed), and the
>     rollover waits for open runs to close. Silo's placements are not in
>     the root, so they need nothing. In an
>     intake, open tasks stay in the old repo until they close, and new
>     tasks go to the new one.
> - **Availability.** While Silo's git is down:
>   - A Grain keeps running what it has.
>   - It commits locally and pushes on recovery, rebasing onto the new tip
>     with a front-matter-field 3-way merge. Its root has no other writer,
>     so the rebase only ever replays its own commits, and Silo's
>     placements, which changed meanwhile, are in another repo.
>   - Nothing new is placed.
>   - Standalone and locally scheduled work continues.
