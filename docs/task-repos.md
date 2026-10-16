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
  Silo repo, holding its placements and its assigned team schedules. Silo
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

This design stores grain's **tasks, schedules, templates and design docs**
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

Every Grain has a **root folder** (§3.2): a whole repo, or a subtree
such as `.grain/`. Its README marks it, and the root holds the Grain's
own objects as ordinary files: its local backlog, its execution records
and their threads, its own schedules and templates. **Only the Grain and
its owner write it.** What the Grain acts on is its root's
**namespace**: the root's own objects plus whatever its `kind: include`
objects bring in, less what `kind: exclude` objects cut (§3.3).

**Linking a Grain to Silo is an include.** Silo keeps a folder for each
Grain in a Silo repo (the team's `placements` repo), holding that
Grain's **placements** (backlog references to tasks) and the team
schedules assigned to it. The Grain's root includes that folder, live and
restricted to those kinds (§8.12). Silo writes only its own repos; the
Grain reads them. Silo also grants the Grain read access to the repos
those references point into: the team's intake, where the tasks
themselves live, and the shared repos the tasks reference.

**Unlinking** deletes that include. Nothing moves: the root, with all of
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

Repo **roles** are conventions recorded in each repo's metadata (§3).
The format does not depend on them:

| Role | How many | Holds | Typical writers |
| --- | --- | --- | --- |
| **grain-root** | one per Grain instance: a repo, or a subtree of one | that Grain's root (§3.2): its include and exclude objects, its own lane of backlog references, its execution records and their threads, its own tasks and schedules | **that Grain and its owner only** |
| **placements** | one per team, in Silo | a folder per Grain serving the team: its placements (`backlog/`) and the team schedules assigned to it (`schedules/`, as includes) | **Silo only**, on its own behalf and for the people who reorder through its UI |
| **intake** | one per team, in Silo | the team's tasks: proposals, approvals and cancels; the team's ranking of unplaced work; discussion on tasks; the schedules the team adopts (as includes) | any authenticated user (new proposals only), the team's approvers, the team's members (ranks), Silo. The one role with several writers (§8.1) |
| **shared** | any number, in Silo or elsewhere | templates, schedules, design docs, template partials, and includes of other shared folders | the team's maintainers, through review branches |
| **code** | the code repos themselves | a grain folder (`.grain/`, with its own `.grain/README.md`): templates, schedules, suites, triggers and workflows, tasks homed there (§5.5), and design docs in place in `docs/design/` (§3.1, §12) | whoever the repo's own review and merge process lets land a change (§8.13) |

"Silo" is **one or more repos**, not a special store. Everything works by
references alone:

- a backlog reference in a Grain's placements, or in its root, points at
  a task in an intake, a Grain root, or a code repo's grain folder (§5.5);
- a Grain's root includes the folders whose objects it acts on (§3.3);
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
- **Each repo or folder has one writer** (§3, §8.1), except a team's
  intake, where each field has one. The databases may mirror a field for
  queries, but they never compete with git for it.

| Git-backed | Runtime only (database) |
| --- | --- |
| task: `title`, body (the prompt), `team`, `context` (§5.6), its `attachments/` | the attempt list, attempt numbers, sandboxes, leases |
| task: `submitter`, `approved_by`, `approved_at`, `origin`, `filed_by`, `state` (proposed, approved, rejected, cancelled), `cancel` | the live `running` sub-states (sandboxing, agent turn, waiting for CI) |
| task: `template` (+ `inputs`, `rev`), `schedule`, `depends_on`, `implements`, `capabilities`, `constraints` | events, `seq`, transcripts, tool calls, diffs, logs |
| backlog reference: `to`, `spec_rev`, `rank`, `priority`, `expedite`, `attestation`, who added it and when | heartbeats, slot counts, offer timers |
| execution record: `task`, `from`, `state` at the coarse grain of §5.3, timestamps, `refusal`, `superseded`, `previous` | the dispatcher's claim, `refused_by` lists, offer counts |
| execution record: `closed`: outcome, change link, merge commit, summary, requirement revisions | merge-queue position, CI run ids, cost and token counts (summarised into `closed` if wanted) |

A coarse state change, such as a run going `accepted → running → done`,
is a commit. A fine-grained step, such as "agent turn 14", is an event.

## 2. Objects: one self-contained Markdown file each

Every object is one UTF-8 file. The file has **YAML front matter**
between `---` lines, then a **Markdown body**. Every file declares:

- `kind:`, one of `repo`, `task`, `ref`, `run`, `comment`, `template`,
  `schedule`, `suite`, `trigger`, `design`, `moved`, `include`,
  `exclude` or `asset` (a sidecar, §2.2);
- `schema:`, as in `grain/task@v1` (§10).

There is **no `id:` field.** An object is named by where it is (§2.1).
Every other file in a repo, an image or a CSV or Markdown without `kind:`,
is an **asset**: referenceable the same way, but not an object (§2.2).

The body carries what a human reads: a task's prompt, a template's text,
a design's prose. A file means something on its own. You can read it,
mail it or `cat` it without knowing which repo it came from.

**Layout is loosely specified.** Tools find objects by **scanning front
matter**: every `*.md` file whose front matter has `kind:`. The
recommended folders below are conventions only. Two places where a folder
matters: a repo's own backlog references sit in the folders its README's
`backlog:` names, `backlog/` by default (§3), and the folder holding a
reference is its **lane** (§5.2).

A Grain's root (§3.2):

```
README.md                                  kind: repo    (role: grain-root)
backlog/silo.md                            kind: include (Silo's placements for this Grain, §8.12)
backlog/<name>.md                          kind: ref     (the Grain's own lane)
schedules/silo.md                          kind: include (the team schedules Silo assigned it)
schedules/no-<name>.md                     kind: exclude (a cut, §3.3)
schedules/<name>.md                        kind: schedule (its own)
templates/<name>.md                        kind: include (a shared template set) or kind: template
tasks/<name>/task.md                       kind: task    (its owner's own work)
runs/<name>/run.md                         kind: run     (one execution record)
runs/<name>/comments/<utc>-<author>.md     kind: comment (the run's thread)
runs/<name>/comments/<utc>-<author>/<file> an asset    (a reply's attachment, §2.2)
```

Silo's placements repo, one folder per Grain:

```
README.md                                  kind: repo    (role: placements)
grains/<grain>/backlog/<name>.md           kind: ref     (one placement)
grains/<grain>/schedules/<name>.md         kind: include (a team schedule, or folder of them, assigned to that Grain)
```

An intake or a shared repo:

```
README.md                                  kind: repo
tasks/<name>/task.md                       kind: task
tasks/<name>/attachments/<file>            an asset      (attached to the task, §2.2)
tasks/<name>/comments/<utc>-<author>.md    kind: comment (discussion of the task)
backlog/<name>.md                          kind: ref     (one ranked reference to a task)
templates/<name>.md                        kind: template
templates/partials/<name>.md               kind: template (include-only, inputs declared)
templates/<name>.md                        kind: include or kind: exclude (another shared set, and cuts from it)
schedules/<name>.md                        kind: schedule (or, in an intake, kind: include: an adopted schedule)
designs/<name>.md                          kind: design
designs/img/<file>                         an asset      (an image the design embeds)
designs/img/<file>.md                      kind: asset   (its optional sidecar, §2.2)
<an old path>                              kind: moved   (a tombstone, §2.1)
```

A code repo keeps the same kinds in its grain folder, beside the code
(§3.1):

```
README.md                                  the code's own README: no front matter needed
.grain/README.md                           kind: repo    (scope: [/.grain/, /docs/design/])
.grain/templates/<name>.md                 kind: template
.grain/schedules/<name>.md                 kind: schedule
.grain/suites/<name>.md                    kind: suite
.grain/triggers/<name>.md                  kind: trigger
.grain/workflows/<name>.yml                a GitHub Actions workflow: YAML, not an object (§12.2)
.grain/tasks/<name>/task.md                kind: task    (homed here, §5.5)
docs/design/<name>.md                      kind: design  (in place, §12.3)
```

**Names.** Whoever creates an object picks its path, once:

- a new task or run: `<yyyy-mm-dd>-<slug>`, with `-2`, `-3`… if that is
  taken. Two writers racing for one name collide as an ordinary git
  conflict, and the second one picks the next suffix;
- a migrated object: `<legacy number>-<slug>` (§13);
- a generated object: a name derived from what generated it, so a retry
  lands on the same path. Schedule firings use
  `tasks/scheduled/<schedule>/<fired_at>` (§8.12), and planning uses
  `tasks/planned/…` (§7.4);
- a backlog reference: its target's name
  (`backlog/2026-10-16-approval-flow.md`);
- an include or exclude: what it is for, from its includer's side
  (`backlog/silo.md`, `schedules/no-granule-pull.md`);
- a comment: `<utc>-<author>`, such as `20261016T144710Z-grain-eu.md`.
  Each author writes its own comments one at a time, so two authors
  appending to one thread never conflict.

The slug is taken from the title when the object is created. A later
title change leaves the path alone.

### 2.1 Identity is the path

- **Within a repo**, an object is its repo-relative path:
  `tasks/4310-carry-approver/task.md`.
- **Globally**, it is the git URL plus that path, which is exactly the
  cross-repo reference form of §4:
  `https://silo.example.com/git/hub/intake.git//tasks/4310-carry-approver/task.md`.

An alias (`intake:tasks/4310-carry-approver/task.md`) or a relative path
(`../4310-carry-approver/task.md`) names the same identity once it is
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
move to another repo replaces the file at the old path with a
`kind: moved` object:

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
references, includes and excludes are ever deleted**: they are pointers
and configuration, not addresses anyone keeps (§5.2, §3.3). Every other
object leaves its path only by becoming a tombstone. Assets are not
objects, and are deleted or moved like any file (§2.2), except a task's
`attachments/`, which are frozen with its spec (§5.1).

**Repo moves.** References name repos by URL, usually through an alias
declared in the referring repo's README. Moving a repo therefore means:

- changing the alias in the READMEs that declare it, one line each;
- optionally setting `moved_to: <new git URL>` in the old repo's README,
  as a **repo-level redirect**. The old repo stays readable. A reader
  that resolves into a repo whose README has `moved_to` retries at the
  new URL with the same path, and that counts as one hop.

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
- **Never in a namespace.** Assets are not work. An include contributes
  objects only, so an asset under an included folder adds no name, and
  `kinds: ["*"]` means every object kind, not every file (§3.3). Nothing
  ranks, places, fires or runs an asset.
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
bytes: 3412
---
<an optional longer description, for humans and for agents>
```

This is [`hub-shared/designs/img/claims-flow.png.md`](../examples/task-repos/hub-shared/designs/img/claims-flow.png.md).
Schema: [`schemas/asset.schema.json`](../examples/task-repos/schemas/asset.schema.json).

- **Where it lives.** By convention, beside the asset, named for it with
  `.md` appended: `img/claims-flow.png.md` describes `img/claims-flow.png`.
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
- **What it is not.** A sidecar is configuration about data, like an
  include. It is never ranked, placed or run. It may be in a namespace
  like any object (an include with `kinds: [asset]`), where it means
  nothing to a Grain.
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

## 3. Repo metadata: `README.md` front matter (`kind: repo`)

A whole-repo task repo's root `README.md` declares the repo. A grain
folder inside another repo is declared by its own `README.md`
(`.grain/README.md`), with a `scope` (§3.1). Either way the prose body is
for humans. Schema: [`schemas/repo.schema.json`](../examples/task-repos/schemas/repo.schema.json).

```yaml
---
kind: repo
schema: grain/repo@v1
role: intake                       # convention: grain | intake | shared | code | …
title: Hub team intake
owner: {team: hub, silo: https://silo.example.com}
host: silo                         # silo: Silo enforces writes (§8); other: readers validate (§8.10)
default_ref: main                  # what an unpinned reference resolves to
ref_policy: {protected: [main], fast_forward_only: true, review_prefix: review/}
backlog: [backlog/]                # the folders whose kind: ref files are this repo's queue (§5.2)
aliases:                           # optional shorthand: alias → git URL
  shared: https://silo.example.com/git/hub/shared.git
  eu: https://silo.example.com/git/grains/eu.git
writers:                           # write ownership; whatever is not allowed is refused
  - who: [authenticated]
    may: create
    kinds: [task, asset]               # asset: the new task's attachments (§2.2)
    paths: [tasks/**]
    where: {state: proposed}
    fields: [kind, schema, title, team, context, state, submitter, submitted_via, origin, depends_on, implements, template, capabilities, constraints, labels, created_at, body]
    bind: {submitter: $pusher}
  - who: [team:hub#approver]
    may: update
    kinds: [task]
    transition: {field: state, from: [proposed], to: [approved]}
    fields: [state, approved_by, approved_at]
    bind: {approved_by: $pusher}
    forbid: {approved_by: $submitter}
  - who: [team:hub#member]
    may: update
    kinds: [ref]
    paths: [backlog/**]
    fields: [rank, priority, ranked_by, ranked_at]
    bind: {ranked_by: $pusher}
  - who: [silo]
    may: [create, delete]
    kinds: [ref]
    paths: [backlog/**]
    fields: "*"
---
```

**Fields:**

- `role`: a convention for tools and humans.
- `owner`: the team, the Grain (`{grain: eu}`) or the user the repo
  belongs to. It also names the Silo that hosts the repo, if one does.
- `host`: who enforces writes (§8).
- `default_ref`: what an unpinned reference resolves to.
- `ref_policy`: which refs are protected, whether pushes must
  fast-forward, and the prefix for review branches.
- `backlog`: the folders that hold this repo's queue of ranked
  references (§5.2). The default is `[backlog/]`.
- `aliases`: shorthand for related repos.
- `moved_to`: a repo-level redirect, set when the repo has moved (§2.1).
- `scope`: the subtrees this README governs (§3.1). The default is the
  README's own directory, so a root README governs the whole repo.
- `task_home`: where approved tasks live (§5.5). On an intake, it is the
  team's setting; on a code repo's grain folder, the repo's.

Paths in a README (`backlog`, `scope`, `writers.paths`, `task_home.path`)
are relative to the README's directory, or to the repo root when they
start with `/`. For a root README the two are the same, so a whole-repo
task repo reads exactly as before.
- `grain`: on a Grain root (`role: grain-root`), the Grain's identity,
  `{id, silo}` (§3.2).
- `lanes`: on a Grain root, the relative priority of its backlog lanes
  (§5.2).
- `include_limits`: on a Grain root, lower limits for the namespace
  resolver (§3.3); they can never be raised.
- `writers`: who may write here. **Usually a list of principals**, the
  folder's one writer: `writers: ["grain:eu", "user:bsalmon@google.com"]`
  lets those principals write any object in scope, still subject to the
  schemas, the path rules and the state transitions the hook checks
  (§8.1). Only a folder that genuinely has several writers uses the
  older form, an **ordered list of allow rules**, and in this design
  that is a team's intake. Nothing is writable unless a principal or a
  rule allows it.

A README carries **metadata only**. It never holds objects, and never
adds names to a namespace: that is what `kind: include` objects are for
(§3.3).

**A `writers` rule** (the intake's form) says:

- `who`: one or more **principals**:
  - `silo` (Silo's own service identity);
  - `grain:<node-id>` (a node credential, bound to its owner);
  - `user:<email>`;
  - `team:<name>#member`, `team:<name>#approver` or `team:<name>#admin`
    (resolved from Silo's directory, hub.md §5.3);
  - `authenticated` (any signed-in human).
- `may`: `create`, `update`, `delete` and/or `move`. `move` replaces an
  object with a tombstone and creates it at its new path with
  `moved_from` (§2.1). `delete` is only ever granted on `kind: ref`.
- `kinds` and `paths`: globs that limit which files the rule covers.
  `asset` in `kinds` covers files that are not objects (§2.2), and
  `kind: asset` sidecars.
- `fields`: the front-matter fields the push may set or change. `body` is
  the Markdown body, and `"*"` means every field.
- `where`: required values in the result.
- `transition`: `{field, from: […], to: […]}`, the state changes the rule allows.
- `merge_of`: the change must arrive as a merge of a matching branch
  (`review/*`), pushed by someone other than that branch's author (§8.11).
- `refs`: the refs the rule applies to (default: `default_ref`).
- `bind`: fields that must equal the pusher's identity (`$pusher`).
- `forbid`: fields that must not equal another field.

A rule allows a file change only if every changed field is in `fields`.
Ref deletion and non-fast-forward pushes are never allowed on protected
refs.

**Changes to the README itself** may be made only by the repo's owner
(the team's admins, or the Grain's owner). For a Grain root, `owner` and
`grain` cannot be changed at all. So a Grain cannot widen its own write
rights, and nobody can quietly take over its root.

**Write ownership: one writer per repo or folder.** Silo writes Silo's
repos, a Grain and its owner write its root, and code repos change
through their own review (§8.13). So the hook needs no field-level rules
anywhere but the intake:

| Repo or folder | Its writer | Field-level rules? |
| --- | --- | --- |
| a Grain root | the Grain and its owner (`writers: ["grain:eu", "user:…"]`): its own references and their ranks, its execution records and every state of them, the thread, its own tasks, schedules, includes and excludes | none. The hook still checks schemas, the path rules, run transitions (terminal states are final) and the fence on a withdrawn placement (§8.8) |
| Silo's placements | Silo (`writers: [silo]`): placements, their ranks (for whoever asked, with `ranked_by`), `expedite`, the assigned schedules | none |
| a team's intake | submitters (new proposals), approvers (`approved`/`rejected`), members (ranks of unplaced work), Silo (API proposals, firings, cancels, the intake's references), the team's admins (adopted schedules, tombstones) | **yes**: a task has several writers over its life, and the rules say which may change which field (§8.2, §8.3) |
| a shared repo | its maintainers, directly or by merging review branches (§8.11) | none beyond `merge_of` |
| a code repo's grain folder | whoever its own review lets merge (§8.13) | advisory only |

**The intake is the one folder with two writers**, because a task's
life there really is shared: its author proposes it and an approver
approves it, and the approval is only worth something if the hook can
tell the two apart. Nothing else is: Silo no longer writes a Grain's
backlog or its runs (§5.2, §8.8), and a Grain never writes Silo's repos.

### 3.1 Grain folders inside any repo

**Any repo may hold task-repo objects in a subfolder**: a code repo, a
docs repo, a monorepo with one folder per project. The convention is
`.grain/`, but nothing is located by a fixed path. Objects are still
found by scanning front matter (§2), and the folder is declared by a
`README.md` in it with `kind: repo`. The repo's top-level `README.md`
belongs to the code, and is left alone.

```yaml
---
kind: repo
schema: grain/repo@v1
role: code
title: The widgets repo's grain folder
owner: {team: hub, silo: https://silo.example.com}
host: other                        # the forge enforces writes; readers validate (§8.13)
scope: [/.grain/, /docs/design/]   # what this README governs
default_ref: main
backlog: []                        # a code repo queues nothing itself
task_home: {mode: promote, path: tasks/}
aliases:
  intake: https://silo.example.com/git/hub/intake.git
writers: […]                       # what belongs here; checked on read (§8.13)
---
```

This is [`widgets-code/.grain/README.md`](../examples/task-repos/widgets-code/.grain/README.md).

**Nearest-README discovery.** To find the README that governs a file —
whose `aliases` expand its references, whose `backlog` folders it may
sit in, whose `writers` rules it is checked against — a reader:

1. collects every `README.md` in the repo, at the commit it is reading,
   whose front matter has `kind: repo`;
2. keeps those with a `scope` entry that contains the file;
3. picks the one whose containing entry is **longest**. With default
   scopes, that is exactly the nearest enclosing README with
   `kind: repo`. An explicit `scope` lets a grain folder's README also
   take in a sibling subtree, as `.grain/README.md` takes in
   `/docs/design/`.

Two READMEs whose entries tie for one file are an error, reported on
both, and the file is governed by neither. A file with `kind:` that no
README governs is reported, and is not an object. A code repo's other
Markdown, with no `kind:` front matter, is never read as an object.

A reader finds the candidates cheaply: it reads `README.md` files only,
and a reader that caches them per commit does this once per fetch.

**Identity does not change.** An object in a grain folder is named by
its repo URL and its **repo-relative** path, as anywhere else (§2.1):
`https://git.example.com/hub/widgets.git//.grain/templates/release-notes.md`.
A `/`-rooted reference inside the folder is rooted at the repo, not at
the folder, so `/docs/design/widgets.md#WG-2` names the design doc from
anywhere in that repo.

**A `.grain/` folder with no README** reads as if it had one with
`role: code`, `scope: [/.grain/]`, `host: other`, `task_home: intake`
and, for `writers`, today's declaration rules (§12.1). So every repo's
existing `.grain/` keeps working unchanged until it adds one (§13.1).

### 3.2 A Grain's root folder

**Each Grain has one root folder**: a whole repo (`grains/eu.git`), or a
subtree of one (`.grain/` in its owner's own repo, or a local
`file:///var/lib/grain/tasks.git`). The folder's `README.md` marks it:

```yaml
---
kind: repo
schema: grain/repo@v1
role: grain-root
grain: {id: eu, silo: https://silo.example.com}
owner: {grain: eu, user: bsalmon@google.com, silo: https://silo.example.com}
host: silo
default_ref: main
backlog: [backlog/]
lanes:
  mode: weighted
  order:
    - {lane: "placements:grains/eu/backlog/", weight: 3}
    - {lane: backlog/, weight: 1}
aliases:
  placements: https://silo.example.com/git/hub/placements.git
  shared: https://silo.example.com/git/hub/shared.git
writers: ["grain:eu", "user:bsalmon@google.com"]
---
```

This is [`grain-eu/README.md`](../examples/task-repos/grain-eu/README.md).
The README carries metadata only. The root holds the Grain's objects as
ordinary files, found by scanning front matter like any others (§2):
its include and exclude objects, its own backlog references, the tasks
and schedules its owner writes for it, its execution records and their
threads. A Grain's local config names its root and nothing else; every
other folder it acts on reaches it through an include.

A root is a folder like any other, so nearest-README discovery (§3.1)
finds it, a root inside a monorepo works, and another Grain may include
part of it (a Grain that includes a colleague's `schedules/`, say).
What makes it a root is only that a Grain was configured to start there.

### 3.3 Namespaces: includes and excludes

**Names are global and unique.** A name is a repo plus a path (§2.1).
A Grain's **namespace** is the set of names it acts on by itself: the
references it may dispatch, the schedules it fires, the templates,
suites and triggers it offers. **Including an object adds its name
unchanged.** There is no aliasing and no renaming, so two includes can
never collide, and there are no override rules to need. An object
reached by several routes is one name.

**References and includes are different things.** Any object may
*reference* any other: a task's `template:`, `depends_on:` and
`implements:`, a backlog reference's `to:`, a schedule's
`derived_from:`. A reference resolves for the object that holds it, with
the reader's rights (§4, §11.4), and **never adds a name to a
namespace**. A task can use a template no namespace contains. **Only
`kind: include` adds names**, so no other object, and no README, can
widen a namespace. **Assets** (§2.2) are never names at all: an include
contributes the objects under its target, never its other files. And
**a reference never materialises anything** in a sandbox either; only a
task's `context:` does, through the same resolver (§5.6).

#### Include objects

```yaml
---
kind: include
schema: grain/include@v1
target: "placements:grains/eu/backlog/@main"   # a file, or a folder ending in /
ref_mode: track                               # track a branch | pin a tag or commit
kinds: [ref]                                  # what it accepts; ["*"] for every kind
max_depth: 2                                  # optional: can only lower the depth left
---
```

This is [`grain-eu/backlog/silo.md`](../examples/task-repos/grain-eu/backlog/silo.md).
An include is a standalone object, and may sit **anywhere in a folder's
tree**: `backlog/silo.md`, `schedules/team.md`, `templates/org.md`. It
names one file or one folder:

- **by a relative or `/`-rooted path**, with no `@`. It reads the same
  commit as the include itself, so a relative include inside a pinned
  folder is pinned too;
- **by alias or git URL plus `@ref`**, with a **ref mode**:
  - `track`: `@` names a branch, read at its tip on every fetch. This is
    the live case: Silo's placements, a team's schedules;
  - `pin`: `@` names a tag or a 40-hex commit, and the target never
    changes under the includer. This is the stable case: a shared
    template set, moved forward by editing one line.
- **`kinds:`** is required. It lists the object kinds the include
  accepts, and is a **trust boundary** on top of the Grain's local
  policy: an include of Silo's placements with `kinds: [ref]` brings in
  backlog references and nothing else, whatever that folder ever holds.

**Includes are always transitive.** There is no `transitive` field.
Include objects inside an included folder apply too, because they are
part of what that folder means: `hub/shared`'s `templates/` is its own
templates *and* the org's set it includes, less the one it cuts.
Following only the first level would leave a half-working copy, with
partials missing and cuts not applied. A Grain that does not want a
branch of an included folder excludes the include object that brings it
(below).

#### Exclude objects

```yaml
---
kind: exclude
schema: grain/exclude@v1
names: ["shared:schedules/granule-pull.md"]   # exact names, as references
patterns: ["org:templates/legacy-*.md"]       # or globs over canonical paths: * in a segment, ** across
reason: This Grain runs the granule pull weekly, from its own schedule.
---
```

An exclude names objects, by exact name or by pattern. Names are
canonicalised like references (§2.1). A pattern's repo part is
canonicalised too and is never a glob, so an exclude can only name
objects in repos it names.

#### Resolution: bottom-up, scoped, order-independent

A **space** is a Grain's root, or the target of an include: a folder, or
a single file. Every include and exclude object anywhere in a space's
tree belongs to that space. **Each space first gathers what it holds and
what its includes contribute, then applies its own excludes**, and the
result is what it contributes upward. Siblings combine by union:

```
contribution(space) = ( space's own objects ∪ ⋃ contribution(target of each include in space) )
                      − names matched by the exclude objects in space
namespace(root)     = contribution(root)
```

What follows from it, each with an example from
[`ns-demo/`](../examples/task-repos/ns-demo/README.md) or the other
examples:

- **An exclude only subtracts from its own space's contribution.** It
  never affects the root's own objects, or any other branch.
  `hub/shared`'s `templates/no-legacy-triage.md` cuts
  `org:templates/legacy-triage.md` from what `hub/shared`'s templates
  contribute. A Grain that also includes `org/templates` directly still
  has it.
- **The root's excludes are applied last**, so the root's owner can always
  cut anything: Grain `us`'s `schedules/no-granule-pull.md` cuts a
  schedule that reached it through two levels of includes.
- **Local rules only ever remove, never re-add.** There is no
  "re-include" operator. A space brings an object in only by a new
  contribution: including it directly, or through another branch.
- **The worked case.** Folder X excludes Y. Z includes X, and includes Y
  directly. **Z's namespace contains Y**: Z's contribution is
  (X's contribution − Y) ∪ {Y}. X's exclude subtracted from X's
  contribution, and Z's direct include is a contribution of its own. In
  the examples, `ns-demo/x/` includes `ns-demo/y/` and excludes
  `y/templates/greeting.md`; `ns-demo/z/` includes both `x/` and that
  file; `check.py` finds the greeting in Z's namespace and not in X's
  contribution.
- **A space that includes Y and excludes Y contributes no Y**
  (`ns-demo/w/`), whichever file is read first.
- **File order, include order and depth never matter.** Union and set
  difference do not care about order, and an exclude applies to its
  space whatever depth the names it matches came from. `check.py`
  resolves every namespace in two iteration orders and requires the same
  answer.

**Includes and excludes reached through includes** follow five rules:

1. **An include reached transitively applies**, with its relative paths
   resolved from its own location. **It can never widen its parent:**
   - its `kinds:` are intersected with every include above it on the
     path, so a branch under `kinds: [ref]` can only ever bring
     references;
   - the depth limit counts down at each level (8 at the root; an
     include's `max_depth` can only lower what is left);
   - cycles are detected and cut (below);
   - pinning follows **its own** ref: a pinned include inside a tracked
     folder stays pinned, and a tracked include inside a pinned folder
     still tracks. A team that wants a fully stable set pins each level,
     or uses only relative includes inside a pinned folder.
2. **Including a single include object directly**, a file include whose
   target is a `kind: include`, is **delegation**: you get what it
   brings, under the same narrowing. An includer can adopt somebody
   else's choice of target and ref, and follow it when they change it.
3. **An exclude reached through an include is scoped to that branch**, by
   the formula above: it subtracts from the contribution of the space
   that holds it.
4. **Excluding an include object's name disables that include.** Whatever
   it contributed drops out, unless another route also brings it.
   `ns-demo/v/` includes `x/` and excludes `x/include-y.md`, and gets X's
   own objects without X's branch into `y/`. This is the switch that
   optional transitivity would otherwise have needed.
5. **Include and exclude objects are names in the namespace**, so a
   Grain can list what configured it, but they are configuration: never
   executed, ranked or placed. Excluding an *exclude* object only hides
   its name; it never cancels its cut, because that would be a
   re-include.

**The resolver.** It reads every space at the commit its route names, and
records the commit it used for each, so a namespace can be reproduced:

```
LIMITS = {depth: 8, includes_per_space: 64, spaces: 256, repos: 32, objects: 50000}
         # the root's include_limits may lower these, never raise them

namespace(root):
    budget = {spaces: 0, repos: {}}
    names  = contribution(space(root, commit = root's tip), kinds = ALL,
                          depth = LIMITS.depth, path = [], disabled = [], budget)
    if budget exceeded or |names| > LIMITS.objects:
        report; keep the last good namespace          # fail closed, deterministically
    return names

contribution(S, kinds, depth, path, disabled, budget):
    if (S.repo, S.path) in path:                       # a cycle: everything on it is
        report cycle; return ∅                         # already in an ancestor's union
    count S against budget.spaces and budget.repos
    held  = objects under S at S.commit                # a file space holds one object;
                                                       # READMEs and tombstones never count
    excl  = [matcher(x) for x in held if x.kind == exclude]
    if any matcher is invalid (an undeclared alias, a bad pattern):
        report; return ∅                               # an exclude that cannot be applied
                                                       # must not widen: fail the space closed
    got   = {o in held : o.kind in (include, exclude) or kinds == ALL or o.kind in kinds}
    incs  = [i in held : i.kind == include]
    if |incs| > LIMITS.includes_per_space: report; return ∅
    above = disabled + excl                            # this space's excludes, and those
                                                       # of every space above it on the path
    for i in incs:
        if i.name matched by above:  report disabled; continue      # rule 4
        if depth == 0:               report depth;    continue
        T = resolve(i.target, from i's own location)   # §4, with the reader's own credential
        if T is missing, unreadable, or its redirects break:
            report and skip                            # dangling: never widen
            continue
        T.commit = i.target has @ref ? (pin: that ref; track: the branch's tip)
                                     : S.commit        # relative: the same snapshot
        k = kinds ∩ i.kinds                            # "*" is ALL
        d = min(depth - 1, i.max_depth ?? depth - 1)
        got ∪= contribution(T, k, d, path + [(S.repo, S.path)], above, budget)
    return {n in got : n not matched by excl}          # this space's own excludes, last
```

`disabled` is how rule 4 reaches down: an include object deep in a
branch is checked against the excludes of every space above it on that
route. It only ever stops an include from contributing, so it can only
remove, and only on that route. A contribution therefore depends on its
route, and a resolver that memoises one keys it by (space, commit,
kinds, depth, the excludes above).

**Unreadable and dangling includes** are skipped and reported, never
widened. A skipped include contributes nothing, so the namespace can
only shrink. A Grain treats a space it could not read as **unknown**,
which is not the same as empty: while Silo's placements folder is
unreadable, the Grain starts nothing new from it, and it keeps running
the runs it has. Only a placement that is readably gone withdraws a run
(§8.8).

### 3.4 Prior art, and what is borrowed

Four existing systems solve parts of this, and the design takes ideas
from each.

- **Backstage's software catalog** (a CNCF project). Every entity is a
  YAML document with an envelope, `apiVersion`, `kind`,
  `metadata.name` and `spec`. A `kind: Location` entity points at more
  entity files by URL, through `target` or `targets`, on any git host
  Backstage has an integration for, and locations found through
  locations are followed in turn. Which kinds a location may bring in is
  set by catalog rules (`catalog.rules`, and per-location `rules`) in the
  app's config. Relations between entities are references of the form
  `kind:namespace/name`.
- **Android `repo` manifests.** A manifest's `<include name="…"/>` pulls
  in another manifest, and `<remove-project>` removes a project an
  earlier manifest declared. Local manifests are applied after the main
  one, so each layer adds to and cuts from what it was given.
- **Kustomize.** An overlay names its bases (resources), then applies its
  own patches and deletions (a `$patch: delete`, or a component that
  removes resources) to what they produced. Each layer only sees what
  its bases gave it.
- **Plan 9's per-process namespaces.** Each process builds its own view
  of the file tree by binding and mounting other trees into it, and
  union directories combine several of them. Names are local to the
  namespace that built them.

**Decision: borrow the ideas, not Backstage's letter.** The design takes:

- **a kind plus a spec and a body**: every object says `kind:` and a
  `schema:` version, as an entity says `kind` and `apiVersion`;
- **`Location`-like include objects with kind restrictions**, as
  standalone objects that follow each other transitively;
- **relations as references**: an edge is a field naming another object,
  never a separate relation record.

From `repo` and Kustomize it takes **layers that include, then cut**:
each space gathers its includes and then applies its own removals.
From Plan 9 it takes the **conceptual model**: a namespace is a union
built per Grain, from the Grain's own root, and two Grains may see
different sets of the same global objects. It does not take Plan 9's
renaming. A bind can put a tree under any name, and here a name never
changes when it is included.

**It does not adopt Backstage's envelope verbatim**, because:

- `metadata.name` is mandatory and limited to 63 characters, with no
  slashes. It cannot be a path, so adopting it would bring back the
  separate, drifting id that 4448 removed (§2.1);
- references are `kind:namespace/name`, resolved in a catalog database,
  not by location. Here a reference is an address any git client can
  follow (§4);
- the catalog ingests YAML documents, not Markdown with front matter.
  Reading these files would need a custom processor, and their bodies
  (prompts, designs) would be lost to it;
- `apiVersion` and per-kind processors are ceremony here: `schema:`
  versions each kind (§10), and the kinds are fixed;
- `relations` and `status` are computed by the catalog and live in its
  database, not in files. Here what is derived is derived by any reader,
  from git (§5.1);
- `Location` has no excludes, and its kind rules live in the app's
  config rather than on the location. Here `kinds:` is on the include,
  where the trust grant is made, and excludes are objects scoped to their
  space.

**An optional exporter, for teams with a catalog.** `silo catalog
export` (or the same function in grain) emits Backstage-compatible YAML
for the objects in a namespace, or in a repo:

- one entity per object: `kind` mapped (`task`, `template`, `schedule`,
  `design` as custom kinds, or `Resource` with `spec.type` the kind),
  `metadata.name` a slug of the path, cut to 63 characters with a short
  hash suffix where needed to stay unique;
- the source path in an annotation, `grain.dev/source:
  <git URL>//<path>`, plus Backstage's own `backstage.io/source-location`,
  so a catalog user can always get back to the file;
- relations from references: `implements` as `dependsOn` a design
  entity, `depends_on` as `dependsOn`, `repo:` as a relation to the
  catalog's `Component` for that repo when one exists;
- a `Location` per included folder, so the catalog can follow the same
  structure. The exporter applies excludes itself, because `Location`
  cannot.

The export is one-way and is never read back. **Task objects can name
catalog entities** in the other direction, as data: `catalog:
[component:default/payments]` on a task says which components it
touches, in Backstage's own reference form. Silo resolves them against
a configured catalog, to show the task on the component's page and the
component on the task's. They are labels, not references in the sense
of §4: they add nothing to a namespace, grant no read, and a task whose
entity no longer exists is unaffected.

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
  referring repo's README. It resolves to exactly the URL form. An alias
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
   (§2.1). The aliases are those of the README that governs the
   referring file, found by nearest-README discovery (§3.1): an object in
   a code repo's `.grain/` uses `.grain/README.md`'s aliases, not the
   code's README. A `/`-rooted path is rooted at the repo.
2. **Unpinned references** resolve to the tip of the target repo's
   `default_ref` at the moment they are read. Anything recorded for
   traceability records the commit sha that was actually used: a task's
   `template.rev` and `implements[].rev`, a backlog reference's
   `spec_rev`, and an execution record's `task.rev`, `from.rev` and
   `closed.design_revs`. So a reference reads live and is recorded
   pinned.
3. **Pinned references** (`@v3`, `@<sha>`) resolve to exactly that ref.
   A branch pin follows the branch's tip.
4. **Redirects.** If the target repo's README has `moved_to`, retry at
   that URL with the same path. If the file is `kind: moved`, follow its
   `to:`, resolved relative to the tombstone. Each is one hop:
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
| task → schedule of origin | `schedule: {ref, fired_at}` | written by whoever fired it |
| task → design requirements | `implements: [{ref: …#HS-6, rev, hash}]` | the requirement and its revision (§7.2) |
| task → the run that filed it | `filed_by: {grain, run: <ref>}` | an agent's upward filing (§8.12) |
| schedule → template | `template: <ref>` + `inputs` | unpinned, so a firing uses the template's current revision |
| comment → what it discusses | `run: <ref>` or `task: <ref>` | usually `../run.md` or `../task.md` (§5.4) |
| tombstone → new address | `to: <ref>` | the moved object lists the old address in `moved_from` |
| include → what it includes | `target: <ref>@<gitref>` + `ref_mode`, `kinds` | **the only edge that adds names to a namespace** (§3.3) |
| exclude → what it cuts | `names: [<ref>]`, `patterns` | subtracts from its own space only; may name what is not there |
| schedule → the schedule it was customised from | `derived_from: <ref>` | provenance only: inherits nothing (§6.2) |
| repo → its new URL | README `moved_to` | a repo-level redirect |
| task → what its sandbox holds | `context: [{ref, mode, path, depth}]` | **the only edge that materialises anything** (§5.6); a relative entry reads the task's own commit |
| comment → its attachments | `attachments: [<ref>]` | assets beside the comment (§2.2) |
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
`title`, the body, `context`, `template`, `depends_on`,
`implements`, `capabilities` or `constraints`, and any change to the
task's `attachments/` (§2.2). An attestation's
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

A repo's own backlog is the `kind: ref` files in its backlog folders
(`backlog/` by default, §3). A Grain's queue is every reference in its
namespace, from its own lane and from the lanes it includes (below).
Each reference is small:

```yaml
---
kind: ref
schema: grain/ref@v1
to: "intake:tasks/2026-10-16-approval-flow/task.md"
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

**Lanes.** A Grain's queue is every `kind: ref` in its namespace (§3.3).
They come from several sources, each with its own writer: the root's own
`backlog/`, Silo's placements for this Grain, perhaps a second team's
placements or a colleague's shared queue. **Each source is a lane**: the
folder that holds a reference is its lane, so a reference's lane does not
depend on the route that brought it. **Ranks are compared only within a
lane.** Two writers' ranks are not comparable (Silo's `t` and the
owner's `t` mean nothing to each other), so merging them would let
whichever writer picked more extreme keys win.

**Effective order within a lane:**

1. references whose task's dependencies are all satisfied (§8.9), ahead
   of those still blocked;
2. then `expedite: true` ahead of everything else;
3. then `priority`: `urgent`, `high`, `normal`, `low`;
4. then `rank`;
5. then the task's `created_at`, and finally the reference's path.

**Between lanes, the root decides**, with `lanes` in its README:

```yaml
lanes:
  mode: weighted            # strict (the default) | weighted | round_robin
  order:                    # first is highest; each entry matches its lane and the folders under it
    - {lane: "placements:grains/eu/backlog/", weight: 3}
    - {lane: backlog/, weight: 1}
  unlisted: last            # lanes not named: after the listed ones, by name (default), or ignore
  expedite: lane            # Silo's expedite reorders within its lane (default), or global
```

- **`strict`**: take the first runnable reference of the first listed
  lane that has one. A lane whose references are all blocked does not
  hold up the next lane.
- **`weighted`**: smooth weighted round-robin over the lanes that have a
  runnable reference. Each pick, every such lane gains its weight in
  credit; the lane with the most credit (ties in `order`) gives its next
  reference, and pays the total. With weights 3 and 1, Silo's lane gets
  three picks in four. The credits are runtime state, in the Grain's
  database, not in git.
- **`round_robin`**: `weighted` with every weight 1.

A Grain skips references whose task is `cancelled`, or which it already
has an open or closed run of, and picks a task referenced from two lanes
once, at whichever comes first.

**Why lane priority is declared in the root README, not on the include
objects:**

- **It is one decision about the set of lanes**, not a property of any
  one of them. Spread over include objects as numbers, it would be a
  merged rank again, across files with different writers.
- **The root's owner owns it.** An include object deep in a branch, which
  someone else wrote (Silo's folder could hold one), must not be able to
  promote its own lane. Kinds narrowing bounds *what* a branch brings,
  not how urgent it says it is.
- **A lane can arrive with no include object in the root at all**:
  through a transitive include, or as the root's own `backlog/`.
- **It is metadata, and it changes rarely**, so a README field neither
  holds objects nor becomes a contended file. The includes stay
  standalone objects, so linking and unlinking (§8.12) still touch only
  their own files, and an unlisted lane falls in after the listed ones
  until the owner places it.

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
  them. The owner's last word over their Grain is the lane order and
  their own excludes, not Silo's ranks.
- **The root's own lane** (`backlog/`): the Grain and its owner, for
  everything: the references, their ranks, adding and removing them.
- **In an intake's backlog**, the team's members own `rank` and
  `priority`. They push directly, or reorder through Silo's UI, in which
  case Silo commits with `ranked_by` set to the member it acted for.
  Silo adds and removes the intake's references.

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
task: {ref: "intake:tasks/4310-carry-approver/task.md", rev: <intake commit>}
from: {ref: "placements:grains/eu/backlog/4310-carry-approver.md", rev: <placements commit where it was read>}
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
  follow_ups: ["intake:tasks/2026-10-16-approval-flow/task.md"]
---
<optional: the closing report, in prose>
```

This file is `eu:runs/4310-carry-approver/run.md`.

```
accepted → running ⇄ waiting → done | failed | cancelled
    ↘ refused (by local policy, with refusal: {reason, at})
accepted | running | waiting → superseded (the Grain, once it sees its placement withdrawn, §8.8)
```

- **`waiting`** is a coarse state for a run held on something outside
  it: a dependency, a human's answer, or a held gate.
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
| **`code-repo`** | the target code repo's grain folder (`.grain/tasks/<name>/task.md`), committed on approval through that repo's own review and merge. The intake path becomes a tombstone | teams that want every task reviewed and versioned with the code |
| **`promote`** | the intake, until an explicit **promote** moves it into the code repo, leaving a tombstone | long-lived or design-linked work, chosen task by task |

**Configuring it.** The team's setting is in its intake's README; a
code repo's is in its `.grain/README.md`:

```yaml
# hub-intake/README.md: the team
task_home:
  mode: intake                          # the team's default
  allow: [intake, promote, code-repo]   # what a code repo may choose for itself (default: all three)
  repos:                                # per-repo overrides
    https://git.example.com/hub/widgets.git: promote

# widgets/.grain/README.md: the code repo
task_home: {mode: promote, path: tasks/}  # path: where tasks are committed, under this README
```

A bare mode (`task_home: promote`) is short for `{mode: promote}`. The
**effective mode** for an approved task is the first of these that is
set, for the code repo that is the task's primary repo, its first
`write` entry (§5.6):

1. that code repo's `.grain/README.md` `task_home.mode`, if the team's
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
- **Scheduled-task instances are never committed to code repos.** Only
  schedule definitions are (§6.2). `task_home` never applies to a task
  with `schedule:`, Silo never promotes one, and readers reject a
  `kind: task` with `schedule:` in a code repo's grain folder.
- **Backlogs and execution records do not care which mode is used.** A
  backlog reference's `to:`, a run's `task:` and a dependency's
  `depends_on` name the task by path or git URL wherever it is (§4).
- **The spec stays frozen** across the move (§5.1). The file at its new
  home is the intake's file with `moved_from` added. The only other
  change allowed is to how its references are written: an alias or a
  relative path that meant something in the intake is re-expressed for
  the new home's README, and must name the same canonical identity
  (§2.1). The spec digest is computed over canonical references, so it
  does not change. The example task's `implements:` was
  `widgets:docs/design/widgets.md#WG-2` in the intake, and is
  `/docs/design/widgets.md#WG-2` in the widgets repo.

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
  repo's checks and triggers run on it as on any change, and grain's
  merge queue lands it. In a repo whose `.grain/**` trigger requires a
  human (as bwsalmon/silo's `grain-declarations` trigger does), that is a
  human's Submit per task. That cost is the repo's choice, and one
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
neither can a rejected, cancelled or scheduled task. The target is the
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
an entry. 4446's `repo:` and `base:` on a task, template and schedule are
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
| `mode` | `read` (the default) or `write`. **`write` is valid only for a repo or a repo folder**, and means the run may push **its own task branch** to that repo, based on the entry's `@ref`, and nothing else (below). Every other target is read-only |
| `path` | optional: where it lands, relative to the sandbox's working root. Otherwise the default layout below |
| `depth` | optional, file entries only: also materialise the files this one's Markdown body links to, `n` links deep (below). The default is `0` |
| `input` | in a template only, in place of `ref`: take the reference from a template input of type `ref` (§6.1) |

**Which ref an entry reads:**

- **A relative or `/`-rooted entry** (`attachments/x.png`,
  `/designs/a.md`) reads **the task's own commit**, the `spec_rev` it was
  placed at, as a relative include reads its includer's (§3.3). So a
  task's attachments are exactly the ones that were approved.
- **An entry in another repo** reads its `@ref`. With no `@`, it reads the
  target repo's `default_ref`, or the remote's `HEAD` for a repo with no
  task-repo README, at the moment the run is materialised. Either way the
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
  `.git` (`silo`, `grain`), as for scheduled firings (§6.2). If two
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
    - {ref: "shared:designs/hub-intake.md", mode: read, rev: <commit>, path: "context/silo.example.com/git/hub/shared/designs/hub-intake.md", bytes: 4912}
    - {ref: "shared:designs/img/claims-flow.png", via: "shared:designs/hub-intake.md", mode: read, rev: <commit>, path: "context/silo.example.com/git/hub/shared/designs/img/claims-flow.png", bytes: 3412}
    - {ref: "intake:tasks/2026-10-16-reject-dangling-refs/attachments/dangling-ref-error.png", mode: read, rev: <commit>, path: context/screenshot.png, bytes: 2210}
  skipped:
    - {ref: "../../hub.md", via: "shared:designs/hub-intake.md", reason: dangling}
  limits: {reached_files: 1, reached_bytes: 3412}
```

#### Only `context:` materialises

Tasks hold many other references, and **none of them pulls anything into
a sandbox**: `depends_on`, `template`, `implements`, `supersedes`,
`schedule`, a backlog reference's `to`, `moved_from`, `filed_by`, a
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
  store's credential against `capabilities`;
- LFS content against `repos:` and `network` for the LFS endpoint's host.

The Grain resolves outside the sandbox, with its own credential, into a
staging area. A file it must read to find links is fetched only after it
has passed. **Nothing enters the sandbox until the whole set has
passed**, and then the git proxy's scope is set to exactly that set.
This generalises grain task 4303's fix, which made the policy check the
repositories a capability implies: no repo, file or URL reaches a sandbox
unseen by policy, and **a capability no longer implies a repo**. A
`repo-write:<repo>` capability becomes a `write` entry (§13).

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

#### Templates and schedules contribute entries

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
  schedule, or the task for a task filed in Silo's UI. The API requires a
  URL or an alias of the intake's README.
- **A schedule** supplies the values in its `inputs:`, and may add
  entries of its own in its own `context:`, after the template's.
  `prune-packages`'s schedule sets `repo:` to grain at `candidate`
  (§8.16).
- **A filed task records the result.** Filing, by hand, by a schedule or
  by planning, writes the concrete `context:` into the task, every
  `input:` replaced by its value, and `template.inputs` records the
  values used. The task's `context:` is what the approver reads, what
  `spec_digest` covers, and what the Grain's policy checks. The template
  is never consulted again.
- **In a code repo's grain folder,** a template's or schedule's entries,
  and its `ref` inputs' defaults and values, may name only that repo.
  Readers refuse anything else, as they refuse `reads` and `writes`
  there today (§12.1). An adopting team that wants another repo
  supplies it from its own schedule.

#### One resolver, two consumers

Includes (§3.3) and `context:` use the **same reference resolution**:
canonicalisation, aliases from the governing README, refs and pins,
redirects with the hop limit, cycle detection, and dangling handling
(§4). They differ in what they build:

| | Includes | `context:` |
| --- | --- | --- |
| builds | a **Grain's namespace**: the names it acts on | a **sandbox's view**: what one run sees and may write |
| accepts | objects, narrowed by `kinds` | repos, folders, objects and assets, with a mode |
| transitive | always, with excludes and a depth limit of 8 | off, unless an entry sets `depth` (at most 3), over body links only |
| on a bad target | skip the branch; the namespace only shrinks | a listed entry refuses the run; a reached file is skipped |
| gate | the reader's credential (§11.18) | the reader's credential **and** the Grain's policy, before each fetch |
| written by | the root's writers | the task's author, frozen on approval |

So one package, `taskrepo`, has one `resolve` with a gate supplied by its
caller, and two consumers:

```
resolve(ref, from, cred, gate):                 # §4; shared by namespaces and sandboxes
    id   = canonicalise(ref, from)              # alias, relative path, /-rooted; keeps @ref
    seen = []
    loop at most LIMITS.hops times:
        if id in seen: return dangling("redirect loop")
        seen += id
        if not gate(id): return refused(id, gate.rule)   # before any read of id's repo
        if id.repo's README has moved_to: id = id at moved_to; continue
        t = stat(id, at id.ref or id.repo's default_ref, cred)
        if t is missing or unreadable: return dangling(t.reason)
        if t is a kind: moved object: id = canonicalise(t.to, from = id); continue
        return Target(id, commit = t.commit,
                      what = repo | folder | object(t.kind) | asset)
    return dangling("more than 8 redirects")

# Includes (§3.3): gate = always true; the credential is the gate.
# contribution(...) calls resolve(i.target, i, reader's credential, ALLOW).

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
    record run.context = {entries: plan with commits and paths, skipped, limits used}
```

`refuse` is the record of the PEP section above: `refused` for a placed
task, `waiting` for one from the Grain's own lane. Silo uses the same
resolver to show a task's context in its views and to check it on push,
with the pusher's rights as the gate (§8.1).

## 6. Templates and schedules


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

### 6.2 Schedules

A schedule's front matter carries the cadence exactly as grain writes it
today (`daily: {at: "09:00"}`, `weekly: {day: sunday, at: "04:00"}`,
`overlap`, `timezone`, `enabled`, `team`, `auto_approve_proposals`),
plus `template: <ref>` and `inputs`, `fires_in` (`silo` or `grain`), and
optionally `context:` entries of its own and `derived_from` (below). Its
body is optional notes. Grain's `target` and `base` on a schedule become
the value of the template's `ref` input (§5.6, §13).

**A firing creates a task and a reference to it**, at a path derived from
the schedule and the firing time, so a retried firing writes nothing new:
`tasks/scheduled/<schedule>/<fired_at as yyyymmddThhmmZ>/task.md`.

- **A `fires_in: grain` schedule in a Grain's namespace fires on that
  Grain**, locally: the Grain renders the task into its own root, adds a
  reference to it in its own lane, and runs it, so it never needs Silo.
  That covers the Grain's own schedules and any it includes (§3.3).
  A schedule from another repo fires at
  `tasks/scheduled/<repo name>/<schedule>/<fired_at>/task.md` in the root,
  so two repos' schedules of one name never collide.
- **A team schedule is assigned to a Grain by an include in Silo's folder
  for it** (`placements:grains/us/schedules/hub.md`, including
  `shared:schedules/`), which the Grain's root includes in turn
  (`schedules/silo.md`, `kinds: [schedule]`). Silo assigns each
  `fires_in: grain` schedule to one Grain, and moves the include to
  re-assign it. Silo never copies the schedule.
- **A `fires_in: silo` schedule fires in Silo** for the team it names.
  Silo renders the task into the team's intake, approved if the
  schedule's approval policy allows it, and places it by adding a
  placement (§8.12). If no Grain can take it now, Silo adds a reference
  to the intake's backlog instead, and the task waits there in the team's
  order. A Grain whose namespace includes such a schedule lists it, and
  never fires it.

**Approval of a Grain's firings.** A firing in a Grain's root is the
Grain's own task. It is filed `approved` (with `approved_by:
grain:<id>`) only if the schedule says `auto_approve_proposals` **and**
the schedule is the root's own, or reached through includes that are all
pinned (`ref_mode: pin`), so that what fires is exactly what the owner
reviewed. Through a tracking include it is filed `proposed`, and the
Grain's owner approves it, or the Grain's local policy does.

**Customising an included schedule** is an exclude plus a new object.
Grain `us` is assigned the team's daily `shared:schedules/granule-pull.md`
and wants it weekly. It does not edit or override it: it excludes it
(`schedules/no-granule-pull.md`) and adds its own
`schedules/granule-pull-weekly.md`, a whole, self-contained schedule
whose `derived_from` names the original. `derived_from` is a reference:
it adds no name and inherits nothing, and Silo's views use it to show the
two diverging when the team changes the original (§8.15).

`overlap` reads the previous firing's derived status (§5.1): `skip`
skips a firing while the last one is unplaced, queued or has an open run.

**Schedules in code repos.** A schedule's *definition* may live in a
code repo's grain folder, beside the code it is about
(`widgets:.grain/schedules/weekly-release-notes.md`). Its *instances*
never do:

- **In Silo**, a schedule that Silo does not host fires for a team only
  if that team **adopts** it, with a `kind: include` of it in its intake
  (`hub-intake/schedules/widgets.md`, `kinds: [schedule]`, written by the
  team's admins). A code repo cannot make a team's intake receive firings
  by naming the team. The instances go into the adopting team's intake,
  at `tasks/scheduled/<repo name>/<schedule>/<fired_at>/task.md`, where
  `<repo name>` is the last element of the code repo's URL without
  `.git`. So two code repos' schedules of the same name never collide.
  A firing is filed `approved` only if the schedule says
  `auto_approve_proposals` **and** the adopting include is **pinned** to
  a commit the admins reviewed, at which the template it names also
  resolves (a relative template reference reads that same commit).
  Otherwise it is filed `proposed` (§11.13). A pin replaces 4450's
  `auto_approve: {digest}`: it fixes the same bytes, and taking a change
  is re-pinning, one line.
- **On a Grain** whose namespace includes a code repo's schedule, a
  firing goes into the Grain's own root as above, or is an **inline
  firing**: a
  backlog reference whose `to:` is the schedule, with `fired_at`, and
  then the execution record. No task file is written, because the task is
  exactly the schedule rendered at that time. `spec_rev` pins the
  schedule's commit, and with it the template's, so an inline firing is
  allowed only when the template is in the same repo as the schedule.

**Why instances are kept out of code repos.** A daily schedule fires 365
times a year. In a code repo, each firing would be:

- a commit in the code's history, showing up in `git log`, blame and
  bisect;
- a change through the repo's review and CI, and, in a repo whose
  `.grain/**` needs a human, a human's Submit every day;
- a review of nothing, because an instance is the schedule plus a time,
  rendered deterministically.

What deserves review is the definition, which changes rarely, so that is
what lives with the code. `task_home` never applies to a firing, and
readers reject a `kind: task` with `schedule:` in a code repo's grain
folder (§5.5).

**Runtime state is not committed.** `next_run_at`, `last_run_at` and
the overlap bookkeeping stay in the database. The task each firing
produces records `schedule.fired_at`, so the history of firings is in
git anyway.

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

### 7.4 Planning is a Silo operation

When a push to a design's `default_ref` is accepted, Silo diffs every
requirement between the old and new tips. It compares sections by ID and
uses the hashes of §7.2. Then it acts, following the planner's existing
table (bwsalmon/grain `docs/design-docs.md`):

| Class | Action (files go to the planning team's intake) |
| --- | --- |
| new | an implementation task plus a test task that depends on it, `state: proposed` |
| changed, task still open | a proposal superseding the open task (`supersedes: <ref>`). Approving it makes Silo cancel the old task, which its Grain honours (§8.7) |
| changed, done | a delta task covering only the changed text |
| removed, open | a proposal to cancel the open tasks. Nothing is cancelled without approval |
| removed, done | a removal task, whose commit carries `Design-Removed:` |
| unchanged | nothing |

**Idempotency.** Each planned task's path is derived from (design path,
requirement ID, revision hash, action):
`tasks/planned/<design name>/<HS-6>-<action>-<first 8 hex of the hash>/task.md`.
Re-running planning on the same push finds every path already there, and
writes nothing new.

**Flood control:**

- At most `max_tasks_per_change` tasks are filed per push (default 10).
  A larger plan files **one** plan proposal listing every intended task.
  Approving it files them.
- Planning is capped per team per day (default 50).
- Planned tasks are always `proposed`, unless the design says
  `approval: inherit` **and** the pusher whose change was merged is an
  approver for the planning team.
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
pushed commit, not only the tip, against the repo's README `writers`,
for the pusher identity that authentication established.

**One writer makes the hook simple.** Every Silo-hosted repo but an
intake names its writer as a list of principals: Silo for its placements,
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
   field). It then applies the rules. Any file not covered by a rule
   refuses the push, with a message naming the file, the field and the
   rule that was missing.
3. **Paths.** Identity is the path (§2.1), so:
   - a `kind: moved` file can never be changed or deleted, so no new
     object can be created at a tombstoned path;
   - only `kind: ref`, `kind: include` and `kind: exclude` files, and
     assets outside a frozen task's `attachments/`, may be deleted. Any other object leaves its path only by becoming a
     tombstone (in an intake, under a `may: move` rule);
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
   - An include must name a `kinds:` list, and a target in another repo
     must carry its `@ref` and `ref_mode`; a `track` include names a
     branch, and a `pin` a tag or a commit. Its target must resolve with
     the pusher's rights when it is added (§11.4). An exclude must
     canonicalise.
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
   backlogs, runs, each Grain's namespace) and its reverse indexes (§4).
   It sends `pull` notifications to the Grains whose namespaces include
   what changed, removes the placements and intake references of tasks
   that just closed (§8.5), and triggers planning (§7.4).

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

1. The Grain fetches its root and the repos its includes name (prompted
   by the notify, and also every few minutes in case a notify was
   missed), and re-resolves its namespace (§3.3). It finds a new
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
- **The owner reweights the lanes:** edit `lanes` in the root's README.
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
  transitions against the README `writers` rules. It cannot check who
  pushed. Where the README says `require_signed: true`, it also checks
  commit signatures against the `keys` listed for each principal (§11.8).
- **Placed tasks.** A Grain runs only tasks referenced from a lane of
  **its own** namespace: its root's own lane, or a folder its root
  includes with `kinds` covering `ref`. It never runs a task merely
  because a foreign repo contains one, ranks one, or references one.
- **Foreign content is claims.** Templates, includes and designs from
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
- a placement in a folder no Grain's root includes yet (link in
  progress): Silo does not place there until the include exists, and a
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
| link | Silo's folder for the Grain (created, if new) → the Grain root's include of it |
| unlink | the Grain root (include deleted) → Silo re-places what had no run |
| move across repos | new home (with `moved_from`) → tombstone at the old path → Silo rewrites references' `to:` |
| upward filing | intake (proposed, committed by Silo) → the Grain's comment on its run naming it |
| schedule firing in Silo | intake (approved task, deterministic path) → placements (reference) |
| schedule firing on a Grain | Grain root only: the task (deterministic path) and its reference in the root's lane, in one commit |
| planning | intake proposals and their references (deterministic paths) → design's `planning.last_rev` recorded in Silo's DB |
| definitions via review branch | `review/*` branch → the approver's merge commit on `main` |
| home a task in a code repo (`code-repo` on approval, or `promote`) | code repo (the task with `moved_from`, by direct push or a merged change) → intake (tombstone) → Silo rewrites `to:` and `spec_rev` in the backlogs it hosts |
| cancel a task homed in a code repo | placements (references removed, which fences open runs) → code repo (`cancelled`, by direct push or change) |

**Definitions** (templates, schedules and designs) are pushed straight
to `main` by maintainers, or through **review branches**. Anyone with
`create` rights may push to `review/*`. The hook accepts a commit on
`main` that touches a definition only if one of these holds:

- the pusher is a maintainer;
- the commit is a merge whose second parent is a `review/*` tip, and the
  merge's pusher is an approver other than the branch's author.

On receive, Silo checks schema and references, and plans designs.

### 8.12 The remaining worked flows

**An agent's upward filing.** An agent on Grain `eu` calls
`propose_task` for another team's repo. The Grain does not push to that
team's intake, because it never writes other repos. It asks Silo, over
its node connection, as `propose_task` does today. Silo checks that the
Grain has an open run whose task has that submitter, then commits to the
team's intake a new task with:

- `state: proposed` and `submitted_via: agent`;
- `submitter`: the human who submitted the originating task;
- `filed_by: {grain: eu, run: "eu:runs/4310-carry-approver/run.md"}`.

Silo returns the new task's path. The Grain writes a comment on its run
naming it, and lists it in `closed.follow_ups` when the run closes.

Commits:

- **intake (other team)**, 1, by Silo;
- **eu**, 1, by the Grain: the comment on its run.

It then needs approval like any proposal. A standalone Grain files into
its own repo instead.

**A schedule fires a templated task.** The `prune-packages` schedule in
`hub/shared` fires at 04:00 on Sunday. Silo renders the template it
names at its current tip `<sha>`, and commits `tasks/scheduled/prune-packages/20261011T0400Z/task.md` to
the team intake with:

- `state: approved` and `approved_by: silo:schedule`. This is allowed
  only because the schedule's `auto_approve_proposals` was itself
  approved when the schedule merged;
- `template: {ref, rev: <sha>, inputs: {}}`;
- `schedule: {ref, fired_at}`.

Silo then places it, by adding a placement to a Grain's folder. Commits:

- **intake**, 1;
- **placements**, 1.

If no Grain can take it, the second commit is a reference in the
intake's backlog instead.

**A design edit triggers planning.** The `review/hs-approval-by-push`
branch adds `### Approval is a push {#HS-6}` and edits HS-3. An approver
merges it to `main` in `hub/shared`, and the hook accepts it under
§8.11. Post-receive classifies the requirements:

- HS-6 is **new**;
- HS-3 is **changed, done**;
- the rest are unchanged.

Silo commits three proposals to `hub/intake` in one commit, at the
deterministic paths of §7.4:

- implement HS-6;
- test HS-6;
- a delta for HS-3.

Each has `implements: [{…#HS-n, rev, hash}]`. The same commit adds an
intake reference for each, ranked in plan order (the implementation
ahead of its test) after the team's current last reference. Commits:
**intake**, 1. Approving each one then follows §8.3 and §8.4.

**Link, then unlink, a standalone Grain.** Linking is an include, and
unlinking is deleting it.

- **Link:** `grain hub login`, then `grain tasks link --team hub`.
  1. Silo creates the Grain's folder in the team's placements repo,
     `grains/<id>/`, if it is new. If the Grain's root is a local repo,
     Silo also creates a hosted repo for it, `grains/<id>.git`, whose
     README lets only the Grain and its owner write, and the Grain pushes
     its local history there. This first push is allowed for the
     owner's credential only, and only into an empty repo. A root that
     already lives where Silo can read it stays where it is.
  2. The owner's commit in the root, which `grain tasks link` makes, adds
     `backlog/silo.md` (`target: "placements:grains/<id>/backlog/@main"`,
     `ref_mode: track`, `kinds: [ref]`) and, for a Grain that runs team
     schedules, `schedules/silo.md` (`kinds: [schedule]`), plus the
     `placements` alias and the Silo lane's place in `lanes`.
  3. Silo reads the root, sees the include, and from then on places work
     on the Grain.

  The Grain's objects keep their paths, so every reference inside the
  root still resolves. If the root moved from `file://` to the hosted
  URL and something outside referenced it, the local README gets
  `moved_to`. Commits: **placements**, 0 or 1 by Silo; **Grain root**,
  its history (if newly hosted) plus 1 by the owner.
- **Unlink:** `grain tasks unlink`, or deleting `backlog/silo.md` by
  hand.
  1. The owner's commit deletes the include objects. From that commit the
     Grain's namespace has no placements, so it starts nothing from Silo,
     and nothing Silo writes can reach it.
  2. Silo sees the include gone. It re-places the placements that have
     no run (§8.8), each a move out of this Grain's folder. Open runs of
     placed tasks must close first; with `--force` the owner unlinks
     anyway, and those runs are fenced as their placements move.
  3. Silo stops placing on the Grain, and leaves its folder as an archive,
     so references into it (`from`, `previous`) still resolve.

  The root, with all of its history, stays the Grain's. A Silo-hosted
  root becomes a remote the Grain may keep pushing to, or the owner sets
  `moved_to` and goes back to a local repo. Commits: **Grain root**, 1 by
  the owner; **placements**, 1 per re-placed task.

### 8.13 Code repos: write authority is the repo's own

Silo's pre-receive hook (§8.1) runs only on the repos Silo hosts. In a
code repo, on GitHub, Gerrit, a depot or anything else, **write
authority comes from that repo's own host and review process**: whatever
its branch protection, required reviews, path triggers and merge queue
let land on `default_ref` is what the repo says. Silo neither proxies
nor mirrors those pushes. The grain folder's README says `host: other`,
and its `writers` rules describe what belongs there, but nobody can
check who pushed. Readers treat a code repo's objects like this:

- **Trust follows the repo's merge process.** An object on the code
  repo's `default_ref` is trusted exactly as much as code merged beside
  it: no more, because Silo did not check its pusher, and no less,
  because the repo's own review did. A repo that wants its grain folder
  reviewed more strictly than its code says so with a path trigger on
  `.grain/**`, as bwsalmon/silo already does.
- **Silo validates on read.** Silo reads a code repo when one of its
  repos references it, on the host's push webhook where there is one,
  and otherwise on its next pass. For each changed file in a grain
  folder's scope, it checks what the hook would: schema, the path rules
  (§2.1), the README's `writers` rules on read (§8.10), references
  (§4), design ID allocation (§7.1), and the invariants of §5.5. It
  resolves references from a code repo with the rights of whoever is
  using them, never with its own (§11.4). An object that fails is shown
  in the repo's health view, and as a comment on the change if Silo
  opened it, and is not used. Silo never writes to a code repo except
  through §5.5's direct push or change.
- **Grains are the PEPs, as everywhere.** A Grain runs a task from a
  code repo only through a placement in its namespace, whose
  attestation's `spec_digest` must match the file (§11.7). Its local
  policy may limit the code repos it reads templates, includes and
  designs from. Definitions are data that an accepted task may use; they
  grant nothing (§8.10).
- **What a code-repo object can do is bounded by the design, not by its
  README.** Anyone who can merge to the repo can edit its
  `.grain/README.md`, so its `writers` rules are advisory, and the
  limits below hold whatever it says:
  - a task there is placed only with an intake approval behind it (§5.5);
  - a template, schedule, suite or trigger carries no `reads`, `writes`,
    `grants` or `target` (§12.1);
  - a schedule fires for a team only if the team adopted it (§6.2);
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
`intake:tasks/4310-carry-approver/task.md` throughout.

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
| a member confirms | 1, by Silo: a comment naming the pending change | 1 commit on `silo/task-home/2026-10-16-widget-schema-v2`, then the merge queue's merge on `main`, after the repo's `.grain/**` trigger is satisfied | — |
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

### 8.15 Worked flows for namespaces

Placement and re-placement through Silo's folder are §8.4 and §8.8, and
linking and unlinking are in §8.12. The rest, each with the commits it
makes:

**An included team schedule fires.** Grain `us`'s root includes
`placements:grains/us/schedules/` (`kinds: [schedule]`), and Silo's folder
there includes `shared:schedules/`. At 06:00 on 2026-10-16, before the
customisation below:

1. `us`'s namespace holds `shared:schedules/granule-pull.md`, reached
   through two includes, both tracking `main`. The schedule says
   `fires_in: grain`, so `us` fires it. (It also holds
   `shared:schedules/prune-packages.md`, which says `fires_in: silo`:
   listed, never fired here.)
2. `us` renders the template the schedule names, at its tip, and commits
   to its own root, in one commit,
   `tasks/scheduled/shared/granule-pull/20261016T0600Z/task.md`
   (`template.rev`, `schedule: {ref, fired_at}`) and a reference to it in
   `backlog/`. The includes are tracking and the schedule says
   `auto_approve_proposals: false`, so the task is `proposed`, and a
   team admin approves it in `us`'s root.
3. It runs from the root's own lane like any local task.

Commits: **Grain root**, 1 (the firing), 1 (the approval), then the run's
own. **Silo**: none. A retried firing finds the path taken and writes
nothing.

**Customising an included schedule.** The team admins want `us` to pull
weekly instead. One commit to `us`'s root adds:

- `schedules/no-granule-pull.md`, `kind: exclude`, naming
  `shared:schedules/granule-pull.md`;
- `schedules/granule-pull-weekly.md`, a whole schedule (`weekly: {day:
  monday, at: "06:00"}`) with `derived_from:
  "shared:schedules/granule-pull.md"`.

The root's exclude is applied last, so the daily schedule leaves the
namespace whatever route brought it, and the weekly one, a name of its
own, enters it. Nothing in `hub/shared` or in Silo's folder changes, and
no override rule decides between them: there is only one of them in the
namespace. Commits: **Grain root**, 1.

**Disabling a transitive branch.** Grain `eu` includes
`shared:templates/` (pinned at `2026-10-16`), which includes
`org:templates/@v3`. Suppose `eu`'s owner wants the team's templates
without the org's. Its root adds one exclude, naming the include object:
`names: ["shared:templates/org.md"]`. That disables the include (§3.3,
rule 4): everything it contributed drops out of `eu`'s namespace, and so
does its name, while the team's own templates stay. If `eu` also included
`org:templates/` directly, those templates would stay, because that
route is a contribution of its own. Commits: **Grain root**, 1. The same
move in miniature is `ns-demo/v/`.

**The X/Y/Z case.** `ns-demo/x/` includes `../y/` and excludes
`../y/templates/greeting.md` (Y). `ns-demo/z/`, a Grain root, includes
`../x/` and `../y/templates/greeting.md`.

```
contribution(y/)              = {greeting}
contribution(x/)              = ({note, include-y, exclude-y} ∪ {greeting}) − {greeting}
                              = {note, include-y, exclude-y}
contribution(greeting, file)  = {greeting}
namespace(z)                  = {include-x, include-y} ∪ contribution(x/) ∪ {greeting}
                              ∋ greeting
```

X's exclude cut Y from X's contribution only. Z's direct include is a
new contribution, so Z's namespace contains Y. `ns-demo/w/`, which
includes Y and excludes it, contributes none.

**A Grain picks its next task across lanes.** Grain `eu` has two lanes
with runnable references, and `lanes: {mode: weighted}` with Silo's
placements at weight 3 and its own `backlog/` at 1:

| Lane | Its order (§5.2) |
| --- | --- |
| `placements:grains/eu/backlog/` | `2026-10-16-reject-dangling-refs` (expedite), `2026-10-16-approval-flow` (`high`, rank `t`), `2026-10-16-widget-schema-v2` (rank `w`); the prune firing is skipped, because `eu` has an open run of it |
| `backlog/` | `2026-10-16-tidy-run-notes` (rank `i`) |

Credits start at 0. Pick 1: Silo's lane has 3, the root's 1, so Silo's
lane gives `reject-dangling-refs` and pays 4 (−1 against 1). Pick 2: 2
against 2, a tie, which `order` breaks for Silo's lane: `approval-flow`
(−2 against 2). Pick 3: 1 against 3: the root's lane gives
`tidy-run-notes`. Pick 4: only Silo's lane is left: `widget-schema-v2`.
`check.py` prints the same: `next on eu (weighted):
2026-10-16-reject-dangling-refs, 2026-10-16-approval-flow,
2026-10-16-tidy-run-notes, 2026-10-16-widget-schema-v2`. Under
`mode: strict` with the root's lane first, `tidy-run-notes` would go
first and Silo's three after it. Nothing is written to pick: the
choice shows up as the next run's `accepted` commit in the root.

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
   and is 3,412 bytes, inside the caps: it is planned, `via` the doc. The
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
pull (`us:schedules/granule-pull-weekly.md`) fires on Grain `us`, and its
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
`context: [{input: repo, mode: write}]`. Its schedule,
`shared:schedules/prune-packages.md`, says `inputs: {repo:
"https://github.com/bwsalmon/grain.git@candidate"}`. When it fires in
Silo:

1. Silo checks the value against §4's grammar and `ref_kind`: a repo, with
   a branch. A value naming a file, or `@<tag>`, files nothing and is
   reported on the schedule.
2. Silo writes the task at
   `intake:tasks/scheduled/prune-packages/20261011T0400Z/task.md` with
   `context: [{ref: "https://github.com/bwsalmon/grain.git@candidate",
   mode: write}]` and `template.inputs.repo` recording the value. No
   `input:` survives into a task.
3. The placement's `spec_digest` covers that `context:`, and the Grain's
   policy checks it like any task's. Another team's schedule can use the
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

## 9. How the task interface reduces


| hub-interface.md today | With task repos |
| --- | --- |
| `hello` / `welcome` / `heartbeat` / `goodbye` | unchanged (the registry and slots stay live) |
| `offer` (task + attestation) | Silo's commit of a placement (with `attestation`) in the Grain's folder of its placements repo + `{"type":"pull"}` |
| `resume` | gone: the placement is in Silo's repo, the Grain's root includes it, and a returning Grain pulls |
| `accept` / `refuse` | the Grain's commit of an execution record in `accepted` / `refused` + `refusal` |
| `result` | the Grain's commit of the record's `done\|failed` + `closed` |
| `cancel` | Silo's commit of `cancelled` + `cancel` on the task, + `pull`; the run's `cancelled` commit |
| re-placement (today a `cancel` to one Grain and an `offer` to another) | Silo's one commit moving the placement between Grains' folders; the old run is fenced |
| a user's reply to a running agent | relayed over the node connection; the Grain commits it in its root |
| `propose_task` (upward filing) | unchanged as a request; Silo makes the intake commit |
| event stream, view stream, http stream | unchanged (runtime; never in git) |

The contract becomes:

- the repo schema (§3, §10);
- namespace resolution: include and exclude objects, and lanes (§3.3,
  §5.2);
- the layout of Silo's folder for a Grain: `backlog/` and `schedules/`;
- a task's `context:`, its resolution and limits, and the sandbox layout
  (§5.6);
- the `pull` notification.

A customised Grain conforms by reading and writing those files. Until
stage 4 of §16, both paths run, and the WebSocket messages are derived
from the commits.

## 10. Schemas and versioning

Each kind has a JSON Schema (2020-12) in
[`examples/task-repos/schemas/`](../examples/task-repos/schemas/):
`common` (shared definitions), `repo`, `task`, `ref`, `run`, `comment`,
`template`, `schedule`, `suite`, `trigger`, `design`, `moved`, `include`
and `exclude`. `repo` covers a grain folder's README as well as a root
one, with `scope` and `task_home` (§3, §3.1, §5.5), and a Grain root's
`grain`, `lanes` and `include_limits` (§3.2, §5.2). Its `writers` is a
list of principals, or, for an intake, the list of rules. An intake's
`schedules:` is gone: adoption is an include (§6.2). `ref` gains
`fired_at` for inline firings (§6.2). `schedule` gains `derived_from`.
`run`'s `superseded` is written by the Grain and records `withdrawn_at`
(§8.8). `include` has `target`, `ref_mode`, `kinds` and `max_depth`, and
no `transitive` field, because includes are always transitive (§3.3).
`task` and `template` replace `repo` and `base` with `context` (§5.6), and
`template` inputs gain `type: ref`; `schedule` drops them too, and may
carry `context`. `run` gains `context` (what was materialised and
skipped) and `waiting`, and `refusal` gains `entry`. `comment` gains
`attachments`. `asset` is new: a sidecar (§2.2). The shared definition of
a `context:` entry is in `common`.

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
  and `schedule`, the loader also accepts the hyphenated keys of
  `.grain/` declarations (`auto-merge`, `auto-approve-proposals`) as
  aliases, so existing declarations move over verbatim (§12.1).

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
   - `capabilities: [gcp-key, repo-write:bwsalmon/grain]` and
     `secret: github-app/app-id` are **names**. They resolve only in the
     Grain's local policy and secret store, or in Silo's grant table.
   - A capability in a task file is a **request**. Its grant is Silo's
     claim, carried in the attestation's `granted_capabilities`, and the
     Grain's local policy has the last word.
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
   - The README lists each principal's SSH or GPG signing keys.
   - Approvers sign their approval commits.
   - Grains verify that a task's `approved_by` change is in a commit
     signed by that approver's key.

   That makes approval verifiable end to end, with no need to trust Silo,
   which the JWT alone cannot do. It also covers repos on other hosts.
9. **A rogue writer** can only do what its rules allow. Examples:
   - a compromised Grain can change only its own root: its execution
     records, its own lane and its namespace. It cannot write any task,
     so it cannot rewrite what it was asked to do. It cannot write a
     placement, so it cannot claim it was asked, and it cannot write
     another Grain's records, so it cannot claim another Grain's work;
   - Silo, or a credential for it, can write placements and the intake,
     and nothing in any Grain's root. It cannot rewrite a Grain's history,
     forge its runs, or reach past the `kinds` each Grain's include of
     its folder accepts;
   - a compromised member can reorder the team's unplaced work, and
     nothing else;
   - a compromised approver can approve, and the commit names them.

   Every write is an attributable commit, and the hook logs every refusal
   to the audit log.
10. **A malicious design edit** can file at most a capped number of
    *proposed* tasks, which still need approval (§7.4). Approval itself
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
    - The design's limits do not depend on the folder's README, which
      anyone who can merge may edit (§8.13).
    - Grains apply local policy as PEPs. A placed task's spec must match
      the attestation's digest, and a Grain's policy may refuse code
      repos it does not trust for templates, includes or designs.
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
    - **Edit a template or include.** A task already filed pins
      `template.rev`. A schedule renders its template's tip, so a firing
      after the edit uses it. That firing is filed `proposed`, and an
      approver reads the rendered prompt, unless the team's adopting
      include is pinned to a reviewed commit: then an edit does not reach
      the team at all until an admin re-pins it. What remains is prompt injection, bounded by
      local policy (item 5).
    - **Grant itself access.** A template, schedule, suite or trigger
      with `reads`, `writes`, `grants` or `target`, or with a `context:`
      entry or `ref` input naming another repo, is refused (§12.1). A
      capability named in a homed task is a request, granted only in
      Silo's attestation (item 6).
    - **Flood a team.** A schedule naming a team the team has not adopted
      never fires (§6.2). Firings never land in the code repo itself. A
      design edit files at most a capped number of proposals (item 10).
      Silo reads at most 1,000 changed files per code-repo push, as the
      hook caps pushes (§14).
    - **Widen its own rules.** It can edit `.grain/README.md`'s
      `writers`, which are advisory there, and its `task_home`, which the
      team's `allow` bounds.
    - **Reference a private Silo repo** to make Silo reveal it. Silo
      resolves code-repo references with the rights of the principal
      using them, never its own (item 4).
    - **Cancel a homed task.** That is honoured, because it only stops
      work, and the commit names who merged it.

    The repo's own defence is to review `.grain/**` as strictly as it
    reviews code that touches credentials, which bwsalmon/silo already
    does with a path trigger that requires a human (`grain-declarations`).
14. **An include is a trust grant.** Adding one says "act on what this
    folder holds, of these kinds":
    - **`kinds:` narrows it**, and narrowing is intersected down every
      path, so nothing reached through an include can bring a kind its
      includer did not accept. Silo's placements come in as `kind: ref`
      only: a compromised placements folder cannot add a schedule to a
      Grain, or a template, or another include that widens anything.
    - **The local policy still applies** to everything that arrives. A
      placement is still a claim with an attestation (item 7); a schedule
      that fires still files a task the Grain's policy may refuse; a
      template is still data (§8.10). An include decides what a Grain
      *considers*, never what it is *allowed*.
    - **Only `kind: include` widens a namespace**, and only the root's
      writers can add one to the root. A reference names anything, but
      adds nothing (§3.3), so a task cannot smuggle a folder into a
      namespace by referencing it.
15. **Excludes are scoped, so nothing included can hide anything else.**
    An exclude subtracts only from the contribution of the space that
    holds it (§3.3). A folder you include can hide its own objects from
    you, which it could do anyway by deleting them, but it cannot hide
    an object you hold yourself, or one another include brings: a
    global, unscoped exclude could have hidden a placement, or one of
    the root's own tasks, from anywhere in the graph. Silo's
    hook and readers therefore need not trust an included folder's
    excludes at all.
16. **The root's excludes are the final word.** They are applied last,
    to everything, so the root's owner can always cut anything that
    reached them, however deep. Excluding an include object cuts its
    whole branch (rule 4). There is no re-include, so no later layer can
    undo a cut. An exclude that cannot be read fails its space closed
    (contributes nothing) rather than open.
17. **Malicious include chains are bounded.** An attacker who controls a
    folder you include can put more includes in it. It cannot:
    - **go deep:** the depth limit (8, counting down; `max_depth` and the
      root's `include_limits` only lower it) cuts the chain;
    - **loop:** a cycle is detected on the path and cut. Cutting loses
      nothing, because whatever is on a cycle is already in the union
      of an ancestor, and it cannot re-add a name an ancestor cut;
    - **fan out:** at most 64 includes in one space (more fails that
      space closed), 256 spaces, 32 distinct repos and 50,000 names per
      namespace. Over a global limit, resolution fails and the Grain
      keeps its last good namespace, so the limits are deterministic,
      not first-come;
    - **widen:** every level intersects `kinds`, and an unreadable or
      dangling include is skipped, never replaced by something broader.
18. **Following an include is a read, with the reader's own credential.**
    A Grain follows includes with its node credential, Silo with the
    rights of the principal the namespace belongs to, and nobody with
    Silo's own. An include of a repo the reader cannot read is reported
    and skipped (item 4). Silo grants a Grain read on its own folder in
    the team's placements repo, and on what its placements need (item
    4); it never grants read because an include names a repo. The hook
    checks a new include's target with the pusher's rights, so an
    include cannot be used to make Silo reveal a repo, and Silo resolves
    a Grain's namespace for its views with the Grain's grants, not its
    own. Read access is per repo, so a team's placements repo is
    readable by all the team's Grains (§8.12); a deployment that must
    isolate Grains from one another gives each its own placements repo.
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
    made general: a capability no longer implies a repo, so there is no
    second route by which a repo reaches a sandbox.
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


## 12. Grain folders in code repos: `.grain/` and `docs/design/`

bwsalmon/grain's `.grain/` declarations (`docs/declarations.md`,
`docs/declaration-schema.md`) **converge on the task-repo format**: each
becomes a Markdown file with front matter, read by the same parser as
every other object, in a grain folder governed by `.grain/README.md`
(§3.1). Workflows are the one exception (§12.2). The keys do not change:
each kind carries exactly the keys `pkg/declschema` defines for it today,
and this design adds and removes none. The loader accepts the hyphenated
spellings (`auto-merge`) as aliases of the `snake_case` ones (§10).

### 12.1 The mapping

| `.grain/` today | Becomes | `kind:` | Front matter | Body |
| --- | --- | --- | --- | --- |
| `templates/<name>.yml` | `templates/<name>.md` | `template` | every template key, verbatim; `fields` become `inputs` (§13) | the prompt, now Markdown rather than a YAML block scalar; `{{name}}` placeholders are already Jinja (§6.1) |
| `schedules/<name>.yml` | `schedules/<name>.md` | `schedule` | every schedule key, verbatim: the cadence, `overlap`, `timezone`, `enabled`, `base`, `team`, `auto-approve-proposals`; `template: <name>` becomes the reference `../templates/<name>.md` | notes: the YAML file's leading comment |
| `suites/<name>.yml` | `suites/<name>.md` | `suite` | every suite key, verbatim | the suite's description: the leading comment |
| `triggers/<name>.yml` | `triggers/<name>.md` | `trigger` | every trigger key, verbatim (`on`, `paths`, `gate`, `human`, `sensitive`, …); a trigger that files a task names its template by reference, and its variables (`{{group}}`, `{{from_sha}}`) become that template's declared inputs | the leading comment |
| `workflows/<name>.yml` | unchanged | — | — | — |

The new kinds' schemas are
[`suite.schema.json`](../examples/task-repos/schemas/suite.schema.json) and
[`trigger.schema.json`](../examples/task-repos/schemas/trigger.schema.json).
[`widgets-code/.grain/triggers/grain-declarations.md`](../examples/task-repos/widgets-code/.grain/triggers/grain-declarations.md)
is bwsalmon/silo's own `.grain/triggers/grain-declarations.yml`,
converted. One spelling needs care: front matter should quote the key
`"on"`, because a YAML 1.1 reader (PyYAML) reads a bare `on` as `true`.
The Go loader, on YAML 1.2, accepts both.

- **Identity carries over.** A declaration's existing name,
  `repo:<kind>:<owner>:<repo>:<name>`, comes from its name and not its
  file extension, so it is the same for the `.md` file, and goes in its
  `aka:`. Settings rows in grain/config that cite it keep working.
- **The refusals of `docs/declaration-schema.md` still hold, for every
  kind:** a repo declaration carries no `reads`, `writes`, `grants` or
  `target`, and its `context:` entries, and its `ref` inputs' defaults
  and values, name only its own repo (§5.6). The schemas say so, and readers enforce it on every object
  in a code repo's grain folder, whatever its README says (§8.13). A job
  that needs another repo's access stays a settings-owned row.
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
  workflow with a `kind:`-checked edge. A suite or a schedule that names
  one uses its plain path (`.grain/workflows/hub.yml`), resolved as a
  file, like a reference into code. A workflow's `on.schedule` still
  becomes a grain schedule that runs it, as today.

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
  the in-repo planner trigger (`.grain/triggers/design-docs.yml`) unless
  its front matter has `planning:`. With `planning:`, Silo plans it on
  each change to `default_ref` that it reads (§7.4), and the in-repo
  trigger skips it. A design in a Silo repo is always planned by Silo.
  Doing both is refused.

## 13. Migration

`grain tasks export` writes grain's database into task-repo files:

- tasks, with their comments;
- the backlog's order, as references;
- each dispatched task's runs, as execution records;
- schedules and templates from the database, or from grain/config
  `settings/*.yml`.

**Rules:**

- **Stable paths, derived from the old id and a slug.** A task becomes
  `tasks/<legacy number>-<slug>/task.md`, such as
  `tasks/4310-carry-approver/task.md`. The slug is the title, lower-cased,
  with every run of other characters turned into `-`, cut to its first
  few words and at most 40 characters. Templates and schedules keep their
  declared names (`templates/prune-packages.md`). An export run twice
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
- **Runtime columns are dropped.** `next-run-at`, `last-run-at` and the
  attempt rows are not exported.
- **Templates.** A template's `fields` (`model.TemplateField`) become
  `inputs`.
- **Target, reads and writes become `context:`** (§5.6). For a task, in
  this order:

  | grain today (`model.Task`) | `context:` entry |
  | --- | --- |
  | `Target: owner/name`, `Base: b` | `{ref: "<forge URL of owner/name>@b", mode: write}`, first, so it is the primary repo |
  | each `Writes[i]: {repo, base}` | `{ref: "<URL>@<base, or Base>", mode: write}` |
  | each `Reads[i]: owner/name` | `{ref: "<URL>", mode: read}`, unpinned |
  | a `repo-write:owner/name` capability | a `write` entry for that repo, if none exists, and the capability is dropped: a capability no longer implies a repo |
  | each attachment | an asset beside the task and a `read` entry for it (§2.2) |
  | no `Target` (team-targeted) | no `write` entry |

  `<forge URL>` is the deployment's configured forge,
  `https://github.com/owner/name.git` for these deployments. Pasted
  context stays in the prompt, where it was; an author who wants it as a
  file attaches it instead.
- **Templates and schedules** with `repo` and `base` get an input `repo`
  of type `ref`, defaulting to that repo at that base, and
  `context: [{input: repo, mode: write}]`; a template's `reads` become
  `read` entries. A schedule's `target` and `base` become its
  `inputs.repo` (§8.16).
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

It exports as
[`hub-shared/schedules/prune-packages.md`](../examples/task-repos/hub-shared/schedules/prune-packages.md).
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
   `schedules/`, `suites/` and `triggers/` in either form, using the
   `taskrepo` parser for `.md`. If one name exists in both forms, the
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
   the body (and a template's prompt into the body), turns a schedule's
   template name into a reference, and deletes the `.yml` in the same
   commit. **No tombstones are needed:** nothing references a
   declaration by path today, only by the name id, which does not change
   (§12.1).
4. **bwsalmon/grain and bwsalmon/silo convert first.** Then
   `grain declarations check` warns on `.yml` templates, schedules,
   suites and triggers. It keeps loading them until a `v2` of the
   declaration format, which this design does not schedule.

Workflows are never converted (§12.2). Design docs gain front matter
whenever their owners choose (§12.3); until then they are referenceable
as plain files.

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
| Includes listed in a central file, such as the root README (`includes: […]`) | Not self-contained: a line in the README means nothing without the file it names, and cannot carry its own ref mode, kinds and history. One file for every include is contended: Silo linking, the owner adding a template set and a teammate adding schedules would all edit it. Standalone include objects sit where they apply (`backlog/silo.md`), and appear and disappear as files (§3.3) |
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
| Scheduled firings committed to code repos | A commit, and a review, per firing, in the code's history, for content that is the schedule plus a time (§6.2) |
| Let a code repo name the team its schedules fire for, with no adoption | Any repo could fill any team's intake. Adoption makes the team's admins the ones who choose (§6.2) |
| Materialise every reference a task holds | `depends_on` would pull other tasks' repos into the sandbox, `template` and `moved_from` would bring files nobody meant the agent to have, and a placement's `to:` would bring the intake. The sandbox would be whatever the graph happened to touch. Only `context:` materialises, and the rest stays metadata (§5.6) |
| Transitive `context:` by default | Unbounded sandboxes: one design that links to its neighbours brings the whole docs tree, and what a run sees would change whenever someone adds a link. Off by default, `depth: n` where wanted, under file and byte caps that skip and record (§5.6) |
| Keep separate `Target`, `Reads` and `Writes` fields | Three mechanisms for one idea, plus attachments and pasted context as a fourth and fifth. Each needed its own policy check, and task 4303 was a repo that reached a sandbox by a route the policy did not see. One list with a mode is one check (§5.6, §11.21) |
| Require Git LFS for assets | Not plain git everywhere: Silo's git host, a bare `git daemon`, a `file://` root and many mirrors do not serve it, and a clone without LFS gets pointer files. LFS is allowed where the host has it; size limits and external artefacts cover the rest (§2.2) |
| Other names for `context:` (`mounts`, `workspace`, `sandbox`, `inputs`, `reads`/`writes`) | Each names one mechanism or collides with an existing field (§5.6) |
| A sidecar for every asset | Metadata most assets do not need: an image in a design has its alt text in the link. Sidecars are optional, and are where an asset needs a license, provenance, or an external location (§2.2) |
| Attachments inline in front matter (base64) | Front matter is for review; a megabyte of base64 is not. Every diff, digest and parse would carry it. A file beside the object is plain git, and renders in any forge (§2.2) |


## 16. Staged implementation plan

| Stage | silo (bwsalmon/silo) | grain (bwsalmon/grain) |
| --- | --- | --- |
| **1. Format** | a `taskrepo` package: front-matter parse and write (field-preserving), JSON Schemas (`suite`, `trigger`, `include` and `exclude` included), the reference grammar and resolver with redirects (tombstones and repo `moved_to`), nearest-README discovery and `scope`, the namespace resolver of §3.3 with its limits, the path rules, rank keys, lanes and backlog ordering, `task_home` resolution, and `writers` evaluation (principal lists, and the intake's rules); the `context:` resolver shared with namespaces, with its limits and default layout, and the `asset` kind; the Jinja subset on gonja with the Python fixture CI job | pin `taskrepo`; `grain tasks export/import` with the round-trip test on a copy of the database; `pkg/repodecl` and `grain declarations check` read `.md` declarations beside `.yml` (§13.1); `grain declarations convert`; convert bwsalmon/grain's own `.grain/` |
| **2. Standalone Grain on a task repo** | none | a local root becomes the backlog's source of truth for git-backed fields, with the database as runtime and index: tasks, ranked references in `backlog/`, execution records in `runs/`, comments as files; schedules from the namespace; includes of shared folders; `context:` materialisation: the PEP before fetch, staging, the proxy's scope and push rule, the sandbox layout, and the run's `context:` record; attachments as assets |
| **3. Silo hosts git** | `silo-git`: smart HTTP behind IAP and node tokens, the pre-receive hook, protected refs, bundles to GCS, restore drill, metrics; intake and placements repos per team; hosted Grain roots writable by the Grain only; propose and approve by push and through the API (the same commit); assets served from a user-content origin (§11.25); the threat-model boundary | `grain tasks link/unlink` as include objects; push and pull to a Silo remote; lanes |
| **4. Placement by commit** | the dispatcher adds, moves and removes placements in its own repo, and writes cancels on tasks, in §8.11 order; the fence on withdrawn placements; the run index and dependency resolution from execution records; the `pull` notify; WebSocket offers derived from commits behind a flag, then retired | accept, refuse and close as execution records, attestation on the reference, the removed-reference fence, upward filing through Silo; then drop `offer`/`resume` handling |
| **5. Shared definitions and planning** | templates and schedules fired in Silo; the design index, planning and flood control; the review-branch gate | render shared templates locally; `implements`/`closed.design_revs`; trailers from refs |
| **5b. Code repos and task homes** | the code-repo reader (webhook or poll) with validation on read (§8.13); schedule adoption; designs planned in place; `task_home`: landing a task by direct push or a change through grain's merge queue, the pending-change record, the tombstone and reference rewrite, `silo task promote`; cancels of homed tasks | `propose_promotion`; inline firings; local policy for code repos a Grain trusts |

| **6. Signed commits** | keys in README rules; verifying signatures in the hook | verifying the approver's signature against policy (`require_signed`) |

Each stage is a design-item set to be filed after this review. None is
filed by this change.
