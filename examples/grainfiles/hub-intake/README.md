# Hub team intake

This is where work for the **hub** team starts, and where its tasks stay.
It holds proposals, approvals and cancels, discussion of tasks, and the
team's ordering of its unplaced work in `backlog/`.

A task is never copied or moved to the Grain that runs it. Placement adds
a reference to it in that Grain's folder in `hub/placements`, Silo's own
repo, which the Grain's `grain.config.md` names as a lane. The Grain
records what it did in its own `runs/`. How a task is going is read from
those records (docs/grainfiles.md §6.1).

- **To propose:** add `tasks/<yyyy-mm-dd>-<slug>.task.md` with
  `state: proposed` and `submitter: user:<you>`, then push. You can also
  use `silo task propose` or the UI, which make the same commit.
- **To approve** (approvers only, and never your own proposal): set
  `state: approved`, `approved_by: user:<you>` and `approved_at`, then push.
- **To reorder unplaced work** (members): edit `rank` in a reference in
  `backlog/`, or drag it in Silo's UI.
- **To promote a task** into its code repo (members, or an agent
  proposing it for a member to confirm): `silo task promote <task>`.
  The task moves to that repo's `.grain/tasks/`, through its review, and
  leaves a tombstone here.

**Task home.** Tasks stay here by default (`task_home` in `.grainconfig`).
The widgets repo is set to `promote`, which its own `.grainconfig` also
says. The team adopts the widgets repo's weekly release-notes trigger in
this repo's `grain.config.md`, so its firings are filed here, never into
the widgets repo (docs/grainfiles.md §9.2, §6.6).

`GRAINOWNERS` says who may write what, enforced by Silo's pre-receive hook
(docs/grainfiles.md §8). This is the one repo that needs field-level
lines: a task here has several writers over its life (its submitter, an
approver, the team's members ranking it, and Silo).
