# Task repos: Grain's backlog and the Silo/Grain channel as plain git

**Status: design for review (bsalmon@google.com). Nothing here is built,
and no implementation task is filed until it is approved.** The examples
are in [`examples/task-repos/`](../examples/task-repos/README.md).

*Amended 2026-10-16 (task 4448), as the owner settled:*

- *an object's file path is its identity, and there is no `id:` field (§2.1);*
- *a backlog is a folder of ranked references to tasks, and tasks stay
  where they were authored (§5.2);*
- *a Grain records its work as execution records in its own repo (§5.3).*

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

**Linking a Grain to Silo** points it at the repos Silo provides:

- its own repo, now **hosted by Silo**. Silo adds and removes references
  to tasks in its backlog folder. The Grain ranks them and records its
  work there;
- read access to the repos those references point into: the team's
  intake, where the tasks themselves live, and the shared repos the
  tasks reference.

**Unlinking** points it back at a local repo. Because the backlog is git,
the history comes along: the Grain keeps its clone, and the hosted repo
becomes a remote it no longer pushes to.

Silo hosts the Grains' repos, and not the other way round, because
Grains sit behind NAT and firewalls and Silo never dials them (hub.md §4).
The Grain **pulls from and pushes to** its Silo-hosted repo, and **only
pulls** from every other repo. The existing rendezvous WebSocket stays,
and carries:

- `pull` notifications ("repo `<url>` moved to `<sha>`");
- live events;
- the view-through and node UI streams (hub.md §7, §11).

**The Grain stays the PEP.** Every reference Silo adds to a Grain's
backlog is a **claim**. The Grain validates the claim and holds the task
it points at to its local policy, and it may refuse it as it refuses an
offer today.

### 1.1 A graph of repos

Repo **roles** are conventions recorded in each repo's metadata (§3).
The format does not depend on them:

| Role | How many | Holds | Typical writers |
| --- | --- | --- | --- |
| **grain** | one per Grain instance | that Grain's backlog (ranked references to tasks), its execution records and their threads | Silo (adds and removes references, sets `expedite`, supersedes a lost Grain's runs), that Grain and its owner (ranks, execution records, threads) |
| **intake** | one per team, in Silo | the team's tasks: proposals, approvals and cancels; the team's ranking of unplaced work; discussion on tasks | any authenticated user (new proposals only), the team's approvers, the team's members (ranks), Silo |
| **shared** | any number, in Silo or elsewhere | templates, schedules, design docs, include files | the team's maintainers, through review branches |
| **code** | the code repos themselves | `.grain/` declarations and `docs/design/` (§12) | the forge's usual rules |

"Silo" is **one or more repos**, not a special store. Everything works by
references alone:

- a backlog reference in a Grain repo points at a task in an intake;
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
- **Each git-backed field has one writer** (§3, §5). The databases may
  mirror a field for queries, but they never compete with git for it.

| Git-backed | Runtime only (database) |
| --- | --- |
| task: `title`, body (the prompt), `team`, `repo`, `base` | the attempt list, attempt numbers, sandboxes, leases |
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
  `schedule`, `design` or `moved`;
- `schema:`, as in `grain/task@v1` (§10).

There is **no `id:` field.** An object is named by where it is (§2.1).

The body carries what a human reads: a task's prompt, a template's text,
a design's prose. A file means something on its own. You can read it,
mail it or `cat` it without knowing which repo it came from.

**Layout is loosely specified.** Tools find objects by **scanning front
matter**: every `*.md` file whose front matter has `kind:`. The
recommended folders below are conventions only. The one place a folder
matters is the backlog: a repo's queue is the `kind: ref` files in the
folders its README's `backlog:` names, `backlog/` by default (§3).

