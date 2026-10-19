---
grainfile: v1
title: "Nightly release qualification: summary report"
when:
  schedule:
    cron: "0 2 * * *"
    timezone: "Europe/Berlin"
  manual: {}
references:
  - ./smoke.process.grain.md
  - https://github.com/acme/widgets/blob/main/docs/widget-schema.md
---
Run the smoke qualification checks in `./smoke.process.grain.md` first (or inspect its latest run), then summarise the outcome into a release-readiness report, noting whether all platforms passed and filing follow-up tasks for any new failures.
