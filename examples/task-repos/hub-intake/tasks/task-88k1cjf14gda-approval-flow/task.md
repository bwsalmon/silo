---
kind: task
schema: grain/task@v1
id: task-88k1cjf14gda
title: Give the hub an approval flow that records approved_by
team: hub
repo: https://github.com/bwsalmon/silo.git
base: main
state: proposed
submitter: "user:bsalmon@google.com"
submitted_via: agent
filed_by:
  grain: eu
  task: {ref: "eu:tasks/task-4redj39vwnz4-carry-approver/task.md", id: task-4redj39vwnz4}
  principal: "agent:grain:eu/task-4redj39vwnz4"
origin: task 4193
created_at: "2026-10-16T14:56:40Z"
depends_on:
  - {ref: "eu:tasks/task-4redj39vwnz4-carry-approver/task.md", id: task-4redj39vwnz4}
---

An agent's upward filing (`propose_task`), pushed here by Grain `eu` as
`grain:eu`. Its `submitter` is the human who submitted the originating
task (docs/task-repos.md §8.10). It lands as `proposed` and needs a hub
approver like any other proposal.

`tasks.Spec.ApprovedBy` is now carried and signed (HS-2), but nothing at
the hub can set it: a submission cannot name an approver. Add the
`proposed` state and an approve action for team approvers, never the
submitter, that records `approved_by` and `approved_at`.
