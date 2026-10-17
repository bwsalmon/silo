---
kind: task
schema: grain/task@v1
title: Report on release qualification
state: sec-approved
submitter: user:bsalmon@google.com
created_at: "2026-10-17T02:00:00Z"
approved_by: user:bsalmon@google.com
approved_at: "2026-10-17T02:00:00Z"
submitted_via: template
lifecycle: "https://github.com/acme/hub.git//intake/lifecycles/security.lifecycle.md@0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
instantiated_from: "https://github.com/acme/hub.git//intake/templates/release-qualification/release-qualification.template.md@0c4e9a7b5d3f1e2a6b8c9d0e1f2a3b4c5d6e7f80"
depends_on:
  - qualification-mergequeue.task.md
  - qualification-ci-diagnostics.task.md
  - qualification-base-triage.task.md
  - qualification-system-smoke.task.md
---
Summarise the four qualification results for `candidate`.
