# Example grain repos

These examples go with [docs/grainfiles.md](../../docs/grainfiles.md).
Each folder below stands for one plain-git repository, a grain repo. Every
grainfile is a Markdown file named `<name>.<kind>.md`, whose front matter's `kind:` matches
its suffix (§2.1); every other file is an asset. A repo's settings are in
`.grainconfig` and its write ownership in `GRAINOWNERS`; a Grain's typed
active roots are in its `grain.config.md` (§4.2, §8.1, §8.2). `README.md` files
are prose for humans. The repos reference each other by git URL and by
alias, exactly as real ones would, and every grainfile is named by its
path: there are no ids.

| Folder | Stands for | Role |
| --- | --- | --- |
| [`hub-intake/`](hub-intake/README.md) | `https://silo.example.com/git/hub/intake.git` | the hub team's intake: its tasks, approvals, task discussion, its ranking of unplaced work, field-level `GRAINOWNERS`, and a team `grain.config.md` adopting one code-repo trigger |
| [`hub-shared/`](hub-shared/README.md) | `https://silo.example.com/git/hub/shared.git` | the team repo: `triggers/` (with a retired one in `triggers/archive/`) and `memory/`, which Grains name as roots; templates (one moved, leaving a tombstone), capability definitions and a design doc, which are only referenced |
| [`grain-eu/`](grain-eu/README.md) | `https://silo.example.com/git/grains/eu.git` | the root of Grain `eu` (owned by bsalmon@google.com): a `grain.config.md` with two backlog lanes, two trigger roots and two memory roots; its own `backlog/`, `triggers/`, `memory/`, task and execution records; `GRAINOWNERS` keeping the config the owner's |
| [`grain-us/`](grain-us/README.md) | `https://silo.example.com/git/grains/us.git` | the root of Grain `us` (a team Grain): Silo's lane first, Silo's assigned triggers with one ignored, its own weekly replacement, a code repo's nightly CI trigger, and one re-placed task's record |
| [`hub-placements/`](hub-placements/README.md) | `https://silo.example.com/git/hub/placements.git` | Silo's own repo, written only by Silo: a folder per Grain, holding its placements and the team triggers assigned to it, active only where a Grain's config names them |
| [`org-templates/`](org-templates/README.md) | `https://silo.example.com/git/org/templates.git` | an org-wide template set, released at `v3`, referenced by URL |
| [`org-capabilities/`](org-capabilities/README.md) | `https://silo.example.com/git/org/capabilities.git` | org-wide capability grainfiles: the GCP base narrowed to one project, released at `v1`, which the hub team narrows further |
| [`widgets-code/`](widgets-code/.grain/README.md) | `https://git.example.com/hub/widgets.git` | a code repo on a forge Silo does not host. Its top-level `README.md` is the code's; `.grainconfig` sets `task_home: promote`; `.grain/` holds its gate, templates, triggers and a promoted task; `docs/design/widgets.design.md` is a design doc in place |

They also reference real repos by URL: `https://github.com/bwsalmon/silo.git`
and `https://github.com/bwsalmon/grain.git`. Those are named but not
copied here.

## Where each grainfile comes from

| Grainfile | File | Real source |
| --- | --- | --- |
| trigger (`on: schedule`) | `hub-shared/triggers/prune-packages.trigger.md` | grain/config `settings/schedules.yml` `repo:schedule:bwsalmon:grain:prune-packages` |
| template | `hub-shared/templates/ghcr/prune-packages.template.md` | grain/config `settings/templates.yml` `repo:template:bwsalmon:grain:prune-packages` (body verbatim) |
| template with `reads` | `hub-shared/templates/granule-pull.template.md` | grain/config `template-1` (body verbatim) |
| template with inputs, `if`/`for` and an include | `hub-shared/templates/design-plan.template.md` + `partials/closing-report.template.md` | bwsalmon/grain `.grain/templates/design-planner.yml`, abridged |
| design doc | `hub-shared/designs/hub-intake.design.md` | bwsalmon/silo `docs/hub.md` §1.2, §6.1 and §6.4, with `{#HS-n}` anchors added |
| a task, run to completion with a thread | `hub-intake/tasks/4310-carry-approver.task.md` and `grain-eu/runs/4310-carry-approver/` | grain task 4310; its change is silo commit `5623aa2` |
| a second task on the same design | `hub-intake/tasks/4248-sign-offers.task.md` and `grain-eu/runs/4248-sign-offers/` | grain task 4248; silo PR #140, merge `776f8e8` |

