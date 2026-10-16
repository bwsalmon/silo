---
kind: run
schema: grain/run@v1
aka: ["grain:eu/task/4248"]
task: {ref: "intake:tasks/4248-sign-offers/task.md", rev: 2f4e6a8c0b1d3f5a7c9e1b3d5f7a9c0e2b4d6f82}
from: {ref: /backlog/4248-sign-offers.md, rev: a1b2c3d4e5f60718293a4b5c6d7e8f9012345602}
grain: eu
state: done
accepted_at: "2026-10-16T09:13:20Z"
started_at: "2026-10-16T09:13:21Z"
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
