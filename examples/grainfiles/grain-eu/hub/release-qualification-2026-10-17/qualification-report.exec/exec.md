---
kind: exec
schema: grain/exec@v1
definition: "hub:intake/templates/release-qualification/qualification-report.task.md@0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
definition_sha256: "sha256:0776177cf55a68502f08ab1be181d9a806017f0a21c83579b21a2335265a2976"
instantiated_from: "hub:intake/templates/release-qualification/release-qualification.template.md@0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
inputs: {candidate: candidate}
lifecycle: "hub:intake/lifecycles/security.lifecycle.md@0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
state: sec-approved
created_by: user:bsalmon@google.com
created_at: "2026-10-17T02:00:00Z"
created_via: template
rank: "g"
approved_by: user:bsalmon@google.com
approved_at: "2026-10-17T02:00:00Z"
depends_on:
  - ../qualification-mergequeue.exec/
  - ../qualification-ci-diagnostics.exec/
  - ../qualification-base-triage.exec/
  - ../qualification-system-smoke.exec/
---
