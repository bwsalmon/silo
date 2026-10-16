---
kind: task
schema: grain/task@v1
aka: ["grain:eu/task/4248"]
title: Sign task offers with the claims grain's local policy verifies
team: hub
repo: https://github.com/bwsalmon/silo.git
base: main
state: approved
submitter: "user:bsalmon@google.com"
submitted_via: api
approved_by: "user:approver@example.com"
approved_at: "2026-10-16T09:12:40Z"
origin: task 3024
created_at: "2026-10-16T08:50:02Z"
implements:
  - ref: "shared:designs/hub-intake.md#HS-3"
    rev: e3c5b90fb62a5ee1d1d3102b820212bbf7e77fc4
    hash: "sha256:99e82ca902f20fc62b923e7e54500ec4ffd8a6bed0eec570f78d89f530a205b6"
---

Sign the claims the hub puts on each offer, so that a Grain whose policy
names `silo_issuer` can verify them. Use the claims grain's local policy
already checks: `iss`, `aud`, `iat`, `exp`, `requester`, `team`,
`approved_by`, `granted_capabilities` and `spec_digest`. Carry them on the
offer as `requester_attestation`, publish the keys past IAP, and document
key creation and rotation in docs/deploy.md.
