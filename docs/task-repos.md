# Task repos: Grain's backlog and the Silo/Grain channel as plain git

**Status: design for review (bsalmon@google.com). Nothing here is built,
and no implementation task is filed until it is approved.** The examples
are in [`examples/task-repos/`](../examples/task-repos/README.md).

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
own task repo, which can be a purely local git repo (`file:///var/lib/grain/tasks.git`).

**Linking a Grain to Silo** points it at the repos Silo provides:

- its own backlog repo, now **hosted by Silo**, where Silo writes placements;
- read access to the shared repos that backlog references.

**Unlinking** points it back at a local repo. Because the backlog is git,
the history comes along: the Grain keeps its clone, and the hosted repo
becomes a remote it no longer pushes to.

Silo hosts the Grains' repos, and not the other way round, because
Grains sit behind NAT and firewalls and Silo never dials them (hub.md §4).
The Grain **pulls from and pushes to** its Silo-hosted repo. The existing
rendezvous WebSocket stays, and carries:

- `pull` notifications ("your repo moved to `<sha>`");
- live events;
- the view-through and node UI streams (hub.md §7, §11).

**The Grain stays the PEP.** Everything Silo writes into a Grain's repo
is a **claim**. The Grain validates the claim and holds it to its local
policy, and it may refuse a placed task as it refuses an offer today.

### 1.1 A graph of repos

Repo **roles** are conventions recorded in each repo's metadata (§3).
The format does not depend on them:

| Role | How many | Holds | Typical writers |
| --- | --- | --- | --- |
| **grain** | one per Grain instance | that Grain's backlog: placed tasks, their threads and closing snapshots | Silo (placements, cancels, tombstones), that Grain (state, thread, snapshot) |
| **intake** | one per team, in Silo | proposals and unplaced tasks, approvals, placement records | any authenticated user (new proposals only), the team's approvers, Silo |
| **shared** | any number, in Silo or elsewhere | templates, schedules, design docs, include files | the team's maintainers, through review branches |
| **code** | the code repos themselves | `.grain/` declarations and `docs/design/` (§12) | the forge's usual rules |

"Silo" is **one or more repos**, not a special store. Everything works by
references alone: an intake file links to the Grain repo it was placed
in, a task links to its template in a shared repo, and a design
requirement's tasks are found by following the links back (§7.3).

### 1.2 What goes in git, and what does not

Git holds what people review and refer to:

- definitions, intent and decisions;
- designs;
- the human-readable thread;
- for each task, a final-state snapshot.

**High-churn runtime state stays in the Grain's or Silo's database:**
run attempts, live status, heartbeats, events, transcripts and logs. Two
rules keep the split clean:

- **State lives in front matter, never in file location.** A file is
  written once, at a stable path, and never moves to change state.
- **Each git-backed field has one writer** (§3.2). The databases may
  mirror a field for queries, but they never compete with git for it.

| Git-backed (task front matter) | Runtime only (database) |
| --- | --- |
| `id`, `title`, body (the prompt), `team`, `repo`, `base` | the attempt list, attempt numbers, sandboxes, leases |
| `submitter`, `approved_by`, `approved_at`, `origin`, `filed_by` | the live `running` sub-states (sandboxing, agent turn, waiting for CI) |
| `template` (+ `inputs`, `rev`), `schedule` | events, `seq`, transcripts, tool calls, diffs, logs |
| `depends_on`, `implements`, `capabilities`, `constraints` | heartbeats, slot counts, offer timers |
| `state` at the coarse grain of §5.1, `refusal`, `cancel` | the dispatcher's claim, `refused_by` lists, offer counts |
| `placement` (Grain, repo, generation, time), `moved_to` | merge-queue position, CI run ids |
| `closed`: outcome, change link, merge commit, summary, requirement revisions | cost, token counts (summarised into `closed` if wanted) |

A coarse `state` change, such as `placed → running → done`, is a commit.
A fine-grained step, such as "agent turn 14", is an event.

## 2. Objects: one self-contained Markdown file each

Every object is one UTF-8 file. The file has **YAML front matter**
between `---` lines, then a **Markdown body**. Every file declares:

- `kind:`, one of `repo`, `task`, `comment`, `template`, `schedule` or
  `design`;
- `schema:`, as in `grain/task@v1` (§10);
- `id:`, a stable id (§2.1). A `repo` is the exception: its URL
  identifies it.

