---
kind: task
schema: grain/task@v1
id: task-4redj39vwnz4
aka: ["grain:eu/task/4310"]
title: Carry a hub task's approver on its offer and attestation
team: hub
repo: https://github.com/bwsalmon/silo.git
base: main
state: done
submitter: "user:bsalmon@google.com"
submitted_via: api
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T13:55:12Z"
origin: task 4193
created_at: "2026-10-16T13:41:07Z"
implements:
  - ref: "shared:designs/hub-intake.md#HS-2"
    id: des-2ghanwagam5v
    rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
    hash: "sha256:54a64d8a703a5e4ca442f5008a56871fbeccf84d5ba01e729b0e2a2ed2aca330"
placement:
  grain: eu
  ref: /tasks/task-4redj39vwnz4-carry-approver/task.md
  generation: 1
  at: "2026-10-16T13:58:00Z"
  intake: {ref: "intake:tasks/task-4redj39vwnz4-carry-approver/task.md", id: task-4redj39vwnz4}
attestation: eyJhbGciOiJFZERTQSIsImtpZCI6ImNsYWltcy0yMDI2LTEwIiwidHlwIjoiSldUIn0.eyJpc3MiOiJodHRwczovL3NpbG8uZXhhbXBsZS5jb20iLCJhdWQiOiJncmFpbjpldSIsImlhdCI6MTc5MjE1OTA4MCwiZXhwIjoxNzkyMTYyNjgwLCJyZXF1ZXN0ZXIiOiJic2FsbW9uQGdvb2dsZS5jb20iLCJ0ZWFtIjoiaHViIiwiYXBwcm92ZWRfYnkiOiJ1c2VyOmFwcHJvdmVyQGV4YW1wbGUuY29tIiwidGFzayI6InRhc2stNHJlZGozOXZ3bno0In0.ZXhhbXBsZS1zaWduYXR1cmUtbm90LXZhbGlk
closed:
  outcome: done
  at: "2026-10-16T14:58:30Z"
  summary: >-
    tasks.Spec gains approved_by, stored in a new approved_by column, offered
    with the rest of the spec and signed as the attestation's approved_by
    claim. A spec whose approver is its own submitter is refused, and the
    submit API does not accept the field. The approval flow itself remains
    open, and is proposed as a follow-up.
  change:
    url: https://github.com/bwsalmon/silo/commit/5623aa2544627ce1b119ca5b5d2eb1f2f490c123
    branch: grain/eu/task-4310
    merge_commit: 5623aa2544627ce1b119ca5b5d2eb1f2f490c123
  design_revs:
    - item: "https://silo.example.com/git/hub/shared.git//designs/hub-intake.md#HS-2"
      rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
      hash: "sha256:54a64d8a703a5e4ca442f5008a56871fbeccf84d5ba01e729b0e2a2ed2aca330"
  follow_ups:
    - {ref: "intake:tasks/task-88k1cjf14gda-approval-flow/task.md", id: task-88k1cjf14gda}
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
