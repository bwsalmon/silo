# Example task repos

These examples go with [docs/task-repos.md](../../docs/task-repos.md).
Each folder below stands for one plain-git repository, and its
`README.md` front matter (`kind: repo`) declares the repository's role,
owner, backlog folders, aliases and write rules. The repos reference each
other by git URL and by alias, exactly as real ones would. Every object
is named by its path: there are no ids.

| Folder | Stands for | Role |
| --- | --- | --- |
| [`hub-intake/`](hub-intake/README.md) | `https://silo.example.com/git/hub/intake.git` | the hub team's intake: its tasks, approvals, task discussion, and its ranking of unplaced work |
| [`hub-shared/`](hub-shared/README.md) | `https://silo.example.com/git/hub/shared.git` | shared templates (one moved, leaving a tombstone), a schedule, and a design doc |
| [`grain-eu/`](grain-eu/README.md) | `https://silo.example.com/git/grains/eu.git` | Grain `eu` (owned by bsalmon@google.com): its backlog of ranked references and its execution records |
| [`grain-us/`](grain-us/README.md) | `https://silo.example.com/git/grains/us.git` | Grain `us` (a team Grain): the same, with one re-placed task |

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

The tasks are real, except `2026-10-16-reject-dangling-refs`, which is
illustrative. Their prompts and comments are reconstructed from their
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
  stays there unchanged. Placement is a `kind: ref` in a Grain's
  `backlog/`, carrying the rank, `spec_rev` and the attestation
  (§8.3, §8.4).
- **A Grain's backlog of ranked references:** `grain-eu/backlog/` holds
  three, pointing at tasks in the intake. Its effective order is the
  expedited `2026-10-16-reject-dangling-refs`, then the `high`-priority
  `2026-10-16-approval-flow`, then the running prune at rank `c` (§5.2).
  The owner moved `2026-10-16-approval-flow` from the intake's rank `c`
  to `t` by editing one field (§8.6).
- **One task, two views:** `2026-10-16-approval-flow` is referenced from
  both `hub-intake/backlog/` (rank `c`) and `grain-eu/backlog/` (rank `t`).
- **Run to completion, with a thread and a snapshot:**
  `grain-eu/runs/4310-carry-approver/`. Its comments reference `../run.md`.
  Its backlog reference was removed when it closed, and `from.rev` still
  names it (§5.3, §8.5).
- **An agent's upward filing:** `hub-intake/tasks/2026-10-16-approval-flow/`,
  committed by Silo for the agent of run 4310. Its `filed_by.run` names
  that run, which comments on it and lists it in `closed.follow_ups`
  (§8.12).
- **A schedule firing a templated task:**
  `hub-intake/tasks/scheduled/prune-packages/20261011T0400Z/`, at a path
  derived from the schedule and firing time, recording `template.rev` and
  `schedule.fired_at`; its reference in `grain-eu/backlog/`; and its run,
  `grain-eu/runs/2026-10-11-prune-ghcr/` (§6.2, §8.12).
- **Re-placement after a lost Grain:** the task
  `hub-intake/tasks/2026-10-16-refuse-self-approved/` never moved. Its
  `eu` run is `superseded` (written by Silo, with Silo's comment), `eu`'s
  reference is gone, and `grain-us/backlog/` holds the new reference.
  The `us` run names the `eu` run as `previous` (§8.8).
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

## Checking them

```
python3 examples/task-repos/check.py     # needs python3-yaml, python3-jsonschema, python3-jinja2
```

[`check.py`](check.py) is a reviewer's aid, not the implementation. It
does these things:

- finds objects by scanning front matter;
- validates each against [`schemas/`](schemas/);
- resolves every reference (relative, by alias and by URL), with the path
  as the identity, following tombstones with a hop limit and cycle
  detection;
- checks that tombstones and `moved_from` agree;
- checks that every backlog reference resolves to a task, that no backlog
  holds two references to one task, and that no task has two open runs;
- derives each task's status from its execution records, and prints
  each backlog's effective order;
- recomputes the requirement hashes;
- renders the template fixture in [`fixtures/`](fixtures/) with Python
  Jinja2 under `StrictUndefined`. A Go implementation must produce the
  same bytes.
