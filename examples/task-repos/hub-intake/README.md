---
kind: repo
schema: grain/repo@v1
role: intake
title: Hub team intake
owner:
  team: hub
  silo: https://silo.example.com
host: silo
default_ref: main
ref_policy:
  protected: [main]
  fast_forward_only: true
  review_prefix: review/
backlog: [backlog/]
aliases:
  shared: https://silo.example.com/git/hub/shared.git
  eu: https://silo.example.com/git/grains/eu.git
  us: https://silo.example.com/git/grains/us.git
  silo-code: https://github.com/bwsalmon/silo.git
  widgets: https://git.example.com/hub/widgets.git
  placements: https://silo.example.com/git/hub/placements.git
task_home:
  mode: intake
  allow: [intake, promote, code-repo]
  repos:
    https://git.example.com/hub/widgets.git: promote
writers:
  - who: [authenticated]
    may: create
    kinds: [task, asset]             # asset: the new task's attachments/ (docs/task-repos.md §2.2)
    paths: ["tasks/**"]
    where: {state: proposed}
    fields: [kind, schema, title, team, context, state, submitter, submitted_via, origin, depends_on, implements, template, constraints, labels, created_at, body]
    bind: {submitter: $pusher}
    note: Anyone signed in may add a new proposal, and nothing else.
  - who: ["team:hub#approver"]
    may: update
    kinds: [task]
    transition: {field: state, from: [proposed], to: [approved]}
    fields: [state, approved_by, approved_at]
    bind: {approved_by: $pusher}
    forbid: {approved_by: $submitter}
  - who: [authenticated]
    may: update
    kinds: [task]
    where: {state: approved}
    fields: [capability_approvals]
    bind: {"capability_approvals[].by": $pusher}
    forbid: {"capability_approvals[].by": $submitter}
    note: >-
      An approval of a capability the task requests that needs one
      (docs/task-repos.md §5.7). The pusher may only append an entry
      naming themselves, and the hook also requires them to be among the
      principals that capability's approval names, for the digest of its
      resolved chain. Usually a team approver; for some capabilities a
      smaller set.
  - who: ["team:hub#approver"]
    may: update
    kinds: [task]
    transition: {field: state, from: [proposed], to: [rejected]}
    fields: [state, rejection]
    bind: {rejection.by: $pusher}
  - who: [silo]
    may: create
    kinds: [task]
    paths: ["tasks/**"]
    fields: "*"
    note: >-
      Proposals made through the API and UI (submitted_via api|ui|cli),
      agents' upward filings (submitted_via agent, with filed_by naming an
      open run in the asking Grain's repo), and
      firings of the team's triggers (submitted_via trigger), planning
      among them. Silo never sets approved_by to a
      human who did not push it.
  - who: [silo]
    may: update
    kinds: [task]
    transition: {field: state, from: [proposed, approved], to: [cancelled]}
    fields: [state, cancel]
    note: A cancel; every Grain whose backlog references the task honours it.
  - who: ["team:hub#member"]
    may: update
    kinds: [ref]
    paths: ["backlog/**"]
    fields: [rank, priority, ranked_by, ranked_at]
    bind: {ranked_by: $pusher}
    note: The team's ordering of its unplaced work.
  - who: [silo]
    may: [create, update, delete]
    kinds: [ref]
    paths: ["backlog/**"]
    fields: "*"
    note: >-
      Silo adds and removes the team's references, and reorders them for
      members who use the UI, with ranked_by set to that member.
  - who: [authenticated]
    may: create
    kinds: [comment]
    paths: ["tasks/**/comments/**"]
    fields: "*"
    bind: {author: $pusher}
  - who: [silo]
    may: create
    kinds: [comment]
    paths: ["tasks/**/comments/**"]
    where: {via: silo}
    fields: "*"
  - who: ["team:hub#admin", silo]
    may: move
    kinds: [task]
    note: A task moved to another team's intake, or reorganised, leaves a tombstone.
  - who: ["team:hub#admin"]
    may: [create, update, delete]
    kinds: [include, exclude]
    paths: ["triggers/**"]
    fields: "*"
    note: >-
      The team adopts a trigger Silo does not host with a kind: include
      of it here, narrowed to kinds: [trigger]. Nobody else may add one,
      so no repo can make this intake receive firings by naming the team. A tracking include files its firings proposed; only a
      pinned one may file them approved (docs/task-repos.md §6.2, §11.13).
  - who: ["team:hub#admin"]
    may: update
    kinds: [repo]
    fields: "*"
---

# Hub team intake

This is where work for the **hub** team starts, and where its tasks stay.
It holds proposals, approvals and cancels, discussion of tasks, and the
team's ordering of its unplaced work in `backlog/`.

A task is never copied or moved to the Grain that runs it. Placement adds
a reference to it in that Grain's folder in `hub/placements`, Silo's own
repo, which the Grain's root includes. The Grain records what it did in
its own `runs/`. How a task is going is read from those
records (docs/task-repos.md §5.1).

- **To propose:** add `tasks/<yyyy-mm-dd>-<slug>/task.md` with
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

**Task home.** Tasks stay here by default (`task_home: intake`). The
widgets repo is set to `promote`, which its own `.grain/README.md` also
says. The team adopts the widgets repo's weekly release-notes trigger
with `triggers/widgets.md`, a `kind: include` of it, so its firings are
filed here, never into the widgets repo (docs/task-repos.md §5.5, §6.2).

The rules above are enforced by Silo's pre-receive hook
(docs/task-repos.md §8). This is the one kind of folder that keeps
field-level rules: a task here really does have several writers over its
life (its submitter, an approver, the team's members ranking it, and
Silo), so the hook has to say which of them may change which field. Every
other repo in these examples has a single writer.
