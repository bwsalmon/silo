---
kind: task
schema: grain/task@v1
title: Interop test that a Grain refuses an offer approved by its own submitter
team: hub
context:
  - {ref: "https://github.com/bwsalmon/silo.git@main", mode: write}
  - {ref: "shared:designs/hub-intake.md"}
state: approved
submitter: "user:bsalmon@google.com"
submitted_via: git
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T15:20:00Z"
created_at: "2026-10-16T15:02:44Z"
depends_on:
  - ../4310-carry-approver/task.md
implements:
  - ref: "shared:designs/hub-intake.md#HS-2"
    rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
    hash: "sha256:54a64d8a703a5e4ca442f5008a56871fbeccf84d5ba01e729b0e2a2ed2aca330"
---

Extend `scripts/grain-interop.sh` so that a task whose `approved_by`
equals its `submitter` is never offered, and so that a hand-crafted offer
carrying one is refused by grain's local policy. Run it against grain's
main.

This task was placed on Grain `eu`, which was lost while running it, and
then re-placed on Grain `us`. Neither move touched this file: see
`eu:runs/2026-10-16-refuse-self-approved/run.md` (superseded) and
`us:runs/2026-10-16-refuse-self-approved/run.md` (docs/task-repos.md §8.8).