The body carries what a human reads: a task's prompt, a template's text,
a design's prose. A file means something on its own. You can read it,
mail it or `cat` it without knowing which repo it came from.

**Layout is loosely specified.** Tools find objects by **scanning front
matter**: every `*.md` file whose front matter has `kind:`. They never
rely on fixed paths. The recommended folders below are conventions only:

```
README.md                       kind: repo
tasks/<id>-<slug>/task.md       kind: task
tasks/<id>-<slug>/comments/<utc>-<id>.md   kind: comment (one file per comment)
templates/<name>.md             kind: template
templates/partials/<name>.md    kind: template (include-only, inputs declared)
schedules/<name>.md             kind: schedule
designs/<name>.md               kind: design
```

Comments are one file each, so two writers appending to the same thread
never conflict.

### 2.1 IDs

An id is `<kind-prefix>-<12 Crockford base32 chars>`, for example
`task-4redj39vwnz4`, `tmpl-…`, `sch-…`, `des-…` or `cmt-…`. The 12
characters are 60 random bits. That makes ids:

- **globally unique without coordination.** Standalone Grains mint ids
  offline. At 10⁶ objects the chance of a collision is about 4·10⁻⁷, and
  Silo's hook refuses a duplicate id within the graph it hosts anyway;
- **human-friendly.** They are short, case-insensitive, and contain no
  `0/O/1/I/L` ambiguity. The file name and `title` carry the readable
  part;
- **stable.** The id never changes when a file moves, the task is
  re-placed (§8.6) or the object is migrated (§13). Earlier identities are
  kept in `aka:`, for example `grain:eu/task/4310`, `silo:hub/task/812` or
  `repo:template:bwsalmon:grain:prune-packages`. Migration derives the id
  from the old one (§13), so an export run twice produces the same id.

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
host: silo                         # silo: Silo enforces writes (§8); other: readers validate (§8.8)
default_ref: main                  # what an unpinned reference resolves to
ref_policy: {protected: [main], fast_forward_only: true, review_prefix: review/}
aliases:                           # optional shorthand: alias → git URL
  shared: https://silo.example.com/git/hub/shared.git
  eu: https://silo.example.com/git/grains/eu.git
