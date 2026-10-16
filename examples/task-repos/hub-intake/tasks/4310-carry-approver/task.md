---
kind: task
schema: grain/task@v1
aka: ["grain:eu/task/4310"]
title: Carry a hub task's approver on its offer and attestation
team: hub
repo: https://github.com/bwsalmon/silo.git
base: main
state: approved
submitter: "user:bsalmon@google.com"
submitted_via: api
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T13:55:12Z"
origin: task 4193
created_at: "2026-10-16T13:41:07Z"
implements:
  - ref: "shared:designs/hub-intake.md#HS-2"
    rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
    hash: "sha256:54a64d8a703a5e4ca442f5008a56871fbeccf84d5ba01e729b0e2a2ed2aca330"
---

Add the approver to what the hub offers a Grain. `hub/tasks.Spec` should
carry `approved_by`, the human who approved the task at the hub, and
never its submitter. Store it with the task, send it with the offer, and
sign it as the attestation's `approved_by` claim (docs/hub.md §6.4).

- Refuse a spec whose approver is its own submitter.
- The submit API must not accept the field, so nobody can name their own
  approver.
- Add the column to existing databases in `Open` and `silo hub migrate`.
- Update docs/threat-model.md (INV-17), hub.md, hub-interface.md and
  ui.md to say what is carried.

grain's side (`pkg/daemon/hub.go`, `hubOffer.ApprovedBy`) already files
an offer without an approver as a proposal for local approval.