The tasks are real, except `2026-10-16-reject-dangling-refs` and the
widgets repo's, which are illustrative. Their prompts and comments are reconstructed from their
commits. Migration maps grain's team `default` to the Silo team `hub`,
grain's `target: owner/name` and `base` to a `write` entry in `context:`
(the repo's git URL at that branch), and grain task 4310
to the path `tasks/4310-carry-approver.task.md` (docs/grainfiles.md §13).
Commit shas *inside* the example repos are illustrative, since those
repos do not exist; code-repo shas are real silo commits. The
`attestation` values are JWT-shaped, but the signature is a placeholder.

## The worked flows, as files

- **Typed roots (§4.2):** [`grain-eu/grain.config.md`](grain-eu/grain.config.md)
  names Silo's `placements:grains/eu/backlog/` and its own `backlog/` as
  lanes (weighted 3 to 1, `drafts/` ignored), its own `triggers/` and the
  team's `shared:triggers/` as trigger roots, and its own `memory/`
  (`write: true`) and the team's `shared:memory/` as memory roots, in
  precedence order. Nothing else is active on `eu`; templates,
  capability definitions, designs and the tasks themselves are reached by
  reference. `check.py` prints each Grain's active set.
- **Adding a task:** `grain-eu/tasks/2026-10-16-tidy-run-notes.task.md`, the
  owner's own work, and `grain-eu/backlog/2026-10-16-tidy-run-notes.ref.md`
  in the `own` lane (§10.11).
- **Proposed by git push, and ranked by the team:**
  `hub-intake/tasks/2026-10-16-approval-by-push.task.md`, with a comment
  beside it, and its reference in `hub-intake/backlog/` (§10.1, §10.5).
- **Approved and placed:** each approved task in `hub-intake/tasks/`
  stays there unchanged. Placement is a `*.ref.md` in the Grain's folder
  of `hub-placements/`, carrying the rank, `spec_rev` and the attestation
  (§10.2, §10.3). Silo writes nothing in the Grain's root.
- **Linked by an edit to `grain.config.md`:** `eu`'s `silo` lane and
  `us`'s Silo lane and trigger entry are the link; removing them is
  unlinking (§10.9). `hub-placements/GRAINOWNERS` gives that repo to
  Silo, and each Grain's `GRAINOWNERS` gives its `grain.config.md` to its
  owner, so nothing Silo writes is active unless the owner named it.
- **Lanes:** Grain `eu` has two, Silo's placements (four references:
  the expedited `2026-10-16-reject-dangling-refs`, the `high`
  `2026-10-16-approval-flow`, the running prune at rank `c`, and
  `2026-10-16-widget-schema-v2`) and its own `backlog/`
  (`2026-10-16-tidy-run-notes`). Ranks compare within a lane only, and
  `check.py` prints the picks (§6.2, §10.11). The owner moved
  `2026-10-16-approval-flow` from the intake's rank `c` to `t` through
  Silo's UI, and Silo made the commit (§10.5).
- **One task, two views:** `2026-10-16-approval-flow` is referenced from
  both `hub-intake/backlog/` (rank `c`) and `eu`'s placements (rank `t`).
- **Run to completion, with a thread and a snapshot:**
  `grain-eu/runs/4310-carry-approver/20261016T135830Z.run.md`. Its
  comments reference `../20261016T135830Z.run.md`. Its placement was
  removed when it closed, and `from.rev` still names it (§6.3, §10.4).