writers:                           # write ownership; whatever is not allowed is refused
  - who: [authenticated]
    may: create
    kinds: [task]
    where: {state: proposed}
    fields: [kind, schema, id, title, team, repo, base, submitter, origin, filed_by, depends_on, implements, template, capabilities, constraints, labels, created_at, body]
    bind: {submitter: $pusher}
  - who: [team:hub#approver]
    may: update
    kinds: [task]
    transition: {field: state, from: [proposed], to: [approved]}
    fields: [state, approved_by, approved_at]
    bind: {approved_by: $pusher}
    forbid: {approved_by: $submitter}
  - who: [silo]
    may: [create, update]
    kinds: [task, comment]
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
- `aliases`: shorthand for related repos.
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
- `may`: `create`, `update` and/or `delete`.
- `kinds` and `paths`: globs that limit which files the rule covers.
- `fields`: the front-matter fields the push may set or change. `body` is
  the Markdown body, and `"*"` means every field.
- `where`: required values in the result.
- `transition`: `{field, from: […], to: […]}`, the state changes the rule allows.
- `merge_of`: the change must arrive as a merge of a matching branch
  (`review/*`), pushed by someone other than that branch's author (§8.9).
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

**Every reference in front matter is an object:** `{ref: …, id: …}`.
The two parts do different jobs:

- the **path is a hint**: it is where the target was when the reference
  was written;
- the **id is the truth**: it is what the target must be.

A reader opens the path at the resolved ref. If the file is missing, or
its id differs, the reader scans that repo's front matter for the id.
That index is cheap: a reader keeps an id → path map per commit, updated
from `git diff --name-only`. The Markdown body may use bare refs in
ordinary links. Those are for humans and carry no meaning.

**Resolution:**

1. **Unpinned references** resolve to the tip of the target repo's
   `default_ref` at the moment they are read. Anything recorded for
   traceability records the commit sha that was actually used: a task's
   `template.rev`, `implements[].rev` and `closed.design_revs`. So a
   reference reads live and is recorded pinned.
2. **Pinned references** (`@v3`, `@<sha>`) resolve to exactly that ref.
   A branch pin follows the branch's tip.
3. A **tombstone** is a task whose `state` is `moved` and which carries
   `moved_to: {ref, id}`. Readers follow it, at most 8 hops.
4. **Cycles:**
   - `depends_on` cycles are rejected on write in Silo-hosted repos. A
     reader treats every task on a cycle as blocked and reports the cycle.
   - Include/import cycles are rejected at render time.
   - Following tombstones is bounded by the hop limit.
5. **Dangling references:** the target is missing, or the reader may not
   read it (§11.4). They are never errors at write time across repos,
   because no transaction spans repos (§8.9). At read time:
   - a dependency stays **blocked**, with the reason shown;
   - a template reference makes the task **unrenderable**;
   - an `implements` reference is shown as **unresolved** in
     traceability.

   Silo re-checks dangling references on every push it receives, and
   lists them in the repo's health view.

**The edges, all `{ref, id}` objects:**

| Edge | Field | Notes |
| --- | --- | --- |
| task → template | `template: {ref, id, rev, inputs: {…}}` | `rev` is the template commit rendered; `inputs` are the values used |
| task → dependencies | `depends_on: [{ref, id}]` | the target may be in another Grain's repo; "done" means its `state: done` (§8.7) |
| task → schedule of origin | `schedule: {ref, id, fired_at}` | written by whoever fired it |
| task → design requirements | `implements: [{ref: …#HS-6, id, rev, hash}]` | the requirement and its revision (§7.2) |
| schedule → template | `template: {ref, id}` + `inputs` | unpinned, so a firing uses the template's current revision |
| comment → task | `task: {ref, id}` | usually `../task.md` |
| tombstone → new location | `moved_to: {ref, id}` | same `id` at the target |
| intake record → placed copy | `placement: {grain, ref, id, generation, at}` | the copy in the Grain repo is authoritative after placement |

## 5. Tasks and comments

```yaml
---
kind: task
schema: grain/task@v1
id: task-4redj39vwnz4
aka: [grain:eu/task/4310]
title: Carry a hub task's approver on its offer and attestation
team: hub
repo: https://github.com/bwsalmon/silo.git
base: main
state: done
submitter: user:bsalmon@google.com
approved_by: user:approver@example.com
placement: {grain: eu, ref: /tasks/task-4redj39vwnz4-carry-approver/task.md, generation: 1, at: 2026-10-16T13:58:00Z}
implements:
  - {ref: "shared:designs/hub-intake.md#HS-2", id: des-2ghanwagam5v, rev: <design commit>, hash: "sha256:…"}
closed: {outcome: done, at: …, change: {url: …, merge_commit: 5623aa25…}, summary: …}
---
<the prompt, in Markdown>
```

The full examples are in
[`examples/task-repos/`](../examples/task-repos/README.md), and
`python3 examples/task-repos/check.py` checks them against the schemas,
resolves every reference, and renders the template fixture.

### 5.1 States

```
proposed → approved → placed → running → done | failed
    ↘ rejected           ↘ refused (→ re-placed: moved)   ↘ cancelled
                                ↘ moved (tombstone; the task lives on at moved_to)
```

Each state has one writer:

- `proposed`: the submitter.
- `approved` and `rejected`: an approver.
- `placed`, `moved` and the `cancel` request: Silo.
- `running`, `refused`, `done`, `failed` and `cancelled`: the Grain.

On a standalone Grain, the Grain and its owner hold every role.

### 5.2 Comments

A comment has `kind: comment`, an `id`, `task: {ref, id}`, `author`
(`user:…`, `grain:eu`, or `agent:grain:eu/task-…`), `at`, an optional
`in_reply_to`, and a body. Comments are append-only: an edit is a new
comment with `supersedes:`.

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
plus `template: {ref, id}` and `inputs`. Its body is optional notes.

**Where a schedule fires:**

- A schedule in a Grain's own repo, or one the Grain's config lists,
  fires **locally**. The Grain renders the task and commits it to its own
  repo, so it never needs Silo.
- A schedule in a Silo repo that names a team fires **in Silo**. Silo
  renders the task into the team's intake, approved if the schedule's
  approval policy allows it, and places it (§8.10).

**Runtime state is not committed.** `next_run_at`, `last_run_at` and
the overlap bookkeeping stay in the database. The task each firing
produces records `schedule.fired_at`, so the history of firings is in
git anyway.

## 7. Design docs (`kind: design`)

```yaml
---
kind: design
schema: grain/design@v1
id: des-2ghanwagam5v
title: Hub task intake, approval and claims
status: accepted            # draft | proposed | accepted | implementing | implemented | superseded | withdrawn
owners: [user:bsalmon@google.com]
applies_to: [{ref: "silo-code:/"}, {ref: "grain-code:pkg/daemon/hub.go"}]
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

**What a task records:**

- **When it is filed:** `implements: [{ref, id, rev, hash}]`.
- **When it closes:** the Grain copies the revisions it worked against
  into `closed.design_revs`. Commits carry them in the existing trailers,
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

For each requirement, the index shows its tasks, their states (from the
Grain copy), and **changed since implemented**. A requirement counts as
changed when its current hash differs from the hash in the done task's
`closed.design_revs`. Designs outside Silo, in a code repo or on GitHub,
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
| changed, task still open | a proposal superseding the open task (`supersedes: {ref, id}`). Approving it makes Silo write a `cancel` request to the old task's Grain |
| changed, done | a delta task covering only the changed text |
| removed, open | a proposal to cancel the open tasks. Nothing is cancelled without approval |
| removed, done | a removal task, whose commit carries `Design-Removed:` |
| unchanged | nothing |

**Idempotency.** Each planned task's id is derived from (design id,
requirement ID, revision hash, action). Re-running planning on the same
push writes nothing new.

**Flood control:**

- At most `max_tasks_per_change` tasks are filed per push (default 10).
  A larger plan files **one** plan proposal listing every intended task.
  Approving it files them.
- Planning is capped per team per day (default 50).
- Planned tasks are always `proposed`, unless the design says
  `approval: inherit` **and** the pusher whose change was merged is an
  approver for the planning team.
- A design change pushed by a non-approver lands only through a review
  branch an approver merged (§8.9).

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
3. **Validation.** Each changed file must pass its kind's JSON Schema,
   its `schema` version must be one the hook knows, and its id must be
   unique in the repo and in Silo's index. A new cross-repo reference
   must resolve with **the pusher's** read rights (§11.4). A push that
   adds a `depends_on` cycle within Silo's graph is refused.
4. **Secrets scan.** Content is checked against secret patterns (§11.6).
5. **Post-receive.** Silo updates its database projection (tasks,
   placements, the traceability index). It sends `pull` notifications to
   the affected Grains and triggers planning (§7.4).

Plain git cannot express per-path permissions, but the server that
receives the push can. **No pull requests and no GitHub are needed.**

### 8.2 Propose

**By git:** a user clones the team's intake repo, adds
`tasks/<id>-<slug>/task.md` with `state: proposed`, commits and pushes.

**The hook allows it** under the intake's `authenticated` rule:

- the push may only *create* files;
- each new file must be a `kind: task` with `state: proposed`;
- `submitter` must equal the pusher.

**Silo's UI, CLI and API** (`POST /v1/tasks`) make the **same commit**,
authored by Silo, with `submitter` taken from the caller's sign-on. Silo
pushes as `silo` under its own rule, which requires
`submitted_via: api`. Commits: **intake**, 1.

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
remote: tasks/task-…/task.md: field state: no writers rule lets user:mallory@example.com
remote:   update kind task (team:hub#approver required for proposed→approved)
remote: tasks/task-…/task.md: approved_by must equal the pusher (user:mallory@example.com)
```

Commits: none. An approver who sets `state: rejected` (plus a reason) is
accepted under the same rule.

### 8.4 Place on a Grain

The dispatcher chooses a Grain the same way it does today (hub.md §6.2).
It writes in this order:

1. **The Grain's repo:** create `tasks/<id>-<slug>/task.md`. The file is
   a copy of the intake file, plus:
   - `state: placed`;
   - `placement: {grain, generation: 1, at, intake: {ref, id}}`;
   - `attestation`: the signed-claims JWT of hub.md §6.4, unchanged,
     with `spec_digest` computed over the task's canonical front matter
     and body.

   Then notify the Grain over the WebSocket: `{"type":"pull","repo":…,"sha":…}`.
2. **The intake repo:** set `state: placed` and
   `placement: {grain, ref: "<grain URL>//tasks/…/task.md", generation: 1, at}`.
   The body and every other field stay as approved.

Each step is idempotent: Silo writes a file only if it is not already in
the target state. A Grain that is offline is not chosen, as today. A
Grain that was notified but does not answer within the offer timeout is
handled as a refusal (§8.6).

### 8.5 The Grain runs it

1. The Grain fetches (`git fetch`, prompted by the notify, and also every
   few minutes in case a notify was missed). It finds a `placed` file and
   validates it (§11.7). It then holds the task to local policy, as it
   does an offer today.
2. **Accept:** the Grain commits `state: running` to its repo and pushes.
   **Refuse:** it commits `state: refused` with `refusal: {reason, at}`.
3. **While it runs:** the thread is pushed as comment files, at most one
   push every 5 seconds, batched. Events stream over the WebSocket as
   they do today.
4. **On finish:** one commit sets `state: done|failed` and writes the
   `closed:` snapshot:
   - `outcome`, `at` and `summary`;
   - `change: {url, merge_commit, branch}`;
   - `design_revs`;
   - `follow_ups: [{ref, id}]`.

Commits: **Grain repo**, 2 + ⌈comments/batch⌉, all by `grain:<id>`.

The rule in a Grain repo's README limits the Grain to its own fields:
`state` (only the Grain's transitions), `refusal`, `closed`, and
creating comment files. It may never change `approved_by`, `submitter`,
`placement`, `attestation` or the body. So a Grain cannot rewrite what
it was asked to do.

### 8.6 Cancel, refusal and re-placement

**Cancel.** A user asks in Silo's UI, or edits the intake file, and Silo
writes `cancel: {by, at, reason}` into the Grain's copy, then notifies.
The Grain stops the task and commits `state: cancelled`. A Grain that is
away sees the cancel when it next pulls, which replaces today's
`cancel_pending`. Commits:

- **Grain repo**: 1 by Silo, then 1 by the Grain;
- the intake file needs none, because it points at the Grain copy.

**Re-placement** happens after a lost Grain (the team's grace, hub.md
§6.3), a refusal, or an offer timeout. Silo writes in this order:

1. **The new Grain's repo:** the task at the same `id`, with
   `placement.generation: n+1` and `placement.previous: {ref}`.
2. **The old Grain's repo:** a tombstone, `state: moved` with
   `moved_to: {ref, id}`. This is a Silo-owned field. The body and the
   thread stay where they are, so the history is not split.
3. **The intake repo:** `placement` is updated to the new location.

The **generation** is the fence. If the old Grain comes back mid-task,
its pull shows `moved`. It stops, and its `closed` push is refused,
because the Grain may not write `closed` on a `moved` task. Readers who
see the same id in two Grain repos believe the higher generation.

### 8.7 A cross-Grain dependency completes

Task B is placed on Grain `us` and has
`depends_on: [{ref: "eu:tasks/…/task.md", id: task-A}]`. Grain `eu`
pushes `state: done` for A.

Silo's post-receive keeps a reverse index of `depends_on` edges, so it
notifies `us`. `us` fetches A's file and sees `done`, so it unblocks B.
It needs read access to the `eu` repo, granted by Silo when B was placed
(§11.4). A Grain without that access sees the dependency as dangling and
keeps B blocked.

Commits: **eu**, 1, A's close. The dependency needs no commit in `us`.
B's next state change is its own `running` commit.

### 8.8 Content on other hosts

Silo cannot enforce writes to repos it does not host:

- a standalone Grain's local repo;
- GitHub, Gerrit or a bare server holding shared templates or designs.

So for those a Grain applies the same rules **on read**:

- **Validation.** It checks schema and allowed transitions against the
  README `writers` rules. It cannot check who pushed. Where the README
  says `require_signed: true`, it also checks commit signatures against
  the `keys` listed for each principal (§11.8).
- **Placed tasks.** A Grain runs only tasks placed in **its own** repo,
  by a writer allowed there. It never runs a task merely because a
  foreign repo contains one.
- **Foreign content is claims.** Templates, includes and designs from
  another host are data that a task the Grain already accepted may use.
  They grant nothing (§11.4).
- **A standalone Grain's own repo** is trusted as its owner's: the owner
  has the filesystem anyway.

### 8.9 No cross-repo transactions

Every multi-repo flow above writes **the new state's home first, and the
pointer to it second**. A reader that catches the gap sees one of two
things:

- an id that exists but is not linked yet. The intake still says
  `approved`, and the Grain copy exists. Readers prefer the Grain copy;
- a link to a copy that is newer than its source. Readers believe the
  higher generation.

Each step is keyed by (id, operation, generation), so a retry is a
no-op. Silo's dispatcher records the step it has reached in its database
row, as it records an offer today. After a crash it resumes from the
first step whose effect is missing in git.

| Flow | Order |
| --- | --- |
| place | Grain repo → notify → intake |
| re-place | new Grain → old Grain tombstone → intake |
| cancel | Grain repo (`cancel`) → notify; the Grain's `cancelled` follows |
| upward filing | intake (proposed) → the Grain's comment on the originating task naming it |
| schedule firing in Silo | intake (approved) → place |
| planning | intake proposals (deterministic ids) → design's `planning.last_rev` recorded in Silo's DB |
| definitions via review branch | `review/*` branch → the approver's merge commit on `main` |

**Definitions** (templates, schedules and designs) are pushed straight
to `main` by maintainers, or through **review branches**. Anyone with
`create` rights may push to `review/*`. The hook accepts a commit on
`main` that touches a definition only if one of these holds:

- the pusher is a maintainer;
- the commit is a merge whose second parent is a `review/*` tip, and the
  merge's pusher is an approver other than the branch's author.

On receive, Silo checks schema and references, and plans designs.

### 8.10 The remaining worked flows

**An agent's upward filing.** An agent on Grain `eu` calls
`propose_task` for another team's repo. The Grain pushes to that team's
intake, as `grain:eu`, a new task with:

- `state: proposed`;
- `submitter`: the human who submitted the originating task;
- `filed_by: {grain: eu, task: {ref, id}}`.

The intake's rule for `grain:*` principals allows exactly this, and
`bind` checks that the `submitter` and `filed_by.task` exist in the
pusher's own repo. Commits:

- **intake (other team)**, 1;
- **eu**, 1: a comment on the originating task linking the proposal.

It then needs approval like any proposal.

**A schedule fires a templated task.** The `prune-packages` schedule in
`hub/shared` fires at 04:00 on Sunday. Silo renders
`templates/prune-packages.md` at its current tip `<sha>` and records:

- `template: {ref, id, rev: <sha>, inputs: {}}`;
- `schedule: {ref, id, fired_at}`.

It commits the task to the team intake as `approved`, with
`approved_by: silo:schedule/sch-…`. This is allowed only because the
schedule's `auto_approve_proposals` was itself approved when the schedule
merged. Silo then places the task. Commits:

- **intake**, 1;
- **Grain**, 1;
- **intake**, 1 (placement).

**A design edit triggers planning.** The `review/hs-approval-by-push`
branch adds `### Approval is a push {#HS-6}` and edits HS-3. An approver
merges it to `main` in `hub/shared`, and the hook accepts it under
§8.9. Post-receive classifies the requirements:

- HS-6 is **new**;
- HS-3 is **changed, done**;
- the rest are unchanged.

Silo commits three proposals to `hub/intake` in one commit:

- implement HS-6;
- test HS-6;
- a delta for HS-3.

Each has `implements: [{…#HS-n, rev, hash}]` and a deterministic id.
Approving each one then follows §8.3 and §8.4.

**Link, then unlink, a standalone Grain.**

- **Link:** `grain hub login`, then `grain tasks link`. Silo creates
  `grains/<id>.git` with a `kind: repo` README naming the Grain and the
  Silo. The Grain pushes its local history into it. This first push is
  allowed for the owner's credential only, and only into an empty repo.
  The Grain then makes it its task remote. Open tasks keep their ids.
  From then on Silo is a writer, per the README.
- **Unlink:** `grain tasks unlink` points the Grain back at its local
  clone. A README commit sets `host: local` and drops Silo's `writers`
  rule; only the owner can make it. Silo re-places any task still
  `placed` but not yet `running` there, through intake.
  The local clone keeps all history.

## 9. How the task interface reduces

| hub-interface.md today | With task repos |
| --- | --- |
| `hello` / `welcome` / `heartbeat` / `goodbye` | unchanged (the registry and slots stay live) |
| `offer` (task + attestation) | Silo's commit of a `placed` task file (with `attestation`) + `{"type":"pull"}` |
| `resume` | gone: the task is in the Grain's repo, and a returning Grain pulls |
| `accept` / `refuse` | the Grain's commit of `state: running` / `state: refused` + `refusal` |
| `result` | the Grain's commit of `state: done\|failed` + `closed` |
| `cancel` | Silo's commit of `cancel:` + `pull`; the Grain's `cancelled` commit |
| event stream, view stream, http stream | unchanged (runtime; never in git) |

The contract becomes:

- the repo schema (§3, §10);
- the README `writers` rules for a Grain repo;
- the `pull` notification.

A customised Grain conforms by reading and writing those files. Until
stage 4 of §15, both paths run, and the WebSocket messages are derived
from the commits.

## 10. Schemas and versioning

Each kind has a JSON Schema (2020-12) in
[`examples/task-repos/schemas/`](../examples/task-repos/schemas/):
`repo`, `task`, `comment`, `template`, `schedule` and `design`.

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
   placed tasks reference (template, includes, dependencies, designs),
   for as long as those tasks are open. The hook checks new references
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
   - A placed task carries Silo's signed `attestation` (hub.md §6.4). Its
     `aud` is this Grain and its `spec_digest` covers the canonical
     front matter and body. A Grain whose policy names `silo_issuer`
     refuses a placed task without a valid one.
   - Any Silo-owned field that changes later (`cancel`, `moved_to`) is
     accepted only if it is signed or attested the same way.
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
   - a compromised Grain can change only its own tasks' outcomes, in its
     own repo;
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
- schedules and templates from the database, or from grain/config
  `settings/*.yml`.

**Rules:**

- **Ids.** New ids are derived deterministically:
  `base32(sha256(deployment ‖ legacy id))[:12]`. The legacy id goes in
  `aka:`.
- **Runtime columns are dropped.** `next-run-at`, `last-run-at` and the
  attempt rows are not exported.
- **Closed tasks** get a `closed:` snapshot built from the final run,
  the pull request and the merge commit.
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
The template is
[`hub-shared/templates/prune-packages.md`](../examples/task-repos/hub-shared/templates/prune-packages.md),
and the settings-owned `template-1`/`sched-1` pair, which has `reads`, is
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
- **Scale.** Assume thousands of Grain repos, each with about 10⁴ tasks
  a year at roughly 5 KiB each, including comments. That is about
  50 MiB a year per Grain before compression.
  - **Commit batching.** Silo makes one commit per dispatcher pass per
    repo, Grains push at most every 5 seconds, and notifies are
    coalesced (as hub.md §2.2.4 does today).
  - **Fetch cost.** Grains use partial clone (`--filter=blob:none`), so
    a fetch costs what changed.
  - **Archival of closed tasks.** Closed tasks never move. After a year
    a Grain repo can be **rolled over**: a new repo continues it, and the
    old one becomes read-only under an alias (`eu-2026`). Open tasks
    move as in §8.6. Every old reference still resolves, because it
    names the old repo's URL.
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

## 16. Staged implementation plan

| Stage | silo (bwsalmon/silo) | grain (bwsalmon/grain) |
| --- | --- | --- |
| **1. Format** | a `taskrepo` package: front-matter parse and write (field-preserving), JSON Schemas, the reference grammar and resolver, the id index, and `writers` rule evaluation; the Jinja subset on gonja with the Python fixture CI job | pin `taskrepo`; `grain tasks export/import` with the round-trip test on a copy of the database; `grain declarations check` accepts `.md` declarations |
| **2. Standalone Grain on a task repo** | none | a local task repo becomes the backlog's source of truth for git-backed fields, with the database as runtime and index; comments as files; schedules from the repo |
| **3. Silo hosts git** | `silo-git`: smart HTTP behind IAP and node tokens, the pre-receive hook, protected refs, bundles to GCS, restore drill, metrics; intake repos per team; propose and approve by push and through the API (the same commit); the threat-model boundary | `grain tasks link/unlink`; push and pull to a Silo remote |
| **4. Placement by commit** | the dispatcher writes placements, cancels and tombstones in §8.9 order; the `pull` notify; WebSocket offers derived from commits behind a flag, then retired | accept, refuse and close by commit, attestation on the file, the generation fence; then drop `offer`/`resume` handling |
| **5. Shared definitions and planning** | templates and schedules fired in Silo; the design index, planning and flood control; the review-branch gate | render shared templates locally; `implements`/`closed.design_revs`; trailers from refs |
| **6. Signed commits** | keys in README rules; verifying signatures in the hook | verifying the approver's signature against policy (`require_signed`) |

Each stage is a design-item set to be filed after this review. None is
filed by this change.
