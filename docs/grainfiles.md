# Grainfiles: an authoring language for work, as plain git

**Status: design for review (bsalmon@google.com). Nothing here is built,
and no implementation task is filed until it is approved.** The examples
are in [`examples/grainfiles/`](../examples/grainfiles/README.md).

This document is the rationale. The normative specification is
**[The Grainfile Format](grainfiles-spec.md)** (`docs/grainfiles-spec.md`),
with its schemas ([`schemas/grainfiles/`](../schemas/grainfiles/)) and
its reference linter
([`tools/grainfiles/grainlint.py`](../tools/grainfiles/grainlint.py)).
Where the two disagree, the spec wins and this document is corrected.
Section numbers written "spec §N" are the spec's.

## Contents

1. Purpose and scope
2. Grainfiles and grain folders
3. Definitions and their threads
4. Templates
5. Triggers
6. Capabilities
7. Designs and requirement hashes
8. Gates and lifecycles
9. Execution records
10. References: go-getter strings and versioned aliases
11. Write authority
12. Settings: what the format leaves to executors
13. How grain and Silo intend to use the format (non-normative)
14. Migration from today's grain
15. Prior art
- Appendix A. How we got here

## 1. Purpose and scope

Grain's work is written down in many places today: `.grain/templates`,
suites and schedules in each repository, rows in grain's database, design
docs with requirement ids, capability names in grain's configuration.
The Grainfile Format gives the **inputs** to that work one portable,
reviewable form: plain Markdown files with YAML front matter, kept in git
on ordinary forges, that any engine can read.

It is an **authoring language for work**, not an engine's runtime model.
That is the main decision of this revision (Appendix A, 4658), and it
decides what is in and out:

**In scope: the inputs, and one output.**

- **Task definitions**: what to do, with their discussion thread and
  assets (§3).
- **Templates** and **folder templates**, the "processes" (§4).
- **Triggers**: when to run something (§5).
- **Capabilities**: names for authority a run needs (§6).
- **Designs**: requirements, with per-requirement hashes, that work
  implements (§7).
- **Gates**: merge policy a repository declares (§8).
- **Lifecycles**: optional state-machine definitions work may name (§8).
- **Execution records**: what a deployment writes after it ran
  something (§9).
- The mechanics they share: references (§10), tombstones, `GRAINOWNERS`
  and the forge check (§11), and a minimal `grain.config.md`.

**Out of scope: everything only an engine acts on.** Backlogs and lanes,
ranking, root entries and reachability, live execution state, placement,
runs in progress, snapshots, memory, registries, hubs, task homes,
version negotiation, model and sandbox policy. Each engine keeps those
its own way; §13 says how grain and Silo intend to.

**Why draw the line there.** The earlier drafts (Appendix A) tried to
make git the engine's database too: backlogs as folders of executions,
state as a field a deployment edited, placement as creating a folder in
someone's repository, runs as files rewritten at each status change.
Each piece was defensible, and together they made the format large (a
2,400-line spec), made every engine adopt one runtime model, and turned
ordinary scheduling into contended commits. Inputs are different: they
are written by people, reviewed, versioned and shared, which is what git
is good at. Outputs are append-only facts, which git also holds well.
Live state is neither, and an engine's database holds it better.

## 2. Grainfiles and grain folders

A **grainfile** is one object in one Markdown file, `<name>.<kind>.md`,
with YAML front matter (spec §4, §5). The suffix is the kind: there is
no `kind:` field to disagree with it and no per-file `schema:` version
(spec §3). A **grain folder** is a directory with a `grain.config.md`,
which declares `format: 1` once for every file in it, the folder's
owner, its versioned aliases (§10), its check settings, the
capabilities it offers if it is a deployment's (§6), and an opaque
`settings:` mapping for executors (§12). It is the folder's only
configuration file: the separate `.grainconfig` is folded into it.

