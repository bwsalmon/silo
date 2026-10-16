---
kind: task
schema: grain/task@v1
id: task-s23ve0hq82dn
aka: ["grain:eu/task/4248"]
title: Sign task offers with the claims grain's local policy verifies
team: hub
repo: https://github.com/bwsalmon/silo.git
base: main
state: done
submitter: "user:bsalmon@google.com"
submitted_via: api
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T09:12:40Z"
origin: task 3024
created_at: "2026-10-16T08:50:02Z"
implements:
  - ref: "shared:designs/hub-intake.md#HS-3"
    id: des-2ghanwagam5v
    rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
    hash: "sha256:99e82ca902f20fc62b923e7e54500ec4ffd8a6bed0eec570f78d89f530a205b6"
placement:
  grain: eu
  ref: /tasks/task-s23ve0hq82dn-sign-offers/task.md
  generation: 1
  at: "2026-10-16T09:13:00Z"
  intake: {ref: "intake:tasks/task-s23ve0hq82dn-sign-offers/task.md", id: task-s23ve0hq82dn}
attestation: eyJhbGciOiJFZERTQSIsImtpZCI6ImNsYWltcy0yMDI2LTEwIiwidHlwIjoiSldUIn0.eyJpc3MiOiJodHRwczovL3NpbG8uZXhhbXBsZS5jb20iLCJhdWQiOiJncmFpbjpldSIsImlhdCI6MTc5MjE0MjQwMCwiZXhwIjoxNzkyMTQ2MDAwLCJyZXF1ZXN0ZXIiOiJic2FsbW9uQGdvb2dsZS5jb20iLCJ0ZWFtIjoiaHViIiwiYXBwcm92ZWRfYnkiOiJ1c2VyOmFwcHJvdmVyQGV4YW1wbGUuY29tIiwidGFzayI6InRhc2stczIzdmUwaHE4MmRuIn0.ZXhhbXBsZS1zaWduYXR1cmUtbm90LXZhbGlk
closed:
  outcome: done
  at: "2026-10-16T11:48:29Z"
  summary: >-
    The hub signs each offer's claims for the node it offers the task to
    (hub/claims) and publishes its keys at /v1/claims/keys past IAP; hub.md
    gains §6.4. Signing guards everything between the hub and a Grain, but
    not the hub itself.
  change:
    url: https://github.com/bwsalmon/silo/pull/140
    branch: grain/eu/task-4248
    merge_commit: 776f8e8d2941fc5a1f2f52b9cc5ad84372235456
    commits: [25b772df8f53f2ed4873e4c02f957acbd0bbe838]
  design_revs:
    - item: "https://silo.example.com/git/hub/shared.git//designs/hub-intake.md#HS-3"
      rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
      hash: "sha256:99e82ca902f20fc62b923e7e54500ec4ffd8a6bed0eec570f78d89f530a205b6"
---

Sign the claims the hub puts on each offer, so that a Grain whose policy
names `silo_issuer` can verify them. Use the claims grain's local policy
already checks: `iss`, `aud`, `iat`, `exp`, `requester`, `team`,
`approved_by`, `granted_capabilities` and `spec_digest`. Carry them on the
offer as `requester_attestation`, publish the keys past IAP, and document
key creation and rotation in docs/deploy.md.