```
README.md                                  kind: repo
tasks/<name>/task.md                       kind: task
tasks/<name>/comments/<utc>-<author>.md    kind: comment (discussion of the task)
backlog/<name>.md                          kind: ref     (one ranked reference to a task)
runs/<name>/run.md                         kind: run     (one Grain's execution record)
runs/<name>/comments/<utc>-<author>.md     kind: comment (the run's thread)
templates/<name>.md                        kind: template
templates/partials/<name>.md               kind: template (include-only, inputs declared)
schedules/<name>.md                        kind: schedule
designs/<name>.md                          kind: design
<an old path>                              kind: moved   (a tombstone, §2.1)
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
placed, run and closed without its path changing. Placement adds a
reference to it in a Grain's backlog (§5.2), and running it adds an
execution record in that Grain's repo (§5.3).

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
references are ever deleted**: they are pointers, not addresses anyone
keeps (§5.2). Every other object leaves its path only by becoming a
tombstone.

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

## 3. Repo metadata: `README.md` front matter (`kind: repo`)

Each repo's root `README.md` declares the repo. Its prose body is for
humans. Schema: [`schemas/repo.schema.json`](../examples/task-repos/schemas/repo.schema.json).

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
    kinds: [task]
    where: {state: proposed}
    fields: [kind, schema, title, team, repo, base, state, submitter, submitted_via, origin, depends_on, implements, template, capabilities, constraints, labels, created_at, body]
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
- `writers`: an **ordered list of allow rules**. Nothing is writable
  unless a rule allows it.

**A `writers` rule** says:

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
(the team's admins, or the Grain's owner). For a Grain repo, `owner`
cannot be changed at all. So a Grain cannot widen its own write rights,
and nobody can quietly take over its repo.

**Write ownership by kind.** The examples' READMEs spell these rules out
in full. In outline:

| Kind | In an intake repo | In a Grain repo |
| --- | --- | --- |
| `task` | author: creates it `proposed`; approver: `approved`/`rejected`; Silo: Silo-made proposals and firings, `cancelled` + `cancel` | the Grain: tasks it authors itself (local schedules, its owner's own work); its owner the same |
| `ref` | members: `rank`, `priority` (directly, or through Silo); Silo: create and delete | Silo: create and delete, `expedite`, and `to` when it rewrites a redirect; the Grain and its owner: `rank`, `priority` |
| `run` | none | the Grain: creates it and every state but one; Silo: `superseded`, only once the run's reference is gone (§8.8) |
| `comment` | any signed-in user, on tasks; Silo on their behalf | the Grain, on its runs; Silo on a user's behalf; the owner |
| `moved` | the team's admins and Silo (`may: move`) | the Grain's owner |

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
   (§2.1).
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

**Indexes.** Nothing needs an id index any more, because a reader opens
the path. Silo does keep **reverse indexes**, rebuilt from git on start
and updated on each received push: task → backlog references, task →
execution records, task → dependents, requirement → tasks, and
`aka` → path.

**The edges:**

| Edge | Field | Notes |
| --- | --- | --- |
| backlog reference → task | `to: <ref>` + `spec_rev` | `spec_rev` is the commit of the task's repo the placement was made against (§5.2) |
| execution record → task | `task: {ref, rev}` | `rev` is the commit of the task that was run |
| execution record → backlog reference | `from: {ref, rev}` | `rev` is the Grain repo commit the reference was read at, so it still resolves after Silo removes the reference |
| execution record → earlier run | `previous: <ref>` | set on re-placement (§8.8) |
| task → template | `template: {ref, rev, inputs: {…}}` | `rev` is the template commit rendered; `inputs` are the values used |
| task → dependencies | `depends_on: [<ref>]` | "done" means the target task has a `done` execution record (§8.9) |
| task → schedule of origin | `schedule: {ref, fired_at}` | written by whoever fired it |
| task → design requirements | `implements: [{ref: …#HS-6, rev, hash}]` | the requirement and its revision (§7.2) |
| task → the run that filed it | `filed_by: {grain, run: <ref>}` | an agent's upward filing (§8.12) |
| schedule → template | `template: <ref>` + `inputs` | unpinned, so a firing uses the template's current revision |
| comment → what it discusses | `run: <ref>` or `task: <ref>` | usually `../run.md` or `../task.md` (§5.4) |
| tombstone → new address | `to: <ref>` | the moved object lists the old address in `moved_from` |
| repo → its new URL | README `moved_to` | a repo-level redirect |

## 5. Tasks, backlogs, execution records and comments

A task is written once, where its author wrote it, and is never copied.
Three other kinds point at it:

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
repo: https://github.com/bwsalmon/silo.git
base: main
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
`title`, the body, `repo`, `base`, `template`, `depends_on`,
`implements`, `capabilities` or `constraints`. An attestation's
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

A repo's backlog is the `kind: ref` files in its backlog folders
(`backlog/` by default, §3). Each one is small:

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

**Effective order**, for picking the next task:

1. references whose task's dependencies are all satisfied (§8.9), ahead
   of those still blocked;
2. then `expedite: true` ahead of everything else;
3. then `priority`: `urgent`, `high`, `normal`, `low`;
4. then `rank`;
5. then the task's `created_at`, and finally the reference's path.

A Grain skips references whose task is `cancelled`, or which it already
has an open run of.

**Who writes what:**

- **In a Grain's backlog**, Silo adds references (placement) and removes
  them (re-placement, cancel, close). It may set `expedite`, and it may
  rewrite `to:` to the end of a redirect chain. **The Grain and its owner
  own `rank` and `priority`.** Silo sets the initial rank and priority
  when it adds a reference, and never changes them afterwards, so an
  owner's ordering of their own Grain is never overridden by the hub.
- **In an intake's backlog**, the team's members own `rank` and
  `priority`. They push directly, or reorder through Silo's UI, in which
  case Silo commits with `ranked_by` set to the member it acted for.
  Silo adds and removes the intake's references.

**One task, many views.** The intake's references are the team's
ordering of its unplaced work. The same task can be referenced from the
intake and from a Grain's backlog, each at its own rank. Placement does
not remove the intake's reference. When a task closes, or is cancelled
or rejected, Silo removes every reference to it from the backlogs it
hosts. A Grain repo's `backlog/` is therefore its open queue, and its
`runs/` folder is its history.

**Placement reads the intake's order.** A new reference in a Grain's
backlog copies the rank and priority of the task's intake reference, if
it has one, so the team's order is kept among its tasks and interleaves
with the owner's own. Otherwise it is ranked after the Grain's last
reference.

**The attestation lives on the reference.** It is Silo's signed claim to
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
reference to the same task in the same backlog, which makes placement
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
from: {ref: /backlog/4310-carry-approver.md, rev: <this repo's commit where the reference was read>}
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
accepted | running | waiting → superseded (Silo, after re-placement, §8.8)
```

- **`waiting`** is a coarse state for a run held on something outside
  it: a dependency, a human's answer, or a held gate.
- **The Grain writes every state except `superseded`.** Silo writes
  `superseded: {at, by, reason}`, and only once it has removed the run's
  backlog reference (§8.8). `done`, `failed`, `refused`, `cancelled` and
  `superseded` are terminal, so a returning Grain cannot close a run
  that was taken away from it.
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
beside it in the Grain's repo (`runs/<name>/comments/`):

- the Grain writes only its own repo, and the agent's thread is written
  while it runs;
- a thread belongs to one attempt. After re-placement each Grain's
  thread stays with its own record, so neither is split or merged;
- a person replying to a running agent writes through Silo's UI or API,
  and Silo commits the comment into the Grain's repo under the run
  (`via: silo`). The Grain's owner may also push it directly.

**Discussion of the task itself**, before it is placed or about whether
to do it at all, references the task, and lives beside it in its repo
(`tasks/<name>/comments/`).

The hub shows one thread per task: the task's comments and every run's
comments, merged by time and labelled by Grain.

## 6. Templates and schedules

### 6.1 Templating: a strict Jinja2 subset

Template front matter declares its **inputs**. Each input has:

- `name`;
- `type`: `string`, `text`, `int`, `bool`, `enum` or `list`;
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
`overlap`, `timezone`, `enabled`, `base`, `team`, `auto_approve_proposals`),
plus `template: <ref>` and `inputs`. Its body is optional notes.

**A firing creates a task and a reference to it**, at a path derived from
the schedule and the firing time, so a retried firing writes nothing new:
`tasks/scheduled/<schedule>/<fired_at as yyyymmddThhmmZ>/task.md`.

- A schedule in a Grain's own repo, or one the Grain's config lists,
  fires **locally**. The Grain renders the task into its own repo, adds a
  reference to it in its own backlog, and runs it, so it never needs Silo.
- A schedule in a Silo repo that names a team fires **in Silo**. Silo
  renders the task into the team's intake, approved if the schedule's
  approval policy allows it, and places it by adding a reference to a
  Grain's backlog (§8.12). If no Grain can take it now, Silo adds a
  reference to the intake's backlog instead, and the task waits there in
  the team's order.

`overlap` reads the previous firing's derived status (§5.1): `skip`
skips a firing while the last one is unplaced, queued or has an open run.

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
pushed commit, not only the tip, against the repo's README `writers`
rules, for the pusher identity that authentication established. It
works at the level of kinds, files and front-matter fields:

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
   - only a `kind: ref` file may be deleted. Any other object leaves its
     path only by becoming a tombstone, under a `may: move` rule;
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
     reference in the same backlog may point at the same task.
   - An execution record's `task` must resolve, and its `from` must have
     existed at `from.rev`. Its state changes must follow §5.3, and
     terminal states are final.
   - A task's spec fields are frozen once it is approved (§5.1).
   - A push that adds a `depends_on` cycle within Silo's graph is
     refused.
5. **Secrets scan.** Content is checked against secret patterns (§11.6).
6. **Post-receive.** Silo updates its database projection (tasks,
   backlogs, runs) and its reverse indexes (§4). It sends `pull`
   notifications to the affected Grains, removes the backlog references
   of tasks that just closed (§8.5), and triggers planning (§7.4).

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

Silo then places the task (§8.4). Commits: **intake**, 1, by the
approver.

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

The dispatcher chooses a Grain the same way it does today (hub.md §6.2).
Placement writes **one file**, in the Grain's repo:

1. **The Grain's repo:** Silo creates `backlog/<task name>.md`, a
   `kind: ref` with:
   - `to:` the task, and `spec_rev:` the commit of the task's repo it read;
   - `rank` and `priority` copied from the task's intake reference, or
     ranked after the Grain's last reference (§5.2);
   - `added_by: silo` and `added_at`;
   - `attestation`: the signed-claims JWT of hub.md §6.4, with `aud`
     this Grain, `task` the task's global identity, and `spec_digest`
     computed over the task's canonical spec at `spec_rev`.

   Then Silo notifies the Grain over the WebSocket:
   `{"type":"pull","repo":…,"sha":…}`.

**The task is untouched, and is not copied.** The intake's reference, if
any, stays as the team's view. The placement is idempotent: Silo writes
nothing if the backlog already references the task. A Grain that is
offline is not chosen, as today. A Grain that was notified but does not
create a run within the offer timeout is handled as a refusal (§8.8).

Commits: **Grain repo**, 1, by Silo. **Intake**: none.

### 8.5 The Grain runs it

1. The Grain fetches its own repo (prompted by the notify, and also every
   few minutes in case a notify was missed). It finds a new reference,
   resolves its `to:` at `spec_rev`, and validates the reference and its
   attestation (§11.7). It reads the task's tip for its state, so it sees
   a cancel (§8.7). It then holds the task to local policy, as it does
   an offer today.
2. **Accept:** the Grain creates `runs/<yyyy-mm-dd>-<slug>/run.md` with
   `state: accepted`, `task: {ref, rev}`, `from: {ref, rev}`, `grain`
   and `accepted_at`, and pushes. When the task reaches the front of its
   effective order (§5.2) and a slot is free, it commits
   `state: running`.
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
   references from the Grain's backlog and the intake's backlog. The
   run's `from.rev` still resolves the reference it came from.

Commits:

- **Grain repo**: 1 (accepted) + 1 (running) + ⌈comments/batch⌉ + 1
  (closed), all by `grain:<id>`; then 1 by Silo, removing the
  reference;
- **intake**: 1 by Silo, removing the team's reference, if there was one;
- the task file: none, ever.

The rules in a Grain repo's README limit the Grain to its own objects.
It may create and update execution records and their comments, and edit
`rank` and `priority` on its references. It may never create or delete a
reference Silo placed, or write `superseded`. It cannot write the task
at all, because the task is in another repo. So a Grain cannot rewrite
what it was asked to do.

### 8.6 Reorder

- **The Grain's owner moves a task up their Grain's queue:** edit `rank`
  in one reference (`grain backlog move …` or by hand) and push. The
  hook binds `ranked_by` to the pusher. Commits: **Grain repo**, 1, by
  the owner or by `grain:<id>` on the owner's behalf.
- **The team reorders its unplaced work:** a member drags it in Silo's
  UI, or edits `rank` in the intake's reference and pushes. Commits:
  **intake**, 1.
- **Silo expedites a task already placed** (an operator's `at_front`):
  set `expedite: true` on the Grain's reference. Commits: **Grain
  repo**, 1, by Silo. Silo does not touch `rank`.

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
today's `cancel_pending`.

Commits:

- **intake**, 1 by Silo (the cancel, and the intake reference's removal
  in the same commit);
- **Grain repo**, 1 by the Grain (its run cancelled), then 1 by Silo
  (the reference removed).

### 8.8 Re-placement

Re-placement happens after a lost Grain (the team's grace, hub.md
§6.3), a refusal, or an offer timeout. **Nothing moves, so nothing is
tombstoned**: the task keeps its address, and only references change.
Silo writes in this order:

1. **The old Grain's repo, one commit:** delete the reference. If that
   Grain has an open run of the task, set it `state: superseded` with
   `superseded: {at, by: silo, reason}` in the same commit. A refused run
   is already terminal and is left as it is.
2. **The new Grain's repo, one commit:** add a reference to the same
   task, as in §8.4, with `previous:` naming the old run if there was
   one. Then notify.
3. The new Grain accepts and starts **its own** run, copying `previous:`.

Each step is a no-op if its effect is already in git: there is no
reference in the old backlog and no open run, or there is a reference in
the new one. So a retry after a crash is safe.

**Removal comes first** because the two gaps are not equal. Between steps
1 and 2 the task is briefly unplaced, which Silo's dispatcher notices and
finishes. The other order would leave two Grains holding the task, which
can produce two changes.

**The fence** is the removed reference plus the terminal `superseded`
state. If the old Grain comes back mid-task, its pull shows its
reference gone and its run superseded. It stops, and the hook refuses
any later push to that run. A branch or pull request it had already
pushed is reported in the hub as belonging to a superseded run.

**History shows both attempts**: the old record and its thread stay in
the old Grain's repo, and the new record's `previous:` links to them.

Commits: **old Grain**, 1 by Silo; **new Grain**, 1 by Silo, then the
new run's own commits; **intake**, none.

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
- **Placed tasks.** A Grain runs only tasks referenced from **its own**
  backlog, by a writer allowed there. It never runs a task merely because
  a foreign repo contains one, or ranks one.
- **Foreign content is claims.** Templates, includes and designs from
  another host are data that a task the Grain already accepted may use.
  They grant nothing (§11.4).
- **A standalone Grain's own repo** is trusted as its owner's: the owner
  has the filesystem anyway.

### 8.11 No cross-repo transactions

Every multi-repo flow above writes in a fixed order, and records the
step it has reached. A reader that catches the gap sees one of these:

- an approved task that no backlog references yet: unplaced, and the
  dispatcher finishes the placement;
- a run marked `superseded` with no reference yet in a new Grain's
  backlog: the same;
- a closed run whose reference has not yet been removed: a Grain skips
  it, because it already has a closed run of the task;
- a moved object at its new address whose old address is not yet a
  tombstone: readers of the old address still find the old object, which
  is unchanged.

Each step is keyed by (task identity, operation, Grain), so a retry is a
no-op. Silo's dispatcher records the step it has reached in its database
row, as it records an offer today. After a crash it resumes from the
first step whose effect is missing in git.

| Flow | Order |
| --- | --- |
| place | Grain repo (reference) → notify |
| close | Grain repo (run closed) → Silo removes the references (Grain repo, intake) |
| cancel | intake (task `cancelled`, intake reference removed) → notify → the Grain's run `cancelled` → Grain reference removed |
| re-place | old Grain (reference removed, run superseded) → new Grain (reference) → notify |
| move across repos | new home (with `moved_from`) → tombstone at the old path → Silo rewrites references' `to:` |
| upward filing | intake (proposed, committed by Silo) → the Grain's comment on its run naming it |
| schedule firing in Silo | intake (approved task, deterministic path) → Grain repo (reference) |
| planning | intake proposals and their references (deterministic paths) → design's `planning.last_rev` recorded in Silo's DB |
| definitions via review branch | `review/*` branch → the approver's merge commit on `main` |

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

Silo then places it, by adding a reference to a Grain's backlog. Commits:

- **intake**, 1;
- **Grain**, 1.

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

**Link, then unlink, a standalone Grain.**

- **Link:** `grain hub login`, then `grain tasks link`.
  1. Silo creates an empty `grains/<id>.git`.
  2. The Grain pushes its local history into it. This first push is
     allowed for the owner's credential only, and only into an empty
     repo.
  3. The owner's README commit, which `grain tasks link` makes, sets
     `host: silo` and adds Silo's `writers` rules.

  The Grain then makes the hosted repo its remote. Its tasks, references
  and runs keep their paths, so every reference inside the repo still
  resolves. Their global identity is now the hosted URL. Nothing outside
  could read the `file://` URL, so nothing needs a redirect. If something
  did reference it, the local README gets `moved_to`. Commits: **Grain
  repo**, its history plus 1.
- **Unlink:** `grain tasks unlink`.
  1. Silo removes the references it placed that have no run yet, and
     re-places those tasks (§8.8).
  2. Open runs of Silo-placed tasks must close first. With `--force`,
     Silo supersedes them instead.
  3. The owner's README commit sets `host: local` and drops Silo's
     `writers` rules.

  The Grain points back at its local clone, which keeps all history.
  The hosted repo stays as a read-only archive, so references into it
  from the intake (`filed_by`, `previous`) still resolve. Commits:
  **Grain repo**, 1 by Silo, then 1 by the owner; **other Grains**, 1
  each for the re-placed tasks.

## 9. How the task interface reduces

| hub-interface.md today | With task repos |
| --- | --- |
| `hello` / `welcome` / `heartbeat` / `goodbye` | unchanged (the registry and slots stay live) |
| `offer` (task + attestation) | Silo's commit of a backlog reference (with `attestation`) + `{"type":"pull"}` |
| `resume` | gone: the reference is in the Grain's repo, and a returning Grain pulls |
| `accept` / `refuse` | the Grain's commit of an execution record in `accepted` / `refused` + `refusal` |
| `result` | the Grain's commit of the record's `done\|failed` + `closed` |
| `cancel` | Silo's commit of `cancelled` + `cancel` on the task, + `pull`; the run's `cancelled` commit |
| `propose_task` (upward filing) | unchanged as a request; Silo makes the intake commit |
| event stream, view stream, http stream | unchanged (runtime; never in git) |

The contract becomes:

- the repo schema (§3, §10);
- the README `writers` rules for a Grain repo;
- the `pull` notification.

A customised Grain conforms by reading and writing those files. Until
stage 4 of §16, both paths run, and the WebSocket messages are derived
from the commits.

## 10. Schemas and versioning

Each kind has a JSON Schema (2020-12) in
[`examples/task-repos/schemas/`](../examples/task-repos/schemas/):
`common` (shared definitions), `repo`, `task`, `ref`, `run`, `comment`,
`template`, `schedule`, `design` and `moved`.

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
  aliases, so existing declarations move over verbatim (§12).

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
   not authorized for. Silo grants a Grain read on exactly the repos its
   backlog references need: the task's own repo, the repos the task
   references (template, includes, designs), and the Grain repos holding
   its dependencies' execution records (§8.9), for as long as the
   reference is in its backlog. The hook checks new references
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
     spec at `spec_rev`. A Grain whose policy names `silo_issuer` refuses
     a reference without a valid one.
   - **A redirect cannot change what was placed.** If the task has moved
     since, the Grain follows the tombstone and recomputes the digest on
     what it finds. A spec that no longer matches is refused, so a
     tombstone can only ever lead to the same task.
   - Any Silo-owned field that changes later (a task's `cancel`, a run's
     `superseded`) is accepted only if it is signed or attested the same
     way.
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
   - a compromised Grain can change only its own execution records and
     the order of its own backlog, in its own repo. It cannot write any
     task, so it cannot rewrite what it was asked to do, and it cannot
     write another Grain's records, so it cannot claim another Grain's
     work;
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

## 12. Interop with `.grain/` and `docs/design/`

- **Declarations stay readable.** `pkg/repodecl` keeps loading
  `.grain/templates/*.yml` and `.grain/schedules/*.yml`.
- **The recommended form is the same Markdown** with front matter
  (`.grain/templates/<name>.md`, `kind: template`). It carries the same
  keys `pkg/declschema` defines, and the body is the prompt. The
  refusals of `docs/declaration-schema.md` still hold: a repo declaration
  cannot carry `reads`, `writes`, `grants` or `target`. The hook and
  `grain declarations check` enforce this from the README `writers` rules
  of a code repo.
- **`docs/design/*.md`** gains optional `kind: design` front matter.
  `{#D-n}` and `{#HS-n}` anchors, and the `Design-Item:`, `Design-Rev:`
  and `Design-Removed:` trailers, carry over unchanged. A doc in a code
  repo keeps its in-repo `Design-Item:` path. A Silo-hosted design is
  cited by its ref. `pkg/designdoc`'s parsing becomes the shared parser.
  Once a design is in a Silo repo, the in-repo planner trigger
  (`.grain/triggers/design-docs.yml`) is replaced by Silo planning, and
  doing both is refused.

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
  repo. For a Grain linked to Silo, tasks submitted at the hub are
  exported into the team's intake from Silo's database, and the tasks
  the Grain authored itself stay in the Grain's repo.
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
  - It caps a push at 1,000 changed files and 10 MiB.
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
    the old repo's URL. In a Grain repo, Silo re-adds the open backlog
    references in the new repo (references are pointers, so that is
    allowed), and the rollover waits for open runs to close. In an
    intake, open tasks stay in the old repo until they close, and new
    tasks go to the new one.
- **Availability.** While Silo's git is down:
  - A Grain keeps running what it has.
  - It commits locally and pushes on recovery, rebasing onto the new tip
    with a front-matter-field 3-way merge. Field ownership is disjoint,
    so the merge never conflicts on owned fields, and comments are
    separate files.
  - Nothing new is placed.
  - Standalone and locally scheduled work continues.

## 15. Alternatives considered

| Alternative | Why not |
| --- | --- |
| GitHub pull requests as the proposal and approval mechanism | Needs GitHub, its accounts and its API. Approval would be a forge setting, not a team rule Silo decides. Repos on other hosts and local repos would be second class |
| Move files between folders to change state (`queued/`, `done/`) | Splits `git log --follow` history, breaks every reference by path, and turns each state change into a delete plus an add that races with concurrent edits |
| One repo per team, Grains included | Every Grain would fetch every other Grain's churn, and per-Grain write rules would become per-path rules in a shared repo with more contention. A Grain repo per Grain matches the single-tenant Grain |
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

## 16. Staged implementation plan

| Stage | silo (bwsalmon/silo) | grain (bwsalmon/grain) |
| --- | --- | --- |
| **1. Format** | a `taskrepo` package: front-matter parse and write (field-preserving), JSON Schemas, the reference grammar and resolver with redirects (tombstones and repo `moved_to`), the path rules, rank keys and backlog ordering, and `writers` rule evaluation; the Jinja subset on gonja with the Python fixture CI job | pin `taskrepo`; `grain tasks export/import` with the round-trip test on a copy of the database; `grain declarations check` accepts `.md` declarations |
| **2. Standalone Grain on a task repo** | none | a local task repo becomes the backlog's source of truth for git-backed fields, with the database as runtime and index: tasks, ranked references in `backlog/`, execution records in `runs/`, comments as files; schedules from the repo |
| **3. Silo hosts git** | `silo-git`: smart HTTP behind IAP and node tokens, the pre-receive hook, protected refs, bundles to GCS, restore drill, metrics; intake repos per team; propose and approve by push and through the API (the same commit); the threat-model boundary | `grain tasks link/unlink`; push and pull to a Silo remote |
| **4. Placement by commit** | the dispatcher adds and removes backlog references, writes cancels on tasks and supersedes runs, in §8.11 order; the run index and dependency resolution from execution records; the `pull` notify; WebSocket offers derived from commits behind a flag, then retired | accept, refuse and close as execution records, attestation on the reference, the removed-reference fence, upward filing through Silo; then drop `offer`/`resume` handling |
| **5. Shared definitions and planning** | templates and schedules fired in Silo; the design index, planning and flood control; the review-branch gate | render shared templates locally; `implements`/`closed.design_revs`; trailers from refs |
| **6. Signed commits** | keys in README rules; verifying signatures in the hook | verifying the approver's signature against policy (`require_signed`) |

Each stage is a design-item set to be filed after this review. None is
filed by this change.
