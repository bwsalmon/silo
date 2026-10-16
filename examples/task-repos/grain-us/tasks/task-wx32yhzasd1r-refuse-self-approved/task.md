---
kind: task
schema: grain/task@v1
id: task-wx32yhzasd1r
title: Interop test that a Grain refuses an offer approved by its own submitter
team: hub
repo: https://github.com/bwsalmon/silo.git
base: main
state: running
submitter: "user:bsalmon@google.com"
submitted_via: git
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T15:20:00Z"
created_at: "2026-10-16T15:02:44Z"
depends_on:
  - {ref: "eu:tasks/task-4redj39vwnz4-carry-approver/task.md", id: task-4redj39vwnz4}
implements:
  - ref: "shared:designs/hub-intake.md#HS-2"
    id: des-2ghanwagam5v
    rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
    hash: "sha256:54a64d8a703a5e4ca442f5008a56871fbeccf84d5ba01e729b0e2a2ed2aca330"
placement:
  grain: us
  ref: /tasks/task-wx32yhzasd1r-refuse-self-approved/task.md
  generation: 2
  at: "2026-10-16T16:46:00Z"
  intake: {ref: "intake:tasks/task-wx32yhzasd1r-refuse-self-approved/task.md", id: task-wx32yhzasd1r}
  previous: {ref: "eu:tasks/task-wx32yhzasd1r-refuse-self-approved/task.md", id: task-wx32yhzasd1r}
attestation: eyJhbGciOiJFZERTQSIsImtpZCI6ImNsYWltcy0yMDI2LTEwIiwidHlwIjoiSldUIn0.eyJpc3MiOiJodHRwczovL3NpbG8uZXhhbXBsZS5jb20iLCJhdWQiOiJncmFpbjp1cyIsImlhdCI6MTc5MjE3MDAwMCwiZXhwIjoxNzkyMTczNjAwLCJyZXF1ZXN0ZXIiOiJic2FsbW9uQGdvb2dsZS5jb20iLCJ0ZWFtIjoiaHViIiwiYXBwcm92ZWRfYnkiOiJ1c2VyOmFwcHJvdmVyQGV4YW1wbGUuY29tIiwidGFzayI6InRhc2std3gzMnloemFzZDFyIn0.ZXhhbXBsZS1zaWduYXR1cmUtbm90LXZhbGlk
---

Extend `scripts/grain-interop.sh` so that a task whose `approved_by`
equals its `submitter` is never offered, and so that a hand-crafted offer
carrying one is refused by grain's local policy. Run it against grain's
main.
