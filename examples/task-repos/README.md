# Example task repos

These examples go with [docs/task-repos.md](../../docs/task-repos.md).
Each folder below stands for one plain-git repository, and its
`README.md` front matter (`kind: repo`) declares the repository's role,
owner, backlog folders, aliases and writers. The repos reference each
other by git URL and by alias, exactly as real ones would. Every object
is named by its path: there are no ids.

| Folder | Stands for | Role |
| --- | --- | --- |
| [`hub-intake/`](hub-intake/README.md) | `https://silo.example.com/git/hub/intake.git` | the hub team's intake: its tasks, approvals, task discussion, and its ranking of unplaced work |
| [`hub-shared/`](hub-shared/README.md) | `https://silo.example.com/git/hub/shared.git` | shared templates (one moved, leaving a tombstone), a schedule, and a design doc |
| [`grain-eu/`](grain-eu/README.md) | `https://silo.example.com/git/grains/eu.git` | the root of Grain `eu` (owned by bsalmon@google.com): its include of Silo's placements, its own lane, a pinned include of the team's templates, its own task and its execution records. Only the Grain and its owner write it |
| [`grain-us/`](grain-us/README.md) | `https://silo.example.com/git/grains/us.git` | the root of Grain `us` (a team Grain): its includes of Silo's placements and assigned schedules, one exclude, a customised schedule, and one re-placed task's record |
| [`hub-placements/`](hub-placements/README.md) | `https://silo.example.com/git/hub/placements.git` | Silo's own repo, written only by Silo: a folder per Grain, holding its placements and the team schedules assigned to it |
| [`org-templates/`](org-templates/README.md) | `https://silo.example.com/git/org/templates.git` | an org-wide template set, which `hub-shared` includes at `v3` and cuts one template from |
| [`ns-demo/`](ns-demo/README.md) | `https://silo.example.com/git/demo/namespaces.git` | the X/Y/Z cases of namespace resolution, as a tiny tree |
| [`widgets-code/`](widgets-code/.grain/README.md) | `https://git.example.com/hub/widgets.git` | a code repo on a forge Silo does not host. Its top-level `README.md` is the code's; its grain folder, `.grain/`, has its own `README.md` with `scope` and `task_home: promote` |

They also reference real repos by URL: `https://github.com/bwsalmon/silo.git`
and `https://github.com/bwsalmon/grain.git`. Those are named but not
copied here.

## Where each object comes from

| Object | File | Real source |
| --- | --- | --- |
| schedule | `hub-shared/schedules/prune-packages.md` | grain/config `settings/schedules.yml` `repo:schedule:bwsalmon:grain:prune-packages` |
| template | `hub-shared/templates/ghcr/prune-packages.md` | grain/config `settings/templates.yml` `repo:template:bwsalmon:grain:prune-packages` (body verbatim) |
| template with `reads` | `hub-shared/templates/granule-pull.md` | grain/config `template-1` (body verbatim) |
| template with inputs, `if`/`for` and an include | `hub-shared/templates/design-plan.md` + `partials/closing-report.md` | bwsalmon/grain `.grain/templates/design-planner.yml`, abridged |
| design doc | `hub-shared/designs/hub-intake.md` | bwsalmon/silo `docs/hub.md` §1.2, §6.1 and §6.4, with `{#HS-n}` anchors added |
| a task, run to completion with a thread | `hub-intake/tasks/4310-carry-approver/` and `grain-eu/runs/4310-carry-approver/` | grain task 4310; its change is silo commit `5623aa2` |
| a second task on the same design | `hub-intake/tasks/4248-sign-offers/` and `grain-eu/runs/4248-sign-offers/` | grain task 4248; silo PR #140, merge `776f8e8` |

