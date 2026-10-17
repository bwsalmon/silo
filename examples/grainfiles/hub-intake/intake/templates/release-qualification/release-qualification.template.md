---
kind: template
schema: grain/template@v1
form: folder
title: Release qualification
inputs:
  - {name: candidate, type: string, default: candidate}
lifecycle: ../../lifecycles/security.lifecycle.md
run:
  mode: count
  count: 1
  require_approval: false
  auto_merge: true
---
grain's `.grain/suites/release-qualification.yml`, converted: four
qualification tasks, plus a report that depends on all four.
