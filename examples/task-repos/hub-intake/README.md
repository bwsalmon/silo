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
aliases:
  shared: https://silo.example.com/git/hub/shared.git
  eu: https://silo.example.com/git/grains/eu.git
  us: https://silo.example.com/git/grains/us.git
  silo-code: https://github.com/bwsalmon/silo.git
writers:
  - who: [authenticated]
    may: create
    kinds: [task]
    where: {state: proposed}
    fields: [kind, schema, id, title, team, repo, base, state, submitter, submitted_via, origin, depends_on, implements, template, capabilities, constraints, labels, created_at, body]
    bind: {submitter: $pusher}
    note: Anyone signed in may add a new proposal, and nothing else.
  - who: ["grain:*"]
    may: create
    kinds: [task]
    where: {state: proposed, submitted_via: agent}
    fields: [kind, schema, id, title, team, repo, base, state, submitter, submitted_via, origin, filed_by, depends_on, capabilities, labels, created_at, body]
    bind: {filed_by.grain: $pusher}
    note: >-
      An agent's upward filing (propose_task). submitter must be the human
      who submitted filed_by.task, which the hook reads from the pushing
      Grain's own repo.
  - who: ["team:hub#approver"]
    may: update
    kinds: [task]
    transition: {field: state, from: [proposed], to: [approved]}
    fields: [state, approved_by, approved_at]
    bind: {approved_by: $pusher}
    forbid: {approved_by: $submitter}
  - who: ["team:hub#approver"]
    may: update
    kinds: [task]
    transition: {field: state, from: [proposed], to: [rejected]}
    fields: [state, rejection]
    bind: {rejection.by: $pusher}
  - who: [silo]
    may: [create, update]
    kinds: [task]
    fields: "*"
    note: >-
      Proposals made through the API and UI (submitted_via api|ui|cli),
      firings of approved schedules, planning proposals, and placement
      records. Silo never sets approved_by to a human who did not push it.
  - who: ["team:hub#admin"]
    may: update
    kinds: [repo]
    fields: "*"
---

# Hub team intake

This is where work for the **hub** team starts. It holds new proposals,
their approvals, and a record of where each approved task was placed.
After placement, the copy in the Grain's repo is the authoritative one.
The `placement.ref` here links to it.

- **To propose:** add `tasks/<id>-<slug>/task.md` with `state: proposed`
  and `submitter: user:<you>`, then push. You can also use
  `silo task propose` or the UI, which make the same commit.
- **To approve** (approvers only, and never your own proposal): set
  `state: approved`, `approved_by: user:<you>` and `approved_at`, then push.

The rules above are enforced by Silo's pre-receive hook
(`docs/task-repos.md` §8).