The tasks are real, except `2026-10-16-reject-dangling-refs` and the
widgets repo's, which are illustrative. Their prompts and comments are reconstructed from their
commits. Migration maps grain's team `default` to the Silo team `hub`,
grain's `target: owner/name` to the repo's git URL, and grain task 4310
to the path `tasks/4310-carry-approver/task.md` (docs/task-repos.md §13).
Commit shas *inside* the example repos are illustrative, since those
repos do not exist; code-repo shas are real silo commits. The
`attestation` values are JWT-shaped, but the signature is a placeholder.

## The worked flows, as files

- **Proposed by git push, and ranked by the team:**
  `hub-intake/tasks/2026-10-16-approval-by-push/`, with a comment on the
  task, and its reference in `hub-intake/backlog/` (§8.2, §8.6).
- **Approved and placed:** each approved task in `hub-intake/tasks/`
  stays there unchanged. Placement is a `kind: ref` in the Grain's
  folder of `hub-placements/`, carrying the rank, `spec_rev` and the
  attestation (§8.3, §8.4). Silo writes nothing in the Grain's root.
- **Linked by an include:** `grain-eu/backlog/silo.md` includes
  `placements:grains/eu/backlog/`, tracking `main`, `kinds: [ref]`.
  Deleting it is unlinking (§8.12).
- **Lanes:** Grain `eu` has two, Silo's placements (four references:
  the expedited `2026-10-16-reject-dangling-refs`, the `high`
  `2026-10-16-approval-flow`, the running prune at rank `c`, and
  `2026-10-16-widget-schema-v2`) and its own `backlog/`
  (`2026-10-16-tidy-run-notes`, the owner's own task in
  `grain-eu/tasks/`). Ranks compare within a lane only; `lanes` in its
  README weights Silo's 3 to 1, and `check.py` prints the picks (§5.2,
  §8.15). The owner moved `2026-10-16-approval-flow` from the intake's
  rank `c` to `t` through Silo's UI, and Silo made the commit (§8.6).
- **One task, two views:** `2026-10-16-approval-flow` is referenced from
  both `hub-intake/backlog/` (rank `c`) and `eu`'s placements (rank `t`).
- **Run to completion, with a thread and a snapshot:**
  `grain-eu/runs/4310-carry-approver/`. Its comments reference `../run.md`.
  Its placement was removed when it closed, and `from.rev` still names it
  (§5.3, §8.5).
- **An agent's upward filing:** `hub-intake/tasks/2026-10-16-approval-flow/`,
  committed by Silo for the agent of run 4310. Its `filed_by.run` names
  that run, which comments on it and lists it in `closed.follow_ups`
  (§8.12).
- **A schedule firing a templated task:**
  `hub-intake/tasks/scheduled/prune-packages/20261011T0400Z/`, at a path
  derived from the schedule and firing time, recording `template.rev` and
  `schedule.fired_at`; its placement in `hub-placements/grains/eu/backlog/`; and its run,
  `grain-eu/runs/2026-10-11-prune-ghcr/` (§6.2, §8.12).
- **Re-placement after a lost Grain:** the task
  `hub-intake/tasks/2026-10-16-refuse-self-approved/` never moved. One
  Silo commit moved its placement from `hub-placements/grains/eu/backlog/`
  to `grains/us/backlog/`, and Silo's comment on the task says why. `eu`'s
  run was fenced until `eu` came back and wrote `superseded` itself. The
  `us` run names the `eu` run as `previous` (§8.8).
- **A cross-Grain dependency:** that task `depends_on` task 4310, which
  is satisfied because `grain-eu/runs/4310-carry-approver/run.md` is
  `done`. The `us` run's comment says so (§8.9).
- **A tombstone from a rename:** `hub-shared/templates/prune-packages.md`
  is `kind: moved`, `to: ghcr/prune-packages.md`, and the template there
  lists it in `moved_from`. The schedule names the new path. The task
  fired before the move still names the old one, and resolves through
  the tombstone (§2.1, §4).
- **Traceability:** tasks 4310, 4248 and `2026-10-16-refuse-self-approved`
  `implements:` a requirement of `hub-shared/designs/hub-intake.md` (HS-2,
  HS-3), with the section hash they were filed against. The done runs
  record the hash they were done against in `closed.design_revs` (§7.2).

