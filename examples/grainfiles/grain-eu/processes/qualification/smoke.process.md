---
title: "Nightly release qualification: smoke tests"
requestor: https://github.com/acme/grain-eu
recipient: https://github.com/acme/grain-eu
created_at: "2026-10-16T12:00:00Z"
"on":
  schedule:
    cron: "0 2 * * *"
    timezone: "Europe/Berlin"
  manual: {}
references:
  - https://github.com/acme/widgets/tree/main/src
  - https://github.com/acme/widgets/blob/main/docs/widget-schema.md#wire-framing
---
Run the nightly qualification smoke suite against the `widgets`
repository on `linux/amd64` and `linux/arm64`, recording any failing
test cases or regressions.
