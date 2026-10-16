# Example task repos

These examples go with [docs/task-repos.md](../../docs/task-repos.md).
Each folder below stands for one plain-git repository, and its
`README.md` front matter (`kind: repo`) declares the repository's role,
owner, aliases and write rules. The repos reference each other by git
URL and by alias, exactly as real ones would.

| Folder | Stands for | Role |
| --- | --- | --- |
| [`hub-intake/`](hub-intake/README.md) | `https://silo.example.com/git/hub/intake.git` | the hub team's intake: proposals, approvals and placement records |
| [`hub-shared/`](hub-shared/README.md) | `https://silo.example.com/git/hub/shared.git` | shared templates, a schedule, and a design doc |
| [`grain-eu/`](grain-eu/README.md) | `https://silo.example.com/git/grains/eu.git` | Grain `eu`'s backlog (owned by bsalmon@google.com) |
| [`grain-us/`](grain-us/README.md) | `https://silo.example.com/git/grains/us.git` | Grain `us`'s backlog (a team Grain) |

They also reference real repos by URL: `https://github.com/bwsalmon/silo.git`
and `https://github.com/bwsalmon/grain.git`. Those are named but not
copied here.

## Where each object comes from

| Object | File | Real source |
| --- | --- | --- |
| schedule | `hub-shared/schedules/prune-packages.md` | grain/config `settings/schedules.yml` `repo:schedule:bwsalmon:grain:prune-packages` |
| template | `hub-shared/templates/prune-packages.md` | grain/config `settings/templates.yml` `repo:template:bwsalmon:grain:prune-packages` (body verbatim) |
| template with `reads` | `hub-shared/templates/granule-pull.md` | grain/config `template-1` (body verbatim) |
| template with inputs, `if`/`for` and an include | `hub-shared/templates/design-plan.md` + `partials/closing-report.md` | bwsalmon/grain `.grain/templates/design-planner.yml`, abridged |
| design doc | `hub-shared/designs/hub-intake.md` | bwsalmon/silo `docs/hub.md` §1.2, §6.1 and §6.4, with `{#HS-n}` anchors added |
| task placed on a Grain, with comments | `grain-eu/tasks/task-4redj39vwnz4-carry-approver/` | grain task 4310; its change is silo commit `5623aa2` |
| a second task on the same design | `grain-eu/tasks/task-s23ve0hq82dn-sign-offers/` | grain task 4248; silo PR #140, merge `776f8e8` |

The tasks are real, but their prompts and comments are reconstructed from
their commits. Migration maps grain's team `default` to the Silo team
`hub`, and grain's `target: owner/name` to the repo's git URL. Commit
shas *inside* the example repos are illustrative, since those repos do
not exist; code-repo shas are real silo commits. The `attestation` values
are JWT-shaped, but the signature is a placeholder.

## The worked flows, as files

- **Proposed by git push:** `hub-intake/tasks/task-8gpsbwgsy4c1-*/` (§8.2).
- **An agent's upward filing:** `hub-intake/tasks/task-88k1cjf14gda-*/`.
  Grain `eu` pushed it while running task 4310. It is linked from 4310's
  `closed.follow_ups` (§8.10).
- **Approved and placed:** each `hub-intake/tasks/*/task.md` with
  `state: placed`, and its copy in a Grain repo (§8.3, §8.4).
- **Run to completion, with a thread and a snapshot:**
  `grain-eu/tasks/task-4redj39vwnz4-*/` (§8.5).
- **A schedule firing a templated task:**
  `grain-eu/tasks/task-m0jpyjbjz6p3-prune-ghcr/`, which records
  `template.rev` and `schedule.fired_at` (§8.10).
- **Re-placement after a lost Grain:** the tombstone at
  `grain-eu/tasks/task-wx32yhzasd1r-*/` (`state: moved`, `moved_to`) and the
  generation-2 copy at `grain-us/tasks/task-wx32yhzasd1r-*/` (§8.6).
- **A cross-Grain dependency:** the `grain-us` copy `depends_on` task
  4310 in `grain-eu`, which is `done` (§8.7).
- **Traceability:** both `grain-eu` tasks `implements:` a requirement of
  `hub-shared/designs/hub-intake.md` (HS-2, HS-3), with the section hash
  they were done against (§7.2).

## Checking them

```
python3 examples/task-repos/check.py     # needs python3-yaml, python3-jsonschema, python3-jinja2
```

[`check.py`](check.py) is a reviewer's aid, not the implementation. It
does these things:

- finds objects by scanning front matter;
- validates each against [`schemas/`](schemas/);
- checks that ids are unique and that only one copy of each task is live;
- resolves every `{ref, id}` (relative, by alias and by URL);
- recomputes the requirement hashes;
- renders the template fixture in [`fixtures/`](fixtures/) with Python
  Jinja2 under `StrictUndefined`. A Go implementation must produce the
  same bytes.