- **A grain folder in a code repo:** `widgets-code/.grain/` holds a
  template, a schedule, a trigger converted from bwsalmon/silo's own
  `.grain/triggers/grain-declarations.yml`, and a promoted task. Its CI
  stays YAML in `.grain/workflows/`. `.grain/README.md` governs the
  folder and, through its `scope`, `docs/design/widgets.md`, a design doc
  in place (§3.1, §12).
- **A promoted task:** `widgets-code/.grain/tasks/2026-10-16-widget-schema-v2/`
  was proposed and approved in the intake, then promoted. Its intake path
  is a tombstone, beside a Silo comment recording the change that landed
  it. It lists that path in `moved_from` (§5.5, §8.14).
- **Backlog references into both homes:** `eu`'s placements hold
  `2026-10-16-widget-schema-v2.md`, pointing into the code repo, beside
  references pointing into the intake (§5.5).
- **An adopted schedule:** `hub-intake/README.md` sets the team's
  `task_home` (with the widgets repo on `promote`), and
  `hub-intake/schedules/widgets.md`, a `kind: include`, adopts the widgets
  repo's weekly schedule, whose firings go to the intake and never into
  the code repo (§6.2).
- **Team schedules assigned to a Grain, with one exclude:**
  `hub-placements/grains/us/schedules/hub.md` is Silo's include of
  `shared:schedules/`, and `grain-us/schedules/silo.md` includes that
  folder, so the team's schedules reach `us` transitively, narrowed to
  `kinds: [schedule]`. `grain-us/schedules/no-granule-pull.md` excludes
  the daily granule pull, and `granule-pull-weekly.md` replaces it with
  `derived_from` naming the original (§6.2, §8.15).
- **A transitive template set with a cut:** `hub-shared/templates/org.md`
  includes `org-templates/templates/` pinned at `v3`, and
  `hub-shared/templates/no-legacy-triage.md` excludes one of them.
  `grain-eu/templates/hub.md` includes the team's templates, so `eu`'s
  namespace has the org's `security-review` and `dependency-bump` and not
  `legacy-triage`, while a direct include of the org set would still
  have it (§3.3).
- **X/Y/Z:** `ns-demo/`. X excludes Y; Z includes X and Y; Z's
  namespace contains Y. W includes and excludes Y and contributes none.
  V excludes X's include object, disabling that branch (§3.3, §8.15).

## Checking them

```
python3 examples/task-repos/check.py     # needs python3-yaml, python3-jsonschema, python3-jinja2
```

[`check.py`](check.py) is a reviewer's aid, not the implementation. It
does these things:

- finds objects by scanning front matter, and each one's governing README
  by nearest-README discovery;
- validates each against [`schemas/`](schemas/);
- resolves every reference (relative, by alias and by URL), with the path
  as the identity, following tombstones with a hop limit and cycle
  detection;
- checks that tombstones and `moved_from` agree;
- resolves every Grain root's namespace bottom-up from its includes and
  excludes, with kinds intersected, a depth limit, cycles cut and
  excludes scoped to their own folder; checks the `ns-demo/` cases and
  the examples' expected names; and requires the same answer in two
  iteration orders;
- checks that every backlog reference resolves to a task, that no lane
  holds two references to one task, and that no task has two open runs;
- checks include objects: a target in another repo carries its `@ref`
  and `ref_mode`, and a relative one carries neither;
- checks code repos' grain folders: a task there came from an intake and
  is not a scheduled firing, and a template there carries no `reads`,
  `writes`, `grants` or `target`;

- derives each task's status from its execution records, and prints
  each lane's effective order and each Grain's next picks across lanes;
- recomputes the requirement hashes;
- renders the template fixture in [`fixtures/`](fixtures/) with Python
  Jinja2 under `StrictUndefined`. A Go implementation must produce the
  same bytes.
