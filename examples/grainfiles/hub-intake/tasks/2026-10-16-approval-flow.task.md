---
kind: task
schema: grain/task@v1
title: Give the hub an approval flow that records approved_by
team: hub
context:
  - {ref: "https://github.com/bwsalmon/silo.git@main", mode: write}
  - {ref: "shared:designs/hub-intake.design.md"}
state: approved
submitter: "user:bsalmon@google.com"
submitted_via: agent
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T17:05:00Z"
filed_by:
  grain: eu
  run: "eu:runs/4310-carry-approver/20261016T135830Z.run.md"
  principal: "agent:grain:eu/runs/4310-carry-approver"
origin: task 4193
created_at: "2026-10-16T14:56:40Z"
thread: 2026-10-16-approval-flow/comments/
depends_on:
  - 4310-carry-approver.task.md
---

An agent's upward filing (`propose_task`). The agent running
`eu:runs/4310-carry-approver/20261016T135830Z.run.md` asked for it, and Silo committed it
here, because a Grain never writes another repo. Its `submitter` is the
human who submitted the originating task (docs/grainfiles.md §10.9). It
landed as `proposed`, and a hub approver approved it.

`tasks.Spec.ApprovedBy` is now carried and signed (HS-2), but nothing at
the hub can set it: a submission cannot name an approver. Add the
`proposed` state and an approve action for team approvers, never the
submitter, that records `approved_by` and `approved_at`.