- **An agent's upward filing:** `hub-intake/tasks/2026-10-16-approval-flow.task.md`,
  committed by Silo for the agent of run 4310. Its `filed_by.run` names
  that run, which comments on it and lists it in `closed.follow_ups`
  (§10.9).
- **A trigger firing a templated task on its schedule:**
  `hub-intake/tasks/fired/prune-packages/20261011T0400Z.task.md`, at a
  path derived from the trigger and the event's key, recording
  `template.rev` and `trigger: {ref, on, key, fired_at}`; its placement in
  `hub-placements/grains/eu/backlog/`; and its run,
  `grain-eu/runs/2026-10-11-prune-ghcr/` (§6.6, §10.9).
- **Silo placement and re-placement:** the task
  `hub-intake/tasks/2026-10-16-refuse-self-approved.task.md` never moved.
  One Silo commit moved its placement from `hub-placements/grains/eu/backlog/`
  to `grains/us/backlog/`, and Silo's comment on the task says why. `eu`'s
  run was fenced until `eu` came back and wrote `superseded` itself. The
  `us` run names the `eu` run as `previous` (§10.7).
- **A cross-Grain dependency:** that task `depends_on` task 4310, which
  is satisfied because `eu`'s run of it is `done`. The `us` run's comment
  says so (§10.8).
- **A tombstone from a rename:** `hub-shared/templates/prune-packages.template.moved.md`
  is `kind: moved`, `to: ghcr/prune-packages.template.md`, and the
  template there lists the old path in `moved_from`. The trigger names
  the new path. The task fired before the move still names the old one,
  and resolves through the tombstone beside it (§2.2, §3).
- **Traceability:** tasks 4310, 4248 and `2026-10-16-refuse-self-approved`
  `implements:` a requirement of `hub-shared/designs/hub-intake.design.md`
  (HS-2, HS-3), with the section hash they were filed against. The done
  runs record the hash they were done against in `closed.design_revs`
  (§6.9).
- **A team trigger firing through a trigger root:** `eu` names
  `shared:triggers/@main`, so `hub-shared/triggers/granule-pull.trigger.md`
  (`fires_in: grain`) fires on `eu`; `prune-packages` and `plan-designs`
  (`fires_in: silo`) are listed there and fired only by Silo (§6.6, §10.11).
- **Ignoring one team trigger via the entry's `ignore:`:** Silo assigned
  `us` the daily pull, `hub-placements/grains/us/triggers/granule-pull.trigger.md`.
  `us`'s entry for that folder says `ignore: ["granule-pull.trigger.md"]`,
  and `grain-us/triggers/granule-pull-weekly.trigger.md` replaces it, with
  `derived_from` naming the original. `check.py` reports the ignored copy
  as an orphan: nothing activates it (§11.3, §10.11).
- **A team retiring a trigger:** `hub-shared/triggers/archive/legacy-triage-sweep.trigger.md`
  is under `archive/`, so no trigger root lists it, and it fires nowhere
  (§4.3, §10.11).
- **Memory precedence:** `grain-eu/memory/deploy-notes.memory.md` (squash
  merges for widgets) and `hub-shared/memory/merge-style.memory.md`
  (merge commits). `eu` lists its own memory first, so its guidance wins;
  `check.py` prints the order (§4.2, §10.11).
- **A grain folder in a code repo:** `widgets-code/.grain/` holds a
  template; two triggers, one filing the weekly release notes and one
  running the CI workflow nightly and filing a review of the run; a gate
  converted from bwsalmon/silo's own `.grain/triggers/grain-declarations.yml`;
  and a promoted task. Its CI stays YAML in `.grain/workflows/`.
  `docs/design/widgets.design.md` is a design doc in place (§9.1, §9.3).
- **A gate enforced from a code repo's `.grain/`:**
  `widgets-code/.grain/gates/grain-declarations.gate.md` holds every
  change to `.grain/**` for a security review and then a hub approver.
  The widgets repo's merge queue reads it there; no Grain's config names
  it, and no Grain's `ignore:` could release it (§6.7, §10.11).
