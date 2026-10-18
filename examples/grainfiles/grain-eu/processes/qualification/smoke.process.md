---
title: "Nightly release qualification: smoke tests"
requestor: git::https://github.com/acme/grain-eu.git
recipient: git::https://github.com/acme/grain-eu.git
created_at: "2026-10-16T12:00:00Z"
"on":
  schedule:
    cron: "0 2 * * *"
    timezone: "Europe/Berlin"
  manual: {}
references:
  - git::https://github.com/acme/widgets.git//src?ref=main
  - git::https://github.com/acme/widgets.git//docs/widget-schema.md#wire-framing
---
Run the nightly qualification smoke suite against the `widgets`
repository on `linux/amd64` and `linux/arm64`, recording any failing
test cases or regressions.