Grain folders are whole repositories or subdirectories (a code
repository's `.grain/`). They do not nest. Everything else in one is an
asset, never interpreted.

**Containers.** A grainfile with children is a folder: a definition with
a thread is `<name>.task/` (`task.md`, `thread/`, `assets/`); a
capability with attached files is `<name>.capability/`; an execution
record is always `<name>.record/` (`record.md`, `comments/`, `assets/`).
Children are found by listing the folder. Nothing points down to its
children and nothing points back up to its parent, so there is nothing
to keep in agreement. A definition or capability with no children may
stay a bare file, and is *folded* into the folder form, byte for byte,
when its first child arrives; a reference to `<name>.task.md` finds
either form, so a fold changes no reference (spec §4.3.3).

**Moves and tombstones.** A grainfile is identified by where it is (its
canonical reference, §10). Moving it inside one repository is an
ordinary commit that also fixes every reference in the repository; the
linter catches any it missed. Archiving is just such a move, into an
`archive/` directory or anywhere else. Only a move to **another
repository** leaves a tombstone, `<name>.<kind>.moved.md`, naming the new
place, because references from other repositories cannot be fixed in
the same commit. That is the format's one redirect mechanism.

## 3. Definitions and their threads

A **definition** says what to do. Its body is the prompt; its front
matter carries a title, the submitter, input declarations, `context:`
(what a run's sandbox holds: repositories and files, read or write),
`depends_on`, `implements` edges to design requirements, capability
requests, an optional lifecycle and `settings:` (spec §7.1).

A definition holds **no state**. Whether it is queued, approved, running
or done, and on which deployment, is an engine's business. That is what
lets one definition live anywhere: in an intake folder where anyone may
propose work, in the code repository it changes, in a shared library,
or in a deployment's own folder.

**The thread stays with the definition.** Discussion of the request
itself ("this spec is wrong", "should this cover v1 too?") is about the
definition, so it lives in `<name>.task/thread/` as one
`*.comment.md` per comment, named by time and author so concurrent
comments never conflict. An intake that accepts proposals from anyone is
a grain folder whose `GRAINOWNERS` lets `*` create definitions and
comments; what a proposal reveals is only what that folder holds.

**Editing.** A definition may be edited. Records pin the commit that
ran (§9), so nothing is lost; a materially different request should be a
new definition with `supersedes:`. Earlier drafts snapshotted the
definition into each execution and warned on later edits; with
executions out of the format, the pin in the record does the same job
with no copy.

## 4. Templates

A **template** is work with inputs. A single-task template renders to
one definition; a **folder template** (a directory with a `form: folder`
manifest) renders to a closed graph of definitions: its members, with
`depends_on` edges among themselves. Grain's suites are folder templates
(spec §11, Appendix C).

**Jinja2, by reference.** Earlier drafts specified a hand-picked Jinja
subset. The format now says "Jinja2", points at Jinja's own
documentation, and adds only what a sandboxed, deterministic renderer
needs: Jinja2's immutable sandbox, undefined names as errors, no
`random`, no `lipsum`, and output, loop and include limits. A subset is
a second language to learn, implement and keep in step; Jinja2 is
already implemented, documented and familiar, and its sandbox is the
security boundary that matters. Grain's own `{{name}}` placeholders are
valid Jinja2 already.

**What rendering touches.** Only titles and bodies. Front-matter fields
and paths are never rendered, so an input can change what an agent reads
but never what a run depends on, may write or is authorised for.
`include`, `import` and `extends` read files of the same grain folder at
the same commit; included files are ordinary assets
(conventionally `partials/`), so there is no partial kind.

**What happens to a rendering** (queued, run, written somewhere) is the
engine's business. Its records name the template or member, pinned, and
the inputs.

## 5. Triggers

A **trigger** says "when this happens, run that" (spec §7.5). Sources are
a closed set: `schedule`, `push`, `merge`, `pull_request`,
`design-change` and `manual`. A trigger whose source a reader does not
know never fires, because ignoring it would silently change what runs.

**Schedules are cron plus a time zone.** Earlier drafts had five cadence
forms (`every`, `daily`, `weekly`, `monthly`, `cron`), an `overlap`
switch and three catch-up modes. Cron covers every cadence and is
universally understood; the time zone is the one thing cron lacks. A
schedule never overlaps itself, so the switch went; `catch_up` keeps
`latest-only` (the default) and `none`.

**What it fires** is a template, a definition, or a workflow in the
trigger's own repository. `per` says how one event becomes work (one per
event, per changed requirement, per folder or per change), defaulting to
`event`. Where the work lands (`fires_in`, `fires.into`, lanes) is gone:
that is the engine's decision. Sources that keyed off live state
(`task-state`, `task-closed`) are reserved and refused for the same
reason.

## 6. Capabilities

A **capability** is authority a run needs beyond its files: a cloud
service account, a package registry, a write credential. Earlier drafts
modelled what a capability grants: base declarations, chains of
reducers that could only narrow, a reducer language (Anselm), approval
digests and secret slots. None of that was ever going to be the same in
two deployments, and all of it had to be trusted by the deployment that
grants the authority anyway.

So a capability is now **an opaque, referenced name**: a
`<name>.capability.md` with a title and a description, whose body and
attached assets (a Terraform module that provisions the accounts, say)
no reader interprets. Its identity is its canonical reference.
Definitions and templates **request** capabilities by reference; a
deployment's `grain.config.md` lists the capabilities it **offers**.
Work runs only where every request matches an offer, and the linter
checks both sides: an unresolvable request is an error, an unresolvable
offer a warning, and a record of work that ran where a request was not
offered is an error. What a capability actually grants, to which runs,
with which secrets and approvals, is the deployment's local policy.
`GRAINOWNERS` can restrict who adds a request.

Sharing works by reference: a library such as `acme/commons` publishes
`gcp-key.capability.md`, every folder that uses it pins the library
through a versioned alias (§10), and two folders agree they mean the same
capability exactly when their references are canonically equal.

## 7. Designs and requirement hashes

A **design** grainfile lists requirement ids, each with a heading in its
body. A requirement's **hash** is the SHA-256 of its section's text,
normalised (spec §7.8). The design may record the hash beside each id
(the linter checks it), and a definition's `implements` edge carries the
hash of the requirement as it was when the definition was written.

Per-requirement hashes are what make designs useful to an engine without
a database of their own: a stale edge (the edge's hash differs from the
requirement's) shows that a requirement changed after the work was
specified, and a `design-change` trigger fires on exactly the
requirements whose hashes changed. The owner asked explicitly for this
to be kept. Design → implementing definitions is a derived index, never
a list on the design.

## 8. Gates and lifecycles

A **gate** is merge policy a code repository declares for itself: which
paths, and what a change to them requires (a human, a security review,
a second review). Unknown requirements hold every change they match
(fail closed). Requirements that keyed off an execution's live state
are gone with live state.

A **lifecycle** is an optional state-machine **definition**: states,
each with a category from a closed set of six (proposed, ready, active,
waiting, done, cancelled), an initial state and transitions. A
definition or template may name one, so that a team can say "this kind
of work is approved before it runs". How an engine tracks an execution
against it, who may move it between states, and what it does in each
category, is the engine's.

## 9. Execution records

The one output the format defines is a **record** of an execution,
written once by the deployment that ran it, after it finished
(spec §13). A record is a folder `<name>.record/`:

- `record.md`: what ran (`definition`, a canonical reference pinned to
  the commit with `?ref=<sha>`, or, for work that had no definition, the
  task text inline), the inputs and trigger, the `outcome`
  (`succeeded`, `failed`, `cancelled`, `nothing_needed` or `refused`),
  the deployment's forge account, start and finish times, links to the
  pull request, branch and commit, and a free-form Markdown body;
- `comments/`: the execution's process log, one comment per entry;
- `assets/`: anything else worth keeping.

**Why a record, not an execution folder.** 4635 made each execution a
live folder in a deployment's lane: a snapshot of the definition, a
state field rewritten at each transition, a thread and one folder per
attempt, rewritten as the run progressed. That is a runtime model
written into git, with the costs §1 lists. A record keeps what is worth
sharing (what ran, at which commit, with what result, and the log) as
an immutable fact written once. `refused` is in the outcome set because
work a deployment would not start (a capability it does not offer, a
reference that did not resolve) is neither a failure of the work nor a
person's cancellation.

**Immutability.** `record.md`, and the comments written with it, never
change. Comments may be appended afterwards, so a person or the
deployment can annotate an outcome ("this failure was the flaky
runner"); they are new files only, and their times after `finished_at`
tell them from the log.

**Where records go** is the deployment's choice: any grain folder it may
write. Grain will keep them in its own folder; another engine might write
them beside the definition. Definition → records is a derived index:
the definition never points at its records.

## 10. References: go-getter strings and versioned aliases

**Within a folder**, references are relative or `/`-rooted paths, read
at the same commit as the file that holds them.

**Across repositories**, a reference is a **go-getter string**, exactly
as a Terraform module source is written:

```
git::https://github.com/acme/commons.git//capabilities/gcp-key.capability.md?ref=v1.4.0
```

The format allows only the `git::` getter, `https://` (and `file://` for
tests) and the `ref` parameter, and adds its own `#anchor` suffix for
headings and requirement ids, which a reader splits off before parsing.
Earlier drafts had their own locator grammar (`<git url>//<path>@<ref>`
with aliases in `.grainconfig`). go-getter's syntax says the same things,
is documented and implemented already, and is what anyone who has
written a Terraform module source expects.

**Versioned aliases are the one place to pin.** `grain.config.md`
declares aliases, each a go-getter `source` that may carry `?ref=` and an
optional `sha256`; references then say `commons:path`. Two rules keep it
coherent:

- **One version per repository per folder.** Two aliases for one
  repository at different refs is an error, and a raw reference to an
  aliased repository is a warning. A folder therefore uses one commons,
  and a reviewer sees which.
- **Pinning.** A reference to a branch, or with no ref, is unpinned: a
  warning by default and an error with `check.pinning: required`.
  `context:` entries are exempt: they name the code a run works on, which
  is resolved when it runs, and the record pins what ran.

The `sha256` is Go's `dirhash` `h1:` over the aliased directory. It is
reused rather than invented, it does not depend on git's object format,
and anyone can compute it from a checkout; a mismatch refuses every
reference through the alias. **Bumping** a dependency is a one-line
change to the alias (and its hash), and a small `bump` tool could do it.

The **canonical form** (the normalised go-getter string, without
`?ref=`) is the identity of a grainfile: tombstones, capability matching
and records compare it, and records add `?ref=<sha>`. A reader may also
emit a Package URL for provenance tools; purl is not a reference form.

**What else was considered.** A Go-modules-style `grain.mod` plus
`grain.sum` (a require list with minimum version selection and a checksum
database) was the closest fit and is deferred: versioned aliases cover
today's needs, and the `h1:` hash keeps that upgrade path open. Nix
flakes (a lock file and inputs) are close too but bring Nix's evaluation
model. Cargo and npm manifests and lock files assume a registry. Bazel's
bzlmod also assumes a registry and its own module graph. Git submodules
pin well but make every reader a git client with nested checkouts, and
cannot name a file. OCI artifacts would add a registry to a format built
on forges. SPDX and pip VCS URLs (`git+https://…@ref#subdirectory=`)
name the same things as go-getter, in a form fewer people write by hand.
purl identifies packages but does not locate files. SWHID gives
intrinsic, content-addressed ids, which suit archives better than
references people edit.

## 11. Write authority

**The forge is authoritative.** A grain folder's repository protects its
default branch, and the status check `grainfiles/authorize` (the forge
check) is required on it: there is **one enforcement mode**. The check
lints the change, then authorises it against `GRAINOWNERS` read from the
base tree, so a change cannot widen its own authority. Readers act only
on protected-branch commits (and on what aliases pin). Earlier drafts
also allowed a "CODEOWNERS only" mode and a "reader verifies" mode;
every reader then had to know which mode a folder claimed and re-check
history itself, which nobody would implement twice.

**`GRAINOWNERS` names forge identities directly**, as CODEOWNERS does:
`@login` and `@org/team`. Earlier drafts had a principal grammar
(`user:`, `team:…#role`, `deployment:`, `agent:`) and an identity map
from principals to forge accounts; that was a second identity system to
keep in step with the forge's. Author fields (a comment's `author`, a
definition's `submitter`, a record's `deployment`) are forge identities
too, checked against the actor. **A deployment is its forge account**:
its records and its agents' comments are authored by it, and an agent's
comment outside a record says which record it came from.

`GRAINOWNERS` keeps two things CODEOWNERS lacks, because the check can
enforce them: **field rules** (`/tasks/:capabilities @acme/security`
lets only security add a capability request) and **create-only** owners
(`/tasks/**/thread/ *(create)` lets anyone add a comment and nobody
edit one). Compiling to CODEOWNERS, so that the forge requests the
right reviewers, is a direct copy of the whole-path rules; the rest is
the check's alone.

**Fixed rules** hold whatever `GRAINOWNERS` says: author fields are the
actor, records, comments and tombstones never change, and the folder's
owner alone changes `owner`.

## 12. Settings: what the format leaves to executors

Everything about *how* an executor runs work is out of the format:
model, network access, what to do on `nothing_needed`,
auto-merge, the base branch, sandbox sizes, suite-style run policy
(count, until-clean, approval), team, scheduling, the hub's address,
limits. It goes in one opaque `settings:` mapping, allowed in
`grain.config.md` and on definitions, templates and triggers. Readers
ignore what they do not understand, writers preserve it, the linter
does not look inside, and the format imposes no structure or
namespacing on it: grain decides its own keys, and grain's
documentation defines them.

The alternative, defining these fields in the format, made every new
executor option a format revision and made other engines carry grain's
vocabulary. Extension fields (`x_*`) remain for everything else.

## 13. How grain and Silo intend to use the format (non-normative)

- **Grain** reads definitions, templates, triggers, gates, capabilities
  and designs from grain folders (its own, code repositories' `.grain/`
  folders, and libraries it pins), and keeps its **backlog, queue and
  live state in its own database**, as it does today. When an execution
  finishes it writes a record into its own grain folder, with the
  process log as comments. Its capability policy maps offered capability
  references to its existing grants (`gcp-key`'s service-account pool,
  `repo-write:` credentials, …). Grain's runtime model belongs in
  bwsalmon/grain's own documentation; a follow-up there should document
  it, including the `settings:` keys it reads.
- **Silo** is a reader and a writer for people: it shows definitions with
  their threads, lets people propose and discuss work by writing
  grainfiles through the forge as the user, and builds the derived
  indexes (definition → records, requirement → definitions, definition →
  dependents) from what it reads. Its own task model, its offers to
  grains over [the task interface](hub-interface.md) and its registry of
  folders are Silo's, not the format's.

## 14. Migration from today's grain

Spec Appendix C maps grain's `.grain/` declarations onto grainfiles:
templates become single-task templates, suites folder templates,
schedules cron triggers in the schedule's own time zone, `.grain/settings.yml`
the `settings:` of `grain.config.md`, and every per-template executor key
(`model`, `network`, `auto-merge`, `base`, …) moves into `settings:`
unchanged, defined by grain's documentation. Capability names such as
`gcp-key` become capability grainfiles a shared library publishes.

## 15. Prior art

- **CODEOWNERS** for `GRAINOWNERS`' syntax and precedence.
- **Terraform module sources and go-getter** for references.
- **Go modules' `dirhash`** for alias hashes, and Go modules generally
  as the deferred upgrade path.
- **Jinja2** and its sandbox for templates.
- **cron** for schedules; **GitHub Actions' `on:`** for trigger sources.
- **Kanban state categories** (Jira's status categories) for lifecycles.
- **Markdown with YAML front matter** (Jekyll, Hugo, Obsidian) for the
  file form.

## Appendix A. How we got here

This design was first written as "task repos" by task 4446, and amended
many times before it was consolidated here and renamed (grain repos
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
| 4658 | **The format as an authoring language for work**: inputs (definitions with their threads, templates, triggers, capabilities, designs with per-requirement hashes, gates, lifecycle definitions) plus an immutable **execution record**; go-getter references with versioned aliases; forge identities in `GRAINOWNERS`; one enforcement mode; one config file; tombstones as the one redirect; standard sandboxed Jinja2; opaque capabilities; `format: 1` once per folder; executor settings in an opaque `settings:` | backlogs, lanes, root entries and reachability; executions (`<name>.exec/`, `exec.md`, snapshots, `runs/`, placement, re-placement); memory; the deployment and registry forms of `grain.config.md`, registry views and hub behaviour; `task_home`; negotiation; per-file `kind:`/`schema:`; principals and the identity map; the `codeowners` and `reader` enforcement modes; `.grainconfig`; `moved_to`, `moved_from`, archive lookup; capability bases, reducers, Anselm, approval digests and secret slots; the Jinja subset; `fires_in`, `overlap` and the non-cron cadences; `context:` `depth`; the `asset` kind and external assets; `aka`, `submitted_via`, `filed_by`, `origin`, `labels`; the custom locator grammar (below) |

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

### Superseded by 4658: the format as an authoring language

Task 4658 applied, in one rewrite, the owner's decisions of 2026-10-17
(which replaced the separately filed amendments 4636, 4639, 4641, 4648,
4652, 4654 and 4655). One entry per decision:

- **A. Scope.** The format now covers the inputs to work and one output,
  the execution record. Backlogs and lanes, root entries and the
  reachability rule, backlog modes and weights, live execution state and
  categories on executions, placement and re-placement, runs in
  progress, snapshots, back-pointers, `memory`, the deployment and
  registry forms of `grain.config.md`, registry folders and views, hub
  and registry behaviour, the hub as writer, `task_home` modes and
  negotiation were removed from the spec and from this design's
  normative text. *Why:* they are each engine's runtime model, which git
  holds badly and which no two engines share (§1). §13 says,
  non-normatively, how grain and Silo intend to use the format.
- **A. Definitions keep their threads.** Comments on a definition stay
  in `<name>.task/thread/`; a bare `<name>.task.md` is still allowed
  until the first comment or asset folds it (spec §4.3.3).
- **A. Lifecycles are definitions only.** States with categories and
  transitions, named by a definition or template; `aliases`,
  `on_outcome`, `self_approval`, `stalled_after`, the built-in
  `grain/default`, transition rules in `GRAINOWNERS`, the `task-state`
  trigger source and gates' `requires.state` went with live state.
- **A2. The execution record replaces 4635's `.exec/` folder.**
  `exec.md`, the snapshot `task.md`, the live `runs/` and placement are
  gone; a deployment writes `<name>.record/` once, after the execution,
  with `record.md`, a `comments/` process log and `assets/`. Decisions:
  records are named by start time and slug; work with no definition
  carries `task: {title, prompt}` inline; the outcome set adds
  `refused`; `record.md` and its original comments are immutable but
  comments may be appended. *Why:* §9.
- **B1. `GRAINOWNERS` names forge identities**, CODEOWNERS-style
  (`@login`, `@org/team`, `@host:name` for other forges in author
  fields). The principal grammar and the identity map are gone; a
  deployment is its forge account; CODEOWNERS compile copies whole-path
  rules.
- **B2. One enforcement mode**: the required `grainfiles/authorize`
  check on a protected default branch. The `codeowners` and `reader`
  modes and reader verification are gone.
- **B3. One config file per grain folder.** `.grainconfig` folded into
  `grain.config.md`.
- **B4. One redirect mechanism, tombstones.** Repository-level
  `moved_to`, archive lookup, `archive_repo` and `moved_from` are gone;
  `archive/` is an ordinary directory and archiving a move; same-repo
  moves need no tombstone (lint catches dangling references), and
  cross-repo moves leave one.
- **B5. Standard Jinja2**, specified by reference, rendered in the
  sandbox with only the limits that needs, replacing the hand-picked
  subset (and with it `form: partial`: includes are plain assets).
- **B6. Capabilities are opaque referenced grainfiles**, requested by
  definitions and templates and offered by deployments; chains,
  `capability-base`, reducers, Anselm, approval digests, secret slots
  and `context: mode: use` are gone. A capability's attachments live in
  `<name>.capability/assets/`.
- **B7. Versioning** is `format: 1` once in `grain.config.md`. Per-file
  `schema:`, the `kind:` field, negotiation and the hyphenated key
  aliases are gone; `x_` extensions and fail-closed trigger sources and
  gate requirements stay.
- **B8.** Lanes and registry views went with A.
- **B9. Triggers.** `on.schedule` is `cron` plus `timezone`; `fires_in`
  and `overlap` (always skip) are gone; `per` defaults to `event`;
  `catch_up` keeps `latest-only` (default) and `none`.
- **B10.** `context:` loses `depth`.
- **B11.** The `asset` sidecar kind and external assets are gone: large
  files go in Git LFS or are linked.
- **B12.** `aka`, `submitted_via`, `filed_by`, `origin` and `labels` are
  gone.
- **B13.** URLs are `https://` only (plus `file://` for tests).
- **B14. Executor settings leave the format** for one opaque
  `settings:` mapping (§12); Appendix C of the spec maps grain's
  `.grain/` keys into it.
- **C. References are go-getter strings**, replacing the custom locator
  grammar (`<git url>//<path>@<ref>` and `.grainconfig` aliases). Only
  `git::`, `https://`/`file://` and `?ref=` are allowed, with the
  format's own `#anchor` suffix. **Versioned aliases** in
  `grain.config.md` are the one place to pin, with an optional `h1:`
  dirhash, one version per repository per folder, and pinning a warning
  or (with `check.pinning: required`) an error. The canonical form is the
  normalised go-getter string. A Go-modules-style `grain.mod`/`grain.sum`
  was considered and deferred, as the upgrade path; Nix flakes,
  Cargo/npm, bzlmod, git submodules, OCI artifacts, SPDX/pip VCS URLs,
  purl and SWHID were considered and not taken (§10).
- **Lint codes.** Codes whose rules survive keep their numbers. Retired:
  GF002, GF023, GF040, GF044, GF046, GF047, GF048, GF051, GF053, GF054,
  GF055, GF064, GF067, GF069, GF074, GF080, GF090 (with GF041 and GF042
  from 4635). New: GF037, GF038, GF039, GF043 (references and aliases),
  GF056, GF057 (requirement hashes), GF081, GF082, GF083 (capabilities
  and records). GF021 now means an unknown `format`, and GF071 an owner
  that is not a forge identity.