- **A promoted task:** `widgets-code/.grain/tasks/2026-10-16-widget-schema-v2.task.md`
  was proposed and approved in the intake, then promoted. Its intake path
  is a tombstone, `hub-intake/tasks/2026-10-16-widget-schema-v2.task.moved.md`,
  beside a Silo comment recording the change that landed it. It lists
  that path in `moved_from` (§9.2, §10.10).
- **Backlog references into both homes:** `eu`'s placements hold
  `2026-10-16-widget-schema-v2.ref.md`, pointing into the code repo,
  beside references pointing into the intake (§9.2).
- **An adopted code-repo trigger:** `hub-intake/.grainconfig` sets the
  team's `task_home` (with the widgets repo on `promote`), and
  `hub-intake/grain.config.md`, the team's trigger roots, names the
  widgets repo's weekly release-notes trigger, whose firings go to the
  intake and never into the code repo. Grain `us` names the same repo's
  nightly CI trigger, one file, as a trigger root (§6.6).
- **The planner as a trigger:** `hub-shared/triggers/plan-designs.trigger.md`
  says `on: design-change` over `designs/`, and fires the design-plan
  template once per merged change, binding the event's range and paths
  to the template's inputs (§6.9).
- **A task's whole `context:`:** `hub-intake/tasks/2026-10-16-reject-dangling-refs.task.md`
  lists silo at `main` to write, grain's `pkg/daemon/` at `candidate` to
  read (a sparse checkout), the hub intake design with `depth: 1`, and a
  screenshot attached to the task, stored beside it in
  `2026-10-16-reject-dangling-refs/attachments/`. `check.py` prints its
  sandbox layout (§7, §10.12).
- **An image asset, reached by `depth: 1`:** the design embeds
  `img/claims-flow.png` by a relative link, so that task's sandbox gets the
  image too, while the design's link to grain's `pkg/daemon/` folder is
  skipped and recorded as not a file (§7).
- **A sidecar:** `hub-shared/designs/img/claims-flow.png.asset.md`, with
  alt text, license and provenance, and the image's `sha256` and `bytes`,
  which `check.py` checks (§2.3).
- **A template input choosing the target repo:**
  `hub-shared/templates/ghcr/prune-packages.template.md` and
  `templates/granule-pull.template.md` declare an input `repo` of
  `type: ref`, and take their `write` entry from it (§7, §10.12).
- **What a run's sandbox held:** `grain-eu/runs/4310-carry-approver/20261016T135830Z.run.md`
  records its `context:`, with the commit of each entry (§7).
- **Two base declarations:** `grain-eu/capabilities/bases/gcp-service-account.capability-base.md`
  and `git.capability-base.md`, written by `eu` from its provider code and
  published for readers; never a root (§6.8).
- **A capability requested by reference and allowed by policy:**
  `org-capabilities/gcp/acme-widgets.capability.md` is
  `gcp-service-account@v1` restricted to project `acme-widgets`;
  `hub-shared/capabilities/widgets-deployer.capability.md` narrows it,
  pinned at `v1`; and `hub-intake/tasks/2026-10-17-widgets-preview-deploy.task.md`
  requests it with `mode: use`, narrowed again. Nothing names the
  definitions in any root. `check.py` resolves the chain, applies the
  built-in reducers to `eu`'s base declaration under an illustrative
  policy whose source allowlist is the control, and prints the effective
  capability and the Anselm reducer that makes `eu` refuse the request
  until Anselm is adopted (§6.8, §10.13).
- **Relations navigated from the parent (§5):** every task and every
  execution record names its `thread:` folder, and every comment sits in
  one and names that parent back. Each reference in a Grain's lane names
  its `runs:` folder in that Grain's root:
  `hub-placements/grains/eu/backlog/prune-packages-20261011T0400Z.ref.md`
  says `eu:runs/2026-10-11-prune-ghcr/`, and the record there names the
  same task. The promoted task's `thread:` still names the intake folder
  its comments were written in. `check.py` reaches threads only through
  `thread:`, and checks back-pointers without using them to find anything.
