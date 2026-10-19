---
grainfile: v1
title: Triage failing CI workflows and verify release workflow runs
gate: false
when:
  workflow:
    - workflows:
        - "grain CI"
        - "e2e-*"
        - ".github/workflows/release.yml"
      events:
        - triggered
        - succeeded
        - failed
      branches:
        - main
        - "release/*"
    - workflow: "nightly-qualification"
      events:
        - failed
      repo: acme/widgets
  schedule:
    - cron: "0 9 * * 1-5"
      timezone: "America/Los_Angeles"
    - cron: "0 9 * * 1-5"
      timezone: "Europe/Berlin"
references:
  - /docs/widget-schema.md
  - /src/widget.go
---
When a CI workflow run on `main` or `release/*` fails, inspect the failing job logs, reproduce the failure against `/src/widget.go` and `/docs/widget-schema.md`, and propose a targeted fix or flake report.
