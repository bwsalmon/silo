---
title: "Nightly release qualification: summary report"
requestor: git::https://github.com/acme/grain-eu.git
recipient: git::https://github.com/acme/grain-eu.git
created_at: "2026-10-16T12:00:00Z"
trigger_with:
  - ./smoke.process.md
depends_on:
  - ./smoke.process.md
references:
  - git::https://github.com/acme/widgets.git//docs/widget-schema.md
---
Summarise the outcome of `smoke.process.md` into a release-readiness
report, noting whether all platforms passed and filing follow-up tasks
for any new failures.