- **A capability approval rule:** `hub-intake/GRAINOWNERS` lets an
  authenticated principal append to a task's `capability_approvals`, and
  the hook's fixed rules bind it to the pusher, never the submitter,
  among the capability's approvers (§8.2, §6.8).
- **A `GRAINOWNERS`-rejected push:** `check.py` replays a teammate
  creating `grain-eu/backlog/urgent-thing.ref.md` (refused: the root is
  the Grain's and its owner's), Silo or `grain:eu` editing `eu`'s
  `grain.config.md` (refused), and a non-approver setting `approved_by` in
  the intake (refused by a field line) (§8.2, §10.11).
- **A suffix mismatch:** `check.py` checks that
  `backlog/2026-10-17-fix-dns.task.md` with `kind: ref` is refused (§2.1,
  §10.11).

## Checking them

```
python3 examples/grainfiles/check.py     # needs python3-yaml, python3-jsonschema, python3-jinja2
```

[`check.py`](check.py) is a reviewer's aid, not the implementation. It
does these things:

- finds grainfiles by their file names' kind suffixes, never by scanning
  front matter, and refuses a name whose suffix and `kind:` disagree;
- validates each grainfile against [`schemas/`](schemas/), and each
  `.grainconfig` against `grainconfig.schema.json`;
- resolves every reference (relative, by alias from the repo's
  `.grainconfig`, and by URL), with the path as the identity, following
  tombstones beside old paths with a hop limit and cycle detection, then
  the archive mapping;
- checks that tombstones and `moved_from` agree;
- checks each comment against the parent whose `thread:` folder holds it,
  and each run against the reference whose `runs:` folder holds it;
- lists every `grain.config.md`'s root entries from the files alone, by
  kind suffix, less each entry's `ignore:` (gitignore semantics, with
  self-tests) and any `archive/`, the most specific entry of a type
  winning; checks the examples' expected active sets, that no active set
  holds a gate, template, capability, task or design, that an entry over
  its limit is unknown and never truncated, and the entries' `@ref` and
  `ref_mode` rules;
- checks that every backlog reference resolves to a task, that no lane
  holds two references to one task, and that no task has two open runs;
- checks code repos (`host: other`): a task there came from an intake
  and is not a trigger's firing, a template there carries no `reads`,
  `writes`, `grants` or `target`, and no `context:` entry naming another
  repo, a gate there names no other repos, and a workflow trigger's
  workflow is a file in the same repo;
- checks `context:` entries: `write` only on a repo or folder at a
  branch, `depth` only on files, `input:` only in templates and only of a
  `ref` input, and no overlapping sandbox paths; prints each task's
  sandbox layout, following `depth` through Markdown body links under the
  caps, and checks what the example's `depth: 1` reaches;
- treats every file that is not a grainfile as an asset, and checks each
  sidecar's `asset`, `sha256` and `bytes`;
- derives each task's status from its execution records, and prints
  each lane's effective order and each Grain's next picks across lanes;
- resolves every capability grainfile's chain to a base type, with cycle
  detection (and a self-test of it) and a depth limit; for each `use`
  entry, applies the built-in reducers to the Grain's published base
  declaration, checks an illustrative local policy (sanctioned bases and
  definition sources for every link, implied egress and repos), prints
  the effective capability, and reports Anselm reducers as not
  evaluable;
- evaluates `GRAINOWNERS`: every grainfile has an owner, and the design's
  allowed and refused pushes come out as §8.2 says;
- runs the orphan check of §11.3 and prints each orphan as a warning;
- recomputes the requirement hashes;
- renders the template fixture in [`fixtures/`](fixtures/) with Python
  Jinja2 under `StrictUndefined`. A Go implementation must produce the
  same bytes.
